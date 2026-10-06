# A Claude Code mod for sessionhub

Date: 2026-10-06. Status: approved in chat ("let's just develop it"),
after an evaluation of Claude Code mods (launched 2026-10-01, v2.1.287).

This spec was written before the project was renamed from `hub` to
`sessionhub`; the names here are updated. It was first planned as schema
version 10; it shipped as version 12. The shipped behavior differs in places
(for example, only the message poll refreshes `mod_seen_at`, a question is
never queued offline, a clear names the question it clears, and the status
line has no prefix). `docs/server.md` and `docs/cli.md` describe what
shipped.

## Goal

Add a Claude Code mod to sessionhub that works inside each session, next to the
existing hooks and the herdr watcher:

1. **Blocked on what:** report the question Claude asks you
   (`AskUserQuestion`), so the inbox, dashboard, and Telegram say what a
   blocked session waits on.
2. **Live usage:** report the context window fill and cost after each turn.
3. **Messages without herdr:** a session with the mod receives sessionhub messages
   itself, so sessions outside herdr can be messaged.
4. **Inbox count:** a status line in Claude Code shows how many sessions
   need you.

The hooks and the herdr watcher stay. The mod adds to them; nothing that
works today depends on the mod.

## Facts this rests on

From the mods docs and the declarations Claude Code 2.1.291 writes:

- A mod is a plugin directory: `.claude-plugin/plugin.json`,
  `hooks/hooks.json` with `"modules": ["./register.js"]`, and an ES module
  exporting `register(on)`. `claude plugin validate <dir>` and `claude
  plugin test` check and test it. `CLAUDE_CODE_PLUGIN_DIRS` (in `env` of
  `~/.claude/settings.json`) loads a directory in every session.
- `$.process.run(argv, { stdin, timeoutMs })` runs a program without a
  shell (30 s default, 10 min max). `$.clock.after(ms, fn)` schedules work.
  `$.prompt.submit({ text })` starts a turn once the session is idle.
  `$.session.id()`, `$.session.cwd()` and `$.session.usage()` return
  promises. `$.ui.status(text)` sets a status line.
- `tool.call` fires before a tool runs; for `AskUserQuestion`, `e` carries
  `questions[]` (`question`, `header`, `options[].label`, `multiSelect`),
  and `next(e)` resolves once the user answered.
- `session.measure` fires after each turn with `context` (`tokens`,
  `window`, `percent`), `rateLimits`, and `cost`.
- A hook's own time is capped at 10 s, not counting time inside `next` or
  mods API calls. Mods load only in trusted folders, not in `-p` or
  `dontAsk` runs, and not with `--safe-mode`.

## Design

### The mod calls `sessionhub`, never the network

The mod never reads the token or calls the server. Every action is
`$.process.run([sessionhub, "mod", <sub>, ...], { stdin })`, where `sessionhub` is the
absolute binary path written into the installed mod. Go keeps auth,
offline queueing, and cleaning. The mod is small and has no secrets.

### Server (schema version 12)

`sessions` gains:

- `blocked_on TEXT NOT NULL DEFAULT ''`: one line, at most 300 runes,
  cleaned, such as `Question: Which library should we use? (date-fns,
  luxon, dayjs)`.
