package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// setStateAt posts a state_changed event for id on tower with time ts, as the
// hooks client and the plugin do.
func (e *controlEnv) setStateAt(id, state string, ts time.Time) {
	e.t.Helper()
	p, _ := json.Marshal(map[string]string{"agent_state": state})
	if err := e.s.AddEvent(context.Background(), e.tower.ID, id,
		api.EventIn{Kind: api.KindStateChanged, Source: api.SourceHooks, TS: ts, Payload: p}); err != nil {
		e.t.Fatal(err)
	}
}

// setState posts a state_changed event at the store clock's now.
func (e *controlEnv) setState(id, state string) {
	e.t.Helper()
	e.setStateAt(id, state, e.clock.Now())
}

// turnEnded reads sessions.turn_ended_at.
func (e *controlEnv) turnEnded(id string) *time.Time {
	e.t.Helper()
	var ns sql.NullString
	if err := e.s.db.QueryRow(`SELECT turn_ended_at FROM sessions WHERE id = ?`, id).Scan(&ns); err != nil {
		e.t.Fatal(err)
	}
	ts, err := parseNullTS(ns)
	if err != nil {
		e.t.Fatal(err)
	}
	return ts
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func TestMigrateV5ToV6(t *testing.T) {
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
	rollbackV6(t, s)
	if _, err := s.db.Exec("PRAGMA user_version = 5"); err != nil {
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
	// No backfill: an idle session from before the upgrade has no turn end.
	var turn sql.NullString
	if err := s2.db.QueryRow(`SELECT turn_ended_at FROM sessions WHERE id = 's1'`).Scan(&turn); err != nil || turn.Valid {
		t.Errorf("turn_ended_at after upgrade: %v %v, want NULL", turn, err)
	}
	var blocked sql.NullString
	if err := s2.db.QueryRow(`SELECT blocked_at FROM sessions WHERE id = 's1'`).Scan(&blocked); err != nil || blocked.Valid {
		t.Errorf("blocked_at after upgrade: %v %v, want NULL", blocked, err)
	}
	if _, err := s2.db.Exec(`INSERT INTO inbox_triage (session_id, triaged_since, updated_at) VALUES ('s1', 'x', 'x')`); err != nil {
		t.Errorf("inbox_triage after upgrade: %v", err)
	}
}

// TestTurnEndedAtEvents walks one session through state_changed events.
// Event times are explicit and in the past, so the server's clamp to now
// never applies.
func TestTurnEndedAtEvents(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	t0 := e.clock.Now()
	e.clock.Advance(time.Hour)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	tp := func(m int) *time.Time { x := at(m); return &x }
	if got := e.turnEnded("s1"); got != nil {
		t.Fatalf("new session turn_ended_at %v, want none", got)
	}
	steps := []struct {
		name  string
		state string
		ts    time.Time
		want  *time.Time
	}{
		{"idle without work", "idle", at(1), nil},
		{"working", "working", at(2), nil},
		{"working to idle", "idle", at(3), tp(3)},
		{"idle to done", "done", at(4), tp(3)},
		{"done to idle", "idle", at(5), tp(3)},
		{"idle to idle", "idle", at(6), tp(3)},
		{"working again", "working", at(7), tp(3)},
		{"working to done", "done", at(8), tp(8)},
		{"blocked", "blocked", at(9), tp(8)},
		{"blocked to idle", "idle", at(10), tp(10)},
		{"working at 11", "working", at(11), tp(10)},
		{"late idle, older than state_ts", "idle", at(10).Add(30 * time.Second), tp(10)},
		{"unknown", "unknown", at(12), tp(10)},
		{"unknown to idle", "idle", at(13), tp(10)},
	}
	for _, st := range steps {
		e.setStateAt("s1", st.state, st.ts)
		if got := e.turnEnded("s1"); !sameTime(got, st.want) {
			t.Errorf("%s: turn_ended_at %v, want %v", st.name, got, st.want)
		}
	}
	if s := e.get("s1"); s.AgentState != "idle" {
		t.Errorf("agent_state %q, want idle", s.AgentState)
	}
}

// TestTurnEndedAtUpserts covers the other path: upserts and herdr snapshots
// set the state with the server's time. A snapshot that repeats the state
// must leave turn_ended_at alone, or every heartbeat would undo a dismiss.
func TestTurnEndedAtUpserts(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "p1")
	up := func(state string) {
		t.Helper()
		if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: "s1", Source: api.SourcePlugin, AgentState: state}); err != nil {
			t.Fatal(err)
		}
	}
	snap := func(state string) {
		t.Helper()
		res, err := e.s.ReconcileHerdr(ctx, e.tower.ID, api.HerdrSessionsPut{HerdrSession: "default",
			Sessions: []api.SessionUpsert{{ID: "s1", HerdrPane: "p1", AgentState: state}}})
		if err != nil || res.Upserted != 1 {
			t.Fatalf("snapshot: %+v %v", res, err)
		}
	}
	now := func() *time.Time { x := e.clock.Now(); return &x }

	up("working")
	if got := e.turnEnded("s1"); got != nil {
		t.Fatalf("working: turn_ended_at %v", got)
	}
	e.clock.Advance(time.Minute)
	up("idle")
	first := now()
	if got := e.turnEnded("s1"); !sameTime(got, first) {
		t.Errorf("upsert working to idle: %v, want %v", got, first)
	}
	e.clock.Advance(time.Minute)
	up("idle")
	snap("idle")
	snap("done")
	snap("idle")
	if got := e.turnEnded("s1"); !sameTime(got, first) {
		t.Errorf("repeated or idle/done snapshots moved turn_ended_at to %v, want %v", got, first)
	}
	snap("working")
	e.clock.Advance(time.Minute)
	snap("done")
	second := now()
	if got := e.turnEnded("s1"); !sameTime(got, second) {
		t.Errorf("snapshot working to done: %v, want %v", got, second)
	}
	snap("blocked")
	e.clock.Advance(time.Minute)
	snap("idle")
	third := now()
	if got := e.turnEnded("s1"); !sameTime(got, third) {
		t.Errorf("snapshot blocked to idle: %v, want %v", got, third)
	}
	e.clock.Advance(time.Minute)
	up("") // no state: nothing changes
	if got := e.turnEnded("s1"); !sameTime(got, third) {
		t.Errorf("upsert without a state: %v, want %v", got, third)
	}
}

