// Package cli implements the sessionhub client commands ls, show, status, login, and inbox.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/client"
)

// hubAPI is the part of *client.Client the commands use.
type hubAPI interface {
	ListSessions(ctx context.Context, live bool, machine string) ([]api.Session, error)
	GetSession(ctx context.Context, idOrPrefix string) (api.SessionDetail, error)
	Health(ctx context.Context) error
}

// env is everything a command needs; tests build their own.
type env struct {
	cfg   client.Config
	api   hubAPI
	login loginAPI
	inbox inboxAPI
	tasks taskAPI
	notes noteAPI
	queue noteQueue
	// getenv reads the environment; sessionhub note takes the session from it.
	getenv  func(string) string
	actions actionsAPI
	moves   moveAPI
	starts  startAPI
	// getwd is the current directory, the default for sessionhub start on
	// this machine.
	getwd func() (string, error)
	// pause waits between delivery reads in sessionhub send; tests advance a fake
	// clock instead.
	pause func(time.Duration)
	width func() int // terminal width in columns
	out   io.Writer
	now   func() time.Time
	// isTerminal reports whether stdin and stdout are a terminal; nil means
	// no (sessionhub inbox --watch refuses to start).
	isTerminal func() bool
	// in is standard input, where sessionhub approve and sessionhub deny read the answer to
	// their question; stdinTerminal reports whether it is a terminal (nil
	// means no: they refuse to ask without --yes).
	in            io.Reader
	stdinTerminal func() bool
	// jump takes sessionhub inbox --watch to a session; nil prints the resume
	// command in the status line instead.
	jump jumper
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
	return &env{cfg: cfg, api: c, login: c, inbox: c, tasks: c, notes: c, queue: client.DefaultQueue(), getenv: os.Getenv, actions: c, moves: c, starts: c, getwd: os.Getwd, pause: time.Sleep, width: termWidth, out: os.Stdout, now: time.Now,
		isTerminal:    func() bool { return isTerminal(os.Stdin.Fd()) && isTerminal(os.Stdout.Fd()) },
		in:            os.Stdin,
		stdinTerminal: func() bool { return isTerminal(os.Stdin.Fd()) }}, nil
}

// RunLs implements `sessionhub ls [--all] [--machine M]`.
func RunLs(ctx context.Context, args []string) error {
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	return e.ls(ctx, args)
}

// RunShow implements `sessionhub show <id-prefix>`.
func RunShow(ctx context.Context, args []string) error {
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	return e.show(ctx, args)
}

// RunStatus implements `sessionhub status`.
func RunStatus(ctx context.Context, args []string) error {
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	return e.status(ctx, args)
}

func (e *env) ls(ctx context.Context, args []string) error {
	const usage = "usage: sessionhub ls [--all] [--machine M] [--grep TEXT]"
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	all := fs.Bool("all", false, "include stale and ended sessions")
	machine := fs.String("machine", "", "only this machine")
	grep := fs.String("grep", "", "only sessions whose title, recap, last prompt, branch, directory, or machine contains TEXT")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%s: %w", usage, err)
	}
	if fs.NArg() > 0 {
		return errors.New(usage)
	}
	list, err := e.api.ListSessions(ctx, !*all, *machine)
	if err != nil {
		return err
	}
	if !*all {
		list = filterStatus(list, api.StatusLive, api.StatusBlocked)
	}
	if *grep != "" {
		kept := list[:0]
		for _, s := range list {
			if matches(s, *grep) {
				kept = append(kept, s)
			}
		}
		list = kept
		if len(list) == 0 && !*all {
			// The live list has no match: say so when an ended or stale
			// session does, so the search doesn't look empty.
			every, err := e.api.ListSessions(ctx, false, *machine)
			if err != nil {
				return err
			}
			n := 0
			for _, s := range every {
				if matches(s, *grep) {
					n++
				}
			}
			if n > 0 {
				fmt.Fprintf(e.out, "no live sessions match; %d ended match: use --all\n", n)
				return nil
			}
		}
	}
	if len(list) == 0 {
		fmt.Fprintln(e.out, "no sessions")
		return nil
	}
	renderTable(e.out, list, e.now())
	return nil
}

func (e *env) show(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: sessionhub show <id-prefix>")
	}
	d, err := e.api.GetSession(ctx, args[0])
	if err != nil {
		return err
	}
	renderDetail(e.out, d, e.now())
	return nil
}

func (e *env) status(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: sessionhub status")
	}
	fmt.Fprintf(e.out, "server:  %s\n", e.cfg.ServerURL)
	fmt.Fprintf(e.out, "machine: %s\n", orDash(e.cfg.Machine))
	if err := e.api.Health(ctx); err != nil {
		fmt.Fprintf(e.out, "health:  unreachable (%v)\n", err)
		return errors.New("sessionhub server is not reachable")
	}
	fmt.Fprintln(e.out, "health:  ok")
	if e.cfg.Machine == "" {
		return errors.New("no machine name in the client config; set machine in config.toml or SESSIONHUB_MACHINE")
	}
	list, err := e.api.ListSessions(ctx, true, e.cfg.Machine)
	if err != nil {
		return err
	}
	fmt.Fprintln(e.out)
	if len(list) == 0 {
		fmt.Fprintf(e.out, "no sessions on %s\n", e.cfg.Machine)
		return nil
	}
	renderTable(e.out, list, e.now())
	return nil
}

