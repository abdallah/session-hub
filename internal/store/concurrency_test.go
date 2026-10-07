package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

// TestMigrateV1Database opens a database created by the first release and
// checks it upgrades in place, keeping its rows.
func TestMigrateV1Database(t *testing.T) {
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
	// Roll the file back to schema v1.
	rollbackV3(t, s)
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE sessions DROP COLUMN state_ts"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
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
	m, err := s2.MachineByToken(ctx, tok)
	if err != nil || m.Name != "tower" {
		t.Errorf("machine after migration: %+v %v", m, err)
	}
	if _, err := s2.UpsertSession(ctx, m.ID, api.SessionUpsert{ID: "s1", Source: api.SourceHooks, AgentState: "idle"}); err != nil {
		t.Errorf("upsert after migration: %v", err)
	}
}

// TestSecondConnectionWritesDoNotFail runs a second *sql.DB (as `sessionhub machine
// add` does while the server runs) against the same file while the store
// upserts. With _txlock=immediate each write transaction takes the write lock
// up front and waits on busy_timeout, so none fails with SQLITE_BUSY.
func TestSecondConnectionWritesDoNotFail(t *testing.T) {
	ctx := context.Background()
	s, path := openTemp(t)
	tok, _, err := s.AddMachine(ctx, "tower", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.MachineByToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	const n = 200
	errs := make(chan error, 2)
	go func() {
		for i := range n {
			u := api.SessionUpsert{ID: fmt.Sprintf("s%d", i), Source: api.SourceHooks}
			if _, err := s.UpsertSession(ctx, m.ID, u); err != nil {
				errs <- err
				return
			}
			// Second write on the same row: read-then-write in one transaction.
			u.AgentState = "working"
			if _, err := s.UpsertSession(ctx, m.ID, u); err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()
	go func() {
		for i := range n {
			if _, _, err := other.AddMachine(ctx, fmt.Sprintf("m%d", i), "", ""); err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("concurrent write failed: %v", err)
		}
	}
	var sessions, machines int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions").Scan(&sessions); err != nil || sessions != n {
		t.Errorf("sessions stored: %d %v, want %d", sessions, err, n)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM machines").Scan(&machines); err != nil || machines != n+1 {
		t.Errorf("machines stored: %d %v, want %d", machines, err, n+1)
	}
}

// rollbackV3 removes what schema v3 added, so a test can set user_version
// to 1 or 2 and reopen the file as that older release left it.
func rollbackV3(t *testing.T, s *Store) {
	t.Helper()
	rollbackV4(t, s)
	for _, q := range []string{
		"DROP TABLE control_requests",
		"ALTER TABLE machines DROP COLUMN last_poll",
		"ALTER TABLE sessions DROP COLUMN rc_url",
		"ALTER TABLE sessions DROP COLUMN rc_at",
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// rollbackV4 removes what schema v4 added, so a test can set user_version
// to 3 or lower and reopen the file as an older release left it.
func rollbackV4(t *testing.T, s *Store) {
	t.Helper()
	rollbackV5(t, s)
	for _, q := range []string{
		"DROP TABLE session_digests",
		"ALTER TABLE sessions DROP COLUMN last_prompt",
		"ALTER TABLE sessions DROP COLUMN last_prompt_at",
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// rollbackV6 removes what schema v6 added, so a test can set user_version
// to 5 or lower and reopen the file as an older release left it.
func rollbackV6(t *testing.T, s *Store) {
	t.Helper()
	rollbackV7(t, s)
	for _, q := range []string{"DROP TABLE inbox_triage", "ALTER TABLE sessions DROP COLUMN turn_ended_at", "ALTER TABLE sessions DROP COLUMN blocked_at"} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// rollbackV7 removes what schema v7 added, so a test can set user_version
// to 6 or lower and reopen the file as an older release left it.
func rollbackV7(t *testing.T, s *Store) {
	t.Helper()
	rollbackV8(t, s)
	if _, err := s.db.Exec("DROP TABLE inbox_alerts"); err != nil {
		t.Fatalf("DROP TABLE inbox_alerts: %v", err)
	}
}

// rollbackV8 removes what schema v8 added, so a test can set user_version
// to 7 or lower and reopen the file as an older release left it.
func rollbackV8(t *testing.T, s *Store) {
	t.Helper()
	rollbackV9(t, s)
	for _, q := range []string{"DROP TABLE instructions", "DROP TABLE messages", "DROP TABLE permission_requests"} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// rollbackV9 removes what schema v9 added, so a test can set user_version
// to 8 or lower and reopen the file as an older release left it.
func rollbackV9(t *testing.T, s *Store) {
	t.Helper()
	rollbackV10(t, s)
	for _, q := range []string{"DROP TABLE moves", "ALTER TABLE machines DROP COLUMN move_key"} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// rollbackV5 removes what schema v5 added, so a test can set user_version
// to 4 or lower and reopen the file as an older release left it.
func rollbackV5(t *testing.T, s *Store) {
	t.Helper()
	rollbackV6(t, s)
	for _, q := range []string{"DROP TABLE login_codes", "DROP TABLE web_sessions"} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// rollbackV10 removes what schema v10 added, so a test can set user_version
// to 9 or lower and reopen the file as an older release left it.
func rollbackV10(t *testing.T, s *Store) {
	t.Helper()
	rollbackV11(t, s)
	if _, err := s.db.Exec("DROP TABLE start_requests"); err != nil {
		t.Fatalf("drop start_requests: %v", err)
	}
}

// rollbackV11 removes what schema v11 added, so a test can set user_version
// to 10 or lower and reopen the file as an older release left it.
func rollbackV11(t *testing.T, s *Store) {
	t.Helper()
	rollbackV12(t, s)
	if _, err := s.db.Exec("ALTER TABLE start_requests DROP COLUMN trust"); err != nil {
		t.Fatalf("drop start_requests.trust: %v", err)
	}
}

// rollbackV12 removes what schema v12 added, so a test can set user_version
// to 11 or lower and reopen the file as an older release left it.
func rollbackV12(t *testing.T, s *Store) {
	t.Helper()
	rollbackV13(t, s)
	for _, c := range []string{"blocked_on", "context_percent", "usage_at", "live_cost_usd", "mod_seen_at"} {
		if _, err := s.db.Exec("ALTER TABLE sessions DROP COLUMN " + c); err != nil {
			t.Fatalf("drop sessions.%s: %v", c, err)
		}
	}
}

// rollbackV13 removes what schema v13 added, so a test can set user_version
// to 12 or lower and reopen the file as an older release left it.
func rollbackV13(t *testing.T, s *Store) {
	t.Helper()
	rollbackV14(t, s)
	for _, tb := range []string{"task_session_ignores", "task_sessions", "task_events", "tasks"} {
		if _, err := s.db.Exec("DROP TABLE " + tb); err != nil {
			t.Fatalf("drop %s: %v", tb, err)
		}
	}
}
