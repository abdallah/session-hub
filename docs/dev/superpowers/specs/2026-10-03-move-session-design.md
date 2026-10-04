# Move a session to another machine or to the cloud

Date: 2026-10-03. Status: approved in chat (option 2: relay through sessionhub,
encrypted end to end between machines); the user asked for implementation
without further questions.

## Goal

Move a Claude Code session from one machine to another, with its full
conversation, its branch, and its uncommitted changes, and resume it there in
a herdr pane. For a repository with a GitHub remote, also hand a session off
to a Claude Code cloud session.

## Facts this rests on

From the Claude Code docs (about v2.1.285) and checks on 2026-10-03:

- A transcript is `~/.claude/projects/<encoded cwd>/<id>.jsonl`, where the
  encoded name replaces every non-alphanumeric character of the cwd with `-`
  (names over 200 characters are cut and get a hash suffix). Next to it,
  `<id>/` holds `subagents/` and `tool-results/`. Checkpoint snapshots are in
  `~/.claude/file-history/<id>/`.
- `claude --resume <id>` searches the current project first, then every
  project, and refuses when more than one project holds the ID. The entry
  format is internal and changes between versions, so both machines must run
  the same Claude Code version.
- A session must never be resumed in two places at once.
- The CLI cannot push an existing local session to the cloud. `claude --cloud
  "<task>"` starts a new cloud session that clones the GitHub remote at a
  branch; `claude --teleport <id>` brings a cloud session to a terminal. Cloud
  sessions need claude.ai sign-in (not an API key), GitHub access through the
  Claude GitHub App or `/web-setup`, and the organization setting that allows
  cloud sessions. Non-GitHub repositories can't push back.
- Repository paths differ between machines (`~/Code/OTGS/...` on `bluebox`,
  `~/Code/otgs/...` on `tower`), so the target is found by Git remote, not
  path.
- `bluebox` reaches `tower` over SSH; `tower` does not reach `bluebox`. Traffic to
  `sessionhub.example.com` passes through Cloudflare's tunnel, which terminates TLS.

## Decisions

| Topic | Decision |
|---|---|
| Transport | Through the sessionhub server, encrypted end to end between the two machines' watchers. The server and Cloudflare see only ciphertext. |
| Who acts | The herdr watcher on each machine, through the existing control long poll (new kinds `move-out` and `move-in`). |
| Start a move | `sessionhub move <id> <machine>` or `sessionhub move <id> cloud` on any machine; a **Move** menu on the dashboard card. |
| When allowed | The session is live in a herdr pane on a polling machine, and its agent is idle or done. Never while working or blocked. |
| Uncommitted work | Carried as a binary patch plus untracked files. The user's branch history is never rewritten. |
| Target repository | An existing clone on the target with the same remote. No automatic clone in this version. |
| Cloud | Only for a GitHub remote. A new cloud session seeded with a hand-off prompt; the local session ends. |

## Keys and encryption

- Each machine has an X25519 key pair in `~/.config/sessionhub/move.key` (mode
  0600), created on first use. The watcher registers the public key with the
  server on start: `PUT /v1/machines/self/move-key` `{"public_key": "<base64>"}`
  (machine token). `sessionhub machine ls` and `GET /v1/machines` show each key's
  fingerprint (first 16 hex characters of its SHA-256).
- A bundle is sealed for the target machine: an ephemeral X25519 key pair,
  ECDH with the target's public key, plus ECDH between the source's static
  key and the target's key, both fed to HKDF-SHA256 (info `sessionhub move v1`, salt
  the move ID) to make a 32-byte key; AES-256-GCM over the whole bundle, with
  the move ID as additional data. The sealed form starts with a version byte,
  the ephemeral public key, and the nonce.
- The target opens it with its private key and the source's registered
  public key, so a bundle that wasn't sealed by the source machine fails.
- Only Go's standard library (`crypto/ecdh`, `crypto/hkdf`, `crypto/aes`,
  `crypto/cipher`). No new dependency.
- Threat model, documented: the server and Cloudflare relay ciphertext and
  can't read it. Whoever controls the server or the tunnel could swap a
  public key; `sessionhub machine ls` fingerprints let you check keys out of band.

