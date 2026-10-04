# Shared rules, messages, and remote permission answers: deploy and live check

Date: 2026-10-03. Commit: `8fb388c`.

## Deploy

1. Backed up `~/.claude/settings.json` on `bluebox` and `tower`
   (`settings.json.bak-sessionhub-v8`).
2. `make deploy`: database backup `sessionhub.db.bak-20261003T001446`, then the new
   binary and plugin on `tower`. The database is at schema version 8.
3. `make install` and `sessionhub install-plugin` on `bluebox`. Both machines run
   `sessionhub 8fb388c`.
4. `sessionhub install-hooks` on each machine, after both binaries were updated.
5. One sessionhub group per event on both machines (`jq` check from the deploy
   notes): `SessionStart`, `UserPromptSubmit`, `Stop`, `Notification`,
   `SessionEnd`, and `PermissionRequest` each `1`; `tower`'s non-sessionhub
   `PreToolUse` and `PostToolUse` entries untouched.

## Live check (on `bluebox`, a throwaway session in a scratch herdr pane)

- **Rules:** with no rules, `sessionhub hook context` printed nothing and exited 0;
  `sessionhub rules ls` printed `no standing rules`.
- **Permission request:** the session was asked to run
  `touch .../live-v8/approved.txt` in manual mode. Within seconds,
  `sessionhub inbox` showed it under **Blocked** with
  `asks to use Bash: touch ...`.
- **Remote approval:** `sessionhub approve 703fa8f7 --yes` printed the tool, the
  whole command, and request `pr_yvxrn1beqmo4aIC9_7B9cg`, then
  `allowed once`. The command ran (the file exists), and the session's
  events show a `permission` event with `"by":"machine:bluebox"` and
  `"decision":"allow"`.
- **Message:** `sessionhub send 703fa8f7 -m "..."` reported `queued`, then
  `delivered`. The session's prompt showed
  `From the user via sessionhub (cli on bluebox):` followed by the text.
- The test session exited normally; sessionhub shows it `ended`.

Checked earlier (2026-10-02, before the build): the terminal permission
dialog stays visible while a `PermissionRequest` hook waits, a terminal
answer wins, and herdr submits a 16-line message as one prompt.

## Not checked live

- The dashboard's **Rules** tab, send bar, and **Allow once** on a real
  phone (checked with Playwright screenshots on a scratch server at 360 and
  1440 pixels during the build).
- The Telegram "Asks to use" line (needs a real block of 30 seconds).
