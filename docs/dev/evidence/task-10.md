# Task 10 evidence: deploy to tower, README, and end to end

Run on 2026-09-30 from `bluebox` (WSL2) against `tower` (Ubuntu 25.10), both on
herdr 0.9.3. Tokens never appear here: the read token went straight into
`tower:~/.config/sessionhub/server.toml`, and every command that used it read it from
that file. Titles of the user's own live sessions are replaced with
`<user session>`; the scratch sessions this task created keep theirs.

## Summary

| Check | Result |
|---|---|
| `make test lint` before deploy | pass, 13 packages `ok`, 0 failures, `gofmt -l` empty, `go vet` clean |
| `make deploy` (first run, no client config) | server active, `/healthz` `ok`, install-plugin skipped with a message |
| Server listens on `127.0.0.1:8787` and `10.200.0.1:8787` only | yes (`ss -ltnp`) |
| Bind failure exits non-zero, so `Restart=on-failure` applies | yes, exit 1 |
| `cloudflared-tunnel`'s network reaches `10.200.0.1:8787` | yes, `ok`; ufw needs no change |
| `sessionhub join` on `tower` (local shortcut) and `bluebox` (over ssh) with the real installers | enrolled; plugin, hooks, and MCP installed; health `FAILED` only because `sessionhub.example.com` doesn't resolve yet |
| Both machines' sessions in `sessionhub ls` | yes, 5 on `bluebox`, 2 on `tower` |
| `report_progress` from a real Claude session in `sessionhub show` | yes, on `bluebox` and on `tower`; herdr sidebar `hub_summary` set |
| Closed pane marked `ended` | yes, on `bluebox` and `tower`, with a plugin `pane_closed` event |
| `make deploy` restarts the watcher on the new binary | yes, watcher inode equals the installed binary's |
| Dashboard `?token=` → `303`; no cookie → `401`; write with read token → `403` | yes |
| `https://sessionhub.example.com/healthz` | **pending**: the hostname does not exist yet |
| A real `sessionhub` POST through Cloudflare | **pending** on the hostname |
| Printed remote resume commands over non-interactive ssh | command forms work over the LAN alias; `tower.example.com` and `bluebox.example.com` need Cloudflare Access login, **pending** |

## Suite before deploy

```
$ GOFLAGS=-count=1 make test lint
ok  	session-hub/cmd/sessionhub	0.012s
?   	session-hub/internal/api	[no test files]
ok  	session-hub/internal/cli	0.167s
ok  	session-hub/internal/cli/termtext	3.374s
ok  	session-hub/internal/client	3.155s
ok  	session-hub/internal/gitinfo	0.075s
ok  	session-hub/internal/herdr	2.148s
?   	session-hub/internal/herdr/herdrtest	[no test files]
ok  	session-hub/internal/hooks	7.168s
ok  	session-hub/internal/join	0.823s
ok  	session-hub/internal/mcp	0.742s
ok  	session-hub/internal/paths	0.004s
ok  	session-hub/internal/plugin	6.707s
ok  	session-hub/internal/resume	2.512s
ok  	session-hub/internal/server	8.547s
ok  	session-hub/internal/store	2.068s
test -z "$(gofmt -l .)"
go vet ./...
```

The same suite after the Task 10 commits, counted with
`go test -count=1 -json ./...`: 479 tests and subtests pass, 0 fail, and 1
skips (`internal/herdr` `TestLiveReadOnly`, which runs only against a live
socket on request). `make lint` prints nothing.

An earlier `-json` run, started right after two full suites, failed 4 to 9
tests at random. This machine has 300 ephemeral ports (`60700 61000`), and
`ss -tan state time-wait` counted 474 sockets. Once the count fell below 20,
the rerun above passed. This is the known port-range limit, not a code fault.

## Starting state

`tower` had no `~/.config/sessionhub/`, `~/.local/share/sessionhub/`, or
`~/.local/state/sessionhub/`, no `sessionhub.service`, lingering on (`Linger=yes`), and
`docker0` at `10.200.0.1/24`. `bluebox` had no sessionhub files, no sessionhub hook entries in
`~/.claude/settings.json`, and no `sessionhub` MCP server.

## Server config on tower

Written by one `ssh tower` command that generated the token into a shell
variable and printed the file with the token redacted:

