package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tfi-display/config"
	"tfi-display/display"
	"tfi-display/display/driver"
	"tfi-display/gtfs"
	"tfi-display/remotelog"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	secretsPath := flag.String("secrets", "/etc/tfi-display/secrets.yaml", "path to secrets.yaml (api_key)")
	mock := flag.Bool("mock", false, "use mock display driver (writes PNG files)")
	mockDir := flag.String("mock-dir", "mock_output", "directory for mock PNG frames")
	verbose := flag.Bool("v", false, "enable debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg, err := config.LoadWithSecrets(*cfgPath, *secretsPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		os.Exit(1)
	}

	// Reports this device's own log lines to the update server, using the same
	// base_url/device_token tfi-agent already syncs config/releases with. A
	// diagnostic sink only — see remotelog package doc for the fire-and-forget
	// contract. rlog is safe to use even when base_url/device_token are unset
	// (Log becomes a no-op).
	rlog := remotelog.New(cfg.BaseURL, cfg.DeviceToken, remotelog.ParseLevel(cfg.RemoteLogLevel))
	rlog.Info("tfi-display starting up")

	// --- Static data ---
	stopNumbers := make([]string, len(cfg.Stops))
	for i, s := range cfg.Stops {
		stopNumbers[i] = s.StopNumber
	}

	slog.Info("loading static GTFS data…")
	db, err := gtfs.LoadOrBuild(cfg.StaticURL, cfg.DataDir, stopNumbers)
	if err != nil {
		slog.Error("loading static data", "err", err)
		rlog.Error("loading static GTFS data: " + err.Error())
		os.Exit(1)
	}

	// Hold the static DB behind an atomic pointer so the background refresher can
	// swap in a freshly rebuilt dataset while the poller and render loop keep
	// reading a consistent snapshot.
	dbHolder := gtfs.NewDB(db)

	// --- Live store & poller ---
	poller := gtfs.NewPoller(cfg.LiveURL, cfg.APIKey, dbHolder)
	poller.SetRemoteLogger(rlog)
	live := poller.Store()

	// Initial live data fetch.
	poller.Poll()

	// --- Display driver ---
	var drv driver.Driver
	if *mock {
		drv, err = driver.NewMockDriver(*mockDir)
		if err != nil {
			slog.Error("creating mock driver", "err", err)
			rlog.Error("creating mock driver: " + err.Error())
			os.Exit(1)
		}
	} else {
		drv, err = newHardwareDriver(cfg)
		if err != nil {
			slog.Error("opening hardware display", "err", err)
			rlog.Error("opening hardware display: " + err.Error())
			os.Exit(1)
		}
	}

	if err := drv.Init(); err != nil {
		slog.Error("display init", "err", err)
		rlog.Error("display init: " + err.Error())
		os.Exit(1)
	}

	// Build route filter map.
	routeFilter := gtfs.BuildRouteFilter(cfg.Routes)

	// --- Schedule ---
	var (
		schedEnabled bool
		schedStart   time.Time
		schedStop    time.Time
	)
	if cfg.StartTime != "" {
		schedEnabled = true
		schedStart, _ = time.Parse("15:04", cfg.StartTime)
		schedStop, _ = time.Parse("15:04", cfg.StopTime)
	}

	sleeping := false
	if schedEnabled && !isActiveTime(time.Now(), schedStart, schedStop) {
		slog.Info("outside active hours — display sleeping", "start", cfg.StartTime, "stop", cfg.StopTime)
		if err := drv.Sleep(); err != nil {
			slog.Warn("display sleep failed", "err", err)
			rlog.Warn("display sleep failed: " + err.Error())
		}
		sleeping = true
	} else {
		// Ensure display is unblanked on startup (guards against a previous manual blank).
		if err := drv.Wake(); err != nil {
			slog.Warn("display wake failed", "err", err)
			rlog.Warn("display wake failed: " + err.Error())
		}
	}

	// --- Goroutines ---

	// Background static-data refresh. TFI republishes the static GTFS feed
	// roughly weekly and the trip IDs change when it does; without a rebuild the
	// in-memory dataset goes stale and realtime updates stop matching, so every
	// arrival reverts to its scheduled time until the process is restarted.
	// Rebuilding parses the whole ZIP and is CPU-heavy, so we prefer to do it
	// while the display is asleep (overnight). A non-positive interval disables it.
	if cfg.StaticRefreshSec > 0 {
		go func() {
			interval := time.Duration(cfg.StaticRefreshSec) * time.Second
			// Check more often than the interval so we can seize a sleep window
			// promptly once one opens; the elapsed-time gate below keeps actual
			// network calls down to ~once per interval.
			check := time.NewTicker(15 * time.Minute)
			defer check.Stop()
			lastRefresh := time.Now() // DB was just built/validated at startup
			for range check.C {
				if time.Since(lastRefresh) < interval {
					continue
				}
				// Prefer off-hours: if a schedule is configured and the display is
				// currently active, defer the rebuild — unless we've gone well past
				// the interval (2×), in which case refresh anyway so data can't go
				// stale on an always-on or rarely-sleeping board.
				if schedEnabled && isActiveTime(time.Now(), schedStart, schedStop) &&
					time.Since(lastRefresh) < 2*interval {
					continue
				}
				cur := dbHolder.Load()
				newDB, err := gtfs.MaybeRebuild(cfg.StaticURL, cfg.DataDir, stopNumbers, cur.Timestamp)
				lastRefresh = time.Now()
				if err != nil {
					slog.Warn("static refresh failed", "err", err)
					rlog.Warn("static refresh failed: " + err.Error())
					continue
				}
				if newDB != nil {
					dbHolder.Store(newDB)
					slog.Info("static data refreshed",
						"trips", len(newDB.Trips),
						"timestamp", newDB.Timestamp.Format(time.RFC3339))
					rlog.Info(fmt.Sprintf("static data refreshed: %d trips", len(newDB.Trips)))
				} else {
					slog.Debug("static data already current")
				}
			}
		}()
	}

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

	// --- Tickers ---
	refreshTicker := time.NewTicker(time.Duration(cfg.PollIntervalSec) * time.Second)
	defer refreshTicker.Stop()
	pageTicker := time.NewTicker(time.Duration(cfg.PageIntervalSec) * time.Second)
	defer pageTicker.Stop()

	var scheduleTicker *time.Ticker
	var scheduleCh <-chan time.Time
	if schedEnabled {
		scheduleTicker = time.NewTicker(time.Minute)
		defer scheduleTicker.Stop()
		scheduleCh = scheduleTicker.C
	}

	page := 0

	// One renderer reused for the whole loop so the ~600 KB frame buffer is
	// allocated once, not on every refresh/page tick.
	renderer := &display.Renderer{}

	// Reused across frames so the per-frame render allocates nothing for the
	// section-list header (its length is fixed by the configured stops).
	sections := make([]display.StopSection, len(cfg.Stops))
	// One arrivals scratch slice per stop, fed back to QueryArrivalsInto each
	// frame so the query reuses its backing array instead of allocating one per
	// render (see QueryArrivalsInto). The slices live for the whole process.
	arrScratch := make([][]gtfs.Arrival, len(cfg.Stops))

	// Render immediately on start (if awake).
	if !sleeping {
		renderAndDisplay(renderer, sections, arrScratch, drv, dbHolder, live, cfg, routeFilter, page, rlog)
	}

	// Signal handler for graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	for {
		select {
		case <-scheduleCh:
			active := isActiveTime(time.Now(), schedStart, schedStop)
			if sleeping && active {
				slog.Info("entering active hours — waking display")
				rlog.Info("entering active hours — waking display")
				if err := drv.Wake(); err != nil {
					slog.Warn("display wake failed", "err", err)
					rlog.Warn("display wake failed: " + err.Error())
				}
				sleeping = false
				renderAndDisplay(renderer, sections, arrScratch, drv, dbHolder, live, cfg, routeFilter, page, rlog)
			} else if !sleeping && !active {
				slog.Info("outside active hours — sleeping display")
				rlog.Info("outside active hours — sleeping display")
				drv.Clear()
				if err := drv.Sleep(); err != nil {
					slog.Warn("display sleep failed", "err", err)
					rlog.Warn("display sleep failed: " + err.Error())
				}
				sleeping = true
			}
		case <-refreshTicker.C:
			if !sleeping {
				renderAndDisplay(renderer, sections, arrScratch, drv, dbHolder, live, cfg, routeFilter, page, rlog)
			}
		case <-pageTicker.C:
			if !sleeping {
				page++
				renderAndDisplay(renderer, sections, arrScratch, drv, dbHolder, live, cfg, routeFilter, page, rlog)
			}
		case sig := <-quit:
			slog.Info("shutting down", "signal", sig)
			rlog.Info(fmt.Sprintf("shutting down (signal %s)", sig))
			drv.Sleep()
			// rlog.Info above fires from a background goroutine; give it a brief
			// window to actually reach the network before os.Exit tears the
			// process down, otherwise the shutdown log is silently lost.
			time.Sleep(200 * time.Millisecond)
			os.Exit(0)
		}
	}
}

