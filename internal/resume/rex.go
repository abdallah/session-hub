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
	"github.com/abdallah/session-hub/internal/rex"
)

// HerdrUp reports whether a herdr server answers on socket. A socket file
// left by a server that died does not count.
func HerdrUp(socket string) bool {
	if socket == "" {
		return false
	}
	_, err := herdr.Dial(socket)
	return err == nil
}

// Backends lists the terminal backends this machine has, herdr first: herdr
// when its socket answers, Rex when the rex command is installed.
func Backends(socket string) []string {
	out := []string{}
	if HerdrUp(socket) {
		out = append(out, api.BackendHerdr)
	}
	if rex.Installed() {
		out = append(out, api.BackendRex)
	}
	return out
}

// backendFor is the backend that runs session s on this machine: the one
// its row names, else this machine's first. A session that does not exist
// yet (a start) is the zero Session.
func backendFor(s api.Session, socket string) string {
	if b := s.Backend(); b != "" {
		return b
	}
	if bs := Backends(socket); len(bs) > 0 {
		return bs[0]
	}
	return ""
}

// where names the pane or Rex block of r for a message.
func where(r ControlResult) string {
	if r.RexBlock != "" {
		return "Rex block " + clean(r.RexBlock)
	}
	return "pane " + clean(r.Pane)
}

// rexScreen is the last rcLines lines of the block's text.
func rexScreen(ctx context.Context, session, block string) (string, error) {
	text, err := rex.Screen(ctx, session, block)
	if err != nil {
		return "", err
	}
	lines := strings.Split(text, "\n")
	if len(lines) > rcLines {
		lines = lines[len(lines)-rcLines:]
	}
	return strings.Join(lines, "\n"), nil
}

// controlRex is Control for a session in a Rex block. When Claude runs in
// the recorded block it types /remote-control there and waits for the link.
// Otherwise, with o.Resume, it starts `claude --resume <id>
// --remote-control` in a new Rex session, but only when the block is gone
// or the session's hooks reported its end, and never when the server
// reports the session live or blocked.
func controlRex(ctx context.Context, s api.Session, o ControlOptions) ControlResult {
	running, gone, err := rexState(ctx, s)
	switch {
	case err != nil:
		return failure(err)
	case running:
		return rexInjectAndWait(ctx, s, o)
	case !o.Resume:
		return ControlResult{Outcome: OutcomeNotRunning}
	case isLive(s), !gone && s.Status != api.StatusEnded:
		// A block that is still there, with no end reported: Claude may run
		// under a multiplexer in it, where Rex does not show it.
		return ControlResult{Outcome: OutcomeLooksLive}
	}
	r := startInRex(ctx, s, o, remoteControlArgs(s.ID))
	if r.Outcome == OutcomeResumed {
		r.URL = rexWaitForLink(ctx, r, o)
	}
	return r
}

// rexState reports whether Rex shows Claude in s's recorded block, and
// whether the block is gone. A session with no block has none.
func rexState(ctx context.Context, s api.Session) (running, gone bool, err error) {
	if s.RexBlock == "" {
		return false, true, nil
	}
	running, err = rex.ClaudeRuns(ctx, s.RexSession, s.RexBlock)
	switch {
	case errors.Is(err, rex.ErrGone):
		return false, true, nil
	case err != nil:
		return false, false, fmt.Errorf("read Rex block %s: %w", clean(s.RexBlock), err)
	}
	return running, false, nil
}

