package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/client"
)

// taskAPI is the part of *client.Client that sessionhub task uses.
type taskAPI interface {
	OpenTasks(ctx context.Context) ([]api.Task, error)
	CreateTask(ctx context.Context, in api.TaskIn) (api.Task, error)
	SetTaskState(ctx context.Context, id string, in api.TaskStateIn) (api.Task, error)
	MergeTask(ctx context.Context, id, into string) (api.Task, error)
	TaskDay(ctx context.Context, date, tz string) (api.TaskDay, error)
	TaskReview(ctx context.Context) (api.TaskReview, error)
}

const taskUsage = `usage:
  sessionhub task add "title" [--ref X] [--url U] [--source S] [--now]
  sessionhub task ls [--date YYYY-MM-DD] [--json]
  sessionhub task review
  sessionhub task accept|reject|done|drop|start|reopen <id> [--note T]
  sessionhub task merge <id> --into <id>`

// taskVerbs maps a state command to its target state. reject is decided per
// task in taskState.
var taskVerbs = map[string]string{
	"accept": api.TaskTodo,
	"start":  api.TaskInProgress,
	"done":   api.TaskDone,
	"drop":   api.TaskDropped,
	"reopen": api.TaskTodo,
}

// RunTask implements `sessionhub task`.
func RunTask(ctx context.Context, args []string) error {
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	return e.taskCmd(ctx, args)
}

func (e *env) taskCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("task: want a subcommand\n%s", taskUsage)
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "add":
		return e.taskAdd(ctx, rest)
	case "ls":
		return e.taskLs(ctx, rest)
	case "review":
		return e.taskReview(ctx, rest)
	case "merge":
		return e.taskMerge(ctx, rest)
	case "reject":
		return e.taskState(ctx, cmd, rest)
	}
	if _, ok := taskVerbs[cmd]; ok {
		return e.taskState(ctx, cmd, rest)
	}
	return fmt.Errorf("task: unknown subcommand %q\n%s", clean(cmd, 0), taskUsage)
}

// parseInterspersed parses fs over args, allowing flags after positional
// arguments, and returns the positionals.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func taskLine(t api.Task, full bool) string {
	id := taskShortID(t.ID)
	if full {
		id = clean(t.ID, 0)
	}
	parts := []string{id, clean(t.Title, termtext.TitleWidth)}
	if r := clean(t.Ref, 0); r != "" {
		parts = append(parts, r)
	}
	return "  " + strings.Join(parts, "  ")
}

// taskShortID is the first 8 characters after "t_".
func taskShortID(id string) string {
	return shortID(strings.TrimPrefix(id, "t_"))
}

func (e *env) taskAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessionhub task add", flag.ContinueOnError)
	ref := fs.String("ref", "", "ticket or message reference")
	url := fs.String("url", "", "link to the reference")
	source := fs.String("source", "", "ticket|email|chat|other")
	now := fs.Bool("now", false, "start the task now")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		return fmt.Errorf("task add: want one title\n%s", taskUsage)
	}
	in := api.TaskIn{Title: pos[0], Ref: *ref, RefURL: *url, Source: *source}
	if *now {
		in.State = api.TaskInProgress
	}
	t, err := e.tasks.CreateTask(ctx, in)
	if err != nil {
		return fmt.Errorf("task add: %w", err)
	}
	fmt.Fprintf(e.out, "added %s (%s)\n", taskShortID(t.ID), clean(t.Title, termtext.TitleWidth))
	return nil
}

// localZone is the IANA name of the local time zone: TZ, else the
// /etc/localtime link target, else UTC.
func localZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" && tz != "Local" {
		return tz
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok && name != "" {
			return name
		}
	}
	return "UTC"
}

func (e *env) taskLs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessionhub task ls", flag.ContinueOnError)
	date := fs.String("date", "", "day to show, YYYY-MM-DD (default today)")
	asJSON := fs.Bool("json", false, "print the day view as JSON")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) > 0 {
		return fmt.Errorf("task ls: unexpected arguments\n%s", taskUsage)
	}
	tz := localZone()
	if *date == "" {
		*date = e.now().Format("2006-01-02")
	} else if _, err := time.Parse("2006-01-02", *date); err != nil {
		return fmt.Errorf("task ls: --date %q: want YYYY-MM-DD", clean(*date, 0))
	}
	d, err := e.tasks.TaskDay(ctx, *date, tz)
	if err != nil {
		return fmt.Errorf("task ls: %w", err)
	}
	if *asJSON {
		enc := json.NewEncoder(e.out)
		enc.SetIndent("", "  ")
		return enc.Encode(d)
	}
	// Past days list tasks that may be in neither the open list nor today's
	// view, so the verbs only find them by full id.
	full := *date < e.now().Format("2006-01-02")
	e.taskSection("Todo", d.Todo, false, full)
	e.taskSection("In progress", d.InProgress, true, full)
	e.taskSection("Done", d.Done, true, full)
	return nil
}

// taskSection prints a heading with a count and one line per task. blank
// puts an empty line before the heading.
func (e *env) taskSection(heading string, tasks []api.Task, blank, full bool) {
	if blank {
		fmt.Fprintln(e.out)
	}
	fmt.Fprintf(e.out, "%s (%d)\n", heading, len(tasks))
	for _, t := range tasks {
		fmt.Fprintln(e.out, cut(taskLine(t, full), e.width()))
	}
}

