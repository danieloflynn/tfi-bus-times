// Package eink provides a hardware-agnostic interface to Waveshare e-ink
// displays, plus concrete implementations for the 2.13" and 2.9" models.
package eink

import "image"

// Driver abstracts one e-ink display panel.
type Driver interface {
	// Init resets and initialises the display controller.
	Init() error

	// DisplayFrame sends a full 1bpp image to the display and triggers a
	// full refresh. The image bounds must match the display's native size
	// (Width() × Height()). Pixels with luminance > 127 are white; ≤ 127 are black.
	DisplayFrame(img *image.Gray) error

	// Clear fills the display with white.
	Clear() error

	// Sleep puts the controller into deep-sleep mode (lowest power).
	// A hardware reset is required to wake it.
	Sleep() error

	// Width returns the display width in pixels.
	Width() int

	// Height returns the display height in pixels.
	Height() int
}
