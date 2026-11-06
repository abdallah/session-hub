package server

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// attachStream opens a GET stream and returns its response once the
// headers are in.
func (e *env) attachStream(path, token, cookie string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest("GET", e.srv.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		req.Header.Set(api.HeaderAction, api.HeaderActionAttach)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// readChunk reads n bytes from a stream within 5 s.
func readChunk(t *testing.T, r io.Reader, n int) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		b := make([]byte, n)
		k, _ := io.ReadFull(r, b)
		got <- string(b[:k])
	}()
	select {
	case s := <-got:
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("no %d bytes within 5 s", n)
	}
	return ""
}

// readEOF requires the stream to end within 5 s.
func readEOF(t *testing.T, r io.Reader) {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(r); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("stream ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end within 5 s")
	}
}

// TestAttachRelay: a guest attaches to its session; bytes flow both ways
// through the server; the machine's result ends both streams.
func TestAttachRelay(t *testing.T) {
	e := newEnv(t)
	e.controllable(sid1, "p1")
	e.register(e.tokA, api.SessionUpsert{ID: sid3, HerdrSession: "other", HerdrPane: "p2"})
	guest := e.guestSession("guest", sid1)
	hdr := map[string]string{api.HeaderAction: api.HeaderActionAttach}

	code, body, _ := e.doHdr("POST", "/v1/sessions/"+sid1+"/attach", "", guest, hdr, nil)
	var at api.Attach
	if err := json.Unmarshal(body, &at); err != nil || code != 202 || at.SessionID != sid1 || at.Machine != "tower" {
		t.Fatalf("attach: %d %s", code, body)
	}
	if code, _, _ := e.doHdr("POST", "/v1/sessions/"+sid1+"/attach", "", guest, hdr, nil); code != 200 {
		t.Errorf("second attach while pending: %d, want 200", code)
	}
	if code, _, _ := e.doHdr("POST", "/v1/sessions/"+sid3+"/attach", "", guest, hdr, nil); code != 403 {
		t.Errorf("guest attach to another session: %d, want 403", code)
	}
	if code, _, _ := e.doHdr("POST", "/v1/sessions/"+sid1+"/remote-control", "", guest,
		map[string]string{api.HeaderAction: api.HeaderActionRemoteControl}, nil); code != 403 {
		t.Errorf("guest remote-control: %d, want 403", code)
	}

	// The watcher's poll hands out the attach.
	r := recv(t, e.pollAsync(e.tokA, "?wait=1"))
	var claim api.ControlClaim
	if err := json.Unmarshal(r.body, &claim); err != nil || r.code != 200 || claim.Request.ID != at.ID || claim.Request.Action != api.ActionAttach {
		t.Fatalf("poll: %d %s", r.code, r.body)
	}

	out := e.attachStream("/v1/attach/"+at.ID+"/output", "", guest)
	if out.StatusCode != 200 || out.Header.Get("Content-Type") != "application/octet-stream" ||
		out.Header.Get("X-Content-Type-Options") != "nosniff" || out.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("output stream: %d %v", out.StatusCode, out.Header)
	}
	if again := e.attachStream("/v1/attach/"+at.ID+"/output", "", guest); again.StatusCode != 409 {
		t.Errorf("second output reader: %d, want 409", again.StatusCode)
	}
	in := e.attachStream("/v1/machines/self/attach/"+at.ID+"/input", e.tokA, "")
	if in.StatusCode != 200 {
		t.Fatalf("input stream: %d", in.StatusCode)
	}
	if other := e.attachStream("/v1/machines/self/attach/"+at.ID+"/input", e.tokB, ""); other.StatusCode != 409 {
		t.Errorf("input stream for another machine: %d, want 409", other.StatusCode)
	}

	if code, b, _ := e.do("POST", "/v1/machines/self/attach/"+at.ID+"/output", e.tokA, []byte("\x1b[H\x1b[2Jframe")); code != 204 {
		t.Fatalf("post output: %d %s", code, b)
	}
	if got := readChunk(t, out.Body, 12); got != "\x1b[H\x1b[2Jframe" {
		t.Errorf("guest read %q", got)
	}
	if code, b, _ := e.doHdr("POST", "/v1/attach/"+at.ID+"/input", "", guest, hdr, []byte("ls\r")); code != 204 {
		t.Fatalf("post input: %d %s", code, b)
	}
	if got := readChunk(t, in.Body, 3); got != "ls\r" {
		t.Errorf("machine read %q", got)
	}
	big := make([]byte, MaxBodyBytes+1)
	if code, _, _ := e.doHdr("POST", "/v1/attach/"+at.ID+"/input", "", guest, hdr, big); code != 413 {
		t.Errorf("input over 64 KiB: %d, want 413", code)
	}
	if code, _, _ := e.do("POST", "/v1/machines/self/attach/"+at.ID+"/output", e.tokA, make([]byte, maxAttachOutput+1)); code != 413 {
		t.Errorf("output over 1 MiB: %d, want 413", code)
	}

	// The result ends both streams; the relay routes then answer 404.
	if code, b, _ := e.do("POST", "/v1/control/"+at.ID+"/result", e.tokA,
		api.ControlResultIn{State: api.ControlDone, Detail: "guest left"}); code != 200 {
		t.Fatalf("result: %d %s", code, b)
	}
	readEOF(t, out.Body)
	readEOF(t, in.Body)
	if code, _, _ := e.doHdr("POST", "/v1/attach/"+at.ID+"/input", "", guest, hdr, []byte("x")); code != 404 {
		t.Errorf("input after the end: %d, want 404", code)
	}
}

// TestAttachSweep: an attach ends when its guest has been gone 60 s, when
// no machine connects within ControlTTL, and after 30 min without traffic.
func TestAttachSweep(t *testing.T) {
	h := newAttachHub()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return t0 }
	h.open("gone", sid1, "tower")
	h.open("idle", sid1, "tower")
	h.open("nomachine", sid1, "tower")
	for _, id := range []string{"idle", "nomachine"} {
		h.take(h.get(id), h.get(id).out) // the guest reads
	}
	h.touch(h.get("idle"), true)
	h.sweep(t0.Add(attachGuestGone - time.Second))
	if h.get("gone") == nil {
		t.Fatal("ended before attachGuestGone")
	}
	gone := h.get("gone")
	h.sweep(t0.Add(attachGuestGone))
	select {
	case <-gone.done:
	default:
		t.Error("guest gone 60 s: attach still open")
	}
	h.sweep(t0.Add(2 * time.Minute))
	if h.get("nomachine") != nil || h.get("idle") == nil {
		t.Errorf("after ControlTTL: nomachine %v, idle %v", h.get("nomachine"), h.get("idle"))
	}
	if h.sweep(t0.Add(attachIdle)) || h.get("idle") != nil {
		t.Error("30 min without traffic: attach still open")
	}
}