- `context_percent INTEGER` (NULL until reported), `usage_at`.
- `live_cost_usd REAL` (NULL until reported).
- `mod_seen_at` (when the session's mod last polled).

Routes (machine token of the session's owning machine, like the hooks'
routes):

| Route | Does |
|---|---|
| `POST /v1/sessions/{id}/blocked-on` `{"text": "..."}` | Sets `blocked_on` and `agent_state=blocked` (empty text clears `blocked_on` and sets `working`). Records a `blocked_on` event. |
| `PUT /v1/sessions/{id}/usage` `{"context_percent", "cost_usd"}` | Stores the numbers and `usage_at`. |
| `GET /v1/sessions/{id}/messages/next?wait=25` | Long-polls: claims the oldest queued message for this session (same rules as `ClaimMessage`: order, 15 s re-offer), returns `api.ControlClaim`-like `{id, text}` or `204`. Sets `mod_seen_at` on every call. |
| `POST /v1/messages/{id}/result` `{"state": "delivered"|"busy"|"refused", "detail"}` | Same transitions as the watcher's control result for messages. |

Rules:

- Any state change to `working` or `idle` from the hooks clears
  `blocked_on`.
- A session counts as **controllable for messages** when it has a herdr
  pane on a polling machine (today's rule) **or** `mod_seen_at` is within
  `ControlPollWindow`. `Session.Controllable` keeps meaning herdr control
  (Remote Control and moves still need herdr); add `Session.Messageable`.
- The watcher's `ClaimMessage` skips sessions whose `mod_seen_at` is
  fresh, so a session with the mod gets its messages from the mod only.
- `api.Session` gains `blocked_on`, `context_percent`, `live_cost_usd`,
  `messageable`.
- The inbox's blocked item, the Telegram blocked alert, the dashboard card,
  and `sessionhub ls`/`sessionhub show` show `blocked_on` when set. The dashboard card
  shows `context NN%` when known. Text goes in with `textContent`.

### CLI: `sessionhub mod`

Internal subcommands the mod calls (hidden from `sessionhub help` except one
line). Each reads the session ID from `--session`:

- `sessionhub mod blocked-on --session ID` reads `{"questions": [...]}` (the
  `AskUserQuestion` input) or `{"text": ""}` on stdin, formats the line,
  and posts it. Queued offline like the hooks.
- `sessionhub mod usage --session ID` reads `{"context": {...}, "cost": {...}}` on
  stdin and puts the usage. Skips the call when nothing changed by at least
  1 percent point or $0.01 since the last report (state file per session).
- `sessionhub mod poll --session ID` long-polls once (25 s) and prints the claim
  as JSON, or nothing. Exit 0 either way; non-zero only on a usage error.
- `sessionhub mod result --message ID --state S [--detail D]` posts the result.
- `sessionhub mod inbox-count` prints `{"blocked": n, "waiting": n, "finished":
  n}` from `GET /v1/inbox`.

`sessionhub install-mod` / `sessionhub uninstall-mod`:

- Writes the mod from files embedded in the binary (`go:embed`) to
  `~/.local/share/sessionhub/claude-mod/`, with the absolute `sessionhub` path filled
  into `hooks/config.js`. Atomic write, restore on failure, like
  `install-plugin`.
- Adds that directory to `env.CLAUDE_CODE_PLUGIN_DIRS` in
  `~/.claude/settings.json` (keeping other entries, `:`-separated), with the
  same backup and formatting-preserving edit as `install-hooks`. Uninstall
  removes only that entry.
- `make deploy` and `make install` users run `sessionhub install-mod` once; `sessionhub
  install-hooks` prints a hint when the mod isn't installed.

### The mod (`internal/claudemod/mod/`)

`hooks/register.js`:

- `session.start`: reads `$.session.id()`; starts the message loop and the
  inbox-count loop; returns `next(e)`.
- `tool.call` with matcher `{ tool: 'AskUserQuestion' }`: runs `sessionhub mod
  blocked-on` with the questions, then `const r = await next(e)`, then runs
  `sessionhub mod blocked-on` with `{"text": ""}`, and returns `r`. Errors from
  `sessionhub` never block the tool.
- `session.measure`: runs `sessionhub mod usage` with the event's `context` and
  `cost`; never blocks.
- Message loop: `sessionhub mod poll` (timeout 35 s); on a claim, `await
  $.prompt.submit({ text })`, then `sessionhub mod result --state delivered`; if
  submit throws, `--state busy`. Reschedules itself with `$.clock.after`
  (1 s after a claim or an empty poll, 30 s after a failure). Stops on
  `session.end`.
- Inbox loop: every 60 s, `sessionhub mod inbox-count`; `$.ui.status("sessionhub: 2
  blocked · 1 waiting")`, or clears it when all counts are 0.
- Reentrancy: one loop of each kind per mod instance; a reload cancels the
  old timers (store the handles; `session.start` fires again after a
  reload).
- Tests (`claude plugin test`): blocked-on is posted and cleared around an
  `AskUserQuestion` call; a `sessionhub` failure doesn't block the tool; usage is
  passed through; a polled message is submitted and reported delivered;
  the status line text. `$.process.run` is stubbed in tests.

## Security

- The mod has no token and no network access of its own.
- Every text the server stores from the mod is cleaned with `termtext`
  and length-capped server-side.
- The message route only hands a session's messages to the machine that
  owns the session.

## Testing

- Store: v11 → v12 upgrade; blocked_on set/clear; usage; mod claim and
  watcher skip; messageable rule.
- Server: auth matrix rows for the four routes; long-poll timing.
- CLI: `sessionhub mod` subcommands against a test server; offline queue for
  blocked-on; usage throttling.
- Install: settings edit keeps other `CLAUDE_CODE_PLUGIN_DIRS` entries,
  backs up, and uninstall reverses it; embedded files written atomically.
- Mod: `claude plugin validate` passes; `claude plugin test` passes.
- Live: a scratch session outside herdr receives a `sessionhub send` message; an
  `AskUserQuestion` shows in the inbox with its text; the dashboard shows
  context %.

## Out of scope

- Answering questions or permission prompts from the mod.
- Drawing panes; the status line is the only UI.
- Replacing the hooks or herdr.