// TestBlockedAt covers blocked_at on both paths: it moves when the state
// moves into blocked, and a repeated blocked leaves it alone.
func TestBlockedAt(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "p1")
	blockedAt := func() *time.Time {
		t.Helper()
		var ns sql.NullString
		if err := e.s.db.QueryRow(`SELECT blocked_at FROM sessions WHERE id = 's1'`).Scan(&ns); err != nil {
			t.Fatal(err)
		}
		ts, err := parseNullTS(ns)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	snap := func(state string) {
		t.Helper()
		res, err := e.s.ReconcileHerdr(ctx, e.tower.ID, api.HerdrSessionsPut{HerdrSession: "default",
			Sessions: []api.SessionUpsert{{ID: "s1", HerdrPane: "p1", AgentState: state}}})
		if err != nil || res.Upserted != 1 {
			t.Fatalf("snapshot: %+v %v", res, err)
		}
	}
	if got := blockedAt(); got != nil {
		t.Fatalf("new session blocked_at %v", got)
	}
	t0 := e.clock.Now()
	e.clock.Advance(time.Hour)
	// Event path: the event's time, only when applied.
	e.setStateAt("s1", "blocked", t0.Add(time.Minute))
	first := t0.Add(time.Minute)
	if got := blockedAt(); !sameTime(got, &first) {
		t.Fatalf("event: blocked_at %v, want %v", got, first)
	}
	e.setStateAt("s1", "blocked", t0.Add(2*time.Minute))
	e.setStateAt("s1", "working", t0.Add(3*time.Minute))
	e.setStateAt("s1", "blocked", t0.Add(30*time.Second)) // stale: not applied
	if got := blockedAt(); !sameTime(got, &first) {
		t.Errorf("repeat, leave, stale event: blocked_at %v, want %v", got, first)
	}
	again := t0.Add(4 * time.Minute)
	e.setStateAt("s1", "blocked", again)
	if got := blockedAt(); !sameTime(got, &again) {
		t.Errorf("re-block: blocked_at %v, want %v", got, again)
	}
	// Snapshot path: server time.
	snap("working")
	e.clock.Advance(time.Minute)
	snap("blocked")
	second := e.clock.Now()
	if got := blockedAt(); !sameTime(got, &second) {
		t.Errorf("snapshot: blocked_at %v, want %v", got, second)
	}
	e.clock.Advance(time.Minute)
	snap("blocked")
	if got := blockedAt(); !sameTime(got, &second) {
		t.Errorf("repeated snapshot: blocked_at %v, want %v", got, second)
	}
	// Insert path: a new session that starts blocked.
	if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: "s2", Source: api.SourcePlugin, AgentState: "blocked"}); err != nil {
		t.Fatal(err)
	}
	var ns sql.NullString
	if err := e.s.db.QueryRow(`SELECT blocked_at FROM sessions WHERE id = 's2'`).Scan(&ns); err != nil || ns.String != formatTS(e.clock.Now()) {
		t.Errorf("insert: blocked_at %v %v, want %s", ns, err, formatTS(e.clock.Now()))
	}
}

