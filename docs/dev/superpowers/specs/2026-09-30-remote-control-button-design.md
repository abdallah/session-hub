# Remote Control button design

Date: 2026-09-30. Status: approved in chat section by section; this document
is for review before planning.

## Goal

From the sessionhub dashboard on a phone, turn on Claude Code Remote Control for a
session with one tap, then open the session in the Claude app. If the session
isn't running, the same tap resumes it with Remote Control on.

The same channel lets `sessionhub remote-control <id>` work for a session on another
machine without SSH. The printed `ssh` command stays as a backup.

## Decisions

| Topic | Decision | Why |
|---|---|---|
| Who may trigger | The existing dashboard read cookie, only for this one action, only with an `X-Hub-Action` header | No new secret on the phone. Turning on Remote Control exposes the session only to the user's own Claude account. |
| Sessions not running | Resume them with `claude --resume <id> --remote-control` in a new herdr workspace | Pick up any session from the phone. |
| Delivery to the machine | Long polling by the plugin watcher | Near-instant, and keeps today's model: machines call the server, never the reverse. |
| SSH | Kept as the backup path for the CLI | Works when the channel or watcher is down. |

This changes one earlier spec decision: the dashboard is no longer strictly
read-only. It may create control requests, and nothing else.

## Facts this rests on

Verified live on `bluebox` (herdr 0.9.3, Claude Code 2.1.285):

- `herdr agent prompt <pane> '/remote-control'` (socket `agent.prompt`) on an
  idle Claude pane runs the command. The pane then shows
  `/remote-control is active · Continue here, on your phone, or at https://claude.ai/code/session_…`.
- `agent.prompt` refuses with `agent_blocked` before sending input when the
  agent is waiting on a prompt.
- `claude --help` lists `--remote-control [name]`. The CLI task verifies the
  flag live before this work starts.

## Data model

New table `control_requests`:

| Column | Notes |
|---|---|
| `id` | Random ID, for example `cr_` + 16 random bytes base64url |
| `session_id`, `machine_id` | Foreign keys. The machine is the session's owner at creation time. |
| `action` | Always `remote_control`. Checked against a fixed list in code. |
| `state` | `pending`, `claimed`, `done`, `failed`, or `expired` |
| `requested_by` | `dashboard` or `machine:<name>` |
| `created_at`, `expires_at` | `expires_at` = `created_at` + 2 minutes |
| `claimed_at`, `finished_at` | Nullable |
| `url` | The claude.ai link, when found |
| `detail` | The failure reason or a note such as "sent, link not seen" |

Schema version 3, upgraded in place like version 2.

`api.Session` gains:

- `RemoteControl *ControlRequest`: the session's latest request.
- `RemoteControlURL string` and `RemoteControlAt *time.Time`: the last link
  found, cleared when the session ends.
- `Controllable bool`: the session has a herdr pane, and its machine's watcher
  polled within the last 2 minutes.

New event kinds: `remote_control_requested` and `remote_control_result`.

## Server API

1. `POST /v1/sessions/{id}/remote-control`
   - Auth: the read cookie with header `X-Hub-Action: remote-control`, or a
     machine token. The bearer read token and the cookie without the header
     are refused.
   - Rejects the session with `409 not in herdr` when it has no herdr pane or
     its machine's watcher is offline.
   - If the session already has a `pending` or `claimed` request, returns that
     request with `200`. Otherwise creates one and returns `202`.
   - Refuses with `429` when the machine already has 10 pending requests.
   - Records a `remote_control_requested` event.
2. `GET /v1/machines/self/control?wait=30`
   - Machine tokens only. `wait` is clamped to 1–30 seconds.
   - Records the machine's last poll time, which drives `Controllable`.
   - Returns the oldest pending request for the machine and marks it
     `claimed` in the same transaction. With nothing pending, it waits up to
     `wait` seconds, woken in-process when a request is created, then answers
     `204`.
   - At most 2 open polls per machine. A third is refused with `429`.
3. `POST /v1/control/{request-id}/result`
   - Machine tokens only, and only the machine that claimed the request.
   - Body: `{"state":"done"|"failed","url":"…","detail":"…"}`. `url` must
     match `^https://claude\.ai/code/session_[A-Za-z0-9_-]+$`. `detail` goes
     through the free-text rules.
   - On `done` with a URL, sets `RemoteControlURL` and `RemoteControlAt` on
     the session. Records a `remote_control_result` event.

