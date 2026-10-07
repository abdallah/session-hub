package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
)

const (
	uuidA = "11111111-2222-4333-8444-555555555555" // w7:p1 in the captured snapshot
	uuidB = "66666666-7777-4888-8999-aaaaaaaaaaaa" // w8:p1: restored, no detected agent, so not live
)

type testEnv struct {
	dir        string
	q          *client.Queue
	sessionhub *fakeHub
	herdr      *herdrtest.Server
	w          *watcher
	logs       *bytes.Buffer
	mu         sync.Mutex
	url        string
}

func (e *testEnv) setURL(u string) { e.mu.Lock(); e.url = u; e.mu.Unlock() }

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{dir: t.TempDir(), sessionhub: newFakeHub(t), logs: &bytes.Buffer{}}
	e.q = client.NewQueue(e.dir)
	e.herdr = herdrtest.New(t, "session-snapshot.ndjson")
	hc, err := herdr.Dial(e.herdr.Path)
	if err != nil {
		t.Fatal(err)
	}
	e.url = e.sessionhub.URL
	newClient := func() (*client.Client, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		return client.New(client.Config{ServerURL: e.url, Token: "hub_m_test"})
	}
	e.w = newWatcher(e.q, hc, "default", newClient, nil, log.New(e.logs, "", 0))
	return e
}

