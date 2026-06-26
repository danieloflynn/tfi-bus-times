// Package display_test (external test package) — FR7 display golden/snapshot tests.
//
// Covers RowsPerSection, the small-display Render path, renderHD edge cases, and a
// pixel-hash golden snapshot for an HD frame. All time values are pinned to fixed
// instants via time.Date — never time.Now().
//
// Helper function/variable names are prefixed "displayRender" to avoid collisions
// with other test files in this package (renderer_preview_test.go, version_test.go).
package display_test

import (
	"crypto/sha256"
	"fmt"
	"image"
	"testing"
	"time"

	"tfi-display/display"
	"tfi-display/gtfs"
)

// ── Fixed instants ────────────────────────────────────────────────────────────

// displayRenderNow is the deterministic reference instant for all render tests.
// It is the canonical fixture time (2026-06-26 18:06:25 UTC = 19:06:25 Dublin BST)
// expressed in UTC so it compiles without requiring the tzdata database.
var displayRenderNow = time.Date(2026, 6, 26, 18, 6, 25, 0, time.UTC)

// displayRenderFeed is the feed-update timestamp injected into render headers.
var displayRenderFeed = time.Date(2026, 6, 26, 18, 0, 0, 0, time.UTC)

// ── Pixel-inspection helpers ──────────────────────────────────────────────────

// displayRenderHasBlackInRect reports whether any pixel in [x0,x1) × [y0,y1)
// has gray value 0 (black).
func displayRenderHasBlackInRect(img *image.Gray, x0, y0, x1, y1 int) bool {
	b := img.Bounds()
	for y := y0; y < y1 && y < b.Max.Y; y++ {
		for x := x0; x < x1 && x < b.Max.X; x++ {
			if img.GrayAt(x, y).Y == 0 {
				return true
			}
		}
	}
	return false
}

// displayRenderHasNonBlackInRect reports whether any pixel in [x0,x1) × [y0,y1)
// has gray value other than 0 (i.e. is not background-black).
func displayRenderHasNonBlackInRect(img *image.Gray, x0, y0, x1, y1 int) bool {
	b := img.Bounds()
	for y := y0; y < y1 && y < b.Max.Y; y++ {
		for x := x0; x < x1 && x < b.Max.X; x++ {
			if img.GrayAt(x, y).Y != 0 {
				return true
			}
		}
	}
	return false
}

// displayRenderCountNonBlack counts pixels in [x0,x1) × [y0,y1) with gray ≠ 0.
func displayRenderCountNonBlack(img *image.Gray, x0, y0, x1, y1 int) int {
	b := img.Bounds()
	n := 0
	for y := y0; y < y1 && y < b.Max.Y; y++ {
		for x := x0; x < x1 && x < b.Max.X; x++ {
			if img.GrayAt(x, y).Y != 0 {
				n++
			}
		}
	}
	return n
}

// ── Golden-snapshot helpers ───────────────────────────────────────────────────

// displayRenderGoldenSections returns the deterministic section set used by the
// HD golden snapshot test. It covers DART (route-box skip), a scheduled-only
// arrival (Sched badge), a delayed arrival, and an empty section.
func displayRenderGoldenSections(now time.Time) []display.StopSection {
	return []display.StopSection{
		{
			Label: "Vinny's",
			Arrivals: []gtfs.Arrival{
				{
					RouteShort:    "4",
					Headsign:      "Monkstown Avenue",
					ScheduledTime: now.Add(3 * time.Minute),
				},
				{
					RouteShort:    "7A",
					Headsign:      "Bride's Glen (via UCD)",
					ScheduledTime: now.Add(5 * time.Minute),
					RealtimeTime:  now.Add(7 * time.Minute),
					DelayMinutes:  2,
				},
			},
		},
		{
			Label: "Sandymount",
			Arrivals: []gtfs.Arrival{
				{
					RouteShort:    "S2",
					Headsign:      "Sandymount Village",
					ScheduledTime: now.Add(8 * time.Minute),
				},
			},
		},
		{
			Label:    "DART",
			Arrivals: nil, // empty → "No departures"
		},
	}
}

// ── RowsPerSection table test ─────────────────────────────────────────────────

