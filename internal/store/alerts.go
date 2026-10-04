package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LastAlerts returns, for each session that was alerted on, the since of the
// blocked item it was last alerted on. See docs/server.md, "Inbox alerts".
func (s *Store) LastAlerts(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, since FROM inbox_alerts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id, since string
		if err := rows.Scan(&id, &since); err != nil {
			return nil, err
		}
		t, err := parseTS(since)
		if err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// RecordAlert stores that session id was alerted on the blocked item with
// this since. One row per session: a later alert replaces it. An unknown
// session returns ErrNotFound.
func (s *Store) RecordAlert(ctx context.Context, id string, since time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_alerts (session_id, since, sent_at) VALUES (?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET since = excluded.since, sent_at = excluded.sent_at`,
		id, formatTS(since), formatTS(s.Now())); err != nil {
		return err
	}
	return tx.Commit()
}
