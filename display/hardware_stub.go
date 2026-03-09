//go:build !linux

package main

import (
	"fmt"

	"tfi-display/config"
	"tfi-display/display/eink"
)

// newHardwareDriver returns an error on non-Linux platforms.
// Use the -mock flag to run without hardware.
func newHardwareDriver(_ *config.Config) (eink.Driver, error) {
	return nil, fmt.Errorf("hardware SPI display is only supported on Linux; use -mock flag")
}
