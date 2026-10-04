package resume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// ctlScript scripts herdr for Control.
type ctlScript struct {
	mu        sync.Mutex
	pane      string           // the pane pane.get knows ("" for none)
	agent     string           // its detected agent ("" at a shell prompt)
	session   string           // its agent_session
	snapPanes []map[string]any // panes in session.snapshot
	snapErr   string
	promptErr string
	wsErr     string
	startErr  string
	before    string // pane text until /remote-control is sent or Claude starts
	after     string // pane text from then on
	// redraw is the pane text of the first read after agent.start returns:
	// live, the resumed conversation is on screen then, but not yet the new
	// banner (0 banners right after agent.start, 1 three seconds later).
	redraw    string
	gotPaneID string // the pane_id pane.get reports ("" for the one asked)
	rootPane  string // the new workspace's root pane
	// knownPane is a second pane pane.get knows, with knownAgent and
	// knownSession: the pane a recent resume started the session in.
	knownPane, knownAgent, knownSession string
	// onPrompt runs when agent.prompt succeeds.
	onPrompt   func()
	acted      bool
	started    bool
	startReads int
	// The new workspace's root pane after agent.start: pane.get reports
	// agent claude from its claudeOnGet-th call on (default 1, the first),
	// and startAgent before that. startScreen is what pane.read shows
	// until then. rootGets counts the pane.get calls on the root pane.
	claudeOnGet int
	startAgent  string
	startScreen string
	rootGets    int
	recGets     int // pane.get calls on the recorded pane w1:p1
}

func newCtlScript(t *testing.T) *ctlScript {
	before := fixtureText(t, "pane-read-before-remote-control.ndjson")
	return &ctlScript{
		pane: "w1:p1", agent: "claude", session: uuid,
		before: before,
		after:  fixtureText(t, "pane-read-after-remote-control.ndjson"),
		redraw: before, rootPane: "w9:p1", claudeOnGet: 1,
	}
}

// oldURL is a Remote Control link from an earlier run of the session.
const oldURL = "https://claude.ai/code/session_01OldOldOldOldOldOldOldOld"

// withOldBanner is pane text whose Remote Control banner carries oldURL.
func withOldBanner(t *testing.T) string {
	return strings.ReplaceAll(fixtureText(t, "pane-read-after-remote-control.ndjson"), rcURL, oldURL)
}

// newBanner is the line Claude prints when Remote Control turns on, as
// captured (pane-read-after-remote-control.ndjson), with link u.
func newBanner(u string) string {
	return "\n  /remote-control is active · Continue here, on your phone, or at " + u + "\n"
}

// paneOf is one snapshot pane.
func paneOf(id, sessionUUID, agent string) map[string]any {
	return paneResult(id, sessionUUID, agent)["pane"].(map[string]any)
}

func (c *ctlScript) herdr(t *testing.T) *fakeHerdr {
	return newFakeHerdr(t, func(m string, p map[string]any) (any, string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch m {
		case "pane.get":
			if c.knownPane != "" && p["pane_id"] == c.knownPane {
				return paneResult(c.knownPane, c.knownSession, c.knownAgent), ""
			}
			if c.started && p["pane_id"] == c.rootPane {
				c.rootGets++
				if c.rootGets >= c.claudeOnGet {
					return paneResult(c.rootPane, "", "claude"), ""
				}
				return paneResult(c.rootPane, "", c.startAgent), ""
			}
			if p["pane_id"] == "w1:p1" {
				c.recGets++
			}
			if c.pane != "" && p["pane_id"] == c.pane {
				id := c.pane
				if c.gotPaneID != "" {
					id = c.gotPaneID
				}
				return paneResult(id, c.session, c.agent), ""
			}
			return nil, "pane_not_found"
		case "session.snapshot":
			if c.snapErr != "" {
				return nil, c.snapErr
			}
			panes := c.snapPanes
			if panes == nil {
				panes = []map[string]any{}
			}
			return map[string]any{"type": "session_snapshot", "snapshot": map[string]any{"panes": panes}}, ""
		case "pane.read":
			text := c.before
			switch {
			case c.started && c.rootGets < c.claudeOnGet && c.startScreen != "":
				text = c.startScreen
			case c.started && c.startReads == 0:
				c.startReads++
				text = c.redraw
			case c.acted:
				text = c.after
			}
			return map[string]any{"type": "pane_read", "read": map[string]any{"pane_id": p["pane_id"], "text": text}}, ""
		case "agent.prompt":
			if c.promptErr != "" {
				return nil, c.promptErr
			}
			c.acted = true
			if c.onPrompt != nil {
				c.onPrompt()
			}
			return map[string]any{"type": "agent_prompted"}, ""
		case "pane.send_keys":
			return map[string]any{"type": "ok"}, ""
		case "workspace.create":
			if c.wsErr != "" {
				return nil, c.wsErr
			}
			return map[string]any{"type": "workspace_created",
				"workspace": map[string]any{"workspace_id": "w9"},
				"root_pane": map[string]any{"pane_id": c.rootPane, "workspace_id": "w9"}}, ""
		case "agent.start":
			if c.startErr != "" {
				return nil, c.startErr
			}
			c.acted, c.started = true, true
			return map[string]any{"type": "agent_started"}, ""
		}
		return nil, "no_fixture"
	})
}

