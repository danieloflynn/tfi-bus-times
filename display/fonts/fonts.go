// Package fonts provides pre-parsed font faces for the HD renderer.
// It uses Go Mono (a monospace truetype font from golang.org/x/image) at
// several sizes rendered at 150 DPI for the 10.3" 1872×1404 display.
package fonts

import (
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
)

var (
	// HeaderFace is 28pt @ 150 DPI (~58px) — stop name / timestamp.
	HeaderFace font.Face
	// RouteFace is 32pt @ 150 DPI (~67px) — route number in route box.
	RouteFace font.Face
	// BodyFace is 24pt @ 150 DPI (~50px) — headsign, times, delay.
	BodyFace font.Face
	// SmallFace is 18pt @ 150 DPI (~37px) — "min" suffix.
	SmallFace font.Face
)

func init() {
	f, err := opentype.Parse(gomono.TTF)
	if err != nil {
		panic("fonts: failed to parse Go Mono TTF: " + err.Error())
	}
	HeaderFace = mustFace(f, 28)
	RouteFace = mustFace(f, 32)
	BodyFace = mustFace(f, 24)
	SmallFace = mustFace(f, 18)
}

func mustFace(f *opentype.Font, size float64) font.Face {
	face, err := opentype.NewFace(f, &opentype.FaceOptions{
		Size:    size,
		DPI:     150,
		Hinting: font.HintingFull,
	})
	if err != nil {
		panic("fonts: failed to create face: " + err.Error())
	}
	return face
}
