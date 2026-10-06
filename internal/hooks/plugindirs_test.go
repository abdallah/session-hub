package hooks

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const modDir = "/home/u/.local/share/sessionhub/claude-mod"

func TestAddPluginDir(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"empty file", "", "{\n  \"env\": {\n    \"CLAUDE_CODE_PLUGIN_DIRS\": \"" + modDir + "\"\n  }\n}\n"},
		{"no env", "{\n    \"model\": \"opus\"\n}\n",
			"{\n    \"model\": \"opus\",\n    \"env\": {\n        \"CLAUDE_CODE_PLUGIN_DIRS\": \"" + modDir + "\"\n    }\n}\n"},
		{"env without the key", "{\n  \"env\": {\n    \"FOO\": \"1\"\n  }\n}\n",
			"{\n  \"env\": {\n    \"FOO\": \"1\",\n    \"CLAUDE_CODE_PLUGIN_DIRS\": \"" + modDir + "\"\n  }\n}\n"},
		{"empty env", "{\n  \"env\": {}\n}\n",
			"{\n  \"env\": {\n    \"CLAUDE_CODE_PLUGIN_DIRS\": \"" + modDir + "\"\n  }\n}\n"},
		{"other dirs", "{\"env\": {\"CLAUDE_CODE_PLUGIN_DIRS\": \"/a:/b\"}, \"x\": 1}",
			"{\"env\": {\"CLAUDE_CODE_PLUGIN_DIRS\": \"/a:/b:" + modDir + "\"}, \"x\": 1}"},
		{"empty value", "{\"env\": {\"CLAUDE_CODE_PLUGIN_DIRS\": \"\"}}",
			"{\"env\": {\"CLAUDE_CODE_PLUGIN_DIRS\": \"" + modDir + "\"}}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AddPluginDir([]byte(tt.src), modDir)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("got\n%s\nwant\n%s", got, tt.want)
			}
			if !json.Valid(got) {
				t.Errorf("invalid JSON:\n%s", got)
			}
			again, err := AddPluginDir(got, modDir)
			if err != nil || string(again) != string(got) {
				t.Errorf("second add changed the file: %v\n%s", err, again)
			}
		})
	}
}

func TestAddPluginDirAlreadyListed(t *testing.T) {
	src := "{\"env\": {\"CLAUDE_CODE_PLUGIN_DIRS\": \"/a:" + modDir + "/:/b\"}}"
	got, err := AddPluginDir([]byte(src), modDir)
	if err != nil || string(got) != src {
		t.Errorf("got %s, %v; want the file unchanged", got, err)
	}
}

func TestAddPluginDirRefusals(t *testing.T) {
	for _, src := range []string{`[]`, `{"env": []}`, `{"env": {"CLAUDE_CODE_PLUGIN_DIRS": 3}}`, `{"env": `} {
		if _, err := AddPluginDir([]byte(src), modDir); err == nil {
			t.Errorf("%s: no error", src)
		}
	}
}

func TestRemovePluginDir(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"keeps other dirs", "{\"env\": {\"CLAUDE_CODE_PLUGIN_DIRS\": \"/a:" + modDir + ":/b\"}}",
			"{\"env\": {\"CLAUDE_CODE_PLUGIN_DIRS\": \"/a:/b\"}}"},
		{"keeps other env", "{\n  \"env\": {\n    \"CLAUDE_CODE_PLUGIN_DIRS\": \"" + modDir + "\",\n    \"FOO\": \"1\"\n  }\n}\n",
			"{\n  \"env\": {\n    \"FOO\": \"1\"\n  }\n}\n"},
		{"keeps other env before", "{\n  \"env\": {\n    \"FOO\": \"1\",\n    \"CLAUDE_CODE_PLUGIN_DIRS\": \"" + modDir + "\"\n  }\n}\n",
			"{\n  \"env\": {\n    \"FOO\": \"1\"\n  }\n}\n"},
		{"not listed", "{\"env\": {\"CLAUDE_CODE_PLUGIN_DIRS\": \"/a\"}}", "{\"env\": {\"CLAUDE_CODE_PLUGIN_DIRS\": \"/a\"}}"},
		{"no env", "{\"x\": 1}", "{\"x\": 1}"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RemovePluginDir([]byte(tt.src), modDir)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("got\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// An add then a remove gives back the original bytes.
func TestPluginDirRoundTrip(t *testing.T) {
	for _, src := range []string{
		"{\n  \"model\": \"opus\",\n  \"hooks\": {}\n}\n",
		"{\n  \"env\": {\n    \"FOO\": \"1\"\n  }\n}\n",
		"{\n  \"env\": {\n    \"CLAUDE_CODE_PLUGIN_DIRS\": \"/a\"\n  }\n}\n",
		"{}\n",
	} {
		added, err := AddPluginDir([]byte(src), modDir)
		if err != nil {
			t.Fatal(err)
		}
		back, err := RemovePluginDir(added, modDir)
		if err != nil {
			t.Fatal(err)
		}
		if string(back) != src {
			t.Errorf("round trip of\n%s\ngave\n%s", src, back)
		}
	}
}

func TestEditSettingsBacksUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{\"x\": 1}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	add := func(src []byte) ([]byte, error) { return AddPluginDir(src, modDir) }
	changed, backup, err := EditSettings(path, now, add)
	if err != nil || !changed || backup == "" {
		t.Fatalf("changed %v backup %q err %v", changed, backup, err)
	}
	if b, _ := os.ReadFile(backup); string(b) != "{\"x\": 1}\n" {
		t.Errorf("backup holds %q", b)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640", st.Mode().Perm())
	}
	changed, _, err = EditSettings(path, now, add)
	if err != nil || changed {
		t.Errorf("second edit: changed %v err %v", changed, err)
	}
}

// install-hooks prints a hint while the mod is not installed, and none once
// it is.
func TestInstallHooksModHint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	run := func() string {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stdout
		os.Stdout = w
		err = RunInstall(context.Background(), []string{"--binary", "/opt/sessionhub"})
		os.Stdout = old
		w.Close()
		out, _ := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if out := run(); !strings.Contains(out, "sessionhub install-mod") {
		t.Errorf("no hint without the mod:\n%s", out)
	}
	manifest := filepath.Join(home, ".local", "share", "sessionhub", "claude-mod", ".claude-plugin", "plugin.json")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := run(); strings.Contains(out, "install-mod") {
		t.Errorf("hint with the mod installed:\n%s", out)
	}
}
