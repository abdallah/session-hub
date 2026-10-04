package hooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

// upsertFor builds the registration used when the server answers 404 to an
// event: the session is unknown, for example because hooks were installed
// while it was already running.
type upsertFor func(ctx context.Context, id string) api.SessionUpsert

// deliver sends items in order. At the first failure it queues that item and
// every later one, and does not drain (the server just failed). When every
// item was sent and no watcher runs, it drains the queue.
func (h *handler) deliver(ctx context.Context, items []client.Item, mk upsertFor) error {
	c, err := h.newClient()
	if err != nil {
		return h.enqueue(items, err)
	}
	for i, it := range items {
		if err := sendItem(ctx, c, it, mk); err != nil {
			// Auth errors are kept like retryable ones: fixing the token
			// makes the same items succeed.
			if !client.IsRetryable(err) && !client.IsAuthError(err) {
				fmt.Fprintf(h.stderr, "hook: dropping %s: %v\n", it.Op, err)
				continue
			}
			return h.enqueue(items[i:], err)
		}
	}
	if !h.watcherRunning() {
		h.drain(ctx, c, mk)
	}
	return nil
}

func (h *handler) enqueue(items []client.Item, cause error) error {
	var errs []error
	for _, it := range items {
		if err := h.queue.Append(it); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("hook: queue: %w", errors.Join(errs...))
	}
	fmt.Fprintf(h.stderr, "hook: queued %d item(s): %v\n", len(items), cause)
	return nil
}

// drain sends up to drainMaxItems queued items within drainBudget in total
// (one deadline for the pass, so the queue lock stays short and a concurrent
// append never waits past SessionEnd's 1.5 s). It stops at the first retryable
// or auth failure and keeps that item and every later one. It also keeps every
// plugin item that has a pane but no session ID: only the watcher can resolve
// those, so this drainer must neither send nor drop them.
func (h *handler) drain(ctx context.Context, c *client.Client, mk upsertFor) {
	ctx, cancel := context.WithTimeout(ctx, drainBudget)
	defer cancel()
	err := h.queue.Drain(func(items []client.Item) []client.Item {
		var keep []client.Item
		sent := 0
		for i, it := range items {
			if it.SessionID == "" && it.PaneID != "" {
				keep = append(keep, it)
				continue
			}
			if sent >= drainMaxItems || ctx.Err() != nil {
				return append(keep, items[i:]...)
			}
			sent++
			if err := sendItem(ctx, c, it, mk); err != nil {
				if client.IsAuthError(err) {
					fmt.Fprintf(h.stderr, "hook: drain stopped, server rejected the token: %v\n", err)
					return append(keep, items[i:]...)
				}
				if !client.IsRetryable(err) {
					fmt.Fprintf(h.stderr, "hook: dropping queued %s: %v\n", it.Op, err)
					continue
				}
				return append(keep, items[i:]...)
			}
		}
		return keep
	})
	if err != nil {
		fmt.Fprintf(h.stderr, "hook: drain: %v\n", err)
	}
}

// sendItem replays one item through the shared client.Replay. For an event,
// report, or title answered with 404 it upserts the session and retries the
// item once.
func sendItem(ctx context.Context, c *client.Client, it client.Item, mk upsertFor) error {
	err := client.Replay(ctx, c, it)
	if !client.IsNotFound(err) || mk == nil {
		return err
	}
	switch it.Op {
	case client.OpEvent, client.OpReport, client.OpTitle:
	default:
		return err
	}
	if err := c.UpsertSession(ctx, mk(ctx, it.SessionID)); err != nil {
		return err
	}
	return client.Replay(ctx, c, it)
}

// watcherRunning reports whether another process holds <state>/watcher.lock,
// using a non-blocking flock that is released at once.
func watcherRunning(stateDir string) bool {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "watcher.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err == syscall.EWOULDBLOCK
	}
	return false // we hold it now; closing the file releases it
}

// claudePID returns the nearest ancestor that is the Claude Code binary, else
// the parent PID.
func claudePID() int {
	pid := os.Getppid()
	for cur, hops := pid, 0; cur > 1 && hops < 32; hops++ {
		if isClaudeExe(exeOf(cur)) {
			return cur
		}
		cur = ppidOf(cur)
	}
	return pid
}

func isClaudeExe(exe string) bool {
	if exe == "" {
		return false
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	return strings.HasPrefix(filepath.Base(exe), "claude") || strings.Contains(exe, "/share/claude/versions/")
}
