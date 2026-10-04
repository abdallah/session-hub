# Using sessionhub

- `sessionhub ls` lists live and blocked sessions on every machine; `--all` adds
  stale and ended ones.
- `sessionhub show <id-prefix>` prints one session with its reports and events.
- `sessionhub resume <id-prefix>` focuses the session's herdr pane, restarts it in
  herdr, or prints the `ssh` command for a session on another machine. The
  herdr plugin's **resume** action opens a picker. Add `--remote-control` to
  start Claude with Remote Control on.
- `sessionhub remote-control <id-prefix>` turns on Claude Code Remote Control for a
  running session on this machine and prints its `claude.ai/code` URL. For a
  session on another machine it asks that machine through the sessionhub, and
  prints the `ssh` command when that fails.
- The dashboard is at `https://sessionhub.example.com/`. To sign a browser in, run
  `sessionhub login --name <device>` on any joined machine, then open the printed
  link on that device or scan the QR code, and press **Sign in**. The browser
  stays signed in for 30 days after its last visit. `sessionhub login ls` lists
  signed-in browsers, and `sessionhub login rm <name>` signs one out.
- On the dashboard, **Remote Control** turns on Claude Code Remote Control for
  a session in herdr, and **Resume with Remote Control** restarts an ended one
  with it on, in a new herdr workspace on its machine. Tap **Open in Claude**
  to continue in the Claude app.
- The dashboard's **Inbox** tab, and `sessionhub inbox` in a terminal, list the
  sessions that need you: blocked on a prompt, waiting on you, or finished a
  turn you haven't answered. Open one from there, or dismiss or snooze it.
  Triage is shared across browsers and machines. When herdr marks a finished
  pane seen, its **Finished** item leaves on its own.
- A blocked session plays herdr's request sound with a notification on its
  machine and, after 30 seconds, sends you a Telegram message if you set
  that up (see [Turn on Telegram alerts](self-hosting.md#turn-on-telegram-alerts)).
- In herdr, the plugin's **sessionhub: inbox** action, or `sessionhub plugin open-inbox`,
  opens the inbox in a pane. **j** and **k** move, **Enter** jumps to the
  session on any machine, **d** dismisses, **s** snoozes, and **q** quits.
  A jump to another machine runs `ssh -o BatchMode=yes`, so it needs
  key-based SSH (see [Resume across machines](self-hosting.md#resume-across-machines));
  without it, it shows the resume command instead.
- Standing rules reach every Claude session on every machine, at its start
  and with every prompt. Add one with `sessionhub rules add "<text>"`, on the
  dashboard's **Rules** tab, or by asking Claude to `remember` it for all
  sessions; `sessionhub rules` lists them and `sessionhub rules rm <id>` removes one.
  Keep each to one line; the list holds 2,000 characters.
- `sessionhub send <id-prefix>... -m "<text>"` types a message into sessions,
  prefixed with who sent it, once each one's agent is idle. On the
  dashboard, **Select** the sessions and use the **Send a message** bar.
  Only sessions in herdr on a machine whose watcher runs can take one.
- When a session waits on a permission prompt, the inbox row shows the tool
  and command with **Allow once** and **Deny**, and the Telegram alert names
  them; `sessionhub approve <id-prefix>` and `sessionhub deny <id-prefix> ["reason"]` do
  the same in a terminal, after they show the full request and ask you to
  confirm. The prompt still shows in the terminal, and
  whichever answer comes first wins. sessionhub never adds an "always allow" rule.
  To keep a machine out of this, add `remote_permissions = false` to
  `~/.config/sessionhub/config.toml`.
- **New session** on the dashboard's Sessions tab, or
  `sessionhub start <machine> --dir <dir> [--trust] [-m "<first prompt>"]`,
  starts a new Claude session on any machine whose watcher runs, with Remote
  Control on, in a new herdr workspace, and gives you its **Open in Claude**
  link. The directory must be inside that machine's home directory, and
  Claude there must already trust it; if it doesn't, the start fails and
  says so. To trust that one folder as part of the start, pass `--trust` or
  tick **Trust this folder** on the dashboard. To keep a machine
  out of this, add `remote_start = false` to its
  `~/.config/sessionhub/config.toml`.
- `sessionhub move <id-prefix> tower` moves a session to `tower`: its conversation,
  its branch, and its uncommitted changes, resumed there in a new herdr
  workspace. `sessionhub move <id-prefix> cloud` hands a session in a GitHub
  repository to a new Claude Code cloud session. On the dashboard, expand
  the card and use **Move to…**. The session must be idle in herdr, the
  target needs a clean clone of the same repository under `~/Code` (or a
  `move_roots` entry in `~/.config/sessionhub/config.toml`), and both machines must
  run the same Claude Code version. The bundle is encrypted between the two
  machines; the server only relays it. Files such as `.env` stay behind, and
  the result says which. Press Ctrl+C to stop following; the move goes on,
  and the command exits with status 130 and prints the `sessionhub move --status`
  command that checks it.

Every command is documented in [`cli.md`](cli.md).
