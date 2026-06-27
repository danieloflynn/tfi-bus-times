package gtfs

import (
	"log/slog"
	"slices"
	"time"
)

// lookbackHours is how many hours of scheduled stop-time buckets QueryArrivals
// scans before the current hour. It bounds the maximum delay we can still
// surface (a trip delayed more than this past its schedule is no longer
// matched). Three hours covers realistic disruption delays while keeping the
// per-query scan trivially cheap.
const lookbackHours = 3

// dedupLinearMax is the arrival-count threshold below which QueryArrivals
// deduplicates with an allocation-free O(N²) linear scan instead of a map. Real
// stops rarely exceed a few dozen arrivals in the window, so the linear path is
// the common case and avoids the per-query map allocation.
const dedupLinearMax = 48

// Arrival is one upcoming bus arrival as shown on the display.
type Arrival struct {
	RouteShort    string
	Platform      string    // platform_code from stops.txt; empty if not available
	Headsign      string
	ScheduledTime time.Time
	RealtimeTime  time.Time // zero if no realtime data
	DelayMinutes  int       // signed; 0 if no realtime data
	IsAdded       bool
}

// EffectiveTime returns the best available arrival time.
func (a Arrival) EffectiveTime() time.Time {
	if !a.RealtimeTime.IsZero() {
		return a.RealtimeTime
	}
	return a.ScheduledTime
}

// MinutesUntil returns minutes until effective arrival from now.
func (a Arrival) MinutesUntil(now time.Time) int {
	d := a.EffectiveTime().Sub(now)
	return int(d.Seconds() / 60)
}

// IsServiceActive checks whether serviceID is scheduled to run on the date
// portion of dt. It applies calendar_dates exceptions (type 1 = force on,
// type 2 = force off).
func IsServiceActive(db *StaticDB, serviceID string, dt time.Time) bool {
	// Normalise to date-only (midnight) in the same timezone so that range
	// comparisons work regardless of the time-of-day component in dt.
	date := time.Date(dt.Year(), dt.Month(), dt.Day(), 0, 0, 0, 0, dt.Location())

	// Build "serviceID:YYYYMMDD" in a stack buffer and look it up via
	// m[string(b)] — the compiler elides the []byte→string copy, so this hot
	// path (run per candidate stop-time on every render) avoids the
	// time.Format + concatenation allocation that dominated the query's heap.
	var keyBuf [64]byte
	kb := keyBuf[:0]
	kb = append(kb, serviceID...)
	kb = append(kb, ':')
	y, mo, d := date.Date()
	kb = appendYYYYMMDD(kb, y, int(mo), d)
	if ex, ok := db.Exceptions[string(kb)]; ok {
		return ex == 1 // 1 = added, 2 = removed
	}
	svc, ok := db.Services[serviceID]
	if !ok {
		return false
	}
	// Normalise service dates too (they are stored as midnight UTC).
	start := time.Date(svc.StartDate.Year(), svc.StartDate.Month(), svc.StartDate.Day(), 0, 0, 0, 0, date.Location())
	end := time.Date(svc.EndDate.Year(), svc.EndDate.Month(), svc.EndDate.Day(), 0, 0, 0, 0, date.Location())
	if date.Before(start) || date.After(end) {
		return false
	}
	// GTFS weekday: Monday=0 … Sunday=6. Go's time.Weekday: Sunday=0, Monday=1, …
	// Convert: (go_weekday + 6) % 7 gives GTFS index.
	idx := (int(date.Weekday()) + 6) % 7
	return svc.Days[idx]
}

