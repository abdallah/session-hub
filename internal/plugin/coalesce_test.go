package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
)

// items for the coalescing table, built from captured events moved onto
// panes A (w7:p1) and B (w4:p1). The letter after the colon is the tag.
func tableItems(t *testing.T, spec string) []client.Item {
	t.Helper()
	status := capturedEvent(t, "pane.agent_status_changed-blocked")
	closed := capturedEvent(t, "pane.closed-agent-pane")
	detected := capturedEvent(t, "pane.agent_detected")
	panes := map[byte]string{'A': "w7:p1", 'B': "w4:p1"}
	var out []client.Item
	for i, tok := range strings.Fields(spec) {
		kind, rest, _ := strings.Cut(tok, ":")
		pane := panes[rest[0]]
		var e herdr.Event
		session := "default"
		switch kind {
		case "S":
			e = onPane(t, status, pane)
		case "C":
			e = onPane(t, closed, pane)
		case "U":
			e = onPane(t, detected, pane)
		case "X": // same pane ID, other herdr server
			e = onPane(t, status, pane)
			session = "other"
		case "P": // a hooks prompt event for a session (not coalesced)
			b, _ := json.Marshal(api.EventIn{Kind: api.KindPrompt, Source: api.SourceHooks, TS: time.Now()})
			out = append(out, client.Item{ID: tok + "#" + string(rune('0'+i)), Op: client.OpEvent, SessionID: "s-" + rest, Body: b})
			continue
		case "Q", "W", "T":
			// Hooks items, shaped like internal/hooks on sessionhub-v1: upserts carry
			// SessionID and PaneID (from HERDR_PANE_ID); state_changed carries
			// only SessionID. The session is the one herdr sees in the pane.
			sid := map[byte]string{'A': uuidA, 'B': uuidB}[rest[0]]
			it := hooksItem(kind, sid, pane, fmt.Sprintf("prompt %d", i))
			it.ID = tok + "#" + string(rune('0'+i))
			out = append(out, it)
			continue
		case "H": // queued herdr_sessions
			b, _ := json.Marshal(api.HerdrSessionsPut{HerdrSession: "default", Sessions: []api.SessionUpsert{}})
			out = append(out, client.Item{ID: tok + "#" + string(rune('0'+i)), Op: client.OpHerdrSessions, Body: b})
			continue
		default:
			t.Fatalf("bad spec token %q", tok)
		}
		it := mustItem(t, e, session, time.Now())
		it.ID = tok + "#" + string(rune('0'+i))
		out = append(out, it)
	}
	return out
}

// hooksItem builds a queue item the way internal/hooks (sessionhub-v1) does. Bodies
// are synthetic, not captured payloads:
//
//   - "Q": the `prompt` hook's upsert (agent_state working, first_prompt).
//   - "W": the `session-start` hook's full upsert.
//   - "T": a hooks state_changed event (`stop` → idle).
func hooksItem(kind, sid, pane, prompt string) client.Item {
	switch kind {
	case "Q", "W":
		up := api.SessionUpsert{ID: sid, Agent: "claude", Source: api.SourceHooks, CWD: "/home/user/project", HerdrPane: pane}
		if kind == "Q" {
			up.AgentState, up.FirstPrompt = "working", prompt
		} else {
			up.HerdrWorkspace, up.HerdrSession = strings.Split(pane, ":")[0], "default"
		}
		b, _ := json.Marshal(up)
		return client.Item{Op: client.OpUpsert, SessionID: up.ID, PaneID: up.HerdrPane, Body: b}
	}
	p, _ := json.Marshal(map[string]string{"agent_state": "idle"})
	b, _ := json.Marshal(api.EventIn{Kind: api.KindStateChanged, Source: api.SourceHooks, TS: time.Now().UTC(), Payload: p})
	return client.Item{Op: client.OpEvent, SessionID: sid, Body: b}
}

func ids(items []client.Item) string {
	var s []string
	for _, it := range items {
		s = append(s, it.ID)
	}
	return strings.Join(s, " ")
}

// A later digest supersedes an earlier one for the same session, whatever sits
// between them.
func TestCoalesceDigests(t *testing.T) {
	digest := func(id, tag string) client.Item {
		return client.Item{ID: tag, Op: client.OpDigest, SessionID: id, Body: []byte(`{}`)}
	}
	prompt := tableItems(t, "P:1")[0]
	in := []client.Item{digest("sess-1", "D1a"), prompt, digest("sess-2", "D2"), digest("sess-1", "D1b")}
	keep, sup := coalesce(in)
	if got := ids(keep); got != prompt.ID+" D2 D1b" {
		t.Errorf("keep %q", got)
	}
	if len(sup) != 1 || sup[0].ID != "D1a" {
		t.Errorf("superseded %+v, want the earlier sess-1 digest", sup)
	}
}

