package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
)

// Shared instruction limits. See docs/server.md, "Shared instructions".
const (
	// MaxInstructionRunes caps one rule.
	MaxInstructionRunes = 300
	// MaxInstructionsRunes caps the whole list.
	MaxInstructionsRunes = 2000
	// maxCreatedByRunes caps created_by: a machine name or web:<name>.
	maxCreatedByRunes = 100
)

// querier is the part of *sql.DB and *sql.Tx the list read needs.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Instructions returns every rule, oldest first, and the list's version.
func (s *Store) Instructions(ctx context.Context) (api.InstructionList, error) {
	return readInstructions(ctx, s.db)
}

func readInstructions(ctx context.Context, q querier) (api.InstructionList, error) {
	out := api.InstructionList{Instructions: []api.Instruction{}}
	rows, err := q.QueryContext(ctx, `SELECT id, text, created_at, created_by FROM instructions ORDER BY id`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var in api.Instruction
		var created string
		if err := rows.Scan(&in.ID, &in.Text, &created, &in.CreatedBy); err != nil {
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
// rule's ID and text.
func instructionsVersion(list []api.Instruction) string {
	h := sha256.New()
	for _, in := range list {
		fmt.Fprintf(h, "%d\x00%s\x00", in.ID, in.Text)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// AddInstruction stores a rule. text is trimmed and must be 1 to
// MaxInstructionRunes characters with no control characters. A rule that
// would take the list past MaxInstructionsRunes returns ErrFull.
func (s *Store) AddInstruction(ctx context.Context, text, createdBy string) (api.Instruction, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return api.Instruction{}, invalidf("text is empty")
	}
	if err := checkText("text", text, MaxInstructionRunes); err != nil {
		return api.Instruction{}, err
	}
	if createdBy == "" {
		return api.Instruction{}, invalidf("created_by is empty")
	}
	if err := checkText("created_by", createdBy, maxCreatedByRunes); err != nil {
		return api.Instruction{}, err
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Instruction{}, err
	}
	defer tx.Rollback()
	cur, err := readInstructions(ctx, tx)
	if err != nil {
		return api.Instruction{}, err
	}
	used := 0
	for _, in := range cur.Instructions {
		used += utf8.RuneCountInString(in.Text)
	}
	if n := utf8.RuneCountInString(text); used+n > MaxInstructionsRunes {
		return api.Instruction{}, fmt.Errorf("%w: the rules use %d of %d characters and this one has %d; remove a rule first",
			ErrFull, used, MaxInstructionsRunes, n)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO instructions (text, created_at, created_by) VALUES (?, ?, ?)`,
		text, formatTS(now), createdBy)
	if err != nil {
		return api.Instruction{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return api.Instruction{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Instruction{}, err
	}
	return api.Instruction{ID: id, Text: text, CreatedAt: now, CreatedBy: createdBy}, nil
}

// DeleteInstruction removes rule id, or returns ErrNotFound.
func (s *Store) DeleteInstruction(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM instructions WHERE id = ?`, id)
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
