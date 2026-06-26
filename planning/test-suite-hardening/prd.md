# PRD — Test Suite Hardening for Autonomous Performance Iteration

**Status:** Draft for review (Phase A)
**Author:** Daniel (with Claude)
**Repo state at writing:** `main` @ `72f5c96` (post-pull; includes the disruption fix, background static refresh, and board versioning)

---

## 1. Summary

Build out the test suite so that an automated agent can iterate on the codebase to
improve **performance** safely and measurably. Today the suite proves a handful of
correctness facts but provides **no way to (a) detect that a refactor changed
observable behaviour, or (b) measure whether a change made anything faster.** Those
two capabilities — a behaviour lock and a performance fitness function — are the
prerequisites for trustworthy autonomous optimisation. This PRD defines what the
hardened suite must guarantee, not the line-by-line implementation.

## 2. Background & problem

`tfi-display` turns two upstream feeds (a GTFS static ZIP, a GTFS-realtime
protobuf) into a sorted list of arrivals and renders them to a framebuffer. The
hot paths are `gtfs.QueryArrivals` (per stop, per refresh), `Poller.parse` (every
poll), `gtfs.buildFromZIPFile` (startup + weekly refresh), and the HD renderer
(every frame).

Current automated coverage (`go test -cover ./...`, post-pull):

| Package | Coverage | Notes |
| --- | --- | --- |
| `gtfs` | **41.4%** | hot path `QueryArrivals` 91.5%, but the entire static parse/cache/refresh pipeline is ~0% |
| `display` | 48.4% | HD path 88.8%; small-display `Render` 3.3%; `RowsPerSection` 0% |
| `config` | 80.6% | solid |
| `updater` | 64.6% | happy path good; rollback paths thin |
| `agent` | 71.9% | operations good; `Run`/`cycle` loop 0% |
| `main` / `cmd/agent` / `display/driver` / `fonts` | 0% | orchestration, glue, hardware |
| **overall** | **44.2%** | |

Concrete blockers to autonomous performance work:

1. **No benchmarks anywhere.** "Improve performance" has no target to optimise
   against and no regression signal.
2. **No end-to-end behaviour lock.** There is no "fixed input → exact expected
   arrivals" test, so an internal refactor can silently change output. The one test
   that aimed at this (`gtfs/realtime_test.go` reading `../../test_data/test_live_response.gtfsr`)
   **silently `t.Skip`s** because the fixture was never committed.
3. **The static pipeline is untested.** `buildFromZIPFile`, `parseCSV`,
   `LoadOrBuild` (cache validation/invalidation), `MaybeRebuild` (the new
   background refresh), and the gob round-trip are all ~0%.
4. **The newly-expanded concurrency surface is unverified.** The static dataset is
   now hot-swapped at runtime via `gtfs.DB` (`atomic.Pointer[StaticDB]`); the
   poller, render loop, and background refresher all touch shared state, plus
   `LiveStore` (RWMutex) and the `DiagLogOnce` dedupe. No test runs these
   concurrently under `-race`.
5. **Wall-clock coupling blocks deterministic tests.** `Poll`, `IsCancelled`, the
   24h cancellation carry-over in `parse`, and `MaybeRebuild` call `time.Now()` /
   `time.Since` directly, so their time-dependent branches can't be tested
   reproducibly.

## 3. Goals

- **G1 — Behaviour lock.** A golden end-to-end test pins the full pipeline output;
  any change to observable arrivals fails it. *This is what makes autonomous
  refactoring safe.*
- **G2 — Performance fitness function.** Benchmarks with `-benchmem` baselines on
  every hot path, so a change's speed/allocation impact is measurable.
- **G3 — Edge-case correctness.** The enumerated edge cases (Section 6) each have
  an assertion.
- **G4 — Robustness.** Fuzzing of all upstream-data parsers (no panic on malformed
  ZIP / CSV / protobuf); the concurrency surface verified under `-race`.
- **G5 — CI enforcement.** CI runs tests under `-race` and runs benchmarks for
  visibility.

## 4. Non-goals

- **Not** optimising the production code in this effort — this PRD builds the
  harness that a later effort (human or agent) optimises against.
- **Not** changing any observable behaviour. All testability refactors must be
  behaviour-preserving, guarded by the G1 golden test.
- **Not** testing the real hardware driver (`display/driver/lcd_dpi.go`) in CI — it
  is build-tagged Linux/periph.io and is covered only via the mock driver.
- **Not** a coverage-number chase for its own sake; coverage targets (Section 7)
  are a proxy, the edge-case checklist is the real bar.

