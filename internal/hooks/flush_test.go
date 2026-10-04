package hooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/client"
)

// TestMain lets the test binary act as the detached `sessionhub hook flush` child.
func TestMain(m *testing.M) {
	if os.Getenv("SESSIONHUB_HOOKS_TEST_CHILD") == "1" {
		// Same entry point as `sessionhub hook flush`.
		if err := Run(context.Background(), []string{"flush"}); err != nil {
			os.Exit(0) // the real command also exits 0 on every error
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSessionEndStartsDetachedFlush(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	started := 0
	fx.h.flushCmd = func() *exec.Cmd {
		started++
		return exec.Command("true")
	}
	if err := fx.call(t, "session-end", fixtureFile(t, "SessionEnd.json")); err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Errorf("flush started %d times", started)
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("session-end itself made %d network calls", n)
	}
	if n := len(queued(t, fx.state)); n != 1 {
		t.Errorf("queue has %d items, want the ended event", n)
	}
}

func TestSessionEndSurvivesFlushStartFailure(t *testing.T) {
	fx := newFixture(t, "http://127.0.0.1:1")
	fx.h.flushCmd = func() *exec.Cmd { return exec.Command("/nonexistent/sessionhub", "hook", "flush") }
	if err := fx.call(t, "session-end", fixtureFile(t, "SessionEnd.json")); err != nil {
		t.Fatalf("session-end must not fail: %v", err)
	}
	if n := len(queued(t, fx.state)); n != 1 {
		t.Errorf("queue has %d items", n)
	}
}

// A real child process (this test binary) sends the ended event to httptest.
func TestDetachedChildDeliversEnded(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	fx.h.flushCmd = func() *exec.Cmd {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(),
			"SESSIONHUB_HOOKS_TEST_CHILD=1",
			"SESSIONHUB_STATE_DIR="+fx.state,
			"SESSIONHUB_SERVER_URL="+srv.URL,
			"SESSIONHUB_TOKEN=hub_m_test",
			"SESSIONHUB_CONFIG="+fx.state+"/none.toml",
		)
		return cmd
	}
	start := time.Now()
	if err := fx.call(t, "session-end", fixtureFile(t, "SessionEnd.json")); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("session-end took %v", d)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(srv.requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	vs := view(t, srv.requests())
	if len(vs) != 1 || vs[0].Path != "POST /v1/sessions/"+sessionID+"/events" || vs[0].Kind != "ended" || vs[0].Src != "hooks" {
		t.Fatalf("requests = %+v", vs)
	}
	for time.Now().Before(deadline) && len(queued(t, fx.state)) != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(queued(t, fx.state)); n != 0 {
		t.Errorf("queue has %d items after the child ran", n)
	}
}

func TestFlushRules(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	ended := json.RawMessage(`{"kind":"ended","source":"hooks","ts":"2026-09-30T00:00:00Z"}`)
	fx.h.queue.Append(client.Item{Op: client.OpEvent, SessionID: "s", Body: ended})

	fx.h.watcherRunning = func() bool { return true }
	if err := fx.h.run(context.Background(), []string{"flush"}); err != nil || len(srv.requests()) != 0 {
		t.Fatalf("flush with a watcher: err=%v requests=%d", err, len(srv.requests()))
	}
	fx.h.watcherRunning = func() bool { return false }
	if err := fx.h.run(context.Background(), []string{"flush"}); err != nil {
		t.Fatal(err)
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("%d requests", n)
	}
	if n := len(queued(t, fx.state)); n != 0 {
		t.Errorf("queue has %d items", n)
	}
	// Empty queue and missing config: quiet, no error, no requests.
	if err := fx.h.run(context.Background(), []string{"flush"}); err != nil {
		t.Error(err)
	}
	fx.h.newClient = func() (*client.Client, error) { return client.New(client.Config{}) }
	fx.h.queue.Append(client.Item{Op: client.OpEvent, SessionID: "s", Body: ended})
	_ = fx.h.run(context.Background(), []string{"flush"})
	if n := len(queued(t, fx.state)); n != 1 {
		t.Errorf("queue has %d items with no config", n)
	}
}

func TestFlushUpsertsUnknownSessionBeforeEnded(t *testing.T) {
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
	fx.h.queue.Append(client.Item{Op: client.OpEvent, SessionID: "s", Body: json.RawMessage(`{"kind":"ended","source":"hooks","ts":"2026-09-30T00:00:00Z"}`)})
	if err := fx.h.run(context.Background(), []string{"flush"}); err != nil {
		t.Fatal(err)
	}
	if n := len(srv.requests()); n != 3 {
		t.Errorf("%d requests, want event, upsert, event", n)
	}
	if n := len(queued(t, fx.state)); n != 0 {
		t.Errorf("queue has %d items", n)
	}
}

func TestAuthErrorKeepsQueue(t *testing.T) {
	srv := newFakeServer(t)
	srv.status = func(r rec) int { return 401 }
	fx := newFixture(t, srv.URL)
	body := json.RawMessage(`{"kind":"prompt","source":"hooks","ts":"2026-09-30T00:00:00Z"}`)
	for _, id := range []string{"a", "b", "c"} {
		fx.h.queue.Append(client.Item{Op: client.OpEvent, SessionID: id, Body: body})
	}
	// The hook's own item is kept too, and nothing is dropped.
	if err := fx.call(t, "stop", fixtureFile(t, "Stop.json")); err != nil {
		t.Fatal(err)
	}
	items := queued(t, fx.state)
	if len(items) != 4 {
		t.Fatalf("queue has %d items, want 3 old + the new one: %+v", len(items), items)
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("%d requests after a 401, want 1 (live send only)", n)
	}

	// Drain alone (token now fixed for the live send but rejected in the queue).
	srv.mu.Lock()
	srv.reqs = nil
	srv.status = func(r rec) int {
		if strings.Contains(r.Path, "/b/") {
			return 403
		}
		return 200
	}
	srv.mu.Unlock()
	if err := fx.h.run(context.Background(), []string{"flush"}); err != nil {
		t.Fatal(err)
	}
	got := queued(t, fx.state)
	if len(got) != 3 || got[0].SessionID != "b" || got[1].SessionID != "c" {
		t.Errorf("queue after 403 on b = %+v (a was sent; b and c must stay)", got)
	}
}

func TestDrainBudgetIsPerPass(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer slow.Close()
	fx := newFixture(t, slow.URL)
	body := json.RawMessage(`{"kind":"prompt","source":"hooks","ts":"2026-09-30T00:00:00Z"}`)
	for i := 0; i < 20; i++ {
		fx.h.queue.Append(client.Item{Op: client.OpEvent, SessionID: "s", Body: body})
	}
	start := time.Now()
	if err := fx.h.run(context.Background(), []string{"flush"}); err != nil {
		t.Fatal(err)
	}
	d := time.Since(start)
	// Without a per-pass budget, 20 items at 300 ms take 6 s. The margin is
	// wide so a loaded machine does not fail the test.
	if d > 3*time.Second {
		t.Errorf("drain pass took %v, want about 1 s (no budget would take 6 s)", d)
	}
	n := len(queued(t, fx.state))
	if n < 1 || n >= 20 {
		t.Errorf("queue has %d items; some but not all should have been sent", n)
	}
}

func TestAppendDuringDrainWaitsUnderBudget(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
	}))
	defer slow.Close()
	fx := newFixture(t, slow.URL)
	body := json.RawMessage(`{"kind":"prompt","source":"hooks","ts":"2026-09-30T00:00:00Z"}`)
	for i := 0; i < 10; i++ {
		fx.h.queue.Append(client.Item{Op: client.OpEvent, SessionID: "s", Body: body})
	}
	done := make(chan struct{})
	go func() {
		fx.h.run(context.Background(), []string{"flush"})
		close(done)
	}()
	time.Sleep(100 * time.Millisecond) // the drain holds the queue lock now
	start := time.Now()
	fx.h.queue.Append(client.Item{Op: client.OpEvent, SessionID: "late", Body: body})
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("append waited %v, want under the 1.5 s SessionEnd budget", d)
	}
	<-done
}

