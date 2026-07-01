package remotelog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// received captures one POST to the activity_logs endpoint for assertions.
type received struct {
	auth string
	body reportBody
}

func startServer(t *testing.T, status int) (*httptest.Server, <-chan received) {
	t.Helper()
	ch := make(chan received, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rb reportBody
		json.NewDecoder(r.Body).Decode(&rb)
		ch <- received{auth: r.Header.Get("Authorization"), body: rb}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

func awaitReceived(t *testing.T, ch <-chan received) received {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request")
		return received{}
	}
}

func assertNoRequest(t *testing.T, ch <-chan received) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("unexpected request: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestLogSendsExpectedRequest(t *testing.T) {
	srv, ch := startServer(t, http.StatusCreated)
	c := New(srv.URL, "tok123", LevelInfo)

	c.Info("connected to wifi")

	r := awaitReceived(t, ch)
	if r.auth != "Bearer tok123" {
		t.Errorf("auth = %q, want Bearer tok123", r.auth)
	}
	if r.body.ActivityLog.Level != "info" || r.body.ActivityLog.Message != "connected to wifi" {
		t.Errorf("body = %+v", r.body)
	}
}

func TestLogDoesNotBlockCaller(t *testing.T) {
	// Server never responds within the test's patience; Log must still return
	// immediately since it's fire-and-forget.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	c := New(srv.URL, "tok", LevelInfo)
	done := make(chan struct{})
	go func() {
		c.Error("should not block")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Log blocked the caller")
	}
}

func TestLevelFiltering(t *testing.T) {
	srv, ch := startServer(t, http.StatusCreated)
	c := New(srv.URL, "tok", LevelWarn)

	c.Debug("debug message")
	c.Info("info message")
	assertNoRequest(t, ch)

	c.Warn("warn message")
	r := awaitReceived(t, ch)
	if r.body.ActivityLog.Level != "warn" {
		t.Errorf("expected warn to pass the filter, got %+v", r.body)
	}
}

func TestNoOpWithoutCredentials(t *testing.T) {
	srv, ch := startServer(t, http.StatusCreated)
	c := New(srv.URL, "", LevelInfo) // no device token yet
	c.Info("should not send")
	assertNoRequest(t, ch)

	c2 := New("", "tok", LevelInfo) // no base URL yet
	c2.Info("should not send either")
	assertNoRequest(t, ch)
}

func TestNilClientIsSafe(t *testing.T) {
	var c *Client
	c.Info("nil client should not panic")
	c.Debug("nil client should not panic")
	c.Warn("nil client should not panic")
	c.Error("nil client should not panic")
	c.Log(LevelInfo, "nil client should not panic")
}

func TestUpdateChangesTarget(t *testing.T) {
	srv1, ch1 := startServer(t, http.StatusCreated)
	srv2, ch2 := startServer(t, http.StatusCreated)

	c := New(srv1.URL, "tok1", LevelInfo)
	c.Info("first")
	r := awaitReceived(t, ch1)
	if r.auth != "Bearer tok1" {
		t.Fatalf("auth = %q", r.auth)
	}

	c.Update(srv2.URL, "tok2", LevelInfo)
	c.Info("second")
	r = awaitReceived(t, ch2)
	if r.auth != "Bearer tok2" {
		t.Fatalf("auth = %q", r.auth)
	}
}

func TestServerErrorIsSwallowed(t *testing.T) {
	srv, ch := startServer(t, http.StatusUnauthorized)
	c := New(srv.URL, "bad-token", LevelInfo)

	done := make(chan struct{})
	go func() {
		c.Error("should not panic on 401")
		close(done)
	}()
	awaitReceived(t, ch)
	select {
	case <-done:
	case <-time.After(1 * time.Second):
	}
	// Reaching here without a panic/hang is the assertion.
}

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"debug":   LevelDebug,
		"info":    LevelInfo,
		"warn":    LevelWarn,
		"error":   LevelError,
		"DEBUG":   LevelDebug,
		"":        LevelInfo,
		"bogus":   LevelInfo,
		"warning": LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %q, want %q", in, got, want)
		}
	}
}
