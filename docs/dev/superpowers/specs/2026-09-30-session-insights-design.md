# Session insights design

Date: 2026-09-30. Status: approved in chat section by section; this document
is for review before planning.

## Goal

From the dashboard or the CLI, recognize any session, running or ended, and
know enough to follow up on it: what you last asked, where Claude left off,
what it produced, and what it cost.

## Decisions

| Topic | Decision | Why |
|---|---|---|
| Text that leaves the machine | Only what Claude writes about the session (recap, automatic title, `report_progress`), commit subjects, and the prompt text sessionhub already collects. Claude's reply text is never sent. | A reply can quote a secret Claude saw in a file or command output. |
| Existing prompt text | Kept as is: the first 200 characters of each prompt, stored as `prompt` events. | Chosen explicitly; it gives the card the last prompt for free. |
| Git activity | Up to 5 commit subjects plus counts. | Commit subjects are the best follow-up signal after the recap. |
| Where the digest is built | On each machine, from its own transcripts, pushed to the server. | Works when the machine is later offline, and reuses the queue and auth. |
| Existing sessions | Filled once by `sessionhub digest --all` on each machine. | Ended sessions get insights right away. |
| Dollar cost | The latest figure Claude Code wrote, shown with its time. | The transcript records cost only at exit or compaction; a pricing table is out of scope. |

## Facts this rests on

Measured on `bluebox` over the 70 transcripts with a user prompt from the week
before 2026-09-30 (Claude Code 2.1.285):

| Transcript entry | Sessions with it | Fields used |
|---|---|---|
| `type: assistant` with `message.usage` | 70 | `message.id`, `input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens` |
| any entry with `gitBranch` | 70 | none; the branch is already in sessionhub |
| `type: cost-state` | 69 | `totalCostUSD`, written at exit and compaction only |
| `type: ai-title` | 23 | `aiTitle` |
| `type: system, subtype: away_summary` | 14 | `content`, `timestamp` |
| `type: pr-link` | 6 | `prNumber`, `prUrl`, `prRepository`, `timestamp` |
| `type: custom-title` | 1 | `customTitle` |

- Claude Code writes each assistant reply several times with the same
  `message.id`. In one session, 885 assistant entries held 403 replies.
- Subagent transcripts live in `<project-dir>/<session-id>/subagents/*.jsonl`.
- Every recap ends with ` (disable recaps in /config)`.
- The largest transcript was 9.2 MB; the median 0.33 MB.
- The format is not documented and may change in any Claude Code release.

## What you see

Each session has one **digest**, replaced on every update. There is no
history of digests.

**Dashboard card**, top to bottom:

1. **Title.** Precedence: your own title (`set_title` or `custom-title`),
   then the herdr title, then Claude's automatic title, then the first
   prompt.
2. **Recap** with its age, for example "Recap, 2 h ago: …". Hidden when the
   session has none.
3. **Latest report** (`done`, `in flight`, `waiting on`), unchanged.
4. **Last prompt**: "You, 14 min ago: …", one line.
5. **Summary line**, each part shown only when known:
   `3 commits · 2 uncommitted · 1 unpushed · MR !391 · $18.27`. The merge
   request or PR part is a link. When no dollar figure exists, output tokens
   are shown instead, for example `371k tokens out`.
6. The **Remote Control** button, unchanged.

**Detail view** (`sessionhub show <id>`, and an expanded card on the dashboard):

- Commit subjects made during the session, newest first, up to 5, with the
  total count.
- Every merge request or PR link.
- Tokens by kind (input, output, cache read, cache write) and the dollar cost
  with "as of <time>".
- The digest's `as_of` time.

**Filter:** one text field at the top of the dashboard and `sessionhub list --grep
<text>`. Both match, case-insensitively, the title, recap, last prompt,
branch, directory, and machine. Filtering runs in the client over the list it
already has.

## Machine side

New package `internal/digest`, used by both clients.

### Reading the transcript

- **Finding it:** glob `<claude-dir>/projects/*/<session-id>.jsonl`, where
  `<claude-dir>` is `CLAUDE_CONFIG_DIR` or `~/.claude`. Exactly one match is
  required; zero or several means no transcript.