## The bundle

A tar archive, before sealing:

| Entry | Content |
|---|---|
| `manifest.json` | move ID, session ID, source machine, source cwd, repository root relative path of the cwd, remote URL, branch, HEAD SHA, Claude Code version, created time, list of files |
| `transcript/<id>.jsonl` | the transcript |
| `transcript/<id>/` | the sidecar folder, if present |
| `file-history/<id>/` | checkpoint snapshots, if present |
| `git/changes.patch` | `git diff --binary HEAD` (staged and unstaged) |
| `git/untracked.tar` | untracked, not ignored files |

Limits: the sealed bundle is at most 64 MiB; untracked files over 10 MiB
each, or over the limit together, fail the move with a message naming them.
Files that look like secrets (`.env*`, `*.pem`, `*.key`, `*.tfvars`) are not
carried; the manifest lists them so the target can say what to copy by hand.

## Server

Schema version 9 adds `moves`: `id` (`mv_` + random), `session_id`,
`source_machine_id`, `target` (a machine name or `cloud`), `state`,
`detail`, `created_at`, `updated_at`, `bundle_size`, `cloud_url`, `by`
(who started it). Bundles are stored as files under the server's data
directory (`moves/<id>.sealed`, mode 0600), never in the database, and are
deleted when the move ends or after one hour.

States: `requested` → `packing` → `uploaded` → `unpacking` → `done`, or
`failed` (with `detail` and the failed step), or `cancelled`. A cloud move
goes `requested` → `packing` → `done` or `failed`.

| Route | Access | Does |
|---|---|---|
| `POST /v1/sessions/{id}/move` `{"target": "tower"}` or `{"target": "cloud"}` | Machine token, or session cookie with `X-Hub-Action: move` | Checks the rules in "When allowed", that the target machine is polling and has a move key, and that no move is open for the session; creates the move and queues `move-out` for the source machine. `409` with a reason otherwise. |
| `GET /v1/moves/{id}` | Read | The move's state and detail. |
| `PUT /v1/moves/{id}/bundle` | Machine token of the source machine | Stores the sealed bundle (at most 64 MiB; `413` above), sets `uploaded`, queues `move-in` for the target. |
| `GET /v1/moves/{id}/bundle` | Machine token of the target machine | Streams the sealed bundle. |
| `POST /v1/moves/{id}/result` `{"state", "detail", "cloud_url"}` | Machine token of the machine whose step it is | Advances or fails the move. On `done` for a machine move, the session's owner becomes the target machine and its herdr fields clear until the next snapshot; an event `moved` is recorded on the session. |
| `PUT /v1/machines/self/move-key` | Machine token | Registers this machine's public key. |

A move left in `packing` or `unpacking` for 10 minutes fails with
"timed out".

## Source machine (`move-out`)

1. Check that the pane still runs the session and its agent is idle or done
   (same checks as message delivery). Otherwise fail.
2. Read the session's cwd, find the repository root, remote URL, branch, and
   HEAD. Fail if the cwd is not in a Git repository with a remote.
3. For a cloud move, fail unless the remote host is `github.com`.
4. Push the branch if it has commits not on its upstream (`git push` with
   `BatchMode`; set upstream if it has none). Fail on a push error.
5. End the Claude session in the pane by submitting `/exit` with herdr, and
   wait up to 30 seconds for the agent to leave the pane.
6. **Machine move:** build the bundle, seal it for the target, upload it, and
   report `uploaded`. Keep the source transcript, its sidecar and file
   history until the move is `done`, then move them to
   `~/.local/state/sessionhub/moved/<id>/` (kept 30 days), so the session can't be
   resumed in two places.
7. **Cloud move:** if there are uncommitted changes, record them on a new
   branch `sessionhub/cloud-<first 8 of id>` made from HEAD with a temporary index
   (`GIT_INDEX_FILE`), so the working tree and the current branch don't
   change, and push it; otherwise use the current branch. Run
   `claude --cloud "<hand-off prompt>"` in the cwd, capture the cloud
   session link from its output, and report `done` with `cloud_url`. The
   local transcript is kept in place; sessionhub marks the session ended with
   reason `moved_to_cloud`.

