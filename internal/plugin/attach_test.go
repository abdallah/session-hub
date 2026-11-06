package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
)

const attachSession = "0a1b2c3d-4444-4222-8333-000000000000"

// paneHerdr is a herdr where pane wS:p1 runs Claude with attachSession and
// shows frame, until gone.
type paneHerdr struct {
	*herdrtest.Server
	mu    sync.Mutex
	frame string
	gone  bool
}

func newPaneHerdr(t *testing.T, frame string) *paneHerdr {
	h := &paneHerdr{Server: herdrtest.New(t, "pane-send-keys-esc.ndjson"), frame: frame}
	h.Handle("pane.get", func(json.RawMessage) (any, string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.gone {
			return nil, "pane_not_found"
		}
		return map[string]any{"pane": map[string]any{"pane_id": "wS:p1", "workspace_id": "wS", "agent": "claude",
			"agent_session": map[string]string{"kind": "id", "value": attachSession}}}, ""
	})
	h.Handle("pane.read", func(json.RawMessage) (any, string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return map[string]any{"read": map[string]any{"text": h.frame}}, ""
	})
	h.Handle("pane.send_input", func(json.RawMessage) (any, string) { return map[string]string{"type": "ok"}, "" })
	return h
}

func (h *paneHerdr) set(frame string, gone bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.frame, h.gone = frame, gone
}

func (h *paneHerdr) mirror(t *testing.T) herdrMirror {
	c, err := herdr.Dial(h.Path)
	if err != nil {
		t.Fatal(err)
	}
	return herdrMirror{h: c, socket: h.Path, every: 5 * time.Millisecond}
}

// syncBuf is a bytes.Buffer safe for one writer and one reader.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestHerdrMirror(t *testing.T) {
	h := newPaneHerdr(t, "\x1b[1mone\x1b[0m")
	var out syncBuf
	inR, inW := io.Pipe()
	defer inW.Close()
	done := make(chan error, 1)
	go func() {
		done <- h.mirror(t).Run(context.Background(), api.Session{ID: attachSession, HerdrPane: "wS:p1"}, &out, inR)
	}()

	waitFor(t, "first frame", func() bool { return out.String() == clearScreen+"\x1b[1mone\x1b[0m" })
	reads := len(h.RequestsFor("pane.read"))
	waitFor(t, "more reads", func() bool { return len(h.RequestsFor("pane.read")) >= reads+3 })
	if got := out.String(); got != clearScreen+"\x1b[1mone\x1b[0m" {
		t.Fatalf("unchanged pane wrote again: %q", got)
	}

	h.set("two", false)
	waitFor(t, "second frame", func() bool { return strings.HasSuffix(out.String(), "\x1b[0m"+clearScreen+"two") })

	if _, err := inW.Write([]byte("ls\r\x03")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "input", func() bool { return len(h.RequestsFor("pane.send_input")) == 1 })
	if p := string(h.RequestsFor("pane.send_input")[0].Params); p != `{"pane_id":"wS:p1","text":"ls\r\u0003"}` {
		t.Errorf("send_input params: %s", p)
	}

	h.set("two", true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after the pane went: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the pane went")
	}
}

// attachHub fakes the machine side of the attach routes: it sends input,
// collects output, and records the control result.
type attachHub struct {
	srv    *httptest.Server
	input  string
	hold   bool // keep the input stream open until the client goes
	out    syncBuf
	result chan api.ControlResultIn
}

func newAttachHub(t *testing.T, input string, hold bool) *attachHub {
	a := &attachHub{input: input, hold: hold, result: make(chan api.ControlResultIn, 4)}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines/self/attach/r1/input":
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, a.input)
			w.(http.Flusher).Flush()
			if a.hold {
				<-r.Context().Done()
				return
			}
			// The guest leaves once it has seen a frame.
			for end := time.Now().Add(2 * time.Second); time.Now().Before(end) && a.out.String() == ""; {
				time.Sleep(2 * time.Millisecond)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/machines/self/attach/r1/output":
			io.Copy(&a.out, r.Body)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/control/r1/result":
			var in api.ControlResultIn
			json.NewDecoder(r.Body).Decode(&in)
			a.result <- in
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *attachHub) client(t *testing.T) *client.Client {
	c, err := client.New(client.Config{ServerURL: a.srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (a *attachHub) wait(t *testing.T) api.ControlResultIn {
	t.Helper()
	select {
	case in := <-a.result:
		return in
	case <-time.After(3 * time.Second):
		t.Fatal("no control result")
	}
	return api.ControlResultIn{}
}

func attachClaim() api.ControlClaim {
	return api.ControlClaim{Request: api.ControlRequest{ID: "r1", Action: api.ActionAttach},
		Session: api.Session{ID: attachSession, HerdrPane: "wS:p1"}}
}

func TestControllerAttach(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hold   bool
		want   api.ControlResultIn
		finish func(h *paneHerdr)
	}{
		{"guest leaves", false, api.ControlResultIn{State: api.ControlDone, Detail: detailGuestLeft}, func(*paneHerdr) {}},
		{"session ends", true, api.ControlResultIn{State: api.ControlDone, Detail: detailSessionEnded}, func(h *paneHerdr) { h.set("frame", true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPaneHerdr(t, "frame")
			hub := newAttachHub(t, "q", tc.hold)
			var logs bytes.Buffer
			ctl := newController(nil, nil, log.New(&logs, "", 0))
			ctl.mirrorFor = func(api.Session) (mirror, error) { return h.mirror(t), nil }
			ctl.handle(context.Background(), hub.client(t), attachClaim()) // returns at once
			waitFor(t, "output", func() bool { return hub.out.String() != "" })
			tc.finish(h)
			if got := hub.wait(t); got != tc.want {
				t.Errorf("result %+v, want %+v; log:\n%s", got, tc.want, logs.String())
			}
			ctl.attaches.Wait()
			if got := hub.out.String(); got != clearScreen+"frame" {
				t.Errorf("output %q", got)
			}
			if r := h.RequestsFor("pane.send_input"); len(r) != 1 || string(r[0].Params) != `{"pane_id":"wS:p1","text":"q"}` {
				t.Errorf("send_input: %+v", r)
			}
		})
	}
}

func TestControllerAttachLimit(t *testing.T) {
	hub := newAttachHub(t, "", false)
	ctl := newController(nil, nil, log.New(io.Discard, "", 0))
	for range maxAttaches {
		ctl.attachSlots <- struct{}{}
	}
	ctl.handle(context.Background(), hub.client(t), attachClaim())
	if got := hub.wait(t); got.State != api.ControlFailed || got.Detail != detailTooMany {
		t.Errorf("result %+v", got)
	}
}

func TestBatchWriter(t *testing.T) {
	var mu sync.Mutex
	var posts []string
	b := &batchWriter{every: 20 * time.Millisecond, size: 8, post: func(p []byte) error {
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, string(p))
		return nil
	}}
	b.Write([]byte("ab"))
	b.Write([]byte("cd"))
	b.Write([]byte("efghij")) // reaches size: posted at once
	b.Write([]byte("k"))
	waitFor(t, "timed flush", func() bool { mu.Lock(); defer mu.Unlock(); return len(posts) == 2 })
	if posts[0] != "abcdefghij" || posts[1] != "k" {
		t.Errorf("posts %q", posts)
	}
}
