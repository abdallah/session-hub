# Notes to tasks design

Date: 2026-10-07. Status: design approved in chat; spec awaiting review.
Builds on `2026-10-07-tasks-design.md`.

## Goal

Tasks fill themselves from the notes agents already write with
`worklog note`, at the level a person names work. Each task carries its
working time. `worklog report` becomes a view of sessionhub tasks.

Success: the day's **Tasks** tab matches the real work list without tasks
created by hand, with no 0 s rows and no duplicates. Check against
2026-10-07: 17991, 17908, 18032, 17935, 17683, 16975, MR !2223, MR !2218,
SSO deploy plan.

## Decisions

| Topic | Decision |
|---|---|
| Source of tasks | `worklog note`, through a new `sessionhub note` command. Agents stop calling `propose_task` and `propose_done`; the tools stay. |
| Confirmation | None. A note creates or moves a task directly; you fix it on the dashboard (merge, drop, reopen). Replaces "nothing reaches **Done** without you" from the tasks design. |
| Note to task | A ref picks the task with that ref. Without a ref, the session's open task. A new task only when neither exists. |
| Finishing | A finish note marks the task done. |
| Session | `CLAUDE_CODE_SESSION_ID`, which Claude Code sets in every Bash call. |
| Time | Working periods from the `state_changed` events sessionhub already stores. No herdr sampling. |
| Out of scope | Backfill of past days, Google Tasks sync, removing the `propose_*` tools. |

## Note parsing

`internal/tasks/note.go`, a pure function: `Parse(text) Note{Title, Ref, RefKind, Finish}`.

- Finish: the text starts with `done:`, `done `, `finished`, `closed`, or
  `close ` (any case). The word and a following `:` are cut from the title.
- Ticket ref: the first `systemsdev-\d+` (any case), stored lower case.
  `RefKind` `ticket`.
- MR ref: `!\d+` or `MR !\d+` at a word start, only when there is no ticket
  ref. Stored as `!NNNN`. `RefKind` `mr`. A `!` inside a word
  (`content!430`) is not a ref.
- Title: the remaining text, trimmed, at most 200 runes. If empty, the ref.

## Routing

`Store.AddNote(ctx, NoteIn, Actor) (api.NoteResult, error)`, one transaction:

1. If `id` was seen in `task_events.client_id`, return the earlier result.
2. With a session, check it belongs to the actor's machine (`checkAgentOwner`).
3. Pick the task:
   - Ref → the open task (`todo`, `in_progress`, `done_proposed`) with that
     ref; for a finish note also a `done` task touched today. Else a new task.
   - No ref, with session → the task of the session's latest span if open.
     Else the session's open linked task, newest first. Else a new task.
   - No ref, no session → a new task.
4. A new task: title from the note, `ref`, `ref_url` from config, `source`
   `ticket` for a ref, else `other`, `created_by` the actor.
5. Start note: state to `in_progress` (from `todo` or `done_proposed`; a
   `done` task reopens to `in_progress`). Finish note: state to `done`.
   Dropped or merged tasks are never picked.
6. Link the session (`linkTx`) and add a span if the session's latest span
   is for another task.
7. Insert one `task_events` row: actor, note text, `client_id` = `id`.
   Written even when the state does not change (`from_state` = `to_state`).

`NoteResult{Task api.Task, Action string}`: `created`, `joined`, or `done`.

## Data model

Schema 14 adds:

```sql
CREATE TABLE task_spans (
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	started_at TEXT NOT NULL,
	PRIMARY KEY (session_id, started_at)
);
```

A merge moves spans to the target task, as it moves links.

## Time

`TaskDay` adds `active_seconds` to each task and each `TaskSession`.

- Working periods per session: from a `state_changed` event with
  `agent_state` `working` to the next event with another state, or to now
  for a session still working. Both sources (`hooks`, `plugin`) count; take
  the union. A period still open after 2 h with no event is cut at 2 h.
- Cut periods to the day in the requested time zone.
- Spans assign each moment of a session to one task: the task of the latest
  span at or before it. Before the first span, the first span's task. A
  session linked with no spans gives all its time to each linked task.
- Task time: the sum over its sessions.

## HTTP API

| Route | Auth | Body | Answer |
|---|---|---|---|
| `POST /v1/notes` | machine | `NoteIn{id, text, session_id?}`; `id` is `te_` plus 22 base64url characters; `text` 1 to 500 runes | `200`, `NoteResult` |

`400` for bad input, `409` for another machine's session (as `propose_task`), `404` for an
unknown session.

`server.toml`:

```toml
[tasks]
ticket_url = "https://example.youtrack.cloud/issue/{ref}"  # optional
mr_url     = ""                                            # optional, {n} = number
```

## CLI

`sessionhub note <text>`:

- Session: `CLAUDE_CODE_SESSION_ID` if set and valid, else none.
- Sends `POST /v1/notes` with a new `te_` ID. On a network error or a 5xx,
  queues the call in the existing client queue and prints `queued`.
- Prints `noted → <ref> <title> (<action>)`, one line.

`sessionhub task ls` gains `--json`: the `TaskDay` answer as is.

## worklog

`~/.local/bin/worklog` (not in a repo; same file on togh and blu):

- `note`: writes the spool as now, then runs `sessionhub note <text>` with a
  5 s timeout. A failure prints a warning and still exits 0.
- `report` and `tasks`: read `sessionhub task ls --json [--date D]`; on
  failure, the herdr rollup as now. Notes print under their task.

## Agent guidance

- `propose_task` and `propose_done` descriptions, the MCP `initialize`
  instructions (`internal/mcp/server.go`), `docs/CLAUDE-snippet.md` and
  `internal/join/CLAUDE-snippet.md`: record work with `worklog note`, one
  note at start, one at finish, ticket ref in the note.
- Global `CLAUDE.md`: no change.

## Testing

- [ ] `note.go`: table of the 22 notes from 2026-10-07 with expected title,
      ref, and finish. Includes `close systemsdev-17845: rebase content!430`.
- [ ] Store: ref routing, session routing, ref switch inside one session,
      finish with and without ref, retry no-op, other machine refused,
      dropped and merged never picked, reopen of a done task.
- [ ] Store: spans split time; day edges; open period capped at 2 h; merge
      moves spans; migration 13 to 14.
- [ ] Server: `POST /v1/notes` auth and validation.
- [ ] CLI: with and without `CLAUDE_CODE_SESSION_ID`; queued when offline;
      `task ls --json`.
- [ ] Replay: 2026-10-07 notes against a copy of the live DB in a scratch
      dir (`SESSIONHUB_DB` and friends set); compare with the list in Goal.

## Rollout

- [ ] Merge, deploy togh (user restarts), install on blu.
- [ ] Update `worklog` on togh and blu.
- [ ] Update agent guidance.
- [ ] After a day of use, review the **Tasks** tab and tune the parser.

Estimate: server and store ~0.5d, CLI and worklog ~2h, tests and replay ~2h.
