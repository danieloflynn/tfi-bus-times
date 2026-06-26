package main

import (
	"testing"
	"time"
)

// mainLogicAt builds a deterministic time.Time at the given wall-clock hour and
// minute. isActiveTime only reads Hour()/Minute(), so the date and location are
// arbitrary as long as they are fixed; tests here never call time.Now().
func mainLogicAt(hour, minute int) time.Time {
	return time.Date(2026, time.June, 26, hour, minute, 0, 0, time.UTC)
}

// TestMainLogic_IsActiveTime table-tests the schedule window predicate, covering
// a normal daytime range, an overnight range that wraps midnight, the degenerate
// start==stop case, and the exact start/stop boundary instants. The window is
// half-open [start, stop): active at exactly start, inactive at exactly stop.
func TestMainLogic_IsActiveTime(t *testing.T) {
	tests := []struct {
		name  string
		now   time.Time
		start time.Time
		stop  time.Time
		want  bool
	}{
		// Normal daytime range [07:00, 23:00).
		{"normal/midday inside", mainLogicAt(12, 0), mainLogicAt(7, 0), mainLogicAt(23, 0), true},
		{"normal/just before start", mainLogicAt(6, 59), mainLogicAt(7, 0), mainLogicAt(23, 0), false},
		{"normal/exactly start is active", mainLogicAt(7, 0), mainLogicAt(7, 0), mainLogicAt(23, 0), true},
		{"normal/just after start", mainLogicAt(7, 1), mainLogicAt(7, 0), mainLogicAt(23, 0), true},
		{"normal/just before stop", mainLogicAt(22, 59), mainLogicAt(7, 0), mainLogicAt(23, 0), true},
		{"normal/exactly stop is inactive", mainLogicAt(23, 0), mainLogicAt(7, 0), mainLogicAt(23, 0), false},
		{"normal/after stop", mainLogicAt(23, 30), mainLogicAt(7, 0), mainLogicAt(23, 0), false},

		// Overnight range [22:00, 06:00) wrapping midnight.
		{"overnight/late evening inside", mainLogicAt(23, 0), mainLogicAt(22, 0), mainLogicAt(6, 0), true},
		{"overnight/after midnight inside", mainLogicAt(2, 0), mainLogicAt(22, 0), mainLogicAt(6, 0), true},
		{"overnight/midnight inside", mainLogicAt(0, 0), mainLogicAt(22, 0), mainLogicAt(6, 0), true},
		{"overnight/midday outside", mainLogicAt(12, 0), mainLogicAt(22, 0), mainLogicAt(6, 0), false},
		{"overnight/just before start outside", mainLogicAt(21, 59), mainLogicAt(22, 0), mainLogicAt(6, 0), false},
		{"overnight/exactly start is active", mainLogicAt(22, 0), mainLogicAt(22, 0), mainLogicAt(6, 0), true},
		{"overnight/just before stop", mainLogicAt(5, 59), mainLogicAt(22, 0), mainLogicAt(6, 0), true},
		{"overnight/exactly stop is inactive", mainLogicAt(6, 0), mainLogicAt(22, 0), mainLogicAt(6, 0), false},

		// Degenerate start == stop: treated as always active regardless of now.
		{"degenerate/now equals bound", mainLogicAt(9, 0), mainLogicAt(9, 0), mainLogicAt(9, 0), true},
		{"degenerate/now far from bound", mainLogicAt(3, 0), mainLogicAt(9, 0), mainLogicAt(9, 0), true},
		{"degenerate/midnight bounds", mainLogicAt(23, 59), mainLogicAt(0, 0), mainLogicAt(0, 0), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isActiveTime(tc.now, tc.start, tc.stop)
			if got != tc.want {
				t.Errorf("isActiveTime(now=%02d:%02d, start=%02d:%02d, stop=%02d:%02d) = %v, want %v",
					tc.now.Hour(), tc.now.Minute(),
					tc.start.Hour(), tc.start.Minute(),
					tc.stop.Hour(), tc.stop.Minute(),
					got, tc.want)
			}
		})
	}
}

