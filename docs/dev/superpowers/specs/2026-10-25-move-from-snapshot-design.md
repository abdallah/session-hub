# Move from a snapshot

Status: draft, 2026-10-25.

## Problem

A move needs the source machine's watcher online: it packs the transcript,
patch, and untracked files at move time. `CreateMove` refuses with "the
sessionhub watcher on %q is offline" (`internal/store/moves.go:301`). If the source is asleep,
off, or unreachable, the session cannot continue anywhere else.

## Approach

Each watcher uploads a sealed snapshot of every live session it owns when a
turn ends. If the source is offline, a move to another machine uses the
newest snapshot instead of asking the source to pack. When the source comes
back, the existing finish step (`move-out` finish claim) closes its copy.

Facts that shape the design:

- Two machines have move keys today (`togh`, `blu`). A snapshot is sealed
  once per other machine with a move key: one copy today.
- `Seal` binds the move ID into the key and AAD (`internal/move/seal.go:29`),
  and the target checks `Manifest.MoveID` against the claim
  (`internal/plugin/move.go:594`). Each snapshot gets its own `mv_` ID, and
  a move from a snapshot reuses that ID as its move ID. No crypto or
  manifest format change.
- A live move pushes the branch (`internal/plugin/move.go:164`). Snapshots
  do not push, since a push per turn triggers CI. Commits missing from
  `origin/<branch>` go in the bundle as `git/commits.bundle` (`git bundle`).
- Measured on `togh`: past bundles 270–370 KB; transcripts touched in a day
  65 MB raw, largest 23 MB raw / 7.5 MB compressed.

## Work items

- [ ] **Store: `snapshots` table**, schema bump (~0.5d). Columns:
  `id` (`mv_…`), `session_id` (cascade), `source_machine_id`,
  `recipient_machine_id`, `taken_at`, `size`, `head`, `branch`. One row per
  session and recipient; a new upload replaces the row and file. Files live
  in `snapshots/<id>.sealed` next to the DB, mode `0600`.
- [ ] **Server: `PUT /v1/sessions/{id}/snapshots/{recipient}`** (~0.5d).
  Machine auth, the session's own machine only. Header carries the `mv_` ID;
  reject an ID already used by a move or snapshot. Same size cap and
  streaming as `putMoveBundle` (`internal/server/moves.go:181`).
- [ ] **Watcher: snapshot on turn end** (~0.5d). When a session goes `idle`
  or `done`, build with `move.Build`, add `git/commits.bundle`, seal per
  recipient, upload. Skip if the transcript size, HEAD, and patch hash are
  unchanged since the last upload. At most one upload per session per
  10 minutes; the last turn of a burst still uploads once the 10 minutes
  pass.
- [ ] **Bundle: unpushed commits** (~0.5d). `Build` writes
  `git/commits.bundle` for `origin/<branch>..HEAD` when it is not empty;
  `Checkout.Apply` (`internal/move/target.go:302`) fetches from it before
  checking out `Head`. Bump `Manifest.V`; an old target refuses with
  `NewerBundleError`.
- [ ] **Move from a snapshot** (~0.5d). In `CreateMove`, if the source
  watcher is offline and a snapshot for the target exists, create the move
  with `id` = snapshot ID, state `uploaded`, and hard-link the snapshot file
  to `moves/<id>.sealed`. The target claims `move-in` as today. The move
  response and row carry `from_snapshot` and `snapshot_taken_at`. Cloud
  targets stay refused when the source is offline.
- [ ] **Source returns** (~0.5d). The source's `move-out` finish claim
  already ends the pane and archives the transcript. Verify, with a test, that
  a source offline for days still gets the finish, that no move timeout
  drops it, and that its heartbeat for the moved session gets `409` until it
  claims.
- [ ] **Dashboard and CLI** (~0.25d). In the Move menu, an offline source
  with a snapshot shows "from snapshot, 12m old". The confirm text names what
  is lost: anything after that time. `sessionhub move` prints the same.
- [ ] **Cleanup** (~0.25d), in the existing once-a-minute sweep
  (`internal/server/moves.go:72`):
  - session ends: delete its snapshots;
  - move done: delete the session's snapshots;
  - snapshot older than 14 days: delete;
  - total over 1 GB: delete oldest first, log each.
- [ ] **Docs** (~0.25d): `docs/server.md` Moves and schema sections,
  `docs/cli.md`, `docs/upgrading.md` (schema bump, deploy server first).

Total: about 3.5 days.

## Decisions

- Full bundle per upload, rate-limited to one per 10 minutes per session,
  not append-only transcript segments. Estimated traffic: 100–200 MB/day
  across active sessions. Segments would cut that to ~20 MB/day for
  ~1 extra day of work; revisit if the traffic shows up as a problem.
- No opt-out setting in v1. Add one if a repo must never leave its machine.

## Acceptance criteria

- [ ] With `blu`'s watcher stopped, moving a session from `blu` to `togh`
  resumes it on `togh` with the transcript, uncommitted changes, and
  unpushed commits as of the last snapshot.
- [ ] Restarting `blu`'s watcher afterwards ends the old pane and archives
  the transcript; no second copy resumes.
- [ ] Snapshots of an ended session are gone within one sweep.
- [ ] The server cannot read a snapshot (sealed to the recipient's key).
- [ ] Snapshot uploads never push to `origin`.

## Refs

- `docs/dev/superpowers/specs/2026-10-03-move-session-design.md`
- Commit `81d6858` (failed moves in the inbox)
