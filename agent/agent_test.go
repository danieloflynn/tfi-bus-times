package agent

import (
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

// testServer is a configurable stand-in for the dandev API.
type testServer struct {
	latestVersion string
	downloadBody  string
	configBody    string

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

// --- binary sync ---

func TestCheckBinary_InstallsWhenDifferent(t *testing.T) {
	ts := &testServer{latestVersion: "v2", downloadBody: "BINARY-V2"}
	srv := ts.start(t)
	a, fu := newTestAgent(t, srv.URL)
	a.writeInstalledVersion("v1")

	if err := a.checkBinary(); err != nil {
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

	if err := a.checkBinary(); err != nil {
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

	if err := a.checkBinary(); err != nil {
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

	if err := a.checkBinary(); err == nil {
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

// --- config sync ---

func TestCheckConfig_AppliesWhenDifferent(t *testing.T) {
	ts := &testServer{configBody: "stops:\n  - stop_number: \"1\"\n"}
	srv := ts.start(t)
	a, fu := newTestAgent(t, srv.URL)
	a.deviceToken = "dev-token"
	os.WriteFile(a.configPath, []byte("old config"), 0644)

	if err := a.checkConfig(); err != nil {
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

	if err := a.checkConfig(); err != nil {
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

	if err := a.checkConfig(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fu.applyCalled != 0 {
		t.Errorf("applyConfig should not be called without a device token")
	}
}

// --- settings ---

func TestLoadSettings_Defaults(t *testing.T) {
	s := loadSettings("/nonexistent/config.yaml", "/nonexistent/secrets.yaml")
	if s.BaseURL != defaultBaseURL {
		t.Errorf("BaseURL = %q, want default", s.BaseURL)
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
	os.WriteFile(cfg, []byte("update_interval_seconds: 120\nupdate_base_url: https://example.test\n"), 0644)
	os.WriteFile(sec, []byte("device_token: tok-123\n"), 0644)

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
