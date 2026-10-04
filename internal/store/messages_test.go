package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// messageEnv has s-ok (tower, pane, watcher polling), s-nopane (tower, no
// pane), s-ended (tower, pane, ended), and s-bluebox (bluebox, pane, no poll).
func messageEnv(t *testing.T) *controlEnv {
	t.Helper()
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s-ok", "w1:p1")
	e.session(e.tower, "s-nopane", "")
	e.session(e.tower, "s-ended", "w1:p2")
	if err := e.s.AddEvent(ctx, e.tower.ID, "s-ended", api.EventIn{Kind: api.KindEnded, Source: api.SourcePlugin}); err != nil {
		t.Fatal(err)
	}
	e.session(e.bluebox, "s-bluebox", "w2:p1")
	e.poll(e.tower)
	return e
}

func TestSendMessagesTargets(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	res, machines, err := e.s.SendMessages(ctx, []string{"s-ok", "s-nopane", "s-ended", "s-bluebox", "nope", "s-ok", "bad id"},
		"Please rebase.\n\tThanks", "cli on bluebox", "cli on bluebox")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(res))
	for i, r := range res {
		got[i] = r.SessionID + " " + r.State + " " + r.Detail
	}
	want := []string{
		"s-ok queued ",
		"s-nopane refused the session is not in herdr",
		"s-ended refused the session has ended",
		`s-bluebox refused the sessionhub watcher on "bluebox" is offline`,
		"nope refused unknown session",
		"bad id refused invalid session id",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("results:\n got %q\nwant %q", got, want)
	}
	if !ValidMessageID(res[0].ID) || res[1].ID != "" {
		t.Errorf("IDs: queued %q, refused %q", res[0].ID, res[1].ID)
	}
	if !reflect.DeepEqual(machines, []string{"tower"}) {
		t.Errorf("machines %v, want [tower]", machines)
	}
	m, err := e.s.GetMessage(ctx, res[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.SessionID != "s-ok" || m.Machine != "tower" || m.Sender != "cli on bluebox" || m.Text != "Please rebase.\n\tThanks" ||
		m.State != api.MessageQueued || !m.CreatedAt.Equal(e.clock.Now()) {
		t.Errorf("stored %+v", m)
	}
	if n := e.count(`SELECT COUNT(*) FROM messages`); n != 1 {
		t.Errorf("%d rows, want 1: refused targets are not stored", n)
	}
}

func TestSendMessagesValidation(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	many := make([]string, MaxMessageTargets+1)
	for i := range many {
		many[i] = fmt.Sprintf("s-%d", i)
	}
	for name, c := range map[string]struct {
		ids          []string
		text, sender string
	}{
		"no targets":      {nil, "hi", "dashboard"},
		"21 targets":      {many, "hi", "dashboard"},
		"blank text":      {[]string{"s-ok"}, " \n ", "dashboard"},
		"long text":       {[]string{"s-ok"}, strings.Repeat("x", MaxMessageRunes+1), "dashboard"},
		"escape":          {[]string{"s-ok"}, "red \x1b[31m", "dashboard"},
		"carriage return": {[]string{"s-ok"}, "a\rb", "dashboard"},
		"no sender":       {[]string{"s-ok"}, "hi", ""},
	} {
		if _, _, err := e.s.SendMessages(ctx, c.ids, c.text, c.sender, c.sender); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, strings.Repeat("é", MaxMessageRunes), "dashboard", "dashboard"); err != nil {
		t.Errorf("4,000 characters: %v", err)
	}
}

func TestSendMessagesRateLimit(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	ids := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		ids = append(ids, "s-ok") // one distinct target: one message
	}
	for i := 0; i < MessagesPerMinute; i++ {
		if _, _, err := e.s.SendMessages(ctx, ids, fmt.Sprintf("m%d", i), "dashboard", "dashboard"); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		e.clock.Advance(time.Second)
	}
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "one more", "dashboard", "dashboard"); !errors.Is(err, ErrTooMany) {
		t.Fatalf("31st message in a minute: %v, want ErrTooMany", err)
	}
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "other sender", "cli on tower", "cli on tower"); err != nil {
		t.Errorf("another sender: %v", err)
	}
	// The current send's targets count, refused or not: a send of two where
	// the window has one slot left is refused as a whole.
	e.clock.Advance(30 * time.Second) // the first message leaves the window
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok", "s-bluebox"}, "two", "dashboard", "dashboard"); !errors.Is(err, ErrTooMany) {
		t.Errorf("two targets with one slot: %v, want ErrTooMany", err)
	}
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "one", "dashboard", "dashboard"); err != nil {
		t.Errorf("one target with one slot: %v", err)
	}
}

