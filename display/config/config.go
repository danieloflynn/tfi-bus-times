package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// StopConfig identifies one physical bus stop.
type StopConfig struct {
	StopNumber string `yaml:"stop_number"`
	Label      string `yaml:"label"`
}

// Config is loaded from config.yaml at startup.
type Config struct {
	APIKey          string       `yaml:"api_key"`
	StaticURL       string       `yaml:"static_url"`
	LiveURL         string       `yaml:"live_url"`
	Stops           []StopConfig `yaml:"stops"`
	Routes          []string     `yaml:"routes"` // empty = show all routes
	PollIntervalSec int          `yaml:"poll_interval_seconds"`
	MaxMinutes      int          `yaml:"max_minutes"`
	DisplayModel    string       `yaml:"display_model"` // "2.13" or "2.9"
	DataDir         string       `yaml:"data_dir"`
	SPIBus          int          `yaml:"spi_bus"`
	SPIChip         int          `yaml:"spi_chip"`
	PinDC           int          `yaml:"pin_dc"`
	PinRST          int          `yaml:"pin_rst"`
	PinBUSY         int          `yaml:"pin_busy"`
	VCOM            float64      `yaml:"vcom"` // IT8951 only; e.g. -2.06 (written on FPC ribbon)
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
		cfg.DisplayModel = "2.13"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "/var/lib/tfi-display"
	}
	if cfg.PinDC == 0 {
		cfg.PinDC = 25
	}
	if cfg.PinRST == 0 {
		cfg.PinRST = 17
	}
	if cfg.PinBUSY == 0 {
		cfg.PinBUSY = 24
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
