package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// StopConfig identifies one physical bus stop.
type StopConfig struct {
	StopNumber string `yaml:"stop_number"`
	Label      string `yaml:"label"`
}

// Config is loaded from config.yaml at startup.
type Config struct {
	APIKey            string       `yaml:"api_key"`
	StaticURL         string       `yaml:"static_url"`
	LiveURL           string       `yaml:"live_url"`
	Stops             []StopConfig `yaml:"stops"`
	Routes            []string     `yaml:"routes"` // empty = show all routes
	PollIntervalSec   int          `yaml:"poll_interval_seconds"`
	MaxMinutes        int          `yaml:"max_minutes"`
	MaxPages          int          `yaml:"max_pages"`             // max pages to cycle (0 = unlimited)
	PageIntervalSec   int          `yaml:"page_interval_seconds"` // seconds between pages (default: 5)
	DisplayModel      string       `yaml:"display_model"`      // "lcd"
	FramebufferDevice string       `yaml:"framebuffer_device"` // default "/dev/fb0"
	DataDir           string       `yaml:"data_dir"`
	StartTime         string       `yaml:"start_time"` // e.g. "07:00" — display powers on
	StopTime          string       `yaml:"stop_time"`  // e.g. "22:00" — display powers off
}

type secretsFile struct {
	APIKey string `yaml:"api_key"`
}

// loadFile reads and parses a YAML config file, applies defaults, and validates
// structure (schedule times, stops). It does NOT validate api_key.
func loadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Defaults
	if cfg.StaticURL == "" {
		cfg.StaticURL = "https://www.transportforireland.ie/transitData/Data/GTFS_Realtime.zip"
	}
	if cfg.LiveURL == "" {
		cfg.LiveURL = "https://api.nationaltransport.ie/gtfsr/v2/TripUpdates"
	}
	if cfg.PollIntervalSec == 0 {
		cfg.PollIntervalSec = 60
	}
	if cfg.MaxMinutes == 0 {
		cfg.MaxMinutes = 90
	}
	if cfg.PageIntervalSec == 0 {
		cfg.PageIntervalSec = 5
	}
	if cfg.DisplayModel == "" {
		cfg.DisplayModel = "lcd"
	}
	if cfg.FramebufferDevice == "" {
		cfg.FramebufferDevice = "/dev/fb0"
	}
	if cfg.DataDir == "" {
		if cacheDir, err := os.UserCacheDir(); err == nil {
			cfg.DataDir = filepath.Join(cacheDir, "tfi-display")
		} else {
			cfg.DataDir = filepath.Join(os.TempDir(), "tfi-display")
		}
	}

	// Schedule validation: both must be set, or neither.
	if (cfg.StartTime == "") != (cfg.StopTime == "") {
		return nil, fmt.Errorf("start_time and stop_time must both be set or both be unset")
	}
	if cfg.StartTime != "" {
		if _, err := time.Parse("15:04", cfg.StartTime); err != nil {
			return nil, fmt.Errorf("invalid start_time %q: expected HH:MM 24-hour format", cfg.StartTime)
		}
		if _, err := time.Parse("15:04", cfg.StopTime); err != nil {
			return nil, fmt.Errorf("invalid stop_time %q: expected HH:MM 24-hour format", cfg.StopTime)
		}
	}

	if len(cfg.Stops) == 0 {
		return nil, fmt.Errorf("at least one stop must be configured")
	}
	for i, s := range cfg.Stops {
		if s.StopNumber == "" {
			return nil, fmt.Errorf("stop[%d]: stop_number is required", i)
		}
	}

	return &cfg, nil
}

// Load reads and validates a single YAML config file (may include api_key), applying defaults.
// Preserved for tests and one-file setups.
func Load(path string) (*Config, error) {
	cfg, err := loadFile(path)
	if err != nil {
		return nil, err
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("api_key is required")
	}
	return cfg, nil
}

// LoadWithSecrets loads config.yaml then sources api_key from (in priority order):
//  1. secretsPath file, if non-empty and the file exists
//  2. TFI_API_KEY environment variable
//  3. api_key field already present in config.yaml (transitional compat)
//
// Returns an error if api_key is still empty after all sources.
// A missing secrets file is non-fatal; a malformed one is an error.
func LoadWithSecrets(configPath, secretsPath string) (*Config, error) {
	cfg, err := loadFile(configPath)
	if err != nil {
		return nil, err
	}

	// 1. Secrets file (highest priority — overrides any key baked into config.yaml)
	if secretsPath != "" {
		data, err := os.ReadFile(secretsPath)
		if err == nil {
			var s secretsFile
			if err := yaml.Unmarshal(data, &s); err != nil {
				return nil, fmt.Errorf("parsing secrets file %q: %w", secretsPath, err)
			}
			if s.APIKey != "" {
				cfg.APIKey = s.APIKey
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading secrets file %q: %w", secretsPath, err)
		} else {
			log.Printf("secrets file %q not found, falling back to TFI_API_KEY env var or config file", secretsPath)
		}
	}

	// 2. Environment variable (overrides config.yaml but not secrets file)
	if cfg.APIKey == "" {
		if key := os.Getenv("TFI_API_KEY"); key != "" {
			cfg.APIKey = key
		}
	}

	// 3. api_key in config.yaml is already in cfg.APIKey from loadFile (transitional compat)

	if cfg.APIKey == "" {
		return nil, fmt.Errorf("api_key is required: set in %s or TFI_API_KEY env var", secretsPath)
	}
	return cfg, nil
}
