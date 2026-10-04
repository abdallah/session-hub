# Shared instructions, messages to sessions, and remote permission answers implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let sessionhub act on your sessions: one short list of standing rules that
every session sees at start and with every prompt, messages you send to one or
several sessions, and **Allow once** or **Deny** answers to a pending
permission prompt from the inbox, the CLI, or Telegram (by link).

**Architecture:** Schema v8 adds `instructions`, `messages`, and
`permission_requests`. Rules live on the server; the herdr watcher (every
heartbeat) and `sessionhub hook session-start` (in a detached child) copy them to
`~/.local/state/sessionhub/instructions.json`, and a new synchronous hook entry,
`sessionhub hook context`, prints that local copy as `additionalContext` on
`SessionStart` and `UserPromptSubmit` without touching the network. Messages
ride the Remote Control long poll as a second request kind, `message`: the
watcher submits the text with herdr's `agent.prompt` only when the pane's agent
is `idle` or `done`, and otherwise reports `busy` so the server offers it again
later. A new `PermissionRequest` hook entry (`sessionhub hook permission-request`,
timeout 660) posts the request and long-polls for a decision; the inbox shows
the open request on the session's **Blocked** item, and a decision from the
dashboard, `sessionhub approve`, or `sessionhub deny` reaches the hook within a second. A
session that leaves `blocked` closes its open requests as answered locally.

**Tech Stack:** Go 1.25, `modernc.org/sqlite`, `github.com/BurntSushi/toml`,
standard library only. No new dependencies. herdr 0.9.3 (socket protocol 22).
Claude Code 2.1.287 hooks. Dashboard: one embedded HTML page, pure functions
tested under `node`.

**Spec:** `docs/dev/superpowers/specs/2026-10-02-session-actions-design.md`

## Global Constraints

- Schema version 8, upgraded in place like versions 2 to 7. It adds three
  tables:
  - `instructions`: `id` (integer primary key), `text`, `created_at`,
    `created_by`.
  - `messages`: `id` (`msg_` and 22 base64url characters), `session_id`
    (foreign key, `ON DELETE CASCADE`), `machine_id` (foreign key, `ON DELETE
    CASCADE`), `text` (as sent, without the prefix), `sender`, `state`
    (`queued`, `delivered`, `expired`, `refused`), `detail`, `created_at`,
    `updated_at`, `offered_at` (the last time a watcher was handed it).
  - `permission_requests`: `id` (`pr_` and 22 base64url characters),
    `session_id`, `machine_id` (both foreign keys, `ON DELETE CASCADE`),
    `tool_name`, `tool_input` (JSON text, at most 8 KiB), `truncated`,
    `state` (`open`, `decided`, `expired`, `answered_locally`, `closed`),
    `decision` (`allow`, `deny`, or empty), `reason`, `decided_by`,
    `created_at`, `expires_at`, `updated_at`, `decided_at`.
- Rules: each is 1 to 300 characters after trimming, with no control
  characters (the existing `checkText` rule). The whole list is at most 2,000
  characters; an add past that is `409`. `version` is the first 16 hex digits
  of a SHA-256 over every rule's ID and text, so an unchanged list has an
  unchanged version. `created_by` is the machine name for a machine token and
  `web:<session name>` for a browser.
- Context block, printed only when the list is not empty:

  ```
  Standing instructions from sessionhub (apply in every session):
  - <rule>
  - <rule>
  ```

  `sessionhub hook context` reads only `~/.local/state/sessionhub/instructions.json`. It
  never builds a sessionhub client, so the prompt path makes no network call.
- Messages: 1 to 20 distinct session IDs per send (full IDs; the CLI resolves
  prefixes first), text 1 to 4,000 characters after trimming, newlines and tabs
  allowed, every other control character refused. A target is queued only if
  the session exists, has not ended, has a herdr pane, and its machine's
  watcher polled in the last 2 minutes; otherwise its result is `refused` with
  a reason and the other targets still go. At most 30 messages (one per
  target) per sender in any 60 seconds; a send that would pass that is `429`
  as a whole.
- Sender: `dashboard` for a browser, `cli on <machine>` for a machine token,
  `session <first 8 characters>` when a machine token sends `from_session`
  (the MCP tool). The delivered prompt is
  `From the user via sessionhub (<sender>):` + a blank line + the text.
- Message lifetime: `queued` until a watcher reports `delivered` or `refused`,
  or 10 minutes pass (`expired`). A watcher's `busy` keeps it `queued` and
  stores the detail. A queued message is offered again 15 seconds after the
  last offer. `delivered` and `expired` each add a `message` event (source
  `server`) with `message_id`, `sender`, `state`, and the first 200
  characters of the text.
- The watcher delivers only when herdr reports the pane's agent `idle` or
  `done` and the pane runs the target session. `working` or `blocked` (or
  herdr's `agent_blocked` error) is `busy`. No pane, another session in the
  pane, or no agent is `refused`. A herdr socket that does not answer is
  `busy`.
- Permission requests: `POST /v1/sessions/{id}/permissions` (machine token,
  owning machine). `tool_input` is compacted JSON; over 8,192 bytes it is
  stored as a JSON string of its first 8,000 bytes (cut on a rune boundary)
  with `truncated` set. `cwd` and `suggestions` are accepted and not stored.
  A request is open for 10 minutes. `GET /v1/permissions/{id}/decision?wait=N`
  (owning machine; `N` is 1 to 30, default 30) answers `200` with the request
  once it is not open, else `204` after the wait. `POST
  /v1/permissions/{id}/decide` takes `allow` or `deny` with an optional
  reason (deny only; at most 200 characters, no control characters). A
  second decision is `409`; an expired, answered-locally, or closed request
  is `410`.
- Local answers: when a state change moves a session from `blocked` to any
  other state, its open requests created at or before that change become
  `answered_locally`. On the event path "at or before" uses the event's own
  time; on the snapshot path it uses the server time minus 5 seconds. Ending
  a session (`pane_closed`, `ended`, or missing from a snapshot) makes its
  open requests `closed`.
- Each decision adds a `permission` event (source `server`): `request_id`,
  `tool`, `input` (the first 200 characters of the input's main field),
  `decision`, `reason`, `by` (`web:<name>` or `machine:<name>`).
- Main field of a tool input (`api.PermissionInputText`): the first non-empty
  string among `command`, `file_path`, `notebook_path`, `path`, `url`,
  `pattern`, `query`, `prompt`; a stored JSON string (a cut input) as itself;
  else the compact JSON. The CLI, the dashboard, and Telegram show it cleaned
  with `termtext.Clean` (Go) or as `textContent` (page).
- Hook outputs, one JSON object on stdout and nothing else:
  - Context: `{"hookSpecificOutput":{"hookEventName":"<SessionStart|UserPromptSubmit>","additionalContext":"<block>"}}`
  - Allow: `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`
  - Deny: `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"<reason, or Denied from sessionhub.>"}}}`
  - `updatedPermissions` is never sent. On expiry, any server error, no
    server, or an answered-locally or closed request, the hook prints nothing
    and exits 0, so Claude Code's own dialog applies.
- Opt-out: `SESSIONHUB_REMOTE_PERMISSIONS=off` in the environment, or
  `remote_permissions = false` in the client config, makes `sessionhub hook
  permission-request` exit at once with no output and no request.
- New `X-Hub-Action` values: `instructions` (`POST /v1/instructions`,
  `DELETE /v1/instructions/{id}`), `send` (`POST /v1/messages`), `approve`
  (`POST /v1/permissions/{id}/decide`). A cookie with no header or another
  value gets `403`, as for Remote Control and triage.
- New routes and access:

  | Route | Access |
  |---|---|
  | `GET /v1/instructions` | read |
  | `POST /v1/instructions` | machine token, or cookie + `instructions` |
  | `DELETE /v1/instructions/{id}` | machine token, or cookie + `instructions` |
  | `POST /v1/messages` | machine token, or cookie + `send` |
  | `GET /v1/messages/{id}` | read |
  | `POST /v1/sessions/{id}/permissions` | machine token (owning machine) |
  | `GET /v1/permissions/{id}/decision` | machine token (owning machine) |
  | `POST /v1/permissions/{id}/decide` | machine token, or cookie + `approve` |

- Watcher results for a message use the existing `POST
  /v1/control/{id}/result` with `state` `delivered`, `refused`, or `busy`. A
  `failed` result (a watcher from before this change, which does not know the
  `message` action) counts as `refused`.
- Installed hook entries after this change (`sessionhub install-hooks`):

  | Event | Matcher | Entries |
  |---|---|---|
  | `SessionStart` | `*` | `sessionhub hook session-start` (async), `sessionhub hook context` (timeout 5) |
  | `UserPromptSubmit` | none | `sessionhub hook prompt` (async), `sessionhub hook context` (timeout 5) |
  | `Stop` | none | `sessionhub hook stop` (async) |
  | `Notification` | `*` | `sessionhub hook notification` (async) |
  | `SessionEnd` | `*` | `sessionhub hook session-end` (timeout 2) |
  | `PermissionRequest` | `*` | `sessionhub hook permission-request` (timeout 660) |

- No new dependencies. No secrets in logs, test output, or docs examples.
  The dashboard inserts every server string with `textContent`.
- Follow `docs/dev/DEFINITION-OF-DONE.md`: `make test` and `make lint` green after
  every commit, and component docs updated in the commit of the task that
  owns the change. Run `gofmt -w` on every Go file you edit before
  `make lint`; the plan's code blocks are not guaranteed to be aligned the
  way `gofmt` wants.
- On this machine, tests can fail with `bind: address already in use`. If a
  run fails that way, wait 60 seconds and run the failing package alone
  before you debug. Run `go test ./internal/server/` on its own, never in the
  same command as the other packages.
- Rollback is restoring the database backup only. A v7 binary refuses a v8
  database (`v > schemaVersion`).
- Anchor every edit on the quoted text, not on line numbers, and `git add`
  only the files your task lists.
- Commit trailer on every commit:
  `Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h`.
- Resolved spec ambiguities (Task 12 records them in `docs/dev/PLAN.md`):
  1. `sessionhub hook session-start` and `sessionhub hook prompt` are installed with
     `async: true`, and Claude Code does not add an async hook's output to
     the prompt it runs with. The block is printed by a new synchronous entry,
     `sessionhub hook context`, in the same matcher group as each of them, so the
     existing network sends stay off the prompt path. One group per event
     keeps `installEvents`' one-sessionhub-group rule.
  2. `sessionhub hook context` takes the event name from stdin's `hook_event_name`
     and prints nothing for any other event or for a subagent call.
  3. `sessionhub hook session-start` starts `sessionhub hook refresh-instructions` as a
     detached child; it fetches the list and rewrites the local copy only
     when `version` changed.
  4. The rate limit counts messages, one per queued or refused target, so a
     20-target send uses 20 of the 30 a minute.
  5. A message is retried after `busy` by offering it again 15 seconds after
     the last offer, on the next long poll; there is no separate retry
     queue. A watcher that crashes after a claim gets the message offered
     again 15 seconds later, so a message can, rarely, arrive twice.
  6. Messages and permission IDs use the control requests' ID scheme with
     their own prefixes: `msg_` and `pr_` plus 16 random bytes in base64url.
  7. `GET /v1/messages/{id}` (read access) is added so `sessionhub send` and the
     MCP tool can wait for delivery results. The spec does not name a
     message read route.
  8. The permission hook stops on the first server error or `404`, as the
     spec says; it does not retry within the 10 minutes.
  9. "Leaves blocked" is a state change from `blocked` to any other state. A
     hooks-only session leaves `blocked` only at its next `Stop` or prompt,
     so its request stays open (and harmless) until then.
  10. Telegram shows `Asks to use <tool>: <main field>` cut to 300
      characters as the third line, before the existing detail line.
  11. A rule's ID is the database row ID. IDs are not reused while rows
      exist, and `sessionhub rules rm` and `forget` take that number.
  12. The inbox item carries the session's newest open request, and only on
      a **Blocked** item.
  13. Messages, rules, and permission inputs keep newlines when stored. The
      watcher sends a message's newlines as they are; the live check
      confirms herdr submits a multi-line text as one prompt (see the deploy
      notes for the fallback).

## Review Focus

1. **A prompt that waits on the network.** `UserPromptSubmit` runs on every
   prompt. Expected: `sessionhub hook context` opens one local file and prints; it
   never calls `newClient`, and a missing or broken file prints nothing.
   Pinned in Task 6 (`TestContextNeverCallsServer`).
2. **A local answer that closes the wrong request.** A session answers
   prompt A in the terminal, Claude asks for B at once, and the `working`
   event for A arrives after B's request. Expected: B stays open, because
   only requests created at or before the state change's own time close.
   Pinned in Task 3 (`TestPermissionAnsweredLocallyKeepsLaterRequest`).
3. **Two decisions for one request.** The dashboard and the CLI decide at
   the same time. Expected: one wins, the other gets `409`, and the hook
   prints exactly the winner's decision. Pinned in Task 3
   (`TestDecidePermissionOnce`) and Task 4 (`TestPermissionDecisionRoutes`).
4. **A message typed into a busy agent.** herdr's `agent.prompt` types into
   whatever is in the pane. Expected: the watcher calls `agent.prompt` only
   after `pane.get` reports `idle` or `done` for the same session ID, and
   reports `busy` otherwise. Pinned in Task 7 (`TestDeliverMessageOnlyWhenIdle`).
5. **A cookie that writes without its action header.** Three new action
   values. Expected: the auth matrix walks every new route with every
   cookie variant, and a rejected write leaves the three new tables
   unchanged. Pinned in Task 4 (`TestAuthMatrix`).
6. **Hook stdout that is not the decision.** Claude Code parses the whole
   stdout of a hook as JSON. Expected: the hook writes one JSON object or
   nothing, and errors go to stderr. Pinned in Task 6
   (`TestPermissionHookOutputs`).

## Dependencies and parallelism

| Task | Needs | Can run alongside |
|---|---|---|
| 1 Store: schema v8, API types, and rules | none | none |
| 2 Store: messages | 1 | none |
| 3 Store: permission requests and the inbox | 1, 2 | none |
| 4 Server: routes, long polls, and auth | 1 to 3 | none |
| 5 Client: HTTP calls and the local rules copy | 4 | none |
| 6 Hooks: context, refresh, and permission requests | 5 | 7, 8 |
| 7 Watcher: rules refresh and message delivery | 5 | 6, 8 |
| 8 MCP: remember, forget, instructions, send_to_sessions | 5 | 6, 7 |
| 9 CLI: rules, send, approve, deny, and the inbox line | 5 | 6 to 8 |
| 10 Telegram: the request in the Blocked message | 3, 4 | 6 to 9 |
| 11 Dashboard: Rules tab, send bar, permission block | 4 | 6 to 10 |
| 12 README, SPEC, and PLAN | all | none |

Tasks 1 to 4 and 10 all edit `docs/server.md` and the store or server
packages; run them in order. Tasks 5 and 7 both edit `internal/client` and
`docs/client.md`; tasks 6 and 9 both read `client.Config`. Tasks 4, 10, and
11 all edit files in `internal/server`; run 10 and 11 one after the other.

---

### Task 1: Store: schema v8, API types, and rules

**Files:**
- Create: `internal/api/actions.go` (types and constants for all three
  features, `PermissionInputText`, `MessagePrompt`)
- Create: `internal/api/actions_test.go`
- Modify: `internal/api/types.go` (`ControlClaim.Text`,
  `InboxItem.Permission`)
- Modify: `internal/store/store.go` (`ErrFull`, `ErrGone`,
  `schemaVersion = 8`, `schemaV8`, migration step)
- Create: `internal/store/instructions.go`
- Create: `internal/store/instructions_test.go`
- Modify: `internal/store/concurrency_test.go` (`rollbackV8`, called first by
  `rollbackV7`)
- Modify: `internal/store/store_test.go` (`user_version` `8`, the new tables'
  columns)
- Modify: `docs/server.md` (Database section)

**Interfaces:**
- Consumes: `checkText`, `invalidf`, `formatTS`, `parseTS`, `openTemp`,
  `newControlEnv`.
- Produces:
  - `api.Instruction`, `api.InstructionList`, `api.InstructionIn`,
    `api.MessagesIn`, `api.MessageResult`, `api.MessagesOut`, `api.Message`,
    `api.PermissionIn`, `api.PermissionRequest`, `api.DecisionIn`
  - constants `api.HeaderActionInstructions`, `api.HeaderActionSend`,
    `api.HeaderActionApprove`, `api.ActionMessage`, `api.KindMessage`,
    `api.KindPermission`, `api.Message*`, `api.Permission*`,
    `api.DecisionAllow`, `api.DecisionDeny`, `api.SenderDashboard`
  - `func api.PermissionInputText(input json.RawMessage) string`
  - `func api.MessagePrompt(sender, text string) string`
  - `store.ErrFull` (server: 409), `store.ErrGone` (server: 410)
  - `func (s *Store) Instructions(ctx context.Context) (api.InstructionList, error)`
  - `func (s *Store) AddInstruction(ctx context.Context, text, createdBy string) (api.Instruction, error)`
  - `func (s *Store) DeleteInstruction(ctx context.Context, id int64) error`
  - test helper `func rollbackV8(t *testing.T, s *Store)`

- [ ] **Step 1: Write the failing API tests**

Create `internal/api/actions_test.go`:

```go
package api

import (
	"encoding/json"
	"testing"
)

func TestPermissionInputText(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
	}{
		{`{"command":"git push","description":"Push"}`, "git push"},
		{`{"file_path":"/tmp/a.go","content":"x"}`, "/tmp/a.go"},
		{`{"notebook_path":"/n.ipynb"}`, "/n.ipynb"},
		{`{"url":"https://example.test","prompt":"read"}`, "https://example.test"},
		{`{"pattern":"*.go"}`, "*.go"},
		{`{"command":"","path":"/srv"}`, "/srv"},
		{`{ "a": 1,  "b": [2] }`, `{"a":1,"b":[2]}`},
		{`"cut input…"`, "cut input…"},
		{``, ""},
		{`not json`, "not json"},
	} {
		if got := PermissionInputText(json.RawMessage(c.in)); got != c.want {
			t.Errorf("PermissionInputText(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMessagePrompt(t *testing.T) {
	got := MessagePrompt("cli on tower", "line one\nline two")
	want := "From the user via sessionhub (cli on tower):\n\nline one\nline two"
	if got != want {
		t.Errorf("MessagePrompt = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/api/ -count=1`
Expected: FAIL to compile with `undefined: PermissionInputText` and
`undefined: MessagePrompt`.

- [ ] **Step 3: Add the API types**

Create `internal/api/actions.go`:

```go
package api

import (
	"bytes"
	"encoding/json"
	"time"
)

// Shared instructions, messages to sessions, and remote permission answers.
// See docs/server.md, "Shared instructions", "Messages to sessions", and
// "Permission requests".
const (
	// X-Hub-Action values the dashboard sends with these writes.
	HeaderActionInstructions = "instructions" // POST /v1/instructions, DELETE /v1/instructions/{id}
	HeaderActionSend         = "send"         // POST /v1/messages
	HeaderActionApprove      = "approve"      // POST /v1/permissions/{id}/decide

	// ActionMessage is the control request kind that carries a message.
	ActionMessage = "message"

	// Event kinds the server records.
	KindMessage    = "message"
	KindPermission = "permission"

	// Message states. MessageBusy is a watcher result only: the agent is
	// working or blocked, and the message stays queued.
	MessageQueued    = "queued"
	MessageDelivered = "delivered"
	MessageExpired   = "expired"
	MessageRefused   = "refused"
	MessageBusy      = "busy"

	// Permission request states.
	PermissionOpen            = "open"
	PermissionDecided         = "decided"
	PermissionExpired         = "expired"
	PermissionAnsweredLocally = "answered_locally"
	PermissionClosed          = "closed"

	DecisionAllow = "allow"
	DecisionDeny  = "deny"

	// SenderDashboard is the sender of a message from a browser.
	SenderDashboard = "dashboard"
)

// Instruction is one standing rule.
type Instruction struct {
	ID        int64     `json:"id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

// InstructionList is the response of GET /v1/instructions, oldest first.
// Version changes whenever the list does.
type InstructionList struct {
	Instructions []Instruction `json:"instructions"`
	Version      string        `json:"version"`
}

// InstructionIn is the body of POST /v1/instructions.
type InstructionIn struct {
	Text string `json:"text"`
}

// MessagesIn is the body of POST /v1/messages.
type MessagesIn struct {
	SessionIDs []string `json:"session_ids"`
	Text       string   `json:"text"`
	// FromSession is the sending session's ID, which the MCP tool sets. The
	// server ignores it for a browser.
	FromSession string `json:"from_session,omitempty"`
}

// MessageResult is what happened to one target of a send: queued with an
// ID, or refused with the reason.
type MessageResult struct {
	SessionID string `json:"session_id"`
	ID        string `json:"id,omitempty"`
	State     string `json:"state"`
	Detail    string `json:"detail,omitempty"`
}

// MessagesOut is the response of POST /v1/messages, one result per distinct
// target, in the order sent.
type MessagesOut struct {
	Results []MessageResult `json:"results"`
}

// Message is one stored message, from GET /v1/messages/{id}. Text is the
// text as sent, without the prefix.
type Message struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Machine   string    `json:"machine"`
	Sender    string    `json:"sender"`
	Text      string    `json:"text"`
	State     string    `json:"state"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PermissionIn is the body of POST /v1/sessions/{id}/permissions: the
// PermissionRequest hook's input.
type PermissionIn struct {
	ToolName    string          `json:"tool_name"`
	ToolInput   json.RawMessage `json:"tool_input,omitempty"`
	CWD         string          `json:"cwd,omitempty"`
	Suggestions json.RawMessage `json:"suggestions,omitempty"`
}

// PermissionRequest is one permission prompt waiting for, or past, an
// answer.
type PermissionRequest struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	Machine   string          `json:"machine"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	Truncated bool            `json:"truncated,omitempty"`
	State     string          `json:"state"`
	Decision  string          `json:"decision,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	DecidedBy string          `json:"decided_by,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	DecidedAt *time.Time      `json:"decided_at,omitempty"`
}

// DecisionIn is the body of POST /v1/permissions/{id}/decide.
type DecisionIn struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// permissionFields are the tool input fields that say what a tool will do,
// in the order PermissionInputText tries them.
var permissionFields = []string{"command", "file_path", "notebook_path", "path", "url", "pattern", "query", "prompt"}

// PermissionInputText is the part of a tool input a person reads to decide:
// the first non-empty string among permissionFields, a cut input (stored as
// a JSON string) as itself, else the compact JSON. The result is untrusted
// text: clean it before printing.
func PermissionInputText(input json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(input, &m) == nil {
		for _, k := range permissionFields {
			var s string
			if v, ok := m[k]; ok && json.Unmarshal(v, &s) == nil && s != "" {
				return s
			}
		}
	}
	var s string
	if json.Unmarshal(input, &s) == nil {
		return s
	}
	var buf bytes.Buffer
	if json.Compact(&buf, input) == nil {
		return buf.String()
	}
	return string(input)
}

// MessagePrompt is the text the watcher submits for a message: who sent it,
// a blank line, and the text.
func MessagePrompt(sender, text string) string {
	return "From the user via sessionhub (" + sender + "):\n\n" + text
}
```

In `internal/api/types.go`, replace:

```go
type ControlClaim struct {
	Request ControlRequest `json:"request"`
	Session Session        `json:"session"`
}
```

with:

```go
type ControlClaim struct {
	Request ControlRequest `json:"request"`
	Session Session        `json:"session"`
	// Text is the prompt to submit, for a request whose action is
	// ActionMessage.
	Text string `json:"text,omitempty"`
}
```

and replace:

```go
type InboxItem struct {
	Group     string    `json:"group"`
	Since     time.Time `json:"since"`
	WaitingOn []string  `json:"waiting_on,omitempty"`
	Session   Session   `json:"session"`
}
```

with:

```go
type InboxItem struct {
	Group     string    `json:"group"`
	Since     time.Time `json:"since"`
	WaitingOn []string  `json:"waiting_on,omitempty"`
	Session   Session   `json:"session"`
	// Permission is the session's newest open permission request, on a
	// blocked item only.
	Permission *PermissionRequest `json:"permission,omitempty"`
}
```

- [ ] **Step 4: Run the API tests to verify they pass**

Run: `go test ./internal/api/ -count=1`
Expected: PASS.

- [ ] **Step 5: Write the failing store tests**

In `internal/store/store_test.go` (`TestOpenPragmasAndSchema`), replace
`{"user_version", "7"},` with `{"user_version", "8"},`, replace:

```go
	for _, table := range []string{"machines", "sessions", "events", "reports", "control_requests", "session_digests", "login_codes", "web_sessions", "inbox_triage", "inbox_alerts"} {
```

with:

```go
	for _, table := range []string{"machines", "sessions", "events", "reports", "control_requests", "session_digests", "login_codes", "web_sessions", "inbox_triage", "inbox_alerts",
		"instructions", "messages", "permission_requests"} {
```

and after the line `"inbox_alerts":     "sent_at session_id since",` add:

```go
		"instructions":        "created_at created_by id text",
		"messages":            "created_at detail id machine_id offered_at sender session_id state text updated_at",
		"permission_requests": "created_at decided_at decided_by decision expires_at id machine_id reason session_id state tool_input tool_name truncated updated_at",
```

In `internal/store/concurrency_test.go`, replace:

```go
func rollbackV7(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec("DROP TABLE inbox_alerts"); err != nil {
		t.Fatalf("DROP TABLE inbox_alerts: %v", err)
	}
}
```

with:

```go
func rollbackV7(t *testing.T, s *Store) {
	t.Helper()
	rollbackV8(t, s)
	if _, err := s.db.Exec("DROP TABLE inbox_alerts"); err != nil {
		t.Fatalf("DROP TABLE inbox_alerts: %v", err)
	}
}

// rollbackV8 removes what schema v8 added, so a test can set user_version
// to 7 or lower and reopen the file as an older release left it.
func rollbackV8(t *testing.T, s *Store) {
	t.Helper()
	for _, q := range []string{"DROP TABLE instructions", "DROP TABLE messages", "DROP TABLE permission_requests"} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}
```

Create `internal/store/instructions_test.go`:

```go
package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestMigrateV7ToV8(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	if _, _, err := s.AddMachine(ctx, "tower", "", ""); err != nil {
		t.Fatal(err)
	}
	rollbackV8(t, s)
	if _, err := s.db.Exec("PRAGMA user_version = 7"); err != nil {
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
	if ms, _ := s2.ListMachines(ctx); len(ms) != 1 {
		t.Errorf("machines after upgrade: %+v", ms)
	}
	if _, err := s2.AddInstruction(ctx, "Use British spelling.", "tower"); err != nil {
		t.Errorf("AddInstruction after upgrade: %v", err)
	}
}

func TestInstructions(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	empty, err := e.s.Instructions(ctx)
	if err != nil || empty.Instructions == nil || len(empty.Instructions) != 0 || empty.Version == "" {
		t.Fatalf("empty list: %+v %v", empty, err)
	}
	a, err := e.s.AddInstruction(ctx, "  Never push to main.  ", "tower")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == 0 || a.Text != "Never push to main." || a.CreatedBy != "tower" || !a.CreatedAt.Equal(e.clock.Now()) {
		t.Errorf("added %+v", a)
	}
	e.clock.Advance(time.Second)
	b, err := e.s.AddInstruction(ctx, "Write in British English.", "web:phone")
	if err != nil {
		t.Fatal(err)
	}
	list, err := e.s.Instructions(ctx)
	if err != nil || len(list.Instructions) != 2 || list.Instructions[0].ID != a.ID || list.Instructions[1].ID != b.ID {
		t.Fatalf("list %+v %v", list, err)
	}
	if list.Version == empty.Version || len(list.Version) != 16 {
		t.Errorf("version %q did not change from %q, or is not 16 hex digits", list.Version, empty.Version)
	}
	again, _ := e.s.Instructions(ctx)
	if again.Version != list.Version {
		t.Errorf("an unchanged list changed version: %q then %q", list.Version, again.Version)
	}
	if err := e.s.DeleteInstruction(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteInstruction(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v, want ErrNotFound", err)
	}
	list, _ = e.s.Instructions(ctx)
	if len(list.Instructions) != 1 || list.Instructions[0].ID != b.ID {
		t.Errorf("after delete: %+v", list)
	}
}

func TestInstructionLimits(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	for name, text := range map[string]string{
		"empty":       "   ",
		"too long":    strings.Repeat("x", MaxInstructionRunes+1),
		"newline":     "one\ntwo",
		"escape":      "red \x1b[31m",
	} {
		if _, err := e.s.AddInstruction(ctx, text, "tower"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := e.s.AddInstruction(ctx, "ok", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty created_by: %v, want ErrInvalid", err)
	}
	// 300 characters is allowed; the list holds six of them (1,800), and a
	// seventh would pass 2,000.
	rule := strings.Repeat("é", MaxInstructionRunes)
	for i := 0; i < 6; i++ {
		if _, err := e.s.AddInstruction(ctx, rule, "tower"); err != nil {
			t.Fatalf("rule %d: %v", i, err)
		}
	}
	if _, err := e.s.AddInstruction(ctx, rule, "tower"); !errors.Is(err, ErrFull) {
		t.Fatalf("past 2,000 characters: %v, want ErrFull", err)
	}
	if _, err := e.s.AddInstruction(ctx, strings.Repeat("y", 200), "tower"); err != nil {
		t.Errorf("exactly 2,000 characters: %v", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM instructions`); n != 7 {
		t.Errorf("%d rows, want 7", n)
	}
	var list api.InstructionList
	list, _ = e.s.Instructions(ctx)
	if len(list.Instructions) != 7 {
		t.Errorf("list has %d rules, want 7", len(list.Instructions))
	}
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./internal/store/ -run 'TestMigrateV7ToV8|TestInstructions|TestInstructionLimits|TestOpenPragmasAndSchema' -count=1`
Expected: FAIL to compile with `e.s.AddInstruction undefined`,
`undefined: MaxInstructionRunes`, and `undefined: ErrFull`.

- [ ] **Step 7: Add schema v8 and the errors**

In `internal/store/store.go`, after:

```go
	// ErrCodeGone: the sign-in code is unknown, expired, or used. 410.
	ErrCodeGone = errors.New("sign-in link expired or already used")
```

add:

```go
	// ErrFull: the shared instructions would pass their size limit. 409.
	ErrFull = errors.New("full")
	// ErrGone: a permission request expired, was answered in the terminal,
	// or closed with its session. 410.
	ErrGone = errors.New("gone")
```

Replace `const schemaVersion = 7` with `const schemaVersion = 8`. After the
`schemaV7` constant, add:

```go
// schemaV8 adds shared instructions, messages to sessions, and permission
// requests. Messages and permission requests go with their session or
// machine.
const schemaV8 = `
CREATE TABLE instructions (
	id         INTEGER PRIMARY KEY,
	text       TEXT NOT NULL,
	created_at TEXT NOT NULL,
	created_by TEXT NOT NULL              -- machine name, or web:<session name>
);
CREATE TABLE messages (
	id         TEXT PRIMARY KEY,          -- msg_ + 16 random bytes, base64url
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	machine_id INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	text       TEXT NOT NULL,             -- as sent, without the prefix
	sender     TEXT NOT NULL,             -- dashboard | cli on <machine> | session <id8>
	state      TEXT NOT NULL,             -- queued|delivered|expired|refused
	detail     TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	offered_at TEXT                       -- the last time a watcher was handed it
);
CREATE INDEX messages_machine ON messages(machine_id, state, created_at);
CREATE INDEX messages_sender ON messages(sender, created_at);
CREATE TABLE permission_requests (
	id         TEXT PRIMARY KEY,          -- pr_ + 16 random bytes, base64url
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	machine_id INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	tool_name  TEXT NOT NULL,
	tool_input TEXT NOT NULL,             -- compact JSON, at most 8 KiB
	truncated  INTEGER NOT NULL DEFAULT 0,
	state      TEXT NOT NULL,             -- open|decided|expired|answered_locally|closed
	decision   TEXT NOT NULL DEFAULT '',  -- allow|deny once decided
	reason     TEXT NOT NULL DEFAULT '',
	decided_by TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	decided_at TEXT
);
CREATE INDEX permission_requests_session ON permission_requests(session_id, state, created_at);
`
```

In `migrate`, after the `if v < 7 { ... }` block, add:

```go
	if v < 8 {
		if _, err := tx.ExecContext(ctx, schemaV8); err != nil {
			return fmt.Errorf("migrate schema to v8: %w", err)
		}
	}
```

- [ ] **Step 8: Add the rule queries**

Create `internal/store/instructions.go`:

```go
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
)

// Shared instruction limits. See docs/server.md, "Shared instructions".
const (
	// MaxInstructionRunes caps one rule.
	MaxInstructionRunes = 300
	// MaxInstructionsRunes caps the whole list.
	MaxInstructionsRunes = 2000
	// maxCreatedByRunes caps created_by: a machine name or web:<name>.
	maxCreatedByRunes = 100
)

// querier is the part of *sql.DB and *sql.Tx the list read needs.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Instructions returns every rule, oldest first, and the list's version.
func (s *Store) Instructions(ctx context.Context) (api.InstructionList, error) {
	return readInstructions(ctx, s.db)
}

func readInstructions(ctx context.Context, q querier) (api.InstructionList, error) {
	out := api.InstructionList{Instructions: []api.Instruction{}}
	rows, err := q.QueryContext(ctx, `SELECT id, text, created_at, created_by FROM instructions ORDER BY id`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var in api.Instruction
		var created string
		if err := rows.Scan(&in.ID, &in.Text, &created, &in.CreatedBy); err != nil {
			return out, err
		}
		if in.CreatedAt, err = parseTS(created); err != nil {
			return out, err
		}
		out.Instructions = append(out.Instructions, in)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.Version = instructionsVersion(out.Instructions)
	return out, nil
}

// instructionsVersion is the first 16 hex digits of a SHA-256 over each
// rule's ID and text.
func instructionsVersion(list []api.Instruction) string {
	h := sha256.New()
	for _, in := range list {
		fmt.Fprintf(h, "%d\x00%s\x00", in.ID, in.Text)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// AddInstruction stores a rule. text is trimmed and must be 1 to
// MaxInstructionRunes characters with no control characters. A rule that
// would take the list past MaxInstructionsRunes returns ErrFull.
func (s *Store) AddInstruction(ctx context.Context, text, createdBy string) (api.Instruction, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return api.Instruction{}, invalidf("text is empty")
	}
	if err := checkText("text", text, MaxInstructionRunes); err != nil {
		return api.Instruction{}, err
	}
	if createdBy == "" {
		return api.Instruction{}, invalidf("created_by is empty")
	}
	if err := checkText("created_by", createdBy, maxCreatedByRunes); err != nil {
		return api.Instruction{}, err
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Instruction{}, err
	}
	defer tx.Rollback()
	cur, err := readInstructions(ctx, tx)
	if err != nil {
		return api.Instruction{}, err
	}
	used := 0
	for _, in := range cur.Instructions {
		used += utf8.RuneCountInString(in.Text)
	}
	if n := utf8.RuneCountInString(text); used+n > MaxInstructionsRunes {
		return api.Instruction{}, fmt.Errorf("%w: the rules use %d of %d characters and this one has %d; remove a rule first",
			ErrFull, used, MaxInstructionsRunes, n)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO instructions (text, created_at, created_by) VALUES (?, ?, ?)`,
		text, formatTS(now), createdBy)
	if err != nil {
		return api.Instruction{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return api.Instruction{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Instruction{}, err
	}
	return api.Instruction{ID: id, Text: text, CreatedAt: now, CreatedBy: createdBy}, nil
}

// DeleteInstruction removes rule id, or returns ErrNotFound.
func (s *Store) DeleteInstruction(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM instructions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: rule %d", ErrNotFound, id)
	}
	return nil
}
```

`now` comes from the test clock with a zero nanosecond part, so the returned
`CreatedAt` equals the stored one.

- [ ] **Step 9: Run the tests to verify they pass**

Run: `go test ./internal/store/ ./internal/api/ -count=1`
Expected: PASS, including `TestMigrateV6ToV7` and every older migration
test (they now also drop the v8 tables through `rollbackV7`).

- [ ] **Step 10: Document it**

In `docs/server.md`, section "Database", replace:

```markdown
version 7 adds `inbox_alerts`), and refuses
```

with:

```markdown
version 7 adds `inbox_alerts`; version 8 adds `instructions`, `messages`, and `permission_requests`), and refuses
```

After the "Inbox alerts" subsection of "Database" (it ends with "The v6
binary refuses a version 7 database."), add:

```markdown
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

To roll back to the v7 binary, restore the pre-deploy backup. The v7 binary
refuses a version 8 database.
```

- [ ] **Step 11: Run the full suite and lint**

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1 && make lint`
Expected: every package `ok`, `gofmt -l` prints nothing, `go vet` clean.

- [ ] **Step 12: Commit**

```bash
git add internal/api/actions.go internal/api/actions_test.go internal/api/types.go \
  internal/store/store.go internal/store/instructions.go internal/store/instructions_test.go \
  internal/store/concurrency_test.go internal/store/store_test.go docs/server.md
git commit -m "$(cat <<'EOF'
Add schema v8 and the shared instructions store

Schema 8 adds instructions, messages, and permission_requests. The
store keeps standing rules of at most 300 characters each and 2,000 in
all, with a version hash clients use to skip unchanged copies. The API
types for messages and permission requests land here so later tasks
share them.

Refs: docs/dev/superpowers/plans/2026-10-02-session-actions.md, Task 1

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 2: Store: messages

**Files:**
- Create: `internal/store/messages.go`
- Create: `internal/store/messages_test.go`
- Modify: `docs/server.md` (new "Messages to sessions" section)

**Interfaces:**
- Consumes: `api.MessageResult`, `api.Message`, `api.ControlClaim`,
  `api.MessagePrompt`, the `api.Message*` states (Task 1);
  `ValidSessionID`, `checkText`, `insertEventTx`, `ControlPollWindow`,
  `(*Store).GetSession`, `newControlEnv`, `controlEnv.session`,
  `controlEnv.poll`, `controlEnv.events`, `controlEnv.count`.
- Produces:
  - constants `MaxMessageTargets` (20), `MaxMessageRunes` (4000),
    `MessageTTL` (10 minutes), `MessageRetry` (15 seconds),
    `MessagesPerMinute` (30)
  - `func ValidMessageID(id string) bool`
  - `func newRandomID(prefix string) (string, error)`
  - `func firstRunes(s string, n int) string`
  - `func (s *Store) SendMessages(ctx context.Context, ids []string, text, sender string) (results []api.MessageResult, machines []string, err error)`
    (`machines` are the machines with a newly queued message, for the long
    poll's wake-up)
  - `func (s *Store) ClaimMessage(ctx context.Context, machineID int64) (api.ControlClaim, bool, error)`
  - `func (s *Store) FinishMessage(ctx context.Context, machineID int64, id string, in api.ControlResultIn) (api.Message, error)`
  - `func (s *Store) GetMessage(ctx context.Context, id string) (api.Message, error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/store/messages_test.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// messageEnv has s-ok (tower, pane, watcher polling), s-nopane (tower, no
// pane), s-ended (tower, pane, ended), and s-bluebox (bluebox, pane, no poll).
func messageEnv(t *testing.T) *controlEnv {
	t.Helper()
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s-ok", "w1:p1")
	e.session(e.tower, "s-nopane", "")
	e.session(e.tower, "s-ended", "w1:p2")
	if err := e.s.AddEvent(ctx, e.tower.ID, "s-ended", api.EventIn{Kind: api.KindEnded, Source: api.SourcePlugin}); err != nil {
		t.Fatal(err)
	}
	e.session(e.bluebox, "s-bluebox", "w2:p1")
	e.poll(e.tower)
	return e
}

func TestSendMessagesTargets(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	res, machines, err := e.s.SendMessages(ctx, []string{"s-ok", "s-nopane", "s-ended", "s-bluebox", "nope", "s-ok", "bad id"},
		"Please rebase.\n\tThanks", "cli on bluebox")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(res))
	for i, r := range res {
		got[i] = r.SessionID + " " + r.State + " " + r.Detail
	}
	want := []string{
		"s-ok queued ",
		"s-nopane refused the session is not in herdr",
		"s-ended refused the session has ended",
		`s-bluebox refused the sessionhub watcher on "bluebox" is offline`,
		"nope refused unknown session",
		"bad id refused invalid session id",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("results:\n got %q\nwant %q", got, want)
	}
	if !ValidMessageID(res[0].ID) || res[1].ID != "" {
		t.Errorf("IDs: queued %q, refused %q", res[0].ID, res[1].ID)
	}
	if !reflect.DeepEqual(machines, []string{"tower"}) {
		t.Errorf("machines %v, want [tower]", machines)
	}
	m, err := e.s.GetMessage(ctx, res[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.SessionID != "s-ok" || m.Machine != "tower" || m.Sender != "cli on bluebox" || m.Text != "Please rebase.\n\tThanks" ||
		m.State != api.MessageQueued || !m.CreatedAt.Equal(e.clock.Now()) {
		t.Errorf("stored %+v", m)
	}
	if n := e.count(`SELECT COUNT(*) FROM messages`); n != 1 {
		t.Errorf("%d rows, want 1: refused targets are not stored", n)
	}
}

func TestSendMessagesValidation(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	many := make([]string, MaxMessageTargets+1)
	for i := range many {
		many[i] = fmt.Sprintf("s-%d", i)
	}
	for name, c := range map[string]struct {
		ids          []string
		text, sender string
	}{
		"no targets":      {nil, "hi", "dashboard"},
		"21 targets":      {many, "hi", "dashboard"},
		"blank text":      {[]string{"s-ok"}, " \n ", "dashboard"},
		"long text":       {[]string{"s-ok"}, strings.Repeat("x", MaxMessageRunes+1), "dashboard"},
		"escape":          {[]string{"s-ok"}, "red \x1b[31m", "dashboard"},
		"carriage return": {[]string{"s-ok"}, "a\rb", "dashboard"},
		"no sender":       {[]string{"s-ok"}, "hi", ""},
	} {
		if _, _, err := e.s.SendMessages(ctx, c.ids, c.text, c.sender); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, strings.Repeat("é", MaxMessageRunes), "dashboard"); err != nil {
		t.Errorf("4,000 characters: %v", err)
	}
}

func TestSendMessagesRateLimit(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	ids := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		ids = append(ids, "s-ok") // one distinct target: one message
	}
	for i := 0; i < MessagesPerMinute; i++ {
		if _, _, err := e.s.SendMessages(ctx, ids, fmt.Sprintf("m%d", i), "dashboard"); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		e.clock.Advance(time.Second)
	}
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "one more", "dashboard"); !errors.Is(err, ErrTooMany) {
		t.Fatalf("31st message in a minute: %v, want ErrTooMany", err)
	}
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "other sender", "cli on tower"); err != nil {
		t.Errorf("another sender: %v", err)
	}
	// A refused target counts too: a send of two where the window has one
	// slot left is refused as a whole.
	e.clock.Advance(30 * time.Second) // the first message leaves the window
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok", "s-bluebox"}, "two", "dashboard"); !errors.Is(err, ErrTooMany) {
		t.Errorf("two targets with one slot: %v, want ErrTooMany", err)
	}
	if _, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "one", "dashboard"); err != nil {
		t.Errorf("one target with one slot: %v", err)
	}
}

func TestClaimAndFinishMessage(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	res, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "Please rebase.", "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	id := res[0].ID
	if _, ok, err := e.s.ClaimMessage(ctx, e.bluebox.ID); ok || err != nil {
		t.Fatalf("bluebox claimed tower's message: %v %v", ok, err)
	}
	claim, ok, err := e.s.ClaimMessage(ctx, e.tower.ID)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	r := claim.Request
	if r.ID != id || r.Action != api.ActionMessage || r.State != api.ControlClaimed || r.SessionID != "s-ok" ||
		r.Machine != "tower" || r.RequestedBy != "dashboard" || !r.ExpiresAt.Equal(r.CreatedAt.Add(MessageTTL)) {
		t.Errorf("request %+v", r)
	}
	if claim.Text != "From the user via sessionhub (dashboard):\n\nPlease rebase." || claim.Session.ID != "s-ok" || claim.Session.HerdrPane != "w1:p1" {
		t.Errorf("claim text %q session %+v", claim.Text, claim.Session)
	}
	// Offered: not again until MessageRetry passes.
	if _, ok, _ := e.s.ClaimMessage(ctx, e.tower.ID); ok {
		t.Fatal("claimed twice at once")
	}
	m, err := e.s.FinishMessage(ctx, e.tower.ID, id, api.ControlResultIn{State: api.MessageBusy, Detail: "the agent is working"})
	if err != nil || m.State != api.MessageQueued || m.Detail != "the agent is working" {
		t.Fatalf("busy: %+v %v", m, err)
	}
	e.clock.Advance(MessageRetry - time.Second)
	if _, ok, _ := e.s.ClaimMessage(ctx, e.tower.ID); ok {
		t.Fatal("offered again before MessageRetry")
	}
	e.clock.Advance(time.Second)
	if _, ok, _ := e.s.ClaimMessage(ctx, e.tower.ID); !ok {
		t.Fatal("not offered again after MessageRetry")
	}
	if _, err := e.s.FinishMessage(ctx, e.bluebox.ID, id, api.ControlResultIn{State: api.MessageDelivered}); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("bluebox's result: %v, want ErrWrongMachine", err)
	}
	m, err = e.s.FinishMessage(ctx, e.tower.ID, id, api.ControlResultIn{State: api.MessageDelivered})
	if err != nil || m.State != api.MessageDelivered {
		t.Fatalf("delivered: %+v %v", m, err)
	}
	evs := e.events("s-ok", api.KindMessage)
	if len(evs) != 1 || evs[0].Source != api.SourceServer {
		t.Fatalf("message events %+v", evs)
	}
	var p map[string]string
	json.Unmarshal(evs[0].Payload, &p)
	if p["message_id"] != id || p["sender"] != "dashboard" || p["state"] != api.MessageDelivered || p["text"] != "Please rebase." {
		t.Errorf("event payload %v", p)
	}
	if _, err := e.s.FinishMessage(ctx, e.tower.ID, id, api.ControlResultIn{State: api.MessageDelivered}); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("second result: %v, want ErrRequestClosed", err)
	}
	for _, bad := range []api.ControlResultIn{{State: "done"}, {State: api.MessageDelivered, URL: "https://claude.ai/code/session_x"}} {
		if _, err := e.s.FinishMessage(ctx, e.tower.ID, id, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("result %+v: %v, want ErrInvalid", bad, err)
		}
	}
	if _, err := e.s.FinishMessage(ctx, e.tower.ID, "cr_AAAAAAAAAAAAAAAAAAAAAA", api.ControlResultIn{State: api.MessageDelivered}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a control ID: %v, want ErrInvalid", err)
	}
}

func TestFinishMessageRefusedAndFailed(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	for _, state := range []string{api.MessageRefused, api.ControlFailed} {
		res, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "hi "+state, "dashboard")
		if err != nil {
			t.Fatal(err)
		}
		m, err := e.s.FinishMessage(ctx, e.tower.ID, res[0].ID, api.ControlResultIn{State: state, Detail: "pane not found"})
		if err != nil || m.State != api.MessageRefused || m.Detail != "pane not found" {
			t.Errorf("%s: %+v %v, want refused", state, m, err)
		}
	}
	if evs := e.events("s-ok", api.KindMessage); len(evs) != 0 {
		t.Errorf("a refused message added %d events, want 0", len(evs))
	}
}

func TestMessageExpiry(t *testing.T) {
	e := messageEnv(t)
	ctx := context.Background()
	res, _, err := e.s.SendMessages(ctx, []string{"s-ok"}, "too late", "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	id := res[0].ID
	e.clock.Advance(MessageTTL)
	// A read reports the expiry before any write stores it.
	if m, _ := e.s.GetMessage(ctx, id); m.State != api.MessageExpired {
		t.Errorf("read after TTL: %s, want expired", m.State)
	}
	if _, ok, err := e.s.ClaimMessage(ctx, e.tower.ID); ok || err != nil {
		t.Fatalf("claimed an expired message: %v %v", ok, err)
	}
	if n := e.count(`SELECT COUNT(*) FROM messages WHERE state = 'expired'`); n != 1 {
		t.Errorf("stored expired rows %d, want 1", n)
	}
	if evs := e.events("s-ok", api.KindMessage); len(evs) != 1 {
		t.Errorf("expiry events %d, want 1", len(evs))
	}
	if _, err := e.s.FinishMessage(ctx, e.tower.ID, id, api.ControlResultIn{State: api.MessageDelivered}); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("result after expiry: %v, want ErrRequestClosed", err)
	}
	if _, err := e.s.GetMessage(ctx, "msg_AAAAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown message: %v, want ErrNotFound", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/store/ -run 'TestSendMessages|TestClaimAndFinishMessage|TestFinishMessageRefusedAndFailed|TestMessageExpiry' -count=1`
Expected: FAIL to compile with `e.s.SendMessages undefined`.

- [ ] **Step 3: Write the message queries**

Create `internal/store/messages.go`:

```go
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
)

// Message limits. See docs/server.md, "Messages to sessions".
const (
	MaxMessageTargets = 20
	MaxMessageRunes   = 4000
	// MessageTTL is how long a message stays queued.
	MessageTTL = 10 * time.Minute
	// MessageRetry is how long after an offer a queued message is offered
	// again: after a busy result, or a watcher that never answered.
	MessageRetry = 15 * time.Second
	// MessagesPerMinute caps one sender's messages in any 60 seconds.
	MessagesPerMinute = 30
	maxSenderRunes    = 100
	// eventTextRunes is how much of a text a message or permission event
	// keeps.
	eventTextRunes = 200
)

var messageIDRE = regexp.MustCompile(`^msg_[A-Za-z0-9_-]{22}$`)

// ValidMessageID reports whether id has the shape of a message ID.
func ValidMessageID(id string) bool { return messageIDRE.MatchString(id) }

// newRandomID is prefix followed by 16 random bytes in base64url.
func newRandomID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// firstRunes cuts s to at most n runes.
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// checkMessageText allows newlines and tabs, unlike checkText: a message is
// prose a person wrote.
func checkMessageText(text string) error {
	if strings.TrimSpace(text) == "" {
		return invalidf("text is empty")
	}
	if n := utf8.RuneCountInString(text); n > MaxMessageRunes {
		return invalidf("text is %d characters, at most %d allowed", n, MaxMessageRunes)
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return invalidf("text contains a control character (U+%04X), which is not allowed", r)
		}
	}
	return nil
}

func messageEventTx(ctx context.Context, tx *sql.Tx, sessionID, id, sender, state, text string, now time.Time) error {
	payload, err := json.Marshal(map[string]string{"message_id": id, "sender": sender, "state": state,
		"text": firstRunes(text, eventTextRunes)})
	if err != nil {
		return err
	}
	return insertEventTx(ctx, tx, sessionID, now, api.SourceServer, api.KindMessage, payload)
}

// expireMessagesTx stores the expiry of every queued message older than
// MessageTTL and records its event. Sends, claims, and results run it
// first; GetMessage reports the expiry without storing it.
func expireMessagesTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, session_id, sender, text FROM messages WHERE state = ? AND created_at <= ?`,
		api.MessageQueued, formatTS(now.Add(-MessageTTL)))
	if err != nil {
		return err
	}
	type old struct{ id, session, sender, text string }
	var list []old
	for rows.Next() {
		var o old
		if err := rows.Scan(&o.id, &o.session, &o.sender, &o.text); err != nil {
			rows.Close()
			return err
		}
		list = append(list, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range list {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET state = ?, detail = ?, updated_at = ? WHERE id = ?`,
			api.MessageExpired, "not delivered within 10 minutes", formatTS(now), o.id); err != nil {
			return err
		}
		if err := messageEventTx(ctx, tx, o.session, o.id, o.sender, api.MessageExpired, o.text, now); err != nil {
			return err
		}
	}
	return nil
}

