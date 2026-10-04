# Web sign-in implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Sign a browser in to the dashboard with a one-time link from
`sessionhub login`, and revoke any signed-in browser while the server runs, in place
of the static read token.

**Architecture:** Schema v5 adds `login_codes` and `web_sessions`, holding only
SHA-256 hashes. A machine token creates a code (`POST /v1/logins`); the
browser confirms it on `/login/{code}`, which sets the `hub_session` cookie
(30-day sliding expiry). `authenticate` resolves that cookie instead of the
read token, which is removed and only warned about. `sessionhub login` prints the
link and a half-block QR code; `sessionhub login ls|rm` manage sessions; the
dashboard shows who is signed in and a **Sign out** button.

**Tech Stack:** Go 1.25, `modernc.org/sqlite` v1.50.0, `rsc.io/qr` (new, the
only new dependency), standard library otherwise. Dashboard: one embedded HTML
page, no libraries, pure functions tested under `node`.

**Spec:** `docs/dev/superpowers/specs/2026-10-01-web-sign-in-design.md`

## Global Constraints

- Schema version 5, upgraded in place like versions 2 to 4.
- A code is 16 random bytes in unpadded lowercase base32 (26 characters),
  matched against `^[a-z2-7]{26}$` before any lookup. A session token is
  `hub_s_` followed by 32 random bytes in base64url. A session ID is 8 random
  bytes, hex (16 characters).
- Only hashes are stored (`HashToken`, SHA-256 hex). No response body contains
  a session token; only `Set-Cookie` does.
- A code expires 10 minutes after creation. `POST /v1/logins` returns `429`
  when 5 unused, unexpired codes exist. The limit is global, not per machine.
- A session expires 30 days after `last_used_at`. A request slides it only
  when `last_used_at` is more than one hour old, and then re-sends the cookie
  with a fresh `Max-Age`.
- Session names follow the machine-name rules (`ValidMachineName`) and are
  unique among rows in `web_sessions`. Expired rows are deleted before the
  uniqueness check, so an expired name is free.
- Cookie: `hub_session`, `Path=/`, `HttpOnly`, `Secure`, `SameSite=Lax`,
  `Max-Age` 30 days.
- The session cookie authorizes reads, `POST /v1/sessions/{id}/remote-control`
  with `X-Hub-Action: remote-control`, and `POST /logout` with
  `X-Hub-Action: sign-out`. Nothing else. It never reaches `/v1/logins` or
  `/v1/web-sessions`.
- A bearer token matches only machine tokens. A `hub_r_...` token gets `401`.
- `read_token` in `server.toml` and `SESSIONHUB_READ_TOKEN` are ignored. If either is
  set, the server logs exactly one warning at startup. The key stays in the
  TOML struct so an old file still loads.
- Login pages: `Content-Security-Policy: default-src 'none'; style-src
  '<hash>'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'`, plus
  `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`, and
  `Referrer-Policy: same-origin` (see Review Focus 1).
- The `Origin` check compares the header with `public_url` (trailing slash
  trimmed) for exact equality. A missing or different `Origin` gets `403`.
- QR code: `rsc.io/qr`, error correction level L, half blocks (`▀`, `▄`, `█`,
  space), two module rows per line, quiet zone of 2 modules, printed on stdout
  after the link. A light module is drawn filled; a dark module is a space.
- Every dashboard string is inserted with `textContent`. The page has no
  `style=` or `href=` attributes and one inline script.
- Resolved spec ambiguities (record them in `docs/dev/PLAN.md` in Task 7):
  - A redeem that finds the name taken rolls back, so the code stays usable
    until it expires, and the page answers `409`.
  - A machine token on `POST /logout` gets `403`: it has no browser session.
  - Any machine token may list and revoke any browser session.
  - `GET /` still accepts a machine bearer token (least change).
  - The request log labels a browser caller `web:<name>`.
  - Login page failures (`403`, `409`, `410`) are HTML pages, since a browser
    form receives them.
  - `sessionhub login rm <x>` matches a name first, then an ID.
  - The request log prints `/login/<code>` in place of a real code.
- Follow `docs/dev/DEFINITION-OF-DONE.md`: `make test` and `make lint` green after
  every commit, docs in the same commit where the task owns them. Run
  `gofmt -w` on every Go file you edit before `make lint`; the plan's code
  blocks are not guaranteed to be aligned the way `gofmt` wants. After you
  delete code, remove any import the compiler reports as unused.
- On this machine, parallel test runs can hit port exhaustion
  (`address already in use`). If a run fails that way, wait 60 seconds and run
  it again before you debug.
- Commit trailer on every commit:
  `Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h`.

## Review Focus

1. **`Referrer-Policy: no-referrer` on the login page.** With that policy,
   browsers send `Origin: null` on a form `POST`, so every **Sign in** press
   would get `403`. The login pages send `Referrer-Policy: same-origin`, which
   keeps the code out of any cross-site `Referer` and still sends the real
   `Origin`. Pinned in Task 3 (`TestLoginPageChangesNothing` checks the
   header).
2. **A phone still holding only the old `hub_read` cookie after the upgrade.**
   It must get the signed-out page (`401`, HTML naming `sessionhub login`), not an
   error or an empty list. Pinned in Task 2 (`TestOldReadCookieIsSignedOut`).
3. **`Origin` variants.** A trailing slash, `http://` instead of `https://`,
   and `null` are `403`, and the refused press does not use the code: the
   same code still works with the right `Origin`. Pinned in Task 3
   (`TestLoginSubmitOrigin`).
4. **Reusing a name after its session expired.** `UNIQUE(name)` alone would
   make an expired `phone` row block a new `phone` forever. Cleanup runs
   before the check and before the insert. Pinned in Task 1
   (`TestExpiredRowsDeleted`).
5. **A live code in the request log.** The logger prints the path, so an
   unused code would sit in `journalctl` for its 10 minutes. The logger
   prints `/login/<code>` instead. Pinned in Task 3
   (`TestLoginCodeNotLogged`).

## Dependencies and parallelism

| Task | Needs | Can run alongside |
|---|---|---|
| 1 Store: schema v5, codes, sessions | none | none |
| 2 Server: session cookie replaces the read token | 1 | 4 |
| 3 Server: login links, pages, session API, sign-out | 2 | 4 |
| 4 Client methods and QR renderer | 1 | 2, 3 |
| 5 `sessionhub login` command | 4 | 6 |
| 6 Dashboard | 3 | 5 |
| 7 README, SPEC, PLAN | all | none |

Tasks 2 and 3 both edit `internal/server/auth_test.go`, `helpers_test.go`, and
`docs/server.md`, so run them in order.

---

### Task 1: Store: schema v5, login codes, and web sessions

**Files:**
- Modify: `internal/api/types.go` (header constants, `LoginIn`, `Login`,
  `WebSession`, `WebSessionList`)
- Modify: `internal/store/store.go` (errors, `schemaVersion = 5`, `schemaV5`,
  migration step)
- Create: `internal/store/weblogin.go`
- Create: `internal/store/weblogin_test.go`
- Modify: `internal/store/concurrency_test.go` (`rollbackV5`, chained from
  `rollbackV4`)
- Modify: `internal/store/store_test.go` (`user_version` `5`, `TestTokens`)
- Modify: `internal/store/machines.go` (remove `ReadTokenPrefix`)
- Modify: `docs/server.md` (Database section)

**Interfaces:**
- Consumes: `NewToken`, `HashToken`, `ValidMachineName`, `invalidf`,
  `formatTS`, `parseTS`, `ErrTooMany`, `ErrNotFound` (existing).
- Produces:
  - `api.HeaderActionSignOut = "sign-out"`,
    `api.HeaderSessionName = "X-Hub-Session-Name"`.
  - `api.LoginIn{Name string}`, `api.Login{URL, Name string; ExpiresAt
    time.Time}`, `api.WebSession{ID, Name, Machine string; CreatedAt,
    LastUsedAt, ExpiresAt time.Time}`, `api.WebSessionList{Sessions
    []WebSession}`.
  - `store.ErrNameTaken` (server maps to `409`), `store.ErrCodeGone` (`410`).
  - `store.SessionTokenPrefix = "hub_s_"`, `store.LoginCodeTTL = 10 *
    time.Minute`, `store.WebSessionTTL = 30 * 24 * time.Hour`,
    `store.WebSessionTouchEvery = time.Hour`, `store.MaxOpenLoginCodes = 5`.
  - `func NewLoginCode() (string, error)`,
    `func ValidLoginCode(code string) bool`.
  - `func (s *Store) CreateLoginCode(ctx context.Context, machineID int64,
    name string) (code string, expiresAt time.Time, err error)`
  - `func (s *Store) LoginCodeName(ctx context.Context, code string) (string,
    error)` (read-only; `ErrCodeGone`)
  - `func (s *Store) RedeemLoginCode(ctx context.Context, code string) (token
    string, ws api.WebSession, err error)` (`ErrCodeGone`; `ErrNameTaken`
    with `ws.Name` set)
  - `func (s *Store) WebSessionByToken(ctx context.Context, token string) (ws
    api.WebSession, touched bool, err error)` (`ErrNotFound`)
  - `func (s *Store) ListWebSessions(ctx context.Context) ([]api.WebSession,
    error)`
  - `func (s *Store) RevokeWebSession(ctx context.Context, id string) error`
    (`ErrNotFound`)

- [ ] **Step 1: Add the API types**

In `internal/api/types.go`, extend the const block after
`HeaderActionRemoteControl`:

```go
	HeaderAction              = "X-Hub-Action"
	HeaderActionRemoteControl = "remote-control"
	// HeaderActionSignOut is the X-Hub-Action value the dashboard sends with
	// POST /logout.
	HeaderActionSignOut = "sign-out"
	// HeaderSessionName carries the browser session's name on GET
	// /v1/sessions when the caller used the session cookie.
	HeaderSessionName = "X-Hub-Session-Name"
```

Append to the end of the file:

```go
// LoginIn is the body of POST /v1/logins.
type LoginIn struct {
	Name string `json:"name"`
}

// Login is a one-time sign-in link, from POST /v1/logins.
type Login struct {
	URL       string    `json:"url"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
}

