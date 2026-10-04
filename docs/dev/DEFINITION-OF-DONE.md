# Definition of done

A task in this repo is done when every item below is true, not when the code
works on one machine. This list adapts `wpml-org/app/docs/dev/DEFINITION-OF-DONE.md`
to a small Go project. Skip an item only with a one-line reason in the task
report, for example "no runtime surface: docs-only change". A silent skip is
what this list exists to prevent.

## The checklist

- [ ] **Correctness is proven mechanically, not asserted.** Where a property
  holds across many instances (every write endpoint requires a machine token,
  every hook exits 0, every event kind round-trips), a table-driven test or a
  script checks all of them and reports `0` failures. A claim that a real
  consumer tolerates something (herdr, Claude Code, Cloudflare) was checked
  against that consumer, not reasoned about.
- [ ] **Behavior was observed end to end**, not only unit-tested. The task runs
  the real binary against the real neighbor (a running `sessionhub server`, the live
  herdr socket, a real `claude` run) and records the commands and their output
  in `docs/dev/evidence/task-<N>.md`.
- [ ] **The full suite is green, and the report says so in numbers.** Paste the
  summary of `make test` (packages `ok`, test count, `0` failures) and
  `make lint` (`gofmt -l` prints nothing, `go vet` clean). There is no CI, so
  the local run is the only gate there is.
- [ ] **The branch stays working after every commit.** Each task leaves
  `make build` and `make test` green on its own, with no dependency on a later
  task. Unimplemented subcommands return a clear "not implemented" error; they
  never panic.
- [ ] **Commits are small and self-documenting.** One concern each, with the
  root cause and any gotcha in the body, and `Refs: docs/dev/PLAN.md` or the task number.
- [ ] **Docs are updated in the same change.** A new command, config key, path,
  or endpoint is documented in `docs/` (and the README once it exists) in the
  same commit. A gotcha learned the hard way goes where the next session will
  hit it: a pointer comment, `docs/dev/NOTES.md`, or the component doc.
- [ ] **Scope stayed at the plan.** No speculative features. A nice-to-have
  goes in `docs/dev/IDEAS.md`, not in code.
- [ ] **Long-running work is resumable.** Anything that runs in the background
  (the plugin watcher, the retry queue) survives being killed at any point
  without losing queued data or double-sending.
- [ ] **Every new or changed test walks the full arc.** It observes the start
  state (the store, the queue file, the socket), acts, asserts what must *not*
  have happened as well as what must, and reads the end state back from where
  the user would see it (the API response or CLI output). Wrong paths are
  tested too: bad token, unknown session, unreachable server, malformed payload.
- [ ] **The plan's status is updated.** The task's line in the SDD ledger is
  complete, and `docs/dev/PLAN.md` is corrected wherever the implementation had to
  differ, with the reason.

## Why this exists

The work should check itself, so review is a spot-check rather than a full
re-read. A reviewer, or the next session with no context, can trust a task
that meets this list.
