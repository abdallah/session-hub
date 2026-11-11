# Rex as a second terminal backend

Problem. `sessionhub start`, `resume`, `remote-control`, and `move` need a herdr
pane. A machine that runs Rex and no herdr has no watcher and can do none of
them. Sessions already record `rex_session` and `rex_block`; only `attach` uses
them.

Branch `rex-backend`, worktree `~/Code/session-hub-rex-backend`.

## Shape

```
sessionhub server <──HTTP── machine watcher ──> herdr pane (herdr socket)
                                           └──> Rex block  (rex CLI)
```

- One switch: `api.Session.Backend()` is `herdr` for a row with a herdr pane,
  `rex` for a row with a Rex block, `""` for neither. A row never has both:
  an upsert with a herdr pane clears the Rex IDs, and an upsert with a Rex
  block and no pane clears the herdr IDs (`internal/store/sessions.go`).
- A request with no session row yet (a start, the target of a move) uses the
  machine's backend: `resume.Backends(socket)` is herdr when the socket
  answers, Rex when `rex` is on `PATH`; herdr first.
- `internal/rex`: every `rex` call. `Call` is `mirror_rex.go`'s helper, moved
  so `internal/resume` can use it; the mirror calls it. Also `ClaudeRuns`,
  `Screen` (`format` as text), `Submit` (`write`, then Enter), `Start`
  (`rex new` or `rex run`), `Focus`, `Kill`, `Reap`, `Installed`.
- Rex variants sit beside the herdr code: `internal/resume/rex.go`
  (`controlRex`, `startResumedRex`, `startNewRex`, `localRex`) and
  `internal/plugin/move_rex.go` (`rexIdle`, `endRex`, `stopRex`).

## Routes (server)

| Route | Change |
|---|---|
| `PUT /v1/machines/self/backends` | New. Machine token. Body `{"backends":["herdr","rex"]}`; an empty list is stored as `none`. 204. |
| `POST /v1/sessions/{id}/remote-control`, `/attach` | A session with a Rex block and no herdr pane is accepted. Neither: `409` "session … is not in herdr or Rex". |
| `POST /v1/machines/{name}/start` | `409` "machine … has no herdr or Rex" when the machine reported `none`. |
| `POST /v1/sessions/{id}/move` | Source: a Rex block counts like a pane. Target that reported `none`: `409`, same text as start. |

Store: schema v22 adds `machines.backends` (`''` = never reported, which is a
watcher from before this version, so herdr). `Session.Controllable` is true
for a pane or a block; `Messageable` stays herdr pane or mod. A Rex block
names one session per machine: a Rex upsert takes the block from any other
row. Remote Control for a Rex row needs a watcher that reported `rex`.

## Watcher

- `sessionhub watch` runs the watcher as a plain process. With a herdr socket
  it is `sessionhub plugin watch`. Without one it runs with no herdr calls and
  exits when a socket appears, so the next start uses it.
  `sessionhub install-watcher` writes and enables the systemd user unit
  `sessionhub-watcher.service` (`Restart=always`); `uninstall-watcher` removes
  it. `sessionhub plugin watch` still exits when there is no socket.
- Each heartbeat, both modes: `PUT backends`, then Rex liveness
  (`rexBeat`): for this machine's live sessions with a Rex block,
  `terminal.process` says whether Claude runs. Running: upsert (last seen).
  Block gone: an `ended` event with reason `rex_block_gone`. Block there
  with no Claude shown: nothing (Claude may run under tmux in it).
  Live Rex sessions enter the watcher's cache, so snapshots cover them.
  Rex keeps a session whose command exited, empty: `rex.Reap` kills one that
  sessionhub created (label `sessionhub: …`) once its block is gone.
- Remote Control (`resume.Control`): Claude runs in the block → `write`
  `/remote-control`, read the link from `format` text. Block gone, or no
  Claude shown and the hooks reported the end → `rex new … -- claude
  --resume ID --remote-control`, unless the server reports the session
  live. The watcher records the new session and block IDs on the row.
- Start (`resume.StartNew`): `rex new "sessionhub: new in DIR" -c DIR --
  claude --remote-control`, wait for the link (in a narrow block it is on
  the line below the banner), then submit the first prompt.
  The session's own hook records its Rex IDs.
- Move: `idlePane`/`endSession` variants use `process` and the server's agent
  state (hooks report it; Rex does not) and `write` `/exit`. The target and a
  source restart use `startResumedRex`. A start the move takes back always
  ends with `rex session kill`. `cloneFree` on a machine without herdr checks this
  machine's live sessions' directories instead of herdr panes.

## CLI

`sessionhub resume <id>`, session on this machine with a Rex block, or with no
backend recorded on a machine that has Rex and no herdr:

| Case | Action |
|---|---|
| Claude runs in the block | `rex block focus` |
| It does not | `rex run -c CWD -- claude --resume ID` (new tab, focused) |
| `rex run` fails (no Rex session open) | `rex new "sessionhub: TITLE" -c CWD -- claude --resume ID` |
| Both fail | print `cd CWD && claude --resume ID` |

Session on another machine with a Rex block: print
`ssh -t HOST '~/.local/bin/sessionhub resume ID'`.

`sessionhub remote-control <id>` on this machine sends `/remote-control` to the
block. `sessionhub start` and `sessionhub move` need no change: the watcher
picks the backend.

## Limits

- Idle comes from the hooks' agent state: a Rex session with no hooks
  installed cannot move.
- A blocked Rex session is known only from the server's state; `write` does
  not refuse.
- A move from a snapshot does not end a Claude left in a Rex block on the
  source: the row no longer names the block.
- Messages to a session are still herdr pane or mod only.
- `install-watcher` is systemd only.
- `rex block stream` is not used; the attach mirror still polls.

## Acceptance

- [x] Start in Rex, the link, `/remote-control` on a running block, and `/exit`: live on togh, Rex 0.0.1794371922, Claude Code 2.1.322 (`TestRexLive`, `SESSIONHUB_LIVE_REX=1`).
- [ ] Rex-only machine, watcher running: `sessionhub start <machine>` opens a Rex session running Claude with Remote Control and prints the link.
- [ ] `sessionhub move <id> <rex-machine>` from a herdr machine resumes the session in a Rex session; the reverse lands in a herdr workspace.
- [ ] `sessionhub resume <id>` on the Rex machine focuses the block when Claude runs, else starts it with `rex run` in the session's cwd.
- [ ] A machine with neither backend gets `409`.
- [ ] `docs/cli.md`, `docs/plugin.md`, `docs/server.md` say "herdr pane or Rex block"; the resume table has the Rex rows.
- [ ] `go test ./...` green.
