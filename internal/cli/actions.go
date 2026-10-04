package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

// actionsAPI is the part of *client.Client that sessionhub rules, sessionhub send, sessionhub
// approve, and sessionhub deny use.
type actionsAPI interface {
	Instructions(ctx context.Context) (api.InstructionList, error)
	AddInstruction(ctx context.Context, text string) (api.Instruction, error)
	DeleteInstruction(ctx context.Context, id int64) error
	SendMessages(ctx context.Context, in api.MessagesIn) (api.MessagesOut, error)
	GetMessage(ctx context.Context, id string) (api.Message, error)
	DecidePermission(ctx context.Context, id string, in api.DecisionIn) (api.PermissionRequest, error)
}

var _ actionsAPI = (*client.Client)(nil)

const (
	// sendWait is how long sessionhub send waits for delivery results.
	sendWait = 15 * time.Second
	// sendPoll is how often it reads them.
	sendPoll = time.Second
)

const rulesUsage = `usage:
  sessionhub rules [ls]
  sessionhub rules add "<text>"
  sessionhub rules rm <id>`

const sendUsage = `usage:
  sessionhub send <id-or-prefix>... -m "<text>"
  sessionhub send --machine <machine> -m "<text>"`

const approveUsage = `usage: sessionhub approve [--yes] <request-id|session-prefix>`

const denyUsage = `usage: sessionhub deny [--yes] <request-id|session-prefix> ["reason"]`

// errText cleans an error that may carry a server-supplied message and caps
// its length.
func errText(err error) string { return clean(err.Error(), 300) }

func runWith(ctx context.Context, args []string, fn func(*env, context.Context, []string) error) error {
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	return fn(e, ctx, args)
}

// RunRules implements `sessionhub rules`.
func RunRules(ctx context.Context, args []string) error { return runWith(ctx, args, (*env).rules) }

// RunSend implements `sessionhub send`.
func RunSend(ctx context.Context, args []string) error { return runWith(ctx, args, (*env).send) }

// RunApprove implements `sessionhub approve`.
func RunApprove(ctx context.Context, args []string) error { return runWith(ctx, args, (*env).approve) }

// RunDeny implements `sessionhub deny`.
func RunDeny(ctx context.Context, args []string) error { return runWith(ctx, args, (*env).deny) }

func (e *env) rules(ctx context.Context, args []string) error {
	switch {
	case len(args) == 0 || (len(args) == 1 && args[0] == "ls"):
		list, err := e.actions.Instructions(ctx)
		if err != nil {
			return fmt.Errorf("rules: %s", errText(err))
		}
		if len(list.Instructions) == 0 {
			fmt.Fprintln(e.out, "no standing rules")
			return nil
		}
		for _, r := range list.Instructions {
			fmt.Fprintf(e.out, "%d  %s  (%s)\n", r.ID, clean(r.Text, 0), clean(r.CreatedBy, 0))
		}
		return nil
	case len(args) == 2 && args[0] == "add":
		r, err := e.actions.AddInstruction(ctx, args[1])
		if err != nil {
			return fmt.Errorf("rules add: %s", errText(err))
		}
		fmt.Fprintf(e.out, "added rule %d\n", r.ID)
		return nil
	case len(args) == 2 && args[0] == "rm":
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("rules rm: %q is not a rule number\n%s", clean(args[1], 0), rulesUsage)
		}
		if err := e.actions.DeleteInstruction(ctx, id); err != nil {
			return fmt.Errorf("rules rm: %s", errText(err))
		}
		fmt.Fprintf(e.out, "removed rule %d\n", id)
		return nil
	}
	return fmt.Errorf("rules: unexpected arguments\n%s", rulesUsage)
}

// sendArgs splits sessionhub send's arguments. Flags may come before or after the
// targets, which the flag package does not allow.
func sendArgs(args []string) (targets []string, machine, text string, err error) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-m", "--message", "--machine":
			if i+1 >= len(args) {
				return nil, "", "", fmt.Errorf("send: %s needs a value\n%s", a, sendUsage)
			}
			i++
			if a == "--machine" {
				machine = args[i]
			} else {
				text = args[i]
			}
		default:
			targets = append(targets, a)
		}
	}
	switch {
	case strings.TrimSpace(text) == "":
		return nil, "", "", fmt.Errorf("send: -m needs the message text\n%s", sendUsage)
	case len(targets) == 0 && machine == "":
		return nil, "", "", fmt.Errorf("send: name the sessions or --machine\n%s", sendUsage)
	case len(targets) > 0 && machine != "":
		return nil, "", "", fmt.Errorf("send: use session IDs or --machine, not both\n%s", sendUsage)
	}
	return targets, machine, text, nil
}

