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

// EPD2in9 drives a Waveshare 2.9" e-Paper HAT (SSD1680 / UC8151 controller).
// Resolution: 296 × 128 pixels.
type EPD2in9 struct {
	conn spi.Conn
	port spi.PortCloser

	pinDC   gpio.PinOut
	pinRST  gpio.PinOut
	pinBUSY gpio.PinIn
}

const (
	epd29Width  = 296
	epd29Height = 128
)

// NewEPD2in9 opens the SPI bus and GPIO pins for the 2.9" panel.
func NewEPD2in9(spiBus, spiChip, pinDC, pinRST, pinBUSY int) (*EPD2in9, error) {
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

	return &EPD2in9{
		conn:    conn,
		port:    port,
		pinDC:   dc.(gpio.PinIO),
		pinRST:  rst.(gpio.PinIO),
		pinBUSY: busyIn,
	}, nil
}

func (e *EPD2in9) Width() int  { return epd29Width }
func (e *EPD2in9) Height() int { return epd29Height }

// Init resets and initialises the 2.9" controller.
func (e *EPD2in9) Init() error {
	slog.Debug("EPD2in9 Init")
	e.hwReset()

	if err := e.sendCmd(0x12); err != nil { // SW Reset
		return err
	}
	e.waitBusy()

	// Driver output control: 127 gates (0x7F)
	if err := e.sendCmdData(0x01, 0x27, 0x01, 0x00); err != nil {
		return err
	}
	// Data entry mode
	if err := e.sendCmdData(0x11, 0x03); err != nil {
		return err
	}
	// Set RAM X range: 0x00 to 0x12 (0..18 → 0..151 bits, 19 bytes per row for 148px)
	// 296px / 8 = 37 bytes per row → 0x00..0x24
	if err := e.sendCmdData(0x44, 0x00, 0x24); err != nil {
		return err
	}
	// Set RAM Y range: 0..127 (0x7F)
	if err := e.sendCmdData(0x45, 0x00, 0x00, 0x27, 0x01); err != nil {
		return err
	}
	// Border waveform
	if err := e.sendCmdData(0x3C, 0x05); err != nil {
		return err
	}
	// Internal temp sensor
	if err := e.sendCmdData(0x18, 0x80); err != nil {
		return err
	}
	// RAM counters
	if err := e.sendCmdData(0x4E, 0x00); err != nil {
		return err
	}
	if err := e.sendCmdData(0x4F, 0x00, 0x00); err != nil {
		return err
	}
	e.waitBusy()
	return nil
}

// DisplayFrame sends a full 1bpp frame to the 2.9" display.
// img must be 296 × 128 pixels.
func (e *EPD2in9) DisplayFrame(img *image.Gray) error {
	if img.Bounds().Dx() != epd29Width || img.Bounds().Dy() != epd29Height {
		return fmt.Errorf("image must be %dx%d, got %dx%d",
			epd29Width, epd29Height,
			img.Bounds().Dx(), img.Bounds().Dy())
	}

	if err := e.sendCmdData(0x4E, 0x00); err != nil {
		return err
	}
	if err := e.sendCmdData(0x4F, 0x00, 0x00); err != nil {
		return err
	}

	// 37 bytes per row × 128 rows = 4736 bytes
	bytesPerRow := (epd29Width + 7) / 8
	buf := make([]byte, bytesPerRow*epd29Height)

	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		row := y - bounds.Min.Y
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			col := x - bounds.Min.X
			byteIdx := row*bytesPerRow + col/8
			bitIdx := uint(7 - col%8)
			if img.GrayAt(x, y).Y > 127 {
				buf[byteIdx] |= 1 << bitIdx
			}
		}
	}

	if err := e.sendCmd(0x24); err != nil {
		return err
	}
	if err := e.sendData(buf); err != nil {
		return err
	}
	if err := e.sendCmdData(0x20); err != nil {
		return err
	}
	e.waitBusy()
	slog.Debug("EPD2in9 frame displayed")
	return nil
}

// Clear fills the display with white.
func (e *EPD2in9) Clear() error {
	white := image.NewGray(image.Rect(0, 0, epd29Width, epd29Height))
	for i := range white.Pix {
		white.Pix[i] = 0xFF
	}
	return e.DisplayFrame(white)
}

// Sleep puts the controller into deep-sleep mode.
func (e *EPD2in9) Sleep() error {
	slog.Debug("EPD2in9 Sleep")
	if err := e.sendCmd(0x10); err != nil {
		return err
	}
	return e.sendData([]byte{0x01})
}

func (e *EPD2in9) hwReset() {
	e.pinRST.Out(gpio.High)
	time.Sleep(10 * time.Millisecond)
	e.pinRST.Out(gpio.Low)
	time.Sleep(10 * time.Millisecond)
	e.pinRST.Out(gpio.High)
	time.Sleep(10 * time.Millisecond)
}

func (e *EPD2in9) waitBusy() {
	for e.pinBUSY.Read() == gpio.High {
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *EPD2in9) sendCmd(cmd byte) error {
	e.pinDC.Out(gpio.Low)
	return e.conn.Tx([]byte{cmd}, nil)
}

func (e *EPD2in9) sendData(data []byte) error {
	e.pinDC.Out(gpio.High)
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

func (e *EPD2in9) sendCmdData(cmd byte, data ...byte) error {
	if err := e.sendCmd(cmd); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return e.sendData(data)
}