func filterStatus(in []api.Session, keep ...string) []api.Session {
	var out []api.Session
	for _, s := range in {
		for _, k := range keep {
			if s.Status == k {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shortID returns the first 8 characters of a session ID, cleaned for the
// terminal (the server does not validate IDs).
func shortID(id string) string {
	r := []rune(termtext.Clean(id, 0))
	if len(r) > 8 {
		r = r[:8]
	}
	return string(r)
}

// clean is termtext.Clean: every server string goes through it before it
// reaches the terminal.
func clean(s string, max int) string { return termtext.Clean(s, max) }

// age formats the time since t as a short duration ("45s", "12m", "3h", "2d").
func age(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// reportSummary is one line for the latest report: what is in flight, else
// what it waits on, else the last thing done, else the note.
func reportSummary(r *api.Report) string {
	if r == nil {
		return "-"
	}
	switch {
	case len(r.InFlight) > 0:
		return "doing: " + r.InFlight[0]
	case len(r.WaitingOn) > 0:
		return "waiting: " + r.WaitingOn[0]
	case len(r.Done) > 0:
		return "done: " + r.Done[len(r.Done)-1]
	case r.Note != "":
		return r.Note
	}
	return "-"
}

// sessionSummary is the REPORT column of sessionhub ls: what a blocked session
// waits on, as its mod reported it, else reportSummary.
func sessionSummary(s api.Session) string {
	if s.BlockedOn != "" {
		return "blocked on: " + s.BlockedOn
	}
	return reportSummary(s.LatestReport)
}

// usageLine is the context window fill and cost a session's mod last
// reported, such as "context 42% · $1.25 (2m ago)", or "".
func usageLine(s api.Session, now time.Time) string {
	var parts []string
	if s.ContextPercent != nil {
		parts = append(parts, fmt.Sprintf("context %d%%", *s.ContextPercent))
	}
	if s.LiveCostUSD != nil {
		parts = append(parts, fmt.Sprintf("$%.2f", *s.LiveCostUSD))
	}
	if len(parts) == 0 {
		return ""
	}
	line := strings.Join(parts, " · ")
	if s.UsageAt != nil {
		line += fmt.Sprintf(" (%s ago)", age(now, *s.UsageAt))
	}
	return line
}

func title(s api.Session) string {
	if s.Title != "" {
		return s.Title
	}
	if s.CWD != "" {
		return s.CWD
	}
	return "(untitled)"
}

func renderTable(w io.Writer, list []api.Session, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MACHINE\tID\tTITLE\tSTATUS\tAGE\tREPORT")
	for _, s := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", clean(s.Machine, 0), shortID(s.ID),
			clean(title(s), termtext.TitleWidth), clean(s.Status, 0), age(now, s.LastSeenAt), clean(sessionSummary(s), 60))
	}
	tw.Flush()
}

func renderDetail(w io.Writer, d api.SessionDetail, now time.Time) {
	field := func(k, v string) {
		if v = clean(v, 0); v != "" {
			fmt.Fprintf(w, "%-10s %s\n", k+":", v)
		}
	}
	field("id", d.ID)
	field("title", d.Title)
	field("machine", d.Machine)
	field("status", d.Status)
	field("agent", d.Agent)
	field("state", d.AgentState)
	field("blocked on", d.BlockedOn)
	field("usage", usageLine(d.Session, now))
	field("cwd", d.CWD)
	field("repo", d.GitRepo)
	field("branch", d.GitBranch)
	if d.HerdrPane != "" || d.HerdrSession != "" {
		var parts []string
		for _, kv := range [][2]string{{"session", d.HerdrSession}, {"workspace", d.HerdrWorkspace}, {"pane", d.HerdrPane}} {
			if kv[1] != "" {
				parts = append(parts, kv[0]+"="+kv[1])
			}
		}
		field("herdr", strings.Join(parts, " "))
	}
	field("started", fmt.Sprintf("%s (%s ago)", d.StartedAt.Format(time.RFC3339), age(now, d.StartedAt)))
	field("last seen", fmt.Sprintf("%s (%s ago)", d.LastSeenAt.Format(time.RFC3339), age(now, d.LastSeenAt)))
	if d.EndedAt != nil {
		field("ended", d.EndedAt.Format(time.RFC3339))
	}
	field("resume", d.ResumeCommand)
	if d.Recap != "" {
		at := ""
		if d.RecapAt != nil {
			at = fmt.Sprintf(" (%s ago)", age(now, *d.RecapAt))
		}
		field("recap", d.Recap+at)
	}
	if d.LastPrompt != "" && d.LastPromptAt != nil {
		field("last prompt", fmt.Sprintf("%s (%s ago)", d.LastPrompt, age(now, *d.LastPromptAt)))
	}
	if line := summaryLine(d.Summary); line != "" {
		field("summary", line)
	}
	if dg := d.Digest; dg != nil {
		renderDigest(w, d.GitBranch, dg, now)
	}

	fmt.Fprintf(w, "\nreports (%d, newest first)\n", len(d.Reports))
	for _, r := range d.Reports {
		fmt.Fprintf(w, "  %s\n", r.TS.Format(time.RFC3339))
		list := func(name string, items []string) {
			for _, it := range items {
				fmt.Fprintf(w, "    %-11s %s\n", name, clean(it, 0))
			}
		}
		list("done", r.Done)
		list("in flight", r.InFlight)
		list("waiting on", r.WaitingOn)
		if r.Note != "" {
			fmt.Fprintf(w, "    %-11s %s\n", "note", clean(r.Note, 0))
		}
	}
	fmt.Fprintf(w, "\nevents (%d, newest first)\n", len(d.Events))
	for _, ev := range d.Events {
		line := fmt.Sprintf("  %s  %-8s %s", ev.TS.Format(time.RFC3339), clean(ev.Source, 0), clean(ev.Kind, 0))
		if len(ev.Payload) > 0 && string(ev.Payload) != "null" {
			line += "  " + clean(string(ev.Payload), 100)
		}
		fmt.Fprintln(w, line)
	}
}

// matches reports whether q occurs, ignoring case, in the session's title,
// recap, last prompt, branch, directory, or machine. q is plain text.
func matches(s api.Session, q string) bool {
	if q == "" {
		return true
	}
	q = strings.ToLower(q)
	for _, f := range []string{s.Title, s.Recap, s.LastPrompt, s.GitBranch, s.CWD, s.Machine} {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// linkLabel is "!391" for a GitLab merge request, "#7" otherwise, or
// "link" without a number.
func linkLabel(l api.DigestLink) string {
	switch {
	case l.Number == "":
		return "link"
	case strings.Contains(l.URL, "/merge_requests/"):
		return "!" + l.Number
	}
	return "#" + l.Number
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// humanCount is 950, 371k, or 1.5M.
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1000:
		return strconv.FormatInt(n/1000, 10) + "k"
	}
	return strconv.FormatInt(n, 10)
}

// summaryLine is the card's one-line summary: git counts, the latest link,
// and the cost, or output tokens when no cost is known. Parts that are zero
// or unknown are left out.
func summaryLine(sum *api.SessionSummary) string {
	if sum == nil {
		return ""
	}
	var parts []string
	if g := sum.Git; g != nil {
		if g.CommitCount > 0 {
			parts = append(parts, plural(g.CommitCount, "commit"))
		}
		if g.Uncommitted > 0 {
			parts = append(parts, fmt.Sprintf("%d uncommitted", g.Uncommitted))
		}
		if g.Unpushed > 0 {
			parts = append(parts, fmt.Sprintf("%d unpushed", g.Unpushed))
		}
	}
	if sum.LatestLink != nil {
		parts = append(parts, linkLabel(*sum.LatestLink))
	}
	switch {
	case sum.CostUSD != nil:
		parts = append(parts, fmt.Sprintf("$%.2f", *sum.CostUSD))
	case sum.OutputTokens > 0:
		parts = append(parts, humanCount(sum.OutputTokens)+" tokens out")
	}
	return strings.Join(parts, " · ")
}

func renderDigest(w io.Writer, branch string, dg *api.Digest, now time.Time) {
	if g := dg.Git; g != nil && g.CommitCount > 0 {
		on := "HEAD"
		if branch != "" {
			on = branch
		}
		fmt.Fprintf(w, "\ncommits on %s during the session (%d)\n", clean(on, 0), g.CommitCount)
		for _, c := range g.Commits {
			fmt.Fprintf(w, "  %s  %s\n", clean(c.SHA, 0), clean(c.Subject, 0))
		}
	}
	if len(dg.Links) > 0 {
		fmt.Fprintf(w, "\nlinks (%d)\n", len(dg.Links))
		for _, l := range dg.Links {
			fmt.Fprintf(w, "  %-6s %s\n", linkLabel(l), clean(l.URL, 0))
		}
	}
	t := dg.Tokens
	fmt.Fprintf(w, "\ntokens:    input %s · output %s · cache read %s · cache write %s\n",
		humanCount(t.Input), humanCount(t.Output), humanCount(t.CacheRead), humanCount(t.CacheWrite))
	if dg.CostUSD != nil {
		as := ""
		if dg.CostAt != nil {
			as = " as of " + dg.CostAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "cost:      $%.2f%s\n", *dg.CostUSD, as)
	}
	fmt.Fprintf(w, "digest as of %s (%s ago)\n", dg.AsOf.Format(time.RFC3339), age(now, dg.AsOf))
}
