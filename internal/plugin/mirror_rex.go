package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// rexCallTimeout bounds one rex command.
const rexCallTimeout = 5 * time.Second

// errRexGone is a rex command's answer once the session or block is gone.
var errRexGone = errors.New("rex block not found")

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
	bin   string
	every time.Duration
}

// newRexMirror returns the mirror for a session that runs in a Rex block.
func newRexMirror() mirror { return rexMirror{bin: "rex", every: attachPollEvery} }

// Run returns nil when ctx ends or the block is gone.
func (m rexMirror) Run(ctx context.Context, s api.Session, out io.Writer, in io.Reader) error {
	if s.RexSession == "" || s.RexBlock == "" {
		return errors.New("session has no Rex block")
	}
	// The input goroutine ends when in does: the caller closes it after Run.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			// shortcut: a UTF-8 rune split across two reads turns into
			// U+FFFD in the JSON text; buffer the tail if guests see it.
			if n > 0 {
				m.call(ctx, s, "write", map[string]string{"data": string(buf[:n])}) // a failure shows as block gone on the next read
			}
			if err != nil {
				return
			}
		}
	}()
	t := time.NewTicker(m.every)
	defer t.Stop()
	var last string
	for {
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

// call runs `rex block call -s SESSION -b BLOCK METHOD ARGS` and returns its
// JSON result. A session or block that is gone is errRexGone.
func (m rexMirror) call(ctx context.Context, s api.Session, method string, args any) ([]byte, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, rexCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, m.bin, "block", "call", "-s", s.RexSession, "-b", s.RexBlock, method, string(body))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	res, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "not found") {
			return nil, errRexGone
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return res, nil
}
