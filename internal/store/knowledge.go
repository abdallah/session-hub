package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/knowledge"
)

// Knowledge limits. Entry text shares MaxInstructionRunes and every shares
// MaxInstructionEvery with the rules.
const (
	// MaxKnowledgeWhyRunes caps an entry's why.
	MaxKnowledgeWhyRunes = 200
	// MaxActiveKnowledge caps active entries.
	MaxActiveKnowledge = 500
	// MaxProposedKnowledge caps proposals waiting for a decision.
	MaxProposedKnowledge = 100
	// maxKnowledgeList caps ListKnowledge at the active limit, so an
	// unfiltered active list holds every active entry.
	maxKnowledgeList = MaxActiveKnowledge
	// defaultFactEvery is every for a fact or decision that sets none.
	defaultFactEvery = 20
	// maxClientIDRunes caps client_id and source_session.
	maxClientIDRunes = 100
)

const knowledgeCols = `k.id, k.kind, k.scope, k.text, k.why, k.state, k.every, k.replaces, k.source_session,
	k.created_at, k.created_by, k.decided_at, k.decided_by, k.confirmed_at`

// normalizeScope checks scope and returns it as global or <kind>:<value>.
func normalizeScope(scope string) (string, error) {
	kind, value, err := knowledge.ParseScope(strings.TrimSpace(scope))
	if err != nil {
		return "", invalidf("%v: use global, repo:<url>, ticket:<ref>, or machine:<name>", err)
	}
	if kind == "global" {
		return kind, nil
	}
	return kind + ":" + value, nil
}

// checkEntryText trims text and checks it as an entry's text.
func checkEntryText(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", invalidf("text is empty")
	}
	if err := checkText("text", text, MaxInstructionRunes); err != nil {
		return "", err
	}
	if knowledge.LooksSecret(text) {
		return "", invalidf("text looks like it holds a secret; store where the secret lives, not its value")
	}
	return text, nil
}

func isGlobalConvention(kind, scope string) bool {
	return kind == api.KnowledgeConvention && scope == "global"
}

