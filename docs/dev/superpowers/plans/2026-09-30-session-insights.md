# Session insights implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every session a digest (recap, automatic title, merge request
links, tokens, cost, git activity) plus its last prompt, shown on the
dashboard and in the CLI, with a filter to find a session.

**Architecture:** A new package `internal/digest` reads each session's Claude
Code transcript incrementally and runs git, on the machine that owns the
session. `sessionhub digest <id>` sends the result with `PUT
/v1/sessions/{id}/digest`, queueing it when the server is down. The hooks
client starts `sessionhub digest` on `stop` and `session-end`, the herdr watcher runs
it on heartbeats, and `sessionhub digest --all` backfills. The server stores one
digest per session (schema v4) and adds summary fields to the session list.

**Tech Stack:** Go 1.25, `modernc.org/sqlite` v1.50.0 (JSON1 built in),
standard library only for new code. Dashboard: one embedded HTML page, no
libraries, tested under `node`.

**Spec:** `docs/dev/superpowers/specs/2026-09-30-session-insights-design.md`

## Global Constraints

- Claude's reply text never leaves the machine. No task reads
  `last_assistant_message` or assistant `message.content`.
- Prompt text collection is unchanged.
- Limits: recap 600 characters; titles 200; commit subjects 120; at most 5
  commits; at most 20 links; link URL `https://`, at most 300 bytes; digest
  body at most 16 KB (`413` above); counts and tokens 0 to 10^12; `unpushed`
  may be `-1`; `cost_usd` 0 to 100000; `as_of` at most 5 minutes in the
  future.
- Title precedence, highest first: `user` (set_title or `custom-title`),
  `herdr`, `claude` (`ai-title`), `prompt`.
- A digest never changes `last_seen_at` or `ended_at`.
- Transcript lines over 1 MB are skipped and counted as bad. More than half
  bad lines in one run: send nothing, keep the old state.
- Every dashboard string is inserted with `textContent`. Links are rendered
  only when they match `^https://`.
- Follow `docs/dev/DEFINITION-OF-DONE.md`: `make test` and `make lint` green after
  every commit, docs in the same commit, evidence for live behavior.
- Commit trailer on every commit:
  `Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h`.
- The CLI's list command is `sessionhub ls`, not `sessionhub list`: the spec's
  `sessionhub list --grep` is `sessionhub ls --grep`.

## Review Focus

1. **Backfilled digest for an ended session.** `sessionhub digest --all` sends
   digests for dozens of ended sessions; none of them may turn live. Pinned in
   Task 1 (`TestPutDigestKeepsEndedSession`).
2. **Recap with newlines.** Real recaps can span lines; the server rejects
   control characters, so the client must fold them or the recap is lost.
   Pinned in Task 3 (`newlines folded` case).
3. **`/rename` versus `set_title`.** Re-applying `custom-title` on every
   digest would overwrite a later `set_title`. Pinned in Task 1
   (`TestPutDigestTitles`).
4. **The hook child and the watcher send the same digest.** Equal `as_of` must
   store without error and without a second `digest` event. Pinned in Task 1
   (`TestPutDigestStaleAndEqual`).
5. **A repository with no commits yet** (unborn `HEAD`). Git must return nil,
   not panic or send zero counts. Pinned in Task 4 (`unborn HEAD` case).

## Dependencies and parallelism

| Task | Needs | Can run alongside |
|---|---|---|
| 1 API types and store | none | none |
| 2 Server endpoint | 1 | 3, 4, 7, 8 |
| 3 Transcript reader | 1 | 2, 4, 7, 8 |
| 4 Git summary | 1 | 2, 3, 7, 8 |
| 5 `sessionhub digest` command | 2, 3, 4 | 7, 8 |
| 6 Triggers (hooks, watcher) | 5 | 7, 8 |
| 7 CLI `ls --grep`, `show` | 1 | 2 to 6, 8 |
| 8 Dashboard | 1 | 2 to 7 |
| 9 Deploy, backfill, evidence | all | none |

No two tasks that may run together edit the same file.

---

### Task 1: API types and store

**Files:**
- Modify: `internal/api/types.go`
- Modify: `internal/store/store.go` (schema v4, migration)
- Create: `internal/store/digest.go` (validation, `PutDigest`, summary)
- Modify: `internal/store/sessions.go` (title rank, last prompt in `AddEvent`)
- Modify: `internal/store/read.go` (select and scan the new columns, detail)
- Modify: `internal/store/concurrency_test.go` (`rollbackV3` also removes v4)
- Create: `internal/store/digest_test.go`
- Modify: `docs/server.md` (schema v4 section)

**Interfaces:**
- Produces: `api.DigestIn`, `api.Digest`, `api.DigestLink`,
  `api.DigestTokens`, `api.DigestCommit`, `api.GitCounts`, `api.DigestGit`,
  `api.SessionSummary`, `api.KindDigest`; new `api.Session` fields `Recap`,
  `RecapAt`, `LastPrompt`, `LastPromptAt`, `Summary`; new
  `api.SessionDetail.Digest`.
- Produces: `func (s *Store) PutDigest(ctx context.Context, machineID int64,
  id string, d api.DigestIn) (stored bool, err error)`,
  `store.TitleSourceClaude`, `store.MaxRecapRunes = 600`,
  `store.MaxSubjectRunes = 120`, `store.MaxDigestLinks = 20`,
  `store.MaxDigestCommits = 5`, `store.MaxLinkBytes = 300`.

- [ ] **Step 1: Add the API types**

In `internal/api/types.go`, add `KindDigest` to the first const block:

```go
	KindEnded        = "ended"
	// KindDigest is recorded by the server when a digest changes the recap
	// or the set of links.
	KindDigest = "digest"
```

Add to `Session`, after `Controllable`:

```go
	// Recap and RecapAt are Claude's latest recap, from the digest.
	Recap   string     `json:"recap,omitempty"`
	RecapAt *time.Time `json:"recap_at,omitempty"`
	// LastPrompt and LastPromptAt are the newest prompt event's text (the
	// first 200 characters the hooks client sends) and time.
	LastPrompt   string     `json:"last_prompt,omitempty"`
	LastPromptAt *time.Time `json:"last_prompt_at,omitempty"`
	// Summary is the digest's counts for the card; nil with no digest.
	Summary *SessionSummary `json:"summary,omitempty"`
```

Add to `SessionDetail`:

```go
	Digest  *Digest  `json:"digest,omitempty"` // nil until a machine sends one
```

Append the digest types:

```go
// DigestLink is one merge request or pull request a session created.
type DigestLink struct {
	Number string `json:"number,omitempty"`
	URL    string `json:"url"`
	Repo   string `json:"repo,omitempty"`
}

// DigestTokens sums the usage of every assistant reply, each counted once,
// including subagents.
type DigestTokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// DigestCommit is one commit made during the session.
type DigestCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
}

// GitCounts are the git numbers the card shows. Unpushed is -1 when the
// branch has no upstream.
type GitCounts struct {
	CommitCount int `json:"commit_count"`
	Uncommitted int `json:"uncommitted"`
	Unpushed    int `json:"unpushed"`
}

// DigestGit is the git activity in the session's window.
type DigestGit struct {
	GitCounts
	Commits []DigestCommit `json:"commits"` // newest first, at most 5
}

// DigestIn is the body of PUT /v1/sessions/{id}/digest: everything a machine
// learned about a session from its transcript and git. Each digest replaces
// the previous one.
type DigestIn struct {
	AsOf        time.Time    `json:"as_of"` // the last transcript entry's time
	FirstAt     *time.Time   `json:"first_at,omitempty"`
	Recap       string       `json:"recap,omitempty"`
	RecapAt     *time.Time   `json:"recap_at,omitempty"`
	AITitle     string       `json:"ai_title,omitempty"`
	CustomTitle string       `json:"custom_title,omitempty"`
	Links       []DigestLink `json:"links,omitempty"`
	Tokens      DigestTokens `json:"tokens"`
	CostUSD     *float64     `json:"cost_usd,omitempty"`
	CostAt      *time.Time   `json:"cost_at,omitempty"`
	Git         *DigestGit   `json:"git,omitempty"`
	BadLines    int          `json:"bad_lines"`
}

// Digest is a stored digest and when the server received it.
type Digest struct {
	DigestIn
	ReceivedAt time.Time `json:"received_at"`
}

// SessionSummary is the part of the digest the session list carries.
type SessionSummary struct {
	AsOf         time.Time   `json:"as_of"`
	Git          *GitCounts  `json:"git,omitempty"`
	LatestLink   *DigestLink `json:"latest_link,omitempty"`
	CostUSD      *float64    `json:"cost_usd,omitempty"`
	OutputTokens int64       `json:"output_tokens"`
}
```

- [ ] **Step 2: Write the failing store tests**

Create `internal/store/digest_test.go`:

```go
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
		AsOf:   asOf,
		Recap:  "We fixed the review notes. Next, merge MR 391.",
		Links:  []api.DigestLink{{Number: "391", URL: "https://git.example.com/org/repo/-/merge_requests/391", Repo: "org/repo"}},
		Tokens: api.DigestTokens{Input: 10, Output: 371471, CacheRead: 5, CacheWrite: 7},
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
```

In `internal/store/concurrency_test.go`, add `rollbackV4` and call it at the
start of `rollbackV3`, so the existing v1 and v2 upgrade tests keep working:

```go
// rollbackV4 removes what schema v4 added, so a test can set user_version
// to 3 or lower and reopen the file as an older release left it.
func rollbackV4(t *testing.T, s *Store) {
	t.Helper()
	for _, q := range []string{
		"DROP TABLE session_digests",
		"ALTER TABLE sessions DROP COLUMN last_prompt",
		"ALTER TABLE sessions DROP COLUMN last_prompt_at",
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}
```

```go
func rollbackV3(t *testing.T, s *Store) {
	t.Helper()
	rollbackV4(t, s)
	for _, q := range []string{
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `go test ./internal/store/ -run 'Digest|LastPrompt|MigrateV3' -count=1`
Expected: build failure: `s.PutDigest undefined`, `TitleSourceClaude undefined`.

- [ ] **Step 4: Add schema v4**

In `internal/store/store.go`, set `const schemaVersion = 4` and add after
`schemaV3`:

```go
// schemaV4 adds session digests (one row per session, replaced by newer
// ones) and the session's last prompt, filled once from existing prompt
// events. A malformed payload is skipped rather than failing the upgrade.
const schemaV4 = `
CREATE TABLE session_digests (
	session_id  TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
	as_of       TEXT NOT NULL,   -- the digest's last transcript entry; newer wins
	received_at TEXT NOT NULL,
	body        TEXT NOT NULL    -- api.DigestIn as JSON, validated
);
ALTER TABLE sessions ADD COLUMN last_prompt TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN last_prompt_at TEXT;
UPDATE sessions SET
	last_prompt = COALESCE((SELECT json_extract(CASE WHEN json_valid(e.payload_json) THEN e.payload_json END, '$.prompt')
		FROM events e
		WHERE e.session_id = sessions.id AND e.kind = 'prompt'
			AND json_type(CASE WHEN json_valid(e.payload_json) THEN e.payload_json END, '$.prompt') = 'text'
		ORDER BY e.ts DESC, e.id DESC LIMIT 1), ''),
	last_prompt_at = (SELECT e.ts FROM events e
		WHERE e.session_id = sessions.id AND e.kind = 'prompt'
			AND json_type(CASE WHEN json_valid(e.payload_json) THEN e.payload_json END, '$.prompt') = 'text'
		ORDER BY e.ts DESC, e.id DESC LIMIT 1);
`
```

The `CASE` wrappers matter: SQLite doesn't promise to evaluate
`json_valid(x) AND json_type(x, ...)` left to right, and `json_type` on
malformed JSON is an error that would abort the whole upgrade. With the
`CASE`, malformed payloads become `NULL`, and `json_type(NULL, ...)` is
`NULL`.

In `migrate`, after the `v < 3` block:

```go
	if v < 4 {
		if _, err := tx.ExecContext(ctx, schemaV4); err != nil {
			return fmt.Errorf("migrate schema to v4: %w", err)
		}
	}
