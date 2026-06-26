package config

import (
	"path/filepath"
	"testing"
)

// config_edge_test.go (FR6, config) exhaustively covers loadFile's defaults and
// validation branches plus the LoadWithSecrets api_key priority chain. It reuses
// writeTemp from config_test.go and adds only configEdge*-prefixed identifiers so
// it never collides with the sibling config_test.go in the same package.

// configEdgeMinimalStops is the smallest stops block that satisfies loadFile's
// "at least one stop" rule, letting other fields fall to their defaults.
const configEdgeMinimalStops = `
stops:
  - stop_number: "478"
    label: Edge Stop
`

// --- loadFile defaults ---

// TestConfigEdge_Defaults asserts every default loadFile applies when the
// corresponding YAML field is omitted: the two upstream URLs, the three interval
// knobs, the display model, the framebuffer device, and the derived data dir.
func TestConfigEdge_Defaults(t *testing.T) {
	p := writeTemp(t, "config.yaml", configEdgeMinimalStops)
	cfg, err := loadFile(p)
	if err != nil {
		t.Fatalf("loadFile: unexpected error: %v", err)
	}

	const wantStatic = "https://www.transportforireland.ie/transitData/Data/GTFS_Realtime.zip"
	if cfg.StaticURL != wantStatic {
		t.Errorf("default StaticURL = %q, want %q", cfg.StaticURL, wantStatic)
	}
	const wantLive = "https://api.nationaltransport.ie/gtfsr/v2/TripUpdates"
	if cfg.LiveURL != wantLive {
		t.Errorf("default LiveURL = %q, want %q", cfg.LiveURL, wantLive)
	}
	if cfg.PollIntervalSec != 60 {
		t.Errorf("default PollIntervalSec = %d, want 60", cfg.PollIntervalSec)
	}
	if cfg.MaxMinutes != 90 {
		t.Errorf("default MaxMinutes = %d, want 90", cfg.MaxMinutes)
	}
	if cfg.PageIntervalSec != 5 {
		t.Errorf("default PageIntervalSec = %d, want 5", cfg.PageIntervalSec)
	}
	if cfg.DisplayModel != "lcd" {
		t.Errorf("default DisplayModel = %q, want %q", cfg.DisplayModel, "lcd")
	}
	if cfg.FramebufferDevice != "/dev/fb0" {
		t.Errorf("default FramebufferDevice = %q, want %q", cfg.FramebufferDevice, "/dev/fb0")
	}
	// DataDir is derived from os.UserCacheDir()/os.TempDir(); the host varies but
	// the final path element is always "tfi-display".
	if cfg.DataDir == "" {
		t.Error("default DataDir should not be empty")
	}
	if base := filepath.Base(cfg.DataDir); base != "tfi-display" {
		t.Errorf("default DataDir = %q, want last element %q", cfg.DataDir, "tfi-display")
	}
}

// TestConfigEdge_DefaultsNotOverridden confirms loadFile leaves explicitly-set
// fields alone instead of stamping defaults over them (the "== 0" / "== \"\""
// guards only fire on the zero value).
func TestConfigEdge_DefaultsNotOverridden(t *testing.T) {
	const yaml = `
static_url: "https://example.test/static.zip"
live_url: "https://example.test/live"
poll_interval_seconds: 30
max_minutes: 45
page_interval_seconds: 8
display_model: "custom"
framebuffer_device: "/dev/fb1"
data_dir: "/var/lib/custom"
` + configEdgeMinimalStops
	p := writeTemp(t, "config.yaml", yaml)
	cfg, err := loadFile(p)
	if err != nil {
		t.Fatalf("loadFile: unexpected error: %v", err)
	}
	if cfg.StaticURL != "https://example.test/static.zip" {
		t.Errorf("StaticURL overwritten: got %q", cfg.StaticURL)
	}
	if cfg.LiveURL != "https://example.test/live" {
		t.Errorf("LiveURL overwritten: got %q", cfg.LiveURL)
	}
	if cfg.PollIntervalSec != 30 {
		t.Errorf("PollIntervalSec overwritten: got %d", cfg.PollIntervalSec)
	}
	if cfg.MaxMinutes != 45 {
		t.Errorf("MaxMinutes overwritten: got %d", cfg.MaxMinutes)
	}
	if cfg.PageIntervalSec != 8 {
		t.Errorf("PageIntervalSec overwritten: got %d", cfg.PageIntervalSec)
	}
	if cfg.DisplayModel != "custom" {
		t.Errorf("DisplayModel overwritten: got %q", cfg.DisplayModel)
	}
	if cfg.FramebufferDevice != "/dev/fb1" {
		t.Errorf("FramebufferDevice overwritten: got %q", cfg.FramebufferDevice)
	}
	if cfg.DataDir != "/var/lib/custom" {
		t.Errorf("DataDir overwritten: got %q", cfg.DataDir)
	}
}

