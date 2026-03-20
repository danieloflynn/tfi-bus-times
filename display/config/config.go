package config

import (
	"fmt"
	"os"
	"path/filepath"

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
	DisplayModel      string       `yaml:"display_model"`      // "lcd"
	FramebufferDevice string       `yaml:"framebuffer_device"` // default "/dev/fb0"
	DataDir           string       `yaml:"data_dir"`
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
