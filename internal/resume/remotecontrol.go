package resume

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
)

const remoteControlUsage = "usage: sessionhub remote-control <id-prefix>"

// RunRemoteControl implements `sessionhub remote-control <id-prefix>`: it turns on
// Claude Code Remote Control for a running session by sending /remote-control
// to its pane.
func RunRemoteControl(ctx context.Context, args []string) error {
	return runRemoteControl(ctx, args, os.Stdout)
}

// runRemoteControl is RunRemoteControl with its stdout injected. -h and
// --help print the usage and succeed; they are never a session ID.
func runRemoteControl(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stdout, remoteControlUsage)
		return nil
	}
	if len(args) != 1 {
		return errors.New(remoteControlUsage)
	}
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	return e.remoteControlRun(ctx, args[0])
}

const (
	// rcPollEvery and rcPollFor bound the wait for Claude to print the URL.
	rcPollEvery = 500 * time.Millisecond
	rcPollFor   = 10 * time.Second
	// ctlPollEvery and ctlPollFor bound the wait for another machine's
	// watcher to answer a request.
	ctlPollEvery = 2 * time.Second
	ctlPollFor   = 2 * time.Minute
	// rcLines is how much of the pane's recent text to read.
	rcLines = 120
	// rcCommand is the slash command sessionhub sends.
	rcCommand = "/remote-control"
)

// rcURLPattern is the only text sessionhub prints from the pane. Claude prints
// "/remote-control is active · ... or at https://claude.ai/code/session_...".
var rcURLPattern = regexp.MustCompile(`https://claude\.ai/code/session_[A-Za-z0-9_-]+`)

// remoteControlURLs returns every Remote Control URL in pane text. The text
// is cleaned first, so a control character inside it can't reach the
// terminal; the pattern then stops at the first character outside the URL.
func remoteControlURLs(text string) []string {
	return rcURLPattern.FindAllString(termtext.Clean(spaceControls(text), 0), -1)
}

// spaceControls replaces control and bidirectional-control characters with a
// space. termtext.Clean would delete them, which could glue the text on both
// sides into one longer ID ("session_a1\x07abc"); a space ends the URL.
func spaceControls(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069 || r == 0x200E || r == 0x200F || r == 0x061C {
			return ' '
		}
		return r
	}, s)
}