// checkKnowledgeCount returns ErrFull when state already holds its limit.
func checkKnowledgeCount(ctx context.Context, tx *sql.Tx, state string) error {
	limit := MaxActiveKnowledge
	if state == api.KnowledgeProposed {
		limit = MaxProposedKnowledge
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge WHERE state = ?`, state).Scan(&n); err != nil {
		return err
	}
	if n >= limit {
		return fmt.Errorf("%w: %d %s entries, at most %d", ErrFull, n, state, limit)
	}
	return nil
}

// AddKnowledge stores an entry: active when active is set, proposed
// otherwise. A repeated in.ClientID returns the stored entry. It returns
// ErrInvalid for a bad kind, scope, text, why, every, or replaces, or text
// that looks like a secret, and ErrFull past the count limits or, for an
// active global convention, past MaxInstructionsRunes.
func (s *Store) AddKnowledge(ctx context.Context, in api.KnowledgeIn, createdBy string, active bool) (api.KnowledgeAdded, error) {
	var out api.KnowledgeAdded
	switch in.Kind {
	case api.KnowledgeConvention, api.KnowledgeFact, api.KnowledgeDecision:
	default:
		return out, invalidf("kind %q: use convention, fact, or decision", in.Kind)
	}
	scope, err := normalizeScope(in.Scope)
	if err != nil {
		return out, err
	}
	text, err := checkEntryText(in.Text)
	if err != nil {
		return out, err
	}
	why := strings.TrimSpace(in.Why)
	if err := checkText("why", why, MaxKnowledgeWhyRunes); err != nil {
		return out, err
	}
	if knowledge.LooksSecret(why) {
		return out, invalidf("why looks like it holds a secret")
	}
	every := 0
	if in.Every != nil {
		every = *in.Every
	} else if in.Kind != api.KnowledgeConvention {
		every = defaultFactEvery
	}
	if every < 0 || every > MaxInstructionEvery {
		return out, invalidf("every must be 0 to %d", MaxInstructionEvery)
	}
	if createdBy == "" {
		return out, invalidf("created_by is empty")
	}
	for name, v := range map[string]string{"created_by": createdBy, "client_id": in.ClientID, "source_session": in.SourceSession} {
		if err := checkText(name, v, maxClientIDRunes); err != nil {
			return out, err
		}
	}
	state := api.KnowledgeProposed
	if active {
		state = api.KnowledgeActive
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if in.ClientID != "" {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM knowledge WHERE client_id = ?`, in.ClientID).Scan(&id)
		if err == nil {
			if out.Entry, err = readKnowledge(ctx, tx, id); err != nil {
				return out, err
			}
			out.Similar, err = similarKnowledge(ctx, tx, id, out.Entry.Text)
			return out, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
	}
	if in.Replaces != 0 {
		if _, err := readKnowledge(ctx, tx, in.Replaces); errors.Is(err, ErrNotFound) {
			return out, invalidf("replaces: entry %d does not exist", in.Replaces)
		} else if err != nil {
			return out, err
		}
	}
	if err := checkKnowledgeCount(ctx, tx, state); err != nil {
		return out, err
	}
	if active && isGlobalConvention(in.Kind, scope) {
		if err := checkConventionRoom(ctx, tx, text); err != nil {
			return out, err
		}
	}
	now := formatTS(s.Now())
	res, err := tx.ExecContext(ctx, `INSERT INTO knowledge (client_id, kind, scope, text, why, state, every, replaces,
		source_session, created_at, created_by, confirmed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullIfEmpty(in.ClientID), in.Kind, scope, text, why, state, every, sql.NullInt64{Int64: in.Replaces, Valid: in.Replaces != 0},
		nullIfEmpty(in.SourceSession), now, createdBy, now)
	if err != nil {
		return out, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return out, err
	}
	if out.Entry, err = readKnowledge(ctx, tx, id); err != nil {
		return out, err
	}
	if out.Similar, err = similarKnowledge(ctx, tx, id, text); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func nullIfEmpty(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// DecideKnowledge approves or rejects proposed entry id. Approve may replace
// its text and scope, and retires the entry it replaces when that one is
// active. It returns ErrNotFound for an unknown entry and ErrInvalid for one
// that is not proposed.
func (s *Store) DecideKnowledge(ctx context.Context, id int64, in api.KnowledgeDecisionIn, by string) (api.Knowledge, error) {
	if by == "" {
		return api.Knowledge{}, invalidf("decided_by is empty")
	}
	if err := checkText("decided_by", by, maxCreatedByRunes); err != nil {
		return api.Knowledge{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Knowledge{}, err
	}
	defer tx.Rollback()
	k, err := readKnowledge(ctx, tx, id)
	if err != nil {
		return k, err
	}
	if k.State != api.KnowledgeProposed {
		return k, invalidf("entry %d is %s; only a proposed entry can be decided", id, k.State)
	}
	now := formatTS(s.Now())
	switch in.Decision {
	case "reject":
		_, err = tx.ExecContext(ctx, `UPDATE knowledge SET state = 'rejected', decided_at = ?, decided_by = ? WHERE id = ?`, now, by, id)
	case "approve":
		if in.Text != "" {
			if k.Text, err = checkEntryText(in.Text); err != nil {
				return k, err
			}
		}
		if in.Scope != "" {
			if k.Scope, err = normalizeScope(in.Scope); err != nil {
				return k, err
			}
		}
		if err := checkKnowledgeCount(ctx, tx, api.KnowledgeActive); err != nil {
			return k, err
		}
		if isGlobalConvention(k.Kind, k.Scope) {
			if err := checkConventionRoom(ctx, tx, k.Text); err != nil {
				return k, err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE knowledge SET state = 'active', text = ?, scope = ?, decided_at = ?, decided_by = ?,
			confirmed_at = ? WHERE id = ?`, k.Text, k.Scope, now, by, now, id)
		if err == nil && k.Replaces != 0 {
			// A target already retired or rejected stays as it is.
			_, err = tx.ExecContext(ctx, `UPDATE knowledge SET state = 'retired' WHERE id = ? AND state = 'active'`, k.Replaces)
		}
	default:
		return k, invalidf("decision %q: use approve or reject", in.Decision)
	}
	if err != nil {
		return k, err
	}
	if k, err = readKnowledge(ctx, tx, id); err != nil {
		return k, err
	}
	return k, tx.Commit()
}

// ConfirmKnowledge sets entry id's confirmed_at to now, or returns
// ErrNotFound.
func (s *Store) ConfirmKnowledge(ctx context.Context, id int64) error {
	return s.execOne(ctx, id, `UPDATE knowledge SET confirmed_at = ? WHERE id = ?`, formatTS(s.Now()), id)
}

// RetireKnowledge retires entry id, or returns ErrNotFound. Retiring a
// retired entry is a no-op.
func (s *Store) RetireKnowledge(ctx context.Context, id int64) error {
	return s.execOne(ctx, id, `UPDATE knowledge SET state = 'retired' WHERE id = ?`, id)
}

// execOne runs an update of entry id and returns ErrNotFound if it matched
// no row.
func (s *Store) execOne(ctx context.Context, id int64, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: knowledge entry %d", ErrNotFound, id)
	}
	return nil
}