// TestConfigEdge_StaticRefreshPositivePreserved covers the static-refresh field
// distinct from config_test.go's TestLoad_StaticRefreshDisabled (negative kept)
// and TestLoad_Defaults (zero → 86400): a positive value must pass through
// untouched because only the literal zero triggers the daily default.
func TestConfigEdge_StaticRefreshPositivePreserved(t *testing.T) {
	const yaml = `static_refresh_seconds: 3600` + configEdgeMinimalStops
	p := writeTemp(t, "config.yaml", yaml)
	cfg, err := loadFile(p)
	if err != nil {
		t.Fatalf("loadFile: unexpected error: %v", err)
	}
	if cfg.StaticRefreshSec != 3600 {
		t.Errorf("StaticRefreshSec = %d, want 3600 (positive preserved)", cfg.StaticRefreshSec)
	}
}

// --- schedule validation ---

// TestConfigEdge_ScheduleOnlyStartTime: setting start_time without stop_time
// fails the both-or-neither rule.
func TestConfigEdge_ScheduleOnlyStartTime(t *testing.T) {
	const yaml = `start_time: "07:00"` + configEdgeMinimalStops
	p := writeTemp(t, "config.yaml", yaml)
	if _, err := loadFile(p); err == nil {
		t.Fatal("expected error when only start_time is set")
	}
}

// TestConfigEdge_ScheduleOnlyStopTime: setting stop_time without start_time
// fails the both-or-neither rule (the mirror of the above).
func TestConfigEdge_ScheduleOnlyStopTime(t *testing.T) {
	const yaml = `stop_time: "22:00"` + configEdgeMinimalStops
	p := writeTemp(t, "config.yaml", yaml)
	if _, err := loadFile(p); err == nil {
		t.Fatal("expected error when only stop_time is set")
	}
}

// TestConfigEdge_ScheduleInvalidStartTime: a non-HH:MM start_time (hour out of
// range) is rejected even though stop_time is valid.
func TestConfigEdge_ScheduleInvalidStartTime(t *testing.T) {
	const yaml = `
start_time: "25:00"
stop_time: "22:00"` + configEdgeMinimalStops
	p := writeTemp(t, "config.yaml", yaml)
	if _, err := loadFile(p); err == nil {
		t.Fatal("expected error for invalid start_time")
	}
}

// TestConfigEdge_ScheduleInvalidStopTime: a non-HH:MM stop_time (minute out of
// range) is rejected even though start_time is valid.
func TestConfigEdge_ScheduleInvalidStopTime(t *testing.T) {
	const yaml = `
start_time: "07:00"
stop_time: "10:75"` + configEdgeMinimalStops
	p := writeTemp(t, "config.yaml", yaml)
	if _, err := loadFile(p); err == nil {
		t.Fatal("expected error for invalid stop_time")
	}
}

// TestConfigEdge_ScheduleBothValid: a well-formed start/stop pair loads cleanly
// and the values survive verbatim.
func TestConfigEdge_ScheduleBothValid(t *testing.T) {
	const yaml = `
start_time: "07:00"
stop_time: "22:00"` + configEdgeMinimalStops
	p := writeTemp(t, "config.yaml", yaml)
	cfg, err := loadFile(p)
	if err != nil {
		t.Fatalf("loadFile: unexpected error for valid schedule: %v", err)
	}
	if cfg.StartTime != "07:00" {
		t.Errorf("StartTime = %q, want %q", cfg.StartTime, "07:00")
	}
	if cfg.StopTime != "22:00" {
		t.Errorf("StopTime = %q, want %q", cfg.StopTime, "22:00")
	}
}

