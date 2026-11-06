package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/resume"
)

// Attach timing and limits.
const (
	attachPollEvery  = 250 * time.Millisecond // how often herdrMirror reads the pane
	attachFlushEvery = 30 * time.Millisecond  // the longest output waits before it is posted
	attachFlushSize  = 8 << 10                // post at once when this much output is waiting
	maxAttaches      = 4                      // open attaches per watcher
)

const (
	detailGuestLeft    = "guest left"
	detailSessionEnded = "session ended"
	detailTooMany      = "too many open attaches"
	detailNoAttach     = "this watcher cannot attach"
	detailStopped      = "the watcher stopped"
)

// clearScreen goes before each frame of a snapshot mirror.
const clearScreen = "\x1b[H\x1b[2J"

// mirror streams one session's terminal to out and feeds in to it until
// ctx ends or the terminal ends.
type mirror interface {
	Run(ctx context.Context, s api.Session, out io.Writer, in io.Reader) error
}

// newMirror returns the mirror for session s: Rex for a session in a Rex
// block, herdr otherwise.
func newMirror(socket string, s api.Session) (mirror, error) {
	if s.RexBlock != "" {
		return newRexMirror(), nil
	}
	h, err := herdr.Dial(socket)
	if err != nil {
		return nil, errors.New(detailHerdrDown)
	}
	return herdrMirror{h: h, socket: socket, every: attachPollEvery}, nil
}

// startAttach runs an attach claim in its own goroutine, so the control
// loop keeps polling, and posts its result when the mirror ends. Past
// maxAttaches open attaches it fails the claim at once.
func (c *controller) startAttach(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	target := "session " + claim.Session.ID
	select {
	case c.attachSlots <- struct{}{}:
	default:
		c.postResult(ctx, cl, claim.Request.ID, target, api.ControlResultIn{State: api.ControlFailed, Detail: detailTooMany})
		return
	}
	c.attaches.Add(1)
	go func() {
		defer c.attaches.Done()
		defer func() { <-c.attachSlots }()
		c.postResult(ctx, cl, claim.Request.ID, target, c.attach(ctx, cl, claim))
	}()
}

// attach relays one mirror: guest input from the machine input stream into
// the mirror, its output in batches to the server. The guest leaving (the
// input stream ends) ends the mirror.
func (c *controller) attach(ctx context.Context, cl *client.Client, claim api.ControlClaim) api.ControlResultIn {
	failed := func(err error) api.ControlResultIn {
		return api.ControlResultIn{State: api.ControlFailed, Detail: termtext.Clean(err.Error(), api.MaxControlDetailRunes)}
	}
	if c.mirrorFor == nil {
		return api.ControlResultIn{State: api.ControlFailed, Detail: detailNoAttach}
	}
	m, err := c.mirrorFor(claim.Session)
	if err != nil {
		return failed(err)
	}
	id := claim.Request.ID
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stream, err := cl.MachineAttachInput(actx, id)
	if err != nil {
		return failed(fmt.Errorf("open guest input: %w", err))
	}
	defer stream.Close()
	in := readFunc(func(p []byte) (int, error) {
		n, err := stream.Read(p)
		if err != nil {
			cancel(err) // io.EOF: the guest left
		}
		return n, err
	})
	out := &batchWriter{post: func(b []byte) error { return cl.PostAttachOutput(actx, id, b) },
		every: attachFlushEvery, size: attachFlushSize}
	err = m.Run(actx, claim.Session, out, in)
	out.Flush()
	cause := context.Cause(actx)
	switch {
	case errors.Is(cause, io.EOF):
		return api.ControlResultIn{State: api.ControlDone, Detail: detailGuestLeft}
	case err != nil:
		return failed(err)
	case ctx.Err() != nil:
		return api.ControlResultIn{State: api.ControlFailed, Detail: detailStopped}
	case cause != nil:
		return failed(fmt.Errorf("guest input: %w", cause))
	}
	return api.ControlResultIn{State: api.ControlDone, Detail: detailSessionEnded}
}

// herdrMirror is a snapshot mirror of a herdr pane: clear-screen and the
// visible screen with ANSI each time it changes, guest bytes typed into the
// pane with pane.send_input.
//
// shortcut: it polls every 250 ms, because internal/herdr has no
// events.subscribe stream (a Client answers one request per connection);
// switch to pane.updated events if a subscriber lands there.
type herdrMirror struct {
	h      *herdr.Client
	socket string
	every  time.Duration
}

// Run returns nil when ctx ends or the pane no longer runs session s.
func (m herdrMirror) Run(ctx context.Context, s api.Session, out io.Writer, in io.Reader) error {
	pane, err := resume.FindPane(m.h, m.socket, s)
	if err != nil {
		return err
	}
	if pane == "" {
		return errors.New(detailNotRunning)
	}
	// alive reports whether the pane still runs s: once Claude exits, the
	// pane is a shell, and the guest must not type into it.
	alive := func() (bool, error) {
		p, err := m.h.PaneGet(pane)
		var he *herdr.Error
		if errors.As(err, &he) && he.Code == "pane_not_found" {
			return false, nil
		}
		return err == nil && resume.Runs(p, s.ID), err
	}
	// The input goroutine ends when in does: the caller closes it after Run.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			// shortcut: a UTF-8 rune split across two reads turns into
			// U+FFFD in the JSON text; buffer the tail if guests see it.
			if n > 0 {
				if ok, _ := alive(); ok {
					m.h.SendInput(pane, string(buf[:n])) // a failure shows as pane gone on the next read
				}
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
		ok, err := alive()
		if err != nil {
			return fmt.Errorf("look up pane %s: %w", pane, err)
		}
		if !ok {
			return nil
		}
		frame, err := m.h.PaneReadANSI(pane, herdr.SourceVisible)
		var he *herdr.Error
		switch {
		case errors.As(err, &he) && he.Code == "pane_not_found":
			return nil
		case err != nil:
			return fmt.Errorf("read pane %s: %w", pane, err)
		case frame != last:
			last = frame
			if _, err := io.WriteString(out, clearScreen+frame); err != nil {
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

// batchWriter collects output and posts it in chunks: when attachFlushSize
// bytes wait, or attachFlushEvery after the first byte of a chunk. After a
// post fails, every Write returns that error.
type batchWriter struct {
	post  func([]byte) error
	every time.Duration
	size  int

	mu    sync.Mutex // also held while posting, so chunks go out in order
	buf   []byte
	timer *time.Timer
	err   error
}

func (b *batchWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return 0, b.err
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) >= b.size {
		b.flushLocked()
		return len(p), b.err
	}
	if b.timer == nil {
		b.timer = time.AfterFunc(b.every, b.Flush)
	}
	return len(p), nil
}

// Flush posts what waits.
func (b *batchWriter) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushLocked()
}

func (b *batchWriter) flushLocked() {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	if len(b.buf) == 0 || b.err != nil {
		return
	}
	b.err = b.post(b.buf)
	b.buf = nil
}

// readFunc is an io.Reader made of a function.
type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }
