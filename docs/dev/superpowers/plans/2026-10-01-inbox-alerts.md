# Inbox alerts and the herdr inbox pane implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the inbox reach you: a Telegram message and a herdr
notification when a session blocks, a seen pane clears its **Finished** item,
an inbox count in the herdr sidebar, and a herdr pane (`sessionhub inbox --watch`)
whose **Enter** jumps to a session on any machine.

**Architecture:** Schema v7 adds `inbox_alerts`. The store dismisses a
**Finished** item inside the same transaction that moves the agent state from
`done` to `idle`, on both state paths. A notifier goroutine in `sessionhub server`
reads `store.Inbox` every 15 seconds and sends one Telegram `sendMessage` per
new block, recording it only after Telegram answers `ok: true`. The plugin's
event hook calls herdr's `notification.show` for a blocked pane, and the
watcher's heartbeat writes an `inbox` token on every workspace with
`workspace.report_metadata`. `sessionhub inbox --watch` is a pure state machine
(state and key in, state and action out) driven by a small terminal loop that
uses `stty`; its **Enter** calls `resume.Jumper`, which reuses `sessionhub resume`'s
local logic and, for another machine, runs `ssh -o BatchMode=yes` and opens a
local herdr tab running `herdr --remote <herdr_host>`.

**Tech Stack:** Go 1.25, `modernc.org/sqlite`, standard library only. No new
dependencies. herdr 0.9.3 (socket protocol 22). Dashboard: one embedded HTML
page, pure functions tested under `node`.

**Spec:** `docs/dev/superpowers/specs/2026-10-01-inbox-alerts-design.md`

## Global Constraints

- Schema version 7, upgraded in place like versions 2 to 6. It adds the table
  `inbox_alerts`: `session_id` (primary key, foreign key to `sessions` with
  `ON DELETE CASCADE`), `since` (the `since` alerted on), and `sent_at`.
- Telegram settings: `server.toml` keys `telegram_bot_token` and
  `telegram_chat_id`, env overrides `SESSIONHUB_TELEGRAM_BOT_TOKEN` and
  `SESSIONHUB_TELEGRAM_CHAT_ID`. When either is empty, alerts are off and the server
  logs one line at startup saying so. The token is never logged or returned
  by any endpoint.
- The notifier runs every 15 seconds. For each **Blocked** item that has been
  blocked for at least 30 seconds (`now - since >= 30s`), is not hidden by
  triage, and whose `since` is later than the last alert recorded for that
  session, it sends one message and records it. The row is written only after
  Telegram answers `ok: true`.
- Message, plain text, no `parse_mode`:
  ```
  ⏸ Blocked: <title>
  <machine> · blocked <age> ago
  <waiting_on items, or the recap, cut to 300 characters>
  ```
  Inline keyboard of URL buttons: **Open inbox** (`<public_url>/#inbox`) and,
  when the session has a Remote Control link, **Remote Control** (`rc_url`).
  Every server-supplied string passes the free-text cleaning
  (`termtext.Clean`) before it is sent.
- Failures: a failed send (network error, non-`ok` response, HTTP 429) records
  nothing; on HTTP 429 the notifier waits `retry_after` seconds before its
  next send; each failure is logged at most once a minute, without the token
  or the request URL; at most one alert per session per tick and at most 10
  sends per tick; the notifier stops when the server stops.
- The dashboard selects the **Inbox** tab when the URL fragment is `#inbox`.
- herdr notification on a `pane.agent_status_changed` event whose new status
  is `blocked`: title `Blocked: <title>`, body `<machine>`, sound `request`.
  `<title>` is the pane's title, cleaned and cut to 80 characters. A failure
  is logged to the watcher log and ignored.
- Auto-dismiss: when the agent state moves from `done` to `idle`, the store
  classifies the session; if it is **Finished**, it writes the triage row as a
  dismiss with `triaged_since` = that item's `since` (`turn_ended_at`), in the
  same transaction, on both the `state_changed` event path and the snapshot
  upsert path. **Blocked** and **Waiting on you** items are never touched.
  Hooks-only sessions (`working` to `idle`) never auto-dismiss.
- Sidebar: on every 60-second heartbeat the watcher reads the inbox counts and
  sets the workspace token `inbox` (source `sessionhub`) to `<total>`, plus
  ` · <n> blocked` when any are blocked (for example `3 · 1 blocked`), on each
  workspace of its herdr session. An empty inbox clears the token. A workspace
  whose text is unchanged since the last heartbeat gets no call. If the inbox
  read fails, the previous value stays.
- `sessionhub inbox --watch`: the same groups and lines as `sessionhub inbox`, a cursor on
  one row, refreshed every 5 seconds. **j** / **k** or the arrow keys move;
  **Enter** jumps; **d** dismisses; **s** then **1**, **4**, or **t** snoozes
  for 1 hour, 4 hours, or until 9:00 tomorrow; **r** refreshes; **q** or
  **Ctrl+C** quits. A status line at the bottom shows the last refresh time
  and the last error. Alternate screen and raw mode through `stty`, restored
  on exit, including on a signal. If stdin or stdout is not a terminal, it
  exits with an error.
- Jump on **Enter**: on this machine, the same logic as `sessionhub resume <id>`
  without printing the resume command. On another machine:
  `ssh -o BatchMode=yes <ssh_host> '~/.local/bin/sessionhub resume <uuid>'` with a
  15-second limit; then focus the local herdr tab named `sessionhub:<machine>`, or
  create it running `herdr --remote <herdr_host>` and focus it. If the machine
  has no `herdr_host`, or the `ssh` step fails, show the resume command in the
  status line instead.
- herdr plugin entrypoint `inbox` runs `sessionhub inbox --watch`.
  `sessionhub plugin open-inbox` runs `herdr plugin pane open --plugin sessionhub
  --entrypoint inbox --placement split --direction right --focus`, using
  `HERDR_BIN_PATH` when set.
- No new dependencies. No secrets in logs, test output, or docs examples: the
  docs use `<bot-token>` and `<chat-id>`, and tests use the obvious
  placeholder `123456:TEST-PLACEHOLDER-TOKEN`.
- herdr facts confirmed on this machine (herdr 0.9.3, `herdr api schema
  --json`, protocol 22); use these names exactly:
  - Socket `notification.show`, params `{title, body?, sound?, position?}`,
    `sound` one of `none`, `done`, `request`. Result
    `{"type":"notification_show","shown":bool,"reason":...}`, reason one of
    `shown`, `disabled`, `rate_limited`, `no_foreground_client`, `busy`.
  - Socket `workspace.report_metadata`, params `{workspace_id, source,
    tokens}` (`tokens` required; a `null` value clears that token; token names
    match `^[A-Za-z0-9_-]{1,32}$`, at most 16). Result `{"type":"ok"}`. CLI
    equivalent: `herdr workspace report-metadata <workspace_id> --source ID
    [--token NAME=VALUE] [--clear-token NAME]`.
  - Socket `tab.list` (params `{workspace_id?}`; without it, every tab of the
    session) returns `{"type":"tab_list","tabs":[{tab_id, workspace_id,
    number, label, focused, pane_count, agent_status}]}`. `tab.focus` takes
    `{tab_id}`. `tab.create` takes `{workspace_id?, cwd?, label?, env?,
    focus}` and returns `{"type":"tab_created","tab":{...},"root_pane":{...}}`.
    `tab.create` cannot run a command, so the jump runs `herdr pane run
    <pane_id> <command>` (CLI, confirmed in `herdr pane --help`; it sends the
    text and Enter atomically).
  - `session.snapshot` carries `workspaces[]` with `workspace_id` and `label`
    (in `testdata/herdr/socket/session-snapshot.ndjson`).
  - `herdr plugin pane open` flags: `--plugin`, `--entrypoint`, `--placement
    overlay|split|tab|zoomed`, `--direction right|down`, `--focus`. Manifest
    pane `placement` accepts `overlay`, `popup`, `split`, `tab`, `zoomed`.
  - `herdr --remote <ssh-target>` attaches to a remote herdr server.
  - The sidebar shows workspace metadata tokens as `$name` in
    `[ui.sidebar.spaces] rows` (`herdr --default-config`: "Custom values
    reported through workspace metadata use a $name token"), so the inbox
    count shows with `$inbox`. Pane tokens go in `[ui.sidebar.agents]`, as
    `$hub_summary` already does.
  - Not confirmed: whether a `[[keys.command]]` binding can run
    `sessionhub plugin open-inbox`. The docs point to the plugin action instead.
- Resolved spec ambiguities (Task 8 records them in `docs/dev/PLAN.md`):
  1. herdr notification and sidebar use the socket methods the code already
     calls the same way (`internal/herdr.Client`), not `exec herdr`. The
     spec's CLI lines are the same calls.
  2. The `pane.agent_status_changed` payload has no title (captured in
     `testdata/herdr/events/pane.agent_status_changed-blocked.json`), so the
     title comes from the event's `title` when set, else `pane.get`'s
     `terminal_title_stripped`, else `terminal_title`, else `Claude`.
  3. `<machine>` in the herdr notification is the client config's `machine`,
     else the host name.
  4. A **Blocked** item carries no `waiting_on` (only **Waiting** items do).
     The message's third line is the latest report's `waiting_on` items when
     no prompt came after that report, else the recap; it is left out when
     both are empty.
  5. The **Remote Control** button is added only for an `https://` link,
     because Telegram rejects other URL buttons.
  6. The notifier stops the tick at the first failed send; the rest wait for
     the next tick. A `RecordAlert` failure after a successful send is logged,
     and the next tick may send that alert again.
  7. HTTP 429 without `retry_after` waits 30 seconds.
  8. The sidebar skip rule is per workspace, so a new workspace gets the count
     at the next heartbeat, and a failed report is retried at the next one.
     The first heartbeat after a watcher starts reports every workspace, even
     an empty inbox (a clear), so a stale count from an older watcher goes.
  9. "No `herdr_host`" means the server's machine list has no entry for the
     session's machine, or the entry has an empty `herdr_host` or `ssh_host`
     that `ssh` or herdr would read as an option. The server fills
     `herdr_host` when a machine is added, so in practice this is a removed
     machine or a failed list.
  10. An existing `sessionhub:<machine>` tab is focused as is, even if its
      `herdr --remote` has exited.
  11. The manifest also gets an action `inbox` ("sessionhub: inbox") that runs
      `sessionhub plugin open-inbox`, so you can bind it like the picker.
  12. **Esc** cancels a pending snooze; **q** and **Ctrl+C** quit even while a
      snooze is pending. Any key other than **1**, **4**, or **t** after **s**
      cancels the snooze.
  13. The URL fragment `#sessions` selects the **Sessions** tab, and the page
      also follows later `hashchange` events.
  14. Rollback is restoring the database backup only. A v6 binary refuses a
      v7 database (`v > schemaVersion`).
- Follow `docs/dev/DEFINITION-OF-DONE.md`: `make test` and `make lint` green after
  every commit, and component docs (`docs/server.md`, `docs/plugin.md`,
  `docs/cli.md`, `docs/client.md`) updated in the commit of the task that owns
  the change. Task 8 does `README.md`, `docs/dev/SPEC.md`, and `docs/dev/PLAN.md`. Run
  `gofmt -w` on every Go file you edit before `make lint`; the plan's code
  blocks are not guaranteed to be aligned the way `gofmt` wants.
- On this machine, parallel test runs can hit `address already in use`. If a
  run fails that way, wait 60 seconds and run it again before you debug.
- Anchor every edit on the quoted text, not on line numbers, and `git add`
  only the files your task lists.
- Commit trailer on every commit:
  `Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h`.

## Review Focus

1. **The bot token in a log line.** `http.Client` errors are `*url.Error`,
   whose text holds the full request URL, and the URL holds
   `/bot<token>/`. A network error logged with `err.Error()` leaks the token.
   Expected: no log line ever holds the token, on a network error, a 500, or
   a 429. Pinned in Task 2 (`TestNotifierNeverLogsToken`).
2. **A `done` to `idle` that is not a seen pane.** The watcher repeats `idle`
   every minute, hooks-only sessions go `working` to `idle`, and sessions from
   before schema 6 have a null `turn_ended_at`. Expected: only a real `done`
   to `idle` on a **Finished** item writes a triage row, and a later turn
   comes back. Pinned in Task 1 (`TestAutoDismissSeenSnapshot`,
   `TestAutoDismissLeavesOtherItems`).
3. **A blocked session whose `since` never moves.** A session that stays
   blocked across many heartbeats keeps one `since` (it may be `started_at`
   for a row from before `blocked_at` existed). Expected: one alert, not one
   per tick, and no alert after a dismiss. Pinned in Task 2
   (`TestNotifierAlertsOncePerBlock`, `TestNotifierSkipsTriagedAndOtherGroups`).
4. **The terminal after a crash.** If the watch loop panics or gets a signal,
   a terminal left in raw mode on the alternate screen is unusable. Expected:
   `stty` gets the saved settings back and the alternate screen is left on
   every way out, including a panic. Pinned in Task 6
   (`TestRawTerminalRestores`).
5. **The cursor when its row leaves.** A dismiss or a refresh can remove the
   selected session. Expected: the cursor stays on the same session while it
   is listed, else moves to the row now at its old position, else to the last
   row, and **Enter** never acts on a session that left. Pinned in Task 6
   (`TestWatchKeepsCursor`).

## Dependencies and parallelism

| Task | Needs | Can run alongside |
|---|---|---|
| 1 Store: schema v7, alerts, auto-dismiss | none | 3, 4 |
| 2 Server: Telegram notifier | 1 | 3, 4 |
| 3 Dashboard: `#inbox` | none | 1, 2, 4, 5 |
| 4 Plugin: herdr notification for Blocked | none | 1, 2, 3 |
| 5 Plugin: inbox count in the sidebar | 4 | 1, 2, 3 |
| 6 CLI: `sessionhub inbox --watch` | none | 1 to 5 |
| 7 Jump and `sessionhub plugin open-inbox` | 4, 5, 6 | none |
| 8 README, SPEC, and PLAN | all | none |

Tasks 1, 2, and 3 all edit `docs/server.md`; tasks 4, 5, and 7 all edit
`internal/herdr/herdr.go`, `internal/herdr/herdr_test.go`, `docs/plugin.md`,
and `docs/client.md` (4 and 5 also share `internal/plugin`); tasks 6 and 7
both edit `internal/cli/inbox.go` and `docs/cli.md`. Run each group in
order.

---

### Task 1: Store: schema v7, alerts, and auto-dismiss

**Files:**
- Modify: `internal/store/store.go` (`schemaVersion = 7`, `schemaV7`,
  migration step)
- Create: `internal/store/alerts.go` (`LastAlerts`, `RecordAlert`)
- Modify: `internal/store/inbox.go` (`autoDismissSeenTx`)
- Modify: `internal/store/sessions.go` (`upsertTx`, `AddEvent`)
- Modify: `internal/store/concurrency_test.go` (`rollbackV7`, chained from
  `rollbackV6`)
- Modify: `internal/store/inbox_test.go` (`TestMigrateV5ToV6` expects
  `schemaVersion`)
- Modify: `internal/store/store_test.go` (`user_version` `7`, the
  `inbox_alerts` columns)
- Create: `internal/store/alerts_test.go`
- Modify: `docs/server.md` (Inbox section, Database section)

**Interfaces:**
- Consumes: `classifyInbox`, `inboxState`, `sessionSelect`,
  `(*Store).scanSession`, `formatTS`, `parseTS`, `parseNullTS`,
  `newControlEnv`, `controlEnv.session`, `controlEnv.setState`,
  `controlEnv.setStateAt`, `controlEnv.sendReport`, `controlEnv.inboxFor`,
  `controlEnv.count`, `openTemp`.
- Produces:
  - `func (s *Store) LastAlerts(ctx context.Context) (map[string]time.Time, error)`
  - `func (s *Store) RecordAlert(ctx context.Context, id string, since time.Time) error`
    (returns an error wrapping `ErrNotFound` for an unknown session)
  - `func (s *Store) autoDismissSeenTx(ctx context.Context, tx *sql.Tx, id string, now time.Time) error`
  - test helper `func rollbackV7(t *testing.T, s *Store)`

- [ ] **Step 1: Write the failing tests**

Create `internal/store/alerts_test.go`:

```go
package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// triage reads session id's triage row. ok is false when there is none;
// dismiss is true when snooze_until is null.
func (e *controlEnv) triage(id string) (since time.Time, dismiss, ok bool) {
	e.t.Helper()
	var ts string
	var until sql.NullString
	err := e.s.db.QueryRow(`SELECT triaged_since, snooze_until FROM inbox_triage WHERE session_id = ?`, id).Scan(&ts, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	t, err := parseTS(ts)
	if err != nil {
		e.t.Fatal(err)
	}
	return t, !until.Valid, true
}

// snapshot sends a herdr snapshot with session id in pane p1 in state.
func (e *controlEnv) snapshot(id, state string) {
	e.t.Helper()
	res, err := e.s.ReconcileHerdr(context.Background(), e.tower.ID, api.HerdrSessionsPut{HerdrSession: "default",
		Sessions: []api.SessionUpsert{{ID: id, HerdrPane: "p1", AgentState: state}}})
	if err != nil || res.Upserted != 1 {
		e.t.Fatalf("snapshot: %+v %v", res, err)
	}
}

func TestMigrateV6ToV7(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	tok, _, err := s.AddMachine(ctx, "tower", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.MachineByToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertSession(ctx, m.ID, api.SessionUpsert{ID: "s1", Source: api.SourceHooks, AgentState: "idle"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Triage(ctx, "s1", s.Now(), nil); err != nil {
		t.Fatal(err)
	}
	rollbackV7(t, s)
	if _, err := s.db.Exec("PRAGMA user_version = 6"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var v int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version %d %v, want %d", v, err, schemaVersion)
	}
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM inbox_triage`).Scan(&n); err != nil || n != 1 {
		t.Errorf("triage rows after upgrade: %d %v, want 1", n, err)
	}
	if err := s2.RecordAlert(ctx, "s1", s2.Now()); err != nil {
		t.Errorf("RecordAlert after upgrade: %v", err)
	}
}

func TestAlerts(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "")
	got, err := e.s.LastAlerts(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("LastAlerts on an empty table: %v %v", got, err)
	}
	first := e.clock.Now().Add(-time.Minute)
	if err := e.s.RecordAlert(ctx, "s1", first); err != nil {
		t.Fatal(err)
	}
	got, err = e.s.LastAlerts(ctx)
	if err != nil || len(got) != 1 || !got["s1"].Equal(first) {
		t.Fatalf("LastAlerts %v %v, want s1 at %v", got, err, first)
	}
	var sent string
	if err := e.s.db.QueryRow(`SELECT sent_at FROM inbox_alerts WHERE session_id = 's1'`).Scan(&sent); err != nil || sent != formatTS(e.clock.Now()) {
		t.Errorf("sent_at %q %v, want %s", sent, err, formatTS(e.clock.Now()))
	}
	// A later alert replaces the row.
	second := e.clock.Now()
	e.clock.Advance(time.Second)
	if err := e.s.RecordAlert(ctx, "s1", second); err != nil {
		t.Fatal(err)
	}
	got, _ = e.s.LastAlerts(ctx)
	if !got["s1"].Equal(second) {
		t.Errorf("after a second alert: %v, want %v", got["s1"], second)
	}
	if n := e.count(`SELECT COUNT(*) FROM inbox_alerts`); n != 1 {
		t.Errorf("%d alert rows, want 1", n)
	}
	// An unknown session is not found and writes nothing.
	if err := e.s.RecordAlert(ctx, "nope", second); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v, want ErrNotFound", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM inbox_alerts WHERE session_id = 'nope'`); n != 0 {
		t.Errorf("unknown session got %d rows", n)
	}
	// The row goes with its session.
	if _, err := e.s.db.Exec(`DELETE FROM sessions WHERE id = 's1'`); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT COUNT(*) FROM inbox_alerts`); n != 0 {
		t.Errorf("%d alert rows after the session was deleted, want 0", n)
	}
}

// TestAutoDismissSeenEvent: herdr marks a finished pane seen (done to idle)
// with a state_changed event. The Finished item is dismissed at its since,
// and the next turn comes back.
func TestAutoDismissSeenEvent(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "")
	e.setState("s1", "working")
	e.clock.Advance(time.Minute)
	e.setState("s1", "done")
	end := e.clock.Now()
	if it := e.inboxFor("s1"); it == nil || it.Group != api.InboxFinished {
		t.Fatalf("after done: %+v, want finished", it)
	}
	if _, _, ok := e.triage("s1"); ok {
		t.Fatal("triage row before the pane was seen")
	}
	// An idle event older than the applied state is stored but not applied:
	// no dismiss.
	e.setStateAt("s1", "idle", end.Add(-time.Second))
	if _, _, ok := e.triage("s1"); ok {
		t.Fatal("a stale idle event dismissed the item")
	}
	e.clock.Advance(time.Minute)
	e.setState("s1", "idle")
	if e.inboxFor("s1") != nil {
		t.Error("seen finished item still in the inbox")
	}
	if since, dismiss, ok := e.triage("s1"); !ok || !dismiss || !since.Equal(end) {
		t.Errorf("triage row since=%v dismiss=%v ok=%v, want a dismiss at %v", since, dismiss, ok, end)
	}
	// The next turn has a later since and comes back.
	e.clock.Advance(time.Minute)
	e.setState("s1", "working")
	e.clock.Advance(time.Minute)
	e.setState("s1", "done")
	if it := e.inboxFor("s1"); it == nil || !it.Since.Equal(e.clock.Now()) {
		t.Errorf("next turn: %+v, want finished since %v", it, e.clock.Now())
	}
}

// TestAutoDismissSeenSnapshot: the same on the snapshot path, where herdr's
// heartbeat repeats the state every minute. Only the move dismisses.
func TestAutoDismissSeenSnapshot(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "s1", "p1")
	e.snapshot("s1", "working")
	e.clock.Advance(time.Minute)
	e.snapshot("s1", "done")
	end := e.clock.Now()
	e.clock.Advance(time.Minute)
	e.snapshot("s1", "done")
	if _, _, ok := e.triage("s1"); ok {
		t.Fatal("a repeated done dismissed the item")
	}
	e.snapshot("s1", "idle")
	if since, dismiss, ok := e.triage("s1"); !ok || !dismiss || !since.Equal(end) {
		t.Fatalf("triage row since=%v dismiss=%v ok=%v, want a dismiss at %v", since, dismiss, ok, end)
	}
	if e.inboxFor("s1") != nil {
		t.Error("seen finished item still in the inbox")
	}
	// Later idle heartbeats leave the row alone.
	e.clock.Advance(time.Minute)
	e.snapshot("s1", "idle")
	if since, _, _ := e.triage("s1"); !since.Equal(end) {
		t.Errorf("idle heartbeat moved triaged_since to %v, want %v", since, end)
	}
	// So does the plain upsert path.
	if _, err := e.s.UpsertSession(context.Background(), e.tower.ID, api.SessionUpsert{ID: "s1", Source: api.SourcePlugin, AgentState: "idle"}); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT COUNT(*) FROM inbox_triage`); n != 1 {
		t.Errorf("%d triage rows, want 1", n)
	}
}

