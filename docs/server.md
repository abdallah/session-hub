# sessionhub server

`sessionhub server` is the cross-machine registry for Claude Code sessions. It is one
process with an HTTP and JSON API and a SQLite database. `sessionhub machine` manages
the machines that may write to it.

## Run the server

```sh
sessionhub server
```

On a server host it usually runs as the systemd user unit
`deploy/sessionhub.service`, installed and restarted by `make deploy`, or in
Docker. See [`self-hosting.md`](self-hosting.md) for the first-time setup.

The server reads its configuration, opens the database, listens on every
`listen` address, and logs one line per request to stderr:

```
2026/09/30 13:14:35 POST "/v1/sessions" 201 9.737ms machine=tower
```

The fields are method, path (quoted, so control characters are escaped), status, duration, and who called: a machine name,
`web:<name>` for a browser session, or `-` for no valid credentials. The log never includes
the query string.

`SIGINT` or `SIGTERM` stops the server cleanly: it stops accepting
connections, answers open control long polls with `204`, waits up to 10
seconds for requests in flight, and closes the database.

If `server.toml` sets `read_token` or the environment sets `SESSIONHUB_READ_TOKEN`, the server logs one warning at startup and ignores the value.

### Configuration

The server reads `~/.config/sessionhub/server.toml` (mode `0600`). A missing file
means defaults. An unknown key is an error, so a typo does not go unnoticed.
The exception is a `telegram_*` key: an unknown one is a warning in the log,
because a Telegram setting never stops the server.

| Key | Default | Meaning |
|---|---|---|
| `listen` | `["127.0.0.1:8787"]` | Addresses to listen on. The server binds all of them before serving. |
| `public_url` | `https://sessionhub.example.com` | The URL clients use. `sessionhub machine add` prints it. |
| `read_token` | none | Ignored. The server logs a warning when it is set. Delete it once you no longer need to roll back to a release before web sign-in. |
| `stale_after` | `5m` | A Go duration. A session with no write for longer than this is `stale`. |
| `telegram_bot_token` | none | A Telegram bot token, in quotes. An unquoted token (`123456:ABC...`) is a TOML syntax error and stops the server. With `telegram_chat_id`, turns on alerts for **Blocked** and **Waiting on you** inbox items. Never logged. |
| `telegram_chat_id` | none | The chat the alerts go to, as a quoted string (`"-1001234567890"` for a group) or an integer. Any other type turns alerts off and logs why. |
| `task_ticket_url` | none | Link for a ticket ref in a note; `{ref}` is the ref, for example `"https://example.youtrack.cloud/issue/{ref}"`. Must be an `http` or `https` URL with a host. |
| `task_mr_url` | none | Link for a merge request ref (`!NNNN`) in a note; `{n}` is the number. Same rules. |

Example:

```toml
listen = ["127.0.0.1:8787"]
public_url = "https://sessionhub.example.com"
stale_after = "5m"
```

### Environment overrides

Each variable, when set and non-empty, replaces the file value.

| Variable | Replaces |
|---|---|
| `SESSIONHUB_SERVER_CONFIG` | The config file path. |
| `SESSIONHUB_DB` | The database path, default `~/.local/share/sessionhub/sessionhub.db`. |
| `SESSIONHUB_LISTEN` | `listen`, as a comma-separated list. |
| `SESSIONHUB_PUBLIC_URL` | `public_url`. |
| `SESSIONHUB_READ_TOKEN` | Nothing. Ignored, with a warning at startup. |
| `SESSIONHUB_STALE_AFTER` | `stale_after`. |
| `SESSIONHUB_TELEGRAM_BOT_TOKEN` | `telegram_bot_token`. |
| `SESSIONHUB_TELEGRAM_CHAT_ID` | `telegram_chat_id`. |

## Manage machines

`sessionhub machine` works on the local database (`SESSIONHUB_DB`) directly, so you run it on
the server host. It does not need the server to be running, and changes take
effect for a running server at once.

```sh
sessionhub machine add <name> [--ssh-host H] [--herdr-host H] [--json]
sessionhub machine rm <name> [--yes]
sessionhub machine ls
```

- `add` creates the machine and prints a new token once. The database stores
  only the token's SHA-256 hash. If the machine exists, `add` rotates its
  token: the old token stops working immediately.
- `--ssh-host` is the host in the machine's resume commands
  (`ssh -t <ssh_host> '~/.local/bin/sessionhub resume <id>'`). `--herdr-host` is for herdr remote
  attach. For a new machine, both default to the name. When you rotate a
  token without these flags, the stored hosts are kept.
- `--json` prints one line and nothing else on stdout, for `sessionhub join`:
  `{"name":"tower","token":"hub_m_...","server_url":"https://sessionhub.example.com"}`.
- `rm` revokes the machine's token and deletes the machine with its sessions,
  events, and reports. Without `--yes`, `rm` deletes nothing, prints how many
  sessions, events, and reports it would delete, and exits non-zero. Run it
  again with `--yes` to confirm. There is no undo.
- `ls` prints each machine's name, hosts, when its token was last used, and
  its move key's fingerprint (`-` before its watcher registers one). Compare
  it with `sessionhub move-key` on that machine.

A machine name is 1 to 64 letters, digits, `.`, `_`, or `-`, starting with a
letter or digit. A host may not contain spaces or shell metacharacters,
because it ends up in a shell command.

## HTTP API

All bodies are JSON, and every error is `{"error": "..."}`. The request and
response types are in `internal/api/types.go`.

### Authentication

Machines send `Authorization: Bearer <token>`. Browsers send the
`__Host-hub_session` cookie.

- A machine token (`hub_m_...`) can read and write. Every write is attributed
  to that machine. A bearer token that is not a machine token, such as an old
  read token (`hub_r_...`), gets `401`.
- The `__Host-hub_session` cookie holds a browser session token (`hub_s_...`). It
  authorizes reads, `POST /v1/sessions/{id}/remote-control` with the header
  `X-Hub-Action: remote-control`, `POST /v1/inbox/{id}/dismiss` and
  `POST /v1/inbox/{id}/snooze` with `X-Hub-Action: triage`,
  `POST /v1/instructions` and `DELETE /v1/instructions/{id}` with
  `X-Hub-Action: instructions`, `POST /v1/messages` with
  `X-Hub-Action: send`, `POST /v1/permissions/{id}/decide` with
  `X-Hub-Action: approve`, `POST /v1/sessions/{id}/move` with
  `X-Hub-Action: move`, `POST /v1/machines/{name}/start` with
  `X-Hub-Action: start`, and
  `POST /logout` with `X-Hub-Action: sign-out`. Nothing else: the cookie never
  reaches `/v1/logins` or `/v1/web-sessions`, and with any other write it gets
  `401`. A cookie write without the right header gets `403`. The header makes
  a cross-site request need a CORS preflight, which the server never grants.
- An unknown, revoked, or expired session counts as no credentials: `401`.
- A session lasts 30 days from its last use. When a request finds
  `last_used_at` more than one hour old, the server moves `last_used_at` to
  now and `expires_at` 30 days later, and sends the cookie again with a fresh
  `Max-Age`.
- `GET /v1/sessions` called with the cookie carries the header
  `X-Hub-Session-Name` with the session's name.

The server compares machine tokens in constant time. It looks up a session by its token's SHA-256 hash. Each request with a machine token updates that machine's `last_seen`.

### Endpoints

