package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"

	"github.com/abdallah/session-hub/internal/api"
)

// MachineTokenPrefix starts every machine token. Browser session tokens use
// SessionTokenPrefix (weblogin.go).
const MachineTokenPrefix = "hub_m_"

// NewToken returns prefix + 32 random bytes, base64url without padding.
func NewToken(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken is the SHA-256 hex digest stored in machines.token_hash.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

var (
	machineNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	// Hosts end up in a shell command (resume_command), so no spaces or
	// shell metacharacters, no leading '-' (ssh would read it as an option),
	// and no '/', '[', or ']'.
	hostRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:-]{0,254}$`)
)

// ValidMachineName reports whether name is an acceptable machine name.
func ValidMachineName(name string) bool { return machineNameRE.MatchString(name) }

// ValidHost reports whether h is an acceptable ssh_host or herdr_host.
func ValidHost(h string) bool { return hostRE.MatchString(h) }

// Machine identifies the machine a token belongs to.
type Machine struct {
	ID   int64
	Name string
}

// AddMachine creates the machine, or rotates its token if it exists, and
// returns the new token. An empty sshHost or herdrHost keeps the stored
// value, or defaults to the name for a new machine.
func (s *Store) AddMachine(ctx context.Context, name, sshHost, herdrHost string) (token string, created bool, err error) {
	if !ValidMachineName(name) {
		return "", false, invalidf("machine name %q: use 1-64 letters, digits, '.', '_', or '-'", name)
	}
	for _, h := range []string{sshHost, herdrHost} {
		if h != "" && !ValidHost(h) {
			return "", false, invalidf("host %q contains characters that are not allowed: use 1-255 letters, digits, '@', '.', '_', ':', or '-', starting with a letter or digit", h)
		}
	}
	token, err = NewToken(MachineTokenPrefix)
	if err != nil {
		return "", false, err
	}
	hash := HashToken(token)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM machines WHERE name = ?`, name).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		created = true
		if sshHost == "" {
			sshHost = name
		}
		if herdrHost == "" {
			herdrHost = name
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO machines (name, token_hash, ssh_host, herdr_host) VALUES (?, ?, ?, ?)`,
			name, hash, sshHost, herdrHost)
	case err == nil:
		_, err = tx.ExecContext(ctx, `UPDATE machines SET token_hash = ?,
			ssh_host = CASE WHEN ? = '' THEN ssh_host ELSE ? END,
			herdr_host = CASE WHEN ? = '' THEN herdr_host ELSE ? END
			WHERE id = ?`, hash, sshHost, sshHost, herdrHost, herdrHost, id)
	}
	if err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return token, created, nil
}

// MachineImpact counts what removing a machine deletes with it.
type MachineImpact struct {
	Sessions, Events, Reports int
}

// MachineImpact returns what RemoveMachine would delete, or ErrNotFound.
func (s *Store) MachineImpact(ctx context.Context, name string) (MachineImpact, error) {
	var in MachineImpact
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM machines WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return in, fmt.Errorf("machine %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return in, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM sessions WHERE machine_id = ?),
		(SELECT COUNT(*) FROM events WHERE session_id IN (SELECT id FROM sessions WHERE machine_id = ?)),
		(SELECT COUNT(*) FROM reports WHERE session_id IN (SELECT id FROM sessions WHERE machine_id = ?))`,
		id, id, id).Scan(&in.Sessions, &in.Events, &in.Reports)
	return in, err
}

// RemoveMachine deletes the machine, which revokes its token. Its sessions,
// events, and reports are deleted with it.
func (s *Store) RemoveMachine(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM machines WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("machine %q: %w", name, ErrNotFound)
	}
	return nil
}

// ListMachines returns every machine ordered by name.
func (s *Store) ListMachines(ctx context.Context) ([]api.Machine, error) {
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
		m.Online = ok
		out = append(out, m)
	}
	return out, rows.Err()
}

// MachineByToken returns the machine whose token hash matches token, and
// records it as seen now. It compares against every stored hash in constant
// time, with no early exit. It returns ErrNotFound for an unknown token.
func (s *Store) MachineByToken(ctx context.Context, token string) (Machine, error) {
	want := []byte(HashToken(token))
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, token_hash FROM machines`)
	if err != nil {
		return Machine{}, err
	}
	var found Machine
	for rows.Next() {
		var m Machine
		var hash string
		if err := rows.Scan(&m.ID, &m.Name, &hash); err != nil {
			rows.Close()
			return Machine{}, err
		}
		if subtle.ConstantTimeCompare([]byte(hash), want) == 1 {
			found = m
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Machine{}, err
	}
	if found.ID == 0 {
		return Machine{}, ErrNotFound
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE machines SET last_seen = ? WHERE id = ?`,
		formatTS(s.Now()), found.ID); err != nil {
		return Machine{}, err
	}
	return found, nil
}
