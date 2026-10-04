package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testBinary = "/home/user/.local/bin/sessionhub"

// realisticSettings mimics ~/.claude/settings.json: herdr's managed
// SessionStart entry, other user hooks, and unrelated keys with odd values.
const realisticSettings = `{
  "$schema": "https://json.schemastore.org/claude-code-settings.json",
  "permissions": {
    "allow": [
      "Bash(docker build *)",
      "Bash(grep -n '\\\\$CI_COMMIT_BRANCH == \"main\"' .gitlab-ci.yml)"
    ],
    "additionalDirectories": [
      "/home/user/Code/OTGS/infrastructure/terraform/infrastructure/Global Services/iam/ci"
    ]
  },
  "hooks": {
    "SessionStart": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "bash '/home/user/.claude/hooks/herdr-agent-state.sh' session",
            "timeout": 10
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          { "type": "command", "command": "/home/user/bin/audit-bash.sh" }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {"type":"command","command":"/home/user/bin/notify-done.sh","async":true}
        ]
      }
    ]
  },
  "enabledPlugins": {
    "grafana-plugins@grafana-skills": true
  },
  "note": "café 😀 <&>",
  "big": 12345678901234567890.5e-3
}
`

const herdrGroup = `{
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "bash '/home/user/.claude/hooks/herdr-agent-state.sh' session",
            "timeout": 10
          }
        ]
      }`

const userStopGroup = `{
        "hooks": [
          {"type":"command","command":"/home/user/bin/notify-done.sh","async":true}
        ]
      }`

