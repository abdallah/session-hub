package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

const testLink = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"

func TestRemoteControlCreate(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1, HerdrSession: "default", HerdrPane: "w1:p1"})
	e.register(e.tokA, api.SessionUpsert{ID: sid2}) // hooks only: no pane
	before := e.snapshot()
	for _, tc := range []struct {
		id   string
		want int
		msg  string
	}{
		{sid3, 404, "not found"},
		{sid2, 409, "not in herdr"},
		{sid1, 409, "offline"}, // tower's watcher never polled
		{"-x", 400, "invalid session id"},
	} {
		if code, _, b := e.tap(tc.id); code != tc.want || !strings.Contains(string(b), tc.msg) {
			t.Errorf("tap %s: %d %s, want %d with %q", tc.id, code, b, tc.want, tc.msg)
		}
	}
	if after := e.snapshot(); after != before {
		t.Fatalf("refused requests changed state:\nbefore %s\nafter  %s", before, after)
	}

	e.recordPoll(e.tokA)
	if d := e.detail(sid1); !d.Controllable || d.RemoteControl != nil {
		t.Fatalf("after a poll: controllable=%v request=%+v", d.Controllable, d.RemoteControl)
	}
	code, first, b := e.tap(sid1)
	if code != http.StatusAccepted || first.State != api.ControlPending || first.RequestedBy != api.RequestedByDashboard ||
		first.Machine != "tower" || !first.ExpiresAt.Equal(e.clock.Now().Add(2*time.Minute)) {
		t.Fatalf("first tap: %d %s", code, b)
	}
	// A second tap, and bluebox's CLI, get the open request.
	if code, again, _ := e.tap(sid1); code != http.StatusOK || again.ID != first.ID {
		t.Errorf("second tap: %d %+v", code, again)
	}
	var viaBluebox api.ControlRequest
	e.must(http.StatusOK, "POST", "/v1/sessions/"+sid1+"/remote-control", e.tokB, nil, &viaBluebox)
	if viaBluebox.ID != first.ID {
		t.Errorf("bluebox's request: %+v", viaBluebox)
	}
	d := e.detail(sid1)
	n := 0
	for _, ev := range d.Events {
		if ev.Kind == api.KindRemoteControlRequested {
			n++
		}
	}
	if d.RemoteControl == nil || d.RemoteControl.ID != first.ID || n != 1 {
		t.Errorf("session: request %+v, %d requested events", d.RemoteControl, n)
	}

	// No poll for over 2 minutes: not controllable, the request expired,
	// and a tap is refused.
	e.clock.Advance(2*time.Minute + time.Second)
	if d := e.detail(sid1); d.Controllable || d.RemoteControl.State != api.ControlExpired {
		t.Errorf("after 2m1s: controllable=%v state=%s", d.Controllable, d.RemoteControl.State)
	}
	if code, _, b := e.tap(sid1); code != http.StatusConflict || !strings.Contains(string(b), "offline") {
		t.Errorf("tap with a silent watcher: %d %s", code, b)
	}
	// A machine token's new request says which machine asked.
	e.recordPoll(e.tokA)
	var viaMachine api.ControlRequest
	e.must(http.StatusAccepted, "POST", "/v1/sessions/"+sid1+"/remote-control", e.tokB, nil, &viaMachine)
	if viaMachine.RequestedBy != "machine:bluebox" || viaMachine.ID == first.ID {
		t.Errorf("machine request: %+v", viaMachine)
	}
}

func TestRemoteControlLimit(t *testing.T) {
	e := newEnv(t)
	e.server.after = instantTimer
	e.recordPoll(e.tokA)
	var ids []string
	for i := range 11 {
		id := fmt.Sprintf("limit-%02d", i)
		e.register(e.tokA, api.SessionUpsert{ID: id, HerdrSession: "default", HerdrPane: fmt.Sprintf("w1:p%d", i+1)})
		ids = append(ids, id)
	}
	for _, id := range ids[:10] {
		if code, _, b := e.tap(id); code != http.StatusAccepted {
			t.Fatalf("tap %s: %d %s", id, code, b)
		}
	}
	before := e.snapshot()
	if code, _, b := e.tap(ids[10]); code != http.StatusTooManyRequests || !strings.Contains(string(b), "10 pending") {
		t.Errorf("11th tap: %d %s", code, b)
	}
	if after := e.snapshot(); after != before {
		t.Error("the refused tap changed state")
	}
	// A claim frees a slot.
	e.must(http.StatusOK, "GET", "/v1/machines/self/control?wait=1", e.tokA, nil, nil)
	if code, _, b := e.tap(ids[10]); code != http.StatusAccepted {
		t.Errorf("tap after a claim: %d %s", code, b)
	}
}

