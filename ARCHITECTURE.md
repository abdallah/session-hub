# Architecture

This page is a map for contributors: what each piece does, how data flows,
and where to look in the code. The component docs in `docs/` cover the
behavior in full.

## One binary, several roles

`cmd/sessionhub/main.go` routes a subcommand to a package. The same binary
runs as:

| Role | Command | Package | Runs |
|---|---|---|---|
| Server | `sessionhub server` | `internal/server`, `internal/store` | On one always-on host, as a systemd user unit or a container. |
| Hooks client | `sessionhub hook <event>` | `internal/hooks` | Started by Claude Code for each hook event. |
| MCP server | `sessionhub mcp` | `internal/mcp` | Started by Claude Code, one per session, over stdio. |
| herdr plugin | `sessionhub plugin startup\|event\|watch` | `internal/plugin` | Started by herdr; `watch` is the long-running watcher. |
| CLI | `sessionhub ls`, `inbox`, `resume`, … | `internal/cli`, `internal/resume` | Run by you. |
| Enrollment | `sessionhub join` | `internal/join` | Once per machine. |
| Claude Code mod | `sessionhub install-mod`, `sessionhub mod <command>` | `internal/claudemod` (the installer and the mod's JavaScript, embedded), `internal/modcmd` | The mod runs inside each interactive Claude Code session and calls `sessionhub mod`. |

Shared code: `internal/client` (config, HTTP client, offline queue),
`internal/api` (the JSON types both sides use), `internal/paths` (every
file location and its env override), `internal/herdr` (the herdr socket
client), `internal/digest` (commits, links, and cost read from a
transcript), and `internal/move` (moving a session between machines).

## Data flow

```
Claude Code ──hook JSON──▶ sessionhub hook ──┐
Claude Code ──stdio──────▶ sessionhub mcp ───┤
herdr ───────events──────▶ sessionhub plugin ┤      offline queue
                                             ├──▶ ~/.local/state/sessionhub/queue.jsonl
                                             │           │ drained by the watcher,
                                             │           │ `hook flush`, and retries
                                             ▼           ▼
                                   POST /v1/sessions, /events, /report
                                             │
                                             ▼
                                sessionhub server ── SQLite (store)
                                   │        │
                       dashboard ◀─┘        └─▶ Telegram alerts
                       CLI (GET)
```

- **Writes fail open.** Every client call has a 2-second limit. A failed
  write goes to the offline queue and is replayed later; Claude Code and herdr
  never wait for the server. See `docs/client.md`.
- **Sessions are keyed by Claude's session UUID.** The hooks, the MCP
  server, and the herdr plugin can all report the same session; the server
  upserts, so duplicates are harmless.
- **Status is computed when read.** `live`, `blocked`, `stale`, and `ended`
  come from the last event and `stale_after` (`internal/store`).

## Requests from the server to a machine

The server never connects to a machine. Actions that must happen on a machine,
such as turning on Remote Control, typing a message into a session, moving
one, or starting a new one, are queued on the server. The machine's herdr watcher picks them
up through a long poll (`GET /v1/machines/self/control?wait=30`) and posts
the result back. Permission prompts work the other way round: the
`PermissionRequest` hook holds a long poll on the server until someone
answers on the dashboard or the CLI, or the terminal answers first.

A session with the Claude Code mod takes its own messages: the mod runs
`sessionhub mod poll`, which long-polls
`GET /v1/sessions/{id}/messages/next`, so a session outside herdr can get
messages too. While a session's mod polls, the watcher leaves its messages
alone.

## Auth

- **Machines** get a bearer token from `sessionhub join` (or
  `sessionhub machine add`). The server stores only its hash. Every write is
  attributed to the machine whose token made it.
- **Browsers** sign in with a one-time link from `sessionhub login`, and then
  hold a cookie that lasts 30 days after it was last used. State-changing
  dashboard requests also need an `X-Hub-Action` header.
- **Moves** are sealed between the two machines with per-machine keys
  (`~/.config/sessionhub/move.key`); the server only relays the bundle.

See `docs/server.md`, "Authentication", and [`SECURITY.md`](SECURITY.md).

## Storage

One SQLite file (`modernc.org/sqlite`, pure Go, so the binary is static).
The schema version is in `PRAGMA user_version`. `internal/store/store.go`
upgrades an older database on start and refuses a newer one, so a rollback
means restoring a backup (`docs/self-hosting.md`, "Upgrade").

Tasks (`internal/store/tasks.go`, `taskday.go`) are shared data like the
inbox triage: one list for you, written by the dashboard, the CLI, and
agents through MCP. Past days are rebuilt from the `task_events` log
(`docs/server.md`, "Tasks").

## The dashboard

`internal/server` serves a single HTML page with inline CSS and JavaScript,
with no build step and no dependencies. It polls the same JSON API the CLI
uses.

## Tests

- `go test ./...` runs everything. There are no external services: tests use
  temp `HOME` directories, an in-process server, and fake `ssh`, `herdr`, and
  `claude` scripts on a private `PATH`.
- `internal/herdr/herdrtest` is a fake herdr socket that replays responses
  captured from a real herdr in `testdata/herdr/`. `testdata/claude/` holds
  real Claude Code hook payloads. `testdata/README.md` says how each was
  captured.
- Tests that need a live herdr are skipped unless `SESSIONHUB_LIVE_HERDR` is
  set.

## Design history

`docs/dev/` holds the original spec, the plans, design notes for each
feature, and the evidence recorded as each task was done. It explains why
things are the way they are; it isn't user documentation.
