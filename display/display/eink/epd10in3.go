//go:build linux

package eink

import (
	"encoding/binary"
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

const (
	epd10in3Width  = 1872
	epd10in3Height = 1404

	// IT8951 SPI preamble words.
	it8951PreambleCmd  = 0x6000
	it8951PreambleData = 0x0000
	it8951PreambleRead = 0x1000

	// IT8951 host commands.
	it8951CmdSysRun      = 0x0001
	it8951CmdSleep       = 0x0003
	it8951CmdRegWr       = 0x0011
	it8951CmdLdImg       = 0x0020
	it8951CmdLdImgArea   = 0x0021
	it8951CmdLdImgEnd    = 0x0022
	it8951CmdDisplayArea = 0x0034
	it8951CmdGetDevInfo  = 0x0350

	// IT8951 registers.
	it8951RegI80CPCR = 0x0004 // packed-write enable
	it8951RegVCOM    = 0x0008

	// IT8951 display modes.
	it8951ModeINIT = 0 // full flash
	it8951ModeGC16 = 2 // 16-level grey
)

// EPD10in3 drives a Waveshare 10.3" HD e-Paper display (IT8951 controller).
// Resolution: 1872 × 1404 pixels.
type EPD10in3 struct {
	conn       spi.Conn
	port       spi.PortCloser
	pinRST     gpio.PinOut
	pinBUSY    gpio.PinIn
	imgBufAddr uint32
	vcom       int16 // millivolts magnitude (positive), e.g. 2060 for VCOM=-2.06V
}

// NewEPD10in3 opens the SPI bus and GPIO pins for the IT8951-based 10.3" display.
// vcom is the negative VCOM voltage printed on the FPC ribbon (e.g. -2.06).
func NewEPD10in3(spiBus, spiChip, pinRST, pinBUSY int, vcom float64) (*EPD10in3, error) {
	if _, err := host.Init(); err != nil {
		return nil, fmt.Errorf("periph host init: %w", err)
	}

	portName := fmt.Sprintf("/dev/spidev%d.%d", spiBus, spiChip)
	port, err := spireg.Open(portName)
	if err != nil {
		return nil, fmt.Errorf("opening SPI %s: %w", portName, err)
	}

	// 24 MHz — the 1.3 MB pixel payload would take ~5s at 2 MHz.
	conn, err := port.Connect(24_000_000, spi.Mode0, 8)
	if err != nil {
		port.Close()
		return nil, fmt.Errorf("SPI connect: %w", err)
	}

	rst := gpioreg.ByName(fmt.Sprintf("GPIO%d", pinRST))
	if rst == nil {
		port.Close()
		return nil, fmt.Errorf("GPIO pin RST (BCM %d) not found", pinRST)
	}
	busy := gpioreg.ByName(fmt.Sprintf("GPIO%d", pinBUSY))
	if busy == nil {
		port.Close()
		return nil, fmt.Errorf("GPIO pin BUSY (BCM %d) not found", pinBUSY)
	}

	_ = rst.(gpio.PinIO).Out(gpio.Low)
	busyIn, ok := busy.(gpio.PinIn)
	if !ok {
		port.Close()
		return nil, fmt.Errorf("BUSY pin does not support input")
	}
	if err := busyIn.In(gpio.PullUp, gpio.NoEdge); err != nil {
		port.Close()
		return nil, fmt.Errorf("BUSY pin input: %w", err)
	}

	// Store VCOM as positive millivolts magnitude.
	vcomMV := int16(-vcom * 1000)

	return &EPD10in3{
		conn:    conn,
		port:    port,
		pinRST:  rst.(gpio.PinIO),
		pinBUSY: busyIn,
		vcom:    vcomMV,
	}, nil
}

func (e *EPD10in3) Width() int  { return epd10in3Width }
func (e *EPD10in3) Height() int { return epd10in3Height }

// Init resets and initialises the IT8951 controller.
func (e *EPD10in3) Init() error {
	slog.Debug("EPD10in3 Init")

	// Hardware reset: RST low 200ms → high 200ms → wait busy.
	e.pinRST.Out(gpio.Low)
	time.Sleep(200 * time.Millisecond)
	e.pinRST.Out(gpio.High)
	time.Sleep(200 * time.Millisecond)
	e.waitBusy()

	// Wake up the system.
	if err := e.writeCmd(it8951CmdSysRun); err != nil {
		return fmt.Errorf("SYS_RUN: %w", err)
	}

	// Enable packed write mode.
	if err := e.writeReg(it8951RegI80CPCR, 0x0001); err != nil {
		return fmt.Errorf("enable pack write: %w", err)
	}

	// Get device info to retrieve image buffer address.
	addr, err := e.getDevInfo()
	if err != nil {
		return fmt.Errorf("GetDevInfo: %w", err)
	}
	e.imgBufAddr = addr
	slog.Debug("EPD10in3 imgBufAddr", "addr", fmt.Sprintf("0x%08X", addr))

	// Set VCOM voltage.
	if err := e.writeReg(it8951RegVCOM, uint16(e.vcom)); err != nil {
		return fmt.Errorf("set VCOM: %w", err)
	}

	return nil
}

// DisplayFrame sends a full 4BPP frame to the display using GC16 waveform.
func (e *EPD10in3) DisplayFrame(img *image.Gray) error {
	return e.displayFrame(img, it8951ModeGC16)
}

// Clear fills the display with white using the INIT (full-flash) waveform.
func (e *EPD10in3) Clear() error {
	white := image.NewGray(image.Rect(0, 0, epd10in3Width, epd10in3Height))
	for i := range white.Pix {
		white.Pix[i] = 0xFF
	}
	return e.displayFrame(white, it8951ModeINIT)
}

// Sleep sends the IT8951 to low-power mode.
func (e *EPD10in3) Sleep() error {
	slog.Debug("EPD10in3 Sleep")
	return e.writeCmd(it8951CmdSleep)
}

// displayFrame is the internal implementation shared by DisplayFrame and Clear.
func (e *EPD10in3) displayFrame(img *image.Gray, mode uint16) error {
	addrLo := uint16(e.imgBufAddr & 0xFFFF)
	addrHi := uint16(e.imgBufAddr >> 16)

	// LD_IMG: set up load with endian=0, pixFmt=4BPP (0x2), rotate=0.
	if err := e.writeCmd(it8951CmdLdImg); err != nil {
		return err
	}
	if err := e.writeWords([]uint16{0x0000, 0x0002, 0x0000, addrLo, addrHi}); err != nil {
		return err
	}

	// LD_IMG_AREA: define the area to load.
	if err := e.writeCmd(it8951CmdLdImgArea); err != nil {
		return err
	}
	if err := e.writeWords([]uint16{
		0x0000, 0x0002, 0x0000, addrLo, addrHi,
		0, 0, epd10in3Width, epd10in3Height,
	}); err != nil {
		return err
	}

	// Send packed 4BPP pixel data in chunks.
	if err := e.sendPixels(img); err != nil {
		return err
	}

	// LD_IMG_END.
	if err := e.writeCmd(it8951CmdLdImgEnd); err != nil {
		return err
	}

	// DISPLAY_AREA: trigger refresh.
	if err := e.writeCmd(it8951CmdDisplayArea); err != nil {
		return err
	}
	if err := e.writeWords([]uint16{
		addrLo, addrHi, mode,
		0, 0, epd10in3Width, epd10in3Height,
	}); err != nil {
		return err
	}
	e.waitBusy()

	slog.Debug("EPD10in3 frame displayed")
	return nil
}

// sendPixels packs 8-bit grayscale into 4BPP and sends via data preamble chunks.
// Each byte carries two pixels: lo nibble = left pixel >> 4, hi nibble = right pixel >> 4.
// Row stride: 936 bytes (1872 px / 2). Total: 936 × 1404 = 1,314,144 bytes.
func (e *EPD10in3) sendPixels(img *image.Gray) error {
	const chunk = 4096
	buf := make([]byte, chunk)
	// Preamble + waitBusy before the data stream.
	if err := e.sendPreamble(it8951PreambleData); err != nil {
		return err
	}

	pos := 0
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x += 2 {
			p0 := img.GrayAt(x, y).Y
			p1 := img.GrayAt(x+1, y).Y
			buf[pos] = (p0 >> 4) | ((p1 >> 4) << 4)
			pos++
			if pos == chunk {
				if err := e.conn.Tx(buf, nil); err != nil {
					return err
				}
				pos = 0
			}
		}
	}
	if pos > 0 {
		if err := e.conn.Tx(buf[:pos], nil); err != nil {
			return err
		}
	}
	return nil
}