func fastControl(sock string) ControlOptions {
	return ControlOptions{Socket: sock, Resume: true, PollEvery: 5 * time.Millisecond, PollFor: 150 * time.Millisecond}
}

func TestControlDecisionTable(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone")
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadSock := filepath.Join(t.TempDir(), "none.sock")
	at := func(status, cwd string) api.Session {
		s := sess("tower", "w1:p1")
		s.Status, s.CWD = status, cwd
		return s
	}
	cases := []struct {
		name     string
		sess     api.Session
		script   func(c *ctlScript)
		noHerdr  bool
		want     Outcome
		wantURL  string
		wantPane string
		errHas   string
		noRecGet bool     // pane.get must not look up the recorded pane w1:p1
		rootGets int      // pane.get calls on the new pane, when not 0
		wantWS   string   // ControlResult.Workspace, when not ""
		known    string   // ControlOptions.KnownPane
		calls    []string // herdr methods that must run
		never    []string // herdr methods that must not run
	}{
		{name: "Claude runs it in the recorded pane: inject",
			sess: at(api.StatusLive, dir), script: func(c *ctlScript) {},
			want: OutcomeLinked, wantURL: rcURL, wantPane: "w1:p1",
			calls: []string{"pane.get", "agent.prompt"}, never: []string{"session.snapshot", "workspace.create", "agent.start"}},
		{name: "the pane moved since the heartbeat: found in a fresh snapshot, inject there",
			sess: at(api.StatusLive, dir),
			script: func(c *ctlScript) {
				c.pane = ""
				c.snapPanes = []map[string]any{
					paneOf("w2:p4", "99999999-0000-4000-8000-000000000000", "claude"),
					paneOf("w2:p3", uuid, "claude"),
				}
			},
			want: OutcomeLinked, wantURL: rcURL, wantPane: "w2:p3",
			calls: []string{"pane.get", "session.snapshot", "agent.prompt"}, never: []string{"workspace.create", "agent.start"}},
		{name: "blocked on a prompt: nothing typed, nothing started",
			sess: at(api.StatusBlocked, dir), script: func(c *ctlScript) { c.promptErr = "agent_blocked" },
			want: OutcomeBlocked, calls: []string{"agent.prompt"}, never: []string{"workspace.create", "agent.start", "pane.send_keys"}},
		{name: "a Remote Control dialog is already open: nothing sent",
			sess:   at(api.StatusLive, dir),
			script: func(c *ctlScript) { c.before = fixtureText(t, "pane-read-remote-control-dialog.ndjson") },
			want:   OutcomeDialogOpen, never: []string{"agent.prompt", "pane.send_keys", "workspace.create"}},
		{name: "Remote Control was already on: the link from the dialog, dialog closed",
			sess:   at(api.StatusLive, dir),
			script: func(c *ctlScript) { c.after = fixtureText(t, "pane-read-remote-control-dialog.ndjson") },
			want:   OutcomeAlreadyOn, wantURL: rcURL, calls: []string{"agent.prompt", "pane.send_keys"}, never: []string{"workspace.create"}},
		{name: "ended and the pane is gone: resume in a new workspace",
			sess: at(api.StatusEnded, dir), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeResumed, wantURL: rcURL, wantPane: "w9:p1",
			calls: []string{"pane.get", "session.snapshot", "workspace.create", "agent.start", "pane.read"}, never: []string{"agent.prompt"}},
		{name: "resumed, but no link within the wait",
			sess: at(api.StatusEnded, dir), script: func(c *ctlScript) { c.pane = ""; c.after = "starting" },
			want: OutcomeResumed, wantPane: "w9:p1", calls: []string{"agent.start"}},
		{name: "stale and no pane runs it: resume",
			sess: at(api.StatusStale, dir), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeResumed, wantURL: rcURL, calls: []string{"workspace.create"}},
		{name: "the pane kept the session at a shell prompt: resume in a new workspace, not there",
			sess: at(api.StatusEnded, dir), script: func(c *ctlScript) { c.agent = "" },
			want: OutcomeResumed, wantPane: "w9:p1", wantURL: rcURL,
			calls: []string{"workspace.create"}, never: []string{"agent.prompt"}},
		{name: "live but no pane runs it: looks live, start nothing",
			sess: at(api.StatusLive, dir), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeLooksLive, calls: []string{"session.snapshot"}, never: []string{"workspace.create", "agent.start", "agent.prompt"}},
		{name: "blocked but no pane runs it: looks live, start nothing",
			sess: at(api.StatusBlocked, dir), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeLooksLive, never: []string{"workspace.create", "agent.start"}},
		{name: "the directory is gone: create nothing",
			sess: at(api.StatusEnded, gone), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeNoDir, never: []string{"workspace.create", "agent.start"}},
		{name: "the directory is a file: create nothing",
			sess: at(api.StatusEnded, file), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeNoDir, never: []string{"workspace.create", "agent.start"}},
		{name: "no directory recorded: create nothing",
			sess: at(api.StatusEnded, ""), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeNoDir, never: []string{"workspace.create", "agent.start"}},
		{name: "workspace.create fails: no agent.start",
			sess: at(api.StatusEnded, dir), script: func(c *ctlScript) { c.pane = ""; c.wsErr = "internal_error" },
			want: OutcomeError, errHas: "create workspace", never: []string{"agent.start"}},
		{name: "agent.start fails: the error names the new workspace",
			sess: at(api.StatusEnded, dir), script: func(c *ctlScript) { c.pane = ""; c.startErr = "pane_not_ready" },
			want: OutcomeError, errHas: "w9", calls: []string{"workspace.create", "agent.start"}},
		// The defect from tower: zsh's compinit prompt ate the first
		// character, so the pane ran "laude" and Claude never started.
		{name: "claude never appears in the new pane: error with the screen's last line, pane recorded, no prompt",
			sess: at(api.StatusEnded, dir),
			script: func(c *ctlScript) {
				c.pane = ""
				c.claudeOnGet = 1 << 30
				c.startScreen = "tower% laude --resume x --remote-control\nzsh: command not found: laude\n\n  \n"
			},
			want: OutcomeError, errHas: "claude didn't start in the new workspace w9: zsh: command not found: laude",
			wantPane: "w9:p1", wantWS: "w9",
			calls: []string{"agent.start", "pane.read"}, never: []string{"agent.prompt"}},
		{name: "claude never appears and the screen ends in the next shell prompt: the line before it",
			sess: at(api.StatusEnded, dir),
			script: func(c *ctlScript) {
				c.pane = ""
				c.claudeOnGet = 1 << 30
				c.startScreen = "zsh: command not found: laude\ntower% \n\n"
			},
			want: OutcomeError, errHas: "claude didn't start in the new workspace w9: zsh: command not found: laude",
			wantPane: "w9:p1", wantWS: "w9", never: []string{"agent.prompt"}},
		{name: "claude never appears and the screen holds only a prompt: the prompt line",
			sess: at(api.StatusEnded, dir),
			script: func(c *ctlScript) {
				c.pane = ""
				c.claudeOnGet = 1 << 30
				c.startScreen = "\nuser@host:~$ \n"
			},
			want: OutcomeError, errHas: "claude didn't start in the new workspace w9: user@host:~$",
			wantPane: "w9:p1", wantWS: "w9", never: []string{"agent.prompt"}},
		{name: "claude appears on the second pane.get: resumed",
			sess: at(api.StatusEnded, dir),
			script: func(c *ctlScript) {
				c.pane = ""
				c.claudeOnGet = 2
				c.startScreen = "starting\n"
			},
			want: OutcomeResumed, wantURL: rcURL, wantPane: "w9:p1", wantWS: "w9", rootGets: 2,
			calls: []string{"agent.start"}, never: []string{"agent.prompt"}},
		{name: "the snapshot fails: start nothing",
			sess: at(api.StatusEnded, dir), script: func(c *ctlScript) { c.pane = ""; c.snapErr = "internal_error" },
			want: OutcomeError, errHas: "snapshot", never: []string{"workspace.create"}},
		{name: "recorded in another herdr server: skip pane.get, look in the snapshot",
			sess:   func() api.Session { s := at(api.StatusEnded, dir); s.HerdrSession = "other"; return s }(),
			script: func(c *ctlScript) {},
			want:   OutcomeResumed, wantURL: rcURL, calls: []string{"session.snapshot", "workspace.create"}, noRecGet: true},
		{name: "herdr is not running",
			sess: at(api.StatusEnded, dir), noHerdr: true, want: OutcomeError, errHas: "herdr is not running"},
		{name: "a relative directory (.): create nothing",
			sess: at(api.StatusEnded, "."), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeNoDir, never: []string{"workspace.create", "agent.start"}},
		{name: "a relative directory (repo): create nothing",
			sess: at(api.StatusEnded, "repo"), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeNoDir, never: []string{"workspace.create", "agent.start"}},
		{name: "the recorded pane runs another Claude session: never inject there; resume",
			sess:   at(api.StatusEnded, dir),
			script: func(c *ctlScript) { c.session = "99999999-0000-4000-8000-000000000000" },
			want:   OutcomeResumed, wantURL: rcURL, wantPane: "w9:p1",
			calls: []string{"pane.get", "workspace.create"}, never: []string{"agent.prompt"}},
		{name: "the recorded pane runs another Claude session, server says live: never inject, start nothing",
			sess:   at(api.StatusLive, dir),
			script: func(c *ctlScript) { c.session = "99999999-0000-4000-8000-000000000000" },
			want:   OutcomeLooksLive, never: []string{"agent.prompt", "workspace.create", "agent.start"}},
		{name: "herdr reports a bad ID for the recorded pane: send nothing, start nothing",
			sess:   at(api.StatusEnded, dir),
			script: func(c *ctlScript) { c.gotPaneID = "-w1:p1" },
			want:   OutcomeError, errHas: "not a herdr pane ID", never: []string{"pane.read", "agent.prompt", "workspace.create", "agent.start"}},
		{name: "a snapshot pane with a bad ID runs it: start nothing",
			sess: at(api.StatusEnded, dir),
			script: func(c *ctlScript) {
				c.pane = ""
				c.snapPanes = []map[string]any{paneOf("-w2:p3", uuid, "claude")}
			},
			want: OutcomeError, errHas: "not a herdr pane ID", never: []string{"agent.prompt", "workspace.create", "agent.start"}},
		{name: "the new workspace's root pane has a bad ID: no agent.start",
			sess: at(api.StatusEnded, dir), script: func(c *ctlScript) { c.pane = ""; c.rootPane = "-w9:p1" },
			want: OutcomeError, errHas: "w9", calls: []string{"workspace.create"}, never: []string{"agent.start"}},
		{name: "resume redraws an old banner, then the new one appears: the new link",
			sess: at(api.StatusEnded, dir),
			script: func(c *ctlScript) {
				c.pane = ""
				c.redraw = withOldBanner(t)
				c.after = c.redraw + newBanner(rcURL)
			},
			want: OutcomeResumed, wantURL: rcURL, wantPane: "w9:p1"},
		{name: "resume redraws an old banner and no new one appears: no link, never the old one",
			sess: at(api.StatusEnded, dir),
			script: func(c *ctlScript) {
				c.pane = ""
				c.redraw = withOldBanner(t)
				c.after = c.redraw
			},
			want: OutcomeResumed, wantPane: "w9:p1"},
		// A resume moments ago: herdr may not report the session in its
		// pane yet. Never start a second copy.
		{name: "the known pane runs the session: inject there",
			sess: at(api.StatusEnded, dir), known: "w9:p1",
			script: func(c *ctlScript) { c.pane = ""; c.knownPane, c.knownAgent, c.knownSession = "w9:p1", "claude", uuid },
			want:   OutcomeLinked, wantURL: rcURL, wantPane: "w9:p1",
			calls: []string{"agent.prompt"}, never: []string{"workspace.create", "agent.start"}},
		{name: "the known pane runs Claude, session not reported yet: inject there",
			sess: at(api.StatusEnded, dir), known: "w9:p1",
			script: func(c *ctlScript) { c.pane = ""; c.knownPane, c.knownAgent = "w9:p1", "claude" },
			want:   OutcomeLinked, wantURL: rcURL, wantPane: "w9:p1",
			calls: []string{"agent.prompt"}, never: []string{"workspace.create", "agent.start"}},
		{name: "the known pane runs Claude, server says live: inject there, not looks live",
			sess: at(api.StatusLive, dir), known: "w9:p1",
			script: func(c *ctlScript) { c.pane = ""; c.knownPane, c.knownAgent = "w9:p1", "claude" },
			want:   OutcomeLinked, wantURL: rcURL, wantPane: "w9:p1",
			calls: []string{"agent.prompt"}, never: []string{"workspace.create", "agent.start"}},
		{name: "the known pane is at a shell: just resumed, start nothing",
			sess: at(api.StatusEnded, dir), known: "w9:p1",
			script: func(c *ctlScript) { c.pane = ""; c.knownPane = "w9:p1" },
			want:   OutcomeJustResumed, never: []string{"agent.prompt", "workspace.create", "agent.start"}},
		{name: "the known pane is gone: just resumed, start nothing",
			sess: at(api.StatusEnded, dir), known: "w9:p1",
			script: func(c *ctlScript) { c.pane = "" },
			want:   OutcomeJustResumed, never: []string{"agent.prompt", "workspace.create", "agent.start"}},
		{name: "the known pane runs another Claude session: just resumed, send nothing",
			sess: at(api.StatusEnded, dir), known: "w9:p1",
			script: func(c *ctlScript) {
				c.pane = ""
				c.knownPane, c.knownAgent, c.knownSession = "w9:p1", "claude", "99999999-0000-4000-8000-000000000000"
			},
			want: OutcomeJustResumed, never: []string{"agent.prompt", "workspace.create", "agent.start"}},
		{name: "the known pane is at a shell but the recorded pane runs it: inject in the recorded pane",
			sess: at(api.StatusEnded, dir), known: "w9:p1",
			script: func(c *ctlScript) { c.knownPane = "w9:p1" },
			want:   OutcomeLinked, wantURL: rcURL, wantPane: "w1:p1",
			never: []string{"workspace.create", "agent.start"}},
		{name: "the known pane is gone but a snapshot pane runs it: inject there",
			sess: at(api.StatusEnded, dir), known: "w9:p1",
			script: func(c *ctlScript) { c.pane = ""; c.snapPanes = []map[string]any{paneOf("w2:p3", uuid, "claude")} },
			want:   OutcomeLinked, wantURL: rcURL, wantPane: "w2:p3",
			never: []string{"workspace.create", "agent.start"}},
		{name: "a bad known pane ID: touch nothing",
			sess: at(api.StatusEnded, dir), known: "-w9:p1",
			script: func(c *ctlScript) { c.pane = "" },
			want:   OutcomeError, errHas: "not a herdr pane ID",
			never: []string{"pane.get", "agent.prompt", "workspace.create", "agent.start"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sock := deadSock
			var fh *fakeHerdr
			var c *ctlScript
			if !tc.noHerdr {
				c = newCtlScript(t)
				tc.script(c)
				fh = c.herdr(t)
				sock = fh.path
			}
			o := fastControl(sock)
			o.KnownPane = tc.known
			r := Control(context.Background(), tc.sess, o)
			if r.Outcome != tc.want || r.URL != tc.wantURL {
				t.Fatalf("outcome %s url %q err %v; want %s %q", r.Outcome, r.URL, r.Err, tc.want, tc.wantURL)
			}
			if tc.wantPane != "" && r.Pane != tc.wantPane {
				t.Errorf("pane %q, want %q", r.Pane, tc.wantPane)
			}
			if tc.wantWS != "" && r.Workspace != tc.wantWS {
				t.Errorf("workspace %q, want %q", r.Workspace, tc.wantWS)
			}
			if tc.noRecGet {
				c.mu.Lock()
				got := c.recGets
				c.mu.Unlock()
				if got != 0 {
					t.Errorf("pane.get looked up the recorded pane %d times, want 0", got)
				}
			}
			if tc.rootGets != 0 {
				c.mu.Lock()
				got := c.rootGets
				c.mu.Unlock()
				if got != tc.rootGets {
					t.Errorf("pane.get on the new pane ran %d times, want %d", got, tc.rootGets)
				}
			}
			if tc.errHas == "" && r.Err != nil {
				t.Errorf("err %v", r.Err)
			}
			if tc.errHas != "" && (r.Err == nil || !strings.Contains(r.Err.Error(), tc.errHas)) {
				t.Errorf("err %v, want %q", r.Err, tc.errHas)
			}
			if fh != nil {
				got := "," + strings.Join(fh.methods(), ",") + ","
				for _, m := range tc.calls {
					if !strings.Contains(got, ","+m+",") {
						t.Errorf("%s never ran; methods %s", m, got)
					}
				}
				for _, m := range tc.never {
					if strings.Contains(got, ","+m+",") {
						t.Errorf("%s ran; methods %s", m, got)
					}
				}
			}
		})
	}
}

