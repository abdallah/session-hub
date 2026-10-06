package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
)

// The server cleans and caps the blocked-on text, whatever the client sent.
func TestModBlockedOn(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	raw := "Question:\tWhich\n\x1b[2Jlibrary‮? " + strings.Repeat("x", 400)
	var s api.Session
	e.must(http.StatusOK, "POST", "/v1/sessions/"+sid1+"/blocked-on", e.tokA, api.BlockedOnIn{Text: raw}, &s)
	if !strings.HasPrefix(s.BlockedOn, "Question: Which [2Jlibrary? xxx") || !strings.HasSuffix(s.BlockedOn, "…") ||
		utf8.RuneCountInString(s.BlockedOn) != api.MaxBlockedOnRunes || s.AgentState != "blocked" || s.Status != api.StatusBlocked {
		t.Errorf("after set: %q (%d runes) state %q status %q", s.BlockedOn, utf8.RuneCountInString(s.BlockedOn), s.AgentState, s.Status)
	}
	var inbox api.Inbox
	e.must(http.StatusOK, "GET", "/v1/inbox", e.tokB, nil, &inbox)
	if len(inbox.Items) != 1 || inbox.Items[0].Group != api.InboxBlocked || inbox.Items[0].Session.BlockedOn != s.BlockedOn {
		t.Errorf("inbox: %+v", inbox.Items)
	}

	before := e.snapshot()
	for _, c := range []struct {
		token, id string
		body      any
		want      int
	}{
		{e.tokB, sid1, api.BlockedOnIn{Text: "x"}, http.StatusConflict},
		{e.tokA, sid2, api.BlockedOnIn{Text: "x"}, http.StatusNotFound},
		{e.tokA, "-x", api.BlockedOnIn{Text: "x"}, http.StatusBadRequest},
		{e.tokA, sid1, "not an object", http.StatusBadRequest},
		{e.tokA, sid1, api.BlockedOnIn{Text: "x", Clears: "y"}, http.StatusBadRequest},
	} {
		if code, b, _ := e.do("POST", "/v1/sessions/"+c.id+"/blocked-on", c.token, c.body); code != c.want {
			t.Errorf("%s blocked-on: %d %s, want %d", c.id, code, b, c.want)
		}
	}
	if after := e.snapshot(); after != before {
		t.Errorf("refused requests changed state:\nbefore %s\nafter  %s", before, after)
	}

	// A clear for another question changes nothing.
	var stale api.Session
	e.must(http.StatusOK, "POST", "/v1/sessions/"+sid1+"/blocked-on", e.tokA, api.BlockedOnIn{Clears: "Question: other?"}, &stale)
	if stale.BlockedOn != s.BlockedOn || stale.AgentState != "blocked" {
		t.Errorf("after a stale clear: %q %q", stale.BlockedOn, stale.AgentState)
	}
	// The clear names the question as the client sent it; the server cleans
	// it the same way, so it matches.
	var cleared api.Session
	e.must(http.StatusOK, "POST", "/v1/sessions/"+sid1+"/blocked-on", e.tokA, api.BlockedOnIn{Clears: raw}, &cleared)
	if cleared.BlockedOn != "" || cleared.AgentState != "working" {
		t.Errorf("after clear: %q %q", cleared.BlockedOn, cleared.AgentState)
	}
	// An older mod's clear, {"text": ""}, clears only the mod's own block.
	e.must(http.StatusOK, "POST", "/v1/sessions/"+sid1+"/blocked-on", e.tokA, api.BlockedOnIn{Text: "Question: again?"}, nil)
	e.must(http.StatusOK, "POST", "/v1/sessions/"+sid1+"/blocked-on", e.tokA, api.BlockedOnIn{Text: ""}, &cleared)
	if cleared.BlockedOn != "" || cleared.AgentState != "working" {
		t.Errorf("after the legacy clear: %q %q", cleared.BlockedOn, cleared.AgentState)
	}

	// A late report for an ended session answers 200 and changes nothing.
	e.must(http.StatusOK, "POST", "/v1/sessions/"+sid1+"/events", e.tokA, api.EventIn{Kind: api.KindEnded, Source: api.SourceHooks}, nil)
	before = e.snapshot()
	var ended api.Session
	e.must(http.StatusOK, "POST", "/v1/sessions/"+sid1+"/blocked-on", e.tokA, api.BlockedOnIn{Text: "late"}, &ended)
	e.must(http.StatusOK, "PUT", "/v1/sessions/"+sid1+"/usage", e.tokA, json.RawMessage(`{"context_percent": 9}`), nil)
	if ended.Status != api.StatusEnded || ended.BlockedOn != "" {
		t.Errorf("ended session: %+v", ended)
	}
	if after := e.snapshot(); after != before {
		t.Errorf("a report for an ended session changed state:\nbefore %s\nafter  %s", before, after)
	}
}

