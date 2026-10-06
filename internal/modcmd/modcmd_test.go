package modcmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/server"
	"github.com/abdallah/session-hub/internal/store"
)

const sid = "6f1d2c3b-aaaa-bbbb-cccc-000000000001"

// fixture runs `sessionhub mod` against the real server handler and store.
type fixture struct {
	st    *store.Store
	url   string
	token string
	state string
	srv   *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(home, "config.toml"))
	t.Setenv("SESSIONHUB_STATE_DIR", filepath.Join(home, "state"))
	st, err := store.Open(filepath.Join(home, "sessionhub.db"), store.Options{StaleAfter: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tok, _, err := st.AddMachine(context.Background(), "tower", "ssh.example.test", "ssh.example.test")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, "https://sessionhub.example.test", log.New(io.Discard, "", 0)).Handler())
	t.Cleanup(srv.Close)
	f := &fixture{st: st, url: srv.URL, token: tok, state: filepath.Join(home, "state"), srv: srv}
	c := f.client(t)
	if err := c.UpsertSession(context.Background(), api.SessionUpsert{ID: sid, Agent: "claude", Source: "hooks", CWD: "/tmp/p", AgentState: "working"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) client(t *testing.T) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{ServerURL: f.url, Token: f.token})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// call runs `sessionhub mod args...` with stdin and returns stdout, stderr, and the
// error.
func (f *fixture) call(args []string, stdin string) (string, string, error) {
	var out, errOut bytes.Buffer
	e := &env{
		stdin:  strings.NewReader(stdin),
		stdout: &out,
		stderr: &errOut,
		newClient: func() (*client.Client, error) {
			c, err := client.New(client.Config{ServerURL: f.url, Token: f.token})
			if err == nil {
				c.SetTimeout(2 * time.Second)
			}
			return c, err
		},
		queue:        client.NewQueue(f.state),
		stateDir:     f.state,
		now:          time.Now,
		pollWait:     time.Second,
		pollDeadline: 5 * time.Second,
	}
	err := e.run(context.Background(), args)
	return out.String(), errOut.String(), err
}

func (f *fixture) session(t *testing.T) api.Session {
	t.Helper()
	s, err := f.st.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fixture) queued(t *testing.T) []client.Item {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.state, "queue.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var items []client.Item
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var it client.Item
		if err := json.Unmarshal(sc.Bytes(), &it); err != nil {
			t.Fatal(err)
		}
		items = append(items, it)
	}
	return items
}

// The AskUserQuestion input as the mod passes it (e.questions as is), with a
// field sessionhub does not read.
const questionsIn = `{"questions":[{"question":"Which library should we use?","header":"Library",` +
	`"options":[{"label":"date-fns","description":"small"},{"label":"luxon","description":"zones"}],` +
	`"multiSelect":false,"kind":"choice"}],"extra":1}`

func TestBlockedOnSetAndClear(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.call([]string{"blocked-on", "--session", sid}, questionsIn); err != nil {
		t.Fatal(err)
	}
	s := f.session(t)
	if s.BlockedOn != "Question: Which library should we use? (date-fns, luxon)" || s.AgentState != "blocked" {
		t.Errorf("blocked_on %q state %q", s.BlockedOn, s.AgentState)
	}
	if s.Messageable {
		t.Error("blocked-on made the session messageable; only the poll does")
	}
	// A clear for another question (a late one) changes nothing.
	other := strings.Replace(questionsIn, "Which library", "Which other library", 1)
	if _, _, err := f.call([]string{"blocked-on", "--session", sid}, strings.Replace(other, `"extra":1`, `"clear":true`, 1)); err != nil {
		t.Fatal(err)
	}
	if s := f.session(t); s.AgentState != "blocked" {
		t.Errorf("a stale clear moved the state to %q", s.AgentState)
	}
	// The mod's clear: the same questions, with clear set.
	if _, _, err := f.call([]string{"blocked-on", "--session", sid}, strings.Replace(questionsIn, `"extra":1`, `"clear":true`, 1)); err != nil {
		t.Fatal(err)
	}
	if s := f.session(t); s.BlockedOn != "" || s.AgentState != "working" {
		t.Errorf("after the clear: blocked_on %q state %q", s.BlockedOn, s.AgentState)
	}
	// An older mod's clear, {"text": ""}, still works for a mod-set block.
	if _, _, err := f.call([]string{"blocked-on", "--session", sid}, questionsIn); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.call([]string{"blocked-on", "--session", sid}, `{"text":""}`); err != nil {
		t.Fatal(err)
	}
	if s := f.session(t); s.BlockedOn != "" || s.AgentState != "working" {
		t.Errorf("after the legacy clear: blocked_on %q state %q", s.BlockedOn, s.AgentState)
	}
	if q := f.queued(t); len(q) != 0 {
		t.Errorf("queued %+v", q)
	}
}

