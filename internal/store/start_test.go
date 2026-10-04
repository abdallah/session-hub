package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

func TestCreateStartChecks(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	in := api.StartIn{Dir: "~/Code/app", Prompt: "fix the build"}

	// An offline watcher refuses, and stores nothing.
	if _, err := e.s.CreateStart(ctx, "tower", in, "web:phone"); !errors.Is(err, ErrNotControllable) || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("offline: %v", err)
	}
	if _, err := e.s.CreateStart(ctx, "nosuch", in, "web:phone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown machine: %v", err)
	}
	for name, bad := range map[string]api.StartIn{
		"empty dir":       {Dir: ""},
		"relative dir":    {Dir: "Code/app"},
		"tilde user":      {Dir: "~bob/x"},
		"control char":    {Dir: "/home/a\x1b[31m"},
		"long dir":        {Dir: "/" + strings.Repeat("a", api.MaxStartDirBytes)},
		"long prompt":     {Dir: "/x", Prompt: strings.Repeat("é", api.MaxStartPromptRunes+1)},
		"prompt with \\r": {Dir: "/x", Prompt: "a\rb"},
		"prompt with ESC": {Dir: "/x", Prompt: "a\x1bb"},
	} {
		if _, err := e.s.CreateStart(ctx, "tower", bad, "web:phone"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	var n int
	if err := e.s.db.QueryRow(`SELECT COUNT(*) FROM start_requests`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after refusals: %d, %v", n, err)
	}

	e.poll(e.tower)
	r, err := e.s.CreateStart(ctx, "tower", in, "web:phone")
	if err != nil {
		t.Fatal(err)
	}
	if !ValidStartID(r.ID) || r.Machine != "tower" || r.Dir != in.Dir || r.Prompt != in.Prompt ||
		r.State != api.ControlPending || r.RequestedBy != "web:phone" || !r.ExpiresAt.Equal(r.CreatedAt.Add(ControlTTL)) {
		t.Fatalf("created %+v", r)
	}
	// Newlines and tabs are fine in a prompt.
	if _, err := e.s.CreateStart(ctx, "tower", api.StartIn{Dir: "/srv", Prompt: "a\n\tb"}, "machine:bluebox"); err != nil {
		t.Fatal(err)
	}
	for i := 2; i < MaxPendingStartsPerMachine; i++ {
		if _, err := e.s.CreateStart(ctx, "tower", in, "web:phone"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.CreateStart(ctx, "tower", in, "web:phone"); !errors.Is(err, ErrTooMany) {
		t.Fatalf("over the cap: %v", err)
	}
	// Another machine has its own cap, and its own offline check.
	if _, err := e.s.CreateStart(ctx, "bluebox", in, "web:phone"); !errors.Is(err, ErrNotControllable) {
		t.Fatalf("bluebox offline: %v", err)
	}
}

func TestStartClaimFinish(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.poll(e.tower)
	e.poll(e.bluebox)
	first, err := e.s.CreateStart(ctx, "tower", api.StartIn{Dir: "/a"}, "web:phone")
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(1)
	second, err := e.s.CreateStart(ctx, "tower", api.StartIn{Dir: "/b", Prompt: "hi", Trust: true}, "web:phone")
	if err != nil {
		t.Fatal(err)
	}

	// bluebox sees nothing of tower's.
	if _, ok, err := e.s.ClaimStart(ctx, e.bluebox.ID); err != nil || ok {
		t.Fatalf("bluebox claim: ok %v, %v", ok, err)
	}
	c, ok, err := e.s.ClaimStart(ctx, e.tower.ID)
	if err != nil || !ok {
		t.Fatalf("claim: ok %v, %v", ok, err)
	}
	if c.Request.ID != first.ID || c.Request.Action != api.ActionStart || c.Request.State != api.ControlClaimed ||
		c.Start == nil || c.Start.Dir != "/a" || c.Start.Trust || c.Session.ID != "" {
		t.Fatalf("first claim %+v start %+v", c.Request, c.Start)
	}
	c2, ok, err := e.s.ClaimStart(ctx, e.tower.ID)
	if err != nil || !ok || c2.Start.ID != second.ID || c2.Start.Prompt != "hi" || !c2.Start.Trust {
		t.Fatalf("second claim: %+v ok %v %v", c2.Start, ok, err)
	}
	if _, ok, _ := e.s.ClaimStart(ctx, e.tower.ID); ok {
		t.Fatal("a third claim found something")
	}

	// Only the claiming machine may finish, once, with a valid result.
	done := api.ControlResultIn{State: api.ControlDone, URL: testLink}
	if _, err := e.s.FinishStart(ctx, e.bluebox.ID, first.ID, done); !errors.Is(err, ErrWrongMachine) {
		t.Fatalf("other machine: %v", err)
	}
	for name, bad := range map[string]api.ControlResultIn{
		"state":        {State: api.ControlClaimed},
		"url on fail":  {State: api.ControlFailed, URL: testLink},
		"foreign url":  {State: api.ControlDone, URL: "https://evil.example/x"},
		"control char": {State: api.ControlFailed, Detail: "a\x1bb"},
	} {
		if _, err := e.s.FinishStart(ctx, e.tower.ID, first.ID, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	got, err := e.s.FinishStart(ctx, e.tower.ID, first.ID, done)
	if err != nil || got.State != api.ControlDone || got.URL != testLink || got.FinishedAt == nil {
		t.Fatalf("finish: %+v, %v", got, err)
	}
	if _, err := e.s.FinishStart(ctx, e.tower.ID, first.ID, done); !errors.Is(err, ErrRequestClosed) {
		t.Fatalf("second finish: %v", err)
	}
	if got, err := e.s.GetStart(ctx, first.ID); err != nil || got.State != api.ControlDone || got.URL != testLink {
		t.Fatalf("read back: %+v, %v", got, err)
	}

	// A claimed request past its TTL reads as expired, and can't be finished.
	e.clock.Advance(ControlTTL)
	if got, err := e.s.GetStart(ctx, second.ID); err != nil || got.State != api.ControlExpired {
		t.Fatalf("expired read: %+v, %v", got, err)
	}
	if _, err := e.s.FinishStart(ctx, e.tower.ID, second.ID, api.ControlResultIn{State: api.ControlFailed}); !errors.Is(err, ErrRequestClosed) {
		t.Fatalf("finish after expiry: %v", err)
	}
	if _, err := e.s.GetStart(ctx, "st_"+strings.Repeat("A", 22)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := e.s.GetStart(ctx, "cr_"+strings.Repeat("A", 22)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
}

func TestStartPendingExpiresAndMachineOnline(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.poll(e.tower)
	ms, err := e.s.ListMachines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	online := map[string]bool{}
	for _, m := range ms {
		online[m.Name] = m.Online
	}
	if !online["tower"] || online["bluebox"] {
		t.Fatalf("online: %v", online)
	}
	r, err := e.s.CreateStart(ctx, "tower", api.StartIn{Dir: "/a"}, "web:phone")
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(ControlTTL)
	if _, ok, err := e.s.ClaimStart(ctx, e.tower.ID); err != nil || ok {
		t.Fatalf("claim of an expired request: ok %v, %v", ok, err)
	}
	var state string
	if err := e.s.db.QueryRow(`SELECT state FROM start_requests WHERE id = ?`, r.ID).Scan(&state); err != nil || state != api.ControlExpired {
		t.Fatalf("stored state %q, %v", state, err)
	}
	// Removing the machine removes its requests.
	if err := e.s.RemoveMachine(ctx, "tower"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetStart(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after machine rm: %v", err)
	}
}

func TestMigrateV9ToV10(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	tok, _, err := s.AddMachine(ctx, "tower", "", "")
	if err != nil {
		t.Fatal(err)
	}
	rollbackV10(t, s)
	if _, err := s.db.Exec("PRAGMA user_version = 9"); err != nil {
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
	m, err := s2.MachineByToken(ctx, tok)
	if err != nil {
		t.Fatalf("token after upgrade: %v", err)
	}
	if err := s2.RecordPoll(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.CreateStart(ctx, "tower", api.StartIn{Dir: "/a"}, "machine:tower"); err != nil {
		t.Errorf("CreateStart after upgrade: %v", err)
	}
}

func TestMigrateV10ToV11(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	if _, _, err := s.AddMachine(ctx, "tower", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("ALTER TABLE start_requests DROP COLUMN trust"); err != nil {
		t.Fatal(err)
	}
	// A request stored by a v10 server.
	if _, err := s.db.Exec(`INSERT INTO start_requests (id, machine_id, dir, prompt, state, requested_by, created_at, expires_at)
		SELECT 'st_AAAAAAAAAAAAAAAAAAAAAA', id, '/a', '', 'done', 'web:phone', '2026-10-04T08:00:00.000000000Z',
		'2026-10-04T08:02:00.000000000Z' FROM machines WHERE name = 'tower'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 10"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	r, err := s2.GetStart(ctx, "st_AAAAAAAAAAAAAAAAAAAAAA")
	if err != nil || r.Trust || r.Dir != "/a" {
		t.Fatalf("old request after upgrade: %+v %v", r, err)
	}
}
