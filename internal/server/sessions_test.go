package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestUpsertRules(t *testing.T) {
	e := newEnv(t)
	if l := e.list(""); len(l) != 0 {
		t.Fatalf("start state: %d sessions", len(l))
	}
	full := api.SessionUpsert{ID: sid1, Agent: "claude", Source: api.SourcePlugin, CWD: "/home/user/proj",
		GitRepo: "session-hub", GitBranch: "main", HerdrSession: "default", HerdrWorkspace: "w1",
		HerdrPane: "p1", AgentState: "idle"}
	code, b, _ := e.do("POST", "/v1/sessions", e.tokA, full)
	if code != http.StatusCreated {
		t.Fatalf("first upsert: %d %s", code, b)
	}
	var s api.Session
	json.Unmarshal(b, &s)
	t0 := e.clock.Now()
	if s.Machine != "tower" || s.Status != api.StatusLive || !s.StartedAt.Equal(t0) || !s.LastSeenAt.Equal(t0) {
		t.Errorf("first upsert: %+v", s)
	}
	if want := "ssh -t tower.example.com '~/.local/bin/sessionhub resume " + sid1 + "'"; s.ResumeCommand != want {
		t.Errorf("resume_command %q, want %q", s.ResumeCommand, want)
	}

	// A sparse upsert changes nothing but last_seen_at, and returns 200.
	e.clock.Advance(time.Minute)
	code, b, _ = e.do("POST", "/v1/sessions", e.tokA, api.SessionUpsert{ID: sid1, Source: api.SourceHooks, GitBranch: "feature"})
	if code != http.StatusOK {
		t.Fatalf("second upsert: %d %s", code, b)
	}
	d := e.detail(sid1)
	want := s
	want.GitBranch = "feature"
	want.LastSeenAt = e.clock.Now()
	if !reflect.DeepEqual(d.Session, want) {
		t.Errorf("after sparse upsert:\n got %+v\nwant %+v", d.Session, want)
	}
	// Exactly one registered event, from the first insert's source.
	if len(d.Events) != 1 || d.Events[0].Kind != api.KindRegistered || d.Events[0].Source != api.SourcePlugin {
		t.Errorf("events: %+v", d.Events)
	}
	if d.Reports == nil || len(d.Reports) != 0 {
		t.Errorf("reports should be an empty list, got %#v", d.Reports)
	}
}

