// Package epd2in66b implements a driver for the Waveshare 2.66inch E-Paper E-Ink Display Module (B)
// for Raspberry Pi Pico, 296×152, Red / Black / White
// Datasheet: https://files.waveshare.com/upload/e/ec/2.66inch-e-paper-b-specification.pdf
package epd2in66b

import (
	"image/color"
	"machine"
	"time"

	"tinygo.org/x/drivers"
)

const (
	width  = 152
	height = 296

	// using numerical values to enable generic tinygo compilation
	rstPin  = 12
	dcPin   = 8
	csPin   = 9
	busyPin = 13
)

const Baudrate = 4 * machine.MHz

type Config struct {
	ResetPin      machine.Pin
	DataPin       machine.Pin
	ChipSelectPin machine.Pin
	BusyPin       machine.Pin
}

type Device struct {
	bus  drivers.SPI
	cs   machine.Pin
	dc   machine.Pin
	rst  machine.Pin
	busy machine.Pin

	width  int16
	height int16

	blackBuffer []byte
	redBuffer   []byte
}

// New allocates a new device. The SPI for the built-in header to be used is picos machine.SPI1 at 4 MHz baudrate.
// The bus is expected to be configured and ready for use.
func New(bus drivers.SPI) Device {
	pixelCount := width * height
	if pixelCount%8 != 0 {
		// defend against copy & pasta foot-guns
		panic("pixel count expected to be a multiple of 8")
	}

	bufLen := pixelCount / 8

	return Device{
		bus:    bus,
		cs:     csPin,
		dc:     dcPin,
		rst:    rstPin,
		busy:   busyPin,
		height: height,
		width:  width,

		blackBuffer: make([]byte, bufLen),
		redBuffer:   make([]byte, bufLen),
	}
}

// Configure configures the device and its pins. The 'zero' config will fall back to the defaults.
//
// Default pins are:
//
//	Data       = GP8
//	ChipSelect = GP9
//	Reset      = GP12
//	Busy       = GP13
func (d *Device) Configure(c Config) error {
	if c.ChipSelectPin > 0 {
		d.cs = c.ChipSelectPin
	}
	if c.DataPin > 0 {
		d.dc = c.DataPin
	}

	if c.ResetPin > 0 {
		d.rst = c.ResetPin
	}

	if c.BusyPin > 0 {
		d.busy = c.BusyPin
	}

	d.cs.Configure(machine.PinConfig{Mode: machine.PinOutput})
	d.dc.Configure(machine.PinConfig{Mode: machine.PinOutput})
	d.rst.Configure(machine.PinConfig{Mode: machine.PinOutput})
	d.busy.Configure(machine.PinConfig{Mode: machine.PinInput})

	return nil
}

func (d *Device) Size() (x, y int16) {
	return d.width, d.height
}

// SetPixel modifies the internal buffer in a single pixel.
// The display has 3 colors: red, black and white
//
// - white = RGBA(255,255,255, 1-255)
// - red = RGBA(1-255,0,0,1-255)
// - Anything else as black
func (d *Device) SetPixel(x int16, y int16, c color.RGBA) {
	if x < 0 || x >= d.width || y < 0 || y >= d.height {
		return
	}

	bytePos, bitPos := pos(x, y, d.width)

	if c.R == 0xff && c.G == 0xff && c.B == 0xff && c.A > 0 { // white
		set(d.blackBuffer, bytePos, bitPos, true)
		set(d.redBuffer, bytePos, bitPos, false)
	} else if c.R != 0 && c.G == 0 && c.B == 0 && c.A > 0 { // red-ish
		set(d.blackBuffer, bytePos, bitPos, true)
		set(d.redBuffer, bytePos, bitPos, true)
	} else { // black or other
		set(d.blackBuffer, bytePos, bitPos, false)
		set(d.redBuffer, bytePos, bitPos, false)
	}
}

func set(buf []byte, bytePos, bitPos int, v bool) {
	if v {
		buf[bytePos] |= 0x1 << bitPos
	} else {
		buf[bytePos] &^= 0x1 << bitPos
	}
}

func pos(x, y, stride int16) (bytePos int, bitPos int) {
	p := int(x) + int(y)*int(stride)
	bytePos = p / 8

	// reverse bit position as it is reversed on the device's buffer
	bitPos = 7 - p%8

	return bytePos, bitPos
}