func TestClaimAndFinishMessage(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	res, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "Please rebase.", "dashboard", "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	id := res[0].ID
	if _, ok, err := e.s.ClaimMessage(ctx, e.bluebox.ID); ok || err != nil {
		t.Fatalf("bluebox claimed tower's message: %v %v", ok, err)
	}
	claim, ok, err := e.s.ClaimMessage(ctx, e.tower.ID)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	r := claim.Request
	if r.ID != id || r.Action != api.ActionMessage || r.State != api.ControlClaimed || r.SessionID != "s-ok" ||
		r.Machine != "tower" || r.RequestedBy != "dashboard" || !r.ExpiresAt.Equal(r.CreatedAt.Add(MessageTTL)) {
		t.Errorf("request %+v", r)
	}
	if claim.Text != "From the user via sessionhub (dashboard):\n\nPlease rebase." || claim.Session.ID != "s-ok" || claim.Session.HerdrPane != "w1:p1" {
		t.Errorf("claim text %q session %+v", claim.Text, claim.Session)
	}
	// Offered: not again until MessageRetry passes.
	if _, ok, _ := e.s.ClaimMessage(ctx, e.tower.ID); ok {
		t.Fatal("claimed twice at once")
	}
	m, err := e.s.FinishMessage(ctx, e.tower.ID, id, api.ControlResultIn{State: api.MessageBusy, Detail: "the agent is working"})
	if err != nil || m.State != api.MessageQueued || m.Detail != "the agent is working" {
		t.Fatalf("busy: %+v %v", m, err)
	}
	e.clock.Advance(MessageRetry - time.Second)
	if _, ok, _ := e.s.ClaimMessage(ctx, e.tower.ID); ok {
		t.Fatal("offered again before MessageRetry")
	}
	e.clock.Advance(time.Second)
	if _, ok, _ := e.s.ClaimMessage(ctx, e.tower.ID); !ok {
		t.Fatal("not offered again after MessageRetry")
	}
	if _, err := e.s.FinishMessage(ctx, e.bluebox.ID, id, api.ControlResultIn{State: api.MessageDelivered}); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("bluebox's result: %v, want ErrWrongMachine", err)
	}
	m, err = e.s.FinishMessage(ctx, e.tower.ID, id, api.ControlResultIn{State: api.MessageDelivered})
	if err != nil || m.State != api.MessageDelivered {
		t.Fatalf("delivered: %+v %v", m, err)
	}
	evs := e.events("s-ok", api.KindMessage)
	if len(evs) != 1 || evs[0].Source != api.SourceServer {
		t.Fatalf("message events %+v", evs)
	}
	var p map[string]string
	json.Unmarshal(evs[0].Payload, &p)
	if p["message_id"] != id || p["sender"] != "dashboard" || p["state"] != api.MessageDelivered || p["text"] != "Please rebase." {
		t.Errorf("event payload %v", p)
	}
	if _, err := e.s.FinishMessage(ctx, e.tower.ID, id, api.ControlResultIn{State: api.MessageDelivered}); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("second result: %v, want ErrRequestClosed", err)
	}
	for _, bad := range []api.ControlResultIn{{State: "done"}, {State: api.MessageDelivered, URL: "https://claude.ai/code/session_x"}} {
		if _, err := e.s.FinishMessage(ctx, e.tower.ID, id, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("result %+v: %v, want ErrInvalid", bad, err)
		}
	}
	if _, err := e.s.FinishMessage(ctx, e.tower.ID, "cr_AAAAAAAAAAAAAAAAAAAAAA", api.ControlResultIn{State: api.MessageDelivered}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a control ID: %v, want ErrInvalid", err)
	}
}

func TestFinishMessageRefusedAndFailed(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	for _, state := range []string{api.MessageRefused, api.ControlFailed} {
		res, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "hi "+state, "dashboard", "dashboard")
		if err != nil {
			t.Fatal(err)
		}
		m, err := e.s.FinishMessage(ctx, e.tower.ID, res[0].ID, api.ControlResultIn{State: state, Detail: "pane not found"})
		if err != nil || m.State != api.MessageRefused || m.Detail != "pane not found" {
			t.Errorf("%s: %+v %v, want refused", state, m, err)
		}
	}
	if evs := e.events("s-ok", api.KindMessage); len(evs) != 0 {
		t.Errorf("a refused message added %d events, want 0", len(evs))
	}
}

func TestMessageExpiry(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	res, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "too late", "dashboard", "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	id := res[0].ID
	e.clock.Advance(MessageTTL)
	// A read reports the expiry before any write stores it.
	if m, _ := e.s.GetMessage(ctx, id); m.State != api.MessageExpired {
		t.Errorf("read after TTL: %s, want expired", m.State)
	}
	if _, ok, err := e.s.ClaimMessage(ctx, e.tower.ID); ok || err != nil {
		t.Fatalf("claimed an expired message: %v %v", ok, err)
	}
	if n := e.count(`SELECT COUNT(*) FROM messages WHERE state = 'expired'`); n != 1 {
		t.Errorf("stored expired rows %d, want 1", n)
	}
	if evs := e.events("s-ok", api.KindMessage); len(evs) != 1 {
		t.Errorf("expiry events %d, want 1", len(evs))
	}
	if _, err := e.s.FinishMessage(ctx, e.tower.ID, id, api.ControlResultIn{State: api.MessageDelivered}); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("result after expiry: %v, want ErrRequestClosed", err)
	}
	if _, err := e.s.GetMessage(ctx, "msg_AAAAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown message: %v, want ErrNotFound", err)
	}
}

