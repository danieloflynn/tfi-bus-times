// Package fonts provides pre-parsed font faces for the HD renderer.
// Uses Atkinson Hyperlegible Bold — designed for readability at distance.
package fonts

import (
	_ "embed"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

//go:embed AtkinsonHyperlegible-Bold.ttf
var atkinsonBoldTTF []byte

var (
	// HeaderFace — section title / timestamp.
	HeaderFace font.Face
	// RouteFace — route number in the route box.
	RouteFace font.Face
	// BodyFace — headsign and times.
	BodyFace font.Face
	// SmallFace — "min" suffix.
	SmallFace font.Face
	// TinyFace — small badges like "(Sched)".
	TinyFace font.Face
)

func init() {
	f, err := opentype.Parse(atkinsonBoldTTF)
	if err != nil {
		panic("fonts: failed to parse Atkinson Hyperlegible TTF: " + err.Error())
	}
	HeaderFace = mustFace(f, 14)
	RouteFace  = mustFace(f, 18)
	BodyFace   = mustFace(f, 16)
	SmallFace  = mustFace(f, 18)
	TinyFace   = mustFace(f, 10)
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
