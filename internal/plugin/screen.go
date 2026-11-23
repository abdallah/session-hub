package plugin

import (
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
)

// sizeReport is the xterm resize sequence, "\x1b[8;<rows>;<cols>t", which the
// guest CLI sends in its input at start and on each resize.
var sizeReport = regexp.MustCompile(`\x1b\[8;(\d+);(\d+)t`)

var ansiSeq = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

// screen fits a mirror's frames to the guest's terminal: once the guest
// reports its height, each frame is cropped to its last non-blank rows that
// fit, so the prompt stays on screen. Width is not cropped: drawFrame turns
// autowrap off, so long lines are cut at the guest's right edge.
//
// shortcut: a size report split across two input reads is passed through as
// keys; the guest sends it in its own request, so it arrives whole in practice.
type screen struct {
	rows     atomic.Int32 // the guest's height, 0 until it reports one
	last     string
	lastRows int32
}

// input strips size reports from guest input b and records the last one.
func (s *screen) input(b []byte) []byte {
	for _, m := range sizeReport.FindAllSubmatch(b, -1) {
		if n, err := strconv.Atoi(string(m[1])); err == nil && n > 0 && n < 1000 {
			s.rows.Store(int32(n))
		}
	}
	return sizeReport.ReplaceAll(b, nil)
}

// draw returns the bytes that move the guest's screen from the last frame to
// frame, and false when nothing changed. A new guest size redraws in full.
func (s *screen) draw(frame string) (string, bool) {
	rows := s.rows.Load()
	if rows > 0 {
		lines := frameLines(frame)
		for len(lines) > 1 && strings.TrimSpace(ansiSeq.ReplaceAllString(lines[len(lines)-1], "")) == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > int(rows) {
			lines = lines[len(lines)-int(rows):]
		}
		frame = strings.Join(lines, "\n")
	}
	if frame == s.last && rows == s.lastRows {
		return "", false
	}
	prev := s.last
	if rows != s.lastRows {
		prev = ""
	}
	s.last, s.lastRows = frame, rows
	return drawFrame(prev, frame), true
}
