package display

import (
	"testing"
	"time"
)

func TestIsStale(t *testing.T) {
	orig := StaleAfter
	defer func() { StaleAfter = orig }()
	StaleAfter = 10 * time.Minute

	now := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)

	tests := []struct {
		name     string
		feedTime time.Time
		want     bool
	}{
		{"just polled", now.Add(-30 * time.Second), false},
		{"a few missed polls, still within tolerance", now.Add(-5 * time.Minute), false},
		{"exactly at the threshold is not stale", now.Add(-10 * time.Minute), false},
		{"past the threshold", now.Add(-11 * time.Minute), true},
		{"frozen for hours — the reported symptom", now.Add(-9 * time.Hour), true},
		// renderAndDisplay substitutes now for a zero poll time at startup, so a
		// zero value here means "no poll yet" and must not be flagged.
		{"no poll yet", time.Time{}, false},
		// A clock stepped backwards by NTP can put the feed time ahead of now.
		{"feed time in the future", now.Add(time.Minute), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStale(now, tc.feedTime); got != tc.want {
				t.Errorf("isStale(now, %v) = %v; want %v", tc.feedTime, got, tc.want)
			}
		})
	}
}

func TestIsStaleDisabled(t *testing.T) {
	orig := StaleAfter
	defer func() { StaleAfter = orig }()

	now := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)
	ancient := now.Add(-72 * time.Hour)

	for _, d := range []time.Duration{0, -time.Minute} {
		StaleAfter = d
		if isStale(now, ancient) {
			t.Errorf("StaleAfter = %s should disable the marker, but isStale returned true", d)
		}
	}
}

func TestStaleAfterDefaultToleratesMissedPolls(t *testing.T) {
	// The default must never flag a board over a single missed 60s poll — a
	// false STALE is worse than no marker, because it trains you to ignore it.
	if StaleAfter < 5*time.Minute {
		t.Errorf("default StaleAfter = %s; too tight for a 60s poll interval", StaleAfter)
	}
}

// headerInk counts lit pixels in the HD header band — a proxy for "how much
// text is drawn up there" without pinning exact glyph positions.
func headerInk(t *testing.T, feedTime, now time.Time) int {
	t.Helper()
	const w, h = 1024, 600
	img := Render(nil, now, feedTime, w, h)
	n := 0
	for y := 0; y < hdHeaderHeight; y++ {
		for x := 0; x < w; x++ {
			if img.GrayAt(x, y).Y > 0 {
				n++
			}
		}
	}
	return n
}

// TestStaleMarkerIsDrawn checks the marker reaches the frame, not just the
// predicate: a stale board must look different from a healthy one.
func TestStaleMarkerIsDrawn(t *testing.T) {
	orig := StaleAfter
	defer func() { StaleAfter = orig }()
	StaleAfter = 10 * time.Minute

	now := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)

	// Same rendered timestamp in both cases (09:20:00 vs a stale 09:20:00 the
	// previous day), so any pixel difference is the marker, not the digits.
	fresh := headerInk(t, now.Add(-1*time.Minute), now)
	stale := headerInk(t, now.Add(-9*time.Hour), now)

	if stale <= fresh {
		t.Errorf("stale header ink = %d, fresh = %d; want the stale header to draw more (the marker is missing)",
			stale, fresh)
	}
}
