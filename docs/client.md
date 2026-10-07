# Client library

Shared code for the herdr plugin, Claude Code hooks, MCP server, and CLI.
Callers decide to swallow errors, so an unreachable server never blocks herdr
or Claude Code.

## `internal/client`

- `LoadConfig() (Config, error)`: reads `paths.ClientConfig()`
  (`~/.config/sessionhub/config.toml`, env `SESSIONHUB_CONFIG`), then applies
  `SESSIONHUB_SERVER_URL`, `SESSIONHUB_TOKEN`, `SESSIONHUB_MACHINE`. A missing file gives an empty
  config. `Config.Save()` writes mode 0600 through a temp file and creates
  parent directories. Keys: `server_url`, `token`, `machine`,
  `remote_permissions` (unset means on; `false` turns off remote permission
  answers on this machine), `remote_start` (unset means on; `false` stops
  the watcher from starting new sessions on request), and `move_roots`
  (directories besides `~/Code` where a moved session's clone is searched
  for, absolute or starting with `~/`). `RemotePermissionsOff(cfg, os.Getenv)`
  is also true when `SESSIONHUB_REMOTE_PERMISSIONS=off`, and
  `RemoteStartOff(cfg, os.Getenv)` when `SESSIONHUB_REMOTE_START=off`.
- `New(cfg) (*Client, error)`: errors if `server_url` is empty. Every request
  has a 2 s timeout (`Timeout`) and `Authorization: Bearer <token>`.
  `SetTimeout(d)` changes the limit; the commands a person runs and waits for
  (`sessionhub ls`, `sessionhub show`, `sessionhub login`, `sessionhub inbox`, `sessionhub resume`) use
  `InteractiveTimeout`, 15 s, so a slow tunnel doesn't fail them. Hooks, the
  MCP server, the watcher, and digests keep 2 s. The control long poll is the
  exception (below). Every call's time limit is a context deadline,
  not `http.Client.Timeout`, so one call can have a longer one.
- Methods (all take a `context.Context` first): `UpsertSession`,
  `PutHerdrSessions`, `PostEvent(id, api.EventIn)`, `PostReport(id, api.ReportIn)`,
  `SetTitle(id, title)`, `ListSessions(live bool, machine string)`,
  `GetSession(idOrPrefix)`, `ListMachines`, `Health`,
  `CreateControl(id)`, `PollControl(wait)`,
  `PostControlResult(id, api.ControlResultIn)`, `CreateLogin(name)`,
  `ListWebSessions`, `RevokeWebSession(id)`, `Inbox`,
  `DismissInbox(id, since)`, `SnoozeInbox(id, since, until)`,
  `Instructions`, `AddInstruction(text)`, `DeleteInstruction(id)`,
  `SendMessages(api.MessagesIn)`, `GetMessage(id)`,
  `CreatePermission(sessionID, api.PermissionIn)`,
  `WaitDecision(id, wait)`, and `DecidePermission(id, api.DecisionIn)`.
  Pass the inbox item's `since` to `DismissInbox` and `SnoozeInbox`
  unchanged. `WaitDecision` is a long poll like `PollControl`: its time limit
  is `wait` plus 10 s, and it returns `nil` on `204`.
  `SendMessages` sends `from_session` only when the caller sets it (the MCP
  tool sets its own session). The server answers `404` for an unknown
  `from_session` and `409` for another machine's; both come back as a
  `*StatusError` with the server's message.
- The local rules copy is `<state dir>/instructions.json`
  (`InstructionsPath`). `LoadInstructions` reads it without any network
  call, `SaveInstructions` writes it with mode 0600 through a temp file and a
  rename, and `RefreshInstructions(ctx, c, path)` fetches the list and
  rewrites the copy only when its `version` changed.
