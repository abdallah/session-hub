package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

const (
	taskA  = "t_AAAAAAAAAAA"
	taskB  = "t_BBBBBBBBBBB"
	eventA = "te_AAAAAAAAAAA"
)

// webTask sends a task write as the dashboard does.
func (e *env) webTask(method, path string, body any) (int, []byte) {
	e.t.Helper()
	code, b, _ := e.doHdr(method, path, "", e.web, map[string]string{api.HeaderAction: api.HeaderActionTasks}, body)
	return code, b
}

func TestTaskCreateActors(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1, CWD: "/home/user/proj"})
	e.register(e.tokB, api.SessionUpsert{ID: sid2, CWD: "/home/user/proj"})

	code, b := e.webTask("POST", "/v1/tasks", api.TaskIn{Title: "from web"})
	if code != http.StatusCreated {
		t.Fatalf("web create: %d %s", code, b)
	}
	var task api.Task
	e.must(http.StatusCreated, "POST", "/v1/tasks", e.tokA, api.TaskIn{Title: "agent", SessionID: sid1}, &task)
	if task.State != api.TaskProposed || task.CreatedBy != "session:"+sid1 {
		t.Errorf("agent create: %+v", task)
	}
	e.must(http.StatusCreated, "POST", "/v1/tasks", e.tokA, api.TaskIn{Title: "cli"}, &task)
	if task.State != api.TaskTodo || task.CreatedBy != "machine:tower" {
		t.Errorf("machine create: %+v", task)
	}
	// A session of another machine is refused.
	e.must(http.StatusConflict, "POST", "/v1/tasks", e.tokA, api.TaskIn{Title: "x", SessionID: sid2}, nil)
	// A repeated id returns the stored task with 200.
	e.must(http.StatusCreated, "POST", "/v1/tasks", e.tokA, api.TaskIn{ID: taskA, Title: "once"}, nil)
	e.must(http.StatusOK, "POST", "/v1/tasks", e.tokA, api.TaskIn{ID: taskA, Title: "once"}, &task)
	if task.ID != taskA {
		t.Errorf("repeat: %+v", task)
	}
	var open []api.Task
	e.must(http.StatusOK, "GET", "/v1/tasks", e.tokA, nil, &open)
	if len(open) != 4 {
		t.Errorf("open tasks: %d, want 4", len(open))
	}
	e.must(http.StatusBadRequest, "GET", "/v1/tasks?state=done", e.tokA, nil, nil)
	e.must(http.StatusOK, "GET", "/v1/tasks?state=open", e.tokA, nil, nil)
}

func TestTaskWebActor(t *testing.T) {
	e := newEnv(t)
	code, b := e.webTask("POST", "/v1/tasks", api.TaskIn{Title: "from web"})
	var task api.Task
	if code != http.StatusCreated || json.Unmarshal(b, &task) != nil {
		t.Fatalf("%d %s", code, b)
	}
	if task.CreatedBy != "web:phone" || task.State != api.TaskTodo {
		t.Errorf("web create: %+v", task)
	}
	// The cookie without the header is refused.
	if code, _, _ := e.doWith("POST", "/v1/tasks", "", e.web, api.TaskIn{Title: "x"}); code != http.StatusForbidden {
		t.Errorf("cookie without header: %d", code)
	}
}

func TestTaskAgentCannotFinish(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	e.must(http.StatusCreated, "POST", "/v1/tasks", e.tokA, api.TaskIn{ID: taskA, Title: "t"}, nil)
	e.must(http.StatusConflict, "POST", "/v1/tasks/"+taskA+"/state", e.tokA, api.TaskStateIn{To: api.TaskDone, SessionID: sid1}, nil)
	var task api.Task
	e.must(http.StatusOK, "POST", "/v1/tasks/"+taskA+"/state", e.tokA, api.TaskStateIn{To: api.TaskDoneProposed, SessionID: sid1, EventID: eventA}, &task)
	if task.State != api.TaskDoneProposed {
		t.Errorf("state %s", task.State)
	}
	// A repeated event id is 200 with the current task.
	e.must(http.StatusOK, "POST", "/v1/tasks/"+taskA+"/state", e.tokA, api.TaskStateIn{To: api.TaskDoneProposed, SessionID: sid1, EventID: eventA}, &task)
	e.must(http.StatusBadRequest, "POST", "/v1/tasks/bad/state", e.tokA, api.TaskStateIn{To: api.TaskDone}, nil)
	e.must(http.StatusNotFound, "POST", "/v1/tasks/"+taskB+"/state", e.tokA, api.TaskStateIn{To: api.TaskDone}, nil)
	e.must(http.StatusOK, "POST", "/v1/tasks/"+taskA+"/state", e.tokA, api.TaskStateIn{To: api.TaskDone}, &task)
}

func TestTaskLinkMergeEdit(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	e.register(e.tokB, api.SessionUpsert{ID: sid2})
	e.must(http.StatusCreated, "POST", "/v1/tasks", e.tokA, api.TaskIn{ID: taskA, Title: "a"}, nil)
	e.must(http.StatusCreated, "POST", "/v1/tasks", e.tokA, api.TaskIn{ID: taskB, Title: "b", SessionID: sid1}, nil)
	var task api.Task
	e.must(http.StatusOK, "POST", "/v1/tasks/"+taskA+"/sessions", e.tokA, api.TaskLinkIn{SessionID: sid1}, &task)
	if len(task.Sessions) != 1 || task.State != api.TaskInProgress {
		t.Errorf("link: %+v", task)
	}
	e.must(http.StatusConflict, "POST", "/v1/tasks/"+taskA+"/sessions", e.tokA, api.TaskLinkIn{SessionID: sid2, EventID: eventA}, nil)
	title := "renamed"
	e.must(http.StatusOK, "PATCH", "/v1/tasks/"+taskA, e.tokA, api.TaskEditIn{Title: &title}, &task)
	if task.Title != title {
		t.Errorf("edit: %+v", task)
	}
	e.must(http.StatusOK, "POST", "/v1/tasks/"+taskB+"/merge", e.tokA, api.TaskMergeIn{Into: taskA}, &task)
	if task.ID != taskA {
		t.Errorf("merge returns the target: %+v", task)
	}
	e.must(http.StatusConflict, "POST", "/v1/tasks/"+taskA+"/merge", e.tokA, api.TaskMergeIn{Into: taskB}, nil)
}

func TestTaskDayAndReview(t *testing.T) {
	e := newEnv(t)
	e.must(http.StatusOK, "GET", "/v1/tasks/day?date=2026-10-03&tz=Asia/Amman", e.tokA, nil, nil)
	e.must(http.StatusOK, "GET", "/v1/tasks/day?date=2026-10-03", e.tokA, nil, nil)
	e.must(http.StatusBadRequest, "GET", "/v1/tasks/day?date=2026-10-03&tz=Mars/Base", e.tokA, nil, nil)
	e.must(http.StatusBadRequest, "GET", "/v1/tasks/day?tz=Asia/Amman", e.tokA, nil, nil)
	e.must(http.StatusBadRequest, "GET", "/v1/tasks/day?date=nope", e.tokA, nil, nil)
	e.must(http.StatusOK, "GET", "/v1/tasks/review", e.tokA, nil, nil)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	e.must(http.StatusNoContent, "POST", "/v1/tasks/review/ignore", e.tokA, api.TaskLinkIn{SessionID: sid1}, nil)
	e.must(http.StatusNotFound, "POST", "/v1/tasks/review/ignore", e.tokA, api.TaskLinkIn{SessionID: sid2}, nil)
}
