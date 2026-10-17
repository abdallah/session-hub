package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
)

// Shared instruction limits. See docs/server.md, "Shared instructions".
const (
	// MaxInstructionRunes caps one rule.
	MaxInstructionRunes = 300
	// MaxInstructionsRunes caps the whole list.
	MaxInstructionsRunes = 2000
	// MaxInstructionEvery caps a rule's every: at most every 100th prompt.
	MaxInstructionEvery = 100
	// maxCreatedByRunes caps created_by: a machine name or web:<name>.
	maxCreatedByRunes = 100
)

// querier is the part of *sql.DB and *sql.Tx the list read needs.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Instructions returns every rule (active global convention), oldest first,
// and the list's version.
func (s *Store) Instructions(ctx context.Context) (api.InstructionList, error) {
	return readInstructions(ctx, s.db)
}

func readInstructions(ctx context.Context, q querier) (api.InstructionList, error) {
	out := api.InstructionList{Instructions: []api.Instruction{}}
	rows, err := q.QueryContext(ctx, `SELECT id, text, created_at, created_by, every FROM knowledge
		WHERE state = 'active' AND kind = 'convention' AND scope = 'global' ORDER BY id`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var in api.Instruction
		var created string
		if err := rows.Scan(&in.ID, &in.Text, &created, &in.CreatedBy, &in.Every); err != nil {
			return out, err
		}
		if in.CreatedAt, err = parseTS(created); err != nil {
			return out, err
		}
		out.Instructions = append(out.Instructions, in)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.Version = instructionsVersion(out.Instructions)
	return out, nil
}

// instructionsVersion is the first 16 hex digits of a SHA-256 over each
// rule's ID, text, and every.
func instructionsVersion(list []api.Instruction) string {
	h := sha256.New()
	for _, in := range list {
		fmt.Fprintf(h, "%d\x00%s\x00", in.ID, in.Text)
		if in.Every != 0 {
			// Only when set, so the version of a list without one stays the same.
			fmt.Fprintf(h, "every=%d\x00", in.Every)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// AddInstruction stores a rule as an active global convention: the
// remember path. text is trimmed and must be 1 to MaxInstructionRunes
// characters with no control characters. every is 0 (every prompt) to
// MaxInstructionEvery. A rule that would take the list past
// MaxInstructionsRunes returns ErrFull.
func (s *Store) AddInstruction(ctx context.Context, text, createdBy string, every int) (api.Instruction, error) {
	added, err := s.AddKnowledge(ctx, api.KnowledgeIn{Kind: api.KnowledgeConvention, Scope: "global", Text: text, Every: &every}, createdBy, true)
	if err != nil {
		return api.Instruction{}, err
	}
	k := added.Entry
	return api.Instruction{ID: k.ID, Text: k.Text, CreatedAt: k.CreatedAt, CreatedBy: k.CreatedBy, Every: k.Every}, nil
}

// checkConventionRoom returns ErrFull when adding text to the active global
// conventions would take them past MaxInstructionsRunes.
func checkConventionRoom(ctx context.Context, q querier, text string) error {
	cur, err := readInstructions(ctx, q)
	if err != nil {
		return err
	}
	used := 0
	for _, in := range cur.Instructions {
		used += utf8.RuneCountInString(in.Text)
	}
	if n := utf8.RuneCountInString(text); used+n > MaxInstructionsRunes {
		return fmt.Errorf("%w: the rules use %d of %d characters and this one has %d; remove a rule first",
			ErrFull, used, MaxInstructionsRunes, n)
	}
	return nil
}

// DeleteInstruction retires rule id, or returns ErrNotFound if it is not an
// active global convention.
func (s *Store) DeleteInstruction(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE knowledge SET state = 'retired'
		WHERE id = ? AND state = 'active' AND kind = 'convention' AND scope = 'global'`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: rule %d", ErrNotFound, id)
	}
	return nil
}
