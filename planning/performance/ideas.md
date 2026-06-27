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
