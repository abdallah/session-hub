package resume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/herdr"
)

// Outcome names what Control did.
type Outcome string

const (
	OutcomeLinked     Outcome = "linked"      // sent /remote-control; a new link appeared
	OutcomeAlreadyOn  Outcome = "already_on"  // Claude showed its already-on dialog; the link is from it
	OutcomeNoLink     Outcome = "no_link"     // sent /remote-control; no link within PollFor
	OutcomeResumed    Outcome = "resumed"     // started claude --resume <id> --remote-control in a new workspace; herdr detects Claude there
	OutcomeBlocked    Outcome = "blocked"     // herdr refused: Claude waits on a prompt; nothing was sent
	OutcomeDialogOpen Outcome = "dialog_open" // a Remote Control dialog was already open; nothing was sent
	OutcomeNotRunning Outcome = "not_running" // no pane runs the session (only without Resume)
	OutcomeLooksLive  Outcome = "looks_live"  // the server says live or blocked, but no pane runs it; nothing started
	OutcomeNoDir      Outcome = "no_dir"      // the session's directory is missing; nothing created
	OutcomeError      Outcome = "error"       // a herdr call failed; Err says which
	// OutcomeJustResumed: KnownPane neither runs the session nor Claude
	// waiting to report it, and no other pane runs it; nothing started.
	OutcomeJustResumed Outcome = "just_resumed"
	// OutcomeRunning: StartResumed found a pane that already runs the
	// session; nothing started.
	OutcomeRunning Outcome = "running"
)

// ControlOptions configures Control.
type ControlOptions struct {
	// Socket is the herdr socket path.
	Socket string
	// Resume starts a session that no pane runs in a new herdr workspace.
	// Without it (`sessionhub remote-control`), that session is OutcomeNotRunning.
	Resume bool
	// PollEvery and PollFor bound the wait for the link. Zero means 500 ms
	// and 10 s.
	PollEvery, PollFor time.Duration
	// KnownPane is a pane the caller started the session in moments ago
	// (a ControlResult.Pane from OutcomeResumed). herdr reports the session
	// in a pane only some seconds after Claude starts, so Control never
	// resumes the session again: it sends /remote-control to KnownPane when
	// Claude runs there and reports no other session, and otherwise, when no
	// pane runs the session, returns OutcomeJustResumed.
	KnownPane string
}

// ControlResult is what Control did.
type ControlResult struct {
	Outcome Outcome
	// URL is the Remote Control link, or "" when none was seen.
	URL string
	// Pane got /remote-control, or is the new workspace's pane.
	Pane string
	// Workspace is the workspace Control created, if it created one.
	Workspace string
	// DialogLeftOpen is "" when Control closed the already-on dialog (or it
	// closed on its own), else why it stayed open, as the CLI prints it.
	DialogLeftOpen string
	// Err says what failed, for OutcomeError.
	Err error
}

// maxLabelTitle caps the title in a new workspace's label.
const maxLabelTitle = 40

