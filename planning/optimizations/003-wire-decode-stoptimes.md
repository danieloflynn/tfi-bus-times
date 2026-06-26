# 003 — Wire-decode the relevant trip-updates (drop `proto.Unmarshal`)

**Target:** `PollerParse` (post-002: 2.60 ms / 1.45 MB / 29,119 allocs).

## Hypothesis

After 001/002, a re-profile shows ~79% of the remaining allocations are the
`proto.Unmarshal` of the ~195 relevant trip-updates (message structs via
`reflect.unsafe_New`, string fields via `consumeStringPtr`, `int64` times). We were
still building the full decoded `TripUpdate`/`StopTimeUpdate`/`StopTimeEvent` tree
for each.

**Idea:** decode the stop-time-updates at the wire level too, reading only the
fields we use:

- scheduled: `stop_sequence`, `schedule_relationship` (skip SKIPPED),
  `arrival.time` / `arrival.delay`;
- added: `stop_id`, `arrival.time` / `departure.time`, `trip.route_id`.

This removes `proto.Unmarshal` (and the `gtfsrt`/`proto` imports) from the hot path
entirely. The decode logic is a line-for-line translation of the previous
proto-based code, so behaviour is identical — guarded by the golden e2e test and
the realtime edge tests (abs-time vs delta, SKIPPED, implausible-delay discard,
departure fallback, dedup, rail stop-id resolution).

GTFS-realtime field numbers are fixed by the spec; the new ones are named constants
with comments.

## Result — ✅ WIN, committed

`benchstat` vs post-002 baseline (count=6, p=0.002), all tests pass unchanged
(full `go test ./...` + `-race ./gtfs/`):

| metric | post-002 | after 003 | change | **cumulative vs original** |
| --- | --- | --- | --- | --- |
| time/op | 2.60 ms | 1.30 ms | −50.0% | **−89.2% (9.2×)** |
| bytes/op | 1.45 MB | 514 KB | −64.6% | **−93.1% (14.4×)** |
| allocs/op | 29,119 | 4,879 | −83.2% | **−97.7% (43×)** |

`PollerParse` now allocates 4.9k objects / 514 KB per poll, down from 210k / 7.4 MB.
Kept.
