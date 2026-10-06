package store

import (
	"context"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessionhub.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestOpenPragmasAndSchema(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	checks := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"busy_timeout", "5000"},
		{"foreign_keys", "1"},
		{"user_version", "12"},
	}
	for _, c := range checks {
		var got string
		if err := s.db.QueryRowContext(ctx, "PRAGMA "+c.pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("PRAGMA %s = %s, want %s", c.pragma, got, c.want)
		}
	}
	if n := s.db.Stats().MaxOpenConnections; n != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1", n)
	}

	cols := map[string][]string{}
	for _, table := range []string{"machines", "sessions", "events", "reports", "control_requests", "session_digests", "login_codes", "web_sessions", "inbox_triage", "inbox_alerts",
		"instructions", "messages", "permission_requests", "moves", "start_requests"} {
		rows, err := s.db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var c string
			rows.Scan(&c)
			cols[table] = append(cols[table], c)
		}
		rows.Close()
		sort.Strings(cols[table])
	}
	want := map[string]string{
		"machines":            "herdr_host id last_poll last_seen move_key name ssh_host token_hash",
		"moves":               "bundle_size cloud_url created_at detail id requested_by session_id source_closed_at source_machine_id state target target_machine_id updated_at",
		"sessions":            "agent agent_state blocked_at blocked_on context_percent cwd ended_at first_prompt git_branch git_repo herdr_pane herdr_session herdr_workspace id last_prompt last_prompt_at last_seen_at live_cost_usd machine_id mod_seen_at rc_at rc_url started_at state_ts title title_source turn_ended_at usage_at",
		"events":              "id kind payload_json session_id source ts",
		"reports":             "done_json id in_flight_json note session_id ts waiting_on_json",
		"control_requests":    "action claimed_at created_at detail expires_at finished_at id machine_id requested_by session_id state url",
		"session_digests":     "as_of body received_at session_id",
		"login_codes":         "code_hash created_at expires_at machine_id name used_at",
		"inbox_triage":        "session_id snooze_until triaged_since updated_at",
		"inbox_alerts":        "sent_at session_id since",
		"instructions":        "created_at created_by id text",
		"messages":            "created_at detail id limit_key machine_id offered_at sender session_id state text updated_at",
		"permission_requests": "created_at decided_at decided_by decision expires_at id machine_id reason session_id state tool_input tool_name truncated updated_at",
		"web_sessions":        "created_at expires_at id last_used_at machine_id name token_hash",
		"start_requests":      "claimed_at created_at detail dir expires_at finished_at id machine_id prompt requested_by state trust url",
	}
	for table, w := range want {
		if got := strings.Join(cols[table], " "); got != w {
			t.Errorf("%s columns:\n got %s\nwant %s", table, got, w)
		}
	}

	for _, idx := range []string{"messages_machine", "messages_limit", "messages_session", "permission_requests_session",
		"moves_session", "moves_source", "moves_target", "start_requests_machine"} {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", idx).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s: count %d, err %v", idx, n, err)
		}
	}

	// Reopening an existing database keeps its data.
	if _, _, err := s.AddMachine(ctx, "tower", "", ""); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if ms, _ := s2.ListMachines(ctx); len(ms) != 1 || ms[0].SSHHost != "tower" || ms[0].HerdrHost != "tower" {
		t.Errorf("after reopen: %+v", ms)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	s, path := openTemp(t)
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(path, Options{}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("err %v, want a newer-schema error", err)
	}
}