func TestControlPollWakesOnNewRequest(t *testing.T) {
	e := newEnv(t)
	ft := &fakeTimers{}
	e.server.after = ft.after
	e.controllable(sid1, "w1:p1")

	// One at a time, so timer 0 is tower's and timer 1 is bluebox's.
	tower := e.pollAsync(e.tokA, "?wait=30")
	ft.await(t, 1)
	bluebox := e.pollAsync(e.tokB, "?wait=30")
	ft.await(t, 2)
	code, req, b := e.tap(sid1)
	if code != http.StatusAccepted {
		t.Fatalf("tap: %d %s", code, b)
	}
	r := recv(t, tower)
	var claim api.ControlClaim
	if r.code != http.StatusOK || json.Unmarshal(r.body, &claim) != nil {
		t.Fatalf("tower's poll: %d %s", r.code, r.body)
	}
	if claim.Request.ID != req.ID || claim.Request.State != api.ControlClaimed || claim.Request.ClaimedAt == nil ||
		claim.Session.ID != sid1 || claim.Session.HerdrPane != "w1:p1" || claim.Session.CWD != "/home/user/proj" {
		t.Errorf("claim: %+v", claim)
	}
	if d := e.detail(sid1); d.RemoteControl.State != api.ControlClaimed {
		t.Errorf("session reads %s after the claim", d.RemoteControl.State)
	}
	// bluebox's poll was not woken by tower's request; its timer ends it.
	select {
	case r := <-bluebox:
		t.Fatalf("bluebox's poll answered early: %d %s", r.code, r.body)
	case <-time.After(50 * time.Millisecond):
	}
	for i := range 2 {
		if ft.wait(i) != 30*time.Second {
			t.Errorf("timer %d waits %s, want 30s", i, ft.wait(i))
		}
	}
	ft.fire(1)
	if r := recv(t, bluebox); r.code != http.StatusNoContent || len(r.body) != 0 {
		t.Errorf("bluebox's poll: %d %q", r.code, r.body)
	}
	// tower's watcher counts as polling now.
	if d := e.detail(sid1); !d.Controllable {
		t.Error("a poll did not refresh controllable")
	}
}

func TestControlPollWait(t *testing.T) {
	e := newEnv(t)
	ft := &fakeTimers{}
	e.server.after = ft.after
	for i, tc := range []struct {
		query string
		want  time.Duration
	}{
		{"", 30 * time.Second},
		{"?wait=7", 7 * time.Second},
		{"?wait=0", time.Second},
		{"?wait=-5", time.Second},
		{"?wait=31", 30 * time.Second},
		{"?wait=3600", 30 * time.Second},
	} {
		ch := e.pollAsync(e.tokA, tc.query)
		ft.await(t, i+1)
		if got := ft.wait(i); got != tc.want {
			t.Errorf("%q: waits %s, want %s", tc.query, got, tc.want)
		}
		ft.fire(i)
		if r := recv(t, ch); r.code != http.StatusNoContent {
			t.Errorf("%q: %d %s", tc.query, r.code, r.body)
		}
	}
	if code, b, _ := e.do("GET", "/v1/machines/self/control?wait=soon", e.tokA, nil); code != http.StatusBadRequest {
		t.Errorf("wait=soon: %d %s", code, b)
	}
}

