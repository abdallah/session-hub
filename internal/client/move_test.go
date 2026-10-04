package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestMoveCallsRequestShape(t *testing.T) {
	ctx := context.Background()
	c, log := server(t, 200, `{}`)
	for _, tc := range []struct {
		name, method, path, body string
		call                     func() error
	}{
		{"create", "POST", "/v1/sessions/s1/move", `{"target":"tower"}`,
			func() error { _, err := c.CreateMove(ctx, "s1", "tower"); return err }},
		{"get", "GET", "/v1/moves/mv_x", "", func() error { _, err := c.GetMove(ctx, "mv_x"); return err }},
		{"key", "PUT", "/v1/machines/self/move-key", `{"public_key":"k"}`, func() error { return c.PutMoveKey(ctx, "k") }},
		{"result", "POST", "/v1/moves/mv_x/result", `{"state":"failed","detail":"check pane: busy"}`,
			func() error {
				return c.PostMoveResult(ctx, "mv_x", api.MoveResultIn{State: api.MoveFailed, Detail: "check pane: busy"})
			}},
	} {
		*log = nil
		if err := tc.call(); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		g := (*log)[0]
		if g.Method != tc.method || g.Path != tc.path || string(g.Body) != tc.body || g.Auth != "Bearer hub_m_tok" {
			t.Errorf("%s: got %s %s body %s auth %q", tc.name, g.Method, g.Path, g.Body, g.Auth)
		}
	}
}

// bundleServer answers bundle requests and records the last one.
type bundleServer struct {
	*httptest.Server
	got           seen
	contentLength int64
	status        int
	body          []byte
	delay         time.Duration
}

func newBundleServer(t *testing.T) *bundleServer {
	b := &bundleServer{status: 200}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		b.got = seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), data}
		b.contentLength = r.ContentLength
		time.Sleep(b.delay)
		w.WriteHeader(b.status)
		w.Write(b.body)
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *bundleServer) client(t *testing.T) *Client {
	c, err := New(Config{ServerURL: b.URL, Token: "hub_m_tok"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPutMoveBundle(t *testing.T) {
	ctx := context.Background()
	b := newBundleServer(t)
	b.body = []byte(`{"id":"mv_x","state":"uploaded"}`)
	c := b.client(t)
	// The 2 s default limit does not apply to a bundle.
	c.SetTimeout(10 * time.Millisecond)
	b.delay = 50 * time.Millisecond
	data := bytes.Repeat([]byte{0x5A}, 300<<10)
	if err := c.PutMoveBundle(ctx, "mv_x", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if b.got.Method != "PUT" || b.got.Path != "/v1/moves/mv_x/bundle" || b.got.CT != "application/octet-stream" ||
		b.got.Auth != "Bearer hub_m_tok" || !bytes.Equal(b.got.Body, data) || b.contentLength != int64(len(data)) {
		t.Errorf("upload: %s %s %q %q, %d bytes, length %d", b.got.Method, b.got.Path, b.got.CT, b.got.Auth, len(b.got.Body), b.contentLength)
	}
	b.delay = 0
	b.status, b.body = 409, []byte(`{"error":"move mv_x is uploaded, not this machine's step"}`)
	var se *StatusError
	if err := c.PutMoveBundle(ctx, "mv_x", bytes.NewReader(data), int64(len(data))); !errors.As(err, &se) || se.Status != 409 ||
		se.Message != "move mv_x is uploaded, not this machine's step" {
		t.Errorf("409: %v", err)
	}
}

func TestGetMoveBundle(t *testing.T) {
	ctx := context.Background()
	b := newBundleServer(t)
	b.body = bytes.Repeat([]byte{0xC3}, 200<<10)
	c := b.client(t)
	c.SetTimeout(10 * time.Millisecond)
	b.delay = 50 * time.Millisecond
	var out bytes.Buffer
	n, err := c.GetMoveBundle(ctx, "mv_x", &out)
	if err != nil || n != int64(len(b.body)) || !bytes.Equal(out.Bytes(), b.body) || b.got.Path != "/v1/moves/mv_x/bundle" || b.got.Method != "GET" {
		t.Fatalf("download: %d bytes, %v; %s %s", n, err, b.got.Method, b.got.Path)
	}
	b.delay = 0
	b.status, b.body = 410, []byte(`{"error":"the bundle is gone"}`)
	var se *StatusError
	if _, err := c.GetMoveBundle(ctx, "mv_x", io.Discard); !errors.As(err, &se) || se.Status != 410 {
		t.Errorf("410: %v", err)
	}
	old := maxBundleDownload
	maxBundleDownload = 1024
	t.Cleanup(func() { maxBundleDownload = old })
	b.status, b.body = 200, bytes.Repeat([]byte("x"), 1025)
	if _, err := c.GetMoveBundle(ctx, "mv_x", io.Discard); err == nil {
		t.Error("an oversized bundle was accepted")
	}
}

func TestMoveRootsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("SESSIONHUB_CONFIG", path)
	if err := os.WriteFile(path, []byte("server_url = \"https://sessionhub.example.test\"\nmove_roots = [\"~/src\", \"/srv/code\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil || !slices.Equal(cfg.MoveRoots, []string{"~/src", "/srv/code"}) {
		t.Errorf("move_roots: %q %v", cfg.MoveRoots, err)
	}
}
