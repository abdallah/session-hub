package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

var noteSeq int

// note adds a note from session sid on the tower (or as you when sid is
// empty) and fails the test on an error.
func (e *controlEnv) note(sid, text string) api.NoteResult {
	e.t.Helper()
	r, err := e.addNote(sid, text, nextNoteID())
	if err != nil {
		e.t.Fatalf("note %q: %v", text, err)
	}
	e.clock.Advance(time.Minute)
	return r
}

func (e *controlEnv) addNote(sid, text, id string) (api.NoteResult, error) {
	a := tYou
	if sid != "" {
		if _, err := e.s.GetSession(context.Background(), sid); err != nil {
			e.session(e.tower, sid, "")
		}
		a = tAgent(sid, e.tower)
	}
	return e.s.AddNote(context.Background(), api.NoteIn{ID: id, Text: text, SessionID: sid}, a, testRefURLs)
}

func nextNoteID() string {
	noteSeq++
	return fmt.Sprintf("te_%011d", noteSeq)
}

var testRefURLs = RefURLs{Ticket: "https://yt/issue/{ref}", MR: "https://gl/mr/{n}"}

func TestNoteRefCreatesThenJoins(t *testing.T) {
	e := newControlEnv(t)
	r := e.note("s1", "investigate X (systemsdev-1)")
	if r.Action != api.NoteCreated || r.Task.State != api.TaskInProgress || r.Task.Source != "ticket" ||
		r.Task.Ref != "systemsdev-1" || r.Task.RefURL != "https://yt/issue/systemsdev-1" || r.Task.Title != "investigate X" {
		t.Fatalf("first note: %+v", r)
	}
	r2 := e.note("s2", "more on SYSTEMSDEV-1")
	if r2.Action != api.NoteJoined || r2.Task.ID != r.Task.ID || len(r2.Task.Sessions) != 2 {
		t.Errorf("second note: %+v", r2)
	}
	if mr := e.note("s3", "MR !7 review"); mr.Task.RefURL != "https://gl/mr/7" || mr.Task.Ref != "!7" {
		t.Errorf("MR note: %+v", mr.Task)
	}
}

func TestNoteSessionTask(t *testing.T) {
	e := newControlEnv(t)
	a := e.note("s1", "design X")
	if b := e.note("s1", "tune X"); b.Action != api.NoteJoined || b.Task.ID != a.Task.ID {
		t.Errorf("same session: %+v", b)
	}
	c := e.note("s1", "new ticket systemsdev-2")
	if c.Action != api.NoteCreated || c.Task.ID == a.Task.ID {
		t.Errorf("ref switch: %+v", c)
	}
	if d := e.note("s1", "back to X"); d.Task.ID != c.Task.ID {
		t.Errorf("after the switch the note joins %s (%s), want the latest span's task %s", d.Task.ID, d.Task.Title, c.Task.ID)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_spans WHERE session_id = 's1'`); n != 2 {
		t.Errorf("spans: %d, want 2", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_events WHERE task_id = ? AND note = 'tune X'`, a.Task.ID); n != 1 {
		t.Errorf("note events: %d", n)
	}
}

// An MR ref is the result of the session's work: with a current task, the
// note goes to it.
func TestNoteMRJoinsSessionTask(t *testing.T) {
	e := newControlEnv(t)
	a := e.note("s1", "fix silent alert (16975)")
	if r := e.note("s1", "done: alert fix MR !2223"); r.Task.ID != a.Task.ID || r.Action != api.NoteDone {
		t.Errorf("MR finish: %+v", r)
	}
	if r := e.note("s2", "MR !7 review"); r.Action != api.NoteCreated || r.Task.Ref != "!7" {
		t.Errorf("MR with no session task: %+v", r)
	}
}

func TestNoteFinish(t *testing.T) {
	e := newControlEnv(t)
	a := e.note("s1", "work on Y")
	if r := e.note("s1", "done: Y shipped"); r.Action != api.NoteDone || r.Task.ID != a.Task.ID || r.Task.State != api.TaskDone {
		t.Errorf("finish session task: %+v", r)
	}
	tk := e.note("s2", "start systemsdev-1")
	other := e.note("s3", "something else")
	if r := e.note("s3", "closed systemsdev-1"); r.Task.ID != tk.Task.ID || r.Task.State != api.TaskDone {
		t.Errorf("finish by ref: %+v", r)
	}
	if got, _ := e.s.GetTask(context.Background(), other.Task.ID); got.State != api.TaskInProgress {
		t.Errorf("the session's own task moved to %s", got.State)
	}
	if r := e.note("s4", "done: a thing nobody started"); r.Action != api.NoteCreated || r.Task.State != api.TaskDone {
		t.Errorf("finish with no task: %+v", r)
	}
}

