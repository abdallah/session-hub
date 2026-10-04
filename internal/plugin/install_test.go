package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/abdallah/session-hub/internal/paths"
)

type manifestDoc struct {
	ID              string   `toml:"id"`
	Name            string   `toml:"name"`
	Version         string   `toml:"version"`
	MinHerdrVersion string   `toml:"min_herdr_version"`
	Description     string   `toml:"description"`
	Platforms       []string `toml:"platforms"`
	Startup         []struct {
		Command []string `toml:"command"`
	} `toml:"startup"`
	Events []struct {
		On      string   `toml:"on"`
		Command []string `toml:"command"`
	} `toml:"events"`
	Actions []struct {
		ID       string   `toml:"id"`
		Title    string   `toml:"title"`
		Contexts []string `toml:"contexts"`
		Command  []string `toml:"command"`
	} `toml:"actions"`
	Panes []struct {
		ID        string   `toml:"id"`
		Title     string   `toml:"title"`
		Placement string   `toml:"placement"`
		Command   []string `toml:"command"`
	} `toml:"panes"`
}

func TestManifestMatchesPlan(t *testing.T) {
	bin := "/home/me/.local/bin/sessionhub"
	var m manifestDoc
	md, err := toml.Decode(Manifest(bin), &m)
	if err != nil {
		t.Fatal(err)
	}
	if u := md.Undecoded(); len(u) != 0 {
		t.Errorf("keys not in the test struct: %v", u)
	}
	if m.ID != "sessionhub" || m.Name != "sessionhub" || m.Version != "0.1.0" || m.MinHerdrVersion != "0.9.3" ||
		m.Description != "Register the Claude Code sessions in herdr panes with sessionhub." || strings.Join(m.Platforms, ",") != "linux,macos" {
		t.Errorf("header %+v", m)
	}
	cmd := func(c []string) string { return strings.Join(c, " ") }
	if len(m.Startup) != 1 || cmd(m.Startup[0].Command) != bin+" plugin startup" {
		t.Errorf("startup %+v", m.Startup)
	}
	var on []string
	for _, e := range m.Events {
		on = append(on, e.On)
		if cmd(e.Command) != bin+" plugin event" {
			t.Errorf("event %s command %v", e.On, e.Command)
		}
	}
	if strings.Join(on, " ") != "pane.agent_detected pane.agent_status_changed pane.closed pane.exited" {
		t.Errorf("events %v", on)
	}
	if len(m.Actions) != 3 ||
		m.Actions[0].ID != "status" || m.Actions[0].Title != "sessionhub: sessions on this machine" || cmd(m.Actions[0].Command) != bin+" status" ||
		m.Actions[1].ID != "resume" || m.Actions[1].Title != "sessionhub: resume a session" || cmd(m.Actions[1].Command) != bin+" plugin open-picker" ||
		m.Actions[2].ID != "inbox" || m.Actions[2].Title != "sessionhub: inbox" || cmd(m.Actions[2].Command) != bin+" plugin open-inbox" ||
		cmd(m.Actions[0].Contexts) != "global" || cmd(m.Actions[1].Contexts) != "global" || cmd(m.Actions[2].Contexts) != "global" {
		t.Errorf("actions %+v", m.Actions)
	}
	if len(m.Panes) != 2 || m.Panes[0].ID != "resume-picker" || m.Panes[0].Title != "sessionhub: resume" ||
		m.Panes[0].Placement != "overlay" || cmd(m.Panes[0].Command) != bin+" resume --pick" ||
		m.Panes[1].ID != "inbox" || m.Panes[1].Title != "sessionhub: inbox" ||
		m.Panes[1].Placement != "split" || cmd(m.Panes[1].Command) != bin+" inbox --watch" {
		t.Errorf("panes %+v", m.Panes)
	}
	// A path with a quote and a space survives TOML quoting.
	odd := `/tmp/a "b"/sessionhub`
	if _, err := toml.Decode(Manifest(odd), &m); err != nil || m.Startup[0].Command[0] != odd {
		t.Errorf("odd path: %v %v", err, m.Startup[0].Command)
	}
}

