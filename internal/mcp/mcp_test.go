package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
)

// ---- helpers ----

type hubRec struct {
	Method, Path, Auth string
	Body               json.RawMessage
}

// fakeHub records requests and answers with status(method, path, n)
// (200 by default). It stands in for the server described in docs/dev/PLAN.md
// "HTTP API".
type fakeHub struct {
	mu     sync.Mutex
	recs   []hubRec
	status func(method, path string, n int) int
	srv    *httptest.Server
}

func newFakeHub(t *testing.T, status func(method, path string, n int) int) *fakeHub {
	h := &fakeHub{status: status}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		b.ReadFrom(r.Body)
		h.mu.Lock()
		h.recs = append(h.recs, hubRec{r.Method, r.URL.Path, r.Header.Get("Authorization"), b.Bytes()})
		n := len(h.recs)
		h.mu.Unlock()
		code := 200
		if h.status != nil {
			code = h.status(r.Method, r.URL.Path, n)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if code >= 400 {
			w.Write([]byte(`{"error":"nope"}`))
		} else {
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHub) records() []hubRec {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hubRec(nil), h.recs...)
}

// newTestServer builds a Server against url with an isolated state dir and
// no herdr, and returns the queue and state dir.
func newTestServer(t *testing.T, url string, ppid int) (*Server, *client.Queue, string) {
	t.Helper()
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	state := t.TempDir()
	q := client.NewQueue(state)
	s := &Server{
		API: func() (hubAPI, error) {
			return client.New(client.Config{ServerURL: url, Token: "hub_m_test"})
		},
		Queue:    q,
		StateDir: state,
		PPID:     func() int { return ppid },
	}
	return s, q, state
}

func writeCurrent(t *testing.T, state string, pid int, id string) {
	t.Helper()
	dir := filepath.Join(state, "current")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)), []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// transcript feeds lines to Serve and returns the response lines.
func transcript(t *testing.T, s *Server, lines ...string) []string {
	t.Helper()
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	var got []string
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	return got
}

func decode(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("stdout line is not JSON: %q: %v", line, err)
	}
	return m
}

func toolText(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	c := res["content"].([]any)[0].(map[string]any)
	if c["type"] != "text" {
		t.Fatalf("content type = %v", c["type"])
	}
	isErr, _ := res["isError"].(bool)
	return c["text"].(string), isErr
}

const (
	sid1 = "11111111-2222-4333-8444-555555555555"
	sid2 = "66666666-7777-4888-8999-aaaaaaaaaaaa"
)

func callLine(id int, name, args string) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
}

// ---- protocol ----

func TestProtocolTranscript(t *testing.T) {
	s, _, _ := newTestServer(t, "http://127.0.0.1:1", 1)
	got := transcript(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"abc","method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":5,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`,
		`not json`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/unknown"}`,
	)
	// 9 lines in; two are notifications and get no response.
	if len(got) != 7 {
		t.Fatalf("got %d responses, want 7:\n%s", len(got), strings.Join(got, "\n"))
	}
	r := make([]map[string]any, len(got))
	for i, l := range got {
		r[i] = decode(t, l)
		if r[i]["jsonrpc"] != "2.0" {
			t.Errorf("line %d: jsonrpc = %v", i, r[i]["jsonrpc"])
		}
	}
	init := r[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-03-26" {
		t.Errorf("did not echo supported version: %v", init["protocolVersion"])
	}
	if _, ok := init["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("capabilities.tools missing")
	}
	if init["serverInfo"].(map[string]any)["name"] != "sessionhub" {
		t.Error("serverInfo.name != sessionhub")
	}
	if r[1]["id"] != "abc" || len(r[1]["result"].(map[string]any)) != 0 {
		t.Errorf("ping: %v", r[1])
	}
	tools := r[2]["result"].(map[string]any)["tools"].([]any)
	names := map[string]map[string]any{}
	for _, x := range tools {
		m := x.(map[string]any)
		names[m["name"].(string)] = m
		if m["description"] == "" || m["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("tool %v lacks description or object schema", m["name"])
		}
	}
	if len(names) != 10 || names["report_progress"] == nil || names["set_title"] == nil || names["remember"] == nil ||
		names["forget"] == nil || names["instructions"] == nil || names["send_to_sessions"] == nil {
		t.Errorf("tools = %v", names)
	}
	if d, _ := names["remember"]["description"].(string); !strings.Contains(d, "only when the user asks") {
		t.Errorf("remember description %q", d)
	}
	props := names["report_progress"]["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, k := range []string{"done", "in_flight", "waiting_on", "note"} {
		if props[k] == nil {
			t.Errorf("report_progress schema lacks %s", k)
		}
	}
	if e := r[3]["error"].(map[string]any); e["code"].(float64) != -32601 {
		t.Errorf("unknown method code = %v", e["code"])
	}
	if v := r[4]["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		t.Errorf("unsupported version got %v, want 2025-06-18", v)
	}
	if e := r[5]["error"].(map[string]any); e["code"].(float64) != -32700 || r[5]["id"] != nil {
		t.Errorf("parse error: %v", r[5])
	}
	if e := r[6]["error"].(map[string]any); e["code"].(float64) != -32602 {
		t.Errorf("unknown tool code = %v", e["code"])
	}
}

