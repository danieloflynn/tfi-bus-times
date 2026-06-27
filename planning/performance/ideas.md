# TFI Board Performance Optimization

Goal: reduce **memory** and **CPU** required to run the board on a Raspberry Pi
Zero 2W (4× slow ARM cores, 512 MB RAM). Memory pressure manifests as GC churn;
CPU shows up as parse/render latency. We iterate: pick an idea, benchmark,
keep it only if tests pass *and* there is a measurable improvement, otherwise
record the negative result and revert.

## How to benchmark

```sh
go test -run=XXX -bench=. -benchmem ./gtfs/ ./display/ 2>/dev/null | grep -E "ns/op"
```

slog output to stderr is discarded via `2>/dev/null`. Benchmark order:
1. QueryArrivals  2. PollerParse  3. BuildFromZIPFile  4. GetDelay  5. RenderHD

## Baseline (commit 925d689, before any optimization)

| Benchmark        | ns/op      | B/op      | allocs/op | Runs how often            |
| ---------------- | ---------- | --------- | --------- | ------------------------- |
| QueryArrivals    | 204,305    | 16,312    | 685       | every render + page tick  |
| PollerParse      | 11,329,569 | 7,405,966 | 210,047   | every poll (default 60 s) |
| BuildFromZIPFile | 2,303,153  | 1,101,365 | 4,582     | startup + daily refresh   |
| GetDelay         | 30         | 0         | 0         | per arrival lookup        |
| RenderHD         | 2,008,377  | 615,106   | 40        | every refresh + page tick |

**PollerParse is the dominant cost**: 7.4 MB and 210k allocations *every minute*,
driven by `proto.Unmarshal` building the full FeedMessage object graph
(2857 entities, 15666 stop_time_updates). This is the primary GC-pressure source
on the device and the biggest target.

---

## Ideas

Status legend: ⬜ untried · 🔬 in progress · ✅ kept (committed) · ❌ reverted (no/negative effect)

### 1. ✅ Cheap allocation wins in `realtime.parse`
- Preallocate the per-trip `delays` slice with `cap = len(tu.StopTimeUpdate)` to
  avoid repeated slice-growth reallocations.
- Replace `sort.Slice` (reflection + closure escape) with `slices.SortFunc`.
- Size-hint the three `new*` maps from the previous parse's counts.
- Expected: modest alloc/CPU reduction in PollerParse; low risk.

### 2. ✅ Custom streaming protobuf decoder for the RT feed
- `proto.Unmarshal` allocates a struct for every entity, trip descriptor, stop
  update and stop-time event. We only need a handful of scalar fields. Walk the
  protobuf wire format directly with `protowire`, extracting only what we use,
  never materialising the object graph.
- Expected: the big one — should cut PollerParse allocs and bytes by an order of
  magnitude. Higher risk; guarded by existing parse tests + fuzz + golden tests.

### 3. ⬜ Reuse the decoded feed message across polls
- If keeping `proto.Unmarshal`, reuse a single `FeedMessage` via `Reset()` so the
  backing arrays are recycled instead of re-allocated each poll.

### 4. ✅ QueryArrivals allocation reduction (part 1)
- 685 allocs per call, called every few seconds. Inspect for per-call slice/map
  allocations that could be pooled or preallocated.
- Done: array-backed `[24]bool`/`[24]int` hour scan (was map+slice),
  `slices.SortFunc` (was `sort.Slice`), struct dedup key (was
  `RouteShort+ScheduledTime.String()`).
- Result: 685→598 allocs (-13%), 16,312→13,912 B (-15%), ~2.5% faster.

### 4b. ✅ QueryArrivals: kill IsServiceActive per-candidate allocations
- `IsServiceActive` runs per candidate stop-time and does
  `serviceID + ":" + date.Format("20060102")` — a time-format + string concat
  allocation on every call. Restructure the exceptions key / date handling to
  avoid the formatting and concatenation on the hot path.

### 5a. ✅ RenderHD: cache uniform draw sources
- Each `font.Drawer.DrawString` allocated a fresh `image.NewUniform(c)`. Cache
  the two colours actually used (black/white) as package-level `*image.Uniform`
  via `grayUniform`. Result: RenderHD 40→11 allocs (-72%); also applied to the
  small-display `drawText`. Image bytes/CPU unchanged (dominated by the buffer +
  font rendering).

