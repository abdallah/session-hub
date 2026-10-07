# Environment findings (step 1)

Captured on 2026-09-30 on machine `bluebox` (WSL2, Linux 5.15). Everything under
"Observed locally" comes from running the installed binaries. Everything under
"From docs" cites the source URL.

## Versions

| Tool | Version | Notes |
|---|---|---|
| herdr | `herdr 0.8.2` (stable channel, protocol 20) | server running, socket `/home/me/.config/herdr/herdr.sock` |
| Claude Code | `2.1.284 (Claude Code)` | 2.1.285 is also downloaded under `~/.local/share/claude/versions/` |
| Go | `go1.25.6 linux/amd64` | |
| litestream | not installed | only needed inside the container |
| gcloud | not installed | needed on the deploy machine for `make deploy` |

## herdr 0.8.2: observed locally

### CLI surface

`herdr plugin --help` lists: `install`, `uninstall`, `link`, `unlink`, `enable`,
`disable`, `list`, `config-dir`, `action`, `log`, `pane`.

- `herdr plugin install [OPTIONS] <OWNER/REPO[/SUBDIR]>` with `--ref <REF>`, `-y, --yes`.
- `herdr plugin link [OPTIONS] <PATH>` with `--disabled`, `--enabled`.
- `herdr plugin action` has `list` and `invoke`.
- `herdr plugin log list [--plugin <ID>] [--limit <N>]`. Log entries record
  command, event, exit code, stdout, stderr, and timing. They do **not** record
  the event payload, so real payloads must be captured with a probe plugin (see
  "Payload capture plan").

`herdr api` has `snapshot` (live session snapshot) and `schema` (bundled JSON
schema, `--json` or `--output PATH`). The schema is protocol 20, schema_version 1,
with sub-schemas `error_response`, `event`, `request`, `subscription_event`,
`success_response`.

### Remote attach

`herdr --help`: `herdr --remote <ssh-target> [--session <name>]` — "Attach through
SSH to a remote Herdr server". There is no `herdr machine` command in 0.8.2.

### `agent_session` shape (captured from `herdr api snapshot`)

On both `snapshot.panes[]` and `snapshot.agents[]`:

```json
"agent": "claude",
"agent_session": {
  "agent": "claude",
  "kind": "id",
  "source": "herdr:claude",
  "value": "ed0904a1-eba3-4b41-8fc0-71156ddbd47f"
},
"agent_status": "done",
"cwd": "/home/me/Code/OTGS/wpml-org/app",
"pane_id": "wA:p1",
"tab_id": "wA:t1",
"workspace_id": "wA",
"terminal_id": "term_65c86b838db451",
"label": "17619 lead: CI pipeline reshape",
"terminal_title_stripped": "Alertra checks validation and synthetic probes"
```

Schema (`AgentSessionInfo`): required `source`, `agent`, `kind`, `value`;
`kind` is `"id"` or `"path"`. `agent_session` is `null`/absent on panes with no
agent (a plain shell pane had no `agent` or `agent_session` key). `agent_status`
enum: `idle`, `working`, `blocked`, `done`, `unknown`.

`PaneInfo` also carries `display_agent`, `title`, `state_labels` (map),
`tokens` (map, ≤32), `foreground_cwd`, `revision`.

Pane IDs look like `wA:p1` and are scoped to the herdr session. The snapshot does
not include the herdr session name; the plugin gets it from `herdr session list`
or config.

### Events

Manifest `[[events]]` use dot names (from installed manifests, e.g.
`on = "pane.agent_status_changed"`). The socket `events.subscribe` `Subscription`
types in the schema are:

`workspace.created`, `workspace.updated`, `workspace.metadata_updated`,
`workspace.renamed`, `workspace.moved`, `workspace.reordered`, `workspace.closed`,
`workspace.focused`, `worktree.created`, `worktree.opened`, `worktree.removed`,
`tab.created`, `tab.closed`, `tab.focused`, `tab.renamed`, `tab.moved`,
`pane.created`, `pane.closed`, `pane.updated`, `pane.focused`, `pane.moved`,
`pane.exited`, `pane.agent_detected`, `pane.output_matched` (needs pane_id),
`pane.agent_status_changed` (needs pane_id), `pane.scroll_changed` (needs
pane_id), `layout.updated`.

