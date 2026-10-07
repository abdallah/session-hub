package server

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

// Session IDs used across tests. They look like Claude session UUIDs; they
// are request fixtures for the sessionhub API, not captured payloads.
const (
	sid1 = "3f2a9c10-1111-4a4a-8b8b-000000000001"
	sid2 = "3f2a9c10-2222-4a4a-8b8b-000000000002"
	sid3 = "7c1e5d22-3333-4c4c-9d9d-000000000003"
	sid4 = "9b0f7e44-4444-4e4e-afaf-000000000004"
	sid5 = "c4d8a166-5555-4f4f-b0b0-000000000005"
	sid6 = "e6a2c388-6666-4a4a-c1c1-000000000006"
)

// testPublicURL is the env server's public_url: login links start with it,
// and POST /login/{code} must carry it as Origin.
const testPublicURL = "https://sessionhub.example.test"

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// env is a running API over a temp database with two machines: tower
// (token A) and bluebox (token B).
type env struct {
	t       *testing.T
	st      *store.Store
	clock   *fakeClock
	srv     *httptest.Server
	server  *Server
	log     *syncBuffer
	tokA    string
	tokB    string
	web     string // session cookie of the browser session "phone", made by tower
	webID   string // that session's ID
	dbPath  string // the store's file, for reads the store has no method for
	moveDir string // where the server keeps sealed bundles
}

const staleAfter = 5 * time.Minute

func newEnv(t *testing.T) *env {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	dbPath := filepath.Join(t.TempDir(), "sessionhub.db")
	st, err := store.Open(dbPath, store.Options{Now: clock.Now, StaleAfter: staleAfter})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	tokA, _, err := st.AddMachine(ctx, "tower", "tower.example.com", "tower.example.com")
	if err != nil {
		t.Fatal(err)
	}
	tokB, _, err := st.AddMachine(ctx, "bluebox", "bluebox.example.com", "bluebox.example.com")
	if err != nil {
		t.Fatal(err)
	}
	logBuf := &syncBuffer{}
	s := New(st, testPublicURL, log.New(logBuf, "", 0))
	moveDir := filepath.Join(t.TempDir(), "moves")
	s.SetMoveDir(moveDir)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e := &env{t: t, st: st, clock: clock, srv: srv, server: s, log: logBuf, tokA: tokA, tokB: tokB, dbPath: dbPath, moveDir: moveDir}
	e.web, e.webID = e.webSession("phone")
	return e
}

// do sends a request. body may be nil, a []byte (sent raw), or a value
// marshaled as JSON.
func (e *env) do(method, path, token string, body any) (int, []byte, http.Header) {
	e.t.Helper()
	return e.doWith(method, path, token, "", body)
}

// doWith is do plus an optional session cookie value.
func (e *env) doWith(method, path, token, cookie string, body any) (int, []byte, http.Header) {
	e.t.Helper()
	return e.doHdr(method, path, token, cookie, nil, body)
}

// doHdr is doWith plus extra request headers.
func (e *env) doHdr(method, path, token, cookie string, hdr map[string]string, body any) (int, []byte, http.Header) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := noRedirect().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, out, resp.Header
}

