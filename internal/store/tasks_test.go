package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

var (
	tYou   = Actor{Name: "web:abdallah"}
	tAgent = func(id string, m Machine) Actor { return Actor{Name: "session:" + id, Agent: true, MachineID: m.ID} }
)

func (e *controlEnv) task(in api.TaskIn, a Actor) api.Task {
	e.t.Helper()
	t, _, err := e.s.CreateTask(context.Background(), in, a)
	if err != nil {
		e.t.Fatal(err)
	}
	return t
}

// rollbackV14 removes what schema v14 added.
func rollbackV14(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec("DROP TABLE task_spans"); err != nil {
		t.Fatalf("drop task_spans: %v", err)
	}
}

func TestMigrateV13ToV14(t *testing.T) {
	s, path := openTemp(t)
	rollbackV14(t, s)
	if _, err := s.db.Exec("PRAGMA user_version = 13"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var v, n int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 14 {
		t.Fatalf("user_version %d %v, want 14", v, err)
	}
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='task_spans'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("task_spans missing")
	}
}

func TestMigrateV12ToV13(t *testing.T) {
	s, path := openTemp(t)
	rollbackV13(t, s)
	if _, err := s.db.Exec("PRAGMA user_version = 12"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var v int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version %d %v, want %d", v, err, schemaVersion)
	}
	for _, tb := range []string{"tasks", "task_events", "task_sessions", "task_session_ignores"} {
		var n int
		if err := s2.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tb).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing", tb)
		}
	}
}

func TestCreateTaskAgent(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	got, created, err := e.s.CreateTask(context.Background(), api.TaskIn{Title: "  Fix DNS ", Source: "ticket", Ref: "SYS-1", SessionID: "s1"}, tAgent("s1", e.tower))
	if err != nil || !created {
		t.Fatal(created, err)
	}
	if got.State != api.TaskProposed || got.Title != "Fix DNS" || got.CreatedBy != "session:s1" || !ValidTaskID(got.ID) ||
		len(got.Sessions) != 1 || got.Sessions[0].ID != "s1" || got.Sessions[0].Machine != "tower" || !got.CreatedAt.Equal(e.clock.Now()) {
		t.Errorf("task %+v", got)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_events WHERE task_id = ? AND from_state = '' AND to_state = 'proposed' AND actor = 'session:s1'`, got.ID); n != 1 {
		t.Errorf("creating events: %d", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_sessions WHERE task_id = ? AND session_id = 's1'`, got.ID); n != 1 {
		t.Errorf("task_sessions rows: %d", n)
	}
}

func TestCreateTaskAgentStateIgnored(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	if got := e.task(api.TaskIn{Title: "x", State: "done", SessionID: "s1"}, tAgent("s1", e.tower)); got.State != api.TaskProposed {
		t.Errorf("state %s", got.State)
	}
}

func TestCreateTaskYou(t *testing.T) {
	e := newControlEnv(t)
	if got := e.task(api.TaskIn{Title: "a"}, tYou); got.State != api.TaskTodo || got.Source != "other" {
		t.Errorf("%+v", got)
	}
	if got := e.task(api.TaskIn{Title: "b", State: "in_progress"}, tYou); got.State != api.TaskInProgress {
		t.Errorf("%+v", got)
	}
	if _, _, err := e.s.CreateTask(context.Background(), api.TaskIn{Title: "c", State: "done"}, tYou); !errors.Is(err, ErrInvalid) {
		t.Errorf("done create: %v", err)
	}
}

func TestCreateTaskYouWithSession(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	got := e.task(api.TaskIn{Title: "a", SessionID: "s1"}, tYou)
	if got.State != api.TaskTodo || len(got.Sessions) != 1 || got.Sessions[0].ID != "s1" {
		t.Errorf("%+v", got)
	}
}

func TestCreateTaskSameRef(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.session(e.tower, "s2", "")
	first := e.task(api.TaskIn{Title: "a", Ref: "sys-1"}, tYou)
	got, created, err := e.s.CreateTask(context.Background(), api.TaskIn{Title: "b", Ref: "SYS-1", SessionID: "s2"}, tAgent("s2", e.tower))
	if err != nil || created || got.ID != first.ID || len(got.Sessions) != 1 || got.Sessions[0].ID != "s2" {
		t.Fatalf("%+v %v %v", got, created, err)
	}
	if n := e.count(`SELECT COUNT(*) FROM tasks`); n != 1 {
		t.Errorf("tasks: %d", n)
	}
	_, _, err = e.s.CreateTask(context.Background(), api.TaskIn{Title: "c", Ref: "SYS-1"}, tYou)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), first.ID) {
		t.Errorf("web duplicate: %v", err)
	}
}