func (d *Device) Display() error {
	// Write RAM (Black White) / RAM 0x24
	// 1 == white, 0 == black
	if err := d.sendCommandByte(0x24); err != nil {
		return err
	}

	if err := d.sendData(d.blackBuffer); err != nil {
		return err
	}

	// Write RAM (RED) / RAM 0x26)
	// 0 == blank, 1 == red
	if err := d.sendCommandByte(0x26); err != nil {
		return err
	}

	if err := d.sendData(d.redBuffer); err != nil {
		return err
	}

	err := d.turnOnDisplay()

	return err
}

func (d *Device) ClearBuffer() {
	fill(d.redBuffer, 0x00)
	fill(d.blackBuffer, 0xff)
}

func (d *Device) turnOnDisplay() error {
	// also documented as 'Master Activation'
	if err := d.sendCommandByte(0x20); err != nil {
		return err
	}
	d.WaitUntilIdle()
	return nil
}

func (d *Device) Reset() error {
	d.hwReset()
	d.WaitUntilIdle()

	// soft reset & set defaults
	if err := d.sendCommandByte(0x12); err != nil {
		return err
	}
	d.WaitUntilIdle()

	// data entry mode setting
	if err := d.sendCommandSequence([]byte{0x11, 0x03}); err != nil {
		return err
	}

	if err := d.setWindow(0, d.width-1, 0, d.height-1); err != nil {
		return err
	}

	// display update control 1 - resolution setting
	if err := d.sendCommandSequence([]byte{0x21, 0x00, 0x80}); err != nil {
		return err
	}

	if err := d.setCursor(0, 0); err != nil {
		return err
	}
	d.WaitUntilIdle()

	return nil
}

func (d *Device) setCursor(x, y uint16) error {
	// Set RAM X address counter
	if err := d.sendCommandSequence([]byte{0x4e, byte(x & 0x1f)}); err != nil {
		return err
	}

	// Set RAM Y address counter
	yLo := byte(y)
	yHi := byte(y>>8) & 0x1
	if err := d.sendCommandSequence([]byte{0x4f, yLo, yHi}); err != nil {
		return err
	}

	return nil
}

func (d *Device) hwReset() {
	d.rst.High()
	time.Sleep(50 * time.Millisecond)
	d.rst.Low()
	time.Sleep(2 * time.Millisecond)
	d.rst.High()
	time.Sleep(50 * time.Millisecond)
}

func (d *Device) setWindow(xstart, xend, ystart, yend int16) error {
	// set RAM X-address start / end position
	d1 := byte((xstart >> 3) & 0x1f)
	d2 := byte((xend >> 3) & 0x1f)
	if err := d.sendCommandSequence([]byte{0x44, d1, d2}); err != nil {
		return err
	}

	// set RAM Y-address start / end position
	ystartLo := byte(ystart)
	ystartHi := byte(ystart>>8) & 0x1

	yendLo := byte(yend)
	yendHi := byte(yend>>8) & 0x1

	if err := d.sendCommandSequence([]byte{0x45, ystartLo, ystartHi, yendLo, yendHi}); err != nil {
		return err
	}
	return nil
}

func (d *Device) WaitUntilIdle() {
	// give it some time to get busy
	time.Sleep(50 * time.Millisecond)

	for d.busy.Get() { // high = busy
		time.Sleep(10 * time.Millisecond)
	}

	// give it some extra time
	time.Sleep(50 * time.Millisecond)
}

// sendCommandSequence sends the first byte in the buffer as a 'command' and all following bytes as data
func (d *Device) sendCommandSequence(seq []byte) error {
	err := d.sendCommandByte(seq[0])
	if err != nil {
		return err
	}

	for i := 1; i < len(seq); i++ {
		err = d.sendDataByte(seq[i])
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *Device) sendCommandByte(b byte) error {
	d.dc.Low()
	d.cs.Low()
	_, err := d.bus.Transfer(b)
	d.cs.High()
	return err
}

func (d *Device) sendDataByte(b byte) error {
	d.dc.High()
	d.cs.Low()
	_, err := d.bus.Transfer(b)
	d.cs.High()
	return err
}

func (d *Device) sendData(b []byte) error {
	d.dc.High()
	d.cs.Low()
	err := d.bus.Tx(b, nil)
	d.cs.High()
	return err
}

// fill quickly fills a slice with a given value
func fill(s []byte, b byte) {
	s[0] = b
	for j := 1; j < len(s); j *= 2 {
		copy(s[j:], s[:j])
	}
}