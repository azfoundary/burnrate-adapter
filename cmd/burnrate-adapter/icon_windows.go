package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
)

// trayIcon draws the notification-area icon for a state.
//
// Drawn rather than shipped as four .ico files. The icon's whole job is to
// answer "is it working" from the corner of an eye, so the states have to stay
// visibly different from each other, and four hand-made files drift apart the
// first time one is edited. Generating them keeps the shapes identical and the
// colour the only difference, which is exactly the design.
func trayIcon(s state) []byte {
	var c color.RGBA
	switch s {
	case stateWorking:
		c = color.RGBA{0x1d, 0x7d, 0x3f, 0xff} // green: writing
	case stateIdle:
		c = color.RGBA{0x2d, 0x6a, 0x4f, 0xff} // calm green: connected, nothing to do
	case statePaused:
		c = color.RGBA{0xb4, 0x7d, 0x1a, 0xff} // amber: deliberately stopped
	default:
		c = color.RGBA{0xa3, 0x2f, 0x2f, 0xff} // red: cannot reach BurnRate
	}
	const n = 32
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	// A filled disc. At 16 px on a taskbar a disc reads as one solid colour,
	// where a glyph turns to mud and the state stops being legible.
	const r = n/2 - 2
	cx, cy := n/2, n/2
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r*r {
				img.SetRGBA(x, y, c)
			}
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		return nil
	}
	return icoWrap(pngBuf.Bytes(), n)
}

// icoWrap puts a PNG inside an ICO container, which is what SetIcon wants on
// Windows. PNG-compressed ICO entries have been understood since Vista.
func icoWrap(pngData []byte, size int) []byte {
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	w(uint16(0)) // reserved
	w(uint16(1)) // type: icon
	w(uint16(1)) // one image
	w(uint8(size))
	w(uint8(size))
	w(uint8(0)) // colours in palette: none, it is truecolour
	w(uint8(0)) // reserved
	w(uint16(1))
	w(uint16(32))
	w(uint32(len(pngData)))
	w(uint32(22)) // the image starts after this 22-byte header
	b.Write(pngData)
	return b.Bytes()
}
