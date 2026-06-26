package agent

// agent_download_test.go — FR coverage-topup: downloadFile, postReport, checkBinary,
// checkConfig, writeInstalledVersion, and loadSettings branches not hit by the main suite.
//
// All test function names are prefixed TestCoverTopup_ and all helpers are prefixed
// coverTopup to avoid collisions with the existing suite.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- downloadFile ----------

// TestCoverTopup_DownloadFile_Success verifies that downloadFile writes the HTTP
// response body to dest, creating the parent directory when it does not yet exist.
func TestCoverTopup_DownloadFile_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "BINARY-CONTENT")
	}))
	t.Cleanup(srv.Close)

	a, _ := newTestAgent(t, srv.URL)
	// dest lives inside a subdirectory that does not exist yet — downloadFile must
	// call os.MkdirAll to create it before opening the file.
	dest := filepath.Join(t.TempDir(), "staging", "tfi-display")

	if err := a.downloadFile(srv.URL+"/bin", dest); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if string(data) != "BINARY-CONTENT" {
		t.Errorf("downloaded content = %q, want %q", data, "BINARY-CONTENT")
	}
}

// TestCoverTopup_DownloadFile_NonOKStatus verifies that downloadFile returns an
// error (mentioning the status) when the server responds with a non-200 code.
func TestCoverTopup_DownloadFile_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	a, _ := newTestAgent(t, srv.URL)
	dest := filepath.Join(t.TempDir(), "bin")

	err := a.downloadFile(srv.URL+"/bin", dest)
	if err == nil {
		t.Fatal("expected error for non-200 status")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status code, got: %v", err)
	}
}

// ---------- postReport ----------

// TestCoverTopup_PostReport_EmptyToken verifies that postReport returns an error
// immediately when no device token is set, without making any HTTP request.
func TestCoverTopup_PostReport_EmptyToken(t *testing.T) {
	a, _ := newTestAgent(t, "http://127.0.0.1:0") // unreachable — must not be called
	a.deviceToken = ""

	err := a.postReport(context.Background(), "release_failure", map[string]string{"version": "v1"})
	if err == nil {
		t.Fatal("postReport should return an error when deviceToken is empty")
	}
	if !strings.Contains(err.Error(), "device_token") {
		t.Errorf("error should mention device_token, got: %v", err)
	}
}

// TestCoverTopup_PostReport_NonOKStatus verifies that postReport returns an error
// when the report endpoint responds with a non-2xx status code.
func TestCoverTopup_PostReport_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // 400 — not retried by doWithRetry
	}))
	t.Cleanup(srv.Close)

	a, _ := newTestAgent(t, srv.URL)
	a.deviceToken = "tok"
	a.retryAttempts = 1

	err := a.postReport(context.Background(), "release_failure", map[string]string{"version": "v1"})
	if err == nil {
		t.Fatal("postReport should return an error on a non-2xx response")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention the status code, got: %v", err)
	}
}

// ---------- checkBinary ----------

// TestCoverTopup_CheckBinary_Non200Latest verifies that checkBinary returns an
// error when the /latest endpoint responds with a non-200 status code.
func TestCoverTopup_CheckBinary_Non200Latest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // 4xx — not retried
	}))
	t.Cleanup(srv.Close)

	a, _ := newTestAgent(t, srv.URL)
	a.retryAttempts = 1

	err := a.checkBinary(context.Background())
	if err == nil {
		t.Fatal("checkBinary should return an error on non-200 /latest")
	}
}

// TestCoverTopup_CheckBinary_BadJSONLatest verifies that checkBinary returns an
// error when the /latest response body is not valid JSON.
func TestCoverTopup_CheckBinary_BadJSONLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "not-json{{{")
	}))
	t.Cleanup(srv.Close)

	a, _ := newTestAgent(t, srv.URL)
	a.retryAttempts = 1

	err := a.checkBinary(context.Background())
	if err == nil {
		t.Fatal("checkBinary should return an error on malformed JSON")
	}
}

// TestCoverTopup_CheckBinary_EmptyVersionOrURL verifies that checkBinary returns
// an error when the /latest response omits version or download_url.
func TestCoverTopup_CheckBinary_EmptyVersionOrURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Both fields empty — the agent must reject this rather than proceeding.
		io.WriteString(w, `{"version":"","download_url":""}`)
	}))
	t.Cleanup(srv.Close)

	a, _ := newTestAgent(t, srv.URL)
	a.retryAttempts = 1

	err := a.checkBinary(context.Background())
	if err == nil {
		t.Fatal("checkBinary should return an error when version or download_url is empty")
	}
}

