package store

import (
	"context"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

// rollbackV21 removes what schema v21 and later added.
func rollbackV21(t *testing.T, s *Store) {
	t.Helper()
	for _, q := range []string{"ALTER TABLE machines DROP COLUMN backends", // v22
		"ALTER TABLE sessions DROP COLUMN rex_session", "ALTER TABLE sessions DROP COLUMN rex_block"} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestMigrateV20ToV21(t *testing.T) {
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
	if _, err := s.UpsertSession(ctx, m.ID, api.SessionUpsert{ID: "s1", Source: api.SourceHooks}); err != nil {
		t.Fatal(err)
	}
	rollbackV21(t, s)
	if _, err := s.db.Exec("PRAGMA user_version = 20"); err != nil {
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
	// The Rex identity round-trips, and an upsert without it keeps it.
	up := api.SessionUpsert{ID: "s1", Source: api.SourceHooks,
		RexSession: "session:01a1b1e4-463e-770f-b6be-80a3d50f9542", RexBlock: "block:01a1b1e4-463e-77da-98f0-5a350b451a4f"}
	if _, err := s2.UpsertSession(ctx, m.ID, up); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.UpsertSession(ctx, m.ID, api.SessionUpsert{ID: "s1", Source: api.SourceHooks, AgentState: "idle"}); err != nil {
		t.Fatal(err)
	}
	got, err := s2.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RexSession != up.RexSession || got.RexBlock != up.RexBlock {
		t.Errorf("rex = %q %q, want %q %q", got.RexSession, got.RexBlock, up.RexSession, up.RexBlock)
	}
	if _, err := s2.UpsertSession(ctx, m.ID, api.SessionUpsert{ID: "s2", Source: api.SourceHooks, RexBlock: "-bad"}); err == nil {
		t.Error("rex_block \"-bad\" accepted")
	}
}
