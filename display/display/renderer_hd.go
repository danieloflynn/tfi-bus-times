package display

import (
	"fmt"
	"image"
	"image/color"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
	"tfi-display/fonts"
)

// HD layout constants for a 1872×1404 display (10.3").
const (
	hdHeaderHeight     = 80 // px — top timestamp bar
	hdSectionBarHeight = 80 // px — per-stop section header bar (≥ HeaderFace ~58px + padding)
	hdRowHeight        = 80 // px per arrival row
	hdSectionSeparator = 4  // px between sections

	// Column zones (x-axis).
	hdRouteBoxStart   = 0
	hdRouteBoxEnd     = 110
	hdHeadsignStart   = 126
	hdHeadsignEnd     = 1500 // wider now that scheduled/RT/delay columns are gone
	hdScheduledStart  = 1510 // "(Scheduled)" status label
	hdMinEnd          = 1870
)

// renderHD draws per-stop sections onto a 1872×1404 (or similarly large) image.
// Each section gets a labelled header bar followed by its arrival rows.
// Available height is divided evenly between sections.
func renderHD(sections []StopSection, now time.Time, width, height int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, width, height))
	for i := range img.Pix {
		img.Pix[i] = 0xFF
	}

	// Top header: timestamp.
	headerBaseline := (hdHeaderHeight + fonts.HeaderFace.Metrics().Ascent.Ceil()) / 2
	updated := "Updated: " + now.Format("15:04:05")
	hdDrawTextRight(img, updated, width-4, headerBaseline, black, fonts.HeaderFace)
	hLine(img, 0, width, hdHeaderHeight, black)
	hLine(img, 0, width, hdHeaderHeight+1, black)

	if len(sections) == 0 {
		return img
	}

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
			// Thick separator between sections.
			fillRect(img, 0, y, width, y+hdSectionSeparator, color.Gray{Y: 0x60})
			y += hdSectionSeparator
		}

		// Section header bar: dark gray background, white stop label.
		fillRect(img, 0, y, width, y+hdSectionBarHeight, color.Gray{Y: 0x30})
		barAscent := fonts.HeaderFace.Metrics().Ascent.Ceil()
		barBaseline := y + (hdSectionBarHeight+barAscent)/2
		hdDrawText(img, sec.Label, 16, barBaseline, white, fonts.HeaderFace)

		y += hdSectionBarHeight

		arrivals := sec.Arrivals
		if len(arrivals) == 0 {
			// "No departures" centred in the section body.
			bodyMid := y + heightPerSection/2
			msg := "No departures"
			hdDrawText(img, msg, (width-hdMeasureString(fonts.BodyFace, msg))/2, bodyMid, black, fonts.BodyFace)
			y += heightPerSection - hdSectionBarHeight
			continue
		}

		if len(arrivals) > rowsPerSection {
			arrivals = arrivals[:rowsPerSection]
		}

		for ri, a := range arrivals {
			rowY := y + ri*hdRowHeight

			if ri > 0 {
				hLine(img, 0, width, rowY, color.Gray{Y: 0xCC})
			}

			ascent := fonts.BodyFace.Metrics().Ascent.Ceil()
			baseline := rowY + (hdRowHeight+ascent)/2

			// Route box: show platform code if available (e.g. DART), else route short name.
			routeLabel := a.RouteShort
			if a.Platform != "" {
				routeLabel = "Plt " + a.Platform
			}
			fillRect(img, hdRouteBoxStart, rowY, hdRouteBoxEnd, rowY+hdRowHeight, black)
			routeW := hdMeasureString(fonts.RouteFace, routeLabel)
			routeX := hdRouteBoxStart + (hdRouteBoxEnd-hdRouteBoxStart-routeW)/2
			if routeX < hdRouteBoxStart+2 {
				routeX = hdRouteBoxStart + 2
			}
			routeAscent := fonts.RouteFace.Metrics().Ascent.Ceil()
			routeBaseline := rowY + (hdRowHeight+routeAscent)/2
			hdDrawText(img, routeLabel, routeX, routeBaseline, white, fonts.RouteFace)

			// Headsign.
			charW := hdMeasureString(fonts.BodyFace, "M")
			if charW < 1 {
				charW = 1
			}
			maxRunes := (hdHeadsignEnd - hdHeadsignStart) / charW
			hs := truncate(a.Headsign, maxRunes)
			hdDrawText(img, hs, hdHeadsignStart, baseline, black, fonts.BodyFace)

			// "(Scheduled)" badge when no realtime data — signals the bus may not show.
			if a.RealtimeTime.IsZero() {
				hdDrawText(img, "(Scheduled)", hdScheduledStart, baseline, color.Gray{Y: 0x80}, fonts.SmallFace)
			}

			// Minutes until effective arrival (realtime if available, else scheduled).
			mins := a.MinutesUntil(now)
			var minsStr string
			switch {
			case mins < 1:
				minsStr = "Due"
			case mins > 99:
				minsStr = "99 min"
			default:
				minsStr = fmt.Sprintf("%d min", mins)
			}
			smallAscent := fonts.SmallFace.Metrics().Ascent.Ceil()
			smallBaseline := rowY + (hdRowHeight+smallAscent)/2
			hdDrawTextRight(img, minsStr, hdMinEnd, smallBaseline, black, fonts.SmallFace)
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
		Src:  image.NewUniform(c),
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
