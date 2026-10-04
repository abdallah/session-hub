package plugin

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
	"github.com/abdallah/session-hub/internal/move"
	"github.com/abdallah/session-hub/internal/resume"
	"github.com/abdallah/session-hub/internal/server"
	"github.com/abdallah/session-hub/internal/store"
)

// moveSID is the moved session.
const moveSID = "5e5e5e5e-1111-4222-8333-444455556666"

func gitIsolate(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Hub Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "sessionhub@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "Hub Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "sessionhub@example.test")
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// newGitRepo makes a bare origin under base and a clone of it at
// base/<name> with one pushed commit on main (a.txt).
func newGitRepo(t *testing.T, base, name string) (origin, work string) {
	t.Helper()
	gitIsolate(t)
	origin = filepath.Join(base, name+"-origin.git")
	gitT(t, base, "init", "-q", "--bare", "-b", "main", origin)
	work = filepath.Join(base, name)
	gitT(t, base, "clone", "-q", origin, work)
	writeFile(t, filepath.Join(work, "a.txt"), "one\n", 0o644)
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "-q", "-m", "initial")
	gitT(t, work, "push", "-q", "-u", "origin", "main")
	return origin, work
}

// fakeClaude puts an executable named claude first on PATH. script is the
// body of a shell script.
func fakeClaude(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "claude"), "#!/bin/sh\n"+script+"\n", 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// orderLog records the order of steps across herdr and the sessionhub.
type orderLog struct {
	mu    sync.Mutex
	items []string
}

func (o *orderLog) add(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.items = append(o.items, s)
}

func (o *orderLog) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.items...)
}

// movePane is a herdr with one pane, w1:p1, where Claude runs a session
// until it gets /exit and then sits at a shell until agent.start.
type movePane struct {
	mu      sync.Mutex
	session string
	agent   string // "claude", or "" at a shell
	status  string
	exits   bool // false: /exit is typed but Claude stays
	// exitDropsID: /exit leaves Claude in the pane, but herdr reports no
	// session ID for it, as it does for a moment while Claude starts or ends.
	exitDropsID bool
	busyAfter   int // > 0: the agent turns working after that many pane.get calls
	gets        int
	prompts     []string
	starts      [][]string
}

func newMovePane(t *testing.T, session, status string, order *orderLog) (*movePane, *herdrtest.Server) {
	t.Helper()
	p := &movePane{session: session, agent: "claude", status: status, exits: true}
	srv := herdrtest.New(t, "session-snapshot.ndjson")
	srv.Handle("pane.get", func(params json.RawMessage) (any, string) {
		var q struct {
			PaneID string `json:"pane_id"`
		}
		json.Unmarshal(params, &q)
		if q.PaneID != "w1:p1" {
			return nil, "pane_not_found"
		}
		p.mu.Lock()
		if p.gets++; p.busyAfter > 0 && p.gets > p.busyAfter {
			p.status = "working"
		}
		p.mu.Unlock()
		return map[string]any{"pane": p.info()}, ""
	})
	srv.Handle("session.snapshot", func(json.RawMessage) (any, string) {
		return map[string]any{"snapshot": map[string]any{"panes": []any{p.info()}}}, ""
	})
	srv.Handle("agent.prompt", func(params json.RawMessage) (any, string) {
		var q struct{ Target, Text string }
		json.Unmarshal(params, &q)
		p.mu.Lock()
		defer p.mu.Unlock()
		p.prompts = append(p.prompts, q.Text)
		if q.Text == "/exit" {
			order.add("exit")
			switch {
			case p.exitDropsID:
				p.session = ""
			case p.exits:
				p.agent, p.status = "", ""
			}
		}
		return map[string]any{"type": "agent_prompted"}, ""
	})
	srv.Handle("agent.start", func(params json.RawMessage) (any, string) {
		var q struct {
			PaneID string   `json:"pane_id"`
			Args   []string `json:"args"`
		}
		json.Unmarshal(params, &q)
		p.mu.Lock()
		defer p.mu.Unlock()
		order.add("start")
		p.starts = append(p.starts, q.Args)
		p.agent, p.status = "claude", "idle"
		return map[string]any{"type": "agent_started"}, ""
	})
	return p, srv
}

