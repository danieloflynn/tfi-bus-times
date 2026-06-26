# 000 — Baseline

Recorded on the `perf-resource-reduction` branch (off `test-suite-hardening`), host
darwin/amd64, Go 1.26, `-benchmem -count=6`. Logging is silenced during the run by
`gtfs/benchmain_test.go` (a `TestMain` that discards slog — changes no test behaviour,
only keeps benchmark output clean for benchstat).

Command:

```
go test -bench . -benchmem -run '^$' -count=6 ./gtfs/ ./display/
```

## Baseline numbers (median of 6)

| Benchmark | time/op | bytes/op | allocs/op | Notes |
| --- | --- | --- | --- | --- |
| `PollerParse` | **12.0 ms** | **7.41 MB** | **210,044** | parses the 776 KB realtime feed every poll (~60s). **Prime target.** |
| `BuildFromZIPFile` | 2.58 ms | 1.11 MB | 4,582 | startup + weekly static refresh |
| `RenderHD` | 2.22 ms | 615 KB | 40 | per frame (every refresh + page tick) |
| `QueryArrivals` | 251 µs | 16.1 KB | 676 | per stop, per render |
| `GetDelay` | 31.6 ns | 0 | 0 | already allocation-free |

## Where the cost is

- **`PollerParse`** dominates everything: 7.4 MB / 210k allocs per poll. The bulk is
  `proto.Unmarshal` decoding all 2857 trip-update entities (15666 stop_time_updates),
  when only ~73 of those trips serve our stops. This is both the biggest memory churn
  and the biggest GC/CPU driver at steady state.
- `RenderHD` allocates a fresh 1024×600 `image.Gray` (~615 KB) every frame.
- `QueryArrivals` builds several transient maps/slices per call.

## Measurement protocol for each attempt

```
# baseline already in /tmp/bench_base.txt
go test -bench <Name> -benchmem -run '^$' -count=6 ./<pkg>/ > /tmp/bench_new.txt
benchstat /tmp/bench_base.txt /tmp/bench_new.txt
go test ./...            # all tests must still pass, unchanged
go test -race ./gtfs/    # for changes to the concurrent paths
```