// TestAutoDismissLeavesOtherItems: done to idle on a Waiting item, working to
// idle (hooks-only), blocked to idle, and done to idle with no turn end (a
// session from before schema 6) write no triage row.
func TestAutoDismissLeavesOtherItems(t *testing.T) {
	e := newControlEnv(t)
	for _, id := range []string{"wait", "hooks", "blk", "old"} {
		e.session(e.tower, id, "")
	}
	// wait: finished a turn, then reported waiting_on, which outranks Finished.
	e.setState("wait", "working")
	e.clock.Advance(time.Minute)
	e.setState("wait", "done")
	e.clock.Advance(time.Minute)
	e.sendReport("wait", "pick a name")
	// hooks: working to idle, never done.
	e.setState("hooks", "working")
	e.clock.Advance(time.Minute)
	e.setState("hooks", "idle")
	// blk: blocked to idle ends a turn, but is not a seen pane.
	e.setState("blk", "blocked")
	e.clock.Advance(time.Minute)
	e.setState("blk", "idle")
	// old: done without a turn end.
	e.setState("old", "done")
	e.clock.Advance(time.Minute)
	e.setState("wait", "idle")
	e.setState("hooks", "idle")
	e.setState("old", "idle")

	for _, id := range []string{"wait", "hooks", "blk", "old"} {
		if _, _, ok := e.triage(id); ok {
			t.Errorf("%s: triage row written", id)
		}
	}
	if it := e.inboxFor("wait"); it == nil || it.Group != api.InboxWaiting {
		t.Errorf("wait: %+v, want waiting", it)
	}
	for _, id := range []string{"hooks", "blk"} {
		if it := e.inboxFor(id); it == nil || it.Group != api.InboxFinished {
			t.Errorf("%s: %+v, want finished", id, it)
		}
	}
	if it := e.inboxFor("old"); it != nil {
		t.Errorf("old: %+v, want no item", it)
	}
}
```

In `internal/store/concurrency_test.go`, replace:

```go
func rollbackV6(t *testing.T, s *Store) {
	t.Helper()
	for _, q := range []string{"DROP TABLE inbox_triage",
```

with:

```go
func rollbackV6(t *testing.T, s *Store) {
	t.Helper()
	rollbackV7(t, s)
	for _, q := range []string{"DROP TABLE inbox_triage",
```

and add after the end of `rollbackV6`:

```go
// rollbackV7 removes what schema v7 added, so a test can set user_version
// to 6 or lower and reopen the file as an older release left it.
func rollbackV7(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec("DROP TABLE inbox_alerts"); err != nil {
		t.Fatalf("DROP TABLE inbox_alerts: %v", err)
	}
}
```

In `internal/store/inbox_test.go` (`TestMigrateV5ToV6`), replace:

```go
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 6 {
		t.Fatalf("user_version %d %v, want 6", v, err)
	}
```

with:

```go
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version %d %v, want %d", v, err, schemaVersion)
	}
```

In `internal/store/store_test.go` (`TestOpenPragmasAndSchema`), replace
`{"user_version", "6"},` with `{"user_version", "7"},`, replace the table
list

```go
	for _, table := range []string{"machines", "sessions", "events", "reports", "control_requests", "session_digests", "login_codes", "web_sessions", "inbox_triage"} {
```

with

```go
	for _, table := range []string{"machines", "sessions", "events", "reports", "control_requests", "session_digests", "login_codes", "web_sessions", "inbox_triage", "inbox_alerts"} {
```

and add this line to the `want` map, after the `"inbox_triage"` entry:

```go
		"inbox_alerts":     "sent_at session_id since",
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestMigrateV6ToV7|TestAlerts|TestAutoDismiss|TestOpenPragmasAndSchema' -count=1`
Expected: FAIL to compile with `e.s.LastAlerts undefined` and
`e.s.RecordAlert undefined`.

- [ ] **Step 3: Add schema v7**

In `internal/store/store.go`, replace `const schemaVersion = 6` with
`const schemaVersion = 7`. After the `schemaV6` constant, add:

```go
// schemaV7 adds inbox_alerts: the blocked item each session was last sent a
// Telegram alert for. The notifier writes the row only after Telegram
// accepts the message, so a failed send is retried on the next tick.
const schemaV7 = `
CREATE TABLE inbox_alerts (
	session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
	since      TEXT NOT NULL,             -- the blocked item's since that was alerted on
	sent_at    TEXT NOT NULL
);
`
```

In `migrate`, after the `if v < 6 { ... }` block, add:

```go
	if v < 7 {
		if _, err := tx.ExecContext(ctx, schemaV7); err != nil {
			return fmt.Errorf("migrate schema to v7: %w", err)
		}
	}
```

- [ ] **Step 4: Add the alert queries**

Create `internal/store/alerts.go`:

```go
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LastAlerts returns, for each session that was alerted on, the since of the
// blocked item it was last alerted on. See docs/server.md, "Telegram alerts".
func (s *Store) LastAlerts(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, since FROM inbox_alerts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id, since string
		if err := rows.Scan(&id, &since); err != nil {
			return nil, err
		}
		t, err := parseTS(since)
		if err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// RecordAlert stores that session id was alerted on the blocked item with
// this since. One row per session: a later alert replaces it. An unknown
// session returns ErrNotFound.
func (s *Store) RecordAlert(ctx context.Context, id string, since time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_alerts (session_id, since, sent_at) VALUES (?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET since = excluded.since, sent_at = excluded.sent_at`,
		id, formatTS(since), formatTS(s.Now())); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 5: Add the auto-dismiss helper**

In `internal/store/inbox.go`, add at the end of the file:

```go
// autoDismissSeenTx dismisses session id's Finished item inside tx, with
// triaged_since set to the item's since (turn_ended_at). Callers run it after
// an update that moved agent_state from done to idle, which is herdr marking
// the pane seen. A Blocked or Waiting item, or no item, is left alone.
//
// Every read goes through tx: the store has one connection, and the
// transaction holds it, so a read on s.db here would wait forever.
func (s *Store) autoDismissSeenTx(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	x, err := s.scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE s.id = ?", id), now)
	if err != nil {
		return err
	}
	var blocked, stateTS, turnEnded sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT blocked_at, state_ts, turn_ended_at FROM sessions WHERE id = ?`, id).
		Scan(&blocked, &stateTS, &turnEnded); err != nil {
		return err
	}
	var st inboxState
	if st.blockedAt, err = parseNullTS(blocked); err != nil {
		return err
	}
	if st.stateTS, err = parseNullTS(stateTS); err != nil {
		return err
	}
	if st.turnEndedAt, err = parseNullTS(turnEnded); err != nil {
		return err
	}
	item, ok := classifyInbox(x, st.blockedAt, st.stateTS, st.turnEndedAt)
	if !ok || item.Group != api.InboxFinished {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO inbox_triage (session_id, triaged_since, snooze_until, updated_at)
		VALUES (?, ?, NULL, ?)
		ON CONFLICT(session_id) DO UPDATE SET triaged_since = excluded.triaged_since,
			snooze_until = NULL, updated_at = excluded.updated_at`,
		id, formatTS(item.Since), formatTS(now))
	return err
}
```

- [ ] **Step 6: Call it from both state paths**

In `internal/store/sessions.go` (`upsertTx`), replace:

```go
	var titleSource, firstPrompt string
	err := tx.QueryRowContext(ctx, `SELECT title_source, first_prompt FROM sessions WHERE id = ?`, u.ID).
		Scan(&titleSource, &firstPrompt)
```

with:

```go
	var titleSource, firstPrompt, prevState string
	err := tx.QueryRowContext(ctx, `SELECT title_source, first_prompt, agent_state FROM sessions WHERE id = ?`, u.ID).
		Scan(&titleSource, &firstPrompt, &prevState)
```

and replace the end of `upsertTx`:

```go
	args = append(args, u.ID)
	_, err = tx.ExecContext(ctx, "UPDATE sessions SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	return false, err
}
```

with:

```go
	args = append(args, u.ID)
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return false, err
	}
	if prevState == "done" && u.AgentState == "idle" {
		// herdr marked the pane seen: its Finished item is read.
		return false, s.autoDismissSeenTx(ctx, tx, u.ID, now)
	}
	return false, nil
}
```

In `AddEvent`, replace:

```go
	if state != "" {
		var last sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT state_ts FROM sessions WHERE id = ?`, id).Scan(&last); err != nil {
			return err
		}
		// Apply the state unless a newer one is already in place. The event
		// row is stored either way.
		if !last.Valid || formatTS(ts) >= last.String {
			q += ", agent_state = ?, state_ts = ?, turn_ended_at = " + turnEndedExpr + ", blocked_at = " + blockedAtExpr
			args = append(args, state, formatTS(ts), state, formatTS(ts), state, formatTS(ts))
		}
	}
```

with:

```go
	seen := false // the update moves the state from done to idle
	if state != "" {
		var last sql.NullString
		var prev string
		if err := tx.QueryRowContext(ctx, `SELECT state_ts, agent_state FROM sessions WHERE id = ?`, id).Scan(&last, &prev); err != nil {
			return err
		}
		// Apply the state unless a newer one is already in place. The event
		// row is stored either way.
		if !last.Valid || formatTS(ts) >= last.String {
			q += ", agent_state = ?, state_ts = ?, turn_ended_at = " + turnEndedExpr + ", blocked_at = " + blockedAtExpr
			args = append(args, state, formatTS(ts), state, formatTS(ts), state, formatTS(ts))
			seen = prev == "done" && state == "idle"
		}
	}
```

and replace the end of `AddEvent`:

```go
	args = append(args, id)
	if _, err := tx.ExecContext(ctx, q+" WHERE id = ?", args...); err != nil {
		return err
	}
	return tx.Commit()
}
```

with:

```go
	args = append(args, id)
	if _, err := tx.ExecContext(ctx, q+" WHERE id = ?", args...); err != nil {
		return err
	}
	if seen {
		// herdr marked the pane seen: its Finished item is read.
		if err := s.autoDismissSeenTx(ctx, tx, id, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/store/ -count=1`
Expected: PASS, including every existing inbox and migration test.

- [ ] **Step 8: Document it**

In `docs/server.md`, section "Inbox", after this bullet:

```markdown
- An item leaves on its own when its condition stops holding: the session
  stops being blocked, a later report has no `waiting_on`, or a new prompt
  arrives.
```

add:

```markdown
- When the agent state moves from `done` to `idle` (herdr marks a finished
  pane seen), the server dismisses that session's **Finished** item at its
  `since`, in the same write, on both the `state_changed` event and the herdr
  snapshot. A **Blocked** or **Waiting on you** item is never dismissed this
  way. Hooks-only sessions go from `working` straight to `idle`, so you
  dismiss their items yourself.
```

In section "Database", replace this text:

```markdown
version 6 adds `sessions.turn_ended_at`, `sessions.blocked_at`, and `inbox_triage`), and refuses
```

with:

```markdown
version 6 adds `sessions.turn_ended_at`, `sessions.blocked_at`, and `inbox_triage`; version 7 adds `inbox_alerts`), and refuses
```

After the "Inbox triage" subsection, before the line that starts
`The source is in`, add:

```markdown
### Inbox alerts

Schema version 7 adds `inbox_alerts`, one row per session: `since` (the
blocked item's `since` that the last Telegram alert was for) and `sent_at`.
The notifier writes the row only after Telegram accepts the message. The row
is deleted with its session.

To roll back to the v6 binary, restore the pre-deploy backup. The v6 binary
refuses a version 7 database.
```

- [ ] **Step 9: Run the full suite and lint**

Run: `make test && make lint`
Expected: every package `ok`, `gofmt -l` prints nothing, `go vet` clean.

- [ ] **Step 10: Commit**

```bash
git add internal/store/store.go internal/store/alerts.go internal/store/inbox.go internal/store/sessions.go \
  internal/store/concurrency_test.go internal/store/inbox_test.go internal/store/store_test.go \
  internal/store/alerts_test.go docs/server.md
git commit -m "$(cat <<'EOF'
Add schema v7 inbox_alerts and dismiss seen Finished items

inbox_alerts records the blocked item each session was last alerted on.
When herdr marks a finished pane seen (done to idle), the store dismisses
its Finished item in the same transaction, on the event and snapshot
paths. The helper reads through the transaction: the store has one
connection, so a read on s.db inside it would deadlock.

Refs: docs/dev/superpowers/plans/2026-10-01-inbox-alerts.md, Task 1

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---
### Task 2: Server: Telegram notifier

**Files:**
- Modify: `internal/server/config.go` (`Secret`, the `Config` fields, env
  overrides, `AlertsEnabled`). `fileConfig` already has
  `TelegramBotToken` and `TelegramChatID` (keys `telegram_bot_token` and
  `telegram_chat_id`); do not add them again.
- Modify: `internal/server/config_test.go` (`configEnv`, new cases; keep the
  existing `TestLoadConfigAcceptsTelegramKeys`)
- Create: `internal/server/notify.go` (notifier, message, `startAlerts`)
- Create: `internal/server/notify_test.go`
- Modify: `internal/server/run.go` (start and stop the notifier)
- Modify: `internal/server/run_test.go` (`TestRunTelegramAlerts`)
- Modify: `docs/server.md` (Configuration, Environment overrides, new
  "Telegram alerts" section)

**Interfaces:**
- Consumes: `(*store.Store).Inbox`, `(*store.Store).LastAlerts`,
  `(*store.Store).RecordAlert`, `(*store.Store).Now` (Task 1);
  `termtext.Clean` (`internal/cli/termtext`); test helpers `fakeClock`,
  `syncBuffer`, `configEnv`, `listenLocal`, `waitUp`.
- Produces:
  - `type Secret string` with `String()` and `GoString()` returning
    `[redacted]`
  - `Config.TelegramBotToken Secret`, `Config.TelegramChatID string`,
    `func (c Config) AlertsEnabled() bool`
  - `type notifier struct`, `func newNotifier(st alertStore, cfg Config, logger *log.Logger) *notifier`,
    `func (n *notifier) tick(ctx context.Context) int`,
    `func (n *notifier) run(ctx context.Context, ticks <-chan time.Time)`
  - `func alertMessage(it api.InboxItem, now time.Time, publicURL string) (string, []inlineButton)`
  - `func startAlerts(ctx context.Context, st alertStore, cfg Config, logger *log.Logger) (stop func())`
  - package variable `telegramAPI` (the Bot API base URL)

- [ ] **Step 1: Write the failing config tests**

In `internal/server/config_test.go` (`configEnv`), replace:

```go
	for _, k := range []string{"SESSIONHUB_LISTEN", "SESSIONHUB_PUBLIC_URL", "SESSIONHUB_READ_TOKEN", "SESSIONHUB_STALE_AFTER"} {
```

with:

```go
	for _, k := range []string{"SESSIONHUB_LISTEN", "SESSIONHUB_PUBLIC_URL", "SESSIONHUB_READ_TOKEN", "SESSIONHUB_STALE_AFTER",
		"SESSIONHUB_TELEGRAM_BOT_TOKEN", "SESSIONHUB_TELEGRAM_CHAT_ID"} {
```

In `TestLoadConfig`, add these cases after the `"SESSIONHUB_READ_TOKEN alone"` case:

```go
		{"telegram from the file", "telegram_bot_token = \" 123456:TEST-PLACEHOLDER-TOKEN \"\ntelegram_chat_id = \"-1001\"\n", nil,
			Config{Listen: []string{"127.0.0.1:8787"}, PublicURL: "https://sessionhub.example.com", StaleAfter: 5 * time.Minute,
				TelegramBotToken: "123456:TEST-PLACEHOLDER-TOKEN", TelegramChatID: "-1001"}},
		{"telegram env overrides file", "telegram_bot_token = \"file\"\ntelegram_chat_id = \"1\"\n", map[string]string{
			"SESSIONHUB_TELEGRAM_BOT_TOKEN": "123456:TEST-PLACEHOLDER-TOKEN", "SESSIONHUB_TELEGRAM_CHAT_ID": "42"},
			Config{Listen: []string{"127.0.0.1:8787"}, PublicURL: "https://sessionhub.example.com", StaleAfter: 5 * time.Minute,
				TelegramBotToken: "123456:TEST-PLACEHOLDER-TOKEN", TelegramChatID: "42"}},
```

In `TestLoadConfigErrors`, add this case after `"empty SESSIONHUB_LISTEN list"`:

```go
		{"unquoted telegram_chat_id", "telegram_chat_id = 42\n", nil, "telegram_chat_id"},
```

Add at the end of the file:

```go
func TestConfigHidesTelegramToken(t *testing.T) {
	cfg := Config{TelegramBotToken: "123456:TEST-PLACEHOLDER-TOKEN", TelegramChatID: "42"}
	for _, s := range []string{fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), fmt.Sprint(cfg.TelegramBotToken)} {
		if strings.Contains(s, "TEST-PLACEHOLDER") {
			t.Errorf("the token is printed: %s", s)
		}
	}
	if !cfg.AlertsEnabled() {
		t.Error("alerts off with both settings")
	}
	if (Config{TelegramChatID: "42"}).AlertsEnabled() || (Config{TelegramBotToken: "x"}).AlertsEnabled() {
		t.Error("alerts on with one setting missing")
	}
}
```

Add `"fmt"` to the file's imports.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/server/ -run 'TestLoadConfig|TestConfigHidesTelegramToken' -count=1`
Expected: FAIL to compile with `unknown field TelegramBotToken in struct
literal of type Config`.

- [ ] **Step 3: Add the settings**

In `internal/server/config.go`, replace:

```go
	StaleAfter      time.Duration
	DB              string // from SESSIONHUB_DB (paths.DB); not a file key
}
```

with:

```go
	StaleAfter      time.Duration
	DB              string // from SESSIONHUB_DB (paths.DB); not a file key
	// TelegramBotToken and TelegramChatID turn on Telegram alerts for
	// Blocked inbox items. Both must be set. See docs/server.md, "Telegram
	// alerts".
	TelegramBotToken Secret
	TelegramChatID   string
}

// Secret is a setting that must never reach a log. It prints as
// [redacted], so a %v, %+v, or %#v of a Config is safe.
type Secret string

// String hides the value.
func (Secret) String() string { return "[redacted]" }

// GoString hides the value from %#v.
func (Secret) GoString() string { return `"[redacted]"` }

// AlertsEnabled reports whether both Telegram settings are set.
func (c Config) AlertsEnabled() bool { return c.TelegramBotToken != "" && c.TelegramChatID != "" }
```

`fileConfig` already has `TelegramBotToken` (`telegram_bot_token`) and
`TelegramChatID` (`telegram_chat_id`), added on `main` so the live
`server.toml` on `tower` loads. Leave those fields and their comment as they
are, and leave `TestLoadConfigAcceptsTelegramKeys` in `config_test.go` in
place.

Replace:

```go
	cfg.LegacyReadToken = fc.ReadToken != ""
```

with:

```go
	cfg.LegacyReadToken = fc.ReadToken != ""
	cfg.TelegramBotToken = Secret(strings.TrimSpace(fc.TelegramBotToken))
	cfg.TelegramChatID = strings.TrimSpace(fc.TelegramChatID)
```

Replace:

```go
	if os.Getenv("SESSIONHUB_READ_TOKEN") != "" {
		cfg.LegacyReadToken = true
	}
```

with:

```go
	if os.Getenv("SESSIONHUB_READ_TOKEN") != "" {
		cfg.LegacyReadToken = true
	}
	if v := strings.TrimSpace(os.Getenv("SESSIONHUB_TELEGRAM_BOT_TOKEN")); v != "" {
		cfg.TelegramBotToken = Secret(v)
	}
	if v := strings.TrimSpace(os.Getenv("SESSIONHUB_TELEGRAM_CHAT_ID")); v != "" {
		cfg.TelegramChatID = v
	}
```

- [ ] **Step 4: Run the config tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestLoadConfig|TestConfigHidesTelegramToken' -count=1`
Expected: PASS.

- [ ] **Step 5: Write the failing notifier tests**

Create `internal/server/notify_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

// testToken is an obvious placeholder; no real token appears in tests.
const testToken = "123456:TEST-PLACEHOLDER-TOKEN"

type tgReply struct {
	status int
	body   string
}

// fakeTelegram is a Bot API server. It records each request and answers it
// with the next scripted reply, else 200 {"ok":true}.
type fakeTelegram struct {
	*httptest.Server
	mu      sync.Mutex
	paths   []string
	raw     []string
	bodies  []sendMessageIn
	replies []tgReply
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	t.Helper()
	f := &fakeTelegram{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var in sendMessageIn
		json.Unmarshal(b, &in)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.raw = append(f.raw, string(b))
		f.bodies = append(f.bodies, in)
		rep := tgReply{http.StatusOK, `{"ok":true,"result":{"message_id":1}}`}
		if len(f.replies) > 0 {
			rep, f.replies = f.replies[0], f.replies[1:]
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		w.Write([]byte(rep.body))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTelegram) reply(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, tgReply{status, body})
}

// request returns the path and raw body of request i.
func (f *fakeTelegram) request(i int) (path, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paths[i], f.raw[i]
}

func (f *fakeTelegram) sent() []sendMessageIn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sendMessageIn(nil), f.bodies...)
}

// alertEnv is a store with a fake clock and machine tower, and a notifier
// that talks to a fake Telegram.
type alertEnv struct {
	t     *testing.T
	st    *store.Store
	clock *fakeClock
	m     store.Machine
	tg    *fakeTelegram
	n     *notifier
	logs  *syncBuffer
}

func newAlertEnv(t *testing.T) *alertEnv {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	st, err := store.Open(filepath.Join(t.TempDir(), "sessionhub.db"), store.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	tok, _, err := st.AddMachine(ctx, "tower", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.MachineByToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	tg := newFakeTelegram(t)
	logs := &syncBuffer{}
	n := newNotifier(st, Config{PublicURL: "https://sessionhub.example.test", TelegramBotToken: testToken, TelegramChatID: "42"},
		log.New(logs, "", 0))
	n.apiBase = tg.URL
	return &alertEnv{t: t, st: st, clock: clock, m: m, tg: tg, n: n, logs: logs}
}

// set registers or updates session id with this agent state, the way the
// watcher's heartbeat does.
func (e *alertEnv) set(id, state string) {
	e.t.Helper()
	if _, err := e.st.UpsertSession(context.Background(), e.m.ID, api.SessionUpsert{ID: id, Source: api.SourcePlugin,
		AgentState: state, TitleHint: "fix login"}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *alertEnv) tick() int { return e.n.tick(context.Background()) }

func (e *alertEnv) alerted() map[string]time.Time {
	e.t.Helper()
	m, err := e.st.LastAlerts(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func TestNotifierGraceAndMessage(t *testing.T) {
	e := newAlertEnv(t)
	e.set("s1", "blocked")
	since := e.clock.Now()
	e.clock.Advance(29 * time.Second)
	if n := e.tick(); n != 0 || len(e.tg.sent()) != 0 {
		t.Fatalf("29 s after blocking: sent %d, requests %d, want none", n, len(e.tg.sent()))
	}
	if len(e.alerted()) != 0 {
		t.Fatal("alert recorded before any send")
	}
	e.clock.Advance(time.Second)
	if n := e.tick(); n != 1 {
		t.Fatalf("30 s after blocking: sent %d, want 1", n)
	}
	want := sendMessageIn{ChatID: "42", Text: "⏸ Blocked: fix login\ntower · blocked 30s ago",
		ReplyMarkup: &replyMarkup{InlineKeyboard: [][]inlineButton{{{Text: "Open inbox", URL: "https://sessionhub.example.test/#inbox"}}}}}
	if got := e.tg.sent(); !reflect.DeepEqual(got[0], want) {
		t.Errorf("message %+v\nwant    %+v", got[0], want)
	}
	path, raw := e.tg.request(0)
	if path != "/bot"+testToken+"/sendMessage" {
		t.Error("request path is not /bot<token>/sendMessage")
	}
	if strings.Contains(raw, "parse_mode") {
		t.Errorf("request sets parse_mode: %s", raw)
	}
	if got := e.alerted(); !got["s1"].Equal(since) {
		t.Errorf("recorded alert %v, want since %v", got, since)
	}
}

// TestNotifierAlertsOncePerBlock: a block whose since never moves alerts
// once, however many heartbeats repeat it. Blocking again alerts again.
func TestNotifierAlertsOncePerBlock(t *testing.T) {
	e := newAlertEnv(t)
	e.set("s1", "blocked")
	e.clock.Advance(30 * time.Second)
	if n := e.tick(); n != 1 {
		t.Fatalf("first tick sent %d, want 1", n)
	}
	for i := 0; i < 5; i++ {
		e.clock.Advance(15 * time.Second)
		e.set("s1", "blocked")
		if n := e.tick(); n != 0 {
			t.Fatalf("repeat %d sent %d, want 0", i, n)
		}
	}
	e.set("s1", "working")
	e.clock.Advance(time.Minute)
	e.set("s1", "blocked")
	if n := e.tick(); n != 0 {
		t.Fatalf("new block inside the grace sent %d", n)
	}
	e.clock.Advance(30 * time.Second)
	if n := e.tick(); n != 1 {
		t.Fatalf("new block after the grace sent %d, want 1", n)
	}
	if got := len(e.tg.sent()); got != 2 {
		t.Errorf("%d messages in total, want 2", got)
	}
}

// TestNotifierSkipsTriagedAndOtherGroups: a dismissed or snoozed Blocked
// item, and Waiting and Finished items, never alert. A snooze that ends
// alerts.
func TestNotifierSkipsTriagedAndOtherGroups(t *testing.T) {
	e := newAlertEnv(t)
	ctx := context.Background()
	e.set("dis", "blocked")
	e.set("snz", "blocked")
	e.set("fin", "working")
	e.set("fin", "idle")
	e.clock.Advance(30 * time.Second)
	in, err := e.st.Inbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range in.Items {
		switch it.Session.ID {
		case "dis":
			if err := e.st.Triage(ctx, "dis", it.Since, nil); err != nil {
				t.Fatal(err)
			}
		case "snz":
			until := e.clock.Now().Add(time.Hour)
			if err := e.st.Triage(ctx, "snz", it.Since, &until); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := e.tick(); n != 0 {
		t.Fatalf("sent %d with every blocked item triaged, want 0", n)
	}
	e.clock.Advance(time.Hour)
	if n := e.tick(); n != 1 {
		t.Fatalf("after the snooze ended: sent %d, want 1", n)
	}
	if got := e.alerted(); len(got) != 1 || got["snz"].IsZero() {
		t.Errorf("alerts %v, want snz only", got)
	}
}

// TestNotifierRetriesFailedSend: a 500, or a 200 with ok false, records
// nothing, and the next tick sends again.
func TestNotifierRetriesFailedSend(t *testing.T) {
	e := newAlertEnv(t)
	e.tg.reply(http.StatusInternalServerError, `{"ok":false,"error_code":500,"description":"Internal Server Error"}`)
	e.set("s1", "blocked")
	e.clock.Advance(30 * time.Second)
	if n := e.tick(); n != 0 || len(e.alerted()) != 0 {
		t.Fatalf("failed send: sent %d, alerts %v", n, e.alerted())
	}
	e.clock.Advance(15 * time.Second)
	if n := e.tick(); n != 1 || e.alerted()["s1"].IsZero() {
		t.Fatalf("retry: sent %d, alerts %v", n, e.alerted())
	}
	e.tg.reply(http.StatusOK, `{"ok":false,"description":"Bad Request: chat not found"}`)
	e.set("s2", "blocked")
	e.clock.Advance(30 * time.Second)
	if n := e.tick(); n != 0 || !e.alerted()["s2"].IsZero() {
		t.Fatalf("ok false: sent %d, alerts %v", n, e.alerted())
	}
	e.clock.Advance(15 * time.Second)
	if n := e.tick(); n != 1 {
		t.Fatalf("retry after ok false: sent %d, want 1", n)
	}
	if got := len(e.tg.sent()); got != 4 {
		t.Errorf("%d requests, want 4 (two failures, two retries)", got)
	}
}

// TestNotifierHonoursRetryAfter: after HTTP 429 the notifier sends nothing
// until retry_after has passed, or 30 s without it.
func TestNotifierHonoursRetryAfter(t *testing.T) {
	e := newAlertEnv(t)
	e.tg.reply(http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 40","parameters":{"retry_after":40}}`)
	e.set("s1", "blocked")
	e.clock.Advance(30 * time.Second)
	if n := e.tick(); n != 0 {
		t.Fatalf("429: sent %d", n)
	}
	for _, d := range []time.Duration{15 * time.Second, 24 * time.Second} {
		e.clock.Advance(d)
		if n := e.tick(); n != 0 || len(e.tg.sent()) != 1 {
			t.Fatalf("inside retry_after: sent %d, requests %d, want 0 and 1", n, len(e.tg.sent()))
		}
	}
	e.clock.Advance(time.Second)
	if n := e.tick(); n != 1 || len(e.tg.sent()) != 2 {
		t.Fatalf("at retry_after: sent %d, requests %d, want 1 and 2", n, len(e.tg.sent()))
	}
	e.tg.reply(http.StatusTooManyRequests, `{"ok":false,"error_code":429}`)
	e.set("s2", "blocked")
	e.clock.Advance(30 * time.Second)
	e.tick()
	e.clock.Advance(29 * time.Second)
	if n := e.tick(); n != 0 || len(e.tg.sent()) != 3 {
		t.Fatalf("429 without retry_after, 29 s later: sent %d, requests %d", n, len(e.tg.sent()))
	}
	e.clock.Advance(time.Second)
	if n := e.tick(); n != 1 {
		t.Fatalf("429 without retry_after, 30 s later: sent %d, want 1", n)
	}
}

