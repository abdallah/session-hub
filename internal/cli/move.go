package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/move"
)

// moveAPI is the part of *client.Client that sessionhub move uses.
type moveAPI interface {
	GetSession(ctx context.Context, idOrPrefix string) (api.SessionDetail, error)
	CreateMove(ctx context.Context, sessionID, target string) (api.Move, error)
	GetMove(ctx context.Context, id string) (api.Move, error)
}

var _ moveAPI = (*client.Client)(nil)

const (
	// moveFollow is how long sessionhub move prints a move's progress.
	moveFollow = 3 * time.Minute
	// movePoll is how often it reads the move.
	movePoll = 2 * time.Second
)

const moveUsage = `usage:
  sessionhub move <id-or-prefix> <machine|cloud>
  sessionhub move --status <move-id>`

// RunMove implements `sessionhub move`.
func RunMove(ctx context.Context, args []string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWith(ctx, args, (*env).move)
}

// RunMoveKey implements `sessionhub move-key`: it prints this machine's move key
// fingerprint, creating the key when there is none.
func RunMoveKey(_ context.Context, args []string) error {
	return moveKey(os.Stdout, move.KeyPath(), args)
}

func moveKey(out io.Writer, path string, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: sessionhub move-key")
	}
	k, err := move.LoadOrCreateKey(path)
	if err != nil {
		return fmt.Errorf("move-key: %w", err)
	}
	_, err = fmt.Fprintln(out, move.Fingerprint(k.PublicKey()))
	return err
}

func (e *env) move(ctx context.Context, args []string) error {
	switch {
	case len(args) == 2 && args[0] == "--status":
		mv, err := e.moves.GetMove(ctx, args[1])
		if err != nil {
			return fmt.Errorf("move: %s", errText(err))
		}
		fmt.Fprintln(e.out, clean(mv.ID, 0)+"  "+moveLine(mv))
		return nil
	case len(args) == 2 && !strings.HasPrefix(args[0], "-"):
		d, err := e.moves.GetSession(ctx, args[0])
		if err != nil {
			return fmt.Errorf("move: %s: %s", clean(args[0], 0), errText(err))
		}
		mv, err := e.moves.CreateMove(ctx, d.ID, args[1])
		if err != nil {
			return fmt.Errorf("move: %s", errText(err))
		}
		fmt.Fprintf(e.out, "%s  moving %s (%s) from %s to %s\n", clean(mv.ID, 0), shortID(d.ID), clean(title(d.Session), 0),
			clean(mv.Source, 0), clean(mv.Target, 0))
		return e.followMove(ctx, mv)
	}
	return fmt.Errorf("move: unexpected arguments\n%s", moveUsage)
}

// followMove prints each new state of the move until it ends or moveFollow
// passes. A move that did not end done exits 1.
func (e *env) followMove(ctx context.Context, mv api.Move) error {
	last := ""
	failures := 0
	deadline := e.now().Add(moveFollow)
	for {
		if mv.State != last {
			fmt.Fprintln(e.out, moveLine(mv))
			last = mv.State
		}
		if !api.MoveOpen(mv.State) {
			break
		}
		if ctx.Err() != nil {
			return e.moveInterrupted(mv)
		}
		if !e.now().Before(deadline) {
			fmt.Fprintf(e.out, "still %s after 3 minutes; check with: sessionhub move --status %s\n", clean(mv.State, 0), clean(mv.ID, 0))
			return nil
		}
		waited := make(chan struct{})
		go func() { e.pause(movePoll); close(waited) }()
		select {
		case <-ctx.Done():
			return e.moveInterrupted(mv)
		case <-waited:
		}
		next, err := e.moves.GetMove(ctx, mv.ID)
		if err == nil {
			mv, failures = next, 0
			continue
		}
		if ctx.Err() != nil {
			return e.moveInterrupted(mv)
		}
		var se *client.StatusError
		if errors.As(err, &se) && (se.Status == 401 || se.Status == 403 || se.Status == 404) {
			return fmt.Errorf("move: %s", errText(err))
		}
		if failures++; failures == 3 {
			fmt.Fprintf(e.out, "can't read move %s: %s\n", clean(mv.ID, 0), errText(err))
		}
	}
	// The source may add a note after this (an archive or a restart that
	// failed there).
	if mv.State == api.MoveDone || mv.State == api.MoveFailed {
		fmt.Fprintf(e.out, "see notes later with: sessionhub move --status %s\n", clean(mv.ID, 0))
	}
	if mv.State != api.MoveDone {
		return &ExitError{Code: 1}
	}
	return nil
}

// moveLine says where a move is, cleaned for the terminal.
func moveLine(mv api.Move) string {
	src, dst, detail := clean(mv.Source, 0), clean(mv.Target, 0), clean(mv.Detail, 0)
	switch mv.State {
	case api.MoveRequested:
		return "requested: waiting for the sessionhub watcher on " + src
	case api.MovePacking:
		if mv.Target == api.MoveCloud {
			return "packing on " + src + ": ending the session and starting the cloud session"
		}
		return "packing on " + src + ": ending the session and sealing the bundle"
	case api.MoveUploaded:
		return fmt.Sprintf("uploaded (%.1f MiB): waiting for %s", float64(mv.BundleSize)/(1<<20), dst)
	case api.MoveUnpacking:
		return "unpacking on " + dst
	case api.MoveDone:
		line := "done: the session runs on " + dst + " now"
		if mv.Target == api.MoveCloud {
			line = "done: the session continues in the cloud at " + clean(mv.CloudURL, 0)
		}
		if detail != "" {
			line += "; " + detail
		}
		return line
	}
	if detail == "" {
		detail = "no reason given"
	}
	return clean(mv.State, 0) + ": " + detail
}

// moveInterrupted reports that the user stopped following; the move goes on.
func (e *env) moveInterrupted(mv api.Move) error {
	id := clean(mv.ID, 0)
	fmt.Fprintf(e.out, "move %s continues; check with: sessionhub move --status %s\n", id, id)
	return &ExitError{Code: 130}
}
