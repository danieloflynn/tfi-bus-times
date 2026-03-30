// Package driver provides a hardware-agnostic interface to display panels,
// plus concrete implementations for Waveshare e-ink and DPI LCD displays.
package driver

import "image"

// Driver abstracts one display panel.
type Driver interface {
	// Init resets and initialises the display controller.
	Init() error

	// DisplayFrame sends a full 1bpp image to the display and triggers a
	// full refresh. The image bounds must match the display's native size
	// (Width() × Height()). Pixels with luminance > 127 are white; ≤ 127 are black.
	DisplayFrame(img *image.Gray) error

	// Clear fills the display with white.
	Clear() error

	// Sleep puts the display into low-power mode (e.g. blanks backlight).
	Sleep() error

	// Wake brings the display back from sleep.
	Wake() error

	// Width returns the display width in pixels.
	Width() int

	// Height returns the display height in pixels.
	Height() int
}
