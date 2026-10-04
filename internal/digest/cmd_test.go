package digest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

type fakeHub struct {
	mu       sync.Mutex
	digests  map[string]api.DigestIn
	status   int // answer for PUT digest; 0 means 200
	sessions []api.Session
}

func (f *fakeHub) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/digest"):
		if f.status != 0 {
			w.WriteHeader(f.status)
			io.WriteString(w, `{"error":"x"}`)
			return
		}
		var d api.DigestIn
		json.NewDecoder(r.Body).Decode(&d)
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/digest")
		f.digests[id] = d
		io.WriteString(w, `{}`)
	case r.Method == "GET" && r.URL.Path == "/v1/sessions/"+sid[:8]:
		json.NewEncoder(w).Encode(api.SessionDetail{Session: api.Session{ID: sid}})
	case r.Method == "GET" && r.URL.Path == "/v1/sessions":
		if r.URL.Query().Get("machine") != "bluebox" {
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(f.sessions)
	default:
		w.WriteHeader(404)
	}
}

func newCmdEnv(t *testing.T, sessionhub *fakeHub, r Reader) (*cmdEnv, *bytes.Buffer, *client.Queue) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(sessionhub.handler))
	t.Cleanup(srv.Close)
	q := client.NewQueue(t.TempDir())
	out := &bytes.Buffer{}
	e := &cmdEnv{
		out:   out,
		queue: q,
		cfg:   client.Config{ServerURL: srv.URL, Token: "hub_m_test", Machine: "bluebox"},
		build: Builder{Reader: r, Git: func(context.Context, string, time.Time, time.Time) *api.DigestGit { return nil }},
	}
	return e, out, q
}

func TestDigestOne(t *testing.T) {
	r, _ := setup(t, assistant(t, "m1", 1, 10))
	sessionhub := &fakeHub{digests: map[string]api.DigestIn{}}
	e, out, q := newCmdEnv(t, sessionhub, r)
	if err := e.run(context.Background(), []string{sid}); err != nil {
		t.Fatal(err)
	}
	if d, ok := sessionhub.digests[sid]; !ok || d.Tokens.Output != 10 {
		t.Errorf("server got %+v %v", d, ok)
	}
	if n, _ := q.Len(); n != 0 {
		t.Errorf("queue has %d items, want 0", n)
	}
	if !strings.Contains(out.String(), "sent") {
		t.Errorf("output %q, want sent", out.String())
	}
}

func TestDigestQueuesWhenServerDown(t *testing.T) {
	r, _ := setup(t, assistant(t, "m1", 1, 10))
	sessionhub := &fakeHub{digests: map[string]api.DigestIn{}, status: 503}
	e, _, q := newCmdEnv(t, sessionhub, r)
	if err := e.run(context.Background(), []string{sid}); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.Len(); n != 1 {
		t.Fatalf("queue has %d items, want 1", n)
	}
}

func TestDigestSkips(t *testing.T) {
	sessionhub := &fakeHub{digests: map[string]api.DigestIn{}}
	e, out, q := newCmdEnv(t, sessionhub, Reader{ClaudeDir: t.TempDir(), StateDir: t.TempDir()})
	// No transcript: success, nothing sent, nothing queued.
	if err := e.run(context.Background(), []string{sid}); err != nil {
		t.Fatal(err)
	}
	if len(sessionhub.digests) != 0 || !strings.Contains(out.String(), "skipped") {
		t.Errorf("digests=%v out=%q", sessionhub.digests, out.String())
	}
	if n, _ := q.Len(); n != 0 {
		t.Errorf("queue has %d items", n)
	}
	// 404: the server doesn't know the session. An error, not queued.
	r, _ := setup(t, assistant(t, "m1", 1, 10))
	sessionhub.status = 404
	e.build.Reader = r
	if err := e.run(context.Background(), []string{sid}); err == nil {
		t.Error("404: want an error")
	}
	if n, _ := q.Len(); n != 0 {
		t.Errorf("404 queued %d items", n)
	}
}

func TestDigestAll(t *testing.T) {
	r, _ := setup(t, assistant(t, "m1", 1, 10))
	sessionhub := &fakeHub{digests: map[string]api.DigestIn{}, sessions: []api.Session{
		{ID: sid, Machine: "bluebox"}, {ID: "no-transcript-session", Machine: "bluebox"},
	}}
	e, out, _ := newCmdEnv(t, sessionhub, r)
	if err := e.run(context.Background(), []string{"--all"}); err != nil {
		t.Fatal(err)
	}
	if len(sessionhub.digests) != 1 {
		t.Errorf("sent %d digests, want 1", len(sessionhub.digests))
	}
	for _, want := range []string{sid[:8] + "  sent", "no-trans  skipped: no transcript", "1 sent, 1 skipped, 0 failed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	// Running it again is safe.
	if err := e.run(context.Background(), []string{"--all"}); err != nil {
		t.Fatal(err)
	}
}

func TestDigestUsage(t *testing.T) {
	e, _, _ := newCmdEnv(t, &fakeHub{digests: map[string]api.DigestIn{}}, Reader{})
	for _, args := range [][]string{nil, {"a", "b"}, {"--all", "x"}} {
		if err := e.run(context.Background(), args); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("args %v: err %v, want usage", args, err)
		}
	}
}

