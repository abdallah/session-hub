# herdr plugin

The plugin registers the Claude Code sessions running in herdr with the sessionhub
server and keeps their state current. It targets herdr 0.9.3. Code:
`internal/plugin`.

## Install

```
make install          # puts the binary at ~/.local/bin/sessionhub
sessionhub install-plugin
```

`sessionhub install-plugin`:

1. Writes `~/.local/share/sessionhub/herdr-plugin/herdr-plugin.toml`. Every command
   in it is the absolute path of the installed binary, `~/.local/bin/sessionhub`
   (the same path the hook entries and the MCP registration use), because
   herdr runs plugin commands without a shell and with its server's `PATH`.
   It names that path even when you run another copy, such as
   `./bin/sessionhub install-plugin`. If `~/.local/bin/sessionhub` doesn't exist, the
   command fails and tells you to run `make install` first; it writes and
   links nothing.
2. Runs `herdr plugin link` on that directory. herdr reads the manifest at link
   time, so the command relinks (`unlink`, then `link`) when the manifest
   changed or the plugin is linked from another directory, and does nothing
   when it is already linked with the same manifest. If `unlink` or `link`
   fails, it puts the previous manifest back (or removes a new one), so the
   next run sees the change again and relinks.
3. Stops a running watcher (it may be an older binary), then runs the startup
   hook once. herdr runs `[[startup]]` only when its server starts, not on
   link, so without this step nothing is registered until the next herdr
   restart.

`sessionhub uninstall-plugin` runs `herdr plugin unlink sessionhub`, removes the plugin
directory, and stops the watcher. It keeps the state directory, because the
Claude Code hooks share its queue. Both commands are idempotent.

The client config (`~/.config/sessionhub/config.toml`, see `docs/client.md`) must
exist: herdr runs plugin commands with its server's environment, so the
plugin reads the default config file, not your shell's `SESSIONHUB_*` variables.

## What each hook does

| Hook | Command | Does |
|---|---|---|
| `[[startup]]` | `sessionhub plugin startup` | `session.snapshot` over the herdr socket → `PUT /v1/machines/self/herdr-sessions` with every pane that runs a live Claude session (see below). If the send fails, the body is queued. Then starts the watcher. |
| `pane.agent_detected` | `sessionhub plugin event` | Queues an upsert for the pane. The watcher fills in the session from its snapshot. |
| `pane.agent_status_changed` | `sessionhub plugin event` | Queues a `state_changed` event with `agent_state` in the payload. When the new status is `blocked`, it also shows a herdr notification with a sound (see below). |
| `pane.closed`, `pane.exited` | `sessionhub plugin event` | Queues a `pane_closed` event. The server marks the session `ended`. |
| action `status` | `sessionhub status` | Prints this machine's sessions. |
| action `resume` | `sessionhub plugin open-picker` | Opens the `resume-picker` overlay pane (`sessionhub resume --pick`). |
| action `inbox` | `sessionhub plugin open-inbox` | Opens the `inbox` pane (`sessionhub inbox --watch`) split to the right. Bind it to a key like the picker. |

`startup` and `event` always exit 0. `sessionhub plugin event` makes no network call:
it parses `HERDR_PLUGIN_EVENT_JSON`, appends one line to the queue, and starts
the watcher if none is running. Events for agents other than Claude are
ignored in v1.