// TestConfigEdge_ScheduleNeitherSet: omitting both times is valid (the display
// runs 24/7) and leaves both fields empty.
func TestConfigEdge_ScheduleNeitherSet(t *testing.T) {
	p := writeTemp(t, "config.yaml", configEdgeMinimalStops)
	cfg, err := loadFile(p)
	if err != nil {
		t.Fatalf("loadFile: unexpected error when no schedule set: %v", err)
	}
	if cfg.StartTime != "" || cfg.StopTime != "" {
		t.Errorf("expected empty schedule, got start=%q stop=%q", cfg.StartTime, cfg.StopTime)
	}
}

// --- stop validation ---

// TestConfigEdge_NegativeWalkingMinutesClamped: a negative walking_minutes is
// meaningless, so loadFile clamps it to 0 (filter disabled) rather than erroring.
func TestConfigEdge_NegativeWalkingMinutesClamped(t *testing.T) {
	const yaml = `
stops:
  - stop_number: "478"
    label: Clamp Me
    walking_minutes: -7
`
	p := writeTemp(t, "config.yaml", yaml)
	cfg, err := loadFile(p)
	if err != nil {
		t.Fatalf("loadFile: unexpected error: %v", err)
	}
	if cfg.Stops[0].WalkingMinutes != 0 {
		t.Errorf("negative walking_minutes should clamp to 0, got %d", cfg.Stops[0].WalkingMinutes)
	}
}

// TestConfigEdge_EmptyStops: a config with no stops is rejected.
func TestConfigEdge_EmptyStops(t *testing.T) {
	const yaml = `poll_interval_seconds: 60`
	p := writeTemp(t, "config.yaml", yaml)
	if _, err := loadFile(p); err == nil {
		t.Fatal("expected error when no stops are configured")
	}
}

// TestConfigEdge_StopMissingStopNumber: a stop entry without stop_number is
// rejected, and the error names the offending index.
func TestConfigEdge_StopMissingStopNumber(t *testing.T) {
	const yaml = `
stops:
  - label: Has No Number
`
	p := writeTemp(t, "config.yaml", yaml)
	if _, err := loadFile(p); err == nil {
		t.Fatal("expected error for stop missing stop_number")
	}
}

// --- LoadWithSecrets priority chain ---

// TestConfigEdge_SecretsFileOverridesConfig: the secrets file is the highest
// priority source and overrides an api_key baked into config.yaml.
func TestConfigEdge_SecretsFileOverridesConfig(t *testing.T) {
	cfgPath := writeTemp(t, "config.yaml", `api_key: "from-config"`+configEdgeMinimalStops)
	secPath := writeTemp(t, "secrets.yaml", `api_key: "from-secrets"`)
	cfg, err := LoadWithSecrets(cfgPath, secPath)
	if err != nil {
		t.Fatalf("LoadWithSecrets: unexpected error: %v", err)
	}
	if cfg.APIKey != "from-secrets" {
		t.Errorf("secrets file should win; got APIKey %q", cfg.APIKey)
	}
}

// TestConfigEdge_SecretsFileOverridesEnv: the secrets file also outranks the
// TFI_API_KEY environment variable.
func TestConfigEdge_SecretsFileOverridesEnv(t *testing.T) {
	t.Setenv("TFI_API_KEY", "from-env")
	cfgPath := writeTemp(t, "config.yaml", configEdgeMinimalStops)
	secPath := writeTemp(t, "secrets.yaml", `api_key: "from-secrets"`)
	cfg, err := LoadWithSecrets(cfgPath, secPath)
	if err != nil {
		t.Fatalf("LoadWithSecrets: unexpected error: %v", err)
	}
	if cfg.APIKey != "from-secrets" {
		t.Errorf("secrets file should outrank env; got APIKey %q", cfg.APIKey)
	}
}

