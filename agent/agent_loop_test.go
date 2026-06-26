package agent

// agent_loop_test.go — FR6 orchestration coverage for cycle(), reloadSettings(),
// doWithRetry context cancellation, Run() shutdown, and state-file round-trips.
//
// All test function names are prefixed TestAgentLoop_; helper names are prefixed
// agentLoop to avoid collisions with identifiers declared in agent_test.go.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tfi-display/updater"
)

// agentLoopMakeAgent constructs an Agent backed by a fresh t.TempDir() and a
// no-op fakeUpdater, with an explicit secretsPath so that reloadSettings() can
// be driven by files the test controls. It differs from newTestAgent (defined
// in agent_test.go) only in the added secretsPath parameter.
func agentLoopMakeAgent(t *testing.T, baseURL, secretsPath string) (*Agent, *fakeUpdater) {
	t.Helper()
	dir := t.TempDir()
	fu := &fakeUpdater{}
	a := &Agent{
		configPath:      filepath.Join(dir, "config.yaml"),
		secretsPath:     secretsPath,
		baseURL:         baseURL,
		interval:        time.Hour,
		updaterCfg:      updater.Config{StagingDir: dir, ServiceName: "tfi-display", WaitTimeout: time.Second},
		versionFile:     filepath.Join(dir, "tfi-display.version"),
		badVersionsFile: filepath.Join(dir, "bad-versions"),
		serviceName:     "tfi-display",
		waitTimeout:     time.Second,
		http:            &http.Client{Timeout: 5 * time.Second},
		retryAttempts:   4,
		retryDelay:      time.Millisecond,
		runUpdate:       fu.run,
		applyConfig:     fu.apply,
	}
	return a, fu
}

// ---------- cycle() ----------

// TestAgentLoop_CycleBothChecks verifies that cycle() drives both the binary
// check and the config check in a single call, and that the two checks are
// independent: a binary that is already up-to-date does not prevent a changed
// config from being applied.
func TestAgentLoop_CycleBothChecks(t *testing.T) {
	ts := &testServer{
		latestVersion: "v1",           // matches installed → runUpdate not called
		downloadBody:  "BINARY",
		configBody:    "new: config\n",
	}
	srv := ts.start(t)
	a, fu := agentLoopMakeAgent(t, srv.URL, "")
	a.deviceToken = "tok-cycle"

	if err := a.writeInstalledVersion("v1"); err != nil {
		t.Fatalf("setup writeInstalledVersion: %v", err)
	}
	if err := os.WriteFile(a.configPath, []byte("old: config\n"), 0644); err != nil {
		t.Fatalf("setup config file: %v", err)
	}

	a.cycle(context.Background())

	if fu.runCalled != 0 {
		t.Errorf("runUpdate called %d times; want 0 (binary is current)", fu.runCalled)
	}
	if fu.applyCalled != 1 {
		t.Errorf("applyConfig called %d times; want 1 (config changed)", fu.applyCalled)
	}
}