// Control turns on Remote Control for session s in this machine's herdr.
//
// When herdr detects Claude running the session in a pane (the recorded one,
// or with o.Resume any pane in a fresh snapshot), Control sends
// /remote-control there and waits for the link. Otherwise, with o.Resume, it
// starts `claude --resume <id> --remote-control` in a new workspace in the
// session's directory, without focus. It starts nothing when the server
// reports the session live or blocked: it may run where herdr can't see, and
// a second copy would write to the same transcript.
func Control(ctx context.Context, s api.Session, o ControlOptions) ControlResult {
	if !validID(s.ID) {
		return failure(fmt.Errorf("refusing session ID %q: expected letters, digits, and hyphens, not starting with a hyphen", s.ID))
	}
	if s.HerdrPane != "" && !validPaneID.MatchString(s.HerdrPane) {
		return failure(fmt.Errorf("refusing pane ID %q: not a herdr pane ID", clean(s.HerdrPane)))
	}
	if o.KnownPane != "" && !validPaneID.MatchString(o.KnownPane) {
		return failure(fmt.Errorf("refusing known pane ID %q: not a herdr pane ID", clean(o.KnownPane)))
	}
	// Pane IDs belong to one herdr server: another server's pane is not ours.
	sameServer := s.HerdrSession == "" || s.HerdrSession == herdr.SessionName(o.Socket)
	if !o.Resume && o.KnownPane == "" && (s.HerdrPane == "" || !sameServer) {
		return ControlResult{Outcome: OutcomeNotRunning}
	}
	h, err := herdr.Dial(o.Socket)
	if err != nil {
		if !o.Resume {
			return ControlResult{Outcome: OutcomeNotRunning}
		}
		return failure(fmt.Errorf("herdr is not running: %w", err))
	}
	pane, err := findPane(h, s, o, sameServer)
	switch {
	case err != nil:
		return failure(err)
	case pane != "":
		return injectAndWait(ctx, h, pane, s.ID, o)
	case o.KnownPane != "":
		return controlKnownPane(ctx, h, s.ID, o)
	case !o.Resume:
		return ControlResult{Outcome: OutcomeNotRunning}
	case isLive(s):
		return ControlResult{Outcome: OutcomeLooksLive}
	}
	return resumeInWorkspace(ctx, h, s, o)
}

func failure(err error) ControlResult { return ControlResult{Outcome: OutcomeError, Err: err} }

// runs reports whether herdr detects Claude running session id in p.
func runs(p herdr.PaneInfo, id string) bool { return p.Agent == "claude" && p.SessionID() == id }

// findPane returns the pane where Claude runs session s: o.KnownPane first,
// then the recorded pane, then with o.Resume any pane in a fresh snapshot
// (the pane can move between heartbeats). "" means none. An error other than
// pane_not_found ends Control.
func findPane(h *herdr.Client, s api.Session, o ControlOptions, sameServer bool) (string, error) {
	var direct []string
	if o.KnownPane != "" {
		direct = append(direct, o.KnownPane)
	}
	if s.HerdrPane != "" && sameServer {
		direct = append(direct, s.HerdrPane)
	}
	for _, id := range direct {
		p, found, err := paneGet(h, id)
		switch {
		case err != nil:
			return "", err
		case found && runs(p, s.ID):
			return checkedPane(p.PaneID)
		}
	}
	if !o.Resume {
		return "", nil
	}
	snap, err := h.Snapshot()
	if err != nil {
		return "", fmt.Errorf("read herdr snapshot: %w", err)
	}
	for _, p := range snap.Panes {
		if runs(p, s.ID) {
			return checkedPane(p.PaneID)
		}
	}
	return "", nil
}

// paneGet looks up pane id. found is false when herdr has no such pane.
func paneGet(h *herdr.Client, id string) (p herdr.PaneInfo, found bool, err error) {
	p, err = h.PaneGet(id)
	var he *herdr.Error
	switch {
	case errors.As(err, &he) && he.Code == "pane_not_found":
		return p, false, nil
	case err != nil:
		return p, false, fmt.Errorf("look up pane %s: %w", clean(id), err)
	}
	return p, true, nil
}

// controlKnownPane handles a session that no pane runs, as far as herdr
// reports, but that the caller started in o.KnownPane moments ago. When
// Claude runs there and reports no session yet, or this one, it gets
// /remote-control. Anything else is OutcomeJustResumed: a pane at a shell or
// gone, or Claude on another session. It never starts the session again.
func controlKnownPane(ctx context.Context, h *herdr.Client, id string, o ControlOptions) ControlResult {
	p, found, err := paneGet(h, o.KnownPane)
	switch {
	case err != nil:
		return failure(err)
	case !found || p.Agent != "claude" || p.SessionID() != "" && p.SessionID() != id:
		return ControlResult{Outcome: OutcomeJustResumed, Pane: o.KnownPane}
	}
	pane, err := checkedPane(p.PaneID)
	if err != nil {
		return failure(err)
	}
	return injectAndWait(ctx, h, pane, id, o)
}