There is no `server.ready` or startup-complete event. The resurrect manifest
says so directly: "herdr has no 'server ready' plugin event today, and its
manifest validator rejects unknown event names". The second half is wrong for
0.8.2: unknown names link with a warning (see "From docs").

Not every subscribable type can be a manifest `[[events]]` hook. Hookable in
0.8.2 (`PLUGIN_HOOK_EVENT_KINDS`): the `workspace.*`, `worktree.*`, and `tab.*`
lifecycle events, plus `pane.created`, `pane.closed`, `pane.focused`,
`pane.moved`, `pane.exited`, `pane.agent_detected`, `pane.agent_status_changed`.
`pane.updated`, `pane.output_changed`, `layout.updated`, and
`workspace.metadata_updated` are socket-subscription only.

Event payload shapes relevant to sessionhub (`schemas.event.$defs.EventData`):

| Event | Data |
|---|---|
| `pane_created` | `pane: PaneInfo` |
| `pane_updated` | `pane: PaneInfo` |
| `pane_closed` | `pane_id`, `workspace_id` |
| `pane_exited` | `pane_id`, `workspace_id` |
| `pane_agent_detected` | `pane_id`, `workspace_id`, `agent?`, `final_status?`, `released` |
| `pane_agent_status_changed` | `pane_id`, `workspace_id`, `agent_status`, `agent?`, `display_agent?`, `title?`, `state_labels` |

`pane_agent_status_changed` does not carry `agent_session`, so the handler must
look the pane up (`herdr pane get <id>`) to get the session UUID, or use a cache
built from the startup snapshot and `pane_created`/`pane_updated`.

### How plugin commands receive context (from installed plugins' sources)

Installed plugins read these environment variables: `HERDR_PLUGIN_EVENT_JSON`
(event payload), `HERDR_PLUGIN_CONTEXT_JSON` (action context), `HERDR_PLUGIN_ROOT`,
`HERDR_PLUGIN_STATE_DIR`, `HERDR_PLUGIN_CONFIG_DIR`, `HERDR_PLUGIN_ID`,
`HERDR_BIN_PATH`, `HERDR_SOCKET_PATH`, `HERDR_WORKSPACE_ID`, `HERDR_PANE_ID`.
Example: `claude-auto-retry/bin/main.js:66`:
`JSON.parse(process.env.HERDR_PLUGIN_EVENT_JSON || '{}')`; `reviewr/herdr/pane.sh`
reads `.data.already_open` from it, so the payload is `{event, data}`-shaped.

The `claude-auto-retry` manifest notes that "herdr spawns plugin commands with no
shell and with the server's PATH", so the manifest must call the Go binary by
`$HERDR_PLUGIN_ROOT`-relative or absolute path.

### `[[startup]]` and long-running work

- `[[startup]]` exists (used by `claude-auto-retry`, `min_herdr_version = "0.7.5"`).
  Its comment: "Re-attach monitors as soon as herdr has restored the session (and
  again after a live handoff). herdr emits no event for restored panes".
  So startup runs after restore and again after a live handoff.
- No supervised daemon mechanism is visible in any manifest. The `worklog` plugin
  starts a detached background process from `[[startup]]` with a `pgrep` guard:
  `pgrep -f 'worklog watch' >/dev/null || nohup ... &`. That is the existing
  pattern for a long-running subscriber, and it is unsupervised.
- `[[build]]` runs on `herdr plugin install` but not on `herdr plugin link`
  (pickr and reviewr manifests say so). reviewr's build step downloads a
  prebuilt binary from GitHub Releases.

### `pane report-metadata` flags (spec correction)

`herdr pane report-metadata [OPTIONS] --source <ID> <PANE_ID>` with `--agent`,
`--applies-to-source`, `--title`, `--clear-title`, `--display-agent`,
`--clear-display-agent`, `--state-label <STATUS=TEXT>`, `--clear-state-labels`,
`--token <NAME=VALUE>`, `--clear-token <NAME>`, `--seq <N>`, `--ttl-ms <N>`.

**There is no `--custom-status` flag in 0.8.2.** `--source` is required. The
candidates for a short summary are `--title` or a `--token`. Which one the
sidebar renders is an open question for the plan.

### Other relevant commands

- `herdr agent start <NAME> --kind claude --pane <ID> [--timeout <MS>] [-- AGENT_ARG...]`:
  "The pane must be at its interactive shell prompt." This is the native way to
  start `claude --resume <uuid>` in a pane (`-- --resume <uuid>`).
