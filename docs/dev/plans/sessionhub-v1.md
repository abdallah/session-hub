# sessionhub v1 implementation plan

Spec: `docs/dev/SPEC.md` (binding). Design: `docs/dev/PLAN.md` (approved 2026-09-30). Facts:
`docs/dev/NOTES.md`. Definition of done: `docs/dev/DEFINITION-OF-DONE.md`.

## Global constraints

These bind every task. Copy them verbatim into every review.

- Go 1.25, module path `session-hub`, one binary `sessionhub` built from `cmd/sessionhub`.
  `CGO_ENABLED=0`. Dependencies: the standard library,
  `modernc.org/sqlite`, and `github.com/BurntSushi/toml`. No others.
- The installed binary lives at `~/.local/bin/sessionhub`. Manifests, hook entries,
  and the MCP registration use that absolute path.
- Paths (all overridable by env, see `internal/paths`):
  - client config `~/.config/sessionhub/config.toml` (mode 0600), env `SESSIONHUB_CONFIG`
  - server config `~/.config/sessionhub/server.toml` (mode 0600), env `SESSIONHUB_SERVER_CONFIG`
  - state dir `~/.local/state/sessionhub/` (queue, locks, `current/`), env `SESSIONHUB_STATE_DIR`
  - database `~/.local/share/sessionhub/sessionhub.db`, env `SESSIONHUB_DB`
  - plugin dir `~/.local/share/sessionhub/herdr-plugin/`
- Client config keys: `server_url`, `token`, `machine`. Env overrides:
  `SESSIONHUB_SERVER_URL`, `SESSIONHUB_TOKEN`, `SESSIONHUB_MACHINE`.
- Server config keys: `listen` (list, default `["127.0.0.1:8787"]`),
  `public_url` (default `https://sessionhub.example.com`), `read_token`, `stale_after`
  (Go duration, default `5m`). Env overrides: `SESSIONHUB_LISTEN` (comma-separated),
  `SESSIONHUB_PUBLIC_URL`, `SESSIONHUB_READ_TOKEN`, `SESSIONHUB_STALE_AFTER`, `SESSIONHUB_DB`.
- Client HTTP calls time out after **2 seconds**. Clients fail open: an
  unreachable server never produces a non-zero exit from a hook, event
  handler, or MCP tool call, and never blocks herdr or Claude Code.
- herdr: target **0.9.3** (`min_herdr_version = "0.9.3"`). Talk to herdr over
  its Unix socket (NDJSON: one JSON request per line, one response per line),
  path from `HERDR_SOCKET_PATH`, else `~/.config/herdr/herdr.sock`. The herdr
  session name is `default` for the default socket, or `<name>` for
  `~/.config/herdr/sessions/<name>/herdr.sock`.
- Session status values: `live`, `stale`, `ended`, `blocked`. Agent state
  values: `idle`, `working`, `blocked`, `done`, `unknown`. Event sources:
  `plugin`, `hooks`, `mcp`. Event kinds: `registered`, `state_changed`,
  `prompt`, `pane_closed`, `ended`.
- Tokens: machine tokens are `hub_m_` + 32 random bytes base64url (no
  padding); the dashboard read token is `hub_r_` + the same. The database
  stores only SHA-256 hex of machine tokens. Compare tokens in constant time.
- herdr sidebar: `pane.report_metadata` with `source: "sessionhub"` and token key
  `hub_summary` (value ≤ 80 chars).
- Machines in this deployment: `tower` (`ssh_host`/`herdr_host`
  `tower.example.com`) and `bluebox` (`bluebox.example.com`).
- Never modify herdr's managed files (`~/.claude/hooks/herdr-agent-state.sh`,
  herdr's own `SessionStart` entry in `~/.claude/settings.json`), and never
  edit `~/.config/herdr/config.toml`.
- Test payloads come from `testdata/`, captured from real herdr 0.9.3 and
  Claude Code runs (Task 2). Never hand-write a payload and present it as
  captured. Home paths are scrubbed to `/home/user`.
- Every task meets `docs/dev/DEFINITION-OF-DONE.md`, including an evidence file
  `docs/dev/evidence/task-<N>.md`.
- Commit messages: imperative subject, body with reason and gotchas, ending
  with `Refs: docs/dev/plans/sessionhub-v1.md Task <N>`.

## Shared API types (owned by Task 0, used by everyone)

`internal/api/types.go`, verbatim:

```go
package api

import (
	"encoding/json"
	"time"
)

const (
	StatusLive    = "live"
	StatusStale   = "stale"
	StatusEnded   = "ended"
	StatusBlocked = "blocked"

	SourcePlugin = "plugin"
	SourceHooks  = "hooks"
	SourceMCP    = "mcp"

	KindRegistered   = "registered"
	KindStateChanged = "state_changed"
	KindPrompt       = "prompt"
	KindPaneClosed   = "pane_closed"
	KindEnded        = "ended"
)

// SessionUpsert registers or refreshes one session. Empty fields never
// overwrite stored values.
type SessionUpsert struct {
	ID             string `json:"id"`     // Claude session UUID
	Agent          string `json:"agent"`  // "claude"
	Source         string `json:"source"` // plugin|hooks|mcp
	CWD            string `json:"cwd,omitempty"`
	GitRepo        string `json:"git_repo,omitempty"`
	GitBranch      string `json:"git_branch,omitempty"`
	HerdrSession   string `json:"herdr_session,omitempty"`
	HerdrWorkspace string `json:"herdr_workspace,omitempty"`
	HerdrPane      string `json:"herdr_pane,omitempty"`
	AgentState     string `json:"agent_state,omitempty"`
	TitleHint      string `json:"title_hint,omitempty"`   // herdr terminal_title_stripped
	FirstPrompt    string `json:"first_prompt,omitempty"` // truncated to 200 chars by the client
}

// HerdrSessionsPut is the full set of herdr agent panes on the calling machine.
type HerdrSessionsPut struct {
	HerdrSession string          `json:"herdr_session"`
	Sessions     []SessionUpsert `json:"sessions"`
}

type EventIn struct {
	Kind    string          `json:"kind"`
	Source  string          `json:"source"`
	TS      time.Time       `json:"ts"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type ReportIn struct {
	Done      []string `json:"done"`
	InFlight  []string `json:"in_flight"`
	WaitingOn []string `json:"waiting_on"`
	Note      string   `json:"note,omitempty"`
}

type TitleIn struct {
	Title string `json:"title"`
}

type Report struct {
	TS        time.Time `json:"ts"`
	Done      []string  `json:"done"`
	InFlight  []string  `json:"in_flight"`
	WaitingOn []string  `json:"waiting_on"`
	Note      string    `json:"note,omitempty"`
}

type Event struct {
	TS      time.Time       `json:"ts"`
	Source  string          `json:"source"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Session struct {
	ID             string     `json:"id"`
	Agent          string     `json:"agent"`
	Machine        string     `json:"machine"`
	CWD            string     `json:"cwd,omitempty"`
	GitRepo        string     `json:"git_repo,omitempty"`
	GitBranch      string     `json:"git_branch,omitempty"`
	HerdrSession   string     `json:"herdr_session,omitempty"`
	HerdrWorkspace string     `json:"herdr_workspace,omitempty"`
	HerdrPane      string     `json:"herdr_pane,omitempty"`
	Title          string     `json:"title,omitempty"`
	TitleSource    string     `json:"title_source,omitempty"` // user|herdr|prompt
	StartedAt      time.Time  `json:"started_at"`
	LastSeenAt     time.Time  `json:"last_seen_at"`
	Status         string     `json:"status"`
	AgentState     string     `json:"agent_state,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	ResumeCommand  string     `json:"resume_command"` // ssh -t <ssh_host> '~/.local/bin/sessionhub resume <id>'
	LatestReport   *Report    `json:"latest_report,omitempty"`
}

type SessionDetail struct {
	Session
	Reports []Report `json:"reports"` // newest first
	Events  []Event  `json:"events"`  // newest first, at most 50
}