// TestDisplayRender_RowsPerSection table-tests RowsPerSection across the three
// behaviours the PRD requires: small-display short-circuit (maxRows), HD division
// across sections, and the ≥1 clamp.
//
// Reference constants used in the expected values:
//   hdMinWidth=800  maxRows=4  hdHeaderHeight=30  hdSectionBarHeight=36
//   hdRowHeight=40  hdSectionSeparator=4
func TestDisplayRender_RowsPerSection(t *testing.T) {
	cases := []struct {
		name        string
		numSections int
		width       int
		height      int
		want        int
	}{
		{
			// Small display always returns maxRows regardless of other args.
			name: "small display returns maxRows",
			numSections: 1, width: 250, height: 122,
			want: 4,
		},
		{
			// Still small because width < hdMinWidth, even with large height.
			name: "small display with large height still returns maxRows",
			numSections: 3, width: 799, height: 800,
			want: 4,
		},
		{
			// HD single section: availHeight=568, heightPerSection=568,
			// rows = (568-36)/40 = 13.
			name: "HD single section divides full height",
			numSections: 1, width: 1024, height: 600,
			want: 13,
		},
		{
			// HD three sections: availHeight=568, totalSep=8,
			// heightPerSection=186, rows = (186-36)/40 = 3.
			name: "HD three sections divides height evenly",
			numSections: 3, width: 1024, height: 600,
			want: 3,
		},
		{
			// numSections=0 is clamped to 1 before dividing, same as single section.
			name: "HD zero sections treated as one",
			numSections: 0, width: 1024, height: 600,
			want: 13,
		},
		{
			// Height too small: availHeight=38, heightPerSection=38,
			// rows = (38-36)/40 = 0 → clamped to 1.
			name: "HD clamps rows to at least 1",
			numSections: 1, width: 1024, height: 70,
			want: 1,
		},
		{
			// Exact 2.9" display at HD boundary.
			name: "HD wide display 1872x1404 single section",
			numSections: 1, width: 1872, height: 1404,
			// availHeight=1372, heightPerSection=1372, rows=(1372-36)/40=33
			want: 33,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := display.RowsPerSection(tc.numSections, tc.width, tc.height)
			if got != tc.want {
				t.Errorf("RowsPerSection(%d, %d, %d) = %d; want %d",
					tc.numSections, tc.width, tc.height, got, tc.want)
			}
		})
	}
}

// ── Small-display Render tests ────────────────────────────────────────────────

// TestDisplayRender_SmallDimensions verifies that Render returns an image of
// exactly the requested pixel dimensions for each small-display model.
func TestDisplayRender_SmallDimensions(t *testing.T) {
	cases := []struct{ w, h int }{
		{250, 122}, // 2.13"
		{296, 128}, // 2.9"
	}
	for _, tc := range cases {
		img := display.Render(nil, displayRenderNow, displayRenderFeed, tc.w, tc.h)
		b := img.Bounds()
		if b.Dx() != tc.w || b.Dy() != tc.h {
			t.Errorf("Render(%d,%d): got bounds %v; want %dx%d",
				tc.w, tc.h, b, tc.w, tc.h)
		}
	}
}

// TestDisplayRender_SmallNoDepartures asserts the empty-arrivals path on the
// small display: "No departures" text is drawn near the vertical centre, and no
// route box is drawn (which would incorrectly fill x=[0..30), y=[14..38] black).
func TestDisplayRender_SmallNoDepartures(t *testing.T) {
	img := display.Render(nil, displayRenderNow, displayRenderFeed, 250, 122)

	// "No departures" baseline ≈ height/2 - 6 = 55; with basicfont ascent 10,
	// text occupies y=[45..58]. Check a band well within that region.
	if !displayRenderHasBlackInRect(img, 30, 40, 220, 90) {
		t.Error("expected 'No departures' text black pixels in centre band (y=40..90)")
	}

	// With no arrivals, the route box area must NOT be filled black.
	// fillRect(0, 14, 30, 39, black) is only called for actual arrivals.
	if displayRenderHasBlackInRect(img, 0, 14, 30, 38) {
		t.Error("route box area should be white (background) when there are no arrivals")
	}
}