When a Claude pane's status becomes `blocked`, `sessionhub plugin event` also calls
herdr's `notification.show` over the socket (the same as `herdr notification
show "Blocked: <title>" --body "<machine>" --sound request`). `<title>` is the
pane's terminal title, cleaned and cut to 80 characters, or `Claude`;
`<machine>` is the client config's `machine`, else the host name. It runs in
the herdr where the session lives, so you hear it on that machine. If herdr
refuses (for example `rate_limited` or `busy`), the hook writes one line to
`watcher.log` and carries on. When herdr shows nothing because notifications
are `disabled` or there is no foreground client (`no_foreground_client`), the
hook logs nothing: that is not an error.

A pane counts as a live Claude session only when herdr has detected the
agent (the pane's `agent` is `claude`) and its `agent_session` has
`kind == "id"` and a value. If `agent_session.agent` is set, it must be
`claude` too. `title_hint` is herdr's `terminal_title_stripped`.

A pane where herdr detects Claude but reports no session ID is sent in the
heartbeat's `unidentified_panes`. herdr does this for some resumed sessions
(seen live on `tower`, pane `w4:p1`). The server then keeps the session that
the hooks registered in that pane, instead of ending it every minute as
missing from the snapshot.

A pane that keeps its `agent_session` without a detected agent is at a shell
prompt, so it is left out. herdr keeps `agent_session` on a restored pane
(captured: `w8:p1`, agent status `unknown`, the shell's title); counting it
made every heartbeat revive a session the server had ended. On a clean
`/exit`, herdr 0.9.3 drops both `agent` and `agent_session` from the pane
(checked live, see `docs/dev/evidence/final-fix-B.md`). Events for a pane that is
not live wait unresolved and are dropped after 10 minutes.

## Watcher

`sessionhub plugin watch` is started detached (`setsid`, stdin and stdout at
`/dev/null`, stderr appended to `watcher.log`) by `startup` and by every
`event` call whose non-blocking `flock` probe finds `watcher.lock` free. The
watcher takes the lock itself and holds it for its lifetime, so when two calls
race, the second watcher exits. Because every `event` call's probe holds the
lock for an instant, a new watcher retries the lock for about 200 ms before it
concludes another watcher holds it.

Every 2 seconds it:

1. Peeks at the queue. When a pane item names a pane its cache doesn't know,
   or was queued after the cache last saw that pane, it takes one fresh
   snapshot (at most once per tick) before it goes on. The second case
   catches `/clear`: the pane keeps its ID but runs a new session, so an older
   cache entry would send the item to the old session. No herdr or git call
   runs while it holds the queue lock.
2. Takes the queue lock (`Queue.Drain`) and coalesces per pane: the latest
   `state_changed` wins, `pane_closed` beats earlier state, `pane.closed` and
   `pane.exited` for one pane send one event, and only the latest pane upsert
   and the latest queued `herdr_sessions` body are kept. Superseded lines are
   removed. Items that name a session (from the Claude Code hooks or the MCP
   server) are keyed by session instead, and their upserts are never dropped:
   a hooks upsert can carry `first_prompt`, which the server records once.
3. Resolves pane → Claude session from its cache. Closed panes stay in the
   cache for 10 minutes so their `pane_closed` still resolves. Items are keyed
   by herdr session as well, because pane IDs repeat across herdr servers.
4. Sends each item with `client.Replay`, still under the queue lock, for at
   most 1 second in total (a deadline on the pass). Items it doesn't reach stay
   queued for the next tick. The deadline keeps the lock short, so a
   concurrent `sessionhub plugin event` or Claude Code hook waits at most about 1
   second to append.
   - A retryable failure (network, timeout, 408, 429, 5xx) stops the pass and
     keeps that item and every later one.
   - An auth failure (401 or 403: the token is wrong or revoked) also stops
     the pass and keeps everything. Fix the token and the queue drains.
   - Any other 4xx drops the item with a log line.
   - On a 404 for an event, a report, or a title, the watcher upserts the
     session and retries once. The upsert carries what the pane gives (cwd,
     herdr IDs, git info, title hint) when a cached pane runs that session,
     else only the ID, agent `claude`, and source `plugin`.
5. Drops pane items it still can't resolve after 10 minutes (for example a
   Claude that never got past the folder-trust prompt, so herdr never learned
   its session ID).

Every 60 seconds, and once at start, it sends a heartbeat: snapshot →
`PUT /v1/machines/self/herdr-sessions`. This refreshes `last_seen_at`, picks
up session ID changes after `/clear`, and ends sessions whose pane vanished
without an event. A failed heartbeat is logged, not queued.

After each heartbeat the server accepted, the watcher reads
`GET /v1/instructions` and rewrites `<state>/instructions.json` when the
`version` changed. `sessionhub hook context` prints that copy into every prompt. A
failed read keeps the old copy and logs `rules: refresh failed`, at most
once every 5 minutes. The refresh runs after the heartbeat is sent, so it
never delays or fails the heartbeat.

After a heartbeat the server accepts, the watcher reads `GET /v1/inbox` and
writes the count on every workspace of its herdr session, as the
display-only workspace token `inbox` (source `sessionhub`, herdr's
`workspace.report_metadata`): the total, plus ` · <n> blocked` when any are
blocked, for example `3 · 1 blocked`. An empty inbox clears the token. A
workspace whose text didn't change since its last report gets no call; a
report herdr refuses is retried at the next heartbeat. If the inbox read
fails, the sidebar keeps the last count. herdr shows the token where you put
`$inbox` in `[ui.sidebar.spaces]` rows in `~/.config/herdr/config.toml`:

```toml
[ui.sidebar.spaces]
rows = [["state_icon", "workspace", "$inbox"], ["branch", "git_status"]]
```

After each heartbeat, the watcher digests up to 10 Claude sessions whose
transcript size or modification time changed since their last good digest.
This includes panes that closed within the 10-minute cache window, so the
last turn of a session is digested too. A digest that fails is logged as
`digest <id>: <error>` and retried on the next heartbeat. The digests run
after the heartbeat is sent and never delay it.

It exits when the herdr socket disappears or is replaced (its device and inode
change), and on `SIGTERM`. The next herdr server runs `[[startup]]` and starts
a new watcher.

Delivery is at least once. The queue file is rewritten once, at the end of a
pass. If the watcher is killed mid-pass, the file is unchanged, so the next
watcher resends the items that pass already sent (at most 1 second of sends).
Upserts are idempotent; an event can be recorded twice in that window.

### Control loop

Beside the 2-second ticks, the watcher keeps one long poll open on
`GET /v1/machines/self/control?wait=30`. It never takes the queue lock.

- For each claimed request it calls `resume.Control` with the herdr socket
  and posts the result:
  - **herdr detects Claude running the session** (in its recorded pane, or
    any pane in a fresh snapshot): send `/remote-control` with
    `agent.prompt`, read the pane for up to 15 seconds for a link that wasn't
    there before, and report `done` with it, or `done` with "sent, link not
    seen". If Remote Control was already on, Claude shows a dialog; the
    watcher takes the link from it and closes it with **Esc**, unless
    Claude is working (Esc would interrupt the turn). If the dialog is
    already open before the watcher sends anything, it reports `failed` and
    sends nothing.
  - **`agent_blocked`:** `failed`, "waiting on a prompt in the pane". Nothing
    is typed.
  - **No pane runs it, and the sessionhub says it ended or is stale:** create a
    workspace in the session's directory without focus, labeled
    `sessionhub: <title>` (the title cut to 40 characters, or the short ID), start
    `claude --resume <id> --remote-control` there with `agent.start`, and
    wait for the link the same way, counting only a new
    `/remote-control is active` line. A missing or relative directory is
    `failed`, and nothing is created.
  - **No pane runs it, but the sessionhub says it is live or blocked:** `failed`,
    "looks live but pane not found". It never starts a second copy.
- The watcher remembers, for 2 minutes, the pane it resumed a session in,
  because herdr reports the session there only some seconds after Claude
  starts. A second request in that window never resumes again: it sends
  `/remote-control` to that pane when Claude runs there, and otherwise
  reports `failed`, "just resumed; try again in a moment".
- A claim whose action is `message` carries the prompt in `text`. The
  watcher reads the session's pane with `pane.get`. Only when herdr reports
  the agent `idle` or `done`, and the pane runs that session, does it send
  the text with `agent.prompt` (newlines included, as one prompt), and it
  reports `delivered`. A `working` or `blocked` agent, an agent with no
  reported status, a pane whose session ID herdr has not learned yet (its
  `agent_session` is missing or not of kind `id`; herdr learns it a few
  seconds after Claude starts), herdr's `agent_blocked` error, any other
  herdr error from `pane.get` except `pane_not_found`, or a herdr socket
  that does not answer is `busy`: the message stays queued and the server
  offers it again 15 seconds after the last offer. No pane, a pane that is
  gone (`pane_not_found`), another session in the pane, or no agent is
  `refused`. The log line never holds the message text.
- The watcher gets no messages for a session whose Claude Code mod polled in
  the last 2 minutes, or for a session without a herdr pane: the mod takes
  those itself (see `docs/cli.md`, "`sessionhub mod`").
- A claim whose action is `start` carries a start request in `start` and no
  session. Unless the machine set `remote_start = false` (or
  `SESSIONHUB_REMOTE_START=off`; the watcher reads the config for each
  request, and refuses when it can't), it expands `~` in the directory, resolves symbolic links, and
  checks that the directory exists inside the home directory; otherwise it
  reports `failed` and creates nothing. Then it creates a workspace there
  without focus, labeled `sessionhub: new in <directory name>`, starts
  `claude --remote-control` with `agent.start` (agent name
  `sessionhub-new-<8 characters of the request ID>`), waits up to 15 seconds
  for herdr to detect Claude and up to 15 more for a
  `/remote-control is active` line, and reports `done` with the link, or
  `done` with "started in a new herdr workspace, link not seen". With a
  first prompt, it then waits up to 15 seconds for the agent to be `idle`
  and sends the prompt with `agent.prompt`; a `blocked` agent (a folder
  Claude asks to trust) or a timeout leaves it unsent and adds why to the
  detail. It never answers a prompt in the pane.
- The idle check and `agent.prompt` are two herdr calls, so the agent can
  change state in the milliseconds between them. If you type into the
  session, or it starts working, in that window, the message is typed in
  anyway: Claude takes it as the next prompt, or it lands in your input
  line. herdr's `agent_blocked` error still stops it when a permission
  prompt is open.
- After a network error the loop waits 5 seconds, doubling to 60. After
  `401` or `403`, or when the client config can't be read or has no
  `server_url`, it logs and waits 5 minutes. A `204` that came back in under a second waits 1 second,
  so a server that is shutting down can't spin the loop.
- It posts the result with a 2-second limit, also when the watcher is
  stopping meanwhile, and the watcher waits for the loop before it exits.
  `sessionhub install-plugin` waits up to 7 seconds for the old watcher to exit.
- A result the server refuses (the request expired meanwhile) is logged, not
  retried. The dashboard then says the machine didn't respond in time.
- Log lines start with `control:`. `control: waiting for Remote Control
  requests` means the loop runs, and each request logs
  `control: request <id> for session <id>: <state> <detail>`, or
  `for a new session` for a start request.

### Moves

The watcher does both ends of a move (see `docs/server.md`, "Moves"). At
start it loads or creates `~/.config/sessionhub/move.key` and, after its first
accepted heartbeat and every hour, registers the public key with
`PUT /v1/machines/self/move-key`; `move: key <fingerprint> registered` is
logged once. Without a usable key file it logs why and fails every move
step it is given with "this watcher has no move key".

Move claims arrive on the same long poll as Remote Control and messages, and
the loop runs one at a time, so a move (push, `/exit`, upload) holds other
requests back until it ends.

Every detail and `watcher.log` line a move writes is cleaned of terminal
escapes, and of the user and password in any URL (`https://user:token@host/`
becomes `https://host/`), since Git's messages can name a remote with its
credentials.