func TestLivenessTransitions(t *testing.T) {
	e := newEnv(t)
	status := func() string { return e.detail(sid1).Status }
	live := func() []string { return ids(e.list("?live=true")) }

	e.register(e.tokA, api.SessionUpsert{ID: sid1, HerdrSession: "default", HerdrPane: "p1"})
	if got := status(); got != api.StatusLive {
		t.Fatalf("after register: %s", got)
	}

	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"blocked"}`)
	if got := status(); got != api.StatusBlocked {
		t.Errorf("after state_changed blocked: %s", got)
	}
	if got := live(); !reflect.DeepEqual(got, []string{sid1}) {
		t.Errorf("live=true should include blocked: %v", got)
	}

	e.clock.Advance(staleAfter) // exactly at the threshold: not yet stale
	if got := status(); got != api.StatusBlocked {
		t.Errorf("at stale_after: %s, want blocked", got)
	}
	e.clock.Advance(time.Nanosecond)
	if got := status(); got != api.StatusStale {
		t.Errorf("past stale_after: %s, want stale", got)
	}
	if got := live(); len(got) != 0 {
		t.Errorf("live=true should exclude stale: %v", got)
	}
	if got := ids(e.list("?live=false")); !reflect.DeepEqual(got, []string{sid1}) {
		t.Errorf("live=false should include stale: %v", got)
	}

	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"working"}`)
	if d := e.detail(sid1); d.Status != api.StatusLive || d.AgentState != "working" {
		t.Errorf("after state_changed working: %s/%s", d.Status, d.AgentState)
	}

	// The herdr snapshot is the heartbeat: it revives a stale session and
	// stores no event row.
	e.clock.Advance(10 * time.Minute)
	before := len(e.detail(sid1).Events)
	if got := status(); got != api.StatusStale {
		t.Fatalf("before heartbeat: %s", got)
	}
	e.must(200, "PUT", "/v1/machines/self/herdr-sessions", e.tokA,
		api.HerdrSessionsPut{HerdrSession: "default", Sessions: []api.SessionUpsert{{ID: sid1, HerdrPane: "p1"}}}, nil)
	d := e.detail(sid1)
	if d.Status != api.StatusLive || !d.LastSeenAt.Equal(e.clock.Now()) {
		t.Errorf("after heartbeat: %s last_seen %v", d.Status, d.LastSeenAt)
	}
	if len(d.Events) != before {
		t.Errorf("heartbeat stored %d event rows", len(d.Events)-before)
	}

	e.event(e.tokA, sid1, api.KindPaneClosed, "")
	d = e.detail(sid1)
	if d.Status != api.StatusEnded || d.EndedAt == nil || !d.EndedAt.Equal(e.clock.Now()) {
		t.Errorf("after pane_closed: %s ended_at %v", d.Status, d.EndedAt)
	}
	endedAt := *d.EndedAt
	e.clock.Advance(time.Hour) // ended never turns stale
	if got := status(); got != api.StatusEnded {
		t.Errorf("an hour after pane_closed: %s", got)
	}
	e.event(e.tokA, sid1, api.KindEnded, "")
	if d := e.detail(sid1); !d.EndedAt.Equal(endedAt) {
		t.Errorf("a second end moved ended_at from %v to %v", endedAt, d.EndedAt)
	}
	if got := live(); len(got) != 0 {
		t.Errorf("live=true should exclude ended: %v", got)
	}

	// Any later write clears ended_at: the session was resumed.
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	if d := e.detail(sid1); d.Status != api.StatusLive || d.EndedAt != nil {
		t.Errorf("after re-register: %s ended_at %v", d.Status, d.EndedAt)
	}
	e.event(e.tokA, sid1, api.KindEnded, "")
	e.must(200, "POST", "/v1/sessions/"+sid1+"/report", e.tokA, api.ReportIn{Note: "back"}, nil)
	if d := e.detail(sid1); d.Status != api.StatusLive {
		t.Errorf("report should revive an ended session: %s", d.Status)
	}
	e.event(e.tokA, sid1, api.KindEnded, "")
	e.must(200, "POST", "/v1/sessions/"+sid1+"/title", e.tokA, api.TitleIn{Title: "t"}, nil)
	if d := e.detail(sid1); d.Status != api.StatusLive {
		t.Errorf("title should revive an ended session: %s", d.Status)
	}

	var kinds []string
	for _, ev := range e.detail(sid1).Events {
		kinds = append(kinds, ev.Kind)
	}
	wantKinds := []string{"ended", "ended", "ended", "pane_closed", "state_changed", "state_changed", "registered"}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Errorf("events newest first: %v, want %v", kinds, wantKinds)
	}
}

