# sessionhub: a central session registry for Claude Code sessions running in herdr

## Who I am and how I work
Senior DevOps/platform engineer. I run many Claude Code sessions in parallel, inside herdr (https://herdr.dev, agent-aware terminal multiplexer), on several Linux machines. I attach from a desktop and from Android (Moshi/Termux over SSH, sometimes through a Cloudflare Tunnel). I value accuracy over convenience: do not invent CLI flags, APIs, hook event names, or plugin manifest fields. Verify against the actual `herdr --help`, `herdr plugin --help`, `claude --help`, and the herdr and Claude Code docs matching the installed versions before using them, and quote what you found.

## Problem
Each herdr server only knows about its own machine, and it only knows agent *state* (working / blocked / done), not *meaning*. Claude Code's own session list is per-machine. I have no single place that answers:
1. Which sessions exist across all my machines, and which are live?
2. For each session: what has it finished, what is in flight, what is it waiting on (me, a CI run, another session)?
3. How do I get back into any of them from wherever I am?

## What to build
A small self-hosted service ("sessionhub") plus a herdr plugin and an MCP tool so that every Claude Code session is registered, reports progress, and can be resumed from any machine.

## Decisions already made (do not re-litigate; stop and ask if something makes one impossible)
- The primary client is a **herdr plugin**. Herdr's official Claude Code integration already reports the session UUID to herdr; the plugin reads it from herdr, it does not re-implement it.
- **Claude Code hooks are a supported alternative client** (component 3b) for sessions that run outside herdr, or on machines without herdr. Both clients share one Go client library and talk to the same endpoints. They can run side by side: the server upserts by Claude session UUID, so duplicate registrations are harmless. The hooks client is added by `sessionhub install-hooks` beside herdr's managed hook entries and never edits them.
- Central server: Go, single static binary, SQLite (modernc.org/sqlite or mattn), plain HTTP. Standard library where reasonable.
- Plugin language: Go (single binary, no runtime deps on client machines). If herdr's plugin conventions make a script noticeably simpler for a piece, say so and ask.
- Semantic progress ("done / in flight / waiting on") comes from an MCP tool the session calls. No transcript parsing of `~/.claude/projects/*.jsonl`.
- SQLite is the database, full stop. No storage abstraction, no cloud database.
- Hosting: the `sessionhub server` binary runs on the home machine `tower` as a systemd user service (see Deployment). Cloud Run was dropped on 2026-09-30: an always-on instance costs about $45–50 a month, and the free tier does not cover it (see `docs/dev/NOTES.md`).
- Scope v1: Claude Code only. Design the data model so other herdr-supported agents can be added later, but do not implement them.

## Components

### 1. `sessionhub server`
- HTTP + JSON API, SQLite database, single process.
- Auth: per-machine bearer tokens; every write is attributed to a machine. Machine tokens can also read, so the CLI on each machine uses its own token. Browsers sign in to the dashboard with a one-time link from `sessionhub login` (confirmed with a button, shown with a QR code) and then hold a session cookie that lasts 30 days after its last use; `sessionhub login rm` revokes one without a restart. The design is in `docs/dev/superpowers/specs/2026-10-01-web-sign-in-design.md`.
- Machine enrollment is one command per machine and needs no copy-pasted secrets (see component 6).
- Endpoints (adjust naming, keep the shape):
  - `POST /v1/sessions` — register/upsert a session
  - `POST /v1/sessions/{id}/events` — lifecycle events forwarded by the plugin or the hooks client
  - `POST /v1/sessions/{id}/report` — structured progress report from the MCP tool
  - `GET /v1/sessions?live=true&machine=...` — list
  - `GET /v1/sessions/{id}` — full detail including report history
  - `POST /v1/sessions/{id}/remote-control` — ask the session's machine to turn on Claude Code Remote Control (dashboard cookie with `X-Hub-Action: remote-control`, or a machine token)
  - `GET /v1/machines/self/control?wait=30` — the plugin watcher's long poll for those requests
  - `POST /v1/control/{request-id}/result` — the watcher's answer
  - `GET /healthz`
- Liveness: a session is `live` if a heartbeat or event arrived within N minutes (configurable, default 5). Mark `stale` after that, `ended` when the plugin reports the pane/agent gone.
- Serve a mobile-friendly, dependency-free web dashboard at `/` (single HTML file, no build step): sessions grouped by machine, status colour, last report, and a copy button for the exact resume command. Read-only, with one exception: each session in herdr has a **Remote Control** button that turns on Claude Code Remote Control, or resumes the session with it on, and shows an **Open in Claude** link. The design is in `docs/dev/superpowers/specs/2026-09-30-remote-control-button-design.md`. Must be usable on a phone screen.

### 2. Data model (minimum)
```
machines:  id, name, token_hash, last_seen
sessions:  id (Claude session UUID), machine_id, cwd, git_repo, git_branch,
           herdr_session, herdr_workspace, herdr_pane (nullable),
           title (first prompt or user-set), started_at, last_seen_at,
           status (live|stale|ended|blocked), ended_at
events:    id, session_id, ts, source (plugin|hooks|mcp), kind (registered|state_changed|pane_closed|heartbeat|...), payload_json
reports:   id, session_id, ts, done_json, in_flight_json, waiting_on_json, note
```
`done`, `in_flight`, `waiting_on` are lists of short strings. The latest report is the canonical view; history is kept.

### 3. herdr plugin `sessionhub`
Manifest `herdr-plugin.toml`, installable with `herdr plugin install` / `herdr plugin link`.
- `[[startup]]`: after server restore, take a full pane/agent snapshot via the herdr CLI or socket API and upsert every pane that has an `agent_session` reference into the central server. Record machine name, herdr session name, workspace, pane id, cwd, agent kind, agent session id/path.
- `[[events]]` (or a long-running subscriber started from startup, whichever the installed herdr version supports for continuous handling — check, and note that startup hooks are one-shot, not supervised): forward pane created/closed, agent state changes, and agent session reports to the server. Debounce; never block herdr; fail open if the server is unreachable and retry from a local queue in `~/.local/state/sessionhub/`. Client-side POST timeout ≤ 2s.
- `[[actions]]`:
  - `sessionhub.status` — print this machine's sessions as seen by the server (sanity check).
  - `sessionhub.resume` — given a sessionhub session id, if the session belongs to this machine: focus the pane if it is still alive; otherwise open a pane in the recorded cwd and start `claude --resume <uuid>` (use herdr's agent start / native restore mechanism if it exposes one; verify). If it belongs to another machine: print the `herdr --remote <host>` command (or `herdr machine` equivalent; verify syntax) and the action to run there. Never run remote commands silently.
- The watcher also keeps a long poll open for Remote Control requests and runs them in the local herdr (inject `/remote-control`, or resume in a new workspace).
- Do not modify herdr's managed integration hook files.

### 3b. Claude Code hooks client (alternative to the plugin)
Same `sessionhub` binary, invoked as `sessionhub hook <event>` by Claude Code with the hook JSON on stdin. Installed at user scope by `sessionhub install-hooks`, which adds entries beside herdr's managed ones and removes only its own on `sessionhub uninstall-hooks`.
- Use when a session runs outside herdr, or on a machine without herdr. On a herdr machine with the plugin installed, hooks are optional; both clients may run.
- Hook events (verify names, matchers, and stdin fields against the installed Claude Code version's hooks reference, and quote them in `docs/dev/NOTES.md`):
  - `SessionStart` → register/upsert the session (session id, cwd, git repo/branch, machine). If `HERDR_PANE_ID`, `HERDR_WORKSPACE_ID`, and `HERDR_TAB_ID` are in the environment, include them.
  - `UserPromptSubmit` → heartbeat and `state_changed` (working). The first prompt sets the title if none is set.
  - `Stop` → heartbeat and `state_changed` (idle).
  - `Notification` → `state_changed` (blocked) when Claude Code waits on the user.
  - `SessionEnd` → `ended`.
- Same client rules as the plugin: POST timeout ≤ 2s, fail open, retry from the shared local queue in `~/.local/state/sessionhub/`, never block or fail the Claude Code turn (always exit 0; use the hooks' async mode if the installed version has one).
- Known limit: hooks fire only on activity, so an idle hooks-only session goes `stale` after N minutes even though its process is alive. Do not add a background heartbeat daemon to fix this in v1; note it in `docs/dev/IDEAS.md`.
- Every event records its `source` (`plugin`, `hooks`, or `mcp`) so the server and tests can tell the clients apart.

### 4. MCP server `sessionhub mcp`
Registered with Claude Code at user scope by `sessionhub install-mcp`.
- Tools: `report_progress(done: [str], in_flight: [str], waiting_on: [str], note?: str)`, `set_title(title: str)`.
- Session identity: read the Claude session UUID from `CLAUDE_CODE_SESSION_ID`, which Claude Code sets in every MCP server's environment. This works with or without herdr and with or without the hooks client.
- On every call: POST the report to the central server, and also run `herdr pane report-metadata "$HERDR_PANE_ID" --custom-status "<short summary>"` so the herdr sidebar shows it locally. Detect `HERDR_PANE_ID` from the environment; if the session is not inside herdr, skip the herdr call and still report centrally. Verify the exact flag names of `pane report-metadata` on the installed version.
- Ship a `CLAUDE.md` snippet (user-level or per-project) telling sessions to call `report_progress` when they finish a task, start a long one, or are blocked waiting on me.

### 5. `sessionhub` CLI (client, cross-machine)
- `sessionhub ls` — table of sessions (machine, title, status, age, last report summary).
- `sessionhub show <id-prefix>` — full detail.
- `sessionhub resume <id-prefix>` — delegates to the plugin action when the target is local; prints the remote-attach recipe otherwise. If herdr pane info is missing, fall back to printing an SSH + cd + `claude --resume` command.
- `sessionhub remote-control <id-prefix>` — turns on Claude Code Remote Control for a running local session by sending `/remote-control` to its pane through herdr (`agent.prompt`) and printing the URL Claude shows. For a session on another machine it asks that machine's watcher through the sessionhub, and prints the SSH command when the sessionhub can't reach it. `sessionhub resume --remote-control` starts Claude with `--remote-control`.
- Each machine's `ssh_host` / `herdr_host` is stored on the server when the machine joins (default: its hostname, overridable with a flag), so every client prints correct resume commands without per-client config.

### 7. Session insights
Digests tell you what a session did without opening it. Each machine builds them from the Claude Code transcripts and from git, and pushes them with `PUT /v1/sessions/{id}/digest`. The server stores one digest per session (see `docs/server.md`, "Session digests").
- A digest holds the recap, the AI and custom titles, token usage and cost, pull request and merge request links, and git facts (recent commits, `uncommitted`, `unpushed`). The last prompt comes from the `prompt` events the hooks already send.
- The dashboard and the CLI show the recap, the last prompt, and a summary line. The dashboard also has a **Details** view for one session and a filter over the list; `sessionhub ls --grep` filters in the CLI.
- Machines build digests on Claude Code hook events and from the plugin watcher's heartbeats, and `sessionhub digest` builds them on demand (see `docs/client.md`, "Digests").
- Rule: Claude's reply text never leaves the machine. The reader uses timestamps, usage, the recap, titles, and links. It never reads or sends the text of Claude's replies.

### 8. Inbox
One list, across machines, of the sessions that need you: blocked on a prompt, waiting on you (a report with `waiting_on`), or finished a turn you haven't answered. The dashboard's **Inbox** tab and `sessionhub inbox` show it; on the dashboard, tapping an item opens the session, and **Done** and **Later** dismiss or snooze it. Triage is stored on the server, one row per session, shared by every viewer. A blocked or waiting session reaches you: a Telegram message from the server (after 30 seconds, once per block or request), and, for a blocked one, a herdr notification with a sound on the session's machine. When herdr marks a finished pane seen, its **Finished** item is dismissed. The herdr sidebar shows the inbox count on each workspace, and `sessionhub inbox --watch`, a herdr plugin pane, jumps to a session on any machine. Answering a permission prompt from the dashboard's inbox or the CLI is section 9; Telegram and the inbox pane only link to it. The designs are in `docs/dev/superpowers/specs/2026-10-01-inbox-design.md` and `docs/dev/superpowers/specs/2026-10-01-inbox-alerts-design.md`.

### 9. Session actions
sessionhub acts on sessions, not only shows them. Shared instructions: one short list of standing rules (each at most 300 characters, 2,000 in all) that every session on every machine sees at its start and with every prompt, through a synchronous `sessionhub hook context` entry that reads a local copy and never waits on the network; `sessionhub rules`, the dashboard's **Rules** tab, and the MCP tools `remember`, `forget`, and `instructions` edit it. Messages: `sessionhub send`, the dashboard's send bar, and the MCP tool `send_to_sessions` send one message to up to 20 sessions in herdr; the session's watcher types it in with herdr's `agent.prompt` only when the agent is idle or done, and retries for 10 minutes. Remote permission answers: a `PermissionRequest` hook posts each permission prompt and waits up to 10 minutes; **Allow once** or **Deny** in the inbox, or `sessionhub approve` and `sessionhub deny`, answer it, while the terminal dialog stays and wins if answered first. Browser writes need `X-Hub-Action` `instructions`, `send`, or `approve`. sessionhub never sends a permanent permission rule, and Telegram only links to the inbox. The design is in `docs/dev/superpowers/specs/2026-10-02-session-actions-design.md`.

### 10. Moving a session
A session moves between machines with its conversation, its branch, and its uncommitted changes, and resumes in a herdr pane on the target: `sessionhub move <id> <machine>` or the dashboard card's **Move to…**. Only an idle or done session in herdr moves. The source's watcher pushes the branch to `origin`, ends Claude with `/exit`, packs the transcript, its sidecar folder, the file history, `git diff --binary HEAD`, and the untracked files (secret-looking files such as `.env*`, `*.pem`, `*.key`, and `*.tfvars` never travel, tracked or untracked), and seals the bundle with X25519, HKDF-SHA256, and AES-256-GCM for the target machine's key; the server relays the ciphertext and deletes it when the move ends. The build checks sizes before it reads files and never writes the repository's index. The target finds a clean clone with the same remote that no other agent on the target works in when its branch would change, fast-forwards the branch, applies the changes, writes the transcript, and resumes while the move is still open; a failure on the target takes back only what the move did to the clone. The source restarts the session after a failure only once the sessionhub has recorded it. A target whose `done` the sessionhub did not record ends Claude again; if the target can't end Claude, the move is marked done with a note on the pane, because the live session decides who owns the session. The source archives its copy once the move is done, so a session never runs in two places. While a move is open, the server refuses Remote Control for the session, the dashboard hides its Remote Control button, and `sessionhub resume` refuses it. For a GitHub remote, `sessionhub move <id> cloud` records uncommitted work on a sessionhub-owned branch without touching the working tree, never overwriting commits a cloud session pushed there, and starts a Claude Code cloud session with a hand-off prompt. A cloud move that may have started a cloud session never restarts the local one; the source keeps a marker in its state directory and adds a note to the move. A cloud move's `done` clears the session's herdr pane, so the dashboard offers no Remote Control resume of the local copy. The design is in `docs/dev/superpowers/specs/2026-10-03-move-session-design.md`.

### 6. Machine enrollment
Goal: adding a machine is one command, and sessions on it then register themselves with no further steps.
- Trust comes from the SSH access you already have to `tower`; there is no shared enrollment secret.
- On `tower`, `sessionhub machine add <name> [--ssh-host H] [--herdr-host H]` writes the machine row directly to the local database and prints a new per-machine token once. Only the token hash is stored. Running it again for an existing name rotates the token.
- On a client, `sessionhub join <tower-ssh-target>` does the rest:
  1. Runs `ssh <tower-ssh-target> sessionhub machine add $(hostname)` and reads the token and server URL from its output.
  2. Writes `~/.config/sessionhub/config.toml` (mode 0600).
  3. Installs the clients that fit the machine: the herdr plugin if `herdr` is present, the Claude Code hooks, and the MCP server (`sessionhub install-mcp`). Each step is skipped with a message if its prerequisite is missing.
  4. Calls `/healthz` and `sessionhub status` to confirm the machine can reach the server.
- `sessionhub join` is idempotent; rerunning it rotates the token and reinstalls the clients.
- `sessionhub machine rm <name>` on `tower` revokes a machine.

## Non-goals for v1
- No multi-user, no orgs, no RBAC beyond per-machine tokens.
- No transcript sync between machines.
- No live cost tracking or billing. Digests carry token totals and the last dollar figure Claude Code recorded, for display only.
- No replacing herdr's own UI or herdr-web. Hub is the cross-machine index; herdr stays the local multiplexer.

## Quality bar
- Tests for the server API, the plugin's event handling, the hooks client's event handling, and the MCP tool, using real sample payloads captured from the installed herdr and Claude Code (capture them; do not fabricate).
- `make build`, `make test`, `make deploy` (build and install on `tower` over SSH), and a `README.md` with install steps for the server and for each client machine.
- Config via a single TOML/YAML file plus env overrides. No secrets in the repo.
- Keep it small. If a feature is not needed to answer the three questions in "Problem", leave it out and note it in `docs/dev/IDEAS.md`.

## Deployment: `tower`
- `tower` is on the home LAN (`192.168.1.145`, SSH alias `tower`, public SSH name `tower.example.com`), Ubuntu 25.10, x86_64, with systemd and Docker. It is also a client machine.
- `sessionhub server` runs as a systemd **user** service with `Restart=on-failure`. The README includes `loginctl enable-linger` so it runs without a login session.
- SQLite at `~/.local/share/sessionhub/sessionhub.db`, WAL mode, single writer (one process). No Litestream and no bucket in v1: the database lives on `tower`'s disk. Live session state rebuilds itself from the plugin's startup snapshot; only report history would be lost with the disk. Backups go in `docs/dev/IDEAS.md`.
- Config in `~/.config/sessionhub/server.toml` plus env overrides; the read-only dashboard token lives there (mode 0600), never in the repo.
- `make deploy`: cross-compile a static binary, `scp` it to `tower`, install the unit file, and `systemctl --user restart sessionhub`. One explicit command sequence in the Makefile, no deploy script.
- Reachability: `https://sessionhub.example.com` through the existing Cloudflare Tunnel on `tower` (container `cloudflared-tunnel`, token-based, so the public hostname is added in the Cloudflare dashboard, not in a file). The server listens on the Docker bridge address `10.200.0.1:8787`, which the container can reach, and on `127.0.0.1:8787`. It does not listen on the LAN address. Every client, the CLI, and the phone use `https://sessionhub.example.com`.
- No Cloudflare Access policy on `sessionhub.example.com`: clients authenticate with bearer tokens, and Access would block them. The app does its own auth.
- Non-goals: no containers, no CI, no multi-host, no TLS termination inside the app.

## How to proceed
1. Inspect the environment first: `herdr --version`, `herdr plugin --help`, the plugins and socket API docs for that exact version (docs are per-release); `claude --version`, `claude --help`. Write findings to `docs/dev/NOTES.md` before designing. Specifically confirm:
   - Which events the manifest `[[events]]` block can subscribe to, and whether there is a `server.ready`-style event (as of recent releases there is not; herdr-resurrect works around it by watching the socket file — read that plugin's README).
   - The exact shape of `agent_session` metadata in pane/agent responses.
   - Whether `HERDR_PANE_ID` and `HERDR_BIN_PATH` are set in agent panes or only in plugin invocation context.
   - Read herdr-resurrect and herdr-plugin-agent-usage as prior art for plugin structure; do not copy code, note what they do.
2. Propose the final API, the herdr events you will use, and the plugin manifest, in a short plan. Wait for my approval.
3. Implement in this order: server + DB + `sessionhub machine add/rm` + tests → plugin (startup snapshot, event forwarding) → Claude Code hooks client → MCP tool → dashboard → resume → `sessionhub join` → deploy to `tower`. Commit after each step with a clear message.
   - The hooks client comes right after the plugin because it reuses the client library the plugin step builds (config, HTTP client with the 2s timeout, local retry queue) and adds only a stdin parser and an installer.
   - It comes before the MCP tool because `report_progress` needs a registered session to attach to. Outside herdr, only the hooks client registers sessions, so without it the MCP tool could not be tested end to end for non-herdr sessions.
4. Do not add scope. If you think something is missing, ask; do not build it.
5. If any of the "Decisions already made" turns out to be wrong or impossible (e.g. herdr cannot be queried for pane identity), stop and tell me instead of working around it.

Questions to ask me before writing code, if the answers are not obvious from the environment:
- Answered 2026-09-30: there are two client machines. `tower` has SSH name `tower.example.com`, and `bluebox` (WSL2) has SSH name `bluebox.example.com`. Both run herdr and Claude Code. The CLI uses each machine's own token; the read-only token is for the dashboard only. Everything, including the phone, reaches the server at `https://sessionhub.example.com`.
