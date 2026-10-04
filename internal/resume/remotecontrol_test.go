package resume

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

const rcURL = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"

// fixtureText returns result.read.text from a captured pane.read exchange.
func fixtureText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "herdr", "socket", name))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var resp struct {
		Result struct {
			Read struct {
				Text string `json:"text"`
			} `json:"read"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Result.Read.Text
}

// rcScript scripts the herdr side of `sessionhub remote-control`.
type rcScript struct {
	pane       string // pane ID that exists ("" for none)
	agent      string // detected agent in that pane
	session    string // its agent_session value
	promptErr  string // error code for agent.prompt ("" for success)
	keysErr    string // error code for pane.send_keys ("" for success)
	readErr    bool   // pane.read fails
	before     string // pane text before the prompt
	after      string // pane text once the prompt was sent
	staleReads int    // post-prompt reads that still return `before`
	// visibleBefore and visible are the visible screen before and after the
	// prompt (source "visible"); status overrides the pane's agent_status.
	visibleBefore, visible, status string
	// visibleLater replaces visible from the second post-prompt visible read.
	visibleLater string
	// statusAfterPrompt applies status only after the prompt was sent.
	statusAfterPrompt bool
	postVis           int
	mu                sync.Mutex
	prompted          bool
	postReads         int
	readsBefore       int
}

func (rc *rcScript) herdr(t *testing.T) *fakeHerdr {
	return newFakeHerdr(t, func(m string, p map[string]any) (any, string) {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		switch m {
		case "pane.get":
			if p["pane_id"] == rc.pane {
				res := paneResult(rc.pane, rc.session, rc.agent)
				if rc.status != "" && (!rc.statusAfterPrompt || rc.prompted) {
					res["pane"].(map[string]any)["agent_status"] = rc.status
				}
				return res, ""
			}
			return nil, "pane_not_found"
		case "pane.read":
			if rc.readErr {
				return nil, "pane_not_found"
			}
			if p["source"] == "visible" {
				text := rc.visibleBefore
				if rc.prompted {
					rc.postVis++
					text = rc.visible
					if rc.visibleLater != "" && rc.postVis > 1 {
						text = rc.visibleLater
					}
				}
				return map[string]any{"type": "pane_read", "read": map[string]any{"pane_id": rc.pane, "text": text}}, ""
			}
			text := rc.before
			if rc.prompted {
				rc.postReads++
				if rc.postReads > rc.staleReads {
					text = rc.after
				}
			} else {
				rc.readsBefore++
			}
			return map[string]any{"type": "pane_read", "read": map[string]any{"pane_id": rc.pane, "text": text}}, ""
		case "pane.send_keys":
			if rc.keysErr != "" {
				return nil, rc.keysErr
			}
			return map[string]any{"type": "ok"}, ""
		case "agent.prompt":
			if rc.promptErr != "" {
				return nil, rc.promptErr
			}
			rc.prompted = true
			return map[string]any{"type": "agent_prompted"}, ""
		}
		return nil, "no_fixture"
	})
}

func newRCScript(t *testing.T) *rcScript {
	return &rcScript{
		pane: "w1:p1", agent: "claude", session: uuid,
		before: fixtureText(t, "pane-read-before-remote-control.ndjson"),
		after:  fixtureText(t, "pane-read-after-remote-control.ndjson"),
		// A normal Claude screen, before and after.
		visibleBefore: fixtureText(t, "pane-read-visible-prompt.ndjson"),
		visible:       fixtureText(t, "pane-read-visible-prompt.ndjson"),
	}
}

func fastPoll(e *env) { e.pollEvery, e.pollFor = 5*time.Millisecond, 150*time.Millisecond }

func TestRemoteControlDecisionTable(t *testing.T) {
	const notRunning = "session is not running; start it with: sessionhub resume --remote-control " + uuid
	const blockedMsg = "the session is waiting on a prompt in its pane; answer it, then run this again"
	deadSock := filepath.Join(t.TempDir(), "none.sock")
	cases := []struct {
		name        string
		sess        api.Session
		script      func(rc *rcScript) // nil result: herdr not running
		noHerdr     bool
		wantErr     string
		wantOut     []string
		notOut      []string
		wantMethods string // prefix of the herdr methods, joined
		untouched   bool   // herdr must get no request
	}{
		{
			name:        "local pane runs the session: prompt, then print the new URL",
			sess:        sess("bluebox", "w1:p1"),
			script:      func(rc *rcScript) {},
			wantOut:     []string{rcURL},
			wantMethods: "pane.get,pane.read,pane.read,agent.prompt,pane.read,pane.read",
		},
		{
			name:        "the URL shows up on a later poll",
			sess:        sess("bluebox", "w1:p1"),
			script:      func(rc *rcScript) { rc.staleReads = 3 },
			wantOut:     []string{rcURL},
			wantMethods: "pane.get,pane.read,pane.read,agent.prompt,pane.read,pane.read,pane.read,pane.read,pane.read,pane.read,pane.read,pane.read",
		},
		{
			name: "a URL already in the pane before the prompt does not count",
			sess: sess("bluebox", "w1:p1"),
			script: func(rc *rcScript) {
				rc.before = rc.after
			},
			wantOut: []string{"Sent /remote-control", "Check the pane"},
			notOut:  []string{rcURL},
		},
		{
			name:    "no URL appears within the timeout: exit 0 with a hint",
			sess:    sess("bluebox", "w1:p1"),
			script:  func(rc *rcScript) { rc.after = rc.before },
			wantOut: []string{"Sent /remote-control", "Check the pane"},
			notOut:  []string{"https://"},
		},
		{
			name:        "agent_blocked: exit 1, no retry, no polling",
			sess:        sess("bluebox", "w1:p1"),
			script:      func(rc *rcScript) { rc.promptErr = "agent_blocked" },
			wantErr:     blockedMsg,
			wantMethods: "pane.get,pane.read,pane.read,agent.prompt",
		},
		{
			name:        "another prompt error is reported as is",
			sess:        sess("bluebox", "w1:p1"),
			script:      func(rc *rcScript) { rc.promptErr = "agent_not_found" },
			wantErr:     "agent_not_found",
			wantMethods: "pane.get,pane.read,pane.read,agent.prompt",
		},
		{
			name:        "the baseline read fails: nothing is sent",
			sess:        sess("bluebox", "w1:p1"),
			script:      func(rc *rcScript) { rc.readErr = true },
			wantErr:     "read pane",
			wantMethods: "pane.get,pane.read",
		},
		{
			name:        "pane gone: not running",
			sess:        sess("bluebox", "w1:p1"),
			script:      func(rc *rcScript) { rc.pane = "w1:p2" },
			wantErr:     notRunning,
			wantMethods: "pane.get",
		},
		{
			name:        "pane at a shell prompt: not running",
			sess:        sess("bluebox", "w1:p1"),
			script:      func(rc *rcScript) { rc.agent = "" },
			wantErr:     notRunning,
			wantMethods: "pane.get",
		},
		{
			name:        "pane runs another agent: not running",
			sess:        sess("bluebox", "w1:p1"),
			script:      func(rc *rcScript) { rc.agent = "codex" },
			wantErr:     notRunning,
			wantMethods: "pane.get",
		},
		{
			name:        "pane runs another Claude session: not running",
			sess:        sess("bluebox", "w1:p1"),
			script:      func(rc *rcScript) { rc.session = "99999999-0000-4000-8000-000000000000" },
			wantErr:     notRunning,
			wantMethods: "pane.get",
		},
		{
			name:        "pane has no agent_session: not running",
			sess:        sess("bluebox", "w1:p1"),
			script:      func(rc *rcScript) { rc.session = "" },
			wantErr:     notRunning,
			wantMethods: "pane.get",
		},
		{
			name:        "session recorded in another herdr server: not running, herdr untouched",
			sess:        func() api.Session { s := sess("bluebox", "w1:p1"); s.HerdrSession = "other"; return s }(),
			script:      func(rc *rcScript) {},
			wantErr:     notRunning,
			wantMethods: "",
			untouched:   true,
		},
		{
			name:        "no herdr info recorded: not running, herdr untouched",
			sess:        sess("bluebox", ""),
			script:      func(rc *rcScript) {},
			wantErr:     notRunning,
			wantMethods: "",
			untouched:   true,
		},
		{
			name:    "herdr is not running: not running",
			sess:    sess("bluebox", "w1:p1"),
			noHerdr: true,
			wantErr: notRunning,
		},
		{
			name:        "the server says ended but the pane still runs it: herdr decides",
			sess:        func() api.Session { s := sess("bluebox", "w1:p1"); s.Status = api.StatusEnded; return s }(),
			script:      func(rc *rcScript) {},
			wantOut:     []string{rcURL},
			wantMethods: "pane.get,pane.read,pane.read,agent.prompt,pane.read,pane.read",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sock := deadSock
			var fh *fakeHerdr
			if !tc.noHerdr {
				rc := newRCScript(t)
				tc.script(rc)
				fh = rc.herdr(t)
				sock = fh.path
			}
			e, out := newEnv(t, hubServer(t, tc.sess), "bluebox", sock, nil)
			fastPoll(e)
			err := e.remoteControlRun(context.Background(), uuid[:8])
			if tc.wantErr == "" && err != nil {
				t.Fatalf("err = %v\n%s", err, out)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			for _, w := range tc.wantOut {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tc.notOut {
				if strings.Contains(out.String(), w) {
					t.Errorf("output has %q:\n%s", w, out)
				}
			}
			if fh != nil {
				got := strings.Join(fh.methods(), ",")
				if tc.untouched && got != "" {
					t.Errorf("herdr was called: %s", got)
				}
				if !strings.HasPrefix(got, tc.wantMethods) {
					t.Errorf("herdr methods = %s, want prefix %s", got, tc.wantMethods)
				}
			}
		})
	}
}

// The prompt goes to the session's pane with the exact text, and the
// baseline read happens before it.
func TestRemoteControlRequestParams(t *testing.T) {
	rc := newRCScript(t)
	fh := rc.herdr(t)
	e, _ := newEnv(t, hubServer(t, sess("bluebox", "w1:p1")), "bluebox", fh.path, nil)
	fastPoll(e)
	if err := e.remoteControlRun(context.Background(), uuid[:8]); err != nil {
		t.Fatal(err)
	}
	if p := fh.params("agent.prompt"); p["target"] != "w1:p1" || p["text"] != "/remote-control" || len(p) != 2 {
		t.Errorf("agent.prompt params = %v", p)
	}
	if p := fh.params("pane.read"); p["pane_id"] != "w1:p1" || p["source"] != "recent_unwrapped" || p["lines"] != float64(120) {
		t.Errorf("pane.read params = %v", p)
	}
	if rc.readsBefore != 1 {
		t.Errorf("reads before the prompt = %d, want 1", rc.readsBefore)
	}
}

func TestRemoteControlHostileOutputIsCleaned(t *testing.T) {
	rc := newRCScript(t)
	rc.before = "nothing here"
	rc.after = "\x1b]0;pwned\a\x1b[2Jhttps://evil.example/code/session_bad " +
		"see https://claude.ai/code/session_Good-1_x\x1b[31m;rm -rf ~ and more"
	fh := rc.herdr(t)
	e, out := newEnv(t, hubServer(t, sess("bluebox", "w1:p1")), "bluebox", fh.path, nil)
	fastPoll(e)
	if err := e.remoteControlRun(context.Background(), uuid[:8]); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\a") {
		t.Errorf("control characters reached the output: %q", out)
	}
	if !strings.Contains(out.String(), "https://claude.ai/code/session_Good-1_x\n") || strings.Contains(out.String(), "evil") || strings.Contains(out.String(), "rm -rf") {
		t.Errorf("output: %q", out)
	}
}

func TestExtractRemoteControlURL(t *testing.T) {
	tests := []struct {
		name, before, after, want string
	}{
		{"new URL", "", "x " + rcURL + " y", rcURL},
		{"already there", rcURL, rcURL, ""},
		{"new one next to an old one", rcURL, rcURL + "\nhttps://claude.ai/code/session_New1", "https://claude.ai/code/session_New1"},
		{"wrong host", "", "https://claude.ai.evil.example/code/session_x https://evil.example/https://claude.ai/code/session_", ""},
		{"empty ID", "", "https://claude.ai/code/session_ ", ""},
		{"wrong path", "", "https://claude.ai/code/other_abc https://claude.ai/chat/session_abc", ""},
		{"stops at punctuation", "", "at https://claude.ai/code/session_abc123.", "https://claude.ai/code/session_abc123"},
		{"control characters are removed first", "", "\x1b[1mhttps://claude.ai/code/session_a1\x1b[0m", "https://claude.ai/code/session_a1"},
		{"newline ends the URL", "", "https://claude.ai/code/session_a1\nb2", "https://claude.ai/code/session_a1"},
		{"http is refused", "", "http://claude.ai/code/session_a1", ""},
		{"a control character does not glue two IDs", "", "https://claude.ai/code/session_a1\x07abc", "https://claude.ai/code/session_a1"},
		{"a bidi control does not glue two IDs", "", "https://claude.ai/code/session_a1‮abc", "https://claude.ai/code/session_a1"},
		{"none", "", "no url", ""},
	}
	for _, tt := range tests {
		if got := newRemoteControlURL(tt.before, tt.after); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestRemoteControlRemoteForms(t *testing.T) {
	sessions := []struct {
		name   string
		sess   api.Session
		saved  []SavedMachine
		want   []string
		notOut []string
	}{
		{
			name: "no saved machine: ssh only",
			sess: sess("tower", "w1:p1"),
			want: []string{"ssh -t tower.example.com '~/.local/bin/sessionhub remote-control " + uuid + "'"},
			notOut: []string{
				"--machine",
			},
		},
		{
			name:  "saved machine matches: also the herdr prompt form",
			sess:  sess("tower", "w1:p1"),
			saved: []SavedMachine{{Label: "my tower", SSHTarget: "me@tower.example.com:2222"}},
			want: []string{
				"ssh -t tower.example.com '~/.local/bin/sessionhub remote-control " + uuid + "'",
				"herdr --machine 'my tower' agent prompt w1:p1 /remote-control",
			},
		},
		{
			name:   "saved machine but no pane recorded: ssh only",
			sess:   sess("tower", ""),
			saved:  []SavedMachine{{Label: "tower", SSHTarget: "tower.example.com"}},
			want:   []string{"ssh -t tower.example.com '~/.local/bin/sessionhub remote-control " + uuid + "'"},
			notOut: []string{"--machine"},
		},
	}
	for _, tc := range sessions {
		t.Run(tc.name, func(t *testing.T) {
			// herdr must not be touched: the socket does not exist.
			e, out := newEnv(t, hubServer(t, tc.sess), "bluebox", filepath.Join(t.TempDir(), "none.sock"), tc.saved)
			if err := e.remoteControlRun(context.Background(), uuid[:8]); err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tc.notOut {
				if strings.Contains(out.String(), w) {
					t.Errorf("output has %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestRemoteControlErrors(t *testing.T) {
	a := sess("bluebox", "")
	b := sess("bluebox", "")
	b.ID = "0a1b2c3d-9999-4222-8333-444455556666"
	srv := hubServer(t, a, b)
	e, _ := newEnv(t, srv, "bluebox", "", nil)
	if err := e.remoteControlRun(context.Background(), "0a1b"); err == nil || !strings.Contains(err.Error(), b.ID) {
		t.Errorf("ambiguous prefix: err = %v, want candidate IDs", err)
	}
	if err := e.remoteControlRun(context.Background(), "ffff"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("unknown prefix: err = %v", err)
	}
}
