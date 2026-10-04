# Shared instructions, messages to sessions, and remote permission answers

Date: 2026-10-02. Status: approved in chat; the user asked for the spec,
plan, and implementation.

## Goal

Three ways for sessionhub to act on your sessions, not only show them:

1. **Shared instructions.** One short list of standing rules that every
   session on every machine sees, at start and again with every prompt, so
   rules don't fade in long, busy sessions.
2. **Messages to sessions.** Send one message to one or several sessions,
   from the dashboard, the CLI, or another session.
3. **Remote permission answers.** Allow or deny a pending permission prompt
   from the inbox, Telegram (by link), or the CLI.

The inbox's earlier "view and triage only" scope is lifted for these three,
with the safety rules below.

## Facts this rests on

From the Claude Code hooks docs (Claude Code 2.1.287) and a test on `bluebox`
on 2026-10-02:

- `PermissionRequest` hooks receive `tool_name`, `tool_input`, and optional
  `permission_suggestions`. They answer with
  `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`
  or `"deny"` with `message`. Exit 0 with no output means no decision; the
  normal dialog applies. The default command-hook timeout is 600 seconds.
- **Tested:** while a `PermissionRequest` hook waits, the normal permission
  dialog is shown in the terminal. Answering in the terminal 5 seconds into
  the hook's wait ran the command 0.1 seconds later; the hook kept running
  and its result was not used. A hook that printed `allow` after 6 seconds
  ran the command with no terminal input.
- `SessionStart` and `UserPromptSubmit` hooks add context with
  `hookSpecificOutput.additionalContext` (up to 10,000 characters). The
  `UserPromptSubmit` hook has a 30-second default timeout, and on timeout
  its output is discarded.
- herdr's `agent prompt <pane> <text>` submits a prompt to the agent in a
  pane. It rejects the submission when the agent is blocked.
- Remote Control already forwards permission prompts to the Claude app.
  sessionhub's answers cover sessions without Remote Control, in one place across
  machines.

## 1. Shared instructions

### Storage and API

Schema version 8 adds `instructions`: `id` (integer primary key), `text`,
`created_at`, `created_by` (machine name, or `web:<session name>`).

- Each rule is 1 to 300 characters of free text (the existing free-text
  rules). The whole list is at most 2,000 characters; adding past that is
  `409` with a message.
- `GET /v1/instructions`: read access. Returns `{"instructions":[{id, text,
  created_at, created_by}], "version": "<hash>"}`, oldest first. `version`
  is a hash of the list, so clients can skip unchanged copies.
- `POST /v1/instructions` `{"text": "..."}`: machine token, or the session
  cookie with `X-Hub-Action: instructions`. Returns the new rule.
- `DELETE /v1/instructions/{id}`: the same access. `404` for an unknown ID.

### Delivery to sessions

- Local copy: `~/.local/state/sessionhub/instructions.json` (the list and its
  version). The herdr watcher refreshes it on each heartbeat; `sessionhub hook
  session-start` refreshes it in a detached child, like the flush.
- `sessionhub hook session-start` prints `additionalContext` with the full list:

  ```
  Standing instructions from sessionhub (apply in every session):
  - <rule>
  - <rule>
  ```

- `sessionhub hook prompt` prints the same block, so every prompt carries it. It
  reads only the local copy and never calls the server, so it adds no
  network wait to a prompt.
- An empty list adds nothing.

### Editing

- MCP tools: `remember(text)` adds a rule; `forget(id)` removes one;
  `instructions()` lists them. Tool descriptions tell Claude to use
  `remember` only when the user asks for something to apply to all
  sessions.
- CLI: `sessionhub rules ls`, `sessionhub rules add "<text>"`, `sessionhub rules rm <id>`.
- Dashboard: a **Rules** tab lists the rules with a delete button each and
  an add box.

## 2. Messages to sessions

### Sending

- `POST /v1/messages` `{"session_ids": [...], "text": "..."}`: machine
  token, or the session cookie with `X-Hub-Action: send`. 1 to 20 sessions,
  text 1 to 4,000 characters. Returns one message ID per session.
- Each target must be a live session in a herdr pane on a machine whose
  watcher is polling (the existing "controllable" rule); otherwise that
  target is refused in the response, and the others still go.
- The delivered text is prefixed with `From the user via sessionhub (<sender>):`
  and a blank line, where `<sender>` is `dashboard`, `cli on <machine>`, or
  `session <short id>`.
- At most 30 messages a minute per sender; more is `429`.

### Delivery

- Stored in a new table `messages`: `id`, `session_id`, `text`, `sender`,
  `created_at`, `state` (`queued`, `delivered`, `expired`, `refused`),
  `updated_at`, `detail`.
- The session's machine's watcher receives the message through the existing
  long poll (`GET /v1/machines/self/control`), as a new request kind
  `message`, alongside Remote Control requests.
