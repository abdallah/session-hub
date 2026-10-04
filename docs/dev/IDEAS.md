# Ideas, not in v1

- Scheduled database backups for `tower`. `make deploy` backs up the database
  only before it replaces the binary.
- A background heartbeat for hooks-only sessions (they go `stale` while idle).
- Saving herdr `herdr machine` profiles for remote resume. Remote resume
  already prints the `--machine` form, so this only adds the profile setup.
- Other herdr agents besides Claude Code.
- Soft revoke for machines: disable a token and hide the machine without
  deleting its sessions, events, and reports.
- Retention and pagination for the session list, which grows without bound.
- One watcher per named herdr session, instead of one watcher per machine.
- Move a session to another machine or to the cloud (requested 2026-10-02,
  next after session actions). Between machines: stop or idle it, carry the
  branch and uncommitted changes (push plus a patch), copy the transcript
  into the target's `~/.claude/projects/<encoded cwd>/`, and run `claude
  --resume <id>` in a herdr pane there. Open questions: repositories at
  different paths, and whether a local session can move to Claude Code on
  the web (`claude --teleport` goes the other way).
- Retry a Remote Control result the watcher couldn't post. Today the request
  expires and the dashboard says the machine didn't respond.
- Remote Control for sessions outside herdr (hooks-only).
- Live dollar cost with a pricing table, instead of the cost Claude Code records
  in the transcript.
- Digest history, so you can see how a session's recap, links, and git state
  changed over time. Each digest replaces the previous one today.
- Prune `~/.local/state/sessionhub/digest/` for sessions whose transcript is gone. It
  holds one `.json` and one `.lock` file per session and nothing removes them.
- The inbox, in the style of tuios: one view of the sessions that need you.
  Version 1 is view and triage only, with no actions on a session from the
  inbox. You chose it as the next feature.
- Duplicate a session instead of moving it (requested 2026-10-03, after the
  move feature ships). The source keeps running; the target resumes with
  `claude --resume <id> --fork-session`, so the copy gets a new ID and the
  "never in two places" rule doesn't apply. Uses: try two approaches in
  parallel, run a long job on another machine while you keep working, or
  branch off a side question. Reuses the move bundle without the `/exit`. On
  the same machine, the copy needs its own Git worktree so the two don't edit
  one checkout.
