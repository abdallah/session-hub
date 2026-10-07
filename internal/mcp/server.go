// Package mcp implements the sessionhub MCP server (newline-delimited JSON-RPC 2.0
// over stdio) and the install-mcp and uninstall-mcp commands.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

// Version is the serverInfo version reported to clients.
const Version = "dev"

// instructions is the MCP initialize instructions string. Keep it in step
// with docs/CLAUDE-snippet.md.
const instructions = "Call report_progress when you finish a task, start a long one, or are blocked. Call set_title once the session has a clear purpose. " +
	"Record human-level work with sessionhub note (or worklog note, which calls it): one note when you start a piece of work, with its ticket ref (systemsdev-12345, MR !1234) if it has one, " +
	"and one starting with done: when you finish it. sessionhub turns notes into tasks. " +
	"Use list_tasks, link_task, propose_task, and propose_done only when you cannot run shell commands. No notes for steps inside a piece of work."

// Protocol versions this server speaks. The first is the default.
var supportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Run serves MCP on stdin and stdout until stdin closes. Logs go to stderr.
func Run(ctx context.Context, args []string) error {
	log.SetOutput(os.Stderr)
	log.SetPrefix("sessionhub mcp: ")
	return NewServer().Serve(ctx, os.Stdin, os.Stdout)
}

// Serve reads requests from r and writes responses to w, one JSON message
// per line. It returns nil when r reaches EOF.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	br := bufio.NewReaderSize(r, 64<<10)
	enc := json.NewEncoder(w) // Encode ends each message with a newline
	for {
		line, err := br.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			if resp := s.handleLine(ctx, line); resp != nil {
				if werr := enc.Encode(resp); werr != nil {
					return werr
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (s *Server) handleLine(ctx context.Context, line []byte) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return errResp(json.RawMessage("null"), codeParse, "parse error")
	}
	if req.Method == "" {
		return nil // a stray response; servers send no requests
	}
	res, rerr := s.dispatch(ctx, req)
	if len(req.ID) == 0 {
		return nil // notification
	}
	if rerr != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rerr}
	}
	return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: res}
}

func errResp(id json.RawMessage, code int, msg string) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := supportedVersions[0]
		for _, v := range supportedVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "sessionhub", "version": Version},
			"instructions":    instructions,
		}, nil
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefs}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call needs a tool name"}
		}
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage("{}")
		}
		switch p.Name {
		case "report_progress":
			return s.reportProgress(ctx, p.Arguments), nil
		case "set_title":
			return s.setTitle(ctx, p.Arguments), nil
		case "remember":
			return s.remember(ctx, p.Arguments), nil
		case "forget":
			return s.forget(ctx, p.Arguments), nil
		case "instructions":
			return s.instructions(ctx, p.Arguments), nil
		case "send_to_sessions":
			return s.sendToSessions(ctx, p.Arguments), nil
		case "list_tasks":
			return s.listTasks(ctx, p.Arguments), nil
		case "propose_task":
			return s.proposeTask(ctx, p.Arguments), nil
		case "link_task":
			return s.linkTask(ctx, p.Arguments), nil
		case "propose_done":
			return s.proposeDone(ctx, p.Arguments), nil
		}
		return nil, &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf("unknown tool %q", p.Name)}
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
}

type toolResult struct {
	Content []map[string]string `json:"content"`
	IsError bool                `json:"isError,omitempty"`
}

func text(msg string, isErr bool) toolResult {
	return toolResult{Content: []map[string]string{{"type": "text", "text": msg}}, IsError: isErr}
}

func stringArray(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

var toolDefs = []map[string]any{
	{
		"name": "report_progress",
		"description": "Report what this session has done, what is in flight, and what it is waiting on, so the user can " +
			"see progress from the sessionhub dashboard and the herdr sidebar without opening the session. Call it when you " +
			"finish a task, when you start a long-running task, and when you are blocked waiting on the user or on " +
			"something external. Keep each item to one short line. Each call replaces the previous status.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"done":       stringArray("What you finished since the last report."),
				"in_flight":  stringArray("What you are working on now."),
				"waiting_on": stringArray("What you are blocked on, for example a decision from the user."),
				"note":       map[string]any{"type": "string", "description": "Optional free-text detail."},
			},
			"required": []string{"done", "in_flight", "waiting_on"},
		},
	},
	{
		"name": "set_title",
		"description": "Set a short title for this session (a few words) that says what it is about. Call it once the " +
			"session has a clear purpose, and again if the purpose changes. The title appears in the sessionhub dashboard " +
			"and overrides automatic titles.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title": map[string]any{"type": "string", "description": "The session title."},
			},
			"required": []string{"title"},
		},
	},
	{
		"name": "remember",
		"description": "Add a standing rule that every Claude session on every machine sees at its start and with " +
			"every prompt, through sessionhub. Use it only when the user asks for something to apply to all sessions, for " +
			"example \"from now on, in every session, ...\". Do not use it for this session's own preferences, for " +
			"notes, or on your own initiative. Keep the rule to one short line, at most 300 characters.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string", "description": "The rule, in one line."},
			},
			"required": []string{"text"},
		},
	},
	{
		"name":        "forget",
		"description": "Remove a standing rule by its number. Call instructions first to find the number. Use it only when the user asks.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "integer", "description": "The rule number from instructions."},
			},
			"required": []string{"id"},
		},
	},
	{
		"name":        "instructions",
		"description": "List the standing rules every session sees, with their numbers and who added them.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name": "send_to_sessions",
		"description": "Send a message to other Claude sessions through sessionhub. sessionhub types it into each session's terminal, " +
			"prefixed with who sent it, once that session's agent is idle. Use it only when the user asks you to " +
			"message other sessions. Session IDs are full IDs, for example from `sessionhub ls`.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_ids": stringArray("Full session IDs, 1 to 20."),
				"text":        map[string]any{"type": "string", "description": "The message."},
			},
			"required": []string{"session_ids", "text"},
		},
	},
	{
		"name":        "list_tasks",
		"description": "List the open human-level tasks (proposed, todo, in progress, awaiting done). Call it before propose_task so you link to an existing task instead of creating a duplicate.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name":        "propose_task",
		"description": "Propose a human-level task for this session's goal: a ticket, an email or chat request, a bug you were asked to fix. Write the title the way a person would name it in a standup, not an implementation step. The user accepts or rejects it. Prefer sessionhub note: use this only when you cannot run shell commands.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":   map[string]any{"type": "string", "description": "The task title, at most 200 characters."},
				"source":  map[string]any{"type": "string", "enum": []string{"ticket", "email", "chat", "other"}, "description": "Where the goal came from."},
				"ref":     map[string]any{"type": "string", "description": "Optional reference, for example a ticket key. An open task with the same ref is reused."},
				"ref_url": map[string]any{"type": "string", "description": "Optional http or https link to the reference."},
			},
			"required": []string{"title", "source"},
		},
	},
	{
		"name":        "link_task",
		"description": "Link this session to an existing task from list_tasks. A todo task moves to in progress.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "The task id from list_tasks."},
			},
			"required": []string{"task_id"},
		},
	},
	{
		"name":        "propose_done",
		"description": "Tell the user this task's goal is finished. The user confirms it. Do not call it for steps inside a task. Prefer sessionhub note: use this only when you cannot run shell commands.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "The task id from list_tasks."},
				"note":    map[string]any{"type": "string", "description": "Optional one-line result, for example the merge request."},
			},
			"required": []string{"task_id"},
		},
	},
}