- `PollControl(ctx, wait)` holds the control long poll open
  (`GET /v1/machines/self/control?wait=<whole seconds, at least 1>`) with a
  time limit of `wait` plus 10 s. It returns the claimed request and its
  session, or `nil` on `204`. `PostControlResult` and `CreateControl` are
  ordinary calls; `CreateControl` returns the new request, or the session's
  open one.
- Starts: `CreateStart(machine, api.StartIn)` and `GetStart(id)` are
  ordinary calls; `sessionhub start` uses them.
- Moves: `CreateMove(sessionID, target)`, `GetMove(id)`,
  `PutMoveKey(publicKey)`, and `PostMoveResult(id, api.MoveResultIn)` are
  ordinary calls. `PutMoveBundle(id, reader, size)` streams a sealed bundle
  with `Content-Length` and `application/octet-stream`;
  `GetMoveBundle(id, writer)` streams one back and refuses more than 64 MiB.
  Both use `BundleTimeout` (10 minutes) instead of the per-request limit and
  return `*StatusError` for a non-2xx answer.
- The Claude Code mod's calls, which `sessionhub mod` makes for the mod:
  - `SetBlockedOn(id, text)` posts `POST /v1/sessions/{id}/blocked-on`; an
    empty `text` clears a mod-set block. The server cleans and cuts the text.
  - `ClearBlockedOn(id, question)` posts `{"text": "", "clears": question}`,
    which clears the block only while it is that question.
  - `PutUsage(id, api.UsageIn)` puts the context window fill and cost; a nil
    field is left out and keeps the stored value.
  - `PollSessionMessage(id, wait) (*api.ModMessage, error)` holds `GET
    /v1/sessions/{id}/messages/next?wait=<whole seconds, 1 to 30>` open with
    a time limit of `wait` plus 10 s, and returns the claimed message (`ID`,
    and `Text`, the prompt to submit), or `nil` on `204`. A `200` without an
    ID is an error.
  - `PostMessageResult(id, api.MessageResultIn) (api.Message, error)` posts
    `delivered`, `busy`, or `refused`.

  Another machine's session gets a `*StatusError` with status `409`.
  There is no queue op that sets a question. `internal/modcmd` (`sessionhub mod`)
  uses these calls with a 5-second limit, and `poll` with a 30-second
  deadline for a 25-second wait. See `docs/cli.md`, "`sessionhub mod`".
- Non-2xx responses return `*StatusError{Status, Message}`. `Message` is the
  `api.Error` text, or the trimmed body when it is not JSON (a proxy page).
- `ListSessions` and `ListMachines` decode a bare JSON array, which is what
  the server returns. An object response is a decode error.
  `ListSessions(live=true)` returns `live` and `blocked` sessions.

### Queue

`NewQueue(dir)` or `DefaultQueue()` (`paths.StateDir()`): files `queue.jsonl`
and `queue.lock`.

- `Append(Item)`: takes an exclusive `flock`, writes one line, fsyncs. Sets
  `ID` and `QueuedAt` when empty. Item ops: `upsert`, `event`,
  `herdr_sessions`, `report`, `title`, `digest`, and `blocked_on_clear`
  (`OpBlockedOnClear`, with `SessionID` and a body of `{"clears":
  "<question>"}`: it clears that question only, through `ClearBlockedOn`; an
  item with no body clears only a mod-set block), and the three task ops:
  `task_create` (`OpTaskCreate`, body `api.TaskIn` with a client-made `id`),
  `task_state` (`OpTaskState`, body `api.TaskStateIn`), and `task_link`
  (`OpTaskLink`, body `api.TaskLinkIn`). Each task op needs `SessionID`, and
  `task_state` and `task_link` also need `TaskID`, the task the call acts on;
  their bodies carry a client-made `event_id`, so a replay changes nothing
  twice. `Replay` sets the body's `session_id` from `Item.SessionID`. `Body`
  is the JSON body of the matching API call.
  `Replay` sends any of them. There is no op that sets a question: a set
  replayed later would block a session that was already answered, so
  `SetBlockedOn` with text is best effort.