type Machine struct {
	Name      string     `json:"name"`
	SSHHost   string     `json:"ssh_host"`
	HerdrHost string     `json:"herdr_host"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
}

type Error struct {
	Error string `json:"error"`
}
```

## Task graph

```
Task 0 scaffold
 ├─ Task 1 server ─────────────┬─ Task 7 dashboard
 ├─ Task 2 capture ─┐          │
 │                  └─ Task 3 client + herdr socket
 │                       ├─ Task 4 plugin ──┐
 │                       ├─ Task 5 hooks    ├─ Task 8 CLI + resume ─ Task 9 join ─ Task 10 deploy
 │                       └─ Task 6 MCP ─────┘
```

Tasks on the same row after their dependencies run in parallel, each in its
own git worktree, and are merged into `sessionhub-v1` by the controller.

### Task 0: Scaffold

Create the skeleton every other task builds in. No behavior yet.

- `go.mod` (module `session-hub`, go 1.25) with the two dependencies added
  (`go get modernc.org/sqlite github.com/BurntSushi/toml`), and `go.sum`.
- `internal/api/types.go`: the shared API types above, verbatim.
- `internal/paths/paths.go`: functions returning every path in Global
  constraints, applying the env overrides, with a table-driven test.
- `cmd/sessionhub/main.go`: subcommand dispatch. Every subcommand below routes to a
  function `Run(ctx context.Context, args []string) error` in its package.
  Packages that later tasks own get a stub file `stub.go` whose `Run` returns
  `errors.New("<command>: not implemented yet")`. Main prints the error to
  stderr and exits 1, except `hook` subcommands, which always exit 0.
  `sessionhub help` and no arguments print usage listing every command.

  | Command | Package |
  |---|---|
  | `server` | `internal/server` |
  | `machine add\|rm\|ls` | `internal/server` (`RunMachine`) |
  | `plugin startup\|event\|watch\|open-picker` | `internal/plugin` (`open-picker` → `internal/resume.RunOpenPicker`) |
  | `install-plugin`, `uninstall-plugin` | `internal/plugin` (`RunInstall`, `RunUninstall`) |
  | `hook <event>` | `internal/hooks` |
  | `install-hooks`, `uninstall-hooks` | `internal/hooks` (`RunInstall`, `RunUninstall`) |
  | `mcp` | `internal/mcp` |
  | `install-mcp`, `uninstall-mcp` | `internal/mcp` (`RunInstall`, `RunUninstall`) |
  | `ls`, `show`, `status` | `internal/cli` (`RunLs`, `RunShow`, `RunStatus`) |
  | `resume` | `internal/resume` |
  | `join` | `internal/join` |
  | `version` | main (prints `sessionhub dev` or the `-ldflags` version) |

- `Makefile` targets: `build` (static binary to `bin/sessionhub`, version from
  `git describe --always --dirty`), `test` (`go test ./...`), `lint`
  (`test -z "$(gofmt -l .)"` and `go vet ./...`), `install` (copy to
  `~/.local/bin/sessionhub`), `clean`. `deploy` is added by Task 10.
- `.gitignore`: `bin/`, `.superpowers/`.
- `docs/dev/IDEAS.md` with the four entries from `docs/dev/PLAN.md` "What goes in docs/dev/IDEAS.md".
- `docs/dev/evidence/task-0.md`: `make build`, `make test`, `make lint`,
  `./bin/sessionhub help`, `./bin/sessionhub ls` (shows the not-implemented error, exit 1),
  `./bin/sessionhub hook stop </dev/null; echo $?` (prints 0).

### Task 1: Server, store, and machine admin

Build `internal/store` and `internal/server` per `docs/dev/SPEC.md` components 1, 2,
and 6 (the `sessionhub machine` part), and `docs/dev/PLAN.md` "HTTP API" and "Data model
changes".

- **Store** (`internal/store`): SQLite via `modernc.org/sqlite`, WAL mode,
  `busy_timeout` 5000, foreign keys on, one `*sql.DB` with
  `SetMaxOpenConns(1)`. Schema created on open (versioned with
  `PRAGMA user_version`). Tables per `docs/dev/SPEC.md` data model plus: `sessions.agent`,
  `sessions.agent_state`, `sessions.title_source`, `sessions.first_prompt`,
  `machines.ssh_host`, `machines.herdr_host`, `events.source`. Times stored as
  UTC RFC 3339 with nanoseconds.
- **Upsert rules:** empty fields never overwrite; a session's `machine_id`
  is set on first insert and a write from another machine's token for an
  existing session returns 409. First insert records a `registered` event.
  Title precedence: `user` (from `/title`) beats `herdr` (`title_hint`) beats
  `prompt` (`first_prompt`); a lower-precedence source never replaces a
  higher one. Any upsert, event, report, or title call sets `last_seen_at` to
  now and clears `ended_at` unless the event kind is `pane_closed` or `ended`.
- **Events:** `state_changed` stores the payload's `agent_state` into
  `sessions.agent_state`. `pane_closed` and `ended` set `ended_at`. Heartbeats
  are never stored as event rows.
- **`PUT /v1/machines/self/herdr-sessions`:** upsert each entry with
  `source=plugin`, then set `ended_at` on every session of the calling machine
  that has a non-empty `herdr_pane`, has `herdr_session` equal to the body's
  `herdr_session`, no `ended_at`, and is not in the set. Record an `ended`
  event (source `plugin`, payload `{"reason":"missing_from_snapshot"}`) for each.
  Sessions with an empty `herdr_pane` are never touched.
- **Status** is computed at read time: `ended` if `ended_at` is set; else
  `stale` if `now - last_seen_at > stale_after`; else `blocked` if
  `agent_state = blocked`; else `live`. `?live=true` returns `live` and
  `blocked`. Inject the clock so tests control it.
- **`resume_command`** in every `Session`: `ssh -t <ssh_host> '~/.local/bin/sessionhub resume <id>'`
  using the owning machine's `ssh_host` (machine name if empty).
- **Endpoints and auth** exactly as the table in `docs/dev/PLAN.md` "HTTP API". JSON
  errors use `api.Error`. `{id}` in `GET /v1/sessions/{id}` accepts a unique
  prefix of at least 4 characters (400 if shorter, 404 if none, 409 with the
  candidate IDs if ambiguous); write endpoints need the full ID. Request bodies
  over 64 KiB are rejected with 413. Report lists are capped at 20 items of 200
  characters each (400 beyond). `GET /` in this task returns a plain-text
  placeholder with 200 (Task 7 replaces it); the `?token=` cookie exchange is
  Task 7's.
- **Machine admin** (`sessionhub machine add <name> [--ssh-host H] [--herdr-host H] [--json]`,
  `sessionhub machine rm <name>`, `sessionhub machine ls`): operate directly on the local
  database (`SESSIONHUB_DB`). `add` creates or rotates the token and prints it once;
  with `--json` it prints `{"name":..,"token":..,"server_url":<public_url>}`
  and nothing else on stdout. Defaults: `ssh_host` and `herdr_host` = name.
- **`sessionhub server`:** loads server config, opens the store, listens on every
  `listen` address, logs one line per request (method, path, status,
  duration, machine) to stderr, and shuts down cleanly on SIGINT/SIGTERM.
  Refuses to start without a `read_token`.
- **Tests:** `httptest` against a temp database file. Table-driven auth
  test covering every endpoint × {no token, bad token, read token, machine
  token}. Liveness transitions with a fake clock. Reconcile scoping
  (hooks-only session untouched; other herdr session name untouched; other
  machine untouched). Prefix lookup cases. Cross-machine 409. Title
  precedence matrix.
- `docs/server.md`: endpoints, config keys, env overrides, `sessionhub machine`.
- `docs/dev/evidence/task-1.md`: start `sessionhub server` on a temp DB and port, add a
  machine, and use `curl` to walk: register, event, report, title, list, detail,
  reconcile, bad token, and stale after a short `SESSIONHUB_STALE_AFTER`.

### Task 2: Capture real payloads

Capture the payloads every later test uses. Only writes under `testdata/`.

On `tower` (herdr 0.9.3, reached with `ssh tower`; herdr is at
`~/.local/bin/herdr`):

1. Create a probe plugin in `/tmp/sessionhub-probe-plugin/` with `id = "sessionhub-probe"`,
   `min_herdr_version = "0.9.3"`, `platforms = ["linux"]`, a `[[startup]]`
   entry, `[[events]]` for `pane.created`, `pane.closed`, `pane.exited`,
   `pane.agent_detected`, `pane.agent_status_changed`, and one `[[actions]]`
   entry `dump`. Every command is `/bin/sh -c 'env | grep ^HERDR_ > /tmp/sessionhub-probe/$(date +%s%N)-$HERDR_PLUGIN_EVENT.env; printf %s "$HERDR_PLUGIN_EVENT_JSON" > /tmp/sessionhub-probe/$(date +%s%N)-$HERDR_PLUGIN_EVENT.json'`
   (adjust quoting so it works through herdr's argv exec; the action writes
   `HERDR_PLUGIN_CONTEXT_JSON` instead).
2. `herdr plugin link /tmp/sessionhub-probe-plugin`, then trigger events: create a
   scratch workspace with `cwd=/tmp/sessionhub-probe-work`, split a pane, close it
   (`pane.created`, `pane.closed`); in another new pane run
   `herdr agent start probe --kind claude --pane <id>` and wait for it
   (`pane.agent_detected`, `pane.agent_status_changed`); capture
   `herdr pane get <id>` and `herdr api snapshot` while Claude runs (this
   carries a real `agent_session`); invoke the action with
   `herdr plugin action invoke dump --plugin sessionhub-probe`; then close the Claude
   pane (`pane.closed`/`pane.exited`) and the scratch workspace. Do not send
   Claude a prompt.
3. Capture raw socket exchanges (request line and response line) for
   `session.snapshot`, `pane.get` (existing and missing pane),
   `pane.report_metadata` with `source:"sessionhub"` and a `hub_summary` token (then
   clear it), and `pane.focus` on a missing pane.
4. `herdr plugin unlink sessionhub-probe`, delete `/tmp/sessionhub-probe*`, and confirm with
   `herdr plugin list` that nothing remains. The `[[startup]]` payload is
   captured only if herdr runs it on link; if it doesn't, note that in the
   README instead of restarting herdr.

On `bluebox` (Claude Code 2.1.284), in a scratch directory outside the repo:

5. Write `.claude/settings.local.json` with hooks for `SessionStart`,
   `UserPromptSubmit`, `Stop`, `Notification`, and `SessionEnd`, each running
   a command that saves stdin and `env | grep -E '^(CLAUDE|HERDR)'` (values of
   anything named `*TOKEN*` or `*KEY*` removed) to a file per event. Run
   `claude -p --model haiku "Reply with the single word ok"` there. If
   `Notification` does not fire in print mode, record that in the README; do
   not invent it.
6. Delete the scratch directory.

Scrub every file: `/home/me` → `/home/user`, workspace/tab labels and
terminal titles → neutral text, no tokens. Layout:
`testdata/herdr/events/*.json`, `testdata/herdr/env/*.env`,
`testdata/herdr/socket/*.ndjson`, `testdata/herdr/snapshot.json`,
`testdata/claude/hooks/<event>.json` and `.env`, and `testdata/README.md`
listing each file with how and when it was captured (herdr 0.9.3, Claude Code
version), and any gaps. `docs/dev/evidence/task-2.md` holds the commands you ran.
No Go code in this task.

### Task 3: Client library and herdr socket client

Build what the plugin, hooks, MCP server, and CLI share. Uses captured
payloads from Task 2.

- `internal/client/config.go`: load/save the client config (Global
  constraints paths and env overrides). Save writes mode 0600 and creates
  parent dirs.
- `internal/client/http.go`: `Client` with a 2 s timeout and bearer auth,
  and one method per endpoint: `UpsertSession`, `PutHerdrSessions`,
  `PostEvent`, `PostReport`, `SetTitle`, `ListSessions(live bool, machine string)`,
  `GetSession(idOrPrefix)`, `ListMachines`, `Health`. Non-2xx responses become
  errors carrying the status and the `api.Error` message.
- `internal/client/queue.go`: an append-only JSONL queue at
  `<state>/queue.jsonl`. `Append(item)` takes an exclusive `flock` on
  `<state>/queue.lock`, appends one line, fsyncs. `Drain(fn)` takes the lock,
  reads all items, calls `fn` with them, and rewrites the file with only the
  items `fn` returns as unsent (write to temp + rename). Items: a tagged union
  `{"op":"upsert"|"event"|"herdr_sessions", "session_id":..., "pane_id":...,
  "body": {...}, "queued_at": ...}`. Survives a kill between any two steps
  without losing or duplicating items (test it by crashing a helper process).
- `internal/gitinfo`: `Lookup(ctx, cwd) (repo, branch string)` with a 500 ms
  timeout using `git -C <cwd> remote get-url origin` and
  `git -C <cwd> rev-parse --abbrev-ref HEAD`; empty strings on any error.
- `internal/herdr`: socket client. `Dial(path)`, request IDs, and typed calls
  `Snapshot()`, `PaneGet(id)`, `PaneFocus(id)`, `PaneSplit(params)`,
  `AgentStart(params)`, `ReportMetadata(params)`, each with a 2 s deadline.
  Types for the parts sessionhub reads (`PaneInfo`, `AgentSessionInfo`, snapshot
  panes/agents/focused IDs), decoded leniently (unknown fields ignored).
  `SocketPath()` and `SessionName(socketPath)` per Global constraints. Parse
  `HERDR_PLUGIN_EVENT_JSON` into `{Event string; Data json.RawMessage}` plus
  helpers for the five hooked event kinds.
- `internal/herdr/herdrtest`: a fake herdr server on a temp Unix socket that
  replays responses from `testdata/herdr/socket/*.ndjson` and records requests.
- Tests use `testdata/` payloads only. `docs/client.md`. Evidence: run a small
  `go run` or test binary against the live herdr socket on `bluebox` (read-only
  calls: `Snapshot`, `PaneGet` on an existing pane) and record the output.

### Task 4: herdr plugin

Implement `docs/dev/PLAN.md` "herdr plugin" exactly: manifest, install, startup,
event handler, and watcher. Owns `internal/plugin`.

- `sessionhub install-plugin`: write the manifest from `docs/dev/PLAN.md` (with
  `min_herdr_version = "0.9.3"`, the absolute path of the running executable
  resolved with `os.Executable`, and the `status`/`resume` actions and the
  `resume-picker` pane) to the plugin dir, then run `herdr plugin link <dir>`.
  Idempotent. `sessionhub uninstall-plugin` runs `herdr plugin unlink sessionhub` and
  removes the dir.
- `sessionhub plugin startup`: `Snapshot()` → build `HerdrSessionsPut` from panes
  that have `agent_session` with `kind == "id"` (agent `claude` only in v1;
  others skipped), with `title_hint` from `terminal_title_stripped`,
  git info, and the herdr session name → `PutHerdrSessions`; on failure, queue
  it. Then `ensureWatcher`. Always exits 0.
- `sessionhub plugin event`: parse `HERDR_PLUGIN_EVENT_JSON`, append one queue item,
  `ensureWatcher`, exit 0. No network, no herdr calls. Must finish in well
  under 100 ms (measure it in the evidence).
- `ensureWatcher`: non-blocking `flock` on `<state>/watcher.lock`; if free,
  release it and start `sessionhub plugin watch` detached (`Setsid`, stdin/stdout/stderr
  to `/dev/null` except stderr to `<state>/watcher.log`, size-capped at 1 MiB by
  truncation on start).
- `sessionhub plugin watch`: holds `watcher.lock` for its lifetime. Every 2 s: drain
  the queue, coalesce (latest `state_changed` per pane wins; `pane_closed`
  beats earlier state for that pane), resolve pane → session from its cache
  (refreshed from `Snapshot()` when a pane is unknown), and send. Items that
  fail stay queued. Items whose pane can't be resolved are dropped after 10
  minutes. Every 60 s: snapshot → `PutHerdrSessions`. Exits when the socket's
  device/inode changes or it disappears. Logs one line per send failure.
- Tests: event handler writes the right item for each captured event;
  coalescing table; watcher loop against `herdrtest` and an `httptest` sessionhub
  server with a fake clock (including server down → queue retained → server up
  → sent once); ensureWatcher runs one watcher under concurrent calls.
- `docs/plugin.md`: install, what each hook does, files in the state dir,
  troubleshooting (`herdr plugin log list --plugin sessionhub`, `watcher.log`).
- Evidence: on `tower`, run a temporary `sessionhub server` on `127.0.0.1:8788` with a
  temp database. herdr runs plugin commands with its server's environment, so
  the plugin reads the default client config: write
  `~/.config/sessionhub/config.toml` on `tower` pointing at the temporary server (it
  does not exist yet; if it does, back it up and restore it). Install the
  plugin, show sessions appearing via `curl`, change a pane's agent state,
  close a Claude pane and show `ended`. Then uninstall the plugin, stop the
  server, and delete everything you created, including the watcher process,
  the state dir, and the temporary config.

### Task 5: Claude Code hooks client

Implement `docs/dev/SPEC.md` component 3b and `docs/dev/PLAN.md` "Claude Code hooks client".
Owns `internal/hooks`.

- `sessionhub hook <session-start|prompt|stop|notification|session-end>`: read stdin
  JSON (captured shapes in `testdata/claude/hooks/`), skip when `agent_id` is
  present, build the upsert/event, and send with the 2 s timeout. On failure,
  append to the queue. When no watcher holds `watcher.lock`, also drain the
  queue before exiting (sending at most 50 items). Always exit 0, even on
  malformed input.
  - `session-start`: upsert (cwd, git info, `HERDR_PANE_ID`,
    `HERDR_WORKSPACE_ID`, herdr session name from `HERDR_SOCKET_PATH`, title
    hint from `session_title` if present); write
    `<state>/current/<claude-pid>` containing the session ID, where
    `<claude-pid>` is the nearest ancestor process whose `/proc/<pid>/exe`
    basename starts with `claude` or whose path contains
    `/share/claude/versions/` (fall back to the parent PID).
  - `prompt`: `prompt` event (payload: first 200 chars) plus
    `state_changed` working; upsert with `first_prompt`.
  - `stop`: `state_changed` idle. `notification`: `state_changed` blocked.
  - `session-end`: append an `ended` event to the queue and exit (no network
    call; the 1.5 s `SessionEnd` budget). Remove `<state>/current/<pid>`.
- `sessionhub install-hooks`: back up `~/.claude/settings.json` to
  `settings.json.sessionhub-backup-<timestamp>`, add one matcher group per event with
  the table in `docs/dev/PLAN.md` (async flags, `SessionEnd` `timeout: 2`), each
  command the absolute `sessionhub` path + `hook <event>`. Preserve every other key
  and hook entry byte-for-byte where possible (decode into
  `map[string]json.RawMessage`, touch only `hooks`). Idempotent.
  `sessionhub uninstall-hooks` removes only entries whose command starts with the sessionhub
  path + ` hook `. Honor `CLAUDE_CONFIG_DIR`.
- Tests: each captured payload → the right requests (against `httptest`);
  exit 0 on garbage stdin and on server down; install/uninstall round-trip
  on a copy of a realistic settings file containing herdr's entry and other
  user hooks (herdr's entry is unchanged, verified by comparing the raw JSON).
- `docs/hooks.md`. Evidence: a scratch project with project-level hook
  settings pointing at the built `sessionhub`, a local `sessionhub server`, and one
  `claude -p --model haiku` run showing the session registered and its events.

### Task 6: MCP server

Implement `docs/dev/SPEC.md` component 4 and `docs/dev/PLAN.md` "MCP server". Owns
`internal/mcp`.

- `sessionhub mcp`: MCP over stdio (newline-delimited JSON-RPC 2.0). Handle
  `initialize` (protocol version echoed from the client's request when it is
  one this server supports, else `2025-06-18`), `notifications/initialized`,
  `ping`, `tools/list`, `tools/call`. Unknown methods get a JSON-RPC
  `-32601` error. Stdout carries only protocol messages; logs go to stderr.
- Tools `report_progress(done: string[], in_flight: string[], waiting_on:
  string[], note?: string)` and `set_title(title: string)` with JSON Schemas
  and descriptions telling the model when to call them. Results are text
  content. A server failure returns a successful tool result whose text says
  the report was queued (fail open), and the report is queued.
- Session ID resolution order from `docs/dev/PLAN.md` (herdr `pane.get` on
  `HERDR_PANE_ID`, then `<state>/current/<ppid>`, then
  `CLAUDE_CODE_SESSION_ID`), resolved on every call.
- On `report_progress`, if `HERDR_PANE_ID` is set, send `pane.report_metadata`
  with `source: "sessionhub"` and `tokens: {"hub_summary": <summary>}` where the
  summary is the first `waiting_on` item prefixed `waiting: `, else the first
  `in_flight` item, else `done: ` + the first `done` item, truncated to 80.
- `sessionhub install-mcp`: `claude mcp add --scope user sessionhub -- <abs sessionhub path> mcp`
  (remove and re-add if present). `sessionhub uninstall-mcp`: `claude mcp remove
  --scope user sessionhub`.
- `docs/CLAUDE-snippet.md`: the text for `CLAUDE.md` telling sessions to call
  `report_progress` when they finish a task, start a long one, or are blocked.
- Tests: JSON-RPC transcript tests over pipes; session ID resolution with
  `herdrtest` and temp state dir; summary rules table; server down → queued.
- `docs/mcp.md`. Evidence: `claude -p --model haiku --strict-mcp-config
  --mcp-config <tmp.json>` (pointing at the built binary and a local
  `sessionhub server`) with a prompt that makes it call `report_progress`; show the
  report in `GET /v1/sessions/{id}`.

### Task 7: Dashboard

Replace Task 1's `GET /` placeholder. Owns `internal/server/dashboard/`
(embedded `index.html`) and the route wiring in `internal/server`.

- `GET /?token=<read token>`: constant-time check, then set cookie
  `hub_read` (HttpOnly, Secure, SameSite=Strict, Path=/, Max-Age 400 days)
  and redirect 303 to `/`. `GET /` without a valid cookie returns a small
  401 page that says to open the link with `?token=`. Read endpoints accept
  the cookie.
- One self-contained HTML file: no build step, no external requests, inline
  CSS and JS. Fetches `GET /v1/sessions` and renders sessions grouped by
  machine, ordered by status (blocked, live, stale, ended) then
  `last_seen_at`. Each card: title, status colour, agent state, age, cwd and
  branch, the latest report (done / in flight / waiting on / note), and a copy
  button for `resume_command`. Ended sessions are collapsed under a toggle.
  Auto-refresh every 30 s. Readable at 360 px wide, and in dark mode.
- Tests: cookie exchange, bad token, cookie accepted on `/v1/sessions`,
  HTML served with `Content-Type: text/html` and a strict CSP.
- Evidence: a screenshot or `curl` output against a server with seeded
  sessions, plus the HTML checked at 360 px (a headless browser if available,
  else describe how it was checked).

### Task 8: CLI and resume

Implement `docs/dev/SPEC.md` component 5 and `docs/dev/PLAN.md` "Resume". Owns
`internal/cli` and `internal/resume`.

- `sessionhub ls [--all] [--machine M]`: table (machine, short ID, title, status,
  age, latest report summary). Default shows `live` and `blocked`; `--all`
  shows everything.
- `sessionhub show <id-prefix>`: full detail including report history and events.
- `sessionhub status`: this machine's sessions (the `sessionhub.status` action runs it).
- `sessionhub resume <id-prefix>`: fetch the session; if its machine is this
  machine (`machine` in client config), do the local steps in `docs/dev/PLAN.md`
  (focus the live pane, else split with the recorded cwd and `AgentStart`
  with `args: ["--resume", <uuid>]` and a name `sessionhub-<first 8 of uuid>`); if
  herdr is not running or the session has no herdr info, print
  `cd <cwd> && claude --resume <uuid>`. Otherwise print the remote recipe
  from `docs/dev/PLAN.md`, including the `herdr --machine` form when a saved herdr
  machine matches. Never run anything remote.
- `sessionhub resume --pick`: numbered list of this machine's non-live sessions
  plus live ones, read a number from the terminal, then resume it.
  `sessionhub plugin open-picker`: `herdr plugin pane open --plugin sessionhub
  --entrypoint resume-picker --placement overlay --focus`.
- Tests: table rendering; resume decision table (local live pane, local
  dead pane, no herdr, remote with and without a saved machine) against
  `herdrtest` and `httptest`; `--pick` with scripted stdin.
- `docs/cli.md`. Evidence: `sessionhub ls`, `sessionhub show`, and a local resume on
  `bluebox` or `tower` of a real ended session.

### Task 9: Join

Implement `docs/dev/SPEC.md` component 6 `sessionhub join`. Owns `internal/join`.

- `sessionhub join <server-ssh-target> [--name N] [--ssh-host H] [--herdr-host H]`:
  name defaults to the short hostname. If the local `SESSIONHUB_DB` (or default DB
  path) exists and a server config exists, run `sessionhub machine add` locally;
  otherwise run `ssh <target> ~/.local/bin/sessionhub machine add <name> --ssh-host H
  --herdr-host H --json` and parse the JSON. Write the client config. Then run
  `install-plugin` (if `herdr` is on `PATH` and reports ≥ 0.9.3; else print why
  it was skipped), `install-hooks`, `install-mcp` (if `claude` is on `PATH`),
  `Health`, and print `sessionhub status`. Print where to paste
  `docs/CLAUDE-snippet.md` and the herdr sidebar row snippet. Idempotent.
- Tests: with a fake `ssh` on `PATH` (a script that runs the local binary),
  a temp HOME, and fake `herdr`/`claude` scripts recording their arguments.
- `docs/join.md`. Evidence: a full join in a temp HOME against a local
  server.

### Task 10: Deploy to tower, README, and end-to-end

- `deploy/sessionhub.service` (systemd user unit: `ExecStart=%h/.local/bin/sessionhub
  server`, `Restart=on-failure`, `Environment=` none; config from
  `server.toml`).
- `Makefile` `deploy`: build, `scp bin/sessionhub tower:.local/bin/sessionhub.new`, `ssh tower`
  to move it into place, install the unit to
  `~/.config/systemd/user/sessionhub.service`, `systemctl --user daemon-reload`,
  `enable --now`, and `restart sessionhub`. One explicit command sequence, no script.
- `README.md`: what sessionhub is, install for the server (first-time `server.toml`
  with `listen = ["127.0.0.1:8787", "10.200.0.1:8787"]`, generating the read
  token, `make deploy`, the Cloudflare public-hostname step
  `sessionhub.example.com → http://10.200.0.1:8787` with no Access policy, the ufw check),
  install for each client (`sessionhub join tower`), the herdr sidebar row snippet,
  the `CLAUDE.md` snippet, and uninstall.
- Do the deployment: run `make deploy`, create `server.toml` on `tower` with a
  fresh read token (never printed into the repo or evidence), check the
  cloudflared container reaches `10.200.0.1:8787`
  (`docker exec cloudflared-tunnel` has no shell, so test with
  `docker run --rm --network bridge curlimages/curl` against
  `http://10.200.0.1:8787/healthz`), and run `sessionhub join` on `tower` and on
  `bluebox`. If `bluebox`'s herdr is still older than 0.9.3, join skips the plugin
  with a message; record that.
- End-to-end evidence (`docs/dev/evidence/task-10.md`): both machines' sessions
  in `sessionhub ls`, a `report_progress` from a real session visible in `sessionhub show`,
  a closed pane marked `ended`, and `/healthz` via `https://sessionhub.example.com` once
  the Cloudflare hostname exists (if it does not yet, record the exact step
  left for the user).
- As built: the unit also sets `RestartSec=5s` and `StartLimitIntervalSec=0`,
  because the `10.200.0.1` bind fails until `docker0` exists and a user unit
  cannot order itself after `docker.service`. `make deploy` also waits for
  `/healthz` and then runs `sessionhub install-plugin` on `tower` (once the client
  config exists), which stops the old plugin watcher and starts one on the
  new binary.