func (e *testEnv) append(t *testing.T, items ...client.Item) {
	t.Helper()
	for _, it := range items {
		if err := e.q.Append(it); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *testEnv) snapshots() int {
	n := 0
	for _, r := range e.herdr.Requests() {
		if r.Method == "session.snapshot" {
			n++
		}
	}
	return n
}

// Server down → queue retained → server up → each item sent exactly once.
func TestWatcherServerDownThenUp(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	status := capturedEvent(t, "pane.agent_status_changed-blocked")
	e.append(t,
		mustItem(t, onPane(t, capturedEvent(t, "pane.agent_detected"), "w7:p1"), "default", t0),
		mustItem(t, onPane(t, status, "w7:p1"), "default", t0),
		mustItem(t, onPane(t, status, "w7:p1"), "default", t0.Add(time.Second)),
		mustItem(t, onPane(t, capturedEvent(t, "pane.closed-agent-pane"), "w8:p1"), "default", t0.Add(time.Second)),
	)
	if n := len(queueItems(t, e.q)); n != 4 {
		t.Fatalf("start: %d items", n)
	}

	e.setURL(deadURL(t))
	e.w.step(ctx, t0)
	if got := e.sessionhub.requests(); len(got) != 0 {
		t.Fatalf("server down but sessionhub got %v", got)
	}
	left := queueItems(t, e.q)
	// Coalescing already removed the older state_changed; the rest stays.
	if len(left) != 3 {
		t.Fatalf("server down: %d items left, want 3", len(left))
	}
	if !strings.Contains(e.logs.String(), "send failed, keeping queue") {
		t.Errorf("no send failure line in log:\n%s", e.logs)
	}

	e.setURL(e.sessionhub.URL)
	e.w.step(ctx, t0.Add(2*time.Second))
	// w8:p1 kept its agent_session but herdr detects no agent there: it is
	// not a live session, so its pane_closed never resolves and stays queued.
	if left := queueItems(t, e.q); len(left) != 1 || left[0].PaneID != "w8:p1" {
		t.Fatalf("server up: queue %+v, want only the w8:p1 item", left)
	}
	var got []string
	for _, r := range e.sessionhub.requests() {
		got = append(got, r.Method+" "+r.Path)
	}
	want := []string{
		"POST /v1/sessions",                      // agent_detected, resolved to uuidA with the snapshot body
		"POST /v1/sessions/" + uuidA + "/events", // latest state
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	reqs := e.sessionhub.requests()
	var u api.SessionUpsert
	json.Unmarshal(reqs[0].Body, &u)
	if u.ID != uuidA || u.HerdrPane != "w7:p1" || u.CWD != "/home/user/project" || u.TitleHint != "session title" || u.Source != "plugin" {
		t.Errorf("upsert body %+v", u)
	}
	var ev api.EventIn
	json.Unmarshal(reqs[1].Body, &ev)
	var p eventPayload
	json.Unmarshal(ev.Payload, &p)
	if ev.Kind != api.KindStateChanged || p.AgentState != "blocked" || !ev.TS.Equal(t0.Add(time.Second)) {
		t.Errorf("state event %+v payload %+v", ev, p)
	}

	// Nothing is sent twice.
	e.w.step(ctx, t0.Add(4*time.Second))
	if n := len(e.sessionhub.requests()); n != len(want) {
		t.Errorf("after an idle pass sessionhub has %d requests, want %d", n, len(want))
	}

	// Heartbeat: t0's failed, the next is due 60 s after it.
	e.w.step(ctx, t0.Add(60*time.Second))
	if n := e.sessionhub.count(http.MethodPut, "/v1/machines/self/herdr-sessions"); n != 1 {
		t.Fatalf("heartbeats %d, want 1", n)
	}
	last := e.sessionhub.requests()[len(e.sessionhub.requests())-1]
	var put api.HerdrSessionsPut
	json.Unmarshal(last.Body, &put)
	if put.HerdrSession != "default" || len(put.Sessions) != 1 || put.Sessions[0].ID != uuidA {
		t.Errorf("heartbeat body %s", last.Body)
	}
}

// A pane with no session is retried for 10 minutes, then dropped; it is never
// sent, and the snapshot is refreshed at most once per pass.
func TestWatcherDropsUnresolvedAfterTenMinutes(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	// The captured probe pane w9:p1 never got an agent_session.
	e.append(t,
		mustItem(t, capturedEvent(t, "pane.agent_status_changed-blocked"), "default", t0),
		mustItem(t, capturedEvent(t, "pane.agent_detected"), "default", t0),
		// Same pane ID on another herdr server: never resolved against this one.
		mustItem(t, onPane(t, capturedEvent(t, "pane.agent_status_changed-blocked"), "w7:p1"), "other", t0),
	)
	e.w.step(ctx, t0) // the heartbeat's snapshot also serves the unknown pane
	before := e.snapshots()
	if before != 1 {
		t.Errorf("snapshots in the first step: %d, want 1", before)
	}
	e.w.step(ctx, t0.Add(9*time.Minute+59*time.Second))
	if n := len(queueItems(t, e.q)); n != 3 {
		t.Fatalf("before 10 min: %d items, want 3", n)
	}
	if d := e.snapshots() - before; d != 1 {
		t.Errorf("snapshots in one step: %d, want 1", d)
	}
	e.w.step(ctx, t0.Add(10*time.Minute))
	if n := len(queueItems(t, e.q)); n != 0 {
		t.Fatalf("after 10 min: %d items, want 0", n)
	}
	for _, r := range e.sessionhub.requests() {
		if r.Method == http.MethodPost {
			t.Errorf("unresolved item was sent: %s %s", r.Method, r.Path)
		}
	}
	if c := strings.Count(e.logs.String(), "no session after 10m0s"); c != 3 {
		t.Errorf("drop lines %d, want 3:\n%s", c, e.logs)
	}
}

// Retryable errors keep items; other 4xx drop them with a log line. Auth
// errors are in TestWatcherAuthErrorKeepsQueue.
func TestWatcherErrorClasses(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		status int
		kept   int
	}{
		{http.StatusServiceUnavailable, 1},
		{http.StatusTooManyRequests, 1},
		{http.StatusBadRequest, 0},
		{http.StatusConflict, 0},
	} {
		e := newTestEnv(t)
		e.sessionhub.known[uuidA] = true
		e.sessionhub.eventErr = c.status
		e.append(t, mustItem(t, onPane(t, capturedEvent(t, "pane.agent_status_changed-blocked"), "w7:p1"), "default", t0))
		e.w.step(ctx, t0)
		if n := len(queueItems(t, e.q)); n != c.kept {
			t.Errorf("HTTP %d: %d items kept, want %d; log:\n%s", c.status, n, c.kept, e.logs)
		}
		if c.kept == 0 && !strings.Contains(e.logs.String(), "dropping event state_changed") {
			t.Errorf("HTTP %d: no drop line:\n%s", c.status, e.logs)
		}
	}
}

// A herdr_sessions body queued by a failed startup is sent when it is newer
// than the last accepted heartbeat, and dropped unsent when it is older.
func TestWatcherQueuedHerdrSessions(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	body, _ := json.Marshal(api.HerdrSessionsPut{HerdrSession: "default", Sessions: []api.SessionUpsert{}})

	e := newTestEnv(t)
	e.append(t, client.Item{Op: client.OpHerdrSessions, Body: body, QueuedAt: t0.Add(-time.Second)})
	e.w.step(ctx, t0) // heartbeat accepted at t0, then the pass
	if n := e.sessionhub.count(http.MethodPut, "/v1/machines/self/herdr-sessions"); n != 1 {
		t.Errorf("stale queued body: %d PUTs, want 1 (the heartbeat only)", n)
	}
	if n := len(queueItems(t, e.q)); n != 0 {
		t.Errorf("stale queued body kept")
	}

	e.append(t, client.Item{Op: client.OpHerdrSessions, Body: body, QueuedAt: t0.Add(time.Second)})
	e.w.step(ctx, t0.Add(2*time.Second))
	if n := e.sessionhub.count(http.MethodPut, "/v1/machines/self/herdr-sessions"); n != 2 {
		t.Errorf("newer queued body: %d PUTs, want 2", n)
	}
}

// promptItem is a hooks-style item that already names its session. The body
// is synthetic (the watcher only forwards it), not a captured payload.
func promptItem(t *testing.T, id, session string, ts time.Time) client.Item {
	t.Helper()
	body, err := json.Marshal(api.EventIn{Kind: api.KindPrompt, Source: api.SourceHooks, TS: ts,
		Payload: json.RawMessage(`{"n":"` + id + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return client.Item{ID: id, Op: client.OpEvent, SessionID: session, Body: body, QueuedAt: ts}
}

// Hooks upserts that share a session and pane all reach the server, in queue
// order, with their own first_prompt; the plugin's upsert for the same pane
// is sent too. Before the fix, coalescing kept only the last of them.
func TestWatcherSendsEveryHooksUpsert(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	e.append(t,
		mustItem(t, onPane(t, capturedEvent(t, "pane.agent_detected"), "w7:p1"), "default", t0),
		hooksItem("Q", uuidA, "w7:p1", "first"),
		hooksItem("Q", uuidA, "w7:p1", "second"),
	)
	if n := len(queueItems(t, e.q)); n != 3 {
		t.Fatalf("start: %d items", n)
	}
	e.w.pass(ctx, t0)
	if n := len(queueItems(t, e.q)); n != 0 {
		t.Fatalf("%d items left, want 0; log:\n%s", n, e.logs)
	}
	var got []string
	for _, r := range e.sessionhub.requests() {
		if r.Method != http.MethodPost || r.Path != "/v1/sessions" {
			continue
		}
		var u api.SessionUpsert
		json.Unmarshal(r.Body, &u)
		got = append(got, u.Source+":"+u.FirstPrompt)
	}
	if want := "plugin: hooks:first hooks:second"; strings.Join(got, " ") != want {
		t.Errorf("upserts sent %q, want %q", strings.Join(got, " "), want)
	}
}

// An auth error (wrong or revoked token) stops the pass and keeps that item
// and every later one; nothing is dropped. With a good token, all are sent.
func TestWatcherAuthErrorKeepsQueue(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		e := newTestEnv(t)
		ctx := context.Background()
		t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
		e.sessionhub.known[uuidA] = true
		e.append(t, promptItem(t, "p1", uuidA, t0), promptItem(t, "p2", uuidA, t0), promptItem(t, "p3", uuidA, t0))
		if got := ids(queueItems(t, e.q)); got != "p1 p2 p3" {
			t.Fatalf("start: queue %q", got)
		}

		e.sessionhub.mu.Lock()
		e.sessionhub.status = status
		e.sessionhub.mu.Unlock()
		e.w.pass(ctx, t0)
		if n := e.sessionhub.count(http.MethodPost, "/v1/sessions/"+uuidA+"/events"); n != 1 {
			t.Errorf("HTTP %d: %d event posts, want 1 (the pass stops at the first)", status, n)
		}
		if got := ids(queueItems(t, e.q)); got != "p1 p2 p3" {
			t.Errorf("HTTP %d: queue %q, want all three kept", status, got)
		}
		if !strings.Contains(e.logs.String(), "check the token; keeping queue") || strings.Contains(e.logs.String(), "dropping") {
			t.Errorf("HTTP %d: log:\n%s", status, e.logs)
		}

		e.sessionhub.mu.Lock()
		e.sessionhub.status = 0
		e.sessionhub.mu.Unlock()
		e.w.pass(ctx, t0.Add(2*time.Second))
		if got := ids(queueItems(t, e.q)); got != "" {
			t.Errorf("HTTP %d then fixed: queue %q, want empty", status, got)
		}
		if n := e.sessionhub.count(http.MethodPost, "/v1/sessions/"+uuidA+"/events"); n != 4 {
			t.Errorf("HTTP %d then fixed: %d event posts, want 4 (1 refused + 3 sent)", status, n)
		}
	}
}

// A pass sends for at most passBudget. Items it does not reach stay queued in
// order, and an Append that arrives while the pass holds the queue lock waits
// less than 1.5 s. Later passes deliver the rest.
func TestWatcherPassBudget(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	e.sessionhub.known[uuidA] = true
	e.sessionhub.mu.Lock()
	e.sessionhub.delay = 400 * time.Millisecond
	e.sessionhub.mu.Unlock()
	var want []string
	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		want = append(want, id)
		e.append(t, promptItem(t, id, uuidA, t0))
	}

	appendWait := make(chan time.Duration, 1)
	go func() {
		time.Sleep(100 * time.Millisecond) // the pass holds the lock by now
		start := time.Now()
		if err := e.q.Append(promptItem(t, "late", uuidA, t0)); err != nil {
			t.Error(err)
		}
		appendWait <- time.Since(start)
	}()
	start := time.Now()
	e.w.pass(ctx, t0)
	took := time.Since(start)
	// The deadline cancels the send in flight at passBudget; the slack
	// covers scheduling on a loaded CI runner (1.33 s seen with every
	// package's tests running at once). A pass that kept sending past the
	// budget takes at least 1.6 s (a fourth 400 ms send), so it still fails.
	if took > passBudget+550*time.Millisecond {
		t.Errorf("pass took %s, want about %s", took, passBudget)
	}
	if d := <-appendWait; d > 1500*time.Millisecond {
		t.Errorf("Append waited %s on the queue lock, want < 1.5s", d)
	}
	left := queueItems(t, e.q)
	sent := 5 - (len(left) - 1) // "late" is queued too
	if sent < 1 || sent > 3 {
		t.Fatalf("one pass sent %d items, want 1 to 3 within %s; queue %q", sent, passBudget, ids(left))
	}
	if got, wantLeft := ids(left), strings.Join(append(append([]string{}, want[sent:]...), "late"), " "); got != wantLeft {
		t.Errorf("queue after one pass %q, want %q", got, wantLeft)
	}
	if !strings.Contains(e.logs.String(), "pass budget of 1s used up") {
		t.Errorf("no budget line in log:\n%s", e.logs)
	}

	e.sessionhub.mu.Lock()
	e.sessionhub.delay = 0
	e.sessionhub.mu.Unlock()
	e.w.pass(ctx, t0.Add(2*time.Second))
	if got := ids(queueItems(t, e.q)); got != "" {
		t.Fatalf("queue %q after the server sped up, want empty", got)
	}
	delivered := map[string]bool{}
	for _, r := range e.sessionhub.requests() {
		var ev api.EventIn
		json.Unmarshal(r.Body, &ev)
		var p struct{ N string }
		json.Unmarshal(ev.Payload, &p)
		delivered[p.N] = true
	}
	for _, id := range append(want, "late") {
		if !delivered[id] {
			t.Errorf("item %s never delivered", id)
		}
	}
}

// runWatcher holds the lock, refuses a second copy, and exits when the herdr
// socket disappears.
func TestRunWatcherLifecycle(t *testing.T) {
	dir := t.TempDir()
	// runWatcher loads or creates the move key next to the client config:
	// never the real ~/.config/sessionhub.
	cfgDir := t.TempDir()
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(cfgDir, "config.toml"))
	h := herdrtest.New(t, "session-snapshot.ndjson")
	sessionhub := newFakeHub(t)
	var logs bytes.Buffer
	var mu sync.Mutex
	logger := log.New(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return logs.Write(p) }), "", 0)
	done := make(chan error, 1)
	go func() {
		done <- runWatcher(context.Background(), dir, h.Path, sessionhub.clientFor(sessionhub.URL), nil, logger)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for sessionhub.count(http.MethodPut, "/v1/machines/self/herdr-sessions") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("watcher sent no heartbeat")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if running, _ := watcherRunning(dir); !running {
		t.Fatal("lock not held while the watcher runs")
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "move.key")); err != nil {
		t.Errorf("move key not created beside SESSIONHUB_CONFIG: %v", err)
	}
	pid, _ := os.ReadFile(filepath.Join(dir, watcherLockFile))
	if strings.TrimSpace(string(pid)) == "" {
		t.Error("lock file has no PID")
	}
	// A second watcher exits at once.
	if err := runWatcher(context.Background(), dir, h.Path, sessionhub.clientFor(sessionhub.URL), nil, logger); err != nil {
		t.Fatal(err)
	}
	os.Remove(h.Path)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not exit after the socket disappeared")
	}
	if running, _ := watcherRunning(dir); running {
		t.Error("lock still held after exit")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"another watcher holds", "herdr socket gone"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
}