func mustInstall(t *testing.T, src string) string {
	t.Helper()
	out, err := installJSON([]byte(src), testBinary)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func mustUninstall(t *testing.T, src string) string {
	t.Helper()
	out, err := uninstallJSON([]byte(src), testBinary)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// hubGroups decodes settings and returns the sessionhub matcher groups per event.
func hubGroups(t *testing.T, doc string) map[string][]matcherGroup {
	t.Helper()
	var s struct {
		Hooks map[string][]matcherGroup `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(doc), &s); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, doc)
	}
	out := map[string][]matcherGroup{}
	for ev, gs := range s.Hooks {
		for _, g := range gs {
			for _, h := range g.Hooks {
				if isHubCommand(h.Command, testBinary) {
					out[ev] = append(out[ev], g)
					break
				}
			}
		}
	}
	return out
}

func TestInstallAddsTheTable(t *testing.T) {
	out := mustInstall(t, realisticSettings)
	got := hubGroups(t, out)
	if len(got) != 6 {
		t.Fatalf("sessionhub events = %d, want 6: %v", len(got), got)
	}
	cmd := func(arg string) string { return testBinary + " hook " + arg }
	ctx := hookEntry{Type: "command", Command: cmd("context"), Timeout: 5}
	want := map[string][]hookEntry{
		"SessionStart":      {{Type: "command", Command: cmd("session-start"), Async: true}, ctx},
		"UserPromptSubmit":  {{Type: "command", Command: cmd("prompt"), Async: true}, ctx},
		"Stop":              {{Type: "command", Command: cmd("stop"), Async: true}},
		"Notification":      {{Type: "command", Command: cmd("notification"), Async: true}},
		"SessionEnd":        {{Type: "command", Command: cmd("session-end"), Timeout: 2}},
		"PermissionRequest": {{Type: "command", Command: cmd("permission-request"), Timeout: 660}},
	}
	for _, spec := range hookTable {
		gs := got[spec.Event]
		if len(gs) != 1 {
			t.Errorf("%s: groups = %+v", spec.Event, gs)
			continue
		}
		g := gs[0]
		if spec.Matcher == "" && g.Matcher != nil {
			t.Errorf("%s: unexpected matcher %q", spec.Event, *g.Matcher)
		}
		if spec.Matcher != "" && (g.Matcher == nil || *g.Matcher != spec.Matcher) {
			t.Errorf("%s: matcher = %v, want %q", spec.Event, g.Matcher, spec.Matcher)
		}
		if !reflect.DeepEqual(g.Hooks, want[spec.Event]) {
			t.Errorf("%s: entries\n got %+v\nwant %+v", spec.Event, g.Hooks, want[spec.Event])
		}
	}
}

func TestInstallPreservesForeignBytes(t *testing.T) {
	out := mustInstall(t, realisticSettings)
	// herdr's entry and the user's hooks appear byte for byte.
	for _, frag := range []string{herdrGroup, userStopGroup,
		`{ "type": "command", "command": "/home/user/bin/audit-bash.sh" }`} {
		if !strings.Contains(out, frag) {
			t.Errorf("output lost a fragment:\n%s\n---\n%s", frag, out)
		}
	}
	// Everything before "hooks" and after it is unchanged.
	pre := realisticSettings[:strings.Index(realisticSettings, `"hooks"`)]
	post := realisticSettings[strings.Index(realisticSettings, `  "enabledPlugins"`):]
	if !strings.HasPrefix(out, pre) || !strings.HasSuffix(out, post) {
		t.Errorf("bytes outside hooks changed:\n%s", out)
	}
	// The untouched PreToolUse event keeps its exact bytes.
	pre2 := realisticSettings[strings.Index(realisticSettings, `"PreToolUse"`):strings.Index(realisticSettings, `    "Stop"`)]
	if !strings.Contains(out, pre2) {
		t.Errorf("PreToolUse changed:\n%s", out)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	once := mustInstall(t, realisticSettings)
	twice := mustInstall(t, once)
	if once != twice {
		t.Errorf("second install changed the file:\n%s\n---\n%s", once, twice)
	}
	if n := strings.Count(twice, " hook session-start"); n != 1 {
		t.Errorf("session-start appears %d times", n)
	}
}

func TestInstallRepairsAStaleEntry(t *testing.T) {
	stale := strings.Replace(mustInstall(t, realisticSettings), `"timeout":2`, `"timeout":9`, 1)
	if stale == mustInstall(t, realisticSettings) {
		t.Fatal("test setup did not change the file")
	}
	fixed := mustInstall(t, stale)
	if fixed != mustInstall(t, realisticSettings) {
		t.Errorf("stale entry was not repaired:\n%s", fixed)
	}
}

func TestUninstallRemovesOnlyHub(t *testing.T) {
	out := mustUninstall(t, mustInstall(t, realisticSettings))
	if hg := hubGroups(t, out); len(hg) != 0 {
		t.Errorf("sessionhub groups remain: %v", hg)
	}
	if out != realisticSettings {
		t.Errorf("round trip is not byte-identical:\n%s\n---\n%s", realisticSettings, out)
	}
}

func TestRoundTripOtherLayouts(t *testing.T) {
	cases := map[string]string{
		"no hooks key":       "{\n  \"model\": \"opus\"\n}\n",
		"empty object":       "{}\n",
		"empty hooks":        "{\n  \"hooks\": {}\n}\n",
		"hooks first":        "{\n  \"hooks\": {\n    \"PreToolUse\": []\n  },\n  \"model\": \"x\"\n}\n",
		"tab indent, no eol": "{\n\t\"a\": 1,\n\t\"hooks\": {\n\t\t\"Stop\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"x\"}]}]\n\t}\n}",
		"compact":            `{"a":1,"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x"}]}]},"b":2}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			in := mustInstall(t, src)
			if n := len(hubGroups(t, in)); n != 6 {
				t.Fatalf("installed %d sessionhub events:\n%s", n, in)
			}
			if in != mustInstall(t, in) {
				t.Error("not idempotent")
			}
			out := mustUninstall(t, in)
			if len(hubGroups(t, out)) != 0 {
				t.Errorf("sessionhub groups remain:\n%s", out)
			}
			// Uninstall may leave an empty "hooks" object behind, nothing else.
			if !jsonEqual(t, src, out) && !jsonEqual(t, strings.Replace(out, `"hooks": {}`, `"hooks": null`, 1), `null`) {
				var m map[string]json.RawMessage
				json.Unmarshal([]byte(out), &m)
				var o map[string]json.RawMessage
				json.Unmarshal([]byte(src), &o)
				delete(m, "hooks")
				delete(o, "hooks")
				mb, _ := json.Marshal(m)
				ob, _ := json.Marshal(o)
				if !bytes.Equal(mb, ob) {
					t.Errorf("uninstall changed content:\n%s\n---\n%s", src, out)
				}
			}
		})
	}
}

func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		t.Fatal(err)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}

func TestUninstallMixedGroupKeepsUserEntry(t *testing.T) {
	src := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/home/user/.local/bin/sessionhub hook stop","async":true},{"type":"command","command":"/usr/bin/other"}]}]}}`
	out := mustUninstall(t, src)
	if strings.Contains(out, "sessionhub hook") || !strings.Contains(out, "/usr/bin/other") {
		t.Errorf("out = %s", out)
	}
}