// TestDisplayRender_SmallRouteBox verifies that when an arrival is present, the
// first row's route box (x=[0..30), y=[14..39)) is filled solid black by fillRect.
func TestDisplayRender_SmallRouteBox(t *testing.T) {
	sections := []display.StopSection{{
		Label: "Test",
		Arrivals: []gtfs.Arrival{{
			RouteShort:    "4",
			Headsign:      "Somewhere",
			ScheduledTime: displayRenderNow.Add(5 * time.Minute),
		}},
	}}
	img := display.Render(sections, displayRenderNow, displayRenderFeed, 250, 122)

	// fillRect(img, 0, 14, 30, 39, black) fills x=[0..29], y=[14..38] inclusive.
	// Spot-check three pixels solidly inside that rectangle.
	for _, pt := range [][2]int{{5, 20}, {14, 25}, {24, 32}} {
		x, y := pt[0], pt[1]
		if img.GrayAt(x, y).Y != 0 {
			t.Errorf("expected black pixel at route box (%d,%d), got gray=%d",
				x, y, img.GrayAt(x, y).Y)
		}
	}
}

// TestDisplayRender_SmallDelayFormatting verifies that delay text is drawn in the
// delay zone (x=[223..250)) when RealtimeTime is set and DelayMinutes ≠ 0, and is
// absent otherwise.
func TestDisplayRender_SmallDelayFormatting(t *testing.T) {
	now := displayRenderNow
	cases := []struct {
		name         string
		delay        int
		hasRealtime  bool
		wantDelayPix bool
	}{
		{"positive delay +2 drawn", 2, true, true},
		{"negative delay -3 drawn", -3, true, true},
		{"zero delay not drawn", 0, true, false},
		{"no realtime, delay ignored", 2, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			arr := gtfs.Arrival{
				RouteShort:    "7A",
				Headsign:      "Test",
				ScheduledTime: now.Add(5 * time.Minute),
				DelayMinutes:  tc.delay,
			}
			if tc.hasRealtime {
				arr.RealtimeTime = now.Add(time.Duration(5+tc.delay) * time.Minute)
			}
			sections := []display.StopSection{{Label: "T", Arrivals: []gtfs.Arrival{arr}}}
			img := display.Render(sections, now, displayRenderFeed, 250, 122)

			// 2.13" delay zone: delayStart213=223, delayEnd213=249.
			// First row body: y=[14..38]. Black pixels here = delay text drawn.
			hasBlack := displayRenderHasBlackInRect(img, 223, 14, 250, 39)
			if hasBlack != tc.wantDelayPix {
				t.Errorf("delay zone black pixels = %v; want %v (delay=%d realtime=%v)",
					hasBlack, tc.wantDelayPix, tc.delay, tc.hasRealtime)
			}
		})
	}
}

// TestDisplayRender_SmallPlatformPrefix verifies that when an arrival has a
// Platform set, the route box label becomes "P<platform>" (e.g. "P3") and the
// route box is still drawn (filled black).
//
// padRoute("P3") → " P3 " so the rendered glyphs occupy x≈[9..23), baseline≈32.
// Text top is approximately y=21 (baseline 32 − ascent 11). Points chosen here
// are above the text (y=15) so they are guaranteed to be the fillRect black fill,
// not overwritten by a white glyph.
func TestDisplayRender_SmallPlatformPrefix(t *testing.T) {
	now := displayRenderNow
	sections := []display.StopSection{{
		Label: "Test",
		Arrivals: []gtfs.Arrival{{
			RouteShort:    "DART",
			Platform:      "3",
			Headsign:      "Greystones",
			ScheduledTime: now.Add(5 * time.Minute),
		}},
	}}
	img := display.Render(sections, now, displayRenderFeed, 250, 122)

	// Check pixels in the route box (x=[0..29], y=[14..38]) that are above the
	// text baseline so they are not overwritten by white glyph pixels.
	// Text baseline ≈ 32, text top ≈ 21 (ascent≈11): y=15 is reliably below
	// the fillRect top (y=14) and above any text pixel.
	for _, pt := range [][2]int{{5, 15}, {14, 15}, {24, 15}} {
		x, y := pt[0], pt[1]
		if img.GrayAt(x, y).Y != 0 {
			t.Errorf("route box pixel (%d,%d) should be black (fillRect) with platform prefix, got gray=%d",
				x, y, img.GrayAt(x, y).Y)
		}
	}
}

