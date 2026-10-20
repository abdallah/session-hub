package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// rollbackV17 restores the instructions table schema v17 replaced, so a test
// can set user_version to 16 or lower and reopen the file as an older release
// left it.
func rollbackV17(t *testing.T, s *Store) {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE instructions (
			id         INTEGER PRIMARY KEY,
			text       TEXT NOT NULL,
			created_at TEXT NOT NULL,
			created_by TEXT NOT NULL,
			every      INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO instructions (id, text, created_at, created_by, every)
			SELECT id, text, created_at, created_by, every FROM knowledge
			WHERE state = 'active' AND kind = 'convention' AND scope = 'global'`,
		"DROP TABLE knowledge_fts",
		"DROP TABLE knowledge",
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestMigrateV16ToV17(t *testing.T) {
	s, path := openTemp(t)
	rollbackV17(t, s)
	for _, q := range []string{
		`INSERT INTO instructions (text, created_at, created_by) VALUES ('Be brief.', '2026-10-01T00:00:00Z', 'tower')`,
		`INSERT INTO instructions (text, created_at, created_by, every) VALUES ('Recheck the plan.', '2026-10-02T00:00:00Z', 'web:phone', 10)`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old, err := readInstructionsTable(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 16"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	ctx := context.Background()
	list, err := s2.Instructions(ctx)
	if err != nil || len(list.Instructions) != 2 {
		t.Fatalf("after migrate: %+v %v", list, err)
	}
	a, b := list.Instructions[0], list.Instructions[1]
	if a.Text != "Be brief." || a.Every != 0 || a.CreatedBy != "tower" || b.Text != "Recheck the plan." || b.Every != 10 {
		t.Errorf("rules after migrate: %+v", list.Instructions)
	}
	if list.Version != old {
		t.Errorf("version %q after migrate, want %q as before", list.Version, old)
	}
	ks, err := s2.ListKnowledge(ctx, api.KnowledgeActive, "global", "")
	if err != nil || len(ks) != 2 {
		t.Fatalf("knowledge after migrate: %+v %v", ks, err)
	}
	for _, k := range ks {
		if k.Kind != api.KnowledgeConvention || !k.ConfirmedAt.Equal(k.CreatedAt) {
			t.Errorf("migrated entry %+v", k)
		}
	}
	// The FTS index holds the copied rows.
	if hits, err := s2.ListKnowledge(ctx, "", "", "recheck"); err != nil || len(hits) != 1 {
		t.Errorf("search after migrate: %+v %v", hits, err)
	}
}

// readInstructionsTable is the version a v16 release computes from its
// instructions table.
func readInstructionsTable(s *Store) (string, error) {
	rows, err := s.db.Query(`SELECT id, text, every FROM instructions ORDER BY id`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var list []api.Instruction
	for rows.Next() {
		var in api.Instruction
		if err := rows.Scan(&in.ID, &in.Text, &in.Every); err != nil {
			return "", err
		}
		list = append(list, in)
	}
	return instructionsVersion(list), rows.Err()
}

func addK(t *testing.T, s *Store, in api.KnowledgeIn, by string, active bool) api.KnowledgeAdded {
	t.Helper()
	got, err := s.AddKnowledge(context.Background(), in, by, active)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAddKnowledgeProposedAndActive(t *testing.T) {
	e := newControlEnv(t)
	p := addK(t, e.s, api.KnowledgeIn{ClientID: "k1", Kind: "fact", Scope: "ticket:SYSTEMSDEV-17908", Text: " The bot runs in eu-west-1. ", SourceSession: "s1"}, "tower", false)
	k := p.Entry
	if k.ID == 0 || k.State != api.KnowledgeProposed || k.Scope != "ticket:systemsdev-17908" || k.Text != "The bot runs in eu-west-1." ||
		k.Every != 20 || k.SourceSession != "s1" || k.CreatedBy != "tower" || !k.CreatedAt.Equal(e.clock.Now()) || !k.ConfirmedAt.Equal(k.CreatedAt) {
		t.Errorf("proposed %+v", k)
	}
	if p.Similar == nil || len(p.Similar) != 0 {
		t.Errorf("similar %+v, want an empty slice", p.Similar)
	}
	again := addK(t, e.s, api.KnowledgeIn{ClientID: "k1", Kind: "fact", Scope: "global", Text: "Something else."}, "tower", false)
	if again.Entry.ID != k.ID || again.Entry.Text != k.Text {
		t.Errorf("replay %+v, want entry %d", again.Entry, k.ID)
	}
	a := addK(t, e.s, api.KnowledgeIn{Kind: "convention", Scope: "global", Text: "Use British spelling."}, "web:phone", true).Entry
	if a.State != api.KnowledgeActive || a.Every != 0 {
		t.Errorf("active %+v", a)
	}
	zero := 0
	if d := addK(t, e.s, api.KnowledgeIn{Kind: "decision", Scope: "global", Text: "Keep SQLite.", Every: &zero}, "tower", false).Entry; d.Every != 0 {
		t.Errorf("explicit every 0 became %d", d.Every)
	}
}

func TestAddKnowledgeRejects(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	big := 101
	for name, in := range map[string]api.KnowledgeIn{
		"kind":   {Kind: "rule", Scope: "global", Text: "x"},
		"scope":  {Kind: "fact", Scope: "team:x", Text: "x"},
		"empty":  {Kind: "fact", Scope: "global", Text: "  "},
		"text":   {Kind: "fact", Scope: "global", Text: strings.Repeat("x", 301)},
		"why":    {Kind: "fact", Scope: "global", Text: "x", Why: strings.Repeat("y", 201)},
		"every":  {Kind: "fact", Scope: "global", Text: "x", Every: &big},
		"secret": {Kind: "fact", Scope: "global", Text: "the key is AKIAIOSFODNN7EXAMPLE"},
		"ctrl":   {Kind: "fact", Scope: "global", Text: "one\ntwo"},
	} {
		if _, err := e.s.AddKnowledge(ctx, in, "tower", false); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	for i := 0; i < MaxProposedKnowledge; i++ {
		addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: fmt.Sprintf("fact %d", i)}, "tower", false)
	}
	if _, err := e.s.AddKnowledge(ctx, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "one more"}, "tower", false); !errors.Is(err, ErrFull) {
		t.Errorf("proposal %d: %v, want ErrFull", MaxProposedKnowledge+1, err)
	}
}

// TestGlobalConventionCap: the 2,000-character total of active global
// conventions holds in AddInstruction, AddKnowledge, and DecideKnowledge.
func TestGlobalConventionCap(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	rule := strings.Repeat("é", MaxInstructionRunes)
	for i := 0; i < 6; i++ {
		if _, err := e.s.AddInstruction(ctx, rule, "tower", 0); err != nil {
			t.Fatal(err)
		}
	}
	// 1,800 used: 201 more is past the cap.
	over := strings.Repeat("z", 201)
	if _, err := e.s.AddKnowledge(ctx, api.KnowledgeIn{Kind: "convention", Scope: "global", Text: over}, "web:phone", true); !errors.Is(err, ErrFull) {
		t.Errorf("active global convention past the cap: %v, want ErrFull", err)
	}
	// Other kinds and scopes do not count.
	addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: over}, "web:phone", true)
	addK(t, e.s, api.KnowledgeIn{Kind: "convention", Scope: "machine:tower", Text: over}, "web:phone", true)
	p := addK(t, e.s, api.KnowledgeIn{Kind: "convention", Scope: "global", Text: over}, "tower", false).Entry
	if _, err := e.s.DecideKnowledge(ctx, p.ID, api.KnowledgeDecisionIn{Decision: "approve"}, "web:phone"); !errors.Is(err, ErrFull) {
		t.Errorf("approve past the cap: %v, want ErrFull", err)
	}
	if _, err := e.s.DecideKnowledge(ctx, p.ID, api.KnowledgeDecisionIn{Decision: "approve", Text: strings.Repeat("z", 200)}, "web:phone"); err != nil {
		t.Errorf("approve at exactly 2,000: %v", err)
	}
}

func TestDecideKnowledge(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	old := addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "Deploys run on Fridays."}, "web:phone", true).Entry
	p := addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "Deploys run on Monday.", Replaces: old.ID}, "tower", false).Entry
	e.clock.Advance(time.Hour)
	got, err := e.s.DecideKnowledge(ctx, p.ID, api.KnowledgeDecisionIn{Decision: "approve", Text: " Deploys run on Mondays. ", Scope: "repo:git@gitlab.com:Group/Repo.git"}, "web:phone")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != api.KnowledgeActive || got.Text != "Deploys run on Mondays." || got.Scope != "repo:gitlab.com/group/repo" ||
		got.DecidedBy != "web:phone" || got.DecidedAt == nil || !got.DecidedAt.Equal(e.clock.Now()) || !got.ConfirmedAt.Equal(e.clock.Now()) {
		t.Errorf("approved %+v", got)
	}
	if l, _ := e.s.ListKnowledge(ctx, api.KnowledgeRetired, "", ""); len(l) != 1 || l[0].ID != old.ID {
		t.Errorf("retired after approve: %+v, want entry %d", l, old.ID)
	}
	// A replaces target that is already retired stays retired.
	p2 := addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "Deploys run on Tuesdays.", Replaces: old.ID}, "tower", false).Entry
	if _, err := e.s.DecideKnowledge(ctx, p2.ID, api.KnowledgeDecisionIn{Decision: "approve"}, "web:phone"); err != nil {
		t.Fatal(err)
	}
	if l, _ := e.s.ListKnowledge(ctx, api.KnowledgeRetired, "", ""); len(l) != 1 || l[0].ID != old.ID {
		t.Errorf("retired after second approve: %+v", l)
	}
	// Only a proposed entry can be decided.
	if _, err := e.s.DecideKnowledge(ctx, p.ID, api.KnowledgeDecisionIn{Decision: "reject"}, "web:phone"); !errors.Is(err, ErrInvalid) {
		t.Errorf("deciding an active entry: %v, want ErrInvalid", err)
	}
	r := addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "Wrong."}, "tower", false).Entry
	if _, err := e.s.DecideKnowledge(ctx, r.ID, api.KnowledgeDecisionIn{Decision: "maybe"}, "web:phone"); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown decision: %v, want ErrInvalid", err)
	}
	if got, err := e.s.DecideKnowledge(ctx, r.ID, api.KnowledgeDecisionIn{Decision: "reject"}, "web:phone"); err != nil || got.State != api.KnowledgeRejected {
		t.Errorf("reject: %+v %v", got, err)
	}
	if _, err := e.s.DecideKnowledge(ctx, 9999, api.KnowledgeDecisionIn{Decision: "reject"}, "web:phone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown entry: %v, want ErrNotFound", err)
	}
	e.clock.Advance(time.Hour)
	if err := e.s.ConfirmKnowledge(ctx, got.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := e.s.ListKnowledge(ctx, api.KnowledgeActive, "repo:https://user@gitlab.com/group/repo", ""); len(l) != 1 || !l[0].ConfirmedAt.Equal(e.clock.Now()) {
		t.Errorf("after confirm: %+v", l)
	}
	if err := e.s.ConfirmKnowledge(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("confirm unknown: %v, want ErrNotFound", err)
	}
}

// Search takes plain words, not FTS5 syntax: punctuation in natural queries
// splits words, and every word must match.
func TestKnowledgeSearchNaturalQueries(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	k := addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "systemsdev-17908: telegram-bot secret is prod/telegram, see repo:x"}, "web:phone", true).Entry
	addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "Grafana lives on togh."}, "web:phone", true)
	for _, q := range []string{"systemsdev-17908", "telegram-bot", "prod/telegram", "repo:x", "telegram secret?", `"telegram`} {
		if l, err := e.s.ListKnowledge(ctx, "", "", q); err != nil || len(l) != 1 || l[0].ID != k.ID {
			t.Errorf("q %q: %+v %v, want entry %d", q, l, err, k.ID)
		}
	}
	// "?", "-/:" and `"` have no words; "where is it?" has words the entry lacks.
	for _, q := range []string{"?", "-/:", `"`, "where is it?"} {
		if l, err := e.s.ListKnowledge(ctx, "", "", q); err != nil || len(l) != 0 {
			t.Errorf("q %q: %+v %v, want no rows and no error", q, l, err)
		}
	}
}

