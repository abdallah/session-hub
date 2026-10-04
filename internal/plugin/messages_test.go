package plugin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
)

func TestWatcherRefreshesInstructions(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	path := filepath.Join(e.dir, client.InstructionsFile)
	e.w.instructionsPath = path
	e.sessionhub.mu.Lock()
	e.sessionhub.rulesBody = `{"instructions":[{"id":1,"text":"Be brief."}],"version":"v1"}`
	e.sessionhub.mu.Unlock()
	t0 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	e.w.step(ctx, t0)
	got, err := client.LoadInstructions(path)
	if err != nil || got.Version != "v1" || got.Instructions[0].Text != "Be brief." {
		t.Fatalf("copy after the heartbeat: %+v %v", got, err)
	}
	// A failed read keeps the copy and logs once.
	e.sessionhub.mu.Lock()
	e.sessionhub.rulesStatus = http.StatusInternalServerError
	e.sessionhub.mu.Unlock()
	e.w.step(ctx, t0.Add(heartbeatInterval))
	if got, _ := client.LoadInstructions(path); got.Version != "v1" {
		t.Errorf("a failed refresh changed the copy: %+v", got)
	}
	if !strings.Contains(e.logs.String(), "rules: refresh failed") {
		t.Errorf("no failure line:\n%s", e.logs)
	}
	// Without a path the watcher never asks.
	e2 := newTestEnv(t)
	e2.w.step(ctx, t0)
	if n := e2.sessionhub.count(http.MethodGet, "/v1/instructions"); n != 0 {
		t.Errorf("rules read %d times with no path", n)
	}
}

// fakeAgent is a paneAgent with one pane.
type fakeAgent struct {
	pane      herdr.PaneInfo
	getErr    error
	promptErr error
	prompts   []string // "pane|text"
}

func (f *fakeAgent) PaneGet(id string) (herdr.PaneInfo, error) {
	if f.getErr != nil {
		return herdr.PaneInfo{}, f.getErr
	}
	return f.pane, nil
}

func (f *fakeAgent) AgentPrompt(target, text string) error {
	f.prompts = append(f.prompts, target+"|"+text)
	return f.promptErr
}