func TestUninstallIgnoresLookalikes(t *testing.T) {
	src := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/home/user/.local/bin/sessionhub-other hook stop"},{"type":"command","command":"/home/user/.local/bin/sessionhub hookx"},{"type":"command","command":"/opt/sessionhub hook stop"}]}]}}`
	if out := mustUninstall(t, src); out != src {
		t.Errorf("lookalike entries were touched:\n%s", out)
	}
}

func TestMalformedSettingsAreRefused(t *testing.T) {
	for _, src := range []string{"[1,2]", "{", `{"hooks": []}`, `{"hooks": {"Stop": {}}}`, "nonsense"} {
		if _, err := installJSON([]byte(src), testBinary); err == nil {
			t.Errorf("install accepted %q", src)
		}
	}
}

func TestPathWithSpacesIsQuoted(t *testing.T) {
	bin := "/home/my user/bin/sessionhub"
	out, err := installJSON([]byte("{}"), bin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `'/home/my user/bin/sessionhub' hook stop`) {
		t.Errorf("out = %s", out)
	}
	back, err := uninstallJSON(out, bin)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(back), " hook ") {
		t.Errorf("uninstall missed quoted entries: %s", back)
	}
}

func TestFileInstallBackupAndConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if got := settingsPath(); got != filepath.Join(dir, "settings.json") {
		t.Fatalf("settingsPath = %s", got)
	}
	path := settingsPath()
	os.WriteFile(path, []byte(realisticSettings), 0o640)
	o := installOpts{settings: path, binary: testBinary}
	now := time.Date(2026, 9, 30, 12, 34, 56, 0, time.UTC)

	changed, backup, err := installFile(o, now)
	if err != nil || !changed {
		t.Fatalf("install: %v changed=%v", err, changed)
	}
	if backup != path+".sessionhub-backup-20260930T123456" {
		t.Errorf("backup = %s", backup)
	}
	if b, _ := os.ReadFile(backup); string(b) != realisticSettings {
		t.Error("backup differs from the original")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}

	// Second run: nothing changes, no new backup.
	changed, backup, err = installFile(o, now.Add(time.Second))
	if err != nil || changed || backup != "" {
		t.Errorf("second install: changed=%v backup=%q err=%v", changed, backup, err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Errorf("dir has %d files, want settings + 1 backup", len(ents))
	}

	changed, _, err = uninstallFile(o, now.Add(2*time.Second))
	if err != nil || !changed {
		t.Fatalf("uninstall: %v changed=%v", err, changed)
	}
	if b, _ := os.ReadFile(path); string(b) != realisticSettings {
		t.Error("round trip through files changed the settings")
	}
	changed, _, _ = uninstallFile(o, now.Add(3*time.Second))
	if changed {
		t.Error("second uninstall reported a change")
	}
}

func TestFileInstallCreatesMissingFileAndFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "sub", "settings.json")
	if _, backup, err := installFile(installOpts{settings: fresh, binary: testBinary}, time.Now()); err != nil || backup != "" {
		t.Fatalf("fresh install: backup=%q err=%v", backup, err)
	}
	if b, _ := os.ReadFile(fresh); len(hubGroups(t, string(b))) != 6 {
		t.Errorf("fresh file: %s", b)
	}

	real := filepath.Join(dir, "dotfiles-settings.json")
	link := filepath.Join(dir, "link.json")
	os.WriteFile(real, []byte("{}\n"), 0o600)
	os.Symlink(real, link)
	if _, _, err := installFile(installOpts{settings: link, binary: testBinary}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was replaced by a regular file")
	}
	if b, _ := os.ReadFile(real); len(hubGroups(t, string(b))) != 6 {
		t.Error("the symlink target was not updated")
	}
}

func TestMalformedFileIsNotModified(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte("{ broken"), 0o600)
	if _, _, err := installFile(installOpts{settings: p, binary: testBinary}, time.Now()); err == nil {
		t.Fatal("expected an error")
	}
	if b, _ := os.ReadFile(p); string(b) != "{ broken" {
		t.Errorf("file changed: %s", b)
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Errorf("stray files: %v", ents)
	}
}

