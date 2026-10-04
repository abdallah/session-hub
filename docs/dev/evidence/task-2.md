# Task 2 evidence: capture real payloads

Date: 2026-09-30. herdr 0.9.3 on `tower`. Claude Code on `bluebox`.

## tower: probe plugin

```sh
ssh tower 'mkdir -p /tmp/sessionhub-probe-plugin /tmp/sessionhub-probe /tmp/sessionhub-probe-work'
scp probe.toml tower:/tmp/sessionhub-probe-plugin/herdr-plugin.toml
ssh tower '~/.local/bin/herdr plugin link /tmp/sessionhub-probe-plugin'    # plugin_linked, rc=0
ssh tower 'ls /tmp/sessionhub-probe'                                        # empty: startup did not run on link
```

The manifest declares `id = "sessionhub-probe"`, `min_herdr_version = "0.9.3"`,
`platforms = ["linux"]`, one `[[startup]]`, five `[[events]]`
(`pane.created`, `pane.closed`, `pane.exited`, `pane.agent_detected`,
`pane.agent_status_changed`), and one `[[actions]]` `dump`. Every command is
`["/bin/sh","-c","t=$(date +%s%N); e=${HERDR_PLUGIN_EVENT:-x}; env | grep ^HERDR_ > /tmp/sessionhub-probe/$t-$e.env; printf %s \"$HERDR_PLUGIN_EVENT_JSON\" > /tmp/sessionhub-probe/$t-$e.json"]`
(the action writes `$HERDR_PLUGIN_CONTEXT_JSON` to `$t-action.json`).

## tower: trigger events

```sh
herdr workspace create --cwd /tmp/sessionhub-probe-work --label probe-ws --no-focus   # w9, root pane w9:p1
herdr pane split w9:p1 --direction right --no-focus                              # w9:p2
herdr pane close w9:p2
herdr agent start probe --kind claude --pane w9:p1 --timeout 60000
```

First `agent start` timed out: the pane's shell was stopped at a zsh
`compinit` "insecure directories" prompt, so herdr typed `claude` into it. It
produced `zsh: command not found: laude` at the next prompt and nothing else.
Second attempt:

```
{"error":{"code":"agent_not_ready","message":"agent probe is blocked during startup and is not ready for prompts"},"id":"cli:agent:start"}
```

Claude was at the folder-trust prompt (`blocked`). I sent it nothing.

```sh
herdr pane get w9:p1 > /tmp/sessionhub-probe/out/pane-get.json
herdr api snapshot   > /tmp/sessionhub-probe/out/snapshot.json
herdr plugin action invoke dump --plugin sessionhub-probe
python3 /tmp/sessionhub-probe/sock.py     # socket exchanges, see testdata/README.md
herdr pane close w9:p1             # closed the workspace w9 too
```

## tower: cleanup verification

```
$ herdr plugin unlink sessionhub-probe
{"id":"cli:plugin","result":{"plugin_id":"sessionhub-probe","removed":true,"type":"plugin_unlinked"}}
$ rm -rf /tmp/sessionhub-probe /tmp/sessionhub-probe-plugin /tmp/sessionhub-probe-work
$ ls -d /tmp/sessionhub-probe*
zsh:1: no matches found: /tmp/sessionhub-probe*
$ herdr plugin list | grep -c sessionhub-probe
0
$ herdr plugin list | head -1
5 plugins installed:
$ herdr workspace list | grep -o '"workspace_id":"w[0-9]*"'
"workspace_id":"w4"
"workspace_id":"w7"
"workspace_id":"w8"
```

The five plugins are the same ones that were installed before. Workspaces
`w4`, `w7`, `w8` are the user's and were untouched.

## bluebox: Claude Code hooks

Scratch directory under the session scratchpad (not the repo) with
`.claude/settings.local.json` hooks for `SessionStart`, `UserPromptSubmit`,
`Stop`, `Notification`, `SessionEnd`. Each ran `save.sh <Event>`:

```sh
cat > "$out/$1.json"
env | grep -E '^(CLAUDE|HERDR)' | grep -viE '^[^=]*(TOKEN|KEY)[^=]*=' > "$out/$1.env"
```

```
$ claude -p --model haiku "Reply with the single word ok"
ok
```

Files produced: `SessionStart`, `UserPromptSubmit`, `Stop`, `SessionEnd`.
`Notification` did not fire. The scratch directory and hook output were
deleted after copying.

## Scrub verification

```
$ grep -rniE 'abdallah|otgs|wpml|systemsdev|terraform|tower|hub_m_|hub_r_|sk-|secret|password|lean_ctx|VPN|cloudfront|3514117|screen' testdata/
testdata/herdr/machine-list.constructed.json:5:    "ssh_target": "me@bluebox.example.com:22",
testdata/README.md:6:Capture date: 2026-09-30. herdr 0.9.3 (protocol 22) on `tower`. Claude Code on
testdata/README.md:76:| `pane-split` | `pane.split` in a scratch workspace on `tower` (created and closed by the Task 3 fix round; not scrubbed, ...
```

The grep is not empty. Every hit is intended: `tower` in `README.md` names the
capture machine, and `machine-list.constructed.json` is a hand-constructed
fixture (its name says so), not a capture. No token, secret, or home path
appears in a captured file. The `socket/*.ndjson` files do contain the word
"token" in herdr field names, which the pattern does not search for and which
is not a credential.

A scrub script (kept out of the repo) applied the replacements listed in
`testdata/README.md`. Every `socket/*.ndjson` line parses as JSON.
