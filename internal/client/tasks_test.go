package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	hubserver "github.com/abdallah/session-hub/internal/server"
	"github.com/abdallah/session-hub/internal/store"
)

func TestTaskMethods(t *testing.T) {
	s := "new"
	tests := []struct {
		name       string
		call       func(c *Client) error
		wantMethod string
		wantURI    string
		wantBody   string
	}{
		{"OpenTasks", func(c *Client) error { _, err := c.OpenTasks(context.Background()); return err },
			"GET", "/v1/tasks?state=open", ""},
		{"CreateTask", func(c *Client) error {
			_, err := c.CreateTask(context.Background(), api.TaskIn{ID: "t_x", Title: "T"})
			return err
		},
			"POST", "/v1/tasks", `{"id":"t_x","title":"T"}`},
		{"SetTaskState", func(c *Client) error {
			_, err := c.SetTaskState(context.Background(), "t_x", api.TaskStateIn{To: "done", EventID: "te_1"})
			return err
		}, "POST", "/v1/tasks/t_x/state", `{"to":"done","event_id":"te_1"}`},
		{"LinkTask", func(c *Client) error {
			_, err := c.LinkTask(context.Background(), "t_x", api.TaskLinkIn{SessionID: "s1"})
			return err
		}, "POST", "/v1/tasks/t_x/sessions", `{"session_id":"s1"}`},
		{"MergeTask", func(c *Client) error { _, err := c.MergeTask(context.Background(), "t_x", "t_y"); return err },
			"POST", "/v1/tasks/t_x/merge", `{"into":"t_y"}`},
		{"EditTask", func(c *Client) error {
			_, err := c.EditTask(context.Background(), "t_x", api.TaskEditIn{Source: &s})
			return err
		},
			"PATCH", "/v1/tasks/t_x", `{"source":"new"}`},
		{"TaskDay", func(c *Client) error {
			_, err := c.TaskDay(context.Background(), "2026-10-07", "Asia/Amman")
			return err
		},
			"GET", "/v1/tasks/day?date=2026-10-07&tz=Asia%2FAmman", ""},
		{"TaskReview", func(c *Client) error { _, err := c.TaskReview(context.Background()); return err },
			"GET", "/v1/tasks/review", ""},
		{"IgnoreUntasked", func(c *Client) error { return c.IgnoreUntasked(context.Background(), "s1") },
			"POST", "/v1/tasks/review/ignore", `{"session_id":"s1"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var method, uri, body, auth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				method, uri, body, auth = r.Method, r.URL.RequestURI(), string(b), r.Header.Get("Authorization")
				if tc.name == "IgnoreUntasked" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if tc.name == "OpenTasks" {
					w.Write([]byte(`[]`))
					return
				}
				w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			c, _ := New(Config{ServerURL: srv.URL, Token: "tok"})
			if err := tc.call(c); err != nil {
				t.Fatal(err)
			}
			if method != tc.wantMethod || uri != tc.wantURI || body != tc.wantBody || auth != "Bearer tok" {
				t.Errorf("got %s %s %q auth=%q", method, uri, body, auth)
			}
		})
	}
}

func TestNewTaskIDs(t *testing.T) {
	task := regexp.MustCompile(`^t_[A-Za-z0-9_-]{11}$`)
	event := regexp.MustCompile(`^te_[A-Za-z0-9_-]{11}$`)
	a, b := NewTaskID(), NewTaskID()
	if !task.MatchString(a) || a == b {
		t.Errorf("NewTaskID = %q, %q", a, b)
	}
	if e := NewTaskEventID(); !event.MatchString(e) {
		t.Errorf("NewTaskEventID = %q", e)
	}
}

func TestReplayTaskOps(t *testing.T) {
	tests := []struct {
		name       string
		item       Item
		wantMethod string
		wantPath   string
		wantBody   string
	}{
		{"create", Item{Op: OpTaskCreate, SessionID: "s1", Body: []byte(`{"id":"t_abcdefghijk","title":"T"}`)},
			"POST", "/v1/tasks", `{"id":"t_abcdefghijk","title":"T","session_id":"s1"}`},
		{"state", Item{Op: OpTaskState, SessionID: "s1", TaskID: "t_x", Body: []byte(`{"to":"done_proposed","event_id":"te_1"}`)},
			"POST", "/v1/tasks/t_x/state", `{"to":"done_proposed","session_id":"s1","event_id":"te_1"}`},
		{"link", Item{Op: OpTaskLink, SessionID: "s1", TaskID: "t_x", Body: []byte(`{"session_id":"s1","event_id":"te_1"}`)},
			"POST", "/v1/tasks/t_x/sessions", `{"session_id":"s1","event_id":"te_1"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var method, path, body string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				method, path, body = r.Method, r.URL.Path, string(b)
				w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			c, _ := New(Config{ServerURL: srv.URL, Token: "tok"})
			if err := Replay(context.Background(), c, tc.item); err != nil {
				t.Fatal(err)
			}
			if method != tc.wantMethod || path != tc.wantPath || body != tc.wantBody {
				t.Errorf("got %s %s %s", method, path, body)
			}
		})
	}
}