func TestModUsage(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	var s api.Session
	e.must(http.StatusOK, "PUT", "/v1/sessions/"+sid1+"/usage", e.tokA,
		json.RawMessage(`{"context_percent": 63, "cost_usd": 2.5}`), &s)
	if s.ContextPercent == nil || *s.ContextPercent != 63 || s.LiveCostUSD == nil || *s.LiveCostUSD != 2.5 ||
		s.UsageAt == nil || !s.UsageAt.Equal(e.clock.Now()) {
		t.Errorf("usage: %v %v %v", s.ContextPercent, s.LiveCostUSD, s.UsageAt)
	}
	before := e.snapshot()
	for _, c := range []struct {
		token string
		body  string
		want  int
	}{
		{e.tokB, `{"context_percent": 1}`, http.StatusConflict},
		{e.tokA, `{}`, http.StatusBadRequest},
		{e.tokA, `{"context_percent": 101}`, http.StatusBadRequest},
		{e.tokA, `{"context_percent": 1.5}`, http.StatusBadRequest},
		{e.tokA, `{"cost_usd": -1}`, http.StatusBadRequest},
	} {
		if code, b, _ := e.do("PUT", "/v1/sessions/"+sid1+"/usage", c.token, json.RawMessage(c.body)); code != c.want {
			t.Errorf("usage %s: %d %s, want %d", c.body, code, b, c.want)
		}
	}
	if after := e.snapshot(); after != before {
		t.Errorf("refused requests changed state:\nbefore %s\nafter  %s", before, after)
	}
}

