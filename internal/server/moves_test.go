package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

func mustURL(t *testing.T, p string) *url.URL {
	t.Helper()
	u, err := url.Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// moveSetup gives both machines a key over the API, records a poll from
// both, and registers id on tower, idle in a herdr pane with a GitHub remote.
func (e *env) moveSetup(id string) (towerKey, blueboxKey string) {
	e.t.Helper()
	towerKey, blueboxKey = testMoveKey(e.t), testMoveKey(e.t)
	if code, b, _ := e.do("PUT", "/v1/machines/self/move-key", e.tokA, api.MoveKeyIn{PublicKey: towerKey}); code != 204 {
		e.t.Fatalf("tower key: %d %s", code, b)
	}
	if code, b, _ := e.do("PUT", "/v1/machines/self/move-key", e.tokB, api.MoveKeyIn{PublicKey: blueboxKey}); code != 204 {
		e.t.Fatalf("bluebox key: %d %s", code, b)
	}
	e.register(e.tokA, api.SessionUpsert{ID: id, HerdrSession: "default", HerdrPane: "w1:p1", CWD: "/home/user/proj",
		AgentState: "idle", GitRepo: "git@github.com:o/r.git", GitBranch: "main"})
	e.recordPoll(e.tokA)
	e.recordPoll(e.tokB)
	return towerKey, blueboxKey
}

// claim polls once as token and decodes the claim; it fails unless one
// came.
func (e *env) claim(token string) api.ControlClaim {
	e.t.Helper()
	e.server.after = instantTimer
	var c api.ControlClaim
	e.must(200, "GET", "/v1/machines/self/control?wait=1", token, nil, &c)
	return c
}

func (e *env) startMove(id, target string) api.Move {
	e.t.Helper()
	code, b, _ := e.doHdr("POST", "/v1/sessions/"+id+"/move", "", e.web, map[string]string{api.HeaderAction: api.HeaderActionMove},
		api.MoveIn{Target: target})
	if code != http.StatusAccepted {
		e.t.Fatalf("POST move: %d %s", code, b)
	}
	var mv api.Move
	json.Unmarshal(b, &mv)
	return mv
}

func TestMoveRoutesFlow(t *testing.T) {
	e := newEnv(t)
	towerKey, blueboxKey := e.moveSetup(sid1)
	var ms []api.Machine
	e.must(200, "GET", "/v1/machines", e.tokA, nil, &ms)
	if len(ms) != 2 || len(ms[0].MoveKey) != 16 || !ms[0].MoveReady || len(ms[1].MoveKey) != 16 {
		t.Fatalf("machines: %+v", ms)
	}

	mv := e.startMove(sid1, "bluebox")
	if mv.State != api.MoveRequested || mv.By != "web:phone" || mv.Source != "tower" {
		t.Fatalf("move %+v", mv)
	}
	if code, b, _ := e.doHdr("POST", "/v1/sessions/"+sid1+"/move", "", e.web, map[string]string{api.HeaderAction: api.HeaderActionMove},
		api.MoveIn{Target: "bluebox"}); code != http.StatusConflict || !strings.Contains(string(b), "is requested") {
		t.Errorf("second move: %d %s", code, b)
	}

	out := e.claim(e.tokA)
	if out.Request.Action != api.ActionMoveOut || out.Move == nil || out.Move.PeerKey != blueboxKey || out.Move.State != api.MovePacking {
		t.Fatalf("move-out claim %+v %+v", out.Request, out.Move)
	}
	sealed := bytes.Repeat([]byte{0xA5}, 4096)
	var up api.Move
	e.must(200, "PUT", "/v1/moves/"+mv.ID+"/bundle", e.tokA, sealed, &up)
	if up.State != api.MoveUploaded || up.BundleSize != 4096 {
		t.Fatalf("upload: %+v", up)
	}
	path := filepath.Join(e.moveDir, mv.ID+".sealed")
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("bundle file: %v %v", fi, err)
	}
	if di, _ := os.Stat(e.moveDir); di.Mode().Perm() != 0o700 {
		t.Errorf("bundle dir mode %v", di.Mode().Perm())
	}

	in := e.claim(e.tokB)
	if in.Request.Action != api.ActionMoveIn || in.Move.PeerKey != towerKey || in.Move.State != api.MoveUnpacking {
		t.Fatalf("move-in claim %+v %+v", in.Request, in.Move)
	}
	code, got, h := e.do("GET", "/v1/moves/"+mv.ID+"/bundle", e.tokB, nil)
	if code != 200 || !bytes.Equal(got, sealed) || h.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("download: %d, %d bytes, %q", code, len(got), h.Get("Content-Type"))
	}
	var done api.Move
	e.must(200, "POST", "/v1/moves/"+mv.ID+"/result", e.tokB, api.MoveResultIn{State: api.MoveDone}, &done)
	if done.State != api.MoveDone {
		t.Fatalf("result: %+v", done)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("bundle kept after done: %v", err)
	}
	if d := e.detail(sid1); d.Machine != "bluebox" || d.HerdrPane != "" || d.Move == nil || d.Move.State != api.MoveDone {
		t.Errorf("session after the move: %+v", d.Session)
	}
	var read api.Move
	e.must(200, "GET", "/v1/moves/"+mv.ID, e.tokA, nil, &read)
	if read.State != api.MoveDone {
		t.Errorf("GET move: %+v", read)
	}
	if fin := e.claim(e.tokA); fin.Request.Action != api.ActionMoveOut || fin.Move.State != api.MoveDone {
		t.Errorf("finish: %+v %+v", fin.Request, fin.Move)
	}
	if code, _, _ := e.do("GET", "/v1/moves/nope", e.tokA, nil); code != 400 {
		t.Errorf("bad move id: %d", code)
	}
}

