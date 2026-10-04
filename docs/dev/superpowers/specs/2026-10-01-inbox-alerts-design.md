# Inbox alerts and the herdr inbox pane

Date: 2026-10-01. Status: approved in chat; the user asked for implementation
without further questions.

## Goal

Make the inbox reach you instead of waiting to be opened, cut the noise in
**Finished**, and let you jump to any session from inside herdr.

## Decisions

| Topic | Decision |
|---|---|
| Blocked alerts | A Telegram message from the server, plus a herdr notification with a sound on the machine that runs the session. |
| Telegram bot | Reuse one of Hermes' bots. sessionhub only calls `sendMessage` and uses URL buttons, so it never reads updates and never competes with Hermes' `getUpdates`. |
| Finished noise | When herdr marks a pane seen (`done` to `idle`), the server dismisses that session's **Finished** item. |
| Sidebar | A display-only inbox count on the herdr workspaces, set by the watcher. |
| Inbox pane | `sessionhub inbox --watch`, opened as a herdr plugin pane. **Enter** jumps to the session, on this machine or another one. |
| Cross-machine jump | Focus the session's pane on the other machine over SSH, then focus or open a local herdr tab attached with `herdr --remote <herdr_host>`. |

## 1. Telegram alerts for Blocked and Waiting on you

Update, 2026-10-02: alerts also fire for **Waiting on you** items, with the
same rules. Their `since` is the report's timestamp, so a new report alerts
again. **Finished** items stay silent. The message for a **Waiting on you**
item starts with `💬 Waiting on you: <title>`, its second line reads
`<machine> · waiting <age> ago`, and its third line is the `waiting_on` items
joined with `; `. The text below describes **Blocked** items.

### Configuration

`server.toml` gains `telegram_bot_token` and `telegram_chat_id`, with the env
overrides `SESSIONHUB_TELEGRAM_BOT_TOKEN` and `SESSIONHUB_TELEGRAM_CHAT_ID`. When either is
empty, alerts are off and the server logs one line at startup saying so. The
token is never logged or returned by any endpoint.

### When a message is sent

A notifier goroutine in the server runs every 15 seconds. It computes the inbox
(the same store function as `GET /v1/inbox`) and, for each **Blocked** item:

- that has been blocked for at least 30 seconds (`now - since >= 30s`), so a
  prompt you answer at once never alerts, and
- that is not hidden by triage (the inbox already excludes those), and
- whose `since` is later than the last alert recorded for that session,

it sends one message and records it. A session that blocks again gets a new
`since`, so it alerts again.

New table (schema version 7) `inbox_alerts`: `session_id` (primary key,
foreign key, `ON DELETE CASCADE`), `since` (the `since` alerted on), and
`sent_at`. The row is written only after Telegram answers `ok: true`.

### The message

```
⏸ Blocked: <title>
<machine> · blocked <age> ago
<waiting_on items, or the recap, cut to 300 characters>
```

Plain text, no `parse_mode`. An inline keyboard of URL buttons: **Open inbox**
(`<public_url>/#inbox`) and, when the session has a Remote Control link,
**Remote Control** (`rc_url`). Every server-supplied string passes the existing
free-text cleaning before it is sent.

### Failures

- A failed send (network error, non-`ok` response, HTTP 429) records nothing,
  so the next tick retries. On HTTP 429 the notifier waits `retry_after`
  seconds before its next send.
- The notifier logs each failure at most once a minute, without the token or
  the request URL.
- One alert at most per session per tick, and at most 10 sends per tick.
- The notifier stops when the server stops.

### Dashboard

The dashboard selects the **Inbox** tab when the URL fragment is `#inbox`.

## 2. herdr notifications for Blocked

`sessionhub plugin event`, on a `pane.agent_status_changed` event whose new status is
`blocked`, runs:

```
herdr notification show "Blocked: <title>" --body "<machine>" --sound request
```

using `HERDR_BIN_PATH` when set. `<title>` is the pane's title, cleaned and cut
to 80 characters. A failure is logged to the watcher log and ignored. This runs
on the machine that runs the session, so you hear it in the herdr where that
session lives.

## 3. Auto-dismiss Finished when seen

