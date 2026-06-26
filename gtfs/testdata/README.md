# GTFS test fixtures

## Canonical capture — DO NOT RE-CAPTURE

The realtime feed `tripupdates.gtfsr` was captured **once** from the live TFI
GTFS-Realtime TripUpdates endpoint. Every fixture-based test pins `now` to the
feed's own header timestamp so results are deterministic and reproducible:

| Field | Value |
| --- | --- |
| Feed header `timestamp` (unix) | **1782497185** |
| Feed header time (UTC) | **2026-06-26T18:06:25Z** |
| Feed header time (Europe/Dublin) | **2026-06-26T19:06:25+01:00** |
| Wall-clock capture time (UTC) | 2026-06-26T18:06:32Z |
| `gtfs_realtime_version` | 2.0 |
| Incrementality | FULL_DATASET |

**The canonical test instant is `time.Unix(1782497185, 0)`.** Use it (in UTC, or
`Europe/Dublin` for display assertions) as `now` in every test that consumes these
fixtures, so a captured delay/ETA maps to a fixed minutes-until value.

### Feed profile (for building trimmed / synthetic variants)

- 2857 trip-update entities: 2652 scheduled, 122 added, 83 cancelled.
- 15666 stop_time_updates with an arrival: 2836 carry an absolute time, 9304 carry
  a non-zero delay.
- 5317 distinct stop_ids, 318 distinct routes.
- Feed stop_ids are raw GTFS ids (e.g. `8220DB000614`), not stop_codes — exercising
  the `StopIDToNumber` resolution path.

### Configured stops (trim target)

`config.yaml` watches stop_codes `478` (Vinny's), `2808` (Sandymount),
`999126` (Dart); routes `4, 7, 7A, S2, DART`.

## Files

- `tripupdates.gtfsr` — raw canonical realtime capture (preserved verbatim).
- Trimmed / synthetic variants are derived from this + the static ZIP via
  `tools/` and documented alongside as they are added.

## Regenerating derived fixtures

Derived fixtures are reproducible from the two raw captures; the raw realtime feed
is **not** reproducible (the live feed moves on), so it is committed verbatim and
must never be overwritten.