// ---------- checkConfig ----------

// TestCoverTopup_CheckConfig_ConfigReadError verifies that checkConfig returns an
// error when the existing config file cannot be read for a reason other than
// "file does not exist" (e.g. the path is a directory).
func TestCoverTopup_CheckConfig_ConfigReadError(t *testing.T) {
	ts := &testServer{configBody: "remote: config\n"}
	srv := ts.start(t)

	a, _ := newTestAgent(t, srv.URL)
	a.deviceToken = "tok"
	// Point configPath at a directory — os.ReadFile returns an error that is not
	// os.IsNotExist, exercising the non-nil, non-NotExist branch.
	a.configPath = t.TempDir()

	err := a.checkConfig(context.Background())
	if err == nil {
		t.Fatal("checkConfig should return an error when configPath is unreadable")
	}
}

// TestCoverTopup_CheckConfig_ApplyError verifies that checkConfig returns an
// error when applyConfig fails, and that the error is surfaced to the caller.
func TestCoverTopup_CheckConfig_ApplyError(t *testing.T) {
	ts := &testServer{configBody: "remote: config\n"}
	srv := ts.start(t)

	a, fu := newTestAgent(t, srv.URL)
	a.deviceToken = "tok"
	os.WriteFile(a.configPath, []byte("local: config\n"), 0644)
	fu.applyErr = errors.New("synthetic apply failure")

	err := a.checkConfig(context.Background())
	if err == nil {
		t.Fatal("checkConfig should return an error when applyConfig fails")
	}
	if fu.applyCalled != 1 {
		t.Errorf("applyConfig called %d times, want 1", fu.applyCalled)
	}
}

// TestCoverTopup_Cycle_CheckConfigError verifies that cycle() logs a checkConfig
// error but does not propagate it — a failed config sync must not abort the loop.
func TestCoverTopup_Cycle_CheckConfigError(t *testing.T) {
	ts := &testServer{
		latestVersion: "v1", // matches installed → binary check is a no-op
		configBody:    "remote: config\n",
	}
	srv := ts.start(t)

	a, fu := newTestAgent(t, srv.URL)
	a.deviceToken = "tok"
	// Installed version matches latest so runUpdate is never called.
	a.writeInstalledVersion("v1")
	// configPath is a directory → checkConfig errors after fetching (non-NotExist read error).
	a.configPath = t.TempDir()

	// cycle() must not panic or return an error — it only logs.
	a.cycle(context.Background())

	if fu.runCalled != 0 {
		t.Errorf("runUpdate called %d times, want 0", fu.runCalled)
	}
	// applyConfig should NOT be reached because the read-config step errors first.
	if fu.applyCalled != 0 {
		t.Errorf("applyConfig called %d times, want 0", fu.applyCalled)
	}
}

// ---------- writeInstalledVersion ----------

// TestCoverTopup_WriteInstalledVersion_MkdirFails verifies that
// writeInstalledVersion returns an error when the parent directory cannot be
// created (e.g. a component of the path is a read-only directory).
func TestCoverTopup_WriteInstalledVersion_MkdirFails(t *testing.T) {
	roDir := t.TempDir()
	if err := os.Chmod(roDir, 0555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(roDir, 0755) })

	a := &Agent{versionFile: filepath.Join(roDir, "subdir", "version")}
	if err := a.writeInstalledVersion("v1"); err == nil {
		t.Fatal("expected error when parent directory cannot be created")
	}
}

// ---------- loadSettings ----------

// TestCoverTopup_LoadSettings_InvalidYAMLInConfig verifies that loadSettings
// falls back to the default interval (and logs a warning) when config.yaml
// contains invalid YAML rather than crashing the agent.
func TestCoverTopup_LoadSettings_InvalidYAMLInConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	sec := filepath.Join(dir, "secrets.yaml")

	// Write syntactically invalid YAML so yaml.Unmarshal returns an error.
	os.WriteFile(cfg, []byte("update_interval_seconds: !!invalid\n"), 0644)
	os.WriteFile(sec, []byte("base_url: https://example.test\n"), 0644)

	s := loadSettings(cfg, sec)
	if s.Interval != defaultInterval {
		t.Errorf("Interval = %v, want default %v after YAML parse error", s.Interval, defaultInterval)
	}
	// BaseURL should still be loaded from the valid secrets file.
	if s.BaseURL != "https://example.test" {
		t.Errorf("BaseURL = %q, want https://example.test", s.BaseURL)
	}
}
