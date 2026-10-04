package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

const sessionID = "6fb035d6-af8d-4901-95ca-62ffa95b54c3"

type rec struct {
	Method, Path, Auth string
	Body               []byte
}

type fakeServer struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []rec
	status func(r rec) int // nil: 200
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc := rec{r.Method, r.URL.Path, r.Header.Get("Authorization"), b}
		f.mu.Lock()
		f.reqs = append(f.reqs, rc)
		st := f.status
		f.mu.Unlock()
		code := 200
		if st != nil {
			code = st(rc)
		}
		w.WriteHeader(code)
		if code >= 400 {
			io.WriteString(w, `{"error":"nope"}`)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) requests() []rec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rec(nil), f.reqs...)
}

type fixture struct {
	h     *handler
	state string
	env   map[string]string
	errs  *bytes.Buffer
	out   *bytes.Buffer // the hook's stdout
}

func newFixture(t *testing.T, serverURL string) *fixture {
	t.Helper()
	state := t.TempDir()
	fx := &fixture{state: state, env: map[string]string{}, errs: &bytes.Buffer{}, out: &bytes.Buffer{}}
	fx.h = &handler{
		getenv:   func(k string) string { return fx.env[k] },
		now:      func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 123456789, time.UTC) },
		stateDir: state,
		queue:    client.NewQueue(state),
		newClient: func() (*client.Client, error) {
			return client.New(client.Config{ServerURL: serverURL, Token: "hub_m_test"})
		},
		git:            func(context.Context, string) (string, string) { return "git@example.com:o/r.git", "main" },
		claudePID:      func() int { return 4242 },
		watcherRunning: func() bool { return false },
		flushCmd:       func() *exec.Cmd { return nil },
		digestCmd:      func(string) *exec.Cmd { return nil },
		deadline:       5 * time.Second,
		stderr:         fx.errs,
		stdout:         fx.out,
		refreshCmd:     func() *exec.Cmd { return nil },
		remoteOff: func() bool {
			return client.RemotePermissionsOff(client.Config{}, func(k string) string { return fx.env[k] })
		},
		permWait:  5 * time.Second,
		pollWait:  time.Second,
		pollPause: 10 * time.Millisecond,
	}
	return fx
}

func (fx *fixture) call(t *testing.T, event, stdin string) error {
	t.Helper()
	fx.h.stdin = strings.NewReader(stdin)
	return fx.h.run(context.Background(), []string{event})
}

func fixtureFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claude", "hooks", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func queued(t *testing.T, state string) []client.Item {
	t.Helper()
	var items []client.Item
	if err := client.NewQueue(state).Drain(func(in []client.Item) []client.Item {
		items = in
		return in
	}); err != nil {
		t.Fatal(err)
	}
	return items
}

type reqView struct {
	Path string
	Kind string
	Src  string
	Data map[string]any
}

func view(t *testing.T, rs []rec) []reqView {
	t.Helper()
	var out []reqView
	for _, r := range rs {
		v := reqView{Path: r.Method + " " + r.Path}
		var m map[string]any
		if err := json.Unmarshal(r.Body, &m); err != nil {
			t.Fatalf("body of %s is not JSON: %s", r.Path, r.Body)
		}
		v.Data = m
		if k, ok := m["kind"].(string); ok {
			v.Kind = k
		}
		if s, ok := m["source"].(string); ok {
			v.Src = s
		}
		out = append(out, v)
	}
	return out
}

func payloadOf(t *testing.T, v reqView) map[string]any {
	t.Helper()
	p, _ := v.Data["payload"].(map[string]any)
	if p == nil {
		t.Fatalf("no payload in %v", v.Data)
	}
	return p
}