- `herdr agent focus <target>`, `herdr pane get <pane_id>`, `herdr pane list [--workspace]`,
  `herdr pane split` (socket `pane.split` takes `cwd`, `direction`, `focus`,
  `target_pane_id`, `workspace_id`), `herdr workspace create`.
- `herdr pane report-agent-session --source <ID> --agent <LABEL> <PANE_ID>
  [--agent-session-id <ID>] [--agent-session-path <PATH>] [--session-start-source <SOURCE>]`.

### herdr's Claude Code integration

`herdr integration status`: `claude: current (v8) (/home/me/.claude/hooks/herdr-agent-state.sh)`.
It registers one hook in `~/.claude/settings.json`: `SessionStart`, matcher `*`,
`bash '/home/me/.claude/hooks/herdr-agent-state.sh' session`, timeout 10.
The script reads the hook JSON from stdin and, if `HERDR_ENV=1`,
`HERDR_SOCKET_PATH`, and `HERDR_PANE_ID` are set, sends `pane.report_agent_session`
over the socket with `source "herdr:claude"`, `agent_session_id = session_id`,
`agent_session_path = transcript_path`, and `session_start_source = source`.
It skips subagents (`agent_id` present). The file header says "managed by herdr;
reinstalling or updating the integration overwrites this file. add custom hooks
beside this file instead of editing it."

### Environment in agent panes

Read from `/proc/<pid>/environ` of the three `claude` processes running in herdr
panes. Each has:

```
HERDR_BIN_PATH=/home/me/.local/bin/herdr
HERDR_ENV=1
HERDR_PANE_ID=wA:p1
HERDR_SOCKET_PATH=/home/me/.config/herdr/herdr.sock
HERDR_TAB_ID=wA:t1
HERDR_WORKSPACE_ID=wA
```

So `HERDR_PANE_ID` and `HERDR_BIN_PATH` are set in agent panes, not only in
plugin context. A `claude` process outside herdr (this session) has none of them.

## Claude Code 2.1.284: observed locally

- `claude --resume <session-id>` resumes by UUID; `--session-id <uuid>` sets one;
  `--fork-session` exists.
- `claude mcp add [options] <name> <commandOrUrl> [args...]` with
  `-s, --scope <scope>` "(local, user, or project) (default: "local")",
  `-e, --env`, `-t, --transport` (stdio default).
- MCP server processes get these variables from Claude Code (read from
  `/proc/<pid>/environ` of this session's MCP children):
  `CLAUDE_CODE_SESSION_ID=<uuid>`, `CLAUDE_PROJECT_DIR`, `CLAUDE_CODE_ENTRYPOINT=cli`,
  `CLAUDECODE=1`. The MCP tool can identify its session from
  `CLAUDE_CODE_SESSION_ID` with no herdr or hooks dependency. Open question:
  whether the value is updated after `/clear` or an in-session `/resume` (the MCP
  process keeps its original environment).
- Hook event names present as strings in the 2.1.284 binary: `PreToolUse`,
  `PostToolUse`, `PostToolUseFailure`, `PermissionRequest`, `UserPromptSubmit`,
  `Stop`, `StopFailure`, `SubagentStart`, `SubagentStop`, `SessionStart`,
  `SessionEnd`, `Notification`, `PreCompact`, `PostCompact`, `TeammateIdle`,
  `TaskCompleted`, `WorktreeCreate`, `WorktreeRemove`, `Elicitation`,
  `ElicitationResult`, `FileChanged`, `CwdChanged`, `InstructionsLoaded`,
  `ConfigChange`. This is a hint only; see "From docs" for the authoritative list.

## Payload capture plan

The tests must use real payloads. Captured so far: the `herdr api snapshot`
output (panes, agents, `agent_session`). Still needed, captured during
implementation:

- herdr event payloads (`HERDR_PLUGIN_EVENT_JSON`) for `pane.created`,
  `pane.closed`, `pane.exited`, `pane.agent_detected`, `pane.agent_status_changed`,
  and the `[[startup]]` and action environments. Method: `herdr plugin link` a
  throwaway probe plugin whose command writes its environment to a file, trigger
  the events, then `herdr plugin unlink` it. This changes the live herdr setup
  briefly, so it needs approval first.
- Claude Code hook stdin for `SessionStart`, `UserPromptSubmit`, `Stop`,
  `Notification`, `SessionEnd`, from a probe hook in a scratch project's
  `.claude/settings.local.json`.

Testdata will have home paths and workspace labels scrubbed before commit.

## From docs

Sources: herdr repo `github.com/herdrdev/herdr` at tag `v0.8.2` (commit
`9eb5214`), docs at https://herdr.dev/docs/0.8.2/ (page sources under
`docs/next/website/src/content/docs/<page>.mdx` at that tag). Current herdr
stable is 0.9.3; everything here is pinned to 0.8.2.

