# Task 1 evidence

Server, store, and machine admin. Everything below is real output from the
binary built at commit `c1f01c8`, run on `bluebox` (WSL2) on 2026-09-30.

Toolchain: `go version go1.25.6 linux/amd64`.

## `make build`, `make test`, `make lint`

```
$ make build
CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=c1f01c8" -o bin/sessionhub ./cmd/sessionhub
[exit 0]

$ make test
go test ./...
ok  	session-hub/cmd/sessionhub	0.006s
?   	session-hub/internal/api	[no test files]
?   	session-hub/internal/cli	[no test files]
?   	session-hub/internal/hooks	[no test files]
?   	session-hub/internal/join	[no test files]
?   	session-hub/internal/mcp	[no test files]
ok  	session-hub/internal/paths	(cached)
?   	session-hub/internal/plugin	[no test files]
?   	session-hub/internal/resume	[no test files]
ok  	session-hub/internal/server	2.194s
ok  	session-hub/internal/store	(cached)
[exit 0]

$ make lint
test -z "$(gofmt -l .)"
go vet ./...
[exit 0]
```

Test counts from `go test -count=1 -v ./...`: 99 tests and subtests run,
99 pass, 0 fail. The auth matrix test logs:

```
auth_test.go:107: auth matrix: 10 routes x 4 tokens, 0 failures
```

`CGO_ENABLED=1 go test -race ./internal/...` also passes.

## End-to-end walk with `curl`

The script below starts `sessionhub server` on a temp database and port 18787 with
`SESSIONHUB_STALE_AFTER=15s`, adds two machines, and walks register, event, report,
title, list, detail, reconcile, bad tokens, the wrong machine, an oversized
body, staleness, token rotation, `machine rm`, and shutdown on `SIGTERM`.

Each block is a command (`$ ...`) and its output. `req` is `curl -sS` that
prints the HTTP status after the body; `api` is plain `curl -sS`, used when
piping to `jq`. `$TOWER`, `$BLUEBOX`, and `$READ` hold the tokens printed by
`sessionhub machine add` and the read token from `server.toml`. The only edit to the
output is that the session scratch directory path is shortened to `$SCRATCH`.
The tokens belong to a throwaway database that no longer exists.

<details><summary>The script</summary>