```
-rw-------  1 abdallah abdallah  166 Sep 30 13:21 server.toml
listen = ["127.0.0.1:8787", "10.200.0.1:8787"]
public_url = "https://sessionhub.example.com"
read_token = <redacted>
stale_after = "5m"
$ grep -c '^read_token = "hub_r_[A-Za-z0-9_-]\{43\}"$' ~/.config/sessionhub/server.toml
1
```

## First deploy

```
$ make deploy
GOOS=linux GOARCH=amd64 make build
CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=7156f36-dirty" -o bin/sessionhub ./cmd/sessionhub
ssh tower 'mkdir -p ~/.local/bin ~/.config/systemd/user'
scp bin/sessionhub tower:.local/bin/sessionhub.new
scp deploy/sessionhub.service tower:.config/systemd/user/sessionhub.service
ssh tower 'chmod 0755 ~/.local/bin/sessionhub.new && mv -f ~/.local/bin/sessionhub.new ~/.local/bin/sessionhub'
ssh tower 'systemctl --user daemon-reload && systemctl --user enable --now sessionhub && systemctl --user restart sessionhub'
Created symlink '/home/me/.config/systemd/user/default.target.wants/sessionhub.service' → '/home/me/.config/systemd/user/sessionhub.service'.
ssh tower 'for i in 1 2 3 4 5; do curl -fsS http://127.0.0.1:8787/healthz ...'
ok
ssh tower 'if [ -f ~/.config/sessionhub/config.toml ]; then ... sessionhub install-plugin; else echo ...; fi'
no client config yet: skipping install-plugin (run sessionhub join)
ssh tower '~/.local/bin/sessionhub version'
sessionhub 7156f36-dirty
```

The version says `-dirty` because the unit and Makefile were not committed
yet; the code is `7156f36`.

```
$ ssh tower 'systemctl --user status sessionhub; ss -ltnp | grep 8787; journalctl --user -u sessionhub -n 3'
● sessionhub.service - sessionhub session registry server
     Loaded: loaded (/home/me/.config/systemd/user/sessionhub.service; enabled; preset: enabled)
     Active: active (running) since Wed 2026-09-30 13:21:53 UTC; 14s ago
   Main PID: 3008086 (sessionhub)
LISTEN 0      4096       10.200.0.1:8787       0.0.0.0:*    users:(("sessionhub",pid=3008086,fd=9))
LISTEN 0      4096        127.0.0.1:8787       0.0.0.0:*    users:(("sessionhub",pid=3008086,fd=8))
sessionhub[3008086]: 2026/09/30 13:21:53 sessionhub server listening on 127.0.0.1:8787 (db /home/me/.local/share/sessionhub/sessionhub.db, stale_after 5m0s)
sessionhub[3008086]: 2026/09/30 13:21:53 sessionhub server listening on 10.200.0.1:8787 (db /home/me/.local/share/sessionhub/sessionhub.db, stale_after 5m0s)
sessionhub[3008086]: 2026/09/30 13:21:54 GET "/healthz" 200 14µs machine=-
```

### Boot order: a missing docker0 fails the start

`bluebox` has no `10.200.0.1`, which is the state `tower` is in at boot before
Docker creates `docker0`:

```
$ SESSIONHUB_SERVER_CONFIG=<scratch>/none.toml SESSIONHUB_DB=<scratch>/sessionhub.db SESSIONHUB_READ_TOKEN=hub_r_test \
  SESSIONHUB_LISTEN=127.0.0.1:18899,10.200.0.1:8787 ~/.local/bin/sessionhub server; echo "exit=$?"
server: listen 10.200.0.1:8787: listen tcp 10.200.0.1:8787: bind: cannot assign requested address
exit=1
```

Exit 1 is a failure for `Restart=on-failure`, and with `RestartSec=5s` and
`StartLimitIntervalSec=0` systemd retries every 5 seconds until the bind
succeeds. Docker was not stopped to test this on `tower`.

## Tunnel reachability

```
$ ssh tower 'docker run --rm --network bridge curlimages/curl -sS -m 5 http://10.200.0.1:8787/healthz; echo "exit=$?"; docker inspect -f "{{.HostConfig.NetworkMode}}" cloudflared-tunnel'
ok
exit=0
bridge
```

A container on the same network as `cloudflared-tunnel` reaches the server, so
ufw needs no rule.

```
$ curl -sS -m 10 https://sessionhub.example.com/healthz
curl: (6) Could not resolve host: sessionhub.example.com
```

The public hostname doesn't exist yet. The step left for the user is in the
README ("Install the server on tower", step 5).

