package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestMigrateV11ToV12(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	tok, _, err := s.AddMachine(ctx, "tower", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.MachineByToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertSession(ctx, m.ID, api.SessionUpsert{ID: "s1", Source: api.SourceHooks, AgentState: "idle"}); err != nil {
		t.Fatal(err)
	}
	rollbackV12(t, s)
	if _, err := s.db.Exec("PRAGMA user_version = 11"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var v int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version %d %v, want %d", v, err, schemaVersion)
	}
	x, err := s2.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if x.BlockedOn != "" || x.ContextPercent != nil || x.LiveCostUSD != nil || x.UsageAt != nil || x.Messageable {
		t.Errorf("session after upgrade: %+v", x)
	}
	if err := s2.SetBlockedOn(ctx, m.ID, "s1", "Question: which?"); err != nil {
		t.Errorf("SetBlockedOn after upgrade: %v", err)
	}
}

func TestSetBlockedOn(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	if err := e.s.AddEvent(ctx, e.tower.ID, "s1", api.EventIn{Kind: api.KindStateChanged, Source: api.SourceHooks,
		Payload: json.RawMessage(`{"agent_state":"working"}`)}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	const q = "Question: Which library should we use? (date-fns, luxon, dayjs)"
	if err := e.s.SetBlockedOn(ctx, e.tower.ID, "s1", "  "+q+" "); err != nil {
		t.Fatal(err)
	}
	x, err := e.s.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if x.BlockedOn != q || x.AgentState != "blocked" || x.Status != api.StatusBlocked || !x.LastSeenAt.Equal(e.clock.Now()) {
		t.Errorf("after set: blocked_on %q state %q status %q seen %v", x.BlockedOn, x.AgentState, x.Status, x.LastSeenAt)
	}
	inbox, err := e.s.Inbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox.Items) != 1 || inbox.Items[0].Group != api.InboxBlocked || inbox.Items[0].Session.BlockedOn != q ||
		!inbox.Items[0].Since.Equal(e.clock.Now()) {
		t.Errorf("inbox after set: %+v", inbox.Items)
	}
	d, err := e.s.SessionDetail(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if ev := d.Events[0]; ev.Source != api.SourceMod || ev.Kind != api.KindBlockedOn || !strings.Contains(string(ev.Payload), "date-fns") {
		t.Errorf("event %+v %s", ev, ev.Payload)
	}

	// An empty text clears it and moves the state from blocked to working.
	e.clock.Advance(time.Minute)
	if err := e.s.SetBlockedOn(ctx, e.tower.ID, "s1", ""); err != nil {
		t.Fatal(err)
	}
	if x, _ = e.s.GetSession(ctx, "s1"); x.BlockedOn != "" || x.AgentState != "working" {
		t.Errorf("after clear: blocked_on %q state %q", x.BlockedOn, x.AgentState)
	}

	// A clear when the block is not the mod's (a permission prompt, with no
	// stored blocked_on) leaves the state alone: a replayed clear must not
	// undo it.
	if err := e.s.AddEvent(ctx, e.tower.ID, "s1", api.EventIn{Kind: api.KindStateChanged, Source: api.SourceHooks,
		Payload: json.RawMessage(`{"agent_state":"blocked"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetBlockedOn(ctx, e.tower.ID, "s1", ""); err != nil {
		t.Fatal(err)
	}
	if x, _ = e.s.GetSession(ctx, "s1"); x.AgentState != "blocked" {
		t.Errorf("a clear undid a permission block: %q", x.AgentState)
	}

	// A clear after the turn ended leaves the state alone.
	if err := e.s.AddEvent(ctx, e.tower.ID, "s1", api.EventIn{Kind: api.KindStateChanged, Source: api.SourceHooks,
		Payload: json.RawMessage(`{"agent_state":"idle"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetBlockedOn(ctx, e.tower.ID, "s1", ""); err != nil {
		t.Fatal(err)
	}
	if x, _ = e.s.GetSession(ctx, "s1"); x.AgentState != "idle" {
		t.Errorf("a late clear moved the state to %q", x.AgentState)
	}

	// Refusals change nothing.
	for _, c := range []struct {
		name    string
		machine int64
		id      string
		text    string
		want    error
	}{
		{"other machine", e.bluebox.ID, "s1", "x", ErrWrongMachine},
		{"unknown session", e.tower.ID, "nope", "x", ErrNotFound},
		{"too long", e.tower.ID, "s1", strings.Repeat("é", api.MaxBlockedOnRunes+1), ErrInvalid},
		{"control character", e.tower.ID, "s1", "a\x1b[2Jb", ErrInvalid},
	} {
		if err := e.s.SetBlockedOn(ctx, c.machine, c.id, c.text); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if x, _ = e.s.GetSession(ctx, "s1"); x.BlockedOn != "" || x.AgentState != "idle" {
		t.Errorf("a refused call wrote: %q %q", x.BlockedOn, x.AgentState)
	}
	if err := e.s.SetBlockedOn(ctx, e.tower.ID, "s1", strings.Repeat("é", api.MaxBlockedOnRunes)); err != nil {
		t.Errorf("300 runes: %v", err)
	}
}

// A state change out of blocked, from an event or an upsert, clears
// blocked_on; a blocked one keeps it.
func TestBlockedOnClearedByStateChange(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	set := func() {
		t.Helper()
		e.clock.Advance(time.Second)
		if err := e.s.SetBlockedOn(ctx, e.tower.ID, "s1", "Question: ok?"); err != nil {
			t.Fatal(err)
		}
	}
	event := func(state string) {
		t.Helper()
		e.clock.Advance(time.Second)
		if err := e.s.AddEvent(ctx, e.tower.ID, "s1", api.EventIn{Kind: api.KindStateChanged, Source: api.SourceHooks,
			TS: e.clock.Now(), Payload: json.RawMessage(`{"agent_state":"` + state + `"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	blockedOn := func() string {
		t.Helper()
		x, err := e.s.GetSession(ctx, "s1")
		if err != nil {
			t.Fatal(err)
		}
		return x.BlockedOn
	}
	set()
	event("blocked") // a permission prompt on top keeps the question
	if got := blockedOn(); got != "Question: ok?" {
		t.Errorf("after a blocked event: %q", got)
	}
	event("working")
	if got := blockedOn(); got != "" {
		t.Errorf("after a working event: %q", got)
	}
	set()
	event("idle")
	if got := blockedOn(); got != "" {
		t.Errorf("after an idle event: %q", got)
	}
	set()
	if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: "s1", Source: api.SourceHooks, AgentState: "working"}); err != nil {
		t.Fatal(err)
	}
	if got := blockedOn(); got != "" {
		t.Errorf("after a working upsert: %q", got)
	}
	set()
	if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: "s1", Source: api.SourceHooks}); err != nil {
		t.Fatal(err)
	}
	if got := blockedOn(); got == "" {
		t.Error("an upsert without a state cleared blocked_on")
	}
}

// setState applies a hooks state change to session s1.
func setState(t *testing.T, e *controlEnv, state string) {
	t.Helper()
	if err := e.s.AddEvent(context.Background(), e.tower.ID, "s1", api.EventIn{Kind: api.KindStateChanged, Source: api.SourceHooks,
		Payload: json.RawMessage(`{"agent_state":"` + state + `"}`)}); err != nil {
		t.Fatal(err)
	}
}

// A question posted after the turn ended (the Stop hook already set idle or
// done) changes nothing, and the chained clear that follows it changes
// nothing either: the finished session stays finished.
func TestLateBlockedOnAfterTurnEnded(t *testing.T) {
	const q = "Question: Ship it? (yes, no)"
	for _, state := range []string{"idle", "done"} {
		t.Run(state, func(t *testing.T) {
			e := newControlEnv(t)
			ctx := context.Background()
			e.session(e.tower, "s1", "")
			setState(t, e, state)
			before, err := e.s.SessionDetail(ctx, "s1")
			if err != nil {
				t.Fatal(err)
			}
			e.clock.Advance(time.Second)
			if err := e.s.SetBlockedOn(ctx, e.tower.ID, "s1", q); err != nil {
				t.Fatal(err)
			}
			if err := e.s.ClearBlockedOn(ctx, e.tower.ID, "s1", q); err != nil {
				t.Fatal(err)
			}
			after, err := e.s.SessionDetail(ctx, "s1")
			if err != nil {
				t.Fatal(err)
			}
			if after.AgentState != state || after.BlockedOn != "" || len(after.Events) != len(before.Events) {
				t.Errorf("state %q blocked_on %q events %d (was %d)", after.AgentState, after.BlockedOn, len(after.Events), len(before.Events))
			}
			inbox, err := e.s.Inbox(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, it := range inbox.Items {
				if it.Group == api.InboxBlocked {
					t.Errorf("blocked inbox item: %+v", it)
				}
			}
		})
	}
}

// A clear names the question it clears. A stale one, for a question that a
// newer one replaced, changes nothing; the matching one clears.
func TestStaleClearAfterNewerQuestion(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	setState(t, e, "working")
	const q1, q2 = "Question: First?", "Question: Second?"
	get := func() api.Session {
		t.Helper()
		x, err := e.s.GetSession(ctx, "s1")
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	if err := e.s.SetBlockedOn(ctx, e.tower.ID, "s1", q1); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetBlockedOn(ctx, e.tower.ID, "s1", q2); err != nil {
		t.Fatal(err)
	}
	// Q1's clear, replayed late from the queue.
	if err := e.s.ClearBlockedOn(ctx, e.tower.ID, "s1", q1); err != nil {
		t.Fatal(err)
	}
	if x := get(); x.BlockedOn != q2 || x.AgentState != "blocked" {
		t.Errorf("after the stale clear: blocked_on %q state %q", x.BlockedOn, x.AgentState)
	}
	if err := e.s.ClearBlockedOn(ctx, e.tower.ID, "s1", "  "+q2+" "); err != nil {
		t.Fatal(err)
	}
	if x := get(); x.BlockedOn != "" || x.AgentState != "working" {
		t.Errorf("after the clear: blocked_on %q state %q", x.BlockedOn, x.AgentState)
	}
	// Once cleared, the same clear again is a no-op.
	setState(t, e, "idle")
	if err := e.s.ClearBlockedOn(ctx, e.tower.ID, "s1", q2); err != nil {
		t.Fatal(err)
	}
	if x := get(); x.AgentState != "idle" {
		t.Errorf("a repeated clear moved idle to %q", x.AgentState)
	}
	if err := e.s.ClearBlockedOn(ctx, e.tower.ID, "s1", " "); err == nil {
		t.Error("an empty clears was accepted")
	}
}

func TestSetUsage(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	pct, cost := 42, 1.25
	if err := e.s.SetUsage(ctx, e.tower.ID, "s1", api.UsageIn{ContextPercent: &pct, CostUSD: &cost}); err != nil {
		t.Fatal(err)
	}
	x, _ := e.s.GetSession(ctx, "s1")
	if x.ContextPercent == nil || *x.ContextPercent != 42 || x.LiveCostUSD == nil || *x.LiveCostUSD != 1.25 ||
		x.UsageAt == nil || !x.UsageAt.Equal(e.clock.Now()) {
		t.Errorf("usage %v %v %v", x.ContextPercent, x.LiveCostUSD, x.UsageAt)
	}
	// A nil field keeps the stored value.
	e.clock.Advance(time.Minute)
	pct = 50
	if err := e.s.SetUsage(ctx, e.tower.ID, "s1", api.UsageIn{ContextPercent: &pct}); err != nil {
		t.Fatal(err)
	}
	x, _ = e.s.GetSession(ctx, "s1")
	if *x.ContextPercent != 50 || *x.LiveCostUSD != 1.25 || !x.UsageAt.Equal(e.clock.Now()) {
		t.Errorf("partial update: %v %v %v", *x.ContextPercent, *x.LiveCostUSD, x.UsageAt)
	}
	bad := func(p int) *int { return &p }
	badF := func(f float64) *float64 { return &f }
	for _, c := range []struct {
		name    string
		machine int64
		in      api.UsageIn
		want    error
	}{
		{"empty", e.tower.ID, api.UsageIn{}, ErrInvalid},
		{"percent below 0", e.tower.ID, api.UsageIn{ContextPercent: bad(-1)}, ErrInvalid},
		{"percent above 100", e.tower.ID, api.UsageIn{ContextPercent: bad(101)}, ErrInvalid},
		{"negative cost", e.tower.ID, api.UsageIn{CostUSD: badF(-0.01)}, ErrInvalid},
		{"NaN cost", e.tower.ID, api.UsageIn{CostUSD: badF(math.NaN())}, ErrInvalid},
		{"huge cost", e.tower.ID, api.UsageIn{CostUSD: badF(math.Inf(1))}, ErrInvalid},
		{"other machine", e.bluebox.ID, api.UsageIn{ContextPercent: bad(1)}, ErrWrongMachine},
	} {
		if err := e.s.SetUsage(ctx, c.machine, "s1", c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if x, _ = e.s.GetSession(ctx, "s1"); *x.ContextPercent != 50 || *x.LiveCostUSD != 1.25 {
		t.Errorf("a refused call wrote: %v %v", *x.ContextPercent, *x.LiveCostUSD)
	}
}

// A session without herdr becomes messageable while its mod polls, gets its
// messages from the mod in order, and the watcher leaves them alone.
func TestModMessages(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s-mod", "")
	e.session(e.tower, "s-pane", "w1:p1")
	e.poll(e.tower)

	if x, _ := e.s.GetSession(ctx, "s-mod"); x.Messageable || x.Controllable {
		t.Errorf("before a mod poll: messageable %v controllable %v", x.Messageable, x.Controllable)
	}
	if x, _ := e.s.GetSession(ctx, "s-pane"); !x.Messageable || !x.Controllable {
		t.Errorf("herdr session: messageable %v controllable %v", x.Messageable, x.Controllable)
	}
	res, _, err := e.s.SendMessages(ctx, []string{"s-mod"}, "hi", "cli on tower", "machine:tower")
	if err != nil || res[0].State != api.MessageRefused {
		t.Fatalf("send before a mod poll: %+v %v", res, err)
	}

	// A poll from another machine is refused and records nothing.
	if _, _, err := e.s.ClaimSessionMessage(ctx, e.bluebox.ID, "s-mod"); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("other machine: %v", err)
	}
	if err := e.s.RecordModPoll(ctx, e.bluebox.ID, "s-mod"); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("RecordModPoll from another machine: %v", err)
	}
	if x, _ := e.s.GetSession(ctx, "s-mod"); x.Messageable {
		t.Error("a refused poll made the session messageable")
	}

	if _, ok, err := e.s.ClaimSessionMessage(ctx, e.tower.ID, "s-mod"); err != nil || ok {
		t.Fatalf("empty poll: %v %v", ok, err)
	}
	x, _ := e.s.GetSession(ctx, "s-mod")
	if !x.Messageable || x.Controllable {
		t.Errorf("after a mod poll: messageable %v controllable %v", x.Messageable, x.Controllable)
	}
	res, machines, err := e.s.SendMessages(ctx, []string{"s-mod"}, "first", "cli on tower", "machine:tower")
	if err != nil || res[0].State != api.MessageQueued || len(machines) != 1 {
		t.Fatalf("send to a mod session: %+v %v %v", res, machines, err)
	}
	e.clock.Advance(time.Second)
	res2, _, err := e.s.SendMessages(ctx, []string{"s-mod"}, "second", "session abcdefgh", "machine:tower")
	if err != nil || res2[0].State != api.MessageQueued {
		t.Fatalf("second send: %+v %v", res2, err)
	}

	// The watcher skips the session while its mod is fresh.
	if _, ok, err := e.s.ClaimMessage(ctx, e.tower.ID); err != nil || ok {
		t.Errorf("watcher claimed a mod session's message: %v %v", ok, err)
	}
	if _, ok, err := e.s.NextMessageRetry(ctx, e.tower.ID); err != nil || ok {
		t.Errorf("watcher retry for a mod session: %v %v", ok, err)
	}

	m, ok, err := e.s.ClaimSessionMessage(ctx, e.tower.ID, "s-mod")
	if err != nil || !ok || m.ID != res[0].ID || m.Text != api.MessagePrompt("cli on tower", "first") {
		t.Fatalf("mod claim: %+v %v %v", m, ok, err)
	}
	// The older message is offered again only after MessageRetry; the newer
	// one waits behind it.
	if _, ok, _ := e.s.ClaimSessionMessage(ctx, e.tower.ID, "s-mod"); ok {
		t.Error("claimed again inside MessageRetry, or out of order")
	}
	if d, ok, err := e.s.NextSessionMessageRetry(ctx, e.tower.ID, "s-mod"); err != nil || !ok || d != MessageRetry {
		t.Errorf("retry %v %v %v, want %v", d, ok, err, MessageRetry)
	}
	if _, err := e.s.FinishModMessage(ctx, e.tower.ID, m.ID, api.MessageResultIn{State: api.MessageBusy}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(MessageRetry)
	again, ok, err := e.s.ClaimSessionMessage(ctx, e.tower.ID, "s-mod")
	if err != nil || !ok || again.ID != m.ID {
		t.Fatalf("re-offer after busy: %+v %v %v", again, ok, err)
	}
	for _, c := range []struct {
		machine int64
		state   string
		want    error
	}{
		{e.bluebox.ID, api.MessageDelivered, ErrWrongMachine},
		{e.tower.ID, api.ControlFailed, ErrInvalid},
		{e.tower.ID, "", ErrInvalid},
	} {
		if _, err := e.s.FinishModMessage(ctx, c.machine, m.ID, api.MessageResultIn{State: c.state}); !errors.Is(err, c.want) {
			t.Errorf("finish %q from %d: %v, want %v", c.state, c.machine, err, c.want)
		}
	}
	got, err := e.s.FinishModMessage(ctx, e.tower.ID, m.ID, api.MessageResultIn{State: api.MessageDelivered})
	if err != nil || got.State != api.MessageDelivered {
		t.Fatalf("delivered: %+v %v", got, err)
	}
	if _, err := e.s.FinishModMessage(ctx, e.tower.ID, m.ID, api.MessageResultIn{State: api.MessageDelivered}); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("second result: %v", err)
	}
	next, ok, err := e.s.ClaimSessionMessage(ctx, e.tower.ID, "s-mod")
	if err != nil || !ok || next.ID != res2[0].ID || !strings.HasPrefix(next.Text, "From session abcdefgh via sessionhub") {
		t.Fatalf("second message: %+v %v %v", next, ok, err)
	}

	// Once the mod stops polling, the session is messageable only through
	// herdr, and the watcher takes a herdr session's messages again.
	e.clock.Advance(ControlPollWindow + time.Second)
	e.poll(e.tower)
	if x, _ := e.s.GetSession(ctx, "s-mod"); x.Messageable {
		t.Error("still messageable after the mod went quiet")
	}
	res3, _, err := e.s.SendMessages(ctx, []string{"s-mod", "s-pane"}, "third", "cli on tower", "machine:tower")
	if err != nil || res3[0].State != api.MessageRefused || res3[1].State != api.MessageQueued {
		t.Fatalf("send after the mod went quiet: %+v %v", res3, err)
	}
	if c, ok, err := e.s.ClaimMessage(ctx, e.tower.ID); err != nil || !ok || c.Request.ID != res3[1].ID {
		t.Errorf("watcher claim of the herdr session: %+v %v %v", c.Request, ok, err)
	}
}

// A herdr session with a fresh mod gets its messages from the mod only.
func TestWatcherSkipsModSession(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	e.poll(e.tower)
	if err := e.s.RecordModPoll(ctx, e.tower.ID, "s1"); err != nil {
		t.Fatal(err)
	}
	res, _, err := e.s.SendMessages(ctx, []string{"s1"}, "hi", "dashboard", "web:phone")
	if err != nil || res[0].State != api.MessageQueued {
		t.Fatalf("send: %+v %v", res, err)
	}
	if _, ok, _ := e.s.ClaimMessage(ctx, e.tower.ID); ok {
		t.Error("the watcher claimed a message of a session whose mod is fresh")
	}
	if m, ok, err := e.s.ClaimSessionMessage(ctx, e.tower.ID, "s1"); err != nil || !ok || m.ID != res[0].ID {
		t.Errorf("mod claim: %+v %v %v", m, ok, err)
	}
}

// Only the message poll marks the session's mod seen. Blocked-on, usage,
// and a message result leave mod_seen_at alone, so a mod whose message loop
// died stops holding the session's messages back from the watcher.
func TestOnlyModPollRefreshesModSeen(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	messageable := func() bool {
		t.Helper()
		x, err := e.s.GetSession(ctx, "s1")
		if err != nil {
			t.Fatal(err)
		}
		return x.Messageable
	}
	if err := e.s.SetBlockedOn(ctx, e.tower.ID, "s1", "Question: ok?"); err != nil || messageable() {
		t.Errorf("after blocked-on: %v, messageable %v", err, messageable())
	}
	pct := 5
	if err := e.s.SetUsage(ctx, e.tower.ID, "s1", api.UsageIn{ContextPercent: &pct}); err != nil || messageable() {
		t.Errorf("after usage: %v, messageable %v", err, messageable())
	}
	if err := e.s.RecordModPoll(ctx, e.tower.ID, "s1"); err != nil || !messageable() {
		t.Fatalf("after a poll: %v, messageable %v", err, messageable())
	}
	res, _, err := e.s.SendMessages(ctx, []string{"s1"}, "hi", "dashboard", "web:phone")
	if err != nil || res[0].State != api.MessageQueued {
		t.Fatalf("send: %+v %v", res, err)
	}
	e.clock.Advance(ControlPollWindow + time.Second)
	if messageable() {
		t.Fatal("still messageable after the window")
	}
	if _, err := e.s.FinishModMessage(ctx, e.tower.ID, res[0].ID, api.MessageResultIn{State: api.MessageBusy}); err != nil || messageable() {
		t.Errorf("after a result: %v, messageable %v", err, messageable())
	}
	if err := e.s.SetBlockedOn(ctx, e.tower.ID, "s1", ""); err != nil || messageable() {
		t.Errorf("after a clear: %v, messageable %v", err, messageable())
	}
}

// A late mod write for an ended session changes nothing and does not revive
// it.
func TestModWritesIgnoreEndedSession(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	if err := e.s.AddEvent(ctx, e.tower.ID, "s1", api.EventIn{Kind: api.KindEnded, Source: api.SourceHooks}); err != nil {
		t.Fatal(err)
	}
	before, err := e.s.SessionDetail(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	pct := 5
	if err := e.s.SetBlockedOn(ctx, e.tower.ID, "s1", "Question: ok?"); err != nil {
		t.Errorf("blocked-on: %v", err)
	}
	if err := e.s.SetUsage(ctx, e.tower.ID, "s1", api.UsageIn{ContextPercent: &pct}); err != nil {
		t.Errorf("usage: %v", err)
	}
	after, err := e.s.SessionDetail(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(before)
	b2, _ := json.Marshal(after)
	if string(b1) != string(b2) {
		t.Errorf("an ended session changed:\nbefore %s\nafter  %s", b1, b2)
	}
	// The owner check still runs first.
	if err := e.s.SetBlockedOn(ctx, e.bluebox.ID, "s1", "x"); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("other machine: %v", err)
	}
}