func TestCreateTaskIdempotent(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	in := api.TaskIn{ID: "t_AAAAAAAAAAA", Title: "a", SessionID: "s1"}
	a := tAgent("s1", e.tower)
	first, created, err := e.s.CreateTask(context.Background(), in, a)
	if err != nil || !created {
		t.Fatal(created, err)
	}
	second, created, err := e.s.CreateTask(context.Background(), in, a)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("%+v %v %v", second, created, err)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_events`); n != 1 {
		t.Errorf("events: %d", n)
	}
}

func TestCreateTaskValidation(t *testing.T) {
	e := newControlEnv(t)
	for name, in := range map[string]api.TaskIn{
		"empty title": {Title: "  "},
		"long title":  {Title: strings.Repeat("é", 201)},
		"ref_url":     {Title: "a", RefURL: "ftp://x"},
		"source":      {Title: "a", Source: "fax"},
		"id":          {Title: "a", ID: "t_short"},
		"long ref":    {Title: "a", Ref: strings.Repeat("a", 201)},
	} {
		if _, _, err := e.s.CreateTask(context.Background(), in, tYou); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM tasks`); n != 0 {
		t.Errorf("stored %d", n)
	}
}

func TestCreateTaskWrongMachine(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	_, _, err := e.s.CreateTask(context.Background(), api.TaskIn{Title: "a", SessionID: "s1"}, tAgent("s1", e.bluebox))
	if !errors.Is(err, ErrWrongMachine) {
		t.Errorf("%v", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM tasks`); n != 0 {
		t.Errorf("stored %d", n)
	}
}

// force puts a task into state st directly.
func (e *controlEnv) force(id, st string) {
	e.t.Helper()
	if _, err := e.s.db.Exec(`UPDATE tasks SET state = ? WHERE id = ?`, st, id); err != nil {
		e.t.Fatal(err)
	}
}

func TestTaskTransitions(t *testing.T) {
	type key struct{ from, to string }
	// allowed[key] = {you, agent}
	allowed := map[key][2]bool{
		{"proposed", "todo"}:             {true, false},
		{"proposed", "in_progress"}:      {true, false},
		{"proposed", "dropped"}:          {true, false},
		{"todo", "in_progress"}:          {true, false},
		{"in_progress", "todo"}:          {true, false},
		{"todo", "done_proposed"}:        {false, true},
		{"in_progress", "done_proposed"}: {false, true},
		{"done_proposed", "done"}:        {true, false},
		{"done_proposed", "in_progress"}: {true, false},
		{"todo", "done"}:                 {true, false},
		{"in_progress", "done"}:          {true, false},
		{"todo", "dropped"}:              {true, false},
		{"in_progress", "dropped"}:       {true, false},
		{"done_proposed", "dropped"}:     {true, false},
		{"done", "dropped"}:              {true, false},
		{"done", "todo"}:                 {true, false},
		{"done", "in_progress"}:          {true, false},
		{"dropped", "done"}:              {true, false},
		{"dropped", "todo"}:              {true, false},
		{"dropped", "in_progress"}:       {true, false},
	}
	states := []string{"proposed", "todo", "in_progress", "done_proposed", "done", "dropped"}
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	ctx := context.Background()
	for _, from := range states {
		for _, to := range states {
			for i, actor := range []Actor{tYou, tAgent("s1", e.tower)} {
				task := e.task(api.TaskIn{Title: "x"}, tYou)
				e.force(task.ID, from)
				before := e.count(`SELECT COUNT(*) FROM task_events`)
				got, err := e.s.SetTaskState(ctx, task.ID, to, "n", "", actor)
				ok := allowed[key{from, to}][i]
				after := e.count(`SELECT COUNT(*) FROM task_events`)
				if ok {
					if err != nil || got.State != to || after != before+1 {
						t.Errorf("%s -> %s agent=%v: %+v %v events %d->%d", from, to, i == 1, got, err, before, after)
					}
				} else if !errors.Is(err, ErrConflict) || after != before || !strings.Contains(err.Error(), "cannot move a "+from+" task to "+to) {
					t.Errorf("%s -> %s agent=%v: want ErrConflict, got %v (events %d->%d)", from, to, i == 1, err, before, after)
				}
			}
		}
	}
}

func TestSetTaskStateEventAndNote(t *testing.T) {
	e := newControlEnv(t)
	task := e.task(api.TaskIn{Title: "x"}, tYou)
	e.clock.Advance(time.Minute)
	got, err := e.s.SetTaskState(context.Background(), task.ID, "done", " shipped ", "", tYou)
	if err != nil || !got.UpdatedAt.Equal(e.clock.Now()) {
		t.Fatalf("%+v %v", got, err)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_events WHERE task_id = ? AND from_state = 'todo' AND to_state = 'done' AND note = 'shipped' AND actor = 'web:abdallah'`, task.ID); n != 1 {
		t.Errorf("event rows: %d", n)
	}
	if _, err := e.s.SetTaskState(context.Background(), task.ID, "todo", strings.Repeat("n", 501), "", tYou); !errors.Is(err, ErrInvalid) {
		t.Errorf("long note: %v", err)
	}
	if _, err := e.s.SetTaskState(context.Background(), "t_nope", "todo", "", "", tYou); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad id: %v", err)
	}
	if _, err := e.s.SetTaskState(context.Background(), "t_AAAAAAAAAAA", "todo", "", "", tYou); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestProposeDoneOnDone(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	task := e.task(api.TaskIn{Title: "x"}, tYou)
	if _, err := e.s.SetTaskState(context.Background(), task.ID, "done", "", "", tYou); err != nil {
		t.Fatal(err)
	}
	_, err := e.s.SetTaskState(context.Background(), task.ID, "done_proposed", "", "", tAgent("s1", e.tower))
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "already done") {
		t.Errorf("%v", err)
	}
}

