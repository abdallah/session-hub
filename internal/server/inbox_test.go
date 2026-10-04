package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// triage sends a dismiss or snooze as the dashboard does: the session
// cookie and X-Hub-Action: triage.
func (e *env) triage(kind, id string, body any) (int, []byte) {
	e.t.Helper()
	code, b, _ := e.doHdr("POST", "/v1/inbox/"+id+"/"+kind, "", e.web,
		map[string]string{api.HeaderAction: api.HeaderActionTriage}, body)
	return code, b
}

func (e *env) inbox() api.Inbox {
	e.t.Helper()
	var in api.Inbox
	e.must(http.StatusOK, "GET", "/v1/inbox", e.tokA, nil, &in)
	return in
}

func TestInboxShapeAndOrder(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{sid1, sid2, sid3, sid4, sid5} {
		e.register(e.tokA, api.SessionUpsert{ID: id, CWD: "/home/user/proj"})
	}
	t0 := e.clock.Now()
	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"working"}`)
	e.clock.Advance(time.Minute)
	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"idle"}`) // finished, t0+1m
	e.clock.Advance(time.Minute)
	e.event(e.tokA, sid2, api.KindStateChanged, `{"agent_state":"working"}`)
	e.event(e.tokA, sid2, api.KindStateChanged, `{"agent_state":"done"}`) // finished, t0+2m
	e.clock.Advance(time.Minute)
	e.must(http.StatusOK, "POST", "/v1/sessions/"+sid3+"/report", e.tokA,
		api.ReportIn{WaitingOn: []string{"review MR !12"}}, nil) // waiting, t0+3m
	e.clock.Advance(time.Minute)
	e.event(e.tokA, sid4, api.KindStateChanged, `{"agent_state":"blocked"}`) // blocked, t0+4m
	e.must(http.StatusOK, "POST", "/v1/sessions/"+sid4+"/report", e.tokA,
		api.ReportIn{WaitingOn: []string{"answer the prompt"}}, nil) // also waiting: shown as blocked
	e.event(e.tokA, sid5, api.KindStateChanged, `{"agent_state":"working"}`)
	e.event(e.tokA, sid5, api.KindStateChanged, `{"agent_state":"idle"}`)
	e.event(e.tokA, sid5, api.KindEnded, "") // ended: never in the inbox

	// The session cookie reads the inbox too.
	code, body, _ := e.doWith("GET", "/v1/inbox", "", e.web, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/inbox with cookie: %d %s", code, body)
	}
	var in api.Inbox
	if err := json.Unmarshal(body, &in); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range in.Items {
		got = append(got, it.Group+":"+it.Session.ID)
	}
	want := []string{"blocked:" + sid4, "waiting:" + sid3, "finished:" + sid1, "finished:" + sid2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("items %v, want %v", got, want)
	}
	wantSince := []time.Time{t0.Add(4 * time.Minute), t0.Add(3 * time.Minute), t0.Add(time.Minute), t0.Add(2 * time.Minute)}
	for i, it := range in.Items {
		if !it.Since.Equal(wantSince[i]) {
			t.Errorf("%s since %v, want %v", it.Session.ID, it.Since, wantSince[i])
		}
		if it.Session.Machine != "tower" || it.Session.ResumeCommand == "" || (it.Session.Status != api.StatusLive && it.Session.Status != api.StatusBlocked) {
			t.Errorf("%s session %+v", it.Session.ID, it.Session)
		}
	}
	if in.Counts != (api.InboxCounts{Blocked: 1, Waiting: 1, Finished: 2}) {
		t.Errorf("counts %+v", in.Counts)
	}
	if !reflect.DeepEqual(in.Items[1].WaitingOn, []string{"review MR !12"}) {
		t.Errorf("waiting_on %v", in.Items[1].WaitingOn)
	}

	// The wire shape: waiting_on only on waiting items, counts by name.
	var raw struct {
		Items  []map[string]json.RawMessage `json:"items"`
		Counts map[string]int               `json:"counts"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw.Items[0]["waiting_on"]; ok {
		t.Error("blocked item carries waiting_on")
	}
	for _, k := range []string{"group", "since", "waiting_on", "session"} {
		if _, ok := raw.Items[1][k]; !ok {
			t.Errorf("waiting item lacks %q", k)
		}
	}
	if !reflect.DeepEqual(raw.Counts, map[string]int{"blocked": 1, "waiting": 1, "finished": 2}) {
		t.Errorf("counts %v", raw.Counts)
	}
}

func TestInboxTriageFlow(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"working"}`)
	e.clock.Advance(time.Minute)
	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"idle"}`)
	in := e.inbox()
	if len(in.Items) != 1 {
		t.Fatalf("inbox %+v, want one item", in)
	}
	since := in.Items[0].Since
	before := e.detail(sid1)
	e.clock.Advance(time.Minute)

	if code, b := e.triage("dismiss", sid1, api.TriageIn{Since: since}); code != http.StatusNoContent || len(b) != 0 {
		t.Fatalf("dismiss: %d %s", code, b)
	}
	if got := e.inbox(); len(got.Items) != 0 || got.Counts.Finished != 0 {
		t.Errorf("after dismiss: %+v", got)
	}
	after := e.detail(sid1)
	if !after.LastSeenAt.Equal(before.LastSeenAt) || len(after.Events) != len(before.Events) {
		t.Errorf("dismiss changed the session: last_seen %v → %v, events %d → %d",
			before.LastSeenAt, after.LastSeenAt, len(before.Events), len(after.Events))
	}

	// Another finished turn brings it back with a later since.
	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"working"}`)
	e.clock.Advance(time.Minute)
	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"idle"}`)
	in = e.inbox()
	if len(in.Items) != 1 || !in.Items[0].Since.After(since) {
		t.Fatalf("after a new turn: %+v", in)
	}

	// Snooze for an hour: hidden, then the same item is back, marked stale.
	since = in.Items[0].Since
	until := e.clock.Now().Add(time.Hour)
	if code, b := e.triage("snooze", sid1, api.TriageIn{Since: since, Until: &until}); code != http.StatusNoContent {
		t.Fatalf("snooze: %d %s", code, b)
	}
	if got := e.inbox(); len(got.Items) != 0 {
		t.Errorf("after snooze: %+v", got)
	}
	e.clock.Advance(time.Hour)
	in = e.inbox()
	if len(in.Items) != 1 || !in.Items[0].Since.Equal(since) || in.Items[0].Session.Status != api.StatusStale {
		t.Errorf("after the snooze ended: %+v", in)
	}

	// A new prompt clears it.
	e.event(e.tokA, sid1, api.KindPrompt, `{"prompt":"next step"}`)
	if got := e.inbox(); len(got.Items) != 0 {
		t.Errorf("after a prompt: %+v", got)
	}
}

func TestInboxTriageValidation(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	now := e.clock.Now()
	at := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	cases := []struct {
		name, kind, id string
		body           any
		want           int
	}{
		{"no since", "dismiss", sid1, []byte(`{}`), 400},
		{"since not a time", "dismiss", sid1, []byte(`{"since":"yesterday"}`), 400},
		{"body not JSON", "dismiss", sid1, []byte(`since=now`), 400},
		{"since 6m ahead", "dismiss", sid1, api.TriageIn{Since: now.Add(6 * time.Minute)}, 400},
		{"unknown session", "dismiss", sid6, api.TriageIn{Since: now}, 404},
		{"malformed id", "dismiss", "bad%20id", api.TriageIn{Since: now}, 400},
		{"snooze without until", "snooze", sid1, api.TriageIn{Since: now}, 400},
		{"until now", "snooze", sid1, api.TriageIn{Since: now, Until: at(0)}, 400},
		{"until in the past", "snooze", sid1, api.TriageIn{Since: now, Until: at(-time.Minute)}, 400},
		{"until over 7 days", "snooze", sid1, api.TriageIn{Since: now, Until: at(7*24*time.Hour + time.Second)}, 400},
		{"since 5m ahead", "dismiss", sid1, api.TriageIn{Since: now.Add(5 * time.Minute)}, 204},
		{"dismiss ignores until", "dismiss", sid1, api.TriageIn{Since: now, Until: at(-time.Hour)}, 204},
		{"until 7 days", "snooze", sid1, api.TriageIn{Since: now, Until: at(7 * 24 * time.Hour)}, 204},
	}
	for _, c := range cases {
		before := e.triageRows()
		code, b := e.triage(c.kind, c.id, c.body)
		if code != c.want {
			t.Errorf("%s: status %d, want %d; body %s", c.name, code, c.want, b)
			continue
		}
		if code < 400 {
			continue
		}
		var ae api.Error
		if err := json.Unmarshal(b, &ae); err != nil || ae.Error == "" {
			t.Errorf("%s: error body %q is not api.Error", c.name, b)
		}
		if after := e.triageRows(); after != before {
			t.Errorf("%s: a rejected request changed inbox_triage:\n%s→\n%s", c.name, before, after)
		}
	}
}