// WebSession is one signed-in browser. It never carries the token or its
// hash.
type WebSession struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Machine    string    `json:"machine"` // the machine whose link created it
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// WebSessionList is the response of GET /v1/web-sessions.
type WebSessionList struct {
	Sessions []WebSession `json:"sessions"`
}
```

- [ ] **Step 2: Write the failing store tests**

In `internal/store/concurrency_test.go`, make `rollbackV4` call a new
`rollbackV5` first, and add `rollbackV5` below it:

```go
// rollbackV4 removes what schema v4 added, so a test can set user_version
// to 3 or lower and reopen the file as an older release left it.
func rollbackV4(t *testing.T, s *Store) {
	t.Helper()
	rollbackV5(t, s)
	for _, q := range []string{
		"DROP TABLE session_digests",
		"ALTER TABLE sessions DROP COLUMN last_prompt",
		"ALTER TABLE sessions DROP COLUMN last_prompt_at",
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// rollbackV5 removes what schema v5 added, so a test can set user_version
// to 4 or lower and reopen the file as an older release left it.
func rollbackV5(t *testing.T, s *Store) {
	t.Helper()
	for _, q := range []string{"DROP TABLE login_codes", "DROP TABLE web_sessions"} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}
```

In `internal/store/store_test.go`, change the `user_version` check in
`TestOpenPragmasAndSchema` from `{"user_version", "4"}` to
`{"user_version", "5"}`, and in `TestTokens` replace the read-token check:

```go
	if r, _ := NewToken(ReadTokenPrefix); !strings.HasPrefix(r, "hub_r_") {
		t.Errorf("read token %q", r)
	}
```

with:

```go
	if r, _ := NewToken(SessionTokenPrefix); !regexp.MustCompile(`^hub_s_[A-Za-z0-9_-]{43}$`).MatchString(r) {
		t.Errorf("session token %q", r)
	}
```

Create `internal/store/weblogin_test.go`:

```go
package store

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestMigrateV4ToV5(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	tok, _, err := s.AddMachine(ctx, "tower", "", "")
	if err != nil {
		t.Fatal(err)
	}
	rollbackV5(t, s)
	if _, err := s.db.Exec("PRAGMA user_version = 4"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var v int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 5 {
		t.Fatalf("user_version %d %v, want 5", v, err)
	}
	m, err := s2.MachineByToken(ctx, tok)
	if err != nil {
		t.Fatalf("machine after upgrade: %v", err)
	}
	code, _, err := s2.CreateLoginCode(ctx, m.ID, "phone")
	if err != nil {
		t.Fatalf("create code after upgrade: %v", err)
	}
	if _, _, err := s2.RedeemLoginCode(ctx, code); err != nil {
		t.Fatalf("redeem after upgrade: %v", err)
	}
}

func TestLoginCodeFormat(t *testing.T) {
	a, err := NewLoginCode()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewLoginCode()
	if len(a) != 26 || !ValidLoginCode(a) || a == b {
		t.Errorf("codes %q %q", a, b)
	}
	for _, bad := range []string{"", strings.ToUpper(a), a[:25], a + "a", "0" + a[1:], "1" + a[1:], "8" + a[1:], a[:25] + "="} {
		if ValidLoginCode(bad) {
			t.Errorf("ValidLoginCode(%q) = true", bad)
		}
	}
}

func TestCreateLoginCode(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	for _, bad := range []string{"", "has space", "-lead", strings.Repeat("a", 65), "a/b"} {
		if _, _, err := e.s.CreateLoginCode(ctx, e.tower.ID, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q: err %v, want ErrInvalid", bad, err)
		}
	}
	code, exp, err := e.s.CreateLoginCode(ctx, e.tower.ID, "phone")
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(e.clock.Now().Add(10 * time.Minute)) {
		t.Errorf("expires_at %v, want now+10m", exp)
	}
	// Only the hash is stored.
	var n int
	if err := e.s.db.QueryRow(`SELECT COUNT(*) FROM login_codes WHERE code_hash = ?`, HashToken(code)).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows with the code's hash: %d %v, want 1", n, err)
	}
	if err := e.s.db.QueryRow(`SELECT COUNT(*) FROM login_codes WHERE code_hash = ? OR name = ?`, code, code).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows holding the plain code: %d %v, want 0", n, err)
	}
	if name, err := e.s.LoginCodeName(ctx, code); err != nil || name != "phone" {
		t.Errorf("LoginCodeName = %q %v", name, err)
	}
	// LoginCodeName changes nothing: the code is still unused.
	if _, _, err := e.s.RedeemLoginCode(ctx, code); err != nil {
		t.Errorf("redeem after a read: %v", err)
	}
}

func TestRedeemLoginCodeOnce(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	code, _, _ := e.s.CreateLoginCode(ctx, e.tower.ID, "phone")
	now := e.clock.Now()
	tok, ws, err := e.s.RedeemLoginCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^hub_s_[A-Za-z0-9_-]{43}$`).MatchString(tok) {
		t.Errorf("token %q", tok)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(ws.ID) || ws.Name != "phone" || ws.Machine != "tower" ||
		!ws.CreatedAt.Equal(now) || !ws.LastUsedAt.Equal(now) || !ws.ExpiresAt.Equal(now.Add(30*24*time.Hour)) {
		t.Errorf("session %+v", ws)
	}
	var n int
	if err := e.s.db.QueryRow(`SELECT COUNT(*) FROM web_sessions WHERE token_hash = ?`, HashToken(tok)).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows with the token's hash: %d %v", n, err)
	}
	if err := e.s.db.QueryRow(`SELECT COUNT(*) FROM web_sessions WHERE token_hash = ? OR id = ? OR name = ?`, tok, tok, tok).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows holding the plain token: %d %v", n, err)
	}
	if _, _, err := e.s.RedeemLoginCode(ctx, code); !errors.Is(err, ErrCodeGone) {
		t.Errorf("second redeem: %v, want ErrCodeGone", err)
	}
	if _, err := e.s.LoginCodeName(ctx, code); !errors.Is(err, ErrCodeGone) {
		t.Errorf("LoginCodeName after use: %v, want ErrCodeGone", err)
	}
	if _, _, err := e.s.RedeemLoginCode(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, ErrCodeGone) {
		t.Errorf("unknown code: %v, want ErrCodeGone", err)
	}
	if list, _ := e.s.ListWebSessions(ctx); len(list) != 1 {
		t.Errorf("sessions after the refused redeems: %d, want 1", len(list))
	}
}

func TestLoginCodeExpiry(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	code, _, _ := e.s.CreateLoginCode(ctx, e.tower.ID, "phone")
	e.clock.Advance(10*time.Minute - time.Second)
	if _, err := e.s.LoginCodeName(ctx, code); err != nil {
		t.Errorf("at 9m59s: %v", err)
	}
	e.clock.Advance(time.Second)
	if _, err := e.s.LoginCodeName(ctx, code); !errors.Is(err, ErrCodeGone) {
		t.Errorf("at 10m: %v, want ErrCodeGone", err)
	}
	if _, _, err := e.s.RedeemLoginCode(ctx, code); !errors.Is(err, ErrCodeGone) {
		t.Errorf("redeem at 10m: %v, want ErrCodeGone", err)
	}
}

func TestLoginCodeLimit(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	var codes []string
	for i, m := range []int64{e.tower.ID, e.tower.ID, e.tower.ID, e.bluebox.ID, e.bluebox.ID} {
		c, _, err := e.s.CreateLoginCode(ctx, m, "phone")
		if err != nil {
			t.Fatalf("code %d: %v", i, err)
		}
		codes = append(codes, c)
	}
	// The limit counts every machine's codes.
	if _, _, err := e.s.CreateLoginCode(ctx, e.bluebox.ID, "phone"); !errors.Is(err, ErrTooMany) {
		t.Fatalf("sixth code: %v, want ErrTooMany", err)
	}
	// A used code no longer counts.
	if _, _, err := e.s.RedeemLoginCode(ctx, codes[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.CreateLoginCode(ctx, e.tower.ID, "tablet"); err != nil {
		t.Fatalf("after a redeem: %v", err)
	}
	// Expired codes are deleted and no longer count.
	e.clock.Advance(10 * time.Minute)
	for i := range 5 {
		if _, _, err := e.s.CreateLoginCode(ctx, e.tower.ID, "tablet"); err != nil {
			t.Fatalf("after expiry, code %d: %v", i, err)
		}
	}
	var n int
	if err := e.s.db.QueryRow(`SELECT COUNT(*) FROM login_codes`).Scan(&n); err != nil || n != 5 {
		t.Errorf("stored codes %d %v, want 5 (expired ones deleted)", n, err)
	}
}

func TestWebSessionNameTaken(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	first, _, _ := e.s.CreateLoginCode(ctx, e.tower.ID, "tablet")
	other, _, _ := e.s.CreateLoginCode(ctx, e.bluebox.ID, "tablet")
	if _, _, err := e.s.RedeemLoginCode(ctx, other); err != nil {
		t.Fatal(err)
	}
	// A new code for a live name is refused.
	if _, _, err := e.s.CreateLoginCode(ctx, e.tower.ID, "tablet"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("code for a live name: %v, want ErrNameTaken", err)
	}
	// A code made before the name was taken: refused, and not used up.
	_, ws, err := e.s.RedeemLoginCode(ctx, first)
	if !errors.Is(err, ErrNameTaken) || ws.Name != "tablet" {
		t.Fatalf("redeem of a taken name: %v %+v, want ErrNameTaken with the name", err, ws)
	}
	if _, err := e.s.LoginCodeName(ctx, first); err != nil {
		t.Errorf("code after a refused redeem: %v, want still usable", err)
	}
	list, _ := e.s.ListWebSessions(ctx)
	if len(list) != 1 {
		t.Fatalf("sessions %d, want 1", len(list))
	}
	if err := e.s.RevokeWebSession(ctx, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, ws, err := e.s.RedeemLoginCode(ctx, first); err != nil || ws.Machine != "tower" {
		t.Errorf("redeem after the revoke: %+v %v", ws, err)
	}
}

func TestWebSessionSlidingExpiry(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	code, _, _ := e.s.CreateLoginCode(ctx, e.tower.ID, "phone")
	start := e.clock.Now()
	tok, _, _ := e.s.RedeemLoginCode(ctx, code)

	e.clock.Advance(30 * time.Minute)
	ws, touched, err := e.s.WebSessionByToken(ctx, tok)
	if err != nil || touched || !ws.LastUsedAt.Equal(start) {
		t.Fatalf("at +30m: %+v touched=%v %v; want untouched", ws, touched, err)
	}
	e.clock.Advance(31 * time.Minute)
	ws, touched, err = e.s.WebSessionByToken(ctx, tok)
	slid := start.Add(61 * time.Minute)
	if err != nil || !touched || !ws.LastUsedAt.Equal(slid) || !ws.ExpiresAt.Equal(slid.Add(30*24*time.Hour)) {
		t.Fatalf("at +61m: %+v touched=%v %v; want slid", ws, touched, err)
	}
	// The slide was stored.
	e.clock.Advance(30 * time.Minute)
	if ws, touched, _ = e.s.WebSessionByToken(ctx, tok); touched || !ws.LastUsedAt.Equal(slid) {
		t.Errorf("at +91m: %+v touched=%v; want the stored slide", ws, touched)
	}
	// Expired exactly at expires_at.
	e.clock.Advance(slid.Add(30 * 24 * time.Hour).Sub(e.clock.Now()))
	if _, _, err := e.s.WebSessionByToken(ctx, tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("at expires_at: %v, want ErrNotFound", err)
	}
	for _, bad := range []string{"", "hub_s_unknown", tok + "x"} {
		if _, _, err := e.s.WebSessionByToken(ctx, bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("token %q: %v, want ErrNotFound", bad, err)
		}
	}
}

func TestListAndRevokeWebSessions(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	c1, _, _ := e.s.CreateLoginCode(ctx, e.tower.ID, "windows")
	c2, _, _ := e.s.CreateLoginCode(ctx, e.bluebox.ID, "phone")
	tok1, ws1, _ := e.s.RedeemLoginCode(ctx, c1)
	_, ws2, _ := e.s.RedeemLoginCode(ctx, c2)
	list, err := e.s.ListWebSessions(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "phone" || list[0].Machine != "bluebox" ||
		list[1].Name != "windows" || list[1].ID != ws1.ID || list[0].ID != ws2.ID {
		t.Fatalf("list %+v %v; want phone (bluebox) then windows (tower)", list, err)
	}
	if err := e.s.RevokeWebSession(ctx, "0000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke unknown: %v, want ErrNotFound", err)
	}
	if err := e.s.RevokeWebSession(ctx, ws1.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.WebSessionByToken(ctx, tok1); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked token: %v, want ErrNotFound", err)
	}
	if list, _ := e.s.ListWebSessions(ctx); len(list) != 1 || list[0].Name != "phone" {
		t.Errorf("after revoke: %+v", list)
	}
}

func TestExpiredRowsDeleted(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	code, _, _ := e.s.CreateLoginCode(ctx, e.tower.ID, "phone")
	if _, _, err := e.s.RedeemLoginCode(ctx, code); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(30*24*time.Hour + time.Second)
	// The expired "phone" row no longer holds the name.
	code, _, err := e.s.CreateLoginCode(ctx, e.tower.ID, "phone")
	if err != nil {
		t.Fatalf("code for an expired name: %v", err)
	}
	if _, ws, err := e.s.RedeemLoginCode(ctx, code); err != nil || ws.Name != "phone" {
		t.Fatalf("redeem for an expired name: %+v %v", ws, err)
	}
	var n int
	if err := e.s.db.QueryRow(`SELECT COUNT(*) FROM web_sessions`).Scan(&n); err != nil || n != 1 {
		t.Errorf("web_sessions rows %d %v, want 1 (the expired row deleted)", n, err)
	}
	// Listing deletes expired sessions and codes.
	if _, _, err := e.s.CreateLoginCode(ctx, e.tower.ID, "tablet"); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(30*24*time.Hour + time.Second)
	if list, err := e.s.ListWebSessions(ctx); err != nil || len(list) != 0 {
		t.Errorf("list after expiry: %+v %v", list, err)
	}
	for _, table := range []string{"web_sessions", "login_codes"} {
		if err := e.s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s rows %d %v, want 0", table, n, err)
		}
	}
}

func TestRemoveMachineDeletesItsLogins(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	c1, _, _ := e.s.CreateLoginCode(ctx, e.tower.ID, "windows")
	tok1, _, _ := e.s.RedeemLoginCode(ctx, c1)
	if _, _, err := e.s.CreateLoginCode(ctx, e.tower.ID, "tablet"); err != nil {
		t.Fatal(err)
	}
	c2, _, _ := e.s.CreateLoginCode(ctx, e.bluebox.ID, "phone")
	tok2, _, _ := e.s.RedeemLoginCode(ctx, c2)

	if err := e.s.RemoveMachine(ctx, "tower"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.WebSessionByToken(ctx, tok1); !errors.Is(err, ErrNotFound) {
		t.Errorf("tower's session after machine rm: %v, want ErrNotFound", err)
	}
	if ws, _, err := e.s.WebSessionByToken(ctx, tok2); err != nil || ws.Machine != "bluebox" {
		t.Errorf("bluebox's session after tower rm: %+v %v", ws, err)
	}
	var n int
	if err := e.s.db.QueryRow(`SELECT COUNT(*) FROM login_codes WHERE machine_id = ?`, e.tower.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("tower's codes %d %v, want 0", n, err)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/store/ -count=1`
Expected: FAIL to build with `undefined: SessionTokenPrefix`,
`undefined: NewLoginCode`, and similar.

- [ ] **Step 4: Add schema v5 and the errors**

In `internal/store/store.go`, add to the error `var` block after
`ErrRequestClosed`:

```go
	// ErrNameTaken: a browser session with the name exists. 409.
	ErrNameTaken = errors.New("name taken")
	// ErrCodeGone: the sign-in code is unknown, expired, or used. 410.
	ErrCodeGone = errors.New("sign-in link expired or already used")
```

Change `const schemaVersion = 4` to `const schemaVersion = 5`, add after
`schemaV4`:

```go
// schemaV5 adds web sign-in: one-time login codes and browser sessions. Only
// SHA-256 hashes of codes and session tokens are stored. Removing a machine
// deletes the codes and sessions it created.
const schemaV5 = `
CREATE TABLE login_codes (
	code_hash  TEXT PRIMARY KEY,          -- SHA-256 hex of the code
	name       TEXT NOT NULL,             -- the name the session gets
	machine_id INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,             -- created_at + 10 minutes
	used_at    TEXT                       -- NULL until the code is used
);
CREATE INDEX login_codes_machine ON login_codes(machine_id);
CREATE TABLE web_sessions (
	id           TEXT PRIMARY KEY,        -- 8 random bytes, hex; public
	token_hash   TEXT NOT NULL UNIQUE,    -- SHA-256 hex of the cookie token
	name         TEXT NOT NULL UNIQUE,
	machine_id   INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	created_at   TEXT NOT NULL,
	last_used_at TEXT NOT NULL,
	expires_at   TEXT NOT NULL            -- last_used_at + 30 days
);
CREATE INDEX web_sessions_machine ON web_sessions(machine_id);
`
```

In `migrate`, after the `if v < 4 { ... }` block:

```go
	if v < 5 {
		if _, err := tx.ExecContext(ctx, schemaV5); err != nil {
			return fmt.Errorf("migrate schema to v5: %w", err)
		}
	}
```

In `internal/store/machines.go`, replace the prefix block:

```go
// Token prefixes: machine tokens and the dashboard read token.
const (
	MachineTokenPrefix = "hub_m_"
	ReadTokenPrefix    = "hub_r_"
)
```

with:

```go
// MachineTokenPrefix starts every machine token. Browser session tokens use
// SessionTokenPrefix (weblogin.go).
const MachineTokenPrefix = "hub_m_"
```

- [ ] **Step 5: Implement `internal/store/weblogin.go`**

```go
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// SessionTokenPrefix starts every browser session token, the value of the
// hub_session cookie.
const SessionTokenPrefix = "hub_s_"

const (
	// LoginCodeTTL is how long a sign-in link works.
	LoginCodeTTL = 10 * time.Minute
	// WebSessionTTL is how long a browser session lasts after its last use.
	WebSessionTTL = 30 * 24 * time.Hour
	// WebSessionTouchEvery is how old last_used_at must be before a request
	// slides the expiry, so a polling dashboard writes at most once an hour.
	WebSessionTouchEvery = time.Hour
	// MaxOpenLoginCodes caps unused, unexpired codes across all machines.
	MaxOpenLoginCodes = 5
)

var (
	loginCodeRE  = regexp.MustCompile(`^[a-z2-7]{26}$`)
	codeEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)
)

// NewLoginCode returns 16 random bytes in unpadded lowercase base32.
func NewLoginCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(codeEncoding.EncodeToString(b)), nil
}

// ValidLoginCode reports whether code has the shape NewLoginCode makes. The
// server checks it before any lookup.
func ValidLoginCode(code string) bool { return loginCodeRE.MatchString(code) }

func newSessionID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// execer is the part of *sql.DB and *sql.Tx that deleteExpiredLogins uses.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// deleteExpiredLogins removes expired codes and sessions. It runs whenever a
// code or session is created and when sessions are listed, and always before
// a name check, so an expired session never holds its name.
func deleteExpiredLogins(ctx context.Context, db execer, now string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM login_codes WHERE expires_at <= ?`, now); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `DELETE FROM web_sessions WHERE expires_at <= ?`, now)
	return err
}

// nameTaken reports whether a browser session holds name. Call it after
// deleteExpiredLogins.
func nameTaken(ctx context.Context, tx *sql.Tx, name string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_sessions WHERE name = ?`, name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("a browser session named %q exists: %w", name, ErrNameTaken)
	}
	return nil
}

// CreateLoginCode makes a one-time sign-in code for a browser session named
// name, created by machineID. It returns the code, which only the caller
// sees; the database keeps its hash.
func (s *Store) CreateLoginCode(ctx context.Context, machineID int64, name string) (code string, expiresAt time.Time, err error) {
	if !ValidMachineName(name) {
		return "", time.Time{}, invalidf("name %q: use 1-64 letters, digits, '.', '_', or '-', starting with a letter or digit", name)
	}
	code, err = NewLoginCode()
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.Now()
	expiresAt = now.Add(LoginCodeTTL)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	if err := deleteExpiredLogins(ctx, tx, formatTS(now)); err != nil {
		return "", time.Time{}, err
	}
	if err := nameTaken(ctx, tx, name); err != nil {
		return "", time.Time{}, err
	}
	var open int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM login_codes WHERE used_at IS NULL`).Scan(&open); err != nil {
		return "", time.Time{}, err
	}
	if open >= MaxOpenLoginCodes {
		return "", time.Time{}, fmt.Errorf("%d sign-in links are unused; use one or wait up to 10 minutes: %w", open, ErrTooMany)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO login_codes (code_hash, name, machine_id, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		HashToken(code), name, machineID, formatTS(now), formatTS(expiresAt)); err != nil {
		return "", time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", time.Time{}, err
	}
	return code, expiresAt, nil
}

// LoginCodeName returns the session name of an unused, unexpired code, or
// ErrCodeGone. It changes nothing, so a link previewer can fetch the page.
func (s *Store) LoginCodeName(ctx context.Context, code string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT name FROM login_codes WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?`,
		HashToken(code), formatTS(s.Now())).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrCodeGone
	}
	return name, err
}

// RedeemLoginCode uses code once and creates its browser session, in one
// transaction. It returns the session token, which belongs in the cookie and
// nowhere else. Of two concurrent calls, the first wins and the second gets
// ErrCodeGone. If a session took the name after the code was made, it
// returns ErrNameTaken with ws.Name set and rolls back, so the code stays
// usable until it expires.
func (s *Store) RedeemLoginCode(ctx context.Context, code string) (token string, ws api.WebSession, err error) {
	token, err = NewToken(SessionTokenPrefix)
	if err != nil {
		return "", ws, err
	}
	id, err := newSessionID()
	if err != nil {
		return "", ws, err
	}
	now := s.Now()
	nowS := formatTS(now)
	codeHash := HashToken(code)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", ws, err
	}
	defer tx.Rollback()
	if err := deleteExpiredLogins(ctx, tx, nowS); err != nil {
		return "", ws, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE login_codes SET used_at = ? WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?`,
		nowS, codeHash, nowS)
	if err != nil {
		return "", ws, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return "", ws, err
	} else if n == 0 {
		return "", ws, ErrCodeGone
	}
	var machineID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT c.name, c.machine_id, m.name FROM login_codes c JOIN machines m ON m.id = c.machine_id WHERE c.code_hash = ?`,
		codeHash).Scan(&ws.Name, &machineID, &ws.Machine); err != nil {
		return "", ws, err
	}
	if err := nameTaken(ctx, tx, ws.Name); err != nil {
		return "", api.WebSession{Name: ws.Name}, err
	}
	ws.ID, ws.CreatedAt, ws.LastUsedAt, ws.ExpiresAt = id, now, now, now.Add(WebSessionTTL)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO web_sessions (id, token_hash, name, machine_id, created_at, last_used_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ws.ID, HashToken(token), ws.Name, machineID, nowS, nowS, formatTS(ws.ExpiresAt)); err != nil {
		return "", ws, err
	}
	if err := tx.Commit(); err != nil {
		return "", ws, err
	}
	return token, ws, nil
}

// webSessionCols is what scanWebSession reads, joined with machines as m.
const webSessionCols = `w.id, w.name, m.name, w.created_at, w.last_used_at, w.expires_at`

func scanWebSession(sc interface{ Scan(...any) error }) (api.WebSession, error) {
	var ws api.WebSession
	var created, last, exp string
	if err := sc.Scan(&ws.ID, &ws.Name, &ws.Machine, &created, &last, &exp); err != nil {
		return ws, err
	}
	var err error
	if ws.CreatedAt, err = parseTS(created); err != nil {
		return ws, err
	}
	if ws.LastUsedAt, err = parseTS(last); err != nil {
		return ws, err
	}
	ws.ExpiresAt, err = parseTS(exp)
	return ws, err
}

// WebSessionByToken returns the live browser session for token, or
// ErrNotFound for an unknown or expired one. It looks the session up by the
// token's SHA-256 hash; timing on a hash lookup reveals nothing usable about
// the token. When last_used_at is more than WebSessionTouchEvery old, it
// slides the expiry to WebSessionTTL from now and reports touched, so the
// caller sends the cookie again.
func (s *Store) WebSessionByToken(ctx context.Context, token string) (ws api.WebSession, touched bool, err error) {
	now := s.Now()
	ws, err = scanWebSession(s.db.QueryRowContext(ctx,
		`SELECT `+webSessionCols+` FROM web_sessions w JOIN machines m ON m.id = w.machine_id
		WHERE w.token_hash = ? AND w.expires_at > ?`, HashToken(token), formatTS(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return api.WebSession{}, false, ErrNotFound
	}
	if err != nil {
		return api.WebSession{}, false, err
	}
	if now.Sub(ws.LastUsedAt) <= WebSessionTouchEvery {
		return ws, false, nil
	}
	ws.LastUsedAt, ws.ExpiresAt = now, now.Add(WebSessionTTL)
	if _, err := s.db.ExecContext(ctx, `UPDATE web_sessions SET last_used_at = ?, expires_at = ? WHERE id = ?`,
		formatTS(ws.LastUsedAt), formatTS(ws.ExpiresAt), ws.ID); err != nil {
		return api.WebSession{}, false, err
	}
	return ws, true, nil
}

// ListWebSessions deletes expired codes and sessions, then returns every
// browser session ordered by name.
func (s *Store) ListWebSessions(ctx context.Context) ([]api.WebSession, error) {
	if err := deleteExpiredLogins(ctx, s.db, formatTS(s.Now())); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+webSessionCols+` FROM web_sessions w JOIN machines m ON m.id = w.machine_id ORDER BY w.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.WebSession{}
	for rows.Next() {
		ws, err := scanWebSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// RevokeWebSession deletes the session with id, which signs that browser
// out at its next request. It returns ErrNotFound for an unknown id.
func (s *Store) RevokeWebSession(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("browser session %q: %w", id, ErrNotFound)
	}
	return nil
}
```

- [ ] **Step 6: Run the store tests**

Run: `go test ./internal/store/ -count=1`
Expected: PASS. `go build ./...` fails in `internal/server` only if it used
`ReadTokenPrefix`; it does not, so `go build ./...` also passes.

- [ ] **Step 7: Document schema v5**

In `docs/server.md`, section **Database**, change the upgrade list in the first
paragraph from `...; version 4 adds `session_digests` and `sessions.last_prompt` and `last_prompt_at`)`
to `...; version 4 adds `session_digests` and `sessions.last_prompt` and `last_prompt_at`; version 5 adds `login_codes` and `web_sessions`)`.

Add this subsection after **Session digests**, before the closing
"The source is in" paragraph:

```markdown
### Web sign-in