// A question that reaches sessionhub after the turn ended, and its clear, leave the
// idle session idle.
func TestBlockedOnAfterTurnEnded(t *testing.T) {
	f := newFixture(t)
	if err := f.client(t).UpsertSession(context.Background(), api.SessionUpsert{ID: sid, Agent: "claude", Source: "hooks", CWD: "/tmp/p", AgentState: "idle"}); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{questionsIn, strings.Replace(questionsIn, `"extra":1`, `"clear":true`, 1)} {
		if _, _, err := f.call([]string{"blocked-on", "--session", sid}, in); err != nil {
			t.Fatal(err)
		}
		if s := f.session(t); s.AgentState != "idle" || s.BlockedOn != "" {
			t.Errorf("after %s: state %q blocked_on %q", in, s.AgentState, s.BlockedOn)
		}
	}
}

func TestFormatQuestions(t *testing.T) {
	q := func(text, header string, labels ...string) question {
		var x question
		x.Question, x.Header = text, header
		for _, l := range labels {
			x.Options = append(x.Options, struct {
				Label string `json:"label"`
			}{l})
		}
		return x
	}
	tests := []struct {
		in   []question
		want string
	}{
		{nil, "Question"},
		{[]question{q("Name it?", "Name")}, "Question: Name it?"},
		{[]question{q("", "Auth", "a", " ", "b")}, "Question: Auth (a, b)"},
		{[]question{q("One?", "", "x"), q("Two?", "", "y", "z")}, "Questions: One? (x) · Two? (y, z)"},
	}
	for _, tt := range tests {
		if got := formatQuestions(tt.in); got != tt.want {
			t.Errorf("formatQuestions(%+v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A question is never queued: replayed later, it would block a session that
// was already answered. A clear is.
func TestBlockedOnOffline(t *testing.T) {
	f := newFixture(t)
	f.srv.Close()
	if _, _, err := f.call([]string{"blocked-on", "--session", sid}, questionsIn); err == nil {
		t.Error("a question to an unreachable server succeeded")
	}
	if q := f.queued(t); len(q) != 0 {
		t.Fatalf("the question was queued: %+v", q)
	}
	_, stderr, err := f.call([]string{"blocked-on", "--session", sid}, strings.Replace(questionsIn, `"extra":1`, `"clear":true`, 1))
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, _, err := f.call([]string{"blocked-on", "--session", sid}, `{"text": ""}`); err != nil {
		t.Fatalf("legacy clear: %v", err)
	}
	q := f.queued(t)
	if len(q) != 2 || q[0].Op != client.OpBlockedOnClear || q[0].SessionID != sid ||
		string(q[0].Body) != `{"text":"","clears":"Question: Which library should we use? (date-fns, luxon)"}` ||
		q[1].Op != client.OpBlockedOnClear || len(q[1].Body) != 0 {
		t.Errorf("queued %+v", q)
	}
	if !strings.Contains(stderr, "queued") {
		t.Errorf("stderr %q", stderr)
	}
}

// A clear the server refuses for good (an unknown session) is not queued.
func TestBlockedOnClearRefusedNotQueued(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.call([]string{"blocked-on", "--session", "unknown-session"}, `{"text":""}`); err == nil {
		t.Error("no error for an unknown session")
	}
	if q := f.queued(t); len(q) != 0 {
		t.Errorf("queued %+v", q)
	}
}

func TestBadInput(t *testing.T) {
	f := newFixture(t)
	for _, tt := range []struct {
		args  []string
		stdin string
	}{
		{nil, ""},
		{[]string{"bogus"}, ""},
		{[]string{"blocked-on"}, `{"text":""}`},
		{[]string{"blocked-on", "--session", "bad id"}, `{"text":""}`},
		{[]string{"blocked-on", "--session", sid, "--bogus"}, `{"text":""}`},
		{[]string{"blocked-on", "--session", sid, "extra"}, `{"text":""}`},
		{[]string{"blocked-on", "--session", sid}, `not json`},
		{[]string{"blocked-on", "--session", sid}, `{}`},
		{[]string{"blocked-on", "--session", sid}, ``},
		{[]string{"usage", "--session", sid}, `[]`},
		{[]string{"poll"}, ""},
		{[]string{"poll", "--session", sid, "--wait", "3"}, ""},
		{[]string{"result", "--message", "msg_x", "--state", "delivered"}, ""},
		{[]string{"result", "--message", "msg_0123456789abcdefghijkl", "--state", "failed"}, ""},
		{[]string{"result", "--message", "msg_0123456789abcdefghijkl"}, ""},
		{[]string{"inbox-count", "x"}, ""},
	} {
		if _, _, err := f.call(tt.args, tt.stdin); err == nil {
			t.Errorf("%v with %q: no error", tt.args, tt.stdin)
		}
	}
	if s := f.session(t); s.BlockedOn != "" || s.AgentState != "working" {
		t.Errorf("bad input changed the session: %+v", s)
	}
}

func TestBlockedOnStdinLimit(t *testing.T) {
	f := newFixture(t)
	big := `{"text":"` + strings.Repeat("a", maxStdin) + `"}`
	if _, _, err := f.call([]string{"blocked-on", "--session", sid}, big); err == nil || !strings.Contains(err.Error(), "over") {
		t.Errorf("err = %v", err)
	}
}

func TestUsageThrottle(t *testing.T) {
	f := newFixture(t)
	report := func(stdin string) {
		t.Helper()
		if _, _, err := f.call([]string{"usage", "--session", sid}, stdin); err != nil {
			t.Fatal(err)
		}
	}
	usageAt := func() time.Time {
		t.Helper()
		s := f.session(t)
		if s.UsageAt == nil {
			return time.Time{}
		}
		return *s.UsageAt
	}
	report(`{"context":{"tokens":84000,"window":200000,"percent":42},"cost":{"usd":1.25},"rateLimits":{}}`)
	s := f.session(t)
	if s.ContextPercent == nil || *s.ContextPercent != 42 || s.LiveCostUSD == nil || *s.LiveCostUSD != 1.25 {
		t.Fatalf("first report: %+v", s)
	}
	if s.Messageable {
		t.Error("usage made the session messageable; only the poll does")
	}
	first := usageAt()

	// Below both thresholds: no call.
	time.Sleep(10 * time.Millisecond)
	report(`{"context":{"window":200000,"percent":42.4},"cost":{"usd":1.255}}`)
	if !usageAt().Equal(first) {
		t.Error("a change under 1 point and $0.01 was reported")
	}
	// The cost moved by $0.01 since the last report sent.
	report(`{"context":{"window":200000,"percent":42},"cost":{"usd":1.26}}`)
	if s := f.session(t); *s.LiveCostUSD != 1.26 || usageAt().Equal(first) {
		t.Errorf("a $0.01 change was not reported: %+v", s)
	}
	// Percent left out: computed from tokens and window.
	report(`{"context":{"tokens":100000,"window":200000}}`)
	if s := f.session(t); *s.ContextPercent != 50 || *s.LiveCostUSD != 1.26 {
		t.Errorf("tokens/window: %+v", s)
	}
	// Nothing sessionhub stores: no call, no error.
	report(`{"context":{"window":200000}}`)
	report(`{}`)

	var st usageState
	b, err := os.ReadFile(filepath.Join(f.state, "mod-usage", sid+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil || *st.ContextPercent != 50 || *st.CostUSD != 1.26 {
		t.Errorf("state %s %v", b, err)
	}
}

// A failed report leaves the state alone, so the next one goes out.
func TestUsageFailureKeepsState(t *testing.T) {
	f := newFixture(t)
	f.srv.Close()
	if _, _, err := f.call([]string{"usage", "--session", sid}, `{"context":{"window":1,"percent":10}}`); err == nil {
		t.Error("no error from an unreachable server")
	}
	if _, err := os.Stat(filepath.Join(f.state, "mod-usage", sid+".json")); !os.IsNotExist(err) {
		t.Errorf("state written after a failure: %v", err)
	}
}

func TestPruneUsageState(t *testing.T) {
	dir := t.TempDir()
	old, fresh := filepath.Join(dir, "old.json"), filepath.Join(dir, "fresh.json")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if err := os.Chtimes(old, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	pruneUsageState(dir, now)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old state kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh state removed")
	}
}

func TestPollAndResult(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// An empty poll prints nothing and makes the session messageable.
	out, _, err := f.call([]string{"poll", "--session", sid}, "")
	if err != nil || out != "" {
		t.Fatalf("empty poll: %q %v", out, err)
	}
	if !f.session(t).Messageable {
		t.Fatal("not messageable after a poll")
	}
	res, err := f.client(t).SendMessages(ctx, api.MessagesIn{SessionIDs: []string{sid}, Text: "run the tests"})
	if err != nil || len(res.Results) != 1 || res.Results[0].State != api.MessageQueued {
		t.Fatalf("send: %+v %v", res, err)
	}
	mid := res.Results[0].ID
	out, _, err = f.call([]string{"poll", "--session", sid}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		t.Errorf("poll output %q is not one line", out)
	}
	var claim struct{ ID, Text string }
	if err := json.Unmarshal([]byte(out), &claim); err != nil || claim.ID != mid || !strings.Contains(claim.Text, "run the tests") {
		t.Fatalf("claim %q: %+v %v", out, claim, err)
	}
	// The mod passes a drop reason as --detail; it may start with "-".
	if _, _, err := f.call([]string{"result", "--message", mid, "--state", "refused", "--detail", "-hook: no\nthanks"}, ""); err != nil {
		t.Fatal(err)
	}
	m, err := f.st.GetMessage(ctx, mid)
	if err != nil {
		t.Fatal(err)
	}
	if m.State != api.MessageRefused || m.Detail != "-hook: no thanks" {
		t.Errorf("message %+v", m)
	}
	// A second result for a closed message is an error.
	if _, _, err := f.call([]string{"result", "--message", mid, "--state", "delivered"}, ""); err == nil {
		t.Error("a result for a closed message succeeded")
	}
}

func TestPollErrors(t *testing.T) {
	f := newFixture(t)
	// Another machine's session: 409, an error.
	other, _, err := f.st.AddMachine(context.Background(), "bluebox", "bluebox.example.test", "bluebox.example.test")
	if err != nil {
		t.Fatal(err)
	}
	f.token = other
	if out, _, err := f.call([]string{"poll", "--session", sid}, ""); err == nil || out != "" {
		t.Errorf("another machine's poll: %q %v", out, err)
	}
	// An unreachable server: an error, quickly.
	f.srv.Close()
	start := time.Now()
	if out, _, err := f.call([]string{"poll", "--session", sid}, ""); err == nil || out != "" {
		t.Errorf("offline poll: %q %v", out, err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("offline poll took %v", d)
	}
}

func TestInboxCount(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.call([]string{"blocked-on", "--session", sid}, questionsIn); err != nil {
		t.Fatal(err)
	}
	out, _, err := f.call([]string{"inbox-count"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"blocked":1,"waiting":0,"finished":0}`+"\n" {
		t.Errorf("inbox-count printed %q", out)
	}
	f.srv.Close()
	if out, _, err := f.call([]string{"inbox-count"}, ""); err == nil || out != "" {
		t.Errorf("offline: %q %v", out, err)
	}
}

func TestDefaultEnvHonorsStateDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SESSIONHUB_STATE_DIR", dir)
	e := defaultEnv()
	if e.stateDir != dir {
		t.Errorf("stateDir %q, want %q", e.stateDir, dir)
	}
	if e.pollWait != 25*time.Second || e.pollDeadline != 30*time.Second {
		t.Errorf("poll %v / %v, want 25s / 30s", e.pollWait, e.pollDeadline)
	}
}
