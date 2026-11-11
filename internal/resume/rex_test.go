package resume

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/rex/rextest"
)

// TestMain keeps the tests off the machine's own Rex: a test that needs rex
// puts the fake on PATH. The live test wants the machine's own.
func TestMain(m *testing.M) {
	if os.Getenv("SESSIONHUB_LIVE_REX") != "1" {
		rextest.HideReal()
	}
	os.Exit(m.Run())
}

// rexSess is a session of machine in a Rex block.
func rexSess(machine string) api.Session {
	s := sess(machine, "")
	s.RexSession, s.RexBlock = "session:1", "block:1"
	return s
}

var fastRex = ControlOptions{PollEvery: 5 * time.Millisecond, PollFor: 300 * time.Millisecond}

const rexBanner = "/remote-control is active · Continue here, on your phone, or at " + rcURL

// frame is text as the fake's frame file holds it: a JSON string.
func frame(text string) string { return strconv.Quote(text) }

func TestBackendFor(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "herdr.sock")
	none := api.Session{}
	if got := Backends(sock); len(got) != 0 || backendFor(none, sock) != "" {
		t.Errorf("no herdr, no rex: %v, %q", got, backendFor(none, sock))
	}
	rextest.Fake(t)
	if got := Backends(sock); !slices.Equal(got, []string{api.BackendRex}) || backendFor(none, sock) != api.BackendRex {
		t.Errorf("rex only: %v, %q", got, backendFor(none, sock))
	}
	// A socket file nothing listens on is not herdr.
	stale := filepath.Join(t.TempDir(), "stale.sock")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Backends(stale); !slices.Equal(got, []string{api.BackendRex}) {
		t.Errorf("stale socket file: %v, want rex only", got)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if got := Backends(sock); !slices.Equal(got, []string{api.BackendHerdr, api.BackendRex}) || backendFor(none, sock) != api.BackendHerdr {
		t.Errorf("both: %v, %q; want herdr first", got, backendFor(none, sock))
	}
	// A session's own row wins over what the machine has.
	if got := backendFor(rexSess("tower"), sock); got != api.BackendRex {
		t.Errorf("a Rex session on a machine with both: %q", got)
	}
	if got := backendFor(sess("tower", "w1:p1"), filepath.Join(t.TempDir(), "none")); got != api.BackendHerdr {
		t.Errorf("a herdr session: %q", got)
	}
}