```

Check `TestOpenPragmasAndSchema` in `store_test.go`: if it lists tables or
columns, add `session_digests`, `last_prompt`, and `last_prompt_at` there.

- [ ] **Step 5: Rank the `claude` title source**

In `internal/store/sessions.go`, replace the title source block and
`titleRank`:

```go
// Title sources, from lowest to highest precedence.
const (
	TitleSourcePrompt = "prompt"
	TitleSourceClaude = "claude" // Claude Code's automatic title (ai-title), from a digest
	TitleSourceHerdr  = "herdr"
	TitleSourceUser   = "user"
)

// titleRank orders title sources; a lower rank never replaces a higher one.
func titleRank(src string) int {
	switch src {
	case TitleSourceUser:
		return 4
	case TitleSourceHerdr:
		return 3
	case TitleSourceClaude:
		return 2
	case TitleSourcePrompt:
		return 1
	}
	return 0
}
```

Leave `schemaV1` as it is (it records the historical schema). Add a line to
the `schemaV4` doc comment: `sessions.title_source` also takes `claude`.

- [ ] **Step 6: Record the last prompt in `AddEvent`**

In `AddEvent`, after the `state_changed` block, parse the prompt:

```go
	// A prompt event also sets the session's last prompt. A payload sessionhub
	// can't use is still stored as an event, as before.
	var prompt string
	if e.Kind == api.KindPrompt && len(payload) > 0 {
		var p struct {
			Prompt string `json:"prompt"`
		}
		if json.Unmarshal(payload, &p) == nil && p.Prompt != "" && checkText("prompt", p.Prompt, MaxTitleRunes) == nil {
			prompt = p.Prompt
		}
	}
```

After the `if state != ""` block, before `args = append(args, id)`:

```go
	if prompt != "" {
		// SQLite evaluates every SET expression on the old row, so both
		// columns compare against the stored time. An older event replayed
		// late leaves them alone.
		q += `, last_prompt = CASE WHEN last_prompt_at IS NULL OR last_prompt_at <= ? THEN ? ELSE last_prompt END,
			last_prompt_at = CASE WHEN last_prompt_at IS NULL OR last_prompt_at <= ? THEN ? ELSE last_prompt_at END`
		args = append(args, formatTS(ts), prompt, formatTS(ts), formatTS(ts))
	}
```

- [ ] **Step 7: Write `internal/store/digest.go`**

```go
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// Digest limits. See docs/server.md, "Session digests".
const (
	MaxRecapRunes    = 600
	MaxSubjectRunes  = 120
	MaxDigestLinks   = 20
	MaxDigestCommits = 5
	MaxLinkBytes     = 300

	maxDigestCount   = 1_000_000_000_000
	maxCostUSD       = 100000
	digestFutureSkew = 5 * time.Minute
)

var (
	shaRE      = regexp.MustCompile(`^[0-9a-f]{4,40}$`)
	prNumberRE = regexp.MustCompile(`^[0-9]{1,10}$`)
)

func checkCount(name string, v, lo int64) error {
	if v < lo || v > maxDigestCount {
		return invalidf("%s is %d, want %d to %d", name, v, lo, int64(maxDigestCount))
	}
	return nil
}

func checkLink(name string, l api.DigestLink) error {
	if len(l.URL) > MaxLinkBytes {
		return invalidf("%s.url is %d bytes, at most %d allowed", name, len(l.URL), MaxLinkBytes)
	}
	if err := checkNoControl(name+".url", l.URL); err != nil {
		return err
	}
	u, err := url.Parse(l.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return invalidf("%s.url %q: want an https:// URL without user info", name, l.URL)
	}
	if l.Number != "" && !prNumberRE.MatchString(l.Number) {
		return invalidf("%s.number %q: want 1-10 digits", name, l.Number)
	}
	return checkText(name+".repo", l.Repo, MaxTitleRunes)
}

func validateDigest(d api.DigestIn, now time.Time) error {
	if d.AsOf.IsZero() {
		return invalidf("as_of is required")
	}
	if d.AsOf.After(now.Add(digestFutureSkew)) {
		return invalidf("as_of %s is more than %s ahead of the server clock", d.AsOf.UTC().Format(time.RFC3339), digestFutureSkew)
	}
	for _, f := range []struct {
		name, v string
		max     int
	}{{"recap", d.Recap, MaxRecapRunes}, {"ai_title", d.AITitle, MaxTitleRunes}, {"custom_title", d.CustomTitle, MaxTitleRunes}} {
		if err := checkText(f.name, f.v, f.max); err != nil {
			return err
		}
	}
	if len(d.Links) > MaxDigestLinks {
		return invalidf("links has %d items, at most %d allowed", len(d.Links), MaxDigestLinks)
	}
	for i, l := range d.Links {
		if err := checkLink(fmt.Sprintf("links[%d]", i), l); err != nil {
			return err
		}
	}
	for _, c := range []struct {
		name string
		v    int64
	}{{"tokens.input", d.Tokens.Input}, {"tokens.output", d.Tokens.Output},
		{"tokens.cache_read", d.Tokens.CacheRead}, {"tokens.cache_write", d.Tokens.CacheWrite},
		{"bad_lines", int64(d.BadLines)}} {
		if err := checkCount(c.name, c.v, 0); err != nil {
			return err
		}
	}
	if d.CostUSD != nil && (*d.CostUSD < 0 || *d.CostUSD > maxCostUSD) {
		return invalidf("cost_usd is %v, want 0 to %d", *d.CostUSD, maxCostUSD)
	}
	if g := d.Git; g != nil {
		if len(g.Commits) > MaxDigestCommits {
			return invalidf("git.commits has %d items, at most %d allowed", len(g.Commits), MaxDigestCommits)
		}
		for _, c := range []struct {
			name  string
			v, lo int64
		}{{"git.commit_count", int64(g.CommitCount), int64(len(g.Commits))},
			{"git.uncommitted", int64(g.Uncommitted), 0}, {"git.unpushed", int64(g.Unpushed), -1}} {
			if err := checkCount(c.name, c.v, c.lo); err != nil {
				return err
			}
		}
		for i, c := range g.Commits {
			if !shaRE.MatchString(c.SHA) {
				return invalidf("git.commits[%d].sha %q: want 4-40 lowercase hex digits", i, c.SHA)
			}
			if err := checkText(fmt.Sprintf("git.commits[%d].subject", i), c.Subject, MaxSubjectRunes); err != nil {
				return err
			}
		}
	}
	return nil
}