- The watcher delivers only when the pane's agent is `idle` or `done`, with
  `herdr agent prompt <pane> <text>`. If the agent is `working` or
  `blocked`, it reports `busy`, and the server offers the message again on
  a later poll. A message not delivered within 10 minutes becomes
  `expired`.
- The watcher reports `delivered` or `refused` (with herdr's reason) through
  the existing result route.
- Each delivered or expired message adds a `message` event to the target
  session, with sender and the first 200 characters.

### Interfaces

- Dashboard: each card gets a selection checkbox (in the expanded card and
  as a "Select" mode on the Sessions tab). With sessions selected, a
  **Send message** bar appears: a text box, the target list, and **Send**,
  which asks for confirmation. Results show per session.
- CLI: `sessionhub send <id-or-prefix>... -m "<text>"`, and `sessionhub send --machine M
  -m "<text>"` for every live controllable session on M. It prints each
  target's result and waits up to 15 seconds for delivery results.
- MCP tool: `send_to_sessions(session_ids, text)`.

## 3. Remote permission answers

### Requests

- sessionhub installs a `PermissionRequest` hook (all tools) with `"timeout": 660`.
  `sessionhub hook permission-request` sends the request to the server and waits
  for a decision.
- `POST /v1/sessions/{id}/permissions` (machine token, owning machine):
  `{"tool_name", "tool_input", "cwd", "suggestions"}` → `{"id": "pr_..."}`.
  `tool_input` is stored as JSON, capped at 8 KB (bigger input is cut and
  marked). The session's inbox item shows the request.
- The hook then long-polls `GET /v1/permissions/{id}/decision?wait=30`
  (machine token, owning machine) in a loop for up to 10 minutes. On
  `allow`, it prints the allow decision. On `deny`, it prints the deny
  decision with the reason as `message`. On expiry, server error, or no
  server, it exits 0 with no output, so the normal dialog applies.
- When the session leaves `blocked` (an answer in the terminal), the server
  marks its open request `answered_locally`; the hook stops waiting and
  exits with no output.

### Decisions

- `POST /v1/permissions/{id}/decide` `{"decision": "allow"|"deny",
  "reason": "..."}`: machine token, or the session cookie with
  `X-Hub-Action: approve`. One decision per request; a second one is
  `409`. Expired or closed requests are `410`.
- Only "allow once" and "deny". No permanent rules from remote in this
  version; `updatedPermissions` is never sent.
- Each decision adds a `permission` event to the session: tool, the first
  200 characters of the input, decision, reason, and who decided.
- `sessionhub hook permission-request` honours `SESSIONHUB_REMOTE_PERMISSIONS=off` and
  the client config key `remote_permissions = false` by exiting at once
  with no output, so a machine can opt out.

### Where you answer

- **Inbox:** a Blocked item with an open request shows the tool and its
  input (a `Bash` command in `code`, other tools as their main field), and
  **Allow once** and **Deny** buttons. **Deny** offers an optional reason.
- **Telegram:** the Blocked message includes the tool and the first 300
  characters of the input, and the **Open inbox** button links to
  `<public_url>/#inbox`. The bot only uses link buttons.
- **CLI:** `sessionhub approve <request-or-session-prefix>` and `sessionhub deny
  <prefix> ["reason"]`; `sessionhub inbox` shows the request on blocked rows.

## Security

- Every write is a machine token, or a signed-in browser with the matching
  `X-Hub-Action` value (`instructions`, `send`, `approve`), as with Remote
  Control and triage.
- Request and message IDs are random and unguessable; decisions apply only
  to their own request, and only once.
- Standing instructions, messages, and permission inputs go through the
  existing free-text cleaning where shown in a terminal or the dashboard,
  and are inserted with `textContent`.
- Local `deny` rules in Claude Code still win over a remote allow.

## Testing

- Store: v7 to v8 upgrade; instruction limits; message state transitions
  and expiry; permission request lifecycle (open, decided, expired,
  answered locally); one decision only.
- Server: auth table for every new route (cookie needs the right action
  header); validation; rate limits; long-poll decision endpoint.
- Hooks: session-start and prompt print the block from the local copy and
  never call the server on the prompt path; the permission hook prints
  allow, deny, or nothing for each server answer, timeout, and outage, and
  honours the opt-out.
- Watcher: message delivery only when idle or done; `busy` retry; result
  reporting; uses the fake herdr.
- CLI and MCP: `sessionhub rules`, `sessionhub send`, `sessionhub approve`/`deny`, and the MCP
  tools against a test server.
- Dashboard: pure functions for the Rules tab, selection and send bar, and
  the permission block; string checks for the action headers.
- Live: add a rule and see it in a new session on each machine; send a
  message to two sessions; approve and deny a scratch session's permission
  prompt from the dashboard.

## Out of scope

- Remote "always allow" rules.
- Answering `AskUserQuestion` dialogs.
- Messages to sessions outside herdr.
- Telegram buttons that answer directly (they would need the bot's update
  feed, which Hermes owns).
