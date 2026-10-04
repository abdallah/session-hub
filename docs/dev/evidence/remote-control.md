# Remote Control evidence

Run on 2026-09-30 on `bluebox` (herdr 0.9.3, Claude Code 2.1.285) with `./bin/sessionhub`
built from this branch. The client config is the real one
(`~/.config/sessionhub/config.toml`, server `https://sessionhub.example.com`, machine `bluebox`), and
the real herdr plugin registered the scratch sessions. All panes belong to a
scratch workspace, `sessionhub-rc-cli` (`wS`), created with `--no-focus` and closed at
the end. No other pane was touched. Remote Control URLs are masked (`session_01Qi…`) and session IDs are shortened to 8 characters. Fix round 1 used a second scratch workspace, `sessionhub-rc-fix` (`wV`), also closed at the end.

## Summary

| Check | Result |
|---|---|
| `claude --remote-control` works as a flag | **yes**: `sessionhub resume --remote-control` started `claude --resume <uuid> --remote-control` and Claude printed `/remote-control is active` with the same URL as before |
| `sessionhub remote-control` on an idle Claude | printed the new URL, exit 0 |
| `sessionhub remote-control` on a blocked Claude | "the session is waiting on a prompt in its pane; answer it, then run this again", exit 1, nothing sent |
| `sessionhub remote-control` after `/exit` | "session is not running; start it with: sessionhub resume --remote-control ...", exit 1 |
| `sessionhub remote-control` on a session of another machine | printed the `ssh -t` form, ran nothing, exit 0 |
| `sessionhub remote-control` when Remote Control is already on | fixed: reports "already on", prints the URL, closes the dialog with Esc, exit 0 |
| `sessionhub remote-control` while Claude is `working` | runs at once, turn not broken (see below) |

## Idle Claude

Scratch pane `wS:p3`, started with
`herdr agent start sessionhub-rc-cli-c --kind claude --pane wS:p3`. The plugin
registered it as `d222fe18` (`sessionhub ls` showed `live`).

```
$ ./bin/sessionhub remote-control d222fe18
Remote Control is on for session d222fe18:
https://claude.ai/code/session_01Qi…
exit=0
```

## Blocked Claude

Scratch pane `wS:p2` (`6803f0c4`). I prompted it to use the `AskUserQuestion`
tool, which left it `blocked` on a question. A Claude in `/tmp` did not show a
folder-trust prompt, so this was the reliable way to block one.

```
$ ./bin/sessionhub remote-control 6803f0c4
the session is waiting on a prompt in its pane; answer it, then run this again
exit=1
```

The raw socket exchange is `testdata/herdr/socket/agent-prompt-blocked.ndjson`:
`{"error":{"code":"agent_blocked","message":"agent wS:p2 is blocked and requires interactive input"}}`.

## Exit, then not running, then resume with the flag

```
$ herdr agent prompt wS:p3 '/exit'
$ ./bin/sessionhub remote-control d222fe18
session is not running; start it with: sessionhub resume --remote-control d222fe18
exit=1
$ HERDR_PANE_ID=wS:p3 ./bin/sessionhub resume --remote-control d222fe18
Started claude --resume d222fe18 --remote-control in new pane wS:p4.
```

I set `HERDR_PANE_ID` to my scratch pane so the split landed in my workspace,
not next to the pane that runs this session. `herdr pane read wS:p4` afterwards:

```
❯ /remote-control
  /remote-control is active · Continue here, on your phone,
  or at
  https://claude.ai/code/session_01Qi…
```

The URL matches the one from the first run: resuming the session with the flag
re-attached Remote Control for the same session.

## Session on another machine

```
$ ./bin/sessionhub remote-control 600a66d0
Session 600a66d0 runs on machine "tower", not on this machine. To turn on Remote Control:

ssh -t tower.example.com '~/.local/bin/sessionhub remote-control 600a66d0'
exit=0
```

No saved herdr machine matches on `bluebox`, so the `herdr --machine` form is not
printed.

## Already on: the dialog case (fix round 1)

Before the fix, `sessionhub remote-control` on a session with Remote Control already
on exited 0 with "Sent /remote-control ... no Remote Control URL appeared" and
left this dialog open in the pane:

```
Remote Control
This session is available in the Claude mobile app and at https://claude.ai/code/session_01BG….
  Disconnect this session
  Show QR code              Scan with your phone to open this session
❯ Continue
Enter to select · Esc to continue
```