func TestNotifierLimitsSendsPerTick(t *testing.T) {
	e := newAlertEnv(t)
	for i := 0; i < 12; i++ {
		e.set(fmt.Sprintf("s%02d", i), "blocked")
	}
	e.clock.Advance(30 * time.Second)
	for i, want := range []int{10, 2, 0} {
		if n := e.tick(); n != want {
			t.Errorf("tick %d sent %d, want %d", i, n, want)
		}
	}
	if got := len(e.alerted()); got != 12 {
		t.Errorf("%d sessions alerted, want 12", got)
	}
}

// TestNotifierNeverLogsToken: a network error, a 500 whose description
// echoes the token, and a 429 each log one line a minute apart, and no line
// holds the token or the request path. Failures inside a minute log once.
func TestNotifierNeverLogsToken(t *testing.T) {
	e := newAlertEnv(t)
	e.set("s1", "blocked")
	e.clock.Advance(30 * time.Second)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	e.n.apiBase = dead.URL
	e.tick()
	e.n.apiBase = e.tg.URL
	e.tg.reply(http.StatusInternalServerError, `{"ok":false,"description":"boom `+testToken+`"}`)
	e.clock.Advance(time.Minute)
	e.tick()
	e.tg.reply(http.StatusTooManyRequests, `{"ok":false,"parameters":{"retry_after":1}}`)
	e.clock.Advance(time.Minute)
	e.tick()
	e.tg.reply(http.StatusInternalServerError, `{"ok":false,"description":"again"}`)
	e.clock.Advance(2 * time.Second)
	e.tick()
	out := e.logs.String()
	if n := strings.Count(out, "\n"); n != 3 {
		t.Errorf("%d log lines, want 3 (one per minute)", n)
	}
	if strings.Contains(out, testToken) || strings.Contains(out, "TEST-PLACEHOLDER") || strings.Contains(out, "/bot") {
		t.Error("a log line holds the token or the request path")
	}
	if !strings.Contains(out, "HTTP 500") || !strings.Contains(out, "HTTP 429") {
		t.Errorf("log lacks the failures:\n%s", out)
	}
}

func TestAlertMessage(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rc := "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"
	before, after := now.Add(-time.Hour), now.Add(-time.Minute)
	report := &api.Report{TS: now.Add(-10 * time.Minute), WaitingOn: []string{"approve the plan", "pick a name"}}
	open := inlineButton{Text: "Open inbox", URL: "https://sessionhub.example.test/#inbox"}
	cases := []struct {
		name    string
		s       api.Session
		since   time.Duration
		text    string
		buttons []inlineButton
	}{
		{"report waiting_on, cleaned title, Remote Control",
			api.Session{ID: "0a1b2c3d-1111", Title: "fix \x1b[31mlogin‮", Machine: "bluebox", Recap: "Asked about tokens.",
				RemoteControlURL: rc, LastPromptAt: &before, LatestReport: report},
			3 * time.Minute, "⏸ Blocked: fix [31mlogin\nbluebox · blocked 3m ago\napprove the plan; pick a name",
			[]inlineButton{open, {Text: "Remote Control", URL: rc}}},
		{"report older than the last prompt: the recap",
			api.Session{ID: "0a1b2c3d-1111", Title: "t", Machine: "bluebox", Recap: "Asked\nabout tokens.", LastPromptAt: &after, LatestReport: report},
			2 * time.Hour, "⏸ Blocked: t\nbluebox · blocked 2h ago\nAsked about tokens.", []inlineButton{open}},
		{"no detail: two lines; cwd as title",
			api.Session{ID: "0a1b2c3d-1111", CWD: "/home/user/x", Machine: "tower"},
			72 * time.Hour, "⏸ Blocked: /home/user/x\ntower · blocked 3d ago", []inlineButton{open}},
		{"no title or cwd: the ID; an http link gets no button",
			api.Session{ID: "0a1b2c3d-1111", Machine: "tower", RemoteControlURL: "http://claude.ai/x"},
			45 * time.Second, "⏸ Blocked: session 0a1b2c3d\ntower · blocked 45s ago", []inlineButton{open}},
		{"detail cut to 300 characters",
			api.Session{ID: "0a1b2c3d-1111", Title: "t", Machine: "tower", Recap: strings.Repeat("x", 400)},
			time.Minute, "⏸ Blocked: t\ntower · blocked 1m ago\n" + strings.Repeat("x", 299) + "…", []inlineButton{open}},
	}
	for _, c := range cases {
		it := api.InboxItem{Group: api.InboxBlocked, Since: now.Add(-c.since), Session: c.s}
		text, buttons := alertMessage(it, now, "https://sessionhub.example.test")
		if text != c.text {
			t.Errorf("%s: text\n%q\nwant\n%q", c.name, text, c.text)
		}
		if !reflect.DeepEqual(buttons, c.buttons) {
			t.Errorf("%s: buttons %+v, want %+v", c.name, buttons, c.buttons)
		}
	}
}
```

In `internal/server/run_test.go`, add at the end:

```go
// TestRunTelegramAlerts: the server logs once whether alerts are on, never
// logs the token, and stops the notifier with the server.
func TestRunTelegramAlerts(t *testing.T) {
	cases := []struct{ name, token, chat, want string }{
		{"off without settings", "", "", "telegram alerts are off"},
		{"off with the token only", testToken, "", "telegram alerts are off"},
		{"on", testToken, "42", "telegram alerts are on"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configEnv(t, "")
			t.Setenv("SESSIONHUB_TELEGRAM_BOT_TOKEN", tc.token)
			t.Setenv("SESSIONHUB_TELEGRAM_CHAT_ID", tc.chat)
			ln := listenLocal(t)
			ctx, cancel := context.WithCancel(context.Background())
			var logs syncBuffer
			done := make(chan error, 1)
			go func() { done <- runWithListeners(ctx, nil, &logs, []net.Listener{ln}) }()
			waitUp(t, ln.Addr().String(), done)
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("run: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("run did not return: the notifier did not stop")
			}
			out := logs.String()
			if n := strings.Count(out, "telegram alerts are"); n != 1 || !strings.Contains(out, tc.want) {
				t.Errorf("want one %q line:\n%s", tc.want, out)
			}
			if strings.Contains(out, "TEST-PLACEHOLDER") {
				t.Error("the token is in the log")
			}
		})
	}
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./internal/server/ -run 'TestNotifier|TestAlertMessage|TestRunTelegramAlerts' -count=1`
Expected: FAIL to compile with `undefined: sendMessageIn`, `undefined:
newNotifier`, and `undefined: alertMessage`.

- [ ] **Step 7: Write the notifier**

Create `internal/server/notify.go`:

```go
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
)

// Telegram alert timing and limits. See docs/server.md, "Telegram alerts".
const (
	alertEvery        = 15 * time.Second // notifier tick
	alertGrace        = 30 * time.Second // a shorter block never alerts
	maxAlertsPerTick  = 10
	alertLogEvery     = time.Minute // at most one failure line per minute
	alertTitleRunes   = 120
	alertDetailRunes  = 300
	telegramTimeout   = 10 * time.Second
	defaultRetryAfter = 30 * time.Second // HTTP 429 without retry_after
)

// telegramAPI is the Bot API base URL. Tests point a notifier's apiBase at a
// fake server instead.
var telegramAPI = "https://api.telegram.org"

// alertStore is the part of *store.Store the notifier uses.
type alertStore interface {
	Now() time.Time
	Inbox(ctx context.Context) (api.Inbox, error)
	LastAlerts(ctx context.Context) (map[string]time.Time, error)
	RecordAlert(ctx context.Context, id string, since time.Time) error
}

// notifier sends one Telegram message for each new Blocked inbox item. It
// only calls sendMessage, so it never reads the bot's updates and never
// competes with another program that does.
type notifier struct {
	st        alertStore
	publicURL string
	token     string
	chatID    string
	apiBase   string
	http      *http.Client
	log       *log.Logger
	notBefore time.Time // after HTTP 429, no send before this
	lastLog   time.Time // when the last failure line was written
}

func newNotifier(st alertStore, cfg Config, logger *log.Logger) *notifier {
	return &notifier{
		st:        st,
		publicURL: strings.TrimRight(cfg.PublicURL, "/"),
		token:     string(cfg.TelegramBotToken),
		chatID:    cfg.TelegramChatID,
		apiBase:   telegramAPI,
		// No redirects: a redirect error would quote the request URL.
		http: &http.Client{Timeout: telegramTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		log: logger,
	}
}

// startAlerts starts the notifier when both Telegram settings are set, or
// logs once that alerts are off. stop cancels the notifier and waits for it
// to return.
func startAlerts(ctx context.Context, st alertStore, cfg Config, logger *log.Logger) (stop func()) {
	if !cfg.AlertsEnabled() {
		logger.Printf("telegram alerts are off: set telegram_bot_token and telegram_chat_id in server.toml to turn them on")
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	n := newNotifier(st, cfg, logger)
	t := time.NewTicker(alertEvery)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer t.Stop()
		n.run(ctx, t.C)
	}()
	logger.Printf("telegram alerts are on: checking the inbox every %s", alertEvery)
	return func() {
		cancel()
		<-done
	}
}

// run ticks at once, then on every tick, until ctx ends.
func (n *notifier) run(ctx context.Context, ticks <-chan time.Time) {
	n.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			n.tick(ctx)
		}
	}
}

// tick sends the alerts that are due and returns how many it sent. An item
// is due when it is Blocked, has been blocked for alertGrace, and has a
// since later than the last alert for its session. Triage already hides
// dismissed and snoozed items. The first failed send ends the tick.
func (n *notifier) tick(ctx context.Context) int {
	now := n.st.Now()
	if now.Before(n.notBefore) {
		return 0
	}
	in, err := n.st.Inbox(ctx)
	if err != nil {
		n.logf(now, "telegram alerts: read the inbox: %v", err)
		return 0
	}
	last, err := n.st.LastAlerts(ctx)
	if err != nil {
		n.logf(now, "telegram alerts: read the last alerts: %v", err)
		return 0
	}
	sent := 0
	for _, it := range in.Items {
		if it.Group != api.InboxBlocked || now.Sub(it.Since) < alertGrace {
			continue
		}
		if t, ok := last[it.Session.ID]; ok && !it.Since.After(t) {
			continue
		}
		if sent == maxAlertsPerTick {
			break
		}
		text, buttons := alertMessage(it, now, n.publicURL)
		retry, err := n.send(ctx, text, buttons)
		if err != nil {
			if retry > 0 {
				n.notBefore = now.Add(retry)
			}
			n.logf(now, "telegram alerts: send failed, retrying on a later tick: %v", err)
			return sent
		}
		if err := n.st.RecordAlert(ctx, it.Session.ID, it.Since); err != nil {
			n.logf(now, "telegram alerts: record the alert for session %s: %v", it.Session.ID, err)
		}
		sent++
	}
	return sent
}

// logf writes one failure line at most every alertLogEvery. The line never
// holds the token, even when an error quotes it.
func (n *notifier) logf(now time.Time, format string, args ...any) {
	if !n.lastLog.IsZero() && now.Sub(n.lastLog) < alertLogEvery {
		return
	}
	n.lastLog = now
	line := fmt.Sprintf(format, args...)
	if n.token != "" {
		line = strings.ReplaceAll(line, n.token, "[redacted]")
	}
	n.log.Print(line)
}

// inlineButton is one Telegram URL button.
type inlineButton struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

type replyMarkup struct {
	InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
}

// sendMessageIn is the sendMessage body. It sets no parse_mode, so the text
// is plain.
type sendMessageIn struct {
	ChatID      string       `json:"chat_id"`
	Text        string       `json:"text"`
	ReplyMarkup *replyMarkup `json:"reply_markup,omitempty"`
}

// telegramOut is the part of a Bot API response the notifier reads.
type telegramOut struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// send posts one message, one button per row. retry is how long to wait
// before the next send after HTTP 429, else 0. No error it returns holds
// the request URL, which holds the token.
func (n *notifier) send(ctx context.Context, text string, buttons []inlineButton) (retry time.Duration, err error) {
	in := sendMessageIn{ChatID: n.chatID, Text: text}
	if len(buttons) > 0 {
		rm := &replyMarkup{}
		for _, b := range buttons {
			rm.InlineKeyboard = append(rm.InlineKeyboard, []inlineButton{b})
		}
		in.ReplyMarkup = rm
	}
	body, err := json.Marshal(in)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.apiBase+"/bot"+n.token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("build the sendMessage request") // the parse error quotes the URL
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // *url.Error's text holds the URL
		}
		return 0, err
	}
	defer resp.Body.Close()
	var out telegramOut
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(b, &out)
	if resp.StatusCode == http.StatusTooManyRequests {
		retry = time.Duration(out.Parameters.RetryAfter) * time.Second
		if retry <= 0 {
			retry = defaultRetryAfter
		}
		return retry, fmt.Errorf("HTTP 429, waiting %s", retry)
	}
	if resp.StatusCode != http.StatusOK || !out.OK {
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, termtext.Clean(out.Description, 200))
	}
	return 0, nil
}

// alertMessage is the text and buttons for one Blocked item. Every server
// string is cleaned first. The Remote Control button needs an https link:
// Telegram rejects other URL buttons.
func alertMessage(it api.InboxItem, now time.Time, publicURL string) (string, []inlineButton) {
	s := it.Session
	title := termtext.Clean(s.Title, alertTitleRunes)
	if title == "" {
		title = termtext.Clean(s.CWD, alertTitleRunes)
	}
	if title == "" {
		id := []rune(termtext.Clean(s.ID, 0))
		if len(id) > 8 {
			id = id[:8]
		}
		title = "session " + string(id)
	}
	lines := []string{
		"⏸ Blocked: " + title,
		termtext.Clean(s.Machine, 0) + " · blocked " + alertAge(now.Sub(it.Since)) + " ago",
	}
	if d := termtext.Clean(alertDetail(it), alertDetailRunes); d != "" {
		lines = append(lines, d)
	}
	buttons := []inlineButton{{Text: "Open inbox", URL: publicURL + "/#inbox"}}
	if u := s.RemoteControlURL; strings.HasPrefix(u, "https://") && termtext.Clean(u, 0) == u {
		buttons = append(buttons, inlineButton{Text: "Remote Control", URL: u})
	}
	return strings.Join(lines, "\n"), buttons
}

// alertDetail is what the session waits on: the item's waiting_on, else the
// latest report's when no prompt came after that report, else the recap.
func alertDetail(it api.InboxItem) string {
	if len(it.WaitingOn) > 0 {
		return strings.Join(it.WaitingOn, "; ")
	}
	s := it.Session
	if r := s.LatestReport; r != nil && len(r.WaitingOn) > 0 && (s.LastPromptAt == nil || !s.LastPromptAt.After(r.TS)) {
		return strings.Join(r.WaitingOn, "; ")
	}
	return s.Recap
}

// alertAge is a short duration: 45s, 12m, 3h, or 2d.
func alertAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
```

- [ ] **Step 8: Start and stop it with the server**

In `internal/server/run.go`, replace:

```go
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
```

with:

```go
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Deferred after st.Close, so it runs first: the notifier stops before
	// the database closes, on every way out.
	stopAlerts := startAlerts(ctx, st, cfg, logger)
	defer stopAlerts()
```

- [ ] **Step 9: Run the tests to verify they pass**

Run: `go test ./internal/server/ -count=1`
Expected: PASS. If a test fails with `address already in use`, wait 60
seconds and run it again.

- [ ] **Step 10: Document it**

In `docs/server.md`, section "Configuration", add these rows to the key
table after the `stale_after` row:

```markdown
| `telegram_bot_token` | none | A Telegram bot token. With `telegram_chat_id`, turns on alerts for **Blocked** inbox items. Never logged. |
| `telegram_chat_id` | none | The chat the alerts go to, as a quoted string (`"-1001234567890"` for a group). |
```

In "Environment overrides", add after the `SESSIONHUB_STALE_AFTER` row:

```markdown
| `SESSIONHUB_TELEGRAM_BOT_TOKEN` | `telegram_bot_token`. |
| `SESSIONHUB_TELEGRAM_CHAT_ID` | `telegram_chat_id`. |
```

After the "Inbox" section (before "### Query parameters and IDs"), add:

````markdown
### Telegram alerts

When `telegram_bot_token` and `telegram_chat_id` are both set, the server
sends a Telegram message for each new **Blocked** inbox item. When either is
empty, alerts are off, and the server logs one line at startup saying so.

Every 15 seconds the server reads the inbox, the same list as
`GET /v1/inbox`, and sends one message for each **Blocked** item that:

- has been blocked for at least 30 seconds, so a prompt you answer at once
  never alerts,
- is not dismissed or snoozed, and
- has a `since` later than the last alert for that session. Blocking again
  gives a later `since`, so it alerts again.

The message is plain text:

```
⏸ Blocked: <title>
<machine> · blocked <age> ago
<what it waits on, or the recap, cut to 300 characters>
```

The third line is the latest report's `waiting_on` items when you haven't
sent a prompt since that report, else the recap; it is left out when both are
empty. Under the text is an **Open inbox** button
(`<public_url>/#inbox`) and, when the session has an `https` Remote Control
link, a **Remote Control** button. Every string from a session is cleaned
first.

- The server only calls `sendMessage`. It never reads the bot's updates, so
  it can share a bot with another program that does.
- `inbox_alerts` records each alert, and the row is written only after
  Telegram answers `ok: true`. A failed send (network error, a response that
  is not `ok`, HTTP 429) records nothing, and a later tick retries it. After
  HTTP 429 the server waits `retry_after` seconds (30 without it) before it
  sends again.
- At most 10 messages a tick and one a session. A failure is logged at most
  once a minute, without the token or the request URL.
````

- [ ] **Step 11: Run the full suite and lint**

Run: `make test && make lint`
Expected: every package `ok`, `gofmt -l` prints nothing, `go vet` clean.

- [ ] **Step 12: Commit**

```bash
git add internal/server/config.go internal/server/config_test.go internal/server/notify.go \
  internal/server/notify_test.go internal/server/run.go internal/server/run_test.go docs/server.md
git commit -m "$(cat <<'EOF'
Send Telegram alerts for Blocked inbox items

A notifier in sessionhub server reads the inbox every 15 s and sends one
sendMessage per block that is 30 s old and newer than its last alert,
recording it only after ok: true. Gotcha: *url.Error's text holds the
request URL, which holds the bot token, so errors are unwrapped and every
log line is redacted. The token is a Secret that prints as [redacted].

Refs: docs/dev/superpowers/plans/2026-10-01-inbox-alerts.md, Task 2

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 3: Dashboard: select the Inbox tab on `#inbox`

**Files:**
- Modify: `internal/server/dashboard/index.html` (`hashTab` in the
  `inbox-state` block, the first tab, `hashchange`)
- Modify: `internal/server/dashboard_test.go` (`TestDashboardInboxStates`,
  `TestDashboardWrites`)
- Modify: `docs/server.md` (Dashboard section)

**Interfaces:**
- Consumes: `showTab`, `firstTab`, `signedOut`, `load` in the page script.
- Produces: `function hashTab(hash)` returning `"inbox"`, `"sessions"`, or
  `null`. The Telegram **Open inbox** button (Task 2) links to
  `<public_url>/#inbox`.

- [ ] **Step 1: Write the failing tests**

In `internal/server/dashboard_test.go` (`TestDashboardInboxStates`), in the
node script, replace:

```go
  tabs: [firstTab(0), firstTab(2)],
```

with:

```go
  tabs: [firstTab(0), firstTab(2)],
  hash: [hashTab("#inbox"), hashTab("#sessions"), hashTab(""), hashTab("#other"), hashTab("#INBOX")],
```

In the `got` struct, after `Tabs []string \`json:"tabs"\``, add:

```go
		Hash        []*string         `json:"hash"`
```

and after the `titles %v tabs %v` check, add:

```go
	wantHash := []string{"inbox", "sessions"}
	for i, w := range wantHash {
		if got.Hash[i] == nil || *got.Hash[i] != w {
			t.Errorf("hashTab case %d = %v, want %s", i, got.Hash[i], w)
		}
	}
	for i := 2; i < len(got.Hash); i++ {
		if got.Hash[i] != nil {
			t.Errorf("hashTab case %d = %s, want null", i, *got.Hash[i])
		}
	}
```

In `TestDashboardWrites`, in the list that starts with
`"loadInbox();\n    fetch(\"/v1/sessions\""`, add these two strings:

```go
		"var askedTab = hashTab(location.hash);\n  if (askedTab) showTab(askedTab);\n  load();",
		"if (t && !signedOut) showTab(t);",
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/server/ -run 'TestDashboardInboxStates|TestDashboardWrites' -count=1`
Expected: FAIL. `TestDashboardWrites` reports `page lacks "var askedTab =
..."`. If `node` is on `PATH`, `TestDashboardInboxStates` fails with
`hashTab is not defined`; without `node` it is skipped.

- [ ] **Step 3: Add `hashTab`**

In `internal/server/dashboard/index.html`, inside the `inbox-state` block,
replace:

```js
  // firstTab is the tab the page opens on: Inbox when it has items.
  function firstTab(n) { return n > 0 ? "inbox" : "sessions"; }
```

with:

```js
  // firstTab is the tab the page opens on: Inbox when it has items.
  function firstTab(n) { return n > 0 ? "inbox" : "sessions"; }

  // hashTab is the tab a URL fragment asks for: #inbox or #sessions, else
  // null. The Telegram alert's Open inbox button links to /#inbox.
  function hashTab(hash) {
    if (hash === "#inbox") return "inbox";
    if (hash === "#sessions") return "sessions";
    return null;
  }
```

