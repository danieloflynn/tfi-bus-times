package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tfi-display/updater"
)

// testServer is a configurable stand-in for the update server API.
type testServer struct {
	latestVersion  string
	downloadBody   string
	downloadStatus int // 0 → 200 OK; set non-2xx to simulate a download failure
	configBody     string

	// captured
	reportBody  []byte
	reportToken string
	fetchToken  string
}

func (ts *testServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tfi/v1/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(latestResponse{
			Version:     ts.latestVersion,
			DownloadURL: "http://" + r.Host + "/download",
		})
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		if ts.downloadStatus != 0 {
			w.WriteHeader(ts.downloadStatus)
			return
		}
		io.WriteString(w, ts.downloadBody)
	})
	mux.HandleFunc("/api/tfi/v1/config_files/fetch", func(w http.ResponseWriter, r *http.Request) {
		ts.fetchToken = r.Header.Get("Authorization")
		io.WriteString(w, ts.configBody)
	})
	mux.HandleFunc("/api/tfi/v1/releases/report", func(w http.ResponseWriter, r *http.Request) {
		ts.reportToken = r.Header.Get("Authorization")
		ts.reportBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newTestAgent wires an Agent to the test server with temp state files and a
// fake updater so no test touches systemctl.
func newTestAgent(t *testing.T, baseURL string) (*Agent, *fakeUpdater) {
	t.Helper()
	dir := t.TempDir()
	fu := &fakeUpdater{}
	a := &Agent{
		configPath:      filepath.Join(dir, "config.yaml"),
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

type fakeUpdater struct {
	runCalled    int
	runErr       error
	applyCalled  int
	applyErr     error
	appliedBytes []byte
}

func (f *fakeUpdater) run(updater.Config) error {
	f.runCalled++
	return f.runErr
}

func (f *fakeUpdater) apply(content []byte, _, _ string, _ time.Duration) error {
	f.applyCalled++
	f.appliedBytes = content
	return f.applyErr
}

// --- construction ---

// TestNew_StagesAwayFromLiveBinary guards against the ETXTBSY regression: the
// agent must not download new releases onto the running tfi-display it sits
// beside in /usr/local/bin. The staged path has to differ from the install
// target, otherwise the download fails every cycle with "text file busy" and
// the device is stuck on its old binary.
func TestNew_StagesAwayFromLiveBinary(t *testing.T) {
	a, err := New("/etc/tfi-display/config.yaml", "/etc/tfi-display/secrets.yaml")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.updaterCfg.StagingDir != defaultStagingDir {
		t.Errorf("StagingDir = %q, want dedicated %q", a.updaterCfg.StagingDir, defaultStagingDir)
	}
	staged := filepath.Join(a.updaterCfg.StagingDir, binaryName)
	if staged == a.updaterCfg.TargetBinary {
		t.Errorf("staged download path %q must differ from live target %q (writing onto a running binary fails with ETXTBSY)", staged, a.updaterCfg.TargetBinary)
	}
}

// --- binary sync ---

func TestCheckBinary_InstallsWhenDifferent(t *testing.T) {
	ts := &testServer{latestVersion: "v2", downloadBody: "BINARY-V2"}
	srv := ts.start(t)
	a, fu := newTestAgent(t, srv.URL)
	a.writeInstalledVersion("v1")

	if err := a.checkBinary(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fu.runCalled != 1 {
		t.Errorf("runUpdate called %d times, want 1", fu.runCalled)
	}
	staged, err := os.ReadFile(filepath.Join(a.updaterCfg.StagingDir, binaryName))
	if err != nil {
		t.Fatalf("staged binary not written: %v", err)
	}
	if string(staged) != "BINARY-V2" {
		t.Errorf("staged content %q, want %q", staged, "BINARY-V2")
	}
	if a.installedVersion() != "v2" {
		t.Errorf("version marker = %q, want v2", a.installedVersion())
	}
}

func TestCheckBinary_SkipsWhenSame(t *testing.T) {
	ts := &testServer{latestVersion: "v2", downloadBody: "BINARY-V2"}
	srv := ts.start(t)
	a, fu := newTestAgent(t, srv.URL)
	a.writeInstalledVersion("v2")

	if err := a.checkBinary(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fu.runCalled != 0 {
		t.Errorf("runUpdate should not be called when version matches")
	}
}

func TestCheckBinary_SkipsKnownBadVersion(t *testing.T) {
	ts := &testServer{latestVersion: "v2", downloadBody: "BINARY-V2"}
	srv := ts.start(t)
	a, fu := newTestAgent(t, srv.URL)
	a.writeInstalledVersion("v1")
	if err := a.markBadVersion("v2"); err != nil {
		t.Fatal(err)
	}

	if err := a.checkBinary(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fu.runCalled != 0 {
		t.Errorf("runUpdate should not be called for a known-bad version")
	}
}

func TestCheckBinary_FailureRecordsBadAndReports(t *testing.T) {
	ts := &testServer{latestVersion: "v2", downloadBody: "BINARY-V2"}
	srv := ts.start(t)
	a, fu := newTestAgent(t, srv.URL)
	a.deviceToken = "dev-token"
	a.writeInstalledVersion("v1")
	fu.runErr = io.ErrUnexpectedEOF // any install failure

	if err := a.checkBinary(context.Background()); err == nil {
		t.Fatal("expected error when update fails")
	}
	if !a.isBadVersion("v2") {
		t.Error("v2 should be recorded as bad after a failed install")
	}
	if a.installedVersion() != "v1" {
		t.Errorf("version marker should remain v1 after failure, got %q", a.installedVersion())
	}
	if ts.reportBody == nil {
		t.Fatal("failure should have been reported")
	}
	var payload map[string]map[string]string
	if err := json.Unmarshal(ts.reportBody, &payload); err != nil {
		t.Fatalf("report body not valid JSON: %v", err)
	}
	if payload["release_failure"]["version"] != "v2" {
		t.Errorf("reported version = %q, want v2", payload["release_failure"]["version"])
	}
	if ts.reportToken != "Bearer dev-token" {
		t.Errorf("report auth = %q, want Bearer dev-token", ts.reportToken)
	}
}

// TestCheckBinary_DownloadFailureReportsButNotBad verifies a failed *download*
// (the ETXTBSY class of bug) is reported to the server for visibility but does
// NOT blacklist the version: the binary is fine, the environment failed, so the
// agent must be free to retry the same version next cycle.
func TestCheckBinary_DownloadFailureReportsButNotBad(t *testing.T) {
	ts := &testServer{latestVersion: "v2", downloadStatus: http.StatusInternalServerError}
	srv := ts.start(t)
	a, fu := newTestAgent(t, srv.URL)
	a.deviceToken = "dev-token"
	a.writeInstalledVersion("v1")

	if err := a.checkBinary(context.Background()); err == nil {
		t.Fatal("expected error when the download fails")
	}
	if fu.runCalled != 0 {
		t.Errorf("runUpdate should not run after a failed download, got %d", fu.runCalled)
	}
	if ts.reportBody == nil {
		t.Fatal("download failure should have been reported to the server")
	}
	var payload map[string]map[string]string
	if err := json.Unmarshal(ts.reportBody, &payload); err != nil {
		t.Fatalf("report body not valid JSON: %v", err)
	}
	ue, ok := payload["update_error"]
	if !ok {
		t.Fatalf("download failure should report the non-blacklisting update_error event, got %v", payload)
	}
	if ue["version"] != "v2" {
		t.Errorf("reported version = %q, want v2", ue["version"])
	}
	if ue["stage"] != "download" {
		t.Errorf("reported stage = %q, want download", ue["stage"])
	}
	if ts.reportToken != "Bearer dev-token" {
		t.Errorf("report auth = %q, want Bearer dev-token", ts.reportToken)
	}
	// The crucial distinction from an install failure: a download failure must
	// not blacklist a good release, and the marker must not advance.
	if a.isBadVersion("v2") {
		t.Error("a download failure must NOT mark the version bad")
	}
	if a.installedVersion() != "v1" {
		t.Errorf("version marker should remain v1, got %q", a.installedVersion())
	}
}

// --- config sync ---

func TestCheckConfig_AppliesWhenDifferent(t *testing.T) {
	ts := &testServer{configBody: "stops:\n  - stop_number: \"1\"\n"}
	srv := ts.start(t)
	a, fu := newTestAgent(t, srv.URL)
	a.deviceToken = "dev-token"
	os.WriteFile(a.configPath, []byte("old config"), 0644)

	if err := a.checkConfig(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fu.applyCalled != 1 {
		t.Errorf("applyConfig called %d times, want 1", fu.applyCalled)
	}
	if string(fu.appliedBytes) != ts.configBody {
		t.Errorf("applied %q, want %q", fu.appliedBytes, ts.configBody)
	}
	if ts.fetchToken != "Bearer dev-token" {
		t.Errorf("fetch auth = %q, want Bearer dev-token", ts.fetchToken)
	}
}

func TestCheckConfig_SkipsWhenUnchanged(t *testing.T) {
	body := "same config\n"
	ts := &testServer{configBody: body}
	srv := ts.start(t)
	a, fu := newTestAgent(t, srv.URL)
	a.deviceToken = "dev-token"
	os.WriteFile(a.configPath, []byte(body), 0644)

	if err := a.checkConfig(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fu.applyCalled != 0 {
		t.Errorf("applyConfig should not be called when config is unchanged")
	}
}

func TestCheckConfig_SkipsWithoutToken(t *testing.T) {
	ts := &testServer{configBody: "anything"}
	srv := ts.start(t)
	a, fu := newTestAgent(t, srv.URL)
	a.deviceToken = "" // not provisioned

	if err := a.checkConfig(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fu.applyCalled != 0 {
		t.Errorf("applyConfig should not be called without a device token")
	}
}

// --- retry (fly scale-to-zero cold-start 502s) ---

func TestDoWithRetry_RecoversFrom5xx(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tfi/v1/latest", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 { // first two requests hit a "cold" machine
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(latestResponse{Version: "v2", DownloadURL: "http://" + r.Host + "/download"})
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "BINARY-V2") })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a, fu := newTestAgent(t, srv.URL)
	a.writeInstalledVersion("v1")

	if err := a.checkBinary(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Errorf("server called %d times, want 3 (2 retries then success)", calls)
	}
	if fu.runCalled != 1 {
		t.Errorf("runUpdate called %d times, want 1", fu.runCalled)
	}
}

func TestDoWithRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	a, _ := newTestAgent(t, srv.URL)
	a.writeInstalledVersion("v1")

	if err := a.checkBinary(context.Background()); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != a.retryAttempts {
		t.Errorf("server called %d times, want %d", calls, a.retryAttempts)
	}
}

func TestDoWithRetry_NoRetryOn4xx(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	a, _ := newTestAgent(t, srv.URL)
	a.deviceToken = "dev-token"
	os.WriteFile(a.configPath, []byte("old"), 0644)

	if err := a.checkConfig(context.Background()); err == nil {
		t.Fatal("expected error on 401")
	}
	if calls != 1 {
		t.Errorf("server called %d times, want 1 (4xx is not retried)", calls)
	}
}

// --- settings ---

func TestLoadSettings_Defaults(t *testing.T) {
	s := loadSettings("/nonexistent/config.yaml", "/nonexistent/secrets.yaml")
	if s.BaseURL != "" {
		t.Errorf("BaseURL = %q, want empty", s.BaseURL)
	}
	if s.Interval != defaultInterval {
		t.Errorf("Interval = %s, want %s", s.Interval, defaultInterval)
	}
	if s.DeviceToken != "" {
		t.Errorf("DeviceToken = %q, want empty", s.DeviceToken)
	}
}

func TestLoadSettings_Overrides(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	sec := filepath.Join(dir, "secrets.yaml")
	// Interval comes from config.yaml; base_url + device_token from secrets.yaml.
	os.WriteFile(cfg, []byte("update_interval_seconds: 120\n"), 0644)
	os.WriteFile(sec, []byte("base_url: https://example.test\ndevice_token: tok-123\n"), 0644)

	s := loadSettings(cfg, sec)
	if s.BaseURL != "https://example.test" {
		t.Errorf("BaseURL = %q", s.BaseURL)
	}
	if s.Interval != 120*time.Second {
		t.Errorf("Interval = %s, want 2m", s.Interval)
	}
	if s.DeviceToken != "tok-123" {
		t.Errorf("DeviceToken = %q", s.DeviceToken)
	}
}