func TestControlRex(t *testing.T) {
	dialog := "Remote Control\nDisconnect this session\nShow QR code\n" + rcURL + "\nEsc to continue"
	cases := []struct {
		name    string
		setup   func(t *testing.T, state string, s *api.Session)
		resume  bool
		want    Outcome
		wantURL string
		writes  []string // substrings of the writes file, in order; nil wants none
		called  string   // a command line that must have run
		block   string   // RexBlock of the result
	}{
		{name: "Claude runs: types the command and reads the new link",
			setup: func(t *testing.T, state string, _ *api.Session) {
				rextest.Set(t, state, "frame_after", frame(rexBanner))
			},
			want: OutcomeLinked, wantURL: rcURL, writes: []string{rcCommand, `"\r"`}, block: "block:1"},
		{name: "a link already on screen is not new",
			setup: func(t *testing.T, state string, _ *api.Session) { rextest.Set(t, state, "frame", frame(rexBanner)) },
			want:  OutcomeNoLink, writes: []string{rcCommand}, block: "block:1"},
		{name: "already on: the dialog's link, then Esc",
			setup: func(t *testing.T, state string, _ *api.Session) { rextest.Set(t, state, "frame_after", frame(dialog)) },
			want:  OutcomeAlreadyOn, wantURL: rcURL, writes: []string{rcCommand, `"\r"`, `\u001b`}, block: "block:1"},
		{name: "a dialog is open: nothing typed",
			setup: func(t *testing.T, state string, _ *api.Session) { rextest.Set(t, state, "frame", frame(dialog)) },
			want:  OutcomeDialogOpen, block: "block:1"},
		{name: "the server reports blocked: nothing typed",
			setup: func(_ *testing.T, _ string, s *api.Session) { s.AgentState = "blocked" },
			want:  OutcomeBlocked, block: "block:1"},
		{name: "Claude left the block: not running",
			setup: func(t *testing.T, state string, _ *api.Session) { rextest.Set(t, state, "noclaude", "") },
			want:  OutcomeNotRunning},
		{name: "block still there, no end reported: nothing started",
			setup: func(t *testing.T, state string, s *api.Session) {
				rextest.Set(t, state, "noclaude", "")
				s.Status = api.StatusStale
			},
			resume: true, want: OutcomeLooksLive},
		{name: "block still there, the hooks reported the end: resumes",
			setup:  func(t *testing.T, state string, _ *api.Session) { rextest.Set(t, state, "noclaude", "") },
			resume: true, want: OutcomeResumed, called: "-- claude --resume " + uuid + " --remote-control", block: "block:new"},
		{name: "block can't be read: an error, nothing started",
			setup:  func(t *testing.T, state string, _ *api.Session) { rextest.Set(t, state, "broken", "") },
			resume: true, want: OutcomeError},
		{name: "block gone, the server says live: nothing started",
			setup: func(t *testing.T, state string, s *api.Session) {
				rextest.Set(t, state, "gone", "")
				s.Status = api.StatusLive
			},
			resume: true, want: OutcomeLooksLive},
		{name: "block gone: resumes in a new Rex session",
			setup:  func(t *testing.T, state string, _ *api.Session) { rextest.Set(t, state, "gone", "") },
			resume: true, want: OutcomeResumed, called: "-- claude --resume " + uuid + " --remote-control", block: "block:new"},
		{name: "rex new fails: an error",
			setup: func(t *testing.T, state string, _ *api.Session) {
				rextest.Set(t, state, "gone", "")
				rextest.Set(t, state, "nostart", "")
			},
			resume: true, want: OutcomeError},
		{name: "Claude never shows in the new block: an error",
			setup: func(t *testing.T, state string, _ *api.Session) {
				rextest.Set(t, state, "gone", "")
				rextest.Set(t, state, "noclaude_after_start", "")
			},
			resume: true, want: OutcomeError, block: "block:new"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := rextest.Fake(t)
			s := rexSess("tower")
			s.CWD = t.TempDir()
			tc.setup(t, state, &s)
			o := fastRex
			o.Resume = tc.resume
			r := Control(context.Background(), s, o)
			if r.Outcome != tc.want || r.URL != tc.wantURL || r.RexBlock != tc.block || r.Pane != "" {
				t.Fatalf("got %s url %q block %q pane %q err %v; want %s url %q block %q", r.Outcome, r.URL, r.RexBlock, r.Pane, r.Err, tc.want, tc.wantURL, tc.block)
			}
			writes := rextest.Read(t, state, "writes")
			if tc.writes == nil && writes != "" {
				t.Errorf("typed %q, want nothing", writes)
			}
			at := 0
			for _, w := range tc.writes {
				i := strings.Index(writes[at:], w)
				if i < 0 {
					t.Fatalf("writes %q: want %q after offset %d", writes, w, at)
				}
				at += i + len(w)
			}
			if tc.called != "" && !rextest.Called(t, state, tc.called) {
				t.Errorf("calls %q, want %q", rextest.Read(t, state, "calls"), tc.called)
			}
			if tc.want != OutcomeResumed && tc.called == "" && tc.block != "block:new" && rextest.Called(t, state, "new ") && tc.name != "rex new fails: an error" {
				t.Errorf("started a Rex session: %q", rextest.Read(t, state, "calls"))
			}
		})
	}
}

func TestControlRexMissingDir(t *testing.T) {
	state := rextest.Fake(t)
	rextest.Set(t, state, "gone", "")
	s := rexSess("tower")
	s.CWD = filepath.Join(t.TempDir(), "gone")
	o := fastRex
	o.Resume = true
	if r := Control(context.Background(), s, o); r.Outcome != OutcomeNoDir || rextest.Called(t, state, "new ") {
		t.Errorf("got %s, calls %q; want no_dir and no rex new", r.Outcome, rextest.Read(t, state, "calls"))
	}
}

func TestStartResumedRex(t *testing.T) {
	state := rextest.Fake(t)
	s := rexSess("tower")
	s.CWD = t.TempDir()
	// Claude runs in the recorded block: no second copy.
	if r := StartResumed(context.Background(), "", s, fastRex); r.Outcome != OutcomeRunning || r.RexBlock != "block:1" || rextest.Called(t, state, "new ") {
		t.Fatalf("running: %+v", r)
	}
	// A session with no backend on a machine with Rex only starts in Rex,
	// without Remote Control.
	s.RexSession, s.RexBlock = "", ""
	s.Title = "Fix login"
	r := StartResumed(context.Background(), filepath.Join(t.TempDir(), "none.sock"), s, fastRex)
	if r.Outcome != OutcomeResumed || r.RexSession != "session:new" || r.RexBlock != "block:new" {
		t.Fatalf("start: %+v", r)
	}
	if want := "new sessionhub: Fix login --json -c " + s.CWD + " -- claude --resume " + uuid + "\n"; !rextest.Called(t, state, want) {
		t.Errorf("calls %q, want %q", rextest.Read(t, state, "calls"), want)
	}
}