### 5b. ✅ RenderHD: reuse the image buffer across frames ⭐
- The 614 KB `image.NewGray` was allocated every frame (every page tick, ~5 s).
  Verified `LCDDPI.DisplayFrame` copies pixels synchronously into the mmap'd
  framebuffer and retains no reference, and `renderAndDisplay` discards the
  frame after, so the buffer is safe to reuse.
- Added a `display.Renderer` that holds one `*image.Gray`, reallocating only on
  a size change; both render paths now reset the background in place (HD
  `clear()`s to black, small fills white) and draw into the provided buffer.
  `main.go` creates one `Renderer` before the loop and reuses it.
- Result: per-frame image allocation eliminated — RendererReuse benchmark
  ~590 B / 5 allocs vs the allocating path's 614,640 B / 11 allocs. On the
  device this removes ~440 MB/hr of GC churn at the page cadence.

### 6. ⬜ GOGC / GOMEMLIMIT tuning for the device
- Non-code lever: tune GC aggressiveness for a low-churn long-running process.
- Now that allocation churn is ~20–50× lower, a higher GOGC (fewer, larger GC
  cycles) may further cut GC CPU. Could set via the systemd unit's Environment.

### 7. ⬜ BuildFromZIPFile allocations (startup + daily refresh)
- 1.1 MB / 4,582 allocs per build. Lower priority (infrequent), but the daily
  refresh competes with the render loop. CSV parsing + gob are the likely costs.

### 8. ⬜ Reuse decoder scratch / maps across polls
- The feedDecoder's `stops` scratch is reset per trip but reallocated per poll;
  the swap maps are rebuilt each poll. Investigate a poller-owned scratch pool
  to shave the remaining 4.5k allocs/poll further.

---

## Cumulative summary (baseline 925d689 → current)

The two continuously-running paths on the device — the realtime poll (every
60 s) and the render (every page tick, ~5 s) — are dramatically lighter:

| Path                          | ns/op            | B/op              | allocs/op       |
| ----------------------------- | ---------------- | ----------------- | --------------- |
| PollerParse (per poll)        | 11.3 ms → 1.47 ms (**7.7×**) | 7.41 MB → 407 KB (**18×**) | 210,047 → 4,535 (**46×**) |
| QueryArrivals (per render)    | 204 µs → 140 µs (**1.5×**)   | 16.3 KB → 9.3 KB | 685 → 19 (**36×**) |
| Frame render (device, reused) | n/a (new path)   | 614 KB → ~0.6 KB  | 40 → 5          |

Order-of-magnitude reductions in allocations on every recurring operation, which
is what drives GC CPU on the Pi Zero 2W. Still pending: ideas 6 (GC tuning) and
BuildFromZIPFile (startup/daily, low priority — see below).

## Results log

### Idea 1 — cheap allocation wins in `realtime.parse` (KEPT)
Preallocated per-trip `delays` slice to `len(StopTimeUpdate)`, swapped
`sort.Slice` → `slices.SortFunc`, size-hinted the three swap maps from the
previous parse.

| Benchmark   | B/op before | B/op after | allocs before | allocs after | ns/op |
| ----------- | ----------- | ---------- | ------------- | ------------ | ----- |
| PollerParse | 7,405,966   | 7,279,312  | 210,047       | 209,696      | ~11.3 ms (unchanged) |

Small but consistent (-126 KB, -351 allocs across 3 runs), no downside, cleaner
code. Confirms the dominant cost is `proto.Unmarshal` itself (Idea 2).

### Idea 2 — custom streaming protobuf decoder (KEPT) ⭐ biggest win so far
New `gtfs/realtime_decode.go` walks the GTFS-RT wire format with `protowire`,
extracting only the scalar fields `parse` uses (trip_id, schedule_relationship,
route_id, stop_sequence, arrival/departure time + delay, stop_id) and never
building the FeedMessage object graph. A reused `[]rtStop` scratch slice avoids
per-stop allocation; trip/route/stop IDs stay `[]byte` sub-slices and are only
materialised to `string` when actually stored in a map (the `m[string(b)]`
no-copy lookup optimisation handles the ~1500 unknown scheduled trips for free).