The hand-off prompt:

```
Continue work moved from a local Claude Code session (<first 8 of id> on <machine>).
Branch: <branch>.
Recap: <recap, or "none">
Last request: <last prompt, or "none">
Status: <latest report's done / in flight / waiting on, if any>
```

If any step after step 5 fails, the source restarts the session in a pane
with `claude --resume <id>`, so a failed move leaves it where it was.

## Target machine (`move-in`)

1. Download and open the bundle; fail on any check (wrong sender, bad
   ciphertext, manifest mismatch).
2. Fail if this machine's Claude Code version differs from the manifest's.
3. Find a clone with the same remote: search `~/Code` (and any roots in the
   client config key `move_roots`) up to depth 4 for Git repositories whose
   normalized remote URL matches (scheme, user, `.git` suffix and case of
   the host ignored). None: fail with "no clone of <remote> on <machine>".
   Several: use the one whose path ends with the same relative path as the
   source, else fail listing them.
4. Fail if that clone has uncommitted changes or untracked files that the
   bundle would overwrite.
5. `git fetch`, check out the branch (creating a tracking branch if needed),
   and move it to the bundle's HEAD (fast-forward only; fail otherwise).
6. Apply `changes.patch` with `git apply --binary`, then extract the
   untracked files, refusing to overwrite.
7. Fail if a transcript with this session ID already exists anywhere under
   `~/.claude/projects` on this machine.
8. Write the transcript, sidecar and file history into
   `~/.claude/projects/<encoded target cwd>/` and `~/.claude/file-history/`.
9. Start `claude --resume <id>` in a new herdr pane at the target cwd (the
   same pane logic as `sessionhub resume`), and report `done`.

On a failure after step 5, the target reports the step and leaves the clone
as it was (it checks out the previous branch again and removes applied
files), and the source restarts the session.

## Interfaces

- **CLI:** `sessionhub move <id-or-prefix> <machine|cloud>` starts the move and
  follows it, printing each state, for up to 3 minutes. `sessionhub move --status
  <move-id>` prints a move. `sessionhub move-key` prints this machine's key
  fingerprint.
- **Dashboard:** an expanded card gets **Move to…** with the other machines
  and, for a GitHub remote, **Cloud**; disabled with a reason when the rules
  don't allow it. A confirmation names what moves. Progress shows on the card;
  a finished cloud move shows the cloud session link.
- **MCP:** none in this version.

## Security

- Only machine tokens and signed-in browsers with `X-Hub-Action: move` can
  start a move. Upload and download are limited to the move's source and
  target machines.
- The bundle is sealed between the two machines; the server stores only
  ciphertext and deletes it when the move ends or after an hour.
- No secret-looking files are carried.
- Every step's output that reaches the dashboard or terminal is cleaned.

## Testing

- **Crypto:** seal and open round trip; a wrong target key, a wrong source
  key, a changed byte, and a changed move ID all fail.
- **Bundle:** build and extract from a temporary Git repository with
  staged, unstaged, binary, and untracked changes; secret-looking files
  skipped; size limits.
- **Server:** the auth table for every new route; move state machine;
  ownership transfer on `done`; bundle size limit; expiry; one open move per
  session.
- **Watcher:** `move-out` and `move-in` against a fake herdr and temporary
  Git repositories and a temporary `~/.claude`; failure at each step restores
  the session; the "no clone", "dirty clone", and "transcript exists"
  failures.
- **Cloud:** the temporary-index branch leaves the working tree and current
  branch unchanged; non-GitHub remotes refused; the hand-off prompt; output
  parsing for the cloud link with a fake `claude`.
- **Live:** move a scratch session from `bluebox` to `tower` and back; check the
  conversation continues and uncommitted changes arrived; a cloud move for a
  scratch GitHub repository, if the account allows it.

## Out of scope

- Cloning a repository on the target automatically.
- Moving a session that is working or blocked.
- Bringing a cloud session back automatically (use `claude --teleport`).
- Non-GitHub cloud moves.
