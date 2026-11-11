// Package rex runs the rex command, Rex's CLI, for sessions in a Rex
// terminal block. The command speaks the Rex server's WebSocket control API.
package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// callTimeout bounds one rex command.
const callTimeout = 5 * time.Second

// submitPause is the pause between typed text and Enter: Claude reads text
// and Enter that arrive together as one paste, and does not submit it.
const submitPause = 150 * time.Millisecond

// ErrGone is a block call's answer once the session or block is gone.
var ErrGone = errors.New("rex block not found")

// Installed reports whether the rex command is on PATH.
func Installed() bool {
	_, err := exec.LookPath("rex")
	return err == nil
}

// run runs rex with args and returns its stdout. On failure the error is
// what rex printed to stderr, which is also returned.
func run(ctx context.Context, args ...string) (out []byte, stderr string, err error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "rex", args...)
	var eb bytes.Buffer
	cmd.Stderr = &eb
	out, err = cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(eb.String())
		if msg == "" {
			return nil, "", err
		}
		return nil, msg, errors.New(msg)
	}
	return out, "", nil
}

// Call runs `rex block call -s SESSION -b BLOCK METHOD ARGS` and returns its
// JSON result. A session or block that is gone is ErrGone.
func Call(ctx context.Context, session, block, method string, args any) ([]byte, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	res, stderr, err := run(ctx, "block", "call", "-s", session, "-b", block, method, string(body))
	if strings.Contains(stderr, "not found") {
		return nil, ErrGone
	}
	return res, err
}

// ClaudeRuns reports whether a process named claude runs in the block. Rex
// reports process names, not the Claude session ID.
func ClaudeRuns(ctx context.Context, session, block string) (bool, error) {
	res, err := Call(ctx, session, block, "process", map[string]any{})
	if err != nil {
		return false, err
	}
	type proc struct{ Name, Argv0, Script string }
	var out struct {
		Foreground *proc
		Child      *proc
		Ancestors  []proc
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return false, errors.New("bad JSON from rex process")
	}
	procs := out.Ancestors
	for _, p := range []*proc{out.Foreground, out.Child} {
		if p != nil {
			procs = append(procs, *p)
		}
	}
	for _, p := range procs {
		if p.Name == "claude" || strings.Contains(p.Argv0, "claude") || strings.Contains(p.Script, "claude") {
			return true, nil
		}
	}
	return false, nil
}

// Screen returns the block's screen and scrollback as plain text, one line
// per line the program wrote.
// shortcut: Rex has no call for the screen alone, and this one fails with
// too_large once the scrollback passes 960 KiB; page it with max_bytes and
// blob_read if long sessions hit that.
func Screen(ctx context.Context, session, block string) (string, error) {
	res, err := Call(ctx, session, block, "format", map[string]any{"format": "text", "trim": true, "unwrap": true})
	if err != nil {
		return "", err
	}
	var out struct{ Content string }
	if err := json.Unmarshal(res, &out); err != nil {
		return "", errors.New("bad JSON from rex format")
	}
	return out.Content, nil
}

// Write types data into the block.
func Write(ctx context.Context, session, block, data string) error {
	_, err := Call(ctx, session, block, "write", map[string]string{"data": data})
	return err
}

// Submit types text into the block and presses Enter. Text with a newline
// goes in as a bracketed paste, so the newline does not submit half of it.
func Submit(ctx context.Context, session, block, text string) error {
	if strings.Contains(text, "\n") {
		text = "\x1b[200~" + text + "\x1b[201~"
	}
	if err := Write(ctx, session, block, text); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(submitPause):
	}
	return Write(ctx, session, block, "\r")
}

// created is what `rex new --json` and `rex run --json` print.
type created struct {
	SessionID      string   `json:"session_id"`
	BlockIDs       []string `json:"block_ids"`
	InitialWindows []struct {
		BlockIDs []string `json:"block_ids"`
	} `json:"initial_windows"`
}

func (c created) block() string {
	if len(c.InitialWindows) > 0 && len(c.InitialWindows[0].BlockIDs) > 0 {
		return c.InitialWindows[0].BlockIDs[0]
	}
	if len(c.BlockIDs) > 0 {
		return c.BlockIDs[0]
	}
	return ""
}

// start runs a rex command that creates a terminal and returns what it made.
func start(ctx context.Context, args ...string) (created, error) {
	var c created
	res, _, err := run(ctx, args...)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(res, &c); err != nil || c.block() == "" {
		return c, fmt.Errorf("rex %s printed no block ID", args[0])
	}
	return c, nil
}

// New runs argv in a new session labelled label, in dir ("" is Rex's
// default directory), and returns the session and its block. Rex shows the session in no app, so nothing loses
// focus. The block closes when argv exits.
func New(ctx context.Context, label, dir string, argv []string) (session, block string, err error) {
	args := []string{"new", label, "--json"}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	c, err := start(ctx, append(append(args, "--"), argv...)...)
	if err == nil && c.SessionID == "" {
		err = errors.New("rex new printed no session ID")
	}
	return c.SessionID, c.block(), err
}

// Run runs argv in a new, focused tab of the current session ($REX_SESSION,
// else Rex's default) and returns its block. dir "" is Rex's default
// directory.
func Run(ctx context.Context, dir string, argv []string) (block string, err error) {
	args := []string{"run", "--json"}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	c, err := start(ctx, append(append(args, "--"), argv...)...)
	return c.block(), err
}

// Focus focuses the block in its session.
func Focus(ctx context.Context, session, block string) error {
	_, _, err := run(ctx, "block", "focus", "-s", session, "--", block)
	return err
}

// LabelPrefix starts the label of every session sessionhub creates.
const LabelPrefix = "sessionhub: "

// Reap ends session when sessionhub created it and its last block is gone:
// Rex keeps a session whose command exited, empty. Any other session, and
// any failure, changes nothing.
func Reap(ctx context.Context, session string) {
	res, _, err := run(ctx, "block", "ls", "-s", session, "--json")
	var in struct{ Blocks *[]json.RawMessage }
	if err != nil || json.Unmarshal(res, &in) != nil || in.Blocks == nil || len(*in.Blocks) != 0 {
		return
	}
	res, _, err = run(ctx, "ls", "--json")
	var all struct {
		Sessions []struct {
			ID    string `json:"session_id"`
			Label string
		}
	}
	if err != nil || json.Unmarshal(res, &all) != nil {
		return
	}
	for _, s := range all.Sessions {
		if s.ID == session && strings.HasPrefix(s.Label, LabelPrefix) {
			Kill(ctx, session)
		}
	}
}

// Kill ends the session and every program in it.
func Kill(ctx context.Context, session string) error {
	_, _, err := run(ctx, "session", "kill", "--", session)
	return err
}