func TestReconcileScoping(t *testing.T) {
	e := newEnv(t)
	reg := func(tok, id, herdrSession, pane string) {
		e.register(tok, api.SessionUpsert{ID: id, HerdrSession: herdrSession, HerdrPane: pane})
	}
	reg(e.tokA, sid1, "default", "p1") // in the snapshot
	reg(e.tokA, sid2, "default", "p2") // missing: ended
	reg(e.tokA, sid3, "default", "")   // hooks-only: untouched
	reg(e.tokA, sid4, "work", "p4")    // other herdr session: untouched
	reg(e.tokB, sid5, "default", "p5") // other machine: untouched, and a conflict
	reg(e.tokA, sid6, "default", "p6") // already ended: no second event
	e.event(e.tokA, sid6, api.KindPaneClosed, "")

	e.clock.Advance(time.Minute)
	before := map[string]api.SessionDetail{}
	for _, id := range []string{sid3, sid4, sid5, sid6} {
		before[id] = e.detail(id)
	}

	newID := "0a0b0c0d-7777-4000-8000-000000000007"
	var res struct {
		Upserted  int      `json:"upserted"`
		Ended     []string `json:"ended"`
		Conflicts []string `json:"conflicts"`
	}
	e.must(200, "PUT", "/v1/machines/self/herdr-sessions", e.tokA, api.HerdrSessionsPut{
		HerdrSession: "default",
		Sessions: []api.SessionUpsert{
			{ID: sid1, HerdrPane: "p1", TitleHint: "fix login", Source: api.SourceHooks},
			{ID: newID, Agent: "claude", HerdrPane: "p7"},
			{ID: sid5, HerdrPane: "p5"},
		},
	}, &res)
	if res.Upserted != 2 || !reflect.DeepEqual(res.Ended, []string{sid2}) || !reflect.DeepEqual(res.Conflicts, []string{sid5}) {
		t.Errorf("result: %+v", res)
	}

	now := e.clock.Now()
	d1 := e.detail(sid1)
	if d1.Status != api.StatusLive || !d1.LastSeenAt.Equal(now) || d1.Title != "fix login" || d1.TitleSource != "herdr" {
		t.Errorf("listed session: %+v", d1.Session)
	}
	d2 := e.detail(sid2)
	if d2.Status != api.StatusEnded || !d2.EndedAt.Equal(now) {
		t.Errorf("missing session: %s ended_at %v", d2.Status, d2.EndedAt)
	}
	if ev := d2.Events[0]; ev.Kind != api.KindEnded || ev.Source != api.SourcePlugin ||
		string(ev.Payload) != `{"reason":"missing_from_snapshot"}` {
		t.Errorf("missing session's event: %+v payload %s", ev, ev.Payload)
	}
	dn := e.detail(newID)
	if dn.Machine != "tower" || dn.HerdrSession != "default" || dn.Events[0].Source != api.SourcePlugin {
		t.Errorf("new session: %+v events %+v", dn.Session, dn.Events)
	}
	for id, b := range before {
		if a := e.detail(id); !reflect.DeepEqual(a, b) {
			t.Errorf("session %s changed:\nbefore %+v\nafter  %+v", id, b, a)
		}
	}

	// A snapshot with an empty herdr_session is rejected and changes nothing.
	snap := e.snapshot()
	if code, b, _ := e.do("PUT", "/v1/machines/self/herdr-sessions", e.tokA, api.HerdrSessionsPut{}); code != 400 {
		t.Errorf("empty herdr_session: %d %s", code, b)
	}
	if e.snapshot() != snap {
		t.Error("rejected snapshot changed state")
	}

	// An empty snapshot ends every remaining paned session in that herdr
	// session, and still leaves the hooks-only one alone.
	e.must(200, "PUT", "/v1/machines/self/herdr-sessions", e.tokA, api.HerdrSessionsPut{HerdrSession: "default"}, &res)
	if !reflect.DeepEqual(res.Ended, []string{newID, sid1}) && !reflect.DeepEqual(res.Ended, []string{sid1, newID}) {
		t.Errorf("empty snapshot ended %v", res.Ended)
	}
	if d := e.detail(sid3); d.Status == api.StatusEnded {
		t.Error("hooks-only session was ended")
	}
}

// A pane that still runs Claude but has lost its session ID in herdr (seen
// live on a resumed session) keeps the session that hooks registered there.
func TestReconcileKeepsUnidentifiedPanes(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1, HerdrSession: "default", HerdrPane: "w4:p1"})
	e.register(e.tokA, api.SessionUpsert{ID: sid2, HerdrSession: "default", HerdrPane: "w5:p1"})
	e.clock.Advance(time.Minute)

	var res struct {
		Ended []string `json:"ended"`
	}
	e.must(200, "PUT", "/v1/machines/self/herdr-sessions", e.tokA, api.HerdrSessionsPut{
		HerdrSession:      "default",
		UnidentifiedPanes: []string{"w4:p1"},
	}, &res)
	if !reflect.DeepEqual(res.Ended, []string{sid2}) {
		t.Errorf("ended %v, want only %s", res.Ended, sid2)
	}
	if d := e.detail(sid1); d.Status == api.StatusEnded {
		t.Error("session in an unidentified Claude pane was ended")
	}
}

