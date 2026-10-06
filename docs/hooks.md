# Claude Code hooks client

`sessionhub hook <event>` reports a Claude Code session to the sessionhub server. Claude
Code runs it with the hook JSON on stdin. It always exits 0, so it never fails
or blocks a Claude Code turn.

## Commands

| Command | What it does |
|---|---|
| `sessionhub install-hooks` | Adds sessionhub's hook entries for six events to `settings.json`. Prints a reminder to run `sessionhub install-mod` while the mod is not installed. |
| `sessionhub uninstall-hooks` | Removes only sessionhub's entries. |
| `sessionhub hook session-start\|prompt\|stop\|notification\|session-end\|context\|permission-request` | Handles one hook call. Claude Code runs this. |
| `sessionhub hook flush` | Internal. Drains the queue. `session-end` starts it. |
| `sessionhub hook refresh-instructions` | Internal. Rewrites the local rules copy. `session-start` starts it. |

Both install commands accept `--settings FILE` (default
`$CLAUDE_CONFIG_DIR/settings.json`, else `~/.claude/settings.json`) and
`--binary PATH` (default `~/.local/bin/sessionhub`, must be absolute). Use
`--settings .claude/settings.local.json` to install for one project.

## Installed entries

| Event | Matcher | Command | Options |
|---|---|---|---|
| `SessionStart` | `*` | `<sessionhub> hook session-start`; `<sessionhub> hook context` | `async: true`; `timeout: 5` |
| `UserPromptSubmit` | none | `<sessionhub> hook prompt`; `<sessionhub> hook context` | `async: true`; `timeout: 5` |
| `Stop` | none | `<sessionhub> hook stop` | `async: true` |
| `Notification` | `*` | `<sessionhub> hook notification` | `async: true` |
| `SessionEnd` | `*` | `<sessionhub> hook session-end` | `timeout: 2` |
| `PermissionRequest` | `*` | `<sessionhub> hook permission-request` | `timeout: 660` |

`SessionEnd` is synchronous because async hooks can be killed at teardown. It
does no network I/O (see below). The `context` entry is synchronous because
Claude Code adds only a synchronous hook's output to the prompt; it shares
the group with the async entry, so each event has one sessionhub group.
`PermissionRequest` is synchronous because its output is the decision.

Install and uninstall edit the file in place and keep every byte outside the
sessionhub entries: they locate the `hooks` value, change only that, and copy other
events and other matcher groups verbatim (herdr's `SessionStart` entry
included). Details:

- The first change makes `settings.json.sessionhub-backup-<YYYYMMDDTHHMMSS>` next to
  the file. A run that changes nothing writes no backup. A same-second backup
  gets a `-1`, `-2` suffix.
- Install is idempotent. A sessionhub entry that already matches stays where it is; a
  stale one (for example an old timeout) is replaced.
- Uninstall removes an entry only when its command starts with the sessionhub path
  plus ` hook `. A group that mixes sessionhub and other hooks keeps the others. It
  can leave an empty `"hooks": {}`.
- A path with spaces or shell characters is single-quoted in the command.
- A symlinked `settings.json` stays a symlink; the target is updated.
- An unparsable file is left untouched and the command fails.
- Rewritten sessionhub groups are compact single-line JSON.

## Hook behavior

Each call except `permission-request` skips itself when the stdin JSON has
`agent_id` (a subagent). `permission-request` handles a subagent's prompt,
because it shows in the same terminal. Bad JSON or a missing `session_id`
logs one line on stderr and does nothing.

| Event | Sends |
|---|---|
| `session-start` | `POST /v1/sessions`: `id`, `agent: claude`, `source: hooks`, `cwd`, git repo and branch, `herdr_pane` (`HERDR_PANE_ID`), `herdr_workspace` (`HERDR_WORKSPACE_ID`), `herdr_session` (from `HERDR_SOCKET_PATH`), `title_hint` (`session_title` if present). Also writes `<state>/current/<claude-pid>` with the session ID. |
| `prompt` | `POST /v1/sessions` with `first_prompt` (first 200 characters) and `agent_state: working`; a `prompt` event with payload `{"prompt": "<first 200 characters>"}`; a `state_changed` event with `{"agent_state": "working"}`. |
| `stop` | `state_changed` with `{"agent_state": "idle"}`. Also starts a detached `sessionhub digest <id>`; the hook never waits for it. |
| `notification` | `state_changed` with `{"agent_state": "blocked"}` when `notification_type` is `permission_prompt` or `elicitation_dialog`. Any other type (`idle_prompt`, `auth_success`, a missing type) sends nothing. The filter is in code; the settings matcher stays `*`. |
| `session-end` | Appends an `ended` event (payload `{"reason": ...}`) to the queue, removes `current/<claude-pid>` if it still names this session, starts a detached `sessionhub hook flush` and a detached `sessionhub digest <id>` (the hook never waits for either), and exits. This process makes no network call. |

Every event has `source: hooks` and a UTC RFC 3339 nanosecond timestamp.