Replace:

```js
  load();
  filterEl.addEventListener("input", function () {
```

with:

```js
  // A fragment picks the tab before the first inbox read, which then keeps
  // it (tab is no longer null).
  var askedTab = hashTab(location.hash);
  if (askedTab) showTab(askedTab);
  load();
  window.addEventListener("hashchange", function () {
    var t = hashTab(location.hash);
    if (t && !signedOut) showTab(t);
  });
  filterEl.addEventListener("input", function () {
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestDashboard' -count=1`
Expected: PASS (the node tables run when `node` is on `PATH`).

- [ ] **Step 5: Document it**

In `docs/server.md`, section "Dashboard", the paragraph that starts
`The page has two tabs, **Inbox** and **Sessions**.` says the **Inbox** tab
is selected when the page loads with items in it. Directly after the sentence
that ends `the page never switches tabs on its own.`, insert:

```markdown
The URL fragment `#inbox` opens the **Inbox** tab and `#sessions` the
**Sessions** tab, whatever the inbox holds; the Telegram alert's **Open
inbox** button links to `/#inbox`. Changing the fragment later switches the
tab too.
```

- [ ] **Step 6: Run the full suite and lint**

Run: `make test && make lint`
Expected: every package `ok`, `gofmt -l` prints nothing, `go vet` clean.

- [ ] **Step 7: Commit**

```bash
git add internal/server/dashboard/index.html internal/server/dashboard_test.go docs/server.md
git commit -m "$(cat <<'EOF'
Open the dashboard on the Inbox tab for #inbox

The Telegram alert links to <public_url>/#inbox. The fragment picks the
tab before the first inbox read, so that read keeps it, and a later
hashchange switches tabs.

Refs: docs/dev/superpowers/plans/2026-10-01-inbox-alerts.md, Task 3

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 4: Plugin: herdr notification for Blocked

**Files:**
- Modify: `internal/herdr/herdr.go` (`NotificationParams`,
  `NotificationShow`)
- Modify: `internal/herdr/herdrtest/herdrtest.go` (`RequestsFor`)
- Modify: `internal/herdr/herdr_test.go` (`TestNotificationShow`)
- Create: `internal/plugin/notify.go` (`blockedStatus`, `notifyBlocked`,
  `alertBlocked`, `machineName`, `watcherLogf`)
- Create: `internal/plugin/notify_test.go`
- Modify: `internal/plugin/plugin.go` (`runEvent`)
- Modify: `internal/plugin/helpers_test.go` (the `exit` helper writes a
  `spawned` marker)
- Modify: `internal/plugin/event_test.go` (`TestRunEventQueuesAndStartsWatcher`
  waits for the marker)
- Modify: `docs/plugin.md` (hook table, the "no herdr call" paragraph)
- Modify: `docs/client.md` (`internal/herdr` and `herdrtest` sections)

**Interfaces:**
- Consumes: `herdr.Dial`, `(*herdr.Client).PaneGet`, `herdr.Event`,
  `herdr.AgentStatusChanged`, `herdr.EventAgentStatusChange`,
  `termtext.Clean`, `client.LoadConfig`, `watcherLogFile`, `stderr`,
  `capturedEvent`, `fixture`, `queueItems`, `executable`.
- Produces:
  - `type herdr.NotificationParams struct { Title, Body, Sound string }`
  - `func (c *herdr.Client) NotificationShow(p NotificationParams) error`
  - `func (s *herdrtest.Server) RequestsFor(method string) []herdrtest.Request`
  - `func blockedStatus(e herdr.Event) (herdr.AgentStatusChanged, bool)`
  - `func notifyBlocked(h notifyHerdr, d herdr.AgentStatusChanged, machine string) error`
  - `func watcherLogf(stateDir, format string, args ...any)`

- [ ] **Step 1: Write the failing herdr client test**

In `internal/herdr/herdr_test.go` (package `herdr_test`), add at the end:

```go
func TestNotificationShow(t *testing.T) {
	srv := herdrtest.New(t, "pane-get-existing.ndjson")
	shown := true
	srv.Handle("notification.show", func(json.RawMessage) (any, string) {
		if shown {
			return map[string]any{"type": "notification_show", "shown": true, "reason": "shown"}, ""
		}
		return map[string]any{"type": "notification_show", "shown": false, "reason": "no_foreground_client"}, ""
	})
	c, err := herdr.Dial(srv.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.NotificationShow(herdr.NotificationParams{Title: "Blocked: x", Body: "tower", Sound: "request"}); err != nil {
		t.Fatal(err)
	}
	reqs := srv.RequestsFor("notification.show")
	if len(reqs) != 1 || string(reqs[0].Params) != `{"title":"Blocked: x","body":"tower","sound":"request"}` {
		t.Errorf("requests %+v", reqs)
	}
	if n := len(srv.RequestsFor("pane.get")); n != 0 {
		t.Errorf("RequestsFor(pane.get) = %d, want 0", n)
	}
	shown = false
	if err := c.NotificationShow(herdr.NotificationParams{Title: "t"}); err == nil || !strings.Contains(err.Error(), "no_foreground_client") {
		t.Errorf("shown=false: %v, want an error with the reason", err)
	}
}
```

Add `"encoding/json"` and `"strings"` to that file's imports if they are not
there, and `"github.com/abdallah/session-hub/internal/herdr/herdrtest"` if it is not imported.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/herdr/... -run TestNotificationShow -count=1`
Expected: FAIL to compile with `c.NotificationShow undefined` and
`srv.RequestsFor undefined`.

- [ ] **Step 3: Add `NotificationShow` and `RequestsFor`**

In `internal/herdr/herdr.go`, after `SetSummary`, add:

```go
// NotificationParams mirrors herdr's NotificationShowParams. Sound is "none",
// "done", or "request".
type NotificationParams struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Sound string `json:"sound,omitempty"`
}