Expiry is applied lazily on every read and claim: a `pending` or `claimed`
request past `expires_at` becomes `expired`.

## Machine side (plugin watcher)

The watcher gains a second loop that keeps the long poll open.

- After a network error it backs off from 5 seconds up to 60 seconds. On 401
  or 403 it logs and waits 5 minutes.
- For each claimed request it takes a fresh herdr snapshot and finds the
  session's pane:
  - **Claude running in the pane, with a matching `agent_session`:** send
    `/remote-control` with `agent.prompt`, then read the pane for up to
    15 seconds for a link that wasn't there before. Report `done` with the
    link, or `done` with "sent, link not seen".
  - **`agent_blocked`:** report `failed` with "waiting on a prompt in the
    pane". Nothing is typed.
  - **Not running** (ended, or the pane is gone or at a shell): create a herdr
    workspace in the session's recorded directory, without focus, labeled
    `sessionhub: <title>`. Start `claude --resume <id> --remote-control` with
    `agent.start` there, then wait for the link as above. If the directory no
    longer exists, report `failed` and create nothing.
  - **The server says live, but no matching pane exists:** report `failed`
    with "looks live but pane not found". Never start a second copy of a
    running session.
- The logic lives in `internal/resume`, shared with `sessionhub remote-control` and
  `sessionhub resume --remote-control`.

## CLI

`sessionhub remote-control <id>`:

- **Local session:** unchanged from the CLI task.
- **Session on another machine:** post a control request through the server,
  then poll it every 2 seconds for up to 2 minutes and print the link or the
  reason. If the request can't be created (`409`, network error) or it
  expires, print the `ssh -t <host> '~/.local/bin/sessionhub remote-control <id>'`
  backup, and the `herdr --machine` form when a saved machine matches.

## Dashboard

- Each card shows a button when `Controllable` is true:
  - **Remote Control** on a live, idle, or done session.
  - **Resume with Remote Control** on an ended or stale session.
  - Disabled, with "waiting on a prompt in the pane", on a blocked session.
- A tap posts the request with the `X-Hub-Action` header and shows
  **Sending…**. The page then polls that session every 2 seconds for up to
  2 minutes.
- Result states:
  - **Done:** an **Open in Claude** link to the claude.ai session, plus a copy
    button.
  - **Failed:** the reason.
  - **Expired:** "the machine didn't respond in time".
- The last link and its time stay on the card until the session ends.
- The content security policy stays strict, and all text is still inserted
  with `textContent`. The page sends only this one kind of write request.

## Limits and failure handling

- At most one open request per session, 10 pending per machine, and 2 open
  polls per machine.
- Requests expire 2 minutes after creation, whether pending or claimed. An
  expired request is never retried automatically.
- A server restart drops open polls; watchers reconnect. Pending requests are
  in the database and survive.
- The action list is fixed in code.
- Every request and result is an event, visible in `sessionhub show`.

## Testing

- **Server:**
  - The table-driven auth test covers the three new endpoints: the cookie with
    and without `X-Hub-Action`, the bearer read token, a machine token, and a
    token of a different machine.
  - Duplicate-request handling, expiry of pending and claimed requests, and
    the limits.
  - The long poll with a fake clock: a request mid-wait wakes the poll, and
    30 seconds with nothing returns `204`.
  - The URL format check on results.
- **Watcher:** a decision table for inject, blocked, resume in a new
  workspace, "pane not found", and a missing directory, against `herdrtest`
  and a test server. Also reconnect and backoff.
- **CLI:** the remote path through the server, and the SSH backup when the
  request fails or expires.
- **Dashboard:** which card state shows which button, and a check that no raw
  HTML insertion is used.
- **Live, end to end:** tap the button on the real dashboard for a scratch
  session on `bluebox` (inject path) and on `tower` (resume path), and open each
  link on claude.ai.

## Out of scope

- Any action other than Remote Control.
- Sessions that aren't in herdr.
- Session insights (last reply, git activity, usage, recap). That work gets
  its own design.
