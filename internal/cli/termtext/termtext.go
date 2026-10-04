// Package termtext makes server and herdr strings safe to print on one
// terminal line. Titles, reports, cwds, and pane IDs come from other
// machines and other processes; an escape sequence in one would reach the
// terminal as a command, and a newline would break the table.
package termtext

import (
	"strings"
	"unicode"
)

// TitleWidth is how many runes `sessionhub ls` and the resume picker show of a title.
const TitleWidth = 60

// Clean returns s with control characters removed, whitespace runs (including
// newlines and tabs) folded to one space, and the ends trimmed. When max > 0
// and the result is longer than max runes, it keeps max-1 runes and adds "…".
//
// Removed: C0 and C1 control characters, DEL, and the Unicode bidirectional
// controls, which can make printed text read in a different order than it
// runs.
func Clean(s string, max int) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r), isBidi(r):
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	out := b.String()
	if max <= 0 {
		return out
	}
	rs := []rune(out)
	if len(rs) <= max {
		return out
	}
	return string(rs[:max-1]) + "…"
}

func isBidi(r rune) bool {
	return r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069 || r == 0x200E || r == 0x200F || r == 0x061C
}