// fakeHerdrCLI records herdr CLI calls and keeps a linked-plugin registry.
type fakeHerdrCLI struct {
	calls  []string
	linked map[string]string // id → root
	fail   string            // "link" or "unlink": that subcommand fails
}

func (f *fakeHerdrCLI) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if len(args) >= 2 && args[1] == f.fail {
		return nil, errors.New("herdr plugin " + f.fail + ": forced failure")
	}
	switch {
	case len(args) >= 2 && args[0] == "plugin" && args[1] == "list":
		var ps []map[string]string
		if root, ok := f.linked[PluginID]; ok {
			ps = append(ps, map[string]string{"plugin_id": PluginID, "plugin_root": root, "manifest_path": filepath.Join(root, ManifestFile)})
		}
		b, _ := json.Marshal(map[string]any{"id": "cli:plugin", "result": map[string]any{"plugins": ps, "type": "plugin_list"}})
		return b, nil
	case len(args) == 3 && args[1] == "link":
		f.linked[PluginID] = args[2]
	case len(args) == 3 && args[1] == "unlink":
		delete(f.linked, args[2])
	}
	return nil, nil
}

func TestInstallIdempotentAndUninstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	f := &fakeHerdrCLI{linked: map[string]string{}}
	herdrRunner = f.run
	defer func() { herdrRunner = defaultHerdrRunner }()
	ctx := context.Background()
	dir := paths.PluginDir()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("plugin dir exists before install: %v", err)
	}

	steps := []struct {
		name      string
		bin       string
		wantCalls string
		wantLink  bool
	}{
		{"first install", "/opt/sessionhub", "plugin list --json --plugin sessionhub|plugin link " + dir, true},
		{"again, unchanged", "/opt/sessionhub", "plugin list --json --plugin sessionhub", false},
		{"new binary path", "/usr/local/bin/sessionhub", "plugin list --json --plugin sessionhub|plugin unlink sessionhub|plugin link " + dir, true},
	}
	for _, s := range steps {
		f.calls = nil
		linked, err := install(ctx, s.bin, io.Discard)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if got := strings.Join(f.calls, "|"); got != s.wantCalls || linked != s.wantLink {
			t.Errorf("%s: calls %q linked=%v, want %q %v", s.name, got, linked, s.wantCalls, s.wantLink)
		}
		b, err := os.ReadFile(filepath.Join(dir, ManifestFile))
		if err != nil || string(b) != Manifest(s.bin) {
			t.Errorf("%s: manifest on disk differs (%v)", s.name, err)
		}
	}
	// Linked from somewhere else (for example a manual link): relinked here.
	f.linked[PluginID] = "/elsewhere"
	f.calls = nil
	if linked, err := install(ctx, "/usr/local/bin/sessionhub", io.Discard); err != nil || !linked {
		t.Errorf("relink from other root: linked=%v err=%v calls=%v", linked, err, f.calls)
	}

	f.calls = nil
	if err := uninstall(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, "|"); got != "plugin list --json --plugin sessionhub|plugin unlink sessionhub" {
		t.Errorf("uninstall calls %q", got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("plugin dir still there: %v", err)
	}
	if _, ok := f.linked[PluginID]; ok {
		t.Error("still linked")
	}
	// Uninstall again: nothing to do, no error.
	f.calls = nil
	if err := uninstall(ctx, io.Discard); err != nil || strings.Join(f.calls, "|") != "plugin list --json --plugin sessionhub" {
		t.Errorf("second uninstall: %v %v", err, f.calls)
	}
}

