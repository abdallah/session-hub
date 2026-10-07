# Tasks tab evidence

Run on 2026-10-07 against a scratch `sessionhub server` built from `70a484a`
(`127.0.0.1:8799`, scratch database, no `server.toml`), in headless
`/usr/bin/google-chrome`. The browser time zone was UTC.

## Setup

1. `sessionhub machine add tower --json` and `bluebox --json` against the
   scratch database (`SESSIONHUB_DB`, `SESSIONHUB_SERVER_CONFIG` set).
2. [`tasks-seed.mjs`](tasks-seed.mjs) writes yesterday's tasks through the
   HTTP API, moves their task events and reports back 24 hours with
   `sqlite3`, then writes today's. Result:
   - 2026-10-06: Todo — *Write the tasks docs*, *Reply to a teammate*; In progress —
     *Upgrade Postgres on staging*; Done — *Fix the flaky login test*,
     *Rotate the CI deploy token*.
   - 2026-10-07: Todo — *Write the tasks docs*; In progress — *Upgrade
     Postgres* (`done_proposed`), *Fix the flaky login test* (reopened),
     *Reply to a teammate*; Done — *Renew the wildcard certificate*.
   - Review: 2 proposals, 1 done proposal, 2 sessions with no task.
3. [`tasks-shots.mjs`](tasks-shots.mjs) signs the browser in with a one-time
   login link, takes the screenshots, then merges the duplicate proposal and
   makes a task from a session through the page. Tokens come from the
   environment and are not printed.

## Screenshots

| View | Desktop (1280 px) | Phone (390 px) |
|---|---|---|
| Today | [tasks-today-desktop.png](tasks-today-desktop.png) | [tasks-today-phone.png](tasks-today-phone.png) |
| Day before | [tasks-past-desktop.png](tasks-past-desktop.png) | [tasks-past-phone.png](tasks-past-phone.png) |
| Review strip | [tasks-review-desktop.png](tasks-review-desktop.png) | [tasks-review-phone.png](tasks-review-phone.png) |
| After Merge and Make task | [tasks-after-writes-desktop.png](tasks-after-writes-desktop.png) | — |

## Checks

- Today: three columns, a **Move** menu per card (open on *Upgrade Postgres*
  in the phone shot: **Confirm done**, **Not done**, **Drop**), done items
  collapsed under a count.
- Day before: *Reply to a teammate* shows in **Todo** and *Fix the flaky login
  test* in **Done**, though both are `in_progress` now. No **Move** menus
  (`menus: 0`); the page says the day is read-only.
- Tab badge: **Tasks (3)** (2 proposals + 1 done proposal); **Tasks (2)**
  after the merge.
- Merge: the duplicate *Write the tasks docs* proposal left the strip and its
  session link moved to the open task.
- **Make task**: *Answer the Hetzner invoice question* left the strip and
  appears in **Todo**, linked to its session.
- Below 700 px the columns stack.

## Tests

```
$ go test ./internal/server -run Dashboard
ok  	github.com/abdallah/session-hub/internal/server	2.410s
$ go vet ./...
$ go test ./...
```

Every package passes except `internal/join`
(`TestSnippetIsPrintedInFullAndMatchesDocs`), which fails on `50c9978`
without this change too.