func TestKnowledgeSearch(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	k := addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "telegram bot token in prod/telegram-bot"}, "web:phone", true).Entry
	addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "Grafana lives on togh."}, "web:phone", true)
	if l, err := e.s.ListKnowledge(ctx, api.KnowledgeActive, "", "telegram"); err != nil || len(l) != 1 || l[0].ID != k.ID {
		t.Fatalf("search: %+v %v", l, err)
	}
	if l, _ := e.s.ListKnowledge(ctx, "", "", ""); len(l) != 2 || l[0].ID == k.ID {
		t.Errorf("list all, newest first: %+v", l)
	}
	if err := e.s.RetireKnowledge(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if l, err := e.s.ListKnowledge(ctx, api.KnowledgeActive, "", "telegram"); err != nil || len(l) != 0 {
		t.Errorf("search after retire: %+v %v", l, err)
	}
	if err := e.s.RetireKnowledge(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("retire unknown: %v, want ErrNotFound", err)
	}
	if _, err := e.s.ListKnowledge(ctx, "lost", "", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad state: %v, want ErrInvalid", err)
	}
}

func TestKnowledgeLocal(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	e.session(e.tower, "s2", "")
	e.session(e.bluebox, "s3", "")
	e.task(api.TaskIn{Title: "Bot", Ref: "SYSTEMSDEV-17908", SessionID: "s1"}, tYou)
	e.task(api.TaskIn{Title: "Other", Ref: "systemsdev-1", SessionID: "s3"}, tYou)
	e.task(api.TaskIn{Title: "Review", Ref: "!34", SessionID: "s2"}, tYou)
	act := addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "ticket:systemsdev-17908", Text: "Bot runs in eu-west-1."}, "web:phone", true).Entry
	addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "Not yet approved."}, "tower", false)
	got, err := e.s.KnowledgeLocal(ctx, e.tower.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 1 || got.Entries[0].ID != act.ID {
		t.Errorf("entries %+v, want only the active one", got.Entries)
	}
	if fmt.Sprint(got.SessionRefs) != "map[s1:[systemsdev-17908] s2:[!34]]" {
		t.Errorf("session_refs %v", got.SessionRefs)
	}
	if len(got.Version) != 16 {
		t.Errorf("version %q", got.Version)
	}
	again, _ := e.s.KnowledgeLocal(ctx, e.tower.ID)
	if again.Version != got.Version {
		t.Errorf("unchanged entries changed version")
	}
	addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "New fact."}, "web:phone", true)
	if changed, _ := e.s.KnowledgeLocal(ctx, e.tower.ID); changed.Version == got.Version {
		t.Errorf("a new entry kept version %q", got.Version)
	}
	// A stale session is not live.
	e.clock.Advance(DefaultStaleAfter + time.Minute)
	if stale, _ := e.s.KnowledgeLocal(ctx, e.tower.ID); len(stale.SessionRefs) != 0 || stale.SessionRefs == nil {
		t.Errorf("session_refs of stale sessions: %v", stale.SessionRefs)
	}
}