// rexInjectAndWait is injectAndWait for a Rex block. Rex types into a block
// whatever it shows, so a session the server reports blocked gets nothing.
func rexInjectAndWait(ctx context.Context, s api.Session, o ControlOptions) ControlResult {
	r := ControlResult{RexSession: s.RexSession, RexBlock: s.RexBlock}
	fail := func(err error) ControlResult {
		r.Outcome, r.Err = OutcomeError, err
		return r
	}
	if s.AgentState == "blocked" {
		r.Outcome = OutcomeBlocked
		return r
	}
	// Read first, so a URL from an earlier run is not mistaken for the new one.
	before, err := rexScreen(ctx, s.RexSession, s.RexBlock)
	if err != nil {
		return fail(fmt.Errorf("read Rex block %s: %w", clean(s.RexBlock), err))
	}
	if _, open := visibleDialog(before); open {
		r.Outcome = OutcomeDialogOpen
		return r
	}
	if err := rex.Submit(ctx, s.RexSession, s.RexBlock, rcCommand); err != nil {
		return fail(fmt.Errorf("send %s to Rex block %s: %w", rcCommand, clean(s.RexBlock), err))
	}
	every, total := pollBounds(o)
	deadline := time.Now().Add(total)
	for {
		select {
		case <-ctx.Done():
			r.Outcome = OutcomeNoLink
			return r
		case <-time.After(every):
		}
		// A failed read is treated like no URL yet; the deadline ends the wait.
		if after, err := rexScreen(ctx, s.RexSession, s.RexBlock); err == nil {
			if u, open := visibleDialog(after); open {
				r.Outcome, r.URL = OutcomeAlreadyOn, u
				r.DialogLeftOpen = rexCloseDialog(ctx, s)
				return r
			}
			if u := newRemoteControlURL(before, after); u != "" {
				r.Outcome, r.URL = OutcomeLinked, u
				return r
			}
		}
		if !time.Now().Before(deadline) {
			r.Outcome = OutcomeNoLink
			return r
		}
	}
}

// rexCloseDialog is closeDialog for a Rex block: Esc, unless the server
// reports Claude working or the dialog already closed.
func rexCloseDialog(ctx context.Context, s api.Session) string {
	if s.AgentState == "working" {
		return "Claude is working, so sessionhub did not press Esc. Press Esc in the pane to close the dialog."
	}
	text, err := rexScreen(ctx, s.RexSession, s.RexBlock)
	if err != nil {
		return fmt.Sprintf("Could not close its dialog (%v). Press Esc in the pane.", err)
	}
	if _, open := visibleDialog(text); !open {
		return ""
	}
	if err := rex.Write(ctx, s.RexSession, s.RexBlock, "\x1b"); err != nil {
		return fmt.Sprintf("Could not close its dialog (%v). Press Esc in the pane.", err)
	}
	return ""
}

// startResumedRex is StartResumed in Rex: OutcomeRunning when Claude runs in
// s's recorded block, else `claude --resume <id>` in a new Rex session.
func startResumedRex(ctx context.Context, s api.Session, o ControlOptions) ControlResult {
	running, _, err := rexState(ctx, s)
	switch {
	case err != nil:
		return failure(err)
	case running:
		return ControlResult{Outcome: OutcomeRunning, RexSession: s.RexSession, RexBlock: s.RexBlock}
	}
	return startInRex(ctx, s, o, []string{"--resume", s.ID})
}

// startInRex is startInWorkspace for Rex: claude with args in a new Rex
// session in the session's directory.
func startInRex(ctx context.Context, s api.Session, o ControlOptions, args []string) ControlResult {
	if !filepath.IsAbs(s.CWD) {
		return ControlResult{Outcome: OutcomeNoDir}
	}
	dir := filepath.Clean(s.CWD)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return ControlResult{Outcome: OutcomeNoDir}
	}
	return startClaudeInRex(ctx, dir, workspaceLabel(s), o, args)
}

// startClaudeInRex starts claude with args in a new Rex session in dir,
// labelled label, and waits until Rex shows Claude in its block. Rex shows
// the new session in no app, so nothing loses focus. It returns
// OutcomeResumed with the session and block.
func startClaudeInRex(ctx context.Context, dir, label string, o ControlOptions, args []string) ControlResult {
	session, block, err := rex.New(ctx, label, dir, append([]string{"claude"}, args...))
	if err != nil {
		return failure(fmt.Errorf("start a Rex session: %s", termtext.Clean(err.Error(), maxScreenLine)))
	}
	r := ControlResult{RexSession: session, RexBlock: block}
	every, total := pollBounds(o)
	deadline := time.Now().Add(total)
	for {
		running, err := rex.ClaudeRuns(ctx, session, block)
		switch {
		case running:
			r.Outcome = OutcomeResumed
			return r
		case errors.Is(err, rex.ErrGone):
			// The block closes when its command exits.
			r.Outcome, r.Err = OutcomeError, fmt.Errorf("claude didn't start in the new Rex session %s: its block closed", clean(session))
			return r
		case !time.Now().Before(deadline):
			r.Outcome, r.Err = OutcomeError, fmt.Errorf("claude didn't start in the new Rex session %s: not detected in time", clean(session))
			return r
		}
		select {
		case <-ctx.Done():
			r.Outcome, r.Err = OutcomeError, fmt.Errorf("the watcher stopped before Claude was confirmed in the new Rex session %s", clean(session))
			return r
		case <-time.After(every):
		}
	}
}

