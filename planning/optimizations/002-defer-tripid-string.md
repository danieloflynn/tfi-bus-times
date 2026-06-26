# 002 — Defer the `trip_id` string allocation in `parse`

**Target:** `PollerParse` (post-001: 2.69 ms / 1.53 MB / 31,700 allocs).

## Hypothesis

After 001, a re-profile shows ~20% of the remaining allocations are in `parse`
itself — chiefly `tripID = string(idb)`, which converts every entity's `trip_id`
to a Go string (all ~2,857 entities) even though only ~156 are used (cancellations
+ scheduled-in-db). Added trips don't use it at all.

Go's compiler elides the allocation for `m[string(b)]` when the conversion is used
directly as a map index in a *read*. So: keep the `trip_id` as raw bytes, use
`db.Trips[string(idb)]` (no alloc) for the relevance check, and only materialise
the string when it must be stored as a map key (cancellations, `newDelays`).

Saves ~2,700 transient string allocations per poll.

## Result — ✅ WIN, committed

`benchstat` vs post-001 baseline (count=6, p=0.002), all tests passing unchanged:

| metric | post-001 | after 002 | change |
| --- | --- | --- | --- |
| time/op | 2.69 ms | 2.60 ms | −3.3% |
| bytes/op | 1.53 MB | 1.49 MB | −2.6% |
| allocs/op | 31,700 | 29,120 | **−8.1%** |

Small but free and statistically significant — ~2,580 transient `trip_id` strings
eliminated per poll. Kept.
