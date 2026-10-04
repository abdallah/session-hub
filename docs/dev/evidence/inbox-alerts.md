# Inbox alerts and the herdr inbox pane: deploy and live check

Date: 2026-10-01. Commit: `c0d582b`, deployed to `tower` with `make deploy` and
installed on `bluebox` with `make install` and `sessionhub install-plugin`.

## Deploy

- `tower` runs `sessionhub c0d582b`; the database is at schema version 7
  (`pragma user_version` prints `7`), with a backup taken by `make deploy`.
- The server log shows `telegram alerts are on: checking the inbox every
  15s`, so the Telegram settings in `server.toml` loaded.
- The watchers restarted on the new build: `tower` at 16:03:25 and `bluebox` at
  16:04:11 (`watcher.log`). The `bluebox` watcher had been running a build from
  2026-09-30, because `make install` does not restart it; `sessionhub install-plugin`
  does.

## Live checks

1. **Sidebar count.** After one heartbeat, `herdr workspace list` on `bluebox`
   shows `"tokens":{"inbox":"12"}` on both workspaces, matching `sessionhub inbox`
   (2 waiting, 9 finished, plus one more by then). The count appears in the
   sidebar once `$inbox` is in a `[ui.sidebar.spaces]` row of herdr's config.
2. **Telegram and herdr notification for Blocked.** Not triggered yet: no
   session was blocked for 30 seconds during the check. The notifier is
   running (log line above).
3. **Inbox pane and jump.** Not run from this session, because it needs a real
   terminal. Open it with `sessionhub plugin open-inbox`.
