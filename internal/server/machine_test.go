package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

// machineEnv points SESSIONHUB_DB and SESSIONHUB_SERVER_CONFIG at a temp dir.
func machineEnv(t *testing.T) (dbPath string) {
	t.Helper()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "data", "sessionhub.db")
	t.Setenv("SESSIONHUB_DB", dbPath)
	t.Setenv("SESSIONHUB_SERVER_CONFIG", filepath.Join(dir, "server.toml")) // absent: defaults
	t.Setenv("SESSIONHUB_PUBLIC_URL", "https://sessionhub.example.test/")
	t.Setenv("SESSIONHUB_READ_TOKEN", "")
	t.Setenv("SESSIONHUB_LISTEN", "")
	t.Setenv("SESSIONHUB_STALE_AFTER", "")
	return dbPath
}

func machineCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runMachine(context.Background(), args, &out)
	return out.String(), err
}

// tokenWorks opens the database the way the server does and checks a token.
func tokenWorks(t *testing.T, dbPath, token string) (string, bool) {
	t.Helper()
	st, err := store.Open(dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m, err := st.MachineByToken(context.Background(), token)
	if errors.Is(err, store.ErrNotFound) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return m.Name, true
}

var tokenLineRE = regexp.MustCompile(`(?m)^token: (hub_m_[A-Za-z0-9_-]{43})$`)

func TestMachineAddRotateRemove(t *testing.T) {
	db := machineEnv(t)

	out, err := machineCmd(t, "ls")
	if err != nil || !strings.Contains(out, "no machines") {
		t.Fatalf("ls on empty db: %q %v", out, err)
	}

	out, err = machineCmd(t, "add", "tower", "--ssh-host", "tower.example.com", "--herdr-host", "tower.example.com")
	if err != nil {
		t.Fatal(err)
	}
	m := tokenLineRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no token line in %q", out)
	}
	tok1 := m[1]
	if !strings.Contains(out, "added machine tower (ssh_host tower.example.com, herdr_host tower.example.com)") ||
		!strings.Contains(out, "server_url: https://sessionhub.example.test\n") {
		t.Errorf("add output: %q", out)
	}
	if name, ok := tokenWorks(t, db, tok1); !ok || name != "tower" {
		t.Errorf("new token does not authenticate as tower")
	}

	// Only the hash is stored: the token appears nowhere in the database files.
	for _, f := range []string{db, db + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(tok1)) {
			t.Errorf("%s contains the plaintext token", f)
		}
		if f == db && !bytes.Contains(b, []byte(store.HashToken(tok1))) && !fileContains(t, db+"-wal", store.HashToken(tok1)) {
			t.Errorf("token hash not found in the database")
		}
	}
	if fi, err := os.Stat(filepath.Dir(db)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("database dir mode: %v %v", fi.Mode(), err)
	}

	// Rerunning add rotates: new token works, old one does not, and hosts set
	// earlier are kept when no flags are given.
	out, err = machineCmd(t, "add", "tower", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("--json must print exactly one line, got %q", out)
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("--json output %q: %v", out, err)
	}
	if len(raw) != 3 || raw["name"] != "tower" || raw["server_url"] != "https://sessionhub.example.test" ||
		!regexp.MustCompile(`^hub_m_[A-Za-z0-9_-]{43}$`).MatchString(raw["token"]) {
		t.Errorf("--json output: %v", raw)
	}
	tok2 := raw["token"]
	if tok2 == tok1 {
		t.Fatal("rotation returned the same token")
	}
	if _, ok := tokenWorks(t, db, tok1); ok {
		t.Error("old token still works after rotation")
	}
	if _, ok := tokenWorks(t, db, tok2); !ok {
		t.Error("rotated token does not work")
	}

	// Defaults: a new machine's hosts are its name.
	if _, err := machineCmd(t, "add", "--json", "bluebox"); err != nil {
		t.Fatal(err)
	}
	out, err = machineCmd(t, "ls")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	want := []*regexp.Regexp{
		regexp.MustCompile(`^NAME\s+SSH_HOST\s+HERDR_HOST\s+LAST_SEEN\s+MOVE_KEY$`),
		regexp.MustCompile(`^bluebox\s+bluebox\s+bluebox\s+never\s+-$`),
		regexp.MustCompile(`^tower\s+tower\.example\.com\s+tower\.example\.com\s+\d{4}-\d\d-\d\dT`), // seen by tokenWorks
	}
	if len(lines) != len(want) {
		t.Fatalf("ls output: %q", out)
	}
	for i, re := range want {
		if !re.MatchString(lines[i]) {
			t.Errorf("ls line %d = %q, want %s", i, lines[i], re)
		}
	}

	// Give tower data, then rm without --yes: refused, counts printed, nothing deleted.
	seedMachineData(t, db, tok2)
	if out, err = machineCmd(t, "rm", "tower"); err == nil || out != "" ||
		!strings.Contains(err.Error(), "deletes 2 sessions, 3 events, and 1 reports") ||
		!strings.Contains(err.Error(), "--yes") {
		t.Fatalf("rm without --yes: %q %v", out, err)
	}
	if _, ok := tokenWorks(t, db, tok2); !ok {
		t.Fatal("rm without --yes revoked the token")
	}
	if out, _ = machineCmd(t, "ls"); !strings.Contains(out, "tower") {
		t.Fatalf("rm without --yes removed the machine: %q", out)
	}
	if got := sessionCount(t, db); got != 2 {
		t.Fatalf("rm without --yes left %d sessions, want 2", got)
	}

	// rm --yes revokes the token and removes the machine and its data.
	if out, err = machineCmd(t, "rm", "tower", "--yes"); err != nil || !strings.Contains(out, "removed machine tower") {
		t.Fatalf("rm: %q %v", out, err)
	}
	if _, ok := tokenWorks(t, db, tok2); ok {
		t.Error("token works after rm")
	}
	out, _ = machineCmd(t, "ls")
	if strings.Contains(out, "tower") {
		t.Errorf("tower still listed: %q", out)
	}
	if got := sessionCount(t, db); got != 0 {
		t.Errorf("%d sessions left after rm --yes", got)
	}
	for _, args := range [][]string{{"rm", "tower"}, {"rm", "tower", "--yes"}} {
		if _, err := machineCmd(t, args...); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%v of a missing machine: %v", args, err)
		}
	}
}

