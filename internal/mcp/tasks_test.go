package mcp

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	hubserver "github.com/abdallah/session-hub/internal/server"
	"github.com/abdallah/session-hub/internal/store"
)

// taskHub is a small in-memory stand-in for the task routes.
type taskHub struct {
	*httptest.Server
	mu       sync.Mutex
	tasks    map[string]*api.Task
	order    []string
	bodies   []string // "METHOD path body"
	conflict string   // task id whose state change answers 409
}

func newTaskHub(t *testing.T) *taskHub {
	h := &taskHub{tasks: map[string]*api.Task{}}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Close)
	return h
}

func (h *taskHub) seed(tk api.Task) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tasks[tk.ID] = &tk
	h.order = append(h.order, tk.ID)
}

func (h *taskHub) get(id string) (api.Task, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if tk, ok := h.tasks[id]; ok {
		return *tk, true
	}
	return api.Task{}, false
}

func (h *taskHub) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.bodies...)
}

func (h *taskHub) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bodies = append(h.bodies, r.Method+" "+r.URL.Path+" "+string(b))
	w.Header().Set("Content-Type", "application/json")
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // v1 tasks <id> <rest>
	switch {
	case r.URL.Path == "/v1/sessions":
		io.WriteString(w, `{}`)
	case r.Method == "GET" && r.URL.Path == "/v1/tasks":
		out := []api.Task{}
		for _, id := range h.order {
			out = append(out, *h.tasks[id])
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == "POST" && r.URL.Path == "/v1/tasks":
		var in api.TaskIn
		json.Unmarshal(b, &in)
		for _, id := range h.order {
			tk := h.tasks[id]
			if tk.ID == in.ID || (in.Ref != "" && strings.EqualFold(tk.Ref, in.Ref)) {
				json.NewEncoder(w).Encode(tk)
				return
			}
		}
		tk := &api.Task{ID: in.ID, Title: in.Title, Ref: in.Ref, RefURL: in.RefURL, Source: in.Source, State: "proposed", CreatedAt: time.Now()}
		h.tasks[tk.ID] = tk
		h.order = append(h.order, tk.ID)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(tk)
	case r.Method == "POST" && len(parts) == 4 && parts[1] == "tasks":
		tk, ok := h.tasks[parts[2]]
		if !ok {
			fail(404, "not found: task "+parts[2])
			return
		}
		switch parts[3] {
		case "state":
			var in api.TaskStateIn
			json.Unmarshal(b, &in)
			if h.conflict == tk.ID {
				fail(409, "conflict: cannot move a dropped task to done_proposed")
				return
			}
			tk.State = in.To
		case "sessions":
			if tk.State == "todo" {
				tk.State = "in_progress"
			}
		}
		json.NewEncoder(w).Encode(tk)
	default:
		fail(404, "no route")
	}
}

func lastBody(t *testing.T, h *taskHub, prefix string, v any) {
	t.Helper()
	seen := h.seen()
	for i := len(seen) - 1; i >= 0; i-- {
		if strings.HasPrefix(seen[i], prefix) {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(seen[i], prefix)), v); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("no request with prefix %q in %q", prefix, seen)
}

func queued(t *testing.T, q *client.Queue) []client.Item {
	t.Helper()
	var items []client.Item
	q.Drain(func(in []client.Item) []client.Item { items = in; return in })
	return items
}

