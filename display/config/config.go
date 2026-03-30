package config

import (
	"fmt"
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

// Load reads and validates a YAML config file, applying defaults.
func Load(path string) (*Config, error) {
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

	// Validation
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("api_key is required")
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
