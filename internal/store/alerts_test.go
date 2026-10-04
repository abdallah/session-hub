package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// triage reads session id's triage row. ok is false when there is none;
// dismiss is true when snooze_until is null.
func (e *controlEnv) triage(id string) (since time.Time, dismiss, ok bool) {
	e.t.Helper()
	var ts string
	var until sql.NullString
	err := e.s.db.QueryRow(`SELECT triaged_since, snooze_until FROM inbox_triage WHERE session_id = ?`, id).Scan(&ts, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	t, err := parseTS(ts)
	if err != nil {
		e.t.Fatal(err)
	}
	return t, !until.Valid, true
}

// setStateFrom posts a state_changed event from source src at ts.
func (e *controlEnv) setStateFrom(id, state, src string, ts time.Time) {
	e.t.Helper()
	p, _ := json.Marshal(map[string]string{"agent_state": state})
	if err := e.s.AddEvent(context.Background(), e.tower.ID, id,
		api.EventIn{Kind: api.KindStateChanged, Source: src, TS: ts, Payload: p}); err != nil {
		e.t.Fatal(err)
	}
}

// snapshot sends a herdr snapshot with session id in pane p1 in state.
func (e *controlEnv) snapshot(id, state string) {
	e.t.Helper()
	res, err := e.s.ReconcileHerdr(context.Background(), e.tower.ID, api.HerdrSessionsPut{HerdrSession: "default",
		Sessions: []api.SessionUpsert{{ID: id, HerdrPane: "p1", AgentState: state}}})
	if err != nil || res.Upserted != 1 {
		e.t.Fatalf("snapshot: %+v %v", res, err)
	}
}

func TestMigrateV6ToV7(t *testing.T) {
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
	if err := s.Triage(ctx, "s1", s.Now(), nil); err != nil {
		t.Fatal(err)
	}
	rollbackV7(t, s)
	if _, err := s.db.Exec("PRAGMA user_version = 6"); err != nil {
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
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM inbox_triage`).Scan(&n); err != nil || n != 1 {
		t.Errorf("triage rows after upgrade: %d %v, want 1", n, err)
	}
	if err := s2.RecordAlert(ctx, "s1", s2.Now()); err != nil {
		t.Errorf("RecordAlert after upgrade: %v", err)
	}
}

func TestAlerts(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	got, err := e.s.LastAlerts(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("LastAlerts on an empty table: %v %v", got, err)
	}
	first := e.clock.Now().Add(-time.Minute)
	if err := e.s.RecordAlert(ctx, "s1", first); err != nil {
		t.Fatal(err)
	}
	got, err = e.s.LastAlerts(ctx)
	if err != nil || len(got) != 1 || !got["s1"].Equal(first) {
		t.Fatalf("LastAlerts %v %v, want s1 at %v", got, err, first)
	}
	var sent string
	if err := e.s.db.QueryRow(`SELECT sent_at FROM inbox_alerts WHERE session_id = 's1'`).Scan(&sent); err != nil || sent != formatTS(e.clock.Now()) {
		t.Errorf("sent_at %q %v, want %s", sent, err, formatTS(e.clock.Now()))
	}
	// A later alert replaces the row.
	second := e.clock.Now()
	e.clock.Advance(time.Second)
	if err := e.s.RecordAlert(ctx, "s1", second); err != nil {
		t.Fatal(err)
	}
	got, _ = e.s.LastAlerts(ctx)
	if !got["s1"].Equal(second) {
		t.Errorf("after a second alert: %v, want %v", got["s1"], second)
	}
	if n := e.count(`SELECT COUNT(*) FROM inbox_alerts`); n != 1 {
		t.Errorf("%d alert rows, want 1", n)
	}
	// An unknown session is not found and writes nothing.
	if err := e.s.RecordAlert(ctx, "nope", second); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v, want ErrNotFound", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM inbox_alerts WHERE session_id = 'nope'`); n != 0 {
		t.Errorf("unknown session got %d rows", n)
	}
	// The row goes with its session.
	if _, err := e.s.db.Exec(`DELETE FROM sessions WHERE id = 's1'`); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT COUNT(*) FROM inbox_alerts`); n != 0 {
		t.Errorf("%d alert rows after the session was deleted, want 0", n)
	}
}

// TestAutoDismissSeenEvent: herdr marks a finished pane seen (done to idle)
// with a state_changed event. The Finished item is dismissed at its since,
// and the next turn comes back.
func TestAutoDismissSeenEvent(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.setState("s1", "working")
	e.clock.Advance(time.Minute)
	e.setState("s1", "done")
	end := e.clock.Now()
	if it := e.inboxFor("s1"); it == nil || it.Group != api.InboxFinished {
		t.Fatalf("after done: %+v, want finished", it)
	}
	if _, _, ok := e.triage("s1"); ok {
		t.Fatal("triage row before the pane was seen")
	}
	// An idle event older than the applied state is stored but not applied:
	// no dismiss.
	e.setStateFrom("s1", "idle", api.SourcePlugin, end.Add(-time.Second))
	if _, _, ok := e.triage("s1"); ok {
		t.Fatal("a stale idle event dismissed the item")
	}
	e.clock.Advance(time.Minute)
	e.setStateFrom("s1", "idle", api.SourcePlugin, e.clock.Now())
	if e.inboxFor("s1") != nil {
		t.Error("seen finished item still in the inbox")
	}
	if since, dismiss, ok := e.triage("s1"); !ok || !dismiss || !since.Equal(end) {
		t.Errorf("triage row since=%v dismiss=%v ok=%v, want a dismiss at %v", since, dismiss, ok, end)
	}
	// The next turn has a later since and comes back.
	e.clock.Advance(time.Minute)
	e.setState("s1", "working")
	e.clock.Advance(time.Minute)
	e.setState("s1", "done")
	if it := e.inboxFor("s1"); it == nil || !it.Since.Equal(e.clock.Now()) {
		t.Errorf("next turn: %+v, want finished since %v", it, e.clock.Now())
	}
}

// TestHooksIdleDoesNotLeaveDone: Claude's Stop hook posts idle with source
// hooks, possibly after herdr's done. That idle must not replace done, or
// herdr's later done to idle (the real "seen") would no longer dismiss.
func TestHooksIdleDoesNotLeaveDone(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.setState("s1", "working")
	e.clock.Advance(time.Minute)
	e.setStateFrom("s1", "done", api.SourcePlugin, e.clock.Now())
	end := e.clock.Now()
	e.clock.Advance(time.Second)
	e.setStateFrom("s1", "idle", api.SourceHooks, e.clock.Now())
	if it := e.inboxFor("s1"); it == nil || it.Group != api.InboxFinished {
		t.Fatalf("after the hooks idle: %+v, want finished", it)
	}
	if _, _, ok := e.triage("s1"); ok {
		t.Fatal("the hooks idle dismissed the item")
	}
	if n := e.count(`SELECT COUNT(*) FROM events WHERE session_id = 's1' AND source = 'hooks'`); n != 2 {
		t.Errorf("%d hooks event rows, want 2 (working, and the stored idle)", n)
	}
	e.clock.Advance(time.Minute)
	e.setStateFrom("s1", "idle", api.SourcePlugin, e.clock.Now())
	if since, dismiss, ok := e.triage("s1"); !ok || !dismiss || !since.Equal(end) {
		t.Errorf("triage row since=%v dismiss=%v ok=%v, want a dismiss at %v", since, dismiss, ok, end)
	}
}

// TestHooksOnlyIdleStillApplies: a session that never reached done goes
// working to idle from the hooks as before.
func TestHooksOnlyIdleStillApplies(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "h1", "")
	e.setStateFrom("h1", "working", api.SourceHooks, e.clock.Now())
	e.clock.Advance(time.Minute)
	e.setStateFrom("h1", "idle", api.SourceHooks, e.clock.Now())
	if n := e.count(`SELECT COUNT(*) FROM sessions WHERE id = 'h1' AND agent_state = 'idle'`); n != 1 {
		t.Errorf("hooks working to idle did not apply")
	}
}

// TestAutoDismissSeenSnapshot: the same on the snapshot path, where herdr's
// heartbeat repeats the state every minute. Only the move dismisses.
func TestAutoDismissSeenSnapshot(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "p1")
	e.snapshot("s1", "working")
	e.clock.Advance(time.Minute)
	e.snapshot("s1", "done")
	end := e.clock.Now()
	e.clock.Advance(time.Minute)
	e.snapshot("s1", "done")
	if _, _, ok := e.triage("s1"); ok {
		t.Fatal("a repeated done dismissed the item")
	}
	e.snapshot("s1", "idle")
	if since, dismiss, ok := e.triage("s1"); !ok || !dismiss || !since.Equal(end) {
		t.Fatalf("triage row since=%v dismiss=%v ok=%v, want a dismiss at %v", since, dismiss, ok, end)
	}
	if e.inboxFor("s1") != nil {
		t.Error("seen finished item still in the inbox")
	}
	// Later idle heartbeats leave the row alone.
	e.clock.Advance(time.Minute)
	e.snapshot("s1", "idle")
	if since, _, _ := e.triage("s1"); !since.Equal(end) {
		t.Errorf("idle heartbeat moved triaged_since to %v, want %v", since, end)
	}
	// So does the plain upsert path.
	if _, err := e.s.UpsertSession(context.Background(), e.tower.ID, api.SessionUpsert{ID: "s1", Source: api.SourcePlugin, AgentState: "idle"}); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT COUNT(*) FROM inbox_triage`); n != 1 {
		t.Errorf("%d triage rows, want 1", n)
	}
}

