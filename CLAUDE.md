# TFI Display

> **This file is a living document.** It must be kept up to date as the project evolves. Update it when new patterns are introduced, guidelines change, or the Current State no longer matches the application. If you notice it becoming outdated, raise the issue and update it — do not silently let it drift.

Real-time bus/tram departure board for Raspberry Pi. Fetches live GTFS data from the Transport for Ireland API and renders it to an LCD display via the Linux framebuffer (`/dev/fb0`). No desktop environment required — the binary writes pixels directly.

---

## Stack

| Tool                   | Version | Notes                                                   |
| ---------------------- | ------- | ------------------------------------------------------- |
| Go                     | 1.24    |                                                         |
| periph.io              | v3      | Hardware I/O for the LCD DPI driver                     |
| golang.org/x/image     | latest  | Font rendering and image utilities                      |
| gtfs-realtime-bindings | v1      | Protobuf bindings for GTFS-RT feed                      |
| google.golang.org/protobuf | v1.33 |                                                       |
| gopkg.in/yaml.v3       |         | Config file parsing                                     |

---

## Architecture

**`config/`** — YAML config loading with defaults and validation. `Config` is the single source of truth for all runtime settings. `config.yaml` is gitignored; `config.yaml.example` is the reference.

**`gtfs/`** — All GTFS logic:
- `static.go` — downloads the TFI GTFS ZIP, parses it into a `StaticDB` (stops, trips, services, calendar exceptions), and caches it as a gob file. Cache is invalidated by the upstream `Last-Modified` header or a schema version bump.
- `realtime.go` — polls the GTFS-RT TripUpdates endpoint, parses protobuf into a `LiveStore`. Handles delays (absolute timestamps or delta seconds), cancellations, and added trips. Includes exponential backoff on rate-limit errors.
- `arrivals.go` — `QueryArrivals` joins static and live data to produce a time-sorted `[]Arrival` for a stop. Applies calendar validity, cancellation checks, route filtering, and deduplication.

**`display/`** — Image rendering:
- `renderer.go` — small-display path (e-ink, < 800 px wide). All sections are merged into one sorted list. Uses `basicfont 7×13`.
- `renderer_hd.go` — HD path (≥ 800 px, e.g. the 1024×600 LCD). Each stop gets its own labelled section band. Column x-coordinates are scaled proportionally from a 1872-px base layout. Uses Atkinson Hyperlegible fonts from `fonts/`.
- `driver/driver.go` — `Driver` interface (`Init`, `DisplayFrame`, `Width`, `Height`, `Sleep`, `Wake`, `Clear`).
- `driver/lcd_dpi.go` — hardware implementation: writes raw RGB565 pixels to `/dev/fb0` via periph.io.
- `driver/mock.go` — writes PNG files to a local directory; no hardware required.

**`fonts/`** — Embeds `AtkinsonHyperlegible-Bold.ttf` and exposes pre-loaded `font.Face` values (`HeaderFace`, `BodyFace`, `RouteFace`, `SmallFace`, `TinyFace`) for the HD renderer.

**`hardware_linux.go` / `hardware_stub.go`** — build-tag separation. `hardware_linux.go` constructs the real LCD driver; `hardware_stub.go` is a no-op stub for non-Linux builds, keeping the binary buildable on macOS for mock runs. Never add periph.io imports outside these files or `driver/lcd_dpi.go`.

**`main.go`** — Orchestration: loads config, builds `StaticDB`, starts `Poller`, creates display driver. Three tickers drive the loop: `refreshTicker` (re-render on poll cadence), `pageTicker` (advance the arrival page), and optional `scheduleTicker` (wake/sleep by time of day). `renderAndDisplay` slices arrivals into pages via `RowsPerSection` and pushes each frame to the driver.

---

## Key Patterns

**GTFS static caching** — `LoadOrBuild` writes a gob file after the first parse. On subsequent starts it validates schema version, the configured stop list, and the upstream `Last-Modified` header before reusing the cache. Rebuilding is triggered automatically when any of these change.

**Hour-bucket indexing** — `StaticDB.StopTimes` is indexed `stopNumber → hour → []StopTime`. `QueryArrivals` scans only the relevant hour buckets (current hour ±1 plus lookahead), keeping query time sub-millisecond even for large feeds.

