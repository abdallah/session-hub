package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
)

// Every captured event → the queue item runEvent writes (or none).
func TestItemFromCapturedEvents(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 123456789, time.UTC)
	cases := []struct {
		file       string
		wantOp     string // "" means no item
		wantKind   string // event kind for OpEvent
		wantState  string
		wantStatus string
	}{
		{file: "pane.agent_detected", wantOp: client.OpUpsert},
		{file: "pane.agent_status_changed-blocked", wantOp: client.OpEvent, wantKind: api.KindStateChanged, wantState: "blocked", wantStatus: "blocked"},
		{file: "pane.closed-agent-pane", wantOp: client.OpEvent, wantKind: api.KindPaneClosed},
		{file: "pane.closed-split", wantOp: client.OpEvent, wantKind: api.KindPaneClosed},
		{file: "pane.created-root"}, // not hooked: no item
		{file: "pane.created-split"},
	}
	fails := 0
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			defer func() {
				if t.Failed() {
					fails++
				}
			}()
			e := capturedEvent(t, c.file)
			var data struct {
				PaneID      string `json:"pane_id"`
				WorkspaceID string `json:"workspace_id"`
			}
			json.Unmarshal(e.Data, &data)
			it, ok, err := itemFromEvent(e, "default", now)
			if err != nil {
				t.Fatal(err)
			}
			if c.wantOp == "" {
				if ok {
					t.Fatalf("want no item, got %+v", it)
				}
				return
			}
			if !ok || it.Op != c.wantOp {
				t.Fatalf("ok=%v op=%q, want %q", ok, it.Op, c.wantOp)
			}
			if it.PaneID != data.PaneID || it.SessionID != "" || !it.QueuedAt.Equal(now) {
				t.Errorf("item pane=%q session=%q queued=%v", it.PaneID, it.SessionID, it.QueuedAt)
			}
			switch it.Op {
			case client.OpUpsert:
				var u api.SessionUpsert
				if err := json.Unmarshal(it.Body, &u); err != nil {
					t.Fatal(err)
				}
				want := api.SessionUpsert{Agent: "claude", Source: api.SourcePlugin, HerdrSession: "default", HerdrWorkspace: data.WorkspaceID, HerdrPane: data.PaneID}
				if u != want {
					t.Errorf("upsert body %+v, want %+v", u, want)
				}
			case client.OpEvent:
				var ev api.EventIn
				if err := json.Unmarshal(it.Body, &ev); err != nil {
					t.Fatal(err)
				}
				if ev.Kind != c.wantKind || ev.Source != api.SourcePlugin || !ev.TS.Equal(now) {
					t.Errorf("event %+v", ev)
				}
				var p eventPayload
				if err := json.Unmarshal(ev.Payload, &p); err != nil {
					t.Fatal(err)
				}
				want := eventPayload{AgentState: c.wantState, AgentStatus: c.wantStatus, PaneID: data.PaneID,
					WorkspaceID: data.WorkspaceID, HerdrSession: "default", HerdrEvent: e.Event}
				if p != want {
					t.Errorf("payload %+v, want %+v", p, want)
				}
				// The server reads agent_state; ts keeps nanoseconds.
				var raw map[string]any
				json.Unmarshal(it.Body, &raw)
				if raw["ts"] != "2026-09-30T12:00:00.123456789Z" {
					t.Errorf("ts %v", raw["ts"])
				}
			}
		})
	}
	t.Logf("captured events checked: %d, failures: %d", len(cases), fails)
}

// pane.exited has no capture (testdata/README.md gaps). Its schema payload is
// the same PaneRef as pane.closed, so the captured pane.closed payload is
// relabelled to check the mapping; this is not presented as a capture.
func TestPaneExitedMapsToPaneClosed(t *testing.T) {
	e := capturedEvent(t, "pane.closed-agent-pane")
	e.Event = herdr.EventPaneExited
	it, ok, err := itemFromEvent(e, "default", time.Now())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if m := metaOf(it); m.kind != api.KindPaneClosed || it.PaneID != "w9:p1" {
		t.Errorf("meta %+v pane %q", m, it.PaneID)
	}
}

func TestItemFromEventSkipsOtherAgents(t *testing.T) {
	for _, name := range []string{"pane.agent_detected", "pane.agent_status_changed-blocked"} {
		e := capturedEvent(t, name)
		var d map[string]any
		json.Unmarshal(e.Data, &d)
		d["agent"] = "codex"
		e.Data, _ = json.Marshal(d)
		if it, ok, err := itemFromEvent(e, "default", time.Now()); ok || err != nil {
			t.Errorf("%s with agent codex: ok=%v err=%v item=%+v", name, ok, err, it)
		}
	}
}

func TestItemFromEventMalformed(t *testing.T) {
	for _, raw := range []string{
		`{"event":"pane_closed","data":"not an object"}`,
		`{"event":"pane_agent_status_changed","data":[1]}`,
	} {
		e, err := herdr.ParseEvent([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := itemFromEvent(e, "default", time.Now()); ok || err == nil {
			t.Errorf("%s: ok=%v err=%v, want an error", raw, ok, err)
		}
	}
}

// runEvent end to end: the env payload lands in the queue, the watcher is
// started, and nothing touches the network (no server is configured at all).
func TestRunEventQueuesAndStartsWatcher(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SESSIONHUB_STATE_DIR", dir)
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(dir, "no-config.toml"))
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(dir, "sessions", "work", "herdr.sock"))
	t.Setenv("SESSIONHUB_PLUGIN_TEST_HELPER", "exit")
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", string(fixture(t, "events/pane.agent_status_changed-blocked.json")))
	executable = func() (string, error) { return os.Args[0], nil }
	defer func() { executable = os.Executable }()

	q := client.NewQueue(dir)
	if n := len(queueItems(t, q)); n != 0 {
		t.Fatalf("queue starts with %d items", n)
	}
	start := time.Now()
	runEvent(dir, time.Now())
	elapsed := time.Since(start)
	items := queueItems(t, q)
	if len(items) != 1 || items[0].Op != client.OpEvent || items[0].PaneID != "w9:p1" {
		t.Fatalf("queue: %+v", items)
	}
	if m := metaOf(items[0]); m.kind != api.KindStateChanged || m.herdrSession != "work" {
		t.Errorf("meta %+v", m)
	}
	spawned := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "spawned")); err == nil {
			spawned = true
			break
		}
	}
	if !spawned {
		t.Error("watcher was not started (no spawned marker)")
	}
	t.Logf("runEvent took %s", elapsed)

	// Garbage payload: nothing queued, still no panic or error exit.
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", "{not json")
	runEvent(dir, time.Now())
	if n := len(queueItems(t, q)); n != 1 {
		t.Errorf("queue has %d items after a malformed payload, want 1", n)
	}
}
