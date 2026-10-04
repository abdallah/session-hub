package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
)

// withData returns a captured event with one data field set ("" removes it).
func withData(t *testing.T, e herdr.Event, field, value string) herdr.Event {
	t.Helper()
	var d map[string]json.RawMessage
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatal(err)
	}
	if value == "" {
		delete(d, field)
	} else {
		d[field], _ = json.Marshal(value)
	}
	e.Data, _ = json.Marshal(d)
	return e
}

func TestBlockedStatus(t *testing.T) {
	blocked := capturedEvent(t, "pane.agent_status_changed-blocked")
	cases := []struct {
		name string
		e    herdr.Event
		want bool
	}{
		{"captured blocked claude pane", blocked, true},
		{"no agent named", withData(t, blocked, "agent", ""), true},
		{"working", withData(t, blocked, "agent_status", "working"), false},
		{"another agent", withData(t, blocked, "agent", "codex"), false},
		{"no pane", withData(t, blocked, "pane_id", ""), false},
		{"another event", capturedEvent(t, "pane.agent_detected"), false},
	}
	for _, c := range cases {
		d, ok := blockedStatus(c.e)
		if ok != c.want || (ok && d.PaneID != "w9:p1") {
			t.Errorf("%s: ok=%v pane=%q, want ok=%v", c.name, ok, d.PaneID, c.want)
		}
	}
}

// shownParams decodes the notification.show requests the fake received.
func shownParams(t *testing.T, srv *herdrtest.Server) []herdr.NotificationParams {
	t.Helper()
	var out []herdr.NotificationParams
	for _, r := range srv.RequestsFor("notification.show") {
		var p herdr.NotificationParams
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func showOK(json.RawMessage) (any, string) {
	return map[string]any{"type": "notification_show", "shown": true, "reason": "shown"}, ""
}

func TestNotifyBlocked(t *testing.T) {
	srv := herdrtest.New(t, "pane-get-existing.ndjson") // pane.get w9:p1: title "claude"
	srv.Handle("notification.show", showOK)
	h, err := herdr.Dial(srv.Path)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := blockedStatus(capturedEvent(t, "pane.agent_status_changed-blocked"))
	// The captured event has no title: the pane's.
	if err := notifyBlocked(h, d, "tower"); err != nil {
		t.Fatal(err)
	}
	// An event title is cleaned and cut to 80 characters; so is the machine.
	d.Title = "fix \x1b]0;evil\x07 " + strings.Repeat("y", 100)
	if err := notifyBlocked(h, d, "to\ngh"); err != nil {
		t.Fatal(err)
	}
	// No title anywhere (the pane is unknown): "Claude".
	gone := herdr.AgentStatusChanged{PaneID: "w9:p404", AgentStatus: "blocked"}
	if err := notifyBlocked(h, gone, "tower"); err != nil {
		t.Fatal(err)
	}
	want := []herdr.NotificationParams{
		{Title: "Blocked: claude", Body: "tower", Sound: "request"},
		{Title: "Blocked: fix ]0;evil " + strings.Repeat("y", 67) + "…", Body: "to gh", Sound: "request"},
		{Title: "Blocked: Claude", Body: "tower", Sound: "request"},
	}
	got := shownParams(t, srv)
	if len(got) != len(want) {
		t.Fatalf("%d notifications, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("notification %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	srv.Handle("notification.show", func(json.RawMessage) (any, string) {
		return map[string]any{"type": "notification_show", "shown": false, "reason": "rate_limited"}, ""
	})
	if err := notifyBlocked(h, d, "tower"); err == nil || !strings.Contains(err.Error(), "rate_limited") {
		t.Errorf("shown=false: %v, want an error with the reason", err)
	}
}

// TestRunEventNotifiesBlocked: sessionhub plugin event shows the notification for
// a blocked status and still queues the event; a working status shows none;
// a herdr failure goes to watcher.log and the hook still returns.
func TestRunEventNotifiesBlocked(t *testing.T) {
	dir := t.TempDir()
	srv := herdrtest.New(t, "pane-get-existing.ndjson")
	srv.Handle("notification.show", showOK)
	blocked := string(fixture(t, "events/pane.agent_status_changed-blocked.json"))
	t.Setenv("SESSIONHUB_STATE_DIR", dir)
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(dir, "no-config.toml"))
	t.Setenv("SESSIONHUB_MACHINE", "tower")
	t.Setenv("HERDR_SOCKET_PATH", srv.Path)
	t.Setenv("SESSIONHUB_PLUGIN_TEST_HELPER", "exit")
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", blocked)
	executable = func() (string, error) { return os.Args[0], nil }
	defer func() { executable = os.Executable }()

	runEvent(dir, time.Now())
	got := shownParams(t, srv)
	if len(got) != 1 || got[0] != (herdr.NotificationParams{Title: "Blocked: claude", Body: "tower", Sound: "request"}) {
		t.Fatalf("notifications %+v", got)
	}
	if n := len(queueItems(t, client.NewQueue(dir))); n != 1 {
		t.Errorf("queue has %d items, want 1", n)
	}

	t.Setenv("HERDR_PLUGIN_EVENT_JSON", strings.Replace(blocked, `"agent_status":"blocked"`, `"agent_status":"working"`, 1))
	runEvent(dir, time.Now())
	if n := len(shownParams(t, srv)); n != 1 {
		t.Errorf("a working status showed a notification (%d total)", n)
	}

	srv.Handle("notification.show", func(json.RawMessage) (any, string) { return nil, "test_refused" })
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", blocked)
	runEvent(dir, time.Now())
	b, err := os.ReadFile(filepath.Join(dir, watcherLogFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `herdr notification for blocked pane "w9:p1"`) || !strings.Contains(string(b), "test_refused") {
		t.Errorf("watcher.log lacks the failure:\n%s", b)
	}
}
