package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
)

// Task limits. See docs/server.md, "Tasks".
const (
	maxTaskTitleRunes = 200
	maxTaskRefRunes   = 200
	maxTaskRefURLLen  = 2000
	maxTaskNoteRunes  = 500
)

var (
	taskIDRE      = regexp.MustCompile(`^t_[A-Za-z0-9_-]{11}$`)
	taskEventIDRE = regexp.MustCompile(`^te_[A-Za-z0-9_-]{11}$`)
	taskSources   = map[string]bool{"ticket": true, "email": true, "chat": true, "other": true}
)

// ValidTaskID reports whether id has the shape of a task ID.
func ValidTaskID(id string) bool { return taskIDRE.MatchString(id) }

// ValidTaskEventID reports whether id has the shape of a task event ID.
func ValidTaskEventID(id string) bool { return taskEventIDRE.MatchString(id) }

// Actor is who makes a task change. The server decides it, not the client.
type Actor struct {
	Name      string // web:<name>, machine:<name>, or session:<id>
	Agent     bool   // an agent has fewer rights than you
	MachineID int64  // the agent's machine, checked against the session's owner
}

// sessionID is the session an agent acts for, from its session:<id> name.
func (a Actor) sessionID() (string, bool) {
	id, ok := strings.CutPrefix(a.Name, "session:")
	return id, ok && id != ""
}

// taskMoves is the transition table. A move absent from the table, or not
// allowed for the actor, is ErrConflict. todo to in_progress is not an agent
// move: LinkTask makes it.
var taskMoves = map[[2]string]struct{ you, agent bool }{
	{api.TaskProposed, api.TaskTodo}:           {true, false},
	{api.TaskProposed, api.TaskInProgress}:     {true, false},
	{api.TaskProposed, api.TaskDropped}:        {true, false},
	{api.TaskTodo, api.TaskInProgress}:         {true, false},
	{api.TaskInProgress, api.TaskTodo}:         {true, false},
	{api.TaskTodo, api.TaskDoneProposed}:       {false, true},
	{api.TaskInProgress, api.TaskDoneProposed}: {false, true},
	{api.TaskDoneProposed, api.TaskDone}:       {true, false},
	{api.TaskDoneProposed, api.TaskInProgress}: {true, false},
	{api.TaskTodo, api.TaskDone}:               {true, false},
	{api.TaskInProgress, api.TaskDone}:         {true, false},
	{api.TaskTodo, api.TaskDropped}:            {true, false},
	{api.TaskInProgress, api.TaskDropped}:      {true, false},
	{api.TaskDoneProposed, api.TaskDropped}:    {true, false},
	{api.TaskDone, api.TaskDropped}:            {true, false},
	{api.TaskDone, api.TaskTodo}:               {true, false},
	{api.TaskDone, api.TaskInProgress}:         {true, false},
	{api.TaskDropped, api.TaskDone}:            {true, false},
	{api.TaskDropped, api.TaskTodo}:            {true, false},
	{api.TaskDropped, api.TaskInProgress}:      {true, false},
}

const openTaskStates = `('proposed','todo','in_progress','done_proposed')`

func newTaskID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "t_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func checkTaskRefURL(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > maxTaskRefURLLen {
		return invalidf("ref_url is %d bytes, at most %d allowed", len(v), maxTaskRefURLLen)
	}
	if err := checkNoControl("ref_url", v); err != nil {
		return err
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return invalidf("ref_url must be an http or https URL")
	}
	return nil
}

func checkTaskTitle(v string) error {
	if v == "" {
		return invalidf("title is empty")
	}
	return checkText("title", v, maxTaskTitleRunes)
}

func checkTaskSource(v string) error {
	if !taskSources[v] {
		return invalidf("source %q: use ticket, email, chat, or other", v)
	}
	return nil
}

func checkTaskActor(a Actor) error {
	if a.Name == "" {
		return invalidf("actor is empty")
	}
	return checkText("actor", a.Name, maxCreatedByRunes)
}

// checkAgentOwner runs checkOwnerTx for an agent's session. You are not
// checked.
func checkAgentOwner(ctx context.Context, tx *sql.Tx, a Actor, sessionID string) error {
	if !a.Agent || sessionID == "" {
		return nil
	}
	return checkOwnerTx(ctx, tx, a.MachineID, sessionID)
}

// sessionExistsTx returns ErrNotFound unless session id exists.
func sessionExistsTx(ctx context.Context, tx *sql.Tx, id string) error {
	_, _, err := ownerTx(ctx, tx, id)
	return err
}