// TestDisplayRender_Small213vs29Zones asserts that the column zones shift
// correctly between the 2.13" and 2.9" layout modes. With a delayed arrival the
// delay text must appear in x=[223..250) on a 250-wide display and in
// x=[274..296) on a 296-wide display.
func TestDisplayRender_Small213vs29Zones(t *testing.T) {
	now := displayRenderNow
	arr := gtfs.Arrival{
		RouteShort:    "7",
		Headsign:      "Belfield",
		ScheduledTime: now.Add(5 * time.Minute),
		RealtimeTime:  now.Add(7 * time.Minute),
		DelayMinutes:  2,
	}
	sections := []display.StopSection{{Label: "T", Arrivals: []gtfs.Arrival{arr}}}

	img213 := display.Render(sections, now, displayRenderFeed, 250, 122)
	img29 := display.Render(sections, now, displayRenderFeed, 296, 128)

	// Verify image dimensions match the requested size.
	if b := img213.Bounds(); b.Dx() != 250 || b.Dy() != 122 {
		t.Errorf("2.13\" image: got %v; want 250×122", b)
	}
	if b := img29.Bounds(); b.Dx() != 296 || b.Dy() != 128 {
		t.Errorf("2.9\" image: got %v; want 296×128", b)
	}

	// 2.13" layout: delayStart213=223; delay text drawn from x=223.
	if !displayRenderHasBlackInRect(img213, 223, 14, 250, 39) {
		t.Error("2.13\" layout: expected delay text in x=[223..250), y=[14..39)")
	}

	// 2.9" layout: dStart = headsignEnd29(230) + 2 + 40 + 2 = 274.
	// The delay "+2" starts at x=274 on a 296-wide display.
	if !displayRenderHasBlackInRect(img29, 274, 14, 296, 40) {
		t.Error("2.9\" layout: expected delay text in x=[274..296), y=[14..40)")
	}
}

// ── HD Render tests ───────────────────────────────────────────────────────────

// TestDisplayRender_HDDimensions verifies that renderHD (triggered when
// width >= hdMinWidth=800) returns an image of exactly the requested size.
func TestDisplayRender_HDDimensions(t *testing.T) {
	sections := []display.StopSection{{
		Label: "A",
		Arrivals: []gtfs.Arrival{{
			RouteShort:    "4",
			Headsign:      "Somewhere",
			ScheduledTime: displayRenderNow.Add(5 * time.Minute),
		}},
	}}
	img := display.Render(sections, displayRenderNow, displayRenderFeed, 1024, 600)
	b := img.Bounds()
	if b.Dx() != 1024 || b.Dy() != 600 {
		t.Errorf("HD Render: got %v; want 1024×600", b)
	}
}

// TestDisplayRender_HDNoDepartures asserts that an empty section in HD mode
// draws "No departures" text (white on black background) in the section body.
// With two empty sections the text must appear in both.
func TestDisplayRender_HDNoDepartures(t *testing.T) {
	sections := []display.StopSection{
		{Label: "Stop A", Arrivals: nil},
		{Label: "Stop B", Arrivals: nil},
	}
	img := display.Render(sections, displayRenderNow, displayRenderFeed, 1024, 600)

	// Layout for 2 sections (1024×600):
	//   availHeight=568, totalSep=4, heightPerSection=282
	//   Section A bar: y=32..68; body: y=68..314; bodyMid≈209
	//   Section B bar: y=315..351; body: y=351..597; bodyMid≈492
	//
	// Text is white (non-black) on the black background. Check a horizontal
	// band that spans both section bodies.
	if !displayRenderHasNonBlackInRect(img, 100, 100, 900, 280) {
		t.Error("section A: expected 'No departures' white text in section body (y=[100..280))")
	}
	if !displayRenderHasNonBlackInRect(img, 100, 450, 900, 560) {
		t.Error("section B: expected 'No departures' white text in section body (y=[450..560))")
	}
}

