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
	"github.com/abdallah/session-hub/internal/claudetrust"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/herdr"
)

// Outcomes of StartNew besides OutcomeNoDir and OutcomeError.
const (
	// OutcomeStarted: Claude runs in a new workspace with Remote Control on.
	OutcomeStarted Outcome = "started"
	// OutcomeOutsideHome: the directory is not inside the home directory;
	// nothing was created.
	OutcomeOutsideHome Outcome = "outside_home"
	// OutcomeUntrusted: Claude doesn't trust the directory and the request
	// didn't ask to trust it, so Claude would stop at its trust prompt;
	// nothing was created.
	OutcomeUntrusted Outcome = "untrusted"
)

// NewSessionResult is what StartNew did.
type NewSessionResult struct {
	ControlResult
	// Dir is the resolved directory, once ResolveStartDir accepted it.
	Dir string
	// PromptNote is "" when there was no first prompt or it was submitted,
	// else why it wasn't.
	PromptNote string
}

// Notes for a first prompt that wasn't submitted.
const (
	notePromptBlocked = "the first prompt was not sent: Claude waits on a prompt in the pane (a folder it hasn't seen asks whether to trust it)"
	notePromptBusy    = "the first prompt was not sent: Claude didn't become idle in time"
)

// ResolveStartDir turns a start request's directory into the absolute,
// symlink-free directory to start in. "~" and "~/..." expand to home. The
// result must exist, be a directory, and be home or inside it.
func ResolveStartDir(dir, home string) (string, Outcome) {
	switch {
	case dir == "~":
		dir = home
	case strings.HasPrefix(dir, "~/"):
		dir = filepath.Join(home, dir[2:])
	}
	if !filepath.IsAbs(dir) || home == "" {
		return "", OutcomeNoDir
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return "", OutcomeNoDir
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return "", OutcomeNoDir
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", OutcomeOutsideHome
	}
	rel, err := filepath.Rel(realHome, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", OutcomeOutsideHome
	}
	return real, ""
}

// StartNew starts a new Claude session with Remote Control on, for start
// request req, in a new herdr workspace in req.Dir, without focus. It waits
// for the Remote Control link, then submits req.Prompt, if any, once Claude
// is idle. home is the directory req.Dir must be inside.
//
// Claude must trust the directory, or it stops at its trust prompt. When
// req.Trust is set, StartNew first marks the directory, and only it,
// trusted in claudeConfig (see claudetrust.Path); otherwise an untrusted
// directory is OutcomeUntrusted.
func StartNew(ctx context.Context, socket, home, claudeConfig string, req api.StartRequest, o ControlOptions) NewSessionResult {
	dir, out := ResolveStartDir(req.Dir, home)
	if out != "" {
		return NewSessionResult{ControlResult: ControlResult{Outcome: out}}
	}
	if r, ok := ensureTrusted(claudeConfig, dir, home, req.Trust); !ok {
		return r
	}
	h, err := herdr.Dial(socket)
	if err != nil {
		return NewSessionResult{ControlResult: failure(fmt.Errorf("herdr is not running: %w", err)), Dir: dir}
	}
	r := startClaudeIn(ctx, h, dir, newSessionLabel(dir), newSessionAgentName(req.ID), o, []string{"--remote-control"})
	if r.Outcome != OutcomeResumed {
		return NewSessionResult{ControlResult: r}
	}
	r.Outcome = OutcomeStarted
	res := NewSessionResult{ControlResult: r, Dir: dir}
	res.URL = waitForLink(ctx, h, r.Pane, o)
	if req.Prompt != "" {
		res.PromptNote = submitWhenIdle(ctx, h, r.Pane, req.Prompt, o)
	}
	return res
}

// ensureTrusted makes sure Claude trusts dir, marking it trusted when trust
// is set. ok is false, with the result to return, when it doesn't.
func ensureTrusted(config, dir, home string, trust bool) (NewSessionResult, bool) {
	fail := func(r ControlResult) (NewSessionResult, bool) {
		return NewSessionResult{ControlResult: r, Dir: dir}, false
	}
	if trust {
		if err := claudetrust.Trust(config, dir, home); err != nil {
			return fail(failure(fmt.Errorf("can't mark %s trusted: %w", dir, err)))
		}
		return NewSessionResult{}, true
	}
	ok, err := claudetrust.Trusted(config, dir)
	if err != nil {
		return fail(failure(fmt.Errorf("can't tell whether Claude trusts %s: %w", dir, err)))
	}
	if !ok {
		return fail(ControlResult{Outcome: OutcomeUntrusted})
	}
	return NewSessionResult{}, true
}

// submitWhenIdle submits text to pane once herdr reports the agent idle, for
// up to PollFor. It returns "" when it was submitted, else a note saying
// why not.
func submitWhenIdle(ctx context.Context, h *herdr.Client, pane, text string, o ControlOptions) string {
	every, total := pollBounds(o)
	deadline := time.Now().Add(total)
	for {
		p, err := h.PaneGet(pane)
		if err == nil {
			switch p.AgentStatus {
			case "idle", "done":
				err := h.AgentPrompt(pane, text)
				var be *herdr.BlockedError
				switch {
				case errors.As(err, &be):
					return notePromptBlocked
				case err != nil:
					return "the first prompt was not sent: " + termtext.Clean(err.Error(), 120)
				}
				return ""
			case "blocked":
				return notePromptBlocked
			}
		}
		if !time.Now().Before(deadline) {
			return notePromptBusy
		}
		select {
		case <-ctx.Done():
			return notePromptBusy
		case <-time.After(every):
		}
	}
}

// newSessionLabel is "sessionhub: new in <directory name>", cleaned and cut.
func newSessionLabel(dir string) string {
	base := termtext.Clean(filepath.Base(dir), maxLabelTitle)
	if strings.TrimSpace(base) == "" || base == string(filepath.Separator) {
		base = termtext.Clean(dir, maxLabelTitle)
	}
	return "sessionhub: new in " + base
}

// newSessionAgentName is "sessionhub-new-" and the first 8 letters or
// digits of the request ID after its st_ prefix, lowercased.
func newSessionAgentName(id string) string {
	var b strings.Builder
	for _, r := range strings.TrimPrefix(id, "st_") {
		if b.Len() == 8 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if r >= 'A' && r <= 'Z' {
			b.WriteRune(r + 'a' - 'A')
		}
	}
	return "sessionhub-new-" + b.String()
}