func (e *controlEnv) sendPrompt(id string) {
	e.t.Helper()
	if err := e.s.AddEvent(context.Background(), e.tower.ID, id, api.EventIn{Kind: api.KindPrompt,
		Source: api.SourceHooks, TS: e.clock.Now(), Payload: json.RawMessage(`{"prompt":"next step"}`)}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *controlEnv) sendReport(id string, waiting ...string) {
	e.t.Helper()
	if err := e.s.AddReport(context.Background(), e.tower.ID, id, api.ReportIn{Done: []string{"a step"}, WaitingOn: waiting}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *controlEnv) readInbox() api.Inbox {
	e.t.Helper()
	in, err := e.s.Inbox(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return in
}

// inboxFor returns id's item, or nil when id is not in the inbox.
func (e *controlEnv) inboxFor(id string) *api.InboxItem {
	e.t.Helper()
	in := e.readInbox()
	for i := range in.Items {
		if in.Items[i].Session.ID == id {
			return &in.Items[i]
		}
	}
	return nil
}

// finishTurn moves id from working to idle one minute apart and returns the
// turn's end.
func (e *controlEnv) finishTurn(id string) time.Time {
	e.t.Helper()
	e.setState(id, "working")
	e.clock.Advance(time.Minute)
	e.setState(id, "idle")
	return e.clock.Now()
}

func TestInboxGroups(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	for _, id := range []string{"blk", "both", "fin", "fresh", "gone", "wait"} {
		e.session(e.tower, id, "")
	}
	e.clock.Advance(time.Minute)
	finAt := e.finishTurn("fin")
	e.setState("fresh", "idle") // never worked: not finished
	e.clock.Advance(time.Minute)
	t3 := e.clock.Now()
	e.sendReport("wait", "pick a name")
	e.setState("blk", "blocked")
	e.setState("both", "blocked")
	e.sendReport("both", "approve the plan") // also waiting: shown once, as blocked
	e.finishTurn("gone")
	if err := e.s.AddEvent(ctx, e.tower.ID, "gone", api.EventIn{Kind: api.KindEnded, Source: api.SourceHooks}); err != nil {
		t.Fatal(err)
	}

	in := e.readInbox()
	var got []string
	for _, it := range in.Items {
		got = append(got, it.Group+":"+it.Session.ID)
	}
	want := []string{"blocked:blk", "blocked:both", "waiting:wait", "finished:fin"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("items %v, want %v", got, want)
	}
	if in.Counts != (api.InboxCounts{Blocked: 2, Waiting: 1, Finished: 1}) {
		t.Errorf("counts %+v", in.Counts)
	}
	since := map[string]time.Time{"blk": t3, "both": t3, "wait": t3, "fin": finAt}
	for _, it := range in.Items {
		if !it.Since.Equal(since[it.Session.ID]) {
			t.Errorf("%s since %v, want %v", it.Session.ID, it.Since, since[it.Session.ID])
		}
	}
	if w := in.Items[2].WaitingOn; len(w) != 1 || w[0] != "pick a name" {
		t.Errorf("waiting item waiting_on %v", w)
	}
	if in.Items[1].WaitingOn != nil {
		t.Errorf("blocked item carries waiting_on %v", in.Items[1].WaitingOn)
	}
	if in.Items[0].Session.ResumeCommand == "" || in.Items[0].Session.Machine != "tower" {
		t.Errorf("item session is not the list object: %+v", in.Items[0].Session)
	}

	// A newer prompt clears waiting and finished.
	e.clock.Advance(time.Minute)
	e.sendPrompt("wait")
	e.sendPrompt("fin")
	if e.inboxFor("wait") != nil || e.inboxFor("fin") != nil {
		t.Error("a prompt after the trigger did not clear the item")
	}
	// A later report with waiting_on brings waiting back; one without clears it.
	e.clock.Advance(time.Minute)
	e.sendReport("wait", "pick a name again")
	if it := e.inboxFor("wait"); it == nil || it.Group != api.InboxWaiting {
		t.Errorf("new waiting report: %+v", it)
	}
	e.clock.Advance(time.Minute)
	e.sendReport("wait")
	if e.inboxFor("wait") != nil {
		t.Error("a report without waiting_on did not clear the item")
	}
	// A blocked session that answers its prompt and works again leaves.
	e.setState("blk", "working")
	if e.inboxFor("blk") != nil {
		t.Error("unblocked session still in the inbox")
	}
	// A stale session stays, marked stale.
	e.clock.Advance(10 * time.Minute)
	if it := e.inboxFor("both"); it == nil || it.Session.Status != api.StatusStale {
		t.Errorf("stale blocked session: %+v", it)
	}
}

// TestInboxBlockedSinceStable: a blocked session reported by an upsert only
// takes blocked_at on insert. since must stay the same across heartbeats, or a
// dismiss would never stick.
func TestInboxBlockedSinceStable(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: "s1", Source: api.SourcePlugin, AgentState: "blocked"}); err != nil {
		t.Fatal(err)
	}
	started := e.get("s1").StartedAt
	e.clock.Advance(time.Minute)
	if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: "s1", Source: api.SourcePlugin, AgentState: "blocked"}); err != nil {
		t.Fatal(err)
	}
	it := e.inboxFor("s1")
	if it == nil || it.Group != api.InboxBlocked || !it.Since.Equal(started) {
		t.Fatalf("item %+v, want blocked since started_at %v", it, started)
	}
	if err := e.s.Triage(ctx, "s1", it.Since, nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: "s1", Source: api.SourcePlugin, AgentState: "blocked"}); err != nil {
		t.Fatal(err)
	}
	if e.inboxFor("s1") != nil {
		t.Error("a heartbeat brought a dismissed blocked item back")
	}
}

