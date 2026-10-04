# Move a session to another machine or to the cloud implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move a Claude Code session, with its conversation, its branch, and
its uncommitted changes, from one machine to another and resume it there in a
herdr pane. For a repository with a GitHub remote, hand the session off to a
new Claude Code cloud session instead.

**Architecture:** Each machine's herdr watcher owns an X25519 key in
`~/.config/sessionhub/move.key` and registers the public half with the server. Schema
v9 adds `moves` and `machines.move_key`. `POST /v1/sessions/{id}/move` checks
the rules and queues the move; the source machine's watcher claims it through
the existing control long poll as action `move-out`, checks the pane is idle,
pushes the branch, ends Claude with `/exit`, builds a tar bundle (transcript,
sidecar, file history, `git diff --binary HEAD`, untracked files), seals it
for the target with ECDH + HKDF + AES-256-GCM, and uploads the ciphertext. The
server stores it as a file next to the database and offers the target's
watcher a `move-in` claim. The target opens the bundle, finds a clean clone
with the same remote, fast-forwards the branch, applies the patch, writes the
transcript under its own `~/.claude/projects`, starts `claude --resume` in a
new herdr workspace while the move is still open, and reports `done`; the
server hands the session to the target machine. When the move ends, the
server offers the source one more `move-out` claim (a *finish*): on `done`
the source archives its transcript, on `failed` it restarts the session. A cloud move skips the bundle: the source
records uncommitted work on a sessionhub-owned branch through a temporary index,
runs `claude --cloud "<hand-off prompt>"`, and reports the link.

**Tech Stack:** Go 1.25 standard library only (`crypto/ecdh`, `crypto/hkdf`,
`crypto/aes`, `crypto/cipher`, `archive/tar`), `modernc.org/sqlite`,
`github.com/BurntSushi/toml`. No new dependencies. `git` and `claude` run as
child processes with timeouts. herdr 0.9.3 (socket protocol 22). Dashboard:
one embedded HTML page, pure functions tested under `node`.

**Spec:** `docs/dev/superpowers/specs/2026-10-03-move-session-design.md`

## Global Constraints

- Schema version 9, upgraded in place like versions 2 to 8. It adds:
  - `machines.move_key`: `TEXT NOT NULL DEFAULT ''`, the machine's X25519
    public key in standard base64 (44 characters), or empty.
  - `moves`: `id` (`mv_` and 22 base64url characters), `session_id` (foreign
    key, `ON DELETE CASCADE`), `source_machine_id` (foreign key, `ON DELETE
    CASCADE`), `target` (a machine name or `cloud`), `target_machine_id`
    (foreign key, `ON DELETE CASCADE`, `NULL` for `cloud`), `state`, `detail`,
    `created_at`, `updated_at`, `bundle_size`, `cloud_url`, `requested_by`,
    `source_closed_at` (when the source finished its part; `NULL` while the
    source still owes a finish).
- Move states: `requested` → `packing` → `uploaded` → `unpacking` → `done`,
  or `failed` with a detail that starts with the failed step. A cloud move goes
  `requested` → `packing` → `done` or `failed`. `cancelled` is a defined state
  that no route sets in this version; every reader treats it like `failed`.
- Timeouts, enforced by `store.ExpireMoves` (each move route runs it first, and
  `sessionhub server` runs it once a minute): `requested` for 2 minutes
  (`MoveRequestTTL`, the source never claimed it), or `packing`, `uploaded`, or
  `unpacking` for 10 minutes since `updated_at` (`MoveStepTTL`), fail with
  detail `<state>: timed out`. The server deletes a bundle file when its move ends, and
  any bundle file older than one hour (`moveBundleTTL`).
- Bundle files: `<dir of the database>/moves/<id>.sealed`, mode `0600`, in a
  `0700` directory. The upload streams to a temp file in that directory and
  renames it. The sealed
  bundle is at most `api.MaxSealedBundle` (64 MiB); a bigger upload is `413`
  and leaves no file.
- Routes and access (the auth matrix walks all six):

  | Route | Access |
  |---|---|
  | `POST /v1/sessions/{id}/move` | machine token, or cookie + `X-Hub-Action: move` |
  | `GET /v1/moves/{id}` | read |
  | `PUT /v1/moves/{id}/bundle` | machine token; the move's source machine, state `packing` |
  | `GET /v1/moves/{id}/bundle` | machine token; the move's target machine, state `unpacking` |
  | `POST /v1/moves/{id}/result` | machine token; the source while `packing`, the target while `unpacking` |
  | `PUT /v1/machines/self/move-key` | machine token |

  A machine token of the wrong machine, or the right machine in the wrong
  state, is `409`, like the other owner checks.
- When allowed (`store.CreateMove`, every failure `409` through
  `ErrNotControllable` with the reason): the session exists (`404`
  otherwise), has not ended, has a herdr pane, its agent is `idle` or `done`,
  its machine polled in the last 2 minutes and has a move key, and it has no
  open move (`requested`, `packing`, `uploaded`, or `unpacking`). For a machine
  target: the machine exists, is not the source, polled in the last 2 minutes,
  and has a move key. For `cloud`: the session's `git_repo` is a `github.com`
  remote. A target that is not `cloud` and not a valid machine name is `400`.
- Claims through `GET /v1/machines/self/control`, after control requests and
  messages, oldest first, in this order: a `requested` move of this source
  becomes `packing` and is handed out as `move-out` with the target's public
  key; an `uploaded` move of this target becomes `unpacking` and is handed out
  as `move-in` with the source's public key; an ended machine move of this
  source with `source_closed_at` `NULL` gets `source_closed_at` and is handed
  out as `move-out` with its final state (the finish). `api.ControlClaim.Move`
  carries `api.MoveClaim`. A claim is never offered twice: a watcher that dies
  after a claim lets the move time out.
- `source_closed_at` is set when the source posts its own result (a `failed`
  while `packing`, or a cloud `done`), when a `requested` move times out (the
  source never acted), and when the finish is claimed. Every other ending
  (the target's `done` or `failed`, a timeout in `packing`, `uploaded`, or
  `unpacking`) leaves it `NULL`, so the source gets a finish.
- Source notes: once a move ended and `source_closed_at` is set, its source
  may post a result with the move's final state and a `detail`. The server
  appends that detail to the move's (`; ` between, cut to 300 characters)
  and changes nothing else. Any other result on an ended move is `409`.
- On a machine move's `done`, in one transaction: `sessions.machine_id`
  becomes the target, `herdr_session`, `herdr_workspace`, `herdr_pane`, and
  `rc_url` clear, `rc_at` becomes `NULL`, and a `moved` event (source
  `server`) records `move_id`, `from`, and `to`. On a cloud `done`: the
  session ends (`ended_at` set if empty, `rc_url` cleared), a `moved` event
  records `move_id`, `from`, `to: "cloud"`, and `cloud_url`, an `ended` event
  records `{"reason":"moved_to_cloud"}`, and open permission requests close.
- Keys: `move.KeyPath()` is `move.key` next to `paths.ClientConfig()`, so
  `SESSIONHUB_CONFIG` moves it in tests. The file holds the 32-byte X25519 private key
  in standard base64 and a newline, mode `0600`, created on first use with a
  temp file and `link` (a lost race rereads the winner's file). A file that
  group or others can read is refused, never replaced. The fingerprint is the
  first 16 hex characters of the SHA-256 of the 32 raw public key bytes
  (`api.MoveKeyFingerprint`).
- Sealed form: version byte `1`, the 32-byte ephemeral public key, the 12-byte
  nonce, then AES-256-GCM ciphertext and tag. Key: `hkdf.Key(sha256.New,
  ecdh(ephemeral, target) || ecdh(source static, target), salt = move ID,
  info = "sessionhub move v1", 32)`. Additional data: the move ID. `move.Open` takes
  the target's private key and the source's registered public key; any
  mismatch is `move.ErrOpen`. Seal and Open work on the whole bundle in memory
  (at most 64 MiB of plaintext plus the same again of ciphertext).
- Bundle: a tar archive with `manifest.json`, `transcript/<id>.jsonl`,
  `transcript/<id>/...`, `file-history/<id>/...`, `git/changes.patch`, and
  `git/untracked.tar`, regular files and directories only. `move.Extract`
  refuses any other name, `..`, an absolute path, a link, a duplicate, and a
  file the manifest does not list. Untracked files are `git ls-files --others
  --exclude-standard`; symbolic links among them are skipped and listed.
  Secret-looking names (base name `.env*`, `*.pem`, `*.key`, `*.tfvars`) are
  never carried, untracked or tracked: `git/changes.patch` and the cloud
  commit leave them out with `:(exclude,icase,glob)` pathspecs, and they are
  listed in `manifest.skipped`. A single untracked file
  over 10 MiB fails the move naming it; a bundle whose sealed size would pass
  64 MiB fails naming the five largest files.
- Git: every call is `git -C <dir> ...` with a timeout (30 seconds; 2 minutes
  for `push` and `fetch`), `GIT_TERMINAL_PROMPT=0`, and SSH in `BatchMode`
  (`GIT_SSH_COMMAND`, appended to when the user set one). sessionhub never runs
  `git stash`, `git reset`, or `git clean`, never moves a branch except by
  fast-forward (and back, with a compare-and-swap `update-ref`, when it
  undoes its own fast-forward), never force-pushes, and never creates a
  commit on the user's branch. It replaces the sessionhub-owned `sessionhub/cloud-<id8>`
  only with `--force-with-lease` against the commit sessionhub pushed there. The
  cloud branch is built with a temporary `GIT_INDEX_FILE`, `write-tree`, and
  `commit-tree`; the working tree, the real index, and `HEAD` do not change.
  Every `git diff` runs with `--no-color --no-ext-diff --no-textconv
  --no-renames --full-index --src-prefix=a/ --dst-prefix=b/`, so the user's
  diff config never changes a patch.
- The watcher posts every move result through `POST /v1/moves/{id}/result`
  (not the control result route), retrying a network error or `5xx` up to
  5 times over about 20 seconds. Every detail is cleaned with
  `termtext.Clean` and cut to `api.MaxMoveDetailRunes` (300).
- Detail format: `<step>: <reason>`. Source steps: `check pane`, `read repo`,
  `cloud`, `push`, `end session`, `build bundle`, `seal`, `upload`,
  `cloud branch`, `start cloud`. Target steps: `download`, `open`,
  `claude version`, `transcript`, `find clone`, `check clone`, `checkout`,
  `apply`, `write transcript`, `start`. A source failure after `/exit` adds
  `; the session resumes here`, and the source restarts the session only
  once the sessionhub has recorded that failure. A restart or an archive that fails
  after the source's part closed is appended to the ended move's detail as a
  note (`restart failed: <why>`, `archive on <machine> failed: <why>; ...`).
- Tests use temp dirs and fakes only: `herdrtest` for herdr, temp Git
  repositories with a temp bare `origin`, `t.Setenv("HOME", ...)`,
  `CLAUDE_CONFIG_DIR`, `SESSIONHUB_CONFIG`, and `SESSIONHUB_STATE_DIR` for every path, and
  a fake `claude` shell script first on `PATH`. Git tests set
  `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`, and an identity
  through `GIT_AUTHOR_*` and `GIT_COMMITTER_*`, so a developer's
  `commit.gpgsign` or hooks never apply. No test touches the real `~/.claude`,
  `~/.config/sessionhub`, the live server, or `tower`.
- No new dependencies. No secrets in logs, test output, or docs examples. The
  dashboard inserts every server string with `textContent`.
- Follow `docs/dev/DEFINITION-OF-DONE.md`: `make test` and `make lint` green after
  every commit, and component docs updated in the commit of the task that owns
  the change. Run `gofmt -w` on every Go file you edit before `make lint`; the
  plan's code blocks are not guaranteed to be aligned the way `gofmt` wants.
- On this machine, tests can fail with `bind: address already in use`. If a
  run fails that way, wait 60 seconds and run the failing package alone before
  you debug. Run `go test ./internal/server/` on its own, never in the same
  command as the other packages.
- Rollback is restoring the database backup only. A v8 binary refuses a v9
  database (`v > schemaVersion`).
- Anchor every edit on the quoted text, not on line numbers, and `git add`
  only the files your task lists. Never `git add -a`, never `git stash`.
- Commit trailer on every commit:
  `Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h`.

### Interface ledger

Every task uses these names and signatures exactly.

- `internal/api/move.go`: constants `HeaderActionMove = "move"`,
  `ActionMoveOut = "move-out"`, `ActionMoveIn = "move-in"`,
  `KindMoved = "moved"`, `MoveCloud = "cloud"`, `MoveRequested`,
  `MovePacking`, `MoveUploaded`, `MoveUnpacking`, `MoveDone`, `MoveFailed`,
  `MoveCancelled`, `MaxMoveDetailRunes = 300`, `MaxSealedBundle = 64 << 20`,
  `EndedMovedToCloud = "moved_to_cloud"`; types `MoveIn{Target}`,
  `MoveKeyIn{PublicKey}`, `MoveResultIn{State, Detail, CloudURL}`, `Move`,
  `MoveClaim`; `func MoveOpen(state string) bool`,
  `func ParseMoveKey(s string) ([]byte, error)`,
  `func MoveKeyFingerprint(raw []byte) string`. `ControlClaim.Move *MoveClaim`,
  `Session.Move *Move`, `Machine.MoveKey string` (fingerprint),
  `Machine.MoveReady bool`.
- `internal/move` (new package):
  `func KeyPath() string`, `func LoadOrCreateKey(path string) (*ecdh.PrivateKey, error)`,
  `func PublicKeyString(k *ecdh.PrivateKey) string`,
  `func ParsePublicKey(s string) (*ecdh.PublicKey, error)`,
  `func Fingerprint(pub *ecdh.PublicKey) string`,
  `func Seal(sender *ecdh.PrivateKey, recipient *ecdh.PublicKey, moveID string, plaintext []byte) ([]byte, error)`,
  `func Open(recipient *ecdh.PrivateKey, sender *ecdh.PublicKey, moveID string, sealed []byte) ([]byte, error)`,
  `var ErrOpen`, `const SealOverhead`;
  `type Repo`, `func ReadRepo(ctx, cwd string, roots []string) (Repo, error)`,
  `func PushBranch(ctx, r Repo) error`, `func NormalizeRemote(u string) string`,
  `func IsGitHub(u string) bool`, `func DefaultRoots(extra []string) []string`;
  `type Manifest`, `type BuildInput`, `type Bundle`,
  `func Build(ctx, in BuildInput) ([]byte, Manifest, error)`,
  `func Extract(data []byte) (*Bundle, error)`, `func IsSecretName(name string) bool`;
  `func ProjectDir(claudeDir, cwd string) (string, error)`,
  `func FindTranscripts(claudeDir, id string) ([]string, error)`,
  `func WriteSession(claudeDir, cwd string, b *Bundle) ([]string, error)`,
  `func Archive(claudeDir, stateDir, sessionID, moveID string, now time.Time) error`,
  `func ClaudeVersion(ctx, bin string) (string, error)`;
  `var ErrNoClone`, `func FindClone(ctx, roots []string, remote, rootRel string) (string, error)`,
  `func CheckClone(ctx, root string, carried []string) error`,
  `func PatchNewFiles(patch []byte) []string`,
  `type Checkout`, `func PrepareClone(ctx, root, branch, head string) (*Checkout, error)`,
  `func (c *Checkout) Apply(ctx, b *Bundle) error`, `func (c *Checkout) Undo(ctx) error`,
  `func (c *Checkout) Prev() string`,
  `func RemoveFiles(paths []string)`;
  `func CloudBranch(sessionID string) string`,
  `func PushCloudBranch(ctx, r Repo, sessionID string) (string, []string, error)`,
  `func HandOffPrompt(s api.Session, machine, branch string) string`,
  `func StartCloud(ctx, bin, dir, prompt string, wait time.Duration) (string, error)`.
- `internal/store/moves.go`: `MoveRequestTTL`, `MoveStepTTL`,
  `func ValidMoveID(id string) bool`,
  `func (s *Store) SetMoveKey(ctx, machineID int64, key string) error`,
  `func (s *Store) CreateMove(ctx, sessionID, target, by string) (api.Move, error)`,
  `func (s *Store) ClaimMove(ctx, machineID int64) (api.ControlClaim, bool, error)`,
  `func (s *Store) MoveForUpload(ctx, machineID int64, id string) (api.Move, error)`,
  `func (s *Store) MoveUploaded(ctx, machineID int64, id string, size int64) (api.Move, error)`,
  `func (s *Store) MoveForDownload(ctx, machineID int64, id string) (api.Move, error)`,
  `func (s *Store) FinishMove(ctx, machineID int64, id string, in api.MoveResultIn) (api.Move, error)`,
  `func (s *Store) GetMove(ctx, id string) (api.Move, error)`,
  `func (s *Store) ExpireMoves(ctx) ([]api.Move, error)`.
- `internal/server`: `func (s *Server) SetMoveDir(dir string)`,
  `func (s *Server) sweepMoves(ctx)`, `func (s *Server) runMoveSweeper(ctx)`,
  access level `accessMove = "move"`.
- `internal/client/move.go`: `BundleTimeout = 10 * time.Minute`,
  `func (c *Client) CreateMove(ctx, sessionID, target string) (api.Move, error)`,
  `func (c *Client) GetMove(ctx, id string) (api.Move, error)`,
  `func (c *Client) PutMoveKey(ctx, publicKey string) error`,
  `func (c *Client) PostMoveResult(ctx, id string, in api.MoveResultIn) error`,
  `func (c *Client) PutMoveBundle(ctx, id string, r io.Reader, size int64) error`,
  `func (c *Client) GetMoveBundle(ctx, id string, w io.Writer) (int64, error)`;
  `Config.MoveRoots []string` (`move_roots`).
- `internal/resume`: `OutcomeRunning Outcome = "running"`,
  `func StartResumed(ctx, socket string, s api.Session, o ControlOptions) ControlResult`.
- `internal/plugin/move.go`: `type mover`, `func newMover(...) *mover`,
  `func (m *mover) handle(ctx, cl *client.Client, claim api.ControlClaim)`;
  `controller.move func(ctx, *client.Client, api.ControlClaim)`;
  `watcher.moveKey string`, `watcher.moveKeyAt time.Time`, `moveKeyEvery`.
- `internal/cli/move.go`: `RunMove`, `RunMoveKey`, `moveAPI` interface,
  `env.moves`.

### Resolved spec ambiguities

Task 12 records these in `docs/dev/PLAN.md`.

1. **How the source learns the end.** The spec has the source archive on
   `done` and restart on a failure but names no channel. The control loop runs
   one claim at a time, so waiting inline would block messages and Remote
   Control, and a goroutine dies with a watcher restart. The server offers the
   source a finish through the same long poll (`move-out` with the final
   state), tracked by `moves.source_closed_at`, so a watcher restart between
   upload and `done` still archives or restores.
2. **The public key needs a column.** The spec's schema v9 lists only `moves`;
   v9 also adds `machines.move_key`.
3. **`requested_by`, not `by`.** `BY` is an SQL keyword. The column is
   `requested_by`; the JSON field stays `by`, holding `web:<name>` or
   `machine:<name>`.
4. **`target_machine_id` and `source_closed_at`** are added to `moves` for the
   claims; the spec's `target` text column stays for display and `cloud`.
5. **`cancelled`** has no route in the spec. It is defined, no route sets it,
   and every reader treats it like `failed`.
6. **`requested` timeout.** The spec times out only `packing` and
   `unpacking`. `requested` fails after 2 minutes (the control request TTL),
   and `uploaded` after 10 minutes like the other steps.
7. **When ownership moves.** The target reports `done` after herdr detects
   Claude in the new pane. Until then the target's writes for the session get
   `409` (the hooks client drops them: the first `SessionStart` upsert and any
   state change in the first seconds). The target's next heartbeat registers
   the pane, so nothing stays lost.
8. **Step 7 runs first.** The "transcript exists" check runs before the target
   touches the clone, so that failure leaves nothing to undo. It runs again
   right before writing.
9. **What "dirty" means on the target.** Any tracked change (`git status
   --porcelain --untracked-files=no` not empty) fails the move; an untracked
   file fails only when the bundle carries the same path or the patch creates
   it (`PatchNewFiles`).
10. **The undo takes back only what the move did.** On a target failure after
    checkout, sessionhub removes the untracked files it wrote (one changed since
    stays, and the detail says so) and the folders it made, reverses the
    patch with `git apply -R`, checks out the previous branch, and puts the
    moved branch back: a branch it fast-forwarded returns to its old commit,
    and a tracking branch it created is deleted with the tracking config it
    added. Both ref updates are compare-and-swap `update-ref` calls on the
    commit sessionhub left, so a change made since is never overwritten. sessionhub never
    runs `git reset` or `git clean` on the clone, so unrelated work
    survives.
11. **Same remote** means the `origin` remote. Normalizing drops the scheme,
    the user, a trailing `/` and `.git`, and the host's case, and turns
    `git@host:path` into `host/path`.
12. **Several clones.** "Ends with the same relative path" compares the
    repository root's path relative to the search root that holds it (for
    example `OTGS/app` under `~/Code` on `bluebox` and `otgs/app` on `tower`),
    case-insensitively. A root outside every search root compares by base
    name.
13. **Detached HEAD** on the source fails `read repo`: there is no branch to
    move.
14. **Push.** The source pushes when the branch has no upstream (`git push -u
    origin <branch>`) or is ahead of it (`git push <remote> HEAD:<merge ref>`
    from `branch.<b>.remote` and `branch.<b>.merge`).
15. **Long project folder names.** The spec says names over 200 characters
    get a hash suffix, which is Claude Code's internal scheme. sessionhub uses the
    exact encoding up to 200 characters; above that, the one existing folder
    whose name starts with the first 200 characters, else it fails
    `write transcript` naming the path.
16. **`claude --cloud` output** is not documented. sessionhub reads its output line
    by line, takes the first `https://claude.ai/code/session_...` link, stops
    the process once it has it, and gives up after 2 minutes. The link must
    pass `store.ValidRemoteControlURL`. This is unverified until the live
    check.
17. **The cloud branch.** `sessionhub/cloud-<id8>` is sessionhub-owned. A retry replaces
    it with `--force-with-lease` against the commit sessionhub pushed there before
    (read with `git ls-remote` from the push URL and recognized by its
    message); when anything else is on the branch, such as a cloud session's
    commits, the move fails instead. `claude --cloud` starts from the
    checked-out branch; the hand-off prompt's `Branch:` line tells the cloud
    session where the work is. Secret-looking files, tracked or untracked,
    and files over 10 MiB stay out of the cloud commit too, and the `done`
    detail lists the secret-looking ones.
18. **Restart after a failure.** The source restarts the session with
    `claude --resume <id>` in its old pane when herdr reports that pane at a
    shell, else in a new workspace, and does nothing when a pane already runs
    the session. It restarts only after the sessionhub recorded the failure: its own
    `failed`, or the finish.
19. **Archive.** `~/.local/state/sessionhub/moved/<move id>/` holds `transcript/`
    and `file-history/` as in the bundle. Each finish removes archive folders
    older than 30 days.
20. **`sessionhub move-key`** creates the key when it is missing, like the watcher.
21. **Claude Code version** is the first field of `claude --version`. Both
    machines must report the same string.
22. **`GET /v1/machines`** gains `move_key` (the fingerprint) and
    `move_ready` (a key and a poll in the last 2 minutes), so the dashboard
    can list targets without a new route.
23. **An upload with an unknown outcome.** When the bundle upload fails, the
    source reads the move back. `packing` means the bundle did not land: the
    source posts `failed` and restarts. Any other state means it landed. When
    the move can't be read, the source posts nothing and restarts nothing; the
    move times out and the finish restarts the session.
24. **Checks before `/exit`.** The source runs every check that can fail
    while the session still runs before it ends Claude: `claude --version`, a
    trial bundle build (the transcript, the size limits), the push, and for a
    cloud move the cloud branch. Right before `/exit` it checks again that
    the pane is idle, since a push can take minutes.
25. **The target starts only while the move is open.** Right before
    `claude --resume`, the target reads the move and starts Claude only while
    it is still `unpacking`. If the sessionhub then does not record its `done`, and
    the move does not read back as `done`, the target ends Claude in the new
    pane with `/exit`, removes the transcript it wrote, and undoes the clone,
    so the session never runs on both machines.
26. **Problems after the source's part closed.** A restart or an archive
    that fails after the move ended has no step left to report through. The
    source posts the move's final state again with a `detail`, and the server
    appends it to the move's detail (see "Source notes"), so `sessionhub move
    --status` and the dashboard show it.

## Review Focus

1. **A session resumed in two places.** The source must end Claude before it
   uploads, and must not leave a resumable transcript once the target runs
   it, the source restarts only after the sessionhub recorded a failure, and the
   target starts only while the move is open. Expected: the bundle is
   uploaded only after herdr reports the agent gone from the pane; on `done`
   the finish moves the transcript, sidecar, and file history to the
   archive; an upload with an unknown outcome restarts nothing; a target
   whose `done` is not recorded ends Claude again. Pinned in Task 7
   (`TestMoveOutEndsSessionBeforeUpload`, `TestFinishDoneArchives`,
   `TestMoveOutRestartsOnlyAfterRecordedFailure`) and Task 8
   (`TestMoveInStopsWhenMoveEnded`, `TestMoveInStopsWhenDoneNotRecorded`).
2. **A watcher restart mid-move.** Expected: the finish rides the long poll
   and is stored in `source_closed_at`, so a new watcher still archives or
   restores. Pinned in Task 3 (`TestClaimMoveFinishOnce`) and Task 7
   (`TestFinishFailedRestarts`).
3. **A bundle from the wrong machine.** Expected: `move.Open` fails for a
   wrong target key, a wrong source key, a changed byte, and a changed move
   ID; the target checks the manifest's move, session, and source. Pinned in
   Task 1 (`TestSealOpen`) and Task 8 (`TestMoveInFailures/wrong_sender`).
4. **A rewritten branch or a lost change on the target.** Expected:
   checkout is fast-forward only, a dirty clone fails before anything
   changes, and a failure after checkout takes back only what the move did:
   the files it wrote, the patch (`git apply -R`), and the branch (back to its
   old commit, or deleted when sessionhub created it). Work that is not the move's
   survives, and no file is written through a linked folder. Pinned in Task 8
   (`TestPrepareCloneRefusesDirty`, `TestPrepareCloneApplyUndo`,
   `TestApplyRefusesLinkedFolder`, `TestMoveInUndoAfterApplyFailure`).
5. **The cloud branch touching the user's work.** Expected: the temporary
   index leaves `git status`, the index, `HEAD`, and the current branch
   exactly as they were. Pinned in Task 9 (`TestPushCloudBranchLeavesTreeAlone`).
6. **Access.** Expected: the auth matrix walks the six routes with every
   credential; only the source uploads, only the target downloads, and a
   rejected request changes no row, no key, and no bundle file. Pinned in
   Task 4 (`TestAuthMatrix`).
7. **Secrets in transit.** Expected: no secret-looking file, untracked or
   tracked, enters a bundle or a cloud commit, the server only ever holds
   ciphertext, and the bundle file goes when the move ends. Pinned in Task 2
   (`TestBuildSkipsSecrets`), Task 4 (`TestMoveBundleDeletedOnEnd`), and
   Task 9 (`TestPushCloudBranchLeavesTreeAlone`).

## Dependencies and parallelism

| Task | Needs | Can run alongside |
|---|---|---|
| 1 Crypto, keys, and API types | none | none |
| 2 Bundle build and extract | 1 | 6 |
| 3 Store: schema v9 and moves | 1, 2 | 6 |
| 4 Server: routes, bundle files, and auth | 3 | none |
| 5 Client: move calls and streaming | 4 | none |
| 6 Resume: start a plain resume | none | 2 to 5 |
| 7 Watcher: key registration, move-out, and finish | 2, 5, 6 | none |
| 8 Watcher: move-in | 7 | none |
| 9 Cloud move | 7 | 10, 11 |
| 10 CLI: sessionhub move and sessionhub move-key | 5 | 9, 11 |
| 11 Dashboard: Move menu | 4 | 9, 10 |
| 12 README, SPEC, and PLAN | all | none |

Tasks 3 and 4 both edit `docs/server.md`; Tasks 7, 8, and 9 all edit
`internal/plugin/move.go` and `docs/plugin.md`; run each group in order.
Tasks 1, 2, 8, and 9 all add to `internal/move`; Task 3 imports it for
`move.IsGitHub`. Task 6 changes only `internal/resume` and can start at once.

---

### Task 1: Crypto, keys, and API types

**Files:**
- Create: `internal/api/move.go` (constants, types, `MoveOpen`,
  `ParseMoveKey`, `MoveKeyFingerprint`)
- Create: `internal/api/move_test.go`
- Modify: `internal/api/types.go` (`ControlClaim.Move`, `Session.Move`,
  `Machine.MoveKey`, `Machine.MoveReady`)
- Create: `internal/move/keys.go`
- Create: `internal/move/seal.go`
- Create: `internal/move/keys_test.go`
- Create: `internal/move/seal_test.go`
- Modify: `docs/client.md` (new section "Moves")

**Interfaces:**
- Consumes: `paths.ClientConfig`.
- Produces: everything under `internal/api/move.go` and `internal/move`
  keys and sealing in the interface ledger.

- [ ] **Step 1: Write the failing API tests**

Create `internal/api/move_test.go`:

```go
package api

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func TestMoveOpen(t *testing.T) {
	for state, want := range map[string]bool{
		MoveRequested: true, MovePacking: true, MoveUploaded: true, MoveUnpacking: true,
		MoveDone: false, MoveFailed: false, MoveCancelled: false, "": false, "other": false,
	} {
		if got := MoveOpen(state); got != want {
			t.Errorf("MoveOpen(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestParseMoveKey(t *testing.T) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	good := base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
	raw, err := ParseMoveKey(good)
	if err != nil || len(raw) != 32 {
		t.Fatalf("ParseMoveKey(good) = %d bytes, %v", len(raw), err)
	}
	for _, bad := range []string{
		"",
		"not base64!",
		base64.StdEncoding.EncodeToString(make([]byte, 31)),
		base64.StdEncoding.EncodeToString(make([]byte, 33)),
		base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
		good + "\n",
		" " + good,
	} {
		if _, err := ParseMoveKey(bad); err == nil {
			t.Errorf("ParseMoveKey(%q) accepted", bad)
		}
	}
}

func TestMoveKeyFingerprint(t *testing.T) {
	// SHA-256 of 32 zero bytes is 66687aadf862bd776c8fc18b8e9f8e20...
	if got := MoveKeyFingerprint(make([]byte, 32)); got != "66687aadf862bd77" {
		t.Errorf("fingerprint = %q", got)
	}
	if got := MoveKeyFingerprint(nil); len(got) != 16 || strings.Trim(got, "0123456789abcdef") != "" {
		t.Errorf("fingerprint of nothing = %q", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/api/ -count=1`
Expected: FAIL to compile with `undefined: MoveOpen`, `undefined:
ParseMoveKey`, and `undefined: MoveKeyFingerprint`.

- [ ] **Step 3: Add the API types**

Create `internal/api/move.go`:

```go
package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

// Moving a session to another machine or to the cloud. See docs/server.md,
// "Moves".
const (
	// HeaderActionMove is the X-Hub-Action value the dashboard sends with
	// POST /v1/sessions/{id}/move.
	HeaderActionMove = "move"

	// Control claim actions. move-out goes to the source machine (to pack, or
	// to finish once the move ended); move-in goes to the target machine.
	ActionMoveOut = "move-out"
	ActionMoveIn  = "move-in"

	// KindMoved is the event the server records when a move is done.
	KindMoved = "moved"

	// MoveCloud is the target of a move to a Claude Code cloud session.
	MoveCloud = "cloud"

	MoveRequested = "requested"
	MovePacking   = "packing"
	MoveUploaded  = "uploaded"
	MoveUnpacking = "unpacking"
	MoveDone      = "done"
	MoveFailed    = "failed"
	// MoveCancelled is defined for readers; no route sets it yet. Treat it
	// like MoveFailed.
	MoveCancelled = "cancelled"

	// MaxMoveDetailRunes caps MoveResultIn.Detail.
	MaxMoveDetailRunes = 300
	// MaxSealedBundle caps a sealed bundle, in bytes.
	MaxSealedBundle = 64 << 20

	// EndedMovedToCloud is the reason of the ended event a cloud move records.
	EndedMovedToCloud = "moved_to_cloud"
)

// MoveIn is the body of POST /v1/sessions/{id}/move: a machine name or
// "cloud".
type MoveIn struct {
	Target string `json:"target"`
}

// MoveKeyIn is the body of PUT /v1/machines/self/move-key: the machine's
// X25519 public key in standard base64.
type MoveKeyIn struct {
	PublicKey string `json:"public_key"`
}

// MoveResultIn is the body of POST /v1/moves/{id}/result: done or failed.
// CloudURL is set on a cloud move's done only.
type MoveResultIn struct {
	State    string `json:"state"`
	Detail   string `json:"detail,omitempty"`
	CloudURL string `json:"cloud_url,omitempty"`
}

// Move is one move of a session. Source and Target are machine names;
// Target is MoveCloud for a cloud move. By is web:<name> or machine:<name>.
type Move struct {
	ID         string    `json:"id"`
	SessionID  string    `json:"session_id"`
	Source     string    `json:"source"`
	Target     string    `json:"target"`
	State      string    `json:"state"`
	Detail     string    `json:"detail,omitempty"`
	CloudURL   string    `json:"cloud_url,omitempty"`
	By         string    `json:"by"`
	BundleSize int64     `json:"bundle_size"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// MoveClaim is the move part of a control claim. State is packing (pack and
// upload, or hand off to the cloud), unpacking (download and resume), or a
// final state (the source's finish: archive on done, restart otherwise).
// PeerKey is the other machine's public key: the target's on a pack, the
// source's on an unpack.
type MoveClaim struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	Target  string `json:"target"`
	State   string `json:"state"`
	PeerKey string `json:"peer_key,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// MoveOpen reports whether a move in state is still under way.
func MoveOpen(state string) bool {
	switch state {
	case MoveRequested, MovePacking, MoveUploaded, MoveUnpacking:
		return true
	}
	return false
}

// moveKeySize is the size of an X25519 public key.
const moveKeySize = 32

// ParseMoveKey decodes a move public key: standard base64 of exactly 32
// bytes, with nothing around it.
func ParseMoveKey(s string) ([]byte, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || len(raw) != moveKeySize || base64.StdEncoding.EncodeToString(raw) != s {
		return nil, errors.New("public_key: want 32 bytes in standard base64")
	}
	return raw, nil
}

// MoveKeyFingerprint is the first 16 hex characters of the SHA-256 of a raw
// public key, as sessionhub machine ls and sessionhub move-key print it.
func MoveKeyFingerprint(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}
```

In `internal/api/types.go`, replace:

```go
	// Text is the prompt to submit, for a request whose action is
	// ActionMessage.
	Text string `json:"text,omitempty"`
}
```

with:

```go
	// Text is the prompt to submit, for a request whose action is
	// ActionMessage.
	Text string `json:"text,omitempty"`
	// Move is set for ActionMoveOut and ActionMoveIn.
	Move *MoveClaim `json:"move,omitempty"`
}
```

replace:

```go
	// Summary is the digest's counts for the card; nil with no digest.
	Summary *SessionSummary `json:"summary,omitempty"`
}
```

with:

```go
	// Summary is the digest's counts for the card; nil with no digest.
	Summary *SessionSummary `json:"summary,omitempty"`
	// Move is the session's latest move, open or not; nil with none.
	Move *Move `json:"move,omitempty"`
}
```

and replace:

```go
type Machine struct {
	Name      string     `json:"name"`
	SSHHost   string     `json:"ssh_host"`
	HerdrHost string     `json:"herdr_host"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
}
```

with:

```go
type Machine struct {
	Name      string     `json:"name"`
	SSHHost   string     `json:"ssh_host"`
	HerdrHost string     `json:"herdr_host"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	// MoveKey is the fingerprint of the machine's move key, or "".
	MoveKey string `json:"move_key,omitempty"`
	// MoveReady is true when the machine has a move key and its watcher
	// polled in the last 2 minutes: it can take a moved session.
	MoveReady bool `json:"move_ready"`
}
```

- [ ] **Step 4: Run the API tests to verify they pass**

Run: `go test ./internal/api/ -count=1`
Expected: PASS.

- [ ] **Step 5: Write the failing key and seal tests**

Create `internal/move/keys_test.go`:

```go
package move

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

func TestKeyPathFollowsHubConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(dir, "config.toml"))
	if got, want := KeyPath(), filepath.Join(dir, "move.key"); got != want {
		t.Errorf("KeyPath() = %q, want %q", got, want)
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "move.key")
	k, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(data), "\n") || len(strings.TrimSpace(string(data))) != 44 {
		t.Errorf("key file %q: want 44 base64 characters and a newline", data)
	}
	again, err := LoadOrCreateKey(path)
	if err != nil || !bytes.Equal(again.Bytes(), k.Bytes()) {
		t.Fatalf("second load: %v; same key %v", err, err == nil && bytes.Equal(again.Bytes(), k.Bytes()))
	}
	pub := PublicKeyString(k)
	parsed, err := ParsePublicKey(pub)
	if err != nil || !bytes.Equal(parsed.Bytes(), k.PublicKey().Bytes()) {
		t.Fatalf("ParsePublicKey(PublicKeyString(k)): %v", err)
	}
	if got, want := Fingerprint(parsed), api.MoveKeyFingerprint(k.PublicKey().Bytes()); got != want || len(got) != 16 {
		t.Errorf("Fingerprint = %q, want %q", got, want)
	}

	// A key file others can read is refused, not used or replaced.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("mode 0644: %v, want a chmod 600 hint", err)
	}
	// A file that holds no key is refused and kept.
	bad := filepath.Join(t.TempDir(), "move.key")
	os.WriteFile(bad, []byte("hello\n"), 0o600)
	if _, err := LoadOrCreateKey(bad); err == nil {
		t.Error("garbage key file accepted")
	}
	if b, _ := os.ReadFile(bad); string(b) != "hello\n" {
		t.Errorf("garbage key file rewritten: %q", b)
	}
}

func TestLoadOrCreateKeyRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "move.key")
	const n = 8
	keys := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := LoadOrCreateKey(path)
			errs[i] = err
			if err == nil {
				keys[i] = PublicKeyString(k)
			}
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || keys[i] != keys[0] {
			t.Fatalf("racer %d: %v, key %q vs %q", i, errs[i], keys[i], keys[0])
		}
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".move-key-*")); len(m) != 0 {
		t.Errorf("temp files left: %v", m)
	}
}
```

Create `internal/move/seal_test.go`:

```go
package move

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"testing"
)

func newKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpen(t *testing.T) {
	src, dst, other := newKey(t), newKey(t), newKey(t)
	const id = "mv_AAAAAAAAAAAAAAAAAAAAAA"
	msg := bytes.Repeat([]byte("transcript line\n"), 1000)
	sealed, err := Seal(src, dst.PublicKey(), id, msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) != len(msg)+SealOverhead || sealed[0] != 1 {
		t.Fatalf("sealed size %d (want %d), version %d", len(sealed), len(msg)+SealOverhead, sealed[0])
	}
	if bytes.Contains(sealed, []byte("transcript line")) {
		t.Fatal("sealed bundle contains plaintext")
	}
	got, err := Open(dst, src.PublicKey(), id, sealed)
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("Open: %v, equal %v", err, bytes.Equal(got, msg))
	}
	again, _ := Seal(src, dst.PublicKey(), id, msg)
	if bytes.Equal(again, sealed) {
		t.Error("two seals of one bundle are identical; the ephemeral key or nonce is reused")
	}

	fails := map[string]func() ([]byte, error){
		"wrong target key": func() ([]byte, error) { return Open(other, src.PublicKey(), id, sealed) },
		"wrong source key": func() ([]byte, error) { return Open(dst, other.PublicKey(), id, sealed) },
		"other move id":    func() ([]byte, error) { return Open(dst, src.PublicKey(), "mv_BBBBBBBBBBBBBBBBBBBBBB", sealed) },
		"truncated":        func() ([]byte, error) { return Open(dst, src.PublicKey(), id, sealed[:SealOverhead-1]) },
		"empty":            func() ([]byte, error) { return Open(dst, src.PublicKey(), id, nil) },
	}
	for _, at := range []int{0, 1, 32, 33, 44, 45, len(sealed) / 2, len(sealed) - 1} {
		flipped := bytes.Clone(sealed)
		flipped[at] ^= 0x01
		fails["changed byte "+itoa(at)] = func() ([]byte, error) { return Open(dst, src.PublicKey(), id, flipped) }
	}
	for name, open := range fails {
		if out, err := open(); !errors.Is(err, ErrOpen) || out != nil {
			t.Errorf("%s: %v, %d bytes; want ErrOpen and nothing", name, err, len(out))
		}
	}
	if _, err := Seal(src, dst.PublicKey(), "", msg); err == nil {
		t.Error("Seal with an empty move ID succeeded")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./internal/move/ -count=1`
Expected: FAIL to compile with `undefined: KeyPath`, `undefined:
LoadOrCreateKey`, `undefined: Seal`, and `undefined: Open`.

- [ ] **Step 7: Write the key file code**

Create `internal/move/keys.go`:

```go
// Package move carries a Claude Code session to another machine or to the
// cloud: the machine key pair, the sealed bundle, the Git work on both ends,
// the transcript files, and the cloud hand-off. See docs/client.md, "Moves".
package move

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/paths"
)

// KeyPath is move.key next to the client config: ~/.config/sessionhub/move.key, or
// beside $SESSIONHUB_CONFIG.
func KeyPath() string { return filepath.Join(filepath.Dir(paths.ClientConfig()), "move.key") }

// LoadOrCreateKey reads this machine's X25519 private key from path, or
// creates it (mode 0600) when the file is missing. The new key is written to
// a temp file and linked into place, so a reader never sees half a key and a
// second process that loses the race reads the winner's key. A file others
// can read, or one that holds no key, is an error: sessionhub never replaces it.
func LoadOrCreateKey(path string) (*ecdh.PrivateKey, error) {
	k, err := readKey(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return k, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	k, err = ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".move-key-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.WriteString(base64.StdEncoding.EncodeToString(k.Bytes()) + "\n"); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(tmp.Name(), path); errors.Is(err, fs.ErrExist) {
		return readKey(path)
	} else if err != nil {
		return nil, err
	}
	return k, nil
}

func readKey(path string) (*ecdh.PrivateKey, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("move key %s has mode %04o; others must not read it: run chmod 600 %s", path, perm, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("move key %s does not hold a key; move it aside to make a new one", path)
	}
	return ecdh.X25519().NewPrivateKey(raw)
}

// PublicKeyString is k's public key as the server stores it: standard
// base64 of 32 bytes.
func PublicKeyString(k *ecdh.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
}

// ParsePublicKey reads a public key in the server's form.
func ParsePublicKey(s string) (*ecdh.PublicKey, error) {
	raw, err := api.ParseMoveKey(s)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPublicKey(raw)
}

// Fingerprint is the key's short form for checking it out of band.
func Fingerprint(pub *ecdh.PublicKey) string { return api.MoveKeyFingerprint(pub.Bytes()) }
```

- [ ] **Step 8: Write the sealing code**

Create `internal/move/seal.go`:

```go
package move

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

const (
	sealVersion = 1
	ephSize     = 32
	nonceSize   = 12
	tagSize     = 16
	// SealOverhead is how many bytes Seal adds: the version byte, the
	// ephemeral public key, the nonce, and the GCM tag.
	SealOverhead = 1 + ephSize + nonceSize + tagSize
	hkdfInfo     = "sessionhub move v1"
)

// ErrOpen is every Open failure. It never says which check failed.
var ErrOpen = errors.New("the bundle did not open: it was not sealed by the source machine for this one, or it changed on the way")

// sealAEAD derives the AES-256-GCM key from both shared secrets, with the
// move ID as the salt.
func sealAEAD(shared1, shared2 []byte, moveID string) (cipher.AEAD, error) {
	secret := make([]byte, 0, len(shared1)+len(shared2))
	secret = append(append(secret, shared1...), shared2...)
	key, err := hkdf.Key(sha256.New, secret, []byte(moveID), hkdfInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext for recipient. An ephemeral key pair gives
// forward secrecy; the second ECDH, between sender's static key and the
// recipient, makes only sender able to produce a bundle the recipient opens
// with sender's public key. The move ID is the HKDF salt and the GCM
// additional data, so a bundle opens only for its own move.
func Seal(sender *ecdh.PrivateKey, recipient *ecdh.PublicKey, moveID string, plaintext []byte) ([]byte, error) {
	if moveID == "" {
		return nil, errors.New("seal: empty move ID")
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	s1, err := eph.ECDH(recipient)
	if err != nil {
		return nil, err
	}
	s2, err := sender.ECDH(recipient)
	if err != nil {
		return nil, err
	}
	aead, err := sealAEAD(s1, s2, moveID)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, SealOverhead+len(plaintext))
	out = append(out, sealVersion)
	out = append(out, eph.PublicKey().Bytes()...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, []byte(moveID)), nil
}

// Open decrypts a sealed bundle with the recipient's private key and the
// sender's registered public key. Every failure is ErrOpen.
func Open(recipient *ecdh.PrivateKey, sender *ecdh.PublicKey, moveID string, sealed []byte) ([]byte, error) {
	if len(sealed) < SealOverhead || sealed[0] != sealVersion || moveID == "" {
		return nil, ErrOpen
	}
	eph, err := ecdh.X25519().NewPublicKey(sealed[1 : 1+ephSize])
	if err != nil {
		return nil, ErrOpen
	}
	s1, err := recipient.ECDH(eph)
	if err != nil {
		return nil, ErrOpen
	}
	s2, err := recipient.ECDH(sender)
	if err != nil {
		return nil, ErrOpen
	}
	aead, err := sealAEAD(s1, s2, moveID)
	if err != nil {
		return nil, ErrOpen
	}
	nonce := sealed[1+ephSize : 1+ephSize+nonceSize]
	out, err := aead.Open(nil, nonce, sealed[1+ephSize+nonceSize:], []byte(moveID))
	if err != nil {
		return nil, ErrOpen
	}
	return out, nil
}
```

- [ ] **Step 9: Run the tests to verify they pass**

Run: `go test ./internal/api/ ./internal/move/ -count=1 -race`
Expected: PASS. `TestLoadOrCreateKeyRace` proves that eight racing starts
end with one key and no temp files.

- [ ] **Step 10: Document the key**

In `docs/client.md`, before `## Digests`, add:

```markdown
## Moves

`internal/move` carries a session to another machine or to the cloud. This
section covers the key and the sealing; later sections cover the bundle, Git,
and the watcher's steps.

- **Key.** Each machine has an X25519 key pair. `move.KeyPath()` is
  `move.key` next to the client config (`~/.config/sessionhub/move.key`, or beside
  `$SESSIONHUB_CONFIG`). The file holds the private key in standard base64 and a
  newline, mode `0600`. `LoadOrCreateKey` creates it on first use through a
  temp file and `link`, so two processes that start at once end with one
  key. A file that others can read, or that holds no key, is refused and
  never replaced: move it aside to make a new key, then restart the watcher
  so the server learns the new public key.
- **Fingerprint.** The first 16 hex characters of the SHA-256 of the 32-byte
  public key. `sessionhub machine ls` (on the server) and `sessionhub move-key` (on each
  machine) print it, so you can compare them out of band.
- **Sealing.** `Seal(source key, target public key, move ID, bundle)` makes an
  ephemeral key pair, runs ECDH with the target's key, runs ECDH between the
  source's static key and the target's key, and derives a 32-byte key with
  HKDF-SHA256 (salt: the move ID, info: `sessionhub move v1`). AES-256-GCM encrypts
  the whole bundle with the move ID as additional data. The sealed form is
  the version byte `1`, the 32-byte ephemeral public key, the 12-byte nonce,
  and the ciphertext with its tag (`SealOverhead`, 61 bytes, in all). `Open`
  takes the target's private key and the source's registered public key;
  a wrong key, a changed byte, or another move ID all fail with `ErrOpen`.
  Both work in memory, on at most 64 MiB.
- **Threat model.** The server and Cloudflare relay ciphertext and cannot
  read it. Whoever controls the server or the tunnel could swap a registered
  public key; compare fingerprints out of band to detect that.
```

- [ ] **Step 11: Lint and commit**

Run: `gofmt -w internal/api/move.go internal/api/move_test.go internal/api/types.go internal/move/*.go && make lint`
Expected: no output from `gofmt -l`, `go vet` clean.

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1`
Expected: every package `ok` (the new fields are `omitempty` or new, so no
existing JSON test changes).

```bash
git add internal/api/move.go internal/api/move_test.go internal/api/types.go \
  internal/move/keys.go internal/move/seal.go internal/move/keys_test.go internal/move/seal_test.go docs/client.md
git commit -m "$(cat <<'EOF'
Add move keys, sealing, and the move API types

Each machine gets an X25519 key in ~/.config/sessionhub/move.key (0600, created
through a temp file and link so racing starts agree). Seal uses an
ephemeral ECDH plus a static ECDH between the two machines, HKDF-SHA256
salted with the move ID, and AES-256-GCM with the move ID as additional
data, so only the source can make a bundle the target opens.

Refs: docs/dev/superpowers/plans/2026-10-03-move-session.md, Task 1

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 2: Bundle build and extract

**Files:**
- Create: `internal/move/git.go` (`git` runner, `Repo`, `ReadRepo`,
  `NormalizeRemote`, `IsGitHub`, `DefaultRoots`)
- Create: `internal/move/bundle.go` (`Manifest`, `File`, `Bundle`,
  `BuildInput`, `Build`, `Extract`, `FindTranscripts`, `IsSecretName`)
- Create: `internal/move/helpers_test.go` (temp Git repositories and a temp
  `~/.claude`)
- Create: `internal/move/git_test.go`
- Create: `internal/move/bundle_test.go`
- Modify: `docs/client.md` (section "Moves")

**Interfaces:**
- Consumes: `api.MaxSealedBundle`, `SealOverhead` (Task 1),
  `termtext.Clean`.
- Produces: `Repo`, `ReadRepo`, `NormalizeRemote`, `IsGitHub`,
  `DefaultRoots`, `Manifest`, `File`, `Bundle`, `BuildInput`, `Build`,
  `Extract`, `FindTranscripts`, `IsSecretName`; `diffArgs`,
  `secretPathspecs`, `changedSecrets`, `trackedChanges` (Task 9 reuses the
  pathspecs and `changedSecrets`); test helpers `gitIsolate`,
  `gitT`, `writeFile`, `newRepo`, `newClaudeDir`, `testSessionID`.

- [ ] **Step 1: Write the test helpers**

Create `internal/move/helpers_test.go`:

```go
package move

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testSessionID looks like a Claude session UUID.
const testSessionID = "11111111-2222-4333-8444-555555555555"

// gitIsolate keeps the developer's Git config (signing, hooks, aliases) out
// of the test and gives commits a fixed identity.
func gitIsolate(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Hub Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "sessionhub@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "Hub Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "sessionhub@example.test")
}

// gitT runs git in dir and fails the test on an error.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeFile writes data to path with mode, creating parent directories.
func writeFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// newRepo makes a bare origin and a clone of it with one pushed commit on
// main: a.txt, bin.dat (binary), gone.txt, sub/keep.txt, and a .gitignore
// that ignores build/.
func newRepo(t *testing.T) (origin, work string) {
	t.Helper()
	gitIsolate(t)
	base := t.TempDir()
	origin = filepath.Join(base, "origin.git")
	gitT(t, base, "init", "-q", "--bare", "-b", "main", origin)
	work = filepath.Join(base, "work")
	gitT(t, base, "clone", "-q", origin, work)
	writeFile(t, filepath.Join(work, "a.txt"), "one\n", 0o644)
	writeFile(t, filepath.Join(work, "bin.dat"), "\x00\x01\x02\x03binary", 0o644)
	writeFile(t, filepath.Join(work, "gone.txt"), "delete me\n", 0o644)
	writeFile(t, filepath.Join(work, "sub", "keep.txt"), "keep\n", 0o644)
	writeFile(t, filepath.Join(work, ".gitignore"), "build/\n", 0o644)
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "-q", "-m", "initial")
	gitT(t, work, "push", "-q", "-u", "origin", "main")
	return origin, work
}

// newClaudeDir makes a temp ~/.claude with session id's transcript in a
// project folder, a sidecar folder (subagents/ and tool-results/), and file
// history.
func newClaudeDir(t *testing.T, id string) string {
	t.Helper()
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "-home-user-proj")
	writeFile(t, filepath.Join(proj, id+".jsonl"), `{"type":"user","message":"hello"}`+"\n", 0o600)
	writeFile(t, filepath.Join(proj, id, "subagents", "agent-1.jsonl"), `{"type":"assistant"}`+"\n", 0o600)
	writeFile(t, filepath.Join(proj, id, "tool-results", "r1.txt"), "tool output\n", 0o600)
	writeFile(t, filepath.Join(dir, "file-history", id, "abc@v1"), "snapshot\n", 0o600)
	return dir
}
```

- [ ] **Step 2: Write the failing Git tests**

Create `internal/move/git_test.go`:

```go
package move

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeRemote(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"git@github.com:Owner/Repo.git", "github.com/Owner/Repo"},
		{"https://github.com/Owner/Repo", "github.com/Owner/Repo"},
		{"ssh://git@GitHub.com/Owner/Repo.git/", "github.com/Owner/Repo"},
		{"https://user:secret@github.com/Owner/Repo.git", "github.com/Owner/Repo"},
		{"git@gitlab.example.com:group/sub/proj.git", "gitlab.example.com/group/sub/proj"},
		{"/srv/git/proj.git", "/srv/git/proj"},
		{"  https://github.com/Owner/Repo.git\n", "github.com/Owner/Repo"},
	} {
		if got := NormalizeRemote(c.in); got != c.want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for u, want := range map[string]bool{
		"git@github.com:o/r.git":      true,
		"https://GITHUB.COM/o/r":      true,
		"git@gitlab.com:o/r.git":      false,
		"https://github.com.evil/o/r": false,
		"/srv/git/github.com/o/r":     false,
	} {
		if got := IsGitHub(u); got != want {
			t.Errorf("IsGitHub(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestGitEnvBatchMode(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "ssh -i /k")
	env := gitEnv([]string{"GIT_INDEX_FILE=/tmp/i"})
	last := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		last[k] = v
	}
	if last["GIT_SSH_COMMAND"] != "ssh -i /k -o BatchMode=yes" || last["GIT_TERMINAL_PROMPT"] != "0" || last["GIT_INDEX_FILE"] != "/tmp/i" {
		t.Errorf("env: ssh %q prompt %q index %q", last["GIT_SSH_COMMAND"], last["GIT_TERMINAL_PROMPT"], last["GIT_INDEX_FILE"])
	}
	t.Setenv("GIT_SSH_COMMAND", "")
	for _, kv := range gitEnv(nil) {
		if strings.HasPrefix(kv, "GIT_SSH_COMMAND=") && kv != "GIT_SSH_COMMAND=" && kv != "GIT_SSH_COMMAND=ssh -o BatchMode=yes" {
			t.Errorf("default ssh: %q", kv)
		}
	}
}

func TestDefaultRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := DefaultRoots([]string{"~/src", "/opt/x", "relative", "/opt/x/"})
	want := []string{filepath.Join(home, "Code"), filepath.Join(home, "src"), "/opt/x"}
	if !slices.Equal(got, want) {
		t.Errorf("DefaultRoots = %q, want %q", got, want)
	}
}

func TestReadRepo(t *testing.T) {
	ctx := context.Background()
	_, work := newRepo(t)
	r, err := ReadRepo(ctx, filepath.Join(work, "sub"), []string{filepath.Dir(filepath.Dir(work))})
	if err != nil {
		t.Fatal(err)
	}
	wantRootRel := filepath.Base(filepath.Dir(work)) + "/work"
	if r.Branch != "main" || r.Head != gitT(t, work, "rev-parse", "HEAD") || r.RelPath != "sub" || r.RootRel != wantRootRel ||
		!strings.HasSuffix(r.Remote, "origin.git") {
		t.Errorf("repo %+v; want branch main, rel sub, root rel %q", r, wantRootRel)
	}
	if r, err := ReadRepo(ctx, work, nil); err != nil || r.RelPath != "" || r.RootRel != "work" {
		t.Errorf("at the root: %+v %v", r, err)
	}

	fails := []struct {
		name, want string
		cwd        string
		setup      func()
	}{
		{"relative", "not an absolute path", "work", nil},
		{"missing", "does not exist", filepath.Join(work, "nope"), nil},
		{"not a repo", "not in a Git repository", t.TempDir(), nil},
		{"detached", "detached HEAD", work, func() { gitT(t, work, "checkout", "-q", "--detach") }},
		{"no origin", "no origin remote", work, func() { gitT(t, work, "checkout", "-q", "main"); gitT(t, work, "remote", "remove", "origin") }},
	}
	for _, f := range fails {
		if f.setup != nil {
			f.setup()
		}
		if _, err := ReadRepo(ctx, f.cwd, nil); err == nil || !strings.Contains(err.Error(), f.want) {
			t.Errorf("%s: %v, want %q", f.name, err, f.want)
		}
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 3: Write the failing bundle tests**

Create `internal/move/bundle_test.go`:

```go
package move

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var bundleNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// dirtyRepo is newRepo with a tracked secret-looking file (committed and
// pushed, then changed), and staged, unstaged, binary, deleted, untracked,
// executable, secret-looking, linked, and ignored changes.
func dirtyRepo(t *testing.T) (origin, work string) {
	t.Helper()
	origin, work = newRepo(t)
	writeFile(t, filepath.Join(work, "conf", ".env.prod"), "secret\n", 0o600)
	gitT(t, work, "add", "conf/.env.prod")
	gitT(t, work, "commit", "-q", "-m", "tracked secret")
	gitT(t, work, "push", "-q")
	writeFile(t, filepath.Join(work, "conf", ".env.prod"), "secret\nrotated\n", 0o600)
	writeFile(t, filepath.Join(work, "a.txt"), "one\nstaged\n", 0o644)
	gitT(t, work, "add", "a.txt")
	writeFile(t, filepath.Join(work, "a.txt"), "one\nstaged\nunstaged\n", 0o644)
	writeFile(t, filepath.Join(work, "bin.dat"), "\x00\x01\x02changed\xff\xfe", 0o644)
	if err := os.Remove(filepath.Join(work, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(work, "notes", "new.md"), "new note\n", 0o644)
	writeFile(t, filepath.Join(work, "run.sh"), "#!/bin/sh\necho hi\n", 0o755)
	for _, s := range []string{".env.local", "certs/server.pem", "deploy.key", "prod.tfvars"} {
		writeFile(t, filepath.Join(work, s), "secret\n", 0o600)
	}
	if err := os.Symlink("a.txt", filepath.Join(work, "link")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(work, "build", "out.o"), "object", 0o644)
	return origin, work
}

func buildFor(t *testing.T, work, claude string) ([]byte, Manifest) {
	t.Helper()
	ctx := context.Background()
	repo, err := ReadRepo(ctx, filepath.Join(work, "sub"), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, man, err := Build(ctx, BuildInput{MoveID: "mv_AAAAAAAAAAAAAAAAAAAAAA", SessionID: testSessionID, Machine: "bluebox",
		CWD: filepath.Join(work, "sub"), ClaudeDir: claude, ClaudeVersion: "2.1.285", Repo: repo, Now: bundleNow})
	if err != nil {
		t.Fatal(err)
	}
	return data, man
}

func names(fs []File) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

func TestBuildAndExtract(t *testing.T) {
	origin, work := dirtyRepo(t)
	claude := newClaudeDir(t, testSessionID)
	data, man := buildFor(t, work, claude)

	b, err := Extract(data)
	if err != nil {
		t.Fatal(err)
	}
	m := b.Manifest
	if m.V != 1 || m.MoveID != "mv_AAAAAAAAAAAAAAAAAAAAAA" || m.SessionID != testSessionID || m.SourceMachine != "bluebox" ||
		m.RelPath != "sub" || m.RootRel != "work" || m.Branch != "main" || m.Head != gitT(t, work, "rev-parse", "HEAD") ||
		m.Remote != origin || m.ClaudeVersion != "2.1.285" || !m.Created.Equal(bundleNow) || m.SourceCWD != filepath.Join(work, "sub") {
		t.Errorf("manifest %+v", m)
	}
	if !slices.Equal(m.Files, man.Files) {
		t.Errorf("extracted files %q, built %q", m.Files, man.Files)
	}
	if string(b.Transcript) != `{"type":"user","message":"hello"}`+"\n" {
		t.Errorf("transcript %q", b.Transcript)
	}
	if got := names(b.Sidecar); !slices.Equal(got, []string{"subagents/agent-1.jsonl", "tool-results/r1.txt"}) {
		t.Errorf("sidecar %q", got)
	}
	if got := names(b.History); !slices.Equal(got, []string{"abc@v1"}) {
		t.Errorf("file history %q", got)
	}
	if got := names(b.Untracked); !slices.Equal(got, []string{"notes/new.md", "run.sh"}) {
		t.Errorf("untracked %q", got)
	}
	for _, f := range b.Untracked {
		if f.Exec != (f.Name == "run.sh") {
			t.Errorf("%s: exec %v", f.Name, f.Exec)
		}
	}

	// The patch carries staged, unstaged, binary, and deleted changes: a
	// fresh clone at HEAD ends up with the same files.
	fresh := filepath.Join(t.TempDir(), "fresh")
	gitT(t, filepath.Dir(fresh), "clone", "-q", origin, fresh)
	patch := filepath.Join(t.TempDir(), "changes.patch")
	if err := os.WriteFile(patch, b.Patch, 0o600); err != nil {
		t.Fatal(err)
	}
	gitT(t, fresh, "apply", "--binary", patch)
	for _, f := range []string{"a.txt", "bin.dat"} {
		want, _ := os.ReadFile(filepath.Join(work, f))
		got, _ := os.ReadFile(filepath.Join(fresh, f))
		if !bytes.Equal(got, want) {
			t.Errorf("%s after the patch: %q, want %q", f, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(fresh, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("gone.txt survived the patch: %v", err)
	}
}

func TestBuildSkipsSecrets(t *testing.T) {
	_, work := dirtyRepo(t)
	data, man := buildFor(t, work, newClaudeDir(t, testSessionID))
	skipped := slices.Clone(man.Skipped)
	slices.Sort(skipped)
	if want := []string{".env.local", "certs/server.pem", "conf/.env.prod", "deploy.key", "link", "prod.tfvars"}; !slices.Equal(skipped, want) {
		t.Errorf("skipped %q, want %q", skipped, want)
	}
	// The skipped names are in the manifest on purpose; their content, the
	// tracked secret's change, and ignored files must stay out.
	for _, leak := range []string{"secret\n", "rotated", "build/out.o", "object"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Errorf("bundle contains %q", leak)
		}
	}
	for _, n := range []string{".env", ".ENV.prod", "a/b/x.pem", "id.key", "x.tfvars", ".envrc"} {
		if !IsSecretName(n) {
			t.Errorf("IsSecretName(%q) = false", n)
		}
	}
	for _, n := range []string{"env.go", "keys.go", "pem.md", "terraform.tf", "a.txt"} {
		if IsSecretName(n) {
			t.Errorf("IsSecretName(%q) = true", n)
		}
	}
}

// The user's diff config (no prefixes, color, rename and copy detection, an
// external diff tool) must not change the patch.
func TestBuildIgnoresDiffConfig(t *testing.T) {
	origin, work := dirtyRepo(t)
	for _, kv := range [][2]string{{"diff.noprefix", "true"}, {"color.diff", "always"}, {"color.ui", "always"},
		{"diff.renames", "copies"}, {"diff.external", "false"}, {"diff.mnemonicPrefix", "true"}} {
		gitT(t, work, "config", kv[0], kv[1])
	}
	data, _ := buildFor(t, work, newClaudeDir(t, testSessionID))
	b, err := Extract(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b.Patch, []byte("diff --git a/a.txt b/a.txt\n")) || bytes.Contains(b.Patch, []byte("\x1b[")) {
		t.Fatalf("patch shaped by the user's config:\n%s", b.Patch)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	gitT(t, filepath.Dir(fresh), "clone", "-q", origin, fresh)
	patch := filepath.Join(t.TempDir(), "changes.patch")
	if err := os.WriteFile(patch, b.Patch, 0o600); err != nil {
		t.Fatal(err)
	}
	gitT(t, fresh, "apply", "--binary", patch)
	gitT(t, fresh, "apply", "-R", "--binary", patch) // full object IDs make the binary hunk reversible
	if st := gitT(t, fresh, "status", "--porcelain"); st != "" {
		t.Errorf("apply then apply -R left:\n%s", st)
	}
}

func TestBuildLimits(t *testing.T) {
	ctx := context.Background()
	_, work := newRepo(t)
	claude := newClaudeDir(t, testSessionID)
	repo, err := ReadRepo(ctx, work, nil)
	if err != nil {
		t.Fatal(err)
	}
	in := BuildInput{MoveID: "mv_AAAAAAAAAAAAAAAAAAAAAA", SessionID: testSessionID, Machine: "bluebox", CWD: work,
		ClaudeDir: claude, Repo: repo, Now: bundleNow}

	oldFile, oldSealed := maxUntrackedFile, maxSealed
	t.Cleanup(func() { maxUntrackedFile, maxSealed = oldFile, oldSealed })

	maxUntrackedFile = 16
	writeFile(t, filepath.Join(work, "big.bin"), strings.Repeat("x", 17), 0o644)
	writeFile(t, filepath.Join(work, "small.txt"), "ok", 0o644)
	if _, _, err := Build(ctx, in); err == nil || !strings.Contains(err.Error(), "big.bin") || strings.Contains(err.Error(), "small.txt") {
		t.Errorf("file limit: %v, want an error naming big.bin only", err)
	}
	maxUntrackedFile = oldFile

	maxSealed = 4096
	writeFile(t, filepath.Join(claude, "projects", "-home-user-proj", testSessionID+".jsonl"), strings.Repeat("y", 8192), 0o600)
	if _, _, err := Build(ctx, in); err == nil || !strings.Contains(err.Error(), "limit") ||
		!strings.Contains(err.Error(), "transcript/"+testSessionID+".jsonl") {
		t.Errorf("bundle limit: %v, want an error naming the transcript", err)
	}

	// Two transcripts with one ID is refused.
	maxSealed = oldSealed
	writeFile(t, filepath.Join(claude, "projects", "-other", testSessionID+".jsonl"), "x\n", 0o600)
	if _, _, err := Build(ctx, in); err == nil || !strings.Contains(err.Error(), "found 2 transcripts") {
		t.Errorf("two transcripts: %v", err)
	}
}

type tarEntry struct {
	hdr  tar.Header
	data []byte
}

func readEntries(t *testing.T, data []byte) []tarEntry {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	var out []tarEntry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out = append(out, tarEntry{*h, b})
	}
}

func writeEntries(t *testing.T, es []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		h := e.hdr
		h.Size = int64(len(e.data))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		tw.Write(e.data)
	}
	tw.Close()
	return buf.Bytes()
}

func TestExtractRefuses(t *testing.T) {
	_, work := dirtyRepo(t)
	data, _ := buildFor(t, work, newClaudeDir(t, testSessionID))
	base := readEntries(t, data)
	reg := func(name, body string) tarEntry {
		return tarEntry{tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Format: tar.FormatPAX}, []byte(body)}
	}
	// withListed adds name to the manifest's file list, so only the check
	// under test can refuse it.
	withListed := func(es []tarEntry, name string) []tarEntry {
		var m Manifest
		json.Unmarshal(es[0].data, &m)
		m.Files = append(m.Files, name)
		es[0].data, _ = json.Marshal(m)
		return es
	}
	innerWith := func(name string) []byte {
		return writeEntries(t, []tarEntry{reg(name, "x")})
	}
	cases := map[string]func([]tarEntry) []tarEntry{
		"traversal": func(es []tarEntry) []tarEntry {
			n := "transcript/" + testSessionID + "/../../escape"
			return append(withListed(es, n), reg(n, "x"))
		},
		"absolute": func(es []tarEntry) []tarEntry { return append(withListed(es, "/etc/x"), reg("/etc/x", "x")) },
		"unknown":  func(es []tarEntry) []tarEntry { return append(withListed(es, "other.txt"), reg("other.txt", "x")) },
		"unlisted": func(es []tarEntry) []tarEntry {
			return append(es, reg("file-history/"+testSessionID+"/extra", "x"))
		},
		"listed but missing": func(es []tarEntry) []tarEntry { return withListed(es, "file-history/"+testSessionID+"/ghost") },
		"duplicate":          func(es []tarEntry) []tarEntry { return append(es, es[len(es)-1]) },
		"link": func(es []tarEntry) []tarEntry {
			n := "transcript/" + testSessionID + "/l"
			return append(withListed(es, n), tarEntry{tar.Header{Typeflag: tar.TypeSymlink, Name: n, Linkname: "/etc/passwd"}, nil})
		},
		"manifest last": func(es []tarEntry) []tarEntry { return append(es[1:], es[0]) },
		"no manifest":   func(es []tarEntry) []tarEntry { return es[1:] },
		"no transcript": func(es []tarEntry) []tarEntry {
			var out []tarEntry
			for _, e := range es {
				if e.hdr.Name != "transcript/"+testSessionID+".jsonl" {
					out = append(out, e)
				}
			}
			return out
		},
		"untracked escape": func(es []tarEntry) []tarEntry {
			es[len(es)-1].data = innerWith("../evil")
			return es
		},
		"untracked into .git": func(es []tarEntry) []tarEntry {
			es[len(es)-1].data = innerWith(".git/hooks/post-checkout")
			return es
		},
		"untracked not in manifest": func(es []tarEntry) []tarEntry {
			es[len(es)-1].data = innerWith("surprise.txt")
			return es
		},
	}
	for name, edit := range cases {
		es := edit(slices.Clone(base))
		if b, err := Extract(writeEntries(t, es)); err == nil || b != nil {
			t.Errorf("%s: Extract accepted it", name)
		}
	}
	if _, err := Extract(data); err != nil {
		t.Fatalf("the unedited bundle no longer extracts: %v", err)
	}
	if _, err := Extract([]byte("not a tar")); err == nil {
		t.Error("garbage accepted")
	}
}
```

- [ ] **Step 4: Run them to verify they fail**

Run: `go test ./internal/move/ -count=1`
Expected: FAIL to compile with `undefined: NormalizeRemote`, `undefined:
gitEnv`, `undefined: ReadRepo`, `undefined: Build`, and `undefined: Extract`.
`TestBuildIgnoresDiffConfig` and the tracked secret in `TestBuildSkipsSecrets`
pin the diff flags and the pathspec excludes.

- [ ] **Step 5: Write the Git runner and repository reader**

Create `internal/move/git.go`:

```go
package move

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/cli/termtext"
)

// Git time limits: local commands, and push or fetch.
const (
	gitTimeout    = 30 * time.Second
	gitNetTimeout = 2 * time.Minute
)

// gitOpts are the less common parts of a git call.
type gitOpts struct {
	timeout time.Duration
	env     []string
	stdin   io.Reader
}

// gitEnv is the environment of every git call: never a password prompt, and
// SSH that fails instead of asking (BatchMode, added to the user's
// GIT_SSH_COMMAND when there is one). extra comes last, so it wins.
func gitEnv(extra []string) []string {
	ssh := os.Getenv("GIT_SSH_COMMAND")
	if ssh == "" {
		ssh = "ssh"
	}
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND="+ssh+" -o BatchMode=yes")
	return append(env, extra...)
}

// gitRaw runs git -C dir args and returns its stdout as is. The error names
// the subcommand and carries git's message on one line.
func gitRaw(ctx context.Context, dir string, o gitOpts, args ...string) ([]byte, error) {
	if o.timeout == 0 {
		o.timeout = gitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv(o.env)
	cmd.Stdin = o.stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.Join(strings.Fields(errb.String()), " ")
		switch {
		case ctx.Err() != nil:
			msg = "timed out after " + o.timeout.String()
		case msg == "":
			msg = err.Error()
		}
		return out.Bytes(), fmt.Errorf("git %s: %s", args[0], termtext.Clean(msg, 200))
	}
	return out.Bytes(), nil
}

// git is gitRaw with the default timeout and trimmed output.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitRaw(ctx, dir, gitOpts{}, args...)
	return strings.TrimSpace(string(out)), err
}

// Repo is what a move needs to know about the repository a session works in.
type Repo struct {
	Root    string // the work tree's top level, as git reports it
	RelPath string // the session's directory relative to Root, slash-separated; "" at Root
	RootRel string // Root relative to the search root that holds it, else Root's base name
	Remote  string // origin's URL
	Branch  string
	Head    string // the full SHA of HEAD
}

// ReadRepo reads the repository of cwd. It fails when cwd is not an absolute
// directory in a Git work tree with an origin remote, a branch checked out,
// and at least one commit. roots are the search roots (DefaultRoots) that
// RootRel is relative to.
func ReadRepo(ctx context.Context, cwd string, roots []string) (Repo, error) {
	if !filepath.IsAbs(cwd) {
		return Repo{}, fmt.Errorf("the session's directory %q is not an absolute path", cwd)
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return Repo{}, fmt.Errorf("the session's directory %s does not exist", cwd)
	}
	root, err := git(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return Repo{}, fmt.Errorf("%s is not in a Git repository", cwd)
	}
	r := Repo{Root: root}
	if r.Remote, err = git(ctx, root, "remote", "get-url", "origin"); err != nil || r.Remote == "" {
		return Repo{}, errors.New("the repository has no origin remote")
	}
	if r.Branch, err = git(ctx, root, "symbolic-ref", "--short", "-q", "HEAD"); err != nil || r.Branch == "" {
		return Repo{}, errors.New("the repository is on a detached HEAD; check out a branch first")
	}
	if r.Head, err = git(ctx, root, "rev-parse", "--verify", "-q", "HEAD"); err != nil || r.Head == "" {
		return Repo{}, errors.New("the branch has no commits yet")
	}
	if r.RelPath, err = relUnder(root, cwd); err != nil {
		return Repo{}, err
	}
	r.RootRel = rootRel(root, roots)
	return r, nil
}

// relUnder is dir relative to root, slash-separated, "" for root itself,
// with symbolic links resolved on both. It fails when dir is not under root.
func relUnder(root, dir string) (string, error) {
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	dd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rr, dd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is not under %s", dir, root)
	}
	if rel == "." {
		return "", nil
	}
	return filepath.ToSlash(rel), nil
}

// rootRel is root relative to the first search root that holds it, else its
// base name.
func rootRel(root string, roots []string) string {
	for _, r := range roots {
		if rel, err := relUnder(r, root); err == nil && rel != "" {
			return rel
		}
	}
	return filepath.Base(root)
}

// DefaultRoots are the directories searched for clones: ~/Code, then each
// absolute or ~/ entry of extra (the client config's move_roots), once each.
func DefaultRoots(extra []string) []string {
	home, _ := os.UserHomeDir()
	out := []string{filepath.Join(home, "Code")}
	for _, r := range extra {
		if strings.HasPrefix(r, "~/") {
			r = filepath.Join(home, r[2:])
		}
		if !filepath.IsAbs(r) {
			continue
		}
		if r = filepath.Clean(r); !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// NormalizeRemote is a remote URL without its scheme, user, trailing "/" and
// ".git", with the host in lower case, and with git@host:path written as
// host/path, so the same repository compares equal over SSH and HTTPS. A
// local path keeps its form, without a trailing "/" or ".git".
func NormalizeRemote(u string) string {
	s := strings.TrimSpace(u)
	local := false
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if at := strings.IndexByte(s, '@'); at >= 0 && at < strings.IndexByte(s+"/", '/') {
			s = s[at+1:]
		}
	} else if c := strings.IndexByte(s, ':'); c > 0 && !strings.Contains(s[:c], "/") {
		host := s[:c]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		s = host + "/" + strings.TrimPrefix(s[c+1:], "/")
	} else {
		local = true
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	if local {
		return s
	}
	host, path, _ := strings.Cut(s, "/")
	return strings.ToLower(host) + "/" + path
}

// IsGitHub reports whether remote u is on github.com.
func IsGitHub(u string) bool {
	n := NormalizeRemote(u)
	return strings.HasPrefix(n, "github.com/")
}
```

- [ ] **Step 6: Write the bundle code**

Create `internal/move/bundle.go`:

```go
package move

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

const manifestVersion = 1

// Bundle entry names. The transcript, its sidecar folder, and the file
// history are under transcript/ and file-history/.
const (
	entryManifest  = "manifest.json"
	entryPatch     = "git/changes.patch"
	entryUntracked = "git/untracked.tar"
)

var (
	// maxUntrackedFile caps one untracked file; maxSealed caps the sealed
	// bundle. Tests lower them.
	maxUntrackedFile int64 = 10 << 20
	maxSealed        int64 = api.MaxSealedBundle

	sessionIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
)

// Manifest describes a bundle. Files lists every entry but manifest.json, in
// order; Untracked lists the files in git/untracked.tar; Skipped lists what
// sessionhub did not carry: tracked secret-looking files whose changes it left out
// of the patch, and untracked secret-looking names, links, and nested
// repositories, so the target can say what to copy by hand.
type Manifest struct {
	V             int       `json:"v"`
	MoveID        string    `json:"move_id"`
	SessionID     string    `json:"session_id"`
	SourceMachine string    `json:"source_machine"`
	SourceCWD     string    `json:"source_cwd"`
	RelPath       string    `json:"rel_path"`
	RootRel       string    `json:"root_rel"`
	Remote        string    `json:"remote"`
	Branch        string    `json:"branch"`
	Head          string    `json:"head"`
	ClaudeVersion string    `json:"claude_version"`
	Created       time.Time `json:"created"`
	Files         []string  `json:"files"`
	Untracked     []string  `json:"untracked"`
	Skipped       []string  `json:"skipped"`
}

// File is one file of a bundle. Name is relative to its folder (the sidecar
// folder, the file-history folder, or the repository root) and
// slash-separated. Exec is the executable bit of an untracked file.
type File struct {
	Name string
	Data []byte
	Exec bool
}

// Bundle is an opened, checked bundle.
type Bundle struct {
	Manifest   Manifest
	Transcript []byte
	Sidecar    []File
	History    []File
	Patch      []byte
	Untracked  []File
}

// BuildInput is what Build packs.
type BuildInput struct {
	MoveID, SessionID, Machine, CWD string
	ClaudeDir                       string // ~/.claude, or $CLAUDE_CONFIG_DIR
	ClaudeVersion                   string
	Repo                            Repo
	Now                             time.Time
}

// FindTranscripts lists <claudeDir>/projects/*/<id>.jsonl.
func FindTranscripts(claudeDir, id string) ([]string, error) {
	if !sessionIDRE.MatchString(id) {
		return nil, fmt.Errorf("invalid session ID %q", id)
	}
	return filepath.Glob(filepath.Join(claudeDir, "projects", "*", id+".jsonl"))
}

// IsSecretName reports whether a file looks like it holds a secret: a base
// name that starts with .env or ends in .pem, .key, or .tfvars, in any case.
func IsSecretName(name string) bool {
	b := strings.ToLower(path.Base(filepath.ToSlash(name)))
	return strings.HasPrefix(b, ".env") || strings.HasSuffix(b, ".pem") ||
		strings.HasSuffix(b, ".key") || strings.HasSuffix(b, ".tfvars")
}

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

type entry struct {
	name string
	data []byte
}

// Build packs the session's transcript, sidecar folder, and file history,
// the repository's tracked changes against HEAD as a binary patch, and its
// untracked files, into a tar archive. Secret-looking files stay out of
// both and are listed in the manifest's Skipped. It fails when the transcript is missing or not
// unique, an untracked file passes 10 MiB, or the sealed bundle would pass
// 64 MiB, naming the files.
func Build(ctx context.Context, in BuildInput) ([]byte, Manifest, error) {
	m := Manifest{V: manifestVersion, MoveID: in.MoveID, SessionID: in.SessionID, SourceMachine: in.Machine,
		SourceCWD: in.CWD, RelPath: in.Repo.RelPath, RootRel: in.Repo.RootRel, Remote: in.Repo.Remote,
		Branch: in.Repo.Branch, Head: in.Repo.Head, ClaudeVersion: in.ClaudeVersion, Created: in.Now.UTC(),
		Files: []string{}, Untracked: []string{}, Skipped: []string{}}
	ts, err := FindTranscripts(in.ClaudeDir, in.SessionID)
	if err != nil {
		return nil, m, err
	}
	if len(ts) != 1 {
		return nil, m, fmt.Errorf("found %d transcripts for session %s under %s, want 1", len(ts), in.SessionID, in.ClaudeDir)
	}
	var entries []entry
	add := func(name string, data []byte) {
		entries = append(entries, entry{name, data})
		m.Files = append(m.Files, name)
	}
	data, err := os.ReadFile(ts[0])
	if err != nil {
		return nil, m, err
	}
	add("transcript/"+in.SessionID+".jsonl", data)
	for _, d := range []struct{ dir, prefix string }{
		{strings.TrimSuffix(ts[0], ".jsonl"), "transcript/" + in.SessionID + "/"},
		{filepath.Join(in.ClaudeDir, "file-history", in.SessionID), "file-history/" + in.SessionID + "/"},
	} {
		files, err := readTree(d.dir)
		if err != nil {
			return nil, m, err
		}
		for _, f := range files {
			add(d.prefix+f.Name, f.Data)
		}
	}
	patch, secret, err := trackedChanges(ctx, in.Repo.Root)
	if err != nil {
		return nil, m, err
	}
	add(entryPatch, patch)
	files, skipped, err := untrackedFiles(ctx, in.Repo.Root)
	if err != nil {
		return nil, m, err
	}
	m.Skipped = append(append(m.Skipped, secret...), skipped...)
	for _, f := range files {
		m.Untracked = append(m.Untracked, f.Name)
	}
	inner, err := tarFiles(files, in.Now)
	if err != nil {
		return nil, m, err
	}
	add(entryUntracked, inner)

	mj, err := json.Marshal(m)
	if err != nil {
		return nil, m, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writeEntry(tw, entryManifest, mj, 0o600, in.Now); err != nil {
		return nil, m, err
	}
	for _, e := range entries {
		if err := writeEntry(tw, e.name, e.data, 0o600, in.Now); err != nil {
			return nil, m, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, m, err
	}
	if size := int64(buf.Len()) + SealOverhead; size > maxSealed {
		return nil, m, fmt.Errorf("the bundle is %s, over the %s limit; largest files: %s",
			mib(size), mib(maxSealed), largest(entries, files, 5))
	}
	return buf.Bytes(), m, nil
}

func writeEntry(tw *tar.Writer, name string, data []byte, mode int64, now time.Time) error {
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(data)),
		ModTime: now.UTC(), Format: tar.FormatPAX}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// largest names the n biggest files, untracked ones by their repository
// path, as "name (size)".
func largest(entries []entry, untracked []File, n int) string {
	type sized struct {
		name string
		size int64
	}
	var all []sized
	for _, e := range entries {
		if e.name != entryUntracked {
			all = append(all, sized{e.name, int64(len(e.data))})
		}
	}
	for _, f := range untracked {
		all = append(all, sized{f.Name, int64(len(f.Data))})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].size > all[j].size })
	var out []string
	for i := 0; i < len(all) && i < n; i++ {
		out = append(out, fmt.Sprintf("%s (%s)", all[i].name, mib(all[i].size)))
	}
	return strings.Join(out, ", ")
}

// readTree reads every regular file under dir, in lexical order, with names
// relative to dir. A missing dir is empty. Links are skipped.
func readTree(dir string) ([]File, error) {
	var out []File
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p == dir {
			return filepath.SkipAll
		}
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, File{Name: filepath.ToSlash(rel), Data: data})
		return nil
	})
	return out, err
}

// untrackedFiles reads the repository's untracked, not ignored files.
// Secret-looking names, links, and anything that is not a regular file
// (a nested repository shows as "dir/") are skipped and returned by name.
func untrackedFiles(ctx context.Context, root string) ([]File, []string, error) {
	out, err := gitRaw(ctx, root, gitOpts{}, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, nil, err
	}
	var files []File
	skipped := []string{}
	var big []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" {
			continue
		}
		if IsSecretName(name) {
			skipped = append(skipped, name)
			continue
		}
		p := filepath.Join(root, filepath.FromSlash(name))
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, nil, err
		}
		if !fi.Mode().IsRegular() {
			skipped = append(skipped, name)
			continue
		}
		if fi.Size() > maxUntrackedFile {
			big = append(big, fmt.Sprintf("%s (%s)", name, mib(fi.Size())))
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, File{Name: name, Data: data, Exec: fi.Mode().Perm()&0o111 != 0})
	}
	if len(big) > 0 {
		return nil, nil, fmt.Errorf("untracked files over %s each: %s", mib(maxUntrackedFile), strings.Join(big, ", "))
	}
	return files, skipped, nil
}

// secretGlobs match the names IsSecretName matches, at any depth, as git
// pathspecs with the icase and glob magic.
var secretGlobs = []string{"**/.env*", "**/*.pem", "**/*.key", "**/*.tfvars"}

// secretPathspecs are pathspecs for secret-looking files: with exclude, the
// whole tree without them; else only them.
func secretPathspecs(exclude bool) []string {
	magic, out := ":(icase,glob)", []string{}
	if exclude {
		magic, out = ":(exclude,icase,glob)", []string{"."}
	}
	for _, g := range secretGlobs {
		out = append(out, magic+g)
	}
	return out
}

// diffArgs is a git diff command line that the user's diff config can't
// change: no color, no external diff or textconv, no rename detection, full
// object IDs (so git apply -R can reverse a binary hunk), and the a/ and b/
// prefixes.
func diffArgs(args ...string) []string {
	return append([]string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--full-index",
		"--src-prefix=a/", "--dst-prefix=b/"}, args...)
}

// changedSecrets lists the tracked secret-looking files that differ from
// HEAD, staged or not.
func changedSecrets(ctx context.Context, root string) ([]string, error) {
	out, err := gitRaw(ctx, root, gitOpts{}, append(diffArgs("--name-only", "-z", "HEAD", "--"), secretPathspecs(false)...)...)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Split(string(out), "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// trackedChanges is the binary patch of the tracked changes against HEAD,
// staged and unstaged, without secret-looking files, and the names of the
// secret-looking files it left out.
func trackedChanges(ctx context.Context, root string) ([]byte, []string, error) {
	secret, err := changedSecrets(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	patch, err := gitRaw(ctx, root, gitOpts{}, append(diffArgs("--binary", "HEAD", "--"), secretPathspecs(true)...)...)
	return patch, secret, err
}

// tarFiles packs untracked files with mode 0755 or 0644.
func tarFiles(files []File, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		mode := int64(0o644)
		if f.Exec {
			mode = 0o755
		}
		if err := writeEntry(tw, f.Name, f.Data, mode, now); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// safeName accepts a relative, clean, slash-separated name with no "." or
// ".." element.
func safeName(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// Extract reads and checks a bundle: manifest.json first, then only the
// entry names a move uses, each a regular file listed in the manifest, once;
// the manifest's list complete; the untracked files safe repository paths
// outside .git that match the manifest. It never touches the disk.
func Extract(data []byte) (*Bundle, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	b := &Bundle{}
	seen := map[string]bool{}
	first := true
	var untracked []byte
	haveTranscript, havePatch, haveUntracked := false, false, false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read bundle: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("bundle entry %q is not a regular file", h.Name)
		}
		if !safeName(h.Name) {
			return nil, fmt.Errorf("bundle entry %q has an unsafe name", h.Name)
		}
		if seen[h.Name] {
			return nil, fmt.Errorf("bundle entry %q appears twice", h.Name)
		}
		seen[h.Name] = true
		if h.Size < 0 || h.Size > maxSealed {
			return nil, fmt.Errorf("bundle entry %q has size %d", h.Name, h.Size)
		}
		body, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return nil, fmt.Errorf("read bundle entry %q: %w", h.Name, err)
		}
		if first {
			if h.Name != entryManifest {
				return nil, errors.New("bundle does not start with manifest.json")
			}
			if err := json.Unmarshal(body, &b.Manifest); err != nil {
				return nil, fmt.Errorf("bundle manifest: %w", err)
			}
			if b.Manifest.V != manifestVersion {
				return nil, fmt.Errorf("bundle manifest version %d, want %d", b.Manifest.V, manifestVersion)
			}
			if !sessionIDRE.MatchString(b.Manifest.SessionID) {
				return nil, fmt.Errorf("bundle manifest has session ID %q", b.Manifest.SessionID)
			}
			first = false
			continue
		}
		id := b.Manifest.SessionID
		switch name := h.Name; {
		case name == "transcript/"+id+".jsonl":
			b.Transcript, haveTranscript = body, true
		case strings.HasPrefix(name, "transcript/"+id+"/"):
			b.Sidecar = append(b.Sidecar, File{Name: strings.TrimPrefix(name, "transcript/"+id+"/"), Data: body})
		case strings.HasPrefix(name, "file-history/"+id+"/"):
			b.History = append(b.History, File{Name: strings.TrimPrefix(name, "file-history/"+id+"/"), Data: body})
		case name == entryPatch:
			b.Patch, havePatch = body, true
		case name == entryUntracked:
			untracked, haveUntracked = body, true
		default:
			return nil, fmt.Errorf("bundle entry %q is not part of a move", name)
		}
	}
	switch {
	case first:
		return nil, errors.New("bundle is empty")
	case !haveTranscript || !havePatch || !haveUntracked:
		return nil, errors.New("bundle lacks the transcript, the patch, or the untracked files")
	}
	delete(seen, entryManifest)
	if !sameSet(seen, b.Manifest.Files) {
		return nil, errors.New("bundle entries do not match its manifest")
	}
	files, err := readUntracked(untracked)
	if err != nil {
		return nil, err
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f.Name] = true
	}
	if !sameSet(got, b.Manifest.Untracked) {
		return nil, errors.New("bundle's untracked files do not match its manifest")
	}
	b.Untracked = files
	return b, nil
}

// sameSet reports whether the keys of have are exactly list, each once.
func sameSet(have map[string]bool, list []string) bool {
	if len(have) != len(list) {
		return false
	}
	for _, n := range list {
		if !have[n] {
			return false
		}
	}
	return len(slices.Compact(slices.Sorted(slices.Values(list)))) == len(list)
}

// readUntracked reads git/untracked.tar: regular files with safe names
// outside .git, each once.
func readUntracked(data []byte) ([]File, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	var out []File
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read untracked files: %w", err)
		}
		bad := h.Typeflag != tar.TypeReg || !safeName(h.Name) || seen[h.Name] || h.Size < 0 || h.Size > maxUntrackedFile
		for _, part := range strings.Split(h.Name, "/") {
			if strings.EqualFold(part, ".git") {
				bad = true
			}
		}
		if bad {
			return nil, fmt.Errorf("untracked file %q is not allowed", h.Name)
		}
		seen[h.Name] = true
		body, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return nil, err
		}
		out = append(out, File{Name: h.Name, Data: body, Exec: h.Mode&0o111 != 0})
	}
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/move/ -count=1`
Expected: PASS. `TestBuildAndExtract` applies the extracted patch to a
fresh clone and gets the source's files back byte for byte;
`TestBuildIgnoresDiffConfig` applies and reverses it under a hostile diff
config.

- [ ] **Step 8: Document the bundle**

In `docs/client.md`, at the end of section "Moves", add:

```markdown
- **Repository.** `ReadRepo(ctx, cwd, roots)` reads the work tree's top
  level, `origin`'s URL, the checked-out branch (a detached HEAD fails), the
  HEAD SHA, the session directory relative to the top level, and the top
  level relative to the search root that holds it (`DefaultRoots`: `~/Code`
  plus the client config's `move_roots`). Every Git call runs with
  `GIT_TERMINAL_PROMPT=0`, SSH in `BatchMode`, and a timeout (30 s, 2 min for
  `push` and `fetch`). `NormalizeRemote` compares remotes without scheme,
  user, `.git`, or host case.
- **Bundle.** `Build` writes a tar archive: `manifest.json` first, then
  `transcript/<id>.jsonl`, the sidecar folder `transcript/<id>/...`,
  `file-history/<id>/...`, `git/changes.patch` (`git diff --binary HEAD`:
  staged and unstaged changes, binary files, deletions), and
  `git/untracked.tar` (`git ls-files --others --exclude-standard`, with the
  executable bit). The diff runs with `--no-color --no-ext-diff
  --no-textconv --no-renames --full-index --src-prefix=a/ --dst-prefix=b/`,
  so the user's diff config can't change it and `git apply -R` can reverse
  it. Secret-looking names (`.env*`, `*.pem`, `*.key`, `*.tfvars`) are not
  carried, tracked or untracked: the diff leaves them out with
  `:(exclude,icase,glob)` pathspecs. Those, links, and nested repositories
  are listed in the manifest under `skipped`. An untracked file over 10 MiB, or a sealed
  bundle over 64 MiB, fails the move with the file names. `Extract` refuses
  anything else: another entry name, `..`, an absolute path, a link, a
  duplicate, an entry the manifest does not list, or an untracked path inside
  `.git`.
```

- [ ] **Step 9: Lint and commit**

Run: `gofmt -w internal/move/*.go && make lint`
Expected: clean.

```bash
git add internal/move/git.go internal/move/bundle.go internal/move/helpers_test.go \
  internal/move/git_test.go internal/move/bundle_test.go docs/client.md
git commit -m "$(cat <<'EOF'
Build and check move bundles

A bundle is a tar archive of the transcript, its sidecar folder, the file
history, git diff --binary HEAD, and the untracked files, with a manifest
that lists every entry. The diff ignores the user's diff config.
Secret-looking files, tracked or untracked, and links stay behind and are
named in the manifest. Extract refuses unknown names, path escapes, links,
duplicates, and anything the manifest does not list.

Refs: docs/dev/superpowers/plans/2026-10-03-move-session.md, Task 2

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 3: Store: schema v9 and moves

**Files:**
- Modify: `internal/store/store.go` (`schemaVersion = 9`, `schemaV9`,
  migration step)
- Create: `internal/store/moves.go`
- Create: `internal/store/moves_test.go`
- Modify: `internal/store/read.go` (`sessionSelect` and `scanSession` carry
  the latest move)
- Modify: `internal/store/machines.go` (`ListMachines` reads `move_key` and
  `last_poll`)
- Modify: `internal/store/concurrency_test.go` (`rollbackV9`, called first by
  `rollbackV8`)
- Modify: `internal/store/store_test.go` (`user_version` `9`, the new table,
  column, and indexes)
- Modify: `docs/server.md` (Database section)

**Interfaces:**
- Consumes: `api` move types (Task 1), `move.IsGitHub` (Task 2),
  `newRandomID`, `insertEventTx`, `closePermissionsTx`, `checkText`,
  `invalidf`, `ValidRemoteControlURL`, `ControlPollWindow`, `newControlEnv`.
- Produces: the `internal/store/moves.go` functions in the interface ledger,
  and test helpers `moveKey(t) string` and `(*controlEnv).moveReady(id string)`.

- [ ] **Step 1: Write the failing schema tests**

In `internal/store/store_test.go` (`TestOpenPragmasAndSchema`), replace
`{"user_version", "8"},` with `{"user_version", "9"},`, replace:

```go
		"instructions", "messages", "permission_requests"} {
```

with:

```go
		"instructions", "messages", "permission_requests", "moves"} {
```

replace:

```go
		"machines":            "herdr_host id last_poll last_seen name ssh_host token_hash",
```

with:

```go
		"machines":            "herdr_host id last_poll last_seen move_key name ssh_host token_hash",
		"moves":               "bundle_size cloud_url created_at detail id requested_by session_id source_closed_at source_machine_id state target target_machine_id updated_at",
```

and replace:

```go
	for _, idx := range []string{"messages_machine", "messages_limit", "messages_session", "permission_requests_session"} {
```

with:

```go
	for _, idx := range []string{"messages_machine", "messages_limit", "messages_session", "permission_requests_session",
		"moves_session", "moves_source", "moves_target"} {
```

In `internal/store/concurrency_test.go`, replace:

```go
func rollbackV8(t *testing.T, s *Store) {
	t.Helper()
	for _, q := range []string{"DROP TABLE instructions", "DROP TABLE messages", "DROP TABLE permission_requests"} {
```

with:

```go
func rollbackV8(t *testing.T, s *Store) {
	t.Helper()
	rollbackV9(t, s)
	for _, q := range []string{"DROP TABLE instructions", "DROP TABLE messages", "DROP TABLE permission_requests"} {
```

and add after the `rollbackV8` function:

```go
// rollbackV9 removes what schema v9 added, so a test can set user_version
// to 8 or lower and reopen the file as an older release left it.
func rollbackV9(t *testing.T, s *Store) {
	t.Helper()
	for _, q := range []string{"DROP TABLE moves", "ALTER TABLE machines DROP COLUMN move_key"} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}
```

- [ ] **Step 2: Write the failing move tests**

Create `internal/store/moves_test.go`:

```go
package store

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// moveKey is a fresh X25519 public key in the server's form.
func moveKey(t *testing.T) string {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
}

// moveReady registers id on tower in a herdr pane with an idle agent and a
// GitHub remote, gives both machines a key, and records a poll from both.
func (e *controlEnv) moveReady(id string) (towerKey, bluKey string) {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: id, Agent: "claude", Source: api.SourcePlugin,
		CWD: "/home/user/proj", GitRepo: "git@github.com:o/r.git", GitBranch: "main", HerdrSession: "default",
		HerdrWorkspace: "w1", HerdrPane: "w1:p1", AgentState: "idle"}); err != nil {
		e.t.Fatal(err)
	}
	towerKey, bluKey = moveKey(e.t), moveKey(e.t)
	if err := e.s.SetMoveKey(ctx, e.tower.ID, towerKey); err != nil {
		e.t.Fatal(err)
	}
	if err := e.s.SetMoveKey(ctx, e.bluebox.ID, bluKey); err != nil {
		e.t.Fatal(err)
	}
	e.poll(e.tower)
	e.poll(e.bluebox)
	return towerKey, bluKey
}

const moveSID = "3f2a9c10-1111-4a4a-8b8b-00000000aaaa"

func TestMigrateV8ToV9(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	if _, _, err := s.AddMachine(ctx, "tower", "", ""); err != nil {
		t.Fatal(err)
	}
	rollbackV9(t, s)
	if _, err := s.db.Exec("PRAGMA user_version = 8"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var v int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version %d %v, want %d", v, err, schemaVersion)
	}
	ms, err := s2.ListMachines(ctx)
	if err != nil || len(ms) != 1 || ms[0].MoveKey != "" || ms[0].MoveReady {
		t.Fatalf("machines after upgrade: %+v %v", ms, err)
	}
	var id int64
	s2.db.QueryRow("SELECT id FROM machines").Scan(&id)
	if err := s2.SetMoveKey(ctx, id, moveKey(t)); err != nil {
		t.Errorf("SetMoveKey after upgrade: %v", err)
	}
}

func TestSetMoveKeyAndMachines(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	if err := e.s.SetMoveKey(ctx, e.tower.ID, "nope"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad key: %v", err)
	}
	key := moveKey(t)
	if err := e.s.SetMoveKey(ctx, e.tower.ID, key); err != nil {
		t.Fatal(err)
	}
	e.poll(e.tower)
	raw, _ := api.ParseMoveKey(key)
	ms, _ := e.s.ListMachines(ctx)
	if ms[0].Name != "bluebox" || ms[0].MoveKey != "" || ms[0].MoveReady || ms[1].MoveKey != api.MoveKeyFingerprint(raw) || !ms[1].MoveReady {
		t.Fatalf("machines: %+v", ms)
	}
	e.clock.Advance(ControlPollWindow + time.Second)
	if ms, _ := e.s.ListMachines(ctx); ms[1].MoveReady {
		t.Error("tower is move-ready without a recent poll")
	}
}

func TestCreateMoveRules(t *testing.T) {
	ctx := context.Background()
	refused := func(t *testing.T, e *controlEnv, target, want string) {
		t.Helper()
		before := e.count("SELECT COUNT(*) FROM moves")
		_, err := e.s.CreateMove(ctx, moveSID, target, "machine:tower")
		if !errors.Is(err, ErrNotControllable) || !strings.Contains(err.Error(), want) {
			t.Errorf("target %s: %v, want 409 with %q", target, err, want)
		}
		if n := e.count("SELECT COUNT(*) FROM moves"); n != before {
			t.Errorf("a refused move was stored: %d moves, %d before", n, before)
		}
	}
	cases := []struct {
		name, target, want string
		setup              func(e *controlEnv)
	}{
		{"ended", "bluebox", "has ended", func(e *controlEnv) {
			e.s.AddEvent(ctx, e.tower.ID, moveSID, api.EventIn{Kind: api.KindEnded, Source: api.SourcePlugin})
		}},
		{"no pane", "bluebox", "not in herdr", func(e *controlEnv) {
			e.s.db.Exec("UPDATE sessions SET herdr_pane = '' WHERE id = ?", moveSID)
		}},
		{"working", "bluebox", "the agent is working", func(e *controlEnv) {
			e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: moveSID, Source: api.SourcePlugin, AgentState: "working"})
		}},
		{"blocked", "bluebox", "the agent is blocked", func(e *controlEnv) {
			e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: moveSID, Source: api.SourcePlugin, AgentState: "blocked"})
		}},
		{"source offline", "bluebox", `watcher on "tower" is offline`, func(e *controlEnv) {
			e.clock.Advance(ControlPollWindow + time.Second)
			e.poll(e.bluebox)
		}},
		{"source has no key", "bluebox", `"tower" has no move key`, func(e *controlEnv) {
			e.s.db.Exec("UPDATE machines SET move_key = '' WHERE id = ?", e.tower.ID)
		}},
		{"unknown target", "nosuch", `no machine named "nosuch"`, nil},
		{"same machine", "tower", `already on "tower"`, nil},
		{"target offline", "bluebox", `watcher on "bluebox" is offline`, func(e *controlEnv) {
			e.clock.Advance(ControlPollWindow + time.Second)
			e.poll(e.tower)
		}},
		{"target has no key", "bluebox", `"bluebox" has no move key`, func(e *controlEnv) {
			e.s.db.Exec("UPDATE machines SET move_key = '' WHERE id = ?", e.bluebox.ID)
		}},
		{"cloud without GitHub", api.MoveCloud, "GitHub remote", func(e *controlEnv) {
			e.s.db.Exec("UPDATE sessions SET git_repo = 'git@gitlab.com:o/r.git' WHERE id = ?", moveSID)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newControlEnv(t)
			e.moveReady(moveSID)
			if c.setup != nil {
				c.setup(e)
			}
			refused(t, e, c.target, c.want)
		})
	}

	e := newControlEnv(t)
	e.moveReady(moveSID)
	if _, err := e.s.CreateMove(ctx, "nosuch-session", "bluebox", "machine:tower"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}
	if _, err := e.s.CreateMove(ctx, moveSID, "bad name!", "machine:tower"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad target: %v", err)
	}
	if _, err := e.s.CreateMove(ctx, moveSID, "bluebox", "someone"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad by: %v", err)
	}
	mv, err := e.s.CreateMove(ctx, moveSID, "bluebox", "web:phone")
	if err != nil {
		t.Fatal(err)
	}
	if !ValidMoveID(mv.ID) || mv.State != api.MoveRequested || mv.Source != "tower" || mv.Target != "bluebox" ||
		mv.By != "web:phone" || !mv.CreatedAt.Equal(e.clock.Now()) {
		t.Errorf("move %+v", mv)
	}
	if got := e.get(moveSID).Move; got == nil || got.ID != mv.ID || got.State != api.MoveRequested {
		t.Errorf("session's move: %+v", got)
	}
	refused(t, e, "bluebox", "move "+mv.ID+" of this session is requested")
	if cm, err := e.s.CreateMove(ctx, moveSID, api.MoveCloud, "machine:bluebox"); err == nil {
		t.Errorf("a cloud move beside an open one: %+v", cm)
	}
}

func TestClaimMoveFlow(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	towerKey, bluKey := e.moveReady(moveSID)
	mv, err := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := e.s.ClaimMove(ctx, e.bluebox.ID); ok || err != nil {
		t.Fatalf("the target claimed a requested move: %v %v", ok, err)
	}
	out, ok, err := e.s.ClaimMove(ctx, e.tower.ID)
	if err != nil || !ok {
		t.Fatalf("source claim: %v %v", ok, err)
	}
	if out.Request.Action != api.ActionMoveOut || out.Request.ID != mv.ID || out.Request.Machine != "tower" ||
		out.Session.ID != moveSID || out.Move == nil || out.Move.State != api.MovePacking || out.Move.PeerKey != bluKey ||
		out.Move.Source != "tower" || out.Move.Target != "bluebox" {
		t.Fatalf("move-out claim %+v move %+v", out.Request, out.Move)
	}
	if _, ok, _ := e.s.ClaimMove(ctx, e.tower.ID); ok {
		t.Fatal("a packing move was handed out twice")
	}

	if _, err := e.s.MoveForUpload(ctx, e.bluebox.ID, mv.ID); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("target upload check: %v", err)
	}
	if _, err := e.s.MoveForUpload(ctx, e.tower.ID, mv.ID); err != nil {
		t.Errorf("source upload check: %v", err)
	}
	if _, err := e.s.MoveUploaded(ctx, e.tower.ID, mv.ID, 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty bundle: %v", err)
	}
	if _, err := e.s.MoveUploaded(ctx, e.tower.ID, mv.ID, api.MaxSealedBundle+1); !errors.Is(err, ErrInvalid) {
		t.Errorf("huge bundle: %v", err)
	}
	up, err := e.s.MoveUploaded(ctx, e.tower.ID, mv.ID, 1234)
	if err != nil || up.State != api.MoveUploaded || up.BundleSize != 1234 {
		t.Fatalf("uploaded: %+v %v", up, err)
	}
	if _, err := e.s.MoveUploaded(ctx, e.tower.ID, mv.ID, 1234); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("second upload (the target's step now): %v", err)
	}
	if _, err := e.s.MoveForDownload(ctx, e.bluebox.ID, mv.ID); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("download before the claim: %v", err)
	}

	in, ok, err := e.s.ClaimMove(ctx, e.bluebox.ID)
	if err != nil || !ok || in.Request.Action != api.ActionMoveIn || in.Request.Machine != "bluebox" || in.Move.State != api.MoveUnpacking ||
		in.Move.PeerKey != towerKey {
		t.Fatalf("move-in claim: %v %v %+v %+v", ok, err, in.Request, in.Move)
	}
	if _, err := e.s.MoveForDownload(ctx, e.tower.ID, mv.ID); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("source download: %v", err)
	}
	if _, err := e.s.MoveForDownload(ctx, e.bluebox.ID, mv.ID); err != nil {
		t.Errorf("target download: %v", err)
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveDone}); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("source result while unpacking: %v", err)
	}
	if _, err := e.s.FinishMove(ctx, e.bluebox.ID, mv.ID, api.MoveResultIn{State: api.MoveDone, CloudURL: "https://claude.ai/code/session_x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("machine move with a cloud link: %v", err)
	}
	done, err := e.s.FinishMove(ctx, e.bluebox.ID, mv.ID, api.MoveResultIn{State: api.MoveDone})
	if err != nil || done.State != api.MoveDone {
		t.Fatalf("done: %+v %v", done, err)
	}
	s := e.get(moveSID)
	if s.Machine != "bluebox" || s.HerdrPane != "" || s.HerdrSession != "" || s.HerdrWorkspace != "" || s.Move.State != api.MoveDone {
		t.Errorf("session after the move: %+v", s)
	}
	ev := e.events(moveSID, api.KindMoved)
	var p map[string]string
	if len(ev) != 1 || json.Unmarshal(ev[0].Payload, &p) != nil || p["move_id"] != mv.ID || p["from"] != "tower" || p["to"] != "bluebox" {
		t.Errorf("moved event: %+v", ev)
	}
	// The new owner's writes work; the old owner's do not.
	if _, err := e.s.UpsertSession(ctx, e.bluebox.ID, api.SessionUpsert{ID: moveSID, Source: api.SourcePlugin, HerdrPane: "w2:p1"}); err != nil {
		t.Errorf("target upsert: %v", err)
	}
	if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: moveSID, Source: api.SourcePlugin}); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("source upsert after the move: %v", err)
	}
}

func TestClaimMoveFinishOnce(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	mv, _ := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower")
	e.s.ClaimMove(ctx, e.tower.ID)
	e.s.MoveUploaded(ctx, e.tower.ID, mv.ID, 10)
	e.s.ClaimMove(ctx, e.bluebox.ID)
	if _, err := e.s.FinishMove(ctx, e.bluebox.ID, mv.ID, api.MoveResultIn{State: api.MoveFailed, Detail: "find clone: no clone of github.com/o/r on bluebox"}); err != nil {
		t.Fatal(err)
	}
	if s := e.get(moveSID); s.Machine != "tower" || s.HerdrPane != "w1:p1" {
		t.Errorf("a failed move changed the session: %+v", s)
	}
	if _, ok, _ := e.s.ClaimMove(ctx, e.bluebox.ID); ok {
		t.Error("the target got a finish")
	}
	fin, ok, err := e.s.ClaimMove(ctx, e.tower.ID)
	if err != nil || !ok || fin.Request.Action != api.ActionMoveOut || fin.Move.State != api.MoveFailed ||
		!strings.HasPrefix(fin.Move.Detail, "find clone:") || fin.Move.PeerKey != "" {
		t.Fatalf("finish: %v %v %+v", ok, err, fin.Move)
	}
	if _, ok, _ := e.s.ClaimMove(ctx, e.tower.ID); ok {
		t.Error("the finish was handed out twice")
	}
	if n := e.count("SELECT COUNT(*) FROM moves WHERE source_closed_at IS NOT NULL"); n != 1 {
		t.Errorf("source_closed_at set on %d moves", n)
	}
}

func TestFinishMoveBySource(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	mv, _ := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower")
	e.s.ClaimMove(ctx, e.tower.ID)
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveDone}); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("source done on a machine move: %v", err)
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: "uploaded"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("state uploaded through the result route: %v", err)
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveFailed, Detail: "line\nbreak"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("detail with a newline: %v", err)
	}
	f, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveFailed, Detail: "check pane: the agent is working"})
	if err != nil || f.State != api.MoveFailed {
		t.Fatalf("failed: %+v %v", f, err)
	}
	if _, ok, _ := e.s.ClaimMove(ctx, e.tower.ID); ok {
		t.Error("a source that failed its own step got a finish")
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveFailed}); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("second result: %v", err)
	}
}

// TestFinishMoveSourceNote: once a move ended and the source's part closed,
// the source may add a note to the detail (a restart or an archive that
// failed), and nothing else changes.
func TestFinishMoveSourceNote(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	mv, _ := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower")
	e.s.ClaimMove(ctx, e.tower.ID)
	e.s.MoveUploaded(ctx, e.tower.ID, mv.ID, 10)
	e.s.ClaimMove(ctx, e.bluebox.ID)
	note := api.MoveResultIn{State: api.MoveDone, Detail: "archive on tower failed: disk full"}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, note); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("a note while the target unpacks: %v", err)
	}
	if _, err := e.s.FinishMove(ctx, e.bluebox.ID, mv.ID, api.MoveResultIn{State: api.MoveDone, Detail: "not carried: .env"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, note); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("a note before the finish was claimed: %v", err)
	}
	if _, ok, _ := e.s.ClaimMove(ctx, e.tower.ID); !ok {
		t.Fatal("no finish")
	}
	for _, bad := range []api.MoveResultIn{{State: api.MoveFailed, Detail: "restart failed: x"}, {State: api.MoveDone}} {
		if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, bad); !errors.Is(err, ErrRequestClosed) {
			t.Errorf("note %+v: %v", bad, err)
		}
	}
	if _, err := e.s.FinishMove(ctx, e.bluebox.ID, mv.ID, note); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("the target added a note: %v", err)
	}
	got, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, note)
	if err != nil || got.State != api.MoveDone || got.Detail != "not carried: .env; archive on tower failed: disk full" {
		t.Fatalf("note: %+v %v", got, err)
	}
	if s := e.get(moveSID); s.Machine != "bluebox" {
		t.Errorf("a note moved the session: %+v", s)
	}
	if moved := e.events(moveSID, api.KindMoved); len(moved) != 1 {
		t.Errorf("a note recorded another moved event: %+v", moved)
	}
	long := api.MoveResultIn{State: api.MoveDone, Detail: strings.Repeat("x", api.MaxMoveDetailRunes)}
	if got, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, long); err != nil || len([]rune(got.Detail)) != api.MaxMoveDetailRunes {
		t.Errorf("long note: %d runes, %v", len([]rune(got.Detail)), err)
	}
}

func TestCloudMoveDone(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	e.s.db.Exec("UPDATE machines SET move_key = '' WHERE id = ?", e.bluebox.ID) // a cloud move needs no target key
	mv, err := e.s.CreateMove(ctx, moveSID, api.MoveCloud, "machine:tower")
	if err != nil {
		t.Fatal(err)
	}
	out, ok, _ := e.s.ClaimMove(ctx, e.tower.ID)
	if !ok || out.Move.Target != api.MoveCloud || out.Move.PeerKey != "" {
		t.Fatalf("cloud claim %+v", out.Move)
	}
	if _, err := e.s.MoveUploaded(ctx, e.tower.ID, mv.ID, 10); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("a cloud move took a bundle: %v", err)
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveDone}); !errors.Is(err, ErrInvalid) {
		t.Errorf("cloud done without a link: %v", err)
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveDone, CloudURL: "https://evil.test/x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("cloud done with a foreign link: %v", err)
	}
	const link = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"
	d, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveDone, CloudURL: link})
	if err != nil || d.State != api.MoveDone || d.CloudURL != link {
		t.Fatalf("cloud done: %+v %v", d, err)
	}
	s := e.get(moveSID)
	if s.Status != api.StatusEnded || s.Machine != "tower" || s.Move.CloudURL != link {
		t.Errorf("session after a cloud move: %+v", s)
	}
	ended := e.events(moveSID, api.KindEnded)
	if len(ended) != 1 || !strings.Contains(string(ended[0].Payload), `"reason":"moved_to_cloud"`) {
		t.Errorf("ended events: %+v", ended)
	}
	if moved := e.events(moveSID, api.KindMoved); len(moved) != 1 || !strings.Contains(string(moved[0].Payload), link) {
		t.Errorf("moved events: %+v", moved)
	}
	if _, ok, _ := e.s.ClaimMove(ctx, e.tower.ID); ok {
		t.Error("a cloud move the source finished itself got a finish")
	}
}

func TestExpireMoves(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	mv, _ := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower")
	e.clock.Advance(MoveRequestTTL - time.Second)
	if got, _ := e.s.ExpireMoves(ctx); len(got) != 0 {
		t.Fatalf("expired early: %+v", got)
	}
	e.clock.Advance(time.Second)
	got, err := e.s.ExpireMoves(ctx)
	if err != nil || len(got) != 1 || got[0].ID != mv.ID || got[0].State != api.MoveFailed || got[0].Detail != "requested: timed out" {
		t.Fatalf("expire requested: %+v %v", got, err)
	}
	if _, ok, _ := e.s.ClaimMove(ctx, e.tower.ID); ok {
		t.Error("a move the source never claimed got a finish")
	}

	// A move stuck in packing fails after MoveStepTTL, and the source gets
	// a finish, because it may have ended the session.
	e.poll(e.tower)
	e.poll(e.bluebox)
	mv2, err := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower")
	if err != nil {
		t.Fatal(err)
	}
	e.s.ClaimMove(ctx, e.tower.ID)
	e.clock.Advance(MoveStepTTL)
	if got, _ := e.s.ExpireMoves(ctx); len(got) != 1 || got[0].ID != mv2.ID || got[0].Detail != "packing: timed out" {
		t.Fatalf("expire packing: %+v", got)
	}
	fin, ok, _ := e.s.ClaimMove(ctx, e.tower.ID)
	if !ok || fin.Move.ID != mv2.ID || fin.Move.State != api.MoveFailed {
		t.Errorf("finish after a timeout: %v %+v", ok, fin.Move)
	}
	if g, err := e.s.GetMove(ctx, mv2.ID); err != nil || g.State != api.MoveFailed {
		t.Errorf("GetMove: %+v %v", g, err)
	}
	if _, err := e.s.GetMove(ctx, "mv_nosuchnosuchnosuchnosu"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown move: %v", err)
	}
	if _, err := e.s.GetMove(ctx, "x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad move id: %v", err)
	}
}
```

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/store/ -count=1`
Expected: FAIL to compile with `e.s.SetMoveKey undefined`,
`e.s.CreateMove undefined`, and the other move methods.

- [ ] **Step 4: Add schema v9**

In `internal/store/store.go`, replace `const schemaVersion = 8` with
`const schemaVersion = 9`, add after the `schemaV8` constant:

```go
// schemaV9 adds moves of a session to another machine or to the cloud, and
// each machine's move public key. A move goes with its session and with
// either machine. source_closed_at is NULL while the source still owes its
// finish (archive on done, restart otherwise).
const schemaV9 = `
ALTER TABLE machines ADD COLUMN move_key TEXT NOT NULL DEFAULT '';  -- X25519 public key, base64
CREATE TABLE moves (
	id                TEXT PRIMARY KEY,   -- mv_ + 16 random bytes, base64url
	session_id        TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	source_machine_id INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	target            TEXT NOT NULL,      -- a machine name, or cloud
	target_machine_id INTEGER REFERENCES machines(id) ON DELETE CASCADE, -- NULL for cloud
	state             TEXT NOT NULL,      -- requested|packing|uploaded|unpacking|done|failed|cancelled
	detail            TEXT NOT NULL DEFAULT '',
	created_at        TEXT NOT NULL,
	updated_at        TEXT NOT NULL,
	bundle_size       INTEGER NOT NULL DEFAULT 0,
	cloud_url         TEXT NOT NULL DEFAULT '',
	requested_by      TEXT NOT NULL,      -- web:<name> | machine:<name>
	source_closed_at  TEXT
);
CREATE INDEX moves_session ON moves(session_id, created_at);
CREATE INDEX moves_source ON moves(source_machine_id, state);
CREATE INDEX moves_target ON moves(target_machine_id, state);
`
```

and replace:

```go
	if v < 8 {
		if _, err := tx.ExecContext(ctx, schemaV8); err != nil {
			return fmt.Errorf("migrate schema to v8: %w", err)
		}
	}
```

with:

```go
	if v < 8 {
		if _, err := tx.ExecContext(ctx, schemaV8); err != nil {
			return fmt.Errorf("migrate schema to v8: %w", err)
		}
	}
	if v < 9 {
		if _, err := tx.ExecContext(ctx, schemaV9); err != nil {
			return fmt.Errorf("migrate schema to v9: %w", err)
		}
	}
```

- [ ] **Step 5: Write the move store**

Create `internal/store/moves.go`:

```go
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/move"
)

// Move timeouts. See docs/server.md, "Moves".
const (
	// MoveRequestTTL is how long a move waits for its source to claim it.
	MoveRequestTTL = 2 * time.Minute
	// MoveStepTTL is how long a move may stay packing, uploaded, or
	// unpacking since its last change.
	MoveStepTTL = 10 * time.Minute
	maxByRunes  = 100
)

var moveIDRE = regexp.MustCompile(`^mv_[A-Za-z0-9_-]{22}$`)

// sourceNoteTx appends in.Detail to the detail of move id when machineID is
// its source, the move ended in in.State, and the source's part is closed.
// It is how the source reports a problem after that, such as a restart or an
// archive that failed. It reports whether it wrote.
func sourceNoteTx(ctx context.Context, tx *sql.Tx, machineID int64, id string, in api.MoveResultIn, nowS string) (bool, error) {
	if in.Detail == "" || in.CloudURL != "" {
		return false, nil
	}
	var detail string
	err := tx.QueryRowContext(ctx, `SELECT detail FROM moves WHERE id = ? AND source_machine_id = ? AND state = ?
		AND source_closed_at IS NOT NULL`, id, machineID, in.State).Scan(&detail)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if detail != "" {
		detail += "; "
	}
	detail += in.Detail
	if r := []rune(detail); len(r) > api.MaxMoveDetailRunes {
		detail = string(r[:api.MaxMoveDetailRunes])
	}
	if _, err := tx.ExecContext(ctx, `UPDATE moves SET detail = ?, updated_at = ? WHERE id = ?`, detail, nowS, id); err != nil {
		return false, err
	}
	return true, nil
}

// ValidMoveID reports whether id has the shape of a move ID.
func ValidMoveID(id string) bool { return moveIDRE.MatchString(id) }

func checkMoveID(id string) error {
	if !ValidMoveID(id) {
		return invalidf("move id %q: want mv_ and 22 base64url characters", id)
	}
	return nil
}

// refusef is a 409 with a reason a person can act on.
func refusef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotControllable, fmt.Sprintf(format, args...))
}

// SetMoveKey stores machineID's move public key, replacing any earlier one.
func (s *Store) SetMoveKey(ctx context.Context, machineID int64, key string) error {
	if _, err := api.ParseMoveKey(key); err != nil {
		return invalidf("%v", err)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE machines SET move_key = ? WHERE id = ?`, key, machineID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: machine %d", ErrNotFound, machineID)
	}
	return nil
}

// moveSelect reads a move and its source machine's name.
const moveSelect = `SELECT mv.id, mv.session_id, ms.name, mv.target, mv.state, mv.detail, mv.cloud_url,
	mv.requested_by, mv.bundle_size, mv.created_at, mv.updated_at
FROM moves mv JOIN machines ms ON ms.id = mv.source_machine_id`

func scanMove(sc scanner) (api.Move, error) {
	var m api.Move
	var created, updated string
	if err := sc.Scan(&m.ID, &m.SessionID, &m.Source, &m.Target, &m.State, &m.Detail, &m.CloudURL,
		&m.By, &m.BundleSize, &created, &updated); err != nil {
		return api.Move{}, err
	}
	var err error
	if m.CreatedAt, err = parseTS(created); err != nil {
		return api.Move{}, err
	}
	if m.UpdatedAt, err = parseTS(updated); err != nil {
		return api.Move{}, err
	}
	return m, nil
}

// nullMove is a moves row read through a LEFT JOIN.
type nullMove struct {
	id, target, state, detail, cloudURL, by, created, updated, source sql.NullString
	size                                                              sql.NullInt64
}

// move returns the row as a move, or nil when the join found none.
func (n nullMove) move(sessionID string) (*api.Move, error) {
	if !n.id.Valid {
		return nil, nil
	}
	m := api.Move{ID: n.id.String, SessionID: sessionID, Source: n.source.String, Target: n.target.String,
		State: n.state.String, Detail: n.detail.String, CloudURL: n.cloudURL.String, By: n.by.String, BundleSize: n.size.Int64}
	var err error
	if m.CreatedAt, err = parseTS(n.created.String); err != nil {
		return nil, err
	}
	if m.UpdatedAt, err = parseTS(n.updated.String); err != nil {
		return nil, err
	}
	return &m, nil
}

// polled reports whether a machine's last poll is within ControlPollWindow.
func polled(lastPoll sql.NullString, now time.Time) (bool, error) {
	t, err := parseNullTS(lastPoll)
	if err != nil || t == nil {
		return false, err
	}
	return now.Sub(*t) <= ControlPollWindow, nil
}

// expireMovesTx fails every move past its timeout: requested for
// MoveRequestTTL (the source never acted, so its part is closed too), or
// packing, uploaded, or unpacking for MoveStepTTL since its last change. It
// returns the IDs it failed.
func expireMovesTx(ctx context.Context, tx *sql.Tx, now time.Time) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, state FROM moves
		WHERE (state = ? AND updated_at <= ?) OR (state IN (?, ?, ?) AND updated_at <= ?)`,
		api.MoveRequested, formatTS(now.Add(-MoveRequestTTL)),
		api.MovePacking, api.MoveUploaded, api.MoveUnpacking, formatTS(now.Add(-MoveStepTTL)))
	if err != nil {
		return nil, err
	}
	type old struct{ id, state string }
	var list []old
	for rows.Next() {
		var o old
		if err := rows.Scan(&o.id, &o.state); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	nowS := formatTS(now)
	var ids []string
	for _, o := range list {
		closed := sql.NullString{}
		if o.state == api.MoveRequested {
			closed = sql.NullString{String: nowS, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE moves SET state = ?, detail = ?, updated_at = ?,
			source_closed_at = COALESCE(source_closed_at, ?) WHERE id = ?`,
			api.MoveFailed, o.state+": timed out", nowS, closed, o.id); err != nil {
			return nil, err
		}
		ids = append(ids, o.id)
	}
	return ids, nil
}

// ExpireMoves fails every move past its timeout and returns them as they
// are now. The server deletes their bundles and wakes their sources.
func (s *Store) ExpireMoves(ctx context.Context) ([]api.Move, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids, err := expireMovesTx(ctx, tx, s.Now())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	var out []api.Move
	for _, id := range ids {
		m, err := s.GetMove(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// GetMove reads one move.
func (s *Store) GetMove(ctx context.Context, id string) (api.Move, error) {
	if err := checkMoveID(id); err != nil {
		return api.Move{}, err
	}
	m, err := scanMove(s.db.QueryRowContext(ctx, moveSelect+` WHERE mv.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return api.Move{}, fmt.Errorf("%w: move %s", ErrNotFound, id)
	}
	return m, err
}

// CreateMove opens a move of session sessionID to target (a machine name or
// api.MoveCloud), started by by (web:<name> or machine:<name>). Every rule
// in docs/server.md, "Moves", that fails is ErrNotControllable with the
// reason; an unknown session is ErrNotFound.
func (s *Store) CreateMove(ctx context.Context, sessionID, target, by string) (api.Move, error) {
	if !ValidSessionID(sessionID) {
		return api.Move{}, invalidf("session id %q: use 1-128 letters, digits, '_', or '-', starting with a letter or digit", sessionID)
	}
	if target != api.MoveCloud && !ValidMachineName(target) {
		return api.Move{}, invalidf("target %q: want a machine name or cloud", target)
	}
	if !strings.HasPrefix(by, "web:") && !strings.HasPrefix(by, api.RequestedByMachinePrefix) {
		return api.Move{}, invalidf("by %q: want web:<name> or machine:<name>", by)
	}
	if err := checkText("by", by, maxByRunes); err != nil {
		return api.Move{}, err
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Move{}, err
	}
	defer tx.Rollback()
	if _, err := expireMovesTx(ctx, tx, now); err != nil {
		return api.Move{}, err
	}
	var srcID int64
	var srcName, srcKey, pane, agentState, gitRepo string
	var ended, srcPoll sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT s.machine_id, m.name, m.move_key, s.herdr_pane, s.agent_state, s.git_repo,
		s.ended_at, m.last_poll FROM sessions s JOIN machines m ON m.id = s.machine_id WHERE s.id = ?`, sessionID).
		Scan(&srcID, &srcName, &srcKey, &pane, &agentState, &gitRepo, &ended, &srcPoll)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Move{}, fmt.Errorf("%w: session %s", ErrNotFound, sessionID)
	}
	if err != nil {
		return api.Move{}, err
	}
	switch {
	case ended.Valid:
		return api.Move{}, refusef("session %s has ended", sessionID)
	case pane == "":
		return api.Move{}, refusef("session %s is not in herdr", sessionID)
	case agentState != "idle" && agentState != "done":
		state := agentState
		if state == "" {
			state = "in an unknown state"
		}
		return api.Move{}, refusef("the agent is %s; a session moves only when it is idle or done", state)
	}
	if ok, err := polled(srcPoll, now); err != nil {
		return api.Move{}, err
	} else if !ok {
		return api.Move{}, refusef("the sessionhub watcher on %q is offline", srcName)
	}
	if srcKey == "" {
		return api.Move{}, refusef("machine %q has no move key yet; run sessionhub install-plugin there", srcName)
	}
	var openID, openState string
	err = tx.QueryRowContext(ctx, `SELECT id, state FROM moves WHERE session_id = ? AND state IN (?, ?, ?, ?)
		ORDER BY created_at DESC LIMIT 1`, sessionID, api.MoveRequested, api.MovePacking, api.MoveUploaded, api.MoveUnpacking).
		Scan(&openID, &openState)
	if err == nil {
		return api.Move{}, refusef("move %s of this session is %s", openID, openState)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return api.Move{}, err
	}
	var targetID sql.NullInt64
	if target == api.MoveCloud {
		if !move.IsGitHub(gitRepo) {
			return api.Move{}, refusef("a cloud move needs a GitHub remote; this session's remote is %q", gitRepo)
		}
	} else {
		var tid int64
		var tkey string
		var tpoll sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id, move_key, last_poll FROM machines WHERE name = ?`, target).Scan(&tid, &tkey, &tpoll)
		if errors.Is(err, sql.ErrNoRows) {
			return api.Move{}, refusef("no machine named %q", target)
		}
		if err != nil {
			return api.Move{}, err
		}
		if tid == srcID {
			return api.Move{}, refusef("session %s is already on %q", sessionID, target)
		}
		if ok, err := polled(tpoll, now); err != nil {
			return api.Move{}, err
		} else if !ok {
			return api.Move{}, refusef("the sessionhub watcher on %q is offline", target)
		}
		if tkey == "" {
			return api.Move{}, refusef("machine %q has no move key yet; run sessionhub install-plugin there", target)
		}
		targetID = sql.NullInt64{Int64: tid, Valid: true}
	}
	id, err := newRandomID("mv_")
	if err != nil {
		return api.Move{}, err
	}
	nowS := formatTS(now)
	if _, err := tx.ExecContext(ctx, `INSERT INTO moves (id, session_id, source_machine_id, target, target_machine_id,
		state, created_at, updated_at, requested_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sessionID, srcID, target, targetID, api.MoveRequested, nowS, nowS, by); err != nil {
		return api.Move{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Move{}, err
	}
	return api.Move{ID: id, SessionID: sessionID, Source: srcName, Target: target, State: api.MoveRequested,
		By: by, CreatedAt: now, UpdatedAt: now}, nil
}

// ClaimMove hands machineID its next move step, oldest first: a requested
// move it is the source of (now packing, action move-out, with the target's
// public key), else an uploaded move it is the target of (now unpacking,
// action move-in, with the source's public key), else an ended move whose
// source still owes its finish (action move-out with the final state; the
// finish is marked given). ok is false when there is none.
func (s *Store) ClaimMove(ctx context.Context, machineID int64) (api.ControlClaim, bool, error) {
	now := s.Now()
	nowS := formatTS(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	defer tx.Rollback()
	if _, err := expireMovesTx(ctx, tx, now); err != nil {
		return api.ControlClaim{}, false, err
	}
	var id, peerKey, action string
	err = tx.QueryRowContext(ctx, `SELECT mv.id, COALESCE(mt.move_key, '') FROM moves mv
		LEFT JOIN machines mt ON mt.id = mv.target_machine_id
		WHERE mv.source_machine_id = ? AND mv.state = ? ORDER BY mv.created_at, mv.rowid LIMIT 1`,
		machineID, api.MoveRequested).Scan(&id, &peerKey)
	if err == nil {
		action = api.ActionMoveOut
		_, err = tx.ExecContext(ctx, `UPDATE moves SET state = ?, updated_at = ? WHERE id = ?`, api.MovePacking, nowS, id)
	} else if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT mv.id, ms.move_key FROM moves mv JOIN machines ms ON ms.id = mv.source_machine_id
			WHERE mv.target_machine_id = ? AND mv.state = ? ORDER BY mv.created_at, mv.rowid LIMIT 1`,
			machineID, api.MoveUploaded).Scan(&id, &peerKey)
		if err == nil {
			action = api.ActionMoveIn
			_, err = tx.ExecContext(ctx, `UPDATE moves SET state = ?, updated_at = ? WHERE id = ?`, api.MoveUnpacking, nowS, id)
		} else if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `SELECT id FROM moves WHERE source_machine_id = ? AND state IN (?, ?, ?)
				AND source_closed_at IS NULL ORDER BY updated_at, rowid LIMIT 1`,
				machineID, api.MoveDone, api.MoveFailed, api.MoveCancelled).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return api.ControlClaim{}, false, tx.Commit()
			}
			if err == nil {
				action, peerKey = api.ActionMoveOut, ""
				_, err = tx.ExecContext(ctx, `UPDATE moves SET source_closed_at = ? WHERE id = ?`, nowS, id)
			}
		}
	}
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return api.ControlClaim{}, false, err
	}
	// GetMove and GetSession use s.db, so they run after the commit.
	mv, err := s.GetMove(ctx, id)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	sess, err := s.GetSession(ctx, mv.SessionID)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	machine := mv.Source
	if action == api.ActionMoveIn {
		machine = mv.Target
	}
	req := api.ControlRequest{ID: mv.ID, SessionID: mv.SessionID, Machine: machine, Action: action, State: api.ControlClaimed,
		RequestedBy: mv.By, CreatedAt: mv.CreatedAt, ExpiresAt: mv.UpdatedAt.Add(MoveStepTTL), ClaimedAt: &now}
	return api.ControlClaim{Request: req, Session: sess, Move: &api.MoveClaim{ID: mv.ID, Source: mv.Source,
		Target: mv.Target, State: mv.State, PeerKey: peerKey, Detail: mv.Detail}}, true, nil
}

// moveRow is what the step checks read.
type moveRow struct {
	sessionID string
	source    int64
	target    sql.NullInt64 // invalid for a cloud move
	state     string
}

// stepTx reads move id after expiring old moves, and checks that it is
// machineID's step: the source while packing, or the target while
// unpacking. A machine whose step it is not is ErrWrongMachine; the machine
// whose step it is, in another state, or either machine once the move
// ended, is ErrRequestClosed.
func stepTx(ctx context.Context, tx *sql.Tx, machineID int64, id string, now time.Time) (moveRow, error) {
	if _, err := expireMovesTx(ctx, tx, now); err != nil {
		return moveRow{}, err
	}
	var r moveRow
	err := tx.QueryRowContext(ctx, `SELECT session_id, source_machine_id, target_machine_id, state FROM moves WHERE id = ?`, id).
		Scan(&r.sessionID, &r.source, &r.target, &r.state)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("%w: move %s", ErrNotFound, id)
	}
	if err != nil {
		return r, err
	}
	isSource := r.source == machineID
	isTarget := r.target.Valid && r.target.Int64 == machineID
	// The source acts while requested or packing, the target while uploaded
	// or unpacking.
	stepOf := r.source
	if (r.state == api.MoveUploaded || r.state == api.MoveUnpacking) && r.target.Valid {
		stepOf = r.target.Int64
	}
	switch {
	case r.state == api.MovePacking && isSource, r.state == api.MoveUnpacking && isTarget:
		return r, nil
	case (isSource || isTarget) && !api.MoveOpen(r.state), machineID == stepOf:
		return r, fmt.Errorf("%w: move %s is %s, not this machine's step", ErrRequestClosed, id, r.state)
	}
	return r, fmt.Errorf("%w: move %s: this machine has no step in it now", ErrWrongMachine, id)
}

// MoveForUpload checks that machineID may upload move id's bundle now: it
// is the source of a machine move that is packing.
func (s *Store) MoveForUpload(ctx context.Context, machineID int64, id string) (api.Move, error) {
	if err := checkMoveID(id); err != nil {
		return api.Move{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Move{}, err
	}
	defer tx.Rollback()
	r, err := stepTx(ctx, tx, machineID, id, s.Now())
	if err != nil {
		return api.Move{}, err
	}
	if r.source != machineID || !r.target.Valid {
		return api.Move{}, fmt.Errorf("%w: move %s takes no bundle from this machine", ErrRequestClosed, id)
	}
	if err := tx.Commit(); err != nil {
		return api.Move{}, err
	}
	return s.GetMove(ctx, id)
}

// MoveUploaded records that move id's sealed bundle of size bytes is
// stored: the move becomes uploaded, ready for the target's claim.
func (s *Store) MoveUploaded(ctx context.Context, machineID int64, id string, size int64) (api.Move, error) {
	if err := checkMoveID(id); err != nil {
		return api.Move{}, err
	}
	if size <= 0 || size > api.MaxSealedBundle {
		return api.Move{}, invalidf("bundle size %d: want 1 to %d bytes", size, api.MaxSealedBundle)
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Move{}, err
	}
	defer tx.Rollback()
	r, err := stepTx(ctx, tx, machineID, id, now)
	if err != nil {
		return api.Move{}, err
	}
	if r.source != machineID || !r.target.Valid {
		return api.Move{}, fmt.Errorf("%w: move %s takes no bundle from this machine", ErrRequestClosed, id)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE moves SET state = ?, bundle_size = ?, updated_at = ? WHERE id = ?`,
		api.MoveUploaded, size, formatTS(now), id); err != nil {
		return api.Move{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Move{}, err
	}
	return s.GetMove(ctx, id)
}

// MoveForDownload checks that machineID may download move id's bundle now:
// it is the target and the move is unpacking.
func (s *Store) MoveForDownload(ctx context.Context, machineID int64, id string) (api.Move, error) {
	if err := checkMoveID(id); err != nil {
		return api.Move{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Move{}, err
	}
	defer tx.Rollback()
	r, err := stepTx(ctx, tx, machineID, id, s.Now())
	if err != nil {
		return api.Move{}, err
	}
	if r.state != api.MoveUnpacking {
		return api.Move{}, fmt.Errorf("%w: move %s is %s, not unpacking", ErrRequestClosed, id, r.state)
	}
	if err := tx.Commit(); err != nil {
		return api.Move{}, err
	}
	return s.GetMove(ctx, id)
}

// FinishMove records the result of machineID's step of move id: done or
// failed. The source may fail a packing move, or finish a cloud move done
// with its link; the target may finish or fail an unpacking move. A
// machine move's done hands the session to the target; a cloud move's done
// ends it. A result the source posts closes its part, so it gets no finish.
// Once the move ended and the source's part closed, the source may post the
// final state again with a detail, which is added to the move's detail
// (sourceNoteTx).
func (s *Store) FinishMove(ctx context.Context, machineID int64, id string, in api.MoveResultIn) (api.Move, error) {
	if err := checkMoveID(id); err != nil {
		return api.Move{}, err
	}
	if in.State != api.MoveDone && in.State != api.MoveFailed {
		return api.Move{}, invalidf("state %q: want done or failed", in.State)
	}
	if err := checkText("detail", in.Detail, api.MaxMoveDetailRunes); err != nil {
		return api.Move{}, err
	}
	if in.CloudURL != "" && (in.State != api.MoveDone || !ValidRemoteControlURL(in.CloudURL)) {
		return api.Move{}, invalidf("cloud_url %q: want https://claude.ai/code/session_<id>, on a done result only", in.CloudURL)
	}
	now := s.Now()
	nowS := formatTS(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Move{}, err
	}
	defer tx.Rollback()
	r, err := stepTx(ctx, tx, machineID, id, now)
	if errors.Is(err, ErrRequestClosed) {
		noted, nerr := sourceNoteTx(ctx, tx, machineID, id, in, nowS)
		if nerr != nil {
			return api.Move{}, nerr
		}
		if noted {
			if err := tx.Commit(); err != nil {
				return api.Move{}, err
			}
			return s.GetMove(ctx, id)
		}
	}
	if err != nil {
		return api.Move{}, err
	}
	cloud := !r.target.Valid
	bySource := r.state == api.MovePacking
	switch {
	case in.State == api.MoveDone && bySource && !cloud:
		return api.Move{}, fmt.Errorf("%w: move %s is done only when its target reports it", ErrRequestClosed, id)
	case in.State == api.MoveDone && cloud && in.CloudURL == "":
		return api.Move{}, invalidf("cloud_url: a cloud move's done carries the cloud session link")
	case in.CloudURL != "" && !cloud:
		return api.Move{}, invalidf("cloud_url: only a cloud move has a link")
	}
	closed := sql.NullString{}
	if bySource {
		closed = sql.NullString{String: nowS, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE moves SET state = ?, detail = ?, cloud_url = ?, updated_at = ?,
		source_closed_at = COALESCE(source_closed_at, ?) WHERE id = ?`,
		in.State, in.Detail, in.CloudURL, nowS, closed, id); err != nil {
		return api.Move{}, err
	}
	if in.State == api.MoveDone {
		var from, to string
		if err := tx.QueryRowContext(ctx, `SELECT ms.name, mv.target FROM moves mv JOIN machines ms ON ms.id = mv.source_machine_id
			WHERE mv.id = ?`, id).Scan(&from, &to); err != nil {
			return api.Move{}, err
		}
		p := map[string]string{"move_id": id, "from": from, "to": to}
		if cloud {
			p["cloud_url"] = in.CloudURL
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at = COALESCE(ended_at, ?), rc_url = '', rc_at = NULL
				WHERE id = ?`, nowS, r.sessionID); err != nil {
				return api.Move{}, err
			}
			ended, _ := json.Marshal(map[string]string{"reason": api.EndedMovedToCloud})
			if err := insertEventTx(ctx, tx, r.sessionID, now, api.SourceServer, api.KindEnded, ended); err != nil {
				return api.Move{}, err
			}
			if err := closePermissionsTx(ctx, tx, r.sessionID, api.PermissionClosed, now, now); err != nil {
				return api.Move{}, err
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE sessions SET machine_id = ?, herdr_session = '', herdr_workspace = '',
			herdr_pane = '', rc_url = '', rc_at = NULL WHERE id = ?`, r.target.Int64, r.sessionID); err != nil {
			return api.Move{}, err
		}
		payload, err := json.Marshal(p)
		if err != nil {
			return api.Move{}, err
		}
		if err := insertEventTx(ctx, tx, r.sessionID, now, api.SourceServer, api.KindMoved, payload); err != nil {
			return api.Move{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return api.Move{}, err
	}
	return s.GetMove(ctx, id)
}
```

- [ ] **Step 6: Carry the latest move on every session read**

In `internal/store/read.go`, replace:

```go
	s.last_prompt, s.last_prompt_at, dg.body
FROM sessions s
```

with:

```go
	s.last_prompt, s.last_prompt_at, dg.body,
	mv.id, mv.target, mv.state, mv.detail, mv.cloud_url, mv.requested_by, mv.bundle_size, mv.created_at, mv.updated_at, mvs.name
FROM sessions s
```

replace:

```go
LEFT JOIN session_digests dg ON dg.session_id = s.id`
```

with:

```go
LEFT JOIN session_digests dg ON dg.session_id = s.id
LEFT JOIN moves mv ON mv.id = (SELECT id FROM moves WHERE session_id = s.id ORDER BY created_at DESC, rowid DESC LIMIT 1)
LEFT JOIN machines mvs ON mvs.id = mv.source_machine_id`
```

replace:

```go
	var rc nullControl
	err := sc.Scan(&x.ID, &x.Agent, &x.Machine, &sshHost, &x.CWD, &x.GitRepo, &x.GitBranch,
		&x.HerdrSession, &x.HerdrWorkspace, &x.HerdrPane, &x.Title, &x.TitleSource, &x.AgentState,
		&started, &lastSeen, &ended, &rTS, &rDone, &rInFlight, &rWaiting, &rNote,
		&lastPoll, &x.RemoteControlURL, &rcAt,
		&rc.id, &rc.action, &rc.state, &rc.requestedBy, &rc.created, &rc.expires, &rc.claimed, &rc.finished, &rc.url, &rc.detail,
		&x.LastPrompt, &lastPromptAt, &digestBody)
```

with:

```go
	var rc nullControl
	var mv nullMove
	err := sc.Scan(&x.ID, &x.Agent, &x.Machine, &sshHost, &x.CWD, &x.GitRepo, &x.GitBranch,
		&x.HerdrSession, &x.HerdrWorkspace, &x.HerdrPane, &x.Title, &x.TitleSource, &x.AgentState,
		&started, &lastSeen, &ended, &rTS, &rDone, &rInFlight, &rWaiting, &rNote,
		&lastPoll, &x.RemoteControlURL, &rcAt,
		&rc.id, &rc.action, &rc.state, &rc.requestedBy, &rc.created, &rc.expires, &rc.claimed, &rc.finished, &rc.url, &rc.detail,
		&x.LastPrompt, &lastPromptAt, &digestBody,
		&mv.id, &mv.target, &mv.state, &mv.detail, &mv.cloudURL, &mv.by, &mv.size, &mv.created, &mv.updated, &mv.source)
```

and replace:

```go
	if x.LastPromptAt, err = parseNullTS(lastPromptAt); err != nil {
		return x, err
	}
```

with:

```go
	if x.LastPromptAt, err = parseNullTS(lastPromptAt); err != nil {
		return x, err
	}
	if x.Move, err = mv.move(x.ID); err != nil {
		return x, err
	}
```

- [ ] **Step 7: Show keys on the machine list**

In `internal/store/machines.go`, replace:

```go
	rows, err := s.db.QueryContext(ctx, `SELECT name, ssh_host, herdr_host, last_seen FROM machines ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Machine{}
	for rows.Next() {
		var m api.Machine
		var last sql.NullString
		if err := rows.Scan(&m.Name, &m.SSHHost, &m.HerdrHost, &last); err != nil {
			return nil, err
		}
		if m.LastSeen, err = parseNullTS(last); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
```

with:

```go
	rows, err := s.db.QueryContext(ctx, `SELECT name, ssh_host, herdr_host, last_seen, move_key, last_poll FROM machines ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.Now()
	out := []api.Machine{}
	for rows.Next() {
		var m api.Machine
		var last, poll sql.NullString
		var key string
		if err := rows.Scan(&m.Name, &m.SSHHost, &m.HerdrHost, &last, &key, &poll); err != nil {
			return nil, err
		}
		if m.LastSeen, err = parseNullTS(last); err != nil {
			return nil, err
		}
		if raw, err := api.ParseMoveKey(key); err == nil {
			m.MoveKey = api.MoveKeyFingerprint(raw)
		}
		ok, err := polled(poll, now)
		if err != nil {
			return nil, err
		}
		m.MoveReady = m.MoveKey != "" && ok
		out = append(out, m)
	}
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test ./internal/store/ -count=1 -race`
Expected: PASS, including the older migration tests (their rollbacks now drop
v9 first).

- [ ] **Step 9: Document the table**

In `docs/server.md`, at the end of the "Database" section (after
"### Shared instructions, messages, and permission requests" and its text),
add:

```markdown
### Moves

Schema version 9 adds `machines.move_key` (the machine's X25519 public key
in standard base64, or empty until its watcher registers one) and `moves`:

| Column | Holds |
|---|---|
| `id` | `mv_` and 16 random bytes in base64url |
| `session_id` | the session (`ON DELETE CASCADE`) |
| `source_machine_id` | the machine the session moves from (`ON DELETE CASCADE`) |
| `target` | the target machine's name, or `cloud` |
| `target_machine_id` | the target machine (`ON DELETE CASCADE`); `NULL` for `cloud` |
| `state` | `requested`, `packing`, `uploaded`, `unpacking`, `done`, `failed`, or `cancelled` (defined, never set yet) |
| `detail` | why it failed, as `<step>: <reason>`, or what the target did not carry; then any notes the source added after the move ended |
| `created_at`, `updated_at` | times; the timeouts count from `updated_at` |
| `bundle_size` | the sealed bundle's size in bytes |
| `cloud_url` | the cloud session link of a cloud move |
| `requested_by` | `web:<name>` or `machine:<name>` (`BY` is an SQL keyword) |
| `source_closed_at` | when the source finished its part; `NULL` while it still owes a finish |

Bundles are files, never rows (see "Moves" under the API). The version 8
binary refuses a version 9 database; restoring the backup is the only
rollback.
```

- [ ] **Step 10: Lint and commit**

Run: `gofmt -w internal/store/*.go && make lint`
Expected: clean.

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1`
Expected: every package `ok`. The server's `TestMachineLastSeenAndList`
still passes: it reads fields, and `move_ready` is new.

```bash
git add internal/store/store.go internal/store/moves.go internal/store/moves_test.go internal/store/read.go \
  internal/store/machines.go internal/store/concurrency_test.go internal/store/store_test.go docs/server.md
git commit -m "$(cat <<'EOF'
Store moves and machine move keys (schema 9)

A move is requested, claimed by its source (packing), uploaded, claimed
by its target (unpacking), then done or failed. The source of a move
that ended without its own result gets one more claim, the finish, so a
watcher restart mid-move still archives or restores the session. Done
hands the session to the target machine; a cloud move's done ends it
with reason moved_to_cloud. Once its part closed, the source can add a
note to an ended move's detail.

Refs: docs/dev/superpowers/plans/2026-10-03-move-session.md, Task 3

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 4: Server: routes, bundle files, and auth

**Files:**
- Create: `internal/server/moves.go` (handlers, `SetMoveDir`, `sweepMoves`,
  `runMoveSweeper`, `isBundleUpload`)
- Create: `internal/server/moves_test.go`
- Modify: `internal/server/routes.go` (`accessMove`, six routes)
- Modify: `internal/server/server.go` (`moveDir` field, the bundle upload's
  body limit)
- Modify: `internal/server/control.go` (the long poll also claims moves)
- Modify: `internal/server/run.go` (bundle directory and sweeper)
- Modify: `internal/server/machine.go` (`MOVE_KEY` column)
- Modify: `internal/server/machine_test.go` (the `ls` header)
- Modify: `internal/server/auth_test.go` (six cases, `cookie+move`)
- Modify: `internal/server/helpers_test.go` (`env.moveDir`, `moveRows` in
  the snapshot, `testMoveKey`)
- Modify: `docs/server.md` (Authentication, Endpoints, a "Moves" section,
  Limits, Field validation, Manage machines)

**Interfaces:**
- Consumes: Task 3's store functions; `decider`, `pathID`, `decode`,
  `storeError`, `writeJSON`, `writeError`, `s.control.notify`.
- Produces: `func (s *Server) SetMoveDir(dir string)`, `sweepMoves`,
  `runMoveSweeper`, `maxBundleBytes` (a variable tests lower), the six routes
  in Global Constraints.

- [ ] **Step 1: Write the failing route tests**

In `internal/server/helpers_test.go`, add `"crypto/ecdh"`, `"crypto/rand"`,
`"encoding/base64"`, and `"os"` to the imports, replace:

```go
	dbPath string // the store's file, for reads the store has no method for
}
```

with:

```go
	dbPath  string // the store's file, for reads the store has no method for
	moveDir string // where the server keeps sealed bundles
}
```

replace:

```go
	logBuf := &syncBuffer{}
	s := New(st, testPublicURL, log.New(logBuf, "", 0))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e := &env{t: t, st: st, clock: clock, srv: srv, server: s, log: logBuf, tokA: tokA, tokB: tokB, dbPath: dbPath}
```

with:

```go
	logBuf := &syncBuffer{}
	s := New(st, testPublicURL, log.New(logBuf, "", 0))
	moveDir := filepath.Join(t.TempDir(), "moves")
	s.SetMoveDir(moveDir)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e := &env{t: t, st: st, clock: clock, srv: srv, server: s, log: logBuf, tokA: tokA, tokB: tokB, dbPath: dbPath, moveDir: moveDir}
```

replace:

```go
	return string(b) + "\ninbox_triage:\n" + e.triageRows() + "actions:\n" + e.actionRows()
}
```

with:

```go
	return string(b) + "\ninbox_triage:\n" + e.triageRows() + "actions:\n" + e.actionRows() + "moves:\n" + e.moveRows()
}

// moveRows dumps moves, every machine's move key, and the bundle files, so
// the auth matrix sees a rejected request that wrote only there.
func (e *env) moveRows() string {
	e.t.Helper()
	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	var b strings.Builder
	for _, q := range []string{
		`SELECT 'move|' || id || '|' || state || '|' || detail || '|' || bundle_size || '|' || COALESCE(source_closed_at, '') FROM moves ORDER BY id`,
		`SELECT 'key|' || name || '|' || move_key FROM machines ORDER BY name`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			e.t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				e.t.Fatal(err)
			}
			b.WriteString(line + "\n")
		}
		rows.Close()
	}
	files, _ := os.ReadDir(e.moveDir)
	for _, f := range files {
		info, _ := f.Info()
		fmt.Fprintf(&b, "file|%s|%d\n", f.Name(), info.Size())
	}
	return b.String()
}

// testMoveKey is a fresh X25519 public key in the server's form.
func testMoveKey(t *testing.T) string {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
}
```

Create `internal/server/moves_test.go`:

```go
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

func mustURL(t *testing.T, p string) *url.URL {
	t.Helper()
	u, err := url.Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// moveSetup gives both machines a key over the API, records a poll from
// both, and registers id on tower, idle in a herdr pane with a GitHub remote.
func (e *env) moveSetup(id string) (towerKey, bluKey string) {
	e.t.Helper()
	towerKey, bluKey = testMoveKey(e.t), testMoveKey(e.t)
	if code, b, _ := e.do("PUT", "/v1/machines/self/move-key", e.tokA, api.MoveKeyIn{PublicKey: towerKey}); code != 204 {
		e.t.Fatalf("tower key: %d %s", code, b)
	}
	if code, b, _ := e.do("PUT", "/v1/machines/self/move-key", e.tokB, api.MoveKeyIn{PublicKey: bluKey}); code != 204 {
		e.t.Fatalf("bluebox key: %d %s", code, b)
	}
	e.register(e.tokA, api.SessionUpsert{ID: id, HerdrSession: "default", HerdrPane: "w1:p1", CWD: "/home/user/proj",
		AgentState: "idle", GitRepo: "git@github.com:o/r.git", GitBranch: "main"})
	e.recordPoll(e.tokA)
	e.recordPoll(e.tokB)
	return towerKey, bluKey
}

// claim polls once as token and decodes the claim; it fails unless one
// came.
func (e *env) claim(token string) api.ControlClaim {
	e.t.Helper()
	e.server.after = instantTimer
	var c api.ControlClaim
	e.must(200, "GET", "/v1/machines/self/control?wait=1", token, nil, &c)
	return c
}

func (e *env) startMove(id, target string) api.Move {
	e.t.Helper()
	code, b, _ := e.doHdr("POST", "/v1/sessions/"+id+"/move", "", e.web, map[string]string{api.HeaderAction: api.HeaderActionMove},
		api.MoveIn{Target: target})
	if code != http.StatusAccepted {
		e.t.Fatalf("POST move: %d %s", code, b)
	}
	var mv api.Move
	json.Unmarshal(b, &mv)
	return mv
}

func TestMoveRoutesFlow(t *testing.T) {
	e := newEnv(t)
	towerKey, bluKey := e.moveSetup(sid1)
	var ms []api.Machine
	e.must(200, "GET", "/v1/machines", e.tokA, nil, &ms)
	if len(ms) != 2 || len(ms[0].MoveKey) != 16 || !ms[0].MoveReady || len(ms[1].MoveKey) != 16 {
		t.Fatalf("machines: %+v", ms)
	}

	mv := e.startMove(sid1, "bluebox")
	if mv.State != api.MoveRequested || mv.By != "web:phone" || mv.Source != "tower" {
		t.Fatalf("move %+v", mv)
	}
	if code, b, _ := e.doHdr("POST", "/v1/sessions/"+sid1+"/move", "", e.web, map[string]string{api.HeaderAction: api.HeaderActionMove},
		api.MoveIn{Target: "bluebox"}); code != http.StatusConflict || !strings.Contains(string(b), "is requested") {
		t.Errorf("second move: %d %s", code, b)
	}

	out := e.claim(e.tokA)
	if out.Request.Action != api.ActionMoveOut || out.Move == nil || out.Move.PeerKey != bluKey || out.Move.State != api.MovePacking {
		t.Fatalf("move-out claim %+v %+v", out.Request, out.Move)
	}
	sealed := bytes.Repeat([]byte{0xA5}, 4096)
	var up api.Move
	e.must(200, "PUT", "/v1/moves/"+mv.ID+"/bundle", e.tokA, sealed, &up)
	if up.State != api.MoveUploaded || up.BundleSize != 4096 {
		t.Fatalf("upload: %+v", up)
	}
	path := filepath.Join(e.moveDir, mv.ID+".sealed")
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("bundle file: %v %v", fi, err)
	}
	if di, _ := os.Stat(e.moveDir); di.Mode().Perm() != 0o700 {
		t.Errorf("bundle dir mode %v", di.Mode().Perm())
	}

	in := e.claim(e.tokB)
	if in.Request.Action != api.ActionMoveIn || in.Move.PeerKey != towerKey || in.Move.State != api.MoveUnpacking {
		t.Fatalf("move-in claim %+v %+v", in.Request, in.Move)
	}
	code, got, h := e.do("GET", "/v1/moves/"+mv.ID+"/bundle", e.tokB, nil)
	if code != 200 || !bytes.Equal(got, sealed) || h.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("download: %d, %d bytes, %q", code, len(got), h.Get("Content-Type"))
	}
	var done api.Move
	e.must(200, "POST", "/v1/moves/"+mv.ID+"/result", e.tokB, api.MoveResultIn{State: api.MoveDone}, &done)
	if done.State != api.MoveDone {
		t.Fatalf("result: %+v", done)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("bundle kept after done: %v", err)
	}
	if d := e.detail(sid1); d.Machine != "bluebox" || d.HerdrPane != "" || d.Move == nil || d.Move.State != api.MoveDone {
		t.Errorf("session after the move: %+v", d.Session)
	}
	var read api.Move
	e.must(200, "GET", "/v1/moves/"+mv.ID, e.tokA, nil, &read)
	if read.State != api.MoveDone {
		t.Errorf("GET move: %+v", read)
	}
	if fin := e.claim(e.tokA); fin.Request.Action != api.ActionMoveOut || fin.Move.State != api.MoveDone {
		t.Errorf("finish: %+v %+v", fin.Request, fin.Move)
	}
	if code, _, _ := e.do("GET", "/v1/moves/nope", e.tokA, nil); code != 400 {
		t.Errorf("bad move id: %d", code)
	}
}

func TestMoveBundleLimits(t *testing.T) {
	e := newEnv(t)
	e.moveSetup(sid1)
	old := maxBundleBytes
	maxBundleBytes = 1024
	t.Cleanup(func() { maxBundleBytes = old })
	mv := e.startMove(sid1, "bluebox")
	e.claim(e.tokA)
	for _, c := range []struct {
		body []byte
		want int
	}{{nil, 400}, {bytes.Repeat([]byte("x"), 1025), 413}} {
		code, b, _ := e.do("PUT", "/v1/moves/"+mv.ID+"/bundle", e.tokA, c.body)
		if code != c.want {
			t.Errorf("%d bytes: %d %s, want %d", len(c.body), code, b, c.want)
		}
		if files, _ := os.ReadDir(e.moveDir); len(files) != 0 {
			t.Errorf("%d bytes left files: %v", len(c.body), files)
		}
	}
	if code, b, _ := e.do("PUT", "/v1/moves/"+mv.ID+"/bundle", e.tokA, bytes.Repeat([]byte("x"), 1024)); code != 200 {
		t.Errorf("exactly the limit: %d %s", code, b)
	}
	// A body over 64 KiB elsewhere is still refused.
	if code, _, _ := e.do("PUT", "/v1/machines/self/move-key", e.tokA, bytes.Repeat([]byte("x"), MaxBodyBytes+1)); code != 413 {
		t.Errorf("big body on another route: %d", code)
	}
	if isBundleUpload(&http.Request{Method: "PUT", URL: mustURL(t, "/v1/moves/x/bundle/more")}) {
		t.Error("a longer path counts as a bundle upload")
	}
}

func TestMoveBundleDeletedOnEnd(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.moveSetup(sid1)
	mv := e.startMove(sid1, "bluebox")
	e.claim(e.tokA)
	e.must(200, "PUT", "/v1/moves/"+mv.ID+"/bundle", e.tokA, []byte("sealed"), nil)
	e.claim(e.tokB)
	var failed api.Move
	e.must(200, "POST", "/v1/moves/"+mv.ID+"/result", e.tokB, api.MoveResultIn{State: api.MoveFailed, Detail: "find clone: none"}, &failed)
	if _, err := os.Stat(filepath.Join(e.moveDir, mv.ID+".sealed")); !os.IsNotExist(err) {
		t.Errorf("bundle kept after failed: %v", err)
	}
	if fin := e.claim(e.tokA); fin.Move.State != api.MoveFailed || fin.Move.Detail != "find clone: none" {
		t.Errorf("finish: %+v", fin.Move)
	}

	// A move that times out while uploaded loses its bundle at the next
	// sweep, and its source gets a finish.
	e.recordPoll(e.tokA)
	e.recordPoll(e.tokB)
	mv2 := e.startMove(sid1, "bluebox")
	e.claim(e.tokA)
	e.must(200, "PUT", "/v1/moves/"+mv2.ID+"/bundle", e.tokA, []byte("sealed"), nil)
	e.clock.Advance(store.MoveStepTTL)
	var read api.Move
	e.must(200, "GET", "/v1/moves/"+mv2.ID, e.tokB, nil, &read)
	if read.State != api.MoveFailed || read.Detail != "uploaded: timed out" {
		t.Errorf("after the timeout: %+v", read)
	}
	if _, err := os.Stat(filepath.Join(e.moveDir, mv2.ID+".sealed")); !os.IsNotExist(err) {
		t.Errorf("bundle kept after the timeout: %v", err)
	}

	// Stray files: an unknown move's bundle goes; a temp file goes once it
	// is an hour old.
	stray := filepath.Join(e.moveDir, "mv_AAAAAAAAAAAAAAAAAAAAAA.sealed")
	oldTmp := filepath.Join(e.moveDir, "mv_x.123.tmp")
	newTmp := filepath.Join(e.moveDir, "mv_y.456.tmp")
	for _, p := range []string{stray, oldTmp, newTmp} {
		os.WriteFile(p, []byte("x"), 0o600)
	}
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(oldTmp, past, past)
	e.server.sweepMoves(ctx)
	for p, want := range map[string]bool{stray: false, oldTmp: false, newTmp: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s: exists %v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}

func TestMoveWakesPolls(t *testing.T) {
	e := newEnv(t)
	e.moveSetup(sid1)
	timers := &fakeTimers{}
	e.server.after = timers.after
	src := e.pollAsync(e.tokA, "?wait=30")
	timers.await(t, 1)
	mv := e.startMove(sid1, "bluebox")
	r := recv(t, src)
	var c api.ControlClaim
	if json.Unmarshal(r.body, &c); r.code != 200 || c.Move == nil || c.Move.ID != mv.ID {
		t.Fatalf("source poll: %d %s", r.code, r.body)
	}
	dst := e.pollAsync(e.tokB, "?wait=30")
	timers.await(t, 2)
	e.must(200, "PUT", "/v1/moves/"+mv.ID+"/bundle", e.tokA, []byte("sealed"), nil)
	r = recv(t, dst)
	if json.Unmarshal(r.body, &c); r.code != 200 || c.Request.Action != api.ActionMoveIn {
		t.Fatalf("target poll: %d %s", r.code, r.body)
	}
}
```


In `internal/server/auth_test.go`, add `"os"` and `"path/filepath"` to the
imports, and before the line
`linkCode := e.loginCode("tablet") // one unused code serves every GET` add:

```go
	// Moves. Both machines have a key and polled. Each upload and result
	// gets a fresh move in the state it needs, so a rejected request that
	// wrote shows up. claimUntil claims past older moves that authorized
	// requests left behind.
	towerKey := testMoveKey(t)
	for _, k := range []struct{ tok, key string }{{e.tokA, towerKey}, {e.tokB, testMoveKey(t)}} {
		if err := e.st.SetMoveKey(ctx, e.machine(k.tok).ID, k.key); err != nil {
			t.Fatal(err)
		}
	}
	e.recordPoll(e.tokB)
	moveN := 0
	movable := func(tok string) string {
		moveN++
		id := fmt.Sprintf("move-%d", moveN)
		e.register(tok, api.SessionUpsert{ID: id, HerdrSession: fmt.Sprintf("mv%d", moveN), HerdrPane: "p1", AgentState: "idle"})
		return id
	}
	claimUntil := func(tok, id string) {
		for range 50 {
			c, ok, err := e.st.ClaimMove(ctx, e.machine(tok).ID)
			if err != nil || !ok {
				t.Fatalf("claim %s: %v %v", id, ok, err)
			}
			if c.Move.ID == id {
				return
			}
		}
		t.Fatalf("move %s never claimed", id)
	}
	// packed is a move from tower to bluebox that tower claimed: tower uploads.
	packed := func() string {
		mv, err := e.st.CreateMove(ctx, movable(e.tokA), "bluebox", "machine:tower")
		if err != nil {
			t.Fatal(err)
		}
		claimUntil(e.tokA, mv.ID)
		return mv.ID
	}
	// unpacking is a move from bluebox to tower, uploaded and claimed by tower:
	// tower downloads and posts the result.
	unpacking := func() string {
		mv, err := e.st.CreateMove(ctx, movable(e.tokB), "tower", "machine:bluebox")
		if err != nil {
			t.Fatal(err)
		}
		claimUntil(e.tokB, mv.ID)
		if _, err := e.st.MoveUploaded(ctx, e.machine(e.tokB).ID, mv.ID, 6); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(e.moveDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.moveDir, mv.ID+".sealed"), []byte("sealed"), 0o600); err != nil {
			t.Fatal(err)
		}
		claimUntil(e.tokA, mv.ID)
		return mv.ID
	}
	downloadID := unpacking()
```

In the same file, before the line
`"GET /healthz":              {path: "/healthz", ok: 200},` add:

```go
		"POST /v1/sessions/{id}/move": {fresh: func() string { return "/v1/sessions/" + movable(e.tokA) + "/move" },
			body: api.MoveIn{Target: "bluebox"}, ok: 202},
		"GET /v1/moves/{id}": {path: "/v1/moves/" + downloadID, ok: 200},
		"PUT /v1/moves/{id}/bundle": {fresh: func() string { return "/v1/moves/" + packed() + "/bundle" },
			body: []byte("sealed bundle"), ok: 200, okB: 409},
		"GET /v1/moves/{id}/bundle": {path: "/v1/moves/" + downloadID + "/bundle", ok: 200, okB: 409},
		"POST /v1/moves/{id}/result": {fresh: func() string { return "/v1/moves/" + unpacking() + "/result" },
			body: api.MoveResultIn{State: api.MoveFailed, Detail: "matrix"}, ok: 200, okB: 409},
		"PUT /v1/machines/self/move-key": {path: "/v1/machines/self/move-key", body: api.MoveKeyIn{PublicKey: towerKey}, ok: 204},
```

replace:

```go
		{"cookie+approve", "", newSession, api.HeaderActionApprove},
```

with:

```go
		{"cookie+approve", "", newSession, api.HeaderActionApprove},
		{"cookie+move", "", newSession, api.HeaderActionMove},
```

and replace:

```go
		accessApprove:      api.HeaderActionApprove,
	}
```

with:

```go
		accessApprove:      api.HeaderActionApprove,
		accessMove:         api.HeaderActionMove,
	}
```

In `internal/server/machine_test.go`, replace:

```go
		regexp.MustCompile(`^NAME\s+SSH_HOST\s+HERDR_HOST\s+LAST_SEEN$`),
		regexp.MustCompile(`^bluebox\s+bluebox\s+bluebox\s+never$`),
```

with:

```go
		regexp.MustCompile(`^NAME\s+SSH_HOST\s+HERDR_HOST\s+LAST_SEEN\s+MOVE_KEY$`),
		regexp.MustCompile(`^bluebox\s+bluebox\s+bluebox\s+never\s+-$`),
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/server/ -count=1`
Expected: FAIL to compile with `s.SetMoveDir undefined`, `undefined:
maxBundleBytes`, `undefined: isBundleUpload`, and `undefined: accessMove`.

- [ ] **Step 3: Write the handlers**

Create `internal/server/moves.go`:

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

const (
	// moveBundleTTL is the longest a bundle file stays on disk.
	moveBundleTTL = time.Hour
	// moveSweepEvery is how often sessionhub server expires moves and deletes
	// bundle files.
	moveSweepEvery = time.Minute
	// bundleIOFor bounds one bundle upload or download, past the server's
	// 30-second read and write timeouts.
	bundleIOFor  = 10 * time.Minute
	bundleSuffix = ".sealed"
)

// maxBundleBytes caps a sealed bundle upload. Tests lower it.
var maxBundleBytes int64 = api.MaxSealedBundle

// SetMoveDir sets the directory for sealed bundles. Run sets moves/ next to
// the database; without one, the bundle routes answer 503.
func (s *Server) SetMoveDir(dir string) { s.moveDir = dir }

// isBundleUpload reports whether r is PUT /v1/moves/{id}/bundle, the one
// request whose body may pass MaxBodyBytes.
func isBundleUpload(r *http.Request) bool {
	if r.Method != http.MethodPut {
		return false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/moves/")
	id, tail, _ := strings.Cut(rest, "/")
	return ok && id != "" && tail == "bundle"
}

func (s *Server) bundlePath(id string) string { return filepath.Join(s.moveDir, id+bundleSuffix) }

// removeBundle deletes move id's bundle file, if there is one.
func (s *Server) removeBundle(id string) {
	if s.moveDir == "" {
		return
	}
	if err := os.Remove(s.bundlePath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Printf("moves: remove bundle %s: %v", id, err)
	}
}

// moveID returns the {id} path value if it is a move ID, or writes a 400.
func moveID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !store.ValidMoveID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid move id %q", id))
		return "", false
	}
	return id, true
}

// sweepMoves fails moves past their timeouts and wakes their sources for
// the finish, then deletes bundle files whose move ended or is unknown, and
// any file older than moveBundleTTL. It logs errors and never fails a
// request.
func (s *Server) sweepMoves(ctx context.Context) {
	ended, err := s.store.ExpireMoves(ctx)
	if err != nil {
		s.log.Printf("moves: expire: %v", err)
		return
	}
	for _, m := range ended {
		s.removeBundle(m.ID)
		s.control.notify(m.Source)
	}
	if s.moveDir == "" {
		return
	}
	entries, err := os.ReadDir(s.moveDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Printf("moves: read %s: %v", s.moveDir, err)
		}
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(s.moveDir, e.Name())
		if time.Since(info.ModTime()) > moveBundleTTL {
			os.Remove(p)
			continue
		}
		id, ok := strings.CutSuffix(e.Name(), bundleSuffix)
		if !ok {
			continue // an upload in progress
		}
		m, err := s.store.GetMove(ctx, id)
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalid) || (err == nil && !api.MoveOpen(m.State)) {
			os.Remove(p)
		}
	}
}

// runMoveSweeper runs sweepMoves every moveSweepEvery until ctx ends.
func (s *Server) runMoveSweeper(ctx context.Context) {
	t := time.NewTicker(moveSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepMoves(ctx)
		}
	}
}

// postMove is POST /v1/sessions/{id}/move. It wakes the source's poll.
func (s *Server) postMove(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in api.MoveIn
	if !decode(w, r, &in) {
		return
	}
	s.sweepMoves(r.Context())
	mv, err := s.store.CreateMove(r.Context(), id, strings.TrimSpace(in.Target), decider(p))
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.control.notify(mv.Source)
	writeJSON(w, http.StatusAccepted, mv)
}

// getMove is GET /v1/moves/{id}.
func (s *Server) getMove(w http.ResponseWriter, r *http.Request, _ principal) {
	id, ok := moveID(w, r)
	if !ok {
		return
	}
	s.sweepMoves(r.Context())
	mv, err := s.store.GetMove(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mv)
}

// putMoveKey is PUT /v1/machines/self/move-key.
func (s *Server) putMoveKey(w http.ResponseWriter, r *http.Request, p principal) {
	var in api.MoveKeyIn
	if !decode(w, r, &in) {
		return
	}
	if err := s.store.SetMoveKey(r.Context(), p.machine.ID, in.PublicKey); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// putMoveBundle is PUT /v1/moves/{id}/bundle: the source streams the sealed
// bundle to a temp file, which is renamed into place before the move turns
// uploaded. It wakes the target's poll.
func (s *Server) putMoveBundle(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := moveID(w, r)
	if !ok {
		return
	}
	if s.moveDir == "" {
		writeError(w, http.StatusServiceUnavailable, "this server has no bundle directory")
		return
	}
	ctx := r.Context()
	s.sweepMoves(ctx)
	if _, err := s.store.MoveForUpload(ctx, p.machine.ID, id); err != nil {
		s.storeError(w, err)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bundleIOFor))
	_ = rc.SetWriteDeadline(time.Now().Add(bundleIOFor))
	if err := os.MkdirAll(s.moveDir, 0o700); err != nil {
		s.internalError(w, err)
		return
	}
	f, err := os.CreateTemp(s.moveDir, id+".*.tmp")
	if err != nil {
		s.internalError(w, err)
		return
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once renamed
	n, err := io.Copy(f, r.Body)
	cerr := f.Close()
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge, "the bundle is larger than "+strconv.FormatInt(maxBundleBytes, 10)+" bytes")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "read bundle: "+err.Error())
		return
	case cerr != nil:
		s.internalError(w, cerr)
		return
	case n == 0:
		writeError(w, http.StatusBadRequest, "the bundle is empty")
		return
	}
	if err := os.Rename(tmp, s.bundlePath(id)); err != nil {
		s.internalError(w, err)
		return
	}
	mv, err := s.store.MoveUploaded(ctx, p.machine.ID, id, n)
	if err != nil {
		s.removeBundle(id)
		s.storeError(w, err)
		return
	}
	s.control.notify(mv.Target)
	writeJSON(w, http.StatusOK, mv)
}

// getMoveBundle is GET /v1/moves/{id}/bundle: the target streams the
// sealed bundle.
func (s *Server) getMoveBundle(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := moveID(w, r)
	if !ok {
		return
	}
	if s.moveDir == "" {
		writeError(w, http.StatusServiceUnavailable, "this server has no bundle directory")
		return
	}
	ctx := r.Context()
	s.sweepMoves(ctx)
	if _, err := s.store.MoveForDownload(ctx, p.machine.ID, id); err != nil {
		s.storeError(w, err)
		return
	}
	f, err := os.Open(s.bundlePath(id))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusGone, "the bundle is gone")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		s.internalError(w, err)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(bundleIOFor))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, f); err != nil {
		s.log.Printf("moves: send bundle %s: %v", id, err)
	}
}

// postMoveResult is POST /v1/moves/{id}/result. A move that ended loses its
// bundle, and its source's poll wakes for the finish.
func (s *Server) postMoveResult(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := moveID(w, r)
	if !ok {
		return
	}
	var in api.MoveResultIn
	if !decode(w, r, &in) {
		return
	}
	s.sweepMoves(r.Context())
	mv, err := s.store.FinishMove(r.Context(), p.machine.ID, id, in)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if !api.MoveOpen(mv.State) {
		s.removeBundle(id)
	}
	s.control.notify(mv.Source)
	writeJSON(w, http.StatusOK, mv)
}
```

- [ ] **Step 4: Register the routes and the body limit**

In `internal/server/routes.go`, replace:

```go
	accessApprove      = "approve"      // machine token, or the session cookie with X-Hub-Action: approve
)
```

with:

```go
	accessApprove      = "approve"      // machine token, or the session cookie with X-Hub-Action: approve
	accessMove         = "move"         // machine token, or the session cookie with X-Hub-Action: move
)
```

replace:

```go
		{"POST", "/v1/permissions/{id}/decide", accessApprove, s.actor(api.HeaderActionApprove, s.decidePermission)},
```

with:

```go
		{"POST", "/v1/permissions/{id}/decide", accessApprove, s.actor(api.HeaderActionApprove, s.decidePermission)},
		{"POST", "/v1/sessions/{id}/move", accessMove, s.actor(api.HeaderActionMove, s.postMove)},
		{"PUT", "/v1/moves/{id}/bundle", accessWrite, s.writer(s.putMoveBundle)},
		{"GET", "/v1/moves/{id}/bundle", accessWrite, s.writer(s.getMoveBundle)},
		{"POST", "/v1/moves/{id}/result", accessWrite, s.writer(s.postMoveResult)},
		{"PUT", "/v1/machines/self/move-key", accessWrite, s.writer(s.putMoveKey)},
```

and replace:

```go
		{"GET", "/v1/messages/{id}", accessRead, s.reader(s.getMessage)},
```

with:

```go
		{"GET", "/v1/messages/{id}", accessRead, s.reader(s.getMessage)},
		{"GET", "/v1/moves/{id}", accessRead, s.reader(s.getMove)},
```

In `internal/server/server.go`, replace:

```go
	// ruleAdded is called with each stored rule; it must not block. Run sets
	// it to the Telegram notifier when alerts are on.
	ruleAdded func(api.Instruction)
}
```

with:

```go
	// ruleAdded is called with each stored rule; it must not block. Run sets
	// it to the Telegram notifier when alerts are on.
	ruleAdded func(api.Instruction)
	// moveDir holds sealed move bundles; "" turns the bundle routes off.
	moveDir string
}
```

and replace:

```go
		r.Body = http.MaxBytesReader(sw, r.Body, MaxBodyBytes)
```

with:

```go
		limit := int64(MaxBodyBytes)
		if isBundleUpload(r) {
			limit = maxBundleBytes // the handler checks the caller first
		}
		r.Body = http.MaxBytesReader(sw, r.Body, limit)
```

In `internal/server/control.go`, replace:

```go
		if err == nil && !ok {
			claim, ok, err = s.store.ClaimMessage(r.Context(), p.machine.ID)
		}
```

with:

```go
		if err == nil && !ok {
			claim, ok, err = s.store.ClaimMessage(r.Context(), p.machine.ID)
		}
		if err == nil && !ok {
			claim, ok, err = s.store.ClaimMove(r.Context(), p.machine.ID)
		}
```

In `internal/server/run.go`, add `"path/filepath"` to the imports, replace:

```go
	sessionhub := New(st, cfg.PublicURL, logger)
```

with:

```go
	sessionhub := New(st, cfg.PublicURL, logger)
	sessionhub.SetMoveDir(filepath.Join(filepath.Dir(cfg.DB), "moves"))
```

and replace:

```go
	sessionhub.ruleAdded = onRule
	defer stopAlerts()
```

with:

```go
	sessionhub.ruleAdded = onRule
	defer stopAlerts()
	go sessionhub.runMoveSweeper(ctx)
```

In `internal/server/machine.go`, replace:

```go
	fmt.Fprintln(tw, "NAME\tSSH_HOST\tHERDR_HOST\tLAST_SEEN")
	for _, m := range list {
		seen := "never"
		if m.LastSeen != nil {
			seen = m.LastSeen.Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.Name, m.SSHHost, m.HerdrHost, seen)
	}
```

with:

```go
	fmt.Fprintln(tw, "NAME\tSSH_HOST\tHERDR_HOST\tLAST_SEEN\tMOVE_KEY")
	for _, m := range list {
		seen := "never"
		if m.LastSeen != nil {
			seen = m.LastSeen.Format(time.RFC3339)
		}
		key := m.MoveKey
		if key == "" {
			key = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.Name, m.SSHHost, m.HerdrHost, seen, key)
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/server/ -count=1`
Expected: PASS. `TestAuthMatrix` logs `auth matrix: 39 routes x 15
credentials, 0 failures`. If the run fails with `bind: address already in
use`, wait 60 seconds and run it again.

- [ ] **Step 6: Document the routes**

In `docs/server.md`, section "Authentication", replace:

```markdown
  `X-Hub-Action: send`, `POST /v1/permissions/{id}/decide` with
  `X-Hub-Action: approve`, and
```

with:

```markdown
  `X-Hub-Action: send`, `POST /v1/permissions/{id}/decide` with
  `X-Hub-Action: approve`, `POST /v1/sessions/{id}/move` with
  `X-Hub-Action: move`, and
```

In the "Endpoints" table, after the `POST /v1/permissions/{id}/decide` row,
add:

```markdown
| `POST /v1/sessions/{id}/move` | machine, or cookie with `X-Hub-Action: move` | `202`, the `Move` | Move the session to a machine or to `cloud` (`MoveIn`). `409` with the reason when a rule fails. See [Moves](#moves). |
| `GET /v1/moves/{id}` | read | `200`, the `Move` | One move and its state. |
| `PUT /v1/moves/{id}/bundle` | machine (the move's source, while `packing`) | `200`, the `Move` | Upload the sealed bundle (`application/octet-stream`, at most 64 MiB; `413` above). |
| `GET /v1/moves/{id}/bundle` | machine (the move's target, while `unpacking`) | `200`, the bundle | Download the sealed bundle. `410` when its file is gone. |
| `POST /v1/moves/{id}/result` | machine (the source while `packing`, the target while `unpacking`; the source again once the move ended and its part closed) | `200`, the `Move` | Report `done` or `failed` (`MoveResultIn`). After the move ended, the source's result with the final state adds its `detail` as a note. |
| `PUT /v1/machines/self/move-key` | machine | `204` | Register this machine's move public key (`MoveKeyIn`). |
```

After the "### Permission requests" section (before "### Inbox"), add:

```markdown
### Moves

A move takes a session, with its conversation, branch, and uncommitted
changes, to another machine, or hands it to a Claude Code cloud session. The
watchers do the work; the server checks the rules, relays a sealed bundle it
cannot read, and hands the session to the target. The design is in
`docs/dev/superpowers/specs/2026-10-03-move-session-design.md`.

Each watcher registers its X25519 public key with
`PUT /v1/machines/self/move-key` `{"public_key": "<base64>"}` when it starts.
`GET /v1/machines` shows each key's fingerprint as `move_key` and sets
`move_ready` when the machine has a key and its watcher polled in the last
2 minutes.

`POST /v1/sessions/{id}/move` `{"target": "tower"}` or `{"target": "cloud"}`
answers `202` with the move, or `409` with the reason when a rule fails: the
session has ended, is not in herdr, its agent is not `idle` or `done`, its
machine's watcher is offline or has no key, it already has an open move, the
target machine is unknown, the same machine, offline, or has no key, or a
cloud move's remote is not on GitHub.

States and who moves them:

| State | Set by | Next |
|---|---|---|
| `requested` | the request | the source's long poll claims it as `move-out` |
| `packing` | the source's claim | the source uploads (`uploaded`), or reports `failed`, or a cloud `done` with `cloud_url` |
| `uploaded` | the upload | the target's long poll claims it as `move-in` |
| `unpacking` | the target's claim | the target reports `done` or `failed` |
| `done`, `failed` | a result or a timeout | the source gets one more `move-out` claim, the finish, unless it posted the result itself |
| `cancelled` | nothing yet | treated like `failed` |

A move fails with detail `<state>: timed out` after 2 minutes in
`requested`, or 10 minutes in `packing`, `uploaded`, or `unpacking` since its
last change. The server checks on every move request and once a minute.

The claim's `move` field carries the move ID, the source and target names,
the state, and `peer_key`: the target's public key on `move-out`, the
source's on `move-in`. Each claim is handed out once.

On `done` for a machine move, the session's machine becomes the target, its
herdr fields and Remote Control link clear until the target's next
heartbeat, and a `moved` event records `move_id`, `from`, and `to`. Until
then the target's own writes for the session get `409`. On a cloud `done`,
the session ends with an `ended` event, reason `moved_to_cloud`, and a
`moved` event that carries `cloud_url`.

Once a move ended and its source's part is closed (the source posted its own
result, or claimed the finish), the source may post a result with the
move's final state and a `detail`: a restart or an archive that failed
there. The server appends the detail to the move's, with `; ` between, cut
to 300 characters, and changes nothing else, so `sessionhub move --status` and the
dashboard show it. Any other result on an ended move is `409`.

Bundles live in `moves/<id>.sealed` next to the database, mode `0600`. The
upload streams to a temp file and is renamed when complete. The server
deletes a bundle when its move ends, and any file in that directory older
than one hour. It only ever holds ciphertext.
```

In "Limits", replace:

```markdown
- A request body over 64 KiB returns `413`.
```

with:

```markdown
- A request body over 64 KiB returns `413`, except a move bundle
  (`PUT /v1/moves/{id}/bundle`), which may be 64 MiB. Uploads and downloads
  of a bundle may take 10 minutes; every other request has 30 seconds.
```

In "Field validation", after the `Control request ID` row, add:

```markdown
| Move ID | `^mv_[A-Za-z0-9_-]{22}$` |
| Move `target` | a machine name, or `cloud` |
| `public_key` | standard base64 of exactly 32 bytes |
| Move result `detail` | At most 300 characters, no control characters. |
| Move result `cloud_url` | `^https://claude\.ai/code/session_[A-Za-z0-9_-]+$`, on a cloud move's `done` only, where it is required. |
```

In "Manage machines", replace:

```markdown
- `ls` prints each machine's name, hosts, and when its token was last used.
```

with:

```markdown
- `ls` prints each machine's name, hosts, when its token was last used, and
  its move key's fingerprint (`-` before its watcher registers one). Compare
  it with `sessionhub move-key` on that machine.
```

- [ ] **Step 7: Lint and commit**

Run: `gofmt -w internal/server/*.go && make lint`
Expected: clean.

Run: `go test $(go list ./... | grep -v internal/server) -count=1`, then
`go test ./internal/server/ -count=1`
Expected: every package `ok`.

```bash
git add internal/server/moves.go internal/server/moves_test.go internal/server/routes.go internal/server/server.go \
  internal/server/control.go internal/server/run.go internal/server/machine.go internal/server/machine_test.go \
  internal/server/auth_test.go internal/server/helpers_test.go docs/server.md
git commit -m "$(cat <<'EOF'
Serve moves: rules, claims, sealed bundle relay, and keys

POST /v1/sessions/{id}/move checks the rules and wakes the source's long
poll; the source uploads a sealed bundle (64 MiB, streamed to a temp file
and renamed), the target downloads it and reports the result. Bundles are
files next to the database, deleted when the move ends or after an hour.
The auth matrix covers the six routes, the key, and the bundle files.

Refs: docs/dev/superpowers/plans/2026-10-03-move-session.md, Task 4

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 5: Client: move calls and streaming

**Files:**
- Create: `internal/client/move.go`
- Create: `internal/client/move_test.go`
- Modify: `internal/client/config.go` (`MoveRoots`, key `move_roots`)
- Modify: `internal/client/config_test.go` (a slice field makes `Config`
  not comparable with `==`)
- Modify: `internal/client/http.go` (`doTimeout` uses `authorize` and
  `responseError`)
- Modify: `docs/client.md` (`internal/client` methods and keys)

**Interfaces:**
- Consumes: `api.Move`, `api.MoveIn`, `api.MoveKeyIn`, `api.MoveResultIn`,
  `api.MaxSealedBundle`; `Client.do`, `StatusError`.
- Produces: the `internal/client/move.go` methods in the interface ledger,
  `BundleTimeout`, `Config.MoveRoots`; `authorize` and `responseError`, which
  `doTimeout` uses too, so every call sets the token and reads an error the
  same way.

- [ ] **Step 1: Write the failing client tests**

Create `internal/client/move_test.go`:

```go
package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestMoveCallsRequestShape(t *testing.T) {
	ctx := context.Background()
	c, log := server(t, 200, `{}`)
	for _, tc := range []struct {
		name, method, path, body string
		call                     func() error
	}{
		{"create", "POST", "/v1/sessions/s1/move", `{"target":"tower"}`,
			func() error { _, err := c.CreateMove(ctx, "s1", "tower"); return err }},
		{"get", "GET", "/v1/moves/mv_x", "", func() error { _, err := c.GetMove(ctx, "mv_x"); return err }},
		{"key", "PUT", "/v1/machines/self/move-key", `{"public_key":"k"}`, func() error { return c.PutMoveKey(ctx, "k") }},
		{"result", "POST", "/v1/moves/mv_x/result", `{"state":"failed","detail":"check pane: busy"}`,
			func() error {
				return c.PostMoveResult(ctx, "mv_x", api.MoveResultIn{State: api.MoveFailed, Detail: "check pane: busy"})
			}},
	} {
		*log = nil
		if err := tc.call(); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		g := (*log)[0]
		if g.Method != tc.method || g.Path != tc.path || string(g.Body) != tc.body || g.Auth != "Bearer hub_m_tok" {
			t.Errorf("%s: got %s %s body %s auth %q", tc.name, g.Method, g.Path, g.Body, g.Auth)
		}
	}
}

// bundleServer answers bundle requests and records the last one.
type bundleServer struct {
	*httptest.Server
	got           seen
	contentLength int64
	status        int
	body          []byte
	delay         time.Duration
}

func newBundleServer(t *testing.T) *bundleServer {
	b := &bundleServer{status: 200}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		b.got = seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), data}
		b.contentLength = r.ContentLength
		time.Sleep(b.delay)
		w.WriteHeader(b.status)
		w.Write(b.body)
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *bundleServer) client(t *testing.T) *Client {
	c, err := New(Config{ServerURL: b.URL, Token: "hub_m_tok"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPutMoveBundle(t *testing.T) {
	ctx := context.Background()
	b := newBundleServer(t)
	b.body = []byte(`{"id":"mv_x","state":"uploaded"}`)
	c := b.client(t)
	// The 2 s default limit does not apply to a bundle.
	c.SetTimeout(10 * time.Millisecond)
	b.delay = 50 * time.Millisecond
	data := bytes.Repeat([]byte{0x5A}, 300<<10)
	if err := c.PutMoveBundle(ctx, "mv_x", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if b.got.Method != "PUT" || b.got.Path != "/v1/moves/mv_x/bundle" || b.got.CT != "application/octet-stream" ||
		b.got.Auth != "Bearer hub_m_tok" || !bytes.Equal(b.got.Body, data) || b.contentLength != int64(len(data)) {
		t.Errorf("upload: %s %s %q %q, %d bytes, length %d", b.got.Method, b.got.Path, b.got.CT, b.got.Auth, len(b.got.Body), b.contentLength)
	}
	b.delay = 0
	b.status, b.body = 409, []byte(`{"error":"move mv_x is uploaded, not this machine's step"}`)
	var se *StatusError
	if err := c.PutMoveBundle(ctx, "mv_x", bytes.NewReader(data), int64(len(data))); !errors.As(err, &se) || se.Status != 409 ||
		se.Message != "move mv_x is uploaded, not this machine's step" {
		t.Errorf("409: %v", err)
	}
}

func TestGetMoveBundle(t *testing.T) {
	ctx := context.Background()
	b := newBundleServer(t)
	b.body = bytes.Repeat([]byte{0xC3}, 200<<10)
	c := b.client(t)
	c.SetTimeout(10 * time.Millisecond)
	b.delay = 50 * time.Millisecond
	var out bytes.Buffer
	n, err := c.GetMoveBundle(ctx, "mv_x", &out)
	if err != nil || n != int64(len(b.body)) || !bytes.Equal(out.Bytes(), b.body) || b.got.Path != "/v1/moves/mv_x/bundle" || b.got.Method != "GET" {
		t.Fatalf("download: %d bytes, %v; %s %s", n, err, b.got.Method, b.got.Path)
	}
	b.delay = 0
	b.status, b.body = 410, []byte(`{"error":"the bundle is gone"}`)
	var se *StatusError
	if _, err := c.GetMoveBundle(ctx, "mv_x", io.Discard); !errors.As(err, &se) || se.Status != 410 {
		t.Errorf("410: %v", err)
	}
	old := maxBundleDownload
	maxBundleDownload = 1024
	t.Cleanup(func() { maxBundleDownload = old })
	b.status, b.body = 200, bytes.Repeat([]byte("x"), 1025)
	if _, err := c.GetMoveBundle(ctx, "mv_x", io.Discard); err == nil {
		t.Error("an oversized bundle was accepted")
	}
}

func TestMoveRootsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("SESSIONHUB_CONFIG", path)
	if err := os.WriteFile(path, []byte("server_url = \"https://sessionhub.example.test\"\nmove_roots = [\"~/src\", \"/srv/code\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil || !slices.Equal(cfg.MoveRoots, []string{"~/src", "/srv/code"}) {
		t.Errorf("move_roots: %q %v", cfg.MoveRoots, err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/client/ -count=1`
Expected: FAIL to compile with `c.CreateMove undefined`, `undefined:
maxBundleDownload`, and `cfg.MoveRoots undefined`.

- [ ] **Step 3: Write the client calls**

Create `internal/client/move.go`:

```go
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// BundleTimeout bounds one bundle upload or download. It replaces the
// per-request limit, which a 64 MiB transfer through the tunnel would pass.
const BundleTimeout = 10 * time.Minute

// maxBundleDownload caps a downloaded bundle. Tests lower it.
var maxBundleDownload int64 = api.MaxSealedBundle

// CreateMove asks the sessionhub to move session sessionID to target: a machine
// name or api.MoveCloud.
func (c *Client) CreateMove(ctx context.Context, sessionID, target string) (api.Move, error) {
	var out api.Move
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/move", api.MoveIn{Target: target}, &out)
	return out, err
}

// GetMove reads one move.
func (c *Client) GetMove(ctx context.Context, id string) (api.Move, error) {
	var out api.Move
	err := c.do(ctx, http.MethodGet, "/v1/moves/"+url.PathEscape(id), nil, &out)
	return out, err
}

// PutMoveKey registers this machine's move public key.
func (c *Client) PutMoveKey(ctx context.Context, publicKey string) error {
	return c.do(ctx, http.MethodPut, "/v1/machines/self/move-key", api.MoveKeyIn{PublicKey: publicKey}, nil)
}

// PostMoveResult reports this machine's step of a move: done or failed.
func (c *Client) PostMoveResult(ctx context.Context, id string, in api.MoveResultIn) error {
	return c.do(ctx, http.MethodPost, "/v1/moves/"+url.PathEscape(id)+"/result", in, nil)
}

// PutMoveBundle streams a sealed bundle of size bytes from r.
func (c *Client) PutMoveBundle(ctx context.Context, id string, r io.Reader, size int64) error {
	ctx, cancel := context.WithTimeout(ctx, BundleTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/v1/moves/"+url.PathEscape(id)+"/bundle", r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return responseError(resp)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return nil
}

// GetMoveBundle streams a sealed bundle into w and returns its size. A
// bundle over api.MaxSealedBundle is an error.
func (c *Client) GetMoveBundle(ctx context.Context, id string, w io.Writer) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, BundleTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/moves/"+url.PathEscape(id)+"/bundle", nil)
	if err != nil {
		return 0, err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, responseError(resp)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxBundleDownload+1))
	if err != nil {
		return n, err
	}
	if n > maxBundleDownload {
		return n, fmt.Errorf("sessionhub: the bundle is larger than %d bytes", maxBundleDownload)
	}
	return n, nil
}

func (c *Client) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// responseError reads a non-2xx response as a *StatusError, like do.
func responseError(resp *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return errors.Join(&StatusError{Status: resp.StatusCode}, err)
	}
	msg := strings.TrimSpace(string(data))
	var e api.Error
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	return &StatusError{Status: resp.StatusCode, Message: msg}
}
```

In `internal/client/config.go`, replace:

```go
// Config is the client config. File keys: server_url, token, machine, and
// remote_permissions. Env overrides: SESSIONHUB_SERVER_URL, SESSIONHUB_TOKEN, SESSIONHUB_MACHINE.
type Config struct {
	ServerURL string `toml:"server_url"`
	Token     string `toml:"token"`
	Machine   string `toml:"machine"`
	// RemotePermissions = false turns off remote permission answers on this
	// machine. Unset means on.
	RemotePermissions *bool `toml:"remote_permissions,omitempty"`
}
```

with:

```go
// Config is the client config. File keys: server_url, token, machine,
// remote_permissions, and move_roots. Env overrides: SESSIONHUB_SERVER_URL,
// SESSIONHUB_TOKEN, SESSIONHUB_MACHINE.
type Config struct {
	ServerURL string `toml:"server_url"`
	Token     string `toml:"token"`
	Machine   string `toml:"machine"`
	// RemotePermissions = false turns off remote permission answers on this
	// machine. Unset means on.
	RemotePermissions *bool `toml:"remote_permissions,omitempty"`
	// MoveRoots are directories, besides ~/Code, where a moved session's
	// clone is searched for. Absolute or starting with ~/.
	MoveRoots []string `toml:"move_roots,omitempty"`
}
```

`Config` now holds a slice, so `==` on it no longer compiles. In
`internal/client/config_test.go`, add `"reflect"` to the imports, replace:

```go
	if c, err := LoadConfig(); err != nil || c != (Config{}) {
```

with:

```go
	if c, err := LoadConfig(); err != nil || !reflect.DeepEqual(c, Config{}) {
```

replace:

```go
	if err != nil || got != want {
```

with:

```go
	if err != nil || !reflect.DeepEqual(got, want) {
```

and replace:

```go
	if c != want {
```

with:

```go
	if !reflect.DeepEqual(c, want) {
```

Run `go vet ./...` to confirm no other package compares a `client.Config`
with `==`.

`doTimeout` and the bundle calls now share the token header and the error
reading. In `internal/client/http.go`, replace:

```go
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(data))
		var e api.Error
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &StatusError{Status: resp.StatusCode, Message: msg}
	}
```

with:

```go
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return responseError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
```

An error body is now read up to 64 KiB, enough for any `api.Error`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/client/ -count=1 -race`
Expected: PASS. The bundle tests pass with a 10 ms default limit and a 50 ms
server, so the bundle calls use `BundleTimeout`. The existing `http_test.go`
and `autherror_test.go` tests pass unchanged through the shared
`responseError`.

- [ ] **Step 5: Document the calls**

In `docs/client.md`, section "`internal/client`", replace:

```markdown
  parent directories. Keys: `server_url`, `token`, `machine`, and
  `remote_permissions` (unset means on; `false` turns off remote permission
  answers on this machine). `RemotePermissionsOff(cfg, os.Getenv)` is also
  true when `SESSIONHUB_REMOTE_PERMISSIONS=off`.
```

with:

```markdown
  parent directories. Keys: `server_url`, `token`, `machine`,
  `remote_permissions` (unset means on; `false` turns off remote permission
  answers on this machine), and `move_roots` (directories besides `~/Code`
  where a moved session's clone is searched for, absolute or starting with
  `~/`). `RemotePermissionsOff(cfg, os.Getenv)` is also true when
  `SESSIONHUB_REMOTE_PERMISSIONS=off`.
```

and after the bullet that starts "- `PollControl(ctx, wait)` holds the
control long poll open", add:

```markdown
- Moves: `CreateMove(sessionID, target)`, `GetMove(id)`,
  `PutMoveKey(publicKey)`, and `PostMoveResult(id, api.MoveResultIn)` are
  ordinary calls. `PutMoveBundle(id, reader, size)` streams a sealed bundle
  with `Content-Length` and `application/octet-stream`;
  `GetMoveBundle(id, writer)` streams one back and refuses more than 64 MiB.
  Both use `BundleTimeout` (10 minutes) instead of the per-request limit and
  return `*StatusError` for a non-2xx answer.
```

- [ ] **Step 6: Lint and commit**

Run: `gofmt -w internal/client/*.go && make lint`
Expected: clean.

```bash
git add internal/client/move.go internal/client/move_test.go internal/client/config.go internal/client/config_test.go \
  internal/client/http.go docs/client.md
git commit -m "$(cat <<'EOF'
Add client calls for moves and bundle streaming

The bundle upload and download stream with their own 10-minute limit
instead of the 2-second default, and the download refuses more than
64 MiB. Every call now sets the token and reads an error body through
the same two helpers. move_roots in the client config adds clone search
roots.

Refs: docs/dev/superpowers/plans/2026-10-03-move-session.md, Task 5

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 6: Resume: start a plain resume

**Files:**
- Modify: `internal/resume/control.go` (`OutcomeRunning`, `StartResumed`,
  `startInWorkspace` split out of `resumeInWorkspace`)
- Create: `internal/resume/start_test.go`

**Interfaces:**
- Consumes: `validID`, `validPaneID`, `runs`, `paneGet`, `checkedPane`,
  `agentName`, `waitForClaude`, `lastLine`, `waitForLink`, `failure`,
  `clean`; test helpers `newCtlScript`, `(*ctlScript).herdr`, `paneOf`,
  `sess`, `uuid`, `fastControl`, `(*fakeHerdr).methods`,
  `(*fakeHerdr).params`.
- Produces: `OutcomeRunning`,
  `func StartResumed(ctx context.Context, socket string, s api.Session, o ControlOptions) ControlResult`.

No component doc changes: `StartResumed` is internal, and Task 7 documents
where the watcher uses it.

- [ ] **Step 1: Write the failing tests**

Create `internal/resume/start_test.go`:

```go
package resume

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

func TestStartResumed(t *testing.T) {
	dir := t.TempDir()
	at := func(cwd, herdrSession string) api.Session {
		s := sess("tower", "w1:p1")
		s.CWD, s.HerdrSession = cwd, herdrSession
		return s
	}
	cases := []struct {
		name     string
		sess     api.Session
		script   func(c *ctlScript)
		noHerdr  bool
		want     Outcome
		wantPane string
		errHas   string
		calls    []string
		never    []string
		noRecGet bool // pane.get must not look up the recorded pane w1:p1
	}{
		{name: "no pane runs it and its pane is gone: a new workspace",
			sess: at(dir, "default"), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeResumed, wantPane: "w9:p1",
			calls: []string{"session.snapshot", "workspace.create", "agent.start"}, never: []string{"agent.prompt", "pane.read"}},
		{name: "its pane sits at a shell: start there",
			sess: at(dir, "default"), script: func(c *ctlScript) { c.agent = ""; c.rootPane = "w1:p1" },
			want: OutcomeResumed, wantPane: "w1:p1",
			calls: []string{"session.snapshot", "pane.get", "agent.start"}, never: []string{"workspace.create", "agent.prompt"}},
		{name: "its pane runs another program: a new workspace",
			sess: at(dir, "default"), script: func(c *ctlScript) { c.agent = "codex" },
			want: OutcomeResumed, wantPane: "w9:p1", calls: []string{"workspace.create", "agent.start"}},
		{name: "its pane belongs to another herdr server: a new workspace, no lookup",
			sess: at(dir, "other"), script: func(c *ctlScript) { c.agent = "" },
			want: OutcomeResumed, wantPane: "w9:p1", calls: []string{"workspace.create"}, noRecGet: true},
		{name: "a pane already runs it: start nothing",
			sess: at(dir, "default"),
			script: func(c *ctlScript) {
				c.pane = ""
				c.snapPanes = []map[string]any{paneOf("w2:p3", uuid, "claude")}
			},
			want: OutcomeRunning, wantPane: "w2:p3", never: []string{"workspace.create", "agent.start"}},
		{name: "the directory is gone: create nothing",
			sess: at(filepath.Join(dir, "gone"), "default"), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeNoDir, never: []string{"workspace.create", "agent.start"}},
		{name: "herdr is not running",
			sess: at(dir, "default"), noHerdr: true, want: OutcomeError, errHas: "herdr is not running"},
		{name: "claude never appears: an error, no second start",
			sess: at(dir, "default"), script: func(c *ctlScript) { c.pane = ""; c.claudeOnGet = 1000 },
			want: OutcomeError, wantPane: "w9:p1", errHas: "didn't start"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCtlScript(t)
			if tc.script != nil {
				tc.script(c)
			}
			fh := c.herdr(t)
			sock := fh.path
			if tc.noHerdr {
				sock = filepath.Join(t.TempDir(), "none.sock")
			}
			r := StartResumed(context.Background(), sock, tc.sess, fastControl(sock))
			if r.Outcome != tc.want || (tc.wantPane != "" && r.Pane != tc.wantPane) {
				t.Fatalf("got %s pane %q err %v; want %s pane %q", r.Outcome, r.Pane, r.Err, tc.want, tc.wantPane)
			}
			if tc.errHas != "" && (r.Err == nil || !strings.Contains(r.Err.Error(), tc.errHas)) {
				t.Errorf("err %v, want %q", r.Err, tc.errHas)
			}
			if tc.noHerdr {
				return
			}
			m := fh.methods()
			for _, want := range tc.calls {
				if !slices.Contains(m, want) {
					t.Errorf("%s not called; calls %v", want, m)
				}
			}
			for _, bad := range tc.never {
				if slices.Contains(m, bad) {
					t.Errorf("%s called; calls %v", bad, m)
				}
			}
			if tc.noRecGet && c.recGets != 0 {
				t.Errorf("looked up the recorded pane %d times", c.recGets)
			}
			if n := countOf(m, "agent.start"); n > 1 {
				t.Errorf("agent.start called %d times", n)
			}
			if p := fh.params("agent.start"); p != nil {
				args, _ := p["args"].([]any)
				if len(args) != 2 || args[0] != "--resume" || args[1] != uuid {
					t.Errorf("agent.start args %v, want --resume %s and nothing else", args, uuid)
				}
			}
			if p := fh.params("workspace.create"); p != nil && p["focus"] != false {
				t.Errorf("workspace.create takes focus: %v", p)
			}
		})
	}
}

func countOf(list []string, v string) int {
	n := 0
	for _, x := range list {
		if x == v {
			n++
		}
	}
	return n
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/resume/ -run TestStartResumed -count=1`
Expected: FAIL to compile with `undefined: StartResumed` and `undefined:
OutcomeRunning`.

- [ ] **Step 3: Split the workspace start and add StartResumed**

In `internal/resume/control.go`, replace:

```go
	// OutcomeJustResumed: KnownPane neither runs the session nor Claude
	// waiting to report it, and no other pane runs it; nothing started.
	OutcomeJustResumed Outcome = "just_resumed"
)
```

with:

```go
	// OutcomeJustResumed: KnownPane neither runs the session nor Claude
	// waiting to report it, and no other pane runs it; nothing started.
	OutcomeJustResumed Outcome = "just_resumed"
	// OutcomeRunning: StartResumed found a pane that already runs the
	// session; nothing started.
	OutcomeRunning Outcome = "running"
)
```

replace:

```go
// resumeInWorkspace starts `claude --resume <id> --remote-control` in a new
// herdr workspace in the session's directory, without focus, and waits for
// the link. A directory that is missing, not a directory, or not absolute
// creates nothing: the server doesn't check the recorded path, and a relative
// one would resolve against herdr's own directory.
func resumeInWorkspace(ctx context.Context, h *herdr.Client, s api.Session, o ControlOptions) ControlResult {
	if !filepath.IsAbs(s.CWD) {
```

with:

```go
// resumeInWorkspace starts `claude --resume <id> --remote-control` in a new
// herdr workspace in the session's directory, without focus, and waits for
// the link.
func resumeInWorkspace(ctx context.Context, h *herdr.Client, s api.Session, o ControlOptions) ControlResult {
	r := startInWorkspace(ctx, h, s, o, remoteControlArgs(s.ID))
	if r.Outcome != OutcomeResumed {
		return r
	}
	r.URL = waitForLink(ctx, h, r.Pane, o)
	return r
}

// StartResumed starts `claude --resume <id>` for session s on this machine,
// without Remote Control and without focus, and waits until herdr detects
// Claude in the pane. It never starts a second copy: when a pane already
// runs s, it returns OutcomeRunning. It starts in s's recorded pane when that
// pane is on this herdr server and sits at a shell, else in a new workspace
// at s.CWD (a missing or relative directory is OutcomeNoDir). It ignores the
// server's status: the watcher calls it for a session it just ended or just
// wrote, which the server may still show as live.
func StartResumed(ctx context.Context, socket string, s api.Session, o ControlOptions) ControlResult {
	if !validID(s.ID) {
		return failure(fmt.Errorf("refusing session ID %q: expected letters, digits, and hyphens, not starting with a hyphen", s.ID))
	}
	if s.HerdrPane != "" && !validPaneID.MatchString(s.HerdrPane) {
		return failure(fmt.Errorf("refusing pane ID %q: not a herdr pane ID", clean(s.HerdrPane)))
	}
	h, err := herdr.Dial(socket)
	if err != nil {
		return failure(fmt.Errorf("herdr is not running: %w", err))
	}
	snap, err := h.Snapshot()
	if err != nil {
		return failure(fmt.Errorf("read herdr snapshot: %w", err))
	}
	for _, p := range snap.Panes {
		if runs(p, s.ID) {
			pane, err := checkedPane(p.PaneID)
			if err != nil {
				return failure(err)
			}
			return ControlResult{Outcome: OutcomeRunning, Pane: pane}
		}
	}
	args := []string{"--resume", s.ID}
	if s.HerdrPane != "" && (s.HerdrSession == "" || s.HerdrSession == herdr.SessionName(socket)) {
		p, found, err := paneGet(h, s.HerdrPane)
		if err != nil {
			return failure(err)
		}
		if found && p.Agent == "" {
			pane, err := checkedPane(p.PaneID)
			if err != nil {
				return failure(err)
			}
			r := ControlResult{Pane: pane}
			if _, err := h.AgentStart(herdr.AgentStartParams{Name: agentName(s.ID), Kind: "claude", PaneID: pane, Args: args}); err != nil {
				r.Outcome, r.Err = OutcomeError, fmt.Errorf("start claude in pane %s: %w", clean(pane), err)
				return r
			}
			if err := waitForClaude(ctx, h, pane, o); err != nil {
				r.Outcome, r.Err = OutcomeError, fmt.Errorf("claude didn't start in pane %s: %s", clean(pane), lastLine(h, pane))
				return r
			}
			r.Outcome = OutcomeResumed
			return r
		}
	}
	return startInWorkspace(ctx, h, s, o, args)
}

// startInWorkspace starts claude with args in a new herdr workspace in the
// session's directory, without focus, and waits until herdr detects Claude
// there. A directory that is missing, not a directory, or not absolute
// creates nothing: the server doesn't check the recorded path, and a relative
// one would resolve against herdr's own directory.
func startInWorkspace(ctx context.Context, h *herdr.Client, s api.Session, o ControlOptions, args []string) ControlResult {
	if !filepath.IsAbs(s.CWD) {
```

and replace:

```go
	if _, err := h.AgentStart(herdr.AgentStartParams{
		Name:   agentName(s.ID),
		Kind:   "claude",
		PaneID: r.Pane,
		Args:   remoteControlArgs(s.ID),
	}); err != nil {
```

with:

```go
	if _, err := h.AgentStart(herdr.AgentStartParams{
		Name:   agentName(s.ID),
		Kind:   "claude",
		PaneID: r.Pane,
		Args:   args,
	}); err != nil {
```

and replace:

```go
	r.Outcome = OutcomeResumed
	r.URL = waitForLink(ctx, h, r.Pane, o)
	return r
}
```

with:

```go
	r.Outcome = OutcomeResumed
	return r
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/resume/ -count=1`
Expected: PASS: `TestStartResumed` and every existing `Control` test,
whose behavior did not change.

- [ ] **Step 5: Lint and commit**

Run: `gofmt -w internal/resume/control.go internal/resume/start_test.go && make lint`
Expected: clean.

```bash
git add internal/resume/control.go internal/resume/start_test.go
git commit -m "$(cat <<'EOF'
Start a plain claude --resume in herdr for the watcher

StartResumed reuses the Remote Control resume's workspace start without
--remote-control, starts in the session's own pane when it sits at a
shell, and does nothing when a pane already runs the session. The move
target starts a moved session with it, and the source restarts one after
a failed move.

Refs: docs/dev/superpowers/plans/2026-10-03-move-session.md, Task 6

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 7: Watcher: key registration, move-out, and finish

**Files:**
- Create: `internal/move/source.go` (`PushBranch`, `ClaudeVersion`,
  `Archive`)
- Create: `internal/move/source_test.go`
- Create: `internal/plugin/move.go` (`mover`, `newMover`, `handle`,
  `moveOut`, `finish`, and the herdr steps)
- Create: `internal/plugin/move_helpers_test.go` (temp Git repositories, a
  fake `claude`, a scripted herdr pane, and a real sessionhub with two machines)
- Create: `internal/plugin/move_test.go`
- Modify: `internal/plugin/control.go` (`controller.move`, move claims in
  `handle`)
- Modify: `internal/plugin/watcher.go` (`moveKey`, `moveKeyAt`,
  `registerMoveKey`; `runWatcher` loads the key and wires the mover)
- Modify: `internal/plugin/watcher_test.go` (the `runWatcher` tests set
  `SESSIONHUB_CONFIG` to a temp dir, so the key they create never lands in the real
  `~/.config/sessionhub`)
- Modify: `docs/plugin.md` (section "Moves", "Files in the state dir")
- Modify: `docs/client.md` (section "Moves")

**Interfaces:**
- Consumes: `move.ReadRepo`, `move.Build`, `move.Seal`, `move.ParsePublicKey`,
  `move.IsGitHub`, `move.LoadOrCreateKey`, `move.KeyPath`,
  `move.PublicKeyString`, `move.DefaultRoots`, `move.FindTranscripts` (Tasks
  1, 2); `client.PutMoveBundle`, `client.PostMoveResult`, `client.GetMove`,
  `client.PutMoveKey`, `client.LoadConfig` (Task 5); `resume.StartResumed`
  (Task 6); `digest.DefaultClaudeDir`; the `detail*` constants in
  `control.go`.
- Produces: `move.PushBranch`, `move.ClaudeVersion`, `move.Archive`;
  `type mover`, `newMover(socket, claudeDir, stateDir string, roots []string,
  key *ecdh.PrivateKey, logger *log.Logger) *mover`, `(*mover).handle`;
  `controller.move`; `(*mover).postResult` (reports whether the sessionhub recorded
  the result), `abort`, `restart`, `endIdle`, `detailResumes`; test helpers
  `gitIsolate`, `gitT`, `writeFile`, `newGitRepo`, `fakeClaude`, `orderLog`,
  `movePane` (`busyAfter`), `newMovePane`, `moveHub`, `newMoveHub`,
  `(*moveHub).setIntercept`, `isFrom`, `badGateway`, `moveEnv`, `newMoveEnv`,
  `moveSID`.

- [ ] **Step 1: Write the failing source-side Git tests**

Create `internal/move/source_test.go`:

```go
package move

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude writes an executable script named claude and returns its path.
func fakeClaude(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	writeFile(t, p, "#!/bin/sh\n"+script+"\n", 0o755)
	return p
}

func TestPushBranch(t *testing.T) {
	ctx := context.Background()
	origin, work := newRepo(t)
	r, err := ReadRepo(ctx, work, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := PushBranch(ctx, r); err != nil {
		t.Fatalf("up to date: %v", err)
	}
	writeFile(t, filepath.Join(work, "a.txt"), "two\n", 0o644)
	gitT(t, work, "commit", "-qam", "ahead")
	if err := PushBranch(ctx, r); err != nil {
		t.Fatal(err)
	}
	if got, want := gitT(t, origin, "rev-parse", "main"), gitT(t, work, "rev-parse", "HEAD"); got != want {
		t.Errorf("origin main %s, local %s", got, want)
	}
	// A branch without an upstream is pushed and gets one.
	gitT(t, work, "checkout", "-q", "-b", "feature/x")
	writeFile(t, filepath.Join(work, "f.txt"), "f\n", 0o644)
	gitT(t, work, "add", "f.txt")
	gitT(t, work, "commit", "-qm", "feature")
	r.Branch = "feature/x"
	if err := PushBranch(ctx, r); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, origin, "rev-parse", "feature/x"); got != gitT(t, work, "rev-parse", "HEAD") {
		t.Errorf("origin feature/x = %s", got)
	}
	if up := gitT(t, work, "rev-parse", "--abbrev-ref", "@{u}"); up != "origin/feature/x" {
		t.Errorf("upstream %q", up)
	}
	// A push that fails is an error that names it; the branch is untouched.
	writeFile(t, filepath.Join(work, "g.txt"), "g\n", 0o644)
	gitT(t, work, "add", "g.txt")
	gitT(t, work, "commit", "-qm", "more")
	head := gitT(t, work, "rev-parse", "HEAD")
	gitT(t, work, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	if err := PushBranch(ctx, r); err == nil || !strings.Contains(err.Error(), "git push") {
		t.Errorf("push to a missing remote: %v", err)
	}
	if gitT(t, work, "rev-parse", "HEAD") != head {
		t.Error("a failed push moved HEAD")
	}
}

func TestClaudeVersion(t *testing.T) {
	ctx := context.Background()
	v, err := ClaudeVersion(ctx, fakeClaude(t, `[ "$1" = --version ] && echo "2.1.285 (Claude Code)"`))
	if err != nil || v != "2.1.285" {
		t.Errorf("version %q %v", v, err)
	}
	if _, err := ClaudeVersion(ctx, fakeClaude(t, "exit 3")); err == nil {
		t.Error("a failing claude gave a version")
	}
	if _, err := ClaudeVersion(ctx, fakeClaude(t, "true")); err == nil {
		t.Error("an empty version was accepted")
	}
	if _, err := ClaudeVersion(ctx, filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("a missing claude gave a version")
	}
}

func TestArchive(t *testing.T) {
	claude := newClaudeDir(t, testSessionID)
	state := t.TempDir()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	old := filepath.Join(state, "moved", "mv_old")
	writeFile(t, filepath.Join(old, "transcript", "x.jsonl"), "x", 0o600)
	recent := filepath.Join(state, "moved", "mv_recent")
	writeFile(t, filepath.Join(recent, "x"), "x", 0o600)
	// Every age counts from the fixed now, never from the clock.
	for p, age := range map[string]time.Duration{old: 31 * 24 * time.Hour, recent: 24 * time.Hour} {
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}

	if err := Archive(claude, state, testSessionID, "mv_AAAAAAAAAAAAAAAAAAAAAA", now); err != nil {
		t.Fatal(err)
	}
	if ts, _ := FindTranscripts(claude, testSessionID); len(ts) != 0 {
		t.Errorf("transcript still resumable: %v", ts)
	}
	dest := filepath.Join(state, "moved", "mv_AAAAAAAAAAAAAAAAAAAAAA")
	for _, p := range []string{
		"transcript/-home-user-proj/" + testSessionID + ".jsonl",
		"transcript/-home-user-proj/" + testSessionID + "/subagents/agent-1.jsonl",
		"file-history/" + testSessionID + "/abc@v1",
	} {
		if _, err := os.Stat(filepath.Join(dest, p)); err != nil {
			t.Errorf("archive lacks %s: %v", p, err)
		}
	}
	for _, p := range []string{filepath.Join(claude, "projects", "-home-user-proj", testSessionID), filepath.Join(claude, "file-history", testSessionID)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", p, err)
		}
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("a 31-day-old archive was kept: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("a recent archive was removed: %v", err)
	}
	// Archiving a session with no transcript left is not an error.
	if err := Archive(claude, state, testSessionID, "mv_BBBBBBBBBBBBBBBBBBBBBB", now); err != nil {
		t.Errorf("second archive: %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/move/ -count=1`
Expected: FAIL to compile with `undefined: PushBranch`, `undefined:
ClaudeVersion`, and `undefined: Archive`.

- [ ] **Step 3: Write the source-side Git and file steps**

Create `internal/move/source.go`:

```go
package move

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// archiveKeep is how long ~/.local/state/sessionhub/moved/<id> folders stay.
const archiveKeep = 30 * 24 * time.Hour

// PushBranch makes sure the branch's commits are on the remote: with no
// upstream it runs git push -u origin <branch>; with one it pushes HEAD to
// the upstream branch when HEAD is ahead of it. It never forces.
func PushBranch(ctx context.Context, r Repo) error {
	remote, _ := git(ctx, r.Root, "config", "--get", "branch."+r.Branch+".remote")
	merge, _ := git(ctx, r.Root, "config", "--get", "branch."+r.Branch+".merge")
	if remote == "" || merge == "" {
		ref := "refs/heads/" + r.Branch
		_, err := gitRaw(ctx, r.Root, gitOpts{timeout: gitNetTimeout}, "push", "-u", "origin", ref+":"+ref)
		return err
	}
	if n, err := git(ctx, r.Root, "rev-list", "--count", "@{u}..HEAD"); err == nil && n == "0" {
		return nil
	}
	_, err := gitRaw(ctx, r.Root, gitOpts{timeout: gitNetTimeout}, "push", remote, "HEAD:"+merge)
	return err
}

// ClaudeVersion is the first field of `claude --version`, for example
// "2.1.285".
func ClaudeVersion(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("claude --version: %w", err)
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "", errors.New("claude --version printed nothing")
	}
	return f[0], nil
}

// Archive moves session id's transcript, sidecar folder, and file history
// out of claudeDir into <stateDir>/moved/<moveID>/, so `claude --resume` no
// longer finds the session here, and removes archive folders older than
// 30 days. A session with nothing left to move is not an error.
func Archive(claudeDir, stateDir, sessionID, moveID string, now time.Time) error {
	base := filepath.Join(stateDir, "moved")
	dest := filepath.Join(base, moveID)
	ts, err := FindTranscripts(claudeDir, sessionID)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range ts {
		into := filepath.Join(dest, "transcript", filepath.Base(filepath.Dir(t)))
		if err := os.MkdirAll(into, 0o700); err != nil {
			return err
		}
		errs = append(errs, os.Rename(t, filepath.Join(into, sessionID+".jsonl")))
		side := strings.TrimSuffix(t, ".jsonl")
		if _, err := os.Stat(side); err == nil {
			errs = append(errs, os.Rename(side, filepath.Join(into, sessionID)))
		}
	}
	fh := filepath.Join(claudeDir, "file-history", sessionID)
	if _, err := os.Stat(fh); err == nil {
		if err := os.MkdirAll(filepath.Join(dest, "file-history"), 0o700); err != nil {
			return err
		}
		errs = append(errs, os.Rename(fh, filepath.Join(dest, "file-history", sessionID)))
	}
	errs = append(errs, pruneArchive(base, now))
	return errors.Join(errs...)
}

// pruneArchive removes archive folders older than archiveKeep.
func pruneArchive(base string, now time.Time) error {
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() || now.Sub(info.ModTime()) <= archiveKeep {
			continue
		}
		errs = append(errs, os.RemoveAll(filepath.Join(base, e.Name())))
	}
	return errors.Join(errs...)
}
```

Run: `go test ./internal/move/ -count=1`
Expected: PASS.

- [ ] **Step 4: Write the plugin test helpers**

Create `internal/plugin/move_helpers_test.go`:

```go
package plugin

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
	"github.com/abdallah/session-hub/internal/move"
	"github.com/abdallah/session-hub/internal/resume"
	"github.com/abdallah/session-hub/internal/server"
	"github.com/abdallah/session-hub/internal/store"
)

// moveSID is the moved session.
const moveSID = "5e5e5e5e-1111-4222-8333-444455556666"

func gitIsolate(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Hub Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "sessionhub@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "Hub Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "sessionhub@example.test")
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// newGitRepo makes a bare origin under base and a clone of it at
// base/<name> with one pushed commit on main (a.txt).
func newGitRepo(t *testing.T, base, name string) (origin, work string) {
	t.Helper()
	gitIsolate(t)
	origin = filepath.Join(base, name+"-origin.git")
	gitT(t, base, "init", "-q", "--bare", "-b", "main", origin)
	work = filepath.Join(base, name)
	gitT(t, base, "clone", "-q", origin, work)
	writeFile(t, filepath.Join(work, "a.txt"), "one\n", 0o644)
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "-q", "-m", "initial")
	gitT(t, work, "push", "-q", "-u", "origin", "main")
	return origin, work
}

// fakeClaude puts an executable named claude first on PATH. script is the
// body of a shell script.
func fakeClaude(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "claude"), "#!/bin/sh\n"+script+"\n", 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// orderLog records the order of steps across herdr and the sessionhub.
type orderLog struct {
	mu    sync.Mutex
	items []string
}

func (o *orderLog) add(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.items = append(o.items, s)
}

func (o *orderLog) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.items...)
}

// movePane is a herdr with one pane, w1:p1, where Claude runs a session
// until it gets /exit and then sits at a shell until agent.start.
type movePane struct {
	mu        sync.Mutex
	session   string
	agent     string // "claude", or "" at a shell
	status    string
	exits     bool // false: /exit is typed but Claude stays
	busyAfter int  // > 0: the agent turns working after that many pane.get calls
	gets      int
	prompts   []string
	starts    [][]string
}

func newMovePane(t *testing.T, session, status string, order *orderLog) (*movePane, *herdrtest.Server) {
	t.Helper()
	p := &movePane{session: session, agent: "claude", status: status, exits: true}
	srv := herdrtest.New(t, "session-snapshot.ndjson")
	srv.Handle("pane.get", func(params json.RawMessage) (any, string) {
		var q struct {
			PaneID string `json:"pane_id"`
		}
		json.Unmarshal(params, &q)
		if q.PaneID != "w1:p1" {
			return nil, "pane_not_found"
		}
		p.mu.Lock()
		if p.gets++; p.busyAfter > 0 && p.gets > p.busyAfter {
			p.status = "working"
		}
		p.mu.Unlock()
		return map[string]any{"pane": p.info()}, ""
	})
	srv.Handle("session.snapshot", func(json.RawMessage) (any, string) {
		return map[string]any{"snapshot": map[string]any{"panes": []any{p.info()}}}, ""
	})
	srv.Handle("agent.prompt", func(params json.RawMessage) (any, string) {
		var q struct{ Target, Text string }
		json.Unmarshal(params, &q)
		p.mu.Lock()
		defer p.mu.Unlock()
		p.prompts = append(p.prompts, q.Text)
		if q.Text == "/exit" {
			order.add("exit")
			if p.exits {
				p.agent, p.status = "", ""
			}
		}
		return map[string]any{"type": "agent_prompted"}, ""
	})
	srv.Handle("agent.start", func(params json.RawMessage) (any, string) {
		var q struct {
			PaneID string   `json:"pane_id"`
			Args   []string `json:"args"`
		}
		json.Unmarshal(params, &q)
		p.mu.Lock()
		defer p.mu.Unlock()
		order.add("start")
		p.starts = append(p.starts, q.Args)
		p.agent, p.status = "claude", "idle"
		return map[string]any{"type": "agent_started"}, ""
	})
	return p, srv
}

func (p *movePane) info() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": p.status,
		"agent_session": map[string]any{"agent": "claude", "kind": "id", "source": "herdr:claude", "value": p.session}}
	if p.agent != "" {
		m["agent"] = p.agent
	}
	return m
}

func (p *movePane) snapshot() (prompts []string, starts [][]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts...), append([][]string(nil), p.starts...)
}

// hubMachine is one machine of a moveHub: its token, ID, and move key.
type hubMachine struct {
	tok string
	id  int64
	key *ecdh.PrivateKey
}

// moveHub is the real sessionhub server over a temp database with machines tower
// and bluebox, both with registered move keys and a recent poll. Every bundle
// upload adds "upload" to the order log. An intercept sees each request
// first and may answer it.
type moveHub struct {
	st        *store.Store
	srv       *httptest.Server
	moveDir   string
	tower, bluebox hubMachine
	mu        sync.Mutex
	intercept func(w http.ResponseWriter, r *http.Request) bool
}

// setIntercept makes f see every request before the sessionhub; f answers a
// request by returning true.
func (h *moveHub) setIntercept(f func(w http.ResponseWriter, r *http.Request) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.intercept = f
}

// isFrom reports whether r carries machine m's token.
func isFrom(r *http.Request, m hubMachine) bool {
	return r.Header.Get("Authorization") == "Bearer "+m.tok
}

// badGateway answers like a tunnel whose server is down.
func badGateway(w http.ResponseWriter) {
	http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
}

func newMoveHub(t *testing.T, order *orderLog) *moveHub {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sessionhub.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	h := &moveHub{st: st, moveDir: filepath.Join(dir, "moves")}
	for _, m := range []struct {
		name string
		dst  *hubMachine
	}{{"tower", &h.tower}, {"bluebox", &h.bluebox}} {
		tok, _, err := st.AddMachine(ctx, m.name, "", "")
		if err != nil {
			t.Fatal(err)
		}
		mm, err := st.MachineByToken(ctx, tok)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetMoveKey(ctx, mm.ID, move.PublicKeyString(key)); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordPoll(ctx, mm.ID); err != nil {
			t.Fatal(err)
		}
		*m.dst = hubMachine{tok: tok, id: mm.ID, key: key}
	}
	s := server.New(st, "https://sessionhub.example.test", nil)
	s.SetMoveDir(h.moveDir)
	handler := s.Handler()
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/bundle") {
			order.add("upload")
		}
		h.mu.Lock()
		f := h.intercept
		h.mu.Unlock()
		if f != nil && f(w, r) {
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *moveHub) client(t *testing.T, m hubMachine) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{ServerURL: h.srv.URL, Token: m.tok})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// claim polls once as m and fails unless a claim came.
func (h *moveHub) claim(t *testing.T, m hubMachine) api.ControlClaim {
	t.Helper()
	c, err := h.client(t, m).PollControl(context.Background(), time.Second)
	if err != nil || c == nil {
		t.Fatalf("poll: %v %v", c, err)
	}
	return *c
}

func (h *moveHub) move(t *testing.T, id string) api.Move {
	t.Helper()
	mv, err := h.st.GetMove(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return mv
}

// moveEnv is a session on tower in a temp Git repository, with a temp
// ~/.claude, a scripted herdr pane, a fake claude, the real sessionhub, and tower's
// mover.
type moveEnv struct {
	sessionhub    *moveHub
	order  *orderLog
	pane   *movePane
	herdr  *herdrtest.Server
	base   string // holds the repositories
	origin string
	work   string // the session's directory
	claude string // tower's ~/.claude
	state  string // tower's sessionhub state dir
	mover  *mover
	logs   *strings.Builder
}

func newMoveEnv(t *testing.T, paneStatus string) *moveEnv {
	t.Helper()
	e := &moveEnv{order: &orderLog{}, base: t.TempDir(), claude: t.TempDir(), state: t.TempDir(), logs: &strings.Builder{}}
	e.origin, e.work = newGitRepo(t, e.base, "app")
	writeFile(t, filepath.Join(e.claude, "projects", "-src-app", moveSID+".jsonl"), `{"type":"user","message":"hi"}`+"\n", 0o600)
	writeFile(t, filepath.Join(e.claude, "projects", "-src-app", moveSID, "subagents", "a.jsonl"), "{}\n", 0o600)
	writeFile(t, filepath.Join(e.claude, "file-history", moveSID, "h@v1"), "snap\n", 0o600)
	fakeClaude(t, `[ "$1" = --version ] && echo "2.1.285 (Claude Code)"`)
	e.sessionhub = newMoveHub(t, e.order)
	e.pane, e.herdr = newMovePane(t, moveSID, paneStatus, e.order)
	if _, err := e.sessionhub.st.UpsertSession(context.Background(), e.sessionhub.tower.id, api.SessionUpsert{ID: moveSID, Agent: "claude",
		Source: api.SourcePlugin, CWD: e.work, GitRepo: e.origin, GitBranch: "main", HerdrSession: "default",
		HerdrWorkspace: "w1", HerdrPane: "w1:p1", AgentState: "idle"}); err != nil {
		t.Fatal(err)
	}
	e.mover = newMover(e.herdr.Path, e.claude, e.state, []string{e.base}, e.sessionhub.tower.key, log.New(e.logs, "", 0))
	fastMover(e.mover)
	return e
}

// fastMover shortens a mover's waits for tests.
func fastMover(m *mover) {
	m.exitWait, m.pollEvery = 2*time.Second, 5*time.Millisecond
	m.start = resume.ControlOptions{PollEvery: 5 * time.Millisecond, PollFor: time.Second}
	m.resultPause = time.Millisecond
}

// startMove opens a move of the session to target and returns tower's claim.
func (e *moveEnv) startMove(t *testing.T, target string) api.ControlClaim {
	t.Helper()
	if _, err := e.sessionhub.st.CreateMove(context.Background(), moveSID, target, "machine:tower"); err != nil {
		t.Fatal(err)
	}
	return e.sessionhub.claim(t, e.sessionhub.tower)
}
```

- [ ] **Step 5: Write the failing watcher tests**

Create `internal/plugin/move_test.go`:

```go
package plugin

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/move"
)

func TestMoveOutEndsSessionBeforeUpload(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nchanged\n", 0o644)
	writeFile(t, filepath.Join(e.work, "b.txt"), "b\n", 0o644)
	gitT(t, e.work, "add", "b.txt")
	gitT(t, e.work, "commit", "-qm", "local commit")
	writeFile(t, filepath.Join(e.work, "new.txt"), "untracked\n", 0o644)

	claim := e.startMove(t, "bluebox")
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)

	if got := e.order.list(); !slices.Equal(got, []string{"exit", "upload"}) {
		t.Fatalf("order %v, want exit then upload (logs:\n%s)", got, e.logs)
	}
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveUploaded || mv.BundleSize == 0 {
		t.Fatalf("move %+v", mv)
	}
	if got, want := gitT(t, e.origin, "rev-parse", "main"), gitT(t, e.work, "rev-parse", "HEAD"); got != want {
		t.Errorf("the local commit was not pushed: origin %s, local %s", got, want)
	}
	sealed, err := os.ReadFile(filepath.Join(e.sessionhub.moveDir, mv.ID+".sealed"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := move.Open(e.sessionhub.bluebox.key, e.sessionhub.tower.key.PublicKey(), mv.ID, sealed)
	if err != nil {
		t.Fatalf("bluebox cannot open the bundle: %v", err)
	}
	b, err := move.Extract(plain)
	if err != nil {
		t.Fatal(err)
	}
	m := b.Manifest
	if m.SessionID != moveSID || m.MoveID != mv.ID || m.SourceMachine != "tower" || m.Branch != "main" ||
		m.Head != gitT(t, e.work, "rev-parse", "HEAD") || m.ClaudeVersion != "2.1.285" || m.RootRel != "app" ||
		!slices.Equal(m.Untracked, []string{"new.txt"}) || !strings.Contains(string(b.Patch), "+changed") {
		t.Errorf("manifest %+v patch %q", m, b.Patch)
	}
	// The transcript stays until the move is done.
	if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 1 {
		t.Errorf("transcripts after upload: %v", ts)
	}
	if prompts, starts := e.pane.snapshot(); !slices.Equal(prompts, []string{"/exit"}) || len(starts) != 0 {
		t.Errorf("prompts %q starts %v", prompts, starts)
	}
}

func TestMoveOutRefusesBusyPane(t *testing.T) {
	for _, status := range []string{"working", "blocked", ""} {
		t.Run(fmt.Sprintf("status %q", status), func(t *testing.T) {
			e := newMoveEnv(t, status)
			claim := e.startMove(t, "bluebox")
			e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
			mv := e.sessionhub.move(t, claim.Move.ID)
			if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "check pane: ") {
				t.Errorf("move %+v", mv)
			}
			if prompts, _ := e.pane.snapshot(); len(prompts) != 0 {
				t.Errorf("typed into a busy pane: %q", prompts)
			}
			if len(e.order.list()) != 0 {
				t.Errorf("steps ran: %v", e.order.list())
			}
		})
	}
}

// Every check that can fail while the session runs comes before /exit, and
// the pane is checked again right before it.
func TestMoveOutChecksBeforeExit(t *testing.T) {
	for _, c := range []struct {
		name, prefix string
		setup        func(t *testing.T, e *moveEnv)
	}{
		{"no transcript", "build bundle: ", func(t *testing.T, e *moveEnv) { os.RemoveAll(filepath.Join(e.claude, "projects")) }},
		{"too big", "build bundle: untracked files over", func(t *testing.T, e *moveEnv) {
			writeFile(t, filepath.Join(e.work, "huge.bin"), strings.Repeat("x", 10<<20+1), 0o644)
		}},
		{"busy again after the push", "check pane: the agent is working", func(t *testing.T, e *moveEnv) {
			e.pane.mu.Lock()
			e.pane.busyAfter = 1
			e.pane.mu.Unlock()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newMoveEnv(t, "idle")
			c.setup(t, e)
			claim := e.startMove(t, "bluebox")
			e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
			if mv := e.sessionhub.move(t, claim.Move.ID); mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, c.prefix) ||
				strings.HasSuffix(mv.Detail, detailResumes) {
				t.Errorf("move %+v", mv)
			}
			if prompts, starts := e.pane.snapshot(); len(prompts) != 0 || len(starts) != 0 {
				t.Errorf("prompts %q starts %v", prompts, starts)
			}
		})
	}
}

func TestMoveOutRestartsAfterFailure(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	// The sessionhub turns the upload away and still shows the move packing: the
	// bundle did not land, so the move fails and the session restarts here.
	e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/bundle") {
			badGateway(w)
			return true
		}
		return false
	})
	claim := e.startMove(t, "bluebox")
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "upload: ") || !strings.HasSuffix(mv.Detail, detailResumes) {
		t.Fatalf("move %+v", mv)
	}
	if got := e.order.list(); !slices.Equal(got, []string{"exit", "upload", "start"}) {
		t.Errorf("order %v", got)
	}
	if _, starts := e.pane.snapshot(); len(starts) != 1 || !slices.Equal(starts[0], []string{"--resume", moveSID}) {
		t.Errorf("starts %v", starts)
	}
	if _, ok, _ := e.sessionhub.st.ClaimMove(ctx, e.sessionhub.tower.id); ok {
		t.Error("a source that failed its own step got a finish")
	}
}

// The source restarts the session only once the sessionhub recorded the failure:
// an upload whose outcome is unknown, or a failure the sessionhub did not take,
// leaves the session ended for the move's finish.
func TestMoveOutRestartsOnlyAfterRecordedFailure(t *testing.T) {
	for _, c := range []struct {
		name  string
		block func(r *http.Request) bool
	}{
		{"upload outcome unknown", func(r *http.Request) bool {
			return r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/bundle") ||
				r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/moves/")
		}},
		{"failure not recorded", func(r *http.Request) bool {
			return r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/bundle") ||
				r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/result")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newMoveEnv(t, "idle")
			e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
				if isFrom(r, e.sessionhub.tower) && c.block(r) {
					badGateway(w)
					return true
				}
				return false
			})
			claim := e.startMove(t, "bluebox")
			e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
			if mv := e.sessionhub.move(t, claim.Move.ID); mv.State != api.MovePacking {
				t.Errorf("move %+v, want it left packing for its timeout", mv)
			}
			if got := e.order.list(); !slices.Equal(got, []string{"exit", "upload"}) {
				t.Errorf("order %v: the session restarted without a recorded failure (logs:\n%s)", got, e.logs)
			}
		})
	}
}

func TestMoveOutExitTimesOut(t *testing.T) {
	e := newMoveEnv(t, "idle")
	e.pane.exits = false
	e.mover.exitWait = 50 * time.Millisecond
	claim := e.startMove(t, "bluebox")
	e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "end session: ") || !strings.HasSuffix(mv.Detail, detailResumes) {
		t.Errorf("move %+v", mv)
	}
	if slices.Contains(e.order.list(), "upload") || slices.Contains(e.order.list(), "start") {
		t.Errorf("order %v", e.order.list())
	}
}

func TestFinishDoneArchives(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	claim := e.startMove(t, "bluebox")
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
	e.sessionhub.claim(t, e.sessionhub.bluebox) // the target's move-in
	if _, err := e.sessionhub.st.FinishMove(ctx, e.sessionhub.bluebox.id, claim.Move.ID, api.MoveResultIn{State: api.MoveDone}); err != nil {
		t.Fatal(err)
	}
	fin := e.sessionhub.claim(t, e.sessionhub.tower)
	if fin.Request.Action != api.ActionMoveOut || fin.Move.State != api.MoveDone {
		t.Fatalf("finish claim %+v", fin.Move)
	}
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 0 {
		t.Errorf("still resumable on the source: %v", ts)
	}
	if _, err := os.Stat(filepath.Join(e.state, "moved", claim.Move.ID, "transcript", "-src-app", moveSID+".jsonl")); err != nil {
		t.Errorf("archive: %v", err)
	}
	if _, starts := e.pane.snapshot(); len(starts) != 0 {
		t.Errorf("a done move restarted the session: %v", starts)
	}
}

func TestFinishFailedRestarts(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	claim := e.startMove(t, "bluebox")
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
	e.sessionhub.claim(t, e.sessionhub.bluebox)
	if _, err := e.sessionhub.st.FinishMove(ctx, e.sessionhub.bluebox.id, claim.Move.ID, api.MoveResultIn{State: api.MoveFailed, Detail: "find clone: none"}); err != nil {
		t.Fatal(err)
	}
	// A new mover stands for a watcher that restarted since the upload.
	m2 := newMover(e.herdr.Path, e.claude, e.state, nil, e.sessionhub.tower.key, log.New(e.logs, "", 0))
	fastMover(m2)
	fin := e.sessionhub.claim(t, e.sessionhub.tower)
	m2.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	if _, starts := e.pane.snapshot(); len(starts) != 1 || !slices.Equal(starts[0], []string{"--resume", moveSID}) {
		t.Fatalf("starts %v (logs:\n%s)", starts, e.logs)
	}
	if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 1 {
		t.Errorf("a failed move archived the transcript: %v", ts)
	}
	// The session already runs again: a second finish starts nothing.
	m2.finish(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	if _, starts := e.pane.snapshot(); len(starts) != 1 {
		t.Errorf("a running session was started again: %v", starts)
	}
}

// An archive that keeps failing is retried, then shows in the move's detail.
func TestFinishArchiveFailureIsNoted(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	claim := e.startMove(t, "bluebox")
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
	e.sessionhub.claim(t, e.sessionhub.bluebox)
	if _, err := e.sessionhub.st.FinishMove(ctx, e.sessionhub.bluebox.id, claim.Move.ID, api.MoveResultIn{State: api.MoveDone}); err != nil {
		t.Fatal(err)
	}
	// moved is a file, so every try fails.
	writeFile(t, filepath.Join(e.state, "moved"), "not a folder", 0o600)
	fin := e.sessionhub.claim(t, e.sessionhub.tower)
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveDone || !strings.HasPrefix(mv.Detail, "archive on tower failed: ") ||
		!strings.HasSuffix(mv.Detail, "remove the transcript there by hand") {
		t.Errorf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if n := strings.Count(e.logs.String(), "archive session "+moveSID); n != 1 {
		t.Errorf("logged the archive failure %d times", n)
	}
	if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 1 {
		t.Errorf("transcripts %v", ts)
	}
}

func TestControllerWithoutMoverFailsTheStep(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctl := newController(func() (*client.Client, error) { return e.sessionhub.client(t, e.sessionhub.tower), nil }, nil, log.New(e.logs, "", 0))
	claim := e.startMove(t, "bluebox")
	ctl.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
	if mv := e.sessionhub.move(t, claim.Move.ID); mv.State != api.MoveFailed || mv.Detail != "check pane: "+detailNoMover {
		t.Errorf("move %+v", mv)
	}
}

func TestWatcherRegistersMoveKey(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.w.moveKey = "Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyMDA="
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	e.w.step(ctx, t0)
	e.w.step(ctx, t0.Add(heartbeatInterval))
	if n := e.sessionhub.count(http.MethodPut, "/v1/machines/self/move-key"); n != 1 {
		t.Fatalf("key sent %d times in the first hour, want 1", n)
	}
	e.w.step(ctx, t0.Add(moveKeyEvery+heartbeatInterval))
	if n := e.sessionhub.count(http.MethodPut, "/v1/machines/self/move-key"); n != 2 {
		t.Errorf("key sent %d times after an hour, want 2", n)
	}
	// Without a key the watcher never sends one.
	e2 := newTestEnv(t)
	e2.w.step(ctx, t0)
	if n := e2.sessionhub.count(http.MethodPut, "/v1/machines/self/move-key"); n != 0 {
		t.Errorf("sent a key it does not have: %d", n)
	}
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./internal/plugin/ -count=1`
Expected: FAIL to compile with `undefined: newMover`, `undefined: mover`,
`undefined: detailNoMover`, `undefined: detailResumes`, `e.w.moveKey
undefined`, and `undefined: moveKeyEvery`.

- [ ] **Step 7: Write the mover**

Create `internal/plugin/move.go`:

```go
package plugin

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/move"
	"github.com/abdallah/session-hub/internal/resume"
)

// Mover timing.
const (
	moveExitWait     = 30 * time.Second // how long Claude may take to leave the pane after /exit
	movePollEvery    = 500 * time.Millisecond
	moveResultTries  = 5
	moveResultPause  = 2 * time.Second // grows by this much after each failed try
	moveArchiveTries = 3
	exitCommand      = "/exit"
)

// detailNoMover is why a watcher without a move key fails a move step.
const detailNoMover = "this watcher has no move key; see watcher.log"

// detailResumes ends the detail of a source step that failed after /exit.
// The source restarts the session once the sessionhub has recorded the failure.
const detailResumes = "; the session resumes here"

// mover runs this machine's part of a move: packing and handing off on the
// source (move-out while packing), unpacking on the target (move-in), and
// the source's finish (move-out once the move ended). See docs/plugin.md,
// "Moves".
type mover struct {
	socket    string // the herdr socket
	claudeDir string // ~/.claude, or $CLAUDE_CONFIG_DIR
	stateDir  string // sessionhub's state dir; moved/ holds archives
	roots     []string
	claudeBin string
	key       *ecdh.PrivateKey
	log       *log.Logger
	now       func() time.Time
	// Waits. Tests shorten them.
	exitWait, pollEvery time.Duration
	start               resume.ControlOptions // PollEvery and PollFor of StartResumed
	resultTries         int
	resultPause         time.Duration // also the pause between archive tries
	archiveTries        int
}

func newMover(socket, claudeDir, stateDir string, roots []string, key *ecdh.PrivateKey, logger *log.Logger) *mover {
	return &mover{socket: socket, claudeDir: claudeDir, stateDir: stateDir, roots: roots, claudeBin: "claude", key: key,
		log: logger, now: time.Now, exitWait: moveExitWait, pollEvery: movePollEvery,
		start: resume.ControlOptions{PollFor: controlPollFor}, resultTries: moveResultTries, resultPause: moveResultPause,
		archiveTries: moveArchiveTries}
}

// stepError is a failed step: the step's name and why.
type stepError struct {
	step string
	err  error
}

func (e *stepError) Error() string { return e.step + ": " + e.err.Error() }

func stepf(step, format string, args ...any) *stepError {
	return &stepError{step: step, err: fmt.Errorf(format, args...)}
}

// failed is the result for a failed step.
func failed(err error) api.MoveResultIn {
	return api.MoveResultIn{State: api.MoveFailed, Detail: termtext.Clean(err.Error(), api.MaxMoveDetailRunes)}
}

// handle runs one move claim. Each part posts its own results.
func (m *mover) handle(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	mv := claim.Move
	if mv == nil {
		m.log.Printf("move: claim %s has no move", claim.Request.ID)
		return
	}
	switch {
	case claim.Request.Action == api.ActionMoveOut && mv.State == api.MovePacking:
		m.moveOut(ctx, cl, claim)
	case claim.Request.Action == api.ActionMoveOut:
		m.finish(ctx, cl, claim)
	case claim.Request.Action == api.ActionMoveIn:
		m.moveIn(ctx, cl, claim)
	}
}

// moveOut is the source's part: steps 1 to 6 of the spec. Every check that
// can fail while the session runs comes before /exit; a step that fails
// after it goes through abort. A pack that uploaded its bundle posts
// nothing: the upload moved the move on.
func (m *mover) moveOut(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	s, mv := claim.Session, claim.Move
	m.log.Printf("move: %s: moving session %s to %s", mv.ID, s.ID, termtext.Clean(mv.Target, 64))
	fail := func(err error) { m.postResult(ctx, cl, mv.ID, failed(err)) }
	if _, err := m.idlePane(s); err != nil {
		fail(&stepError{"check pane", err})
		return
	}
	repo, err := move.ReadRepo(ctx, s.CWD, m.roots)
	if err != nil {
		fail(&stepError{"read repo", err})
		return
	}
	if mv.Target == api.MoveCloud {
		if !move.IsGitHub(repo.Remote) {
			fail(stepf("cloud", "the remote %s is not on GitHub", repo.Remote))
			return
		}
		fail(stepf("cloud", "this watcher cannot hand sessions to the cloud yet"))
		return
	}
	peer, err := move.ParsePublicKey(mv.PeerKey)
	if err != nil {
		fail(stepf("seal", "the target's key: %v", err))
		return
	}
	version, err := move.ClaudeVersion(ctx, m.claudeBin)
	if err != nil {
		fail(&stepError{"build bundle", err})
		return
	}
	in := move.BuildInput{MoveID: mv.ID, SessionID: s.ID, Machine: mv.Source, CWD: s.CWD,
		ClaudeDir: m.claudeDir, ClaudeVersion: version, Repo: repo, Now: m.now()}
	// A trial build finds a missing transcript or a size limit while the
	// session still runs. The real build follows /exit.
	if _, _, err := move.Build(ctx, in); err != nil {
		fail(&stepError{"build bundle", err})
		return
	}
	if err := move.PushBranch(ctx, repo); err != nil {
		fail(&stepError{"push", err})
		return
	}
	if !m.endIdle(ctx, cl, s, mv.ID) {
		return
	}
	in.Now = m.now()
	data, man, err := move.Build(ctx, in)
	if err != nil {
		m.abort(ctx, cl, s, mv.ID, &stepError{"build bundle", err})
		return
	}
	sealed, err := move.Seal(m.key, peer, mv.ID, data)
	if err != nil {
		m.abort(ctx, cl, s, mv.ID, &stepError{"seal", err})
		return
	}
	if err := cl.PutMoveBundle(ctx, mv.ID, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		// The bundle may have landed even though the answer did not. Only a
		// move the sessionhub still shows packing certainly lacks it.
		cur, gerr := cl.GetMove(context.WithoutCancel(ctx), mv.ID)
		switch {
		case gerr != nil:
			m.log.Printf("move: %s: upload: %v; the move can't be read (%v), so session %s stays ended until the move ends",
				mv.ID, err, gerr, s.ID)
		case cur.State != api.MovePacking:
			m.log.Printf("move: %s: upload answered %v, but the move is %s", mv.ID, err, cur.State)
		default:
			m.abort(ctx, cl, s, mv.ID, &stepError{"upload", err})
		}
		return
	}
	m.log.Printf("move: %s: uploaded %d bytes; skipped %d files", mv.ID, len(sealed), len(man.Skipped))
}

// endIdle checks again that the session's pane is idle, since a push can
// take minutes, and ends Claude with /exit. It reports whether the session
// ended; when it did not, it posted the failure.
func (m *mover) endIdle(ctx context.Context, cl *client.Client, s api.Session, id string) bool {
	pane, err := m.idlePane(s)
	if err != nil {
		m.postResult(ctx, cl, id, failed(&stepError{"check pane", err}))
		return false
	}
	if err := m.endSession(ctx, pane, s.ID); err != nil {
		m.abort(ctx, cl, s, id, &stepError{"end session", err})
		return false
	}
	return true
}

// abort handles a source step that failed after /exit. It posts the failure
// and restarts the session here only once the sessionhub recorded it. A failure the
// sessionhub did not take (the move ended meanwhile, the upload landed after all,
// or the sessionhub is out of reach) leaves the session ended, and the move's
// finish restarts it, so it never runs on two machines.
func (m *mover) abort(ctx context.Context, cl *client.Client, s api.Session, id string, err error) {
	if !m.postResult(ctx, cl, id, failed(errors.New(err.Error()+detailResumes))) {
		m.log.Printf("move: %s: session %s stays ended until the move ends", id, s.ID)
		return
	}
	m.restart(ctx, cl, s, id, api.MoveFailed)
}

// restart starts session s here again after move id ended in state, unless
// a pane already runs it. A restart that fails is added to the move's
// detail as a note.
func (m *mover) restart(ctx context.Context, cl *client.Client, s api.Session, id, state string) {
	r := resume.StartResumed(context.WithoutCancel(ctx), m.socket, s, m.start)
	var why string
	switch r.Outcome {
	case resume.OutcomeResumed, resume.OutcomeRunning:
		m.log.Printf("move: %s: session %s %s here", id, s.ID, r.Outcome)
		return
	case resume.OutcomeNoDir:
		why = "the directory is gone"
	default:
		why = fmt.Sprint(r.Err)
	}
	m.log.Printf("move: %s: restart session %s: %s", id, s.ID, why)
	m.postResult(ctx, cl, id, api.MoveResultIn{State: state, Detail: "restart failed: " + why})
}

// finish is the source's last step, once the move ended: archive the
// transcript after a machine move's done (with retries), or restart the
// session after a failure. A problem here is added to the move's detail.
func (m *mover) finish(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	s, mv := claim.Session, claim.Move
	switch {
	case mv.State == api.MoveDone && mv.Target == api.MoveCloud:
		m.log.Printf("move: %s: done in the cloud; the transcript stays here", mv.ID)
	case mv.State == api.MoveDone:
		var err error
		for try := 1; ; try++ {
			if err = move.Archive(m.claudeDir, m.stateDir, s.ID, mv.ID, m.now()); err == nil || try >= m.archiveTries {
				break
			}
			sleepCtx(context.WithoutCancel(ctx), m.resultPause*time.Duration(try))
		}
		if err != nil {
			m.log.Printf("move: %s: archive session %s: %v; remove its transcript by hand so it is not resumed twice", mv.ID, s.ID, err)
			m.postResult(ctx, cl, mv.ID, api.MoveResultIn{State: api.MoveDone,
				Detail: "archive on " + mv.Source + " failed: " + err.Error() + "; remove the transcript there by hand"})
			return
		}
		m.log.Printf("move: %s: done; session %s archived under moved/%s", mv.ID, s.ID, mv.ID)
	default:
		m.log.Printf("move: %s: %s (%s); restarting session %s", mv.ID, mv.State, termtext.Clean(mv.Detail, 200), s.ID)
		m.restart(ctx, cl, s, mv.ID, mv.State)
	}
}

// idlePane returns the pane where Claude runs session s, idle or done.
func (m *mover) idlePane(s api.Session) (string, error) {
	if s.HerdrPane == "" {
		return "", errors.New(detailNotInHerdr)
	}
	if s.HerdrSession != "" && s.HerdrSession != herdr.SessionName(m.socket) {
		return "", errors.New("the session's pane is on another herdr server")
	}
	h, err := herdr.Dial(m.socket)
	if err != nil {
		return "", errors.New(detailHerdrDown)
	}
	defer h.Close()
	p, err := h.PaneGet(s.HerdrPane)
	var he *herdr.Error
	switch {
	case errors.As(err, &he) && he.Code == "pane_not_found":
		return "", errors.New(detailPaneGone)
	case err != nil:
		return "", errors.New(detailHerdrDown)
	case p.Agent == "":
		return "", errors.New(detailNoAgent)
	case p.SessionID() == "":
		return "", errors.New(detailNoSessionID)
	case p.SessionID() != s.ID:
		return "", errors.New(detailOtherSession)
	}
	switch p.AgentStatus {
	case "idle", "done":
		return p.PaneID, nil
	case "":
		return "", errors.New("the agent's status is unknown")
	}
	return "", errors.New("the agent is " + termtext.Clean(p.AgentStatus, 40))
}

// endSession types /exit into pane and waits up to exitWait for Claude to
// leave it: the pane is gone, runs no agent, or runs another session. With
// id empty (a pane this watcher started, before herdr reports the session),
// only the agent leaving counts.
func (m *mover) endSession(ctx context.Context, pane, id string) error {
	h, err := herdr.Dial(m.socket)
	if err != nil {
		return errors.New(detailHerdrDown)
	}
	defer h.Close()
	if err := h.AgentPrompt(pane, exitCommand); err != nil {
		var be *herdr.BlockedError
		if errors.As(err, &be) {
			return errors.New(detailBlocked)
		}
		return fmt.Errorf("send %s: %w", exitCommand, err)
	}
	deadline := time.Now().Add(m.exitWait)
	for {
		p, err := h.PaneGet(pane)
		var he *herdr.Error
		if errors.As(err, &he) && he.Code == "pane_not_found" || err == nil && (p.Agent == "" || id != "" && p.SessionID() != id) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("Claude was still in the pane %s after %s", termtext.Clean(pane, 64), m.exitWait)
		}
		if !sleepCtx(ctx, m.pollEvery) {
			return ctx.Err()
		}
	}
}

// postResult posts a move result and reports whether the sessionhub recorded it. A
// network error or 5xx is retried up to resultTries times; a 4xx (the move
// ended meanwhile, or it is not this machine's step) is logged.
func (m *mover) postResult(ctx context.Context, cl *client.Client, id string, in api.MoveResultIn) bool {
	in.Detail = termtext.Clean(in.Detail, api.MaxMoveDetailRunes)
	pctx := context.WithoutCancel(ctx)
	for try := 1; ; try++ {
		err := cl.PostMoveResult(pctx, id, in)
		if err == nil {
			m.log.Printf("move: %s: %s %s", id, in.State, in.Detail)
			return true
		}
		var se *client.StatusError
		if errors.As(err, &se) && se.Status < 500 || try >= m.resultTries {
			m.log.Printf("move: %s: result %s not recorded: %v", id, in.State, err)
			return false
		}
		sleepCtx(pctx, m.resultPause*time.Duration(try))
	}
}

// moveIn is the target's part. This watcher cannot take moves yet.
func (m *mover) moveIn(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	m.postResult(ctx, cl, claim.Move.ID, failed(stepf("download", "this watcher cannot take moved sessions yet")))
}
```

In `internal/plugin/control.go`, replace:

```go
	// deliver submits a message; nil refuses every message. runWatcher sets
	// it.
	deliver messageFunc
```

with:

```go
	// deliver submits a message; nil refuses every message. runWatcher sets
	// it.
	deliver messageFunc
	// move runs a move claim and posts its result; nil fails every move
	// step. runWatcher sets it when the machine has a move key.
	move func(ctx context.Context, cl *client.Client, claim api.ControlClaim)
```

and replace:

```go
func (c *controller) handle(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	req := claim.Request
```

with:

```go
func (c *controller) handle(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	req := claim.Request
	if req.Action == api.ActionMoveOut || req.Action == api.ActionMoveIn {
		c.handleMove(ctx, cl, claim)
		return
	}
```

and add after the `handle` function:

```go
// handleMove passes a move claim to the mover. Without one it fails a pack
// or unpack step, so the move ends at once instead of timing out, and logs
// a finish.
func (c *controller) handleMove(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	if c.move != nil {
		c.move(ctx, cl, claim)
		return
	}
	mv := claim.Move
	if mv == nil || (mv.State != api.MovePacking && mv.State != api.MoveUnpacking) {
		c.log.Printf("control: move claim %s ignored: %s", claim.Request.ID, detailNoMover)
		return
	}
	step := "check pane"
	if mv.State == api.MoveUnpacking {
		step = "download"
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlPostFor)
	defer cancel()
	if err := cl.PostMoveResult(pctx, mv.ID, api.MoveResultIn{State: api.MoveFailed, Detail: step + ": " + detailNoMover}); err != nil {
		c.log.Printf("control: move %s: result not recorded: %v", mv.ID, err)
	}
}
```

- [ ] **Step 8: Register the key and wire the mover**

In `internal/plugin/watcher.go`, replace:

```go
	// instructionsPath is the local rules copy the heartbeat refreshes; ""
	// turns the refresh off. newWatcher leaves it empty, runWatcher sets it.
	instructionsPath string
}
```

with:

```go
	// instructionsPath is the local rules copy the heartbeat refreshes; ""
	// turns the refresh off. newWatcher leaves it empty, runWatcher sets it.
	instructionsPath string
	// moveKey is this machine's move public key, sent after the first
	// accepted heartbeat and every moveKeyEvery after that; "" sends none.
	moveKey   string
	moveKeyAt time.Time // when the server last took it
}

// moveKeyEvery is how often the watcher registers its move key again, so a
// server restored from a backup learns it back.
const moveKeyEvery = time.Hour
```

replace:

```go
	w.lastBeatOK = now
	w.reportInbox(ctx, c, now)
	w.refreshInstructions(ctx, c, now)
}
```

with:

```go
	w.lastBeatOK = now
	w.reportInbox(ctx, c, now)
	w.refreshInstructions(ctx, c, now)
	w.registerMoveKey(ctx, c, now)
}

// registerMoveKey sends the move public key when the server has not taken it
// in the last moveKeyEvery. A failure is logged and retried at the next
// heartbeat.
func (w *watcher) registerMoveKey(ctx context.Context, c *client.Client, now time.Time) {
	if w.moveKey == "" || (!w.moveKeyAt.IsZero() && now.Sub(w.moveKeyAt) < moveKeyEvery) {
		return
	}
	if err := c.PutMoveKey(ctx, w.moveKey); err != nil {
		w.logf(now, "move: key not registered: %v", err)
		return
	}
	if w.moveKeyAt.IsZero() {
		if raw, err := api.ParseMoveKey(w.moveKey); err == nil {
			w.log.Printf("move: key %s registered", api.MoveKeyFingerprint(raw))
		}
	}
	w.moveKeyAt = now
}
```

In `runWatcher`, add `"github.com/abdallah/session-hub/internal/move"` to the file's imports,
and replace:

```go
	ctl := newController(newClient, controlRunner(socketPath, 0, controlPollFor), logger)
	ctl.deliver = messageRunner(socketPath)
```

with:

```go
	ctl := newController(newClient, controlRunner(socketPath, 0, controlPollFor), logger)
	ctl.deliver = messageRunner(socketPath)
	if key, err := move.LoadOrCreateKey(move.KeyPath()); err != nil {
		logger.Printf("move: no move key, so this machine can't move sessions: %v", err)
	} else {
		cfg, _ := client.LoadConfig()
		w.moveKey = move.PublicKeyString(key)
		ctl.move = newMover(socketPath, digest.DefaultClaudeDir(), stateDir, move.DefaultRoots(cfg.MoveRoots), key, logger).handle
	}
```

`runWatcher` now creates `move.key` next to `SESSIONHUB_CONFIG`, so its tests must
point that at a temp dir. In `internal/plugin/watcher_test.go`, replace:

```go
func TestRunWatcherLifecycle(t *testing.T) {
	dir := t.TempDir()
```

with:

```go
func TestRunWatcherLifecycle(t *testing.T) {
	dir := t.TempDir()
	// runWatcher loads or creates the move key next to the client config:
	// never the real ~/.config/sessionhub.
	cfgDir := t.TempDir()
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(cfgDir, "config.toml"))
```

replace:

```go
	if running, _ := watcherRunning(dir); !running {
		t.Fatal("lock not held while the watcher runs")
	}
```

with:

```go
	if running, _ := watcherRunning(dir); !running {
		t.Fatal("lock not held while the watcher runs")
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "move.key")); err != nil {
		t.Errorf("move key not created beside SESSIONHUB_CONFIG: %v", err)
	}
```

and replace:

```go
func TestRunWatcherOutlastsLockProbe(t *testing.T) {
	dir := t.TempDir()
```

with:

```go
func TestRunWatcherOutlastsLockProbe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
```

- [ ] **Step 9: Run the tests to verify they pass**

Run: `go test ./internal/plugin/ ./internal/move/ -count=1 -race`
Expected: PASS. `TestMoveOutEndsSessionBeforeUpload` shows the order
`[exit upload]`, and the target's key opens the bundle. Then check that no
test wrote a key outside its temp dirs: with `HOME` and `SESSIONHUB_STATE_DIR` set
to fresh temp dirs, `SESSIONHUB_CONFIG` and every `HERDR_*` variable unset,
`go test ./internal/plugin/ -count=1` leaves no `$HOME/.config/sessionhub/move.key`.

- [ ] **Step 10: Document the source's part**

In `docs/plugin.md`, before "## Files in the state dir", add:

```markdown
### Moves

The watcher does both ends of a move (see `docs/server.md`, "Moves"). At
start it loads or creates `~/.config/sessionhub/move.key` and, after its first
accepted heartbeat and every hour, registers the public key with
`PUT /v1/machines/self/move-key`; `move: key <fingerprint> registered` is
logged once. Without a usable key file it logs why and fails every move
step it is given with "this watcher has no move key".

Move claims arrive on the same long poll as Remote Control and messages, and
the loop runs one at a time, so a move (push, `/exit`, upload) holds other
requests back until it ends.

**Source (`move-out` while `packing`).** Each step's name starts the
`failed` detail. Every step that can fail while the session runs comes
before `/exit`:

1. `check pane`: herdr must report the session's pane running that session
   with the agent `idle` or `done`, the same checks as message delivery.
2. `read repo`: the session's directory must be in a Git work tree with an
   `origin` remote and a branch checked out.
3. `build bundle`: `claude --version`, then a trial build of the bundle (see
   `docs/client.md`, "Moves"), so a missing transcript or a size limit fails
   while the session still runs.
4. `push`: the branch is pushed when it has no upstream (`git push -u origin
   <branch>`) or is ahead of it, with SSH in `BatchMode`. It never forces.
5. `check pane` again, since a push can take minutes, then `end session`:
   the watcher types `/exit` with `agent.prompt` and waits up to 30 seconds
   for herdr to report no agent in the pane.
6. `build bundle`, `seal`, `upload`: the real bundle, sealed for the
   target's key, streamed to `PUT /v1/moves/{id}/bundle`. The upload moves
   the move to `uploaded`; the watcher posts nothing more.

If the upload fails, the watcher reads the move back. Still `packing` means
the bundle did not land, and the step fails. Any other state means it
landed. If the move can't be read, the outcome is unknown: the watcher
posts nothing and leaves the session ended, and the move's finish restarts
it once the move times out.

A step that fails after `/exit` is posted with a detail that ends
`; the session resumes here`. Only once the sessionhub records that failure does
the watcher restart the session with `claude --resume <id>` in its old pane
(which sits at a shell) or a new workspace; if a pane still runs it,
nothing starts. A failure the sessionhub did not record (the move ended meanwhile,
or the sessionhub is out of reach) leaves the session ended until the finish, so
the session never runs on two machines. A restart that fails adds
`restart failed: <why>` to the move's detail.

**Finish (`move-out` once the move ended).** On `done`, the watcher moves
the transcript, its sidecar folder, and the file history to
`<state>/moved/<move id>/`, so the session can't be resumed here as well;
archives older than 30 days are removed then. It tries 3 times; if the
archive still fails, it adds `archive on <machine> failed: <why>; remove
the transcript there by hand` to the move's detail, which `sessionhub move
--status` and the dashboard show. On `failed` (the target's failure, or a
timeout), it restarts the session as above, unless a pane already runs it.
The finish arrives through the long poll, so a watcher that restarted
mid-move still gets it.

Log lines start with `move:`. Results are posted to
`POST /v1/moves/{id}/result`, retried up to 5 times on a network error or a
`5xx`. After the move ended, the same route takes the source's notes.
```

In the same file, section "## Files in the state dir", replace:

```markdown
| `watcher.log` | The watcher's stderr. Truncated when the watcher starts if it is over 1 MiB, and checked on every tick. An identical failure line is repeated at most every 5 minutes. |
```

with:

```markdown
| `watcher.log` | The watcher's stderr. Truncated when the watcher starts if it is over 1 MiB, and checked on every tick. An identical failure line is repeated at most every 5 minutes. |
| `moved/<move id>/` | The transcript, sidecar folder, and file history of a session moved to another machine, kept 30 days. |
```

In `docs/client.md`, at the end of section "Moves", add:

```markdown
- **Source steps.** `PushBranch` pushes the branch (`-u origin` when it has
  no upstream, else to its upstream when ahead; never forced).
  `ClaudeVersion` is the first field of `claude --version`. `Archive` moves
  a session's transcript, sidecar folder, and file history into
  `<state>/moved/<move id>/` and removes archives older than 30 days.
```

- [ ] **Step 11: Lint and commit**

Run: `gofmt -w internal/move/*.go internal/plugin/*.go && make lint`
Expected: clean.

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1`
Expected: every package `ok`.

```bash
git add internal/move/source.go internal/move/source_test.go internal/plugin/move.go internal/plugin/move_helpers_test.go \
  internal/plugin/move_test.go internal/plugin/control.go internal/plugin/watcher.go internal/plugin/watcher_test.go \
  docs/plugin.md docs/client.md
git commit -m "$(cat <<'EOF'
Pack a moving session on the source machine

The watcher registers its move key, and on a move-out claim checks the
pane is idle, runs every check that can fail before /exit, pushes the
branch, checks the pane again, ends Claude with /exit, then builds,
seals, and uploads the bundle. A failure after /exit restarts the
session only once the sessionhub recorded it; an upload with an unknown outcome
restarts nothing. The finish archives the transcript on done (and notes
an archive that keeps failing on the move) or restarts the session on
failure, and survives a watcher restart. The runWatcher tests keep the
key they create in a temp dir. Move-in and cloud hand-off still fail
with a clear detail; the next tasks add them.

Refs: docs/dev/superpowers/plans/2026-10-03-move-session.md, Task 7

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 8: Watcher: move-in

**Files:**
- Create: `internal/move/target.go` (`ErrNoClone`, `FindClone`,
  `CheckClone`, `PatchNewFiles`, `Checkout`, `PrepareClone`, `Apply`, `Undo`,
  `ProjectDir`, `WriteSession`, `RemoveFiles`)
- Create: `internal/move/target_test.go`
- Modify: `internal/plugin/move.go` (`moveIn` replaces the stub)
- Modify: `internal/plugin/move_helpers_test.go` (`targetHerdr`,
  `(*targetHerdr).state`, `targetEnv`, `newTargetEnv`, `(*moveEnv).packed`)
- Modify: `internal/plugin/move_test.go` (move-in tests)
- Modify: `docs/plugin.md` (section "Moves")
- Modify: `docs/client.md` (section "Moves")

**Interfaces:**
- Consumes: `git`, `gitRaw`, `gitOpts`, `gitNetTimeout`, `NormalizeRemote`,
  `rootRel`, `FindTranscripts`, `Bundle`, `File` (Task 2); `ClaudeVersion`
  (Task 7); `resume.StartResumed` (Task 6); the Task 7 plugin helpers.
- Produces: the target functions in the interface ledger; `(*mover).moveIn`,
  `(*mover).stopClaude`.

- [ ] **Step 1: Write the failing target-side tests**

Create `internal/move/target_test.go`:

```go
package move

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProjectDir(t *testing.T) {
	claude := t.TempDir()
	for _, c := range []struct{ cwd, want string }{
		{"/home/user/my project", "-home-user-my-project"},
		{"/home/user/Code/OTGS/app.v2", "-home-user-Code-OTGS-app-v2"},
		{"/tmp/café", "-tmp-caf-"},
		{"/tmp/\U0001F600x", "-tmp---x"}, // one rune outside the BMP is two UTF-16 units, so two dashes
	} {
		got, err := ProjectDir(claude, c.cwd)
		if err != nil || got != filepath.Join(claude, "projects", c.want) {
			t.Errorf("ProjectDir(%q) = %q, %v; want projects/%s", c.cwd, got, err, c.want)
		}
	}
	long := "/" + strings.Repeat("d", 250)
	if _, err := ProjectDir(claude, long); err == nil || !strings.Contains(err.Error(), "longer than 200") {
		t.Errorf("long path with no folder: %v", err)
	}
	existing := filepath.Join(claude, "projects", "-"+strings.Repeat("d", 199)+"-abc123")
	os.MkdirAll(existing, 0o700)
	if got, err := ProjectDir(claude, long); err != nil || got != existing {
		t.Errorf("long path with its folder: %q %v", got, err)
	}
}

func TestWriteSession(t *testing.T) {
	claude := t.TempDir()
	cwd := "/home/user/proj"
	b := &Bundle{Manifest: Manifest{SessionID: testSessionID}, Transcript: []byte("t\n"),
		Sidecar: []File{{Name: "subagents/a.jsonl", Data: []byte("s")}}, History: []File{{Name: "x@v1", Data: []byte("h")}}}
	written, err := WriteSession(claude, cwd, b)
	if err != nil || len(written) != 3 {
		t.Fatalf("written %v %v", written, err)
	}
	proj := filepath.Join(claude, "projects", "-home-user-proj")
	for _, p := range []string{filepath.Join(proj, testSessionID+".jsonl"), filepath.Join(proj, testSessionID, "subagents", "a.jsonl"),
		filepath.Join(claude, "file-history", testSessionID, "x@v1")} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", p, fi, err)
		}
	}
	if _, err := WriteSession(claude, "/elsewhere", b); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("second write: %v", err)
	}
	RemoveFiles(written)
	if ts, _ := FindTranscripts(claude, testSessionID); len(ts) != 0 {
		t.Errorf("RemoveFiles left %v", ts)
	}
	if _, err := os.Stat(filepath.Join(proj, testSessionID)); !os.IsNotExist(err) {
		t.Errorf("empty sidecar folder left: %v", err)
	}
}

func TestFindClone(t *testing.T) {
	ctx := context.Background()
	gitIsolate(t)
	rootA, rootB := t.TempDir(), t.TempDir()
	mk := func(dir, remote string) string {
		gitT(t, filepath.Dir(dir), "init", "-q", dir)
		gitT(t, dir, "remote", "add", "origin", remote)
		return dir
	}
	os.MkdirAll(filepath.Join(rootA, "OTGS"), 0o755)
	a := mk(filepath.Join(rootA, "OTGS", "app"), "git@github.com:Org/app.git")
	os.MkdirAll(filepath.Join(rootA, "a", "b", "c", "d"), 0o755)
	mk(filepath.Join(rootA, "a", "b", "c", "d", "deep"), "git@github.com:Org/deep.git") // depth 5: not searched
	mk(filepath.Join(rootA, "other"), "git@github.com:Org/other.git")

	got, err := FindClone(ctx, []string{rootA}, "https://GitHub.com/Org/app", "otgs/app")
	if err != nil || got != a {
		t.Fatalf("one clone: %q %v", got, err)
	}
	if _, err := FindClone(ctx, []string{rootA}, "git@github.com:Org/deep.git", "x"); !errors.Is(err, ErrNoClone) {
		t.Errorf("too deep: %v", err)
	}
	if _, err := FindClone(ctx, []string{rootA}, "git@github.com:Org/none.git", "x"); !errors.Is(err, ErrNoClone) ||
		!strings.Contains(err.Error(), "github.com/Org/none") {
		t.Errorf("none: %v", err)
	}
	// A second clone: the one whose path under its root matches wins, in any
	// case; with no match, the error lists both.
	os.MkdirAll(filepath.Join(rootB, "forks"), 0o755)
	b := mk(filepath.Join(rootB, "forks", "app"), "ssh://git@github.com/Org/app.git")
	if got, err := FindClone(ctx, []string{rootA, rootB}, "git@github.com:Org/app.git", "FORKS/app"); err != nil || got != b {
		t.Errorf("by relative path: %q %v", got, err)
	}
	if _, err := FindClone(ctx, []string{rootA, rootB}, "git@github.com:Org/app.git", "elsewhere/app"); err == nil ||
		!strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
		t.Errorf("ambiguous: %v", err)
	}
	// A missing root is skipped.
	if got, err := FindClone(ctx, []string{filepath.Join(rootA, "nope"), rootA}, "git@github.com:Org/app.git", ""); err != nil || got != a {
		t.Errorf("missing root: %q %v", got, err)
	}
}

// moved is a clone of origin, as the target has it, on branch other.
func moved(t *testing.T, origin string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "clone")
	gitT(t, filepath.Dir(clone), "clone", "-q", origin, clone)
	gitT(t, clone, "checkout", "-q", "-b", "other")
	return clone
}

func TestPrepareCloneRefusesDirty(t *testing.T) {
	ctx := context.Background()
	origin, _ := newRepo(t)
	clone := moved(t, origin)
	if err := CheckClone(ctx, clone, []string{"notes/new.md"}); err != nil {
		t.Fatalf("clean clone: %v", err)
	}
	writeFile(t, filepath.Join(clone, "a.txt"), "local edit\n", 0o644)
	if err := CheckClone(ctx, clone, nil); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("tracked change: %v", err)
	}
	gitT(t, clone, "checkout", "-q", "--", "a.txt")
	writeFile(t, filepath.Join(clone, "notes", "new.md"), "mine\n", 0o644)
	writeFile(t, filepath.Join(clone, "unrelated.txt"), "fine\n", 0o644)
	if err := CheckClone(ctx, clone, []string{"notes/new.md", "run.sh"}); err == nil || !strings.Contains(err.Error(), "notes/new.md") ||
		strings.Contains(err.Error(), "unrelated") {
		t.Errorf("collision: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(clone, "notes", "new.md")); string(b) != "mine\n" {
		t.Errorf("the check changed a file: %q", b)
	}
}

func TestPatchNewFiles(t *testing.T) {
	_, work := dirtyRepo(t)
	writeFile(t, filepath.Join(work, "new dir", "a b.txt"), "x\n", 0o644)
	writeFile(t, filepath.Join(work, "\u00fc.bin"), "\x00\x01", 0o644)
	writeFile(t, filepath.Join(work, "empty.txt"), "", 0o644)
	gitT(t, work, "add", "new dir/a b.txt", "\u00fc.bin", "empty.txt")
	data, _ := buildFor(t, work, newClaudeDir(t, testSessionID))
	b, err := Extract(data)
	if err != nil {
		t.Fatal(err)
	}
	got := PatchNewFiles(b.Patch)
	slices.Sort(got)
	// a.txt and bin.dat change and gone.txt goes: none of them is new.
	if want := []string{"empty.txt", "new dir/a b.txt", "\u00fc.bin"}; !slices.Equal(got, want) {
		t.Errorf("PatchNewFiles = %q, want %q", got, want)
	}
}

func TestPrepareCloneApplyUndo(t *testing.T) {
	ctx := context.Background()
	origin, work := dirtyRepo(t)
	clone := moved(t, origin) // made before the next push, so PrepareClone must fetch
	oldMain := gitT(t, clone, "rev-parse", "main")
	writeFile(t, filepath.Join(work, "c.txt"), "committed\n", 0o644)
	gitT(t, work, "add", "c.txt")
	gitT(t, work, "commit", "-qm", "ahead", "--", "c.txt") // only c.txt; a.txt stays staged
	gitT(t, work, "push", "-q")
	data, _ := buildFor(t, work, newClaudeDir(t, testSessionID))
	b, err := Extract(data)
	if err != nil {
		t.Fatal(err)
	}
	co, err := PrepareClone(ctx, clone, "main", b.Manifest.Head)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, clone, "rev-parse", "HEAD"); got != b.Manifest.Head || gitT(t, clone, "symbolic-ref", "--short", "HEAD") != "main" {
		t.Fatalf("after checkout: HEAD %s on %s", got, gitT(t, clone, "symbolic-ref", "--short", "HEAD"))
	}
	if err := co.Apply(ctx, b); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.txt", "bin.dat", "c.txt", "notes/new.md", "run.sh"} {
		want, _ := os.ReadFile(filepath.Join(work, f))
		got, _ := os.ReadFile(filepath.Join(clone, f))
		if string(got) != string(want) {
			t.Errorf("%s: %q, want %q", f, got, want)
		}
	}
	if fi, _ := os.Stat(filepath.Join(clone, "run.sh")); fi.Mode().Perm()&0o111 == 0 {
		t.Error("run.sh lost its executable bit")
	}
	if _, err := os.Stat(filepath.Join(clone, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("gone.txt: %v", err)
	}
	// Work that is not the move's, made after Apply: a new file, an edit to
	// a file the patch did not touch, and an edit to a file Apply wrote.
	writeFile(t, filepath.Join(clone, "bystander.txt"), "mine\n", 0o644)
	writeFile(t, filepath.Join(clone, "sub", "keep.txt"), "keep\nmine\n", 0o644)
	writeFile(t, filepath.Join(clone, "notes", "new.md"), "edited here\n", 0o644)
	if err := co.Undo(ctx); err == nil || !strings.Contains(err.Error(), "kept notes/new.md") {
		t.Errorf("undo: %v, want it to keep notes/new.md", err)
	}
	if br := gitT(t, clone, "symbolic-ref", "--short", "HEAD"); br != "other" {
		t.Errorf("after undo on %s, want other", br)
	}
	for f, want := range map[string]string{"bystander.txt": "mine\n", "sub/keep.txt": "keep\nmine\n", "notes/new.md": "edited here\n",
		"a.txt": "one\n"} {
		if got, _ := os.ReadFile(filepath.Join(clone, f)); string(got) != want {
			t.Errorf("%s after undo: %q, want %q", f, got, want)
		}
	}
	for _, f := range []string{"run.sh", "c.txt"} {
		if _, err := os.Stat(filepath.Join(clone, f)); !os.IsNotExist(err) {
			t.Errorf("%s survived the undo: %v", f, err)
		}
	}
	if st := gitT(t, clone, "status", "--porcelain"); st != "M sub/keep.txt\n?? bystander.txt\n?? notes/" {
		t.Errorf("after undo:\n%s", st)
	}
	// main goes back to where it was: the undo takes back its own fast-forward.
	if got := gitT(t, clone, "rev-parse", "main"); got != oldMain {
		t.Errorf("main at %s after undo, want %s", got, oldMain)
	}
}

func TestPrepareCloneUndoDeletesCreatedBranch(t *testing.T) {
	ctx := context.Background()
	origin, work := newRepo(t)
	clone := moved(t, origin)
	gitT(t, work, "checkout", "-q", "-b", "feature/x")
	writeFile(t, filepath.Join(work, "f.txt"), "f\n", 0o644)
	gitT(t, work, "add", "f.txt")
	gitT(t, work, "commit", "-qm", "feature")
	gitT(t, work, "push", "-q", "-u", "origin", "feature/x")
	co, err := PrepareClone(ctx, clone, "feature/x", gitT(t, work, "rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if up := gitT(t, clone, "rev-parse", "--abbrev-ref", "feature/x@{u}"); up != "origin/feature/x" {
		t.Fatalf("tracking %q", up)
	}
	if err := co.Undo(ctx); err != nil {
		t.Fatal(err)
	}
	if br := gitT(t, clone, "symbolic-ref", "--short", "HEAD"); br != "other" {
		t.Errorf("after undo on %s", br)
	}
	if out := gitT(t, clone, "branch", "--list", "feature/x"); out != "" {
		t.Errorf("the tracking branch sessionhub made stayed: %q", out)
	}
	if gitOK(ctx, clone, "config", "--get", "branch.feature/x.merge") {
		t.Error("its tracking config stayed")
	}
	if st := gitT(t, clone, "status", "--porcelain"); st != "" {
		t.Errorf("status:\n%s", st)
	}
}

func TestApplyRefusesLinkedFolder(t *testing.T) {
	ctx := context.Background()
	origin, work := dirtyRepo(t)
	clone := moved(t, origin)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(clone, "notes")); err != nil {
		t.Fatal(err)
	}
	data, _ := buildFor(t, work, newClaudeDir(t, testSessionID))
	b, err := Extract(data)
	if err != nil {
		t.Fatal(err)
	}
	co, err := PrepareClone(ctx, clone, "main", b.Manifest.Head)
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Apply(ctx, b); err == nil || !strings.Contains(err.Error(), "notes is a symbolic link") {
		t.Errorf("apply through a link: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("wrote outside the clone: %v", entries)
	}
	if err := co.Undo(ctx); err != nil {
		t.Fatal(err)
	}
	if st := gitT(t, clone, "status", "--porcelain"); st != "?? notes" {
		t.Errorf("after undo:\n%s", st)
	}
}

func TestPrepareCloneFastForwardOnly(t *testing.T) {
	ctx := context.Background()
	origin, work := newRepo(t)
	clone := moved(t, origin)
	gitT(t, clone, "checkout", "-q", "main")
	writeFile(t, filepath.Join(clone, "local.txt"), "x\n", 0o644)
	gitT(t, clone, "add", "local.txt")
	gitT(t, clone, "commit", "-qm", "only here")
	localMain := gitT(t, clone, "rev-parse", "main")
	gitT(t, clone, "checkout", "-q", "other")
	if _, err := PrepareClone(ctx, clone, "main", gitT(t, work, "rev-parse", "HEAD")); err == nil || !strings.Contains(err.Error(), "fast-forward") {
		t.Errorf("diverged: %v", err)
	}
	if br := gitT(t, clone, "symbolic-ref", "--short", "HEAD"); br != "other" || gitT(t, clone, "rev-parse", "main") != localMain {
		t.Errorf("a refused checkout left the clone on %s, main at %s", br, gitT(t, clone, "rev-parse", "main"))
	}
	if _, err := PrepareClone(ctx, clone, "main", strings.Repeat("ab", 20)); err == nil || !strings.Contains(err.Error(), "not on origin") {
		t.Errorf("unknown commit: %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/move/ -count=1`
Expected: FAIL to compile with `undefined: ProjectDir`, `undefined:
WriteSession`, `undefined: FindClone`, `undefined: CheckClone`,
`undefined: PatchNewFiles`, and `undefined: PrepareClone`.

- [ ] **Step 3: Write the target-side steps**

Create `internal/move/target.go`:

```go
package move

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Clone search and project folder limits.
const (
	cloneDepth     = 4
	maxProjectName = 200
)

// ErrNoClone is FindClone's error when no clone has the remote.
var ErrNoClone = errors.New("no clone")

// FindClone finds the Git repository whose origin remote is remote, under
// the search roots, at most cloneDepth levels down. It does not descend into
// a repository, a hidden directory, or node_modules. With several, it takes
// the one whose path under its root equals rootRel, ignoring case, and
// otherwise fails listing them.
func FindClone(ctx context.Context, roots []string, remote, rootRelPath string) (string, error) {
	want := NormalizeRemote(remote)
	var found []string
	seen := map[string]bool{}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			real, err := filepath.EvalSymlinks(dir)
			if err != nil || seen[real] {
				return
			}
			seen[real] = true
			if u, err := git(ctx, dir, "remote", "get-url", "origin"); err == nil && NormalizeRemote(u) == want {
				found = append(found, dir)
			}
			return
		}
		if depth >= cloneDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "node_modules" {
				continue
			}
			walk(filepath.Join(dir, e.Name()), depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%w of %s", ErrNoClone, want)
	case 1:
		return found[0], nil
	}
	var match []string
	for _, f := range found {
		if strings.EqualFold(rootRel(f, roots), rootRelPath) {
			match = append(match, f)
		}
	}
	if len(match) == 1 {
		return match[0], nil
	}
	sort.Strings(found)
	return "", fmt.Errorf("several clones of %s and none at %s: %s", want, rootRelPath, strings.Join(found, ", "))
}

// CheckClone fails when the clone has uncommitted tracked changes, or when a
// carried path (an untracked file of the bundle, or a file its patch creates:
// PatchNewFiles) already exists there.
func CheckClone(ctx context.Context, root string, carried []string) error {
	out, err := git(ctx, root, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if out != "" {
		return fmt.Errorf("%s has uncommitted changes; commit or put them aside there first", root)
	}
	var clash []string
	for _, n := range carried {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(n))); err == nil {
			clash = append(clash, n)
		}
	}
	if len(clash) > 0 {
		if len(clash) > 5 {
			clash = append(clash[:5], "...")
		}
		return fmt.Errorf("files in %s would be overwritten: %s", root, strings.Join(clash, ", "))
	}
	return nil
}

// PatchNewFiles lists the files a patch from Build creates: each one's
// "diff --git" line is followed by "new file mode". A path git quoted is
// unquoted.
func PatchNewFiles(patch []byte) []string {
	var out []string
	lines := strings.Split(string(patch), "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, "diff --git ") || i+1 == len(lines) || !strings.HasPrefix(lines[i+1], "new file mode ") {
			continue
		}
		if name, ok := diffPath(strings.TrimPrefix(l, "diff --git ")); ok {
			out = append(out, name)
		}
	}
	return out
}

// diffPath is the path of a "diff --git" line whose two sides name the same
// file, as they do for a new file: a/<path> b/<path>, each quoted the way
// git quotes a path with special characters.
func diffPath(rest string) (string, bool) {
	if strings.HasPrefix(rest, `"`) {
		q, err := strconv.QuotedPrefix(rest)
		if err != nil {
			return "", false
		}
		a, err := strconv.Unquote(q)
		if err != nil || !strings.HasPrefix(a, "a/") {
			return "", false
		}
		return a[2:], true
	}
	if n := len(rest) - len("a/ b/"); n > 0 && n%2 == 0 {
		p := rest[2 : 2+n/2]
		if rest == "a/"+p+" b/"+p {
			return p, true
		}
	}
	return "", false
}

// Checkout is a clone sessionhub switched to the moved branch, and what Apply did
// to it, so Undo can take back exactly that and nothing else.
type Checkout struct {
	root    string
	prev    string // the branch, or the SHA when detached, before the switch
	branch  string // the moved branch
	oldTip  string // the branch's commit before the switch; "" when sessionhub created the branch
	newTip  string // the branch's commit after the switch
	hadConf bool   // branch.<branch>.remote or .merge was set before
	patch   []byte // the patch Apply applied, or nil
	wrote   []File // the untracked files Apply wrote, as written
	dirs    []string
}

// PrepareClone fetches origin and checks out branch at head: the local
// branch when there is one, else a tracking branch from origin, else a new
// branch at head. It moves the branch by fast-forward only, and fails, back
// on the previous branch with the moved branch as it was, when the branch
// here has commits head lacks.
func PrepareClone(ctx context.Context, root, branch, head string) (*Checkout, error) {
	c := &Checkout{root: root, branch: branch}
	prev, err := git(ctx, root, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil || prev == "" {
		if prev, err = git(ctx, root, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
			return nil, fmt.Errorf("%s has no commits", root)
		}
	}
	c.prev = prev
	c.oldTip, _ = git(ctx, root, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	c.hadConf = gitOK(ctx, root, "config", "--get", "branch."+branch+".remote") ||
		gitOK(ctx, root, "config", "--get", "branch."+branch+".merge")
	if _, err := gitRaw(ctx, root, gitOpts{timeout: gitNetTimeout}, "fetch", "-q", "origin"); err != nil {
		return nil, err
	}
	if _, err := git(ctx, root, "cat-file", "-e", head+"^{commit}"); err != nil {
		return nil, fmt.Errorf("commit %.12s is not on origin; push it from the source machine", head)
	}
	switch {
	case c.oldTip != "":
		_, err = git(ctx, root, "checkout", "-q", branch)
	case gitOK(ctx, root, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+branch):
		_, err = git(ctx, root, "checkout", "-q", "-b", branch, "--track", "origin/"+branch)
	default:
		_, err = git(ctx, root, "checkout", "-q", "-b", branch, head)
	}
	if err != nil {
		return nil, err
	}
	c.newTip, _ = git(ctx, root, "rev-parse", "HEAD")
	if _, err := git(ctx, root, "merge", "-q", "--ff-only", head); err != nil {
		c.restore(ctx)
		return nil, fmt.Errorf("branch %s here cannot fast-forward to %.12s: %v", branch, head, err)
	}
	if c.newTip, _ = git(ctx, root, "rev-parse", "HEAD"); c.newTip != head {
		c.restore(ctx)
		return nil, fmt.Errorf("branch %s here has commits the source lacks, so it cannot fast-forward to %.12s", branch, head)
	}
	return c, nil
}

func gitOK(ctx context.Context, dir string, args ...string) bool {
	_, err := git(ctx, dir, args...)
	return err == nil
}

// Prev is the branch (or SHA) the clone was on before PrepareClone.
func (c *Checkout) Prev() string { return c.prev }

// restore checks out the previous branch again and puts the moved branch
// back: a branch sessionhub created is deleted, with the tracking config it added,
// and a branch it fast-forwarded returns to its old commit. Each ref update
// is a compare-and-swap on the commit sessionhub left, so a change made since is
// never overwritten.
func (c *Checkout) restore(ctx context.Context) error {
	if _, err := git(ctx, c.root, "checkout", "-q", "--detach"); err != nil {
		return err
	}
	var errs []error
	ref := "refs/heads/" + c.branch
	switch {
	case c.oldTip == "":
		if _, err := git(ctx, c.root, "update-ref", "-d", ref, c.newTip); err != nil {
			errs = append(errs, err)
		} else if !c.hadConf {
			git(ctx, c.root, "config", "--remove-section", "branch."+c.branch) // fails when there is none
		}
	case c.oldTip != c.newTip:
		if _, err := git(ctx, c.root, "update-ref", ref, c.oldTip, c.newTip); err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := git(ctx, c.root, "checkout", "-q", c.prev); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// withPatch writes patch to a temp file for git apply, and removes it after
// f.
func withPatch(patch []byte, f func(name string) error) error {
	tf, err := os.CreateTemp("", "sessionhub-move-*.patch")
	if err != nil {
		return err
	}
	defer os.Remove(tf.Name())
	if _, err := tf.Write(patch); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Close(); err != nil {
		return err
	}
	return f(tf.Name())
}

// Apply applies the bundle's patch to the working tree (not the index) and
// writes its untracked files, refusing to overwrite any file or to write
// through a linked folder. It records what it did for Undo.
func (c *Checkout) Apply(ctx context.Context, b *Bundle) error {
	if len(b.Patch) > 0 {
		if err := withPatch(b.Patch, func(name string) error {
			_, err := gitRaw(ctx, c.root, gitOpts{}, "apply", "--binary", name)
			return err
		}); err != nil {
			return err
		}
		c.patch = b.Patch
	}
	for _, u := range b.Untracked {
		if err := c.writeUntracked(u); err != nil {
			return fmt.Errorf("untracked file %s: %w", u.Name, err)
		}
	}
	return nil
}

// writeUntracked writes one untracked file with O_EXCL. Each folder on the
// way is checked with Lstat: it must be a real folder, not a link, so a file
// never lands outside the clone. Missing folders are made and recorded.
func (c *Checkout) writeUntracked(u File) error {
	parts := strings.Split(u.Name, "/")
	dir := c.root
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		fi, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(dir, 0o755); err != nil {
				return err
			}
			c.dirs = append(c.dirs, dir)
		case err != nil:
			return err
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symbolic link", part)
		case !fi.IsDir():
			return fmt.Errorf("%s is not a folder", part)
		}
	}
	mode := os.FileMode(0o644)
	if u.Exec {
		mode = 0o755
	}
	p := filepath.Join(dir, parts[len(parts)-1])
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, werr := f.Write(u.Data)
	if err := errors.Join(werr, f.Close()); err != nil {
		os.Remove(p)
		return err
	}
	c.wrote = append(c.wrote, u)
	return nil
}

// Undo takes back what Apply did, and only that: it removes the untracked
// files Apply wrote while they still hold what it wrote, removes the
// folders it made once they are empty, reverses the patch with
// git apply -R, and puts the branch back (restore). It never resets or
// cleans the tree, so work that is not the move's survives. A file that
// changed since stays, and the error names it; when the patch no longer
// reverses, the tree and the branch stay as they are.
func (c *Checkout) Undo(ctx context.Context) error {
	var errs []error
	for i := len(c.wrote) - 1; i >= 0; i-- {
		u := c.wrote[i]
		p := filepath.Join(c.root, filepath.FromSlash(u.Name))
		data, err := os.ReadFile(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err == nil && bytes.Equal(data, u.Data):
			errs = append(errs, os.Remove(p))
		default:
			errs = append(errs, fmt.Errorf("kept %s: it changed after the move wrote it", u.Name))
		}
	}
	for i := len(c.dirs) - 1; i >= 0; i-- {
		os.Remove(c.dirs[i]) // only succeeds when empty
	}
	if c.patch != nil {
		if err := withPatch(c.patch, func(name string) error {
			_, err := gitRaw(ctx, c.root, gitOpts{}, "apply", "-R", "--binary", name)
			return err
		}); err != nil {
			return errors.Join(append(errs, fmt.Errorf("reverse the changes: %w", err))...)
		}
	}
	errs = append(errs, c.restore(ctx))
	return errors.Join(errs...)
}

// projectName is Claude Code's project folder name for cwd: every character
// that is not an ASCII letter or digit becomes "-", once per UTF-16 unit.
func projectName(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteString(strings.Repeat("-", max(utf16.RuneLen(r), 1)))
	}
	return b.String()
}

// ProjectDir is <claudeDir>/projects/<folder of cwd>. A name over 200
// characters gets a hash suffix in Claude Code that sessionhub does not compute, so
// it uses the one existing folder that starts with the first 200, else
// fails.
func ProjectDir(claudeDir, cwd string) (string, error) {
	name := projectName(cwd)
	base := filepath.Join(claudeDir, "projects")
	if len(name) <= maxProjectName {
		return filepath.Join(base, name), nil
	}
	m, _ := filepath.Glob(filepath.Join(base, name[:maxProjectName]+"*"))
	if len(m) == 1 {
		return m[0], nil
	}
	return "", fmt.Errorf("the project folder name for %s is longer than %d characters; start Claude there once, then move again", cwd, maxProjectName)
}

// WriteSession writes the bundle's transcript and sidecar folder into the
// project folder of cwd, and its file history, each file new (mode 0600).
// It fails when the session already has a transcript anywhere here. On a
// failure it removes what it wrote; on success it returns the paths, for
// RemoveFiles.
func WriteSession(claudeDir, cwd string, b *Bundle) ([]string, error) {
	id := b.Manifest.SessionID
	if ts, err := FindTranscripts(claudeDir, id); err != nil {
		return nil, err
	} else if len(ts) > 0 {
		return nil, fmt.Errorf("session %s already has a transcript here: %s", id, ts[0])
	}
	proj, err := ProjectDir(claudeDir, cwd)
	if err != nil {
		return nil, err
	}
	var written []string
	write := func(p string, data []byte) error {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		written = append(written, p)
		_, werr := f.Write(data)
		return errors.Join(werr, f.Close())
	}
	err = write(filepath.Join(proj, id+".jsonl"), b.Transcript)
	for _, f := range b.Sidecar {
		if err == nil {
			err = write(filepath.Join(proj, id, filepath.FromSlash(f.Name)), f.Data)
		}
	}
	for _, f := range b.History {
		if err == nil {
			err = write(filepath.Join(claudeDir, "file-history", id, filepath.FromSlash(f.Name)), f.Data)
		}
	}
	if err != nil {
		RemoveFiles(written)
		return nil, err
	}
	return written, nil
}

// RemoveFiles removes the files WriteSession wrote, then the folders they
// leave empty, deepest first.
func RemoveFiles(paths []string) {
	var dirs []string
	for _, p := range paths {
		os.Remove(p)
		for d := filepath.Dir(p); !slices.Contains(dirs, d); d = filepath.Dir(d) {
			dirs = append(dirs, d)
			if base := filepath.Base(filepath.Dir(d)); base == "projects" || base == "file-history" || d == filepath.Dir(d) {
				break
			}
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		os.Remove(d) // only succeeds when empty
	}
}
```

Run: `go test ./internal/move/ -count=1`
Expected: PASS.

- [ ] **Step 4: Write the failing move-in tests**

In `internal/plugin/move_helpers_test.go`, add at the end:

```go
// targetHerdr is bluebox's herdr: no panes until agent.start, which makes the
// new workspace's root pane w5:p1 run Claude, until /exit.
type targetHerdr struct {
	mu      sync.Mutex
	started bool
	cwd     string   // workspace.create's cwd
	args    []string // agent.start's args
	prompts []string // agent.prompt's texts
}

// state is whether Claude runs in w5:p1, and the prompts typed there.
func (th *targetHerdr) state() (bool, []string) {
	th.mu.Lock()
	defer th.mu.Unlock()
	return th.started, append([]string(nil), th.prompts...)
}

func newTargetHerdr(t *testing.T) (*targetHerdr, *herdrtest.Server) {
	t.Helper()
	th := &targetHerdr{}
	srv := herdrtest.New(t, "session-snapshot.ndjson")
	srv.Handle("session.snapshot", func(json.RawMessage) (any, string) {
		return map[string]any{"snapshot": map[string]any{"panes": []any{}}}, ""
	})
	srv.Handle("workspace.create", func(params json.RawMessage) (any, string) {
		var q struct {
			CWD string `json:"cwd"`
		}
		json.Unmarshal(params, &q)
		th.mu.Lock()
		th.cwd = q.CWD
		th.mu.Unlock()
		return map[string]any{"type": "workspace_created", "workspace": map[string]any{"workspace_id": "w5"},
			"root_pane": map[string]any{"pane_id": "w5:p1", "workspace_id": "w5"}}, ""
	})
	srv.Handle("agent.start", func(params json.RawMessage) (any, string) {
		var q struct {
			Args []string `json:"args"`
		}
		json.Unmarshal(params, &q)
		th.mu.Lock()
		defer th.mu.Unlock()
		th.started, th.args = true, q.Args
		return map[string]any{"type": "agent_started"}, ""
	})
	srv.Handle("agent.prompt", func(params json.RawMessage) (any, string) {
		var q struct{ Text string }
		json.Unmarshal(params, &q)
		th.mu.Lock()
		defer th.mu.Unlock()
		th.prompts = append(th.prompts, q.Text)
		if q.Text == "/exit" {
			th.started = false
		}
		return map[string]any{"type": "agent_prompted"}, ""
	})
	srv.Handle("pane.get", func(params json.RawMessage) (any, string) {
		var q struct {
			PaneID string `json:"pane_id"`
		}
		json.Unmarshal(params, &q)
		if q.PaneID != "w5:p1" {
			return nil, "pane_not_found"
		}
		th.mu.Lock()
		defer th.mu.Unlock()
		p := map[string]any{"pane_id": "w5:p1", "workspace_id": "w5", "agent_status": "idle"}
		if th.started {
			p["agent"] = "claude"
		}
		return map[string]any{"pane": p}, ""
	})
	return th, srv
}

// targetEnv is bluebox: its own ~/.claude and state dir, a search root that
// holds a clone of the source's origin named App, its herdr, and its mover.
type targetEnv struct {
	claude, state, root, clone string
	herdr                      *targetHerdr
	mover                      *mover
}

func newTargetEnv(t *testing.T, e *moveEnv) *targetEnv {
	t.Helper()
	tt := &targetEnv{claude: t.TempDir(), state: t.TempDir(), root: t.TempDir()}
	tt.clone = filepath.Join(tt.root, "App")
	gitT(t, tt.root, "clone", "-q", e.origin, tt.clone)
	var srv *herdrtest.Server
	tt.herdr, srv = newTargetHerdr(t)
	tt.mover = newMover(srv.Path, tt.claude, tt.state, []string{tt.root}, e.sessionhub.bluebox.key, log.New(e.logs, "bluebox ", 0))
	fastMover(tt.mover)
	return tt
}

// packed moves the session from tower toward bluebox up to the upload, and
// returns bluebox's move-in claim.
func (e *moveEnv) packed(t *testing.T) api.ControlClaim {
	t.Helper()
	claim := e.startMove(t, "bluebox")
	e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
	if mv := e.sessionhub.move(t, claim.Move.ID); mv.State != api.MoveUploaded {
		t.Fatalf("pack: %+v (logs:\n%s)", mv, e.logs)
	}
	return e.sessionhub.claim(t, e.sessionhub.bluebox)
}
```

In `internal/plugin/move_test.go`, add `"crypto/ecdh"` and `"crypto/rand"`
to the imports, and add at the end:

```go
func TestMoveInResumesOnTarget(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nunstaged\n", 0o644)
	writeFile(t, filepath.Join(e.work, "staged.txt"), "staged\n", 0o644)
	gitT(t, e.work, "add", "staged.txt")
	writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
	writeFile(t, filepath.Join(e.work, ".env"), "SECRET=1\n", 0o600)
	tt := newTargetEnv(t, e)

	in := e.packed(t)
	if in.Request.Action != api.ActionMoveIn {
		t.Fatalf("bluebox got %+v", in.Request)
	}
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	mv := e.sessionhub.move(t, in.Move.ID)
	if mv.State != api.MoveDone || mv.Detail != "not carried: .env" {
		t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if s, _ := e.sessionhub.st.GetSession(ctx, moveSID); s.Machine != "bluebox" {
		t.Errorf("owner %s", s.Machine)
	}
	if got, want := gitT(t, tt.clone, "rev-parse", "HEAD"), gitT(t, e.work, "rev-parse", "HEAD"); got != want ||
		gitT(t, tt.clone, "symbolic-ref", "--short", "HEAD") != "main" {
		t.Errorf("clone at %s, want %s on main", got, want)
	}
	for _, f := range []string{"a.txt", "staged.txt", "notes.md"} {
		want, _ := os.ReadFile(filepath.Join(e.work, f))
		got, _ := os.ReadFile(filepath.Join(tt.clone, f))
		if string(got) != string(want) {
			t.Errorf("%s on bluebox: %q, want %q", f, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(tt.clone, ".env")); !os.IsNotExist(err) {
		t.Errorf(".env was carried: %v", err)
	}
	proj, _ := move.ProjectDir(tt.claude, tt.clone)
	if b, err := os.ReadFile(filepath.Join(proj, moveSID+".jsonl")); err != nil || !strings.Contains(string(b), `"message":"hi"`) {
		t.Errorf("transcript on bluebox: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(tt.claude, "file-history", moveSID, "h@v1")); err != nil {
		t.Errorf("file history on bluebox: %v", err)
	}
	if tt.herdr.cwd != tt.clone || !slices.Equal(tt.herdr.args, []string{"--resume", moveSID}) {
		t.Errorf("started in %q with %q", tt.herdr.cwd, tt.herdr.args)
	}
	// The source's finish archives its copy.
	fin := e.sessionhub.claim(t, e.sessionhub.tower)
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 0 {
		t.Errorf("the source can still resume it: %v", ts)
	}
}

func TestMoveInFailures(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, e *moveEnv, tt *targetEnv)
		prefix string
		has    string
	}{
		// The detail is "find clone: no clone of <origin path> on bluebox";
		// TestFindClone pins the middle.
		{"no clone", func(t *testing.T, e *moveEnv, tt *targetEnv) { os.RemoveAll(tt.clone) },
			"find clone: no clone of ", " on bluebox"},
		{"dirty clone", func(t *testing.T, e *moveEnv, tt *targetEnv) {
			writeFile(t, filepath.Join(tt.clone, "a.txt"), "local edit\n", 0o644)
		}, "check clone: ", "uncommitted changes"},
		{"transcript exists", func(t *testing.T, e *moveEnv, tt *targetEnv) {
			writeFile(t, filepath.Join(tt.claude, "projects", "-old", moveSID+".jsonl"), "{}\n", 0o600)
		}, "transcript: ", "already has a transcript"},
		{"other Claude Code version", func(t *testing.T, e *moveEnv, tt *targetEnv) {
			p := filepath.Join(t.TempDir(), "claude")
			writeFile(t, p, "#!/bin/sh\necho '2.1.999 (Claude Code)'\n", 0o755)
			tt.mover.claudeBin = p
		}, "claude version: ", "2.1.999 here, 2.1.285 on tower"},
		{"wrong sender", func(t *testing.T, e *moveEnv, tt *targetEnv) {
			// The server now hands bluebox a different key for tower.
			k, _ := ecdh.X25519().GenerateKey(rand.Reader)
			e.sessionhub.st.SetMoveKey(context.Background(), e.sessionhub.tower.id, move.PublicKeyString(k))
		}, "open: ", "did not open"},
		{"patch creates a file bluebox has", func(t *testing.T, e *moveEnv, tt *targetEnv) {
			writeFile(t, filepath.Join(tt.clone, "made.txt"), "bluebox's own\n", 0o644)
		}, "check clone: ", "made.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newMoveEnv(t, "idle")
			ctx := context.Background()
			writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
			writeFile(t, filepath.Join(e.work, "made.txt"), "staged new file\n", 0o644)
			gitT(t, e.work, "add", "made.txt")
			tt := newTargetEnv(t, e)
			claim := e.startMove(t, "bluebox")
			e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
			c.setup(t, e, tt)
			before := cloneState(t, tt.clone)
			in := e.sessionhub.claim(t, e.sessionhub.bluebox)
			tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
			mv := e.sessionhub.move(t, claim.Move.ID)
			if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, c.prefix) || !strings.Contains(mv.Detail, c.has) {
				t.Fatalf("move %+v, want %q ... %q (logs:\n%s)", mv, c.prefix, c.has, e.logs)
			}
			if after := cloneState(t, tt.clone); after != before {
				t.Errorf("the clone changed:\nbefore %s\nafter  %s", before, after)
			}
			if tt.herdr.started {
				t.Error("bluebox started Claude")
			}
			// The source gets the failure and restarts the session.
			fin := e.sessionhub.claim(t, e.sessionhub.tower)
			e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
			if _, starts := e.pane.snapshot(); len(starts) != 1 {
				t.Errorf("source restarts: %v", starts)
			}
		})
	}
}

// cloneState is the clone's branch, HEAD, status, and file list, or "none".
func cloneState(t *testing.T, clone string) string {
	t.Helper()
	if _, err := os.Stat(clone); err != nil {
		return "none"
	}
	var files []string
	filepath.WalkDir(clone, func(p string, d os.DirEntry, err error) error {
		if d != nil && d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d != nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			files = append(files, strings.TrimPrefix(p, clone)+"="+string(b))
		}
		return nil
	})
	return gitT(t, clone, "symbolic-ref", "--short", "HEAD") + " " + gitT(t, clone, "rev-parse", "HEAD") + " " +
		gitT(t, clone, "status", "--porcelain") + " " + strings.Join(files, ",")
}

func TestMoveInUndoAfterApplyFailure(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nchanged\n", 0o644)
	writeFile(t, filepath.Join(e.work, "made.txt"), "staged new file\n", 0o644)
	gitT(t, e.work, "add", "made.txt")
	writeFile(t, filepath.Join(e.work, "notes", "new.md"), "untracked\n", 0o644)
	tt := newTargetEnv(t, e)
	gitT(t, tt.clone, "checkout", "-q", "-b", "other")
	// bluebox's notes is a link to a folder outside the clone, so writing
	// notes/new.md fails after the patch went in. bluebox's own file is not the
	// move's and must survive the undo.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(tt.clone, "notes")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tt.clone, "mine.txt"), "bluebox's own\n", 0o644)
	in := e.packed(t)
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	mv := e.sessionhub.move(t, in.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "apply: ") || !strings.Contains(mv.Detail, "symbolic link") ||
		!strings.HasSuffix(mv.Detail, "; the clone is back on other") {
		t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if br := gitT(t, tt.clone, "symbolic-ref", "--short", "HEAD"); br != "other" {
		t.Errorf("clone on %s", br)
	}
	if b, _ := os.ReadFile(filepath.Join(tt.clone, "mine.txt")); string(b) != "bluebox's own\n" {
		t.Errorf("bluebox's own file changed: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(tt.clone, "a.txt")); string(b) != "one\n" {
		t.Errorf("a.txt: %q", b)
	}
	if _, err := os.Stat(filepath.Join(tt.clone, "made.txt")); !os.IsNotExist(err) {
		t.Errorf("made.txt left behind: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("wrote through the link: %v", entries)
	}
	if st := gitT(t, tt.clone, "status", "--porcelain"); st != "?? mine.txt\n?? notes" {
		t.Errorf("status:\n%s", st)
	}
}

func TestMoveInUndoAfterWriteFailure(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nchanged\n", 0o644)
	writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
	tt := newTargetEnv(t, e)
	// projects is a file, so the transcript cannot be written.
	writeFile(t, filepath.Join(tt.claude, "projects"), "not a folder", 0o600)
	in := e.packed(t)
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	mv := e.sessionhub.move(t, in.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "write transcript: ") {
		t.Fatalf("move %+v", mv)
	}
	if st := gitT(t, tt.clone, "status", "--porcelain"); st != "" {
		t.Errorf("the clone kept changes:\n%s", st)
	}
	if _, err := os.Stat(filepath.Join(tt.clone, "notes.md")); !os.IsNotExist(err) {
		t.Errorf("notes.md left behind: %v", err)
	}
}

// The move ends (here: fails) while bluebox unpacks. bluebox reads it back before it
// starts Claude, starts nothing, and puts everything back; the source gets
// the session.
func TestMoveInStopsWhenMoveEnded(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nchanged\n", 0o644)
	writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
	tt := newTargetEnv(t, e)
	in := e.packed(t)
	before := cloneState(t, tt.clone)
	e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/moves/"+in.Move.ID && isFrom(r, e.sessionhub.bluebox) {
			e.sessionhub.st.FinishMove(context.Background(), e.sessionhub.bluebox.id, in.Move.ID,
				api.MoveResultIn{State: api.MoveFailed, Detail: "unpacking: timed out"})
		}
		return false
	})
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	if started, prompts := tt.herdr.state(); started || len(prompts) != 0 {
		t.Errorf("bluebox started Claude for a move that ended: %v %q", started, prompts)
	}
	if ts, _ := move.FindTranscripts(tt.claude, moveSID); len(ts) != 0 {
		t.Errorf("transcript left on bluebox: %v", ts)
	}
	if after := cloneState(t, tt.clone); after != before {
		t.Errorf("the clone changed:\nbefore %s\nafter  %s", before, after)
	}
	if mv := e.sessionhub.move(t, in.Move.ID); mv.State != api.MoveFailed || mv.Detail != "unpacking: timed out" {
		t.Errorf("move %+v", mv)
	}
	fin := e.sessionhub.claim(t, e.sessionhub.tower)
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	if _, starts := e.pane.snapshot(); len(starts) != 1 {
		t.Errorf("source restarts: %v", starts)
	}
}

// bluebox started Claude, but the sessionhub never records its done: bluebox ends Claude
// again and puts everything back, so the session never runs on both
// machines.
func TestMoveInStopsWhenDoneNotRecorded(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nchanged\n", 0o644)
	writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
	tt := newTargetEnv(t, e)
	in := e.packed(t)
	before := cloneState(t, tt.clone)
	e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/result") && isFrom(r, e.sessionhub.bluebox) {
			badGateway(w)
			return true
		}
		return false
	})
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	if started, prompts := tt.herdr.state(); started || !slices.Equal(prompts, []string{"/exit"}) {
		t.Errorf("Claude on bluebox: running %v, prompts %q (logs:\n%s)", started, prompts, e.logs)
	}
	if ts, _ := move.FindTranscripts(tt.claude, moveSID); len(ts) != 0 {
		t.Errorf("transcript left on bluebox: %v", ts)
	}
	if after := cloneState(t, tt.clone); after != before {
		t.Errorf("the clone changed:\nbefore %s\nafter  %s", before, after)
	}
	if mv := e.sessionhub.move(t, in.Move.ID); mv.State != api.MoveUnpacking {
		t.Errorf("move %+v", mv)
	}
}
```


- [ ] **Step 5: Run them to verify they fail**

Run: `go test ./internal/plugin/ -run 'MoveIn' -count=1`
Expected: FAIL to compile with `undefined: PatchNewFiles` until Step 3's
code is in; then every move-in ends `failed` with "download: this watcher
cannot take moved sessions yet".

- [ ] **Step 6: Write the target's part**

In `internal/plugin/move.go`, add `"os"`, `"path/filepath"`, `"slices"`,
and `"strings"` to the imports, and replace:

```go
// moveIn is the target's part. This watcher cannot take moves yet.
func (m *mover) moveIn(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	m.postResult(ctx, cl, claim.Move.ID, failed(stepf("download", "this watcher cannot take moved sessions yet")))
}
```

with:

```go
// moveIn is the target's part: download and open the bundle, check the
// Claude Code version and that the session is new here, find a clean clone
// with the same remote, check out the source's HEAD, apply the changes,
// write the transcript, and start Claude while the move is still open.
// Every check that needs no change runs before the clone is touched; a
// failure after the checkout takes back what the move did, and nothing
// else.
func (m *mover) moveIn(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	mv := claim.Move
	m.log.Printf("move: %s: taking session %s from %s", mv.ID, claim.Session.ID, termtext.Clean(mv.Source, 64))
	fail := func(err error) { m.postResult(ctx, cl, mv.ID, failed(err)) }
	var buf bytes.Buffer
	if _, err := cl.GetMoveBundle(ctx, mv.ID, &buf); err != nil {
		fail(&stepError{"download", err})
		return
	}
	peer, err := move.ParsePublicKey(mv.PeerKey)
	if err != nil {
		fail(stepf("open", "the source's key: %v", err))
		return
	}
	plain, err := move.Open(m.key, peer, mv.ID, buf.Bytes())
	if err != nil {
		fail(&stepError{"open", err})
		return
	}
	b, err := move.Extract(plain)
	if err != nil {
		fail(&stepError{"open", err})
		return
	}
	man := b.Manifest
	if man.MoveID != mv.ID || man.SessionID != claim.Session.ID || man.SourceMachine != mv.Source {
		fail(stepf("open", "the bundle's manifest does not match the move"))
		return
	}
	version, err := move.ClaudeVersion(ctx, m.claudeBin)
	if err != nil {
		fail(&stepError{"claude version", err})
		return
	}
	if version != man.ClaudeVersion {
		fail(stepf("claude version", "Claude Code %s here, %s on %s; run the same version on both", version, man.ClaudeVersion, mv.Source))
		return
	}
	if ts, err := move.FindTranscripts(m.claudeDir, man.SessionID); err != nil {
		fail(&stepError{"transcript", err})
		return
	} else if len(ts) > 0 {
		fail(stepf("transcript", "session %s already has a transcript here: %s", man.SessionID, ts[0]))
		return
	}
	clone, err := move.FindClone(ctx, m.roots, man.Remote, man.RootRel)
	if errors.Is(err, move.ErrNoClone) {
		fail(stepf("find clone", "%v on %s", err, mv.Target))
		return
	}
	if err != nil {
		fail(&stepError{"find clone", err})
		return
	}
	carried := append(slices.Clone(man.Untracked), move.PatchNewFiles(b.Patch)...)
	if err := move.CheckClone(ctx, clone, carried); err != nil {
		fail(&stepError{"check clone", err})
		return
	}
	co, err := move.PrepareClone(ctx, clone, man.Branch, man.Head)
	if err != nil {
		fail(&stepError{"checkout", err})
		return
	}
	var written []string
	// undo removes the transcript it wrote and takes back what the move did
	// to the clone; the note says how that went.
	undo := func() string {
		move.RemoveFiles(written)
		if err := co.Undo(context.WithoutCancel(ctx)); err != nil {
			return "; putting the clone back failed: " + err.Error()
		}
		return "; the clone is back on " + co.Prev()
	}
	abort := func(err error) {
		note := undo()
		m.log.Printf("move: %s: %v%s", mv.ID, err, note)
		fail(errors.New(err.Error() + note))
	}
	if err := co.Apply(ctx, b); err != nil {
		abort(&stepError{"apply", err})
		return
	}
	cwd := filepath.Join(clone, filepath.FromSlash(man.RelPath))
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		abort(stepf("apply", "the session's directory %s is missing", cwd))
		return
	}
	if written, err = move.WriteSession(m.claudeDir, cwd, b); err != nil {
		abort(&stepError{"write transcript", err})
		return
	}
	// Start only while the move is open. Once it ended (a timeout), the
	// source restarts the session, so it must not start here too.
	if cur, err := cl.GetMove(ctx, mv.ID); err != nil || cur.State != api.MoveUnpacking {
		why := fmt.Sprint(err)
		if err == nil {
			why = "the move is " + cur.State
		}
		abort(stepf("start", "not starting: %s", why))
		return
	}
	s := api.Session{ID: man.SessionID, CWD: cwd, Title: claim.Session.Title}
	r := resume.StartResumed(ctx, m.socket, s, m.start)
	if r.Outcome != resume.OutcomeResumed {
		why := string(r.Outcome)
		if r.Err != nil {
			why = r.Err.Error()
		}
		if r.Outcome == resume.OutcomeError && r.Pane != "" {
			m.stopClaude(ctx, mv.ID, r.Pane)
		}
		abort(stepf("start", "%s", why))
		return
	}
	in := api.MoveResultIn{State: api.MoveDone}
	if len(man.Skipped) > 0 {
		in.Detail = "not carried: " + strings.Join(man.Skipped, ", ")
	}
	if m.postResult(ctx, cl, mv.ID, in) {
		m.log.Printf("move: %s: session %s resumed in %s", mv.ID, man.SessionID, cwd)
		return
	}
	// The sessionhub did not record done. Unless the move reads back done, the
	// source gets the session back, so Claude must not stay here.
	if cur, err := cl.GetMove(context.WithoutCancel(ctx), mv.ID); err == nil && cur.State == api.MoveDone {
		m.log.Printf("move: %s: session %s resumed in %s", mv.ID, man.SessionID, cwd)
		return
	}
	m.stopClaude(ctx, mv.ID, r.Pane)
	m.log.Printf("move: %s: done was not recorded, so session %s stopped here%s", mv.ID, man.SessionID, undo())
}

// stopClaude ends Claude with /exit in a pane this watcher started.
func (m *mover) stopClaude(ctx context.Context, id, pane string) {
	if err := m.endSession(context.WithoutCancel(ctx), pane, ""); err != nil {
		m.log.Printf("move: %s: end Claude in pane %s: %v; end it by hand", id, termtext.Clean(pane, 64), err)
	}
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/plugin/ ./internal/move/ -count=1 -race`
Expected: PASS. `TestMoveInFailures` shows every early failure leaves the
clone byte for byte as it was, and the source restarts the session each
time. `TestMoveInStopsWhenMoveEnded` and `TestMoveInStopsWhenDoneNotRecorded`
show the session never runs on both machines.

- [ ] **Step 8: Document the target's part**

In `docs/plugin.md`, section "Moves", before the paragraph that starts
"Log lines start with `move:`", add:

```markdown
**Target (`move-in` while `unpacking`).** Steps, in this order:

1. `download`, `open`: the sealed bundle from `GET /v1/moves/{id}/bundle`,
   opened with this machine's key and the source's registered key (a bundle
   sealed by anyone else fails), and checked: entry names, the manifest's
   move, session, and source.
2. `claude version`: `claude --version` here must equal the source's.
3. `transcript`: no transcript of the session may exist anywhere under
   `~/.claude/projects` here.
4. `find clone`: a Git repository under `~/Code` or a `move_roots` entry, at
   most 4 levels down, whose `origin` is the same remote (scheme, user,
   `.git`, and host case ignored). With several, the one whose path under
   its root matches the source's, ignoring case (`OTGS/app` and `otgs/app`).
5. `check clone`: no uncommitted tracked change, and none of the carried
   untracked files, and no file the patch creates, already there.
6. `checkout`: `git fetch origin`, check out the branch (a tracking branch
   from `origin` if there is no local one), fast-forward it to the source's
   HEAD. A branch with commits the source lacks fails; nothing is rewritten.
7. `apply`: `git apply --binary` of the changes, then the untracked files,
   never over an existing file and never through a linked folder (each
   folder on the way is checked with `lstat`).
8. `write transcript`: the transcript and sidecar folder into
   `~/.claude/projects/<folder of the new directory>/`, the file history into
   `~/.claude/file-history/<id>/`.
9. `start`: the watcher reads the move again and starts only while it is
   still `unpacking` (a move that timed out goes back to the source). Then
   `claude --resume <id>` in a new workspace at the session's directory in
   this clone, and `done`. The detail lists the files the source did not
   carry (`not carried: .env`): copy those by hand. If the sessionhub does not
   record the `done`, and the move does not read back as `done`, the watcher
   ends Claude in that pane with `/exit`, removes the transcript it wrote,
   and puts the clone back, so the session never runs on both machines.

Steps 1 to 5 change nothing. A failure from step 6 on takes back what the
move did, and only that: it removes the untracked files it wrote (a file
changed since stays, and the detail names it) and the folders it made,
reverses the patch with `git apply -R`, checks out the branch the clone was
on, and puts the moved branch back where it was: a fast-forwarded branch
returns to its old commit, and a tracking branch the move created is
deleted. Both ref updates are compare-and-swap, so a change made since is
kept. The watcher never runs `git reset` or `git clean`, so work in the
clone that is not the move's survives. The detail ends with `; the clone is
back on <branch>`. The source then gets the failure as its finish and
restarts the session.
```

In `docs/client.md`, at the end of section "Moves", add:

```markdown
- **Target steps.** `FindClone` searches the roots (4 levels, skipping
  hidden folders, `node_modules`, and the inside of a repository) and
  returns `ErrNoClone` when nothing matches. `CheckClone` refuses tracked
  changes and carried paths that exist: the untracked files and the files
  the patch creates (`PatchNewFiles`). `PrepareClone` fetches `origin` and
  checks out the branch at the source's HEAD by fast-forward only;
  `Checkout.Apply` applies the patch to the working tree and writes the
  untracked files with `O_EXCL`, checking each folder on the way with
  `lstat` so nothing is written through a link. `Checkout.Undo` takes back
  only what `Apply` did: it removes the files it wrote while they hold what
  it wrote, reverses the patch with `git apply -R`, checks out the previous
  branch, and moves the branch back to its old commit, or deletes a
  tracking branch `PrepareClone` created, with compare-and-swap
  `update-ref`. `ProjectDir` is Claude Code's project
  folder for a directory (every character but an ASCII letter or digit
  becomes `-`; over 200 characters, the one existing folder with that
  prefix). `WriteSession` writes the transcript, sidecar folder, and file
  history with `O_EXCL` and mode `0600`, and `RemoveFiles` takes them back.
```

- [ ] **Step 9: Lint and commit**

Run: `gofmt -w internal/move/*.go internal/plugin/*.go && make lint`
Expected: clean.

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1`
Expected: every package `ok`.

```bash
git add internal/move/target.go internal/move/target_test.go internal/plugin/move.go \
  internal/plugin/move_helpers_test.go internal/plugin/move_test.go docs/plugin.md docs/client.md
git commit -m "$(cat <<'EOF'
Resume a moved session on the target machine

The target opens the bundle with its key and the source's registered
key, checks the Claude Code version and that the session is new here,
finds a clean clone with the same remote, fast-forwards the branch,
applies the changes and untracked files, writes the transcript, and
starts claude --resume in a new herdr workspace while the move is still
open. Every check that needs no change runs first, and a patch's new
files count as carried. A later failure takes back only what the move
did: its files, its patch (git apply -R), and its branch move. If the
sessionhub does not record done, the target ends Claude again.

Refs: docs/dev/superpowers/plans/2026-10-03-move-session.md, Task 8

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 9: Cloud move

**Files:**
- Create: `internal/move/cloud.go` (`CloudBranch`, `PushCloudBranch`,
  `HandOffPrompt`, `StartCloud`)
- Create: `internal/move/cloud_test.go`
- Modify: `internal/plugin/move.go` (`moveOut` hands a cloud move to
  `toCloud`; `cloudWait`)
- Modify: `internal/plugin/move_helpers_test.go` (`fastMover` sets
  `cloudWait`)
- Modify: `internal/plugin/move_test.go` (cloud tests)
- Modify: `docs/plugin.md`, `docs/client.md` (section "Moves")

**Interfaces:**
- Consumes: `git`, `gitRaw`, `gitOpts`, `gitNetTimeout`, `untrackedFiles`,
  `changedSecrets`, `secretPathspecs`, `Repo` (Task 2);
  `store.ValidRemoteControlURL`'s link shape (the server checks the link
  again); `api.Session`; the Task 7 mover (`endIdle`, `abort`,
  `postResult`).
- Produces: `CloudBranch`, `PushCloudBranch` (returns the branch and the
  files it left out), `HandOffPrompt`, `StartCloud`; `(*mover).toCloud`,
  `mover.cloudWait`.

- [ ] **Step 1: Write the failing cloud tests**

Create `internal/move/cloud_test.go`:

```go
package move

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestCloudBranch(t *testing.T) {
	if got := CloudBranch("5E5E5E5E-1111-4222-8333-444455556666"); got != "sessionhub/cloud-5e5e5e5e" {
		t.Errorf("CloudBranch = %q", got)
	}
}

// treeState is everything PushCloudBranch must leave alone: HEAD, the
// branch, the status, the index, and the working tree's files.
func treeState(t *testing.T, work string) string {
	t.Helper()
	index, err := os.ReadFile(filepath.Join(work, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	filepath.WalkDir(work, func(p string, d os.DirEntry, err error) error {
		if d != nil && d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d != nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			files = append(files, strings.TrimPrefix(p, work)+"="+string(b))
		}
		return nil
	})
	return gitT(t, work, "rev-parse", "HEAD") + "|" + gitT(t, work, "symbolic-ref", "HEAD") + "|" +
		gitT(t, work, "status", "--porcelain") + "|" + string(index) + "|" + strings.Join(files, ",") + "|" +
		gitT(t, work, "branch", "--list")
}

func TestPushCloudBranchLeavesTreeAlone(t *testing.T) {
	ctx := context.Background()
	origin, work := dirtyRepo(t)
	r, err := ReadRepo(ctx, work, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := treeState(t, work)
	branch, skipped, err := PushCloudBranch(ctx, r, testSessionID)
	if err != nil || branch != "sessionhub/cloud-11111111" {
		t.Fatalf("branch %q %v", branch, err)
	}
	slices.Sort(skipped)
	if want := []string{".env.local", "certs/server.pem", "conf/.env.prod", "deploy.key", "link", "prod.tfvars"}; !slices.Equal(skipped, want) {
		t.Errorf("skipped %q, want %q", skipped, want)
	}
	if after := treeState(t, work); after != before {
		t.Fatalf("the working tree, index, or branch changed:\nbefore %s\nafter  %s", before, after)
	}
	commit := gitT(t, origin, "rev-parse", branch)
	if parent := gitT(t, origin, "rev-parse", commit+"^"); parent != r.Head {
		t.Errorf("parent %s, want HEAD %s", parent, r.Head)
	}
	files := gitT(t, origin, "ls-tree", "-r", "--name-only", commit)
	for _, want := range []string{"a.txt", "bin.dat", "notes/new.md", "run.sh", "sub/keep.txt"} {
		if !strings.Contains("\n"+files+"\n", "\n"+want+"\n") {
			t.Errorf("cloud commit lacks %s:\n%s", want, files)
		}
	}
	for _, bad := range []string{"gone.txt", ".env.local", "certs/server.pem", "deploy.key", "prod.tfvars", "link", "build/out.o"} {
		if strings.Contains("\n"+files+"\n", "\n"+bad+"\n") {
			t.Errorf("cloud commit has %s", bad)
		}
	}
	if got := gitT(t, origin, "show", commit+":a.txt"); got != "one\nstaged\nunstaged" {
		t.Errorf("a.txt in the cloud commit: %q", got)
	}
	// The tracked secret keeps HEAD's content: its change stays local.
	if got := gitT(t, origin, "show", commit+":conf/.env.prod"); got != "secret" {
		t.Errorf("conf/.env.prod in the cloud commit: %q", got)
	}
	// A retry replaces the branch sessionhub pushed, with a lease on that commit.
	writeFile(t, filepath.Join(work, "notes", "new.md"), "newer\n", 0o644)
	if _, _, err := PushCloudBranch(ctx, r, testSessionID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := gitT(t, origin, "show", branch+":notes/new.md"); got != "newer" {
		t.Errorf("after the retry: %q", got)
	}
	// A cloud session pushed to the branch: the next retry refuses rather
	// than drop its commit.
	cloud := filepath.Join(t.TempDir(), "cloud")
	gitT(t, filepath.Dir(cloud), "clone", "-q", "-b", branch, origin, cloud)
	writeFile(t, filepath.Join(cloud, "cloud.txt"), "from the cloud\n", 0o644)
	gitT(t, cloud, "add", "cloud.txt")
	gitT(t, cloud, "commit", "-qm", "cloud work")
	gitT(t, cloud, "push", "-q")
	cloudTip := gitT(t, origin, "rev-parse", branch)
	writeFile(t, filepath.Join(work, "notes", "new.md"), "newest\n", 0o644)
	if _, _, err := PushCloudBranch(ctx, r, testSessionID); err == nil || !strings.Contains(err.Error(), "did not push") {
		t.Errorf("over a cloud session's commit: %v", err)
	}
	if got := gitT(t, origin, "rev-parse", branch); got != cloudTip {
		t.Errorf("the cloud session's commit was dropped: %s, want %s", got, cloudTip)
	}
}

func TestPushCloudBranchCleanTree(t *testing.T) {
	ctx := context.Background()
	origin, work := newRepo(t)
	r, _ := ReadRepo(ctx, work, nil)
	branch, skipped, err := PushCloudBranch(ctx, r, testSessionID)
	if err != nil || branch != "main" || len(skipped) != 0 {
		t.Errorf("clean tree: %q %q %v", branch, skipped, err)
	}
	if out := gitT(t, origin, "branch", "--list", "sessionhub/*"); out != "" {
		t.Errorf("a clean tree pushed %q", out)
	}
}

func TestHandOffPrompt(t *testing.T) {
	s := api.Session{ID: "5e5e5e5e-1111-4222-8333-444455556666", Recap: "Fixed the\nlogin bug.", LastPrompt: "run the tests",
		LatestReport: &api.Report{Done: []string{"fix", "tests"}, InFlight: []string{"docs"}, WaitingOn: []string{}}}
	want := "Continue work moved from a local Claude Code session (5e5e5e5e on bluebox).\n" +
		"Branch: sessionhub/cloud-5e5e5e5e.\n" +
		"Recap: Fixed the login bug.\n" +
		"Last request: run the tests\n" +
		"Status: done: fix; tests / in flight: docs"
	if got := HandOffPrompt(s, "bluebox", "sessionhub/cloud-5e5e5e5e"); got != want {
		t.Errorf("prompt:\n%s\nwant:\n%s", got, want)
	}
	bare := HandOffPrompt(api.Session{ID: s.ID}, "bluebox", "main")
	if bare != "Continue work moved from a local Claude Code session (5e5e5e5e on bluebox).\nBranch: main.\nRecap: none\nLast request: none" {
		t.Errorf("bare prompt:\n%s", bare)
	}
}

func TestStartCloud(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	args := filepath.Join(t.TempDir(), "args")
	const link = "https://claude.ai/code/session_01CloudCloudCloudCloud1"
	ok := fakeClaude(t, `printf '%s\n' "$1" "$2" > `+args+`; pwd >> `+args+`
echo "Creating a cloud session..."
printf '\033[1mView it at `+link+`\033[0m\n'
exec sleep 30`)
	start := time.Now()
	got, err := StartCloud(ctx, ok, dir, "line one\nline two", 10*time.Second)
	if err != nil || got != link {
		t.Fatalf("link %q %v", got, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("StartCloud waited %s after the link", time.Since(start))
	}
	if b, _ := os.ReadFile(args); string(b) != "--cloud\nline one\nline two\n"+dir+"\n" {
		t.Errorf("claude got %q", b)
	}
	atEnd := fakeClaude(t, `printf 'link: `+link+`'`)
	if got, err := StartCloud(ctx, atEnd, dir, "p", 10*time.Second); err != nil || got != link {
		t.Errorf("link at the very end: %q %v", got, err)
	}
	fails := fakeClaude(t, `echo "error: cloud sessions need a claude.ai sign-in" >&2; exit 1`)
	if _, err := StartCloud(ctx, fails, dir, "p", 10*time.Second); err == nil || !strings.Contains(err.Error(), "claude.ai sign-in") {
		t.Errorf("failure: %v", err)
	}
	silent := fakeClaude(t, `exec sleep 30`)
	if _, err := StartCloud(ctx, silent, dir, "p", 200*time.Millisecond); err == nil || !strings.Contains(err.Error(), "no cloud session link within") {
		t.Errorf("silence: %v", err)
	}
}
```

Create `internal/move/cloud.go`:

```go
package move

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
)

// cloudLinkRE is the cloud session link in claude --cloud's output.
var cloudLinkRE = regexp.MustCompile(`https://claude\.ai/code/session_[A-Za-z0-9_-]+`)

const (
	maxRecapRunes = 1000
	outputTail    = 64 << 10
)

func shortSessionID(id string) string {
	if len(id) > 8 {
		id = id[:8]
	}
	return strings.ToLower(id)
}

// CloudBranch is the sessionhub-owned branch for session id's uncommitted work.
func CloudBranch(id string) string { return "sessionhub/cloud-" + shortSessionID(id) }

// cloudMessage is the message of every commit sessionhub makes on CloudBranch(id).
func cloudMessage(id string) string {
	return "sessionhub: uncommitted work from session " + shortSessionID(id)
}

// PushCloudBranch records the working tree's changes (tracked changes and
// untracked files, without secret-looking files or files over 10 MiB) as one
// commit on top of HEAD, built in a temporary index so the working tree, the
// real index, and the current branch do not change, and pushes it to the
// sessionhub-owned CloudBranch with --force-with-lease (cloudLease). It returns the
// branch and the files it left out. With nothing to record it returns the
// current branch and pushes nothing.
func PushCloudBranch(ctx context.Context, r Repo, sessionID string) (string, []string, error) {
	files, skipped, err := untrackedFiles(ctx, r.Root)
	if err != nil {
		return "", nil, err
	}
	secret, err := changedSecrets(ctx, r.Root)
	if err != nil {
		return "", nil, err
	}
	skipped = append(secret, skipped...)
	tmp, err := os.MkdirTemp("", "sessionhub-cloud-index-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(tmp)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	if _, err := gitRaw(ctx, r.Root, gitOpts{env: env}, "read-tree", "HEAD"); err != nil {
		return "", nil, err
	}
	if _, err := gitRaw(ctx, r.Root, gitOpts{env: env}, append([]string{"add", "-u", "--"}, secretPathspecs(true)...)...); err != nil {
		return "", nil, err
	}
	if len(files) > 0 {
		var names bytes.Buffer
		for _, f := range files {
			names.WriteString(f.Name)
			names.WriteByte(0)
		}
		if _, err := gitRaw(ctx, r.Root, gitOpts{env: env, stdin: &names}, "add", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return "", nil, err
		}
	}
	out, err := gitRaw(ctx, r.Root, gitOpts{env: env}, "write-tree")
	if err != nil {
		return "", nil, err
	}
	tree := strings.TrimSpace(string(out))
	head, err := git(ctx, r.Root, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", nil, err
	}
	if head == tree {
		return r.Branch, skipped, nil
	}
	commit, err := git(ctx, r.Root, "commit-tree", tree, "-p", "HEAD", "-m", cloudMessage(sessionID))
	if err != nil {
		return "", nil, err
	}
	branch := CloudBranch(sessionID)
	lease, err := cloudLease(ctx, r, sessionID, branch)
	if err != nil {
		return "", nil, err
	}
	if _, err := gitRaw(ctx, r.Root, gitOpts{timeout: gitNetTimeout}, "push", "--force-with-lease=refs/heads/"+branch+":"+lease,
		"origin", commit+":refs/heads/"+branch); err != nil {
		return "", nil, err
	}
	return branch, skipped, nil
}

// cloudLease is the commit sessionhub expects on the push remote's CloudBranch:
// "" (the branch must not exist yet), or the last commit sessionhub pushed there,
// which this repository has and whose message is cloudMessage. Anything
// else, such as commits a cloud session pushed, is refused, so a retry never
// drops them.
func cloudLease(ctx context.Context, r Repo, sessionID, branch string) (string, error) {
	url, err := git(ctx, r.Root, "remote", "get-url", "--push", "origin")
	if err != nil {
		return "", err
	}
	out, err := gitRaw(ctx, r.Root, gitOpts{timeout: gitNetTimeout}, "ls-remote", "--", url, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "", nil
	}
	raw, err := git(ctx, r.Root, "cat-file", "commit", f[0])
	if _, msg, ok := strings.Cut(raw, "\n\n"); err == nil && ok && strings.TrimSpace(msg) == cloudMessage(sessionID) {
		return f[0], nil
	}
	return "", fmt.Errorf("origin's %s holds commits sessionhub did not push there (a cloud session's work?); merge or delete that branch, then move again", branch)
}

// HandOffPrompt is the first prompt of the cloud session.
func HandOffPrompt(s api.Session, machine, branch string) string {
	orNone := func(v string, n int) string {
		if v = termtext.Clean(v, n); strings.TrimSpace(v) == "" {
			return "none"
		}
		return v
	}
	lines := []string{
		fmt.Sprintf("Continue work moved from a local Claude Code session (%s on %s).", shortSessionID(s.ID), machine),
		"Branch: " + branch + ".",
		"Recap: " + orNone(s.Recap, maxRecapRunes),
		"Last request: " + orNone(s.LastPrompt, 0),
	}
	if r := s.LatestReport; r != nil {
		var parts []string
		for _, p := range []struct {
			name  string
			items []string
		}{{"done", r.Done}, {"in flight", r.InFlight}, {"waiting on", r.WaitingOn}} {
			if len(p.items) > 0 {
				parts = append(parts, p.name+": "+termtext.Clean(strings.Join(p.items, "; "), 500))
			}
		}
		if len(parts) > 0 {
			lines = append(lines, "Status: "+strings.Join(parts, " / "))
		}
	}
	return strings.Join(lines, "\n")
}

// linkWatcher collects a command's output and calls found once it holds a
// whole cloud session link.
type linkWatcher struct {
	mu    sync.Mutex
	buf   []byte
	link  string
	found func()
}

func (w *linkWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > outputTail {
		w.buf = w.buf[len(w.buf)-outputTail:]
	}
	if w.link == "" {
		// A link that ends the buffer may continue in the next write.
		if loc := cloudLinkRE.FindIndex(w.buf); loc != nil && loc[1] < len(w.buf) {
			w.link = string(w.buf[loc[0]:loc[1]])
			w.found()
		}
	}
	return len(p), nil
}

// lastLine is the last non-blank output line, cleaned.
func (w *linkWatcher) lastLine() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	lines := strings.Split(string(w.buf), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := termtext.Clean(lines[i], 200); strings.TrimSpace(l) != "" {
			return l
		}
	}
	return "no output"
}

// StartCloud runs `claude --cloud <prompt>` in dir and returns the first
// cloud session link it prints. It stops the command once the link is out,
// and gives up after wait.
func StartCloud(ctx context.Context, bin, dir, prompt string, wait time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	w := &linkWatcher{found: cancel}
	cmd := exec.CommandContext(ctx, bin, "--cloud", prompt)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = w, w
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	w.mu.Lock()
	link := w.link
	if link == "" {
		link = string(cloudLinkRE.Find(w.buf))
	}
	w.mu.Unlock()
	switch {
	case link != "":
		return link, nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", fmt.Errorf("no cloud session link within %s: %s", wait, w.lastLine())
	case err != nil:
		return "", fmt.Errorf("claude --cloud: %v: %s", err, w.lastLine())
	}
	return "", fmt.Errorf("claude --cloud printed no session link: %s", w.lastLine())
}
```

- [ ] **Step 2: Run the move tests**

Run: `go test ./internal/move/ -run 'Cloud|HandOff' -count=1`
Expected: PASS. `TestPushCloudBranchLeavesTreeAlone` proves the working
tree, the index file, HEAD, the branch, and the branch list are byte for
byte the same after the push.

- [ ] **Step 3: Write the failing watcher tests**

In `internal/plugin/move_helpers_test.go`, replace:

```go
	m.resultPause = time.Millisecond
}
```

with:

```go
	m.resultPause = time.Millisecond
	m.cloudWait = 10 * time.Second
}

// onGitHub makes the session's origin look like a GitHub remote, while
// pushes still go to the local bare repository.
func (e *moveEnv) onGitHub(t *testing.T) {
	t.Helper()
	const gh = "https://github.com/o/app.git"
	gitT(t, e.work, "remote", "set-url", "origin", gh)
	gitT(t, e.work, "config", "url."+e.origin+".pushInsteadOf", gh)
	if _, err := e.sessionhub.st.UpsertSession(context.Background(), e.sessionhub.tower.id, api.SessionUpsert{ID: moveSID,
		Source: api.SourcePlugin, GitRepo: gh}); err != nil {
		t.Fatal(err)
	}
}
```

In `internal/plugin/move_test.go`, add at the end:

```go
const cloudLink = "https://claude.ai/code/session_01CloudCloudCloudCloud1"

// cloudClaude is a fake claude whose --cloud writes its prompt to
// $SESSIONHUB_TEST_PROMPT and prints a link, or fails when $SESSIONHUB_TEST_CLOUD_FAIL
// is set.
const cloudClaude = `case "$1" in
--version) echo "2.1.285 (Claude Code)";;
--cloud)
  printf '%s' "$2" > "$SESSIONHUB_TEST_PROMPT"
  if [ -n "$SESSIONHUB_TEST_CLOUD_FAIL" ]; then echo "error: $SESSIONHUB_TEST_CLOUD_FAIL" >&2; exit 1; fi
  echo "Creating a cloud session..."
  echo "View it at ` + cloudLink + `"
  exec sleep 30;;
esac`

func TestCloudMove(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	fakeClaude(t, cloudClaude)
	prompt := filepath.Join(t.TempDir(), "prompt")
	t.Setenv("SESSIONHUB_TEST_PROMPT", prompt)
	e.onGitHub(t)
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nwip\n", 0o644)
	before := gitT(t, e.work, "status", "--porcelain")

	claim := e.startMove(t, api.MoveCloud)
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveDone || mv.CloudURL != cloudLink {
		t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if s, _ := e.sessionhub.st.GetSession(ctx, moveSID); s.Status != api.StatusEnded {
		t.Errorf("session %s after a cloud move", s.Status)
	}
	branch := "sessionhub/cloud-5e5e5e5e"
	if got := gitT(t, e.origin, "show", branch+":a.txt"); got != "one\nwip" {
		t.Errorf("cloud branch a.txt: %q", got)
	}
	if after := gitT(t, e.work, "status", "--porcelain"); after != before || gitT(t, e.work, "symbolic-ref", "--short", "HEAD") != "main" {
		t.Errorf("the local tree changed: %q -> %q", before, after)
	}
	p, _ := os.ReadFile(prompt)
	if !strings.HasPrefix(string(p), "Continue work moved from a local Claude Code session (5e5e5e5e on tower).\nBranch: "+branch+".\n") {
		t.Errorf("prompt %q", p)
	}
	if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 1 {
		t.Errorf("a cloud move must keep the local transcript: %v", ts)
	}
	if _, ok, _ := e.sessionhub.st.ClaimMove(ctx, e.sessionhub.tower.id); ok {
		t.Error("a cloud move got a finish")
	}
}

func TestCloudMoveRefusesNonGitHub(t *testing.T) {
	e := newMoveEnv(t, "idle")
	// The sessionhub thinks the remote is on GitHub; the clone says otherwise.
	if _, err := e.sessionhub.st.UpsertSession(context.Background(), e.sessionhub.tower.id, api.SessionUpsert{ID: moveSID,
		Source: api.SourcePlugin, GitRepo: "https://github.com/o/app.git"}); err != nil {
		t.Fatal(err)
	}
	claim := e.startMove(t, api.MoveCloud)
	e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
	if mv := e.sessionhub.move(t, claim.Move.ID); mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "cloud: ") ||
		!strings.Contains(mv.Detail, "not on GitHub") {
		t.Errorf("move %+v", mv)
	}
	if prompts, _ := e.pane.snapshot(); len(prompts) != 0 {
		t.Errorf("ended the session anyway: %q", prompts)
	}
}

func TestCloudMoveFailureRestarts(t *testing.T) {
	e := newMoveEnv(t, "idle")
	fakeClaude(t, cloudClaude)
	t.Setenv("SESSIONHUB_TEST_PROMPT", filepath.Join(t.TempDir(), "prompt"))
	t.Setenv("SESSIONHUB_TEST_CLOUD_FAIL", "cloud sessions are not enabled for this organization")
	e.onGitHub(t)
	claim := e.startMove(t, api.MoveCloud)
	e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "start cloud: ") ||
		!strings.Contains(mv.Detail, "not enabled") || !strings.HasSuffix(mv.Detail, detailResumes) {
		t.Errorf("move %+v", mv)
	}
	if _, starts := e.pane.snapshot(); len(starts) != 1 {
		t.Errorf("starts %v", starts)
	}
}
```

- [ ] **Step 4: Run them to verify they fail**

Run: `go test ./internal/plugin/ -run Cloud -count=1`
Expected: FAIL to compile with `m.cloudWait undefined`; after Step 5's field
alone, `TestCloudMove` fails with "cloud: this watcher cannot hand sessions
to the cloud yet".

- [ ] **Step 5: Hand a cloud move off**

In `internal/plugin/move.go`, replace:

```go
	resultPause         time.Duration // also the pause between archive tries
	archiveTries        int
}
```

with:

```go
	resultPause         time.Duration // also the pause between archive tries
	archiveTries        int
	cloudWait           time.Duration // how long claude --cloud may take to print its link
}
```

replace:

```go
		start: resume.ControlOptions{PollFor: controlPollFor}, resultTries: moveResultTries, resultPause: moveResultPause,
		archiveTries: moveArchiveTries}
```

with:

```go
		start: resume.ControlOptions{PollFor: controlPollFor}, resultTries: moveResultTries, resultPause: moveResultPause,
		archiveTries: moveArchiveTries, cloudWait: 2 * time.Minute}
```

replace:

```go
	if mv.Target == api.MoveCloud {
		if !move.IsGitHub(repo.Remote) {
			fail(stepf("cloud", "the remote %s is not on GitHub", repo.Remote))
			return
		}
		fail(stepf("cloud", "this watcher cannot hand sessions to the cloud yet"))
		return
	}
```

with:

```go
	if mv.Target == api.MoveCloud {
		if !move.IsGitHub(repo.Remote) {
			fail(stepf("cloud", "the remote %s is not on GitHub", repo.Remote))
			return
		}
		m.toCloud(ctx, cl, claim, repo)
		return
	}
```

and add after the `moveOut` function:

```go
// toCloud is the cloud part of move-out. Everything that can fail without
// the cloud runs while the session still runs: the push, the Claude Code
// version, and the sessionhub-owned branch with the uncommitted work. Then it ends
// the session, starts a cloud session with the hand-off prompt, and reports
// done with its link and the files it left out.
func (m *mover) toCloud(ctx context.Context, cl *client.Client, claim api.ControlClaim, repo move.Repo) {
	s, mv := claim.Session, claim.Move
	fail := func(err error) { m.postResult(ctx, cl, mv.ID, failed(err)) }
	if err := move.PushBranch(ctx, repo); err != nil {
		fail(&stepError{"push", err})
		return
	}
	if _, err := move.ClaudeVersion(ctx, m.claudeBin); err != nil {
		fail(&stepError{"start cloud", err})
		return
	}
	branch, skipped, err := move.PushCloudBranch(ctx, repo, s.ID)
	if err != nil {
		fail(&stepError{"cloud branch", err})
		return
	}
	if !m.endIdle(ctx, cl, s, mv.ID) {
		return
	}
	link, err := move.StartCloud(ctx, m.claudeBin, s.CWD, move.HandOffPrompt(s, mv.Source, branch), m.cloudWait)
	if err != nil {
		m.abort(ctx, cl, s, mv.ID, &stepError{"start cloud", err})
		return
	}
	in := api.MoveResultIn{State: api.MoveDone, CloudURL: link}
	if len(skipped) > 0 {
		in.Detail = "not carried: " + strings.Join(skipped, ", ")
	}
	m.log.Printf("move: %s: session %s handed to the cloud on branch %s: %s", mv.ID, s.ID, branch, link)
	m.postResult(ctx, cl, mv.ID, in)
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/plugin/ ./internal/move/ -count=1 -race`
Expected: PASS.

- [ ] **Step 7: Document the cloud hand-off**

In `docs/plugin.md`, section "Moves", before the paragraph that starts
"Log lines start with `move:`", add:

```markdown
**Cloud (`move-out` to `cloud`).** Steps 1 and 2 of the source run as for
a machine move, and the remote must be on `github.com` (`cloud`) before
anything changes. Then, while the session still runs:

- `push`, as for a machine move, and `start cloud` checks `claude
  --version`.
- `cloud branch`: when there are uncommitted changes or untracked files,
  the watcher builds one commit on top of HEAD in a temporary index
  (`GIT_INDEX_FILE`, `read-tree`, `add`, `write-tree`, `commit-tree`), so the
  working tree, the index, and your branch don't change, and pushes it to
  `sessionhub/cloud-<first 8 of the session ID>`, a branch only sessionhub writes, with
  `--force-with-lease` against the commit sessionhub pushed there before. If
  anything else is on that branch, such as a cloud session's commits, the
  move fails instead of dropping them. Secret-looking files, tracked or
  untracked, and files over 10 MiB stay out; the `done` detail lists the
  files left out. With no changes it uses your branch.

Then `check pane` again and `end session`, as for a machine move, and:

- `start cloud`: `claude --cloud "<hand-off prompt>"` in the session's
  directory. The watcher takes the first `https://claude.ai/code/session_...`
  link the command prints, stops it, and reports `done` with the link; it
  gives up after 2 minutes. The sessionhub ends the local session with reason
  `moved_to_cloud`. The local transcript stays, so `claude --resume` still
  works here; the cloud session is a new one.

The hand-off prompt:

    Continue work moved from a local Claude Code session (<id8> on <machine>).
    Branch: <branch>.
    Recap: <recap, or none>
    Last request: <last prompt, or none>
    Status: done: ... / in flight: ... / waiting on: ...

The `Status` line is left out when the session has no report. A cloud
session needs a claude.ai sign-in, GitHub access through the Claude GitHub
App or `/web-setup`, and an organization that allows cloud sessions; when
`claude --cloud` fails, its last line is the detail and, once the sessionhub
records the failure, the session restarts here. Bring a cloud session back
with `claude --teleport <id>`.
```

In `docs/client.md`, at the end of section "Moves", add:

```markdown
- **Cloud.** `PushCloudBranch` commits the working tree's changes on top of
  HEAD through a temporary `GIT_INDEX_FILE`, leaving secret-looking files
  out with the same pathspec excludes as the bundle, and pushes
  `CloudBranch(id)` (`sessionhub/cloud-<id8>`) with `--force-with-lease` against
  the commit sessionhub pushed there before (read with `git ls-remote`), so a
  cloud session's commits on that branch are never dropped. It runs the
  user's pre-push hook like any push, and never touches the working tree,
  the real index, or the current branch. `HandOffPrompt`
  writes the cloud session's first prompt. `StartCloud` runs
  `claude --cloud <prompt>`, returns the first session link in its output,
  stops the command then, and fails after the wait with the output's last
  line.
```

- [ ] **Step 8: Lint and commit**

Run: `gofmt -w internal/move/*.go internal/plugin/*.go && make lint`
Expected: clean.

```bash
git add internal/move/cloud.go internal/move/cloud_test.go internal/plugin/move.go internal/plugin/move_helpers_test.go \
  internal/plugin/move_test.go docs/plugin.md docs/client.md
git commit -m "$(cat <<'EOF'
Hand a session off to a Claude Code cloud session

For a GitHub remote, the source records uncommitted work as one commit
on sessionhub/cloud-<id8>, built in a temporary index so the working tree and
the user's branch never change, without secret-looking files, and pushed
with a lease so a cloud session's commits are never dropped. Then it
ends the session, runs claude --cloud with a hand-off prompt, and
reports the session link. A failure the sessionhub recorded restarts the
session here.

Refs: docs/dev/superpowers/plans/2026-10-03-move-session.md, Task 9

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 10: CLI: sessionhub move and sessionhub move-key

**Files:**
- Create: `internal/cli/move.go` (`RunMove`, `RunMoveKey`, `moveAPI`,
  `(*env).move`, `followMove`, `moveLine`, `moveKey`)
- Create: `internal/cli/move_test.go`
- Modify: `internal/cli/cli.go` (`env.moves`, set in `defaultEnv`)
- Modify: `cmd/sessionhub/main.go` (routes and usage)
- Modify: `cmd/sessionhub/main_test.go` (`commands`, a dispatch case)
- Modify: `docs/cli.md` (Commands table, a "`sessionhub move`" section)

**Interfaces:**
- Consumes: `client.CreateMove`, `client.GetMove`, `client.GetSession`
  (Task 5); `move.LoadOrCreateKey`, `move.KeyPath`, `move.Fingerprint`
  (Task 1); `clean`, `errText`, `shortID`, `title`, `runWith`, `ExitError`,
  `env.pause`, `env.now`.
- Produces: `func RunMove(ctx context.Context, args []string) error`,
  `func RunMoveKey(ctx context.Context, args []string) error`, `moveAPI`.

- [ ] **Step 1: Write the failing CLI tests**

Create `internal/cli/move_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

const moveID = "mv_AAAAAAAAAAAAAAAAAAAAAA"

// fakeMoves is a moveAPI. states is what GetMove returns, one per read;
// the last one repeats.
type fakeMoves struct {
	detail    api.SessionDetail
	getErr    error
	createErr error
	created   []string // "id target"
	states    []api.Move
	reads     int
}

func (f *fakeMoves) GetSession(context.Context, string) (api.SessionDetail, error) {
	return f.detail, f.getErr
}

func (f *fakeMoves) CreateMove(_ context.Context, id, target string) (api.Move, error) {
	f.created = append(f.created, id+" "+target)
	if f.createErr != nil {
		return api.Move{}, f.createErr
	}
	return api.Move{ID: moveID, SessionID: id, Source: "bluebox", Target: target, State: api.MoveRequested}, nil
}

func (f *fakeMoves) GetMove(context.Context, string) (api.Move, error) {
	i := min(f.reads, len(f.states)-1)
	f.reads++
	return f.states[i], nil
}

func runMove(t *testing.T, f *fakeMoves, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	clock := now
	e := &env{moves: f, out: &out, now: func() time.Time { return clock }, pause: func(d time.Duration) { clock = clock.Add(d) }}
	err := e.move(context.Background(), args)
	return out.String(), err
}

func state(s string, mod func(*api.Move)) api.Move {
	m := api.Move{ID: moveID, Source: "bluebox", Target: "tower", State: s, BundleSize: 3 << 20}
	if mod != nil {
		mod(&m)
	}
	return m
}

func session() api.SessionDetail {
	return api.SessionDetail{Session: api.Session{ID: "3f2a9c10-1111-4a4a-8b8b-000000000001", Title: "auth work", Machine: "bluebox"}}
}

func TestMoveFollowsToDone(t *testing.T) {
	f := &fakeMoves{detail: session(), states: []api.Move{state(api.MovePacking, nil), state(api.MovePacking, nil),
		state(api.MoveUploaded, nil), state(api.MoveUnpacking, nil),
		state(api.MoveDone, func(m *api.Move) { m.Detail = "not carried: .env" })}}
	out, err := runMove(t, f, "3f2a", "tower")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 || f.created[0] != "3f2a9c10-1111-4a4a-8b8b-000000000001 tower" {
		t.Errorf("created %v", f.created)
	}
	want := moveID + "  moving 3f2a9c10 (auth work) from bluebox to tower\n" +
		"requested: waiting for the sessionhub watcher on bluebox\n" +
		"packing on bluebox: ending the session and sealing the bundle\n" +
		"uploaded (3.0 MiB): waiting for tower\n" +
		"unpacking on tower\n" +
		"done: the session runs on tower now; not carried: .env\n"
	if out != want {
		t.Errorf("output:\n%s\nwant:\n%s", out, want)
	}
}

func TestMoveFailedExitsOne(t *testing.T) {
	f := &fakeMoves{detail: session(), states: []api.Move{state(api.MoveFailed, func(m *api.Move) {
		m.Detail = "find clone: no clone of github.com/o/r on tower\x1b[31m"
	})}}
	out, err := runMove(t, f, "3f2a", "tower")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Fatalf("err %v", err)
	}
	if !strings.HasSuffix(out, "failed: find clone: no clone of github.com/o/r on tower[31m\n") || strings.Contains(out, "\x1b") {
		t.Errorf("output %q", out)
	}
}

func TestMoveCloudDone(t *testing.T) {
	f := &fakeMoves{detail: session(), states: []api.Move{state(api.MoveDone, func(m *api.Move) {
		m.Target, m.CloudURL = api.MoveCloud, "https://claude.ai/code/session_01AbC"
	})}}
	out, err := runMove(t, f, "3f2a", "cloud")
	if err != nil || !strings.Contains(out, "done: the session continues in the cloud at https://claude.ai/code/session_01AbC\n") {
		t.Errorf("output %q %v", out, err)
	}
}

func TestMoveGivesUpFollowing(t *testing.T) {
	f := &fakeMoves{detail: session(), states: []api.Move{state(api.MovePacking, nil)}}
	out, err := runMove(t, f, "3f2a", "tower")
	if err != nil || !strings.HasSuffix(out, "still packing after 3 minutes; check with: sessionhub move --status "+moveID+"\n") {
		t.Errorf("output %q %v", out, err)
	}
	if strings.Count(out, "packing on bluebox") != 1 {
		t.Errorf("a repeated state printed twice:\n%s", out)
	}
}

func TestMoveRefusedAndUsage(t *testing.T) {
	f := &fakeMoves{detail: session(), createErr: &client.StatusError{Status: 409,
		Message: "not controllable: the agent is working; a session moves only when it is idle or done"}}
	if _, err := runMove(t, f, "3f2a", "tower"); err == nil || !strings.Contains(err.Error(), "only when it is idle or done") {
		t.Errorf("refusal: %v", err)
	}
	f = &fakeMoves{getErr: &client.StatusError{Status: 404, Message: `not found: no session matches "zzzz"`}}
	if _, err := runMove(t, f, "zzzz", "tower"); err == nil || !strings.Contains(err.Error(), "no session matches") {
		t.Errorf("unknown session: %v", err)
	}
	for _, args := range [][]string{nil, {"3f2a"}, {"3f2a", "tower", "x"}, {"--status"}, {"--bogus", "x"}} {
		if _, err := runMove(t, &fakeMoves{}, args...); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("%q: %v", args, err)
		}
	}
}

func TestMoveStatus(t *testing.T) {
	f := &fakeMoves{states: []api.Move{state(api.MoveUnpacking, nil)}}
	out, err := runMove(t, f, "--status", moveID)
	if err != nil || out != moveID+"  unpacking on tower\n" {
		t.Errorf("status %q %v", out, err)
	}
	if len(f.created) != 0 {
		t.Error("--status started a move")
	}
}

func TestMoveKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "move.key")
	var out bytes.Buffer
	if err := moveKey(&out, path, nil); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}\n$`).MatchString(out.String()) {
		t.Errorf("output %q", out.String())
	}
	first := out.String()
	out.Reset()
	moveKey(&out, path, nil)
	if out.String() != first {
		t.Errorf("the fingerprint changed: %q then %q", first, out.String())
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", fi.Mode().Perm())
	}
	if err := moveKey(&out, path, []string{"x"}); err == nil {
		t.Error("an argument was accepted")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/cli/ -count=1`
Expected: FAIL to compile with `unknown field moves in struct literal of type
env`, `e.move undefined`, and `undefined: moveKey`.

- [ ] **Step 3: Write the commands**

Create `internal/cli/move.go`:

```go
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/move"
)

// moveAPI is the part of *client.Client that sessionhub move uses.
type moveAPI interface {
	GetSession(ctx context.Context, idOrPrefix string) (api.SessionDetail, error)
	CreateMove(ctx context.Context, sessionID, target string) (api.Move, error)
	GetMove(ctx context.Context, id string) (api.Move, error)
}

var _ moveAPI = (*client.Client)(nil)

const (
	// moveFollow is how long sessionhub move prints a move's progress.
	moveFollow = 3 * time.Minute
	// movePoll is how often it reads the move.
	movePoll = 2 * time.Second
)

const moveUsage = `usage:
  sessionhub move <id-or-prefix> <machine|cloud>
  sessionhub move --status <move-id>`

// RunMove implements `sessionhub move`.
func RunMove(ctx context.Context, args []string) error { return runWith(ctx, args, (*env).move) }

// RunMoveKey implements `sessionhub move-key`: it prints this machine's move key
// fingerprint, creating the key when there is none.
func RunMoveKey(_ context.Context, args []string) error {
	return moveKey(os.Stdout, move.KeyPath(), args)
}

func moveKey(out io.Writer, path string, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: sessionhub move-key")
	}
	k, err := move.LoadOrCreateKey(path)
	if err != nil {
		return fmt.Errorf("move-key: %w", err)
	}
	_, err = fmt.Fprintln(out, move.Fingerprint(k.PublicKey()))
	return err
}

func (e *env) move(ctx context.Context, args []string) error {
	switch {
	case len(args) == 2 && args[0] == "--status":
		mv, err := e.moves.GetMove(ctx, args[1])
		if err != nil {
			return fmt.Errorf("move: %s", errText(err))
		}
		fmt.Fprintln(e.out, clean(mv.ID, 0)+"  "+moveLine(mv))
		return nil
	case len(args) == 2 && !strings.HasPrefix(args[0], "-"):
		d, err := e.moves.GetSession(ctx, args[0])
		if err != nil {
			return fmt.Errorf("move: %s: %s", clean(args[0], 0), errText(err))
		}
		mv, err := e.moves.CreateMove(ctx, d.ID, args[1])
		if err != nil {
			return fmt.Errorf("move: %s", errText(err))
		}
		fmt.Fprintf(e.out, "%s  moving %s (%s) from %s to %s\n", clean(mv.ID, 0), shortID(d.ID), clean(title(d.Session), 0),
			clean(mv.Source, 0), clean(mv.Target, 0))
		return e.followMove(ctx, mv)
	}
	return fmt.Errorf("move: unexpected arguments\n%s", moveUsage)
}

// followMove prints each new state of the move until it ends or moveFollow
// passes. A move that did not end done exits 1.
func (e *env) followMove(ctx context.Context, mv api.Move) error {
	last := ""
	deadline := e.now().Add(moveFollow)
	for {
		if mv.State != last {
			fmt.Fprintln(e.out, moveLine(mv))
			last = mv.State
		}
		if !api.MoveOpen(mv.State) {
			break
		}
		if !e.now().Before(deadline) || ctx.Err() != nil {
			fmt.Fprintf(e.out, "still %s after 3 minutes; check with: sessionhub move --status %s\n", clean(mv.State, 0), clean(mv.ID, 0))
			return nil
		}
		e.pause(movePoll)
		if next, err := e.moves.GetMove(ctx, mv.ID); err == nil {
			mv = next
		}
	}
	if mv.State != api.MoveDone {
		return &ExitError{Code: 1}
	}
	return nil
}

// moveLine says where a move is, cleaned for the terminal.
func moveLine(mv api.Move) string {
	src, dst, detail := clean(mv.Source, 0), clean(mv.Target, 0), clean(mv.Detail, 0)
	switch mv.State {
	case api.MoveRequested:
		return "requested: waiting for the sessionhub watcher on " + src
	case api.MovePacking:
		if mv.Target == api.MoveCloud {
			return "packing on " + src + ": ending the session and starting the cloud session"
		}
		return "packing on " + src + ": ending the session and sealing the bundle"
	case api.MoveUploaded:
		return fmt.Sprintf("uploaded (%.1f MiB): waiting for %s", float64(mv.BundleSize)/(1<<20), dst)
	case api.MoveUnpacking:
		return "unpacking on " + dst
	case api.MoveDone:
		line := "done: the session runs on " + dst + " now"
		if mv.Target == api.MoveCloud {
			line = "done: the session continues in the cloud at " + clean(mv.CloudURL, 0)
		}
		if detail != "" {
			line += "; " + detail
		}
		return line
	}
	if detail == "" {
		detail = "no reason given"
	}
	return clean(mv.State, 0) + ": " + detail
}
```

In `internal/cli/cli.go`, replace:

```go
	inbox   inboxAPI
	actions actionsAPI
```

with:

```go
	inbox   inboxAPI
	actions actionsAPI
	moves   moveAPI
```

and replace:

```go
	return &env{cfg: cfg, api: c, login: c, inbox: c, actions: c, pause: time.Sleep, width: termWidth, out: os.Stdout, now: time.Now,
```

with:

```go
	return &env{cfg: cfg, api: c, login: c, inbox: c, actions: c, moves: c, pause: time.Sleep, width: termWidth, out: os.Stdout, now: time.Now,
```

In `cmd/sessionhub/main.go`, replace:

```go
  deny [--yes] <id> [reason]    deny a pending permission prompt
```

with:

```go
  deny [--yes] <id> [reason]    deny a pending permission prompt
  move <id> <machine|cloud>     move a session to another machine or the cloud
  move --status <move-id>       show a move
  move-key                      print this machine's move key fingerprint
```

and replace:

```go
	"deny":               cli.RunDeny,
```

with:

```go
	"deny":               cli.RunDeny,
	"move":               cli.RunMove,
	"move-key":           cli.RunMoveKey,
```

In `cmd/sessionhub/main_test.go`, replace:

```go
	"approve", "deny", "resume", "remote-control", "join"}
```

with:

```go
	"approve", "deny", "move", "move-key", "resume", "remote-control", "join"}
```

and replace:

```go
		{[]string{"join"}, "join", []string{}},
```

with:

```go
		{[]string{"join"}, "join", []string{}},
		{[]string{"move", "abcd", "tower"}, "move", []string{"abcd", "tower"}},
		{[]string{"move-key"}, "move-key", []string{}},
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli/ ./cmd/sessionhub/ -count=1`
Expected: PASS.

- [ ] **Step 5: Document the commands**

In `docs/cli.md`, in the Commands table, after the `sessionhub deny` row, add:

```markdown
| `sessionhub move <id-or-prefix> <machine\|cloud>` | Move a session to another machine, or hand it to a Claude Code cloud session, and print its progress for up to 3 minutes. See below. |
| `sessionhub move --status <move-id>` | Print where a move is. |
| `sessionhub move-key` | Print this machine's move key fingerprint (16 hex characters), creating the key if there is none. |
```

After the "## `sessionhub approve` and `sessionhub deny`" section, add:

```markdown
## `sessionhub move`

`sessionhub move <id-or-prefix> tower` moves a session to the machine `tower`, with
its conversation, its branch, and its uncommitted changes, and resumes it
there in a new herdr workspace. `sessionhub move <id-or-prefix> cloud` hands it to
a new Claude Code cloud session instead (GitHub remotes only). You can run it
on any machine.

The session must be in herdr on a machine whose watcher runs, and its agent
must be idle or done. The target machine needs a running watcher and a clone
of the same repository under `~/Code` (or a `move_roots` entry in
`~/.config/sessionhub/config.toml`) with no uncommitted changes, and the same Claude
Code version. When a rule fails, the server says which, and nothing moves.

It prints one line per state:

    mv_...  moving 3f2a9c10 (auth work) from bluebox to tower
    requested: waiting for the sessionhub watcher on bluebox
    packing on bluebox: ending the session and sealing the bundle
    uploaded (3.0 MiB): waiting for tower
    unpacking on tower
    done: the session runs on tower now; not carried: .env

A failure prints `failed: <step>: <reason>` and exits 1; the session then
runs again on the source machine. After 3 minutes the command stops
following and prints the `sessionhub move --status` command to check later. Files
listed as "not carried" (`.env*`, `*.pem`, `*.key`, `*.tfvars`) stay on the
source; copy them by hand.

`sessionhub move-key` on each machine and `sessionhub machine ls` on the server show the
same fingerprint for a machine. If they differ, someone swapped a key on the
server: don't move sessions until you find out why.
```

- [ ] **Step 6: Lint and commit**

Run: `gofmt -w internal/cli/*.go cmd/sessionhub/*.go && make lint`
Expected: clean.

```bash
git add internal/cli/move.go internal/cli/move_test.go internal/cli/cli.go cmd/sessionhub/main.go cmd/sessionhub/main_test.go docs/cli.md
git commit -m "$(cat <<'EOF'
Add sessionhub move and sessionhub move-key

sessionhub move starts a move and prints each state for up to 3 minutes,
exiting 1 when it fails; --status reads one move. sessionhub move-key prints
this machine's key fingerprint to compare with sessionhub machine ls.

Refs: docs/dev/superpowers/plans/2026-10-03-move-session.md, Task 10

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 11: Dashboard: Move menu

**Files:**
- Modify: `internal/server/dashboard/index.html` (a `move-state` block of
  pure functions; `moveRow`, `loadMachines`, `startMove`, `pollMove`; the
  card calls `moveRow`; CSS)
- Modify: `internal/server/dashboard_test.go` (`TestDashboardWrites` counts,
  `TestDashboardMoveStates`)
- Modify: `docs/server.md` (section "Dashboard")

**Interfaces:**
- Consumes: `POST /v1/sessions/{id}/move`, `GET /v1/moves/{id}`,
  `GET /v1/machines` (Task 4); `Session.move`, `Machine.move_key`,
  `Machine.move_ready`; page helpers `el`, `keyed`, `refreshCard`,
  `rcRefusal`, `showSignedOut`, `sessionsById`, `runBlocks`.
- Produces: page functions `isGitHub`, `moveOpen`, `moveBlocked`,
  `moveTargets`, `moveConfirm`, `moveRequest`, `moveView` (block
  `move-state`), and `moveRow`, `loadMachines`, `startMove`, `pollMove`.

- [ ] **Step 1: Write the failing dashboard tests**

In `internal/server/dashboard_test.go` (`TestDashboardWrites`), replace:

```go
		{"method:", 7},
		{`method: "POST"`, 6},
```

with:

```go
		{"method:", 8},
		{`method: "POST"`, 7},
		{`"X-Hub-Action": "move"`, 1},
		{"// move-state:begin", 1},
		{"// move-state:end", 1},
		{"= moveRow(s)", 1},
```

and replace:

```go
		{"fetch(", 11}, // the list, the inbox, the request, the session poll, the Details read, sign-out, triage,
		// the rules read, a rules write, the send, and a decision
```

with:

```go
		{"fetch(", 14}, // the list, the inbox, the request, the session poll, the Details read, sign-out, triage,
		// the rules read, a rules write, the send, a decision, the machines read, a move, and the move poll
```

Add to the same file:

```go
// TestDashboardMoveStates runs the page's move-state block under node: who
// may move, the targets, the confirmation, the request, and the card line.
func TestDashboardMoveStates(t *testing.T) {
	script := `
var idle = { id: "3f2a9c10-1111", title: "auth work", machine: "bluebox", status: "live", controllable: true,
  agent_state: "idle", git_branch: "main", git_repo: "git@github.com:o/r.git" };
var machines = [{ name: "bluebox", move_ready: true, move_key: "aaaa" }, { name: "tower", move_ready: true, move_key: "bbbb" },
  { name: "nas", move_ready: false, move_key: "cccc" }, { name: "pi", move_ready: false }];
function withMove(state, extra) { return Object.assign({ id: "mv_1", source: "bluebox", target: "tower", state: state }, extra || {}); }
process.stdout.write(JSON.stringify({
  github: ["git@github.com:o/r.git", "https://GitHub.com/o/r", "ssh://git@github.com/o/r.git", "git@gitlab.com:o/r.git",
    "https://github.com.evil/o/r", "/srv/github.com/o/r", "", null].map(isGitHub),
  blocked: [moveBlocked(idle), moveBlocked(Object.assign({}, idle, { agent_state: "working" })),
    moveBlocked(Object.assign({}, idle, { agent_state: "done" })),
    moveBlocked(Object.assign({}, idle, { controllable: false })), moveBlocked(Object.assign({}, idle, { status: "ended" })),
    moveBlocked(Object.assign({}, idle, { move: withMove("packing") })), moveBlocked(Object.assign({}, idle, { move: withMove("failed") }))],
  targets: moveTargets(idle, machines),
  noCloud: moveTargets(Object.assign({}, idle, { git_repo: "git@gitlab.com:o/r.git" }), machines.slice(0, 2)),
  confirm: [moveConfirm(idle, { target: "tower", label: "tower" }), moveConfirm(idle, { target: "cloud", label: "Cloud" })],
  req: moveRequest("3f2a/x", "tower"),
  views: [moveView(null), moveView(withMove("requested")), moveView(withMove("packing")), moveView(withMove("uploaded")),
    moveView(withMove("unpacking")), moveView(withMove("done")), moveView(withMove("done", { detail: "not carried: .env" })),
    moveView(withMove("done", { target: "cloud", cloud_url: "https://claude.ai/code/session_01Ab" })),
    moveView(withMove("done", { target: "cloud", cloud_url: "javascript:alert(1)" })),
    moveView(withMove("failed", { detail: "find clone: no clone of github.com/o/r on tower" })),
    moveView(withMove("cancelled"))]
}));`
	out := runBlocks(t, []string{"move-state"}, script, map[string]any{})
	want := `{
  "github": [true, true, true, false, false, false, false, false],
  "blocked": ["", "Wait until the agent is idle or done.", "", "Not in herdr, or its machine's watcher is offline.",
    "The session has ended.", "A move is under way.", ""],
  "targets": [{"target": "tower", "label": "tower", "ok": true, "why": ""},
    {"target": "nas", "label": "nas", "ok": false, "why": "its watcher is offline"},
    {"target": "pi", "label": "pi", "ok": false, "why": "no move key yet"},
    {"target": "cloud", "label": "Cloud", "ok": true, "why": ""}],
  "noCloud": [{"target": "tower", "label": "tower", "ok": true, "why": ""}],
  "confirm": ["Move \"auth work\" to tower? Its conversation, branch main, and uncommitted changes move; the session ends here and resumes on tower.",
    "Hand \"auth work\" to a new Claude Code cloud session? Uncommitted work goes on a sessionhub branch on GitHub, and the session ends here."],
  "req": {"url": "/v1/sessions/3f2a%2Fx/move", "init": {"method": "POST",
    "headers": {"Content-Type": "application/json", "X-Hub-Action": "move"}, "body": "{\"target\":\"tower\"}", "cache": "no-store"}},
  "views": [null,
    {"kind": "note", "text": "Moving to tower: waiting for bluebox\u2026", "url": ""},
    {"kind": "note", "text": "Moving to tower: packing on bluebox\u2026", "url": ""},
    {"kind": "note", "text": "Moving to tower: waiting for tower\u2026", "url": ""},
    {"kind": "note", "text": "Moving to tower: unpacking on tower\u2026", "url": ""},
    {"kind": "note", "text": "Moved to tower.", "url": ""},
    {"kind": "note", "text": "Moved to tower. not carried: .env", "url": ""},
    {"kind": "link", "text": "Moved to the cloud.", "url": "https://claude.ai/code/session_01Ab"},
    {"kind": "note", "text": "Moved to the cloud.", "url": ""},
    {"kind": "error", "text": "Move to tower failed: find clone: no clone of github.com/o/r on tower", "url": ""},
    {"kind": "error", "text": "Move to tower failed: cancelled", "url": ""}]
}`
	var got, exp map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %s: %v", out, err)
	}
	if err := json.Unmarshal([]byte(want), &exp); err != nil {
		t.Fatal(err)
	}
	for k, w := range exp {
		if !reflect.DeepEqual(got[k], w) {
			gb, _ := json.Marshal(got[k])
			wb, _ := json.Marshal(w)
			t.Errorf("%s:\n got %s\nwant %s", k, gb, wb)
		}
	}
	page := string(dashboardHTML)
	for _, want := range []string{"if (mr) c.appendChild(mr);", "if (!window.confirm(moveConfirm(s, t))) return;",
		`fetch("/v1/moves/" + encodeURIComponent(id)`, `fetch("/v1/machines"`, "if (menu.open) loadMachines(s.id);",
		"var why = moveBlocked(s);", "b.disabled = !t.ok || busy;", ".move { display: flex;"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/server/ -run 'TestDashboard' -count=1`
Expected: FAIL: `TestDashboardWrites` reports the new counts, and
`TestDashboardMoveStates` stops with "dashboard/index.html has no
move-state block". (Without `node` on `PATH`, the block test skips; the
string checks in `TestDashboardWrites` still fail.)

- [ ] **Step 3: Add the pure functions**

In `internal/server/dashboard/index.html`, replace:

```js
  // actions-state:end
```

with:

```js
  // actions-state:end

  // move-state:begin
  var MOVE_POLL_MS = 2000;
  var MOVE_FOLLOW_MS = 180000;
  var MOVE_LINK = /^https:\/\/claude\.ai\/code\/session_[A-Za-z0-9_-]+$/;

  // isGitHub reports whether a remote URL is on github.com: the host of a
  // URL with a scheme, or of git@host:path, ignoring the user and case. (The
  // page must not hold a literal scheme-and-slashes string: the CSP test
  // looks for one.)
  function isGitHub(u) {
    var s = String(u || "").trim();
    var i = s.indexOf("://");
    if (i >= 0) {
      s = s.slice(i + 3);
      var at = s.indexOf("@");
      var slash = s.indexOf("/");
      if (at >= 0 && (slash < 0 || at < slash)) s = s.slice(at + 1);
    } else {
      var c = s.indexOf(":");
      if (c <= 0 || s.slice(0, c).indexOf("/") !== -1) return false;
      s = s.slice(s.lastIndexOf("@", c) + 1, c) + "/" + s.slice(c + 1);
    }
    return s.toLowerCase().indexOf("github.com/") === 0;
  }

  // moveOpen reports whether a move is under way.
  function moveOpen(m) {
    return !!m && ["requested", "packing", "uploaded", "unpacking"].indexOf(m.state) !== -1;
  }

  // moveBlocked is why the session cannot move now, or "". The server
  // checks again and has the last word.
  function moveBlocked(s) {
    if (!s || s.status === "ended") return "The session has ended.";
    if (s.controllable !== true) return "Not in herdr, or its machine's watcher is offline.";
    if (s.agent_state !== "idle" && s.agent_state !== "done") return "Wait until the agent is idle or done.";
    if (moveOpen(s.move)) return "A move is under way.";
    return "";
  }

  // moveTargets is where the session can go: every other machine, ready or
  // not with the reason, then Cloud for a GitHub remote.
  function moveTargets(s, machines) {
    var out = [];
    (machines || []).forEach(function (m) {
      if (!m || typeof m.name !== "string" || m.name === s.machine) return;
      var ok = m.move_ready === true;
      out.push({ target: m.name, label: m.name, ok: ok, why: ok ? "" : (m.move_key ? "its watcher is offline" : "no move key yet") });
    });
    if (isGitHub(s.git_repo)) out.push({ target: "cloud", label: "Cloud", ok: true, why: "" });
    return out;
  }

  // moveConfirm is the question the page asks before a move.
  function moveConfirm(s, t) {
    var name = s.title || String(s.id).slice(0, 8);
    if (t.target === "cloud") {
      return "Hand \"" + name + "\" to a new Claude Code cloud session? Uncommitted work goes on a sessionhub branch on GitHub, and the session ends here.";
    }
    return "Move \"" + name + "\" to " + t.label + "? Its conversation, branch" + (s.git_branch ? " " + s.git_branch : "") +
      ", and uncommitted changes move; the session ends here and resumes on " + t.label + ".";
  }

  // moveRequest is the Move write.
  function moveRequest(id, target) {
    return {
      url: "/v1/sessions/" + encodeURIComponent(id) + "/move",
      init: {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Hub-Action": "move" },
        body: JSON.stringify({ target: target }),
        cache: "no-store"
      }
    };
  }

  // moveView is the card's line for its latest move, or null: a note while
  // it runs, the cloud link when a cloud move is done, an error when it
  // failed.
  function moveView(m) {
    if (!m || typeof m.state !== "string") return null;
    var to = m.target === "cloud" ? "the cloud" : m.target;
    switch (m.state) {
      case "requested": return { kind: "note", text: "Moving to " + to + ": waiting for " + m.source + "\u2026", url: "" };
      case "packing": return { kind: "note", text: "Moving to " + to + ": packing on " + m.source + "\u2026", url: "" };
      case "uploaded": return { kind: "note", text: "Moving to " + to + ": waiting for " + m.target + "\u2026", url: "" };
      case "unpacking": return { kind: "note", text: "Moving to " + to + ": unpacking on " + m.target + "\u2026", url: "" };
      case "done":
        if (m.target === "cloud" && MOVE_LINK.test(m.cloud_url || "")) return { kind: "link", text: "Moved to the cloud.", url: m.cloud_url };
        return { kind: "note", text: "Moved to " + to + "." + (m.detail ? " " + m.detail : ""), url: "" };
    }
    return { kind: "error", text: "Move to " + to + " failed: " + (m.detail || m.state), url: "" };
  }
  // move-state:end
```

- [ ] **Step 4: Add the menu**

In the same file, replace:

```js
  var selected = {};    // session id -> true while selected for a message
```

with:

```js
  var selected = {};    // session id -> true while selected for a message
  var moveState = {};   // session id -> {phase: "sending"} while a move request is out, or {phase: "error", error}
  var openMove = {};    // session id -> true while its Move menu is open
  var machinesList = null; // the last GET /v1/machines, read when a Move menu opens
```

replace:

```js
    var rc = rcRow(s, now);
    if (rc) c.appendChild(rc);
    return art;
  }
```

with:

```js
    var rc = rcRow(s, now);
    if (rc) c.appendChild(rc);
    var mr = moveRow(s);
    if (mr) c.appendChild(mr);
    return art;
  }

  // moveRow is the card's Move menu and its latest move's line. A session
  // that cannot move gets a disabled button and the reason; an ended one
  // without a move gets nothing.
  function moveRow(s) {
    var st = moveState[s.id];
    var line = st && st.phase === "sending" ? { kind: "note", text: "Sending\u2026", url: "" } :
      st && st.phase === "error" ? { kind: "error", text: st.error, url: "" } : moveView(s.move);
    if (s.status === "ended" && !line) return null;
    var box = el("div", "move");
    var why = moveBlocked(s);
    var busy = !!st && st.phase === "sending";
    if (s.status === "ended") {
      // Nothing to offer; only the line below.
    } else if (why) {
      var off = el("button", null, "Move to\u2026");
      off.type = "button";
      off.disabled = true;
      box.appendChild(off);
      box.appendChild(el("span", "note", why));
    } else {
      var menu = keyed(el("details", "move-menu"), "move:" + s.id);
      menu.open = !!openMove[s.id];
      menu.appendChild(el("summary", null, "Move to\u2026"));
      menu.addEventListener("toggle", function () {
        openMove[s.id] = menu.open;
        if (menu.open) loadMachines(s.id);
      });
      if (machinesList === null) menu.appendChild(el("p", "note", "Reading the machines\u2026"));
      var targets = moveTargets(s, machinesList || []);
      if (machinesList !== null && !targets.length) menu.appendChild(el("p", "note", "No other machine, and no GitHub remote for the cloud."));
      targets.forEach(function (t) {
        var b = el("button", "textbtn", t.label);
        b.type = "button";
        b.disabled = !t.ok || busy;
        b.addEventListener("click", function () { startMove(s, t); });
        menu.appendChild(b);
        if (t.why) menu.appendChild(el("span", "note", t.label + ": " + t.why));
      });
      box.appendChild(menu);
    }
    if (line && line.kind === "link") {
      box.appendChild(el("span", "note", line.text));
      var a = el("a", "open", "Open in Claude");
      a.href = line.url;
      a.target = "_blank";
      a.rel = "noopener noreferrer";
      box.appendChild(a);
    } else if (line) {
      box.appendChild(el("span", line.kind === "error" ? "rc-error" : "note", line.text));
    }
    return box;
  }

  // loadMachines reads the machines for an open Move menu.
  function loadMachines(id) {
    fetch("/v1/machines", { headers: { "Accept": "application/json" }, cache: "no-store" })
      .then(function (resp) {
        if (resp.status === 401) { showSignedOut(); return null; }
        if (!resp.ok) throw new Error("status " + resp.status);
        return resp.json();
      })
      .then(function (list) {
        if (list === null) return;
        machinesList = Array.isArray(list) ? list : [];
        refreshCard(id);
      })
      .catch(function () {
        moveState[id] = { phase: "error", error: "Could not read the machines." };
        refreshCard(id);
      });
  }

  // startMove asks, then sends the move and follows it.
  function startMove(s, t) {
    if (!window.confirm(moveConfirm(s, t))) return;
    moveState[s.id] = { phase: "sending" };
    openMove[s.id] = false;
    refreshCard(s.id);
    var req = moveRequest(s.id, t.target);
    fetch(req.url, req.init)
      .then(function (resp) {
        return resp.json().then(
          function (body) { return { resp: resp, body: body }; },
          function () { return { resp: resp, body: null }; });
      })
      .then(function (r) {
        if (r.resp.status === 401) { showSignedOut(); return; }
        if (r.resp.status !== 202 || !r.body || typeof r.body.id !== "string") {
          moveState[s.id] = { phase: "error", error: rcRefusal(r.resp.status, r.body) };
          refreshCard(s.id);
          return;
        }
        delete moveState[s.id];
        var cur = sessionsById[s.id];
        if (cur) cur.move = r.body;
        refreshCard(s.id);
        var until = Date.now() + MOVE_FOLLOW_MS;
        setTimeout(function () { pollMove(s.id, r.body.id, until); }, MOVE_POLL_MS);
      })
      .catch(function () {
        moveState[s.id] = { phase: "error", error: "Could not reach the sessionhub." };
        refreshCard(s.id);
      });
  }

  // pollMove reads the move every 2 seconds while it runs, for up to
  // 3 minutes; the 30-second list refresh shows it after that.
  function pollMove(sid, id, until) {
    if (signedOut || Date.now() > until) return;
    function again() { setTimeout(function () { pollMove(sid, id, until); }, MOVE_POLL_MS); }
    fetch("/v1/moves/" + encodeURIComponent(id), { headers: { "Accept": "application/json" }, cache: "no-store" })
      .then(function (resp) {
        if (!resp.ok) throw new Error("status " + resp.status);
        return resp.json();
      })
      .then(function (m) {
        var cur = sessionsById[sid];
        if (cur && (!cur.move || cur.move.id === id)) cur.move = m;
        refreshCard(sid);
        if (moveOpen(m)) again();
      })
      .catch(again);
  }
```

and replace:

```css
.rc-link { width: 100%; }
```

with:

```css
.rc-link { width: 100%; }
.move { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin-top: 10px; }
.move .note { color: var(--muted); font-size: .85rem; }
.move .rc-error { color: var(--blocked); font-size: .85rem; }
.move-menu > summary { cursor: pointer; color: var(--accent); min-height: 44px; display: flex; align-items: center; }
.move-menu[open] { width: 100%; display: flex; flex-wrap: wrap; gap: 8px; align-items: center; }
.move-menu[open] > summary { width: 100%; }
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestDashboard' -count=1`
Expected: PASS, including `TestDashboardPermissionConfirm`, which checks
the page holds no raw bidirectional control or U+FFFD (the new strings use
`\u2026` escapes).

Then load the page in a browser against a local `sessionhub server` with a
temporary database (`SESSIONHUB_DB=$(mktemp -d)/sessionhub.db sessionhub server`, a machine
added with `sessionhub machine add`, and a session registered with `curl`), sign
in with `sessionhub login`, expand a card, and check: an idle session in herdr
shows **Move to…**, a working one shows the disabled button and "Wait until
the agent is idle or done.", and opening the menu lists the other machines.
Stop the server and delete the temporary directory afterwards.

- [ ] **Step 6: Document the menu**

In `docs/server.md`, section "Dashboard", after the paragraph that starts
"A session in herdr whose machine's watcher polled in the last 2 minutes",
add:

```markdown
An expanded card also gets **Move to…**. When the session can't move now
(it ended, it is not in herdr or its watcher is offline, its agent is not
idle or done, or a move is under way) the button is disabled and the reason
shows beside it. Opening the menu reads `GET /v1/machines` and lists every
other machine (one that can't take a session now is disabled, with "its
watcher is offline" or "no move key yet") and, for a GitHub remote,
**Cloud**. A choice asks for confirmation, naming what moves, then sends
`POST /v1/sessions/{id}/move` with `X-Hub-Action: move` and reads
`GET /v1/moves/{id}` every 2 seconds for up to 3 minutes. The card shows the
state ("Moving to tower: packing on bluebox…"), the server's reason when it
refuses or the move fails, and, for a finished cloud move, **Open in
Claude** with the cloud session link. The 30-second list refresh shows the
latest move after that.
```

- [ ] **Step 7: Lint and commit**

Run: `make lint`
Expected: clean.

Run: `go test ./internal/server/ -count=1`
Expected: `ok`.

```bash
git add internal/server/dashboard/index.html internal/server/dashboard_test.go docs/server.md
git commit -m "$(cat <<'EOF'
Add a Move menu to the dashboard card

An expanded card offers Move to… with the other machines and, for a
GitHub remote, Cloud, disabled with the reason when the session can't
move. A confirmation names what moves; the card follows the move and
shows the cloud session link when a cloud move is done.

Refs: docs/dev/superpowers/plans/2026-10-03-move-session.md, Task 11

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

### Task 12: README, SPEC, and PLAN

**Files:**
- Modify: `README.md` (Use it, Upgrade)
- Modify: `docs/dev/SPEC.md` (new section 10)
- Modify: `docs/dev/PLAN.md` (as-built notes)

**Interfaces:** none (documentation only).

- [ ] **Step 1: README: use**

In `README.md`, section "Use it", after the bullet that ends
"add `remote_permissions = false` to `~/.config/sessionhub/config.toml`.", add:

```markdown
- `sessionhub move <id-prefix> tower` moves a session to `tower`: its conversation,
  its branch, and its uncommitted changes, resumed there in a new herdr
  workspace. `sessionhub move <id-prefix> cloud` hands a session in a GitHub
  repository to a new Claude Code cloud session. On the dashboard, expand
  the card and use **Move to…**. The session must be idle in herdr, the
  target needs a clean clone of the same repository under `~/Code` (or a
  `move_roots` entry in `~/.config/sessionhub/config.toml`), and both machines must
  run the same Claude Code version. The bundle is encrypted between the two
  machines; the server only relays it. Files such as `.env` stay behind, and
  the result says which.
```

- [ ] **Step 2: README: upgrade**

In `README.md`, before "### Upgrade to session actions", add:

```markdown
### Upgrade to moves

This release moves the database to schema version 9. Deploy with
`make deploy`, which backs up the database first and reinstalls the plugin
on `tower`; the first start upgrades the database. Then, on every other
machine, run `make install` and `sessionhub install-plugin`. The new watcher creates
`~/.config/sessionhub/move.key` and registers its public key within a minute.

Check the keys out of band: `sessionhub machine ls` on `tower` shows each machine's
`MOVE_KEY`, and `sessionhub move-key` on each machine prints its own. They must
match.

To roll back, stop the server, restore the database backup as described
above, install the previous binary on `tower`, and start the server; then
install the previous binary on every machine and run `sessionhub install-plugin`.
The version 8 binary refuses a version 9 database, so restoring the backup
is the only way back. `move.key` can stay; the old binary ignores it.
```

- [ ] **Step 3: SPEC**

In `docs/dev/SPEC.md`, after section "### 9. Session actions" (before "### 6. Machine
enrollment"), add:

```markdown
### 10. Moving a session
A session moves between machines with its conversation, its branch, and its uncommitted changes, and resumes in a herdr pane on the target: `sessionhub move <id> <machine>` or the dashboard card's **Move to…**. Only an idle or done session in herdr moves. The source's watcher pushes the branch, ends Claude with `/exit`, packs the transcript, its sidecar folder, the file history, `git diff --binary HEAD`, and the untracked files (secret-looking files such as `.env*`, `*.pem`, `*.key`, and `*.tfvars` never travel, tracked or untracked), and seals the bundle with X25519, HKDF-SHA256, and AES-256-GCM for the target machine's key; the server relays the ciphertext and deletes it when the move ends. The target finds a clean clone with the same remote, fast-forwards the branch, applies the changes, writes the transcript, and resumes while the move is still open; a failure on the target takes back only what the move did to the clone. The source restarts the session after a failure only once the sessionhub has recorded it, a target whose `done` the sessionhub did not record ends Claude again, and the source archives its copy once the move is done, so a session never runs in two places. For a GitHub remote, `sessionhub move <id> cloud` records uncommitted work on a sessionhub-owned branch without touching the working tree, never overwriting commits a cloud session pushed there, and starts a Claude Code cloud session with a hand-off prompt. The design is in `docs/dev/superpowers/specs/2026-10-03-move-session-design.md`.
```

- [ ] **Step 4: PLAN**

In `docs/dev/PLAN.md`, after the "## Session actions, as built" section and before
"## Tests and captured payloads", add:

```markdown
## Moves, as built

The design is in `docs/dev/superpowers/specs/2026-10-03-move-session-design.md`,
and the plan in `docs/dev/superpowers/plans/2026-10-03-move-session.md`. The
build settles these points the spec leaves open:

- Schema version 9 adds `moves` and `machines.move_key`. The column is
  `requested_by` (`BY` is an SQL keyword); `target_machine_id` and
  `source_closed_at` drive the claims. Rollback is restoring the database
  backup.
- The source learns how a move ended from one more `move-out` claim, the
  finish, offered once through the long poll and recorded in
  `source_closed_at`, so a watcher restart mid-move still archives or
  restores. A result the source posts itself closes its part.
- `requested` fails after 2 minutes; `packing`, `uploaded`, and `unpacking`
  after 10 minutes without a change. `cancelled` is defined and unused.
- Ownership moves when the target reports `done`, after herdr detects
  Claude. The target's first hook writes for the session get `409` until
  then; its next heartbeat registers the pane.
- The target runs every check that changes nothing (bundle, version,
  existing transcript, clone search, clean clone, including the files the
  patch creates) before it touches the clone. Its undo takes back only what
  the move did: the files it wrote (one changed since stays), the patch
  (`git apply -R`), and the branch (back to its old commit, or deleted when
  sessionhub created it, each with a compare-and-swap `update-ref`). It never runs
  `git reset` or `git clean`, and it never writes through a linked folder.
- A session never runs in two places. The source runs every check that can
  fail before `/exit` and checks the pane again right before it. After
  `/exit`, it restarts the session only once the sessionhub recorded the failure;
  an upload with an unknown outcome restarts nothing, and the finish
  restarts it after the move times out. The target starts Claude only while
  the move is `unpacking`, and ends it again when the sessionhub does not record
  its `done`.
- A restart or an archive that fails after the source's part closed is
  added to the ended move's detail as a note.
- Secret-looking files never travel, tracked or untracked: the patch and
  the cloud commit leave them out with `:(exclude,icase,glob)` pathspecs.
  Every `git diff` ignores the user's diff config (`--no-color
  --no-ext-diff --no-textconv --no-renames --full-index` and fixed
  prefixes).
- "Same remote" compares `origin` with the scheme, user, `.git`, and host
  case dropped. With several clones, the repository's path under its search
  root decides, ignoring case.
- A detached HEAD on the source fails. The source pushes only a branch with
  no upstream or one ahead of it, never forced. `sessionhub/cloud-<id8>`, which sessionhub
  owns, is replaced only with `--force-with-lease` against the commit sessionhub
  pushed there; when a cloud session pushed to it, the move fails instead.
- A project folder name over 200 characters uses the one existing folder
  with that prefix, else the move fails and says to start Claude there once.
- `claude --cloud`'s first `https://claude.ai/code/session_...` line is the
  link; sessionhub stops the command then and gives up after 2 minutes. Record what
  the live check saw here.
- The watcher re-registers its key every hour, so a database restored from a
  backup learns it back.
```

- [ ] **Step 5: Check the docs build nothing and break nothing**

Run: `go test $(go list ./... | grep -v internal/server) -count=1 && go test ./internal/server/ -count=1 && make lint`
Expected: every package `ok`, lint clean.
Then run `grep -n "move_roots\|move.key\|sessionhub move" README.md docs/*.md docs/dev/PLAN.md`
Expected: matches in `README.md`, `docs/cli.md`, `docs/client.md`,
`docs/plugin.md`, `docs/server.md`, and `docs/dev/PLAN.md`.

- [ ] **Step 6: Commit**

```bash
git add README.md docs/dev/SPEC.md docs/dev/PLAN.md
git commit -m "$(cat <<'EOF'
Document moving a session

README covers sessionhub move, the Move menu, and the upgrade to schema 9 with
the key check. SPEC gains section 10, and docs/dev/PLAN.md's as-built notes
record what the build settled.

Refs: docs/dev/superpowers/plans/2026-10-03-move-session.md, Task 12

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
EOF
)"
```

---

## Self-review against the spec

| Spec item | Where |
|---|---|
| Transport through the sessionhub, encrypted end to end; server and Cloudflare see ciphertext | Tasks 1, 4, 7, 8 |
| Watchers act through the control long poll, kinds `move-out` and `move-in` | Tasks 3, 4, 7, 8 |
| `sessionhub move <id> <machine>`, `sessionhub move <id> cloud`, dashboard **Move** menu | Tasks 10, 11 |
| Allowed only live in a herdr pane on a polling machine, idle or done | Task 3 (`TestCreateMoveRules`), Task 7 (`TestMoveOutRefusesBusyPane`) |
| Uncommitted work as a binary patch plus untracked files; branch history never rewritten | Tasks 2, 8 (`TestPrepareCloneFastForwardOnly`) |
| Target is an existing clone with the same remote; no automatic clone | Task 8 (`TestFindClone`) |
| Cloud only for GitHub; new cloud session with a hand-off prompt; local session ends | Tasks 3, 9 |
| X25519 key in `~/.config/sessionhub/move.key` (0600), created on first use | Task 1 |
| `PUT /v1/machines/self/move-key` on watcher start; fingerprints in `sessionhub machine ls` and `GET /v1/machines` | Tasks 3, 4, 7 |
| Ephemeral ECDH + static ECDH, HKDF-SHA256 (`sessionhub move v1`, salt move ID), AES-256-GCM with the move ID as additional data; version byte, ephemeral key, nonce | Task 1 |
| Target opens with the source's registered key; a bundle not sealed by the source fails | Task 1 (`TestSealOpen`), Task 8 (wrong sender) |
| Standard library crypto only | Task 1 |
| Threat model documented; fingerprints for out-of-band checks | Tasks 1, 10, 12 |
| Bundle entries: manifest, transcript, sidecar, file history, `changes.patch`, `untracked.tar` | Task 2 |
| Manifest fields | Task 2 (`Manifest`) |
| 64 MiB sealed limit; 10 MiB per untracked file; failure names the files | Task 2 (`TestBuildLimits`), Task 4 (`TestMoveBundleLimits`) |
| Secret-looking files not carried, tracked or untracked; listed in the manifest | Task 2 (`TestBuildSkipsSecrets`), Task 8 (`not carried:`), Task 9 (`TestPushCloudBranchLeavesTreeAlone`) |
| Schema v9 `moves` with the spec's columns | Task 3 |
| Bundles as files under the data directory, 0600, deleted at the end or after an hour | Task 4 (`TestMoveBundleDeletedOnEnd`) |
| States and transitions; cloud path | Tasks 3, 4 |
| The six routes and their access | Task 4 (`TestAuthMatrix`) |
| `done` hands ownership to the target, clears herdr fields, records `moved` | Task 3 (`TestClaimMoveFlow`) |
| 10-minute timeout in `packing` or `unpacking` | Task 3 (`TestExpireMoves`) |
| Source steps 1 to 6: pane check, repo, GitHub check, push, `/exit` with a 30 s wait, bundle and upload | Task 7 (`TestMoveOutEndsSessionBeforeUpload`, `TestMoveOutChecksBeforeExit`) |
| Source keeps its transcript until `done`, then moves it to `~/.local/state/sessionhub/moved/<id>/`, kept 30 days | Task 7 (`TestFinishDoneArchives`, `TestArchive`, `TestFinishArchiveFailureIsNoted`) |
| Cloud: temporary-index branch `sessionhub/cloud-<id8>`, `claude --cloud`, link captured, `done` with `cloud_url`, session ends `moved_to_cloud` | Tasks 3, 9 |
| Hand-off prompt format | Task 9 (`TestHandOffPrompt`) |
| A failure after step 5 restarts the session with `claude --resume` | Task 7 (`TestMoveOutRestartsAfterFailure`; only once the sessionhub recorded the failure: `TestMoveOutRestartsOnlyAfterRecordedFailure`), Task 9 (`TestCloudMoveFailureRestarts`) |
| A session is never resumed in two places | Task 7 (`TestMoveOutRestartsOnlyAfterRecordedFailure`, `TestFinishDoneArchives`), Task 8 (`TestMoveInStopsWhenMoveEnded`, `TestMoveInStopsWhenDoneNotRecorded`) |
| Target steps 1 to 9, including version check, clone search (depth 4, `move_roots`), dirty check, fetch and fast-forward, `git apply --binary`, untracked without overwrite, transcript exists, write, start | Task 8 |
| Target undo after step 5 leaves the clone as it was; source restarts | Task 8 (`TestPrepareCloneApplyUndo`, `TestPrepareCloneUndoDeletesCreatedBranch`, `TestApplyRefusesLinkedFolder`, `TestMoveInUndoAfterApplyFailure`, `TestMoveInUndoAfterWriteFailure`, `TestMoveInFailures`) |
| CLI: `sessionhub move` follows for 3 minutes; `sessionhub move --status`; `sessionhub move-key` | Task 10 |
| Dashboard: **Move to…**, other machines, **Cloud** for GitHub, disabled with a reason, confirmation, progress, cloud link | Task 11 |
| No MCP tool | not built |
| Security: machine tokens or cookie + `X-Hub-Action: move`; upload by source, download by target only; output cleaned | Tasks 4, 7, 10, 11 |
| Testing list (crypto, bundle, server, watcher, cloud, live) | Tasks 1 to 11; deploy notes |
| Out of scope: automatic clone, moving a busy session, bringing a cloud session back, non-GitHub cloud | not built; Tasks 9 and 12 record it |

Gaps checked and closed: the 64 KiB body limit and 30-second read timeout
on the bundle upload (Task 4: `isBundleUpload`, `SetReadDeadline`); the
client's 2-second limit and 16 MiB response cap (Task 5: separate bundle
calls); `server.New` called from plugin tests (Task 4: `SetMoveDir`, not a
new argument); the auth matrix's poll claiming fixture moves (Task 4:
`claimUntil` and fresh moves); `client.Config` no longer comparable with
`==` once it holds a slice (Task 5, which also adds `config_test.go` to its
commit); the `sessionhub machine ls` header test (Task 4); an upload whose answer
is lost (Task 7: the move is read back, and an unknown outcome restarts
nothing); the race between an upload's rename and the sweeper (the sweeper
keeps files of open moves); the existing `runWatcher` tests, which would
create a real `~/.config/sessionhub/move.key` (Task 7 sets `SESSIONHUB_CONFIG`); a
tracked secret's change (Task 2: pathspec excludes); the user's diff
config (Task 2: fixed diff flags); a patch's new file already on the
target (Task 8: `PatchNewFiles`); and a linked folder on the target
(Task 8: `lstat` on each folder).

Type and name consistency: `store.ClaimMove` returns
`(api.ControlClaim, bool, error)` in Tasks 3, 4, and 7;
`api.ControlClaim.Move` is a `*api.MoveClaim` whose `PeerKey` is the other
machine's key in Tasks 3, 7, and 8; `move.Seal(sender, recipient, id, data)`
and `move.Open(recipient, sender, id, sealed)` match in Tasks 1, 7, and 8;
`move.PrepareClone(ctx, root, branch, head)`, `move.CheckClone(ctx, root,
carried)`, and `move.PatchNewFiles(patch)` match Task 8's mover;
`move.PushCloudBranch` returns `(string, []string, error)` in Task 9 and its
mover; `(*mover).postResult` returns whether the sessionhub recorded the result
and `(*mover).finish` takes the client in Tasks 7 to 9; `newMover(socket, claudeDir, stateDir, roots,
key, logger)` matches in Tasks 7 to 9 and `runWatcher`; `resume.StartResumed`
returns `resume.ControlResult` in Tasks 6 to 8; `client.GetMoveBundle(ctx,
id, w)` returns `(int64, error)` in Tasks 5 and 8.

## Deploy and live check (notes for the controller)

These steps run on the real machines. They are not a task: the controller
runs them with the user's approval, after Task 12. Use a scratch repository
and scratch Claude sessions only; never move a session that holds real work.

1. **Full suite.** Run `go test $(go list ./... | grep -v internal/server) -count=1`,
   then `go test ./internal/server/ -count=1`, then `make lint`, and record
   the package count and `0` failures. If `internal/server` fails with
   `bind: address already in use`, wait 60 seconds and run it alone again.
2. **Deploy.** Run `make deploy` (backs up `sessionhub.db` on `tower`, swaps the
   binary, restarts the server, reinstalls the plugin there). On `bluebox`, run
   `make install` and `sessionhub install-plugin`.
3. **Confirm the upgrade and the keys.** On `tower`:
   `sqlite3 ~/.local/share/sessionhub/sessionhub.db 'PRAGMA user_version'` prints `9`.
   Within a minute, `sessionhub machine ls` on `tower` shows a 16-character
   `MOVE_KEY` for `bluebox` and `tower`, and `sessionhub move-key` on each machine prints
   the same value. `grep 'move: key' ~/.local/state/sessionhub/watcher.log` shows
   one `registered` line per machine. Record `claude --version` on both
   machines; they must match before step 5.
4. **Scratch repository.** Make a private scratch repository on GitHub, and
   clone it on both machines under `~/Code` (the path may differ only in
   case, for example `~/Code/Scratch/move-check` and
   `~/Code/scratch/move-check`). Both clones need push access over SSH in
   `BatchMode` (no passphrase prompt).
5. **Move `bluebox` → `tower`.** In herdr on `bluebox`, start Claude in the clone,
   tell it "Remember the word PELICAN.", and let it edit a tracked file. Add
   one staged change, one unstaged change, one untracked file, and a `.env`
   file by hand. When the agent is idle, run `sessionhub move <prefix> tower`.
   Expected: one line per state, then `done: ... not carried: .env`. On
   `tower`: a herdr workspace `sessionhub: <title>` runs Claude; ask "Which word did
   I ask you to remember?" and get PELICAN. `git status` and `git diff HEAD`
   on `tower` match what `bluebox` had, without `.env`. On `bluebox`:
   `ls ~/.local/state/sessionhub/moved/` shows the move ID, and `claude --resume
   <id>` in the clone finds no session. `sessionhub ls` shows the session on `tower`.
   On `tower`, `ls ~/.local/share/sessionhub/moves/` is empty again.
6. **Move back from the dashboard.** On a phone, expand the card, choose
   **Move to…** → **bluebox**, confirm, and watch the card's line go to
   "Moved to bluebox.". The conversation continues on `bluebox` with PELICAN.
7. **A failure restores.** Edit a tracked file in `tower`'s clone, then run
   `sessionhub move <prefix> tower` on `bluebox`. Expected: `failed: check clone: ...
   uncommitted changes ...`, exit 1, and within a minute the session runs
   again on `bluebox` (`sessionhub ls` shows it live there). Undo the edit on `tower`.
8. **Refusals.** Give the session a long task (`sleep 30` through Bash) and
   run `sessionhub move <prefix> tower`: the server answers with "the agent is
   working". The dashboard's **Move to…** is disabled with "Wait until the
   agent is idle or done.".
9. **Cloud, if the account allows it.** With the session idle on `bluebox` and an
   uncommitted change, run `sessionhub move <prefix> cloud`. Expected: `done: the
   session continues in the cloud at https://claude.ai/code/session_...`;
   the link opens a cloud session whose first prompt is the hand-off text;
   GitHub has `sessionhub/cloud-<id8>` with the change; `git status` on `bluebox` is
   unchanged. If cloud sessions are not allowed, record the failure line
   (`start cloud: ...; the session resumes here`) and check the session runs again on
   `bluebox`. Either way, record exactly what `claude --cloud` printed, and if
   the link format differs from `https://claude.ai/code/session_...`, fix
   `cloudLinkRE` and the server's check with a test before continuing, and
   update the cloud bullet in `docs/dev/PLAN.md`'s as-built notes.
10. **Evidence.** Write `docs/dev/evidence/move-session.md` with the commands and
    outputs from steps 1 to 9 (no tokens, no key material beyond
    fingerprints), and commit it with the usual trailers, adding only that
    file. Delete the scratch sessions, the scratch repository's clones, the
    `sessionhub/cloud-*` branch, and the scratch GitHub repository.

Rollback, if a step fails badly: on `tower`, stop the server, restore the
newest `sessionhub.db.bak-*` over `sessionhub.db`, delete `sessionhub.db-wal` and `sessionhub.db-shm`,
install the previous binary, and start the server; on `bluebox` and `tower`,
install the previous binary and run `sessionhub install-plugin`. The version 8
binary refuses a version 9 database, so restoring the backup is the only
rollback. A session that was mid-move when you rolled back may have its
transcript on neither machine's normal path: look in
`~/.local/state/sessionhub/moved/` on the source and move it back by hand.