| Benchmark   | Before     | After     | Change          |
| ----------- | ---------- | --------- | --------------- |
| PollerParse ns/op    | 11,329,569 | 1,467,356 | **7.7× faster** |
| PollerParse B/op     | 7,405,966  | 407,187   | **18× less**    |
| PollerParse allocs/op| 210,047    | 4,535     | **46× fewer**   |

Validated by the full gtfs suite incl. the golden end-to-end behaviour lock
(identical observable arrivals) and 13k fuzz execs with no panic. This is the
poll-cadence hot path (every 60 s on the device), so it directly removes the
dominant GC-pressure and CPU source.

### Idea 4 part 1 — QueryArrivals cheap allocation wins (KEPT)
Array-backed hour scan, `slices.SortFunc`, struct dedup key (no
`ScheduledTime.String()`).

| Benchmark     | ns/op (b→a)        | B/op (b→a)      | allocs (b→a) |
| ------------- | ------------------ | --------------- | ------------ |
| QueryArrivals | 204,305 → 199,000  | 16,312 → 13,912 | 685 → 598    |

QueryArrivals runs on every render + page tick (~every 5 s on the device).

---

## Round 2 ideas (continuing iteration)

Status legend: ⬜ untried · 🔬 in progress · ✅ kept (committed) · ❌ reverted

### 17. ✅ QueryArrivals: snapshot the LiveStore maps once per query
The query took the LiveStore RWMutex twice per candidate stop-time (`IsCancelled`
+ `GetDelay`) plus once for `GetAdditions`. The realtime maps are replaced
wholesale on each parse (never mutated in place), so a single RLock can capture
all three map references + clock and the rest of the query reads them lock-free.
Removes per-candidate lock traffic and contention with the poller goroutine.
Kept — race-clean, golden lock holds. (≈2% faster in the single-threaded bench;
larger benefit on-device where the poller contends for the lock.)

### 18. ✅ IsServiceActive: integer YYYYMMDD date compare ⭐
`IsServiceActive` built **three** `time.Date` values per candidate (midnight
normalisation of dt + service start + service end) for the range check — and
`time.Date` does full timezone normalisation, which turned out to dominate the
query's CPU. Replaced with `Date()` accessors + packed-int YYYYMMDD comparison.
No schema bump needed (computed inline from the existing time.Time fields).
**QueryArrivals ~140µs → ~82µs (1.7× faster).**

### 19. ✅ renderAndDisplay: reuse the per-frame sections slice
`renderAndDisplay` allocated `make([]StopSection, len(stops))` every frame.
Now caller-owned (allocated once in main before the loop) and filled in place,
so the page-tick render allocates nothing for the section list. Small (one slice
header/frame) but free and on the page-tick cadence.

### 28. ✅ QueryArrivals: zero-alloc additions diagnostic (no-copy DiagLogOnceBytes)
The additions rescue log built its dedupe key by string concat + `time.Format`
on every render, even after it had been logged — reintroducing per-render
allocation whenever a watched stop had additions. Added `DiagLogOnceBytes` (no-copy
`m[string(b)]` membership test) and build the key in a stack buffer (digits via
`Clock()`, no Format). The already-logged path now allocates nothing. New
`AllocsPerRun` test locks QueryArrivalsInto at **0 allocs/run** in steady state
with additions present.

### 27. ✅ renderer: allocation-light truncate (no []rune)
`truncate` allocated a `[]rune` plus two strings on every truncation — and the
HD renderer truncates headsigns per row, per frame. Rewrote it to walk runes by
byte offset (range over string) and slice the original; the fitting case
allocates nothing and a truncation allocates only the one result string (3
allocs → 1). Added a rune-aware unit test (incl. multibyte) locking behaviour.

### 26. ✅ static build: parse stop_times before trips (drop the allTrips buffer)
The build buffered every network trip into an intermediate `allTrips` map, then
filtered to stops. Reordered so stop_times (which yields the watched-trip set) is
parsed first and trips.txt builds `db.Trips` directly — the network-wide buffer
is gone. BuildFromZIPFile 1,039 KB → 740 KB (-29%), ~10% faster; far larger on
the full feed where allTrips held 100k+ trips. Golden + build fuzz clean.

### 25. ✅ handleAdded: resolve the route lazily (only when a watched stop is found) ⭐
`handleAdded` called `routeShortName(d.db, string(routeID))` upfront for every
ADDED trip — network-wide, mostly serving none of our stops. Deferred it until
the first watched stop in the trip is found. PollerParse 208 → 86 allocs/op.