// QueryArrivals returns upcoming arrivals for stopNumber within maxMinutes,
// optionally filtered to routeFilter (empty = all routes), sorted by effective time.
//
// minMinutes is the walking time to the stop: arrivals sooner than this (by
// effective time) are dropped, since you couldn't get there in time. 0 = no
// such filtering.
func QueryArrivals(
	db *StaticDB,
	live *LiveStore,
	stopNumber string,
	now time.Time,
	maxMinutes int,
	minMinutes int,
	routeFilter map[string]bool,
) []Arrival {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	nowSecs := now.Hour()*3600 + now.Minute()*60 + now.Second()
	windowEnd := now.Add(time.Duration(maxMinutes) * time.Minute)
	// Walking cutoff: arrivals before this are too soon to catch. Buses arriving
	// exactly at the cutoff are still shown (strictly-under semantics).
	walkCutoff := now.Add(time.Duration(minMinutes) * time.Minute)

	// Determine which hour buckets to scan. Stop times are bucketed by their
	// *scheduled* hour, so to surface a severely delayed trip we must scan the
	// bucket its schedule falls in, which can be hours before now. We look back
	// lookbackHours (to catch such delays) and forward enough to fill the window.
	// Previously the lookback was a single hour, which dropped any trip delayed
	// more than ~1h: its scheduled bucket was never scanned, the realtime overlay
	// never applied, and the arrival vanished — exactly what happens during major
	// disruptions. Scanning a few extra buckets is sub-millisecond.
	extraHours := maxMinutes/60 + 2
	startHour := ((now.Hour()-lookbackHours)%24 + 24) % 24
	// Hours are 0–23, so a fixed [24] array backs both the dedup set and the
	// result slice — no map/slice heap allocation on this per-render hot path.
	var seen [24]bool
	var hoursArr [24]int
	tryHours := hoursArr[:0]
	for i := 0; i <= lookbackHours+extraHours+1; i++ {
		h := (startHour + i) % 24
		if !seen[h] {
			tryHours = append(tryHours, h)
			seen[h] = true
		}
	}

	var arrivals []Arrival

	stopHours := db.StopTimes[stopNumber]
	for _, hour := range tryHours {
		for _, st := range stopHours[hour] {
			trip, ok := db.Trips[st.TripID]
			if !ok {
				continue
			}

			// Route filter.
			if len(routeFilter) > 0 && !routeFilter[trip.RouteShort] {
				continue
			}

			// Reconstruct arrival datetime.
			arrSecs := st.ArrivalSecs
			// 12-hour rule: if the arrival (from midnight) is more than 12 hours
			// in the past relative to now, it belongs to tomorrow.
			if nowSecs-12*3600 > arrSecs {
				arrSecs += 24 * 3600
			}
			scheduledTime := midnight.Add(time.Duration(arrSecs) * time.Second)

			// Calendar check (on the actual arrival date).
			arrDate := scheduledTime
			if !IsServiceActive(db, trip.ServiceID, arrDate) {
				continue
			}

			// Cancellation check.
			if live.IsCancelled(st.TripID) {
				continue
			}

			// Apply realtime delay.
			var realtimeTime time.Time
			var delayMin int
			if sd, found := live.GetDelay(st.TripID, st.StopSequence); found {
				if sd.AbsTime != 0 {
					realtimeTime = time.Unix(sd.AbsTime, 0)
				} else {
					realtimeTime = scheduledTime.Add(time.Duration(sd.DelaySeconds) * time.Second)
				}
				delaySec := realtimeTime.Sub(scheduledTime).Seconds()
				delayMin = int(delaySec / 60)
			}

			// Effective time for window check.
			effectiveTime := scheduledTime
			if !realtimeTime.IsZero() {
				effectiveTime = realtimeTime
			}

			// Skip if already departed or beyond window.
			if !effectiveTime.After(now) && !scheduledTime.After(now) {
				continue
			}
			if effectiveTime.After(windowEnd) && scheduledTime.After(windowEnd) {
				continue
			}

			// Skip if it arrives sooner than we can walk there.
			if minMinutes > 0 && effectiveTime.Before(walkCutoff) {
				continue
			}

			// Diagnostic: a trip whose schedule is more than an hour before now
			// would have been dropped by the old single-hour lookback. Logging it
			// once per poll confirms when the widened window is what's keeping a
			// severely delayed service on the board.
			if !realtimeTime.IsZero() && scheduledTime.Before(now.Add(-time.Hour)) &&
				live.DiagLogOnce("delay|"+st.TripID) {
				slog.Info("rescued severely-delayed arrival (outside old 1h lookback)",
					"stop", stopNumber, "route", trip.RouteShort,
					"scheduled", scheduledTime.Format("15:04"),
					"expected", realtimeTime.Format("15:04"),
					"delay_min", delayMin)
			}

			arrivals = append(arrivals, Arrival{
				RouteShort:    trip.RouteShort,
				Platform:      db.StopPlatforms[stopNumber],
				Headsign:      trip.Headsign,
				ScheduledTime: scheduledTime,
				RealtimeTime:  realtimeTime,
				DelayMinutes:  delayMin,
			})
		}
	}

	// Add realtime additions.
	for _, add := range live.GetAdditions(stopNumber) {
		// Apply the route whitelist only when the route resolved to a real short
		// name. An unresolved route (raw route_id) can't be matched against the
		// whitelist, and these are typically the brand-new replacement services a
		// disruption spins up — show them rather than silently dropping them.
		if len(routeFilter) > 0 && add.RouteResolved && !routeFilter[add.RouteShortName] {
			continue
		}
		if add.ArrivalTime.Before(now) || add.ArrivalTime.After(windowEnd) {
			continue
		}
		// Skip if it arrives sooner than we can walk there.
		if minMinutes > 0 && add.ArrivalTime.Before(walkCutoff) {
			continue
		}
		// Diagnostic: confirms an added (unscheduled) service is being shown, and
		// whether its route had to be shown unfiltered because it couldn't be
		// resolved against the static feed.
		if live.DiagLogOnce("add|" + stopNumber + "|" + add.RouteShortName + "|" + add.ArrivalTime.Format("1504")) {
			slog.Info("showing added (unscheduled) arrival",
				"stop", stopNumber, "route", add.RouteShortName,
				"route_resolved", add.RouteResolved,
				"arrival", add.ArrivalTime.Format("15:04"))
		}
		arrivals = append(arrivals, Arrival{
			RouteShort:    add.RouteShortName,
			ScheduledTime: add.ArrivalTime,
			RealtimeTime:  add.ArrivalTime,
			IsAdded:       true,
		})
	}

	// Deduplicate: same tripID can appear in multiple hour buckets. Most stops
	// yield only a handful of arrivals, so a linear scan over the kept results
	// (in-place filter; reading already-written slots is safe) avoids allocating
	// the dedup map at all. Only fall back to a map above dedupLinearMax, where
	// the O(N²) scan would start to cost more than the map. Dedup key is
	// {route, scheduled-unix} (no formatted-string allocation).
	deduped := arrivals[:0]
	if len(arrivals) <= dedupLinearMax {
		for _, a := range arrivals {
			u := a.ScheduledTime.Unix()
			dup := false
			for j := range deduped {
				if deduped[j].RouteShort == a.RouteShort && deduped[j].ScheduledTime.Unix() == u {
					dup = true
					break
				}
			}
			if !dup {
				deduped = append(deduped, a)
			}
		}
	} else {
		type dedupKey struct {
			route string
			unix  int64
		}
		seen2 := make(map[dedupKey]bool, len(arrivals))
		for _, a := range arrivals {
			key := dedupKey{a.RouteShort, a.ScheduledTime.Unix()}
			if !seen2[key] {
				seen2[key] = true
				deduped = append(deduped, a)
			}
		}
	}

	// slices.SortFunc avoids sort.Slice's reflection + closure-escape overhead.
	slices.SortFunc(deduped, func(a, b Arrival) int {
		return a.EffectiveTime().Compare(b.EffectiveTime())
	})
	return deduped
}

// appendYYYYMMDD appends a zero-padded YYYYMMDD date to b, matching
// time.Format("20060102") without allocating. Used to build exception-map keys
// on the QueryArrivals hot path.
func appendYYYYMMDD(b []byte, y, m, d int) []byte {
	b = append(b, byte('0'+(y/1000)%10), byte('0'+(y/100)%10), byte('0'+(y/10)%10), byte('0'+y%10))
	b = append(b, byte('0'+(m/10)%10), byte('0'+m%10))
	b = append(b, byte('0'+(d/10)%10), byte('0'+d%10))
	return b
}

// BuildRouteFilter converts a slice of route short names into a lookup map.
// An empty slice means "all routes".
func BuildRouteFilter(routes []string) map[string]bool {
	if len(routes) == 0 {
		return nil
	}
	m := make(map[string]bool, len(routes))
	for _, r := range routes {
		m[r] = true
	}
	return m
}