// NotificationShow calls notification.show. When herdr shows nothing, it
// answers shown=false with a reason (disabled, rate_limited,
// no_foreground_client, busy), which comes back as an error.
func (c *Client) NotificationShow(p NotificationParams) error {
	res, err := c.Call("notification.show", p)
	if err != nil {
		return err
	}
	var out struct {
		Shown  bool   `json:"shown"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return err
	}
	if !out.Shown {
		return fmt.Errorf("herdr: notification not shown: %s", out.Reason)
	}
	return nil
}
```

In `internal/herdr/herdrtest/herdrtest.go`, after `Requests`, add:

```go
// RequestsFor returns the requests received so far for method, in order.
func (s *Server) RequestsFor(method string) []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Request
	for _, r := range s.requests {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/herdr/... -count=1`
Expected: PASS.

- [ ] **Step 5: Write the failing plugin tests**

Create `internal/plugin/notify_test.go`:

```go
package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
)

// withData returns a captured event with one data field set ("" removes it).
func withData(t *testing.T, e herdr.Event, field, value string) herdr.Event {
	t.Helper()
	var d map[string]json.RawMessage
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatal(err)
	}
	if value == "" {
		delete(d, field)
	} else {
		d[field], _ = json.Marshal(value)
	}
	e.Data, _ = json.Marshal(d)
	return e
}

func TestBlockedStatus(t *testing.T) {
	blocked := capturedEvent(t, "pane.agent_status_changed-blocked")
	cases := []struct {
		name string
		e    herdr.Event
		want bool
	}{
		{"captured blocked claude pane", blocked, true},
		{"no agent named", withData(t, blocked, "agent", ""), true},
		{"working", withData(t, blocked, "agent_status", "working"), false},
		{"another agent", withData(t, blocked, "agent", "codex"), false},
		{"no pane", withData(t, blocked, "pane_id", ""), false},
		{"another event", capturedEvent(t, "pane.agent_detected"), false},
	}
	for _, c := range cases {
		d, ok := blockedStatus(c.e)
		if ok != c.want || (ok && d.PaneID != "w9:p1") {
			t.Errorf("%s: ok=%v pane=%q, want ok=%v", c.name, ok, d.PaneID, c.want)
		}
	}
}

// shownParams decodes the notification.show requests the fake received.
func shownParams(t *testing.T, srv *herdrtest.Server) []herdr.NotificationParams {
	t.Helper()
	var out []herdr.NotificationParams
	for _, r := range srv.RequestsFor("notification.show") {
		var p herdr.NotificationParams
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func showOK(json.RawMessage) (any, string) {
	return map[string]any{"type": "notification_show", "shown": true, "reason": "shown"}, ""
}

func TestNotifyBlocked(t *testing.T) {
	srv := herdrtest.New(t, "pane-get-existing.ndjson") // pane.get w9:p1: title "claude"
	srv.Handle("notification.show", showOK)
	h, err := herdr.Dial(srv.Path)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := blockedStatus(capturedEvent(t, "pane.agent_status_changed-blocked"))
	// The captured event has no title: the pane's.
	if err := notifyBlocked(h, d, "tower"); err != nil {
		t.Fatal(err)
	}
	// An event title is cleaned and cut to 80 characters; so is the machine.
	d.Title = "fix \x1b]0;evil\x07 " + strings.Repeat("y", 100)
	if err := notifyBlocked(h, d, "to\ngh"); err != nil {
		t.Fatal(err)
	}
	// No title anywhere (the pane is unknown): "Claude".
	gone := herdr.AgentStatusChanged{PaneID: "w9:p404", AgentStatus: "blocked"}
	if err := notifyBlocked(h, gone, "tower"); err != nil {
		t.Fatal(err)
	}
	want := []herdr.NotificationParams{
		{Title: "Blocked: claude", Body: "tower", Sound: "request"},
		{Title: "Blocked: fix ]0;evil " + strings.Repeat("y", 67) + "…", Body: "to gh", Sound: "request"},
		{Title: "Blocked: Claude", Body: "tower", Sound: "request"},
	}
	got := shownParams(t, srv)
	if len(got) != len(want) {
		t.Fatalf("%d notifications, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("notification %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	srv.Handle("notification.show", func(json.RawMessage) (any, string) {
		return map[string]any{"type": "notification_show", "shown": false, "reason": "rate_limited"}, ""
	})
	if err := notifyBlocked(h, d, "tower"); err == nil || !strings.Contains(err.Error(), "rate_limited") {
		t.Errorf("shown=false: %v, want an error with the reason", err)
	}
}

// TestRunEventNotifiesBlocked: sessionhub plugin event shows the notification for
// a blocked status and still queues the event; a working status shows none;
// a herdr failure goes to watcher.log and the hook still returns.
func TestRunEventNotifiesBlocked(t *testing.T) {
	dir := t.TempDir()
	srv := herdrtest.New(t, "pane-get-existing.ndjson")
	srv.Handle("notification.show", showOK)
	blocked := string(fixture(t, "events/pane.agent_status_changed-blocked.json"))
	t.Setenv("SESSIONHUB_STATE_DIR", dir)
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(dir, "no-config.toml"))
	t.Setenv("SESSIONHUB_MACHINE", "tower")
	t.Setenv("HERDR_SOCKET_PATH", srv.Path)
	t.Setenv("SESSIONHUB_PLUGIN_TEST_HELPER", "exit")
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", blocked)
	executable = func() (string, error) { return os.Args[0], nil }
	defer func() { executable = os.Executable }()

	runEvent(dir, time.Now())
	got := shownParams(t, srv)
	if len(got) != 1 || got[0] != (herdr.NotificationParams{Title: "Blocked: claude", Body: "tower", Sound: "request"}) {
		t.Fatalf("notifications %+v", got)
	}
	if n := len(queueItems(t, client.NewQueue(dir))); n != 1 {
		t.Errorf("queue has %d items, want 1", n)
	}

	t.Setenv("HERDR_PLUGIN_EVENT_JSON", strings.Replace(blocked, `"agent_status":"blocked"`, `"agent_status":"working"`, 1))
	runEvent(dir, time.Now())
	if n := len(shownParams(t, srv)); n != 1 {
		t.Errorf("a working status showed a notification (%d total)", n)
	}

	srv.Handle("notification.show", func(json.RawMessage) (any, string) { return nil, "test_refused" })
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", blocked)
	runEvent(dir, time.Now())
	b, err := os.ReadFile(filepath.Join(dir, watcherLogFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `herdr notification for blocked pane "w9:p1"`) || !strings.Contains(string(b), "test_refused") {
		t.Errorf("watcher.log lacks the failure:\n%s", b)
	}
}
```

In `internal/plugin/helpers_test.go` (`TestMain`), replace:

```go
	case "exit":
		os.Exit(0)
```

with:

```go
	case "exit":
		// The marker tells a test the watcher was spawned: watcher.log is no
		// proof, since sessionhub plugin event also writes to it.
		if dir := os.Getenv("SESSIONHUB_STATE_DIR"); dir != "" {
			os.WriteFile(filepath.Join(dir, "spawned"), nil, 0o600)
		}
		os.Exit(0)
```

In `internal/plugin/event_test.go` (`TestRunEventQueuesAndStartsWatcher`),
replace:

```go
	if _, err := os.Stat(filepath.Join(dir, watcherLogFile)); err != nil {
		t.Errorf("watcher was not started (no log file): %v", err)
	}
```

with:

```go
	spawned := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "spawned")); err == nil {
			spawned = true
			break
		}
	}
	if !spawned {
		t.Error("watcher was not started (no spawned marker)")
	}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./internal/plugin/ -run 'TestBlockedStatus|TestNotifyBlocked|TestRunEvent' -count=1`
Expected: FAIL to compile with `undefined: blockedStatus` and
`undefined: notifyBlocked`.

- [ ] **Step 7: Write the notification code**

Create `internal/plugin/notify.go`:

```go
package plugin

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
)

// blockedTitleRunes caps the session title in a Blocked notification.
const blockedTitleRunes = 80

// notifyHerdr is the part of *herdr.Client a Blocked notification uses.
type notifyHerdr interface {
	PaneGet(id string) (herdr.PaneInfo, error)
	NotificationShow(p herdr.NotificationParams) error
}

// blockedStatus returns the payload of a pane.agent_status_changed event
// whose new status is blocked, for a Claude pane or one whose agent herdr
// did not name (the same filter itemFromEvent uses). ok is false for every
// other event.
func blockedStatus(e herdr.Event) (herdr.AgentStatusChanged, bool) {
	if e.Event != herdr.EventAgentStatusChange {
		return herdr.AgentStatusChanged{}, false
	}
	d, err := e.AgentStatusChanged()
	if err != nil || d.PaneID == "" || d.AgentStatus != "blocked" || (d.Agent != "" && d.Agent != "claude") {
		return herdr.AgentStatusChanged{}, false
	}
	return d, true
}

// notifyBlocked shows "Blocked: <title>" with the machine as the body and
// the request sound. The title is the event's, else the pane's terminal
// title (herdr's events carry none so far), cleaned and cut to
// blockedTitleRunes, else "Claude".
func notifyBlocked(h notifyHerdr, d herdr.AgentStatusChanged, machine string) error {
	title := d.Title
	if title == "" {
		if p, err := h.PaneGet(d.PaneID); err == nil {
			title = p.TitleClean
			if title == "" {
				title = p.Title
			}
		}
	}
	title = termtext.Clean(title, blockedTitleRunes)
	if title == "" {
		title = "Claude"
	}
	return h.NotificationShow(herdr.NotificationParams{
		Title: "Blocked: " + title,
		Body:  termtext.Clean(machine, 0),
		Sound: "request",
	})
}

// machineName is the client config's machine, else the host name.
func machineName() string {
	if cfg, err := client.LoadConfig(); err == nil && cfg.Machine != "" {
		return cfg.Machine
	}
	h, _ := os.Hostname()
	return h
}

// alertBlocked shows the Blocked notification in the herdr at socketPath. A
// failure goes to the watcher log and is otherwise ignored: the hook never
// fails.
func alertBlocked(stateDir, socketPath string, d herdr.AgentStatusChanged) {
	h, err := herdr.Dial(socketPath)
	if err == nil {
		err = notifyBlocked(h, d, machineName())
	}
	if err != nil {
		watcherLogf(stateDir, "event: herdr notification for blocked pane %q: %v", d.PaneID, err)
	}
}

// watcherLogf appends one line to <stateDir>/watcher.log in the watcher's
// format. If the file can't be opened, the line goes to stderr (herdr's
// plugin log).
func watcherLogf(stateDir, format string, args ...any) {
	os.MkdirAll(stateDir, 0o700)
	f, err := os.OpenFile(filepath.Join(stateDir, watcherLogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "sessionhub plugin event: "+format+"\n", args...)
		return
	}
	defer f.Close()
	log.New(f, "", log.LstdFlags|log.LUTC).Printf(format, args...)
}
```

In `internal/plugin/plugin.go`, replace `runEvent`:

```go
// runEvent queues one item for HERDR_PLUGIN_EVENT_JSON and makes sure the
// watcher runs. No network call and no herdr call.
func runEvent(stateDir string, now time.Time) {
	e, err := herdr.EventFromEnv()
	if err != nil {
		fmt.Fprintf(stderr, "sessionhub plugin event: %v\n", err)
	} else if it, ok, err := itemFromEvent(e, herdr.SessionName(herdr.SocketPath()), now); err != nil {
		fmt.Fprintf(stderr, "sessionhub plugin event: %s: %v\n", e.Event, err)
	} else if ok {
		if err := client.NewQueue(stateDir).Append(it); err != nil {
			fmt.Fprintf(stderr, "sessionhub plugin event: queue: %v\n", err)
		}
	}
	startWatcher(stateDir)
}
```

with:

```go
// runEvent queues one item for HERDR_PLUGIN_EVENT_JSON and makes sure the
// watcher runs. It makes no network call. Its only herdr calls are for a
// Claude pane that became blocked: a notification with a sound, on this
// machine, where the session runs.
func runEvent(stateDir string, now time.Time) {
	e, err := herdr.EventFromEnv()
	if err != nil {
		fmt.Fprintf(stderr, "sessionhub plugin event: %v\n", err)
		startWatcher(stateDir)
		return
	}
	if it, ok, err := itemFromEvent(e, herdr.SessionName(herdr.SocketPath()), now); err != nil {
		fmt.Fprintf(stderr, "sessionhub plugin event: %s: %v\n", e.Event, err)
	} else if ok {
		if err := client.NewQueue(stateDir).Append(it); err != nil {
			fmt.Fprintf(stderr, "sessionhub plugin event: queue: %v\n", err)
		}
	}
	startWatcher(stateDir)
	if d, ok := blockedStatus(e); ok {
		alertBlocked(stateDir, herdr.SocketPath(), d)
	}
}
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test ./internal/plugin/ ./internal/herdr/... -count=1`
Expected: PASS.

- [ ] **Step 9: Document it**

In `docs/plugin.md`, section "What each hook does", replace the table row:

```markdown
| `pane.agent_status_changed` | `sessionhub plugin event` | Queues a `state_changed` event with `agent_state` in the payload. |
```

with:

```markdown
| `pane.agent_status_changed` | `sessionhub plugin event` | Queues a `state_changed` event with `agent_state` in the payload. When the new status is `blocked`, it also shows a herdr notification with a sound (see below). |
```

and replace:

```markdown
`startup` and `event` always exit 0. `sessionhub plugin event` makes no network call
and no herdr call: it parses `HERDR_PLUGIN_EVENT_JSON`, appends one line to the
queue, and starts the watcher if none is running. Events for agents other than
Claude are ignored in v1.
```

with:

```markdown
`startup` and `event` always exit 0. `sessionhub plugin event` makes no network call:
it parses `HERDR_PLUGIN_EVENT_JSON`, appends one line to the queue, and starts
the watcher if none is running. Events for agents other than Claude are
ignored in v1.

When a Claude pane's status becomes `blocked`, `sessionhub plugin event` also calls
herdr's `notification.show` over the socket (the same as `herdr notification
show "Blocked: <title>" --body "<machine>" --sound request`). `<title>` is the
pane's terminal title, cleaned and cut to 80 characters, or `Claude`;
`<machine>` is the client config's `machine`, else the host name. It runs in
the herdr where the session lives, so you hear it on that machine. If herdr
refuses or shows nothing (for example `no_foreground_client`), the hook writes
one line to `watcher.log` and carries on.
```

In `docs/client.md`, section "`internal/herdr`", insert this bullet before
the bullet that starts `- **One request per connection.**`:

```markdown
- `NotificationShow(p)`: `notification.show` with `title`, `body`, and
  `sound` (`none`, `done`, `request`). herdr's `shown: false` comes back as
  an error that names the reason.
```

In section "`internal/herdr/herdrtest`", add at the end:

```markdown
`RequestsFor(method)` returns the requests the fake got for one method, in
order. `Handle(method, fn)` answers a method that has no captured fixture,
such as `notification.show`.
```

- [ ] **Step 10: Run the full suite and lint**

Run: `make test && make lint`
Expected: every package `ok`, `gofmt -l` prints nothing, `go vet` clean.

- [ ] **Step 11: Commit**

```bash
git add internal/herdr/herdr.go internal/herdr/herdrtest/herdrtest.go internal/herdr/herdr_test.go \
  internal/plugin/notify.go internal/plugin/notify_test.go internal/plugin/plugin.go \
  internal/plugin/helpers_test.go internal/plugin/event_test.go docs/plugin.md docs/client.md
git commit -m "$(cat <<'EOF'
Show a herdr notification when a Claude pane blocks

sessionhub plugin event calls notification.show over the socket for a
pane.agent_status_changed to blocked, with the request sound, on the
machine where the session runs. The captured event has no title, so the
title comes from pane.get. A failure goes to watcher.log; the hook never
fails. The watcher-spawn test now waits for a marker, because
watcher.log no longer proves a spawn.

Refs: docs/dev/superpowers/plans/2026-10-01-inbox-alerts.md, Task 4

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 5: Plugin: inbox count in the herdr sidebar

**Files:**
- Modify: `internal/herdr/herdr.go` (`WorkspaceInfo`,
  `Snapshot.Workspaces`, `WorkspaceMetadataParams`,
  `ReportWorkspaceMetadata`)
- Modify: `internal/herdr/herdr_test.go` (`TestReportWorkspaceMetadata`)
- Modify: `internal/plugin/watcher.go` (`workspaceReporter`, `inboxText`,
  `workspaceIDs`, `reportInbox`, fields, `refresh`, `heartbeat`,
  `runWatcher`)
- Modify: `internal/plugin/helpers_test.go` (`fakeHub` serves
  `GET /v1/inbox`)
- Create: `internal/plugin/sidebar_test.go`
- Modify: `docs/plugin.md` (Watcher section)
- Modify: `docs/client.md` (`internal/herdr` section)

**Interfaces:**
- Consumes: `(*client.Client).Inbox`, `api.InboxCounts`, `watcher.logf`,
  `herdrtest.Server.Handle` and `RequestsFor` (Task 4), `newTestEnv`,
  `capturedSnap`.
- Produces:
  - `type herdr.WorkspaceInfo struct { WorkspaceID, Label string }`;
    `herdr.Snapshot.Workspaces []WorkspaceInfo`
  - `type herdr.WorkspaceMetadataParams struct { WorkspaceID, Source string; Tokens map[string]*string }`
  - `func (c *herdr.Client) ReportWorkspaceMetadata(p WorkspaceMetadataParams) error`
  - `func inboxText(c api.InboxCounts) string`
  - `watcher.sidebar workspaceReporter` (nil in `newWatcher`; `runWatcher`
    sets it)

- [ ] **Step 1: Write the failing herdr client test**

In `internal/herdr/herdr_test.go`, add at the end:

```go
func TestReportWorkspaceMetadata(t *testing.T) {
	srv := herdrtest.New(t, "session-snapshot.ndjson")
	srv.Handle("workspace.report_metadata", func(json.RawMessage) (any, string) {
		return map[string]any{"type": "ok"}, ""
	})
	c := dial(t, srv)
	snap, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Workspaces) == 0 || snap.Workspaces[0].WorkspaceID != "w4" || snap.Workspaces[0].Label != "workspace-a" {
		t.Fatalf("snapshot workspaces %+v", snap.Workspaces)
	}
	text := "3 · 1 blocked"
	if err := c.ReportWorkspaceMetadata(herdr.WorkspaceMetadataParams{WorkspaceID: "w4", Source: "sessionhub",
		Tokens: map[string]*string{"inbox": &text}}); err != nil {
		t.Fatal(err)
	}
	if err := c.ReportWorkspaceMetadata(herdr.WorkspaceMetadataParams{WorkspaceID: "w4", Source: "sessionhub",
		Tokens: map[string]*string{"inbox": nil}}); err != nil {
		t.Fatal(err)
	}
	reqs := srv.RequestsFor("workspace.report_metadata")
	want := []string{
		`{"workspace_id":"w4","source":"sessionhub","tokens":{"inbox":"3 · 1 blocked"}}`,
		`{"workspace_id":"w4","source":"sessionhub","tokens":{"inbox":null}}`,
	}
	if len(reqs) != len(want) {
		t.Fatalf("%d requests, want %d", len(reqs), len(want))
	}
	for i := range want {
		if string(reqs[i].Params) != want[i] {
			t.Errorf("request %d params %s, want %s", i, reqs[i].Params, want[i])
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/herdr/... -run TestReportWorkspaceMetadata -count=1`
Expected: FAIL to compile with `snap.Workspaces undefined` and
`c.ReportWorkspaceMetadata undefined`.

- [ ] **Step 3: Add the types and the call**

In `internal/herdr/herdr.go`, replace:

```go
	FocusedPaneID      string      `json:"focused_pane_id"`
	Panes              []PaneInfo  `json:"panes"`
```

with:

```go
	FocusedPaneID      string          `json:"focused_pane_id"`
	Workspaces         []WorkspaceInfo `json:"workspaces"`
	Panes              []PaneInfo      `json:"panes"`
```

Before `// Snapshot calls session.snapshot.`, add:

```go
// WorkspaceInfo is the part of a snapshot workspace sessionhub reads.
type WorkspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
}
```

After `NotificationShow` (Task 4), add:

```go
// WorkspaceMetadataParams mirrors herdr's WorkspaceReportMetadataParams:
// display-only tokens on a workspace, which the sidebar shows as $name. A
// nil token value clears that token. Tokens is required by herdr.
type WorkspaceMetadataParams struct {
	WorkspaceID string             `json:"workspace_id"`
	Source      string             `json:"source"`
	Tokens      map[string]*string `json:"tokens"`
}

// ReportWorkspaceMetadata calls workspace.report_metadata.
func (c *Client) ReportWorkspaceMetadata(p WorkspaceMetadataParams) error {
	_, err := c.Call("workspace.report_metadata", p)
	return err
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/herdr/... -count=1`
Expected: PASS.

- [ ] **Step 5: Write the failing watcher tests**

In `internal/plugin/helpers_test.go`, add two fields to `fakeHub`, after
`putBody  string`:

```go
	inboxStatus int    // when non-zero, GET /v1/inbox answers this status
	inboxBody   string // the body of GET /v1/inbox
```

In the handler's `switch`, add this case first:

```go
		case r.Method == http.MethodGet && r.URL.Path == "/v1/inbox":
			if h.inboxStatus != 0 {
				http.Error(w, `{"error":"forced"}`, h.inboxStatus)
				return
			}
			w.Write([]byte(h.inboxBody))
			return
```

and add after `count`:

```go
// setInbox sets the status and body GET /v1/inbox answers (status 0: 200).
func (h *fakeHub) setInbox(status int, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inboxStatus, h.inboxBody = status, body
}
```

Create `internal/plugin/sidebar_test.go`:

```go
package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
)

func TestInboxText(t *testing.T) {
	cases := []struct {
		c    api.InboxCounts
		want string
	}{
		{api.InboxCounts{}, ""},
		{api.InboxCounts{Waiting: 2, Finished: 1}, "3"},
		{api.InboxCounts{Blocked: 1, Waiting: 1, Finished: 1}, "3 · 1 blocked"},
		{api.InboxCounts{Blocked: 2}, "2 · 2 blocked"},
	}
	for _, c := range cases {
		if got := inboxText(c.c); got != c.want {
			t.Errorf("inboxText(%+v) = %q, want %q", c.c, got, c.want)
		}
	}
}

// metadataReports decodes the workspace.report_metadata requests so far.
func metadataReports(t *testing.T, s *herdrtest.Server) []herdr.WorkspaceMetadataParams {
	t.Helper()
	var out []herdr.WorkspaceMetadataParams
	for _, r := range s.RequestsFor("workspace.report_metadata") {
		var p herdr.WorkspaceMetadataParams
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func inboxBody(c api.InboxCounts) string {
	b, _ := json.Marshal(api.Inbox{Items: []api.InboxItem{}, Counts: c})
	return string(b)
}

// TestHeartbeatInboxCount walks the sidebar count through heartbeats: set,
// unchanged (no call), changed, inbox read failure (no call), report
// failure (retried), and cleared.
func TestHeartbeatInboxCount(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var mu sync.Mutex
	refuse := false
	e.herdr.Handle("workspace.report_metadata", func(json.RawMessage) (any, string) {
		mu.Lock()
		defer mu.Unlock()
		if refuse {
			return nil, "test_refused"
		}
		return map[string]any{"type": "ok"}, ""
	})
	e.w.sidebar = e.w.herdr.(*herdr.Client)
	var ids []string
	for _, ws := range capturedSnap(t).snap.Workspaces {
		ids = append(ids, ws.WorkspaceID)
	}
	if len(ids) < 2 {
		t.Fatalf("captured snapshot has %d workspaces, want several", len(ids))
	}
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	beat := func(i int) { e.w.step(ctx, t0.Add(time.Duration(i)*heartbeatInterval)) }
	// check asserts the reports since the last check: one per workspace with
	// text want ("" for a clear), or none when want is "-".
	seen := 0
	check := func(step, want string) {
		t.Helper()
		all := metadataReports(t, e.herdr)
		got := all[seen:]
		seen = len(all)
		if want == "-" {
			if len(got) != 0 {
				t.Errorf("%s: %d reports, want none", step, len(got))
			}
			return
		}
		if len(got) != len(ids) {
			t.Fatalf("%s: %d reports, want %d (one per workspace)", step, len(got), len(ids))
		}
		for i, p := range got {
			v, present := p.Tokens["inbox"]
			if p.WorkspaceID != ids[i] || p.Source != "sessionhub" || len(p.Tokens) != 1 || !present {
				t.Errorf("%s: report %d %+v", step, i, p)
				continue
			}
			if (want == "" && v != nil) || (want != "" && (v == nil || *v != want)) {
				t.Errorf("%s: %s inbox token %v, want %q", step, p.WorkspaceID, v, want)
			}
		}
	}

	e.sessionhub.setInbox(0, inboxBody(api.InboxCounts{Blocked: 1, Waiting: 1, Finished: 1}))
	beat(0)
	check("first beat", "3 · 1 blocked")
	beat(1)
	check("unchanged", "-")
	e.sessionhub.setInbox(0, inboxBody(api.InboxCounts{Waiting: 2}))
	beat(2)
	check("changed", "2")
	e.sessionhub.setInbox(http.StatusInternalServerError, "")
	beat(3)
	check("inbox read fails: the count stays", "-")
	mu.Lock()
	refuse = true
	mu.Unlock()
	e.sessionhub.setInbox(0, inboxBody(api.InboxCounts{}))
	beat(4)
	check("report refused", "")
	mu.Lock()
	refuse = false
	mu.Unlock()
	beat(5)
	check("retried after a refusal", "")
	beat(6)
	check("cleared and unchanged", "-")
}

// TestHeartbeatSidebarStartsWithClear: a new watcher reports every
// workspace on its first heartbeat, even with an empty inbox, so a count an
// older watcher left behind goes away.
func TestHeartbeatSidebarStartsWithClear(t *testing.T) {
	e := newTestEnv(t)
	e.herdr.Handle("workspace.report_metadata", func(json.RawMessage) (any, string) { return map[string]any{"type": "ok"}, "" })
	e.w.sidebar = e.w.herdr.(*herdr.Client)
	e.sessionhub.setInbox(0, inboxBody(api.InboxCounts{}))
	e.w.step(context.Background(), time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	got := metadataReports(t, e.herdr)
	if len(got) != len(capturedSnap(t).snap.Workspaces) {
		t.Fatalf("%d reports, want one per workspace", len(got))
	}
	for _, p := range got {
		if v, ok := p.Tokens["inbox"]; !ok || v != nil {
			t.Errorf("%s: %v, want a clear", p.WorkspaceID, p.Tokens)
		}
	}
}

// TestHeartbeatWithoutSidebar: a watcher without a reporter (the default in
// tests) never reads the inbox.
func TestHeartbeatWithoutSidebar(t *testing.T) {
	e := newTestEnv(t)
	e.w.step(context.Background(), time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	if n := e.sessionhub.count(http.MethodGet, "/v1/inbox"); n != 0 {
		t.Errorf("%d inbox reads without a sidebar reporter", n)
	}
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./internal/plugin/ -run 'TestInboxText|TestHeartbeatInboxCount|TestHeartbeatSidebar|TestHeartbeatWithoutSidebar' -count=1`
Expected: FAIL to compile with `undefined: inboxText` and
`e.w.sidebar undefined`.

- [ ] **Step 7: Add the sidebar count to the watcher**

In `internal/plugin/watcher.go`, add `"strconv"` to the imports. After the
`snapshotter` interface, add:

```go
// workspaceReporter is the herdr call the sidebar count needs
// (*herdr.Client).
type workspaceReporter interface {
	ReportWorkspaceMetadata(p herdr.WorkspaceMetadataParams) error
}

// inboxToken is the workspace token that carries the inbox count. Show it
// with $inbox in [ui.sidebar.spaces] rows.
const inboxToken = "inbox"

// inboxText is the sidebar text for the inbox counts: the total, plus
// " · n blocked" when any are blocked. "" means clear the token.
func inboxText(c api.InboxCounts) string {
	total := c.Blocked + c.Waiting + c.Finished
	switch {
	case total == 0:
		return ""
	case c.Blocked > 0:
		return fmt.Sprintf("%d · %d blocked", total, c.Blocked)
	}
	return strconv.Itoa(total)
}

// workspaceIDs lists the snapshot's workspaces, in snapshot order.
func workspaceIDs(snap herdr.Snapshot) []string {
	out := make([]string, 0, len(snap.Workspaces))
	for _, ws := range snap.Workspaces {
		if ws.WorkspaceID != "" {
			out = append(out, ws.WorkspaceID)
		}
	}
	return out
}
```

In the `watcher` struct, after `failed map[string]time.Time ...`, add:

```go
	// sidebar writes the inbox count on each workspace; nil turns the count
	// off. newWatcher leaves it nil, runWatcher sets it.
	sidebar     workspaceReporter
	workspaces  []string          // workspace IDs from the last snapshot
	sidebarText map[string]string // workspace ID → inbox text it last accepted
```

In `newWatcher`, replace:

```go
		digested: map[string]transcriptMark{}, failed: map[string]time.Time{},
	}
```

with:

```go
		digested: map[string]transcriptMark{}, failed: map[string]time.Time{},
		sidebarText: map[string]string{},
	}
```

In `refresh`, replace:

```go
	ups := buildSessions(ctx, snap, w.herdrSession, nil)
	w.unidentified = unidentifiedPanes(snap)
```

with:

```go
	ups := buildSessions(ctx, snap, w.herdrSession, nil)
	w.unidentified = unidentifiedPanes(snap)
	w.workspaces = workspaceIDs(snap)
```

In `heartbeat`, replace:

```go
	for _, in := range res.Invalid {
		w.logf(now, "heartbeat: server skipped session %q: %s", in.ID, in.Reason)
	}
	w.lastBeatOK = now
}
```

with:

```go
	for _, in := range res.Invalid {
		w.logf(now, "heartbeat: server skipped session %q: %s", in.ID, in.Reason)
	}
	w.lastBeatOK = now
	w.reportInbox(ctx, c, now)
}

// reportInbox puts the inbox count on every workspace of this herdr
// session. A failed inbox read keeps what the sidebar shows. A workspace
// whose text is unchanged since it last accepted one gets no call; a failed
// report is retried at the next heartbeat. The first heartbeat reports every
// workspace, a clear included, so a count an older watcher left goes away.
func (w *watcher) reportInbox(ctx context.Context, c *client.Client, now time.Time) {
	if w.sidebar == nil {
		return
	}
	in, err := c.Inbox(ctx)
	if err != nil {
		w.logf(now, "sidebar: read the inbox: %v", err)
		return
	}
	text := inboxText(in.Counts)
	listed := map[string]bool{}
	for _, ws := range w.workspaces {
		listed[ws] = true
		if prev, ok := w.sidebarText[ws]; ok && prev == text {
			continue
		}
		p := herdr.WorkspaceMetadataParams{WorkspaceID: ws, Source: "sessionhub", Tokens: map[string]*string{inboxToken: nil}}
		if text != "" {
			t := text
			p.Tokens[inboxToken] = &t
		}
		if err := w.sidebar.ReportWorkspaceMetadata(p); err != nil {
			w.logf(now, "sidebar: workspace %s: %v", ws, err)
			continue
		}
		w.sidebarText[ws] = text
	}
	for ws := range w.sidebarText {
		if !listed[ws] {
			delete(w.sidebarText, ws) // closed workspaces
		}
	}
}
```

In `runWatcher`, replace:

```go
	w.logPath = filepath.Join(stateDir, watcherLogFile)
```

with:

```go
	w.logPath = filepath.Join(stateDir, watcherLogFile)
	w.sidebar = hc
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test ./internal/plugin/ ./internal/herdr/... -count=1`
Expected: PASS, including every existing watcher test (they never set
`sidebar`, so their request lists do not change).

- [ ] **Step 9: Document it**

In `docs/plugin.md`, section "Watcher", after the paragraph that starts
`Every 60 seconds, and once at start, it sends a heartbeat`, add:

````markdown
After a heartbeat the server accepts, the watcher reads `GET /v1/inbox` and
writes the count on every workspace of its herdr session, as the
display-only workspace token `inbox` (source `sessionhub`, herdr's
`workspace.report_metadata`): the total, plus ` · <n> blocked` when any are
blocked, for example `3 · 1 blocked`. An empty inbox clears the token. A
workspace whose text didn't change since its last report gets no call; a
report herdr refuses is retried at the next heartbeat. If the inbox read
fails, the sidebar keeps the last count. herdr shows the token where you put
`$inbox` in `[ui.sidebar.spaces]` rows in `~/.config/herdr/config.toml`:

```toml
[ui.sidebar.spaces]
rows = [["state_icon", "workspace", "$inbox"], ["branch", "git_status"]]
```
````

In `docs/client.md`, section "`internal/herdr`", insert this bullet before
the bullet that starts `- **One request per connection.**`:

```markdown
- `ReportWorkspaceMetadata(p)`: `workspace.report_metadata` with
  `workspace_id`, `source`, and `tokens` (a nil value clears a token). The
  watcher's sidebar count uses it. `Snapshot().Workspaces` lists each
  workspace's ID and label.
```

- [ ] **Step 10: Run the full suite and lint**

Run: `make test && make lint`
Expected: every package `ok`, `gofmt -l` prints nothing, `go vet` clean.

- [ ] **Step 11: Commit**

```bash
git add internal/herdr/herdr.go internal/herdr/herdr_test.go internal/plugin/watcher.go \
  internal/plugin/helpers_test.go internal/plugin/sidebar_test.go docs/plugin.md docs/client.md
git commit -m "$(cat <<'EOF'
Show the inbox count on herdr workspaces

After each accepted heartbeat the watcher reads the inbox and writes the
workspace token inbox ("3 · 1 blocked") on every workspace, skipping a
workspace whose text is unchanged and retrying a refused report. The
reporter is nil by default, so existing watcher tests see no extra
request.

Refs: docs/dev/superpowers/plans/2026-10-01-inbox-alerts.md, Task 5

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 6: CLI: `sessionhub inbox --watch`

**Files:**
- Create: `internal/cli/watch.go` (keys, state, `handleKey`, `renderWatch`,
  `watchLoop`, `watchAct`, the `jumper` interface)
- Create: `internal/cli/watchterm.go` (`withRawTerminal`, `runWatch`,
  `readKeys`)
- Create: `internal/cli/watch_test.go`
- Modify: `internal/cli/width.go` (`isTerminal`, `termSize`)
- Modify: `internal/cli/cli.go` (`env.isTerminal`, `env.jump`,
  `defaultEnv`)
- Modify: `internal/cli/inbox.go` (`--watch`, usage)
- Modify: `internal/cli/inbox_test.go` (`TestIsTerminal`)
- Modify: `cmd/sessionhub/main.go` (usage line)
- Modify: `docs/cli.md` (Commands table, "`sessionhub inbox`" section)

**Interfaces:**
- Consumes: `inboxAPI`, `inboxHeadings`, `inboxLine`, `cut`, `clean`,
  `shortID`, `parseSnooze`, test helpers `fakeInbox`, `inboxFixture`,
  `runInbox`, `now`.
- Produces:
  - `type jumper interface { Jump(ctx context.Context, s api.Session) (string, error) }`
    (Task 7's `*resume.Jumper` implements it)
  - `env.jump jumper` (nil until Task 7 sets it), `env.isTerminal func() bool`
  - `func parseKeys(b []byte) []key`,
    `func handleKey(s watchState, k key, now time.Time) (watchState, watchAction)`,
    `func renderWatch(s watchState, now time.Time, width, height int) []string`,
    `func (e *env) watchLoop(ctx context.Context, w watchIO) error`,
    `func withRawTerminal(stty sttyFunc, out io.Writer, fn func() error) error`

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/watch_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestParseKeys(t *testing.T) {
	got := parseKeys([]byte("jk\x1b[A\x1b[B\x1bOA\r\n\x03q\x1bs\x01"))
	want := []key{"j", "k", keyUp, keyDown, keyUp, keyEnter, keyEnter, keyCtrlC, "q", keyEsc, "s"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseKeys = %v, want %v", got, want)
	}
}

// press applies keys in order and returns the state and the last action.
func press(s watchState, keys ...key) (watchState, watchAction) {
	var a watchAction
	for _, k := range keys {
		s, a = handleKey(s, k, now)
	}
	return s, a
}

func TestWatchKeys(t *testing.T) {
	start := watchState{}.withInbox(inboxFixture().Items, now)
	if start.selected != "aaaaaaaa-1111" {
		t.Fatalf("first selection %q, want the first row", start.selected)
	}
	tomorrow9 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC) // now is 2026-09-30 12:00 UTC
	cases := []struct {
		name     string
		keys     []key
		selected string
		kind     actionKind
		item     string
		until    time.Time
	}{
		{"j moves down", []key{"j"}, "bbbbbbbb-2222", actNone, "", time.Time{}},
		{"down arrow", []key{keyDown, keyDown}, "cccccccc-3333", actNone, "", time.Time{}},
		{"stops at the last row", []key{"j", "j", "j", "j", "j"}, "cccc0000-4444", actNone, "", time.Time{}},
		{"k and up stop at the first row", []key{"j", "k", keyUp}, "aaaaaaaa-1111", actNone, "", time.Time{}},
		{"Enter jumps to the cursor row", []key{"j", keyEnter}, "bbbbbbbb-2222", actJump, "bbbbbbbb-2222", time.Time{}},
		{"d dismisses", []key{"d"}, "aaaaaaaa-1111", actDismiss, "aaaaaaaa-1111", time.Time{}},
		{"s 1 snoozes an hour", []key{"s", "1"}, "aaaaaaaa-1111", actSnooze, "aaaaaaaa-1111", now.Add(time.Hour)},
		{"s 4 snoozes four hours", []key{"j", "s", "4"}, "bbbbbbbb-2222", actSnooze, "bbbbbbbb-2222", now.Add(4 * time.Hour)},
		{"s t snoozes until 9:00 tomorrow", []key{"s", "t"}, "aaaaaaaa-1111", actSnooze, "aaaaaaaa-1111", tomorrow9},
		{"s Esc cancels", []key{"s", keyEsc}, "aaaaaaaa-1111", actNone, "", time.Time{}},
		{"s x cancels", []key{"s", "x"}, "aaaaaaaa-1111", actNone, "", time.Time{}},
		{"r refreshes", []key{"r"}, "aaaaaaaa-1111", actRefresh, "", time.Time{}},
		{"q quits", []key{"q"}, "aaaaaaaa-1111", actQuit, "", time.Time{}},
		{"Ctrl+C quits", []key{keyCtrlC}, "aaaaaaaa-1111", actQuit, "", time.Time{}},
		{"q quits during a snooze", []key{"s", "q"}, "aaaaaaaa-1111", actQuit, "", time.Time{}},
	}
	for _, c := range cases {
		s, a := press(start, c.keys...)
		if s.selected != c.selected || a.kind != c.kind || a.item.Session.ID != c.item || !a.until.Equal(c.until) {
			t.Errorf("%s: selected %q action %v item %q until %v, want %q %v %q %v",
				c.name, s.selected, a.kind, a.item.Session.ID, a.until, c.selected, c.kind, c.item, c.until)
		}
	}
	// A key after s ends the snooze prompt and does nothing else.
	s, _ := press(start, "s")
	if !s.snoozing || !strings.Contains(s.status, "1 = 1 hour") {
		t.Fatalf("after s: snoozing=%v status %q", s.snoozing, s.status)
	}
	if s, _ = press(s, "j"); s.snoozing || s.selected != "aaaaaaaa-1111" || s.status != "snooze cancelled" {
		t.Errorf("j after s: snoozing=%v selected %q status %q, want a cancel and no move", s.snoozing, s.selected, s.status)
	}
	// With no items, no key acts.
	empty := watchState{}.withInbox(nil, now)
	for _, k := range []key{"j", "k", keyEnter, "d", "s"} {
		if s, a := handleKey(empty, k, now); a.kind != actNone || s.snoozing {
			t.Errorf("%q on an empty inbox: action %v snoozing %v", k, a.kind, s.snoozing)
		}
	}
}

func TestWatchKeepsCursor(t *testing.T) {
	items := inboxFixture().Items // a, b, c, c0
	s, _ := press(watchState{}.withInbox(items, now), "j")
	if s = s.withInbox(items[1:], now); s.selected != "bbbbbbbb-2222" {
		t.Errorf("another row left: selected %q, want b", s.selected)
	}
	if s = s.withInbox(items[2:], now); s.selected != "cccccccc-3333" {
		t.Errorf("the selected row left: selected %q, want the row now at its position", s.selected)
	}
	s, _ = press(s, "j") // c0, index 1 of [c, c0]
	if s = s.withInbox(items[:1], now); s.selected != "aaaaaaaa-1111" {
		t.Errorf("the selected row left past the end: selected %q, want the last row", s.selected)
	}
	if s = s.withInbox(nil, now); s.selected != "" {
		t.Errorf("empty inbox: selected %q", s.selected)
	}
	if _, a := handleKey(s, keyEnter, now); a.kind != actNone {
		t.Errorf("Enter on an empty inbox: %v", a.kind)
	}
	if s = s.withInbox(items, now); s.selected != "aaaaaaaa-1111" {
		t.Errorf("items again: selected %q, want the first row", s.selected)
	}
}

func TestRenderWatch(t *testing.T) {
	s, _ := press(watchState{}.withInbox(inboxFixture().Items, now), "j")
	want := []string{
		watchHelp,
		"Blocked (1)",
		"  aaaaaaaa  fix login  tower  12m  Asked which token to rotate.",
		"",
		"Waiting on you (1)",
		"> bbbbbbbb  docs  bluebox  30m  review MR !12; pick a name",
		"",
		"Finished (2)",
		"  cccccccc  ci [31mred  tower  5m  stale  Tests pass. Next, deploy.",
		"  cccc0000  /home/user/x  tower  2m",
		"",
		"updated 12:00:00",
	}
	if got := renderWatch(s, now, 200, 12); !reflect.DeepEqual(got, want) {
		t.Errorf("render:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Too short: the window scrolls to keep the cursor row in view.
	s, _ = press(s, "j", "j")
	want = []string{
		watchHelp,
		"Finished (2)",
		"  cccccccc  ci [31mred  tower  5m  stale  Tests pass. Next, deploy.",
		"> cccc0000  /home/user/x  tower  2m",
		"updated 12:00:00",
	}
	if got := renderWatch(s, now, 200, 5); !reflect.DeepEqual(got, want) {
		t.Errorf("scrolled:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Narrow: every line fits.
	for i, l := range renderWatch(s, now, 30, 12) {
		if n := len([]rune(l)); n > 30 {
			t.Errorf("line %d is %d runes wide at width 30: %q", i, n, l)
		}
	}
	// Before the first read; empty after a read; status and a cleaned error.
	if got := renderWatch(watchState{}, now, 80, 4); !reflect.DeepEqual(got, []string{watchHelp, "", "", "loading..."}) {
		t.Errorf("loading: %q", got)
	}
	e := watchState{}.withInbox(nil, now)
	if got := renderWatch(e, now, 80, 4); !reflect.DeepEqual(got, []string{watchHelp, "nothing needs you", "", "updated 12:00:00"}) {
		t.Errorf("empty: %q", got)
	}
	e.status, e.errText = "dismissed aaaaaaaa", "refresh: down\x1b[2J"
	if got := renderWatch(e, now, 80, 4)[3]; got != "updated 12:00:00  dismissed aaaaaaaa  error: refresh: down[2J" {
		t.Errorf("status line %q", got)
	}
}

type fakeJumper struct {
	ids []string
	msg string
	err error
}

func (j *fakeJumper) Jump(_ context.Context, s api.Session) (string, error) {
	j.ids = append(j.ids, s.ID)
	return j.msg, j.err
}

// runLoop runs watchLoop over the inputs, one channel element per string,
// then closes the input, and returns the frames it drew.
func runLoop(t *testing.T, e *env, inputs ...string) [][]string {
	t.Helper()
	keys := make(chan []byte, len(inputs))
	for _, in := range inputs {
		keys <- []byte(in)
	}
	close(keys)
	var frames [][]string
	err := e.watchLoop(context.Background(), watchIO{keys: keys,
		draw: func(l []string) { frames = append(frames, l) },
		size: func() (int, int) { return 200, 12 }})
	if err != nil {
		t.Fatalf("watchLoop: %v", err)
	}
	return frames
}

func lastStatus(frames [][]string) string {
	f := frames[len(frames)-1]
	return f[len(f)-1]
}

func TestWatchLoopDismissAndSnooze(t *testing.T) {
	f := &fakeInbox{inbox: inboxFixture()}
	e := &env{inbox: f, now: func() time.Time { return now }}
	frames := runLoop(t, e, "j", "d", "s", "1", "q", "j")
	wantDismiss := "bbbbbbbb-2222 " + now.Add(-30*time.Minute).Format(time.RFC3339Nano)
	if !reflect.DeepEqual(f.dismissed, []string{wantDismiss}) {
		t.Errorf("dismissed %v, want %v", f.dismissed, wantDismiss)
	}
	wantSnooze := wantDismiss + " " + now.Add(time.Hour).Format(time.RFC3339)
	if !reflect.DeepEqual(f.snoozed, []string{wantSnooze}) {
		t.Errorf("snoozed %v, want %v", f.snoozed, wantSnooze)
	}
	if got := lastStatus(frames); got != "updated 12:00:00  snoozed bbbbbbbb until Wed 30 Sep 13:00" {
		t.Errorf("status %q", got)
	}
	if f.reads != 3 { // the first read, then one after each action; q stops before the last j
		t.Errorf("%d inbox reads, want 3", f.reads)
	}
}

func TestWatchLoopJumpAndErrors(t *testing.T) {
	inbox := inboxFixture()
	inbox.Items[0].Session.ResumeCommand = "ssh -t tower '~/.local/bin/sessionhub resume aaaaaaaa-1111'"
	f := &fakeInbox{inbox: inbox}
	e := &env{inbox: f, now: func() time.Time { return now }}
	// No jumper: Enter shows the resume command.
	if got := lastStatus(runLoop(t, e, "\r")); got != "updated 12:00:00  run: ssh -t tower '~/.local/bin/sessionhub resume aaaaaaaa-1111'" {
		t.Errorf("without a jumper: %q", got)
	}
	j := &fakeJumper{msg: `Focused pane "w1:p1"`}
	e.jump = j
	frames := runLoop(t, e, "j", "\r")
	if !reflect.DeepEqual(j.ids, []string{"bbbbbbbb-2222"}) || lastStatus(frames) != `updated 12:00:00  Focused pane "w1:p1"` {
		t.Errorf("jump: ids %v status %q", j.ids, lastStatus(frames))
	}
	// The frame drawn before a slow jump says so.
	if got := frames[len(frames)-2]; got[len(got)-1] != "updated 12:00:00  jumping to bbbbbbbb..." {
		t.Errorf("frame before the jump: %q", got[len(got)-1])
	}
	j.err = errors.New("ssh: connect timed out")
	if got := lastStatus(runLoop(t, e, "\r")); got != "updated 12:00:00  error: jump aaaaaaaa: ssh: connect timed out" {
		t.Errorf("jump error: %q", got)
	}
	// A failed refresh keeps the rows and shows the error.
	fl := &flakyInbox{fakeInbox{inbox: inboxFixture()}}
	frames = runLoop(t, &env{inbox: fl, now: func() time.Time { return now }}, "r")
	last := frames[len(frames)-1]
	if last[2] != "> aaaaaaaa  fix login  tower  12m  Asked which token to rotate." ||
		last[len(last)-1] != "updated 12:00:00  error: refresh: server down" {
		t.Errorf("refresh error frame:\n%s", strings.Join(last, "\n"))
	}
}

// flakyInbox answers the first read and fails every later one.
type flakyInbox struct{ fakeInbox }

func (f *flakyInbox) Inbox(context.Context) (api.Inbox, error) {
	f.reads++
	if f.reads > 1 {
		return api.Inbox{}, errors.New("server down")
	}
	return f.inbox, nil
}

func TestWatchLoopFirstReadFails(t *testing.T) {
	e := &env{inbox: &fakeInbox{err: errors.New("server down")}, now: func() time.Time { return now }}
	frames := runLoop(t, e)
	if got := lastStatus(frames); got != "loading...  error: refresh: server down" {
		t.Errorf("status %q", got)
	}
}

func TestRawTerminalRestores(t *testing.T) {
	var calls []string
	stty := func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if len(args) == 1 && args[0] == "-g" {
			return "500:5:bf:8a3b\n", nil
		}
		return "", nil
	}
	want := []string{"-g", "raw -echo", "500:5:bf:8a3b"}
	for _, fn := range []func() error{
		func() error { return nil },
		func() error { return errors.New("boom") },
	} {
		calls = nil
		var out bytes.Buffer
		withRawTerminal(stty, &out, fn)
		if !reflect.DeepEqual(calls, want) || out.String() != enterScreen+leaveScreen {
			t.Errorf("stty calls %q, output %q", calls, out.String())
		}
	}
	calls = nil
	var out bytes.Buffer
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed")
			}
		}()
		withRawTerminal(stty, &out, func() error { panic("boom") })
	}()
	if !reflect.DeepEqual(calls, want) || out.String() != enterScreen+leaveScreen {
		t.Errorf("after a panic: stty calls %q, output %q", calls, out.String())
	}
	// stty -g fails: nothing changes and fn never runs.
	calls = nil
	out.Reset()
	failing := func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return "", errors.New("not a tty")
	}
	err := withRawTerminal(failing, &out, func() error { t.Error("fn ran"); return nil })
	if err == nil || !reflect.DeepEqual(calls, []string{"-g"}) || out.Len() != 0 {
		t.Errorf("stty failure: err %v calls %q output %q", err, calls, out.String())
	}
}

func TestInboxWatchNeedsTerminal(t *testing.T) {
	f := &fakeInbox{inbox: inboxFixture()}
	if _, err := runInbox(t, f, 100, now, "--watch"); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("--watch without a terminal: %v", err)
	}
	if _, err := runInbox(t, f, 100, now, "--watch", "--json"); err == nil || !strings.Contains(err.Error(), "--json") {
		t.Errorf("--watch --json: %v", err)
	}
	if f.reads != 0 {
		t.Errorf("%d inbox reads, want 0", f.reads)
	}
}
```

In `internal/cli/inbox_test.go`, add at the end:

```go
func TestIsTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notatty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f.Fd()) {
		t.Error("a regular file is a terminal")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/cli/ -run 'TestParseKeys|TestWatch|TestRenderWatch|TestRawTerminal|TestInboxWatch|TestIsTerminal' -count=1`
Expected: FAIL to compile with `undefined: parseKeys`, `undefined:
watchState`, and `undefined: isTerminal`.

- [ ] **Step 3: Write the view and the loop**

Create `internal/cli/watch.go`:

```go
package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// watchRefresh is how often sessionhub inbox --watch reads the inbox.
const watchRefresh = 5 * time.Second

// watchHelp is the first line of the view.
const watchHelp = "sessionhub inbox   j/k move  Enter jump  d dismiss  s snooze  r refresh  q quit"

// key is one key press: a printable character such as "j", or a name below.
type key string

const (
	keyUp    key = "up"
	keyDown  key = "down"
	keyEnter key = "enter"
	keyEsc   key = "esc"
	keyCtrlC key = "ctrl+c"
)

// parseKeys splits raw terminal input into keys. Arrow keys arrive as
// ESC [ A and ESC [ B, or ESC O A and ESC O B in application cursor mode.
// Other control bytes are dropped.
func parseKeys(b []byte) []key {
	var out []key
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == 0x1b && i+2 < len(b) && (b[i+1] == '[' || b[i+1] == 'O'):
			switch b[i+2] {
			case 'A':
				out = append(out, keyUp)
			case 'B':
				out = append(out, keyDown)
			}
			i += 2
		case c == 0x1b:
			out = append(out, keyEsc)
		case c == '\r' || c == '\n':
			out = append(out, keyEnter)
		case c == 0x03:
			out = append(out, keyCtrlC)
		case c >= 0x20 && c < 0x7f:
			out = append(out, key(string(rune(c))))
		}
	}
	return out
}

type actionKind int

const (
	actNone actionKind = iota
	actQuit
	actRefresh
	actJump
	actDismiss
	actSnooze
)

// watchAction is what a key asks the loop to do.
type watchAction struct {
	kind  actionKind
	item  api.InboxItem // jump, dismiss, snooze
	until time.Time     // snooze
}

// watchState is what the view shows. Keys change it through handleKey,
// reads through withInbox, and actions through watchAct.
type watchState struct {
	items     []api.InboxItem
	selected  string    // session ID under the cursor; "" with no items
	snoozing  bool      // s was pressed: the next key picks the length
	status    string    // what the last action did
	errText   string    // the last error
	refreshed time.Time // the last good inbox read
}

// cursor is the index of the selected item, or -1.
func (s watchState) cursor() int {
	for i, it := range s.items {
		if it.Session.ID == s.selected {
			return i
		}
	}
	return -1
}

// withInbox replaces the items after a read. The cursor stays on its
// session while it is listed; when it leaves, the cursor moves to the item
// now at its old position, else to the last item. With nothing selected
// before, it starts on the first item.
func (s watchState) withInbox(items []api.InboxItem, now time.Time) watchState {
	old := s.cursor()
	s.items = items
	s.refreshed = now
	switch {
	case s.cursor() >= 0:
	case len(items) == 0:
		s.selected = ""
	case old < 0:
		s.selected = items[0].Session.ID
	case old < len(items):
		s.selected = items[old].Session.ID
	default:
		s.selected = items[len(items)-1].Session.ID
	}
	return s
}

// snoozeKeys maps the key after s to a parseSnooze argument.
var snoozeKeys = map[key]string{"1": "1h", "4": "4h", "t": "tomorrow"}

// handleKey applies one key. q and Ctrl+C always quit. After s, the next key
// picks the snooze length, and any other key (Esc included) cancels.
func handleKey(s watchState, k key, now time.Time) (watchState, watchAction) {
	if k == keyCtrlC || k == "q" {
		return s, watchAction{kind: actQuit}
	}
	i := s.cursor()
	if s.snoozing {
		s.snoozing = false
		arg, ok := snoozeKeys[k]
		if !ok || i < 0 {
			s.status = "snooze cancelled"
			return s, watchAction{}
		}
		until, err := parseSnooze(arg, now)
		if err != nil {
			s.errText = err.Error()
			return s, watchAction{}
		}
		return s, watchAction{kind: actSnooze, item: s.items[i], until: until}
	}
	switch k {
	case "j", keyDown:
		if i >= 0 && i+1 < len(s.items) {
			s.selected = s.items[i+1].Session.ID
		}
	case "k", keyUp:
		if i > 0 {
			s.selected = s.items[i-1].Session.ID
		}
	case "r":
		return s, watchAction{kind: actRefresh}
	case keyEnter:
		if i >= 0 {
			return s, watchAction{kind: actJump, item: s.items[i]}
		}
	case "d":
		if i >= 0 {
			return s, watchAction{kind: actDismiss, item: s.items[i]}
		}
	case "s":
		if i >= 0 {
			s.snoozing = true
			s.status = "snooze for: 1 = 1 hour, 4 = 4 hours, t = until 9:00 tomorrow, Esc = cancel"
		}
	}
	return s, watchAction{}
}

// renderWatch draws the view as exactly height lines (at least 3) of at most
// width runes: the help line, the groups as sessionhub inbox prints them with the
// cursor row marked "> ", and the status line last. When the groups don't
// fit, the window scrolls to keep the cursor row in view.
func renderWatch(s watchState, now time.Time, width, height int) []string {
	if height < 3 {
		height = 3
	}
	var body []string
	cur := -1
	if len(s.items) == 0 && !s.refreshed.IsZero() {
		body = append(body, "nothing needs you")
	}
	for i := 0; i < len(s.items); {
		j := i
		for j < len(s.items) && s.items[j].Group == s.items[i].Group {
			j++
		}
		if i > 0 {
			body = append(body, "")
		}
		heading := inboxHeadings[s.items[i].Group]
		if heading == "" {
			heading = clean(s.items[i].Group, 0)
		}
		body = append(body, cut(fmt.Sprintf("%s (%d)", heading, j-i), width))
		for _, it := range s.items[i:j] {
			line := inboxLine(it, now, width)
			if it.Session.ID == s.selected {
				cur = len(body)
				line = "> " + strings.TrimPrefix(line, "  ")
			}
			body = append(body, line)
		}
		i = j
	}
	avail := height - 2
	start := 0
	if cur >= avail {
		start = cur - avail + 1
	}
	end := start + avail
	if end > len(body) {
		end = len(body)
	}
	out := []string{cut(watchHelp, width)}
	out = append(out, body[start:end]...)
	for len(out) < height-1 {
		out = append(out, "")
	}
	return append(out, cut(watchStatus(s), width))
}

// watchStatus is the last line: the last refresh time, what the last
// action did, and the last error, each cleaned.
func watchStatus(s watchState) string {
	parts := []string{"loading..."}
	if !s.refreshed.IsZero() {
		parts[0] = "updated " + s.refreshed.Format("15:04:05")
	}
	if s.status != "" {
		parts = append(parts, clean(s.status, 0))
	}
	if s.errText != "" {
		parts = append(parts, "error: "+clean(s.errText, 0))
	}
	return strings.Join(parts, "  ")
}

// jumper takes the inbox pane to a session (*resume.Jumper). It returns one
// line for the status line.
type jumper interface {
	Jump(ctx context.Context, s api.Session) (string, error)
}

// watchIO is the loop's outside world. Tests drive it with channels; a nil
// channel never fires.
type watchIO struct {
	keys   <-chan []byte    // raw terminal input, closed at the end of input
	ticks  <-chan time.Time // the refresh timer
	resize <-chan os.Signal // SIGWINCH
	draw   func(lines []string)
	size   func() (width, height int)
}

// watchLoop runs the view until q, Ctrl+C, the end of input, or ctx ends.
// It reads the inbox at start, on every tick, and after every action.
func (e *env) watchLoop(ctx context.Context, w watchIO) error {
	var s watchState
	refresh := func() {
		in, err := e.inbox.Inbox(ctx)
		if err != nil {
			s.errText = "refresh: " + err.Error()
			return
		}
		s = s.withInbox(in.Items, e.now())
	}
	draw := func() {
		width, height := w.size()
		w.draw(renderWatch(s, e.now(), width, height))
	}
	refresh()
	draw()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.ticks:
			if s.errText != "" && strings.HasPrefix(s.errText, "refresh: ") {
				s.errText = ""
			}
			refresh()
		case <-w.resize:
		case b, ok := <-w.keys:
			if !ok {
				return nil
			}
			for _, k := range parseKeys(b) {
				var a watchAction
				s, a = handleKey(s, k, e.now())
				switch a.kind {
				case actQuit:
					return nil
				case actNone:
					continue
				case actJump:
					// ssh can take up to 15 s: say what is going on first.
					s.status, s.errText = "jumping to "+shortID(a.item.Session.ID)+"...", ""
					draw()
				}
				s = e.watchAct(ctx, s, a)
				refresh()
			}
		}
		draw()
	}
}

// watchAct runs one action and puts its outcome in the status line.
func (e *env) watchAct(ctx context.Context, s watchState, a watchAction) watchState {
	id := shortID(a.item.Session.ID)
	s.status, s.errText = "", ""
	switch a.kind {
	case actDismiss:
		if err := e.inbox.DismissInbox(ctx, a.item.Session.ID, a.item.Since); err != nil {
			s.errText = "dismiss " + id + ": " + err.Error()
		} else {
			s.status = "dismissed " + id
		}
	case actSnooze:
		if err := e.inbox.SnoozeInbox(ctx, a.item.Session.ID, a.item.Since, a.until); err != nil {
			s.errText = "snooze " + id + ": " + err.Error()
		} else {
			s.status = "snoozed " + id + " until " + a.until.Format("Mon 2 Jan 15:04")
		}
	case actJump:
		if e.jump == nil {
			s.status = "run: " + a.item.Session.ResumeCommand
			break
		}
		msg, err := e.jump.Jump(ctx, a.item.Session)
		if err != nil {
			s.errText = "jump " + id + ": " + err.Error()
		} else {
			s.status = msg
		}
	}
	return s
}
```

`TestWatchLoopJumpAndErrors` expects the refresh error to keep the rows: the
`refresh` closure leaves `s.items` alone on an error, and `watchStatus`
shows it.

- [ ] **Step 4: Write the terminal part**

Create `internal/cli/watchterm.go`:

```go
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Escape sequences for the alternate screen.
const (
	enterScreen = "\x1b[?1049h\x1b[?25l" // alternate screen, cursor hidden
	leaveScreen = "\x1b[?25h\x1b[?1049l" // cursor shown, main screen back
	clearScreen = "\x1b[H\x1b[2J"
)

// sttyFunc runs stty on the terminal with args and returns its output.
type sttyFunc func(args ...string) (string, error)

// realStty runs stty with stdin on the terminal, which is how stty finds it.
func realStty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return string(out), err
}

// withRawTerminal saves the terminal settings, sets raw mode without echo,
// switches to the alternate screen, and runs fn. It restores the screen and
// the settings on every way out: a return, an error, or a panic (the
// deferred restore runs while the panic unwinds). If the settings can't be
// read or changed, fn does not run.
func withRawTerminal(stty sttyFunc, out io.Writer, fn func() error) error {
	saved, err := stty("-g")
	if err != nil {
		return fmt.Errorf("read the terminal settings: %w", err)
	}
	if _, err := stty("raw", "-echo"); err != nil {
		return fmt.Errorf("set raw mode: %w", err)
	}
	restore := sync.OnceFunc(func() {
		fmt.Fprint(out, leaveScreen)
		stty(strings.TrimSpace(saved))
	})
	defer restore()
	fmt.Fprint(out, enterScreen)
	return fn()
}

// runWatch is sessionhub inbox --watch. SIGINT, SIGTERM, and SIGHUP (the pane
// closing) end the loop, so the terminal is restored. In raw mode Ctrl+C is
// a key, not a signal.
func (e *env) runWatch(ctx context.Context) error {
	if e.isTerminal == nil || !e.isTerminal() {
		return errors.New("inbox --watch: stdin and stdout must be a terminal")
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return withRawTerminal(realStty, os.Stdout, func() error {
		keys := make(chan []byte)
		go readKeys(os.Stdin, keys)
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		t := time.NewTicker(watchRefresh)
		defer t.Stop()
		return e.watchLoop(ctx, watchIO{
			keys:   keys,
			ticks:  t.C,
			resize: winch,
			// Raw mode turns off the newline translation: lines end in \r\n.
			draw: func(lines []string) { fmt.Fprint(os.Stdout, clearScreen+strings.Join(lines, "\r\n")) },
			size: termSize,
		})
	})
}

// readKeys sends each read from r on keys and closes keys at the end of
// input or on an error.
func readKeys(r io.Reader, keys chan<- []byte) {
	defer close(keys)
	buf := make([]byte, 64)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			keys <- append([]byte(nil), buf[:n]...)
		}
		if err != nil {
			return
		}
	}
}
```

In `internal/cli/width.go`, add at the end:

```go
// isTerminal reports whether fd is a terminal: the TCGETS ioctl succeeds.
func isTerminal(fd uintptr) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

// termSize is stdout's width and height in cells, else termWidth and LINES,
// else 24 rows.
func termSize() (width, height int) {
	var ws struct{ row, col, xpixel, ypixel uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno == 0 && ws.col > 0 && ws.row > 0 {
		return int(ws.col), int(ws.row)
	}
	height = 24
	if n, err := strconv.Atoi(os.Getenv("LINES")); err == nil && n > 0 {
		height = n
	}
	return termWidth(), height
}
```

In `internal/cli/cli.go`, replace:

```go
	width func() int // terminal width in columns
	out   io.Writer
	now   func() time.Time
}
```

with:

```go
	width func() int // terminal width in columns
	out   io.Writer
	now   func() time.Time
	// isTerminal reports whether stdin and stdout are a terminal; nil means
	// no (sessionhub inbox --watch refuses to start).
	isTerminal func() bool
	// jump takes sessionhub inbox --watch to a session; nil prints the resume
	// command in the status line instead.
	jump jumper
}
```

and replace:

```go
	return &env{cfg: cfg, api: c, login: c, inbox: c, width: termWidth, out: os.Stdout, now: time.Now}, nil
```

with:

```go
	return &env{cfg: cfg, api: c, login: c, inbox: c, width: termWidth, out: os.Stdout, now: time.Now,
		isTerminal: func() bool { return isTerminal(os.Stdin.Fd()) && isTerminal(os.Stdout.Fd()) }}, nil
```

In `internal/cli/inbox.go`, replace:

```go
const inboxUsage = `usage:
  sessionhub inbox [--json]
```

with:

```go
const inboxUsage = `usage:
  sessionhub inbox [--json | --watch]
```

and in `inboxList`, replace:

```go
	asJSON := fs.Bool("json", false, "print the server response")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return fmt.Errorf("inbox: unexpected arguments\n%s", inboxUsage)
	}
```

with:

```go
	asJSON := fs.Bool("json", false, "print the server response")
	watch := fs.Bool("watch", false, "keep a keyboard-driven list on screen")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return fmt.Errorf("inbox: unexpected arguments\n%s", inboxUsage)
	}
	if *watch {
		if *asJSON {
			return fmt.Errorf("inbox: --watch and --json don't go together\n%s", inboxUsage)
		}
		return e.runWatch(ctx)
	}
```

In `cmd/sessionhub/main.go`, replace:

```go
  inbox [--json]                list the sessions that need you
```

with:

```go
  inbox [--json|--watch]        list the sessions that need you
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/cli/ ./cmd/sessionhub/ -count=1`
Expected: PASS.

- [ ] **Step 6: Try it in a terminal**

Run: `go build -o /tmp/sessionhub-watch ./cmd/sessionhub && /tmp/sessionhub-watch inbox --watch`
Expected: the inbox on the alternate screen with `>` on the first row; **j**
and **k** move; **q** returns to the shell with echo on and the cursor back.
Then run `stty -a | head -2` and check `echo` and `icanon` show without a
`-`. Also run `/tmp/sessionhub-watch inbox --watch < /dev/null`: it prints
`inbox --watch: stdin and stdout must be a terminal` and exits 1. Record the
commands and what you saw in the task report.

- [ ] **Step 7: Document it**

In `docs/cli.md`, in the Commands table, replace the row:

```markdown
| `sessionhub inbox [--json]` | The sessions that need you, across machines, under the headings **Blocked**, **Waiting on you**, and **Finished**, oldest first in each. One line per item: ID prefix, title, machine, how long ago it arrived, `stale` for a stale session, and what it waits on or its recap, cut to the terminal width. `--json` prints the server response. See below. |
```

with:

```markdown
| `sessionhub inbox [--json]` | The sessions that need you, across machines, under the headings **Blocked**, **Waiting on you**, and **Finished**, oldest first in each. One line per item: ID prefix, title, machine, how long ago it arrived, `stale` for a stale session, and what it waits on or its recap, cut to the terminal width. `--json` prints the server response. See below. |
| `sessionhub inbox --watch` | The same list, full screen, refreshed every 5 seconds, with a cursor you move with the keyboard. See below. |
```

In section "`sessionhub inbox`", after the bullet list about triage (the last
bullet ends `on every other machine.`), add:

```markdown
### `sessionhub inbox --watch`

`sessionhub inbox --watch` keeps the inbox on screen and reads it again every 5
seconds and after every action. It needs a terminal: with stdin or stdout
redirected, it exits with an error.

| Key | Does |
|---|---|
| **j** or **Down**, **k** or **Up** | Move the cursor. |
| **Enter** | Jump to the session (see below). |
| **d** | Dismiss the item. |
| **s**, then **1**, **4**, or **t** | Snooze for 1 hour, 4 hours, or until 9:00 tomorrow. Any other key cancels. |
| **r** | Read the inbox now. |
| **q** or **Ctrl+C** | Quit. |

The bottom line shows when the list was last read, what the last action did,
and the last error. The cursor stays on its session across reads; if the
session leaves the inbox, the cursor moves to the row that took its place.
The view uses the terminal's alternate screen and raw mode (through `stty`)
and restores both when it exits, also on `SIGINT`, `SIGTERM`, `SIGHUP`, or a
crash.
```

- [ ] **Step 8: Run the full suite and lint**

Run: `make test && make lint`
Expected: every package `ok`, `gofmt -l` prints nothing, `go vet` clean.

- [ ] **Step 9: Commit**

```bash
git add internal/cli/watch.go internal/cli/watchterm.go internal/cli/watch_test.go internal/cli/width.go \
  internal/cli/cli.go internal/cli/inbox.go internal/cli/inbox_test.go cmd/sessionhub/main.go docs/cli.md
git commit -m "$(cat <<'EOF'
Add sessionhub inbox --watch, a keyboard-driven inbox view

The view is pure functions (keys and reads in, state and actions out)
that tests drive without a terminal; a small loop adds stty raw mode and
the alternate screen, restored on return, error, panic, or signal. The
cursor follows its session across reads. Enter calls a jumper, which
Task 7 provides; without one it shows the resume command.

Refs: docs/dev/superpowers/plans/2026-10-01-inbox-alerts.md, Task 6

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 7: Jump from the inbox pane, and `sessionhub plugin open-inbox`

**Files:**
- Modify: `internal/herdr/herdr.go` (`TabInfo`, `TabList`, `TabFocus`,
  `TabCreateParams`, `TabCreated`, `TabCreate`)
- Modify: `internal/herdr/herdr_test.go` (`TestTabs`)
- Create: `internal/resume/jump.go` (`planJump`, `Jumper`, `NewJumper`,
  `Jump`, `runSSH`, `herdrBin`)
- Create: `internal/resume/jump_test.go`
- Modify: `internal/resume/machines.go` (`openPane`, `openPicker`,
  `openInbox`)
- Modify: `internal/resume/resume.go` (`RunOpenPicker` uses `herdrBin`,
  `RunOpenInbox`)
- Modify: `internal/resume/picker_test.go` (`TestOpenInboxArguments`)
- Modify: `internal/cli/inbox.go` (`RunInbox` sets `e.jump`)
- Modify: `internal/plugin/manifest.go` (action and pane `inbox`)
- Modify: `internal/plugin/install_test.go` (`TestManifestMatchesPlan`)
- Modify: `cmd/sessionhub/main.go` (route, dispatch, usage)
- Modify: `cmd/sessionhub/main_test.go` (route tables)
- Modify: `docs/cli.md`, `docs/plugin.md`, `docs/client.md`

**Interfaces:**
- Consumes: `jumper` and `env.jump` (Task 6); in `internal/resume`:
  `env`, `defaultEnv`, `(*env).local`, `(*env).resumeCmd`, `errNeedsForce`,
  `validID`, `validHost`, `orDefault`, `shellQuote`, `clean`, `remoteHub`;
  test helpers `newFakeHerdr`, `happyHerdr`, `hubServer`, `newEnv`, `sess`,
  `uuid`.
- Produces:
  - `type herdr.TabInfo struct { TabID, WorkspaceID, Label string; Focused bool }`
  - `func (c *herdr.Client) TabList() ([]TabInfo, error)`,
    `TabFocus(id string) error`,
    `TabCreate(p TabCreateParams) (TabCreated, error)`
  - `type resume.Jumper`, `func resume.NewJumper() (*Jumper, error)`,
    `func (j *Jumper) Jump(ctx context.Context, s api.Session) (string, error)`
  - `func resume.RunOpenInbox(ctx context.Context, args []string) error`
  - route key `"plugin open-inbox"`

- [ ] **Step 1: Write the failing herdr client test**

In `internal/herdr/herdr_test.go`, add at the end:

```go
func TestTabs(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("tab.list", func(json.RawMessage) (any, string) {
		return map[string]any{"type": "tab_list", "tabs": []any{
			map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "number": 1, "label": "1", "focused": true, "pane_count": 2, "agent_status": "idle"},
			map[string]any{"tab_id": "w2:t3", "workspace_id": "w2", "number": 3, "label": "sessionhub:bluebox", "focused": false, "pane_count": 1, "agent_status": "unknown"},
		}}, ""
	})
	srv.Handle("tab.focus", func(json.RawMessage) (any, string) { return map[string]any{"type": "ok"}, "" })
	created := map[string]any{"type": "tab_created",
		"tab":       map[string]any{"tab_id": "w1:t4", "workspace_id": "w1", "number": 4, "label": "sessionhub:bluebox", "focused": true, "pane_count": 1, "agent_status": "unknown"},
		"root_pane": map[string]any{"pane_id": "w1:p8", "workspace_id": "w1", "tab_id": "w1:t4"}}
	srv.Handle("tab.create", func(json.RawMessage) (any, string) { return created, "" })
	c := dial(t, srv)

	tabs, err := c.TabList()
	if err != nil || len(tabs) != 2 || tabs[1] != (herdr.TabInfo{TabID: "w2:t3", WorkspaceID: "w2", Label: "sessionhub:bluebox"}) {
		t.Fatalf("TabList = %+v, %v", tabs, err)
	}
	if err := c.TabFocus("w2:t3"); err != nil {
		t.Fatal(err)
	}
	got, err := c.TabCreate(herdr.TabCreateParams{Label: "sessionhub:bluebox", Focus: true})
	if err != nil || got.Tab.TabID != "w1:t4" || got.RootPane.PaneID != "w1:p8" {
		t.Fatalf("TabCreate = %+v, %v", got, err)
	}
	for _, c := range []struct{ method, params string }{
		{"tab.list", `{}`},
		{"tab.focus", `{"tab_id":"w2:t3"}`},
		{"tab.create", `{"label":"sessionhub:bluebox","focus":true}`},
	} {
		reqs := srv.RequestsFor(c.method)
		if len(reqs) != 1 || string(reqs[0].Params) != c.params {
			t.Errorf("%s requests %+v, want params %s", c.method, reqs, c.params)
		}
	}
	// A result without a root pane is an error, so no caller runs a
	// command in pane "".
	delete(created, "root_pane")
	if _, err := c.TabCreate(herdr.TabCreateParams{Label: "x"}); err == nil {
		t.Error("TabCreate without a root pane: no error")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/herdr/... -run TestTabs -count=1`
Expected: FAIL to compile with `c.TabList undefined`.

- [ ] **Step 3: Add the tab calls**

In `internal/herdr/herdr.go`, after `ReportWorkspaceMetadata` (Task 5), add:

```go
// TabInfo is the part of herdr's tab object sessionhub reads.
type TabInfo struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
}

// TabList calls tab.list without a workspace: every tab of this herdr
// session.
func (c *Client) TabList() ([]TabInfo, error) {
	res, err := c.Call("tab.list", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tabs []TabInfo `json:"tabs"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	return out.Tabs, nil
}

// TabFocus calls tab.focus.
func (c *Client) TabFocus(id string) error {
	_, err := c.Call("tab.focus", map[string]string{"tab_id": id})
	return err
}

// TabCreateParams is the part of herdr's TabCreateParams sessionhub sends. An empty
// WorkspaceID lets herdr pick the focused workspace. herdr's tab.create
// cannot run a command.
type TabCreateParams struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	Label       string `json:"label,omitempty"`
	Focus       bool   `json:"focus"`
}

// TabCreated is the part of a tab.create result sessionhub reads.
type TabCreated struct {
	Tab      TabInfo
	RootPane PaneInfo
}

// TabCreate calls tab.create. It errors when the result has no tab or root
// pane, so no caller runs a command in pane "".
func (c *Client) TabCreate(p TabCreateParams) (TabCreated, error) {
	res, err := c.Call("tab.create", p)
	if err != nil {
		return TabCreated{}, err
	}
	var out struct {
		Tab      TabInfo  `json:"tab"`
		RootPane PaneInfo `json:"root_pane"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return TabCreated{}, err
	}
	if out.Tab.TabID == "" || out.RootPane.PaneID == "" {
		return TabCreated{}, errors.New("herdr: tab.create response has no tab or root pane")
	}
	return TabCreated{Tab: out.Tab, RootPane: out.RootPane}, nil
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/herdr/... -count=1`
Expected: PASS.

- [ ] **Step 5: Write the failing jump tests**

Create `internal/resume/jump_test.go`:

```go
package resume

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

func TestPlanJump(t *testing.T) {
	machines := []api.Machine{
		{Name: "bluebox", SSHHost: "bluebox.example.com", HerdrHost: "bluebox.example.com"},
		{Name: "tower", HerdrHost: "tower.example.com"},
		{Name: "bare", SSHHost: "bare.example"},
		{Name: "evil", SSHHost: "-oProxyCommand=x", HerdrHost: "evil.example"},
	}
	bluebox := jumpPlan{kind: jumpRemote, sshHost: "bluebox.example.com", herdrHost: "bluebox.example.com", tab: "sessionhub:bluebox"}
	cases := []struct {
		name, machine, this string
		want                jumpPlan
	}{
		{"this machine", "bluebox", "bluebox", jumpPlan{kind: jumpLocal}},
		{"another machine", "bluebox", "tower", bluebox},
		{"no machine in the client config: never local", "bluebox", "", bluebox},
		{"no ssh_host: the machine name", "tower", "bluebox", jumpPlan{kind: jumpRemote, sshHost: "tower", herdrHost: "tower.example.com", tab: "sessionhub:tower"}},
		{"no herdr_host", "bare", "bluebox", jumpPlan{kind: jumpPrint, why: `machine "bare" has no herdr_host`}},
		{"a host that is an option", "evil", "bluebox", jumpPlan{kind: jumpPrint, why: `machine "evil" has an SSH or herdr host that starts with a hyphen`}},
		{"a machine the server doesn't list", "zed", "bluebox", jumpPlan{kind: jumpPrint, why: `the server lists no machine "zed"`}},
	}
	for _, c := range cases {
		if got := planJump(api.Session{ID: uuid, Machine: c.machine}, c.this, machines); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	if got := planJump(api.Session{ID: uuid, Machine: "bluebox"}, "tower", nil); got.kind != jumpPrint {
		t.Errorf("no machine list: %+v, want a print", got)
	}
}

// newJumper returns a Jumper on e whose ssh records "host command" and
// returns sshErr.
func newJumper(e *env, sshErr error) (*Jumper, *[]string) {
	var calls []string
	return &Jumper{e: e, herdrBin: "/nonexistent/herdr", ssh: func(_ context.Context, host, command string) error {
		calls = append(calls, host+" "+command)
		return sshErr
	}}, &calls
}

func TestJumpLocal(t *testing.T) {
	ctx := context.Background()
	// The pane runs the session: focus it, and print nothing.
	h := happyHerdr(t, "w1:p1", uuid)
	s := sess("bluebox", "w1:p1")
	e, out := newEnv(t, hubServer(t, s), "bluebox", h.path, nil)
	j, calls := newJumper(e, nil)
	msg, err := j.Jump(ctx, s)
	if err != nil || msg != `Focused pane "w1:p1", which still runs session 0a1b2c3d.` {
		t.Errorf("focus: %q %v", msg, err)
	}
	if strings.Join(h.methods(), " ") != "pane.get pane.focus" || len(*calls) != 0 || out.Len() != 0 {
		t.Errorf("focus: herdr %v, ssh %v, printed %q", h.methods(), *calls, out.String())
	}
	// No pane recorded: the resume command, nothing started.
	s = sess("bluebox", "")
	e, out = newEnv(t, hubServer(t, s), "bluebox", filepath.Join(t.TempDir(), "none.sock"), nil)
	j, _ = newJumper(e, nil)
	msg, err = j.Jump(ctx, s)
	want := "no herdr pane to use; run: cd '/home/user/my project' && claude --resume " + uuid
	if err != nil || msg != want || out.Len() != 0 {
		t.Errorf("no pane: %q %v printed %q, want %q", msg, err, out.String(), want)
	}
	// Live but no pane runs it: refuse, and say how to force it.
	s = sess("bluebox", "w1:p1")
	s.Status = api.StatusLive
	h = happyHerdr(t, "", "")
	e, _ = newEnv(t, hubServer(t, s), "bluebox", h.path, nil)
	j, _ = newJumper(e, nil)
	if _, err := j.Jump(ctx, s); err == nil || !strings.Contains(err.Error(), "sessionhub resume --force "+uuid) {
		t.Errorf("live without a pane: %v", err)
	}
	for _, m := range h.methods() {
		if m == "agent.start" || m == "pane.split" {
			t.Errorf("live without a pane started something: %v", h.methods())
		}
	}
	// A hostile ID never reaches herdr or ssh.
	bad := sess("bluebox", "w1:p1")
	bad.ID = "-rf"
	if _, err := j.Jump(ctx, bad); err == nil || !strings.Contains(err.Error(), "refusing session ID") {
		t.Errorf("bad ID: %v", err)
	}
}

func TestJumpRemoteFocusesExistingTab(t *testing.T) {
	f := newFakeHerdr(t, func(m string, p map[string]any) (any, string) {
		switch m {
		case "tab.list":
			return map[string]any{"type": "tab_list", "tabs": []any{
				map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "label": "1", "focused": true},
				map[string]any{"tab_id": "w2:t3", "workspace_id": "w2", "label": "sessionhub:bluebox"},
			}}, ""
		case "tab.focus":
			return map[string]any{"type": "ok"}, ""
		}
		return nil, "no_fixture"
	})
	s := sess("bluebox", "w1:p1")
	e, out := newEnv(t, hubServer(t, s), "tower", f.path, nil)
	j, calls := newJumper(e, nil)
	msg, err := j.Jump(context.Background(), s)
	if err != nil || msg != "sessionhub resume ran on bluebox; showing it in tab sessionhub:bluebox" {
		t.Errorf("Jump = %q, %v", msg, err)
	}
	if want := []string{"bluebox.example.com ~/.local/bin/sessionhub resume " + uuid}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("ssh calls %q, want %q", *calls, want)
	}
	if strings.Join(f.methods(), " ") != "tab.list tab.focus" || f.params("tab.focus")["tab_id"] != "w2:t3" {
		t.Errorf("herdr %v, focus %v", f.methods(), f.params("tab.focus"))
	}
	if out.Len() != 0 {
		t.Errorf("printed %q", out.String())
	}
}

func TestJumpRemoteCreatesTab(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFakeHerdr(t, func(m string, p map[string]any) (any, string) {
		switch m {
		case "tab.list":
			return map[string]any{"type": "tab_list", "tabs": []any{
				map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "label": "1", "focused": true}}}, ""
		case "tab.create":
			return map[string]any{"type": "tab_created",
				"tab":       map[string]any{"tab_id": "w1:t4", "workspace_id": "w1", "label": "sessionhub:bluebox", "focused": true},
				"root_pane": map[string]any{"pane_id": "w1:p8", "workspace_id": "w1", "tab_id": "w1:t4"}}, ""
		}
		return nil, "no_fixture"
	})
	s := sess("bluebox", "w1:p1")
	e, _ := newEnv(t, hubServer(t, s), "tower", f.path, nil)
	j, _ := newJumper(e, nil)
	j.herdrBin = bin
	msg, err := j.Jump(context.Background(), s)
	if err != nil || msg != "sessionhub resume ran on bluebox; showing it in tab sessionhub:bluebox" {
		t.Errorf("Jump = %q, %v", msg, err)
	}
	if strings.Join(f.methods(), " ") != "tab.list tab.create" ||
		!reflect.DeepEqual(f.params("tab.create"), map[string]any{"label": "sessionhub:bluebox", "focus": true}) {
		t.Errorf("herdr %v, create %v", f.methods(), f.params("tab.create"))
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimRight(string(b), "\n"), "\n"); !reflect.DeepEqual(got, []string{"pane", "run", "w1:p8", "herdr --remote bluebox.example.com"}) {
		t.Errorf("herdr args %q", got)
	}
}

func TestJumpRemoteFallbacks(t *testing.T) {
	ctx := context.Background()
	s := sess("bluebox", "w1:p1")
	// ssh fails: the resume command, and no local tab.
	f := newFakeHerdr(t, func(string, map[string]any) (any, string) { return nil, "no_fixture" })
	e, _ := newEnv(t, hubServer(t, s), "tower", f.path, nil)
	j, _ := newJumper(e, errors.New("exit status 255: Permission denied (publickey)"))
	msg, err := j.Jump(ctx, s)
	want := "ssh bluebox.example.com failed (exit status 255: Permission denied (publickey)); run: " + s.ResumeCommand
	if err != nil || msg != want || len(f.methods()) != 0 {
		t.Errorf("ssh failure: %q %v, herdr %v; want %q", msg, err, f.methods(), want)
	}
	// ssh works but the local herdr refuses: the herdr --remote command.
	j, _ = newJumper(e, nil)
	msg, err = j.Jump(ctx, s)
	if err != nil || !strings.HasPrefix(msg, "sessionhub resume ran on bluebox, but the local tab failed") ||
		!strings.HasSuffix(msg, "run: herdr --remote bluebox.example.com") {
		t.Errorf("tab failure: %q %v", msg, err)
	}
	// A machine the server doesn't list: no ssh at all.
	zed := sess("zed", "w1:p1")
	e, _ = newEnv(t, hubServer(t, zed), "tower", f.path, nil)
	j, calls := newJumper(e, nil)
	msg, err = j.Jump(ctx, zed)
	if err != nil || msg != `the server lists no machine "zed"; run: `+zed.ResumeCommand || len(*calls) != 0 {
		t.Errorf("unknown machine: %q %v ssh %v", msg, err, *calls)
	}
}
```

In `internal/resume/picker_test.go`, add at the end:

```go
func TestOpenInboxArguments(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := openInbox(context.Background(), bin, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argsFile)
	got := strings.Join(strings.Fields(string(b)), " ")
	want := "plugin pane open --plugin sessionhub --entrypoint inbox --placement split --direction right --focus"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./internal/resume/ -run 'TestPlanJump|TestJump|TestOpenInboxArguments|TestOpenPickerArguments' -count=1`
Expected: FAIL to compile with `undefined: jumpPlan`, `undefined: Jumper`,
and `undefined: openInbox`.

- [ ] **Step 7: Write the jump**

Create `internal/resume/jump.go`:

```go
package resume

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/herdr"
)

// Jump time limits.
const (
	jumpSSHTimeout = 15 * time.Second // sessionhub resume on the other machine
	paneRunTimeout = 5 * time.Second  // herdr pane run in the new local tab
)

type jumpKind int

const (
	jumpLocal  jumpKind = iota // the session runs on this machine
	jumpRemote                 // ssh, then a local tab on the machine's herdr
	jumpPrint                  // only the resume command can be shown
)

// jumpPlan is what Enter in the inbox pane does for one session.
type jumpPlan struct {
	kind      jumpKind
	sshHost   string // jumpRemote
	herdrHost string // jumpRemote
	tab       string // jumpRemote: the local tab label, sessionhub:<machine>
	why       string // jumpPrint: why only the command can be shown
}

// planJump decides how to reach session s from machine this (the client
// config's machine; "" never matches). machines is the server's list. The
// server fills herdr_host when a machine is added, so a machine without one,
// or missing from the list, means the list failed or the machine was
// removed, and the plan is to show the resume command.
func planJump(s api.Session, this string, machines []api.Machine) jumpPlan {
	if this != "" && s.Machine == this {
		return jumpPlan{kind: jumpLocal}
	}
	for _, m := range machines {
		if m.Name != s.Machine {
			continue
		}
		ssh := orDefault(m.SSHHost, m.Name)
		switch {
		case m.HerdrHost == "":
			return jumpPlan{kind: jumpPrint, why: fmt.Sprintf("machine %s has no herdr_host", strconv.Quote(m.Name))}
		case !validHost(ssh) || !validHost(m.HerdrHost):
			return jumpPlan{kind: jumpPrint, why: fmt.Sprintf("machine %s has an SSH or herdr host that starts with a hyphen", strconv.Quote(m.Name))}
		}
		return jumpPlan{kind: jumpRemote, sshHost: ssh, herdrHost: m.HerdrHost, tab: "sessionhub:" + s.Machine}
	}
	return jumpPlan{kind: jumpPrint, why: fmt.Sprintf("the server lists no machine %s", strconv.Quote(s.Machine))}
}

// Jumper takes the inbox pane (sessionhub inbox --watch) to a session. On this
// machine it does what sessionhub resume does: focus the session's pane, or start
// the session in one. On another machine it runs sessionhub resume there over
// SSH, then focuses or opens a local herdr tab attached to that machine's
// herdr.
type Jumper struct {
	e *env
	// ssh runs command on host; tests replace it.
	ssh func(ctx context.Context, host, command string) error
	// herdrBin runs `herdr pane run` in a new tab.
	herdrBin string
}

// NewJumper returns a Jumper on the client config, the local herdr socket,
// and the real ssh.
func NewJumper() (*Jumper, error) {
	e, err := defaultEnv()
	if err != nil {
		return nil, err
	}
	e.in = strings.NewReader("")
	return &Jumper{e: e, ssh: runSSH, herdrBin: herdrBin()}, nil
}

// Jump returns one line for the inbox pane's status line. An error means
// nothing happened. A message that ends in "run: <command>" means sessionhub could
// only show the command.
func (j *Jumper) Jump(ctx context.Context, s api.Session) (string, error) {
	if !validID(s.ID) {
		return "", fmt.Errorf("refusing session ID %q: expected letters, digits, and hyphens, not starting with a hyphen", clean(s.ID))
	}
	var machines []api.Machine
	if j.e.cfg.Machine == "" || s.Machine != j.e.cfg.Machine {
		machines, _ = j.e.api.ListMachines(ctx) // a failed list plans a print
	}
	p := planJump(s, j.e.cfg.Machine, machines)
	switch p.kind {
	case jumpLocal:
		return j.local(s)
	case jumpRemote:
		return j.remote(ctx, s, p), nil
	}
	return p.why + "; run: " + s.ResumeCommand, nil
}

// local is sessionhub resume on this machine, with its output kept for the status
// line instead of printed.
func (j *Jumper) local(s api.Session) (string, error) {
	var out bytes.Buffer
	e := *j.e
	e.out = &out
	acted, err := e.local(s)
	if errors.Is(err, errNeedsForce) {
		return "", fmt.Errorf("the server reports it %s but no pane runs it; run: sessionhub resume --force %s", clean(s.Status), s.ID)
	}
	if err != nil {
		return "", err
	}
	if !acted {
		return "no herdr pane to use; run: " + e.resumeCmd(s), nil
	}
	return firstLine(out.String()), nil
}

// remote runs sessionhub resume on the session's machine, so its herdr focuses or
// starts the session, then shows that herdr in the local tab sessionhub:<machine>.
func (j *Jumper) remote(ctx context.Context, s api.Session, p jumpPlan) string {
	sctx, cancel := context.WithTimeout(ctx, jumpSSHTimeout)
	defer cancel()
	if err := j.ssh(sctx, p.sshHost, remoteHub+" resume "+s.ID); err != nil {
		return fmt.Sprintf("ssh %s failed (%v); run: %s", p.sshHost, err, s.ResumeCommand)
	}
	if err := j.openTab(p); err != nil {
		return fmt.Sprintf("sessionhub resume ran on %s, but the local tab failed (%v); run: herdr --remote %s", s.Machine, err, shellQuote(p.herdrHost))
	}
	return fmt.Sprintf("sessionhub resume ran on %s; showing it in tab %s", s.Machine, p.tab)
}

// openTab focuses the local tab named p.tab, or creates it, focused, and
// runs herdr --remote <herdr_host> in it. tab.create can't run a command,
// so the command goes in with `herdr pane run`, which types it and presses
// Enter.
func (j *Jumper) openTab(p jumpPlan) error {
	h, err := herdr.Dial(j.e.socket)
	if err != nil {
		return err
	}
	tabs, err := h.TabList()
	if err != nil {
		return err
	}
	for _, t := range tabs {
		if t.Label == p.tab {
			return h.TabFocus(t.TabID)
		}
	}
	created, err := h.TabCreate(herdr.TabCreateParams{Label: p.tab, Focus: true})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), paneRunTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, j.herdrBin, "pane", "run", created.RootPane.PaneID, "herdr --remote "+shellQuote(p.herdrHost))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("herdr pane run: %w: %s", err, termtext.Clean(string(out), 200))
	}
	return nil
}

