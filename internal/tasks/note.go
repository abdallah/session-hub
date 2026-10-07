// Package tasks holds task logic that needs no store: parsing the notes
// agents write with `worklog note`.
package tasks

import (
	"regexp"
	"strings"
)

// RefKind says what a note's ref names.
type RefKind string

const (
	RefNone   RefKind = ""
	RefTicket RefKind = "ticket"
	RefMR     RefKind = "mr"
)

// maxTitleRunes matches the store's task title limit.
const maxTitleRunes = 200

// Note is a parsed note: the task title, the ref it names, and whether it
// finishes the work.
type Note struct {
	Title  string
	Ref    string
	Kind   RefKind
	Finish bool
}

var (
	// finishRE matches a finish word at the start, with the separator after it.
	finishRE = regexp.MustCompile(`(?i)^(?:done:|done\s|finished\b|closed\b|close\s)\s*:?\s*`)
	// ticketRE matches a ticket ref, in optional parentheses.
	ticketRE = regexp.MustCompile(`(?i)\(?\b(systemsdev-\d+)\b\)?`)
	// mrRE matches an MR ref at a word start, with an optional "MR " before it.
	mrRE = regexp.MustCompile(`(?i)(?:^|\s)\(?(?:MR\s+)?!(\d+)\b\)?`)
)

// trimSet is what Parse trims from both ends of a title.
const trimSet = " \t:,-—"

// Parse reads a note. A ticket ref wins over an MR ref; the ref is cut from
// the title. A title left empty becomes the ref.
func Parse(text string) Note {
	var n Note
	s := strings.TrimSpace(text)
	if loc := finishRE.FindStringIndex(s); loc != nil {
		n.Finish = true
		s = s[loc[1]:]
	} else if strings.EqualFold(s, "done") || strings.EqualFold(s, "finished") || strings.EqualFold(s, "closed") {
		return Note{Finish: true}
	}
	if m := ticketRE.FindStringSubmatchIndex(s); m != nil {
		n.Ref, n.Kind = strings.ToLower(s[m[2]:m[3]]), RefTicket
		s = s[:m[0]] + " " + s[m[1]:]
	} else if m := mrRE.FindStringSubmatchIndex(s); m != nil {
		n.Ref, n.Kind = "!"+s[m[2]:m[3]], RefMR
		s = s[:m[0]] + " " + s[m[1]:]
	}
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, trimSet)
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") && strings.Count(s, "(") == 1 {
		s = strings.Trim(s[1:len(s)-1], trimSet)
	}
	if r := []rune(s); len(r) > maxTitleRunes {
		s = string(r[:maxTitleRunes])
	}
	if s == "" {
		s = n.Ref
	}
	n.Title = s
	return n
}