func sameLinks(a, b []api.DigestLink) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// PutDigest stores session id's digest unless the stored one has a newer
// as_of; then it returns stored=false and changes nothing. An equal as_of is
// stored again, so the hook child and the watcher can both send one.
//
// It never touches last_seen_at or ended_at: a backfilled digest for an
// ended session must not revive it.
func (s *Store) PutDigest(ctx context.Context, machineID int64, id string, d api.DigestIn) (stored bool, err error) {
	now := s.Now()
	if err := validateDigest(d, now); err != nil {
		return false, err
	}
	d.AsOf = d.AsOf.UTC()
	body, err := json.Marshal(d)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := checkOwnerTx(ctx, tx, machineID, id); err != nil {
		return false, err
	}
	var prevAsOf, prevBody string
	var prev api.DigestIn
	err = tx.QueryRowContext(ctx, `SELECT as_of, body FROM session_digests WHERE session_id = ?`, id).Scan(&prevAsOf, &prevBody)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	default:
		if formatTS(d.AsOf) < prevAsOf {
			return false, nil
		}
		if err := json.Unmarshal([]byte(prevBody), &prev); err != nil {
			return false, fmt.Errorf("stored digest of %s: %w", id, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_digests (session_id, as_of, received_at, body)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET as_of = excluded.as_of, received_at = excluded.received_at, body = excluded.body`,
		id, formatTS(d.AsOf), formatTS(now), string(body)); err != nil {
		return false, err
	}
	if err := applyDigestTitle(ctx, tx, id, d, prev); err != nil {
		return false, err
	}
	if d.Recap != prev.Recap || !sameLinks(d.Links, prev.Links) {
		payload, _ := json.Marshal(struct {
			RecapAt *time.Time `json:"recap_at,omitempty"`
			Links   int        `json:"links"`
		}{d.RecapAt, len(d.Links)})
		if err := insertEventTx(ctx, tx, id, now, api.SourceServer, api.KindDigest, payload); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// applyDigestTitle applies the digest's titles. A custom title (from
// /rename) is applied only when it differs from the previous digest's, so a
// later set_title keeps winning. The automatic title fills in below herdr and
// user titles.
func applyDigestTitle(ctx context.Context, tx *sql.Tx, id string, d, prev api.DigestIn) error {
	var cur string
	if err := tx.QueryRowContext(ctx, `SELECT title_source FROM sessions WHERE id = ?`, id).Scan(&cur); err != nil {
		return err
	}
	var title, src string
	switch {
	case d.CustomTitle != "" && d.CustomTitle != prev.CustomTitle:
		title, src = d.CustomTitle, TitleSourceUser
	case d.AITitle != "" && titleRank(cur) <= titleRank(TitleSourceClaude):
		title, src = d.AITitle, TitleSourceClaude
	default:
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET title = ?, title_source = ? WHERE id = ?`, title, src, id)
	return err
}

// summarize is the part of a digest the session list carries.
func summarize(d api.DigestIn) *api.SessionSummary {
	sum := &api.SessionSummary{AsOf: d.AsOf, CostUSD: d.CostUSD, OutputTokens: d.Tokens.Output}
	if d.Git != nil {
		g := d.Git.GitCounts
		sum.Git = &g
	}
	if n := len(d.Links); n > 0 {
		l := d.Links[n-1]
		sum.LatestLink = &l
	}
	return sum
}
```

- [ ] **Step 8: Read the new columns**

In `internal/store/read.go`, extend `sessionSelect`: add
`, s.last_prompt, s.last_prompt_at, dg.body` to the end of the column list
(after `c.detail`) and add the join after the `control_requests` join:

```go
LEFT JOIN session_digests dg ON dg.session_id = s.id`
```

In `scanSession`, declare `var lastPromptAt, digestBody sql.NullString`, append
`&x.LastPrompt, &lastPromptAt, &digestBody` to the end of the `Scan` call, and
before `return x, nil` add:

```go
	if x.LastPromptAt, err = parseNullTS(lastPromptAt); err != nil {
		return x, err
	}
	if digestBody.Valid {
		var d api.DigestIn
		if err := json.Unmarshal([]byte(digestBody.String), &d); err != nil {
			return x, fmt.Errorf("stored digest of %s: %w", x.ID, err)
		}
		x.Recap, x.RecapAt = d.Recap, d.RecapAt
		x.Summary = summarize(d)
	}
```

In `SessionDetail`, after `d.Events = []api.Event{}`:

```go
	var body, received string
	err = s.db.QueryRowContext(ctx, `SELECT body, received_at FROM session_digests WHERE session_id = ?`, id).Scan(&body, &received)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return d, err
	default:
		dg := &api.Digest{}
		if err := json.Unmarshal([]byte(body), &dg.DigestIn); err != nil {
			return d, fmt.Errorf("stored digest of %s: %w", id, err)
		}
		if dg.ReceivedAt, err = parseTS(received); err != nil {
			return d, err
		}
		d.Digest = dg
	}
```

- [ ] **Step 9: Run the store tests**

Run: `go test ./internal/store/ -count=1`
Expected: `ok`. Every existing test passes, including the v1 and v2 upgrade
tests that now also roll back v4.

- [ ] **Step 10: Document schema v4**

In `docs/server.md`, add a "Session digests" section: the table, the
`last_prompt` columns and their one-time fill, the `claude` title source and
its rank, the stale and equal `as_of` rules, the rule that a digest never
changes `last_seen_at` or `ended_at`, and the rollback note: to roll back to
the v3 binary, restore the pre-deploy backup, or run
`PRAGMA user_version=3` (the v3 binary ignores the extra table and columns);
run `PRAGMA user_version=4` before upgrading again.

- [ ] **Step 11: Run the suite and commit**

Run: `make test lint`
Expected: every package `ok`, `gofmt -l` prints nothing, `go vet` clean.

```bash
git add internal/api/types.go internal/store docs/server.md
git commit -m "Store session digests and the last prompt (schema v4)" -m "Refs: docs/dev/superpowers/plans/2026-09-30-session-insights.md, Task 1"
```

Add the two trailer lines from Global Constraints to every commit message.

---

### Task 2: Server endpoint

**Files:**
- Modify: `internal/server/routes.go`
- Modify: `internal/server/handlers.go`
- Modify: `internal/server/auth_test.go`
- Create: `internal/server/digest_test.go`
- Modify: `docs/server.md` (endpoint)

**Interfaces:**
- Consumes: `store.PutDigest`, `api.DigestIn` (Task 1).
- Produces: `PUT /v1/sessions/{id}/digest` (machine token; `200` with the
  session; `400`, `404`, `409`, `413`); `server.MaxDigestBytes = 16 << 10`.

- [ ] **Step 1: Write the failing tests**

Add a case to `cases` in `TestAuthMatrix` (`internal/server/auth_test.go`):

```go
		"PUT /v1/sessions/{id}/digest": {path: "/v1/sessions/" + sid1 + "/digest",
			body: api.DigestIn{AsOf: time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)}, ok: 200, okB: 409},
```

Add `"time"` to the imports if it's missing.

Create `internal/server/digest_test.go`:

```go
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestPutDigestEndpoint(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	asOf := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	d := api.DigestIn{AsOf: asOf, Recap: "Recap text.",
		Links: []api.DigestLink{{Number: "7", URL: "https://github.com/o/r/pull/7"}}}

	code, body, _ := e.do("PUT", "/v1/sessions/"+sid1+"/digest", e.tokA, d)
	if code != http.StatusOK {
		t.Fatalf("PUT digest = %d %s", code, body)
	}
	var s api.Session
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatal(err)
	}
	if s.Recap != "Recap text." || s.Summary == nil || s.Summary.LatestLink == nil {
		t.Errorf("response session = %+v", s)
	}

	// The list carries the summary, the detail carries the digest.
	code, body, _ = e.do("GET", "/v1/sessions/"+sid1, e.tokA, nil)
	var det api.SessionDetail
	if code != 200 || json.Unmarshal(body, &det) != nil || det.Digest == nil || det.Digest.Links[0].Number != "7" {
		t.Errorf("GET detail = %d %s", code, body)
	}

	// Wrong paths.
	for name, tc := range map[string]struct {
		path string
		body any
		want int
	}{
		"unknown session": {"/v1/sessions/nope/digest", d, 404},
		"javascript link": {"/v1/sessions/" + sid1 + "/digest",
			api.DigestIn{AsOf: asOf, Links: []api.DigestLink{{URL: "javascript:alert(1)"}}}, 400},
		"no as_of":  {"/v1/sessions/" + sid1 + "/digest", api.DigestIn{}, 400},
		"not json":  {"/v1/sessions/" + sid1 + "/digest", json.RawMessage(`{`), 400},
		"oversize":  {"/v1/sessions/" + sid1 + "/digest", map[string]string{"recap": strings.Repeat("x", 17<<10)}, 413},
	} {
		code, body, _ := e.do("PUT", tc.path, e.tokA, tc.body)
		if code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, code, body, tc.want)
		}
	}
	// Another machine's token: 409, and the digest is unchanged.
	code, _, _ = e.do("PUT", "/v1/sessions/"+sid1+"/digest", e.tokB, api.DigestIn{AsOf: asOf.Add(time.Minute), Recap: "hijack"})
	if code != http.StatusConflict {
		t.Errorf("other machine = %d, want 409", code)
	}
	_, body, _ = e.do("GET", "/v1/sessions/"+sid1, e.tokA, nil)
	json.Unmarshal(body, &det)
	if det.Recap != "Recap text." {
		t.Errorf("recap after refused writes = %q", det.Recap)
	}
}
```

If `e.do` can't send a `json.RawMessage` as a raw body, check how
`helpers_test.go` marshals `body` (`doHdr`) and pass the malformed body the
way existing "invalid JSON" tests do; search `invalid JSON` in
`internal/server/*_test.go`.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/server/ -run 'AuthMatrix|PutDigestEndpoint' -count=1`
Expected: FAIL: the auth case has no route (`405` or `404`), and
`TestPutDigestEndpoint` gets `405`.

- [ ] **Step 3: Add the route and handler**

In `routes()`, after the `title` route:

```go
		{"PUT", "/v1/sessions/{id}/digest", accessWrite, s.writer(s.putDigest)},
```

In `internal/server/handlers.go`:

```go
// MaxDigestBytes caps a digest body, below the 64 KiB cap on every body.
const MaxDigestBytes = 16 << 10

func (s *Server) putDigest(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxDigestBytes))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "digest body is larger than 16 KiB")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var in api.DigestIn
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if _, err := s.store.PutDigest(r.Context(), p.machine.ID, id, in); err != nil {
		s.storeError(w, err)
		return
	}
	s.respondSession(w, r, id, http.StatusOK)
}
```

Add `errors`, `io`, and `encoding/json` imports if `handlers.go` lacks them.

- [ ] **Step 4: Run the server tests**

Run: `go test ./internal/server/ -count=1`
Expected: `ok`.

- [ ] **Step 5: Document the endpoint**

In `docs/server.md`, add `PUT /v1/sessions/{id}/digest` to the endpoint list
with its auth, body (copy the JSON example from the spec), validation limits,
status codes, and the stale rule. Add the new list and detail fields.

- [ ] **Step 6: Run the suite and commit**

Run: `make test lint`

```bash
git add internal/server docs/server.md
git commit -m "Accept session digests at PUT /v1/sessions/{id}/digest" -m "Refs: docs/dev/superpowers/plans/2026-09-30-session-insights.md, Task 2"
```

---

### Task 3: Transcript reader

**Files:**
- Create: `internal/digest/transcript.go` (parsing, incremental reads)
- Create: `internal/digest/state.go` (state file, lock, `Reader`)
- Create: `internal/digest/transcript_test.go`
- Create: `internal/digest/testdata/` fixtures (written by the test itself; no
  real transcript text)

**Interfaces:**
- Consumes: `api.DigestIn`, `api.DigestLink`, `api.DigestTokens` (Task 1);
  `termtext.Clean(s string, max int) string` from
  `session-hub/internal/cli/termtext` (collapses whitespace, drops control and
  bidi characters, cuts to `max` runes with `…`).
- Produces:
  - `type Reader struct { ClaudeDir, StateDir string }`
  - `func (r Reader) Read(id string) (*State, error)`
  - `func (st *State) DigestIn() (api.DigestIn, bool)`
  - `func TranscriptPath(claudeDir, id string) (string, error)`
  - `func DefaultClaudeDir() string`, `func DefaultStateDir() string`
  - `State` fields used by later tasks: `CWD string`, `FirstAt, AsOf *time.Time`
  - Errors: `ErrNoTranscript`, `ErrLocked`, `ErrTooManyBad`

- [ ] **Step 1: Write the failing tests**

Create `internal/digest/transcript_test.go`. The tests build transcripts from
JSON lines, so no real conversation text is committed.

```go
package digest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sid = "11111111-2222-3333-4444-555555555555"

