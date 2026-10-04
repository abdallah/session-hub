package herdr_test

import (
	"os"
	"testing"

	"github.com/abdallah/session-hub/internal/herdr"
)

// TestLiveReadOnly runs read-only calls against the real herdr socket. It is
// skipped unless SESSIONHUB_LIVE_HERDR=1. It never focuses, splits, starts, or
// reports metadata.
func TestLiveReadOnly(t *testing.T) {
	if os.Getenv("SESSIONHUB_LIVE_HERDR") != "1" {
		t.Skip("set SESSIONHUB_LIVE_HERDR=1 to run against the live herdr socket")
	}
	c, err := herdr.Dial("")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	snap, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("socket=%s session=%s herdr=%s protocol=%d panes=%d agents=%d focused=%s",
		herdr.SocketPath(), herdr.SessionName(herdr.SocketPath()), snap.Version, snap.Protocol, len(snap.Panes), len(snap.Agents), snap.FocusedPaneID)
	for _, p := range snap.Panes {
		t.Logf("pane %s agent=%q status=%s session=%q title=%q", p.PaneID, p.Agent, p.AgentStatus, p.SessionID(), p.TitleClean)
	}
	if len(snap.Panes) == 0 {
		t.Fatal("no panes")
	}
	id := snap.Panes[0].PaneID
	p, err := c.PaneGet(id)
	if err != nil {
		t.Fatal(err)
	}
	if p.PaneID != id {
		t.Errorf("PaneGet(%s) returned %s", id, p.PaneID)
	}
	t.Logf("PaneGet(%s): agent=%q status=%s cwd=%s", id, p.Agent, p.AgentStatus, p.CWD)
	_, err = c.PaneGet("w999:p999")
	t.Logf("PaneGet(missing) error: %v", err)
	if err == nil {
		t.Error("missing pane must error")
	}
}
