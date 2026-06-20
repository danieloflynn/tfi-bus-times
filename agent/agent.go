// Package agent is the tfi-agent foundation layer: a long-running process that
// periodically syncs the tfi-display binary and its config from a self-hosted
// update server, delegating the risky install/restart/rollback work to the
// updater package.
//
// This is entirely optional. tfi-display runs standalone with no network
// dependency beyond the TFI API itself; tfi-agent is a separate binary for
// anyone who wants to run their own update server and push releases to
// multiple devices. See README.md for the API contract it expects.
//
// It is deliberately decoupled from the display's own config validation — the
// agent must keep running (and keep fetching) even when the on-disk config is
// broken, since fixing that is part of its job.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"tfi-display/updater"
)

const (
	// defaultInterval is the poll cadence used until config.yaml sets one.
	defaultInterval = time.Hour
	// defaultVersionFile records the version of the currently installed binary.
	defaultVersionFile = "/usr/local/bin/tfi-display.version"
	// defaultBadVersionsFile lists versions that failed to install, so the agent
	// does not retry them every cycle. Persisted so it survives agent restarts.
	defaultBadVersionsFile = "/var/lib/tfi-agent/bad-versions"
	binaryName             = "tfi-display"
	httpTimeout            = 30 * time.Second
	// defaultRetryAttempts/Delay ride out the 502s fly returns while a
	// scale-to-zero machine cold-starts: the first request triggers the start,
	// and a retry a few seconds later hits the now-running app.
	defaultRetryAttempts = 4
	defaultRetryDelay    = 5 * time.Second
)

// Agent holds the resolved runtime state for one polling loop.
type Agent struct {
	configPath  string
	secretsPath string

	// Resolved fresh each cycle from configPath/secretsPath (with defaults), so
	// a newly fetched config.yaml can change the cadence, base URL, or token.
	baseURL     string
	deviceToken string
	interval    time.Duration

	updaterCfg      updater.Config
	versionFile     string
	badVersionsFile string
	serviceName     string
	waitTimeout     time.Duration
	http            *http.Client
	retryAttempts   int
	retryDelay      time.Duration

	// Hooks over the updater package, swapped out in tests so unit tests never
	// touch systemctl or the real filesystem layout.
	runUpdate   func(updater.Config) error
	applyConfig func(content []byte, configPath, service string, timeout time.Duration) error
}

// New builds an Agent that reads its settings from configPath and secretsPath
// and installs into the standard system locations.
func New(configPath, secretsPath string) (*Agent, error) {
	uc, err := updater.DefaultConfig()
	if err != nil {
		return nil, err
	}
	return &Agent{
		configPath:      configPath,
		secretsPath:     secretsPath,
		updaterCfg:      uc,
		versionFile:     defaultVersionFile,
		badVersionsFile: defaultBadVersionsFile,
		serviceName:     uc.ServiceName,
		waitTimeout:     uc.WaitTimeout,
		http:            &http.Client{Timeout: httpTimeout},
		retryAttempts:   defaultRetryAttempts,
		retryDelay:      defaultRetryDelay,
		runUpdate:       updater.Run,
		applyConfig:     updater.ApplyConfig,
	}, nil
}

// Run loops until ctx is cancelled. Each iteration reloads settings, runs one
// sync cycle, then waits for the configured interval. A failed cycle is logged
// and retried next tick — the running display is never disturbed on failure.
func (a *Agent) Run(ctx context.Context) error {
	for {
		a.reloadSettings()
		a.cycle()

		select {
		case <-ctx.Done():
			log.Printf("tfi-agent: shutting down")
			return nil
		case <-time.After(a.interval):
		}
	}
}

// cycle runs the binary check then the config check. The two are independent:
// a config edit must not wait on a binary release, and vice versa.
func (a *Agent) cycle() {
	if a.baseURL == "" {
		log.Printf("tfi-agent: no base_url configured in secrets.yaml — skipping")
		return
	}
	if err := a.checkBinary(); err != nil {
		log.Printf("tfi-agent: binary check: %v", err)
	}
	if err := a.checkConfig(); err != nil {
		log.Printf("tfi-agent: config check: %v", err)
	}
}

// --- settings ---

type fileSettings struct {
	UpdateIntervalSec int `yaml:"update_interval_seconds"`
}

type secretSettings struct {
	BaseURL     string `yaml:"base_url"`
	DeviceToken string `yaml:"device_token"`
}

type settings struct {
	BaseURL     string
	DeviceToken string
	Interval    time.Duration
}

func (a *Agent) reloadSettings() {
	s := loadSettings(a.configPath, a.secretsPath)
	a.baseURL = s.BaseURL
	a.deviceToken = s.DeviceToken
	a.interval = s.Interval
}

// loadSettings reads only the fields the agent needs, leniently: missing or
// unparseable files fall back to defaults rather than stopping the agent.
//
// base_url and device_token come from secrets.yaml — the one file the agent
// never overwrites, so the API origin survives config syncs. The poll interval
// lives in config.yaml so it can be managed centrally (and re-read each cycle).
func loadSettings(configPath, secretsPath string) settings {
	s := settings{Interval: defaultInterval}

	if data, err := os.ReadFile(configPath); err == nil {
		var fs fileSettings
		if err := yaml.Unmarshal(data, &fs); err == nil {
			if fs.UpdateIntervalSec > 0 {
				s.Interval = time.Duration(fs.UpdateIntervalSec) * time.Second
			}
		} else {
			log.Printf("tfi-agent: parsing %s for interval: %v (using default)", configPath, err)
		}
	}

	if data, err := os.ReadFile(secretsPath); err == nil {
		var ss secretSettings
		if err := yaml.Unmarshal(data, &ss); err == nil {
			s.BaseURL = ss.BaseURL
			s.DeviceToken = ss.DeviceToken
		} else {
			log.Printf("tfi-agent: parsing %s for base_url/device_token: %v", secretsPath, err)
		}
	}

	return s
}

