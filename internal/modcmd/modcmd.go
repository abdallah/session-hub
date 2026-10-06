// Package modcmd implements `sessionhub mod`, the calls the sessionhub Claude Code mod
// makes. The mod runs `sessionhub mod <sub>` with $.process.run and holds no token:
// this package loads the client config, calls the server, and queues what
// must not be lost. See docs/cli.md, "sessionhub mod".
//
// The argv and stdin each subcommand takes are the mod's contract
// (internal/claudemod/mod/hooks/register.js). Keep them backward compatible:
// unknown JSON fields are ignored; unknown flags are an error.
package modcmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/paths"
	"github.com/abdallah/session-hub/internal/store"
)

const (
	// callTimeout is the limit for each call but the poll. The mod gives
	// every `sessionhub mod` run 10 s.
	callTimeout = 5 * time.Second
	// pollWait is how long the server holds a poll open, and pollDeadline
	// the most a poll may take in all. The mod kills `sessionhub mod poll` after
	// 35 s.
	pollWait     = 25 * time.Second
	pollDeadline = 30 * time.Second
	// maxStdin is the most stdin a subcommand reads.
	maxStdin = 1 << 20
	// usageStateMaxAge is how long a session's usage state file is kept
	// after its last report.
	usageStateMaxAge = 7 * 24 * time.Hour
)

const usage = `usage: sessionhub mod <command> [args]

The sessionhub Claude Code mod runs these; they are not meant to be run by hand.

  blocked-on --session ID   read {"questions": [...]} or {"text": "..."} on stdin and report it
  usage --session ID        read {"context": {...}, "cost": {...}} on stdin and report it
  poll --session ID         wait up to 25 s for a message; print it as JSON, or nothing
  result --message ID --state delivered|busy|refused [--detail TEXT]
  inbox-count               print {"blocked": n, "waiting": n, "finished": n}
`

// env is what the subcommands need; tests build their own.
type env struct {
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	newClient func() (*client.Client, error)
	queue     *client.Queue
	stateDir  string
	now       func() time.Time
	// pollWait and pollDeadline are the constants, shorter in tests.
	pollWait     time.Duration
	pollDeadline time.Duration
}

func defaultEnv() *env {
	return &env{
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		newClient: func() (*client.Client, error) {
			cfg, err := client.LoadConfig()
			if err != nil {
				return nil, err
			}
			c, err := client.New(cfg)
			if err != nil {
				return nil, err
			}
			c.SetTimeout(callTimeout)
			return c, nil
		},
		queue:        client.DefaultQueue(),
		stateDir:     paths.StateDir(),
		now:          time.Now,
		pollWait:     pollWait,
		pollDeadline: pollDeadline,
	}
}

// Run implements `sessionhub mod <command>`. Every failure is an error, so the
// command exits non-zero and the mod backs off.
func Run(ctx context.Context, args []string) error {
	return defaultEnv().run(ctx, args)
}

func (e *env) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "blocked-on":
		return e.blockedOn(ctx, rest)
	case "usage":
		return e.usage(ctx, rest)
	case "poll":
		return e.poll(ctx, rest)
	case "result":
		return e.result(ctx, rest)
	case "inbox-count":
		return e.inboxCount(ctx, rest)
	case "help", "-h", "--help":
		fmt.Fprint(e.stdout, usage)
		return nil
	}
	return fmt.Errorf("sessionhub mod: unknown command %q\n\n%s", sub, usage)
}

// parse parses a subcommand's flags and refuses positional arguments.
func parse(name string, f *flag.FlagSet, args []string) error {
	f.SetOutput(io.Discard)
	if err := f.Parse(args); err != nil {
		return fmt.Errorf("sessionhub mod %s: %w", name, err)
	}
	if f.NArg() > 0 {
		return fmt.Errorf("sessionhub mod %s: unexpected argument %q", name, f.Arg(0))
	}
	return nil
}

// sessionFlag parses `--session ID`, which name requires.
func sessionFlag(name string, args []string) (string, error) {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	id := f.String("session", "", "session ID")
	if err := parse(name, f, args); err != nil {
		return "", err
	}
	if !store.ValidSessionID(*id) {
		return "", fmt.Errorf("sessionhub mod %s: --session: invalid session ID %q", name, *id)
	}
	return *id, nil
}

// readJSON decodes stdin into v. Unknown fields are ignored.
func (e *env) readJSON(name string, v any) error {
	b, err := io.ReadAll(io.LimitReader(e.stdin, maxStdin+1))
	if err != nil {
		return fmt.Errorf("sessionhub mod %s: read stdin: %w", name, err)
	}
	if len(b) > maxStdin {
		return fmt.Errorf("sessionhub mod %s: stdin is over %d bytes", name, maxStdin)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("sessionhub mod %s: stdin: %w", name, err)
	}
	return nil
}