func TestForeignKeys(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, machine_id, started_at, last_seen_at) VALUES ('x', 42, 'a', 'a')`); err == nil {
		t.Error("session with an unknown machine_id was accepted: foreign keys are off")
	}

	// Removing a machine removes its sessions, events, and reports, and
	// leaves other machines' rows alone.
	tokA, _, _ := s.AddMachine(ctx, "tower", "", "")
	tokB, _, _ := s.AddMachine(ctx, "bluebox", "", "")
	a, _ := s.MachineByToken(ctx, tokA)
	b, _ := s.MachineByToken(ctx, tokB)
	for _, x := range []struct {
		m  Machine
		id string
	}{{a, "sess-a"}, {b, "sess-b"}} {
		if _, err := s.UpsertSession(ctx, x.m.ID, api.SessionUpsert{ID: x.id, Source: api.SourceHooks}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddReport(ctx, x.m.ID, x.id, api.ReportIn{Note: "n"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RemoveMachine(ctx, "tower"); err != nil {
		t.Fatal(err)
	}
	count := func(q string) int {
		var n int
		if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM sessions WHERE id = 'sess-a'`) +
		count(`SELECT count(*) FROM events WHERE session_id = 'sess-a'`) +
		count(`SELECT count(*) FROM reports WHERE session_id = 'sess-a'`); n != 0 {
		t.Errorf("%d rows of the removed machine remain", n)
	}
	if n := count(`SELECT count(*) FROM events WHERE session_id = 'sess-b'`) +
		count(`SELECT count(*) FROM reports WHERE session_id = 'sess-b'`); n != 2 {
		t.Errorf("other machine's rows: %d, want 2", n)
	}
}

func TestTimeFormat(t *testing.T) {
	t1 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	t2 := t1.Add(1500 * time.Millisecond)
	s1, s2 := formatTS(t1), formatTS(t2)
	if s1 != "2026-09-30T10:00:00.000000000Z" || s2 != "2026-09-30T10:00:01.500000000Z" {
		t.Errorf("formatTS: %s %s", s1, s2)
	}
	if !(s1 < s2) {
		t.Error("stored times do not sort as strings")
	}
	back, err := parseTS(s2)
	if err != nil || !back.Equal(t2) {
		t.Errorf("round trip: %v %v", back, err)
	}
}

func TestTokens(t *testing.T) {
	re := regexp.MustCompile(`^hub_m_[A-Za-z0-9_-]{43}$`)
	a, _ := NewToken(MachineTokenPrefix)
	b, _ := NewToken(MachineTokenPrefix)
	if !re.MatchString(a) || a == b {
		t.Errorf("tokens %q %q", a, b)
	}
	if r, _ := NewToken(SessionTokenPrefix); !regexp.MustCompile(`^hub_s_[A-Za-z0-9_-]{43}$`).MatchString(r) {
		t.Errorf("session token %q", r)
	}
	if h := HashToken("x"); h != "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881" {
		t.Errorf("HashToken is not SHA-256 hex: %s", h)
	}
}

func TestResumeCommand(t *testing.T) {
	if got := ResumeCommand("", "bluebox", "abc"); got != "ssh -t bluebox '~/.local/bin/sessionhub resume abc'" {
		t.Errorf("empty ssh_host: %q", got)
	}
	if got := ResumeCommand("bluebox.example.com", "bluebox", "abc"); got != "ssh -t bluebox.example.com '~/.local/bin/sessionhub resume abc'" {
		t.Errorf("ssh_host: %q", got)
	}
}

func TestStatus(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)
	ended := now.Add(-time.Hour)
	tests := []struct {
		lastSeen time.Time
		ended    *time.Time
		state    string
		want     string
	}{
		{now, nil, "working", "live"},
		{now, nil, "blocked", "blocked"},
		{now.Add(-5 * time.Minute), nil, "idle", "live"},
		{now.Add(-5*time.Minute - 1), nil, "blocked", "stale"},
		{now, &ended, "blocked", "ended"},
		{now.Add(-time.Hour), &ended, "", "ended"},
	}
	for _, tt := range tests {
		if got := status(now, 5*time.Minute, tt.lastSeen, tt.ended, tt.state); got != tt.want {
			t.Errorf("status(%v, ended=%v, %q) = %s, want %s", tt.lastSeen, tt.ended != nil, tt.state, got, tt.want)
		}
	}
}
