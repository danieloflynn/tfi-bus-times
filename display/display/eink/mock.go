// Package eink mock driver — writes frames as PNG files for visual inspection.
// This file is always compiled (no build tag) and can be used on any platform.
package eink

import (
	"fmt"
	"image"
	"image/png"
	"os"
)

// Display dimensions (duplicated here so mock.go compiles without the linux drivers).
const (
	mockEPD213Width   = 250
	mockEPD213Height  = 122
	mockEPD29Width    = 296
	mockEPD29Height   = 128
	mockEPD10in3Width = 1872
	mockEPD10in3Height = 1404
)

// MockDriver implements Driver by saving frames as PNG files.
// Useful for developing the renderer on non-Pi hardware.
type MockDriver struct {
	width, height int
	outDir        string
	frameCount    int
}

// NewMockDriver creates a MockDriver writing PNG files to outDir.
// model should be "2.13" or "2.9".
func NewMockDriver(model, outDir string) (*MockDriver, error) {
	w, h := mockEPD213Width, mockEPD213Height
	switch model {
	case "2.9":
		w, h = mockEPD29Width, mockEPD29Height
	case "10.3":
		w, h = mockEPD10in3Width, mockEPD10in3Height
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating mock output dir: %w", err)
	}
	return &MockDriver{width: w, height: h, outDir: outDir}, nil
}

func (m *MockDriver) Width() int  { return m.width }
func (m *MockDriver) Height() int { return m.height }
func (m *MockDriver) Init() error  { return nil }
func (m *MockDriver) Sleep() error { return nil }

func (m *MockDriver) Clear() error {
	white := image.NewGray(image.Rect(0, 0, m.width, m.height))
	for i := range white.Pix {
		white.Pix[i] = 0xFF
	}
	return m.DisplayFrame(white)
}

func (m *MockDriver) DisplayFrame(img *image.Gray) error {
	m.frameCount++
	path := fmt.Sprintf("%s/frame_%04d.png", m.outDir, m.frameCount)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return err
	}
	fmt.Printf("[mock] frame %d written to %s\n", m.frameCount, path)
	return nil
}