// TestAutoDismissLeavesOtherItems: done to idle on a Waiting item, working to
// idle (hooks-only), blocked to idle, and done to idle with no turn end (a
// session from before schema 6) write no triage row.
func TestAutoDismissLeavesOtherItems(t *testing.T) {
	e := newControlEnv(t)
	for _, id := range []string{"wait", "hooks", "blk", "old"} {
		e.session(e.tower, id, "")
	}
	// wait: finished a turn, then reported waiting_on, which outranks Finished.
	e.setState("wait", "working")
	e.clock.Advance(time.Minute)
	e.setState("wait", "done")
	e.clock.Advance(time.Minute)
	e.sendReport("wait", "pick a name")
	// hooks: working to idle, never done.
	e.setState("hooks", "working")
	e.clock.Advance(time.Minute)
	e.setStateFrom("hooks", "idle", api.SourcePlugin, e.clock.Now())
	// blk: blocked to idle ends a turn, but is not a seen pane.
	e.setState("blk", "blocked")
	e.clock.Advance(time.Minute)
	e.setStateFrom("blk", "idle", api.SourcePlugin, e.clock.Now())
	// old: done without a turn end.
	e.setState("old", "done")
	e.clock.Advance(time.Minute)
	e.setStateFrom("wait", "idle", api.SourcePlugin, e.clock.Now())
	e.setStateFrom("hooks", "idle", api.SourcePlugin, e.clock.Now())
	e.setStateFrom("old", "idle", api.SourcePlugin, e.clock.Now())

	for _, id := range []string{"wait", "hooks", "blk", "old"} {
		if _, _, ok := e.triage(id); ok {
			t.Errorf("%s: triage row written", id)
		}
	}
	if it := e.inboxFor("wait"); it == nil || it.Group != api.InboxWaiting {
		t.Errorf("wait: %+v, want waiting", it)
	}
	for _, id := range []string{"hooks", "blk"} {
		if it := e.inboxFor(id); it == nil || it.Group != api.InboxFinished {
			t.Errorf("%s: %+v, want finished", id, it)
		}
	}
	if it := e.inboxFor("old"); it != nil {
		t.Errorf("old: %+v, want no item", it)
	}
}

