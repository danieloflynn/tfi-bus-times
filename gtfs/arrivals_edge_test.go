package gtfs

import (
	"testing"
	"time"
)

// This file covers the FR3 QueryArrivals edge cases that are NOT already
// asserted in arrivals_test.go. The disruption-fix trio (severe-delay rescue,
// delay-beyond-lookback drop, unresolved-route bypass) live there and are NOT
// duplicated here. Everything below uses the shared synthetic builders
// (makeTestDB / addStopTime / mustDate) with a fixed time.Date `now` — never
// time.Now — so each branch is deterministic.

// arrivalsEdgeServiceID is an all-week service spanning 2023 so synthetic trips
// added by these tests pass the calendar (IsServiceActive) check regardless of
// which `now` date a given case picks.
const arrivalsEdgeServiceID = "ARR_EDGE_SVC"

// arrivalsEdgeRegisterService installs the all-week service on db.
func arrivalsEdgeRegisterService(db *StaticDB) {
	db.Services[arrivalsEdgeServiceID] = Service{
		StartDate: mustDate("2023-01-01"),
		EndDate:   mustDate("2023-12-31"),
		Days:      [7]bool{true, true, true, true, true, true, true},
	}
}

// arrivalsEdgeAddTrip registers tripID on the all-week service and a single
// stop_time at stop, bucketed naturally by addStopTime ((arrSecs/3600)%24).
func arrivalsEdgeAddTrip(db *StaticDB, tripID, route, stop string, arrSecs, seq int) {
	db.Trips[tripID] = Trip{RouteShort: route, ServiceID: arrivalsEdgeServiceID, Headsign: route + " dest"}
	addStopTime(db, stop, StopTime{TripID: tripID, ArrivalSecs: arrSecs, StopSequence: seq})
}

// arrivalsEdgeForceBucket inserts st into db.StopTimes[stop][hour] verbatim,
// bypassing addStopTime's natural bucketing. Used to place the same stop_time in
// two hour buckets so QueryArrivals' de-duplication pass actually runs.
func arrivalsEdgeForceBucket(db *StaticDB, stop string, hour int, st StopTime) {
	if db.StopTimes[stop] == nil {
		db.StopTimes[stop] = make(map[int][]StopTime)
	}
	db.StopTimes[stop][hour] = append(db.StopTimes[stop][hour], st)
}

// arrivalsEdgeSetAbsDelay records an absolute-timestamp realtime arrival for
// tripID at stop sequence seq (the AbsTime delay branch).
func arrivalsEdgeSetAbsDelay(ls *LiveStore, tripID string, seq int, at time.Time) {
	ls.Delays[tripID] = []StopDelay{{StopSequence: int32(seq), AbsTime: at.Unix()}}
}

// arrivalsEdgeFind returns the first arrival with the given route short name, or
// nil if none is present.
func arrivalsEdgeFind(arrivals []Arrival, route string) *Arrival {
	for i := range arrivals {
		if arrivals[i].RouteShort == route {
			return &arrivals[i]
		}
	}
	return nil
}

// TestArrivalsEdge_OvernightRollover exercises the 12-hour rollover rule in both
// directions using the SAME early-morning ArrivalSecs (00:10). When `now` is late
// evening the schedule belongs to *tomorrow* (the `nowSecs-12h > arrSecs` branch
// fires, +24h applied); when `now` is just after midnight the very same schedule
// belongs to *today* (the branch does not fire). The contrast is the entire
// point of the rule.
func TestArrivalsEdge_OvernightRollover(t *testing.T) {
	const arrSecs = 0*3600 + 10*60 // 00:10, bucketed in hour 0

	t.Run("forward_late_evening_rolls_to_next_day", func(t *testing.T) {
		db := makeTestDB()
		arrivalsEdgeRegisterService(db)
		arrivalsEdgeAddTrip(db, "OVN_FWD", "OVN", "1358", arrSecs, 5)

		// Friday 2026-style date doesn't matter; use the suite's 2023 frame.
		now := time.Date(2023, 9, 15, 23, 50, 0, 0, time.UTC)
		arrivals := QueryArrivals(db, withClock(NewLiveStore(), now), "1358", now, 60, 0, nil)

		a := arrivalsEdgeFind(arrivals, "OVN")
		if a == nil {
			t.Fatal("00:10 arrival should be surfaced (rolled forward to tomorrow) when now is 23:50")
		}
		want := time.Date(2023, 9, 16, 0, 10, 0, 0, time.UTC)
		if !a.ScheduledTime.Equal(want) {
			t.Errorf("rolled scheduled time: want %v, got %v", want, a.ScheduledTime)
		}
		if a.ScheduledTime.Day() != now.Day()+1 {
			t.Errorf("scheduled time should be on the day after now (%d), got day %d",
				now.Day()+1, a.ScheduledTime.Day())
		}
		if !a.ScheduledTime.After(now) {
			t.Error("rolled-forward arrival must be in the future relative to now")
		}
	})

	t.Run("viceversa_early_morning_stays_same_day", func(t *testing.T) {
		db := makeTestDB()
		arrivalsEdgeRegisterService(db)
		arrivalsEdgeAddTrip(db, "OVN_SAME", "OVN", "1358", arrSecs, 5)

		// now is 00:00 — the SAME 00:10 schedule is 10 minutes ahead *today*,
		// the +24h branch must NOT fire.
		now := time.Date(2023, 9, 16, 0, 0, 0, 0, time.UTC)
		arrivals := QueryArrivals(db, withClock(NewLiveStore(), now), "1358", now, 60, 0, nil)

		a := arrivalsEdgeFind(arrivals, "OVN")
		if a == nil {
			t.Fatal("00:10 arrival should be surfaced (same day) when now is 00:00")
		}
		want := time.Date(2023, 9, 16, 0, 10, 0, 0, time.UTC)
		if !a.ScheduledTime.Equal(want) {
			t.Errorf("non-rolled scheduled time: want %v, got %v", want, a.ScheduledTime)
		}
		if a.ScheduledTime.Day() != now.Day() {
			t.Errorf("scheduled time should stay on now's day (%d), got day %d",
				now.Day(), a.ScheduledTime.Day())
		}
	})
}

