package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// at sets the store clock to 2026-10-<day> at hour h UTC.
func (e *controlEnv) at(day, h int) {
	e.clock.mu.Lock()
	e.clock.t = time.Date(2026, 10, day, h, 0, 0, 0, time.UTC)
	e.clock.mu.Unlock()
}

func (e *controlEnv) move(id, to string) {
	e.t.Helper()
	a := tYou
	if to == api.TaskDoneProposed {
		a = tAgentFor(e, "sdp")
	}
	if _, err := e.s.SetTaskState(context.Background(), id, to, "", "", a); err != nil {
		e.t.Fatal(err)
	}
}

// day reads the day view for 2026-10-<d> in UTC.
func (e *controlEnv) day(d int) api.TaskDay {
	e.t.Helper()
	return e.dayIn(d, time.UTC)
}

func (e *controlEnv) dayIn(d int, loc *time.Location) api.TaskDay {
	e.t.Helper()
	v, err := e.s.TaskDay(context.Background(), time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02"), loc)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

// col reports which column of v holds id: "todo", "in_progress", "done", or "".
func col(v api.TaskDay, id string) string {
	for name, ts := range map[string][]api.Task{"todo": v.Todo, "in_progress": v.InProgress, "done": v.Done} {
		for _, t := range ts {
			if t.ID == id {
				return name
			}
		}
	}
	return ""
}

func (e *controlEnv) wantCol(d int, id, want string) {
	e.t.Helper()
	if got := col(e.day(d), id); got != want {
		e.t.Errorf("Oct %d: column %q, want %q", d, got, want)
	}
}

func TestTaskDayCarryOver(t *testing.T) {
	e := newControlEnv(t)
	e.at(1, 9)
	tk := e.task(api.TaskIn{Title: "carry"}, tYou)
	e.at(3, 10)
	e.move(tk.ID, api.TaskDropped)
	e.at(4, 8)
	e.wantCol(1, tk.ID, "todo")
	e.wantCol(2, tk.ID, "todo")
	e.wantCol(3, tk.ID, "")
	e.wantCol(4, tk.ID, "")
}

func TestTaskDayCarryOverNoEvents(t *testing.T) {
	e := newControlEnv(t)
	e.at(1, 9)
	tk := e.task(api.TaskIn{Title: "carry"}, tYou)
	e.at(5, 9)
	e.wantCol(1, tk.ID, "todo")
	e.wantCol(3, tk.ID, "todo")
	e.wantCol(5, tk.ID, "todo")
}

func TestTaskDayDone(t *testing.T) {
	e := newControlEnv(t)
	e.at(1, 9)
	tk := e.task(api.TaskIn{Title: "d"}, tYou)
	e.move(tk.ID, api.TaskInProgress)
	e.at(2, 9)
	e.move(tk.ID, api.TaskDone)
	e.at(4, 9)
	e.wantCol(1, tk.ID, "in_progress")
	e.wantCol(2, tk.ID, "done")
	e.wantCol(3, tk.ID, "")
}

func TestTaskDayReopened(t *testing.T) {
	e := newControlEnv(t)
	e.at(1, 9)
	tk := e.task(api.TaskIn{Title: "r"}, tYou)
	e.move(tk.ID, api.TaskInProgress)
	e.at(2, 9)
	e.move(tk.ID, api.TaskDone)
	e.at(3, 9)
	e.move(tk.ID, api.TaskInProgress)
	e.at(4, 9)
	e.wantCol(2, tk.ID, "done")
	e.wantCol(3, tk.ID, "in_progress")
	e.wantCol(4, tk.ID, "in_progress")
}

func TestTaskDayDoneProposed(t *testing.T) {
	e := newControlEnv(t)
	e.at(1, 9)
	tk := e.task(api.TaskIn{Title: "dp"}, tYou)
	e.move(tk.ID, api.TaskInProgress)
	e.move(tk.ID, api.TaskDoneProposed)
	e.at(3, 9)
	e.wantCol(1, tk.ID, "in_progress")
	e.wantCol(2, tk.ID, "in_progress")
}

func TestTaskDayDroppedMidDay(t *testing.T) {
	e := newControlEnv(t)
	e.at(1, 9)
	tk := e.task(api.TaskIn{Title: "x"}, tYou)
	e.move(tk.ID, api.TaskInProgress)
	e.at(2, 9)
	e.move(tk.ID, api.TaskDropped)
	e.at(3, 9)
	e.wantCol(1, tk.ID, "in_progress")
	e.wantCol(2, tk.ID, "")
}

func TestTaskDayHidesProposedAndDropped(t *testing.T) {
	e := newControlEnv(t)
	e.at(1, 9)
	p := e.task(api.TaskIn{Title: "p"}, tAgentFor(e, "sp"))
	d := e.task(api.TaskIn{Title: "d"}, tYou)
	e.move(d.ID, api.TaskDropped)
	e.at(2, 9)
	v := e.day(1)
	if col(v, p.ID) != "" || col(v, d.ID) != "" {
		t.Errorf("proposed or dropped task in day view: %+v", v)
	}
	if v.Todo == nil || v.InProgress == nil || v.Done == nil {
		t.Error("columns must be non-nil slices")
	}
}

func TestTaskDayZone(t *testing.T) {
	e := newControlEnv(t)
	e.at(1, 9)
	tk := e.task(api.TaskIn{Title: "z"}, tYou)
	e.move(tk.ID, api.TaskInProgress)
	ev := time.Date(2026, 10, 2, 21, 30, 0, 0, time.UTC)
	if _, err := e.s.db.Exec(`INSERT INTO task_events (task_id, ts, from_state, to_state, actor, note) VALUES (?, ?, 'in_progress', 'done', 'web:x', '')`,
		tk.ID, formatTS(ev)); err != nil {
		t.Fatal(err)
	}
	e.at(5, 9)
	amman, err := time.LoadLocation("Asia/Amman")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	if got := col(e.dayIn(2, time.UTC), tk.ID); got != "done" {
		t.Errorf("UTC Oct 2: %q, want done", got)
	}
	if got := col(e.dayIn(3, time.UTC), tk.ID); got != "" {
		t.Errorf("UTC Oct 3: %q, want none", got)
	}
	if got := col(e.dayIn(3, amman), tk.ID); got != "done" {
		t.Errorf("Amman Oct 3: %q, want done", got)
	}
	if got := col(e.dayIn(2, amman), tk.ID); got != "in_progress" {
		t.Errorf("Amman Oct 2: %q, want in_progress", got)
	}
}

func TestTaskDayToday(t *testing.T) {
	e := newControlEnv(t)
	e.at(3, 9)
	tk := e.task(api.TaskIn{Title: "t"}, tYou)
	e.clock.Advance(time.Hour)
	e.wantCol(3, tk.ID, "todo")
	// An event after the store clock is not visible.
	if _, err := e.s.db.Exec(`INSERT INTO task_events (task_id, ts, from_state, to_state, actor, note) VALUES (?, ?, 'todo', 'dropped', 'web:x', '')`,
		tk.ID, formatTS(e.clock.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	e.wantCol(3, tk.ID, "todo")
	e.clock.Advance(2 * time.Hour)
	e.wantCol(3, tk.ID, "")
	v := e.day(4)
	if len(v.Todo)+len(v.InProgress)+len(v.Done) != 0 {
		t.Errorf("future day not empty: %+v", v)
	}
}

func TestTaskDayEvidence(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.at(1, 9)
	e.sendReport("s1") // Oct 1, outside the view
	e.at(2, 9)
	tk := e.task(api.TaskIn{Title: "ev", SessionID: "s1"}, tYou)
	e.move(tk.ID, api.TaskInProgress)
	e.sendReport("s1")
	e.at(3, 9)
	e.sendReport("s1")
	e.at(4, 9)
	v := e.day(2)
	if len(v.InProgress) != 1 || len(v.InProgress[0].Sessions) != 1 {
		t.Fatalf("day view: %+v", v)
	}
	s := v.InProgress[0].Sessions[0]
	if s.ID != "s1" || s.Machine != "tower" || len(s.Done) != 1 || s.Done[0] != "a step" {
		t.Errorf("session evidence: %+v", s)
	}
}

func TestTaskDayBadDate(t *testing.T) {
	e := newControlEnv(t)
	for _, d := range []string{"2026-13-01", "", "10/02/2026"} {
		if _, err := e.s.TaskDay(context.Background(), d, time.UTC); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v, want ErrInvalid", d, err)
		}
	}
}

func TestTaskReview(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.at(1, 9)
	p1 := e.task(api.TaskIn{Title: "p1"}, tAgentFor(e, "sp"))
	e.at(1, 10)
	p2 := e.task(api.TaskIn{Title: "p2"}, tAgentFor(e, "sp"))
	e.at(1, 11)
	d1 := e.task(api.TaskIn{Title: "d1"}, tYou)
	e.move(d1.ID, api.TaskInProgress)
	e.move(d1.ID, api.TaskDoneProposed)
	e.at(1, 12)
	d2 := e.task(api.TaskIn{Title: "d2"}, tYou)
	e.move(d2.ID, api.TaskInProgress)
	e.move(d2.ID, api.TaskDoneProposed)

	e.at(10, 12)
	mk := func(id, title string, ago time.Duration) {
		e.session(e.tower, id, "")
		if title != "" {
			if err := e.s.SetTitle(ctx, e.tower.ID, id, title); err != nil {
				t.Fatal(err)
			}
		}
		now := e.clock.Now()
		e.clock.Advance(-ago)
		e.sendPrompt(id)
		e.clock.mu.Lock()
		e.clock.t = now
		e.clock.mu.Unlock()
	}
	mk("recent", "Recent", 3*24*time.Hour)
	mk("recent2", "Recent two", 2*24*time.Hour)
	mk("old", "Old", 8*24*time.Hour)
	mk("untitled", "", 24*time.Hour)
	mk("linked", "Linked", 24*time.Hour)
	mk("ignored", "Ignored", 24*time.Hour)
	if _, err := e.s.LinkTask(ctx, d1.ID, "linked", "", tYou); err != nil {
		t.Fatal(err)
	}
	if err := e.s.IgnoreUntasked(ctx, "ignored"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.IgnoreUntasked(ctx, "ignored"); err != nil {
		t.Fatalf("second ignore: %v", err)
	}
	if err := e.s.IgnoreUntasked(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v, want ErrNotFound", err)
	}

	r, err := e.s.TaskReview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(ts []api.Task) []string {
		out := []string{}
		for _, x := range ts {
			out = append(out, x.ID)
		}
		return out
	}
	if g := ids(r.Proposed); len(g) != 2 || g[0] != p1.ID || g[1] != p2.ID {
		t.Errorf("proposed %v, want [%s %s]", g, p1.ID, p2.ID)
	}
	if g := ids(r.DoneProposed); len(g) != 2 || g[0] != d1.ID || g[1] != d2.ID {
		t.Errorf("done proposed %v, want [%s %s]", g, d1.ID, d2.ID)
	}
	if len(r.Untasked) != 2 || r.Untasked[0].ID != "recent2" || r.Untasked[1].ID != "recent" {
		t.Errorf("untasked %+v, want recent2 then recent", r.Untasked)
	}
}

func tAgentFor(e *controlEnv, id string) Actor {
	if _, err := e.s.GetSession(context.Background(), id); err != nil {
		e.session(e.tower, id, "")
	}
	return tAgent(id, e.tower)
}