// blockedOnIn is what the mod sends: the AskUserQuestion input, or a text.
// With Clear, it names the question to clear: the same input as the set, so
// it formats to the same line.
type blockedOnIn struct {
	Questions []question `json:"questions"`
	Text      *string    `json:"text"`
	Clear     bool       `json:"clear"`
}

type question struct {
	Question string `json:"question"`
	Header   string `json:"header"`
	Options  []struct {
		Label string `json:"label"`
	} `json:"options"`
}

// blockedOn implements `sessionhub mod blocked-on --session ID`. A question is
// posted once, best effort: a question replayed later would block a session
// that was already answered. A clear names the question it clears, so the
// server ignores it once a newer question stands; a clear that cannot reach
// the server is queued. `{"text": ""}` (an older mod) clears only a block
// the mod set.
func (e *env) blockedOn(ctx context.Context, args []string) error {
	id, err := sessionFlag("blocked-on", args)
	if err != nil {
		return err
	}
	var in blockedOnIn
	if err := e.readJSON("blocked-on", &in); err != nil {
		return err
	}
	var text string
	switch {
	case in.Questions != nil:
		text = formatQuestions(in.Questions)
	case in.Text != nil:
		text = *in.Text
	case !in.Clear:
		return errors.New(`sessionhub mod blocked-on: stdin has neither "questions" nor "text"`)
	}
	text = termtext.Clean(text, api.MaxBlockedOnRunes)
	c, cerr := e.newClient()
	if !in.Clear && text != "" {
		if cerr != nil {
			return cerr
		}
		return c.SetBlockedOn(ctx, id, text)
	}
	// A clear: of question text, or with no text, of the mod's own block.
	sendClear := func() error {
		if text == "" {
			return c.SetBlockedOn(ctx, id, "")
		}
		return c.ClearBlockedOn(ctx, id, text)
	}
	if cerr == nil {
		err = sendClear()
		if err == nil || (!client.IsRetryable(err) && !client.IsAuthError(err)) {
			return err
		}
	} else {
		err = cerr
	}
	it := client.Item{Op: client.OpBlockedOnClear, SessionID: id}
	if text != "" {
		body, merr := json.Marshal(api.BlockedOnIn{Clears: text})
		if merr != nil {
			return merr
		}
		it.Body = body
	}
	if qerr := e.queue.Append(it); qerr != nil {
		return fmt.Errorf("sessionhub mod blocked-on: %v; queue: %w", err, qerr)
	}
	fmt.Fprintf(e.stderr, "sessionhub mod blocked-on: queued the clear: %v\n", err)
	return nil
}

// formatQuestions makes one line of the AskUserQuestion questions:
// `Question: Which library should we use? (date-fns, luxon, dayjs)`, with
// more questions joined by " · ".
func formatQuestions(qs []question) string {
	var parts []string
	for _, q := range qs {
		s := strings.TrimSpace(q.Question)
		if s == "" {
			s = strings.TrimSpace(q.Header)
		}
		var labels []string
		for _, o := range q.Options {
			if l := strings.TrimSpace(o.Label); l != "" {
				labels = append(labels, l)
			}
		}
		if len(labels) > 0 {
			s = strings.TrimSpace(s + " (" + strings.Join(labels, ", ") + ")")
		}
		if s != "" {
			parts = append(parts, s)
		}
	}
	switch len(parts) {
	case 0:
		return "Question"
	case 1:
		return "Question: " + parts[0]
	}
	return "Questions: " + strings.Join(parts, " · ")
}

// usageIn is the session.measure event's context and cost.
type usageIn struct {
	Context *struct {
		Tokens  *float64 `json:"tokens"`
		Window  float64  `json:"window"`
		Percent *float64 `json:"percent"`
	} `json:"context"`
	Cost *struct {
		USD *float64 `json:"usd"`
	} `json:"cost"`
}

// usageState is the last usage sent for a session.
type usageState struct {
	ContextPercent *int     `json:"context_percent,omitempty"`
	CostUSD        *float64 `json:"cost_usd,omitempty"`
}