## Backups before the real installers

| Machine | Backup |
|---|---|
| `tower` | `/home/me/.claude/settings.json.pre-sessionhub-20260930T132224Z` |
| `tower` | `/home/me/.claude.json.pre-sessionhub-20260930T132224Z` |
| `tower` | `/home/me/.claude/settings.json.sessionhub-backup-20260930T132246` (written by `install-hooks`) |
| `bluebox` | `/home/me/.claude/settings.json.pre-sessionhub-20260930T132332Z` |
| `bluebox` | `/home/me/.claude.json.pre-sessionhub-20260930T132332Z` |
| `bluebox` | `/home/me/.claude/settings.json.sessionhub-backup-20260930T162341` (written by `install-hooks`, local time) |

## Join tower

Run through a login shell, because non-interactive ssh on `tower` has no
`~/.local/bin` in `PATH`, where `herdr` and `claude` live:

```
$ ssh tower 'exec $SHELL -lc "~/.local/bin/sessionhub join tower --name tower --ssh-host tower.example.com --herdr-host tower.example.com"'
Server config and database found here: adding machine "tower" locally.
The ssh target "tower" is ignored.
Wrote /home/me/.config/sessionhub/config.toml (mode 0600).
Installing the herdr plugin.
wrote /home/me/.local/share/sessionhub/herdr-plugin/herdr-plugin.toml
linked herdr plugin "sessionhub" from /home/me/.local/share/sessionhub/herdr-plugin
Installing the Claude Code hooks.
Installed sessionhub hooks in /home/me/.claude/settings.json (backup: /home/me/.claude/settings.json.sessionhub-backup-20260930T132246)
Registering the MCP server with Claude Code.
sessionhub plugin startup: send 2 sessions: Put "https://sessionhub.example.com/v1/machines/self/herdr-sessions": dial tcp: lookup sessionhub.example.com on 127.0.0.53:53: no such host (queued)
Registered the sessionhub MCP server: /home/me/.local/bin/sessionhub mcp
FAILED  health: Get "https://sessionhub.example.com/healthz": dial tcp: lookup sessionhub.example.com on 127.0.0.53:53: no such host
...
exit=1
join: enrolled tower, but these steps failed: health
```

## Join bluebox

```
$ make install
$ sessionhub join tower --name bluebox --ssh-host bluebox.example.com --herdr-host bluebox.example.com
Adding machine "bluebox" on tower over ssh.
Wrote /home/me/.config/sessionhub/config.toml (mode 0600).
Installing the herdr plugin.
wrote /home/me/.local/share/sessionhub/herdr-plugin/herdr-plugin.toml
linked herdr plugin "sessionhub" from /home/me/.local/share/sessionhub/herdr-plugin
sessionhub plugin startup: send 5 sessions: Put "https://sessionhub.example.com/v1/machines/self/herdr-sessions": dial tcp: lookup sessionhub.example.com on 127.0.0.42:53: no such host (queued)
Installing the Claude Code hooks.
Installed sessionhub hooks in /home/me/.claude/settings.json (backup: /home/me/.claude/settings.json.sessionhub-backup-20260930T162341)
Registering the MCP server with Claude Code.
Registered the sessionhub MCP server: /home/me/.local/bin/sessionhub mcp
FAILED  health: Get "https://sessionhub.example.com/healthz": dial tcp: lookup sessionhub.example.com on 127.0.0.42:53: no such host
join: enrolled bluebox, but these steps failed: health
```

`bluebox`'s herdr is 0.9.3, so the plugin was installed, not skipped.

```
$ ssh tower '~/.local/bin/sessionhub machine ls'
NAME  SSH_HOST     HERDR_HOST   LAST_SEEN
bluebox   bluebox.example.com  bluebox.example.com  2026-09-30T13:31:59Z
tower  tower.example.com  tower.example.com  2026-09-30T13:32:00Z
```

## End to end without the public hostname

Both clients were enrolled with `server_url = "https://sessionhub.example.com"`, which
doesn't resolve yet, so their writes queued. To run the system end to end
now, the `server_url` line was pointed at a working address for the test and
set back to `https://sessionhub.example.com` afterwards:

- `tower`: `http://127.0.0.1:8787`.
- `bluebox`: `http://127.0.0.1:18787`, forwarded to `tower:127.0.0.1:8787` by
  `ssh -N -L 127.0.0.1:18787:127.0.0.1:8787 tower`.

