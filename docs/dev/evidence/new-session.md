# Evidence: start a new session on a machine

Feature: `POST /v1/machines/{name}/start`, `GET /v1/starts/{id}`, the
watcher's `start` claim, `sessionhub start`, and the dashboard's
**New session** panel. Schema version 10 (`start_requests`).

## Tests

- `internal/store/start_test.go`: refusals store nothing (offline watcher,
  unknown machine, bad dir or prompt), the per-machine cap, claim order and
  isolation between machines, finish only by the claiming machine and only
  once, expiry on read and on claim, cascade on `machine rm`, v9 to v10
  upgrade.
- `internal/server/start_test.go` and the auth matrix in `auth_test.go`:
  every credential against both new routes, including the cookie with and
  without `X-Hub-Action: start`.
- `internal/resume/newsession_test.go`: directory resolution (`~`, links
  that stay inside home, links and `..` that leave it, missing, a file,
  relative), and `StartNew` against the scripted herdr: workspace without
  focus in the resolved directory, `claude --remote-control` started once,
  link read from the banner, first prompt sent only when idle, a blocked
  agent leaves it unsent, nothing created outside home.
- `internal/plugin/start_test.go`: the controller runs a start claim and
  posts its result; the `remote_start` opt-out (config and env) never
  reaches herdr.
- `internal/cli/start_test.go`: following to done, default directories,
  usage errors, a refused create, a failed, expired, or timed-out request,
  a 404 while following.
- `TestDashboardStartStates` runs the page's start-state block under node.

## Live checks

Server and client from this branch in a temporary HOME, `tower` joined
locally, a fake watcher (curl) answering claims.

Dashboard, driven with Playwright at 400 px:

```
options [["tower","tower",false],["bluebox","bluebox (watcher offline)",true]] dirs ["/home/me/Code/api"]
bad dir: Use an absolute directory, or one starting with ~/.
after send: Waiting for the watcher on tower…
claim start ~/Code/api fix the flaky test
result 200
done: Started on tower.Open in Claude https://claude.ai/code/session_01NewNewNewNewNewNewNewNe
errors []
```

CLI, with the fake watcher failing the request:

```
$ sessionhub start tower --dir /etc -m hi
st_roa-9PDiUM1O9kEUXQZxZQ  starting a session on tower in /etc
waiting for the sessionhub watcher on tower
starting Claude on tower
failed: the directory is outside this machine's home directory
exit 1
$ sessionhub start nosuch
start: sessionhub: HTTP 404: not found: machine "nosuch"
exit 1
```

Not checked here: a real herdr and `claude` (this container has neither).
The herdr side runs on the captured fixtures only; check it on a machine
with herdr after deploying.