## 5. Decisions & constraints (resolved)

| Decision | Choice | Implication |
| --- | --- | --- |
| Fixture data | **Real, trimmed TFI samples** | Static ZIP is downloadable without a key and will be trimmed to the configured stops; the realtime `.gtfsr` snapshot is captured with the existing local `secrets.yaml` key. Fixtures committed under Go's `testdata/` convention. |
| Refactor latitude | **Free to introduce seams/interfaces** | A clock seam and pure-function extraction are in scope where they unlock determinism/testability, provided behaviour is preserved. |
| CI | **Add `-race` + run benchmarks** | `.github/workflows/ci.yml` gains a race-enabled test run and an informational benchmark run (printed, no hard timing gate — bench numbers are noisy on shared runners). |

## 6. Requirements

### Functional

**FR1 — Golden end-to-end pipeline (keystone).**
Given committed real fixtures (trimmed GTFS static ZIP + captured `.gtfsr`), a test
must run `ZIP → StaticDB → Poller.parse(rt) → QueryArrivals(now = fixed)` and assert
the **exact, ordered `[]Arrival`** (route, headsign, platform, scheduled/realtime
time, delay, added flag).
*Acceptance:* the test fails if any field of any arrival changes. Fixtures live under
`gtfs/testdata/`; the currently-skipped live-response test is relocated there and
runs (no `t.Skip`).

**FR2 — Static pipeline coverage.** `buildFromZIPFile` and helpers must be tested
for: UTF-8 BOM header stripping; `stop_code` fallback to `stop_id` when empty/`"0"`;
`platform_code` present/absent; `StopIDToNumber` population where `stop_id ≠
stop_code` (rail); trip filtering to only trips serving configured stops; tolerated
missing `calendar.txt`; skipped malformed `stop_times` rows; overnight hour
bucketing (`arrivalSecs/3600 % 24`). `LoadOrBuild`/`MaybeRebuild` must be tested for:
gob save/load round-trip; cache reuse when fresh; rebuild on schema-version bump,
on `FilterStops` change, and on a newer `Last-Modified` (HEAD via `httptest`).
*Acceptance:* `gtfs/static.go` ≥ 90%; each listed case has an assertion.

**FR3 — Arrivals hot-path edge cases.** Beyond what the disruption fix already
covers (severe-delay rescue, delay-beyond-lookback drop, unresolved-route bypass),
add: overnight 12-hour rollover; hour-bucket lookback boundary (a trip scheduled
exactly `lookbackHours` ago); de-duplication of a trip appearing in multiple
buckets; the absolute-timestamp (`AbsTime`) delay branch; the walking-time filter
applied to realtime additions; the dual-condition "already departed" guard when
realtime and scheduled disagree about past/future; platform propagation;
`MinutesUntil`.
*Acceptance:* each case asserted; `QueryArrivals` ≥ 95%.

**FR4 — Realtime parse/poll/backoff.** `parse` must cover: 24h cancellation
carry-over across polls; implausible-delay discard (`> maxDelaySeconds`);
skipped-stop exclusion; unknown-trip counting; `AbsTime` vs delta-`Delay`;
added-trip arrival-vs-departure-time fallback and same-route dedupe; nil header /
empty feed. `Poll`/`fetch` must cover (via `httptest`): 429 → `rateLimitCount++`,
200 → reset, other non-200 → error, transport error. `BackoffDuration` must cover
exponential growth and the 3600s cap. `resolveStopID` must cover the rail
`StopIDToNumber` path.
*Acceptance:* `realtime.go` ≥ 90%; `BackoffDuration`, `Poll`, `fetch` no longer 0%.

**FR5 — Concurrency verification.** A test must exercise, concurrently and under
`-race`: `Poller.parse` (writes `LiveStore` + resets `diagLogged`),
`QueryArrivals` (reads `LiveStore`, takes the `DiagLogOnce` write lock), and
`DB.Store`/`DB.Load` (atomic swap of `StaticDB`).
*Acceptance:* `go test -race ./...` passes with the new test present.

**FR6 — Surrounding packages.**
- *config:* defaults application; schedule validation (both-or-neither, invalid
  `HH:MM`); negative `walking_minutes` clamp; empty stops; `LoadWithSecrets`
  priority (secrets file > env > config) and malformed/missing secrets handling;
  the new static-refresh config field.
- *updater:* rollback paths via the injectable `systemctlCmd` fake (restart fails →
  rollback; `waitForActive` timeout → rollback; rollback fails with no `.prev`);
  `installBinary` failure; full `ApplyConfig` lifecycle; `DefaultConfig`.
