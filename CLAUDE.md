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

**`config/`** — YAML config loading with defaults and validation. `Config` is the single source of truth for all runtime settings. `config.yaml` is gitignored; `config.yaml.example` is the reference. `LoadWithSecrets(configPath, secretsPath)` is the production entry point: it reads `api_key` from `secrets.yaml` first, then `TFI_API_KEY` env var, then falls back to `api_key` in `config.yaml`. `Load(path)` is preserved for tests that supply a complete single-file config.

**`updater/`** — Low-level install logic (used by the agent). `Run(Config)` finds a staged `tfi-display` binary in `StagingDir`, backs up the existing install to `<target>.prev`, atomically installs the new binary, restarts the systemd service, and rolls back on failure. `ApplyConfig(content, configPath, service, timeout)` does the same lifecycle for a config file: backup to `<path>.prev`, atomic write, restart, verify, rollback. `DefaultConfig()` uses `os.Executable()` to set `StagingDir` to the running binary's directory. This package never makes network calls.

**`agent/`** — The `tfi-agent` foundation layer. `Run(ctx)` loops forever: each cycle it reloads settings, then independently (a) checks `GET /api/tfi/v1/latest` and, if the version *differs* from the local marker and isn't known-bad, downloads `download_url` and calls `updater.Run`; (b) fetches `GET /api/tfi/v1/config_files/fetch` (Bearer device token) and, if different from the on-disk config, calls `updater.ApplyConfig`. Failed binary installs are recorded locally and reported via `POST /api/tfi/v1/releases/report`. Deliberately decoupled from `config.LoadWithSecrets` — it reads only the fields it needs, leniently, so it keeps running even when the display config is broken.

**`cmd/agent/`** — Entry point for the `tfi-agent` binary. Parses flags, builds the agent, and runs it under a SIGTERM-aware context.

**`gtfs/`** — All GTFS logic:
- `static.go` — downloads the TFI GTFS ZIP, parses it into a `StaticDB` (stops, trips, services, calendar exceptions), and caches it as a gob file. Cache is invalidated by the upstream `Last-Modified` header or a schema version bump.
- `realtime.go` — polls the GTFS-RT TripUpdates endpoint, parses protobuf into a `LiveStore`. Handles delays (absolute timestamps or delta seconds), cancellations, and added trips. Includes exponential backoff on rate-limit errors.
- `arrivals.go` — `QueryArrivals` joins static and live data to produce a time-sorted `[]Arrival` for a stop. Applies calendar validity, cancellation checks, route filtering, a per-stop walking-time cutoff (`minMinutes`), and deduplication.

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

**Walking-time cutoff** — each stop may set `walking_minutes`. `QueryArrivals` drops any arrival whose *effective* time (realtime if present, else scheduled) falls before `now + walking_minutes`, since you couldn't reach the stop in time. The cutoff is strictly-under: a bus arriving exactly at `now + walking_minutes` is still shown. `0` (or omitted, or negative — clamped to 0 at load) disables it.

**Realtime overlay** — `LiveStore` is a concurrent-safe in-memory store updated by the poller goroutine. `QueryArrivals` reads from it under `RLock`. Delays are applied as an absolute Unix timestamp (preferred) or a delta-seconds offset.

**Paging** — `renderAndDisplay` uses `RowsPerSection` to determine how many rows fit per section at the current resolution, then windows into the arrival list with integer modulo. `page` increments on `pageTicker`.

**Display sleep/wake** — When `start_time`/`stop_time` are set, the display sleeps outside active hours. The schedule ticker fires every minute to check `isActiveTime`. Overnight ranges (e.g. 22:00–06:00) are supported.

**Secrets/config split** — `api_key` and the agent's `device_token` are kept in `/etc/tfi-display/secrets.yaml` (mode 600, root-only), separate from the main `config.yaml` so config can be distributed remotely without exposing secrets. `tfi-display` merges both via `LoadWithSecrets`; `tfi-agent` reads only `device_token`. The agent never *writes* `secrets.yaml`.