The watcher loads the config on every send, so the queued startup snapshots
drained within one tick without a restart:

```
$ ssh tower 'wc -l ~/.local/state/sessionhub/queue.jsonl'     # before
1 /home/me/.local/state/sessionhub/queue.jsonl
$ ssh tower 'wc -l ~/.local/state/sessionhub/queue.jsonl'     # 5 s later
wc: /home/me/.local/state/sessionhub/queue.jsonl: No such file or directory
```

### Both machines in `sessionhub ls`

```
$ sessionhub ls --all        # on bluebox
MACHINE  ID        TITLE                                               STATUS   AGE  REPORT
bluebox      39fdf11d  <user session>                                      live     4s   -
bluebox      47ebb3a2  <user session>                                      live     4s   -
bluebox      4f344a57  <user session>                                      live     4s   -
bluebox      87d22816  <user session>                                      live     4s   -
bluebox      ed0904a1  <user session>                                      live     4s   -
tower     a95eafb5  <user session>                                      blocked  9s   -
tower     600a66d0  <user session>                                      live     21s  -
tower     38cbca4f  <user session>                                      ended    27s  -
```

The five `bluebox` rows and the two live `tower` rows are the user's herdr panes,
registered by each machine's plugin startup snapshot. `38cbca4f` is a
hooks-registered session on `tower` that ended on its own.

### `report_progress` from a real session (bluebox)

A scratch workspace (`--no-focus`) with a real interactive Claude Code, started
by herdr:

```
$ herdr workspace create --cwd /home/me/Code/Personal/session-hub --label sessionhub-e2e-scratch --no-focus
... "workspace_id":"wN" ... "pane_id":"wN:p1" ...
$ herdr agent start hube2e --kind claude --pane wN:p1 -- --allowedTools mcp__hub__report_progress mcp__hub__set_title
... "agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"c1dc2630-46c9-4cb7-bc7e-b3ab2a3018bf"},"agent_status":"idle" ...
$ herdr agent prompt hube2e "This is an end-to-end test of the sessionhub MCP server. Call the sessionhub report_progress tool exactly once with done=[\"sessionhub e2e: report from a real session\"], in_flight=[\"verifying sessionhub show\"], waiting_on=[], note=\"task 10 deploy check\". Then call set_title with title \"sessionhub e2e scratch\". ..." --wait --timeout 180000
... "agent_status":"done", ... "terminal_title_stripped":"Hub e2e scratch","tokens":{"hub_summary":"verifying sessionhub show"} ...
```

The pane's `tokens.hub_summary` is the MCP server's `pane.report_metadata`,
which the herdr sidebar row shows.

```
$ sessionhub show c1dc2630
id:        c1dc2630-46c9-4cb7-bc7e-b3ab2a3018bf
title:     sessionhub e2e scratch
machine:   bluebox
status:    live
agent:     claude
state:     done
cwd:       /home/me/Code/Personal/session-hub
branch:    sessionhub-v1
herdr:     session=default workspace=wN pane=wN:p1
started:   2026-09-30T13:24:31Z (1m ago)
last seen: 2026-09-30T13:25:24Z (9s ago)
resume:    ssh -t bluebox.example.com '~/.local/bin/sessionhub resume c1dc2630-46c9-4cb7-bc7e-b3ab2a3018bf'

reports (1, newest first)
  2026-09-30T13:25:08Z
    done        sessionhub e2e: report from a real session
    in flight   verifying sessionhub show
    note        task 10 deploy check

events (7, newest first)
  2026-09-30T13:25:24Z  plugin   state_changed  {"agent_state":"done","agent_status":"done","pane_id":"wN:p1","workspace_id":"wN","herdr_session":"…
  2026-09-30T13:25:23Z  hooks    state_changed  {"agent_state":"idle"}
  2026-09-30T13:24:52Z  plugin   state_changed  {"agent_state":"working","agent_status":"working","pane_id":"wN:p1","workspace_id":"wN","herdr_sess…
  2026-09-30T13:24:51Z  hooks    state_changed  {"agent_state":"working"}
  2026-09-30T13:24:51Z  hooks    prompt  {"prompt":"This is an end-to-end test of the sessionhub MCP server. Call the sessionhub report_progress tool exac…
  2026-09-30T13:24:34Z  plugin   state_changed  {"agent_state":"idle","agent_status":"idle","pane_id":"wN:p1","workspace_id":"wN","herdr_session":"…
  2026-09-30T13:24:31Z  hooks    registered
```

