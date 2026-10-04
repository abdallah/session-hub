# Task 4 evidence: herdr plugin

Run on `bluebox` against the live herdr 0.9.3 server (default session,
`~/.config/herdr/herdr.sock`), on 2026-09-30. Times in herdr and watcher lines
are UTC; server log lines are local time (UTC+3). Tokens are redacted.

## Start state

```
$ ls -la ~/.config/sessionhub/ ~/.local/bin/sessionhub ~/.local/state/sessionhub ~/.local/share/sessionhub
ls: cannot access '/home/me/.config/sessionhub/': No such file or directory
ls: cannot access '/home/me/.local/bin/sessionhub': No such file or directory
ls: cannot access '/home/me/.local/state/sessionhub': No such file or directory
ls: cannot access '/home/me/.local/share/sessionhub': No such file or directory
$ herdr --version
herdr 0.9.3
$ herdr plugin list --json --plugin sessionhub
{"id":"cli:plugin","result":{"plugins":[],"type":"plugin_list"}}
```

No client config, binary, state dir, or plugin existed, so cleanup removes all
of them.

## Setup

```
$ make install
install -m 0755 bin/sessionhub /home/me/.local/bin/sessionhub
$ SESSIONHUB_SERVER_CONFIG=<scratch>/ev/server.toml SESSIONHUB_DB=<scratch>/ev/sessionhub.db SESSIONHUB_READ_TOKEN=hub_r_<redacted> \
    SESSIONHUB_LISTEN=127.0.0.1:8788 SESSIONHUB_PUBLIC_URL=http://127.0.0.1:8788 ~/.local/bin/sessionhub server &
$ curl -sS http://127.0.0.1:8788/healthz
ok
$ SESSIONHUB_DB=<scratch>/ev/sessionhub.db ... sessionhub machine add bluebox --ssh-host bluebox.example.com --herdr-host bluebox.example.com --json
{"name":"bluebox","token":"hub_m_<redacted>","server_url":"http://127.0.0.1:8788"}
$ ls -la ~/.config/sessionhub/        # written from that output, mode 0600
-rw-------  1 abdallah abdallah  113 Sep 30 14:16 config.toml
server_url = "http://127.0.0.1:8788"
token = "hub_m_<redacted>"
machine = "bluebox"
```

## Install, and sessions appearing

```
$ sessionhub install-plugin; echo "exit=$?"
wrote /home/me/.local/share/sessionhub/herdr-plugin/herdr-plugin.toml
linked herdr plugin "sessionhub" from /home/me/.local/share/sessionhub/herdr-plugin
sessionhub plugin startup: sent 5 sessions for herdr session "default"
exit=0
$ herdr plugin list --json --plugin sessionhub     # selected fields
{'plugin_id': 'sessionhub', 'enabled': True, 'min_herdr_version': '0.9.3',
 'plugin_root': '/home/me/.local/share/sessionhub/herdr-plugin',
 'manifest_path': '/home/me/.local/share/sessionhub/herdr-plugin/herdr-plugin.toml', 'source': {'kind': 'local'}}
$ pgrep -af 'sessionhub plugin watch'; cat ~/.local/state/sessionhub/watcher.lock
41216 /home/me/.local/bin/sessionhub plugin watch
41216
```

herdr accepted the manifest (`min_herdr_version = "0.9.3"`) and reports
`plugin_root` as the directory we linked, which is what install compares to
stay idempotent. Running it again does not relink:

```
$ sessionhub install-plugin; echo "exit=$?"
herdr plugin "sessionhub" already linked from /home/me/.local/share/sessionhub/herdr-plugin
stopped the running watcher
sessionhub plugin startup: sent 5 sessions for herdr session "default"
exit=0
$ cat ~/.local/state/sessionhub/watcher.log
2026/09/30 11:16:39 watcher started: pid 41216, herdr session "default", socket /home/me/.config/herdr/herdr.sock
2026/09/30 11:16:48 watcher stopping: context canceled
2026/09/30 11:16:48 watcher started: pid 41476, herdr session "default", socket /home/me/.config/herdr/herdr.sock
$ curl -sS -H 'Authorization: Bearer hub_r_<redacted>' http://127.0.0.1:8788/v1/sessions   # id, machine, herdr ids, status, state, title, branch
39fdf11d bluebox default wG wG:p1 live done 'CMO read-only database access' develop
47ebb3a2 bluebox default wC wC:p1 live idle 'Systemsdev-17893 CI job images from ECR' main
4f344a57 bluebox default wD wD:p1 live idle 'Big Workers for GitLab WPML' develop
87d22816 bluebox default wF wF:p1 live idle 'MRs 391 and 392 review notes' develop
ed0904a1 bluebox default wA wA:p1 live done 'Alertra checks validation and synthetic probes' develop
```

