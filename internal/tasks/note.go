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
	finishRE = regexp.MustCompile(`(?i)^(?:done[:.]|done\s|finished\s|finished$)\s*:?\s*`)
	// closeRE matches close or closed at the start. They finish only a note
	// with a ref, so "close look at the logs" is not a finish.
	closeRE = regexp.MustCompile(`(?i)^closed?\s+:?\s*`)
	// ticketRE matches a ticket ref, in optional parentheses.
	ticketRE = regexp.MustCompile(`(?i)\(?\b(systemsdev-\d+)\b\)?`)
	// bareTicketRE matches a ticket number without its prefix: five digits
	// at the start of the note or alone in parentheses.
	bareTicketRE = regexp.MustCompile(`^(\d{5})\b|\((\d{5})\)`)
	// mrRE matches an MR ref at a word start, with an optional "MR " before it.
	mrRE = regexp.MustCompile(`(?i)(?:^|\s)\(?(?:MR\s+)?!(\d+)\b\)?`)
)

// trimSet is what Parse trims from both ends of a title.
const trimSet = " \t:,-—"

// Parse reads a note. A ticket ref wins over an MR ref; the ref is cut from
// the title. A title left empty becomes the ref.
func Parse(text string) Note {
	s := strings.TrimSpace(text)
	if strings.EqualFold(s, "done") {
		return Note{Finish: true}
	}
	if loc := finishRE.FindStringIndex(s); loc != nil {
		n := parseRest(s[loc[1]:])
		n.Finish = true
		return n
	}
	if loc := closeRE.FindStringIndex(s); loc != nil {
		if n := parseRest(s[loc[1]:]); n.Ref != "" {
			n.Finish = true
			return n
		}
	}
	return parseRest(s)
}

// parseRest finds the ref in s and makes the title.
func parseRest(s string) Note {
	var n Note
	if m := ticketRE.FindStringSubmatchIndex(s); m != nil {
		n.Ref, n.Kind = strings.ToLower(s[m[2]:m[3]]), RefTicket
		s = s[:m[0]] + " " + s[m[1]:]
	} else if m := bareTicketRE.FindStringSubmatchIndex(s); m != nil {
		var num string
		if m[2] >= 0 {
			num = s[m[2]:m[3]]
		} else {
			num = s[m[4]:m[5]]
		}
		n.Ref, n.Kind = "systemsdev-"+num, RefTicket
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