func (p *movePane) info() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": p.status}
	if p.session != "" {
		m["agent_session"] = map[string]any{"agent": "claude", "kind": "id", "source": "herdr:claude", "value": p.session}
	}
	if p.agent != "" {
		m["agent"] = p.agent
	}
	return m
}

func (p *movePane) snapshot() (prompts []string, starts [][]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts...), append([][]string(nil), p.starts...)
}

// hubMachine is one machine of a moveHub: its token, ID, and move key.
type hubMachine struct {
	tok string
	id  int64
	key *ecdh.PrivateKey
}

// moveHub is the real sessionhub server over a temp database with machines tower
// and bluebox, both with registered move keys and a recent poll. Every bundle
// upload adds "upload" to the order log. An intercept sees each request
// first and may answer it.
type moveHub struct {
	st             *store.Store
	srv            *httptest.Server
	moveDir        string
	tower, bluebox hubMachine
	mu             sync.Mutex
	intercept      func(w http.ResponseWriter, r *http.Request) bool
	skew           atomic.Int64 // added to the store's clock, in nanoseconds
}

// setIntercept makes f see every request before the sessionhub; f answers a
// request by returning true.
func (h *moveHub) setIntercept(f func(w http.ResponseWriter, r *http.Request) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.intercept = f
}

// isFrom reports whether r carries machine m's token.
func isFrom(r *http.Request, m hubMachine) bool {
	return r.Header.Get("Authorization") == "Bearer "+m.tok
}

// badGateway answers like a tunnel whose server is down.
func badGateway(w http.ResponseWriter) {
	http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
}

