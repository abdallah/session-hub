# CLI

The `sessionhub` client commands read from the sessionhub server and get you back into a
session. They use the client config (`~/.config/sessionhub/config.toml`, env
`SESSIONHUB_CONFIG`, `SESSIONHUB_SERVER_URL`, `SESSIONHUB_TOKEN`, `SESSIONHUB_MACHINE`) and the 2-second
HTTP timeout from `docs/client.md`.

## Commands

| Command | What it does |
| --- | --- |
| `sessionhub ls [--all] [--machine M] [--grep TEXT]` | Table of machine, short ID, title, status, age (since last seen), and the latest report's summary. Without `--all` it lists `live` and `blocked` sessions only. `--grep TEXT` keeps the sessions whose title, recap, last prompt, branch, directory, or machine contains `TEXT`. The match is plain text and ignores case, so `[build]` is not a pattern. To search ended sessions, combine `--grep` with `--all`. If no live session matches but ended or stale ones do, the command says how many and suggests `--all`. |
| `sessionhub show <id-prefix>` | One session in full: fields (including `recap:`, `last prompt:`, and a one-line `summary:` of commits, latest merge request, and cost), the session digest, report history, and the last 50 events, newest first. The digest lists up to five commits made during the session, the links the session touched, token totals (input, output, cache read, cache write), the cost with its `as of` time, and when the digest was taken. |
| `sessionhub status` | Server URL, machine name, server health, and this machine's non-ended sessions. The `sessionhub.status` plugin action runs it. Exits non-zero when the server is unreachable. |
| `sessionhub login --name <name> [--no-qr] [--json]` | Create a one-time link that signs a browser in to the dashboard as `<name>`, and print the link, when it expires, and a QR code of it. `--no-qr` omits the QR code. `--json` prints only `{"url":...,"name":...,"expires_at":...}`. See below. |
| `sessionhub login ls [--json]` | Signed-in browsers: name, ID, machine, created, last used, and expiry. |
| `sessionhub login rm <name\|id>` | Sign a browser out. It matches a name first, then an ID. No confirmation. |
| `sessionhub inbox [--json]` | The sessions that need you, across machines, under the headings **Blocked**, **Waiting on you**, and **Finished**, oldest first in each. One line per item: ID prefix, title, machine, how long ago it arrived, `stale` for a stale session, and what it waits on (for a blocked session with a permission request, `asks to use <tool>: <command or file>`) or its recap, cut to the terminal width. `--json` prints the server response. See below. |
| `sessionhub inbox --watch` | The same list, full screen, refreshed every 5 seconds, with a cursor you move with the keyboard. See below. |
| `sessionhub inbox dismiss <id\|prefix>` | Hide an inbox item until something new happens in that session. |
| `sessionhub inbox snooze <id\|prefix> <1h\|4h\|tomorrow\|duration>` | Hide an inbox item for a while. `tomorrow` means 9:00 local time the next day. A Go duration such as `90m` also works, up to just under 7 days (167h59m). |
| `sessionhub rules [ls]` | The standing rules every session sees, one per line: number, text, and who added it. See below. |
| `sessionhub rules add "<text>"` | Add a rule, 1 to 300 characters on one line. The list holds at most 2,000 characters. |
| `sessionhub rules rm <id>` | Remove rule `<id>`. |
| `sessionhub send <id-or-prefix>... -m "<text>"` | Send a message to sessions. See below. |
| `sessionhub send --machine <M> -m "<text>"` | Send a message to every live session in herdr on `<M>`. |
| `sessionhub approve [--yes] <request-id\|prefix>` | Allow a pending permission prompt once, after it shows the request and asks. See below. |
| `sessionhub deny [--yes] <request-id\|prefix> ["reason"]` | Deny a pending permission prompt, with an optional reason Claude sees. |
| `sessionhub move <id-or-prefix> <machine\|cloud>` | Move a session to another machine, or hand it to a Claude Code cloud session, and print its progress for up to 3 minutes. See below. |
| `sessionhub move --status <move-id>` | Print where a move is. |
| `sessionhub move-key` | Print this machine's move key fingerprint (16 hex characters), creating the key if there is none. |
| `sessionhub start <machine> [--dir D] [--trust] [-m "<prompt>"]` | Start a new Claude session on `<machine>`, with Remote Control on, in a new herdr workspace, and print its link. See below. |
| `sessionhub resume [--force] [--remote-control] <id-prefix>` | Get back into a session. See below. `--force` starts a session the server reports `live` or `blocked` even though no pane runs it to focus. `--remote-control` starts Claude with Remote Control on. Flags go before the prefix. |
| `sessionhub resume --pick [--force] [--remote-control]` | Numbered list of this machine's sessions (live first, then newest; at most 30). Type a number, then the same logic as `sessionhub resume`. Empty input cancels. |
| `sessionhub remote-control <id-prefix>` | Turn on Claude Code Remote Control for a session. See below. `-h` or `--help` prints the usage. |
| `sessionhub plugin open-picker` | Runs `herdr plugin pane open --plugin sessionhub --entrypoint resume-picker --placement overlay --focus`, which runs `sessionhub resume --pick`. It uses `HERDR_BIN_PATH` when set. |
| `sessionhub plugin open-inbox` | Runs `herdr plugin pane open --plugin sessionhub --entrypoint inbox --placement split --direction right --focus`, which runs `sessionhub inbox --watch` in a pane to the right. It uses `HERDR_BIN_PATH` when set. The pane opens in the herdr of the machine the command runs on, so run it in the herdr you are looking at; outside a herdr pane (for example over SSH), it prints a note saying where the pane opened. |