- *agent:* `cycle`/`Run`/`reloadSettings` via `httptest` + the existing hooks
  (version-differs→install, same→skip, bad→skip, download-fail→`update_error`
  without blacklist, install-fail→`markBad`+`release_failure`); `doWithRetry`
  (5xx-retry-then-success, 4xx-no-retry, context-cancel); `postReport` event names
  and auth header; state-file round-trips; loop exits on context cancel.
- *main:* extract and table-test the paging-window math from `renderAndDisplay`;
  `isActiveTime` (normal, overnight, degenerate `start==stop`, boundaries).
*Acceptance:* config ≥ 90%, updater ≥ 85%, agent ≥ 90%; `isActiveTime` and the
paging math covered.

**FR7 — Display golden/snapshot tests.** `RowsPerSection` table-tested. Rendering
verified by golden snapshot (assert image dimensions + a content hash or sampled
pixels, regenerable via a flag): small-display `Render` (`padRoute`, `truncate`/`…`,
"No departures", delay `+/-`, platform `P` prefix, 2.13" vs 2.9" zones) and HD
`renderHD` (per-section "No departures", DART route-box skip, "(Sched)" badge,
"Due"/"99 min", multi-section layout, version label). `TestRenderPreview` upgraded
to assert rather than only writing a PNG.
*Acceptance:* `display` ≥ 80%; snapshots fail on visual regressions.

**FR8 — Fuzzing.** Native Go fuzz targets that must not panic on arbitrary input:
`parseGTFSTime`; `buildFromZIPFile` (malformed ZIP/CSV); `Poller.parse` (malformed
protobuf). Seed corpora committed.
*Acceptance:* seed corpus passes in CI; a short `-fuzz` smoke run finds no crash.

### Non-functional

- **NFR1 — Determinism.** No test depends on wall-clock time. Time-dependent
  production code accepts an injectable clock (`now func() time.Time` or a small
  `Clock` interface), defaulting to real time in production.
- **NFR2 — Speed.** The non-bench unit suite stays fast (target: well under ~10s
  without `-race`) so agent iterations are cheap. Benchmarks and fuzz runs are
  separate invocations.
- **NFR3 — Hermetic.** No test makes real network calls (use `httptest` + fixtures)
  or touches `systemctl` / real install paths (use the fakes/hooks and `t.TempDir`).
- **NFR4 — CI.** `ci.yml` runs `go test -race ./...` and a separate
  `go test -bench . -benchmem -run '^$' ./...` step (informational).

## 7. Success metrics

- **Behaviour lock exists:** the FR1 golden test is present and demonstrably fails
  on an intentional output change.
- **Fitness function exists:** benchmark baselines recorded for `QueryArrivals`,
  `parse`, `buildFromZIPFile`, `renderHD`, `GetDelay`, each with `ReportAllocs`.
- **Coverage:** `gtfs` ≥ 90%, overall ≥ 80% (proxy; the Section 6 checklist is the
  real bar).
- **`go test -race ./...` is green** with the concurrency test present.
- **Robustness:** all three fuzz targets run clean on their seed corpora.
- **The end-state proof:** an agent can make a behaviour-preserving change to a hot
  path and the suite (a) stays green on the golden test and (b) shows a benchmark
  delta — i.e. the harness can both *protect* and *measure*.

## 8. Dependencies / open items

- **Realtime fixture capture.** The static ZIP is downloadable keylessly and will be
  trimmed in-repo. The `.gtfsr` snapshot needs one capture against the live API
  using the existing local `secrets.yaml` key — to be done at Phase 1 start
  (preference: capture it via a documented one-liner so the key never leaves your
  machine; confirm whether you'll run it or want me to, given the key is local).
- **Fixture location.** Standardise on `gtfs/testdata/` (Go excludes `testdata/`
  from builds) and fix the stale `../../test_data/...` path in the existing test.
- **Coverage targets** in Section 7 are proposed; adjust at review if you'd prefer
  different thresholds.

## 9. Delivery sequence (milestones, not prescriptive)

1. **Keystone + seams** — fixtures, FR1 golden test, clock seam, pure-function
   extraction. (Unlocks the most safety per line.)
2. **Core `gtfs` edges** — FR2, FR3, FR4, plus FR5 race test.
3. **Surrounding packages** — FR6 (config, updater, agent, main) and FR7 (display).
4. **Performance & robustness** — FR8 fuzz, benchmarks, and the FR-NFR4 CI wiring.

Each milestone lands behind-a-branch with `make test` green, per the repo workflow
(Phase B), and ships as its own PR to `main`.