func TestSessionStartCaptured(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	fx.env["HERDR_PANE_ID"] = "w1:p2"
	fx.env["HERDR_WORKSPACE_ID"] = "w1"
	fx.env["HERDR_SOCKET_PATH"] = "/home/u/.config/herdr/sessions/work/herdr.sock"

	stdin := fixtureFile(t, "SessionStart.json")
	// The captured payload has no session_title; add the documented field.
	stdin = strings.Replace(stdin, `"source":"startup"`, `"source":"startup","session_title":"my title"`, 1)
	if err := fx.call(t, "session-start", stdin); err != nil {
		t.Fatal(err)
	}
	rs := srv.requests()
	if len(rs) != 1 || rs[0].Method != "POST" || rs[0].Path != "/v1/sessions" {
		t.Fatalf("requests = %+v", rs)
	}
	if rs[0].Auth != "Bearer hub_m_test" {
		t.Errorf("auth = %q", rs[0].Auth)
	}
	var up api.SessionUpsert
	if err := json.Unmarshal(rs[0].Body, &up); err != nil {
		t.Fatal(err)
	}
	want := api.SessionUpsert{
		ID: sessionID, Agent: "claude", Source: "hooks", CWD: "/tmp/sessionhub-hook-probe",
		GitRepo: "git@example.com:o/r.git", GitBranch: "main",
		HerdrSession: "work", HerdrWorkspace: "w1", HerdrPane: "w1:p2", TitleHint: "my title",
	}
	if up != want {
		t.Errorf("upsert = %+v\nwant     %+v", up, want)
	}
	cur, err := os.ReadFile(filepath.Join(fx.state, "current", "4242"))
	if err != nil || string(cur) != sessionID {
		t.Errorf("current/4242 = %q, %v", cur, err)
	}
	if n := len(queued(t, fx.state)); n != 0 {
		t.Errorf("queue has %d items", n)
	}
}

func TestSessionStartOutsideHerdr(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	if err := fx.call(t, "session-start", fixtureFile(t, "SessionStart.json")); err != nil {
		t.Fatal(err)
	}
	var up map[string]any
	json.Unmarshal(srv.requests()[0].Body, &up)
	for _, k := range []string{"herdr_session", "herdr_workspace", "herdr_pane", "title_hint"} {
		if _, ok := up[k]; ok {
			t.Errorf("%s present outside herdr: %v", k, up[k])
		}
	}
}

func TestPromptCaptured(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	if err := fx.call(t, "prompt", fixtureFile(t, "UserPromptSubmit.json")); err != nil {
		t.Fatal(err)
	}
	vs := view(t, srv.requests())
	if len(vs) != 3 {
		t.Fatalf("got %d requests: %+v", len(vs), vs)
	}
	if vs[0].Path != "POST /v1/sessions" || vs[0].Data["first_prompt"] != "Reply with the single word ok" || vs[0].Data["agent_state"] != "working" {
		t.Errorf("upsert = %+v", vs[0])
	}
	evPath := "POST /v1/sessions/" + sessionID + "/events"
	if vs[1].Path != evPath || vs[1].Kind != "prompt" || vs[1].Src != "hooks" || payloadOf(t, vs[1])["prompt"] != "Reply with the single word ok" {
		t.Errorf("prompt event = %+v", vs[1])
	}
	if vs[2].Path != evPath || vs[2].Kind != "state_changed" || payloadOf(t, vs[2])["agent_state"] != "working" {
		t.Errorf("state event = %+v", vs[2])
	}
	if ts := vs[1].Data["ts"]; ts != "2026-09-30T12:00:00.123456789Z" {
		t.Errorf("ts = %v, want full-precision UTC", ts)
	}
}

func TestPromptTruncatesTo200Runes(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	long := strings.Repeat("é", 250)
	b, _ := json.Marshal(map[string]string{"session_id": sessionID, "cwd": "/x", "prompt": long})
	if err := fx.call(t, "prompt", string(b)); err != nil {
		t.Fatal(err)
	}
	vs := view(t, srv.requests())
	got := vs[0].Data["first_prompt"].(string)
	if n := len([]rune(got)); n != 200 {
		t.Errorf("first_prompt has %d runes", n)
	}
	if n := len([]rune(payloadOf(t, vs[1])["prompt"].(string))); n != 200 {
		t.Errorf("event prompt has %d runes", n)
	}
}

func TestStopAndNotification(t *testing.T) {
	cases := []struct{ event, file, state string }{
		{"stop", "Stop.json", "idle"},
		{"notification", "Notification.constructed.json", "blocked"},
	}
	for _, c := range cases {
		t.Run(c.event, func(t *testing.T) {
			srv := newFakeServer(t)
			fx := newFixture(t, srv.URL)
			if err := fx.call(t, c.event, fixtureFile(t, c.file)); err != nil {
				t.Fatal(err)
			}
			vs := view(t, srv.requests())
			if len(vs) != 1 || vs[0].Path != "POST /v1/sessions/"+sessionID+"/events" || vs[0].Kind != "state_changed" || vs[0].Src != "hooks" {
				t.Fatalf("requests = %+v", vs)
			}
			if payloadOf(t, vs[0])["agent_state"] != c.state {
				t.Errorf("payload = %v", vs[0].Data["payload"])
			}
		})
	}
}

