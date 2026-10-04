package server

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"sync"
)

// The dashboard's icon: a sessionhub with four spokes, white on the page's accent
// blue. Browsers ask for /favicon.ico on their own, and iOS asks for
// /apple-touch-icon.png when you add the page to the home screen, so the
// page needs no <link> and keeps its no-href rule.

var (
	iconOnce   sync.Once
	faviconICO []byte // 32x32 PNG in an ICO container
	touchPNG   []byte // 180x180 PNG
)

func buildIcons() {
	iconOnce.Do(func() {
		faviconICO = icoFromPNG(drawHub(32), 32)
		touchPNG = drawHub(180)
	})
}

// drawHub renders the icon at size x size pixels, anti-aliased by sampling
// each pixel 4x4 times.
func drawHub(size int) []byte {
	accent := color.NRGBA{0x0b, 0x5c, 0xad, 0xff}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	const n = 4
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var bg, fg int
			for sy := 0; sy < n; sy++ {
				for sx := 0; sx < n; sx++ {
					px := (float64(x) + (float64(sx)+.5)/n) / s
					py := (float64(y) + (float64(sy)+.5)/n) / s
					switch {
					case !inRoundedSquare(px, py, .22):
					case inHub(px, py):
						fg++
					default:
						bg++
					}
				}
			}
			total := n * n
			if bg+fg == 0 {
				continue
			}
			// Mix white over the accent by the share of sessionhub samples; the
			// share of samples inside the square sets the alpha.
			mix := func(a, b uint8) uint8 {
				return uint8((int(a)*bg + int(b)*fg) / (bg + fg))
			}
			img.SetNRGBA(x, y, color.NRGBA{
				mix(accent.R, 0xff), mix(accent.G, 0xff), mix(accent.B, 0xff),
				uint8(255 * (bg + fg) / total),
			})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

// inRoundedSquare reports whether (x, y), in unit coordinates, is inside the
// unit square with corners rounded to radius r.
func inRoundedSquare(x, y, r float64) bool {
	cx := math.Max(r, math.Min(1-r, x))
	cy := math.Max(r, math.Min(1-r, y))
	return math.Hypot(x-cx, y-cy) <= r
}

// inHub reports whether (x, y) is on the sessionhub mark: a centre disc, four
// outer discs on the diagonals, and the spokes joining them.
func inHub(x, y float64) bool {
	const c, d = .5, .27 // centre, and the outer discs' offset from it
	if math.Hypot(x-c, y-c) <= .14 {
		return true
	}
	for _, o := range [][2]float64{{-d, -d}, {d, -d}, {-d, d}, {d, d}} {
		ox, oy := c+o[0], c+o[1]
		if math.Hypot(x-ox, y-oy) <= .085 {
			return true
		}
		if segDist(x, y, c, c, ox, oy) <= .035 {
			return true
		}
	}
	return false
}

// segDist is the distance from (px, py) to the segment (ax, ay)-(bx, by).
func segDist(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := ((px-ax)*dx + (py-ay)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
}

// icoFromPNG wraps one PNG image in an ICO container, which every browser
// accepts at /favicon.ico.
func icoFromPNG(p []byte, size int) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, []uint16{0, 1, 1}) // reserved, type icon, one image
	dim := byte(size)
	if size >= 256 {
		dim = 0
	}
	b.Write([]byte{dim, dim, 0, 0})                                         // width, height, palette, reserved
	binary.Write(&b, binary.LittleEndian, []uint16{1, 32})                  // planes, bits per pixel
	binary.Write(&b, binary.LittleEndian, []uint32{uint32(len(p)), 6 + 16}) // size, offset
	b.Write(p)
	return b.Bytes()
}

func (s *Server) favicon(w http.ResponseWriter, r *http.Request) {
	buildIcons()
	writeIcon(w, "image/x-icon", faviconICO)
}

func (s *Server) touchIcon(w http.ResponseWriter, r *http.Request) {
	buildIcons()
	writeIcon(w, "image/png", touchPNG)
}

func writeIcon(w http.ResponseWriter, ctype string, b []byte) {
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "public, max-age=86400")
	h.Set("X-Content-Type-Options", "nosniff")
	w.Write(b)
}
