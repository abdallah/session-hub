package hooks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/client"
)

// The hooks drain replays every op the MCP server queues, through
// client.Replay, and drops a 401 like any other non-retryable 4xx.
func TestDrainReplaysReportAndTitleOps(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	fx.h.queue.Append(client.Item{Op: client.OpReport, SessionID: "s1", Body: json.RawMessage(`{"done":["a"],"in_flight":[],"waiting_on":[]}`)})
	fx.h.queue.Append(client.Item{Op: client.OpTitle, SessionID: "s1", Body: json.RawMessage(`{"title":"T"}`)})
	fx.h.queue.Append(client.Item{Op: "mystery", SessionID: "s1", Body: json.RawMessage(`{}`)})
	if err := fx.call(t, "stop", fixtureFile(t, "Stop.json")); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range srv.requests() {
		paths = append(paths, r.Path)
	}
	got := strings.Join(paths, " ")
	if !strings.Contains(got, "/v1/sessions/s1/report /v1/sessions/s1/title") {
		t.Errorf("paths = %s", got)
	}
	if n := len(queued(t, fx.state)); n != 0 {
		t.Errorf("queue has %d items; the unknown op should be dropped", n)
	}
}

// Plugin items that carry a pane but no session ID can only be resolved by
// the watcher. Neither the hooks drain (after a live hook) nor the flush child
// may send or drop them; everything else drains as usual.
func TestDrainKeepsPaneOnlyPluginItems(t *testing.T) {
	plugin := func(id, op string) client.Item {
		return client.Item{ID: id, Op: op, PaneID: "w1:p1", Body: json.RawMessage(`{"kind":"state_changed","source":"plugin","ts":"2026-09-30T00:00:00Z"}`)}
	}
	for _, via := range []string{"hook", "flush"} {
		t.Run(via, func(t *testing.T) {
			srv := newFakeServer(t)
			fx := newFixture(t, srv.URL)
			fx.h.queue.Append(plugin("p1", client.OpEvent))
			fx.h.queue.Append(client.Item{ID: "n1", Op: client.OpTitle, SessionID: "s1", Body: json.RawMessage(`{"title":"T"}`)})
			fx.h.queue.Append(plugin("p2", client.OpUpsert))
			fx.h.queue.Append(client.Item{ID: "bad", Op: "mystery", SessionID: "s1", Body: json.RawMessage(`{}`)})
			if before := len(queued(t, fx.state)); before != 4 {
				t.Fatalf("start state has %d items, want 4", before)
			}
			var err error
			if via == "hook" {
				err = fx.call(t, "stop", fixtureFile(t, "Stop.json"))
			} else {
				err = fx.h.run(context.Background(), []string{"flush"})
			}
			if err != nil {
				t.Fatal(err)
			}
			var sentTitle bool
			for _, r := range srv.requests() {
				if r.Path == "/v1/sessions" || strings.Contains(string(r.Body), `"source":"plugin"`) {
					t.Errorf("a plugin item was sent: %s %s %s", r.Method, r.Path, r.Body)
				}
				sentTitle = sentTitle || r.Path == "/v1/sessions/s1/title"
			}
			if !sentTitle {
				t.Error("the ordinary title item was not sent")
			}
			var left []string
			for _, it := range queued(t, fx.state) {
				left = append(left, it.ID)
			}
			if got := strings.Join(left, ","); got != "p1,p2" {
				t.Errorf("queue = %s, want p1,p2 (plugin items kept in order; sent and bad items gone)", got)
			}
		})
	}
}

// A report or title queued for a session the server does not know gets the
// same upsert-and-retry an event gets.
func TestDrainUpsertsAndRetriesReportAndTitleOn404(t *testing.T) {
	srv := newFakeServer(t)
	known := map[string]bool{}
	srv.status = func(r rec) int {
		if r.Path == "/v1/sessions" {
			var u struct {
				ID string `json:"id"`
			}
			json.Unmarshal(r.Body, &u)
			known[u.ID] = true
			return 200
		}
		for _, id := range []string{"s1", "s2"} {
			if strings.HasPrefix(r.Path, "/v1/sessions/"+id+"/") && !known[id] {
				return 404
			}
		}
		return 200
	}
	fx := newFixture(t, srv.URL)
	fx.h.queue.Append(client.Item{Op: client.OpReport, SessionID: "s1", Body: json.RawMessage(`{"done":["a"],"in_flight":[],"waiting_on":[]}`)})
	fx.h.queue.Append(client.Item{Op: client.OpTitle, SessionID: "s2", Body: json.RawMessage(`{"title":"T"}`)})
	if err := fx.h.run(context.Background(), []string{"flush"}); err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, r := range srv.requests() {
		e := r.Method + " " + r.Path
		if r.Path == "/v1/sessions" {
			var u struct {
				ID string `json:"id"`
			}
			json.Unmarshal(r.Body, &u)
			e += " " + u.ID
		}
		seq = append(seq, e)
	}
	want := strings.Join([]string{
		"POST /v1/sessions/s1/report", "POST /v1/sessions s1", "POST /v1/sessions/s1/report",
		"POST /v1/sessions/s2/title", "POST /v1/sessions s2", "POST /v1/sessions/s2/title",
	}, ",")
	if got := strings.Join(seq, ","); got != want {
		t.Errorf("requests = %s\nwant       %s", got, want)
	}
	if n := len(queued(t, fx.state)); n != 0 {
		t.Errorf("queue has %d items, want 0", n)
	}
}