func TestDeliverMessageOnlyWhenIdle(t *testing.T) {
	const sid = "11111111-2222-4333-8444-555555555555"
	s := api.Session{ID: sid, HerdrPane: "w1:p1"}
	pane := func(status, session string) herdr.PaneInfo {
		p := herdr.PaneInfo{PaneID: "w1:p1", Agent: "claude", AgentStatus: status}
		if session != "" {
			p.AgentSession = &herdr.AgentSessionInfo{Kind: "id", Value: session}
		}
		return p
	}
	herdrErr := &herdr.Error{Code: "pane_not_found", Message: "no pane w1:p1"}
	for name, c := range map[string]struct {
		agent    fakeAgent
		sess     api.Session
		state    string
		detail   string
		prompted bool
	}{
		"idle":           {fakeAgent{pane: pane("idle", sid)}, s, api.MessageDelivered, "", true},
		"done":           {fakeAgent{pane: pane("done", sid)}, s, api.MessageDelivered, "", true},
		"idle, no id":    {fakeAgent{pane: pane("idle", "")}, s, api.MessageBusy, detailNoSessionID, false},
		"unknown status": {fakeAgent{pane: pane("", sid)}, s, api.MessageBusy, "the agent's status is unknown", false},
		"working":        {fakeAgent{pane: pane("working", sid)}, s, api.MessageBusy, "the agent is working", false},
		"blocked":        {fakeAgent{pane: pane("blocked", sid)}, s, api.MessageBusy, "the agent is blocked", false},
		"other session":  {fakeAgent{pane: pane("idle", "99999999-2222-4333-8444-555555555555")}, s, api.MessageRefused, detailOtherSession, false},
		"no agent":       {fakeAgent{pane: herdr.PaneInfo{PaneID: "w1:p1"}}, s, api.MessageRefused, detailNoAgent, false},
		"no pane":        {fakeAgent{pane: pane("idle", sid)}, api.Session{ID: sid}, api.MessageRefused, detailNotInHerdr, false},
		"pane gone":      {fakeAgent{getErr: herdrErr}, s, api.MessageRefused, detailPaneGone, false},
		"herdr down":     {fakeAgent{getErr: errors.New("dial unix: no such file")}, s, api.MessageBusy, detailHerdrDown, false},
		"herdr error":    {fakeAgent{getErr: &herdr.Error{Code: "internal", Message: "try later"}}, s, api.MessageBusy, detailHerdrDown, false},
		"name, no id": {fakeAgent{pane: herdr.PaneInfo{PaneID: "w1:p1", Agent: "claude", AgentStatus: "idle",
			AgentSession: &herdr.AgentSessionInfo{Kind: "name", Value: sid}}}, s, api.MessageBusy, detailNoSessionID, false},
		"no kind": {fakeAgent{pane: herdr.PaneInfo{PaneID: "w1:p1", Agent: "claude", AgentStatus: "idle",
			AgentSession: &herdr.AgentSessionInfo{Value: sid}}}, s, api.MessageBusy, detailNoSessionID, false},
		"agent_blocked":  {fakeAgent{pane: pane("idle", sid), promptErr: &herdr.BlockedError{Err: &herdr.Error{Code: "agent_blocked"}}}, s, api.MessageBusy, detailBlocked, true},
		"prompt refused": {fakeAgent{pane: pane("idle", sid), promptErr: herdrErr}, s, api.MessageRefused, "herdr: pane_not_found: no pane w1:p1", true},
		"prompt failed":  {fakeAgent{pane: pane("idle", sid), promptErr: errors.New("broken pipe")}, s, api.MessageBusy, detailHerdrDown, true},
	} {
		a := c.agent
		got := deliverMessage(&a, c.sess, "From the user via sessionhub (dashboard):\n\nPlease rebase.")
		if got.State != c.state || got.Detail != c.detail {
			t.Errorf("%s: %+v, want %s %q", name, got, c.state, c.detail)
		}
		if prompted := len(a.prompts) > 0; prompted != c.prompted {
			t.Errorf("%s: prompted=%v, want %v", name, prompted, c.prompted)
		}
		if c.prompted && a.prompts[0] != "w1:p1|From the user via sessionhub (dashboard):\n\nPlease rebase." {
			t.Errorf("%s: prompt %q", name, a.prompts[0])
		}
	}
}