func TestCoalesceTable(t *testing.T) {
	cases := []struct {
		name, in, keep string
	}{
		{"empty", "", ""},
		{"latest state wins", "S:A S:A S:A", "S:A#2"},
		{"panes are independent", "S:A S:B S:A", "S:B#1 S:A#2"},
		{"pane_closed beats earlier state", "S:A S:A C:A", "C:A#2"},
		{"state after close is kept", "C:A S:A", "C:A#0 S:A#1"},
		{"closed and exited send one", "C:A C:A", "C:A#1"},
		{"close of B keeps state of A", "S:A C:B", "S:A#0 C:B#1"},
		{"latest pane upsert wins", "U:A U:A", "U:A#1"},
		{"upsert is not state", "U:A S:A C:A", "U:A#0 C:A#2"},
		{"other herdr server is separate", "X:A S:A", "X:A#0 S:A#1"},
		{"session events never coalesce", "P:1 P:1", "P:1#0 P:1#1"},
		{"latest herdr_sessions wins", "H:0 S:A H:0", "S:A#1 H:0#2"},
		// Hooks items name their session, so they are keyed by it.
		{"hooks prompt upserts are all kept, in order", "Q:A Q:A Q:A", "Q:A#0 Q:A#1 Q:A#2"},
		{"plugin then hooks upsert for one pane", "U:A W:A", "U:A#0 W:A#1"},
		{"hooks then plugin upsert for one pane", "W:A U:A", "W:A#0 U:A#1"},
		{"hooks upserts survive plugin state and close", "Q:A S:A Q:A C:A", "Q:A#0 Q:A#2 C:A#3"},
		{"hooks state_changed: latest per session", "T:A Q:A T:A T:B", "Q:A#1 T:A#2 T:B#3"},
		{"plugin close does not drop hooks state", "T:A C:A", "T:A#0 C:A#1"},
	}
	fails := 0
	for _, c := range cases {
		in := tableItems(t, c.in)
		keep, sup := coalesce(in)
		if got := ids(keep); got != c.keep {
			t.Errorf("%s: keep %q, want %q", c.name, got, c.keep)
			fails++
		}
		if len(keep)+len(sup) != len(in) {
			t.Errorf("%s: %d kept + %d superseded != %d in", c.name, len(keep), len(sup), len(in))
			fails++
		}
	}
	t.Logf("coalescing cases: %d, failures: %d", len(cases), fails)
}

// The captured snapshot → the herdr-sessions body startup sends.
func TestBuildSessionsFromCapturedSnapshot(t *testing.T) {
	var resp struct {
		Result struct {
			Snapshot herdr.Snapshot `json:"snapshot"`
		} `json:"result"`
	}
	lines := strings.Split(strings.TrimSpace(string(fixture(t, "socket/session-snapshot.ndjson"))), "\n")
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatal(err)
	}
	var gitCalls []string
	var mu sync.Mutex // buildSessions runs git lookups in parallel
	git := func(_ context.Context, cwd string) (string, string) {
		mu.Lock()
		defer mu.Unlock()
		gitCalls = append(gitCalls, cwd)
		return "git@example:repo.git", "main"
	}
	got := buildSessions(context.Background(), resp.Result.Snapshot, "default", git)
	want := []api.SessionUpsert{
		{ID: "11111111-2222-4333-8444-555555555555", Agent: "claude", Source: "plugin", CWD: "/home/user/project",
			GitRepo: "git@example:repo.git", GitBranch: "main", HerdrSession: "default", HerdrWorkspace: "w7",
			HerdrPane: "w7:p1", AgentState: "idle", TitleHint: "session title"},
		// w8:p1 is left out: a restored pane that kept agent_session with no
		// detected agent (agent_status unknown, shell title) runs no Claude.
	}
	if len(got) != len(want) {
		t.Fatalf("got %d sessions, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("session %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
	// Panes without agent_session (w4:p1 claude, w4:p5 shell, w9:p1 probe) and
	// the restored w8:p1 are skipped, and get no git lookup.
	for _, u := range got {
		if u.HerdrPane == "w8:p1" {
			t.Errorf("restored pane w8:p1 counted as live: %+v", u)
		}
	}
	if len(gitCalls) != 1 {
		t.Errorf("git lookups %v", gitCalls)
	}
	// An empty snapshot still sends an empty list, not null.
	b, _ := json.Marshal(api.HerdrSessionsPut{HerdrSession: "default", Sessions: buildSessions(context.Background(), herdr.Snapshot{}, "default", nil)})
	if !strings.Contains(string(b), `"sessions":[]`) {
		t.Errorf("empty set marshals as %s", b)
	}
}

func TestPaneUpsertSkipsOtherAgents(t *testing.T) {
	cases := []herdr.PaneInfo{
		{PaneID: "w1:p1", Agent: "codex", AgentSession: &herdr.AgentSessionInfo{Agent: "codex", Kind: "id", Value: "x"}},
		{PaneID: "w1:p2", Agent: "claude", AgentSession: &herdr.AgentSessionInfo{Agent: "claude", Kind: "path", Value: "/x"}},
		{PaneID: "w1:p3", Agent: "claude", AgentSession: &herdr.AgentSessionInfo{Agent: "claude", Kind: "id"}},
		{PaneID: "w1:p4", Agent: "claude"},
		// agent_session kept, no detected agent: a shell prompt, not Claude.
		{PaneID: "w1:p5", AgentStatus: "unknown", AgentSession: &herdr.AgentSessionInfo{Agent: "claude", Kind: "id", Value: "x"}},
		// agent_session from Claude, but herdr now detects another agent.
		{PaneID: "w1:p6", Agent: "codex", AgentSession: &herdr.AgentSessionInfo{Agent: "claude", Kind: "id", Value: "x"}},
	}
	for _, p := range cases {
		if u, ok := paneUpsert(p, "default"); ok {
			t.Errorf("%s: want skipped, got %+v", p.PaneID, u)
		}
	}
	if agentState("thinking") != "unknown" || agentState("working") != "working" || agentState("") != "" {
		t.Error("agentState mapping")
	}
}
