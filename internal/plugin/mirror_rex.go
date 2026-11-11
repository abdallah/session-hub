package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/rex"
)

// errRexGone is a rex command's answer once the session or block is gone.
var errRexGone = rex.ErrGone

// rexMirror is a snapshot mirror of a Rex terminal block: clear-screen and
// the screen as escape sequences (terminal.format vt) each time it changes,
// guest bytes typed into the block with terminal.write. It runs the rex
// command, which speaks the server's WebSocket control API.
//
// shortcut: it polls every 250 ms, because the block's data stream
// (rex.data.terminal.v1) is an undocumented binary WebSocket protocol and
// `rex block stream` is not implemented in Rex 0.0.1793981048; switch to the
// stream when either lands.
type rexMirror struct {
	every time.Duration
}

// newRexMirror returns the mirror for a session that runs in a Rex block.
func newRexMirror() mirror { return rexMirror{every: attachPollEvery} }

// Run returns nil when ctx ends or the block is gone.
func (m rexMirror) Run(ctx context.Context, s api.Session, out io.Writer, in io.Reader) error {
	if s.RexSession == "" || s.RexBlock == "" {
		return errors.New("session has no Rex block")
	}
	// alive is whether Claude still runs in the block, refreshed every
	// rexProcessEvery by the poll loop. Input is dropped while it is false,
	// so a guest never types into the shell left after Claude exits.
	// shortcut: Rex reports process names, not the Claude session ID, so a
	// different Claude in the same block passes; check the ID if Rex
	// exposes it.
	var alive atomic.Bool
	alive.Store(true)
	// The input goroutine ends when in does: the caller closes it after Run.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			// shortcut: a UTF-8 rune split across two reads turns into
			// U+FFFD in the JSON text; buffer the tail if guests see it.
			if b := dropNUL(buf[:n]); len(b) > 0 && alive.Load() {
				m.call(ctx, s, "write", map[string]string{"data": string(b)}) // a failure shows as block gone on the next read
			}
			if err != nil {
				return
			}
		}
	}()
	t := time.NewTicker(m.every)
	defer t.Stop()
	var last string
	checked := time.Time{}
	for {
		if time.Since(checked) >= rexProcessEvery {
			checked = time.Now()
			running, err := m.claudeRuns(ctx, s)
			switch {
			case errors.Is(err, errRexGone), ctx.Err() != nil, err == nil && !running:
				return nil // the session ended
			case err != nil:
				return fmt.Errorf("read block %s processes: %w", s.RexBlock, err)
			}
			alive.Store(running)
		}
		res, err := m.call(ctx, s, "format", map[string]any{"format": "vt", "trim": true, "extra_cursor": true})
		var frame struct{ Content string }
		switch {
		case errors.Is(err, errRexGone):
			return nil
		case ctx.Err() != nil:
			return nil
		case err != nil:
			return fmt.Errorf("read block %s: %w", s.RexBlock, err)
		case json.Unmarshal(res, &frame) != nil:
			return fmt.Errorf("read block %s: bad JSON from rex", s.RexBlock)
		case frame.Content != last:
			last = frame.Content
			if _, err := io.WriteString(out, clearScreen+frame.Content); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// rexProcessEvery is how often the mirror checks that Claude still runs.
const rexProcessEvery = time.Second

// claudeRuns reports whether a process named claude runs in the block.
func (m rexMirror) claudeRuns(ctx context.Context, s api.Session) (bool, error) {
	return rex.ClaudeRuns(ctx, s.RexSession, s.RexBlock)
}

// call runs one method of the session's block; see rex.Call.
func (m rexMirror) call(ctx context.Context, s api.Session, method string, args any) ([]byte, error) {
	return rex.Call(ctx, s.RexSession, s.RexBlock, method, args)
}
