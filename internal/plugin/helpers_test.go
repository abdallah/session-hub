package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
)

// TestMain doubles as the "sessionhub" binary that ensureWatcher starts, selected by
// SESSIONHUB_PLUGIN_TEST_HELPER:
//
//   - "exit": exit at once (event handler tests that only need a spawn).
//   - "watch": take watcher.lock like runWatcher, record the PID in
//     <state>/holders when it won, and hold the lock until <state>/stop exists.
func TestMain(m *testing.M) {
	switch os.Getenv("SESSIONHUB_PLUGIN_TEST_HELPER") {
	case "exit":
		// The marker tells a test the watcher was spawned: watcher.log is no
		// proof, since sessionhub plugin event also writes to it.
		if dir := os.Getenv("SESSIONHUB_STATE_DIR"); dir != "" {
			os.WriteFile(filepath.Join(dir, "spawned"), nil, 0o600)
		}
		os.Exit(0)
	case "watch":
		dir := os.Getenv("SESSIONHUB_STATE_DIR")
		f, held, err := tryLock(filepath.Join(dir, watcherLockFile))
		if err != nil || held {
			os.Exit(0)
		}
		fmt.Fprintf(f, "%d\n", os.Getpid())
		h, _ := os.OpenFile(filepath.Join(dir, "holders"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		fmt.Fprintf(h, "%d\n", os.Getpid())
		h.Close()
		for i := 0; i < 500; i++ {
			if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(herdrtest.FixtureDir(), "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// capturedEvent parses testdata/herdr/events/<name>.json.
func capturedEvent(t *testing.T, name string) herdr.Event {
	t.Helper()
	e, err := herdr.ParseEvent(fixture(t, "events/"+name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// onPane returns a captured event with data.pane_id replaced. The captured
// probe pane (w9:p1) never had an agent_session, so tests that need a pane
// with a session move the real payload onto w7:p1 of the captured snapshot.
// Every other byte stays as captured.
func onPane(t *testing.T, e herdr.Event, pane string) herdr.Event {
	t.Helper()
	var d map[string]json.RawMessage
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatal(err)
	}
	d["pane_id"], _ = json.Marshal(pane)
	e.Data, _ = json.Marshal(d)
	return e
}

type hubRequest struct {
	Method, Path string
	Body         json.RawMessage
}

// fakeHub is an httptest sessionhub server that records requests. Events, reports,
// and titles for a session it has not seen an upsert for return 404, like the
// real server.
type fakeHub struct {
	*httptest.Server
	mu       sync.Mutex
	reqs     []hubRequest
	known    map[string]bool
	status   int           // when non-zero, every request gets this status
	eventErr int           // when non-zero, event posts get this status
	delay    time.Duration // when non-zero, every request waits this long first
	putBody  string        // when set, the response body of herdr-sessions PUTs

	inboxStatus int    // when non-zero, GET /v1/inbox answers this status
	inboxBody   string // the body of GET /v1/inbox
	rulesStatus int    // when non-zero, GET /v1/instructions answers this status
	rulesBody   string // the body of GET /v1/instructions
}

func newFakeHub(t *testing.T) *fakeHub {
	h := &fakeHub{known: map[string]bool{}}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		delay := h.delay
		h.mu.Unlock()
		if delay > 0 {
			// A slow server. A request the client gave up on is not recorded.
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.reqs = append(h.reqs, hubRequest{r.Method, r.URL.Path, body})
		if h.status != 0 {
			http.Error(w, `{"error":"forced"}`, h.status)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/instructions":
			if h.rulesStatus != 0 {
				http.Error(w, `{"error":"forced"}`, h.rulesStatus)
				return
			}
			w.Write([]byte(h.rulesBody))
			return
		case r.Method == http.MethodGet && r.URL.Path == "/v1/inbox":
			if h.inboxStatus != 0 {
				http.Error(w, `{"error":"forced"}`, h.inboxStatus)
				return
			}
			w.Write([]byte(h.inboxBody))
			return
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
			var u struct{ ID string }
			json.Unmarshal(body, &u)
			h.known[u.ID] = true
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/tasks"):
			// Agent task calls name the session in the body.
			var b struct {
				SessionID string `json:"session_id"`
			}
			json.Unmarshal(body, &b)
			if !h.known[b.SessionID] {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":"unknown session"}`))
				return
			}
		case r.Method == http.MethodPut && r.URL.Path == "/v1/machines/self/herdr-sessions":
			var p struct{ Sessions []struct{ ID string } }
			json.Unmarshal(body, &p)
			for _, s := range p.Sessions {
				h.known[s.ID] = true
			}
		case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/events") ||
			strings.HasSuffix(r.URL.Path, "/report") || strings.HasSuffix(r.URL.Path, "/title")):
			if h.eventErr != 0 && strings.HasSuffix(r.URL.Path, "/events") {
				http.Error(w, `{"error":"forced"}`, h.eventErr)
				return
			}
			id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
			id = id[:strings.LastIndex(id, "/")]
			if !h.known[id] {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":"unknown session"}`))
				return
			}
		}
		if r.Method == http.MethodPut && h.putBody != "" {
			w.Write([]byte(h.putBody))
			return
		}
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *fakeHub) requests() []hubRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hubRequest(nil), h.reqs...)
}

func (h *fakeHub) count(method, path string) int {
	n := 0
	for _, r := range h.requests() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

// setInbox sets the status and body GET /v1/inbox answers (status 0: 200).
func (h *fakeHub) setInbox(status int, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inboxStatus, h.inboxBody = status, body
}

func (h *fakeHub) clientFor(url string) func() (*client.Client, error) {
	return func() (*client.Client, error) { return client.New(client.Config{ServerURL: url, Token: "hub_m_test"}) }
}

// deadURL is a URL where nothing listens (connection refused).
func deadURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr
}

func queueItems(t *testing.T, q *client.Queue) []client.Item {
	t.Helper()
	var out []client.Item
	if err := q.Drain(func(items []client.Item) []client.Item {
		out = append(out, items...)
		return items
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustItem(t *testing.T, e herdr.Event, session string, now time.Time) client.Item {
	t.Helper()
	it, ok, err := itemFromEvent(e, session, now)
	if err != nil || !ok {
		t.Fatalf("itemFromEvent(%s): ok=%v err=%v", e.Event, ok, err)
	}
	return it
}

// reap waits for a spawned helper so it leaves no zombie.
func reap(p *os.Process) {
	if p != nil {
		p.Wait()
	}
}
