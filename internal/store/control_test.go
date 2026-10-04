package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

const testLink = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *testClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// controlEnv is a store with a fake clock and machines tower and bluebox.
type controlEnv struct {
	t              *testing.T
	s              *Store
	clock          *testClock
	tower, bluebox Machine
}

func newControlEnv(t *testing.T) *controlEnv {
	t.Helper()
	clock := &testClock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	s, err := Open(filepath.Join(t.TempDir(), "sessionhub.db"), Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := &controlEnv{t: t, s: s, clock: clock}
	ctx := context.Background()
	for _, m := range []struct {
		name string
		dst  *Machine
	}{{"tower", &e.tower}, {"bluebox", &e.bluebox}} {
		tok, _, err := s.AddMachine(ctx, m.name, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if *m.dst, err = s.MachineByToken(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// session registers id on m, with a herdr pane unless pane is "".
func (e *controlEnv) session(m Machine, id, pane string) {
	e.t.Helper()
	u := api.SessionUpsert{ID: id, Agent: "claude", Source: api.SourcePlugin, CWD: "/home/user/proj", HerdrPane: pane}
	if pane != "" {
		u.HerdrSession = "default"
	}
	if _, err := e.s.UpsertSession(context.Background(), m.ID, u); err != nil {
		e.t.Fatal(err)
	}
}

func (e *controlEnv) poll(m Machine) {
	e.t.Helper()
	if err := e.s.RecordPoll(context.Background(), m.ID); err != nil {
		e.t.Fatal(err)
	}
}

func (e *controlEnv) get(id string) api.Session {
	e.t.Helper()
	x, err := e.s.GetSession(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return x
}

func (e *controlEnv) create(id string) api.ControlRequest {
	e.t.Helper()
	r, created, err := e.s.CreateControl(context.Background(), id, api.RequestedByDashboard)
	if err != nil || !created {
		e.t.Fatalf("create %s: created=%v err=%v", id, created, err)
	}
	return r
}

func (e *controlEnv) events(id, kind string) []api.Event {
	e.t.Helper()
	d, err := e.s.SessionDetail(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []api.Event
	for _, ev := range d.Events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func (e *controlEnv) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.s.db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestCreateControlRules(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "sess-a", "w1:p1")
	e.session(e.tower, "sess-hooks", "")

	// Refusals change nothing.
	for _, tc := range []struct {
		id, by string
		want   error
		msg    string
	}{
		{"sess-none", api.RequestedByDashboard, ErrNotFound, "sess-none"},
		{"sess-hooks", api.RequestedByDashboard, ErrNotControllable, "not in herdr"},
		{"sess-a", api.RequestedByDashboard, ErrNotControllable, "offline"},
		{"sess-a", "phone", ErrInvalid, "requested_by"},
		{"-bad", api.RequestedByDashboard, ErrInvalid, "session id"},
	} {
		if _, _, err := e.s.CreateControl(ctx, tc.id, tc.by); !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("create %s by %q: err %v, want %v with %q", tc.id, tc.by, err, tc.want, tc.msg)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM control_requests`); n != 0 {
		t.Fatalf("refused creates stored %d rows", n)
	}

	e.poll(e.tower)
	if x := e.get("sess-a"); !x.Controllable || x.RemoteControl != nil {
		t.Fatalf("after a poll: controllable=%v request=%+v", x.Controllable, x.RemoteControl)
	}
	if x := e.get("sess-hooks"); x.Controllable {
		t.Error("a session without a pane is controllable")
	}
	r := e.create("sess-a")
	now := e.clock.Now()
	if !regexp.MustCompile(`^cr_[A-Za-z0-9_-]{22}$`).MatchString(r.ID) || !ValidControlID(r.ID) {
		t.Errorf("request ID %q", r.ID)
	}
	if r.SessionID != "sess-a" || r.Machine != "tower" || r.Action != api.ActionRemoteControl || r.State != api.ControlPending ||
		r.RequestedBy != api.RequestedByDashboard || !r.CreatedAt.Equal(now) || !r.ExpiresAt.Equal(now.Add(2*time.Minute)) ||
		r.ClaimedAt != nil || r.FinishedAt != nil || r.URL != "" || r.Detail != "" {
		t.Errorf("new request: %+v", r)
	}

	// The open request comes back for a second create, from anyone.
	again, created, err := e.s.CreateControl(ctx, "sess-a", api.RequestedByMachinePrefix+"bluebox")
	if err != nil || created || again.ID != r.ID {
		t.Errorf("second create: %+v created=%v err=%v", again, created, err)
	}
	if x := e.get("sess-a"); x.RemoteControl == nil || x.RemoteControl.ID != r.ID || x.RemoteControl.State != api.ControlPending {
		t.Errorf("session's latest request: %+v", x.RemoteControl)
	}
	evs := e.events("sess-a", api.KindRemoteControlRequested)
	if len(evs) != 1 || evs[0].Source != api.SourceServer || !strings.Contains(string(evs[0].Payload), r.ID) ||
		!strings.Contains(string(evs[0].Payload), `"requested_by":"dashboard"`) {
		t.Errorf("requested events: %+v", evs)
	}
}

func TestControllableWindow(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "sess-a", "w1:p1")
	e.poll(e.tower)
	e.clock.Advance(2 * time.Minute)
	if !e.get("sess-a").Controllable {
		t.Error("exactly 2 minutes after a poll: not controllable")
	}
	e.clock.Advance(time.Second)
	if e.get("sess-a").Controllable {
		t.Error("2m1s after a poll: still controllable")
	}
	if _, _, err := e.s.CreateControl(context.Background(), "sess-a", api.RequestedByDashboard); !errors.Is(err, ErrNotControllable) {
		t.Errorf("create with a silent watcher: %v", err)
	}
}

func TestCreateControlLimit(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.poll(e.tower)
	for i := range MaxPendingPerMachine + 1 {
		e.session(e.tower, fmt.Sprintf("limit-%02d", i), fmt.Sprintf("w1:p%d", i+1))
	}
	for i := range MaxPendingPerMachine {
		e.create(fmt.Sprintf("limit-%02d", i))
	}
	if _, _, err := e.s.CreateControl(ctx, "limit-10", api.RequestedByDashboard); !errors.Is(err, ErrTooMany) || !strings.Contains(err.Error(), "10 pending") {
		t.Fatalf("11th create: %v", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM control_requests`); n != MaxPendingPerMachine {
		t.Errorf("%d rows after the refused create, want %d", n, MaxPendingPerMachine)
	}
	// Another machine has its own limit.
	e.session(e.bluebox, "bluebox-a", "w1:p1")
	e.poll(e.bluebox)
	e.create("bluebox-a")
	// A claim frees a slot.
	if _, ok, err := e.s.ClaimControl(ctx, e.tower.ID); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	e.create("limit-10")
}

func TestClaimControlOrderAndOwner(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.poll(e.tower)
	e.session(e.tower, "sess-a", "w1:p1")
	e.session(e.tower, "sess-b", "w1:p2")
	a := e.create("sess-a")
	e.clock.Advance(time.Second)
	b := e.create("sess-b")

	if _, ok, err := e.s.ClaimControl(ctx, e.bluebox.ID); ok || err != nil {
		t.Errorf("bluebox claimed tower's request: ok=%v err=%v", ok, err)
	}
	for _, want := range []api.ControlRequest{a, b} {
		c, ok, err := e.s.ClaimControl(ctx, e.tower.ID)
		if err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		if c.Request.ID != want.ID || c.Request.State != api.ControlClaimed || c.Request.ClaimedAt == nil ||
			!c.Request.ClaimedAt.Equal(e.clock.Now()) || c.Session.ID != want.SessionID || c.Session.HerdrPane == "" {
			t.Errorf("claim: %+v, want request %s", c, want.ID)
		}
	}
	if _, ok, err := e.s.ClaimControl(ctx, e.tower.ID); ok || err != nil {
		t.Errorf("third claim: ok=%v err=%v", ok, err)
	}
	if x := e.get("sess-a"); x.RemoteControl.State != api.ControlClaimed {
		t.Errorf("session reads %s, want claimed", x.RemoteControl.State)
	}
}

func TestControlExpiry(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.poll(e.tower)
	e.session(e.tower, "sess-a", "w1:p1")
	e.session(e.tower, "sess-b", "w1:p2")

	// Pending past expires_at: reads say expired, a claim skips it, and a
	// new create opens a fresh request.
	a := e.create("sess-a")
	e.clock.Advance(2 * time.Minute)
	e.poll(e.tower)
	if x := e.get("sess-a"); x.RemoteControl.State != api.ControlExpired {
		t.Errorf("pending after 2m: %s", x.RemoteControl.State)
	}
	if _, ok, _ := e.s.ClaimControl(ctx, e.tower.ID); ok {
		t.Error("an expired request was claimed")
	}
	if a2 := e.create("sess-a"); a2.ID == a.ID {
		t.Error("create returned the expired request")
	}

	// Claimed past expires_at: reads say expired, and a result is refused.
	b := e.create("sess-b")
	// sess-a's fresh request is older, so claim twice to reach sess-b's.
	e.s.ClaimControl(ctx, e.tower.ID)
	if c, ok, err := e.s.ClaimControl(ctx, e.tower.ID); !ok || err != nil || c.Request.ID != b.ID {
		t.Fatalf("claim b: %+v %v %v", c, ok, err)
	}
	e.clock.Advance(2 * time.Minute)
	if x := e.get("sess-b"); x.RemoteControl.State != api.ControlExpired {
		t.Errorf("claimed after 2m: %s", x.RemoteControl.State)
	}
	_, err := e.s.FinishControl(ctx, e.tower.ID, b.ID, api.ControlResultIn{State: api.ControlDone, URL: testLink})
	if !errors.Is(err, ErrRequestClosed) || !strings.Contains(err.Error(), "expired") {
		t.Errorf("late result: %v", err)
	}
	if x := e.get("sess-b"); x.RemoteControlURL != "" || x.RemoteControlAt != nil {
		t.Errorf("a late result set the link: %q", x.RemoteControlURL)
	}
	if n := len(e.events("sess-b", api.KindRemoteControlResult)); n != 0 {
		t.Errorf("a late result recorded %d events", n)
	}
}

func TestFinishControl(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.poll(e.tower)
	e.session(e.tower, "sess-a", "w1:p1")
	r := e.create("sess-a")

	if _, err := e.s.FinishControl(ctx, e.tower.ID, r.ID, api.ControlResultIn{State: api.ControlDone}); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("result before the claim: %v", err)
	}
	if _, ok, err := e.s.ClaimControl(ctx, e.tower.ID); !ok || err != nil {
		t.Fatal(ok, err)
	}
	bad := []struct {
		name string
		id   string
		in   api.ControlResultIn
		want error
	}{
		{"malformed ID", "cr_short", api.ControlResultIn{State: "done"}, ErrInvalid},
		{"state pending", r.ID, api.ControlResultIn{State: "pending"}, ErrInvalid},
		{"http", r.ID, api.ControlResultIn{State: "done", URL: "http://claude.ai/code/session_abc"}, ErrInvalid},
		{"other host", r.ID, api.ControlResultIn{State: "done", URL: "https://claude.ai.evil.example/code/session_abc"}, ErrInvalid},
		{"extra path", r.ID, api.ControlResultIn{State: "done", URL: "https://claude.ai/code/session_abc/x"}, ErrInvalid},
		{"query", r.ID, api.ControlResultIn{State: "done", URL: "https://claude.ai/code/session_abc?x=1"}, ErrInvalid},
		{"empty session", r.ID, api.ControlResultIn{State: "done", URL: "https://claude.ai/code/session_"}, ErrInvalid},
		{"trailing newline", r.ID, api.ControlResultIn{State: "done", URL: testLink + "\n"}, ErrInvalid},
		{"URL on failed", r.ID, api.ControlResultIn{State: "failed", URL: testLink}, ErrInvalid},
		{"URL too long", r.ID, api.ControlResultIn{State: "done", URL: testLink + strings.Repeat("a", 200)}, ErrInvalid},
		{"control character", r.ID, api.ControlResultIn{State: "failed", Detail: "a\x1b[31mb"}, ErrInvalid},
		{"detail too long", r.ID, api.ControlResultIn{State: "failed", Detail: strings.Repeat("x", 201)}, ErrInvalid},
		{"unknown request", "cr_AAAAAAAAAAAAAAAAAAAAAA", api.ControlResultIn{State: "done"}, ErrNotFound},
	}
	for _, tc := range bad {
		if _, err := e.s.FinishControl(ctx, e.tower.ID, tc.id, tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: err %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := e.s.FinishControl(ctx, e.bluebox.ID, r.ID, api.ControlResultIn{State: api.ControlDone}); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("another machine's result: %v", err)
	}
	if x := e.get("sess-a"); x.RemoteControl.State != api.ControlClaimed || x.RemoteControlURL != "" {
		t.Fatalf("refused results changed the request: %+v", x.RemoteControl)
	}

	e.clock.Advance(3 * time.Second)
	got, err := e.s.FinishControl(ctx, e.tower.ID, r.ID, api.ControlResultIn{State: api.ControlDone, URL: testLink, Detail: "resumed in a new herdr workspace"})
	if err != nil {
		t.Fatal(err)
	}
	now := e.clock.Now()
	if got.State != api.ControlDone || got.URL != testLink || got.FinishedAt == nil || !got.FinishedAt.Equal(now) {
		t.Errorf("finished request: %+v", got)
	}
	x := e.get("sess-a")
	if x.RemoteControlURL != testLink || x.RemoteControlAt == nil || !x.RemoteControlAt.Equal(now) || x.RemoteControl.State != api.ControlDone {
		t.Errorf("session after done: url %q at %v request %+v", x.RemoteControlURL, x.RemoteControlAt, x.RemoteControl)
	}
	evs := e.events("sess-a", api.KindRemoteControlResult)
	var p map[string]string
	if len(evs) != 1 || evs[0].Source != api.SourcePlugin || json.Unmarshal(evs[0].Payload, &p) != nil ||
		p["request_id"] != r.ID || p["state"] != "done" || p["url"] != testLink {
		t.Errorf("result events: %+v", evs)
	}
	if _, err := e.s.FinishControl(ctx, e.tower.ID, r.ID, api.ControlResultIn{State: api.ControlFailed}); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("second result: %v", err)
	}
}

// The last link goes away in each write path that ends a session.
func TestRemoteControlLinkClearedWhenSessionEnds(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.poll(e.tower)
	link := func(id string) {
		e.t.Helper()
		e.session(e.tower, id, "w1:"+id)
		r := e.create(id)
		if _, ok, err := e.s.ClaimControl(ctx, e.tower.ID); !ok || err != nil {
			t.Fatal(ok, err)
		}
		if _, err := e.s.FinishControl(ctx, e.tower.ID, r.ID, api.ControlResultIn{State: api.ControlDone, URL: testLink}); err != nil {
			t.Fatal(err)
		}
		if e.get(id).RemoteControlURL != testLink {
			t.Fatalf("%s has no link", id)
		}
	}
	link("p1")
	link("p2")
	link("p3")
	link("p4")

	// A state change keeps the link.
	if err := e.s.AddEvent(ctx, e.tower.ID, "p4", api.EventIn{Kind: api.KindStateChanged, Source: api.SourcePlugin,
		Payload: json.RawMessage(`{"agent_state":"working"}`)}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{api.KindPaneClosed, api.KindEnded} {
		id := map[string]string{api.KindPaneClosed: "p1", api.KindEnded: "p2"}[kind]
		if err := e.s.AddEvent(ctx, e.tower.ID, id, api.EventIn{Kind: kind, Source: api.SourcePlugin}); err != nil {
			t.Fatal(err)
		}
	}
	// p3 is missing from a snapshot that lists p4.
	if _, err := e.s.ReconcileHerdr(ctx, e.tower.ID, api.HerdrSessionsPut{HerdrSession: "default",
		Sessions: []api.SessionUpsert{{ID: "p4", Agent: "claude", HerdrPane: "w1:p4"}}}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"p1": "", "p2": "", "p3": "", "p4": testLink} {
		x := e.get(id)
		if x.RemoteControlURL != want || (want == "") != (x.RemoteControlAt == nil) {
			t.Errorf("%s: link %q at %v, want %q", id, x.RemoteControlURL, x.RemoteControlAt, want)
		}
	}
}

// A v2 database upgrades to v3 in place and keeps its rows.
func TestMigrateV2Database(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessionhub.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.AddMachine(ctx, "tower", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := s.MachineByToken(ctx, tok)
	if _, err := s.UpsertSession(ctx, m.ID, api.SessionUpsert{ID: "s1", Source: api.SourcePlugin, HerdrSession: "default", HerdrPane: "w1:p1"}); err != nil {
		t.Fatal(err)
	}
	rollbackV3(t, s)
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var v int
	if err := s2.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Errorf("user_version %d %v, want %d", v, err, schemaVersion)
	}
	if err := s2.RecordPoll(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s2.CreateControl(ctx, "s1", api.RequestedByDashboard); err != nil || !created {
		t.Errorf("create after migration: %v %v", created, err)
	}
}
