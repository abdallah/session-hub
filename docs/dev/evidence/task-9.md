# Task 9 evidence: sessionhub join

Real `bin/sessionhub` built from this tree. A `sessionhub server` runs on `127.0.0.1:18799`
with a temporary database and `public_url` set to that address. Fake `ssh`,
`herdr`, and `claude` scripts are first on `PATH` and record their arguments.
The fake `ssh` drops the ssh options and target, rewrites `~/.local/bin/sessionhub` to
the built binary, and runs the command with the server's `SESSIONHUB_DB` and
`SESSIONHUB_SERVER_CONFIG`. The client HOME is a temp directory; nothing touches the
real `~/.config/sessionhub`, `~/.claude`, or herdr. `<tmp>` stands for the scratch
directory.

## Full join over ssh

Client has no server config or database, so it uses ssh.

```
$ sessionhub join tower --name evidence-a --ssh-host a.lan --herdr-host a.lan
Adding machine "evidence-a" on tower over ssh.
Wrote <tmp>/client/.config/sessionhub/config.toml (mode 0600).
Installing the herdr plugin.
FAILED  install-plugin: install-plugin: not implemented yet
Warning: <tmp>/client/.local/bin/sessionhub does not exist yet; hooks and the MCP server call that path. Run `make install`.
Installing the Claude Code hooks.
Installed sessionhub hooks in <tmp>/client/.claude/settings.json
Registering the MCP server with Claude Code.
Registered the sessionhub MCP server: <tmp>/client/.local/bin/sessionhub mcp
Health check passed: http://127.0.0.1:18799 is reachable.

sessionhub status:
Could not print status: status: not implemented yet

Two manual steps remain. ... (CLAUDE-snippet.md and the [ui.sidebar.agents] row)
join: enrolled evidence-a, but these steps failed: install-plugin
exit=1
```

The plugin installer and `sessionhub status` are still stubs on this base (Task 4 and
the CLI task), so they report as failure and as a note. Recorded arguments:

```
ssh:    -o BatchMode=yes -- tower ~/.local/bin/sessionhub machine add evidence-a --ssh-host a.lan --herdr-host a.lan --json
herdr:  --version
claude: mcp add --scope user sessionhub -- <tmp>/client/.local/bin/sessionhub mcp
config: mode 600; server_url = "http://127.0.0.1:18799", token redacted, machine = "evidence-a"
settings.json: 5 sessionhub hook entries
server `sessionhub machine ls`: evidence-a  a.lan  a.lan
```

## Local shortcut and repeat join

HOME `client2` has `SESSIONHUB_DB` and `SESSIONHUB_SERVER_CONFIG` pointing at the server's
files. The ssh log stays at 1 line (from the first run) across two joins.

```
--- run 1: sessionhub join tower --name evidence-b
Server config and database found here: adding machine "evidence-b" locally.
Health check passed: http://127.0.0.1:18799 is reachable.
token = "hub_m_jfIPR...
--- run 2
Server config and database found here: adding machine "evidence-b" locally.
Health check passed: http://127.0.0.1:18799 is reachable.
token = "hub_m_5cT50...        (rotated)
ssh.log lines after: 1
NAME        SSH_HOST    HERDR_HOST  LAST_SEEN
evidence-a  a.lan       a.lan       never
evidence-b  evidence-b  evidence-b  never
```
