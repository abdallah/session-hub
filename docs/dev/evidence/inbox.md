# Inbox: deploy and live check

Date: 2026-10-01. Commit: `3d62c15`, deployed to `tower` with `make deploy`.

## Deploy

- `make deploy` backed up the database to `sessionhub.db.bak-20261001T095024`, and
  the first start upgraded it to schema version 6
  (`sqlite3 sessionhub.db "pragma user_version"` prints `6`).
- `make install` put the same build on `bluebox`.

## Live check

1. After the upgrade, `sessionhub inbox` printed `nothing needs you`, as expected:
   `turn_ended_at` and `blocked_at` are not backfilled.
2. This session (`7684db37`) called `report_progress` with a `waiting_on`
   item. `GET /v1/inbox` then returned
   `{"blocked": 0, "waiting": 1, "finished": 0}`, with the item in group
   `waiting`, `since` `2026-10-01T09:54:14.312515103Z`, and the report's
   `waiting_on` text.
3. `POST /v1/inbox/7684db37-.../dismiss` with that `since` returned `204`, and
   the next `GET /v1/inbox` returned all counts at `0`.

The CLI calls (`sessionhub inbox`) timed out during the check because the Cloudflare
tunnel was slow at the time: `GET /healthz` took 6.4 s from `tower` itself,
while the server log shows each `/v1/inbox` request served in about 3 ms. The
API calls above used `curl` with a 60-second limit.

## Not yet checked live

- A finished turn bringing a dismissed item back (it needs this session's
  next turn to end).
- The dashboard **Inbox** tab on a phone. The scratch-server browser check at
  360 px passed during Task 7 (headless Chrome on `bluebox`).