// waitBusy polls until BUSY goes high (IT8951: low=busy, high=ready).
func (e *EPD10in3) waitBusy() {
	for e.pinBUSY.Read() == gpio.Low {
		time.Sleep(10 * time.Millisecond)
	}
}

// sendPreamble sends the 2-byte preamble word then waits for BUSY.
func (e *EPD10in3) sendPreamble(preamble uint16) error {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], preamble)
	if err := e.conn.Tx(b[:], nil); err != nil {
		return err
	}
	e.waitBusy()
	return nil
}

// writeCmd sends a command word to IT8951.
func (e *EPD10in3) writeCmd(cmd uint16) error {
	if err := e.sendPreamble(it8951PreambleCmd); err != nil {
		return err
	}
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], cmd)
	return e.conn.Tx(b[:], nil)
}

// writeWords sends a data preamble followed by the given 16-bit words.
func (e *EPD10in3) writeWords(words []uint16) error {
	if err := e.sendPreamble(it8951PreambleData); err != nil {
		return err
	}
	buf := make([]byte, len(words)*2)
	for i, w := range words {
		binary.BigEndian.PutUint16(buf[i*2:], w)
	}
	return e.conn.Tx(buf, nil)
}

// writeReg writes a 16-bit value to an IT8951 register.
func (e *EPD10in3) writeReg(reg, val uint16) error {
	if err := e.writeCmd(it8951CmdRegWr); err != nil {
		return err
	}
	return e.writeWords([]uint16{reg, val})
}

// getDevInfo reads the IT8951 device-info block and returns the image buffer address.
func (e *EPD10in3) getDevInfo() (uint32, error) {
	if err := e.writeCmd(it8951CmdGetDevInfo); err != nil {
		return 0, err
	}
	// Read 20 words (40 bytes). Words [2] and [3] are imgBufAddr lo/hi.
	if err := e.sendPreamble(it8951PreambleRead); err != nil {
		return 0, err
	}
	buf := make([]byte, 40)
	dummy := make([]byte, 40)
	if err := e.conn.Tx(dummy, buf); err != nil {
		return 0, err
	}
	addrLo := binary.BigEndian.Uint16(buf[4:6])  // word index 2
	addrHi := binary.BigEndian.Uint16(buf[6:8])  // word index 3
	return uint32(addrHi)<<16 | uint32(addrLo), nil
}
