# Upgrade notes

The general upgrade steps and how to roll back are in
[`self-hosting.md`](self-hosting.md#upgrade). This page lists the releases
that need more than that. Newest first.

## Upgrade to the Claude Code mod

This release adds the sessionhub Claude Code mod and moves the database to
schema version 12. Deploy the server first; it upgrades the database on its
first start. A new client against an old server finds no targets for
`sessionhub send --machine`, because the old server doesn't send
`messageable`.

Then, on every machine where you want the mod, install the new binary and
run:

```sh
sessionhub install-plugin
sessionhub install-mod
```

`sessionhub install-mod` writes the mod to
`~/.local/share/sessionhub/claude-mod/` and adds that directory to
`env.CLAUDE_CODE_PLUGIN_DIRS` in `~/.claude/settings.json`, after a backup.
Sessions started after that load the mod; running ones don't. From now on,
`make deploy` reruns `sessionhub install-mod` on the server host when the mod
is installed there; on other machines, rerun it after each upgrade.

To roll back, run `sessionhub uninstall-mod` with the new binary on every
machine that has the mod. Then stop the server, restore the database backup
as described in [self-hosting.md](self-hosting.md#upgrade), install the
previous binary everywhere, and start the server. The version 11 binary
refuses a version 12 database.

## Upgrade to start trust checks

This release moves the database to schema version 11. Deploy the server
first, then install the new binary on every machine and run
`sessionhub install-plugin` to restart the watcher. Until a machine's
watcher runs the new binary, it ignores `--trust` and starts Claude in an
untrusted folder as before, where Claude stops at its trust prompt.

To roll back, stop the server, restore the database backup as described in
[self-hosting.md](self-hosting.md#upgrade), install the previous binary
everywhere, and start the server. The version 10 binary refuses a version 11
database.

## Upgrade to new sessions

This release moves the database to schema version 10. Deploy the server
first (it upgrades the database on its first start), then install the new
binary on every machine and run `sessionhub install-plugin`, so the watcher
can take start requests. A watcher on an older binary fails them at once
with "unknown action start".

To roll back, stop the server, restore the database backup as described in
[self-hosting.md](self-hosting.md#upgrade), install the previous binary
everywhere, and start the server. The version 9 binary refuses a version 10
database.

## Upgrade to moves

This release moves the database to schema version 9. Deploy with
`make deploy`, which backs up the database first and reinstalls the plugin
on `tower`; the first start upgrades the database. Then, on every other
machine, run `make install` and `sessionhub install-plugin`. The new watcher creates
`~/.config/sessionhub/move.key` and registers its public key within a minute. It
registers the key again every hour, so a database restored from a backup
learns it back.

Check the keys out of band: `sessionhub machine ls` on `tower` shows each machine's
`MOVE_KEY`, and `sessionhub move-key` on each machine prints its own. They must
match.

To roll back, stop the server, restore the database backup as described
in [self-hosting.md](self-hosting.md#upgrade), install the previous binary on `tower`, and start the server; then
install the previous binary on every machine and run `sessionhub install-plugin`.
The version 8 binary refuses a version 9 database, so restoring the backup
is the only way back. `move.key` can stay; the old binary ignores it.

## Upgrade to session actions

This release moves the database to schema version 8 and adds hook entries.
Deploy with `make deploy`, which backs up the database first and reinstalls
the plugin on `tower`; the first start upgrades the database. Then, on every
machine, `tower` included:

1. Run `make install` (not needed on `tower` after `make deploy`), then
   `sessionhub install-plugin`, so the new watcher delivers messages and copies the
   rules.
2. Run `sessionhub install-hooks`. It adds the `context` entry to `SessionStart`
   and `UserPromptSubmit` and a new `PermissionRequest` entry with a
   660-second timeout. Sessions that were running pick up the new hooks only
   after a restart.

To roll back, stop the server, restore the database backup as described
in [self-hosting.md](self-hosting.md#upgrade), install the previous binary on `tower`, and start the server. The
version 7 binary refuses a version 8 database, so restoring the backup is the
only way back. On every machine, first run `sessionhub uninstall-hooks` with the
new binary (the older `install-hooks` on its own does not remove the
`PermissionRequest` and `context` entries), then install the previous
binary and run `sessionhub install-plugin` and `sessionhub install-hooks`.

## Upgrade to inbox alerts

This release moves the database to schema version 7. Deploy with
`make deploy`, which backs up the database first and reinstalls the plugin on
`tower`; the first start upgrades the database. On each other machine, run
`make install`, then `sessionhub install-plugin`: the plugin manifest gains the
`inbox` pane and action, and the new watcher writes the sidebar count. Then
turn on Telegram alerts and add the `$inbox` sidebar row if you want them.

To roll back, stop the server, restore the database backup as described
in [self-hosting.md](self-hosting.md#upgrade), install the previous binary on `tower`, and start the server. The
version 6 binary refuses a version 7 database, so restoring the backup is the
only way back. On the other machines, install the previous binary and run
`sessionhub install-plugin` again.

## Upgrade to the inbox

This release moves the database to schema version 6. Deploy with
`make deploy`, which backs up the database first; the first start upgrades
it. Run `make install` on the other machines to get `sessionhub inbox`. Sessions
that were already idle enter the inbox only after their next turn.

To roll back, stop the server, restore the database backup as described
in [self-hosting.md](self-hosting.md#upgrade), install the previous binary on `tower`, and start the server.

## Upgrade to web sign-in

This release replaces the dashboard's read token with sign-in links. The
database moves to schema version 5. Clients (hooks, the herdr watcher, MCP)
are unchanged: they use machine tokens.

1. Deploy with `make deploy`, which backs up the database first. The first
   start upgrades it to version 5.
2. Keep the `read_token` line in `tower:~/.config/sessionhub/server.toml` until the
   upgrade is confirmed. The new server ignores it and logs one warning; the
   previous binary needs it if you roll back.
3. Sign in each device. On `tower`, or on a machine where you ran `make install`
   with the new version (`make deploy` updates only the server host):

   ```sh
   sessionhub login --name windows
   sessionhub login --name phone
   ```

   Open each link on its device, or scan the QR code with the phone, and
   press **Sign in**.
4. Delete the `read_token` line, then restart the server:
   `systemctl --user restart sessionhub`.

To roll back, stop the server (`systemctl --user stop sessionhub`), restore the
database backup and the previous binary as described above, put `read_token`
back if you deleted it, and start the server. Browsers sign in with
`?token=` again.
