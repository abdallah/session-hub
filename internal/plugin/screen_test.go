package plugin

import "testing"

func TestScreen(t *testing.T) {
	var s screen
	if got := string(s.input([]byte("a\x1b[8;3;40tb"))); got != "ab" {
		t.Fatalf("input = %q, want the size report stripped", got)
	}
	if got := s.rows.Load(); got != 3 {
		t.Fatalf("rows = %d, want 3", got)
	}
	// Cropped to the last 3 non-blank lines.
	d, ok := s.draw("1\r\n2\r\n3\r\n4\r\n\x1b[0m \r\n")
	if want := drawFrame("", "2\n3\n4"); !ok || d != want {
		t.Errorf("draw = %q, want %q", d, want)
	}
	if _, ok := s.draw("1\r\n2\r\n3\r\n4"); ok {
		t.Error("same cropped frame drew again")
	}
	// A new size redraws in full.
	s.input([]byte("\x1b[8;2;40t"))
	if d, ok := s.draw("1\r\n2\r\n3\r\n4"); !ok || d != drawFrame("", "3\n4") {
		t.Errorf("after resize draw = %q", d)
	}
	// No report: frames pass through uncropped.
	var u screen
	if d, _ := u.draw("1\r\n\r\n"); d != drawFrame("", "1\r\n\r\n") {
		t.Errorf("unsized draw = %q", d)
	}
}