// machine returns the store's machine for a token.
func (e *env) machine(token string) store.Machine {
	e.t.Helper()
	m, err := e.st.MachineByToken(context.Background(), token)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

// loginCode creates a sign-in code for name as tower's `sessionhub login` would.
func (e *env) loginCode(name string) string {
	e.t.Helper()
	code, _, err := e.st.CreateLoginCode(context.Background(), e.machine(e.tokA).ID, name)
	if err != nil {
		e.t.Fatal(err)
	}
	return code
}

// webSession signs a browser in as name through the store and returns its
// cookie token and session ID.
func (e *env) webSession(name string) (token, id string) {
	e.t.Helper()
	tok, ws, err := e.st.RedeemLoginCode(context.Background(), e.loginCode(name))
	if err != nil {
		e.t.Fatal(err)
	}
	return tok, ws.ID
}

// submitLogin presses Sign in: POST /login/{code} with Origin set to origin,
// or with no Origin when origin is "".
func (e *env) submitLogin(code, origin string) (int, []byte, http.Header) {
	e.t.Helper()
	var hdr map[string]string
	if origin != "" {
		hdr = map[string]string{"Origin": origin}
	}
	return e.doHdr("POST", "/login/"+code, "", "", hdr, nil)
}

// recordPoll marks the token's machine as polling now, as its watcher would.
func (e *env) recordPoll(token string) {
	e.t.Helper()
	if err := e.st.RecordPoll(context.Background(), e.machine(token).ID); err != nil {
		e.t.Fatal(err)
	}
}

// controllable registers id on tower with a pane and records a tower poll.
func (e *env) controllable(id, pane string) {
	e.t.Helper()
	e.register(e.tokA, api.SessionUpsert{ID: id, HerdrSession: "default", HerdrPane: pane, CWD: "/home/user/proj"})
	e.recordPoll(e.tokA)
}

// tap sends the dashboard's Remote Control write: the session cookie and
// X-Hub-Action.
func (e *env) tap(id string) (int, api.ControlRequest, []byte) {
	e.t.Helper()
	code, b, _ := e.doHdr("POST", "/v1/sessions/"+id+"/remote-control", "", e.web,
		map[string]string{api.HeaderAction: api.HeaderActionRemoteControl}, nil)
	var r api.ControlRequest
	json.Unmarshal(b, &r)
	return code, r, b
}

// instantTimer is a Server.after whose timer has already fired.
func instantTimer(time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	c <- time.Time{}
	return c
}

// fakeTimers is a Server.after that records each wait and fires only when
// the test says so.
type fakeTimers struct {
	mu    sync.Mutex
	waits []time.Duration
	chans []chan time.Time
}

func (f *fakeTimers) after(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make(chan time.Time, 1)
	f.waits = append(f.waits, d)
	f.chans = append(f.chans, c)
	return c
}

// await blocks until n timers exist.
func (f *fakeTimers) await(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		got := len(f.chans)
		f.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d poll timers after 5 s, want %d", got, n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *fakeTimers) fire(i int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chans[i] <- time.Time{}
}

func (f *fakeTimers) wait(i int) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waits[i]
}

type pollResult struct {
	code int
	body []byte
	err  error
}

// pollAsync sends GET /v1/machines/self/control in a goroutine. It never
// calls t.Fatal, which is only allowed on the test's own goroutine.
func (e *env) pollAsync(token, query string) <-chan pollResult {
	ch := make(chan pollResult, 1)
	go func() {
		req, err := http.NewRequest("GET", e.srv.URL+"/v1/machines/self/control"+query, nil)
		if err != nil {
			ch <- pollResult{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			ch <- pollResult{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		ch <- pollResult{code: resp.StatusCode, body: b, err: err}
	}()
	return ch
}

func recv(t *testing.T, ch <-chan pollResult) pollResult {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("poll: %v", r.err)
		}
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not answer within 5 s")
	}
	return pollResult{}
}

// must sends a request, requires the status, and decodes the body into out.
func (e *env) must(want int, method, path, token string, body, out any) {
	e.t.Helper()
	code, b, _ := e.do(method, path, token, body)
	if code != want {
		e.t.Fatalf("%s %s: status %d, want %d; body %s", method, path, code, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: decode %s: %v", method, path, b, err)
		}
	}
}

func (e *env) register(token string, u api.SessionUpsert) api.Session {
	e.t.Helper()
	if u.Source == "" {
		u.Source = api.SourceHooks
	}
	if u.Agent == "" {
		u.Agent = "claude"
	}
	var s api.Session
	code, b, _ := e.do("POST", "/v1/sessions", token, u)
	if code != http.StatusCreated && code != http.StatusOK {
		e.t.Fatalf("register %s: status %d: %s", u.ID, code, b)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) event(token, id, kind string, payload string) api.Session {
	e.t.Helper()
	in := api.EventIn{Kind: kind, Source: api.SourcePlugin, TS: e.clock.Now()}
	if payload != "" {
		in.Payload = json.RawMessage(payload)
	}
	var s api.Session
	e.must(http.StatusOK, "POST", "/v1/sessions/"+id+"/events", token, in, &s)
	return s
}

func (e *env) detail(id string) api.SessionDetail {
	e.t.Helper()
	var d api.SessionDetail
	e.must(http.StatusOK, "GET", "/v1/sessions/"+id, e.tokA, nil, &d)
	return d
}

func (e *env) list(query string) []api.Session {
	e.t.Helper()
	var l []api.Session
	e.must(http.StatusOK, "GET", "/v1/sessions"+query, e.tokA, nil, &l)
	return l
}

// snapshot is every session's detail as JSON, to prove a rejected request
// changed nothing.
func (e *env) snapshot() string {
	e.t.Helper()
	var all []api.SessionDetail
	for _, s := range e.list("") {
		all = append(all, e.detail(s.ID))
	}
	b, _ := json.Marshal(all)
	return string(b) + "\ninbox_triage:\n" + e.triageRows() + "actions:\n" + e.actionRows() + "moves:\n" + e.moveRows()
}

// moveRows dumps moves, every machine's move key, and the bundle files, so
// the auth matrix sees a rejected request that wrote only there.
func (e *env) moveRows() string {
	e.t.Helper()
	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	var b strings.Builder
	for _, q := range []string{
		`SELECT 'move|' || id || '|' || state || '|' || detail || '|' || bundle_size || '|' || COALESCE(source_closed_at, '') FROM moves ORDER BY id`,
		`SELECT 'key|' || name || '|' || move_key FROM machines ORDER BY name`,
		`SELECT 'start|' || id || '|' || state || '|' || dir || '|' || url FROM start_requests ORDER BY id`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			e.t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				e.t.Fatal(err)
			}
			b.WriteString(line + "\n")
		}
		rows.Close()
	}
	files, _ := os.ReadDir(e.moveDir)
	for _, f := range files {
		info, _ := f.Info()
		fmt.Fprintf(&b, "file|%s|%d\n", f.Name(), info.Size())
	}
	return b.String()
}