func TestControlPollLimit(t *testing.T) {
	e := newEnv(t)
	ft := &fakeTimers{}
	e.server.after = ft.after
	a := e.pollAsync(e.tokA, "")
	b := e.pollAsync(e.tokA, "")
	ft.await(t, 2)
	if code, body, _ := e.do("GET", "/v1/machines/self/control", e.tokA, nil); code != http.StatusTooManyRequests ||
		!strings.Contains(string(body), "2 open polls") {
		t.Errorf("third poll: %d %s", code, body)
	}
	// bluebox is not limited by tower's polls.
	c := e.pollAsync(e.tokB, "")
	ft.await(t, 3)
	ft.fire(2)
	if r := recv(t, c); r.code != http.StatusNoContent {
		t.Errorf("bluebox: %d", r.code)
	}
	ft.fire(0)
	ft.fire(1)
	recv(t, a)
	recv(t, b)
	// Once they ended, tower may poll again.
	d := e.pollAsync(e.tokA, "")
	ft.await(t, 4)
	ft.fire(3)
	if r := recv(t, d); r.code != http.StatusNoContent {
		t.Errorf("poll after the others ended: %d", r.code)
	}
}

func TestControlPollEndsOnShutdown(t *testing.T) {
	e := newEnv(t)
	ft := &fakeTimers{}
	e.server.after = ft.after
	ch := e.pollAsync(e.tokA, "")
	ft.await(t, 1)
	e.server.StopPolls()
	if r := recv(t, ch); r.code != http.StatusNoContent {
		t.Errorf("open poll at shutdown: %d", r.code)
	}
	if code, _, _ := e.do("GET", "/v1/machines/self/control", e.tokA, nil); code != http.StatusNoContent {
		t.Errorf("poll after shutdown: %d", code)
	}
	e.server.StopPolls() // a second call is harmless
}

// sessionhub server sets WriteTimeout 30s; a full 30 s poll must still answer.
func TestControlPollOutlivesWriteTimeout(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewUnstartedServer(e.server.Handler())
	srv.Config.WriteTimeout = 500 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/machines/self/control?wait=1", nil)
	req.Header.Set("Authorization", "Bearer "+e.tokA)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("a poll longer than WriteTimeout failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || time.Since(start) < 900*time.Millisecond {
		t.Errorf("status %d after %s, want 204 after about 1 s", resp.StatusCode, time.Since(start))
	}
}

// HEAD matches the GET route on the mux. It must not claim a request.
func TestControlPollRefusesHead(t *testing.T) {
	e := newEnv(t)
	e.server.after = instantTimer
	e.controllable(sid1, "w1:p1")
	if code, _, b := e.tap(sid1); code != http.StatusAccepted {
		t.Fatalf("tap: %d %s", code, b)
	}
	code, _, h := e.do("HEAD", "/v1/machines/self/control?wait=1", e.tokA, nil)
	if code != http.StatusMethodNotAllowed || h.Get("Allow") != "GET" {
		t.Errorf("HEAD: status %d, Allow %q; want 405, GET", code, h.Get("Allow"))
	}
	if d := e.detail(sid1); d.RemoteControl == nil || d.RemoteControl.State != api.ControlPending {
		t.Errorf("HEAD changed the request: %+v", d.RemoteControl)
	}
	var claim api.ControlClaim
	e.must(http.StatusOK, "GET", "/v1/machines/self/control?wait=1", e.tokA, nil, &claim)
}

// claimOne taps sid and claims the request as tower's watcher.
func (e *env) claimOne(sid string) api.ControlClaim {
	e.t.Helper()
	if code, _, b := e.tap(sid); code != http.StatusAccepted && code != http.StatusOK {
		e.t.Fatalf("tap %s: %d %s", sid, code, b)
	}
	var claim api.ControlClaim
	e.must(http.StatusOK, "GET", "/v1/machines/self/control?wait=1", e.tokA, nil, &claim)
	return claim
}