func TestControllerDeliversMessage(t *testing.T) {
	h := newLiveHub(t)
	ctx := context.Background()
	c, _ := h.clientFunc()()
	const sid = "11111111-2222-4333-8444-555555555555"
	if err := c.UpsertSession(ctx, api.SessionUpsert{ID: sid, Agent: "claude", Source: api.SourcePlugin,
		HerdrSession: "default", HerdrPane: "w1:p1"}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.RecordPoll(ctx, h.id); err != nil {
		t.Fatal(err)
	}
	res, _, err := h.st.SendMessages(ctx, []string{sid}, "Please rebase.", "dashboard", "web:test")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	ctl := newController(h.clientFunc(), nil, log.New(&logs, "", 0))
	ctl.wait = time.Second
	results := []api.ControlResultIn{{State: api.MessageBusy, Detail: "the agent is working"}, {State: api.MessageDelivered}}
	var got []string
	ctl.deliver = func(_ context.Context, s api.Session, text string) api.ControlResultIn {
		got = append(got, s.ID+"|"+s.HerdrPane+"|"+text)
		r := results[0]
		results = results[1:]
		return r
	}
	ctl.once(ctx)
	m, _ := h.st.GetMessage(ctx, res[0].ID)
	if m.State != api.MessageQueued || m.Detail != "the agent is working" {
		t.Fatalf("after busy: %+v", m)
	}
	// Offered again only after store.MessageRetry: age the offer through a
	// second connection, as the server tests read tables.
	db, err := sql.Open("sqlite", filepath.Join(h.dir, "sessionhub.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE messages SET offered_at = '2000-01-01T00:00:00.000000000Z' WHERE id = ?`, res[0].ID); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ctl.once(ctx)
	if m, _ := h.st.GetMessage(ctx, res[0].ID); m.State != api.MessageDelivered {
		t.Fatalf("after delivered: %+v", m)
	}
	want := sid + "|w1:p1|From the user via sessionhub (dashboard):\n\nPlease rebase."
	if len(got) != 2 || got[0] != want || got[1] != want {
		t.Errorf("deliveries %q", got)
	}
	if strings.Contains(logs.String(), "Please rebase.") {
		t.Errorf("the log holds the message text:\n%s", logs.String())
	}
}

func TestControllerWithoutDeliveryRefuses(t *testing.T) {
	h := newLiveHub(t)
	ctx := context.Background()
	c, _ := h.clientFunc()()
	const sid = "11111111-2222-4333-8444-555555555555"
	c.UpsertSession(ctx, api.SessionUpsert{ID: sid, Agent: "claude", Source: api.SourcePlugin, HerdrSession: "default", HerdrPane: "w1:p1"})
	h.st.RecordPoll(ctx, h.id)
	res, _, _ := h.st.SendMessages(ctx, []string{sid}, "hi", "dashboard", "web:test")
	ctl := newController(h.clientFunc(), nil, log.New(&bytes.Buffer{}, "", 0))
	ctl.wait = time.Second
	ctl.once(ctx)
	if m, _ := h.st.GetMessage(ctx, res[0].ID); m.State != api.MessageRefused || m.Detail != detailNoDelivery {
		t.Errorf("message %+v", m)
	}
}

// TestMessageRunnerOnFakeSocket drives messageRunner over a real socket with
// the captured agent.prompt answer: it asks pane.get, then sends the text.
func TestMessageRunnerOnFakeSocket(t *testing.T) {
	const sid = "11111111-2222-4333-8444-555555555555"
	hs := herdrtest.New(t, "agent-prompt-ok.ndjson")
	status := "working"
	hs.Handle("pane.get", func(json.RawMessage) (any, string) {
		return map[string]any{"type": "pane_info", "pane": map[string]any{
			"pane_id": "wS:p1", "workspace_id": "wS", "agent": "claude", "agent_status": status,
			"agent_session": map[string]any{"source": "herdr:claude", "agent": "claude", "kind": "id", "value": sid}}}, ""
	})
	hs.Handle("agent.prompt", func(json.RawMessage) (any, string) {
		return map[string]any{"type": "agent_prompted"}, ""
	})
	run := messageRunner(hs.Path)
	s := api.Session{ID: sid, HerdrPane: "wS:p1"}
	text := "From the user via sessionhub (dashboard):\n\nPlease rebase."
	if got := run(context.Background(), s, text); got.State != api.MessageBusy || len(hs.RequestsFor("agent.prompt")) != 0 {
		t.Fatalf("working: %+v, prompts %d", got, len(hs.RequestsFor("agent.prompt")))
	}
	status = "idle"
	if got := run(context.Background(), s, text); got.State != api.MessageDelivered {
		t.Fatalf("idle: %+v", got)
	}
	reqs := hs.RequestsFor("agent.prompt")
	var p struct{ Target, Text string }
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Params, &p) != nil || p.Target != "wS:p1" || p.Text != text {
		t.Fatalf("agent.prompt requests: %+v", reqs)
	}
	// No socket: busy, not a crash.
	if got := messageRunner(filepath.Join(t.TempDir(), "none.sock"))(context.Background(), s, text); got.State != api.MessageBusy || got.Detail != detailHerdrDown {
		t.Errorf("no socket: %+v", got)
	}
}
