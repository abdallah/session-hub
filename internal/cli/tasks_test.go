package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

type fakeTasks struct {
	open    []api.Task
	day     api.TaskDay
	review  api.TaskReview
	err     error
	created []api.TaskIn
	states  []string // "id to note"
	merges  []string // "id into"
	dayArgs []string // "date tz"
}

func (f *fakeTasks) OpenTasks(context.Context) ([]api.Task, error) { return f.open, f.err }

func (f *fakeTasks) CreateTask(_ context.Context, in api.TaskIn) (api.Task, error) {
	f.created = append(f.created, in)
	return api.Task{ID: "t_newtask12345", Title: in.Title, State: api.TaskTodo}, f.err
}

func (f *fakeTasks) SetTaskState(_ context.Context, id string, in api.TaskStateIn) (api.Task, error) {
	f.states = append(f.states, id+" "+in.To+" "+in.Note)
	title := "older task"
	for _, t := range f.open {
		if t.ID == id {
			title = t.Title
		}
	}
	return api.Task{ID: id, Title: title, State: in.To}, f.err
}

func (f *fakeTasks) MergeTask(_ context.Context, id, into string) (api.Task, error) {
	f.merges = append(f.merges, id+" "+into)
	return api.Task{ID: into, Title: "target"}, f.err
}

func (f *fakeTasks) TaskDay(_ context.Context, date, tz string) (api.TaskDay, error) {
	f.dayArgs = append(f.dayArgs, date+" "+tz)
	return f.day, f.err
}

func (f *fakeTasks) TaskReview(context.Context) (api.TaskReview, error) { return f.review, f.err }

func runTask(t *testing.T, f *fakeTasks, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := &env{tasks: f, out: &out, now: func() time.Time { return now }, width: func() int { return 200 }}
	err := e.taskCmd(context.Background(), args)
	return out.String(), err
}

func taskFixture() *fakeTasks {
	return &fakeTasks{open: []api.Task{
		{ID: "t_aaaaaaaa1111", Title: "proposal one", State: api.TaskProposed},
		{ID: "t_aaaabbbb2222", Title: "proposal two", State: api.TaskProposed},
		{ID: "t_cccccccc3333", Title: "needs confirm", State: api.TaskDoneProposed},
		{ID: "t_dddddddd4444", Title: "working", State: api.TaskInProgress},
		{ID: "t_eeeeeeee5555", Title: "queued", State: api.TaskTodo},
	}}
}

func TestTaskAdd(t *testing.T) {
	f := &fakeTasks{}
	out, err := runTask(t, f, "add", "fix login", "--now", "--ref", "SYS-1", "--url", "https://x/1", "--source", "email")
	if err != nil {
		t.Fatal(err)
	}
	want := api.TaskIn{Title: "fix login", Ref: "SYS-1", RefURL: "https://x/1", Source: "email", State: api.TaskInProgress}
	if len(f.created) != 1 || f.created[0] != want {
		t.Fatalf("created = %+v, want %+v", f.created, want)
	}
	if !strings.Contains(out, "newtask1") || !strings.Contains(out, "fix login") {
		t.Errorf("out = %q", out)
	}
	f = &fakeTasks{}
	if _, err := runTask(t, f, "add", "plain"); err != nil {
		t.Fatal(err)
	}
	if f.created[0].State != "" {
		t.Errorf("state = %q, want empty", f.created[0].State)
	}
	if _, err := runTask(t, f, "add"); err == nil {
		t.Error("add without a title succeeded")
	}
}

func TestTaskLs(t *testing.T) {
	f := &fakeTasks{day: api.TaskDay{Date: "2026-10-03",
		Todo:       []api.Task{{ID: "t_aaaaaaaa1111", Title: "queued", Ref: "SYS-2"}},
		InProgress: []api.Task{{ID: "t_bbbbbbbb2222", Title: "working \x1b[31mred"}},
		Done:       []api.Task{{ID: "t_cccccccc3333", Title: "shipped", Ref: "SYS-3"}}}}
	t.Setenv("TZ", "Asia/Amman")
	out, err := runTask(t, f, "ls", "--date", "2026-10-03")
	if err != nil {
		t.Fatal(err)
	}
	want := `Todo (1)
  aaaaaaaa  queued  SYS-2

In progress (1)
  bbbbbbbb  working [31mred

Done (1)
  cccccccc  shipped  SYS-3
`
	if out != want {
		t.Errorf("out:\n%s\nwant:\n%s", out, want)
	}
	if f.dayArgs[0] != "2026-10-03 Asia/Amman" {
		t.Errorf("day args = %v", f.dayArgs)
	}
	if _, err := runTask(t, f, "ls", "--date", "nope"); err == nil {
		t.Error("bad date accepted")
	}
}

