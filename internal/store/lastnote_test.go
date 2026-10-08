package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestSessionLastNote(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	a := e.note("s1", "design X (systemsdev-1)")
	b := e.note("s1", "review Y systemsdev-2")
	e.note("s2", "other work systemsdev-3")
	// Back to the first task: the latest note picks it, across tasks.
	at := e.clock.Now()
	e.note("s1", "more on systemsdev-1")
	e.note("s2", "s2 again on systemsdev-2")
	e.session(e.tower, "s3", "")

	want := api.SessionNote{TaskID: a.Task.ID, TaskTitle: a.Task.Title, TaskRef: "systemsdev-1",
		TaskState: api.TaskInProgress, Text: "more on systemsdev-1", At: at}
	check := func(name string, x api.Session) {
		t.Helper()
		switch x.ID {
		case "s1":
			if x.LastNote == nil || *x.LastNote != want {
				t.Errorf("%s: s1 last note = %+v, want %+v", name, x.LastNote, want)
			}
		case "s2":
			if x.LastNote == nil || x.LastNote.TaskID != b.Task.ID || x.LastNote.Text != "s2 again on systemsdev-2" {
				t.Errorf("%s: s2 last note = %+v, want the note on %s", name, x.LastNote, b.Task.ID)
			}
		case "s3":
			if x.LastNote != nil {
				t.Errorf("%s: s3 has a note: %+v", name, x.LastNote)
			}
		}
	}
	list, err := e.s.ListSessions(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("listed %d sessions, want 3", len(list))
	}
	for _, x := range list {
		check("list", x)
	}
	for _, id := range []string{"s1", "s2", "s3"} {
		x, err := e.s.GetSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		check("get", x)
	}
	// A task event without a client ID (a state change, not a note) does
	// not count.
	if _, err := e.s.db.Exec(`INSERT INTO task_events (task_id, ts, from_state, to_state, actor, note)
		VALUES (?, ?, 'in_progress', 'done', 'session:s1', 'closed')`, b.Task.ID, formatTS(e.clock.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	x, err := e.s.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	check("get after a state change", x)
	// Two notes at the same time: the later ID wins.
	if _, err := e.s.db.Exec(`INSERT INTO task_events (task_id, ts, to_state, actor, note, client_id)
		VALUES (?, ?, 'in_progress', 'session:s1', 'same time', 'te_SAMETIME00')`, b.Task.ID, formatTS(at)); err != nil {
		t.Fatal(err)
	}
	if x, err = e.s.GetSession(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if x.LastNote == nil || x.LastNote.Text != "same time" || x.LastNote.TaskID != b.Task.ID {
		t.Errorf("tie on ts: last note = %+v, want the later ID", x.LastNote)
	}
}

// TestListSessionsLastNoteSpeed lists 500 sessions with 20 notes each.
func TestListSessionsLastNoteSpeed(t *testing.T) {
	if testing.Short() {
		t.Skip("slow setup")
	}
	e := newControlEnv(t)
	ctx := context.Background()
	tx, err := e.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	ts := formatTS(e.clock.Now())
	for i := 0; i < 500; i++ {
		sid := fmt.Sprintf("s%03d", i)
		if _, err := tx.Exec(`INSERT INTO sessions (id, machine_id, agent, started_at, last_seen_at) VALUES (?, ?, 'claude', ?, ?)`,
			sid, e.tower.ID, ts, ts); err != nil {
			t.Fatal(err)
		}
		tid := fmt.Sprintf("t_%03d", i)
		if _, err := tx.Exec(`INSERT INTO tasks (id, title, source, state, created_by, created_at, updated_at)
			VALUES (?, 'task', 'other', 'in_progress', 'you', ?, ?)`, tid, ts, ts); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 20; j++ {
			if _, err := tx.Exec(`INSERT INTO task_events (task_id, ts, to_state, actor, note, client_id) VALUES (?, ?, 'in_progress', ?, ?, ?)`,
				tid, formatTS(e.clock.Now().Add(time.Duration(j)*time.Minute)), "session:"+sid, fmt.Sprintf("note %d", j),
				fmt.Sprintf("te_%05d%06d", i, j)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var best time.Duration
	for k := 0; k < 3; k++ {
		start := time.Now()
		list, err := e.s.ListSessions(ctx, ListFilter{})
		d := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 500 || list[0].LastNote == nil || list[0].LastNote.Text != "note 19" {
			t.Fatalf("list: %d sessions, first note %+v", len(list), list[0].LastNote)
		}
		if k == 0 || d < best {
			best = d
		}
	}
	start := time.Now()
	if _, err := e.s.lastNotes(ctx, ""); err != nil {
		t.Fatal(err)
	}
	t.Logf("ListSessions, 500 sessions x 20 notes: %v; the last-note query alone: %v", best, time.Since(start))
	if best > 50*time.Millisecond {
		t.Errorf("ListSessions took %v, want under 50ms", best)
	}
}
