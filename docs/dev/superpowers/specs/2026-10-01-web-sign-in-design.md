# Web sign-in design

Date: 2026-10-01. Status: approved in chat section by section; the user
accepted the written spec in advance.

## Goal

Sign a browser in to the dashboard without copying a long-lived secret, and
revoke any signed-in browser while the server runs, without editing files or
restarting.

## Decisions

| Topic | Decision | Why |
|---|---|---|
| Who signs in | You now, other people later. | Design for one person, keep the path open. |
| How a browser signs in | A one-time link from `sessionhub login`, confirmed with a button, shown with a QR code. | Machine tokens become the root of trust; nothing long-lived is copied. |
| The read token | Removed. A `read_token` setting is ignored with a warning. | Nothing but the dashboard used it, and it was a static shared secret. |
| Session lifetime | Sliding 30 days from the last request. | Rare re-sign-ins; a forgotten device expires. |
| Managing sessions | `sessionhub login ls` and `sessionhub login rm` with a machine token; **Sign out** on the dashboard ends only the current browser's session. | One person's browser can't sign out another's. |
| QR code | `rsc.io/qr`, rendered with half-block characters. | Small encoder with no dependencies. |

## Storage

Schema version 5, upgraded in place like versions 2 to 4.

`login_codes`:

| Column | Notes |
|---|---|
| `code_hash` | Primary key. SHA-256 of the code, hex. |
| `name` | The name the session gets. |
| `machine_id` | The machine whose token created the code. Foreign key, `ON DELETE CASCADE`. |
| `created_at`, `expires_at` | `expires_at` is 10 minutes after creation. |
| `used_at` | Null until the code is used. |

`web_sessions`:

| Column | Notes |
|---|---|
| `id` | Primary key. 8 random bytes, hex (16 characters). Public, used to list and revoke. |
| `token_hash` | Unique. SHA-256 of the cookie token, hex. |
| `name` | Unique among sessions in the table. Same rules as machine names. |
| `machine_id` | The machine whose link created it. Foreign key, `ON DELETE CASCADE`. |
| `created_at`, `last_used_at`, `expires_at` | `expires_at` is 30 days after `last_used_at`. |

- A code is 16 random bytes in unpadded lowercase base32 (26 characters). A
  session token is `hub_s_` followed by 32 random bytes in base64url.
- Only hashes are stored.
- Revoking a session deletes its row. Expired sessions and codes are deleted
  whenever a code or session is created or sessions are listed.
- Removing a machine (`sessionhub machine rm`) deletes the codes and sessions it
  created.

## Server

### Routes

| Route | Access | Does |
|---|---|---|
| `POST /v1/logins` `{"name": "phone"}` | Machine token | Creates a code; returns `201` with `{"url": "<public_url>/login/<code>", "name": "phone", "expires_at": "..."}`. `400` for a bad name, `409` if a live session has the name, `429` if 5 unused, unexpired codes exist. |
| `GET /login/{code}` | Public | HTML page. Valid code: "Sign in this browser as **phone**?" with a **Sign in** button (a form that posts to the same URL). Otherwise `410` with "This link expired or was already used. Run `sessionhub login` again." Changes nothing. |
| `POST /login/{code}` | Public, `Origin` check | Uses the code once, creates the session, sets the cookie, `303` to `/`. Used, expired, or unknown code: `410` page. Missing `Origin` or one not equal to `public_url`: `403`. A session name taken since the code was made: `409` page. |
| `GET /v1/web-sessions` | Machine token | `{"sessions": [{id, name, machine, created_at, last_used_at, expires_at}]}`. Never the token or hash. |
| `DELETE /v1/web-sessions/{id}` | Machine token | `204`, or `404` for an unknown ID. |
| `POST /logout` | Session cookie with `X-Hub-Action: sign-out` | Deletes the current session, clears the cookie, `204`. |

- The code in `/login/{code}` is matched against `^[a-z2-7]{26}$` before any
  lookup; anything else gets the `410` page.
- Using a code runs in one transaction: `UPDATE login_codes SET used_at=?
  WHERE code_hash=? AND used_at IS NULL AND expires_at>?`, then the session
  insert. When two presses race, the first wins and the second gets `410`.
- Every failed `POST /login/{code}` is logged with the client IP from
  `Cf-Connecting-Ip`, else the remote address.
- The login pages use the same security headers as the dashboard, with their
  own CSP: `default-src 'none'; style-src '<hash>'; form-action 'self';
  base-uri 'none'; frame-ancestors 'none'`.

### The cookie

`__Host-hub_session`: the session token, `Path=/`, `HttpOnly`, `Secure`,
`SameSite=Lax`, `Max-Age` 30 days. The name uses the `__Host-` prefix to
block cookie tossing from sibling subdomains.

`Lax` replaces `Strict` so that opening the dashboard from a link in another
app sends the cookie. Writes stay protected by the custom `X-Hub-Action`
header, which needs a CORS preflight the server never grants.

