package resume

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/claudetrust"
)

func TestResolveStartDir(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	other := filepath.Join(root, "other")
	for _, d := range []string{filepath.Join(home, "Code", "app"), other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(home, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "Code"), filepath.Join(home, "code-link")); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(home, "Code", "app")
	for _, tc := range []struct {
		dir     string
		want    string
		outcome Outcome
	}{
		{"~", home, ""},
		{"~/Code/app", app, ""},
		{app, app, ""},
		{app + "/../app/", app, ""},
		{"~/code-link/app", app, ""}, // a link that stays inside home
		{"~/escape", "", OutcomeOutsideHome},
		{other, "", OutcomeOutsideHome},
		{home + "/../other", "", OutcomeOutsideHome},
		{"/", "", OutcomeOutsideHome},
		{"~/missing", "", OutcomeNoDir},
		{"~/file", "", OutcomeNoDir},
		{"Code/app", "", OutcomeNoDir},
		{"", "", OutcomeNoDir},
	} {
		got, out := ResolveStartDir(tc.dir, home)
		if got != tc.want || out != tc.outcome {
			t.Errorf("ResolveStartDir(%q) = %q, %q; want %q, %q", tc.dir, got, out, tc.want, tc.outcome)
		}
	}
	if _, out := ResolveStartDir("/anything", ""); out != OutcomeNoDir {
		t.Errorf("no home: %q", out)
	}
}

func TestStartNew(t *testing.T) {
	home := t.TempDir()
	app := filepath.Join(home, "app")
	if err := os.Mkdir(app, 0o700); err != nil {
		t.Fatal(err)
	}
	realApp, _ := filepath.EvalSymlinks(app)
	untrusted := filepath.Join(home, "untrusted")
	if err := os.Mkdir(untrusted, 0o700); err != nil {
		t.Fatal(err)
	}
	req := func(dir, prompt string) api.StartRequest {
		return api.StartRequest{ID: "st_AbC-_dEf0123456789xyzw", Dir: dir, Prompt: prompt}
	}
	trusting := func(dir string) api.StartRequest {
		r := req(dir, "")
		r.Trust = true
		return r
	}
	cases := []struct {
		name    string
		req     api.StartRequest
		script  func(c *ctlScript)
		noHerdr bool
		want    Outcome
		wantURL string
		note    string // substring of PromptNote; "" wants none
		errHas  string
		calls   []string
		never   []string
		config  string // Claude's config; "" trusts app
		trusted []string
	}{
		{name: "starts with Remote Control and sends the first prompt",
			req: req("~/app", "fix the build"), want: OutcomeStarted, wantURL: rcURL,
			calls: []string{"workspace.create", "agent.start", "agent.prompt"}, never: []string{"session.snapshot"}},
		{name: "no prompt: nothing submitted",
			req: req(app, ""), want: OutcomeStarted, wantURL: rcURL,
			calls: []string{"workspace.create", "agent.start"}, never: []string{"agent.prompt"}},
		{name: "Claude waits on a prompt: started, first prompt not sent",
			req: req(app, "hi"), script: func(c *ctlScript) { c.promptErr = "agent_blocked" },
			want: OutcomeStarted, wantURL: rcURL, note: "trust"},
		{name: "outside home: nothing created",
			req: req(t.TempDir(), "hi"), want: OutcomeOutsideHome, never: []string{"workspace.create", "agent.start"}},
		{name: "missing directory: nothing created",
			req: req("~/gone", ""), want: OutcomeNoDir, never: []string{"workspace.create", "agent.start"}},
		{name: "Claude never appears: an error, no prompt",
			req: req(app, "hi"), script: func(c *ctlScript) { c.claudeOnGet = 1000 },
			want: OutcomeError, errHas: "didn't start", never: []string{"agent.prompt"}},
		{name: "herdr is not running", req: req(app, ""), noHerdr: true, want: OutcomeError, errHas: "herdr is not running"},
		{name: "untrusted directory: nothing created",
			req: req("~/untrusted", "hi"), want: OutcomeUntrusted, never: []string{"workspace.create", "agent.start"}},
		{name: "no Claude config: untrusted",
			req: req(app, ""), config: "missing", want: OutcomeUntrusted, never: []string{"workspace.create"}},
		{name: "unreadable Claude config: an error, nothing created",
			req: req(app, ""), config: "{broken", want: OutcomeError, errHas: "can't tell whether Claude trusts",
			never: []string{"workspace.create"}},
		{name: "a trusted parent trusts the directory",
			req: req(app, ""), config: `{"projects": {"` + home + `": {"hasTrustDialogAccepted": true}}}`,
			want: OutcomeStarted, wantURL: rcURL, calls: []string{"workspace.create"}},
		{name: "trust marks only the directory, then starts",
			req: trusting("~/untrusted"), want: OutcomeStarted, wantURL: rcURL,
			calls: []string{"workspace.create", "agent.start"}, trusted: []string{untrusted}},
		{name: "trust refuses home: nothing created",
			req: trusting("~"), want: OutcomeError, errHas: "home directory", never: []string{"workspace.create"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCtlScript(t)
			c.pane = ""
			if tc.script != nil {
				tc.script(c)
			}
			fh := c.herdr(t)
			sock := fh.path
			if tc.noHerdr {
				sock = filepath.Join(t.TempDir(), "none.sock")
			}
			config := filepath.Join(t.TempDir(), ".claude.json")
			body := `{"projects": {"` + realApp + `": {"hasTrustDialogAccepted": true}}}`
			if tc.config != "" {
				body = tc.config
			}
			if body != "missing" {
				if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			r := StartNew(context.Background(), sock, home, config, tc.req, fastControl(sock))
			for _, d := range tc.trusted {
				if ok, err := claudetrust.Trusted(config, d); !ok || err != nil {
					t.Errorf("%s not trusted: %v", d, err)
				}
			}
			if tc.req.Trust {
				if ok, _ := claudetrust.Trusted(config, filepath.Dir(untrusted)+"/other"); ok {
					t.Error("trust reached a sibling")
				}
			}
			if r.Outcome != tc.want || r.URL != tc.wantURL {
				t.Fatalf("got %s url %q err %v; want %s url %q", r.Outcome, r.URL, r.Err, tc.want, tc.wantURL)
			}
			if tc.errHas != "" && (r.Err == nil || !strings.Contains(r.Err.Error(), tc.errHas)) {
				t.Errorf("err %v, want %q", r.Err, tc.errHas)
			}
			if (tc.note == "") != (r.PromptNote == "") || !strings.Contains(r.PromptNote, tc.note) {
				t.Errorf("prompt note %q, want %q", r.PromptNote, tc.note)
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
			if n := countOf(m, "agent.start"); n > 1 {
				t.Errorf("agent.start called %d times", n)
			}
			if p := fh.params("workspace.create"); p != nil && tc.trusted == nil {
				if p["cwd"] != realApp || p["focus"] != false || p["label"] != "sessionhub: new in app" {
					t.Errorf("workspace.create %v", p)
				}
			}
			if p := fh.params("agent.start"); p != nil {
				args, _ := p["args"].([]any)
				if len(args) != 1 || args[0] != "--remote-control" || p["name"] != "sessionhub-new-abcdef01" {
					t.Errorf("agent.start %v", p)
				}
			}
			if p := fh.params("agent.prompt"); p != nil && (p["target"] != "w9:p1" || p["text"] != tc.req.Prompt) {
				t.Errorf("agent.prompt %v", p)
			}
		})
	}
}