// A new watcher that meets the lock held by a brief probe (what every
// `sessionhub plugin event` does) waits it out and runs. Against a lock held for
// longer than lockRetryFor it gives up and exits, as before.
func TestRunWatcherOutlastsLockProbe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	h := herdrtest.New(t, "session-snapshot.ndjson")
	sessionhub := newFakeHub(t)
	var logs bytes.Buffer
	var mu sync.Mutex
	logger := log.New(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return logs.Write(p) }), "", 0)
	logged := func() string { mu.Lock(); defer mu.Unlock(); return logs.String() }
	lockPath := filepath.Join(dir, watcherLockFile)

	// Held past the retry window: the watcher gives up, having tried for it.
	probe, held, err := tryLock(lockPath)
	if err != nil || held {
		t.Fatalf("test lock: held=%v err=%v", held, err)
	}
	start := time.Now()
	if err := runWatcher(context.Background(), dir, h.Path, sessionhub.clientFor(sessionhub.URL), nil, logger); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < lockRetryFor {
		t.Errorf("gave up after %s, want at least %s of retries", took, lockRetryFor)
	}
	if !strings.Contains(logged(), "another watcher holds") || len(sessionhub.requests()) != 0 {
		t.Fatalf("held lock: requests %v, log:\n%s", sessionhub.requests(), logged())
	}

	// Held by a probe for 60 ms: the watcher waits, then takes it and runs.
	done := make(chan error, 1)
	go func() {
		done <- runWatcher(context.Background(), dir, h.Path, sessionhub.clientFor(sessionhub.URL), nil, logger)
	}()
	time.Sleep(60 * time.Millisecond)
	probe.Close()
	deadline := time.Now().Add(3 * time.Second)
	for sessionhub.count(http.MethodPut, "/v1/machines/self/herdr-sessions") == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("watcher never ran after the probe let go; log:\n%s", logged())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if running, _ := watcherRunning(dir); !running {
		t.Error("watcher does not hold the lock")
	}
	if c := strings.Count(logged(), "another watcher holds"); c != 1 {
		t.Errorf("%d give-up lines, want 1 (from the first run only):\n%s", c, logged())
	}
	os.Remove(h.Path)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not exit after the socket disappeared")
	}
}