func TestReplayTaskOpsNeedIDs(t *testing.T) {
	c, _ := New(Config{ServerURL: "http://127.0.0.1:1", Token: "tok"})
	for _, it := range []Item{
		{Op: OpTaskCreate, Body: []byte(`{"title":"T"}`)},
		{Op: OpTaskState, SessionID: "s1", Body: []byte(`{"to":"done_proposed"}`)},
		{Op: OpTaskLink, SessionID: "s1", Body: []byte(`{"session_id":"s1"}`)},
		{Op: OpTaskState, SessionID: "s1", TaskID: "t_x", Body: []byte(`{`)},
	} {
		if err := Replay(context.Background(), c, it); err == nil || IsRetryable(err) {
			t.Errorf("%s: err = %v, want a non-retryable error", it.Op, err)
		}
	}
}

// realHub is the real server handler over a real store. internal/server does
// not import this package, so the test can use both.
func realHub(t *testing.T) (*Client, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sessionhub.db"), store.Options{StaleAfter: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tok, _, err := st.AddMachine(context.Background(), "tower", "tower.example.com", "tower.example.com")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(hubserver.New(st, "https://sessionhub.example.test", log.New(io.Discard, "", 0)).Handler())
	t.Cleanup(srv.Close)
	c, err := New(Config{ServerURL: srv.URL, Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.UpsertSession(context.Background(), api.SessionUpsert{ID: "s1", Agent: "claude", Source: "hooks"}); err != nil {
		t.Fatal(err)
	}
	return c, st
}

func jsonBody(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Review Focus 1: a queued task_create replayed after a timeout that actually
// succeeded creates one task.
func TestReplayTaskCreateTwiceMakesOneTask(t *testing.T) {
	c, _ := realHub(t)
	ctx := context.Background()
	it := Item{Op: OpTaskCreate, SessionID: "s1", Body: jsonBody(t, api.TaskIn{ID: NewTaskID(), Title: "Fix login"})}
	for i := 0; i < 2; i++ {
		if err := Replay(ctx, c, it); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}
	open, err := c.OpenTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].State != api.TaskProposed {
		t.Errorf("tasks = %+v, want one proposed task", open)
	}
}

// A done_proposed item replayed after you rejected it must not reopen the
// proposal.
func TestReplayTaskStateAfterRejectIsNoop(t *testing.T) {
	c, _ := realHub(t)
	ctx := context.Background()
	task, err := c.CreateTask(ctx, api.TaskIn{Title: "Fix login", State: api.TaskTodo})
	if err != nil {
		t.Fatal(err)
	}
	if err := Replay(ctx, c, Item{Op: OpTaskLink, SessionID: "s1", TaskID: task.ID,
		Body: jsonBody(t, api.TaskLinkIn{SessionID: "s1", EventID: NewTaskEventID()})}); err != nil {
		t.Fatal(err)
	}
	it := Item{Op: OpTaskState, SessionID: "s1", TaskID: task.ID,
		Body: jsonBody(t, api.TaskStateIn{To: api.TaskDoneProposed, EventID: NewTaskEventID()})}
	if err := Replay(ctx, c, it); err != nil {
		t.Fatal(err)
	}
	if got, err := c.SetTaskState(ctx, task.ID, api.TaskStateIn{To: api.TaskInProgress}); err != nil || got.State != api.TaskInProgress {
		t.Fatalf("reject: %+v, %v", got, err)
	}
	if err := Replay(ctx, c, it); err != nil {
		t.Fatalf("replay after reject: %v", err)
	}
	open, _ := c.OpenTasks(ctx)
	if len(open) != 1 || open[0].State != api.TaskInProgress {
		t.Errorf("tasks = %+v, want one in_progress task", open)
	}
}

// A bad transition answers 409; the queue drops it instead of retrying.
func TestReplayTaskConflictIsNotRetryable(t *testing.T) {
	c, _ := realHub(t)
	ctx := context.Background()
	task, err := c.CreateTask(ctx, api.TaskIn{Title: "Fix login", State: api.TaskTodo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetTaskState(ctx, task.ID, api.TaskStateIn{To: api.TaskDone}); err != nil {
		t.Fatal(err)
	}
	err = Replay(ctx, c, Item{Op: OpTaskState, SessionID: "s1", TaskID: task.ID,
		Body: jsonBody(t, api.TaskStateIn{To: api.TaskDoneProposed, EventID: NewTaskEventID()})})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusConflict {
		t.Fatalf("err = %v, want 409", err)
	}
	if IsRetryable(err) {
		t.Error("409 is retryable")
	}
}
