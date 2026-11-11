package rex_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/rex"
	"github.com/abdallah/session-hub/internal/rex/rextest"
)

func TestBlockCalls(t *testing.T) {
	state := rextest.Fake(t)
	ctx := context.Background()
	rextest.Set(t, state, "frame", `"line one\nline two"`)

	if got, err := rex.Screen(ctx, "s", "b"); err != nil || got != "line one\nline two" {
		t.Errorf("Screen = %q, %v", got, err)
	}
	if ok, err := rex.ClaudeRuns(ctx, "s", "b"); err != nil || !ok {
		t.Errorf("ClaudeRuns = %v, %v; want true", ok, err)
	}
	// One line goes in as typed text, several as one paste; Enter follows
	// in its own write.
	if err := rex.Submit(ctx, "s", "b", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := rex.Submit(ctx, "s", "b", "a\nb"); err != nil {
		t.Fatal(err)
	}
	want := `{"data":"hello"}` + "\n" + `{"data":"\r"}` + "\n" + `{"data":"\u001b[200~a\nb\u001b[201~"}` + "\n" + `{"data":"\r"}` + "\n"
	if got := rextest.Read(t, state, "writes"); got != want {
		t.Errorf("writes = %q, want %q", got, want)
	}

	rextest.Set(t, state, "noclaude", "")
	if ok, err := rex.ClaudeRuns(ctx, "s", "b"); err != nil || ok {
		t.Errorf("ClaudeRuns at a shell = %v, %v; want false", ok, err)
	}
	rextest.Set(t, state, "gone", "")
	if _, err := rex.ClaudeRuns(ctx, "s", "b"); !errors.Is(err, rex.ErrGone) {
		t.Errorf("ClaudeRuns on a missing block = %v, want ErrGone", err)
	}
}

func TestReap(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, session string
		gone, killed  bool
	}{
		{"its own session, empty", "session:new", true, true},
		{"its own session, a block left", "session:new", false, false},
		{"someone else's empty session", "session:1", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := rextest.Fake(t)
			if c.gone {
				rextest.Set(t, state, "gone", "")
			}
			rex.Reap(ctx, c.session)
			if got := rextest.Called(t, state, "session kill -- "+c.session); got != c.killed {
				t.Errorf("killed = %v, want %v; calls %q", got, c.killed, rextest.Read(t, state, "calls"))
			}
		})
	}
}

func TestStartFocusKill(t *testing.T) {
	state := rextest.Fake(t)
	ctx := context.Background()

	s, b, err := rex.New(ctx, "label", "/tmp", []string{"claude", "--resume", "id"})
	if err != nil || s != "session:new" || b != "block:new" {
		t.Fatalf("New = %q, %q, %v", s, b, err)
	}
	if !rextest.Called(t, state, "new label --json -c /tmp -- claude --resume id") {
		t.Errorf("calls = %q", rextest.Read(t, state, "calls"))
	}
	if b, err := rex.Run(ctx, "/tmp", []string{"claude"}); err != nil || b != "block:run" {
		t.Errorf("Run = %q, %v", b, err)
	}
	if err := rex.Focus(ctx, s, b); err != nil {
		t.Errorf("Focus = %v", err)
	}
	if err := rex.Kill(ctx, s); err != nil {
		t.Errorf("Kill = %v", err)
	}
	if !rextest.Called(t, state, "session kill -- session:new") {
		t.Errorf("calls = %q", rextest.Read(t, state, "calls"))
	}

	rextest.Set(t, state, "nostart", "")
	if _, _, err := rex.New(ctx, "label", "/tmp", []string{"claude"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("New that fails = %v, want rex's message", err)
	}
}