All three clients wrote to one session: hooks (`registered`, `prompt`,
state), the plugin (state from herdr), and the MCP server (report and title,
`title` source `user`).

`herdr workspace close wN` then marked it `ended` through the hooks'
`SessionEnd` (`hooks ended {"reason":"other"}`). herdr 0.9.3 fired no
`pane.closed` for the pane when the whole workspace closed: `herdr plugin log
list --plugin sessionhub` shows no new entry after the close. So the next check closes
a single pane.

### A closed pane is marked `ended` (bluebox)

```
$ herdr workspace create --cwd /home/me/Code/Personal/session-hub --label sessionhub-e2e-scratch2 --no-focus
"wP:p1"
$ herdr pane split wP:p1 --direction right --cwd /home/me/Code/Personal/session-hub --no-focus
"wP:p2"
$ herdr agent start hube2e2 --kind claude --pane wP:p2
"8c91984a-ca79-4f47-9497-371dfc73f6dd" "idle"
$ sessionhub show 8c91984a        # before
status:    live
$ herdr pane close wP:p2
{"id":"cli:pane:close","result":{"type":"ok"}}
$ sessionhub show 8c91984a        # 6 s later
id:        8c91984a-ca79-4f47-9497-371dfc73f6dd
status:    ended
ended:     2026-09-30T13:26:45Z
events (4, newest first)
  2026-09-30T13:26:45Z  plugin   pane_closed  {"pane_id":"wP:p2","workspace_id":"wP","herdr_session":"default","herdr_event":"pane_closed"}
  2026-09-30T13:26:45Z  hooks    ended  {"reason":"other"}
  2026-09-30T13:26:34Z  plugin   state_changed  {"agent_state":"idle",...}
  2026-09-30T13:26:32Z  hooks    registered
$ herdr plugin log list --plugin sessionhub     # last three
plugin-log-211 pane.agent_detected succeeded
plugin-log-213 pane.agent_status_changed succeeded
plugin-log-217 pane.closed succeeded
$ herdr workspace close wP
```

### The same on tower

The first `agent start` timed out: the new zsh pane showed a `compinit`
"insecure directories" prompt, which swallowed the first character of the
`claude` command. The retry in the same, now idle, shell succeeded.

```
$ herdr workspace create --cwd <trusted scratch project> --label sessionhub-e2e-scratch --no-focus   # wB
$ herdr pane split wB:p1 --direction right --no-focus                                          # wB:p2
$ herdr agent start hubtower --kind claude --pane wB:p2 -- --allowedTools mcp__hub__report_progress
["d513ba2c-1332-4175-ad6b-1f9cac4d2153","idle",null]
$ herdr agent prompt hubtower "... report_progress ... done=[\"tower e2e report\"], in_flight=[], waiting_on=[\"task 10 evidence\"], note=\"tower check\" ..." --wait
["done",{"hub_summary":"waiting: task 10 evidence"},null]
$ sessionhub show d513ba2c
machine:   tower
status:    live
resume:    ssh -t tower.example.com '~/.local/bin/sessionhub resume d513ba2c-1332-4175-ad6b-1f9cac4d2153'
reports (1, newest first)
  2026-09-30T13:28:23Z
    done        tower e2e report
    waiting on  task 10 evidence
    note        tower check
$ herdr pane close wB:p2; sleep 6; sessionhub show d513ba2c
status:    ended
ended:     2026-09-30T13:28:42Z
events (9, newest first)
  2026-09-30T13:28:41Z  plugin   pane_closed  {"pane_id":"wB:p2","workspace_id":"wB","herdr_session":"default","herdr_event":"pane_closed"}
  2026-09-30T13:28:41Z  hooks    ended  {"reason":"other"}
  ...
$ herdr workspace close wB
```

### Server log

Requests by endpoint, status, and caller, from `journalctl --user -u sessionhub`
(`/healthz` left out):

```
     13 PUT "/v1/machines/self/herdr-sessions" 200 machine=tower
     11 PUT "/v1/machines/self/herdr-sessions" 200 machine=bluebox
      8 POST "/v1/sessions/d513ba2c-.../events" 200 machine=tower
      7 POST "/v1/sessions/c1dc2630-.../events" 200 machine=bluebox
      5 POST "/v1/sessions" 201 machine=bluebox
      3 POST "/v1/sessions" 201 machine=tower
      ...
```

Every non-2xx line, with the reason:

