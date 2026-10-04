package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

type seen struct {
	Method, Path, Query, Auth, CT string
	Body                          []byte
}

func server(t *testing.T, status int, resp string) (*Client, *[]seen) {
	t.Helper()
	var log []seen
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		log = append(log, seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), b})
		w.WriteHeader(status)
		io.WriteString(w, resp)
	}))
	t.Cleanup(ts.Close)
	c, err := New(Config{ServerURL: ts.URL + "/", Token: "hub_m_tok"})
	if err != nil {
		t.Fatal(err)
	}
	return c, &log
}

func TestEndpointsRequestShape(t *testing.T) {
	ctx := context.Background()
	c, log := server(t, 200, `{}`)
	ts := time.Date(2026, 9, 30, 1, 2, 3, 0, time.UTC)
	calls := []struct {
		name       string
		call       func() error
		method     string
		path       string
		query      string
		wantBody   bool
		bodyKey    string
		bodyExpect string
	}{
		{"upsert", func() error {
			return c.UpsertSession(ctx, api.SessionUpsert{ID: "u1", Agent: "claude", Source: "hooks"})
		}, "POST", "/v1/sessions", "", true, "id", "u1"},
		{"put", func() error {
			return c.PutHerdrSessions(ctx, api.HerdrSessionsPut{HerdrSession: "default", Sessions: []api.SessionUpsert{{ID: "u1"}}})
		}, "PUT", "/v1/machines/self/herdr-sessions", "", true, "herdr_session", "default"},
		{"event", func() error {
			return c.PostEvent(ctx, "u1", api.EventIn{Kind: api.KindPrompt, Source: api.SourceHooks, TS: ts})
		}, "POST", "/v1/sessions/u1/events", "", true, "kind", "prompt"},
		{"report", func() error { return c.PostReport(ctx, "u1", api.ReportIn{Done: []string{"a"}}) }, "POST", "/v1/sessions/u1/report", "", true, "note", ""},
		{"title", func() error { return c.SetTitle(ctx, "u1", "My title") }, "POST", "/v1/sessions/u1/title", "", true, "title", "My title"},
		{"health", func() error { return c.Health(ctx) }, "GET", "/healthz", "", false, "", ""},
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
		if g.Method != tc.method || g.Path != tc.path || g.Query != tc.query {
			t.Errorf("%s: got %s %s?%s, want %s %s?%s", tc.name, g.Method, g.Path, g.Query, tc.method, tc.path, tc.query)
		}
		if g.Auth != "Bearer hub_m_tok" {
			t.Errorf("%s: Authorization = %q", tc.name, g.Auth)
		}
		if tc.wantBody {
			var m map[string]any
			if err := json.Unmarshal(g.Body, &m); err != nil || g.CT != "application/json" {
				t.Errorf("%s: body %q ct %q err %v", tc.name, g.Body, g.CT, err)
			} else if tc.bodyExpect != "" && m[tc.bodyKey] != tc.bodyExpect {
				t.Errorf("%s: body[%s] = %v, want %s", tc.name, tc.bodyKey, m[tc.bodyKey], tc.bodyExpect)
			}
		} else if len(g.Body) != 0 {
			t.Errorf("%s: unexpected body %q", tc.name, g.Body)
		}
	}
}