func TestBadArgumentsAreToolErrors(t *testing.T) {
	sessionhub := newFakeHub(t, nil)
	s, q, state := newTestServer(t, sessionhub.srv.URL, 42)
	writeCurrent(t, state, 42, sid1)
	got := transcript(t, s,
		callLine(1, "set_title", `{"title":"   "}`),
		callLine(2, "report_progress", `{"done":"not an array"}`),
		callLine(3, "set_title", `{}`),
	)
	if len(got) != 3 {
		t.Fatalf("got %d responses", len(got))
	}
	for i, l := range got {
		if _, isErr := toolText(t, decode(t, l)); !isErr {
			t.Errorf("response %d: want isError", i+1)
		}
	}
	if n := len(sessionhub.records()); n != 0 {
		t.Errorf("server got %d requests for invalid calls", n)
	}
	if n, _ := q.Len(); n != 0 {
		t.Errorf("queue has %d items for invalid calls", n)
	}
}

// ---- tools against the server ----

func TestReportProgressPostsAndReadsBack(t *testing.T) {
	sessionhub := newFakeHub(t, nil)
	s, q, state := newTestServer(t, sessionhub.srv.URL, 42)
	writeCurrent(t, state, 42, sid1)

	got := transcript(t, s, callLine(1, "report_progress",
		`{"done":["wrote tests"],"in_flight":["wiring"],"waiting_on":[],"note":"n"}`))
	txt, isErr := toolText(t, decode(t, got[0]))
	if isErr || !strings.Contains(txt, "recorded") {
		t.Fatalf("result = %q isErr=%v", txt, isErr)
	}
	recs := sessionhub.records()
	if len(recs) != 1 {
		t.Fatalf("server got %d requests, want 1", len(recs))
	}
	if recs[0].Method != "POST" || recs[0].Path != "/v1/sessions/"+sid1+"/report" || recs[0].Auth != "Bearer hub_m_test" {
		t.Errorf("request = %+v", recs[0])
	}
	var in api.ReportIn
	if err := json.Unmarshal(recs[0].Body, &in); err != nil {
		t.Fatal(err)
	}
	if len(in.Done) != 1 || in.Done[0] != "wrote tests" || in.InFlight[0] != "wiring" || in.Note != "n" {
		t.Errorf("body = %s", recs[0].Body)
	}
	if !strings.Contains(string(recs[0].Body), `"waiting_on":[]`) {
		t.Errorf("empty list must be [] not null: %s", recs[0].Body)
	}
	if n, _ := q.Len(); n != 0 {
		t.Errorf("queue has %d items after success", n)
	}
}

func TestSetTitlePosts(t *testing.T) {
	sessionhub := newFakeHub(t, nil)
	s, _, state := newTestServer(t, sessionhub.srv.URL, 42)
	writeCurrent(t, state, 42, sid1)
	got := transcript(t, s, callLine(1, "set_title", `{"title":"Fix CI"}`))
	if txt, isErr := toolText(t, decode(t, got[0])); isErr || !strings.Contains(txt, "recorded") {
		t.Fatalf("result = %q", txt)
	}
	recs := sessionhub.records()
	if len(recs) != 1 || recs[0].Path != "/v1/sessions/"+sid1+"/title" || string(recs[0].Body) != `{"title":"Fix CI"}` {
		t.Errorf("requests = %+v", recs)
	}
}

