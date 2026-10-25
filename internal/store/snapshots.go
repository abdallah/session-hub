package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// Snapshot limits. See docs/server.md, "Snapshots".
const (
	// MaxSnapshotBytes caps the snapshots the server keeps, in total.
	MaxSnapshotBytes int64 = 1 << 30
)

// snapshotTarget checks that machineID may store a snapshot of session
// sessionID sealed for recipient: the session is machineID's and has not
// ended, and recipient is another machine with a move key. It returns the
// recipient's ID.
func snapshotTarget(ctx context.Context, q queryer, machineID int64, sessionID, recipient string) (int64, error) {
	if !ValidSessionID(sessionID) {
		return 0, invalidf("session id %q: use 1-128 letters, digits, '_', or '-', starting with a letter or digit", sessionID)
	}
	if !ValidMachineName(recipient) {
		return 0, invalidf("machine %q: want a machine name", recipient)
	}
	var owner int64
	var ended sql.NullString
	err := q.QueryRowContext(ctx, `SELECT machine_id, ended_at FROM sessions WHERE id = ?`, sessionID).Scan(&owner, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: session %s", ErrNotFound, sessionID)
	}
	if err != nil {
		return 0, err
	}
	switch {
	case owner != machineID:
		return 0, fmt.Errorf("%w: session %s is on another machine", ErrWrongMachine, sessionID)
	case ended.Valid:
		return 0, refusef("session %s has ended", sessionID)
	}
	var rid int64
	var key string
	err = q.QueryRowContext(ctx, `SELECT id, move_key FROM machines WHERE name = ?`, recipient).Scan(&rid, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, refusef("no machine named %q", recipient)
	}
	if err != nil {
		return 0, err
	}
	switch {
	case rid == machineID:
		return 0, refusef("a snapshot is sealed for another machine, not %q", recipient)
	case key == "":
		return 0, refusef("machine %q has no move key yet", recipient)
	}
	return rid, nil
}

// queryer is what snapshotTarget reads through: the database or a
// transaction.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CheckSnapshot checks, before the upload, that machineID may store a
// snapshot of session sessionID for recipient.
func (s *Store) CheckSnapshot(ctx context.Context, machineID int64, sessionID, recipient string) error {
	_, err := snapshotTarget(ctx, s.db, machineID, sessionID, recipient)
	return err
}

// PutSnapshot records snapshot id of session sessionID, size bytes, sealed
// for recipient, taken now. It replaces the session's snapshot for that
// recipient and returns the replaced snapshot's ID, or "". An id already used
// by another snapshot or a move is refused.
func (s *Store) PutSnapshot(ctx context.Context, machineID int64, sessionID, recipient, id string, size int64) (string, error) {
	if err := checkMoveID(id); err != nil {
		return "", err
	}
	if size <= 0 || size > api.MaxSealedBundle {
		return "", invalidf("snapshot size %d: want 1 to %d bytes", size, api.MaxSealedBundle)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	rid, err := snapshotTarget(ctx, tx, machineID, sessionID, recipient)
	if err != nil {
		return "", err
	}
	var used int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM snapshots WHERE id = ?) + (SELECT COUNT(*) FROM moves WHERE id = ?)`,
		id, id).Scan(&used); err != nil {
		return "", err
	}
	if used > 0 {
		return "", refusef("snapshot id %s is already used", id)
	}
	var old string
	err = tx.QueryRowContext(ctx, `SELECT id FROM snapshots WHERE session_id = ? AND recipient_machine_id = ?`, sessionID, rid).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE session_id = ? AND recipient_machine_id = ?`, sessionID, rid); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO snapshots (id, session_id, source_machine_id, recipient_machine_id, taken_at, size)
		VALUES (?, ?, ?, ?, ?, ?)`, id, sessionID, machineID, rid, formatTS(s.Now()), size); err != nil {
		return "", err
	}
	return old, tx.Commit()
}

// sessionSnapshots returns the snapshots of session id, or of every session
// when id is "", by session ID, ordered by machine name.
func (s *Store) sessionSnapshots(ctx context.Context, id string) (map[string][]api.Snapshot, error) {
	q := `SELECT sn.session_id, m.name, sn.taken_at, sn.size FROM snapshots sn JOIN machines m ON m.id = sn.recipient_machine_id`
	var args []any
	if id != "" {
		q += " WHERE sn.session_id = ?"
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY m.name", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]api.Snapshot{}
	for rows.Next() {
		var sid, taken string
		var sn api.Snapshot
		if err := rows.Scan(&sid, &sn.Machine, &taken, &sn.Size); err != nil {
			return nil, err
		}
		if sn.TakenAt, err = parseTS(taken); err != nil {
			return nil, err
		}
		out[sid] = append(out[sid], sn)
	}
	return out, rows.Err()
}

// SweepSnapshots deletes the snapshots the server no longer keeps and
// returns their IDs: those of a session that ended or is now on another
// machine, those older than api.SnapshotTTL, and then the oldest until the
// rest fit in maxTotal bytes.
func (s *Store) SweepSnapshots(ctx context.Context, maxTotal int64) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT sn.id, sn.size, sn.taken_at,
		s.ended_at IS NOT NULL OR s.machine_id != sn.source_machine_id OR sn.taken_at < ?
		FROM snapshots sn JOIN sessions s ON s.id = sn.session_id ORDER BY sn.taken_at DESC, sn.id`,
		formatTS(s.Now().Add(-api.SnapshotTTL)))
	if err != nil {
		return nil, err
	}
	var gone []string
	var total int64
	for rows.Next() {
		var id, taken string
		var size int64
		var drop bool
		if err := rows.Scan(&id, &size, &taken, &drop); err != nil {
			rows.Close()
			return nil, err
		}
		// Newest first, so the ones past the cap are the oldest.
		if !drop && total+size > maxTotal {
			drop = true
		}
		if drop {
			gone = append(gone, id)
			continue
		}
		total += size
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range gone {
		if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	return gone, tx.Commit()
}

// SnapshotIDs returns the IDs of every snapshot the server keeps.
func (s *Store) SnapshotIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM snapshots`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SetSnapshotLinker sets the function CreateMove calls, inside its
// transaction, to put snapshot sealID's file in place as move moveID's
// bundle. Without one, a move from a snapshot is refused.
func (s *Store) SetSnapshotLinker(f func(moveID, sealID string) error) { s.linkSnapshot = f }

// snapshotFor returns the snapshot of session sessionID from machine src
// sealed for machine dst: its ID, when it was taken, and its size. ok is
// false when there is none.
func snapshotFor(ctx context.Context, tx *sql.Tx, sessionID string, src, dst int64) (id string, taken time.Time, size int64, ok bool, err error) {
	var ts string
	err = tx.QueryRowContext(ctx, `SELECT id, taken_at, size FROM snapshots
		WHERE session_id = ? AND source_machine_id = ? AND recipient_machine_id = ?`, sessionID, src, dst).Scan(&id, &ts, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, 0, false, nil
	}
	if err != nil {
		return "", time.Time{}, 0, false, err
	}
	taken, err = parseTS(ts)
	return id, taken, size, err == nil, err
}