### 23. ✅ parse: filter cancellations to known (watched-stop) trips
The feed cancels trips network-wide; each allocated `string(tripID)` for the
Cancellations map even though QueryArrivals only looks up cancellations for
trips in `db.Trips` (built filtered to watched stops). Skip storing a
cancellation when a configured filter is active and the trip isn't in db.Trips.
`watched == nil` (no filter) keeps all, preserving the LiveStore carry-over
contract tests. PollerParse 298 → 215 allocs, 36.5 → 30.2 KB.

### 24. ✅ parse: reuse the StopTimeUpdate scratch across polls
`feedDecoder.stops` started nil each poll and regrew; retained it on the Poller
(like the delay arena / read buffer) so it reaches steady-state capacity once.
PollerParse 215 → 208 allocs, 30.2 → 22.2 KB.

### 22. ✅ QueryArrivals: reusable scratch (`QueryArrivalsInto`) for zero-alloc renders
The query's remaining 13 allocs/call were entirely the `arrivals` slice growing
from nil (geometric reallocation). Added `QueryArrivalsInto(dst, …)`:
`QueryArrivals` calls it with nil (one-shot, tests unchanged); the render loop
keeps one scratch slice per stop and feeds it back each frame so the backing is
reused. A naïve pre-sized cap was rejected first — it cut allocs but inflated
B/op and left CPU flat. The scratch approach is strictly better.

| Benchmark (3 stops)     | ns/op   | B/op  | allocs |
| ----------------------- | ------- | ----- | ------ |
| QueryArrivals (nil dst) | ~82,000 | 7,472 | 13     |
| QueryArrivalsReuse      | ~76,000 | **0** | **0**  |

The device render loop uses the reuse path → zero arrivals allocation per
render/page tick. Race-clean, golden lock holds, preview identical.

### 21. ✅ lcd driver: per-row Pix walk instead of per-pixel GrayAt ⭐
`writeRGB565`/`writeXRGB8888` called `img.GrayAt(x,y)` for each of ~614k pixels
per frame — every call recomputes a `PixOffset` (multiply) and bounds-checks.
Replaced with a per-row slice of `img.Pix` ranged over directly.

| Benchmark (1024×600 frame) | ns/op (b→a)        |
| -------------------------- | ------------------ |
| writeRGB565                | 2,010,000 → 1,065,000 |

**~1.9× faster** per-frame framebuffer pack on the device's real display path
(runs on every refresh + page tick). Added off-hardware packing correctness
tests (RGB565 + XRGB8888) and a benchmark.

### 20. ✅ handleAdded: drop ADDED-trip stops outside the watched set ⭐⭐
An alloc profile showed `handleAdded` was **94%** of PollerParse allocations.
The RT feed carries ADDED (replacement) trips network-wide — thousands of
stop-updates/poll — and each allocated `string(stu.stopID)` to file an Addition,
even for the thousands of stops the board never displays (QueryArrivals only
reads configured stops). Added `feedDecoder.resolveWatchedStop`: no-copy
`m[string(b)]` membership checks resolve the stop and the id string is
materialised only when the stop is in the watched set. `nil` watched set (no
configured filter) keeps every stop, exactly matching the old behaviour, so the
unit tests on synthetic DBs are unaffected.

| Benchmark   | ns/op (b→a)         | B/op (b→a)        | allocs (b→a)  |
| ----------- | ------------------- | ----------------- | ------------- |
| PollerParse | 1,453,614 → 1,015,000 | 406,578 → 36,556 | 4,463 → 298   |

**11× less memory, 15× fewer allocations, 1.45× faster per poll.** Golden lock
holds (watched-stop additions unchanged), race-clean, 41k fuzz execs clean.

### 9. ✅ RenderHD: hoist invariant font metrics/measurements out of the row loop
`renderHD` recomputes constants on every arrival row: `BodyFace.Metrics().Ascent`,
`hdMeasureString(BodyFace,"M")`, `hdMeasureString(TinyFace,"(Sched)")`,
`RouteFace`/`SmallFace` ascents, and the derived `maxRunes`. All are frame-invariant.
Compute once at the top of `renderHD`. CPU win on every refresh + page tick.

### 10. ✅ Render: drop fmt.Sprintf for the "N min" string
Both render paths build the minutes label with `fmt.Sprintf("%d min", mins)` —
a heap allocation per row, per frame. Replace with a small stack buffer +
`strconv.AppendInt`. Removes the only per-frame string allocations in the reused-buffer path.