// mutableSnap serves the captured snapshot through the snapshotter
// interface, with pane fields a test may change between calls.
type mutableSnap struct {
	mu    sync.Mutex
	snap  herdr.Snapshot
	calls int
}

func capturedSnap(t *testing.T) *mutableSnap {
	t.Helper()
	var resp struct {
		Result struct {
			Snapshot herdr.Snapshot `json:"snapshot"`
		} `json:"result"`
	}
	lines := strings.Split(strings.TrimSpace(string(fixture(t, "socket/session-snapshot.ndjson"))), "\n")
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatal(err)
	}
	return &mutableSnap{snap: resp.Result.Snapshot}
}

func (m *mutableSnap) Snapshot() (herdr.Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	s := m.snap
	s.Panes = nil
	for _, p := range m.snap.Panes {
		if p.AgentSession != nil {
			as := *p.AgentSession
			p.AgentSession = &as
		}
		s.Panes = append(s.Panes, p)
	}
	return s, nil
}

// setSession gives a pane a new agent_session value, as /clear does.
func (m *mutableSnap) setSession(pane, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.snap.Panes {
		if m.snap.Panes[i].PaneID == pane {
			m.snap.Panes[i].AgentSession.Value = id
		}
	}
}

func (m *mutableSnap) count() int { m.mu.Lock(); defer m.mu.Unlock(); return m.calls }

