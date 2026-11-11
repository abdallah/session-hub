package plugin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/move"
	"github.com/abdallah/session-hub/internal/resume"
	"github.com/abdallah/session-hub/internal/rex/rextest"
	"github.com/abdallah/session-hub/internal/store"
)

const (
	rexSID    = "0a1b2c3d-1111-4222-8333-444455556666"
	rexLink   = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"
	rexBanner = "/remote-control is active · Continue here, on your phone, or at " + rexLink
)

// noSocket is a herdr socket path where nothing exists.
func noSocket(t *testing.T) string { return filepath.Join(t.TempDir(), "none.sock") }

// rexUpsert is session id in a Rex block, as its hooks report it.
func rexUpsert(id, cwd, state string) api.SessionUpsert {
	return api.SessionUpsert{ID: id, Agent: "claude", Source: api.SourceHooks, CWD: cwd,
		RexSession: "session:1", RexBlock: "block:1", AgentState: state}
}

// A watcher without herdr reports its backends and keeps Rex sessions
// current: seen while Claude runs, ended once it is gone.
func TestWatcherRexBeat(t *testing.T) {
	state := rextest.Fake(t)
	h := newLiveHub(t)
	ctx := context.Background()
	c, _ := h.clientFunc()()
	for _, u := range []api.SessionUpsert{
		rexUpsert(rexSID, "/home/user/proj", "idle"),
		{ID: "herdr-1", Agent: "claude", Source: api.SourcePlugin, HerdrSession: "default", HerdrPane: "w1:p1"},
	} {
		if err := c.UpsertSession(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	var logs bytes.Buffer
	w := newWatcher(client.NewQueue(t.TempDir()), nil, "default", h.clientFunc(), nil, log.New(&logs, "", 0))
	w.machine = "tower"
	backends := []string{}
	w.backends = func() []string { return backends }
	now := time.Now()

	// Neither backend: the server refuses a start here.
	w.heartbeat(ctx, now)
	if err := h.st.RecordPoll(ctx, h.id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.CreateStart(ctx, "tower", api.StartIn{Dir: "~/x"}, "web:phone"); !errors.Is(err, store.ErrNotControllable) {
		t.Errorf("start with no backend reported: %v, want ErrNotControllable", err)
	}
	if s, _ := h.st.GetSession(ctx, rexSID); s.Status != api.StatusLive {
		t.Fatalf("Rex session with Claude running: %s (logs: %s)", s.Status, &logs)
	}
	if got := w.snapCands(); len(got) != 1 || got[0] != (snapCand{rexSID, "/home/user/proj"}) {
		t.Errorf("snapshot candidates %v, want the idle Rex session", got)
	}

	// Rex appears. A block that shows no Claude, or can't be read, ends
	// nothing: Claude may run under a multiplexer there.
	backends = []string{api.BackendRex}
	for i, file := range []string{"noclaude", "broken"} {
		rextest.Set(t, state, file, "")
		w.heartbeat(ctx, now.Add(time.Duration(i+1)*heartbeatInterval))
		if s, _ := h.st.GetSession(ctx, rexSID); s.Status == api.StatusEnded {
			t.Fatalf("%s: the Rex session ended", file)
		}
		os.Remove(filepath.Join(state, file))
	}
	if !strings.Contains(logs.String(), "rex: session "+rexSID+": Error: connection refused") {
		t.Errorf("logs %q, want the unreadable block", &logs)
	}
	logs.Reset()
	if len(w.snapCands()) != 0 {
		t.Errorf("snapshot candidates %v after the block stopped showing Claude", w.snapCands())
	}
	if _, err := h.st.CreateStart(ctx, "tower", api.StartIn{Dir: "~/x"}, "web:phone"); err != nil {
		t.Errorf("start once Rex is reported: %v", err)
	}
	// The block goes: the session ends.
	rextest.Set(t, state, "gone", "")
	w.heartbeat(ctx, now.Add(3*heartbeatInterval))
	d, err := c.GetSession(ctx, rexSID)
	if err != nil || d.Status != api.StatusEnded {
		t.Fatalf("Rex session after Claude left: %s %v (logs: %s)", d.Status, err, &logs)
	}
	if len(d.Events) == 0 || d.Events[0].Kind != api.KindEnded || string(d.Events[0].Payload) != rexEnded {
		t.Errorf("newest event %+v, want ended with %s", d.Events, rexEnded)
	}
	// The herdr session is not this check's: no snapshot, so nothing ends it.
	if s, _ := h.st.GetSession(ctx, "herdr-1"); s.Status == api.StatusEnded {
		t.Error("the herdr session ended")
	}
	if logs.Len() != 0 {
		t.Errorf("logs: %s", &logs)
	}
}

// `sessionhub watch` runs without a herdr socket, waits while another
// watcher holds the lock, and exits when a socket appears.
func TestRunWatcherWithoutHerdr(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	old := lockWaitEvery
	lockWaitEvery = 10 * time.Millisecond
	t.Cleanup(func() { lockWaitEvery = old })
	sock := noSocket(t)
	hub := newFakeHub(t)
	var mu sync.Mutex
	var logs bytes.Buffer
	logger := log.New(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return logs.Write(p) }), "", 0)
	logged := func(want string) bool { mu.Lock(); defer mu.Unlock(); return strings.Contains(logs.String(), want) }

	// The plugin's watcher exits at once without a socket.
	if err := runWatcher(context.Background(), dir, sock, false, hub.clientFor(hub.URL), nil, logger); err != nil || !logged("exiting") {
		t.Fatalf("plugin watch without a socket: %v", err)
	}

	lock, held, err := tryLock(filepath.Join(dir, watcherLockFile))
	if err != nil || held {
		t.Fatalf("lock: %v %v", held, err)
	}
	done := make(chan error, 1)
	go func() { done <- runWatcher(context.Background(), dir, sock, true, hub.clientFor(hub.URL), nil, logger) }()
	waitUntil(t, "the wait for the lock", func() bool { return logged("waiting for it to exit") })
	if n := hub.count(http.MethodPut, "/v1/machines/self/backends"); n != 0 {
		t.Fatalf("%d heartbeats while another watcher holds the lock", n)
	}
	lock.Close()
	waitUntil(t, "a heartbeat", func() bool { return hub.count(http.MethodPut, "/v1/machines/self/backends") > 0 })
	if n := hub.count(http.MethodPut, "/v1/machines/self/herdr-sessions"); n != 0 {
		t.Errorf("%d herdr-sessions PUTs without herdr", n)
	}
	for _, r := range hub.requests() {
		if r.Path == "/v1/machines/self/backends" && string(r.Body) != `{"backends":[]}` {
			t.Errorf("backends body %s, want none", r.Body)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("watcher exited without a socket: %v", err)
	default:
	}
	// herdr starts: the watcher exits, and its next start uses the socket.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	select {
	case err := <-done:
		if err != nil || !logged("herdr socket appeared") {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watcher did not exit after a socket appeared")
	}
}

func TestIsWatcher(t *testing.T) {
	for cmdline, want := range map[string]bool{
		"/home/u/.local/bin/sessionhub plugin watch": true,
		"/home/u/.local/bin/sessionhub watch":        true,
		"/home/u/.local/bin/sessionhub server":       false,
		"watch -n1 date":                             false,
		"/usr/bin/other watch":                       false,
	} {
		if got := isWatcher(cmdline); got != want {
			t.Errorf("isWatcher(%q) = %v, want %v", cmdline, got, want)
		}
	}
}

func TestInstallWatcher(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("SESSIONHUB_STATE_DIR", t.TempDir())
	var calls []string
	old := systemctl
	systemctl = func(_ context.Context, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() { systemctl = old })
	unit := filepath.Join(cfg, "systemd", "user", watcherUnit)

	// A herdr socket that is not the default reaches the unit.
	t.Setenv("HERDR_SOCKET_PATH", "/run/user/1000/herdr/work.sock")
	if err := RunInstallWatcher(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(unit); !strings.Contains(string(b), "Environment=\"HERDR_SOCKET_PATH=/run/user/1000/herdr/work.sock\"\nRestart=always") {
		t.Fatalf("unit %q, want the socket path", b)
	}
	calls = nil
	t.Setenv("HERDR_SOCKET_PATH", "")
	if err := RunInstallWatcher(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(unit); err != nil || !strings.Contains(string(b), "ExecStart=%h/.local/bin/sessionhub watch\nRestart=always") ||
		strings.Contains(string(b), "Environment") {
		t.Fatalf("unit %q, %v", b, err)
	}
	if want := []string{"daemon-reload", "enable " + watcherUnit, "restart " + watcherUnit}; !slices.Equal(calls, want) {
		t.Errorf("systemctl calls %q, want %q", calls, want)
	}
	calls = nil
	if err := RunUninstallWatcher(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Errorf("unit still there: %v", err)
	}
	if want := []string{"disable --now " + watcherUnit, "daemon-reload"}; !slices.Equal(calls, want) {
		t.Errorf("systemctl calls %q, want %q", calls, want)
	}
	// A second uninstall has nothing to do.
	calls = nil
	if err := RunUninstallWatcher(context.Background(), nil); err != nil || len(calls) != 0 {
		t.Errorf("second uninstall: %v, calls %q", err, calls)
	}
}

// The controller turns on Remote Control for a session in a Rex block, and
// resumes one whose block is gone in a new Rex session, which the row then
// names.
func TestControlRexSession(t *testing.T) {
	run := func(t *testing.T, h *liveHub, req api.ControlRequest, ctl *controller) api.ControlRequest {
		t.Helper()
		ctl.wait = time.Second
		ctl.once(context.Background())
		d, err := h.st.GetSession(context.Background(), rexSID)
		if err != nil || d.RemoteControl == nil || d.RemoteControl.ID != req.ID {
			t.Fatalf("session %+v %v", d.RemoteControl, err)
		}
		return *d.RemoteControl
	}
	t.Run("Claude runs: the link", func(t *testing.T) {
		state := rextest.Fake(t)
		rextest.Set(t, state, "frame_after", strconv.Quote(rexBanner))
		h := newLiveHub(t)
		h.reportsRex(t)
		req := h.open(t, rexUpsert(rexSID, t.TempDir(), "idle"), false)
		ctl := newController(h.clientFunc(), controlRunner(noSocket(t), 5*time.Millisecond, 300*time.Millisecond), log.New(io.Discard, "", 0))
		if got := run(t, h, req, ctl); got.State != api.ControlDone || got.URL != rexLink || got.Detail != "" {
			t.Fatalf("result %+v", got)
		}
		if rextest.Called(t, state, "new ") {
			t.Error("started a second copy")
		}
	})
	t.Run("ended, block gone: resumed in a new Rex session", func(t *testing.T) {
		state := rextest.Fake(t)
		rextest.Set(t, state, "gone", "")
		h := newLiveHub(t)
		h.reportsRex(t)
		req := h.open(t, rexUpsert(rexSID, t.TempDir(), "idle"), true)
		var logs bytes.Buffer
		ctl := newController(h.clientFunc(), controlRunner(noSocket(t), 5*time.Millisecond, 50*time.Millisecond), log.New(&logs, "", 0))
		if got := run(t, h, req, ctl); got.State != api.ControlDone || got.Detail != "resumed in a new Rex session, link not seen" {
			t.Fatalf("result %+v (logs: %s)", got, &logs)
		}
		if s, _ := h.st.GetSession(context.Background(), rexSID); s.RexSession != "session:new" || s.RexBlock != "block:new" {
			t.Errorf("row names %q %q, want the new block", s.RexSession, s.RexBlock)
		}
		if got := ctl.resumed[rexSID]; got.rexBlock != "block:new" {
			t.Errorf("remembered %+v, want the new block", got)
		}
	})
}

// reportsRex records that tower's watcher has Rex.
func (h *liveHub) reportsRex(t *testing.T) {
	t.Helper()
	if err := h.st.SetBackends(context.Background(), h.id, []string{api.BackendRex}); err != nil {
		t.Fatal(err)
	}
}

// A resume the row does not name yet is not repeated: the next request goes
// to the block the controller remembers.
func TestControlRemembersRexBlock(t *testing.T) {
	var seen []api.Session
	ctl := newController(nil, func(_ context.Context, s api.Session, _ string) resume.ControlResult {
		seen = append(seen, s)
		if len(seen) == 1 {
			return resume.ControlResult{Outcome: resume.OutcomeResumed, RexSession: "session:new", RexBlock: "block:new"}
		}
		return resume.ControlResult{Outcome: resume.OutcomeLinked, RexSession: s.RexSession, RexBlock: s.RexBlock}
	}, log.New(io.Discard, "", 0))
	s := api.Session{ID: rexSID, RexSession: "session:1", RexBlock: "block:1"}
	ctl.control(context.Background(), s)
	ctl.control(context.Background(), s)
	if len(seen) != 2 || seen[0].RexBlock != "block:1" || seen[1].RexSession != "session:new" || seen[1].RexBlock != "block:new" {
		t.Errorf("runs saw %+v", seen)
	}
}

func TestRexResultWording(t *testing.T) {
	in := resume.ControlResult{Outcome: resume.OutcomeStarted, URL: rexLink, RexSession: "session:new", RexBlock: "block:new"}
	if got := startResultFor("tower", resume.NewSessionResult{ControlResult: in}); got.Detail != "started in a new Rex session" || got.URL != rexLink {
		t.Errorf("start: %+v", got)
	}
	in.Outcome = resume.OutcomeResumed
	if got := resultFor(in); got.Detail != "resumed in a new Rex session" {
		t.Errorf("resume: %+v", got)
	}
	if got := place(in); got != "Rex block block:new" {
		t.Errorf("place = %q", got)
	}
	if got := place(resume.ControlResult{Pane: "w1:p2"}); got != "pane w1:p2" {
		t.Errorf("place = %q", got)
	}
}

// toRex puts the move env's session in a Rex block on tower.
func (e *moveEnv) toRex(t *testing.T, state string) {
	t.Helper()
	if _, err := e.sessionhub.st.UpsertSession(context.Background(), e.sessionhub.tower.id, rexUpsert(moveSID, e.work, state)); err != nil {
		t.Fatal(err)
	}
}

// A move works between the two backends in every direction.
func TestMoveBetweenBackends(t *testing.T) {
	for _, c := range []struct {
		name           string
		srcRex, dstRex bool
	}{
		{"Rex to Rex", true, true},
		{"Rex to herdr", true, false},
		{"herdr to Rex", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := rextest.Fake(t)
			e := newMoveEnv(t, "idle")
			ctx := context.Background()
			writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
			if c.srcRex {
				e.toRex(t, "idle")
			}
			tt := newTargetEnv(t, e)
			if c.dstRex {
				tt.mover.socket = noSocket(t)
			}
			in := e.packed(t)
			if c.srcRex {
				if w := rextest.Read(t, state, "writes"); !strings.Contains(w, `{"data":"/exit"}`) {
					t.Fatalf("the source typed %q, want /exit", w)
				}
				if prompts, _ := e.pane.snapshot(); len(prompts) != 0 {
					t.Errorf("the source's herdr pane got %v", prompts)
				}
			}
			tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
			if mv := e.sessionhub.move(t, in.Move.ID); mv.State != api.MoveDone {
				t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
			}
			s, _ := e.sessionhub.st.GetSession(ctx, moveSID)
			if s.Machine != "bluebox" {
				t.Errorf("owner %s", s.Machine)
			}
			started, _ := tt.herdr.state()
			if c.dstRex {
				if want := "--json -c " + tt.clone + " -- claude --resume " + moveSID + "\n"; !rextest.Called(t, state, want) {
					t.Errorf("calls %q, want %q", rextest.Read(t, state, "calls"), want)
				}
				if s.RexSession != "session:new" || s.RexBlock != "block:new" || s.HerdrPane != "" || started {
					t.Errorf("row %q %q pane %q, herdr started %v; want the new Rex block only", s.RexSession, s.RexBlock, s.HerdrPane, started)
				}
			} else if !started || !slices.Equal(tt.herdr.args, []string{"--resume", moveSID}) || s.RexBlock != "" || rextest.Called(t, state, "new ") {
				t.Errorf("herdr started %v with %q, row block %q, calls %q", started, tt.herdr.args, s.RexBlock, rextest.Read(t, state, "calls"))
			}
			fin := e.sessionhub.claim(t, e.sessionhub.tower)
			e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
			if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 0 {
				t.Errorf("the source can still resume it: %v", ts)
			}
		})
	}
}

// A Rex source moves only when Claude runs in the block and the server
// reports it idle or done; nothing is typed otherwise.
func TestMoveOutRexChecks(t *testing.T) {
	for _, c := range []struct {
		name, agent, file, want string
	}{
		{"working", "working", "", "check pane: the agent is working"},
		{"block gone", "idle", "gone", "check pane: " + detailRexGone},
		{"Claude left the block", "idle", "noclaude", "check pane: " + detailNoAgent},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := rextest.Fake(t)
			e := newMoveEnv(t, "idle")
			e.toRex(t, "idle")
			claim := e.startMove(t, "bluebox")
			// The state changes after the server accepted the move.
			e.toRex(t, c.agent)
			if c.file != "" {
				rextest.Set(t, state, c.file, "")
			}
			e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
			if mv := e.sessionhub.move(t, claim.Move.ID); mv.State != api.MoveFailed || mv.Detail != c.want {
				t.Fatalf("move %+v, want failed with %q", mv, c.want)
			}
			if w := rextest.Read(t, state, "writes"); w != "" {
				t.Errorf("typed %q", w)
			}
		})
	}
}

// A Rex source whose upload fails restarts the session in a new Rex
// session, which the row then names.
func TestMoveOutRexRestartsAfterFailure(t *testing.T) {
	state := rextest.Fake(t)
	e := newMoveEnv(t, "idle")
	e.toRex(t, "idle")
	e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/bundle") {
			badGateway(w)
			return true
		}
		return false
	})
	claim := e.startMove(t, "bluebox")
	e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
	if mv := e.sessionhub.move(t, claim.Move.ID); mv.State != api.MoveFailed || !strings.HasSuffix(mv.Detail, detailResumes) {
		t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if want := " -- claude --resume " + moveSID + "\n"; !rextest.Called(t, state, want) {
		t.Errorf("calls %q, want %q", rextest.Read(t, state, "calls"), want)
	}
	if s, _ := e.sessionhub.st.GetSession(context.Background(), moveSID); s.RexBlock != "block:new" || s.Machine != "tower" {
		t.Errorf("row block %q on %s, want block:new on tower", s.RexBlock, s.Machine)
	}
	if _, starts := e.pane.snapshot(); len(starts) != 0 {
		t.Errorf("restarted in herdr: %v", starts)
	}
}

