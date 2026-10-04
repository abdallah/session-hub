# Task 7 evidence: dashboard

Date: 2026-09-30. The real `bin/sessionhub server` built from this tree ran on
`127.0.0.1:18787` with a temp database, `SESSIONHUB_STALE_AFTER=6s`, and read token
`hub_r_evidence-read-token-0123456789abcdef` (an evidence-only value).

## Seeding

Two machines (`tower`, `bluebox`) were added with `sessionhub machine add`. Sessions were
posted with curl: a live one with a report (its note contains `<b>tests</b> &`
to prove escaping), a blocked one with a report, a stale one (posted, then no
write for 7 s), a live idle one, and an ended one (an `ended` event). All posts
returned `201` or `200`.

## Cookie exchange, with curl

Headers of interest, from `curl -si`:

```
GET /                       (no cookie)   -> 401 text/html, body says to use ?token=
GET /?token=wrong                         -> 401 text/html, no Set-Cookie

GET /?token=<read token>
HTTP/1.1 303 See Other
Location: /
Referrer-Policy: no-referrer
Set-Cookie: hub_read=<read token>; Path=/; Max-Age=34560000; HttpOnly; Secure; SameSite=Strict

GET / with the cookie
HTTP/1.1 200 OK
Cache-Control: no-store
Content-Security-Policy: default-src 'none'; script-src 'sha256-cs3Pnx21OFLkO9nmGPZLHCC8d3y98wq8hz+mVCPnGhY='; style-src 'sha256-IL8N5urLPWcj7w8OkwT6hv1mLPNDw54bqrSG8zVy/0M='; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'
Content-Type: text/html; charset=utf-8

GET /v1/sessions   cookie hub_read=<read token>  -> 200
GET /v1/sessions   cookie hub_read=nope          -> 401 {"error":"missing or invalid bearer token"}
POST /v1/sessions  cookie hub_read=<read token>  -> 401 (the cookie never authorizes a write)
```

`Max-Age=34560000` is 400 days. The server log for these requests shows the
path only (`GET / 303 ... machine=read`); `grep -c` of the read token in the
log printed `0`.

## Page at 360 px

Rendered with `chrome-headless-shell` (Playwright's Chromium 1223) at
`--window-size=360,1200`, which loaded `/?token=...`, followed the 303, ran the
page's script under the CSP above, and fetched `/v1/sessions` with the cookie.
Screenshot: `docs/dev/evidence/task-7-dashboard-360.png`.

What the screenshot shows, checked by eye:

- No horizontal overflow. The long title, long path, and long command wrap or
  scroll inside their card; the badge and Copy button stay on screen.
- Machines are sorted by name. Within `tower`, the blocked session comes before
  the live one. Within `bluebox`, live comes before stale.
- The ended session is collapsed under **Ended (1)**.
- The report shows Done (3 of 5, "+2 more"), In flight, Waiting on, and Note.
  The note `<b>tests</b> next & then evidence` appears as literal text.
- Status shows as colour and as a text label.

Plain `google-chrome --headless=new --window-size=360,...` does not work for
this check: it enforces a wider minimum layout width and cropped the image, so
the first screenshot overflowed. That was a false result; the headless shell
does honour 360 px.

Not checked in a browser: dark mode (the CSS uses `prefers-color-scheme` with
its own variables) and the Copy button's clipboard path. A real Android device
is the remaining check.

## Tests

`internal/server/dashboard_test.go` covers the exchange, cookie attributes, bad
tokens, the cookie on `/v1/sessions`, no cookie write, no token in the log, the
`text/html` type, the CSP hashes matching the served inline script and style,
and no external URLs, `style=` attributes, or `innerHTML` in the page.
`auth_test.go` now sends a `cookie` and `badcookie` credential to every route
and requires a case for `GET /`.
