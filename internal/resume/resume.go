// Package resume gets you back into a session: it focuses or restarts the
// session in the local herdr, or prints the commands to reach a session on
// another machine. It never runs anything remote.
package resume

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
)

// hubAPI is the part of *client.Client that resume uses.
type hubAPI interface {
	GetSession(ctx context.Context, idOrPrefix string) (api.SessionDetail, error)
	ListSessions(ctx context.Context, live bool, machine string) ([]api.Session, error)
	ListMachines(ctx context.Context) ([]api.Machine, error)
	CreateControl(ctx context.Context, id string) (api.ControlRequest, error)
}

// env holds the dependencies of one resume run; tests build their own.
type env struct {
	cfg        client.Config
	api        hubAPI
	socket     string // herdr socket path
	callerPane string // HERDR_PANE_ID, may be empty
	saved      func(ctx context.Context) []SavedMachine
	in         io.Reader
	out        io.Writer
	force      bool // --force: start a session the server reports live
	// remoteControl (--remote-control) starts Claude with Remote Control on.
	remoteControl bool
	// pollEvery and pollFor bound `sessionhub remote-control`'s wait for the URL;
	// zero means the defaults. Tests shorten them.
	pollEvery, pollFor time.Duration
	// ctlEvery and ctlFor bound the wait for a request that another
	// machine's watcher runs; zero means 2 s and 2 min. Tests shorten them.
	ctlEvery, ctlFor time.Duration
}

func defaultEnv() (*env, error) {
	cfg, err := client.LoadConfig()
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg)
	if err != nil {
		return nil, err
	}
	c.SetTimeout(client.InteractiveTimeout)
	return &env{
		cfg:        cfg,
		api:        c,
		socket:     herdr.SocketPath(),
		callerPane: os.Getenv("HERDR_PANE_ID"),
		saved:      listSavedMachines,
		in:         os.Stdin,
		out:        os.Stdout,
	}, nil
}

const usage = "usage: sessionhub resume [--force] [--remote-control] <id-prefix> | sessionhub resume --pick [--force] [--remote-control]"

// Run implements `sessionhub resume <id-prefix>` and `sessionhub resume --pick`.
func Run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pick := fs.Bool("pick", false, "choose a session from a numbered list")
	force := fs.Bool("force", false, "start a session the server reports live even though its pane can't be focused")
	rc := fs.Bool("remote-control", false, "start Claude with Remote Control on")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%s: %w", usage, err)
	}
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	e.force = *force
	e.remoteControl = *rc
	if *pick {
		if fs.NArg() != 0 {
			return errors.New(usage)
		}
		return e.pick(ctx)
	}
	if fs.NArg() != 1 {
		return errors.New(usage)
	}
	_, err = e.resume(ctx, fs.Arg(0))
	return err
}

// RunOpenPicker implements `sessionhub plugin open-picker`: it opens the herdr
// plugin's resume-picker overlay pane.
func RunOpenPicker(ctx context.Context, args []string) error {
	return openPicker(ctx, herdrBin(), os.Stdout, os.Stderr)
}

// RunOpenInbox implements `sessionhub plugin open-inbox`: it opens the herdr
// plugin's inbox pane, which runs sessionhub inbox --watch, to the right.
func RunOpenInbox(ctx context.Context, args []string) error {
	if err := openInbox(ctx, herdrBin(), os.Stdout, os.Stderr); err != nil {
		return err
	}
	host, _ := os.Hostname()
	if note := outsideHerdrNote(os.Getenv, host); note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	return nil
}

// outsideHerdrNote warns when the command runs in a shell that is not in a
// herdr pane, for example over SSH from another machine's herdr: the pane
// then opens in this machine's herdr, which you may not be looking at.
func outsideHerdrNote(getenv func(string) string, host string) string {
	if getenv("HERDR_ENV") != "" {
		return ""
	}
	return "Opened the inbox pane in herdr on " + host + ". This shell is not in a herdr pane, so you may not see it; " +
		"run sessionhub plugin open-inbox in the herdr you are looking at."
}

// resume resolves the prefix and resumes the session. acted is true when it
// changed herdr (focus or new pane) and false when it only printed commands.
func (e *env) resume(ctx context.Context, prefix string) (acted bool, err error) {
	d, err := e.api.GetSession(ctx, prefix)
	if err != nil {
		return false, err
	}
	return e.resumeSession(ctx, d.Session)
}

