//go:build !linux

package main

import (
	"fmt"

	"tfi-display/config"
	"tfi-display/display/driver"
)

// newHardwareDriver returns an error on non-Linux platforms.
// Use the -mock flag to run without hardware.
func newHardwareDriver(_ *config.Config) (driver.Driver, error) {
	return nil, fmt.Errorf("hardware display is only supported on Linux; use -mock flag")
}
