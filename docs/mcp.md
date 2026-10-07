# MCP server

`sessionhub mcp` is an MCP server over stdio that lets a Claude Code session report
semantic progress and set its title. It is stdlib only: newline-delimited
JSON-RPC 2.0, one message per line. Stdout carries protocol messages only;
logs go to stderr.

## Install

```
sessionhub install-mcp      # claude mcp add --scope user sessionhub -- ~/.local/bin/sessionhub mcp
sessionhub uninstall-mcp    # claude mcp remove --scope user sessionhub
```

If `claude mcp add` fails (sessionhub is already registered), `install-mcp` removes the
entry and adds it again. Both commands need `claude` on `PATH`.

Paste `docs/CLAUDE-snippet.md` into `~/.claude/CLAUDE.md` so sessions know
when to call the tools.

## Protocol

Handled: `initialize`, `notifications/initialized`, `ping`, `tools/list`,
`tools/call`. Any other method returns JSON-RPC error `-32601`. An unknown tool
name returns `-32602`; bad JSON returns `-32700`.

`initialize` echoes the client's `protocolVersion` when it is `2025-06-18`,
`2025-03-26`, or `2024-11-05`, and answers `2025-06-18` otherwise.

## Tools

| Tool | Arguments |
|---|---|
| `report_progress` | `done: string[]`, `in_flight: string[]`, `waiting_on: string[]`, `note?: string` |
| `set_title` | `title: string` |
| `remember` | `text: string` |
| `forget` | `id: integer` |
| `instructions` | none |
| `send_to_sessions` | `session_ids: string[]`, `text: string` |
| `list_tasks` | none |
| `propose_task` | `title: string`, `source: string`, `ref?: string`, `ref_url?: string` |
| `link_task` | `task_id: string` |
| `propose_done` | `task_id: string`, `note?: string` |

Results are one text content item. For `report_progress` and `set_title`,
invalid arguments (a non-array list, an empty title) return a result with
`isError: true`, and everything else succeeds, including when the server is
down (fail open).

`remember`, `forget`, `instructions`, and `send_to_sessions` act at once and
never queue: a failure, including an unreachable server, is a result with
`isError: true` that says what happened. `remember` adds a standing rule
(see `docs/server.md`, "Shared instructions"); its description tells Claude
to use it only when the user asks for something to apply to all sessions.
It folds the text to one line and refuses more than 300 characters. A full
list (2,000 characters) tells Claude to ask the user which rule to `forget`.
`send_to_sessions` sends as `session <first 8 characters>` of the calling
session (the target reads `From session <id8> via sessionhub (sent on the user's
behalf):` above the text) and lists each target as queued or refused with the reason. It
returns at once and does not wait for delivery; only `sessionhub send` waits. If the
calling session's ID is unknown, it sends nothing. A `404` (unknown ID or
unregistered calling session), `409` (the calling session belongs to another
machine), or `429` (more than 30 messages a minute) is an error result that
names the cause.

The client shortens and cleans text so the server's validation never rejects a
model-written call. `report_progress` keeps at most 20 items of 200 characters
each and cuts `note` to 2000 characters; `set_title` cuts the title to 200
characters. Every control character (C0, DEL, and C1, including tab and
newline) becomes a space, and the result is trimmed. An item that is empty
after cleaning is dropped, and a title that is empty after cleaning is an
invalid-arguments error. Counts are in characters (runes), not bytes.

## Tasks

The task tools let a session tie its work to a task you see on the
dashboard's **Tasks** tab. A session acts as an agent: it can propose a
task, link itself to one, and propose one done. You decide the rest. See
[Tasks](server.md#tasks) for the rules.

- `list_tasks` returns one line per open task (proposed, todo, in progress,
  or awaiting done): `<id> [<state>] <title> (<ref>)`. When the server is
  unreachable it answers in plain text and does not set `isError`, because
  listing is advice and never a blocker.
- `propose_task` creates a `proposed` task and links the calling session.
  `title` is required, up to 200 characters. `source` is required: `ticket`,
  `email`, `chat`, or `other`; a missing or other value is an error result.
  If an open task already has the same `ref` (ignoring case), the call
  returns that task, links the session to it, and tells the agent not to
  propose it again. If the call was queued, the server applies the same rule
  on replay and keeps the queued task ID as a dropped task merged into the
  open one, so `link_task` and `propose_done` on that ID act on the open
  task.
- `link_task` links the calling session to a task from `list_tasks`. A
  `todo` task moves to `in_progress`; a task in any other state keeps its
  state.
- `propose_done` moves a `todo` or `in_progress` task to `done_proposed`,
  with `note` (up to 500 characters) on the event. You confirm it. If the
  server refuses the move, such as for a task that is already done, the
  reply is plain text that names the server's reason, with `isError` false,
  and the call is not queued, because a retry cannot change the answer.

These tools fail open like `report_progress`. If the server is
unreachable, the call goes to the offline queue and the tool reports that.
Each queued call carries a client-made task or event ID, so a replay never
creates a second task or repeats a state change. A call that needs the
calling session's ID and does not have it sends nothing.

The `initialize` instructions and `docs/CLAUDE-snippet.md` carry the rule
for agents: for a clear human-level goal, call `list_tasks`, then
`link_task` or `propose_task`; when the goal is finished, call
`propose_done`; create no tasks for steps inside a goal.

## Session ID

Resolved on every call, because the ID changes after `/clear` or an in-session
`/resume` and the MCP process keeps its spawn-time environment:

1. `<state>/current/<os.Getppid()>`, written by the `SessionStart` hook. The
   MCP server's parent is the Claude Code process. A hit costs no herdr call.
2. `HERDR_PANE_ID` set: herdr `pane.get`, read `agent_session.value`.
3. `CLAUDE_CODE_SESSION_ID`.

If none gives an ID, the tool returns a success result saying nothing was
reported.

## Delivery

1. `POST /v1/sessions/{id}/report` or `/title`.
2. On `404` (the session started before sessionhub was installed), the server upserts a
   minimal session (`id`, `agent: claude`, `source: mcp`, `cwd` from
   `CLAUDE_PROJECT_DIR`, herdr pane, workspace, and session from the
   environment) and retries once.
3. On a network error, a timeout (2 s), a `5xx`, `404` after the retry, or a
   missing client config, the call goes to the offline queue
   (`<state>/queue.jsonl`) as an item with `op` `report` or `title`,
   `session_id`, and `body` set to the request body. The tool result says it
   was queued.
4. Other `4xx` responses (bad token, invalid body) are not queued, because a
   retry cannot fix them. The tool result says the server rejected the call.

Drainers replay every item, including `report` and `title`, with
`client.Replay(ctx, c, item)` and keep it only when `client.IsRetryable(err)`.

## herdr sidebar

If `HERDR_PANE_ID` is set, `report_progress` sends `pane.report_metadata` with
`source: "sessionhub"` and `tokens: {"hub_summary": <summary>}`. The summary is the
first `waiting_on` item prefixed `waiting: `, else the first `in_flight` item,
else `done: ` plus the first `done` item, truncated to 80 characters. An empty
report clears the token. This happens even when the sessionhub server is down, and a
herdr failure never fails the tool. To see the value, add a `$hub_summary` row
to `[ui.sidebar.agents]` in your herdr config; sessionhub does not edit that file.

## Tests

`go test ./internal/mcp` covers the JSON-RPC transcript, session ID resolution
order, the summary rules, queueing when the server is down, the 404 upsert
retry, and `install-mcp` against a fake `claude` script that records its
arguments. The tests never touch your real Claude config.