func TestKnowledgeSimilar(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	k := addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "The telegram bot token lives in prod/telegram-bot."}, "web:phone", true).Entry
	addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "Grafana lives on togh."}, "web:phone", true)
	got, err := e.s.AddKnowledge(ctx, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: `The "telegram" bot token is in prod/telegram-bot* (NEAR`}, "tower", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Similar) == 0 || got.Similar[0].ID != k.ID {
		t.Errorf("similar %+v, want entry %d first", got.Similar, k.ID)
	}
	for _, s := range got.Similar {
		if s.ID == got.Entry.ID {
			t.Errorf("similar lists the new entry itself")
		}
	}
	if none := addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "xy"}, "tower", false); none.Similar == nil || len(none.Similar) != 0 {
		t.Errorf("similar for short words: %+v", none.Similar)
	}
}

// TestListKnowledgeAllActive: the dashboard reads every active entry in one
// list, so the oldest, which are the ones due for review, are never cut.
func TestListKnowledgeAllActive(t *testing.T) {
	e := newControlEnv(t)
	const n = 250
	first := addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "entry 0"}, "web:phone", true).Entry
	for i := 1; i < n; i++ {
		addK(t, e.s, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: fmt.Sprintf("entry %d", i)}, "web:phone", true)
	}
	list, err := e.s.ListKnowledge(context.Background(), api.KnowledgeActive, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != n || list[len(list)-1].ID != first.ID {
		t.Errorf("listed %d entries, want %d ending with the oldest (id %d)", len(list), n, first.ID)
	}
}