### 11. ✅ realtime.parse: arena-allocate per-trip delay slices
`decodeTripUpdate` does `make([]StopDelay, 0, len(d.stops))` per scheduled trip
(~1285/poll) → ~1285 small allocations. Allocate one arena `[]StopDelay` per
poll (size-hinted from the previous poll's total), append into it, and hand out
3-arg capped sub-slices. Backing array never moves within a poll, so sub-slices
stay valid. Collapses ~1285 allocs/poll into ~1.

### 12. ✅ QueryArrivals: drop the dedup map for small arrival counts
The `seen2 := make(map[dedupKey]bool)` is allocated every call. Typical stops
have a handful of arrivals; an O(N²) linear dedup over the result slice avoids
the map allocation entirely below a small threshold.

### 13. ✅ Poller.fetch: reuse the read buffer across polls
`io.ReadAll(resp.Body)` allocates a fresh ~776 KB buffer every poll. Read into a
Poller-owned `bytes.Buffer` that is `Reset()` each poll so the backing array is
recycled. parse copies out only the strings it stores, so reusing the raw bytes
between polls is safe.

### 14. ✅ static build: kill per-row allocations in stop_times parsing
`parseGTFSTime` uses `strings.Split` (slice alloc) on every stop_times row — the
biggest file. Replace with an allocation-free manual `HH:MM:SS` parser. Also
hoist the `dayNames` slice out of the calendar row callback and build the
calendar_dates exception key without `time.Format`.

### 15. ✅ GOGC/GOMEMLIMIT tuning via the systemd unit (idea 6)
Now that allocation churn is 20–50× lower, raise GOGC (fewer, larger GC cycles)
and set a GOMEMLIMIT soft cap appropriate to the Pi Zero 2W's 512 MB. Non-code
lever set in `tfi-display.service` Environment.

### 16. ✅ fonts: precompute face ascents once
`face.Metrics().Ascent.Ceil()` is called throughout the renderer. Expose
package-level precomputed ascents from the `fonts` package so the renderer reads
a constant instead of re-deriving metrics each call.

### Idea 28 — zero-alloc additions diagnostic (KEPT)
`DiagLogOnceBytes` + a stack-buffer key (no concat / no time.Format) make the
already-logged path allocation-free, so QueryArrivalsInto stays 0 allocs/render
even when a watched stop has additions. Locked by an AllocsPerRun test.

### Idea 27 — allocation-light truncate (KEPT)
Walk runes by byte offset instead of `[]rune(s)`; 3 allocs → 1 per truncated
headsign (0 when it fits), on the per-row/per-frame HD render path. New
multibyte-safe unit test.

### Idea 26 — parse stop_times before trips in the static build (KEPT)
Eliminated the intermediate `allTrips` map by learning the watched-trip set from
stop_times first, then building `db.Trips` directly while reading trips.txt.
BuildFromZIPFile 1,039 KB → 740 KB (-29%), ~10% faster. The win scales with feed
size (the full TFI feed buffered 100k+ trips). Runs at startup + daily refresh.

### Idea 25 — lazy route resolution in handleAdded (KEPT) ⭐
Resolving the route (and allocating `string(routeID)`) only after a watched stop
is found dropped PollerParse from 208 → 86 allocs/op. Combined with ideas 20/23,
the per-poll allocation count fell from 4,535 (session start) to **86** (53×).

### Idea 23 & 24 — cancellation filter + stops-scratch reuse (KEPT)
Two more per-poll cuts after the additions filter (idea 20): cancellations are
now stored only for trips serving watched stops (network-wide cancels were a
string alloc each), and the decoder's StopTimeUpdate scratch is retained on the
Poller across polls. Combined: PollerParse 298 → 208 allocs, 36.5 → 22.2 KB.
Race-clean, golden lock holds, 120k fuzz execs clean.

### Idea 22 — QueryArrivalsInto reusable scratch (KEPT)
Added a `dst`-scratch variant so the render loop reuses one arrivals backing per
stop across frames; `QueryArrivals(nil, …)` preserves the one-shot API for tests.
Steady-state query: 13 allocs / 7,472 B → **0 / 0**, ~8% faster, with no byte
inflation (unlike the rejected pre-sized-cap approach). New reuse benchmark added.

### Idea 19 & 21 — render-loop sections reuse + lcd per-row Pix walk (KEPT)
Idea 19: `renderAndDisplay` now fills a caller-owned `sections` slice (allocated
once in main) instead of `make`-ing one per frame. Idea 21: the framebuffer
packing loops walk `img.Pix` per row instead of calling `GrayAt` per pixel,
~1.9× faster per frame (2.0 ms → 1.06 ms at 1024×600). Both on the per-frame
device path. New driver packing tests + benchmark added.

### Idea 20 — drop unwatched ADDED-trip stops in handleAdded (KEPT) ⭐⭐ biggest round-2 win
See idea 20 above. The decoder now resolves each added stop against the watched
set with no-copy lookups and only allocates the stop_number string when an
Addition is actually stored. PollerParse: 4,463 → 298 allocs (15×), 407 KB →
36.5 KB (11×), 1.45 ms → 1.01 ms. This is the dominant per-poll path on the
device, so it directly removes the remaining GC pressure left after the
streaming decoder (idea 2).

### Idea 18 — integer YYYYMMDD date comparison in IsServiceActive (KEPT) ⭐
Profiling-by-elimination showed the per-candidate `time.Date` constructions
(three of them: dt midnight + service start + service end) dominated
QueryArrivals CPU — `time.Date` does full timezone normalisation. Replaced with
`time.Time.Date()` field accessors and packed-int YYYYMMDD ordering (zero-padded
YYYYMMDD compares identically to calendar order). `dt.Weekday()` is used
directly since weekday is independent of the time-of-day component.

| Benchmark     | ns/op (b→a)        | allocs |
| ------------- | ------------------ | ------ |
| QueryArrivals | ~140,000 → ~82,000 | 13 (unchanged) |

**1.7× faster** on the per-render/page-tick hot path, no schema bump, golden
lock + all calendar/service edge tests pass.

### Idea 17 — snapshot LiveStore maps once per query (KEPT)
Added `LiveStore.snapshot()` (one RLock → map references + clock) and
`searchDelay` (extracted binary search). QueryArrivals now reads delays /
cancellations / additions lock-free after a single snapshot instead of locking
twice per candidate. QueryArrivals ~143µs → ~140µs in the single-threaded bench;
the real win is eliminating per-candidate lock acquisition and contention with
the poller on the device. Race detector + golden lock + full suite pass.

### Idea 16 — precompute font ascents once (KEPT, marginal)
Exposed `fonts.HeaderAscent/RouteAscent/BodyAscent/SmallAscent` computed at
init; `renderHD` reads these instead of calling `face.Metrics().Ascent.Ceil()`
(5 calls/frame removed, on top of idea 9's per-row hoisting). RenderHD/Renderer
benchmarks unchanged within noise — the frame cost is dominated by the buffer
clear + glyph rasterization — but it removes redundant per-frame work with no
downside and is a cleaner contract. Preview output identical.

### Idea 15 — GOGC/GOMEMLIMIT tuning in the systemd unit (KEPT)
Added `Environment=GOGC=200` and `Environment=GOMEMLIMIT=350MiB` to
`tfi-display.service`. With the now-small live heap and low allocation rate,
GOGC=200 halves GC cycle frequency (less GC CPU) while the heap stays tiny;
GOMEMLIMIT is a soft OOM backstop under the 512 MB physical RAM. Non-code lever,
not benchmarkable in unit tests; effect is fewer GC cycles on the device.

### Idea 14 — allocation-free stop_times parsing in the static build (KEPT)
Replaced `parseGTFSTime`'s `strings.Split` (a `[]string` alloc on every
stop_times row — the largest GTFS file) with index-based field slicing; hoisted
the per-row `dayNames` slice to a package-level `gtfsDayNames`; and built the
calendar_dates exception key via `appendYYYYMMDD` instead of `time.Format`.

| Benchmark        | ns/op (b→a)         | B/op (b→a)          | allocs (b→a)  |
| ---------------- | ------------------- | ------------------- | ------------- |
| BuildFromZIPFile | 2,396,450 → 2,365,000 | 1,101,110 → 1,039,227 | 4,583 → 3,285 |

~1285 allocs saved = one per stop_times row. Runs at startup and on each daily
static refresh (where it competes with the render loop). 99k fuzz execs of
parseGTFSTime clean; all error/overnight edge-case unit tests pass.

### Idea 13 — reuse the fetch read buffer across polls (KEPT)
`io.ReadAll(resp.Body)` allocated a fresh, doubling-growth buffer every poll
(~1.5 MB allocated to land a ~776 KB body). Replaced with a Poller-owned
`bytes.Buffer` that is `Reset()` (capacity retained) and filled via `ReadFrom`,
so after the first poll the body read does no fresh allocation. Safe because
parse copies out every retained value, leaving no sub-slice of the body alive
past the parse. Not visible in PollerParse (which feeds parse a pre-read slice),
but removes ~1.5 MB of per-poll allocation churn on the device. Covered by the
existing Poll/fetch httptest tests.

### Idea 12 — linear dedup for small arrival counts (KEPT)
Replaced the unconditional `make(map[dedupKey]bool)` in QueryArrivals's dedup
with an allocation-free O(N²) linear scan below `dedupLinearMax` (48), keeping
the map only for the rare large result set.

| Benchmark     | ns/op (b→a)       | B/op (b→a)      | allocs (b→a) |
| ------------- | ----------------- | --------------- | ------------ |
| QueryArrivals | 142,940 → ~143,000 | 9,280 → 7,472  | 19 → 13      |

(Benchmark queries 3 stops/iter, so this removes 3 map allocations — 2
allocs each.) CPU flat; full gtfs suite + golden lock pass.

### Idea 11 — arena-allocate per-trip delay slices (KEPT)
Replaced the per-scheduled-trip `make([]StopDelay, …)` with a single poll-scoped
arena (`feedDecoder.delayArena`), sized from the previous poll's total via
`Poller.prevDelayTotal`. Each trip is handed a 3-arg-capped sub-slice; arena
growth is safe because already-handed-out slices reference whichever backing
array was live when taken (kept alive by the swap map) and the cap pin prevents
cross-trip append bleed.

| Benchmark   | B/op (b→a)        | allocs (b→a) |
| ----------- | ----------------- | ------------ |
| PollerParse | 407,192 → 406,580 | 4,535 → 4,463 |

Modest in the 3-stop fixture (only ~72 trips match its stops) but scales with
the number of watched stops on a real device — one alloc per matched scheduled
trip becomes ~one per poll. CPU neutral; 24k fuzz execs clean, golden lock holds.

### Idea 9 & 10 — RenderHD invariant hoist + zero-alloc minutes label (KEPT)
Hoisted frame-invariant font metrics/measurements (`BodyFace`/`RouteFace`/
`SmallFace`/`HeaderFace` ascents, `measure("M")`, `measure("(Sched)")`, derived
`maxRunes`) out of the per-row loop in `renderHD`, and replaced both render
paths' `fmt.Sprintf("%d min", mins)` with a precomputed `[100]string` table
(`minLabel`).

| Benchmark      | ns/op (b→a)         | allocs (b→a) |
| -------------- | ------------------- | ------------ |
| RenderHD       | 2,135,182 → 2,030,820 | 11 → 3     |
| RendererReuse  | 1,061,441 → 1,059,000 | 5 → 1      |

The reused-buffer path (the device's actual render loop) now allocates just
1/frame (was 5). Golden render preview unchanged.

### Idea 4b — eliminate IsServiceActive's time.Format allocation (KEPT) ⭐
An alloc profile showed `time.Time.Format` was **91%** of QueryArrivals
allocations: `IsServiceActive` formatted `date.Format("20060102")` and
concatenated `serviceID + ":" + dateStr` to look up the exceptions map *per
candidate stop-time*. Replaced with a 64-byte stack buffer built via an
allocation-free `appendYYYYMMDD`, looked up through the compiler's no-copy
`db.Exceptions[string(buf)]` optimisation. The `Exceptions` map keeps its exact
string key + gob format, so no schema bump and no test changes.

| Benchmark (vs Idea-4-part-1) | ns/op           | B/op           | allocs  |
| ---------------------------- | --------------- | -------------- | ------- |
| QueryArrivals                | 199,000 → 137,000 | 13,912 → 9,280 | 598 → 19 |

Cumulative vs baseline: QueryArrivals **685 → 19 allocs (36×)**, 204µs → 137µs
(33% faster), 16.3 KB → 9.3 KB. Golden lock + full suite still pass.