// ListKnowledge returns up to 500 entries (MaxActiveKnowledge), newest first. An empty state,
// scope, or query matches all; query is an FTS5 query over text and why,
// and orders the result by rank.
func (s *Store) ListKnowledge(ctx context.Context, state, scope, query string) ([]api.Knowledge, error) {
	q := `SELECT ` + knowledgeCols + ` FROM knowledge k`
	var where []string
	var args []any
	if query != "" {
		q += ` JOIN knowledge_fts ON knowledge_fts.rowid = k.id`
		where = append(where, `knowledge_fts MATCH ?`)
		args = append(args, query)
	}
	if state != "" {
		switch state {
		case api.KnowledgeProposed, api.KnowledgeActive, api.KnowledgeRejected, api.KnowledgeRetired:
		default:
			return nil, invalidf("state %q: use proposed, active, rejected, or retired", state)
		}
		where = append(where, `k.state = ?`)
		args = append(args, state)
	}
	if scope != "" {
		norm, err := normalizeScope(scope)
		if err != nil {
			return nil, err
		}
		where = append(where, `k.scope = ?`)
		args = append(args, norm)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	if query != "" {
		q += ` ORDER BY bm25(knowledge_fts), k.id DESC`
	} else {
		q += ` ORDER BY k.id DESC`
	}
	q += fmt.Sprintf(` LIMIT %d`, maxKnowledgeList)
	out, err := queryKnowledge(ctx, s.db, q, args...)
	if err != nil && query != "" {
		// shortcut: any error with a query is blamed on the query; a real DB
		// fault reads as a 400 too. Match the fts5 error text if that misleads.
		return nil, invalidf("query %q: %v", query, err)
	}
	return out, err
}

var wordRE = regexp.MustCompile(`[\p{L}\p{N}]+`)

// similarKnowledge returns up to three active or proposed entries other than
// id that share a word of three or more characters with text, best first.
func similarKnowledge(ctx context.Context, tx *sql.Tx, id int64, text string) ([]api.Knowledge, error) {
	var words []string
	for _, w := range wordRE.FindAllString(text, -1) {
		if utf8.RuneCountInString(w) >= 3 {
			// Letters and digits only, so the quoted word is never FTS syntax.
			words = append(words, `"`+w+`"`)
		}
	}
	if len(words) == 0 {
		return []api.Knowledge{}, nil
	}
	return queryKnowledge(ctx, tx, `SELECT `+knowledgeCols+` FROM knowledge k JOIN knowledge_fts ON knowledge_fts.rowid = k.id
		WHERE knowledge_fts MATCH ? AND k.state IN ('active', 'proposed') AND k.id != ?
		ORDER BY bm25(knowledge_fts) LIMIT 3`, strings.Join(words, " OR "), id)
}

// KnowledgeLocal returns the active entries, their version, and the refs of
// tasks linked to machineID's live sessions.
func (s *Store) KnowledgeLocal(ctx context.Context, machineID int64) (api.KnowledgeLocal, error) {
	out := api.KnowledgeLocal{SessionRefs: map[string][]string{}}
	var err error
	out.Entries, err = queryKnowledge(ctx, s.db, `SELECT `+knowledgeCols+` FROM knowledge k WHERE k.state = 'active' ORDER BY k.id`)
	if err != nil {
		return out, err
	}
	h := sha256.New()
	for _, k := range out.Entries {
		fmt.Fprintf(h, "%d\x00%s\x00%s\x00%s\x00%s\x00%d\x00", k.ID, k.Kind, k.Scope, k.Text, k.Why, k.Every)
	}
	out.Version = hex.EncodeToString(h.Sum(nil))[:16]

	// Live: not ended and seen within staleAfter. A blocked session counts;
	// it is still running.
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT ts.session_id, t.ref FROM task_sessions ts
		JOIN tasks t ON t.id = ts.task_id JOIN sessions s ON s.id = ts.session_id
		WHERE s.machine_id = ? AND s.ended_at IS NULL AND s.last_seen_at >= ? AND t.ref != ''
		ORDER BY ts.session_id, t.ref`, machineID, formatTS(s.Now().Add(-s.staleAfter)))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, ref string
		if err := rows.Scan(&id, &ref); err != nil {
			return out, err
		}
		ref = knowledge.NormalizeRef(ref)
		if ref == "" || slices.Contains(out.SessionRefs[id], ref) {
			continue
		}
		out.SessionRefs[id] = append(out.SessionRefs[id], ref)
	}
	return out, rows.Err()
}

func readKnowledge(ctx context.Context, q querier, id int64) (api.Knowledge, error) {
	list, err := queryKnowledge(ctx, q, `SELECT `+knowledgeCols+` FROM knowledge k WHERE k.id = ?`, id)
	if err != nil {
		return api.Knowledge{}, err
	}
	if len(list) == 0 {
		return api.Knowledge{}, fmt.Errorf("%w: knowledge entry %d", ErrNotFound, id)
	}
	return list[0], nil
}

// queryKnowledge runs a SELECT of knowledgeCols. It never returns nil.
func queryKnowledge(ctx context.Context, q querier, query string, args ...any) ([]api.Knowledge, error) {
	out := []api.Knowledge{}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k api.Knowledge
		var replaces sql.NullInt64
		var source, decidedAt, decidedBy sql.NullString
		var created, confirmed string
		if err := rows.Scan(&k.ID, &k.Kind, &k.Scope, &k.Text, &k.Why, &k.State, &k.Every, &replaces, &source,
			&created, &k.CreatedBy, &decidedAt, &decidedBy, &confirmed); err != nil {
			return nil, err
		}
		k.Replaces, k.SourceSession, k.DecidedBy = replaces.Int64, source.String, decidedBy.String
		if k.CreatedAt, err = parseTS(created); err != nil {
			return nil, err
		}
		if k.ConfirmedAt, err = parseTS(confirmed); err != nil {
			return nil, err
		}
		if k.DecidedAt, err = parseNullTS(decidedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