func TestEventTimestampAndPayloadRoundTrip(t *testing.T) {
	c, log := server(t, 200, `{}`)
	ts := time.Date(2026, 9, 30, 1, 2, 3, 0, time.UTC)
	err := c.PostEvent(context.Background(), "u1", api.EventIn{Kind: api.KindStateChanged, Source: api.SourcePlugin, TS: ts, Payload: json.RawMessage(`{"agent_state":"blocked"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var got api.EventIn
	if err := json.Unmarshal((*log)[0].Body, &got); err != nil || !got.TS.Equal(ts) || string(got.Payload) != `{"agent_state":"blocked"}` {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestListAndGet(t *testing.T) {
	ctx := context.Background()
	c, log := server(t, 200, `[{"id":"u1","machine":"bluebox","status":"live","started_at":"2026-09-30T00:00:00Z","last_seen_at":"2026-09-30T00:00:00Z","resume_command":"x","future_field":1}]`)
	got, err := c.ListSessions(ctx, true, "bluebox")
	if err != nil || len(got) != 1 || got[0].ID != "u1" || got[0].Machine != "bluebox" {
		t.Fatalf("got %+v err %v", got, err)
	}
	if (*log)[0].Query != "live=true&machine=bluebox" || (*log)[0].Path != "/v1/sessions" {
		t.Errorf("request = %+v", (*log)[0])
	}
	*log = nil
	if _, err := c.ListSessions(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
	if (*log)[0].Query != "" {
		t.Errorf("no filters must send no query, got %q", (*log)[0].Query)
	}

	// The server returns bare arrays only; an object is a decode error, not
	// a second accepted shape.
	c, _ = server(t, 200, `{"sessions":[{"id":"u2"}]}`)
	if got, err := c.ListSessions(ctx, false, ""); err == nil {
		t.Errorf("object-shaped list must error, got %+v", got)
	}
	c, _ = server(t, 200, `[]`)
	if got, err := c.ListMachines(ctx); err != nil || len(got) != 0 {
		t.Errorf("empty machines: %+v %v", got, err)
	}

	c, log = server(t, 200, `{"id":"u1","machine":"bluebox","status":"live","reports":[{"ts":"2026-09-30T00:00:00Z","done":["a"],"in_flight":[],"waiting_on":[]}],"events":[]}`)
	d, err := c.GetSession(ctx, "u1/../x")
	if err != nil || d.ID != "u1" || len(d.Reports) != 1 {
		t.Fatalf("detail %+v err %v", d, err)
	}
	if (*log)[0].Path != "/v1/sessions/u1/../x" && (*log)[0].Path != "/v1/sessions/u1%2F..%2Fx" {
		// the id must be one escaped path segment
		t.Errorf("path = %q", (*log)[0].Path)
	}

	c, _ = server(t, 200, `[{"name":"tower","ssh_host":"tower.example.com","herdr_host":"tower.example.com"}]`)
	if ms, err := c.ListMachines(ctx); err != nil || len(ms) != 1 || ms[0].SSHHost != "tower.example.com" {
		t.Errorf("machines %+v %v", ms, err)
	}
}

func TestNon2xxCarriesStatusAndMessage(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		status int
		body   string
		msg    string
	}{
		{401, `{"error":"invalid token"}`, "invalid token"},
		{404, `{"error":"unknown session"}`, "unknown session"},
		{409, `{"error":"ambiguous prefix"}`, "ambiguous prefix"},
		{502, "bad gateway\n", "bad gateway"}, // proxy page, not api.Error
	} {
		c, _ := server(t, tc.status, tc.body)
		_, err := c.GetSession(ctx, "x")
		var se *StatusError
		if !errors.As(err, &se) || se.Status != tc.status || se.Message != tc.msg {
			t.Errorf("status %d: err = %v", tc.status, err)
		}
		if err := c.UpsertSession(ctx, api.SessionUpsert{ID: "x"}); err == nil {
			t.Errorf("status %d: upsert returned nil", tc.status)
		}
	}
}

func TestTimeoutAndUnreachable(t *testing.T) {
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer ts.Close()
	defer close(block)
	c, _ := New(Config{ServerURL: ts.URL})
	start := time.Now()
	err := c.Health(context.Background())
	if err == nil {
		t.Fatal("want timeout error")
	}
	if d := time.Since(start); d < 1500*time.Millisecond || d > 3*time.Second {
		t.Errorf("gave up after %v, want about 2s", d)
	}

	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	c, _ = New(Config{ServerURL: url})
	start = time.Now()
	if err := c.Health(context.Background()); err == nil {
		t.Error("unreachable server returned nil")
	}
	if time.Since(start) > 2500*time.Millisecond {
		t.Error("unreachable call too slow")
	}
}

// SetTimeout lets a slow server answer: a 3 s reply fails at the default 2 s
// and succeeds with a longer limit.
func TestSetTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2500 * time.Millisecond)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()
	c, _ := New(Config{ServerURL: ts.URL})
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("default timeout: want an error from a 2.5 s reply")
	}
	c.SetTimeout(InteractiveTimeout)
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("interactive timeout: %v", err)
	}
}

func TestNewRequiresURL(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want error for empty server_url")
	}
}

// pollServer answers each request with the status and body set last.
type pollServer struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	body   string
	delay  time.Duration
	query  string
	path   string
	got    string // the last request body
}

func newPollServer(t *testing.T) *pollServer {
	p := &pollServer{status: 204}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.query, p.path, p.got = r.URL.RawQuery, r.URL.EscapedPath(), string(b)
		status, body, delay := p.status, p.body, p.delay
		p.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer hub_m_x" {
			t.Errorf("Authorization %q", r.Header.Get("Authorization"))
		}
		time.Sleep(delay)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *pollServer) set(status int, body string, delay time.Duration) {
	p.mu.Lock()
	p.status, p.body, p.delay = status, body, delay
	p.mu.Unlock()
}

func (p *pollServer) last() (query, path, body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.query, p.path, p.got
}

func TestPollControl(t *testing.T) {
	ctx := context.Background()
	p := newPollServer(t)
	c, _ := New(Config{ServerURL: p.URL, Token: "hub_m_x"})

	claim, err := c.PollControl(ctx, 30*time.Second)
	if q, path, _ := p.last(); claim != nil || err != nil || q != "wait=30" || path != "/v1/machines/self/control" {
		t.Errorf("204: %+v %v, query %q path %q", claim, err, q, path)
	}
	// A poll outlives the 2 s limit of every other call: wait plus PollSlack.
	p.set(204, "", 2500*time.Millisecond)
	start := time.Now()
	if claim, err := c.PollControl(ctx, time.Second); claim != nil || err != nil || time.Since(start) < 2500*time.Millisecond {
		t.Errorf("slow 204: %+v %v after %s", claim, err, time.Since(start))
	}
	if q, _, _ := p.last(); q != "wait=1" {
		t.Errorf("query %q", q)
	}
	p.set(200, `{"request":{"id":"cr_x","state":"claimed"},"session":{"id":"s1","herdr_pane":"w1:p1"}}`, 0)
	if claim, err := c.PollControl(ctx, time.Second); err != nil || claim == nil || claim.Request.ID != "cr_x" || claim.Session.HerdrPane != "w1:p1" {
		t.Errorf("200: %+v %v", claim, err)
	}
	// A 200 without a request is a broken server, not "nothing pending".
	p.set(200, `{}`, 0)
	if claim, err := c.PollControl(ctx, time.Second); err == nil || claim != nil {
		t.Errorf("empty 200: %+v %v", claim, err)
	}
	p.set(401, `{"error":"missing or invalid bearer token"}`, 0)
	if _, err := c.PollControl(ctx, time.Second); !IsAuthError(err) {
		t.Errorf("401: %v", err)
	}
}

func TestPostControlResult(t *testing.T) {
	p := newPollServer(t)
	p.set(200, `{"id":"cr_x","state":"done"}`, 0)
	c, _ := New(Config{ServerURL: p.URL, Token: "hub_m_x"})
	in := api.ControlResultIn{State: "done", URL: "https://claude.ai/code/session_abc", Detail: "sent"}
	if err := c.PostControlResult(context.Background(), "cr_x/../y", in); err != nil {
		t.Fatal(err)
	}
	_, path, body := p.last()
	var got api.ControlResultIn
	// The ID is one escaped path segment: it can't climb to another route.
	if path != "/v1/control/cr_x%2F..%2Fy/result" {
		t.Errorf("path %q", path)
	}
	if json.Unmarshal([]byte(body), &got) != nil || got != in {
		t.Errorf("body %s", body)
	}
}

func TestCreateControl(t *testing.T) {
	p := newPollServer(t)
	p.set(202, `{"id":"cr_x","session_id":"s1","state":"pending"}`, 0)
	c, _ := New(Config{ServerURL: p.URL, Token: "hub_m_x"})
	r, err := c.CreateControl(context.Background(), "s1")
	if _, path, _ := p.last(); err != nil || r.ID != "cr_x" || r.State != "pending" || path != "/v1/sessions/s1/remote-control" {
		t.Errorf("202: %+v %v %q", r, err, path)
	}
	p.set(409, `{"error":"not controllable: session s1 is not in herdr"}`, 0)
	var se *StatusError
	if _, err := c.CreateControl(context.Background(), "s1"); !errors.As(err, &se) || se.Status != 409 || se.Message != "not controllable: session s1 is not in herdr" {
		t.Errorf("409: %v", err)
	}
}

func TestLoginEndpoints(t *testing.T) {
	ctx := context.Background()
	c, log := server(t, 201, `{"url":"https://sessionhub.example.test/login/abc","name":"phone","expires_at":"2026-10-01T12:10:00Z"}`)
	l, err := c.CreateLogin(ctx, "phone")
	if err != nil {
		t.Fatal(err)
	}
	if l.URL != "https://sessionhub.example.test/login/abc" || l.Name != "phone" || !l.ExpiresAt.Equal(time.Date(2026, 10, 1, 12, 10, 0, 0, time.UTC)) {
		t.Errorf("login %+v", l)
	}
	got := (*log)[0]
	if got.Method != "POST" || got.Path != "/v1/logins" || got.Auth != "Bearer hub_m_tok" || string(got.Body) != `{"name":"phone"}` {
		t.Errorf("request %+v body %s", got, got.Body)
	}

	c, log = server(t, 200, `{"sessions":[{"id":"0123456789abcdef","name":"phone","machine":"tower","created_at":"2026-10-01T12:00:00Z","last_used_at":"2026-10-01T12:00:00Z","expires_at":"2026-10-31T12:00:00Z"}]}`)
	list, err := c.ListWebSessions(ctx)
	if err != nil || len(list) != 1 || list[0].ID != "0123456789abcdef" || list[0].Machine != "tower" {
		t.Fatalf("list %+v %v", list, err)
	}
	if got := (*log)[0]; got.Method != "GET" || got.Path != "/v1/web-sessions" {
		t.Errorf("request %+v", got)
	}

	c, log = server(t, 204, "")
	if err := c.RevokeWebSession(ctx, "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if got := (*log)[0]; got.Method != "DELETE" || got.Path != "/v1/web-sessions/0123456789abcdef" {
		t.Errorf("request %+v", got)
	}

	c, _ = server(t, 409, `{"error":"a browser session named \"phone\" exists: name taken"}`)
	_, err = c.CreateLogin(ctx, "phone")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 409 || !strings.Contains(se.Message, "phone") {
		t.Errorf("409: %v", err)
	}
}

func TestInboxEndpoints(t *testing.T) {
	ctx := context.Background()
	c, log := server(t, 200, `{"items":[{"group":"waiting","since":"2026-10-01T12:00:00.123456789Z","waiting_on":["review"],"session":{"id":"s1","agent":"claude","machine":"tower","started_at":"2026-10-01T11:00:00Z","last_seen_at":"2026-10-01T12:00:00Z","status":"live","resume_command":"x","controllable":false}}],"counts":{"blocked":0,"waiting":1,"finished":0}}`)
	in, err := c.Inbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Items) != 1 || in.Items[0].Since.Nanosecond() != 123456789 || in.Items[0].WaitingOn[0] != "review" ||
		in.Items[0].Session.Machine != "tower" || in.Counts.Waiting != 1 {
		t.Errorf("inbox %+v", in)
	}
	if got := (*log)[0]; got.Method != "GET" || got.Path != "/v1/inbox" || got.Auth != "Bearer hub_m_tok" {
		t.Errorf("request %+v", got)
	}

	// since keeps its nanoseconds: the server hides an item only when the
	// stored since is not earlier than the item's.
	since := time.Date(2026, 10, 1, 12, 0, 0, 123456789, time.UTC)
	c, log = server(t, 204, "")
	if err := c.DismissInbox(ctx, "s1", since); err != nil {
		t.Fatal(err)
	}
	got := (*log)[0]
	if got.Method != "POST" || got.Path != "/v1/inbox/s1/dismiss" || got.CT != "application/json" ||
		string(got.Body) != `{"since":"2026-10-01T12:00:00.123456789Z"}` {
		t.Errorf("dismiss request %+v body %s", got, got.Body)
	}

	c, log = server(t, 204, "")
	if err := c.SnoozeInbox(ctx, "s1", since, since.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got = (*log)[0]
	if got.Path != "/v1/inbox/s1/snooze" ||
		string(got.Body) != `{"since":"2026-10-01T12:00:00.123456789Z","until":"2026-10-01T13:00:00.123456789Z"}` {
		t.Errorf("snooze request %+v body %s", got, got.Body)
	}

	c, _ = server(t, 404, `{"error":"not found: session s1"}`)
	err = c.DismissInbox(ctx, "s1", since)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 404 {
		t.Errorf("404: %v", err)
	}
}
