package resume

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/store"
)

// Jump time limits.
const (
	jumpSSHTimeout = 15 * time.Second // sessionhub resume on the other machine
	childWaitDelay = 2 * time.Second  // after ctx ends, how long a lingering child may hold the pipes
	paneRunTimeout = 5 * time.Second  // herdr pane run in the new local tab
)

type jumpKind int

const (
	jumpLocal  jumpKind = iota // the session runs on this machine
	jumpRemote                 // ssh, then a local tab on the machine's herdr
	jumpPrint                  // only the resume command can be shown
)

// jumpPlan is what Enter in the inbox pane does for one session.
type jumpPlan struct {
	kind      jumpKind
	sshHost   string // jumpRemote
	herdrHost string // jumpRemote
	tab       string // jumpRemote: the local tab label, sessionhub:<machine>
	why       string // jumpPrint: why only the command can be shown
}

// planJump decides how to reach session s from machine this (the client
// config's machine; "" never matches). machines is the server's list. The
// server fills herdr_host when a machine is added, so a machine without one,
// or missing from the list, means the list failed or the machine was
// removed, and the plan is to show the resume command.
func planJump(s api.Session, this string, machines []api.Machine) jumpPlan {
	if this != "" && s.Machine == this {
		return jumpPlan{kind: jumpLocal}
	}
	for _, m := range machines {
		if m.Name != s.Machine {
			continue
		}
		ssh := orDefault(m.SSHHost, m.Name)
		switch {
		case m.HerdrHost == "":
			return jumpPlan{kind: jumpPrint, why: fmt.Sprintf("machine %s has no herdr_host", strconv.Quote(m.Name))}
		case !jumpHost(ssh) || !jumpHost(m.HerdrHost):
			return jumpPlan{kind: jumpPrint, why: fmt.Sprintf("machine %s has an SSH or herdr host sessionhub won't use", strconv.Quote(m.Name))}
		}
		return jumpPlan{kind: jumpRemote, sshHost: ssh, herdrHost: m.HerdrHost, tab: "sessionhub:" + s.Machine}
	}
	return jumpPlan{kind: jumpPrint, why: fmt.Sprintf("the server lists no machine %s", strconv.Quote(s.Machine))}
}

// jumpHost reports whether h is safe to pass to ssh and to type into a shell
// pane: the server's host rule, and no leading hyphen (an ssh option).
func jumpHost(h string) bool { return store.ValidHost(h) && !strings.HasPrefix(h, "-") }

// Jumper takes the inbox pane (sessionhub inbox --watch) to a session. On this
// machine it does what sessionhub resume does: focus the session's pane, or start
// the session in one. On another machine it runs sessionhub resume there over
// SSH, then focuses or opens a local herdr tab attached to that machine's
// herdr.
type Jumper struct {
	e *env
	// ssh runs command on host; tests replace it.
	ssh func(ctx context.Context, host, command string) error
	// herdrBin runs `herdr pane run` in a new tab.
	herdrBin string
	// alias reports whether this machine's SSH config has a Host entry for
	// name; nil means never. Tests replace it.
	alias func(ctx context.Context, name string) bool
}

// NewJumper returns a Jumper on the client config, the local herdr socket,
// and the real ssh.
func NewJumper() (*Jumper, error) {
	e, err := defaultEnv()
	if err != nil {
		return nil, err
	}
	e.in = strings.NewReader("")
	return &Jumper{e: e, ssh: runSSH, herdrBin: herdrBin(), alias: sshAlias}, nil
}

// Jump returns one line for the inbox pane's status line. An error means
// nothing happened. A message that ends in "run: <command>" means sessionhub could
// only show the command.
func (j *Jumper) Jump(ctx context.Context, s api.Session) (string, error) {
	if !validID(s.ID) {
		return "", fmt.Errorf("refusing session ID %q: expected letters, digits, and hyphens, not starting with a hyphen", clean(s.ID))
	}
	// Like sessionhub resume: until the move ends, a jump could start the session
	// in a second place, and no command is offered.
	if s.Move != nil && api.MoveOpen(s.Move.State) {
		return "", fmt.Errorf("it is moving to %s (%s); resume it after the move ends. To follow it, run: sessionhub move --status %s",
			clean(s.Move.Target), clean(s.Move.State), clean(s.Move.ID))
	}
	var machines []api.Machine
	if j.e.cfg.Machine == "" || s.Machine != j.e.cfg.Machine {
		var err error
		if machines, err = j.e.api.ListMachines(ctx); err != nil {
			return "couldn't read the machine list (" + clean(err.Error()) + "); run: " + s.ResumeCommand, nil
		}
	}
	p := planJump(s, j.e.cfg.Machine, machines)
	if p.kind == jumpRemote {
		p = j.preferAlias(ctx, s.Machine, p)
	}
	switch p.kind {
	case jumpLocal:
		return j.local(s)
	case jumpRemote:
		return j.remote(ctx, s, p), nil
	}
	return p.why + "; run: " + s.ResumeCommand, nil
}