// A context that ends after /remote-control was sent ends the wait: the
// command went out, so that is "no link", not an error.
func TestControlCancelAfterPromptIsNoLink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newCtlScript(t)
	c.after = c.before // no link ever appears
	c.onPrompt = cancel
	fh := c.herdr(t)
	o := fastControl(fh.path)
	o.PollFor = 5 * time.Second
	start := time.Now()
	r := Control(ctx, sess("tower", "w1:p1"), o)
	if r.Outcome != OutcomeNoLink || r.Err != nil || r.Pane != "w1:p1" {
		t.Fatalf("result %+v", r)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("waited %s after the context ended", time.Since(start))
	}
}

// The resume path: the exact workspace.create and agent.start params.
func TestControlResumeParams(t *testing.T) {
	dir := t.TempDir()
	c := newCtlScript(t)
	c.pane = ""
	fh := c.herdr(t)
	s := sess("tower", "w1:p1")
	s.CWD, s.Title = dir, "fix CI token rotation"
	r := Control(context.Background(), s, fastControl(fh.path))
	if r.Outcome != OutcomeResumed || r.Workspace != "w9" || r.Pane != "w9:p1" {
		t.Fatalf("result %+v", r)
	}
	if p := fh.params("workspace.create"); p["cwd"] != dir || p["label"] != "sessionhub: fix CI token rotation" || p["focus"] != false || len(p) != 3 {
		t.Errorf("workspace.create params = %v", p)
	}
	p := fh.params("agent.start")
	args, _ := json.Marshal(p["args"])
	if p["name"] != "sessionhub-0a1b2c3d" || p["kind"] != "claude" || p["pane_id"] != "w9:p1" ||
		string(args) != `["--resume","`+uuid+`","--remote-control"]` || len(p) != 4 {
		t.Errorf("agent.start params = %v", p)
	}
	if p := fh.params("pane.read"); p["pane_id"] != "w9:p1" {
		t.Errorf("pane.read params = %v", p)
	}
}

