package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestMigrateV7ToV8(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	if _, _, err := s.AddMachine(ctx, "tower", "", ""); err != nil {
		t.Fatal(err)
	}
	rollbackV8(t, s)
	if _, err := s.db.Exec("PRAGMA user_version = 7"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var v int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version %d %v, want %d", v, err, schemaVersion)
	}
	if ms, _ := s2.ListMachines(ctx); len(ms) != 1 {
		t.Errorf("machines after upgrade: %+v", ms)
	}
	if _, err := s2.AddInstruction(ctx, "Use British spelling.", "tower"); err != nil {
		t.Errorf("AddInstruction after upgrade: %v", err)
	}
}

func TestInstructions(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	empty, err := e.s.Instructions(ctx)
	if err != nil || empty.Instructions == nil || len(empty.Instructions) != 0 || empty.Version == "" {
		t.Fatalf("empty list: %+v %v", empty, err)
	}
	a, err := e.s.AddInstruction(ctx, "  Never push to main.  ", "tower")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == 0 || a.Text != "Never push to main." || a.CreatedBy != "tower" || !a.CreatedAt.Equal(e.clock.Now()) {
		t.Errorf("added %+v", a)
	}
	e.clock.Advance(time.Second)
	b, err := e.s.AddInstruction(ctx, "Write in British English.", "web:phone")
	if err != nil {
		t.Fatal(err)
	}
	list, err := e.s.Instructions(ctx)
	if err != nil || len(list.Instructions) != 2 || list.Instructions[0].ID != a.ID || list.Instructions[1].ID != b.ID {
		t.Fatalf("list %+v %v", list, err)
	}
	if list.Version == empty.Version || len(list.Version) != 16 {
		t.Errorf("version %q did not change from %q, or is not 16 hex digits", list.Version, empty.Version)
	}
	again, _ := e.s.Instructions(ctx)
	if again.Version != list.Version {
		t.Errorf("an unchanged list changed version: %q then %q", list.Version, again.Version)
	}
	if err := e.s.DeleteInstruction(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteInstruction(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v, want ErrNotFound", err)
	}
	list, _ = e.s.Instructions(ctx)
	if len(list.Instructions) != 1 || list.Instructions[0].ID != b.ID {
		t.Errorf("after delete: %+v", list)
	}
}

func TestInstructionLimits(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	for name, text := range map[string]string{
		"empty":    "   ",
		"too long": strings.Repeat("x", MaxInstructionRunes+1),
		"newline":  "one\ntwo",
		"escape":   "red \x1b[31m",
	} {
		if _, err := e.s.AddInstruction(ctx, text, "tower"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := e.s.AddInstruction(ctx, "ok", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty created_by: %v, want ErrInvalid", err)
	}
	// 300 characters is allowed; the list holds six of them (1,800), and a
	// seventh would pass 2,000.
	rule := strings.Repeat("é", MaxInstructionRunes)
	for i := 0; i < 6; i++ {
		if _, err := e.s.AddInstruction(ctx, rule, "tower"); err != nil {
			t.Fatalf("rule %d: %v", i, err)
		}
	}
	if _, err := e.s.AddInstruction(ctx, rule, "tower"); !errors.Is(err, ErrFull) {
		t.Fatalf("past 2,000 characters: %v, want ErrFull", err)
	}
	if _, err := e.s.AddInstruction(ctx, strings.Repeat("y", 200), "tower"); err != nil {
		t.Errorf("exactly 2,000 characters: %v", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM instructions`); n != 7 {
		t.Errorf("%d rows, want 7", n)
	}
	var list api.InstructionList
	list, _ = e.s.Instructions(ctx)
	if len(list.Instructions) != 7 {
		t.Errorf("list has %d rules, want 7", len(list.Instructions))
	}
}