// rexWaitForLink is waitForLink for the block of r: it reads the block
// until a new Remote Control banner appears, for up to PollFor.
func rexWaitForLink(ctx context.Context, r ControlResult, o ControlOptions) string {
	base, err := rexScreen(ctx, r.RexSession, r.RexBlock)
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
		if text, err := rexScreen(ctx, r.RexSession, r.RexBlock); err == nil {
			if u := newBannerURL(base, text); u != "" {
				return u
			}
		}
		if !time.Now().Before(deadline) {
			return ""
		}
	}
}

// startNewRex is StartNew in Rex, for the resolved and trusted dir. Rex has
// no agent status, so the link is the sign that Claude takes input: without
// it the first prompt is not typed.
func startNewRex(ctx context.Context, dir string, req api.StartRequest, o ControlOptions) NewSessionResult {
	r := startClaudeInRex(ctx, dir, newSessionLabel(dir), o, []string{"--remote-control"})
	if r.Outcome != OutcomeResumed {
		return NewSessionResult{ControlResult: r}
	}
	r.Outcome = OutcomeStarted
	res := NewSessionResult{ControlResult: r, Dir: dir}
	res.URL = rexWaitForLink(ctx, r, o)
	switch {
	case req.Prompt == "":
	case res.URL == "":
		res.PromptNote = notePromptBusy
	default:
		if err := rex.Submit(ctx, r.RexSession, r.RexBlock, req.Prompt); err != nil {
			res.PromptNote = "the first prompt was not sent: " + termtext.Clean(err.Error(), 120)
		}
	}
	return res
}

// localRex resumes a session of this machine in Rex: it focuses the block
// when Claude runs there, else starts Claude in the session's directory, in
// a new tab, or in a new Rex session when Rex has none to add a tab to.
func (e *env) localRex(s api.Session) (bool, error) {
	ctx := context.Background()
	running, _, err := rexState(ctx, s)
	if err != nil {
		return false, err
	}
	if running {
		if err := rex.Focus(ctx, s.RexSession, s.RexBlock); err != nil {
			return false, fmt.Errorf("focus Rex block %s: %w", clean(s.RexBlock), err)
		}
		fmt.Fprintf(e.out, "Focused Rex block %q, which still runs session %s.\n", s.RexBlock, shortID(s.ID))
		if e.remoteControl {
			fmt.Fprintf(e.out, "It is already running, so --remote-control has no effect. To turn Remote Control on there, run: sessionhub remote-control %s\n", s.ID)
		}
		return true, nil
	}
	if err := e.guardLive(s); err != nil {
		return false, err
	}
	argv := append([]string{"claude"}, e.claudeArgs(s)...)
	block, err := rex.Run(ctx, s.CWD, argv)
	if err == nil {
		fmt.Fprintf(e.out, "Started claude %s in a new Rex tab, block %s.\n", strings.Join(e.claudeArgs(s), " "), clean(block))
		return true, nil
	}
	// rex run needs a session to add the tab to: outside Rex, with none
	// open (over SSH, say), there is none. Any other failure may have made
	// the tab, so nothing more is started.
	var nerr error = err
	var session string
	if strings.Contains(err.Error(), "no sessions") {
		session, _, nerr = rex.New(ctx, workspaceLabel(s), s.CWD, argv)
	}
	if nerr != nil {
		return false, fmt.Errorf("rex run: %w\nRun this in a shell instead:\n%s", err, e.resumeCmd(s))
	}
	fmt.Fprintf(e.out, "Started claude %s in the new Rex session %q (%s). Open it in Rex.\n",
		strings.Join(e.claudeArgs(s), " "), workspaceLabel(s), clean(session))
	return true, nil
}
