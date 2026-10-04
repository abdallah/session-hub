package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// eventAt posts an event with an explicit ts and returns the session.
func (e *env) eventAt(id, kind, payload string, ts time.Time) api.Session {
	e.t.Helper()
	in := api.EventIn{Kind: kind, Source: api.SourceHooks, TS: ts}
	if payload != "" {
		in.Payload = json.RawMessage(payload)
	}
	var s api.Session
	e.must(200, "POST", "/v1/sessions/"+id+"/events", e.tokA, in, &s)
	return s
}

func TestFutureEventTimestampIsClamped(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	now := e.clock.Now()
	e.eventAt(sid1, api.KindPrompt, "", now.Add(24*time.Hour))
	e.eventAt(sid1, api.KindPrompt, "", now.Add(-time.Minute))

	d := e.detail(sid1)
	for _, ev := range d.Events {
		if ev.TS.After(now) {
			t.Errorf("event ts %v is after the server clock %v", ev.TS, now)
		}
	}
	clamped, past := false, false
	for _, ev := range d.Events {
		clamped = clamped || (ev.Kind == api.KindPrompt && ev.TS.Equal(now))
		past = past || ev.TS.Equal(now.Add(-time.Minute))
	}
	if !clamped {
		t.Error("the future ts should become server now")
	}
	if !past {
		t.Error("a past ts should be kept as sent")
	}
}

func TestStaleStateEventDoesNotOverwrite(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	t0 := e.clock.Now()

	// Start state: no state applied yet.
	if got := e.detail(sid1).AgentState; got != "" {
		t.Fatalf("start state %q", got)
	}
	e.clock.Advance(time.Minute)
	e.eventAt(sid1, api.KindStateChanged, `{"agent_state":"working"}`, t0.Add(30*time.Second))
	if got := e.detail(sid1).AgentState; got != "working" {
		t.Fatalf("after first event: %q", got)
	}
	// A queued event that happened earlier arrives late: stored, not applied.
	e.eventAt(sid1, api.KindStateChanged, `{"agent_state":"blocked"}`, t0.Add(10*time.Second))
	d := e.detail(sid1)
	if d.AgentState != "working" || d.Status != api.StatusLive {
		t.Errorf("stale event applied: state %q status %s", d.AgentState, d.Status)
	}
	rows := 0
	for _, ev := range d.Events {
		if ev.Kind == api.KindStateChanged {
			rows++
		}
	}
	if rows != 2 {
		t.Errorf("state_changed rows = %d, want 2 (both stored)", rows)
	}
	// A newer one still applies, and an equal ts applies too.
	e.eventAt(sid1, api.KindStateChanged, `{"agent_state":"blocked"}`, t0.Add(40*time.Second))
	if got := e.detail(sid1).AgentState; got != "blocked" {
		t.Errorf("newer event: %q", got)
	}
	e.eventAt(sid1, api.KindStateChanged, `{"agent_state":"idle"}`, t0.Add(40*time.Second))
	if got := e.detail(sid1).AgentState; got != "idle" {
		t.Errorf("equal-ts event: %q", got)
	}
	// An upsert sets the state but carries no event time, so it does not
	// stamp state_ts with server time: an event 2 s behind the server clock
	// (client clock skew) still applies.
	e.clock.Advance(time.Minute)
	e.register(e.tokA, api.SessionUpsert{ID: sid1, AgentState: "working"})
	if got := e.detail(sid1).AgentState; got != "working" {
		t.Fatalf("upsert did not set the state: %q", got)
	}
	e.eventAt(sid1, api.KindStateChanged, `{"agent_state":"blocked"}`, e.clock.Now().Add(-2*time.Second))
	if got := e.detail(sid1).AgentState; got != "blocked" {
		t.Errorf("event 2 s behind the server after an upsert was dropped: %q", got)
	}
	// Set the state back for the wrong-machine check below.
	e.eventAt(sid1, api.KindStateChanged, `{"agent_state":"working"}`, e.clock.Now())
	// A wrong-machine caller is refused and changes nothing.
	code, _, _ := e.do("POST", "/v1/sessions/"+sid1+"/events", e.tokB,
		api.EventIn{Kind: api.KindStateChanged, Source: api.SourceHooks, Payload: json.RawMessage(`{"agent_state":"idle"}`)})
	if code != 409 {
		t.Errorf("wrong machine: status %d", code)
	}
	if got := e.detail(sid1).AgentState; got != "working" {
		t.Errorf("wrong-machine event changed state: %q", got)
	}
}
