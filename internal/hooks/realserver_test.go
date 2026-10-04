package hooks

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/server"
	"github.com/abdallah/session-hub/internal/store"
)

// TestMultilinePromptStoredByRealServer runs the hooks against the real
// server handler and a real store. A fake server accepts whatever the hook
// sends; the real one rejects control characters, which once dropped every
// multi-line prompt.
func TestMultilinePromptStoredByRealServer(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "sessionhub.db"), store.Options{StaleAfter: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tok, _, err := st.AddMachine(context.Background(), "tower", "tower.example.com", "tower.example.com")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, "https://sessionhub.example.test", log.New(io.Discard, "", 0)).Handler())
	t.Cleanup(srv.Close)

	fx := newFixture(t, srv.URL)
	fx.h.newClient = func() (*client.Client, error) {
		return client.New(client.Config{ServerURL: srv.URL, Token: tok})
	}
	stdin, _ := json.Marshal(map[string]string{
		"session_id": sessionID,
		"cwd":        "/home/user/proj",
		"prompt":     "Fix the bug\n\tin  parser.go\r\n\nthen run tests",
	})
	if err := fx.call(t, "prompt", string(stdin)); err != nil {
		t.Fatal(err)
	}
	if q := queued(t, fx.state); len(q) != 0 {
		t.Fatalf("items left queued after the server rejected them: %+v", q)
	}
	d, err := st.SessionDetail(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("session was not stored: %v", err)
	}
	const want = "Fix the bug in parser.go then run tests"
	if d.Title != want || d.TitleSource != "prompt" {
		t.Errorf("title = %q (%s), want %q (prompt)", d.Title, d.TitleSource, want)
	}
	if d.AgentState != "working" {
		t.Errorf("agent_state = %q, want working", d.AgentState)
	}
}

func TestCleanText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a\nb\tc", "a b c"},
		{"  a \r\n\n b\u0085c\x7fd ", "a b c d"},
		{"", ""},
	} {
		if got := cleanText(c.in, 200); got != c.want {
			t.Errorf("cleanText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := cleanText("abcd efgh", 5); got != "abcd" {
		t.Errorf("cut inside a word gave %q, want trailing space trimmed", got)
	}
}