func TestPrefixLookup(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{sid1, sid2, "abcd1234", "abcd12345"} {
		e.register(e.tokA, api.SessionUpsert{ID: id})
	}
	tests := []struct {
		name       string
		id         string
		want       int
		wantID     string
		candidates []string
	}{
		{"too short", "3f2", 400, "", nil},
		{"unique prefix", "3f2a9c10-1", 200, sid1, nil},
		{"full id", sid2, 200, sid2, nil},
		{"ambiguous", "3f2a", 409, "", []string{sid1, sid2}},
		{"no match", "ffff", 404, "", nil},
		{"exact beats prefix", "abcd1234", 200, "abcd1234", nil},
		{"bad characters", "3f2a%25", 400, "", nil},
		{"underscore is literal, not a wildcard", "3f2_", 404, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, b, _ := e.do("GET", "/v1/sessions/"+tt.id, e.tokA, nil)
			if code != tt.want {
				t.Fatalf("status %d, want %d: %s", code, tt.want, b)
			}
			var got struct {
				ID         string   `json:"id"`
				Error      string   `json:"error"`
				Candidates []string `json:"candidates"`
			}
			json.Unmarshal(b, &got)
			if got.ID != tt.wantID {
				t.Errorf("id %q, want %q", got.ID, tt.wantID)
			}
			if !reflect.DeepEqual(got.Candidates, tt.candidates) {
				t.Errorf("candidates %v, want %v", got.Candidates, tt.candidates)
			}
			if code != 200 && got.Error == "" {
				t.Errorf("no error message: %s", b)
			}
		})
	}

	// Write endpoints need the full ID: a prefix is an unknown session.
	snap := e.snapshot()
	for _, p := range []string{"/events", "/report", "/title"} {
		body := map[string]any{"kind": "prompt", "source": "hooks", "title": "x"}
		if code, b, _ := e.do("POST", "/v1/sessions/3f2a9c10-1"+p, e.tokA, body); code != 404 {
			t.Errorf("POST prefix%s: %d %s", p, code, b)
		}
	}
	if e.snapshot() != snap {
		t.Error("writes by prefix changed state")
	}
}

func TestCrossMachineConflict(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1, CWD: "/home/user/a", HerdrPane: "p1"})
	snap := e.snapshot()
	writes := []struct {
		path string
		body any
	}{
		{"/v1/sessions", api.SessionUpsert{ID: sid1, Source: api.SourceHooks, CWD: "/home/user/b"}},
		{"/v1/sessions/" + sid1 + "/events", api.EventIn{Kind: api.KindEnded, Source: api.SourceHooks}},
		{"/v1/sessions/" + sid1 + "/report", api.ReportIn{Done: []string{"x"}}},
		{"/v1/sessions/" + sid1 + "/title", api.TitleIn{Title: "stolen"}},
	}
	for _, w := range writes {
		code, b, _ := e.do("POST", w.path, e.tokB, w.body)
		if code != http.StatusConflict || !strings.Contains(string(b), `\"tower\"`) {
			t.Errorf("bluebox POST %s: %d %s, want 409 naming tower", w.path, code, b)
		}
	}
	if e.snapshot() != snap {
		t.Error("rejected cross-machine writes changed state")
	}
	if d := e.detail(sid1); d.Machine != "tower" {
		t.Errorf("owner changed to %s", d.Machine)
	}
}