### Authentication

- The cookie branch of `authenticate` hashes the `__Host-hub_session` value and
  looks it up in `web_sessions`. An unknown or expired session counts as no
  credentials.
- **Sliding expiry.** When a valid session's `last_used_at` is more than one
  hour old, the server sets `last_used_at` to now, `expires_at` to 30 days
  later, and sends the cookie again with a fresh `Max-Age`.
- A bearer token matches only machine tokens. A `hub_r_...` token gets `401`.
- The session cookie authorizes reads, `POST /v1/sessions/{id}/remote-control`
  with `X-Hub-Action: remote-control` (unchanged), and `POST /logout` with
  `X-Hub-Action: sign-out`. Nothing else. In particular it never authorizes
  `/v1/logins` or `/v1/web-sessions`.
- `GET /` without a valid session shows the signed-out page (`401`): "Run
  `sessionhub login --name <device>` on a machine with sessionhub, then open the link it
  prints." A `?token=` parameter is ignored and never compared.
- `read_token` in `server.toml` and `SESSIONHUB_READ_TOKEN` are ignored. If either
  is set, the server logs one warning at startup. The server no longer
  refuses to start without it.

## CLI

On any machine with a client config (`server_url` and a machine token):

- `sessionhub login --name <name> [--no-qr] [--json]`: creates a code and prints the
  link, when it expires, and a QR code of the link. `--no-qr` omits the QR
  code. `--json` prints only `{"url":...,"name":...,"expires_at":...}`.
  `--name` is required. A `409` prints "a browser session named <name>
  exists; revoke it with `sessionhub login rm <name>` or pick another name". Nothing
  is queued: if the server is unreachable, the command fails.
- `sessionhub login ls [--json]`: name, ID, machine, created, last used, expires.
- `sessionhub login rm <name|id>`: resolves the name through the list, then
  deletes by ID. No confirmation.

The QR code uses error correction level L, one module per character width,
upper and lower half blocks (`▀`, `▄`, `█`, space) for two rows per line, and
a quiet zone of 2 modules. It is printed on stdout after the link.

## Dashboard

- The header shows "Signed in as **<name>**" and a **Sign out** button. The
  name comes from a new read endpoint field: `GET /v1/sessions` responses
  carry an `X-Hub-Session-Name` header when the caller used a session cookie.
- **Sign out** sends `POST /logout` with `X-Hub-Action: sign-out`, then shows
  the signed-out message.
- If a poll gets `401`, the dashboard stops polling and shows the signed-out
  message instead of an empty list.

## Upgrade

1. Back up the database. The first start upgrades it to version 5.
2. Keep the `read_token` line until the upgrade is confirmed: the new server
   ignores it, and the rollback binary needs it.
3. Run `sessionhub login --name windows` and `sessionhub login --name phone`, and open each
   link on its device.
4. Delete the `read_token` line.

Rollback: stop the server, restore the database backup and the previous
binary, and start it.

Clients (hooks, herdr watcher, MCP) are unchanged: they use machine tokens.

## Failure handling

| Condition | Behavior |
|---|---|
| Link opened after 10 minutes, or again after use | `410` page. |
| A link previewer fetches the link | `GET` changes nothing. |
| **Sign in** pressed twice or on two devices | First wins; the second gets `410`. |
| Foreign or missing `Origin` on `POST /login` | `403`. |
| Name taken by a live session | `sessionhub login` gets `409` with advice. |
| Cookie for a revoked or expired session | `401`; the dashboard shows the signed-out message. |
| Server unreachable | `sessionhub login` fails; not queued. |
| `read_token` still configured | One warning at startup; ignored. |

## Security invariants

- The database never holds a code or session token, only hashes.
- No response body contains a session token; only `Set-Cookie` does.
- A session cookie authorizes no write other than Remote Control and sign-out,
  each with its own header value.
- A session cookie never reaches `/v1/logins` or `/v1/web-sessions`.

## Testing

- **Store:** v4 to v5 upgrade; one-time use and expiry of codes; the 5-code
  limit; sliding expiry including the one-hour write limit; unique names;
  revoke; cleanup of expired rows; cascade on machine removal.
- **Server:** the auth table covers every new route with a session cookie, a
  machine token, a `hub_r_...` token, and no credentials; the `Origin` check;
  the two-press race; `GET /login/{code}` changes nothing; `401` after
  revocation; `?token=` ignored; the `read_token` warning; no token in any
  response body.
- **CLI:** `--name` required; `--json` output; `409` message; `rm` by name
  and by ID; the QR renderer's output matches the encoder's matrix.
- **Dashboard:** pure functions under node for the signed-out state and the
  sign-out request with its header.
- **Live:** deploy, sign in from the phone by scanning the QR code, revoke
  that session from `bluebox`, and check the phone shows the signed-out message
  within one poll.

## Out of scope

- Passkeys and Cloudflare Access.
- Roles, and a devices panel on the dashboard.
- API tokens for scripts.