- `Drain(fn)`: holds the lock, reads the file, and calls `fn(all items)`. If
  the read fails with anything other than end of file, `Drain` returns the
  error without calling `fn` and leaves the file alone, so a partial read
  never shrinks the queue. `Drain` then rewrites the file
  with the items `fn` returns (temp file, fsync, rename, directory fsync; the
  file is removed when nothing is left).
- Crash behavior: the file is always either the old or the new content. A
  torn last line (kill mid-write) is dropped on read, and the next `Append`
  starts a new line. Delivery is at-least-once: a kill after `fn` sent items
  but before the rename leaves them queued, so the next `Drain` sends them
  again. `Item.ID` is stable across that replay. The server's upsert is
  idempotent; an `event` can be recorded twice in that narrow window.
- `Append` waits while `Drain` holds the lock, so keep `fn` within the HTTP
  timeout per item and stop at the first failure.

### Drainer contract

Three processes drain the queue: the herdr plugin watcher, the hooks client
after a live hook, and the `sessionhub hook flush` child that `session-end` starts.
Each one calls `Queue.Drain` and, for every item, `client.Replay(ctx, c, item)`.
Do not switch on `Item.Op` in a drainer, except for the 404 upsert-and-retry.

**Ops.** `Replay` sends each op to one endpoint:

| Op | Body | Needs `session_id` | Endpoint |
|---|---|---|---|
| `upsert` | `api.SessionUpsert` | no | `POST /v1/sessions` |
| `herdr_sessions` | `api.HerdrSessionsPut` | no | `PUT /v1/machines/self/herdr-sessions` |
| `event` | `api.EventIn` | yes | `POST /v1/sessions/{id}/events` |
| `report` | `api.ReportIn` | yes | `POST /v1/sessions/{id}/report` |
| `title` | `api.TitleIn` | yes | `POST /v1/sessions/{id}/title` |

An unknown op, a body that does not decode, or a missing `session_id` on an op
that needs one is a permanent error. No retry can fix it.

**Error rules.** After `Replay` returns an error:

- `IsAuthError` (401, 403): stop the pass and keep this item and every later
  one. Fixing the token makes them succeed.
- `IsRetryable` (network error, timeout, 408, 429, 5xx): stop the pass and keep
  this item and every later one.
- Anything else (other 4xx, a permanent error): drop the item and log one
  line.
- `IsNotFound` (404): see the upsert retry below. It is not retryable by
  itself.

**Pass budget.** One pass has one deadline of 1 s in total, not per request,
and sends at most 50 items. Items not reached stay queued. This keeps the queue
lock short, so a `session-end` append never waits past Claude Code's 1.5 s
teardown budget.

**404 upsert retry.** A 404 on an `event`, `report`, or `title` means the
server does not know the session, for example because sessionhub was installed while
it ran. The drainer upserts the session and retries that item once. It does
not retry an `upsert` or `herdr_sessions` item on 404.

**Who may drain what.** The plugin watcher resolves pane-only items: an item
with no `session_id` and a `pane_id` came from the plugin, and only the watcher
can map the pane to a session. The hooks drain and the flush child keep those
items, in their original order, and neither send nor drop them. The hooks
drain and the flush child also skip the whole pass while a watcher holds
`watcher.lock`.

## Moves

`internal/move` carries a session to another machine or to the cloud. This
section covers the key and the sealing; later sections cover the bundle, Git,
and the watcher's steps.

- **Key.** Each machine has an X25519 key pair. `move.KeyPath()` is
  `move.key` next to the client config (`~/.config/sessionhub/move.key`, or beside
  `$SESSIONHUB_CONFIG`). The file holds the private key in standard base64 and a
  newline, mode `0600`. `LoadOrCreateKey` creates it on first use through a
  temp file and `link`, so two processes that start at once end with one
  key. A file that others can read, or that holds no key, is refused and
  never replaced: move it aside to make a new key, then restart the watcher
  so the server learns the new public key.