func TestMoveBundleLimits(t *testing.T) {
	e := newEnv(t)
	e.moveSetup(sid1)
	old := maxBundleBytes
	maxBundleBytes = 1024
	t.Cleanup(func() { maxBundleBytes = old })
	mv := e.startMove(sid1, "bluebox")
	e.claim(e.tokA)
	for _, c := range []struct {
		body []byte
		want int
	}{{nil, 400}, {bytes.Repeat([]byte("x"), 1025), 413}} {
		code, b, _ := e.do("PUT", "/v1/moves/"+mv.ID+"/bundle", e.tokA, c.body)
		if code != c.want {
			t.Errorf("%d bytes: %d %s, want %d", len(c.body), code, b, c.want)
		}
		if files, _ := os.ReadDir(e.moveDir); len(files) != 0 {
			t.Errorf("%d bytes left files: %v", len(c.body), files)
		}
	}
	if code, b, _ := e.do("PUT", "/v1/moves/"+mv.ID+"/bundle", e.tokA, bytes.Repeat([]byte("x"), 1024)); code != 200 {
		t.Errorf("exactly the limit: %d %s", code, b)
	}
	// A body over 64 KiB elsewhere is still refused.
	if code, _, _ := e.do("PUT", "/v1/machines/self/move-key", e.tokA, bytes.Repeat([]byte("x"), MaxBodyBytes+1)); code != 413 {
		t.Errorf("big body on another route: %d", code)
	}
	if isBundleUpload(&http.Request{Method: "PUT", URL: mustURL(t, "/v1/moves/x/bundle/more")}) {
		t.Error("a longer path counts as a bundle upload")
	}
}