`sessionhub` now detects it and closes it. Scratch pane `wV:p1` (`d659ba61`), which had
Remote Control on:

```
$ ./bin/sessionhub remote-control d659ba61
Remote Control is already on for session d659ba61:
https://claude.ai/code/session_01BG…
exit=0
```

Afterwards `herdr pane read wV:p1` showed the empty prompt, and a read of the
last 120 lines held neither `Disconnect this session` nor `Show QR code`. The
session stayed connected: the `/rc` marker was still in the header, and a
second `sessionhub remote-control d659ba61` printed the same "already on" message and
URL again (the dialog only opens while connected). The captured exchange is
`pane.send_keys` `{"pane_id":"wV:p1","keys":["esc"]}` with result
`{"type":"ok"}`.

## Dialog detection on the visible screen only (fix round 2)

Round 1 scanned the last 120 lines, so text that merely quoted the dialog
could make `sessionhub` press Esc into a normal prompt (which interrupts a turn) or
refuse to run. Detection now uses `pane.read` source `visible` and needs the
dialog layout at the bottom of the screen (see `docs/cli.md`). Scratch
workspace `sessionhub-rc-fix2` (`wW`), closed at the end.

Dialog already open before the command (refused, nothing sent), then closed
by hand, then reopened and closed by `sessionhub`:

```
$ ./bin/sessionhub remote-control 1b6e7509
a Remote Control dialog is open in the pane; press Esc there, then run this again
exit=1
$ herdr pane send-keys wW:p1 esc
$ ./bin/sessionhub remote-control 1b6e7509
Remote Control is already on for session 1b6e7509:
https://claude.ai/code/session_01J3…
exit=0
```

Afterwards the visible screen had the empty prompt, no dialog strings, and
`agent_status` `idle`.

Misfire check: a fresh Claude (`7f390bd9`, pane `wW:p2`) was told to reply with
the dialog's strings ("Remote Control / Disconnect this session / Show QR code /
Enter to select - Esc to continue / a fake session URL"). Its screen then held
all of them, with the footer text not on the last line. `sessionhub remote-control
7f390bd9` printed the new URL, exit 0, sent no Esc, and did not refuse:

```
Remote Control is on for session 7f390bd9:
https://claude.ai/code/session_01Hp…
exit=0
```

(The sessionhub server answered slowly during this run, so the lookup timed out four
times before it succeeded; that is the 2 s client timeout, not `sessionhub`'s logic.)

## Busy Claude (fix round 1)

Question: does `/remote-control` sent while Claude is `working` run at once,
queue, or break the turn? Scratch pane `wV:p1` (`d659ba61`, fresh Claude, no
Remote Control yet). A first try with "count slowly from 1 to 30" finished in
4 s, too short to test, so I used a tool call that takes longer.

```
$ herdr agent prompt wV:p1 'Use the Bash tool to run: sleep 45. Then reply with the single word done.'
$ herdr pane get wV:p1     -> "agent_status":"working"
17:41:22
$ ./bin/sessionhub remote-control d659ba61
Remote Control is on for session d659ba61:
https://claude.ai/code/session_01BG…
exit=0 17:41:24
$ herdr pane get wV:p1     -> "agent_status":"working"
```

The URL came 2 s after the command, while `agent_status` was still `working`
and the pane showed `✽ Crystallizing… (13s · ↓ 314 tokens)` and
`esc to interrupt`. The pane afterwards, in order:

```
❯ Use the Bash tool to run: sleep 45. Then reply with the single word done.
  Ran 2 shell commands
  /remote-control is active · Continue here, on your phone, or at https://claude.ai/code/session_01BG…
● The 45-second wait is running in the background. I'll reply when it finishes.
✻ Worked for 14s · done 5:41 PM
● Background command "Wait 45 seconds in the background" completed (exit code 0)
```

Result: it runs at once and does not break the turn (the turn continued and
finished normally). Nothing queued, so `sessionhub` keeps the 10 s wait and adds no
`agent_status` guard.

## Protocol facts learned

- `agent.prompt` params are `target` and `text`. Result type `agent_prompted`.
- `pane.read` wire source is `recent_unwrapped` (underscore); the CLI flag is
  `recent-unwrapped`. The text is in `result.read.text`.
- herdr's `agent_blocked` error comes back before any input is sent.