// runSSH runs command on host with BatchMode, so ssh never waits on a
// password prompt, and returns ssh's error with its cleaned stderr. The
// remote shell expands the ~ in the command.
func runSSH(ctx context.Context, host, command string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", host, command)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := termtext.Clean(stderr.String(), 200); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// firstLine is the first non-empty line of s, cleaned.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = clean(l); l != "" {
			return l
		}
	}
	return ""
}

// herdrBin is HERDR_BIN_PATH, else "herdr".
func herdrBin() string {
	if b := os.Getenv("HERDR_BIN_PATH"); b != "" {
		return b
	}
	return "herdr"
}
```

- [ ] **Step 8: Add `sessionhub plugin open-inbox`**

In `internal/resume/machines.go`, replace:

```go
func openPicker(ctx context.Context, bin string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, "plugin", "pane", "open", "--plugin", "sessionhub",
		"--entrypoint", "resume-picker", "--placement", "overlay", "--focus")
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}
```

with:

```go
func openPicker(ctx context.Context, bin string, stdout, stderr io.Writer) error {
	return openPane(ctx, bin, stdout, stderr, "resume-picker", "--placement", "overlay")
}

// openInbox opens the inbox pane (sessionhub inbox --watch) to the right of the
// focused pane.
func openInbox(ctx context.Context, bin string, stdout, stderr io.Writer) error {
	return openPane(ctx, bin, stdout, stderr, "inbox", "--placement", "split", "--direction", "right")
}