// The manifest names ~/.local/bin/sessionhub, not the running test binary. Without
// that file, install fails with a pointer to `make install`, and writes and
// links nothing.
func TestInstallUsesInstalledBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	f := &fakeHerdrCLI{linked: map[string]string{}}
	herdrRunner = f.run
	defer func() { herdrRunner = defaultHerdrRunner }()
	ctx := context.Background()
	bin := filepath.Join(home, ".local", "bin", "sessionhub")
	manifest := filepath.Join(paths.PluginDir(), ManifestFile)

	_, err := installManifest(ctx, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "make install") || !strings.Contains(err.Error(), bin) {
		t.Fatalf("missing binary: err = %v, want one naming %s and `make install`", err, bin)
	}
	if _, serr := os.Stat(manifest); !os.IsNotExist(serr) || len(f.calls) != 0 {
		t.Errorf("missing binary: manifest stat %v, herdr calls %v; want neither", serr, f.calls)
	}

	os.MkdirAll(bin, 0o755) // a directory at that path is not a binary
	if _, err := installManifest(ctx, io.Discard); err == nil || !strings.Contains(err.Error(), "make install") {
		t.Errorf("directory at the binary path: err = %v", err)
	}
	os.Remove(bin)

	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	linked, err := installManifest(ctx, io.Discard)
	if err != nil || !linked {
		t.Fatalf("installed binary: linked=%v err=%v", linked, err)
	}
	b, _ := os.ReadFile(manifest)
	var m manifestDoc
	if _, err := toml.Decode(string(b), &m); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if got := m.Startup[0].Command[0]; got != bin || strings.Contains(string(b), exe) {
		t.Errorf("manifest runs %q, want %s and never the running binary %s", got, bin, exe)
	}
}

// A relink that fails puts the previous manifest back (or removes a first
// one), so the next run sees the change again and relinks instead of
// reporting "already linked" with a manifest herdr never read.
func TestInstallRestoresManifestWhenRelinkFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := &fakeHerdrCLI{linked: map[string]string{}}
	herdrRunner = f.run
	defer func() { herdrRunner = defaultHerdrRunner }()
	ctx := context.Background()
	dir := paths.PluginDir()
	path := filepath.Join(dir, ManifestFile)
	onDisk := func() string { b, _ := os.ReadFile(path); return string(b) }

	// First install fails to link: no manifest is left behind.
	f.fail = "link"
	if _, err := install(ctx, "/opt/sessionhub", io.Discard); err == nil {
		t.Fatal("first install: want the link error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("first install failed but the manifest stayed: %v", err)
	}

	f.fail = ""
	if linked, err := install(ctx, "/opt/sessionhub", io.Discard); err != nil || !linked {
		t.Fatalf("install /opt/sessionhub: linked=%v err=%v", linked, err)
	}
	for _, fail := range []string{"unlink", "link"} {
		f.fail = fail
		var out strings.Builder
		if _, err := install(ctx, "/usr/local/bin/sessionhub", &out); err == nil || !strings.Contains(err.Error(), fail) {
			t.Fatalf("%s fails: err = %v", fail, err)
		}
		if onDisk() != Manifest("/opt/sessionhub") {
			t.Errorf("%s fails: manifest on disk is not the previous one:\n%s", fail, onDisk())
		}
		if !strings.Contains(out.String(), "restored the previous") {
			t.Errorf("%s fails: output %q", fail, out.String())
		}
	}

	// herdr works again: the next run relinks with the new manifest.
	f.fail = ""
	f.calls = nil
	var out strings.Builder
	linked, err := install(ctx, "/usr/local/bin/sessionhub", &out)
	if err != nil || !linked || strings.Contains(out.String(), "already linked") {
		t.Fatalf("after recovery: linked=%v err=%v out=%q", linked, err, out.String())
	}
	if !strings.HasSuffix(strings.Join(f.calls, "|"), "plugin link "+dir) || onDisk() != Manifest("/usr/local/bin/sessionhub") {
		t.Errorf("after recovery: calls %v, manifest:\n%s", f.calls, onDisk())
	}
}

func TestInstallHerdrFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	herdrRunner = func(context.Context, ...string) ([]byte, error) { return nil, os.ErrPermission }
	defer func() { herdrRunner = defaultHerdrRunner }()
	if _, err := install(context.Background(), "/opt/sessionhub", io.Discard); err == nil {
		t.Error("install succeeded with a failing herdr CLI")
	}
	if err := uninstall(context.Background(), io.Discard); err == nil {
		t.Error("uninstall succeeded with a failing herdr CLI")
	}
}