func TestTaskEventIdempotent(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	ctx := context.Background()
	a := tAgent("s1", e.tower)
	task := e.task(api.TaskIn{Title: "x", State: "in_progress"}, tYou)
	if _, err := e.s.SetTaskState(ctx, task.ID, "done_proposed", "", "bad", a); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad event id: %v", err)
	}
	const x = "te_AAAAAAAAAAA"
	if _, err := e.s.SetTaskState(ctx, task.ID, "done_proposed", "", x, a); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetTaskState(ctx, task.ID, "in_progress", "", "", tYou); err != nil {
		t.Fatal(err)
	}
	got, err := e.s.SetTaskState(ctx, task.ID, "done_proposed", "", x, a)
	if err != nil || got.State != api.TaskInProgress {
		t.Fatalf("replay: %+v %v", got, err)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_events WHERE client_id = ?`, x); n != 1 {
		t.Errorf("events with id: %d", n)
	}
}

func TestSetTaskStateWrongMachine(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	task := e.task(api.TaskIn{Title: "x"}, tYou)
	_, err := e.s.SetTaskState(context.Background(), task.ID, "done_proposed", "", "", tAgent("s1", e.bluebox))
	if !errors.Is(err, ErrWrongMachine) {
		t.Errorf("%v", err)
	}
}

func TestReopenMergedRefused(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	a := e.task(api.TaskIn{Title: "a", SessionID: "s1"}, tAgent("s1", e.tower))
	b := e.task(api.TaskIn{Title: "b"}, tYou)
	if _, err := e.s.MergeTask(ctx, a.ID, b.ID, tYou); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetTaskState(ctx, a.ID, "todo", "", "", tYou); !errors.Is(err, ErrConflict) {
		t.Errorf("reopen merged: %v", err)
	}
}

func TestLinkTask(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	a := tAgent("s1", e.tower)
	todo := e.task(api.TaskIn{Title: "a"}, tYou)
	got, err := e.s.LinkTask(ctx, todo.ID, "s1", "", a)
	if err != nil || got.State != api.TaskInProgress || len(got.Sessions) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_events WHERE task_id = ? AND from_state = 'todo' AND to_state = 'in_progress' AND actor = 'session:s1'`, todo.ID); n != 1 {
		t.Errorf("link event: %d", n)
	}
	before := e.count(`SELECT COUNT(*) FROM task_events`)
	if got, err = e.s.LinkTask(ctx, todo.ID, "s1", "", a); err != nil || len(got.Sessions) != 1 {
		t.Fatalf("second link %+v %v", got, err)
	}
	if after := e.count(`SELECT COUNT(*) FROM task_events`); after != before {
		t.Errorf("no-op link wrote an event")
	}

	prop := e.task(api.TaskIn{Title: "p", SessionID: "s1"}, a)
	e.session(e.tower, "s2", "")
	got, err = e.s.LinkTask(ctx, prop.ID, "s2", "", tAgent("s2", e.tower))
	if err != nil || got.State != api.TaskProposed || len(got.Sessions) != 2 {
		t.Fatalf("proposed link %+v %v", got, err)
	}
	if _, err := e.s.LinkTask(ctx, prop.ID, "s2", "", tAgent("s2", e.bluebox)); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("wrong machine: %v", err)
	}
}

