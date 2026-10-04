# Plan (step 2)

Approved 2026-09-30. Targets herdr 0.9.3 (see the 0.9.3 section of `docs/dev/NOTES.md`).

This plan covers the API, the herdr events, and the plugin manifest, plus five
places where herdr 0.9.3 or Claude Code forces a change from `docs/dev/SPEC.md`. Facts
cited here are in `docs/dev/NOTES.md`. Nothing is built until you approve.

## Layout

One Go module, one binary `sessionhub`, standard library plus `modernc.org/sqlite`
(pure Go, so static builds need no cgo) and `github.com/BurntSushi/toml`.

```
cmd/sessionhub/            main: subcommand dispatch
internal/server/    HTTP API, auth, liveness, dashboard (embedded index.html)
internal/store/     SQLite schema and queries
internal/client/    shared by plugin, hooks, MCP, CLI: config, HTTP (2s timeout), retry queue
internal/herdr/     wrapper around the herdr socket (NDJSON) for snapshot, pane get, focus, split, agent start
internal/plugin/    startup, event, watcher, manifest generation
internal/hooks/     Claude Code hook stdin parsing, install/uninstall
internal/mcp/       stdio MCP server (JSON-RPC 2.0, two tools)
testdata/           captured payloads, scrubbed
```

The client talks to herdr over the Unix socket (`HERDR_SOCKET_PATH`), not by
spawning `herdr` subprocesses. The socket protocol is documented and
versioned (protocol 22 on herdr 0.9.3), and it avoids a fork per call inside event hooks. herdr answers one request per connection and then closes it, so the client opens a connection per call.

## HTTP API

All bodies are JSON. Writes need a machine token (`Authorization: Bearer`);
reads accept a machine token or the `__Host-hub_session` cookie of a signed-in
browser. The cookie also authorizes `POST /v1/sessions/{id}/remote-control`
with `X-Hub-Action: remote-control`, the inbox's dismiss and snooze with
`X-Hub-Action: triage`, and `POST /logout` with `X-Hub-Action: sign-out`,
and nothing else.

| Method and path | Who calls it | Purpose |
|---|---|---|
| `POST /v1/sessions` | plugin, hooks | Upsert one session by Claude UUID. Body: `id`, `agent` (`claude`), `cwd`, `git_repo`, `git_branch`, `herdr_session`, `herdr_workspace`, `herdr_pane`, `title_hint`, `source`. |
| `PUT /v1/machines/self/herdr-sessions` | plugin (startup, watcher) | Full set of this machine's herdr agent panes. Upserts each, refreshes `last_seen_at` (this is the heartbeat), and marks `ended` any session of this machine with a `herdr_pane` that is missing from the set. Sessions without a pane (hooks-only) are never touched by this call. |
| `POST /v1/sessions/{id}/events` | plugin, hooks | Body: `kind` (`state_changed`, `pane_closed`, `ended`, `prompt`), `source`, `ts`, `payload`. |
| `POST /v1/sessions/{id}/report` | MCP | Body: `done[]`, `in_flight[]`, `waiting_on[]`, `note`. |
| `POST /v1/sessions/{id}/title` | MCP | Body: `title`. |
| `GET /v1/sessions?live=true&machine=tower` | CLI, dashboard | List with latest report. |
| `GET /v1/sessions/{id}` | CLI, dashboard | Detail with report history and the last 50 events. `{id}` accepts a unique prefix. |
| `GET /v1/machines` | CLI, dashboard | Names, `ssh_host`, `herdr_host`, `last_seen`. |
| `GET /v1/inbox` | CLI (`sessionhub inbox`), dashboard | Sessions that need you, grouped `blocked`, `waiting`, `finished`, with counts. |
| `POST /v1/inbox/{id}/dismiss` | CLI, dashboard (cookie + `X-Hub-Action: triage`) | `{since}`: hide until a later trigger. `204`. |
| `POST /v1/inbox/{id}/snooze` | CLI, dashboard (cookie + `X-Hub-Action: triage`) | `{since, until}`: hide until `until` (at most 7 days) or a later trigger. `204`. |
| `POST /v1/sessions/{id}/remote-control` | dashboard (cookie + `X-Hub-Action`), CLI | Open a Remote Control request for the session's machine. `202` new, `200` the open one, `409` not in herdr or watcher offline, `429` at 10 pending. |
| `GET /v1/machines/self/control?wait=30` | plugin watcher | Long poll: claims the machine's oldest pending request (`200`), or `204` after `wait` seconds (1–30). At most 2 open per machine. |
| `POST /v1/control/{request-id}/result` | plugin watcher | `{"state":"done"\|"failed","url","detail"}` from the claiming machine. |
| `GET /healthz` | anyone | `200 ok` without auth. |
| `GET /` | phone | Dashboard. |
| `POST /v1/logins` | CLI (`sessionhub login`) | One-time sign-in link: `201 {url, name, expires_at}`; `409` name taken; `429` at 5 unused. |
| `GET /login/{code}` | browser | Confirm page; changes nothing. |
| `POST /login/{code}` | browser | Uses the code, sets `__Host-hub_session`, `303` to `/`. `Origin` must equal `public_url`. |
| `GET /v1/web-sessions` | CLI (`sessionhub login ls`) | Signed-in browsers; never a token or hash. |
| `DELETE /v1/web-sessions/{id}` | CLI (`sessionhub login rm`) | Sign a browser out. |
| `POST /logout` | dashboard | Ends this browser's session, clears the cookie, and returns `204`. Needs `X-Hub-Action: sign-out`. |