// preferAlias uses a local SSH alias named after the machine, when this
// machine's SSH config has one, in place of the server's ssh_host. The
// server's host is the same for every viewer (for example a Cloudflare
// hostname), while an alias such as `Host tower` with a LAN address is often
// the only route from a particular machine. The herdr host follows when it
// was the same as the SSH host, because herdr --remote connects over SSH.
func (j *Jumper) preferAlias(ctx context.Context, machine string, p jumpPlan) jumpPlan {
	if j.alias == nil || !jumpHost(machine) || p.sshHost == machine || !j.alias(ctx, machine) {
		return p
	}
	if p.herdrHost == p.sshHost {
		p.herdrHost = machine
	}
	p.sshHost = machine
	return p
}

// sshAlias reports whether `ssh -G name` resolves name to a different host
// name, which means the SSH config has a Host entry for it.
func sshAlias(ctx context.Context, name string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-G", "--", name)
	cmd.WaitDelay = childWaitDelay
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return aliasTarget(string(out), name)
}

// aliasTarget reports whether ssh -G output maps name to another host name.
func aliasTarget(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		if h, ok := strings.CutPrefix(line, "hostname "); ok {
			h = strings.TrimSpace(h)
			return h != "" && !strings.EqualFold(h, name)
		}
	}
	return false
}

// local is sessionhub resume on this machine, with its output kept for the status
// line instead of printed.
func (j *Jumper) local(s api.Session) (string, error) {
	var out bytes.Buffer
	e := *j.e
	e.out = &out
	acted, err := e.local(s)
	if errors.Is(err, errNeedsForce) {
		return "", fmt.Errorf("the server reports it %s but no pane runs it; run: sessionhub resume --force %s", clean(s.Status), s.ID)
	}
	if err != nil {
		return "", err
	}
	if !acted {
		// e.local printed "<reason>. Run this in a shell:" and the command.
		why := strings.TrimSuffix(firstLine(out.String()), ". Run this in a shell:")
		if why == "" {
			why = "no herdr pane to use"
		}
		return why + "; run: " + e.resumeCmd(s), nil
	}
	return firstLine(out.String()), nil
}

// remote runs sessionhub resume on the session's machine, so its herdr focuses or
// starts the session, then shows that herdr in the local tab sessionhub:<machine>.
func (j *Jumper) remote(ctx context.Context, s api.Session, p jumpPlan) string {
	sctx, cancel := context.WithTimeout(ctx, jumpSSHTimeout)
	defer cancel()
	if err := j.ssh(sctx, p.sshHost, remoteHub+" resume "+s.ID); err != nil {
		// ssh exits with the remote command's status, and with 255 when ssh
		// itself failed (no route, no key). Any other positive exit status
		// means the remote sessionhub resume ran and failed. A killed ssh (the
		// timeout) has status -1 and is ssh's failure.
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() > 0 && ee.ExitCode() != 255 {
			return fmt.Sprintf("sessionhub resume on %s failed (%v); run: %s", s.Machine, err, s.ResumeCommand)
		}
		return fmt.Sprintf("ssh %s failed (%v); run: %s", p.sshHost, err, s.ResumeCommand)
	}
	if err := j.openTab(ctx, p); err != nil {
		return fmt.Sprintf("sessionhub resume ran on %s, but the local tab failed (%v); run: herdr --remote %s", s.Machine, err, shellQuote(p.herdrHost))
	}
	return fmt.Sprintf("sessionhub resume ran on %s; showing it in tab %s", s.Machine, p.tab)
}

// openTab focuses the local tab named p.tab, or creates it, focused, and
// runs herdr --remote <herdr_host> in it. tab.create can't run a command,
// so the command goes in with `herdr pane run`, which types it and presses
// Enter.
func (j *Jumper) openTab(ctx context.Context, p jumpPlan) error {
	h, err := herdr.Dial(j.e.socket)
	if err != nil {
		return err
	}
	tabs, err := h.TabList()
	if err != nil {
		return err
	}
	for _, t := range tabs {
		if t.Label == p.tab {
			return h.TabFocus(t.TabID)
		}
	}
	created, err := h.TabCreate(herdr.TabCreateParams{Label: p.tab, Focus: true})
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, paneRunTimeout)
	defer cancel()
	cmd := exec.CommandContext(pctx, j.herdrBin, "pane", "run", created.RootPane.PaneID, "herdr --remote "+shellQuote(p.herdrHost))
	cmd.WaitDelay = childWaitDelay
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("herdr pane run: %w: %s", err, termtext.Clean(string(out), 200))
	}
	return nil
}

// runSSH runs command on host with BatchMode, so ssh never waits on a
// password prompt, and returns ssh's error with its cleaned stderr. The
// remote shell expands the ~ in the command.
func runSSH(ctx context.Context, host, command string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", host, command)
	// The inbox view holds the terminal's stdin, so ssh gets /dev/null
	// (nil Stdin and Stdout) and only its stderr is kept.
	cmd.Stdin, cmd.Stdout = nil, nil
	cmd.WaitDelay = childWaitDelay
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := termtext.Clean(stderr.String(), 200); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// firstLine is the first non-empty line of s, cleaned.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = clean(l); l != "" {
			return l
		}
	}
	return ""
}

// herdrBin is HERDR_BIN_PATH, else "herdr".
func herdrBin() string {
	if b := os.Getenv("HERDR_BIN_PATH"); b != "" {
		return b
	}
	return "herdr"
}