func TestClaimMessageFromSession(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "Rebase on main.", "session 3f2a9c10", "machine:tower"); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := e.s.ClaimMessage(ctx, e.tower.ID)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if want := "From session 3f2a9c10 via sessionhub (sent on the user's behalf):\n\nRebase on main."; claim.Text != want {
		t.Errorf("claim text %q, want %q", claim.Text, want)
	}
}

func TestClaimMessageOldestFirstPerSession(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	first, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "first", "dashboard", "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Second)
	second, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "second", "dashboard", "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := e.s.ClaimMessage(ctx, e.tower.ID)
	if err != nil || !ok || claim.Request.ID != first[0].ID {
		t.Fatalf("first claim: %+v %v %v, want the older message", claim.Request, ok, err)
	}
	if _, err := e.s.FinishMessage(ctx, e.tower.ID, first[0].ID, api.ControlResultIn{State: api.MessageBusy}); err != nil {
		t.Fatal(err)
	}
	// The older message waits out its retry; the newer one must not jump ahead.
	e.clock.Advance(MessageRetry - time.Second)
	if c, ok, _ := e.s.ClaimMessage(ctx, e.tower.ID); ok {
		t.Fatalf("claimed %s while the older message waits", c.Request.ID)
	}
	e.clock.Advance(time.Second)
	claim, ok, _ = e.s.ClaimMessage(ctx, e.tower.ID)
	if !ok || claim.Request.ID != first[0].ID {
		t.Fatalf("after the retry: %+v %v, want the older message again", claim.Request, ok)
	}
	if _, err := e.s.FinishMessage(ctx, e.tower.ID, first[0].ID, api.ControlResultIn{State: api.MessageDelivered}); err != nil {
		t.Fatal(err)
	}
	claim, ok, _ = e.s.ClaimMessage(ctx, e.tower.ID)
	if !ok || claim.Request.ID != second[0].ID {
		t.Fatalf("after delivery: %+v %v, want the newer message", claim.Request, ok)
	}
}

func TestSendMessagesTrimsText(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	res, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "\n  hi there \n", "dashboard", "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := e.s.GetMessage(ctx, res[0].ID); m.Text != "hi there" {
		t.Errorf("stored %q, want trimmed", m.Text)
	}
	padded := " " + strings.Repeat("x", MaxMessageRunes) + " "
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, padded, "dashboard", "dashboard"); err != nil {
		t.Errorf("4,000 characters plus padding: %v", err)
	}
}

func TestSendMessagesLimitKey(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	// Rotating the display sender under one key shares one budget.
	for i := 0; i < MessagesPerMinute; i++ {
		if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "x", fmt.Sprintf("session %08d", i), "machine:tower"); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "x", "cli on tower", "machine:tower"); !errors.Is(err, ErrTooMany) {
		t.Errorf("another sender name, same key: %v, want ErrTooMany", err)
	}
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "x", "cli on tower", "web:phone"); err != nil {
		t.Errorf("another key: %v", err)
	}
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "x", "dashboard", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty key: %v, want ErrInvalid", err)
	}
}

func TestCheckSessionOwner(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	if err := e.s.CheckSessionOwner(ctx, e.tower.ID, "s-ok"); err != nil {
		t.Errorf("owner: %v", err)
	}
	if err := e.s.CheckSessionOwner(ctx, e.bluebox.ID, "s-ok"); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("other machine: %v, want ErrWrongMachine", err)
	}
	if err := e.s.CheckSessionOwner(ctx, e.tower.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v, want ErrNotFound", err)
	}
}

func TestNextMessageRetry(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	if _, ok, err := e.s.NextMessageRetry(ctx, e.tower.ID); err != nil || ok {
		t.Fatalf("no messages: ok %v err %v", ok, err)
	}
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "hi", "dashboard", "web:phone"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.s.NextMessageRetry(ctx, e.tower.ID); ok {
		t.Error("a message never offered has no retry time")
	}
	if _, ok, err := e.s.ClaimMessage(ctx, e.tower.ID); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if d, ok, _ := e.s.NextMessageRetry(ctx, e.tower.ID); !ok || d != MessageRetry {
		t.Errorf("just offered: %s %v, want %s", d, ok, MessageRetry)
	}
	if _, ok, _ := e.s.NextMessageRetry(ctx, e.bluebox.ID); ok {
		t.Error("another machine sees the retry")
	}
	e.clock.Advance(MessageRetry - 200*time.Millisecond)
	if d, _, _ := e.s.NextMessageRetry(ctx, e.tower.ID); d != time.Second {
		t.Errorf("almost due: %s, want the 1 s floor", d)
	}
	e.clock.Advance(MessageTTL)
	if _, ok, _ := e.s.NextMessageRetry(ctx, e.tower.ID); ok {
		t.Error("an expired message still has a retry time")
	}
}
