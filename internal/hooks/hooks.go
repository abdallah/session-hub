// Package hooks is the Claude Code hooks client: `sessionhub hook <event>` handles
// one hook call, and `sessionhub install-hooks` / `sessionhub uninstall-hooks` edit Claude
// Code's settings.json.
package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/gitinfo"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/paths"
)

const (
	maxStdin      = 4 << 20
	promptChars   = 200
	drainMaxItems = 50
	drainBudget   = time.Second     // total time of one drain pass
	hookDeadline  = 5 * time.Second // whole `sessionhub hook` invocation

	stateIdle    = "idle"
	stateWorking = "working"
	stateBlocked = "blocked"
)

// stdinPayload holds the hook stdin fields sessionhub reads. Unknown fields are
// ignored, so a newer Claude Code does not break the hook.
type stdinPayload struct {
	SessionID    string `json:"session_id"`
	CWD          string `json:"cwd"`
	AgentID      string `json:"agent_id"`
	Prompt       string `json:"prompt"`
	SessionTitle string `json:"session_title"`
	Reason       string `json:"reason"`
	// NotificationType is set on Notification hooks.
	NotificationType string `json:"notification_type"`
}

// handler holds everything a hook call touches, so tests can replace it.
type handler struct {
	stdin    io.Reader
	getenv   func(string) string
	now      func() time.Time
	stateDir string
	queue    *client.Queue
	// newClient returns an error when the client config is unusable; the
	// items are then queued.
	newClient func() (*client.Client, error)
	git       func(ctx context.Context, cwd string) (repo, branch string)
	claudePID func() int
	// watcherRunning reports whether the herdr plugin watcher holds
	// watcher.lock. When it does, the watcher drains the queue and the hook
	// does not.
	watcherRunning func() bool
	// flushCmd builds the detached child that drains the queue after
	// session-end.
	flushCmd func() *exec.Cmd
	// digestCmd builds the detached `sessionhub digest <id>` child that the stop
	// and session-end hooks start. nil skips it.
	digestCmd func(id string) *exec.Cmd
	// deadline bounds the whole invocation: live sends and the drain. The
	// context and permission-request hooks do not use it.
	deadline time.Duration
	stderr   io.Writer
	// stdout carries the hook output JSON. Only the context and
	// permission-request hooks write to it.
	stdout io.Writer
	// refreshCmd builds the detached `sessionhub hook refresh-instructions` child
	// that session-start starts. nil skips it.
	refreshCmd func() *exec.Cmd
	// remoteOff reports whether this machine opted out of remote permission
	// answers.
	remoteOff func() bool
	// permWait bounds the permission hook's whole wait; pollWait is one
	// decision long poll. A poll that answers 204 sooner than pollPause is
	// followed by a pause, so a server that does not hold the poll open is
	// not asked in a tight loop.
	permWait, pollWait, pollPause time.Duration
}

func defaultHandler() *handler {
	state := paths.StateDir()
	return &handler{
		stdin:    os.Stdin,
		getenv:   os.Getenv,
		now:      time.Now,
		stateDir: state,
		queue:    client.NewQueue(state),
		newClient: func() (*client.Client, error) {
			cfg, err := client.LoadConfig()
			if err != nil {
				return nil, err
			}
			return client.New(cfg)
		},
		git:            gitinfo.Lookup,
		claudePID:      claudePID,
		watcherRunning: func() bool { return watcherRunning(state) },
		flushCmd:       flushCommand,
		digestCmd:      digestCommand,
		deadline:       hookDeadline,
		stderr:         os.Stderr,
		stdout:         os.Stdout,
		refreshCmd:     refreshCommand,
		remoteOff: func() bool {
			cfg, err := client.LoadConfig()
			if err != nil {
				return false // newClient fails the same way, and the hook prints nothing
			}
			return client.RemotePermissionsOff(cfg, os.Getenv)
		},
		permWait:  permissionWait,
		pollWait:  decisionPoll,
		pollPause: decisionPause,
	}
}

// Run handles `sessionhub hook <event>`. The caller (cmd/sessionhub) exits 0 whatever it
// returns; the error is only for the log.
func Run(ctx context.Context, args []string) error {
	return defaultHandler().run(ctx, args)
}