**Source (`move-out` while `packing`).** Each step's name starts the
`failed` detail. Every step that can fail while the session runs comes
before `/exit`:

1. `check pane`: herdr must report the session's pane running that session
   with the agent `idle` or `done`, the same checks as message delivery.
2. `read repo`: the session's directory must be in a Git work tree with an
   `origin` remote and a branch checked out.
3. `build bundle`: `claude --version`, then a trial build of the bundle (see
   `docs/client.md`, "Moves"), so a missing transcript or a size limit fails
   while the session still runs.
4. `push`: the branch goes to `origin`, the remote the target clones from,
   with SSH in `BatchMode`. With no upstream it is `git push -u origin
   <branch>`; with an upstream on `origin`, HEAD is pushed to it when ahead;
   with an upstream on another remote (a fork's `upstream`, or `.` for a
   local branch), `<branch>` is pushed to `origin` without `-u`, so the
   tracking stays as it is. It never forces and never skips hooks.
5. `check pane` again, since a push can take minutes, then `end session`.
   The watcher first reads the move: if it is no longer `packing` (it timed
   out during the push), the session keeps running and nothing more
   happens; if it can't be read, the step fails. Then it types `/exit` with
   `agent.prompt` and waits up to 30 seconds for herdr to report the pane
   gone, no agent in it, or another session there. Claude that herdr shows
   with no session ID has not left.
