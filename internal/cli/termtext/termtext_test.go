package termtext

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestClean(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"plain title", 0, "plain title"},
		{"  two\n\tlines  \r\n", 0, "two lines"},
		{"red \x1b[31mtext\x1b[0m", 0, "red [31mtext[0m"},
		{"bell\a and nul\x00", 0, "bell and nul"},
		{"c1 \u009b31m", 0, "c1 31m"},
		{"del\x7f", 0, "del"},
		{"bidi ‮evil‬ ⁦x⁩", 0, "bidi evil x"},
		{"w1:p1\x1b]0;pwned\a", 0, "w1:p1]0;pwned"},
		{"émoji 🙂 kept", 0, "émoji 🙂 kept"},
		{"abcdef", 6, "abcdef"},
		{"abcdefg", 6, "abcde…"},
		{"ééééééé", 6, "ééééé…"},
		{"a\x1b\x1b\x1bbcdef", 6, "abcdef"},
		{"", 5, ""},
		{"\n\t ", 5, ""},
	}
	fails := 0
	for _, c := range cases {
		if got := Clean(c.in, c.max); got != c.want {
			t.Errorf("Clean(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
			fails++
		}
	}
	t.Logf("clean cases: %d, failures: %d", len(cases), fails)
}

// Every rune 0..0x10FFFF, alone and inside text: the result never holds a
// control or bidi rune, never a newline, and never exceeds the width.
func TestCleanNeverPassesControls(t *testing.T) {
	bad := 0
	tail := "b" + strings.Repeat("x", 70)
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		got := Clean("a"+string(r)+tail, TitleWidth)
		if utf8.RuneCountInString(got) > TitleWidth {
			bad++
			continue
		}
		for _, g := range got {
			if unicode.IsControl(g) || isBidi(g) || g == '\n' {
				bad++
				break
			}
		}
	}
	if bad != 0 {
		t.Errorf("%d runes got through", bad)
	}
}