func (h *handler) run(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: sessionhub hook <session-start|prompt|stop|notification|session-end|context|permission-request>")
	}
	event := args[0]
	// These two run outside the 5 s deadline: context never touches the
	// network, and permission-request waits up to permWait for an answer.
	switch event {
	case "context":
		return h.printContext()
	case "permission-request":
		return h.permissionRequest(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, h.deadline)
	defer cancel()
	if event == "flush" {
		return h.flush(ctx)
	}
	if event == "refresh-instructions" {
		return h.refreshInstructions(ctx)
	}
	switch event {
	case "session-start", "prompt", "stop", "notification", "session-end":
	default:
		return fmt.Errorf("hook: unknown event %q", event)
	}
	raw, err := io.ReadAll(io.LimitReader(h.stdin, maxStdin))
	if err != nil {
		return fmt.Errorf("hook %s: read stdin: %w", event, err)
	}
	var in stdinPayload
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("hook %s: bad stdin JSON: %w", event, err)
	}
	if in.AgentID != "" {
		return nil // subagent call: the parent session already reports
	}
	if in.SessionID == "" {
		return fmt.Errorf("hook %s: stdin has no session_id", event)
	}

	var items []client.Item
	switch event {
	case "session-start":
		if err := h.writeCurrent(in.SessionID); err != nil {
			fmt.Fprintf(h.stderr, "hook: write current/: %v\n", err)
		}
		items = []client.Item{h.upsertItem(h.fullUpsert(ctx, in))}
		h.startRefresh()
	case "prompt":
		text := cleanText(in.Prompt, promptChars)
		items = []client.Item{
			h.upsertItem(api.SessionUpsert{
				ID: in.SessionID, Agent: "claude", Source: api.SourceHooks,
				CWD: foldControl(in.CWD), HerdrPane: h.getenv("HERDR_PANE_ID"),
				AgentState: stateWorking, FirstPrompt: text,
			}),
			h.eventItem(in.SessionID, api.KindPrompt, map[string]string{"prompt": text}),
			h.stateChanged(in.SessionID, stateWorking),
		}
	case "stop":
		h.startDigest(in.SessionID)
		items = []client.Item{h.stateChanged(in.SessionID, stateIdle)}
	case "notification":
		// Only a notification that waits on the user blocks the session.
		// idle_prompt, auth_success, and future types send nothing.
		if !blocksSession(in.NotificationType) {
			return nil
		}
		items = []client.Item{h.stateChanged(in.SessionID, stateBlocked)}
	case "session-end":
		return h.sessionEnd(in)
	}
	return h.deliver(ctx, items, func(ctx context.Context, id string) api.SessionUpsert {
		if id == in.SessionID {
			return h.fullUpsert(ctx, in)
		}
		return api.SessionUpsert{ID: id, Agent: "claude", Source: api.SourceHooks}
	})
}

// blocksSession reports whether a notification type means Claude waits on the
// user.
func blocksSession(t string) bool {
	return t == "permission_prompt" || t == "elicitation_dialog"
}

// fullUpsert builds the registration for a session from hook input and the
// environment. It runs git, so callers build it only when needed.
func (h *handler) fullUpsert(ctx context.Context, in stdinPayload) api.SessionUpsert {
	up := api.SessionUpsert{
		ID:             in.SessionID,
		Agent:          "claude",
		Source:         api.SourceHooks,
		CWD:            foldControl(in.CWD),
		HerdrPane:      h.getenv("HERDR_PANE_ID"),
		HerdrWorkspace: h.getenv("HERDR_WORKSPACE_ID"),
		TitleHint:      cleanText(in.SessionTitle, promptChars),
	}
	if in.CWD != "" {
		up.GitRepo, up.GitBranch = h.git(ctx, in.CWD)
	}
	if sock := h.getenv("HERDR_SOCKET_PATH"); sock != "" {
		up.HerdrSession = herdr.SessionName(sock)
	} else if up.HerdrPane != "" {
		up.HerdrSession = herdr.SessionName(herdr.SocketPath())
	}
	return up
}

