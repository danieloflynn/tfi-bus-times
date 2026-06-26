package display_test

import (
	"testing"
	"time"

	"tfi-display/display"
	"tfi-display/gtfs"
)

// BenchmarkFuzzBench_RenderHD measures the HD renderer at 1024×600 with three
// labelled sections and nine arrivals in total (a representative board layout).
// Time is pinned to a fixed instant — never time.Now().
func BenchmarkFuzzBench_RenderHD(b *testing.B) {
	// Canonical fixture instant in Europe/Dublin (UTC+1).
	loc, err := time.LoadLocation("Europe/Dublin")
	if err != nil {
		// Falls back to a fixed-offset zone if the zone DB is unavailable.
		loc = time.FixedZone("IST", 3600)
	}
	now := time.Unix(1782497185, 0).In(loc) // 2026-06-26 19:06:25+01
	feedTime := now

	min := func(n int) time.Time { return now.Add(time.Duration(n) * time.Minute) }

	sections := []display.StopSection{
		{
			Label: "Vinny's",
			Arrivals: []gtfs.Arrival{
				{RouteShort: "4", Headsign: "Heuston Station", ScheduledTime: min(1), RealtimeTime: min(2), DelayMinutes: 1},
				{RouteShort: "7A", Headsign: "Mountjoy Square", ScheduledTime: min(8), RealtimeTime: min(13), DelayMinutes: 5},
				{RouteShort: "4", Headsign: "Heuston Station", ScheduledTime: min(11)},
			},
		},
		{
			Label: "Sandymount",
			Arrivals: []gtfs.Arrival{
				{RouteShort: "S2", Headsign: "Heuston Station", ScheduledTime: min(2), RealtimeTime: min(2)},
				{RouteShort: "S2", Headsign: "Heuston Station", ScheduledTime: min(17)},
			},
		},
		{
			Label: "DART",
			Arrivals: []gtfs.Arrival{
				{RouteShort: "DART", Platform: "1", Headsign: "Greystones", ScheduledTime: min(0), RealtimeTime: min(6), DelayMinutes: 6},
				{RouteShort: "DART", Platform: "2", Headsign: "Howth", ScheduledTime: min(20), RealtimeTime: min(24), DelayMinutes: 4},
				{RouteShort: "DART", Platform: "1", Headsign: "Bray (Daly)", ScheduledTime: min(35)},
			},
		},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = display.Render(sections, now, feedTime, 1024, 600)
	}
}
