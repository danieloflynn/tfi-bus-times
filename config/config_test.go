package config

import (
	"os"
	"path/filepath"
	"testing"
)

const validStops = `
stops:
  - stop_number: "1234"
    label: My Stop
`

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("writeTemp: %v", err)
	}
	return p
}

// --- Load (backward-compat) ---

func TestLoad_BackwardCompat(t *testing.T) {
	p := writeTemp(t, "config.yaml", `api_key: "test-key"`+validStops)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIKey != "test-key" {
		t.Errorf("got APIKey %q, want %q", cfg.APIKey, "test-key")
	}
}

func TestLoad_MissingAPIKey(t *testing.T) {
	p := writeTemp(t, "config.yaml", validStops)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for missing api_key")
	}
}

func TestLoad_Defaults(t *testing.T) {
	p := writeTemp(t, "config.yaml", `api_key: "k"`+validStops)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
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
	if cfg.StaticRefreshSec != 86400 {
		t.Errorf("default StaticRefreshSec = %d, want 86400", cfg.StaticRefreshSec)
	}
}

// A negative static_refresh_seconds is an explicit opt-out and must be preserved
// (the refresher treats <= 0 as disabled), not overwritten by the default.
func TestLoad_StaticRefreshDisabled(t *testing.T) {
	p := writeTemp(t, "config.yaml", `api_key: "k"
static_refresh_seconds: -1`+validStops)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.StaticRefreshSec != -1 {
		t.Errorf("StaticRefreshSec = %d, want -1 (disabled, preserved)", cfg.StaticRefreshSec)
	}
}

func TestLoad_WalkingMinutes(t *testing.T) {
	const stops = `
stops:
  - stop_number: "1234"
    label: Near
    walking_minutes: 5
  - stop_number: "5678"
    label: Far
    walking_minutes: -3
  - stop_number: "9012"
    label: Default
`
	p := writeTemp(t, "config.yaml", `api_key: "k"`+stops)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Stops[0].WalkingMinutes != 5 {
		t.Errorf("stop 0 walking_minutes = %d, want 5", cfg.Stops[0].WalkingMinutes)
	}
	// Negative values are clamped to 0 (no filtering).
	if cfg.Stops[1].WalkingMinutes != 0 {
		t.Errorf("negative walking_minutes should clamp to 0, got %d", cfg.Stops[1].WalkingMinutes)
	}
	// Omitted defaults to 0.
	if cfg.Stops[2].WalkingMinutes != 0 {
		t.Errorf("omitted walking_minutes should be 0, got %d", cfg.Stops[2].WalkingMinutes)
	}
}

// --- LoadWithSecrets ---

func TestLoadWithSecrets_FromSecretsFile(t *testing.T) {
	cfgPath := writeTemp(t, "config.yaml", validStops)
	secPath := writeTemp(t, "secrets.yaml", `api_key: "from-secrets"`)

	cfg, err := LoadWithSecrets(cfgPath, secPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIKey != "from-secrets" {
		t.Errorf("got APIKey %q, want %q", cfg.APIKey, "from-secrets")
	}
}

func TestLoadWithSecrets_SecretsFileOverridesConfigFile(t *testing.T) {
	cfgPath := writeTemp(t, "config.yaml", `api_key: "from-config"`+validStops)
	secPath := writeTemp(t, "secrets.yaml", `api_key: "from-secrets"`)

	cfg, err := LoadWithSecrets(cfgPath, secPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIKey != "from-secrets" {
		t.Errorf("secrets file should take priority; got %q", cfg.APIKey)
	}
}

func TestLoadWithSecrets_FromEnvVar(t *testing.T) {
	t.Setenv("TFI_API_KEY", "env-key")
	cfgPath := writeTemp(t, "config.yaml", validStops)
	// Non-existent secrets path — falls through to env var.
	cfg, err := LoadWithSecrets(cfgPath, "/nonexistent/secrets.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIKey != "env-key" {
		t.Errorf("got APIKey %q, want %q", cfg.APIKey, "env-key")
	}
}

func TestLoadWithSecrets_FromConfigFallback(t *testing.T) {
	cfgPath := writeTemp(t, "config.yaml", `api_key: "config-key"`+validStops)
	// Empty secrets path — skips secrets file, no env var set.
	cfg, err := LoadWithSecrets(cfgPath, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIKey != "config-key" {
		t.Errorf("got APIKey %q, want %q", cfg.APIKey, "config-key")
	}
}

func TestLoadWithSecrets_NoKey(t *testing.T) {
	cfgPath := writeTemp(t, "config.yaml", validStops)
	_, err := LoadWithSecrets(cfgPath, "")
	if err == nil {
		t.Fatal("expected error when no api_key from any source")
	}
}

func TestLoadWithSecrets_MissingSecretsFileNonFatal(t *testing.T) {
	t.Setenv("TFI_API_KEY", "env-fallback")
	cfgPath := writeTemp(t, "config.yaml", validStops)
	// Secrets file does not exist — should warn and fall through to env var.
	cfg, err := LoadWithSecrets(cfgPath, "/nonexistent/path/secrets.yaml")
	if err != nil {
		t.Fatalf("missing secrets file should be non-fatal; got: %v", err)
	}
	if cfg.APIKey != "env-fallback" {
		t.Errorf("got APIKey %q, want %q", cfg.APIKey, "env-fallback")
	}
}

func TestLoadWithSecrets_MalformedSecretsFile(t *testing.T) {
	cfgPath := writeTemp(t, "config.yaml", validStops)
	secPath := writeTemp(t, "secrets.yaml", ":: not valid yaml ::")
	_, err := LoadWithSecrets(cfgPath, secPath)
	if err == nil {
		t.Fatal("expected error for malformed secrets file")
	}
}
