package join

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/store"
)

// env is one isolated join scenario: a temp HOME, a fake bin directory that is
// the whole PATH, and a fake sessionhub server that records /healthz calls.
type env struct {
	t       *testing.T
	home    string
	bin     string
	sshLog  string
	deps    Deps
	out     *bytes.Buffer
	mu      sync.Mutex
	health  []string // Authorization headers seen on /healthz
	srv     *httptest.Server
	calls   []string // installer calls in order
	failFor map[string]error
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, home: t.TempDir(), bin: t.TempDir(), out: &bytes.Buffer{}, failFor: map[string]error{}}
	e.sshLog = filepath.Join(e.home, "ssh.log")
	t.Setenv("HOME", e.home)
	t.Setenv("PATH", e.bin)
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(e.home, "cfg", "config.toml"))
	t.Setenv("SESSIONHUB_SERVER_CONFIG", filepath.Join(e.home, "cfg", "server.toml"))
	t.Setenv("SESSIONHUB_DB", filepath.Join(e.home, "data", "sessionhub.db"))
	t.Setenv("SESSIONHUB_SERVER_URL", "")
	t.Setenv("SESSIONHUB_TOKEN", "")
	t.Setenv("SESSIONHUB_MACHINE", "")
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if r.URL.Path == "/healthz" {
			e.health = append(e.health, r.Header.Get("Authorization"))
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(e.srv.Close)
	rec := func(name string) func(context.Context, []string) error {
		return func(_ context.Context, args []string) error {
			e.calls = append(e.calls, name)
			return e.failFor[name]
		}
	}
	e.deps = Deps{
		Stdout:        e.out,
		InstallPlugin: rec("plugin"),
		InstallHooks:  rec("hooks"),
		InstallMCP:    rec("mcp"),
		Status: func(context.Context, []string) error {
			fmt.Fprintln(e.out, "STATUS-LINE")
			return nil
		},
		MachineAddLocal: machineAddLocal,
		Hostname:        func() (string, error) { return "Laptop.example.com", nil },
	}
	return e
}

func (e *env) script(name, body string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		e.t.Fatal(err)
	}
}

// fakeSSH records its arguments one per line and prints reply on stdout.
func (e *env) fakeSSH(reply string, exit int) {
	e.script("ssh", fmt.Sprintf("for a in \"$@\"; do printf '%%s\\n' \"$a\" >> %q; done\nprintf '%%s\\n' '%s'\nexit %d\n",
		e.sshLog, reply, exit))
}