func TestMergeTask(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	e.session(e.tower, "s2", "")
	b := e.task(api.TaskIn{Title: "b", SessionID: "s1"}, tYou)
	a := e.task(api.TaskIn{Title: "a", SessionID: "s1"}, tAgent("s1", e.tower))
	if _, err := e.s.LinkTask(ctx, a.ID, "s2", "", tAgent("s2", e.tower)); err != nil {
		t.Fatal(err)
	}
	evBefore := e.count(`SELECT COUNT(*) FROM task_events`)
	got, err := e.s.MergeTask(ctx, a.ID, b.ID, tYou)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != api.TaskDropped || got.MergedInto != b.ID {
		t.Errorf("source %+v", got)
	}
	tb, _ := e.s.GetTask(ctx, b.ID)
	if len(tb.Sessions) != 2 {
		t.Errorf("target sessions %+v", tb.Sessions)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_events WHERE task_id = ? AND from_state = 'proposed' AND to_state = 'dropped' AND note = ?`, a.ID, "merged into "+b.ID); n != 1 {
		t.Errorf("merge event: %d", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_events`); n != evBefore+1 {
		t.Errorf("events %d -> %d", evBefore, n)
	}

	// Refusals.
	if _, err := e.s.MergeTask(ctx, a.ID, b.ID, tYou); !errors.Is(err, ErrConflict) {
		t.Errorf("merge a merged task again: %v", err)
	}
	p := e.task(api.TaskIn{Title: "p", SessionID: "s1"}, tAgent("s1", e.tower))
	if _, err := e.s.MergeTask(ctx, p.ID, p.ID, tYou); !errors.Is(err, ErrInvalid) {
		t.Errorf("into itself: %v", err)
	}
	if _, err := e.s.MergeTask(ctx, p.ID, a.ID, tYou); !errors.Is(err, ErrInvalid) {
		t.Errorf("into dropped: %v", err)
	}
	p2 := e.task(api.TaskIn{Title: "p2", SessionID: "s1"}, tAgent("s1", e.tower))
	if _, err := e.s.MergeTask(ctx, p.ID, p2.ID, tYou); !errors.Is(err, ErrInvalid) {
		t.Errorf("into proposed: %v", err)
	}
}

func TestEditAndOpenTasks(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	a := e.task(api.TaskIn{Title: "a"}, tYou)
	e.clock.Advance(time.Second)
	b := e.task(api.TaskIn{Title: "b"}, tYou)
	c := e.task(api.TaskIn{Title: "c"}, tYou)
	e.force(c.ID, "done")
	title := " new "
	got, err := e.s.EditTask(ctx, a.ID, api.TaskEditIn{Title: &title})
	if err != nil || got.Title != "new" {
		t.Fatalf("%+v %v", got, err)
	}
	bad := "fax"
	if _, err := e.s.EditTask(ctx, a.ID, api.TaskEditIn{Source: &bad}); !errors.Is(err, ErrInvalid) {
		t.Errorf("%v", err)
	}
	open, err := e.s.OpenTasks(ctx)
	if err != nil || len(open) != 2 || open[0].ID != a.ID || open[1].ID != b.ID {
		t.Errorf("open %+v %v", open, err)
	}
}

