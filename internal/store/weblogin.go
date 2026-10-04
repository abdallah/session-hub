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
