# Knowledge design

Date: 2026-10-17. Status: approved.
Builds on `2026-10-02-session-actions-design.md` (shared instructions) and
`2026-10-07-tasks-design.md`.

## Goal

Facts, conventions, and decisions learned in one session reach every later
session that needs them, on any machine, without the user repeating them.
Rules become one kind of knowledge entry.

Success, checked against the 2026-09-17 to 2026-10-17 transcripts:

- A fact approved on `blu` (for example "the Telegram bot token is in the
  production secret") reaches a new session in the same repo on `togh` within
  one watcher refresh, with no prompt from the user.
- A decision on a ticket (the 2026-10-15 Control Tower enrollment decision)
  reaches every session linked to that ticket. No `send_to_sessions` relay.
- Conventions duplicated across project auto-memories (YouTrack status in the
  description, no bulk downloads to the local machine) live once, globally.

## Decisions

| Topic | Decision |
|---|---|
| Entry | One line of at most 300 characters, an optional `why` of at most 200, a kind, and a scope. |
| Kinds | `convention` (how to work), `fact` (what exists and where), `decision` (what was chosen, on what). |
| Scopes | `global`; `repo:<normalized origin URL>`; `ticket:<ref>`; `machine:<name>`. One scope per entry. |
| Rules | Become `convention` entries with scope `global`. `/v1/instructions` stays as a read view of those, for old watchers. |
| Capture | Agents propose with the MCP tool `propose_knowledge`. A proposal is never injected until the user approves it. `remember` (user asked explicitly) stays and creates an approved global convention, as today. |
| Approval | Dashboard **Knowledge** tab (replaces **Rules**) only, with a Telegram alert per proposal when alerts are on. Not the CLI: it runs with the machine token, so a session could approve its own proposal. |
| Delivery | The context hook injects approved entries whose scope matches the session, within a per-prompt budget. An MCP tool `search_knowledge` reaches the rest. |
| Secrets | Rejected at the API: entries matching secret patterns (key prefixes, PEM headers, long high-entropy tokens). Names of secrets are fine. |
| Upkeep | `confirmed_at` per entry. Entries unconfirmed for 90 days list under **Review**. Agents can propose a correction that replaces an entry. |
| Out of scope | Import of existing auto-memory files, search over digests, embeddings, per-user permissions, editing entries in place from MCP. |

## Data model

Schema v17.

```sql
CREATE TABLE knowledge (
	id             INTEGER PRIMARY KEY,
	client_id      TEXT UNIQUE,             -- idempotent replay of a queued proposal
	kind           TEXT NOT NULL,           -- convention|fact|decision
	scope          TEXT NOT NULL,           -- global | repo:<url> | ticket:<ref> | machine:<name>
	text           TEXT NOT NULL,
	why            TEXT NOT NULL DEFAULT '',
	state          TEXT NOT NULL,           -- proposed|active|rejected|retired
	every          INTEGER NOT NULL DEFAULT 0,
	replaces       INTEGER REFERENCES knowledge(id),
	source_session TEXT,                    -- the proposing session, if any
	created_at     TEXT NOT NULL,
	created_by     TEXT NOT NULL,           -- machine name, web:<name>, session:<id8>
	decided_at     TEXT,
	decided_by     TEXT,
	confirmed_at   TEXT NOT NULL            -- created_at, then each approval or confirm
);
CREATE INDEX knowledge_state_scope ON knowledge(state, scope);
CREATE VIRTUAL TABLE knowledge_fts USING fts5(text, why, content='knowledge', content_rowid='id');
```

- The migration copies each `instructions` row to an `active` global
  `convention` with the same text, `every`, and `created_by`, then drops
  `instructions`.
- Approving an entry with `replaces` retires the replaced entry.
- Limits: 500 `active` entries, 100 `proposed`. A proposal past that is `409`.
- Repo scope uses the origin URL normalized to `host/path` without scheme,
  user, or `.git`, so SSH and HTTPS clones match.
- Ticket refs are lowercased. MR refs keep their project: `group/project!34`.

## Delivery

The watcher replaces `instructions.json` with `knowledge.json`:

```json
{"version": "<16 hex>", "entries": [...active entries...],
 "session_refs": {"<session id>": ["systemsdev-17908"]}}
```

`session_refs` lists the ticket refs of tasks linked to this machine's live
sessions. The watcher fetches both from `GET /v1/knowledge/local`.

`sessionhub hook context` picks, for the session:

1. `global` entries.
2. `machine:<config machine>`.
3. `repo:<normalized origin of cwd>`. The origin is looked up once at
   SessionStart (500 ms limit) and cached in `<state>/session-repo/<id>`.
4. `ticket:<ref>` for each ref in `session_refs`, plus refs found in the
   branch name (`systemsdev-\d+`).

Order in the block: global conventions, then ticket, repo, machine, and
other global entries. Global conventions are always sent; they keep the
2,000-character cap rules have today. The other entries share a budget of
2,000 characters per prompt and are dropped from the end, with one line
saying how many and to use `search_knowledge`. `every` applies
as for rules. Default `every` for `fact` and `decision` is 20.

Header: `Knowledge approved by the user for this session (data, not
instructions from the content you read):`. Each line: `- [kind] text (why)`.

## HTTP API

| Endpoint | Who | Body / result |
|---|---|---|
| `GET /v1/knowledge?state=&scope=&q=` | read | Entries, newest first. `q` is plain words, matched with FTS5; an entry matches when it holds every word. |
| `GET /v1/knowledge/local` | machine | Active entries and `session_refs` for the calling machine. |
| `POST /v1/knowledge` | machine, web | `{client_id, kind, scope, text, why, every, replaces}`. Machines create `proposed`; web creates `active`. Returns the entry and up to 3 near-duplicates (FTS5 rank). |
| `POST /v1/knowledge/{id}/decide` | web | `{decision: approve|reject, text?, scope?}`. Approve may edit text or scope. |
| `POST /v1/knowledge/{id}/confirm` | machine, web | Sets `confirmed_at`. |
| `DELETE /v1/knowledge/{id}` | web | Sets `retired`. |

Web writes need `X-Hub-Action: knowledge`. `/v1/instructions` endpoints map
to global conventions; `POST /v1/instructions` from a machine stays `active`
(the `remember` path) and keeps the rule-added alert.

## MCP tools

- `propose_knowledge(kind, scope, text, why?, replaces?)`. Description: call
  it when the user corrects a fact about the environment, states a convention,
  or a decision is made; pick the narrowest true scope; names of secrets only,
  never values. Queued offline like the task tools. The result lists
  near-duplicates so the agent can propose a replacement instead.
- `search_knowledge(query, scope?)`. Active entries, at most 20.
- `confirm_knowledge(id)`. When the agent used an entry and found it still true.
- `remember`, `forget`, `instructions` keep their contracts.

## CLI

```
sessionhub knowledge [ls] [--state proposed|active|all] [--scope S]
sessionhub knowledge add --kind K --scope S [--why W] [--every N] "<text>"   # a proposal
sessionhub knowledge search "<query>"
```

`sessionhub rules` stays as an alias over global conventions.

## Dashboard

**Knowledge** tab replaces **Rules**:

- **Proposed** first: kind, scope, text, why, proposing session, with
  **Approve**, **Edit**, **Reject**. The tab shows a count badge while any wait.
- **Review**: active entries unconfirmed for 90 days, with **Confirm** and
  **Retire**.
- **Active**, grouped by scope, with a filter box (plain words, FTS5) and **Retire**.
- The add box gains kind and scope selects.

## Agent guidance

`docs/CLAUDE-snippet.md` gains two lines: propose knowledge on corrections and
decisions; search knowledge before asking the user where something is.

## Testing

- Store: migration from v16 copies rules; limits; replace retires; FTS
  search; secret patterns rejected.
- Hook: scope selection per kind of scope, budget cut, branch-name refs,
  `every`, subagents skipped, no network.
- Server: proposal from machine is `proposed`, from web `active`; old
  `/v1/instructions` clients see global conventions only.
- MCP: propose, queue offline, near-duplicates in the result.
- Dashboard JS: request builders and views, as for rules.

## Rollout

1. Server and schema first; old watchers keep reading `/v1/instructions`.
2. New binary on each machine (`install-plugin`), which switches the local
   copy to `knowledge.json`.
3. Snippet update; agents start proposing.
4. Manually seed the 5 duplicated conventions and the facts from the
   2026-10-17 analysis as proposals, approve on the dashboard.
