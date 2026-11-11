package plugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/resume"
	"github.com/abdallah/session-hub/internal/rex"
)

// Why a move's check of a Rex block failed.
const (
	detailRexGone = "the session's Rex block is gone"
	detailRexDown = "rex did not answer"
)

// place names where r put Claude, for a move detail or log line: "pane
// w1:p2" or "Rex block block:…".
func place(r resume.ControlResult) string {
	if r.RexBlock != "" {
		return "Rex block " + termtext.Clean(r.RexBlock, 64)
	}
	return "pane " + termtext.Clean(r.Pane, 64)
}

// idle is idlePane, or rexIdle for a session in a Rex block, whose pane is
// "".
func (m *mover) idle(ctx context.Context, cl *client.Client, s api.Session) (string, error) {
	if s.Backend() == api.BackendRex {
		return "", m.rexIdle(ctx, cl, s)
	}
	return m.idlePane(s)
}

// rexIdle is idlePane for a session in a Rex block: Claude runs there, and
// the server reports it idle or done. Rex has no agent status; the state is
// what the session's hooks last reported, read now.
func (m *mover) rexIdle(ctx context.Context, cl *client.Client, s api.Session) error {
	running, err := rex.ClaudeRuns(ctx, s.RexSession, s.RexBlock)
	switch {
	case errors.Is(err, rex.ErrGone):
		return errors.New(detailRexGone)
	case err != nil:
		return errors.New(detailRexDown)
	case !running:
		return errors.New(detailNoAgent)
	}
	d, err := cl.GetSession(ctx, s.ID)
	if err != nil {
		return fmt.Errorf("the session can't be read: %s", cleanErr(err))
	}
	switch d.AgentState {
	case "idle", "done":
		return nil
	case "":
		return errors.New("the agent's status is unknown")
	}
	return errors.New("the agent is " + termtext.Clean(d.AgentState, 40))
}

// endRex is endSession for a Rex block: it types /exit and waits up to
// exitWait for Claude to leave. A block that is gone has no Claude.
func (m *mover) endRex(ctx context.Context, session, block string) error {
	if err := rex.Submit(ctx, session, block, exitCommand); err != nil {
		if errors.Is(err, rex.ErrGone) {
			return nil
		}
		return fmt.Errorf("send %s: %w", exitCommand, err)
	}
	return m.rexLeft(ctx, session, block)
}

// rexLeft waits up to exitWait until the block is gone or runs no Claude,
// then ends the Rex session if sessionhub created it and it is empty.
func (m *mover) rexLeft(ctx context.Context, session, block string) error {
	deadline := time.Now().Add(m.exitWait)
	for {
		running, err := rex.ClaudeRuns(ctx, session, block)
		if errors.Is(err, rex.ErrGone) || err == nil && !running {
			rex.Reap(ctx, session)
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("Claude was still in the Rex block %s after %s", termtext.Clean(block, 64), m.exitWait)
		}
		if !sleepCtx(ctx, m.pollEvery) {
			return ctx.Err()
		}
	}
}

// stopRex is stopClaude for a Rex block. In a block that already ran
// Claude, /exit must end it. A Rex session that StartResumed created is this
// move's own: /exit is only a courtesy, the session is killed, and Claude
// counts as stopped only once Rex no longer has the block, so a Claude that
// Rex never showed stops too.
func (m *mover) stopRex(ctx context.Context, id string, r resume.ControlResult) error {
	err := m.endRex(ctx, r.RexSession, r.RexBlock)
	if r.Outcome != resume.OutcomeRunning {
		if err != nil {
			m.log.Printf("move: %s: /exit in %s: %s; ending its Rex session", id, place(r), cleanErr(err))
		}
		rex.Kill(ctx, r.RexSession) // a failure shows as a block that stays
		err = m.rexGone(ctx, r.RexSession, r.RexBlock)
	}
	if err != nil {
		m.log.Printf("move: %s: end Claude in %s: %s; end it by hand", id, place(r), cleanErr(err))
	}
	return err
}

// rexGone waits up to exitWait until Rex no longer has the block.
func (m *mover) rexGone(ctx context.Context, session, block string) error {
	deadline := time.Now().Add(m.exitWait)
	for {
		if _, err := rex.ClaudeRuns(ctx, session, block); errors.Is(err, rex.ErrGone) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the Rex block %s was still there %s after ending its session", termtext.Clean(block, 64), m.exitWait)
		}
		if !sleepCtx(ctx, m.pollEvery) {
			return ctx.Err()
		}
	}
}

// rexCloneFree is cloneFree's check for Rex: it fails when a live session of
// machine in a Rex block has its directory in the clone, as the server
// lists them. inClone reports whether a directory is inside the clone.
func rexCloneFree(ctx context.Context, cl *client.Client, machine, clone string, inClone func(string) bool) error {
	if !rex.Installed() {
		return nil
	}
	list, err := cl.ListSessions(ctx, true, machine)
	if err != nil {
		return fmt.Errorf("list this machine's sessions: %s", cleanErr(err))
	}
	for _, s := range list {
		if s.Backend() == api.BackendRex && inClone(s.CWD) {
			return fmt.Errorf("%s is in use by session %s in a Rex block", clone, termtext.Clean(s.ID, 64))
		}
	}
	return nil
}
