package claudemod

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// wantFiles is what the binary carries: no tests, type declarations,
// tsconfig.json, or .gitignore.
var wantFiles = []string{".claude-plugin/plugin.json", "hooks/config.js", "hooks/hooks.json", "hooks/register.js"}

type fixture struct {
	home, settings, bin, dir string
	out                      bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("SESSIONHUB_STATE_DIR", filepath.Join(home, "state"))
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(home, "config.toml"))
	f := &fixture{
		home:     home,
		settings: filepath.Join(home, ".claude", "settings.json"),
		bin:      filepath.Join(home, ".local", "bin", "sessionhub"),
		dir:      filepath.Join(home, ".local", "share", "sessionhub", "claude-mod"),
	}
	if err := os.MkdirAll(filepath.Dir(f.bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) opts() opts {
	f.out.Reset()
	return opts{settings: f.settings, binary: f.bin, dir: f.dir, out: &f.out, now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
}

func TestFilesAreTheModOnly(t *testing.T) {
	files, err := Files("/opt/sessionhub")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for p := range files {
		got = append(got, p)
	}
	sort.Strings(got)
	if strings.Join(got, " ") != strings.Join(wantFiles, " ") {
		t.Errorf("embedded files %v, want %v", got, wantFiles)
	}
}

func TestFilesFillInTheBinary(t *testing.T) {
	for _, bin := range []string{"/home/u/.local/bin/sessionhub", `/odd "dir"\x/sessionhub`} {
		files, err := Files(bin)
		if err != nil {
			t.Fatal(err)
		}
		cfg := string(files[configFile])
		if strings.Contains(cfg, "__SESSIONHUB_BIN__") {
			t.Errorf("placeholder left in:\n%s", cfg)
		}
		// The JS string literal is JSON, so decoding it gives the path back.
		i := strings.Index(cfg, "SESSIONHUB_BIN = ")
		if i < 0 {
			t.Fatalf("no SESSIONHUB_BIN in:\n%s", cfg)
		}
		lit := strings.TrimSpace(strings.SplitN(cfg[i+len("SESSIONHUB_BIN = "):], "\n", 2)[0])
		var got string
		if err := json.Unmarshal([]byte(lit), &got); err != nil || got != bin {
			t.Errorf("SESSIONHUB_BIN literal %s decodes to %q (%v), want %q", lit, got, err, bin)
		}
	}
}

func TestInstallAndUninstall(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.settings), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := "{\n  \"env\": {\n    \"CLAUDE_CODE_PLUGIN_DIRS\": \"/other\"\n  }\n}\n"
	if err := os.WriteFile(f.settings, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := install(f.opts()); err != nil {
		t.Fatal(err)
	}
	for _, p := range wantFiles {
		st, err := os.Stat(filepath.Join(f.dir, p))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if st.Mode().Perm() != 0o644 {
			t.Errorf("%s: mode %v", p, st.Mode().Perm())
		}
	}
	if st, _ := os.Stat(f.dir); st.Mode().Perm() != 0o755 {
		t.Errorf("dir mode %v", st.Mode().Perm())
	}
	cfg, _ := os.ReadFile(filepath.Join(f.dir, configFile))
	if !strings.Contains(string(cfg), `"`+f.bin+`"`) {
		t.Errorf("config.js does not name %s:\n%s", f.bin, cfg)
	}
	settings, _ := os.ReadFile(f.settings)
	if !strings.Contains(string(settings), `"/other:`+f.dir+`"`) {
		t.Errorf("settings:\n%s", settings)
	}
	backups, _ := filepath.Glob(f.settings + ".sessionhub-backup-*")
	if len(backups) != 1 {
		t.Errorf("backups %v", backups)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(f.dir), ".claude-mod-*"))
	if len(leftovers) != 0 {
		t.Errorf("left over: %v", leftovers)
	}

	// A second install changes nothing.
	if err := install(f.opts()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "up to date") || !strings.Contains(f.out.String(), "already lists") {
		t.Errorf("second install said:\n%s", f.out.String())
	}
	if b, _ := filepath.Glob(f.settings + ".sessionhub-backup-*"); len(b) != 1 {
		t.Errorf("second install made a backup: %v", b)
	}

	// A changed or extra file is replaced by a fresh copy.
	if err := os.WriteFile(filepath.Join(f.dir, "hooks", "register.js"), []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "stray.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := install(f.opts()); err != nil {
		t.Fatal(err)
	}
	if !sameFiles(f.dir, mustFiles(t, f.bin)) {
		t.Error("the reinstall did not restore the files")
	}

	if err := uninstall(f.opts()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.dir); !os.IsNotExist(err) {
		t.Errorf("mod dir still there: %v", err)
	}
	if b, _ := os.ReadFile(f.settings); string(b) != orig {
		t.Errorf("settings after uninstall:\n%s\nwant\n%s", b, orig)
	}
	if err := uninstall(f.opts()); err != nil {
		t.Errorf("second uninstall: %v", err)
	}
}

func mustFiles(t *testing.T, bin string) map[string][]byte {
	t.Helper()
	files, err := Files(bin)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// A settings edit that fails after the new files are in place puts the
// previous mod back.
func TestInstallRestoresOnSettingsFailure(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.dir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "hooks", "register.js"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.settings, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := install(f.opts()); err == nil {
		t.Fatal("install succeeded with bad settings")
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, "hooks", "register.js")); string(b) != "old" {
		t.Errorf("register.js = %q, want the old copy", b)
	}
	if _, err := os.Stat(filepath.Join(f.dir, ".claude-plugin")); !os.IsNotExist(err) {
		t.Errorf("new files left: %v", err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(f.dir), ".claude-mod-*"))
	if len(leftovers) != 0 {
		t.Errorf("left over: %v", leftovers)
	}

	// With no previous mod, the new one is removed.
	if err := os.RemoveAll(f.dir); err != nil {
		t.Fatal(err)
	}
	if err := install(f.opts()); err == nil {
		t.Fatal("install succeeded with bad settings")
	}
	if _, err := os.Stat(f.dir); !os.IsNotExist(err) {
		t.Errorf("mod dir left after a failed install: %v", err)
	}
}

func TestInstallNeedsTheBinary(t *testing.T) {
	f := newFixture(t)
	o := f.opts()
	o.binary = filepath.Join(f.home, "missing", "sessionhub")
	if err := install(o); err == nil || !strings.Contains(err.Error(), "install it there first") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(f.dir); !os.IsNotExist(err) {
		t.Errorf("mod written without a binary: %v", err)
	}
	if _, err := os.Stat(f.settings); !os.IsNotExist(err) {
		t.Errorf("settings written without a binary: %v", err)
	}
}

func TestRunInstallFlags(t *testing.T) {
	newFixture(t)
	if err := RunInstall(context.Background(), []string{"--bogus"}); err == nil {
		t.Error("unknown flag accepted")
	}
	if err := RunInstall(context.Background(), []string{"--binary", "rel/sessionhub"}); err == nil {
		t.Error("relative binary accepted")
	}
	if err := RunUninstall(context.Background(), []string{"extra"}); err == nil {
		t.Error("extra argument accepted")
	}
}

// The installed mod passes `claude plugin validate`, when claude is on PATH.
func TestInstalledModValidates(t *testing.T) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not on PATH")
	}
	f := newFixture(t)
	if err := install(f.opts()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, "plugin", "validate", f.dir)
	cmd.Dir = f.home
	cmd.Env = append(os.Environ(), "HOME="+f.home, "CLAUDE_CONFIG_DIR="+filepath.Join(f.home, ".claude"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("claude plugin validate: %v\n%s", err, out)
	}
}
