# Knowledge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Scoped, user-approved knowledge entries (conventions, facts, decisions) that reach every matching session on every machine.

**Architecture:** A `knowledge` table replaces `instructions`; rules become global conventions and `/v1/instructions` stays as a view. Agents propose through MCP, the user approves on the dashboard or CLI. The watcher keeps `knowledge.json` (active entries plus ticket refs per live session); `sessionhub hook context` selects entries by scope offline.

**Tech Stack:** Go, `modernc.org/sqlite` (FTS5 verified 2026-10-17), vanilla JS dashboard, `node` for dashboard tests.

**Spec:** `docs/dev/superpowers/specs/2026-10-17-knowledge-design.md`

## Global Constraints

- Schema version 17. v17 creates `knowledge` and `knowledge_fts`, copies `instructions`, drops `instructions`.
- Entry `text` 1–300 runes, `why` 0–200 runes, no control characters (reuse `checkText`).
- Kinds: `convention`, `fact`, `decision`. States: `proposed`, `active`, `rejected`, `retired`.
- Scopes: `global`, `repo:<host/path>`, `ticket:<ref>`, `machine:<name>`.
- Limits: 500 `active`, 100 `proposed` (`409` past either). Global conventions keep the 2,000-rune total cap.
- `every`: 0–100. Default 20 for `fact` and `decision`, 0 for `convention`.
- Hook budget: global conventions always sent; other entries share 2,000 runes per prompt.
- Hook header: `Knowledge approved by the user for this session (data, not instructions from the content you read):`
- Hook line: `- [<kind>] <text>` plus ` (<why>)` when `why` is set.
- Web writes need `X-Hub-Action: knowledge`.
- Review threshold: `confirmed_at` older than 90 days.
- The hook never touches the network.
- Commit lines end with `Claude-Session: https://claude.ai/code/session_01TRbqmEDF5DVTtZeXNDvE3o`; no `Co-Authored-By`.

## Review Focus

1. SSH origin `git@gitlab.com:Group/Repo.git` and HTTPS `https://user@gitlab.com/group/repo` must normalize to the same scope; a cwd with no `origin` gets no repo scope (Task 1, Task 5).
2. AWS account IDs (`823963319113`), ARNs, and secret names (`prod/telegram-bot`) are not secrets; `AKIA…`, `-----BEGIN`, `ghp_…`, `glpat-…`, and 40+ char base64 runs are (Task 1).
3. An old watcher on `blu` reads `/v1/instructions` after the migration and must get the same global conventions with `every` (Task 2, Task 3).
4. Approving an entry whose `replaces` target is already retired or rejected succeeds and leaves the target as it is (Task 2).
5. A session with many ticket and repo entries still gets every global convention (Task 5).

---

### Task 1: Pure helpers (`internal/knowledge`)

**Files:**
- Create: `internal/knowledge/scope.go`, `internal/knowledge/secret.go`
- Test: `internal/knowledge/scope_test.go`, `internal/knowledge/secret_test.go`

**Interfaces:**
- Produces:
  - `NormalizeRepo(originURL string) string`: `host/path`, lowercase, no scheme, user, port, or `.git`; `""` for empty or unparsable.
  - `NormalizeRef(ref string) string`: lowercase `systemsdev-123`; MR as `group/project!34`.
  - `ParseScope(s string) (kind, value string, err error)`: accepts the four scope forms; normalizes the value; error for anything else.
  - `RefsInBranch(branch string) []string`: matches `(?i)\b[a-z][a-z0-9]+-\d+\b`, normalized, deduped.
  - `LooksSecret(text string) bool`.

- [ ] **Step 1: Write failing tests**

