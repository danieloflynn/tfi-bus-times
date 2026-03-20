package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tfi-display/config"
	"tfi-display/display"
	"tfi-display/display/driver"
	"tfi-display/gtfs"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	mock := flag.Bool("mock", false, "use mock display driver (writes PNG files)")
	mockDir := flag.String("mock-dir", "mock_output", "directory for mock PNG frames")
	verbose := flag.Bool("v", false, "enable debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		os.Exit(1)
	}

	// --- Static data ---
	stopNumbers := make([]string, len(cfg.Stops))
	for i, s := range cfg.Stops {
		stopNumbers[i] = s.StopNumber
	}

	slog.Info("loading static GTFS data…")
	db, err := gtfs.LoadOrBuild(cfg.StaticURL, cfg.DataDir, stopNumbers)
	if err != nil {
		slog.Error("loading static data", "err", err)
		os.Exit(1)
	}

	// --- Live store & poller ---
	poller := gtfs.NewPoller(cfg.LiveURL, cfg.APIKey, db)
	live := poller.Store()

	// Initial live data fetch.
	poller.Poll()

	// --- Display driver ---
	var drv driver.Driver
	if *mock {
		drv, err = driver.NewMockDriver(*mockDir)
		if err != nil {
			slog.Error("creating mock driver", "err", err)
			os.Exit(1)
		}
	} else {
		drv, err = newHardwareDriver(cfg)
		if err != nil {
			slog.Error("opening hardware display", "err", err)
			os.Exit(1)
		}
	}

	if err := drv.Init(); err != nil {
		slog.Error("display init", "err", err)
		os.Exit(1)
	}

	// Build route filter map.
	routeFilter := gtfs.BuildRouteFilter(cfg.Routes)

	// --- Goroutines ---

	// Live data poller.
	go func() {
		base := cfg.PollIntervalSec
		ticker := time.NewTicker(time.Duration(base) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if bo := poller.BackoffDuration(base); bo > 0 {
				slog.Debug("rate-limit backoff", "duration", bo)
				time.Sleep(bo)
			}
			poller.Poll()
		}
	}()

	// --- Display refresh ticker ---
	refreshTicker := time.NewTicker(time.Duration(cfg.PollIntervalSec) * time.Second)
	defer refreshTicker.Stop()

	// Render immediately on start.
	renderAndDisplay(drv, db, live, cfg, routeFilter)

	// Signal handler for graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	for {
		select {
		case <-refreshTicker.C:
			renderAndDisplay(drv, db, live, cfg, routeFilter)
		case sig := <-quit:
			slog.Info("shutting down", "signal", sig)
			drv.Sleep()
			os.Exit(0)
		}
	}
}

// renderAndDisplay queries arrivals per stop and pushes a new frame to the display.
func renderAndDisplay(
	drv driver.Driver,
	db *gtfs.StaticDB,
	live *gtfs.LiveStore,
	cfg *config.Config,
	routeFilter map[string]bool,
) {
	now := time.Now()

	sections := make([]display.StopSection, len(cfg.Stops))
	totalArrivals := 0
	for i, s := range cfg.Stops {
		arr := gtfs.QueryArrivals(db, live, s.StopNumber, now, cfg.MaxMinutes, routeFilter)
		sections[i] = display.StopSection{Label: s.Label, Arrivals: arr}
		totalArrivals += len(arr)
	}

	img := display.Render(sections, now, drv.Width(), drv.Height())
	if err := drv.DisplayFrame(img); err != nil {
		slog.Error("display frame", "err", err)
	} else {
		slog.Info("display updated", "arrivals", totalArrivals, "time", now.Format("15:04:05"))
	}
}
