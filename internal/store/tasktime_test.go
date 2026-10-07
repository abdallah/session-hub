package store

import (
	"context"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// at is a time on the controlEnv's day, 2026-09-30, in UTC.
func at(h, m int) time.Time { return time.Date(2026, 9, 30, h, m, 0, 0, time.UTC) }

func (e *controlEnv) periods(id string, from, to time.Time) []period {
	e.t.Helper()
	ps, err := workingPeriods(context.Background(), e.s.db, id, from, to, e.clock.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	return ps
}

func (e *controlEnv) setClock(t time.Time) {
	e.clock.Advance(t.Sub(e.clock.Now()))
}

func TestWorkingPeriodsUnion(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.setStateFrom("s1", "working", "hooks", at(10, 0))
	e.setStateFrom("s1", "working", "plugin", at(10, 20))
	e.setStateFrom("s1", "idle", "hooks", at(10, 30))
	e.setStateFrom("s1", "idle", "plugin", at(10, 50))
	got := e.periods("s1", at(0, 0), at(23, 59))
	if len(got) != 1 || !got[0].from.Equal(at(10, 0)) || !got[0].to.Equal(at(10, 50)) {
		t.Errorf("periods %v, want one 10:00-10:50", got)
	}
}

func TestWorkingPeriodsCap(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.setStateFrom("s1", "working", "hooks", at(10, 0))
	e.setClock(at(15, 0))
	got := e.periods("s1", at(0, 0), at(23, 59))
	if len(got) != 1 || !got[0].to.Equal(at(12, 0)) {
		t.Errorf("periods %v, want 10:00-12:00", got)
	}
}

func TestWorkingPeriodsStillWorking(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.setClock(at(15, 0))
	e.setStateFrom("s1", "working", "hooks", at(14, 30))
	got := e.periods("s1", at(0, 0), at(23, 59))
	if len(got) != 1 || !got[0].from.Equal(at(14, 30)) || !got[0].to.Equal(at(15, 0)) {
		t.Errorf("periods %v, want 14:30-15:00", got)
	}
}

// workingTask makes an in-progress task linked to session sid.
func (e *controlEnv) workingTask(title, sid string) api.Task {
	e.t.Helper()
	return e.task(api.TaskIn{Title: title, State: api.TaskInProgress, SessionID: sid}, tYou)
}

func (e *controlEnv) span(sid, taskID string, ts time.Time) {
	e.t.Helper()
	if _, err := e.s.db.Exec(`INSERT INTO task_spans (session_id, task_id, started_at) VALUES (?, ?, ?)`, sid, taskID, formatTS(ts)); err != nil {
		e.t.Fatal(err)
	}
}

// daySeconds is the active seconds TaskDay gives task id on date in loc.
func (e *controlEnv) daySeconds(date string, loc *time.Location, id string) int64 {
	e.t.Helper()
	d, err := e.s.TaskDay(context.Background(), date, loc)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, list := range [][]api.Task{d.Todo, d.InProgress, d.Done} {
		for _, tk := range list {
			if tk.ID == id {
				var sum int64
				for _, ss := range tk.Sessions {
					sum += ss.ActiveSeconds
				}
				if sum != tk.ActiveSeconds {
					e.t.Errorf("task %s: sessions sum to %d, task says %d", id, sum, tk.ActiveSeconds)
				}
				return tk.ActiveSeconds
			}
		}
	}
	e.t.Fatalf("task %s not in the %s day view", id, date)
	return 0
}

func TestTaskDaySplitBySpans(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	a := e.workingTask("A", "s1")
	b := e.workingTask("B", "s1")
	e.setStateFrom("s1", "working", "hooks", at(9, 0))
	e.setStateFrom("s1", "idle", "hooks", at(11, 0))
	e.span("s1", a.ID, at(9, 30))
	e.span("s1", b.ID, at(10, 0))
	if got := e.daySeconds("2026-09-30", time.UTC, a.ID); got != 3600 {
		t.Errorf("A: %d s, want 3600", got)
	}
	if got := e.daySeconds("2026-09-30", time.UTC, b.ID); got != 3600 {
		t.Errorf("B: %d s, want 3600", got)
	}
}

func TestTaskDayNoSpans(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	a := e.workingTask("A", "s1")
	e.setStateFrom("s1", "working", "hooks", at(9, 0))
	e.setStateFrom("s1", "idle", "hooks", at(10, 0))
	if got := e.daySeconds("2026-09-30", time.UTC, a.ID); got != 3600 {
		t.Errorf("A: %d s, want 3600", got)
	}
}

func TestTaskDayEdges(t *testing.T) {
	e := newControlEnv(t)
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	e.session(e.tower, "s1", "")
	// 23:30 to 00:30 Berlin is 21:30 to 22:30 UTC in summer time.
	e.setStateFrom("s1", "working", "hooks", time.Date(2026, 9, 29, 21, 30, 0, 0, time.UTC))
	e.setStateFrom("s1", "idle", "hooks", time.Date(2026, 9, 29, 22, 30, 0, 0, time.UTC))
	e.setClock(time.Date(2026, 9, 29, 21, 0, 0, 0, time.UTC))
	a := e.workingTask("A", "s1")
	e.setClock(at(12, 0))
	if got := e.daySeconds("2026-09-29", berlin, a.ID); got != 1800 {
		t.Errorf("first day: %d s, want 1800", got)
	}
	if got := e.daySeconds("2026-09-30", berlin, a.ID); got != 1800 {
		t.Errorf("second day: %d s, want 1800", got)
	}
}