// After /clear the pane keeps its ID but runs a new session. A state change
// queued after the cache was built goes to the new session, not the old one;
// an item older than the cache entry does not cost a snapshot.
func TestWatcherRefreshesCacheAfterClear(t *testing.T) {
	const uuidC = "cccccccc-2222-4333-8444-555555555555" // the session after /clear (not captured)
	e := newTestEnv(t)
	snap := capturedSnap(t)
	e.w.herdr = snap
	ctx := context.Background()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	e.w.step(ctx, t0) // heartbeat: cache w7:p1 → uuidA
	if got := e.w.cache["w7:p1"].u.ID; got != uuidA || snap.count() != 1 {
		t.Fatalf("start: w7:p1 cached as %q after %d snapshots, want %s after 1", got, snap.count(), uuidA)
	}

	// An item queued before the cache entry was seen resolves from the cache.
	status := onPane(t, capturedEvent(t, "pane.agent_status_changed-blocked"), "w7:p1")
	e.append(t, mustItem(t, status, "default", t0.Add(-time.Second)))
	e.w.step(ctx, t0.Add(2*time.Second))
	if snap.count() != 1 {
		t.Errorf("an item older than the cache took a snapshot: %d", snap.count())
	}
	if n := e.sessionhub.count(http.MethodPost, "/v1/sessions/"+uuidA+"/events"); n != 1 {
		t.Fatalf("old item: %d events to %s, want 1", n, uuidA)
	}

	snap.setSession("w7:p1", uuidC) // /clear in the pane
	e.sessionhub.known[uuidC] = true
	e.append(t, mustItem(t, status, "default", t0.Add(5*time.Second)))
	e.w.step(ctx, t0.Add(6*time.Second))
	if n := e.sessionhub.count(http.MethodPost, "/v1/sessions/"+uuidC+"/events"); n != 1 {
		t.Errorf("events to the new session %s: %d, want 1", uuidC, n)
	}
	if n := e.sessionhub.count(http.MethodPost, "/v1/sessions/"+uuidA+"/events"); n != 1 {
		t.Errorf("events to the old session %s: %d, want still 1", uuidA, n)
	}
	if snap.count() != 2 || e.w.cache["w7:p1"].u.ID != uuidC {
		t.Errorf("snapshots %d, cache %q; want 2 and %s", snap.count(), e.w.cache["w7:p1"].u.ID, uuidC)
	}
	if n := len(queueItems(t, e.q)); n != 0 {
		t.Errorf("%d items left", n)
	}
}