### herdr plugins (`plugins.mdx`, `src/app/api/plugins/`)

- Manifest top level: `id`, `name`, `version`, `min_herdr_version` (required),
  `description`, `platforms` (`linux`, `macos`, `windows`). Tables:
  `[[build]]`, `[[startup]]`, `[[actions]]` (`id`, `title`, `command`,
  `description`, `contexts` ∈ `global|workspace|tab|pane|selection`),
  `[[events]]` (`on`, `command`), `[[panes]]`, `[[link_handlers]]`. Local ids
  allow letters, digits, colon, underscore, hyphen, "but not dots".
- "`command` values are argv arrays. Herdr does not run them through a shell."
  Commands run with the plugin directory as cwd; a relative program with a slash
  (`./bin/sessionhub`) resolves against the plugin root.
- Startup: "`[[startup]]` commands run once for each enabled plugin after Herdr
  restores the session and its API socket is ready. They run again when a new
  server takes over during live handoff, but not when a client attaches, config
  reloads, or a plugin is linked or enabled." And: "Startup hooks are one-shot
  initialization commands rather than supervised daemons."
- Unknown event names: "An unrecognised name does not block the link, but the
  returned plugin info includes a warning" (`socket-api.mdx`).
- Environment: "Herdr injects `HERDR_SOCKET_PATH`, `HERDR_BIN_PATH`,
  `HERDR_ENV=1`, `HERDR_PLUGIN_ID`, `HERDR_PLUGIN_ROOT`, `HERDR_PLUGIN_CONFIG_DIR`,
  `HERDR_PLUGIN_STATE_DIR`, `HERDR_PLUGIN_CONTEXT_JSON`, and any available
  `HERDR_WORKSPACE_ID`, `HERDR_TAB_ID`, and `HERDR_PANE_ID`." "Action commands
  also receive `HERDR_PLUGIN_ACTION_ID`; startup and event hooks receive
  `HERDR_PLUGIN_EVENT` (`startup` for startup hooks), event hooks additionally
  receive `HERDR_PLUGIN_EVENT_JSON`". In plugin context `HERDR_PANE_ID` is the
  focused pane, not "the pane the event is about".
- Payload: only in `HERDR_PLUGIN_EVENT_JSON`, serialized `EventEnvelope`
  `{"event": "<snake_case kind>", "data": {"type": ..., ...}}`. Inferred from
  serde attributes; no literal example in docs or tests, so capture one.
- Execution (`runtime.rs`): no timeout; each command on its own thread, so
  handlers do not block herdr; `MAX_PLUGIN_COMMANDS_IN_FLIGHT = 32`, beyond
  which a command fails with `plugin_command_limit_reached`; stdout/stderr
  capped at 64 KiB; log keeps the last 200 entries.
- A startup command that never exits holds one of the 32 slots, so a
  long-running subscriber must detach itself.
- Install: "`plugin install` accepts GitHub shorthand only ... clones with
  `git` ... runs supported build commands". "`plugin link` does not run build
  commands". Build commands "do not receive runtime plugin context or Herdr
  socket env", and herdr "does not install missing toolchains". No release-asset
  download mechanism; reviewr's `[[build]]` script downloads a prebuilt binary
  itself.

### herdr socket API (`socket-api.mdx`)

- "newline-delimited JSON over a local socket". Request
  `{"id":"req_1","method":"ping","params":{}}`; success `{"id":..,"result":{..}}`;
  error `{"id":..,"error":{"code":"not_found","message":"pane not found"}}`.
- Socket path: `~/.config/herdr/herdr.sock`, or
  `~/.config/herdr/sessions/<name>/herdr.sock` for a named session. Resolution:
  `--session`, then `HERDR_SOCKET_PATH`, then `HERDR_SESSION`, then default.
- `events.subscribe`: "The first response acknowledges the subscription. Later
  lines are pushed events." Recommended pattern: bootstrap with
  `session.snapshot`, then "subscribe to resource events and update the local
  cache from those events."
