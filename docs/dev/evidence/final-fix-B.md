# Final-review fix wave B evidence

Date: 2026-09-30, on `bluebox` with herdr 0.9.3. `<scratch>` stands for this
run's scratch directory under `/tmp/claude-1000/…/scratchpad/hubfixb`.

## Item 1: what herdr keeps on a pane after Claude exits

The check ran in a workspace this run created and closed afterwards
(`wM`, label `sessionhub-fix-b scratch`, created with `--no-focus`). No other pane
was read or touched.

```
$ herdr workspace create --no-focus --label "sessionhub-fix-b scratch" --cwd <scratch>
… "root_pane":{"agent_status":"unknown", … "pane_id":"wM:p1", … "workspace_id":"wM"} …

$ herdr agent start hubfixb --kind claude --pane wM:p1 --timeout 60000
{"id":"cli:agent:start","result":{"agent":{"agent":"claude","agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"fa10509b-2cd1-402e-9be8-25decdd7b62d"},"agent_status":"idle", … "interactive_ready":true,"name":"hubfixb","pane_id":"wM:p1", … "terminal_title_stripped":"Claude Code","workspace_id":"wM"},"argv":["claude"],"type":"agent_started"}}

$ herdr pane get wM:p1          # while Claude runs
{"id":"cli:pane:get","result":{"pane":{"agent":"claude","agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"fa10509b-2cd1-402e-9be8-25decdd7b62d"},"agent_status":"idle", … "pane_id":"wM:p1","revision":3, … "terminal_title_stripped":"Claude Code","workspace_id":"wM"},"type":"pane_info"}}

$ herdr pane send-text wM:p1 '/exit'
$ herdr pane send-keys wM:p1 enter
$ herdr pane get wM:p1          # 5 s later
{"id":"cli:pane:get","result":{"pane":{"agent_status":"unknown", … "pane_id":"wM:p1","revision":5, … "terminal_title_stripped":"me@bluebox:<scratch>","workspace_id":"wM"},"type":"pane_info"}}

$ herdr pane get wM:p1          # 15 s later, same fields
{'pane_id': 'wM:p1', 'agent': None, 'agent_session': None, 'agent_status': 'unknown', 'terminal_title_stripped': 'me@bluebox:<scratch>'}

$ herdr workspace close wM
{"id":"cli:workspace:close","result":{"type":"ok"}}
```

Finding: after a clean `/exit`, herdr 0.9.3 removes both `agent` and
`agent_session` from the pane; neither remains. The pane shape that kept
`agent_session` without a detected agent (captured `w8:p1`: `agent_status`
`unknown`, the shell's title) comes from a restored pane, not from an exit.

The rule stays as the finding asked: a pane is a live Claude session only
when `pane.agent == "claude"` and `agent_session` has kind `id` and a value.
It skips `w8:p1`, and it skips an exited pane by either condition.

## Item 3: what a command run over ssh finds on PATH

A command that sshd runs gets a non-interactive `sh` with a minimal `PATH`.
The same conditions on `bluebox`, with `env -i` (script in this run's scratch
directory, `$HOME` shown as `~`):

```
$ sh pathcheck.sh
SHELL=/usr/bin/zsh
--- plain non-interactive sh
sessionhub: not on PATH
claude: not on PATH
--- the printed remote form: exec $SHELL -lc '...'
~/.local/bin/claude
--- the printed remote form: ~/.local/bin/sessionhub
sh: 1: ~/.local/bin/sessionhub: not found
sh: 1: ~/.local/bin/sessionhub: not found
```

A bare `claude` or `sessionhub` is not found; the login shell puts `~/.local/bin` on
`PATH` and finds `claude`. `sessionhub` is not installed on `bluebox` yet
(`~/.local/bin/sessionhub` doesn't exist, and `command -v sessionhub` prints nothing), so
the `~/.local/bin/sessionhub` line can't run here, and no `ssh` to `tower` was made:
the remote branch only prints, and running it would change `tower`'s herdr.
`TestHostileRemoteForms` and `TestHostileRemoteNoHerdrInfo` run every printed
form through three shells with a fake remote `HOME` and `SHELL`, and prove the
remote shell expands `~` to its own home and runs `$SHELL -lc` with the
intended `cd` and `claude --resume` arguments.
