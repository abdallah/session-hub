# Inbox design

Date: 2026-10-01. Status: recommendations approved in chat; the user asked
for full implementation.

## Goal

One list, across machines, of the sessions that need you: blocked on a
prompt, waiting on you, or finished a turn you haven't looked at. You open
the session from the list, or dismiss or snooze the item. Modeled on the
tuios inbox. v1 is view and triage only.

## Decisions

| Topic | Decision |
|---|---|
| Groups | **Blocked**, **Waiting on you**, **Finished**, in that order, oldest item first in each. A session appears once, in its highest group. |
| Ended sessions | Leave the inbox. |
| Stale sessions | Stay, marked **stale**. |
| Rows | Title, machine, group and age, the `waiting_on` items or the recap, the last prompt, and the summary line. |
| Actions | **Open** (the Remote Control link or button, and a copy button for `sessionhub resume`), **Dismiss**, **Snooze** (1 hour, 4 hours, until 9:00 tomorrow). |
| Triage state | On the server, one row per session, shared by all viewers in v1. |
| Where | A dashboard **Inbox** tab and `sessionhub inbox`. The herdr sidebar is unchanged. |
| Out of scope | Answering prompts or typing into sessions, push notifications, per-person triage, the herdr sidebar. |

## When a session is in the inbox

Each item has a **group** and a **since** time, the trigger time.

| Group | Condition | `since` |
|---|---|---|
| Blocked | `agent_state = 'blocked'` | `blocked_at` (when it became blocked), else `state_ts`, else `started_at` |
| Waiting on you | The latest report has a non-empty `waiting_on`, and no prompt arrived after that report (`last_prompt_at` is null or not later than the report's `ts`). | The report's `ts` |
| Finished | `agent_state` is `idle` or `done`, `turn_ended_at` is set, and no prompt arrived after it (`last_prompt_at` is null or not later than `turn_ended_at`). | `turn_ended_at` |

- A session that matches several groups appears only in the first one in
  the table's order.
- Sessions with `ended_at` set are never in the inbox.
- An item clears itself when its condition stops holding: you answer the
  prompt, a later report has no `waiting_on`, or you send a new prompt.

### `turn_ended_at`

A new `sessions` column, set to the state change's time when the agent state
moves from `working` or `blocked` to `idle` or `done`. It is set on both
paths that change the state: a `state_changed` event and a herdr snapshot
upsert. A move between `idle` and `done`, for example herdr marking a pane
seen, leaves it unchanged. A session that never worked has none, so a fresh
session is not **Finished**.

The upgrade does not backfill it: existing idle sessions enter the inbox only
after their next turn.

## Triage

Table `inbox_triage`:

| Column | Notes |
|---|---|
| `session_id` | Primary key, foreign key to `sessions`, `ON DELETE CASCADE`. |
| `triaged_since` | The item's `since` when it was triaged. |
| `snooze_until` | Null for a dismiss. |
| `updated_at` | When the row was written. |

An item is hidden when a triage row exists, the item's `since` is not later
than `triaged_since`, and either `snooze_until` is null (dismissed) or now is
before `snooze_until` (snoozed). So:

- Something new, such as blocking again, a new `waiting_on` report, or
  another finished turn, gives a later `since`, and the item comes back.
- A snooze ends at `snooze_until` even if nothing new happened.
- The client sends the `since` it showed. A trigger that arrived after the
  page loaded has a later `since`, so a dismiss never hides something you
  haven't seen.

## Server

Schema version 6 adds `sessions.turn_ended_at`, `sessions.blocked_at`, and `inbox_triage`.

| Route | Access | Does |
|---|---|---|
| `GET /v1/inbox` | Read: machine token or session cookie | `{"items": [...], "counts": {"blocked": n, "waiting": n, "finished": n}}`. Each item: `group` (`blocked`, `waiting`, `finished`), `since`, `waiting_on` (for `waiting`), and `session` (the same object as in `GET /v1/sessions`, with recap, last prompt, summary, status, and Remote Control fields). Sorted by group, then `since` ascending. |
| `POST /v1/inbox/{id}/dismiss` `{"since": "..."}` | Machine token, or session cookie with `X-Hub-Action: triage` | Upserts the triage row with `snooze_until` null. `204`. |
| `POST /v1/inbox/{id}/snooze` `{"since": "...", "until": "..."}` | Same | Upserts with `snooze_until`. `until` must be after now and at most 7 days ahead. `204`. |

- `since` is required, must parse as a time, and must not be more than 5
  minutes in the future. An unknown session is `404`.
- Triage writes record no event and do not change `last_seen_at`.
- The auth table covers the three routes. The session cookie without the
  `triage` header value gets `403`.

## CLI

- `sessionhub inbox [--json]`: one line per item, grouped with a heading per group:
  ID prefix (8 characters), title, machine, age of `since`, and the
  `waiting_on` items or the recap, cut to the terminal width. `--json` prints
  the server response.
- `sessionhub inbox dismiss <id|prefix>`.
- `sessionhub inbox snooze <id|prefix> <1h|4h|tomorrow|duration>`: `tomorrow` means
  9:00 local time the next day. A Go duration such as `90m` also works, up to
  7 days.
- Both resolve the prefix against the current inbox and send that item's
  `since`. If the session isn't in the inbox, they fail with "not in the
  inbox".

## Dashboard

- Tabs **Inbox** and **Sessions** at the top. **Inbox** is selected on load
  when it has items. Its tab label shows the count, and the page title
  becomes `(3) sessionhub`.
- The inbox polls with the session list, on the same timer.
- Each row: title, machine, group badge and age, the `waiting_on` items or
  the recap, the last prompt, the summary line, and the buttons **Open**,
  **Copy resume**, **Dismiss**, and **Snooze** (a small menu: 1 hour,
  4 hours, 9:00 tomorrow). **Open** behaves like the card's Remote Control
  control: the link when one is active, else the button.
- **Dismiss** and **Snooze** remove the row at once and send the request
  with `X-Hub-Action: triage`. On failure the row comes back with an error.
- Every string goes in with `textContent`.
- The Remote Control and resume code paths are shared with the session cards,
  not copied.

## Testing

- **Store:** v5 to v6 upgrade; `turn_ended_at` set on working to idle and
  done, and on blocked to idle, through both events and snapshot upserts, and
  not on idle to done; each group's condition, including a newer prompt
  clearing **Waiting** and **Finished**; group precedence; ended sessions
  excluded; dismiss hides until a later `since`; snooze hides until
  `snooze_until`; cascade delete.
- **Server:** auth table for the three routes; validation of `since` and
  `until`; `404` for an unknown session; the response shape and order.
- **CLI:** grouping and output; prefix resolution; `tomorrow` computes 9:00
  local the next day; not-in-inbox error.
- **Dashboard:** pure functions under node: grouping and counts, the title
  string, snooze times for 1 hour, 4 hours, and tomorrow, and the triage
  request with its header.
- **Live:** after deploy, finish a turn in a scratch session and see it under
  **Finished** within a minute; dismiss it; prompt again and see it come
  back after the next turn.