Changes from the spec's endpoint list: the `PUT .../herdr-sessions` call is new.
It gives one request per minute per machine for heartbeats instead of one per
session, and it is how `ended` is detected after a crash, when no `pane.closed`
event ever fires. Heartbeats update `last_seen_at` only and are not stored as
`events` rows (about 14,000 rows a day otherwise).

## Data model changes

- `sessions` gains `agent` (`claude` now, other herdr agents later) and
  `agent_state` (`idle|working|blocked|done|unknown`, from herdr or hooks).
  `status` stays the spec's `live|stale|ended|blocked`: `blocked` when
  `agent_state` is `blocked` and the session is live.
- `machines` gains `ssh_host` and `herdr_host`.
- `events` gains `source`, as already added to the spec.
- Title precedence: `set_title` from MCP, then herdr's
  `terminal_title_stripped` (Claude Code keeps it set to a session summary),
  then the first prompt.
- As built (Task 1; details in `docs/server.md`): `sessions` has no `status`
  column, because status is computed at read time and a stored copy would go
  out of date. `herdr_pane` is `''` rather than `NULL` when there is no pane.
  `first_prompt` is recorded once. A herdr snapshot entry that belongs to
  another machine is skipped and listed under `conflicts` instead of failing
  the whole `PUT`, so one bad entry cannot block a machine's heartbeat.
- Schema v3 (the Remote Control button): `control_requests` (`id`, `session_id`,
  `machine_id`, `action`, `state`, `requested_by`, `created_at`, `expires_at`,
  `claimed_at`, `finished_at`, `url`, `detail`), `machines.last_poll`, and the
  session's last link in `sessions.rc_url` and `rc_at`, cleared when the
  session ends. Expiry is computed on read and stored on the next write.
- As built for v3, beyond the Remote Control plan: a result's `url` is also
  capped at 200 bytes, a non-numeric `wait` is `400` rather than clamped, and
  `HEAD` on the poll route is `405`, because the mux routes `HEAD` to a `GET`
  pattern and a `HEAD` must not claim a request. The final review added these.

## herdr plugin

### Install

`sessionhub install-plugin` (run by `sessionhub join`) writes the manifest below into
`~/.local/share/sessionhub/herdr-plugin/` with the absolute path of the installed `sessionhub`
binary, then runs `herdr plugin link` on that directory. No GitHub checkout, no
`[[build]]`, and no Go toolchain on the client, which meets the spec's
"no runtime deps" rule. herdr runs commands without a shell and with the
server's `PATH`, which is why the path is absolute.

herdr 0.9.3 does not run `[[startup]]` on `plugin link`, only when its server
starts. So `sessionhub install-plugin` stops any running watcher (it may be an older
binary) and then runs the startup logic once itself; otherwise nothing is
registered until the next herdr restart. `sessionhub uninstall-plugin` unlinks,
removes the plugin directory, and stops the watcher; it keeps the state
directory, because the hooks share the queue. See `docs/plugin.md`.

### Manifest

```toml
id = "sessionhub"
name = "sessionhub"
version = "0.1.0"
min_herdr_version = "0.9.3"
description = "Register Claude Code sessions with the sessionhub at sessionhub.example.com."
platforms = ["linux"]

[[startup]]
command = ["/home/me/.local/bin/sessionhub", "plugin", "startup"]

[[events]]
on = "pane.agent_detected"
command = ["/home/me/.local/bin/sessionhub", "plugin", "event"]

[[events]]
on = "pane.agent_status_changed"
command = ["/home/me/.local/bin/sessionhub", "plugin", "event"]

[[events]]
on = "pane.closed"
command = ["/home/me/.local/bin/sessionhub", "plugin", "event"]

[[events]]
on = "pane.exited"
command = ["/home/me/.local/bin/sessionhub", "plugin", "event"]

[[actions]]
id = "status"
title = "sessionhub: sessions on this machine"
contexts = ["global"]
command = ["/home/me/.local/bin/sessionhub", "status"]

[[actions]]
id = "resume"
title = "sessionhub: resume a session"
contexts = ["global"]
command = ["/home/me/.local/bin/sessionhub", "plugin", "open-picker"]

[[panes]]
id = "resume-picker"
title = "sessionhub: resume"
placement = "overlay"
command = ["/home/me/.local/bin/sessionhub", "resume", "--pick"]
```