// A Rex target whose Claude never shows takes the start back: the move
// fails, and the clone and transcript are as before.
func TestMoveInRexStartFails(t *testing.T) {
	state := rextest.Fake(t)
	e := newMoveEnv(t, "idle")
	writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
	tt := newTargetEnv(t, e)
	tt.mover.socket = noSocket(t)
	tt.mover.start.PollFor = 50 * time.Millisecond
	in := e.packed(t)
	rextest.Set(t, state, "noclaude_after_start", "")
	tt.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.bluebox), in)
	mv := e.sessionhub.move(t, in.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "start: claude didn't start in the new Rex session") {
		t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if ts, _ := move.FindTranscripts(tt.claude, moveSID); len(ts) != 0 {
		t.Errorf("transcript left on bluebox: %v", ts)
	}
	if _, err := os.Stat(filepath.Join(tt.clone, "notes.md")); !os.IsNotExist(err) {
		t.Errorf("notes.md left on bluebox: %v", err)
	}
	// Rex never showed Claude, but the command may run: its session dies.
	if !rextest.Called(t, state, "session kill -- session:new") {
		t.Errorf("the Rex session was not killed: %q", rextest.Read(t, state, "calls"))
	}
}

func TestStopRex(t *testing.T) {
	newM := func() *mover {
		m := newMover("", "", "", nil, nil, log.New(io.Discard, "", 0))
		m.exitWait, m.pollEvery = 30*time.Millisecond, 5*time.Millisecond
		return m
	}
	started := resume.ControlResult{Outcome: resume.OutcomeResumed, RexSession: "session:new", RexBlock: "block:new"}
	running := started
	running.Outcome = resume.OutcomeRunning
	for _, c := range []struct {
		name          string
		r             resume.ControlResult
		file          string // a fake state file to set first
		killed, wantE bool
	}{
		{"its own session, /exit works: killed all the same", started, "", true, false},
		{"its own session, /exit ignored: killed", started, "exit_ignored", true, false},
		{"its own session, block already gone", started, "gone", true, false},
		{"a block that already ran Claude, /exit works: not killed", running, "", false, false},
		{"a block that already ran Claude, /exit ignored: an error, not killed", running, "exit_ignored", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := rextest.Fake(t)
			if c.file != "" {
				rextest.Set(t, state, c.file, "")
			}
			err := newM().stopRex(context.Background(), "mv", c.r)
			if (err != nil) != c.wantE || rextest.Called(t, state, "session kill -- session:new") != c.killed {
				t.Errorf("err %v, calls %q; want error %v, killed %v", err, rextest.Read(t, state, "calls"), c.wantE, c.killed)
			}
		})
	}
}

// On a machine with Rex, a live Rex session in the clone refuses a checkout
// that would switch its branch.
func TestMoveInRefusesCloneInUseByRexSession(t *testing.T) {
	rextest.Fake(t)
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	tt := newTargetEnv(t, e)
	tt.mover.socket = noSocket(t)
	if err := os.MkdirAll(filepath.Join(tt.clone, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, tt.clone, "checkout", "-q", "-b", "other")
	const other = "0a0a0a0a-1111-4222-8333-444455556666"
	if _, err := e.sessionhub.st.UpsertSession(ctx, e.sessionhub.bluebox.id, rexUpsert(other, filepath.Join(tt.clone, "sub"), "working")); err != nil {
		t.Fatal(err)
	}
	in := e.packed(t)
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	mv := e.sessionhub.move(t, in.Move.ID)
	if want := "check clone: " + tt.clone + " is in use by session " + other + " in a Rex block"; mv.State != api.MoveFailed || mv.Detail != want {
		t.Fatalf("move %+v, want failed with %q (logs:\n%s)", mv, want, e.logs)
	}
}
