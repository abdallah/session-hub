package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

// A row names one backend: the one the latest upsert reported.
func TestOneBackendPerRow(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	up := func(u api.SessionUpsert) api.Session {
		t.Helper()
		u.ID, u.Source = "s1", api.SourceHooks
		if _, err := e.s.UpsertSession(ctx, e.tower.ID, u); err != nil {
			t.Fatal(err)
		}
		return e.get("s1")
	}
	// herdr inside a Rex terminal: the pane wins, on insert and on update.
	both := api.SessionUpsert{HerdrSession: "default", HerdrWorkspace: "w1", HerdrPane: "w1:p1", RexSession: "session:1", RexBlock: "block:1"}
	for _, when := range []string{"insert", "update"} {
		if s := up(both); s.Backend() != api.BackendHerdr || s.RexSession != "" || s.RexBlock != "" {
			t.Errorf("%s with both: %+v, want the herdr pane only", when, s)
		}
	}
	// Resumed in a Rex block: the herdr IDs go.
	if s := up(api.SessionUpsert{RexSession: "session:2", RexBlock: "block:2"}); s.Backend() != api.BackendRex ||
		s.HerdrSession != "" || s.HerdrWorkspace != "" || s.HerdrPane != "" || s.RexSession != "session:2" {
		t.Errorf("after a Rex upsert: %+v, want the Rex block only", s)
	}
	// An upsert that names no backend keeps the row's.
	if s := up(api.SessionUpsert{AgentState: "idle"}); s.RexBlock != "block:2" {
		t.Errorf("after a bare upsert: rex_block %q, want block:2", s.RexBlock)
	}
	// Back in a herdr pane: the Rex IDs go.
	if s := up(api.SessionUpsert{HerdrSession: "default", HerdrPane: "w2:p1"}); s.Backend() != api.BackendHerdr || s.RexBlock != "" {
		t.Errorf("after a herdr upsert: %+v, want the herdr pane only", s)
	}
}

// A Rex block names one session: the one that reported it last.
func TestRexBlockHasOneSession(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	up := func(m Machine, id, block string) {
		t.Helper()
		if _, err := e.s.UpsertSession(ctx, m.ID, api.SessionUpsert{ID: id, Source: api.SourceHooks, RexSession: "session:1", RexBlock: block}); err != nil {
			t.Fatal(err)
		}
	}
	up(e.tower, "old", "block:1")
	up(e.tower, "elsewhere", "block:2")
	up(e.bluebox, "other-machine", "block:1")
	// A new Claude starts in block:1 on tower; "old" died there unreported.
	up(e.tower, "new", "block:1")
	for id, want := range map[string]string{"old": "", "new": "block:1", "elsewhere": "block:2", "other-machine": "block:1"} {
		if got := e.get(id); got.RexBlock != want || (want == "") != (got.RexSession == "") {
			t.Errorf("%s: rex_block %q rex_session %q, want block %q", id, got.RexBlock, got.RexSession, want)
		}
	}
	// A refresh that names no block takes nothing.
	if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: "old", Source: api.SourcePlugin}); err != nil {
		t.Fatal(err)
	}
	if got := e.get("new"); got.RexBlock != "block:1" {
		t.Errorf("new lost its block to a bare upsert: %q", got.RexBlock)
	}
}

func TestBackendsRefuseStartAndMove(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	in := api.StartIn{Dir: "~/Code/app"}

	if err := e.s.SetBackends(ctx, e.bluebox.ID, []string{"tmux"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown backend: %v, want ErrInvalid", err)
	}
	// A watcher that reported neither backend takes no start and no move.
	if err := e.s.SetBackends(ctx, e.bluebox.ID, nil); err != nil {
		t.Fatal(err)
	}
	const want = `machine "bluebox" has no herdr or Rex`
	if _, err := e.s.CreateStart(ctx, "bluebox", in, "web:phone"); !errors.Is(err, ErrNotControllable) || !strings.Contains(err.Error(), want) {
		t.Errorf("start on a machine with no backend: %v", err)
	}
	if _, err := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower"); !errors.Is(err, ErrNotControllable) || !strings.Contains(err.Error(), want) {
		t.Errorf("move to a machine with no backend: %v", err)
	}
	// One that reported Rex takes both, from a source session in a Rex block.
	if err := e.s.SetBackends(ctx, e.bluebox.ID, []string{api.BackendRex, api.BackendHerdr, api.BackendRex}); err != nil {
		t.Fatal(err)
	}
	if got := e.backends(e.bluebox); got != "herdr,rex" {
		t.Errorf("stored backends %q, want herdr,rex", got)
	}
	if _, err := e.s.CreateStart(ctx, "bluebox", in, "web:phone"); err != nil {
		t.Errorf("start on a Rex machine: %v", err)
	}
	if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: moveSID, Source: api.SourceHooks, RexSession: "session:1", RexBlock: "block:1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower"); err != nil {
		t.Errorf("move of a Rex session to a Rex machine: %v", err)
	}
}

func (e *controlEnv) backends(m Machine) string {
	e.t.Helper()
	var v string
	if err := e.s.db.QueryRow(`SELECT backends FROM machines WHERE id = ?`, m.ID).Scan(&v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

func TestMigrateV21ToV22(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	tok, _, err := s.AddMachine(ctx, "tower", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("ALTER TABLE machines DROP COLUMN backends"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 21"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	m, err := s2.MachineByToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	// A machine from before v22 has reported nothing, which refuses nothing.
	var v string
	if err := s2.db.QueryRow(`SELECT backends FROM machines WHERE id = ?`, m.ID).Scan(&v); err != nil || v != "" {
		t.Fatalf("backends after migration = %q, %v; want empty", v, err)
	}
	if err := noBackend("tower", v); err != nil {
		t.Errorf("a machine that never reported is refused: %v", err)
	}
}