**Atomic binary install** — `updater.installBinary` writes to `<target>.new` first, sets mode 0755, then `os.Rename` into place. On Linux, rename within the same filesystem is atomic, so the running binary is never partially overwritten. The previous binary is preserved at `<target>.prev` for one-step rollback. `updater.writeFileAtomic` / `ApplyConfig` do the same for config files.

**Agent update model** — the agent compares `/latest`'s version to a marker file (`/usr/local/bin/tfi-display.version`, written after a successful install) and installs whenever it *differs* — not just when newer — so a central rollback (re-pointing the release) propagates to devices. A version that fails to install is appended to `/var/lib/tfi-agent/bad-versions` (persisted across restarts) so it isn't retried every cycle, and is reported to the API. The poll interval comes from `update_interval_seconds` in `config.yaml` (default hourly, used whenever the file is missing/malformed), re-read each cycle, so a fetched config can change the cadence without an SSH visit. Binary updates need no secrets (`/latest` is public); only config sync and failure reporting use `device_token`.

The API origin (`base_url`) and `device_token` live in `secrets.yaml`, not `config.yaml` — the agent overwrites `config.yaml` on each sync but never touches `secrets.yaml`, so the bootstrap "where is home" survives. `secrets.yaml` is gitignored, keeping the real URL out of this public repo. If `base_url` is unset the agent logs and skips.

Downloaded binaries are staged to a **dedicated directory** (`/var/lib/tfi-agent/staging`), never the directory the agent runs from. The agent and `tfi-display` are both installed in `/usr/local/bin`, so `updater.DefaultConfig`'s exe-dir staging default would write the download straight onto the running `/usr/local/bin/tfi-display` — which the kernel rejects with `ETXTBSY` ("text file busy"), stranding the device on its old binary every cycle. `agent.New` overrides `StagingDir` to the dedicated path; `updater.Run` then installs from there via the atomic `.new` + rename path, which *is* permitted over a running executable.

**Non-obvious code must have comments** — whenever a piece of code does something that isn't immediately clear from reading it (e.g. the 12-hour overnight rule, BOM stripping in CSV headers, backoff logic), add an inline comment explaining _why_, not just _what_. Also update this file to document any new patterns introduced.

---

## Dev Commands

```sh
make build              # build tfi-display binary for the current host → build/
make build-pi           # cross-compile ARM64 Linux tfi-display for Pi Zero 2W
make build-agent-pi     # cross-compile ARM64 Linux tfi-agent for Pi Zero 2W
make test               # go test ./...
make run-mock           # run locally with mock display (PNG output → mock_output/)
make deploy             # build-pi + scp tfi-display + service to Pi, enable & start
make deploy-agent       # install tfi-display + tfi-agent and start the agent service
make preview            # render one preview PNG from fixture data (no API key needed)
```

Update `PI_HOST` in `Makefile` before deploying.

---

## Configuration

```sh
# On Pi (one-time setup):
sudo cp secrets.yaml.example /etc/tfi-display/secrets.yaml
# Edit /etc/tfi-display/secrets.yaml — set api_key and (for the optional
# tfi-agent) base_url + device_token (base_url = your update server's origin;
# device_token = the per-device key it issued). The agent is skipped if unset.
sudo chown root:root /etc/tfi-display/secrets.yaml && sudo chmod 600 /etc/tfi-display/secrets.yaml

sudo cp config.yaml.example /etc/tfi-display/config.yaml
# Edit /etc/tfi-display/config.yaml — set stops, routes, etc.
```

**Secrets vs config split**: `api_key` and `device_token` live in `/etc/tfi-display/secrets.yaml` (root-only, never written by the agent). All other settings live in `config.yaml`. `tfi-display` is started with both `-config` and `-secrets` flags (see `tfi-display.service`); `tfi-agent` takes the same flags (see `tfi-agent.service`).

Key fields:

| Field                          | Default       | Notes                                               |
| ------------------------------ | ------------- | --------------------------------------------------- |
| `api_key`                      | —             | **In `secrets.yaml` only.** Register at nationaltransport.ie |
| `stops`                        | —             | Required. List of `stop_number` + `label` (+ optional `walking_minutes`) |
| `stops[].walking_minutes`      | 0 (off)       | Hide arrivals sooner than this many minutes (walk time) |
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