// validPaneID matches herdr's pane ID shape ("w1:p2"). sessionhub refuses any other
// value before it prints the ID in a command or sends it to herdr.
var validPaneID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,63}$`)

// rcDialogTail is how many of the last screen lines the dialog must sit in.
// The dialog is drawn at the bottom of the screen, its footer on the last line.
const rcDialogTail = 12

// visibleDialog reports whether the visible screen shows the dialog Claude
// opens when Remote Control is already on, and returns its URL. text must be
// the pane's visible screen, not scrollback: quoted output can hold every
// string of the dialog. All of these must hold, in the last rcDialogTail
// non-blank lines: "Disconnect this session", then "Show QR code", then the
// footer "Esc to continue" as the last line, and a session URL that matches
// the strict pattern.
func visibleDialog(text string) (url string, ok bool) {
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(spaceControls(l)); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > rcDialogTail {
		lines = lines[len(lines)-rcDialogTail:]
	}
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "Esc to continue") {
		return "", false
	}
	disc, qr := -1, -1
	for i, l := range lines {
		switch {
		case disc < 0 && strings.Contains(l, "Disconnect this session"):
			disc = i
		case disc >= 0 && qr < 0 && strings.Contains(l, "Show QR code"):
			qr = i
		}
	}
	if disc < 0 || qr < 0 {
		return "", false
	}
	urls := remoteControlURLs(strings.Join(lines, "\n"))
	if len(urls) == 0 {
		return "", false
	}
	return urls[len(urls)-1], true
}

// newRemoteControlURL returns the last URL in after that is not in before, or
// "". A URL from an earlier Remote Control run, still on screen, is not new.
func newRemoteControlURL(before, after string) string {
	old := map[string]bool{}
	for _, u := range remoteControlURLs(before) {
		old[u] = true
	}
	found := ""
	for _, u := range remoteControlURLs(after) {
		if !old[u] {
			found = u
		}
	}
	return found
}

func (e *env) remoteControlRun(ctx context.Context, prefix string) error {
	d, err := e.api.GetSession(ctx, prefix)
	if err != nil {
		return err
	}
	return e.remoteControlSession(ctx, d.Session)
}

func (e *env) remoteControlSession(ctx context.Context, s api.Session) error {
	if !validID(s.ID) {
		return fmt.Errorf("refusing session ID %q: expected letters, digits, and hyphens, not starting with a hyphen", s.ID)
	}
	if s.HerdrPane != "" && !validPaneID.MatchString(s.HerdrPane) {
		return fmt.Errorf("refusing pane ID %q: not a herdr pane ID", clean(s.HerdrPane))
	}
	if e.cfg.Machine != "" && s.Machine == e.cfg.Machine {
		return e.remoteControlLocal(ctx, s)
	}
	return e.remoteControlRemote(ctx, s)
}

// notRunning is the error for a session with no live Claude in a pane on
// this machine. herdr decides, not the server's status: the server can lag a
// pane that still runs the session.
func notRunning(s api.Session) error {
	return fmt.Errorf("session is not running; start it with: sessionhub resume --remote-control %s", s.ID)
}

// remoteControlLocal turns on Remote Control for a session on this machine.
// It never resumes: a session no pane runs is "not running".
func (e *env) remoteControlLocal(ctx context.Context, s api.Session) error {
	o := ControlOptions{Socket: e.socket, PollEvery: e.pollEvery, PollFor: e.pollFor}
	return e.reportControl(s, o, Control(ctx, s, o))
}

// reportControl prints what Control did, as the CLI prints it, or returns
// the error for it.
func (e *env) reportControl(s api.Session, o ControlOptions, r ControlResult) error {
	switch r.Outcome {
	case OutcomeLinked:
		fmt.Fprintf(e.out, "Remote Control is on for session %s:\n%s\n", shortID(s.ID), r.URL)
	case OutcomeAlreadyOn:
		fmt.Fprintf(e.out, "Remote Control is already on for session %s:\n%s\n", shortID(s.ID), r.URL)
		if r.DialogLeftOpen != "" {
			fmt.Fprintln(e.out, r.DialogLeftOpen)
		}
	case OutcomeNoLink:
		_, total := pollBounds(o)
		fmt.Fprintf(e.out, "Sent %s to pane %s, but no Remote Control URL appeared within %s. Check the pane or the Claude app.\n",
			rcCommand, clean(r.Pane), total)
	case OutcomeBlocked:
		return errors.New("the session is waiting on a prompt in its pane; answer it, then run this again")
	case OutcomeDialogOpen:
		return errors.New("a Remote Control dialog is open in the pane; press Esc there, then run this again")
	case OutcomeNotRunning:
		return notRunning(s)
	case OutcomeError:
		if r.Err != nil {
			return r.Err
		}
		return fmt.Errorf("turning on Remote Control for session %s failed", shortID(s.ID))
	default:
		// Control without Resume never returns these; never exit 0 silently.
		return fmt.Errorf("unexpected result %q while turning on Remote Control for session %s", clean(string(r.Outcome)), shortID(s.ID))
	}
	return nil
}

// remoteControlSSH prints the commands that turn Remote Control on for a
// session on another machine, after the line why. It never runs them.
func (e *env) remoteControlSSH(ctx context.Context, s api.Session, why string) error {
	sshHost, herdrHost := e.hosts(ctx, s)
	if !validHost(sshHost) || !validHost(herdrHost) {
		return fmt.Errorf("refusing to print commands for machine %q: its SSH host %q or herdr host %q is empty or starts with a hyphen", s.Machine, sshHost, herdrHost)
	}
	fmt.Fprintln(e.out, why)
	fmt.Fprintf(e.out, "Session %s runs on machine %s, not on this machine. To turn on Remote Control:\n\n", shortID(s.ID), strconv.Quote(s.Machine))
	fmt.Fprintf(e.out, "ssh -t %s %s\n", shellQuote(sshHost), shellQuote(remoteHub+" remote-control "+s.ID))
	if s.HerdrPane == "" {
		return nil
	}
	if label := matchSaved(e.savedMachines(ctx), herdrHost); validHost(label) {
		// Comments never carry session data.
		fmt.Fprintf(e.out, "herdr --machine %s agent prompt %s %s     # saved herdr machine; sends the command without checking the pane\n",
			shellQuote(label), shellQuote(s.HerdrPane), rcCommand)
	}
	return nil
}

// remoteControlRemote asks the sessionhub to have the session's machine turn
// Remote Control on, and waits for the answer. When the request can't be
// made, or the machine doesn't answer before it expires, it prints the SSH
// commands instead.
func (e *env) remoteControlRemote(ctx context.Context, s api.Session) error {
	machine := strconv.Quote(s.Machine)
	req, err := e.api.CreateControl(ctx, s.ID)
	if err != nil {
		return e.remoteControlSSH(ctx, s, fmt.Sprintf("Could not ask machine %s to turn on Remote Control: %s", machine, clean(err.Error())))
	}
	fmt.Fprintf(e.out, "Asked machine %s to turn on Remote Control for session %s. Waiting for it...\n", machine, shortID(s.ID))
	r, ok := e.awaitControl(ctx, s.ID, req.ID)
	switch {
	case !ok || r.State == api.ControlExpired:
		return e.remoteControlSSH(ctx, s, fmt.Sprintf("Machine %s didn't respond in time.", machine))
	case r.State == api.ControlFailed && r.Detail == "":
		return fmt.Errorf("machine %s could not turn on Remote Control", machine)
	case r.State == api.ControlFailed:
		return fmt.Errorf("machine %s could not turn on Remote Control: %s", machine, clean(r.Detail))
	case rcURLExact.MatchString(r.URL):
		fmt.Fprintf(e.out, "Remote Control is on for session %s:\n%s\n", shortID(s.ID), r.URL)
		if r.Detail != "" {
			fmt.Fprintf(e.out, "(%s)\n", clean(r.Detail))
		}
	case r.Detail == "":
		fmt.Fprintf(e.out, "Machine %s sent /remote-control, but no link appeared. Check the Claude app.\n", machine)
	default:
		fmt.Fprintf(e.out, "Machine %s sent /remote-control, but no link appeared (%s). Check the Claude app.\n", machine, clean(r.Detail))
	}
	return nil
}

// rcURLExact is the only link sessionhub prints from the server: all of it must be
// a Remote Control link, or sessionhub prints none.
var rcURLExact = regexp.MustCompile(`^https://claude\.ai/code/session_[A-Za-z0-9_-]+$`)

// awaitControl reads the session every ctlEvery until request id is done,
// failed, or expired, for up to ctlFor. ok is false when time ran out first.
func (e *env) awaitControl(ctx context.Context, sessionID, id string) (api.ControlRequest, bool) {
	every, total := e.ctlEvery, e.ctlFor
	if every <= 0 {
		every = ctlPollEvery
	}
	if total <= 0 {
		total = ctlPollFor
	}
	deadline := time.Now().Add(total)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return api.ControlRequest{}, false
		case <-time.After(every):
		}
		d, err := e.api.GetSession(ctx, sessionID)
		if err != nil {
			continue // a failed read is like no answer yet
		}
		if r := d.RemoteControl; r != nil && r.ID == id {
			switch r.State {
			case api.ControlDone, api.ControlFailed, api.ControlExpired:
				return *r, true
			}
		}
	}
	return api.ControlRequest{}, false
}