type taskQuerier interface {
	querier
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readTask(ctx context.Context, q taskQuerier, id string) (api.Task, error) {
	var t api.Task
	var created, updated string
	var merged sql.NullString
	err := q.QueryRowContext(ctx, `SELECT id, title, ref, ref_url, source, state, merged_into, created_by, created_at, updated_at
		FROM tasks WHERE id = ?`, id).
		Scan(&t.ID, &t.Title, &t.Ref, &t.RefURL, &t.Source, &t.State, &merged, &t.CreatedBy, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("%w: task %s", ErrNotFound, id)
	}
	if err != nil {
		return t, err
	}
	t.MergedInto = merged.String
	if t.CreatedAt, err = parseTS(created); err != nil {
		return t, err
	}
	if t.UpdatedAt, err = parseTS(updated); err != nil {
		return t, err
	}
	t.Sessions = []api.TaskSession{}
	rows, err := q.QueryContext(ctx, `SELECT s.id, s.title, m.name FROM task_sessions ts
		JOIN sessions s ON s.id = ts.session_id JOIN machines m ON m.id = s.machine_id
		WHERE ts.task_id = ? ORDER BY ts.linked_at, s.id`, id)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var ts api.TaskSession
		if err := rows.Scan(&ts.ID, &ts.Title, &ts.Machine); err != nil {
			return t, err
		}
		t.Sessions = append(t.Sessions, ts)
	}
	return t, rows.Err()
}

// GetTask returns task id, or ErrNotFound.
func (s *Store) GetTask(ctx context.Context, id string) (api.Task, error) {
	if !ValidTaskID(id) {
		return api.Task{}, invalidf("task id %q", id)
	}
	return readTask(ctx, s.db, id)
}