Schema version 5 adds two tables. Both hold hashes only: the database never
holds a sign-in code or a session token.

- `login_codes`: one row per `sessionhub login` link. `code_hash` (SHA-256 hex of
  the code), the session `name`, the creating `machine_id`, `created_at`,
  `expires_at` (10 minutes later), and `used_at` (null until used).
- `web_sessions`: one row per signed-in browser. `id` (8 random bytes, hex,
  public), `token_hash` (SHA-256 hex of the cookie token, unique), `name`
  (unique), `machine_id`, `created_at`, `last_used_at`, and `expires_at`
  (30 days after `last_used_at`).

Expired codes and sessions are deleted whenever a code or session is created
and whenever sessions are listed. Removing a machine deletes the codes and
sessions it created.

To roll back to the v4 binary, restore the pre-deploy backup, or run
`PRAGMA user_version=4`. The v4 binary ignores the two tables, but it needs
`read_token` in `server.toml` again. Run `PRAGMA user_version=5` before you
upgrade again.
```

- [ ] **Step 8: Run the full suite and lint**

Run: `make test lint`
Expected: every package `ok`, `gofmt -l` prints nothing, `go vet` clean.

- [ ] **Step 9: Commit**

```bash
git add internal/api/types.go internal/store/store.go internal/store/machines.go internal/store/weblogin.go internal/store/weblogin_test.go internal/store/concurrency_test.go internal/store/store_test.go docs/server.md
git commit -m "Store sign-in codes and browser sessions (schema v5)" -m "Only SHA-256 hashes are stored. Expired rows are deleted before any name check, so an expired session never holds its name. A redeem that finds the name taken rolls back and leaves the code usable.

Refs: docs/dev/superpowers/plans/2026-10-01-web-sign-in.md, Task 1

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h"
```

---

### Task 2: Server: the session cookie replaces the read token

**Files:**
- Create: `internal/server/cookie.go`
- Modify: `internal/server/server.go` (`Server`, `New`, `principal`,
  `authenticate`, `reader`, `writer`, `actor`, `auth`, `storeError`)
- Modify: `internal/server/routes.go` (access comments, `actor` call)
- Modify: `internal/server/handlers.go` (`listSessions` header)
- Modify: `internal/server/control.go` (`postRemoteControl`)
- Modify: `internal/server/dashboard.go` (drop `?token=` and `hub_read`,
  signed-out page)
- Modify: `internal/server/config.go`, `internal/server/run.go`
- Modify tests: `internal/server/helpers_test.go`, `auth_test.go`,
  `dashboard_test.go`, `sessions_test.go`, `run_test.go`, `config_test.go`
- Create: `internal/server/websession_test.go`
- Modify: `internal/hooks/realserver_test.go`, `internal/plugin/control_test.go`,
  `internal/resume/remote_test.go` (their `server.New` calls pass a URL)
- Modify: `docs/server.md` (run, configuration, authentication, endpoints,
  dashboard)

**Interfaces:**
- Consumes: from Task 1, `store.WebSessionByToken`, `store.CreateLoginCode`,
  `store.RedeemLoginCode`, `store.RevokeWebSession`, `store.WebSessionTTL`,
  `store.ErrNameTaken`, `store.ErrCodeGone`, `api.WebSession`,
  `api.HeaderActionSignOut`, `api.HeaderSessionName`.
- Produces:
  - `func New(st *store.Store, publicURL string, logger *log.Logger) *Server`
  - `principal{machine store.Machine; web *api.WebSession}`
  - `func (s *Server) authenticate(w http.ResponseWriter, r *http.Request,
    allowCookie bool) (principal, bool, error)`
  - `func (s *Server) actor(action string, h authedHandler) http.Handler`
  - `const sessionCookie = "hub_session"`, `setSessionCookie(w, token)`,
    `clearSessionCookie(w)`
  - `Config.LegacyReadToken bool` (replaces `Config.ReadToken`)
  - Test helpers: `testPublicURL`, `env.web`, `env.webID`,
    `env.loginCode(name) string`, `env.webSession(name) (token, id string)`,
    `sessionCookieFrom(h http.Header) *http.Cookie`.

- [ ] **Step 1: Update the test helpers**

In `internal/server/helpers_test.go`:

Replace the `testReadToken` constant with:

```go
// testPublicURL is the env server's public_url: login links start with it,
// and POST /login/{code} must carry it as Origin.
const testPublicURL = "https://sessionhub.example.test"
```

Add two fields to `env`, after `tokB`:

```go
	web    string // hub_session cookie of the browser session "phone", made by tower
	webID  string // that session's ID
```

In `newEnv`, replace `s := New(st, testReadToken, log.New(logBuf, "", 0))` with
`s := New(st, testPublicURL, log.New(logBuf, "", 0))`, and replace the final
`return &env{...}` with:

```go
	e := &env{t: t, st: st, clock: clock, srv: srv, server: s, log: logBuf, tokA: tokA, tokB: tokB}
	e.web, e.webID = e.webSession("phone")
	return e
```

In `doHdr`, replace `req.AddCookie(&http.Cookie{Name: readCookie, Value: cookie})`
with `req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})`, and
change the `doWith` comment to `// doWith is do plus an optional hub_session cookie value.`

Replace `tap`:

```go
// tap sends the dashboard's Remote Control write: the session cookie and
// X-Hub-Action.
func (e *env) tap(id string) (int, api.ControlRequest, []byte) {
	e.t.Helper()
	code, b, _ := e.doHdr("POST", "/v1/sessions/"+id+"/remote-control", "", e.web,
		map[string]string{api.HeaderAction: api.HeaderActionRemoteControl}, nil)
	var r api.ControlRequest
	json.Unmarshal(b, &r)
	return code, r, b
}
```

Add after `machine`:

```go
// loginCode creates a sign-in code for name as tower's `sessionhub login` would.
func (e *env) loginCode(name string) string {
	e.t.Helper()
	code, _, err := e.st.CreateLoginCode(context.Background(), e.machine(e.tokA).ID, name)
	if err != nil {
		e.t.Fatal(err)
	}
	return code
}

// webSession signs a browser in as name through the store and returns its
// cookie token and session ID.
func (e *env) webSession(name string) (token, id string) {
	e.t.Helper()
	tok, ws, err := e.st.RedeemLoginCode(context.Background(), e.loginCode(name))
	if err != nil {
		e.t.Fatal(err)
	}
	return tok, ws.ID
}
```

- [ ] **Step 2: Write the failing cookie tests**

Create `internal/server/websession_test.go`:

```go
package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// sessionCookieFrom returns the hub_session cookie a response sets, or nil.
func sessionCookieFrom(h http.Header) *http.Cookie {
	for _, c := range (&http.Response{Header: h}).Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

func TestSessionCookieReadsAndNamesTheBrowser(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	code, body, h := e.doWith("GET", "/v1/sessions", "", e.web, nil)
	if code != 200 || !strings.Contains(string(body), sid1) {
		t.Fatalf("GET /v1/sessions with cookie: %d %s", code, body)
	}
	if got := h.Get(api.HeaderSessionName); got != "phone" {
		t.Errorf("%s = %q, want phone", api.HeaderSessionName, got)
	}
	if c := sessionCookieFrom(h); c != nil {
		t.Errorf("a fresh session re-sent its cookie: %+v", c)
	}
	if _, _, h := e.do("GET", "/v1/sessions", e.tokA, nil); h.Get(api.HeaderSessionName) != "" {
		t.Errorf("machine token got %s %q", api.HeaderSessionName, h.Get(api.HeaderSessionName))
	}
	before := e.snapshot()
	if code, _, _ := e.doWith("POST", "/v1/sessions", "", e.web, api.SessionUpsert{ID: sid2, Source: api.SourceHooks, Agent: "claude"}); code != http.StatusUnauthorized {
		t.Errorf("POST /v1/sessions with cookie: %d, want 401", code)
	}
	if e.snapshot() != before {
		t.Error("a refused cookie write changed state")
	}
	if strings.Contains(e.log.String(), e.web) {
		t.Error("session token in the request log")
	}
}

func TestSessionCookieSlidingExpiry(t *testing.T) {
	e := newEnv(t)
	e.clock.Advance(30 * time.Minute)
	if code, _, h := e.doWith("GET", "/v1/sessions", "", e.web, nil); code != 200 || sessionCookieFrom(h) != nil {
		t.Fatalf("at +30m: %d, cookie %v; want 200 and no cookie", code, sessionCookieFrom(h))
	}
	e.clock.Advance(31 * time.Minute)
	code, _, h := e.doWith("GET", "/v1/sessions", "", e.web, nil)
	c := sessionCookieFrom(h)
	if code != 200 || c == nil {
		t.Fatalf("at +61m: %d, cookie %v; want 200 and a re-sent cookie", code, c)
	}
	if c.Value != e.web || c.MaxAge != 30*24*3600 || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Errorf("re-sent cookie attributes: %+v", c)
	}
	// 30 days after the slide, the session has expired.
	e.clock.Advance(30 * 24 * time.Hour)
	if code, _, _ := e.doWith("GET", "/v1/sessions", "", e.web, nil); code != http.StatusUnauthorized {
		t.Errorf("30 days after the last slide: %d, want 401", code)
	}
}

func TestSessionCookieRevokedOrMachineRemoved(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.st.RevokeWebSession(ctx, e.webID); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := e.doWith("GET", "/v1/sessions", "", e.web, nil); code != http.StatusUnauthorized {
		t.Errorf("revoked session: %d, want 401", code)
	}
	tok, _ := e.webSession("tablet")
	if err := e.st.RemoveMachine(ctx, "tower"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := e.doWith("GET", "/v1/sessions", "", tok, nil); code != http.StatusUnauthorized {
		t.Errorf("session of a removed machine: %d, want 401", code)
	}
}

func TestOldReadCookieIsSignedOut(t *testing.T) {
	e := newEnv(t)
	old := map[string]string{"Cookie": "hub_read=hub_r_test-read-token-0123456789abcdefghijklmnop"}
	code, body, h := e.doHdr("GET", "/", "", "", old, nil)
	if code != http.StatusUnauthorized || !strings.HasPrefix(h.Get("Content-Type"), "text/html") || !strings.Contains(string(body), "sessionhub login") {
		t.Errorf("GET / with the old hub_read cookie: %d %q %s", code, h.Get("Content-Type"), body)
	}
	if code, _, _ := e.doHdr("GET", "/v1/sessions", "", "", old, nil); code != http.StatusUnauthorized {
		t.Errorf("GET /v1/sessions with the old hub_read cookie: %d, want 401", code)
	}
	if code, _, _ := e.do("GET", "/v1/sessions", "hub_r_test-read-token-0123456789abcdefghijklmnop", nil); code != http.StatusUnauthorized {
		t.Errorf("bearer read token: %d, want 401", code)
	}
}

func TestDashboardIgnoresTokenParameter(t *testing.T) {
	e := newEnv(t)
	code, body, h := e.do("GET", "/?token=hub_r_test-read-token-0123456789abcdefghijklmnop", "", nil)
	if code != http.StatusUnauthorized || !strings.Contains(string(body), "sessionhub login") || len((&http.Response{Header: h}).Cookies()) != 0 {
		t.Errorf("?token= without a cookie: %d, cookies %v; want the signed-out page and no cookie", code, (&http.Response{Header: h}).Cookies())
	}
	if code, _, _ := e.doWith("GET", "/?token=junk", "", e.web, nil); code != 200 {
		t.Errorf("?token=junk with a valid cookie: %d, want 200", code)
	}
}
```

- [ ] **Step 3: Rewrite the auth matrix for the new credentials**

In `internal/server/auth_test.go`, in `TestAuthMatrix`, replace everything
from `tokens := []struct {` down to (not including) `failures := 0` with:

```go
	// Browser credentials get a fresh session for each request, because a
	// request may end its session (POST /logout).
	const newSession = "new session"
	tokens := []struct {
		name   string
		bearer string
		cookie string // hub_session value; newSession mints one per request
		action string // X-Hub-Action value to send; empty sends none
	}{
		{"none", "", "", ""},
		{"bad", "hub_m_not-a-real-token", "", ""},
		{"read token", "hub_r_test-read-token-0123456789abcdefghijklmnop", "", ""},
		{"machine", e.tokA, "", ""},
		{"machineB", e.tokB, "", ""},
		{"cookie", "", newSession, ""},
		{"cookie+action", "", newSession, api.HeaderActionRemoteControl},
		{"cookie+sign-out", "", newSession, api.HeaderActionSignOut},
		{"cookie+wrong action", "", newSession, "title"},
		{"badcookie", "", "hub_s_wrong", ""},
	}
	expect := func(rt route, c authCase, tk string) []int {
		switch {
		case rt.access == accessPublic:
		case tk == "none" || tk == "bad" || tk == "read token" || tk == "badcookie":
			return []int{http.StatusUnauthorized}
		case rt.access == accessWrite && strings.HasPrefix(tk, "cookie"):
			return []int{http.StatusUnauthorized} // the cookie never authorizes a machine route
		case rt.access == accessAction && (tk == "cookie" || tk == "cookie+sign-out" || tk == "cookie+wrong action"):
			return []int{http.StatusForbidden} // the cookie only with X-Hub-Action: remote-control
		case tk == "machineB" && c.okB != 0:
			return []int{c.okB}
		}
		if c.alsoOK != 0 {
			return []int{c.ok, c.alsoOK}
		}
		return []int{c.ok}
	}
	minted := 0
```

In the request loop, replace:

```go
			bearer, cookie := tk.token, ""
			if tk.cookie {
				bearer, cookie = "", tk.token
			}
			before := e.snapshot()
			code, body, h := e.doHdr(rt.method, c.path, bearer, cookie, hdr, c.body)
```

with:

```go
			cookie := tk.cookie
			if cookie == newSession {
				minted++
				cookie, _ = e.webSession(fmt.Sprintf("matrix%d", minted))
			}
			before := e.snapshot()
			code, body, h := e.doHdr(rt.method, c.path, tk.bearer, cookie, hdr, c.body)
```

Change the `accessPage` body check from `!strings.Contains(string(body), "?token=")`
to `!strings.Contains(string(body), "sessionhub login")` and its message from
`"GET / with %s: 401 must be an HTML page naming ?token=; got %q %s"` to
`"GET / with %s: 401 must be an HTML page naming sessionhub login; got %q %s"`.
Add `"fmt"` to the imports.

Replace `TestReadTokenCannotBeUsedAsMachine`'s first loop:

```go
	for _, tok := range []string{strings.ToUpper(e.tokA), e.tokA[:len(e.tokA)-1], e.tokA + "x", testReadToken + "x"} {
```

with:

```go
	for _, tok := range []string{strings.ToUpper(e.tokA), e.tokA[:len(e.tokA)-1], e.tokA + "x", "hub_r_" + e.tokA[len("hub_m_"):], e.web} {
```

and rename the test `TestTokenVariantsRejected`.

Replace `TestMachineLastSeenAndList` (newEnv's browser session already marked
tower seen at the start time):

```go
func TestMachineLastSeenAndList(t *testing.T) {
	e := newEnv(t)
	start := e.clock.Now()
	e.clock.Advance(3e9)
	var ms []api.Machine
	code, b, _ := e.doWith("GET", "/v1/machines", "", e.web, nil)
	if code != 200 {
		t.Fatalf("GET /v1/machines with cookie: %d %s", code, b)
	}
	json.Unmarshal(b, &ms)
	if len(ms) != 2 || ms[0].Name != "bluebox" || ms[1].Name != "tower" {
		t.Fatalf("machines: %+v", ms)
	}
	if ms[0].LastSeen != nil || ms[1].LastSeen == nil || !ms[1].LastSeen.Equal(start) {
		t.Errorf("the session cookie must not mark machines seen: %+v", ms)
	}
	e.clock.Advance(3e9)
	e.must(200, "GET", "/v1/machines", e.tokA, nil, &ms)
	if ms[1].LastSeen == nil || !ms[1].LastSeen.Equal(e.clock.Now()) {
		t.Errorf("tower last_seen = %v, want %v", ms[1].LastSeen, e.clock.Now())
	}
	if ms[0].LastSeen != nil {
		t.Errorf("bluebox was never used but has last_seen %v", ms[0].LastSeen)
	}
	if ms[1].SSHHost != "tower.example.com" || ms[1].HerdrHost != "tower.example.com" || ms[0].SSHHost != "bluebox.example.com" {
		t.Errorf("hosts: %+v", ms)
	}
}
```

