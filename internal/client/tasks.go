package client

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"

	"github.com/abdallah/session-hub/internal/api"
)

func randomID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:])
}

// NewTaskID returns a task id: "t_" and 8 random bytes, base64url. Supply it
// in api.TaskIn so a retried create is a no-op.
func NewTaskID() string { return randomID("t_") }

// NewTaskEventID returns a task event id: "te_" and 8 random bytes,
// base64url. Supply it in a state change or link so a retry is a no-op.
func NewTaskEventID() string { return randomID("te_") }

func taskPath(id string, rest string) string {
	return "/v1/tasks/" + url.PathEscape(id) + rest
}

// OpenTasks lists tasks that are not done or dropped.
func (c *Client) OpenTasks(ctx context.Context) ([]api.Task, error) {
	var out []api.Task
	if err := c.do(ctx, http.MethodGet, "/v1/tasks?state=open", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateTask creates a task; a repeat with the same id returns the existing one.
func (c *Client) CreateTask(ctx context.Context, in api.TaskIn) (api.Task, error) {
	var t api.Task
	err := c.do(ctx, http.MethodPost, "/v1/tasks", in, &t)
	return t, err
}

// AddNote sends a worklog note, which the server routes to a task.
func (c *Client) AddNote(ctx context.Context, in api.NoteIn) (api.NoteResult, error) {
	var r api.NoteResult
	err := c.do(ctx, http.MethodPost, "/v1/notes", in, &r)
	return r, err
}

// SetTaskState changes a task's state.
func (c *Client) SetTaskState(ctx context.Context, id string, in api.TaskStateIn) (api.Task, error) {
	var t api.Task
	err := c.do(ctx, http.MethodPost, taskPath(id, "/state"), in, &t)
	return t, err
}

// LinkTask links a session to a task.
func (c *Client) LinkTask(ctx context.Context, id string, in api.TaskLinkIn) (api.Task, error) {
	var t api.Task
	err := c.do(ctx, http.MethodPost, taskPath(id, "/sessions"), in, &t)
	return t, err
}

// MergeTask merges proposal id into task into and returns the target.
func (c *Client) MergeTask(ctx context.Context, id, into string) (api.Task, error) {
	var t api.Task
	err := c.do(ctx, http.MethodPost, taskPath(id, "/merge"), api.TaskMergeIn{Into: into}, &t)
	return t, err
}

// EditTask edits a task's fields; a nil field is left alone.
func (c *Client) EditTask(ctx context.Context, id string, in api.TaskEditIn) (api.Task, error) {
	var t api.Task
	err := c.do(ctx, http.MethodPatch, taskPath(id, ""), in, &t)
	return t, err
}

// TaskDay returns a day's tasks in the zone tz (an IANA name; "" is UTC).
func (c *Client) TaskDay(ctx context.Context, date, tz string) (api.TaskDay, error) {
	q := url.Values{"date": {date}}
	if tz != "" {
		q.Set("tz", tz)
	}
	var d api.TaskDay
	err := c.do(ctx, http.MethodGet, "/v1/tasks/day?"+q.Encode(), nil, &d)
	return d, err
}

// TaskReview returns what waits for review.
func (c *Client) TaskReview(ctx context.Context) (api.TaskReview, error) {
	var r api.TaskReview
	err := c.do(ctx, http.MethodGet, "/v1/tasks/review", nil, &r)
	return r, err
}

// IgnoreUntasked hides a session from the untasked list.
func (c *Client) IgnoreUntasked(ctx context.Context, sessionID string) error {
	return c.do(ctx, http.MethodPost, "/v1/tasks/review/ignore", api.TaskLinkIn{SessionID: sessionID}, nil)
}