```go
func TestNormalizeRepo(t *testing.T) {
	for in, want := range map[string]string{
		"git@gitlab.com:Group/Repo.git":           "gitlab.com/group/repo",
		"https://user@gitlab.com/group/repo":      "gitlab.com/group/repo",
		"ssh://git@gitlab.com:2222/group/repo.git": "gitlab.com/group/repo",
		"": "",
	} {
		if got := NormalizeRepo(in); got != want {
			t.Errorf("NormalizeRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseScope(t *testing.T) // global; repo:<ssh url> → repo:gitlab.com/group/repo; ticket:SystemsDev-17908 → ticket:systemsdev-17908; machine:blu; "repo:" and "team:x" → error

func TestRefsInBranch(t *testing.T) // "abdallah/systemsdev-17908-dr-plan" → ["systemsdev-17908"]; "main" → nil

func TestLooksSecret(t *testing.T) {
	no := []string{"Account 823963319113 is enrolled", "arn:aws:iam::823963319113:role/x", "token is in prod/telegram-bot"}
	yes := []string{"AKIAABCDEFGHIJKLMNOP", "-----BEGIN OPENSSH PRIVATE KEY-----", "ghp_0123456789abcdefghijABCDEFGHIJ012345", "glpat-abcdefghij0123456789", strings.Repeat("aB3+", 11)}
	// assert LooksSecret false for each no, true for each yes
}
```

- [ ] **Step 2: Run** `go test ./internal/knowledge/` — expect FAIL (package missing).
- [ ] **Step 3: Implement** the five functions. `NormalizeRepo`: convert scp form `user@host:path` to `ssh://user@host/path`, then `url.Parse`, drop port and user.
- [ ] **Step 4: Run** `go test ./internal/knowledge/` — expect PASS.
- [ ] **Step 5: Commit** `Knowledge: scope normalization and secret check`.

### Task 2: Schema v17 and store

**Files:**
- Modify: `internal/store/store.go` (schema v17, migrate), `internal/store/instructions.go` (reimplement over `knowledge`)
- Create: `internal/store/knowledge.go`
- Modify: `internal/api/actions.go` (types)
- Test: `internal/store/knowledge_test.go`, `internal/store/instructions_test.go`, `internal/store/store_test.go`, `internal/store/permissions_test.go`

**Interfaces:**
- Consumes: Task 1 (`ParseScope`, `LooksSecret`).
- Produces in `internal/api`:
  - `Knowledge{ID int64; Kind, Scope, Text, Why, State string; Every int; Replaces int64; SourceSession, CreatedBy, DecidedBy string; CreatedAt time.Time; DecidedAt *time.Time; ConfirmedAt time.Time}` with snake_case JSON tags, `omitempty` on optional fields.
  - `KnowledgeIn{ClientID, Kind, Scope, Text, Why string; Every *int; Replaces int64; SourceSession string}`.
  - `KnowledgeDecisionIn{Decision string /* approve|reject */; Text, Scope string}`.
  - `KnowledgeAdded{Entry Knowledge; Similar []Knowledge}`.
  - `KnowledgeLocal{Version string; Entries []Knowledge; SessionRefs map[string][]string}`.
- Produces in `internal/store`:
  - `AddKnowledge(ctx, in api.KnowledgeIn, createdBy string, active bool) (api.KnowledgeAdded, error)`: idempotent on `client_id`; `ErrInvalid` on bad kind, scope, text, `every`, or `LooksSecret`; `ErrFull` past the limits.
  - `DecideKnowledge(ctx, id int64, in api.KnowledgeDecisionIn, by string) (api.Knowledge, error)`: only from `proposed`; approve retires `replaces` when it is `active`.
  - `ConfirmKnowledge(ctx, id int64) error`, `RetireKnowledge(ctx, id int64) error`.
  - `ListKnowledge(ctx, state, scope, query string) ([]api.Knowledge, error)`: `query` through `knowledge_fts MATCH`, max 200.
  - `KnowledgeLocal(ctx, machineID int64) (api.KnowledgeLocal, error)`: active entries, and refs of tasks linked (`task_sessions`) to this machine's live sessions.
  - `Instructions`, `AddInstruction`, `DeleteInstruction` keep their signatures, over active global conventions.

- [ ] **Step 1: Write failing tests** in `knowledge_test.go`:
  - `TestMigrateV16ToV17`: open, `rollbackV17`, insert two `instructions` rows (one `every=10`), set `user_version=16`, reopen; `Instructions()` returns both with text and `every` kept; `ListKnowledge("active","global","")` has kind `convention`.
  - `TestAddKnowledgeProposedAndActive`: machine add → `proposed`; `active=true` → `active`; same `client_id` twice → same ID.
  - `TestAddKnowledgeRejects`: bad kind, `scope:"team:x"`, 301-rune text, 201-rune why, `every=101`, `LooksSecret` text → `ErrInvalid`; the 101st proposal → `ErrFull`.
  - `TestDecideKnowledge`: approve with edited text and scope stores them; approve with `replaces` retires an active target and leaves a retired one retired; deciding a non-proposed entry → `ErrInvalid`.
  - `TestKnowledgeSearch`: entry "telegram bot token in prod/telegram-bot"; `ListKnowledge("active","","telegram")` finds it; after retire it does not.
  - `TestKnowledgeLocal`: two live sessions on the machine, one linked to a task with ref `systemsdev-17908`; result has that ref under its session ID and no proposed entries.
  - `TestKnowledgeSimilar`: adding near-identical text returns the existing entry in `Similar`.