// sessionEnd queues the ended event and never touches the network: Claude
// Code gives all SessionEnd hooks 1.5 s.
func (h *handler) sessionEnd(in stdinPayload) error {
	h.removeCurrent(in.SessionID)
	if err := h.queue.Append(h.eventItem(in.SessionID, api.KindEnded, map[string]string{"reason": in.Reason})); err != nil {
		return err
	}
	// The queued event would otherwise wait for the next hook or the watcher.
	// A detached child sends it; this process does not wait.
	if err := h.startFlush(); err != nil {
		fmt.Fprintf(h.stderr, "hook: start flush: %v\n", err)
	}
	h.startDigest(in.SessionID)
	return nil
}

func (h *handler) startFlush() error {
	return startDetached(h.flushCmd())
}

// startDigest starts `sessionhub digest <id>` without waiting. The child reads the
// transcript and runs git, which can take longer than a hook may.
func (h *handler) startDigest(id string) {
	if h.digestCmd == nil {
		return
	}
	if err := startDetached(h.digestCmd(id)); err != nil {
		fmt.Fprintf(h.stderr, "hook: start digest: %v\n", err)
	}
}

// startDetached starts cmd in its own session without waiting for it. A nil
// command does nothing.
func startDetached(cmd *exec.Cmd) error {
	if cmd == nil {
		return nil
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil // /dev/null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// flushCommand is `sessionhub hook flush`, run by the same binary.
func flushCommand() *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	return exec.Command(exe, "hook", "flush")
}

// digestCommand is `sessionhub digest <id>`, run by the same binary.
func digestCommand(id string) *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	return exec.Command(exe, "digest", id)
}

// refreshCommand is `sessionhub hook refresh-instructions`, run by the same binary.
func refreshCommand() *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	return exec.Command(exe, "hook", "refresh-instructions")
}

// startRefresh starts the rules refresh without waiting for it.
func (h *handler) startRefresh() {
	if h.refreshCmd == nil {
		return
	}
	if err := startDetached(h.refreshCmd()); err != nil {
		fmt.Fprintf(h.stderr, "hook: start rules refresh: %v\n", err)
	}
}

// flush drains the queue unless a watcher holds watcher.lock. It exits
// quietly when the queue is empty or the client is not configured.
func (h *handler) flush(ctx context.Context) error {
	if h.watcherRunning() {
		return nil
	}
	if n, err := h.queue.Len(); err != nil || n == 0 {
		return err
	}
	c, err := h.newClient()
	if err != nil {
		return err
	}
	h.drain(ctx, c, func(_ context.Context, id string) api.SessionUpsert {
		return api.SessionUpsert{ID: id, Agent: "claude", Source: api.SourceHooks}
	})
	return nil
}

func (h *handler) upsertItem(up api.SessionUpsert) client.Item {
	b, _ := json.Marshal(up)
	return client.Item{Op: client.OpUpsert, SessionID: up.ID, PaneID: up.HerdrPane, Body: b}
}

func (h *handler) eventItem(id, kind string, payload any) client.Item {
	p, _ := json.Marshal(payload)
	b, _ := json.Marshal(api.EventIn{Kind: kind, Source: api.SourceHooks, TS: h.now().UTC(), Payload: p})
	return client.Item{Op: client.OpEvent, SessionID: id, Body: b}
}

func (h *handler) stateChanged(id, state string) client.Item {
	return h.eventItem(id, api.KindStateChanged, map[string]string{"agent_state": state})
}

// foldControl replaces every control character (unicode.IsControl, the set
// the server rejects) with a space.
func foldControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// cleanText folds control characters to spaces, collapses runs of whitespace,
// and cuts the result to n runes. The server rejects control characters, so a
// multi-line prompt sent as is would fail the whole upsert.
func cleanText(s string, n int) string {
	return strings.TrimSpace(truncate(strings.Join(strings.Fields(foldControl(s)), " "), n))
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (h *handler) currentFile(pid int) string {
	return filepath.Join(h.stateDir, "current", fmt.Sprint(pid))
}

func (h *handler) writeCurrent(sessionID string) error {
	dir := filepath.Join(h.stateDir, "current")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(sessionID); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), h.currentFile(h.claudePID()))
}

// removeCurrent removes current/<pid> unless it already names another
// session (after /clear the new session may have written it first).
func (h *handler) removeCurrent(sessionID string) {
	f := h.currentFile(h.claudePID())
	b, err := os.ReadFile(f)
	if err != nil {
		return
	}
	if strings.TrimSpace(string(b)) == sessionID {
		_ = os.Remove(f)
	}
}