- `agent_session`: "`pane.get`, `pane.list`, `agent.get`, and `agent.list`
  expose a read-only `agent_session` object ... If no native session reference
  is stored, the field is omitted."

### herdr agent panes (`integrations.mdx`, `cli-reference.mdx`)

- "An agent running in a Herdr pane inherits `HERDR_ENV`, `HERDR_PANE_ID`,
  `HERDR_BIN_PATH`, and `HERDR_SOCKET_PATH`." Matches the `/proc` observation.
- Caveats: popups do not get `HERDR_PANE_ID`; after a cross-workspace
  `pane move`, "the running process keeps its launch-time `HERDR_PANE_ID` ...
  Herdr retains the old pane ID as an alias".
- Claude integration: "The hook reports Claude Code session identity to the
  local Herdr socket on session start. Claude Code state comes from Herdr's
  screen manifest detection." One `SessionStart` hook; the installer removes
  older herdr entries on other events.

### Remote attach (`persistence-remote.mdx`)

- `herdr --remote <ssh-target>`: "your local Herdr is a thin client. It connects
  over SSH, starts or attaches to the remote Herdr server, and streams the UI
  back to your local terminal." Targets like `workbox` or `ssh://you@server:2222`;
  supports `--session`.
- `herdr machine` arrived in 0.9.0 and `herdr --machine` in 0.9.1. Neither is
  in 0.8.2, so sessionhub prints `herdr --remote <herdr_host> [--session <name>]`.

### Prior art (described, not copied)

- **ntindle/herdr-resurrect** (Node, installed here at `5afa675`): snapshots
  and restores workspaces, tabs, panes, cwd, and agents; resumes Claude with
  `claude --resume <id>` from herdr's session reference. No `[[startup]]`
  block. Boot detection: "the plugin detects a fresh boot from herdr's socket
  file (rewritten on every start) and lets the **first** event handler claim
  that boot, rehydrate once" — a boot token from the socket file's content and
  mtime, claimed with an atomic `wx` lock file. It predates `[[startup]]`
  (`min_herdr_version = "0.7.0"`).
- **abhirup-dev/herdr-resurrect** (Go): `[[build]] command = ["go","build","-o","bin/herdr-resurrect","."]`,
  actions call `./bin/herdr-resurrect <verb>`. Installing from GitHub needs a Go
  toolchain on the client.
- **jermen/herdr-plugin-agent-usage** (Python stdlib): requires
  `min_herdr_version = "0.9.1"`, so it will not install on 0.8.2. Its daemon
  pattern is the one to follow: `[[startup]]` and `[[events]]` all run
  `usage.py start`, which takes an `flock` on a lock file and returns if a
  watcher holds it; otherwise it spawns the watcher in a new session with stdin
  at `/dev/null` and exits. The watcher polls `session.snapshot` over the socket
  every 30 s and exits when the socket's `(st_dev, st_ino)` changes or the
  socket disappears ("A new Herdr instance has its own startup hook").

### Claude Code hooks (https://code.claude.com/docs/en/hooks, docs at 2.1.285)

- Events used by the hooks client exist: `SessionStart`, `UserPromptSubmit`,
  `Notification`, `Stop`, `SessionEnd`.
- `SessionStart` matchers: `startup`, `resume`, `clear`, `compact`, `fork`.
  `SessionEnd` `reason`: `clear`, `resume`, `logout`, `prompt_input_exit`,
  `other`.
- Common stdin fields: `session_id`, `transcript_path`, `cwd`,
  `hook_event_name`, `permission_mode` ("Not all events receive this field"),
  `agent_id`/`agent_type` in subagents. Event fields: `SessionStart` `source`
  (+ optional `model`, `session_title`); `UserPromptSubmit` `prompt`;
  `Notification` `message`, `title?`, `notification_type`; `Stop`
  `stop_hook_active`, `last_assistant_message`; `SessionEnd` `reason`.
- Blocking: "By default, hooks block Claude's execution until they complete."
  `"async": true` (command hooks) "runs in the background without blocking".
  "`SessionEnd` hooks share a 1.5-second budget".