func TestStartNewRex(t *testing.T) {
	home := t.TempDir()
	app := filepath.Join(home, "app")
	if err := os.Mkdir(app, 0o700); err != nil {
		t.Fatal(err)
	}
	realApp, _ := filepath.EvalSymlinks(app)
	config := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(config, []byte(`{"projects": {"`+realApp+`": {"hasTrustDialogAccepted": true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "none.sock")
	req := api.StartRequest{ID: "st_AbC-_dEf0123456789xyzw", Dir: "~/app", Prompt: "fix the build\nthen deploy"}

	// showBanner prints the banner a moment after Claude starts.
	showBanner := func(t *testing.T, state string) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
				if rextest.Called(t, state, " format ") {
					time.Sleep(30 * time.Millisecond) // let the baseline read finish
					rextest.Set(t, state, "frame", frame(rexBanner))
					return
				}
			}
		}()
		t.Cleanup(func() { <-done })
	}

	t.Run("starts with Remote Control and submits the first prompt", func(t *testing.T) {
		state := rextest.Fake(t)
		showBanner(t, state)
		r := StartNew(context.Background(), sock, home, config, req, fastRex)
		if r.Outcome != OutcomeStarted || r.URL != rcURL || r.PromptNote != "" || r.RexBlock != "block:new" || r.Dir != realApp {
			t.Fatalf("got %+v err %v", r, r.Err)
		}
		if want := "new sessionhub: new in app --json -c " + realApp + " -- claude --remote-control\n"; !rextest.Called(t, state, want) {
			t.Errorf("calls %q, want %q", rextest.Read(t, state, "calls"), want)
		}
		if w := rextest.Read(t, state, "writes"); !strings.Contains(w, `fix the build\nthen deploy`) || !strings.HasSuffix(w, `{"data":"\r"}`+"\n") {
			t.Errorf("writes %q, want the prompt as a paste, then Enter", w)
		}
	})
	t.Run("no link: started, the first prompt not typed", func(t *testing.T) {
		state := rextest.Fake(t)
		r := StartNew(context.Background(), sock, home, config, req, fastRex)
		if r.Outcome != OutcomeStarted || r.URL != "" || r.PromptNote != notePromptBusy {
			t.Fatalf("got %+v err %v", r, r.Err)
		}
		if w := rextest.Read(t, state, "writes"); w != "" {
			t.Errorf("typed %q, want nothing", w)
		}
	})
	t.Run("untrusted directory: nothing created", func(t *testing.T) {
		state := rextest.Fake(t)
		r := StartNew(context.Background(), sock, home, config, api.StartRequest{Dir: "~"}, fastRex)
		if r.Outcome != OutcomeUntrusted || rextest.Called(t, state, "new ") {
			t.Fatalf("got %s, calls %q", r.Outcome, rextest.Read(t, state, "calls"))
		}
	})
}

func TestResumeLocalRex(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "none.sock")
	t.Run("Claude runs: focuses the block", func(t *testing.T) {
		state := rextest.Fake(t)
		e, out := newEnv(t, hubServer(t, rexSess("tower")), "tower", sock, nil)
		acted, err := e.resume(context.Background(), uuid)
		if err != nil || !acted || !strings.Contains(out.String(), `Focused Rex block "block:1"`) {
			t.Fatalf("acted %v err %v out %q", acted, err, out)
		}
		if !rextest.Called(t, state, "block focus -s session:1 -- block:1") || rextest.Called(t, state, "run ") {
			t.Errorf("calls %q", rextest.Read(t, state, "calls"))
		}
	})
	t.Run("Claude left: starts it in a new tab in the session's directory", func(t *testing.T) {
		state := rextest.Fake(t)
		rextest.Set(t, state, "noclaude", "")
		e, out := newEnv(t, hubServer(t, rexSess("tower")), "tower", sock, nil)
		e.remoteControl = true
		acted, err := e.resume(context.Background(), uuid)
		if err != nil || !acted || !strings.Contains(out.String(), "in a new Rex tab, block block:run") {
			t.Fatalf("acted %v err %v out %q", acted, err, out)
		}
		if want := "run --json -c /home/user/my project -- claude --resume " + uuid + " --remote-control\n"; !rextest.Called(t, state, want) {
			t.Errorf("calls %q, want %q", rextest.Read(t, state, "calls"), want)
		}
	})
	t.Run("no backend recorded on a Rex-only machine: starts it in Rex", func(t *testing.T) {
		state := rextest.Fake(t)
		e, _ := newEnv(t, hubServer(t, sess("tower", "")), "tower", sock, nil)
		if acted, err := e.resume(context.Background(), uuid); err != nil || !acted || !rextest.Called(t, state, "run ") {
			t.Fatalf("acted %v err %v calls %q", acted, err, rextest.Read(t, state, "calls"))
		}
	})
	t.Run("live but not in its block: needs --force", func(t *testing.T) {
		state := rextest.Fake(t)
		rextest.Set(t, state, "gone", "")
		s := rexSess("tower")
		s.Status = api.StatusLive
		e, _ := newEnv(t, hubServer(t, s), "tower", sock, nil)
		if _, err := e.resume(context.Background(), uuid); !errors.Is(err, errNeedsForce) || rextest.Called(t, state, "run ") {
			t.Fatalf("err %v calls %q", err, rextest.Read(t, state, "calls"))
		}
	})
	t.Run("no Rex session to add a tab to: a new Rex session", func(t *testing.T) {
		state := rextest.Fake(t)
		rextest.Set(t, state, "noclaude", "")
		rextest.Set(t, state, "norun", "")
		e, out := newEnv(t, hubServer(t, rexSess("tower")), "tower", sock, nil)
		acted, err := e.resume(context.Background(), uuid)
		if err != nil || !acted || !strings.Contains(out.String(), "in the new Rex session") {
			t.Fatalf("acted %v err %v out %q", acted, err, out)
		}
		if want := "--json -c /home/user/my project -- claude --resume " + uuid + "\n"; !rextest.Called(t, state, "new sessionhub: ") || !rextest.Called(t, state, want) {
			t.Errorf("calls %q", rextest.Read(t, state, "calls"))
		}
	})
	t.Run("rex run fails another way: the command to run by hand", func(t *testing.T) {
		state := rextest.Fake(t)
		rextest.Set(t, state, "noclaude", "")
		rextest.Set(t, state, "nostart", "")
		e, _ := newEnv(t, hubServer(t, rexSess("tower")), "tower", sock, nil)
		if _, err := e.resume(context.Background(), uuid); err == nil || !strings.Contains(err.Error(), "claude --resume "+uuid) {
			t.Fatalf("err %v, want the shell command", err)
		}
		// rex run failed another way than "no sessions": it may have made
		// the tab, so no second start.
		if rextest.Called(t, state, "new ") {
			t.Errorf("fell back to rex new: %q", rextest.Read(t, state, "calls"))
		}
	})
	t.Run("on another machine: the ssh recipe", func(t *testing.T) {
		state := rextest.Fake(t)
		e, out := newEnv(t, hubServer(t, rexSess("bluebox")), "tower", sock, nil)
		acted, err := e.resume(context.Background(), uuid)
		want := "ssh -t bluebox.example.com '~/.local/bin/sessionhub resume " + uuid + "'"
		if err != nil || acted || !strings.Contains(out.String(), want) || strings.Contains(out.String(), "herdr") {
			t.Fatalf("acted %v err %v out %q, want %q and no herdr line", acted, err, out, want)
		}
		if rextest.Read(t, state, "calls") != "" {
			t.Errorf("ran rex for a remote session: %q", rextest.Read(t, state, "calls"))
		}
	})
}

func TestRemoteControlLocalRex(t *testing.T) {
	state := rextest.Fake(t)
	rextest.Set(t, state, "frame_after", frame(rexBanner))
	e, out := newEnv(t, hubServer(t, rexSess("tower")), "tower", filepath.Join(t.TempDir(), "none.sock"), nil)
	e.pollEvery, e.pollFor = fastRex.PollEvery, fastRex.PollFor
	if err := e.remoteControlRun(context.Background(), uuid); err != nil || !strings.Contains(out.String(), rcURL) {
		t.Fatalf("err %v out %q", err, out)
	}
	// Without the banner the message names the block.
	state = rextest.Fake(t)
	out.Reset()
	if err := e.remoteControlRun(context.Background(), uuid); err != nil || !strings.Contains(out.String(), "to Rex block block:1, but no Remote Control URL") {
		t.Fatalf("err %v out %q", err, out)
	}
	_ = state
}

// In a narrow block Claude puts the link on the line below the banner.
func TestNewBannerURLOnNextLine(t *testing.T) {
	const head = "  /remote-control is active · Continue here, on your phone, or at"
	other := strings.Replace(rcURL, "session_01", "session_99", 1)
	for name, c := range map[string]struct{ before, after, want string }{
		"link below the banner":           {"", head + "\n  " + rcURL + "\n❯ ", rcURL},
		"link on the banner line wins":    {"", head + " " + rcURL + "\n  " + other, rcURL},
		"no new banner":                   {head + "\n  " + rcURL, head + "\n  " + rcURL, ""},
		"banner is the last line":         {"", head, ""},
		"nothing below it that is a link": {"", head + "\n❯ ", ""},
	} {
		if got := newBannerURL(c.before, c.after); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}