- [ ] **Step 4: Update the remaining tests that used the read token**

`internal/server/dashboard_test.go`:
- Delete `TestDashboardCookieExchange` and `TestDashboardBadToken`. Keep
  `noRedirect`; Task 3 uses it. Delete the `"net/url"` and `"io"` imports,
  which only those two tests used.
- In `TestDashboardHTMLAndCSP`, change
  `e.doWith("GET", "/", "", testReadToken, nil)` to
  `e.doWith("GET", "/", "", e.web, nil)`.
- In `TestDashboardWithoutCredentials`, change
  `!strings.Contains(string(body), "?token=")` to
  `!strings.Contains(string(body), "sessionhub login")`.

`internal/server/sessions_test.go`:
- In the prefix table test, change
  `e.do("GET", "/v1/sessions/"+tt.id, testReadToken, nil)` to
  `e.do("GET", "/v1/sessions/"+tt.id, e.tokA, nil)`.
- In `TestRequestLog`, change `e.do("GET", "/v1/sessions", testReadToken, nil)`
  to `e.doWith("GET", "/v1/sessions", "", e.web, nil)` and
  `"machine=read"` to `"machine=web:phone"`.

`internal/server/config_test.go`, in `TestLoadConfig`:
- `"file values"`: replace `ReadToken: "hub_r_fromfile",` with
  `LegacyReadToken: true,`.
- `"env overrides file"`: replace `ReadToken: "hub_r_env",` with
  `LegacyReadToken: true,`.
- Add a case after it:

```go
		{"SESSIONHUB_READ_TOKEN alone", "", map[string]string{"SESSIONHUB_READ_TOKEN": "hub_r_env"},
			Config{Listen: []string{"127.0.0.1:8787"}, PublicURL: "https://sessionhub.example.com", StaleAfter: 5 * time.Minute,
				LegacyReadToken: true}},
```

`internal/server/run_test.go`:
- Replace `TestRunRefusesWithoutReadToken` with:

```go
// waitUp polls addr's /healthz until it answers, and fails if run returns
// first.
func waitUp(t *testing.T, addr string, done <-chan error) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("run returned early: %v", err)
		default:
		}
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			resp.Body.Close()
			return
		}
	}
	t.Fatalf("%s never came up", addr)
}

// TestRunWarnsAboutReadToken: read_token and SESSIONHUB_READ_TOKEN are ignored, with
// one warning, and the server starts with or without them.
func TestRunWarnsAboutReadToken(t *testing.T) {
	tests := []struct {
		name     string
		toml     string
		env      string
		warnings int
	}{
		{"neither", "", "", 0},
		{"file", "read_token = \"hub_r_old\"\n", "", 1},
		{"env", "", "hub_r_env", 1},
		{"both", "read_token = \"hub_r_old\"\n", "hub_r_env", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configEnv(t, tt.toml)
			t.Setenv("SESSIONHUB_READ_TOKEN", tt.env)
			ln := listenLocal(t)
			ctx, cancel := context.WithCancel(context.Background())
			var logs syncBuffer
			done := make(chan error, 1)
			go func() { done <- runWithListeners(ctx, nil, &logs, []net.Listener{ln}) }()
			waitUp(t, ln.Addr().String(), done)
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("run: %v", err)
			}
			if n := strings.Count(logs.String(), "warning: read_token"); n != tt.warnings {
				t.Errorf("%d read_token warnings, want %d:\n%s", n, tt.warnings, logs.String())
			}
		})
	}
}
```

- In `TestRunRejectsArgsAndBadListen` and `TestRunServesAllListenersAndStops`,
  delete `t.Setenv("SESSIONHUB_READ_TOKEN", testReadToken)`.
- In `TestRunServesAllListenersAndStops`, replace the `/v1/sessions` request
  block (from `req, _ := http.NewRequest("GET", "http://"+a1+"/v1/sessions", nil)`
  through its status check) with:

```go
	resp, err := http.Get("http://" + a1 + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/v1/sessions without credentials: %d, want 401", resp.StatusCode)
	}
```

  and in its log check change the wants to
  `"listening on " + a1, "listening on " + a2, `GET "/v1/sessions" 401`, "machine=-", "shutting down"`,
  then add after that loop:

```go
	if strings.Contains(out, "warning:") {
		t.Errorf("warning without read_token:\n%s", out)
	}
```

  If the compiler then reports `"bytes" imported and not used`, delete that
  import.

Other packages start a real server in their tests with a read token as the
second argument of `server.New`. The signature keeps its types, so they still
compile, but the argument is now the public URL. Change each to
`"https://sessionhub.example.test"`:

- `internal/hooks/realserver_test.go`: `server.New(st, "hub_r_test", log.New(io.Discard, "", 0))`
  becomes `server.New(st, "https://sessionhub.example.test", log.New(io.Discard, "", 0))`.
- `internal/plugin/control_test.go`: `server.New(st, "hub_r_watcher-test-read-token-0123456789", nil)`
  becomes `server.New(st, "https://sessionhub.example.test", nil)`.
- `internal/resume/remote_test.go`: `server.New(st, "hub_r_resume-test-read-token-0123456789", nil)`
  becomes `server.New(st, "https://sessionhub.example.test", nil)`.

`internal/join/join_test.go` writes `read_token` into a test `server.toml`;
leave it. The key still loads, and the server only warns.

- [ ] **Step 5: Run the server tests to verify they fail**

Run: `go test ./internal/server/ -count=1`
Expected: FAIL to build with `undefined: sessionCookie` and
`unknown field LegacyReadToken in struct literal`.

- [ ] **Step 6: Add the cookie helpers**

Create `internal/server/cookie.go`:

```go
package server

import (
	"net/http"
	"time"

	"github.com/abdallah/session-hub/internal/store"
)

// sessionCookie carries a browser session token. SameSite=Lax, not Strict,
// so opening the dashboard from a link in another app sends it. Writes stay
// protected by X-Hub-Action, which needs a CORS preflight the server never
// grants.
const sessionCookie = "hub_session"

// setSessionCookie sets the session cookie with a Max-Age of the session
// lifetime. It is the only place a session token leaves the server.
func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(store.WebSessionTTL / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie tells the browser to drop the session cookie.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}
```

- [ ] **Step 7: Rework authentication in `internal/server/server.go`**

Replace the `Server` struct, `New`, and `principal`:

```go
// Server serves the sessionhub HTTP API.
type Server struct {
	store *store.Store
	// publicURL is public_url without a trailing slash: the base of login
	// links and the only Origin POST /login/{code} accepts.
	publicURL string
	log       *log.Logger
	control   *controlHub
	// after is the long poll's timer. Tests replace it.
	after func(time.Duration) <-chan time.Time
}

// New returns a Server. publicURL is the server's public_url.
func New(st *store.Store, publicURL string, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Server{store: st, publicURL: strings.TrimRight(publicURL, "/"), log: logger, control: newControlHub(), after: time.After}
}

// principal is who made a request: a machine token or a browser session.
type principal struct {
	machine store.Machine    // set for a machine token
	web     *api.WebSession  // set for the hub_session cookie
}
```

Change the `logRequests` comment's `the machine (or "read" for the read token, "-" for none)`
to `the caller (a machine name, "web:<name>" for a browser session, "-" for none)`.

Replace `authenticate`, `reader`, `writer`, `actor`, and `auth`:

```go
// authenticate resolves the caller. A bearer token matches machine tokens
// only. When allowCookie is set and there is no bearer header, the
// hub_session cookie counts if it names a live browser session; when the
// store slides that session's expiry, the cookie goes out again with a fresh
// Max-Age. ok is false when nothing matches; err is set only for a database
// failure.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, allowCookie bool) (p principal, ok bool, err error) {
	tok := bearer(r)
	if tok == "" && allowCookie {
		c, cerr := r.Cookie(sessionCookie)
		if cerr != nil || c.Value == "" {
			return p, false, nil
		}
		ws, touched, err := s.store.WebSessionByToken(r.Context(), c.Value)
		if errors.Is(err, store.ErrNotFound) {
			return p, false, nil
		}
		if err != nil {
			return p, false, err
		}
		if touched {
			setSessionCookie(w, c.Value)
		}
		setWho(r, "web:"+ws.Name)
		return principal{web: &ws}, true, nil
	}
	if tok == "" {
		return p, false, nil
	}
	m, err := s.store.MachineByToken(r.Context(), tok)
	if errors.Is(err, store.ErrNotFound) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	setWho(r, m.Name)
	return principal{machine: m}, true, nil
}

type authedHandler func(w http.ResponseWriter, r *http.Request, p principal)

// reader accepts a machine token or the session cookie.
func (s *Server) reader(h authedHandler) http.Handler {
	return s.auth(false, h)
}

// writer accepts only a machine token.
func (s *Server) writer(h authedHandler) http.Handler {
	return s.auth(true, h)
}

// actor accepts a machine token, or the session cookie with the header
// X-Hub-Action: action. The custom header makes a cross-site request need a
// CORS preflight, which the server never grants. The cookie without the
// header, or with another value, gets 403.
func (s *Server) actor(action string, h authedHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok, err := s.authenticate(w, r, true)
		switch {
		case err != nil:
			s.internalError(w, err)
			return
		case !ok:
			w.Header().Set("WWW-Authenticate", `Bearer realm="sessionhub"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		case p.web != nil && r.Header.Get(api.HeaderAction) != action:
			writeError(w, http.StatusForbidden, "the dashboard must send "+api.HeaderAction+": "+action+" with this request")
			return
		}
		h(w, r, p)
	})
}