- Settings schema: `{"hooks":{"<Event>":[{"matcher":"…","hooks":[{"type":"command","command":"…","timeout":N,"async":bool}]}]}}`.
- `CLAUDE_CODE_SESSION_ID` (env-vars page): "Set automatically to the current
  session ID in Bash and PowerShell tool subprocesses, hook command
  subprocesses, and stdio MCP server subprocesses. For Bash, PowerShell, and
  hooks this matches the `session_id` field in the hook JSON input and is
  updated on `/clear`. An MCP server subprocess retains the ID it was spawned
  with." **So the MCP tool's session ID goes stale after `/clear` or an
  in-session `/resume`.** Plan must handle this.

### Cloud Run (https://docs.cloud.google.com/sdk/gcloud/reference/run/deploy)

- `--min-instances`, `--max-instances` (per revision; `--min`/`--max` are
  service-wide).
- `--no-cpu-throttling` for CPU always allocated (instance-based billing),
  which requires "at least 512MiB of memory".
- `--execution-environment=gen2`.
- `--set-secrets=KEY=SECRET_NAME:VERSION`: "All other keys correspond to
  environment variables."
- `--startup-probe=httpGet.path=/healthz,httpGet.port=...,initialDelaySeconds=...,failureThreshold=...,timeoutSeconds=...,periodSeconds=...`
  is a CLI flag; no YAML needed.