func TestMoveBundleDeletedOnEnd(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.moveSetup(sid1)
	mv := e.startMove(sid1, "bluebox")
	e.claim(e.tokA)
	e.must(200, "PUT", "/v1/moves/"+mv.ID+"/bundle", e.tokA, []byte("sealed"), nil)
	e.claim(e.tokB)
	var failed api.Move
	e.must(200, "POST", "/v1/moves/"+mv.ID+"/result", e.tokB, api.MoveResultIn{State: api.MoveFailed, Detail: "find clone: none"}, &failed)
	if _, err := os.Stat(filepath.Join(e.moveDir, mv.ID+".sealed")); !os.IsNotExist(err) {
		t.Errorf("bundle kept after failed: %v", err)
	}
	if fin := e.claim(e.tokA); fin.Move.State != api.MoveFailed || fin.Move.Detail != "find clone: none" {
		t.Errorf("finish: %+v", fin.Move)
	}

	// A move that times out while uploaded loses its bundle at the next
	// sweep, and its source gets a finish.
	e.recordPoll(e.tokA)
	e.recordPoll(e.tokB)
	mv2 := e.startMove(sid1, "bluebox")
	e.claim(e.tokA)
	e.must(200, "PUT", "/v1/moves/"+mv2.ID+"/bundle", e.tokA, []byte("sealed"), nil)
	e.clock.Advance(store.MoveStepTTL)
	var read api.Move
	e.must(200, "GET", "/v1/moves/"+mv2.ID, e.tokB, nil, &read)
	if read.State != api.MoveFailed || read.Detail != "uploaded: timed out" {
		t.Errorf("after the timeout: %+v", read)
	}
	if _, err := os.Stat(filepath.Join(e.moveDir, mv2.ID+".sealed")); !os.IsNotExist(err) {
		t.Errorf("bundle kept after the timeout: %v", err)
	}

	// Stray files: an unknown move's bundle goes; a temp file goes once it
	// is an hour old.
	stray := filepath.Join(e.moveDir, "mv_AAAAAAAAAAAAAAAAAAAAAA.sealed")
	oldTmp := filepath.Join(e.moveDir, "mv_x.123.tmp")
	newTmp := filepath.Join(e.moveDir, "mv_y.456.tmp")
	for _, p := range []string{stray, oldTmp, newTmp} {
		os.WriteFile(p, []byte("x"), 0o600)
	}
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(oldTmp, past, past)
	e.server.sweepMoves(ctx)
	for p, want := range map[string]bool{stray: false, oldTmp: false, newTmp: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s: exists %v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}

func TestMoveWakesPolls(t *testing.T) {
	e := newEnv(t)
	e.moveSetup(sid1)
	timers := &fakeTimers{}
	e.server.after = timers.after
	src := e.pollAsync(e.tokA, "?wait=30")
	timers.await(t, 1)
	mv := e.startMove(sid1, "bluebox")
	r := recv(t, src)
	var c api.ControlClaim
	if json.Unmarshal(r.body, &c); r.code != 200 || c.Move == nil || c.Move.ID != mv.ID {
		t.Fatalf("source poll: %d %s", r.code, r.body)
	}
	dst := e.pollAsync(e.tokB, "?wait=30")
	timers.await(t, 2)
	e.must(200, "PUT", "/v1/moves/"+mv.ID+"/bundle", e.tokA, []byte("sealed"), nil)
	r = recv(t, dst)
	if json.Unmarshal(r.body, &c); r.code != 200 || c.Request.Action != api.ActionMoveIn {
		t.Fatalf("target poll: %d %s", r.code, r.body)
	}
}

// cutBody yields some bytes, then fails like a dropped connection.
type cutBody struct{ sent bool }

func (c *cutBody) Read(p []byte) (int, error) {
	if c.sent {
		return 0, errors.New("read tcp 10.1.2.3:8080->10.9.9.9:5555: connection reset")
	}
	c.sent = true
	return copy(p, "partial"), nil
}

func TestMoveBundleReadError(t *testing.T) {
	e := newEnv(t)
	e.moveSetup(sid1)
	mv := e.startMove(sid1, "bluebox")
	e.claim(e.tokA)
	req := httptest.NewRequest("PUT", "/v1/moves/"+mv.ID+"/bundle", io.NopCloser(&cutBody{}))
	req.SetPathValue("id", mv.ID)
	rec := httptest.NewRecorder()
	e.server.putMoveBundle(rec, req, principal{machine: e.machine(e.tokA)})
	body := rec.Body.String()
	if rec.Code != 400 || !strings.Contains(body, "the upload was cut off") || strings.Contains(body, "10.9.9.9") || strings.Contains(body, e.moveDir) {
		t.Errorf("cut upload: %d %s", rec.Code, body)
	}
	if files, _ := os.ReadDir(e.moveDir); len(files) != 0 {
		t.Errorf("files left: %v", files)
	}
}

func TestMoveBundleSecondUploadRefused(t *testing.T) {
	e := newEnv(t)
	e.moveSetup(sid1)
	mv := e.startMove(sid1, "bluebox")
	e.claim(e.tokA)
	// A concurrent upload already put its bundle in place.
	if err := os.MkdirAll(e.moveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(e.moveDir, mv.ID+".sealed")
	os.WriteFile(first, []byte("first"), 0o600)
	if code, b, _ := e.do("PUT", "/v1/moves/"+mv.ID+"/bundle", e.tokA, []byte("second")); code != http.StatusConflict {
		t.Errorf("second upload: %d %s", code, b)
	}
	if got, _ := os.ReadFile(first); string(got) != "first" {
		t.Errorf("first bundle is now %q", got)
	}
	files, _ := os.ReadDir(e.moveDir)
	if len(files) != 1 {
		t.Errorf("files: %v", files)
	}
	// The directory that already existed with mode 0755 is corrected.
	if di, _ := os.Stat(e.moveDir); di.Mode().Perm() != 0o700 {
		t.Errorf("bundle dir mode %v", di.Mode().Perm())
	}
}

func TestMoveResultAndExpiryWakeSource(t *testing.T) {
	e := newEnv(t)
	e.moveSetup(sid1)
	timers := &fakeTimers{}
	e.server.after = timers.after
	mv := e.startMove(sid1, "bluebox")
	e.claim(e.tokA)
	e.server.after = timers.after
	e.must(200, "PUT", "/v1/moves/"+mv.ID+"/bundle", e.tokA, []byte("sealed"), nil)
	e.claim(e.tokB)
	e.server.after = timers.after
	src := e.pollAsync(e.tokA, "?wait=30")
	timers.await(t, 1)
	e.must(200, "POST", "/v1/moves/"+mv.ID+"/result", e.tokB, api.MoveResultIn{State: api.MoveFailed, Detail: "open: x"}, nil)
	var c api.ControlClaim
	r := recv(t, src)
	json.Unmarshal(r.body, &c)
	if r.code != 200 || c.Move == nil || c.Move.State != api.MoveFailed {
		t.Fatalf("result did not wake the source: %d %s", r.code, r.body)
	}

	e.recordPoll(e.tokA)
	e.recordPoll(e.tokB)
	mv2 := e.startMove(sid1, "bluebox")
	e.claim(e.tokA)
	e.server.after = timers.after
	src = e.pollAsync(e.tokA, "?wait=30")
	timers.await(t, 2)
	e.clock.Advance(store.MoveStepTTL)
	e.server.sweepMoves(context.Background())
	r = recv(t, src)
	json.Unmarshal(r.body, &c)
	if r.code != 200 || c.Move == nil || c.Move.ID != mv2.ID || c.Move.State != api.MoveFailed {
		t.Fatalf("expiry did not wake the source: %d %s", r.code, r.body)
	}
}

// Remote Control is refused while the session moves: on the source it
// would resume the session that the target is about to run.
func TestRemoteControlRefusedDuringMove(t *testing.T) {
	e := newEnv(t)
	e.moveSetup(sid1)
	e.startMove(sid1, "bluebox")
	code, _, b := e.tap(sid1)
	if code != http.StatusConflict || !strings.Contains(string(b), "a move is open for this session") {
		t.Errorf("Remote Control during a move: %d %s", code, b)
	}
}