// line marshals one transcript entry.
func line(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func ts(min int) string {
	return time.Date(2026, 9, 30, 10, min, 0, 0, time.UTC).Format(time.RFC3339Nano)
}

func assistant(t *testing.T, id string, min int, out int) string {
	return line(t, map[string]any{"type": "assistant", "timestamp": ts(min), "cwd": "/work/repo",
		"message": map[string]any{"id": id, "content": []any{map[string]any{"type": "text", "text": "reply text"}},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": out, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 10}}})
}

// setup makes <claude>/projects/-work-repo/<sid>.jsonl with content and
// returns a Reader over temp dirs and the transcript path.
func setup(t *testing.T, content string) (Reader, string) {
	t.Helper()
	claude := t.TempDir()
	dir := filepath.Join(claude, "projects", "-work-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return Reader{ClaudeDir: claude, StateDir: t.TempDir()}, p
}

func appendTo(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestReadExtractsFields(t *testing.T) {
	content := line(t, map[string]any{"type": "user", "timestamp": ts(0), "cwd": "/work/repo",
		"message": map[string]any{"role": "user", "content": "do the thing"}}) +
		// Claude Code writes one reply several times with the same message ID.
		assistant(t, "msg_1", 1, 50) + assistant(t, "msg_1", 1, 50) + assistant(t, "msg_2", 2, 25) +
		line(t, map[string]any{"type": "ai-title", "aiTitle": "First title"}) +
		line(t, map[string]any{"type": "ai-title", "aiTitle": "Better title"}) +
		line(t, map[string]any{"type": "system", "subtype": "away_summary", "timestamp": ts(3),
			"content": "We did X.\nNext, do Y. (disable recaps in /config)"}) +
		line(t, map[string]any{"type": "pr-link", "prNumber": 391, "prUrl": "https://git.example.com/o/r/-/merge_requests/391",
			"prRepository": "o/r", "timestamp": ts(4)}) +
		line(t, map[string]any{"type": "pr-link", "prNumber": "391", "prUrl": "https://git.example.com/o/r/-/merge_requests/391"}) +
		line(t, map[string]any{"type": "pr-link", "prNumber": 5, "prUrl": "javascript:alert(1)"}) +
		line(t, map[string]any{"type": "some-future-type", "whatever": true}) +
		line(t, map[string]any{"type": "cost-state", "totalCostUSD": 1.25}) +
		line(t, map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": ts(5)})
	r, _ := setup(t, content)
	st, err := r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := st.DigestIn()
	if !ok {
		t.Fatal("DigestIn not ok")
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"output tokens (msg_1 once)", d.Tokens.Output, int64(75)},
		{"cache read", d.Tokens.CacheRead, int64(200)},
		{"ai title", d.AITitle, "Better title"},
		{"recap: newlines folded, suffix removed", d.Recap, "We did X. Next, do Y."},
		{"recap at", d.RecapAt.Format(time.RFC3339), "2026-09-30T10:03:00Z"},
		{"links (deduplicated, javascript dropped)", len(d.Links), 1},
		{"link number from a JSON number", d.Links[0].Number, "391"},
		{"cost", *d.CostUSD, 1.25},
		{"cost at: the preceding timed entry", d.CostAt.Format(time.RFC3339), "2026-09-30T10:04:00Z"},
		{"first at", d.FirstAt.Format(time.RFC3339), "2026-09-30T10:00:00Z"},
		{"as of", d.AsOf.Format(time.RFC3339), "2026-09-30T10:05:00Z"},
		{"cwd", st.CWD, "/work/repo"},
		{"bad lines", d.BadLines, 0},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	// The reply text must never reach the digest.
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "reply text") {
		t.Errorf("digest contains reply text: %s", b)
	}
}

func TestReadIsIncremental(t *testing.T) {
	r, p := setup(t, assistant(t, "msg_1", 1, 10))
	if _, err := r.Read(sid); err != nil {
		t.Fatal(err)
	}
	// A partial line: written without its newline yet.
	partial := assistant(t, "msg_2", 2, 20)
	appendTo(t, p, partial[:len(partial)/2])
	st, err := r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tokens.Output != 10 || st.BadLines != 0 {
		t.Fatalf("after partial line: output=%d bad=%d, want 10, 0", st.Tokens.Output, st.BadLines)
	}
	appendTo(t, p, partial[len(partial)/2:])
	st, err = r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tokens.Output != 30 {
		t.Errorf("after the line completed: output=%d, want 30", st.Tokens.Output)
	}
	// Reading again with nothing new changes nothing.
	st, _ = r.Read(sid)
	if st.Tokens.Output != 30 {
		t.Errorf("idle read: output=%d, want 30", st.Tokens.Output)
	}
}

func TestReadStartsOverWhenReplaced(t *testing.T) {
	r, p := setup(t, assistant(t, "msg_1", 1, 10)+assistant(t, "msg_2", 2, 10))
	if _, err := r.Read(sid); err != nil {
		t.Fatal(err)
	}
	// Shrunk: a new, shorter file at the same path.
	if err := os.WriteFile(p+".new", []byte(assistant(t, "msg_9", 3, 7)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p+".new", p); err != nil {
		t.Fatal(err)
	}
	st, err := r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tokens.Output != 7 {
		t.Errorf("after replace: output=%d, want 7 (read from the start)", st.Tokens.Output)
	}
}

func TestReadSubagents(t *testing.T) {
	r, p := setup(t, assistant(t, "msg_1", 1, 10))
	sub := filepath.Join(strings.TrimSuffix(p, ".jsonl"), "subagents")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	// A subagent's recap-like entry must not become the session's recap.
	subContent := assistant(t, "msg_s1", 1, 5) + line(t, map[string]any{"type": "system", "subtype": "away_summary", "content": "subagent"})
	if err := os.WriteFile(filepath.Join(sub, "agent-a.jsonl"), []byte(subContent), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tokens.Output != 15 || st.Recap != "" {
		t.Errorf("output=%d recap=%q, want 15 and no recap", st.Tokens.Output, st.Recap)
	}
}

func TestReadBadLines(t *testing.T) {
	long := `{"type":"user","x":"` + strings.Repeat("a", 1<<20) + `"}` + "\n"
	r, p := setup(t, assistant(t, "msg_1", 1, 10)+"not json\n"+long+assistant(t, "msg_2", 2, 10)+assistant(t, "msg_3", 3, 10))
	st, err := r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.BadLines != 2 || st.Tokens.Output != 30 {
		t.Errorf("bad=%d output=%d, want 2 and 30", st.BadLines, st.Tokens.Output)
	}
	// Mostly garbage: nothing is saved, the next good run starts from the
	// same offset.
	appendTo(t, p, "garbage\ngarbage\ngarbage\n"+assistant(t, "msg_4", 4, 10))
	if _, err := r.Read(sid); !errors.Is(err, ErrTooManyBad) {
		t.Fatalf("err = %v, want ErrTooManyBad", err)
	}
	st, err = Reader{ClaudeDir: r.ClaudeDir, StateDir: r.StateDir}.loadOnly(sid)
	if err != nil || st.Tokens.Output != 30 {
		t.Errorf("saved state after refused run: output=%d err=%v, want 30", st.Tokens.Output, err)
	}
}

// loadOnly returns the saved state without reading the transcript.
func (r Reader) loadOnly(id string) (*State, error) {
	return loadState(r.stateDir(), id), nil
}

func TestReadWrongTypedFieldIsNotBad(t *testing.T) {
	// A field of an unexpected type (a future format change) leaves that
	// field empty but keeps the rest of the entry.
	r, _ := setup(t, `{"type":"assistant","timestamp":"`+ts(1)+`","cwd":42,"message":{"id":"m","usage":{"output_tokens":9}}}`+"\n")
	st, err := r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.BadLines != 0 || st.Tokens.Output != 9 {
		t.Errorf("bad=%d output=%d, want 0 and 9", st.BadLines, st.Tokens.Output)
	}
}

func TestReadNoTranscriptAndLocked(t *testing.T) {
	r := Reader{ClaudeDir: t.TempDir(), StateDir: t.TempDir()}
	if _, err := r.Read(sid); !errors.Is(err, ErrNoTranscript) {
		t.Errorf("missing: %v, want ErrNoTranscript", err)
	}
	if _, err := r.Read("../../etc/passwd"); !errors.Is(err, ErrNoTranscript) {
		t.Errorf("path-like id: %v, want ErrNoTranscript", err)
	}
	r2, _ := setup(t, assistant(t, "msg_1", 1, 10))
	unlock, ok, err := lock(r2.StateDir, sid)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	defer unlock()
	if _, err := r2.Read(sid); !errors.Is(err, ErrLocked) {
		t.Errorf("held lock: %v, want ErrLocked", err)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/digest/ -count=1`
Expected: build failure: `undefined: Reader`.

- [ ] **Step 3: Write `internal/digest/transcript.go`**

```go
// Package digest builds a session's digest from its Claude Code transcript
// and git, for PUT /v1/sessions/{id}/digest. The transcript format is not
// documented, so every field is optional and unknown entries are skipped.
// See docs/client.md, "Digests".
package digest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
)

const (
	maxLine      = 1 << 20 // longer lines are skipped and counted as bad
	maxRecap     = 600
	maxTitle     = 200
	maxLinks     = 20
	maxLinkBytes = 300
	recapSuffix  = "(disable recaps in /config)"
	stateVersion = 1
)

var (
	ErrNoTranscript = errors.New("no transcript for this session")
	ErrLocked       = errors.New("another digest run holds the lock")
	ErrTooManyBad   = errors.New("more than half the new transcript lines are unreadable")

	idRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	prNumberRE = regexp.MustCompile(`^[0-9]{1,10}$`)
)

// fileState is how far one transcript file has been read.
type fileState struct {
	Offset int64  `json:"offset"`
	Dev    uint64 `json:"dev"`
	Ino    uint64 `json:"ino"`
}

// State is the running result of reading one session's transcripts, saved
// between runs so each run reads only new lines.
type State struct {
	V           int                  `json:"v"`
	Files       map[string]fileState `json:"files"` // main and subagent files
	Seen        map[string]bool      `json:"seen"`  // assistant message IDs already counted
	Tokens      api.DigestTokens     `json:"tokens"`
	Recap       string               `json:"recap,omitempty"`
	RecapAt     *time.Time           `json:"recap_at,omitempty"`
	AITitle     string               `json:"ai_title,omitempty"`
	CustomTitle string               `json:"custom_title,omitempty"`
	Links       []api.DigestLink     `json:"links,omitempty"`
	CostUSD     *float64             `json:"cost_usd,omitempty"`
	CostAt      *time.Time           `json:"cost_at,omitempty"`
	FirstAt     *time.Time           `json:"first_at,omitempty"`
	AsOf        *time.Time           `json:"as_of,omitempty"`
	CWD         string               `json:"cwd,omitempty"`
	BadLines    int                  `json:"bad_lines"`
}

func newState() *State {
	return &State{V: stateVersion, Files: map[string]fileState{}, Seen: map[string]bool{}}
}

type usage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

// entry holds the transcript fields the digest reads. The message content
// (the reply and prompt text) is deliberately not declared.
type entry struct {
	Type         string          `json:"type"`
	Subtype      string          `json:"subtype"`
	Timestamp    string          `json:"timestamp"`
	CWD          string          `json:"cwd"`
	Content      json.RawMessage `json:"content"`
	AITitle      string          `json:"aiTitle"`
	CustomTitle  string          `json:"customTitle"`
	PRNumber     json.RawMessage `json:"prNumber"`
	PRURL        string          `json:"prUrl"`
	PRRepository string          `json:"prRepository"`
	TotalCostUSD *float64        `json:"totalCostUSD"`
	Message      *struct {
		ID    string `json:"id"`
		Usage *usage `json:"usage"`
	} `json:"message"`
}

// DefaultClaudeDir is CLAUDE_CONFIG_DIR, else ~/.claude.
func DefaultClaudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// TranscriptPath finds <claudeDir>/projects/*/<id>.jsonl. Session IDs are
// unique, so it needs exactly one match; it never rebuilds the project
// folder name from the working directory.
func TranscriptPath(claudeDir, id string) (string, error) {
	if !idRE.MatchString(id) {
		return "", ErrNoTranscript
	}
	m, err := filepath.Glob(filepath.Join(claudeDir, "projects", "*", id+".jsonl"))
	if err != nil || len(m) != 1 {
		return "", ErrNoTranscript
	}
	return m[0], nil
}

func identity(fi os.FileInfo) (dev, ino uint64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), uint64(st.Ino)
	}
	return 0, 0
}

// replaced reports whether the file at p is not the one st read: it shrank
// below the saved offset or has a different inode.
func (st *State) replaced(p string) bool {
	fs, ok := st.Files[p]
	if !ok {
		return false
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false // gone; a missing subagent file just stops adding
	}
	dev, ino := identity(fi)
	return fi.Size() < fs.Offset || dev != fs.Dev || ino != fs.Ino
}

// update reads the new lines of the main transcript and its subagent files
// into st. It starts over when any file was replaced. It returns a nil state
// and ErrTooManyBad when more than half the lines read were bad; the caller
// then keeps its saved state.
func update(st *State, mainPath string) (*State, error) {
	subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(mainPath, ".jsonl"), "subagents", "*.jsonl"))
	sort.Strings(subs)
	files := append([]string{mainPath}, subs...)
	for _, p := range files {
		if st.replaced(p) {
			st = newState()
			break
		}
	}
	var lines, bad int
	for i, p := range files {
		n, b, err := st.readFile(p, i == 0)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			continue
		}
		lines, bad = lines+n, bad+b
	}
	if bad*2 > lines {
		return nil, ErrTooManyBad
	}
	st.BadLines += bad
	return st, nil
}

// readFile applies every complete line after the saved offset. A last line
// without a newline is left for the next run. main is false for subagent
// files, which only add tokens.
func (st *State) readFile(p string, main bool) (lines, bad int, err error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	dev, ino := identity(fi)
	off := st.Files[p].Offset
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, 0, err
	}
	br := bufio.NewReaderSize(f, 64<<10)
	var buf []byte
	var pending int64
	skipping := false
	for {
		chunk, err := br.ReadSlice('\n')
		pending += int64(len(chunk))
		if !skipping {
			if len(buf)+len(chunk) > maxLine {
				skipping, buf = true, buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == bufio.ErrBufferFull:
			continue
		case err == io.EOF:
			st.Files[p] = fileState{Offset: off, Dev: dev, Ino: ino}
			return lines, bad, nil
		case err != nil:
			return lines, bad, err
		}
		off, pending = off+pending, 0
		if l := bytes.TrimSpace(buf); skipping || len(l) > 0 {
			lines++
			if skipping || !st.apply(l, main) {
				bad++
			}
		}
		skipping, buf = false, buf[:0]
	}
}

// apply folds one line into st. It returns false when the line isn't JSON.
func (st *State) apply(line []byte, main bool) bool {
	var e entry
	if err := json.Unmarshal(line, &e); err != nil {
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			return false
		}
		// A field of an unexpected type stays empty; the rest is used.
	}
	if e.Type == "assistant" && e.Message != nil && e.Message.Usage != nil && e.Message.ID != "" && !st.Seen[e.Message.ID] {
		st.Seen[e.Message.ID] = true
		u := e.Message.Usage
		st.Tokens.Input += u.Input
		st.Tokens.Output += u.Output
		st.Tokens.CacheRead += u.CacheRead
		st.Tokens.CacheWrite += u.CacheWrite
	}
	if !main {
		return true
	}
	at, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	hasTS := err == nil
	if hasTS {
		at = at.UTC()
		if st.FirstAt == nil {
			first := at
			st.FirstAt = &first
		}
		if st.AsOf == nil || at.After(*st.AsOf) {
			last := at
			st.AsOf = &last
		}
	}
	if e.CWD != "" {
		st.CWD = e.CWD
	}
	switch {
	case e.Type == "system" && e.Subtype == "away_summary":
		var s string
		if json.Unmarshal(e.Content, &s) == nil {
			s = strings.TrimSuffix(strings.TrimSpace(s), recapSuffix)
			if s = termtext.Clean(s, maxRecap); s != "" {
				st.Recap, st.RecapAt = s, nil
				if hasTS {
					r := at
					st.RecapAt = &r
				}
			}
		}
	case e.Type == "ai-title":
		if s := termtext.Clean(e.AITitle, maxTitle); s != "" {
			st.AITitle = s
		}
	case e.Type == "custom-title":
		if s := termtext.Clean(e.CustomTitle, maxTitle); s != "" {
			st.CustomTitle = s
		}
	case e.Type == "pr-link":
		st.addLink(e)
	case e.Type == "cost-state" && e.TotalCostUSD != nil:
		// cost-state has no timestamp: use the last timed entry before it.
		c := *e.TotalCostUSD
		st.CostUSD, st.CostAt = &c, nil
		if st.AsOf != nil {
			t := *st.AsOf
			st.CostAt = &t
		}
	}
	return true
}

// addLink keeps https links only, once per URL, the newest maxLinks.
func (st *State) addLink(e entry) {
	u := strings.TrimSpace(e.PRURL)
	if !strings.HasPrefix(u, "https://") || len(u) > maxLinkBytes || termtext.Clean(u, 0) != u {
		return
	}
	for _, l := range st.Links {
		if l.URL == u {
			return
		}
	}
	num := strings.Trim(string(e.PRNumber), `"`)
	if !prNumberRE.MatchString(num) {
		num = ""
	}
	st.Links = append(st.Links, api.DigestLink{Number: num, URL: u, Repo: termtext.Clean(e.PRRepository, maxTitle)})
	if len(st.Links) > maxLinks {
		st.Links = st.Links[len(st.Links)-maxLinks:]
	}
}

// DigestIn is the API body for this state, without git. ok is false until
// a timed entry has been read.
func (st *State) DigestIn() (api.DigestIn, bool) {
	if st.AsOf == nil {
		return api.DigestIn{}, false
	}
	return api.DigestIn{
		AsOf: *st.AsOf, FirstAt: st.FirstAt, Recap: st.Recap, RecapAt: st.RecapAt,
		AITitle: st.AITitle, CustomTitle: st.CustomTitle, Links: st.Links, Tokens: st.Tokens,
		CostUSD: st.CostUSD, CostAt: st.CostAt, BadLines: st.BadLines,
	}, true
}
```

- [ ] **Step 4: Write `internal/digest/state.go`**

```go
package digest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/abdallah/session-hub/internal/paths"
)

// Reader reads transcripts and keeps per-session state. Empty fields take
// DefaultClaudeDir and DefaultStateDir.
type Reader struct {
	ClaudeDir string
	StateDir  string
}

// DefaultStateDir is <sessionhub state dir>/digest.
func DefaultStateDir() string { return filepath.Join(paths.StateDir(), "digest") }

func (r Reader) claudeDir() string {
	if r.ClaudeDir != "" {
		return r.ClaudeDir
	}
	return DefaultClaudeDir()
}

func (r Reader) stateDir() string {
	if r.StateDir != "" {
		return r.StateDir
	}
	return DefaultStateDir()
}

// Read takes the session's lock, reads new transcript lines, saves the
// state, and returns it. It returns ErrLocked at once when another run holds
// the lock.
func (r Reader) Read(id string) (*State, error) {
	path, err := TranscriptPath(r.claudeDir(), id)
	if err != nil {
		return nil, err
	}
	unlock, ok, err := lock(r.stateDir(), id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrLocked
	}
	defer unlock()
	st, err := update(loadState(r.stateDir(), id), path)
	if err != nil {
		return nil, err
	}
	if err := saveState(r.stateDir(), id, st); err != nil {
		return nil, err
	}
	return st, nil
}

// loadState returns the saved state, or a fresh one when the file is
// missing, unreadable, or from another state version.
func loadState(dir, id string) *State {
	b, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return newState()
	}
	st := newState()
	if json.Unmarshal(b, st) != nil || st.V != stateVersion || st.Files == nil || st.Seen == nil {
		return newState()
	}
	return st
}

// saveState writes the state atomically (temp file and rename).
func saveState(dir, id string, st *State) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, id+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, id+".json"))
}

