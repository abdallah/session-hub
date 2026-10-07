package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// untaskedWindow is how far back a prompt makes a session a candidate for
// the "Sessions with no task" list.
const untaskedWindow = 7 * 24 * time.Hour

type taskEv struct {
	ts time.Time
	to string
}

// TaskDay rebuilds date's three columns in loc by replaying task events. The
// day runs from 00:00 to 24:00 in loc; today's view ends at the store clock,
// and a later day is empty. loc nil means UTC.
func (s *Store) TaskDay(ctx context.Context, date string, loc *time.Location) (api.TaskDay, error) {
	if loc == nil {
		loc = time.UTC
	}
	out := api.TaskDay{Date: date, TZ: loc.String(), Todo: []api.Task{}, InProgress: []api.Task{}, Done: []api.Task{}}
	d, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return out, invalidf("date %q: use YYYY-MM-DD", date)
	}
	y, m, dd := d.Date()
	start := time.Date(y, m, dd, 0, 0, 0, 0, loc)
	next := time.Date(y, m, dd+1, 0, 0, 0, 0, loc)
	now := s.Now()
	if start.After(now) {
		return out, nil
	}

	rows, err := s.db.QueryContext(ctx, `SELECT task_id, ts, to_state FROM task_events WHERE ts < ? ORDER BY task_id, id`, formatTS(next))
	if err != nil {
		return out, err
	}
	var order []string
	byTask := map[string][]taskEv{}
	for rows.Next() {
		var id, ts, to string
		if err := rows.Scan(&id, &ts, &to); err != nil {
			rows.Close()
			return out, err
		}
		t, err := parseTS(ts)
		if err != nil {
			rows.Close()
			return out, err
		}
		if t.After(now) {
			continue
		}
		if _, ok := byTask[id]; !ok {
			order = append(order, id)
		}
		byTask[id] = append(byTask[id], taskEv{t, to})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	type placed struct {
		id   string
		last time.Time
	}
	cols := map[string][]placed{}
	for _, id := range order {
		evs := byTask[id]
		final := evs[len(evs)-1].to
		var atStart string
		var inDay []taskEv
		for _, e := range evs {
			if e.ts.Before(start) {
				atStart = e.to
			} else {
				inDay = append(inDay, e)
			}
		}
		anyTo := func(states ...string) bool {
			for _, e := range inDay {
				for _, st := range states {
					if e.to == st {
						return true
					}
				}
			}
			return false
		}
		running := func(st string) bool { return st == api.TaskInProgress || st == api.TaskDoneProposed }
		var c string
		switch {
		case final == api.TaskDone && anyTo(api.TaskDone):
			c = "done"
		case final != api.TaskDropped && (running(atStart) || anyTo(api.TaskInProgress, api.TaskDoneProposed)):
			c = "in_progress"
		case final == api.TaskTodo:
			c = "todo"
		default:
			continue
		}
		cols[c] = append(cols[c], placed{id, evs[len(evs)-1].ts})
	}

	fill := func(dst *[]api.Task, ps []placed) error {
		sort.SliceStable(ps, func(i, j int) bool {
			if !ps[i].last.Equal(ps[j].last) {
				return ps[i].last.After(ps[j].last)
			}
			return ps[i].id < ps[j].id
		})
		for _, p := range ps {
			t, err := readTask(ctx, s.db, p.id)
			if err != nil {
				return err
			}
			for i := range t.Sessions {
				if t.Sessions[i].Done, err = s.dayReportItems(ctx, t.Sessions[i].ID, start, next, now); err != nil {
					return err
				}
			}
			*dst = append(*dst, t)
		}
		return nil
	}
	for _, c := range []struct {
		name string
		dst  *[]api.Task
	}{{"todo", &out.Todo}, {"in_progress", &out.InProgress}, {"done", &out.Done}} {
		if err := fill(c.dst, cols[c.name]); err != nil {
			return out, err
		}
	}
	return out, nil
}

// dayReportItems returns the done items of session id's reports made in
// [start, next) and not after now.
func (s *Store) dayReportItems(ctx context.Context, id string, start, next, now time.Time) ([]string, error) {
	hi := next
	if now.Before(hi) {
		hi = now.Add(time.Nanosecond)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT done_json FROM reports WHERE session_id = ? AND ts >= ? AND ts < ? ORDER BY id`,
		id, formatTS(start), formatTS(hi))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var done []string
		if err := json.Unmarshal([]byte(raw), &done); err != nil {
			return nil, fmt.Errorf("stored report list: %w", err)
		}
		items = append(items, done...)
	}
	return items, rows.Err()
}

// TaskReview returns what waits for you: proposed tasks and done proposals,
// oldest first, and titled sessions prompted in the last 7 days that no task
// links and you have not ignored, newest prompt first.
func (s *Store) TaskReview(ctx context.Context) (api.TaskReview, error) {
	out := api.TaskReview{Proposed: []api.Task{}, DoneProposed: []api.Task{}, Untasked: []api.Session{}}
	for _, g := range []struct {
		state string
		dst   *[]api.Task
	}{{api.TaskProposed, &out.Proposed}, {api.TaskDoneProposed, &out.DoneProposed}} {
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM tasks WHERE state = ? ORDER BY created_at, id`, g.state)
		if err != nil {
			return out, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return out, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return out, err
		}
		for _, id := range ids {
			t, err := readTask(ctx, s.db, id)
			if err != nil {
				return out, err
			}
			*g.dst = append(*g.dst, t)
		}
	}

	skip := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT session_id FROM task_sessions UNION SELECT session_id FROM task_session_ignores`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		skip[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	list, err := s.ListSessions(ctx, ListFilter{})
	if err != nil {
		return out, err
	}
	cutoff := s.Now().Add(-untaskedWindow)
	for _, x := range list {
		if x.Title == "" || skip[x.ID] || x.LastPromptAt == nil || x.LastPromptAt.Before(cutoff) {
			continue
		}
		out.Untasked = append(out.Untasked, x)
	}
	sort.SliceStable(out.Untasked, func(i, j int) bool {
		a, b := out.Untasked[i], out.Untasked[j]
		if !a.LastPromptAt.Equal(*b.LastPromptAt) {
			return a.LastPromptAt.After(*b.LastPromptAt)
		}
		return a.ID < b.ID
	})
	return out, nil
}

// IgnoreUntasked hides sessions from the review queue's untasked list, all
// or none: an unknown session is ErrNotFound and nothing is stored.
// Ignoring a session twice is a no-op.
func (s *Store) IgnoreUntasked(ctx context.Context, ids ...string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := formatTS(s.Now())
	for _, id := range ids {
		var one int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: session %s", ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_session_ignores (session_id, ignored_at) VALUES (?, ?)`,
			id, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
