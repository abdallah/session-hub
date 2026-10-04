# Security

## Report a vulnerability

Please report security problems privately through
[GitHub's private vulnerability reporting](https://github.com/abdallah/session-hub/security/advisories/new),
not in a public issue. Include what an attacker needs (network access, a
machine token, a browser session) and what they can do with it. You should
get an answer within a week.

## What the server can do

sessionhub is built for one person and their own machines. Anyone who holds
a credential for the server can act on **every** joined machine, so treat
the credentials like SSH keys.

| Credential | Where it lives | What it can do |
|---|---|---|
| Machine token (`hub_m_...`) | `~/.config/sessionhub/config.toml` on each machine, mode `0600` | Read every session. Write as that machine. Allow or deny permission prompts, add and remove standing rules, send messages that are typed into sessions, move sessions, turn on Remote Control, start new sessions on any machine, and create browser sign-in links. |
| Browser session cookie | A signed-in browser, for 30 days after its last use | Read every session. Allow or deny permission prompts, edit rules, send messages, move sessions, turn on Remote Control, and start new sessions. It can't create sign-in links. |
| Sign-in link | Printed by `sessionhub login` | One use, valid for 10 minutes. Opening it and pressing **Sign in** creates a browser session. |

A message, a standing rule, or a new session's first prompt becomes part of
a Claude prompt, and an approved permission prompt runs a tool. Either credential can therefore get
Claude to run commands on your machines. That is the point of the tool, and
also the reason to guard it.

## Recommendations

- **Keep the server private when you can.** Listening on `127.0.0.1` or a
  private network (a VPN or Tailscale) is the safest setup. If you expose it
  through a tunnel or reverse proxy, use HTTPS.
- **Don't put the server behind a shared login page** that would let other
  people's accounts through. sessionhub does its own authentication; a
  login wall in front of it only breaks the clients.
- **Turn off remote approvals where you don't want them.** Add
  `remote_permissions = false` to a machine's
  `~/.config/sessionhub/config.toml`, or set
  `SESSIONHUB_REMOTE_PERMISSIONS=off`, and that machine's prompts can only be
  answered in its terminal.
- **Turn off remote starts where you don't want them.** Add
  `remote_start = false` to a machine's `~/.config/sessionhub/config.toml`,
  or set `SESSIONHUB_REMOTE_START=off`, and its watcher refuses to start new
  sessions. A start is always confined to a directory inside the machine's
  home directory, and never answers Claude's trust-this-folder prompt.
- **Turn on Telegram alerts.** Besides blocked sessions, the server sends a
  message whenever a standing rule is added, so a rule you didn't add is
  noticed.
- **Revoke what leaks.**
  - A machine token: run `sessionhub machine add <name>` on the server host
    to rotate it (the old token stops working at once), or
    `sessionhub join` again on that machine. `sessionhub machine rm <name>`
    removes the machine and its data.
  - A browser: `sessionhub login ls`, then `sessionhub login rm <name>`.
- **Back up the database** (`~/.local/share/sessionhub/sessionhub.db`). It
  holds prompts, progress reports, and tool inputs from permission prompts
  (cut to 8 KiB), which can include file paths and command lines.

## What sessionhub does not do

- It never adds an "always allow" permission rule. Each approval allows one
  prompt once.
- The server never connects to your machines. Machines poll it.
- A session move is encrypted between the two machines with their own keys;
  the server only relays the sealed bundle. Check the key fingerprints with
  `sessionhub machine ls` on the server and `sessionhub move-key` on each
  machine.
- Strings from other machines (titles, reports, paths) are cleaned before
  they reach a terminal, so an escape sequence in a title can't control
  yours.