func (s *Server) auth(write bool, h authedHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok, err := s.authenticate(w, r, !write)
		switch {
		case err != nil:
			s.internalError(w, err)
			return
		case !ok:
			w.Header().Set("WWW-Authenticate", `Bearer realm="sessionhub"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		h(w, r, p)
	})
}
```

Remove the now-unused `"crypto/subtle"` import. In `storeError`, add before
`case errors.Is(err, store.ErrWrongMachine):`:

```go
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrCodeGone):
		writeError(w, http.StatusGone, err.Error())
```

- [ ] **Step 8: Update routes, handlers, and control**

In `internal/server/routes.go`, change the access comments:

```go
	accessRead   = "read"   // machine token or session cookie
	accessPage   = "page"   // the dashboard: read credentials, else an HTML 401
	accessWrite  = "write"  // machine token only
	accessAction = "action" // machine token, or the session cookie with X-Hub-Action: remote-control
```

and the remote-control route to
`{"POST", "/v1/sessions/{id}/remote-control", accessAction, s.actor(api.HeaderActionRemoteControl, s.postRemoteControl)},`.
Add `"github.com/abdallah/session-hub/internal/api"` to its imports.

In `internal/server/handlers.go`, change `listSessions`'s signature to take
`p principal` and add before `writeJSON(w, http.StatusOK, list)`:

```go
	if p.web != nil {
		w.Header().Set(api.HeaderSessionName, p.web.Name)
	}
```

In `internal/server/control.go`, in `postRemoteControl`, change
`if !p.read {` to `if p.web == nil {`.

- [ ] **Step 9: Replace the dashboard's token exchange**

In `internal/server/dashboard.go`, delete `readCookie`, `cookieMaxAge`, the
`"crypto/subtle"` and `"time"` imports, and `unauthorizedPage`. Add:

```go
const signedOutPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>sessionhub: signed out</title></head>
<body>
<h1>sessionhub</h1>
<p>You are signed out. Run <code>sessionhub login --name &lt;device&gt;</code> on a machine with sessionhub, then open the link it prints.</p>
</body></html>
`
```

Replace `dashboard` and `unauthorizedPage` (the method) with:

```go
// dashboard serves GET / to a caller with read credentials, and the
// signed-out page (401) to anyone else. A ?token= parameter is ignored.
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")

	_, ok, err := s.authenticate(w, r, true)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !ok {
		s.signedOutPage(w)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", dashboardCSP)
	w.Write(dashboardHTML)
}

func (s *Server) signedOutPage(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(signedOutPage))
}
```

- [ ] **Step 10: Ignore `read_token` with one warning**

In `internal/server/config.go`, replace `ReadToken  string` in `Config` with:

```go
	// LegacyReadToken is set when server.toml has read_token or
	// SESSIONHUB_READ_TOKEN is set. Both are ignored; Run logs one warning.
	LegacyReadToken bool
```

In `fileConfig`, change the `ReadToken` line's comment:

```go
	ReadToken  string   `toml:"read_token"` // ignored; kept so an old file still loads
```

Replace `cfg.ReadToken = fc.ReadToken` with
`cfg.LegacyReadToken = fc.ReadToken != ""`, and replace the
`SESSIONHUB_READ_TOKEN` block with:

```go
	if os.Getenv("SESSIONHUB_READ_TOKEN") != "" {
		cfg.LegacyReadToken = true
	}
```

Change the `LoadConfig` comment's last sentence
`It does not require read_token; Run does.` to
`read_token is read only to warn about it.`

In `internal/server/run.go`, delete the `if cfg.ReadToken == "" { ... }` block,
and replace:

```go
	logger := log.New(stderr, "", log.LstdFlags)
	sessionhub := New(st, cfg.ReadToken, logger)
```

with:

```go
	logger := log.New(stderr, "", log.LstdFlags)
	if cfg.LegacyReadToken {
		logger.Printf("warning: read_token (server.toml) and SESSIONHUB_READ_TOKEN are ignored: browsers sign in with `sessionhub login`. Delete the setting once you no longer need to roll back.")
	}
	sessionhub := New(st, cfg.PublicURL, logger)
```

- [ ] **Step 11: Run the server tests**

Run: `go test ./internal/server/ -count=1`
Expected: PASS, including `TestAuthMatrix` (`0 failures` in its log line),
`TestOldReadCookieIsSignedOut`, and `TestRunWarnsAboutReadToken`.

- [ ] **Step 12: Update `docs/server.md`**

- **Run the server**: change `` `read` for the read token, or `-` for no valid token `` to
  `` `web:<name>` for a browser session, or `-` for no valid credentials ``.
  Replace `If `read_token` is not set, the server refuses to start.` with
  `If `server.toml` sets `read_token` or the environment sets `SESSIONHUB_READ_TOKEN`, the server logs one warning at startup and ignores the value.`
- **Configuration** table: replace the `read_token` row with
  ``| `read_token` | none | Ignored. The server logs a warning when it is set. Delete it once you no longer need to roll back to a release before web sign-in. |``.
  Delete `read_token = "hub_r_..."` from the example, and delete the
  paragraph "To generate a read token..." with its code block.
- **Environment overrides**: replace the `SESSIONHUB_READ_TOKEN` row with
  ``| `SESSIONHUB_READ_TOKEN` | Nothing. Ignored, with a warning at startup. |``.
- **Authentication**: replace everything from `Send `Authorization: Bearer <token>`.`
  up to `The server compares tokens in constant time.` with:

```markdown
Machines send `Authorization: Bearer <token>`. Browsers send the
`hub_session` cookie.

- A machine token (`hub_m_...`) can read and write. Every write is attributed
  to that machine. A bearer token that is not a machine token, such as an old
  read token (`hub_r_...`), gets `401`.
- The `hub_session` cookie holds a browser session token (`hub_s_...`). It
  authorizes reads, `POST /v1/sessions/{id}/remote-control` with the header
  `X-Hub-Action: remote-control`, and `POST /logout` with
  `X-Hub-Action: sign-out`. Nothing else: the cookie never reaches
  `/v1/logins` or `/v1/web-sessions`, and with any other write it gets `401`.
  A cookie write without the right header gets `403`. The header makes a
  cross-site request need a CORS preflight, which the server never grants.
- An unknown, revoked, or expired session counts as no credentials: `401`.
- A session lasts 30 days from its last use. When a request finds
  `last_used_at` more than one hour old, the server moves `last_used_at` to
  now and `expires_at` 30 days later, and sends the cookie again with a fresh
  `Max-Age`.
- `GET /v1/sessions` called with the cookie carries the header
  `X-Hub-Session-Name` with the session's name.
```

  Keep the constant-time sentence, but change it to
  `The server compares machine tokens in constant time. It looks up a session by its token's SHA-256 hash. Each request with a machine token updates that machine's `last_seen`.`
- **Endpoints**: change the `GET /` row to
  ``| `GET /` | read (bearer or cookie) | `200` HTML; `401` signed-out HTML otherwise | The dashboard. A `?token=` parameter is ignored. |``,
  the remote-control row's auth to `machine, or cookie with `X-Hub-Action: remote-control``,
  and `"read" means a machine token or the read token.` to
  `"read" means a machine token or the session cookie.`
- **Dashboard**: replace the block from `To sign in on a device, open the link once:`
  through `To revoke every browser, change `read_token` and restart.` with:

```markdown
A browser signs in with a one-time link from `sessionhub login`; see
[Sign in a browser](#sign-in-a-browser). `GET /` without a valid session
returns a small HTML `401` page that says to run
`sessionhub login --name <device>`. A `?token=` parameter is ignored and never
compared.
```

- [ ] **Step 13: Run the full suite and lint**

Run: `make test lint`
Expected: every package `ok`; `gofmt -l` and `go vet` clean.

- [ ] **Step 14: Commit**

```bash
git add internal/server/ internal/hooks/realserver_test.go internal/plugin/control_test.go internal/resume/remote_test.go docs/server.md
git commit -m "Authenticate browsers with a session cookie instead of the read token" -m "The hub_session cookie names a row in web_sessions and slides its 30-day expiry at most once an hour. read_token and SESSIONHUB_READ_TOKEN are ignored with one startup warning, and the dashboard ignores ?token=.

Refs: docs/dev/superpowers/plans/2026-10-01-web-sign-in.md, Task 2

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h"
```

---

### Task 3: Server: login links, login pages, the session API, and sign-out

**Files:**
- Create: `internal/server/login.go`
- Create: `internal/server/login_test.go`
- Modify: `internal/server/routes.go` (six routes, two access levels)
- Modify: `internal/server/server.go` (`browserAction`, log redaction)
- Modify: `internal/server/helpers_test.go` (`doHdr` stops following
  redirects, `submitLogin`)
- Modify: `internal/server/auth_test.go` (cases for the new routes)
- Modify: `docs/server.md` (endpoints, new section)

**Interfaces:**
- Consumes: from Task 1, `store.ValidLoginCode`, `store.CreateLoginCode`,
  `store.LoginCodeName`, `store.RedeemLoginCode`, `store.ListWebSessions`,
  `store.RevokeWebSession`, `store.ErrCodeGone`, `store.ErrNameTaken`,
  `api.LoginIn`, `api.Login`, `api.WebSessionList`. From Task 2,
  `s.actor(action, h)`, `setSessionCookie`, `clearSessionCookie`,
  `s.publicURL`, `env.loginCode`, `env.webSession`, `sessionCookieFrom`,
  `testPublicURL`.
- Produces: routes `POST /v1/logins` (`201 api.Login`),
  `GET /login/{code}`, `POST /login/{code}`, `GET /v1/web-sessions`
  (`200 api.WebSessionList`), `DELETE /v1/web-sessions/{id}` (`204`),
  `POST /logout` (`204`). Access levels `accessMachine = "machine"` and
  `accessSignOut = "sign-out"`. The client (Task 4) and the dashboard (Task 6)
  call these.

- [ ] **Step 1: Stop following redirects in test requests, add `submitLogin`**

In `internal/server/helpers_test.go`, in `doHdr`, change
`resp, err := http.DefaultClient.Do(req)` to `resp, err := noRedirect().Do(req)`.
Add after `webSession`:

```go
// submitLogin presses Sign in: POST /login/{code} with Origin set to origin,
// or with no Origin when origin is "".
func (e *env) submitLogin(code, origin string) (int, []byte, http.Header) {
	e.t.Helper()
	var hdr map[string]string
	if origin != "" {
		hdr = map[string]string{"Origin": origin}
	}
	return e.doHdr("POST", "/login/"+code, "", "", hdr, nil)
}
```

- [ ] **Step 2: Write the failing login tests**

Create `internal/server/login_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

func TestCreateLogin(t *testing.T) {
	e := newEnv(t)
	var l api.Login
	e.must(http.StatusCreated, "POST", "/v1/logins", e.tokA, api.LoginIn{Name: "tablet"}, &l)
	if !regexp.MustCompile(`^` + regexp.QuoteMeta(testPublicURL) + `/login/[a-z2-7]{26}$`).MatchString(l.URL) ||
		l.Name != "tablet" || !l.ExpiresAt.Equal(e.clock.Now().Add(10*time.Minute)) {
		t.Errorf("login %+v", l)
	}
	for _, tc := range []struct {
		name string
		want int
		msg  string
	}{
		{"bad name", http.StatusBadRequest, "name"},
		{"phone", http.StatusConflict, `browser session named "phone"`}, // newEnv signed in "phone"
	} {
		code, b, _ := e.do("POST", "/v1/logins", e.tokA, api.LoginIn{Name: tc.name})
		if code != tc.want || !strings.Contains(string(b), tc.msg) {
			t.Errorf("name %q: %d %s, want %d with %q", tc.name, code, b, tc.want, tc.msg)
		}
	}
	for range 4 {
		e.must(http.StatusCreated, "POST", "/v1/logins", e.tokB, api.LoginIn{Name: "tablet"}, nil)
	}
	if code, b, _ := e.do("POST", "/v1/logins", e.tokA, api.LoginIn{Name: "tablet"}); code != http.StatusTooManyRequests {
		t.Errorf("sixth unused link: %d %s, want 429", code, b)
	}
}

func TestLoginPageChangesNothing(t *testing.T) {
	e := newEnv(t)
	code := e.loginCode("tablet")
	for i := range 2 {
		status, body, h := e.do("GET", "/login/"+code, "", nil)
		page := string(body)
		if status != 200 || !strings.Contains(page, "Sign in this browser as <strong>tablet</strong>?") ||
			!strings.Contains(page, `<form method="post">`) || !strings.Contains(page, "Sign in</button>") {
			t.Fatalf("GET %d: %d %s", i, status, page)
		}
		if got := h.Get("Content-Security-Policy"); got != loginCSP {
			t.Errorf("CSP %q, want %q", got, loginCSP)
		}
		for _, want := range []string{"default-src 'none'", "style-src 'sha256-", "form-action 'self'", "base-uri 'none'", "frame-ancestors 'none'"} {
			if !strings.Contains(loginCSP, want) {
				t.Errorf("CSP lacks %q", want)
			}
		}
		// no-referrer would make browsers send Origin: null on the form POST.
		if got := h.Get("Referrer-Policy"); got != "same-origin" {
			t.Errorf("Referrer-Policy %q, want same-origin", got)
		}
		if h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("headers %v", h)
		}
		if strings.Contains(page, "<script") {
			t.Error("login page has a script")
		}
	}
	// The style hash in the CSP matches the page's one inline style.
	_, body, _ := e.do("GET", "/login/"+code, "", nil)
	if got := buildLoginCSP(regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindSubmatch(body)[1]); got != loginCSP {
		t.Errorf("CSP from the served style %q, want %q", got, loginCSP)
	}
	if list, _ := e.st.ListWebSessions(context.Background()); len(list) != 1 {
		t.Errorf("GET created a session: %d sessions", len(list))
	}
	if status, _, _ := e.submitLogin(code, testPublicURL); status != http.StatusSeeOther {
		t.Errorf("POST after two GETs: %d, want 303", status)
	}
	for _, bad := range []string{code, "short", strings.ToUpper(code), "aaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		status, body, _ := e.do("GET", "/login/"+bad, "", nil)
		if status != http.StatusGone || !strings.Contains(string(body), "This link expired or was already used.") ||
			!strings.Contains(string(body), "<code>sessionhub login</code>") {
			t.Errorf("GET /login/%s: %d %s, want the 410 page", bad, status, body)
		}
	}
}

func TestLoginSubmit(t *testing.T) {
	e := newEnv(t)
	code := e.loginCode("tablet")
	status, body, h := e.submitLogin(code, testPublicURL)
	if status != http.StatusSeeOther || h.Get("Location") != "/" {
		t.Fatalf("POST: %d, Location %q; want 303 to /", status, h.Get("Location"))
	}
	c := sessionCookieFrom(h)
	if c == nil || !strings.HasPrefix(c.Value, store.SessionTokenPrefix) || !c.HttpOnly || !c.Secure ||
		c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != 30*24*3600 {
		t.Fatalf("cookie %+v", c)
	}
	if strings.Contains(string(body), c.Value) {
		t.Error("session token in the response body")
	}
	gs, _, gh := e.doWith("GET", "/v1/sessions", "", c.Value, nil)
	if gs != 200 || gh.Get(api.HeaderSessionName) != "tablet" {
		t.Errorf("the new cookie: %d, name %q", gs, gh.Get(api.HeaderSessionName))
	}
	status, body, h = e.submitLogin(code, testPublicURL)
	if status != http.StatusGone || !strings.Contains(string(body), "expired or was already used") || sessionCookieFrom(h) != nil {
		t.Errorf("second press: %d %s", status, body)
	}
	if !strings.Contains(e.log.String(), "login refused") {
		t.Errorf("refused press not logged:\n%s", e.log.String())
	}
}

func TestLoginSubmitOrigin(t *testing.T) {
	e := newEnv(t)
	code := e.loginCode("tablet")
	for _, origin := range []string{"", "https://evil.example", testPublicURL + "/", "http://sessionhub.example.test", "null", "https://sessionhub.example.test:8443"} {
		status, body, h := e.submitLogin(code, origin)
		if status != http.StatusForbidden || !strings.HasPrefix(h.Get("Content-Type"), "text/html") || sessionCookieFrom(h) != nil {
			t.Errorf("Origin %q: %d %s, want the 403 page", origin, status, body)
		}
	}
	// The refused presses did not use the code.
	if status, _, _ := e.submitLogin(code, testPublicURL); status != http.StatusSeeOther {
		t.Errorf("right Origin after refusals: %d, want 303", status)
	}
	// Refusals are logged with Cf-Connecting-Ip when present.
	e.doHdr("POST", "/login/"+code, "", "", map[string]string{"Origin": "https://evil.example", "Cf-Connecting-Ip": "203.0.113.9"}, nil)
	if !strings.Contains(e.log.String(), `login refused from "203.0.113.9"`) {
		t.Errorf("log lacks the Cf-Connecting-Ip address:\n%s", e.log.String())
	}
	if !strings.Contains(e.log.String(), `login refused from "127.0.0.1"`) {
		t.Errorf("log lacks the remote address:\n%s", e.log.String())
	}
}

func TestLoginSubmitRace(t *testing.T) {
	e := newEnv(t)
	code := e.loginCode("tablet")
	start := make(chan struct{})
	codes := make(chan int, 2)
	for range 2 {
		go func() {
			<-start
			req, _ := http.NewRequest("POST", e.srv.URL+"/login/"+code, nil)
			req.Header.Set("Origin", testPublicURL)
			resp, err := noRedirect().Do(req)
			if err != nil {
				codes <- 0
				return
			}
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	close(start)
	got := []int{<-codes, <-codes}
	slices.Sort(got)
	if !slices.Equal(got, []int{http.StatusSeeOther, http.StatusGone}) {
		t.Errorf("two presses: %v, want one 303 and one 410", got)
	}
	list, _ := e.st.ListWebSessions(context.Background())
	if n := len(list); n != 2 { // phone and tablet
		t.Errorf("%d sessions, want 2", n)
	}
}

func TestLoginSubmitNameTaken(t *testing.T) {
	e := newEnv(t)
	code := e.loginCode("tablet")
	_, id := e.webSession("tablet")
	status, body, h := e.submitLogin(code, testPublicURL)
	if status != http.StatusConflict || !strings.Contains(string(body), "A browser session named tablet exists.") ||
		!strings.Contains(string(body), "<code>sessionhub login rm tablet</code>") || sessionCookieFrom(h) != nil {
		t.Fatalf("taken name: %d %s", status, body)
	}
	if err := e.st.RevokeWebSession(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := e.submitLogin(code, testPublicURL); status != http.StatusSeeOther {
		t.Errorf("same link after the revoke: %d, want 303", status)
	}
}

func TestLoginExpired(t *testing.T) {
	e := newEnv(t)
	code := e.loginCode("tablet")
	e.clock.Advance(10 * time.Minute)
	if status, _, _ := e.do("GET", "/login/"+code, "", nil); status != http.StatusGone {
		t.Errorf("GET after 10m: %d, want 410", status)
	}
	if status, _, _ := e.submitLogin(code, testPublicURL); status != http.StatusGone {
		t.Errorf("POST after 10m: %d, want 410", status)
	}
}

func TestWebSessionsAPI(t *testing.T) {
	e := newEnv(t)
	code, body, _ := e.do("GET", "/v1/web-sessions", e.tokB, nil)
	var l api.WebSessionList
	if code != 200 || json.Unmarshal(body, &l) != nil || len(l.Sessions) != 1 ||
		l.Sessions[0].Name != "phone" || l.Sessions[0].Machine != "tower" || l.Sessions[0].ID != e.webID {
		t.Fatalf("list: %d %s", code, body)
	}
	if strings.Contains(string(body), store.SessionTokenPrefix) || strings.Contains(string(body), store.HashToken(e.web)) {
		t.Errorf("list leaks the token or its hash: %s", body)
	}
	if code, _, _ := e.do("DELETE", "/v1/web-sessions/0000000000000000", e.tokA, nil); code != http.StatusNotFound {
		t.Errorf("DELETE unknown: %d, want 404", code)
	}
	if code, _, _ := e.do("DELETE", "/v1/web-sessions/"+e.webID, e.tokB, nil); code != http.StatusNoContent {
		t.Errorf("DELETE: %d, want 204", code)
	}
	if code, _, _ := e.doWith("GET", "/v1/sessions", "", e.web, nil); code != http.StatusUnauthorized {
		t.Errorf("revoked cookie: %d, want 401", code)
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t)
	other, _ := e.webSession("tablet")
	signOut := map[string]string{api.HeaderAction: api.HeaderActionSignOut}
	code, _, h := e.doHdr("POST", "/logout", "", e.web, signOut, nil)
	c := sessionCookieFrom(h)
	if code != http.StatusNoContent || c == nil || c.MaxAge >= 0 || c.Value != "" {
		t.Fatalf("logout: %d, cookie %+v; want 204 and a cleared cookie", code, c)
	}
	if code, _, _ := e.doWith("GET", "/v1/sessions", "", e.web, nil); code != http.StatusUnauthorized {
		t.Errorf("after sign-out: %d, want 401", code)
	}
	if code, _, _ := e.doWith("GET", "/v1/sessions", "", other, nil); code != 200 {
		t.Errorf("the other browser after sign-out: %d, want 200", code)
	}
	if code, _, _ := e.doHdr("POST", "/logout", e.tokA, "", signOut, nil); code != http.StatusForbidden {
		t.Errorf("machine token on /logout: %d, want 403", code)
	}
}

func TestLoginCodeNotLogged(t *testing.T) {
	e := newEnv(t)
	code := e.loginCode("tablet")
	e.do("GET", "/login/"+code, "", nil)
	e.submitLogin(code, "https://evil.example")
	e.submitLogin(code, testPublicURL)
	out := e.log.String()
	if strings.Contains(out, code) {
		t.Errorf("login code in the request log:\n%s", out)
	}
	if !strings.Contains(out, `GET "/login/<code>" 200`) || !strings.Contains(out, `POST "/login/<code>" 303`) {
		t.Errorf("log lacks the redacted login lines:\n%s", out)
	}
}
```

- [ ] **Step 3: Extend the auth matrix**

In `internal/server/auth_test.go`, add two fields to `authCase`:

```go
	fresh  func() string     // if set, makes the path for each request (the route uses it up)
	hdr    map[string]string // extra request headers
```

In `TestAuthMatrix`, before `cases := map[string]authCase{`, add:

```go
	linkCode := e.loginCode("tablet") // one unused code serves every GET
	freshName := 0
	nextName := func() string { freshName++; return fmt.Sprintf("fresh%d", freshName) }
```

and add these entries to `cases`:

```go
		"GET /login/{code}": {path: "/login/" + linkCode, ok: 200},
		"POST /login/{code}": {fresh: func() string { return "/login/" + e.loginCode(nextName()) },
			hdr: map[string]string{"Origin": testPublicURL}, ok: 303},
		"POST /logout":     {path: "/logout", ok: 204},
		"POST /v1/logins":  {path: "/v1/logins", body: api.LoginIn{Name: "tablet"}, ok: 201},
		"GET /v1/web-sessions": {path: "/v1/web-sessions", ok: 200},
		"DELETE /v1/web-sessions/{id}": {fresh: func() string {
			_, id := e.webSession(nextName())
			return "/v1/web-sessions/" + id
		}, ok: 204},
```

In `expect`, change
`case rt.access == accessWrite && strings.HasPrefix(tk, "cookie"):` to
`case (rt.access == accessWrite || rt.access == accessMachine) && strings.HasPrefix(tk, "cookie"):`,
and add before `case tk == "machineB" && c.okB != 0:`:

```go
		case rt.access == accessSignOut && tk != "cookie+sign-out":
			return []int{http.StatusForbidden} // only a browser ends its own session
```

In the loop, before `before := e.snapshot()`, add:

```go
			path := c.path
			if c.fresh != nil {
				path = c.fresh()
			}
			for k, v := range c.hdr {
				if hdr == nil {
					hdr = map[string]string{}
				}
				hdr[k] = v
			}
```

and change `e.doHdr(rt.method, c.path, tk.bearer, cookie, hdr, c.body)` to
`e.doHdr(rt.method, path, tk.bearer, cookie, hdr, c.body)`.

The unused-code count stays at most 3 (`linkCode` plus two `POST /v1/logins`
successes), under the limit of 5; the codes that `fresh` and `webSession`
make are used at once.

- [ ] **Step 4: Run the tests to verify they fail**

Run: `go test ./internal/server/ -run 'Login|Logout|WebSessionsAPI|AuthMatrix' -count=1`
Expected: FAIL to build with `undefined: loginCSP`, `undefined: buildLoginCSP`,
`undefined: accessMachine`.

- [ ] **Step 5: Implement `internal/server/login.go`**

```go
package server

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"net"
	"net/http"
	"strconv"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

// loginStyle is the login pages' one inline style. The CSP allows it by hash.
const loginStyle = `body{font:16px/1.5 system-ui,sans-serif;margin:0 auto;max-width:32rem;padding:24px 16px}button{font:inherit;min-height:44px;padding:0 20px}code{overflow-wrap:anywhere}`

func buildLoginCSP(style []byte) string {
	sum := sha256.Sum256(style)
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
		"'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
}

// loginCSP is the login pages' Content-Security-Policy.
var loginCSP = buildLoginCSP([]byte(loginStyle))

// loginPage is what the login template shows: the confirm form when Name is
// set, else Message and an optional command to run.
type loginPage struct {
	Name    string // the session name to confirm
	Message string
	Command string // shown in <code> after "Run"
	Tail    string // the rest of the sentence after the command
}

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>sessionhub: sign in</title>
<style>` + loginStyle + `</style></head>
<body>
<h1>sessionhub</h1>
{{if .Name}}<p>Sign in this browser as <strong>{{.Name}}</strong>?</p>
<form method="post"><button type="submit">Sign in</button></form>
{{else}}<p>{{.Message}}</p>
{{if .Command}}<p>Run <code>{{.Command}}</code>{{.Tail}}</p>
{{end}}{{end}}</body></html>
`))

var (
	goneLogin = loginPage{Message: "This link expired or was already used.", Command: "sessionhub login", Tail: " again."}
	// A refused Origin: the POST did not come from the page this server
	// served, for example a form on another site.
	forbiddenLogin = loginPage{Message: "sessionhub refused this sign-in because it did not come from the sessionhub's own page. Open the link again and press Sign in."}
)

func takenLogin(name string) loginPage {
	return loginPage{
		Message: "A browser session named " + name + " exists.",
		Command: "sessionhub login rm " + name,
		Tail:    " to sign that browser out, then press Sign in again. This link works until it expires.",
	}
}

// writeLoginPage sends a login page with the dashboard's security headers
// and its own CSP. Referrer-Policy is same-origin, not no-referrer: with
// no-referrer, browsers send Origin: null on the form POST, which the
// Origin check would refuse. same-origin still keeps the code out of any
// cross-site Referer.
func (s *Server) writeLoginPage(w http.ResponseWriter, status int, pg loginPage) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", loginCSP)
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := loginTmpl.Execute(w, pg); err != nil {
		s.log.Printf("login page: %v", err)
	}
}

// createLogin is POST /v1/logins: a one-time sign-in link for a browser.
func (s *Server) createLogin(w http.ResponseWriter, r *http.Request, p principal) {
	var in api.LoginIn
	if !decode(w, r, &in) {
		return
	}
	code, exp, err := s.store.CreateLoginCode(r.Context(), p.machine.ID, in.Name)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.Login{URL: s.publicURL + "/login/" + code, Name: in.Name, ExpiresAt: exp})
}

// loginPage is GET /login/{code}: the confirm page. It changes nothing, so
// a link previewer that fetches the link does not use it.
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !store.ValidLoginCode(code) {
		s.writeLoginPage(w, http.StatusGone, goneLogin)
		return
	}
	name, err := s.store.LoginCodeName(r.Context(), code)
	if errors.Is(err, store.ErrCodeGone) {
		s.writeLoginPage(w, http.StatusGone, goneLogin)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeLoginPage(w, http.StatusOK, loginPage{Name: name})
}

// loginSubmit is POST /login/{code}, the Sign in button. It uses the code
// once, creates the session, sets the cookie, and redirects to the
// dashboard.
func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if origin := r.Header.Get("Origin"); origin != s.publicURL {
		s.loginRefused(r, "Origin "+strconv.Quote(origin))
		s.writeLoginPage(w, http.StatusForbidden, forbiddenLogin)
		return
	}
	if !store.ValidLoginCode(code) {
		s.loginRefused(r, "malformed code")
		s.writeLoginPage(w, http.StatusGone, goneLogin)
		return
	}
	token, ws, err := s.store.RedeemLoginCode(r.Context(), code)
	switch {
	case errors.Is(err, store.ErrCodeGone):
		s.loginRefused(r, "code expired, used, or unknown")
		s.writeLoginPage(w, http.StatusGone, goneLogin)
		return
	case errors.Is(err, store.ErrNameTaken):
		s.loginRefused(r, "name "+strconv.Quote(ws.Name)+" taken")
		s.writeLoginPage(w, http.StatusConflict, takenLogin(ws.Name))
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	setWho(r, "web:"+ws.Name)
	setSessionCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// loginRefused logs a failed POST /login/{code} with the client address.
func (s *Server) loginRefused(r *http.Request, reason string) {
	s.log.Printf("login refused from %q: %s", clientIP(r), reason)
}

// clientIP is Cf-Connecting-Ip (set by the Cloudflare tunnel), else the
// remote address without its port.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("Cf-Connecting-Ip"); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// listWebSessions is GET /v1/web-sessions. It never returns a token or hash.
func (s *Server) listWebSessions(w http.ResponseWriter, r *http.Request, _ principal) {
	list, err := s.store.ListWebSessions(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.WebSessionList{Sessions: list})
}

// revokeWebSession is DELETE /v1/web-sessions/{id}.
func (s *Server) revokeWebSession(w http.ResponseWriter, r *http.Request, _ principal) {
	if err := s.store.RevokeWebSession(r.Context(), r.PathValue("id")); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// logout is POST /logout: it ends the calling browser's session only.
func (s *Server) logout(w http.ResponseWriter, r *http.Request, p principal) {
	if err := s.store.RevokeWebSession(r.Context(), p.web.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, err)
		return
	}
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 6: Add `browserAction` and redact codes in the log**

In `internal/server/server.go`, add after `actor`:

```go
// browserAction accepts only the session cookie with X-Hub-Action: action.
// A machine token gets 403: it has no browser session to act on.
func (s *Server) browserAction(action string, h authedHandler) http.Handler {
	return s.actor(action, func(w http.ResponseWriter, r *http.Request, p principal) {
		if p.web == nil {
			writeError(w, http.StatusForbidden, "only a signed-in browser can sign out; revoke a browser session with `sessionhub login rm`")
			return
		}
		h(w, r, p)
	})
}
```

In `logRequests`, replace the `s.log.Printf(...)` call with:

```go
		// A login path holds a live one-time code; log a placeholder.
		path := r.URL.Path
		if strings.HasPrefix(path, "/login/") {
			path = "/login/<code>"
		}
		// %q, because the decoded path can hold a newline (from "%0a") that
		// would otherwise forge a second log line.
		s.log.Printf("%s %q %d %s machine=%s", r.Method, path, sw.status,
			time.Since(start).Round(time.Microsecond), info.who)
```

- [ ] **Step 7: Register the routes**

In `internal/server/routes.go`, add to the access constants:

```go
	accessMachine = "machine"  // machine token only, also for reads (sign-in management)
	accessSignOut = "sign-out" // the session cookie with X-Hub-Action: sign-out only
```

and add to `routes()` after the `/{$}` entry:

```go
		{"GET", "/login/{code}", accessPublic, http.HandlerFunc(s.loginPage)},
		{"POST", "/login/{code}", accessPublic, http.HandlerFunc(s.loginSubmit)},
		{"POST", "/logout", accessSignOut, s.browserAction(api.HeaderActionSignOut, s.logout)},
```

and after the `GET /v1/machines` entry:

```go
		{"POST", "/v1/logins", accessMachine, s.writer(s.createLogin)},
		{"GET", "/v1/web-sessions", accessMachine, s.writer(s.listWebSessions)},
		{"DELETE", "/v1/web-sessions/{id}", accessMachine, s.writer(s.revokeWebSession)},
```

- [ ] **Step 8: Run the server tests**

Run: `go test ./internal/server/ -count=1`
Expected: PASS; `TestAuthMatrix` logs `20 routes x 10 credentials, 0 failures`.

- [ ] **Step 9: Update `docs/server.md`**

Add these rows to the **Endpoints** table, after the `GET /` row:

```markdown
| `GET /login/{code}` | none | `200` HTML; `410` HTML | The sign-in confirm page. Changes nothing. |
| `POST /login/{code}` | none, `Origin` must equal `public_url` | `303` to `/` with the cookie; `403`, `409`, or `410` HTML | The **Sign in** button. |
| `POST /logout` | cookie with `X-Hub-Action: sign-out` | `204`, cookie cleared | Ends the calling browser's session. A machine token gets `403`. |
```

and after the `GET /v1/machines` row:

```markdown
| `POST /v1/logins` | machine | `201`, `Login` | Create a one-time sign-in link (`LoginIn`). `400` bad name, `409` name taken, `429` at 5 unused links. |
| `GET /v1/web-sessions` | machine | `200`, `WebSessionList` | Signed-in browsers, by name. Never a token or hash. |
| `DELETE /v1/web-sessions/{id}` | machine | `204`; `404` unknown | Sign a browser out. |
```

Add this section after **Dashboard** (before **Sessions and upserts**):

```markdown
### Sign in a browser

A machine with a client config creates a link, and the browser confirms it:

1. `sessionhub login --name phone` sends `POST /v1/logins` with its machine token.
   The server stores the code's hash and answers with
   `<public_url>/login/<code>`. The code is 26 characters of lowercase
   base32 and works once, for 10 minutes. At most 5 unused links exist at a
   time across all machines.
2. `GET /login/<code>` shows "Sign in this browser as **phone**?" with a
   **Sign in** button. It changes nothing, so a link previewer that fetches
   the link doesn't use it.
3. **Sign in** posts to the same URL. The server checks that `Origin` equals
   `public_url` (`403` otherwise), uses the code in one transaction (the
   first of two presses wins; the second gets `410`), creates the session,
   sets the `hub_session` cookie (`HttpOnly`, `Secure`, `SameSite=Lax`,
   `Path=/`, 30 days), and redirects with `303` to `/`.

A used, expired, or unknown code gets a `410` page that says to run
`sessionhub login` again. If a browser session took the name after the link was
made, the press gets a `409` page and the link keeps working until it
expires. Every refused press is logged with the client address from
`Cf-Connecting-Ip`, else the remote address. The request log prints
`/login/<code>`, never the code.

The login pages carry `Content-Security-Policy: default-src 'none';
style-src '<hash>'; form-action 'self'; base-uri 'none'; frame-ancestors
'none'`, `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`, and
`Referrer-Policy: same-origin`. The policy is `same-origin`, not
`no-referrer`, because with `no-referrer` browsers send `Origin: null` on the
form post.

`sessionhub login ls` lists sessions (`GET /v1/web-sessions`) and `sessionhub login rm`
revokes one (`DELETE /v1/web-sessions/{id}`); any machine token may do both.
**Sign out** on the dashboard (`POST /logout`) ends only that browser's
session. Removing a machine deletes the links and sessions it created.
```

- [ ] **Step 10: Run the full suite and lint**

Run: `make test lint`
Expected: every package `ok`; `gofmt -l` and `go vet` clean.

- [ ] **Step 11: Commit**

```bash
git add internal/server/ docs/server.md
git commit -m "Add one-time sign-in links, the browser session API, and sign-out" -m "GET /login/{code} changes nothing; POST checks Origin against public_url and uses the code once. The login pages use Referrer-Policy same-origin because no-referrer makes browsers send Origin: null on the form post. The request log redacts login codes.

Refs: docs/dev/superpowers/plans/2026-10-01-web-sign-in.md, Task 3

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h"
```

---

### Task 4: Client methods and the QR renderer

**Files:**
- Modify: `go.mod`, `go.sum` (`rsc.io/qr`)
- Modify: `internal/client/http.go` (three methods)
- Modify: `internal/client/http_test.go`
- Create: `internal/cli/qr.go`
- Create: `internal/cli/qr_test.go`

**Interfaces:**
- Consumes: from Task 1, `api.LoginIn`, `api.Login`, `api.WebSession`,
  `api.WebSessionList`. The server routes from Task 3 (the client tests use a
  stub server, so this task does not need Task 3 merged).
- Produces:
  - `func (c *Client) CreateLogin(ctx context.Context, name string) (api.Login, error)`
  - `func (c *Client) ListWebSessions(ctx context.Context) ([]api.WebSession, error)`
  - `func (c *Client) RevokeWebSession(ctx context.Context, id string) error`
  - `func renderQR(w io.Writer, text string) error` and `const qrQuiet = 2` in
    package `cli`.

- [ ] **Step 1: Add the dependency and confirm its API**

Run: `go get rsc.io/qr@latest && go doc rsc.io/qr.Code && go doc rsc.io/qr.Encode`
Expected: `func Encode(text string, level Level) (*Code, error)`, and `Code`
with a `Size int` field and a `Black(x, y int) bool` method; levels `L`, `M`,
`Q`, `H`. If a name differs, use the documented name in Steps 3 and 5 and
keep the behavior.

- [ ] **Step 2: Write the failing client test**

Append to `internal/client/http_test.go`:

```go
func TestLoginEndpoints(t *testing.T) {
	ctx := context.Background()
	c, log := server(t, 201, `{"url":"https://sessionhub.example.test/login/abc","name":"phone","expires_at":"2026-10-01T12:10:00Z"}`)
	l, err := c.CreateLogin(ctx, "phone")
	if err != nil {
		t.Fatal(err)
	}
	if l.URL != "https://sessionhub.example.test/login/abc" || l.Name != "phone" || !l.ExpiresAt.Equal(time.Date(2026, 10, 1, 12, 10, 0, 0, time.UTC)) {
		t.Errorf("login %+v", l)
	}
	got := (*log)[0]
	if got.Method != "POST" || got.Path != "/v1/logins" || got.Auth != "Bearer hub_m_tok" || string(got.Body) != `{"name":"phone"}` {
		t.Errorf("request %+v body %s", got, got.Body)
	}

	c, log = server(t, 200, `{"sessions":[{"id":"0123456789abcdef","name":"phone","machine":"tower","created_at":"2026-10-01T12:00:00Z","last_used_at":"2026-10-01T12:00:00Z","expires_at":"2026-10-31T12:00:00Z"}]}`)
	list, err := c.ListWebSessions(ctx)
	if err != nil || len(list) != 1 || list[0].ID != "0123456789abcdef" || list[0].Machine != "tower" {
		t.Fatalf("list %+v %v", list, err)
	}
	if got := (*log)[0]; got.Method != "GET" || got.Path != "/v1/web-sessions" {
		t.Errorf("request %+v", got)
	}

	c, log = server(t, 204, "")
	if err := c.RevokeWebSession(ctx, "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if got := (*log)[0]; got.Method != "DELETE" || got.Path != "/v1/web-sessions/0123456789abcdef" {
		t.Errorf("request %+v", got)
	}

	c, _ = server(t, 409, `{"error":"a browser session named \"phone\" exists: name taken"}`)
	_, err = c.CreateLogin(ctx, "phone")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 409 || !strings.Contains(se.Message, "phone") {
		t.Errorf("409: %v", err)
	}
}
```

Add `"strings"` to the test file's imports if it is not there.

- [ ] **Step 3: Write the failing QR test**

Create `internal/cli/qr_test.go`:

```go
package cli

import (
	"bytes"
	"strings"
	"testing"

	"rsc.io/qr"
)

// TestRenderQRMatchesEncoder decodes the printed half blocks back into
// modules and compares every one, quiet zone included, with the encoder.
func TestRenderQRMatchesEncoder(t *testing.T) {
	const link = "https://sessionhub.example.com/login/abcdefghijklmnopqrstuvwxyz"
	var buf bytes.Buffer
	if err := renderQR(&buf, link); err != nil {
		t.Fatal(err)
	}
	code, err := qr.Encode(link, qr.L)
	if err != nil {
		t.Fatal(err)
	}
	width := code.Size + 2*qrQuiet
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != (width+1)/2 {
		t.Fatalf("%d lines, want %d", len(lines), (width+1)/2)
	}
	dark := func(x, y int) bool {
		return x >= 0 && y >= 0 && x < code.Size && y < code.Size && code.Black(x, y)
	}
	mismatches := 0
	for i, line := range lines {
		row := []rune(line)
		if len(row) != width {
			t.Fatalf("line %d is %d wide, want %d", i, len(row), width)
		}
		for j, ch := range row {
			var topLight, bottomLight bool
			switch ch {
			case '█':
				topLight, bottomLight = true, true
			case '▀':
				topLight = true
			case '▄':
				bottomLight = true
			case ' ':
			default:
				t.Fatalf("line %d has %q", i, ch)
			}
			x, y := j-qrQuiet, 2*i-qrQuiet
			if topLight == dark(x, y) {
				mismatches++
			}
			if bottomLight == dark(x, y+1) {
				mismatches++
			}
		}
	}
	if mismatches != 0 {
		t.Errorf("%d modules differ from the encoder", mismatches)
	}
	// The quiet zone is light: the first line and each line's first two
	// characters are full blocks.
	if lines[0] != strings.Repeat("█", width) || !strings.HasPrefix(lines[3], "██") {
		t.Errorf("quiet zone not filled:\n%s", buf.String())
	}
}
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `go test ./internal/client/ ./internal/cli/ -count=1`
Expected: FAIL to build with `c.CreateLogin undefined` and
`undefined: renderQR`.

- [ ] **Step 5: Implement the client methods and the renderer**

Append to `internal/client/http.go`:

```go
// CreateLogin asks for a one-time sign-in link for a browser named name.
// Nothing is queued: an unreachable server is an error.
func (c *Client) CreateLogin(ctx context.Context, name string) (api.Login, error) {
	var out api.Login
	err := c.do(ctx, http.MethodPost, "/v1/logins", api.LoginIn{Name: name}, &out)
	return out, err
}

// ListWebSessions lists signed-in browsers.
func (c *Client) ListWebSessions(ctx context.Context) ([]api.WebSession, error) {
	var out api.WebSessionList
	if err := c.do(ctx, http.MethodGet, "/v1/web-sessions", nil, &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

// RevokeWebSession signs out the browser session with id.
func (c *Client) RevokeWebSession(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/web-sessions/"+url.PathEscape(id), nil, nil)
}
```

Create `internal/cli/qr.go`:

```go
package cli

import (
	"io"
	"strings"

	"rsc.io/qr"
)

// qrQuiet is the quiet zone around the code, in modules.
const qrQuiet = 2

// renderQR writes text as a QR code (error correction L) in half-block
// characters, two module rows per line. A light module is drawn filled and a
// dark one is a space, so on a dark terminal, the common case, the code reads
// dark on light with a filled quiet zone. Phone scanners also read the
// inverted code a light terminal shows.
func renderQR(w io.Writer, text string) error {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return err
	}
	light := func(x, y int) bool {
		inside := x >= 0 && y >= 0 && x < code.Size && y < code.Size
		return !inside || !code.Black(x, y)
	}
	var b strings.Builder
	for y := -qrQuiet; y < code.Size+qrQuiet; y += 2 {
		for x := -qrQuiet; x < code.Size+qrQuiet; x++ {
			top, bottom := light(x, y), light(x, y+1)
			switch {
			case top && bottom:
				b.WriteRune('█')
			case top:
				b.WriteRune('▀')
			case bottom:
				b.WriteRune('▄')
			default:
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}
	_, err = io.WriteString(w, b.String())
	return err
}
```

The code size is odd, so the last line's bottom half is one extra light row
below the quiet zone; the test accepts it because `dark` is false outside the
matrix.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/client/ ./internal/cli/ -count=1`
Expected: PASS.

- [ ] **Step 7: Run the full suite and lint, then check the module files**

Run: `go mod tidy && make test lint && git diff go.mod`
Expected: tests and lint green; `go.mod` gains only `rsc.io/qr` in its
direct `require` block, without `// indirect`.

- [ ] **Step 8: Commit**

```bash
git add go.mod go.sum internal/client/http.go internal/client/http_test.go internal/cli/qr.go internal/cli/qr_test.go
git commit -m "Add client calls for sign-in links and a half-block QR renderer" -m "The renderer draws light modules filled, so the quiet zone shows on a dark terminal. Its test decodes the printed characters and compares every module with rsc.io/qr.

Refs: docs/dev/superpowers/plans/2026-10-01-web-sign-in.md, Task 4

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h"
```

---

### Task 5: The `sessionhub login` command

**Files:**
- Create: `internal/cli/login.go`
- Create: `internal/cli/login_test.go`
- Modify: `internal/cli/cli.go` (`env.login`, `defaultEnv`)
- Modify: `cmd/sessionhub/main.go` (route and usage), `cmd/sessionhub/main_test.go`
- Modify: `docs/cli.md`

**Interfaces:**
- Consumes: from Task 4, `(*client.Client).CreateLogin`, `ListWebSessions`,
  `RevokeWebSession`, `renderQR`. `client.StatusError`, `clean`, `age`
  (existing, `internal/cli/cli.go`).
- Produces: `func RunLogin(ctx context.Context, args []string) error` and the
  `login` route in `cmd/sessionhub/main.go`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/login_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

type fakeLogin struct {
	login    api.Login
	sessions []api.WebSession
	err      error
	names    []string // CreateLogin calls
	revoked  []string
	listed   int
}

func (f *fakeLogin) CreateLogin(_ context.Context, name string) (api.Login, error) {
	f.names = append(f.names, name)
	return f.login, f.err
}
func (f *fakeLogin) ListWebSessions(context.Context) ([]api.WebSession, error) {
	f.listed++
	return f.sessions, f.err
}
func (f *fakeLogin) RevokeWebSession(_ context.Context, id string) error {
	f.revoked = append(f.revoked, id)
	return f.err
}

const testLoginURL = "https://sessionhub.example.com/login/abcdefghijklmnopqrstuvwxyz"

func runLogin(t *testing.T, f *fakeLogin, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := &env{login: f, out: &out, now: func() time.Time { return now }}
	err := e.loginCmd(context.Background(), args)
	return out.String(), err
}

func sessionsFixture() []api.WebSession {
	return []api.WebSession{
		{ID: "0123456789abcdef", Name: "phone", Machine: "bluebox", CreatedAt: now.Add(-48 * time.Hour), LastUsedAt: now.Add(-3 * time.Hour), ExpiresAt: now.Add(27 * 24 * time.Hour)},
		{ID: "fedcba9876543210", Name: "windows", Machine: "tower", CreatedAt: now.Add(-24 * time.Hour), LastUsedAt: now.Add(-time.Minute), ExpiresAt: now.Add(30 * 24 * time.Hour)},
	}
}

func TestLoginRequiresName(t *testing.T) {
	f := &fakeLogin{}
	for _, args := range [][]string{nil, {"--no-qr"}, {"--name", ""}, {"phone"}} {
		if _, err := runLogin(t, f, args...); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("args %q: err %v, want a usage error", args, err)
		}
	}
	if len(f.names) != 0 {
		t.Errorf("sent %d requests without a name", len(f.names))
	}
}

func TestLoginPrintsLinkAndQR(t *testing.T) {
	f := &fakeLogin{login: api.Login{URL: testLoginURL, Name: "phone", ExpiresAt: now.Add(10 * time.Minute)}}
	out, err := runLogin(t, f, "--name", "phone")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.names) != 1 || f.names[0] != "phone" {
		t.Errorf("requested %v", f.names)
	}
	link := strings.Index(out, testLoginURL)
	qrAt := strings.Index(out, "█")
	if link < 0 || qrAt < link || !strings.Contains(out, "expires at "+now.Add(10*time.Minute).Local().Format("15:04:05")) ||
		!strings.Contains(out, "sign it in as phone") {
		t.Errorf("output:\n%s", out)
	}
	var qrBuf bytes.Buffer
	renderQR(&qrBuf, testLoginURL)
	if !strings.HasSuffix(out, qrBuf.String()) {
		t.Error("output does not end with the QR code of the link")
	}

	out, err = runLogin(t, f, "--name", "phone", "--no-qr")
	if err != nil || strings.Contains(out, "█") || !strings.Contains(out, testLoginURL) {
		t.Errorf("--no-qr: %v\n%s", err, out)
	}
}

func TestLoginJSON(t *testing.T) {
	exp := time.Date(2026, 10, 1, 12, 10, 0, 0, time.UTC)
	f := &fakeLogin{login: api.Login{URL: testLoginURL, Name: "phone", ExpiresAt: exp}}
	out, err := runLogin(t, f, "--json", "--name", "phone")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"url":"` + testLoginURL + `","name":"phone","expires_at":"2026-10-01T12:10:00Z"}` + "\n"
	if out != want {
		t.Errorf("--json output %q, want %q", out, want)
	}
}

func TestLoginNameTaken(t *testing.T) {
	f := &fakeLogin{err: &client.StatusError{Status: 409, Message: `a browser session named "phone" exists: name taken`}}
	_, err := runLogin(t, f, "--name", "phone")
	want := "login: a browser session named phone exists; revoke it with `sessionhub login rm phone` or pick another name"
	if err == nil || err.Error() != want {
		t.Errorf("err %v, want %q", err, want)
	}
}

func TestLoginUnreachable(t *testing.T) {
	f := &fakeLogin{err: errors.New("dial tcp 127.0.0.1:8787: connect: connection refused")}
	if _, err := runLogin(t, f, "--name", "phone"); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err %v, want the connection error", err)
	}
}

func TestLoginLs(t *testing.T) {
	f := &fakeLogin{sessions: sessionsFixture()}
	out, err := runLogin(t, f, "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "ID", "MACHINE", "CREATED", "LAST_USED", "EXPIRES",
		"phone", "0123456789abcdef", "bluebox", "3h", "windows", "tower", "1m",
		now.Add(27 * 24 * time.Hour).Local().Format("2006-01-02 15:04")} {
		if !strings.Contains(out, want) {
			t.Errorf("ls lacks %q:\n%s", want, out)
		}
	}
	out, err = runLogin(t, f, "ls", "--json")
	var l api.WebSessionList
	if err != nil || json.Unmarshal([]byte(out), &l) != nil || len(l.Sessions) != 2 || l.Sessions[1].Name != "windows" {
		t.Errorf("ls --json: %v %s", err, out)
	}
	out, err = runLogin(t, &fakeLogin{sessions: []api.WebSession{}}, "ls")
	if err != nil || !strings.Contains(out, "no browser sessions") {
		t.Errorf("empty ls: %v %q", err, out)
	}
}

func TestLoginRm(t *testing.T) {
	tests := []struct {
		name    string
		arg     string
		revoked []string
		err     string
	}{
		{"by name", "phone", []string{"0123456789abcdef"}, ""},
		{"by id", "fedcba9876543210", []string{"fedcba9876543210"}, ""},
		{"no match", "tablet", nil, `no browser session named or with ID "tablet"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLogin{sessions: sessionsFixture()}
			out, err := runLogin(t, f, "rm", tt.arg)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Errorf("err %v, want %q", err, tt.err)
				}
			} else if err != nil || !strings.Contains(out, "signed out") {
				t.Errorf("rm %s: %v %q", tt.arg, err, out)
			}
			if strings.Join(f.revoked, ",") != strings.Join(tt.revoked, ",") {
				t.Errorf("revoked %v, want %v", f.revoked, tt.revoked)
			}
		})
	}
	// A name wins over an ID with the same text.
	f := &fakeLogin{sessions: []api.WebSession{
		{ID: "aaaaaaaaaaaaaaaa", Name: "bbbbbbbbbbbbbbbb"},
		{ID: "bbbbbbbbbbbbbbbb", Name: "other"},
	}}
	if _, err := runLogin(t, f, "rm", "bbbbbbbbbbbbbbbb"); err != nil || len(f.revoked) != 1 || f.revoked[0] != "aaaaaaaaaaaaaaaa" {
		t.Errorf("name-over-ID: %v %v", err, f.revoked)
	}
	for _, args := range [][]string{{"rm"}, {"rm", "a", "b"}} {
		if _, err := runLogin(t, &fakeLogin{}, args...); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("args %q: %v, want a usage error", args, err)
		}
	}
}
```

In `cmd/sessionhub/main_test.go`, add `"login"` to the `commands` slice.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/ ./cmd/sessionhub/ -count=1`
Expected: FAIL to build with `unknown field login in struct literal` and
`e.loginCmd undefined`; `cmd/sessionhub` fails with `real route table missing "login"`.