// TestArrivalsEdge_LookbackBoundary pins the EXACT hour-bucket lookback edge
// (complementary to the coarser delay-beyond-lookback test in arrivals_test.go).
// With identical realtime overlays: a trip scheduled exactly lookbackHours ago
// (its bucket == the first scanned bucket) is surfaced, while one a single hour
// further back (its bucket is never scanned) is not — proving the bucket-scan
// window, not the time guard, is the boundary.
func TestArrivalsEdge_LookbackBoundary(t *testing.T) {
	db := makeTestDB()
	arrivalsEdgeRegisterService(db)

	now := time.Date(2023, 9, 15, 12, 0, 0, 0, time.UTC) // startHour = 12 - lookbackHours = 9

	// Exactly lookbackHours ago: 09:00 (bucket 9, the first scanned bucket).
	arrivalsEdgeAddTrip(db, "LBK_IN", "LBK_IN", "1358", (12-lookbackHours)*3600, 10)
	// One hour beyond: 08:00 (bucket 8, never scanned).
	arrivalsEdgeAddTrip(db, "LBK_OUT", "LBK_OUT", "1358", (12-lookbackHours-1)*3600, 10)

	ls := withClock(NewLiveStore(), now)
	// Same realtime treatment for both: an absolute arrival 5 min from now.
	rt := now.Add(5 * time.Minute)
	arrivalsEdgeSetAbsDelay(ls, "LBK_IN", 10, rt)
	ls.Delays["LBK_OUT"] = []StopDelay{{StopSequence: 10, AbsTime: rt.Unix()}}

	arrivals := QueryArrivals(db, ls, "1358", now, 60, 0, nil)

	in := arrivalsEdgeFind(arrivals, "LBK_IN")
	if in == nil {
		t.Fatalf("trip scheduled exactly lookbackHours (%dh) ago should be surfaced via its realtime overlay", lookbackHours)
	}
	if !in.RealtimeTime.Equal(rt) {
		t.Errorf("boundary trip realtime: want %v, got %v", rt, in.RealtimeTime)
	}
	if out := arrivalsEdgeFind(arrivals, "LBK_OUT"); out != nil {
		t.Errorf("trip scheduled lookbackHours+1 ago must not be surfaced; its bucket is never scanned, got %+v", out)
	}
}

// TestArrivalsEdge_DeduplicatesAcrossBuckets verifies the de-dup pass collapses a
// single trip that has been filed into two hour buckets (same route + same
// scheduled time) down to one arrival.
func TestArrivalsEdge_DeduplicatesAcrossBuckets(t *testing.T) {
	db := makeTestDB()
	arrivalsEdgeRegisterService(db)

	now := time.Date(2023, 9, 15, 9, 0, 0, 0, time.UTC)
	st := StopTime{TripID: "DUP_TRIP", ArrivalSecs: 9*3600 + 30*60, StopSequence: 10} // 09:30
	db.Trips["DUP_TRIP"] = Trip{RouteShort: "DUP", ServiceID: arrivalsEdgeServiceID, Headsign: "Dup"}
	// Same stop_time present in two adjacent (both-scanned) buckets.
	arrivalsEdgeForceBucket(db, "1358", 9, st)
	arrivalsEdgeForceBucket(db, "1358", 10, st)

	arrivals := QueryArrivals(db, withClock(NewLiveStore(), now), "1358", now, 60, 0, nil)

	count := 0
	for _, a := range arrivals {
		if a.RouteShort == "DUP" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("trip appearing in two buckets should be de-duplicated to 1 arrival, got %d", count)
	}
}