// --- http with retry ---

// doWithRetry sends the request built by newReq, retrying on network errors and
// 5xx responses. The dandev app runs scale-to-zero on fly, so a stopped machine
// 502s the first (waking) request; a retry a few seconds later succeeds. newReq
// is a factory because each attempt needs a fresh request (and body reader).
//
// A returned response is the caller's to close. 4xx responses are returned
// without retry — they are not transient.
func (a *Agent) doWithRetry(newReq func() (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= a.retryAttempts; attempt++ {
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		resp, err := a.http.Do(req)
		switch {
		case err != nil:
			lastErr = err
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("server returned %s", resp.Status)
			resp.Body.Close()
		default:
			return resp, nil
		}
		if attempt < a.retryAttempts {
			log.Printf("tfi-agent: %v (attempt %d/%d) — retrying in %s", lastErr, attempt, a.retryAttempts, a.retryDelay)
			time.Sleep(a.retryDelay)
		}
	}
	return nil, lastErr
}

// --- binary sync ---

type latestResponse struct {
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
}

func (a *Agent) checkBinary() error {
	resp, err := a.doWithRetry(func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, a.baseURL+"/api/tfi/v1/latest", nil)
	})
	if err != nil {
		return fmt.Errorf("fetching latest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("latest returned %s", resp.Status)
	}

	var latest latestResponse
	if err := json.NewDecoder(resp.Body).Decode(&latest); err != nil {
		return fmt.Errorf("decoding latest: %w", err)
	}
	if latest.Version == "" || latest.DownloadURL == "" {
		return fmt.Errorf("latest response missing version or download_url")
	}

	installed := a.installedVersion()
	// "Different", not "newer": this lets a central rollback (re-pointing the
	// release to an older build) flow down to the device.
	if latest.Version == installed {
		log.Printf("tfi-agent: binary up to date (%s)", installed)
		return nil
	}
	if a.isBadVersion(latest.Version) {
		log.Printf("tfi-agent: skipping known-bad version %s", latest.Version)
		return nil
	}

	log.Printf("tfi-agent: new version %s (installed %q) — downloading", latest.Version, installed)
	staged := filepath.Join(a.updaterCfg.StagingDir, binaryName)
	if err := a.downloadFile(latest.DownloadURL, staged); err != nil {
		return fmt.Errorf("downloading binary: %w", err)
	}

	if err := a.runUpdate(a.updaterCfg); err != nil {
		// updater.Run has already rolled back. Remember the bad version so we do
		// not retry it every cycle, and report it centrally.
		log.Printf("tfi-agent: update to %s failed: %v — recording and reporting", latest.Version, err)
		if mErr := a.markBadVersion(latest.Version); mErr != nil {
			log.Printf("tfi-agent: recording bad version: %v", mErr)
		}
		if rErr := a.reportFailure(latest.Version, err.Error()); rErr != nil {
			log.Printf("tfi-agent: reporting failure: %v", rErr)
		}
		return fmt.Errorf("update failed: %w", err)
	}

	if err := a.writeInstalledVersion(latest.Version); err != nil {
		log.Printf("tfi-agent: writing version marker: %v", err)
	}
	log.Printf("tfi-agent: updated to %s", latest.Version)
	return nil
}

// --- config sync ---

func (a *Agent) checkConfig() error {
	if a.deviceToken == "" {
		log.Printf("tfi-agent: no device_token configured — skipping config sync")
		return nil
	}

	resp, err := a.doWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, a.baseURL+"/api/tfi/v1/config_files/fetch", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+a.deviceToken)
		return req, nil
	})
	if err != nil {
		return fmt.Errorf("fetching config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("config fetch returned %s", resp.Status)
	}
	remote, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading config body: %w", err)
	}

	current, err := os.ReadFile(a.configPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading current config: %w", err)
	}
	if err == nil && bytes.Equal(current, remote) {
		log.Printf("tfi-agent: config up to date")
		return nil
	}

	log.Printf("tfi-agent: config changed — applying")
	if err := a.applyConfig(remote, a.configPath, a.serviceName, a.waitTimeout); err != nil {
		return fmt.Errorf("applying config: %w", err)
	}
	log.Printf("tfi-agent: config updated")
	return nil
}

// --- failure reporting ---

func (a *Agent) reportFailure(version, errMsg string) error {
	if a.deviceToken == "" {
		return fmt.Errorf("no device_token — cannot report failure")
	}
	payload := map[string]map[string]string{
		"release_failure": {"version": version, "error": errMsg},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := a.doWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, a.baseURL+"/api/tfi/v1/releases/report", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+a.deviceToken)
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("report returned %s", resp.Status)
	}
	return nil
}

// --- helpers: download + state files ---

func (a *Agent) downloadFile(url, dest string) error {
	resp, err := a.http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %s", resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}
	return nil
}

func (a *Agent) installedVersion() string {
	data, err := os.ReadFile(a.versionFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (a *Agent) writeInstalledVersion(v string) error {
	if err := os.MkdirAll(filepath.Dir(a.versionFile), 0755); err != nil {
		return err
	}
	return os.WriteFile(a.versionFile, []byte(v+"\n"), 0644)
}

func (a *Agent) isBadVersion(v string) bool {
	data, err := os.ReadFile(a.badVersionsFile)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == v {
			return true
		}
	}
	return false
}

func (a *Agent) markBadVersion(v string) error {
	if a.isBadVersion(v) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(a.badVersionsFile), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(a.badVersionsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(v + "\n")
	return err
}