// TestConfigEdge_EnvUsedWhenNoSecretsFile: with no secrets file and no api_key
// in config.yaml, TFI_API_KEY supplies the key.
func TestConfigEdge_EnvUsedWhenNoSecretsFile(t *testing.T) {
	t.Setenv("TFI_API_KEY", "from-env")
	cfgPath := writeTemp(t, "config.yaml", configEdgeMinimalStops)
	// Non-existent secrets path → missing file is non-fatal → fall through to env.
	cfg, err := LoadWithSecrets(cfgPath, "/nonexistent/edge/secrets.yaml")
	if err != nil {
		t.Fatalf("LoadWithSecrets: unexpected error: %v", err)
	}
	if cfg.APIKey != "from-env" {
		t.Errorf("env var should supply key; got APIKey %q", cfg.APIKey)
	}
}

// TestConfigEdge_ConfigKeyLastResort: with an empty secrets path and no env var,
// the api_key from config.yaml is used as the final fallback.
func TestConfigEdge_ConfigKeyLastResort(t *testing.T) {
	// Neutralise any ambient TFI_API_KEY so the config value is genuinely last.
	t.Setenv("TFI_API_KEY", "")
	cfgPath := writeTemp(t, "config.yaml", `api_key: "from-config"`+configEdgeMinimalStops)
	cfg, err := LoadWithSecrets(cfgPath, "")
	if err != nil {
		t.Fatalf("LoadWithSecrets: unexpected error: %v", err)
	}
	if cfg.APIKey != "from-config" {
		t.Errorf("config api_key should be last resort; got APIKey %q", cfg.APIKey)
	}
}

// TestConfigEdge_MissingSecretsFileNonFatal: a secrets path that does not exist
// is a warning, not an error, as long as another source provides the key.
func TestConfigEdge_MissingSecretsFileNonFatal(t *testing.T) {
	t.Setenv("TFI_API_KEY", "from-env")
	cfgPath := writeTemp(t, "config.yaml", configEdgeMinimalStops)
	cfg, err := LoadWithSecrets(cfgPath, "/nonexistent/edge/path/secrets.yaml")
	if err != nil {
		t.Fatalf("missing secrets file should be non-fatal; got: %v", err)
	}
	if cfg.APIKey != "from-env" {
		t.Errorf("got APIKey %q, want from-env", cfg.APIKey)
	}
}

// TestConfigEdge_SecretsFileEmptyKeyFallsThrough: a secrets file that exists but
// carries an empty api_key does not override; resolution continues to the env
// var (and would continue to config after that).
func TestConfigEdge_SecretsFileEmptyKeyFallsThrough(t *testing.T) {
	t.Setenv("TFI_API_KEY", "from-env")
	cfgPath := writeTemp(t, "config.yaml", configEdgeMinimalStops)
	secPath := writeTemp(t, "secrets.yaml", `api_key: ""`)
	cfg, err := LoadWithSecrets(cfgPath, secPath)
	if err != nil {
		t.Fatalf("LoadWithSecrets: unexpected error: %v", err)
	}
	if cfg.APIKey != "from-env" {
		t.Errorf("empty secrets key should fall through to env; got APIKey %q", cfg.APIKey)
	}
}

// TestConfigEdge_MalformedSecretsFile: a secrets file that is not valid YAML is
// a hard error (distinct from a missing file).
func TestConfigEdge_MalformedSecretsFile(t *testing.T) {
	cfgPath := writeTemp(t, "config.yaml", configEdgeMinimalStops)
	secPath := writeTemp(t, "secrets.yaml", ":: not valid yaml ::")
	if _, err := LoadWithSecrets(cfgPath, secPath); err == nil {
		t.Fatal("expected error for malformed secrets file")
	}
}

// TestConfigEdge_StillEmptyKeyErrors: when no source supplies an api_key, the
// resolved key is empty and LoadWithSecrets errors.
func TestConfigEdge_StillEmptyKeyErrors(t *testing.T) {
	t.Setenv("TFI_API_KEY", "")
	cfgPath := writeTemp(t, "config.yaml", configEdgeMinimalStops)
	if _, err := LoadWithSecrets(cfgPath, ""); err == nil {
		t.Fatal("expected error when no api_key from any source")
	}
}