// TestArrivalsEdge_AbsTimeDelayBranch verifies the absolute-timestamp branch:
// when StopDelay.AbsTime is set it is used verbatim and DelaySeconds is ignored,
// with DelayMinutes derived from (AbsTime - scheduled).
func TestArrivalsEdge_AbsTimeDelayBranch(t *testing.T) {
	db := makeTestDB()
	arrivalsEdgeRegisterService(db)

	now := time.Date(2023, 9, 15, 9, 10, 0, 0, time.UTC)
	arrivalsEdgeAddTrip(db, "ABS_TRIP", "ABS", "1358", 9*3600+20*60, 10) // scheduled 09:20

	ls := withClock(NewLiveStore(), now)
	expected := time.Date(2023, 9, 15, 9, 27, 0, 0, time.UTC) // +7 min
	// AbsTime is set AND a bogus DelaySeconds: the AbsTime branch must win.
	ls.Delays["ABS_TRIP"] = []StopDelay{{StopSequence: 10, AbsTime: expected.Unix(), DelaySeconds: 9999}}

	arrivals := QueryArrivals(db, ls, "1358", now, 60, 0, nil)
	a := arrivalsEdgeFind(arrivals, "ABS")
	if a == nil {
		t.Fatal("absolute-time arrival not found")
	}
	if !a.RealtimeTime.Equal(expected) {
		t.Errorf("AbsTime branch realtime: want %v, got %v (DelaySeconds must be ignored)", expected, a.RealtimeTime)
	}
	if a.DelayMinutes != 7 {
		t.Errorf("AbsTime branch delay: want 7 min, got %d", a.DelayMinutes)
	}
}

// TestArrivalsEdge_WalkingFilterOnAdditions verifies the walking-time (minMinutes)
// filter is applied to realtime ADDITIONS, with strictly-under cutoff semantics
// (an addition arriving exactly at the cutoff is still shown).
func TestArrivalsEdge_WalkingFilterOnAdditions(t *testing.T) {
	db := makeTestDB()
	now := time.Date(2023, 9, 15, 9, 0, 0, 0, time.UTC)

	ls := withClock(NewLiveStore(), now)
	ls.Additions["1358"] = []Addition{
		{RouteShortName: "ADD_EARLY", ArrivalTime: now.Add(5 * time.Minute), RouteResolved: true},  // before 10m cutoff → dropped
		{RouteShortName: "ADD_CUTOFF", ArrivalTime: now.Add(10 * time.Minute), RouteResolved: true}, // exactly at cutoff → shown
		{RouteShortName: "ADD_LATER", ArrivalTime: now.Add(15 * time.Minute), RouteResolved: true},  // after cutoff → shown
	}

	arrivals := QueryArrivals(db, ls, "1358", now, 60, 10, nil) // 10-minute walk → cutoff 09:10

	if a := arrivalsEdgeFind(arrivals, "ADD_EARLY"); a != nil {
		t.Error("addition arriving before the walking cutoff should be dropped")
	}
	if a := arrivalsEdgeFind(arrivals, "ADD_CUTOFF"); a == nil {
		t.Error("addition arriving exactly at the walking cutoff should be shown (strictly-under)")
	} else if !a.IsAdded {
		t.Error("surfaced addition should be marked IsAdded")
	}
	if a := arrivalsEdgeFind(arrivals, "ADD_LATER"); a == nil {
		t.Error("addition arriving after the walking cutoff should be shown")
	}
}