func TestWorkspaceLabel(t *testing.T) {
	for _, tc := range []struct{ title, want string }{
		{"fix CI token rotation", "sessionhub: fix CI token rotation"},
		{"", "sessionhub: 0a1b2c3d"},
		{" \t ", "sessionhub: 0a1b2c3d"},
		{"a\x1b[31mb\nc", "sessionhub: a[31mb c"},
		{strings.Repeat("x", 50), "sessionhub: " + strings.Repeat("x", 39) + "…"},
	} {
		s := sess("tower", "")
		s.Title = tc.title
		if got := workspaceLabel(s); got != tc.want {
			t.Errorf("title %q: label %q, want %q", tc.title, got, tc.want)
		}
	}
}

// The new workspace opens in the cleaned directory.
func TestControlCleansDirectory(t *testing.T) {
	dir := t.TempDir()
	c := newCtlScript(t)
	c.pane = ""
	fh := c.herdr(t)
	s := sess("tower", "w1:p1")
	s.CWD = dir + "/sub/../"
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := Control(context.Background(), s, fastControl(fh.path))
	if r.Outcome != OutcomeResumed {
		t.Fatalf("result %+v", r)
	}
	if p := fh.params("workspace.create"); p["cwd"] != dir {
		t.Errorf("workspace.create cwd = %v, want %s", p["cwd"], dir)
	}
}