func TestTitlePrecedence(t *testing.T) {
	type op struct {
		kind string // prompt | herdr | user
		val  string
	}
	apply := func(e *env, o op) {
		switch o.kind {
		case "prompt":
			e.register(e.tokA, api.SessionUpsert{ID: sid1, FirstPrompt: o.val})
		case "herdr":
			e.register(e.tokA, api.SessionUpsert{ID: sid1, TitleHint: o.val})
		case "user":
			e.must(200, "POST", "/v1/sessions/"+sid1+"/title", e.tokA, api.TitleIn{Title: o.val}, nil)
		}
	}
	starts := map[string]*op{
		"none":   nil,
		"prompt": {"prompt", "P1"},
		"herdr":  {"herdr", "H1"},
		"user":   {"user", "U1"},
	}
	incoming := []op{{"prompt", "P2"}, {"herdr", "H2"}, {"user", "U2"}}
	// want[start][incoming kind] = title/source
	want := map[string]map[string]string{
		"none":   {"prompt": "P2/prompt", "herdr": "H2/herdr", "user": "U2/user"},
		"prompt": {"prompt": "P1/prompt", "herdr": "H2/herdr", "user": "U2/user"},
		"herdr":  {"prompt": "H1/herdr", "herdr": "H2/herdr", "user": "U2/user"},
		"user":   {"prompt": "U1/user", "herdr": "U1/user", "user": "U2/user"},
	}
	for start, first := range starts {
		for _, in := range incoming {
			t.Run(start+"+"+in.kind, func(t *testing.T) {
				e := newEnv(t)
				e.register(e.tokA, api.SessionUpsert{ID: sid1})
				if first != nil {
					apply(e, *first)
				}
				apply(e, in)
				d := e.detail(sid1)
				if got := d.Title + "/" + d.TitleSource; got != want[start][in.kind] {
					t.Errorf("title %s, want %s", got, want[start][in.kind])
				}
			})
		}
	}

	t.Run("hint beats prompt in one upsert", func(t *testing.T) {
		e := newEnv(t)
		s := e.register(e.tokA, api.SessionUpsert{ID: sid1, FirstPrompt: "P", TitleHint: "H"})
		if s.Title != "H" || s.TitleSource != "herdr" {
			t.Errorf("got %s/%s", s.Title, s.TitleSource)
		}
	})
	t.Run("untitled until a title source arrives", func(t *testing.T) {
		e := newEnv(t)
		e.register(e.tokA, api.SessionUpsert{ID: sid1})
		if d := e.detail(sid1); d.Title != "" || d.TitleSource != "" {
			t.Errorf("untitled session has %s/%s", d.Title, d.TitleSource)
		}
	})
}

func TestReportsAndEventsHistory(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	for i := range 3 {
		e.clock.Advance(time.Second)
		e.must(200, "POST", "/v1/sessions/"+sid1+"/report", e.tokA, api.ReportIn{
			Done: []string{fmt.Sprintf("step %d", i)}, InFlight: nil, WaitingOn: []string{}, Note: fmt.Sprint(i)}, nil)
	}
	for i := range 60 {
		e.clock.Advance(time.Second)
		e.event(e.tokA, sid1, api.KindPrompt, fmt.Sprintf(`{"n": %d}`, i))
	}
	d := e.detail(sid1)
	if len(d.Reports) != 3 || d.Reports[0].Note != "2" || d.Reports[2].Note != "0" {
		t.Errorf("reports newest first: %+v", d.Reports)
	}
	if r := d.Reports[0]; r.InFlight == nil || r.WaitingOn == nil || len(r.Done) != 1 {
		t.Errorf("report lists should be non-null: %#v", r)
	}
	if len(d.Events) != 50 {
		t.Fatalf("events: %d, want 50", len(d.Events))
	}
	if string(d.Events[0].Payload) != `{"n":59}` || string(d.Events[49].Payload) != `{"n":10}` {
		t.Errorf("events window: first %s last %s", d.Events[0].Payload, d.Events[49].Payload)
	}
	l := e.list("")
	if len(l) != 1 || l[0].LatestReport == nil || l[0].LatestReport.Note != "2" {
		t.Errorf("list latest_report: %+v", l)
	}
	// An event's own ts is kept; last_seen_at is the server's time.
	old := e.clock.Now().Add(-time.Hour)
	e.must(200, "POST", "/v1/sessions/"+sid1+"/events", e.tokA,
		api.EventIn{Kind: api.KindPrompt, Source: api.SourceHooks, TS: old}, nil)
	d = e.detail(sid1)
	if !d.LastSeenAt.Equal(e.clock.Now()) {
		t.Errorf("last_seen_at %v", d.LastSeenAt)
	}
	found := false
	for _, ev := range d.Events {
		found = found || ev.TS.Equal(old)
	}
	if found {
		t.Error("an hour-old event should sort out of the newest-50 window")
	}
}

