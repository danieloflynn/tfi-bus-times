# Resource-reduction optimization log

Goal: reduce the CPU time and memory the TFI board app uses at runtime, especially on
the target hardware (Raspberry Pi Zero 2W — 512 MB RAM, modest CPU).

## Method (per attempt)

Each idea is one numbered markdown file in this folder. The cycle is:

1. **Hypothesise** — write `NNN-<name>.md` with the idea, where the cost is, and the
   expected effect, *before* changing code.
2. **Measure baseline** — the relevant benchmark(s) with `-benchmem -count=6`.
3. **Implement** — the change, keeping behaviour identical (the hardening test suite is
   the safety net).
4. **Re-measure + test** — re-run the benchmark and the full suite (`go test ./...`,
   and `go test -race ./...` for anything touching the concurrent paths).
5. **Decide**:
   - **Win** (faster/leaner *and* all tests pass) → commit, add a row to `CHANGELOG.md`,
     mark the attempt file ✅.
   - **No win / regression / test failure** → `git restore`/revert the change, mark the
     attempt file ❌ with what happened, move on.

The benchmarks (added by the test-suite-hardening work) are the fitness function:

| Benchmark | Baseline (recorded in 000-baseline.md) |
| --- | --- |
| `BenchmarkFuzzBench_PollerParse` | ~12 ms/op, ~210k allocs/op (parses the 776 KB realtime feed every poll) |
| `BenchmarkFuzzBench_QueryArrivals` | ~251 µs/op, ~676 allocs/op (per stop, per render) |
| `BenchmarkFuzzBench_RenderHD` | ~3.4 ms/op, ~615 KB/op, 40 allocs/op (per frame) |
| `BenchmarkFuzzBench_BuildFromZIPFile` | startup + weekly refresh |
| `BenchmarkFuzzBench_GetDelay` | ~47 ns/op, 0 allocs/op (already optimal) |

## Idea roadmap (worked sequentially; not all will succeed)

1. Reuse the decoded protobuf `FeedMessage` across polls (cut parse allocs/GC churn).
2. Pre-size the maps/slices built in `parse()` (newDelays/newCancels/newAdds, delays).
3. Reuse the render image buffer across frames (eliminate ~615 KB alloc per frame).
4. Reduce per-call allocations in `QueryArrivals` (dedup/seen maps, tryHours).
5. Reuse `font.Drawer`/uniform sources in the renderers.
6. Intern repeated strings (tripID/routeShort) in `StaticDB` to cut steady-state memory.

Ideas may be added/reordered as measurements reveal where the real cost is.

## Index

- `000-baseline.md` — recorded baseline numbers + methodology notes.
- `CHANGELOG.md` — the running list of changes that actually landed.