```
POST "/v1/sessions/38cbca4f-.../events" 404 machine=tower   # session started before sessionhub: client upserts and retries
POST "/v1/sessions/7307be54-.../events" 404 machine=tower   # same
POST "/v1/sessions/d3e23c02-.../events" 404 machine=bluebox    # same
GET "/" 401 machine=-                                      # deliberate: dashboard without a cookie
POST "/v1/sessions" 403 machine=read                       # deliberate: write with the read token
POST "/v1/sessions/effcf0ee-.../events" 404 machine=bluebox    # session started before sessionhub
```

### Dashboard and read-token auth

On `tower`, with the token read from `server.toml` into a shell variable:

```
GET /?token=<read token> -> 303 location=http://127.0.0.1:8787/
GET / no token -> 401
GET /v1/machines (read token) -> [{"name":"bluebox","ssh_host":"bluebox.example.com","herdr_host":"bluebox.example.com"},{"name":"tower","ssh_host":"tower.example.com","herdr_host":"tower.example.com"}]
POST with read token -> 403
```

## Upgrade restarts the watcher on the new binary

`make deploy` again, now that `tower` has a client config:

```
$ ssh tower 'p=$(cat ~/.local/state/sessionhub/watcher.lock); echo "before: pid=$p"'
before: pid=3027475 exe=/home/me/.local/bin/sessionhub
$ make deploy
...
ok
ssh tower 'if [ -f ~/.config/sessionhub/config.toml ]; then PATH=$HOME/.local/bin:$PATH ~/.local/bin/sessionhub install-plugin; ...'
herdr plugin "sessionhub" already linked from /home/me/.local/share/sessionhub/herdr-plugin
stopped the running watcher
sessionhub plugin startup: sent 2 sessions for herdr session "default"
sessionhub 7156f36-dirty
$ ssh tower 'p=$(cat ~/.local/state/sessionhub/watcher.lock); ...'
watcher pid=3199957 exe_inode=46177950 installed_inode=46177950
server pid=3199280 exe_inode=46177950
$ ssh tower 'tail -2 ~/.local/state/sessionhub/watcher.log'
2026/09/30 13:31:13 watcher stopping: context canceled
2026/09/30 13:31:13 watcher started: pid 3199957, herdr session "default", socket /home/me/.config/herdr/herdr.sock
```

The watcher and the server both run the inode now at `~/.local/bin/sessionhub`.

## Remote resume commands over non-interactive ssh

The printed forms work on `tower` over the LAN alias, which reaches the same
sshd and gives the same non-interactive shell:

```
$ ssh -o BatchMode=yes tower '~/.local/bin/sessionhub version'; echo "exit=$?"
sessionhub 7156f36-dirty
exit=0
$ ssh -o BatchMode=yes tower 'exec $SHELL -lc "command -v claude"'; echo "exit=$?"
/home/me/.local/bin/claude
exit=0
$ ssh -o BatchMode=yes tower 'command -v claude; echo "plain-exit=$?"'
plain-exit=1
```

The last line is why the no-herdr form runs `claude` through `$SHELL -lc`.

The public SSH names don't work non-interactively yet. `tower.example.com` and
`bluebox.example.com` resolve to Cloudflare, which carries SSH only through
`cloudflared access ssh` with an Access login:

```
$ ssh -o BatchMode=yes tower.example.com '~/.local/bin/sessionhub version'
(hangs: bluebox has no ProxyCommand for tower.example.com; stopped after 60 s)
$ ssh -o BatchMode=yes -o ProxyCommand='cloudflared access ssh --hostname %h' tower.example.com '~/.local/bin/sessionhub version'
https://tower.example.com/cdn-cgi/access/cli?...     (asks for a browser login)
Connection timed out during banner exchange
exit=255
$ ssh tower 'ssh -o BatchMode=yes bluebox.example.com "~/.local/bin/sessionhub version"'
zsh:1: command not found: cloudflared                (tower's ProxyCommand needs cloudflared on PATH)
Connection closed by UNKNOWN port 65535
```

No session was resumed remotely. The README's "Resume across machines" section
gives the `~/.ssh/config` entry; the Access login is the user's step.

## Final client config

After the checks, both machines' `server_url` went back to
`https://sessionhub.example.com`, and the ssh forward was closed. Until the Cloudflare
hostname exists, both clients queue their writes and the sessions go `stale`
after 5 minutes; they come back on the first heartbeat after it exists.
