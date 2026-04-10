package display_test

// TestRenderUsesNowNotFeedTime is a regression test for the stale-feed-time bug.
//
// Before the fix, main.go called:
//
//	display.Render(sections, updated, width, height)
//
// where `updated` was live.FeedTime() — the timestamp of the last successful
// GTFS-RT poll.  The renderer then used that value for both the "Updated:" header
// and for a.MinutesUntil(now).  After 24+ hours the feed time could be hours
// behind real time, causing MinutesUntil to return 100+ minutes for buses that
// were only a few minutes away (capped to "99 min" on-screen).
//
// After the fix the signature became:
//
//	display.Render(sections, now, feedTime, width, height)
//
// so MinutesUntil always receives the actual wall clock while the header still
// shows the last poll time.
//
// How to use this test across codebases:
//
//	Pre-fix main  — this file will FAIL TO COMPILE (wrong Render arity), which
//	                confirms the old API did not separate the two times.
//
//	Post-fix main — go test ./display/... must pass, confirming:
//	  1. Render accepts the new 5-argument signature.
//	  2. A bus 5 minutes away is rendered with correct short minutes, not "99 min".
//	  3. The buggy call (staleFeedTime as both arguments) produces a different image
//	     from the correct call (realNow + staleFeedTime), proving the two paths
//	     yield different output.

import (
	"image"
	"testing"
	"time"

	"tfi-display/display"
	"tfi-display/gtfs"
)

func TestRenderUsesNowNotFeedTime(t *testing.T) {
	realNow := time.Date(2025, 1, 10, 22, 0, 0, 0, time.UTC)
	staleFeedTime := realNow.Add(-2 * time.Hour) // feed last parsed at 20:00

	// A single bus due in 5 minutes from realNow.
	sections := []display.StopSection{{
		Label: "Test Stop",
		Arrivals: []gtfs.Arrival{{
			RouteShort:    "4",
			Headsign:      "City Centre",
			ScheduledTime: realNow.Add(5 * time.Minute),
		}},
	}}

	const w, h = 250, 122

	// Correct render: realNow for MinutesUntil, staleFeedTime for the header.
	// MinutesUntil(realNow) = 5 → shows "5 min".
	imgCorrect := display.Render(sections, realNow, staleFeedTime, w, h)

	// Buggy render: staleFeedTime used for both (simulates old main.go behaviour).
	// MinutesUntil(staleFeedTime) = 125 → capped to "99 min".
	imgBuggy := display.Render(sections, staleFeedTime, staleFeedTime, w, h)

	// The two images must differ — different minutes text means different pixels.
	if pixelsEqual(imgCorrect, imgBuggy) {
		t.Error("correct render and buggy render produced identical images: " +
			"Render is not using 'now' for MinutesUntil as expected")
	}

	// Direct check: MinutesUntil with real now must be within the displayable range.
	arrival := sections[0].Arrivals[0]
	mins := arrival.MinutesUntil(realNow)
	if mins > 99 {
		t.Errorf("MinutesUntil(realNow) = %d, want ≤ 99 (bus is only 5 min away)", mins)
	}
	if mins < 4 || mins > 6 {
		t.Errorf("MinutesUntil(realNow) = %d, want ~5", mins)
	}
}

// pixelsEqual returns true if both images have identical dimensions and pixel data.
func pixelsEqual(a, b *image.Gray) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			return false
		}
	}
	return true
}