// TestAutoDismissIgnoresHooksIdle: the Claude Stop hook reports idle in a
// herdr pane nobody looked at. It is not a sighting.
func TestAutoDismissIgnoresHooksIdle(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.setStateFrom("s1", "working", api.SourcePlugin, e.clock.Now())
	e.clock.Advance(time.Minute)
	e.setStateFrom("s1", "done", api.SourcePlugin, e.clock.Now())
	e.clock.Advance(time.Minute)
	e.setStateFrom("s1", "idle", api.SourceHooks, e.clock.Now())
	if _, _, ok := e.triage("s1"); ok {
		t.Error("a hooks idle dismissed the item")
	}
	if it := e.inboxFor("s1"); it == nil || it.Group != api.InboxFinished {
		t.Errorf("after a hooks idle: %+v, want finished", it)
	}
	// The same on the upsert path.
	e.session(e.tower, "s2", "p2")
	e.setStateFrom("s2", "working", api.SourcePlugin, e.clock.Now())
	e.clock.Advance(time.Minute)
	e.setStateFrom("s2", "done", api.SourcePlugin, e.clock.Now())
	if _, err := e.s.UpsertSession(context.Background(), e.tower.ID, api.SessionUpsert{ID: "s2", Source: api.SourceHooks, AgentState: "idle"}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := e.triage("s2"); ok {
		t.Error("a hooks upsert idle dismissed the item")
	}
}

// TestAutoDismissSnoozeBecomesDismiss: a snooze of the same Finished item
// turns into a dismiss, and a triage row for a later since is not moved back.
func TestAutoDismissSnoozeBecomesDismiss(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.setState("s1", "working")
	e.clock.Advance(time.Minute)
	e.setState("s1", "done")
	end := e.clock.Now()
	until := end.Add(time.Hour)
	if err := e.s.Triage(context.Background(), "s1", end, &until); err != nil {
		t.Fatal(err)
	}
	if _, dismiss, ok := e.triage("s1"); !ok || dismiss {
		t.Fatalf("setup: want a snooze row, got dismiss=%v ok=%v", dismiss, ok)
	}
	e.clock.Advance(time.Minute)
	e.setStateFrom("s1", "idle", api.SourcePlugin, e.clock.Now())
	if since, dismiss, ok := e.triage("s1"); !ok || !dismiss || !since.Equal(end) {
		t.Errorf("since=%v dismiss=%v ok=%v, want a dismiss at %v", since, dismiss, ok, end)
	}
	// A row for a later since stays.
	later := end.Add(time.Hour)
	if _, err := e.s.db.Exec(`UPDATE inbox_triage SET triaged_since = ? WHERE session_id = 's1'`, formatTS(later)); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	e.setStateFrom("s1", "done", api.SourcePlugin, e.clock.Now())
	e.clock.Advance(time.Minute)
	e.setStateFrom("s1", "idle", api.SourcePlugin, e.clock.Now())
	if since, _, _ := e.triage("s1"); !since.Equal(later) {
		t.Errorf("triaged_since moved to %v, want %v", since, later)
	}
}
