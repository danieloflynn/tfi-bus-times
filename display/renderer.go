// Package display renders bus arrival data onto a grayscale image suitable
// for e-ink display. It uses the basicfont 7×13 bitmap font from
// golang.org/x/image/font/basicfont for all text.
package display

import (
	"fmt"
	"image"
	"image/color"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	"tfi-display/gtfs"
)

// StopSection groups arrivals for a single stop with its display label.
type StopSection struct {
	Label    string
	Arrivals []gtfs.Arrival
}

// Layout constants for a 250 × 122 landscape display (2.13").
// The 2.9" display (296 × 128) shares the same row heights but wider zones.
const (
	headerHeight = 13 // pixels for the stop name / updated line
	rowHeight    = 27 // pixels per arrival row  (4 rows × 27 = 108, header 13 = 121 ≈ 122)

	// Column zones (2.13" defaults; 2.9" extends headsignEnd).
	routeBoxStart  = 0
	routeBoxEnd    = 30 // exclusive
	routeBoxPad    = 2
	headsignStart  = 34
	headsignEnd213 = 180
	headsignEnd29  = 230
	minutesStart   = 181 // right-edge of minutes field (right-aligned)
	minutesEnd213  = 222
	delayStart213  = 223
	delayEnd213    = 249

	maxRows = 4
	// 2.9" accommodates 5 rows; 4 is safe for both models.
)

var (
	black = color.Gray{Y: 0x00}
	white = color.Gray{Y: 0xFF}
)

// hdMinWidth is the threshold above which the HD layout is used.
const hdMinWidth = 800

// Render draws arrival sections onto a new *image.Gray of the given width/height.
// On small displays (< hdMinWidth) all sections are merged into one sorted list.
// On HD displays each section gets its own labelled band.
func Render(sections []StopSection, now time.Time, width, height int) *image.Gray {
	if width >= hdMinWidth {
		return renderHD(sections, now, width, height)
	}

	// --- Small-display path: flatten all sections into one sorted list ---
	var allArrivals []gtfs.Arrival
	var labels []string
	for _, s := range sections {
		allArrivals = append(allArrivals, s.Arrivals...)
		if s.Label != "" {
			labels = append(labels, s.Label)
		}
	}
	sort.Slice(allArrivals, func(i, j int) bool {
		return allArrivals[i].EffectiveTime().Before(allArrivals[j].EffectiveTime())
	})
	stopLabel := strings.Join(labels, " / ")

	img := image.NewGray(image.Rect(0, 0, width, height))
	for i := range img.Pix {
		img.Pix[i] = 0xFF
	}

	hsEnd := headsignEnd213
	mEnd := minutesEnd213
	dStart := delayStart213
	if width >= epd29MinWidth {
		hsEnd = headsignEnd29
		mEnd = hsEnd + 2 + 40
		dStart = mEnd + 2
	}

	// Header line.
	updated := "Updated: " + now.Format("15:04")
	headerText := "STOP: " + stopLabel
	drawText(img, headerText, 2, headerHeight-2, black)
	drawTextRight(img, updated, width-2, headerHeight-2, black)
	hLine(img, 0, width, headerHeight, black)

	if len(allArrivals) == 0 {
		drawTextCentred(img, "No departures", width/2, height/2-6, black)
		drawTextCentred(img, fmt.Sprintf("in next %d min", maxMins(now)), width/2, height/2+8, black)
		return img
	}

	rows := allArrivals
	if len(rows) > maxRows {
		rows = rows[:maxRows]
	}

	for i, a := range rows {
		y0 := headerHeight + i*rowHeight

		if i > 0 {
			hLine(img, 0, width, y0, black)
		}

		fillRect(img, routeBoxStart, y0+1, routeBoxEnd, y0+rowHeight-1, black)
		routeLabel := a.RouteShort
		if a.Platform != "" {
			routeLabel = "P" + a.Platform
		}
		routeText := padRoute(routeLabel)
		baseline := y0 + (rowHeight+basicfont.Face7x13.Metrics().Ascent.Ceil())/2
		drawText(img, routeText, routeBoxPad, baseline, white)

		hs := truncate(a.Headsign, (hsEnd-headsignStart)/7)
		drawText(img, hs, headsignStart, baseline, black)

		mins := a.MinutesUntil(now)
		var minsStr string
		switch {
		case mins < 1:
			minsStr = "< 1 min"
		case mins > 99:
			minsStr = "99 min"
		default:
			minsStr = fmt.Sprintf("%d min", mins)
		}
		drawTextRight(img, minsStr, mEnd, baseline, black)

		if !a.RealtimeTime.IsZero() && a.DelayMinutes != 0 {
			var delayStr string
			if a.DelayMinutes > 0 {
				delayStr = fmt.Sprintf("+%d", a.DelayMinutes)
			} else {
				delayStr = fmt.Sprintf("%d", a.DelayMinutes)
			}
			drawText(img, delayStr, dStart, baseline, black)
		}
	}

	return img
}

const epd29MinWidth = 290 // treat displays ≥ 290px wide as 2.9" layout

// drawText draws s with the basicfont 7×13 face starting at (x, baseline).
func drawText(img *image.Gray, s string, x, baseline int, c color.Gray) {
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(c),
		Face: basicfont.Face7x13,
		Dot:  fixed.P(x, baseline),
	}
	d.DrawString(s)
}

// drawTextRight draws s right-aligned so its right edge is at x.
func drawTextRight(img *image.Gray, s string, x, baseline int, c color.Gray) {
	w := font.MeasureString(basicfont.Face7x13, s)
	startX := x - w.Ceil()
	if startX < 0 {
		startX = 0
	}
	drawText(img, s, startX, baseline, c)
}

// drawTextCentred draws s horizontally centred around cx.
func drawTextCentred(img *image.Gray, s string, cx, baseline int, c color.Gray) {
	w := font.MeasureString(basicfont.Face7x13, s)
	drawText(img, s, cx-w.Ceil()/2, baseline, c)
}

// hLine draws a 1-pixel horizontal line.
func hLine(img *image.Gray, x0, x1, y int, c color.Gray) {
	for x := x0; x < x1; x++ {
		img.SetGray(x, y, c)
	}
}

// fillRect fills an axis-aligned rectangle (x0,y0 inclusive; x1,y1 exclusive).
func fillRect(img *image.Gray, x0, y0, x1, y1 int, c color.Gray) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.SetGray(x, y, c)
		}
	}
}

// padRoute pads a route short name to 4 characters for the route box.
func padRoute(r string) string {
	n := utf8.RuneCountInString(r)
	switch {
	case n >= 4:
		return r[:4]
	case n == 3:
		return r + " "
	case n == 2:
		return " " + r + " "
	default:
		return " " + r + "  "
	}
}

// truncate cuts s to at most maxRunes runes, appending "…" if truncated.
func truncate(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes-1]) + "…"
}

func maxMins(_ time.Time) int { return 90 }