func TestTaskToolsListed(t *testing.T) {
	s, _, _ := newTestServer(t, "http://127.0.0.1:1", 1)
	got := transcript(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := decode(t, got[0])["result"].(map[string]any)["tools"].([]any)
	desc := map[string]string{}
	for _, x := range tools {
		m := x.(map[string]any)
		desc[m["name"].(string)], _ = m["description"].(string)
	}
	want := map[string]string{
		"list_tasks":   "List the open human-level tasks (proposed, todo, in progress, awaiting done). Call it before propose_task so you link to an existing task instead of creating a duplicate.",
		"propose_task": "Propose a human-level task for this session's goal: a ticket, an email or chat request, a bug you were asked to fix. Write the title the way a person would name it in a standup, not an implementation step. The user accepts or rejects it.",
		"link_task":    "Link this session to an existing task from list_tasks. A todo task moves to in progress.",
		"propose_done": "Tell the user this task's goal is finished. The user confirms it. Do not call it for steps inside a task.",
	}
	for name, d := range want {
		if desc[name] != d {
			t.Errorf("%s description = %q", name, desc[name])
		}
	}
}

func TestInitializeInstructionsMentionTasks(t *testing.T) {
	s, _, _ := newTestServer(t, "http://127.0.0.1:1", 1)
	got := transcript(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	ins, _ := decode(t, got[0])["result"].(map[string]any)["instructions"].(string)
	if !strings.Contains(ins, "list_tasks") || !strings.Contains(ins, "propose_done") {
		t.Errorf("instructions = %q", ins)
	}
}

func TestListTasks(t *testing.T) {
	h := newTaskHub(t)
	h.seed(api.Task{ID: "t_a", Title: "Fix DNS", Ref: "ops-1", State: "todo"})
	h.seed(api.Task{ID: "t_b", Title: "Reply\nto a teammate", State: "proposed"})
	s, _, _ := newTestServer(t, h.URL, 1)
	msg, isErr := callTool(t, s, "list_tasks", `{}`)
	want := "t_a [todo] Fix DNS (ops-1)\nt_b [proposed] Reply to a teammate"
	if isErr || msg != want {
		t.Errorf("list_tasks = %q %v", msg, isErr)
	}
}

func TestListTasksEmptyAndDown(t *testing.T) {
	h := newTaskHub(t)
	s, _, _ := newTestServer(t, h.URL, 1)
	if msg, isErr := callTool(t, s, "list_tasks", `{}`); isErr || !strings.Contains(msg, "no open tasks") {
		t.Errorf("empty = %q %v", msg, isErr)
	}
	s, _, _ = newTestServer(t, "http://127.0.0.1:1", 1)
	msg, isErr := callTool(t, s, "list_tasks", `{}`)
	if isErr || msg != "sessionhub: could not reach the server to list tasks. Continue with your work." {
		t.Errorf("down = %q %v", msg, isErr)
	}
}

func TestProposeTaskSendsSessionAndID(t *testing.T) {
	h := newTaskHub(t)
	s, q, state := newTestServer(t, h.URL, 42)
	writeCurrent(t, state, 42, sid1)
	msg, isErr := callTool(t, s, "propose_task", `{"title":" Fix DNS ","source":"ticket","ref":"ops-1","ref_url":"https://x/y"}`)
	var in api.TaskIn
	lastBody(t, h, "POST /v1/tasks ", &in)
	if isErr || !strings.Contains(msg, in.ID) {
		t.Errorf("reply = %q %v", msg, isErr)
	}
	if !strings.HasPrefix(in.ID, "t_") || in.SessionID != sid1 || in.Title != "Fix DNS" || in.Source != "ticket" || in.Ref != "ops-1" || in.RefURL != "https://x/y" {
		t.Errorf("body = %+v", in)
	}
	if n, _ := q.Len(); n != 0 {
		t.Errorf("queue has %d items", n)
	}
}

func TestProposeTaskReportsExisting(t *testing.T) {
	h := newTaskHub(t)
	h.seed(api.Task{ID: "t_old", Title: "Fix DNS", Ref: "OPS-1", State: "todo"})
	s, _, state := newTestServer(t, h.URL, 42)
	writeCurrent(t, state, 42, sid1)
	msg, isErr := callTool(t, s, "propose_task", `{"title":"DNS","source":"ticket","ref":"ops-1"}`)
	if isErr || !strings.Contains(msg, "already") || !strings.Contains(msg, "t_old") || !strings.Contains(msg, "linked") {
		t.Errorf("reply = %q", msg)
	}
}

func TestProposeTaskValidationAndNoSession(t *testing.T) {
	h := newTaskHub(t)
	s, _, state := newTestServer(t, h.URL, 42)
	if msg, isErr := callTool(t, s, "propose_task", `{"title":"x","source":"chat"}`); isErr || msg != noSession {
		t.Errorf("no session = %q %v", msg, isErr)
	}
	writeCurrent(t, state, 42, sid1)
	if _, isErr := callTool(t, s, "propose_task", `{"title":"  ","source":"chat"}`); !isErr {
		t.Error("empty title: want isError")
	}
	for _, args := range []string{`{"title":"x"}`, `{"title":"x","source":" "}`, `{"title":"x","source":"slack"}`} {
		if msg, isErr := callTool(t, s, "propose_task", args); !isErr || !strings.Contains(msg, "source") {
			t.Errorf("%s = %q %v, want a source error", args, msg, isErr)
		}
	}
	if _, isErr := callTool(t, s, "link_task", `{}`); !isErr {
		t.Error("link without task_id: want isError")
	}
	if _, isErr := callTool(t, s, "propose_done", `{"task_id":" "}`); !isErr {
		t.Error("done without task_id: want isError")
	}
	if n := len(h.seen()); n != 0 {
		t.Errorf("server got %d requests", n)
	}
}

func TestProposeTaskQueuedWhenDown(t *testing.T) {
	s, q, state := newTestServer(t, "http://127.0.0.1:1", 42)
	writeCurrent(t, state, 42, sid1)
	msg, isErr := callTool(t, s, "propose_task", `{"title":"Fix DNS","source":"email"}`)
	items := queued(t, q)
	if isErr || len(items) != 1 || items[0].Op != client.OpTaskCreate || items[0].SessionID != sid1 {
		t.Fatalf("items = %+v msg=%q", items, msg)
	}
	var in api.TaskIn
	json.Unmarshal(items[0].Body, &in)
	if in.ID == "" || in.SessionID != sid1 || !strings.Contains(msg, in.ID) || !strings.Contains(msg, "queued") {
		t.Errorf("body = %+v msg=%q", in, msg)
	}
}

func TestLinkTaskSendsEventID(t *testing.T) {
	h := newTaskHub(t)
	h.seed(api.Task{ID: "t_a", Title: "Fix DNS", State: "todo"})
	s, _, state := newTestServer(t, h.URL, 42)
	writeCurrent(t, state, 42, sid1)
	msg, isErr := callTool(t, s, "link_task", `{"task_id":"t_a"}`)
	var in api.TaskLinkIn
	lastBody(t, h, "POST /v1/tasks/t_a/sessions ", &in)
	if isErr || !strings.Contains(msg, "recorded") || in.SessionID != sid1 || !strings.HasPrefix(in.EventID, "te_") {
		t.Errorf("msg=%q body=%+v", msg, in)
	}
	if tk, _ := h.get("t_a"); tk.State != "in_progress" {
		t.Errorf("state = %s", tk.State)
	}
}

func TestProposeDoneSendsEventID(t *testing.T) {
	h := newTaskHub(t)
	h.seed(api.Task{ID: "t_a", Title: "Fix DNS", State: "in_progress"})
	s, _, state := newTestServer(t, h.URL, 42)
	writeCurrent(t, state, 42, sid1)
	msg, isErr := callTool(t, s, "propose_done", `{"task_id":"t_a","note":"merged"}`)
	var in api.TaskStateIn
	lastBody(t, h, "POST /v1/tasks/t_a/state ", &in)
	if isErr || !strings.Contains(msg, "recorded") || in.To != "done_proposed" || in.Note != "merged" || in.SessionID != sid1 || !strings.HasPrefix(in.EventID, "te_") {
		t.Errorf("msg=%q body=%+v", msg, in)
	}
}

func TestProposeDoneConflictNotQueued(t *testing.T) {
	h := newTaskHub(t)
	h.seed(api.Task{ID: "t_a", State: "dropped"})
	h.conflict = "t_a"
	s, q, state := newTestServer(t, h.URL, 42)
	writeCurrent(t, state, 42, sid1)
	msg, isErr := callTool(t, s, "propose_done", `{"task_id":"t_a"}`)
	if isErr || !strings.Contains(msg, "rejected") || !strings.Contains(msg, "dropped task") {
		t.Errorf("msg = %q %v", msg, isErr)
	}
	if n, _ := q.Len(); n != 0 {
		t.Errorf("queue has %d items", n)
	}
}

func TestQueuedTaskItemsCarryProducerIDs(t *testing.T) {
	s, q, state := newTestServer(t, "http://127.0.0.1:1", 42)
	writeCurrent(t, state, 42, sid1)
	callTool(t, s, "propose_task", `{"title":"A","source":"chat"}`)
	callTool(t, s, "link_task", `{"task_id":"t_a"}`)
	callTool(t, s, "propose_done", `{"task_id":"t_a"}`)
	items := queued(t, q)
	if len(items) != 3 {
		t.Fatalf("items = %+v", items)
	}
	var c api.TaskIn
	var l api.TaskLinkIn
	var d api.TaskStateIn
	json.Unmarshal(items[0].Body, &c)
	json.Unmarshal(items[1].Body, &l)
	json.Unmarshal(items[2].Body, &d)
	if !strings.HasPrefix(c.ID, "t_") || !strings.HasPrefix(l.EventID, "te_") || !strings.HasPrefix(d.EventID, "te_") || l.EventID == d.EventID {
		t.Errorf("ids: %+v %+v %+v", c, l, d)
	}
	if items[1].Op != client.OpTaskLink || items[1].TaskID != "t_a" || items[2].Op != client.OpTaskState || items[2].TaskID != "t_a" {
		t.Errorf("items = %+v", items)
	}
}

func TestProposeDoneOnTaskNotYetOnServerThenDrain(t *testing.T) {
	// propose_task while offline.
	down, q, state := newTestServer(t, "http://127.0.0.1:1", 42)
	writeCurrent(t, state, 42, sid1)
	callTool(t, down, "propose_task", `{"title":"Fix DNS","source":"ticket"}`)
	var created api.TaskIn
	json.Unmarshal(queued(t, q)[0].Body, &created)

	// propose_done while online: the server does not know the task yet.
	h := newTaskHub(t)
	up, _, upState := newTestServer(t, h.URL, 42)
	writeCurrent(t, upState, 42, sid1)
	up.Queue = q
	msg, isErr := callTool(t, up, "propose_done", `{"task_id":"`+created.ID+`"}`)
	if isErr || msg != "sessionhub: task `"+created.ID+"` is not on the server yet; queued." {
		t.Fatalf("msg = %q %v", msg, isErr)
	}

	// Drain in order.
	c, err := client.New(client.Config{ServerURL: h.URL, Token: "hub_m_test"})
	if err != nil {
		t.Fatal(err)
	}
	err = q.Drain(func(items []client.Item) []client.Item {
		for _, it := range items {
			if err := client.Replay(t.Context(), c, it); err != nil {
				t.Fatalf("replay %s: %v", it.Op, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if tk, ok := h.get(created.ID); !ok || tk.State != "done_proposed" {
		t.Errorf("task = %+v ok=%v", tk, ok)
	}
}

// An offline propose_task that the server folds into an open task with the
// same ref keeps the agent's id usable: a later propose_done on that id
// reaches the open task.
func TestProposeDoneOnFoldedTaskThenDrain(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "sessionhub.db"), store.Options{StaleAfter: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := t.Context()
	tok, _, err := st.AddMachine(ctx, "tower", "tower.example.com", "tower.example.com")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(hubserver.New(st, "https://sessionhub.example.test", log.New(io.Discard, "", 0)).Handler())
	t.Cleanup(srv.Close)
	c, err := client.New(client.Config{ServerURL: srv.URL, Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.UpsertSession(ctx, api.SessionUpsert{ID: sid1, Agent: "claude", Source: "hooks"}); err != nil {
		t.Fatal(err)
	}
	open, err := c.CreateTask(ctx, api.TaskIn{Title: "Fix DNS", Ref: "ops-1234", State: api.TaskInProgress})
	if err != nil {
		t.Fatal(err)
	}

	// propose_task while offline, with the same ref.
	down, q, state := newTestServer(t, "http://127.0.0.1:1", 42)
	writeCurrent(t, state, 42, sid1)
	callTool(t, down, "propose_task", `{"title":"DNS","source":"ticket","ref":"OPS-1234"}`)
	var held api.TaskIn
	json.Unmarshal(queued(t, q)[0].Body, &held)

	drain := func() {
		t.Helper()
		err := q.Drain(func(items []client.Item) []client.Item {
			for _, it := range items {
				if err := client.Replay(ctx, c, it); err != nil {
					t.Fatalf("replay %s: %v", it.Op, err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// The server folds the proposal into the open task.
	drain()

	// propose_done online on the id the agent holds.
	up := &Server{
		API:      func() (hubAPI, error) { return c, nil },
		Queue:    q,
		StateDir: state,
		PPID:     func() int { return 42 },
	}
	if msg, isErr := callTool(t, up, "propose_done", `{"task_id":"`+held.ID+`","note":"fixed"}`); isErr {
		t.Fatalf("propose_done = %q", msg)
	}
	drain()

	got, err := st.GetTask(ctx, open.ID)
	if err != nil || got.State != api.TaskDoneProposed {
		t.Errorf("open task = %+v %v, want done_proposed", got, err)
	}
	if x, err := st.GetTask(ctx, held.ID); err != nil || x.State != api.TaskDropped || x.MergedInto != open.ID {
		t.Errorf("held id = %+v %v, want dropped and merged into %s", x, err, open.ID)
	}
}