- [ ] **Step 3: Wire the login API into `env`**

In `internal/cli/cli.go`, add a field to `env` after `api hubAPI`:

```go
	login loginAPI
```

and in `defaultEnv`, change `return &env{cfg: cfg, api: c, out: os.Stdout, now: time.Now}, nil`
to `return &env{cfg: cfg, api: c, login: c, out: os.Stdout, now: time.Now}, nil`.
Change the package comment to
`// Package cli implements the sessionhub client commands ls, show, status, and login.`

- [ ] **Step 4: Implement `internal/cli/login.go`**

```go
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

// loginAPI is the part of *client.Client that sessionhub login uses.
type loginAPI interface {
	CreateLogin(ctx context.Context, name string) (api.Login, error)
	ListWebSessions(ctx context.Context) ([]api.WebSession, error)
	RevokeWebSession(ctx context.Context, id string) error
}

const loginUsage = `usage:
  sessionhub login --name <name> [--no-qr] [--json]
  sessionhub login ls [--json]
  sessionhub login rm <name|id>`

// RunLogin implements `sessionhub login`, `sessionhub login ls`, and `sessionhub login rm`.
func RunLogin(ctx context.Context, args []string) error {
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	return e.loginCmd(ctx, args)
}

func (e *env) loginCmd(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "ls":
			return e.loginLs(ctx, args[1:])
		case "rm":
			return e.loginRm(ctx, args[1:])
		}
	}
	return e.loginCreate(ctx, args)
}

func (e *env) loginCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessionhub login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "the browser session's name")
	noQR := fs.Bool("no-qr", false, "omit the QR code")
	asJSON := fs.Bool("json", false, "print only {url, name, expires_at}")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("login: %v\n%s", err, loginUsage)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("login: unexpected argument %q\n%s", fs.Arg(0), loginUsage)
	}
	if *name == "" {
		return fmt.Errorf("login: --name is required\n%s", loginUsage)
	}
	l, err := e.login.CreateLogin(ctx, *name)
	var se *client.StatusError
	if errors.As(err, &se) && se.Status == http.StatusConflict {
		return fmt.Errorf("login: a browser session named %s exists; revoke it with `sessionhub login rm %s` or pick another name", *name, *name)
	}
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if *asJSON {
		b, err := json.Marshal(l)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(e.out, "%s\n", b)
		return err
	}
	fmt.Fprintf(e.out, "Open this link on the device to sign it in as %s:\n\n  %s\n\n", clean(l.Name, 0), clean(l.URL, 0))
	fmt.Fprintf(e.out, "The link works once and expires at %s.\n", l.ExpiresAt.Local().Format("15:04:05"))
	if *noQR {
		return nil
	}
	fmt.Fprintln(e.out)
	return renderQR(e.out, l.URL)
}

func (e *env) loginLs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessionhub login ls", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print {sessions: [...]} as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return fmt.Errorf("login ls: unexpected arguments\n%s", loginUsage)
	}
	list, err := e.login.ListWebSessions(ctx)
	if err != nil {
		return fmt.Errorf("login ls: %w", err)
	}
	if *asJSON {
		b, err := json.Marshal(api.WebSessionList{Sessions: list})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(e.out, "%s\n", b)
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(e.out, "no browser sessions; sign one in with: sessionhub login --name <device>")
		return nil
	}
	const day = "2006-01-02 15:04"
	tw := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tMACHINE\tCREATED\tLAST_USED\tEXPIRES")
	for _, s := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", clean(s.Name, 0), clean(s.ID, 0), clean(s.Machine, 0),
			s.CreatedAt.Local().Format(day), age(e.now(), s.LastUsedAt), s.ExpiresAt.Local().Format(day))
	}
	return tw.Flush()
}

// loginRm resolves a name, then an ID, through the list, and deletes by ID.
func (e *env) loginRm(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("login rm: want one name or ID\n%s", loginUsage)
	}
	target := args[0]
	list, err := e.login.ListWebSessions(ctx)
	if err != nil {
		return fmt.Errorf("login rm: %w", err)
	}
	var hit *api.WebSession
	for i := range list {
		if list[i].Name == target {
			hit = &list[i]
			break
		}
	}
	if hit == nil {
		for i := range list {
			if list[i].ID == target {
				hit = &list[i]
				break
			}
		}
	}
	if hit == nil {
		return fmt.Errorf("login rm: no browser session named or with ID %q; see `sessionhub login ls`", target)
	}
	if err := e.login.RevokeWebSession(ctx, hit.ID); err != nil {
		return fmt.Errorf("login rm: %w", err)
	}
	fmt.Fprintf(e.out, "signed out %s (%s)\n", clean(hit.Name, 0), clean(hit.ID, 0))
	return nil
}

// Compile-time check that the real client satisfies loginAPI.
var _ loginAPI = (*client.Client)(nil)
```