The five Claude panes already running in the user's herdr appear, with
`title` from herdr's `terminal_title_stripped` and git info. Server log: two
`PUT /v1/machines/self/herdr-sessions 200` per install (startup, then the new
watcher's first heartbeat).

## A new Claude pane: registered, state, ended

A scratch workspace (`wH`, created with `--no-focus`) with one pane; nothing
else in herdr was touched.

```
$ herdr workspace create --cwd <worktree> --label sessionhub-task4-evidence --no-focus
... "root_pane":{"pane_id":"wH:p1", ...}, "workspace":{"workspace_id":"wH", ...}
$ herdr agent start hubev --kind claude --pane wH:p1 --timeout 60000
{"result":{"agent":{"agent":"claude","agent_session":{"agent":"claude","kind":"id","source":"herdr:claude",
 "value":"454d0580-1874-46c3-9a6d-27c2548131a2"},"agent_status":"idle", "pane_id":"wH:p1", ...,
 "terminal_title_stripped":"Claude Code","workspace_id":"wH"},"argv":["claude"],"type":"agent_started"}}
$ herdr plugin log list --plugin sessionhub      # event, status, exit, duration herdr measured
pane.agent_detected succeeded 0 17 ms ''
pane.agent_status_changed succeeded 0 12 ms ''
$ server log
14:17:23 POST /v1/sessions 201 9.736ms machine=bluebox
14:17:25 POST /v1/sessions/454d0580-1874-46c3-9a6d-27c2548131a2/events 200 33.584ms machine=bluebox
$ curl ... /v1/sessions/454d0580-1874-46c3-9a6d-27c2548131a2
{
    "id": "454d0580-1874-46c3-9a6d-27c2548131a2",
    "agent": "claude",
    "machine": "bluebox",
    "cwd": "<worktree>",
    "git_repo": "git@github.com:abdallah/session-hub.git",
    "git_branch": "worktree-agent-aa5bfe6cdcd5ce56f",
    "herdr_session": "default",
    "herdr_workspace": "wH",
    "herdr_pane": "wH:p1",
    "title": "Claude Code",
    "title_source": "herdr",
    "status": "live",
    "agent_state": "idle",
    "resume_command": "ssh -t bluebox.example.com sessionhub resume 454d0580-1874-46c3-9a6d-27c2548131a2",
    "events": [
        {"ts": "2026-09-30T11:17:23.542740995Z", "source": "plugin", "kind": "state_changed",
         "payload": {"agent_state": "idle", "agent_status": "idle", "pane_id": "wH:p1", "workspace_id": "wH",
                     "herdr_session": "default", "herdr_event": "pane_agent_status_changed"}},
        {"ts": "2026-09-30T11:17:23.220869762Z", "source": "plugin", "kind": "registered"}
    ]
}
```

The `agent_detected` event queued an upsert with no session ID; the watcher
resolved `wH:p1` to the Claude UUID from a snapshot and sent the full body
(cwd, git, title hint). The agent's state change (`unknown` → `idle`) arrived as
a real `pane.agent_status_changed` and is recorded with `ts` in full precision.
No prompt was sent to the agent.

Closing only that pane:

```
$ herdr pane get wH:p1     # confirm it is ours before closing
wH:p1 wH claude idle {'agent': 'claude', 'kind': 'id', 'source': 'herdr:claude', 'value': '454d0580-...'}
$ herdr pane close wH:p1
{"id":"cli:pane:close","result":{"type":"ok"}}
$ herdr workspace get wH
{"error":{"code":"workspace_not_found","message":"workspace wH not found"},...}   # closing its only pane closed it
$ server log
14:18:18 POST /v1/sessions/454d0580-1874-46c3-9a6d-27c2548131a2/events 200 6.842ms machine=bluebox
$ curl ... /v1/sessions/454d0580-...
{'id': '454d0580-...', 'status': 'ended', 'agent_state': 'idle', 'ended_at': '2026-09-30T11:18:18.332643171Z', 'herdr_pane': 'wH:p1'}
2026-09-30T11:18:17.823871283Z plugin pane_closed {"pane_id": "wH:p1", "workspace_id": "wH", "herdr_session": "default", "herdr_event": "pane_closed"}
$ herdr plugin log list --plugin sessionhub
pane.closed succeeded 0 14 ms ''
```

The session is `ended` about 0.5 s after the close, through the event path.

## Killed watcher and server down: nothing lost, sent once

```
$ kill -9 41476          # the watcher, mid-life: no clean shutdown
$ kill 40898             # the sessionhub server
$ curl -sS -m 2 http://127.0.0.1:8788/healthz
curl: (7) Failed to connect to 127.0.0.1 port 8788 after 0 ms: Could not connect to server
$ herdr workspace create ... --label sessionhub-task4-evidence-2 --no-focus
wJ wJ:p1
$ herdr agent start hubev2 --kind claude --pane wJ:p1 --timeout 60000
wJ:p1 idle 577bbf0a-6097-44c0-b26c-15bd3fd1ca85
$ pgrep -af 'sessionhub plugin watch'; cat ~/.local/state/sessionhub/watcher.lock
48861 /home/me/.local/bin/sessionhub plugin watch
48861
$ ~/.local/state/sessionhub/queue.jsonl     # op, pane, session, kind, queued_at
upsert wJ:p1   2026-09-30T11:19:07.039704083Z
event wJ:p1  state_changed 2026-09-30T11:19:10.356141621Z
$ tail ~/.local/state/sessionhub/watcher.log
11:19:07 watcher started: pid 48861, herdr session "default", socket /home/me/.config/herdr/herdr.sock
11:19:07 heartbeat: send failed: Put "http://127.0.0.1:8788/v1/machines/self/herdr-sessions": dial tcp 127.0.0.1:8788: connect: connection refused
11:19:09 send failed, keeping queue: upsert : Post "http://127.0.0.1:8788/v1/sessions": dial tcp 127.0.0.1:8788: connect: connection refused
```

The `agent_detected` event found `watcher.lock` free (the kernel released the
killed watcher's `flock`) and started a new watcher. With the server down, the
items stay queued and the watcher logs one line. (The blank in `upsert :` was
fixed afterward in 4863dc7; the line now reads `keeping queue: upsert: ...`.)

Server restarted on the same database:

```
$ server log
14:19:26 sessionhub server listening on 127.0.0.1:8788
14:19:27 POST /v1/sessions 201 9.995ms machine=bluebox
14:19:27 POST /v1/sessions/577bbf0a-6097-44c0-b26c-15bd3fd1ca85/events 200 5.873ms machine=bluebox
$ ls ~/.local/state/sessionhub
queue.lock  watcher.lock  watcher.log          # queue.jsonl is gone: drained
$ curl ... /v1/sessions/577bbf0a-...
{'id': '577bbf0a-...', 'status': 'live', 'agent_state': 'idle', 'herdr_pane': 'wJ:p1', 'title': 'Claude Code', 'git_branch': 'worktree-agent-aa5bfe6cdcd5ce56f'}
2026-09-30T11:19:27.618931846Z plugin registered null
2026-09-30T11:19:10.356141621Z plugin state_changed {"agent_state": "idle", ...}
$ herdr pane close wJ:p1
$ server log
14:19:43 POST /v1/sessions/577bbf0a-6097-44c0-b26c-15bd3fd1ca85/events 200 9.598ms machine=bluebox
$ curl ... /v1/sessions
577bbf0a wJ:p1 ended idle 2026-09-30T11:19:43.616900233Z
454d0580 wH:p1 ended idle 2026-09-30T11:18:18.332643171Z
(the five pre-existing sessions: live)
```

Within one 2 s tick of the server coming back, each queued item was sent
exactly once (one upsert, one event), and the queue file was removed.

## `sessionhub plugin event` latency

`docs/dev/evidence/task-4-latency.py` runs the binary at `$SESSIONHUB_BIN` (default: `sessionhub`
on `PATH`) with the captured
`testdata/herdr/events/pane.agent_status_changed-blocked.json` as
`HERDR_PLUGIN_EVENT_JSON`, in a temporary state dir that it removes at the
end, with `SESSIONHUB_CONFIG` and
`HERDR_SOCKET_PATH` pointing at files that don't exist (so nothing reaches the
live herdr or the server). "warm": the script holds `watcher.lock`, as a
running watcher does, so the call only appends. "cold": the lock is free, so the
call also spawns a watcher.

```
$ SESSIONHUB_BIN=~/.local/bin/sessionhub python3 docs/dev/evidence/task-4-latency.py testdata/herdr/events/pane.agent_status_changed-blocked.json
warm (watcher running): n=50 min=7.1ms median=8.1ms p95=9.3ms max=10.2ms
cold (spawns a watcher): n=20 min=8.5ms median=9.9ms p95=11.0ms max=11.4ms
queued lines: 70
```

herdr's own plugin log measured the live calls at 12 to 17 ms, including
herdr's process spawn. Both are well under 100 ms.

The numbers above are from the original run, when the script still had the
binary path and state dir hard-coded. The committed version takes `SESSIONHUB_BIN`
and uses a temporary dir. A rerun of it against `bin/sessionhub` gave a median of
7.8 ms warm and 10.4 ms cold.

## Uninstall and cleanup

```
$ sessionhub uninstall-plugin; echo "exit=$?"
unlinked herdr plugin "sessionhub"
removed /home/me/.local/share/sessionhub/herdr-plugin
stopped the watcher
exit=0
$ sessionhub uninstall-plugin; echo "second exit=$?"
second exit=0
$ herdr plugin list --json --plugin sessionhub
{"id":"cli:plugin","result":{"plugins":[],"type":"plugin_list"}}
$ tail -1 ~/.local/state/sessionhub/watcher.log
2026/09/30 11:19:54 watcher stopping: context canceled
$ kill <sessionhub server pid>
$ pgrep -af '/home/me/.local/bin/sessionhub'
(nothing)
$ rm -rf ~/.local/state/sessionhub ~/.config/sessionhub ~/.local/share/sessionhub ~/.local/bin/sessionhub <scratch>/ev/sessionhub.db* <scratch>/ev/latstate
$ ls -la ~/.local/state/sessionhub ~/.config/sessionhub ~/.local/share/sessionhub ~/.local/bin/sessionhub
ls: cannot access ...: No such file or directory   (all four)
$ ss -ltn | grep -c 8788
0
$ herdr workspace list      # labels: the user's five workspaces, no scratch ones
['Alertra and Probes', 'Move GitLab images to ECR', "Eduard's CI runners", 'Make content CI faster', 'Readonly user for Oleksander']
```

Claude Code state from the two scratch `claude` runs:

```
$ ls ~/.claude/projects | grep -c worktrees          # no project dir for the worktree
0
$ find ~/.claude -maxdepth 3 -name '*454d0580-1874-46c3-9a6d-27c2548131a2*' -o -name '*577bbf0a-6097-44c0-b26c-15bd3fd1ca85*'
/home/me/.claude/session-env/454d0580-1874-46c3-9a6d-27c2548131a2    (empty dir)
/home/me/.claude/session-env/577bbf0a-6097-44c0-b26c-15bd3fd1ca85    (empty dir)
$ rmdir ~/.claude/session-env/454d0580-... ~/.claude/session-env/577bbf0a-...
```

Neither run left a transcript under `~/.claude/projects/`. They got no prompt,
and Claude Code writes the transcript only after the first message. The only
files they left were the two empty `session-env` directories named by their
session IDs, which are now removed.

## Suite

```
$ make lint
test -z "$(gofmt -l .)"
go vet ./...
$ make test
ok  session-hub/cmd/sessionhub
ok  session-hub/internal/client
ok  session-hub/internal/gitinfo
ok  session-hub/internal/herdr
ok  session-hub/internal/mcp
ok  session-hub/internal/paths
ok  session-hub/internal/plugin
ok  session-hub/internal/server
ok  session-hub/internal/store
$ go test -count=1 -v ./... | grep -c -- '--- PASS'     # 210; '--- FAIL': 0
$ go test -count=1 -race ./internal/plugin ./internal/client   # ok, ok
```
