package server

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

// Both icons are served without credentials and decode as images of the
// right size; the ICO wraps a PNG.
func TestIcons(t *testing.T) {
	e := newEnv(t)
	code, b, h := e.do("GET", "/apple-touch-icon.png", "", nil)
	if code != 200 || h.Get("Content-Type") != "image/png" {
		t.Fatalf("touch icon: %d %s", code, h.Get("Content-Type"))
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil || img.Bounds().Dx() != 180 || img.Bounds().Dy() != 180 {
		t.Fatalf("touch icon: %v %v", err, img)
	}

	code, b, h = e.do("GET", "/favicon.ico", "", nil)
	if code != 200 || h.Get("Content-Type") != "image/x-icon" {
		t.Fatalf("favicon: %d %s", code, h.Get("Content-Type"))
	}
	if len(b) < 22 || binary.LittleEndian.Uint16(b[2:]) != 1 || binary.LittleEndian.Uint16(b[4:]) != 1 || b[6] != 32 {
		t.Fatalf("favicon header % x", b[:min(len(b), 22)])
	}
	size := binary.LittleEndian.Uint32(b[14:])
	off := binary.LittleEndian.Uint32(b[18:])
	if int(off+size) != len(b) {
		t.Fatalf("favicon entry size %d offset %d, file %d", size, off, len(b))
	}
	img, err = png.Decode(bytes.NewReader(b[off:]))
	if err != nil || img.Bounds().Dx() != 32 {
		t.Fatalf("favicon PNG: %v %v", err, img)
	}
	// The corners are transparent (rounded square) and the centre is the
	// white sessionhub.
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Errorf("corner alpha %d, want 0", a)
	}
	if r, g, bl, _ := img.At(16, 16).RGBA(); r>>8 < 0xf0 || g>>8 < 0xf0 || bl>>8 < 0xf0 {
		t.Errorf("centre %x %x %x, want white", r>>8, g>>8, bl>>8)
	}
}