A prefix needs at least 4 characters. If it matches more than one session, the
server answers 409 and the error lists the candidate IDs.

The report summary shows the first in-flight item (`doing:`), else the first
waiting-on item (`waiting:`), else the last done item (`done:`), else the note.

Every string from the server or herdr passes through one helper
(`internal/cli/termtext`) before it reaches the terminal: control characters
(including escape sequences' `ESC`) and Unicode bidirectional controls are
removed, and newlines and tabs become spaces. `sessionhub ls` and the `--pick` list
show at most 60 characters of a title, and `sessionhub ls` at most 60 of the report
summary. Printed shell commands are the exception: they keep every byte and
rely on quoting instead.

## Resume decision table

`sessionhub resume` fetches the session, then compares its `machine` with `machine`
in the client config.

| Session | herdr state | What happens |
| --- | --- | --- |
| This machine | `herdr_pane` exists, herdr detects Claude in it, and its `agent_session` matches | `pane.focus` on it. |
| This machine | `herdr_pane` exists and its `agent_session` matches, but herdr detects no agent (the pane is at a shell prompt, as a restored pane is) | `agent.start` in that pane with `kind: "claude"`, `args: ["--resume", <uuid>]`, name `sessionhub-<first 8>`, then `pane.focus` on it. |
| This machine | The pane is gone, runs another session, or runs another agent | `pane.split` (right, focused, session's cwd, target `HERDR_PANE_ID` else the snapshot's focused pane), then `agent.start` with `kind: "claude"`, `args: ["--resume", <uuid>]`, name `sessionhub-<first 8>`. |
| This machine | herdr is not running | Prints `cd <cwd> && claude --resume <uuid>`. |
| This machine | The session has no herdr info | Same printed command; herdr is not contacted. |
| Another machine | Has herdr info | Prints `ssh -t <ssh_host> '~/.local/bin/sessionhub resume <uuid>'` and `herdr --remote <herdr_host>`. |
| Another machine | Has a pane, and a saved herdr machine matches | Also prints `herdr --machine <label> pane focus <pane_id>`. |
| Another machine | No herdr info | Prints `ssh -t <ssh_host> "exec \$SHELL -lc 'cd <cwd> && claude --resume <uuid>'"`. |

The remote branch only prints. `sessionhub` never runs `ssh` or a remote `herdr`.

A command that `ssh` runs gets a non-interactive shell whose `PATH` lacks
`~/.local/bin`, where both `sessionhub` and `claude` are installed. So the printed
`sessionhub resume` calls `~/.local/bin/sessionhub` by path, and the no-herdr form runs
`claude` through the remote login shell (`$SHELL -lc`), which reads the profile that
sets `PATH`. The remote command is quoted for your local shell, so the `~` and
`$SHELL` reach the remote shell unexpanded and it expands them to the remote
home and shell. The server's `resume_command` field uses the same
`ssh -t <ssh_host> '~/.local/bin/sessionhub resume <uuid>'` form.

Pane IDs belong to one herdr server. If the session's `herdr_session` differs
from the current server's session name, `sessionhub resume` skips the focus step and
starts a new pane.

If the split succeeds but `agent.start` fails, the new empty pane stays, and
the error prints the shell command to run instead.

If the server reports the session `live` or `blocked` but no pane runs it to
focus, `sessionhub resume` doesn't start it: the session may still run somewhere
else, such as another herdr server or a plain terminal, and a second
`claude --resume` of the same session would write to the same transcript. It
prints a warning with the `sessionhub resume --force <uuid>` command, changes
nothing in herdr, and exits non-zero. In `--pick`, the warning stays on screen
until you press Enter. When `sessionhub resume` only prints a command, it prints the
same warning above it.

If the session has an open move (`requested`, `packing`, `uploaded`, or
`unpacking`), `sessionhub resume` refuses it, even with `--force`, on every machine:
until the move ends, the source or the target may start the session, so a
resume would run it in two places. It prints the
`sessionhub move --status <move-id>` command, changes nothing in herdr, and exits
non-zero.

In `--pick`, when the result is a printed command rather than a herdr action,
the picker waits for Enter so the overlay does not close over the text.

### `sessionhub resume --remote-control`

The flag adds `--remote-control` (with no name) to every way `sessionhub resume`
starts Claude: the `agent.start` args become
`["--resume", <uuid>, "--remote-control"]`, and every printed `claude` command
ends with `--remote-control`. The printed remote command passes the flag on:
`ssh -t <ssh_host> '~/.local/bin/sessionhub resume --remote-control <uuid>'`. If a
pane already runs the session, `sessionhub resume` only focuses it, because a running
Claude can't take a flag; it prints `sessionhub remote-control <uuid>` to use instead.

## Remote Control

`sessionhub remote-control <id-prefix>` fetches the session (same lookup and ID
validation as `sessionhub resume`) and compares its `machine` with `machine` in the
client config.

| Session | State | What happens |
| --- | --- | --- |
| This machine | The pane exists, herdr detects Claude in it, and its `agent_session` matches | Reads the pane (`pane.read`, `recent_unwrapped`, 120 lines), sends `/remote-control` with `agent.prompt` (target is the pane ID), then reads the pane every 500 ms for up to 10 s. Prints the last `https://claude.ai/code/session_...` URL that was not in the first read (waits up to 10 s; Claude runs the command at once, even mid-turn). If none appears, prints that the command was sent and to check the pane or the Claude app, and exits 0. |
| This machine | herdr answers `agent_blocked` (Claude waits on a permission or other prompt) | Exits 1 with "the session is waiting on a prompt in its pane; answer it, then run this again". herdr sends no input and `sessionhub` doesn't retry. |
| This machine | Ended, no pane recorded, pane gone, pane runs no Claude or another session, the session belongs to another herdr server, or herdr is not running | Exits 1 with "session is not running; start it with: `sessionhub resume --remote-control <id>`". |
| Another machine | Any | Posts a control request through the sessionhub (`POST /v1/sessions/{id}/remote-control`, with this machine's token), prints `Asked machine "<name>" to turn on Remote Control for session <short ID>. Waiting for it...`, then reads the session every 2 s for up to 2 minutes. That machine's watcher runs the request as in `docs/plugin.md`, "Control loop", so an ended session there is resumed in a new herdr workspace. Prints the link (exit 0), says that no link appeared (exit 0), or exits 1 with the machine's reason. If the sessionhub refuses the request (for example `409`: not in herdr, or the machine's watcher is offline), can't be reached, or the request expires, prints why, then `ssh -t <ssh_host> '~/.local/bin/sessionhub remote-control <uuid>'`, and `herdr --machine <label> agent prompt <pane_id> /remote-control` when the session has a pane and a saved herdr machine matches. Never runs either, and exits 0. |

herdr decides whether the session runs, not the server's `status`, so a session
the server still lists as `ended` works when its pane runs it. From another
machine, `sessionhub` prints a link from the sessionhub only when the whole value matches
the pattern below, and prints the machine's detail in parentheses after it,
for example `(Remote Control was already on)`. Only the URL
that matches `https://claude.ai/code/session_[A-Za-z0-9_-]+` is printed, after
the pane text passes through the terminal-text helper. A URL that was already
on screen before the command doesn't count.

If Remote Control is already on, Claude answers `/remote-control` with a
dialog (**Disconnect this session**, **Show QR code**, **Continue**) that shows
the session URL instead of printing a new line. `sessionhub` judges the dialog on the
currently visible screen only (`pane.read` with source `visible`), never on
scrollback, because ordinary output such as a `grep` of the fixtures can hold
every string of it. All of these must hold within the last 12 non-blank visible
lines: "Disconnect this session", then "Show QR code", a URL that matches the
pattern, and the footer "Esc to continue" as the last line. When they do, `sessionhub`
prints "Remote Control is already on for session ..." and the URL, then closes
the dialog with `pane.send_keys` `esc` (Continue is the default). It doesn't
wait out the 10 s and exits 0.

Just before it sends `esc`, `sessionhub` reads the pane again. It sends nothing if
Claude's `agent_status` is `working` (Esc during a turn interrupts it) or if the
dialog is no longer on the visible screen. It says so, and asks you to press
**Esc** in the pane, if the check or the `esc` fails. If the dialog is already
on the visible screen before `sessionhub` sends anything, it exits 1 without sending,
because the command would go into the dialog.

`sessionhub` doesn't check `agent_status` first. Claude runs `/remote-control` at once
even while it works on a turn, and the turn continues (see
`docs/dev/evidence/remote-control.md`).

Text from the pane is cleaned before URL matching: control characters become
spaces, so they can't join two IDs. `sessionhub` refuses a recorded pane ID that
isn't herdr-shaped (`^[A-Za-z0-9][A-Za-z0-9:._-]{0,63}$`, for example a leading
hyphen) before it prints or uses it.

