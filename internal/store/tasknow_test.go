package store

import (
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// find returns task id from any column of v, or nil.
func find(v api.TaskDay, id string) *api.Task {
	for _, list := range [][]api.Task{v.Todo, v.InProgress, v.Done} {
		for i := range list {
			if list[i].ID == id {
				return &list[i]
			}
		}
	}
	return nil
}

func TestTaskDayAgesOut(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.at(1, 9)
	tk := e.workingTask("old", "s1")
	todo := e.task(api.TaskIn{Title: "kept"}, tYou)
	e.at(5, 9)
	e.wantCol(3, tk.ID, "in_progress") // within 3 days of Oct 3's end
	e.wantCol(4, tk.ID, "")            // no activity since Oct 1
	e.wantCol(5, tk.ID, "")
	e.wantCol(5, todo.ID, "todo")
	// Working time brings it back.
	e.setStateFrom("s1", "working", "hooks", time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC))
	e.setStateFrom("s1", "idle", "hooks", time.Date(2026, 10, 5, 8, 30, 0, 0, time.UTC))
	e.wantCol(5, tk.ID, "in_progress")
	got := find(e.day(5), tk.ID)
	if got.LastActiveAt == nil || !got.LastActiveAt.Equal(time.Date(2026, 10, 5, 8, 30, 0, 0, time.UTC)) {
		t.Errorf("last_active_at %v, want Oct 5 08:30", got.LastActiveAt)
	}
}

func TestTaskDayAgesOutSpan(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.at(1, 9)
	tk := e.workingTask("old", "s1")
	e.at(5, 9)
	e.span("s1", tk.ID, time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)) // a note routed to it
	e.wantCol(5, tk.ID, "in_progress")
}

func TestTaskDayNow(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.at(5, 9)
	tk := e.workingTask("live", "s1")
	e.setStateFrom("s1", "working", "hooks", time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC))
	e.clock.Advance(20 * time.Minute)
	if got := find(e.day(5), tk.ID); !got.Now {
		t.Error("working session: now false, want true")
	}
	e.setStateFrom("s1", "idle", "hooks", e.clock.Now())
	e.clock.Advance(10 * time.Minute)
	if got := find(e.day(5), tk.ID); !got.Now {
		t.Error("idle 10 min: now false, want true")
	}
	e.clock.Advance(10 * time.Minute)
	if got := find(e.day(5), tk.ID); got.Now {
		t.Error("idle 20 min: now true, want false")
	}
	if got := find(e.day(4), tk.ID); got != nil && got.Now {
		t.Error("past day: now true")
	}
}

func TestTaskDayWaiting(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.at(5, 9)
	tk := e.workingTask("review", "s1")
	e.setStateFrom("s1", "working", "hooks", time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC))
	e.clock.Advance(10 * time.Minute)
	e.sendReport("s1", "Eran: review !133")
	e.setStateFrom("s1", "idle", "hooks", e.clock.Now().Add(5*time.Second))
	e.clock.Advance(30 * time.Minute)
	got := find(e.day(5), tk.ID)
	if len(got.WaitingOn) != 1 || got.WaitingOn[0] != "Eran: review !133" {
		t.Errorf("waiting_on %v, want the report's", got.WaitingOn)
	}
	// Working again after the report makes it stale.
	e.setStateFrom("s1", "working", "hooks", e.clock.Now())
	e.clock.Advance(5 * time.Minute)
	if got := find(e.day(5), tk.ID); len(got.WaitingOn) != 0 {
		t.Errorf("after more work: waiting_on %v, want none", got.WaitingOn)
	}
}

func TestTaskDayWaitingOtherTask(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.at(5, 9)
	a := e.workingTask("A", "s1")
	b := e.workingTask("B", "s1")
	e.span("s1", a.ID, time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC))
	e.span("s1", b.ID, time.Date(2026, 10, 5, 8, 30, 0, 0, time.UTC))
	e.sendReport("s1", "someone")
	v := e.day(5)
	if got := find(v, a.ID); len(got.WaitingOn) != 0 {
		t.Errorf("A: waiting_on %v, want none: the session moved on to B", got.WaitingOn)
	}
	if got := find(v, b.ID); len(got.WaitingOn) != 1 {
		t.Errorf("B: waiting_on %v, want one", got.WaitingOn)
	}
}