**12-hour rule for overnight trips** — GTFS allows arrival times > 24:00 (e.g. `25:30:00`). When reconstructing wall-clock time, if the scheduled seconds-since-midnight is more than 12 hours behind `now`, the arrival is treated as belonging to the next calendar day.

**Realtime overlay** — `LiveStore` is a concurrent-safe in-memory store updated by the poller goroutine. `QueryArrivals` reads from it under `RLock`. Delays are applied as an absolute Unix timestamp (preferred) or a delta-seconds offset.

**Paging** — `renderAndDisplay` uses `RowsPerSection` to determine how many rows fit per section at the current resolution, then windows into the arrival list with integer modulo. `page` increments on `pageTicker`.

**Display sleep/wake** — When `start_time`/`stop_time` are set, the display sleeps outside active hours. The schedule ticker fires every minute to check `isActiveTime`. Overnight ranges (e.g. 22:00–06:00) are supported.

**Non-obvious code must have comments** — whenever a piece of code does something that isn't immediately clear from reading it (e.g. the 12-hour overnight rule, BOM stripping in CSV headers, backoff logic), add an inline comment explaining _why_, not just _what_. Also update this file to document any new patterns introduced.

---

## Dev Commands

```sh
make build        # build binary for the current host → build/
make build-pi     # cross-compile ARM64 Linux binary for Pi Zero 2W
make test         # go test ./...
make run-mock     # run locally with mock display (PNG output → mock_output/)
make deploy       # build-pi + scp binary + service to Pi, enable & start
make preview      # render one preview PNG from fixture data (no API key needed)
```

Update `PI_HOST` in `Makefile` before deploying.

---

## Configuration

```sh
cp config.yaml.example config.yaml
# Edit api_key and stops, at minimum
```

Key fields:

| Field                          | Default       | Notes                                               |
| ------------------------------ | ------------- | --------------------------------------------------- |
| `api_key`                      | —             | Required. Register at nationaltransport.ie          |
| `stops`                        | —             | Required. List of `stop_number` + `label`           |
| `routes`                       | (all)         | Optional whitelist of route short names             |
| `poll_interval_seconds`        | 60            | How often to fetch live data                        |
| `page_interval_seconds`        | 5             | How often to advance the arrival page               |
| `max_pages`                    | 0 (unlimited) | Cap on page cycling per section                     |
| `max_minutes`                  | 90            | Lookahead window for arrivals                       |
| `display_model`                | `lcd`         | Display type                                        |
| `framebuffer_device`           | `/dev/fb0`    | Path to framebuffer                                 |
| `start_time` / `stop_time`     | (unset)       | HH:MM wake/sleep schedule; set both or neither      |

---

## Current State

The application is fully functional. It fetches GTFS static data from TFI (cached as gob), polls GTFS-RT TripUpdates on a configurable interval, merges static and realtime arrivals per stop, and renders to the 7" 1024×600 LCD via `/dev/fb0`. Paging cycles through arrivals automatically. A wake/sleep schedule limits backlight hours overnight.

Supported display paths: HD LCD (1024×600, the primary target) and small e-ink (< 800 px wide, legacy).

---

## Workflow

Every feature session follows three phases in order.

### Phase A — Planning

1. Enter plan mode and write a plan file (e.g. `planning/my-feature/spec.md`).
2. Do not begin implementation until the plan has been reviewed and approved.

### Phase B — Implementation

1. **Before writing any code**, create a feature branch from main — no exceptions: `git checkout main && git pull && git checkout -b <branch>`.
2. Write tests alongside each meaningful code change.
3. After each implementation unit, run `make test`. **Do not proceed until all tests pass.**
4. For display changes, run `make preview` to verify rendered output visually.
5. Before moving to Phase C, run `make build-pi` to verify the ARM64 cross-compile succeeds. Fix any errors before proceeding.
6. Update this file to reflect any new config fields, packages, display paths, or architectural patterns introduced during implementation.

### Phase C — Commit + PR

**Execute Phase C automatically as soon as all tests pass — do not wait for user confirmation.**

1. Commit scoped files by name — never use `git add .` or `git add -A`.
2. Create a PR targeting `main`: `gh pr create --base main` with a concise title and a summary body. PRs always target `main` unless explicitly instructed otherwise.
3. **Never commit or push directly to `main` under any circumstances** — no exceptions for docs, config, or any other content. Every change goes through a branch and PR.