### Events used, and why

| Event | Handling |
|---|---|
| `pane.agent_detected` | Queue an upsert for the pane. The watcher fills in the session from its snapshot cache: the event handler makes no herdr call (see below). |
| `pane.agent_status_changed` | Queue a `state_changed` event with `agent_status`. Coalesced by the watcher. |
| `pane.closed`, `pane.exited` | Queue `pane_closed`; server marks the session `ended`. |
| `[[startup]]` | Full `session.snapshot` → `PUT /v1/machines/self/herdr-sessions`, then start the watcher. |

Not used: `pane.created` (a new pane has no agent session yet; `agent_detected`
covers it), and `pane.updated` (not hookable in 0.9.3; the watcher catches
session ID changes after `/clear` instead).

### Event handler: never block, debounce, fail open

`sessionhub plugin event` reads `HERDR_PLUGIN_EVENT_JSON`, appends one line to
`~/.local/state/sessionhub/queue.jsonl` (with `flock`), makes sure the watcher is
running, and exits. It makes no network call, so it finishes in milliseconds
and holds none of herdr's 32 command slots. The session UUID is resolved by the
watcher: `pane.agent_status_changed` does not carry `agent_session`.

### Watcher (the long-running part)

herdr 0.9.3 has no supervised daemons and startup hooks are one-shot, so the
watcher follows the `herdr-plugin-agent-usage` pattern:

- `sessionhub plugin startup` and every `sessionhub plugin event` call `ensureWatcher`: take a
  non-blocking `flock` on `~/.local/state/sessionhub/watcher.lock`; if it is free,
  start `sessionhub plugin watch` in a new session (`setsid`, stdin `/dev/null`) and
  return.
- The watcher loop, every 2 seconds: drain the queue, keep only the latest
  state per pane, resolve pane → session from its cache, and send each item
  through `client.Replay`. Sending holds the queue lock, so the send phase of
  a pass has a 1 s deadline; items it does not reach stay queued. That keeps a
  concurrent hook's append under 1.5 s. On a retryable failure, or a 401/403
  (wrong or revoked token), the pass stops and the item and every later one
  stay queued. Other errors drop the item with a log line. A 404 on an event
  upserts the session from the pane and retries once.
- Every 60 seconds: `session.snapshot` over the socket →
  `PUT /v1/machines/self/herdr-sessions`. This is the heartbeat, catches
  session ID changes after `/clear`, and catches panes that vanished without an
  event.
- It exits when the herdr socket's device and inode change or the socket
  disappears. The next herdr server runs `[[startup]]` and starts a fresh
  watcher.
- Beside the 2 s ticks, a control loop holds `GET /v1/machines/self/control`
  open and runs each claimed request through `resume.Control` (see
  `docs/plugin.md`, "Control loop").
- As built, beyond the Remote Control plan (final review): the loop posts a
  result on a context detached from the watcher's, bounded to 2 s, so a
  request that finishes while the watcher stops is still recorded, and
  `sessionhub install-plugin` waits up to 7 s (was 3 s) for the old watcher to exit.
  The loop also remembers, for 2 minutes, the pane it resumed a session in.
  herdr reports the session there only seconds after Claude starts, so
  without that a second tap could start a second copy; now it injects into
  that pane or reports "just resumed; try again in a moment".

## Claude Code hooks client

`sessionhub install-hooks` adds to `~/.claude/settings.json`, next to herdr's entry:

| Event | Matcher | Command | Options |
|---|---|---|---|
| `SessionStart` | `*` | `sessionhub hook session-start` | `async: true` |
| `UserPromptSubmit` | — | `sessionhub hook prompt` | `async: true` |
| `Stop` | — | `sessionhub hook stop` | `async: true` |
| `Notification` | `*` | `sessionhub hook notification` | `async: true` |
| `SessionEnd` | `*` | `sessionhub hook session-end` | `timeout: 2` |

`SessionEnd` is synchronous because async hooks can be killed at teardown, and
Claude Code gives all `SessionEnd` hooks a 1.5 s budget, so it only appends to
the queue and exits. Every hook skips subagent calls (`agent_id` present), exits
0 on every error, and includes `HERDR_PANE_ID`, `HERDR_WORKSPACE_ID`, and the
herdr session name when they are in the environment. Hook entries are
identified by the `sessionhub hook` command prefix, so `sessionhub uninstall-hooks` removes
exactly those.