// isActiveTime reports whether now falls within the [start, stop) window.
// It handles overnight ranges (stop < start), e.g. 22:00–06:00.
func isActiveTime(now, start, stop time.Time) bool {
	nowM := now.Hour()*60 + now.Minute()
	startM := start.Hour()*60 + start.Minute()
	stopM := stop.Hour()*60 + stop.Minute()
	if startM == stopM {
		return true // degenerate: treat as always active
	}
	if startM < stopM {
		return nowM >= startM && nowM < stopM
	}
	// Overnight: active from start until midnight, and from midnight until stop.
	return nowM >= startM || nowM < stopM
}

// renderAndDisplay queries arrivals per stop and pushes a new frame to the display.
// page selects which window of cfg.PageSize arrivals to show; it wraps per-section.
func renderAndDisplay(
	renderer *display.Renderer,
	sections []display.StopSection,
	arrScratch [][]gtfs.Arrival,
	drv driver.Driver,
	dbHolder *gtfs.DB,
	live *gtfs.LiveStore,
	cfg *config.Config,
	routeFilter map[string]bool,
	page int,
	rlog *remotelog.Client,
) {
	// Load the current snapshot each render so a background refresh is picked up.
	db := dbHolder.Load()
	now := time.Now()
	updated := live.PollTime()
	if updated.IsZero() {
		updated = now
	}

	pageSize := display.RowsPerSection(len(cfg.Stops), drv.Width(), drv.Height())

	// sections is caller-owned and reused across frames; fill it in place.
	totalArrivals := 0
	for i, s := range cfg.Stops {
		// Reuse this stop's scratch backing across frames (fed back below) so the
		// query allocates nothing for arrivals in steady state.
		full := gtfs.QueryArrivalsInto(arrScratch[i], db, live, s.StopNumber, now, cfg.MaxMinutes, s.WalkingMinutes, routeFilter)
		arrScratch[i] = full
		totalArrivals += len(full)
		start, end := pageWindow(len(full), pageSize, cfg.MaxPages, page)
		sections[i] = display.StopSection{Label: s.Label, Arrivals: full[start:end]}
	}

	img := renderer.Render(sections, now, updated, drv.Width(), drv.Height())
	if err := drv.DisplayFrame(img); err != nil {
		slog.Error("display frame", "err", err)
		rlog.Error("display frame: " + err.Error())
	} else {
		slog.Info("display updated", "arrivals", totalArrivals, "time", now.Format("15:04:05"))
	}
}

// pageWindow returns the [start, end) bounds of the arrival slice to show for the
// given page of one section. total is the section's arrival count, pageSize the
// number of rows that fit, maxPages an optional cap on cycling (0 = unlimited),
// and page the monotonically increasing tick counter (wrapped per section).
//
// It is the pure core of renderAndDisplay's paging so the wrap-around arithmetic
// can be unit-tested. When pageSize <= 0 or total == 0 it returns the full range,
// so the caller can slice unconditionally without changing behaviour.
func pageWindow(total, pageSize, maxPages, page int) (start, end int) {
	if total <= 0 || pageSize <= 0 {
		return 0, total
	}
	numPages := (total + pageSize - 1) / pageSize
	if maxPages > 0 && numPages > maxPages {
		numPages = maxPages
	}
	p := page % numPages
	start = p * pageSize
	end = start + pageSize
	if end > total {
		end = total
	}
	return start, end
}
