# Migrating from `hub` to `sessionhub`

Before it was published, the binary was called `hub`. The rename changed
every name on disk; tokens, the database contents, and the queue format did
not change. This page moves an existing install over without losing data.

| Old | New |
|---|---|
| `~/.local/bin/hub` | `~/.local/bin/sessionhub` |
| `~/.config/hub/` (`config.toml`, `server.toml`, `move.key`) | `~/.config/sessionhub/` |
| `~/.local/share/hub/hub.db` | `~/.local/share/sessionhub/sessionhub.db` |
| `~/.local/state/hub/` (queue, logs) | `~/.local/state/sessionhub/` |
| `HUB_*` environment variables | `SESSIONHUB_*` |
| systemd unit `hub.service` | `sessionhub.service` |
| MCP server `hub` (tools `mcp__hub__*`) | `sessionhub` (tools `mcp__sessionhub__*`) |
| herdr plugin `hub` | `sessionhub` |

Unchanged: machine and browser tokens, the database schema, the herdr
sidebar tokens (`$hub_summary`, `$inbox`), and the `X-Hub-Action` header.

## On the server host

Do this first, then the other machines. Run the old uninstallers with the
**old** binary, because the new one looks for the new names.

1. Stop the old server and back up the database:

   ```sh
   systemctl --user disable --now hub
   sqlite3 ~/.local/share/hub/hub.db ".backup $HOME/hub.db.pre-sessionhub"
   rm ~/.config/systemd/user/hub.service
   systemctl --user daemon-reload
   ```

2. Remove the old clients:

   ```sh
   ~/.local/bin/hub uninstall-plugin
   ~/.local/bin/hub uninstall-hooks
   ~/.local/bin/hub uninstall-mcp
   ```

3. Move the files:

   ```sh
   mv ~/.config/hub ~/.config/sessionhub
   mv ~/.local/share/hub ~/.local/share/sessionhub
   cd ~/.local/share/sessionhub
   for f in hub.db hub.db-wal hub.db-shm; do [ -e "$f" ] && mv "$f" "session$f"; done
   ls hub.db.bak-* >/dev/null 2>&1 && for f in hub.db.bak-*; do mv "$f" "session$f"; done
   cd -
   [ -d ~/.local/state/hub ] && mv ~/.local/state/hub ~/.local/state/sessionhub
   rm -f ~/.local/bin/hub
   ```

4. From your checkout, deploy the new server. It installs `sessionhub.service`
   and, once it finds `~/.config/sessionhub/config.toml`, the herdr plugin:

   ```sh
   make deploy DEPLOY_HOST=<server host>
   ```

5. Back on the server host, install the hooks and the MCP server:

   ```sh
   sessionhub install-hooks
   sessionhub install-mcp
   sessionhub status
   ```

## On every other machine

1. Remove the old clients with the old binary:

   ```sh
   ~/.local/bin/hub uninstall-plugin
   ~/.local/bin/hub uninstall-hooks
   ~/.local/bin/hub uninstall-mcp
   ```

2. Move the files and remove the old binary:

   ```sh
   mv ~/.config/hub ~/.config/sessionhub
   [ -d ~/.local/state/hub ] && mv ~/.local/state/hub ~/.local/state/sessionhub
   rm -rf ~/.local/share/hub
   rm -f ~/.local/bin/hub
   ```

3. Install the new binary at `~/.local/bin/sessionhub` (`make install`), then
   the clients:

   ```sh
   sessionhub install-plugin
   sessionhub install-hooks
   sessionhub install-mcp
   sessionhub status
   ```

## Everywhere

- Rename any `HUB_*` variables in your shell profile to `SESSIONHUB_*`.
- Replace the snippet in `~/.claude/CLAUDE.md` with the one in
  [`CLAUDE-snippet.md`](CLAUDE-snippet.md); it names the `sessionhub` MCP
  server.
- Rename `mcp__hub__*` to `mcp__sessionhub__*` in any permission allowlist or
  `--allowedTools` flag.
- Restart running Claude sessions to pick up the new hooks and MCP server.

## Roll back

Stop `sessionhub.service`, move the directories and files back to their old
names, reinstall the old binary at `~/.local/bin/hub`, and run its
`install-plugin`, `install-hooks`, and `install-mcp`. If anything went wrong
with the database, restore `~/hub.db.pre-sessionhub`.
