package resume

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

func TestStartResumed(t *testing.T) {
	dir := t.TempDir()
	at := func(cwd, herdrSession string) api.Session {
		s := sess("tower", "w1:p1")
		s.CWD, s.HerdrSession = cwd, herdrSession
		return s
	}
	cases := []struct {
		name     string
		sess     api.Session
		script   func(c *ctlScript)
		noHerdr  bool
		want     Outcome
		wantPane string
		errHas   string
		calls    []string
		never    []string
		noRecGet bool // pane.get must not look up the recorded pane w1:p1
	}{
		{name: "no pane runs it and its pane is gone: a new workspace",
			sess: at(dir, "default"), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeResumed, wantPane: "w9:p1",
			calls: []string{"session.snapshot", "workspace.create", "agent.start"}, never: []string{"agent.prompt", "pane.read"}},
		{name: "its pane sits at a shell: start there",
			sess: at(dir, "default"), script: func(c *ctlScript) { c.agent = ""; c.rootPane = "w1:p1" },
			want: OutcomeResumed, wantPane: "w1:p1",
			calls: []string{"session.snapshot", "pane.get", "agent.start"}, never: []string{"workspace.create", "agent.prompt"}},
		{name: "its pane runs another program: a new workspace",
			sess: at(dir, "default"), script: func(c *ctlScript) { c.agent = "codex" },
			want: OutcomeResumed, wantPane: "w9:p1", calls: []string{"workspace.create", "agent.start"}},
		{name: "its pane belongs to another herdr server: a new workspace, no lookup",
			sess: at(dir, "other"), script: func(c *ctlScript) { c.agent = "" },
			want: OutcomeResumed, wantPane: "w9:p1", calls: []string{"workspace.create"}, noRecGet: true},
		{name: "a pane already runs it: start nothing",
			sess: at(dir, "default"),
			script: func(c *ctlScript) {
				c.pane = ""
				c.snapPanes = []map[string]any{paneOf("w2:p3", uuid, "claude")}
			},
			want: OutcomeRunning, wantPane: "w2:p3", never: []string{"workspace.create", "agent.start"}},
		{name: "its pane runs Claude with no session ID yet: start nothing",
			sess: at(dir, "default"), script: func(c *ctlScript) { c.session = "" },
			want: OutcomeRunning, wantPane: "w1:p1", never: []string{"workspace.create", "agent.start"}},
		{name: "its pane runs Claude on this session, missing from the snapshot: start nothing",
			sess: at(dir, "default"),
			want: OutcomeRunning, wantPane: "w1:p1", never: []string{"workspace.create", "agent.start"}},
		{name: "its pane runs Claude on another session: a new workspace",
			sess: at(dir, "default"), script: func(c *ctlScript) { c.session = "99999999-2222-4222-8333-000000000000" },
			want: OutcomeResumed, wantPane: "w9:p1", calls: []string{"workspace.create", "agent.start"}},
		{name: "the directory is gone: create nothing",
			sess: at(filepath.Join(dir, "gone"), "default"), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeNoDir, never: []string{"workspace.create", "agent.start"}},
		{name: "herdr is not running",
			sess: at(dir, "default"), noHerdr: true, want: OutcomeError, errHas: "herdr is not running"},
		{name: "claude never appears: an error, no second start",
			sess: at(dir, "default"), script: func(c *ctlScript) { c.pane = ""; c.claudeOnGet = 1000 },
			want: OutcomeError, wantPane: "w9:p1", errHas: "didn't start"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCtlScript(t)
			if tc.script != nil {
				tc.script(c)
			}
			fh := c.herdr(t)
			sock := fh.path
			if tc.noHerdr {
				sock = filepath.Join(t.TempDir(), "none.sock")
			}
			r := StartResumed(context.Background(), sock, tc.sess, fastControl(sock))
			if r.Outcome != tc.want || (tc.wantPane != "" && r.Pane != tc.wantPane) {
				t.Fatalf("got %s pane %q err %v; want %s pane %q", r.Outcome, r.Pane, r.Err, tc.want, tc.wantPane)
			}
			if tc.errHas != "" && (r.Err == nil || !strings.Contains(r.Err.Error(), tc.errHas)) {
				t.Errorf("err %v, want %q", r.Err, tc.errHas)
			}
			if tc.noHerdr {
				return
			}
			m := fh.methods()
			for _, want := range tc.calls {
				if !slices.Contains(m, want) {
					t.Errorf("%s not called; calls %v", want, m)
				}
			}
			for _, bad := range tc.never {
				if slices.Contains(m, bad) {
					t.Errorf("%s called; calls %v", bad, m)
				}
			}
			if tc.noRecGet && c.recGets != 0 {
				t.Errorf("looked up the recorded pane %d times", c.recGets)
			}
			if n := countOf(m, "agent.start"); n > 1 {
				t.Errorf("agent.start called %d times", n)
			}
			if p := fh.params("agent.start"); p != nil {
				args, _ := p["args"].([]any)
				if len(args) != 2 || args[0] != "--resume" || args[1] != uuid {
					t.Errorf("agent.start args %v, want --resume %s and nothing else", args, uuid)
				}
			}
			if p := fh.params("workspace.create"); p != nil && p["focus"] != false {
				t.Errorf("workspace.create takes focus: %v", p)
			}
		})
	}
}

func countOf(list []string, v string) int {
	n := 0
	for _, x := range list {
		if x == v {
			n++
		}
	}
	return n
}
