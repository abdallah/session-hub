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

const (
	// carryWindow is how long an in-progress task stays in the day view
	// with no event, span, or working time.
	carryWindow = 3 * 24 * time.Hour
	// nowWindow is how recent working time must be for a task to count as
	// what you work on now.
	nowWindow = 15 * time.Minute
	// waitingSlack is how long a session may keep working after a report
	// before its waiting_on counts as stale.
	waitingSlack = 2 * time.Minute
)

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

	end := minTime(next, now)
	cutoff := end.Add(-carryWindow)
	fill := func(dst *[]api.Task, ps []placed, ages bool) error {
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
			if err := s.fillDay(ctx, &t, start, next, cutoff, now); err != nil {
				return err
			}
			if ages && p.last.Before(cutoff) && t.LastActiveAt == nil {
				spanned, err := s.spannedSince(ctx, t.ID, cutoff, end)
				if err != nil {
					return err
				}
				if !spanned {
					continue
				}
			}
			t.Now = next.After(now) && t.LastActiveAt != nil && !t.LastActiveAt.Before(now.Add(-nowWindow))
			*dst = append(*dst, t)
		}
		return nil
	}
	for _, c := range []struct {
		name string
		dst  *[]api.Task
	}{{"todo", &out.Todo}, {"in_progress", &out.InProgress}, {"done", &out.Done}} {
		if err := fill(c.dst, cols[c.name], c.name == "in_progress"); err != nil {
			return out, err
		}
	}
	return out, nil
}

// fillDay fills t's day fields for the day [start, next): each session's
// report evidence, working seconds and waiting_on, and the task's totals.
// Working time before cutoff does not set LastActiveAt.
func (s *Store) fillDay(ctx context.Context, t *api.Task, start, next, cutoff, now time.Time) error {
	end := minTime(next, now)
	for i := range t.Sessions {
		ss := &t.Sessions[i]
		var err error
		if ss.Done, err = s.dayReportItems(ctx, ss.ID, start, next, now); err != nil {
			return err
		}
		ps, err := s.taskPeriods(ctx, ss.ID, t.ID, minTime(start, cutoff), next, now)
		if err != nil {
			return err
		}
		if ss.LastNote, ss.LastNoteAt, err = s.sessionTaskNote(ctx, ss.ID, t.ID, end); err != nil {
			return err
		}
		ss.ActiveSeconds = seconds(clip(ps, start, next))
		t.ActiveSeconds += ss.ActiveSeconds
		if n := len(ps); n > 0 && !ps[n-1].to.Before(cutoff) && (t.LastActiveAt == nil || ps[n-1].to.After(*t.LastActiveAt)) {
			last := ps[n-1].to
			t.LastActiveAt = &last
		}
		if t.State == api.TaskDone {
			continue
		}
		if ss.WaitingOn, err = s.sessionWaiting(ctx, ss.ID, t.ID, cutoff, end, now); err != nil {
			return err
		}
		t.WaitingOn = append(t.WaitingOn, ss.WaitingOn...)
	}
	return nil
}

// sessionTaskNote is session id's latest note routed to task at or before
// end, or "" and nil.
func (s *Store) sessionTaskNote(ctx context.Context, id, task string, end time.Time) (string, *time.Time, error) {
	var note, ts string
	err := s.db.QueryRowContext(ctx, `SELECT note, ts FROM task_events
		WHERE task_id = ? AND actor = ? AND client_id IS NOT NULL AND note != '' AND ts <= ?
		ORDER BY ts DESC, id DESC LIMIT 1`, task, "session:"+id, formatTS(end)).Scan(&note, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	at, err := parseTS(ts)
	if err != nil {
		return "", nil, err
	}
	return note, &at, nil
}

// sessionWaiting is the waiting_on of session id's latest report in
// [cutoff, end], if the session was on task then and has not worked since.
func (s *Store) sessionWaiting(ctx context.Context, id, task string, cutoff, end, now time.Time) ([]string, error) {
	var ts, raw string
	err := s.db.QueryRowContext(ctx, `SELECT ts, waiting_on_json FROM reports WHERE session_id = ? AND ts >= ? AND ts <= ? ORDER BY ts DESC, id DESC LIMIT 1`,
		id, formatTS(cutoff), formatTS(end)).Scan(&ts, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var waiting []string
	if err := json.Unmarshal([]byte(raw), &waiting); err != nil {
		return nil, fmt.Errorf("stored report list: %w", err)
	}
	if len(waiting) == 0 {
		return nil, nil
	}
	at, err := parseTS(ts)
	if err != nil {
		return nil, err
	}
	spans, err := s.sessionSpans(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(spans) > 0 {
		cur := spans[0].task // time before the first span is the first span's
		for _, sp := range spans {
			if !sp.at.After(at) {
				cur = sp.task
			}
		}
		if cur != task {
			return nil, nil
		}
	}
	ps, err := workingPeriods(ctx, s.db, id, at, end.Add(time.Nanosecond), now)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if p.to.After(at.Add(waitingSlack)) {
			return nil, nil
		}
	}
	return waiting, nil
}

// spannedSince reports whether a session started working for task in
// [from, to].
func (s *Store) spannedSince(ctx context.Context, task string, from, to time.Time) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM task_spans WHERE task_id = ? AND started_at >= ? AND started_at <= ? LIMIT 1`,
		task, formatTS(from), formatTS(to)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
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