// lock takes <dir>/<id>.lock without waiting. ok is false when another
// process holds it.
func lock(dir, id string) (unlock func(), ok bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { f.Close() }, true, nil
}
```

`lock` is taken after `TranscriptPath`, so `TestReadNoTranscriptAndLocked`'s
path-like ID never creates a lock file outside the state directory.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/digest/ -count=1 -race`
Expected: `ok`.

- [ ] **Step 6: Run the suite and commit**

Run: `make test lint`

```bash
git add internal/digest
git commit -m "Read session digests from Claude Code transcripts" -m "Refs: docs/dev/superpowers/plans/2026-09-30-session-insights.md, Task 3"
```

---

### Task 4: Git summary

**Files:**
- Create: `internal/digest/git.go`
- Create: `internal/digest/git_test.go`

**Interfaces:**
- Consumes: `api.DigestGit`, `api.GitCounts`, `api.DigestCommit` (Task 1).
- Produces: `func Git(ctx context.Context, cwd string, since, until time.Time) *api.DigestGit`,
  `digest.GitTimeout = 3 * time.Second`.

- [ ] **Step 1: Write the failing test**

```go
package digest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitRepo makes a repo whose commits have fixed dates.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	run := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), append([]string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e"}, env...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run(nil, "init", "-q", "-b", "main")
	for i, when := range []string{"2026-09-30T08:00:00Z", "2026-09-30T10:10:00Z", "2026-09-30T10:20:00Z", "2026-09-30T12:00:00Z"} {
		name := filepath.Join(dir, "f"+string(rune('a'+i)))
		os.WriteFile(name, []byte("x"), 0o600)
		run(nil, "add", ".")
		subj := "commit " + string(rune('a'+i))
		if i == 2 {
			subj += "\twith a tab and " + strings.Repeat("long ", 40)
		}
		run([]string{"GIT_AUTHOR_DATE=" + when, "GIT_COMMITTER_DATE=" + when}, "commit", "-q", "-m", subj)
	}
	os.WriteFile(filepath.Join(dir, "dirty"), []byte("x"), 0o600)
	return dir
}

func TestGit(t *testing.T) {
	dir := gitRepo(t)
	since := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	g := Git(context.Background(), dir, since, until)
	if g == nil {
		t.Fatal("Git = nil")
	}
	if g.CommitCount != 2 || len(g.Commits) != 2 || !strings.HasPrefix(g.Commits[0].Subject, "commit c") ||
		g.Commits[1].Subject != "commit b" {
		t.Errorf("commits = %d %+v, want c then b", g.CommitCount, g.Commits)
	}
	if n := len([]rune(g.Commits[0].Subject)); n > 120 || strings.ContainsAny(g.Commits[0].Subject, "\t\n") {
		t.Errorf("subject not cleaned: %d runes %q", n, g.Commits[0].Subject)
	}
	if g.Uncommitted != 1 || g.Unpushed != -1 {
		t.Errorf("uncommitted=%d unpushed=%d, want 1 and -1", g.Uncommitted, g.Unpushed)
	}

	for name, cwd := range map[string]string{
		"missing directory": filepath.Join(t.TempDir(), "gone"),
		"not a repository":  t.TempDir(),
		"empty":             "",
	} {
		if g := Git(context.Background(), cwd, since, until); g != nil {
			t.Errorf("%s: Git = %+v, want nil", name, g)
		}
	}
	// unborn HEAD: a repository with no commits yet.
	empty := t.TempDir()
	exec.Command("git", "-C", empty, "init", "-q").Run()
	if g := Git(context.Background(), empty, since, until); g != nil {
		t.Errorf("unborn HEAD: Git = %+v, want nil", g)
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./internal/digest/ -run TestGit -count=1`
Expected: build failure: `undefined: Git`.

- [ ] **Step 3: Write `internal/digest/git.go`**

```go
package digest

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
)

// GitTimeout bounds all git commands of one digest.
const GitTimeout = 3 * time.Second

const (
	maxCommits = 5
	maxSubject = 120
)

// Git summarizes HEAD's commits in [since, until] and the uncommitted and
// unpushed counts in cwd. It returns nil when cwd is missing or not a
// repository, HEAD has no commits, or git fails or times out.
func Git(ctx context.Context, cwd string, since, until time.Time) *api.DigestGit {
	if cwd == "" {
		return nil
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, GitTimeout)
	defer cancel()
	out, err := git(ctx, cwd, "log", "--since="+since.UTC().Format(time.RFC3339),
		"--until="+until.UTC().Format(time.RFC3339), "--format=%h%x00%s", "HEAD")
	if err != nil {
		return nil
	}
	g := &api.DigestGit{Commits: []api.DigestCommit{}}
	for _, l := range strings.Split(out, "\n") {
		if l == "" {
			continue
		}
		g.CommitCount++
		if len(g.Commits) < maxCommits {
			sha, subj, _ := strings.Cut(l, "\x00")
			g.Commits = append(g.Commits, api.DigestCommit{SHA: sha, Subject: termtext.Clean(subj, maxSubject)})
		}
	}
	status, err := git(ctx, cwd, "status", "--porcelain")
	if err != nil {
		return nil
	}
	for _, l := range strings.Split(status, "\n") {
		if l != "" {
			g.Uncommitted++
		}
	}
	g.Unpushed = -1
	if n, err := git(ctx, cwd, "rev-list", "--count", "@{u}..HEAD"); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			g.Unpushed = v
		}
	}
	return g
}

// git runs one read-only git command. GIT_OPTIONAL_LOCKS=0 keeps
// `git status` from taking index.lock, which would get in the way of the
// user's own git commands.
func git(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	out, err := cmd.Output()
	return string(out), err
}
```

- [ ] **Step 4: Run the test**

Run: `go test ./internal/digest/ -run TestGit -count=1 -v`
Expected: `PASS`.

- [ ] **Step 5: Run the suite and commit**

Run: `make test lint`

```bash
git add internal/digest/git.go internal/digest/git_test.go
git commit -m "Summarize a session's git activity for its digest" -m "Refs: docs/dev/superpowers/plans/2026-09-30-session-insights.md, Task 4"
```

---

### Task 5: `sessionhub digest` command, client, and backfill

**Files:**
- Create: `internal/digest/cmd.go`
- Create: `internal/digest/cmd_test.go`
- Modify: `internal/client/http.go` (`PutDigest`)
- Modify: `internal/client/replay.go` (`OpDigest`)
- Modify: `internal/client/replay_test.go`
- Modify: `cmd/sessionhub/main.go` (route and usage)
- Modify: `docs/cli.md`, `docs/client.md`

**Interfaces:**
- Consumes: `Reader`, `State.DigestIn`, `Git` (Tasks 3, 4); `PUT
  /v1/sessions/{id}/digest` (Task 2).
- Produces:
  - `func (c *client.Client) PutDigest(ctx context.Context, id string, d api.DigestIn) error`
  - `client.OpDigest = "digest"` (Body is `api.DigestIn`, needs `SessionID`)
  - `func digest.Run(ctx context.Context, args []string) error` (`sessionhub digest`)
  - `type digest.Builder struct { Reader Reader; Git func(ctx context.Context, cwd string, since, until time.Time) *api.DigestGit }`
  - `func (b Builder) Build(ctx context.Context, id string) (api.DigestIn, error)`
  - `func Send(ctx context.Context, c *client.Client, q *client.Queue, id string, d api.DigestIn) (queued bool, err error)`
  - `digest.ErrEmpty`

- [ ] **Step 1: Write the failing client test**

In `internal/client/replay_test.go`, add a case for `OpDigest` in the same
style as the existing `OpReport` case: a queued item with `SessionID` and an
`api.DigestIn` body must reach `PUT /v1/sessions/<id>/digest` with that body,
and an item without `SessionID` must be permanent (not retryable). Read the
existing test first and copy its fake-server setup; the new case asserts
`r.Method == "PUT"` and `r.URL.Path == "/v1/sessions/sess-1/digest"`.

- [ ] **Step 2: Add `PutDigest` and `OpDigest`**

In `internal/client/http.go`:

```go
// PutDigest sends a session's digest.
func (c *Client) PutDigest(ctx context.Context, id string, d api.DigestIn) error {
	return c.do(ctx, http.MethodPut, "/v1/sessions/"+url.PathEscape(id)+"/digest", d, nil)
}
```

In `internal/client/replay.go`, extend the const block and `Replay`:

```go
const (
	OpReport = "report"
	OpTitle  = "title"
	// OpDigest: Body is api.DigestIn; needs Item.SessionID.
	OpDigest = "digest"
)
```

```go
	case OpDigest:
		var v api.DigestIn
		if err := needID(); err != nil {
			return err
		}
		if err := decode(&v); err != nil {
			return err
		}
		return c.PutDigest(ctx, it.SessionID, v)
```

Run: `go test ./internal/client/ -count=1`
Expected: `ok`.

- [ ] **Step 3: Write the failing command tests**

Create `internal/digest/cmd_test.go`. It runs the command against an
`httptest` server that records requests, a temp transcript (reuse `setup`,
`assistant`, and `line` from `transcript_test.go`), and a temp queue.

```go
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
```

- [ ] **Step 4: Run them to see them fail**

Run: `go test ./internal/digest/ -run TestDigest -count=1`
Expected: build failure: `undefined: cmdEnv`.

- [ ] **Step 5: Write `internal/digest/cmd.go`**