func TestReportClampsToServerCaps(t *testing.T) {
	sessionhub := newFakeHub(t, nil)
	s, _, state := newTestServer(t, sessionhub.srv.URL, 42)
	writeCurrent(t, state, 42, sid1)
	var items []string
	for i := 0; i < 25; i++ {
		items = append(items, strings.Repeat("é", 300))
	}
	args, _ := json.Marshal(map[string]any{"done": items, "in_flight": []string{}, "waiting_on": []string{}})
	transcript(t, s, callLine(1, "report_progress", string(args)))
	var in api.ReportIn
	json.Unmarshal(sessionhub.records()[0].Body, &in)
	if len(in.Done) != 20 {
		t.Errorf("done has %d items, want 20", len(in.Done))
	}
	if n := len([]rune(in.Done[0])); n != 200 {
		t.Errorf("item length = %d runes, want 200", n)
	}
}

func TestUnknownSessionUpsertsAndRetriesOnce(t *testing.T) {
	var reportCalls int
	sessionhub := newFakeHub(t, func(method, path string, n int) int {
		if strings.HasSuffix(path, "/report") {
			reportCalls++
			if reportCalls == 1 {
				return 404
			}
		}
		return 200
	})
	s, q, state := newTestServer(t, sessionhub.srv.URL, 42)
	writeCurrent(t, state, 42, sid1)
	t.Setenv("CLAUDE_PROJECT_DIR", "/work/proj")

	got := transcript(t, s, callLine(1, "report_progress", `{"done":["x"],"in_flight":[],"waiting_on":[]}`))
	if txt, _ := toolText(t, decode(t, got[0])); !strings.Contains(txt, "recorded") {
		t.Fatalf("result = %q", txt)
	}
	recs := sessionhub.records()
	if len(recs) != 3 {
		t.Fatalf("got %d requests, want report, upsert, report: %+v", len(recs), recs)
	}
	if recs[1].Path != "/v1/sessions" || recs[1].Method != "POST" {
		t.Errorf("second request = %+v", recs[1])
	}
	var up api.SessionUpsert
	json.Unmarshal(recs[1].Body, &up)
	if up.ID != sid1 || up.Agent != "claude" || up.Source != "mcp" || up.CWD != "/work/proj" {
		t.Errorf("upsert = %+v", up)
	}
	if n, _ := q.Len(); n != 0 {
		t.Errorf("queue has %d items after a successful retry", n)
	}
}

func TestPersistent404QueuesAfterOneRetry(t *testing.T) {
	sessionhub := newFakeHub(t, func(method, path string, n int) int {
		if strings.HasSuffix(path, "/title") {
			return 404
		}
		return 200
	})
	s, q, state := newTestServer(t, sessionhub.srv.URL, 42)
	writeCurrent(t, state, 42, sid1)
	got := transcript(t, s, callLine(1, "set_title", `{"title":"T"}`))
	txt, isErr := toolText(t, decode(t, got[0]))
	if isErr || !strings.Contains(txt, "queued") {
		t.Errorf("result = %q isErr=%v", txt, isErr)
	}
	if n := len(sessionhub.records()); n != 3 {
		t.Errorf("got %d requests, want 3 (no retry loop)", n)
	}
	if n, _ := q.Len(); n != 1 {
		t.Errorf("queue len = %d, want 1", n)
	}
}

func TestServerDownQueuesAndFailsOpen(t *testing.T) {
	sessionhub := newFakeHub(t, nil)
	url := sessionhub.srv.URL
	sessionhub.srv.Close() // refuse connections
	s, q, state := newTestServer(t, url, 42)
	writeCurrent(t, state, 42, sid1)

	got := transcript(t, s,
		callLine(1, "report_progress", `{"done":[],"in_flight":["a"],"waiting_on":[],"note":"n"}`),
		callLine(2, "set_title", `{"title":"T"}`))
	if len(got) != 2 {
		t.Fatalf("got %d responses", len(got))
	}
	for i, l := range got {
		txt, isErr := toolText(t, decode(t, l))
		if isErr || !strings.Contains(txt, "queued") {
			t.Errorf("response %d = %q isErr=%v", i+1, txt, isErr)
		}
	}
	var items []client.Item
	q.Drain(func(all []client.Item) []client.Item { items = all; return all })
	if len(items) != 2 {
		t.Fatalf("queue has %d items, want 2", len(items))
	}
	if items[0].Op != client.OpReport || items[0].SessionID != sid1 || items[1].Op != client.OpTitle {
		t.Errorf("items = %+v", items)
	}
	var in api.ReportIn
	json.Unmarshal(items[0].Body, &in)
	if in.InFlight[0] != "a" || in.Note != "n" {
		t.Errorf("queued body = %s", items[0].Body)
	}
}

