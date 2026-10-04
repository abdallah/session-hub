package resume

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
)

const uuid = "0a1b2c3d-1111-4222-8333-444455556666"

// fakeHerdr answers one request per connection with scripted results.
type fakeHerdr struct {
	path string
	mu   sync.Mutex
	reqs []herdrtest.Request
	fn   func(method string, params map[string]any) (result any, errCode string)
}

func newFakeHerdr(t *testing.T, fn func(method string, params map[string]any) (any, string)) *fakeHerdr {
	t.Helper()
	dir, err := os.MkdirTemp("", "fh")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeHerdr{path: filepath.Join(dir, "herdr.sock"), fn: fn}
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); os.RemoveAll(dir) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req herdrtest.Request
				json.Unmarshal(line, &req)
				var params map[string]any
				json.Unmarshal(req.Params, &params)
				f.mu.Lock()
				f.reqs = append(f.reqs, req)
				f.mu.Unlock()
				res, code := f.fn(req.Method, params)
				resp := map[string]any{"id": req.ID}
				if code != "" {
					resp["error"] = map[string]string{"code": code, "message": code}
				} else {
					resp["result"] = res
				}
				b, _ := json.Marshal(resp)
				c.Write(append(b, '\n'))
			}()
		}
	}()
	return f
}

func (f *fakeHerdr) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var m []string
	for _, r := range f.reqs {
		m = append(m, r.Method)
	}
	return m
}

func (f *fakeHerdr) params(method string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if r.Method == method {
			var p map[string]any
			json.Unmarshal(r.Params, &p)
			return p
		}
	}
	return nil
}

// paneResult is a pane_info result. agent is herdr's detected agent ("" at a
// shell prompt); sessionUUID fills agent_session.
func paneResult(id, sessionUUID, agent string) map[string]any {
	p := map[string]any{"pane_id": id, "workspace_id": "w1", "agent_status": "unknown"}
	if agent != "" {
		p["agent"], p["agent_status"] = agent, "idle"
	}
	if sessionUUID != "" {
		p["agent_session"] = map[string]any{"agent": "claude", "kind": "id", "source": "herdr:claude", "value": sessionUUID}
	}
	return map[string]any{"type": "pane_info", "pane": p}
}

// happyHerdr models a herdr whose pane `existing` runs Claude with session
// `runsUUID` (or no pane when existing is empty).
func happyHerdr(t *testing.T, existing, runsUUID string) *fakeHerdr {
	return paneHerdr(t, existing, runsUUID, "claude")
}

// paneHerdr is happyHerdr with the pane's detected agent chosen: "" models a
// restored pane at a shell prompt that kept its agent_session.
func paneHerdr(t *testing.T, existing, runsUUID, agent string) *fakeHerdr {
	return newFakeHerdr(t, func(m string, p map[string]any) (any, string) {
		switch m {
		case "pane.get":
			if p["pane_id"] == existing {
				return paneResult(existing, runsUUID, agent), ""
			}
			return nil, "pane_not_found"
		case "pane.focus":
			return map[string]any{"type": "ok"}, ""
		case "session.snapshot":
			return map[string]any{"type": "session_snapshot", "snapshot": map[string]any{"focused_pane_id": "w1:p9"}}, ""
		case "pane.split":
			return paneResult("w1:p7", "", ""), ""
		case "agent.start":
			return map[string]any{"type": "agent_started"}, ""
		}
		return nil, "no_fixture"
	})
}