```go
package digest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

// ErrEmpty: the transcript has no timed entry yet.
var ErrEmpty = errors.New("transcript has no timed entry yet")

// sendTimeout bounds one PUT; a slower server gets the digest from the queue.
const sendTimeout = 5 * time.Second

// Builder reads a session's transcript and runs git.
type Builder struct {
	Reader Reader
	Git    func(ctx context.Context, cwd string, since, until time.Time) *api.DigestGit
}

// Build returns the session's current digest. Git runs in the transcript's
// last working directory over the session's activity window.
func (b Builder) Build(ctx context.Context, id string) (api.DigestIn, error) {
	st, err := b.Reader.Read(id)
	if err != nil {
		return api.DigestIn{}, err
	}
	d, ok := st.DigestIn()
	if !ok {
		return d, ErrEmpty
	}
	if b.Git != nil && st.FirstAt != nil && st.CWD != "" {
		d.Git = b.Git(ctx, st.CWD, *st.FirstAt, *st.AsOf)
	}
	return d, nil
}

// Send puts the digest, or queues it when the server can't take it now
// (network error, timeout, 408, 429, 5xx, or no usable client). A 404 or
// other 4xx is returned as an error and not queued.
func Send(ctx context.Context, c *client.Client, q *client.Queue, id string, d api.DigestIn) (queued bool, err error) {
	if c != nil {
		sctx, cancel := context.WithTimeout(ctx, sendTimeout)
		err = c.PutDigest(sctx, id, d)
		cancel()
		if err == nil {
			return false, nil
		}
		if !client.IsRetryable(err) {
			return false, err
		}
	}
	body, merr := json.Marshal(d)
	if merr != nil {
		return false, merr
	}
	if qerr := q.Append(client.Item{Op: client.OpDigest, SessionID: id, Body: body}); qerr != nil {
		return false, fmt.Errorf("send failed (%v) and queueing failed: %w", err, qerr)
	}
	return true, nil
}

// skipReason names the build errors that are not failures.
func skipReason(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrNoTranscript):
		return "no transcript", true
	case errors.Is(err, ErrLocked):
		return "another run is in progress", true
	case errors.Is(err, ErrEmpty):
		return "nothing to report yet", true
	}
	return "", false
}

type cmdEnv struct {
	out   io.Writer
	queue *client.Queue
	cfg   client.Config
	build Builder
}

// Run is `sessionhub digest <session-id>` and `sessionhub digest --all`.
func Run(ctx context.Context, args []string) error {
	cfg, err := client.LoadConfig()
	if err != nil {
		return err
	}
	e := &cmdEnv{out: os.Stdout, queue: client.DefaultQueue(), cfg: cfg, build: Builder{Git: Git}}
	return e.run(ctx, args)
}

const usage = "usage: sessionhub digest <session-id> | sessionhub digest --all"

func (e *cmdEnv) run(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New(usage)
	}
	c, cerr := client.New(e.cfg)
	if args[0] == "--all" {
		if cerr != nil {
			return cerr
		}
		return e.all(ctx, c)
	}
	if len(args[0]) > 0 && args[0][0] == '-' {
		return errors.New(usage)
	}
	if cerr != nil {
		c = nil // no usable client: the digest is queued
	}
	res, err := e.one(ctx, c, args[0])
	fmt.Fprintf(e.out, "%s  %s\n", short(args[0]), res)
	return err
}

// one builds and sends one digest and describes the result.
func (e *cmdEnv) one(ctx context.Context, c *client.Client, id string) (string, error) {
	d, err := e.build.Build(ctx, id)
	if reason, ok := skipReason(err); ok {
		return "skipped: " + reason, nil
	}
	if err != nil {
		return "failed: " + err.Error(), err
	}
	queued, err := Send(ctx, c, e.queue, id, d)
	switch {
	case err != nil && client.IsNotFound(err):
		return "failed: sessionhub doesn't know this session", err
	case err != nil:
		return "failed: " + err.Error(), err
	case queued:
		return "queued", nil
	}
	return "sent", nil
}

// all sends a digest for every session the server knows on this machine.
func (e *cmdEnv) all(ctx context.Context, c *client.Client) error {
	if e.cfg.Machine == "" {
		return errors.New("sessionhub digest --all: the client config has no machine name; run sessionhub join first")
	}
	list, err := c.ListSessions(ctx, false, e.cfg.Machine)
	if err != nil {
		return err
	}
	var sent, skipped, failed int
	for _, s := range list {
		res, err := e.one(ctx, c, s.ID)
		fmt.Fprintf(e.out, "%s  %s\n", short(s.ID), res)
		switch {
		case err != nil:
			failed++
		case len(res) >= 7 && res[:7] == "skipped":
			skipped++
		default:
			sent++
		}
	}
	fmt.Fprintf(e.out, "%d sent, %d skipped, %d failed\n", sent, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d digests failed", failed)
	}
	return nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
```

Before writing `all`, check `client.ListSessions`'s signature in
`internal/client/http.go` (`ListSessions(ctx, live bool, machine string)`);
`live=false` returns every status. Count `queued` as sent in the total.

- [ ] **Step 6: Register the command**

In `cmd/sessionhub/main.go`, import `session-hub/internal/digest`, add
`"digest": digest.Run,` to `routes`, and add a usage line after `status`:

```
  digest <id> | --all           send session digests to the sessionhub
```

If `cmd/sessionhub/main_test.go` checks that every route appears in `usage`, it now
covers `digest`.

- [ ] **Step 7: Run the tests**

Run: `go test ./internal/digest/ ./internal/client/ ./cmd/sessionhub/ -count=1`
Expected: `ok` for each.

- [ ] **Step 8: Document**

- `docs/cli.md`: `sessionhub digest <id>` and `sessionhub digest --all`, their output lines
  (`sent`, `queued`, `skipped: <reason>`, `failed: <reason>`), the exit status
  (non-zero only when a digest failed), and that `--all` is safe to rerun.
- `docs/client.md`: a "Digests" section: transcript location and glob,
  state files under `~/.local/state/sessionhub/digest/`, what is read and what is
  never read (reply and prompt text), the bad-line rule, the lock, and the
  `digest` queue op.

- [ ] **Step 9: Run the suite and commit**

Run: `make test lint`

```bash
git add internal/digest internal/client cmd/sessionhub docs/cli.md docs/client.md
git commit -m "Add sessionhub digest to send and backfill session digests" -m "Refs: docs/dev/superpowers/plans/2026-09-30-session-insights.md, Task 5"
```

---

### Task 6: Triggers in the hooks client and the watcher

**Files:**
- Modify: `internal/hooks/hooks.go`
- Modify: `internal/hooks/hooks_test.go`, `internal/hooks/flush_test.go`
- Modify: `internal/plugin/watcher.go`
- Modify: `internal/plugin/coalesce.go`
- Modify: `internal/plugin/watcher_test.go`, `internal/plugin/coalesce_test.go`
- Modify: `docs/hooks.md`, `docs/plugin.md`

**Interfaces:**
- Consumes: `sessionhub digest <id>` (Task 5), `digest.Builder`, `digest.Send`,
  `digest.TranscriptPath`, `digest.DefaultClaudeDir`, `client.OpDigest`.
- Produces: hooks `handler.digestCmd func(id string) *exec.Cmd`; watcher
  field `digest func(ctx context.Context, id string) error` and
  `transcriptStat func(id string) (size int64, mod time.Time, ok bool)`.

- [ ] **Step 1: Write the failing hooks tests**

In `internal/hooks/flush_test.go`, following the existing `flushCmd` tests,
add `TestStopAndSessionEndStartDigest`: set `fx.h.digestCmd` to a function
that records the ID and returns `exec.Command("true")`; run the `stop` hook and
the `session-end` hook with `session_id` `sess-1`; assert the recorded IDs are
`["sess-1", "sess-1"]`. Add a second case where `digestCmd` returns
`exec.Command("/nonexistent/sessionhub", "digest", "sess-1")`: the hook still returns
nil and writes one `hook: start digest:` line to stderr. Add a third case: a
`prompt` hook starts no digest.

In `internal/hooks/hooks_test.go`, set `digestCmd: func(string) *exec.Cmd {
return nil }` in the test handler next to `flushCmd`.

- [ ] **Step 2: Start the digest child from the hooks**

In `internal/hooks/hooks.go`, add the field to `handler`:

```go
	// digestCmd builds the detached `sessionhub digest <id>` child that the stop
	// and session-end hooks start. nil skips it.
	digestCmd func(id string) *exec.Cmd
```

Set `digestCmd: digestCommand,` in `defaultHandler`, and add:

```go
// digestCommand is `sessionhub digest <id>`, run by the same binary.
func digestCommand(id string) *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	return exec.Command(exe, "digest", id)
}
```

Generalize `startFlush` into `startDetached(cmd *exec.Cmd) error` (same body,
taking the command), keep `startFlush` as `return h.startDetached(h.flushCmd())`,
and add:

```go
// startDigest starts `sessionhub digest <id>` without waiting. The child reads the
// transcript and runs git, which can take longer than a hook may.
func (h *handler) startDigest(id string) {
	if h.digestCmd == nil {
		return
	}
	if err := startDetached(h.digestCmd(id)); err != nil {
		fmt.Fprintf(h.stderr, "hook: start digest: %v\n", err)
	}
}
```

`startDetached` must return nil for a nil command, like `startFlush` does now.
Call `h.startDigest(in.SessionID)` in the `stop` case before `items = ...`, and
in `sessionEnd` after `h.startFlush()`.

- [ ] **Step 3: Run the hooks tests**

Run: `go test ./internal/hooks/ -count=1`
Expected: `ok`.

- [ ] **Step 4: Write the failing watcher and coalesce tests**

In `internal/plugin/coalesce_test.go`, add a case: two `OpDigest` items for
`sess-1` and one for `sess-2`, with an `OpEvent` prompt between them. `coalesce`
keeps the later `sess-1` digest, the `sess-2` digest, and the prompt, and
reports the earlier `sess-1` digest as superseded.

In `internal/plugin/watcher_test.go`, add `TestHeartbeatDigestsChangedTranscripts`:
build the watcher the way the existing tests do (`newWatcher(...)` at line 55),
then set `e.w.digest` to a recorder and `e.w.transcriptStat` to a fake map
`id → (size, mod)`. Drive heartbeats with the fake clock:

1. First heartbeat with two Claude panes (`sess-1`, `sess-2`): both digested.
2. Second heartbeat, nothing changed: none digested.
3. `sess-1`'s size grows: only `sess-1` digested.
4. `transcriptStat` returns `ok=false` for `sess-2`: not digested, no log line.
5. The digest function returns an error for `sess-1`: one log line
   `digest sess-1: <err>`; the next heartbeat with the same size retries it.
6. With 12 changed sessions, one heartbeat digests at most 10
   (`maxDigestsPerBeat`); the rest go on the next heartbeat.

- [ ] **Step 5: Coalesce digests**