- [ ] **Step 5: Route the command**

In `cmd/sessionhub/main.go`, add to `routes`: `"login": cli.RunLogin,` (after
`"status"`), and to `usage` after the `status` line:

```
  login --name <device>         sign a browser in to the dashboard
  login ls | rm <name|id>       list or sign out browsers
```

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/cli/ ./cmd/sessionhub/ -count=1`
Expected: PASS.

- [ ] **Step 7: Document the command**

In `docs/cli.md`, add rows to the **Commands** table after `sessionhub status`:

```markdown
| `sessionhub login --name <name> [--no-qr] [--json]` | Create a one-time link that signs a browser in to the dashboard as `<name>`, and print the link, when it expires, and a QR code of it. `--no-qr` omits the QR code. `--json` prints only `{"url":...,"name":...,"expires_at":...}`. See below. |
| `sessionhub login ls [--json]` | Signed-in browsers: name, ID, machine, created, last used, and expiry. |
| `sessionhub login rm <name\|id>` | Sign a browser out. It matches a name first, then an ID. No confirmation. |
```

Add a section before `## \`sessionhub digest\``:

```markdown
## `sessionhub login`

`sessionhub login` signs a browser in to the dashboard without copying a secret. Run
it on any machine with a client config; it uses that machine's token.

```sh
sessionhub login --name phone
```

Open the printed link on the device, or scan the QR code, and press
**Sign in**. The link works once, for 10 minutes. The browser stays signed in
for 30 days after its last visit.

- The name follows the machine-name rules: 1 to 64 letters, digits, `.`,
  `_`, or `-`, starting with a letter or digit. If a browser session already
  has the name, the command fails and suggests `sessionhub login rm <name>`.
- At most 5 unused links exist at a time; the server answers `429` above that.
- Nothing is queued. If the server is unreachable, the command fails.
- The QR code uses half blocks, two module rows per line, with light modules
  drawn filled so the code reads dark on light in a dark terminal. If your
  terminal is light and your phone doesn't scan it, use `--no-qr` and open the
  link another way.
- **Sign out** on the dashboard ends only that browser's session.
  `sessionhub login rm` signs out any browser.
```

- [ ] **Step 8: Run the full suite and lint**

Run: `make test lint`
Expected: every package `ok`; `gofmt -l` and `go vet` clean.

- [ ] **Step 9: Commit**

```bash
git add internal/cli/login.go internal/cli/login_test.go internal/cli/cli.go cmd/sessionhub/main.go cmd/sessionhub/main_test.go docs/cli.md
git commit -m "Add sessionhub login to sign browsers in, list them, and sign them out" -m "sessionhub login prints the link, its expiry, and a QR code. rm resolves a name before an ID through the list, then deletes by ID.

Refs: docs/dev/superpowers/plans/2026-10-01-web-sign-in.md, Task 5

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h"
```

---

### Task 6: Dashboard: who is signed in, Sign out, and the signed-out state

**Files:**
- Modify: `internal/server/dashboard/index.html`
- Modify: `internal/server/dashboard_test.go`
- Modify: `docs/server.md` (Dashboard section)

**Interfaces:**
- Consumes: from Task 2, the `X-Hub-Session-Name` header on
  `GET /v1/sessions` and `401` for a missing session; from Task 3,
  `POST /logout` with `X-Hub-Action: sign-out` (`204`).
- Produces: the page's `auth-state` block with pure functions
  `sessionAuth(status, name)`, `signOutRequest()`, `signOutDone(status)` and
  the constant `SIGNED_OUT`.

- [ ] **Step 1: Write the failing tests**

In `internal/server/dashboard_test.go`, add:

```go
// TestDashboardAuthStates runs the page's auth-state block under node: the
// signed-out decision, the sign-out request, and its outcome.
func TestDashboardAuthStates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; this table runs the page's auth-state block under node")
	}
	m := regexp.MustCompile(`(?s)// auth-state:begin\n(.*?)// auth-state:end`).FindSubmatch(dashboardHTML)
	if m == nil {
		t.Fatal("dashboard/index.html has no auth-state block")
	}
	type auth struct {
		SignedOut bool   `json:"signedOut"`
		Name      string `json:"name"`
	}
	auths := []struct {
		status int
		name   any
		want   auth
	}{
		{401, nil, auth{true, ""}},
		{401, "phone", auth{true, ""}},
		{200, "phone", auth{false, "phone"}},
		{200, nil, auth{false, ""}}, // a machine token: no name
		{500, "phone", auth{false, "phone"}},
	}
	var in []any
	for _, a := range auths {
		in = append(in, []any{a.status, a.name})
	}
	input, _ := json.Marshal(map[string]any{"auths": in, "done": []int{204, 401, 403, 500}})
	script := string(m[1]) + `
var input = JSON.parse(require("fs").readFileSync(0, "utf8"));
process.stdout.write(JSON.stringify({
  auths: input.auths.map(function (a) { return sessionAuth(a[0], a[1]); }),
  req: signOutRequest(),
  done: input.done.map(signOutDone),
  text: SIGNED_OUT
}));`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got struct {
		Auths []auth `json:"auths"`
		Req   struct {
			URL  string `json:"url"`
			Init struct {
				Method  string            `json:"method"`
				Headers map[string]string `json:"headers"`
				Cache   string            `json:"cache"`
			} `json:"init"`
		} `json:"req"`
		Done []bool `json:"done"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %s: %v", out, err)
	}
	for i, a := range auths {
		if got.Auths[i] != a.want {
			t.Errorf("sessionAuth(%d, %v) = %+v, want %+v", a.status, a.name, got.Auths[i], a.want)
		}
	}
	if got.Req.URL != "/logout" || got.Req.Init.Method != "POST" || got.Req.Init.Cache != "no-store" ||
		!reflect.DeepEqual(got.Req.Init.Headers, map[string]string{"X-Hub-Action": "sign-out"}) {
		t.Errorf("signOutRequest() = %+v", got.Req)
	}
	if !reflect.DeepEqual(got.Done, []bool{true, true, false, false}) {
		t.Errorf("signOutDone(204, 401, 403, 500) = %v", got.Done)
	}
	if !strings.Contains(got.Text, "sessionhub login --name <device>") {
		t.Errorf("SIGNED_OUT = %q", got.Text)
	}
}
```

Rename `TestDashboardSendsOneKindOfWrite` to `TestDashboardWrites`, change its
comment to `// The page's writes are the Remote Control request and sign-out, each with its header.`,
and replace its count table with:

```go
		{"method:", 2},
		{`method: "POST"`, 2},
		{`"X-Hub-Action": "remote-control"`, 1},
		{`"X-Hub-Action": "sign-out"`, 1},
		{`"/remote-control"`, 1},
		{"fetch(", 5}, // the list, the request, the session poll, the Details read, and sign-out
		{"// rc-state:begin", 1},
		{"// rc-state:end", 1},
		{"// auth-state:begin", 1},
		{"// auth-state:end", 1},
```

and add after the table loop:

```go
	// A 401 stops polling and shows the signed-out message.
	for _, want := range []string{"if (auth.signedOut) { showSignedOut(); return null; }", "clearInterval(refreshTimer)",
		"refreshTimer = setInterval(load, REFRESH_MS)", "if (signedOut) return;", `resp.headers.get("X-Hub-Session-Name")`,
		"whoName.textContent = auth.name", ".who[hidden] { display: none; }"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "?token=") {
		t.Error("page still mentions ?token=")
	}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/server/ -run Dashboard -count=1`
Expected: FAIL: `no auth-state block`, and `page has "method:" 1 times, want 2`.

- [ ] **Step 3: Add the header markup and style**

In `internal/server/dashboard/index.html`, add to the `<style>` block after the
`#updated` rule:

```css
.who { display: flex; gap: 8px; align-items: center; color: var(--muted); font-size: .85rem; }
.who[hidden] { display: none; }
.who strong { color: var(--fg); }
```

Replace the `<header>` element with:

```html
<header>
  <h1>sessionhub</h1>
  <span id="updated" aria-live="polite"></span>
  <span id="who" class="who" hidden>Signed in as <strong id="who-name"></strong> <button id="signout" type="button">Sign out</button></span>
  <input id="filter" type="search" placeholder="Filter sessions" aria-label="Filter sessions" autocomplete="off">
</header>
```

- [ ] **Step 4: Add the auth-state block and wire it**

After the variable declarations at the top of the script (after
`var sessionsById = {};`), add:

```js
  var whoEl = document.getElementById("who");
  var whoName = document.getElementById("who-name");
  var signOutBtn = document.getElementById("signout");
  var signedOut = false;
  var refreshTimer = null;

  // auth-state:begin
  var SIGNED_OUT = "You are signed out. Run sessionhub login --name <device> on a machine with sessionhub, then open the link it prints.";

  // sessionAuth reads a GET /v1/sessions response: signed out on 401, else
  // the browser session's name from X-Hub-Session-Name ("" for a machine
  // token).
  function sessionAuth(status, name) {
    if (status === 401) return { signedOut: true, name: "" };
    return { signedOut: false, name: typeof name === "string" ? name : "" };
  }

  // signOutRequest is the sign-out write: POST /logout with its header.
  function signOutRequest() {
    return { url: "/logout", init: { method: "POST", headers: { "X-Hub-Action": "sign-out" }, cache: "no-store" } };
  }

  // signOutDone: the session is gone after a 204, or was already gone (401).
  function signOutDone(status) { return status === 204 || status === 401; }
  // auth-state:end
```

Before `function load() {`, add:

```js
  // showSignedOut stops polling and replaces the list with the signed-out
  // message.
  function showSignedOut() {
    signedOut = true;
    if (refreshTimer !== null) clearInterval(refreshTimer);
    refreshTimer = null;
    whoEl.hidden = true;
    lastSessions = [];
    root.replaceChildren(el("p", "empty", SIGNED_OUT));
    updated.textContent = "";
  }
```

Replace `load` and the startup lines at the end of the script with:

```js
  function load() {
    if (signedOut) return;
    fetch("/v1/sessions", { headers: { "Accept": "application/json" }, cache: "no-store" })
      .then(function (resp) {
        var auth = sessionAuth(resp.status, resp.headers.get("X-Hub-Session-Name"));
        if (auth.signedOut) { showSignedOut(); return null; }
        if (!resp.ok) throw new Error("The server returned " + resp.status + ".");
        whoName.textContent = auth.name;
        whoEl.hidden = auth.name === "";
        return resp.json();
      })
      .then(function (data) {
        if (data === null || signedOut) return;
        lastSessions = data;
        render(data);
        updated.textContent = "Updated " + new Date().toLocaleTimeString();
      })
      .catch(function (err) {
        if (signedOut) return;
        // Keep the last good render on a transient network error.
        if (!root.querySelector(".card, h2")) root.replaceChildren(el("p", "error", err.message || "Could not load sessions."));
        updated.textContent = "Update failed " + new Date().toLocaleTimeString();
      });
  }

  signOutBtn.addEventListener("click", function () {
    var req = signOutRequest();
    signOutBtn.disabled = true;
    fetch(req.url, req.init)
      .then(function (resp) {
        if (signOutDone(resp.status)) { showSignedOut(); return; }
        signOutBtn.disabled = false;
        updated.textContent = "Sign out failed: the server returned " + resp.status + ".";
      })
      .catch(function () {
        signOutBtn.disabled = false;
        updated.textContent = "Sign out failed: could not reach the sessionhub.";
      });
  });

  load();
  filterEl.addEventListener("input", function () { if (!signedOut) render(lastSessions); });
  refreshTimer = setInterval(load, REFRESH_MS);
  document.addEventListener("visibilitychange", function () { if (!document.hidden) load(); });
```

- [ ] **Step 5: Run the dashboard tests**

Run: `go test ./internal/server/ -run Dashboard -count=1 -v`
Expected: PASS, with `TestDashboardAuthStates` and the three existing node
tables running (not skipped) when `node` is on `PATH`. `TestDashboardHTMLAndCSP`
still passes: the header uses the `hidden` attribute and a class, no `style=`.

- [ ] **Step 6: Document the dashboard change**

In `docs/server.md`, section **Dashboard**, add after the paragraph that starts
`A browser signs in with a one-time link`:

```markdown
The header shows "Signed in as **<name>**", from the `X-Hub-Session-Name`
header, and a **Sign out** button. **Sign out** sends `POST /logout` with
`X-Hub-Action: sign-out` and then shows the signed-out message. If a poll gets
`401`, for example after `sessionhub login rm`, the page stops polling and shows the
signed-out message instead of an empty list.
```

Change `A tap is the page's only write request.` to
`A tap is one of the page's two write requests; the other is **Sign out**.`

- [ ] **Step 7: Run the full suite and lint**

Run: `make test lint`
Expected: every package `ok`; `gofmt -l` and `go vet` clean.

- [ ] **Step 8: Commit**

```bash
git add internal/server/dashboard/index.html internal/server/dashboard_test.go docs/server.md
git commit -m "Show the signed-in browser on the dashboard, with Sign out" -m "A 401 on a poll stops polling and shows the signed-out message. Sign out posts to /logout with X-Hub-Action: sign-out.

Refs: docs/dev/superpowers/plans/2026-10-01-web-sign-in.md, Task 6

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h"
```

---

### Task 7: README, SPEC, and PLAN

**Files:**
- Modify: `README.md` (install step 2, **Use it**, **Upgrade**)
- Modify: `docs/dev/SPEC.md` (component 1 auth line)
- Modify: `docs/dev/PLAN.md` (HTTP API paragraph and table, as-built section)

**Interfaces:**
- Consumes: everything above. No code changes.
- Produces: user-facing docs that match the build.

- [ ] **Step 1: Check what still mentions the read token**

Run: `grep -rn "read_token\|read token\|?token=\|hub_read\|SESSIONHUB_READ_TOKEN" README.md docs/dev/SPEC.md docs/dev/PLAN.md docs/ internal/ cmd/`
Expected: hits in `README.md`, `docs/dev/SPEC.md`, `docs/dev/PLAN.md`, and only the intended
ones elsewhere (`docs/server.md` rows that say the setting is ignored,
`config.go`, `run.go`, and tests). Fix any other hit in this task.

- [ ] **Step 2: Update `README.md`**

Replace install step 2 (from `2. Create \`~/.config/sessionhub/server.toml\` with mode \`0600\` and a fresh read`
through the closing code fence of its `sh` block) with:

````markdown
2. Create `~/.config/sessionhub/server.toml` with mode `0600`:

   ```sh
   umask 077
   mkdir -p ~/.config/sessionhub
   cat > ~/.config/sessionhub/server.toml <<EOF
   listen = ["127.0.0.1:8787", "10.200.0.1:8787"]
   public_url = "https://sessionhub.example.com"
   stale_after = "5m"
   EOF
   chmod 0600 ~/.config/sessionhub/server.toml
   ```
````

In **Use it**, replace the bullet that starts
`- The dashboard is at \`https://sessionhub.example.com/\`. Open` with:

```markdown
- The dashboard is at `https://sessionhub.example.com/`. To sign a browser in, run
  `sessionhub login --name <device>` on any joined machine, then open the printed
  link on that device or scan the QR code, and press **Sign in**. The browser
  stays signed in for 30 days after its last visit. `sessionhub login ls` lists
  signed-in browsers, and `sessionhub login rm <name>` signs one out.
```

In **Upgrade**, add before `To check that the watcher runs the installed binary, compare inodes:`:

```markdown
### Upgrade to web sign-in

This release replaces the dashboard's read token with sign-in links. The
database moves to schema version 5. Clients (hooks, the herdr watcher, MCP)
are unchanged: they use machine tokens.

1. Deploy with `make deploy`, which backs up the database first. The first
   start upgrades it to version 5.
2. Keep the `read_token` line in `tower:~/.config/sessionhub/server.toml` until the
   upgrade is confirmed. The new server ignores it and logs one warning; the
   previous binary needs it if you roll back.
3. Sign in each device. On any joined machine:

   ```sh
   sessionhub login --name windows
   sessionhub login --name phone
   ```

   Open each link on its device, or scan the QR code with the phone, and
   press **Sign in**.
4. Delete the `read_token` line, then restart the server:
   `systemctl --user restart sessionhub`.

To roll back, stop the server (`systemctl --user stop sessionhub`), restore the
database backup and the previous binary as described above, put `read_token`
back if you deleted it, and start the server. Browsers sign in with
`?token=` again.
```

- [ ] **Step 3: Update `docs/dev/SPEC.md`**

Replace the line that starts `- Auth: per-machine bearer tokens;` with:

```markdown
- Auth: per-machine bearer tokens; every write is attributed to a machine. Machine tokens can also read, so the CLI on each machine uses its own token. Browsers sign in to the dashboard with a one-time link from `sessionhub login` (confirmed with a button, shown with a QR code) and then hold a session cookie that lasts 30 days after its last use; `sessionhub login rm` revokes one without a restart. The design is in `docs/dev/superpowers/specs/2026-10-01-web-sign-in-design.md`.
```

- [ ] **Step 4: Update `docs/dev/PLAN.md`**

In **HTTP API**, replace the paragraph that starts
`All bodies are JSON. Writes need a machine token` with:

```markdown
All bodies are JSON. Writes need a machine token (`Authorization: Bearer`);
reads accept a machine token or the `hub_session` cookie of a signed-in
browser. The cookie also authorizes `POST /v1/sessions/{id}/remote-control`
with `X-Hub-Action: remote-control` and `POST /logout` with
`X-Hub-Action: sign-out`, and nothing else.
```

Add these rows to its table after the `GET /` row:

```markdown
| `POST /v1/logins` | CLI (`sessionhub login`) | One-time sign-in link: `201 {url, name, expires_at}`; `409` name taken; `429` at 5 unused. |
| `GET /login/{code}` | browser | Confirm page; changes nothing. |
| `POST /login/{code}` | browser | Uses the code, sets `hub_session`, `303` to `/`. `Origin` must equal `public_url`. |
| `GET /v1/web-sessions` | CLI (`sessionhub login ls`) | Signed-in browsers; never a token or hash. |
| `DELETE /v1/web-sessions/{id}` | CLI (`sessionhub login rm`) | Sign a browser out. |
| `POST /logout` | dashboard | End this browser's session (cookie + `X-Hub-Action: sign-out`). |
```

Add this section before `## Tests and captured payloads`:

```markdown
## Web sign-in, as built

The design is in `docs/dev/superpowers/specs/2026-10-01-web-sign-in-design.md`.
The build settles these points the spec leaves open:

- The login pages send `Referrer-Policy: same-origin`, not the dashboard's
  `no-referrer`: with `no-referrer`, browsers send `Origin: null` on the form
  post and the `Origin` check refuses every sign-in.
- The request log prints `/login/<code>` instead of the code, like it drops
  query strings, so a live link never sits in the journal.
- The 5-link limit is global, not per machine.
- A press whose name a newer session took rolls back: the page answers `409`
  and the link keeps working until it expires.
- A machine token on `POST /logout` gets `403`; only a browser ends its own
  session. Any machine token may list and revoke any browser session.
- `GET /` still accepts a machine bearer token.
- The request log labels a browser caller `web:<name>`.
- Login page failures (`403`, `409`, `410`) are HTML pages.
- `sessionhub login rm` matches a name before an ID.
- The QR code draws light modules filled and dark modules as spaces, so the
  quiet zone shows on a dark terminal.
- `sessionhub login ls` shows `LAST_USED` as an age, like `sessionhub ls`.
```

- [ ] **Step 5: Check the docs build nothing broken**

Run: `make test lint && grep -rn "?token=\|hub_read" README.md docs/dev/SPEC.md docs/dev/PLAN.md`
Expected: tests and lint green; the `grep` prints only the README rollback
sentence that mentions `?token=`.

- [ ] **Step 6: Commit**

```bash
git add README.md docs/dev/SPEC.md docs/dev/PLAN.md
git commit -m "Document web sign-in: install, use, upgrade, rollback, and as-built notes" -m "Refs: docs/dev/superpowers/plans/2026-10-01-web-sign-in.md, Task 7

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h"
```

---

## Deploy and live check (notes for the controller)

These steps run on the real machines. They are not a task: the controller runs
them with the user's approval, after Task 7.

1. **Full suite.** Run `make test lint` and record the package count and `0`
   failures.
2. **Deploy.** Run `make deploy` (backs up `sessionhub.db` on `tower`, swaps the
   binary, restarts the server, reinstalls the plugin). On `bluebox`, run
   `make install`. Keep `read_token` in `tower:~/.config/sessionhub/server.toml`.
3. **Confirm the upgrade.** On `tower`:
   `sqlite3 ~/.local/share/sessionhub/sessionhub.db 'PRAGMA user_version'` prints `5`, and
   `journalctl --user -u sessionhub -n 50 | grep -c 'warning: read_token'` prints
   `1`. Confirm hooks and the watcher still write
   (`sessionhub ls` shows fresh `last_seen` ages).
4. **Sign in.** On `bluebox`, run `sessionhub login --name windows` and open the link in
   the Windows browser; run `sessionhub login --name phone` and scan the QR code with
   the phone. Press **Sign in** on each. Both dashboards show
   "Signed in as ...". `sessionhub login ls` lists both.
5. **Check refusals.** Open a used link again: `410` page. On `tower`,
   `journalctl --user -u sessionhub | grep 'login refused'` shows the address from
   `Cf-Connecting-Ip`, and `journalctl --user -u sessionhub | grep -c '/login/<code>'`
   is non-zero while no real code appears.
6. **Revoke.** From `bluebox`, run `sessionhub login rm phone`. Within one poll
   (30 seconds), the phone shows the signed-out message and stops polling.
   Sign the phone in again afterwards.
7. **Finish the upgrade.** Delete the `read_token` line on `tower` and run
   `systemctl --user restart sessionhub`; the journal shows no `read_token` warning.
8. **Evidence.** Write `docs/dev/evidence/web-sign-in.md` with the commands and
   outputs from steps 1 to 7 (no tokens, no codes), and commit it with the
   usual trailers.