// A mod's poll makes a session outside herdr messageable, wakes on a send,
// and hands out the prompt; the result closes the message.
func TestModMessagePoll(t *testing.T) {
	e := newEnv(t)
	ft := &fakeTimers{}
	e.server.after = ft.after
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	next := "/v1/sessions/" + sid1 + "/messages/next"

	// Another machine's poll is refused before it records anything.
	before := e.snapshot()
	if code, b, _ := e.do("GET", next+"?wait=25", e.tokB, nil); code != http.StatusConflict {
		t.Errorf("bluebox's poll: %d %s", code, b)
	}
	if code, b, _ := e.do("HEAD", next, e.tokA, nil); code != http.StatusMethodNotAllowed {
		t.Errorf("HEAD: %d %s", code, b)
	}
	if code, b, _ := e.do("GET", next+"?wait=soon", e.tokA, nil); code != http.StatusBadRequest {
		t.Errorf("wait=soon: %d %s", code, b)
	}
	if after := e.snapshot(); after != before {
		t.Errorf("refused polls changed state:\nbefore %s\nafter  %s", before, after)
	}

	poll := e.getAsync(e.tokA, next+"?wait=25")
	ft.await(t, 1)
	if ft.wait(0) != 25*time.Second {
		t.Errorf("poll waits %s, want 25s", ft.wait(0))
	}
	if d := e.detail(sid1); !d.Messageable || d.Controllable {
		t.Errorf("during a mod poll: messageable %v controllable %v", d.Messageable, d.Controllable)
	}
	var out api.MessagesOut
	e.must(http.StatusOK, "POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "Rebase, please."}, &out)
	if out.Results[0].State != api.MessageQueued {
		t.Fatalf("send: %+v", out)
	}
	r := recv(t, poll)
	var m api.ModMessage
	if r.code != http.StatusOK || json.Unmarshal(r.body, &m) != nil {
		t.Fatalf("poll: %d %s", r.code, r.body)
	}
	if m.ID != out.Results[0].ID || m.Text != api.MessagePrompt("cli on bluebox", "Rebase, please.") {
		t.Errorf("claim: %+v", m)
	}

	// Results: bluebox may not answer for tower's session; a busy keeps it
	// queued with its detail cleaned; delivered closes it.
	result := "/v1/messages/" + m.ID + "/result"
	if code, b, _ := e.do("POST", result, e.tokB, api.MessageResultIn{State: api.MessageDelivered}); code != http.StatusConflict {
		t.Errorf("bluebox's result: %d %s", code, b)
	}
	if code, b, _ := e.do("POST", result, e.tokA, api.MessageResultIn{State: api.ControlFailed}); code != http.StatusBadRequest {
		t.Errorf("failed result: %d %s", code, b)
	}
	if code, b, _ := e.do("POST", "/v1/messages/msg_nope/result", e.tokA, api.MessageResultIn{State: api.MessageDelivered}); code != http.StatusBadRequest {
		t.Errorf("bad id: %d %s", code, b)
	}
	var got api.Message
	e.must(http.StatusOK, "POST", result, e.tokA, api.MessageResultIn{State: api.MessageBusy, Detail: "agent\nis\x1b busy"}, &got)
	if got.State != api.MessageQueued || got.Detail != "agent is busy" {
		t.Errorf("busy: %+v", got)
	}
	e.must(http.StatusOK, "POST", result, e.tokA, api.MessageResultIn{State: api.MessageDelivered}, &got)
	if got.State != api.MessageDelivered {
		t.Errorf("delivered: %+v", got)
	}

	// With nothing queued, the poll ends with 204 when its timer fires; the
	// wait is capped at 30 seconds.
	poll = e.getAsync(e.tokA, next+"?wait=600")
	ft.await(t, 2)
	if ft.wait(1) != 30*time.Second {
		t.Errorf("wait=600 waits %s, want 30s", ft.wait(1))
	}
	ft.fire(1)
	if r := recv(t, poll); r.code != http.StatusNoContent || len(r.body) != 0 {
		t.Errorf("empty poll: %d %q", r.code, r.body)
	}
}

// A message the mod was handed but never answered is offered again after
// MessageRetry: the poll wakes for it instead of waiting out its timer.
func TestModMessagePollRetry(t *testing.T) {
	e := newEnv(t)
	ft := &fakeTimers{}
	e.server.after = ft.after
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	next := "/v1/sessions/" + sid1 + "/messages/next?wait=25"
	// The first poll records the mod, so the send is accepted.
	poll := e.getAsync(e.tokA, next)
	ft.await(t, 1)
	var out api.MessagesOut
	e.must(http.StatusOK, "POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "hi"}, &out)
	if r := recv(t, poll); r.code != http.StatusOK {
		t.Fatalf("first poll: %d %s", r.code, r.body)
	}
	poll = e.getAsync(e.tokA, next)
	ft.await(t, 3) // the poll's timer and the retry timer
	if ft.wait(2) != 15*time.Second {
		t.Errorf("retry waits %s, want 15s", ft.wait(2))
	}
	e.clock.Advance(15 * time.Second)
	ft.fire(2)
	r := recv(t, poll)
	var m api.ModMessage
	if r.code != http.StatusOK || json.Unmarshal(r.body, &m) != nil || m.ID != out.Results[0].ID {
		t.Errorf("re-offer: %d %s", r.code, r.body)
	}
}