func TestControlResult(t *testing.T) {
	e := newEnv(t)
	e.server.after = instantTimer
	e.controllable(sid1, "w1:p1")
	_, req, _ := e.tap(sid1)
	path := "/v1/control/" + req.ID + "/result"
	if code, b, _ := e.do("POST", path, e.tokA, api.ControlResultIn{State: "done"}); code != http.StatusConflict {
		t.Errorf("result before the claim: %d %s", code, b)
	}
	var claim api.ControlClaim
	e.must(http.StatusOK, "GET", "/v1/machines/self/control?wait=1", e.tokA, nil, &claim)

	before := e.snapshot()
	for _, tc := range []struct {
		name, path, token string
		body              any
		want              int
	}{
		{"another machine", path, e.tokB, api.ControlResultIn{State: "done"}, 409},
		{"unknown request", "/v1/control/cr_AAAAAAAAAAAAAAAAAAAAAA/result", e.tokA, api.ControlResultIn{State: "done"}, 404},
		{"malformed request ID", "/v1/control/nope/result", e.tokA, api.ControlResultIn{State: "done"}, 400},
		{"state pending", path, e.tokA, api.ControlResultIn{State: "pending"}, 400},
		{"http", path, e.tokA, api.ControlResultIn{State: "done", URL: "http://claude.ai/code/session_abc"}, 400},
		{"other host", path, e.tokA, api.ControlResultIn{State: "done", URL: "https://claude.ai.evil.example/code/session_abc"}, 400},
		{"extra path", path, e.tokA, api.ControlResultIn{State: "done", URL: "https://claude.ai/code/session_abc/x"}, 400},
		{"trailing newline", path, e.tokA, api.ControlResultIn{State: "done", URL: testLink + "\n"}, 400},
		{"URL on failed", path, e.tokA, api.ControlResultIn{State: "failed", URL: testLink}, 400},
		{"control character", path, e.tokA, api.ControlResultIn{State: "failed", Detail: "a\x1b[31mb"}, 400},
		{"detail too long", path, e.tokA, api.ControlResultIn{State: "failed", Detail: strings.Repeat("x", 201)}, 400},
		{"bad JSON", path, e.tokA, []byte("{"), 400},
	} {
		if code, b, _ := e.do("POST", tc.path, tc.token, tc.body); code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, code, b, tc.want)
		}
	}
	if after := e.snapshot(); after != before {
		t.Fatalf("refused results changed state:\nbefore %s\nafter  %s", before, after)
	}

	var done api.ControlRequest
	e.must(http.StatusOK, "POST", path, e.tokA, api.ControlResultIn{State: "done", URL: testLink, Detail: "resumed in a new herdr workspace"}, &done)
	if done.State != api.ControlDone || done.URL != testLink || done.FinishedAt == nil {
		t.Errorf("result: %+v", done)
	}
	d := e.detail(sid1)
	if d.RemoteControlURL != testLink || d.RemoteControlAt == nil || !d.RemoteControlAt.Equal(e.clock.Now()) ||
		d.RemoteControl.State != api.ControlDone || d.RemoteControl.Detail != "resumed in a new herdr workspace" {
		t.Errorf("session after the result: %+v", d.Session)
	}
	found := false
	for _, ev := range d.Events {
		found = found || (ev.Kind == api.KindRemoteControlResult && strings.Contains(string(ev.Payload), req.ID))
	}
	if !found {
		t.Error("no remote_control_result event")
	}
	if code, _, _ := e.do("POST", path, e.tokA, api.ControlResultIn{State: "failed"}); code != http.StatusConflict {
		t.Errorf("second result: %d", code)
	}
}

// A result after its request expired is refused and changes nothing.
func TestControlResultAfterExpiry(t *testing.T) {
	e := newEnv(t)
	e.server.after = instantTimer
	e.controllable(sid1, "w1:p1")
	claim := e.claimOne(sid1)
	e.clock.Advance(2 * time.Minute)
	before := e.snapshot()
	code, b, _ := e.do("POST", "/v1/control/"+claim.Request.ID+"/result", e.tokA, api.ControlResultIn{State: "done", URL: testLink})
	if code != http.StatusConflict || !strings.Contains(string(b), "expired") {
		t.Errorf("late result: %d %s", code, b)
	}
	if after := e.snapshot(); after != before {
		t.Errorf("a late result changed state")
	}
	if d := e.detail(sid1); d.RemoteControlURL != "" || d.RemoteControl.State != api.ControlExpired {
		t.Errorf("session: link %q state %s", d.RemoteControlURL, d.RemoteControl.State)
	}
}