func TestParseOpts(t *testing.T) {
	o, err := parseOpts("install-hooks", []string{"--settings", "/x/s.json", "--binary", "/x/sessionhub"})
	if err != nil || o.settings != "/x/s.json" || o.binary != "/x/sessionhub" {
		t.Errorf("o = %+v, err = %v", o, err)
	}
	if _, err := parseOpts("install-hooks", []string{"--binary", "sessionhub"}); err == nil {
		t.Error("relative binary accepted")
	}
	if _, err := parseOpts("install-hooks", []string{"--nope"}); err == nil {
		t.Error("unknown flag accepted")
	}
}

// oldInstall is realisticSettings after a sessionhub install from before the
// context and permission-request entries: one sessionhub group in each of five
// events, beside herdr's and the user's own groups.
const oldInstall = `{
  "hooks": {
    "SessionStart": [
      ` + herdrGroup + `,
      {"matcher":"*","hooks":[{"type":"command","command":"/home/user/.local/bin/sessionhub hook session-start","async":true}]}
    ],
    "UserPromptSubmit": [
      {"hooks":[{"type":"command","command":"/home/user/.local/bin/sessionhub hook prompt","async":true}]}
    ],
    "PreToolUse": [
      {"matcher":"Bash","hooks":[{"type":"command","command":"/home/user/bin/audit-bash.sh"}]}
    ],
    "Stop": [
      ` + userStopGroup + `,
      {"hooks":[{"type":"command","command":"/home/user/.local/bin/sessionhub hook stop","async":true}]}
    ],
    "Notification": [
      {"matcher":"*","hooks":[{"type":"command","command":"/home/user/.local/bin/sessionhub hook notification","async":true}]}
    ],
    "SessionEnd": [
      {"matcher":"*","hooks":[{"type":"command","command":"/home/user/.local/bin/sessionhub hook session-end","timeout":2}]}
    ]
  }
}
`

func TestInstallUpgradesAnOldInstall(t *testing.T) {
	once := mustInstall(t, oldInstall)
	got := hubGroups(t, once)
	fresh := hubGroups(t, mustInstall(t, realisticSettings))
	if !reflect.DeepEqual(got, fresh) {
		t.Errorf("upgraded sessionhub groups differ from a fresh install:\n got %+v\nwant %+v", got, fresh)
	}
	for ev, gs := range got {
		if len(gs) != 1 {
			t.Errorf("%s: %d sessionhub groups, want 1", ev, len(gs))
		}
	}
	// Every sessionhub command appears once, except context (two events).
	for arg, n := range map[string]int{"session-start": 1, "prompt": 1, "stop": 1, "notification": 1,
		"session-end": 1, "permission-request": 1, "context": 2} {
		if c := strings.Count(once, " hook "+arg+`"`); c != n {
			t.Errorf("sessionhub hook %s appears %d times, want %d", arg, c, n)
		}
	}
	// Non-sessionhub groups stay, byte for byte.
	for _, frag := range []string{herdrGroup, userStopGroup, `"command":"/home/user/bin/audit-bash.sh"`} {
		if !strings.Contains(once, frag) {
			t.Errorf("upgrade lost a non-sessionhub fragment:\n%s\n---\n%s", frag, once)
		}
	}
	if twice := mustInstall(t, once); twice != once {
		t.Errorf("second install changed the file:\n%s\n---\n%s", once, twice)
	}
	// Uninstall leaves only the non-sessionhub groups.
	out := mustUninstall(t, once)
	if hg := hubGroups(t, out); len(hg) != 0 {
		t.Errorf("sessionhub groups remain: %v", hg)
	}
	for _, frag := range []string{herdrGroup, userStopGroup, `"command":"/home/user/bin/audit-bash.sh"`} {
		if !strings.Contains(out, frag) {
			t.Errorf("uninstall lost a non-sessionhub fragment:\n%s", frag)
		}
	}
}

func TestInstallKeepsUserEntryInAMixedGroup(t *testing.T) {
	src := `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"/home/user/.local/bin/sessionhub hook prompt","async":true},{"type":"command","command":"/usr/bin/other"}]}]}}`
	once := mustInstall(t, src)
	if !strings.Contains(once, `"/usr/bin/other"`) {
		t.Errorf("install dropped the user's entry:\n%s", once)
	}
	if gs := hubGroups(t, once)["UserPromptSubmit"]; len(gs) != 1 || len(gs[0].Hooks) != 2 {
		t.Errorf("UserPromptSubmit sessionhub groups %+v, want one with prompt and context", gs)
	}
	if twice := mustInstall(t, once); twice != once {
		t.Errorf("second install changed the file:\n%s\n---\n%s", once, twice)
	}
}