func TestNoteReopensDone(t *testing.T) {
	e := newControlEnv(t)
	a := e.note("s1", "systemsdev-3 start")
	e.note("s1", "done: systemsdev-3")
	e.clock.Advance(time.Hour)
	if r := e.note("s2", "more systemsdev-3"); r.Task.ID != a.Task.ID || r.Task.State != api.TaskInProgress || r.Action != api.NoteJoined {
		t.Errorf("reopen: %+v", r)
	}
}

func TestNoteRetry(t *testing.T) {
	e := newControlEnv(t)
	id := nextNoteID()
	r1, err := e.addNote("s1", "design Z", id)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.addNote("s1", "design Z", id)
	if err != nil || r2.Task.ID != r1.Task.ID || r2.Action != api.NoteCreated {
		t.Errorf("retry: %+v %v", r2, err)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_events WHERE client_id = ?`, id); n != 1 {
		t.Errorf("events: %d", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM tasks`); n != 1 {
		t.Errorf("tasks: %d", n)
	}
}

func TestNoteSkipsDroppedAndMerged(t *testing.T) {
	e := newControlEnv(t)
	a := e.note("s1", "systemsdev-4 work")
	e.force(a.Task.ID, api.TaskDropped)
	if r := e.note("s2", "systemsdev-4 again"); r.Task.ID == a.Task.ID || r.Action != api.NoteCreated {
		t.Errorf("dropped task picked: %+v", r)
	}
}

func TestNoteOtherMachine(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	_, err := e.s.AddNote(context.Background(), api.NoteIn{ID: nextNoteID(), Text: "x", SessionID: "s1"}, tAgent("s1", e.bluebox), testRefURLs)
	if !errors.Is(err, ErrWrongMachine) {
		t.Errorf("%v", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM tasks`); n != 0 {
		t.Errorf("stored %d", n)
	}
}

// A retry from another machine is refused, not answered from the first note.
func TestNoteRetryOtherMachine(t *testing.T) {
	e := newControlEnv(t)
	id := nextNoteID()
	if _, err := e.addNote("s1", "design Z", id); err != nil {
		t.Fatal(err)
	}
	_, err := e.s.AddNote(context.Background(), api.NoteIn{ID: id, Text: "design Z", SessionID: "s1"}, tAgent("s1", e.bluebox), testRefURLs)
	if !errors.Is(err, ErrWrongMachine) {
		t.Errorf("%v, want ErrWrongMachine", err)
	}
}

func TestNoteValidation(t *testing.T) {
	e := newControlEnv(t)
	for _, c := range []struct{ text, id string }{
		{"", nextNoteID()},
		{strings.Repeat("x", 501), nextNoteID()},
		{"fine", "te_short"},
		{"done:", nextNoteID()},
	} {
		if _, err := e.addNote("s1", c.text, c.id); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q %q: %v, want ErrInvalid", c.text[:min(len(c.text), 10)], c.id, err)
		}
	}
	// No session: a plain note always creates a task.
	a := e.note("", "plain one")
	if b := e.note("", "plain two"); b.Task.ID == a.Task.ID || len(b.Task.Sessions) != 0 {
		t.Errorf("no-session notes: %+v", b)
	}
}

func TestNoteConcurrentRef(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.session(e.tower, "s2", "")
	var wg sync.WaitGroup
	ids := make([]string, 2)
	for i, sid := range []string{"s1", "s2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.s.AddNote(context.Background(), api.NoteIn{ID: fmt.Sprintf("te_%011d", 9000+i), Text: "systemsdev-9 go", SessionID: sid},
				tAgent(sid, e.tower), testRefURLs)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = r.Task.ID
		}()
	}
	wg.Wait()
	if ids[0] != ids[1] {
		t.Errorf("two tasks for one ref: %v", ids)
	}
}

func TestMergeMovesSpans(t *testing.T) {
	e := newControlEnv(t)
	a := e.note("s1", "first thing")
	b := e.note("s2", "second thing")
	if _, err := e.s.MergeTask(context.Background(), a.Task.ID, b.Task.ID, tYou); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_spans WHERE task_id = ?`, b.Task.ID); n != 2 {
		t.Errorf("spans on the target: %d, want 2", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM task_spans WHERE task_id = ?`, a.Task.ID); n != 0 {
		t.Errorf("spans left on the merged task: %d", n)
	}
}