// validID accepts only what a Claude session ID is made of (a UUID), so an ID
// can never become an option or shell syntax in a printed command.
func validID(id string) bool {
	if id == "" || id[0] == '-' {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// validHost refuses an empty host or one that ssh or herdr would read as an
// option.
func validHost(h string) bool { return h != "" && !strings.HasPrefix(h, "-") }

func (e *env) resumeSession(ctx context.Context, s api.Session) (bool, error) {
	if !validID(s.ID) {
		return false, fmt.Errorf("refusing session ID %q: expected letters, digits, and hyphens, not starting with a hyphen", s.ID)
	}
	if err := e.guardMove(s); err != nil {
		return false, err
	}
	if e.cfg.Machine != "" && s.Machine == e.cfg.Machine {
		return e.local(s)
	}
	return false, e.remote(ctx, s)
}

// claudeArgs is the argument list that starts Claude on the session.
func (e *env) claudeArgs(s api.Session) []string {
	if e.remoteControl {
		return remoteControlArgs(s.ID)
	}
	return []string{"--resume", s.ID}
}

// agentName is the herdr agent name sessionhub gives a Claude it starts.
func agentName(id string) string { return "sessionhub-" + strings.ToLower(shortID(id)) }

// remoteControlArgs starts Claude on session id with Remote Control on.
func remoteControlArgs(id string) []string { return []string{"--resume", id, "--remote-control"} }

func (e *env) resumeCmd(s api.Session) string {
	cmd := "claude --resume " + shellQuote(s.ID)
	if e.remoteControl {
		cmd += " --remote-control"
	}
	if s.CWD != "" {
		cmd = "cd " + shellQuote(s.CWD) + " && " + cmd
	}
	return cmd
}

// local resumes a session that belongs to this machine.
func (e *env) local(s api.Session) (bool, error) {
	fallback := func(why string) (bool, error) {
		if isLive(s) {
			fmt.Fprintf(e.out, "Warning: the server reports session %s as %s. If it still runs somewhere, the command below starts a second copy.\n", shortID(s.ID), clean(s.Status))
		}
		fmt.Fprintf(e.out, "%s. Run this in a shell:\n%s\n", why, e.resumeCmd(s))
		return false, nil
	}
	if s.HerdrSession == "" && s.HerdrPane == "" {
		return fallback("The session has no herdr pane recorded")
	}
	h, err := herdr.Dial(e.socket)
	if err != nil {
		return fallback("herdr is not running")
	}

	// The session's pane, when it still holds this session. Pane IDs belong to
	// one herdr session, so skip the lookup when the session differs.
	sameServer := s.HerdrSession == "" || s.HerdrSession == herdr.SessionName(e.socket)
	if s.HerdrPane != "" && sameServer {
		p, err := h.PaneGet(s.HerdrPane)
		var he *herdr.Error
		switch {
		case err == nil && p.SessionID() == s.ID && p.Agent == "claude":
			// herdr detects Claude there and it runs this session: focus it.
			if err := h.PaneFocus(s.HerdrPane); err != nil {
				return false, fmt.Errorf("focus pane %s: %w", clean(s.HerdrPane), err)
			}
			fmt.Fprintf(e.out, "Focused pane %q, which still runs session %s.\n", s.HerdrPane, shortID(s.ID))
			if e.remoteControl {
				fmt.Fprintf(e.out, "It is already running, so --remote-control has no effect. To turn Remote Control on there, run: sessionhub remote-control %s\n", s.ID)
			}
			return true, nil
		case err == nil && p.SessionID() == s.ID && p.Agent == "":
			// The pane kept this session's agent_session but herdr detects no
			// agent: it is at a shell prompt (a restored pane). Resume there.
			if err := e.guardLive(s); err != nil {
				return false, err
			}
			return e.startIn(h, s, s.HerdrPane, "its pane", true)
		case err == nil, errors.As(err, &he) && he.Code == "pane_not_found":
			// The pane is gone or runs something else: start a new one.
		default:
			return false, fmt.Errorf("look up pane %s: %w", clean(s.HerdrPane), err)
		}
	}

	if err := e.guardLive(s); err != nil {
		return false, err
	}
	target := e.callerPane
	if target == "" {
		snap, err := h.Snapshot()
		if err != nil {
			return false, fmt.Errorf("read herdr snapshot: %w", err)
		}
		target = snap.FocusedPaneID
	}
	pane, err := h.PaneSplit(herdr.PaneSplitParams{
		Direction:    "right",
		CWD:          s.CWD,
		TargetPaneID: target,
		Focus:        true,
	})
	if err != nil {
		return false, fmt.Errorf("split pane: %w\nRun this in a shell instead:\n%s", err, e.resumeCmd(s))
	}
	return e.startIn(h, s, pane.PaneID, "new pane", false)
}

// errMoving is returned when the session has a move under way.
var errMoving = errors.New("refusing to resume a session while it moves")

// guardMove refuses a session with an open move, --force or not: until the
// move ends, the source or the target may start it, and a resume here would
// run it in two places.
func (e *env) guardMove(s api.Session) error {
	if s.Move == nil || !api.MoveOpen(s.Move.State) {
		return nil
	}
	fmt.Fprintf(e.out, "Session %s is moving to %s (%s). Resume it after the move ends. To follow the move, run:\n"+
		"sessionhub move --status %s\n", shortID(s.ID), clean(s.Move.Target), clean(s.Move.State), clean(s.Move.ID))
	return errMoving
}

// errNeedsForce is returned when resume would start a second copy of a
// session the server reports live.
var errNeedsForce = errors.New("refusing to start a second copy of a live session without --force")

func isLive(s api.Session) bool { return s.Status == api.StatusLive || s.Status == api.StatusBlocked }

// guardLive stops a start when the server reports the session live or
// blocked but no pane could be focused: it may still run elsewhere (another
// herdr server, a plain terminal), and a second `claude --resume` of one
// session writes to the same transcript. --force skips the check.
func (e *env) guardLive(s api.Session) error {
	if e.force || !isLive(s) {
		return nil
	}
	fmt.Fprintf(e.out, "Warning: the server reports session %s as %s, but sessionhub found no pane running it to focus.\n"+
		"Starting it again may run a second copy of the same session. If it isn't running anywhere, run:\n"+
		"sessionhub resume --force %s\n", shortID(s.ID), clean(s.Status), s.ID)
	return errNeedsForce
}

// startIn runs claude --resume in a pane at a shell prompt. agent.start never
// moves focus, so focus asks for a pane.focus afterwards (a split made with
// focus: true needs none). which names the pane in the output.
func (e *env) startIn(h *herdr.Client, s api.Session, paneID, which string, focus bool) (bool, error) {
	_, err := h.AgentStart(herdr.AgentStartParams{
		Name:   agentName(s.ID),
		Kind:   "claude",
		PaneID: paneID,
		Args:   e.claudeArgs(s),
	})
	if err != nil {
		return false, fmt.Errorf("start claude in pane %s: %w\nRun this in a shell instead:\n%s", clean(paneID), err, e.resumeCmd(s))
	}
	if focus {
		if err := h.PaneFocus(paneID); err != nil {
			return false, fmt.Errorf("focus pane %s: %w", clean(paneID), err)
		}
	}
	fmt.Fprintf(e.out, "Started claude %s in %s %s.\n", strings.Join(e.claudeArgs(s), " "), which, clean(paneID))
	return true, nil
}

// remoteHub is the sessionhub binary on the other machine. A command that ssh runs
// gets a non-interactive shell whose PATH lacks ~/.local/bin, so a bare "sessionhub"
// is not found. The tilde must be expanded by the remote shell, not the local
// one (the homes may differ), so the printed command single-quotes the whole
// remote string: the local shell passes it to ssh as is, and the remote shell
// sees the tilde unquoted.
const remoteHub = "~/.local/bin/sessionhub"

// remoteLoginCmd wraps cmd so the remote side runs it in the user's login
// shell, which reads the profile that puts ~/.local/bin (where claude is
// installed too) on PATH. cmd is quoted once here; the caller quotes the
// result again with localQuote, so the remote shell parses the outer layer
// and the login shell the inner one.
func remoteLoginCmd(cmd string) string {
	return "exec $SHELL -lc " + shellQuote(cmd)
}

// localQuote quotes a remote command for the local shell. It prefers double
// quotes with each $ escaped, which reads as
// "exec \$SHELL -lc 'cd /x && claude --resume <id>'", and falls back to
// shellQuote when s holds a character that double quotes don't make
// literal: " ` \ or a newline, or ! (history expansion in an interactive
// bash or zsh).
func localQuote(s string) string {
	if strings.ContainsAny(s, "\"`\\!\n") {
		return shellQuote(s)
	}
	return `"` + strings.ReplaceAll(s, "$", `\$`) + `"`
}

// remote prints the commands to reach a session on another machine.
func (e *env) remote(ctx context.Context, s api.Session) error {
	sshHost, herdrHost := e.hosts(ctx, s)
	if !validHost(sshHost) || !validHost(herdrHost) {
		return fmt.Errorf("refusing to print commands for machine %q: its SSH host %q or herdr host %q is empty or starts with a hyphen", s.Machine, sshHost, herdrHost)
	}
	fmt.Fprintf(e.out, "Session %s runs on machine %s, not on this machine. To reach it:\n\n", shortID(s.ID), strconv.Quote(s.Machine))
	if s.HerdrSession == "" && s.HerdrPane == "" {
		fmt.Fprintf(e.out, "ssh -t %s %s\n", shellQuote(sshHost), localQuote(remoteLoginCmd(e.resumeCmd(s))))
		return nil
	}
	// Comments never carry session data: a newline in one would end the comment.
	fmt.Fprintf(e.out, "ssh -t %s %s     # opens or focuses it in the machine's herdr\n", shellQuote(sshHost), shellQuote(remoteHub+" resume "+e.remoteFlag()+s.ID))
	fmt.Fprintf(e.out, "herdr --remote %s               # attach to the machine's herdr from here\n", shellQuote(herdrHost))
	if s.HerdrPane == "" {
		return nil
	}
	if label := matchSaved(e.savedMachines(ctx), herdrHost); validHost(label) {
		fmt.Fprintf(e.out, "herdr --machine %s pane focus %s     # saved herdr machine; works while the pane is live\n", shellQuote(label), shellQuote(s.HerdrPane))
	}
	return nil
}

// remoteFlag is the flag text for a printed `sessionhub resume` command, with a
// trailing space, or "". Flags go before the ID.
func (e *env) remoteFlag() string {
	if e.remoteControl {
		return "--remote-control "
	}
	return ""
}

func (e *env) savedMachines(ctx context.Context) []SavedMachine {
	if e.saved == nil {
		return nil
	}
	return e.saved(ctx)
}

// hosts returns the SSH and herdr hosts for the session's machine. When the
// server does not know the machine, it falls back to the host in the
// session's ResumeCommand ("ssh -t <ssh_host> '~/.local/bin/sessionhub resume <id>'").
func (e *env) hosts(ctx context.Context, s api.Session) (sshHost, herdrHost string) {
	if ms, err := e.api.ListMachines(ctx); err == nil {
		for _, m := range ms {
			if m.Name == s.Machine {
				return orDefault(m.SSHHost, s.Machine), orDefault(m.HerdrHost, orDefault(m.SSHHost, s.Machine))
			}
		}
	}
	if f := strings.Fields(s.ResumeCommand); len(f) >= 3 && f[0] == "ssh" && f[1] == "-t" {
		return f[2], f[2]
	}
	return s.Machine, s.Machine
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func shortID(id string) string {
	r := []rune(clean(id))
	if len(r) > 8 {
		r = r[:8]
	}
	return string(r)
}

// clean is termtext.Clean without a width: server and herdr strings pass
// through it before they reach the terminal in messages. Printed commands
// use shellQuote instead, which must keep every byte.
func clean(s string) string { return termtext.Clean(s, 0) }

// shellQuote single-quotes s unless it is made of safe characters only.
func shellQuote(s string) string {
	safe := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-:@%+=,", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// maxPick caps the picker list: ended sessions pile up.
const maxPick = 30

// pick lists this machine's sessions, reads a number, and resumes it.
func (e *env) pick(ctx context.Context) error {
	if e.cfg.Machine == "" {
		return errors.New("no machine name in the client config; set machine in config.toml or SESSIONHUB_MACHINE")
	}
	list, err := e.api.ListSessions(ctx, false, e.cfg.Machine)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintf(e.out, "No sessions on %s.\n", e.cfg.Machine)
		return nil
	}
	sort.SliceStable(list, func(i, j int) bool {
		li, lj := list[i].Status == api.StatusLive || list[i].Status == api.StatusBlocked, list[j].Status == api.StatusLive || list[j].Status == api.StatusBlocked
		if li != lj {
			return li
		}
		return list[i].LastSeenAt.After(list[j].LastSeenAt)
	})
	if len(list) > maxPick {
		list = list[:maxPick]
	}
	for i, s := range list {
		t := s.Title
		if t == "" {
			t = s.CWD
		}
		fmt.Fprintf(e.out, "%2d  %-8s %-7s %s\n", i+1, shortID(s.ID), clean(s.Status), termtext.Clean(t, termtext.TitleWidth))
	}
	fmt.Fprint(e.out, "Resume which session? (number, empty to cancel) ")
	in := bufio.NewReader(e.in)
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return errors.New("no selection read")
	}
	line = strings.TrimSpace(line)
	if line == "" || line == "q" {
		fmt.Fprintln(e.out, "Cancelled.")
		return nil
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(list) {
		return fmt.Errorf("%q is not a number from 1 to %d", line, len(list))
	}
	acted, err := e.resumeSession(ctx, list[n-1])
	if !acted && (err == nil || errors.Is(err, errNeedsForce) || errors.Is(err, errMoving)) {
		// The overlay closes when we exit; keep the printed commands readable.
		fmt.Fprint(e.out, "\nPress Enter to close. ")
		in.ReadString('\n')
	}
	return err
}
