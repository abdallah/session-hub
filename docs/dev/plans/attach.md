# sessionhub attach: a terminal mirror of one session

Problem. A guest signed in for one session (`sessionhub login --session`) can use
the dashboard but has no terminal access. Terminal access today needs an SSH
account on the machine and shows the whole herdr.

Branch `attach-session`, worktree `~/Code/session-hub-attach-session`.

## Shape

```
guest terminal ──HTTP──> sessionhub server <──HTTP── machine watcher ──> herdr pane / Rex block
```

No WebSocket dependency. Each side has one chunked GET stream (bytes toward it)
and small POSTs (bytes away from it). The server relays in memory.

Shared contract, already on the branch:

- `internal/api/attach.go`: `ActionAttach`, `HeaderActionAttach`, `Attach{ID, SessionID, Machine}`.
- `internal/client/http.go`: `CreateAttach`, `AttachOutput`, `SendAttachInput`,
  `MachineAttachInput`, `PostAttachOutput`, `NewGuest` (cookie auth for a guest sign-in).

## Routes (server)

| Route | Auth | Guest | Body |
|---|---|---|---|
| `POST /v1/sessions/{id}/attach` | machine token, or cookie with `X-Hub-Action: attach` | yes, own session only | → `api.Attach`, 202 created / 200 pending exists. Creates a control request with action `attach` for the session's machine and notifies its long poll. `409` when the session has no machine or the machine's watcher is offline, like remote-control. |
| `GET /v1/attach/{id}/output` | same | yes, when the attach's session is in scope | chunked `application/octet-stream`; terminal bytes from the machine; flush per chunk; ends when the machine side ends or the attach expires |
| `POST /v1/attach/{id}/input` | same | yes | raw bytes from the guest, cap 64 KiB; 204 |
| `GET /v1/machines/self/attach/{id}/input` | machine token of the attach's machine | no | chunked stream of guest input; ends when the guest side ends |
| `POST /v1/machines/self/attach/{id}/output` | same | no | raw bytes, cap 1 MiB; 204 |

Relay state is in-process (`attachHub`): per attach, an output channel, an input
channel, which sides are connected, last activity. One output reader and one
input reader at a time (second gets 409). The attach ends when the machine posts
its control result, when the guest's output stream has been gone 60 s, or after
30 min without any traffic. Control request `attach` reuses the control request
table and the `GET /v1/machines/self/control` long poll; `ClaimControl` returns it
like the others. The routes table test requires an auth test case per route.

## Watcher (plugin)

`internal/plugin/attach.go`:

```go
// mirror streams one session's terminal to out and feeds in to it until
// ctx ends or the terminal ends.
type mirror interface {
	Run(ctx context.Context, s api.Session, out io.Writer, in io.Reader) error
}
func newMirror(socket string, s api.Session) mirror // s.RexBlock != "" → rexMirror, else herdrMirror
```

On a claim with `ActionAttach`, the controller opens `MachineAttachInput`,
runs the mirror with an `out` that batches bytes (flush every 30 ms or 8 KiB)
into `PostAttachOutput`, and posts the control result when `Run` returns:
`done` with detail "guest left" / "session ended", or `failed` with the error.
The attach does not block the control loop: it runs in its own goroutine.

- `herdrMirror`: `pane.read --ansi --source visible` on each `pane.updated`
  event for the pane (subscribe via `events.subscribe`; fall back to a 250 ms
  poll if subscribing is not available in `internal/herdr`). Each frame is
  `\x1b[H\x1b[2J` + frame when it differs from the last. Input goes to
  `pane.send_input` (add it to `internal/herdr` if missing; `SendKeys` exists).
  `Run` returns when the pane is gone.
- `rexMirror`: owned by the Rex session (see below). `data.connect` with
  `rex.data.terminal.v1` on `s.RexSession`/`s.RexBlock` over the local Rex
  socket; input through `block.event`. Until it lands, `newMirror` returns an
  error for a Rex-only session.

## CLI

`sessionhub attach <login-url | session-id-prefix>`:

- A login URL (`https://hub/login/<code>`): redeem it as the browser does,
  `POST /login/<code>` with `Origin: <hub origin>`, no redirect following, take
  the `__Host-hub_session` cookie from `Set-Cookie`. Save
  `{server_url, token, session, expires_at}` to `paths.StateDir()/guest.json`
  (mode 0600). A later `sessionhub attach` with no argument reuses it.
- A session prefix with a machine config: owner mode, machine token.
- Then `CreateAttach`, raw terminal (reuse `withRawTerminal` in
  `internal/cli/watchterm.go`), copy `AttachOutput` to stdout, read stdin in
  chunks and `SendAttachInput`. `Ctrl-]` detaches. Print the exit reason on
  the last line. Exit 0 on detach or session end, 1 on error.
- No resize in v1: the guest sees the host pane's size. Print a one-line note
  when the local terminal is smaller than 80x24.

## Limits

- Snapshot mirror on herdr: redraw per change, host size, host scrollback.
- One guest per attach; several attaches per session are fine.
- Guest credential is the web session (same TTL and `sessionhub login rm`).

## Acceptance

- [ ] Guest link → `sessionhub attach <url>` shows the live Claude pane and keys reach it.
- [ ] The same guest gets 403 on every other route (existing `guest_test.go` pattern extended to the attach routes).
- [ ] Owner `sessionhub attach <prefix>` works with the machine token.
- [ ] Ctrl-] detaches; the watcher posts `done`; the dashboard shows the control result.
- [ ] Watcher offline → `409` and a clear CLI message.
- [ ] `go test ./...` green; `docs/cli.md` and `docs/server.md` updated.