// TestAgentLoop_CycleBothChecksFail verifies that a binary-check failure does
// not prevent the config check from running: the two checks are independent and
// both must execute every cycle.
func TestAgentLoop_CycleBothChecksFail(t *testing.T) {
	// /api/tfi/v1/latest returns a new version; download fails → binary check errors.
	ts := &testServer{
		latestVersion:  "v2",
		downloadStatus: http.StatusInternalServerError,
		configBody:     "remote: config\n",
	}
	srv := ts.start(t)
	a, fu := agentLoopMakeAgent(t, srv.URL, "")
	a.deviceToken = "tok-both"

	if err := a.writeInstalledVersion("v1"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Config differs so applyConfig should be called even though binary check errored.
	if err := os.WriteFile(a.configPath, []byte("local: config\n"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	a.cycle(context.Background())

	// runUpdate must NOT have been called (download failed before it).
	if fu.runCalled != 0 {
		t.Errorf("runUpdate called %d; want 0 (download failed)", fu.runCalled)
	}
	// Config check must still have run.
	if fu.applyCalled != 1 {
		t.Errorf("applyConfig called %d times; want 1 (config check runs after binary-check error)", fu.applyCalled)
	}
}

// TestAgentLoop_CycleSkipsWhenNoBaseURL verifies that cycle() bails out
// immediately when no base URL is set, making neither the binary nor the config
// HTTP call.
func TestAgentLoop_CycleSkipsWhenNoBaseURL(t *testing.T) {
	a, fu := agentLoopMakeAgent(t, "", "")
	// baseURL is already empty — cycle must log and return without doing any work.

	a.cycle(context.Background())

	if fu.runCalled != 0 {
		t.Errorf("runUpdate called %d times; want 0 (no base URL)", fu.runCalled)
	}
	if fu.applyCalled != 0 {
		t.Errorf("applyConfig called %d times; want 0 (no base URL)", fu.applyCalled)
	}
}

// ---------- reloadSettings() ----------

// TestAgentLoop_ReloadSettingsFromFiles verifies that reloadSettings() reads
// config.yaml and secrets.yaml and updates the live baseURL, deviceToken, and
// interval fields on the Agent struct.
func TestAgentLoop_ReloadSettingsFromFiles(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	secPath := filepath.Join(dir, "secrets.yaml")

	if err := os.WriteFile(cfgPath, []byte("update_interval_seconds: 300\n"), 0644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	if err := os.WriteFile(secPath, []byte("base_url: https://update.example\ndevice_token: tok-reload\n"), 0644); err != nil {
		t.Fatalf("writing secrets: %v", err)
	}

	fu := &fakeUpdater{}
	a := &Agent{
		configPath:      cfgPath,
		secretsPath:     secPath,
		baseURL:         "old-url",
		deviceToken:     "old-tok",
		interval:        defaultInterval,
		updaterCfg:      updater.Config{StagingDir: dir},
		versionFile:     filepath.Join(dir, "ver"),
		badVersionsFile: filepath.Join(dir, "bad"),
		http:            &http.Client{},
		retryAttempts:   1,
		retryDelay:      time.Millisecond,
		runUpdate:       fu.run,
		applyConfig:     fu.apply,
	}

	a.reloadSettings()

	if a.baseURL != "https://update.example" {
		t.Errorf("baseURL = %q; want https://update.example", a.baseURL)
	}
	if a.deviceToken != "tok-reload" {
		t.Errorf("deviceToken = %q; want tok-reload", a.deviceToken)
	}
	if a.interval != 300*time.Second {
		t.Errorf("interval = %v; want 300s", a.interval)
	}
}

// TestAgentLoop_ReloadSettingsMissingFiles verifies that reloadSettings() falls
// back to safe defaults when config.yaml and secrets.yaml do not exist — a
// freshly provisioned device must not crash the agent on first boot.
func TestAgentLoop_ReloadSettingsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	fu := &fakeUpdater{}
	a := &Agent{
		configPath:      filepath.Join(dir, "config.yaml"),   // does not exist
		secretsPath:     filepath.Join(dir, "secrets.yaml"),  // does not exist
		baseURL:         "old",
		deviceToken:     "old",
		interval:        defaultInterval,
		updaterCfg:      updater.Config{StagingDir: dir},
		versionFile:     filepath.Join(dir, "ver"),
		badVersionsFile: filepath.Join(dir, "bad"),
		http:            &http.Client{},
		retryAttempts:   1,
		retryDelay:      time.Millisecond,
		runUpdate:       fu.run,
		applyConfig:     fu.apply,
	}

	a.reloadSettings()

	if a.baseURL != "" {
		t.Errorf("baseURL = %q; want empty (no secrets file)", a.baseURL)
	}
	if a.deviceToken != "" {
		t.Errorf("deviceToken = %q; want empty (no secrets file)", a.deviceToken)
	}
	if a.interval != defaultInterval {
		t.Errorf("interval = %v; want default %v (no config file)", a.interval, defaultInterval)
	}
}

// ---------- doWithRetry: context cancellation ----------

// TestAgentLoop_DoWithRetryContextCancel verifies that cancelling a context
// during the inter-attempt sleep aborts the retry loop immediately, not after
// the full retry delay has elapsed. This ensures that SIGTERM-triggered
// cancellation is not held up by a stalled cold-start retry sequence.
func TestAgentLoop_DoWithRetryContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // always 5xx — triggers retry sleep
	}))
	t.Cleanup(srv.Close)

	a, _ := agentLoopMakeAgent(t, srv.URL, "")
	a.retryAttempts = 4
	a.retryDelay = 10 * time.Second // far longer than the context lifetime

	// Context expires at 200 ms — well before any single retry delay.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := a.doWithRetry(ctx, func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL+"/any", nil)
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("doWithRetry should return an error when the context expires")
	}
	// With correct cancel-propagation the call returns in ~200 ms, not in 10 s.
	if elapsed > 2*time.Second {
		t.Errorf("doWithRetry took %v after context cancel; expected abort in ≪ 2s (retry delay is 10s)", elapsed)
	}
}

// ---------- Run() ----------

