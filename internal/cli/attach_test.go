package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
)

// loginServer answers POST /login/{code} with status, setting the session
// cookie on 303, and GET /v1/sessions with one session for that cookie.
func loginServer(t *testing.T, status int, origin *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/login/"):
			*origin = r.Header.Get("Origin")
			if status == http.StatusSeeOther {
				http.SetCookie(w, &http.Cookie{Name: client.SessionCookie, Value: "tok", Path: "/", MaxAge: 3600})
				http.Redirect(w, r, "/", status)
				return
			}
			w.WriteHeader(status)
		case r.URL.Path == "/v1/sessions":
			if ck, err := r.Cookie(client.SessionCookie); err != nil || ck.Value != "tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode([]api.Session{{ID: "abcdef0123456789"}})
		case r.URL.Path == "/":
			t.Error("the redirect was followed")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func noOwner() (*client.Client, error) { return nil, errors.New("owner called") }

func TestAttachRedeemsLoginURL(t *testing.T) {
	var origin string
	srv := loginServer(t, http.StatusSeeOther, &origin)
	path := filepath.Join(t.TempDir(), "state", "guest.json")
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	c, id, err := resolveAttach(context.Background(), srv.URL+"/login/CODE", path, now, &out, noOwner)
	if err != nil {
		t.Fatal(err)
	}
	if origin != srv.URL {
		t.Errorf("Origin %q, want %q", origin, srv.URL)
	}
	if _, ok := c.(*client.Client); !ok || id != "abcdef0123456789" {
		t.Errorf("client %T, id %q", c, id)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	if want := "Signed in as a guest for session abcdef01 on " + host + ".\n"; out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	var g guestLogin
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	want := guestLogin{ServerURL: srv.URL, Token: "tok", Session: "abcdef0123456789", ExpiresAt: now.Add(time.Hour)}
	if g != want {
		t.Errorf("saved %+v, want %+v", g, want)
	}

	// No argument reuses the saved sign-in; the server decides when it has
	// expired, because it slides the expiry on every visit.
	_, id, err = resolveAttach(context.Background(), "", path, now.Add(2*time.Hour), io.Discard, noOwner)
	if err != nil || id != "abcdef0123456789" {
		t.Errorf("reuse: id %q, err %v", id, err)
	}
	// A plain http link to a remote host is refused.
	_, _, err = resolveAttach(context.Background(), "http://hub.example.com/login/abcdefghijklmnopqrstuvwxyz", path, now, io.Discard, noOwner)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("http link: %v", err)
	}
}

func TestAttachLoginGone(t *testing.T) {
	var origin string
	srv := loginServer(t, http.StatusGone, &origin)
	path := filepath.Join(t.TempDir(), "guest.json")
	_, _, err := resolveAttach(context.Background(), srv.URL+"/login/CODE", path, time.Now(), io.Discard, noOwner)
	if err == nil || !strings.Contains(err.Error(), "used or expired") {
		t.Errorf("410: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("guest.json written after a 410: %v", err)
	}
}

func TestAttachDispatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guest.json")
	_, _, err := resolveAttach(context.Background(), "", path, time.Now(), io.Discard, noOwner)
	if err == nil || !strings.Contains(err.Error(), "run sessionhub attach <login-url> first") {
		t.Errorf("no guest.json: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/abc" || r.Header.Get("Authorization") != "Bearer mt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(api.SessionDetail{Session: api.Session{ID: "abc123"}})
	}))
	defer srv.Close()
	owner := func() (*client.Client, error) { return client.New(client.Config{ServerURL: srv.URL, Token: "mt"}) }
	_, id, err := resolveAttach(context.Background(), "abc", path, time.Now(), io.Discard, owner)
	if err != nil || id != "abc123" {
		t.Errorf("prefix: id %q, err %v", id, err)
	}
}

// fakeAttach serves output from a pipe and records input.
type fakeAttach struct {
	out *io.PipeReader
	mu  sync.Mutex
	in  [][]byte
}

func (f *fakeAttach) CreateAttach(context.Context, string) (api.Attach, error) {
	return api.Attach{ID: "a1"}, nil
}

func (f *fakeAttach) AttachOutput(context.Context, string) (io.ReadCloser, error) { return f.out, nil }

func (f *fakeAttach) SendAttachInput(_ context.Context, _ string, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.in = append(f.in, append([]byte(nil), b...))
	return nil
}

func TestMirrorDetach(t *testing.T) {
	outR, outW := io.Pipe()
	f := &fakeAttach{out: outR}
	inR, inW := io.Pipe()
	var screen bytes.Buffer
	res := make(chan error, 1)
	var detached bool
	go func() {
		var err error
		detached, err = mirrorAttach(context.Background(), f, "a1", inR, &screen)
		res <- err
	}()
	outW.Write([]byte("frame"))
	inW.Write([]byte("ls\r"))
	inW.Write([]byte("q\x1d"))
	if err := <-res; err != nil || !detached {
		t.Fatalf("detached %v, err %v", detached, err)
	}
	if screen.String() != "frame" {
		t.Errorf("screen %q", screen.String())
	}
	if len(f.in) != 2 || string(f.in[0]) != "ls\r" || string(f.in[1]) != "q" {
		t.Errorf("input %q", f.in)
	}
}

func TestMirrorSessionEnd(t *testing.T) {
	outR, outW := io.Pipe()
	f := &fakeAttach{out: outR}
	inR, _ := io.Pipe()
	var screen bytes.Buffer
	go func() { outW.Write([]byte("bye")); outW.Close() }()
	detached, err := mirrorAttach(context.Background(), f, "a1", inR, &screen)
	if err != nil || detached || screen.String() != "bye" {
		t.Errorf("detached %v, err %v, screen %q", detached, err, screen.String())
	}
}
