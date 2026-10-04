package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
)

func TestInboxText(t *testing.T) {
	cases := []struct {
		c    api.InboxCounts
		want string
	}{
		{api.InboxCounts{}, ""},
		{api.InboxCounts{Waiting: 2, Finished: 1}, "3"},
		{api.InboxCounts{Blocked: 1, Waiting: 1, Finished: 1}, "3 · 1 blocked"},
		{api.InboxCounts{Blocked: 2}, "2 · 2 blocked"},
	}
	for _, c := range cases {
		if got := inboxText(c.c); got != c.want {
			t.Errorf("inboxText(%+v) = %q, want %q", c.c, got, c.want)
		}
	}
}

// metadataReports decodes the workspace.report_metadata requests so far.
func metadataReports(t *testing.T, s *herdrtest.Server) []herdr.WorkspaceMetadataParams {
	t.Helper()
	var out []herdr.WorkspaceMetadataParams
	for _, r := range s.RequestsFor("workspace.report_metadata") {
		var p herdr.WorkspaceMetadataParams
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func inboxBody(c api.InboxCounts) string {
	b, _ := json.Marshal(api.Inbox{Items: []api.InboxItem{}, Counts: c})
	return string(b)
}

// TestHeartbeatInboxCount walks the sidebar count through heartbeats: set,
// unchanged (no call), changed, inbox read failure (no call), report
// failure (retried), and cleared.
func TestHeartbeatInboxCount(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var mu sync.Mutex
	refuse := false
	e.herdr.Handle("workspace.report_metadata", func(json.RawMessage) (any, string) {
		mu.Lock()
		defer mu.Unlock()
		if refuse {
			return nil, "test_refused"
		}
		return map[string]any{"type": "ok"}, ""
	})
	e.w.sidebar = e.w.herdr.(*herdr.Client)
	var ids []string
	for _, ws := range capturedSnap(t).snap.Workspaces {
		ids = append(ids, ws.WorkspaceID)
	}
	if len(ids) < 2 {
		t.Fatalf("captured snapshot has %d workspaces, want several", len(ids))
	}
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	beat := func(i int) { e.w.step(ctx, t0.Add(time.Duration(i)*heartbeatInterval)) }
	// check asserts the reports since the last check: one per workspace with
	// text want ("" for a clear), or none when want is "-".
	seen := 0
	check := func(step, want string) {
		t.Helper()
		all := metadataReports(t, e.herdr)
		got := all[seen:]
		seen = len(all)
		if want == "-" {
			if len(got) != 0 {
				t.Errorf("%s: %d reports, want none", step, len(got))
			}
			return
		}
		if len(got) != len(ids) {
			t.Fatalf("%s: %d reports, want %d (one per workspace)", step, len(got), len(ids))
		}
		for i, p := range got {
			v, present := p.Tokens["inbox"]
			if p.WorkspaceID != ids[i] || p.Source != "sessionhub" || len(p.Tokens) != 1 || !present {
				t.Errorf("%s: report %d %+v", step, i, p)
				continue
			}
			if (want == "" && v != nil) || (want != "" && (v == nil || *v != want)) {
				t.Errorf("%s: %s inbox token %v, want %q", step, p.WorkspaceID, v, want)
			}
		}
	}

	e.sessionhub.setInbox(0, inboxBody(api.InboxCounts{Blocked: 1, Waiting: 1, Finished: 1}))
	beat(0)
	check("first beat", "3 · 1 blocked")
	beat(1)
	check("unchanged", "-")
	e.sessionhub.setInbox(0, inboxBody(api.InboxCounts{Waiting: 2}))
	beat(2)
	check("changed", "2")
	e.sessionhub.setInbox(http.StatusInternalServerError, "")
	beat(3)
	check("inbox read fails: the count stays", "-")
	mu.Lock()
	refuse = true
	mu.Unlock()
	e.sessionhub.setInbox(0, inboxBody(api.InboxCounts{}))
	beat(4)
	check("report refused", "")
	mu.Lock()
	refuse = false
	mu.Unlock()
	beat(5)
	check("retried after a refusal", "")
	beat(6)
	check("cleared and unchanged", "-")
}

// TestHeartbeatSidebarStartsWithClear: a new watcher reports every
// workspace on its first heartbeat, even with an empty inbox, so a count an
// older watcher left behind goes away.
func TestHeartbeatSidebarStartsWithClear(t *testing.T) {
	e := newTestEnv(t)
	e.herdr.Handle("workspace.report_metadata", func(json.RawMessage) (any, string) { return map[string]any{"type": "ok"}, "" })
	e.w.sidebar = e.w.herdr.(*herdr.Client)
	e.sessionhub.setInbox(0, inboxBody(api.InboxCounts{}))
	e.w.step(context.Background(), time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	got := metadataReports(t, e.herdr)
	if len(got) != len(capturedSnap(t).snap.Workspaces) {
		t.Fatalf("%d reports, want one per workspace", len(got))
	}
	for _, p := range got {
		if v, ok := p.Tokens["inbox"]; !ok || v != nil {
			t.Errorf("%s: %v, want a clear", p.WorkspaceID, p.Tokens)
		}
	}
}

// TestHeartbeatWithoutSidebar: a watcher without a reporter (the default in
// tests) never reads the inbox.
func TestHeartbeatWithoutSidebar(t *testing.T) {
	e := newTestEnv(t)
	e.w.step(context.Background(), time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	if n := e.sessionhub.count(http.MethodGet, "/v1/inbox"); n != 0 {
		t.Errorf("%d inbox reads without a sidebar reporter", n)
	}
}