// SendMessages queues text for each distinct session in ids, in order. A
// target that does not exist, has ended, has no herdr pane, or whose
// machine's watcher has not polled in ControlPollWindow is refused with a
// reason, and the others still go. machines lists the machines that got a
// queued message, once each. More than MessagesPerMinute messages from one
// sender in 60 seconds is ErrTooMany for the whole send.
func (s *Store) SendMessages(ctx context.Context, ids []string, text, sender string) ([]api.MessageResult, []string, error) {
	var uniq []string
	seen := map[string]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 || len(uniq) > MaxMessageTargets {
		return nil, nil, invalidf("session_ids has %d distinct sessions, want 1 to %d", len(uniq), MaxMessageTargets)
	}
	if err := checkMessageText(text); err != nil {
		return nil, nil, err
	}
	if sender == "" {
		return nil, nil, invalidf("sender is empty")
	}
	if err := checkText("sender", sender, maxSenderRunes); err != nil {
		return nil, nil, err
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if err := expireMessagesTx(ctx, tx, now); err != nil {
		return nil, nil, err
	}
	var recent int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE sender = ? AND created_at > ?`,
		sender, formatTS(now.Add(-time.Minute))).Scan(&recent); err != nil {
		return nil, nil, err
	}
	if recent+len(uniq) > MessagesPerMinute {
		return nil, nil, fmt.Errorf("%w: %q sent %d messages in the last minute, and at most %d are allowed",
			ErrTooMany, sender, recent, MessagesPerMinute)
	}
	results := make([]api.MessageResult, 0, len(uniq))
	var machines []string
	for _, id := range uniq {
		r := api.MessageResult{SessionID: id, State: api.MessageRefused}
		reason, machineID, machine, err := messageTargetTx(ctx, tx, id, now)
		if err != nil {
			return nil, nil, err
		}
		if reason != "" {
			r.Detail = reason
			results = append(results, r)
			continue
		}
		mid, err := newRandomID("msg_")
		if err != nil {
			return nil, nil, err
		}
		nowS := formatTS(now)
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, session_id, machine_id, text, sender, state,
			created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			mid, id, machineID, text, sender, api.MessageQueued, nowS, nowS); err != nil {
			return nil, nil, err
		}
		r.ID, r.State = mid, api.MessageQueued
		results = append(results, r)
		if !contains(machines, machine) {
			machines = append(machines, machine)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return results, machines, nil
}

// messageTargetTx checks one target. reason is "" when a message can be
// queued for it.
func messageTargetTx(ctx context.Context, tx *sql.Tx, id string, now time.Time) (reason string, machineID int64, machine string, err error) {
	if !ValidSessionID(id) {
		return "invalid session id", 0, "", nil
	}
	var pane string
	var ended, lastPoll sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT s.machine_id, m.name, s.herdr_pane, s.ended_at, m.last_poll
		FROM sessions s JOIN machines m ON m.id = s.machine_id WHERE s.id = ?`, id).
		Scan(&machineID, &machine, &pane, &ended, &lastPoll)
	if errors.Is(err, sql.ErrNoRows) {
		return "unknown session", 0, "", nil
	}
	if err != nil {
		return "", 0, "", err
	}
	switch {
	case ended.Valid:
		return "the session has ended", 0, "", nil
	case pane == "":
		return "the session is not in herdr", 0, "", nil
	}
	polled, err := parseNullTS(lastPoll)
	if err != nil {
		return "", 0, "", err
	}
	if polled == nil || now.Sub(*polled) > ControlPollWindow {
		return fmt.Sprintf("the sessionhub watcher on %q is offline", machine), 0, "", nil
	}
	return "", machineID, machine, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ClaimMessage hands machineID its oldest queued message that was never
// offered or was last offered at least MessageRetry ago, as a control
// claim whose action is api.ActionMessage and whose Text is the prompt to
// submit. ok is false when there is none.
func (s *Store) ClaimMessage(ctx context.Context, machineID int64) (api.ControlClaim, bool, error) {
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	defer tx.Rollback()
	if err := expireMessagesTx(ctx, tx, now); err != nil {
		return api.ControlClaim{}, false, err
	}
	var id, sessionID, text, sender, created, machine string
	err = tx.QueryRowContext(ctx, `SELECT m.id, m.session_id, m.text, m.sender, m.created_at, mc.name
		FROM messages m JOIN machines mc ON mc.id = m.machine_id
		WHERE m.machine_id = ? AND m.state = ? AND (m.offered_at IS NULL OR m.offered_at <= ?)
		ORDER BY m.created_at, m.rowid LIMIT 1`, machineID, api.MessageQueued, formatTS(now.Add(-MessageRetry))).
		Scan(&id, &sessionID, &text, &sender, &created, &machine)
	if errors.Is(err, sql.ErrNoRows) {
		return api.ControlClaim{}, false, tx.Commit()
	}
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET offered_at = ?, updated_at = ? WHERE id = ?`,
		formatTS(now), formatTS(now), id); err != nil {
		return api.ControlClaim{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return api.ControlClaim{}, false, err
	}
	createdAt, err := parseTS(created)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	// GetSession uses s.db, so it runs after the commit.
	sess, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	req := api.ControlRequest{ID: id, SessionID: sessionID, Machine: machine, Action: api.ActionMessage,
		State: api.ControlClaimed, RequestedBy: sender, CreatedAt: createdAt, ExpiresAt: createdAt.Add(MessageTTL), ClaimedAt: &now}
	return api.ControlClaim{Request: req, Session: sess, Text: api.MessagePrompt(sender, text)}, true, nil
}

// FinishMessage records a watcher's result for a queued message of
// machineID: delivered, refused, or busy (still queued). A failed result,
// from a watcher that does not know the message action, counts as refused.
func (s *Store) FinishMessage(ctx context.Context, machineID int64, id string, in api.ControlResultIn) (api.Message, error) {
	if !ValidMessageID(id) {
		return api.Message{}, invalidf("message id %q: want msg_ and 22 base64url characters", id)
	}
	state := in.State
	if state == api.ControlFailed {
		state = api.MessageRefused
	}
	if state != api.MessageDelivered && state != api.MessageRefused && state != api.MessageBusy {
		return api.Message{}, invalidf("state %q: want delivered, refused, or busy", in.State)
	}
	if in.URL != "" {
		return api.Message{}, invalidf("url: a message result carries no link")
	}
	if err := checkText("detail", in.Detail, api.MaxControlDetailRunes); err != nil {
		return api.Message{}, err
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Message{}, err
	}
	defer tx.Rollback()
	if err := expireMessagesTx(ctx, tx, now); err != nil {
		return api.Message{}, err
	}
	var owner int64
	var cur, sessionID, sender, text string
	err = tx.QueryRowContext(ctx, `SELECT machine_id, state, session_id, sender, text FROM messages WHERE id = ?`, id).
		Scan(&owner, &cur, &sessionID, &sender, &text)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Message{}, fmt.Errorf("%w: message %s", ErrNotFound, id)
	}
	if err != nil {
		return api.Message{}, err
	}
	if owner != machineID {
		return api.Message{}, fmt.Errorf("%w: message %s belongs to another machine", ErrWrongMachine, id)
	}
	if cur != api.MessageQueued {
		return api.Message{}, fmt.Errorf("%w: message %s is %s, not queued", ErrRequestClosed, id, cur)
	}
	nowS := formatTS(now)
	if state == api.MessageBusy {
		_, err = tx.ExecContext(ctx, `UPDATE messages SET detail = ?, updated_at = ? WHERE id = ?`, in.Detail, nowS, id)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE messages SET state = ?, detail = ?, updated_at = ? WHERE id = ?`, state, in.Detail, nowS, id)
	}
	if err != nil {
		return api.Message{}, err
	}
	if state == api.MessageDelivered {
		if err := messageEventTx(ctx, tx, sessionID, id, sender, state, text, now); err != nil {
			return api.Message{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return api.Message{}, err
	}
	return s.GetMessage(ctx, id)
}

// GetMessage reads one message. A queued message past MessageTTL reads as
// expired, whether or not a write stored that yet.
func (s *Store) GetMessage(ctx context.Context, id string) (api.Message, error) {
	if !ValidMessageID(id) {
		return api.Message{}, invalidf("message id %q: want msg_ and 22 base64url characters", id)
	}
	var m api.Message
	var created, updated string
	err := s.db.QueryRowContext(ctx, `SELECT m.id, m.session_id, mc.name, m.sender, m.text, m.state, m.detail,
		m.created_at, m.updated_at FROM messages m JOIN machines mc ON mc.id = m.machine_id WHERE m.id = ?`, id).
		Scan(&m.ID, &m.SessionID, &m.Machine, &m.Sender, &m.Text, &m.State, &m.Detail, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Message{}, fmt.Errorf("%w: message %s", ErrNotFound, id)
	}
	if err != nil {
		return api.Message{}, err
	}
	if m.CreatedAt, err = parseTS(created); err != nil {
		return api.Message{}, err
	}
	if m.UpdatedAt, err = parseTS(updated); err != nil {
		return api.Message{}, err
	}
	if m.State == api.MessageQueued && !s.Now().Before(m.CreatedAt.Add(MessageTTL)) {
		m.State = api.MessageExpired
	}
	return m, nil
}
```

The rate limit counts stored messages and every distinct target of the new
send, refused or not, so a send's cost is known before its targets are
checked.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/ -count=1`
Expected: PASS.

- [ ] **Step 5: Document it**

In `docs/server.md`, after the "Remote Control requests" section (before
"### Inbox"), add:

```markdown
### Messages to sessions

`POST /v1/messages` with `{"session_ids": [...], "text": "..."}` queues one
message per distinct session, 1 to 20 sessions, text 1 to 4,000 characters
(newlines and tabs allowed, no other control characters). A machine token, or
the session cookie with `X-Hub-Action: send`, may send. The response is `200`
with one result per target, in order:

    {"results": [{"session_id": "...", "id": "msg_...", "state": "queued"},
                 {"session_id": "...", "state": "refused", "detail": "the session is not in herdr"}]}

A target is refused when the session is unknown, has ended, has no herdr pane,
or its machine's watcher has not polled in the last 2 minutes. The other
targets still go.

The sender is `dashboard` for a browser, `cli on <machine>` for a machine
token, and `session <first 8 characters>` when a machine token sends
`from_session` (the MCP tool does). Each sender may send 30 messages (one per
target) in any 60 seconds; a send past that is `429` as a whole.

The watcher submits `From the user via sessionhub (<sender>):`, a blank line, and the
text. It gets the message from its long poll (`GET
/v1/machines/self/control`) as a claim whose `request.action` is `message`,
with the prompt in `text`, and answers on `POST /v1/control/{id}/result` with
`state` `delivered`, `refused` (with `detail`), or `busy` (the agent is
working or blocked; the message stays queued and is offered again 15 seconds
after the last offer). A `failed` result counts as `refused`. A message not
delivered within 10 minutes is `expired`.

`GET /v1/messages/{id}` (read access) returns the message: `id`,
`session_id`, `machine`, `sender`, `text`, `state`, `detail`, `created_at`,
`updated_at`.

A delivered or expired message adds a `message` event to its session, with
`message_id`, `sender`, `state`, and the first 200 characters of the text.
```

- [ ] **Step 6: Run the full suite and lint**

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1 && make lint`
Expected: every package `ok`, lint clean.

- [ ] **Step 7: Commit**

```bash
git add internal/store/messages.go internal/store/messages_test.go docs/server.md
git commit -m "$(cat <<'EOF'
Queue messages to sessions in the store

A send queues one message per distinct live session in herdr whose
watcher is polling, and refuses the rest with a reason. Watchers claim
queued messages like control requests; a busy result keeps a message
queued and offers it again 15 seconds later, and ten minutes without
delivery expires it. Each sender may send 30 messages a minute.

Refs: docs/dev/superpowers/plans/2026-10-02-session-actions.md, Task 2

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 3: Store: permission requests and the inbox

**Files:**
- Modify: `internal/api/actions.go` (`MaxToolInputBytes`, `CapToolInput`)
- Modify: `internal/api/actions_test.go`
- Create: `internal/store/permissions.go`
- Create: `internal/store/permissions_test.go`
- Modify: `internal/store/sessions.go` (`upsertTx`, `AddEvent`,
  `ReconcileHerdr`: answered locally and closed)
- Modify: `internal/store/inbox.go` (`Inbox` attaches the open request)
- Modify: `docs/server.md` (new "Permission requests" section, Inbox
  section)

**Interfaces:**
- Consumes: `api.PermissionIn`, `api.PermissionRequest`, `api.DecisionIn`,
  `api.PermissionInputText`, the `api.Permission*` and `api.Decision*`
  constants, `ErrGone` (Task 1); `newRandomID`, `firstRunes`,
  `eventTextRunes` (Task 2); `checkOwnerTx`, `checkText`, `insertEventTx`,
  `scanner`, `controlEnv.snapshot`, `controlEnv.setStateAt`,
  `controlEnv.readInbox`, `controlEnv.inboxFor`.
- Produces:
  - `const api.MaxToolInputBytes = 8 << 10`
  - `func api.CapToolInput(input json.RawMessage) (json.RawMessage, bool, error)`
  - constants `PermissionTTL` (10 minutes), `MaxReasonRunes` (200),
    `PermissionGrace` (5 seconds)
  - `func ValidPermissionID(id string) bool`
  - `func (s *Store) CreatePermission(ctx context.Context, machineID int64, sessionID string, in api.PermissionIn) (api.PermissionRequest, error)`
  - `func (s *Store) GetPermission(ctx context.Context, id string) (api.PermissionRequest, error)`
  - `func (s *Store) PermissionFor(ctx context.Context, machineID int64, id string) (api.PermissionRequest, error)`
    (the owning machine's read; another machine gets `ErrWrongMachine`)
  - `func (s *Store) DecidePermission(ctx context.Context, id string, in api.DecisionIn, by string) (api.PermissionRequest, error)`
  - `func closePermissionsTx(ctx context.Context, tx *sql.Tx, sessionID, state string, upTo, now time.Time) error`

- [ ] **Step 1: Write the failing API test**

Add to `internal/api/actions_test.go` (and add `"strings"` and
`"unicode/utf8"` to its imports):

```go
func TestCapToolInput(t *testing.T) {
	out, cut, err := CapToolInput(json.RawMessage(`{ "command" : "ls" }`))
	if err != nil || cut || string(out) != `{"command":"ls"}` {
		t.Errorf("small input: %s %v %v", out, cut, err)
	}
	for _, empty := range []string{``, `null`, `  `} {
		if out, cut, err := CapToolInput(json.RawMessage(empty)); err != nil || cut || string(out) != `{}` {
			t.Errorf("input %q: %s %v %v, want {}", empty, out, cut, err)
		}
	}
	if _, _, err := CapToolInput(json.RawMessage(`{"a":`)); err == nil {
		t.Error("broken JSON accepted")
	}
	big := `{"file_path":"/tmp/x","content":"` + strings.Repeat("é", 6000) + `"}`
	out, cut, err = CapToolInput(json.RawMessage(big))
	if err != nil || !cut {
		t.Fatalf("big input: cut=%v err=%v", cut, err)
	}
	var s string
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("a cut input is not a JSON string: %v", err)
	}
	if !strings.HasPrefix(s, `{"file_path":"/tmp/x","content":"éé`) || !strings.HasSuffix(s, "…") || !utf8.ValidString(s) {
		t.Errorf("cut input starts %.40q, ends %q", s, s[len(s)-6:])
	}
	if n := len(s) - len("…"); n > 8000 || n < 7997 {
		t.Errorf("kept %d bytes, want 7997 to 8000", n)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/api/ -count=1`
Expected: FAIL to compile with `undefined: CapToolInput`.

- [ ] **Step 3: Add `CapToolInput`**

In `internal/api/actions.go`, add `"unicode/utf8"` to the imports and add at
the end of the file:

```go
// MaxToolInputBytes caps a permission request's tool input, as compact JSON.
const MaxToolInputBytes = 8 << 10

// cutToolInputBytes is how much of a bigger input is kept.
const cutToolInputBytes = 8000

// CapToolInput compacts a tool input. An empty or null input is {}. An
// input over MaxToolInputBytes becomes a JSON string holding its first
// 8,000 bytes (cut on a rune boundary) and "…", and cut is true. Both the
// hook and the server apply it, so a big input never passes the server's
// 64 KiB body limit.
func CapToolInput(input json.RawMessage) (out json.RawMessage, cut bool, err error) {
	in := bytes.TrimSpace(input)
	if len(in) == 0 || string(in) == "null" {
		return json.RawMessage(`{}`), false, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, in); err != nil {
		return nil, false, err
	}
	if buf.Len() <= MaxToolInputBytes {
		return json.RawMessage(buf.Bytes()), false, nil
	}
	kept := buf.Bytes()[:cutToolInputBytes]
	for len(kept) > 0 && !utf8.Valid(kept) {
		kept = kept[:len(kept)-1]
	}
	s, err := json.Marshal(string(kept) + "…")
	if err != nil {
		return nil, false, err
	}
	return json.RawMessage(s), true, nil
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/api/ -count=1`
Expected: PASS.

- [ ] **Step 5: Write the failing store tests**

Create `internal/store/permissions_test.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func (e *controlEnv) permission(id, tool, input string) api.PermissionRequest {
	e.t.Helper()
	p, err := e.s.CreatePermission(context.Background(), e.tower.ID, id, api.PermissionIn{ToolName: tool, ToolInput: json.RawMessage(input)})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func TestCreatePermission(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	p := e.permission("s1", "Bash", `{ "command": "git push", "description": "Push" }`)
	if !ValidPermissionID(p.ID) || p.SessionID != "s1" || p.Machine != "tower" || p.ToolName != "Bash" ||
		string(p.ToolInput) != `{"command":"git push","description":"Push"}` || p.Truncated || p.State != api.PermissionOpen ||
		!p.CreatedAt.Equal(e.clock.Now()) || !p.ExpiresAt.Equal(e.clock.Now().Add(PermissionTTL)) || p.DecidedAt != nil {
		t.Errorf("created %+v", p)
	}
	got, err := e.s.GetPermission(ctx, p.ID)
	if err != nil || got.ID != p.ID || got.State != api.PermissionOpen {
		t.Errorf("GetPermission: %+v %v", got, err)
	}
	if _, err := e.s.PermissionFor(ctx, e.bluebox.ID, p.ID); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("bluebox reads tower's request: %v, want ErrWrongMachine", err)
	}
	if _, err := e.s.CreatePermission(ctx, e.bluebox.ID, "s1", api.PermissionIn{ToolName: "Bash"}); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("bluebox creates for tower's session: %v, want ErrWrongMachine", err)
	}
	if _, err := e.s.CreatePermission(ctx, e.tower.ID, "nope", api.PermissionIn{ToolName: "Bash"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v, want ErrNotFound", err)
	}
	for name, in := range map[string]api.PermissionIn{
		"no tool":      {ToolName: " "},
		"tool newline": {ToolName: "Ba\nsh"},
		"long tool":    {ToolName: strings.Repeat("x", 129)},
		"broken input": {ToolName: "Bash", ToolInput: json.RawMessage(`{"a":`)},
	} {
		if _, err := e.s.CreatePermission(ctx, e.tower.ID, "s1", in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	big := e.permission("s1", "Write", `{"file_path":"/x","content":"`+strings.Repeat("a", 9000)+`"}`)
	if !big.Truncated || api.PermissionInputText(big.ToolInput) == "" || len(big.ToolInput) > 8100 {
		t.Errorf("big input: truncated=%v, %d bytes", big.Truncated, len(big.ToolInput))
	}
	if _, err := e.s.GetPermission(ctx, "pr_AAAAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown request: %v, want ErrNotFound", err)
	}
	if _, err := e.s.GetPermission(ctx, "msg_AAAAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a message ID: %v, want ErrInvalid", err)
	}
}

func TestDecidePermissionOnce(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	a := e.permission("s1", "Bash", `{"command":"git push"}`)
	e.clock.Advance(time.Second)
	got, err := e.s.DecidePermission(ctx, a.ID, api.DecisionIn{Decision: api.DecisionAllow, Reason: "ignored"}, "web:phone")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != api.PermissionDecided || got.Decision != api.DecisionAllow || got.Reason != "" || got.DecidedBy != "web:phone" ||
		got.DecidedAt == nil || !got.DecidedAt.Equal(e.clock.Now()) {
		t.Errorf("decided %+v", got)
	}
	if _, err := e.s.DecidePermission(ctx, a.ID, api.DecisionIn{Decision: api.DecisionDeny}, "machine:tower"); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("second decision: %v, want ErrRequestClosed", err)
	}
	b := e.permission("s1", "Bash", `{"command":"rm -rf build"}`)
	got, err = e.s.DecidePermission(ctx, b.ID, api.DecisionIn{Decision: api.DecisionDeny, Reason: "  use make clean  "}, "machine:bluebox")
	if err != nil || got.Decision != api.DecisionDeny || got.Reason != "use make clean" {
		t.Errorf("deny: %+v %v", got, err)
	}
	evs := e.events("s1", api.KindPermission)
	if len(evs) != 2 {
		t.Fatalf("%d permission events, want 2", len(evs))
	}
	var p map[string]string
	json.Unmarshal(evs[0].Payload, &p) // newest first: the deny
	if p["request_id"] != b.ID || p["tool"] != "Bash" || p["input"] != "rm -rf build" || p["decision"] != "deny" ||
		p["reason"] != "use make clean" || p["by"] != "machine:bluebox" || evs[0].Source != api.SourceServer {
		t.Errorf("event %v from %s", p, evs[0].Source)
	}
	c := e.permission("s1", "Bash", `{"command":"x"}`)
	for name, in := range map[string]api.DecisionIn{
		"maybe":          {Decision: "maybe"},
		"reason newline": {Decision: api.DecisionDeny, Reason: "a\nb"},
		"long reason":    {Decision: api.DecisionDeny, Reason: strings.Repeat("r", MaxReasonRunes+1)},
	} {
		if _, err := e.s.DecidePermission(ctx, c.ID, in, "web:phone"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := e.s.DecidePermission(ctx, c.ID, api.DecisionIn{Decision: api.DecisionAllow}, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("no decider: %v, want ErrInvalid", err)
	}
	if _, err := e.s.DecidePermission(ctx, "pr_AAAAAAAAAAAAAAAAAAAAAA", api.DecisionIn{Decision: api.DecisionAllow}, "web:phone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown request: %v, want ErrNotFound", err)
	}
}

func TestPermissionExpiry(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	p := e.permission("s1", "Bash", `{"command":"x"}`)
	e.clock.Advance(PermissionTTL)
	got, err := e.s.PermissionFor(ctx, e.tower.ID, p.ID)
	if err != nil || got.State != api.PermissionExpired {
		t.Errorf("after TTL: %+v %v, want expired", got, err)
	}
	if _, err := e.s.DecidePermission(ctx, p.ID, api.DecisionIn{Decision: api.DecisionAllow}, "web:phone"); !errors.Is(err, ErrGone) {
		t.Errorf("decide after TTL: %v, want ErrGone", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM permission_requests WHERE state = 'expired'`); n != 1 {
		t.Errorf("stored expired rows %d, want 1", n)
	}
}

func TestPermissionAnsweredLocally(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	e.setState("s1", "blocked")
	e.clock.Advance(time.Second)
	p := e.permission("s1", "Bash", `{"command":"x"}`)
	e.clock.Advance(time.Second)
	e.setState("s1", "working")
	got, _ := e.s.GetPermission(ctx, p.ID)
	if got.State != api.PermissionAnsweredLocally {
		t.Fatalf("after leaving blocked: %s, want answered_locally", got.State)
	}
	if _, err := e.s.DecidePermission(ctx, p.ID, api.DecisionIn{Decision: api.DecisionAllow}, "web:phone"); !errors.Is(err, ErrGone) {
		t.Errorf("decide after a local answer: %v, want ErrGone", err)
	}
	// A state change that does not leave blocked closes nothing.
	q := e.permission("s1", "Bash", `{"command":"y"}`)
	e.setState("s1", "idle")
	if got, _ := e.s.GetPermission(ctx, q.ID); got.State != api.PermissionOpen {
		t.Errorf("idle after working closed a request: %s", got.State)
	}
}

func TestPermissionAnsweredLocallyKeepsLaterRequest(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	t0 := e.clock.Now()
	e.setStateAt("s1", "blocked", t0)
	e.clock.Advance(2 * time.Second)
	p := e.permission("s1", "Bash", `{"command":"second prompt"}`)
	// The working event for the first prompt's answer arrives late, stamped
	// before the second request.
	e.setStateAt("s1", "working", t0.Add(time.Second))
	if got, _ := e.s.GetPermission(ctx, p.ID); got.State != api.PermissionOpen {
		t.Errorf("a late event closed a newer request: %s", got.State)
	}
}

func TestPermissionSnapshotPath(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.snapshot("s1", "blocked")
	p := e.permission("s1", "Bash", `{"command":"x"}`)
	e.clock.Advance(time.Second)
	// The snapshot may have been built before the request: within the
	// grace it leaves the request open.
	e.snapshot("s1", "working")
	if got, _ := e.s.GetPermission(ctx, p.ID); got.State != api.PermissionOpen {
		t.Fatalf("a snapshot within the grace closed the request: %s", got.State)
	}
	e.snapshot("s1", "blocked")
	e.clock.Advance(PermissionGrace + time.Second)
	e.snapshot("s1", "working")
	if got, _ := e.s.GetPermission(ctx, p.ID); got.State != api.PermissionAnsweredLocally {
		t.Errorf("after the grace: %s, want answered_locally", got.State)
	}
}

func TestPermissionClosedWithSession(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	p := e.permission("s1", "Bash", `{"command":"x"}`)
	if err := e.s.AddEvent(ctx, e.tower.ID, "s1", api.EventIn{Kind: api.KindPaneClosed, Source: api.SourcePlugin}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.GetPermission(ctx, p.ID); got.State != api.PermissionClosed {
		t.Errorf("after pane_closed: %s, want closed", got.State)
	}
	// Missing from a snapshot ends the session too.
	e.snapshot("s2", "blocked")
	q := e.permission("s2", "Bash", `{"command":"y"}`)
	if _, err := e.s.ReconcileHerdr(ctx, e.tower.ID, api.HerdrSessionsPut{HerdrSession: "default"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.GetPermission(ctx, q.ID); got.State != api.PermissionClosed {
		t.Errorf("after a snapshot without it: %s, want closed", got.State)
	}
}

func TestInboxCarriesPermission(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	e.setState("s1", "blocked")
	if it := e.inboxFor("s1"); it == nil || it.Permission != nil {
		t.Fatalf("blocked item without a request: %+v", it)
	}
	e.permission("s1", "Bash", `{"command":"first"}`)
	e.clock.Advance(time.Second)
	newest := e.permission("s1", "Bash", `{"command":"second"}`)
	it := e.inboxFor("s1")
	if it == nil || it.Permission == nil || it.Permission.ID != newest.ID {
		t.Fatalf("inbox item %+v, want the newest request", it)
	}
	if _, err := e.s.DecidePermission(ctx, newest.ID, api.DecisionIn{Decision: api.DecisionAllow}, "web:phone"); err != nil {
		t.Fatal(err)
	}
	if it := e.inboxFor("s1"); it == nil || it.Permission == nil || api.PermissionInputText(it.Permission.ToolInput) != "first" {
		t.Errorf("after deciding the newest, the item shows %+v, want the first", it)
	}
	// A waiting item never carries one.
	e.session(e.tower, "s2", "w1:p2")
	e.permission("s2", "Bash", `{"command":"z"}`)
	e.sendReport("s2", "pick a name")
	if it := e.inboxFor("s2"); it == nil || it.Group != api.InboxWaiting || it.Permission != nil {
		t.Errorf("waiting item %+v", it)
	}
}
```

`controlEnv.sendReport` posts for tower, and `controlEnv.snapshot` sends a
tower herdr snapshot with the one session in pane `p1`; both exist already.

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./internal/store/ -run 'Permission|TestInboxCarriesPermission' -count=1`
Expected: FAIL to compile with `e.s.CreatePermission undefined`.

- [ ] **Step 7: Write the permission queries**

Create `internal/store/permissions.go`:

```go
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// Permission request limits. See docs/server.md, "Permission requests".
const (
	// PermissionTTL is how long a request stays open. The hook waits as
	// long, inside its 660-second hook timeout.
	PermissionTTL = 10 * time.Minute
	// MaxReasonRunes caps a deny reason.
	MaxReasonRunes = 200
	// PermissionGrace: a herdr snapshot that shows a session out of
	// blocked leaves a request this recent open, because the snapshot may
	// have been built before the request arrived.
	PermissionGrace = 5 * time.Second
	maxToolNameRunes = 128
	maxDecidedByRunes = 100
)

var permissionIDRE = regexp.MustCompile(`^pr_[A-Za-z0-9_-]{22}$`)

// ValidPermissionID reports whether id has the shape of a permission
// request ID.
func ValidPermissionID(id string) bool { return permissionIDRE.MatchString(id) }

// permissionSelect reads a request, its machine's name, and its machine ID.
const permissionSelect = `SELECT p.id, p.session_id, m.name, p.tool_name, p.tool_input, p.truncated, p.state,
	p.decision, p.reason, p.decided_by, p.created_at, p.expires_at, p.decided_at, p.machine_id
FROM permission_requests p JOIN machines m ON m.id = p.machine_id`

// scanPermission reads one permissionSelect row. An open request past
// expires_at reads as expired.
func scanPermission(sc scanner, now time.Time) (api.PermissionRequest, int64, error) {
	var p api.PermissionRequest
	var input, created, expires string
	var truncated int
	var decided sql.NullString
	var machineID int64
	if err := sc.Scan(&p.ID, &p.SessionID, &p.Machine, &p.ToolName, &input, &truncated, &p.State,
		&p.Decision, &p.Reason, &p.DecidedBy, &created, &expires, &decided, &machineID); err != nil {
		return p, 0, err
	}
	p.ToolInput = json.RawMessage(input)
	p.Truncated = truncated != 0
	var err error
	if p.CreatedAt, err = parseTS(created); err != nil {
		return p, 0, err
	}
	if p.ExpiresAt, err = parseTS(expires); err != nil {
		return p, 0, err
	}
	if p.DecidedAt, err = parseNullTS(decided); err != nil {
		return p, 0, err
	}
	if p.State == api.PermissionOpen && !now.Before(p.ExpiresAt) {
		p.State = api.PermissionExpired
	}
	return p, machineID, nil
}

// expirePermissionsTx stores the expiry of every open request past
// expires_at.
func expirePermissionsTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	n := formatTS(now)
	_, err := tx.ExecContext(ctx, `UPDATE permission_requests SET state = ?, updated_at = ?
		WHERE state = ? AND expires_at <= ?`, api.PermissionExpired, n, api.PermissionOpen, n)
	return err
}

// closePermissionsTx moves session id's open requests created at or before
// upTo to state: answered_locally when the session left blocked, closed
// when it ended.
func closePermissionsTx(ctx context.Context, tx *sql.Tx, sessionID, state string, upTo, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE permission_requests SET state = ?, updated_at = ?
		WHERE session_id = ? AND state = ? AND created_at <= ?`,
		state, formatTS(now), sessionID, api.PermissionOpen, formatTS(upTo))
	return err
}

// CreatePermission opens a permission request for session id, which
// machineID must own. The tool input is capped with api.CapToolInput; cwd
// and suggestions are not stored.
func (s *Store) CreatePermission(ctx context.Context, machineID int64, sessionID string, in api.PermissionIn) (api.PermissionRequest, error) {
	if !ValidSessionID(sessionID) {
		return api.PermissionRequest{}, invalidf("session id %q: use 1-128 letters, digits, '_', or '-', starting with a letter or digit", sessionID)
	}
	tool := strings.TrimSpace(in.ToolName)
	if tool == "" {
		return api.PermissionRequest{}, invalidf("tool_name is empty")
	}
	if err := checkText("tool_name", tool, maxToolNameRunes); err != nil {
		return api.PermissionRequest{}, err
	}
	input, cut, err := api.CapToolInput(in.ToolInput)
	if err != nil {
		return api.PermissionRequest{}, invalidf("tool_input: %v", err)
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.PermissionRequest{}, err
	}
	defer tx.Rollback()
	if err := checkOwnerTx(ctx, tx, machineID, sessionID); err != nil {
		return api.PermissionRequest{}, err
	}
	id, err := newRandomID("pr_")
	if err != nil {
		return api.PermissionRequest{}, err
	}
	truncated := 0
	if cut {
		truncated = 1
	}
	nowS := formatTS(now)
	if _, err := tx.ExecContext(ctx, `INSERT INTO permission_requests (id, session_id, machine_id, tool_name, tool_input,
		truncated, state, created_at, expires_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sessionID, machineID, tool, string(input), truncated, api.PermissionOpen, nowS,
		formatTS(now.Add(PermissionTTL)), nowS); err != nil {
		return api.PermissionRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.PermissionRequest{}, err
	}
	return s.GetPermission(ctx, id)
}

// GetPermission reads one request.
func (s *Store) GetPermission(ctx context.Context, id string) (api.PermissionRequest, error) {
	p, _, err := s.permission(ctx, id)
	return p, err
}

// PermissionFor reads one request for the machine that owns it.
func (s *Store) PermissionFor(ctx context.Context, machineID int64, id string) (api.PermissionRequest, error) {
	p, owner, err := s.permission(ctx, id)
	if err != nil {
		return p, err
	}
	if owner != machineID {
		return api.PermissionRequest{}, fmt.Errorf("%w: request %s belongs to another machine", ErrWrongMachine, id)
	}
	return p, nil
}

func (s *Store) permission(ctx context.Context, id string) (api.PermissionRequest, int64, error) {
	if !ValidPermissionID(id) {
		return api.PermissionRequest{}, 0, invalidf("request id %q: want pr_ and 22 base64url characters", id)
	}
	p, owner, err := scanPermission(s.db.QueryRowContext(ctx, permissionSelect+` WHERE p.id = ?`, id), s.Now())
	if errors.Is(err, sql.ErrNoRows) {
		return api.PermissionRequest{}, 0, fmt.Errorf("%w: request %s", ErrNotFound, id)
	}
	return p, owner, err
}

// DecidePermission records the one decision for an open request: allow or
// deny, with a reason for a deny. by is web:<name> or machine:<name>. A
// decided request is ErrRequestClosed; an expired, answered-locally, or
// closed one is ErrGone.
func (s *Store) DecidePermission(ctx context.Context, id string, in api.DecisionIn, by string) (api.PermissionRequest, error) {
	if !ValidPermissionID(id) {
		return api.PermissionRequest{}, invalidf("request id %q: want pr_ and 22 base64url characters", id)
	}
	if in.Decision != api.DecisionAllow && in.Decision != api.DecisionDeny {
		return api.PermissionRequest{}, invalidf("decision %q: want allow or deny", in.Decision)
	}
	reason := strings.TrimSpace(in.Reason)
	if in.Decision == api.DecisionAllow {
		reason = "" // only a deny carries a reason
	}
	if err := checkText("reason", reason, MaxReasonRunes); err != nil {
		return api.PermissionRequest{}, err
	}
	if by == "" {
		return api.PermissionRequest{}, invalidf("decided_by is empty")
	}
	if err := checkText("decided_by", by, maxDecidedByRunes); err != nil {
		return api.PermissionRequest{}, err
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.PermissionRequest{}, err
	}
	defer tx.Rollback()
	if err := expirePermissionsTx(ctx, tx, now); err != nil {
		return api.PermissionRequest{}, err
	}
	var state, sessionID, tool, input string
	err = tx.QueryRowContext(ctx, `SELECT state, session_id, tool_name, tool_input FROM permission_requests WHERE id = ?`, id).
		Scan(&state, &sessionID, &tool, &input)
	if errors.Is(err, sql.ErrNoRows) {
		return api.PermissionRequest{}, fmt.Errorf("%w: request %s", ErrNotFound, id)
	}
	if err != nil {
		return api.PermissionRequest{}, err
	}
	switch state {
	case api.PermissionOpen:
	case api.PermissionDecided:
		return api.PermissionRequest{}, fmt.Errorf("%w: request %s is already decided", ErrRequestClosed, id)
	default:
		return api.PermissionRequest{}, fmt.Errorf("%w: request %s is %s", ErrGone, id, strings.ReplaceAll(state, "_", " "))
	}
	nowS := formatTS(now)
	if _, err := tx.ExecContext(ctx, `UPDATE permission_requests SET state = ?, decision = ?, reason = ?, decided_by = ?,
		decided_at = ?, updated_at = ? WHERE id = ?`, api.PermissionDecided, in.Decision, reason, by, nowS, nowS, id); err != nil {
		return api.PermissionRequest{}, err
	}
	payload, err := json.Marshal(map[string]string{"request_id": id, "tool": tool,
		"input": firstRunes(api.PermissionInputText(json.RawMessage(input)), eventTextRunes),
		"decision": in.Decision, "reason": reason, "by": by})
	if err != nil {
		return api.PermissionRequest{}, err
	}
	if err := insertEventTx(ctx, tx, sessionID, now, api.SourceServer, api.KindPermission, payload); err != nil {
		return api.PermissionRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.PermissionRequest{}, err
	}
	return s.GetPermission(ctx, id)
}

// openPermissions maps each session with an open, unexpired request to its
// newest one.
func (s *Store) openPermissions(ctx context.Context) (map[string]api.PermissionRequest, error) {
	now := s.Now()
	rows, err := s.db.QueryContext(ctx, permissionSelect+` WHERE p.state = ? AND p.expires_at > ?
		ORDER BY p.created_at, p.rowid`, api.PermissionOpen, formatTS(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]api.PermissionRequest{}
	for rows.Next() {
		p, _, err := scanPermission(rows, now)
		if err != nil {
			return nil, err
		}
		out[p.SessionID] = p // later rows are newer
	}
	return out, rows.Err()
}
```

- [ ] **Step 8: Close requests on the state paths**

In `internal/store/sessions.go` (`upsertTx`), replace:

```go
	if u.Source == api.SourcePlugin && prevState == "done" && u.AgentState == "idle" {
```

with:

```go
	if prevState == "blocked" && u.AgentState != "" && u.AgentState != "blocked" {
		// The prompt was answered in the terminal. The snapshot may predate
		// a request that just arrived, so the newest ones stay open.
		if err := closePermissionsTx(ctx, tx, u.ID, api.PermissionAnsweredLocally, now.Add(-PermissionGrace), now); err != nil {
			return false, err
		}
	}
	if u.Source == api.SourcePlugin && prevState == "done" && u.AgentState == "idle" {
```

In `AddEvent`, replace:

```go
	seen := false // a plugin event moves the state from done to idle
```

with:

```go
	seen := false        // a plugin event moves the state from done to idle
	leftBlocked := false // the event moves the state out of blocked
```

replace:

```go
			seen = e.Source == api.SourcePlugin && prev == "done" && state == "idle"
```

with:

```go
			seen = e.Source == api.SourcePlugin && prev == "done" && state == "idle"
			leftBlocked = prev == "blocked" && state != "blocked"
```

and replace:

```go
	if seen {
		// herdr marked the pane seen: its Finished item is read.
		if err := s.autoDismissSeenTx(ctx, tx, id, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
```

with:

```go
	if seen {
		// herdr marked the pane seen: its Finished item is read.
		if err := s.autoDismissSeenTx(ctx, tx, id, now); err != nil {
			return err
		}
	}
	if leftBlocked {
		// The prompt was answered in the terminal. Only requests from
		// before this event's own time close: a newer one is a new prompt.
		if err := closePermissionsTx(ctx, tx, id, api.PermissionAnsweredLocally, ts, now); err != nil {
			return err
		}
	}
	if ended != "NULL" {
		if err := closePermissionsTx(ctx, tx, id, api.PermissionClosed, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
```

In `ReconcileHerdr`, replace:

```go
		if err := insertEventTx(ctx, tx, id, now, api.SourcePlugin, api.KindEnded, []byte(missingPayload)); err != nil {
			return res, err
		}
		res.Ended = append(res.Ended, id)
```

with:

```go
		if err := insertEventTx(ctx, tx, id, now, api.SourcePlugin, api.KindEnded, []byte(missingPayload)); err != nil {
			return res, err
		}
		if err := closePermissionsTx(ctx, tx, id, api.PermissionClosed, now, now); err != nil {
			return res, err
		}
		res.Ended = append(res.Ended, id)
```

- [ ] **Step 9: Attach the open request to the inbox**

In `internal/store/inbox.go` (`Inbox`), replace:

```go
	list, err := s.ListSessions(ctx, ListFilter{})
	if err != nil {
		return out, err
	}
	now := s.Now()
```

with:

```go
	list, err := s.ListSessions(ctx, ListFilter{})
	if err != nil {
		return out, err
	}
	perms, err := s.openPermissions(ctx)
	if err != nil {
		return out, err
	}
	now := s.Now()
```

and replace:

```go
		item.Session = x
		out.Items = append(out.Items, item)
```

with:

```go
		item.Session = x
		if p, ok := perms[x.ID]; ok && item.Group == api.InboxBlocked {
			item.Permission = &p
		}
		out.Items = append(out.Items, item)
```

- [ ] **Step 10: Run the tests to verify they pass**

Run: `go test ./internal/store/ ./internal/api/ -count=1`
Expected: PASS, including every existing inbox, auto-dismiss, and
reconcile test.

- [ ] **Step 11: Document it**

In `docs/server.md`, after the "Messages to sessions" section, add:

```markdown
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
closed request is `410`. Each decision adds a `permission` event to the
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
```

In section "Inbox", after the bullet that starts "An item leaves on its own",
add:

```markdown
- A **Blocked** item carries `permission`, the session's newest open
  permission request, when it has one. See "Permission requests".
```

- [ ] **Step 12: Run the full suite and lint**

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1 && make lint`
Expected: every package `ok`, lint clean.

- [ ] **Step 13: Commit**

```bash
git add internal/api/actions.go internal/api/actions_test.go internal/store/permissions.go \
  internal/store/permissions_test.go internal/store/sessions.go internal/store/inbox.go docs/server.md
git commit -m "$(cat <<'EOF'
Store permission requests and show the open one in the inbox

A request is open for ten minutes and takes one decision, allow or
deny. A session that leaves blocked closes its requests as answered
locally, up to the state change's own time, so a late event never
closes a newer prompt; an ended session closes them. A blocked inbox
item carries the session's newest open request.

Refs: docs/dev/superpowers/plans/2026-10-02-session-actions.md, Task 3

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 4: Server: routes, long polls, and auth

**Files:**
- Modify: `internal/server/routes.go` (three access levels, eight routes)
- Modify: `internal/server/server.go` (`storeError`: `ErrFull`, `ErrGone`;
  `Server.permCheck`)
- Modify: `internal/server/control.go` (`acquireMax`, `pollWait`, message
  claims in `pollControl`, message results in `postControlResult`)
- Create: `internal/server/actions.go` (the new handlers)
- Create: `internal/server/actions_test.go`
- Modify: `internal/server/helpers_test.go` (`actionRows` in `snapshot`,
  `getAsync`)
- Modify: `internal/server/auth_test.go` (cases, credentials, `expect`)
- Modify: `docs/server.md` (Authentication, Endpoints)

**Interfaces:**
- Consumes: every store method from Tasks 1 to 3; `api.InstructionIn`,
  `api.MessagesIn`, `api.MessagesOut`, `api.PermissionIn`,
  `api.DecisionIn`; `controlHub`, `decode`, `pathID`, `writeJSON`,
  `writeError`; test helpers `newEnv`, `env.doHdr`, `env.controllable`,
  `env.event`, `fakeTimers`, `pollResult`, `recv`, `instantTimer`.
- Produces:
  - access levels `accessInstructions`, `accessSend`, `accessApprove`
  - `func (h *controlHub) acquireMax(key string, n int) bool`
  - `func pollWait(w http.ResponseWriter, r *http.Request) (time.Duration, bool)`
  - handlers `listInstructions`, `addInstruction`, `deleteInstruction`,
    `postMessages`, `getMessage`, `postPermission`, `waitDecision`,
    `decidePermission`
  - `Server.permCheck time.Duration` (default 1 second): how often an open
    decision poll rereads its request
  - test helpers `func (e *env) actionRows() string`,
    `func (e *env) getAsync(token, path string) <-chan pollResult`

- [ ] **Step 1: Write the failing route tests**

Add to `internal/server/helpers_test.go`, after `triageRows`:

```go
// actionRows dumps the instructions, messages, and permission_requests
// tables over a second connection, so the auth matrix sees a rejected
// request that wrote only there.
func (e *env) actionRows() string {
	e.t.Helper()
	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	var b strings.Builder
	for _, q := range []string{
		`SELECT 'rule|' || id || '|' || text || '|' || created_by FROM instructions ORDER BY id`,
		`SELECT 'msg|' || id || '|' || session_id || '|' || state || '|' || detail || '|' || COALESCE(offered_at, '') FROM messages ORDER BY id`,
		`SELECT 'perm|' || id || '|' || state || '|' || decision || '|' || decided_by FROM permission_requests ORDER BY id`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			e.t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				e.t.Fatal(err)
			}
			b.WriteString(line + "\n")
		}
		rows.Close()
	}
	return b.String()
}

// getAsync sends GET path with the token in a goroutine. It never calls
// t.Fatal, which is only allowed on the test's own goroutine.
func (e *env) getAsync(token, path string) <-chan pollResult {
	ch := make(chan pollResult, 1)
	go func() {
		req, err := http.NewRequest("GET", e.srv.URL+path, nil)
		if err != nil {
			ch <- pollResult{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			ch <- pollResult{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		ch <- pollResult{code: resp.StatusCode, body: b, err: err}
	}()
	return ch
}
```

and in `snapshot`, replace:

```go
	return string(b) + "\ninbox_triage:\n" + e.triageRows()
```

with:

```go
	return string(b) + "\ninbox_triage:\n" + e.triageRows() + "actions:\n" + e.actionRows()
```

Create `internal/server/actions_test.go`:

```go
package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

func (e *env) act(method, path, action string, body any) (int, []byte) {
	e.t.Helper()
	code, b, _ := e.doHdr(method, path, "", e.web, map[string]string{api.HeaderAction: action}, body)
	return code, b
}

func TestInstructionRoutes(t *testing.T) {
	e := newEnv(t)
	var a, b api.Instruction
	e.must(201, "POST", "/v1/instructions", e.tokA, api.InstructionIn{Text: " Never push to main. "}, &a)
	if a.Text != "Never push to main." || a.CreatedBy != "tower" {
		t.Errorf("machine add: %+v", a)
	}
	code, body := e.act("POST", "/v1/instructions", api.HeaderActionInstructions, api.InstructionIn{Text: "Write in British English."})
	if code != 201 || json.Unmarshal(body, &b) != nil || b.CreatedBy != "web:phone" {
		t.Fatalf("browser add: %d %s", code, body)
	}
	var list api.InstructionList
	code, body, _ = e.doWith("GET", "/v1/instructions", "", e.web, nil)
	if code != 200 || json.Unmarshal(body, &list) != nil || len(list.Instructions) != 2 || list.Version == "" {
		t.Fatalf("list: %d %s", code, body)
	}
	if code, _, _ := e.do("POST", "/v1/instructions", e.tokA, api.InstructionIn{Text: "a\nb"}); code != 400 {
		t.Errorf("newline: %d, want 400", code)
	}
	if code, _, _ := e.do("DELETE", "/v1/instructions/abc", e.tokA, nil); code != 400 {
		t.Errorf("bad id: %d, want 400", code)
	}
	if code, _, _ := e.do("DELETE", fmt.Sprintf("/v1/instructions/%d", a.ID+100), e.tokA, nil); code != 404 {
		t.Errorf("unknown id: %d, want 404", code)
	}
	if code, _ := e.act("DELETE", fmt.Sprintf("/v1/instructions/%d", a.ID), api.HeaderActionInstructions, nil); code != 204 {
		t.Errorf("browser delete: %d, want 204", code)
	}
	for i := 0; i < 6; i++ {
		e.must(201, "POST", "/v1/instructions", e.tokA, api.InstructionIn{Text: strings.Repeat("x", 300)}, nil)
	}
	code, body, _ = e.do("POST", "/v1/instructions", e.tokA, api.InstructionIn{Text: strings.Repeat("y", 300)})
	if code != 409 || !strings.Contains(string(body), "remove a rule first") {
		t.Errorf("past 2,000 characters: %d %s, want 409", code, body)
	}
}

func TestMessageRoutes(t *testing.T) {
	e := newEnv(t)
	timers := &fakeTimers{}
	e.server.after = timers.after
	e.controllable(sid1, "p1")
	// A poll is open when the message arrives: the send wakes it.
	poll := e.pollAsync(e.tokA, "?wait=30")
	timers.await(t, 1)
	code, body := e.act("POST", "/v1/messages", api.HeaderActionSend, api.MessagesIn{SessionIDs: []string{sid1, sid2}, Text: "Please rebase.",
		FromSession: sid2}) // ignored for a browser
	var out api.MessagesOut
	if code != 200 || json.Unmarshal(body, &out) != nil || len(out.Results) != 2 {
		t.Fatalf("send: %d %s", code, body)
	}
	if out.Results[0].State != api.MessageQueued || out.Results[1].State != api.MessageRefused || out.Results[1].Detail != "unknown session" {
		t.Errorf("results %+v", out.Results)
	}
	r := recv(t, poll)
	var claim api.ControlClaim
	if r.code != 200 || json.Unmarshal(r.body, &claim) != nil {
		t.Fatalf("poll: %d %s", r.code, r.body)
	}
	if claim.Request.Action != api.ActionMessage || claim.Request.ID != out.Results[0].ID ||
		claim.Text != "From the user via sessionhub (dashboard):\n\nPlease rebase." {
		t.Errorf("claim %+v text %q", claim.Request, claim.Text)
	}
	var res api.Message
	e.must(200, "POST", "/v1/control/"+claim.Request.ID+"/result", e.tokA, api.ControlResultIn{State: api.MessageDelivered}, &res)
	if res.State != api.MessageDelivered {
		t.Errorf("result %+v", res)
	}
	var m api.Message
	code, body, _ = e.doWith("GET", "/v1/messages/"+claim.Request.ID, "", e.web, nil)
	if code != 200 || json.Unmarshal(body, &m) != nil || m.State != api.MessageDelivered || m.Sender != api.SenderDashboard {
		t.Errorf("read: %d %s", code, body)
	}
	if code, _, _ := e.do("GET", "/v1/messages/nope", e.tokA, nil); code != 400 {
		t.Errorf("bad id: %d, want 400", code)
	}

	// A machine token sends as the CLI, or as a session with from_session.
	e.must(200, "POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "from bluebox"}, &out)
	if m, _ := e.st.GetMessage(t.Context(), out.Results[0].ID); m.Sender != "cli on bluebox" {
		t.Errorf("machine sender %q", m.Sender)
	}
	e.must(200, "POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "from a session", FromSession: sid2}, &out)
	if m, _ := e.st.GetMessage(t.Context(), out.Results[0].ID); m.Sender != "session 3f2a9c10" {
		t.Errorf("session sender %q", m.Sender)
	}
	if code, _, _ := e.do("POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "x", FromSession: "bad id"}); code != 400 {
		t.Errorf("bad from_session: %d, want 400", code)
	}
	if code, _, _ := e.do("POST", "/v1/messages", e.tokB, api.MessagesIn{Text: "x"}); code != 400 {
		t.Errorf("no targets: %d, want 400", code)
	}
	// bluebox has sent one message as "cli on bluebox"; 29 more fill the minute.
	for i := 0; i < store.MessagesPerMinute-1; i++ {
		e.must(200, "POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "flood"}, nil)
	}
	if code, _, _ := e.do("POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "one too many"}); code != 429 {
		t.Errorf("31st message: %d, want 429", code)
	}
}

func TestPermissionDecisionRoutes(t *testing.T) {
	e := newEnv(t)
	timers := &fakeTimers{}
	e.server.after = timers.after
	e.server.permCheck = 5 * time.Millisecond
	e.controllable(sid1, "p1")
	var p api.PermissionRequest
	e.must(201, "POST", "/v1/sessions/"+sid1+"/permissions", e.tokA,
		api.PermissionIn{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"git push"}`), CWD: "/home/user/proj"}, &p)
	if p.State != api.PermissionOpen || p.Machine != "tower" {
		t.Fatalf("created %+v", p)
	}
	if code, _, _ := e.do("POST", "/v1/sessions/"+sid1+"/permissions", e.tokB, api.PermissionIn{ToolName: "Bash"}); code != 409 {
		t.Errorf("bluebox posts for tower's session: %d, want 409", code)
	}
	if code, _, _ := e.do("GET", "/v1/permissions/"+p.ID+"/decision?wait=1", e.tokB, nil); code != 409 {
		t.Errorf("bluebox waits on tower's request: %d, want 409", code)
	}
	if code, _, _ := e.do("GET", "/v1/permissions/"+p.ID+"/decision?wait=x", e.tokA, nil); code != 400 {
		t.Errorf("wait=x: %d, want 400", code)
	}
	if code, _, _ := e.do("GET", "/v1/permissions/pr_bad/decision", e.tokA, nil); code != 400 {
		t.Errorf("bad id: %d, want 400", code)
	}

	// A decision wakes the waiting hook.
	wait := e.getAsync(e.tokA, "/v1/permissions/"+p.ID+"/decision?wait=30")
	timers.await(t, 1)
	if timers.wait(0) != 30*time.Second {
		t.Errorf("wait %s, want 30s", timers.wait(0))
	}
	code, body := e.act("POST", "/v1/permissions/"+p.ID+"/decide", api.HeaderActionApprove, api.DecisionIn{Decision: api.DecisionAllow})
	if code != 200 {
		t.Fatalf("decide: %d %s", code, body)
	}
	r := recv(t, wait)
	var got api.PermissionRequest
	if r.code != 200 || json.Unmarshal(r.body, &got) != nil || got.State != api.PermissionDecided ||
		got.Decision != api.DecisionAllow || got.DecidedBy != "web:phone" {
		t.Fatalf("hook got %d %s", r.code, r.body)
	}
	code, body = e.act("POST", "/v1/permissions/"+p.ID+"/decide", api.HeaderActionApprove, api.DecisionIn{Decision: api.DecisionDeny})
	if code != 409 {
		t.Errorf("second decision: %d %s, want 409", code, body)
	}
	// A machine decides as machine:<name>.
	var q api.PermissionRequest
	e.must(201, "POST", "/v1/sessions/"+sid1+"/permissions", e.tokA, api.PermissionIn{ToolName: "Bash"}, &q)
	e.must(200, "POST", "/v1/permissions/"+q.ID+"/decide", e.tokB, api.DecisionIn{Decision: api.DecisionDeny, Reason: "not now"}, &got)
	if got.DecidedBy != "machine:bluebox" || got.Reason != "not now" {
		t.Errorf("machine decision %+v", got)
	}

	// An answer in the terminal ends the wait with answered_locally.
	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"blocked"}`)
	var lp api.PermissionRequest
	e.must(201, "POST", "/v1/sessions/"+sid1+"/permissions", e.tokA, api.PermissionIn{ToolName: "Bash"}, &lp)
	wait = e.getAsync(e.tokA, "/v1/permissions/"+lp.ID+"/decision?wait=30")
	timers.await(t, 2)
	e.clock.Advance(time.Second)
	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"working"}`)
	r = recv(t, wait)
	if r.code != 200 || json.Unmarshal(r.body, &got) != nil || got.State != api.PermissionAnsweredLocally {
		t.Fatalf("after a local answer: %d %s", r.code, r.body)
	}
	if code, _ := e.act("POST", "/v1/permissions/"+lp.ID+"/decide", api.HeaderActionApprove, api.DecisionIn{Decision: api.DecisionAllow}); code != 410 {
		t.Errorf("decide after a local answer: %d, want 410", code)
	}

	// A wait that times out answers 204.
	var open api.PermissionRequest
	e.must(201, "POST", "/v1/sessions/"+sid1+"/permissions", e.tokA, api.PermissionIn{ToolName: "Bash"}, &open)
	wait = e.getAsync(e.tokA, "/v1/permissions/"+open.ID+"/decision?wait=5")
	timers.await(t, 3)
	timers.fire(2)
	if r := recv(t, wait); r.code != 204 {
		t.Errorf("timed-out wait: %d %s, want 204", r.code, r.body)
	}
	if code, _, _ := e.do("HEAD", "/v1/permissions/"+open.ID+"/decision", e.tokA, nil); code != 405 {
		t.Errorf("HEAD: %d, want 405", code)
	}
}
```

`t.Context()` exists from Go 1.24, and the module uses Go 1.25.

- [ ] **Step 2: Extend the auth matrix**

In `internal/server/auth_test.go` (`TestAuthMatrix`), after:

```go
	const link = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"
```

add:

```go
	// A message to sid1 for GET /v1/messages/{id}. The authorized control
	// poll may claim it, which its alsoOK covers.
	sent, _, err := e.st.SendMessages(ctx, []string{sid1}, "matrix message", "cli on tower")
	if err != nil || sent[0].ID == "" {
		t.Fatalf("send: %+v %v", sent, err)
	}
	// sid3's open request serves the decision poll; each decide request
	// gets a fresh one, so a rejected decide that wrote shows up.
	newPermission := func() api.PermissionRequest {
		p, err := e.st.CreatePermission(ctx, e.machine(e.tokA).ID, sid3, api.PermissionIn{ToolName: "Bash",
			ToolInput: json.RawMessage(`{"command":"ls"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	waitOn := newPermission()
	ruleN := 0
	freshRule := func() string {
		ruleN++
		r, err := e.st.AddInstruction(ctx, fmt.Sprintf("matrix rule %d", ruleN), "tower")
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("/v1/instructions/%d", r.ID)
	}
```

In the `cases` map, after the `"GET /v1/inbox"` case, add:

```go
		"GET /v1/instructions":          {path: "/v1/instructions", ok: 200},
		"POST /v1/instructions":         {path: "/v1/instructions", body: api.InstructionIn{Text: "Use British spelling."}, ok: 201},
		"DELETE /v1/instructions/{id}":  {fresh: freshRule, ok: 204},
		"POST /v1/messages":             {path: "/v1/messages", body: api.MessagesIn{SessionIDs: []string{sid1}, Text: "hello"}, ok: 200},
		"GET /v1/messages/{id}":         {path: "/v1/messages/" + sent[0].ID, ok: 200},
		"POST /v1/sessions/{id}/permissions": {path: "/v1/sessions/" + sid1 + "/permissions",
			body: api.PermissionIn{ToolName: "Bash"}, ok: 201, okB: 409},
		"GET /v1/permissions/{id}/decision": {path: "/v1/permissions/" + waitOn.ID + "/decision?wait=1", ok: 204, okB: 409},
		"POST /v1/permissions/{id}/decide": {fresh: func() string { return "/v1/permissions/" + newPermission().ID + "/decide" },
			body: api.DecisionIn{Decision: api.DecisionAllow}, ok: 200},
```

Replace the credentials list and `expect`, from:

```go
		{"cookie", "", newSession, ""},
		{"cookie+action", "", newSession, api.HeaderActionRemoteControl},
		{"cookie+sign-out", "", newSession, api.HeaderActionSignOut},
		{"cookie+triage", "", newSession, api.HeaderActionTriage},
		{"cookie+wrong action", "", newSession, "title"},
		{"badcookie", "", "hub_s_wrong", ""},
	}
```

through the end of the `expect` function:

```go
		if c.alsoOK != 0 {
			return []int{c.ok, c.alsoOK}
		}
		return []int{c.ok}
	}
```

with:

```go
		{"cookie", "", newSession, ""},
		{"cookie+remote-control", "", newSession, api.HeaderActionRemoteControl},
		{"cookie+sign-out", "", newSession, api.HeaderActionSignOut},
		{"cookie+triage", "", newSession, api.HeaderActionTriage},
		{"cookie+instructions", "", newSession, api.HeaderActionInstructions},
		{"cookie+send", "", newSession, api.HeaderActionSend},
		{"cookie+approve", "", newSession, api.HeaderActionApprove},
		{"cookie+wrong action", "", newSession, "title"},
		{"badcookie", "", "hub_s_wrong", ""},
	}
	// actionFor is the X-Hub-Action value each actor route needs with the
	// cookie. The credential named cookie+<value> sends exactly that value.
	actionFor := map[string]string{
		accessAction:       api.HeaderActionRemoteControl,
		accessTriage:       api.HeaderActionTriage,
		accessInstructions: api.HeaderActionInstructions,
		accessSend:         api.HeaderActionSend,
		accessApprove:      api.HeaderActionApprove,
	}
	expect := func(rt route, c authCase, tk string) []int {
		switch {
		case rt.access == accessPublic:
		case tk == "none" || tk == "bad" || tk == "read token" || tk == "badcookie":
			return []int{http.StatusUnauthorized}
		case (rt.access == accessWrite || rt.access == accessMachine) && strings.HasPrefix(tk, "cookie"):
			return []int{http.StatusUnauthorized} // the cookie never authorizes a machine route
		case actionFor[rt.access] != "" && strings.HasPrefix(tk, "cookie") && tk != "cookie+"+actionFor[rt.access]:
			return []int{http.StatusForbidden} // the cookie only with the route's own X-Hub-Action
		case rt.access == accessSignOut && tk != "cookie+sign-out":
			return []int{http.StatusForbidden} // only a browser ends its own session
		case tk == "machineB" && c.okB != 0:
			return []int{c.okB}
		}
		if c.alsoOK != 0 {
			return []int{c.ok, c.alsoOK}
		}
		return []int{c.ok}
	}
```

At the end of `TestAuthMatrix`, after the `d3` check, add:

```go
	if list, _ := e.st.Instructions(ctx); !slices.ContainsFunc(list.Instructions, func(in api.Instruction) bool {
		return in.Text == "Use British spelling."
	}) {
		t.Error("authorized rule not stored")
	}
```

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/server/ -run 'TestInstructionRoutes|TestMessageRoutes|TestPermissionDecisionRoutes|TestAuthMatrix' -count=1`
Expected: FAIL to compile with `undefined: accessInstructions` and
`e.server.permCheck undefined`.

- [ ] **Step 4: Add the access levels, routes, and error mapping**

In `internal/server/routes.go`, after:

```go
	accessTriage  = "triage"   // machine token, or the session cookie with X-Hub-Action: triage
```

add:

```go
	accessInstructions = "instructions" // machine token, or the session cookie with X-Hub-Action: instructions
	accessSend         = "send"         // machine token, or the session cookie with X-Hub-Action: send
	accessApprove      = "approve"      // machine token, or the session cookie with X-Hub-Action: approve
```

After the route `{"POST", "/v1/control/{id}/result", ...},` add:

```go
		{"POST", "/v1/instructions", accessInstructions, s.actor(api.HeaderActionInstructions, s.addInstruction)},
		{"DELETE", "/v1/instructions/{id}", accessInstructions, s.actor(api.HeaderActionInstructions, s.deleteInstruction)},
		{"POST", "/v1/messages", accessSend, s.actor(api.HeaderActionSend, s.postMessages)},
		{"POST", "/v1/sessions/{id}/permissions", accessWrite, s.writer(s.postPermission)},
		{"GET", "/v1/permissions/{id}/decision", accessWrite, s.writer(s.waitDecision)},
		{"POST", "/v1/permissions/{id}/decide", accessApprove, s.actor(api.HeaderActionApprove, s.decidePermission)},
```

After the route `{"GET", "/v1/inbox", accessRead, s.reader(s.getInbox)},`
add:

```go
		{"GET", "/v1/instructions", accessRead, s.reader(s.listInstructions)},
		{"GET", "/v1/messages/{id}", accessRead, s.reader(s.getMessage)},
```

In `internal/server/server.go`, in the `Server` struct, after:

```go
	// after is the long poll's timer. Tests replace it.
	after func(time.Duration) <-chan time.Time
```

add:

```go
	// permCheck is how often an open decision poll rereads its request, to
	// see an answer in the terminal or an expiry. Tests shorten it.
	permCheck time.Duration
```

In `New`, replace:

```go
	return &Server{store: st, publicURL: strings.TrimRight(publicURL, "/"), log: logger, control: newControlHub(), after: time.After}
```

with:

```go
	return &Server{store: st, publicURL: strings.TrimRight(publicURL, "/"), log: logger, control: newControlHub(),
		after: time.After, permCheck: time.Second}
```

In `storeError`, replace:

```go
	case errors.Is(err, store.ErrCodeGone):
		writeError(w, http.StatusGone, err.Error())
```

with:

```go
	case errors.Is(err, store.ErrCodeGone), errors.Is(err, store.ErrGone):
		writeError(w, http.StatusGone, err.Error())
	case errors.Is(err, store.ErrFull):
		writeError(w, http.StatusConflict, err.Error())
```

- [ ] **Step 5: Share the poll plumbing and claim messages**

In `internal/server/control.go`, after the `maxPollWait` constant add:

```go
	// maxDecisionPolls caps one machine's open decision polls: one per
	// waiting permission hook.
	maxDecisionPolls = 20
```

Replace:

```go
func (h *controlHub) acquire(machine string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[machine] >= maxPollsPerMachine {
		return false
	}
	h.open[machine]++
	return true
}
```

with:

```go
func (h *controlHub) acquire(machine string) bool { return h.acquireMax(machine, maxPollsPerMachine) }

// acquireMax takes one of n slots under key. Control polls use the machine
// name; decision polls use "decision:" + the machine name.
func (h *controlHub) acquireMax(key string, n int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[key] >= n {
		return false
	}
	h.open[key]++
	return true
}
```

In `pollControl`, replace:

```go
	wait := maxPollWait
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("wait=%q: want a number of seconds", v))
			return
		}
		wait = time.Duration(min(max(n, 1), 30)) * time.Second
	}
	name := p.machine.Name
```

with:

```go
	wait, ok := pollWait(w, r)
	if !ok {
		return
	}
	name := p.machine.Name
```

replace:

```go
		claim, ok, err := s.store.ClaimControl(r.Context(), p.machine.ID)
		if err != nil {
			s.internalError(w, err)
			return
		}
```

with:

```go
		claim, ok, err := s.store.ClaimControl(r.Context(), p.machine.ID)
		if err == nil && !ok {
			claim, ok, err = s.store.ClaimMessage(r.Context(), p.machine.ID)
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
```

In `postControlResult`, replace:

```go
	id := r.PathValue("id")
	if !store.ValidControlID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request id %q", id))
		return
	}
	var in api.ControlResultIn
	if !decode(w, r, &in) {
		return
	}
```

with:

```go
	id := r.PathValue("id")
	if !store.ValidControlID(id) && !store.ValidMessageID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request id %q", id))
		return
	}
	var in api.ControlResultIn
	if !decode(w, r, &in) {
		return
	}
	if store.ValidMessageID(id) {
		m, err := s.store.FinishMessage(r.Context(), p.machine.ID, id, in)
		if err != nil {
			s.storeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, m)
		return
	}
```

At the end of the file, add:

```go
// pollWait reads a long poll's wait=N (seconds, clamped to 1 to 30; 30 when
// absent). It writes a 400 and returns false for a value that is not a
// number.
func pollWait(w http.ResponseWriter, r *http.Request) (time.Duration, bool) {
	v := r.URL.Query().Get("wait")
	if v == "" {
		return maxPollWait, true
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("wait=%q: want a number of seconds", v))
		return 0, false
	}
	return time.Duration(min(max(n, 1), 30)) * time.Second, true
}
```

- [ ] **Step 6: Write the handlers**

Create `internal/server/actions.go`:

```go
package server

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

// decider names who made a request: web:<name> for a browser,
// machine:<name> for a machine token.
func decider(p principal) string {
	if p.web != nil {
		return "web:" + p.web.Name
	}
	return api.RequestedByMachinePrefix + p.machine.Name
}

// listInstructions is GET /v1/instructions.
func (s *Server) listInstructions(w http.ResponseWriter, r *http.Request, _ principal) {
	list, err := s.store.Instructions(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// addInstruction is POST /v1/instructions.
func (s *Server) addInstruction(w http.ResponseWriter, r *http.Request, p principal) {
	var in api.InstructionIn
	if !decode(w, r, &in) {
		return
	}
	by := p.machine.Name
	if p.web != nil {
		by = "web:" + p.web.Name
	}
	rule, err := s.store.AddInstruction(r.Context(), in.Text, by)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

// deleteInstruction is DELETE /v1/instructions/{id}.
func (s *Server) deleteInstruction(w http.ResponseWriter, r *http.Request, _ principal) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid rule id %q", r.PathValue("id")))
		return
	}
	if err := s.store.DeleteInstruction(r.Context(), id); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// postMessages is POST /v1/messages. It wakes the long poll of each machine
// that got a message.
func (s *Server) postMessages(w http.ResponseWriter, r *http.Request, p principal) {
	var in api.MessagesIn
	if !decode(w, r, &in) {
		return
	}
	sender := api.SenderDashboard
	if p.web == nil {
		sender = "cli on " + p.machine.Name
		if in.FromSession != "" {
			if !store.ValidSessionID(in.FromSession) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid from_session %q", in.FromSession))
				return
			}
			short := in.FromSession
			if len(short) > 8 {
				short = short[:8]
			}
			sender = "session " + short
		}
	}
	results, machines, err := s.store.SendMessages(r.Context(), in.SessionIDs, in.Text, sender)
	if err != nil {
		s.storeError(w, err)
		return
	}
	for _, m := range machines {
		s.control.notify(m)
	}
	writeJSON(w, http.StatusOK, api.MessagesOut{Results: results})
}

// getMessage is GET /v1/messages/{id}.
func (s *Server) getMessage(w http.ResponseWriter, r *http.Request, _ principal) {
	m, err := s.store.GetMessage(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// postPermission is POST /v1/sessions/{id}/permissions, from the
// permission hook on the session's own machine.
func (s *Server) postPermission(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in api.PermissionIn
	if !decode(w, r, &in) {
		return
	}
	req, err := s.store.CreatePermission(r.Context(), p.machine.ID, id, in)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, req)
}

// waitDecision is GET /v1/permissions/{id}/decision?wait=N: the permission
// hook's long poll. It answers 200 with the request once it is not open,
// else 204 after the wait. A decision wakes it at once; an answer in the
// terminal or an expiry shows up within permCheck.
func (s *Server) waitDecision(w http.ResponseWriter, r *http.Request, p principal) {
	// The mux routes HEAD to a GET pattern; a HEAD has no body to wait for.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "use GET to wait for a decision")
		return
	}
	id := r.PathValue("id")
	if !store.ValidPermissionID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request id %q", id))
		return
	}
	wait, ok := pollWait(w, r)
	if !ok {
		return
	}
	key := "decision:" + p.machine.Name
	if !s.control.acquireMax(key, maxDecisionPolls) {
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("machine %q already has %d open decision polls", p.machine.Name, maxDecisionPolls))
		return
	}
	defer s.control.release(key)
	// Check the owner and the state before the timer starts, so a refused
	// or settled request answers at once.
	req, err := s.store.PermissionFor(r.Context(), p.machine.ID, id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if req.State != api.PermissionOpen {
		writeJSON(w, http.StatusOK, req)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wait + pollWriteSlack))
	timeout := s.after(wait)
	for {
		// Take the wake-up channel before the read, so a decision made
		// between the read and the select still wakes this poll.
		woken := s.control.waiter("permission:" + id)
		req, err = s.store.PermissionFor(r.Context(), p.machine.ID, id)
		if err != nil {
			s.storeError(w, err)
			return
		}
		if req.State != api.PermissionOpen {
			writeJSON(w, http.StatusOK, req)
			return
		}
		select {
		case <-woken:
		case <-time.After(s.permCheck):
		case <-timeout:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-s.control.done:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// decidePermission is POST /v1/permissions/{id}/decide.
func (s *Server) decidePermission(w http.ResponseWriter, r *http.Request, p principal) {
	id := r.PathValue("id")
	if !store.ValidPermissionID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request id %q", id))
		return
	}
	var in api.DecisionIn
	if !decode(w, r, &in) {
		return
	}
	req, err := s.store.DecidePermission(r.Context(), id, in, decider(p))
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.control.notify("permission:" + id)
	writeJSON(w, http.StatusOK, req)
}
```

`controlHub.waiter` and `notify` are keyed by any string, so the decision
poll uses `permission:<id>` beside the machine names.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/server/ -count=1`
Expected: PASS. `TestAuthMatrix` logs `33 routes x 14 credentials`.

- [ ] **Step 8: Document it**

In `docs/server.md`, section "Authentication", replace:

```markdown
  `POST /v1/inbox/{id}/snooze` with `X-Hub-Action: triage`, and
  `POST /logout` with `X-Hub-Action: sign-out`. Nothing else: the cookie never
```

with:

```markdown
  `POST /v1/inbox/{id}/snooze` with `X-Hub-Action: triage`,
  `POST /v1/instructions` and `DELETE /v1/instructions/{id}` with
  `X-Hub-Action: instructions`, `POST /v1/messages` with
  `X-Hub-Action: send`, `POST /v1/permissions/{id}/decide` with
  `X-Hub-Action: approve`, and
  `POST /logout` with `X-Hub-Action: sign-out`. Nothing else: the cookie never
```

In section "Endpoints", after the `POST /v1/inbox/{id}/snooze` row, add:

```markdown
| `GET /v1/instructions` | read | `200`, `InstructionList` | The standing rules, oldest first, and their `version`. See [Shared instructions](#shared-instructions). |
| `POST /v1/instructions` | machine, or cookie with `X-Hub-Action: instructions` | `201`, the `Instruction` | Add a rule (`InstructionIn`). `400` invalid, `409` past 2,000 characters. |
| `DELETE /v1/instructions/{id}` | machine, or cookie with `X-Hub-Action: instructions` | `204`; `404` unknown | Remove a rule. |
| `POST /v1/messages` | machine, or cookie with `X-Hub-Action: send` | `200`, `MessagesOut` | Send a message to 1 to 20 sessions (`MessagesIn`). `429` past 30 a minute. See [Messages to sessions](#messages-to-sessions). |
| `GET /v1/messages/{id}` | read | `200`, `Message` | One message and its delivery state. |
| `POST /v1/sessions/{id}/permissions` | machine (the owning one) | `201`, `PermissionRequest` | The permission hook's request (`PermissionIn`). See [Permission requests](#permission-requests). |
| `GET /v1/permissions/{id}/decision?wait=30` | machine (the owning one) | `200` the request once it is not open; `204` none yet | The permission hook's long poll. |
| `POST /v1/permissions/{id}/decide` | machine, or cookie with `X-Hub-Action: approve` | `200`, the `PermissionRequest` | Allow once or deny (`DecisionIn`). `409` already decided, `410` expired or answered. |
```

and replace the row:

```markdown
| `POST /v1/control/{id}/result` | machine (the claiming one) | `200`, the `ControlRequest` | The watcher's result (`ControlResultIn`). |
```

with:

```markdown
| `POST /v1/control/{id}/result` | machine (the claiming one) | `200`, the `ControlRequest`, or the `Message` for a `msg_` ID | The watcher's result (`ControlResultIn`). |
```

Before "### Messages to sessions" (added in Task 2), add:

```markdown
### Shared instructions

Standing rules every session sees, at its start and with every prompt. Each
rule is 1 to 300 characters with no control characters; the list holds at
most 2,000 characters, and an add past that is `409`. `GET
/v1/instructions` returns `{"instructions": [{"id", "text", "created_at",
"created_by"}], "version": "<16 hex digits>"}`; the version changes whenever
the list does, so clients skip an unchanged copy. `created_by` is the machine
name, or `web:<session name>` for a browser.
```

- [ ] **Step 9: Run the full suite and lint**

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1 && make lint`
Expected: every package `ok`, lint clean. If `internal/server` fails with
`bind: address already in use`, wait 60 seconds and run it alone again.

- [ ] **Step 10: Commit**

```bash
git add internal/server/routes.go internal/server/server.go internal/server/control.go internal/server/actions.go \
  internal/server/actions_test.go internal/server/helpers_test.go internal/server/auth_test.go docs/server.md
git commit -m "$(cat <<'EOF'
Serve rules, messages, and permission decisions

Eight routes: the rules list and edits, sending and reading messages,
and the permission hook's request, decision long poll, and decide.
Browser writes need X-Hub-Action instructions, send, or approve. The
control long poll now also hands out queued messages, and a send wakes
the target machine's poll. The auth matrix walks every new route with
every cookie variant and checks the new tables after each rejection.

Refs: docs/dev/superpowers/plans/2026-10-02-session-actions.md, Task 4

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 5: Client: HTTP calls and the local rules copy

**Files:**
- Modify: `internal/client/http.go` (eight methods)
- Modify: `internal/client/config.go` (`RemotePermissions`,
  `RemotePermissionsOff`)
- Create: `internal/client/instructions.go` (`InstructionsPath`,
  `LoadInstructions`, `SaveInstructions`, `RefreshInstructions`)
- Create: `internal/client/actions_test.go`
- Modify: `docs/client.md`

**Interfaces:**
- Consumes: the API types (Tasks 1 and 3) and the routes (Task 4);
  `Client.do`, `Client.doTimeout`, `PollSlack`, test helper `server`.
- Produces:
  - `func (c *Client) Instructions(ctx context.Context) (api.InstructionList, error)`
  - `func (c *Client) AddInstruction(ctx context.Context, text string) (api.Instruction, error)`
  - `func (c *Client) DeleteInstruction(ctx context.Context, id int64) error`
  - `func (c *Client) SendMessages(ctx context.Context, in api.MessagesIn) (api.MessagesOut, error)`
  - `func (c *Client) GetMessage(ctx context.Context, id string) (api.Message, error)`
  - `func (c *Client) CreatePermission(ctx context.Context, sessionID string, in api.PermissionIn) (api.PermissionRequest, error)`
  - `func (c *Client) WaitDecision(ctx context.Context, id string, wait time.Duration) (*api.PermissionRequest, error)`
    (`nil` on `204`)
  - `func (c *Client) DecidePermission(ctx context.Context, id string, in api.DecisionIn) (api.PermissionRequest, error)`
  - `Config.RemotePermissions *bool` (`toml:"remote_permissions,omitempty"`)
  - `func RemotePermissionsOff(cfg Config, getenv func(string) string) bool`
  - `const InstructionsFile = "instructions.json"`
  - `func InstructionsPath(stateDir string) string`
  - `func LoadInstructions(path string) (api.InstructionList, error)`
  - `func SaveInstructions(path string, list api.InstructionList) error`
  - `func RefreshInstructions(ctx context.Context, c *Client, path string) (changed bool, err error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/client/actions_test.go`:

```go
package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestActionCallsRequestShape(t *testing.T) {
	ctx := context.Background()
	c, log := server(t, 200, `{}`)
	for _, tc := range []struct {
		name, method, path, query, body string
		call                            func() error
	}{
		{"rules", "GET", "/v1/instructions", "", "", func() error { _, err := c.Instructions(ctx); return err }},
		{"add rule", "POST", "/v1/instructions", "", `{"text":"Be brief."}`,
			func() error { _, err := c.AddInstruction(ctx, "Be brief."); return err }},
		{"remove rule", "DELETE", "/v1/instructions/7", "", "", func() error { return c.DeleteInstruction(ctx, 7) }},
		{"send", "POST", "/v1/messages", "", `{"session_ids":["a","b"],"text":"hi","from_session":"s1"}`,
			func() error {
				_, err := c.SendMessages(ctx, api.MessagesIn{SessionIDs: []string{"a", "b"}, Text: "hi", FromSession: "s1"})
				return err
			}},
		{"message", "GET", "/v1/messages/msg_x", "", "", func() error { _, err := c.GetMessage(ctx, "msg_x"); return err }},
		{"permission", "POST", "/v1/sessions/s1/permissions", "", `{"tool_name":"Bash","tool_input":{"command":"ls"}}`,
			func() error {
				_, err := c.CreatePermission(ctx, "s1", api.PermissionIn{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`)})
				return err
			}},
		{"decide", "POST", "/v1/permissions/pr_x/decide", "", `{"decision":"deny","reason":"no"}`,
			func() error {
				_, err := c.DecidePermission(ctx, "pr_x", api.DecisionIn{Decision: api.DecisionDeny, Reason: "no"})
				return err
			}},
	} {
		*log = nil
		if err := tc.call(); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		g := (*log)[0]
		if g.Method != tc.method || g.Path != tc.path || g.Query != tc.query || string(g.Body) != tc.body || g.Auth != "Bearer hub_m_tok" {
			t.Errorf("%s: got %s %s?%s body %s auth %q", tc.name, g.Method, g.Path, g.Query, g.Body, g.Auth)
		}
	}
}

func TestWaitDecision(t *testing.T) {
	ctx := context.Background()
	c, log := server(t, 204, ``)
	got, err := c.WaitDecision(ctx, "pr_x", 30*time.Second)
	if err != nil || got != nil {
		t.Fatalf("204: %+v %v, want nil", got, err)
	}
	if g := (*log)[0]; g.Method != "GET" || g.Path != "/v1/permissions/pr_x/decision" || g.Query != "wait=30" {
		t.Errorf("request %+v", g)
	}
	c, _ = server(t, 200, `{"id":"pr_x","state":"decided","decision":"allow"}`)
	got, err = c.WaitDecision(ctx, "pr_x", 0)
	if err != nil || got == nil || got.Decision != api.DecisionAllow {
		t.Fatalf("200: %+v %v", got, err)
	}
	c, _ = server(t, 409, `{"error":"request pr_x belongs to another machine"}`)
	_, err = c.WaitDecision(ctx, "pr_x", time.Second)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 409 {
		t.Errorf("409: %v", err)
	}
}

func TestInstructionsCopy(t *testing.T) {
	dir := t.TempDir()
	path := InstructionsPath(filepath.Join(dir, "state"))
	if filepath.Base(path) != InstructionsFile {
		t.Fatalf("path %s", path)
	}
	if _, err := LoadInstructions(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing copy: %v, want ErrNotExist", err)
	}
	list := api.InstructionList{Version: "v1", Instructions: []api.Instruction{{ID: 1, Text: "Be brief."}}}
	if err := SaveInstructions(path, list); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", st.Mode().Perm(), err)
	}
	got, err := LoadInstructions(path)
	if err != nil || got.Version != "v1" || len(got.Instructions) != 1 || got.Instructions[0].Text != "Be brief." {
		t.Errorf("loaded %+v %v", got, err)
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".instructions-*")); len(m) != 0 {
		t.Errorf("temp files left: %v", m)
	}
	os.WriteFile(path, []byte("{broken"), 0o600)
	if _, err := LoadInstructions(path); err == nil {
		t.Error("a broken copy loaded")
	}
}

func TestRefreshInstructions(t *testing.T) {
	ctx := context.Background()
	version := "v1"
	gets := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.InstructionList{Version: version, Instructions: []api.Instruction{{ID: 1, Text: "rule " + version}}})
	}))
	t.Cleanup(ts.Close)
	c, _ := New(Config{ServerURL: ts.URL, Token: "hub_m_tok"})
	path := InstructionsPath(t.TempDir())
	changed, err := RefreshInstructions(ctx, c, path)
	if err != nil || !changed {
		t.Fatalf("first refresh: %v %v", changed, err)
	}
	st1, _ := os.Stat(path)
	time.Sleep(10 * time.Millisecond)
	if changed, err := RefreshInstructions(ctx, c, path); err != nil || changed {
		t.Errorf("same version: changed=%v err=%v", changed, err)
	}
	if st2, _ := os.Stat(path); !st2.ModTime().Equal(st1.ModTime()) {
		t.Error("an unchanged list rewrote the copy")
	}
	version = "v2"
	if changed, _ := RefreshInstructions(ctx, c, path); !changed {
		t.Error("a new version did not rewrite the copy")
	}
	if got, _ := LoadInstructions(path); got.Instructions[0].Text != "rule v2" || gets != 3 {
		t.Errorf("copy %+v after %d gets", got, gets)
	}
}

func TestRemotePermissionsOff(t *testing.T) {
	off, on := false, true
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "SESSIONHUB_REMOTE_PERMISSIONS" {
				return v
			}
			return ""
		}
	}
	for _, c := range []struct {
		cfg  Config
		env  string
		want bool
	}{
		{Config{}, "", false},
		{Config{RemotePermissions: &on}, "", false},
		{Config{RemotePermissions: &off}, "", true},
		{Config{}, "off", true},
		{Config{}, "OFF", true},
		{Config{RemotePermissions: &on}, "off", true},
		{Config{}, "on", false},
	} {
		if got := RemotePermissionsOff(c.cfg, env(c.env)); got != c.want {
			t.Errorf("cfg %v env %q: %v, want %v", c.cfg.RemotePermissions, c.env, got, c.want)
		}
	}
}

func TestConfigRemotePermissionsKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("SESSIONHUB_CONFIG", path)
	for _, k := range []string{"SESSIONHUB_SERVER_URL", "SESSIONHUB_TOKEN", "SESSIONHUB_MACHINE"} {
		t.Setenv(k, "")
	}
	if err := (Config{ServerURL: "https://sessionhub.example"}).Save(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "remote_permissions") {
		t.Errorf("an unset key was saved:\n%s", b)
	}
	os.WriteFile(path, []byte("server_url = \"https://sessionhub.example\"\nremote_permissions = false\n"), 0o600)
	c, err := LoadConfig()
	if err != nil || c.RemotePermissions == nil || *c.RemotePermissions {
		t.Errorf("loaded %+v %v", c, err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/client/ -count=1`
Expected: FAIL to compile with `c.Instructions undefined` and
`undefined: InstructionsPath`.

- [ ] **Step 3: Add the HTTP calls**

In `internal/client/http.go`, add at the end of the file:

```go
// Instructions reads the standing rules and their version.
func (c *Client) Instructions(ctx context.Context) (api.InstructionList, error) {
	var out api.InstructionList
	err := c.do(ctx, http.MethodGet, "/v1/instructions", nil, &out)
	return out, err
}

// AddInstruction adds a standing rule.
func (c *Client) AddInstruction(ctx context.Context, text string) (api.Instruction, error) {
	var out api.Instruction
	err := c.do(ctx, http.MethodPost, "/v1/instructions", api.InstructionIn{Text: text}, &out)
	return out, err
}

// DeleteInstruction removes standing rule id.
func (c *Client) DeleteInstruction(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, "/v1/instructions/"+strconv.FormatInt(id, 10), nil, nil)
}

// SendMessages sends a message to sessions. Each target's result says
// whether it was queued or refused.
func (c *Client) SendMessages(ctx context.Context, in api.MessagesIn) (api.MessagesOut, error) {
	var out api.MessagesOut
	err := c.do(ctx, http.MethodPost, "/v1/messages", in, &out)
	return out, err
}

// GetMessage reads one message and its delivery state.
func (c *Client) GetMessage(ctx context.Context, id string) (api.Message, error) {
	var out api.Message
	err := c.do(ctx, http.MethodGet, "/v1/messages/"+url.PathEscape(id), nil, &out)
	return out, err
}

// CreatePermission posts a permission prompt of session sessionID.
func (c *Client) CreatePermission(ctx context.Context, sessionID string, in api.PermissionIn) (api.PermissionRequest, error) {
	var out api.PermissionRequest
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/permissions", in, &out)
	return out, err
}

// WaitDecision holds GET /v1/permissions/{id}/decision open for up to wait
// (whole seconds, at least 1) and returns the request once it is not open,
// or nil when the wait ended first (204).
func (c *Client) WaitDecision(ctx context.Context, id string, wait time.Duration) (*api.PermissionRequest, error) {
	secs := max(int(wait/time.Second), 1)
	var raw json.RawMessage
	if err := c.doTimeout(ctx, time.Duration(secs)*time.Second+PollSlack, http.MethodGet,
		"/v1/permissions/"+url.PathEscape(id)+"/decision?wait="+strconv.Itoa(secs), nil, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil // 204
	}
	var out api.PermissionRequest
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DecidePermission allows or denies permission request id, once.
func (c *Client) DecidePermission(ctx context.Context, id string, in api.DecisionIn) (api.PermissionRequest, error) {
	var out api.PermissionRequest
	err := c.do(ctx, http.MethodPost, "/v1/permissions/"+url.PathEscape(id)+"/decide", in, &out)
	return out, err
}
```

`json.RawMessage` stays empty on `204` because `doTimeout` returns before
decoding an empty body; `PollControl` relies on the same.

- [ ] **Step 4: Add the config key**

In `internal/client/config.go`, replace:

```go
// Config is the client config. File keys: server_url, token, machine.
// Env overrides: SESSIONHUB_SERVER_URL, SESSIONHUB_TOKEN, SESSIONHUB_MACHINE.
type Config struct {
	ServerURL string `toml:"server_url"`
	Token     string `toml:"token"`
	Machine   string `toml:"machine"`
}
```

with:

```go
// Config is the client config. File keys: server_url, token, machine, and
// remote_permissions. Env overrides: SESSIONHUB_SERVER_URL, SESSIONHUB_TOKEN, SESSIONHUB_MACHINE.
type Config struct {
	ServerURL string `toml:"server_url"`
	Token     string `toml:"token"`
	Machine   string `toml:"machine"`
	// RemotePermissions = false turns off remote permission answers on this
	// machine. Unset means on.
	RemotePermissions *bool `toml:"remote_permissions,omitempty"`
}

// RemotePermissionsOff reports whether this machine opted out of remote
// permission answers: SESSIONHUB_REMOTE_PERMISSIONS=off (any case) in the
// environment, or remote_permissions = false in the config.
func RemotePermissionsOff(cfg Config, getenv func(string) string) bool {
	if strings.EqualFold(getenv("SESSIONHUB_REMOTE_PERMISSIONS"), "off") {
		return true
	}
	return cfg.RemotePermissions != nil && !*cfg.RemotePermissions
}
```

Add `"strings"` to the imports of `config.go`.

- [ ] **Step 5: Add the local rules copy**

Create `internal/client/instructions.go`:

```go
package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/abdallah/session-hub/internal/api"
)

// InstructionsFile is the local copy of the standing rules, in the state
// dir. The watcher and `sessionhub hook refresh-instructions` write it; `sessionhub hook
// context` reads it.
const InstructionsFile = "instructions.json"

// InstructionsPath is the local copy's path in stateDir.
func InstructionsPath(stateDir string) string { return filepath.Join(stateDir, InstructionsFile) }

// LoadInstructions reads the local copy. A missing file returns an error
// that matches os.ErrNotExist.
func LoadInstructions(path string) (api.InstructionList, error) {
	var list api.InstructionList
	b, err := os.ReadFile(path)
	if err != nil {
		return list, err
	}
	err = json.Unmarshal(b, &list)
	return list, err
}

// SaveInstructions writes the local copy with mode 0600 through a temp file
// and a rename, so a reader never sees half a file.
func SaveInstructions(path string, list api.InstructionList) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".instructions-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// RefreshInstructions reads the rules from the server and rewrites the
// local copy when its version differs or it cannot be read.
func RefreshInstructions(ctx context.Context, c *Client, path string) (bool, error) {
	list, err := c.Instructions(ctx)
	if err != nil {
		return false, err
	}
	if cur, err := LoadInstructions(path); err == nil && cur.Version == list.Version {
		return false, nil
	}
	return true, SaveInstructions(path, list)
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/client/ -count=1`
Expected: PASS, including `TestConfigSaveLoad` (a nil pointer keeps
`Config` comparable and is not saved).

- [ ] **Step 7: Document it**

In `docs/client.md`, replace:

```markdown
  config. `Config.Save()` writes mode 0600 through a temp file and creates
  parent directories. Keys: `server_url`, `token`, `machine`.
```

with:

```markdown
  config. `Config.Save()` writes mode 0600 through a temp file and creates
  parent directories. Keys: `server_url`, `token`, `machine`, and
  `remote_permissions` (unset means on; `false` turns off remote permission
  answers on this machine). `RemotePermissionsOff(cfg, os.Getenv)` is also
  true when `SESSIONHUB_REMOTE_PERMISSIONS=off`.
```

and replace:

```markdown
  `DismissInbox(id, since)`, and `SnoozeInbox(id, since, until)`. Pass the
  inbox item's `since` to the last two unchanged.
```

with:

```markdown
  `DismissInbox(id, since)`, `SnoozeInbox(id, since, until)`,
  `Instructions`, `AddInstruction(text)`, `DeleteInstruction(id)`,
  `SendMessages(api.MessagesIn)`, `GetMessage(id)`,
  `CreatePermission(sessionID, api.PermissionIn)`,
  `WaitDecision(id, wait)`, and `DecidePermission(id, api.DecisionIn)`.
  Pass the inbox item's `since` to `DismissInbox` and `SnoozeInbox`
  unchanged. `WaitDecision` is a long poll like `PollControl`: its time limit
  is `wait` plus 10 s, and it returns `nil` on `204`.
- The local rules copy is `<state dir>/instructions.json`
  (`InstructionsPath`). `LoadInstructions` reads it, `SaveInstructions`
  writes it with mode 0600 through a temp file, and
  `RefreshInstructions(ctx, c, path)` fetches the list and rewrites the copy
  only when its `version` changed.
```

- [ ] **Step 8: Run the full suite and lint**

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1 && make lint`
Expected: every package `ok`, lint clean.

- [ ] **Step 9: Commit**

```bash
git add internal/client/http.go internal/client/config.go internal/client/instructions.go \
  internal/client/actions_test.go docs/client.md
git commit -m "$(cat <<'EOF'
Add client calls for rules, messages, and permission decisions

The client gains the eight calls the new routes need, a decision long
poll that returns nil on 204, the remote_permissions opt-out, and the
local rules copy in the state dir, rewritten only when the server's
version changes.

Refs: docs/dev/superpowers/plans/2026-10-02-session-actions.md, Task 5

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 6: Hooks: context, refresh, and permission requests

**Files:**
- Modify: `internal/hooks/hooks.go` (`handler` fields, `run`, session-start
  refresh)
- Create: `internal/hooks/actions.go` (`printContext`, `ContextBlock`,
  `refreshInstructions`, `permissionRequest`, `decisionOutput`)
- Create: `internal/hooks/actions_test.go`
- Modify: `internal/hooks/hooks_test.go` (`newFixture`: the new fields)
- Modify: `internal/hooks/install.go` (`hookSpec.Context`, the table)
- Modify: `internal/hooks/install_test.go` (`TestInstallAddsTheTable`, the
  event counts)
- Modify: `docs/hooks.md`

**Interfaces:**
- Consumes: `client.InstructionsPath`, `client.LoadInstructions`,
  `client.RefreshInstructions`, `client.RemotePermissionsOff`,
  `Client.CreatePermission`, `Client.WaitDecision` (Task 5);
  `api.CapToolInput` (Task 3); `cleanText`, `foldControl`,
  `startDetached`, test helpers `newFixture`, `fixture.call`,
  `newFakeServer`, `fixtureFile`.
- Produces:
  - `handler` fields `stdout io.Writer`, `refreshCmd func() *exec.Cmd`,
    `remoteOff func() bool`, `permWait time.Duration`,
    `pollWait time.Duration`
  - `sessionhub hook context`, `sessionhub hook permission-request`,
    `sessionhub hook refresh-instructions`
  - `func ContextBlock(list api.InstructionList) string`
  - `hookSpec.Context bool` (adds the `sessionhub hook context` entry to the group)

- [ ] **Step 1: Give the fixture the new fields**

In `internal/hooks/hooks_test.go`, replace:

```go
type fixture struct {
	h     *handler
	state string
	env   map[string]string
	errs  *bytes.Buffer
}
```

with:

```go
type fixture struct {
	h     *handler
	state string
	env   map[string]string
	errs  *bytes.Buffer
	out   *bytes.Buffer // the hook's stdout
}
```

and replace:

```go
	fx := &fixture{state: state, env: map[string]string{}, errs: &bytes.Buffer{}}
```

with:

```go
	fx := &fixture{state: state, env: map[string]string{}, errs: &bytes.Buffer{}, out: &bytes.Buffer{}}
```

and replace:

```go
		deadline:       5 * time.Second,
		stderr:         fx.errs,
	}
	return fx
```

with:

```go
		deadline:       5 * time.Second,
		stderr:         fx.errs,
		stdout:         fx.out,
		refreshCmd:     func() *exec.Cmd { return nil },
		remoteOff: func() bool {
			return client.RemotePermissionsOff(client.Config{}, func(k string) string { return fx.env[k] })
		},
		permWait: 5 * time.Second,
		pollWait: time.Second,
	}
	return fx
```

- [ ] **Step 2: Write the failing hook tests**

Create `internal/hooks/actions_test.go`:

```go
package hooks

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

func saveRules(t *testing.T, state string, texts ...string) {
	t.Helper()
	list := api.InstructionList{Version: "v1", Instructions: []api.Instruction{}}
	for i, s := range texts {
		list.Instructions = append(list.Instructions, api.Instruction{ID: int64(i + 1), Text: s})
	}
	if err := client.SaveInstructions(client.InstructionsPath(state), list); err != nil {
		t.Fatal(err)
	}
}

func contextStdin(event string) string {
	return `{"session_id":"` + sessionID + `","hook_event_name":"` + event + `","prompt":"hi"}`
}

func TestContextPrintsBlock(t *testing.T) {
	for _, event := range []string{"SessionStart", "UserPromptSubmit"} {
		fx := newFixture(t, "http://127.0.0.1:1")
		saveRules(t, fx.state, "Never push to main.", "Write in British English.")
		if err := fx.call(t, "context", contextStdin(event)); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		var out struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(fx.out.Bytes(), &out); err != nil {
			t.Fatalf("%s: stdout is not one JSON object: %q", event, fx.out.String())
		}
		want := "Standing instructions from sessionhub (apply in every session):\n- Never push to main.\n- Write in British English."
		if out.HookSpecificOutput.HookEventName != event || out.HookSpecificOutput.AdditionalContext != want {
			t.Errorf("%s: %+v", event, out.HookSpecificOutput)
		}
	}
}

func TestContextNeverCallsServer(t *testing.T) {
	srv := newFakeServer(t)
	for name, setup := range map[string]func(state string){
		"rules":   func(state string) { saveRules(t, state, "Be brief.") },
		"missing": func(string) {},
		"broken":  func(state string) { os.WriteFile(client.InstructionsPath(state), []byte("{"), 0o600) },
	} {
		fx := newFixture(t, srv.URL)
		called := false
		fx.h.newClient = func() (*client.Client, error) {
			called = true
			return nil, errors.New("the context hook built a client")
		}
		setup(fx.state)
		_ = fx.call(t, "context", contextStdin("UserPromptSubmit"))
		if called {
			t.Errorf("%s: the context hook built a sessionhub client", name)
		}
		if name != "rules" && fx.out.Len() != 0 {
			t.Errorf("%s: printed %q, want nothing", name, fx.out.String())
		}
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("the context hook made %d requests", n)
	}
}

func TestContextPrintsNothing(t *testing.T) {
	for name, c := range map[string]struct {
		rules []string
		stdin string
	}{
		"empty list":   {nil, contextStdin("UserPromptSubmit")},
		"other event":  {[]string{"Be brief."}, contextStdin("Stop")},
		"subagent":     {[]string{"Be brief."}, `{"session_id":"s","agent_id":"a","hook_event_name":"SessionStart"}`},
		"control only": {[]string{"\x1b\x07"}, contextStdin("SessionStart")},
	} {
		fx := newFixture(t, "http://127.0.0.1:1")
		saveRules(t, fx.state, c.rules...)
		if err := fx.call(t, "context", c.stdin); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if fx.out.Len() != 0 {
			t.Errorf("%s: printed %q", name, fx.out.String())
		}
	}
}

func TestContextBlockCleansRules(t *testing.T) {
	got := ContextBlock(api.InstructionList{Instructions: []api.Instruction{{Text: "one\ntwo  \x1b[31mred"}}})
	if got != "Standing instructions from sessionhub (apply in every session):\n- one two [31mred" {
		t.Errorf("block %q", got)
	}
}

func TestSessionStartStartsRefresh(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	started := 0
	fx.h.refreshCmd = func() *exec.Cmd {
		started++
		return exec.Command("true")
	}
	if err := fx.call(t, "session-start", fixtureFile(t, "SessionStart.json")); err != nil {
		t.Fatal(err)
	}
	if err := fx.call(t, "prompt", fixtureFile(t, "UserPromptSubmit.json")); err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Errorf("refresh started %d times, want once (session-start only)", started)
	}
	if fx.out.Len() != 0 {
		t.Errorf("session-start and prompt printed %q; only the context hook prints", fx.out.String())
	}
}

func TestRefreshInstructionsWritesCopy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/instructions" {
			w.WriteHeader(404)
			return
		}
		io.WriteString(w, `{"instructions":[{"id":3,"text":"Be brief."}],"version":"abc"}`)
	}))
	t.Cleanup(ts.Close)
	fx := newFixture(t, ts.URL)
	if err := fx.call(t, "refresh-instructions", ""); err != nil {
		t.Fatal(err)
	}
	got, err := client.LoadInstructions(client.InstructionsPath(fx.state))
	if err != nil || got.Version != "abc" || got.Instructions[0].Text != "Be brief." {
		t.Errorf("copy %+v %v", got, err)
	}
}

// permServer answers the permission hook: 201 to the request, then each
// decision poll with the next of answers (status and body). It records
// every request body.
type permServer struct {
	*httptest.Server
	mu      sync.Mutex
	answers []answer
	polls   int
	bodies  []string
	create  int // status for the create; 0 means 201
}

type answer struct {
	status int
	body   string
}

// seen returns the poll count and the request lines so far, under the lock
// the handler holds.
func (p *permServer) seen() (int, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.polls, append([]string(nil), p.bodies...)
}

func newPermServer(t *testing.T, answers ...answer) *permServer {
	t.Helper()
	p := &permServer{answers: answers}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		defer p.mu.Unlock()
		p.bodies = append(p.bodies, r.Method+" "+r.URL.Path+" "+string(b))
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/permissions"):
			code := p.create
			if code == 0 {
				code = 201
			}
			w.WriteHeader(code)
			io.WriteString(w, `{"id":"pr_AAAAAAAAAAAAAAAAAAAAAA","state":"open"}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/decision"):
			a := answer{status: 204}
			if p.polls < len(p.answers) {
				a = p.answers[p.polls]
			}
			p.polls++
			w.WriteHeader(a.status)
			io.WriteString(w, a.body)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

const permStdin = `{"session_id":"` + sessionID + `","hook_event_name":"PermissionRequest","cwd":"/home/user/proj",` +
	`"tool_name":"Bash","tool_input":{"command":"git push","description":"Push"},"permission_suggestions":[{"type":"addRules"}]}`

func TestPermissionHookOutputs(t *testing.T) {
	decided := func(decision, reason string) answer {
		b, _ := json.Marshal(api.PermissionRequest{ID: "pr_AAAAAAAAAAAAAAAAAAAAAA", State: api.PermissionDecided, Decision: decision, Reason: reason})
		return answer{200, string(b)}
	}
	allow := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` + "\n"
	for name, c := range map[string]struct {
		answers []answer
		want    string
	}{
		"allow":            {[]answer{decided("allow", "")}, allow},
		"allow after 204s": {[]answer{{204, ""}, {204, ""}, decided("allow", "")}, allow},
		"deny with reason": {[]answer{decided("deny", "use make clean")},
			`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"use make clean"}}}` + "\n"},
		"deny":             {[]answer{decided("deny", "")},
			`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"Denied from sessionhub."}}}` + "\n"},
		"answered locally": {[]answer{{200, `{"state":"answered_locally"}`}}, ""},
		"expired":          {[]answer{{200, `{"state":"expired"}`}}, ""},
		"server error":     {[]answer{{500, `{"error":"internal error"}`}}, ""},
		"unknown request":  {[]answer{{404, `{"error":"not found"}`}}, ""},
	} {
		srv := newPermServer(t, c.answers...)
		fx := newFixture(t, srv.URL)
		if err := fx.call(t, "permission-request", permStdin); err != nil && c.want != "" {
			t.Errorf("%s: %v", name, err)
		}
		if got := fx.out.String(); got != c.want {
			t.Errorf("%s: stdout %q, want %q", name, got, c.want)
		}
	}
}

func TestPermissionHookRequest(t *testing.T) {
	srv := newPermServer(t, answer{200, `{"state":"decided","decision":"allow"}`})
	fx := newFixture(t, srv.URL)
	if err := fx.call(t, "permission-request", permStdin); err != nil {
		t.Fatal(err)
	}
	_, bodies := srv.seen()
	if len(bodies) != 2 {
		t.Fatalf("requests %q", bodies)
	}
	var in api.PermissionIn
	body := strings.TrimPrefix(bodies[0], "POST /v1/sessions/"+sessionID+"/permissions ")
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatalf("create body %q", bodies[0])
	}
	if in.ToolName != "Bash" || string(in.ToolInput) != `{"command":"git push","description":"Push"}` || in.CWD != "/home/user/proj" ||
		string(in.Suggestions) != `[{"type":"addRules"}]` {
		t.Errorf("create %+v", in)
	}
	if !strings.HasPrefix(bodies[1], "GET /v1/permissions/pr_AAAAAAAAAAAAAAAAAAAAAA/decision") {
		t.Errorf("poll %q", bodies[1])
	}
}

func TestPermissionHookCapsInput(t *testing.T) {
	srv := newPermServer(t, answer{200, `{"state":"expired"}`})
	fx := newFixture(t, srv.URL)
	stdin := `{"session_id":"` + sessionID + `","tool_name":"Write","tool_input":{"file_path":"/x","content":"` +
		strings.Repeat("a", 100<<10) + `"}}`
	if err := fx.call(t, "permission-request", stdin); err != nil {
		t.Fatal(err)
	}
	_, bodies := srv.seen()
	if len(bodies) == 0 || len(bodies[0]) > 10<<10 || !strings.Contains(bodies[0], `"tool_input":"{\"file_path\"`) {
		t.Fatalf("create request %.200q, want a cut input under 10 KiB", bodies)
	}
}

func TestPermissionHookOptOutAndOutage(t *testing.T) {
	srv := newPermServer(t, answer{200, `{"state":"decided","decision":"allow"}`})
	fx := newFixture(t, srv.URL)
	fx.env["SESSIONHUB_REMOTE_PERMISSIONS"] = "off"
	if err := fx.call(t, "permission-request", permStdin); err != nil {
		t.Fatal(err)
	}
	if _, bodies := srv.seen(); fx.out.Len() != 0 || len(bodies) != 0 {
		t.Errorf("opted out, yet printed %q and sent %d requests", fx.out.String(), len(bodies))
	}
	// No server: nothing printed, quickly.
	fx = newFixture(t, "http://127.0.0.1:1")
	start := time.Now()
	_ = fx.call(t, "permission-request", permStdin)
	if fx.out.Len() != 0 || time.Since(start) > 3*time.Second {
		t.Errorf("no server: printed %q after %s", fx.out.String(), time.Since(start))
	}
	// A refused create prints nothing and does not poll.
	srv = newPermServer(t)
	srv.create = 404
	fx = newFixture(t, srv.URL)
	_ = fx.call(t, "permission-request", permStdin)
	if polls, _ := srv.seen(); fx.out.Len() != 0 || polls != 0 {
		t.Errorf("refused create: printed %q, polled %d times", fx.out.String(), polls)
	}
}

func TestPermissionHookWaitsPastHookDeadline(t *testing.T) {
	srv := newPermServer(t) // every poll answers 204
	fx := newFixture(t, srv.URL)
	fx.h.deadline = 20 * time.Millisecond // the 5-second rule for the other hooks
	fx.h.permWait = 300 * time.Millisecond
	start := time.Now()
	if err := fx.call(t, "permission-request", permStdin); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("returned after %s: the hook deadline cut the wait short", d)
	}
	if polls, _ := srv.seen(); fx.out.Len() != 0 || polls < 2 {
		t.Errorf("printed %q after %d polls; want nothing after several", fx.out.String(), polls)
	}
}
```

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/hooks/ -count=1`
Expected: FAIL to compile with `unknown field stdout in struct literal of
type handler` and `undefined: ContextBlock`.

- [ ] **Step 4: Add the handler fields and the dispatch**

In `internal/hooks/hooks.go`, replace:

```go
	// deadline bounds the whole invocation: live sends and the drain.
	deadline time.Duration
	stderr   io.Writer
}
```

with:

```go
	// deadline bounds the whole invocation: live sends and the drain. The
	// context and permission-request hooks do not use it.
	deadline time.Duration
	stderr   io.Writer
	// stdout carries the hook output JSON. Only the context and
	// permission-request hooks write to it.
	stdout io.Writer
	// refreshCmd builds the detached `sessionhub hook refresh-instructions` child
	// that session-start starts. nil skips it.
	refreshCmd func() *exec.Cmd
	// remoteOff reports whether this machine opted out of remote permission
	// answers.
	remoteOff func() bool
	// permWait bounds the permission hook's whole wait; pollWait is one
	// decision long poll.
	permWait, pollWait time.Duration
}
```

In `defaultHandler`, replace:

```go
		deadline:       hookDeadline,
		stderr:         os.Stderr,
	}
}
```

with:

```go
		deadline:       hookDeadline,
		stderr:         os.Stderr,
		stdout:         os.Stdout,
		refreshCmd:     refreshCommand,
		remoteOff: func() bool {
			cfg, err := client.LoadConfig()
			if err != nil {
				return false // newClient fails the same way, and the hook prints nothing
			}
			return client.RemotePermissionsOff(cfg, os.Getenv)
		},
		permWait: permissionWait,
		pollWait: decisionPoll,
	}
}
```

Replace the start of `run`:

```go
func (h *handler) run(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: sessionhub hook <session-start|prompt|stop|notification|session-end>")
	}
	ctx, cancel := context.WithTimeout(ctx, h.deadline)
	defer cancel()
	event := args[0]
	if event == "flush" {
		return h.flush(ctx)
	}
```

with:

```go
func (h *handler) run(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: sessionhub hook <session-start|prompt|stop|notification|session-end|context|permission-request>")
	}
	event := args[0]
	// These two run outside the 5 s deadline: context never touches the
	// network, and permission-request waits up to permWait for an answer.
	switch event {
	case "context":
		return h.printContext()
	case "permission-request":
		return h.permissionRequest(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, h.deadline)
	defer cancel()
	if event == "flush" {
		return h.flush(ctx)
	}
	if event == "refresh-instructions" {
		return h.refreshInstructions(ctx)
	}
```

In the `session-start` case, replace:

```go
		items = []client.Item{h.upsertItem(h.fullUpsert(ctx, in))}
```

with:

```go
		items = []client.Item{h.upsertItem(h.fullUpsert(ctx, in))}
		h.startRefresh()
```

After `digestCommand`, add:

```go
// refreshCommand is `sessionhub hook refresh-instructions`, run by the same binary.
func refreshCommand() *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	return exec.Command(exe, "hook", "refresh-instructions")
}

// startRefresh starts the rules refresh without waiting for it.
func (h *handler) startRefresh() {
	if h.refreshCmd == nil {
		return
	}
	if err := startDetached(h.refreshCmd()); err != nil {
		fmt.Fprintf(h.stderr, "hook: start rules refresh: %v\n", err)
	}
}
```

- [ ] **Step 5: Write the new hooks**

Create `internal/hooks/actions.go`:

```go
package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

const (
	// permissionWait is how long the permission hook waits for an answer:
	// the server's request lifetime. The installed timeout is 660 s.
	permissionWait = 10 * time.Minute
	// decisionPoll is one decision long poll.
	decisionPoll = 30 * time.Second
	// maxSuggestionBytes caps the permission_suggestions the hook forwards;
	// bigger ones are dropped (the server does not store them).
	maxSuggestionBytes = 4 << 10
	// contextHeader starts the rules block.
	contextHeader = "Standing instructions from sessionhub (apply in every session):"
	denyMessage   = "Denied from sessionhub."
)

// ContextBlock is the additionalContext for the rules: the header and one
// "- " line per rule, or "" when no rule has text left after cleaning.
func ContextBlock(list api.InstructionList) string {
	var lines []string
	for _, in := range list.Instructions {
		if t := cleanText(in.Text, 300); t != "" {
			lines = append(lines, "- "+t)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return contextHeader + "\n" + strings.Join(lines, "\n")
}

// contextOutput is the context hook's output.
type contextOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

type permissionDecision struct {
	Behavior string `json:"behavior"`
	Message  string `json:"message,omitempty"`
}

// permissionOutput is the permission hook's output. Structs, not maps, keep
// the key order Claude Code's docs show.
type permissionOutput struct {
	HookSpecificOutput struct {
		HookEventName string             `json:"hookEventName"`
		Decision      permissionDecision `json:"decision"`
	} `json:"hookSpecificOutput"`
}

// writeHookOutput writes v as the hook's one JSON object.
func writeHookOutput(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// printContext is `sessionhub hook context`: it prints the local rules copy as
// additionalContext for SessionStart and UserPromptSubmit. It reads one
// local file and never builds a sessionhub client, so a prompt never waits on the
// network.
func (h *handler) printContext() error {
	raw, err := io.ReadAll(io.LimitReader(h.stdin, maxStdin))
	if err != nil {
		return fmt.Errorf("hook context: read stdin: %w", err)
	}
	var in struct {
		HookEventName string `json:"hook_event_name"`
		AgentID       string `json:"agent_id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("hook context: bad stdin JSON: %w", err)
	}
	if in.AgentID != "" || (in.HookEventName != "SessionStart" && in.HookEventName != "UserPromptSubmit") {
		return nil
	}
	list, err := client.LoadInstructions(client.InstructionsPath(h.stateDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("hook context: read the rules copy: %w", err)
	}
	block := ContextBlock(list)
	if block == "" {
		return nil
	}
	var out contextOutput
	out.HookSpecificOutput.HookEventName = in.HookEventName
	out.HookSpecificOutput.AdditionalContext = block
	return writeHookOutput(h.stdout, out)
}

// refreshInstructions is `sessionhub hook refresh-instructions`, which
// session-start starts detached: it rewrites the local rules copy when the
// server's version changed.
func (h *handler) refreshInstructions(ctx context.Context) error {
	c, err := h.newClient()
	if err != nil {
		return err
	}
	_, err = client.RefreshInstructions(ctx, c, client.InstructionsPath(h.stateDir))
	return err
}

// permissionPayload is the PermissionRequest hook input sessionhub reads.
type permissionPayload struct {
	SessionID   string          `json:"session_id"`
	CWD         string          `json:"cwd"`
	ToolName    string          `json:"tool_name"`
	ToolInput   json.RawMessage `json:"tool_input"`
	Suggestions json.RawMessage `json:"permission_suggestions"`
}

// permissionRequest is `sessionhub hook permission-request`. It posts the request
// and waits up to permWait for a decision from sessionhub. It prints the decision,
// or nothing at all on an opt-out, an error, an expiry, or an answer in the
// terminal, so Claude Code's own dialog applies. Subagent calls are handled
// too: their prompt shows in the same terminal.
func (h *handler) permissionRequest(ctx context.Context) error {
	if h.remoteOff() {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(h.stdin, maxStdin))
	if err != nil {
		return fmt.Errorf("hook permission-request: read stdin: %w", err)
	}
	var in permissionPayload
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("hook permission-request: bad stdin JSON: %w", err)
	}
	if in.SessionID == "" || in.ToolName == "" {
		return errors.New("hook permission-request: stdin has no session_id or tool_name")
	}
	input, _, err := api.CapToolInput(in.ToolInput)
	if err != nil {
		return fmt.Errorf("hook permission-request: tool_input: %w", err)
	}
	suggestions := in.Suggestions
	if len(suggestions) > maxSuggestionBytes {
		suggestions = nil
	}
	c, err := h.newClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, h.permWait)
	defer cancel()
	req, err := c.CreatePermission(ctx, in.SessionID, api.PermissionIn{ToolName: cleanText(in.ToolName, 128),
		ToolInput: input, CWD: foldControl(in.CWD), Suggestions: suggestions})
	if err != nil {
		return fmt.Errorf("hook permission-request: %w", err)
	}
	for {
		r, err := c.WaitDecision(ctx, req.ID, h.pollWait)
		if ctx.Err() != nil {
			return nil // waited permWait: the dialog in the terminal stays
		}
		if err != nil {
			return fmt.Errorf("hook permission-request: %w", err)
		}
		if r == nil {
			continue // 204: still open
		}
		out, ok := decisionOutput(*r)
		if !ok {
			return nil // expired, answered in the terminal, or closed
		}
		return writeHookOutput(h.stdout, out)
	}
}

// decisionOutput is the hook output for a decided request. ok is false for
// any other state or an unknown decision.
func decisionOutput(r api.PermissionRequest) (permissionOutput, bool) {
	var out permissionOutput
	if r.State != api.PermissionDecided {
		return out, false
	}
	out.HookSpecificOutput.HookEventName = "PermissionRequest"
	switch r.Decision {
	case api.DecisionAllow:
		out.HookSpecificOutput.Decision = permissionDecision{Behavior: "allow"}
	case api.DecisionDeny:
		msg := cleanText(r.Reason, 200)
		if msg == "" {
			msg = denyMessage
		}
		out.HookSpecificOutput.Decision = permissionDecision{Behavior: "deny", Message: msg}
	default:
		return out, false
	}
	return out, true
}
```

- [ ] **Step 6: Run the hook tests to verify they pass**

Run: `go test ./internal/hooks/ -run 'Context|Refresh|Permission|SessionStart' -count=1`
Expected: PASS.

- [ ] **Step 7: Install the new entries**

In `internal/hooks/install.go`, replace:

```go
// hookSpec is one row of the docs/dev/PLAN.md table.
type hookSpec struct {
	Event   string
	Matcher string // empty: the group has no matcher key
	Arg     string // `sessionhub hook <Arg>`
	Async   bool
	Timeout int // seconds, 0 for none
}

var hookTable = []hookSpec{
	{"SessionStart", "*", "session-start", true, 0},
	{"UserPromptSubmit", "", "prompt", true, 0},
	{"Stop", "", "stop", true, 0},
	{"Notification", "*", "notification", true, 0},
	{"SessionEnd", "*", "session-end", false, 2},
}
```

with:

```go
// hookSpec is one row of the docs/dev/PLAN.md table.
type hookSpec struct {
	Event   string
	Matcher string // empty: the group has no matcher key
	Arg     string // `sessionhub hook <Arg>`
	Async   bool
	Timeout int // seconds, 0 for none
	// Context adds a second, synchronous entry to the same group:
	// `sessionhub hook context`, which prints the rules. An async hook's output
	// does not reach the prompt, and one group per event keeps
	// installEvents' rule of one sessionhub group.
	Context bool
}

// contextTimeout is the context entry's timeout, in seconds. It reads one
// local file.
const contextTimeout = 5

var hookTable = []hookSpec{
	{"SessionStart", "*", "session-start", true, 0, true},
	{"UserPromptSubmit", "", "prompt", true, 0, true},
	{"Stop", "", "stop", true, 0, false},
	{"Notification", "*", "notification", true, 0, false},
	{"SessionEnd", "*", "session-end", false, 2, false},
	// 660 s: the hook waits up to 10 minutes for an answer from sessionhub.
	{"PermissionRequest", "*", "permission-request", false, 660, false},
}
```

and replace:

```go
func (s hookSpec) group(binary string) matcherGroup {
	g := matcherGroup{Hooks: []hookEntry{{Type: "command", Command: commandFor(binary, s.Arg), Timeout: s.Timeout, Async: s.Async}}}
```

with:

```go
func (s hookSpec) group(binary string) matcherGroup {
	g := matcherGroup{Hooks: []hookEntry{{Type: "command", Command: commandFor(binary, s.Arg), Timeout: s.Timeout, Async: s.Async}}}
	if s.Context {
		g.Hooks = append(g.Hooks, hookEntry{Type: "command", Command: commandFor(binary, "context"), Timeout: contextTimeout})
	}
```

In `internal/hooks/install_test.go`, replace the whole
`TestInstallAddsTheTable` function with:

```go
func TestInstallAddsTheTable(t *testing.T) {
	out := mustInstall(t, realisticSettings)
	got := hubGroups(t, out)
	if len(got) != 6 {
		t.Fatalf("sessionhub events = %d, want 6: %v", len(got), got)
	}
	cmd := func(arg string) string { return testBinary + " hook " + arg }
	ctx := hookEntry{Type: "command", Command: cmd("context"), Timeout: 5}
	want := map[string][]hookEntry{
		"SessionStart":      {{Type: "command", Command: cmd("session-start"), Async: true}, ctx},
		"UserPromptSubmit":  {{Type: "command", Command: cmd("prompt"), Async: true}, ctx},
		"Stop":              {{Type: "command", Command: cmd("stop"), Async: true}},
		"Notification":      {{Type: "command", Command: cmd("notification"), Async: true}},
		"SessionEnd":        {{Type: "command", Command: cmd("session-end"), Timeout: 2}},
		"PermissionRequest": {{Type: "command", Command: cmd("permission-request"), Timeout: 660}},
	}
	for _, spec := range hookTable {
		gs := got[spec.Event]
		if len(gs) != 1 {
			t.Errorf("%s: groups = %+v", spec.Event, gs)
			continue
		}
		g := gs[0]
		if spec.Matcher == "" && g.Matcher != nil {
			t.Errorf("%s: unexpected matcher %q", spec.Event, *g.Matcher)
		}
		if spec.Matcher != "" && (g.Matcher == nil || *g.Matcher != spec.Matcher) {
			t.Errorf("%s: matcher = %v, want %q", spec.Event, g.Matcher, spec.Matcher)
		}
		if !reflect.DeepEqual(g.Hooks, want[spec.Event]) {
			t.Errorf("%s: entries\n got %+v\nwant %+v", spec.Event, g.Hooks, want[spec.Event])
		}
	}
}
```

Add `"reflect"` to the imports of `install_test.go` if it is not there.
Then replace `if n := len(hubGroups(t, in)); n != 5 {` with
`if n := len(hubGroups(t, in)); n != 6 {`, and both
`len(hubGroups(t, string(b))) != 5 {` with
`len(hubGroups(t, string(b))) != 6 {`. If a failure message next to one of
them says "5", change it to "6".

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test ./internal/hooks/ -count=1`
Expected: PASS, including every install, uninstall, and byte-preservation
test.

- [ ] **Step 9: Document it**

In `docs/hooks.md`, replace:

```markdown
| `sessionhub install-hooks` | Adds sessionhub's five hook entries to `settings.json`. |
| `sessionhub uninstall-hooks` | Removes only sessionhub's entries. |
| `sessionhub hook session-start\|prompt\|stop\|notification\|session-end` | Handles one hook call. Claude Code runs this. |
| `sessionhub hook flush` | Internal. Drains the queue. `session-end` starts it. |
```

with:

```markdown
| `sessionhub install-hooks` | Adds sessionhub's hook entries for six events to `settings.json`. |
| `sessionhub uninstall-hooks` | Removes only sessionhub's entries. |
| `sessionhub hook session-start\|prompt\|stop\|notification\|session-end\|context\|permission-request` | Handles one hook call. Claude Code runs this. |
| `sessionhub hook flush` | Internal. Drains the queue. `session-end` starts it. |
| `sessionhub hook refresh-instructions` | Internal. Rewrites the local rules copy. `session-start` starts it. |
```

Replace the "Installed entries" table:

```markdown
| `SessionStart` | `*` | `<sessionhub> hook session-start` | `async: true` |
| `UserPromptSubmit` | none | `<sessionhub> hook prompt` | `async: true` |
| `Stop` | none | `<sessionhub> hook stop` | `async: true` |
| `Notification` | `*` | `<sessionhub> hook notification` | `async: true` |
| `SessionEnd` | `*` | `<sessionhub> hook session-end` | `timeout: 2` |

`SessionEnd` is synchronous because async hooks can be killed at teardown. It
does no network I/O (see below).
```

with:

```markdown
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
```

After the "Hook behavior" table, add:

```markdown
### Standing instructions

`sessionhub hook context` prints the rules from the local copy,
`<state>/instructions.json`, as `additionalContext`:

    {"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"Standing instructions from sessionhub (apply in every session):\n- <rule>\n- <rule>"}}

It reads only that file and never calls the server, so a prompt never waits
on the network. With no copy, an empty list, another event, or a subagent
call, it prints nothing. The herdr watcher refreshes the copy on every
heartbeat, and `session-start` starts a detached
`sessionhub hook refresh-instructions`, which rewrites it when the server's
`version` changed. A rule added on the server reaches a session at its next
prompt after the next refresh.

### Permission requests

`sessionhub hook permission-request` posts the prompt's `tool_name`, `tool_input`
(cut to 8 KiB with `api.CapToolInput`), `cwd`, and `permission_suggestions`
(dropped over 4 KiB) to `POST /v1/sessions/{id}/permissions`, then holds
`GET /v1/permissions/{id}/decision?wait=30` open in a loop for up to 10
minutes. It runs outside the 5 s deadline of the other hooks. Claude Code
shows its own dialog meanwhile; an answer there wins, and the server then
marks the request answered locally.

| Server answer | Hook output |
|---|---|
| decided, allow | `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` |
| decided, deny | the same with `"behavior":"deny","message":"<reason, or Denied from sessionhub.>"` |
| expired, answered locally, closed | nothing |
| an error, no server, or 10 minutes without an answer | nothing |

Nothing means the dialog in the terminal stays. sessionhub never sends
`updatedPermissions`. To opt a machine out, set
`SESSIONHUB_REMOTE_PERMISSIONS=off` in Claude Code's environment, or add
`remote_permissions = false` to `~/.config/sessionhub/config.toml`; the hook then
exits at once without a request.
```

- [ ] **Step 10: Run the full suite and lint**

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1 && make lint`
Expected: every package `ok`, lint clean.

- [ ] **Step 11: Commit**

```bash
git add internal/hooks/hooks.go internal/hooks/actions.go internal/hooks/actions_test.go \
  internal/hooks/hooks_test.go internal/hooks/install.go internal/hooks/install_test.go docs/hooks.md
git commit -m "$(cat <<'EOF'
Print standing rules and answer permission prompts from hooks

sessionhub hook context prints the local rules copy as additionalContext on
SessionStart and UserPromptSubmit, from a synchronous entry beside the
async ones, and never calls the server. session-start refreshes the
copy in a detached child. sessionhub hook permission-request posts the prompt,
waits up to ten minutes for an answer from sessionhub, and prints allow or
deny, or nothing so the terminal dialog applies. SESSIONHUB_REMOTE_PERMISSIONS
=off or remote_permissions = false opts a machine out.

Refs: docs/dev/superpowers/plans/2026-10-02-session-actions.md, Task 6

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 7: Watcher: rules refresh and message delivery

**Files:**
- Modify: `internal/plugin/watcher.go` (`instructionsPath`, the heartbeat
  refresh, `runWatcher`)
- Modify: `internal/plugin/control.go` (`messageFunc`, `deliverMessage`,
  `messageRunner`, `controller.deliver`, `handle`)
- Create: `internal/plugin/messages_test.go`
- Modify: `internal/plugin/helpers_test.go` (`fakeHub` answers
  `GET /v1/instructions`)
- Modify: `docs/plugin.md`

**Interfaces:**
- Consumes: `client.InstructionsPath`, `client.RefreshInstructions`
  (Task 5); `api.ActionMessage`, the `api.Message*` states (Task 1);
  `herdr.Client.PaneGet`, `herdr.Client.AgentPrompt`, `herdr.BlockedError`,
  `herdr.Error`, `herdr.PaneInfo`, `termtext.Clean`; test helpers
  `newTestEnv`, `newLiveHub`, `liveHub.clientFunc`, `fakeHub`.
- Produces:
  - `watcher.instructionsPath string` ("" skips the refresh)
  - `func (w *watcher) refreshInstructions(ctx context.Context, c *client.Client, now time.Time)`
  - `type messageFunc func(ctx context.Context, s api.Session, text string) api.ControlResultIn`
  - `type paneAgent interface { PaneGet(id string) (herdr.PaneInfo, error); AgentPrompt(target, text string) error }`
  - `func deliverMessage(h paneAgent, s api.Session, text string) api.ControlResultIn`
  - `func messageRunner(socket string) messageFunc`
  - `controller.deliver messageFunc` (nil: a message is refused)

- [ ] **Step 1: Let the fake sessionhub serve rules**

In `internal/plugin/helpers_test.go`, in the `fakeHub` struct, after:

```go
	inboxStatus int    // when non-zero, GET /v1/inbox answers this status
	inboxBody   string // the body of GET /v1/inbox
```

add:

```go
	rulesStatus int    // when non-zero, GET /v1/instructions answers this status
	rulesBody   string // the body of GET /v1/instructions
```

and in `newFakeHub`, replace:

```go
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/inbox":
```

with:

```go
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/instructions":
			if h.rulesStatus != 0 {
				http.Error(w, `{"error":"forced"}`, h.rulesStatus)
				return
			}
			w.Write([]byte(h.rulesBody))
			return
		case r.Method == http.MethodGet && r.URL.Path == "/v1/inbox":
```

- [ ] **Step 2: Write the failing tests**

Create `internal/plugin/messages_test.go`:

```go
package plugin

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
)

func TestWatcherRefreshesInstructions(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	path := filepath.Join(e.dir, client.InstructionsFile)
	e.w.instructionsPath = path
	e.sessionhub.mu.Lock()
	e.sessionhub.rulesBody = `{"instructions":[{"id":1,"text":"Be brief."}],"version":"v1"}`
	e.sessionhub.mu.Unlock()
	t0 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	e.w.step(ctx, t0)
	got, err := client.LoadInstructions(path)
	if err != nil || got.Version != "v1" || got.Instructions[0].Text != "Be brief." {
		t.Fatalf("copy after the heartbeat: %+v %v", got, err)
	}
	// A failed read keeps the copy and logs once.
	e.sessionhub.mu.Lock()
	e.sessionhub.rulesStatus = http.StatusInternalServerError
	e.sessionhub.mu.Unlock()
	e.w.step(ctx, t0.Add(heartbeatInterval))
	if got, _ := client.LoadInstructions(path); got.Version != "v1" {
		t.Errorf("a failed refresh changed the copy: %+v", got)
	}
	if !strings.Contains(e.logs.String(), "rules: refresh failed") {
		t.Errorf("no failure line:\n%s", e.logs)
	}
	// Without a path the watcher never asks.
	e2 := newTestEnv(t)
	e2.w.step(ctx, t0)
	if n := e2.sessionhub.count(http.MethodGet, "/v1/instructions"); n != 0 {
		t.Errorf("rules read %d times with no path", n)
	}
}

// fakeAgent is a paneAgent with one pane.
type fakeAgent struct {
	pane      herdr.PaneInfo
	getErr    error
	promptErr error
	prompts   []string // "pane|text"
}

func (f *fakeAgent) PaneGet(id string) (herdr.PaneInfo, error) {
	if f.getErr != nil {
		return herdr.PaneInfo{}, f.getErr
	}
	return f.pane, nil
}

func (f *fakeAgent) AgentPrompt(target, text string) error {
	f.prompts = append(f.prompts, target+"|"+text)
	return f.promptErr
}

func TestDeliverMessageOnlyWhenIdle(t *testing.T) {
	const sid = "11111111-2222-4333-8444-555555555555"
	s := api.Session{ID: sid, HerdrPane: "w1:p1"}
	pane := func(status, session string) herdr.PaneInfo {
		p := herdr.PaneInfo{PaneID: "w1:p1", Agent: "claude", AgentStatus: status}
		if session != "" {
			p.AgentSession = &herdr.AgentSessionInfo{Value: session}
		}
		return p
	}
	herdrErr := &herdr.Error{Code: "pane_not_found", Message: "no pane w1:p1"}
	for name, c := range map[string]struct {
		agent    fakeAgent
		sess     api.Session
		state    string
		detail   string
		prompted bool
	}{
		"idle":           {fakeAgent{pane: pane("idle", sid)}, s, api.MessageDelivered, "", true},
		"done":           {fakeAgent{pane: pane("done", sid)}, s, api.MessageDelivered, "", true},
		"idle, no id":    {fakeAgent{pane: pane("idle", "")}, s, api.MessageDelivered, "", true},
		"working":        {fakeAgent{pane: pane("working", sid)}, s, api.MessageBusy, "the agent is working", false},
		"blocked":        {fakeAgent{pane: pane("blocked", sid)}, s, api.MessageBusy, "the agent is blocked", false},
		"other session":  {fakeAgent{pane: pane("idle", "99999999-2222-4333-8444-555555555555")}, s, api.MessageRefused, detailOtherSession, false},
		"no agent":       {fakeAgent{pane: herdr.PaneInfo{PaneID: "w1:p1"}}, s, api.MessageRefused, detailNoAgent, false},
		"no pane":        {fakeAgent{pane: pane("idle", sid)}, api.Session{ID: sid}, api.MessageRefused, detailNotInHerdr, false},
		"pane gone":      {fakeAgent{getErr: herdrErr}, s, api.MessageRefused, detailPaneGone, false},
		"herdr down":     {fakeAgent{getErr: errors.New("dial unix: no such file")}, s, api.MessageBusy, detailHerdrDown, false},
		"agent_blocked":  {fakeAgent{pane: pane("idle", sid), promptErr: &herdr.BlockedError{Err: &herdr.Error{Code: "agent_blocked"}}}, s, api.MessageBusy, detailBlocked, true},
		"prompt refused": {fakeAgent{pane: pane("idle", sid), promptErr: herdrErr}, s, api.MessageRefused, "herdr: pane_not_found: no pane w1:p1", true},
		"prompt failed":  {fakeAgent{pane: pane("idle", sid), promptErr: errors.New("broken pipe")}, s, api.MessageBusy, detailHerdrDown, true},
	} {
		a := c.agent
		got := deliverMessage(&a, c.sess, "From the user via sessionhub (dashboard):\n\nPlease rebase.")
		if got.State != c.state || got.Detail != c.detail {
			t.Errorf("%s: %+v, want %s %q", name, got, c.state, c.detail)
		}
		if prompted := len(a.prompts) > 0; prompted != c.prompted {
			t.Errorf("%s: prompted=%v, want %v", name, prompted, c.prompted)
		}
		if c.prompted && a.prompts[0] != "w1:p1|From the user via sessionhub (dashboard):\n\nPlease rebase." {
			t.Errorf("%s: prompt %q", name, a.prompts[0])
		}
	}
}

func TestControllerDeliversMessage(t *testing.T) {
	h := newLiveHub(t)
	ctx := context.Background()
	c, _ := h.clientFunc()()
	const sid = "11111111-2222-4333-8444-555555555555"
	if err := c.UpsertSession(ctx, api.SessionUpsert{ID: sid, Agent: "claude", Source: api.SourcePlugin,
		HerdrSession: "default", HerdrPane: "w1:p1"}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.RecordPoll(ctx, h.id); err != nil {
		t.Fatal(err)
	}
	res, _, err := h.st.SendMessages(ctx, []string{sid}, "Please rebase.", "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	ctl := newController(h.clientFunc(), nil, log.New(&logs, "", 0))
	ctl.wait = time.Second
	results := []api.ControlResultIn{{State: api.MessageBusy, Detail: "the agent is working"}, {State: api.MessageDelivered}}
	var got []string
	ctl.deliver = func(_ context.Context, s api.Session, text string) api.ControlResultIn {
		got = append(got, s.ID+"|"+s.HerdrPane+"|"+text)
		r := results[0]
		results = results[1:]
		return r
	}
	ctl.once(ctx)
	m, _ := h.st.GetMessage(ctx, res[0].ID)
	if m.State != api.MessageQueued || m.Detail != "the agent is working" {
		t.Fatalf("after busy: %+v", m)
	}
	// Offered again only after store.MessageRetry: age the offer through a
	// second connection, as the server tests read tables.
	db, err := sql.Open("sqlite", filepath.Join(h.dir, "sessionhub.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE messages SET offered_at = '2000-01-01T00:00:00.000000000Z' WHERE id = ?`, res[0].ID); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ctl.once(ctx)
	if m, _ := h.st.GetMessage(ctx, res[0].ID); m.State != api.MessageDelivered {
		t.Fatalf("after delivered: %+v", m)
	}
	want := sid + "|w1:p1|From the user via sessionhub (dashboard):\n\nPlease rebase."
	if len(got) != 2 || got[0] != want || got[1] != want {
		t.Errorf("deliveries %q", got)
	}
	if strings.Contains(logs.String(), "Please rebase.") {
		t.Errorf("the log holds the message text:\n%s", logs.String())
	}
}

func TestControllerWithoutDeliveryRefuses(t *testing.T) {
	h := newLiveHub(t)
	ctx := context.Background()
	c, _ := h.clientFunc()()
	const sid = "11111111-2222-4333-8444-555555555555"
	c.UpsertSession(ctx, api.SessionUpsert{ID: sid, Agent: "claude", Source: api.SourcePlugin, HerdrSession: "default", HerdrPane: "w1:p1"})
	h.st.RecordPoll(ctx, h.id)
	res, _, _ := h.st.SendMessages(ctx, []string{sid}, "hi", "dashboard")
	ctl := newController(h.clientFunc(), nil, log.New(&bytes.Buffer{}, "", 0))
	ctl.wait = time.Second
	ctl.once(ctx)
	if m, _ := h.st.GetMessage(ctx, res[0].ID); m.State != api.MessageRefused || m.Detail != detailNoDelivery {
		t.Errorf("message %+v", m)
	}
}
```

The test ages the offer through `h.dir`, which `liveHub` does not have yet. Give it the database directory: in `internal/plugin/control_test.go`
replace:

```go
type liveHub struct {
	st  *store.Store
	srv *httptest.Server
	tok string
	id  int64
}

func newLiveHub(t *testing.T) *liveHub {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sessionhub.db"), store.Options{})
```

with:

```go
type liveHub struct {
	st  *store.Store
	srv *httptest.Server
	tok string
	id  int64
	dir string // holds sessionhub.db
}

func newLiveHub(t *testing.T) *liveHub {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sessionhub.db"), store.Options{})
```

and replace:

```go
	return &liveHub{st: st, srv: srv, tok: tok, id: m.ID}
```

with:

```go
	return &liveHub{st: st, srv: srv, tok: tok, id: m.ID, dir: dir}
```

The sqlite driver is registered because the package's tests import
`internal/store` through `control_test.go`.

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/plugin/ -run 'TestWatcherRefreshesInstructions|TestDeliverMessage|TestController.*Message|TestControllerWithoutDelivery' -count=1`
Expected: FAIL to compile with `e.w.instructionsPath undefined` and
`undefined: deliverMessage`.

- [ ] **Step 4: Refresh the rules on the heartbeat**

In `internal/plugin/watcher.go`, in the `watcher` struct, after:

```go
	workspaces  []string          // workspace IDs from the last snapshot
	sidebarText map[string]string // workspace ID → inbox text it last accepted
```

add:

```go
	// instructionsPath is the local rules copy the heartbeat refreshes; ""
	// turns the refresh off. newWatcher leaves it empty, runWatcher sets it.
	instructionsPath string
```

In `heartbeat`, replace:

```go
	w.lastBeatOK = now
	w.reportInbox(ctx, c, now)
```

with:

```go
	w.lastBeatOK = now
	w.reportInbox(ctx, c, now)
	w.refreshInstructions(ctx, c, now)
```

After `reportInbox`, add:

```go
// refreshInstructions rewrites the local rules copy when the server's
// version changed. A failure keeps the old copy; sessions keep seeing it.
func (w *watcher) refreshInstructions(ctx context.Context, c *client.Client, now time.Time) {
	if w.instructionsPath == "" {
		return
	}
	changed, err := client.RefreshInstructions(ctx, c, w.instructionsPath)
	if err != nil {
		w.logf(now, "rules: refresh failed: %v", err)
		return
	}
	if changed {
		w.log.Printf("rules: local copy updated")
	}
}
```

In `runWatcher`, replace:

```go
	w.sidebar = hc
```

with:

```go
	w.sidebar = hc
	w.instructionsPath = client.InstructionsPath(stateDir)
```

and replace:

```go
	ctl := newController(newClient, controlRunner(socketPath, 0, controlPollFor), logger)
```

with:

```go
	ctl := newController(newClient, controlRunner(socketPath, 0, controlPollFor), logger)
	ctl.deliver = messageRunner(socketPath)
```

- [ ] **Step 5: Deliver messages**

In `internal/plugin/control.go`, add `"errors"` and
`"github.com/abdallah/session-hub/internal/herdr"` to the imports. After the `detailJustResumed`
line in the details block, add:

```go
	detailNotInHerdr   = "the session is not in herdr"
	detailPaneGone     = "the session's pane is gone"
	detailOtherSession = "the pane runs another session"
	detailNoAgent      = "no agent runs in the pane"
	detailHerdrDown    = "herdr did not answer"
	detailNoDelivery   = "this watcher cannot deliver messages"
```

After the `controlFunc` type, add:

```go
// messageFunc submits one message prompt to session s's pane and says what
// happened: delivered, busy (try later), or refused.
type messageFunc func(ctx context.Context, s api.Session, text string) api.ControlResultIn

// paneAgent is the herdr calls message delivery needs (*herdr.Client).
type paneAgent interface {
	PaneGet(id string) (herdr.PaneInfo, error)
	AgentPrompt(target, text string) error
}

// messageRunner is the watcher's messageFunc on the herdr socket.
func messageRunner(socket string) messageFunc {
	return func(_ context.Context, s api.Session, text string) api.ControlResultIn {
		hc, err := herdr.Dial(socket)
		if err != nil {
			return api.ControlResultIn{State: api.MessageBusy, Detail: detailHerdrDown}
		}
		defer hc.Close()
		return deliverMessage(hc, s, text)
	}
}

// deliverMessage types text into s's pane with agent.prompt, but only when
// herdr reports the pane's agent idle or done and the pane runs s (or herdr
// has no session ID for it). A working or blocked agent, or a herdr that
// does not answer, is busy: the server offers the message again later. A
// missing pane, another session, or no agent is refused.
func deliverMessage(h paneAgent, s api.Session, text string) api.ControlResultIn {
	busy := func(d string) api.ControlResultIn { return api.ControlResultIn{State: api.MessageBusy, Detail: d} }
	refused := func(d string) api.ControlResultIn { return api.ControlResultIn{State: api.MessageRefused, Detail: d} }
	if s.HerdrPane == "" {
		return refused(detailNotInHerdr)
	}
	p, err := h.PaneGet(s.HerdrPane)
	var he *herdr.Error
	switch {
	case errors.As(err, &he):
		return refused(detailPaneGone)
	case err != nil:
		return busy(detailHerdrDown)
	}
	if id := p.SessionID(); id != "" && id != s.ID {
		return refused(detailOtherSession)
	}
	switch p.AgentStatus {
	case "idle", "done":
	case "working", "blocked":
		return busy("the agent is " + p.AgentStatus)
	default:
		return refused(detailNoAgent)
	}
	err = h.AgentPrompt(s.HerdrPane, text)
	var be *herdr.BlockedError
	switch {
	case errors.As(err, &be):
		return busy(detailBlocked)
	case errors.As(err, &he):
		return refused(termtext.Clean(he.Error(), api.MaxControlDetailRunes))
	case err != nil:
		return busy(detailHerdrDown)
	}
	return api.ControlResultIn{State: api.MessageDelivered}
}
```

In the `controller` struct, after:

```go
	run       controlFunc
```

add:

```go
	// deliver submits a message; nil refuses every message. runWatcher sets
	// it.
	deliver messageFunc
```

In `handle`, replace:

```go
	in := api.ControlResultIn{State: api.ControlFailed, Detail: "unknown action " + termtext.Clean(req.Action, 40)}
	if req.Action == api.ActionRemoteControl {
		in = resultFor(c.control(ctx, claim.Session))
	}
```

with:

```go
	in := api.ControlResultIn{State: api.ControlFailed, Detail: "unknown action " + termtext.Clean(req.Action, 40)}
	switch req.Action {
	case api.ActionRemoteControl:
		in = resultFor(c.control(ctx, claim.Session))
	case api.ActionMessage:
		in = api.ControlResultIn{State: api.MessageRefused, Detail: detailNoDelivery}
		if c.deliver != nil {
			in = c.deliver(ctx, claim.Session, claim.Text)
		}
	}
```

The log line after it prints the state and detail, never `claim.Text`.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/plugin/ -count=1`
Expected: PASS, including every existing control and watcher test.

- [ ] **Step 7: Document it**

In `docs/plugin.md`, section "Control loop", after the bullet that ends
"report `failed`, \"just resumed; try again in a moment\".", add:

```markdown
- A claim whose action is `message` carries the prompt in `text`. The
  watcher reads the session's pane with `pane.get`. Only when herdr reports
  the agent `idle` or `done`, and the pane runs that session (or herdr has
  no session ID for it), does it send the text with `agent.prompt`, and it
  reports `delivered`. A `working` or `blocked` agent, herdr's
  `agent_blocked` error, or a herdr socket that does not answer is `busy`:
  the message stays queued and the server offers it again 15 seconds after
  the last offer. No pane, a pane that is gone, another session in the pane,
  or no agent is `refused`. The log line never holds the message text.
```

In section "Watcher", add this paragraph after the paragraph about the
sidebar inbox count (it starts "On every 60-second heartbeat"):

```markdown
After each heartbeat the server accepted, the watcher reads
`GET /v1/instructions` and rewrites `<state>/instructions.json` when the
`version` changed. `sessionhub hook context` prints that copy into every prompt. A
failed read keeps the old copy and logs `rules: refresh failed`, at most
once every 5 minutes.
```

In section "Files in the state dir", add a row or bullet in the same form as
the others for `instructions.json`: "The standing rules, as the server last
returned them. The watcher and `sessionhub hook refresh-instructions` write it;
`sessionhub hook context` reads it."

- [ ] **Step 8: Run the full suite and lint**

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1 && make lint`
Expected: every package `ok`, lint clean.

- [ ] **Step 9: Commit**

```bash
git add internal/plugin/watcher.go internal/plugin/control.go internal/plugin/messages_test.go \
  internal/plugin/helpers_test.go internal/plugin/control_test.go docs/plugin.md
git commit -m "$(cat <<'EOF'
Refresh rules and deliver messages from the herdr watcher

Each accepted heartbeat refreshes the local rules copy when the server's
version changed. The control loop now runs message claims too: it types
the prompt with agent.prompt only when herdr reports the pane's agent
idle or done for the same session, reports busy while it works or waits
on a prompt so the server retries, and refuses a gone pane or another
session.

Refs: docs/dev/superpowers/plans/2026-10-02-session-actions.md, Task 7

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 8: MCP: remember, forget, instructions, send_to_sessions

**Files:**
- Modify: `internal/mcp/tools.go` (`hubAPI`, four tools)
- Modify: `internal/mcp/server.go` (dispatch, `toolDefs`)
- Create: `internal/mcp/actions_test.go`
- Modify: `internal/mcp/mcp_test.go` (`TestProtocolTranscript` tool count)
- Modify: `docs/mcp.md`

**Interfaces:**
- Consumes: `Client.Instructions`, `Client.AddInstruction`,
  `Client.DeleteInstruction`, `Client.SendMessages` (Task 5);
  `store.MaxInstructionRunes` as the literal 300; `Server.resolveSession`,
  `text`, `clean`, test helpers `newTestServer`, `writeCurrent`,
  `transcript`, `decode`, `toolText`, `callLine`, `sid1`, `sid2`.
- Produces:
  - `hubAPI` gains `Instructions`, `AddInstruction`, `DeleteInstruction`,
    `SendMessages`
  - `func (s *Server) remember(ctx context.Context, args json.RawMessage) toolResult`
  - `func (s *Server) forget(ctx context.Context, args json.RawMessage) toolResult`
  - `func (s *Server) instructions(ctx context.Context, args json.RawMessage) toolResult`
  - `func (s *Server) sendToSessions(ctx context.Context, args json.RawMessage) toolResult`

- [ ] **Step 1: Write the failing tests**

Create `internal/mcp/actions_test.go`:

```go
package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

// actionHub answers the rule and message routes the way the server does.
type actionHub struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string // "METHOD path body"
}

func newActionHub(t *testing.T) *actionHub {
	h := &actionHub{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies = append(h.bodies, r.Method+" "+r.URL.Path+" "+string(b))
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/instructions":
			var in api.InstructionIn
			json.Unmarshal(b, &in)
			if strings.Contains(in.Text, "FULL") {
				w.WriteHeader(409)
				io.WriteString(w, `{"error":"full: the rules use 1900 of 2000 characters and this one has 150; remove a rule first"}`)
				return
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(api.Instruction{ID: 7, Text: in.Text, CreatedBy: "tower"})
		case r.Method == "GET" && r.URL.Path == "/v1/instructions":
			io.WriteString(w, `{"instructions":[{"id":3,"text":"Never push to main.","created_by":"tower"},`+
				`{"id":7,"text":"Write in British English.","created_by":"web:phone"}],"version":"abc"}`)
		case r.Method == "DELETE" && r.URL.Path == "/v1/instructions/7":
			w.WriteHeader(204)
		case r.Method == "DELETE":
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"not found: rule 8"}`)
		case r.Method == "POST" && r.URL.Path == "/v1/messages":
			var in api.MessagesIn
			json.Unmarshal(b, &in)
			out := api.MessagesOut{}
			for _, id := range in.SessionIDs {
				if id == sid2 {
					out.Results = append(out.Results, api.MessageResult{SessionID: id, State: api.MessageRefused, Detail: "the session is not in herdr"})
					continue
				}
				out.Results = append(out.Results, api.MessageResult{SessionID: id, ID: "msg_x", State: api.MessageQueued})
			}
			json.NewEncoder(w).Encode(out)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"no such endpoint"}`)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *actionHub) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.bodies...)
}

func callTool(t *testing.T, s *Server, name, args string) (string, bool) {
	t.Helper()
	got := transcript(t, s, callLine(1, name, args))
	if len(got) != 1 {
		t.Fatalf("%s: %d responses", name, len(got))
	}
	return toolText(t, decode(t, got[0]))
}

func TestRememberAndForget(t *testing.T) {
	h := newActionHub(t)
	s, _, _ := newTestServer(t, h.URL, 1)
	msg, isErr := callTool(t, s, "remember", `{"text":"  Write in\nBritish English.  "}`)
	if isErr || !strings.Contains(msg, "rule 7 saved") || !strings.Contains(msg, "next prompt") {
		t.Errorf("remember: %q %v", msg, isErr)
	}
	if b := h.seen(); len(b) != 1 || b[0] != `POST /v1/instructions {"text":"Write in British English."}` {
		t.Errorf("request %q", b)
	}
	if msg, isErr := callTool(t, s, "remember", `{"text":"FULL rule"}`); !isErr || !strings.Contains(msg, "full") ||
		!strings.Contains(msg, "forget") {
		t.Errorf("full: %q %v", msg, isErr)
	}
	if msg, isErr := callTool(t, s, "remember", `{"text":" \n "}`); !isErr || !strings.Contains(msg, "empty") {
		t.Errorf("empty: %q %v", msg, isErr)
	}
	if msg, isErr := callTool(t, s, "remember", `{"text":"`+strings.Repeat("x", 301)+`"}`); !isErr || !strings.Contains(msg, "300") {
		t.Errorf("too long: %q %v", msg, isErr)
	}
	if msg, isErr := callTool(t, s, "forget", `{"id":7}`); isErr || !strings.Contains(msg, "rule 7 removed") {
		t.Errorf("forget: %q %v", msg, isErr)
	}
	if msg, isErr := callTool(t, s, "forget", `{"id":8}`); !isErr || !strings.Contains(msg, "no rule 8") {
		t.Errorf("forget unknown: %q %v", msg, isErr)
	}
	if msg, isErr := callTool(t, s, "forget", `{"id":"seven"}`); !isErr {
		t.Errorf("forget with a string id: %q", msg)
	}
}

func TestInstructionsTool(t *testing.T) {
	h := newActionHub(t)
	s, _, _ := newTestServer(t, h.URL, 1)
	msg, isErr := callTool(t, s, "instructions", `{}`)
	want := "sessionhub standing rules:\n3. Never push to main. (added by tower)\n7. Write in British English. (added by web:phone)"
	if isErr || msg != want {
		t.Errorf("instructions:\n got %q\nwant %q", msg, want)
	}
	down, _, _ := newTestServer(t, "http://127.0.0.1:1", 1)
	if msg, isErr := callTool(t, down, "instructions", `{}`); !isErr || !strings.Contains(msg, "could not") {
		t.Errorf("server down: %q %v", msg, isErr)
	}
}

func TestSendToSessions(t *testing.T) {
	h := newActionHub(t)
	s, _, state := newTestServer(t, h.URL, 4242)
	writeCurrent(t, state, 4242, sid1)
	msg, isErr := callTool(t, s, "send_to_sessions", `{"session_ids":["`+sid1+`","`+sid2+`"],"text":"Please rebase.\nThanks."}`)
	if isErr {
		t.Fatalf("send: %q", msg)
	}
	for _, want := range []string{"11111111: queued", "66666666: refused: the session is not in herdr"} {
		if !strings.Contains(msg, want) {
			t.Errorf("result %q lacks %q", msg, want)
		}
	}
	b := h.seen()
	var in api.MessagesIn
	json.Unmarshal([]byte(strings.TrimPrefix(b[0], "POST /v1/messages ")), &in)
	if in.Text != "Please rebase.\nThanks." || in.FromSession != sid1 || len(in.SessionIDs) != 2 {
		t.Errorf("request %+v", in)
	}
	if msg, isErr := callTool(t, s, "send_to_sessions", `{"session_ids":[],"text":"x"}`); !isErr {
		t.Errorf("no targets: %q", msg)
	}
	if msg, isErr := callTool(t, s, "send_to_sessions", `{"session_ids":["`+sid1+`"],"text":"  "}`); !isErr {
		t.Errorf("blank text: %q", msg)
	}
}
```

In `internal/mcp/mcp_test.go` (`TestProtocolTranscript`), replace:

```go
	if len(names) != 2 || names["report_progress"] == nil || names["set_title"] == nil {
		t.Errorf("tools = %v", names)
	}
```

with:

```go
	if len(names) != 6 || names["report_progress"] == nil || names["set_title"] == nil || names["remember"] == nil ||
		names["forget"] == nil || names["instructions"] == nil || names["send_to_sessions"] == nil {
		t.Errorf("tools = %v", names)
	}
	if d, _ := names["remember"]["description"].(string); !strings.Contains(d, "only when the user asks") {
		t.Errorf("remember description %q", d)
	}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/mcp/ -count=1`
Expected: FAIL: `TestProtocolTranscript` finds 2 tools, and the new tests
get `unknown tool "remember"`.

- [ ] **Step 3: Add the tools**

In `internal/mcp/tools.go`, replace:

```go
type hubAPI interface {
	UpsertSession(ctx context.Context, s api.SessionUpsert) error
	PostReport(ctx context.Context, id string, r api.ReportIn) error
	SetTitle(ctx context.Context, id, title string) error
}
```

with:

```go
type hubAPI interface {
	UpsertSession(ctx context.Context, s api.SessionUpsert) error
	PostReport(ctx context.Context, id string, r api.ReportIn) error
	SetTitle(ctx context.Context, id, title string) error
	Instructions(ctx context.Context) (api.InstructionList, error)
	AddInstruction(ctx context.Context, text string) (api.Instruction, error)
	DeleteInstruction(ctx context.Context, id int64) error
	SendMessages(ctx context.Context, in api.MessagesIn) (api.MessagesOut, error)
}
```

Add at the end of `tools.go`:

```go
// maxRuleLen is the server's limit on one rule (store.MaxInstructionRunes).
const maxRuleLen = 300

// serverMessage is a short reason for a failed call: the server's message
// for an HTTP error, else the error text.
func serverMessage(err error) string {
	var se *client.StatusError
	if errors.As(err, &se) {
		return se.Message
	}
	return err.Error()
}

// remember adds a standing rule. Nothing is queued: the user asked for it
// now, so a failure is reported.
func (s *Server) remember(ctx context.Context, args json.RawMessage) toolResult {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return text("invalid arguments: "+err.Error(), true)
	}
	rule := strings.Join(strings.Fields(clean(in.Text, 1<<20)), " ")
	if rule == "" {
		return text("text is empty: say the rule in one line", true)
	}
	if n := len([]rune(rule)); n > maxRuleLen {
		return text(fmt.Sprintf("the rule is %d characters; keep it to %d", n, maxRuleLen), true)
	}
	a, err := s.API()
	if err != nil {
		return text("sessionhub: the rule could not be saved: "+err.Error(), true)
	}
	r, err := a.AddInstruction(ctx, rule)
	var se *client.StatusError
	switch {
	case errors.As(err, &se) && se.Status == 409:
		return text("sessionhub: the rules are full ("+se.Message+"). Ask the user which rule to remove, call forget with its ID, then try again.", true)
	case err != nil:
		return text("sessionhub: the rule could not be saved: "+serverMessage(err), true)
	}
	return text(fmt.Sprintf("sessionhub: rule %d saved: %q. Every session on every machine sees it from its next prompt.", r.ID, r.Text), false)
}

// forget removes a standing rule by ID.
func (s *Server) forget(ctx context.Context, args json.RawMessage) toolResult {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.ID <= 0 {
		return text("invalid arguments: id must be a rule number from the instructions tool", true)
	}
	a, err := s.API()
	if err != nil {
		return text("sessionhub: the rule could not be removed: "+err.Error(), true)
	}
	if err := a.DeleteInstruction(ctx, in.ID); err != nil {
		if client.IsNotFound(err) {
			return text(fmt.Sprintf("sessionhub: there is no rule %d; call instructions to list them.", in.ID), true)
		}
		return text("sessionhub: the rule could not be removed: "+serverMessage(err), true)
	}
	return text(fmt.Sprintf("sessionhub: rule %d removed.", in.ID), false)
}

// instructions lists the standing rules with their IDs.
func (s *Server) instructions(ctx context.Context, _ json.RawMessage) toolResult {
	a, err := s.API()
	if err != nil {
		return text("sessionhub: could not read the rules: "+err.Error(), true)
	}
	list, err := a.Instructions(ctx)
	if err != nil {
		return text("sessionhub: could not read the rules: "+serverMessage(err), true)
	}
	if len(list.Instructions) == 0 {
		return text("sessionhub: there are no standing rules.", false)
	}
	lines := []string{"sessionhub standing rules:"}
	for _, r := range list.Instructions {
		lines = append(lines, fmt.Sprintf("%d. %s (added by %s)", r.ID, clean(r.Text, maxRuleLen), clean(r.CreatedBy, 100)))
	}
	return text(strings.Join(lines, "\n"), false)
}

// sendToSessions sends a message to other sessions as this one.
func (s *Server) sendToSessions(ctx context.Context, args json.RawMessage) toolResult {
	var in struct {
		SessionIDs []string `json:"session_ids"`
		Text       string   `json:"text"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return text("invalid arguments: "+err.Error(), true)
	}
	if len(in.SessionIDs) == 0 || len(in.SessionIDs) > 20 {
		return text("session_ids must list 1 to 20 full session IDs", true)
	}
	msg := strings.TrimSpace(in.Text)
	if msg == "" {
		return text("text is empty", true)
	}
	a, err := s.API()
	if err != nil {
		return text("sessionhub: the message could not be sent: "+err.Error(), true)
	}
	out, err := a.SendMessages(ctx, api.MessagesIn{SessionIDs: in.SessionIDs, Text: msg, FromSession: s.resolveSession()})
	if err != nil {
		return text("sessionhub: the message could not be sent: "+serverMessage(err), true)
	}
	lines := make([]string, 0, len(out.Results))
	for _, r := range out.Results {
		id := clean(r.SessionID, 8)
		if r.State == api.MessageQueued {
			lines = append(lines, id+": queued; it is typed into the session when its agent is idle (within 10 minutes)")
		} else {
			lines = append(lines, id+": refused: "+clean(r.Detail, 200))
		}
	}
	return text(strings.Join(lines, "\n"), false)
}
```

`clean` cuts to `max` runes, so `clean(r.SessionID, 8)` is the short ID.

In `internal/mcp/server.go`, replace:

```go
		case "set_title":
			return s.setTitle(ctx, p.Arguments), nil
		}
```

with:

```go
		case "set_title":
			return s.setTitle(ctx, p.Arguments), nil
		case "remember":
			return s.remember(ctx, p.Arguments), nil
		case "forget":
			return s.forget(ctx, p.Arguments), nil
		case "instructions":
			return s.instructions(ctx, p.Arguments), nil
		case "send_to_sessions":
			return s.sendToSessions(ctx, p.Arguments), nil
		}
```

and add these entries at the end of `toolDefs`, after the `set_title` entry:

```go
	{
		"name": "remember",
		"description": "Add a standing rule that every Claude session on every machine sees at its start and with " +
			"every prompt, through sessionhub. Use it only when the user asks for something to apply to all sessions, for " +
			"example \"from now on, in every session, ...\". Do not use it for this session's own preferences, for " +
			"notes, or on your own initiative. Keep the rule to one short line, at most 300 characters.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string", "description": "The rule, in one line."},
			},
			"required": []string{"text"},
		},
	},
	{
		"name":        "forget",
		"description": "Remove a standing rule by its number. Call instructions first to find the number. Use it only when the user asks.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "integer", "description": "The rule number from instructions."},
			},
			"required": []string{"id"},
		},
	},
	{
		"name":        "instructions",
		"description": "List the standing rules every session sees, with their numbers and who added them.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name": "send_to_sessions",
		"description": "Send a message to other Claude sessions through sessionhub. sessionhub types it into each session's terminal, " +
			"prefixed with who sent it, once that session's agent is idle. Use it only when the user asks you to " +
			"message other sessions. Session IDs are full IDs, for example from `sessionhub ls`.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_ids": stringArray("Full session IDs, 1 to 20."),
				"text":        map[string]any{"type": "string", "description": "The message."},
			},
			"required": []string{"session_ids", "text"},
		},
	},
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/mcp/ -count=1`
Expected: PASS.

- [ ] **Step 5: Document it**

In `docs/mcp.md`, replace:

```markdown
| `report_progress` | `done: string[]`, `in_flight: string[]`, `waiting_on: string[]`, `note?: string` |
| `set_title` | `title: string` |

Results are one text content item. Invalid arguments (a non-array list, an
empty title) return a result with `isError: true`. Everything else succeeds,
including when the server is down (fail open).
```

with:

```markdown
| `report_progress` | `done: string[]`, `in_flight: string[]`, `waiting_on: string[]`, `note?: string` |
| `set_title` | `title: string` |
| `remember` | `text: string` |
| `forget` | `id: integer` |
| `instructions` | none |
| `send_to_sessions` | `session_ids: string[]`, `text: string` |

Results are one text content item. For `report_progress` and `set_title`,
invalid arguments (a non-array list, an empty title) return a result with
`isError: true`, and everything else succeeds, including when the server is
down (fail open).

`remember`, `forget`, `instructions`, and `send_to_sessions` act at once and
never queue: a failure, including an unreachable server, is a result with
`isError: true` that says what happened. `remember` adds a standing rule
(see `docs/server.md`, "Shared instructions"); its description tells Claude
to use it only when the user asks for something to apply to all sessions.
It folds the text to one line and refuses more than 300 characters. A full
list (2,000 characters) tells Claude to ask the user which rule to `forget`.
`send_to_sessions` sends as `session <first 8 characters>` of the calling
session and lists each target as queued or refused with the reason.
```

- [ ] **Step 6: Run the full suite and lint**

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1 && make lint`
Expected: every package `ok`, lint clean.

- [ ] **Step 7: Commit**

```bash
git add internal/mcp/tools.go internal/mcp/server.go internal/mcp/actions_test.go internal/mcp/mcp_test.go docs/mcp.md
git commit -m "$(cat <<'EOF'
Add MCP tools for standing rules and messages to sessions

remember, forget, and instructions edit and list the rules every session
sees; remember's description limits it to explicit user requests.
send_to_sessions sends a message as the calling session. These tools act
at once and report failures instead of queueing.

Refs: docs/dev/superpowers/plans/2026-10-02-session-actions.md, Task 8

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 9: CLI: rules, send, approve, deny, and the inbox line

**Files:**
- Create: `internal/cli/actions.go` (`RunRules`, `RunSend`, `RunApprove`,
  `RunDeny`, `actionsAPI`)
- Create: `internal/cli/actions_test.go`
- Modify: `internal/cli/cli.go` (`env.actions`, `env.pause`, `defaultEnv`)
- Modify: `internal/cli/inbox.go` (`inboxLine` shows the request)
- Modify: `cmd/sessionhub/main.go` (usage, routes)
- Modify: `cmd/sessionhub/main_test.go` (`commands`)
- Modify: `docs/cli.md`

**Interfaces:**
- Consumes: the client calls (Task 5); `api.PermissionInputText` (Task 1);
  `findInboxItem`, `shortID`, `title`, `clean`, `ExitError`, `env`,
  test values `now` and `inboxFixture`.
- Produces:
  - `type actionsAPI interface` (`Instructions`, `AddInstruction`,
    `DeleteInstruction`, `SendMessages`, `GetMessage`, `DecidePermission`)
  - `env.actions actionsAPI`, `env.pause func(time.Duration)`
  - `func RunRules(ctx context.Context, args []string) error`
  - `func RunSend(ctx context.Context, args []string) error`
  - `func RunApprove(ctx context.Context, args []string) error`
  - `func RunDeny(ctx context.Context, args []string) error`
  - constants `sendWait` (15 seconds), `sendPoll` (1 second)

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/actions_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

// fakeActions is a sessionhub with sessions, rules, messages, and permission
// requests.
type fakeActions struct {
	sessions []api.Session
	rules    []api.Instruction
	addErr   error
	sent     []api.MessagesIn
	results  []api.MessageResult
	// states is what GetMessage returns for a message, one per read; the
	// last one repeats.
	states   map[string][]api.Message
	decided  []string // "id decision reason"
	decideErr error
	inbox    api.Inbox
}

func (f *fakeActions) ListSessions(_ context.Context, live bool, machine string) ([]api.Session, error) {
	var out []api.Session
	for _, s := range f.sessions {
		if machine == "" || s.Machine == machine {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeActions) GetSession(_ context.Context, prefix string) (api.SessionDetail, error) {
	var hits []api.Session
	for _, s := range f.sessions {
		if strings.HasPrefix(s.ID, prefix) {
			hits = append(hits, s)
		}
	}
	switch len(hits) {
	case 0:
		return api.SessionDetail{}, &client.StatusError{Status: 404, Message: "not found: session " + prefix}
	case 1:
		return api.SessionDetail{Session: hits[0]}, nil
	}
	return api.SessionDetail{}, &client.StatusError{Status: 409, Message: fmt.Sprintf("id prefix %q matches %d sessions", prefix, len(hits))}
}

func (f *fakeActions) Health(context.Context) error { return nil }

func (f *fakeActions) Instructions(context.Context) (api.InstructionList, error) {
	return api.InstructionList{Instructions: f.rules, Version: "v"}, nil
}

func (f *fakeActions) AddInstruction(_ context.Context, text string) (api.Instruction, error) {
	if f.addErr != nil {
		return api.Instruction{}, f.addErr
	}
	r := api.Instruction{ID: int64(len(f.rules) + 1), Text: text, CreatedBy: "tower"}
	f.rules = append(f.rules, r)
	return r, nil
}

func (f *fakeActions) DeleteInstruction(_ context.Context, id int64) error {
	for i, r := range f.rules {
		if r.ID == id {
			f.rules = append(f.rules[:i], f.rules[i+1:]...)
			return nil
		}
	}
	return &client.StatusError{Status: 404, Message: fmt.Sprintf("not found: rule %d", id)}
}

func (f *fakeActions) SendMessages(_ context.Context, in api.MessagesIn) (api.MessagesOut, error) {
	f.sent = append(f.sent, in)
	return api.MessagesOut{Results: f.results}, nil
}

func (f *fakeActions) GetMessage(_ context.Context, id string) (api.Message, error) {
	list := f.states[id]
	m := list[0]
	if len(list) > 1 {
		f.states[id] = list[1:]
	}
	return m, nil
}

func (f *fakeActions) DecidePermission(_ context.Context, id string, in api.DecisionIn) (api.PermissionRequest, error) {
	if f.decideErr != nil {
		return api.PermissionRequest{}, f.decideErr
	}
	f.decided = append(f.decided, strings.TrimSpace(id+" "+in.Decision+" "+in.Reason))
	return api.PermissionRequest{ID: id, State: api.PermissionDecided, Decision: in.Decision}, nil
}

func (f *fakeActions) Inbox(context.Context) (api.Inbox, error) { return f.inbox, nil }
func (f *fakeActions) DismissInbox(context.Context, string, time.Time) error {
	return errors.New("not used")
}
func (f *fakeActions) SnoozeInbox(context.Context, string, time.Time, time.Time) error {
	return errors.New("not used")
}

func actionsEnv(f *fakeActions) (*env, *bytes.Buffer, *time.Time) {
	var out bytes.Buffer
	clock := now
	e := &env{api: f, inbox: f, actions: f, out: &out, width: func() int { return 100 },
		now: func() time.Time { return clock }, pause: func(d time.Duration) { clock = clock.Add(d) }}
	return e, &out, &clock
}

func TestRules(t *testing.T) {
	f := &fakeActions{}
	e, out, _ := actionsEnv(f)
	ctx := context.Background()
	if err := e.rules(ctx, nil); err != nil || out.String() != "no standing rules\n" {
		t.Errorf("empty ls: %q %v", out.String(), err)
	}
	out.Reset()
	if err := e.rules(ctx, []string{"add", "Never push to main."}); err != nil || out.String() != "added rule 1\n" {
		t.Errorf("add: %q %v", out.String(), err)
	}
	e.rules(ctx, []string{"add", "Write in \x1b[31mBritish English."})
	out.Reset()
	if err := e.rules(ctx, []string{"ls"}); err != nil {
		t.Fatal(err)
	}
	want := "1  Never push to main.  (tower)\n2  Write in [31mBritish English.  (tower)\n"
	if out.String() != want {
		t.Errorf("ls:\n got %q\nwant %q", out.String(), want)
	}
	out.Reset()
	if err := e.rules(ctx, []string{"rm", "1"}); err != nil || out.String() != "removed rule 1\n" {
		t.Errorf("rm: %q %v", out.String(), err)
	}
	if err := e.rules(ctx, []string{"rm", "9"}); err == nil || !strings.Contains(err.Error(), "rule 9") {
		t.Errorf("rm unknown: %v", err)
	}
	for _, bad := range [][]string{{"rm"}, {"rm", "x"}, {"add"}, {"add", "a", "b"}, {"frob"}} {
		if err := e.rules(ctx, bad); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%q: %v, want usage", bad, err)
		}
	}
	f.addErr = &client.StatusError{Status: 409, Message: "full: remove a rule first"}
	if err := e.rules(ctx, []string{"add", "x"}); err == nil || !strings.Contains(err.Error(), "remove a rule first") {
		t.Errorf("full: %v", err)
	}
}

func sendFixture() *fakeActions {
	return &fakeActions{sessions: []api.Session{
		{ID: "aaaaaaaa-1111", Title: "fix login", Machine: "tower", Status: api.StatusLive, Controllable: true},
		{ID: "bbbbbbbb-2222", Title: "docs", Machine: "tower", Status: api.StatusLive, Controllable: true},
		{ID: "cccccccc-3333", Title: "hooks only", Machine: "tower", Status: api.StatusLive},
		{ID: "dddddddd-4444", Title: "on bluebox", Machine: "bluebox", Status: api.StatusLive, Controllable: true},
		{ID: "aaaabbbb-5555", Title: "twin", Machine: "bluebox", Status: api.StatusLive, Controllable: true},
	}}
}

func TestSendWaitsForDelivery(t *testing.T) {
	f := sendFixture()
	f.results = []api.MessageResult{
		{SessionID: "aaaaaaaa-1111", ID: "msg_a", State: api.MessageQueued},
		{SessionID: "bbbbbbbb-2222", ID: "msg_b", State: api.MessageQueued},
	}
	f.states = map[string][]api.Message{
		"msg_a": {{State: api.MessageQueued}, {State: api.MessageDelivered}},
		"msg_b": {{State: api.MessageQueued, Detail: "the agent is working"}},
	}
	e, out, clock := actionsEnv(f)
	err := e.send(context.Background(), []string{"aaaaaaaa", "bbbb", "-m", "Please rebase."})
	if len(f.sent) != 1 || strings.Join(f.sent[0].SessionIDs, ",") != "aaaaaaaa-1111,bbbbbbbb-2222" || f.sent[0].Text != "Please rebase." {
		t.Fatalf("sent %+v", f.sent)
	}
	want := "aaaaaaaa  fix login  queued\n" +
		"bbbbbbbb  docs  queued\n" +
		"aaaaaaaa  delivered\n" +
		"bbbbbbbb  not delivered yet: the agent is working; sessionhub keeps trying for 10 minutes\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
	if err != nil {
		t.Errorf("err %v, want nil: nothing was refused", err)
	}
	if waited := clock.Sub(now); waited < sendWait || waited > sendWait+2*sendPoll {
		t.Errorf("waited %s, want about %s", waited, sendWait)
	}
}

func TestSendRefusedAndMachine(t *testing.T) {
	f := sendFixture()
	f.results = []api.MessageResult{{SessionID: "dddddddd-4444", ID: "msg_d", State: api.MessageQueued},
		{SessionID: "aaaabbbb-5555", State: api.MessageRefused, Detail: "the session is not in herdr"}}
	f.states = map[string][]api.Message{"msg_d": {{State: api.MessageDelivered}}}
	e, out, _ := actionsEnv(f)
	err := e.send(context.Background(), []string{"--machine", "bluebox", "-m", "hi"})
	if strings.Join(f.sent[0].SessionIDs, ",") != "dddddddd-4444,aaaabbbb-5555" {
		t.Errorf("--machine targets %v", f.sent[0].SessionIDs)
	}
	if !strings.Contains(out.String(), "aaaabbbb  twin  refused: the session is not in herdr") {
		t.Errorf("output %q", out.String())
	}
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Errorf("a refused target: err %v, want exit 1", err)
	}
	// --machine skips sessions that cannot take a message.
	e, _, _ = actionsEnv(f)
	f.sent = nil
	e.send(context.Background(), []string{"--machine", "tower", "-m", "hi"})
	if got := strings.Join(f.sent[0].SessionIDs, ","); got != "aaaaaaaa-1111,bbbbbbbb-2222" {
		t.Errorf("tower targets %s, want the controllable ones", got)
	}
}

func TestSendUsage(t *testing.T) {
	f := sendFixture()
	e, _, _ := actionsEnv(f)
	ctx := context.Background()
	for name, args := range map[string][]string{
		"no text":    {"aaaaaaaa"},
		"no target":  {"-m", "hi"},
		"both":       {"aaaaaaaa", "--machine", "tower", "-m", "hi"},
		"empty text": {"aaaaaaaa", "-m", "  "},
		"dangling":   {"aaaaaaaa", "-m"},
	} {
		if err := e.send(ctx, args); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%s: %v, want usage", name, err)
		}
	}
	if err := e.send(ctx, []string{"aaaa", "-m", "hi"}); err == nil || !strings.Contains(err.Error(), "matches 2 sessions") {
		t.Errorf("ambiguous prefix: %v", err)
	}
	if err := e.send(ctx, []string{"--machine", "nope", "-m", "hi"}); err == nil || !strings.Contains(err.Error(), "no live session") {
		t.Errorf("empty machine: %v", err)
	}
	if len(f.sent) != 0 {
		t.Errorf("a refused command sent %d messages", len(f.sent))
	}
}

func permissionInbox() api.Inbox {
	p := &api.PermissionRequest{ID: "pr_AAAAAAAAAAAAAAAAAAAAAA", ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"git push"}`),
		State: api.PermissionOpen}
	in := inboxFixture()
	in.Items[0].Permission = p
	return in
}

func TestApproveAndDeny(t *testing.T) {
	f := &fakeActions{inbox: permissionInbox()}
	e, out, _ := actionsEnv(f)
	ctx := context.Background()
	if err := e.approve(ctx, []string{"aaaa"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "allowed once: Bash git push (aaaaaaaa  fix login)\n" {
		t.Errorf("approve output %q", out.String())
	}
	out.Reset()
	if err := e.deny(ctx, []string{"pr_AAAAAAAAAAAAAAAAAAAAAA", "use make clean"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "denied: pr_AAAAAAAAAAAAAAAAAAAAAA\n" {
		t.Errorf("deny by request ID %q", out.String())
	}
	want := []string{"pr_AAAAAAAAAAAAAAAAAAAAAA allow", "pr_AAAAAAAAAAAAAAAAAAAAAA deny use make clean"}
	if strings.Join(f.decided, "|") != strings.Join(want, "|") {
		t.Errorf("decisions %q", f.decided)
	}
	if err := e.approve(ctx, []string{"bbbb"}); err == nil || !strings.Contains(err.Error(), "no open permission request") {
		t.Errorf("waiting item: %v", err)
	}
	for _, bad := range [][]string{{}, {"a", "b"}} {
		if err := e.approve(ctx, bad); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("approve %q: %v", bad, err)
		}
	}
	if err := e.deny(ctx, []string{"aaaa", "a", "b"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("deny with two reasons: %v", err)
	}
	f.decideErr = &client.StatusError{Status: 409, Message: "conflict: request pr_x is already decided"}
	if err := e.approve(ctx, []string{"aaaa"}); err == nil || !strings.Contains(err.Error(), "already decided") {
		t.Errorf("second decision: %v", err)
	}
}

func TestInboxLineShowsRequest(t *testing.T) {
	in := permissionInbox()
	line := inboxLine(in.Items[0], now, 200)
	if !strings.Contains(line, "asks to use Bash: git push") || strings.Contains(line, "Asked which token") {
		t.Errorf("line %q", line)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/cli/ -count=1`
Expected: FAIL to compile with `unknown field actions in struct literal of
type env` and `e.rules undefined`.

- [ ] **Step 3: Add the env fields**

In `internal/cli/cli.go`, in the `env` struct, after:

```go
	inbox inboxAPI
```

add:

```go
	actions actionsAPI
	// pause waits between delivery reads in sessionhub send; tests advance a fake
	// clock instead.
	pause func(time.Duration)
```

In `defaultEnv`, replace:

```go
	return &env{cfg: cfg, api: c, login: c, inbox: c, width: termWidth, out: os.Stdout, now: time.Now,
```

with:

```go
	return &env{cfg: cfg, api: c, login: c, inbox: c, actions: c, pause: time.Sleep, width: termWidth, out: os.Stdout, now: time.Now,
```

- [ ] **Step 4: Write the commands**

Create `internal/cli/actions.go`:

```go
package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

// actionsAPI is the part of *client.Client that sessionhub rules, sessionhub send, sessionhub
// approve, and sessionhub deny use.
type actionsAPI interface {
	Instructions(ctx context.Context) (api.InstructionList, error)
	AddInstruction(ctx context.Context, text string) (api.Instruction, error)
	DeleteInstruction(ctx context.Context, id int64) error
	SendMessages(ctx context.Context, in api.MessagesIn) (api.MessagesOut, error)
	GetMessage(ctx context.Context, id string) (api.Message, error)
	DecidePermission(ctx context.Context, id string, in api.DecisionIn) (api.PermissionRequest, error)
}

var _ actionsAPI = (*client.Client)(nil)

const (
	// sendWait is how long sessionhub send waits for delivery results.
	sendWait = 15 * time.Second
	// sendPoll is how often it reads them.
	sendPoll = time.Second
)

const rulesUsage = `usage:
  sessionhub rules [ls]
  sessionhub rules add "<text>"
  sessionhub rules rm <id>`

const sendUsage = `usage:
  sessionhub send <id-or-prefix>... -m "<text>"
  sessionhub send --machine <machine> -m "<text>"`

const approveUsage = `usage: sessionhub approve <request-id|session-prefix>`

const denyUsage = `usage: sessionhub deny <request-id|session-prefix> ["reason"]`

func runWith(ctx context.Context, args []string, fn func(*env, context.Context, []string) error) error {
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	return fn(e, ctx, args)
}

// RunRules implements `sessionhub rules`.
func RunRules(ctx context.Context, args []string) error { return runWith(ctx, args, (*env).rules) }

// RunSend implements `sessionhub send`.
func RunSend(ctx context.Context, args []string) error { return runWith(ctx, args, (*env).send) }

// RunApprove implements `sessionhub approve`.
func RunApprove(ctx context.Context, args []string) error { return runWith(ctx, args, (*env).approve) }

// RunDeny implements `sessionhub deny`.
func RunDeny(ctx context.Context, args []string) error { return runWith(ctx, args, (*env).deny) }

func (e *env) rules(ctx context.Context, args []string) error {
	switch {
	case len(args) == 0 || (len(args) == 1 && args[0] == "ls"):
		list, err := e.actions.Instructions(ctx)
		if err != nil {
			return fmt.Errorf("rules: %w", err)
		}
		if len(list.Instructions) == 0 {
			fmt.Fprintln(e.out, "no standing rules")
			return nil
		}
		for _, r := range list.Instructions {
			fmt.Fprintf(e.out, "%d  %s  (%s)\n", r.ID, clean(r.Text, 0), clean(r.CreatedBy, 0))
		}
		return nil
	case len(args) == 2 && args[0] == "add":
		r, err := e.actions.AddInstruction(ctx, args[1])
		if err != nil {
			return fmt.Errorf("rules add: %w", err)
		}
		fmt.Fprintf(e.out, "added rule %d\n", r.ID)
		return nil
	case len(args) == 2 && args[0] == "rm":
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("rules rm: %q is not a rule number\n%s", clean(args[1], 0), rulesUsage)
		}
		if err := e.actions.DeleteInstruction(ctx, id); err != nil {
			return fmt.Errorf("rules rm: %w", err)
		}
		fmt.Fprintf(e.out, "removed rule %d\n", id)
		return nil
	}
	return fmt.Errorf("rules: unexpected arguments\n%s", rulesUsage)
}

// sendArgs splits sessionhub send's arguments. Flags may come before or after the
// targets, which the flag package does not allow.
func sendArgs(args []string) (targets []string, machine, text string, err error) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-m", "--message", "--machine":
			if i+1 >= len(args) {
				return nil, "", "", fmt.Errorf("send: %s needs a value\n%s", a, sendUsage)
			}
			i++
			if a == "--machine" {
				machine = args[i]
			} else {
				text = args[i]
			}
		default:
			targets = append(targets, a)
		}
	}
	switch {
	case strings.TrimSpace(text) == "":
		return nil, "", "", fmt.Errorf("send: -m needs the message text\n%s", sendUsage)
	case len(targets) == 0 && machine == "":
		return nil, "", "", fmt.Errorf("send: name the sessions or --machine\n%s", sendUsage)
	case len(targets) > 0 && machine != "":
		return nil, "", "", fmt.Errorf("send: use session IDs or --machine, not both\n%s", sendUsage)
	}
	return targets, machine, text, nil
}

func (e *env) send(ctx context.Context, args []string) error {
	targets, machine, text, err := sendArgs(args)
	if err != nil {
		return err
	}
	byID := map[string]api.Session{}
	var ids []string
	if machine != "" {
		list, err := e.api.ListSessions(ctx, true, machine)
		if err != nil {
			return fmt.Errorf("send: %w", err)
		}
		for _, s := range list {
			if s.Controllable && s.Status != api.StatusEnded {
				byID[s.ID] = s
				ids = append(ids, s.ID)
			}
		}
		if len(ids) == 0 {
			return fmt.Errorf("send: no live session in herdr on %s", clean(machine, 0))
		}
	}
	for _, t := range targets {
		d, err := e.api.GetSession(ctx, t)
		if err != nil {
			return fmt.Errorf("send: %s: %w", clean(t, 0), err)
		}
		if _, dup := byID[d.ID]; !dup {
			byID[d.ID] = d.Session
			ids = append(ids, d.ID)
		}
	}
	out, err := e.actions.SendMessages(ctx, api.MessagesIn{SessionIDs: ids, Text: text})
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	refused := false
	var queued []api.MessageResult
	for _, r := range out.Results {
		line := shortID(r.SessionID) + "  " + clean(title(byID[r.SessionID]), 0) + "  "
		if r.State == api.MessageQueued {
			line += "queued"
			queued = append(queued, r)
		} else {
			line += "refused: " + clean(r.Detail, 0)
			refused = true
		}
		fmt.Fprintln(e.out, line)
	}
	if e.waitDelivery(ctx, queued) {
		refused = true
	}
	if refused {
		return &ExitError{Code: 1}
	}
	return nil
}

// waitDelivery reads each queued message until it leaves queued or sendWait
// passes, then prints one line per message. It reports whether any ended
// refused or expired.
func (e *env) waitDelivery(ctx context.Context, queued []api.MessageResult) bool {
	if len(queued) == 0 {
		return false
	}
	final := map[string]api.Message{}
	deadline := e.now().Add(sendWait)
	for {
		for _, r := range queued {
			if m, ok := final[r.ID]; ok && m.State != api.MessageQueued {
				continue
			}
			m, err := e.actions.GetMessage(ctx, r.ID)
			if err != nil {
				m = api.Message{State: api.MessageQueued, Detail: "could not read the delivery state"}
			}
			final[r.ID] = m
		}
		done := true
		for _, m := range final {
			if m.State == api.MessageQueued {
				done = false
			}
		}
		if done || !e.now().Before(deadline) || ctx.Err() != nil {
			break
		}
		e.pause(sendPoll)
	}
	bad := false
	for _, r := range queued {
		m := final[r.ID]
		line := shortID(r.SessionID) + "  "
		switch m.State {
		case api.MessageDelivered:
			line += "delivered"
		case api.MessageQueued:
			why := clean(m.Detail, 0)
			if why == "" {
				why = "waiting for the agent to be idle"
			}
			line += "not delivered yet: " + why + "; sessionhub keeps trying for 10 minutes"
		default:
			line += clean(m.State, 0)
			if d := clean(m.Detail, 0); d != "" {
				line += ": " + d
			}
			bad = true
		}
		fmt.Fprintln(e.out, line)
	}
	return bad
}

func (e *env) approve(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("approve: unexpected arguments\n%s", approveUsage)
	}
	return e.decide(ctx, args[0], api.DecisionIn{Decision: api.DecisionAllow})
}

func (e *env) deny(ctx context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("deny: unexpected arguments\n%s", denyUsage)
	}
	in := api.DecisionIn{Decision: api.DecisionDeny}
	if len(args) == 2 {
		in.Reason = args[1]
	}
	return e.decide(ctx, args[0], in)
}

// decide resolves target, a request ID (pr_...) or a session prefix whose
// inbox item has an open request, and sends the decision.
func (e *env) decide(ctx context.Context, target string, in api.DecisionIn) error {
	verb := map[string]string{api.DecisionAllow: "allowed once", api.DecisionDeny: "denied"}[in.Decision]
	if strings.HasPrefix(target, "pr_") {
		if _, err := e.actions.DecidePermission(ctx, target, in); err != nil {
			return fmt.Errorf("%s: %w", in.Decision, err)
		}
		fmt.Fprintf(e.out, "%s: %s\n", verb, clean(target, 0))
		return nil
	}
	inbox, err := e.inbox.Inbox(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", in.Decision, err)
	}
	it, err := findInboxItem(inbox.Items, target)
	if err != nil {
		return fmt.Errorf("%s: %w", in.Decision, err)
	}
	p := it.Permission
	if p == nil {
		return fmt.Errorf("%s: %s has no open permission request; see `sessionhub inbox`", in.Decision, shortID(it.Session.ID))
	}
	if _, err := e.actions.DecidePermission(ctx, p.ID, in); err != nil {
		return fmt.Errorf("%s: %w", in.Decision, err)
	}
	fmt.Fprintf(e.out, "%s: %s %s (%s  %s)\n", verb, clean(p.ToolName, 0), clean(api.PermissionInputText(p.ToolInput), 120),
		shortID(it.Session.ID), clean(title(it.Session), 0))
	return nil
}
```

`fakeActions` implements `hubAPI` (`ListSessions`, `GetSession`,
`Health`) too, so the tests set it as `api`, `inbox`, and `actions`.

- [ ] **Step 5: Show the request in `sessionhub inbox`**

In `internal/cli/inbox.go` (`inboxLine`), replace:

```go
	detail := s.Recap
	if len(it.WaitingOn) > 0 {
		detail = strings.Join(it.WaitingOn, "; ")
	}
```

with:

```go
	detail := s.Recap
	if len(it.WaitingOn) > 0 {
		detail = strings.Join(it.WaitingOn, "; ")
	}
	if p := it.Permission; p != nil {
		detail = "asks to use " + p.ToolName + ": " + api.PermissionInputText(p.ToolInput)
	}
```

- [ ] **Step 6: Route the commands**

In `cmd/sessionhub/main.go`, in `usage`, after the line:

```
  inbox dismiss|snooze <id>     dismiss or snooze an inbox item
```

add:

```
  rules [ls|add <text>|rm <id>] list or edit the standing rules
  send <id>... -m <text>        send a message to sessions (or --machine M)
  approve <id>                  allow a pending permission prompt once
  deny <id> [reason]            deny a pending permission prompt
```

and in `routes`, after `"inbox":              cli.RunInbox,` add:

```go
	"rules":              cli.RunRules,
	"send":               cli.RunSend,
	"approve":            cli.RunApprove,
	"deny":               cli.RunDeny,
```

In `cmd/sessionhub/main_test.go`, replace:

```go
	"uninstall-hooks", "mcp", "install-mcp", "uninstall-mcp", "ls", "show", "status", "login", "inbox", "resume", "remote-control", "join"}
```

with:

```go
	"uninstall-hooks", "mcp", "install-mcp", "uninstall-mcp", "ls", "show", "status", "login", "inbox", "rules", "send",
	"approve", "deny", "resume", "remote-control", "join"}
```

so `TestRoutesCoverEveryCommand` and `TestUsageListsEveryCommand` cover
them.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/cli/ ./cmd/sessionhub/ -count=1`
Expected: PASS, including every existing inbox test (the fixture's items
carry no request, so their lines are unchanged).

- [ ] **Step 8: Document it**

In `docs/cli.md`, after the `sessionhub inbox snooze` row of the commands table,
add:

```markdown
| `sessionhub rules [ls]` | The standing rules every session sees, one per line: number, text, and who added it. See below. |
| `sessionhub rules add "<text>"` | Add a rule, 1 to 300 characters on one line. The list holds at most 2,000 characters. |
| `sessionhub rules rm <id>` | Remove rule `<id>`. |
| `sessionhub send <id-or-prefix>... -m "<text>"` | Send a message to sessions. See below. |
| `sessionhub send --machine <M> -m "<text>"` | Send a message to every live session in herdr on `<M>`. |
| `sessionhub approve <request-id\|prefix>` | Allow a pending permission prompt once. See below. |
| `sessionhub deny <request-id\|prefix> ["reason"]` | Deny a pending permission prompt, with an optional reason Claude sees. |
```

In the `sessionhub inbox [--json]` row, replace "and what it waits on or its
recap" with "and what it waits on (for a blocked session with a permission
request, `asks to use <tool>: <command or file>`) or its recap".

After the `sessionhub inbox --watch` section (before "## `sessionhub digest`"), add:

```markdown
## `sessionhub rules`

The rules are short standing instructions that every Claude session on
every machine sees at its start and with every prompt. `sessionhub rules add`
saves one on the server; each machine's herdr watcher copies the list
within a minute, and sessions see it from their next prompt. A rule is one
line of at most 300 characters, and the whole list is at most 2,000; an add
past that fails and asks you to remove a rule first. The dashboard's
**Rules** tab and the MCP tools `remember`, `forget`, and `instructions`
edit the same list.

## `sessionhub send`

`sessionhub send` types a message into each target session's terminal, prefixed
with `From the user via sessionhub (cli on <machine>):` and a blank line. Each
target is a session ID or a prefix of at least 4 characters; `--machine M`
targets every live session in herdr on `M` whose watcher is polling. Flags
may come before or after the targets.

It prints one line per target, `queued` or `refused: <reason>`, then waits
up to 15 seconds and prints the delivery result of each queued one:
`delivered`, `not delivered yet: <reason>; sessionhub keeps trying for 10 minutes`
(the agent is working or waiting on a prompt), `refused: <reason>`, or
`expired`. The watcher types a message only when the session's agent is
idle or done. The command exits 1 when any target was refused or expired.
At most 30 messages a minute leave one machine.

## `sessionhub approve` and `sessionhub deny`

When a session waits on a permission prompt, `sessionhub inbox` shows
`asks to use <tool>: <command or file>` on its **Blocked** line. `sessionhub
approve <prefix>` allows it once, and `sessionhub deny <prefix> ["reason"]` denies
it; Claude sees the reason. The target is the session ID or a prefix of at
least 4 characters, matched against the inbox, or the request ID (`pr_...`).
The answer reaches the session within about a second. If someone answered
already, the command fails with "already decided"; if the prompt was
answered in the terminal or expired, it fails with the server's reason. sessionhub
only ever allows once; it never adds a permanent rule.
```

- [ ] **Step 9: Run the full suite and lint**

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1 && make lint`
Expected: every package `ok`, lint clean.

- [ ] **Step 10: Commit**

```bash
git add internal/cli/actions.go internal/cli/actions_test.go internal/cli/cli.go internal/cli/inbox.go \
  cmd/sessionhub/main.go cmd/sessionhub/main_test.go docs/cli.md
git commit -m "$(cat <<'EOF'
Add sessionhub rules, sessionhub send, sessionhub approve, and sessionhub deny

sessionhub rules lists and edits the standing rules. sessionhub send resolves session
prefixes or a machine's live herdr sessions, sends one message, and
waits up to 15 seconds for delivery results. sessionhub approve and sessionhub deny
answer a pending permission prompt by session prefix or request ID, and
sessionhub inbox shows the tool and command on a blocked line.

Refs: docs/dev/superpowers/plans/2026-10-02-session-actions.md, Task 9

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 10: Telegram: the request in the Blocked message

**Files:**
- Modify: `internal/server/notify.go` (`alertMessage`)
- Modify: `internal/server/notify_test.go`
- Modify: `docs/server.md` (Telegram alerts)

**Interfaces:**
- Consumes: `api.InboxItem.Permission`, `api.PermissionInputText`
  (Tasks 1 and 3); `termtext.Clean`, `alertMessage`, `alertDetailRunes`.
- Produces: `const alertToolRunes = 40`; the Blocked message's request line.

- [ ] **Step 1: Write the failing test**

Add to `internal/server/notify_test.go`:

```go
func TestAlertMessageShowsPermission(t *testing.T) {
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	s := api.Session{ID: "0a1b2c3d-1111", Title: "deploy", Machine: "tower", Recap: "Ready to push."}
	it := api.InboxItem{Group: api.InboxBlocked, Since: at.Add(-time.Minute), Session: s,
		Permission: &api.PermissionRequest{ID: "pr_x", ToolName: "Bash",
			ToolInput: json.RawMessage(`{"command":"git push --force\u001b[31m","description":"Push"}`)}}
	text, buttons := alertMessage(it, at, "https://sessionhub.example.test")
	want := "⏸ Blocked: deploy\ntower · blocked 1m ago\nAsks to use Bash: git push --force[31m\nReady to push."
	if text != want {
		t.Errorf("text\n%q\nwant\n%q", text, want)
	}
	if len(buttons) != 1 || buttons[0].URL != "https://sessionhub.example.test/#inbox" {
		t.Errorf("buttons %+v, want Open inbox only", buttons)
	}
	// The input is cut to 300 characters; a long tool name to 40.
	it.Permission.ToolName = strings.Repeat("T", 50)
	it.Permission.ToolInput = json.RawMessage(`{"command":"` + strings.Repeat("x", 400) + `"}`)
	text, _ = alertMessage(it, at, "https://sessionhub.example.test")
	line := strings.Split(text, "\n")[2]
	if want := "Asks to use " + strings.Repeat("T", 39) + "…: " + strings.Repeat("x", 299) + "…"; line != want {
		t.Errorf("long request line\n%q\nwant\n%q", line, want)
	}
	// A Waiting item never carries a request line.
	w := api.InboxItem{Group: api.InboxWaiting, Since: at.Add(-time.Minute), WaitingOn: []string{"review"}, Session: s}
	if text, _ := alertMessage(w, at, "https://sessionhub.example.test"); strings.Contains(text, "Asks to use") {
		t.Errorf("waiting text %q", text)
	}
}
```

Add `"encoding/json"` to the imports of `notify_test.go` if it is not there.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/server/ -run TestAlertMessageShowsPermission -count=1`
Expected: FAIL: the text has no `Asks to use` line.

- [ ] **Step 3: Add the line**

In `internal/server/notify.go`, after the `alertDetailRunes` constant, add:

```go
	// alertToolRunes caps the tool name in the request line.
	alertToolRunes = 40
```

In `alertMessage`, replace:

```go
	if d := termtext.Clean(alertDetail(it), alertDetailRunes); d != "" {
		lines = append(lines, d)
	}
```

with:

```go
	if p := it.Permission; p != nil && it.Group == api.InboxBlocked {
		lines = append(lines, "Asks to use "+termtext.Clean(p.ToolName, alertToolRunes)+": "+
			termtext.Clean(api.PermissionInputText(p.ToolInput), alertDetailRunes))
	}
	if d := termtext.Clean(alertDetail(it), alertDetailRunes); d != "" {
		lines = append(lines, d)
	}
```

- [ ] **Step 4: Run the server tests to verify they pass**

Run: `go test ./internal/server/ -count=1`
Expected: PASS, including every existing alert test (their items carry no
request, so their text is unchanged).

- [ ] **Step 5: Document it**

In `docs/server.md`, section "Telegram alerts", in the code block of the
**Blocked** message, replace the line:

~~~
<what it waits on, or the recap, cut to 300 characters>
~~~

with these two lines:

~~~
Asks to use <tool>: <command or file, cut to 300 characters>
<what it waits on, or the recap, cut to 300 characters>
~~~

Then replace the paragraph after that block:

~~~markdown
For a **Blocked** item, the third line is the latest report's `waiting_on`
items when you haven't sent a prompt since that report, else the recap; it is
left out when both are empty.
~~~

with:

~~~markdown
For a **Blocked** item, the `Asks to use` line is there when the session has
an open permission request (see [Permission requests](#permission-requests)).
The last line is the latest report's `waiting_on` items when you haven't sent
a prompt since that report, else the recap; it is left out when both are
empty. Answer the request with **Allow once** or **Deny** in the inbox that
**Open inbox** opens; the bot itself never answers.
~~~

- [ ] **Step 6: Run the full suite and lint**

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1 && make lint`
Expected: every package `ok`, lint clean.

- [ ] **Step 7: Commit**

```bash
git add internal/server/notify.go internal/server/notify_test.go docs/server.md
git commit -m "$(cat <<'EOF'
Show the pending permission request in the Telegram Blocked alert

A Blocked alert for a session with an open permission request names the
tool and the first 300 characters of its command or file. The Open
inbox button already leads to the inbox, where Allow once and Deny
answer it; the bot keeps using link buttons only.

Refs: docs/dev/superpowers/plans/2026-10-02-session-actions.md, Task 10

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 11: Dashboard: Rules tab, send bar, and permission block

**Files:**
- Modify: `internal/server/dashboard/index.html` (markup, CSS, the
  `actions-state` block, `hashTab`, the UI wiring)
- Modify: `internal/server/dashboard_test.go` (`TestDashboardWrites`
  counts, new `TestDashboardActionStates`)
- Modify: `docs/server.md` (Dashboard)

**Interfaces:**
- Consumes: the routes (Task 4) and `InboxItem.permission` (Task 3); page
  helpers `el`, `keyed`, `focusKeyed`, `toggleOne`, `rcRefusal`,
  `renderInbox`, `loadInbox`, `render`, `showSignedOut`, `syncAllButton`;
  test helper `runBlocks`.
- Produces, in the node-tested `actions-state` block: `RULES_MAX`,
  `RULE_MAX`, `ruleChars`, `rulesView`, `ruleProblem`, `addRuleRequest`,
  `deleteRuleRequest`, `canMessage`, `sendTargets`, `sendProblem`,
  `sendRequest`, `sendResultLines`, `permissionView`, `decideRequest`,
  `decisionOutcome`. In the page: `loadRules`, `renderRules`, `addRule`,
  `removeRule`, `rulesWrite`, `pickBox`, `renderSendBar`, `sendMessage`,
  `permissionBlock`, `decide`; `hashTab("#rules")` is `"rules"`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/server/dashboard_test.go`:

```go
// TestDashboardActionStates runs the page's actions-state block (and
// inbox-state, for hashTab) under node: the Rules tab, the send bar, and
// the permission block.
func TestDashboardActionStates(t *testing.T) {
	script := `
var many = [];
for (var i = 0; i < 21; i++) many.push({ ok: true });
var byId = {
  a: { id: "a", title: "A", machine: "tower", controllable: true, status: "live" },
  b: { id: "b", title: "", machine: "bluebox", controllable: false, status: "live" }
};
var targets = sendTargets({ b: true, a: true, c: true, z: false }, byId);
process.stdout.write(JSON.stringify({
  hash: hashTab("#rules"),
  view: rulesView({ instructions: [{ id: 1, text: "Never push." }, { id: 2, text: "ééé" }] }),
  emptyView: rulesView(null),
  problems: [ruleProblem("  ", 100), ruleProblem("a\nb", 100), ruleProblem("x".repeat(301), 2000),
    ruleProblem("abcd", 3), ruleProblem(" ok ", 2000)],
  add: addRuleRequest("  Be brief.  "),
  del: deleteRuleRequest(7),
  targets: targets,
  sendProblems: [sendProblem("hi", targets), sendProblem("  ", targets), sendProblem("hi", []),
    sendProblem("x".repeat(4001), targets), sendProblem("hi", many)],
  send: sendRequest(["a"], "hi\nthere"),
  lines: sendResultLines({ results: [{ session_id: "a", state: "queued", id: "msg_x" },
    { session_id: "zzzzzzzzzz", state: "refused", detail: "unknown session" }] }, byId),
  perm: [permissionView({ id: "pr_1", tool_name: "Bash", tool_input: { command: "git push", description: "x" } }),
    permissionView({ id: "pr_2", tool_name: "Write", tool_input: { file_path: "/a", content: "x" } }),
    permissionView({ id: "pr_3", tool_name: "Odd", tool_input: { a: 1 } }),
    permissionView({ id: "pr_4", tool_name: "Write", tool_input: "cut…" }),
    permissionView(null)],
  allow: decideRequest("pr_1/x", "allow", "ignored"),
  deny: decideRequest("pr_1", "deny", "use make"),
  outcomes: [decisionOutcome(200, "allow", null), decisionOutcome(200, "deny", null), decisionOutcome(409, "allow", null),
    decisionOutcome(410, "allow", null), decisionOutcome(500, "allow", { error: "boom" }), decisionOutcome(502, "allow", null)]
}));`
	out := runBlocks(t, []string{"inbox-state", "actions-state"}, script, map[string]any{})
	want := `{
  "hash": "rules",
  "view": {"rules": [{"id": 1, "text": "Never push."}, {"id": 2, "text": "ééé"}], "used": 14, "left": 1986},
  "emptyView": {"rules": [], "used": 0, "left": 2000},
  "problems": ["Write the rule first.", "Keep the rule on one line.", "A rule holds at most 300 characters; this one has 301.",
    "The rules hold at most 2,000 characters; remove one first.", ""],
  "add": {"url": "/v1/instructions", "init": {"method": "POST",
    "headers": {"Content-Type": "application/json", "X-Hub-Action": "instructions"},
    "body": "{\"text\":\"Be brief.\"}", "cache": "no-store"}},
  "del": {"url": "/v1/instructions/7", "init": {"method": "DELETE", "headers": {"X-Hub-Action": "instructions"}, "cache": "no-store"}},
  "targets": [
    {"id": "a", "title": "A", "machine": "tower", "ok": true, "why": ""},
    {"id": "b", "title": "b", "machine": "bluebox", "ok": false, "why": "not in herdr, or its machine's watcher is offline"},
    {"id": "c", "title": "c", "machine": "", "ok": false, "why": "no longer listed"}],
  "sendProblems": ["", "Write the message first.", "Select at least one session sessionhub can message.",
    "A message holds at most 4,000 characters.", "Send to at most 20 sessions at a time."],
  "send": {"url": "/v1/messages", "init": {"method": "POST",
    "headers": {"Content-Type": "application/json", "X-Hub-Action": "send"},
    "body": "{\"session_ids\":[\"a\"],\"text\":\"hi\\nthere\"}", "cache": "no-store"}},
  "lines": ["A: queued; sessionhub types it in when the session is idle.", "zzzzzzzz: not sent: unknown session"],
  "perm": [
    {"id": "pr_1", "tool": "Bash", "main": "git push", "code": true},
    {"id": "pr_2", "tool": "Write", "main": "/a", "code": false},
    {"id": "pr_3", "tool": "Odd", "main": "{\"a\":1}", "code": false},
    {"id": "pr_4", "tool": "Write", "main": "cut…", "code": false},
    null],
  "allow": {"url": "/v1/permissions/pr_1%2Fx/decide", "init": {"method": "POST",
    "headers": {"Content-Type": "application/json", "X-Hub-Action": "approve"},
    "body": "{\"decision\":\"allow\"}", "cache": "no-store"}},
  "deny": {"url": "/v1/permissions/pr_1/decide", "init": {"method": "POST",
    "headers": {"Content-Type": "application/json", "X-Hub-Action": "approve"},
    "body": "{\"decision\":\"deny\",\"reason\":\"use make\"}", "cache": "no-store"}},
  "outcomes": ["Allowed once.", "Denied.", "Someone already answered this request.",
    "Too late: the prompt was answered in the terminal or expired.", "Not sent: boom", "Not sent: the server returned 502."]
}`
	var got, exp map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %s: %v", out, err)
	}
	if err := json.Unmarshal([]byte(want), &exp); err != nil {
		t.Fatal(err)
	}
	for k, w := range exp {
		if !reflect.DeepEqual(got[k], w) {
			gb, _ := json.Marshal(got[k])
			wb, _ := json.Marshal(w)
			t.Errorf("%s:\n got %s\nwant %s", k, gb, wb)
		}
	}
}
```

In `TestDashboardWrites`, replace:

```go
		{"method:", 3},
		{`method: "POST"`, 3},
		{`"X-Hub-Action": "triage"`, 1},
```

with:

```go
		{"method:", 7},
		{`method: "POST"`, 6},
		{`method: "DELETE"`, 1},
		{`"X-Hub-Action": "triage"`, 1},
		{`"X-Hub-Action": "instructions"`, 2},
		{`"X-Hub-Action": "send"`, 1},
		{`"X-Hub-Action": "approve"`, 1},
		{"// actions-state:begin", 1},
		{"// actions-state:end", 1},
```

replace:

```go
		{"fetch(", 7}, // the list, the inbox, the request, the session poll, the Details read, sign-out, and triage
```

with:

```go
		{"fetch(", 11}, // the list, the inbox, the request, the session poll, the Details read, sign-out, triage,
		// the rules read, a rules write, the send, and a decision
```

and add these strings to the last `for _, want := range []string{...}`
list of the function (the one that starts with `"// layout-state:begin"`):

```go
		// Rules, selection, the send bar, and the permission block.
		`<button id="tab-rules" class="tab"`, `<main id="rules" hidden>`, `<section id="sendbar" class="sendbar" hidden`,
		`<button id="select" class="textbtn" type="button" aria-pressed="false" hidden>Select</button>`,
		"if (selectMode || open) art.appendChild(pickBox(s));", `var pv = it.group === "blocked" ? permissionView(it.permission) : null;`,
		"if (pv) c.appendChild(permissionBlock(pv, s));", `pv.code ? el("code", "perm-main", pv.main)`,
		`window.confirm("Send this message to "`, `window.prompt("Deny "`, `window.confirm("Delete this rule for every session?`,
		`rulesRoot.hidden = name !== "rules";`, `if (name === "rules") loadRules();`,
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/server/ -run 'TestDashboard' -count=1`
Expected: FAIL: `dashboard/index.html has no actions-state block`, and
`TestDashboardWrites` reports the old counts and the missing strings.

- [ ] **Step 3: Add the markup and the styles**

In `internal/server/dashboard/index.html`, replace:

```html
    <button id="tab-sessions" class="tab" type="button" role="tab" aria-selected="true" aria-controls="root">Sessions</button>
  </div>
  <input id="filter" type="search" placeholder="Filter sessions" aria-label="Filter sessions" autocomplete="off">
  <button id="all" class="textbtn" type="button" hidden>Collapse all</button>
</div>
<main id="inbox" hidden><p class="empty">Loading the inbox...</p></main>
<main id="root"><p class="empty">Loading sessions...</p></main>
</div>
```

with:

```html
    <button id="tab-sessions" class="tab" type="button" role="tab" aria-selected="true" aria-controls="root">Sessions</button>
    <button id="tab-rules" class="tab" type="button" role="tab" aria-selected="false" aria-controls="rules">Rules</button>
  </div>
  <input id="filter" type="search" placeholder="Filter sessions" aria-label="Filter sessions" autocomplete="off">
  <button id="all" class="textbtn" type="button" hidden>Collapse all</button>
  <button id="select" class="textbtn" type="button" aria-pressed="false" hidden>Select</button>
</div>
<main id="inbox" hidden><p class="empty">Loading the inbox...</p></main>
<main id="root"><p class="empty">Loading sessions...</p></main>
<main id="rules" hidden><p class="empty">Loading the rules...</p></main>
<section id="sendbar" class="sendbar" hidden aria-label="Send a message">
  <h2>Send a message</h2>
  <ul id="send-targets" class="send-targets"></ul>
  <textarea id="send-text" rows="3" maxlength="4000" aria-label="Message"></textarea>
  <div class="actions">
    <button id="send-go" type="button">Send</button>
    <button id="send-clear" class="textbtn" type="button">Clear selection</button>
  </div>
  <ul id="send-result" class="send-result" aria-live="polite"></ul>
</section>
</div>
```

Before the line `/* A mouse or trackpad: smaller targets and hover states. Touch keeps 44px. */`,
add:

```css
.pick { display: flex; align-items: center; gap: 8px; min-height: 44px; padding: 0 12px; font-size: .85rem; color: var(--muted); }
.pick input { width: 20px; height: 20px; }
.sendbar {
  position: sticky; bottom: 0; z-index: 5; margin: 8px 0 0; padding: 8px 12px 12px;
  background: var(--card); border: 1px solid var(--line); border-radius: 8px; box-shadow: var(--shadow);
}
.sendbar[hidden] { display: none; }
.sendbar h2 { font-size: 1rem; margin: 4px 0; }
.sendbar textarea { width: 100%; box-sizing: border-box; font: inherit; padding: 8px; }
.send-targets, .send-result, ul.rules { list-style: none; margin: 4px 0; padding: 0; font-size: .85rem; }
.send-targets .muted { color: var(--muted); }
.send-result .error { color: var(--blocked); }
ul.rules li { display: flex; flex-wrap: wrap; align-items: center; gap: 4px 8px; padding: 6px 0; border-bottom: 1px solid var(--line); }
.rule-text { flex: 1 1 60%; overflow-wrap: anywhere; }
.rule-by { color: var(--muted); font-size: .75rem; }
.rule-add { display: flex; flex-wrap: wrap; gap: 8px; margin: 12px 0; }
.rule-add input { flex: 1 1 16rem; min-height: 44px; padding: 0 8px; font: inherit; }
.perm { padding: 0 12px 4px; font-size: .9rem; }
.perm-tool { font-weight: 600; margin-right: 6px; }
.perm code.perm-main { display: block; margin-top: 4px; white-space: pre-wrap; overflow-wrap: anywhere; }
```

- [ ] **Step 4: Add the pure block and the `#rules` fragment**

In the `inbox-state` block, replace:

```js
    if (hash === "#sessions") return "sessions";
```

with:

```js
    if (hash === "#sessions") return "sessions";
    if (hash === "#rules") return "rules";
```

Right after the line `  // inbox-state:end`, add:

```js

  // actions-state:begin
  var RULES_MAX = 2000; // characters in all rules
  var RULE_MAX = 300;   // characters in one rule

  // ruleChars counts characters as the server does: code points.
  function ruleChars(text) { return Array.from(text).length; }

  // rulesView is the Rules tab: the rules, oldest first, and the characters
  // used and left.
  function rulesView(list) {
    var rules = ((list && list.instructions) || []).filter(function (r) { return r && typeof r.text === "string"; });
    var used = rules.reduce(function (n, r) { return n + ruleChars(r.text); }, 0);
    return { rules: rules, used: used, left: Math.max(0, RULES_MAX - used) };
  }

  // ruleProblem is why the add box's text cannot be added, or "".
  function ruleProblem(text, left) {
    var t = text.trim();
    if (!t) return "Write the rule first.";
    if (/[\u0000-\u001f\u007f-\u009f]/.test(t)) return "Keep the rule on one line.";
    var n = ruleChars(t);
    if (n > RULE_MAX) return "A rule holds at most 300 characters; this one has " + n + ".";
    if (n > left) return "The rules hold at most 2,000 characters; remove one first.";
    return "";
  }

  function addRuleRequest(text) {
    return {
      url: "/v1/instructions",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Hub-Action": "instructions" },
        body: JSON.stringify({ text: text.trim() }),
        cache: "no-store"
      }
    };
  }

  function deleteRuleRequest(id) {
    return {
      url: "/v1/instructions/" + encodeURIComponent(String(id)),
      init: { method: "DELETE", headers: { "X-Hub-Action": "instructions" }, cache: "no-store" }
    };
  }

  // canMessage: sessionhub can type a message into the session.
  function canMessage(s) { return !!s && s.controllable === true && s.status !== "ended"; }

  // sendTargets lists the selected sessions, sorted by ID, with whether
  // each can take a message and why not.
  function sendTargets(selected, byId) {
    return Object.keys(selected).filter(function (id) { return selected[id] === true; }).sort().map(function (id) {
      var s = byId[id];
      var why = "";
      if (!s) why = "no longer listed";
      else if (!canMessage(s)) why = "not in herdr, or its machine's watcher is offline";
      return { id: id, title: (s && s.title) || id.slice(0, 8), machine: s ? s.machine : "", ok: canMessage(s), why: why };
    });
  }

  // sendProblem is why the send bar cannot send, or "".
  function sendProblem(text, targets) {
    var ok = targets.filter(function (t) { return t.ok; }).length;
    if (!ok) return "Select at least one session sessionhub can message.";
    if (ok > 20) return "Send to at most 20 sessions at a time.";
    if (!text.trim()) return "Write the message first.";
    if (Array.from(text).length > 4000) return "A message holds at most 4,000 characters.";
    return "";
  }

  function sendRequest(ids, text) {
    return {
      url: "/v1/messages",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Hub-Action": "send" },
        body: JSON.stringify({ session_ids: ids, text: text }),
        cache: "no-store"
      }
    };
  }

  // sendResultLines is one line per target of a send response.
  function sendResultLines(out, byId) {
    return ((out && out.results) || []).map(function (r) {
      var s = byId[r.session_id];
      var name = (s && s.title) || String(r.session_id).slice(0, 8);
      if (r.state === "queued") return name + ": queued; sessionhub types it in when the session is idle.";
      return name + ": not sent: " + (r.detail || r.state);
    });
  }

  // PERM_FIELDS are the tool input fields that say what a tool will do,
  // in the order the server's PermissionInputText tries them.
  var PERM_FIELDS = ["command", "file_path", "notebook_path", "path", "url", "pattern", "query", "prompt"];

  // permissionView is what an inbox row shows of a request: the tool, its
  // main field, and whether that is a shell command (shown as code).
  function permissionView(p) {
    if (!p || typeof p.tool_name !== "string") return null;
    var input = p.tool_input;
    var main = "";
    if (input && typeof input === "object" && !Array.isArray(input)) {
      for (var i = 0; i < PERM_FIELDS.length && !main; i++) {
        var v = input[PERM_FIELDS[i]];
        if (typeof v === "string" && v) main = v;
      }
    }
    if (!main) main = typeof input === "string" ? input : JSON.stringify(input === undefined ? {} : input);
    return { id: p.id, tool: p.tool_name, main: main, code: p.tool_name === "Bash" };
  }

  // decideRequest is the Allow once or Deny write. Only a deny carries a
  // reason.
  function decideRequest(id, decision, reason) {
    var body = { decision: decision };
    if (decision === "deny" && reason) body.reason = reason;
    return {
      url: "/v1/permissions/" + encodeURIComponent(id) + "/decide",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Hub-Action": "approve" },
        body: JSON.stringify(body),
        cache: "no-store"
      }
    };
  }

  // decisionOutcome is the line a row shows after the server answers.
  function decisionOutcome(status, decision, body) {
    if (status === 200) return decision === "allow" ? "Allowed once." : "Denied.";
    if (status === 409) return "Someone already answered this request.";
    if (status === 410) return "Too late: the prompt was answered in the terminal or expired.";
    var msg = body && typeof body.error === "string" ? body.error : "";
    return "Not sent: " + (msg || "the server returned " + status + ".");
  }
  // actions-state:end
```

- [ ] **Step 5: Wire the page**

After:

```js
  var tabSessions = document.getElementById("tab-sessions");
```

add:

```js
  var tabRules = document.getElementById("tab-rules");
  var rulesRoot = document.getElementById("rules");
  var selectBtn = document.getElementById("select");
  var sendBar = document.getElementById("sendbar");
  var sendTargetsEl = document.getElementById("send-targets");
  var sendText = document.getElementById("send-text");
  var sendGo = document.getElementById("send-go");
  var sendClear = document.getElementById("send-clear");
  var sendResult = document.getElementById("send-result");
  var lastRules = { instructions: [], version: "" };
  var rulesError = "";  // why the last rules read or write failed
  var ruleDraft = "";   // the add box's text, kept across rebuilds
  var selectMode = false;
  var selected = {};    // session id -> true while selected for a message
  var permState = {};   // request id -> {busy} while a decision is in flight, then {done, text} or {text}
```

In `syncAllButton`, replace:

```js
    allBtn.hidden = tab === "inbox" || signedOut || !allNames.length;
```

with:

```js
    allBtn.hidden = tab === "inbox" || tab === "rules" || signedOut || !allNames.length;
    selectBtn.hidden = tab === "inbox" || tab === "rules" || signedOut;
```

In `card`, replace:

```js
    art.appendChild(head);
    cardEls[s.id] = art;
    if (!open) return art;
```

with:

```js
    art.appendChild(head);
    if (selectMode || open) art.appendChild(pickBox(s));
    cardEls[s.id] = art;
    if (!open) return art;
```

In `inboxRow`, replace:

```js
    c.appendChild(main);
    c.appendChild(triageActions(it, s, p, open, tap));
```

with:

```js
    c.appendChild(main);
    var pv = it.group === "blocked" ? permissionView(it.permission) : null;
    if (pv) c.appendChild(permissionBlock(pv, s));
    c.appendChild(triageActions(it, s, p, open, tap));
```

Replace the whole `showTab` function:

```js
  function showTab(name) {
    tab = name;
    var inbox = name === "inbox";
    inboxRoot.hidden = !inbox;
    root.hidden = inbox;
    syncAllButton(filterEl.value.trim());
    tabInbox.setAttribute("aria-selected", inbox ? "true" : "false");
    tabSessions.setAttribute("aria-selected", inbox ? "false" : "true");
  }
```

with:

```js
  function showTab(name) {
    tab = name;
    inboxRoot.hidden = name !== "inbox";
    root.hidden = name !== "sessions";
    rulesRoot.hidden = name !== "rules";
    syncAllButton(filterEl.value.trim());
    tabInbox.setAttribute("aria-selected", name === "inbox" ? "true" : "false");
    tabSessions.setAttribute("aria-selected", name === "sessions" ? "true" : "false");
    tabRules.setAttribute("aria-selected", name === "rules" ? "true" : "false");
    if (name === "rules") loadRules();
  }

  // loadRules reads the rules for the Rules tab.
  function loadRules() {
    if (signedOut) return;
    fetch("/v1/instructions", { headers: { "Accept": "application/json" }, cache: "no-store" })
      .then(function (resp) {
        if (signedOut) return null;
        if (resp.status === 401) { showSignedOut(); return null; }
        if (!resp.ok) throw new Error("The server returned " + resp.status + ".");
        return resp.json();
      })
      .then(function (data) {
        if (data === null || signedOut) return;
        lastRules = data;
        renderRules();
      })
      .catch(function (err) {
        if (signedOut) return;
        rulesError = err.message || "Could not load the rules.";
        renderRules();
      });
  }

  // renderRules draws the Rules tab: what the rules are for, each rule with
  // a Delete button, and the add box.
  function renderRules() {
    var view = rulesView(lastRules);
    var keep = focusedKey();
    var frag = document.createDocumentFragment();
    frag.appendChild(el("p", "meta", "Every session on every machine sees these rules at its start and with every prompt. " +
      view.left + " of 2,000 characters left."));
    if (!view.rules.length) frag.appendChild(el("p", "empty", "No standing rules yet."));
    var ul = el("ul", "rules");
    view.rules.forEach(function (r) {
      var li = el("li", null);
      li.appendChild(el("span", "rule-text", r.text));
      li.appendChild(el("span", "rule-by", r.created_by || ""));
      var del = keyed(el("button", "textbtn", "Delete"), "rule-del:" + r.id);
      del.type = "button";
      del.setAttribute("aria-label", "Delete rule: " + r.text);
      del.addEventListener("click", function () { removeRule(r); });
      li.appendChild(del);
      ul.appendChild(li);
    });
    if (view.rules.length) frag.appendChild(ul);
    var form = el("div", "rule-add");
    var box = keyed(el("input", null), "rule-text");
    box.type = "text";
    box.maxLength = RULE_MAX;
    box.placeholder = "Add a rule, for example: Write in British English.";
    box.setAttribute("aria-label", "New rule");
    box.value = ruleDraft;
    box.addEventListener("input", function () { ruleDraft = box.value; });
    box.addEventListener("keydown", function (e) { if (e.key === "Enter") addRule(); });
    var add = el("button", null, "Add rule");
    add.type = "button";
    add.addEventListener("click", function () { addRule(); });
    form.appendChild(box);
    form.appendChild(add);
    frag.appendChild(form);
    if (rulesError) frag.appendChild(el("p", "error", rulesError));
    rulesRoot.replaceChildren(frag);
    if (keep && focusedKey() !== keep) focusKeyed(keep);
  }

  function addRule() {
    var problem = ruleProblem(ruleDraft, rulesView(lastRules).left);
    if (problem) {
      rulesError = problem;
      renderRules();
      return;
    }
    rulesWrite(addRuleRequest(ruleDraft), function () { ruleDraft = ""; });
  }

  function removeRule(r) {
    if (!window.confirm("Delete this rule for every session?\n\n" + r.text)) return;
    rulesWrite(deleteRuleRequest(r.id), null);
  }

  // rulesWrite sends a rules add or delete and reads the list again.
  function rulesWrite(req, onOK) {
    rulesError = "";
    fetch(req.url, req.init)
      .then(function (resp) {
        if (resp.status === 401) { showSignedOut(); return; }
        if (resp.ok) {
          if (onOK) onOK();
          loadRules();
          return;
        }
        return resp.json().then(
          function (body) { rulesError = rcRefusal(resp.status, body); renderRules(); },
          function () { rulesError = rcRefusal(resp.status, null); renderRules(); });
      })
      .catch(function () { rulesError = "Could not reach the sessionhub."; renderRules(); });
  }

  // pickBox is a card's selection checkbox for the send bar.
  function pickBox(s) {
    var label = el("label", "pick");
    var box = keyed(el("input", null), "pick:" + s.id);
    box.type = "checkbox";
    box.checked = selected[s.id] === true;
    box.disabled = !canMessage(s) && selected[s.id] !== true;
    box.addEventListener("change", function () {
      selected = toggleOne(selected, s.id);
      renderSendBar();
    });
    label.appendChild(box);
    label.appendChild(el("span", null, canMessage(s) ? "Select for a message" : "Can't message: not in herdr, or its watcher is offline"));
    return label;
  }

  // renderSendBar shows the send bar while sessions are selected.
  function renderSendBar() {
    var targets = sendTargets(selected, sessionsById);
    sendBar.hidden = signedOut || targets.length === 0;
    sendTargetsEl.replaceChildren();
    targets.forEach(function (t) {
      sendTargetsEl.appendChild(el("li", t.ok ? null : "muted",
        t.title + (t.machine ? " · " + t.machine : "") + (t.ok ? "" : " (" + t.why + ")")));
    });
    var n = targets.filter(function (t) { return t.ok; }).length;
    sendGo.textContent = n === 1 ? "Send to 1 session" : "Send to " + n + " sessions";
  }

  // sendMessage sends the send bar's text to the selected sessions sessionhub can
  // message, after a confirmation, and lists each target's result.
  function sendMessage() {
    var targets = sendTargets(selected, sessionsById);
    var text = sendText.value;
    var problem = sendProblem(text, targets);
    if (problem) {
      sendResult.replaceChildren(el("li", "error", problem));
      return;
    }
    var ids = targets.filter(function (t) { return t.ok; }).map(function (t) { return t.id; });
    if (!window.confirm("Send this message to " + ids.length + (ids.length === 1 ? " session?" : " sessions?"))) return;
    sendGo.disabled = true;
    var req = sendRequest(ids, text);
    fetch(req.url, req.init)
      .then(function (resp) {
        if (resp.status === 401) { showSignedOut(); return null; }
        return resp.json().then(
          function (body) { return { resp: resp, body: body }; },
          function () { return { resp: resp, body: null }; });
      })
      .then(function (r) {
        sendGo.disabled = false;
        if (!r) return;
        if (!r.resp.ok) {
          sendResult.replaceChildren(el("li", "error", rcRefusal(r.resp.status, r.body)));
          return;
        }
        sendText.value = "";
        sendResult.replaceChildren();
        sendResultLines(r.body, sessionsById).forEach(function (line) { sendResult.appendChild(el("li", null, line)); });
      })
      .catch(function () {
        sendGo.disabled = false;
        sendResult.replaceChildren(el("li", "error", "Could not reach the sessionhub."));
      });
  }

  // permissionBlock is a Blocked row's request: the tool, its command (as
  // code) or main field, and Allow once and Deny.
  function permissionBlock(pv, s) {
    var box = el("div", "perm");
    box.appendChild(el("span", "perm-tool", "Asks to use " + pv.tool));
    box.appendChild(pv.code ? el("code", "perm-main", pv.main) : el("span", "perm-main", pv.main));
    var st = permState[pv.id];
    if (st && st.done) {
      box.appendChild(el("p", "meta", st.text));
      return box;
    }
    var name = s.title || s.id;
    var row = el("div", "actions");
    var allow = keyed(el("button", "textbtn act", "Allow once"), "allow:" + pv.id);
    allow.type = "button";
    allow.disabled = !!(st && st.busy);
    allow.setAttribute("aria-label", "Allow once: " + pv.tool + " in " + name);
    allow.addEventListener("click", function () { decide(pv, "allow", ""); });
    var deny = keyed(el("button", "textbtn", "Deny"), "deny:" + pv.id);
    deny.type = "button";
    deny.disabled = allow.disabled;
    deny.setAttribute("aria-label", "Deny: " + pv.tool + " in " + name);
    deny.addEventListener("click", function () {
      var reason = window.prompt("Deny " + pv.tool + "? Add a reason Claude will see (optional).", "");
      if (reason === null) return;
      decide(pv, "deny", reason.trim());
    });
    row.appendChild(deny);
    row.appendChild(allow);
    box.appendChild(row);
    if (st && st.text) box.appendChild(el("p", "inbox-error", st.text));
    return box;
  }

  // decide sends Allow once or Deny and shows the outcome on the row.
  function decide(pv, decision, reason) {
    permState[pv.id] = { busy: true };
    renderInbox();
    var req = decideRequest(pv.id, decision, reason);
    fetch(req.url, req.init)
      .then(function (resp) {
        if (resp.status === 401) { showSignedOut(); return null; }
        return resp.json().then(
          function (body) { return { status: resp.status, body: body }; },
          function () { return { status: resp.status, body: null }; });
      })
      .then(function (r) {
        if (!r) return;
        var settled = r.status === 200 || r.status === 409 || r.status === 410;
        permState[pv.id] = { done: settled, text: decisionOutcome(r.status, decision, r.body) };
        renderInbox();
        if (settled) loadInbox();
      })
      .catch(function () {
        permState[pv.id] = { text: "Not sent: could not reach the sessionhub." };
        renderInbox();
      });
  }
```

In `load`, replace:

```js
        render(data);
        renderInbox();
        updated.textContent = "Updated " + new Date().toLocaleTimeString();
```

with:

```js
        render(data);
        renderInbox();
        renderSendBar();
        if (tab === "rules") loadRules();
        updated.textContent = "Updated " + new Date().toLocaleTimeString();
```

In `showSignedOut`, replace:

```js
    inboxRoot.replaceChildren();
```

with:

```js
    inboxRoot.replaceChildren();
    rulesRoot.replaceChildren();
    selected = {};
    sendBar.hidden = true;
```

After:

```js
  tabSessions.addEventListener("click", function () { showTab("sessions"); });
```

add:

```js
  tabRules.addEventListener("click", function () { showTab("rules"); });
  selectBtn.addEventListener("click", function () {
    selectMode = !selectMode;
    selectBtn.setAttribute("aria-pressed", selectMode ? "true" : "false");
    selectBtn.textContent = selectMode ? "Done selecting" : "Select";
    render(lastSessions);
  });
  sendGo.addEventListener("click", sendMessage);
  sendClear.addEventListener("click", function () {
    selected = {};
    sendResult.replaceChildren();
    renderSendBar();
    render(lastSessions);
  });
```

`showSignedOut` calls `showTab("sessions")`, which hides the Rules tab and
the **Select** button through `syncAllButton`. The page still has one
inline script, uses `textContent` only, and loads nothing from outside.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestDashboard' -count=1`
Expected: PASS, including `TestDashboardHTMLAndCSP` (one script element,
no `innerHTML`, no external URL, CSP hashes that match). If a count in
`TestDashboardWrites` is off by one, recount the string in the page rather
than changing the test: each new write goes through its request builder,
and `fetch(` appears once per call site.

- [ ] **Step 7: Try it in a browser**

Run the server locally on a scratch database with one machine and one
session, sign a browser in with `sessionhub login`, and check: the **Rules** tab
adds and deletes a rule; `#rules` opens it; **Select** shows a checkbox on
each card and the send bar lists the selection; a Blocked inbox row with a
request (post one with `curl` and the machine token) shows **Allow once**
and **Deny**, and **Deny** asks for a reason. At 360 pixels wide the send
bar and the rule box fit without horizontal scrolling.

- [ ] **Step 8: Document it**

In `docs/server.md`, section "Dashboard", replace:

```markdown
The page has two tabs, **Inbox** and **Sessions**. **Inbox** shows
```

with:

```markdown
The page has three tabs, **Inbox**, **Sessions**, and **Rules**. **Inbox** shows
```

replace:

```markdown
The URL fragment `#inbox` opens the **Inbox** tab and `#sessions` the
**Sessions** tab, whatever the inbox holds; the Telegram alert's **Open
```

with:

```markdown
The URL fragment `#inbox` opens the **Inbox** tab, `#sessions` the
**Sessions** tab, and `#rules` the **Rules** tab, whatever the inbox holds; the Telegram alert's **Open
```

After the paragraph that ends "The filter applies to both tabs; the count
ignores it.", add:

```markdown
A **Blocked** row whose session has an open permission request shows the
tool and its input: a `Bash` command in code, another tool's file, path,
URL, or pattern as text. **Allow once** and **Deny** answer it with
`X-Hub-Action: approve`; **Deny** asks for an optional reason, which Claude
sees. The row then says **Allowed once.**, **Denied.**, that someone
already answered, or that it is too late (the prompt was answered in the
terminal or expired).

The **Rules** tab lists the standing rules (see
[Shared instructions](#shared-instructions)) with a **Delete** button each,
which asks for confirmation, and an add box that refuses a rule over 300
characters or past the 2,000-character total before it sends anything.
Writes send `X-Hub-Action: instructions`.

**Select**, beside the filter on the **Sessions** tab, shows a checkbox on
every card; an expanded card always shows one. A session sessionhub cannot message
(not in herdr, or its machine's watcher is offline) has its checkbox
disabled. While sessions are selected, the **Send a message** bar lists them
with a text box and **Send to N sessions**, which asks for confirmation and
sends with `X-Hub-Action: send`. Each target's result shows below:
queued, or not sent and why.
```

- [ ] **Step 9: Run the full suite and lint**

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1 && make lint`
Expected: every package `ok`, lint clean.

- [ ] **Step 10: Commit**

```bash
git add internal/server/dashboard/index.html internal/server/dashboard_test.go docs/server.md
git commit -m "$(cat <<'EOF'
Add the Rules tab, the send bar, and permission answers to the dashboard

The Rules tab lists, adds, and deletes standing rules. Select puts a
checkbox on each card, and the send bar sends one message to the
selected sessions after a confirmation. A Blocked inbox row with an open
permission request shows the tool and command with Allow once and Deny.
The request builders and views are in a node-tested block, and every
value goes in with textContent.

Refs: docs/dev/superpowers/plans/2026-10-02-session-actions.md, Task 11

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 12: README, SPEC, and PLAN

**Files:**
- Modify: `README.md` (Use it, Upgrade)
- Modify: `docs/dev/SPEC.md` (new section 9)
- Modify: `docs/dev/PLAN.md` (as-built notes)

**Interfaces:** none (documentation only).

- [ ] **Step 1: README: use**

In `README.md`, section "Use it", after the bullet that starts "In herdr, the
plugin's **sessionhub: inbox** action", add:

```markdown
- Standing rules reach every Claude session on every machine, at its start
  and with every prompt. Add one with `sessionhub rules add "<text>"`, on the
  dashboard's **Rules** tab, or by asking Claude to `remember` it for all
  sessions; `sessionhub rules` lists them and `sessionhub rules rm <id>` removes one.
  Keep each to one line; the list holds 2,000 characters.
- `sessionhub send <id-prefix>... -m "<text>"` types a message into sessions,
  prefixed with who sent it, once each one's agent is idle. On the
  dashboard, **Select** the sessions and use the **Send a message** bar.
  Only sessions in herdr on a machine whose watcher runs can take one.
- When a session waits on a permission prompt, the inbox row shows the tool
  and command with **Allow once** and **Deny**, and the Telegram alert names
  them; `sessionhub approve <id-prefix>` and `sessionhub deny <id-prefix> ["reason"]` do
  the same in a terminal. The prompt still shows in the terminal, and
  whichever answer comes first wins. sessionhub never adds an "always allow" rule.
  To keep a machine out of this, add `remote_permissions = false` to
  `~/.config/sessionhub/config.toml`.
```

- [ ] **Step 2: README: upgrade**

In `README.md`, before "### Upgrade to inbox alerts", add:

```markdown
### Upgrade to session actions

This release moves the database to schema version 8 and adds hook entries.
Deploy with `make deploy`, which backs up the database first and reinstalls
the plugin on `tower`; the first start upgrades the database. Then, on every
machine, `tower` included:

1. Run `make install` (not needed on `tower` after `make deploy`), then
   `sessionhub install-plugin`, so the new watcher delivers messages and copies the
   rules.
2. Run `sessionhub install-hooks`. It adds the `context` entry to `SessionStart`
   and `UserPromptSubmit` and a new `PermissionRequest` entry with a
   660-second timeout. Sessions that were running pick up the new hooks only
   after a restart.

To roll back, stop the server, restore the database backup as described
above, install the previous binary on `tower`, and start the server. The
version 7 binary refuses a version 8 database, so restoring the backup is the
only way back. On every machine, first run `sessionhub uninstall-hooks` with the
new binary (the older `install-hooks` on its own does not remove the
`PermissionRequest` and `context` entries), then install the previous
binary and run `sessionhub install-plugin` and `sessionhub install-hooks`.
```

- [ ] **Step 3: SPEC**

In `docs/dev/SPEC.md`, after section "### 8. Inbox" (before "### 6. Machine
enrollment"), add:

```markdown
### 9. Session actions
sessionhub acts on sessions, not only shows them. Shared instructions: one short list of standing rules (each at most 300 characters, 2,000 in all) that every session on every machine sees at its start and with every prompt, through a synchronous `sessionhub hook context` entry that reads a local copy and never waits on the network; `sessionhub rules`, the dashboard's **Rules** tab, and the MCP tools `remember`, `forget`, and `instructions` edit it. Messages: `sessionhub send`, the dashboard's send bar, and the MCP tool `send_to_sessions` send one message to up to 20 sessions in herdr; the session's watcher types it in with herdr's `agent.prompt` only when the agent is idle or done, and retries for 10 minutes. Remote permission answers: a `PermissionRequest` hook posts each permission prompt and waits up to 10 minutes; **Allow once** or **Deny** in the inbox, or `sessionhub approve` and `sessionhub deny`, answer it, while the terminal dialog stays and wins if answered first. Browser writes need `X-Hub-Action` `instructions`, `send`, or `approve`. sessionhub never sends a permanent permission rule, and Telegram only links to the inbox. The design is in `docs/dev/superpowers/specs/2026-10-02-session-actions-design.md`.
```

In the same file, in section "### 8. Inbox", replace the sentence
"Answering prompts from Telegram or the inbox pane is out of scope." with
"Answering a permission prompt from the dashboard's inbox or the CLI is
section 9; Telegram and the inbox pane only link to it."

- [ ] **Step 4: PLAN**

In `docs/dev/PLAN.md`, after the "## Inbox alerts, as built" section and before
"## Tests and captured payloads", add:

```markdown
## Session actions, as built

The design is in `docs/dev/superpowers/specs/2026-10-02-session-actions-design.md`.
The build settles these points the spec leaves open:

- Schema version 8 adds `instructions`, `messages`, and
  `permission_requests`. Rollback is restoring the database backup; the
  version 7 binary refuses a version 8 database.
- `sessionhub hook session-start` and `sessionhub hook prompt` stay async, and Claude Code
  does not add an async hook's output to the prompt. The rules come from a
  synchronous `sessionhub hook context` entry in the same matcher group, which
  reads `<state>/instructions.json` only. It takes the event from stdin's
  `hook_event_name` and prints nothing for a subagent.
- The watcher refreshes the rules copy after each accepted heartbeat, and
  `session-start` starts a detached `sessionhub hook refresh-instructions`. Both
  rewrite the copy only when `version` changed.
- Messages ride the control long poll as claims whose action is `message`,
  with the prompt in `text`. Results use `POST /v1/control/{id}/result` with
  `delivered`, `refused`, or `busy`; `failed` from an older watcher counts as
  `refused`. A queued message is offered again 15 seconds after the last
  offer, so a watcher that dies after a claim can, rarely, deliver twice.
- The watcher calls `agent.prompt` only after `pane.get` reports `idle` or
  `done` for the same session ID. herdr's `agent_blocked`, a `working`,
  `blocked`, or status-less agent, a pane with no session ID yet, or a silent
  socket is `busy`; no agent in the pane is `refused`.
- The rate limit counts one message per target, refused ones included: 30 a
  minute per sender (`dashboard`, `cli on <machine>`, or
  `session <id8>`).
- `GET /v1/messages/{id}` lets `sessionhub send` wait up to 15 seconds for results.
- Permission requests are open for 10 minutes. The hook cuts `tool_input`
  to 8 KiB (a JSON string of the first 8,000 bytes) before sending, so a
  big input never hits the 64 KiB body limit, and drops suggestions over
  4 KiB. It stops on the first server error, as the spec says.
- "Leaves blocked" closes open requests created at or before the state
  change: the event's own time on the event path, the server time minus
  5 seconds on the snapshot path. An ended session closes them as
  `closed`. A hooks-only session leaves `blocked` only at its next stop or
  prompt.
- The inbox shows the newest open request on a **Blocked** item only. The
  Telegram alert adds `Asks to use <tool>: <main field>` as its third line.
- The decision long poll rereads its request every second (for answers in
  the terminal and expiry) and wakes at once on a decision.
- Messages keep their newlines, and herdr submits a multi-line
  `agent.prompt` as one prompt (checked live; see
  `docs/dev/evidence/session-actions.md`).
```

If the live check (deploy notes, step 6) finds that herdr submits at the
first newline, replace the last bullet with what was done instead (the
watcher joins lines with ` / ` before `agent.prompt`) and change
`deliverMessage` and its test in the same commit.

- [ ] **Step 5: Check the docs build nothing and break nothing**

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1 && make lint`
Expected: every package `ok` (no code changed), lint clean.
Then run `grep -n "hook context\|permission-request\|remote_permissions" README.md docs/*.md docs/dev/PLAN.md`
Expected: matches in `README.md`, `docs/hooks.md`, `docs/client.md`, and
`docs/dev/PLAN.md`, and no stale "five hook entries".

- [ ] **Step 6: Commit**

```bash
git add README.md docs/dev/SPEC.md docs/dev/PLAN.md
git commit -m "$(cat <<'EOF'
Document standing rules, messages, and remote permission answers

README covers using the three features and the upgrade to schema 8,
including re-running sessionhub install-hooks on every machine. SPEC gains
section 9, and docs/dev/PLAN.md's as-built notes record what the build settled.

Refs: docs/dev/superpowers/plans/2026-10-02-session-actions.md, Task 12

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

## Self-review against the spec

| Spec item | Where |
|---|---|
| Schema v8 `instructions` with `id`, `text`, `created_at`, `created_by` | Task 1 |
| Rule 1 to 300 characters, list at most 2,000, `409` past it | Tasks 1, 4 |
| `GET /v1/instructions` with `version` hash, oldest first | Tasks 1, 4 |
| `POST` and `DELETE /v1/instructions`, machine or cookie + `instructions`, `404` unknown | Task 4 |
| Local copy `~/.local/state/sessionhub/instructions.json`; watcher refresh each heartbeat; session-start detached refresh | Tasks 5, 6, 7 |
| Session-start and prompt print the block; prompt path never calls the server; empty list adds nothing | Task 6 (`TestContextNeverCallsServer`, `TestContextPrintsNothing`) |
| MCP `remember`, `forget`, `instructions`; `remember` only on user request | Task 8 |
| `sessionhub rules ls/add/rm`; dashboard **Rules** tab | Tasks 9, 11 |
| `POST /v1/messages`, 1 to 20 sessions, 1 to 4,000 characters, cookie + `send` | Tasks 2, 4 |
| Per-target refusal for non-controllable targets; others still go | Task 2 (`TestSendMessagesTargets`) |
| Prefix `From the user via sessionhub (<sender>):` and a blank line; three sender forms | Tasks 1, 4 |
| 30 messages a minute per sender, `429` | Tasks 2, 4 |
| `messages` table and the four states | Tasks 1, 2 |
| Delivery through the control long poll as kind `message` | Tasks 2, 4, 7 |
| Deliver only when `idle` or `done`; `busy` retried; 10-minute expiry | Tasks 2, 7 |
| Watcher reports `delivered` or `refused` with herdr's reason through the result route | Tasks 4, 7 |
| `message` event on delivered or expired, sender and first 200 characters | Task 2 |
| Dashboard selection, **Send message** bar with confirmation, per-session results | Task 11 |
| `sessionhub send <ids> -m`, `sessionhub send --machine M`, waits up to 15 seconds | Task 9 |
| MCP `send_to_sessions` | Task 8 |
| `PermissionRequest` hook for all tools, `timeout: 660` | Task 6 |
| `POST /v1/sessions/{id}/permissions`, owning machine, input capped at 8 KB and marked | Tasks 3, 4, 6 |
| Hook long-polls `decision?wait=30` for up to 10 minutes; allow, deny with message, nothing on expiry, error, or no server | Tasks 4, 6 (`TestPermissionHookOutputs`) |
| Leaving `blocked` marks `answered_locally`; hook stops with no output | Tasks 3, 4, 6 |
| `POST /v1/permissions/{id}/decide`, cookie + `approve`, one decision, `409` second, `410` expired or closed | Tasks 3, 4 |
| Allow once and deny only; no `updatedPermissions` | Tasks 6, 11 |
| `permission` event with tool, first 200 characters, decision, reason, decider | Task 3 |
| `SESSIONHUB_REMOTE_PERMISSIONS=off` and `remote_permissions = false` opt-out | Tasks 5, 6 |
| Inbox Blocked item shows tool and input (`Bash` as code), **Allow once**, **Deny** with optional reason | Tasks 3, 11 |
| Telegram Blocked message includes tool and first 300 characters; **Open inbox** links to `/#inbox` | Task 10 |
| `sessionhub approve`, `sessionhub deny`, `sessionhub inbox` shows the request | Task 9 |
| Security: matching `X-Hub-Action`, random IDs, one decision, free-text cleaning, `textContent` | Tasks 2, 3, 4, 9, 10, 11 |
| Testing list (store, server, hooks, watcher, CLI, MCP, dashboard, live) | Tasks 1 to 11, deploy notes |
| Out of scope: always-allow rules, `AskUserQuestion`, sessions outside herdr, Telegram answer buttons | not built; Task 12 records it |

Gaps checked and closed: the async hook entries (resolved ambiguity 1), the
5-second hook deadline (the permission and context hooks branch before it),
hook stdout (new `stdout` field, one JSON object), the message text newline
rule (`checkMessageText`), the `cr_`-only result route (Task 4 accepts
`msg_`), the snapshot of the auth matrix (now dumps the three tables), and
the 64 KiB body limit for big tool inputs (`api.CapToolInput` in the hook).

Type and name consistency: `api.PermissionInputText(input)` takes only the
input everywhere; `store.SendMessages` returns `(results, machines, err)`
in Tasks 2 and 4; `client.WaitDecision` returns `*api.PermissionRequest`
(nil on `204`) in Tasks 5 and 6; `deliverMessage(h paneAgent, s, text)` in
Task 7 matches its test; `hookSpec` gains a sixth field, `Context`, and
every row of `hookTable` sets it.

## Deploy and live check (notes for the controller)

These steps run on the real machines. They are not a task: the controller
runs them with the user's approval, after Task 12. Use a scratch Claude
session for every prompt; never approve anything destructive.

1. **Full suite.** Run `go test $(go list ./... | grep -v internal/server) -count=1`,
   then `go test ./internal/server/ -count=1`, then `make lint`, and record
   the package count and `0` failures. If `internal/server` fails with
   `bind: address already in use`, wait 60 seconds and run it alone again.
2. **Deploy.** Order matters: the new binary and plugin go on both
   machines first, and only then the hooks. Run `make deploy` (backs up
   `sessionhub.db` on `tower`, swaps the binary, restarts the server, reinstalls the
   plugin), and on `bluebox` run `make install` and `sessionhub install-plugin`. Then
   run `sessionhub install-hooks` on `tower` and on `bluebox`.
3. **Confirm the upgrade.** On `tower`:
   `sqlite3 ~/.local/share/sessionhub/sessionhub.db 'PRAGMA user_version'` prints `8`.
   On each machine, `jq '.hooks.PermissionRequest, .hooks.UserPromptSubmit' ~/.claude/settings.json`
   shows `sessionhub hook permission-request` with `"timeout": 660` and the
   `sessionhub hook context` entry with `"timeout": 5`. On each machine (S13),
   `jq '.hooks | map_values([.[] | select(any(.hooks[]; .command | test("sessionhub hook ")))] | length)' ~/.claude/settings.json`
   shows `1` for every sessionhub event: no event carries a duplicate sessionhub entry.
4. **Rules.** `sessionhub rules add "End every reply with the word ROGER."` on
   `bluebox`. Within a minute, `cat ~/.local/state/sessionhub/instructions.json` on
   both machines shows it. Start a new scratch session on each machine and
   ask "What are your standing instructions from sessionhub?": it quotes the rule.
   In a long-running session, send any prompt: the reply ends with ROGER.
   Remove the rule with the dashboard's **Rules** tab; within a minute a new
   prompt no longer carries it. Time a prompt with the server stopped for
   ten seconds (`systemctl --user stop sessionhub` on `tower`, then start it): the
   prompt is not slower.
5. **Messages.** Start two scratch sessions in herdr, one on each machine,
   and leave them idle. `sessionhub send <prefix-a> <prefix-b> -m "Reply with the single word pong."`
   prints `queued` twice and, within 15 seconds, `delivered` twice; each
   pane shows the prefixed prompt and replies `pong`. Then give one session
   a long task (`sleep 45` through Bash) and send again: `sessionhub send` prints
   `not delivered yet: the agent is working`, and the message arrives after
   the task ends. `sqlite3 ~/.local/share/sessionhub/sessionhub.db "SELECT state, detail FROM messages ORDER BY created_at DESC LIMIT 3"`
   shows the states. Repeat one send from the dashboard's send bar on a
   phone.
6. **Multi-line message.** Send `-m $'Line one.\nLine two: reply with the word both.'`
   to an idle scratch session. Expected: one prompt with both lines, and the
   reply `both`. If herdr submits "Line one." alone, record it, change
   `deliverMessage` to join lines with ` / ` (with a test), update the last
   bullet of `docs/dev/PLAN.md`'s as-built notes, and commit that fix before
   continuing.
7. **Permission answers.** In a scratch session on `tower` in a scratch
   directory, ask Claude to run `touch sessionhub-approve-check` with Bash. The
   terminal shows the dialog. Within 45 seconds the dashboard's inbox shows
   the row with `Asks to use Bash` and the command in code, and Telegram's
   Blocked message names it. Tap **Allow once**: the row says **Allowed
   once.**, the file exists within a few seconds, and `sessionhub show <prefix>`
   lists a `permission` event. Ask for `touch sessionhub-deny-check`, run
   `sessionhub deny <prefix> "not in this directory"`: Claude reports the denial
   with that reason, and no file appears. Ask a third time and answer in
   the terminal: within a few seconds the inbox row's request is gone and
   `sqlite3 ~/.local/share/sessionhub/sessionhub.db "SELECT state FROM permission_requests ORDER BY created_at DESC LIMIT 1"`
   prints `answered_locally`. Repeat the allow on `bluebox` with `sessionhub approve`.
8. **Opt-out.** On `bluebox`, add `remote_permissions = false` to
   `~/.config/sessionhub/config.toml`, start a new scratch session, and ask for a
   Bash command: no request row appears in
   `permission_requests`, and the terminal dialog works. Remove the line.
9. **Evidence.** Write `docs/dev/evidence/session-actions.md` with the commands
   and outputs from steps 1 to 8 (no tokens), and commit it with the usual
   trailers, adding only that file. Delete the scratch files and sessions.

Rollback, if a step fails badly: on `tower`, stop the server, restore the
newest `sessionhub.db.bak-*` over `sessionhub.db`, delete `sessionhub.db-wal` and `sessionhub.db-shm`,
install the previous binary, and start the server; on `bluebox` and `tower`,
run `sessionhub uninstall-hooks` with the new binary first (the older
`install-hooks` on its own does not remove the `PermissionRequest` and
`context` entries), then install the previous
binary and run `sessionhub install-plugin` and `sessionhub install-hooks`. The version 7 binary refuses a version 8 database, so
restoring the backup is the only rollback.