The `herdr --machine ... agent prompt` form sends the command without the
checks above. If the agent is blocked, herdr rejects it the same way.

## Saved herdr machines

`herdr machine list --json` runs with a 2-second timeout. Any failure, or
output the parser cannot read, counts as "no saved machines". A profile matches
when its SSH target host (without `user@`, scheme, or port) equals the
machine's `herdr_host`, case-insensitively. Disabled profiles are skipped.

The JSON field names (`label`, `ssh_target`, `enabled`) are not verified
against a real profile: no machine was saved when this was written. See
`testdata/README.md`.

## Printed commands are shell-safe

`sessionhub resume` and `sessionhub remote-control` refuse a session ID that is not letters, digits, and hyphens (or starts with a hyphen), and refuses to print a remote recipe when the SSH or herdr host is empty or starts with a hyphen. Every value in a printed command (ID, hosts, pane, label, cwd) is single-quoted when it has unsafe characters, and the no-herdr remote command is quoted three times deep: once for your local shell, once for the remote shell, and once for the remote login shell. Each shell parses its layer back to the intended arguments; the tests check this with `sh` for every hostile value. The local layer uses double quotes with `$` escaped when that is safe, and single quotes when the command holds a `"`, `` ` ``, `\`, `!`, or newline. Comments in the output never include session data. The `remote-control` forms get the same hostile-value tests, and `--remote-control` is one plain word in every layer.

## `sessionhub login`

`sessionhub login` signs a browser in to the dashboard without copying a secret. Run
it on any machine with a client config; it uses that machine's token.

```sh
sessionhub login --name phone
```

Open the printed link on the device, or scan the QR code, and press
**Sign in**. The link works once, for 10 minutes. The browser stays signed in
for 30 days after its last visit.

- The name follows the machine-name rules: 1 to 64 letters, digits, `.`,
  `_`, or `-`, starting with a letter or digit. If a browser session already
  has the name, the command fails and suggests `sessionhub login rm <name>`.
- At most 5 unused links exist at a time; the server answers `429` above that.
- Nothing is queued. If the server is unreachable, the command fails.
- The QR code uses half blocks, two module rows per line, with light modules
  drawn filled so the code reads dark on light in a dark terminal. If your
  terminal is light and your phone doesn't scan it, use `--no-qr` and open the
  link another way.
- **Sign out** on the dashboard ends only that browser's session.
  `sessionhub login rm` signs out any browser.

## `sessionhub inbox`

`sessionhub inbox` lists the sessions that need you, the same list as the
dashboard's **Inbox** tab:

- **Blocked**: Claude waits on a prompt in its pane.
- **Waiting on you**: the latest report lists `waiting_on` items, and you
  haven't sent a prompt since.
- **Finished**: Claude finished a turn, and you haven't sent a prompt since.

A session appears once, in the first group that matches. Ended sessions
leave the inbox; stale ones stay, marked `stale`. The line width is the
terminal's, else `COLUMNS`, else 100 characters. An empty inbox prints
`nothing needs you`.

`sessionhub inbox dismiss` and `sessionhub inbox snooze` take a session ID or a prefix of
at least 4 characters, matched against the current inbox; an exact ID wins.
If the session isn't in the inbox, they fail with "not in the inbox".

- A dismissed item stays hidden until something new happens: the session
  blocks again, reports a new `waiting_on`, or finishes another turn.
- A snoozed item comes back at the snooze time even if nothing new happened.
- Triage is shared: a dismiss on one machine also hides the item on the
  dashboard and on every other machine.

### `sessionhub inbox --watch`

`sessionhub inbox --watch` keeps the inbox on screen and reads it again every 5
seconds and after every action. It needs a terminal: with stdin or stdout
redirected, it exits with an error.

| Key | Does |
|---|---|
| **j** or **Down**, **k** or **Up** | Move the cursor. |
| **Enter** | Jump to the session (see below). |
| **d** | Dismiss the item. |
| **s**, then **1**, **4**, or **t** | Snooze for 1 hour, 4 hours, or until 9:00 tomorrow. Any other key cancels. |
| **r** | Read the inbox now. |
| **q** or **Ctrl+C** | Quit. |

The bottom line shows when the list was last read, what the last action did,
and the last error. The cursor stays on its session across reads; if the
session leaves the inbox, the cursor moves to the row that took its place.
The view uses the terminal's alternate screen and raw mode (through `stty`)
and restores both when it exits, also on `SIGINT`, `SIGTERM`, `SIGHUP`, or a
crash. After a signal, the command exits with status 130 and prints no
error message.

**q** and **Ctrl+C** quit at once, even during a slow request: they cancel
the read, jump, dismiss, or snooze in flight.

**Enter** jumps to the session under the cursor. A session with an open move
is refused, like `sessionhub resume` refuses it, and the status line shows the
`sessionhub move --status` command instead. Otherwise:

- **On this machine:** the same as `sessionhub resume <id>` (focus the session's
  pane, or start it in a pane), without printing the resume command. If
  there is no herdr pane to use, the status line shows the command.
- **On another machine:**
  1. `ssh -o BatchMode=yes <ssh_host> '~/.local/bin/sessionhub resume <id>'`, with
     a 15-second limit, so that machine's herdr focuses or starts the
     session. `ssh` reads no input from the terminal and its output isn't
     shown, so the view keeps working.
  2. If a local herdr tab named `sessionhub:<machine>` exists, it is focused.
     Otherwise sessionhub creates it, focused, and runs `herdr --remote
     <herdr_host>` in it (with `herdr pane run`). An existing tab is focused
     as it is, even if its `herdr --remote` has exited.
  3. If the server lists no such machine, the machine has no `herdr_host`,
     or `ssh` fails, the status line shows the resume command instead. If
     `ssh` ran but `sessionhub resume` on the other machine exited non-zero, the
     line says `sessionhub resume on <machine> failed (<reason>)`. It says `ssh
     <host> failed` only when `ssh` itself failed (exit status 255, a
     timeout, or no `ssh` binary).
     `BatchMode` means `ssh` never waits for a password: set up key
     authentication for the jump to work.

  If your SSH config has a `Host` entry named after the machine (for
  example `Host tower` with a LAN address), the jump uses that name instead
  of the server's `ssh_host`, for both `ssh` and `herdr --remote` (when the
  herdr host was the same as the SSH host). The server's host is shared by
  every viewer and may be a name, such as a Cloudflare hostname, that this
  machine can't reach. sessionhub checks for the entry with `ssh -G <machine>`.

## `sessionhub rules`

The rules are short standing instructions that every Claude session on
every machine sees at its start and with every prompt. `sessionhub rules add`
saves one on the server; each machine's herdr watcher copies the list
within a minute, and sessions see it from their next prompt. A rule is one
line of at most 300 characters, and the whole list is at most 2,000; an add
past that fails and asks you to remove a rule first. The dashboard's
**Rules** tab and the MCP tools `remember`, `forget`, and `instructions`
edit the same list.

## `sessionhub send`

`sessionhub send` types a message into each target session's terminal, prefixed
with `From the user via sessionhub (cli on <machine>):` and a blank line. Each
target is a session ID or a prefix of at least 4 characters; `--machine M`
targets every live session in herdr on `M` whose watcher is polling. Flags
may come before or after the targets.

It prints one line per target, `queued` or `refused: <reason>`, then waits
up to 15 seconds and prints the delivery result of each queued one:
`delivered`, `not delivered yet: <reason>; sessionhub keeps trying for 10 minutes`
(the agent is working or waiting on a prompt), `refused: <reason>`, or
`expired`. The watcher types a message only when the session's agent is
idle or done. The command exits 1 when any target was refused or expired.
At most 30 messages a minute leave one machine.

## `sessionhub approve` and `sessionhub deny`

When a session waits on a permission prompt, `sessionhub inbox` shows
`asks to use <tool>: <command or file>` on its **Blocked** line, with
` (cut)` when the hook cut a long input. `sessionhub approve <prefix>` allows it
once, and `sessionhub deny <prefix> ["reason"]` denies it; Claude sees the reason.
The target is the session ID or a prefix of at least 4 characters, matched
against the inbox, or the request ID (`pr_...`, listed by `sessionhub inbox
--json`).

For a session prefix, the command first prints what you answer: the
session, the tool, the whole Bash command (or, for any other tool, its
whole stored input as compact JSON, at most 8 KiB), line by line and
indented four spaces so every line of a multi-line command shows, and the
request ID. It
then asks `Allow this once? [y/N]` (or `Deny this request? [y/N]`) and
sends nothing unless you type `y` or `yes`. If stdin is not a terminal,
the command refuses unless you pass `--yes`, which answers without asking.
A request ID skips the question: you name the exact request.

If the hook cut a long input, you cannot see all of it, so `sessionhub approve`
prints the cut note and exits 1 with "the input was cut; allow it in the
terminal", even with `--yes` or a request ID (the server refuses that
allow with `409`, and the command prints its reason). `sessionhub deny` still
works on a cut input.

The answer reaches the session within about a second. If someone answered
already, the command fails with "already decided"; if the prompt was
answered in the terminal or expired, it fails with the server's reason. sessionhub
only ever allows once; it never adds a permanent rule.

## `sessionhub move`

`sessionhub move <id-or-prefix> tower` moves a session to the machine `tower`, with
its conversation, its branch, and its uncommitted changes, and resumes it
there in a new herdr workspace. `sessionhub move <id-or-prefix> cloud` hands it to
a new Claude Code cloud session instead (GitHub remotes only). You can run it
on any machine.

The session must be in herdr on a machine whose watcher runs, and its agent
must be idle or done. The target machine needs a running watcher and a clone
of the same repository under `~/Code` (or a `move_roots` entry in
`~/.config/sessionhub/config.toml`) with no uncommitted changes, and the same Claude
Code version. When a rule fails, the server says which, and nothing moves.

It prints one line per state:

    mv_...  moving 3f2a9c10 (auth work) from bluebox to tower
    requested: waiting for the sessionhub watcher on bluebox
    packing on bluebox: ending the session and sealing the bundle
    uploaded (3.0 MiB): waiting for tower
    unpacking on tower
    done: the session runs on tower now; not carried: .env
    see notes later with: sessionhub move --status mv_...

A failure prints `failed: <step>: <reason>` and exits 1; the session then
runs again on the source machine. After `done` or `failed`, the last line
prints the `sessionhub move --status` command: the source may add a note to the
move later, such as an archive or a restart that failed there. After 3 minutes the command stops
following and prints the `sessionhub move --status` command to check later. If you press Ctrl+C, the move goes on, and the command prints the same hint and exits 130. Files
listed as "not carried" (`.env*`, `*.pem`, `*.key`, `*.tfvars`) stay on the
source; copy them by hand.

`sessionhub move-key` on each machine and `sessionhub machine ls` on the server show the
same fingerprint for a machine. If they differ, someone swapped a key on the
server: don't move sessions until you find out why.

## `sessionhub start`

```
sessionhub start <machine> [--dir DIR] [--trust] [-m "first prompt"]
```

Asks `<machine>`'s watcher to start a new Claude session with Remote Control
on, in a new herdr workspace in `DIR`, and follows the request for up to
2 minutes 10 seconds:

```
st_roa-9PDiUM1O9kEUXQZxZQ  starting a session on tower in ~/Code/app
waiting for the sessionhub watcher on tower
starting Claude on tower
started on tower: https://claude.ai/code/session_01... (started in a new herdr workspace)
```

- `--dir` defaults to the current directory when `<machine>` is this machine,
  and to `~` (that machine's home directory) otherwise. It must be absolute or
  start with `~/`, and on the machine it must exist and be inside the home
  directory.
- Claude must already trust `DIR`, or a folder above it, on that machine.
  Otherwise the watcher starts nothing and the request fails:

  ```
  failed: /home/me/Code/new is not trusted by Claude on tower; open it once or pass --trust
  ```

  To trust it, run `claude` in that folder once and accept the prompt, or
  pass `--trust`.
- `--trust` marks `DIR`, and only `DIR`, as trusted in Claude's config on
  that machine (`projects["DIR"].hasTrustDialogAccepted` in `~/.claude.json`,
  or `$CLAUDE_CONFIG_DIR/.claude.json`) before starting. It never trusts a
  folder above `DIR`. It refuses a relative `--dir` and the home directory,
  including the `~` default on another machine, because trusting home
  trusts every folder under it. The watcher rewrites the config atomically
  and keeps every other setting.
- `-m` is a first prompt, submitted once Claude is idle. If Claude is still
  waiting on a prompt in the pane, the prompt is not sent, and the result
  says so.
- It exits 0 once the session started, 1 when the request failed or expired,
  and 130 when you press Ctrl+C, which stops following but not the start.
- A machine whose watcher is offline, an unknown machine, or a machine with
  `remote_start = false` refuses with the reason.

## `sessionhub digest`

`sessionhub digest <id>` reads one session's transcript and git activity on this
machine and sends its digest to the sessionhub. `sessionhub digest --all` does this for every
session the sessionhub lists for this machine, which backfills sessions that started
before you installed digests. `<id>` is the full session ID or a unique prefix, such as the 8 characters `sessionhub ls` prints.

Each session prints one line, `<first 8 characters of the ID>  <result>`:

| Result | Meaning |
| --- | --- |
| `sent` | The sessionhub accepted the digest. |
| `queued` | The sessionhub was unreachable or busy (network error, timeout, 408, 429, or 5xx). The digest waits in the queue and the next drain sends it. |
| `skipped: <reason>` | Nothing to send: `no transcript`, `another run is in progress`, or `nothing to report yet`. |
| `failed: <reason>` | The sessionhub rejected the digest (for example, it doesn't know the session) or the transcript has too many unreadable lines. Failed digests aren't queued. |

`--all` ends with a total line, `N sent, N skipped, N failed`, where `queued`
counts as sent. The command exits non-zero only when a digest failed. Running
`--all` again is safe: each digest replaces the previous one. See
[Digests](client.md#digests).
