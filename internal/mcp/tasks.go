package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

const (
	maxRefURLLen   = 2000
	maxTaskNoteLen = 500
	listTasksDown  = "sessionhub: could not reach the server to list tasks. Continue with your work."
)

// listTasks returns one line per open task: <id> [<state>] <title> (<ref>).
// A failure is a plain reply, not an error: listing is advice, never a blocker.
func (s *Server) listTasks(ctx context.Context, _ json.RawMessage) toolResult {
	a, err := s.API()
	if err != nil {
		return text(listTasksDown, false)
	}
	tasks, err := a.OpenTasks(ctx)
	if err != nil {
		return text(listTasksDown, false)
	}
	if len(tasks) == 0 {
		return text("sessionhub: no open tasks.", false)
	}
	lines := make([]string, 0, len(tasks))
	for _, t := range tasks {
		line := fmt.Sprintf("%s [%s] %s", clean(t.ID, 40), clean(t.State, 20), clean(t.Title, maxTitleLen))
		if ref := clean(t.Ref, maxTitleLen); ref != "" {
			line += " (" + ref + ")"
		}
		lines = append(lines, line)
	}
	return text(strings.Join(lines, "\n"), false)
}

// proposeTask creates a proposed task and links this session to it. The
// client-made id makes a replay of the queued call a no-op.
func (s *Server) proposeTask(ctx context.Context, args json.RawMessage) toolResult {
	var in struct {
		Title  string `json:"title"`
		Source string `json:"source"`
		Ref    string `json:"ref"`
		RefURL string `json:"ref_url"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return text("invalid arguments: "+reason(err.Error()), true)
	}
	title := clean(in.Title, maxTitleLen)
	if title == "" {
		return text("title must not be empty", true)
	}
	source := clean(in.Source, 20)
	switch source {
	case "ticket", "email", "chat", "other":
	default:
		return text("source must be ticket, email, chat, or other", true)
	}
	sid := s.resolveSession()
	if sid == "" {
		return text(noSession, false)
	}
	taskID := client.NewTaskID()
	ti := api.TaskIn{
		ID:        taskID,
		Title:     title,
		Ref:       clean(in.Ref, maxTitleLen),
		RefURL:    clean(in.RefURL, maxRefURLLen),
		Source:    source,
		SessionID: sid,
	}
	body, _ := json.Marshal(ti)
	it := client.Item{Op: client.OpTaskCreate, SessionID: sid, TaskID: taskID, Body: body}
	return s.sendItem(ctx, it, "task proposal", func(a hubAPI) (string, error) {
		t, err := a.CreateTask(ctx, ti)
		if err != nil {
			return "", err
		}
		if t.ID != taskID {
			return fmt.Sprintf("An open task with this ref already exists: %s (%s). This session is linked to it; do not propose it again.",
				clean(t.ID, 40), clean(t.Title, maxTitleLen)), nil
		}
		return "Task " + taskID + " proposed. The user accepts or rejects it.", nil
	})
}

// linkTask links this session to an existing task.
func (s *Server) linkTask(ctx context.Context, args json.RawMessage) toolResult {
	taskID, _, res, ok := s.taskArgs(args)
	if !ok {
		return res
	}
	sid := s.resolveSession()
	if sid == "" {
		return text(noSession, false)
	}
	li := api.TaskLinkIn{SessionID: sid, EventID: client.NewTaskEventID()}
	body, _ := json.Marshal(li)
	it := client.Item{Op: client.OpTaskLink, SessionID: sid, TaskID: taskID, Body: body}
	return s.sendItem(ctx, it, "task link", func(a hubAPI) (string, error) {
		_, err := a.LinkTask(ctx, taskID, li)
		return "", err
	})
}

// proposeDone moves a task to done_proposed. A rejected transition (409) is
// reported and not queued, because a retry cannot change the answer.
func (s *Server) proposeDone(ctx context.Context, args json.RawMessage) toolResult {
	taskID, note, res, ok := s.taskArgs(args)
	if !ok {
		return res
	}
	sid := s.resolveSession()
	if sid == "" {
		return text(noSession, false)
	}
	si := api.TaskStateIn{To: "done_proposed", Note: note, SessionID: sid, EventID: client.NewTaskEventID()}
	body, _ := json.Marshal(si)
	it := client.Item{Op: client.OpTaskState, SessionID: sid, TaskID: taskID, Body: body}
	return s.sendItem(ctx, it, "task done proposal", func(a hubAPI) (string, error) {
		_, err := a.SetTaskState(ctx, taskID, si)
		return "", err
	})
}

// taskArgs parses the task_id and note arguments shared by link_task and
// propose_done. ok is false when res is the reply to return.
func (s *Server) taskArgs(args json.RawMessage) (taskID, note string, res toolResult, ok bool) {
	var in struct {
		TaskID string `json:"task_id"`
		Note   string `json:"note"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", "", text("invalid arguments: "+reason(err.Error()), true), false
	}
	taskID = clean(in.TaskID, 100)
	if taskID == "" {
		return "", "", text("task_id must not be empty: take it from list_tasks", true), false
	}
	return taskID, clean(in.Note, maxTaskNoteLen), toolResult{}, true
}
