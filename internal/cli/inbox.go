package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/resume"
)

// inboxAPI is the part of *client.Client that sessionhub inbox uses.
type inboxAPI interface {
	Inbox(ctx context.Context) (api.Inbox, error)
	DismissInbox(ctx context.Context, id string, since time.Time) error
	SnoozeInbox(ctx context.Context, id string, since, until time.Time) error
}

const inboxUsage = `usage:
  sessionhub inbox [--json | --watch]
  sessionhub inbox dismiss <id|prefix>
  sessionhub inbox snooze <id|prefix> <1h|4h|tomorrow|duration>`

// inboxHeadings are the group headings. Items arrive in group order.
var inboxHeadings = map[string]string{
	api.InboxBlocked:  "Blocked",
	api.InboxWaiting:  "Waiting on you",
	api.InboxFinished: "Finished",
}

// RunInbox implements `sessionhub inbox`, `sessionhub inbox dismiss`, and
// `sessionhub inbox snooze`.
func RunInbox(ctx context.Context, args []string) error {
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	// Only the watch view jumps, so only it builds a Jumper.
	if wantsWatch(args) {
		if j, err := resume.NewJumper(); err == nil {
			e.jump = j
		} else {
			e.jump = failedJumper{err}
		}
	}
	return e.inboxCmd(ctx, args)
}

// failedJumper carries NewJumper's error to the status line on Enter.
type failedJumper struct{ err error }

func (f failedJumper) Jump(context.Context, api.Session) (string, error) {
	return "", fmt.Errorf("jump unavailable: %w; run: sessionhub resume <id>", f.err)
}

// wantsWatch reports whether args ask for sessionhub inbox --watch.
func wantsWatch(args []string) bool {
	for _, a := range args {
		if f := strings.TrimLeft(a, "-"); a != f && (f == "watch" || strings.HasPrefix(f, "watch=")) {
			return true
		}
	}
	return false
}

// Compile-time check that resume's Jumper is what sessionhub inbox --watch calls.
var _ jumper = (*resume.Jumper)(nil)

// maxSnooze is the longest snooze the server accepts.
const maxSnooze = 7 * 24 * time.Hour

// maxCLISnooze is the longest snooze the CLI sends. It stays a minute under
// maxSnooze so a client clock slightly ahead of the server's doesn't get a 400.
const maxCLISnooze = maxSnooze - time.Minute

// minInboxPrefix matches the shortest prefix sessionhub show accepts.
const minInboxPrefix = 4

func (e *env) inboxCmd(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "dismiss":
			return e.inboxDismiss(ctx, args[1:])
		case "snooze":
			return e.inboxSnooze(ctx, args[1:])
		}
	}
	return e.inboxList(ctx, args)
}

func (e *env) inboxDismiss(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("inbox dismiss: want one ID or prefix\n%s", inboxUsage)
	}
	it, err := e.inboxItem(ctx, args[0])
	if err != nil {
		return fmt.Errorf("inbox dismiss: %w", err)
	}
	if err := e.inbox.DismissInbox(ctx, it.Session.ID, it.Since); err != nil {
		return fmt.Errorf("inbox dismiss: %w", err)
	}
	fmt.Fprintf(e.out, "dismissed %s (%s)\n", shortID(it.Session.ID), clean(title(it.Session), termtext.TitleWidth))
	return nil
}

func (e *env) inboxSnooze(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("inbox snooze: want an ID or prefix and a time\n%s", inboxUsage)
	}
	until, err := parseSnooze(args[1], e.now())
	if err != nil {
		return fmt.Errorf("inbox snooze: %w", err)
	}
	it, err := e.inboxItem(ctx, args[0])
	if err != nil {
		return fmt.Errorf("inbox snooze: %w", err)
	}
	if err := e.inbox.SnoozeInbox(ctx, it.Session.ID, it.Since, until); err != nil {
		return fmt.Errorf("inbox snooze: %w", err)
	}
	fmt.Fprintf(e.out, "snoozed %s (%s) until %s\n", shortID(it.Session.ID),
		clean(title(it.Session), termtext.TitleWidth), until.Format("Mon 2 Jan 15:04"))
	return nil
}

// inboxItem reads the inbox and finds target in it, so the request carries
// the since the inbox shows now.
func (e *env) inboxItem(ctx context.Context, target string) (api.InboxItem, error) {
	in, err := e.inbox.Inbox(ctx)
	if err != nil {
		return api.InboxItem{}, err
	}
	return findInboxItem(in.Items, target)
}

