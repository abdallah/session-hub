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
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version %d %v, want %d", v, err, schemaVersion)
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
