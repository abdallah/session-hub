# Task 8 evidence: CLI and resume

Date: 2026-09-30, on `bluebox` with herdr 0.9.3. The scratch setup, removed at the
end: a temporary `sessionhub server` on `127.0.0.1:18899` (temp DB, machine `bluebox`
added with `sessionhub machine add`), a client config through `SESSIONHUB_SERVER_URL`,
`SESSIONHUB_TOKEN`, `SESSIONHUB_MACHINE=bluebox`, `SESSIONHUB_CONFIG=/nonexistent` (nothing written to
`~/.config/sessionhub/`), and a herdr workspace `sessionhub-task8-scratch` (`wK`) that this
run created.

## A real resumable session

`claude -p --model haiku "reply ok"` ran in a scratch directory `…/ev/proj`.
The session ID came from the file name in `~/.claude/projects/<escaped
path>/`: `8e77d36b-689d-4360-9cf8-8a9588b82b9a`. It was registered with curl
(`POST /v1/sessions`, cwd = the scratch dir, `herdr_pane: "w99:p99"`, a pane that does
not exist), given a report, and ended with an `ended` event.

## `sessionhub ls`, `sessionhub show`

```
$ sessionhub ls
no sessions
$ sessionhub ls --all
MACHINE  ID        TITLE     STATUS  AGE  REPORT
bluebox      8e77d36b  reply ok  ended   12s  done: replied ok
$ sessionhub show 8e77
id:        8e77d36b-689d-4360-9cf8-8a9588b82b9a
title:     reply ok
machine:   bluebox
status:    ended
agent:     claude
cwd:       …/ev/proj
herdr:     session=default workspace= pane=w99:p99      (empty workspace fixed afterwards)
started:   2026-09-30T11:32:49Z (12s ago)
ended:     2026-09-30T11:32:49Z
resume:    ssh -t bluebox.example.com sessionhub resume 8e77d36b-689d-4360-9cf8-8a9588b82b9a

reports (1, newest first)
  2026-09-30T11:32:49Z
    done        replied ok

events (2, newest first)
  2026-09-30T12:00:00Z  hooks    ended  {"reason":"other"}
  2026-09-30T11:32:49Z  hooks    registered
$ sessionhub resume zzzz
sessionhub: HTTP 404: not found: no session matches "zzzz"
$ sessionhub show 8e
sessionhub: HTTP 400: invalid: id prefix "8e" is shorter than 4 characters
```

## Local resume of the ended session

`sessionhub resume 8e77d3` ran inside the scratch workspace's pane `wK:p1` through
`herdr pane run` (herdr set `HERDR_PANE_ID=wK:p1` there). The recorded pane
`w99:p99` does not exist, so `sessionhub` split and started Claude:

```
Started claude --resume 8e77d36b-689d-4360-9cf8-8a9588b82b9a in new pane wK:p2.
```

`herdr pane get wK:p2` afterwards (herdr detected the agent and its session):

```
"agent":"claude","agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"8e77d36b-689d-4360-9cf8-8a9588b82b9a"},
"agent_status":"idle","cwd":"…/ev/proj","focused":true,"pane_id":"wK:p2","terminal_title":"✳ Claude Code"
```

## Focus path

The session's pane was updated to `wK:p2` with a plugin-style upsert, so
`sessionhub ls` showed it `live`. With `wK:p1` focused (`pane.focus` over the socket),
`sessionhub resume 8e77d3` in `wK:p1` printed:

```
Focused pane wK:p2, which still runs session 8e77d36b.
```

and `herdr pane get` showed `wK:p2` `focused: true`, `wK:p1` `focused: false`.

`sessionhub status` printed `health:  ok` and the live row. `printf '1\n' | sessionhub resume
--pick` listed `1  8e77d36b live    reply ok` and focused `wK:p2`.

## Remote recipe

With `SESSIONHUB_MACHINE=tower`, `sessionhub resume 8e77d3` printed (nothing was run):

```
Session 8e77d36b runs on bluebox, not on this machine. To reach it:

ssh -t bluebox.example.com sessionhub resume 8e77d36b-689d-4360-9cf8-8a9588b82b9a     # opens or focuses it in bluebox's herdr
herdr --remote bluebox.example.com               # attach to bluebox's herdr from here
```

No `herdr --machine` line appeared: `herdr machine list --json` on this machine
printed `[]`.

## Cleanup

`herdr workspace close wK` (closed `wK:p1` and `wK:p2`; `workspace list` no
longer has `sessionhub-task8-scratch`), the server was stopped, and the temp DB,
config, wrapper script, scratch directory, and the scratch Claude project
directory under `~/.claude/projects/` were deleted. No pane outside `wK` was
touched.
