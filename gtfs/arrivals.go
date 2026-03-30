package gtfs

import (
	"sort"
	"time"
)

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

	key := serviceID + ":" + date.Format("20060102")
	if ex, ok := db.Exceptions[key]; ok {
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
func QueryArrivals(
	db *StaticDB,
	live *LiveStore,
	stopNumber string,
	now time.Time,
	maxMinutes int,
	routeFilter map[string]bool,
) []Arrival {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	nowSecs := now.Hour()*3600 + now.Minute()*60 + now.Second()
	windowEnd := now.Add(time.Duration(maxMinutes) * time.Minute)

	// Determine which hour buckets to scan.
	// We look back 1 hour (to catch delayed buses) and forward enough to fill window.
	extraHours := maxMinutes/60 + 2
	var tryHours []int
	startHour := now.Hour() - 1
	if startHour < 0 {
		startHour = 23
	}
	seen := make(map[int]bool)
	for i := 0; i <= extraHours+1; i++ {
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
		if len(routeFilter) > 0 && !routeFilter[add.RouteShortName] {
			continue
		}
		if add.ArrivalTime.Before(now) || add.ArrivalTime.After(windowEnd) {
			continue
		}
		arrivals = append(arrivals, Arrival{
			RouteShort:    add.RouteShortName,
			ScheduledTime: add.ArrivalTime,
			RealtimeTime:  add.ArrivalTime,
			IsAdded:       true,
		})
	}

	// Deduplicate: same tripID can appear in multiple hour buckets.
	seen2 := make(map[string]bool)
	deduped := arrivals[:0]
	for _, a := range arrivals {
		key := a.RouteShort + "|" + a.ScheduledTime.String()
		if !seen2[key] {
			seen2[key] = true
			deduped = append(deduped, a)
		}
	}

	sort.Slice(deduped, func(i, j int) bool {
		return deduped[i].EffectiveTime().Before(deduped[j].EffectiveTime())
	})
	return deduped
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
