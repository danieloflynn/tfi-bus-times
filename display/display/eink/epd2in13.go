//go:build linux

package eink

import (
	"fmt"
	"image"
	"log/slog"
	"time"

	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/spi"
	"periph.io/x/conn/v3/spi/spireg"
	"periph.io/x/host/v3"
)

// EPD2in13 drives a Waveshare 2.13" e-Paper HAT (SSD1675B / SSD1681 controller).
// Resolution: 250 × 122 pixels.
type EPD2in13 struct {
	conn spi.Conn
	port spi.PortCloser

	pinDC   gpio.PinOut
	pinRST  gpio.PinOut
	pinBUSY gpio.PinIn
}

const (
	epd213Width  = 250
	epd213Height = 122
)

// NewEPD2in13 opens the SPI bus and GPIO pins.
// spiBus/spiChip: usually 0/0 on Pi Zero 2W.
// pinDC, pinRST, pinBUSY: BCM numbers (25, 17, 24 per Waveshare HAT wiring).
func NewEPD2in13(spiBus, spiChip, pinDC, pinRST, pinBUSY int) (*EPD2in13, error) {
	if _, err := host.Init(); err != nil {
		return nil, fmt.Errorf("periph host init: %w", err)
	}

	portName := fmt.Sprintf("/dev/spidev%d.%d", spiBus, spiChip)
	port, err := spireg.Open(portName)
	if err != nil {
		return nil, fmt.Errorf("opening SPI %s: %w", portName, err)
	}

	conn, err := port.Connect(2_000_000, spi.Mode0, 8)
	if err != nil {
		port.Close()
		return nil, fmt.Errorf("SPI connect: %w", err)
	}

	dc := gpioreg.ByName(fmt.Sprintf("GPIO%d", pinDC))
	if dc == nil {
		return nil, fmt.Errorf("GPIO pin DC (BCM %d) not found", pinDC)
	}
	rst := gpioreg.ByName(fmt.Sprintf("GPIO%d", pinRST))
	if rst == nil {
		return nil, fmt.Errorf("GPIO pin RST (BCM %d) not found", pinRST)
	}
	busy := gpioreg.ByName(fmt.Sprintf("GPIO%d", pinBUSY))
	if busy == nil {
		return nil, fmt.Errorf("GPIO pin BUSY (BCM %d) not found", pinBUSY)
	}

	_ = dc.(gpio.PinIO).Out(gpio.Low)
	_ = rst.(gpio.PinIO).Out(gpio.Low)
	busyIn, ok := busy.(gpio.PinIn)
	if !ok {
		return nil, fmt.Errorf("BUSY pin does not support input")
	}
	if err := busyIn.In(gpio.PullUp, gpio.NoEdge); err != nil {
		return nil, fmt.Errorf("BUSY pin input: %w", err)
	}

	return &EPD2in13{
		conn:    conn,
		port:    port,
		pinDC:   dc.(gpio.PinIO),
		pinRST:  rst.(gpio.PinIO),
		pinBUSY: busyIn,
	}, nil
}

func (e *EPD2in13) Width() int  { return epd213Width }
func (e *EPD2in13) Height() int { return epd213Height }

// Init resets and initialises the SSD1675B controller.
func (e *EPD2in13) Init() error {
	slog.Debug("EPD2in13 Init")
	e.hwReset()

	if err := e.sendCmd(0x12); err != nil { // SW Reset
		return err
	}
	e.waitBusy()

	// Driver output control: gate = 121 (0x79), scan direction GD=0, SM=0, TB=0
	if err := e.sendCmdData(0x01, 0x79, 0x00, 0x00); err != nil {
		return err
	}
	// Data entry mode: X increment, Y increment (0x03)
	if err := e.sendCmdData(0x11, 0x03); err != nil {
		return err
	}
	// Set RAM X address range: 0x00 to 0x0F (0..15 → 0..127 bits, 32 bytes)
	if err := e.sendCmdData(0x44, 0x00, 0x0F); err != nil {
		return err
	}
	// Set RAM Y address range: 0x00 to 0x79 (0..121)
	if err := e.sendCmdData(0x45, 0x00, 0x00, 0x79, 0x00); err != nil {
		return err
	}
	// Border waveform: follow LUT (0x05 = VSS)
	if err := e.sendCmdData(0x3C, 0x05); err != nil {
		return err
	}
	// Use internal temperature sensor
	if err := e.sendCmdData(0x18, 0x80); err != nil {
		return err
	}
	// Set RAM X counter to 0
	if err := e.sendCmdData(0x4E, 0x00); err != nil {
		return err
	}
	// Set RAM Y counter to 0
	if err := e.sendCmdData(0x4F, 0x00, 0x00); err != nil {
		return err
	}
	e.waitBusy()
	return nil
}

