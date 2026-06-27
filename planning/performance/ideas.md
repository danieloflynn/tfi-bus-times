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

### 2. ⬜ Custom streaming protobuf decoder for the RT feed
- `proto.Unmarshal` allocates a struct for every entity, trip descriptor, stop
  update and stop-time event. We only need a handful of scalar fields. Walk the
  protobuf wire format directly with `protowire`, extracting only what we use,
  never materialising the object graph.
- Expected: the big one — should cut PollerParse allocs and bytes by an order of
  magnitude. Higher risk; guarded by existing parse tests + fuzz + golden tests.

### 3. ⬜ Reuse the decoded feed message across polls
- If keeping `proto.Unmarshal`, reuse a single `FeedMessage` via `Reset()` so the
  backing arrays are recycled instead of re-allocated each poll.

### 4. ⬜ QueryArrivals allocation reduction
- 685 allocs per call, called every few seconds. Inspect for per-call slice/map
  allocations that could be pooled or preallocated.

### 5. ⬜ RenderHD allocation / buffer reuse
- 615 KB per render. Check whether the image buffer and intermediate draws can be
  reused across frames instead of freshly allocated.

### 6. ⬜ GOGC / GOMEMLIMIT tuning for the device
- Non-code lever: tune GC aggressiveness for a low-churn long-running process.

---

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
