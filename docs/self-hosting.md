# Self-hosting sessionhub

This guide runs the server on one always-on machine and joins every machine
that runs Claude Code to it. For a single-machine trial, the README's
[Quickstart](../README.md#quickstart) is enough.

The examples call the server host `tower`, a second machine `bluebox`, and the
public address `https://sessionhub.example.com`. Replace them with yours.

## Run the server as a systemd user service

You do these steps once, on the server host. Commands that start with
`ssh tower` run from your checkout on another machine; the rest run on
`tower`.

1. Keep user services running without a login session:

   ```sh
   loginctl enable-linger "$USER"
   ```

2. Create `~/.config/sessionhub/server.toml` with mode `0600`:

   ```sh
   umask 077
   mkdir -p ~/.config/sessionhub
   cat > ~/.config/sessionhub/server.toml <<EOF
   listen = ["127.0.0.1:8787"]
   public_url = "https://sessionhub.example.com"
   stale_after = "5m"
   EOF
   chmod 0600 ~/.config/sessionhub/server.toml
   ```

   `public_url` is the address clients and browsers use. Leave it out to use
   `http://127.0.0.1:8787`, which only works on the server host itself. See
   [`server.md`](server.md) for every key.

3. From your checkout, build and deploy:

   ```sh
   make deploy DEPLOY_HOST=tower
   ```

   `make deploy` builds a static `linux/amd64` binary, copies it to
   `tower:~/.local/bin/sessionhub`, installs
   [`deploy/sessionhub.service`](../deploy/sessionhub.service) as a systemd user
   unit, restarts it, and waits for `/healthz`.

4. Check it: `curl -sS http://127.0.0.1:8787/healthz` on `tower` prints `ok`.

### Run the server with Docker

The repository has a `Dockerfile` and a `docker-compose.yml`:

```sh
SESSIONHUB_PUBLIC_URL=https://sessionhub.example.com docker compose up -d
```

The database lives in the `sessionhub-data` volume. Every `server.toml` key
has a `SESSIONHUB_*` environment override (see [`server.md`](server.md)), so
the container needs no config file. `sessionhub join` needs to run
`sessionhub machine add` next to the database, which a container doesn't
offer over SSH, so add each machine by hand and write its client config:

```sh
docker compose exec sessionhub sessionhub machine add bluebox --ssh-host bluebox.example.com --json
```

Then, on `bluebox`, write `~/.config/sessionhub/config.toml` (mode `0600`) from
the output and run the installers:

```toml
server_url = "https://sessionhub.example.com"
token = "<token from the output>"
machine = "bluebox"
```

```sh
sessionhub install-hooks && sessionhub install-mcp && sessionhub install-plugin
sessionhub install-mod   # optional: the Claude Code mod
```

### Expose it to your other machines

Clients authenticate with bearer tokens, so the server can sit behind any
reverse proxy or tunnel that passes the `Authorization` header through. Don't
put a login wall such as Cloudflare Access in front of it: it would block the
clients. sessionhub does its own authentication. Read
[`SECURITY.md`](../SECURITY.md) before exposing it to the internet.

Example with a token-based Cloudflare Tunnel whose `cloudflared` container
runs on Docker's default bridge (here `docker0` is `10.200.0.1`):

1. Add the bridge address to `listen` in `server.toml`, so the container can
   reach the server without exposing it on the LAN:

   ```toml
   listen = ["127.0.0.1:8787", "10.200.0.1:8787"]
   ```

2. Check that the container can reach it:

   ```sh
   docker run --rm --network bridge curlimages/curl -sS -m 5 http://10.200.0.1:8787/healthz
   ```

   It prints `ok`. If it times out, a firewall is blocking the bridge. With
   ufw:

   ```sh
   sudo ufw allow in on docker0 to 10.200.0.1 port 8787 proto tcp
   ```

3. In the Cloudflare dashboard, open **Zero Trust** > **Networks** >
   **Tunnels** > your tunnel > **Public hostnames** (newer dashboards call
   it **Published application routes**) > **Add a public hostname**, and
   point `sessionhub.example.com` at `HTTP` `10.200.0.1:8787`.

4. Check the public URL: `curl -sS https://sessionhub.example.com/healthz`.

### How the unit starts at boot

If `listen` includes a Docker bridge address, such as `10.200.0.1` in the
tunnel example above, that address exists only once Docker has created
`docker0`. A user unit can't order itself after the system's `docker.service`,
so at boot the first start can fail with `cannot assign requested address`.
The unit restarts on failure every 5 seconds with no start limit, so the
server comes up as soon as the address exists. Check it with:

```sh
systemctl --user status sessionhub
journalctl --user -u sessionhub -n 20
```

## Turn on Telegram alerts

The server can send you a Telegram message when a session has been blocked
on a prompt, or waiting on you, for 30 seconds. It can reuse a bot you already have:
sessionhub only sends messages, so it never competes with another program for
the bot's updates.

1. Create a bot with @BotFather, or reuse one, and copy its token. Find your
   numeric Telegram user ID, which is your chat ID for a direct message, by
   messaging @userinfobot.
2. On `tower`, add them to `~/.config/sessionhub/server.toml`:

   ```toml
   telegram_bot_token = "<bot-token>"
   telegram_chat_id = "<chat-id>"
   ```

   Quote the chat ID. An integer also works.
3. Check that the file is still private: `stat -c %a
   ~/.config/sessionhub/server.toml` prints `600`.
4. Restart the server and check the log:

   ```sh
   systemctl --user restart sessionhub
   journalctl --user -u sessionhub -n 20 | grep 'telegram alerts'
   ```

   It prints `telegram alerts are on`. With either value missing it prints
   `telegram alerts are off`.

Each message names the session, its machine, and how long it has been
blocked or waiting, with an **Open inbox** button that opens the dashboard's **Inbox**
tab, and a **Remote Control** button when the session has a Remote Control
link. The server also sends a message whenever someone adds a standing rule,
with an **Open rules** button, so you notice a rule that a session planted.
See [`docs/server.md`](server.md), "Telegram alerts".

## Join each machine

Run `sessionhub join` on every machine that runs Claude Code, `tower` included. It
creates a per-machine token on the server, writes
`~/.config/sessionhub/config.toml` (mode `0600`), and installs the herdr plugin, the
Claude Code hooks, and the MCP server.

1. Install the binary at `~/.local/bin/sessionhub` (see the README's
   [Install](../README.md#install)).

2. Back up the two files the installers change:

   ```sh
   ts=$(date -u +%Y%m%dT%H%M%SZ)
   cp -p ~/.claude/settings.json ~/.claude/settings.json.pre-sessionhub-$ts
   cp -p ~/.claude.json ~/.claude.json.pre-sessionhub-$ts
   ```

   `sessionhub install-hooks` also writes its own
   `settings.json.sessionhub-backup-<time>` before its first change.

3. Join. On `tower`, which holds the server database, the join runs locally and
   ignores the ssh target:

   ```sh
   sessionhub join tower --name tower --ssh-host tower.example.com --herdr-host tower.example.com
   ```

   On any other machine, the join reaches `tower` over SSH with key-based login
   (`ssh -o BatchMode=yes tower` must work):

   ```sh
   sessionhub join tower --name bluebox --ssh-host bluebox.example.com --herdr-host bluebox.example.com
   ```

   `--ssh-host` is the host in this machine's resume commands, and
   `--herdr-host` is for herdr remote attach. Run the join from a login shell,
   so `herdr` and `claude` are on `PATH`; otherwise it skips those installers.

4. Check the result:

   ```sh
   sessionhub status
   sessionhub ls
   ```

5. Optional: install the Claude Code mod, which shows a question Claude asks
   in the inbox, reports each session's context window fill, and lets
   `sessionhub send` reach sessions outside herdr. `join` doesn't install it:

   ```sh
   sessionhub install-mod
   ```

The join prints two manual steps. sessionhub edits neither file.

### Add the CLAUDE.md snippet

Paste [`docs/CLAUDE-snippet.md`](CLAUDE-snippet.md) into
`~/.claude/CLAUDE.md`, so sessions call `report_progress` and `set_title`:

```markdown
## Reporting progress to sessionhub

The `sessionhub` MCP server gives you two tools. Use them so I can see what a session
is doing from the sessionhub dashboard or the herdr sidebar without opening it.

- Call `report_progress` when you finish a task, when you start a task that
  will take a while, and when you are blocked waiting on me or on something
  external. Fill `done`, `in_flight`, and `waiting_on` with short one-line
  items, and use an empty list when a category has nothing. Put anything else in
  `note`.
- Call `set_title` once the session has a clear purpose, and again if the
  purpose changes. Use a few words, for example "fix CI token rotation".

If a sessionhub tool reports that the server is unreachable, carry on with your work.
The report is queued and sent later.
```

### Add the herdr sidebar row

To show each session's latest summary in the herdr sidebar, add a
`$hub_summary` row to `[ui.sidebar.agents]` in `~/.config/herdr/config.toml`,
for example:

```toml
[ui.sidebar.agents]
rows = [["state_icon", "workspace", "tab"], ["$hub_summary"], ["agent"]]
```

To show the inbox count on each workspace (for example `3 · 1 blocked`),
add `$inbox` to `[ui.sidebar.spaces]`:

```toml
[ui.sidebar.spaces]
rows = [["state_icon", "workspace", "$inbox"], ["branch", "git_status"]]
```

## Resume across machines

A printed resume command looks like
`ssh -t tower.example.com '~/.local/bin/sessionhub resume <id>'`, built from the
`--ssh-host` you gave `sessionhub join` on that machine. Jumping from the inbox
runs `ssh -o BatchMode=yes`, so key-based login to each `--ssh-host` must work
without a prompt.

If your SSH hosts sit behind Cloudflare Access, the machine you resume from
needs an `~/.ssh/config` entry that proxies them through `cloudflared`, and a
current Access login:

```
Host tower.example.com bluebox.example.com
  User <your user>
  ProxyCommand cloudflared access ssh --hostname %h
```

The first connection opens a browser for the Access login.

## Upgrade

- Server and `tower`'s clients: run `make deploy` again. After it replaces the
  binary, it runs `sessionhub install-plugin` on `tower`, which stops the running
  plugin watcher and starts a new one on the new binary. Without that step,
  the watcher keeps running the old binary until herdr restarts.
- Other machines: run `make install`, then `sessionhub install-plugin` for the same
  reason. The hooks and the MCP server pick up the new binary on their next
  run.
- On every machine with the Claude Code mod, run `sessionhub install-mod`
  after the new binary is in place. The mod is part of the binary, so this
  writes the new copy. `make deploy` does it for you on the server host when
  `~/.local/share/sessionhub/claude-mod` exists.

Release notes that need extra steps (database migrations, new hook entries)
are in [`upgrading.md`](upgrading.md).

After you deploy and install on every machine, backfill the session digests
once on each machine, `tower` included:

```sh
sessionhub digest --all
```

The command builds a digest for every session whose transcript is on that
machine and sends it to the server. It is safe to rerun. A line that reads
`skipped: another run is in progress` means the watcher is digesting that
session, not that the run failed.

For an ended session, `uncommitted` and `unpushed` describe the repository when
the digest ran, not when the session ended.

Before it replaces the binary, `make deploy` backs up the server database on
`tower` to `~/.local/share/sessionhub/sessionhub.db.bak-<timestamp>` with `sqlite3 .backup`,
which is safe while the server runs, and keeps the three newest backups. The
step needs `sqlite3` on the server host and is skipped when no database exists
yet.

To roll back this release, restore the pre-deploy backup. The version 3 binary
refuses a version 4 database, so you can't keep the data and run the old
binary. You lose everything the server stored after the backup.

1. Stop the server: `systemctl --user stop sessionhub`.
2. Copy the newest backup over the database:
   `cp ~/.local/share/sessionhub/sessionhub.db.bak-<timestamp> ~/.local/share/sessionhub/sessionhub.db`.
3. Delete `sessionhub.db-wal` and `sessionhub.db-shm` in the same directory if they exist.
4. Install the old binary on `tower`, then start the server:
   `systemctl --user start sessionhub`.
5. On each other machine, install the old binary.

To check that the watcher runs the installed binary, compare inodes:

```sh
p=$(cat ~/.local/state/sessionhub/watcher.lock)
stat -Lc %i /proc/$p/exe ~/.local/bin/sessionhub
```

## Troubleshoot

- **A client can't reach the server.** Run `sessionhub status`. Writes wait in
  `~/.local/state/sessionhub/queue.jsonl` and drain once the server answers.
- **The watcher isn't sending.** Read `~/.local/state/sessionhub/watcher.log` and
  `herdr plugin log list --plugin sessionhub`. See
  [`docs/plugin.md`](plugin.md#troubleshooting).
- **Clients get `403` through `https://sessionhub.example.com` but not on `tower` at
  `http://127.0.0.1:8787`.** If the response body is a Cloudflare page, Bot
  Fight Mode or Browser Integrity Check is challenging the Go client. The
  clients treat a `403` as an auth error and keep their queue. In the
  Cloudflare dashboard for `example.com`, turn off **Security** > **Bots** >
  **Bot Fight Mode**, or add a configuration rule for hostname
  `sessionhub.example.com` that turns off **Browser Integrity Check**.
- **The dashboard shows no Remote Control button.** The session's machine
  isn't polling: run `sessionhub install-plugin` there and look for `control:` lines
  in `~/.local/state/sessionhub/watcher.log`.

## Uninstall

On each client machine:

```sh
sessionhub uninstall-plugin
sessionhub uninstall-hooks
sessionhub uninstall-mod
sessionhub uninstall-mcp
rm -f ~/.config/sessionhub/config.toml
rm -rf ~/.local/state/sessionhub ~/.local/share/sessionhub/herdr-plugin
```

Then remove the snippet from `~/.claude/CLAUDE.md` and the `$hub_summary` row
from your herdr config. On a machine that isn't the server, also remove the
binary: `rm ~/.local/bin/sessionhub`.

To remove a machine from the server, run on `tower`:

```sh
sessionhub machine rm <name>          # prints what it would delete
sessionhub machine rm <name> --yes    # deletes the machine and its sessions
```

To remove the server from `tower`:

```sh
systemctl --user disable --now sessionhub
rm ~/.config/systemd/user/sessionhub.service
systemctl --user daemon-reload
rm ~/.local/bin/sessionhub
```

Delete the `sessionhub.example.com` public hostname in the Cloudflare dashboard. The
database (`~/.local/share/sessionhub/`) and `~/.config/sessionhub/server.toml` stay until
you delete them; there is no other copy of the report history.