- [ ] **Step 2: Update existing tests**: `store_test.go` expects `user_version` 17, a `knowledge` table, no `instructions`; add `rollbackV17` (recreate `instructions` with `every`, copy global conventions back, drop `knowledge` and `knowledge_fts`) and call it first in `rollbackV16`.
- [ ] **Step 3: Run** `go test ./internal/store/` — expect FAIL.
- [ ] **Step 4: Implement** schema v17 as in the spec's Data model, with FTS triggers on insert, update, and delete. `Similar`: top 3 active or proposed by FTS5 `bm25` over the new text's words joined with `OR`, excluding the new entry. `instructionsVersion` hashes the global conventions as before so old copies stay valid.
- [ ] **Step 5: Run** `go test ./internal/store/` — expect PASS.
- [ ] **Step 6: Commit** `Knowledge: schema v17 and store`.

### Task 3: HTTP API and alerts

**Files:**
- Create: `internal/server/knowledge.go`
- Modify: `internal/server/routes.go`, `internal/server/notify.go` (proposal alert)
- Test: `internal/server/knowledge_test.go`, `internal/server/notify_rules_test.go`

**Interfaces:**
- Consumes: Task 2 store functions and api types.
- Produces the routes in the spec's HTTP API table. Machine `POST /v1/knowledge` passes `active=false`, web passes `active=true`. `s.knowledgeProposed func(api.Knowledge)` alert hook, wired like `ruleAdded`.

