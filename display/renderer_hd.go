package display

import (
	"image"
	"image/color"
	"math"
	"time"

	"tfi-display/fonts"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// HD layout constants for a 1872-px-wide display (10.3").
// Row/section heights are fixed; column x-coordinates are scaled per renderHD.
const (
  hdHeaderHeight     = 30
  hdSectionBarHeight = 36
  hdRowHeight        = 40
	hdSectionSeparator = 4  // px between sections

	// Base column zones calibrated for 1872 px width.
	hdRouteBoxStart      = 0   // always left edge
	hdBaseRouteBoxEnd    = 110
	hdBaseHeadsignStart  = 126
	hdBaseHeadsignEnd    = 1500
	hdBaseScheduledStart = 1510
	hdBaseMinEnd         = 1870
)

// RowsPerSection returns how many arrival rows fit per section for the given
// display dimensions and section count. Use this to calculate page size.
func RowsPerSection(numSections, width, height int) int {
	if width < hdMinWidth {
		return maxRows
	}
	if numSections < 1 {
		numSections = 1
	}
	availHeight := height - hdHeaderHeight - 2
	totalSepHeight := (numSections - 1) * hdSectionSeparator
	heightPerSection := (availHeight - totalSepHeight) / numSections
	rows := (heightPerSection - hdSectionBarHeight) / hdRowHeight
	if rows < 1 {
		rows = 1
	}
	return rows
}

// renderHD draws per-stop sections onto the provided large display image.
// Each section gets a labelled header bar followed by its arrival rows.
// Available height is divided evenly between sections. The image is reset to the
// black background here, so callers may reuse a buffer across frames.
func renderHD(img *image.Gray, sections []StopSection, now, feedTime time.Time, width, height int) *image.Gray {
	// Scale column x-coordinates proportionally from the 1872-px base layout.
	s := float64(width) / 1872.0
	sc := func(base int) int { return int(math.Round(float64(base) * s)) }
	hdRouteBoxEnd    := sc(hdBaseRouteBoxEnd)
	hdHeadsignStart  := sc(hdBaseHeadsignStart)
	hdHeadsignEnd    := sc(hdBaseHeadsignEnd)
	hdMinEnd := sc(hdBaseMinEnd)

	// Background is black (zeroed). clear is a cheap memset on a fresh buffer and
	// resets a reused one.
	clear(img.Pix)

	// Top header: build version on the left, timestamp on the right.
	headerBaseline := (hdHeaderHeight + fonts.HeaderAscent) / 2
	if Version != "" {
		hdDrawText(img, Version, 4, headerBaseline, white, fonts.HeaderFace)
	}
	updated := "Updated: " + feedTime.Format("15:04:05")
	hdDrawTextRight(img, updated, width-4, headerBaseline, white, fonts.HeaderFace)

	if len(sections) == 0 {
		return img
	}

	// Font metrics and fixed-string measurements are frame-invariant: the faces
	// never change and "M"/"(Sched)" are constant. Computing them once here (not
	// per arrival row) removes redundant glyph measurement on every refresh and
	// page tick — the render runs ~every 5 s on the device.
	bodyAscent := fonts.BodyAscent
	routeAscent := fonts.RouteAscent
	smallAscent := fonts.SmallAscent
	barAscent := fonts.HeaderAscent
	charW := hdMeasureString(fonts.BodyFace, "M")
	if charW < 1 {
		charW = 1
	}
	schedW := hdMeasureString(fonts.TinyFace, "(Sched)")
	headsignAvail := hdHeadsignEnd - hdHeadsignStart - schedW - 8
	maxRunes := headsignAvail / charW

	// Divide remaining height evenly between sections.
	availHeight := height - hdHeaderHeight - 2
	numSections := len(sections)
	// Account for separators between sections (not before the first).
	totalSepHeight := (numSections - 1) * hdSectionSeparator
	heightPerSection := (availHeight - totalSepHeight) / numSections
	rowsPerSection := (heightPerSection - hdSectionBarHeight) / hdRowHeight
	if rowsPerSection < 1 {
		rowsPerSection = 1
	}

	y := hdHeaderHeight + 2
	for si, sec := range sections {
		if si > 0 {
			hLine(img, 0, width, y, white)
			y++
		}

		// Section header bar: white background, black label.
		fillRect(img, 0, y, width, y+hdSectionBarHeight, white)
		barBaseline := y + (hdSectionBarHeight+barAscent)/2
		hdDrawText(img, sec.Label, 16, barBaseline, black, fonts.HeaderFace)

		y += hdSectionBarHeight

		arrivals := sec.Arrivals
		if len(arrivals) == 0 {
			// "No departures" centred in the section body.
			bodyMid := y + heightPerSection/2
			msg := "No departures"
			hdDrawText(img, msg, (width-hdMeasureString(fonts.BodyFace, msg))/2, bodyMid, white, fonts.BodyFace)
			y += heightPerSection - hdSectionBarHeight
			continue
		}

		if len(arrivals) > rowsPerSection {
			arrivals = arrivals[:rowsPerSection]
		}

		for ri, a := range arrivals {
			rowY := y + ri*hdRowHeight

			baseline := rowY + (hdRowHeight+bodyAscent)/2

			// Route box: skip for DART (section label is sufficient).
			if a.RouteShort != "DART" {
				fillRect(img, hdRouteBoxStart, rowY, hdRouteBoxEnd, rowY+hdRowHeight, black)
				routeW := hdMeasureString(fonts.RouteFace, a.RouteShort)
				routeX := hdRouteBoxStart + (hdRouteBoxEnd-hdRouteBoxStart-routeW)/2
				if routeX < hdRouteBoxStart+2 {
					routeX = hdRouteBoxStart + 2
				}
				routeBaseline := rowY + (hdRowHeight+routeAscent)/2
				hdDrawText(img, a.RouteShort, routeX, routeBaseline, white, fonts.RouteFace)
			}

			// Headsign.
			hs := truncate(a.Headsign, maxRunes)
			hdDrawText(img, hs, hdHeadsignStart, baseline, white, fonts.BodyFace)

			// Minutes until effective arrival (realtime if available, else scheduled).
			mins := a.MinutesUntil(now)
			minsStr := minLabel(mins, "Due")
			smallBaseline := rowY + (hdRowHeight+smallAscent)/2
			hdDrawTextRight(img, minsStr, hdMinEnd, smallBaseline, white, fonts.SmallFace)

			// "(Sched)" badge right-aligned just before the minutes field.
			if a.RealtimeTime.IsZero() {
				minsW := hdMeasureString(fonts.SmallFace, minsStr)
				schedX := hdMinEnd - minsW - schedW - 12
				hdDrawTextRight(img, "(Sched)", schedX+schedW, smallBaseline, white, fonts.TinyFace)
			}
		}

		y += len(arrivals) * hdRowHeight
		// Advance y to bottom of this section's allocated slot.
		sectionBottom := (hdHeaderHeight + 2) + si*(heightPerSection+hdSectionSeparator) + heightPerSection
		if y < sectionBottom {
			y = sectionBottom
		}
	}

	return img
}

// hdDrawText draws s using the given font.Face with its left edge at x.
func hdDrawText(img *image.Gray, s string, x, baseline int, c color.Gray, face font.Face) {
	d := &font.Drawer{
		Dst:  img,
		Src:  grayUniform(c),
		Face: face,
		Dot:  fixed.P(x, baseline),
	}
	d.DrawString(s)
}

// hdDrawTextRight draws s right-aligned so its right edge is at rightX.
func hdDrawTextRight(img *image.Gray, s string, rightX, baseline int, c color.Gray, face font.Face) {
	w := hdMeasureString(face, s)
	x := rightX - w
	if x < 0 {
		x = 0
	}
	hdDrawText(img, s, x, baseline, c, face)
}

// hdMeasureString returns the advance width of s in pixels for the given face.
func hdMeasureString(face font.Face, s string) int {
	return font.MeasureString(face, s).Ceil()
}