// TestArrivalsEdge_AlreadyDepartedGuard exercises the dual-condition guard
//
//	if !effectiveTime.After(now) && !scheduledTime.After(now) { continue }
//
// which only drops an arrival when realtime AND schedule agree it is in the past.
// When they disagree (one past, one future), the arrival is kept. This locks the
// current behaviour in all three combinations.
func TestArrivalsEdge_AlreadyDepartedGuard(t *testing.T) {
	now := time.Date(2023, 9, 15, 9, 0, 0, 0, time.UTC)

	t.Run("both_past_dropped", func(t *testing.T) {
		db := makeTestDB()
		arrivalsEdgeRegisterService(db)
		arrivalsEdgeAddTrip(db, "GD_BP", "GD_BP", "1358", 8*3600+55*60, 10) // scheduled 08:55
		ls := withClock(NewLiveStore(), now)
		arrivalsEdgeSetAbsDelay(ls, "GD_BP", 10, time.Date(2023, 9, 15, 8, 57, 0, 0, time.UTC)) // realtime 08:57, also past

		arrivals := QueryArrivals(db, ls, "1358", now, 60, 0, nil)
		if a := arrivalsEdgeFind(arrivals, "GD_BP"); a != nil {
			t.Error("arrival with both scheduled and realtime in the past must be dropped")
		}
	})

	t.Run("scheduled_past_realtime_future_kept", func(t *testing.T) {
		db := makeTestDB()
		arrivalsEdgeRegisterService(db)
		arrivalsEdgeAddTrip(db, "GD_SPRF", "GD_SPRF", "1358", 8*3600+58*60, 10) // scheduled 08:58 (past)
		ls := withClock(NewLiveStore(), now)
		rt := time.Date(2023, 9, 15, 9, 4, 0, 0, time.UTC) // realtime 09:04 (future)
		arrivalsEdgeSetAbsDelay(ls, "GD_SPRF", 10, rt)

		arrivals := QueryArrivals(db, ls, "1358", now, 60, 0, nil)
		a := arrivalsEdgeFind(arrivals, "GD_SPRF")
		if a == nil {
			t.Fatal("schedule-past / realtime-future arrival must be kept (they disagree)")
		}
		if !a.RealtimeTime.Equal(rt) {
			t.Errorf("realtime: want %v, got %v", rt, a.RealtimeTime)
		}
	})

	t.Run("scheduled_future_realtime_past_kept", func(t *testing.T) {
		db := makeTestDB()
		arrivalsEdgeRegisterService(db)
		arrivalsEdgeAddTrip(db, "GD_SFRP", "GD_SFRP", "1358", 9*3600+5*60, 10) // scheduled 09:05 (future)
		ls := withClock(NewLiveStore(), now)
		rt := time.Date(2023, 9, 15, 8, 58, 0, 0, time.UTC) // realtime 08:58 (past)
		arrivalsEdgeSetAbsDelay(ls, "GD_SFRP", 10, rt)

		arrivals := QueryArrivals(db, ls, "1358", now, 60, 0, nil)
		a := arrivalsEdgeFind(arrivals, "GD_SFRP")
		if a == nil {
			t.Fatal("schedule-future / realtime-past arrival must be kept (they disagree)")
		}
		if !a.RealtimeTime.Equal(rt) {
			t.Errorf("realtime: want %v, got %v", rt, a.RealtimeTime)
		}
		if a.DelayMinutes >= 0 {
			t.Errorf("early (realtime-past) arrival should have negative delay, got %d", a.DelayMinutes)
		}
	})
}

// TestArrivalsEdge_PlatformPropagation verifies db.StopPlatforms is copied onto
// scheduled arrivals (present and absent cases).
func TestArrivalsEdge_PlatformPropagation(t *testing.T) {
	now := time.Date(2023, 9, 15, 9, 10, 0, 0, time.UTC)

	t.Run("present", func(t *testing.T) {
		db := makeTestDB()
		db.StopPlatforms = map[string]string{"1358": "7"}
		arrivals := QueryArrivals(db, withClock(NewLiveStore(), now), "1358", now, 60, 0, nil)

		a := arrivalsEdgeFind(arrivals, "68")
		if a == nil {
			t.Fatal("expected the route 68 scheduled arrival")
		}
		if a.Platform != "7" {
			t.Errorf("platform should propagate from StopPlatforms: want %q, got %q", "7", a.Platform)
		}
	})

	t.Run("absent", func(t *testing.T) {
		db := makeTestDB() // StopPlatforms is nil → reads yield ""
		arrivals := QueryArrivals(db, withClock(NewLiveStore(), now), "1358", now, 60, 0, nil)

		a := arrivalsEdgeFind(arrivals, "68")
		if a == nil {
			t.Fatal("expected the route 68 scheduled arrival")
		}
		if a.Platform != "" {
			t.Errorf("platform should be empty when StopPlatforms has no entry, got %q", a.Platform)
		}
	})
}

// TestArrivalsEdge_MinutesUntil checks Arrival.MinutesUntil: it uses the
// effective time (realtime over scheduled) and truncates toward zero.
func TestArrivalsEdge_MinutesUntil(t *testing.T) {
	base := time.Date(2023, 9, 15, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		arr  Arrival
		want int
	}{
		{"ten_minutes_scheduled", Arrival{ScheduledTime: base.Add(10 * time.Minute)}, 10},
		{"realtime_overrides_scheduled", Arrival{ScheduledTime: base.Add(5 * time.Minute), RealtimeTime: base.Add(2 * time.Minute)}, 2},
		{"truncates_down_90s", Arrival{ScheduledTime: base.Add(90 * time.Second)}, 1},
		{"exactly_now_is_zero", Arrival{ScheduledTime: base}, 0},
		{"past_truncates_toward_zero", Arrival{ScheduledTime: base.Add(-90 * time.Second)}, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.arr.MinutesUntil(base); got != c.want {
				t.Errorf("MinutesUntil: want %d, got %d", c.want, got)
			}
		})
	}
}