6. `build bundle`, `seal`, `upload`: the real bundle, sealed for the
   target's key, streamed to `PUT /v1/moves/{id}/bundle`. The upload moves
   the move to `uploaded`; the watcher posts nothing more.

If the upload fails, the watcher reads the move back. Still `packing` means
the bundle did not land, and the step fails. Any other state means it
landed. If the move can't be read, the outcome is unknown: the watcher
posts nothing and leaves the session ended, and the move's finish restarts
it once the move times out.

A step that fails after `/exit` is posted with a detail that ends
`; the session resumes here`. Only once the sessionhub records that failure does
the watcher restart the session with `claude --resume <id>` in its old pane
(which sits at a shell) or a new workspace; if a pane still runs it, or
its old pane shows Claude with no session ID yet, nothing starts. If the
answer to that failure is lost, the watcher reads the move back: a move that
is `failed` with the detail it posted was recorded after all, and the
session restarts. A failure the sessionhub did not record (the move ended meanwhile,
or the sessionhub is out of reach) leaves the session ended until the finish, so
the session never runs on two machines. In the rare case that the sessionhub
recorded the failure and then stays out of reach, no finish comes, and the
session stays ended until you resume it with `sessionhub resume`. A restart that fails adds
`restart failed: <why>` to the move's detail. A note on a `cancelled` move
(a state no route sets yet) is posted as `failed`, the state every reader
gives it.

