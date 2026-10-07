package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/tasks"
)

// doneReopenWindow is how long after a task is done a note with its ref
// still reaches it instead of starting a new task.
const doneReopenWindow = 24 * time.Hour

// RefURLs are the templates for a note ref's link: {ref} in Ticket is the
// ticket ref, {n} in MR is the merge request number. An empty template
// gives no link.
type RefURLs struct {
	Ticket, MR string
}

// For returns the link for ref, or "".
func (u RefURLs) For(ref string, kind tasks.RefKind) string {
	switch {
	case kind == tasks.RefTicket && u.Ticket != "":
		return strings.ReplaceAll(u.Ticket, "{ref}", ref)
	case kind == tasks.RefMR && u.MR != "":
		return strings.ReplaceAll(u.MR, "{n}", strings.TrimPrefix(ref, "!"))
	}
	return ""
}

// AddNote routes a worklog note to a task: the task with the note's ref,
// else the session's current task, else a new one. A start note puts the
// task in progress, a finish note marks it done; either links the session
// and records a span when the session changes task. The note is stored as
// a task event whose client ID is in.ID, so a retry returns the first
// answer.
func (s *Store) AddNote(ctx context.Context, in api.NoteIn, a Actor, urls RefURLs) (api.NoteResult, error) {
	if err := checkTaskActor(a); err != nil {
		return api.NoteResult{}, err
	}
	if !ValidTaskEventID(in.ID) {
		return api.NoteResult{}, invalidf("note id %q", in.ID)
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return api.NoteResult{}, invalidf("note is empty")
	}
	if err := checkText("note", text, maxTaskNoteRunes); err != nil {
		return api.NoteResult{}, err
	}
	n := tasks.Parse(text)
	if n.Title == "" {
		return api.NoteResult{}, invalidf("note %q names no work", text)
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.NoteResult{}, err
	}
	defer tx.Rollback()
	if in.SessionID != "" {
		if err := checkAgentOwner(ctx, tx, a, in.SessionID); err != nil {
			return api.NoteResult{}, err
		}
		if err := sessionExistsTx(ctx, tx, in.SessionID); err != nil {
			return api.NoteResult{}, err
		}
	}
	if seen, err := eventSeenTx(ctx, tx, in.ID); err != nil {
		return api.NoteResult{}, err
	} else if seen {
		return earlierNote(ctx, tx, in.ID)
	}
	id, from, err := noteTaskTx(ctx, tx, n, in.SessionID, now)
	if err != nil {
		return api.NoteResult{}, err
	}
	to, action := api.TaskInProgress, api.NoteJoined
	if n.Finish {
		to, action = api.TaskDone, api.NoteDone
	}
	if id == "" {
		if id, err = newTaskID(); err != nil {
			return api.NoteResult{}, err
		}
		source := "other"
		if n.Ref != "" {
			source = "ticket"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks (id, title, ref, ref_url, source, state, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, n.Title, n.Ref, urls.For(n.Ref, n.Kind), source, to, a.Name, formatTS(now), formatTS(now)); err != nil {
			return api.NoteResult{}, err
		}
		action = api.NoteCreated
	} else if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`, to, formatTS(now), id); err != nil {
		return api.NoteResult{}, err
	}
	if err := insertTaskEventTx(ctx, tx, id, now, from, to, a.Name, text, in.ID); err != nil {
		return api.NoteResult{}, err
	}
	if in.SessionID != "" {
		if _, err := linkTx(ctx, tx, id, in.SessionID, now); err != nil {
			return api.NoteResult{}, err
		}
		cur, err := spanTaskTx(ctx, tx, in.SessionID)
		if err != nil {
			return api.NoteResult{}, err
		}
		if cur != id {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO task_spans (session_id, task_id, started_at) VALUES (?, ?, ?)`,
				in.SessionID, id, formatTS(now)); err != nil {
				return api.NoteResult{}, err
			}
		}
	}
	t, err := readTask(ctx, tx, id)
	if err != nil {
		return api.NoteResult{}, err
	}
	return api.NoteResult{Task: t, Action: action}, tx.Commit()
}

// noteTaskTx picks the task a note goes to and returns its ID and state, or
// "" for a new task. A ref picks the newest live task with that ref, a done
// one only within doneReopenWindow; without a ref, the session's latest
// span's task if open, else its newest open linked task.
func noteTaskTx(ctx context.Context, tx *sql.Tx, n tasks.Note, sessionID string, now time.Time) (id, state string, err error) {
	pick := func(q string, args ...any) error {
		err := tx.QueryRowContext(ctx, q, args...).Scan(&id, &state)
		if errors.Is(err, sql.ErrNoRows) {
			id, state = "", ""
			return nil
		}
		return err
	}
	// An MR is usually the result of the session's current task: look
	// there first.
	if n.Kind == tasks.RefMR && sessionID != "" {
		if id, state, err = sessionTaskTx(ctx, tx, sessionID); err != nil || id != "" {
			return id, state, err
		}
	}
	if n.Ref != "" {
		err = pick(`SELECT id, state FROM tasks WHERE lower(ref) = lower(?) AND merged_into IS NULL
			AND (state IN `+openTaskStates+` OR (state = ? AND updated_at >= ?))
			ORDER BY updated_at DESC LIMIT 1`, n.Ref, api.TaskDone, formatTS(now.Add(-doneReopenWindow)))
		return id, state, err
	}
	if sessionID == "" {
		return "", "", nil
	}
	return sessionTaskTx(ctx, tx, sessionID)
}

// sessionTaskTx is the session's current task: its latest span's task if
// open, else its newest open linked task, else "".
func sessionTaskTx(ctx context.Context, tx *sql.Tx, sessionID string) (string, string, error) {
	var id, state string
	scan := func(q string) error {
		err := tx.QueryRowContext(ctx, q, sessionID).Scan(&id, &state)
		if errors.Is(err, sql.ErrNoRows) {
			id, state = "", ""
			return nil
		}
		return err
	}
	if err := scan(`SELECT t.id, t.state FROM task_spans sp JOIN tasks t ON t.id = sp.task_id
		WHERE sp.session_id = ? ORDER BY sp.started_at DESC LIMIT 1`); err != nil {
		return "", "", err
	}
	if id != "" && strings.Contains(openTaskStates, "'"+state+"'") {
		return id, state, nil
	}
	err := scan(`SELECT t.id, t.state FROM task_sessions ts JOIN tasks t ON t.id = ts.task_id
		WHERE ts.session_id = ? AND t.state IN ` + openTaskStates + ` ORDER BY t.updated_at DESC LIMIT 1`)
	return id, state, err
}

// spanTaskTx is the task of the session's latest span, or "".
func spanTaskTx(ctx context.Context, tx *sql.Tx, sessionID string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT task_id FROM task_spans WHERE session_id = ? ORDER BY started_at DESC LIMIT 1`,
		sessionID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// earlierNote rebuilds the answer to a note already stored under eventID.
func earlierNote(ctx context.Context, tx *sql.Tx, eventID string) (api.NoteResult, error) {
	var taskID, from, to string
	if err := tx.QueryRowContext(ctx, `SELECT task_id, from_state, to_state FROM task_events WHERE client_id = ?`, eventID).
		Scan(&taskID, &from, &to); err != nil {
		return api.NoteResult{}, err
	}
	t, err := readTask(ctx, tx, taskID)
	if err != nil {
		return api.NoteResult{}, err
	}
	action := api.NoteJoined
	switch {
	case from == "":
		action = api.NoteCreated
	case to == api.TaskDone:
		action = api.NoteDone
	}
	return api.NoteResult{Task: t, Action: action}, nil
}