func TestTaskCascade(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	task := e.task(api.TaskIn{Title: "a", SessionID: "s1"}, tYou)
	if _, err := e.s.db.Exec(`DELETE FROM sessions WHERE id = 's1'`); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_sessions`); n != 0 {
		t.Errorf("links: %d", n)
	}
	got, err := e.s.GetTask(context.Background(), task.ID)
	if err != nil || len(got.Sessions) != 0 {
		t.Errorf("%+v %v", got, err)
	}
}

func TestDroppedMergedCannotBeDone(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	a := e.task(api.TaskIn{Title: "a", SessionID: "s1"}, tAgent("s1", e.tower))
	b := e.task(api.TaskIn{Title: "b"}, tYou)
	if _, err := e.s.MergeTask(ctx, a.ID, b.ID, tYou); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetTaskState(ctx, a.ID, "done", "", "", tYou); !errors.Is(err, ErrConflict) {
		t.Errorf("done on merged: %v", err)
	}
}

func TestSetTaskStateAgentName(t *testing.T) {
	e := newControlEnv(t)
	task := e.task(api.TaskIn{Title: "x"}, tYou)
	_, err := e.s.SetTaskState(context.Background(), task.ID, "done_proposed", "", "", Actor{Name: "machine:tower", Agent: true, MachineID: e.tower.ID})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("%v", err)
	}
}

func TestCreateTaskSameRefKeepsClientID(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	e.session(e.tower, "s2", "")
	open := e.task(api.TaskIn{Title: "a", Ref: "ops-1", SessionID: "s1"}, tAgent("s1", e.tower))
	if _, err := e.s.SetTaskState(ctx, open.ID, "in_progress", "", "", tYou); err != nil {
		t.Fatal(err)
	}
	a := tAgent("s2", e.tower)
	in := api.TaskIn{ID: "t_BBBBBBBBBBB", Title: "b", Ref: "OPS-1", SessionID: "s2"}
	got, created, err := e.s.CreateTask(ctx, in, a)
	if err != nil || created || got.ID != open.ID {
		t.Fatalf("fold %+v %v %v", got, created, err)
	}
	held, err := e.s.GetTask(ctx, in.ID)
	if err != nil || held.State != api.TaskDropped || held.MergedInto != open.ID || len(held.Sessions) != 0 {
		t.Fatalf("client id row %+v %v", held, err)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_events WHERE task_id = ? AND from_state = '' AND to_state = 'dropped' AND note = ?`, in.ID, "merged into "+open.ID); n != 1 {
		t.Errorf("client id events: %d", n)
	}
	if again, created, err := e.s.CreateTask(ctx, in, a); err != nil || created || again.ID != open.ID {
		t.Errorf("replayed fold %+v %v %v", again, created, err)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_events WHERE task_id = ?`, in.ID); n != 1 {
		t.Errorf("replay wrote events: %d", n)
	}

	got, err = e.s.SetTaskState(ctx, in.ID, "done_proposed", "shipped", "te_AAAAAAAAAAA", a)
	if err != nil || got.ID != open.ID || got.State != api.TaskDoneProposed {
		t.Fatalf("retarget state %+v %v", got, err)
	}
	e.session(e.tower, "s3", "")
	got, err = e.s.LinkTask(ctx, in.ID, "s3", "", tAgent("s3", e.tower))
	if err != nil || got.ID != open.ID || len(got.Sessions) != 3 {
		t.Fatalf("retarget link %+v %v", got, err)
	}
	if held, _ = e.s.GetTask(ctx, in.ID); held.State != api.TaskDropped || len(held.Sessions) != 0 {
		t.Errorf("client id row changed %+v", held)
	}
	if _, err := e.s.SetTaskState(ctx, in.ID, "todo", "", "", tYou); !errors.Is(err, ErrConflict) {
		t.Errorf("you on merged row: %v", err)
	}

	open2, err := e.s.OpenTasks(ctx)
	if err != nil || len(open2) != 1 || open2[0].ID != open.ID {
		t.Errorf("open tasks %+v %v", open2, err)
	}
	day, err := e.s.TaskDay(ctx, e.clock.Now().UTC().Format("2006-01-02"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range [][]api.Task{day.Todo, day.InProgress, day.Done} {
		for _, x := range col {
			if x.ID == in.ID {
				t.Errorf("day view shows the client id row")
			}
		}
	}
}
