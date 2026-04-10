package display_test

import (
	"image/png"
	"os"
	"testing"
	"time"

	"tfi-display/display"
	"tfi-display/gtfs"
)

func TestRenderPreview(t *testing.T) {
	now := time.Now()
	min := func(n int) time.Time { return now.Add(time.Duration(n) * time.Minute) }

	sections := []display.StopSection{
		{
			Label: "Vinny's",
			Arrivals: []gtfs.Arrival{
				{RouteShort: "4", Headsign: "Monkstown Avenue", ScheduledTime: min(1)},
				{RouteShort: "7A", Headsign: "Bride's Glen (via UCD)", ScheduledTime: min(3), RealtimeTime: min(5), DelayMinutes: 2},
				{RouteShort: "7", Headsign: "Belfield", ScheduledTime: min(8)},
				{RouteShort: "4", Headsign: "Monkstown Avenue", ScheduledTime: min(14), RealtimeTime: min(14)},
			},
		},
		{
			Label: "Sandymount",
			Arrivals: []gtfs.Arrival{
				{RouteShort: "S2", Headsign: "Sandymount Village (Circular Route via the very long road name)", ScheduledTime: min(2)},
				{RouteShort: "S2", Headsign: "Sandymount Village", ScheduledTime: min(22), RealtimeTime: min(22)},
			},
		},
		{
			Label: "Dart",
			Arrivals: []gtfs.Arrival{
				{RouteShort: "DART", Platform: "3", Headsign: "Greystones", ScheduledTime: min(0), RealtimeTime: min(0)},
				{RouteShort: "DART", Platform: "1", Headsign: "Malahide", ScheduledTime: min(12)},
			},
		},
	}

	img := display.Render(sections, now, now, 1024, 600)

	if err := os.MkdirAll("../mock_output", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create("../mock_output/preview.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	t.Log("preview written to mock_output/preview.png")
}