// DisplayFrame sends a full 1bpp frame to the display.
// img must be 250 × 122 pixels; luminance > 127 → white (bit 1), ≤ 127 → black (bit 0).
func (e *EPD2in13) DisplayFrame(img *image.Gray) error {
	if img.Bounds().Dx() != epd213Width || img.Bounds().Dy() != epd213Height {
		return fmt.Errorf("image must be %dx%d, got %dx%d",
			epd213Width, epd213Height,
			img.Bounds().Dx(), img.Bounds().Dy())
	}

	// Reset counters.
	if err := e.sendCmdData(0x4E, 0x00); err != nil {
		return err
	}
	if err := e.sendCmdData(0x4F, 0x00, 0x00); err != nil {
		return err
	}

	// Write RAM: 0x24. Data is MSB-first, 1=white, 0=black.
	// 32 bytes per row (256 bits for 250-wide display) × 122 rows = 3904 bytes.
	buf := make([]byte, 32*epd213Height)
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		row := y - bounds.Min.Y
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			col := x - bounds.Min.X
			byteIdx := row*32 + col/8
			bitIdx := uint(7 - col%8)
			px := img.GrayAt(x, y).Y
			if px > 127 {
				buf[byteIdx] |= 1 << bitIdx // white
			}
			// black is 0, already zero-initialised
		}
	}

	if err := e.sendCmd(0x24); err != nil {
		return err
	}
	if err := e.sendData(buf); err != nil {
		return err
	}

	// Master activation (full refresh).
	if err := e.sendCmdData(0x20); err != nil {
		return err
	}
	e.waitBusy()
	slog.Debug("EPD2in13 frame displayed")
	return nil
}

// Clear fills the display with white.
func (e *EPD2in13) Clear() error {
	white := image.NewGray(image.Rect(0, 0, epd213Width, epd213Height))
	for i := range white.Pix {
		white.Pix[i] = 0xFF
	}
	return e.DisplayFrame(white)
}

// Sleep puts the controller into deep-sleep mode 1.
// Note: do NOT call waitBusy after this command.
func (e *EPD2in13) Sleep() error {
	slog.Debug("EPD2in13 Sleep")
	if err := e.sendCmd(0x10); err != nil {
		return err
	}
	return e.sendData([]byte{0x01})
}

// hwReset toggles the RST pin.
func (e *EPD2in13) hwReset() {
	e.pinRST.Out(gpio.High)
	time.Sleep(10 * time.Millisecond)
	e.pinRST.Out(gpio.Low)
	time.Sleep(10 * time.Millisecond)
	e.pinRST.Out(gpio.High)
	time.Sleep(10 * time.Millisecond)
}

// waitBusy polls the BUSY pin until it goes low (controller ready).
func (e *EPD2in13) waitBusy() {
	for e.pinBUSY.Read() == gpio.High {
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *EPD2in13) sendCmd(cmd byte) error {
	e.pinDC.Out(gpio.Low)
	return e.conn.Tx([]byte{cmd}, nil)
}

func (e *EPD2in13) sendData(data []byte) error {
	e.pinDC.Out(gpio.High)
	// Split into chunks to avoid large SPI transfers.
	const chunk = 4096
	for len(data) > 0 {
		n := chunk
		if n > len(data) {
			n = len(data)
		}
		if err := e.conn.Tx(data[:n], nil); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func (e *EPD2in13) sendCmdData(cmd byte, data ...byte) error {
	if err := e.sendCmd(cmd); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return e.sendData(data)
}