// findInboxItem resolves a session ID, or a prefix of at least
// minInboxPrefix characters, against the inbox. An exact ID wins.
func findInboxItem(items []api.InboxItem, target string) (api.InboxItem, error) {
	if len(target) < minInboxPrefix {
		return api.InboxItem{}, fmt.Errorf("id prefix %q is shorter than %d characters", clean(target, 0), minInboxPrefix)
	}
	var hits []api.InboxItem
	for _, it := range items {
		if it.Session.ID == target {
			return it, nil
		}
		if strings.HasPrefix(it.Session.ID, target) {
			hits = append(hits, it)
		}
	}
	switch len(hits) {
	case 0:
		return api.InboxItem{}, fmt.Errorf("%s is not in the inbox; see `sessionhub inbox`", clean(target, 0))
	case 1:
		return hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, it := range hits {
		ids[i] = clean(it.Session.ID, 0)
	}
	return api.InboxItem{}, fmt.Errorf("id prefix %q matches %d inbox items: %s", clean(target, 0), len(hits), strings.Join(ids, ", "))
}

// parseSnooze turns 1h, 4h, a Go duration just under 7 days, or "tomorrow"
// (9:00 the next calendar day in now's time zone) into the snooze end.
func parseSnooze(arg string, now time.Time) (time.Time, error) {
	if arg == "tomorrow" {
		y, m, d := now.Date()
		return time.Date(y, m, d+1, 9, 0, 0, 0, now.Location()), nil
	}
	d, err := time.ParseDuration(arg)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q: want 1h, 4h, tomorrow, or a duration such as 90m", arg)
	}
	if d <= 0 || d > maxCLISnooze {
		return time.Time{}, fmt.Errorf("%s: want more than 0 and less than 7 days (at most 167h59m)", arg)
	}
	return now.Add(d), nil
}

func (e *env) inboxList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessionhub inbox", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print the server response")
	watch := fs.Bool("watch", false, "keep a keyboard-driven list on screen")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return fmt.Errorf("inbox: unexpected arguments\n%s", inboxUsage)
	}
	if *watch {
		if *asJSON {
			return fmt.Errorf("inbox: --watch and --json don't go together\n%s", inboxUsage)
		}
		return e.runWatch(ctx)
	}
	in, err := e.inbox.Inbox(ctx)
	if err != nil {
		return fmt.Errorf("inbox: %w", err)
	}
	if *asJSON {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(e.out, "%s\n", b)
		return err
	}
	if len(in.Items) == 0 {
		fmt.Fprintln(e.out, "nothing needs you")
		return nil
	}
	renderInbox(e.out, in.Items, e.now(), e.width())
	return nil
}

// renderInbox prints a heading with a count per group, then one line per
// item. Items arrive sorted by group, then since.
func renderInbox(w io.Writer, items []api.InboxItem, now time.Time, width int) {
	lines, _ := inboxLines(items, now, width, "")
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// inboxLines is the grouped list shared by sessionhub inbox and sessionhub inbox --watch:
// per group a heading with a count and one line per item, with a blank line
// between groups. The row of the item with session ID selected starts with
// "> " instead of two spaces, and its index in the result is the second
// return value (-1 when none is selected).
func inboxLines(items []api.InboxItem, now time.Time, width int, selected string) ([]string, int) {
	var lines []string
	cur := -1
	for i := 0; i < len(items); {
		j := i
		for j < len(items) && items[j].Group == items[i].Group {
			j++
		}
		if i > 0 {
			lines = append(lines, "")
		}
		heading := inboxHeadings[items[i].Group]
		if heading == "" {
			heading = clean(items[i].Group, 0)
		}
		lines = append(lines, cut(fmt.Sprintf("%s (%d)", heading, j-i), width))
		for _, it := range items[i:j] {
			line := inboxLine(it, now, width)
			if selected != "" && it.Session.ID == selected {
				cur = len(lines)
				line = "> " + strings.TrimPrefix(line, "  ")
			}
			lines = append(lines, line)
		}
		i = j
	}
	return lines, cur
}

// inboxLine is one item: ID prefix, title, machine, age of since, "stale"
// when the session is stale, and the waiting_on items or the recap, cut to
// width runes. Each field is cleaned first, so the two-space separators
// survive.
func inboxLine(it api.InboxItem, now time.Time, width int) string {
	s := it.Session
	parts := []string{shortID(s.ID), clean(title(s), termtext.TitleWidth), clean(s.Machine, 0), age(now, it.Since)}
	if s.Status == api.StatusStale {
		parts = append(parts, "stale")
	}
	detail := s.Recap
	if len(it.WaitingOn) > 0 {
		detail = strings.Join(it.WaitingOn, "; ")
	}
	if p := it.Permission; p != nil {
		detail = "asks to use " + p.ToolName + ": " + api.PermissionInputText(p.ToolInput)
	}
	cutMark := ""
	if p := it.Permission; p != nil && p.Truncated {
		// After the cut to width, so a long command never hides it.
		cutMark = " (cut)"
	}
	if d := clean(detail, 0); d != "" {
		parts = append(parts, d)
	}
	line := "  " + strings.Join(parts, "  ")
	if cutMark == "" || width <= 0 {
		return cut(line, width) + cutMark
	}
	return cut(line, max(width-len(cutMark), 1)) + cutMark
}

// cut keeps s within width runes, ending a cut line with "…".
func cut(s string, width int) string {
	r := []rune(s)
	if width <= 0 || len(r) <= width {
		return s
	}
	return string(r[:width-1]) + "…"
}

// Compile-time check that the real client satisfies inboxAPI.
var _ inboxAPI = (*client.Client)(nil)
