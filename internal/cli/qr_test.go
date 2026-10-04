package cli

import (
	"bytes"
	"strings"
	"testing"

	"rsc.io/qr"
)

// TestRenderQRMatchesEncoder decodes the printed half blocks back into
// modules and compares every one, quiet zone included, with the encoder.
func TestRenderQRMatchesEncoder(t *testing.T) {
	const link = "https://sessionhub.example.com/login/abcdefghijklmnopqrstuvwxyz"
	var buf bytes.Buffer
	if err := renderQR(&buf, link); err != nil {
		t.Fatal(err)
	}
	code, err := qr.Encode(link, qr.L)
	if err != nil {
		t.Fatal(err)
	}
	width := code.Size + 2*qrQuiet
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != (width+1)/2 {
		t.Fatalf("%d lines, want %d", len(lines), (width+1)/2)
	}
	dark := func(x, y int) bool {
		return x >= 0 && y >= 0 && x < code.Size && y < code.Size && code.Black(x, y)
	}
	mismatches := 0
	for i, line := range lines {
		row := []rune(line)
		if len(row) != width {
			t.Fatalf("line %d is %d wide, want %d", i, len(row), width)
		}
		for j, ch := range row {
			var topLight, bottomLight bool
			switch ch {
			case '█':
				topLight, bottomLight = true, true
			case '▀':
				topLight = true
			case '▄':
				bottomLight = true
			case ' ':
			default:
				t.Fatalf("line %d has %q", i, ch)
			}
			x, y := j-qrQuiet, 2*i-qrQuiet
			if topLight == dark(x, y) {
				mismatches++
			}
			if bottomLight == dark(x, y+1) {
				mismatches++
			}
		}
	}
	if mismatches != 0 {
		t.Errorf("%d modules differ from the encoder", mismatches)
	}
	// The quiet zone is light: the first line and each line's first two
	// characters are full blocks.
	if lines[0] != strings.Repeat("█", width) || !strings.HasPrefix(lines[3], "██") {
		t.Errorf("quiet zone not filled:\n%s", buf.String())
	}
}