// OpenTasks returns the proposed, todo, in-progress, and done-proposed tasks,
// oldest first.
func (s *Store) OpenTasks(ctx context.Context) ([]api.Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM tasks WHERE state IN `+openTaskStates+` ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []api.Task{}
	for _, id := range ids {
		t, err := readTask(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func insertTaskEventTx(ctx context.Context, tx *sql.Tx, taskID string, ts time.Time, from, to, actor, note, clientID string) error {
	var cid any
	if clientID != "" {
		cid = clientID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO task_events (task_id, ts, from_state, to_state, actor, note, client_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, taskID, formatTS(ts), from, to, actor, note, cid)
	return err
}

// linkTx links a session to a task and reports whether the link is new.
func linkTx(ctx context.Context, tx *sql.Tx, taskID, sessionID string, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_sessions (task_id, session_id, linked_at) VALUES (?, ?, ?)`,
		taskID, sessionID, formatTS(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func touchTaskTx(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, formatTS(now), id)
	return err
}

// eventSeenTx reports whether a task event with this client ID is stored.
func eventSeenTx(ctx context.Context, tx *sql.Tx, eventID string) (bool, error) {
	if eventID == "" {
		return false, nil
	}
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events WHERE client_id = ?`, eventID).Scan(&n)
	return n > 0, err
}

// CreateTask stores a task. An agent's task is always proposed; you choose
// todo (the default) or in_progress. A repeated in.ID returns the stored task
// with created false. An agent create whose ref matches an open task's, ignoring
// case, returns that task and links the session instead; a non-empty in.ID is
// then stored as a dropped task merged into that one, so the agent's id still
// resolves. The same match from you is ErrConflict. A non-empty in.SessionID is linked for any actor, with no
// state change.
func (s *Store) CreateTask(ctx context.Context, in api.TaskIn, a Actor) (api.Task, bool, error) {
	if err := checkTaskActor(a); err != nil {
		return api.Task{}, false, err
	}
	in.Title = strings.TrimSpace(in.Title)
	in.Ref = strings.TrimSpace(in.Ref)
	in.RefURL = strings.TrimSpace(in.RefURL)
	if in.Source == "" {
		in.Source = "other"
	}
	if in.ID != "" && !ValidTaskID(in.ID) {
		return api.Task{}, false, invalidf("task id %q: use t_ and 11 URL-safe base64 characters", in.ID)
	}
	if err := checkTaskTitle(in.Title); err != nil {
		return api.Task{}, false, err
	}
	if err := checkText("ref", in.Ref, maxTaskRefRunes); err != nil {
		return api.Task{}, false, err
	}
	if err := checkTaskRefURL(in.RefURL); err != nil {
		return api.Task{}, false, err
	}
	if err := checkTaskSource(in.Source); err != nil {
		return api.Task{}, false, err
	}
	state := api.TaskProposed
	if !a.Agent {
		switch in.State {
		case "", api.TaskTodo:
			state = api.TaskTodo
		case api.TaskInProgress:
			state = api.TaskInProgress
		default:
			return api.Task{}, false, invalidf("state %q: a new task is todo or in_progress", in.State)
		}
	}

	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Task{}, false, err
	}
	defer tx.Rollback()

	if in.SessionID != "" {
		if a.Agent {
			err = checkOwnerTx(ctx, tx, a.MachineID, in.SessionID)
		} else {
			err = sessionExistsTx(ctx, tx, in.SessionID)
		}
		if err != nil {
			return api.Task{}, false, err
		}
	}
	if in.ID != "" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE id = ?`, in.ID).Scan(&n); err != nil {
			return api.Task{}, false, err
		}
		if n > 0 {
			t, err := readTask(ctx, tx, in.ID)
			if err == nil && a.Agent && t.MergedInto != "" {
				t, err = readTask(ctx, tx, t.MergedInto)
			}
			return t, false, err
		}
	}
	if in.Ref != "" {
		rows, err := tx.QueryContext(ctx, `SELECT id, ref FROM tasks WHERE ref <> '' AND state IN `+openTaskStates+` ORDER BY created_at, id`)
		if err != nil {
			return api.Task{}, false, err
		}
		match := ""
		for rows.Next() {
			var id, ref string
			if err := rows.Scan(&id, &ref); err != nil {
				rows.Close()
				return api.Task{}, false, err
			}
			if match == "" && strings.EqualFold(strings.TrimSpace(ref), in.Ref) {
				match = id
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return api.Task{}, false, err
		}
		if match != "" {
			if !a.Agent {
				return api.Task{}, false, fmt.Errorf("%w: task %s already tracks ref %q", ErrConflict, match, in.Ref)
			}
			if in.SessionID != "" {
				added, err := linkTx(ctx, tx, match, in.SessionID, now)
				if err != nil {
					return api.Task{}, false, err
				}
				if added {
					if err := touchTaskTx(ctx, tx, match, now); err != nil {
						return api.Task{}, false, err
					}
				}
			}
			if in.ID != "" {
				// Keep the id the agent may already hold, so a later
				// propose_done or link_task on it reaches match.
				if _, err := tx.ExecContext(ctx, `INSERT INTO tasks (id, title, ref, ref_url, source, state, merged_into, created_by, created_at, updated_at)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					in.ID, in.Title, in.Ref, in.RefURL, in.Source, api.TaskDropped, match, a.Name, formatTS(now), formatTS(now)); err != nil {
					return api.Task{}, false, err
				}
				if err := insertTaskEventTx(ctx, tx, in.ID, now, "", api.TaskDropped, a.Name, "merged into "+match, ""); err != nil {
					return api.Task{}, false, err
				}
			}
			t, err := readTask(ctx, tx, match)
			if err != nil {
				return api.Task{}, false, err
			}
			if err := tx.Commit(); err != nil {
				return api.Task{}, false, err
			}
			return t, false, nil
		}
	}
	id := in.ID
	if id == "" {
		if id, err = newTaskID(); err != nil {
			return api.Task{}, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks (id, title, ref, ref_url, source, state, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Title, in.Ref, in.RefURL, in.Source, state, a.Name, formatTS(now), formatTS(now)); err != nil {
		return api.Task{}, false, err
	}
	if err := insertTaskEventTx(ctx, tx, id, now, "", state, a.Name, "", ""); err != nil {
		return api.Task{}, false, err
	}
	if in.SessionID != "" {
		if _, err := linkTx(ctx, tx, id, in.SessionID, now); err != nil {
			return api.Task{}, false, err
		}
	}
	t, err := readTask(ctx, tx, id)
	if err != nil {
		return api.Task{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return api.Task{}, false, err
	}
	return t, true, nil
}

// SetTaskState moves task id to state to, if the actor may. A repeated
// eventID returns the current task and writes nothing. You cannot move a task
// merged into another; an agent's move goes to the merge target.
func (s *Store) SetTaskState(ctx context.Context, id, to, note, eventID string, a Actor) (api.Task, error) {
	if err := checkTaskActor(a); err != nil {
		return api.Task{}, err
	}
	if !ValidTaskID(id) {
		return api.Task{}, invalidf("task id %q", id)
	}
	if eventID != "" && !ValidTaskEventID(eventID) {
		return api.Task{}, invalidf("event id %q: use te_ and 11 URL-safe base64 characters", eventID)
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > maxTaskNoteRunes {
		return api.Task{}, invalidf("note is %d characters, at most %d allowed", utf8.RuneCountInString(note), maxTaskNoteRunes)
	}
	if err := checkNoControl("note", note); err != nil {
		return api.Task{}, err
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Task{}, err
	}
	defer tx.Rollback()
	if a.Agent {
		sid, ok := a.sessionID()
		if !ok {
			return api.Task{}, invalidf("agent actor %q: use session:<id>", a.Name)
		}
		if err := checkAgentOwner(ctx, tx, a, sid); err != nil {
			return api.Task{}, err
		}
	}
	t, id, err := readTaskFor(ctx, tx, id, a)
	if err != nil {
		return api.Task{}, err
	}
	if seen, err := eventSeenTx(ctx, tx, eventID); err != nil || seen {
		return t, err
	}
	rule, ok := taskMoves[[2]string{t.State, to}]
	if !ok || (a.Agent && !rule.agent) || (!a.Agent && !rule.you) {
		msg := ""
		if t.State == api.TaskDone && to == api.TaskDoneProposed {
			msg = ": the task is already done"
		}
		return api.Task{}, fmt.Errorf("%w: cannot move a %s task to %s%s", ErrConflict, t.State, to, msg)
	}
	if t.MergedInto != "" {
		return api.Task{}, fmt.Errorf("%w: cannot move a %s task to %s: it was merged into %s", ErrConflict, t.State, to, t.MergedInto)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`, to, formatTS(now), id); err != nil {
		return api.Task{}, err
	}
	if err := insertTaskEventTx(ctx, tx, id, now, t.State, to, a.Name, note, eventID); err != nil {
		return api.Task{}, err
	}
	if t, err = readTask(ctx, tx, id); err != nil {
		return api.Task{}, err
	}
	return t, tx.Commit()
}

// LinkTask links sessionID to task id. A todo task moves to in_progress with
// one event; any other state is left alone. Linking a linked session is a
// no-op, as is a repeated eventID. An agent's link to a merged task goes to
// the merge target.
func (s *Store) LinkTask(ctx context.Context, id, sessionID, eventID string, a Actor) (api.Task, error) {
	if err := checkTaskActor(a); err != nil {
		return api.Task{}, err
	}
	if !ValidTaskID(id) {
		return api.Task{}, invalidf("task id %q", id)
	}
	if sessionID == "" {
		return api.Task{}, invalidf("session_id is empty")
	}
	if eventID != "" && !ValidTaskEventID(eventID) {
		return api.Task{}, invalidf("event id %q: use te_ and 11 URL-safe base64 characters", eventID)
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Task{}, err
	}
	defer tx.Rollback()
	if a.Agent {
		err = checkOwnerTx(ctx, tx, a.MachineID, sessionID)
	} else {
		err = sessionExistsTx(ctx, tx, sessionID)
	}
	if err != nil {
		return api.Task{}, err
	}
	t, id, err := readTaskFor(ctx, tx, id, a)
	if err != nil {
		return api.Task{}, err
	}
	if seen, err := eventSeenTx(ctx, tx, eventID); err != nil || seen {
		return t, err
	}
	added, err := linkTx(ctx, tx, id, sessionID, now)
	if err != nil {
		return api.Task{}, err
	}
	if t.State == api.TaskTodo {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ? WHERE id = ?`, api.TaskInProgress, id); err != nil {
			return api.Task{}, err
		}
		if err := insertTaskEventTx(ctx, tx, id, now, api.TaskTodo, api.TaskInProgress, a.Name, "", eventID); err != nil {
			return api.Task{}, err
		}
		added = true
	}
	if added {
		if err := touchTaskTx(ctx, tx, id, now); err != nil {
			return api.Task{}, err
		}
	}
	if t, err = readTask(ctx, tx, id); err != nil {
		return api.Task{}, err
	}
	return t, tx.Commit()
}

// readTaskFor reads task id for actor a. An agent acting on a task merged
// into another acts on the merge target instead, one hop only: the agent may
// hold an id the server folded into an existing task. It returns the task
// and the id it read.
func readTaskFor(ctx context.Context, tx *sql.Tx, id string, a Actor) (api.Task, string, error) {
	t, err := readTask(ctx, tx, id)
	if err != nil || !a.Agent || t.MergedInto == "" {
		return t, id, err
	}
	id = t.MergedInto
	t, err = readTask(ctx, tx, id)
	return t, id, err
}

// MergeTask drops proposal id into task into: the proposal becomes dropped
// with merged_into set and one event, and its session links move to the
// target. The target must not be proposed or dropped.
func (s *Store) MergeTask(ctx context.Context, id, into string, a Actor) (api.Task, error) {
	if err := checkTaskActor(a); err != nil {
		return api.Task{}, err
	}
	if !ValidTaskID(id) {
		return api.Task{}, invalidf("task id %q", id)
	}
	if !ValidTaskID(into) {
		return api.Task{}, invalidf("into %q: not a task id", into)
	}
	if id == into {
		return api.Task{}, invalidf("a task cannot merge into itself")
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Task{}, err
	}
	defer tx.Rollback()
	src, err := readTask(ctx, tx, id)
	if err != nil {
		return api.Task{}, err
	}
	dst, err := readTask(ctx, tx, into)
	if err != nil {
		return api.Task{}, err
	}
	if src.State == api.TaskDropped || src.MergedInto != "" {
		return api.Task{}, fmt.Errorf("%w: a %s task does not merge", ErrConflict, src.State)
	}
	if dst.State == api.TaskProposed || dst.State == api.TaskDropped {
		return api.Task{}, invalidf("cannot merge into a %s task", dst.State)
	}
	for _, ss := range src.Sessions {
		if _, err := linkTx(ctx, tx, into, ss.ID, now); err != nil {
			return api.Task{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_sessions WHERE task_id = ?`, id); err != nil {
		return api.Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_spans SET task_id = ? WHERE task_id = ?`, into, id); err != nil {
		return api.Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, merged_into = ?, updated_at = ? WHERE id = ?`,
		api.TaskDropped, into, formatTS(now), id); err != nil {
		return api.Task{}, err
	}
	if err := insertTaskEventTx(ctx, tx, id, now, src.State, api.TaskDropped, a.Name, "merged into "+into, ""); err != nil {
		return api.Task{}, err
	}
	if len(src.Sessions) > 0 {
		if err := touchTaskTx(ctx, tx, into, now); err != nil {
			return api.Task{}, err
		}
	}
	if src, err = readTask(ctx, tx, id); err != nil {
		return api.Task{}, err
	}
	return src, tx.Commit()
}

// EditTask changes the fields in.* sets. It writes no event.
func (s *Store) EditTask(ctx context.Context, id string, in api.TaskEditIn) (api.Task, error) {
	if !ValidTaskID(id) {
		return api.Task{}, invalidf("task id %q", id)
	}
	trim := func(p *string) *string {
		if p == nil {
			return nil
		}
		v := strings.TrimSpace(*p)
		return &v
	}
	in.Title, in.Ref, in.RefURL, in.Source = trim(in.Title), trim(in.Ref), trim(in.RefURL), trim(in.Source)
	if in.Title != nil {
		if err := checkTaskTitle(*in.Title); err != nil {
			return api.Task{}, err
		}
	}
	if in.Ref != nil {
		if err := checkText("ref", *in.Ref, maxTaskRefRunes); err != nil {
			return api.Task{}, err
		}
	}
	if in.RefURL != nil {
		if err := checkTaskRefURL(*in.RefURL); err != nil {
			return api.Task{}, err
		}
	}
	if in.Source != nil {
		if err := checkTaskSource(*in.Source); err != nil {
			return api.Task{}, err
		}
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Task{}, err
	}
	defer tx.Rollback()
	t, err := readTask(ctx, tx, id)
	if err != nil {
		return api.Task{}, err
	}
	if in.Title != nil {
		t.Title = *in.Title
	}
	if in.Ref != nil {
		t.Ref = *in.Ref
	}
	if in.RefURL != nil {
		t.RefURL = *in.RefURL
	}
	if in.Source != nil {
		t.Source = *in.Source
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET title = ?, ref = ?, ref_url = ?, source = ?, updated_at = ? WHERE id = ?`,
		t.Title, t.Ref, t.RefURL, t.Source, formatTS(now), id); err != nil {
		return api.Task{}, err
	}
	t.UpdatedAt = now
	return t, tx.Commit()
}
