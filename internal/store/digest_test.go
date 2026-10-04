package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// digestFixture opens a store with a fake clock, one machine, and one
// session registered through the hooks client.
func digestFixture(t *testing.T) (*Store, int64, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	s, err := Open(t.TempDir()+"/sessionhub.db", Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	tok, _, err := s.AddMachine(ctx, "bluebox", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.MachineByToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertSession(ctx, m.ID, api.SessionUpsert{ID: "sess-1", Agent: "claude", Source: api.SourceHooks}); err != nil {
		t.Fatal(err)
	}
	return s, m.ID, &now
}

func validDigest(asOf time.Time) api.DigestIn {
	cost := 18.27
	return api.DigestIn{
		AsOf:    asOf,
		Recap:   "We fixed the review notes. Next, merge MR 391.",
		Links:   []api.DigestLink{{Number: "391", URL: "https://git.example.com/org/repo/-/merge_requests/391", Repo: "org/repo"}},
		Tokens:  api.DigestTokens{Input: 10, Output: 371471, CacheRead: 5, CacheWrite: 7},
		CostUSD: &cost,
		Git: &api.DigestGit{GitCounts: api.GitCounts{CommitCount: 3, Uncommitted: 2, Unpushed: -1},
			Commits: []api.DigestCommit{{SHA: "ba935ba", Subject: "Record the re-test"}}},
	}
}

func TestPutDigestStoresAndReads(t *testing.T) {
	s, mid, now := digestFixture(t)
	ctx := context.Background()
	before, _ := s.GetSession(ctx, "sess-1")
	if before.Summary != nil || before.Recap != "" {
		t.Fatalf("summary before any digest: %+v %q", before.Summary, before.Recap)
	}
	d := validDigest(now.Add(-time.Minute))
	stored, err := s.PutDigest(ctx, mid, "sess-1", d)
	if err != nil || !stored {
		t.Fatalf("PutDigest = %v, %v; want true, nil", stored, err)
	}
	got, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Recap != d.Recap {
		t.Errorf("Recap = %q, want %q", got.Recap, d.Recap)
	}
	sum := got.Summary
	if sum == nil || sum.Git == nil || sum.Git.CommitCount != 3 || sum.Git.Unpushed != -1 ||
		sum.LatestLink == nil || sum.LatestLink.Number != "391" || sum.CostUSD == nil || *sum.CostUSD != 18.27 ||
		sum.OutputTokens != 371471 || !sum.AsOf.Equal(d.AsOf) {
		t.Errorf("Summary = %+v", sum)
	}
	det, err := s.SessionDetail(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if det.Digest == nil || len(det.Digest.Git.Commits) != 1 || det.Digest.Tokens.CacheWrite != 7 || !det.Digest.ReceivedAt.Equal(*now) {
		t.Errorf("Digest = %+v", det.Digest)
	}
	// One digest event, for the new recap and links.
	n := 0
	for _, e := range det.Events {
		if e.Kind == api.KindDigest {
			n++
		}
	}
	if n != 1 {
		t.Errorf("digest events = %d, want 1", n)
	}
}

func TestPutDigestStaleAndEqual(t *testing.T) {
	s, mid, now := digestFixture(t)
	ctx := context.Background()
	newer := validDigest(now.Add(-time.Minute))
	if _, err := s.PutDigest(ctx, mid, "sess-1", newer); err != nil {
		t.Fatal(err)
	}
	older := validDigest(now.Add(-time.Hour))
	older.Recap = "stale recap"
	stored, err := s.PutDigest(ctx, mid, "sess-1", older)
	if err != nil || stored {
		t.Fatalf("older digest: stored=%v err=%v, want false, nil", stored, err)
	}
	// The same digest again (hook child and watcher): stored, no new event.
	stored, err = s.PutDigest(ctx, mid, "sess-1", newer)
	if err != nil || !stored {
		t.Fatalf("equal digest: stored=%v err=%v, want true, nil", stored, err)
	}
	got, _ := s.GetSession(ctx, "sess-1")
	if got.Recap != newer.Recap {
		t.Errorf("Recap = %q, want the newer one", got.Recap)
	}
	det, _ := s.SessionDetail(ctx, "sess-1")
	n := 0
	for _, e := range det.Events {
		if e.Kind == api.KindDigest {
			n++
		}
	}
	if n != 1 {
		t.Errorf("digest events = %d, want 1", n)
	}
}

func TestPutDigestKeepsStoredGit(t *testing.T) {
	s, mid, now := digestFixture(t)
	ctx := context.Background()
	if _, err := s.PutDigest(ctx, mid, "sess-1", validDigest(now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	// An equal as_of and a newer one, both without git: git means unknown.
	for _, at := range []time.Duration{-time.Minute, -30 * time.Second} {
		d := validDigest(now.Add(at))
		d.Git = nil
		d.Recap = "later recap"
		if stored, err := s.PutDigest(ctx, mid, "sess-1", d); err != nil || !stored {
			t.Fatalf("PutDigest = %v, %v", stored, err)
		}
		got, _ := s.GetSession(ctx, "sess-1")
		if got.Summary == nil || got.Summary.Git == nil || got.Summary.Git.CommitCount != 3 {
			t.Fatalf("as_of %v: Summary.Git = %+v, want the stored git", at, got.Summary)
		}
		det, _ := s.SessionDetail(ctx, "sess-1")
		if det.Digest.Git == nil || len(det.Digest.Git.Commits) != 1 || det.Recap != "later recap" {
			t.Fatalf("as_of %v: detail = %+v", at, det.Digest)
		}
	}
}

func TestPutDigestKeepsEndedSession(t *testing.T) {
	s, mid, now := digestFixture(t)
	ctx := context.Background()
	if err := s.AddEvent(ctx, mid, "sess-1", api.EventIn{Kind: api.KindEnded, Source: api.SourceHooks}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetSession(ctx, "sess-1")
	*now = now.Add(time.Hour)
	if _, err := s.PutDigest(ctx, mid, "sess-1", validDigest(now.Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetSession(ctx, "sess-1")
	if after.Status != api.StatusEnded || after.EndedAt == nil || !after.LastSeenAt.Equal(before.LastSeenAt) {
		t.Errorf("after digest: status=%s ended=%v last_seen=%v; want ended, unchanged %v",
			after.Status, after.EndedAt, after.LastSeenAt, before.LastSeenAt)
	}
}

func TestPutDigestTitles(t *testing.T) {
	s, mid, now := digestFixture(t)
	ctx := context.Background()
	put := func(asOf time.Duration, ai, custom string) {
		t.Helper()
		d := validDigest(now.Add(asOf))
		d.AITitle, d.CustomTitle = ai, custom
		if _, err := s.PutDigest(ctx, mid, "sess-1", d); err != nil {
			t.Fatal(err)
		}
	}
	title := func() (string, string) {
		got, _ := s.GetSession(ctx, "sess-1")
		return got.Title, got.TitleSource
	}
	put(-50*time.Minute, "Fix CI tokens", "")
	if tt, src := title(); tt != "Fix CI tokens" || src != TitleSourceClaude {
		t.Fatalf("after ai-title: %q %q", tt, src)
	}
	put(-40*time.Minute, "Fix CI tokens", "ci-rotation")
	if tt, src := title(); tt != "ci-rotation" || src != TitleSourceUser {
		t.Fatalf("after custom-title: %q %q", tt, src)
	}
	if err := s.SetTitle(ctx, mid, "sess-1", "my own title"); err != nil {
		t.Fatal(err)
	}
	// The same custom title again must not overwrite the later set_title.
	put(-30*time.Minute, "Fix CI tokens", "ci-rotation")
	if tt, src := title(); tt != "my own title" || src != TitleSourceUser {
		t.Errorf("after repeated custom-title: %q %q, want the set_title", tt, src)
	}
	// A herdr title outranks the automatic title.
	if _, err := s.UpsertSession(ctx, mid, api.SessionUpsert{ID: "sess-2", Source: api.SourcePlugin, TitleHint: "herdr says"}); err != nil {
		t.Fatal(err)
	}
	d := validDigest(now.Add(-time.Minute))
	d.AITitle = "automatic"
	if _, err := s.PutDigest(ctx, mid, "sess-2", d); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSession(ctx, "sess-2")
	if got.Title != "herdr says" {
		t.Errorf("sess-2 title = %q, want the herdr title", got.Title)
	}
}

func TestPutDigestValidation(t *testing.T) {
	s, mid, now := digestFixture(t)
	ctx := context.Background()
	long := strings.Repeat("x", 601)
	neg := -1.0
	cases := map[string]func(d *api.DigestIn){
		"no as_of":           func(d *api.DigestIn) { d.AsOf = time.Time{} },
		"as_of in future":    func(d *api.DigestIn) { d.AsOf = now.Add(6 * time.Minute) },
		"recap too long":     func(d *api.DigestIn) { d.Recap = long },
		"recap newline":      func(d *api.DigestIn) { d.Recap = "a\nb" },
		"ai_title too long":  func(d *api.DigestIn) { d.AITitle = long[:201] },
		"javascript link":    func(d *api.DigestIn) { d.Links[0].URL = "javascript:alert(1)" },
		"http link":          func(d *api.DigestIn) { d.Links[0].URL = "http://example.com/1" },
		"link too long":      func(d *api.DigestIn) { d.Links[0].URL = "https://e.com/" + strings.Repeat("a", 300) },
		"link with userinfo": func(d *api.DigestIn) { d.Links[0].URL = "https://u:p@e.com/1" },
		"bad pr number":      func(d *api.DigestIn) { d.Links[0].Number = "12a" },
		"21 links": func(d *api.DigestIn) {
			for i := 0; i < 20; i++ {
				d.Links = append(d.Links, d.Links[0])
			}
		},
		"bad sha":          func(d *api.DigestIn) { d.Git.Commits[0].SHA = "XYZ" },
		"subject too long": func(d *api.DigestIn) { d.Git.Commits[0].Subject = long[:121] },
		"6 commits": func(d *api.DigestIn) {
			for i := 0; i < 5; i++ {
				d.Git.Commits = append(d.Git.Commits, d.Git.Commits[0])
			}
			d.Git.CommitCount = 6
		},
		"count below commits": func(d *api.DigestIn) { d.Git.CommitCount = 0 },
		"unpushed -2":         func(d *api.DigestIn) { d.Git.Unpushed = -2 },
		"negative tokens":     func(d *api.DigestIn) { d.Tokens.Output = -1 },
		"huge tokens":         func(d *api.DigestIn) { d.Tokens.CacheRead = 1_000_000_000_001 },
		"negative cost":       func(d *api.DigestIn) { d.CostUSD = &neg },
		"negative bad_lines":  func(d *api.DigestIn) { d.BadLines = -1 },
	}
	for name, mutate := range cases {
		d := validDigest(now.Add(-time.Minute))
		mutate(&d)
		_, err := s.PutDigest(ctx, mid, "sess-1", d)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	got, _ := s.GetSession(ctx, "sess-1")
	if got.Summary != nil {
		t.Errorf("an invalid digest was stored: %+v", got.Summary)
	}
	if _, err := s.PutDigest(ctx, mid, "nope", validDigest(now.Add(-time.Minute))); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v, want ErrNotFound", err)
	}
	tok, _, _ := s.AddMachine(ctx, "tower", "", "")
	other, _ := s.MachineByToken(ctx, tok)
	if _, err := s.PutDigest(ctx, other.ID, "sess-1", validDigest(now.Add(-time.Minute))); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("other machine: %v, want ErrWrongMachine", err)
	}
}

func TestLastPrompt(t *testing.T) {
	s, mid, now := digestFixture(t)
	ctx := context.Background()
	post := func(at time.Time, text string) {
		t.Helper()
		p, _ := json.Marshal(map[string]string{"prompt": text})
		if err := s.AddEvent(ctx, mid, "sess-1", api.EventIn{Kind: api.KindPrompt, Source: api.SourceHooks, TS: at, Payload: p}); err != nil {
			t.Fatal(err)
		}
	}
	post(now.Add(-10*time.Minute), "first")
	post(now.Add(-5*time.Minute), "second")
	post(now.Add(-8*time.Minute), "late arrival, older") // a queued event replayed late
	got, _ := s.GetSession(ctx, "sess-1")
	if got.LastPrompt != "second" || got.LastPromptAt == nil || !got.LastPromptAt.Equal(now.Add(-5*time.Minute)) {
		t.Errorf("LastPrompt = %q at %v, want second at -5m", got.LastPrompt, got.LastPromptAt)
	}
}

func TestMigrateV3FillsLastPrompt(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	tok, _, _ := s.AddMachine(ctx, "bluebox", "", "")
	m, _ := s.MachineByToken(ctx, tok)
	if _, err := s.UpsertSession(ctx, m.ID, api.SessionUpsert{ID: "sess-1", Source: api.SourceHooks}); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"older", "newest"} {
		p, _ := json.Marshal(map[string]string{"prompt": text})
		at := time.Date(2026, 9, 29, 10, i, 0, 0, time.UTC)
		if err := s.AddEvent(ctx, m.ID, "sess-1", api.EventIn{Kind: api.KindPrompt, Source: api.SourceHooks, TS: at, Payload: p}); err != nil {
			t.Fatal(err)
		}
	}
	// A malformed payload must not break the upgrade.
	if _, err := s.db.Exec(`INSERT INTO events (session_id, ts, source, kind, payload_json) VALUES ('sess-1', '2026-09-29T09:00:00.000000000Z', 'hooks', 'prompt', 'not json')`); err != nil {
		t.Fatal(err)
	}
	rollbackV4(t, s)
	if _, err := s.db.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastPrompt != "newest" || got.LastPromptAt == nil {
		t.Errorf("after upgrade: %q %v, want newest", got.LastPrompt, got.LastPromptAt)
	}
}

func TestPutDigestFirstCustomTitleDoesNotOverrideSetTitle(t *testing.T) {
	s, mid, now := digestFixture(t)
	ctx := context.Background()
	if err := s.SetTitle(ctx, mid, "sess-1", "my own title"); err != nil {
		t.Fatal(err)
	}
	put := func(asOf time.Duration, custom string) {
		t.Helper()
		d := validDigest(now.Add(asOf))
		d.CustomTitle = custom
		if _, err := s.PutDigest(ctx, mid, "sess-1", d); err != nil {
			t.Fatal(err)
		}
	}
	// The first digest carries an old /rename: the later set_title wins.
	put(-10*time.Minute, "old-rename")
	if got, _ := s.GetSession(ctx, "sess-1"); got.Title != "my own title" {
		t.Fatalf("after first digest: title %q, want the set_title", got.Title)
	}
	// A different custom title later is a new /rename.
	put(-5*time.Minute, "new-rename")
	if got, _ := s.GetSession(ctx, "sess-1"); got.Title != "new-rename" || got.TitleSource != TitleSourceUser {
		t.Fatalf("after a new rename: %q %q", got.Title, got.TitleSource)
	}
}
