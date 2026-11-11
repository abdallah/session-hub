package resume

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/claudetrust"
	"github.com/abdallah/session-hub/internal/rex"
)

// TestRexLive starts a real Claude with Remote Control in a new session on
// the local Rex server, asks for the link again, and ends it with /exit. It
// runs only with SESSIONHUB_LIVE_REX=1, in $SESSIONHUB_LIVE_DIR (a directory
// under home that Claude trusts).
func TestRexLive(t *testing.T) {
	dir := os.Getenv("SESSIONHUB_LIVE_DIR")
	if os.Getenv("SESSIONHUB_LIVE_REX") != "1" || dir == "" {
		t.Skip("set SESSIONHUB_LIVE_REX=1 and SESSIONHUB_LIVE_DIR to run against the local Rex server")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	o := ControlOptions{PollFor: 30 * time.Second}
	r := StartNew(ctx, filepath.Join(t.TempDir(), "none.sock"), home, claudetrust.Path(os.Getenv, home),
		api.StartRequest{ID: "st_live", Dir: dir}, o)
	if r.RexSession != "" {
		t.Cleanup(func() { rex.Kill(context.Background(), r.RexSession) })
	}
	if r.Outcome != OutcomeStarted || !rcURLExact.MatchString(r.URL) {
		t.Fatalf("start: %s url %q err %v", r.Outcome, r.URL, r.Err)
	}
	t.Logf("started in %s %s: %s", r.RexSession, r.RexBlock, r.URL)

	// Remote Control is on already: Claude shows its dialog, and Esc closes it.
	s := api.Session{ID: "00000000-0000-4000-8000-000000000000", RexSession: r.RexSession, RexBlock: r.RexBlock}
	c := Control(ctx, s, o)
	if c.Outcome != OutcomeAlreadyOn || c.URL != r.URL || c.DialogLeftOpen != "" {
		t.Errorf("control: %s url %q dialog %q err %v", c.Outcome, c.URL, c.DialogLeftOpen, c.Err)
	}

	time.Sleep(time.Second)
	if err := rex.Submit(ctx, r.RexSession, r.RexBlock, "/exit"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		running, err := rex.ClaudeRuns(ctx, r.RexSession, r.RexBlock)
		if errors.Is(err, rex.ErrGone) || err == nil && !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Claude still runs after /exit: %v %v", running, err)
		}
	}
}