// TestMainLogic_IsActiveTimeIgnoresSubMinuteAndDate locks the current behaviour
// that isActiveTime compares at minute granularity and ignores seconds, nanos,
// and the calendar date entirely (it only reads Hour()/Minute()).
func TestMainLogic_IsActiveTimeIgnoresSubMinuteAndDate(t *testing.T) {
	start := mainLogicAt(9, 0)
	stop := mainLogicAt(17, 0)

	// 17:00:59.999 on a far-future date is still minute 17:00, which equals stop;
	// the half-open window makes it inactive despite the sub-minute components.
	atStopWithSeconds := time.Date(2030, time.January, 2, 17, 0, 59, 999_000_000, time.UTC)
	if isActiveTime(atStopWithSeconds, start, stop) {
		t.Errorf("isActiveTime at exactly stop minute = true, want false (seconds/date must be ignored)")
	}

	// 16:59:59 on a past date is minute 16:59 < stop, so active.
	justBeforeStop := time.Date(1999, time.December, 31, 16, 59, 59, 0, time.UTC)
	if !isActiveTime(justBeforeStop, start, stop) {
		t.Errorf("isActiveTime one minute before stop = false, want true (seconds/date must be ignored)")
	}
}

// TestMainLogic_PageWindow table-tests the pure paging-window arithmetic lifted
// out of renderAndDisplay: single page, exact multiple, a short remainder last
// page, modulo wrap-around, the maxPages cap, and the pageSize<=0 / total==0
// guards that return the full range so the caller can slice unconditionally.
func TestMainLogic_PageWindow(t *testing.T) {
	tests := []struct {
		name      string
		total     int
		pageSize  int
		maxPages  int
		page      int
		wantStart int
		wantEnd   int
	}{
		// Single page: everything fits; any page index wraps back to it.
		{"single page", 3, 5, 0, 0, 0, 3},
		{"single page wraps to itself", 3, 5, 0, 1, 0, 3},

		// Exact multiple of pageSize: two full pages that cycle.
		{"exact multiple page0", 10, 5, 0, 0, 0, 5},
		{"exact multiple page1", 10, 5, 0, 1, 5, 10},
		{"exact multiple wraps to page0", 10, 5, 0, 2, 0, 5},

		// Remainder on the last page (7 items, 3 per page -> 3,3,1).
		{"remainder first page", 7, 3, 0, 0, 0, 3},
		{"remainder middle page", 7, 3, 0, 1, 3, 6},
		{"remainder short last page", 7, 3, 0, 2, 6, 7},
		{"remainder wraps to first", 7, 3, 0, 3, 0, 3},

		// Page index larger than the page count wraps via modulo.
		{"modulo wrap to middle", 10, 5, 0, 3, 5, 10},
		{"modulo wrap to first", 10, 5, 0, 4, 0, 5},

		// maxPages cap: 20 items / 5 = 4 natural pages capped to 2, so only the
		// first two windows ever display and they cycle.
		{"maxpages cap page0", 20, 5, 2, 0, 0, 5},
		{"maxpages cap page1", 20, 5, 2, 1, 5, 10},
		{"maxpages cap wraps to page0", 20, 5, 2, 2, 0, 5},
		{"maxpages cap wraps to page1", 20, 5, 2, 3, 5, 10},
		// The cap also hides a trailing remainder page (index 6 never shows).
		{"maxpages cap hides remainder", 7, 3, 2, 2, 0, 3},
		// maxPages above the natural page count does not cap.
		{"maxpages above natural no cap", 6, 3, 5, 1, 3, 6},

		// pageSize <= 0 returns the full range unchanged.
		{"pagesize zero full range", 10, 0, 0, 0, 0, 10},
		{"pagesize negative full range", 10, -1, 0, 2, 0, 10},

		// total == 0 returns the full (empty) range regardless of other args.
		{"total zero", 0, 5, 0, 0, 0, 0},
		{"total zero pagesize zero", 0, 0, 0, 3, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotStart, gotEnd := pageWindow(tc.total, tc.pageSize, tc.maxPages, tc.page)
			if gotStart != tc.wantStart || gotEnd != tc.wantEnd {
				t.Errorf("pageWindow(total=%d, pageSize=%d, maxPages=%d, page=%d) = (%d, %d), want (%d, %d)",
					tc.total, tc.pageSize, tc.maxPages, tc.page, gotStart, gotEnd, tc.wantStart, tc.wantEnd)
			}
			// The returned bounds must always be a valid, in-range slice window.
			if gotStart < 0 || gotEnd < gotStart || gotEnd > tc.total && tc.total >= 0 {
				t.Errorf("pageWindow(%d, %d, %d, %d) returned out-of-range bounds (%d, %d)",
					tc.total, tc.pageSize, tc.maxPages, tc.page, gotStart, gotEnd)
			}
		})
	}
}
