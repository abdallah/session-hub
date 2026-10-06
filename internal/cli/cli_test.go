package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

var now = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func fixtures() []api.Session {
	return []api.Session{
		{ID: "aaaaaaaa-1111-4222-8333-444455556666", Machine: "bluebox", Title: "fix CI token rotation", Status: api.StatusLive,
			LastSeenAt: now.Add(-30 * time.Second), LatestReport: &api.Report{InFlight: []string{"rotate the token"}, Done: []string{"read docs"}}},
		{ID: "bbbbbbbb-1111-4222-8333-444455556666", Machine: "tower", CWD: "/home/user/proj", Status: api.StatusBlocked,
			LastSeenAt: now.Add(-12 * time.Minute), LatestReport: &api.Report{WaitingOn: []string{"review"}}},
		{ID: "cccccccc-1111-4222-8333-444455556666", Machine: "bluebox", Title: "old work", Status: api.StatusStale,
			LastSeenAt: now.Add(-5 * time.Hour), LatestReport: &api.Report{Done: []string{"a", "shipped it"}}},
		{ID: "dddddddd-1111-4222-8333-444455556666", Machine: "tower", Status: api.StatusEnded,
			LastSeenAt: now.Add(-72 * time.Hour), LatestReport: &api.Report{Note: "just a note"}},
	}
}

type fakeAPI struct {
	sessions []api.Session
	detail   api.SessionDetail
	err      error
	gotLive  bool
	gotMach  string
}

func (f *fakeAPI) ListSessions(_ context.Context, live bool, machine string) ([]api.Session, error) {
	f.gotLive, f.gotMach = live, machine
	return f.sessions, f.err
}
func (f *fakeAPI) GetSession(context.Context, string) (api.SessionDetail, error) {
	return f.detail, f.err
}
func (f *fakeAPI) Health(context.Context) error { return f.err }

func run(t *testing.T, f *fakeAPI, cfg client.Config, fn func(*env) error) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := &env{cfg: cfg, api: f, out: &out, now: func() time.Time { return now }}
	err := fn(e)
	return out.String(), err
}