func TestSessionEndQueuesOnlyAndRemovesCurrent(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	if err := fx.call(t, "session-start", fixtureFile(t, "SessionStart.json")); err != nil {
		t.Fatal(err)
	}
	before := len(srv.requests())
	if err := fx.call(t, "session-end", fixtureFile(t, "SessionEnd.json")); err != nil {
		t.Fatal(err)
	}
	if got := len(srv.requests()); got != before {
		t.Errorf("session-end made %d network calls", got-before)
	}
	if _, err := os.Stat(filepath.Join(fx.state, "current", "4242")); !os.IsNotExist(err) {
		t.Errorf("current/4242 still exists: %v", err)
	}
	items := queued(t, fx.state)
	if len(items) != 1 || items[0].Op != client.OpEvent || items[0].SessionID != sessionID {
		t.Fatalf("queue = %+v", items)
	}
	var ev api.EventIn
	json.Unmarshal(items[0].Body, &ev)
	if ev.Kind != "ended" || ev.Source != "hooks" || !strings.Contains(string(ev.Payload), `"other"`) {
		t.Errorf("event = %+v payload %s", ev, ev.Payload)
	}
}

func TestSessionEndKeepsCurrentOfNewerSession(t *testing.T) {
	fx := newFixture(t, "http://127.0.0.1:1")
	os.MkdirAll(filepath.Join(fx.state, "current"), 0o700)
	f := filepath.Join(fx.state, "current", "4242")
	os.WriteFile(f, []byte("another-session"), 0o600)
	if err := fx.call(t, "session-end", fixtureFile(t, "SessionEnd.json")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f); string(b) != "another-session" {
		t.Errorf("current file = %q", b)
	}
}

func TestSubagentIsSkipped(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	for _, ev := range []string{"session-start", "prompt", "stop", "notification", "session-end"} {
		in := `{"session_id":"` + sessionID + `","cwd":"/x","agent_id":"agent-1","agent_type":"Explore"}`
		if err := fx.call(t, ev, in); err != nil {
			t.Errorf("%s: %v", ev, err)
		}
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("%d requests for subagent calls", n)
	}
	if n := len(queued(t, fx.state)); n != 0 {
		t.Errorf("%d queued items for subagent calls", n)
	}
	if _, err := os.Stat(filepath.Join(fx.state, "current")); !os.IsNotExist(err) {
		t.Errorf("subagent wrote current/")
	}
}

func TestBadInputDoesNothing(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	inputs := []string{"", "not json", "{", "[]", "null", `{"cwd":"/x"}`, `{"session_id":5}`, strings.Repeat("x", 100)}
	for _, ev := range []string{"session-start", "prompt", "stop", "notification", "session-end"} {
		for _, in := range inputs {
			// Must return (never panic) and never send or queue anything.
			_ = fx.call(t, ev, in)
		}
	}
	if err := fx.h.run(context.Background(), []string{"bogus"}); err == nil {
		t.Error("unknown event should return an error")
	}
	if err := fx.h.run(context.Background(), nil); err == nil {
		t.Error("missing event should return an error")
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("%d requests", n)
	}
	if n := len(queued(t, fx.state)); n != 0 {
		t.Errorf("%d queued", n)
	}
}