When the agent state of a session moves from `done` to `idle` (herdr marks the
pane seen), the store checks the session's inbox classification at that
moment. If it is **Finished**, the store writes the triage row as a dismiss
with `triaged_since` = that item's `since` (`turn_ended_at`). It does this on
both state paths, the `state_changed` event and the snapshot upsert, in the
same transaction. A **Blocked** or **Waiting on you** item is never touched.

Hooks-only sessions go straight from `working` to `idle`, so they never
auto-dismiss; you dismiss them by hand.

## 4. Inbox count in the herdr sidebar

On every heartbeat (60 seconds) the watcher fetches the inbox counts and, for
each workspace in its herdr session, runs:

```
herdr workspace report-metadata --source sessionhub --token inbox=<text> <workspace>
```

`<text>` is `<total>` plus ` · <n> blocked` when any are blocked, for example
`3 · 1 blocked`. When the inbox is empty it runs `--clear-token inbox` instead.
It skips the call when the text is unchanged since the last heartbeat. If the
inbox fetch fails, the previous value stays. The exact flags follow `herdr
workspace report-metadata --help`; if herdr does not render workspace tokens
in the sidebar, the implementer uses the closest display-only metadata herdr
does render and records the choice.

## 5. The herdr inbox pane

### `sessionhub inbox --watch`

A full-screen, keyboard-driven list in the terminal, refreshed every 5
seconds:

- The same groups and lines as `sessionhub inbox`, with a cursor on one row.
- **j** / **k** or the arrow keys move; **Enter** jumps; **d** dismisses;
  **s** then **1**, **4**, or **t** snoozes for 1 hour, 4 hours, or until 9:00
  tomorrow; **r** refreshes now; **q** or **Ctrl+C** quits.
- A status line at the bottom shows the last refresh time and the last error.
- It uses the alternate screen and raw mode through `stty`, with no new
  dependency, and restores the terminal on exit, including on a signal.
- If stdin or stdout is not a terminal, it exits with an error.

### Jumping

**Enter** on a row:

- **Session on this machine:** the same logic as `sessionhub resume <id>` (focus the
  pane, or start the session in a pane), without printing the resume command.
- **Session on another machine:**
  1. Run `ssh -o BatchMode=yes <ssh_host> '~/.local/bin/sessionhub resume <uuid>'`
     with a 15-second limit, so the remote herdr focuses the session's pane
     (or starts it).
  2. Find a local herdr tab named `sessionhub:<machine>`. If one exists, focus it.
     Otherwise create a tab with that name running
     `herdr --remote <herdr_host>`, and focus it.
  3. If the machine has no `herdr_host`, or step 1 fails, show the resume
     command in the status line instead.

### Opening it

- New herdr plugin entrypoint `inbox` running `sessionhub inbox --watch`.
- `sessionhub plugin open-inbox` runs `herdr plugin pane open --plugin sessionhub
  --entrypoint inbox --placement split --direction right --focus`, using
  `HERDR_BIN_PATH` when set. You can bind it to a key like the existing picker.

## Testing

- **Store:** v6 to v7 upgrade; `inbox_alerts`; auto-dismiss on `done` to
  `idle` for **Finished** only, on both paths, and not for **Waiting** or
  **Blocked**; a later turn comes back after an auto-dismiss.
- **Notifier:** with a fake Telegram server: the 30-second grace; one message
  per block; a new block alerts again; dismissed and snoozed items don't alert;
  a failed send retries next tick; HTTP 429 honours `retry_after`; the token
  never appears in logs; message text and buttons; off when unconfigured.
- **Plugin:** the herdr notification command for a blocked status event; the
  sidebar text and the skip-when-unchanged rule, against the fake herdr used
  by the existing watcher tests.
- **CLI:** the watch view's rendering and key handling as pure functions
  (state in, keys in, state and actions out); the jump plan for local, remote,
  and remote without `herdr_host`.
- **Live:** set the Telegram values on `tower`, block a scratch session for
  over 30 seconds, and check the Telegram message and the herdr notification;
  view a finished pane in herdr and see the item leave the inbox; open the
  inbox pane and jump to a session on each machine.

## Out of scope

- Answering prompts from Telegram or the inbox pane.
- Alerts for **Waiting on you** or **Finished**.
- Telegram for more than one chat.
