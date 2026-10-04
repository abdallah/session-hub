package cli

import (
	"io"
	"strings"

	"rsc.io/qr"
)

// qrQuiet is the quiet zone around the code, in modules.
const qrQuiet = 2

// renderQR writes text as a QR code (error correction L) in half-block
// characters, two module rows per line. A light module is drawn filled and a
// dark one is a space, so on a dark terminal, the common case, the code reads
// dark on light with a filled quiet zone. Phone scanners also read the
// inverted code a light terminal shows.
func renderQR(w io.Writer, text string) error {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return err
	}
	light := func(x, y int) bool {
		inside := x >= 0 && y >= 0 && x < code.Size && y < code.Size
		return !inside || !code.Black(x, y)
	}
	var b strings.Builder
	for y := -qrQuiet; y < code.Size+qrQuiet; y += 2 {
		for x := -qrQuiet; x < code.Size+qrQuiet; x++ {
			top, bottom := light(x, y), light(x, y+1)
			switch {
			case top && bottom:
				b.WriteRune('█')
			case top:
				b.WriteRune('▀')
			case bottom:
				b.WriteRune('▄')
			default:
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}
	_, err = io.WriteString(w, b.String())
	return err
}
