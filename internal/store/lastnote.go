package store

import (
	"context"
	"fmt"

	"github.com/abdallah/session-hub/internal/api"
)

// lastNotesSelect reads each session's latest note: the newest task event
// (by ts, then id) with a client ID whose actor is session:<id>, joined to
// its task. %s is an extra condition on task_events.
//
// The group key is ts then the zero-padded id: formatTS is fixed width, so
// the key sorts like (ts, id), and max() over it is about twice as fast as
// a ROW_NUMBER() window. task_events has no index on actor: this scans the
// table, about 10 ms for 10,000 notes.
const lastNotesSelect = `SELECT substr(e.actor, 9), e.task_id, t.title, t.ref, t.state, e.note, e.ts
FROM (SELECT max(ts || printf('%%020d', id)) AS k FROM task_events
	WHERE client_id IS NOT NULL AND substr(actor, 1, 8) = 'session:'%s
	GROUP BY actor) m
JOIN task_events e ON e.id = CAST(substr(m.k, -20) AS INTEGER)
JOIN tasks t ON t.id = e.task_id`

// lastNotes returns the latest note of every session that wrote one, by
// session ID, or of session id alone when id is not "".
func (s *Store) lastNotes(ctx context.Context, id string) (map[string]*api.SessionNote, error) {
	q, args := fmt.Sprintf(lastNotesSelect, ""), []any{}
	if id != "" {
		q, args = fmt.Sprintf(lastNotesSelect, " AND actor = ?"), []any{"session:" + id}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*api.SessionNote{}
	for rows.Next() {
		var sid, ts string
		n := &api.SessionNote{}
		if err := rows.Scan(&sid, &n.TaskID, &n.TaskTitle, &n.TaskRef, &n.TaskState, &n.Text, &ts); err != nil {
			return nil, err
		}
		if n.At, err = parseTS(ts); err != nil {
			return nil, err
		}
		out[sid] = n
	}
	return out, rows.Err()
}
