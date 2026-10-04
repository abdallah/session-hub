# Task 3 evidence: client library and herdr socket client

Date: 2026-09-30. herdr 0.9.3 (protocol 22). The live read-only run below used
the default socket on this machine; the `pane.split` capture used `tower`.

Scrubbing applied to pasted output, and nothing else: session UUIDs became
`<uuid>`, terminal titles became `<title>`, the pane working directory became
`<home-path>`, and `/home/me` became `/home/user`. Pane and workspace IDs
(`wA:p1` and so on), counts, statuses, versions, and error text are verbatim.

## Live herdr socket, read-only

```
$ SESSIONHUB_LIVE_HERDR=1 go test -count=1 -v -run TestLiveReadOnly ./internal/herdr
=== RUN   TestLiveReadOnly
    live_test.go:26: socket=/home/user/.config/herdr/herdr.sock session=default herdr=0.9.3 protocol=22 panes=6 agents=4 focused=wA:p1
    live_test.go:29: pane wA:p1 agent="claude" status=done session="<uuid>" title="<title>"
    live_test.go:29: pane wA:pK agent="" status=unknown session="" title="<title>"
    live_test.go:29: pane wC:p1 agent="claude" status=idle session="<uuid>" title="<title>"
    live_test.go:29: pane wC:p4 agent="" status=unknown session="" title="<title>"
    live_test.go:29: pane wD:p1 agent="claude" status=idle session="<uuid>" title="<title>"
    live_test.go:29: pane wF:p1 agent="claude" status=done session="<uuid>" title="<title>"
    live_test.go:42: PaneGet(wA:p1): agent="claude" status=done cwd=<home-path>
    live_test.go:44: PaneGet(missing) error: herdr: pane_not_found: pane w999:p999 not found
--- PASS: TestLiveReadOnly (0.01s)
PASS
ok  	session-hub/internal/herdr	0.011s
```

The test makes only `session.snapshot` and `pane.get` calls. This machine's
herdr also runs 0.9.3, not 0.8.2.

## Finding: herdr answers one request per connection

The first version held one connection and sent `session.snapshot`, then
`pane.get`. The second call failed:

```
live_test.go:37: write unix @->/home/me/.config/herdr/herdr.sock: write: broken pipe
```

Reproduced without Go (python3: connect, send `ping`, send `ping` again):

```
b'{"id":"a","result":{"type":"pong","version":"0.9.3","protocol":22,"capabilities":{"live_handoff":true,"detached_server_daemon":true,"endpoint_protocol_generation":1,"surface_interest":true,"health_che
second [Errno 32] Broken pipe
```

Fix: `herdr.Client` opens a connection per call. The fake server in `herdrtest`
closes after one response, so unit tests fail if a client regresses to reusing
a connection. docs/dev/PLAN.md now states this.

## Finding: real `pane.split` response (fix round 1)

Only in a scratch workspace on `tower` that I created and closed:

```
$ ssh tower 'herdr workspace create --cwd /tmp/sessionhub-probe-work --label t3-scratch --no-focus'   # workspace wA, root pane wA:p1
$ pane.split over the socket, target_pane_id wA:p1, focus false
{"id":"req_1","result":{"type":"pane_info","pane":{"pane_id":"wA:p2","terminal_id":"term_65cb09fe9fbf1b","workspace_id":"wA","tab_id":"wA:t1","focused":false,"cwd":"/tmp/sessionhub-probe-work","foreground_cwd":"/tmp/sessionhub-probe-work","agent_status":"unknown","scroll":{"offset_from_bottom":0,"max_offset_from_bottom":0,"viewport_rows":89},"revision":0}}}
$ ssh tower 'herdr workspace close wA'
{"id":"cli:workspace:close","result":{"type":"ok"}}
$ ssh tower 'herdr workspace list | grep -c t3-scratch'
0
```

Saved unscrubbed as `testdata/herdr/socket/pane-split.ndjson` (nothing in it
is personal). `PaneSplit` now returns an error when the response has no pane
ID; `TestPaneSplitCaptured` and `TestPaneCallsErrorWithoutPane` cover both.

## Queue crash tests

`internal/client/queue_test.go` re-execs the test binary as a helper:

- `TestQueueKillDuringAppendsLosesNothing`: 4 helper processes append 2 KB
  items in a loop and are SIGKILLed at random points. Every item the helper
  printed (returned from `Append`) appears exactly once; no item is duplicated.
- `TestQueueKillDuringDrainKeepsFileIntact`: a helper SIGKILLs itself inside
  the `Drain` callback. The file is byte-identical afterward, and the lock is
  free for the next drain.
- `TestQueueSurvivesTornLastLine`, `TestQueueStaleTempFileIgnored`.

## Suite

```
$ GOFLAGS=-count=1 make test lint      (lines for packages without tests removed)
go test ./...
ok  	session-hub/cmd/sessionhub	0.004s
ok  	session-hub/internal/client	2.747s
ok  	session-hub/internal/gitinfo	0.050s
ok  	session-hub/internal/herdr	2.148s
ok  	session-hub/internal/paths	0.004s
test -z "$(gofmt -l .)"
go vet ./...
```

38 tests pass, 0 fail (`go test -v ./... | grep -c '^--- PASS'`), plus the
skipped opt-in `TestLiveReadOnly` when `SESSIONHUB_LIVE_HERDR` is unset.
