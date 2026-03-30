//go:build linux

package driver

import (
	"encoding/binary"
	"fmt"
	"image"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// FBIOGET_VSCREENINFO is the Linux ioctl to read variable screen info.
const fbioGetVScreenInfo = 0x4600

// LCDDPI drives a Linux DPI framebuffer display (e.g. a 7" IPS LCD on /dev/fb0).
// Pixels are written directly to the mmap'd framebuffer; no SPI or GPIO needed.
type LCDDPI struct {
	file      *os.File
	blankPath string // /sys/class/graphics/fbX/blank
	buf       []byte
	width     int
	height    int
	bpp       int // bits per pixel: 16 or 32
}

// NewLCDDPI opens the framebuffer device at path (e.g. "/dev/fb0").
// Width, height, and bpp are read from the kernel via ioctl after opening.
func NewLCDDPI(path string) (*LCDDPI, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("opening framebuffer %s: %w", path, err)
	}

	// Read variable screen info (160-byte struct; we only need the first 28 bytes).
	var info [160]byte
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		fbioGetVScreenInfo,
		uintptr(unsafe.Pointer(&info[0])),
	); errno != 0 {
		f.Close()
		return nil, fmt.Errorf("FBIOGET_VSCREENINFO: %w", errno)
	}

	width := int(binary.LittleEndian.Uint32(info[0:4]))
	height := int(binary.LittleEndian.Uint32(info[4:8]))
	bpp := int(binary.LittleEndian.Uint32(info[24:28]))

	if width <= 0 || height <= 0 {
		f.Close()
		return nil, fmt.Errorf("invalid framebuffer dimensions %dx%d", width, height)
	}
	if bpp != 16 && bpp != 32 {
		f.Close()
		return nil, fmt.Errorf("unsupported framebuffer bpp %d (need 16 or 32)", bpp)
	}

	bufSize := width * height * (bpp / 8)
	buf, err := syscall.Mmap(
		int(f.Fd()),
		0,
		bufSize,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED,
	)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap framebuffer: %w", err)
	}

	blankPath := "/sys/class/graphics/" + filepath.Base(path) + "/blank"
	slog.Info("LCD framebuffer opened", "path", path, "width", width, "height", height, "bpp", bpp)
	return &LCDDPI{file: f, blankPath: blankPath, buf: buf, width: width, height: height, bpp: bpp}, nil
}

func (d *LCDDPI) Width() int  { return d.width }
func (d *LCDDPI) Height() int { return d.height }

// Init is a no-op for DPI LCD; the framebuffer is ready after mmap.
func (d *LCDDPI) Init() error { return nil }

// Sleep blanks the framebuffer display via sysfs.
func (d *LCDDPI) Sleep() error {
	return os.WriteFile(d.blankPath, []byte("1"), 0)
}

// Wake unblanks the framebuffer display via sysfs.
func (d *LCDDPI) Wake() error {
	return os.WriteFile(d.blankPath, []byte("0"), 0)
}

// Clear fills the framebuffer with white (0xFF bytes).
func (d *LCDDPI) Clear() error {
	for i := range d.buf {
		d.buf[i] = 0xFF
	}
	return nil
}

// DisplayFrame converts the grayscale image to the framebuffer's native pixel
// format (RGB565 or XRGB8888) and copies it into the mmap'd buffer.
func (d *LCDDPI) DisplayFrame(img *image.Gray) error {
	bounds := img.Bounds()
	w := bounds.Dx()
	h := bounds.Dy()
	if w != d.width || h != d.height {
		return fmt.Errorf("image must be %dx%d, got %dx%d", d.width, d.height, w, h)
	}

	if d.bpp == 16 {
		d.writeRGB565(img, bounds)
	} else {
		d.writeXRGB8888(img, bounds)
	}
	return nil
}

// writeRGB565 packs each gray pixel into a 16-bit RGB565 value.
func (d *LCDDPI) writeRGB565(img *image.Gray, bounds image.Rectangle) {
	idx := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			g := img.GrayAt(x, y).Y
			r5 := uint16(g >> 3)
			g6 := uint16(g) * 63 / 255
			b5 := r5
			val := (r5 << 11) | (g6 << 5) | b5
			d.buf[idx] = uint8(val)
			d.buf[idx+1] = uint8(val >> 8)
			idx += 2
		}
	}
}

// writeXRGB8888 writes each gray pixel as a 32-bit XRGB8888 value (little-endian).
func (d *LCDDPI) writeXRGB8888(img *image.Gray, bounds image.Rectangle) {
	idx := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			g := img.GrayAt(x, y).Y
			d.buf[idx] = g    // B
			d.buf[idx+1] = g  // G
			d.buf[idx+2] = g  // R
			d.buf[idx+3] = 0xFF // A/X — set fully opaque in case format is ARGB
			idx += 4
		}
	}
}