func TestTaskReview(t *testing.T) {
	f := &fakeTasks{review: api.TaskReview{
		Proposed:     []api.Task{{ID: "t_aaaaaaaa1111", Title: "proposal"}},
		DoneProposed: []api.Task{{ID: "t_bbbbbbbb2222", Title: "claimed done"}},
		Untasked:     []api.Session{{ID: "ssssssss-9999", Title: "stray", Machine: "tower"}},
	}}
	out, err := runTask(t, f, "review")
	if err != nil {
		t.Fatal(err)
	}
	want := `Proposed (1)
  aaaaaaaa  proposal

Awaiting done (1)
  bbbbbbbb  claimed done

Sessions with no task (1)
  ssssssss  stray  tower
`
	if out != want {
		t.Errorf("out:\n%s\nwant:\n%s", out, want)
	}
	out, _ = runTask(t, &fakeTasks{}, "review")
	if out != "nothing to review\n" {
		t.Errorf("empty out = %q", out)
	}
}

func TestTaskStateCommands(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"accept", "aaaaa"}, "t_aaaaaaaa1111 todo "},
		{[]string{"start", "dddd", "--note", "go"}, "t_dddddddd4444 in_progress go"},
		{[]string{"done", "t_cccc"}, "t_cccccccc3333 done "},
		{[]string{"reject", "aaaaa"}, "t_aaaaaaaa1111 dropped "},
		{[]string{"reject", "cccc"}, "t_cccccccc3333 in_progress "},
		{[]string{"drop", "eeee"}, "t_eeeeeeee5555 dropped "},
		{[]string{"reopen", "eeee"}, "t_eeeeeeee5555 todo "},
	}
	for _, c := range cases {
		f := taskFixture()
		if _, err := runTask(t, f, c.args...); err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if len(f.states) != 1 || f.states[0] != c.want {
			t.Errorf("%v: states = %v, want %q", c.args, f.states, c.want)
		}
	}
	f := taskFixture()
	if _, err := runTask(t, f, "reject", "eeee"); err == nil {
		t.Error("reject of a todo task succeeded")
	}
}

func TestTaskMerge(t *testing.T) {
	f := taskFixture()
	if _, err := runTask(t, f, "merge", "aaaaa", "--into", "eeee"); err != nil {
		t.Fatal(err)
	}
	if len(f.merges) != 1 || f.merges[0] != "t_aaaaaaaa1111 t_eeeeeeee5555" {
		t.Errorf("merges = %v", f.merges)
	}
	if _, err := runTask(t, f, "merge", "aaaaa"); err == nil {
		t.Error("merge without --into succeeded")
	}
}

func TestTaskAmbiguousPrefix(t *testing.T) {
	f := taskFixture()
	_, err := runTask(t, f, "accept", "aaaa")
	if err == nil || !strings.Contains(err.Error(), "aaaaaaaa") || !strings.Contains(err.Error(), "aaaabbbb") {
		t.Fatalf("err = %v", err)
	}
	if len(f.states) != 0 {
		t.Error("state sent for an ambiguous prefix")
	}
	if _, err := runTask(t, f, "accept", "zzz"); err == nil {
		t.Error("unknown prefix accepted")
	}
}

func TestTaskResolvesFromDay(t *testing.T) {
	f := taskFixture()
	f.day.Done = []api.Task{{ID: "t_ffffffff6666", Title: "old", State: api.TaskDone}}
	if _, err := runTask(t, f, "reopen", "ffff"); err != nil {
		t.Fatal(err)
	}
	if f.states[0] != "t_ffffffff6666 todo " {
		t.Errorf("states = %v", f.states)
	}
}

func TestTaskErrors(t *testing.T) {
	f := taskFixture()
	f.err = errors.New("boom")
	if _, err := runTask(t, f, "review"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
	if _, err := runTask(t, taskFixture(), "bogus"); err == nil {
		t.Error("unknown subcommand accepted")
	}
}

func TestTaskFullIDNotListed(t *testing.T) {
	for _, arg := range []string{"t_zzzzzzzzzzz", "zzzzzzzzzzz"} {
		f := taskFixture()
		out, err := runTask(t, f, "reopen", arg)
		if err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		if !strings.Contains(out, "(older task)") {
			t.Errorf("%s: output %q, want the server's title", arg, out)
		}
		if len(f.states) != 1 || f.states[0] != "t_zzzzzzzzzzz todo " {
			t.Errorf("%s: states = %v", arg, f.states)
		}
	}
	f := taskFixture()
	if _, err := runTask(t, f, "reopen", "zzzz"); err == nil || len(f.states) != 0 {
		t.Errorf("unknown short prefix: err = %v, states = %v", err, f.states)
	}
}

func TestTaskLsPastDayShowsFullIDs(t *testing.T) {
	f := &fakeTasks{day: api.TaskDay{Done: []api.Task{{ID: "t_aaaaaaaa111", Title: "old"}}}}
	out, err := runTask(t, f, "ls", "--date", "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "  t_aaaaaaaa111  old") {
		t.Errorf("out = %q", out)
	}
}

func TestTaskLsJSON(t *testing.T) {
	f := taskFixture()
	f.day = api.TaskDay{Date: "2026-09-30", TZ: "UTC", Todo: []api.Task{}, Done: []api.Task{},
		InProgress: []api.Task{{ID: "t_dddddddd4444", Title: "working", State: api.TaskInProgress, ActiveSeconds: 90}}}
	out, err := runTask(t, f, "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got api.TaskDay
	if err := json.Unmarshal([]byte(out), &got); err != nil || !reflect.DeepEqual(got, f.day) {
		t.Errorf("json %q: %+v %v", out, got, err)
	}
}
