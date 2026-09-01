package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

// The icon is the entire status display of a program with no window, so the
// states have to be tellable apart at a glance and the bytes have to be an ICO
// Windows will actually draw. A malformed icon is not a cosmetic problem: it
// is a tray with nothing in it, which reads as "not running".
func TestEachStateDrawsADistinctValidIcon(t *testing.T) {
	seen := map[string]state{}
	for _, s := range []state{stateIdle, stateWorking, stateOffline, statePaused} {
		b := trayIcon(s)
		if len(b) < 22 {
			t.Fatalf("state %d produced no icon", s)
		}
		var hdr struct {
			Reserved, Type, Count uint16
		}
		if err := binary.Read(bytes.NewReader(b[:6]), binary.LittleEndian, &hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Reserved != 0 || hdr.Type != 1 || hdr.Count != 1 {
			t.Errorf("state %d: not an ICO header: %+v", s, hdr)
		}
		// The declared payload offset and length must describe the bytes that
		// are actually there, or Windows draws nothing.
		size := binary.LittleEndian.Uint32(b[14:18])
		off := binary.LittleEndian.Uint32(b[18:22])
		if off != 22 || int(off)+int(size) != len(b) {
			t.Errorf("state %d: header describes %d bytes at %d, file has %d", s, size, off, len(b))
		}
		if _, err := png.Decode(bytes.NewReader(b[off:])); err != nil {
			t.Errorf("state %d: payload is not a decodable PNG: %v", s, err)
		}
		if prev, dup := seen[string(b)]; dup {
			t.Errorf("state %d and state %d draw the same icon, so the tray cannot tell them apart", s, prev)
		}
		seen[string(b)] = s
	}
}