func TestHookDeadlineCoversLiveSends(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	defer hang.Close()
	fx := newFixture(t, hang.URL)
	fx.h.deadline = 300 * time.Millisecond
	start := time.Now()
	if err := fx.call(t, "prompt", fixtureFile(t, "UserPromptSubmit.json")); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("hook took %v with a 300 ms deadline", d)
	}
	if n := len(queued(t, fx.state)); n != 3 {
		t.Errorf("queue has %d items, want all 3 kept", n)
	}
}

func TestStopAndSessionEndStartDigest(t *testing.T) {
	stop := `{"session_id":"sess-1"}`
	t.Run("stop and session-end start one digest each", func(t *testing.T) {
		fx := newFixture(t, "http://127.0.0.1:1")
		var ids []string
		fx.h.digestCmd = func(id string) *exec.Cmd {
			ids = append(ids, id)
			return exec.Command("true")
		}
		if err := fx.call(t, "stop", stop); err != nil {
			t.Fatal(err)
		}
		if err := fx.call(t, "session-end", stop); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(ids, ","); got != "sess-1,sess-1" {
			t.Errorf("digest IDs %q, want sess-1,sess-1", got)
		}
	})
	t.Run("a failed start is logged and the hook succeeds", func(t *testing.T) {
		fx := newFixture(t, "http://127.0.0.1:1")
		fx.h.digestCmd = func(id string) *exec.Cmd {
			return exec.Command("/nonexistent/sessionhub", "digest", id)
		}
		if err := fx.call(t, "stop", stop); err != nil {
			t.Fatalf("stop must not fail: %v", err)
		}
		if n := strings.Count(fx.errs.String(), "hook: start digest:"); n != 1 {
			t.Errorf("stderr %q has %d digest lines, want 1", fx.errs, n)
		}
	})
	t.Run("prompt starts no digest", func(t *testing.T) {
		fx := newFixture(t, "http://127.0.0.1:1")
		started := 0
		fx.h.digestCmd = func(string) *exec.Cmd { started++; return exec.Command("true") }
		if err := fx.call(t, "prompt", `{"session_id":"sess-1","prompt":"hi"}`); err != nil {
			t.Fatal(err)
		}
		if started != 0 {
			t.Errorf("prompt started %d digests", started)
		}
	})
}