// On a 404 for an event, report, or title, the watcher upserts the session
// and retries once. The upsert carries the pane's snapshot data when the item
// names a cached pane of that session, else only id, agent, and source. Item
// bodies are synthetic (the watcher only forwards them).
func TestWatcherUpsertsOn404(t *testing.T) {
	const unknown = "dddddddd-2222-4333-8444-555555555555" // no pane runs it
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	bodies := map[string]any{
		client.OpEvent:  api.EventIn{Kind: api.KindPrompt, Source: api.SourceHooks, TS: t0},
		client.OpReport: api.ReportIn{Done: []string{"x"}, InFlight: []string{}, WaitingOn: []string{}},
		client.OpTitle:  api.TitleIn{Title: "t"},
	}
	suffix := map[string]string{client.OpEvent: "/events", client.OpReport: "/report", client.OpTitle: "/title"}
	fails := 0
	for _, op := range []string{client.OpEvent, client.OpReport, client.OpTitle} {
		for _, c := range []struct {
			name, sid, pane string
			want            api.SessionUpsert
		}{
			{"cached pane", uuidA, "w7:p1", api.SessionUpsert{ID: uuidA, Agent: "claude", Source: "plugin",
				CWD: "/home/user/project", HerdrSession: "default", HerdrWorkspace: "w7", HerdrPane: "w7:p1",
				AgentState: "idle", TitleHint: "session title"}},
			{"no pane", unknown, "", api.SessionUpsert{ID: unknown, Agent: "claude", Source: "plugin"}},
		} {
			e := newTestEnv(t)
			ctx := context.Background()
			if _, err := e.w.refresh(ctx, t0); err != nil { // cache without a heartbeat: the sessionhub knows nothing
				t.Fatal(err)
			}
			body, _ := json.Marshal(bodies[op])
			e.append(t, client.Item{ID: "i1", Op: op, SessionID: c.sid, PaneID: c.pane, Body: body, QueuedAt: t0})
			if len(e.sessionhub.requests()) != 0 || len(queueItems(t, e.q)) != 1 {
				t.Fatalf("%s %s: start state wrong", op, c.name)
			}
			e.w.pass(ctx, t0)
			var got []string
			for _, r := range e.sessionhub.requests() {
				got = append(got, r.Method+" "+r.Path)
			}
			path := "POST /v1/sessions/" + c.sid + suffix[op]
			want := []string{path, "POST /v1/sessions", path}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("%s %s: requests %v, want %v", op, c.name, got, want)
				fails++
				continue
			}
			var u api.SessionUpsert
			json.Unmarshal(e.sessionhub.requests()[1].Body, &u)
			if u != c.want {
				t.Errorf("%s %s: upsert\n got %+v\nwant %+v", op, c.name, u, c.want)
				fails++
			}
			if n := len(queueItems(t, e.q)); n != 0 || strings.Contains(e.logs.String(), "dropping") {
				t.Errorf("%s %s: %d items left, log:\n%s", op, c.name, n, e.logs)
				fails++
			}
		}
	}
	t.Logf("404 cases: 6, failures: %d", fails)
}

