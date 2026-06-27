//go:build linux

package driver

import (
	"image"
	"testing"
)

// newTestLCD builds an LCDDPI backed by a plain byte slice (no mmap/ioctl) so
// the pixel-packing paths can be exercised off-hardware.
func newTestLCD(width, height, bpp int) *LCDDPI {
	return &LCDDPI{
		buf:    make([]byte, width*height*(bpp/8)),
		width:  width,
		height: height,
		bpp:    bpp,
	}
}

// fillGradient writes a deterministic non-uniform gray pattern so packing bugs
// (e.g. wrong stride or channel order) surface.
func fillGradient(img *image.Gray) {
	for i := range img.Pix {
		img.Pix[i] = byte((i * 7) & 0xFF)
	}
}

func TestLCDDPI_WriteRGB565Packing(t *testing.T) {
	const w, h = 8, 4
	d := newTestLCD(w, h, 16)
	img := image.NewGray(image.Rect(0, 0, w, h))
	fillGradient(img)

	if err := d.DisplayFrame(img); err != nil {
		t.Fatalf("DisplayFrame: %v", err)
	}

	// Verify every pixel matches the documented RGB565 packing computed straight
	// from the source gray value (guards the per-row Pix walk against GrayAt).
	idx := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			g := img.GrayAt(x, y).Y
			r5 := uint16(g >> 3)
			g6 := uint16(g) * 63 / 255
			val := (r5 << 11) | (g6 << 5) | r5
			lo, hi := byte(val), byte(val>>8)
			if d.buf[idx] != lo || d.buf[idx+1] != hi {
				t.Fatalf("pixel (%d,%d) g=%d: want [%d %d], got [%d %d]",
					x, y, g, lo, hi, d.buf[idx], d.buf[idx+1])
			}
			idx += 2
		}
	}
}

func TestLCDDPI_WriteXRGB8888Packing(t *testing.T) {
	const w, h = 8, 4
	d := newTestLCD(w, h, 32)
	img := image.NewGray(image.Rect(0, 0, w, h))
	fillGradient(img)

	if err := d.DisplayFrame(img); err != nil {
		t.Fatalf("DisplayFrame: %v", err)
	}

	idx := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			g := img.GrayAt(x, y).Y
			if d.buf[idx] != g || d.buf[idx+1] != g || d.buf[idx+2] != g || d.buf[idx+3] != 0xFF {
				t.Fatalf("pixel (%d,%d) g=%d: got [%d %d %d %d]",
					x, y, g, d.buf[idx], d.buf[idx+1], d.buf[idx+2], d.buf[idx+3])
			}
			idx += 4
		}
	}
}

// BenchmarkWriteRGB565 measures the full-frame pack at the device resolution.
func BenchmarkWriteRGB565(b *testing.B) {
	const w, h = 1024, 600
	d := newTestLCD(w, h, 16)
	img := image.NewGray(image.Rect(0, 0, w, h))
	fillGradient(img)
	bounds := img.Bounds()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.writeRGB565(img, bounds)
	}
}