func TestServerDownQueuesThenDrains(t *testing.T) {
	srv := newFakeServer(t)
	url := srv.URL
	srv.Close() // connection refused from here on
	fx := newFixture(t, url)
	start := time.Now()
	if err := fx.call(t, "prompt", fixtureFile(t, "UserPromptSubmit.json")); err != nil {
		t.Fatalf("server down must not error: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("hook took %v with the server down", d)
	}
	items := queued(t, fx.state)
	if len(items) != 3 || items[0].Op != client.OpUpsert || items[1].Op != client.OpEvent || items[2].Op != client.OpEvent {
		t.Fatalf("queue = %+v", items)
	}

	// The server comes back: the next hook sends its own item, then drains.
	up := newFakeServer(t)
	fx.h.newClient = func() (*client.Client, error) {
		return client.New(client.Config{ServerURL: up.URL, Token: "hub_m_test"})
	}
	if err := fx.call(t, "stop", fixtureFile(t, "Stop.json")); err != nil {
		t.Fatal(err)
	}
	vs := view(t, up.requests())
	if len(vs) != 4 {
		t.Fatalf("got %d requests, want 1 + 3 drained: %+v", len(vs), vs)
	}
	if n := len(queued(t, fx.state)); n != 0 {
		t.Errorf("queue still has %d items", n)
	}
}

func TestNoConfigQueues(t *testing.T) {
	fx := newFixture(t, "")
	if err := fx.call(t, "stop", fixtureFile(t, "Stop.json")); err != nil {
		t.Fatal(err)
	}
	if n := len(queued(t, fx.state)); n != 1 {
		t.Errorf("queue has %d items, want 1", n)
	}
}

func TestWatcherRunningSkipsDrain(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	fx.h.queue.Append(client.Item{Op: client.OpEvent, SessionID: "old", Body: json.RawMessage(`{"kind":"ended","source":"hooks","ts":"2026-09-30T00:00:00Z"}`)})

	fx.h.watcherRunning = func() bool { return true }
	if err := fx.call(t, "stop", fixtureFile(t, "Stop.json")); err != nil {
		t.Fatal(err)
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("%d requests with a watcher running, want 1", n)
	}
	if n := len(queued(t, fx.state)); n != 1 {
		t.Errorf("queue has %d items, want the old one", n)
	}

	fx.h.watcherRunning = func() bool { return false }
	if err := fx.call(t, "stop", fixtureFile(t, "Stop.json")); err != nil {
		t.Fatal(err)
	}
	if n := len(queued(t, fx.state)); n != 0 {
		t.Errorf("queue has %d items after drain", n)
	}
}

func TestDrainSendsAtMost50(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	for i := 0; i < 60; i++ {
		fx.h.queue.Append(client.Item{Op: client.OpEvent, SessionID: "s", Body: json.RawMessage(`{"kind":"prompt","source":"hooks","ts":"2026-09-30T00:00:00Z"}`)})
	}
	if err := fx.call(t, "stop", fixtureFile(t, "Stop.json")); err != nil {
		t.Fatal(err)
	}
	if n := len(srv.requests()); n != 1+50 {
		t.Errorf("%d requests, want 51", n)
	}
	if n := len(queued(t, fx.state)); n != 10 {
		t.Errorf("queue has %d items, want 10", n)
	}
}

func TestDrainStopsOnFailureAndDropsBadRequests(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	body := json.RawMessage(`{"kind":"prompt","source":"hooks","ts":"2026-09-30T00:00:00Z"}`)
	for _, id := range []string{"bad", "flaky", "after"} {
		fx.h.queue.Append(client.Item{Op: client.OpEvent, SessionID: id, Body: body})
	}
	srv.status = func(r rec) int {
		switch {
		case strings.Contains(r.Path, "/bad/"):
			return 400 // permanent: dropped
		case strings.Contains(r.Path, "/flaky/"):
			return 503 // retryable: kept, and stops the drain
		}
		return 200
	}
	if err := fx.call(t, "stop", fixtureFile(t, "Stop.json")); err != nil {
		t.Fatal(err)
	}
	items := queued(t, fx.state)
	if len(items) != 2 || items[0].SessionID != "flaky" || items[1].SessionID != "after" {
		t.Errorf("queue = %+v", items)
	}
	for _, r := range srv.requests() {
		if strings.Contains(r.Path, "/after/") {
			t.Error("drain continued past a retryable failure")
		}
	}
}

func TestUnknownSessionGetsUpsertedAndRetried(t *testing.T) {
	srv := newFakeServer(t)
	known := false
	srv.status = func(r rec) int {
		if r.Path == "/v1/sessions" {
			known = true
			return 200
		}
		if !known {
			return 404
		}
		return 200
	}
	fx := newFixture(t, srv.URL)
	fx.env["HERDR_PANE_ID"] = "w1:p1"
	if err := fx.call(t, "stop", fixtureFile(t, "Stop.json")); err != nil {
		t.Fatal(err)
	}
	vs := view(t, srv.requests())
	if len(vs) != 3 {
		t.Fatalf("requests = %+v", vs)
	}
	evPath := "POST /v1/sessions/" + sessionID + "/events"
	if vs[0].Path != evPath || vs[1].Path != "POST /v1/sessions" || vs[2].Path != evPath {
		t.Errorf("order = %s, %s, %s", vs[0].Path, vs[1].Path, vs[2].Path)
	}
	if vs[1].Data["id"] != sessionID || vs[1].Data["cwd"] != "/tmp/sessionhub-hook-probe" || vs[1].Data["git_branch"] != "main" ||
		vs[1].Data["source"] != "hooks" || vs[1].Data["agent"] != "claude" || vs[1].Data["herdr_pane"] != "w1:p1" {
		t.Errorf("fallback upsert = %v", vs[1].Data)
	}
	if n := len(queued(t, fx.state)); n != 0 {
		t.Errorf("queue has %d items", n)
	}
}

func TestQueuedEventFor404SessionGetsMinimalUpsert(t *testing.T) {
	srv := newFakeServer(t)
	known := map[string]bool{}
	srv.status = func(r rec) int {
		if r.Path == "/v1/sessions" {
			var m map[string]any
			json.Unmarshal(r.Body, &m)
			known[m["id"].(string)] = true
			return 200
		}
		parts := strings.Split(r.Path, "/")
		if len(parts) > 3 && !known[parts[3]] {
			return 404
		}
		return 200
	}
	fx := newFixture(t, srv.URL)
	fx.h.queue.Append(client.Item{Op: client.OpEvent, SessionID: "other-session", Body: json.RawMessage(`{"kind":"ended","source":"hooks","ts":"2026-09-30T00:00:00Z"}`)})
	known[sessionID] = true
	if err := fx.call(t, "stop", fixtureFile(t, "Stop.json")); err != nil {
		t.Fatal(err)
	}
	var upsert map[string]any
	for _, r := range srv.requests() {
		if r.Path == "/v1/sessions" {
			json.Unmarshal(r.Body, &upsert)
		}
	}
	if upsert == nil || upsert["id"] != "other-session" || upsert["cwd"] != nil {
		t.Errorf("upsert for the queued item = %v", upsert)
	}
	if n := len(queued(t, fx.state)); n != 0 {
		t.Errorf("queue has %d items", n)
	}
}

func TestWatcherLockDetection(t *testing.T) {
	dir := t.TempDir()
	if watcherRunning(dir) {
		t.Fatal("no watcher, but watcherRunning is true")
	}
	f, err := os.OpenFile(filepath.Join(dir, "watcher.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if !watcherRunning(dir) {
		t.Error("lock held, but watcherRunning is false")
	}
	f.Close()
	if watcherRunning(dir) {
		t.Error("lock released, but watcherRunning is true")
	}
}

func TestIsClaudeExe(t *testing.T) {
	for exe, want := range map[string]bool{
		"/home/u/.local/share/claude/versions/2.1.284": true,
		"/usr/local/bin/claude":                        true,
		"/usr/bin/claude-code":                         true,
		"/usr/bin/node":                                false,
		"/bin/sh":                                      false,
		"":                                             false,
	} {
		if got := isClaudeExe(exe); got != want {
			t.Errorf("isClaudeExe(%q) = %v", exe, got)
		}
	}
}

func TestClaudePID(t *testing.T) {
	// Under a Claude Code session the answer is the claude ancestor; else the
	// parent. Either way it must satisfy one of the two rules.
	got := claudePID()
	if got != os.Getppid() && !isClaudeExe(exeOf(got)) {
		t.Errorf("claudePID() = %d (exe %q): neither the parent nor a claude process", got, exeOf(got))
	}
	if ppidOf(os.Getpid()) != os.Getppid() {
		t.Errorf("ppidOf(self) = %d, want %d", ppidOf(os.Getpid()), os.Getppid())
	}
}

// Only a notification that waits on the user blocks the session. The other
// types send nothing, and neither does a payload without a type.
func TestNotificationTypesBlockOnlyWhenWaitingOnUser(t *testing.T) {
	base := fixtureFile(t, "Notification.constructed.json")
	withType := func(typ string) string {
		var m map[string]any
		if err := json.Unmarshal([]byte(base), &m); err != nil {
			t.Fatal(err)
		}
		if typ == "" {
			delete(m, "notification_type")
		} else {
			m["notification_type"] = typ
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	cases := []struct {
		typ     string
		blocked bool
	}{
		{"permission_prompt", true},
		{"elicitation_dialog", true},
		{"idle_prompt", false},
		{"auth_success", false},
		{"some_future_type", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run("type="+c.typ, func(t *testing.T) {
			srv := newFakeServer(t)
			fx := newFixture(t, srv.URL)
			if err := fx.call(t, "notification", withType(c.typ)); err != nil {
				t.Fatal(err)
			}
			vs := view(t, srv.requests())
			if !c.blocked {
				if len(vs) != 0 || len(queued(t, fx.state)) != 0 {
					t.Fatalf("type %q sent %+v, queued %d", c.typ, vs, len(queued(t, fx.state)))
				}
				return
			}
			if len(vs) != 1 || vs[0].Kind != "state_changed" || payloadOf(t, vs[0])["agent_state"] != "blocked" {
				t.Fatalf("type %q requests = %+v", c.typ, vs)
			}
		})
	}
}
