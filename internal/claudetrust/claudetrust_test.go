package claudetrust

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPath(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := Path(getenv, "/home/me"); got != "/home/me/.claude.json" {
		t.Errorf("default: %q", got)
	}
	env["CLAUDE_CONFIG_DIR"] = "/cfg"
	if got := Path(getenv, "/home/me"); got != "/cfg/.claude.json" {
		t.Errorf("CLAUDE_CONFIG_DIR: %q", got)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTrusted(t *testing.T) {
	p := writeConfig(t, `{"projects": {
		"/home/me/Code/app": {"hasTrustDialogAccepted": true},
		"/home/me/Code/new": {"hasTrustDialogAccepted": false},
		"/home/me": {"hasTrustDialogAccepted": false, "allowedTools": []},
		"/srv": {"hasTrustDialogAccepted": true},
		"/home/me/odd": {"hasTrustDialogAccepted": "yes"}
	}}`)
	for _, tc := range []struct {
		dir  string
		want bool
	}{
		{"/home/me/Code/app", true},
		{"/home/me/Code/app/", true},
		{"/home/me/Code/app/sub/dir", true}, // a folder above it is trusted
		{"/home/me/Code/new", false},
		{"/home/me/Code", false},
		{"/home/me", false},
		{"/home/me/Code/application", false}, // a prefix is not a parent
		{"/srv/www", true},
		{"/home/me/odd", false},
	} {
		got, err := Trusted(p, tc.dir)
		if err != nil || got != tc.want {
			t.Errorf("Trusted(%q) = %v, %v; want %v", tc.dir, got, err, tc.want)
		}
	}
	if _, err := Trusted(p, "Code/app"); err == nil {
		t.Error("relative dir: no error")
	}
	if got, err := Trusted(filepath.Join(t.TempDir(), "none.json"), "/srv"); got || err != nil {
		t.Errorf("missing config: %v, %v", got, err)
	}
	if _, err := Trusted(writeConfig(t, "{not json"), "/srv"); err == nil {
		t.Error("bad JSON: no error")
	}
	if got, err := Trusted(writeConfig(t, `{"numStartups": 3}`), "/srv"); got || err != nil {
		t.Errorf("no projects: %v, %v", got, err)
	}
}

func TestTrustKeepsOtherKeys(t *testing.T) {
	p := writeConfig(t, `{
  "numStartups": 12345678901234567890,
  "theme": "dark",
  "projects": {
    "/home/me/Code/app": {"allowedTools": ["Bash"], "hasTrustDialogAccepted": false, "lastCost": 1.5},
    "/home/me/Code/other": {"hasTrustDialogAccepted": true}
  },
  "oauthAccount": {"emailAddress": "me@example.com"}
}`)
	if err := Trust(p, "/home/me/Code/app", "/home/me"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	var cfg map[string]any
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	if err := d.Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["numStartups"].(json.Number).String() != "12345678901234567890" || cfg["theme"] != "dark" {
		t.Errorf("top-level keys changed: %s", b)
	}
	if cfg["oauthAccount"].(map[string]any)["emailAddress"] != "me@example.com" {
		t.Errorf("oauthAccount changed: %s", b)
	}
	projects := cfg["projects"].(map[string]any)
	app := projects["/home/me/Code/app"].(map[string]any)
	if app["hasTrustDialogAccepted"] != true || app["lastCost"].(json.Number).String() != "1.5" {
		t.Errorf("app project: %v", app)
	}
	if tools := app["allowedTools"].([]any); len(tools) != 1 || tools[0] != "Bash" {
		t.Errorf("allowedTools: %v", tools)
	}
	if len(projects) != 2 {
		t.Errorf("projects: %v", projects)
	}
	if ok, _ := Trusted(p, "/home/me/Code/app"); !ok {
		t.Error("not trusted after Trust")
	}
	if ok, _ := Trusted(p, "/home/me/Code"); ok {
		t.Error("Trust trusted the parent")
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640", st.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".claude.json.sessionhub-*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestTrustNewProjectAndNewFile(t *testing.T) {
	p := writeConfig(t, `{"theme": "light"}`)
	if err := Trust(p, "/home/me/x", "/home/me"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := Trusted(p, "/home/me/x"); !ok {
		t.Error("new project not trusted")
	}
	missing := filepath.Join(t.TempDir(), ".claude.json")
	if err := Trust(missing, "/home/me/x", "/home/me"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := Trusted(missing, "/home/me/x"); !ok {
		t.Error("created config doesn't trust it")
	}
	if st, _ := os.Stat(missing); st.Mode().Perm() != 0o600 {
		t.Errorf("new file mode %v, want 0600", st.Mode().Perm())
	}
}

func TestTrustRefuses(t *testing.T) {
	const body = `{"projects": {}}`
	p := writeConfig(t, body)
	for _, dir := range []string{
		"Code/app",       // relative
		"./app",          // relative
		"/home/me/../me", // unclean
		"/home/me/app/",  // unclean
		"/home/me",       // home
		"/home",          // above home
		"/",              // root
		"",
	} {
		if err := Trust(p, dir, "/home/me"); err == nil {
			t.Errorf("Trust(%q): no error", dir)
		}
	}
	if err := Trust(p, "/srv/app", ""); err == nil {
		t.Error("no home: no error")
	}
	if b, _ := os.ReadFile(p); string(b) != body {
		t.Errorf("config changed: %s", b)
	}
	bad := writeConfig(t, `{"projects": [1]}`)
	if err := Trust(bad, "/home/me/x", "/home/me"); err == nil {
		t.Error("projects not an object: no error")
	}
	if err := Trust(writeConfig(t, "{oops"), "/home/me/x", "/home/me"); err == nil {
		t.Error("bad JSON: no error")
	}
}

func TestTrustThroughSymlink(t *testing.T) {
	real := writeConfig(t, `{"projects": {}}`)
	link := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := Trust(link, "/home/me/x", "/home/me"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v %v", st, err)
	}
	if ok, _ := Trusted(real, "/home/me/x"); !ok {
		t.Error("target not updated")
	}
}

func TestTrustRetriesWhenClaudeWrites(t *testing.T) {
	p := writeConfig(t, `{"projects": {}}`)
	writes := 0
	beforeRename = func(path string) {
		if writes++; writes == 1 {
			// Claude rewrites the file between Trust's read and its rename.
			if err := os.WriteFile(path, []byte(`{"projects": {}, "numStartups": 7}`), 0o640); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { beforeRename = func(string) {} })
	if err := Trust(p, "/home/me/x", "/home/me"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if writes != 2 || !strings.Contains(string(b), `"numStartups": 7`) {
		t.Errorf("after %d attempts: %s", writes, b)
	}
	if ok, _ := Trusted(p, "/home/me/x"); !ok {
		t.Error("not trusted")
	}

	// A file that never settles: give up, write nothing.
	n := 0
	beforeRename = func(path string) {
		n++
		_ = os.WriteFile(path, []byte(strings.Repeat(" ", n)+`{"projects": {}}`), 0o640)
	}
	if err := Trust(p, "/home/me/y", "/home/me"); err == nil || !strings.Contains(err.Error(), "kept changing") {
		t.Errorf("err %v", err)
	}
	if ok, _ := Trusted(p, "/home/me/y"); ok {
		t.Error("trusted after giving up")
	}
}
