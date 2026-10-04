# Testdata

Payloads captured from real herdr and Claude Code runs. Do not hand-write
payloads here. Each file below says how it was captured.

Capture date: 2026-09-30. herdr 0.9.3 (protocol 22) on `tower`. Claude Code on
`bluebox`: the hook process reported `CLAUDE_CODE_EXECPATH` ending in
`versions/2.1.284`; a later `claude --version` printed 2.1.285.

## Scrubbing

- The real home directory became `/home/user`. Working directories of the user's own
  panes became `/home/user/project` (and `/home/user/project/infra`).
- Workspace labels became `workspace-a`, `workspace-b`, `workspace-c`.
  Terminal titles became `claude`, `user@host:~/project`, or `session title`.
  The `label` of one pane became `agent-label`.
- Two Claude session UUIDs that belong to the user's own panes in
  `snapshot.json` and `socket/session-snapshot.ndjson` were replaced with
  `11111111-2222-4333-8444-555555555555` and
  `66666666-7777-4888-8999-aaaaaaaaaaaa`. Nothing else was changed, so the
  shape is real.
- The Claude scratch project directory became `/tmp/sessionhub-hook-probe`.
- Pane, workspace, and terminal IDs (`w9:p1`, `term_...`) are unmodified.
- Values of variables named `*TOKEN*` or `*KEY*` were filtered out at capture.

## herdr events (`herdr/events/`, `herdr/env/`)

Captured with a throwaway plugin `sessionhub-probe` (linked with `herdr plugin link`,
unlinked afterwards). Each command wrote `env | grep ^HERDR_` to `env/*.env`
and `$HERDR_PLUGIN_EVENT_JSON` (or `$HERDR_PLUGIN_CONTEXT_JSON` for the action)
to `events/*.json`. The event payload shape is
`{"event":"<snake_case>","data":{"type":"<snake_case>", ...}}`. The `.env` file
also carries `HERDR_PLUGIN_CONTEXT_JSON`, which has `workspace_label`,
`focused_pane_*`, and `correlation_id`.

| File | Trigger |
| --- | --- |
| `pane.created-root` | `herdr workspace create --cwd /tmp/sessionhub-probe-work` (root pane `w9:p1`) |
| `pane.created-split` | `herdr pane split w9:p1 --direction right` (`w9:p2`) |
| `pane.closed-split` | `herdr pane close w9:p2` |
| `pane.agent_detected` | `herdr agent start probe --kind claude --pane w9:p1` (second attempt) |
| `pane.agent_status_changed-blocked` | Claude stopped at the folder-trust prompt, status `blocked` |
| `pane.closed-agent-pane` | `herdr pane close w9:p1` (also closed the workspace) |
| `action-dump` | `herdr plugin action invoke dump --plugin sessionhub-probe`. The context describes the user's focused pane, scrubbed. There is no `HERDR_PLUGIN_EVENT` in this env. |

`pane.agent_detected` and `pane.agent_status_changed` carry `agent` and (for
status) `agent_status`, but no `agent_session`. `pane.closed` carries only
`pane_id` and `workspace_id`.

## herdr CLI and snapshot

- `herdr/pane-get.json`: `herdr pane get w9:p1` while Claude sat at the trust
  prompt (`agent_status: blocked`).
- `herdr/snapshot.json`: `herdr api snapshot` in the same state. It shows
  the probe pane (`launch_pending: true`, `name: "probe"` in `agents`), and
  real `agent_session` objects
  (`{"agent":"claude","kind":"id","source":"herdr:claude","value":"<uuid>"}`)
  on the user's other panes. One pane has an `agent_session` but no `agent`
  field (`w8:p1`, a restored pane with agent status `unknown`).

## herdr socket (`herdr/socket/*.ndjson`)

Line 1 is the request, line 2 is the response, sent with python3 over
`~/.config/herdr/herdr.sock`. Against the probe pane `w9:p1`, or the missing
pane `w99:p99`.