`<claude-pid>` is the nearest ancestor whose executable (`/proc/<pid>/exe`
on Linux, `ps -o comm=` on macOS) has a basename that starts
with `claude` or whose path contains `/share/claude/versions/`; if none
matches, the parent PID. The MCP server uses this file to find the current
session after `/clear`.

### Standing instructions

`sessionhub hook context` prints the rules from the local copy,
`<state>/instructions.json`, as `additionalContext`:

    {"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"Standing instructions from sessionhub (apply in every session):\n- <rule>\n- <rule>"}}

It reads only that file and never calls the server, so a prompt never waits
on the network. With a missing, empty, or unreadable copy, an empty list,
another event, or a subagent call, it prints nothing and writes nothing to
stderr. The herdr watcher refreshes the copy on every heartbeat, and
`session-start` starts a detached `sessionhub hook refresh-instructions`, which
rewrites it when the server's `version` changed. A rule added on the server
reaches a session at its next prompt after the next refresh.

### Permission requests

`sessionhub hook permission-request` posts the prompt's `tool_name`, `tool_input`
(cut to 8 KiB with `api.CapToolInput`), `cwd`, and `permission_suggestions`
(dropped over 4 KiB) to `POST /v1/sessions/{id}/permissions`, then holds
`GET /v1/permissions/{id}/decision?wait=30` open in a loop for up to 10
minutes, with at least a second between two polls. It runs outside the 5 s
deadline of the other hooks. It handles subagent calls too, because their
prompt shows in the same terminal. Claude Code shows its own dialog
meanwhile; an answer there wins, and the server then marks the request
answered locally.

When several prompts wait at once (parallel subagents), answering one in
the terminal takes the session out of blocked, and the server marks every
open request from before that moment answered locally, the others included.
Those prompts are still on screen, but you can answer them only in the
terminal: sessionhub returns `410` for them, and their hooks print nothing. This
fails safe: sessionhub never answers a prompt it no longer tracks.

| Server answer | Hook output |
|---|---|
| decided, allow | `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` |
| decided, deny | the same with `"behavior":"deny","message":"<reason, or Denied from sessionhub.>"` |
| expired, answered locally, closed | nothing |
| an error, `404`, `429`, or no server | nothing; one line on stderr |
| 10 minutes without an answer | nothing |
| an answer for another request ID, or an unknown decision | nothing |

Nothing means the dialog in the terminal stays. sessionhub never sends
`updatedPermissions`. To opt a machine out, set `SESSIONHUB_REMOTE_PERMISSIONS=off`
in Claude Code's environment, or add `remote_permissions = false` to
`~/.config/sessionhub/config.toml`; the hook then exits at once without a request.

## Delivery

1. The hook sends its items in order, each with the client's 2 s timeout.
   The whole invocation (live sends and drain) has a 5 s deadline.
2. At the first failure it appends that item and the rest to the shared queue
   (`<state>/queue.jsonl`) and does not drain. A 401 or 403 counts as a
   failure here: the items are kept.
3. After sending everything, if no plugin watcher holds `<state>/watcher.lock`
   (checked with a non-blocking `flock` that is released at once), it drains
   the queue: at most 50 items and at most 1 s for the whole pass (one
   deadline, not per request), so the queue lock stays short and a concurrent
   `session-end` append never waits past its 1.5 s budget. Items not reached
   stay queued.
4. When the server answers 404 to an `event`, `report`, or `title`, the
   session is unknown (for example, hooks were installed mid-session). The
   hook upserts the session and retries the item once. For the current hook's session the upsert
   carries cwd, git info, and herdr fields. For a queued item of another
   session it carries only `id`, `agent`, and `source`.
5. Every send goes through `client.Replay`, so the drain also replays the
   `report` and `title` items the MCP server queues. While draining:
   - A retryable error (network, timeout, 408, 429, 5xx) stops the pass and
     keeps that item and every later one.
   - An auth error (401, 403; `client.IsAuthError`) does the same, because
     fixing the token makes the items succeed.
   - Any other error (other 4xx, bad body, unknown op) drops the item with a
     log line on stderr.
   - An item with no `session_id` but a `pane_id` is a plugin item that only
     the watcher can resolve. The hooks drain keeps it, in order, and neither
     sends nor drops it.
6. `session-end` cannot send (Claude Code allows hooks 1.5 s at teardown).
   After queuing `ended` it starts `sessionhub hook flush` as a detached child
   (`setsid`, stdin, stdout, and stderr on `/dev/null`, not waited for). The
   child drains under the same rules, and skips when a watcher runs. It exits
   0 on every error.

Delivery is at-least-once (see `docs/client.md`).

## Known limits

- Hooks fire only on activity, so an idle hooks-only session goes `stale`
  after `stale_after` even though it is alive. There is no background
  heartbeat in v1.
- `Notification` was not captured from a real run. Its test payload,
  `testdata/claude/hooks/Notification.constructed.json`, is built from the
  docs.
