# Optimization changelog

Running list of changes that **actually landed** (committed) on the
`perf-resource-reduction` branch. Each row links to its attempt write-up. Reverted
experiments are not listed here — they live as ❌ entries in their own `NNN-*.md`.

Baseline (see `000-baseline.md`): PollerParse 12.0 ms / 7.41 MB / 210k allocs ·
BuildFromZIPFile 2.58 ms / 1.11 MB · RenderHD 2.22 ms / 615 KB · QueryArrivals
251 µs / 16 KB · GetDelay 31.6 ns.

| # | Change | Benchmark effect | Commit |
| --- | --- | --- | --- |
| 001 | `parse`: wire-level pre-filter — only fully decode the trip-updates serving our stops (+ added/cancelled), not all ~2,900 | `PollerParse` time −76.5% (12.0→2.8 ms), memory −79.3% (7.41→1.53 MB), allocs −84.9% (210k→31.7k) | 58159ac |
| 002 | `parse`: keep `trip_id` as bytes; no-alloc `db.Trips[string(idb)]` lookup, materialise the string only when stored | `PollerParse` allocs −8.1% (31.7k→29.1k), time −3.3%, mem −2.6% | 4bf80b1 |
| 003 | `parse`: wire-decode the relevant trip-updates' stop_time_updates too — `proto.Unmarshal`/`gtfsrt` removed from the poll path | `PollerParse` time −50% (2.6→1.3 ms), mem −64.6% (1.45 MB→514 KB), allocs −83.2% (29.1k→4.9k) | _next_ |

**Cumulative on `PollerParse` (the dominant hot path) vs original baseline:**
time **−89.2% (9.2×)**, memory **−93.1% (14.4×)**, allocs **−97.7% (43×)**.