func (e *env) send(ctx context.Context, args []string) error {
	targets, machine, text, err := sendArgs(args)
	if err != nil {
		return err
	}
	byID := map[string]api.Session{}
	var ids []string
	if machine != "" {
		list, err := e.api.ListSessions(ctx, true, machine)
		if err != nil {
			return fmt.Errorf("send: %s", errText(err))
		}
		for _, s := range list {
			if s.Controllable && s.Status != api.StatusEnded {
				byID[s.ID] = s
				ids = append(ids, s.ID)
			}
		}
		if len(ids) == 0 {
			return fmt.Errorf("send: no live session in herdr on %s", clean(machine, 0))
		}
	}
	for _, t := range targets {
		d, err := e.api.GetSession(ctx, t)
		if err != nil {
			return fmt.Errorf("send: %s: %s", clean(t, 0), errText(err))
		}
		if _, dup := byID[d.ID]; !dup {
			byID[d.ID] = d.Session
			ids = append(ids, d.ID)
		}
	}
	out, err := e.actions.SendMessages(ctx, api.MessagesIn{SessionIDs: ids, Text: text})
	if err != nil {
		return fmt.Errorf("send: %s", errText(err))
	}
	refused := false
	var queued []api.MessageResult
	for _, r := range out.Results {
		line := shortID(r.SessionID) + "  " + clean(title(byID[r.SessionID]), 0) + "  "
		if r.State == api.MessageQueued {
			line += "queued"
			queued = append(queued, r)
		} else {
			line += "refused: " + clean(r.Detail, 0)
			refused = true
		}
		fmt.Fprintln(e.out, line)
	}
	if e.waitDelivery(ctx, queued) {
		refused = true
	}
	if refused {
		return &ExitError{Code: 1}
	}
	return nil
}

// waitDelivery reads each queued message until it leaves queued or sendWait
// passes, then prints one line per message. It reports whether any ended
// refused or expired.
func (e *env) waitDelivery(ctx context.Context, queued []api.MessageResult) bool {
	if len(queued) == 0 {
		return false
	}
	final := map[string]api.Message{}
	deadline := e.now().Add(sendWait)
	for {
		for _, r := range queued {
			if m, ok := final[r.ID]; ok && m.State != api.MessageQueued {
				continue
			}
			m, err := e.actions.GetMessage(ctx, r.ID)
			if err != nil {
				m = api.Message{State: api.MessageQueued, Detail: "could not read the delivery state"}
			}
			final[r.ID] = m
		}
		done := true
		for _, m := range final {
			if m.State == api.MessageQueued {
				done = false
			}
		}
		if done || !e.now().Before(deadline) || ctx.Err() != nil {
			break
		}
		e.pause(sendPoll)
	}
	bad := false
	for _, r := range queued {
		m := final[r.ID]
		line := shortID(r.SessionID) + "  "
		switch m.State {
		case api.MessageDelivered:
			line += "delivered"
		case api.MessageQueued:
			why := clean(m.Detail, 0)
			if why == "" {
				why = "waiting for the agent to be idle"
			}
			line += "not delivered yet: " + why + "; sessionhub keeps trying for 10 minutes"
		default:
			line += clean(m.State, 0)
			if d := clean(m.Detail, 0); d != "" {
				line += ": " + d
			}
			bad = true
		}
		fmt.Fprintln(e.out, line)
	}
	return bad
}

