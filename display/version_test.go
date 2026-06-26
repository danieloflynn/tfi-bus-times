package display_test

import (
	"testing"
	"time"

	"tfi-display/display"
)

// litPixelsTopLeft counts non-black pixels in the top-left header region, where
// the build version is drawn (the timestamp lives on the right, so this corner
// is otherwise blank background).
func litPixelsTopLeft(t *testing.T, version string) int {
	t.Helper()
	orig := display.Version
	display.Version = version
	t.Cleanup(func() { display.Version = orig })

	now := time.Now()
	img := display.Render(nil, now, now, 1024, 600)

	count := 0
	for y := 0; y < 30; y++ {
		for x := 0; x < 120; x++ {
			if img.GrayAt(x, y).Y != 0 {
				count++
			}
		}
	}
	return count
}

func TestRenderHDDrawsVersionTopLeft(t *testing.T) {
	withVersion := litPixelsTopLeft(t, "v43")
	if withVersion == 0 {
		t.Fatalf("expected version text to light up the top-left corner, got 0 lit pixels")
	}

	// With no version, the corner should stay blank (background black).
	withoutVersion := litPixelsTopLeft(t, "")
	if withoutVersion != 0 {
		t.Fatalf("expected blank top-left corner without a version, got %d lit pixels", withoutVersion)
	}
}