func TestListFilters(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	e.clock.Advance(time.Second)
	e.register(e.tokB, api.SessionUpsert{ID: sid2})
	if got := ids(e.list("")); !reflect.DeepEqual(got, []string{sid2, sid1}) {
		t.Errorf("all, most recent first: %v", got)
	}
	if got := ids(e.list("?machine=bluebox")); !reflect.DeepEqual(got, []string{sid2}) {
		t.Errorf("machine=bluebox: %v", got)
	}
	code, b, _ := e.do("GET", "/v1/sessions?machine=nope", e.tokA, nil)
	if code != 200 || strings.TrimSpace(string(b)) != "[]" {
		t.Errorf("unknown machine: %d %s, want 200 []", code, b)
	}
	if s := e.list("?machine=bluebox")[0]; s.ResumeCommand != "ssh -t bluebox.example.com '~/.local/bin/sessionhub resume "+sid2+"'" {
		t.Errorf("bluebox resume_command %q", s.ResumeCommand)
	}
}

func TestRequestValidation(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	long := strings.Repeat("x", 201)
	items21 := make([]string, 21)
	tests := []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{"malformed JSON", "POST", "/v1/sessions", []byte(`{"id":`), 400},
		{"empty body", "POST", "/v1/sessions", []byte(``), 400},
		{"body over 64 KiB", "POST", "/v1/sessions", []byte(`{"id":"` + strings.Repeat("a", MaxBodyBytes) + `"}`), 413},
		{"missing id", "POST", "/v1/sessions", api.SessionUpsert{Source: "hooks"}, 400},
		{"bad id", "POST", "/v1/sessions", api.SessionUpsert{ID: "a b", Source: "hooks"}, 400},
		{"bad source", "POST", "/v1/sessions", api.SessionUpsert{ID: sid2, Source: "cron"}, 400},
		{"bad agent_state", "POST", "/v1/sessions", api.SessionUpsert{ID: sid2, Source: "hooks", AgentState: "sleepy"}, 400},
		{"unknown session event", "POST", "/v1/sessions/" + sid2 + "/events", api.EventIn{Kind: "prompt", Source: "hooks"}, 404},
		{"bad kind", "POST", "/v1/sessions/" + sid1 + "/events", api.EventIn{Kind: "heartbeat", Source: "hooks"}, 400},
		{"registered is server-only", "POST", "/v1/sessions/" + sid1 + "/events", api.EventIn{Kind: "registered", Source: "hooks"}, 400},
		{"state_changed without state", "POST", "/v1/sessions/" + sid1 + "/events", api.EventIn{Kind: "state_changed", Source: "hooks"}, 400},
		{"state_changed bad state", "POST", "/v1/sessions/" + sid1 + "/events",
			api.EventIn{Kind: "state_changed", Source: "hooks", Payload: json.RawMessage(`{"agent_state":"x"}`)}, 400},
		{"state_changed non-object payload", "POST", "/v1/sessions/" + sid1 + "/events",
			api.EventIn{Kind: "state_changed", Source: "hooks", Payload: json.RawMessage(`[1]`)}, 400},
		{"event bad source", "POST", "/v1/sessions/" + sid1 + "/events", api.EventIn{Kind: "prompt", Source: ""}, 400},
		{"bad id in path", "POST", "/v1/sessions/a.b/events", api.EventIn{Kind: "prompt", Source: "hooks"}, 400},
		{"report 21 items", "POST", "/v1/sessions/" + sid1 + "/report", api.ReportIn{InFlight: items21}, 400},
		{"report item 201 chars", "POST", "/v1/sessions/" + sid1 + "/report", api.ReportIn{WaitingOn: []string{long}}, 400},
		{"unknown session report", "POST", "/v1/sessions/" + sid2 + "/report", api.ReportIn{}, 404},
		{"empty title", "POST", "/v1/sessions/" + sid1 + "/title", api.TitleIn{Title: "  "}, 400},
		{"unknown session title", "POST", "/v1/sessions/" + sid2 + "/title", api.TitleIn{Title: "x"}, 404},
		{"bad live filter", "GET", "/v1/sessions?live=maybe", nil, 400},
		{"unknown route", "GET", "/v1/nope", nil, 404},
		{"unknown root path", "GET", "/robots.txt", nil, 404},
		{"wrong method", "DELETE", "/v1/sessions", nil, 405},
		{"wrong method on id", "PUT", "/v1/sessions/" + sid1, nil, 405},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := e.snapshot()
			code, b, hdr := e.do(tt.method, tt.path, e.tokA, tt.body)
			if code != tt.want {
				t.Fatalf("status %d, want %d: %s", code, tt.want, b)
			}
			var ae api.Error
			if err := json.Unmarshal(b, &ae); err != nil || ae.Error == "" {
				t.Errorf("body is not api.Error: %s", b)
			}
			if code == 405 && hdr.Get("Allow") == "" {
				t.Error("405 without Allow")
			}
			if e.snapshot() != snap {
				t.Error("rejected request changed state")
			}
		})
	}

	// The limits themselves are accepted: 20 items of 200 characters,
	// counted as characters, not bytes.
	full := make([]string, 20)
	for i := range full {
		full[i] = strings.Repeat("é", 200)
	}
	var s api.Session
	e.must(200, "POST", "/v1/sessions/"+sid1+"/report", e.tokA, api.ReportIn{Done: full, InFlight: full, WaitingOn: full}, &s)
	if s.LatestReport == nil || len(s.LatestReport.Done) != 20 || s.LatestReport.Done[0] != full[0] {
		t.Errorf("max report not stored intact")
	}
}

