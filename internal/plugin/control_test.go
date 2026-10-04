package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
	"github.com/abdallah/session-hub/internal/resume"
	"github.com/abdallah/session-hub/internal/server"
	"github.com/abdallah/session-hub/internal/store"
)

const rcLink = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"

// liveHub is the real sessionhub server over a temp database, with machine tower.
type liveHub struct {
	st  *store.Store
	srv *httptest.Server
	tok string
	id  int64
	dir string // holds sessionhub.db
}

func newLiveHub(t *testing.T) *liveHub {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sessionhub.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	tok, _, err := st.AddMachine(ctx, "tower", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.MachineByToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, "https://sessionhub.example.test", nil).Handler())
	t.Cleanup(srv.Close)
	return &liveHub{st: st, srv: srv, tok: tok, id: m.ID, dir: dir}
}

func (h *liveHub) clientFunc() func() (*client.Client, error) {
	return func() (*client.Client, error) { return client.New(client.Config{ServerURL: h.srv.URL, Token: h.tok}) }
}

// open registers the session (ended if asked), marks tower's watcher as
// polling, and opens a request as a dashboard tap does.
func (h *liveHub) open(t *testing.T, u api.SessionUpsert, ended bool) api.ControlRequest {
	t.Helper()
	ctx := context.Background()
	c, _ := h.clientFunc()()
	if err := c.UpsertSession(ctx, u); err != nil {
		t.Fatal(err)
	}
	if ended {
		if err := c.PostEvent(ctx, u.ID, api.EventIn{Kind: api.KindEnded, Source: api.SourcePlugin}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.st.RecordPoll(ctx, h.id); err != nil {
		t.Fatal(err)
	}
	req, created, err := h.st.CreateControl(ctx, u.ID, api.RequestedByDashboard)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	return req
}

// scriptHerdr is a herdr where pane (if not "") runs Claude with session,
// using the captured agent.prompt answers for wS:p1 (ok) and wS:p2 (blocked).
// The banner shows on every pane.read after agent.prompt, and from the second
// pane.read after agent.start: live, the first read after a resume has no
// banner yet, and resume.Control takes that read as its baseline.
func scriptHerdr(t *testing.T, pane, session string) *herdrtest.Server {
	hs := herdrtest.New(t, "agent-prompt-ok.ndjson", "agent-prompt-blocked.ndjson")
	acted := func() bool {
		started, reads := false, 0
		for _, r := range hs.Requests() {
			switch {
			case r.Method == "agent.prompt":
				return true
			case r.Method == "agent.start":
				started = true
			case started && r.Method == "pane.read":
				reads++ // includes the read being answered
			}
		}
		return reads >= 2
	}
	hs.Handle("pane.get", func(p json.RawMessage) (any, string) {
		var in struct {
			PaneID string `json:"pane_id"`
		}
		json.Unmarshal(p, &in)
		if in.PaneID == "wN:p1" {
			// The new workspace's pane: Claude runs there once agent.start ran.
			for _, r := range hs.Requests() {
				if r.Method == "agent.start" {
					return map[string]any{"type": "pane_info", "pane": map[string]any{
						"pane_id": "wN:p1", "workspace_id": "wN", "agent": "claude", "agent_status": "idle"}}, ""
				}
			}
		}
		if pane == "" || in.PaneID != pane {
			return nil, "pane_not_found"
		}
		return map[string]any{"type": "pane_info", "pane": map[string]any{
			"pane_id": pane, "workspace_id": "wS", "agent": "claude", "agent_status": "idle",
			"agent_session": map[string]any{"source": "herdr:claude", "agent": "claude", "kind": "id", "value": session}}}, ""
	})
	hs.Handle("session.snapshot", func(json.RawMessage) (any, string) {
		return map[string]any{"type": "session_snapshot", "snapshot": map[string]any{"panes": []any{}}}, ""
	})
	hs.Handle("pane.read", func(json.RawMessage) (any, string) {
		text := "\n❯ \n"
		if acted() {
			text = "\n❯ /remote-control\n  /remote-control is active · Continue here, on your phone, or at " + rcLink + "\n"
		}
		return map[string]any{"type": "pane_read", "read": map[string]any{"text": text}}, ""
	})
	hs.Handle("workspace.create", func(json.RawMessage) (any, string) {
		return map[string]any{"type": "workspace_created", "workspace": map[string]any{"workspace_id": "wN"},
			"root_pane": map[string]any{"pane_id": "wN:p1", "workspace_id": "wN"}}, ""
	})
	hs.Handle("agent.start", func(json.RawMessage) (any, string) { return map[string]any{"type": "agent_started"}, "" })
	return hs
}

// The watcher, end to end: the real server, a herdrtest socket, and
// resume.Control. Each row opens a request, runs one poll, and reads the
// result back from the API.
func TestControlDecisionTable(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone")
	cases := []struct {
		name       string
		pane       string // recorded herdr pane
		herdrPane  string // the pane herdr knows ("" for none)
		ended      bool
		cwd        string
		wantState  string
		wantURL    string
		wantDetail string
		calls      []string
		never      []string
	}{
		{"inject into the running pane", "wS:p1", "wS:p1", false, dir, api.ControlDone, rcLink, "",
			[]string{"pane.get", "agent.prompt"}, []string{"workspace.create", "agent.start"}},
		{"blocked: fail, type nothing", "wS:p2", "wS:p2", false, dir, api.ControlFailed, "", "waiting on a prompt in the pane",
			[]string{"agent.prompt"}, []string{"workspace.create", "agent.start", "pane.send_keys"}},
		{"ended: resume in a new workspace", "wS:p3", "", true, dir, api.ControlDone, rcLink, "resumed in a new herdr workspace",
			[]string{"workspace.create", "agent.start"}, []string{"agent.prompt"}},
		{"looks live but pane not found", "wS:p3", "", false, dir, api.ControlFailed, "", "looks live but pane not found",
			[]string{"session.snapshot"}, []string{"workspace.create", "agent.start", "agent.prompt"}},
		{"missing directory", "wS:p3", "", true, gone, api.ControlFailed, "", "directory no longer exists",
			nil, []string{"workspace.create", "agent.start"}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sessionhub := newLiveHub(t)
			id := fmt.Sprintf("0a1b2c3d-1111-4222-8333-%012d", i)
			hs := scriptHerdr(t, tc.herdrPane, id)
			req := sessionhub.open(t, api.SessionUpsert{ID: id, Agent: "claude", Source: api.SourcePlugin, CWD: tc.cwd,
				HerdrSession: "default", HerdrPane: tc.pane, TitleHint: "rc scratch"}, tc.ended)
			var logs bytes.Buffer
			ctl := newController(sessionhub.clientFunc(), controlRunner(hs.Path, 5*time.Millisecond, 300*time.Millisecond), log.New(&logs, "", 0))
			ctl.wait = time.Second
			if d := ctl.once(ctx); d != 0 {
				t.Fatalf("once paused %s; log:\n%s", d, logs.String())
			}
			c, _ := sessionhub.clientFunc()()
			d, err := c.GetSession(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			rc := d.RemoteControl
			if rc == nil || rc.ID != req.ID || rc.State != tc.wantState || rc.URL != tc.wantURL || !strings.Contains(rc.Detail, tc.wantDetail) {
				t.Fatalf("request after the poll: %+v; log:\n%s", rc, logs.String())
			}
			if d.RemoteControlURL != tc.wantURL {
				t.Errorf("session link %q, want %q", d.RemoteControlURL, tc.wantURL)
			}
			results := 0
			for _, ev := range d.Events {
				if ev.Kind == api.KindRemoteControlResult {
					results++
				}
			}
			if results != 1 {
				t.Errorf("%d result events, want 1", results)
			}
			var methods []string
			for _, r := range hs.Requests() {
				methods = append(methods, r.Method)
			}
			got := "," + strings.Join(methods, ",") + ","
			for _, m := range tc.calls {
				if !strings.Contains(got, ","+m+",") {
					t.Errorf("%s never ran; methods %s", m, got)
				}
			}
			for _, m := range tc.never {
				if strings.Contains(got, ","+m+",") {
					t.Errorf("%s ran; methods %s", m, got)
				}
			}
			if tc.name == "ended: resume in a new workspace" {
				for _, r := range hs.Requests() {
					var p map[string]any
					json.Unmarshal(r.Params, &p)
					if r.Method == "workspace.create" && (p["cwd"] != dir || p["label"] != "sessionhub: rc scratch" || p["focus"] != false) {
						t.Errorf("workspace.create params %v", p)
					}
					if r.Method == "agent.start" && fmt.Sprint(p["args"]) != "[--resume "+id+" --remote-control]" {
						t.Errorf("agent.start params %v", p)
					}
				}
			}
		})
	}
}

// resumedHerdr is scriptHerdr for a session no pane runs. After agent.start,
// the new workspace's pane wN:p1 runs Claude at first, then agent (claude, or
// "" for a shell that Claude has left), but herdr doesn't report the session
// in it yet.
func resumedHerdr(t *testing.T, session, agent string) *herdrtest.Server {
	hs := scriptHerdr(t, "", session)
	var mu sync.Mutex
	confirmed := 0 // agent.start calls whose confirming look is done
	hs.Handle("pane.get", func(p json.RawMessage) (any, string) {
		var in struct {
			PaneID string `json:"pane_id"`
		}
		json.Unmarshal(p, &in)
		starts := 0
		for _, r := range hs.Requests() {
			if r.Method == "agent.start" {
				starts++
			}
		}
		if in.PaneID != "wN:p1" || starts == 0 {
			return nil, "pane_not_found"
		}
		// The first look after each agent.start is the resume confirming that
		// Claude started; later looks show what the pane runs by then.
		mu.Lock()
		first := starts > confirmed
		confirmed = starts
		mu.Unlock()
		if first {
			return map[string]any{"type": "pane_info", "pane": map[string]any{
				"pane_id": "wN:p1", "workspace_id": "wN", "agent": "claude", "agent_status": "idle"}}, ""
		}
		pane := map[string]any{"pane_id": "wN:p1", "workspace_id": "wN", "agent_status": "unknown"}
		if agent != "" {
			pane["agent"], pane["agent_status"] = agent, "idle"
		}
		return map[string]any{"type": "pane_info", "pane": pane}, ""
	})
	hs.Handle("agent.prompt", func(json.RawMessage) (any, string) {
		return map[string]any{"type": "agent_prompted"}, ""
	})
	return hs
}

// The safety rule: never start a second copy of a running session. A second
// request soon after a resume, before herdr reports the session in its new
// pane, must not resume it again.
func TestControlJustResumed(t *testing.T) {
	cases := []struct {
		name       string
		agent      string        // what the new pane runs
		advance    time.Duration // time between the two requests
		wantState  string
		wantDetail string
		wantStarts int  // workspace.create and agent.start calls in all
		wantPrompt bool // agent.prompt went to the new pane
	}{
		{"Claude in the new pane: send /remote-control there", "claude", 10 * time.Second,
			api.ControlDone, "sent, link not seen", 1, true},
		{"a shell in the new pane: fail, start nothing", "", 10 * time.Second,
			api.ControlFailed, "just resumed; try again in a moment", 1, false},
		{"two minutes later: the guard is over, resume again", "", 2*time.Minute + time.Second,
			api.ControlDone, "resumed in a new herdr workspace, link not seen", 2, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sessionhub := newLiveHub(t)
			id := fmt.Sprintf("0a1b2c3d-2222-4222-8333-%012d", i)
			hs := resumedHerdr(t, id, tc.agent)
			sessionhub.open(t, api.SessionUpsert{ID: id, Agent: "claude", Source: api.SourcePlugin, CWD: t.TempDir(),
				HerdrSession: "default", HerdrPane: "wS:p3", TitleHint: "rc scratch"}, true)
			var logs bytes.Buffer
			ctl := newController(sessionhub.clientFunc(), controlRunner(hs.Path, 5*time.Millisecond, 300*time.Millisecond), log.New(&logs, "", 0))
			ctl.wait = time.Second
			now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
			ctl.now = func() time.Time { return now }
			c, _ := sessionhub.clientFunc()()

			ctl.once(ctx)
			d, err := c.GetSession(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if rc := d.RemoteControl; rc == nil || rc.State != api.ControlDone || rc.Detail != "resumed in a new herdr workspace" {
				t.Fatalf("first request: %+v; log:\n%s", rc, logs.String())
			}

			// The second request, with the session still ended on the sessionhub.
			now = now.Add(tc.advance)
			req, created, err := sessionhub.st.CreateControl(ctx, id, api.RequestedByDashboard)
			if err != nil || !created {
				t.Fatalf("second request: %v %v", created, err)
			}
			ctl.once(ctx)
			if d, err = c.GetSession(ctx, id); err != nil {
				t.Fatal(err)
			}
			if rc := d.RemoteControl; rc == nil || rc.ID != req.ID || rc.State != tc.wantState || rc.Detail != tc.wantDetail {
				t.Errorf("second request: %+v; log:\n%s", rc, logs.String())
			}
			creates, starts, prompted := 0, 0, false
			for _, r := range hs.Requests() {
				var p map[string]any
				json.Unmarshal(r.Params, &p)
				switch r.Method {
				case "workspace.create":
					creates++
				case "agent.start":
					starts++
				case "agent.prompt":
					prompted = prompted || p["target"] == "wN:p1"
				}
			}
			if creates != tc.wantStarts || starts != tc.wantStarts {
				t.Errorf("workspace.create %d, agent.start %d; want %d of each", creates, starts, tc.wantStarts)
			}
			if prompted != tc.wantPrompt {
				t.Errorf("agent.prompt to wN:p1: %v, want %v", prompted, tc.wantPrompt)
			}
		})
	}
}

// A resume whose agent.start failed may still have started Claude. The
// watcher remembers the pane all the same, so a second request can't resume
// the session again.
func TestControlRemembersPaneAfterStartError(t *testing.T) {
	var panes []string
	run := func(ctx context.Context, s api.Session, known string) resume.ControlResult {
		panes = append(panes, known)
		return resume.ControlResult{Outcome: resume.OutcomeError, Workspace: "wN", Pane: "wN:p1", Err: errors.New("agent.start: timeout")}
	}
	ctl := newController(nil, run, log.New(io.Discard, "", 0))
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	ctl.now = func() time.Time { return now }
	s := api.Session{ID: "0a1b2c3d-4444-4222-8333-000000000000"}

	ctl.control(context.Background(), s)
	now = now.Add(time.Minute)
	ctl.control(context.Background(), s)
	now = now.Add(2 * time.Minute)
	ctl.control(context.Background(), s)
	if got, want := strings.Join(panes, ","), ",wN:p1,"; got != want {
		t.Errorf("known panes passed = %q, want %q", got, want)
	}
}

// Install and uninstall wait for a watcher that finishes a request and posts
// its result before they give up on it.
func TestStopWatcherTimeoutCoversResultPost(t *testing.T) {
	if stopWatcherTimeout < controlPostFor+5*time.Second {
		t.Errorf("stopWatcherTimeout = %s, want at least %s", stopWatcherTimeout, controlPostFor+5*time.Second)
	}
}

// A watcher stopped while it runs a request still reports the result.
func TestControlPostsResultAfterCancel(t *testing.T) {
	sessionhub := newLiveHub(t)
	id := "0a1b2c3d-3333-4222-8333-000000000000"
	req := sessionhub.open(t, api.SessionUpsert{ID: id, Agent: "claude", Source: api.SourcePlugin, CWD: t.TempDir(),
		HerdrSession: "default", HerdrPane: "wS:p1"}, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	ctl := newController(sessionhub.clientFunc(), func(context.Context, api.Session, string) resume.ControlResult {
		cancel() // the watcher stops after /remote-control went out
		return resume.ControlResult{Outcome: resume.OutcomeNoLink, Pane: "wS:p1"}
	}, log.New(&logs, "", 0))
	ctl.wait = time.Second
	ctl.once(ctx)
	c, _ := sessionhub.clientFunc()()
	d, err := c.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rc := d.RemoteControl; rc == nil || rc.ID != req.ID || rc.State != api.ControlDone || rc.Detail != "sent, link not seen" {
		t.Fatalf("request: %+v; log:\n%s", rc, logs.String())
	}
}

func TestResultFor(t *testing.T) {
	long := strings.Repeat("é", 300)
	cases := []struct {
		in   resume.ControlResult
		want api.ControlResultIn
	}{
		{resume.ControlResult{Outcome: resume.OutcomeLinked, URL: rcLink}, api.ControlResultIn{State: "done", URL: rcLink}},
		{resume.ControlResult{Outcome: resume.OutcomeAlreadyOn, URL: rcLink}, api.ControlResultIn{State: "done", URL: rcLink, Detail: "Remote Control was already on"}},
		{resume.ControlResult{Outcome: resume.OutcomeAlreadyOn, URL: rcLink, DialogLeftOpen: "Claude is working, so sessionhub did not press Esc. Press Esc in the pane to close the dialog."},
			api.ControlResultIn{State: "done", URL: rcLink, Detail: "Remote Control was already on; press Esc in the pane to close its dialog"}},
		{resume.ControlResult{Outcome: resume.OutcomeNoLink}, api.ControlResultIn{State: "done", Detail: "sent, link not seen"}},
		{resume.ControlResult{Outcome: resume.OutcomeResumed, URL: rcLink}, api.ControlResultIn{State: "done", URL: rcLink, Detail: "resumed in a new herdr workspace"}},
		{resume.ControlResult{Outcome: resume.OutcomeResumed}, api.ControlResultIn{State: "done", Detail: "resumed in a new herdr workspace, link not seen"}},
		{resume.ControlResult{Outcome: resume.OutcomeBlocked}, api.ControlResultIn{State: "failed", Detail: "waiting on a prompt in the pane"}},
		{resume.ControlResult{Outcome: resume.OutcomeDialogOpen}, api.ControlResultIn{State: "failed", Detail: "a Remote Control dialog is open in the pane; press Esc there"}},
		{resume.ControlResult{Outcome: resume.OutcomeLooksLive}, api.ControlResultIn{State: "failed", Detail: "looks live but pane not found"}},
		{resume.ControlResult{Outcome: resume.OutcomeNoDir}, api.ControlResultIn{State: "failed", Detail: "the session's directory no longer exists"}},
		{resume.ControlResult{Outcome: resume.OutcomeNotRunning}, api.ControlResultIn{State: "failed", Detail: "not running"}},
		{resume.ControlResult{Outcome: resume.OutcomeJustResumed}, api.ControlResultIn{State: "failed", Detail: "just resumed; try again in a moment"}},
		// herdr error text can hold anything: control characters go, and
		// the detail fits the server's free-text rules.
		{resume.ControlResult{Outcome: resume.OutcomeError, Err: errors.New("herdr: agent_not_found: x\x1b[31m\nnext")},
			api.ControlResultIn{State: "failed", Detail: "herdr: agent_not_found: x[31m next"}},
		{resume.ControlResult{Outcome: resume.OutcomeError, Err: errors.New(long)},
			api.ControlResultIn{State: "failed", Detail: strings.Repeat("é", 199) + "…"}},
		{resume.ControlResult{Outcome: resume.OutcomeError}, api.ControlResultIn{State: "failed", Detail: "herdr call failed"}},
		// Claude never started in the new pane: the dashboard shows why.
		{resume.ControlResult{Outcome: resume.OutcomeError, Workspace: "w9", Pane: "w9:p1",
			Err: errors.New("claude didn't start in the new workspace w9: zsh: command not found: laude")},
			api.ControlResultIn{State: "failed", Detail: "claude didn't start in the new workspace w9: zsh: command not found: laude"}},
	}
	for _, tc := range cases {
		if got := resultFor(tc.in); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.in.Outcome, got, tc.want)
		}
	}
}

// statusServer answers the control poll with one status per call, in order;
// a 200 carries "{}".
func statusServer(t *testing.T, statuses ...int) *httptest.Server {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		st := statuses[min(n, len(statuses)-1)]
		n++
		mu.Unlock()
		if r.URL.Path != "/v1/machines/self/control" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(st)
		if st == 200 {
			io.WriteString(w, "{}")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestControlBackoff(t *testing.T) {
	ctx := context.Background()
	quiet := log.New(io.Discard, "", 0)
	srv := statusServer(t, 502, 502, 502, 502, 502, 502, 204, 503, 401, 403, 429, 200)
	ctl := newController(func() (*client.Client, error) {
		return client.New(client.Config{ServerURL: srv.URL, Token: "hub_m_test"})
	}, nil, quiet)
	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second, // network errors
		time.Second,                      // a 204 that came back at once: at most one poll a second
		5 * time.Second,                  // the backoff starts over after a success
		5 * time.Minute, 5 * time.Minute, // 401, 403
		5 * time.Second,  // 429 (too many polls) backs off like a network error
		10 * time.Second, // a 200 without a request is a broken server
	}
	for i, w := range want {
		if got := ctl.once(ctx); got != w {
			t.Errorf("poll %d: pause %s, want %s", i+1, got, w)
		}
	}
	// A server that isn't there backs off the same way.
	dead := newController(func() (*client.Client, error) {
		return client.New(client.Config{ServerURL: deadURL(t), Token: "hub_m_test"})
	}, nil, quiet)
	if got := dead.once(ctx); got != 5*time.Second {
		t.Errorf("dead server: pause %s", got)
	}
	// No server_url: a config problem, paused like an auth error.
	nocfg := newController(func() (*client.Client, error) { return client.New(client.Config{}) }, nil, quiet)
	if got := nocfg.once(ctx); got != 5*time.Minute {
		t.Errorf("no config: pause %s", got)
	}
}

// loop keeps polling and pausing until its context ends, then returns.
func TestControlLoopStopsWithContext(t *testing.T) {
	srv := statusServer(t, 502)
	var logs bytes.Buffer
	ctl := newController(func() (*client.Client, error) {
		return client.New(client.Config{ServerURL: srv.URL, Token: "hub_m_test"})
	}, nil, log.New(&logs, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	var pauses []time.Duration
	ctl.sleep = func(ctx context.Context, d time.Duration) bool {
		pauses = append(pauses, d)
		if len(pauses) == 3 {
			cancel()
			return false
		}
		return true
	}
	done := make(chan struct{})
	go func() { ctl.loop(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not return after its context ended")
	}
	if fmt.Sprint(pauses) != "[5s 10s 20s]" {
		t.Errorf("pauses %v", pauses)
	}
	if !strings.Contains(logs.String(), "control: poll failed; retrying in 5s") {
		t.Errorf("log:\n%s", logs.String())
	}
}