// TestInboxBlockedFallbacks covers rows from before blocked_at existed: since
// falls back to state_ts, then to started_at, and a dismiss survives a
// repeated blocked heartbeat.
func TestInboxBlockedFallbacks(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		stateTS bool
	}{{"state_ts", true}, {"started_at", false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newControlEnv(t)
			if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: "s1", Source: api.SourcePlugin, AgentState: "blocked"}); err != nil {
				t.Fatal(err)
			}
			want := e.get("s1").StartedAt
			e.clock.Advance(time.Minute)
			q, args := `UPDATE sessions SET blocked_at = NULL, state_ts = NULL WHERE id = 's1'`, []any{}
			if tc.stateTS {
				want = e.clock.Now().Truncate(time.Second)
				q, args = `UPDATE sessions SET blocked_at = NULL, state_ts = ? WHERE id = 's1'`, []any{want.UTC().Format(time.RFC3339Nano)}
			}
			if _, err := e.s.db.ExecContext(ctx, q, args...); err != nil {
				t.Fatal(err)
			}
			e.clock.Advance(time.Minute)
			it := e.inboxFor("s1")
			if it == nil || it.Group != api.InboxBlocked || !it.Since.Equal(want) {
				t.Fatalf("item %+v, want blocked since %v", it, want)
			}
			if err := e.s.Triage(ctx, "s1", it.Since, nil); err != nil {
				t.Fatal(err)
			}
			e.clock.Advance(time.Minute)
			if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: "s1", Source: api.SourcePlugin, AgentState: "blocked"}); err != nil {
				t.Fatal(err)
			}
			if e.inboxFor("s1") != nil {
				t.Error("a heartbeat brought a dismissed blocked item back")
			}
		})
	}
}