`install-hooks` and `uninstall-hooks` take `--settings FILE` and `--binary PATH`
(for project-level installs and tests). When an event gets a 404, the hook
upserts the session and retries once. Details are in `docs/hooks.md`.

The hooks client also keeps the watcher's queue flushed: if no watcher is
running (no herdr), each hook sends its queued lines directly, with the 2 s
timeout, before exiting.

## MCP server

`sessionhub install-mcp` runs `claude mcp add --scope user sessionhub -- /home/me/.local/bin/sessionhub mcp`.
It serves `report_progress(done, in_flight, waiting_on, note?)` and
`set_title(title)` over stdio.

**Session ID.** `CLAUDE_CODE_SESSION_ID` goes stale in the MCP process after
`/clear` or an in-session `/resume`. The MCP server resolves the current ID in
this order:

1. Inside herdr: `pane.get` on `HERDR_PANE_ID` and read `agent_session.value`
   (herdr's integration updates it on every `SessionStart`, including `/clear`).
2. Outside herdr: `~/.local/state/sessionhub/current/<claude-pid>`, written by the
   `SessionStart` hook. Both the hook and the MCP server find the Claude PID
   the same way: the MCP server's parent, and the hook's nearest ancestor whose
   executable is the Claude binary.
3. Fallback: `CLAUDE_CODE_SESSION_ID`.

**herdr sidebar.** `pane report-metadata` has no `--custom-status`. The MCP
server sends `pane.report_metadata` with `source: "sessionhub"` and
`tokens: {"hub_summary": "<short summary>"}`. herdr documents tokens as
display-only values shown with `$name` in a sidebar row. To see it, add a row
to `[ui.sidebar.agents]` in `~/.config/herdr/config.toml`; the README will give
the snippet, and sessionhub will not edit your herdr config.

**Delivery.** A `404` from the report or title endpoint makes the MCP server
upsert a minimal session and retry once. Failed calls go to the offline queue
as items with `op` `report` or `title` (`session_id`, `body` = request body),
so the queue drainer must replay those two ops. See `docs/mcp.md`.

The `CLAUDE.md` snippet ships as `docs/CLAUDE-snippet.md`, and `sessionhub join`
prints where to paste it.

## Resume

herdr 0.9.3 plugin actions take no arguments (`herdr plugin action invoke
<ACTION_ID> [--plugin <ID>]`), so `sessionhub.resume` cannot receive a session ID.
Instead:

- `sessionhub resume <id-prefix>` on the CLI does the herdr work itself over the local
  socket. It doesn't go through the plugin action.
- The `sessionhub.resume` action opens the `resume-picker` overlay pane, which runs
  `sessionhub resume --pick` with a real terminal: a numbered list of this machine's
  sessions, then the same logic.

Local session:

1. If `herdr_pane` still exists and its `agent_session.value` matches, focus it
   with `pane.focus` (params `{"pane_id": ...}`).
2. Otherwise, split a pane with `cwd` set to the recorded cwd (the calling
   pane from `HERDR_PANE_ID` when set, else the snapshot's `focused_pane_id`), then `agent.start` with `kind: "claude"` and
   `args: ["--resume", "<uuid>"]`. This is herdr's native agent start, so herdr
   detects the agent and its session. (`agent.start` requires the pane to be at
   an interactive shell prompt; a new split is.)

Remote session: print, never run:

```
ssh -t bluebox.example.com '~/.local/bin/sessionhub resume <uuid>'     # opens or focuses it in bluebox's herdr
herdr --remote bluebox.example.com               # attach to bluebox's herdr from here
```

If the target machine is saved in the local herdr (`herdr machine list --json`
has a profile whose SSH target matches `herdr_host`), also print the
`herdr --machine <label> pane focus <pane_id>` form, since herdr 0.9.3 routes
it over SSH without an open TUI.

With no herdr info: `ssh -t bluebox.example.com 'cd <cwd> && claude --resume <uuid>'`.

### Remote Control

`sessionhub remote-control <id-prefix>` sends `/remote-control` with `agent.prompt` to
the session's pane (only when herdr detects Claude there with a matching
`agent_session`), reads the pane with `pane.read` before and after, and prints
the `claude.ai/code/session_...` URL that appeared. `agent_blocked` exits 1
with no retry; a session that isn't running says to use
`sessionhub resume --remote-control`, which adds `--remote-control` to
`claude --resume`. See `docs/cli.md`.

The dashboard button and the remote branch of `sessionhub remote-control` go through
control requests: the sessionhub stores them, the session's watcher claims and runs
them with `resume.Control`, and the dashboard or CLI reads the result from the
session. The watcher resumes a session no pane runs in a new workspace;
`sessionhub remote-control` on the session's own machine still never does. When the
sessionhub refuses the request or the machine doesn't answer in time, the CLI prints
the `ssh` command as before.

As built, beyond the Remote Control plan:

- The injection steps that moved into `resume.Control` are the fix round's:
  the already-on dialog is judged on the visible screen only, and **Esc**
  closes it only when Claude isn't working.
- The dashboard keeps a finished request's outcome on the card for 10 minutes
  after it ends, also across reloads, starts a poller after a reload when a
  request is still open, and hides the link once the session has none (it
  ended). The plan kept this state in the page only, so a reload lost it.
- `sessionhub remote-control --help` prints the usage instead of looking up a
  session called `--help`. The CLI prints a link from the sessionhub only when the
  whole value matches the Remote Control link pattern.

## Enrollment and deploy

As in `docs/dev/SPEC.md` component 6. Machine rows for this setup:

| Name | `ssh_host` | `herdr_host` |
|---|---|---|
| `tower` | `tower.example.com` | `tower.example.com` |
| `bluebox` | `bluebox.example.com` | `bluebox.example.com` |

On `tower` itself, `sessionhub join` sees the server's database locally and calls
`sessionhub machine add` without SSH.

As built for schema v3: before it replaces the binary, `make deploy` backs up
the database on `tower` with `sqlite3 .backup` and keeps the three newest
backups, because v3 is the first upgrade that changes a live database with
data worth keeping. The README lists both ways to roll back.

Manual steps for you, listed in the README:

1. Cloudflare dashboard: add public hostname `sessionhub.example.com` → `http://10.200.0.1:8787`
   on the `cloudflared-tunnel` tunnel.
2. If ufw blocks the container from reaching the host port:
   `sudo ufw allow in on docker0 to 10.200.0.1 port 8787 proto tcp`. I'll test
   whether it's needed before asking you to run it.

## Session insights, as built

The design is in `docs/dev/superpowers/specs/2026-09-30-session-insights-design.md`.
The build differs from it in these ways:

- The summary line prints `!391` or `#7`, not `MR !391`.
- The list's git counts sit under `summary.git`, not flat in `summary`.
- Git gets a 3-second budget (`digest.GitTimeout`), not the 500 ms that
  `gitinfo` uses, because `git log` over a window can take longer than a
  branch lookup.
- The CLI flag is `sessionhub ls --grep`, not `sessionhub list --grep`.
- `Builder.Build` clamps a digest to the server's limits and keeps the JSON
  body to 15 KiB, below the server's 16 KiB cap, so one corrupt entry can't
  make the server reject every later digest.
- A digest without `git` keeps the git data the server already stores, because
  an omitted `git` means unknown.
- A session's first digest doesn't replace a title the user already set with an
  older `/rename` from the transcript.
- The watcher digests sessions that have never failed first, and gives each
  digest a 15-second timeout, so one slow session can't starve the others.
- `sessionhub digest <prefix>` resolves the 8-character ID that `sessionhub ls` prints
  through the server.

## Web sign-in, as built

The design is in `docs/dev/superpowers/specs/2026-10-01-web-sign-in-design.md`.
The build settles these points the spec leaves open:

- The login pages send `Referrer-Policy: same-origin`, not the dashboard's
  `no-referrer`: with `no-referrer`, browsers send `Origin: null` on the form
  post and the `Origin` check refuses every sign-in.
- The request log prints `/login/<code>` instead of the code, like it drops
  query strings, so a live link never sits in the journal.
- The 5-link limit is global, not per machine.
- A press whose name a newer session took rolls back: the page answers `409`
  and the link keeps working until it expires.
- A machine token on `POST /logout` gets `403`; only a browser ends its own
  session. Any machine token may list and revoke any browser session.
- `GET /` still accepts a machine bearer token.
- The request log labels a browser caller `web:<name>`.
- Login page failures (`403`, `409`, `410`) are HTML pages.
- `sessionhub login rm` matches a name before an ID.
- The QR code draws light modules filled and dark modules as spaces, so the
  quiet zone shows on a dark terminal.
- `sessionhub login ls` shows `LAST_USED` as an age, like `sessionhub ls`.

## Inbox, as built

The design is in `docs/dev/superpowers/specs/2026-10-01-inbox-design.md`.
The build settles these points the spec leaves open:

- Schema version 6 adds `sessions.turn_ended_at`, `sessions.blocked_at`, and
  `inbox_triage`. The `since` of a blocked item is `blocked_at`, then
  `state_ts`, then `started_at`. Upserts do not stamp `state_ts`, which keeps
  the clock-skew allowance for hooks events.
- `turn_ended_at` is not backfilled: existing idle sessions enter the inbox
  only after their next turn.
- Rollback is restoring the database backup. The previous binary is not
  documented as running against a version 6 database.
- Items with the same group and `since` sort by session ID.
- A dismiss ignores `until`; a snooze without `until` is `400`.
- The CLI caps snooze durations at 7 days minus one minute (167h59m) to
  tolerate clock skew with the server's 7-day limit.
- `tomorrow` is 9:00 local time on the next calendar day, also after
  midnight, in both the CLI and the dashboard.
- Triage on a session that exists but is not in the inbox is stored; only an
  unknown session is `404`.
- The triage routes take a full session ID. `sessionhub inbox dismiss|snooze`
  resolve an ID or a prefix of at least 4 characters against the current
  inbox, and an exact ID wins.
- The dashboard filter applies to the Inbox tab too; the tab count and the
  page title ignore it.
- The dashboard picks the Inbox tab only on the first inbox read.
- The dashboard sends `since` back as the server's string, because a `Date`
  round trip loses the nanoseconds and the item would never hide.
- `sessionhub inbox` cuts lines to the terminal width (`TIOCGWINSZ`), else
  `COLUMNS`, else 100, and prints `nothing needs you` when empty.
- A turn ends only on a direct move from `working` or `blocked` to `idle` or
  `done`; a detour through `unknown` does not count.
- **Open** on an inbox row is the card's Remote Control row, with the card's
  labels (**Remote Control**, **Resume with Remote Control**, **Open in
  Claude**), not one button named **Open**.
- The auth matrix snapshot reads `inbox_triage` directly, since the store has
  no method that lists it.

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
- Since 2026-10-02, the notifier also alerts for Waiting items, with the same
  rules: 30 seconds from `since`, once per session per `since`, not when
  triaged, the attempt cap, and the first-tick stale skip. A Waiting message
  starts with `💬 Waiting on you:`, says `waiting <age> ago`, and its third
  line is the item's `waiting_on`. Finished items never alert.
- A Blocked item carries no `waiting_on`. The Telegram message's third line
  is the latest report's `waiting_on` when no prompt came after it, else the
  recap, and it is left out when both are empty.
- The **Remote Control** button needs an `https` link; Telegram rejects
  others.
- The notifier ends a tick on a network error or HTTP 429. Any other refused
  message (for example HTTP 400) is logged and the tick goes on with the next
  item, and the refused item is retried on the next tick. On its first tick
  after start, it records Blocked and Waiting items older than 1 hour as
  alerted without sending. HTTP 429 without `retry_after` waits 30 seconds. If recording an alert fails after a
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
- Auto-dismiss of **Finished** fires only for `done` to `idle` reported by
  herdr (the plugin source). Claude's Stop-hook `idle` never dismisses, and a
  hooks `idle` never replaces a stored `done` (the event is stored, the state
  stays), so a late Stop hook cannot hide herdr's seen. The
  dismiss never moves an existing triage row backwards, and it turns a snooze
  of the same **Finished** item into a dismiss.
- A Telegram setting never stops the server. `telegram_chat_id` and
  `telegram_bot_token` may be a TOML string or an integer; any other type
  turns alerts off and logs why, and an unknown `telegram_*` key is a logged
  warning (other unknown keys stay errors). The env overrides still win. An
  unquoted token is a TOML syntax error, so the docs say to quote it. The token is a
  `Secret` type that prints and JSON-encodes as `[redacted]`. Telegram error
  text is redacted before it is truncated, and the notifier logs nothing
  during shutdown.
- In `sessionhub inbox --watch`, **q** and **Ctrl+C** cancel any in-flight request
  and quit at once. A pending snooze is cancelled if the selected row
  changes. A signal exits with status 130 and no message. Plain `sessionhub inbox` headings are cut to the
  terminal width, with the same renderer as the watch view.
- The jump validates the `ssh` and herdr hosts on the client with the
  server's host rule before it uses them, sets a 2-second `WaitDelay` on
  child processes, and runs `ssh` with stdin from `/dev/null`. A remote
  `sessionhub resume` that exits non-zero shows as `sessionhub resume on <machine> failed`;
  only exit status 255 and non-exit errors show as `ssh failed`.
- herdr `notification.show` answers `disabled` and `no_foreground_client`
  are not errors and are not logged.

## Session actions, as built

The design is in `docs/dev/superpowers/specs/2026-10-02-session-actions-design.md`.
The build settles these points the spec leaves open:

- Schema version 8 adds `instructions`, `messages`, and
  `permission_requests`. Rollback is restoring the database backup and
  reinstalling the previous binary (version 8 to 7); the version 7 binary
  refuses a version 8 database. Run `sessionhub uninstall-hooks` with the new binary
  first, so the new hook entries don't call commands the old binary doesn't
  know, then reinstall the hooks with the old binary.
- `sessionhub hook session-start` and `sessionhub hook prompt` stay async, and Claude Code
  does not add an async hook's output to the prompt. The rules come from a
  synchronous `sessionhub hook context` entry in the same matcher group, which
  reads `<state>/instructions.json` only. It takes the event from stdin's
  `hook_event_name` and prints nothing for a subagent. Any file error means
  no rules, silently.
- The watcher refreshes the rules copy after each accepted heartbeat, and
  `session-start` starts a detached `sessionhub hook refresh-instructions`. Both
  rewrite the copy only when `version` changed.
- Messages ride the control long poll as claims whose action is `message`,
  with the prompt in `text`. Results use `POST /v1/control/{id}/result` with
  `delivered`, `refused`, or `busy`; `failed` from an older watcher counts as
  `refused`. A queued message is offered again 15 seconds after the last
  offer, so a watcher that dies after a claim can, rarely, deliver twice.
- The watcher delivers only when herdr reports the message's own session ID
  in the pane and the agent is `idle` or `done`. No ID yet, an unknown
  status, `working`, `blocked`, herdr's `agent_blocked`, a herdr error other
  than `pane_not_found`, or a silent socket is `busy` (retried). A pane that
  runs another session, or `pane_not_found`, is `refused`.
- A machine token's `from_session` must be a session that machine owns: `404`
  for an unknown session, `409` for another machine's. The message rate limit
  is per authenticated caller (`machine:<name>` or `web:<session name>`),
  stored as `messages.limit_key`: 30 a minute, whatever `from_session` the
  caller names. It counts the caller's stored messages of the last 60
  seconds plus the current send's targets (one per target); refused targets
  of earlier sends are not stored, so they do not count. The stored sender
  is `dashboard`, `cli on <machine>`, or `session <id8>`. The prompt starts
  `From the user via sessionhub (dashboard):` or `(cli on <machine>):` for a
  person, and `From session <id8> via sessionhub (sent on the user's behalf):` for
  a session.
- `GET /v1/messages/{id}` lets `sessionhub send` wait up to 15 seconds for results.
- Permission requests are open for 10 minutes. The hook caps `tool_input`
  with `CapToolInput` before sending, so a big input never hits the 64 KiB
  body limit: over 8 KiB after JSON escaping, it is stored as a JSON string
  of the first 8,000 bytes. The hook drops suggestions over 4 KiB. It stops
  on the first server error, as the spec says.
- Any machine token may decide any machine's permission request, so
  `sessionhub approve` works from either machine.
- The permission hook decides only on an explicit `allow` or `deny` for its
  own request ID. Every other path (expiry, a server error, no server, an
  answer in the terminal, a closed request) prints nothing and falls back to
  the terminal dialog. Checked live on 2026-10-02: the dialog shows while the
  hook waits, and an answer in the terminal wins.
- "Leaves blocked" closes open requests created at or before the state
  change: the event's own time on the event path, the server time minus
  5 seconds on the snapshot path. An ended session closes them as
  `closed`. A hooks-only session leaves `blocked` only at its next stop or
  prompt.
- The inbox shows the newest open request on a **Blocked** item only. The
  Telegram alert adds `Asks to use <tool>: <main field>` as its third line.
- The dashboard's **Allow once** has an inline confirm that repeats the
  command. Control and bidi characters in a command show as U+FFFD. The
  sanitizer uses `\u` escapes only, so the page holds no raw bidi characters;
  a test enforces that.
- The decision long poll rereads its request every second (for answers in
  the terminal and expiry) and wakes at once on a decision.
- Adding a rule sends a Telegram message when alerts are on ("New rule for
  all sessions", the rule, who added it, and an **Open rules** button). A
  session can plant a rule through the MCP `remember` tool after it reads
  malicious content, and every session then follows that rule, so the alert
  makes the plant visible at once. The handler queues the alert on a
  16-slot channel that the notifier's goroutine drains, so the request never
  waits for Telegram. A full queue drops the alert with one log line. There is
  one attempt and no retry queue, and the alert honours an HTTP 429 wait.
- Messages keep their newlines. herdr submits a multi-line `agent.prompt` as
  one prompt (checked live on 2026-10-02 with a 16-line message), so the
  watcher sends the text as it is.

## Moves, as built

The design is in `docs/dev/superpowers/specs/2026-10-03-move-session-design.md`,
and the plan in `docs/dev/superpowers/plans/2026-10-03-move-session.md`. The
build settles these points the spec leaves open:

- Schema version 9 adds `moves` and `machines.move_key`. The column is
  `requested_by` (`BY` is an SQL keyword); `target_machine_id` and
  `source_closed_at` drive the claims. Rollback is restoring the database
  backup.
- The source learns how a move ended from one more `move-out` claim, the
  finish, offered once through the long poll and recorded in
  `source_closed_at`, so a watcher restart mid-move still archives or
  restores. A result the source posts itself closes its part.
- `requested` fails after 2 minutes; `packing`, `uploaded`, and `unpacking`
  after 10 minutes without a change. `cancelled` is defined and unused.
- Ownership moves when the target reports `done`, after herdr detects
  Claude. The target's first hook writes for the session get `409` until
  then; its next heartbeat registers the pane.
- The target runs every check that changes nothing (bundle, version,
  existing transcript, clone search, clean clone, including the files the
  patch creates) before it touches the clone. Its undo takes back only what
  the move did: the files it wrote (one changed since stays), the patch
  (`git apply -R`), and the branch (back to its old commit, or deleted when
  sessionhub created it, each with a compare-and-swap `update-ref`). It never runs
  `git reset` or `git clean`, and it never writes through a linked folder.
- A session never runs in two places. The source runs every check that can
  fail before `/exit` and checks the pane again right before it. After
  `/exit`, it restarts the session only once the sessionhub recorded the failure;
  an upload with an unknown outcome restarts nothing, and the finish
  restarts it after the move times out. The target starts Claude only while
  the move is `unpacking`, and ends it again when the sessionhub does not record
  its `done`. When the target can't end Claude (after a failed start or an
  unrecorded `done`), it undoes nothing: after a failed start it posts
  `done` with a note that names the pane, and otherwise it keeps the
  clone as it is and retries the `done` until the move times out. The live
  session decides who owns the session.
- A pane that shows Claude without a session ID counts as running, so a
  restart waits instead of starting a second copy.
- A cloud move that may have started a cloud session never restarts the
  local session. Before it posts anything, the source writes a marker
  under `<state>/moved/<move id>/`, retries the `done`, and, when the move
  times out, the finish adds a note and restarts nothing.
- A restart or an archive that fails after the source's part closed is
  added to the ended move's detail as a note.
- The bundle build never writes `.git/index`: it diffs against a temporary
  copy of the index, and it checks the sizes of all files before it reads
  any of them.
- Secret-looking files never travel, tracked or untracked: the patch and
  the cloud commit leave them out with `:(exclude,icase,glob)` pathspecs.
  Every `git diff` ignores the user's diff config (`--no-color
  --no-ext-diff --no-textconv --no-renames --full-index` and fixed
  prefixes).
- "Same remote" compares `origin` with the scheme, user, `.git`, and host
  case dropped. With several clones, the repository's path under its search
  root decides, ignoring case.
- A detached HEAD on the source fails. The source pushes only to `origin`,
  the remote the target clones from, and only a branch with no upstream or
  one ahead of it, never forced. An upstream on another remote stays as it
  is. `sessionhub/cloud-<id8>`, which sessionhub
  owns, is replaced only with `--force-with-lease` against the commit sessionhub
  pushed there; when a cloud session pushed to it, the move fails instead.
- A project folder name over 200 characters uses the one existing folder
  with that prefix, else the move fails and says to start Claude there once.
- `claude --cloud`'s first `https://claude.ai/code/session_...` line is the
  link; sessionhub stops the command then and gives up after 2 minutes. The live
  check on 2026-10-03 (Claude Code 2.1.288) showed that `claude --cloud`
  refuses without a terminal and prints `View: <link>?from=cli&m=0`, so sessionhub
  runs it under util-linux `script` and drops the query.
- `sessionhub move` follows a move for 3 minutes. Ctrl+C stops following, leaves
  the move running, prints the `sessionhub move --status` command, and exits 130.
- The watcher re-registers its key every hour, so a database restored from a
  backup learns it back.

## Tests and captured payloads

- Server: `httptest` against a temp SQLite file: auth, upsert, reconcile,
  liveness transitions with a fake clock, prefix lookup.
- Plugin: event handler and watcher coalescing fed with captured
  `HERDR_PLUGIN_EVENT_JSON` payloads and the captured snapshot; herdr socket
  faked with a Unix-socket server that replays captured responses.
- Hooks: captured stdin for the five events.
- MCP: JSON-RPC `initialize`, `tools/list`, `tools/call` over pipes.

Capture needs two temporary changes to your setup, each removed right after:
`herdr plugin link` of a probe plugin that writes its environment to a file,
and a probe hook in a scratch project's `.claude/settings.local.json`. I'll ask
before each one.

## What goes in `docs/dev/IDEAS.md`, not v1

- Scheduled database backups for `tower`. As built, `make deploy` backs up the
  database before it replaces the binary; nothing backs it up between deploys.
- A background heartbeat for hooks-only sessions (they go `stale` while idle).
- Upgrading remote resume to herdr 0.9.3's `herdr machine` profiles. Remote resume already prints the `--machine` form; saving profiles is the part left out.
- Other herdr agents besides Claude Code.