// checkedPane refuses a pane ID from herdr that isn't herdr's shape. It is an
// error, not "no pane": the session runs there, and skipping the pane would
// start a second copy.
func checkedPane(id string) (string, error) {
	if !validPaneID.MatchString(id) {
		return "", fmt.Errorf("refusing pane ID %q from herdr: not a herdr pane ID", clean(id))
	}
	return id, nil
}

func pollBounds(o ControlOptions) (every, total time.Duration) {
	every, total = o.PollEvery, o.PollFor
	if every <= 0 {
		every = rcPollEvery
	}
	if total <= 0 {
		total = rcPollFor
	}
	return every, total
}

// injectAndWait sends /remote-control to a pane where Claude runs the
// session and waits for the link. These are the CLI task's steps from
// remoteControlLocal, moved here: they return an Outcome instead of printing.
func injectAndWait(ctx context.Context, h *herdr.Client, pane, sessionID string, o ControlOptions) ControlResult {
	r := ControlResult{Pane: pane}
	fail := func(err error) ControlResult {
		r.Outcome, r.Err = OutcomeError, err
		return r
	}
	// Read first, so a URL from an earlier run is not mistaken for the new one.
	before, err := h.PaneRead(pane, herdr.SourceRecentUnwrapped, rcLines)
	if err != nil {
		return fail(fmt.Errorf("read pane %s: %w", clean(pane), err))
	}
	screen, err := h.PaneRead(pane, herdr.SourceVisible, 0)
	if err != nil {
		return fail(fmt.Errorf("read pane %s: %w", clean(pane), err))
	}
	if _, open := visibleDialog(screen); open {
		r.Outcome = OutcomeDialogOpen
		return r
	}
	if err := h.AgentPrompt(pane, rcCommand); err != nil {
		var blocked *herdr.BlockedError
		if errors.As(err, &blocked) {
			r.Outcome = OutcomeBlocked
			return r
		}
		return fail(fmt.Errorf("send %s to pane %s: %w", rcCommand, clean(pane), err))
	}
	every, total := pollBounds(o)
	deadline := time.Now().Add(total)
	for {
		select {
		case <-ctx.Done():
			// /remote-control went out; only the wait for its link ended.
			r.Outcome = OutcomeNoLink
			return r
		case <-time.After(every):
		}
		// A failed read is treated like no URL yet; the deadline ends the wait.
		after, err := h.PaneRead(pane, herdr.SourceRecentUnwrapped, rcLines)
		if err == nil {
			// Remote Control was already on when the visible screen shows
			// Claude's dialog instead of a new line.
			if vis, verr := h.PaneRead(pane, herdr.SourceVisible, 0); verr == nil {
				if u, open := visibleDialog(vis); open {
					r.Outcome, r.URL = OutcomeAlreadyOn, u
					r.DialogLeftOpen = closeDialog(h, pane, sessionID)
					return r
				}
			}
			if u := newRemoteControlURL(before, after); u != "" {
				r.Outcome, r.URL = OutcomeLinked, u
				return r
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
	}
	r.Outcome = OutcomeNoLink
	return r
}

// closeDialog presses Esc, which picks the dialog's default (Continue), but
// only when the dialog is still on the visible screen and Claude isn't
// working: Esc during a turn interrupts it. It re-checks just before sending,
// because the screen may have changed since the last read. It returns "" when
// the dialog is closed, else the line that tells the user to press Esc.
func closeDialog(h *herdr.Client, pane, sessionID string) string {
	p, err := h.PaneGet(pane)
	if err != nil || !runs(p, sessionID) {
		return "Could not check the pane before closing its dialog. Press Esc in the pane."
	}
	if p.AgentStatus == "working" {
		return "Claude is working, so sessionhub did not press Esc. Press Esc in the pane to close the dialog."
	}
	vis, err := h.PaneRead(pane, herdr.SourceVisible, 0)
	if err != nil {
		return fmt.Sprintf("Could not close its dialog (%v). Press Esc in the pane.", err)
	}
	if _, open := visibleDialog(vis); !open {
		return "" // it closed on its own
	}
	if err := h.SendKeys(pane, "esc"); err != nil {
		return fmt.Sprintf("Could not close its dialog (%v). Press Esc in the pane.", err)
	}
	return ""
}

// resumeInWorkspace starts `claude --resume <id> --remote-control` in a new
// herdr workspace in the session's directory, without focus, and waits for
// the link.
func resumeInWorkspace(ctx context.Context, h *herdr.Client, s api.Session, o ControlOptions) ControlResult {
	r := startInWorkspace(ctx, h, s, o, remoteControlArgs(s.ID))
	if r.Outcome != OutcomeResumed {
		return r
	}
	r.URL = waitForLink(ctx, h, r.Pane, o)
	return r
}

// StartResumed starts `claude --resume <id>` for session s on this machine,
// without Remote Control and without focus, and waits until herdr detects
// Claude in the pane. It never starts a second copy: when a pane already
// runs s, or s's recorded pane shows Claude with no session ID yet, it
// returns OutcomeRunning. It starts in s's recorded pane when that
// pane is on this herdr server and sits at a shell, else in a new workspace
// at s.CWD (a missing or relative directory is OutcomeNoDir). It ignores the
// server's status: the watcher calls it for a session it just ended or just
// wrote, which the server may still show as live.
func StartResumed(ctx context.Context, socket string, s api.Session, o ControlOptions) ControlResult {
	if !validID(s.ID) {
		return failure(fmt.Errorf("refusing session ID %q: expected letters, digits, and hyphens, not starting with a hyphen", s.ID))
	}
	if s.HerdrPane != "" && !validPaneID.MatchString(s.HerdrPane) {
		return failure(fmt.Errorf("refusing pane ID %q: not a herdr pane ID", clean(s.HerdrPane)))
	}
	h, err := herdr.Dial(socket)
	if err != nil {
		return failure(fmt.Errorf("herdr is not running: %w", err))
	}
	snap, err := h.Snapshot()
	if err != nil {
		return failure(fmt.Errorf("read herdr snapshot: %w", err))
	}
	for _, p := range snap.Panes {
		if runs(p, s.ID) {
			pane, err := checkedPane(p.PaneID)
			if err != nil {
				return failure(err)
			}
			return ControlResult{Outcome: OutcomeRunning, Pane: pane}
		}
	}
	args := []string{"--resume", s.ID}
	if s.HerdrPane != "" && (s.HerdrSession == "" || s.HerdrSession == herdr.SessionName(socket)) {
		p, found, err := paneGet(h, s.HerdrPane)
		if err != nil {
			return failure(err)
		}
		// Claude in the recorded pane that herdr reports with no session yet,
		// or with this one, counts as running: starting the session again
		// would run it twice.
		if found && p.Agent == "claude" && (p.SessionID() == "" || p.SessionID() == s.ID) {
			pane, err := checkedPane(p.PaneID)
			if err != nil {
				return failure(err)
			}
			return ControlResult{Outcome: OutcomeRunning, Pane: pane}
		}
		if found && p.Agent == "" {
			pane, err := checkedPane(p.PaneID)
			if err != nil {
				return failure(err)
			}
			r := ControlResult{Pane: pane}
			if _, err := h.AgentStart(herdr.AgentStartParams{Name: agentName(s.ID), Kind: "claude", PaneID: pane, Args: args}); err != nil {
				r.Outcome, r.Err = OutcomeError, fmt.Errorf("start claude in pane %s: %w", clean(pane), err)
				return r
			}
			if err := waitForClaude(ctx, h, pane, o); err != nil {
				r.Outcome, r.Err = OutcomeError, fmt.Errorf("claude didn't start in pane %s: %s", clean(pane), lastLine(h, pane))
				return r
			}
			r.Outcome = OutcomeResumed
			return r
		}
	}
	return startInWorkspace(ctx, h, s, o, args)
}

// startInWorkspace starts claude with args in a new herdr workspace in the
// session's directory, without focus, and waits until herdr detects Claude
// there. A directory that is missing, not a directory, or not absolute
// creates nothing: the server doesn't check the recorded path, and a relative
// one would resolve against herdr's own directory.
func startInWorkspace(ctx context.Context, h *herdr.Client, s api.Session, o ControlOptions, args []string) ControlResult {
	if !filepath.IsAbs(s.CWD) {
		return ControlResult{Outcome: OutcomeNoDir}
	}
	dir := filepath.Clean(s.CWD)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return ControlResult{Outcome: OutcomeNoDir}
	}
	return startClaudeIn(ctx, h, dir, workspaceLabel(s), agentName(s.ID), o, args)
}

// startClaudeIn creates a herdr workspace in dir (absolute and checked by
// the caller) labelled label, without focus, starts claude with args there
// as agent name, and waits until herdr detects Claude. It returns
// OutcomeResumed with the workspace and its pane.
func startClaudeIn(ctx context.Context, h *herdr.Client, dir, label, name string, o ControlOptions, args []string) ControlResult {
	ws, err := h.WorkspaceCreate(herdr.WorkspaceCreateParams{CWD: dir, Label: label})
	if err != nil {
		return failure(fmt.Errorf("create workspace: %w", err))
	}
	r := ControlResult{Workspace: ws.WorkspaceID, Pane: ws.RootPane.PaneID}
	if !validPaneID.MatchString(r.Pane) {
		r.Outcome, r.Err = OutcomeError, fmt.Errorf("new workspace %s has root pane ID %q, not a herdr pane ID; close the workspace", clean(r.Workspace), clean(r.Pane))
		return r
	}
	if _, err := h.AgentStart(herdr.AgentStartParams{
		Name:   name,
		Kind:   "claude",
		PaneID: r.Pane,
		Args:   args,
	}); err != nil {
		r.Outcome, r.Err = OutcomeError, fmt.Errorf("start claude in pane %s of new workspace %s: %w", clean(r.Pane), clean(r.Workspace), err)
		return r
	}
	// herdr accepts agent.start before the shell has run the command: a shell
	// that eats keystrokes (zsh's compinit prompt did) accepts it too. Never
	// retry: a second start risks a second copy.
	if err := waitForClaude(ctx, h, r.Pane, o); err != nil {
		r.Outcome = OutcomeError
		if errors.Is(err, errStopped) {
			r.Err = fmt.Errorf("the watcher stopped before Claude was confirmed in the new workspace %s", clean(r.Workspace))
			return r
		}
		r.Err = fmt.Errorf("claude didn't start in the new workspace %s: %s", clean(r.Workspace), lastLine(h, r.Pane))
		return r
	}
	r.Outcome = OutcomeResumed
	return r
}

// maxScreenLine caps the screen line in a "claude didn't start" error.
const maxScreenLine = 120

// errStopped is waitForClaude's error when ctx ended first.
var errStopped = errors.New("stopped")

// waitForClaude polls pane until herdr detects Claude there, for up to
// PollFor. It doesn't wait for agent_session: herdr reports that later. The
// window is PollFor, the same as the link wait: 15 s in the watcher, so a
// resume can wait up to 30 s in all. It returns errStopped when ctx ends
// first, and another error when the window ends.
func waitForClaude(ctx context.Context, h *herdr.Client, pane string, o ControlOptions) error {
	every, total := pollBounds(o)
	deadline := time.Now().Add(total)
	for {
		// A failed or missing lookup is treated like "not yet".
		if p, err := h.PaneGet(pane); err == nil && p.Agent == "claude" {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.New("not detected in time")
		}
		select {
		case <-ctx.Done():
			return errStopped
		case <-time.After(every):
		}
	}
}

// isPrompt reports whether l looks like a bare shell prompt: a short line
// that ends in %, $, # or ❯ and has no text after it.
func isPrompt(l string) bool {
	rs := []rune(l)
	return len(rs) <= 40 && strings.ContainsRune("%$#❯", rs[len(rs)-1])
}

// lastLine is the last non-blank line on pane's visible screen, cleaned and
// cut to maxScreenLine runes, for an error. Trailing bare shell prompts are
// skipped, because after a failed command the last line is the next prompt:
// the line before it says what failed. With only prompts, it is the last one.
// When the screen can't be read or is blank, it says so.
func lastLine(h *herdr.Client, pane string) string {
	screen, err := h.PaneRead(pane, herdr.SourceVisible, 0)
	if err != nil {
		return "screen unreadable"
	}
	lines := strings.Split(screen, "\n")
	prompt := ""
	for i := len(lines) - 1; i >= 0; i-- {
		l := termtext.Clean(lines[i], maxScreenLine)
		switch {
		case l == "":
		case isPrompt(l):
			if prompt == "" {
				prompt = l
			}
		default:
			return l
		}
	}
	if prompt != "" {
		return prompt
	}
	return "nothing on screen"
}

// waitForLink reads a pane Claude just started in until a new Remote Control
// banner appears, for up to PollFor, and returns its link.
//
// The resumed conversation is redrawn in the pane and may hold an earlier
// banner, so the baseline is the pane right after agent.start returns. Live
// on bluebox (Claude Code 2.1.285), that read had no banner and the new one came
// about 3 s later; the resumed Claude reused the earlier link, so the rule
// counts banner lines and never compares URLs. With no baseline there is no
// link: a missed link is better than a stale one.
func waitForLink(ctx context.Context, h *herdr.Client, pane string, o ControlOptions) string {
	base, err := h.PaneRead(pane, herdr.SourceRecentUnwrapped, rcLines)
	if err != nil {
		return ""
	}
	every, total := pollBounds(o)
	deadline := time.Now().Add(total)
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(every):
		}
		if text, err := h.PaneRead(pane, herdr.SourceRecentUnwrapped, rcLines); err == nil {
			if u := newBannerURL(base, text); u != "" {
				return u
			}
		}
		if !time.Now().Before(deadline) {
			return ""
		}
	}
}

// rcBanner starts the line Claude prints when Remote Control turns on:
// "/remote-control is active · Continue here, on your phone, or at <URL>".
const rcBanner = "/remote-control is active"

// bannerLines returns the lines of text that hold the Remote Control banner.
func bannerLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, rcBanner) {
			out = append(out, l)
		}
	}
	return out
}

// newBannerURL returns the link on the last banner line of after, but only
// when after holds more banner lines than before, else "". A window that
// slid (old lines scrolled out) can hide a new banner: that misses the link,
// never reports a stale one.
func newBannerURL(before, after string) string {
	lines := bannerLines(after)
	if len(lines) <= len(bannerLines(before)) {
		return ""
	}
	urls := remoteControlURLs(lines[len(lines)-1])
	if len(urls) == 0 {
		return ""
	}
	return urls[len(urls)-1]
}

// workspaceLabel is "sessionhub: <title>", with the title cleaned and cut to
// maxLabelTitle runes, or the short session ID when there is no title.
func workspaceLabel(s api.Session) string {
	t := termtext.Clean(s.Title, maxLabelTitle)
	if strings.TrimSpace(t) == "" {
		t = shortID(s.ID)
	}
	return "sessionhub: " + t
}
