# Task 6 evidence: MCP server

## Real binary over stdio, server unreachable

Input has `initialize`, the `initialized` notification, a `report_progress`
call, and an unknown method. `SESSIONHUB_SERVER_URL` points at a closed port and
`CLAUDE_CODE_SESSION_ID` supplies the session.

```
$ SESSIONHUB_STATE_DIR=<scratch>/t6state SESSIONHUB_CONFIG=/nonexistent.toml SESSIONHUB_SERVER_URL=http://127.0.0.1:1 \
    CLAUDE_CODE_SESSION_ID=aaaaaaaa-0000-4000-8000-000000000001 ./bin/sessionhub mcp < t6-in.ndjson
{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{"listChanged":false}},"instructions":"...","protocolVersion":"2025-06-18","serverInfo":{"name":"sessionhub","version":"dev"}}}
sessionhub mcp: 2026/09/30 13:37:26 report: Post "http://127.0.0.1:1/v1/sessions/aaaaaaaa-.../report": dial tcp 127.0.0.1:1: connect: connection refused      <- stderr
{"jsonrpc":"2.0","id":2,"result":{"content":[{"text":"sessionhub: the server is unreachable, so the report was queued and will be sent later.","type":"text"}]}}
{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"method not found: bogus"}}
exit=0
$ cat <scratch>/t6state/queue.jsonl
{"id":"8a27d14835977202","op":"report","session_id":"aaaaaaaa-0000-4000-8000-000000000001","body":{"done":["a"],"in_flight":[],"waiting_on":[]},"queued_at":"2026-09-30T10:37:26.617337863Z"}
```

Stdout has only protocol lines (the notification got no reply); the log line
went to stderr; the tool result is not an error; the report is in the queue.

## Real `claude` against a real `sessionhub server`

Server from this tree (merged `sessionhub-v1` at fce3939) on `127.0.0.1:18787` with a
temporary database, machine `e2e` added with `sessionhub machine add e2e --json`.
The MCP config points `command` at the built `bin/sessionhub mcp` and passes
`SESSIONHUB_SERVER_URL`, `SESSIONHUB_TOKEN`, `SESSIONHUB_CONFIG=/nonexistent.toml`, and a temporary
`SESSIONHUB_STATE_DIR` in `env`. The session was not registered beforehand, so the
404 upsert-and-retry path ran for real.

```
$ cd <scratchpad>/e2e/work
$ claude -p --model haiku --strict-mcp-config --mcp-config ../mcp.json \
    --allowedTools mcp__hub__report_progress mcp__hub__set_title --output-format json \
    "Call the sessionhub set_title tool with title 'e2e evidence run'. Then call the sessionhub report_progress tool with done=['wrote the MCP server'], in_flight=['running evidence'], waiting_on=[] and note='e2e'. Then reply with the text of each tool result."
result: set_title result: "sessionhub: title recorded."   report_progress result: "sessionhub: report recorded."
is_error=false  num_turns=5  permission_denials=[]

$ server log
POST /v1/sessions/1188eb7d-.../title 404 machine=e2e
POST /v1/sessions 201 machine=e2e
POST /v1/sessions/1188eb7d-.../title 200 machine=e2e
POST /v1/sessions/1188eb7d-.../report 200 machine=e2e

$ curl -H "Authorization: Bearer <read token>" http://127.0.0.1:18787/v1/sessions/1188eb7d-fe63-4f40-b017-b3c529d58fc4
{ "id": "1188eb7d-...", "agent": "claude", "machine": "e2e",
  "cwd": "<scratchpad>/e2e/work", "title": "e2e evidence run", "title_source": "user",
  "status": "live",
  "latest_report": { "done": ["wrote the MCP server"], "in_flight": ["running evidence"], "waiting_on": [], "note": "e2e" },
  "reports": [ ...same one report... ],
  "events": [ { "source": "mcp", "kind": "registered" } ] }
```

The session ID came from `CLAUDE_CODE_SESSION_ID` (no herdr, no hook file).
The temporary server, database, config, and state dir were removed afterward.

## Suite

```
$ make test lint
ok  	session-hub/cmd/sessionhub
ok  	session-hub/internal/client
ok  	session-hub/internal/gitinfo
ok  	session-hub/internal/herdr
ok  	session-hub/internal/mcp
ok  	session-hub/internal/paths
test -z "$(gofmt -l .)"
go vet ./...
```

after merging sessionhub-v1: `make test lint` green (cmd/sessionhub, client, gitinfo, herdr, mcp, paths, server, store ok) (`go test -v ./... | grep -c '^--- PASS'`).
