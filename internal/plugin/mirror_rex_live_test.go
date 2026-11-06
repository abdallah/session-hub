package plugin

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// TestRexMirrorLive mirrors a throwaway block on the local Rex server. It
// runs only with SESSIONHUB_LIVE_REX=1.
func TestRexMirrorLive(t *testing.T) {
	if os.Getenv("SESSIONHUB_LIVE_REX") != "1" {
		t.Skip("set SESSIONHUB_LIVE_REX=1 to run against the local Rex server")
	}
	raw, err := exec.Command("rex", "new", "sessionhub-test", "-c", "/tmp", "--json").Output()
	if err != nil {
		t.Fatalf("rex new: %v", err)
	}
	var created struct {
		SessionID      string `json:"session_id"`
		InitialWindows []struct {
			BlockIDs []string `json:"block_ids"`
		} `json:"initial_windows"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Command("rex", "kill", created.SessionID).Run() })
	s := api.Session{ID: "live", RexSession: created.SessionID, RexBlock: created.InitialWindows[0].BlockIDs[0]}
	out := &safeBuffer{}
	in, guest := io.Pipe()
	defer guest.Close()
	done := make(chan error, 1)
	go func() { done <- newRexMirror().(rexMirror).Run(context.Background(), s, out, in) }()
	time.Sleep(time.Second) // the shell prompt
	if _, err := io.WriteString(guest, "echo MIRROR-$((6*7))\n"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "typed command echoed", func() bool { return strings.Contains(out.String(), "MIRROR-42") })
	exec.Command("rex", "kill", created.SessionID).Run()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after kill = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the session was killed")
	}
}