func newMoveHub(t *testing.T, order *orderLog) *moveHub {
	t.Helper()
	dir := t.TempDir()
	h := &moveHub{moveDir: filepath.Join(dir, "moves")}
	st, err := store.Open(filepath.Join(dir, "sessionhub.db"), store.Options{Now: func() time.Time {
		return time.Now().Add(time.Duration(h.skew.Load()))
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	h.st = st
	for _, m := range []struct {
		name string
		dst  *hubMachine
	}{{"tower", &h.tower}, {"bluebox", &h.bluebox}} {
		tok, _, err := st.AddMachine(ctx, m.name, "", "")
		if err != nil {
			t.Fatal(err)
		}
		mm, err := st.MachineByToken(ctx, tok)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetMoveKey(ctx, mm.ID, move.PublicKeyString(key)); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordPoll(ctx, mm.ID); err != nil {
			t.Fatal(err)
		}
		*m.dst = hubMachine{tok: tok, id: mm.ID, key: key}
	}
	s := server.New(st, "https://sessionhub.example.test", nil)
	s.SetMoveDir(h.moveDir)
	handler := s.Handler()
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/bundle") {
			order.add("upload")
		}
		h.mu.Lock()
		f := h.intercept
		h.mu.Unlock()
		if f != nil && f(w, r) {
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *moveHub) client(t *testing.T, m hubMachine) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{ServerURL: h.srv.URL, Token: m.tok})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// claim polls once as m and fails unless a claim came.
func (h *moveHub) claim(t *testing.T, m hubMachine) api.ControlClaim {
	t.Helper()
	c, err := h.client(t, m).PollControl(context.Background(), time.Second)
	if err != nil || c == nil {
		t.Fatalf("poll: %v %v", c, err)
	}
	return *c
}

func (h *moveHub) move(t *testing.T, id string) api.Move {
	t.Helper()
	mv, err := h.st.GetMove(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return mv
}

// moveEnv is a session on tower in a temp Git repository, with a temp
// ~/.claude, a scripted herdr pane, a fake claude, the real sessionhub, and tower's
// mover.
type moveEnv struct {
	sessionhub *moveHub
	order      *orderLog
	pane       *movePane
	herdr      *herdrtest.Server
	base       string // holds the repositories
	origin     string
	work       string // the session's directory
	claude     string // tower's ~/.claude
	state      string // tower's sessionhub state dir
	mover      *mover
	logs       *strings.Builder
}

func newMoveEnv(t *testing.T, paneStatus string) *moveEnv {
	t.Helper()
	// The movers get every path by injection; these guard against a code
	// path that reads a default under the real home.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(home, ".config", "sessionhub", "config.toml"))
	t.Setenv("SESSIONHUB_STATE_DIR", filepath.Join(home, ".local", "state", "sessionhub"))
	e := &moveEnv{order: &orderLog{}, base: t.TempDir(), claude: t.TempDir(), state: t.TempDir(), logs: &strings.Builder{}}
	e.origin, e.work = newGitRepo(t, e.base, "app")
	writeFile(t, filepath.Join(e.claude, "projects", "-src-app", moveSID+".jsonl"), `{"type":"user","message":"hi"}`+"\n", 0o600)
	writeFile(t, filepath.Join(e.claude, "projects", "-src-app", moveSID, "subagents", "a.jsonl"), "{}\n", 0o600)
	writeFile(t, filepath.Join(e.claude, "file-history", moveSID, "h@v1"), "snap\n", 0o600)
	fakeClaude(t, `[ "$1" = --version ] && echo "2.1.285 (Claude Code)"`)
	e.sessionhub = newMoveHub(t, e.order)
	e.pane, e.herdr = newMovePane(t, moveSID, paneStatus, e.order)
	if _, err := e.sessionhub.st.UpsertSession(context.Background(), e.sessionhub.tower.id, api.SessionUpsert{ID: moveSID, Agent: "claude",
		Source: api.SourcePlugin, CWD: e.work, GitRepo: e.origin, GitBranch: "main", HerdrSession: "default",
		HerdrWorkspace: "w1", HerdrPane: "w1:p1", AgentState: "idle"}); err != nil {
		t.Fatal(err)
	}
	e.mover = newMover(e.herdr.Path, e.claude, e.state, []string{e.base}, e.sessionhub.tower.key, log.New(e.logs, "", 0))
	fastMover(e.mover)
	return e
}

// fastMover shortens a mover's waits for tests.
func fastMover(m *mover) {
	m.exitWait, m.pollEvery = 2*time.Second, 5*time.Millisecond
	m.start = resume.ControlOptions{PollEvery: 5 * time.Millisecond, PollFor: time.Second}
	m.resultPause = time.Millisecond
	m.cloudWait = 10 * time.Second
}

// onGitHub makes the session's origin look like a GitHub remote, while
// pushes still go to the local bare repository.
func (e *moveEnv) onGitHub(t *testing.T) {
	t.Helper()
	const gh = "https://github.com/o/app.git"
	gitT(t, e.work, "remote", "set-url", "origin", gh)
	gitT(t, e.work, "config", "url."+e.origin+".pushInsteadOf", gh)
	if _, err := e.sessionhub.st.UpsertSession(context.Background(), e.sessionhub.tower.id, api.SessionUpsert{ID: moveSID,
		Source: api.SourcePlugin, GitRepo: gh}); err != nil {
		t.Fatal(err)
	}
}

// startMove opens a move of the session to target and returns tower's claim.
func (e *moveEnv) startMove(t *testing.T, target string) api.ControlClaim {
	t.Helper()
	if _, err := e.sessionhub.st.CreateMove(context.Background(), moveSID, target, "machine:tower"); err != nil {
		t.Fatal(err)
	}
	return e.sessionhub.claim(t, e.sessionhub.tower)
}

// targetHerdr is bluebox's herdr: no panes until agent.start, which makes the
// new workspace's root pane w5:p1 run Claude, until /exit.
type targetHerdr struct {
	mu      sync.Mutex
	started bool
	cwd     string   // workspace.create's cwd
	args    []string // agent.start's args
	prompts []string // agent.prompt's texts
	// exitFails: agent.prompt refuses /exit, so Claude stays.
	exitFails bool
	// noDetect: pane.get never shows Claude, so StartResumed gives up.
	noDetect bool
	// closed: pane.close closed w5:p1, so pane.get finds no pane.
	closed bool
	// closeFails: pane.close fails and the pane stays.
	closeFails bool
	closes     int // pane.close calls
	// others are more panes in the snapshot, such as another session's.
	others []map[string]any
}

// state is whether Claude runs in w5:p1, and the prompts typed there.
func (th *targetHerdr) state() (bool, []string) {
	th.mu.Lock()
	defer th.mu.Unlock()
	return th.started, append([]string(nil), th.prompts...)
}

// paneClosed reports whether w5:p1 was closed, and how many closes came.
func (th *targetHerdr) paneClosed() (bool, int) {
	th.mu.Lock()
	defer th.mu.Unlock()
	return th.closed, th.closes
}

func newTargetHerdr(t *testing.T) (*targetHerdr, *herdrtest.Server) {
	t.Helper()
	th := &targetHerdr{}
	srv := herdrtest.New(t, "session-snapshot.ndjson")
	srv.Handle("session.snapshot", func(json.RawMessage) (any, string) {
		th.mu.Lock()
		defer th.mu.Unlock()
		panes := []any{}
		for _, p := range th.others {
			panes = append(panes, p)
		}
		return map[string]any{"snapshot": map[string]any{"panes": panes}}, ""
	})
	srv.Handle("pane.close", func(params json.RawMessage) (any, string) {
		var q struct {
			PaneID string `json:"pane_id"`
		}
		json.Unmarshal(params, &q)
		th.mu.Lock()
		defer th.mu.Unlock()
		th.closes++
		switch {
		case q.PaneID != "w5:p1" || th.closed:
			return nil, "pane_not_found"
		case th.closeFails:
			return nil, "internal_error"
		}
		th.closed, th.started = true, false
		return map[string]any{"type": "ok"}, ""
	})
	srv.Handle("workspace.create", func(params json.RawMessage) (any, string) {
		var q struct {
			CWD string `json:"cwd"`
		}
		json.Unmarshal(params, &q)
		th.mu.Lock()
		th.cwd = q.CWD
		th.mu.Unlock()
		return map[string]any{"type": "workspace_created", "workspace": map[string]any{"workspace_id": "w5"},
			"root_pane": map[string]any{"pane_id": "w5:p1", "workspace_id": "w5"}}, ""
	})
	srv.Handle("agent.start", func(params json.RawMessage) (any, string) {
		var q struct {
			Args []string `json:"args"`
		}
		json.Unmarshal(params, &q)
		th.mu.Lock()
		defer th.mu.Unlock()
		th.started, th.args = true, q.Args
		return map[string]any{"type": "agent_started"}, ""
	})
	srv.Handle("agent.prompt", func(params json.RawMessage) (any, string) {
		var q struct{ Text string }
		json.Unmarshal(params, &q)
		th.mu.Lock()
		defer th.mu.Unlock()
		th.prompts = append(th.prompts, q.Text)
		if q.Text == "/exit" && th.exitFails {
			return nil, "internal_error"
		}
		if q.Text == "/exit" {
			th.started = false
		}
		return map[string]any{"type": "agent_prompted"}, ""
	})
	srv.Handle("pane.get", func(params json.RawMessage) (any, string) {
		var q struct {
			PaneID string `json:"pane_id"`
		}
		json.Unmarshal(params, &q)
		th.mu.Lock()
		defer th.mu.Unlock()
		if q.PaneID != "w5:p1" || th.closed {
			return nil, "pane_not_found"
		}
		p := map[string]any{"pane_id": "w5:p1", "workspace_id": "w5", "agent_status": "idle"}
		if th.started && !th.noDetect {
			p["agent"] = "claude"
		}
		return map[string]any{"pane": p}, ""
	})
	return th, srv
}

// targetEnv is bluebox: its own ~/.claude and state dir, a search root that
// holds a clone of the source's origin named App, its herdr, and its mover.
type targetEnv struct {
	claude, state, root, clone string
	herdr                      *targetHerdr
	mover                      *mover
}

func newTargetEnv(t *testing.T, e *moveEnv) *targetEnv {
	t.Helper()
	tt := &targetEnv{claude: t.TempDir(), state: t.TempDir(), root: t.TempDir()}
	tt.clone = filepath.Join(tt.root, "App")
	gitT(t, tt.root, "clone", "-q", e.origin, tt.clone)
	var srv *herdrtest.Server
	tt.herdr, srv = newTargetHerdr(t)
	tt.mover = newMover(srv.Path, tt.claude, tt.state, []string{tt.root}, e.sessionhub.bluebox.key, log.New(e.logs, "bluebox ", 0))
	fastMover(tt.mover)
	return tt
}

// packed moves the session from tower toward bluebox up to the upload, and
// returns bluebox's move-in claim.
func (e *moveEnv) packed(t *testing.T) api.ControlClaim {
	t.Helper()
	claim := e.startMove(t, "bluebox")
	e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
	if mv := e.sessionhub.move(t, claim.Move.ID); mv.State != api.MoveUploaded {
		t.Fatalf("pack: %+v (logs:\n%s)", mv, e.logs)
	}
	return e.sessionhub.claim(t, e.sessionhub.bluebox)
}
