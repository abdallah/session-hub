# Tasks design

Date: 2026-10-07. Status: design approved in chat; spec awaiting review.

## Goal

A daily record of the work that matters to a person, kept apart from the
small steps agents report. Each task is a unit you would name in a standup:
a ticket, an email request, a chat request. You see what is todo, in
progress, and done for any date. Todos carry over from day to day until you
move or drop them.

Agents propose tasks and propose completion; you confirm both. Nothing an
agent says reaches **Done** without you.

## Decisions

| Topic | Decision |
|---|---|
| Where | Inside sessionhub, as `internal/store/tasks.go`, `internal/server/tasks.go`, MCP tools, CLI commands, and a dashboard **Tasks** tab. |
| Creating tasks | Agents propose (`propose_task`), you accept, merge, or reject. You can also add tasks directly. |
| Finishing tasks | Agents propose done (`propose_done`), you confirm. You can mark done directly. |
| References | Generic: free-text `ref`, optional `ref_url`, and a `source`. No ticket system calls, so changing ticket systems costs nothing. |
| Past dates | Rebuilt exactly from a log of state changes. |
| Agent steps | `report_progress` items from linked sessions show under a task as evidence. They are never tasks. |
| Out of scope | Ticket system integration, time tracking, Google Tasks export, per-person task lists, retiring `worklog` (a follow-up). |

## Data model

Schema version 13 adds three tables.

```sql
CREATE TABLE tasks (
	id         TEXT PRIMARY KEY,          -- t_ + 8 random bytes, base64url
	title      TEXT NOT NULL,             -- at most 200 runes
	ref        TEXT NOT NULL DEFAULT '',  -- at most 200 runes, e.g. OPS-1234 or "email from a customer re DNS"
	ref_url    TEXT NOT NULL DEFAULT '',  -- http or https, at most 2000 bytes
	source     TEXT NOT NULL,             -- ticket|email|chat|other
	state      TEXT NOT NULL,             -- proposed|todo|in_progress|done_proposed|done|dropped
	merged_into TEXT REFERENCES tasks(id) ON DELETE SET NULL,
	created_by TEXT NOT NULL,             -- web:<name> | machine:<name> | session:<id>
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX tasks_state ON tasks(state, updated_at);
CREATE TABLE task_events (
	id      INTEGER PRIMARY KEY,
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	ts      TEXT NOT NULL,
	from_state TEXT NOT NULL DEFAULT '',  -- '' for the creating event
	to_state   TEXT NOT NULL,
	actor   TEXT NOT NULL,                -- same form as created_by
	note    TEXT NOT NULL DEFAULT ''      -- at most 500 runes
);
CREATE INDEX task_events_ts ON task_events(ts);
CREATE INDEX task_events_task ON task_events(task_id, id);
CREATE TABLE task_sessions (
	task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	linked_at  TEXT NOT NULL,
	PRIMARY KEY (task_id, session_id)
);
CREATE INDEX task_sessions_session ON task_sessions(session_id);
```

Every state change writes the `tasks` row and one `task_events` row in the
same transaction.

## States and transitions

| From | To | Who |
|---|---|---|
| (new) | `proposed` | An agent, `propose_task` |
| (new) | `todo` or `in_progress` | You |
| `proposed` | `todo` or `in_progress` | You, accept |
| `proposed` | `dropped` | You, reject |
| `proposed` | (merged) | You, merge: the proposal becomes `dropped` with `merged_into` set, and its session links move to the target |
| `todo` | `in_progress` | You, or an agent with `link_task` |
| `in_progress` | `todo` | You |
| `todo`, `in_progress` | `done_proposed` | An agent, `propose_done` |
| `done_proposed` | `done` | You, confirm |
| `done_proposed` | `in_progress` | You, reject the completion |
| any but `proposed` | `done` | You |
| any | `dropped` | You |
| `done`, `dropped` | `todo` or `in_progress` | You, reopen |

The server refuses any other transition with 409. Agents can only create
`proposed` tasks, move a task to `in_progress` through `link_task`, and move
one to `done_proposed`. `link_task` on a `proposed` task links the session
but leaves the state alone.

## Day view

A request names a date and an IANA time zone. The dashboard sends the
browser's zone; the CLI sends the local zone. The day runs from 00:00 to
24:00 in that zone.

For each task, the server replays its `task_events` up to the end of the day.

| Column | Rule |
|---|---|
| Done | Has an event to `done` during the day, and its state at the end of the day is `done`. |
| In progress | Was `in_progress` or `done_proposed` at any moment during the day, and is not in **Done**. |
| Todo | State at the end of the day is `todo`. |

- A task appears in one column only, the first that matches.
- `proposed` tasks and `dropped` tasks never appear in the columns.
- Carry-over falls out of the rules: a `todo` task with no events stays
  `todo` at the end of every later day.
