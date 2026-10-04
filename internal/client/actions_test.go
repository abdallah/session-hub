package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestActionCallsRequestShape(t *testing.T) {
	ctx := context.Background()
	c, log := server(t, 200, `{}`)
	for _, tc := range []struct {
		name, method, path, query, body string
		call                            func() error
	}{
		{"rules", "GET", "/v1/instructions", "", "", func() error { _, err := c.Instructions(ctx); return err }},
		{"add rule", "POST", "/v1/instructions", "", `{"text":"Be brief."}`,
			func() error { _, err := c.AddInstruction(ctx, "Be brief."); return err }},
		{"remove rule", "DELETE", "/v1/instructions/7", "", "", func() error { return c.DeleteInstruction(ctx, 7) }},
		{"send", "POST", "/v1/messages", "", `{"session_ids":["a","b"],"text":"hi","from_session":"s1"}`,
			func() error {
				_, err := c.SendMessages(ctx, api.MessagesIn{SessionIDs: []string{"a", "b"}, Text: "hi", FromSession: "s1"})
				return err
			}},
		{"message", "GET", "/v1/messages/msg_x", "", "", func() error { _, err := c.GetMessage(ctx, "msg_x"); return err }},
		{"permission", "POST", "/v1/sessions/s1/permissions", "", `{"tool_name":"Bash","tool_input":{"command":"ls"}}`,
			func() error {
				_, err := c.CreatePermission(ctx, "s1", api.PermissionIn{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`)})
				return err
			}},
		{"decide", "POST", "/v1/permissions/pr_x/decide", "", `{"decision":"deny","reason":"no"}`,
			func() error {
				_, err := c.DecidePermission(ctx, "pr_x", api.DecisionIn{Decision: api.DecisionDeny, Reason: "no"})
				return err
			}},
	} {
		*log = nil
		if err := tc.call(); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		g := (*log)[0]
		if g.Method != tc.method || g.Path != tc.path || g.Query != tc.query || string(g.Body) != tc.body || g.Auth != "Bearer hub_m_tok" {
			t.Errorf("%s: got %s %s?%s body %s auth %q", tc.name, g.Method, g.Path, g.Query, g.Body, g.Auth)
		}
	}
}

func TestWaitDecision(t *testing.T) {
	ctx := context.Background()
	c, log := server(t, 204, ``)
	got, err := c.WaitDecision(ctx, "pr_x", 30*time.Second)
	if err != nil || got != nil {
		t.Fatalf("204: %+v %v, want nil", got, err)
	}
	if g := (*log)[0]; g.Method != "GET" || g.Path != "/v1/permissions/pr_x/decision" || g.Query != "wait=30" {
		t.Errorf("request %+v", g)
	}
	c, _ = server(t, 200, `{"id":"pr_x","state":"decided","decision":"allow"}`)
	got, err = c.WaitDecision(ctx, "pr_x", 0)
	if err != nil || got == nil || got.Decision != api.DecisionAllow {
		t.Fatalf("200: %+v %v", got, err)
	}
	c, _ = server(t, 409, `{"error":"request pr_x belongs to another machine"}`)
	_, err = c.WaitDecision(ctx, "pr_x", time.Second)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 409 {
		t.Errorf("409: %v", err)
	}
}

func TestInstructionsCopy(t *testing.T) {
	dir := t.TempDir()
	path := InstructionsPath(filepath.Join(dir, "state"))
	if filepath.Base(path) != InstructionsFile {
		t.Fatalf("path %s", path)
	}
	if _, err := LoadInstructions(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing copy: %v, want ErrNotExist", err)
	}
	list := api.InstructionList{Version: "v1", Instructions: []api.Instruction{{ID: 1, Text: "Be brief."}}}
	if err := SaveInstructions(path, list); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", st.Mode().Perm(), err)
	}
	got, err := LoadInstructions(path)
	if err != nil || got.Version != "v1" || len(got.Instructions) != 1 || got.Instructions[0].Text != "Be brief." {
		t.Errorf("loaded %+v %v", got, err)
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".instructions-*")); len(m) != 0 {
		t.Errorf("temp files left: %v", m)
	}
	os.WriteFile(path, []byte("{broken"), 0o600)
	if _, err := LoadInstructions(path); err == nil {
		t.Error("a broken copy loaded")
	}
}

func TestRefreshInstructions(t *testing.T) {
	ctx := context.Background()
	version := "v1"
	gets := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.InstructionList{Version: version, Instructions: []api.Instruction{{ID: 1, Text: "rule " + version}}})
	}))
	t.Cleanup(ts.Close)
	c, _ := New(Config{ServerURL: ts.URL, Token: "hub_m_tok"})
	path := InstructionsPath(t.TempDir())
	changed, err := RefreshInstructions(ctx, c, path)
	if err != nil || !changed {
		t.Fatalf("first refresh: %v %v", changed, err)
	}
	st1, _ := os.Stat(path)
	time.Sleep(10 * time.Millisecond)
	if changed, err := RefreshInstructions(ctx, c, path); err != nil || changed {
		t.Errorf("same version: changed=%v err=%v", changed, err)
	}
	if st2, _ := os.Stat(path); !st2.ModTime().Equal(st1.ModTime()) {
		t.Error("an unchanged list rewrote the copy")
	}
	version = "v2"
	if changed, _ := RefreshInstructions(ctx, c, path); !changed {
		t.Error("a new version did not rewrite the copy")
	}
	if got, _ := LoadInstructions(path); got.Instructions[0].Text != "rule v2" || gets != 3 {
		t.Errorf("copy %+v after %d gets", got, gets)
	}
}

func TestRemotePermissionsOff(t *testing.T) {
	off, on := false, true
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "SESSIONHUB_REMOTE_PERMISSIONS" {
				return v
			}
			return ""
		}
	}
	for _, c := range []struct {
		cfg  Config
		env  string
		want bool
	}{
		{Config{}, "", false},
		{Config{RemotePermissions: &on}, "", false},
		{Config{RemotePermissions: &off}, "", true},
		{Config{}, "off", true},
		{Config{}, "OFF", true},
		{Config{RemotePermissions: &on}, "off", true},
		{Config{}, "on", false},
	} {
		if got := RemotePermissionsOff(c.cfg, env(c.env)); got != c.want {
			t.Errorf("cfg %v env %q: %v, want %v", c.cfg.RemotePermissions, c.env, got, c.want)
		}
	}
}

func TestConfigRemotePermissionsKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("SESSIONHUB_CONFIG", path)
	for _, k := range []string{"SESSIONHUB_SERVER_URL", "SESSIONHUB_TOKEN", "SESSIONHUB_MACHINE"} {
		t.Setenv(k, "")
	}
	if err := (Config{ServerURL: "https://sessionhub.example"}).Save(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "remote_permissions") {
		t.Errorf("an unset key was saved:\n%s", b)
	}
	os.WriteFile(path, []byte("server_url = \"https://sessionhub.example\"\nremote_permissions = false\n"), 0o600)
	c, err := LoadConfig()
	if err != nil || c.RemotePermissions == nil || *c.RemotePermissions {
		t.Errorf("loaded %+v %v", c, err)
	}
}

func TestSendMessagesFromSessionErrors(t *testing.T) {
	for _, status := range []int{404, 409} {
		c, _ := server(t, status, `{"error":"from_session problem"}`)
		_, err := c.SendMessages(context.Background(), api.MessagesIn{SessionIDs: []string{"a"}, Text: "hi", FromSession: "s9"})
		var se *StatusError
		if !errors.As(err, &se) || se.Status != status || se.Message != "from_session problem" {
			t.Errorf("%d: %v", status, err)
		}
	}
}

func TestRemoteStartOff(t *testing.T) {
	off, on := false, true
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "SESSIONHUB_REMOTE_START" {
				return v
			}
			return ""
		}
	}
	for _, c := range []struct {
		cfg  Config
		env  string
		want bool
	}{
		{Config{}, "", false},
		{Config{RemoteStart: &on}, "", false},
		{Config{RemoteStart: &off}, "", true},
		{Config{}, "Off", true},
		{Config{RemoteStart: &on}, "off", true},
		{Config{RemotePermissions: &off}, "", false}, // the other opt-out doesn't count
	} {
		if got := RemoteStartOff(c.cfg, env(c.env)); got != c.want {
			t.Errorf("cfg %v env %q: %v, want %v", c.cfg.RemoteStart, c.env, got, c.want)
		}
	}
	// The key loads from the file.
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("SESSIONHUB_CONFIG", path)
	if err := os.WriteFile(path, []byte("server_url = \"http://x\"\nremote_start = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil || cfg.RemoteStart == nil || *cfg.RemoteStart {
		t.Fatalf("loaded %+v, %v", cfg.RemoteStart, err)
	}
}