// newBannerURL takes the link only from a banner line that was not on screen
// right after agent.start. The resumed Claude can reuse the earlier link
// (seen live on bluebox), so the rule counts banner lines, never compares URLs.
func TestNewBannerURL(t *testing.T) {
	history := "❯ /remote-control\n❯ Reply with the single word ok\n● ok\n"
	old := newBanner(oldURL)
	for _, tc := range []struct{ name, before, after, want string }{
		{"the new banner after the redraw", history, history + newBanner(rcURL), rcURL},
		{"an old banner, history, then the new banner", old + history, old + history + newBanner(rcURL), rcURL},
		{"the new banner reuses the old link", old + history, old + history + newBanner(oldURL), oldURL},
		{"only the old banner, redrawn before the baseline", old + history, old + history, ""},
		{"only the old banner, the window slid", old + history, history + "more\n", ""},
		{"the window slid as the new banner came: missed, never stale", old + history, history + newBanner(rcURL), ""},
		{"a link quoted in the conversation, no banner", history, history + "see " + rcURL + "\n", ""},
		{"a new banner line without a link", history, history + "  /remote-control is active\n", ""},
		{"nothing yet", history, history, ""},
		{"two new banners: the last one", history, history + newBanner(oldURL) + newBanner(rcURL), rcURL},
	} {
		if got := newBannerURL(tc.before, tc.after); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The CLI turns every outcome into output or an error; an outcome it doesn't
// expect is an error, never a silent exit 0.
func TestRemoteControlLocalReportsEveryOutcome(t *testing.T) {
	s := sess("tower", "w1:p1")
	for _, tc := range []struct {
		r       ControlResult
		wantErr string
		wantOut string
	}{
		{ControlResult{Outcome: OutcomeLinked, URL: rcURL}, "", rcURL},
		{ControlResult{Outcome: OutcomeAlreadyOn, URL: rcURL, DialogLeftOpen: "Press Esc in the pane."}, "", "Press Esc in the pane."},
		{ControlResult{Outcome: OutcomeNoLink, Pane: "w1:p1"}, "", "no Remote Control URL appeared"},
		{ControlResult{Outcome: OutcomeBlocked}, "waiting on a prompt", ""},
		{ControlResult{Outcome: OutcomeDialogOpen}, "dialog is open", ""},
		{ControlResult{Outcome: OutcomeNotRunning}, "not running", ""},
		{ControlResult{Outcome: OutcomeError, Err: errors.New("boom")}, "boom", ""},
		{ControlResult{Outcome: OutcomeError}, "failed", ""},
		{ControlResult{Outcome: OutcomeResumed}, "unexpected", ""},
		{ControlResult{Outcome: OutcomeLooksLive}, "unexpected", ""},
		{ControlResult{Outcome: OutcomeNoDir}, "unexpected", ""},
		{ControlResult{Outcome: OutcomeJustResumed}, "unexpected", ""},
		{ControlResult{Outcome: "bogus"}, "unexpected", ""},
	} {
		out := &bytes.Buffer{}
		e := &env{out: out}
		err := e.reportControl(s, ControlOptions{}, tc.r)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: err %v", tc.r.Outcome, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err %v, want %q", tc.r.Outcome, err, tc.wantErr)
		}
		if !strings.Contains(out.String(), tc.wantOut) {
			t.Errorf("%s: output %q, want %q", tc.r.Outcome, out.String(), tc.wantOut)
		}
		if tc.wantErr != "" && out.Len() != 0 {
			t.Errorf("%s: printed %q with an error", tc.r.Outcome, out.String())
		}
	}
}

// A context that ends while the wait for Claude runs is the watcher
// stopping, not a shell that failed: no "claude didn't start", no screen
// line, and the pane stays recorded.
func TestControlCancelWhileWaitingForClaude(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newCtlScript(t)
	c.pane = ""
	c.claudeOnGet = 1 << 30
	c.startScreen = "zsh: command not found: laude\n"
	fh := c.herdr(t)
	o := fastControl(fh.path)
	o.PollFor = 5 * time.Second
	start := time.Now()
	s := sess("tower", "w1:p1")
	s.CWD = t.TempDir()
	r := Control(ctx, s, o)
	if r.Outcome != OutcomeError || r.Workspace != "w9" || r.Pane != "w9:p1" {
		t.Fatalf("result %+v", r)
	}
	if r.Err == nil || !strings.Contains(r.Err.Error(), "watcher stopped before Claude was confirmed") ||
		strings.Contains(r.Err.Error(), "didn't start") || strings.Contains(r.Err.Error(), "command not found") {
		t.Errorf("err %v", r.Err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("waited %s after the context ended", time.Since(start))
	}
}