func (e *env) taskReview(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("task review: unexpected arguments\n%s", taskUsage)
	}
	r, err := e.tasks.TaskReview(ctx)
	if err != nil {
		return fmt.Errorf("task review: %w", err)
	}
	if len(r.Proposed)+len(r.DoneProposed)+len(r.Untasked) == 0 {
		fmt.Fprintln(e.out, "nothing to review")
		return nil
	}
	first := true
	section := func(heading string, n int, lines func(i int) string) {
		if n == 0 {
			return
		}
		if !first {
			fmt.Fprintln(e.out)
		}
		first = false
		fmt.Fprintf(e.out, "%s (%d)\n", heading, n)
		for i := 0; i < n; i++ {
			fmt.Fprintln(e.out, cut(lines(i), e.width()))
		}
	}
	section("Proposed", len(r.Proposed), func(i int) string { return taskLine(r.Proposed[i], false) })
	section("Awaiting done", len(r.DoneProposed), func(i int) string { return taskLine(r.DoneProposed[i], false) })
	section("Sessions with no task", len(r.Untasked), func(i int) string {
		s := r.Untasked[i]
		return "  " + strings.Join([]string{shortID(s.ID), clean(title(s), termtext.TitleWidth), clean(s.Machine, 0)}, "  ")
	})
	return nil
}

func (e *env) taskState(ctx context.Context, cmd string, args []string) error {
	fs := flag.NewFlagSet("sessionhub task "+cmd, flag.ContinueOnError)
	note := fs.String("note", "", "note to record with the change")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 {
		return fmt.Errorf("task %s: want one task id\n%s", cmd, taskUsage)
	}
	t, err := e.findTask(ctx, pos[0])
	if err != nil {
		return fmt.Errorf("task %s: %w", cmd, err)
	}
	to := taskVerbs[cmd]
	if cmd == "reject" {
		switch t.State {
		case api.TaskProposed:
			to = api.TaskDropped
		case api.TaskDoneProposed:
			to = api.TaskInProgress
		default:
			return fmt.Errorf("task reject: %s is %s; only a proposal or a done proposal can be rejected", taskShortID(t.ID), clean(t.State, 0))
		}
	}
	out, err := e.tasks.SetTaskState(ctx, t.ID, api.TaskStateIn{To: to, Note: *note})
	if err != nil {
		return fmt.Errorf("task %s: %w", cmd, err)
	}
	fmt.Fprintf(e.out, "%s %s (%s)\n", clean(out.State, 0), taskShortID(out.ID), clean(out.Title, termtext.TitleWidth))
	return nil
}

func (e *env) taskMerge(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessionhub task merge", flag.ContinueOnError)
	into := fs.String("into", "", "task to merge into")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 || *into == "" {
		return fmt.Errorf("task merge: want <id> --into <id>\n%s", taskUsage)
	}
	src, err := e.findTask(ctx, pos[0])
	if err != nil {
		return fmt.Errorf("task merge: %w", err)
	}
	dst, err := e.findTask(ctx, *into)
	if err != nil {
		return fmt.Errorf("task merge: --into: %w", err)
	}
	t, err := e.tasks.MergeTask(ctx, src.ID, dst.ID)
	if err != nil {
		return fmt.Errorf("task merge: %w", err)
	}
	fmt.Fprintf(e.out, "merged %s into %s (%s)\n", taskShortID(src.ID), taskShortID(t.ID), clean(t.Title, termtext.TitleWidth))
	return nil
}

// fullTaskID matches the part of a task id after "t_": 8 random bytes in
// base64url.
var fullTaskID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// findTask resolves a task id, or a unique prefix of the part after "t_",
// against the open tasks and today's day view.
func (e *env) findTask(ctx context.Context, target string) (api.Task, error) {
	want := strings.TrimPrefix(target, "t_")
	if want == "" {
		return api.Task{}, fmt.Errorf("empty task id")
	}
	open, err := e.tasks.OpenTasks(ctx)
	if err != nil {
		return api.Task{}, err
	}
	day, err := e.tasks.TaskDay(ctx, e.now().Format("2006-01-02"), localZone())
	if err != nil {
		return api.Task{}, err
	}
	seen := map[string]bool{}
	var hits []api.Task
	for _, group := range [][]api.Task{open, day.Todo, day.InProgress, day.Done} {
		for _, t := range group {
			if seen[t.ID] {
				continue
			}
			seen[t.ID] = true
			rest := strings.TrimPrefix(t.ID, "t_")
			if rest == want {
				return t, nil
			}
			if strings.HasPrefix(rest, want) {
				hits = append(hits, t)
			}
		}
	}
	switch len(hits) {
	case 0:
		// A task done on an earlier day is in neither list. A full id goes
		// to the server, which decides.
		if fullTaskID.MatchString(want) {
			return api.Task{ID: "t_" + want}, nil
		}
		return api.Task{}, fmt.Errorf("no task matches %q; see `sessionhub task ls`", clean(target, 0))
	case 1:
		return hits[0], nil
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].ID < hits[j].ID })
	c := make([]string, len(hits))
	for i, t := range hits {
		c[i] = fmt.Sprintf("%s (%s)", clean(strings.TrimPrefix(t.ID, "t_"), 0), clean(t.Title, termtext.TitleWidth))
	}
	return api.Task{}, fmt.Errorf("id prefix %q matches %d tasks: %s", clean(target, 0), len(hits), strings.Join(c, ", "))
}

// Compile-time check that the real client satisfies taskAPI.
var _ taskAPI = (*client.Client)(nil)