func TestNotConfiguredQueues(t *testing.T) {
	s, q, state := newTestServer(t, "", 42) // client.New errors on an empty URL
	writeCurrent(t, state, 42, sid1)
	got := transcript(t, s, callLine(1, "set_title", `{"title":"T"}`))
	if txt, isErr := toolText(t, decode(t, got[0])); isErr || !strings.Contains(txt, "queued") {
		t.Errorf("result = %q", txt)
	}
	if n, _ := q.Len(); n != 1 {
		t.Errorf("queue len = %d", n)
	}
}

func TestRejectedByServerIsNotQueued(t *testing.T) {
	for _, code := range []int{400, 401, 403} {
		sessionhub := newFakeHub(t, func(string, string, int) int { return code })
		s, q, state := newTestServer(t, sessionhub.srv.URL, 42)
		writeCurrent(t, state, 42, sid1)
		got := transcript(t, s, callLine(1, "set_title", `{"title":"T"}`))
		txt, isErr := toolText(t, decode(t, got[0]))
		if isErr || !strings.Contains(txt, "rejected") || strings.Contains(txt, "queued") {
			t.Errorf("%d: result = %q isErr=%v", code, txt, isErr)
		}
		if n, _ := q.Len(); n != 0 {
			t.Errorf("%d: queue len = %d", code, n)
		}
	}
}

func TestServerErrorQueues(t *testing.T) {
	sessionhub := newFakeHub(t, func(string, string, int) int { return 503 })
	s, q, state := newTestServer(t, sessionhub.srv.URL, 42)
	writeCurrent(t, state, 42, sid1)
	transcript(t, s, callLine(1, "set_title", `{"title":"T"}`))
	if n, _ := q.Len(); n != 1 {
		t.Errorf("queue len = %d, want 1", n)
	}
}

func TestNoSessionIDFailsOpenAndSendsNothing(t *testing.T) {
	sessionhub := newFakeHub(t, nil)
	s, q, _ := newTestServer(t, sessionhub.srv.URL, 42)
	got := transcript(t, s, callLine(1, "report_progress", `{"done":[],"in_flight":[],"waiting_on":[]}`))
	txt, isErr := toolText(t, decode(t, got[0]))
	if isErr || !strings.Contains(txt, "could not determine") {
		t.Errorf("result = %q", txt)
	}
	if n, _ := q.Len(); n != 0 || len(sessionhub.records()) != 0 {
		t.Errorf("queued %d, sent %d", n, len(sessionhub.records()))
	}
}

// ---- session ID resolution ----

// fakeHerdr answers pane.get with panes taken from the captured session
// snapshot, answers pane.report_metadata with ok, and records every request.
// It closes each connection after one reply, like herdr 0.9.3.
type fakeHerdr struct {
	path  string
	mu    sync.Mutex
	reqs  []herdrtest.Request
	panes map[string]json.RawMessage
}