- **Fingerprint.** The first 16 hex characters of the SHA-256 of the 32-byte
  public key. `sessionhub machine ls` (on the server) and `sessionhub move-key` (on each
  machine) print it, so you can compare them out of band.
- **Sealing.** `Seal(source key, target public key, move ID, bundle)` makes an
  ephemeral key pair, runs ECDH with the target's key, runs ECDH between the
  source's static key and the target's key, and derives a 32-byte key with
  HKDF-SHA256 (salt: the move ID, info: `sessionhub move v1`). AES-256-GCM encrypts
  the whole bundle with the move ID followed by the 32-byte ephemeral public
  key as additional data. The sealed form is
  the version byte `1`, the 32-byte ephemeral public key, the 12-byte nonce,
  and the ciphertext with its tag (`SealOverhead`, 61 bytes, in all). `Open`
  takes the target's private key and the source's registered public key;
  a wrong key, a changed byte, or another move ID all fail with `ErrOpen`.
  Both work in memory, on at most 64 MiB of sealed bundle.
- **Threat model.** The server and Cloudflare relay ciphertext and cannot
  read it. Whoever controls the server or the tunnel could swap a registered
  public key; compare fingerprints out of band to detect that. Whoever holds the target's
  private key can also forge bundles that appear to come from the source
  (key-compromise impersonation, inherent to static-static ECDH).
- **Repository.** `ReadRepo(ctx, cwd, roots)` reads the work tree's top
  level, `origin`'s URL, the checked-out branch (a detached HEAD fails), the
  HEAD SHA, the session directory relative to the top level, and the top
  level relative to the search root that holds it (`DefaultRoots`: `~/Code`
  plus the client config's `move_roots`). Every Git call runs with
  `GIT_TERMINAL_PROMPT=0`, `GIT_OPTIONAL_LOCKS=0`, SSH in `BatchMode`, a
  timeout (30 s, 2 min for `push` and `fetch`), and a 1 GiB cap on its
  output. `NormalizeRemote` compares remotes without scheme, user, port,
  `.git`, or host case.
- **Bundle.** `Build` writes a tar archive, compressed with zstd at the
  default level on all cores (format 2; format 1 was an uncompressed tar):
  `manifest.json` first, then
  `transcript/<id>.jsonl`, the sidecar folder `transcript/<id>/...`,
  `file-history/<id>/...`, `git/changes.patch` (`git diff --binary HEAD`:
  staged and unstaged changes, binary files, deletions), and
  `git/untracked.tar` (`git ls-files --others --exclude-standard`, with the
  executable bit). The diff runs with `--no-color --no-ext-diff
  --no-textconv --no-renames --full-index --src-prefix=a/ --dst-prefix=b/`,
  so the user's diff config can't change it and `git apply -R` can reverse
  it. The diff and `ls-files` run with `-c core.fsmonitor=false` against a
  temporary copy of the index (`GIT_INDEX_FILE`), so `Build` never rewrites
  the repository's index. Secret-looking names (`.env*`, `*.pem`, `*.key`,
  `*.tfvars`) are not carried, tracked or untracked: the diff leaves them out
  with `:(exclude,icase,glob)` pathspecs. Those, links (in the repository,
  the sidecar folder, or the file history, or either folder itself), and
  nested repositories are listed in the manifest under `skipped`. The
  transcript and the file history are carried as they are, encrypted, so a
  secret that Claude read or edited during the session can travel inside
  them. `Build` checks the sizes of all files before it reads any of them:
  an untracked file over 10 MiB, files over 1 GiB in all, or a compressed
  and sealed bundle over 64 MiB, fails the move with the file names, the
  five largest for the bundle. `Extract` unpacks at most 1 GiB and refuses a
  bundle that unpacks to more. It refuses a format 1 bundle
  (`ErrOldBundle`, "upgrade sessionhub on" the source) and a newer one
  (`NewerBundleError`, "upgrade sessionhub on" the target). It also refuses a manifest with an invalid move ID, session ID, relative path,
  branch, or HEAD, and anything else: another entry name, `..`, an absolute
  path, a link, a hard link, a directory, an oversized entry, a duplicate,
  an entry the manifest does not list, or an untracked path inside `.git`.