| File | Request |
| --- | --- |
| `session-snapshot` | `session.snapshot` |
| `pane-get-existing` | `pane.get` |
| `pane-get-missing` | `pane.get` (error `pane_not_found`) |
| `pane-report-metadata-set` | `pane.report_metadata`, `source:"sessionhub"`, `tokens:{"hub_summary":"probe summary"}` |
| `pane-get-with-metadata` | `pane.get` after the report (shows `tokens`) |
| `pane-report-metadata-clear` | `pane.report_metadata` with `hub_summary: null` |
| `pane-focus-missing` | `pane.focus` (error `pane_not_found`) |
| `pane-split` | `pane.split` in a scratch workspace on `tower` (created and closed by the Task 3 fix round; not scrubbed, IDs are the scratch workspace's own). The response is `result.pane` |

Captured 2026-09-30 on `bluebox` (herdr 0.9.3, Claude Code 2.1.285) in a scratch
workspace, over the live socket with python3. Pane IDs `wS:p1` and `wS:p2`
are the scratch workspace's own. Scrubbing: the project directory became
`/home/user/project`, the scratch Claude's session UUID became
`11111111-2222-4333-8444-555555555555`, and the Remote Control session ID in
the printed URL became `session_01AbCdEfGhIjKlMnOpQrStUv`.

| File | Request |
| --- | --- |
| `agent-prompt-ok` | `agent.prompt` `{"target":"wS:p1","text":"/remote-control"}` on an idle Claude. Params are `target` and `text`. The result is `agent_prompted` with an `agent` object |
| `agent-prompt-blocked` | `agent.prompt` on `wS:p2`, a Claude blocked on an `AskUserQuestion` prompt (error `agent_blocked`; herdr sent no input) |
| `pane-read-before-remote-control` | `pane.read` `{"pane_id":"wS:p1","source":"recent_unwrapped","lines":120}` before the prompt. The wire value is `recent_unwrapped` (underscore); the CLI flag is `recent-unwrapped`. The text is `result.read.text` |
| `pane-read-after-remote-control` | The same request after `/remote-control` ran: the text holds `/remote-control is active · Continue here, on your phone, or at https://claude.ai/code/session_...` |
| `pane-read-remote-control-dialog` | `pane.read` on scratch pane `wV:p1` after a second `/remote-control`: the dialog with `Disconnect this session`, `Show QR code`, `Continue`, and the URL (scrubbed to the same fake ID) |
| `pane-read-visible-prompt` | `pane.read` `{"pane_id":"wW:p1","source":"visible"}` (no `lines`) on a fresh Claude at its prompt. The shell prompt line is scrubbed (`user@host`, `[main]`) |
| `pane-read-visible-dialog` | The same request on `wW:p1` with the dialog open: the earlier "/remote-control is active" line, then the dialog, with `Enter to select · Esc to continue` as the last line. The agent status during the dialog was `idle` |
| `pane-send-keys-esc` | `pane.send_keys` `{"pane_id":"wV:p1","keys":["esc"]}` on that dialog. The result is `{"type":"ok"}`. The dialog closed and the prompt came back |
| `workspace-create` | `workspace.create` `{"cwd":"/tmp/sessionhub-rc-fixture","label":"sessionhub: fixture capture","focus":false}` on `bluebox` (herdr 0.9.3); the scratch workspace was closed right after. The result has `workspace`, `tab`, and `root_pane` |

The `pane-read` fixtures share one request, so load one per `herdrtest` server.

## Claude Code hooks (`claude/hooks/`)

A scratch project with `.claude/settings.local.json` hooks ran
`claude -p --model haiku "Reply with the single word ok"` on `bluebox`. `.json` is
the hook stdin. `.env` is `env | grep -E '^(CLAUDE|HERDR)'`. The run was a
child of another Claude session (`CLAUDE_CODE_CHILD_SESSION=1`,
`CLAUDE_CODE_ENTRYPOINT=sdk-cli`), and `bluebox` was not inside herdr, so there are
no `HERDR_*` variables in these env files.

Files: `SessionStart`, `UserPromptSubmit`, `Stop`, `SessionEnd`.
`SessionEnd` reported `reason: "other"`.

## Gaps

- **`Notification` did not fire** in print mode. There is no captured
  `claude/hooks/Notification.*`. `claude/hooks/Notification.constructed.json`
  is **constructed from docs, not captured**: the common fields are copied
  from the captured `Stop.json`, and `message`, `title`, and
  `notification_type` follow docs/dev/NOTES.md. Those three values are made up.
- **`[[startup]]` did not run on link.** Nothing was written when the probe
  plugin was linked, as documented, and herdr was not restarted. There is no
  startup payload or startup env.
- **`pane.exited` did not fire.** The probe subscribed to it, but closing the
  pane with `herdr pane close` produced only `pane.closed`. There is no
  `pane.exited` payload.
- **No `pane.agent_status_changed` other than `blocked`**, and no event
  from a Claude that got past the trust prompt. Claude stayed at the trust
  prompt, so its `SessionStart` never fired and the probe pane has no
  `agent_session` of its own. The `agent_session` shape comes from other
  panes in the same snapshot.
- **No herdr-managed `SessionStart` hook run** was captured with `HERDR_*`
  variables, because `bluebox` had no herdr.
- **`herdr machine list --json` shape is unverified.** On `bluebox` (herdr 0.9.3, 2026-09-30) it printed `[]`: no saved machines. `herdr/machine-list.json` is that real capture. `herdr/machine-list.constructed.json` is **constructed from the herdr docs, not captured**: the field names (`id`, `label`, `ssh_target`, `remote_session`, `enabled`) are a guess. `internal/resume` reads them leniently and treats anything unreadable as no saved machines. Replace the constructed file when a real profile is saved.