// TestDisplayRender_HDDartRouteBoxSkipped verifies that arrivals with
// RouteShort=="DART" do not draw a route box or route number text in the route
// box area. Non-DART arrivals must draw white route text there.
//
// For a 1024-wide display: sc(hdBaseRouteBoxEnd=110) ≈ 60.
// First arrival row: y=68..108 (below the section bar at y=32..68).
func TestDisplayRender_HDDartRouteBoxSkipped(t *testing.T) {
	now := displayRenderNow

	dartSections := []display.StopSection{{
		Label: "DART",
		Arrivals: []gtfs.Arrival{{
			RouteShort:    "DART",
			Platform:      "1",
			Headsign:      "Malahide",
			ScheduledTime: now.Add(5 * time.Minute),
			RealtimeTime:  now.Add(5 * time.Minute),
		}},
	}}
	nonDartSections := []display.StopSection{{
		Label: "Bus",
		Arrivals: []gtfs.Arrival{{
			RouteShort:    "4",
			Headsign:      "Monkstown Avenue",
			ScheduledTime: now.Add(5 * time.Minute),
			RealtimeTime:  now.Add(5 * time.Minute),
		}},
	}}

	dartImg := display.Render(dartSections, now, displayRenderFeed, 1024, 600)
	nonDartImg := display.Render(nonDartSections, now, displayRenderFeed, 1024, 600)

	// Route box column zone for 1024 px: x=[0..60).
	// First arrival row: y=[68..108) (rowY=68, hdRowHeight=40).
	const routeBoxX1 = 60
	const rowY0, rowY1 = 68, 108

	// Non-DART: white route number text must appear in the route box area.
	if !displayRenderHasNonBlackInRect(nonDartImg, 0, rowY0, routeBoxX1, rowY1) {
		t.Error("non-DART: expected white route text in route box area x=[0..60), y=[68..108)")
	}

	// DART: no text drawn → route box area stays at background colour (black).
	if displayRenderHasNonBlackInRect(dartImg, 0, rowY0, routeBoxX1, rowY1) {
		t.Error("DART: expected no white pixels in route box area (route box should be skipped)")
	}
}

// TestDisplayRender_HDSchedBadge verifies that a scheduled-only arrival (zero
// RealtimeTime) gets a "(Sched)" badge, which adds extra white pixels to the right
// side of its row compared to an equivalent realtime arrival.
func TestDisplayRender_HDSchedBadge(t *testing.T) {
	now := displayRenderNow
	sections := []display.StopSection{{
		Label: "Test",
		Arrivals: []gtfs.Arrival{
			// Row 0: scheduled only — should get "(Sched)" badge.
			{
				RouteShort:    "4",
				Headsign:      "A",
				ScheduledTime: now.Add(3 * time.Minute),
				// RealtimeTime zero → badge drawn
			},
			// Row 1: has realtime data — no "(Sched)" badge.
			{
				RouteShort:    "7",
				Headsign:      "B",
				ScheduledTime: now.Add(5 * time.Minute),
				RealtimeTime:  now.Add(5 * time.Minute),
			},
		},
	}}
	img := display.Render(sections, now, displayRenderFeed, 1024, 600)

	// Row 0: y=[68..108), Row 1: y=[108..148).
	// Check the right portion of each row (x=[600..1024)) for white pixels;
	// the "(Sched)" badge in row 0 adds extra text pixels there.
	row0Whites := displayRenderCountNonBlack(img, 600, 68, 1024, 108)
	row1Whites := displayRenderCountNonBlack(img, 600, 108, 1024, 148)

	if row0Whites <= row1Whites {
		t.Errorf("(Sched) badge: scheduled-only row0 should have more right-side non-black pixels than realtime row1; got row0=%d row1=%d", row0Whites, row1Whites)
	}
}

// TestDisplayRender_HDMinutesFormatting verifies the three minute-label branches
// in renderHD: "Due" (< 1 min), "N min", and "99 min" (> 99 min). The test uses
// the effective arrival time directly (passes arrivals to Render, not through
// QueryArrivals) so the "already departed" guard is not relevant.
func TestDisplayRender_HDMinutesFormatting(t *testing.T) {
	now := displayRenderNow
	cases := []struct {
		name         string
		offset       time.Duration
		wantMinPixel bool // expect non-black pixels near right edge of row
	}{
		{"due (< 1 min)", 30 * time.Second, true},
		{"5 min", 5 * time.Minute, true},
		{"over 99 min shows 99 min", 100 * time.Minute, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			arr := gtfs.Arrival{
				RouteShort:    "4",
				Headsign:      "Somewhere",
				ScheduledTime: now.Add(tc.offset),
				RealtimeTime:  now.Add(tc.offset),
			}
			sections := []display.StopSection{{Label: "T", Arrivals: []gtfs.Arrival{arr}}}
			img := display.Render(sections, now, displayRenderFeed, 1024, 600)

			b := img.Bounds()
			if b.Dx() != 1024 || b.Dy() != 600 {
				t.Fatalf("unexpected image size %v", b)
			}

			// Minutes text is white, right-aligned near x=1023 (sc(1870)).
			// Check x=[900..1024) in the first arrival row y=[68..108).
			if !displayRenderHasNonBlackInRect(img, 900, 68, 1023, 108) {
				t.Errorf("case %q: expected minutes text white pixels near right edge x=[900..1023)", tc.name)
			}
		})
	}
}

