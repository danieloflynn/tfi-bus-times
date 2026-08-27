package gtfs

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestClientsHaveTimeouts locks in the invariant that wedged the board: every
// client this package uses must have a whole-request timeout, and the realtime
// one must be shorter than the default poll interval so a slow poll cannot
// outlive the tick that scheduled it.
func TestClientsHaveTimeouts(t *testing.T) {
	const defaultPollInterval = 60 * time.Second

	for _, tc := range []struct {
		name   string
		client *http.Client
	}{
		{"rtClient", rtClient},
		{"staticHeadClient", staticHeadClient},
		{"staticDownloadClient", staticDownloadClient},
	} {
		if tc.client.Timeout <= 0 {
			t.Errorf("%s has no Timeout: an unbounded request hangs its goroutine forever", tc.name)
		}
		if tc.client.Transport == nil {
			t.Errorf("%s uses the default transport, not the bounded sharedTransport", tc.name)
		}
	}

	if rtClient.Timeout >= defaultPollInterval {
		t.Errorf("rtClient.Timeout = %s; must be under the %s default poll interval",
			rtClient.Timeout, defaultPollInterval)
	}

	// A pooled keep-alive connection that outlives the poll interval is exactly
	// how a poll inherits a socket killed by a WiFi drop.
	if sharedTransport.IdleConnTimeout <= 0 || sharedTransport.IdleConnTimeout >= defaultPollInterval {
		t.Errorf("sharedTransport.IdleConnTimeout = %s; want >0 and under the %s poll interval",
			sharedTransport.IdleConnTimeout, defaultPollInterval)
	}
	if sharedTransport.ResponseHeaderTimeout <= 0 {
		t.Error("sharedTransport.ResponseHeaderTimeout is unset: a half-dead connection stalls here")
	}
	if sharedTransport.TLSHandshakeTimeout <= 0 {
		t.Error("sharedTransport.TLSHandshakeTimeout is unset")
	}
}

// TestNoDefaultHTTPClient is a source-level guard. The bug this package was
// fixed for is one `http.Get`/`http.DefaultClient` away from returning, and it
// only shows up after days of uptime on real hardware — far too late for any
// behavioural test to catch it. The check walks the AST rather than the raw
// text so the explanatory comments naming these symbols don't trip it.
func TestNoDefaultHTTPClient(t *testing.T) {
	banned := map[string]bool{
		"DefaultClient": true, "DefaultTransport": true,
		"Get": true, "Head": true, "Post": true, "PostForm": true,
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0) // 0 = drop comments
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "http" || !banned[sel.Sel.Name] {
				return true
			}
			t.Errorf("%s: uses http.%s, which has no timeout; use one of the bounded clients in httpclient.go",
				fset.Position(sel.Pos()), sel.Sel.Name)
			return true
		})
	}
}

// TestFetchTimesOutOnHungServer is the regression test for the wedge itself: a
// server that accepts the connection and then never answers must produce an
// error, not a goroutine parked forever.
func TestFetchTimesOutOnHungServer(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never write a response — the client must give up on its own.
		<-block
	}))
	defer func() {
		close(block)
		srv.Close()
	}()

	p := NewPoller(srv.URL, "test-key", NewDB(&StaticDB{}))
	// Same shape as rtClient, wound down so the test runs in milliseconds.
	p.client = &http.Client{Transport: sharedTransport, Timeout: 200 * time.Millisecond}

	done := make(chan error, 1)
	go func() {
		_, err := p.fetch()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("fetch returned nil error from a server that never responded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fetch did not return: the request is unbounded, which is the bug this guards against")
	}
}

// TestPollSurvivesHungServer confirms the wedge does not propagate: a hung fetch
// must leave Poll returning normally and LastPollTime untouched (so the board's
// staleness signal stays truthful) rather than blocking the poller goroutine.
func TestPollSurvivesHungServer(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer func() {
		close(block)
		srv.Close()
	}()

	p := NewPoller(srv.URL, "test-key", NewDB(&StaticDB{}))
	p.client = &http.Client{Transport: sharedTransport, Timeout: 200 * time.Millisecond}

	done := make(chan struct{})
	go func() {
		p.Poll()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Poll blocked on a hung server")
	}

	if got := p.Store().PollTime(); !got.IsZero() {
		t.Errorf("LastPollTime = %v after a failed poll; want zero so staleness stays visible", got)
	}
}

// TestNewPollerUsesBoundedClient guards the default wiring: a Poller built the
// production way must carry the bounded client, not a nil one (which would
// panic) or an unbounded one.
func TestNewPollerUsesBoundedClient(t *testing.T) {
	p := NewPoller("http://example.invalid", "k", NewDB(&StaticDB{}))
	if p.client != rtClient {
		t.Fatalf("NewPoller client = %v; want rtClient", p.client)
	}
	if p.client.Timeout <= 0 {
		t.Error("production poller client has no timeout")
	}
}

// TestZeroValuePollerUsesBoundedClient covers the hand-built-literal case: a
// Poller with no client must fall back to the bounded rtClient, not panic and
// not silently acquire an unbounded one.
func TestZeroValuePollerUsesBoundedClient(t *testing.T) {
	var p Poller
	if got := p.httpClient(); got != rtClient {
		t.Fatalf("zero-value Poller httpClient() = %v; want rtClient", got)
	}
}

// TestFetchFailsFastOnUnreachableHost is a sanity check that a dial failure is
// reported as an error rather than retried indefinitely inside fetch.
func TestFetchFailsFastOnUnreachableHost(t *testing.T) {
	// Reserved TEST-NET-1 address; connections are dropped, not refused.
	p := NewPoller("http://192.0.2.1:9/TripUpdates", "k", NewDB(&StaticDB{}))
	p.client = &http.Client{Transport: sharedTransport, Timeout: 300 * time.Millisecond}

	start := time.Now()
	_, err := p.fetch()
	if err == nil {
		t.Fatal("expected an error dialling an unreachable host")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("fetch took %s to fail; the dial is effectively unbounded", elapsed)
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && !netErr.Timeout() {
		t.Logf("non-timeout network error (fine, still bounded): %v", err)
	}
}