- [ ] **Step 1: Write failing tests**:
  - `TestKnowledgeMachineProposes` (machine token → `201`, `state: proposed`).
  - `TestKnowledgeWebNeedsAction` (cookie without `X-Hub-Action: knowledge` → `403`; with it → `active`).
  - `TestKnowledgeDecideWebOnly` (machine token on decide → `403`).
  - `TestKnowledgeLocalForMachine` (only the caller's sessions in `session_refs`).
  - `TestInstructionsViewAfterMigration` (`GET /v1/instructions` returns global conventions with `every`, unchanged JSON shape).
  - `TestKnowledgeProposalAlert` (alert text starts `💡 Knowledge proposed` and carries kind, scope, text, and an **Open knowledge** button linking `/#knowledge`).
- [ ] **Step 2: Run** `go test ./internal/server/ -run Knowledge` — expect FAIL.
- [ ] **Step 3: Implement** handlers following `internal/server/tasks.go`; map `ErrInvalid` → `400`, `ErrFull` → `409`, `ErrNotFound` → `404`.
- [ ] **Step 4: Run** `go test ./internal/server/` — expect PASS.
- [ ] **Step 5: Commit** `Knowledge: HTTP API and proposal alert`.

### Task 4: Client, queue, and watcher copy

**Files:**
- Create: `internal/client/knowledge.go`
- Modify: `internal/client/replay.go` (`OpKnowledgePropose = "knowledge_propose"`), `internal/plugin/watcher.go:285-298`
- Test: `internal/client/knowledge_test.go`, `internal/plugin/watcher_test.go`

**Interfaces:**
- Produces:
  - `(*Client) ProposeKnowledge(ctx, api.KnowledgeIn) (api.KnowledgeAdded, error)`, `SearchKnowledge(ctx, query, scope string) ([]api.Knowledge, error)`, `ConfirmKnowledge(ctx, id int64) error`, `ListKnowledge(ctx, state, scope string) ([]api.Knowledge, error)`, `KnowledgeLocal(ctx) (api.KnowledgeLocal, error)`.
  - `NewKnowledgeID() string` with prefix `k_`.
  - `KnowledgePath(stateDir) string` = `<state>/knowledge.json`; `LoadKnowledge(path) (api.KnowledgeLocal, error)`; `RefreshKnowledge(ctx, c, path) (bool, error)`, which rewrites when `version` or `session_refs` changed.

- [ ] **Step 1: Write failing tests**: replay of a queued `knowledge_propose` posts the same `client_id`; `RefreshKnowledge` writes mode `0600` and skips an unchanged response; the watcher test's fake server sees `GET /v1/knowledge/local` after a heartbeat.
- [ ] **Step 2: Run** `go test ./internal/client/ ./internal/plugin/` — expect FAIL.
- [ ] **Step 3: Implement**; the watcher calls `RefreshKnowledge` where it calls `RefreshInstructions`, and keeps `RefreshInstructions` for one release so a downgrade still works.
- [ ] **Step 4: Run** the same command — expect PASS.
- [ ] **Step 5: Commit** `Knowledge: client, offline queue, and watcher copy`.

### Task 5: Hook selection

**Files:**
- Modify: `internal/hooks/actions.go` (`printContext`, new `selectKnowledge`)
- Test: `internal/hooks/actions_test.go`

**Interfaces:**
- Consumes: Task 1 (`NormalizeRepo`, `RefsInBranch`), Task 4 (`LoadKnowledge`, `KnowledgePath`), existing `countPrompt`/`dueRules` semantics, `client.LoadConfig().Machine`.
- Produces: `selectKnowledge(local api.KnowledgeLocal, sessionID, repo, machine string, branchRefs []string, n int) string`, the full `additionalContext` block or `""`.

- [ ] **Step 1: Write failing tests**:
  - `TestSelectKnowledgeScopes`: entries for global, `repo:gitlab.com/g/r`, `repo:gitlab.com/other`, `ticket:systemsdev-1` (in `session_refs`), `ticket:systemsdev-2` (in branch refs), `ticket:systemsdev-3`, `machine:togh`, `machine:blu`; with repo `gitlab.com/g/r` and machine `togh`, the block contains exactly the global, matching repo, both matching tickets, and `togh` entries, in the order global conventions, ticket, repo, machine, other global.
  - `TestSelectKnowledgeBudget`: 30 repo facts of 100 runes and 3 global conventions; all 3 conventions present; repo entry text totals ≤ 2,000; last line `- 10 more entries match this session; use search_knowledge.`
  - `TestSelectKnowledgeEvery`: a fact with default `every` 20 shows at `n=20`, not at `n=19`, and always at SessionStart (`n=0` means session start).
  - `TestContextRepoCached`: SessionStart in a temp git repo with an `origin` writes `<state>/session-repo/<id>`; a later prompt with `cwd` set to a non-repo dir still uses the cached repo.
  - `TestContextNoOriginNoRepoScope`: a git dir without `origin` gets no repo entries and no error.
  - Existing rules tests keep passing against `knowledge.json` (fall back to `instructions.json` when `knowledge.json` is missing).
- [ ] **Step 2: Run** `go test ./internal/hooks/` — expect FAIL.
- [ ] **Step 3: Implement**. At SessionStart, `gitinfo.Lookup` (500 ms) then cache repo and branch; on prompts, read the cache only. Line format and header from Global Constraints.
- [ ] **Step 4: Run** `go test ./internal/hooks/` — expect PASS.
- [ ] **Step 5: Commit** `Knowledge: hook selects entries by scope`.

### Task 6: MCP tools

**Files:**
- Modify: `internal/mcp/server.go` (dispatch :149-163, `toolDefs` :184, initialize instructions :125), `internal/mcp/tools.go`
- Test: `internal/mcp/knowledge_test.go`

**Interfaces:**
- Consumes: Task 4 client methods, `sendItem`, `resolveSession`.
- Produces tools `propose_knowledge`, `search_knowledge`, `confirm_knowledge` with descriptions from the spec's MCP tools section.

- [ ] **Step 1: Write failing tests**: `propose_knowledge` sets `source_session` from `resolveSession`, returns `sessionhub: knowledge proposal recorded. The user approves it on the dashboard.` and lists `Similar` as `#<id> [kind] text`; offline it queues `knowledge_propose`; a server `400` for a secret returns `the server rejected the knowledge proposal (400 …)`; `search_knowledge` prints at most 20 lines `#<id> [kind] (scope) text`.
- [ ] **Step 2: Run** `go test ./internal/mcp/` — expect FAIL.
- [ ] **Step 3: Implement**; append to the initialize instructions: `Propose knowledge with propose_knowledge when the user corrects a fact about the environment, states a convention, or a decision is made. Search knowledge before asking the user where something is.`
- [ ] **Step 4: Run** `go test ./internal/mcp/` — expect PASS.
- [ ] **Step 5: Commit** `Knowledge: MCP tools`.

### Task 7: CLI

**Files:**
- Create: `internal/cli/knowledge.go`
- Modify: `cmd/sessionhub/main.go` (register `knowledge`), `internal/cli/actions.go` (interface)
- Test: `internal/cli/knowledge_test.go`

**Interfaces:**
- Consumes: Task 4 client methods.
- Produces `ls`, `add` (creates a proposal, like any machine write), and `search`. No approve, reject, or rm: deciding is web-only so a session cannot approve its own proposal through Bash. `ls` lines: `<id>  [<kind>] (<scope>) <text>  (<state>, <created_by>)`. `sessionhub rules` keeps its output.

- [ ] **Step 1: Write failing tests**: usage on bad args; `add` without `--kind` or `--scope` → usage; `add --kind fact --scope global "x"` → `proposed knowledge <id>; approve it on the dashboard`; `approve 7` → usage; `ls --state proposed` passes the state; `search "telegram"` prints matches.
- [ ] **Step 2: Run** `go test ./internal/cli/` — expect FAIL.
- [ ] **Step 3: Implement** with the same `env`/fake pattern as `rules`.
- [ ] **Step 4: Run** `go test ./internal/cli/` — expect PASS.
- [ ] **Step 5: Commit** `Knowledge: CLI`.

### Task 8: Dashboard Knowledge tab

**Files:**
- Modify: `internal/server/dashboard/index.html` (Rules tab :323, :377, `rulesView` :1062, `renderRules` :2825)
- Test: `internal/server/dashboard_test.go`

**Interfaces:**
- Consumes: Task 3 routes.
- Produces JS: `knowledgeView(list, now)` → `{proposed, review, active /* grouped by scope */}`; `decideKnowledgeRequest(id, decision, text, scope)`; `addKnowledgeRequest(kind, scope, text, why, every)`; `retireKnowledgeRequest(id)`; `confirmKnowledgeRequest(id)`. `hashTab("#knowledge")` → `"knowledge"`; `#rules` also maps to `"knowledge"`.

- [ ] **Step 1: Write failing test** in `TestDashboardActionStates`: `knowledgeView` splits proposed, review (confirmed 91 days before `now`), and active; groups active by scope with `global` first; request builders produce the exact URL, method, `X-Hub-Action: knowledge`, and JSON body.
- [ ] **Step 2: Run** `go test ./internal/server/ -run Dashboard` — expect FAIL.
- [ ] **Step 3: Implement** the tab per the spec's Dashboard section; the tab label shows the proposed count as a badge, like the task review count.
- [ ] **Step 4: Run** `go test ./internal/server/ -run Dashboard` — expect PASS.
- [ ] **Step 5: Commit** `Knowledge: dashboard tab`.

### Task 9: Docs and rollout

**Files:**
- Modify: `docs/server.md` (Shared instructions :630 → Knowledge; Endpoints :145; Telegram alerts :1068; Limits :1177; Database :1212), `docs/mcp.md` (Tools :30), `docs/cli.md` (:317), `docs/plugin.md` (local copy), `docs/usage.md`, `docs/CLAUDE-snippet.md`, `docs/dev/SPEC.md:104`

- [ ] **Step 1: Update** each doc to the spec; `docs/CLAUDE-snippet.md` gains the two agent-guidance lines from Task 6.
- [ ] **Step 2: Run** `go test ./... && go vet ./...` — expect PASS.
- [ ] **Step 3: Commit** `Knowledge: docs`.
- [ ] **Step 4: Deploy on togh** per the deploy note: back up the DB, install the binary, restart the server, `sessionhub install-plugin`, `sessionhub install-mod`; check `PRAGMA user_version` is 17 and `sessionhub rules` still lists the 6 rules.
- [ ] **Step 5: Seed proposals** with `sessionhub knowledge add`: the 5 duplicated conventions and the environment facts from the 2026-10-17 analysis. Show the user the list before adding; the user approves them on the dashboard.
