package cli

import (
	"context"
	"fmt"
	"strconv"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

// There is no approve, reject, or rm here: the CLI runs with the machine
// token, so a session could approve its own proposal. Deciding is web-only.
const knowledgeUsage = `usage:
  sessionhub knowledge [ls] [--state proposed|active|all] [--scope S]
  sessionhub knowledge add --kind K --scope S [--why W] [--every N] "<text>"
  sessionhub knowledge search "<query>" [--scope S]`

// RunKnowledge implements `sessionhub knowledge`.
func RunKnowledge(ctx context.Context, args []string) error {
	return runWith(ctx, args, (*env).knowledge)
}

// knowledgeArgs splits args into flag values and positional arguments. Flags
// may come in any order, before or after the positionals.
func knowledgeArgs(args []string, allowed ...string) (flags map[string]string, pos []string, err error) {
	flags = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		ok := false
		for _, n := range allowed {
			ok = ok || a == "--"+n
		}
		switch {
		case ok && i+1 < len(args):
			i++
			flags[a[2:]] = args[i]
		case ok:
			return nil, nil, fmt.Errorf("%s needs a value\n%s", a, knowledgeUsage)
		case len(a) > 1 && a[0] == '-':
			return nil, nil, fmt.Errorf("unknown flag %q\n%s", clean(a, 0), knowledgeUsage)
		default:
			pos = append(pos, a)
		}
	}
	return flags, pos, nil
}

func (e *env) knowledge(ctx context.Context, args []string) error {
	if len(args) > 0 && (args[0] == "ls" || args[0] == "add" || args[0] == "search") {
		switch args[0] {
		case "add":
			return e.knowledgeAdd(ctx, args[1:])
		case "search":
			return e.knowledgeSearch(ctx, args[1:])
		}
		args = args[1:]
	}
	flags, pos, err := knowledgeArgs(args, "state", "scope")
	if err != nil || len(pos) > 0 {
		if err == nil {
			err = fmt.Errorf("unexpected arguments\n%s", knowledgeUsage)
		}
		return fmt.Errorf("knowledge: %w", err)
	}
	state := "active"
	if s, ok := flags["state"]; ok {
		switch s {
		case "proposed", "active":
			state = s
		case "all":
			state = ""
		default:
			return fmt.Errorf("knowledge: --state is proposed, active, or all, not %q\n%s", clean(s, 0), knowledgeUsage)
		}
	}
	list, err := e.actions.ListKnowledge(ctx, state, flags["scope"])
	if err != nil {
		return fmt.Errorf("knowledge: %s", errText(err))
	}
	if len(list) == 0 {
		fmt.Fprintln(e.out, "no knowledge")
		return nil
	}
	for _, k := range list {
		fmt.Fprintf(e.out, "%d  [%s] (%s) %s  (%s, %s)\n", k.ID, clean(k.Kind, 0), clean(k.Scope, 0),
			clean(k.Text, 0), clean(k.State, 0), clean(k.CreatedBy, 0))
	}
	return nil
}

func (e *env) knowledgeAdd(ctx context.Context, args []string) error {
	flags, pos, err := knowledgeArgs(args, "kind", "scope", "why", "every")
	if err == nil && (len(pos) != 1 || flags["kind"] == "" || flags["scope"] == "") {
		err = fmt.Errorf("add needs --kind, --scope, and the text\n%s", knowledgeUsage)
	}
	if err != nil {
		return fmt.Errorf("knowledge: %w", err)
	}
	in := api.KnowledgeIn{ClientID: client.NewKnowledgeID(), Kind: flags["kind"], Scope: flags["scope"],
		Text: pos[0], Why: flags["why"]}
	if v, ok := flags["every"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100 {
			return fmt.Errorf("knowledge add: --every needs 0 (every prompt) to 100, not %q\n%s", clean(v, 0), knowledgeUsage)
		}
		in.Every = &n
	}
	if sid := e.getenv("CLAUDE_CODE_SESSION_ID"); sessionIDRE.MatchString(sid) {
		in.SourceSession = sid
	}
	r, err := e.actions.ProposeKnowledge(ctx, in)
	if err != nil {
		return fmt.Errorf("knowledge add: %s", errText(err))
	}
	fmt.Fprintf(e.out, "proposed knowledge %d; approve it on the dashboard\n", r.Entry.ID)
	for _, s := range r.Similar {
		fmt.Fprintf(e.out, "similar: #%d [%s] %s\n", s.ID, clean(s.Kind, 0), clean(s.Text, 0))
	}
	return nil
}

func (e *env) knowledgeSearch(ctx context.Context, args []string) error {
	flags, pos, err := knowledgeArgs(args, "scope")
	if err == nil && len(pos) != 1 {
		err = fmt.Errorf("search needs one query\n%s", knowledgeUsage)
	}
	if err != nil {
		return fmt.Errorf("knowledge: %w", err)
	}
	list, err := e.actions.SearchKnowledge(ctx, pos[0], flags["scope"])
	if err != nil {
		return fmt.Errorf("knowledge search: %s", errText(err))
	}
	if len(list) == 0 {
		fmt.Fprintln(e.out, "no matching knowledge")
		return nil
	}
	for _, k := range list {
		fmt.Fprintf(e.out, "#%d [%s] (%s) %s\n", k.ID, clean(k.Kind, 0), clean(k.Scope, 0), clean(k.Text, 0))
	}
	return nil
}
