package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestModCallsRequestShape(t *testing.T) {
	ctx := context.Background()
	c, log := server(t, 200, `{"id":"msg_x","state":"delivered"}`)
	pct, cost := 42, 1.5
	calls := []struct {
		name         string
		call         func() error
		method, path string
		body         string
	}{
		{"blocked-on", func() error { return c.SetBlockedOn(ctx, "s 1", "Question: ok?") },
			"POST", "/v1/sessions/s 1/blocked-on", `{"text":"Question: ok?"}`},
		{"blocked-on clear", func() error { return c.SetBlockedOn(ctx, "s1", "") },
			"POST", "/v1/sessions/s1/blocked-on", `{"text":""}`},
		{"blocked-on clear of a question", func() error { return c.ClearBlockedOn(ctx, "s1", "Question: ok?") },
			"POST", "/v1/sessions/s1/blocked-on", `{"text":"","clears":"Question: ok?"}`},
		{"usage", func() error { return c.PutUsage(ctx, "s1", api.UsageIn{ContextPercent: &pct, CostUSD: &cost}) },
			"PUT", "/v1/sessions/s1/usage", `{"context_percent":42,"cost_usd":1.5}`},
		{"usage, percent only", func() error { return c.PutUsage(ctx, "s1", api.UsageIn{ContextPercent: &pct}) },
			"PUT", "/v1/sessions/s1/usage", `{"context_percent":42}`},
		{"result", func() error {
			m, err := c.PostMessageResult(ctx, "msg_x", api.MessageResultIn{State: api.MessageBusy, Detail: "busy"})
			if err == nil && (m.ID != "msg_x" || m.State != api.MessageDelivered) {
				t.Errorf("result response: %+v", m)
			}
			return err
		}, "POST", "/v1/messages/msg_x/result", `{"state":"busy","detail":"busy"}`},
	}
	for _, tc := range calls {
		*log = nil
		if err := tc.call(); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(*log) != 1 {
			t.Errorf("%s: %d requests, want 1", tc.name, len(*log))
			continue
		}
		g := (*log)[0]
		if g.Method != tc.method || g.Path != tc.path || string(g.Body) != tc.body || g.Auth != "Bearer hub_m_tok" ||
			g.CT != "application/json" {
			t.Errorf("%s: got %s %s %s auth=%q ct=%q", tc.name, g.Method, g.Path, g.Body, g.Auth, g.CT)
		}
	}
}

func TestPollSessionMessage(t *testing.T) {
	ctx := context.Background()
	var status int
	var body, query, path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query, path = r.URL.RawQuery, r.URL.Path
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	c, err := New(Config{ServerURL: ts.URL, Token: "hub_m_tok"})
	if err != nil {
		t.Fatal(err)
	}

	status, body = 200, `{"id":"msg_a","text":"From the user via sessionhub (dashboard):\n\nhi"}`
	m, err := c.PollSessionMessage(ctx, "s1", 25*time.Second)
	if err != nil || m == nil || m.ID != "msg_a" || m.Text != "From the user via sessionhub (dashboard):\n\nhi" {
		t.Fatalf("claim: %+v %v", m, err)
	}
	if path != "/v1/sessions/s1/messages/next" || query != "wait=25" {
		t.Errorf("request %s?%s", path, query)
	}
	for _, w := range []struct {
		wait time.Duration
		want string
	}{{0, "wait=1"}, {500 * time.Millisecond, "wait=1"}, {time.Hour, "wait=30"}} {
		if _, err := c.PollSessionMessage(ctx, "s1", w.wait); err != nil {
			t.Fatal(err)
		}
		if query != w.want {
			t.Errorf("wait %s: query %q, want %q", w.wait, query, w.want)
		}
	}

	status, body = 204, ""
	if m, err := c.PollSessionMessage(ctx, "s1", time.Second); err != nil || m != nil {
		t.Errorf("204: %+v %v", m, err)
	}
	status, body = 200, `{}`
	if m, err := c.PollSessionMessage(ctx, "s1", time.Second); err == nil || m != nil {
		t.Errorf("a claim without an ID: %+v %v", m, err)
	}
	status, body = 409, `{"error":"session s1 is registered to machine \"bluebox\""}`
	_, err = c.PollSessionMessage(ctx, "s1", time.Second)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 409 {
		t.Errorf("409: %v", err)
	}
}
