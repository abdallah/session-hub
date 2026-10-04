package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

// startAPI is the part of *client.Client that sessionhub start uses.
type startAPI interface {
	CreateStart(ctx context.Context, machine string, in api.StartIn) (api.StartRequest, error)
	GetStart(ctx context.Context, id string) (api.StartRequest, error)
}

var _ startAPI = (*client.Client)(nil)

const (
	// startFollow is how long sessionhub start prints a request's progress.
	// The watcher takes up to about 45 s; the request expires after 2 minutes.
	startFollow = 2*time.Minute + 10*time.Second
	// startPoll is how often it reads the request.
	startPoll = time.Second
)

const startUsage = `usage: sessionhub start <machine> [--dir DIR] [-m "first prompt"]`

// RunStart implements `sessionhub start`.
func RunStart(ctx context.Context, args []string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWith(ctx, args, (*env).start)
}

func (e *env) start(ctx context.Context, args []string) error {
	var machine string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		machine, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "directory to start in (default: this directory on this machine, ~ elsewhere)")
	prompt := fs.String("m", "", "first prompt")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("start: %w\n%s", err, startUsage)
	}
	if machine == "" && fs.NArg() > 0 {
		machine = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return fmt.Errorf("start: %w\n%s", err, startUsage)
		}
	}
	if machine == "" || fs.NArg() > 0 {
		return errors.New(startUsage)
	}
	d := *dir
	if d == "" {
		d = "~"
		if machine == e.cfg.Machine && e.getwd != nil {
			if wd, err := e.getwd(); err == nil {
				d = wd
			}
		}
	}
	req, err := e.starts.CreateStart(ctx, machine, api.StartIn{Dir: d, Prompt: *prompt})
	if err != nil {
		return fmt.Errorf("start: %s", errText(err))
	}
	fmt.Fprintf(e.out, "%s  starting a session on %s in %s\n", clean(req.ID, 0), clean(req.Machine, 0), clean(req.Dir, 0))
	return e.followStart(ctx, req)
}

// followStart prints each new state of the request until it ends or
// startFollow passes. A request that did not end done exits 1.
func (e *env) followStart(ctx context.Context, req api.StartRequest) error {
	last := ""
	failures := 0
	deadline := e.now().Add(startFollow)
	for {
		if req.State != last {
			fmt.Fprintln(e.out, startLine(req))
			last = req.State
		}
		if req.State != api.ControlPending && req.State != api.ControlClaimed {
			break
		}
		if ctx.Err() != nil {
			fmt.Fprintln(e.out, "stopped following; the session may still start. See the dashboard or sessionhub ls.")
			return &ExitError{Code: 130}
		}
		if !e.now().Before(deadline) {
			fmt.Fprintf(e.out, "still %s after %s; see the dashboard or sessionhub ls\n", clean(req.State, 0), startFollow)
			return &ExitError{Code: 1}
		}
		waited := make(chan struct{})
		go func() { e.pause(startPoll); close(waited) }()
		select {
		case <-ctx.Done():
			continue
		case <-waited:
		}
		next, err := e.starts.GetStart(ctx, req.ID)
		if err == nil {
			req, failures = next, 0
			continue
		}
		if ctx.Err() != nil {
			continue
		}
		var se *client.StatusError
		if errors.As(err, &se) && (se.Status == 401 || se.Status == 403 || se.Status == 404) {
			return fmt.Errorf("start: %s", errText(err))
		}
		if failures++; failures == 3 {
			fmt.Fprintf(e.out, "can't read start request %s: %s\n", clean(req.ID, 0), errText(err))
		}
	}
	if req.State != api.ControlDone {
		return &ExitError{Code: 1}
	}
	return nil
}

// startLine says where a start request is, cleaned for the terminal.
func startLine(r api.StartRequest) string {
	m, detail := clean(r.Machine, 0), clean(r.Detail, 0)
	switch r.State {
	case api.ControlPending:
		return "waiting for the sessionhub watcher on " + m
	case api.ControlClaimed:
		return "starting Claude on " + m
	case api.ControlDone:
		line := "started on " + m
		if r.URL != "" {
			line += ": " + clean(r.URL, 0)
		}
		if detail != "" {
			line += " (" + detail + ")"
		}
		return line
	case api.ControlExpired:
		return "expired: the watcher on " + m + " didn't finish in time"
	}
	if detail == "" {
		detail = "no reason given"
	}
	return clean(r.State, 0) + ": " + detail
}