func (e *env) sshArgs() []string {
	b, err := os.ReadFile(e.sshLog)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (e *env) addReply(name string) string {
	return fmt.Sprintf(`{"name":%q,"token":"hub_m_faketoken","server_url":%q}`, name, e.srv.URL)
}

func (e *env) run(args ...string) error {
	e.out.Reset()
	return RunWith(context.Background(), args, e.deps)
}

func TestJoinOverSSH(t *testing.T) {
	e := newEnv(t)
	e.fakeSSH("Welcome banner\n"+e.addReply("laptop"), 0)
	e.script("herdr", "echo 'herdr 0.9.3'\n")
	e.script("claude", "exit 0\n")

	if _, err := os.Stat(os.Getenv("SESSIONHUB_CONFIG")); err == nil {
		t.Fatal("config exists before join")
	}
	if err := e.run("tower", "--ssh-host", "laptop.lan", "--herdr-host", "laptop.lan"); err != nil {
		t.Fatalf("join: %v\n%s", err, e.out)
	}

	want := []string{"-o", "BatchMode=yes", "--", "tower",
		"~/.local/bin/sessionhub machine add laptop --ssh-host laptop.lan --herdr-host laptop.lan --json"}
	if got := e.sshArgs(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("ssh args = %q, want %q", got, want)
	}
	cfg, err := client.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "hub_m_faketoken" || cfg.Machine != "laptop" || cfg.ServerURL != e.srv.URL {
		t.Errorf("config = %+v", cfg)
	}
	st, err := os.Stat(os.Getenv("SESSIONHUB_CONFIG"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, err %v; want 0600", st.Mode().Perm(), err)
	}
	if got := strings.Join(e.calls, ","); got != "plugin,hooks,mcp" {
		t.Errorf("installers = %s", got)
	}
	if len(e.health) != 1 {
		t.Errorf("health calls = %d, want 1", len(e.health))
	}
	if strings.Contains(e.out.String(), "is ignored") {
		t.Errorf("the ssh path must not print the ignored-target line:\n%s", e.out)
	}
	for _, s := range []string{"STATUS-LINE", "docs/CLAUDE-snippet.md", "[ui.sidebar.agents]", "$hub_summary", "Joined as laptop"} {
		if !strings.Contains(e.out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, e.out)
		}
	}
}

func TestSkipsWhenPrerequisitesMissing(t *testing.T) {
	cases := []struct {
		name     string
		herdr    string // empty: not installed
		claude   bool
		wantCall string
		wantMsg  []string
	}{
		{"no herdr, no claude", "", false, "hooks", []string{"herdr is not on PATH", "claude is not on PATH"}},
		{"old herdr", "herdr 0.9.2", true, "hooks,mcp", []string{"herdr 0.9.2 is older than 0.9.3"}},
		{"newer herdr", "herdr 0.10.0", true, "plugin,hooks,mcp", nil},
		{"garbled herdr", "hello", true, "hooks,mcp", []string{"cannot read the herdr version"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.fakeSSH(e.addReply("laptop"), 0)
			if tc.herdr != "" {
				e.script("herdr", "echo '"+tc.herdr+"'\n")
			}
			if tc.claude {
				e.script("claude", "exit 0\n")
			}
			if err := e.run("tower"); err != nil {
				t.Fatalf("skipping is not a failure: %v\n%s", err, e.out)
			}
			if got := strings.Join(e.calls, ","); got != tc.wantCall {
				t.Errorf("installers = %q, want %q", got, tc.wantCall)
			}
			for _, m := range tc.wantMsg {
				if !strings.Contains(e.out.String(), m) {
					t.Errorf("output lacks %q:\n%s", m, e.out)
				}
			}
		})
	}
}

func TestInstallerFailureIsReportedAndOthersRun(t *testing.T) {
	e := newEnv(t)
	e.fakeSSH(e.addReply("laptop"), 0)
	e.script("herdr", "echo 'herdr 0.9.3'\n")
	e.script("claude", "exit 0\n")
	e.failFor["plugin"] = errors.New("install-plugin: not implemented yet")

	err := e.run("tower")
	if err == nil || !strings.Contains(err.Error(), "install-plugin") {
		t.Fatalf("err = %v, want a failure naming install-plugin", err)
	}
	if got := strings.Join(e.calls, ","); got != "plugin,hooks,mcp" {
		t.Errorf("installers = %s; later steps must still run", got)
	}
	if !strings.Contains(e.out.String(), "FAILED  install-plugin: install-plugin: not implemented yet") {
		t.Errorf("failure not printed:\n%s", e.out)
	}
	if _, err := os.Stat(os.Getenv("SESSIONHUB_CONFIG")); err != nil {
		t.Errorf("config must stay written: %v", err)
	}
}

func TestRejectsUnsafeInputBeforeSSH(t *testing.T) {
	cases := [][]string{
		{"tower", "--name", "a;touch /tmp/pwn"},
		{"tower", "--name", "$(id)"},
		{"tower", "--name", ""},
		{"tower", "--ssh-host", "h; rm -rf ~"},
		{"tower", "--herdr-host", "`id`"},
		{"tower", "--ssh-host", "-oProxyCommand=x"},
		{"-oProxyCommand=x"},
		{"tower; id"},
		{"a", "b"},
		{},
	}
	for _, args := range cases {
		e := newEnv(t)
		e.fakeSSH(e.addReply("laptop"), 0)
		e.deps.Hostname = func() (string, error) { return "bad host", nil }
		if err := e.run(args...); err == nil {
			t.Errorf("args %q: want an error", args)
		}
		if got := e.sshArgs(); got != nil {
			t.Errorf("args %q: ssh ran with %q", args, got)
		}
		if _, err := os.Stat(os.Getenv("SESSIONHUB_CONFIG")); err == nil {
			t.Errorf("args %q: config written", args)
		}
	}
}

func TestSSHFailureAndBadOutput(t *testing.T) {
	for name, tc := range map[string]struct {
		reply string
		exit  int
		want  string
	}{
		"ssh exits 255": {"", 255, "ssh tower failed"},
		"no json":       {"command not found", 0, "no JSON line"},
		"wrong name":    {`{"name":"other","token":"t","server_url":"http://x"}`, 0, "missing name"},
		"empty token":   {`{"name":"laptop","token":"","server_url":"http://x"}`, 0, "missing name"},
		"broken json":   {`{"name":`, 0, "parse machine add output"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.fakeSSH(tc.reply, tc.exit)
			err := e.run("tower")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(os.Getenv("SESSIONHUB_CONFIG")); err == nil {
				t.Error("config written after a failed enrollment")
			}
			if len(e.calls) != 0 {
				t.Errorf("installers ran: %v", e.calls)
			}
		})
	}
}