- Today's view uses the current time as the end of the day.

Each card shows the title, ref (linked if `ref_url` is set), source, linked
sessions with their titles and machines, and the `done` items from those
sessions' reports made during the day.

## Review queue

Not tied to a date. It holds:

- `proposed` tasks, oldest first.
- `done_proposed` tasks, oldest first.
- **Sessions with no task**: sessions that had a prompt in the last 7 days,
  are not linked to any task, and have a title. Each offers **Make task**,
  which creates a `todo` task from the session title and links the session,
  and **Ignore**, which hides it. Ignores are stored in
  `task_session_ignores (session_id PRIMARY KEY, ignored_at)`, also in
  schema version 13.

## MCP tools

Added to `internal/mcp/server.go`. Each takes the current session from the
MCP server's existing session context.

| Tool | Arguments | Effect |
|---|---|---|
| `list_tasks` | none | Returns tasks in `proposed`, `todo`, `in_progress`, and `done_proposed`, with id, title, ref, and state. |
| `propose_task` | `title`, `source`, optional `ref`, `ref_url` | Creates a `proposed` task and links the session. If an open task has the same `ref` (case-insensitive, non-empty), returns that task instead and links the session to it. |
| `link_task` | `task_id` | Links the session; moves a `todo` task to `in_progress`. |
| `propose_done` | `task_id`, `note` | Moves the task to `done_proposed` with the note on the event. |

Tool descriptions tell the agent to call `list_tasks` before
`propose_task`, and to write titles a person would recognize, not
implementation steps.

The shared instruction added to the sessionhub instructions list:

> When a session has a clear human-level goal (a ticket, an email or chat
> request, a bug you were asked to fix), call `list_tasks`, then
> `link_task` if it is already there or `propose_task` if not. When that
> goal is finished, call `propose_done`. Do not create tasks for steps
> inside a goal.

Like `report_progress`, a failed call goes to the offline queue. A queued
`propose_task` carries a client-generated task id so the replay is
idempotent.

## HTTP API

Routes in `internal/server/tasks.go`. Machine-token and web-session auth as
for the existing routes.

| Route | Purpose |
|---|---|
| `GET /v1/tasks?state=open` | Open tasks, for `list_tasks`. |
| `POST /v1/tasks` | Create. Agents get `proposed`; web and CLI choose `todo` or `in_progress`. |
| `POST /v1/tasks/{id}/state` | `{to, note}`. Checks the transition table and the actor. |
| `POST /v1/tasks/{id}/merge` | `{into}`. Proposals only. |
| `POST /v1/tasks/{id}/sessions` | `{session_id}`. Link. |
| `PATCH /v1/tasks/{id}` | Edit title, ref, ref_url, source. You only. |
| `GET /v1/tasks/day?date=YYYY-MM-DD&tz=Area/City` | The three columns. |
| `GET /v1/tasks/review` | The review queue. |
| `POST /v1/tasks/review/ignore` | `{session_id}`. |

## CLI

In `internal/cli/tasks.go`.

- `sessionhub task add "title" [--ref X] [--url U] [--source email] [--now]`:
  creates a `todo` task, or `in_progress` with `--now`.
- `sessionhub task ls [--date YYYY-MM-DD]`: prints the three columns.
- `sessionhub task review`: lists the queue with short ids.
- `sessionhub task accept|reject|done|drop|start|reopen <id> [--note]`.
- `sessionhub task merge <id> --into <id>`.

## Dashboard

A **Tasks** tab in `internal/server/dashboard/index.html`.

- Header: date picker, previous and next day buttons, **Today**, **Add task**.
- **Review** strip above the columns when the queue is not empty, with
  **Accept**, **Merge into…**, **Reject**, **Confirm done**, **Not done**,
  **Make task**, and **Ignore**.
- Three columns: **Todo**, **In progress**, **Done**. On today's view,
  each card has a menu to change state. Past days are read-only.
- A count on the tab of items waiting for review.

## Testing

- Store: day-view replay for carry-over across several days, a task done
  on one day and reopened on the next, a drop, a proposal that is never
  accepted, time zone edges (an event at 23:30 in one zone and 00:30 in
  another), and the transition table, including refusals.
- Server: each route, auth for agent versus web actors, 409 on a bad
  transition.
- MCP: each tool, the same-ref dedupe, and offline replay of a queued
  `propose_task` creating one task.
- CLI: output of `task ls` and `task review` against a test server.
- Dashboard: an evidence note with screenshots of the Tasks tab and review
  strip, as for earlier features.

## Docs

- `docs/usage.md`: a **Tasks** section.
- `docs/mcp.md`: the four tools.
- `docs/cli.md`: the `task` commands.
- `docs/server.md`: the routes.