- **State file:** `~/.local/state/sessionhub/digest/<session-id>.json` holds the
  byte offset, the file's device and inode, the running totals, the set of
  counted message IDs (IDs only), and the latest extracted values.
  Subagent files have their own offsets in the same state file.
- **Incremental reads:** each run reads from the saved offset. If the file is
  smaller than the offset or its inode changed, the run discards the state
  and reads from the start. A trailing line with no newline is left for the
  next run.
- **Line limit:** lines longer than 1 MB are skipped and counted as bad.
- **Extraction**, latest entry wins unless stated:
  - `recap`: the `away_summary` content with the suffix removed, passed
    through the existing free-text cleaning, cut to 600 characters, and its
    `timestamp` as `recap_at`.
  - `ai_title`: `aiTitle`.
  - `custom_title`: `customTitle`.
  - `links`: every `pr-link`, deduplicated by URL, in order of first
    appearance.
  - `tokens`: summed over assistant entries, each `message.id` counted once,
    across the main transcript and its subagent transcripts.
  - `cost_usd` and `cost_at`: `totalCostUSD` and the entry's position time
    (the timestamp of the nearest preceding timed entry, since `cost-state`
    has none).
  - `first_at` and `as_of`: the first and last entry `timestamp` in the main
    transcript.
- **Unknown entry types** are ignored. A line that is not JSON is counted as
  bad. If more than half the lines read in one run are bad, the run sends
  nothing, keeps the old state, and logs one line.

### Git

Run in the session's directory with the existing `gitinfo` timeouts:

- `git log --since=<first_at> --until=<as_of> --format=%h%x00%s` on
  `HEAD`: the total count and the newest 5 subjects, each cleaned and cut to
  120 characters. The window closes at the last transcript entry for live and
  ended sessions alike, so later commits from other work don't count. Commits
  by other sessions in the same repository inside the window do count; the
  detail view labels them "commits on <branch> during the session".
- `git status --porcelain`: the count of lines.
- `git rev-list --count @{u}..HEAD`: the unpushed count, or `-1` when there
  is no upstream.

If the directory is missing or not a repository, or any command fails or times
out, the `git` part is omitted and the rest is still sent.

### When it runs

- **Hooks client:** the `stop` and `session-end` hooks start
  `sessionhub digest <session-id>` as a detached child, like the existing flush.
  The hook doesn't wait, so the SessionEnd budget is unaffected.
- **herdr watcher:** on each 60-second heartbeat, for each Claude session it
  reports whose transcript's size or modification time changed since its last
  run.
- **Concurrency:** a lock file per session next to the state file. A run that
  finds the lock held exits at once with success.
- Both clients are installed on both machines, so a digest may be built
  twice. The server keeps the one with the newest `as_of`.

### Delivery

The digest goes through the existing queue as a new item kind `digest`.
Coalescing keeps only the newest digest item per session. On `404` the item is
dropped, as events are today.

### Backfill

`sessionhub digest --all`:

1. Lists the sessions the server knows for this machine
   (`GET /v1/sessions`, filtered by machine name in the client).
2. Builds and sends a digest for each one that has a transcript, one at a
   time.
3. Prints one line per session: sent, no transcript, or failed with the
   reason, then a total.

It is safe to run more than once.

## Server

### Storage

Schema version 4, upgraded in place like versions 2 and 3:

- New table `session_digests`: `session_id` (primary key, foreign key),
  `as_of`, `received_at`, and `body` (the validated digest as JSON).
- New `sessions` columns `last_prompt` and `last_prompt_at`, set when a
  `prompt` event is stored. The upgrade fills them once from the newest
  existing `prompt` event of each session.
- New title source `claude`, ranked above `prompt` and below `herdr` and
  `user`. A digest's `ai_title` sets it. A digest's `custom_title` sets a
  `user` title.

### `PUT /v1/sessions/{id}/digest`

- **Auth:** machine tokens only, with the same session ownership rule as
  `POST /v1/sessions/{id}/report`.