// TestAgentLoop_RunExitsOnCancel verifies that Run() returns nil promptly
// after its context is cancelled, even though the tick interval is one hour.
// This proves that a SIGTERM (which cancels the root context) shuts the agent
// down immediately rather than blocking until the next poll.
func TestAgentLoop_RunExitsOnCancel(t *testing.T) {
	dir := t.TempDir()
	fu := &fakeUpdater{}
	// No secrets file → reloadSettings() sets baseURL="" → cycle() skips, so the
	// first iteration returns in microseconds and the test reaches the select
	// with ctx.Done() already closed.
	a := &Agent{
		configPath:      filepath.Join(dir, "config.yaml"),
		secretsPath:     filepath.Join(dir, "secrets.yaml"), // intentionally absent
		interval:        time.Hour,                           // would block without cancel
		updaterCfg:      updater.Config{StagingDir: dir},
		versionFile:     filepath.Join(dir, "ver"),
		badVersionsFile: filepath.Join(dir, "bad"),
		http:            &http.Client{Timeout: time.Second},
		retryAttempts:   1,
		retryDelay:      time.Millisecond,
		runUpdate:       fu.run,
		applyConfig:     fu.apply,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Run even starts

	errc := make(chan error, 1)
	go func() { errc <- a.Run(ctx) }()

	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Run returned %v; want nil on clean shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run did not return within 2s after context cancel; the tick interval must not block shutdown")
	}
}

// ---------- state-file round-trips ----------

// TestAgentLoop_InstalledVersionRoundTrip checks that writeInstalledVersion
// persists a version string and installedVersion reads it back exactly,
// including the before-any-write case (no file → empty string) and
// the overwrite case (file already exists).
func TestAgentLoop_InstalledVersionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{versionFile: filepath.Join(dir, "tfi-display.version")}

	// Before any write: missing file → empty string.
	if got := a.installedVersion(); got != "" {
		t.Errorf("installedVersion() before write = %q; want empty", got)
	}

	const v1 = "v1.2.3"
	if err := a.writeInstalledVersion(v1); err != nil {
		t.Fatalf("writeInstalledVersion(%q): %v", v1, err)
	}
	if got := a.installedVersion(); got != v1 {
		t.Errorf("installedVersion() = %q; want %q", got, v1)
	}

	// Overwrite with a newer tag — the file must reflect the latest write.
	const v2 = "v2.0.0-rc1"
	if err := a.writeInstalledVersion(v2); err != nil {
		t.Fatalf("writeInstalledVersion(%q): %v", v2, err)
	}
	if got := a.installedVersion(); got != v2 {
		t.Errorf("installedVersion() after overwrite = %q; want %q", got, v2)
	}
}

// TestAgentLoop_BadVersionRoundTrip checks that markBadVersion persists a
// blacklisted version that isBadVersion then detects, that unmarked versions
// are unaffected, and that re-marking an already-bad version is idempotent and
// does not duplicate the entry (a duplicate would silently pass isBadVersion
// today but could confuse future line-oriented tooling).
func TestAgentLoop_BadVersionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{badVersionsFile: filepath.Join(dir, "bad-versions")}

	// No file yet → any version is clean.
	if a.isBadVersion("v9") {
		t.Error("isBadVersion returned true for an unknown version before any file exists")
	}

	if err := a.markBadVersion("v2.0.0"); err != nil {
		t.Fatalf("markBadVersion(v2.0.0): %v", err)
	}
	if !a.isBadVersion("v2.0.0") {
		t.Error("isBadVersion returned false immediately after markBadVersion")
	}

	// A different version must not be affected.
	if a.isBadVersion("v1.0.0") {
		t.Error("isBadVersion returned true for an unmarked version")
	}

	// Idempotent re-mark: must not error or produce a duplicate entry.
	if err := a.markBadVersion("v2.0.0"); err != nil {
		t.Errorf("second markBadVersion(v2.0.0): %v", err)
	}
	if !a.isBadVersion("v2.0.0") {
		t.Error("isBadVersion returned false after idempotent re-mark")
	}

	// Add a second bad version; both must remain detected.
	if err := a.markBadVersion("v3.0.0"); err != nil {
		t.Fatalf("markBadVersion(v3.0.0): %v", err)
	}
	if !a.isBadVersion("v3.0.0") {
		t.Error("isBadVersion returned false for v3.0.0 after marking")
	}
	if !a.isBadVersion("v2.0.0") {
		t.Error("isBadVersion returned false for v2.0.0 after adding v3.0.0")
	}
}