```bash
#!/usr/bin/env bash
# Walks the sessionhub API with the real binary. Prints each command and its output.
set -u
HUB=${HUB:?path to bin/sessionhub}
T=$(mktemp -d $SCRATCH/sessionhub-e2e.XXXX)
export SESSIONHUB_DB=$T/data/sessionhub.db SESSIONHUB_SERVER_CONFIG=$T/server.toml
unset SESSIONHUB_LISTEN SESSIONHUB_PUBLIC_URL SESSIONHUB_READ_TOKEN SESSIONHUB_STALE_AFTER
URL=http://127.0.0.1:18787

show() { printf '$ %s\n' "$*"; eval "$@"; printf '[exit %d]\n\n' $?; }
section() { printf '## %s\n\n' "$*"; }
# curl printing the status line after the body.
req() { curl -sS -w '\nHTTP %{http_code}\n' "$@"; }
api() { curl -sS "$@"; }

section "Refuses to start without a read_token"
show "$HUB server"

section "Config"
READ=hub_r_$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')
cat >"$SESSIONHUB_SERVER_CONFIG" <<EOF
listen = ["127.0.0.1:18787"]
public_url = "https://sessionhub.example.test"
read_token = "$READ"
EOF
chmod 600 "$SESSIONHUB_SERVER_CONFIG"
show "cat \$SESSIONHUB_SERVER_CONFIG"

section "Add machines"
show "$HUB machine add tower --ssh-host tower.example.com --herdr-host tower.example.com | tee $T/tower.txt"
TOWER=$(sed -n 's/^token: //p' "$T/tower.txt")
show "$HUB machine add bluebox --json | tee $T/bluebox.json"
BLUEBOX=$(jq -r .token "$T/bluebox.json")
show "$HUB machine ls"

section "Start the server (SESSIONHUB_STALE_AFTER=15s)"
SESSIONHUB_STALE_AFTER=15s "$HUB" server 2>"$T/server.log" &
PID=$!
for _ in $(seq 50); do curl -s "$URL/healthz" >/dev/null && break; sleep 0.1; done
show "curl -sS $URL/healthz"

A=$(uuidgen); B=$(uuidgen); C=$(uuidgen); H=$(uuidgen)
echo "Session IDs: A=$A B=$B C=$C (all herdr panes on tower), H=$H (hooks-only on tower)"
echo

section "Register"
show "req -X POST $URL/v1/sessions -H \"Authorization: Bearer \$TOWER\" -d '{\"id\":\"$A\",\"agent\":\"claude\",\"source\":\"plugin\",\"cwd\":\"/home/user/session-hub\",\"git_repo\":\"session-hub\",\"git_branch\":\"sessionhub-v1\",\"herdr_session\":\"default\",\"herdr_workspace\":\"w1\",\"herdr_pane\":\"p1\",\"agent_state\":\"working\",\"first_prompt\":\"build the sessionhub server\"}'"
show "api -X POST $URL/v1/sessions -H \"Authorization: Bearer \$TOWER\" -d '{\"id\":\"$B\",\"agent\":\"claude\",\"source\":\"plugin\",\"herdr_session\":\"default\",\"herdr_pane\":\"p2\",\"title_hint\":\"Fix login redirect\"}' | jq -c '{id,title,title_source,status}'"
show "api -X POST $URL/v1/sessions -H \"Authorization: Bearer \$TOWER\" -d '{\"id\":\"$C\",\"agent\":\"claude\",\"source\":\"plugin\",\"herdr_session\":\"work\",\"herdr_pane\":\"p9\"}' | jq -c '{id,herdr_session,status}'"
show "api -X POST $URL/v1/sessions -H \"Authorization: Bearer \$TOWER\" -d '{\"id\":\"$H\",\"agent\":\"claude\",\"source\":\"hooks\",\"cwd\":\"/home/user/other\"}' | jq -c '{id,herdr_pane,status}'"

section "Event: state_changed to blocked"
show "api -X POST $URL/v1/sessions/$A/events -H \"Authorization: Bearer \$TOWER\" -d '{\"kind\":\"state_changed\",\"source\":\"plugin\",\"ts\":\"$(date -u +%FT%T.%NZ)\",\"payload\":{\"agent_state\":\"blocked\"}}' | jq -c '{status,agent_state}'"

section "Report"
show "api -X POST $URL/v1/sessions/$A/report -H \"Authorization: Bearer \$TOWER\" -d '{\"done\":[\"store and schema\"],\"in_flight\":[\"HTTP handlers\"],\"waiting_on\":[\"review\"],\"note\":\"tests green\"}' | jq -c .latest_report"

section "Title"
show "api -X POST $URL/v1/sessions/$A/title -H \"Authorization: Bearer \$TOWER\" -d '{\"title\":\"sessionhub server, task 1\"}' | jq -c '{title,title_source}'"
echo "A later herdr title hint does not replace the user title:"
show "api -X POST $URL/v1/sessions -H \"Authorization: Bearer \$TOWER\" -d '{\"id\":\"$A\",\"source\":\"plugin\",\"title_hint\":\"Claude Code\"}' | jq -c '{title,title_source}'"

section "List (read token)"
show "api $URL/v1/sessions -H \"Authorization: Bearer \$READ\" | jq -c '.[] | {id,machine,status,agent_state,title,resume_command}'"
show "api '$URL/v1/sessions?live=true&machine=tower' -H \"Authorization: Bearer \$READ\" | jq -c '[.[].id]'"

section "Detail by prefix"
show "api $URL/v1/sessions/${A:0:8} -H \"Authorization: Bearer \$BLUEBOX\" | jq ."
show "req $URL/v1/sessions/${A:0:3} -H \"Authorization: Bearer \$READ\""

section "Reconcile: herdr snapshot lists only A"
show "req -X PUT $URL/v1/machines/self/herdr-sessions -H \"Authorization: Bearer \$TOWER\" -d '{\"herdr_session\":\"default\",\"sessions\":[{\"id\":\"$A\",\"agent\":\"claude\",\"herdr_pane\":\"p1\"}]}'"
show "api $URL/v1/sessions -H \"Authorization: Bearer \$READ\" | jq -c '.[] | {id,herdr_session,herdr_pane,status}'"
show "api $URL/v1/sessions/$B -H \"Authorization: Bearer \$READ\" | jq -c '.events[0]'"

section "Bad tokens and wrong machine"
show "req $URL/v1/sessions"
show "req $URL/v1/sessions -H 'Authorization: Bearer hub_m_forged'"
show "req -X POST $URL/v1/sessions/$A/title -H \"Authorization: Bearer \$READ\" -d '{\"title\":\"x\"}'"
show "req -X POST $URL/v1/sessions/$A/title -H \"Authorization: Bearer \$BLUEBOX\" -d '{\"title\":\"x\"}'"
show "head -c 70000 /dev/zero | tr '\\0' a | req -X POST $URL/v1/sessions -H \"Authorization: Bearer \$TOWER\" --data-binary @-"

section "Stale after SESSIONHUB_STALE_AFTER (15s)"
show "sleep 20"
show "api $URL/v1/sessions -H \"Authorization: Bearer \$READ\" | jq -c '.[] | {id,status}'"
show "req '$URL/v1/sessions?live=true' -H \"Authorization: Bearer \$READ\""
echo "The heartbeat (snapshot) brings A back:"
show "req -X PUT $URL/v1/machines/self/herdr-sessions -H \"Authorization: Bearer \$TOWER\" -d '{\"herdr_session\":\"default\",\"sessions\":[{\"id\":\"$A\",\"herdr_pane\":\"p1\"}]}'"
show "api '$URL/v1/sessions?live=true' -H \"Authorization: Bearer \$READ\" | jq -c '.[] | {id,status}'"

section "Machines"
show "api $URL/v1/machines -H \"Authorization: Bearer \$READ\" | jq -c '.[]'"
show "$HUB machine rm bluebox"
show "req $URL/v1/sessions -H \"Authorization: Bearer \$BLUEBOX\""
echo "Rotating tower while the server runs: the old token stops working at once."
show "$HUB machine add tower --json > \$T/tower2.json; jq -c '{name,server_url}' \$T/tower2.json"
TOWER2=$(jq -r .token "$T/tower2.json")
show "req $URL/v1/machines -H \"Authorization: Bearer \$TOWER\""
show "api $URL/v1/machines -H \"Authorization: Bearer \$TOWER2\" | jq -c '.[]'"

section "Shutdown on SIGTERM"
show "kill -TERM $PID; wait $PID"
show "cat \$T/server.log"
```