// The three task ops get the same upsert-and-retry on a 404.
func TestWatcherUpsertsOn404ForTaskOps(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		it   client.Item
		path string
	}{
		{client.Item{Op: client.OpTaskCreate, Body: []byte(`{"id":"t_abcdefghijk","title":"T"}`)}, "/v1/tasks"},
		{client.Item{Op: client.OpTaskState, TaskID: "t_x", Body: []byte(`{"to":"done_proposed"}`)}, "/v1/tasks/t_x/state"},
		{client.Item{Op: client.OpTaskLink, TaskID: "t_x", Body: []byte(`{"session_id":"` + uuidA + `"}`)}, "/v1/tasks/t_x/sessions"},
	} {
		t.Run(c.it.Op, func(t *testing.T) {
			e := newTestEnv(t)
			ctx := context.Background()
			c.it.ID, c.it.SessionID, c.it.QueuedAt = "i1", uuidA, t0
			e.append(t, c.it)
			e.w.pass(ctx, t0)
			var got []string
			for _, r := range e.sessionhub.requests() {
				got = append(got, r.Method+" "+r.Path)
			}
			want := []string{"POST " + c.path, "POST /v1/sessions", "POST " + c.path}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("requests %v, want %v", got, want)
			}
			if n := len(queueItems(t, e.q)); n != 0 {
				t.Errorf("%d items left, log:\n%s", n, e.logs)
			}
		})
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// The captured restored pane w8:p1 (agent_session kept, no detected agent) is
// not a live session: heartbeats leave it out, so the server ends it, and a
// pane.agent_detected or state change queued for that pane never revives it.
// The items wait for a snapshot that names the session, then are dropped.
func TestWatcherNeverRevivesRestoredPane(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	e.append(t,
		mustItem(t, onPane(t, capturedEvent(t, "pane.agent_detected"), "w8:p1"), "default", t0),
		mustItem(t, onPane(t, capturedEvent(t, "pane.agent_status_changed-blocked"), "w8:p1"), "default", t0),
	)
	if n := len(queueItems(t, e.q)); n != 2 {
		t.Fatalf("start: %d items", n)
	}
	for i := 0; i < 3; i++ {
		e.w.step(ctx, t0.Add(time.Duration(i)*heartbeatInterval))
	}
	var put api.HerdrSessionsPut
	beats := 0
	for _, r := range e.sessionhub.requests() {
		switch {
		case r.Method == http.MethodPut:
			beats++
			json.Unmarshal(r.Body, &put)
			for _, s := range put.Sessions {
				if s.ID == uuidB || s.HerdrPane == "w8:p1" {
					t.Errorf("heartbeat revives the restored pane: %s", r.Body)
				}
			}
		case strings.Contains(r.Path, uuidB), r.Method == http.MethodPost && r.Path == "/v1/sessions":
			t.Errorf("sent for the restored pane: %s %s %s", r.Method, r.Path, r.Body)
		}
	}
	if beats != 3 || len(put.Sessions) != 1 || put.Sessions[0].ID != uuidA {
		t.Errorf("heartbeats %d, last body %+v; want 3 carrying only %s", beats, put, uuidA)
	}
	if n := len(queueItems(t, e.q)); n != 2 {
		t.Errorf("before 10 min: %d items, want 2 waiting", n)
	}
	e.w.step(ctx, t0.Add(unresolvedTTL))
	if n := len(queueItems(t, e.q)); n != 0 {
		t.Errorf("after 10 min: %d items, want 0", n)
	}
	if c := strings.Count(e.logs.String(), "for pane w8:p1: no session after"); c != 2 {
		t.Errorf("drop lines %d, want 2:\n%s", c, e.logs)
	}
}

// digestRig drives the heartbeat digests with a fake transcript table.
type digestRig struct {
	e      *testEnv
	stats  map[string]transcriptMark
	called []string
	fail   map[string]error
	now    time.Time
}

