//go:build linux

package main

import (
	"tfi-display/config"
	"tfi-display/display/driver"
)

// newHardwareDriver opens the real display driver on Linux.
func newHardwareDriver(cfg *config.Config) (driver.Driver, error) {
	dev := cfg.FramebufferDevice
	if dev == "" {
		dev = "/dev/fb0"
	}
	return driver.NewLCDDPI(dev)
}