**Finish (`move-out` once the move ended).** On `done`, the watcher moves
the transcript, its sidecar folder, and the file history to
`<state>/moved/<move id>/`, so the session can't be resumed here as well;
archives older than 30 days are removed then. The transcript moves last, so
after a partial failure it is still in place and the next try moves the
rest. It tries 3 times; if the
archive still fails, it adds `archive on <machine> failed: <why>; remove
the transcript there by hand` to the move's detail, which `sessionhub move
--status` and the dashboard show. On `failed` (the target's failure, or a
timeout), it restarts the session as above, unless a pane already runs it.
The finish arrives through the long poll, so a watcher that restarted
mid-move still gets it.

**Target (`move-in` while `unpacking`).** Steps, in this order:

1. `download`, `open`: the sealed bundle from `GET /v1/moves/{id}/bundle`,
   opened with this machine's key and the source's registered key (a bundle
   sealed by anyone else fails), and checked: entry names, the manifest's
   move, session, and source.
2. `claude version`: `claude --version` here must equal the source's.
3. `transcript`: no transcript of the session may exist in any project
   folder under `~/.claude/projects` here.
4. `find clone`: a Git repository under `~/Code` or a `move_roots` entry, at
   most 4 levels down, whose `origin` is the same remote (scheme, user,
   `.git`, and host case ignored). With several, the one whose path under
   its root matches the source's, ignoring case (`OTGS/app` and `otgs/app`);
   with none or several there, the step fails and lists the clones.
5. `check clone`: no uncommitted tracked change, and none of the carried
   untracked files, and no file the patch creates, already there. When the
   checkout would switch the clone to another branch, no herdr pane here may
   run an agent whose directory (`cwd` or `foreground_cwd`, links resolved)
   is in the clone: its next commit would land on the moved branch. The step
   then fails with `check clone: <clone> is in use by a session in pane
   <pane>`.
6. `checkout`: `git fetch origin`, check out the branch (a tracking branch
   from `origin` if there is no local one), fast-forward it to the source's
   HEAD. A branch with commits the source lacks fails; nothing is rewritten.
7. `apply`: `git apply --binary` of the changes (the patch file sits in the
   clone's Git directory, mode `0600`, while it applies), then the untracked
   files, never over an existing file and never through a linked folder
   (each folder on the way is checked with `lstat`). The session's
   directory, with links resolved, must be inside the clone, else the move
   fails with `unpack: <path> leaves the clone`.
8. `write transcript`: the transcript and sidecar folder into
   `~/.claude/projects/<folder of the new directory>/`, the file history into
   `~/.claude/file-history/<id>/`.
9. `start`: the watcher reads the move again and starts only while it is
   still `unpacking` (a move that timed out goes back to the source). Then
   `claude --resume <id>` in a new workspace at the session's directory in
   this clone, and `done`. The detail lists the files the source did not
   carry (`not carried: .env`): copy those by hand. If the sessionhub does not
   record the `done`, and the move does not read back as `done`, the watcher
   stops Claude (see below), removes the transcript it wrote, and puts the
   clone back, so the session never runs on both machines.

To stop Claude in the new workspace's pane, the watcher types `/exit` there
if herdr showed Claude in it, then closes the pane with herdr's
`pane.close`. Claude counts as stopped only once herdr no longer has the
pane: a pane with no agent proves nothing when herdr never detected Claude
there, because Claude may still be starting. In a pane that already ran the
session (one the watcher did not create), `/exit` must end the Claude that
herdr showed there.

When the watcher can't stop Claude that way (the close fails, or the pane
stays), Claude may still run here, so the watcher keeps the transcript and
the clone as they are and treats the session as here:

- If the start failed (herdr did not detect Claude, or a pane already runs
  the session), it posts `done` with the detail `start: <why>; Claude may be
  running in pane <pane> on <machine>; check it there`. The source archives
  its copy instead of restarting it.
- If the sessionhub did not record the `done`, it logs `session ... still runs here
  in pane <pane>; end it before the move times out or it also restarts on
  <source>` and posts `done` again, with pauses of up to 30 seconds, until
  the sessionhub records it or 10 minutes after the claim, when the move times
  out.

Steps 1 to 5 change nothing. A failure from step 6 on takes back what the
move did, and only that: it removes the untracked files it wrote (a file
changed since stays, and the detail names it) and the folders it made,
reverses the patch with `git apply -R`, checks out the branch the clone was
on, and puts the moved branch back where it was: a fast-forwarded branch
returns to its old commit, and a tracking branch the move created is
deleted. Both ref updates are compare-and-swap, so a change made since is
kept. The watcher never runs `git reset` or `git clean`, so work in the
clone that is not the move's survives. When the move changed the clone, the
detail ends with `; the clone is back on <branch>`, or with `; putting the
clone back failed: <why>`. A `checkout` failure puts the clone back itself
and adds only the second note when that fails. The source then gets the
failure as its finish and restarts the session.

**Cloud (`move-out` to `cloud`).** Steps 1 and 2 of the source run as for
a machine move, and the remote must be on `github.com` (`cloud`) before
anything changes. Then, while the session still runs:

- `push`, as for a machine move, and `start cloud` checks `claude
  --version`.
- `cloud branch`: when there are uncommitted changes or untracked files,
  the watcher builds one commit on top of HEAD in a temporary index
  (`GIT_INDEX_FILE`, `read-tree`, `add`, `write-tree`, `commit-tree`), so the
  working tree, the index, and your branch don't change, and pushes it to
  `sessionhub/cloud-<first 8 of the session ID>`, a branch only sessionhub writes, with
  `--force-with-lease` against the commit sessionhub pushed there before. If
  anything else is on that branch, such as a cloud session's commits, the
  move fails instead of dropping them. Secret-looking files, tracked or
  untracked, links, and untracked files over 10 MiB stay out; the `done`
  detail lists the files left out. A file you staged but never committed is
  carried, unless `.gitignore` ignores it (`git add -f`): commit such a
  file first, or it stays here. The commit is never signed, and the push
  leaves `refs/remotes/origin/sessionhub/cloud-<id8>` in your clone. With no
  changes it uses your branch.

Then `check pane` again and `end session`, as in step 5 of a machine move
(including the read of the move), and:

- `start cloud`: `claude --cloud "<hand-off prompt>"` in the session's
  directory, with the prompt as one argument. `claude --cloud` refuses to
  run without a terminal, so the watcher runs it under util-linux `script`,
  which gives it a pseudo-terminal; the prompt reaches it through an
  environment variable, never as shell text. The watcher
  takes the first `https://claude.ai/code/session_...` link the command
  prints, stops it, and reports `done` with the link; it gives up after 2
  minutes. The sessionhub ends the local session with reason `moved_to_cloud`. The
  local transcript stays, so `claude --resume` still works here; the cloud
  session is a new one.