In `internal/plugin/coalesce.go`, add `digest bool` to `seen`, a line to the
doc comment ("a digest is superseded by a later digest for the same
session"), and a case:

```go
		case it.Op == client.OpDigest:
			drop[i] = s.digest
			s.digest = true
```

- [ ] **Step 6: Digest from the heartbeat**

In `internal/plugin/watcher.go`, add to `watcher`:

```go
	// digest builds and sends one session's digest; transcriptStat reports
	// its transcript's size and modification time. Tests replace both.
	digest         func(ctx context.Context, id string) error
	transcriptStat func(id string) (size int64, mod time.Time, ok bool)
	digested       map[string]transcriptMark // session ID → transcript at the last good digest
```

```go
// transcriptMark is a transcript's size and modification time.
type transcriptMark struct {
	size int64
	mod  time.Time
}

// maxDigestsPerBeat bounds the digests one heartbeat runs; the rest wait
// for the next heartbeat.
const maxDigestsPerBeat = 10
```

Initialize `digested: map[string]transcriptMark{}` in `newWatcher`. In
`runWatcher`, after `newWatcher`, set the defaults:

```go
	w.transcriptStat = func(id string) (int64, time.Time, bool) {
		p, err := digest.TranscriptPath(digest.DefaultClaudeDir(), id)
		if err != nil {
			return 0, time.Time{}, false
		}
		fi, err := os.Stat(p)
		if err != nil {
			return 0, time.Time{}, false
		}
		return fi.Size(), fi.ModTime(), true
	}
	b := digest.Builder{Git: digest.Git}
	w.digest = func(ctx context.Context, id string) error {
		d, err := b.Build(ctx, id)
		if errors.Is(err, digest.ErrLocked) || errors.Is(err, digest.ErrEmpty) {
			return nil
		}
		if err != nil {
			return err
		}
		c, cerr := w.newClient()
		if cerr != nil {
			c = nil
		}
		_, err = digest.Send(ctx, c, w.q, id, d)
		return err
	}
```

In `heartbeat`, right after the `refresh` error check, add
`defer w.digests(ctx, now)`. The digests then run after the heartbeat is sent,
whether or not the send succeeded (a digest is queued when the server is down),
and never when the snapshot failed. Running them after the send keeps a slow
first read of a large transcript from delaying the heartbeat. Add the pass:

```go
// digests runs the digest for each known Claude session whose transcript
// changed since its last good digest. The pane cache still holds panes that
// closed a moment ago, so a session's last turn is digested too.
func (w *watcher) digests(ctx context.Context, now time.Time) {
	if w.digest == nil || w.transcriptStat == nil {
		return
	}
	// The cache is keyed by pane, and a resumed session can sit under two
	// panes for a while, so collect each session once.
	set := map[string]bool{}
	for _, e := range w.cache {
		if e.u.ID != "" {
			set[e.u.ID] = true
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ran := 0
	for _, id := range ids {
		if ran == maxDigestsPerBeat {
			return
		}
		size, mod, ok := w.transcriptStat(id)
		if !ok {
			continue
		}
		mark := transcriptMark{size, mod}
		if w.digested[id] == mark {
			continue
		}
		ran++
		if err := w.digest(ctx, id); err != nil {
			w.logf(now, "digest %s: %v", id, err)
			continue
		}
		w.digested[id] = mark
	}
}
```

Prune `w.digested` for IDs no longer in `w.cache` at the end of `digests`, so
the map can't grow without bound. Add imports `errors`, `os`, `sort`, and
`session-hub/internal/digest` as needed.

- [ ] **Step 7: Run the plugin tests**

Run: `go test ./internal/plugin/ -count=1 -race`
Expected: `ok`. Existing watcher tests leave `digest` nil, so they don't
change.

- [ ] **Step 8: Document**

- `docs/hooks.md`: the `stop` and `session-end` hooks also start
  `sessionhub digest <id>` detached; the hook never waits for it.
- `docs/plugin.md`: the heartbeat digests up to 10 sessions whose transcript
  changed, including panes that closed within the cache window; a failure is
  logged and retried on the next heartbeat.

- [ ] **Step 9: Run the suite and commit**

Run: `make test lint`

```bash
git add internal/hooks internal/plugin docs/hooks.md docs/plugin.md
git commit -m "Send digests from the stop and session-end hooks and the heartbeat" -m "Refs: docs/dev/superpowers/plans/2026-09-30-session-insights.md, Task 6"
```

---

### Task 7: CLI `ls --grep` and `show`

**Files:**
- Modify: `internal/cli/cli.go`
- Modify: `internal/cli/cli_test.go`
- Modify: `docs/cli.md` (the `ls` and `show` sections only)

**Interfaces:**
- Consumes: `api.Session.{Recap, LastPrompt, LastPromptAt, Summary}`,
  `api.SessionDetail.Digest` (Task 1).
- Produces: `func matches(s api.Session, q string) bool`,
  `func summaryLine(sum *api.SessionSummary) string` (package `cli`).

- [ ] **Step 1: Write the failing tests**

In `internal/cli/cli_test.go`, add:

```go
func TestMatches(t *testing.T) {
	s := api.Session{Machine: "tower", Title: "Fix CI", Recap: "Next, merge MR 391.",
		LastPrompt: "why is the [build] red?", GitBranch: "feat/tokens", CWD: "/home/u/app"}
	for q, want := range map[string]bool{
		"":        true,
		"fix ci":  true,  // case-insensitive
		"MR 391":  true,  // recap
		"[build]": true,  // plain text, not a pattern
		"tokens":  true,  // branch
		"/u/app":  true,  // directory
		"TOWER":    true,  // machine
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
```

Add a `TestShowDigest` in the style of the existing `show` test: a fake API
returns a `SessionDetail` with `Recap`, `LastPrompt`, and a `Digest` holding
two commits (one subject with an escape sequence `"\x1b[31mred"`), two links,
tokens, and cost. The output must contain `recap:`, `last prompt:`,
`commits on <branch> during the session (2)`, both commit lines, both link
URLs, `tokens:` with the four kinds, `cost:` with `as of`, and `digest as of`,
and must not contain `\x1b`. Add a `TestLsGrep` that lists three sessions and
checks `sessionhub ls --all --grep merge` prints only the one whose recap contains
"merge", and `sessionhub ls --grep nothing` prints `no sessions`.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/cli/ -count=1`
Expected: build failure: `undefined: matches`.

- [ ] **Step 3: Implement**

In `internal/cli/cli.go`:

```go
// matches reports whether q occurs, ignoring case, in the session's title,
// recap, last prompt, branch, directory, or machine. q is plain text.
func matches(s api.Session, q string) bool {
	if q == "" {
		return true
	}
	q = strings.ToLower(q)
	for _, f := range []string{s.Title, s.Recap, s.LastPrompt, s.GitBranch, s.CWD, s.Machine} {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// linkLabel is "!391" for a GitLab merge request, "#7" otherwise, or
// "link" without a number.
func linkLabel(l api.DigestLink) string {
	switch {
	case l.Number == "":
		return "link"
	case strings.Contains(l.URL, "/merge_requests/"):
		return "!" + l.Number
	}
	return "#" + l.Number
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// humanCount is 950, 371k, or 1.5M.
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1000:
		return strconv.FormatInt(n/1000, 10) + "k"
	}
	return strconv.FormatInt(n, 10)
}

// summaryLine is the card's one-line summary: git counts, the latest link,
// and the cost, or output tokens when no cost is known. Parts that are zero
// or unknown are left out.
func summaryLine(sum *api.SessionSummary) string {
	if sum == nil {
		return ""
	}
	var parts []string
	if g := sum.Git; g != nil {
		if g.CommitCount > 0 {
			parts = append(parts, plural(g.CommitCount, "commit"))
		}
		if g.Uncommitted > 0 {
			parts = append(parts, fmt.Sprintf("%d uncommitted", g.Uncommitted))
		}
		if g.Unpushed > 0 {
			parts = append(parts, fmt.Sprintf("%d unpushed", g.Unpushed))
		}
	}
	if sum.LatestLink != nil {
		parts = append(parts, linkLabel(*sum.LatestLink))
	}
	switch {
	case sum.CostUSD != nil:
		parts = append(parts, fmt.Sprintf("$%.2f", *sum.CostUSD))
	case sum.OutputTokens > 0:
		parts = append(parts, humanCount(sum.OutputTokens)+" tokens out")
	}
	return strings.Join(parts, " · ")
}
```

Add `strconv` to the imports.

In `ls`, add the flag and filter:

```go
	const usage = "usage: sessionhub ls [--all] [--machine M] [--grep TEXT]"
	...
	grep := fs.String("grep", "", "only sessions whose title, recap, last prompt, branch, directory, or machine contains TEXT")
	...
	if *grep != "" {
		kept := list[:0]
		for _, s := range list {
			if matches(s, *grep) {
				kept = append(kept, s)
			}
		}
		list = kept
	}
```

Place the filter after the status filter and before the `no sessions` check.

In `renderDetail`, after `field("resume", d.ResumeCommand)`:

```go
	if d.Recap != "" {
		at := ""
		if d.RecapAt != nil {
			at = fmt.Sprintf(" (%s ago)", age(now, *d.RecapAt))
		}
		field("recap", d.Recap+at)
	}
	if d.LastPrompt != "" && d.LastPromptAt != nil {
		field("last prompt", fmt.Sprintf("%s (%s ago)", d.LastPrompt, age(now, *d.LastPromptAt)))
	}
	if line := summaryLine(d.Summary); line != "" {
		field("summary", line)
	}
	if dg := d.Digest; dg != nil {
		renderDigest(w, d.GitBranch, dg, now)
	}
```

The `field` helper pads keys to 10 characters; `last prompt:` is 12, which
prints without padding, and that's fine. Add:

```go
func renderDigest(w io.Writer, branch string, dg *api.Digest, now time.Time) {
	if g := dg.Git; g != nil && g.CommitCount > 0 {
		on := "HEAD"
		if branch != "" {
			on = branch
		}
		fmt.Fprintf(w, "\ncommits on %s during the session (%d)\n", clean(on, 0), g.CommitCount)
		for _, c := range g.Commits {
			fmt.Fprintf(w, "  %s  %s\n", clean(c.SHA, 0), clean(c.Subject, 0))
		}
	}
	if len(dg.Links) > 0 {
		fmt.Fprintf(w, "\nlinks (%d)\n", len(dg.Links))
		for _, l := range dg.Links {
			fmt.Fprintf(w, "  %-6s %s\n", linkLabel(l), clean(l.URL, 0))
		}
	}
	t := dg.Tokens
	fmt.Fprintf(w, "\ntokens:    input %s · output %s · cache read %s · cache write %s\n",
		humanCount(t.Input), humanCount(t.Output), humanCount(t.CacheRead), humanCount(t.CacheWrite))
	if dg.CostUSD != nil {
		as := ""
		if dg.CostAt != nil {
			as = " as of " + dg.CostAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "cost:      $%.2f%s\n", *dg.CostUSD, as)
	}
	fmt.Fprintf(w, "digest as of %s (%s ago)\n", dg.AsOf.Format(time.RFC3339), age(now, dg.AsOf))
}
```

- [ ] **Step 4: Run the CLI tests**

Run: `go test ./internal/cli/ -count=1`
Expected: `ok`.

- [ ] **Step 5: Document, run the suite, and commit**

In `docs/cli.md`, add `--grep TEXT` to `sessionhub ls` (what it matches; plain text,
case-insensitive; combine with `--all` for ended sessions) and the new
`sessionhub show` fields and sections.

Run: `make test lint`

```bash
git add internal/cli docs/cli.md
git commit -m "Show digests in sessionhub show and filter sessionhub ls with --grep" -m "Refs: docs/dev/superpowers/plans/2026-09-30-session-insights.md, Task 7"
```

If Task 5 edits `docs/cli.md` in parallel, the controller merges the two
sections; they don't overlap.

---

### Task 8: Dashboard

**Files:**
- Modify: `internal/server/dashboard/index.html`
- Modify: `internal/server/dashboard_test.go`

**Interfaces:**
- Consumes: the session list fields and `GET /v1/sessions/{id}` `digest`
  (Task 1).
- Produces: a pure `insight-state` block with `summaryParts(sum)`,
  `linkLabel(l)`, `humanCount(n)`, `matches(s, q)`, `safeLink(url)`.

- [ ] **Step 1: Write the failing node test**

In `internal/server/dashboard_test.go`, add `TestDashboardInsightStates`,
built like `TestDashboardRemoteControlStates`: extract
`(?s)// insight-state:begin\n(.*?)// insight-state:end`, run it under node with
a JSON input, and compare:

| Call | Input | Want |
|---|---|---|
| `summaryParts` | `null` | `[]` |
| `summaryParts` | `{output_tokens: 371471}` | `[{text: "371k tokens out"}]` |
| `summaryParts` | git 3/2/1, link `!391` GitLab URL, cost 18.271 | `[{text:"3 commits"},{text:"2 uncommitted"},{text:"1 unpushed"},{text:"!391",href:"https://git.example.com/o/r/-/merge_requests/391"},{text:"$18.27"}]` |
| `summaryParts` | git 1/0/-1, link `#7`, tokens 1500000 | `[{text:"1 commit"},{text:"#7",href:"https://github.com/o/r/pull/7"},{text:"1.5M tokens out"}]` |
| `summaryParts` | link with URL `javascript:alert(1)` | `[{text:"#5"}]` (no `href`) |
| `matches` | session with recap `Next, merge MR 391.`, query `mr 391` | `true` |
| `matches` | same, query `[build]` against last prompt `why is the [build] red?` | `true` |
| `matches` | same, query `nothing` | `false` |
| `matches` | any session, query `""` | `true` |
| `safeLink` | `https://a.b/c`, `http://a.b`, `javascript:x`, `""` | `"https://a.b/c"`, `null`, `null`, `null` |

Extend `TestDashboardHTMLAndCSP` (or the test that checks for raw HTML
insertion) so the new code is covered: the page must not contain
`innerHTML`, `outerHTML`, `insertAdjacentHTML`, or `document.write`.

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./internal/server/ -run 'Dashboard' -count=1`
Expected: FAIL: `dashboard/index.html has no insight-state block`.

- [ ] **Step 3: Add the pure block**

In `index.html`, after `// rc-state:end`:

```js
  // insight-state:begin
  // safeLink returns url when it is an https link, else null. Only these
  // become href values.
  function safeLink(url) {
    return typeof url === "string" && /^https:\/\/[^\s]+$/.test(url) ? url : null;
  }

  function humanCount(n) {
    if (n >= 1000000) return (n / 1000000).toFixed(1) + "M";
    if (n >= 1000) return Math.floor(n / 1000) + "k";
    return String(n);
  }

  function linkLabel(l) {
    if (!l.number) return "link";
    return (/\/merge_requests\//.test(l.url) ? "!" : "#") + l.number;
  }

  // summaryParts is the card's summary line as [{text, href?}], each part
  // present only when known and non-zero.
  function summaryParts(sum) {
    var out = [];
    if (!sum) return out;
    var g = sum.git;
    if (g) {
      if (g.commit_count > 0) out.push({ text: g.commit_count + (g.commit_count === 1 ? " commit" : " commits") });
      if (g.uncommitted > 0) out.push({ text: g.uncommitted + " uncommitted" });
      if (g.unpushed > 0) out.push({ text: g.unpushed + " unpushed" });
    }
    if (sum.latest_link) {
      var part = { text: linkLabel(sum.latest_link) };
      var href = safeLink(sum.latest_link.url);
      if (href) part.href = href;
      out.push(part);
    }
    if (typeof sum.cost_usd === "number") out.push({ text: "$" + sum.cost_usd.toFixed(2) });
    else if (sum.output_tokens > 0) out.push({ text: humanCount(sum.output_tokens) + " tokens out" });
    return out;
  }

  // matches: q occurs, ignoring case, in the title, recap, last prompt,
  // branch, directory, or machine. Plain text, never a pattern.
  function matches(s, q) {
    if (!q) return true;
    q = q.toLowerCase();
    return [s.title, s.recap, s.last_prompt, s.git_branch, s.cwd, s.machine].some(function (f) {
      return typeof f === "string" && f.toLowerCase().indexOf(q) !== -1;
    });
  }
  // insight-state:end
```

- [ ] **Step 4: Render the card additions**

In `card(s, now)`, after the `where` meta line and before the report:

```js
    if (s.recap) {
      var rp = el("p", "recap");
      rp.appendChild(el("span", "label", "Recap" + (s.recap_at ? ", " + age(s.recap_at, now) : "") + ": "));
      rp.appendChild(document.createTextNode(s.recap));
      c.appendChild(rp);
    }
```

After the report block:

```js
    if (s.last_prompt) {
      var lp = el("p", "prompt");
      lp.appendChild(el("span", "label", "You" + (s.last_prompt_at ? ", " + age(s.last_prompt_at, now) : "") + ": "));
      lp.appendChild(document.createTextNode(s.last_prompt));
      c.appendChild(lp);
    }
    var parts = summaryParts(s.summary);
    if (parts.length) {
      var sl = el("div", "meta summary");
      parts.forEach(function (p, i) {
        if (i) sl.appendChild(document.createTextNode(" · "));
        if (p.href) {
          var a = el("a", null, p.text);
          a.href = p.href;
          a.target = "_blank";
          a.rel = "noopener noreferrer";
          sl.appendChild(a);
        } else {
          sl.appendChild(document.createTextNode(p.text));
        }
      });
      c.appendChild(sl);
    }
    c.appendChild(detailsFor(s, now));
```

Check `age(iso, now)`'s output format (for example "14 min ago" or "14m");
the label reads naturally either way. Add `detailsFor`, which fetches the
detail only when opened:

```js
  // detailsFor is the expandable part of a card: commits, links, usage. It
  // loads GET /v1/sessions/{id} the first time it opens.
  function detailsFor(s, now) {
    var d = el("details", "digest");
    d.appendChild(el("summary", null, "Details"));
    var body = el("div", null, "Loading...");
    d.appendChild(body);
    var loaded = false;
    d.addEventListener("toggle", function () {
      if (!d.open || loaded) return;
      loaded = true;
      fetch("/v1/sessions/" + encodeURIComponent(s.id), { headers: { "Accept": "application/json" }, cache: "no-store" })
        .then(function (resp) { if (!resp.ok) throw new Error("The server returned " + resp.status + "."); return resp.json(); })
        .then(function (det) { body.replaceChildren(digestView(det, now)); })
        .catch(function (err) { loaded = false; body.textContent = err.message || "Could not load details."; });
    });
    return d;
  }

  function digestView(det, now) {
    var frag = document.createDocumentFragment();
    var dg = det.digest;
    if (!dg) {
      frag.appendChild(el("p", "empty", "No digest yet."));
      return frag;
    }
    if (dg.git && dg.git.commit_count > 0) {
      frag.appendChild(el("h3", null, "Commits on " + (det.git_branch || "HEAD") + " during the session (" + dg.git.commit_count + ")"));
      var ul = el("ul");
      (dg.git.commits || []).forEach(function (c) { ul.appendChild(el("li", null, c.sha + "  " + c.subject)); });
      frag.appendChild(ul);
    }
    if (dg.links && dg.links.length) {
      frag.appendChild(el("h3", null, "Links (" + dg.links.length + ")"));
      var ll = el("ul");
      dg.links.forEach(function (l) {
        var li = el("li");
        var href = safeLink(l.url);
        if (href) {
          var a = el("a", null, linkLabel(l) + " " + l.url);
          a.href = href;
          a.target = "_blank";
          a.rel = "noopener noreferrer";
          li.appendChild(a);
        } else {
          li.textContent = linkLabel(l) + " " + l.url;
        }
        ll.appendChild(li);
      });
      frag.appendChild(ll);
    }
    var t = dg.tokens || {};
    frag.appendChild(el("p", "meta", "Tokens: input " + humanCount(t.input || 0) + " · output " + humanCount(t.output || 0) +
      " · cache read " + humanCount(t.cache_read || 0) + " · cache write " + humanCount(t.cache_write || 0)));
    if (typeof dg.cost_usd === "number") {
      frag.appendChild(el("p", "meta", "Cost: $" + dg.cost_usd.toFixed(2) + (dg.cost_at ? " as of " + new Date(dg.cost_at).toLocaleString() : "")));
    }
    frag.appendChild(el("p", "meta", "Last transcript entry " + age(dg.as_of, now)));
    return frag;
  }
```

- [ ] **Step 5: Add the filter box**

In the header, after `<span id="updated" ...>`:

```html
  <input id="filter" type="search" placeholder="Filter sessions" aria-label="Filter sessions" autocomplete="off">
```

In the script, keep the last list and filter before rendering:

```js
  var filterEl = document.getElementById("filter");
  var lastSessions = [];
```

In `load()`, replace `render(data);` with `lastSessions = data; render(data);`.
At the start of `render(sessions)`, keep the full list for `sessionsById` and
the Remote Control bookkeeping, then filter for display:

```js
    var q = filterEl.value.trim();
    var shown = sessions.filter(function (s) { return matches(s, q); });
```

Use `shown` instead of `sessions` in the `byMachine` loop, and when `shown`
is empty but `sessions` isn't, show `el("p", "empty", "No sessions match the filter.")`.
Add the listener after `load();`:

```js
  filterEl.addEventListener("input", function () { render(lastSessions); });
```

Add styles for `.recap`, `.prompt`, `.summary a`, `.label`, `#filter`, and
`details.digest` that follow the existing tokens and work at 360 px wide:
the filter takes the full header width on narrow screens, and long recap text
wraps (`overflow-wrap: anywhere`).

- [ ] **Step 6: Run the dashboard tests**

Run: `go test ./internal/server/ -run Dashboard -count=1 -v`
Expected: `PASS`, including the CSP hash test (the hashes are computed from
the file at startup, so no manual update is needed).

- [ ] **Step 7: Run the suite and commit**

Run: `make test lint`

```bash
git add internal/server/dashboard internal/server/dashboard_test.go
git commit -m "Show recap, last prompt, and digest summary on the dashboard, with a filter" -m "Refs: docs/dev/superpowers/plans/2026-09-30-session-insights.md, Task 8"
```

---

### Task 9: Deploy, backfill, and evidence

**Files:**
- Create: `docs/dev/evidence/session-insights.md`
- Modify: `README.md` (upgrade steps: `sessionhub digest --all` after deploy; rollback to v3)
- Modify: `docs/dev/SPEC.md` (a "Session insights" component entry, the reply-text rule)
- Modify: `docs/dev/PLAN.md` ("as built" notes where the implementation differs from the spec)
- Modify: `docs/dev/IDEAS.md` (live dollar cost with a pricing table, digest history)

This task runs on the real machines. Confirm with the user before deploying.

- [ ] **Step 1: Full suite**

Run: `make test lint`. Record the package count and `0` failures in the
evidence file.

- [ ] **Step 2: Deploy**

Run: `make deploy` (backs up `sessionhub.db` on `tower`, swaps the binary, restarts
the server, reinstalls the plugin), then on `bluebox`: `make install` and
`sessionhub install-plugin`. Record `sessionhub version` on both and
`sqlite3 ~/.local/share/sessionhub/sessionhub.db 'PRAGMA user_version'` on `tower` (`4`).

- [ ] **Step 3: Backfill**

Run `sessionhub digest --all` on `bluebox`, then `ssh tower '~/.local/bin/sessionhub digest --all'`.
Record the totals line from each. Expected: most sessions `sent`; sessions
whose transcripts Claude Code has deleted show `skipped: no transcript`;
`0 failed`. `sessionhub install-plugin` restarted the watcher, whose first
heartbeats digest up to 10 sessions each, so a few lines may read
`skipped: another run is in progress`. That's the per-session lock working,
not a failure; say so in the evidence.

- [ ] **Step 4: Check ended sessions**

Pick one ended session per machine that has a recap and a merge request link
(search with `sessionhub ls --all --grep <word from a recap>`). Record
`sessionhub show <id>` output with the recap, commits, links, tokens, and cost.
Confirm `sessionhub ls --all` still shows them as `ended` (Review Focus 1).

- [ ] **Step 5: Check the live path**

Open a scratch herdr workspace on `bluebox` in `/tmp/sessionhub-insights`, run a git repo
there, start `claude`, ask it to make one commit, and wait for the turn to
end. Within a minute, `sessionhub show <id>` shows `commits ... (1)` and the digest
time. Confirm on the dashboard (phone width) that the card shows the last
prompt and summary line, that **Details** opens and loads, and that the filter
narrows the list. Close the scratch workspace afterwards.

- [ ] **Step 6: Record and commit**

Write `docs/dev/evidence/session-insights.md` with the commands and outputs from
Steps 1 to 5 (no tokens). Update `README.md`, `docs/dev/SPEC.md`, `docs/dev/PLAN.md`, and
`docs/dev/IDEAS.md` as listed above. The `docs/dev/PLAN.md` as-built notes must record these
differences from the spec:

- The summary line prints `!391` or `#7`, not `MR !391`.
- The list's git counts sit under `summary.git`, not flat in `summary`.
- Git uses a 3-second budget (`digest.GitTimeout`), not `gitinfo`'s 500 ms:
  `git log` over a window can take longer than a branch lookup.
- The CLI flag is `sessionhub ls --grep`, not `sessionhub list --grep`.

In `docs/dev/IDEAS.md`, also add: prune `~/.local/state/sessionhub/digest/` (one `.json` and
one `.lock` per session, never removed) for sessions whose transcript is gone.

```bash
git add docs/dev/evidence/session-insights.md README.md docs/dev/SPEC.md docs/dev/PLAN.md docs/dev/IDEAS.md
git commit -m "Record session insights deploy, backfill, and live checks" -m "Refs: docs/dev/superpowers/plans/2026-09-30-session-insights.md, Task 9"
```
