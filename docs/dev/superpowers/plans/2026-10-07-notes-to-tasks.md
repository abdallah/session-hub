# Notes to tasks implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `worklog note` creates and moves sessionhub tasks directly, and the day view shows each task's working time.

**Architecture:** A pure parser turns note text into a title, a ref, and a finish flag. `Store.AddNote` routes the note to a task in one transaction and records a span when a session switches tasks. `TaskDay` adds working seconds from the `state_changed` events already stored. `sessionhub note` posts to `POST /v1/notes`, queuing when offline; `worklog` calls it.

**Tech stack:** Go, SQLite (modernc), existing `internal/client` queue, Python 3 for `~/.local/bin/worklog`.

**Spec:** `docs/dev/superpowers/specs/2026-10-07-notes-to-tasks-design.md`

## Global constraints

- Schema version 14; one new table, `task_spans`, exactly as in the spec.
- Note text 1 to 500 runes; task title at most 200 runes.
- Note ID: `te_` plus 22 base64url characters (`store.ValidTaskEventID`, `client.NewTaskEventID`).
- Finish words, any case, at the start: `done:`, `done `, `finished`, `closed`, `close `.
- Ticket ref `systemsdev-\d+`, stored lower case; MR ref `!\d+` at a word start, only without a ticket ref.
- Open working period cap: 2 h.
- Config keys are flat, like the existing `telegram_*` keys: `task_ticket_url` and `task_mr_url` (the spec's `[tasks]` table, flattened). `{ref}` and `{n}` are the placeholders.
- Never run a dev binary against the live DB: set `SESSIONHUB_DB`, `SESSIONHUB_SERVER_CONFIG`, `SESSIONHUB_STATE_DIR` to a scratch dir.
- Commits end with `Claude-Session: https://claude.ai/code/session_01K417ghVVmbP5cH95TSXwvu`; no `Co-Authored-By`.
- Checks before each commit: `go test -count=1 -p 1 ./...`, `make lint`.

## Review focus

- Two sessions note the same ticket at once: both must land on one task (the ref lookup and insert are in one transaction; test it).
- A note whose text is only a ref or only a finish word (`done:`): title falls back to the ref, else the note is refused with `400`, never an empty title.
- A finish note in a session with no open task and no ref: creates a task already `done`, not an error.
- A session still working at day end: today's time stops at now; yesterday's stops at midnight.
- A merge after notes: the merged task's spans and time move to the target.

---

### Task 1: Note parser

**Files:**
- Create: `internal/tasks/note.go`
- Test: `internal/tasks/note_test.go`

**Interfaces:**
- Produces: `package tasks`; `type RefKind string` with `RefNone = ""`, `RefTicket = "ticket"`, `RefMR = "mr"`; `type Note struct { Title, Ref string; Kind RefKind; Finish bool }`; `func Parse(text string) Note`.

- [ ] **Step 1: Write `TestParseRealNotes`**, a table over the 2026-10-07 notes. Expected values:

| Text | Title | Ref | Kind | Finish |
|---|---|---|---|---|
| `admit SSO role to reporting secrets` | same | | | no |
| `done: Bobby VPN 403 — client-side network drops` | `Bobby VPN 403 — client-side network drops` | | | yes |
| `close systemsdev-17845: rebase content!430` | `rebase content!430` | `systemsdev-17845` | ticket | yes |
| `investigate ATE E2E 1h CI timeout (systemsdev-17987)` | `investigate ATE E2E 1h CI timeout` | `systemsdev-17987` | ticket | no |
| `done: ATE E2E timeout root cause found (ate-large runner 1h cap)` | `ATE E2E timeout root cause found (ate-large runner 1h cap)` | | | yes |
| `finished sessionhub tasks dashboard tab` | `sessionhub tasks dashboard tab` | | | yes |
| `posted Systems review on systemsdev-17991` | `posted Systems review on` | `systemsdev-17991` | ticket | no |
| `closed systemsdev-17845 (P81 to NetBird)` | `P81 to NetBird` | `systemsdev-17845` | ticket | yes |
| `fixed tasks final review findings` | same | | | no |
| `drop retired QA/staging WP Aurora from tofu` | same | | | no |
| `MR !2218 up: QA/staging WP Aurora dropped from tofu` | `up: QA/staging WP Aurora dropped from tofu` | `!2218` | mr | no |
| `Systemsdev-17993` | `systemsdev-17993` | `systemsdev-17993` | ticket | no |
| `done:` | `` | | | yes |

Include the other eleven 2026-10-07 notes from the spec's replay (plain start notes) as title-equals-text rows.

- [ ] **Step 2: Run** `go test ./internal/tasks/` — FAIL, `Parse` undefined.
- [ ] **Step 3: Implement `Parse`.** Order: cut a finish word; find the ticket ref, else the MR ref (`(?:^|\s)(?:MR\s+)?!(\d+)\b`); remove the ref token, a wrapping `(…)` around it, and the `MR ` before it; trim spaces and `:,-—` at both ends; strip one wrapping pair of parentheses left around the whole title; cut to 200 runes; an empty title with a ref becomes the ref.
- [ ] **Step 4: Run** `go test ./internal/tasks/` — PASS.
- [ ] **Step 5: Commit** "Parse worklog notes into a title, a ref, and a finish flag".

### Task 2: Schema 14 and `Store.AddNote`

**Files:**
- Modify: `internal/store/store.go` (`schemaVersion = 14`, `schemaV14`, migrate step)
- Create: `internal/store/notes.go`
- Modify: `internal/store/tasks.go` (`MergeTask` moves spans), `internal/api/tasks.go`
- Test: `internal/store/notes_test.go`, `internal/store/tasks_test.go` (`TestMigrateV13ToV14`, merge)

**Interfaces:**
- Consumes: `tasks.Parse` (Task 1); existing `checkAgentOwner`, `linkTx`, `insertTaskEventTx`, `eventSeenTx`, `readTask`, `newTaskID`.
- Produces:
  - `api.NoteIn{ID string \`json:"id"\`; Text string \`json:"text"\`; SessionID string \`json:"session_id,omitempty"\`}`
  - `api.NoteResult{Task api.Task \`json:"task"\`; Action string \`json:"action"\`}`; `api.NoteCreated = "created"`, `api.NoteJoined = "joined"`, `api.NoteDone = "done"`
  - `store.RefURLs{Ticket, MR string}` with `func (u RefURLs) For(ref string, kind tasks.RefKind) string` (`{ref}` → the ref, `{n}` → the MR number; empty template → `""`)
  - `func (s *Store) AddNote(ctx context.Context, in api.NoteIn, a Actor, urls RefURLs) (api.NoteResult, error)`
  - `func spanTaskTx(ctx, tx, sessionID string) (taskID string, err error)` (latest span's task, `""` if none)

- [ ] **Step 1: Write the failing tests** in `notes_test.go`, using the `controlEnv` helpers from `tasks_test.go`:
  - `TestNoteRefCreatesThenJoins`: note `investigate X (systemsdev-1)` from session A → `created`, state `in_progress`, `source` `ticket`, `ref_url` from `RefURLs{Ticket: "https://yt/issue/{ref}"}` = `https://yt/issue/systemsdev-1`; same ref from session B → `joined`, same task ID, both sessions linked.
  - `TestNoteSessionTask`: session A note `design X` → created; `tune X` → joined same task; `new ticket systemsdev-2` → different task; then `back to X` (no ref) → joins the systemsdev-2 task (latest span).
  - `TestNoteFinish`: `done: X` with an open session task → `done`, state `done`; `closed systemsdev-1` from a session with another open task → finishes the systemsdev-1 task only; a finish note with no task → new task in state `done`.
  - `TestNoteReopensDone`: start note with ref of a task done 1 h ago → `in_progress`.
  - `TestNoteRetry`: same `ID` twice → second returns the same task and action, one `task_events` row.
  - `TestNoteSkipsDroppedAndMerged`: ref matching only a dropped task → new task.
  - `TestNoteOtherMachine`: agent actor of machine 2 on machine 1's session → `ErrWrongMachine`.
  - `TestNoteValidation`: empty text, 501 runes, bad ID, `done:` alone → invalid error.
  - `TestNoteConcurrentRef`: two goroutines, two sessions, same ref → one task.
  - `TestMigrateV13ToV14` (pattern of `TestMigrateV12ToV13`) and `TestMergeMovesSpans`.
- [ ] **Step 2: Run** `go test ./internal/store/ -run 'Note|V13ToV14|MergeMovesSpans'` — FAIL.
- [ ] **Step 3: Implement.** `schemaV14` is the spec's `CREATE TABLE task_spans`. `AddNote` follows the spec's Routing steps 1 to 7, with these choices:
  - "Done task touched today" = `done` with `updated_at` within the last 24 h.
  - Ref match ignores case and excludes `merged_into IS NOT NULL` and `dropped`.
  - State moves bypass `taskMoves`; set `state` directly and write the event with the real `from_state`.
  - A span row is added when `spanTaskTx` differs from the chosen task; `started_at` = now.
  - Retry: look up the event by `client_id`; `Action` = `created` if its `from_state` is `''`, `done` if `to_state` is `done`, else `joined`.
  `MergeTask`: `UPDATE OR IGNORE task_spans SET task_id = ? WHERE task_id = ?`, then delete leftovers, next to the existing `task_sessions` move.
- [ ] **Step 4: Run** the tests — PASS. Then `go test -count=1 -p 1 ./...`.
- [ ] **Step 5: Commit** "Route worklog notes to tasks (schema 14)".

### Task 3: Working time in the day view

**Files:**
- Create: `internal/store/tasktime.go`
- Modify: `internal/store/taskday.go`, `internal/api/tasks.go`
- Test: `internal/store/tasktime_test.go`

**Interfaces:**
- Consumes: `task_spans` (Task 2); `events` rows with `kind = 'state_changed'`, `payload_json.agent_state`, `source` `hooks` or `plugin`.
- Produces: `api.Task.ActiveSeconds int64 \`json:"active_seconds"\``, `api.TaskSession.ActiveSeconds int64 \`json:"active_seconds"\``; `func workingPeriods(ctx, q querier, sessionID string, from, to time.Time) ([]period, error)`; `type period struct{ from, to time.Time }`.

- [ ] **Step 1: Write the failing tests:**
  - `TestWorkingPeriodsUnion`: hooks working 10:00–10:30, plugin working 10:20–10:50 → one period 10:00–10:50.
  - `TestWorkingPeriodsCap`: working at 10:00, no later event, now 15:00 → 10:00–12:00.
  - `TestWorkingPeriodsStillWorking`: working at 14:30, now 15:00 → ends 15:00.
  - `TestTaskDaySplitBySpans`: session works 9:00–11:00; span to task A at 9:30, to task B at 10:00 → A 3600 s (9:00–10:00, the time before the first span goes to the first span's task), B 3600 s.
  - `TestTaskDayNoSpans`: linked by hand, no spans, works 1 h → 3600 s on the task.
  - `TestTaskDayEdges`: works 23:30 to 00:30 in `Europe/Berlin` → 1800 s on each day.
- [ ] **Step 2: Run** `go test ./internal/store/ -run 'WorkingPeriods|TaskDaySplit|TaskDayNoSpans|TaskDayEdges'` — FAIL.
- [ ] **Step 3: Implement.** Per source, walk events in `ts` order: a `working` event opens a period, any other `agent_state` closes it; close at `min(next event, open + 2h, now)`. Union across sources, clip to the day. In `TaskDay`, for each listed task's sessions, assign each period's seconds by spans (latest span at or before each moment; before the first span, the first span's task); a session with no spans gives all its seconds to each linked task. Fill both `ActiveSeconds` fields.
- [ ] **Step 4: Run** — PASS; full suite.
- [ ] **Step 5: Commit** "Show working time per task in the day view".

### Task 4: `POST /v1/notes`, config, client

**Files:**
- Modify: `internal/server/config.go`, `internal/server/routes.go`, `internal/server/tasks.go`, `internal/client/tasks.go`, `docs/server.md` (route table, the two keys)
- Test: `internal/server/notes_test.go`, `internal/server/config_test.go`, `internal/server/auth_test.go` (route table entry)

**Interfaces:**
- Consumes: `Store.AddNote`, `store.RefURLs` (Task 2).
- Produces: `Config.TaskTicketURL`, `Config.TaskMRURL` (toml `task_ticket_url`, `task_mr_url`; must be `http`/`https` with a host when set, else a startup error); route `{"POST", "/v1/notes", accessTasks, s.actor(api.HeaderActionTasks, s.addNote)}`; `func (c *Client) AddNote(ctx context.Context, in api.NoteIn) (api.NoteResult, error)`.

- [ ] **Step 1: Write the failing tests:** machine token with its own session → `200` and a `NoteResult`; another machine's session → `409` (the existing `ErrWrongMachine` mapping); unknown session → `404`; bad ID → `400`; config with `task_ticket_url = "ftp://x"` → load error; with `https://yt/issue/{ref}` → `ref_url` filled.
- [ ] **Step 2: Run** `go test ./internal/server/ -run 'Note|Config'` — FAIL.
- [ ] **Step 3: Implement** `addNote` like `createTask` (`decode`, `actorOf(p, in.SessionID)`, `storeError`, `writeJSON` `200`), passing `store.RefURLs{s.cfg.TaskTicketURL, s.cfg.TaskMRURL}`. Client `AddNote` posts to `/v1/notes`.
- [ ] **Step 4: Run** — PASS; full suite.
- [ ] **Step 5: Commit** "Add POST /v1/notes".

### Task 5: `sessionhub note` and `task ls --json`

**Files:**
- Create: `internal/cli/note.go`, `internal/cli/note_test.go`
- Modify: `internal/client/replay.go` (`OpNote`), `internal/cli/cli.go` (`notes noteAPI`, `queue` on `env`), `internal/cli/tasks.go` (`--json`), `cmd/sessionhub/main.go` (`"note": cli.RunNote`), `docs/cli.md`
- Test: `internal/client/replay_test.go`, `internal/cli/tasks_test.go`

**Interfaces:**
- Consumes: `Client.AddNote` (Task 4), `client.NewTaskEventID`, `client.IsRetryable`, `client.IsNotFound`, `client.DefaultQueue`.
- Produces: `client.OpNote = "note"` (Body `api.NoteIn`; `SessionID` optional); `func RunNote(ctx context.Context, args []string) error`; env field `getenv func(string) string`.

- [ ] **Step 1: Write the failing tests:**
  - `TestNoteSendsSession`: `CLAUDE_CODE_SESSION_ID=aab78df9-…` → `NoteIn.SessionID` set, ID matches `te_`, output `noted → systemsdev-1 investigate X (created)\n`.
  - `TestNoteNoSession`: unset or invalid (`../x`) → no session ID.
  - `TestNoteQueuesWhenOffline`: fake returns a retryable error, then `IsNotFound` → one queued `OpNote` item each, output `queued: <text>\n`, exit nil.
  - `TestNoteRejected`: `400` → error, nothing queued.
  - `TestNoteUsage`: no text → usage error.
  - `TestReplayNote`: a queued `OpNote` item calls `AddNote` with the same body.
  - `TestTaskLsJSON`: `task ls --json` prints the `TaskDay` as JSON, decodable back to the same value.
- [ ] **Step 2: Run** `go test ./internal/cli/ ./internal/client/ -run 'Note|TaskLsJSON'` — FAIL.
- [ ] **Step 3: Implement.** Text is the args joined with spaces. Session ID is valid when it matches the server's session ID rule (`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`). Output line: `noted → ` + ref + ` ` (when a ref) + title + ` (` + action + `)`, cleaned with `clean(…, termtext.TitleWidth)`.
- [ ] **Step 4: Run** — PASS; full suite; `make lint`.
- [ ] **Step 5: Commit** "Add sessionhub note and task ls --json".

### Task 6: Agent guidance

**Files:**
- Modify: `internal/mcp/server.go` (initialize instructions), `internal/mcp/tasks.go` (descriptions of `propose_task`, `propose_done`), `docs/CLAUDE-snippet.md`, `internal/join/CLAUDE-snippet.md`
- Test: existing snippet sync test (run it); `internal/mcp` tests that pin the instruction text, updated

- [ ] **Step 1: Change the text.** Instructions: "Record work with `worklog note`: one note when you start a piece of work, one starting with `done:` when you finish it, with the ticket ref (for example systemsdev-12345 or MR !1234) in the note. sessionhub turns notes into tasks. Use `propose_task` only when you cannot run shell commands." `propose_done`: same last sentence.
- [ ] **Step 2: Run** `go test ./internal/mcp/ ./internal/join/` — PASS after updating pinned strings.
- [ ] **Step 3: Commit** "Point agents at worklog note for tasks".

### Task 7: worklog calls sessionhub

**Files:**
- Modify: `~/.local/bin/worklog` on togh, then copy to blu (not in a repo; back up to the scratchpad first)

- [ ] **Step 1: `note`:** after the spool append, run `sessionhub note <text>` with a 5 s timeout, passing the environment; print its output line; on any failure print `worklog: sessionhub note failed: <reason>` to stderr; exit 0 either way.
- [ ] **Step 2: `report` and `tasks`:** run `sessionhub task ls --json [--date D]` (10 s timeout). On success, print In progress and Done with `human(active_seconds)`, ref, title, and machines of linked sessions; `tasks --json` emits the same list. On failure, the current herdr rollup, with a first line `sessionhub unreachable; showing herdr samples`. `--all` keeps showing the raw herdr rollup.
- [ ] **Step 3: Verify** with a scratch sessionhub (env vars from Global constraints): `worklog note "test systemsdev-1"` prints `noted → …`; `worklog report` lists the task; stopping the scratch server gives the fallback line.
- [ ] **Step 4: Copy** to blu with `scp` + `mv`; `md5sum` matches on both.

### Task 8: Replay check and rollout

- [ ] **Step 1: Replay.** Copy the live DB with `sqlite3 … ".backup <scratch>/replay.db"`; run a scratch server on it; send the 22 notes of 2026-10-07 from blu's spool through `sessionhub note` with `CLAUDE_CODE_SESSION_ID` set from the session that wrote each (match by pane and time against `events`; leave unset when unknown). Compare `task ls --date 2026-10-07` with the spec's Goal list. Record mismatches in the PR description; fix parser rules only for clear misses.
- [ ] **Step 2: Merge** to `main`, push, build; blu: install binary, `install-plugin`, `install-mod`; togh: hand the user the restart command from memory `sessionhub-deploy-on-togh`.
- [ ] **Step 3: Live check:** one `worklog note` from a real session shows on the **Tasks** tab with time after a few minutes.
- [ ] **Step 4:** Note in memory `tasks-feature-tuning`: review the tab with the user on 2026-10-08.