// hubServer serves the session, the machines, and the session list.
func hubServer(t *testing.T, s api.Session, others ...api.Session) *httptest.Server {
	t.Helper()
	all := append([]api.Session{s}, others...)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		prefix := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		var hits []api.Session
		for _, c := range all {
			if strings.HasPrefix(c.ID, prefix) {
				hits = append(hits, c)
			}
		}
		switch len(hits) {
		case 0:
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(api.Error{Error: "not found"})
		case 1:
			json.NewEncoder(w).Encode(api.SessionDetail{Session: hits[0]})
		default:
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(api.Error{Error: "ambiguous prefix: " + hits[0].ID + ", " + hits[1].ID})
		}
	})
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		var out []api.Session
		for _, c := range all {
			if m := r.URL.Query().Get("machine"); m == "" || m == c.Machine {
				out = append(out, c)
			}
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]api.Machine{
			{Name: "bluebox", SSHHost: "bluebox.example.com", HerdrHost: "bluebox.example.com"},
			{Name: "tower", SSHHost: "tower.example.com", HerdrHost: "tower.example.com"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newEnv(t *testing.T, srv *httptest.Server, machine, socket string, saved []SavedMachine) (*env, *bytes.Buffer) {
	t.Helper()
	cfg := client.Config{ServerURL: srv.URL, Token: "hub_m_x", Machine: machine}
	c, err := client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	return &env{
		cfg: cfg, api: c, socket: socket,
		saved: func(context.Context) []SavedMachine { return saved },
		in:    strings.NewReader(""), out: out,
	}, out
}

func sess(machine, pane string) api.Session {
	s := api.Session{
		ID: uuid, Agent: "claude", Machine: machine, CWD: "/home/user/my project",
		Status: api.StatusEnded, StartedAt: time.Now(), LastSeenAt: time.Now(),
		ResumeCommand: "ssh -t bluebox.example.com '~/.local/bin/sessionhub resume " + uuid + "'",
	}
	if pane != "" {
		s.HerdrSession, s.HerdrPane, s.HerdrWorkspace = "default", pane, "w1"
	}
	return s
}

func TestResumeDecisionTable(t *testing.T) {
	deadSock := filepath.Join(t.TempDir(), "none.sock")
	cases := []struct {
		name        string
		sess        api.Session
		herdr       func(t *testing.T) *fakeHerdr // nil: herdr not running
		saved       []SavedMachine
		wantActed   bool
		wantMethods []string
		wantOut     []string
		notOut      []string
	}{
		{
			name:        "local live pane runs the session: focus it",
			sess:        sess("bluebox", "w1:p1"),
			herdr:       func(t *testing.T) *fakeHerdr { return happyHerdr(t, "w1:p1", uuid) },
			wantActed:   true,
			wantMethods: []string{"pane.get", "pane.focus"},
			wantOut:     []string{`Focused pane "w1:p1"`},
		},
		{
			name:        "local pane kept the session at a shell prompt: start in that pane and focus it",
			sess:        sess("bluebox", "w1:p1"),
			herdr:       func(t *testing.T) *fakeHerdr { return paneHerdr(t, "w1:p1", uuid, "") },
			wantActed:   true,
			wantMethods: []string{"pane.get", "agent.start", "pane.focus"},
			wantOut:     []string{"Started claude --resume " + uuid + " in its pane w1:p1"},
			notOut:      []string{"Focused", "new pane"},
		},
		{
			name:        "local pane runs another agent with the old agent_session: split, do not focus it",
			sess:        sess("bluebox", "w1:p1"),
			herdr:       func(t *testing.T) *fakeHerdr { return paneHerdr(t, "w1:p1", uuid, "codex") },
			wantActed:   true,
			wantMethods: []string{"pane.get", "session.snapshot", "pane.split", "agent.start"},
			wantOut:     []string{"new pane w1:p7"},
			notOut:      []string{"Focused"},
		},
		{
			name:        "local pane gone: split and start",
			sess:        sess("bluebox", "w1:p1"),
			herdr:       func(t *testing.T) *fakeHerdr { return happyHerdr(t, "", "") },
			wantActed:   true,
			wantMethods: []string{"pane.get", "session.snapshot", "pane.split", "agent.start"},
			wantOut:     []string{"new pane w1:p7"},
		},
		{
			name:        "local pane reused by another session: split, do not focus",
			sess:        sess("bluebox", "w1:p1"),
			herdr:       func(t *testing.T) *fakeHerdr { return happyHerdr(t, "w1:p1", "99999999-0000-4000-8000-000000000000") },
			wantActed:   true,
			wantMethods: []string{"pane.get", "session.snapshot", "pane.split", "agent.start"},
			notOut:      []string{"Focused"},
		},
		{
			name:      "local, herdr not running: print the local command",
			sess:      sess("bluebox", "w1:p1"),
			wantOut:   []string{"herdr is not running", "cd '/home/user/my project' && claude --resume " + uuid},
			notOut:    []string{"ssh"},
			wantActed: false,
		},
		{
			name:    "local, no herdr info: print the local command without touching herdr",
			sess:    sess("bluebox", ""),
			herdr:   func(t *testing.T) *fakeHerdr { return happyHerdr(t, "w1:p1", uuid) },
			wantOut: []string{"no herdr pane recorded", "claude --resume " + uuid},
		},
		{
			name:    "remote, no saved machine: ssh and remote attach only",
			sess:    sess("tower", "w1:p1"),
			wantOut: []string{"ssh -t tower.example.com '~/.local/bin/sessionhub resume " + uuid + "'", "herdr --remote tower.example.com"},
			notOut:  []string{"--machine"},
		},
		{
			name:  "remote, saved machine matches: also print the --machine form",
			sess:  sess("tower", "w1:p1"),
			saved: []SavedMachine{{ID: "m1", Label: "my tower", SSHTarget: "me@tower.example.com:2222"}},
			wantOut: []string{
				"ssh -t tower.example.com '~/.local/bin/sessionhub resume " + uuid + "'",
				"herdr --remote tower.example.com",
				"herdr --machine 'my tower' pane focus w1:p1",
			},
		},
		{
			name:    "remote, saved machine for another host: no --machine form",
			sess:    sess("tower", "w1:p1"),
			saved:   []SavedMachine{{Label: "bluebox", SSHTarget: "bluebox.example.com"}},
			wantOut: []string{"herdr --remote tower.example.com"},
			notOut:  []string{"--machine"},
		},
		{
			// The cwd has a space, so both quoting layers use single quotes;
			// TestHostileRemoteNoHerdrInfo proves what each shell parses.
			name:    "remote, no herdr info: ssh + login shell + cd + claude --resume",
			sess:    sess("tower", ""),
			wantOut: []string{"ssh -t tower.example.com 'exec $SHELL -lc '\\''cd '\\''\\'\\'''\\''/home/user/my project'", "claude --resume " + uuid},
			notOut:  []string{"herdr --remote"},
		},
		{
			name:    "remote, no herdr info, plain cwd: the double-quoted form",
			sess:    func() api.Session { s := sess("tower", ""); s.CWD = "/home/user/proj"; return s }(),
			wantOut: []string{`ssh -t tower.example.com "exec \$SHELL -lc 'cd /home/user/proj && claude --resume ` + uuid + `'"` + "\n"},
			notOut:  []string{"herdr --remote"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sock := deadSock
			var fh *fakeHerdr
			if tc.herdr != nil {
				fh = tc.herdr(t)
				sock = fh.path
			}
			srv := hubServer(t, tc.sess)
			e, out := newEnv(t, srv, "bluebox", sock, tc.saved)
			acted, err := e.resume(context.Background(), uuid[:8])
			if err != nil {
				t.Fatalf("resume: %v\n%s", err, out)
			}
			if acted != tc.wantActed {
				t.Errorf("acted = %v, want %v", acted, tc.wantActed)
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
				if want := strings.Join(tc.wantMethods, ","); got != want {
					t.Errorf("herdr methods = %s, want %s", got, want)
				}
			}
		})
	}
}

func TestSplitAndAgentStartParams(t *testing.T) {
	fh := happyHerdr(t, "", "")
	srv := hubServer(t, sess("bluebox", "w1:p1"))
	e, _ := newEnv(t, srv, "bluebox", fh.path, nil)
	e.callerPane = "w1:p3" // HERDR_PANE_ID wins over the snapshot's focused pane
	if _, err := e.resume(context.Background(), uuid[:6]); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fh.methods(), ","); got != "pane.get,pane.split,agent.start" {
		t.Errorf("methods = %s (snapshot must not be read when HERDR_PANE_ID is set)", got)
	}
	sp := fh.params("pane.split")
	if sp["cwd"] != "/home/user/my project" || sp["target_pane_id"] != "w1:p3" || sp["direction"] != "right" {
		t.Errorf("pane.split params = %v", sp)
	}
	as := fh.params("agent.start")
	if as["name"] != "sessionhub-0a1b2c3d" || as["kind"] != "claude" || as["pane_id"] != "w1:p7" {
		t.Errorf("agent.start params = %v", as)
	}
	args, _ := as["args"].([]any)
	if len(args) != 2 || args[0] != "--resume" || args[1] != uuid {
		t.Errorf("agent.start args = %v", args)
	}
}

// A pane at a shell prompt that kept the session: agent.start targets that
// pane with the resume args, then pane.focus moves to it. No split.
func TestStartInSessionPaneParams(t *testing.T) {
	fh := paneHerdr(t, "w1:p1", uuid, "")
	srv := hubServer(t, sess("bluebox", "w1:p1"))
	e, out := newEnv(t, srv, "bluebox", fh.path, nil)
	e.callerPane = "w1:p3"
	acted, err := e.resume(context.Background(), uuid[:8])
	if err != nil || !acted {
		t.Fatalf("acted=%v err=%v\n%s", acted, err, out)
	}
	as := fh.params("agent.start")
	args, _ := as["args"].([]any)
	if as["pane_id"] != "w1:p1" || as["kind"] != "claude" || len(args) != 2 || args[0] != "--resume" || args[1] != uuid {
		t.Errorf("agent.start params = %v", as)
	}
	if f := fh.params("pane.focus"); f["pane_id"] != "w1:p1" {
		t.Errorf("pane.focus params = %v", f)
	}
	if p := fh.params("pane.split"); p != nil {
		t.Errorf("split ran: %v", p)
	}
}

// agent.start failing in the session's own pane reports the shell command
// and never focuses the pane.
func TestStartInSessionPaneFailure(t *testing.T) {
	fh := newFakeHerdr(t, func(m string, p map[string]any) (any, string) {
		switch m {
		case "pane.get":
			return paneResult("w1:p1", uuid, ""), ""
		case "agent.start":
			return nil, "pane_not_available"
		}
		return map[string]any{"type": "ok"}, ""
	})
	srv := hubServer(t, sess("bluebox", "w1:p1"))
	e, _ := newEnv(t, srv, "bluebox", fh.path, nil)
	acted, err := e.resume(context.Background(), uuid[:8])
	if err == nil || acted || !strings.Contains(err.Error(), "claude --resume "+uuid) {
		t.Fatalf("acted=%v err=%v, want an error naming the shell command", acted, err)
	}
	if got := strings.Join(fh.methods(), ","); got != "pane.get,agent.start" {
		t.Errorf("methods = %s", got)
	}
}

// A session the server reports live or blocked, whose pane can't be focused,
// is not started again without --force: it may still run elsewhere. Every
// status × herdr state × force combination is checked.
func TestLiveSessionNeedsForce(t *testing.T) {
	type scene struct {
		name   string
		herdr  func(t *testing.T) *fakeHerdr
		starts string // herdr methods when the start goes ahead
	}
	scenes := []scene{
		{"pane gone", func(t *testing.T) *fakeHerdr { return happyHerdr(t, "", "") }, "pane.get,session.snapshot,pane.split,agent.start"},
		{"pane at a prompt", func(t *testing.T) *fakeHerdr { return paneHerdr(t, "w1:p1", uuid, "") }, "pane.get,agent.start,pane.focus"},
	}
	fails, n := 0, 0
	for _, status := range []string{api.StatusLive, api.StatusBlocked, api.StatusStale, api.StatusEnded} {
		for _, sc := range scenes {
			for _, force := range []bool{false, true} {
				n++
				s := sess("bluebox", "w1:p1")
				s.Status = status
				fh := sc.herdr(t)
				srv := hubServer(t, s)
				e, out := newEnv(t, srv, "bluebox", fh.path, nil)
				e.force = force
				acted, err := e.resume(context.Background(), uuid[:8])
				blocked := !force && (status == api.StatusLive || status == api.StatusBlocked)
				methods := strings.Join(fh.methods(), ",")
				name := fmt.Sprintf("%s/%s/force=%v", status, sc.name, force)
				switch {
				case blocked && (!errors.Is(err, errNeedsForce) || acted || methods != "pane.get" ||
					!strings.Contains(out.String(), "sessionhub resume --force "+uuid) || !strings.Contains(out.String(), "second copy")):
					t.Errorf("%s: want a refusal before any change: acted=%v err=%v methods=%s\n%s", name, acted, err, methods, out)
					fails++
				case !blocked && (err != nil || !acted || methods != sc.starts || strings.Contains(out.String(), "Warning")):
					t.Errorf("%s: want a start: acted=%v err=%v methods=%s (want %s)\n%s", name, acted, err, methods, sc.starts, out)
					fails++
				}
			}
		}
	}
	t.Logf("force cases: %d, failures: %d", n, fails)

	// A live session in a pane that still runs it is focused without --force.
	s := sess("bluebox", "w1:p1")
	s.Status = api.StatusLive
	fh := happyHerdr(t, "w1:p1", uuid)
	e, out := newEnv(t, hubServer(t, s), "bluebox", fh.path, nil)
	if acted, err := e.resume(context.Background(), uuid[:8]); err != nil || !acted || strings.Contains(out.String(), "Warning") {
		t.Errorf("focusable live session: acted=%v err=%v\n%s", acted, err, out)
	}

	// Printing a command starts nothing, so it only warns.
	e, out = newEnv(t, hubServer(t, s), "bluebox", filepath.Join(t.TempDir(), "none.sock"), nil)
	if acted, err := e.resume(context.Background(), uuid[:8]); err != nil || acted ||
		!strings.Contains(out.String(), "Warning") || !strings.Contains(out.String(), "claude --resume "+uuid) {
		t.Errorf("live session, herdr down: acted=%v err=%v\n%s", acted, err, out)
	}

	// In the picker the refusal stays on screen until Enter.
	fh = happyHerdr(t, "", "")
	e, out = newEnv(t, hubServer(t, s), "bluebox", fh.path, nil)
	e.in = strings.NewReader("1\n\n")
	if err := e.pick(context.Background()); !errors.Is(err, errNeedsForce) || !strings.Contains(out.String(), "Press Enter") {
		t.Errorf("picker: err=%v\n%s", err, out)
	}
	if got := strings.Join(fh.methods(), ","); got != "pane.get" {
		t.Errorf("picker: methods %s, want pane.get only", got)
	}
}

// The picker and the resume messages clean server and herdr strings: no
// escape sequence reaches the terminal, and titles stop at 60 runes.
func TestPickerAndMessagesAreCleaned(t *testing.T) {
	const esc = "\x1b]0;pwned\a\x1b[2J"
	a := sess("bluebox", "w1:p1")
	a.Title, a.Status = "evil"+esc+"\nline", "ended"+esc
	b := sess("bluebox", "w1:p2")
	b.ID = "11111111-aaaa-4bbb-8ccc-dddddddddddd"
	b.Title = strings.Repeat("é", 80)
	srv := hubServer(t, a, b)

	// herdr answers the split with a hostile pane ID.
	fh := newFakeHerdr(t, func(m string, p map[string]any) (any, string) {
		switch m {
		case "pane.get":
			return nil, "pane_not_found"
		case "session.snapshot":
			return map[string]any{"snapshot": map[string]any{"focused_pane_id": "w1:p9"}}, ""
		case "pane.split":
			return paneResult("w1:p7"+esc, "", ""), ""
		case "agent.start":
			return nil, "pane_not_available"
		}
		return nil, "no_fixture"
	})
	e, out := newEnv(t, srv, "bluebox", fh.path, nil)
	e.in = strings.NewReader("1\n")
	err := e.pick(context.Background())
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\a") || !strings.Contains(err.Error(), "pane w1:p7]0;pwned[2J") {
		t.Errorf("agent.start error not cleaned: %q", err)
	}
	if strings.ContainsAny(out.String(), "\x1b\a") || !strings.Contains(out.String(), "evil]0;pwned[2J line") {
		t.Errorf("picker list not cleaned:\n%q", out.String())
	}
	if !strings.Contains(out.String(), strings.Repeat("é", 59)+"…") || strings.Contains(out.String(), strings.Repeat("é", 60)) {
		t.Errorf("long title not cut to 60 runes:\n%s", out)
	}
}

