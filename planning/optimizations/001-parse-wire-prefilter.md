# 001 — Wire-level pre-filter in `Poller.parse`

**Target:** `PollerParse` — baseline 12.0 ms / 7.41 MB / 210,044 allocs per poll.

## Hypothesis

A memory profile of `PollerParse` shows **98.4% of allocations come from
`proto.Unmarshal`** (`reflect.unsafe_New` 40%, `consumeStringPtr` 38%, the rest
varint/message slices). Our own map-building is ~1.5%. So optimizing our code is
pointless — the cost is decoding the protobuf.

The feed is a `FULL_DATASET` of **every trip in the network (~2,857 entities,
15,666 stop_time_updates)**, but only the trips that serve our 3 stops matter:
~73 scheduled-with-delays + 122 added + 83 cancelled. We currently fully decode
all 2,857 just to throw ~2,580 away (`nUnknown`).

**Idea:** walk the feed at the wire level (`protobuf/encoding/protowire`) and read
each entity's `trip_id` + `schedule_relationship` cheaply (no allocation of the
stop-time-update tree). Only run the full `proto.Unmarshal` on the trip-updates we
actually use:

- cancelled → record `trip_id` only (no stop_time_update decode at all);
- added → full decode (needed for arrivals at our stops);
- scheduled → full decode **only if `trip_id` is in `db.Trips`**, else skip.

The expensive per-stop-time-update decoding logic is unchanged — it still runs via
`proto.Unmarshal` on the relevant `TripUpdate` bytes — so the test suite (golden
e2e + realtime edge cases) fully guards correctness.

GTFS-realtime field numbers are fixed by the spec, so reading them from the wire is
stable.

## Expected effect

Decode ~195 trip-updates instead of ~2,857 → large drop in allocs/bytes/time for
`PollerParse`. Must not change any observable output (all tests pass unchanged).

## Result — ✅ WIN, committed

`benchstat` (count=6), all tests passing unchanged (full `go test ./...` +
`go test -race ./gtfs/`):

| metric | before | after | change |
| --- | --- | --- | --- |
| time/op | 12.03 ms | 2.83 ms | **−76.5%** |
| bytes/op | 7.41 MB | 1.53 MB | **−79.3%** |
| allocs/op | 210,044 | 31,700 | **−84.9%** |

The remaining cost is the full `proto.Unmarshal` of the ~195 relevant trip-updates
plus our map building. Behaviour is identical — the golden end-to-end test and all
realtime edge tests pass without modification. Kept.
