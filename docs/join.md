# Joining a machine

`sessionhub join <server-ssh-target>` enrolls this machine: it gets a per-machine
token from the server host, writes the client config, and installs every
client that fits.

```sh
sessionhub join tower
sessionhub join tower --name bluebox --ssh-host bluebox.example.com --herdr-host bluebox.example.com
```

| Flag | Default | Meaning |
|---|---|---|
| `--name N` | short, lowercased hostname | Machine name. Letters, digits, `.`, `_`, `-`; at most 64 characters. |
| `--ssh-host H` | the name (set by the server) | Host that `sessionhub resume` uses to ssh here. |
| `--herdr-host H` | the name (set by the server) | Host for herdr remote attach. |

## What it does

1. Adds the machine and reads `{"name","token","server_url"}` from
   `sessionhub machine add ... --json`.
   - If a server config (`~/.config/sessionhub/server.toml`) and the database both
     exist on this machine, it runs the add in-process, with no SSH.
   - Otherwise it runs `ssh -o BatchMode=yes -- <target>
     ~/.local/bin/sessionhub machine add <name> ... --json`. `BatchMode` means ssh
     never prompts, so you need key-based login, and sessionhub must be installed at
     `~/.local/bin/sessionhub` on the server host.
2. Writes `~/.config/sessionhub/config.toml` (mode 0600).
3. Installs the clients, each step skipped with a message when its
   prerequisite is missing:
   - `install-plugin`, if `herdr` is on `PATH` and `herdr --version` reports
     0.9.3 or later.
   - `install-hooks`, always.
   - `install-mcp`, if `claude` is on `PATH`.
4. Calls `/healthz` with the new config and runs `sessionhub status`.
5. Prints two manual steps: paste the full CLAUDE.md snippet (printed between
   `--- begin CLAUDE.md snippet ---` and `--- end CLAUDE.md snippet ---`) into
   `~/.claude/CLAUDE.md`, and add a `$hub_summary` row under
   `[ui.sidebar.agents]` in the herdr config. sessionhub edits neither file.

## Behavior to know

- The printed snippet is embedded in the binary from
  `internal/join/CLAUDE-snippet.md`, a copy of `docs/CLAUDE-snippet.md`
  (`go:embed` cannot reach outside the package). A test fails when the two
  differ, so edit both together.
- When the server config and database are on this machine, `sessionhub join` adds
  the machine locally and prints one line saying the ssh target argument is
  ignored. The argument is still validated.

- The name and both hosts are checked against the server's rules before
  anything is sent, because the remote side of the SSH command is a shell. The
  target and hosts cannot start with `-`. Bad input fails before ssh runs.
- A failed install step or a failed health check is printed as `FAILED`, the
  remaining steps still run, and the command exits 1. A skipped step is not a
  failure. A `sessionhub status` failure is printed and does not fail the join.
- Running it again is safe. The server rotates the token, the config is
  rewritten, and the installers are idempotent.
- The config is written before the installers run, so a failed installer
  leaves an enrolled machine. Fix the cause and run `sessionhub join` again.

## Tests

`go test ./internal/join` uses a temp HOME, a `PATH` that holds only fake
`ssh`, `herdr`, and `claude` scripts, and injected installers. It covers the
exact ssh arguments, config mode, skip rules, herdr version comparison,
unsafe input, ssh and parse failures, an unreachable server, the local
shortcut against a real database, and a repeat join.