func TestSplitFailureNeverStartsAgent(t *testing.T) {
	fh := newFakeHerdr(t, func(m string, p map[string]any) (any, string) {
		switch m {
		case "pane.get":
			return nil, "pane_not_found"
		case "session.snapshot":
			return map[string]any{"snapshot": map[string]any{"focused_pane_id": "w1:p9"}}, ""
		case "pane.split":
			return map[string]any{"type": "pane_info"}, "" // no pane in the response
		}
		return nil, "no_fixture"
	})
	srv := hubServer(t, sess("bluebox", "w1:p1"))
	e, out := newEnv(t, srv, "bluebox", fh.path, nil)
	_, err := e.resume(context.Background(), uuid[:8])
	if err == nil || !strings.Contains(err.Error(), "claude --resume "+uuid) {
		t.Fatalf("err = %v, want split failure that names the fallback command", err)
	}
	for _, m := range fh.methods() {
		if m == "agent.start" {
			t.Error("agent.start ran after a failed split")
		}
	}
	_ = out
}

// The captured fixtures answer pane.get for a missing pane; anything else is
// "no_fixture", so this walks the missing-pane path against real payloads.
func TestResumeAgainstCapturedFixtures(t *testing.T) {
	h := herdrtest.New(t, "pane-get-missing.ndjson", "session-snapshot.ndjson")
	s := sess("bluebox", "w99:p99")
	srv := hubServer(t, s)
	e, _ := newEnv(t, srv, "bluebox", h.Path, nil)
	_, err := e.resume(context.Background(), uuid[:8])
	if err == nil || !strings.Contains(err.Error(), "split pane") {
		t.Fatalf("err = %v, want a split error (no split fixture with these params)", err)
	}
	var got []string
	for _, r := range h.Requests() {
		got = append(got, r.Method)
	}
	if strings.Join(got, ",") != "pane.get,session.snapshot,pane.split" {
		t.Errorf("requests = %v", got)
	}
	for _, r := range h.Requests() {
		if r.Method == "pane.split" && !strings.Contains(string(r.Params), `"target_pane_id":"w4:p1"`) {
			t.Errorf("split target should be the snapshot's focused pane w4:p1: %s", r.Params)
		}
	}
}