func TestBuildClamps(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	f := func(v float64) *float64 { return &v }
	tests := []struct {
		name                             string
		asOf                             time.Time
		firstAt, recapAt, costAt         *time.Time
		cost                             *float64
		tokens                           api.DigestTokens
		wantAsOf                         time.Time
		wantFirst, wantRecap, wantCostAt bool
		wantCost                         *float64
		wantTokens                       api.DigestTokens
	}{
		{name: "in range", asOf: now.Add(-time.Hour), firstAt: at(-2 * time.Hour), recapAt: at(-time.Hour), costAt: at(-time.Hour),
			cost: f(1.5), tokens: api.DigestTokens{Input: 1, Output: 2}, wantAsOf: now.Add(-time.Hour),
			wantFirst: true, wantRecap: true, wantCostAt: true, wantCost: f(1.5), wantTokens: api.DigestTokens{Input: 1, Output: 2}},
		{name: "as_of within 5 minutes is kept", asOf: now.Add(4 * time.Minute), wantAsOf: now.Add(4 * time.Minute)},
		{name: "as_of far future is set to now, later times dropped", asOf: now.Add(48 * time.Hour),
			firstAt: at(-time.Hour), recapAt: at(10 * time.Minute), costAt: at(24 * time.Hour),
			wantAsOf: now, wantFirst: true},
		{name: "times within 5 minutes after as_of are kept", asOf: now.Add(-time.Hour),
			recapAt: at(-time.Hour + 4*time.Minute), costAt: at(-time.Hour + 6*time.Minute),
			wantAsOf: now.Add(-time.Hour), wantRecap: true},
		{name: "cost too high", asOf: now, cost: f(100001), wantAsOf: now},
		{name: "cost negative", asOf: now, cost: f(-1), wantAsOf: now},
		{name: "cost at bound", asOf: now, cost: f(100000), wantAsOf: now, wantCost: f(100000)},
		{name: "tokens clamped", asOf: now, tokens: api.DigestTokens{Input: -5, Output: 5e12, CacheRead: 1e12, CacheWrite: 5e12 + 1},
			wantAsOf: now, wantTokens: api.DigestTokens{Input: 0, Output: 1e12, CacheRead: 1e12, CacheWrite: 1e12}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newState()
			st.AsOf, st.FirstAt, st.RecapAt, st.CostAt, st.CostUSD, st.Tokens = &tc.asOf, tc.firstAt, tc.recapAt, tc.costAt, tc.cost, tc.tokens
			in, ok := st.DigestIn()
			if !ok {
				t.Fatal("DigestIn not ok")
			}
			b := Builder{Now: func() time.Time { return now }}
			d := b.clamp(in)
			if !d.AsOf.Equal(tc.wantAsOf) {
				t.Errorf("AsOf = %v, want %v", d.AsOf, tc.wantAsOf)
			}
			for _, c := range []struct {
				name string
				got  *time.Time
				want bool
			}{{"FirstAt", d.FirstAt, tc.wantFirst}, {"RecapAt", d.RecapAt, tc.wantRecap}, {"CostAt", d.CostAt, tc.wantCostAt}} {
				if (c.got != nil) != c.want {
					t.Errorf("%s set = %v, want %v", c.name, c.got != nil, c.want)
				}
			}
			if (d.CostUSD == nil) != (tc.wantCost == nil) || d.CostUSD != nil && *d.CostUSD != *tc.wantCost {
				t.Errorf("CostUSD = %v, want %v", d.CostUSD, tc.wantCost)
			}
			if d.Tokens != tc.wantTokens {
				t.Errorf("Tokens = %+v, want %+v", d.Tokens, tc.wantTokens)
			}
		})
	}
}

func TestDigestResolvesPrefix(t *testing.T) {
	r, _ := setup(t, assistant(t, "m1", 1, 10))
	sessionhub := &fakeHub{digests: map[string]api.DigestIn{}}
	e, out, _ := newCmdEnv(t, sessionhub, r)
	if err := e.run(context.Background(), []string{sid[:8]}); err != nil {
		t.Fatalf("prefix: %v\n%s", err, out)
	}
	if _, ok := sessionhub.digests[sid]; !ok || !strings.Contains(out.String(), "sent") {
		t.Errorf("digests=%v out=%q, want a digest for the full ID", sessionhub.digests, out.String())
	}
	// An unknown prefix prints the server's error.
	out.Reset()
	if err := e.run(context.Background(), []string{"deadbeef"}); err == nil || !strings.Contains(out.String(), "failed") {
		t.Errorf("unknown prefix: err=%v out=%q, want a failure", err, out.String())
	}
}

func TestBoundShrinksOversizedDigest(t *testing.T) {
	d := api.DigestIn{AsOf: time.Now().UTC(), Recap: strings.Repeat("r", 600)}
	for i := 0; i < 20; i++ {
		d.Links = append(d.Links, api.DigestLink{Number: "1", URL: "https://x.example/" + strings.Repeat("u", 250),
			Repo: strings.Repeat("\"\\\u0001", 70)}) // heavy escaping
	}
	if b, _ := json.Marshal(d); len(b) <= maxBodyBytes {
		t.Fatalf("fixture is only %d bytes", len(b))
	}
	got := bound(d)
	b, _ := json.Marshal(got)
	if len(b) > maxBodyBytes || len(got.Links) == 0 || len(got.Links) >= 20 {
		t.Fatalf("body %d bytes, %d links", len(b), len(got.Links))
	}
	if got.Links[len(got.Links)-1] != d.Links[19] {
		t.Error("bound kept the wrong links: want the newest")
	}
	// No links and a recap that is too long by itself: the recap is cut.
	d.Links = nil
	d.Recap = strings.Repeat("\u0001", 3000) // past the 600 rune cap: only bound is under test
	got = bound(d)
	if b, _ := json.Marshal(got); len(b) > maxBodyBytes {
		t.Errorf("recap not cut: %d bytes", len(b))
	}
}