// usage implements `sessionhub mod usage --session ID`. It reports only when the
// context fill moved by at least 1 point or the cost by at least $0.01
// since the last report it sent for the session.
func (e *env) usage(ctx context.Context, args []string) error {
	id, err := sessionFlag("usage", args)
	if err != nil {
		return err
	}
	var in usageIn
	if err := e.readJSON("usage", &in); err != nil {
		return err
	}
	var out api.UsageIn
	if c := in.Context; c != nil {
		pct := math.NaN()
		switch {
		case c.Percent != nil:
			pct = *c.Percent
		case c.Tokens != nil && c.Window > 0:
			pct = *c.Tokens * 100 / c.Window
		}
		if !math.IsNaN(pct) && !math.IsInf(pct, 0) {
			p := int(math.Round(min(max(pct, 0), 100)))
			out.ContextPercent = &p
		}
	}
	if c := in.Cost; c != nil && c.USD != nil && *c.USD >= 0 && *c.USD <= api.MaxLiveCostUSD {
		v := *c.USD
		out.CostUSD = &v
	}
	if out.ContextPercent == nil && out.CostUSD == nil {
		return nil // nothing sessionhub stores
	}
	statePath := filepath.Join(e.stateDir, "mod-usage", id+".json")
	last := readUsageState(statePath)
	if !usageChanged(last, out) {
		return nil
	}
	c, err := e.newClient()
	if err != nil {
		return err
	}
	if err := c.PutUsage(ctx, id, out); err != nil {
		return err
	}
	if out.ContextPercent != nil {
		last.ContextPercent = out.ContextPercent
	}
	if out.CostUSD != nil {
		last.CostUSD = out.CostUSD
	}
	if err := writeUsageState(statePath, last); err != nil {
		fmt.Fprintf(e.stderr, "sessionhub mod usage: save state: %v\n", err)
	}
	pruneUsageState(filepath.Dir(statePath), e.now())
	return nil
}

func usageChanged(last usageState, in api.UsageIn) bool {
	if p := in.ContextPercent; p != nil && (last.ContextPercent == nil || *p != *last.ContextPercent) {
		return true
	}
	if c := in.CostUSD; c != nil && (last.CostUSD == nil || math.Abs(*c-*last.CostUSD) >= 0.01-1e-9) {
		return true
	}
	return false
}

// readUsageState reads a session's state file; a missing or broken one is
// empty, so the next report goes out.
func readUsageState(path string) usageState {
	var s usageState
	if b, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(b, &s) != nil {
			return usageState{}
		}
	}
	return s
}

func writeUsageState(path string, s usageState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".usage-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// pruneUsageState removes state files of sessions that have not reported
// for usageStateMaxAge.
func pruneUsageState(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, d := range entries {
		if d.IsDir() {
			continue
		}
		if info, err := d.Info(); err == nil && now.Sub(info.ModTime()) > usageStateMaxAge {
			os.Remove(filepath.Join(dir, d.Name()))
		}
	}
}

// poll implements `sessionhub mod poll --session ID`: one long poll. It prints the
// claimed message as {"id", "text"} on one line, or nothing when none came.
// A transport or server error is an error, so the mod backs off.
func (e *env) poll(ctx context.Context, args []string) error {
	id, err := sessionFlag("poll", args)
	if err != nil {
		return err
	}
	c, err := e.newClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, e.pollDeadline)
	defer cancel()
	m, err := c.PollSessionMessage(ctx, id, e.pollWait)
	if err != nil {
		return err
	}
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(e.stdout, "%s\n", b)
	return err
}

// result implements `sessionhub mod result --message ID --state S [--detail D]`.
func (e *env) result(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("result", flag.ContinueOnError)
	id := f.String("message", "", "message ID")
	state := f.String("state", "", "delivered, busy, or refused")
	detail := f.String("detail", "", "what happened, for busy or refused")
	if err := parse("result", f, args); err != nil {
		return err
	}
	if !store.ValidMessageID(*id) {
		return fmt.Errorf("sessionhub mod result: --message: invalid message ID %q", *id)
	}
	switch *state {
	case api.MessageDelivered, api.MessageBusy, api.MessageRefused:
	default:
		return fmt.Errorf("sessionhub mod result: --state %q: want delivered, busy, or refused", *state)
	}
	c, err := e.newClient()
	if err != nil {
		return err
	}
	_, err = c.PostMessageResult(ctx, *id, api.MessageResultIn{State: *state, Detail: termtext.Clean(*detail, api.MaxControlDetailRunes)})
	return err
}

// inboxCount implements `sessionhub mod inbox-count`.
func (e *env) inboxCount(ctx context.Context, args []string) error {
	if err := parse("inbox-count", flag.NewFlagSet("inbox-count", flag.ContinueOnError), args); err != nil {
		return err
	}
	c, err := e.newClient()
	if err != nil {
		return err
	}
	in, err := c.Inbox(ctx)
	if err != nil {
		return err
	}
	b, err := json.Marshal(in.Counts)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(e.stdout, "%s\n", b)
	return err
}