// yesFlag removes --yes (or -y) from args, wherever it is, and reports
// whether it was there.
func yesFlag(args []string) ([]string, bool) {
	var rest []string
	yes := false
	for _, a := range args {
		if a == "--yes" || a == "-y" {
			yes = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, yes
}

func (e *env) approve(ctx context.Context, args []string) error {
	args, yes := yesFlag(args)
	if len(args) != 1 {
		return fmt.Errorf("approve: unexpected arguments\n%s", approveUsage)
	}
	return e.decide(ctx, args[0], api.DecisionIn{Decision: api.DecisionAllow}, yes)
}

func (e *env) deny(ctx context.Context, args []string) error {
	args, yes := yesFlag(args)
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("deny: unexpected arguments\n%s", denyUsage)
	}
	in := api.DecisionIn{Decision: api.DecisionDeny}
	if len(args) == 2 {
		in.Reason = args[1]
	}
	return e.decide(ctx, args[0], in, yes)
}

// approverInput is the whole stored tool input as sessionhub approve prints it: a
// Bash command as itself, any other input as its compact JSON. The store
// keeps at most api.MaxToolInputBytes of it; a cut input (a JSON string,
// with Truncated set) prints as that string. See approverLines.
func approverInput(p *api.PermissionRequest) string {
	var s string
	if p.ToolName == "Bash" || json.Unmarshal(p.ToolInput, &s) == nil {
		return approverLines(api.PermissionInputText(p.ToolInput))
	}
	var buf bytes.Buffer
	if json.Compact(&buf, p.ToolInput) == nil {
		return approverLines(buf.String())
	}
	return approverLines(string(p.ToolInput))
}

// approverLines prints s line by line, each indented four spaces, so a
// multi-line command shows every line apart: a CR, LF, or CRLF ends a line.
// Each line is cleaned on its own (control and bidirectional characters
// dropped, inner whitespace folded); its leading spaces and tabs survive as
// spaces, a tab as four.
func approverLines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lead := 0
	indent:
		for _, r := range l {
			switch r {
			case ' ':
				lead++
			case '\t':
				lead += 4
			default:
				break indent
			}
		}
		lines[i] = "    "
		if c := clean(l, 0); c != "" {
			lines[i] += strings.Repeat(" ", lead) + c
		}
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return strings.Join(lines, "\n")
}

// cutNote is what sessionhub approve and sessionhub deny print for a cut input.
const cutNote = "The input was cut, so this is not all of it; check the terminal."

// errCutAllow is why sessionhub approve refuses a cut input, whatever the flags.
// The server refuses it too (409), which covers a request ID.
const errCutAllow = "approve: the input was cut; allow it in the terminal"

// decide resolves target, a request ID (pr_...) or a session prefix whose
// inbox item has an open request, and sends the decision. For a session
// prefix it first prints the request in full and, unless yes is set, asks
// on the terminal. It never allows a request whose input was cut, because
// the person has not seen all of it; for a request ID, the server's refusal
// says so.
func (e *env) decide(ctx context.Context, target string, in api.DecisionIn, yes bool) error {
	cmd, verb := "approve", "allowed once"
	allow := in.Decision == api.DecisionAllow
	if !allow {
		cmd, verb = "deny", "denied"
	}
	if strings.HasPrefix(target, "pr_") {
		// The server refuses an allow for a cut input with its reason.
		if _, err := e.actions.DecidePermission(ctx, target, in); err != nil {
			return fmt.Errorf("%s: %s", cmd, errText(err))
		}
		fmt.Fprintf(e.out, "%s: %s\n", verb, clean(target, 0))
		return nil
	}
	inbox, err := e.inbox.Inbox(ctx)
	if err != nil {
		return fmt.Errorf("%s: %s", cmd, errText(err))
	}
	it, err := findInboxItem(inbox.Items, target)
	if err != nil {
		return fmt.Errorf("%s: %s", cmd, errText(err))
	}
	p := it.Permission
	if p == nil {
		return fmt.Errorf("%s: %s has no open permission request; see `sessionhub inbox`, or `sessionhub inbox --json` for request IDs (pr_...)",
			cmd, shortID(it.Session.ID))
	}
	fmt.Fprintf(e.out, "%s  %s asks to use %s:\n%s\nrequest %s\n", shortID(it.Session.ID), clean(title(it.Session), 0),
		clean(p.ToolName, 0), approverInput(p), clean(p.ID, 0))
	if p.Truncated {
		fmt.Fprintln(e.out, cutNote)
		if allow {
			return errors.New(errCutAllow)
		}
	}
	if !yes {
		if e.stdinTerminal == nil || !e.stdinTerminal() || e.in == nil {
			return fmt.Errorf("%s: stdin is not a terminal, so sessionhub cannot ask; rerun with --yes to answer without asking", cmd)
		}
		q := "Allow this once? [y/N] "
		if !allow {
			q = "Deny this request? [y/N] "
		}
		fmt.Fprint(e.out, q)
		line, _ := bufio.NewReader(e.in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return fmt.Errorf("%s: cancelled; nothing was sent", cmd)
		}
	}
	if _, err := e.actions.DecidePermission(ctx, p.ID, in); err != nil {
		return fmt.Errorf("%s: %s", cmd, errText(err))
	}
	fmt.Fprintf(e.out, "%s: %s %s (%s  %s)\n", verb, clean(p.ToolName, 0), clean(api.PermissionInputText(p.ToolInput), 120),
		shortID(it.Session.ID), clean(title(it.Session), 0))
	return nil
}
