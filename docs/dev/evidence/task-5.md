# Task 5 evidence: Claude Code hooks client

Status: complete. Offline checks first, then the end-to-end run with a real
`claude -p --model haiku` against a local `sessionhub server` built from this tree
(merged with `sessionhub-v1` at `fce3939`).

## Offline: built binary on a copy of the real settings.json

Script: a copy of `~/.claude/settings.json` in a scratch dir, `CLAUDE_CONFIG_DIR`
pointing at it. `~/.claude` was never touched.

```
$ sessionhub install-hooks --binary <sessionhub>        # twice
Installed sessionhub hooks in <scratch>/cfg/settings.json (backup: ...settings.json.sessionhub-backup-20260930T133800)
sessionhub hooks are already installed in <scratch>/cfg/settings.json
$ diff ~/.claude/settings.json <scratch>/cfg/settings.json
  (only added lines: `},` after herdr's group, the new session-start group,
   and the UserPromptSubmit, Stop, Notification, SessionEnd events)
$ sessionhub uninstall-hooks --binary <sessionhub>
Removed sessionhub hooks from <scratch>/cfg/settings.json (backup: ...)
$ diff ~/.claude/settings.json <scratch>/cfg/settings.json && echo ROUNDTRIP-IDENTICAL
ROUNDTRIP-IDENTICAL
```

Fail-open, with the built binary:

```
$ echo garbage | sessionhub hook stop
hook stop: bad stdin JSON: invalid character 'g' looking for beginning of value
exit=0
$ echo '{"session_id":"abc","cwd":"/tmp"}' | sessionhub hook stop      # no server_url
hook: queued 1 item(s): sessionhub: server_url is not configured
exit=0
$ echo '{"session_id":"abc","cwd":"/tmp"}' | SESSIONHUB_SERVER_URL=http://127.0.0.1:1 sessionhub hook prompt
hook: queued 3 item(s): Post "http://127.0.0.1:1/v1/sessions": dial tcp 127.0.0.1:1: connect: connection refused
exit=0
$ wc -l queue.jsonl
4
```

## End to end

Setup (scratch dir under the session scratchpad, nothing in `~/.claude`):
a copy of the built binary, a git repo `proj/` with an `origin`, a temporary
database, `sessionhub server` on `127.0.0.1:18787`, a machine `e2e` from
`sessionhub machine add e2e --json`, and a client config with that token.

```
$ sessionhub install-hooks --settings proj/.claude/settings.local.json --binary <sessionhub>
Installed sessionhub hooks in .../proj/.claude/settings.local.json
  (five events: SessionStart/Notification/SessionEnd with matcher "*",
   UserPromptSubmit/Stop without; async true, SessionEnd timeout 2)
$ cd proj && claude -p --model haiku "Reply with the single word ok"
ok
$ curl -H 'Authorization: Bearer <read token>' $SERVER/v1/sessions
[{"id":"5a66b1a3-...","agent":"claude","machine":"e2e","cwd":".../proj",
  "git_repo":"git@example.com:demo/e2e.git","title":"Reply with the single word ok",
  "title_source":"prompt","status":"live","agent_state":"idle", ...}]
```

`git_branch` is empty because the scratch repo has no commits (`rev-parse`
fails there; see `docs/client.md`).

Session detail (events, oldest first) right after the run:

```
hooks registered
hooks prompt          {'prompt': 'Reply with the single word ok'}
hooks state_changed   {'agent_state': 'working'}
hooks state_changed   {'agent_state': 'idle'}
```

`SessionEnd` queued its event and made no network call. The state dir then
held `queue.jsonl` with exactly one line and an empty `current/` (the
`current/<pid>` file written at SessionStart was removed):

```
{"id":"d5c7e75d3c61b0b1","op":"event","session_id":"5a66b1a3-...","body":{"kind":"ended","source":"hooks","ts":"2026-09-30T10:43:55.165260314Z","payload":{"reason":"other"}},...}
```

The server was restarted on the same database, and the next hook (a `stop`
for an unrelated session, run by hand) drained the queue. The queue file is
gone and the first session reads `status = ended`,
`ended_at = 2026-09-30T10:44:13Z`, with a fifth event
`hooks ended {'reason': 'other'}`.

That hand-run `stop` also covered the unknown-session path (hooks installed
while a session runs). The server log shows the 404 and the retry:

```
POST /v1/sessions/11111111-.../events 404
POST /v1/sessions/11111111-.../events 200
```

and the session detail shows `registered` (from the fallback upsert, with
`git_repo`) followed by `state_changed idle`.

Observed gap in that run: `ended` reached the server only when a later hook
or the plugin watcher drained the queue. Fixed by the detached flush below.

## End to end, detached flush

Same setup, fresh database, one `claude -p --model haiku` run, and no other
hook run by hand:

```
$ claude -p --model haiku "Reply with the single word ok"
ok
$ curl .../v1/sessions/<id>          # about 2 s later
status = ended
ended_at = 2026-09-30T10:58:35.655756449Z
10:58:28 hooks registered
10:58:31 hooks prompt          {'prompt': 'Reply with the single word ok'}
10:58:31 hooks state_changed   {'agent_state': 'working'}
10:58:35 hooks state_changed   {'agent_state': 'idle'}
10:58:35.639 hooks ended       {'reason': 'other'}
$ cat state/queue.jsonl
(gone)
$ pgrep -fa 'sessionhub-e2e hook flush'
none
```

`SessionEnd` queued `ended` and started `sessionhub hook flush` in a detached
process; the child sent it 16 ms after the queue append and exited.
