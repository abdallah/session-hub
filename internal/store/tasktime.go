package store

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// maxWorkingPeriod cuts a working period that no later event closes, so a
// session stuck as working does not count all night.
const maxWorkingPeriod = 2 * time.Hour

// period is a span of working time, [from, to).
type period struct{ from, to time.Time }

// workingPeriods returns session id's working time in [from, to), as
// sorted, disjoint periods. Each event source (hooks, plugin) opens a
// period with a working state_changed event and closes it with its next
// event of another state, after maxWorkingPeriod, or at now; the sources'
// periods are merged.
func workingPeriods(ctx context.Context, q querier, id string, from, to, now time.Time) ([]period, error) {
	rows, err := q.QueryContext(ctx, `SELECT ts, source, payload_json FROM events
		WHERE session_id = ? AND kind = ? AND ts >= ? AND ts < ? ORDER BY ts, id`,
		id, api.KindStateChanged, formatTS(from.Add(-maxWorkingPeriod)), formatTS(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	open := map[string]time.Time{}
	var all []period
	closeAt := func(src string, end time.Time) {
		start, ok := open[src]
		if !ok {
			return
		}
		delete(open, src)
		end = minTime(end, start.Add(maxWorkingPeriod), now)
		if end.After(start) {
			all = append(all, period{start, end})
		}
	}
	for rows.Next() {
		var ts, src string
		var raw *string
		if err := rows.Scan(&ts, &src, &raw); err != nil {
			return nil, err
		}
		t, err := parseTS(ts)
		if err != nil {
			return nil, err
		}
		var p struct {
			AgentState string `json:"agent_state"`
		}
		if raw != nil {
			_ = json.Unmarshal([]byte(*raw), &p) // a bad payload counts as not working
		}
		if p.AgentState == "working" {
			if _, ok := open[src]; !ok {
				open[src] = t
			}
			continue
		}
		closeAt(src, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for src := range open {
		closeAt(src, to)
	}
	return clip(merge(all), from, to), nil
}

func minTime(ts ...time.Time) time.Time {
	m := ts[0]
	for _, t := range ts[1:] {
		if t.Before(m) {
			m = t
		}
	}
	return m
}

// merge sorts periods and joins those that overlap or touch.
func merge(ps []period) []period {
	sort.Slice(ps, func(i, j int) bool { return ps[i].from.Before(ps[j].from) })
	var out []period
	for _, p := range ps {
		if n := len(out); n > 0 && !p.from.After(out[n-1].to) {
			if p.to.After(out[n-1].to) {
				out[n-1].to = p.to
			}
			continue
		}
		out = append(out, p)
	}
	return out
}

// clip cuts periods to [from, to) and drops the empty ones.
func clip(ps []period, from, to time.Time) []period {
	var out []period
	for _, p := range ps {
		if p.from.Before(from) {
			p.from = from
		}
		if p.to.After(to) {
			p.to = to
		}
		if p.to.After(p.from) {
			out = append(out, p)
		}
	}
	return out
}

// span is a task_spans row: from at on, the session works for task.
type span struct {
	at   time.Time
	task string
}

func (s *Store) sessionSpans(ctx context.Context, id string) ([]span, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT started_at, task_id FROM task_spans WHERE session_id = ? ORDER BY started_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []span
	for rows.Next() {
		var ts, task string
		if err := rows.Scan(&ts, &task); err != nil {
			return nil, err
		}
		t, err := parseTS(ts)
		if err != nil {
			return nil, err
		}
		out = append(out, span{t, task})
	}
	return out, rows.Err()
}

// taskSeconds is session id's working seconds for task in [from, to). With
// no spans, all its working time counts. Otherwise each moment belongs to
// the latest span at or before it, and the time before the first span to
// the first span's task.
func (s *Store) taskSeconds(ctx context.Context, id, task string, from, to, now time.Time) (int64, error) {
	ps, err := workingPeriods(ctx, s.db, id, from, to, now)
	if err != nil {
		return 0, err
	}
	spans, err := s.sessionSpans(ctx, id)
	if err != nil {
		return 0, err
	}
	var total time.Duration
	for _, p := range ps {
		if len(spans) == 0 {
			total += p.to.Sub(p.from)
			continue
		}
		for i, sp := range spans {
			if sp.task != task {
				continue
			}
			lo, hi := sp.at, p.to
			if i == 0 {
				lo = p.from
			}
			if i+1 < len(spans) {
				hi = minTime(hi, spans[i+1].at)
			}
			if lo.Before(p.from) {
				lo = p.from
			}
			if hi.After(lo) {
				total += hi.Sub(lo)
			}
		}
	}
	return int64(total / time.Second), nil
}