- `--memory=512Mi`, `--cpu=1`.
- Below 1 vCPU (https://docs.cloud.google.com/run/docs/configuring/services/cpu)
  requires request-based billing and gen1, so it is incompatible with
  always-allocated CPU.

### Cost estimate (https://cloud.google.com/run/pricing, tier 1, instance-based)

$0.000018/vCPU-s and $0.000002/GiB-s. Free tier 240,000 vCPU-s and
450,000 GiB-s per month. 730 h = 2,628,000 s.

| Item | Without free tier | With free tier |
|---|---|---|
| 1 vCPU | $47.30 | $42.98 |
| 0.5 GiB | $2.63 | $1.73 |
| **Total** | **≈ $49.93/month** | **≈ $44.71/month** |

**This is meaningfully above a small VPS** (a few dollars a month for a VPS that
runs this binary easily). The spec asks me to say so before the first deploy.

### Litestream (https://litestream.io/reference/config/)

- Latest stable v0.5.17 (2026-08-31). Asset
  `litestream-0.5.17-linux-x86_64.tar.gz`; checksums in release asset
  `checksums.txt`: `cfb371176d164437ae869f8351cfde49bd1804ae71c61923f75c9cba9c9c006d`.
- **Spec correction:** the GCS scheme is `gs://`, not `gcs://`. v0.5 uses a
  single `replica:` key per database ("Each database now supports only a single
  replica"):
  ```yaml
  dbs:
    - path: /data/sessionhub.db
      replica:
        url: gs://BUCKET/sessionhub.db
  ```
- `litestream restore -if-db-not-exists -if-replica-exists /data/sessionhub.db`, then
  `litestream replicate -exec "sessionhub server"` ("Litestream will exit when the
  child process exits"). v0.5.6+ also offers `restore-if-db-not-exists: true`.
- Credentials on Cloud Run come from the metadata server.

## Spec corrections found

1. `herdr pane report-metadata` has no `--custom-status`; `--source` is required.
2. Litestream GCS URLs use `gs://`; v0.5 uses a single `replica:` key.
3. `herdr machine` does not exist in 0.8.2; use `herdr --remote`.
4. The MCP tool's `CLAUDE_CODE_SESSION_ID` goes stale after `/clear` or an
   in-session `/resume`.
5. Installing a Go plugin with `herdr plugin install` runs `[[build]]` on the
   client, and herdr does not install toolchains. "No runtime deps on client
   machines" needs either a committed binary or a `[[build]]` step that
   downloads a release binary.
6. Cloud Run always-on costs about $45–50 a month.

## herdr 0.9.3 update (2026-09-30)

`tower` runs herdr 0.9.3 (protocol 22, endpoint generation 1). `bluebox` still ran
0.8.2 when checked. sessionhub targets 0.9.3 on both machines
(`min_herdr_version = "0.9.3"`). Checked against tag `v0.9.3` (commit
`7b116c0`) and `herdr api schema --json` from `tower`:

- Unchanged: `plugins.mdx`, the manifest schema, the hookable event list
  (`src/api/schema/events.rs` has no diff; still no `server.ready`),
  `AgentSessionInfo`, `PaneReportMetadataParams`, `AgentStartParams`,
  `PluginActionInvokeParams` (actions still take no arguments), and
  `PaneSplitParams`.
- `PaneInfo` gains `restore_error`.
- `pane.report_agent_session` gains optional `resume_argv`
  (`[-- RESUME_ARG...]` on the CLI): "Command that resumes this session after a
  Herdr restart". sessionhub does not need it; herdr's Claude integration already
  restores Claude sessions.
- Claude integration is now version 10. Same single `SessionStart` hook, now
  with matcher `^(startup|resume|clear|compact|fork)$`, and it now exits for
  any hook event other than `SessionStart`.
- New: saved SSH machines (`connecting-machines.mdx`). `herdr machine add
  <SSH_TARGET> [--label L] [--remote-session NAME]` saves a profile, and the
  herdr UI then shows the agents of every connected machine in one list.
  `herdr --machine <label-or-id> <command>` routes `pane`, `agent`, `tab`,
  `workspace`, `api snapshot`, and plugin commands to that machine over SSH:
  "The CLI uses the saved profile's SSH target and session directly; it does
  not need an open TUI." IDs are per server, and `--current` cannot refer to
  the caller's local pane.
- `pane split`: "An omitted target splits the calling pane when
  `HERDR_PANE_ID` is available, otherwise the focused pane."
- New panes strip "known Claude Code, Codex, and OMP session markers" from the
  inherited environment.
- The pane graphics socket methods were removed (not used by sessionhub).

What this means for sessionhub: saved machines cover part of question 3 (getting
into a session on another machine from one herdr window), but not questions 1
and 2: they don't keep ended sessions, semantic progress, a phone view, or
sessions outside herdr. Remote resume in sessionhub can print a `herdr --machine`
command when the target machine is saved in the local herdr.

## herdr socket: one request per connection (2026-09-30)

herdr 0.9.3 closes the connection after answering one request; a second request on the same connection fails with a broken pipe. `internal/herdr` opens a connection per call. Observed live, see `docs/dev/evidence/task-3.md`.

## Deploying to tower (2026-09-30, Task 10)

Observed during the first deploy. Evidence: `docs/dev/evidence/task-10.md`.

- Non-interactive ssh on `tower` has no `~/.local/bin` in `PATH`, where
  `herdr`, `claude`, and `cloudflared` live. `make deploy` prefixes `PATH` for
  `sessionhub install-plugin`, and `sessionhub join` on `tower` must run from a login shell
  (`ssh tower 'exec $SHELL -lc "..."'`), or it skips the plugin and MCP
  installers.
- herdr 0.9.3 fires `pane.closed` for `herdr pane close`, but not for the
  panes of a workspace closed with `herdr workspace close`. The session still
  ends, through the hooks' `SessionEnd`, and the next heartbeat ends any
  session whose pane is missing.
- `tower.example.com` and `bluebox.example.com` resolve to Cloudflare, which carries SSH only
  through `cloudflared access ssh` with a browser Access login. So the
  printed `ssh -t <ssh_host> ...` resume commands need an `~/.ssh/config`
  `ProxyCommand` and a current login on the machine you resume from. `tower`'s
  entry for `bluebox.example.com` also fails non-interactively, because `cloudflared`
  is not on the non-interactive `PATH`.
- A new zsh pane on `tower` can stop at a `compinit` "insecure directories"
  prompt, which swallows the first character of a command sent to it. `herdr
  agent start` then times out; retry once the shell is at its prompt.

## Claude Code mods: what the inbox pane ran into (2.1.292, 2026-10-07)

- `claude plugin validate` follows `$` only into functions of the same file.
  A hooks module that passes `$` to a function it imports fails validation
  ("$ is followed only into a function declared in this same file"). So
  `register.js` holds every `$` call, and `hooks/pane.jsx` is pure: parsing,
  cleaning, and the tree, with callbacks for the buttons.
- A plugin's own `$.ui.close` does not run its own `ui.close` hook (a call
  dispatches to the hooks beneath the caller). The mod stops the pane's
  timer itself before it closes the pane; the `ui.close` hook covers the
  person's close and an unload.
- `claude plugin validate` reports a `command.run` hook as "answers its own
  command" only when `$.command.register` names the command as a literal.
  Matchers written with an imported constant show as `id=?`.
- The test kit's `$.ui` has no `open` or `close`. A test raises an outside
  close through an inline plugin (`test(name, { plugins: [...] }, body)`)
  whose own hook calls `$.ui.close`.