</details>

```
## Refuses to start without a read_token

$ ./bin/sessionhub server
server: read_token is not set; set read_token in server.toml or SESSIONHUB_READ_TOKEN
[exit 1]

## Config

$ cat $SESSIONHUB_SERVER_CONFIG
listen = ["127.0.0.1:18787"]
public_url = "https://sessionhub.example.test"
read_token = "hub_r_…"
[exit 0]

## Add machines

$ ./bin/sessionhub machine add tower --ssh-host tower.example.com --herdr-host tower.example.com | tee $SCRATCH/sessionhub-e2e.IUrZ/tower.txt
added machine tower (ssh_host tower.example.com, herdr_host tower.example.com)
token: hub_m_…
server_url: https://sessionhub.example.test
Save the token now: it is not shown again, and the database keeps only its hash.
[exit 0]

$ ./bin/sessionhub machine add bluebox --json | tee $SCRATCH/sessionhub-e2e.IUrZ/bluebox.json
{"name":"bluebox","token":"hub_m_…","server_url":"https://sessionhub.example.test"}
[exit 0]

$ ./bin/sessionhub machine ls
NAME  SSH_HOST     HERDR_HOST   LAST_SEEN
bluebox   bluebox          bluebox          never
tower  tower.example.com  tower.example.com  never
[exit 0]

## Start the server (SESSIONHUB_STALE_AFTER=15s)

$ curl -sS http://127.0.0.1:18787/healthz
ok
[exit 0]

Session IDs: A=c0a99df8-a843-4c0f-86a8-467ca8580780 B=432a47a6-1ff6-4e97-ba0b-dff63aad1760 C=97c633f8-e6b2-4e28-8734-1c10bf925702 (all herdr panes on tower), H=04ee7dc1-dd50-4de1-81db-ba7a73852b9a (hooks-only on tower)

## Register

$ req -X POST http://127.0.0.1:18787/v1/sessions -H "Authorization: Bearer $TOWER" -d '{"id":"c0a99df8-a843-4c0f-86a8-467ca8580780","agent":"claude","source":"plugin","cwd":"/home/user/session-hub","git_repo":"session-hub","git_branch":"sessionhub-v1","herdr_session":"default","herdr_workspace":"w1","herdr_pane":"p1","agent_state":"working","first_prompt":"build the sessionhub server"}'
{"id":"c0a99df8-a843-4c0f-86a8-467ca8580780","agent":"claude","machine":"tower","cwd":"/home/user/session-hub","git_repo":"session-hub","git_branch":"sessionhub-v1","herdr_session":"default","herdr_workspace":"w1","herdr_pane":"p1","title":"build the sessionhub server","title_source":"prompt","started_at":"2026-09-30T10:20:47.196277811Z","last_seen_at":"2026-09-30T10:20:47.196277811Z","status":"live","agent_state":"working","resume_command":"ssh -t tower.example.com sessionhub resume c0a99df8-a843-4c0f-86a8-467ca8580780"}

HTTP 201
[exit 0]

$ api -X POST http://127.0.0.1:18787/v1/sessions -H "Authorization: Bearer $TOWER" -d '{"id":"432a47a6-1ff6-4e97-ba0b-dff63aad1760","agent":"claude","source":"plugin","herdr_session":"default","herdr_pane":"p2","title_hint":"Fix login redirect"}' | jq -c '{id,title,title_source,status}'
{"id":"432a47a6-1ff6-4e97-ba0b-dff63aad1760","title":"Fix login redirect","title_source":"herdr","status":"live"}
[exit 0]

$ api -X POST http://127.0.0.1:18787/v1/sessions -H "Authorization: Bearer $TOWER" -d '{"id":"97c633f8-e6b2-4e28-8734-1c10bf925702","agent":"claude","source":"plugin","herdr_session":"work","herdr_pane":"p9"}' | jq -c '{id,herdr_session,status}'
{"id":"97c633f8-e6b2-4e28-8734-1c10bf925702","herdr_session":"work","status":"live"}
[exit 0]

$ api -X POST http://127.0.0.1:18787/v1/sessions -H "Authorization: Bearer $TOWER" -d '{"id":"04ee7dc1-dd50-4de1-81db-ba7a73852b9a","agent":"claude","source":"hooks","cwd":"/home/user/other"}' | jq -c '{id,herdr_pane,status}'
{"id":"04ee7dc1-dd50-4de1-81db-ba7a73852b9a","herdr_pane":null,"status":"live"}
[exit 0]

## Event: state_changed to blocked

$ api -X POST http://127.0.0.1:18787/v1/sessions/c0a99df8-a843-4c0f-86a8-467ca8580780/events -H "Authorization: Bearer $TOWER" -d '{"kind":"state_changed","source":"plugin","ts":"2026-09-30T10:20:47.292952962Z","payload":{"agent_state":"blocked"}}' | jq -c '{status,agent_state}'
{"status":"blocked","agent_state":"blocked"}
[exit 0]

## Report

$ api -X POST http://127.0.0.1:18787/v1/sessions/c0a99df8-a843-4c0f-86a8-467ca8580780/report -H "Authorization: Bearer $TOWER" -d '{"done":["store and schema"],"in_flight":["HTTP handlers"],"waiting_on":["review"],"note":"tests green"}' | jq -c .latest_report
{"ts":"2026-09-30T10:20:47.334912742Z","done":["store and schema"],"in_flight":["HTTP handlers"],"waiting_on":["review"],"note":"tests green"}
[exit 0]

## Title

$ api -X POST http://127.0.0.1:18787/v1/sessions/c0a99df8-a843-4c0f-86a8-467ca8580780/title -H "Authorization: Bearer $TOWER" -d '{"title":"sessionhub server, task 1"}' | jq -c '{title,title_source}'
{"title":"sessionhub server, task 1","title_source":"user"}
[exit 0]

A later herdr title hint does not replace the user title:
$ api -X POST http://127.0.0.1:18787/v1/sessions -H "Authorization: Bearer $TOWER" -d '{"id":"c0a99df8-a843-4c0f-86a8-467ca8580780","source":"plugin","title_hint":"Claude Code"}' | jq -c '{title,title_source}'
{"title":"sessionhub server, task 1","title_source":"user"}
[exit 0]

## List (read token)

$ api http://127.0.0.1:18787/v1/sessions -H "Authorization: Bearer $READ" | jq -c '.[] | {id,machine,status,agent_state,title,resume_command}'
{"id":"c0a99df8-a843-4c0f-86a8-467ca8580780","machine":"tower","status":"blocked","agent_state":"blocked","title":"sessionhub server, task 1","resume_command":"ssh -t tower.example.com sessionhub resume c0a99df8-a843-4c0f-86a8-467ca8580780"}
{"id":"04ee7dc1-dd50-4de1-81db-ba7a73852b9a","machine":"tower","status":"live","agent_state":null,"title":null,"resume_command":"ssh -t tower.example.com sessionhub resume 04ee7dc1-dd50-4de1-81db-ba7a73852b9a"}
{"id":"97c633f8-e6b2-4e28-8734-1c10bf925702","machine":"tower","status":"live","agent_state":null,"title":null,"resume_command":"ssh -t tower.example.com sessionhub resume 97c633f8-e6b2-4e28-8734-1c10bf925702"}
{"id":"432a47a6-1ff6-4e97-ba0b-dff63aad1760","machine":"tower","status":"live","agent_state":null,"title":"Fix login redirect","resume_command":"ssh -t tower.example.com sessionhub resume 432a47a6-1ff6-4e97-ba0b-dff63aad1760"}
[exit 0]

$ api 'http://127.0.0.1:18787/v1/sessions?live=true&machine=tower' -H "Authorization: Bearer $READ" | jq -c '[.[].id]'
["c0a99df8-a843-4c0f-86a8-467ca8580780","04ee7dc1-dd50-4de1-81db-ba7a73852b9a","97c633f8-e6b2-4e28-8734-1c10bf925702","432a47a6-1ff6-4e97-ba0b-dff63aad1760"]
[exit 0]

## Detail by prefix

$ api http://127.0.0.1:18787/v1/sessions/c0a99df8 -H "Authorization: Bearer $BLUEBOX" | jq .
{
  "id": "c0a99df8-a843-4c0f-86a8-467ca8580780",
  "agent": "claude",
  "machine": "tower",
  "cwd": "/home/user/session-hub",
  "git_repo": "session-hub",
  "git_branch": "sessionhub-v1",
  "herdr_session": "default",
  "herdr_workspace": "w1",
  "herdr_pane": "p1",
  "title": "sessionhub server, task 1",
  "title_source": "user",
  "started_at": "2026-09-30T10:20:47.196277811Z",
  "last_seen_at": "2026-09-30T10:20:47.395745612Z",
  "status": "blocked",
  "agent_state": "blocked",
  "resume_command": "ssh -t tower.example.com sessionhub resume c0a99df8-a843-4c0f-86a8-467ca8580780",
  "latest_report": {
    "ts": "2026-09-30T10:20:47.334912742Z",
    "done": [
      "store and schema"
    ],
    "in_flight": [
      "HTTP handlers"
    ],
    "waiting_on": [
      "review"
    ],
    "note": "tests green"
  },
  "reports": [
    {
      "ts": "2026-09-30T10:20:47.334912742Z",
      "done": [
        "store and schema"
      ],
      "in_flight": [
        "HTTP handlers"
      ],
      "waiting_on": [
        "review"
      ],
      "note": "tests green"
    }
  ],
  "events": [
    {
      "ts": "2026-09-30T10:20:47.292952962Z",
      "source": "plugin",
      "kind": "state_changed",
      "payload": {
        "agent_state": "blocked"
      }
    },
    {
      "ts": "2026-09-30T10:20:47.196277811Z",
      "source": "plugin",
      "kind": "registered"
    }
  ]
}
[exit 0]

$ req http://127.0.0.1:18787/v1/sessions/c0a -H "Authorization: Bearer $READ"
{"error":"invalid: id prefix \"c0a\" is shorter than 4 characters"}

HTTP 400
[exit 0]

## Reconcile: herdr snapshot lists only A

$ req -X PUT http://127.0.0.1:18787/v1/machines/self/herdr-sessions -H "Authorization: Bearer $TOWER" -d '{"herdr_session":"default","sessions":[{"id":"c0a99df8-a843-4c0f-86a8-467ca8580780","agent":"claude","herdr_pane":"p1"}]}'
{"upserted":1,"ended":["432a47a6-1ff6-4e97-ba0b-dff63aad1760"],"conflicts":[]}

HTTP 200
[exit 0]

$ api http://127.0.0.1:18787/v1/sessions -H "Authorization: Bearer $READ" | jq -c '.[] | {id,herdr_session,herdr_pane,status}'
{"id":"c0a99df8-a843-4c0f-86a8-467ca8580780","herdr_session":"default","herdr_pane":"p1","status":"blocked"}
{"id":"04ee7dc1-dd50-4de1-81db-ba7a73852b9a","herdr_session":null,"herdr_pane":null,"status":"live"}
{"id":"97c633f8-e6b2-4e28-8734-1c10bf925702","herdr_session":"work","herdr_pane":"p9","status":"live"}
{"id":"432a47a6-1ff6-4e97-ba0b-dff63aad1760","herdr_session":"default","herdr_pane":"p2","status":"ended"}
[exit 0]

$ api http://127.0.0.1:18787/v1/sessions/432a47a6-1ff6-4e97-ba0b-dff63aad1760 -H "Authorization: Bearer $READ" | jq -c '.events[0]'
{"ts":"2026-09-30T10:20:47.527010325Z","source":"plugin","kind":"ended","payload":{"reason":"missing_from_snapshot"}}
[exit 0]

## Bad tokens and wrong machine

$ req http://127.0.0.1:18787/v1/sessions
{"error":"missing or invalid bearer token"}

HTTP 401
[exit 0]

$ req http://127.0.0.1:18787/v1/sessions -H 'Authorization: Bearer hub_m_forged'
{"error":"missing or invalid bearer token"}

HTTP 401
[exit 0]

$ req -X POST http://127.0.0.1:18787/v1/sessions/c0a99df8-a843-4c0f-86a8-467ca8580780/title -H "Authorization: Bearer $READ" -d '{"title":"x"}'
{"error":"the read token cannot write; use a machine token"}

HTTP 403
[exit 0]

$ req -X POST http://127.0.0.1:18787/v1/sessions/c0a99df8-a843-4c0f-86a8-467ca8580780/title -H "Authorization: Bearer $BLUEBOX" -d '{"title":"x"}'
{"error":"conflict: session c0a99df8-a843-4c0f-86a8-467ca8580780 is registered to machine \"tower\""}

HTTP 409
[exit 0]

$ head -c 70000 /dev/zero | tr '\0' a | req -X POST http://127.0.0.1:18787/v1/sessions -H "Authorization: Bearer $TOWER" --data-binary @-
{"error":"request body is larger than 64 KiB"}

HTTP 413
[exit 0]

## Stale after SESSIONHUB_STALE_AFTER (15s)

$ sleep 20
[exit 0]

$ api http://127.0.0.1:18787/v1/sessions -H "Authorization: Bearer $READ" | jq -c '.[] | {id,status}'
{"id":"c0a99df8-a843-4c0f-86a8-467ca8580780","status":"stale"}
{"id":"04ee7dc1-dd50-4de1-81db-ba7a73852b9a","status":"stale"}
{"id":"97c633f8-e6b2-4e28-8734-1c10bf925702","status":"stale"}
{"id":"432a47a6-1ff6-4e97-ba0b-dff63aad1760","status":"ended"}
[exit 0]

$ req 'http://127.0.0.1:18787/v1/sessions?live=true' -H "Authorization: Bearer $READ"
[]

HTTP 200
[exit 0]

The heartbeat (snapshot) brings A back:
$ req -X PUT http://127.0.0.1:18787/v1/machines/self/herdr-sessions -H "Authorization: Bearer $TOWER" -d '{"herdr_session":"default","sessions":[{"id":"c0a99df8-a843-4c0f-86a8-467ca8580780","herdr_pane":"p1"}]}'
{"upserted":1,"ended":[],"conflicts":[]}

HTTP 200
[exit 0]

$ api 'http://127.0.0.1:18787/v1/sessions?live=true' -H "Authorization: Bearer $READ" | jq -c '.[] | {id,status}'
{"id":"c0a99df8-a843-4c0f-86a8-467ca8580780","status":"blocked"}
[exit 0]

## Machines

$ api http://127.0.0.1:18787/v1/machines -H "Authorization: Bearer $READ" | jq -c '.[]'
{"name":"bluebox","ssh_host":"bluebox","herdr_host":"bluebox","last_seen":"2026-09-30T10:20:47.630074447Z"}
{"name":"tower","ssh_host":"tower.example.com","herdr_host":"tower.example.com","last_seen":"2026-09-30T10:21:07.705948239Z"}
[exit 0]

$ ./bin/sessionhub machine rm bluebox
removed machine bluebox; its token no longer works
[exit 0]

$ req http://127.0.0.1:18787/v1/sessions -H "Authorization: Bearer $BLUEBOX"
{"error":"missing or invalid bearer token"}

HTTP 401
[exit 0]

Rotating tower while the server runs: the old token stops working at once.
$ ./bin/sessionhub machine add tower --json > $T/tower2.json; jq -c '{name,server_url}' $T/tower2.json
{"name":"tower","server_url":"https://sessionhub.example.test"}
[exit 0]

$ req http://127.0.0.1:18787/v1/machines -H "Authorization: Bearer $TOWER"
{"error":"missing or invalid bearer token"}

HTTP 401
[exit 0]

$ api http://127.0.0.1:18787/v1/machines -H "Authorization: Bearer $TOWER2" | jq -c '.[]'
{"name":"tower","ssh_host":"tower.example.com","herdr_host":"tower.example.com","last_seen":"2026-09-30T10:21:07.89104439Z"}
[exit 0]

## Shutdown on SIGTERM

$ kill -TERM 54876; wait 54876
[exit 0]

$ cat $T/server.log
2026/09/30 13:20:47 sessionhub server listening on 127.0.0.1:18787 (db $SCRATCH/sessionhub-e2e.IUrZ/data/sessionhub.db, stale_after 15s)
2026/09/30 13:20:47 GET /healthz 200 19µs machine=-
2026/09/30 13:20:47 GET /healthz 200 14µs machine=-
2026/09/30 13:20:47 POST /v1/sessions 201 9.31ms machine=tower
2026/09/30 13:20:47 POST /v1/sessions 201 6.549ms machine=tower
2026/09/30 13:20:47 POST /v1/sessions 201 6.762ms machine=tower
2026/09/30 13:20:47 POST /v1/sessions 201 7.013ms machine=tower
2026/09/30 13:20:47 POST /v1/sessions/c0a99df8-a843-4c0f-86a8-467ca8580780/events 200 6.896ms machine=tower
2026/09/30 13:20:47 POST /v1/sessions/c0a99df8-a843-4c0f-86a8-467ca8580780/report 200 6.803ms machine=tower
2026/09/30 13:20:47 POST /v1/sessions/c0a99df8-a843-4c0f-86a8-467ca8580780/title 200 6.834ms machine=tower
2026/09/30 13:20:47 POST /v1/sessions 200 7.262ms machine=tower
2026/09/30 13:20:47 GET /v1/sessions 200 706µs machine=read
2026/09/30 13:20:47 GET /v1/sessions 200 754µs machine=read
2026/09/30 13:20:47 GET /v1/sessions/c0a99df8 200 5.018ms machine=bluebox
2026/09/30 13:20:47 GET /v1/sessions/c0a 400 94µs machine=read
2026/09/30 13:20:47 PUT /v1/machines/self/herdr-sessions 200 6.293ms machine=tower
2026/09/30 13:20:47 GET /v1/sessions 200 585µs machine=read
2026/09/30 13:20:47 GET /v1/sessions/432a47a6-1ff6-4e97-ba0b-dff63aad1760 200 751µs machine=read
2026/09/30 13:20:47 GET /v1/sessions 401 25µs machine=-
2026/09/30 13:20:47 GET /v1/sessions 401 252µs machine=-
2026/09/30 13:20:47 POST /v1/sessions/c0a99df8-a843-4c0f-86a8-467ca8580780/title 403 18µs machine=read
2026/09/30 13:20:47 POST /v1/sessions/c0a99df8-a843-4c0f-86a8-467ca8580780/title 409 6.082ms machine=bluebox
2026/09/30 13:20:47 POST /v1/sessions 413 3.676ms machine=tower
2026/09/30 13:21:07 GET /v1/sessions 200 659µs machine=read
2026/09/30 13:21:07 GET /v1/sessions 200 545µs machine=read
2026/09/30 13:21:07 PUT /v1/machines/self/herdr-sessions 200 12.046ms machine=tower
2026/09/30 13:21:07 GET /v1/sessions 200 535µs machine=read
2026/09/30 13:21:07 GET /v1/machines 200 379µs machine=read
2026/09/30 13:21:07 GET /v1/sessions 401 390µs machine=-
2026/09/30 13:21:07 GET /v1/machines 401 299µs machine=-
2026/09/30 13:21:07 GET /v1/machines 200 4.11ms machine=tower
2026/09/30 13:21:07 sessionhub server shutting down
[exit 0]
```

