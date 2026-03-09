//go:build linux

package main

import (
	"fmt"

	"tfi-display/config"
	"tfi-display/display/eink"
)

// newHardwareDriver opens the real SPI e-ink driver on Linux.
func newHardwareDriver(cfg *config.Config) (eink.Driver, error) {
	switch cfg.DisplayModel {
	case "2.9":
		return eink.NewEPD2in9(cfg.SPIBus, cfg.SPIChip, cfg.PinDC, cfg.PinRST, cfg.PinBUSY)
	case "2.13", "":
		return eink.NewEPD2in13(cfg.SPIBus, cfg.SPIChip, cfg.PinDC, cfg.PinRST, cfg.PinBUSY)
	case "10.3":
		return eink.NewEPD10in3(cfg.SPIBus, cfg.SPIChip, cfg.PinRST, cfg.PinBUSY, cfg.VCOM)
	default:
		return nil, fmt.Errorf("unknown display model %q (supported: 2.13, 2.9, 10.3)", cfg.DisplayModel)
	}
}