| Method and path | Auth | Success | Purpose |
|---|---|---|---|
| `GET /healthz` | none | `200`, body `ok` | Liveness check. |
| `GET /favicon.ico` | none | `200`, `image/x-icon` | The dashboard's browser icon: a 32-pixel PNG in an ICO file, drawn when the server starts. Browsers ask for it on their own, so the page has no `<link>`. |
| `GET /apple-touch-icon.png` | none | `200`, `image/png` | The same icon at 180 pixels, for a phone's home screen. |
| `GET /` | read (bearer or cookie) | `200` HTML; `401` signed-out HTML otherwise | The dashboard. A `?token=` parameter is ignored. |
| `GET /login/{code}` | none | `200` HTML; `410` HTML | The sign-in confirm page. Changes nothing. |
| `POST /login/{code}` | none, `Origin` must equal `public_url` | `303` to `/` with the cookie; `403`, `409`, or `410` HTML | The **Sign in** button. |
| `POST /logout` | cookie with `X-Hub-Action: sign-out` | `204`, cookie cleared | Ends the calling browser's session. A machine token gets `403`. |
| `POST /v1/sessions` | machine | `201` new, `200` existing; the `Session` | Upsert one session (`SessionUpsert`). |
| `PUT /v1/machines/self/herdr-sessions` | machine | `200`, a reconcile result | Snapshot of this machine's herdr agent panes (`HerdrSessionsPut`). |
| `POST /v1/sessions/{id}/events` | machine | `200`, the `Session` | Record an event (`EventIn`). |
| `POST /v1/sessions/{id}/report` | machine | `200`, the `Session` | Store a progress report (`ReportIn`). |
| `POST /v1/sessions/{id}/title` | machine | `200`, the `Session` | Set a user title (`TitleIn`). |
| `PUT /v1/sessions/{id}/digest` | machine | `200`, the `Session` | Store the session's digest (`DigestIn`). See [Digests](#digests). |
| `POST /v1/sessions/{id}/remote-control` | machine, or cookie with `X-Hub-Action: remote-control` | `202` new, `200` open one; a `ControlRequest` | Ask the session's machine to turn on Remote Control. See [Remote Control requests](#remote-control-requests). |
| `GET /v1/machines/self/control?wait=30` | machine | `200` a `ControlClaim`; `204` none | The watcher's long poll. |
| `POST /v1/control/{id}/result` | machine (the claiming one) | `200`, the `ControlRequest`, the `Message` for a `msg_` ID, or the `StartRequest` for an `st_` ID | The watcher's result (`ControlResultIn`). |
| `GET /v1/sessions` | read | `200`, `[]Session` | List sessions, most recently seen first. |
| `GET /v1/sessions/{id}` | read | `200`, `SessionDetail` | One session with every report and the last 50 events, newest first. |
| `GET /v1/machines` | read | `200`, `[]Machine` | Machines, by name. `online` is true when the machine's watcher polled in the last 2 minutes. |
| `GET /v1/inbox` | read | `200`, `Inbox` | Sessions that need you. See [Inbox](#inbox). |
| `POST /v1/inbox/{id}/dismiss` | machine, or cookie with `X-Hub-Action: triage` | `204` | Hide the item until a later trigger (`TriageIn`, `since` only). |
| `POST /v1/inbox/{id}/snooze` | machine, or cookie with `X-Hub-Action: triage` | `204` | Hide the item until `until` or a later trigger (`TriageIn`). |
| `GET /v1/instructions` | read | `200`, `InstructionList` | The standing rules, oldest first, and their `version`. See [Shared instructions](#shared-instructions). |
| `POST /v1/instructions` | machine, or cookie with `X-Hub-Action: instructions` | `201`, the `Instruction` | Add a rule (`InstructionIn`). `400` invalid, `409` past 2,000 characters. |
| `DELETE /v1/instructions/{id}` | machine, or cookie with `X-Hub-Action: instructions` | `204`; `404` unknown | Remove a rule. |
| `POST /v1/messages` | machine, or cookie with `X-Hub-Action: send` | `200`, `MessagesOut` | Send a message to 1 to 20 sessions (`MessagesIn`). `429` past 30 a minute. See [Messages to sessions](#messages-to-sessions). |
| `GET /v1/tasks?state=open` | read | `200`, `[]Task` | Open tasks (`proposed`, `todo`, `in_progress`, `done_proposed`). `open` is the default and the only value; another value is `400`. |
| `POST /v1/tasks` | machine, or cookie with `X-Hub-Action: tasks` | `201` new, `200` existing; the `Task` | Create a task (`TaskIn`). See [Tasks](#tasks). |
| `POST /v1/notes` | machine, or cookie with `X-Hub-Action: tasks` | `200`, `NoteResult` (`task`, `action`: `created`, `joined`, or `done`) | Route a worklog note to a task (`NoteIn`: `id`, a `te_` ID that makes a retry a no-op; `text`, 1 to 500 characters; optional `session_id`). A ref (`systemsdev-NNNNN`, `!NNNN`) picks that task, else the session's current task, else a new one. A note starting with `done:`, `finished`, `closed`, or `close ` marks it done; any other puts it in progress. `409` for another machine's session, `404` for an unknown one. See [Tasks](#tasks). |
| `POST /v1/tasks/{id}/state` | machine, or cookie with `X-Hub-Action: tasks` | `200`, the `Task` | Move a task (`TaskStateIn`: `to`, `note`, `session_id`, `event_id`). `409` for a move the actor may not make. |
| `POST /v1/tasks/{id}/merge` | machine, or cookie with `X-Hub-Action: tasks` | `200`, the target `Task` | Merge a task into another (`TaskMergeIn`: `into`); the source is dropped and its sessions and spans move to the target. `409` when the source is dropped or merged; `400` when the target is proposed or dropped. |
| `POST /v1/tasks/{id}/sessions` | machine, or cookie with `X-Hub-Action: tasks` | `200`, the `Task` | Link a session (`TaskLinkIn`: `session_id`, `event_id`). |
| `PATCH /v1/tasks/{id}` | machine, or cookie with `X-Hub-Action: tasks` | `200`, the `Task` | Edit `title`, `ref`, `ref_url`, or `source` (`TaskEditIn`). Always acts as you. |
| `GET /v1/tasks/day?date=YYYY-MM-DD&tz=Area/City` | read | `200`, `TaskDay` | The **Todo**, **In progress**, and **Done** columns for a day. `date` is required; a missing `tz` means UTC. `400` for a bad date or zone. |
| `GET /v1/tasks/review` | read | `200`, `TaskReview` | Proposed tasks, done proposals, and sessions with no task. |
| `POST /v1/tasks/review/ignore` | machine, or cookie with `X-Hub-Action: tasks` | `204` | Hide sessions from **Sessions with no task**: one in `session_id`, or up to 500 in `session_ids`. All or nothing: an unknown session is `404` and hides none. |
| `GET /v1/messages/{id}` | read | `200`, `Message` | One message and its delivery state. |
| `POST /v1/sessions/{id}/permissions` | machine (the owning one) | `201`, `PermissionRequest` | The permission hook's request (`PermissionIn`). `410` for an ended session. See [Permission requests](#permission-requests). |
| `GET /v1/permissions/{id}/decision?wait=30` | machine (the owning one) | `200` the request once it is not open; `204` none yet | The permission hook's long poll. |
| `POST /v1/permissions/{id}/decide` | machine, or cookie with `X-Hub-Action: approve` | `200`, the `PermissionRequest` | Allow once or deny (`DecisionIn`). `409` already decided, or an allow for a request whose input was cut ("the input was cut; allow it in the terminal"; the request stays open and a deny still works), `410` expired, answered in the terminal, or closed. |
| `POST /v1/sessions/{id}/move` | machine, or cookie with `X-Hub-Action: move` | `202`, the `Move` | Move the session to a machine or to `cloud` (`MoveIn`). `409` with the reason when a rule fails. See [Moves](#moves). |
| `GET /v1/moves/{id}` | read | `200`, the `Move` | One move and its state. |
| `PUT /v1/moves/{id}/bundle` | machine (the move's source, while `packing`) | `200`, the `Move` | Upload the sealed bundle (`application/octet-stream`, at most 64 MiB; `413` above). |
| `GET /v1/moves/{id}/bundle` | machine (the move's target, while `unpacking`) | `200`, the bundle | Download the sealed bundle. `410` when its file is gone. |
| `POST /v1/moves/{id}/result` | machine (the source while `packing`, the target while `unpacking`; the source again once the move ended and its part closed) | `200`, the `Move` | Report `done` or `failed` (`MoveResultIn`). After the move ended, the source's result with the final state adds its `detail` as a note. |
| `PUT /v1/machines/self/move-key` | machine | `204` | Register this machine's move public key (`MoveKeyIn`). |
| `POST /v1/sessions/{id}/blocked-on` | machine (the owning one) | `200`, the `Session` | Set or clear what the session waits on (`BlockedOnIn`). See [The Claude Code mod](#the-claude-code-mod). |
| `PUT /v1/sessions/{id}/usage` | machine (the owning one) | `200`, the `Session` | Store the context window fill and cost (`UsageIn`). |
| `GET /v1/sessions/{id}/messages/next?wait=25` | machine (the owning one) | `200` a `ModMessage`; `204` none | The mod's long poll for the session's next message. |
| `POST /v1/messages/{id}/result` | machine (the message's) | `200`, the `Message` | The mod's result for a message (`MessageResultIn`). |
| `POST /v1/machines/{name}/start` | machine, or cookie with `X-Hub-Action: start` | `202`, the `StartRequest` | Ask the machine's watcher to start a new session (`StartIn`). `404` unknown machine, `409` watcher offline, `429` at 5 pending. See [Starting sessions](#starting-sessions). |
| `GET /v1/starts/{id}` | read | `200`, the `StartRequest` | One start request and its state. |
| `POST /v1/logins` | machine | `201`, `Login` | Create a one-time sign-in link (`LoginIn`). `400` bad name, `409` name taken, `429` at 5 unused links. |
| `GET /v1/web-sessions` | machine | `200`, `WebSessionList` | Signed-in browsers, by name. Never a token or hash. |
| `DELETE /v1/web-sessions/{id}` | machine | `204`; `404` unknown | Sign a browser out. |

"read" means a machine token or the session cookie.

A wrong method on a known path returns `405` with an `Allow` header. An
unknown path returns `404`.

### Dashboard

`GET /` is a page that lists every session grouped by machine. It refreshes
every 30 seconds. The page is one embedded file,
`internal/server/dashboard/index.html`, with inline CSS and JavaScript and no
external requests. It uses the full width of the window at every screen size, with a 16-pixel
margin on a phone and 24 pixels on a wider screen, and fits a 360-pixel phone
screen without horizontal scrolling. The header, the tabs, the filter, and the cards share the same
left and right edges. On a wide screen the tabs, the filter, and **Collapse
all** sit on one row; on a phone they stack. With a mouse or trackpad, buttons
are smaller and show hover states; on a touch screen every target is at least
44 pixels tall.

The **Sessions** tab shows one section per machine. Each machine header shows
the machine name, a count such as "3 active, 2 ended · 1 needs you" (the
inbox items for that machine), and toggles the machine's sessions when you
tap it. **Collapse all**, beside the filter on the **Sessions** tab only,
collapses every machine, so
only the headers show; when every machine is collapsed, the button reads
**Expand all**. Ended sessions stay in an **Ended** group under their
machine.

A session card is compact: one header line with a status dot, the title, the
time since the session was last seen, and a state badge such as
"blocked 4m", "working", "idle", "stale", or "ended". The blocked time comes
from the inbox item's `since`. Tap the header to expand the card; it then
shows the agent state with `context NN%` when the session's mod reported its
context window fill, what a blocked session waits on (**Blocked on:**, from
its mod), the working directory and branch, the recap, the latest report, the
last prompt, the summary line, **Details**, the resume command with **Copy**,
and the Remote Control row. The headers are buttons, so **Tab**, **Enter**,
and **Space** work too.

Collapsed machines and expanded cards survive the 30-second refresh and are
remembered per browser in `localStorage` (`sessionhub.collapsedMachines` and
`sessionhub.openCards`). If storage is unavailable, for example in a private window,
the page keeps the state until you reload. While the filter has text, every
machine with a match is open and **Collapse all** is unavailable; clearing
the filter brings back the remembered state.

The page has three tabs, **Inbox**, **Sessions**, and **Rules**. **Inbox** shows
`GET /v1/inbox` (see [Inbox](#inbox)) and is selected when the page loads
with items in it; after that, the page never switches tabs on its own.
The URL fragment `#inbox` opens the **Inbox** tab, `#sessions` the
**Sessions** tab, and `#rules` the **Rules** tab, whatever the inbox holds; the Telegram alert's **Open
inbox** button links to `/#inbox`. Changing the fragment later switches the
tab too. The **Inbox**
label shows the count, and the page title becomes `(3) sessionhub`. The inbox polls
with the session list, every 30 seconds.

Each inbox row reads like a phone notification: the title in bold, the time
since the item's `since`, up to two lines that say what is needed, and the machine
and group in small text. The line shows, on a **Blocked** row, the question
the session's mod reported; else the `waiting_on` items, else the recap, else
the group's label. Every string goes in as text, never as markup. Tapping the row does the most useful thing it
can, and the row names it:

1. If the session has a Remote Control link, the row opens it in a new tab
   (**Open in Claude**).
1. Otherwise, if the session is controllable and not blocked, the row sends
   a Remote Control request and expands to show its progress.
1. Otherwise, the row expands to show the resume command.

Below the row is one row of small text buttons: **Done**, **Later**,
**More** on rows whose tap does something other than expand, and, at the
right, the row's tap action by name, such as **Open in Claude** or **Show
resume command**. **Later** opens a small menu over the rows below it: 1
hour, 4 hours, or 9:00 tomorrow. A choice, **Escape**, or a tap outside the
menu closes it. An expanded row shows the recap,
the last prompt, the summary line, the card's Remote Control row, and the
resume command with **Copy resume**. **Done** (dismiss) and **Later**
(snooze) remove the row at once and send the request with
`X-Hub-Action: triage` and the `since` the row showed; if the request fails,
the row comes back with the error. The filter applies to the **Inbox** and
**Sessions** tabs; the count ignores it.

A **Blocked** row whose session has an open permission request shows the
tool and its input: a `Bash` command in code, with its newlines kept, and
for another tool its file, path, URL, or pattern as text, then its whole
stored input (at most 8 KiB) as compact JSON in a box that scrolls
vertically. Control characters other than newlines and tabs, and Unicode
bidirectional controls, show as the replacement character U+FFFD, so the
text reads in the order it runs. If the hook cut the input (`truncated`),
the approver cannot see all of it: the row says **Input was cut; check the
terminal before allowing.** and **Allow once** is off; **Deny** still
works. **Deny** asks for an optional reason, which
Claude sees; a reason over 200 characters or with a control character is
refused before anything is sent. **Allow once** asks once more, showing the
command or the whole input again with **Yes, allow once** and **Cancel**. Both answers send
`X-Hub-Action: approve`, and the page never offers a lasting allow. The row
then says **Allowed once.**, **Denied.**, that someone already answered, or
that it is too late (the prompt was answered in the terminal or expired),
until the session leaves **Blocked**. If the server refuses an allow because
the input was cut, the row says so and keeps **Deny**.

The **Rules** tab lists the standing rules (see
[Shared instructions](#shared-instructions)) with a **Delete** button each,
which asks for confirmation, and an add box that refuses a rule over 300
characters or past the 2,000-character total before it sends anything.
Writes send `X-Hub-Action: instructions`. The filter is hidden on this tab.

**Select**, beside the filter on the **Sessions** tab, shows a checkbox on
every card; an expanded card always shows one. A session sessionhub cannot message
(`messageable` is false: no sessionhub mod polls in it, and it is not in herdr on a
machine whose watcher polls) has its checkbox disabled. While sessions are selected, the **Send a message** bar at the
bottom of the **Sessions** tab lists them
with a text box and **Send to N sessions**, which asks for confirmation,
listing the sessions, and sends with `X-Hub-Action: send`. Each target's
result shows below: queued, or not sent and why.

**New session**, beside **Select** on the **Sessions** tab, opens a panel
that reads `GET /v1/machines`. **Machine** lists every machine, the ones
whose watcher is online first; an offline one is listed but can't be picked.
**Directory** suggests the 10 directories that machine's sessions used most
recently and must be absolute or start with `~/`. **First prompt** is
optional. **Trust this folder** sends `trust` (see
[Starting sessions](#starting-sessions)); it refuses `~` itself. **Start** sends `POST /v1/machines/{name}/start` with
`X-Hub-Action: start`, then reads `GET /v1/starts/{id}` every 2 seconds
until the request ends: **Open in Claude** with the new session's link, a
note when the link wasn't seen, or the machine's reason. A refused request
shows the server's error. See [Starting sessions](#starting-sessions).

A session in herdr whose machine's watcher polled in the last 2 minutes
(`controllable`) gets a button: **Remote Control** when it is live,
**Resume with Remote Control** when it is ended or stale, and a disabled
button with "waiting on a prompt in the pane" when it is blocked. A session
with an open move gets no button. A tap is
one of the page's write requests; the others are **Sign out**, the
inbox's **Done** and **Later**, the permission answers, the rule writes,
the send bar, moves, and **Start** in the New session panel. The card
shows **Sending…**, reads the session every 2 seconds until the request ends
or expires (2 minutes at most), and then shows **Open in Claude** with the
link and a **Copy** button, the machine's reason, or "The machine didn't
respond in time." A refused request shows the server's error as text.

An expanded card also gets **Move to…**. When the session can't move now
(it ended, it is not in herdr or its watcher is offline, its agent is not
idle or done, or a move is under way) the button is disabled and the reason
shows beside it. Opening the menu reads `GET /v1/machines` and lists every
other machine (one that can't take a session now is disabled, with "its
watcher is offline" or "no move key yet") and, for a GitHub remote,
**Cloud**. A choice asks for confirmation, naming what moves, then sends
`POST /v1/sessions/{id}/move` with `X-Hub-Action: move` and reads
`GET /v1/moves/{id}` every 2 seconds for up to 3 minutes. The card shows the
state ("Moving to tower: packing on bluebox…"), the server's reason when it
refuses or the move fails, and, for a finished cloud move, **Open in
Claude** with the cloud session link. The 30-second list refresh shows the
latest move after that.

The outcome of a finished request stays on the card for 10 minutes, also
after a reload, and a reload that finds a request still open starts reading
the session again. The last link and its age stay on the card until the
session ends; once the session has no link, the card shows none, even from
a recent request. The link must match `https://claude.ai/code/session_<id>`;
the page opens nothing else.

A browser signs in with a one-time link from `sessionhub login`; see
[Sign in a browser](#sign-in-a-browser). `GET /` without a valid session
returns a small HTML `401` page that says to run
`sessionhub login --name <device>`. A `?token=` parameter is ignored and never
compared.

The header shows "Signed in as **<name>**", from the `X-Hub-Session-Name`
header, and a **Sign out** button. **Sign out** sends `POST /logout` with
`X-Hub-Action: sign-out` and then shows the signed-out message. If a poll gets
`401`, for example after `sessionhub login rm`, the page stops polling and shows the
signed-out message instead of an empty list.

The HTML response carries a strict `Content-Security-Policy`:
`default-src 'none'`, plus `script-src` and `style-src` set to the SHA-256
hashes of the page's one inline script and one inline style. The server
computes the hashes from the embedded file at startup, so editing the page
needs no other change. The page cannot use inline event handlers or `style=`
attributes, and a test enforces that.

The copy button uses `navigator.clipboard.writeText`, which needs a secure
context: `https://` or `http://127.0.0.1`. If the API is missing or fails, the
page selects the command text and tries `document.execCommand("copy")`; if
that fails too, the text stays selected for a manual copy.

### Sign in a browser

A machine with a client config creates a link, and the browser confirms it:

1. `sessionhub login --name phone` sends `POST /v1/logins` with its machine token.
   The server stores the code's hash and answers with
   `<public_url>/login/<code>`. The code is 26 characters of lowercase
   base32 and works once, for 10 minutes. At most 5 unused links exist at a
   time across all machines.
2. `GET /login/<code>` shows "Sign in this browser as **phone**?" with a
   **Sign in** button. It changes nothing, so a link previewer that fetches
   the link doesn't use it.
3. **Sign in** posts to the same URL. The server checks that `Origin` equals
   `public_url` (`403` otherwise), uses the code in one transaction (the
   first of two presses wins; the second gets `410`), creates the session,
   sets the `__Host-hub_session` cookie (`HttpOnly`, `Secure`, `SameSite=Lax`,
   `Path=/`, 30 days), and redirects with `303` to `/`.

A used, expired, or unknown code gets a `410` page that says to run
`sessionhub login` again. If a browser session took the name after the link was
made, the press gets a `409` page and the link keeps working until it
expires. Every refused press is logged with the client address from
`Cf-Connecting-Ip`, else the remote address. The request log prints
`/login/<code>`, never the code.

The login pages carry `Content-Security-Policy: default-src 'none';
style-src '<hash>'; form-action 'self'; base-uri 'none'; frame-ancestors
'none'`, `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`, and
`Referrer-Policy: same-origin`. The policy is `same-origin`, not
`no-referrer`, because with `no-referrer` browsers send `Origin: null` on the
form post.

`sessionhub login ls` lists sessions (`GET /v1/web-sessions`) and `sessionhub login rm`
revokes one (`DELETE /v1/web-sessions/{id}`); any machine token may do both.
**Sign out** on the dashboard (`POST /logout`) ends only that browser's
session. Removing a machine deletes the links and sessions it created.

### Sessions and upserts

- An empty field never overwrites a stored value.
- The first upsert sets the session's machine and records a `registered`
  event. A write to that session from another machine's token returns `409`.
- `first_prompt` is recorded once; a later value is ignored.
- `source` must be `plugin`, `hooks`, or `mcp`. `agent_state`, if set, must be
  `idle`, `working`, `blocked`, `done`, or `unknown`.
- Every upsert, event, report, and title call sets `last_seen_at` to now. It
  also clears `ended_at`, except for `pane_closed` and `ended` events.
- Each session has `resume_command`: `ssh -t <ssh_host> '~/.local/bin/sessionhub resume <id>'`, using
  the owning machine's `ssh_host`, or its name if that is empty.

### Titles

A title has a source, from highest to lowest precedence:

1. `user`: from `POST /v1/sessions/{id}/title`.
2. `herdr`: from `title_hint` in an upsert.
3. `claude`: Claude Code's automatic title (`ai-title`), from a digest.
4. `prompt`: from `first_prompt` in an upsert.

A lower source never replaces a higher one. A new `herdr` hint replaces an
older one; a new `user` title replaces an older one.

### Events

`kind` is one of `state_changed`, `prompt`, `pane_closed`, or `ended`.
`registered` is recorded by the server only. So are `digest` (source `server`,
recorded when a digest changes the recap or the links) and
`remote_control_requested` (source `server`) and `remote_control_result`
(source `plugin`); their payloads carry `request_id` and, for a result,
`state`, `url`, and `detail`. `requested_by` is in the request's payload.

- `state_changed` needs `payload.agent_state` and stores it on the session,
  unless a `state_changed` event with a later `ts` already set the state. Then
  the server stores the event row and leaves the state alone, so a queued
  event that arrives late cannot overwrite a newer one. The guard compares
  event timestamps only (client clock, clamped to the server's now). An upsert
  or snapshot that sets `agent_state` never changes the guard, so an event a
  few seconds behind the server clock still applies after one.
- `pane_closed` and `ended` set `ended_at` if it is not set yet.
- `ts` is kept as sent, or set to now if it is missing or later than the
  server's clock. Events sort by `ts`, so send full-precision times.
- An event for an unknown session returns `404`: upsert the session first.

### Digests

`PUT /v1/sessions/{id}/digest` stores what a machine learned about a session
from its transcript and git. Each digest replaces the previous one. The
request needs a machine token, and the session must belong to that machine.

```json
{
  "as_of": "2026-09-30T11:00:00Z",
  "first_at": "2026-09-30T09:12:00Z",
  "recap": "Added the digest endpoint and its tests.",
  "recap_at": "2026-09-30T10:58:00Z",
  "ai_title": "Add session digests",
  "custom_title": "",
  "links": [{"number": "7", "url": "https://github.com/o/r/pull/7", "repo": "o/r"}],
  "tokens": {"input": 1200, "output": 800, "cache_read": 90000, "cache_write": 4000},
  "cost_usd": 1.42,
  "cost_at": "2026-09-30T10:59:00Z",
  "git": {"commit_count": 2, "uncommitted": 1, "unpushed": 0,
          "commits": [{"sha": "e479dec", "subject": "Store session digests"}]},
  "bad_lines": 0
}
```

Only `as_of` is required. The server rejects a bad value with `400`:

- `as_of` is at most 5 minutes in the future.
- `recap` is at most 600 characters. Titles are at most 200.
- `git.commits` has at most 5 entries, and each subject is at most 120
  characters.
- `links` has at most 20 entries. Each `url` starts with `https://` and is at
  most 300 bytes.
- Counts and token totals are between 0 and 10^12. `git.unpushed` may also be
  `-1`. `cost_usd` is between 0 and 100000.

Other status codes: `404` for an unknown session, `409` for a session that
another machine owns, and `413` for a body larger than 16 KiB
(`MaxDigestBytes`; every other endpoint allows 64 KiB).

If the digest's `as_of` is older than the stored one, the server keeps the
stored digest and still returns `200` with the session. An equal `as_of`
replaces the stored digest, but a digest without `git` keeps the stored `git`,
because an omitted `git` means unknown. See [Session digests](#session-digests) for how a
digest affects titles, events, and `last_seen_at`.

The `Session` from a list or a write carries `recap`, `recap_at`,
`last_prompt`, `last_prompt_at`, and `summary` (`as_of`, `git`, `latest_link`,
`cost_usd`, and `output_tokens`). `summary` is absent until a machine sends a
digest. `GET /v1/sessions/{id}` adds the whole stored digest as `digest`, with
`received_at`.

### The herdr snapshot

`PUT /v1/machines/self/herdr-sessions` is the plugin's heartbeat. The body is
every herdr agent pane on the machine for one herdr session:

1. Each entry is upserted with `source` forced to `plugin`. An entry without
   `herdr_session` gets the body's. This refreshes `last_seen_at`, and stores
   no event row.
2. Every session of this machine that has a non-empty `herdr_pane`, the same
   `herdr_session`, no `ended_at`, and is not in the snapshot is ended, with an
   `ended` event whose payload is `{"reason":"missing_from_snapshot"}`. A
   session whose `herdr_pane` is in the body's optional `unidentified_panes`
   is not ended: Claude still runs in that pane, but herdr reports no session
   ID for it, which happens for some resumed sessions.

Sessions without a pane (hooks-only), sessions in another herdr session, and
other machines' sessions are never touched. An entry registered to another
machine is skipped rather than failing the snapshot, so it cannot block the
heartbeat. The response reports what happened:

```json
{"upserted": 1, "ended": ["81c8ffd0-..."], "conflicts": [], "invalid": [{"id": "0a0b...", "reason": "herdr_pane \"-w1\": ..."}]}
```

An entry that fails validation is skipped and listed under `invalid` with its
`id` and a `reason`, so one bad pane cannot fail the heartbeat. A skipped entry
still counts as listed, so the server does not end its session as missing. An
empty or malformed snapshot-level `herdr_session` rejects the whole request
with `400`. The plugin logs each `invalid` entry.

### Status

Status is computed when you read, never stored:

1. `ended` if `ended_at` is set.
2. Otherwise `stale` if `now - last_seen_at > stale_after`.
3. Otherwise `blocked` if `agent_state` is `blocked`.
4. Otherwise `live`.

### Remote Control requests

A request asks the session's machine to turn on Claude Code Remote Control.
Its `id` is `cr_` plus 16 random bytes in base64url, its only `action` is
`remote_control`, and `requested_by` is `dashboard` or `machine:<name>`.

| State | Meaning |
|---|---|
| `pending` | Created, not yet claimed. |
| `claimed` | The machine's watcher took it and is working on it. |
| `done` | The watcher sent `/remote-control`, or resumed the session. `url` is the link when it saw one. |
| `failed` | The watcher did nothing, or a herdr call failed. `detail` says why. |
| `expired` | Two minutes passed since creation while it was pending or claimed. It is never retried. |

The `detail` a watcher sends, which the dashboard and the CLI show as is:

| State | `detail` |
|---|---|
| `done` | Empty (a new link), "Remote Control was already on", "Remote Control was already on; press Esc in the pane to close its dialog", "sent, link not seen", "resumed in a new herdr workspace", or "resumed in a new herdr workspace, link not seen". |
| `failed` | "waiting on a prompt in the pane", "a Remote Control dialog is open in the pane; press Esc there", "looks live but pane not found", "the session's directory no longer exists", "just resumed; try again in a moment", or a herdr error. |

- `POST /v1/sessions/{id}/remote-control` takes no body and needs the full
  ID. It answers `404` for an unknown session, `409` when the session has no
  herdr pane, its machine's watcher hasn't polled in 2 minutes, or it has an
  open move ("a move is open for this session"; see [Moves](#moves)), `200` with
  the session's open request if it has one, `429` when the machine already
  has 10 pending requests, and `202` with a new request, in that order.
- `GET /v1/machines/self/control?wait=N` records the machine's poll time
  (which drives `controllable`), then claims its oldest pending request in
  one transaction and answers `200` with `{"request": ..., "session": ...}`.
  A pending request whose session has an open move, queued just before the
  move, is never handed out: the claim marks it `failed` with detail "a move
  is open for this session" and moves on to the next one.
  With nothing pending it waits up to `N` seconds (clamped to 1–30, default
  30), and a new request wakes it at once. It then answers `204` with no
  body. A `wait` that isn't a number gets `400`, and `HEAD` gets `405`, so
  nothing but a `GET` claims a request. A machine may hold 2 polls open; a
  third gets `429`. The poll extends its own write deadline past the
  server's 30-second `WriteTimeout`. On shutdown the server answers open
  polls with `204`, and the watchers reconnect; pending requests are in the
  database.
- `POST /v1/control/{id}/result` takes `{"state": "done"|"failed", "url":
  "...", "detail": "..."}` from the machine that claimed the request; another
  machine gets `409`. `url` must match
  `^https://claude\.ai/code/session_[A-Za-z0-9_-]+$`, only on `done`.
  A request that isn't `claimed`, including one that expired while the
  machine worked on it, gets `409` and changes nothing. A `done` result with
  a `url` sets the session's `remote_control_url` and `remote_control_at`.
- Every session carries `remote_control` (its latest request),
  `remote_control_url` and `remote_control_at` (cleared by `pane_closed`, by
  `ended`, and when a snapshot ends it), and `controllable`.
- A request expires on read: the state reads `expired` once `expires_at`
  passes, and the next create, claim, or result stores it.

### Shared instructions

Standing rules every session sees, at its start and with every prompt. Each
rule is 1 to 300 characters with no control characters; the list holds at
most 2,000 characters, and an add past that is `409`. `GET
/v1/instructions` returns `{"instructions": [{"id", "text", "created_at",
"created_by"}], "version": "<16 hex digits>"}`; the version changes whenever
the list does, so clients skip an unchanged copy. `created_by` is the machine
name, or `web:<session name>` for a browser.

When Telegram alerts are on, each stored rule also sends a Telegram message
(see [Telegram alerts](#telegram-alerts)), whoever added it. A rule that a
session plants, for example through the MCP `remember` tool after the session
reads malicious content, then shows up at once instead of at the next rules
review.

### Messages to sessions

`POST /v1/messages` with `{"session_ids": [...], "text": "..."}` queues one
message per distinct session, 1 to 20 sessions, text 1 to 4,000 characters
(trimmed first; newlines and tabs allowed, no other control characters). A
machine token, or the session cookie with `X-Hub-Action: send`, may send. The response is `200`
with one result per target, in order:

    {"results": [{"session_id": "...", "id": "msg_...", "state": "queued"},
                 {"session_id": "...", "state": "refused", "detail": "the session is not in herdr"}]}

A target is refused when the session is unknown or has ended, or when its
mod has not polled in the last 2 minutes and the session has no herdr pane or
its machine's watcher has not polled in the last 2 minutes. The other targets
still go. `Session.messageable` says whether a send would be accepted, apart
from ended sessions.

The sender is `dashboard` for a browser, `cli on <machine>` for a machine
token, and `session <first 8 characters>` when a machine token sends
`from_session` (the MCP tool does). `from_session` must be a session the
calling machine owns: an unknown one is `404`, another machine's is `409`.
The limit is per caller, not per displayed sender: each machine token
(`machine:<name>`) and each browser session (`web:<name>`) may send 30
messages (one per target) in any 60 seconds, whatever `from_session` it
names; a send past that is `429` as a whole. The count is the stored
messages under that caller plus the send's own targets; refused targets of
earlier sends are not stored, so they do not count.

The watcher submits who sent the message, a blank line, and the text. For
a person (`dashboard` or `cli on <machine>`) the first line is
`From the user via sessionhub (<sender>):`; for a session it is
`From session <first 8 characters> via sessionhub (sent on the user's behalf):`,
so the reader can tell an agent's message from the person's. It gets the message from its long poll (`GET
/v1/machines/self/control`) as a claim whose `request.action` is `message`,
with the prompt in `text`, and answers on `POST /v1/control/{id}/result` with
`state` `delivered`, `refused` (with `detail`), or `busy` (the agent is
working or blocked; the message stays queued and is offered again 15 seconds
after the last offer). A `failed` result counts as `refused`. A message not
delivered within 10 minutes is `expired`.

The watcher leaves alone the messages of a session whose Claude Code mod
polled in the last 2 minutes, and those of a session without a herdr pane:
the mod claims them on `GET /v1/sessions/{id}/messages/next`, with the same
order and the same 15-second re-offer, and answers on `POST
/v1/messages/{id}/result`. See [The Claude Code mod](#the-claude-code-mod).

`GET /v1/messages/{id}` (read access) returns the message: `id`,
`session_id`, `machine`, `sender`, `text`, `state`, `detail`, `created_at`,
`updated_at`.

A delivered or expired message adds a `message` event to its session, with
`message_id`, `sender`, `state`, and the first 200 characters of the text.

### Permission requests

`sessionhub hook permission-request` posts each Claude Code permission prompt to
`POST /v1/sessions/{id}/permissions` (machine token, owning machine):
`{"tool_name": "Bash", "tool_input": {...}, "cwd": "...", "suggestions":
[...]}`. The answer is `201` with the request: `id` (`pr_...`), `session_id`,
`machine`, `tool_name`, `tool_input`, `truncated`, `state`, `created_at`, and
`expires_at` (10 minutes later). The tool input is stored as compact JSON;
over 8 KiB it is stored as a JSON string of its first 8,000 bytes and
`truncated` is `true`. `cwd` and `suggestions` are not stored.

The hook then holds `GET /v1/permissions/{id}/decision?wait=N` open (owning
machine, `N` from 1 to 30, default 30). It answers `200` with the request as
soon as its state is not `open`, else `204` after the wait.

`POST /v1/permissions/{id}/decide` with `{"decision": "allow"|"deny",
"reason": "..."}` decides once. A machine token, or the session cookie with
`X-Hub-Action: approve`, may decide. A reason is kept for a deny only, at most
200 characters. A second decision is `409`; an expired, answered-locally, or
closed request is `410`. An allow for a request whose input was cut
(`truncated`) is `409` with "the input was cut; allow it in the terminal",
whoever asks: nobody can have read all of it. The request stays open, and a
deny still decides it. Each decision adds a `permission` event to the
session with `request_id`, `tool`, `input` (the first 200 characters of the
input's main field), `decision`, `reason`, and `by` (`web:<name>` or
`machine:<name>`). sessionhub only ever allows once: it never sends Claude Code a
permanent rule.

States:

| State | Meaning |
|---|---|
| `open` | waiting for an answer, at most 10 minutes |
| `decided` | allowed or denied from sessionhub |
| `expired` | 10 minutes passed |
| `answered_locally` | the session left `blocked`: the prompt was answered in the terminal |
| `closed` | the session ended |

A state change out of `blocked` moves the session's open requests created at
or before the change to `answered_locally`: on a `state_changed` event, up to
the event's time; on a herdr snapshot, up to 5 seconds before the server's
time, because the snapshot may predate the request.

### Moves

A move takes a session, with its conversation, branch, and uncommitted
changes, to another machine, or hands it to a Claude Code cloud session. The
watchers do the work; the server checks the rules, relays a sealed bundle it
cannot read, and hands the session to the target. The design is in
`docs/dev/superpowers/specs/2026-10-03-move-session-design.md`.

Each watcher registers its X25519 public key with
`PUT /v1/machines/self/move-key` `{"public_key": "<base64>"}` when it starts.
`GET /v1/machines` shows each key's fingerprint as `move_key` and sets
`move_ready` when the machine has a key and its watcher polled in the last
2 minutes.

`POST /v1/sessions/{id}/move` `{"target": "tower"}` or `{"target": "cloud"}`
answers `202` with the move, or `409` with the reason when a rule fails: the
session has ended, is not in herdr, its agent is not `idle` or `done`, its
machine's watcher is offline or has no key, it already has an open move, the
target machine is unknown, the same machine, offline, or has no key, or a
cloud move's remote is not on GitHub.

States and who moves them:

| State | Set by | Next |
|---|---|---|
| `requested` | the request | the source's long poll claims it as `move-out` |
| `packing` | the source's claim | the source uploads (`uploaded`), or reports `failed`, or a cloud `done` with `cloud_url` |
| `uploaded` | the upload | the target's long poll claims it as `move-in` |
| `unpacking` | the target's claim | the target reports `done` or `failed` |
| `done`, `failed` | a result or a timeout | the source gets one more `move-out` claim, the finish, unless it posted the result itself |
| `cancelled` | nothing yet | treated like `failed` |

A move fails with detail `<state>: timed out` after 2 minutes in
`requested`, or 10 minutes in `packing`, `uploaded`, or `unpacking` since its
last change. The server checks on every move request and once a minute.

The claim's `move` field carries the move ID, the source and target names,
the state, and `peer_key`: the target's public key on `move-out`, the
source's on `move-in`. Each claim is handed out once.

On `done` for a machine move, the session's machine becomes the target, its
herdr fields and Remote Control link clear until the target's next
heartbeat, and a `moved` event records `move_id`, `from`, and `to`. Until
then the target's own writes for the session get `409`. On a cloud `done`,
the session ends with an `ended` event, reason `moved_to_cloud`, and a
`moved` event that carries `cloud_url`. Its `herdr_session`,
`herdr_workspace`, and `herdr_pane` clear too, so the dashboard offers no
Remote Control that would resume the local copy while the cloud session
works on it. The transcript stays on the source, so `sessionhub resume` can still
start the local copy on purpose.

Once a move ended and its source's part is closed (the source posted its own
result, or claimed the finish), the source may post a result with the
move's final state (`failed` for a `cancelled` move) and a `detail`: a restart or an archive that failed
there. The server appends the detail to the move's, with `; ` between, cut
to 300 characters, and changes nothing else, so `sessionhub move --status` and the
dashboard show it. A note the detail already ends with is answered `200`
and not added again, so a result retried after a lost answer appears once.
Any other result on an ended move is `409`.

Bundles live in `moves/<id>.sealed` next to the database, mode `0600`. The
upload streams to a temp file and is renamed when complete. The server
deletes a bundle when its move ends, and any file in that directory older
than one hour. It only ever holds ciphertext.

### Starting sessions

`POST /v1/machines/{name}/start` with `{"dir": "~/Code/app", "prompt": "..."}`
asks that machine's watcher to start a new Claude session with Remote Control
on, in a new herdr workspace. The dashboard's **New session** panel and
`sessionhub start` send it.

- `dir` is required: an absolute path, or one starting with `~/` (or `~`
  alone), at most 1,024 bytes, with no control characters. The server checks
  only its shape. The watcher expands `~`, resolves symbolic links, and
  refuses a directory that doesn't exist or is outside its user's home
  directory.
- `prompt` is optional, at most 4,000 characters; newlines and tabs are
  allowed, other control characters are not. The watcher submits it once
  Claude is idle.
- `trust` is optional, `false` by default. With `true`, the watcher marks
  the resolved directory, and only it, as trusted in Claude's config before
  starting, and refuses the home directory or a folder above it.
- The machine's watcher must have polled in the last 2 minutes (`409`
  otherwise), and the machine can have at most 5 pending start requests
  (`429`). An unknown machine is `404`.

A request is `pending` until the watcher claims it from its long poll, then
`claimed`, then `done` or `failed`; one still open 2 minutes after it was
made reads as `expired`. The claim is a `ControlClaim` whose `request.action`
is `start`, with the request in `start` and an empty `session`. The watcher
answers with `POST /v1/control/{st_ id}/result`: a `done` result may carry the
new session's Remote Control link in `url`. The new session itself registers
through the hooks and the herdr plugin like any other.

`requested_by` is `web:<name>` for a browser and `machine:<name>` for a
machine token. A machine can turn this off with `remote_start = false` in its
client config (see `docs/client.md`); its watcher then fails every start
request.

Before it creates a workspace, the watcher checks that Claude trusts the
directory: the directory or a folder above it has `hasTrustDialogAccepted`
in Claude's config (`~/.claude.json`, or `$CLAUDE_CONFIG_DIR/.claude.json`).
If it doesn't and `trust` is not set, the request fails with `<dir> is not
trusted by Claude on <machine>; open it once or pass --trust`, and nothing
starts. The watcher never answers Claude's trust prompt in a pane.

### The Claude Code mod

The mod runs inside each Claude Code session and calls `sessionhub mod`, which
calls these routes with the machine token. Each route takes only the machine
that owns the session: another machine's token gets `409`, an unknown
session `404`. A refused call writes nothing. Only the message poll sets the
session's `mod_seen_at`, so a session stays messageable while its mod
polls; if the mod's message loop stops, the herdr watcher takes the
session's messages again 2 minutes later, even while blocked-on and usage
reports go on. `blocked-on` and `usage` for a session that has ended answer
`200` with the session unchanged: a late report never revives a session.

- `POST /v1/sessions/{id}/blocked-on` with `{"text": "..."}` sets
  `blocked_on`, what the session waits on, such as `Question: Which library
  should we use? (date-fns, luxon, dayjs)`. The server cleans the text first:
  control and bidirectional characters removed, whitespace folded to one
  space, cut to 300 characters with `…`. A non-empty text also sets
  `agent_state` to `blocked` (and `blocked_at` when it was not blocked), so
  the session shows in the inbox. When `agent_state` is `idle` or `done`, a
  text changes nothing: the turn is over, so the question arrived after it
  was answered.
- `{"text": "", "clears": "<question>"}` clears the question named: the
  server cleans `clears` like the text, and only when the stored
  `blocked_on` equals it does it clear `blocked_on` and, when blocked, set
  `agent_state` to `working`. Otherwise it changes nothing, so a late clear
  never ends a newer question. `text` and `clears` both set is `400`.
- `{"text": ""}` without `clears` (an older mod) clears `blocked_on` and,
  when `agent_state` is `blocked` and `blocked_on` was set (the mod's own
  block), sets it to `working`; any other state stays, so a clear that
  arrives after the turn ended, or after a permission prompt blocked the
  session again, does not undo it.
- Each `blocked-on` call that changes the session records a `blocked_on`
  event (source `mod`, payload `text`, the first 200 characters) and sets
  `last_seen_at`. Like an upsert, it leaves `state_ts` alone.
- Any change of `agent_state` to a state other than `blocked`, from a
  `state_changed` event or an upsert, clears `blocked_on`. The hooks' prompt
  (`working`) and Stop (`idle`) are the usual ones.
- `PUT /v1/sessions/{id}/usage` with `{"context_percent": 42, "cost_usd":
  1.25}` stores the context window fill (an integer, 0 to 100) and the
  session's cost in US dollars (0 to 1,000,000), and sets `usage_at` and
  `last_seen_at`. A missing field keeps the stored value; a body with
  neither is `400`.
- `GET /v1/sessions/{id}/messages/next?wait=25` holds the request open for
  up to `wait` seconds (1 to 30, default 30) and answers `200` with
  `{"id": "msg_...", "text": "..."}` once the session has a message, or `204`.
  `text` is the prompt to submit, with the sender line the watcher would
  type. The poll sets the session's `mod_seen_at` before it waits, and again
  on every claim attempt, which makes the session messageable, with or
  without herdr, for 2 minutes. The session's messages go out in order, one
  at a time; a claimed message is offered again 15 seconds after its last
  offer until a result closes it. A send to the
  session wakes the poll. A session holds at most 2 open polls and a machine
  64; more is `429`. A `HEAD` is `405`.
- `POST /v1/messages/{id}/result` with `{"state": "delivered", "detail":
  "..."}` records the mod's result: `delivered`, `busy` (the message stays
  queued and is offered again 15 seconds after its last offer), or
  `refused`. `failed` is `400`. The server cleans `detail` to one line of at
  most 200 characters. A message that is not queued any more is `409`.

`Session` carries what the mod reported: `blocked_on`, `context_percent`,
`live_cost_usd`, and `usage_at` (each left out until a mod reports it), and
`messageable`. `controllable` keeps meaning herdr control: Remote Control and
moves still need herdr and a polling watcher.

### Inbox

`GET /v1/inbox` lists the sessions that need you, across machines. Each
session that has not ended is in at most one group, the first that matches:

| Group | When | `since` |
|---|---|---|
| `blocked` | `agent_state` is `blocked`. | When the session became blocked (`blocked_at`), else `state_ts`, else `started_at`. |
| `waiting` | The latest report has `waiting_on` items, and no prompt is later than the report. | The report's `ts`. |
| `finished` | `agent_state` is `idle` or `done`, `turn_ended_at` is set, and no prompt is later than it. | `turn_ended_at`. |

- The response is `{"items": [...], "counts": {"blocked": n, "waiting": n,
  "finished": n}}`, sorted by group, then `since`, then session ID. Each item
  has `group`, `since`, `waiting_on` (for `waiting` only), and `session`, the
  same object as in `GET /v1/sessions`. The counts leave out hidden items.
- Ended sessions never appear. Stale ones do, with `session.status` set to
  `stale`.
- An item leaves on its own when its condition stops holding: the session
  stops being blocked, a later report has no `waiting_on`, or a new prompt
  arrives.
- A **Blocked** item carries `permission`, the session's newest open
  permission request, when it has one. See "Permission requests".
- A **Blocked** item's `session.blocked_on` is the question the session's
  mod reported, when it has one. The dashboard, `sessionhub inbox`, and the
  Telegram alert show it in place of the recap, and next to the permission
  request when the item has both.
- When the agent state moves from `done` to `idle` (herdr marks a finished
  pane seen), the server dismisses that session's **Finished** item at its
  `since`, in the same write, on both the `state_changed` event and the herdr
  snapshot. A **Blocked** or **Waiting on you** item is never dismissed this
  way. Hooks-only sessions go from `working` straight to `idle`, so you
  dismiss their items yourself. A `state_changed` `idle` from Claude's Stop
  hook (source `hooks`) does not replace a stored `done`, even when it
  arrives after herdr's `done`; the server stores the event and keeps `done`,
  so herdr's later `done` to `idle` still dismisses the item.

`POST /v1/inbox/{id}/dismiss` with `{"since": "..."}` hides an item.
`POST /v1/inbox/{id}/snooze` with `{"since": "...", "until": "..."}` hides it
until `until`. Both answer `204` and keep one row per session in
`inbox_triage`, shared by every viewer.

- An item stays hidden while its `since` is not later than the stored one.
  Blocking again, a new `waiting_on` report, or another finished turn gives a
  later `since`, and the item comes back. A snooze also ends at `until`.
- Send the `since` you showed, exactly as the server sent it. A trigger that
  arrived after you loaded the list has a later `since`, so a dismiss never
  hides something you haven't seen.
- `since` is required and at most 5 minutes ahead of the server clock.
  `until` must be after now and at most 7 days ahead. Otherwise `400`. A
  dismiss ignores `until`; a snooze without it is `400`.
  The server validates `since` and `until` before it looks up the session, so
  a bad body for an unknown session gets `400`, not `404`.
- `{id}` is a full session ID. An unknown session is `404`. A session that is
  not in the inbox is stored anyway.
- A triage write records no event and leaves `last_seen_at` alone.

### Tasks

A task is a unit of work you would name in a standup. Its state is
`proposed`, `todo`, `in_progress`, `done_proposed`, `done`, or `dropped`.
`source` is `ticket`, `email`, `chat`, or `other`. A title is up to 200
characters, a `ref` up to 200, a `ref_url` an `http` or `https` URL of up to
2,000 bytes, and a note up to 500. An ID is `t_` and 11 URL-safe base64
characters; a client can supply one in `TaskIn.id`, and a repeat returns
the stored task with `200`.

Schema version 13 adds `tasks`, `task_events`, `task_sessions`, and
`task_session_ignores`. Every state change writes the `tasks` row and one
`task_events` row in the same transaction. The day view replays those events.

**Who acts.** The server decides the actor from the credentials:

- A dashboard cookie is you (`web:<name>`). Every write needs
  `X-Hub-Action: tasks`.
- A machine token with `session_id` in the body is that session, as an agent
  (`session:<id>`). The session must belong to the calling machine, or the
  call is `409`.
- A machine token without `session_id` is you, as `machine:<name>`. The CLI
  uses this. `merge`, `PATCH`, and `review/ignore` take no actor session, so
  they always act as you; the `session_id` in a `review/ignore` body is the
  session to hide.

**Agent rights.** An agent can create a `proposed` task, link a session
(which starts a `todo` task), and move a `todo` or `in_progress` task to
`done_proposed`. It cannot do anything else. Linking a `proposed` task
links the session and leaves the state alone.

**Creating.** An agent's task is always `proposed`. You choose `todo` (the
default) or `in_progress`. When an open task has the same non-empty `ref`
(ignoring case), an agent's create links its session to that task and
returns it with `200`; the same create from you is `409`. If that agent
create carries an `id`, the server also stores it as a `dropped` task with
`merged_into` set to the open task, so the ID the agent holds still
resolves. A non-empty `session_id` is linked for any actor.

**Transitions.** The server refuses any other move with `409`.

| From | To | Who |
|---|---|---|
| `proposed` | `todo`, `in_progress`, `dropped` | You |
| `todo` | `in_progress`, `done`, `dropped` | You |
| `in_progress` | `todo`, `done`, `dropped` | You |
| `todo`, `in_progress` | `done_proposed` | An agent |
| `done_proposed` | `done`, `in_progress`, `dropped` | You |
| `done`, `dropped` | `todo`, `in_progress` | You |
| `done` | `dropped` | You |
| `dropped` | `done` | You |

You cannot move a task merged into another. An agent's state change or
link on a merged task acts on its merge target instead, one hop only. A
merge drops the source task (any state but `dropped`),
sets `merged_into`, and moves its session links and spans to the target,
which can't be `proposed` or `dropped`.

**Day view.** `TaskDay` has the `date`, the `tz`, and three columns. The day
runs from 00:00 to 24:00 in `tz`; today ends now, and a later day is empty.

| Column | Rule |
|---|---|
| `done` | The task has an event to `done` during the day and is `done` at the end of it. |
| `in_progress` | The task was `in_progress` or `done_proposed` at any moment of the day, and is not in `done`. |
| `todo` | The task is `todo` at the end of the day. |

A task appears in the first column that matches. `proposed` and `dropped`
tasks appear in none. Each task carries its linked sessions and the `done`
items those sessions reported during the day.

**Review.** `TaskReview` lists `proposed` and `done_proposed` tasks, oldest
first, and the sessions with no task: sessions with a prompt in the last 7
days and a title, linked to no task and not ignored.

**Offline replay.** Agent calls fail open into the client's offline queue
(`docs/client.md`). A replay is idempotent: a create repeats a client task
ID, and a state or link call repeats an event ID, so it changes nothing
the second time.

### Telegram alerts

When `telegram_bot_token` and `telegram_chat_id` are both set, the server
sends a Telegram message for each new **Blocked** or **Waiting on you** inbox
item. **Finished** items never alert. When either is
empty, alerts are off, and the server logs one line at startup saying so.
If `telegram_chat_id` or `telegram_bot_token` has the wrong type (an array, a
float, a boolean), alerts are off and the line says why, for example
`telegram alerts are off: telegram_chat_id must be a string or an integer`.
The server still starts. `SESSIONHUB_TELEGRAM_CHAT_ID` and `SESSIONHUB_TELEGRAM_BOT_TOKEN`
override the file, and a valid override clears the problem.

Every 15 seconds the server reads the inbox, the same list as
`GET /v1/inbox`, and sends one message for each **Blocked** or **Waiting on
you** item that:

- is at least 30 seconds old, measured from its `since`, so a prompt you
  answer at once never alerts,
- is not dismissed or snoozed, and
- has a `since` later than the last alert for that session. Blocking again,
  or sending a new report, gives a later `since`, so it alerts again. A
  session that moves between **Blocked** and **Waiting on you** also gets a
  new `since` and alerts again.

The message is plain text. For a **Blocked** item:

```
⏸ Blocked: <title>
<machine> · blocked <age> ago
Asks to use <tool>: <command or file, cut to 300 characters>
<what it waits on, or the recap, cut to 300 characters>
```

For a **Blocked** item, the `Asks to use` line is there when the session has
an open permission request (see [Permission requests](#permission-requests)),
and ends in ` (cut)` when the hook cut the input.
The last line is the question the session's mod reported (`blocked_on`) when
there is one, else the latest report's `waiting_on` items when you haven't
sent a prompt since that report, else the recap; it is left out when all are
empty. An alert goes out once per blocked `since`, so a question the mod
reports after the alert for that block went out does not send another. Answer the request with **Allow once** or **Deny** in the inbox that
**Open inbox** opens; the bot itself never answers. For a **Waiting on you** item:

```
💬 Waiting on you: <title>
<machine> · waiting <age> ago
<the report's waiting_on items, joined with "; ">
```

Under the text is an **Open inbox** button
(`<public_url>/#inbox`) and, when the session has an `https` Remote Control
link, a **Remote Control** button. Every string from a session is cleaned
first.

- The server only calls `sendMessage`. It never reads the bot's updates, so
  it can share a bot with another program that does.
- A new rule sends its own message, so you notice a rule that a session
  planted. See [Rule-added alerts](#rule-added-alerts).
- `inbox_alerts` records each alert, and the row is written only after
  Telegram answers `ok: true`. A failed send records nothing, and a later
  tick retries it. A network error or HTTP 429 ends the tick; after HTTP 429
  the server waits `retry_after` seconds (30 without it) before it sends
  again. Any other refusal (for example HTTP 400 for one message) is logged,
  and the tick goes on with the next item.
- On the first tick after the server starts, a **Blocked** or **Waiting on you**
  item that is more than 1 hour old is recorded as alerted without a message, so
  a restart or deploy does not send a burst of stale alerts.
- At most 10 messages a tick and one a session. A failure is logged at most
  once a minute, without the token or the request URL.

#### Rule-added alerts

When alerts are on, `POST /v1/instructions` also sends one message after it
stores a rule. It makes no difference who adds the rule: a machine token
(including the MCP `remember` tool and `sessionhub rules add`) or the dashboard.

```
📌 New rule for all sessions
<the rule, cut to 300 characters>
added by <machine or web:session name> · rule <id>
```

Under the text is an **Open rules** button (`<public_url>/#rules`). Text from
a session is cleaned first.

- The request never waits for Telegram. The handler queues the message (up
  to 16) and the notifier sends it. If the queue is full, the server drops the
  alert and logs one line; the rule is still stored.
- The server makes one attempt per rule and keeps no retry queue. A failure is
  logged like any other alert failure, without the token. After HTTP 429, the
  server sends no rule alert until `retry_after` has passed.
- A rule alert is not recorded in `inbox_alerts`, and the 30-second grace
  and the 10-messages-per-tick limit do not apply.
- When alerts are off, the server sends nothing.

### Query parameters and IDs

- `GET /v1/sessions?live=true` returns only `live` and `blocked` sessions.
  `?machine=<name>` returns one machine's sessions; an unknown name returns
  `[]`.
- `GET /v1/sessions/{id}` accepts a unique prefix of at least 4 characters. A
  shorter prefix returns `400`, no match `404`, and more than one match `409`
  with the candidates:
  `{"error": "...", "candidates": ["3f2a...", "3f2a..."]}`. An exact ID wins
  over a longer ID it prefixes.
- Write endpoints need the full ID.
- A session ID is 1 to 128 letters, digits, `_`, or `-`, starting with a letter
  or digit.

### Limits

- A request body over 64 KiB returns `413`, except a move bundle
  (`PUT /v1/moves/{id}/bundle`), which may be 64 MiB. Uploads and downloads
  of a bundle may take 10 minutes; every other request has 30 seconds.
- A digest body over 16 KiB (`MaxDigestBytes`) returns `413`.
- A report list (`done`, `in_flight`, `waiting_on`) holds at most 20 items of
  at most 200 characters each; more returns `400`.

### Field validation

The server rejects a bad value with `400` and an error that names the field.
It stores nothing from a rejected request, including a herdr snapshot with one
bad entry.

| Field | Rule |
|---|---|
| Session ID | `^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$` |
| `ssh_host`, `herdr_host` | `^[A-Za-z0-9][A-Za-z0-9@._:-]{0,254}$`: no leading `-`, no `/`, `[`, or `]`. `sessionhub machine add` applies the same rule. |
| `agent`, `herdr_session`, `herdr_workspace`, `herdr_pane` | `^[A-Za-z0-9][A-Za-z0-9:._-]{0,63}$`. Empty is allowed and means unknown. |
| `title`, `title_hint`, `first_prompt` | At most 200 characters, no control characters. |
| Report items | At most 200 characters each, no control characters. |
| `note` | At most 2000 characters, no control characters. |
| `cwd` | At most 4096 bytes, no control characters. |
| Result `url` | `^https://claude\.ai/code/session_[A-Za-z0-9_-]+$`, at most 200 bytes, on `done` only. |
| Result `detail` | At most 200 characters, no control characters. |
| Control request ID | `^cr_[A-Za-z0-9_-]{22}$` |
| Move ID | `^mv_[A-Za-z0-9_-]{22}$` |
| Move `target` | a machine name, or `cloud` |
| `public_key` | standard base64 of exactly 32 bytes |
| Move result `detail` | At most 300 characters, no control characters. |
| Move result `cloud_url` | `^https://claude\.ai/code/session_[A-Za-z0-9_-]+$`, on a cloud move's `done` only, where it is required. |

A control character is anything in C0 (including newline and tab), C1, or DEL.

## Database

The database is SQLite in WAL mode, with `busy_timeout` 5000 ms and foreign
keys on. The server uses one connection, and every transaction starts with
`BEGIN IMMEDIATE` (`_txlock=immediate`), so a second process writing to the
same file, such as `sessionhub machine add`, waits instead of failing with
`SQLITE_BUSY`. The schema version is in
`PRAGMA user_version`; the server creates the schema on first open, upgrades an
older one in place (version 2 adds `sessions.state_ts`; version 3 adds `control_requests`, `machines.last_poll`, and `sessions.rc_url` and `rc_at`; version 4 adds `session_digests` and `sessions.last_prompt` and `last_prompt_at`; version 5 adds `login_codes` and `web_sessions`; version 6 adds `sessions.turn_ended_at`, `sessions.blocked_at`, and `inbox_triage`; version 7 adds `inbox_alerts`; version 8 adds `instructions`, `messages`, and `permission_requests`; version 9 adds `moves` and `machines.move_key`; version 10 adds `start_requests`; version 11 adds `start_requests.trust`; version 12 adds the mod's `sessions` columns), and refuses
a database from a newer version. Times are stored as UTC RFC 3339 with a
fixed nine-digit fraction, so they also sort as text. The database directory
is created with mode `0700`.

### Session digests

Schema version 4 adds the `session_digests` table and two `sessions` columns.

- `session_digests` holds one row per session: `as_of` (the digest's last
  transcript entry), `received_at`, and `body` (the validated `DigestIn` as
  JSON). The row is deleted with its session.
- `sessions.last_prompt` and `last_prompt_at` hold the newest `prompt`
  event's text and time. The upgrade fills them once from existing prompt
  events and skips a malformed payload. After that, `AddEvent` updates them,
  and a prompt event that is older than the stored time leaves them alone.
- `sessions.title_source` also takes `claude`, the automatic title from a
  digest. It ranks above `prompt` and below `herdr` and `user`. A digest's
  `custom_title` (from `/rename`) counts as `user`, but only when it differs
  from the previous digest's, so a later `set_title` keeps winning. A session's
  first digest never replaces a title the user already set.

If a digest has an `as_of` older than the stored one, the store keeps the
stored digest and reports `stored=false`. If the `as_of` is equal, the store
replaces the digest, so the hook child and the watcher can both send one. A
digest records a `digest` event only when the recap or the links change.

A digest never changes `last_seen_at` or `ended_at`, so a backfilled digest
doesn't revive an ended session.

To roll back to the v3 binary, restore the pre-deploy backup, or run
`PRAGMA user_version=3`. The v3 binary ignores the extra table and columns.
Run `PRAGMA user_version=4` before you upgrade again.

### Web sign-in

Schema version 5 adds two tables. Both hold hashes only: the database never
holds a sign-in code or a session token.

- `login_codes`: one row per `sessionhub login` link. `code_hash` (SHA-256 hex of
  the code), the session `name`, the creating `machine_id`, `created_at`,
  `expires_at` (10 minutes later), and `used_at` (null until used).
- `web_sessions`: one row per signed-in browser. `id` (8 random bytes, hex,
  public), `token_hash` (SHA-256 hex of the cookie token, unique), `name`
  (unique), `machine_id`, `created_at`, `last_used_at`, and `expires_at`
  (30 days after `last_used_at`).

Expired codes and sessions are deleted whenever a code or session is created
and whenever sessions are listed. Removing a machine deletes the codes and
sessions it created.

To roll back to the v4 binary, restore the pre-deploy backup. The v4 binary
needs `read_token` in `server.toml` again.

### Inbox triage

Schema version 6 adds two columns and one table for the inbox.

- `sessions.turn_ended_at`: when the agent last stopped working. It is set
  when the agent state moves from `working` or `blocked` to `idle` or `done`:
  to the event's `ts` on a `state_changed` event that is applied, and to the server's time on
  an upsert or herdr snapshot. A move between `idle` and `done`, or to the
  same state, leaves it alone. The upgrade does not backfill it, so an idle
  session enters the inbox only after its next turn.
- `sessions.blocked_at`: when the agent state last moved into `blocked` from
  any other value. It is set to the event's `ts` when a `state_changed` event
  is applied, and to the server's time on an upsert, herdr snapshot, or
  insert. Repeating `blocked` or leaving it leaves the column alone, and the
  inbox reads it only while the session is blocked. The upgrade does not
  backfill it.
- `inbox_triage`: one row per session. `triaged_since` (the item's `since`
  when it was dismissed or snoozed), `snooze_until` (null for a dismiss), and
  `updated_at`. The row is deleted with its session.

To roll back to the v5 binary, restore the pre-deploy backup. The v5 binary
refuses a version 6 database.

### Inbox alerts

Schema version 7 adds `inbox_alerts`, one row per session: `since` (the
item's `since` that the last Telegram alert was for) and `sent_at`.
The notifier writes the row only after Telegram accepts the message. The row
is deleted with its session.

To roll back to the v6 binary, restore the pre-deploy backup. The v6 binary
refuses a version 7 database.

### Shared instructions, messages, and permission requests

Schema version 8 adds three tables:

- `instructions`: one row per standing rule, with `text`, `created_at`, and
  `created_by` (a machine name, or `web:<session name>`).
- `messages`: one row per queued target of a send, with the text as sent,
  the `sender`, the `state` (`queued`, `delivered`, `expired`, `refused`), a
  `detail` from the watcher, and `offered_at`, the last time a watcher was
  handed it.
- `permission_requests`: one row per permission prompt a hook posted, with
  the tool name and input (compact JSON, at most 8 KiB), the `state` (`open`,
  `decided`, `expired`, `answered_locally`, `closed`), and the decision,
  reason, and who decided.

Messages and permission requests are deleted with their session or machine.

Indexes serve the hot paths: `messages(machine_id, state, created_at)` for a
watcher's poll, `messages(session_id, state, created_at)` for the
one-in-flight-per-session check, and `messages(sender, created_at)` for the
rate limit. `permission_requests(session_id, state, created_at)` serves the
local-answer close and the inbox.

To roll back to the v7 binary, restore the pre-deploy backup. The v7 binary
refuses a version 8 database.

### Moves

Schema version 9 adds `machines.move_key` (the machine's X25519 public key
in standard base64, or empty until its watcher registers one) and `moves`:

| Column | Holds |
|---|---|
| `id` | `mv_` and 16 random bytes in base64url |
| `session_id` | the session (`ON DELETE CASCADE`) |
| `source_machine_id` | the machine the session moves from (`ON DELETE CASCADE`) |
| `target` | the target machine's name, or `cloud` |
| `target_machine_id` | the target machine (`ON DELETE CASCADE`); `NULL` for `cloud` |
| `state` | `requested`, `packing`, `uploaded`, `unpacking`, `done`, `failed`, or `cancelled` (defined, never set yet) |
| `detail` | why it failed, as `<step>: <reason>`, or what the target did not carry; then any notes the source added after the move ended |
| `created_at`, `updated_at` | times; the timeouts count from `updated_at` |
| `bundle_size` | the sealed bundle's size in bytes |
| `cloud_url` | the cloud session link of a cloud move |
| `requested_by` | `web:<name>` or `machine:<name>` (`BY` is an SQL keyword) |
| `source_closed_at` | when the source finished its part; `NULL` while it still owes a finish |

Bundles are files, never rows (see "Moves" under the API). The version 8
binary refuses a version 9 database; restoring the backup is the only
rollback.

### Mod columns

Schema version 12 adds five `sessions` columns for the mod:

| Column | Holds |
|---|---|
| `blocked_on` | what the session waits on: one cleaned line, at most 300 characters; `''` when nothing |
| `context_percent` | the context window fill, 0 to 100; `NULL` until reported |
| `usage_at` | when the mod last reported usage |
| `live_cost_usd` | the session's cost in US dollars; `NULL` until reported |
| `mod_seen_at` | the mod's last message poll; within 2 minutes, the mod takes the session's messages |

The version 11 binary refuses a version 12 database; restoring the backup is
the only rollback.

The source is in `internal/store` (schema, queries, status) and
`internal/server` (HTTP, auth, config, commands).
