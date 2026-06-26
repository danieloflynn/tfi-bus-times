# Optimization changelog

Running list of changes that **actually landed** (committed) on the
`perf-resource-reduction` branch. Each row links to its attempt write-up. Reverted
experiments are not listed here — they live as ❌ entries in their own `NNN-*.md`.

Baseline (see `000-baseline.md`): PollerParse 12.0 ms / 7.41 MB / 210k allocs ·
BuildFromZIPFile 2.58 ms / 1.11 MB · RenderHD 2.22 ms / 615 KB · QueryArrivals
251 µs / 16 KB · GetDelay 31.6 ns.

| # | Change | Benchmark effect | Commit |
| --- | --- | --- | --- |
| 001 | `parse`: wire-level pre-filter — only fully decode the trip-updates serving our stops (+ added/cancelled), not all ~2,900 | `PollerParse` time −76.5% (12.0→2.8 ms), memory −79.3% (7.41→1.53 MB), allocs −84.9% (210k→31.7k) | _next_ |