func TestUnreachableServerFailsHealth(t *testing.T) {
	e := newEnv(t)
	e.fakeSSH(`{"name":"laptop","token":"hub_m_x","server_url":"http://127.0.0.1:1"}`, 0)
	err := e.run("tower")
	if err == nil || !strings.Contains(err.Error(), "health") {
		t.Fatalf("err = %v, want a health failure", err)
	}
	if got := strings.Join(e.calls, ","); got != "hooks" {
		t.Errorf("installers = %q; other steps must still run", got)
	}
}

func TestLocalShortcutAndIdempotence(t *testing.T) {
	e := newEnv(t)
	e.fakeSSH("must not be called", 1)
	// A server config and a database exist here.
	if err := os.MkdirAll(filepath.Dir(os.Getenv("SESSIONHUB_SERVER_CONFIG")), 0o700); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf("public_url = %q\nread_token = \"hub_r_x\"\n", e.srv.URL)
	if err := os.WriteFile(os.Getenv("SESSIONHUB_SERVER_CONFIG"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(os.Getenv("SESSIONHUB_DB"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := e.run("tower", "--name", "local1"); err != nil {
		t.Fatalf("join: %v\n%s", err, e.out)
	}
	if got := e.sshArgs(); got != nil {
		t.Fatalf("ssh ran: %q", got)
	}
	cfg1, err := client.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cfg1.Token, "hub_m_") || cfg1.Machine != "local1" || cfg1.ServerURL != e.srv.URL {
		t.Fatalf("config = %+v", cfg1)
	}
	ms, err := st.ListMachines(context.Background())
	if err != nil || len(ms) != 1 || ms[0].Name != "local1" {
		t.Fatalf("machines = %+v, err %v", ms, err)
	}
	if !strings.Contains(e.out.String(), "The ssh target \"tower\" is ignored.") {
		t.Errorf("local shortcut must say the target is ignored:\n%s", e.out)
	}
	if !strings.Contains(e.out.String(), "adding machine \"local1\" locally") {
		t.Errorf("output:\n%s", e.out)
	}

	// Second run: same machine, rotated token, installers run again.
	e.calls = nil
	if err := e.run("tower", "--name", "local1"); err != nil {
		t.Fatalf("rejoin: %v\n%s", err, e.out)
	}
	cfg2, _ := client.LoadConfig()
	if cfg2.Token == cfg1.Token {
		t.Error("token was not rotated")
	}
	if ms, _ = st.ListMachines(context.Background()); len(ms) != 1 {
		t.Errorf("machines after rejoin = %d, want 1", len(ms))
	}
	if got := strings.Join(e.calls, ","); got != "hooks" {
		t.Errorf("installers on rejoin = %q", got)
	}
}

func TestLocalShortcutNeedsBothFiles(t *testing.T) {
	e := newEnv(t)
	e.fakeSSH(e.addReply("laptop"), 0)
	// Only the database exists: fall back to ssh.
	if err := os.MkdirAll(filepath.Dir(os.Getenv("SESSIONHUB_DB")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("SESSIONHUB_DB"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.run("tower"); err != nil {
		t.Fatalf("join: %v\n%s", err, e.out)
	}
	if e.sshArgs() == nil {
		t.Error("ssh was not used")
	}
}

func TestAtLeast(t *testing.T) {
	for _, tc := range []struct {
		v    [3]int
		want bool
	}{{[3]int{0, 9, 3}, true}, {[3]int{0, 9, 2}, false}, {[3]int{0, 10, 0}, true}, {[3]int{1, 0, 0}, true}, {[3]int{0, 8, 9}, false}} {
		if got := atLeast(tc.v, MinHerdr); got != tc.want {
			t.Errorf("atLeast(%v) = %v", tc.v, got)
		}
	}
}

// The join output carries the whole CLAUDE.md snippet, and the embedded copy
// is the same file as docs/CLAUDE-snippet.md.
func TestSnippetIsPrintedInFullAndMatchesDocs(t *testing.T) {
	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "CLAUDE-snippet.md"))
	if err != nil {
		t.Fatal(err)
	}
	if claudeSnippet != string(docs) {
		t.Fatal("internal/join/CLAUDE-snippet.md differs from docs/CLAUDE-snippet.md; copy the docs file over it")
	}
	var buf bytes.Buffer
	printSnippets(&buf)
	if !strings.Contains(buf.String(), string(docs)) {
		t.Errorf("output lacks the full snippet:\n%s", buf.String())
	}
	if strings.Count(buf.String(), "--- begin CLAUDE.md snippet ---") != 1 || strings.Count(buf.String(), "--- end CLAUDE.md snippet ---") != 1 {
		t.Errorf("snippet markers missing or repeated:\n%s", buf.String())
	}
}