// testMoveKey is a fresh X25519 public key in the server's form.
func testMoveKey(t *testing.T) string {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
}

// triageRows is the inbox_triage table as text, read over a second
// connection: the store has no method that lists it, and the auth matrix
// must see a rejected request that wrote only this table. The sqlite driver
// is registered by the store package.
func (e *env) triageRows() string {
	e.t.Helper()
	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT session_id, triaged_since, COALESCE(snooze_until, ''), updated_at
		FROM inbox_triage ORDER BY session_id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, since, until, updated string
		if err := rows.Scan(&id, &since, &until, &updated); err != nil {
			e.t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s|%s|%s|%s\n", id, since, until, updated)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return b.String()
}

// taskRows dumps the task tables over a second connection, so the auth
// matrix sees a rejected request that wrote only there.
func (e *env) taskRows() string {
	e.t.Helper()
	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id, title, ref, ref_url, source, state, COALESCE(merged_into, ''), created_by, updated_at FROM tasks ORDER BY id`,
		`SELECT task_id, ts, from_state, to_state, actor, note, COALESCE(client_id, '') FROM task_events ORDER BY id`,
		`SELECT task_id, session_id, linked_at FROM task_sessions ORDER BY task_id, session_id`,
		`SELECT session_id, ignored_at FROM task_session_ignores ORDER BY session_id`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			e.t.Fatal(err)
		}
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				e.t.Fatal(err)
			}
			fmt.Fprintln(&b, vals...)
		}
		if err := rows.Err(); err != nil {
			e.t.Fatal(err)
		}
		rows.Close()
	}
	return b.String()
}

func ids(l []api.Session) []string {
	out := []string{}
	for _, s := range l {
		out = append(out, s.ID)
	}
	return out
}

// actionRows dumps the instructions, messages, and permission_requests
// tables over a second connection, so the auth matrix sees a rejected
// request that wrote only there.
func (e *env) actionRows() string {
	e.t.Helper()
	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	var b strings.Builder
	for _, q := range []string{
		`SELECT 'rule|' || id || '|' || text || '|' || created_by FROM instructions ORDER BY id`,
		`SELECT 'msg|' || id || '|' || session_id || '|' || state || '|' || detail || '|' || COALESCE(offered_at, '') FROM messages ORDER BY id`,
		`SELECT 'perm|' || id || '|' || state || '|' || decision || '|' || decided_by FROM permission_requests ORDER BY id`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			e.t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				e.t.Fatal(err)
			}
			b.WriteString(line + "\n")
		}
		rows.Close()
	}
	return b.String()
}

// getAsync sends GET path with the token in a goroutine. It never calls
// t.Fatal, which is only allowed on the test's own goroutine.
func (e *env) getAsync(token, path string) <-chan pollResult {
	ch := make(chan pollResult, 1)
	go func() {
		req, err := http.NewRequest("GET", e.srv.URL+path, nil)
		if err != nil {
			ch <- pollResult{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			ch <- pollResult{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		ch <- pollResult{code: resp.StatusCode, body: b, err: err}
	}()
	return ch
}
