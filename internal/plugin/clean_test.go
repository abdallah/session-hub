package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/herdr"
)

// The plugin cleans title_hint and cwd before sending, so one odd pane title
// never makes the server skip the entry.
func TestPaneUpsertCleansTitleAndCWD(t *testing.T) {
	snap, _ := capturedSnap(t).Snapshot()
	var p herdr.PaneInfo
	for _, c := range snap.Panes {
		if c.PaneID == "w7:p1" {
			p = c
		}
	}
	p.TitleClean = "fix\nthe\tbug \r\n  now " + strings.Repeat("x", 300)
	p.CWD = "/home/user/a\tb"
	u, ok := paneUpsert(p, "default")
	if !ok {
		t.Fatal("pane w7:p1 was skipped")
	}
	if !strings.HasPrefix(u.TitleHint, "fix the bug now xxx") || len([]rune(u.TitleHint)) != 200 {
		t.Errorf("title_hint = %q (%d runes)", u.TitleHint, len([]rune(u.TitleHint)))
	}
	if u.CWD != "/home/user/a b" {
		t.Errorf("cwd = %q", u.CWD)
	}
}

// The heartbeat logs each entry the server reports as invalid.
func TestHeartbeatLogsInvalidEntries(t *testing.T) {
	e := newTestEnv(t)
	e.sessionhub.putBody = `{"upserted":0,"ended":[],"conflicts":[],"invalid":[{"id":"` + uuidA + `","reason":"herdr_pane \"-x\": bad"}]}`
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	e.w.heartbeat(context.Background(), t0)
	if n := e.sessionhub.count(http.MethodPut, "/v1/machines/self/herdr-sessions"); n != 1 {
		t.Fatalf("heartbeats = %d, want 1", n)
	}
	if got := e.logs.String(); !strings.Contains(got, "server skipped session") || !strings.Contains(got, uuidA) || !strings.Contains(got, "herdr_pane") {
		t.Errorf("log does not name the skipped entry:\n%s", got)
	}
	// The heartbeat still counts as sent.
	if !e.w.lastBeatOK.Equal(t0) {
		t.Errorf("lastBeatOK = %v, want %v", e.w.lastBeatOK, t0)
	}
	var put api.HerdrSessionsPut
	last := e.sessionhub.requests()[len(e.sessionhub.requests())-1]
	if err := json.Unmarshal(last.Body, &put); err != nil || len(put.Sessions) != 1 {
		t.Errorf("body %s", last.Body)
	}
}

// A pane where herdr detects Claude but reports no session ID is listed as
// unidentified, so the server keeps the session hooks registered there.
func TestUnidentifiedPanes(t *testing.T) {
	id := &herdr.AgentSessionInfo{Kind: "id", Value: "0a0b0c0d-1111-4000-8000-000000000001"}
	snap := herdr.Snapshot{Panes: []herdr.PaneInfo{
		{PaneID: "w4:p1", Agent: "claude"},                                          // no agent_session
		{PaneID: "w4:p2", Agent: "claude", AgentSession: &herdr.AgentSessionInfo{}}, // empty agent_session
		{PaneID: "w7:p1", Agent: "claude", AgentSession: id},                        // identified
		{PaneID: "w4:p5"},                 // a shell
		{PaneID: "w9:p1", Agent: "codex"}, // another agent
	}}
	got := unidentifiedPanes(snap)
	if strings.Join(got, ",") != "w4:p1,w4:p2" {
		t.Errorf("unidentifiedPanes = %v, want [w4:p1 w4:p2]", got)
	}
}