// openPane runs `herdr plugin pane open` for one of the sessionhub plugin's panes,
// focused.
func openPane(ctx context.Context, bin string, stdout, stderr io.Writer, entrypoint string, placement ...string) error {
	args := append([]string{"plugin", "pane", "open", "--plugin", "sessionhub", "--entrypoint", entrypoint}, placement...)
	cmd := exec.CommandContext(ctx, bin, append(args, "--focus")...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}
```

In `internal/resume/resume.go`, replace:

```go
func RunOpenPicker(ctx context.Context, args []string) error {
	bin := os.Getenv("HERDR_BIN_PATH")
	if bin == "" {
		bin = "herdr"
	}
	return openPicker(ctx, bin, os.Stdout, os.Stderr)
}
```

with:

```go
func RunOpenPicker(ctx context.Context, args []string) error {
	return openPicker(ctx, herdrBin(), os.Stdout, os.Stderr)
}

// RunOpenInbox implements `sessionhub plugin open-inbox`: it opens the herdr
// plugin's inbox pane, which runs sessionhub inbox --watch, to the right.
func RunOpenInbox(ctx context.Context, args []string) error {
	return openInbox(ctx, herdrBin(), os.Stdout, os.Stderr)
}
```

- [ ] **Step 9: Run the resume tests to verify they pass**

Run: `go test ./internal/resume/ -count=1`
Expected: PASS.

- [ ] **Step 10: Wire the jump, the manifest, and the route**

In `internal/cli/inbox.go`, add `"github.com/abdallah/session-hub/internal/resume"` to the
imports, and replace:

```go
func RunInbox(ctx context.Context, args []string) error {
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	return e.inboxCmd(ctx, args)
}
```

with:

```go
func RunInbox(ctx context.Context, args []string) error {
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	if j, err := resume.NewJumper(); err == nil {
		e.jump = j
	}
	return e.inboxCmd(ctx, args)
}

// Compile-time check that resume's Jumper is what sessionhub inbox --watch calls.
var _ jumper = (*resume.Jumper)(nil)
```

In `internal/plugin/manifest.go`, replace:

```go
	b.WriteString(cmd("plugin", "open-picker"))
	b.WriteString(`
[[panes]]
id = "resume-picker"
title = "sessionhub: resume"
placement = "overlay"
`)
	b.WriteString(cmd("resume", "--pick"))
	return b.String()
```

with:

```go
	b.WriteString(cmd("plugin", "open-picker"))
	b.WriteString(`
[[actions]]
id = "inbox"
title = "sessionhub: inbox"
contexts = ["global"]
`)
	b.WriteString(cmd("plugin", "open-inbox"))
	b.WriteString(`
[[panes]]
id = "resume-picker"
title = "sessionhub: resume"
placement = "overlay"
`)
	b.WriteString(cmd("resume", "--pick"))
	b.WriteString(`
[[panes]]
id = "inbox"
title = "sessionhub: inbox"
placement = "split"
`)
	b.WriteString(cmd("inbox", "--watch"))
	return b.String()
```

In `internal/plugin/install_test.go` (`TestManifestMatchesPlan`), replace:

```go
	if len(m.Actions) != 2 ||
		m.Actions[0].ID != "status" || m.Actions[0].Title != "sessionhub: sessions on this machine" || cmd(m.Actions[0].Command) != bin+" status" ||
		m.Actions[1].ID != "resume" || m.Actions[1].Title != "sessionhub: resume a session" || cmd(m.Actions[1].Command) != bin+" plugin open-picker" ||
		cmd(m.Actions[0].Contexts) != "global" || cmd(m.Actions[1].Contexts) != "global" {
		t.Errorf("actions %+v", m.Actions)
	}
	if len(m.Panes) != 1 || m.Panes[0].ID != "resume-picker" || m.Panes[0].Title != "sessionhub: resume" ||
		m.Panes[0].Placement != "overlay" || cmd(m.Panes[0].Command) != bin+" resume --pick" {
		t.Errorf("panes %+v", m.Panes)
	}
```

with:

```go
	if len(m.Actions) != 3 ||
		m.Actions[0].ID != "status" || m.Actions[0].Title != "sessionhub: sessions on this machine" || cmd(m.Actions[0].Command) != bin+" status" ||
		m.Actions[1].ID != "resume" || m.Actions[1].Title != "sessionhub: resume a session" || cmd(m.Actions[1].Command) != bin+" plugin open-picker" ||
		m.Actions[2].ID != "inbox" || m.Actions[2].Title != "sessionhub: inbox" || cmd(m.Actions[2].Command) != bin+" plugin open-inbox" ||
		cmd(m.Actions[0].Contexts) != "global" || cmd(m.Actions[1].Contexts) != "global" || cmd(m.Actions[2].Contexts) != "global" {
		t.Errorf("actions %+v", m.Actions)
	}
	if len(m.Panes) != 2 || m.Panes[0].ID != "resume-picker" || m.Panes[0].Title != "sessionhub: resume" ||
		m.Panes[0].Placement != "overlay" || cmd(m.Panes[0].Command) != bin+" resume --pick" ||
		m.Panes[1].ID != "inbox" || m.Panes[1].Title != "sessionhub: inbox" ||
		m.Panes[1].Placement != "split" || cmd(m.Panes[1].Command) != bin+" inbox --watch" {
		t.Errorf("panes %+v", m.Panes)
	}
```

In `cmd/sessionhub/main.go`, replace:

```go
  plugin open-picker            open the session picker
```

with:

```go
  plugin open-picker            open the session picker
  plugin open-inbox             open the inbox pane
```

replace:

```go
	"plugin open-picker": resume.RunOpenPicker,
```

with:

```go
	"plugin open-picker": resume.RunOpenPicker,
	"plugin open-inbox":  resume.RunOpenInbox,
```

and replace:

```go
	if cmd == "plugin" && len(rest) > 0 && rest[0] == "open-picker" {
		h, ok, rest = routes["plugin open-picker"], true, rest[1:]
	}
```

with:

```go
	if cmd == "plugin" && len(rest) > 0 {
		if sub, found := routes["plugin "+rest[0]]; found {
			h, ok, rest = sub, true, rest[1:]
		}
	}
```

Also update the comment above `routes`: replace `// routes maps a command (or "plugin open-picker") to its handler.` with
`// routes maps a command (or "plugin open-picker" and "plugin open-inbox") to its handler.`

In `cmd/sessionhub/main_test.go`, after the `commands` variable, add:

```go
// pluginSubs are the plugin subcommands with their own route.
var pluginSubs = []string{"plugin open-picker", "plugin open-inbox"}
```

replace both occurrences of `append(commands, "plugin open-picker")` with
`append(commands, pluginSubs...)`, and in `TestDispatchRoutes` add after the
`open-picker` case:

```go
		{[]string{"plugin", "open-inbox"}, "plugin open-inbox", []string{}},
```

- [ ] **Step 11: Run the tests to verify they pass**

Run: `go test ./internal/cli/ ./internal/plugin/ ./internal/resume/ ./cmd/sessionhub/ -count=1`
Expected: PASS.

- [ ] **Step 12: Document it**

In `docs/cli.md`, in the Commands table, after the `sessionhub plugin open-picker`
row, add:

```markdown
| `sessionhub plugin open-inbox` | Runs `herdr plugin pane open --plugin sessionhub --entrypoint inbox --placement split --direction right --focus`, which runs `sessionhub inbox --watch` in a pane to the right. It uses `HERDR_BIN_PATH` when set. |
```

At the end of the "`sessionhub inbox --watch`" subsection (Task 6), add:

```markdown
**Enter** jumps to the session under the cursor:

- **On this machine:** the same as `sessionhub resume <id>` (focus the session's
  pane, or start it in a pane), without printing the resume command. If
  there is no herdr pane to use, the status line shows the command.
- **On another machine:**
  1. `ssh -o BatchMode=yes <ssh_host> '~/.local/bin/sessionhub resume <id>'`, with
     a 15-second limit, so that machine's herdr focuses or starts the
     session.
  2. If a local herdr tab named `sessionhub:<machine>` exists, it is focused.
     Otherwise sessionhub creates it, focused, and runs `herdr --remote
     <herdr_host>` in it (with `herdr pane run`). An existing tab is focused
     as it is, even if its `herdr --remote` has exited.
  3. If the server lists no such machine, the machine has no `herdr_host`,
     or `ssh` fails, the status line shows the resume command instead.
     `BatchMode` means `ssh` never waits for a password: set up key
     authentication for the jump to work.
```

In `docs/plugin.md`, section "What each hook does", after the row for action
`resume`, add:

```markdown
| action `inbox` | `sessionhub plugin open-inbox` | Opens the `inbox` pane (`sessionhub inbox --watch`) split to the right. Bind it to a key like the picker. |
```

In `docs/client.md`, section "`internal/herdr`", insert this bullet before
the bullet that starts `- **One request per connection.**`:

```markdown
- `TabList()`, `TabFocus(id)`, and `TabCreate(TabCreateParams)` (`tab.list`,
  `tab.focus`, `tab.create`) let the inbox pane find or open the
  `sessionhub:<machine>` tab. `tab.create` returns the tab and its root pane; a
  result without either is an error. It cannot run a command, so the caller
  runs `herdr pane run <pane> <command>`.
```

- [ ] **Step 13: Run the full suite and lint**

Run: `make test && make lint`
Expected: every package `ok`, `gofmt -l` prints nothing, `go vet` clean.

- [ ] **Step 14: Try the pane in herdr**

Run: `make install && sessionhub install-plugin && sessionhub plugin open-inbox`
Expected: a pane opens to the right running the inbox view. Press **q**: the
pane closes. Record the commands and what you saw in the task report. The
cross-machine jump is checked in "Deploy and live check" below.

- [ ] **Step 15: Commit**

```bash
git add internal/herdr/herdr.go internal/herdr/herdr_test.go internal/resume/jump.go internal/resume/jump_test.go \
  internal/resume/machines.go internal/resume/resume.go internal/resume/picker_test.go internal/cli/inbox.go \
  internal/plugin/manifest.go internal/plugin/install_test.go cmd/sessionhub/main.go cmd/sessionhub/main_test.go \
  docs/cli.md docs/plugin.md docs/client.md
git commit -m "$(cat <<'EOF'
Jump to a session from the inbox pane, on any machine

Enter in sessionhub inbox --watch reuses sessionhub resume's local logic without
printing. For another machine it runs sessionhub resume there with ssh
BatchMode (15 s), then focuses the local tab sessionhub:<machine> or creates it
and runs herdr --remote <herdr_host> with herdr pane run, since
tab.create cannot run a command. The plugin gets an inbox pane and
action, and sessionhub plugin open-inbox opens it.

Refs: docs/dev/superpowers/plans/2026-10-01-inbox-alerts.md, Task 7

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 8: README, SPEC, and PLAN

**Files:**
- Modify: `README.md` (Telegram setup on `tower`, the sidebar row, "Use it",
  "Upgrade to inbox alerts")
- Modify: `docs/dev/SPEC.md` (section 8)
- Modify: `docs/dev/PLAN.md` (new "Inbox alerts, as built" section)

**Interfaces:**
- Consumes: the behavior Tasks 1 to 7 built and documented in
  `docs/server.md`, `docs/plugin.md`, `docs/cli.md`, and `docs/client.md`.
- Produces: user-facing setup and upgrade steps, and the as-built record.

- [ ] **Step 1: README: Telegram setup**

In `README.md`, after the subsection "### How the unit starts at boot" and
before "## Join each machine", add:

````markdown
### Turn on Telegram alerts

The server can send you a Telegram message when a session has been blocked
on a prompt for 30 seconds. It can reuse a bot you already have, such as one
of Hermes' bots: sessionhub only sends messages, so it never competes with Hermes
for the bot's updates. sessionhub never reads Hermes' files; you copy the two values
yourself.

1. Find the bot token and your chat ID where you keep them for Hermes.
2. On `tower`, add them to `~/.config/sessionhub/server.toml`:

   ```toml
   telegram_bot_token = "<bot-token>"
   telegram_chat_id = "<chat-id>"
   ```

   Quote the chat ID, even though it is a number.
3. Check that the file is still private: `stat -c %a
   ~/.config/sessionhub/server.toml` prints `600`.
4. Restart the server and check the log:

   ```sh
   systemctl --user restart sessionhub
   journalctl --user -u sessionhub -n 20 | grep 'telegram alerts'
   ```

   It prints `telegram alerts are on`. With either value missing it prints
   `telegram alerts are off`.

Each message names the session, its machine, and how long it has been
blocked, with an **Open inbox** button that opens the dashboard's **Inbox**
tab, and a **Remote Control** button when the session has a Remote Control
link. See [`docs/server.md`](docs/server.md), "Telegram alerts".
````

- [ ] **Step 2: README: sidebar and use**

In "### Add the herdr sidebar row", after the existing `toml` block, add:

````markdown
To show the inbox count on each workspace (for example `3 · 1 blocked`),
add `$inbox` to `[ui.sidebar.spaces]`:

```toml
[ui.sidebar.spaces]
rows = [["state_icon", "workspace", "$inbox"], ["branch", "git_status"]]
```
````

In "## Use it", replace the bullet:

```markdown
- The dashboard's **Inbox** tab, and `sessionhub inbox` in a terminal, list the
  sessions that need you: blocked on a prompt, waiting on you, or finished a
  turn you haven't answered. Open one from there, or dismiss or snooze it.
  Triage is shared across browsers and machines.
```

with:

```markdown
- The dashboard's **Inbox** tab, and `sessionhub inbox` in a terminal, list the
  sessions that need you: blocked on a prompt, waiting on you, or finished a
  turn you haven't answered. Open one from there, or dismiss or snooze it.
  Triage is shared across browsers and machines. When herdr marks a finished
  pane seen, its **Finished** item leaves on its own.
- A blocked session plays herdr's request sound with a notification on its
  machine and, after 30 seconds, sends you a Telegram message if you set
  that up (see "Turn on Telegram alerts").
- In herdr, the plugin's **sessionhub: inbox** action, or `sessionhub plugin open-inbox`,
  opens the inbox in a pane. **j** and **k** move, **Enter** jumps to the
  session on any machine, **d** dismisses, **s** snoozes, and **q** quits.
  A jump to another machine runs `ssh -o BatchMode=yes`, so it needs the
  key-based SSH setup below and a current Cloudflare Access login; without
  them it shows the resume command instead.
```

- [ ] **Step 3: README: upgrade**

Before "### Upgrade to the inbox", add:

```markdown
### Upgrade to inbox alerts

This release moves the database to schema version 7. Deploy with
`make deploy`, which backs up the database first and reinstalls the plugin on
`tower`; the first start upgrades the database. On each other machine, run
`make install`, then `sessionhub install-plugin`: the plugin manifest gains the
`inbox` pane and action, and the new watcher writes the sidebar count. Then
turn on Telegram alerts and add the `$inbox` sidebar row if you want them.

To roll back, stop the server, restore the database backup as described
above, install the previous binary on `tower`, and start the server. The
version 6 binary refuses a version 7 database, so restoring the backup is the
only way back. On the other machines, install the previous binary and run
`sessionhub install-plugin` again.
```

- [ ] **Step 4: SPEC**

In `docs/dev/SPEC.md`, section "### 8. Inbox", replace:

```markdown
Answering prompts, push notifications, and the herdr sidebar are out of scope. The design is in `docs/dev/superpowers/specs/2026-10-01-inbox-design.md`.
```

with:

```markdown
A blocked session reaches you: a Telegram message from the server (after 30 seconds, once per block) and a herdr notification with a sound on the session's machine. When herdr marks a finished pane seen, its **Finished** item is dismissed. The herdr sidebar shows the inbox count on each workspace, and `sessionhub inbox --watch`, a herdr plugin pane, jumps to a session on any machine. Answering prompts from Telegram or the inbox pane is out of scope. The designs are in `docs/dev/superpowers/specs/2026-10-01-inbox-design.md` and `docs/dev/superpowers/specs/2026-10-01-inbox-alerts-design.md`.
```

- [ ] **Step 5: PLAN**

In `docs/dev/PLAN.md`, after the "## Inbox, as built" section and before
"## Tests and captured payloads", add:

```markdown
## Inbox alerts, as built

The design is in `docs/dev/superpowers/specs/2026-10-01-inbox-alerts-design.md`.
The build settles these points the spec leaves open:

- Schema version 7 adds `inbox_alerts`. Rollback is restoring the database
  backup; the version 6 binary refuses a version 7 database.
- The herdr notification and the sidebar count use herdr's socket methods
  (`notification.show`, `workspace.report_metadata`), the same calls as the
  spec's `herdr notification show` and `herdr workspace report-metadata`
  lines. The jump uses `tab.list`, `tab.focus`, and `tab.create` on the
  socket, and `herdr pane run` for the command, because `tab.create` cannot
  run one.
- herdr's `pane.agent_status_changed` payload has no title, so the
  notification title comes from `pane.get`'s `terminal_title_stripped`, else
  `terminal_title`, else `Claude`. The body is the client config's
  `machine`, else the host name.
- A Blocked item carries no `waiting_on`. The Telegram message's third line
  is the latest report's `waiting_on` when no prompt came after it, else the
  recap, and it is left out when both are empty.
- The **Remote Control** button needs an `https` link; Telegram rejects
  others.
- The notifier ends a tick at the first failed send. HTTP 429 without
  `retry_after` waits 30 seconds. If recording an alert fails after a
  successful send, the next tick may send it again.
- The bot token is a `Secret` that prints as `[redacted]`, errors are
  unwrapped from `*url.Error` (whose text holds the URL and the token), and
  every failure line is redacted again before it is logged.
- The sidebar skip rule is per workspace, a refused report is retried at the
  next heartbeat, and a new watcher's first heartbeat reports every
  workspace, a clear included. herdr shows the token with `$inbox` in
  `[ui.sidebar.spaces]` rows.
- "No `herdr_host`" means the server lists no such machine, or its
  `herdr_host` is empty, or a host starts with a hyphen. The server fills
  `herdr_host` when a machine is added.
- An existing `sessionhub:<machine>` tab is focused as it is, even if its
  `herdr --remote` has exited.
- The plugin also gets an action `inbox` ("sessionhub: inbox") that runs
  `sessionhub plugin open-inbox`.
- In the inbox pane, **Esc** or any key other than **1**, **4**, or **t**
  cancels a pending snooze; **q** and **Ctrl+C** quit even then. The cursor
  follows its session across reads, else takes the row at its old position,
  else the last row.
- The dashboard also follows `#sessions` and later `hashchange` events.
```

- [ ] **Step 6: Check the docs build nothing and break nothing**

Run: `make test && make lint`
Expected: every package `ok` (no code changed), `gofmt -l` prints nothing.
Then run `grep -rn "TEST-PLACEHOLDER\|bot[0-9]" README.md docs/dev/SPEC.md docs/dev/PLAN.md docs/*.md`
Expected: no output: no token, real or placeholder, in the docs.

- [ ] **Step 7: Commit**

```bash
git add README.md docs/dev/SPEC.md docs/dev/PLAN.md
git commit -m "$(cat <<'EOF'
Document inbox alerts: Telegram setup, sidebar, inbox pane

README covers turning on Telegram alerts on tower with values you copy
yourself (sessionhub never reads Hermes' files), the $inbox sidebar row, the
inbox pane, and the upgrade to schema 7 with its backup-only rollback.
SPEC section 8 and docs/dev/PLAN.md's as-built notes follow the build.

Refs: docs/dev/superpowers/plans/2026-10-01-inbox-alerts.md, Task 8

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

## Deploy and live check (notes for the controller)

These steps run on the real machines. They are not a task: the controller
runs them with the user's approval, after Task 8. Never paste the bot token
into a command line, a log excerpt, or the evidence file.

1. **Full suite.** Run `make test lint` and record the package count and `0`
   failures.
2. **Deploy.** Run `make deploy` (backs up `sessionhub.db` on `tower`, swaps the
   binary, restarts the server, reinstalls the plugin). On `bluebox`, run
   `make install`, then `sessionhub install-plugin`.
3. **Confirm the upgrade.** On `tower`:
   `sqlite3 ~/.local/share/sessionhub/sessionhub.db 'PRAGMA user_version'` prints `7`.
   `journalctl --user -u sessionhub -n 50 | grep 'telegram alerts'` prints
   `telegram alerts are on`. Check that the token never reached the journal
   without printing it:
   `journalctl --user -u sessionhub --since today | grep -cF "$(sed -n 's/^telegram_bot_token *= *"\(.*\)"/\1/p' ~/.config/sessionhub/server.toml)"`
   prints `0`.
4. **Blocked alert.** In a scratch Claude session in herdr on `tower`, ask
   for something that needs a permission prompt, and leave it.
   - At once: a herdr notification `Blocked: <title>` with the request sound.
   - Within 45 seconds: one Telegram message with the title, `tower · blocked
     <age> ago`, and an **Open inbox** button. Tap it: the dashboard opens on
     the **Inbox** tab.
   - `sqlite3 ~/.local/share/sessionhub/sessionhub.db 'SELECT session_id, since, sent_at FROM inbox_alerts'`
     shows one row. Wait two more minutes: no second message.
   - Answer a second prompt within 10 seconds: no Telegram message for it.
5. **Seen clears Finished.** Let the scratch session finish a turn while you
   look at another pane; `sessionhub inbox` lists it under **Finished**. Focus its
   pane in herdr. Within a minute it leaves `sessionhub inbox`, and
   `sqlite3 ~/.local/share/sessionhub/sessionhub.db "SELECT triaged_since, snooze_until FROM inbox_triage WHERE session_id LIKE '<prefix>%'"`
   shows a row with an empty `snooze_until`. Send another prompt and let it
   finish: it comes back.
6. **Sidebar.** Add the `$inbox` row from the README to
   `~/.config/herdr/config.toml`, run `herdr server reload-config`, and wait
   up to a minute: each workspace shows the count, for example `2 · 1
   blocked`. Dismiss items until the inbox is empty: the count goes within a
   minute. If herdr does not render the workspace token, record what
   `herdr workspace get <workspace_id>` shows for its metadata and use the
   closest display-only metadata herdr renders, as the spec allows; record
   the choice in `docs/dev/PLAN.md`.
7. **Inbox pane.** Run `sessionhub plugin open-inbox`. **j**/**k** move; **d**
   dismisses the cursor row; **s** then **1** snoozes it (check with
   `sessionhub inbox`). **Enter** on a `tower` session focuses its pane. **Enter** on
   a `bluebox` session: within 15 seconds a tab `sessionhub:bluebox` opens running
   `herdr --remote bluebox.example.com`, with that session focused there. **Enter**
   on it again focuses the same tab, with no second tab. With the Cloudflare
   Access login expired, the jump shows the resume command in the status line
   within 15 seconds.
8. **Terminal restore.** With the pane open, run `pkill -TERM -f 'sessionhub inbox
   --watch'` from another pane: the inbox pane's terminal comes back sane,
   and `stty -a | head -2` in a shell there shows `icanon` and `echo` without
   a `-`.
9. **Evidence.** Write `docs/dev/evidence/inbox-alerts.md` with the commands and
   outputs from steps 1 to 8 (no token, no chat ID), and commit it with the
   usual trailers, adding only that file.

Rollback, if a step fails badly: on `tower`, stop the server, restore the
newest `sessionhub.db.bak-*` over `sessionhub.db`, delete `sessionhub.db-wal` and `sessionhub.db-shm`,
install the previous binary, and start the server; on `bluebox`, install the
previous binary and run `sessionhub install-plugin`. The version 6 binary refuses a
version 7 database, so restoring the backup is the only rollback.