func TestRequestLog(t *testing.T) {
	e := newEnv(t)
	e.do("GET", "/healthz?token=secret", "", nil)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	e.doWith("GET", "/v1/sessions", "", e.web, nil)
	e.do("GET", "/v1/sessions", "hub_m_wrong", nil)
	lines := strings.Split(strings.TrimSpace(e.log.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 log lines, got %d:\n%s", len(lines), e.log.String())
	}
	wants := []string{
		`GET "/healthz" 200 `, `POST "/v1/sessions" 201 `, `GET "/v1/sessions" 200 `, `GET "/v1/sessions" 401 `,
	}
	whos := []string{"machine=-", "machine=tower", "machine=web:phone", "machine=-"}
	for i, l := range lines {
		if !strings.HasPrefix(l, wants[i]) || !strings.HasSuffix(l, whos[i]) {
			t.Errorf("log line %d = %q, want prefix %q and suffix %q", i, l, wants[i], whos[i])
		}
	}
	if strings.Contains(e.log.String(), "secret") {
		t.Error("query string leaked into the log")
	}
}

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	code, b, hdr := e.do("GET", "/healthz", "", nil)
	if code != 200 || string(b) != "ok\n" || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") {
		t.Errorf("healthz: %d %q %s", code, b, hdr.Get("Content-Type"))
	}
}

func TestRequestLogEscapesPath(t *testing.T) {
	e := newEnv(t)
	e.do("GET", "/v1/nope%0aGET%20%22/v1/sessions%22%20200%20machine=read", "", nil)
	lines := strings.Split(strings.TrimSpace(e.log.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d:\n%s", len(lines), e.log.String())
	}
	if !strings.HasPrefix(lines[0], `GET "/v1/nope\nGET`) {
		t.Errorf("path not quoted: %q", lines[0])
	}
}