// seedMachineData gives the machine that owns token 2 sessions, 3 events
// (2 registered plus 1 prompt), and 1 report.
func seedMachineData(t *testing.T, db, token string) {
	t.Helper()
	st, err := store.Open(db, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	m, err := st.MachineByToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{sid1, sid2} {
		if _, err := st.UpsertSession(ctx, m.ID, api.SessionUpsert{ID: id, Agent: "claude", Source: api.SourceHooks}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddEvent(ctx, m.ID, sid1, api.EventIn{Kind: api.KindPrompt, Source: api.SourceHooks}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddReport(ctx, m.ID, sid1, api.ReportIn{Done: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
}

func sessionCount(t *testing.T, db string) int {
	t.Helper()
	st, err := store.Open(db, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ss, err := st.ListSessions(context.Background(), store.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return len(ss)
}

func fileContains(t *testing.T, path, s string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Contains(b, []byte(s))
}

func TestMachineArgErrors(t *testing.T) {
	db := machineEnv(t)
	tests := []struct {
		args []string
		want string
	}{
		{nil, "usage"},
		{[]string{"frob"}, "unknown subcommand"},
		{[]string{"frob", "--json"}, "unknown subcommand"},
		{[]string{"add"}, "want 1 argument"},
		{[]string{"add", "a", "b"}, "want 1 argument"},
		{[]string{"add", "bad name"}, "machine name"},
		{[]string{"add", "-x"}, "flag provided but not defined"},
		{[]string{"add", "ok", "--ssh-host", "evil;rm -rf"}, "not allowed"},
		{[]string{"add", "ok", "--ssh-host", "-oProxyCommand=x"}, "not allowed"},
		{[]string{"add", "ok", "--herdr-host", "-x"}, "not allowed"},
		{[]string{"add", "ok", "--ssh-host", "a/b"}, "not allowed"},
		{[]string{"add", "ok", "--ssh-host", "[::1]"}, "not allowed"},
		{[]string{"add", "ok", "--ssh-host", "h\x07"}, "not allowed"},
		{[]string{"add", "ok", "--ssh-host", "h\nx"}, "not allowed"},
		{[]string{"add", "ok", "--herdr-host", strings.Repeat("a", 256)}, "not allowed"},
		{[]string{"rm"}, "want 1 argument"},
		{[]string{"rm", "x", "--json"}, "apply to add only"},
		{[]string{"add", "x", "--yes"}, "--yes applies to rm only"},
		{[]string{"ls", "--yes"}, "--yes applies to rm only"},
		{[]string{"ls", "extra"}, "want 0 argument"},
	}
	for _, tt := range tests {
		out, err := machineCmd(t, tt.args...)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("machine %v: err %v, want it to contain %q", tt.args, err, tt.want)
		}
		if out != "" {
			t.Errorf("machine %v printed %q on failure", tt.args, out)
		}
	}
	// None of the failures created a machine.
	st, err := store.Open(db, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if ms, _ := st.ListMachines(context.Background()); len(ms) != 0 {
		t.Errorf("failed commands created machines: %+v", ms)
	}
}

func TestMachineHostValidation(t *testing.T) {
	machineEnv(t)
	for i, h := range []string{"tower.example.com", "user@bluebox.example.com", "host:2222", strings.Repeat("a", 255)} {
		name := "m" + string(rune('a'+i))
		if _, err := machineCmd(t, "add", name, "--ssh-host", h, "--herdr-host", h); err != nil {
			t.Fatalf("host %q refused: %v", h, err)
		}
		out, err := machineCmd(t, "ls")
		if err != nil || !strings.Contains(out, h) {
			t.Errorf("host %q not listed after add: %q %v", h, out, err)
		}
	}
}