- **Body** (`api.DigestIn`):

  ```json
  {
    "as_of": "2026-09-30T19:02:05Z",
    "first_at": "2026-09-30T07:19:35Z",
    "recap": "…", "recap_at": "…",
    "ai_title": "…", "custom_title": "…",
    "links": [{"number": "391", "url": "https://…", "repo": "wpml-org/content"}],
    "tokens": {"input": 920, "output": 371471, "cache_read": 195282944, "cache_write": 1107462},
    "cost_usd": 113.12, "cost_at": "…",
    "git": {"commit_count": 12, "commits": [{"sha": "ba935ba", "subject": "…"}],
            "uncommitted": 0, "unpushed": -1},
    "bad_lines": 0
  }
  ```

- **Validation:** `as_of` required and not more than 5 minutes in the
  future. `recap`, titles, and subjects use the existing free-text rules and
  length limits (600, 200, and 120 characters). At most 20 links, each an
  `https://` URL of at most 300 characters. At most 5 commits, `sha` matching
  `^[0-9a-f]{4,40}$`. Counts and tokens non-negative and capped at 10^12;
  `unpushed` may be `-1`. `cost_usd` between 0 and 100000. Bodies over 16 KB
  get `413`. Any failure gets `400` naming the field.
- **Stale digests:** if `as_of` is older than the stored one, the server
  answers `200` with the stored session unchanged.
- **Events:** a `digest` event is recorded only when the recap or the set of
  links changed.
- **Response:** the session, as for `report`.

### Reads

- `GET /v1/sessions` adds to each session: `recap`, `recap_at`,
  `last_prompt`, `last_prompt_at`, and `summary`
  (`commit_count`, `uncommitted`, `unpushed`, `latest_link`, `cost_usd`,
  `output_tokens`, `as_of`).
- `GET /v1/sessions/{id}` adds the full `digest`.
- `sessionhub list` gains `--grep <text>`. `sessionhub show` prints the detail view.

Remote Control fields are unchanged.

## Failure handling

| Condition | Behavior |
|---|---|
| Transcript format changes | Unknown types skipped; more than half bad lines means nothing is sent and one log line is written. |
| Transcript missing (deleted by Claude Code retention) | Nothing sent; the stored digest stays. |
| Git fails or times out | `git` omitted; the rest sent. |
| Line over 1 MB | Skipped and counted as bad. |
| Two runs at once | The second exits at once. |
| Server unreachable | The newest digest waits in the queue. |
| Older client, newer server | Works; the new fields are additions. |
| Digest older than stored | Ignored with `200`. |

## Testing

- **Reader:** table tests over fixture transcripts cut from real ones, with
  all text replaced: duplicate reply entries counted once; subagent totals
  included; recap suffix removed; latest title, recap, and cost win; unknown
  types skipped; a partial last line held for the next run; a shrunk or
  replaced file read from the start; the bad-line limit stops the send; a line
  over 1 MB skipped.
- **Git:** a temporary repository with commits inside and outside the window,
  uncommitted files, with and without an upstream, and a deleted directory.
- **Server:** the auth table extended to the new route; stale-digest
  handling; validation of each field, including a `javascript:` link, a bad
  `sha`, and an oversize body; the version 4 upgrade filling `last_prompt`
  from existing events; title ranking with `claude`; `digest` events only on
  recap or link changes.
- **Clients:** the `stop` and `session-end` hooks start the child without
  waiting; the watcher runs only for changed transcripts; queue coalescing
  keeps the newest digest; backfill skips sessions without a transcript and
  reports each result.
- **Dashboard:** the pure rendering functions under node for each card state
  (with and without recap, cost, links, and git), a check that no raw HTML
  insertion is used, and filter matching.
- **Live, end to end:** deploy; run `sessionhub digest --all` on `bluebox` and `tower`;
  check `sessionhub show` and the dashboard for real ended sessions with a recap,
  commits, a merge request link, and cost; then finish a turn in a scratch
  session and check that its card updates within a minute.

## Out of scope

- Claude's reply text.
- Live dollar cost (needs a pricing table).
- OpenTelemetry.
- Digest history.
- Changes to how prompt text is collected.
