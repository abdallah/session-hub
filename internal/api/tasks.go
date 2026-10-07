package api

import "time"

// Task states. See docs/server.md, "Tasks".
const (
	TaskProposed     = "proposed"
	TaskTodo         = "todo"
	TaskInProgress   = "in_progress"
	TaskDoneProposed = "done_proposed"
	TaskDone         = "done"
	TaskDropped      = "dropped"
)

// HeaderActionTasks is the X-Hub-Action value the dashboard sends with task
// writes.
const HeaderActionTasks = "tasks"

// Task is a unit of work the user tracks across sessions.
type Task struct {
	ID         string        `json:"id"`
	Title      string        `json:"title"`
	Ref        string        `json:"ref"`
	RefURL     string        `json:"ref_url"`
	Source     string        `json:"source"` // ticket|email|chat|other
	State      string        `json:"state"`
	MergedInto string        `json:"merged_into,omitempty"`
	CreatedBy  string        `json:"created_by"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
	Sessions   []TaskSession `json:"sessions"`
	// ActiveSeconds is the working time of its sessions on the day; the
	// day view fills it.
	ActiveSeconds int64 `json:"active_seconds"`
}

// TaskSession is a session linked to a task.
type TaskSession struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Machine string   `json:"machine"`
	Done    []string `json:"done,omitempty"` // filled by the day view only
	// ActiveSeconds is the session's working time for this task on the
	// day; the day view fills it.
	ActiveSeconds int64 `json:"active_seconds"`
}

// TaskIn is the body of a task create. ID is optional; a client that
// supplies one can retry safely. State "" means proposed for an agent and
// todo for you.
type TaskIn struct {
	ID        string `json:"id,omitempty"`
	Title     string `json:"title"`
	Ref       string `json:"ref,omitempty"`
	RefURL    string `json:"ref_url,omitempty"`
	Source    string `json:"source,omitempty"`
	State     string `json:"state,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// TaskStateIn is the body of a task state change. EventID makes a retry a
// no-op.
type TaskStateIn struct {
	To        string `json:"to"`
	Note      string `json:"note,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	EventID   string `json:"event_id,omitempty"`
}

// TaskMergeIn is the body of a task merge.
type TaskMergeIn struct {
	Into string `json:"into"`
}

// TaskLinkIn is the body of a session link. EventID makes a retry a no-op.
type TaskLinkIn struct {
	SessionID string `json:"session_id"`
	EventID   string `json:"event_id,omitempty"`
}

// MaxTaskIgnoreBatch caps the session ids in one ignore request.
const MaxTaskIgnoreBatch = 500

// TaskIgnoreIn is the body of POST /v1/tasks/review/ignore: one session in
// SessionID, or a batch in SessionIDs, never both.
type TaskIgnoreIn struct {
	SessionID  string   `json:"session_id,omitempty"`
	SessionIDs []string `json:"session_ids,omitempty"`
}

// TaskEditIn is the body of a task edit; a nil field is left alone.
type TaskEditIn struct {
	Title  *string `json:"title,omitempty"`
	Ref    *string `json:"ref,omitempty"`
	RefURL *string `json:"ref_url,omitempty"`
	Source *string `json:"source,omitempty"`
}

// TaskDay is a day's tasks in three columns, rebuilt from task events.
type TaskDay struct {
	Date       string `json:"date"` // YYYY-MM-DD
	TZ         string `json:"tz"`
	Todo       []Task `json:"todo"`
	InProgress []Task `json:"in_progress"`
	Done       []Task `json:"done"`
}

// TaskReview is what waits for you: proposals, done proposals, and recent
// sessions with a title and no task.
type TaskReview struct {
	Proposed     []Task    `json:"proposed"`
	DoneProposed []Task    `json:"done_proposed"`
	Untasked     []Session `json:"untasked"`
}

// NoteIn is the body of POST /v1/notes: a worklog note. ID is a te_ task
// event ID that makes a retry a no-op. SessionID is the session that wrote
// it, if any.
type NoteIn struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	SessionID string `json:"session_id,omitempty"`
}

// NoteResult is the answer to a note: the task it went to and what it did.
type NoteResult struct {
	Task   Task   `json:"task"`
	Action string `json:"action"` // created|joined|done
}

// NoteResult actions.
const (
	NoteCreated = "created"
	NoteJoined  = "joined"
	NoteDone    = "done"
)