- **Source steps.** `PushBranch` pushes the branch to `origin`: with `-u`
  when it has no upstream, to its upstream branch when that is on `origin`
  and HEAD is ahead, and under its own name without `-u` when it tracks
  another remote. It never forces.
  `ClaudeVersion` is the first field of `claude --version`. `Archive` moves
  a session's transcript, sidecar folder, and file history into
  `<state>/moved/<move id>/`, the transcript last so a partial failure can
  be retried, and removes archives older than 30 days.
- **Target steps.** `FindClone` searches the roots (4 levels, skipping
  hidden folders, `node_modules`, and the inside of a repository) and
  returns `ErrNoClone` when nothing matches; with several, the error lists
  them, and a cancelled context stops the search. `CheckClone` refuses tracked
  changes and carried paths that exist: the untracked files and the files
  the patch creates (`PatchNewFiles`). `PrepareClone` fetches `origin` and
  checks out the branch at the source's HEAD by fast-forward only, and
  when that fails puts the clone back and names any part it could not put
  back. `Checkout.Apply` applies the patch to the working tree (from a
  `0600` temp file in the clone's Git directory) and writes the
  untracked files with `O_EXCL`, checking each folder on the way with
  `lstat` so nothing is written through a link. `Checkout.Undo` takes back
  only what `Apply` did: it removes the files it wrote while they hold what
  it wrote, reverses the patch with `git apply -R`, checks out the previous
  branch, and moves the branch back to its old commit, or deletes a
  tracking branch `PrepareClone` created, with compare-and-swap
  `update-ref`. `ProjectDir` is Claude Code's project
  folder for a directory (every character but an ASCII letter or digit
  becomes `-`; over 200 characters, the one existing folder with that
  prefix). `WriteSession` writes the transcript, sidecar folder, and file
  history with `O_EXCL` and mode `0600`, and `RemoveFiles` takes them back.
- **Cloud.** `PushCloudBranch` commits the working tree's changes on top of
  HEAD through a temporary `GIT_INDEX_FILE` that starts as HEAD's tree, so
  the real index is never read or written and a staged new file is carried.
  It leaves secret-looking files out with the same pathspec excludes as the
  bundle, adds untracked files with literal pathspecs, skips links and
  untracked files over 10 MiB, and pushes `CloudBranch(id)`
  (`sessionhub/cloud-<id8>`) with `--force-with-lease` against the commit sessionhub
  pushed there before (read with `git ls-remote` and recognized by its
  message), so a cloud session's commits on that branch are never dropped.
  The temporary index is refreshed (`update-index -q --refresh`) after
  `read-tree`, so `add -u` hashes only changed files, and the commit is
  made with `commit-tree --no-gpg-sign`. It runs the user's pre-push hook
  like any push, and never touches the working tree, the real index, or
  the current branch; the push creates or updates the remote-tracking ref
  `refs/remotes/origin/sessionhub/cloud-<id8>` in the clone. `HandOffPrompt`
  writes the cloud session's first prompt, with the recap and the last
  request cut to 2000 characters each. `StartCloud` runs
  `claude --cloud <prompt>`, returns the first session link in its output
  once `ValidCloudLink` accepts it (`https://claude.ai/code/session_...`
  with nothing else in it), stops the command then, and fails after the
  wait with the output's last line. When the command printed another
  `claude.ai` link, or exited 0 without a link, the error is a
  `*CloudStartedError`: a cloud session may exist, so the caller must not
  restart the session.

## Digests

`internal/digest` builds a session's digest, the body of
`PUT /v1/sessions/{id}/digest`. `sessionhub digest <id>` and `sessionhub digest --all` run it.

**Transcript.** The reader looks for `<claude dir>/projects/*/<id>.jsonl`, where
the Claude directory is `CLAUDE_CONFIG_DIR` or `~/.claude`. It needs exactly one
match and never rebuilds the project folder name from the working directory.
Subagent transcripts are read too.

**State.** Each session keeps `~/.local/state/sessionhub/digest/<id>.json`, so each run
reads only the lines added since the last one, and `<id>.lock`. A second run for
the same session doesn't wait: it reports `skipped: another run is in progress`.

**What the reader uses.** Timestamps, the working directory, assistant token
usage (each message ID counted once), the recap, the AI and custom titles,
pull request and merge request links, and the cost. It never reads or sends
the text of Claude's replies or of your prompts. The recap is a system entry,
not a reply.

**Bad lines.** A line that isn't valid JSON, or is longer than 1 MB, is skipped
and counted. If more than half the lines in one run are bad, the run sends
nothing and keeps the old state.

**Limits.** `Builder.Build` clamps the digest to the server's limits so one
corrupt entry can't make the server reject every later digest. It sets
`as_of` to the current time when it is more than 5 minutes ahead, drops
`first_at`, `recap_at`, and `cost_at` that fall more than 5 minutes after
`as_of`, drops a `cost_usd` outside 0 to 100000, and clamps each token count to
0 to 10^12. It also keeps the JSON body under 15 KiB, below the server's
16 KiB cap: it drops the oldest links first, then cuts the recap. `sessionhub digest
<prefix>` accepts the 8-character ID that `sessionhub ls` prints and resolves it
through the server.

**Git.** `Git` runs `git log` with `log.showSignature=false` and skips any
output line that doesn't start with a hex sha and a NUL. A digest without `git`
doesn't erase the git data the server already has.

**Hook and watcher.** The hook child sends its stderr to `/dev/null`, so a run
refused for too many bad lines is silent on the hook path. Only the watcher logs
it. The hook child and the watcher can see different `CLAUDE_CONFIG_DIR`
values. If they differ, each keeps its own file offsets in the state file,
and tokens stay deduplicated by message ID.

**Sending.** `Send` gives the PUT 5 seconds. On a network error, timeout, 408,
429, or 5xx, or when the client can't be built, it appends a `digest` item
(`client.OpDigest`, the `api.DigestIn` body, with `session_id`) to the queue.
Other 4xx responses are returned as errors and aren't queued. `Replay` sends a
`digest` item with `PutDigest` and treats one without `session_id` as
permanently unsendable.

## `internal/gitinfo`

`Lookup(ctx, cwd) (repo, branch string)`: `git -C cwd remote get-url origin`
and `git -C cwd rev-parse --abbrev-ref HEAD` under a 500 ms budget. Returns
empty strings on any error. `rev-parse` fails in a repo with no commits, so
the branch is empty there.

## `internal/herdr`

- `SocketPath()`, `SessionName(socketPath)`: `HERDR_SOCKET_PATH`, else the
  default; the session is `default` or the `<name>` in
  `.../sessions/<name>/herdr.sock`.
- `Dial(path)`, then `Snapshot()`, `PaneGet(id)`, `PaneFocus(id)`,
  `PaneSplit(PaneSplitParams)`, `AgentStart(AgentStartParams)`,
  `ReportMetadata(ReportMetadataParams)`, `SetSummary(paneID, text)`
  (`source "sessionhub"`, token `hub_summary`, 80 runes max, empty clears). Each call
  has a 2 s deadline. Error responses are `*herdr.Error{Code, Message}`, for
  example `pane_not_found`.
- Remote Control uses four more calls:
  - `AgentPrompt(target, text)` (`agent.prompt`) submits text to the agent in
    a pane. A blocked agent returns `*herdr.BlockedError` (code
    `agent_blocked`), and herdr sends no input.
  - `PaneRead(paneID, source, lines)` (`pane.read`) returns the pane's text:
    `SourceRecentUnwrapped` (`recent_unwrapped`, scrollback with soft wraps
    joined) or `SourceVisible` (`visible`, the screen as drawn). A `lines` of
    0 or less sends no limit. The text is another process's terminal output:
    clean it before printing.
  - `SendKeys(paneID, keys...)` (`pane.send_keys`) presses keys by herdr's
    names, such as `esc`. Check the pane first: it goes to whatever runs
    there.
  - `WorkspaceCreate(WorkspaceCreateParams)` (`workspace.create` with `cwd`,
    `label`, `focus`) returns the workspace ID and root pane. `Focus` false
    leaves the user's focus alone. A result without a workspace or root pane
    is an error, so no caller starts an agent in pane "". Captured in
    `workspace-create.ndjson`.
- `NotificationShow(p)`: `notification.show` with `title`, `body`, and
  `sound` (`none`, `done`, `request`). herdr's `shown: false` comes back as
  an error that names the reason.
- `ReportWorkspaceMetadata(p)`: `workspace.report_metadata` with
  `workspace_id`, `source`, and `tokens` (a nil value clears a token). The
  watcher's sidebar count uses it. `Snapshot().Workspaces` lists each
  workspace's ID and label.
- `TabList()`, `TabFocus(id)`, and `TabCreate(TabCreateParams)` (`tab.list`,
  `tab.focus`, `tab.create`) let the inbox pane find or open the
  `sessionhub:<machine>` tab. `tab.create` returns the tab and its root pane; a
  result without either is an error. It cannot run a command, so the caller
  runs `herdr pane run <pane> <command>`.
- **One request per connection.** herdr 0.9.3 closes the connection after it
  answers one request; a second request on the same connection fails with a
  broken pipe. `Client` opens a fresh connection per call, so `Dial` only
  checks the socket and `Close` does nothing.
- Types decode leniently: `PaneInfo`, `AgentSessionInfo`, `AgentInfo`, and
  `Snapshot` keep only the fields sessionhub reads. `PaneInfo.SessionID()` returns
  the Claude UUID from `agent_session.value`, or "".
- Events: `EventFromEnv()` parses `HERDR_PLUGIN_EVENT_JSON` (`ParseEvent` for a
  byte slice) into `Event{Event, Data}`; dotted names are normalized to
  snake_case. Helpers: `PaneCreated`, `PaneClosed`, `PaneExited`,
  `AgentDetected`, `AgentStatusChanged`. A helper called on the wrong event
  kind returns an error.

Not verified against a real capture (see `testdata/README.md`): the
`pane_exited` payload and the `agent.start` response. `pane.split` is captured (`pane-split.ndjson`).
Their types follow herdr's schema (`herdr api schema --json`).

## `internal/herdr/herdrtest`

`herdrtest.New(t, "session-snapshot.ndjson", ...)` starts a fake herdr on a
temp Unix socket. It loads request and response pairs from
`testdata/herdr/socket/`, matches on method plus params, echoes the caller's
request ID, answers one request per connection like herdr, and answers
`no_fixture` for anything else. `Requests()` returns what it received. With no
names it loads every fixture (the later name wins on identical requests).
`Handle(method, fn)` answers one method from a Go function instead of the
fixtures, for a request that must get different answers over time, such as a
`pane.read` before and after a prompt. `fn` returns a result, or an error
code.

`RequestsFor(method)` returns the requests the fake got for one method, in
order. `Handle(method, fn)` answers a method that has no captured fixture,
such as `notification.show`.

## Live check

`SESSIONHUB_LIVE_HERDR=1 go test -run TestLiveReadOnly -v ./internal/herdr` runs
`Snapshot` and `PaneGet` against the real socket. It never focuses, splits,
starts an agent, or writes metadata.