// TestDisplayRender_HDVersionLabel confirms that the Version string is rendered
// as white text in the top-left header area (x=[0..120), y=[0..30)).
// This mirrors version_test.go but uses a unique version string and also verifies
// the blank-version case from a single function call for completeness.
func TestDisplayRender_HDVersionLabel(t *testing.T) {
	orig := display.Version
	t.Cleanup(func() { display.Version = orig })

	display.Version = "v-render-edge"
	imgWith := display.Render(nil, displayRenderNow, displayRenderFeed, 1024, 600)

	if !displayRenderHasNonBlackInRect(imgWith, 0, 0, 120, 30) {
		t.Error("version label: expected white text pixels in top-left header when Version is set")
	}

	display.Version = ""
	imgWithout := display.Render(nil, displayRenderNow, displayRenderFeed, 1024, 600)

	if displayRenderHasNonBlackInRect(imgWithout, 0, 0, 120, 30) {
		t.Error("version label: expected no pixels in top-left header when Version is empty")
	}
}

// ── HD golden snapshot ────────────────────────────────────────────────────────

// displayRenderHDGoldenHash is the SHA-256 of img.Pix for the canonical 1024×600
// HD frame defined by displayRenderGoldenSections with Version="v-golden".
//
// To update this constant intentionally (e.g. after a legitimate layout change):
//
//  1. Delete the hash value (set to "").
//  2. Run: go test -run TestDisplayRender_HDGoldenSnapshot -v ./display/
//  3. Copy the "actual hash" from the log output into this constant.
//  4. Re-run to confirm the test passes.
const displayRenderHDGoldenHash = "66ee5fecc8c46d039b8667cb911bb01f446c03d39f806fa7d23cdfcdcea4661f"

// TestDisplayRender_HDGoldenSnapshot renders a full 1024×600 HD frame with a
// deterministic input set and asserts that the SHA-256 of img.Pix matches the
// recorded constant. Any unintentional pixel-level change (layout, font, column
// widths) will fail this test.
func TestDisplayRender_HDGoldenSnapshot(t *testing.T) {
	orig := display.Version
	display.Version = "v-golden"
	t.Cleanup(func() { display.Version = orig })

	now := displayRenderNow
	sections := displayRenderGoldenSections(now)
	img := display.Render(sections, now, displayRenderFeed, 1024, 600)

	sum := sha256.Sum256(img.Pix)
	got := fmt.Sprintf("%x", sum)

	if displayRenderHDGoldenHash == "" {
		// First run: record the hash so the maintainer can paste it in.
		t.Logf("golden hash not yet recorded")
		t.Logf("actual hash: %s", got)
		t.Log("Set displayRenderHDGoldenHash to the value above, then re-run.")
		// Do not fail: this run establishes the baseline.
		return
	}

	if got != displayRenderHDGoldenHash {
		t.Errorf(
			"HD golden snapshot hash mismatch — a pixel-level change was detected.\n"+
				"  got:  %s\n"+
				"  want: %s\n\n"+
				"If this change is INTENTIONAL (layout, font, column widths), update "+
				"displayRenderHDGoldenHash in display/render_edge_test.go:\n"+
				"  1. Set displayRenderHDGoldenHash = \"\"\n"+
				"  2. go test -run TestDisplayRender_HDGoldenSnapshot -v ./display/\n"+
				"  3. Copy the logged 'actual hash' into the constant.",
			got, displayRenderHDGoldenHash,
		)
	}
}