func newFakeHerdr(t *testing.T) *fakeHerdr {
	t.Helper()
	f, err := os.Open(filepath.Join(herdrtest.FixtureDir(), "session-snapshot.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	var resp struct {
		Result struct {
			Snapshot struct {
				Panes []json.RawMessage `json:"panes"`
			} `json:"snapshot"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatal(err)
	}
	h := &fakeHerdr{panes: map[string]json.RawMessage{}}
	for _, p := range resp.Result.Snapshot.Panes {
		var id struct {
			PaneID string `json:"pane_id"`
		}
		json.Unmarshal(p, &id)
		h.panes[id.PaneID] = p
	}
	dir, err := os.MkdirTemp("", "mcpherdr")
	if err != nil {
		t.Fatal(err)
	}
	h.path = filepath.Join(dir, "herdr.sock")
	ln, err := net.Listen("unix", h.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); os.RemoveAll(dir) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go h.serve(c)
		}
	}()
	return h
}

func (h *fakeHerdr) serve(c net.Conn) {
	defer c.Close()
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	var req herdrtest.Request
	json.Unmarshal(line, &req)
	h.mu.Lock()
	h.reqs = append(h.reqs, req)
	h.mu.Unlock()
	var out any
	switch req.Method {
	case "pane.get":
		var p struct {
			PaneID string `json:"pane_id"`
		}
		json.Unmarshal(req.Params, &p)
		if pane, ok := h.panes[p.PaneID]; ok {
			out = map[string]any{"id": req.ID, "result": map[string]any{"type": "pane_info", "pane": pane}}
		} else {
			out = map[string]any{"id": req.ID, "error": map[string]string{"code": "pane_not_found", "message": "no pane"}}
		}
	case "pane.report_metadata":
		out = map[string]any{"id": req.ID, "result": map[string]string{"type": "ok"}}
	default:
		out = map[string]any{"id": req.ID, "error": map[string]string{"code": "no_fixture", "message": req.Method}}
	}
	b, _ := json.Marshal(out)
	c.Write(append(b, '\n'))
}

func (h *fakeHerdr) requests(method string) []herdrtest.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []herdrtest.Request
	for _, r := range h.reqs {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func TestResolveSessionOrder(t *testing.T) {
	const (
		paneWithSession = "w7:p1"   // captured: agent_session.value = sid1
		paneNoSession   = "w4:p1"   // captured: no agent_session
		paneMissing     = "w99:p99" // not in the snapshot: pane_not_found
	)
	tests := []struct {
		name string
		pane string
		sock bool // false: HERDR_SOCKET_PATH points at nothing
		file string
		env  string
		want string
	}{
		{"file wins over herdr and env", paneWithSession, true, sid2, "env-id", sid2},
		{"file wins when the pane has no session", paneNoSession, true, sid2, "env-id", sid2},
		{"file wins when herdr is down", paneWithSession, false, sid2, "env-id", sid2},
		{"no file: herdr wins over env", paneWithSession, true, "", "env-id", sid1},
		{"no file, pane without session falls to env", paneNoSession, true, "", "env-id", "env-id"},
		{"no file, missing pane falls to env", paneMissing, true, "", "env-id", "env-id"},
		{"no file, herdr socket down falls to env", paneWithSession, false, "", "env-id", "env-id"},
		{"no pane uses file", "", true, sid2, "env-id", sid2},
		{"no pane, no file uses env", "", true, "", "env-id", "env-id"},
		{"nothing", "", true, "", "", ""},
	}
	h := newFakeHerdr(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _, state := newTestServer(t, "http://127.0.0.1:1", 4242)
			if tc.sock {
				t.Setenv("HERDR_SOCKET_PATH", h.path)
			} else {
				t.Setenv("HERDR_SOCKET_PATH", filepath.Join(state, "none.sock"))
			}
			t.Setenv("HERDR_PANE_ID", tc.pane)
			t.Setenv("CLAUDE_CODE_SESSION_ID", tc.env)
			if tc.file != "" {
				writeCurrent(t, state, 4242, tc.file)
			}
			// A file for a different pid must never be used.
			writeCurrent(t, state, 1, "other-pid")
			if got := s.resolveSession(); got != tc.want {
				t.Errorf("resolveSession() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The current/<ppid> file is the cheapest and most direct source, so a hit
// must not cost a herdr round trip.
func TestResolveFileHitSkipsHerdr(t *testing.T) {
	h := newFakeHerdr(t)
	s, _, state := newTestServer(t, "http://127.0.0.1:1", 4242)
	t.Setenv("HERDR_SOCKET_PATH", h.path)
	t.Setenv("HERDR_PANE_ID", "w7:p1")
	writeCurrent(t, state, 4242, sid2)
	if got := s.resolveSession(); got != sid2 {
		t.Errorf("resolveSession() = %q, want %q", got, sid2)
	}
	if n := len(h.requests("pane.get")); n != 0 {
		t.Errorf("herdr got %d pane.get requests, want 0", n)
	}
}

func TestResolveRunsOnEveryCall(t *testing.T) {
	sessionhub := newFakeHub(t, nil)
	s, _, state := newTestServer(t, sessionhub.srv.URL, 42)
	writeCurrent(t, state, 42, sid1)
	transcript(t, s, callLine(1, "set_title", `{"title":"a"}`))
	writeCurrent(t, state, 42, sid2) // /clear happened
	transcript(t, s, callLine(2, "set_title", `{"title":"b"}`))
	recs := sessionhub.records()
	if len(recs) != 2 || !strings.Contains(recs[0].Path, sid1) || !strings.Contains(recs[1].Path, sid2) {
		t.Errorf("requests = %+v", recs)
	}
}

// ---- herdr sidebar ----

func TestReportSendsMetadataInsideHerdr(t *testing.T) {
	h := newFakeHerdr(t)
	sessionhub := newFakeHub(t, nil)
	s, _, _ := newTestServer(t, sessionhub.srv.URL, 42)
	t.Setenv("HERDR_SOCKET_PATH", h.path)
	t.Setenv("HERDR_PANE_ID", "w7:p1")

	transcript(t, s, callLine(1, "report_progress",
		`{"done":["d"],"in_flight":["f"],"waiting_on":["the user"]}`))
	reqs := h.requests("pane.report_metadata")
	if len(reqs) != 1 {
		t.Fatalf("got %d report_metadata requests, want 1", len(reqs))
	}
	var p struct {
		PaneID string            `json:"pane_id"`
		Source string            `json:"source"`
		Tokens map[string]string `json:"tokens"`
	}
	json.Unmarshal(reqs[0].Params, &p)
	if p.PaneID != "w7:p1" || p.Source != "sessionhub" || p.Tokens["hub_summary"] != "waiting: the user" {
		t.Errorf("params = %s", reqs[0].Params)
	}
	// The session ID came from herdr (w7:p1 has agent_session sid1).
	if recs := sessionhub.records(); len(recs) != 1 || !strings.Contains(recs[0].Path, sid1) {
		t.Errorf("server requests = %+v", recs)
	}
}

func TestNoMetadataOutsideHerdrAndOnTitle(t *testing.T) {
	h := newFakeHerdr(t)
	sessionhub := newFakeHub(t, nil)
	s, _, state := newTestServer(t, sessionhub.srv.URL, 42)
	writeCurrent(t, state, 42, sid1)
	t.Setenv("HERDR_SOCKET_PATH", h.path) // available, but HERDR_PANE_ID is unset
	transcript(t, s, callLine(1, "report_progress", `{"done":["d"],"in_flight":[],"waiting_on":[]}`))
	if n := len(h.requests("pane.report_metadata")) + len(h.requests("pane.get")); n != 0 {
		t.Errorf("herdr got %d requests outside herdr", n)
	}
	t.Setenv("HERDR_PANE_ID", "w7:p1")
	transcript(t, s, callLine(2, "set_title", `{"title":"t"}`))
	if n := len(h.requests("pane.report_metadata")); n != 0 {
		t.Errorf("set_title sent %d report_metadata requests", n)
	}
}

func TestMetadataErrorDoesNotFailTool(t *testing.T) {
	sessionhub := newFakeHub(t, nil)
	s, _, state := newTestServer(t, sessionhub.srv.URL, 42)
	writeCurrent(t, state, 42, sid1)
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(state, "gone.sock"))
	t.Setenv("HERDR_PANE_ID", "w7:p1")
	got := transcript(t, s, callLine(1, "report_progress", `{"done":["d"],"in_flight":[],"waiting_on":[]}`))
	if txt, isErr := toolText(t, decode(t, got[0])); isErr || !strings.Contains(txt, "recorded") {
		t.Errorf("result = %q", txt)
	}
}

func TestSummaryRules(t *testing.T) {
	long := strings.Repeat("x", 100)
	tests := []struct {
		name string
		in   api.ReportIn
		want string
	}{
		{"waiting wins", api.ReportIn{Done: []string{"d"}, InFlight: []string{"f"}, WaitingOn: []string{"w", "w2"}}, "waiting: w"},
		{"in flight next", api.ReportIn{Done: []string{"d"}, InFlight: []string{"f", "f2"}}, "f"},
		{"done last", api.ReportIn{Done: []string{"d", "d2"}}, "done: d"},
		{"empty", api.ReportIn{}, ""},
		{"truncated to 80", api.ReportIn{InFlight: []string{long}}, strings.Repeat("x", 80)},
		{"waiting prefix counts toward 80", api.ReportIn{WaitingOn: []string{long}}, "waiting: " + strings.Repeat("x", 71)},
		{"truncates on runes", api.ReportIn{InFlight: []string{strings.Repeat("é", 90)}}, strings.Repeat("é", 80)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Summary(tc.in)
			if got != tc.want {
				t.Errorf("Summary = %q, want %q", got, tc.want)
			}
			if n := len([]rune(got)); n > 80 {
				t.Errorf("summary is %d runes", n)
			}
		})
	}
}

// ---- install ----

// fakeClaude puts a script named claude on PATH. It appends its arguments to
// a log, then runs script.
func fakeClaude(t *testing.T, script string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logPath + "\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return logPath
}

func readLog(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestInstallAndUninstall(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	logPath := fakeClaude(t, "exit 0")
	if err := RunInstall(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := RunUninstall(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"mcp add --scope user sessionhub -- /home/test/.local/bin/sessionhub mcp",
		"mcp remove --scope user sessionhub",
	}
	got := readLog(t, logPath)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("claude calls = %q, want %q", got, want)
	}
}

func TestInstallReplacesExisting(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	marker := filepath.Join(t.TempDir(), "removed")
	// add fails until remove has run.
	logPath := fakeClaude(t, `case "$2" in
remove) touch `+marker+`; exit 0;;
add) [ -f `+marker+` ] && exit 0; echo "MCP server sessionhub already exists" >&2; exit 1;;
esac`)
	if err := RunInstall(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	got := readLog(t, logPath)
	if len(got) != 3 || !strings.HasPrefix(got[0], "mcp add") || !strings.HasPrefix(got[1], "mcp remove --scope user sessionhub") || !strings.HasPrefix(got[2], "mcp add") {
		t.Errorf("claude calls = %q", got)
	}
}

func TestInstallErrors(t *testing.T) {
	fakeClaude(t, "echo boom >&2; exit 1")
	if err := RunInstall(context.Background(), nil); err == nil {
		t.Error("RunInstall succeeded although claude failed")
	}
	if err := RunUninstall(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("RunUninstall err = %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if err := RunInstall(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "claude") {
		t.Errorf("no claude on PATH: err = %v", err)
	}
}

// The server rejects control characters and text over its caps. The client
// cleans and shortens title, note, and items first, so the request the server
// sees always passes its validation.
func TestTitleAndNoteAreCleanedAndClamped(t *testing.T) {
	hasControl := func(s string) bool {
		for _, r := range s {
			if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
				return true
			}
		}
		return false
	}
	sessionhub := newFakeHub(t, nil)
	s, _, state := newTestServer(t, sessionhub.srv.URL, 42)
	writeCurrent(t, state, 42, sid1)

	dirty := "a\x00b\x07c\x7fd\u0085e\tf\ng\x1bh"
	title, _ := json.Marshal(map[string]string{"title": dirty + strings.Repeat("é", 300)})
	report, _ := json.Marshal(map[string]any{
		"done":       []string{dirty, "\x00\x01", strings.Repeat("x", 300)},
		"in_flight":  []string{},
		"waiting_on": []string{},
		"note":       dirty + strings.Repeat("n", 3000),
	})
	got := transcript(t, s,
		callLine(1, "set_title", string(title)),
		callLine(2, "report_progress", string(report)),
		callLine(3, "set_title", `{"title":"\u0000\u0007 \n"}`))

	recs := sessionhub.records()
	if len(recs) != 2 {
		t.Fatalf("server got %d requests, want 2 (the control-only title sends nothing)", len(recs))
	}
	var ti api.TitleIn
	json.Unmarshal(recs[0].Body, &ti)
	if hasControl(ti.Title) || !strings.HasPrefix(ti.Title, "a b c d e f g h") || len([]rune(ti.Title)) != 200 {
		t.Errorf("title = %q (%d runes)", ti.Title, len([]rune(ti.Title)))
	}
	var r api.ReportIn
	json.Unmarshal(recs[1].Body, &r)
	if len(r.Done) != 2 {
		t.Fatalf("done = %q, want the empty-after-cleaning item dropped", r.Done)
	}
	if r.Done[0] != "a b c d e f g h" || len([]rune(r.Done[1])) != 200 {
		t.Errorf("done = %q", r.Done)
	}
	if hasControl(r.Note) || !strings.HasPrefix(r.Note, "a b c d e f g h") || len([]rune(r.Note)) != 2000 {
		t.Errorf("note = %q (%d runes)", r.Note, len([]rune(r.Note)))
	}
	if txt, isErr := toolText(t, decode(t, got[2])); !isErr || !strings.Contains(txt, "empty") {
		t.Errorf("control-only title result = %q isErr=%v", txt, isErr)
	}
}