Once `claude --cloud` may have created a session (it printed a session link,
printed any other `claude.ai` link, or exited 0), the watcher writes
`~/.local/state/sessionhub/moved/<move id>/cloud` (mode `0600`) with the link, or
`unknown`, before it posts anything. Then:

- With a session link, it posts `done` with the link, retrying with growing
  pauses for up to 10 minutes (the step timeout) until the sessionhub records it
  or the move ends otherwise.
- With any other `claude.ai` link, or none after a clean exit, the step
  fails with a detail that says a cloud session may exist, and the session
  is not restarted here.
- If the move ends without that `done` (it timed out), the finish finds the
  marker, adds `cloud session started (<link, or "link unknown">); not
  restarted here` to the move's detail, and does not restart the session,
  so it never runs here and in the cloud at once. Resume it here by hand
  with `claude --resume <id>` if you want both.

The hand-off prompt:

    Continue work moved from a local Claude Code session (<id8> on <machine>).
    Branch: <branch>.
    Recap: <recap, or none>
    Last request: <last prompt, or none>
    Status: done: ... / in flight: ... / waiting on: ...

The `Status` line is left out when the session has no report. A cloud
session needs a claude.ai sign-in, GitHub access through the Claude GitHub
App or `/web-setup`, and an organization that allows cloud sessions; when
`claude --cloud` fails, its last line is the detail and, once the sessionhub
records the failure, the session restarts here. Bring a cloud session back
with `claude --teleport <id>`.