func TestInboxTriage(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	first := e.finishTurn("s1")
	before, err := e.s.SessionDetail(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)

	// Dismiss hides the item, records no event, and leaves last_seen_at.
	if err := e.s.Triage(ctx, "s1", first, nil); err != nil {
		t.Fatal(err)
	}
	if in := e.readInbox(); len(in.Items) != 0 || in.Counts.Finished != 0 {
		t.Fatalf("after dismiss: %+v", in)
	}
	after, err := e.s.SessionDetail(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastSeenAt.Equal(before.LastSeenAt) || len(after.Events) != len(before.Events) {
		t.Errorf("triage changed the session: last_seen %v → %v, events %d → %d",
			before.LastSeenAt, after.LastSeenAt, len(before.Events), len(after.Events))
	}
	// A dismiss has no end.
	e.clock.Advance(48 * time.Hour)
	if e.inboxFor("s1") != nil {
		t.Error("a dismiss expired")
	}
	// Another finished turn has a later since and comes back.
	second := e.finishTurn("s1")
	if it := e.inboxFor("s1"); it == nil || !it.Since.Equal(second) {
		t.Fatalf("after a new turn: %+v, want since %v", it, second)
	}
	// A dismiss sent with an older since (a page loaded before the new turn)
	// does not hide the newer trigger.
	if err := e.s.Triage(ctx, "s1", first, nil); err != nil {
		t.Fatal(err)
	}
	if e.inboxFor("s1") == nil {
		t.Error("a dismiss with an older since hid a newer trigger")
	}
	// Snooze hides until snooze_until, then the same item comes back.
	until := e.clock.Now().Add(time.Hour)
	if err := e.s.Triage(ctx, "s1", second, &until); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(59 * time.Minute)
	if e.inboxFor("s1") != nil {
		t.Error("snoozed item shown before snooze_until")
	}
	e.clock.Advance(time.Minute)
	if it := e.inboxFor("s1"); it == nil || !it.Since.Equal(second) {
		t.Errorf("at snooze_until: %+v, want the same item back", it)
	}
	// One row per session: the snooze replaced the dismiss.
	if n := e.count(`SELECT COUNT(*) FROM inbox_triage WHERE session_id = 's1'`); n != 1 {
		t.Errorf("%d triage rows, want 1", n)
	}
}

func TestTriageValidation(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	now := e.clock.Now()
	at := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	cases := []struct {
		name  string
		id    string
		since time.Time
		until *time.Time
		want  error
	}{
		{"zero since", "s1", time.Time{}, nil, ErrInvalid},
		{"since 5m1s ahead", "s1", now.Add(5*time.Minute + time.Second), nil, ErrInvalid},
		{"until now", "s1", now, at(0), ErrInvalid},
		{"until in the past", "s1", now, at(-time.Minute), ErrInvalid},
		{"until 7 days and 1 second ahead", "s1", now, at(7*24*time.Hour + time.Second), ErrInvalid},
		{"unknown session", "nope", now, nil, ErrNotFound},
		{"since 5m ahead", "s1", now.Add(5 * time.Minute), nil, nil},
		{"until 7 days ahead", "s1", now, at(7 * 24 * time.Hour), nil},
	}
	for _, c := range cases {
		err := e.s.Triage(ctx, c.id, c.since, c.until)
		switch {
		case c.want == nil && err != nil:
			t.Errorf("%s: %v, want nil", c.name, err)
		case c.want != nil && !errors.Is(err, c.want):
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM inbox_triage WHERE session_id = 'nope'`); n != 0 {
		t.Errorf("unknown session got %d triage rows", n)
	}
}

func TestTriageCascade(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	if err := e.s.Triage(ctx, "s1", e.clock.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT COUNT(*) FROM inbox_triage`); n != 1 {
		t.Fatalf("%d rows before delete, want 1", n)
	}
	if _, err := e.s.db.Exec(`DELETE FROM sessions WHERE id = 's1'`); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT COUNT(*) FROM inbox_triage`); n != 0 {
		t.Errorf("%d triage rows after the session was deleted, want 0", n)
	}
}

// TestInboxBlockedSinceFromSnapshots: a session blocked through snapshots
// only (no state_changed event), dismissed, then unblocked and blocked again
// comes back with a later since.
func TestInboxBlockedSinceFromSnapshots(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	snap := func(state string) {
		t.Helper()
		if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: "s1", Source: api.SourcePlugin, AgentState: state}); err != nil {
			t.Fatal(err)
		}
	}
	snap("working")
	e.clock.Advance(time.Minute)
	snap("blocked")
	first := e.inboxFor("s1")
	if first == nil || first.Group != api.InboxBlocked {
		t.Fatalf("item %+v, want blocked", first)
	}
	if err := e.s.Triage(ctx, "s1", first.Since, nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	snap("blocked")
	if e.inboxFor("s1") != nil {
		t.Fatal("a heartbeat brought a dismissed item back")
	}
	e.clock.Advance(time.Minute)
	snap("working")
	e.clock.Advance(time.Minute)
	snap("blocked")
	it := e.inboxFor("s1")
	if it == nil || !it.Since.After(first.Since) {
		t.Fatalf("after re-block: %+v, want since after %v", it, first.Since)
	}
}