## What the walk shows

- Without `read_token`, `sessionhub server` exits 1 with a message and creates no
  database.
- `sessionhub machine add` prints the token once; `--json` prints one JSON line.
  Hosts default to the machine name.
- The first upsert returns `201` with `resume_command` built from `tower`'s
  `ssh_host`. A's title comes from `first_prompt` (`prompt`) and B's from a
  herdr hint (`herdr`). `/title` makes A's title `user`, and a later herdr
  hint does not replace it.
- `state_changed` to `blocked` makes the status `blocked`, and `live=true`
  still lists it.
- The detail accepts an 8-character prefix, with a different machine's token
  (machine tokens can read everything). A 3-character prefix is `400`.
- The herdr snapshot for `default` that lists only A ends B (same machine,
  same herdr session, has a pane) with an `ended` event whose payload is
  `{"reason":"missing_from_snapshot"}`. C (herdr session `work`) and the
  hooks-only H stay `live`.
- No token and a forged token are `401`, the read token writing is `403`,
  `bluebox` writing `tower`'s session is `409`, and a 70,000-byte body is `413`.
- After 20 seconds with no writes, every session not ended is `stale` and
  `live=true` returns `[]`. The next snapshot (the heartbeat) brings A back
  as `blocked`; H and C stay `stale`.
- Gotcha observed on WSL2: an earlier run of this script used `sleep 16`, and
  A was still `blocked` afterwards. The server log showed only about 14
  seconds of wall-clock time across the sleep, and no write to A in between:
  the WSL2 wall clock was adjusted during the sleep. Status compares
  wall-clock times read back from the database, so a clock step shifts
  staleness by the same amount. The rerun above uses `sleep 20`.
- `sessionhub machine rm bluebox` and rotating `tower` both take effect on the running
  server at once. The server log has one line per request with the machine,
  and `SIGTERM` shuts it down with exit 0.