Log lines start with `move:`. Results are posted to
`POST /v1/moves/{id}/result`, retried up to 5 times on a network error or a
`5xx`. After the move ended, the same route takes the source's notes.

## Files in the state dir

`~/.local/state/sessionhub/` (env `SESSIONHUB_STATE_DIR`):

| File | What |
|---|---|
| `queue.jsonl`, `queue.lock` | The shared retry queue (see `docs/client.md`). |
| `watcher.lock` | `flock` held by the running watcher. Its content is the watcher's PID. |
| `instructions.json` | The standing rules, as the server last returned them. The watcher and `sessionhub hook refresh-instructions` write it; `sessionhub hook context` reads it. |
| `watcher.log` | The watcher's stderr. Truncated when the watcher starts if it is over 1 MiB, and checked on every tick. An identical failure line is repeated at most every 5 minutes. |
| `moved/<move id>/` | The transcript, sidecar folder, and file history of a session moved to another machine, kept 30 days. |

## Troubleshooting

- See what herdr ran and what the hooks printed:
  `herdr plugin log list --plugin sessionhub`.
- See what the watcher did: `tail ~/.local/state/sessionhub/watcher.log`.
- Check that the watcher runs: `pgrep -af 'sessionhub plugin watch'`, or
  `cat ~/.local/state/sessionhub/watcher.lock` for its PID.
- See what is waiting to be sent: `cat ~/.local/state/sessionhub/queue.jsonl`.
- If sessions don't appear, check `~/.config/sessionhub/config.toml`: without a
  `server_url` the watcher keeps everything queued and logs
  `send skipped: sessionhub: server_url is not configured`.
- After replacing the binary, run `sessionhub install-plugin` again to restart the
  watcher on the new binary.
- If a resume fails with "claude didn't start", check the machine's interactive
  shell startup, for example a zsh `compaudit` prompt. The fix is
  `compaudit | xargs chmod g-w,o-w`.
- If the dashboard shows no **Remote Control** button for a session in herdr,
  the machine's watcher isn't polling. Look for `control:` lines in
  `watcher.log`; a `401` means the token is wrong.
- If a card says "resumed in a new herdr workspace, link not seen", Claude
  started in the new `sessionhub: <title>` workspace, but no Remote Control banner
  appeared within 15 seconds. Read that workspace's pane for the link. The
  watcher waits up to 15 seconds, twice: first for herdr to report Claude in
  the new pane, then for the banner. If Claude never starts, the card shows
  "claude didn't start" instead (see above).

## Limits

- One watcher per machine: `watcher.lock` is per state dir, not per herdr
  session. If you run a second herdr server (`herdr --session <name>`) at the
  same time, only the herdr session whose hook started the watcher gets
  heartbeats. Events from the other server stay unresolved and are dropped
  after 10 minutes, never misattributed.
- `pane.exited` never fired in the capture run (`testdata/README.md`); it is
  handled like `pane.closed` per herdr's schema.