func newDigestRig(t *testing.T, ids ...string) *digestRig {
	t.Helper()
	r := &digestRig{e: newTestEnv(t), stats: map[string]transcriptMark{}, fail: map[string]error{},
		now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	for i, id := range ids {
		r.e.w.cache[fmt.Sprintf("w9:p%d", i)] = cacheEntry{u: api.SessionUpsert{ID: id}, seen: r.now}
		r.stats[id] = transcriptMark{size: 100, mod: r.now}
	}
	r.e.w.transcriptStat = func(id string) (int64, time.Time, bool) {
		m, ok := r.stats[id]
		return m.size, m.mod, ok
	}
	r.e.w.digest = func(_ context.Context, id string) error {
		r.called = append(r.called, id)
		return r.fail[id]
	}
	return r
}

// beat runs one heartbeat a minute after the last and returns the sessions it
// digested.
func (r *digestRig) beat() []string {
	r.called = nil
	r.now = r.now.Add(heartbeatInterval)
	r.e.w.heartbeat(context.Background(), r.now)
	return r.called
}

func TestHeartbeatDigestsChangedTranscripts(t *testing.T) {
	r := newDigestRig(t, "sess-1", "sess-2")

	// 1. First heartbeat: both digested.
	if got := r.beat(); strings.Join(got, ",") != "sess-1,sess-2" {
		t.Fatalf("first heartbeat digested %v, want sess-1 and sess-2", got)
	}
	// 2. Nothing changed.
	if got := r.beat(); len(got) != 0 {
		t.Fatalf("unchanged heartbeat digested %v", got)
	}
	// 3. sess-1 grows.
	r.stats["sess-1"] = transcriptMark{size: 200, mod: r.now}
	if got := r.beat(); strings.Join(got, ",") != "sess-1" {
		t.Fatalf("after growth digested %v, want sess-1", got)
	}
	// 4. No transcript for sess-2: skipped, nothing logged.
	delete(r.stats, "sess-2")
	r.stats["sess-1"] = transcriptMark{size: 300, mod: r.now}
	r.e.logs.Reset()
	if got := r.beat(); strings.Join(got, ",") != "sess-1" {
		t.Fatalf("missing transcript: digested %v, want only sess-1", got)
	}
	if strings.Contains(r.e.logs.String(), "digest") {
		t.Errorf("a missing transcript logged:\n%s", r.e.logs)
	}
	// 5. A failure is logged once and retried with the same size.
	r.stats["sess-1"] = transcriptMark{size: 400, mod: r.now}
	r.fail["sess-1"] = errors.New("boom")
	r.e.logs.Reset()
	if got := r.beat(); strings.Join(got, ",") != "sess-1" {
		t.Fatalf("failing digest ran %v", got)
	}
	if got := r.e.logs.String(); got != "digest sess-1: boom\n" {
		t.Errorf("log %q, want one %q line", got, "digest sess-1: boom")
	}
	delete(r.fail, "sess-1")
	if got := r.beat(); strings.Join(got, ",") != "sess-1" {
		t.Fatalf("retry digested %v, want sess-1", got)
	}
	if got := r.beat(); len(got) != 0 {
		t.Fatalf("after the retry digested %v", got)
	}
}

// 6. At most maxDigestsPerBeat per heartbeat; the rest wait.
func TestHeartbeatDigestsAtMostTenPerBeat(t *testing.T) {
	var ids []string
	for i := 0; i < 12; i++ {
		ids = append(ids, fmt.Sprintf("sess-%02d", i))
	}
	r := newDigestRig(t, ids...)
	if got := r.beat(); len(got) != maxDigestsPerBeat {
		t.Fatalf("first heartbeat digested %d, want %d", len(got), maxDigestsPerBeat)
	}
	got := r.beat()
	if strings.Join(got, ",") != "sess-10,sess-11" {
		t.Fatalf("second heartbeat digested %v, want the last two", got)
	}
}

// The digested map forgets sessions that left the pane cache.
func TestDigestsPruneForgottenSessions(t *testing.T) {
	r := newDigestRig(t, "sess-1")
	r.beat()
	if _, ok := r.e.w.digested["sess-1"]; !ok {
		t.Fatal("sess-1 not recorded")
	}
	r.e.w.cache = map[string]cacheEntry{}
	r.e.w.digests(context.Background(), r.now)
	if _, ok := r.e.w.digested["sess-1"]; ok {
		t.Error("digested still holds a forgotten session")
	}
}

// Sessions whose digest keeps failing do not starve the others: a session
// that has not failed is tried before one that has.
func TestHeartbeatDigestsFailingSessionsDoNotStarve(t *testing.T) {
	var ids []string
	for i := 0; i < 10; i++ {
		ids = append(ids, fmt.Sprintf("a-fail-%02d", i))
	}
	ids = append(ids, "z-ok-1", "z-ok-2")
	r := newDigestRig(t, ids...)
	for _, id := range ids[:10] {
		r.fail[id] = errors.New("boom")
	}
	if got := r.beat(); len(got) != maxDigestsPerBeat {
		t.Fatalf("first heartbeat attempted %d, want %d", len(got), maxDigestsPerBeat)
	}
	got := r.beat()
	if len(got) < 2 || got[0] != "z-ok-1" || got[1] != "z-ok-2" {
		t.Fatalf("second heartbeat attempted %v, want z-ok-1 and z-ok-2 first", got)
	}
	if len(got) > maxDigestsPerBeat {
		t.Errorf("attempted %d, over the bound", len(got))
	}
	for _, id := range []string{"z-ok-1", "z-ok-2"} {
		if _, ok := r.e.w.digested[id]; !ok {
			t.Errorf("%s not digested", id)
		}
	}
}