func TestResumeErrors(t *testing.T) {
	a := sess("bluebox", "")
	b := sess("bluebox", "")
	b.ID = "0a1b2c3d-9999-4222-8333-444455556666"
	srv := hubServer(t, a, b)
	e, _ := newEnv(t, srv, "bluebox", "", nil)
	if _, err := e.resume(context.Background(), "0a1b"); err == nil || !strings.Contains(err.Error(), b.ID) {
		t.Errorf("ambiguous prefix: err = %v, want candidate IDs", err)
	}
	if _, err := e.resume(context.Background(), "ffff"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("unknown prefix: err = %v", err)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	e2, _ := newEnv(t, dead, "bluebox", "", nil)
	if _, err := e2.resume(context.Background(), "abcd"); err == nil {
		t.Error("unreachable server: want an error")
	}
}

func TestPickScriptedStdin(t *testing.T) {
	live := sess("bluebox", "w1:p1")
	live.ID = "11111111-aaaa-4bbb-8ccc-dddddddddddd"
	live.Status = api.StatusLive
	live.LastSeenAt = time.Now().Add(-time.Hour)
	ended := sess("bluebox", "w1:p2")
	ended.LastSeenAt = time.Now()
	other := sess("tower", "w1:p1")
	other.ID = "22222222-aaaa-4bbb-8ccc-dddddddddddd"
	srv := hubServer(t, ended, live, other)

	t.Run("picks the live session first and focuses it", func(t *testing.T) {
		fh := happyHerdr(t, "w1:p1", live.ID)
		e, out := newEnv(t, srv, "bluebox", fh.path, nil)
		e.in = strings.NewReader("1\n")
		if err := e.pick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "22222222") {
			t.Errorf("list includes another machine's session:\n%s", out)
		}
		if !strings.Contains(out.String(), `Focused pane "w1:p1"`) {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("picks the ended session and splits", func(t *testing.T) {
		fh := happyHerdr(t, "", "")
		e, out := newEnv(t, srv, "bluebox", fh.path, nil)
		e.in = strings.NewReader("2\n")
		if err := e.pick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(fh.methods(), ","); got != "pane.get,session.snapshot,pane.split,agent.start" {
			t.Errorf("methods = %s\n%s", got, out)
		}
	})
	t.Run("empty line cancels without touching herdr", func(t *testing.T) {
		fh := happyHerdr(t, "", "")
		e, out := newEnv(t, srv, "bluebox", fh.path, nil)
		e.in = strings.NewReader("\n")
		if err := e.pick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(fh.methods()) != 0 || !strings.Contains(out.String(), "Cancelled") {
			t.Errorf("methods = %v\n%s", fh.methods(), out)
		}
	})
	t.Run("bad numbers and closed stdin are errors", func(t *testing.T) {
		for _, in := range []string{"0\n", "3\n", "abc\n", ""} {
			fh := happyHerdr(t, "", "")
			e, _ := newEnv(t, srv, "bluebox", fh.path, nil)
			e.in = strings.NewReader(in)
			if err := e.pick(context.Background()); err == nil {
				t.Errorf("input %q: want an error", in)
			}
			if len(fh.methods()) != 0 {
				t.Errorf("input %q: herdr was called: %v", in, fh.methods())
			}
		}
	})
	t.Run("printed commands wait for Enter", func(t *testing.T) {
		e, out := newEnv(t, srv, "bluebox", filepath.Join(t.TempDir(), "none.sock"), nil)
		e.in = strings.NewReader("1\n\n")
		if err := e.pick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "claude --resume") || !strings.Contains(out.String(), "Press Enter") {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("no machine in config", func(t *testing.T) {
		e, _ := newEnv(t, srv, "", "", nil)
		if err := e.pick(context.Background()); err == nil {
			t.Error("want an error")
		}
	})
}

func TestParseSavedMachines(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "herdr", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if got := parseSavedMachines(read("machine-list.json")); len(got) != 0 {
		t.Errorf("captured empty list = %v", got)
	}
	got := parseSavedMachines(read("machine-list.constructed.json"))
	if len(got) != 1 || got[0].Label != "bluebox" {
		t.Fatalf("constructed list = %+v (disabled profiles must be skipped)", got)
	}
	if l := matchSaved(got, "bluebox.example.com"); l != "bluebox" {
		t.Errorf("match = %q", l)
	}
	if l := matchSaved(got, "tower.example.com"); l != "" {
		t.Errorf("match for other host = %q", l)
	}
	for _, bad := range []string{"", "not json", `{"machines": "x"}`, `42`} {
		if got := parseSavedMachines([]byte(bad)); len(got) != 0 {
			t.Errorf("parse(%q) = %v", bad, got)
		}
	}
	if got := parseSavedMachines([]byte(`{"machines":[{"name":"x","target":"u@h"}]}`)); len(got) != 1 || got[0].Label != "x" || got[0].SSHTarget != "u@h" {
		t.Errorf("wrapped form = %+v", got)
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"tower.example.com":                "tower.example.com",
		"me@tower.example.com":             "tower.example.com",
		"me@tower.example.com:22":          "tower.example.com",
		"ssh://u@bluebox.example.com:2222": "bluebox.example.com",
		"[::1]:22":                         "::1",
		"u@[fe80::1]":                      "fe80::1",
		"":                                 "",
	} {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/home/user/x": "/home/user/x",
		"a b":          "'a b'",
		"it's":         `'it'\''s'`,
		"":             "''",
		"$(rm -rf)":    "'$(rm -rf)'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// A session with an open move is never resumed, --force or not, here or on
// another machine: the move's source or target may start it.
func TestResumeRefusesMovingSession(t *testing.T) {
	for _, state := range []string{api.MoveRequested, api.MovePacking, api.MoveUploaded, api.MoveUnpacking} {
		for _, machine := range []string{"bluebox", "tower"} {
			for _, force := range []bool{false, true} {
				s := sess("bluebox", "w1:p1")
				s.Move = &api.Move{ID: "mv_AAAAAAAAAAAAAAAAAAAAAA", SessionID: uuid, Source: "bluebox", Target: "tower", State: state}
				fh := happyHerdr(t, "", "")
				e, out := newEnv(t, hubServer(t, s), machine, fh.path, nil)
				e.force = force
				acted, err := e.resume(context.Background(), uuid[:8])
				if !errors.Is(err, errMoving) || acted || len(fh.methods()) != 0 ||
					!strings.Contains(out.String(), "sessionhub move --status mv_AAAAAAAAAAAAAAAAAAAAAA") {
					t.Errorf("%s on %s, force=%v: acted=%v err=%v methods=%v\n%s", state, machine, force, acted, err, fh.methods(), out)
				}
			}
		}
	}
	// An ended move does not block a resume.
	s := sess("bluebox", "w1:p1")
	s.Move = &api.Move{ID: "mv_AAAAAAAAAAAAAAAAAAAAAA", SessionID: uuid, Source: "bluebox", Target: "tower", State: api.MoveFailed}
	fh := happyHerdr(t, "", "")
	e, out := newEnv(t, hubServer(t, s), "bluebox", fh.path, nil)
	if acted, err := e.resume(context.Background(), uuid[:8]); err != nil || !acted {
		t.Errorf("after a failed move: acted=%v err=%v\n%s", acted, err, out)
	}
}