func TestLsDefaultShowsLiveAndBlocked(t *testing.T) {
	f := &fakeAPI{sessions: fixtures()}
	out, err := run(t, f, client.Config{}, func(e *env) error { return e.ls(context.Background(), nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !f.gotLive {
		t.Error("default ls must ask the server for live sessions")
	}
	for _, want := range []string{"MACHINE", "aaaaaaaa", "bbbbbbbb", "blocked", "30s", "12m",
		"doing: rotate the token", "waiting: review", "/home/user/proj"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"cccccccc", "dddddddd", "stale", "ended"} {
		if strings.Contains(out, bad) {
			t.Errorf("default ls shows %q:\n%s", bad, out)
		}
	}
}

func TestLsAllAndMachine(t *testing.T) {
	f := &fakeAPI{sessions: fixtures()}
	out, err := run(t, f, client.Config{}, func(e *env) error { return e.ls(context.Background(), []string{"--all", "--machine", "bluebox"}) })
	if err != nil {
		t.Fatal(err)
	}
	if f.gotLive || f.gotMach != "bluebox" {
		t.Errorf("live=%v machine=%q", f.gotLive, f.gotMach)
	}
	for _, want := range []string{"cccccccc", "dddddddd", "stale", "ended", "5h", "3d", "done: shipped it", "just a note", "(untitled)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// Columns line up: every row's ID starts at the same offset.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	col := strings.Index(lines[0], "ID")
	for _, l := range lines[1:] {
		if col >= len(l) || l[col-1] != ' ' {
			t.Errorf("misaligned row %q (col %d)", l, col)
		}
	}
}

func TestLsEmptyAndErrors(t *testing.T) {
	out, err := run(t, &fakeAPI{}, client.Config{}, func(e *env) error { return e.ls(context.Background(), nil) })
	if err != nil || strings.TrimSpace(out) != "no sessions" {
		t.Errorf("empty: %q, %v", out, err)
	}
	if _, err := run(t, &fakeAPI{err: errors.New("boom")}, client.Config{}, func(e *env) error { return e.ls(context.Background(), nil) }); err == nil {
		t.Error("server error must fail")
	}
	for _, args := range [][]string{{"--bogus"}, {"extra"}} {
		if _, err := run(t, &fakeAPI{}, client.Config{}, func(e *env) error { return e.ls(context.Background(), args) }); err == nil {
			t.Errorf("ls %v: want usage error", args)
		}
	}
}

func TestCleanAndAge(t *testing.T) {
	if got := clean("a  b\nc", 10); got != "a b c" {
		t.Errorf("clean whitespace = %q", got)
	}
	if got := clean("abcdefghij", 5); got != "abcd…" {
		t.Errorf("clean = %q", got)
	}
	if got := shortID("aaaa\x1bbbbbbbbb"); got != "aaaabbbb" {
		t.Errorf("shortID = %q", got)
	}
	if got := age(now, time.Time{}); got != "-" {
		t.Errorf("zero age = %q", got)
	}
	if got := age(now, now.Add(time.Hour)); got != "0s" {
		t.Errorf("future age = %q", got)
	}
}

func TestShow(t *testing.T) {
	ended := now.Add(-time.Hour)
	d := api.SessionDetail{
		Session: api.Session{ID: "aaaaaaaa-1111-4222-8333-444455556666", Machine: "bluebox", Title: "t", Status: api.StatusEnded,
			CWD: "/home/user/proj", HerdrSession: "default", HerdrPane: "w1:p1", StartedAt: now.Add(-2 * time.Hour),
			LastSeenAt: ended, EndedAt: &ended, ResumeCommand: "ssh -t bluebox.example.com sessionhub resume aaaaaaaa"},
		Reports: []api.Report{{TS: now, Done: []string{"one"}, InFlight: []string{"two"}, WaitingOn: []string{"three"}, Note: "n"}},
		Events:  []api.Event{{TS: now, Source: "hooks", Kind: "ended", Payload: json.RawMessage(`{"reason":"other"}`)}},
	}
	out, err := run(t, &fakeAPI{detail: d}, client.Config{}, func(e *env) error { return e.show(context.Background(), []string{"aaaa"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id:", d.ID, "pane=w1:p1", "ended:", "ssh -t bluebox.example.com sessionhub resume aaaaaaaa",
		"reports (1, newest first)", "done", "in flight", "waiting on", "note", "events (1, newest first)", "hooks", `{"reason":"other"}`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := run(t, &fakeAPI{}, client.Config{}, func(e *env) error { return e.show(context.Background(), nil) }); err == nil {
		t.Error("show with no argument must fail")
	}
}

// Server strings with escape sequences, newlines, and bidi controls never
// reach the terminal raw, and ls shows at most 60 runes of a title.
func TestHostileServerStrings(t *testing.T) {
	const esc = "\x1b]0;pwned\a\x1b[2J"
	long := strings.Repeat("é", 80)
	sessions := []api.Session{
		{ID: "aaaaaaaa-1111-4222-8333-444455556666" + esc, Machine: "bluebox" + esc, Title: "evil" + esc + "\nsecond line\u202e",
			Status: api.StatusLive, LastSeenAt: now, LatestReport: &api.Report{InFlight: []string{"x" + esc + "\ny"}}},
		{ID: "bbbbbbbb-1111-4222-8333-444455556666", Machine: "tower", Title: long, Status: api.StatusLive, LastSeenAt: now},
	}
	out, err := run(t, &fakeAPI{sessions: sessions}, client.Config{}, func(e *env) error { return e.ls(context.Background(), nil) })
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Errorf("ls printed %d lines, want header + 2:\n%s", len(lines), out)
	}
	if strings.ContainsAny(out, "\x1b\a\u202e") || !strings.Contains(out, "evil]0;pwned[2J second line") {
		t.Errorf("ls output not cleaned:\n%q", out)
	}
	if want := strings.Repeat("é", 59) + "…"; !strings.Contains(out, want) || strings.Contains(out, strings.Repeat("é", 60)) {
		t.Errorf("long title not cut to 60 runes:\n%s", out)
	}

	d := api.SessionDetail{Session: sessions[0],
		Reports: []api.Report{{TS: now, Done: []string{"d" + esc}, Note: "n" + esc + "\nnext"}},
		Events:  []api.Event{{TS: now, Source: "hooks" + esc, Kind: "ended", Payload: json.RawMessage(`{"r":"` + `\u001b[2J"}`)}},
	}
	d.CWD, d.HerdrPane = "/x"+esc, "w1:p1"+esc
	out, err = run(t, &fakeAPI{detail: d}, client.Config{}, func(e *env) error { return e.show(context.Background(), []string{"aaaa"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out, "\x1b\a\u202e") {
		t.Errorf("show output not cleaned:\n%q", out)
	}
	for _, want := range []string{"pane=w1:p1]0;pwned[2J", "n]0;pwned[2J next"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
}

func TestShowAgainstHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(api.Error{Error: "ambiguous prefix: aaaa1111, aaaa2222"})
	}))
	defer srv.Close()
	c, _ := client.New(client.Config{ServerURL: srv.URL})
	var out bytes.Buffer
	e := &env{api: c, out: &out, now: time.Now}
	err := e.show(context.Background(), []string{"aaaa"})
	if err == nil || !strings.Contains(err.Error(), "aaaa2222") {
		t.Errorf("err = %v, want candidate IDs", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed on error: %q", out.String())
	}
}

func TestStatus(t *testing.T) {
	f := &fakeAPI{sessions: fixtures()[:1]}
	out, err := run(t, f, client.Config{ServerURL: "https://sessionhub.example", Machine: "bluebox"}, func(e *env) error { return e.status(context.Background(), nil) })
	if err != nil {
		t.Fatal(err)
	}
	if f.gotMach != "bluebox" || !f.gotLive {
		t.Errorf("machine=%q live=%v", f.gotMach, f.gotLive)
	}
	for _, want := range []string{"server:  https://sessionhub.example", "machine: bluebox", "health:  ok", "aaaaaaaa"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	out, err = run(t, &fakeAPI{err: errors.New("down")}, client.Config{Machine: "bluebox"}, func(e *env) error { return e.status(context.Background(), nil) })
	if err == nil || !strings.Contains(out, "unreachable") {
		t.Errorf("unreachable: err=%v out=%q", err, out)
	}
	if _, err := run(t, &fakeAPI{}, client.Config{}, func(e *env) error { return e.status(context.Background(), nil) }); err == nil {
		t.Error("status without a machine name must fail")
	}
}

func TestMatches(t *testing.T) {
	s := api.Session{Machine: "tower", Title: "Fix CI", Recap: "Next, merge MR 391.",
		LastPrompt: "why is the [build] red?", GitBranch: "feat/tokens", CWD: "/home/u/app"}
	for q, want := range map[string]bool{
		"":        true,
		"fix ci":  true,
		"MR 391":  true,
		"[build]": true,
		"tokens":  true,
		"/u/app":  true,
		"TOWER":   true,
		"nothing": false,
	} {
		if got := matches(s, q); got != want {
			t.Errorf("matches(%q) = %v, want %v", q, got, want)
		}
	}
}

func TestSummaryLine(t *testing.T) {
	cost := 18.271
	cases := []struct {
		sum  *api.SessionSummary
		want string
	}{
		{nil, ""},
		{&api.SessionSummary{OutputTokens: 371471}, "371k tokens out"},
		{&api.SessionSummary{Git: &api.GitCounts{CommitCount: 3, Uncommitted: 2, Unpushed: 1},
			LatestLink: &api.DigestLink{Number: "391", URL: "https://git.example.com/o/r/-/merge_requests/391"},
			CostUSD:    &cost, OutputTokens: 5},
			"3 commits · 2 uncommitted · 1 unpushed · !391 · $18.27"},
		{&api.SessionSummary{Git: &api.GitCounts{CommitCount: 1, Unpushed: -1},
			LatestLink: &api.DigestLink{Number: "7", URL: "https://github.com/o/r/pull/7"}, OutputTokens: 1500000},
			"1 commit · #7 · 1.5M tokens out"},
	}
	for _, c := range cases {
		if got := summaryLine(c.sum); got != c.want {
			t.Errorf("summaryLine(%+v) = %q, want %q", c.sum, got, c.want)
		}
	}
}

func TestShowDigest(t *testing.T) {
	cost := 18.27
	at := now.Add(-time.Minute)
	d := api.SessionDetail{
		Session: api.Session{ID: "aaaaaaaa-1111-4222-8333-444455556666", Machine: "bluebox", Title: "t", Status: api.StatusLive,
			GitBranch: "feat/x", StartedAt: now.Add(-2 * time.Hour), LastSeenAt: now,
			Recap: "Next, merge the MR.", RecapAt: &at, LastPrompt: "why is CI red?", LastPromptAt: &at},
		Digest: &api.Digest{DigestIn: api.DigestIn{
			AsOf: at,
			Git: &api.DigestGit{GitCounts: api.GitCounts{CommitCount: 2}, Commits: []api.DigestCommit{
				{SHA: "abc1234", Subject: "\x1b[31mred"}, {SHA: "def5678", Subject: "green"}}},
			Links: []api.DigestLink{
				{Number: "391", URL: "https://git.example.com/o/r/-/merge_requests/391"},
				{Number: "7", URL: "https://github.com/o/r/pull/7"}},
			Tokens:  api.DigestTokens{Input: 10, Output: 371471, CacheRead: 1500000, CacheWrite: 2000},
			CostUSD: &cost, CostAt: &at,
		}},
	}
	out, err := run(t, &fakeAPI{detail: d}, client.Config{}, func(e *env) error { return e.show(context.Background(), []string{"aaaa"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"recap:", "last prompt:", "commits on feat/x during the session (2)",
		"abc1234  ", "def5678  green", "https://git.example.com/o/r/-/merge_requests/391", "https://github.com/o/r/pull/7",
		"tokens:", "input 10", "output 371k", "cache read 1.5M", "cache write 2k", "cost:", "$18.27 as of", "digest as of"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("output contains ESC:\n%q", out)
	}
}

func TestLsGrep(t *testing.T) {
	list := fixtures()[:3]
	list[1].Recap = "Next, merge the MR."
	out, err := run(t, &fakeAPI{sessions: list}, client.Config{}, func(e *env) error {
		return e.ls(context.Background(), []string{"--all", "--grep", "merge"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bbbbbbbb") || strings.Contains(out, "aaaaaaaa") || strings.Contains(out, "cccccccc") {
		t.Errorf("grep merge kept the wrong sessions:\n%s", out)
	}
	out, err = run(t, &fakeAPI{sessions: list}, client.Config{}, func(e *env) error {
		return e.ls(context.Background(), []string{"--grep", "nothing"})
	})
	if err != nil || !strings.Contains(out, "no sessions") {
		t.Errorf("grep nothing = %q, %v", out, err)
	}
}

func TestLsGrepHintsAtEndedMatches(t *testing.T) {
	list := fixtures()
	list[2].Recap = "unique-needle" // stale
	list[3].Title = "unique-needle" // ended
	out, err := run(t, &fakeAPI{sessions: list}, client.Config{}, func(e *env) error {
		return e.ls(context.Background(), []string{"--grep", "unique-needle"})
	})
	if err != nil || !strings.Contains(out, "no live sessions match; 2 ended match") || !strings.Contains(out, "--all") {
		t.Errorf("out = %q, err = %v; want the --all hint", out, err)
	}
}

// A blocked session's question from its mod replaces the report in sessionhub ls,
// and sessionhub show prints it with the mod's usage. Both are cleaned.
func TestLsAndShowBlockedOnAndUsage(t *testing.T) {
	const esc = "\x1b[2J"
	pct, cost, at := 42, 1.25, now.Add(-2*time.Minute)
	s := api.Session{ID: "aaaaaaaa-1111-4222-8333-444455556666", Machine: "bluebox", Title: "t", Status: api.StatusBlocked,
		LastSeenAt: now, LatestReport: &api.Report{InFlight: []string{"hidden"}},
		BlockedOn: "Question: Which" + esc + " library?\u202e", ContextPercent: &pct, LiveCostUSD: &cost, UsageAt: &at}
	out, err := run(t, &fakeAPI{sessions: []api.Session{s}}, client.Config{}, func(e *env) error { return e.ls(context.Background(), nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "blocked on: Question: Which[2J library?") || strings.Contains(out, "hidden") || strings.ContainsAny(out, "\x1b\u202e") {
		t.Errorf("ls:\n%q", out)
	}
	out, err = run(t, &fakeAPI{detail: api.SessionDetail{Session: s}}, client.Config{}, func(e *env) error {
		return e.show(context.Background(), []string{"aaaa"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"blocked on: Question: Which[2J library?\n", "usage:     context 42% · $1.25 (2m ago)\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	if strings.ContainsAny(out, "\x1b\u202e") {
		t.Errorf("show output not cleaned:\n%q", out)
	}
	// Without a report from a mod, show prints neither line.
	out, _ = run(t, &fakeAPI{detail: api.SessionDetail{Session: api.Session{ID: s.ID, Status: api.StatusLive}}}, client.Config{},
		func(e *env) error { return e.show(context.Background(), []string{"aaaa"}) })
	if strings.Contains(out, "blocked on:") || strings.Contains(out, "usage:") {
		t.Errorf("show without mod data:\n%s", out)
	}
}
