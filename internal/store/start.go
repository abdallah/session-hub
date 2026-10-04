package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
)

// MaxPendingStartsPerMachine caps one machine's pending start requests. A
// start request stays open for ControlTTL, pending or claimed, like a Remote
// Control request.
const MaxPendingStartsPerMachine = 5

var startIDRE = regexp.MustCompile(`^st_[A-Za-z0-9_-]{22}$`)

// ValidStartID reports whether id has the shape of a start request ID.
func ValidStartID(id string) bool { return startIDRE.MatchString(id) }

// checkStartIn validates a start request's directory and prompt. The
// directory must look absolute (/... or ~/...); the machine's watcher checks
// that it exists and is inside its home directory.
func checkStartIn(in api.StartIn) error {
	dir := in.Dir
	if dir == "" {
		return invalidf("dir is empty")
	}
	if len(dir) > api.MaxStartDirBytes {
		return invalidf("dir is %d bytes, at most %d allowed", len(dir), api.MaxStartDirBytes)
	}
	if err := checkNoControl("dir", dir); err != nil {
		return err
	}
	if !strings.HasPrefix(dir, "/") && dir != "~" && !strings.HasPrefix(dir, "~/") {
		return invalidf("dir %q: want an absolute path, or one starting with ~/", dir)
	}
	if n := utf8.RuneCountInString(in.Prompt); n > api.MaxStartPromptRunes {
		return invalidf("prompt is %d characters, at most %d allowed", n, api.MaxStartPromptRunes)
	}
	for _, r := range in.Prompt {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return invalidf("prompt contains a control character (U+%04X), which is not allowed", r)
		}
	}
	return nil
}

// startSelect reads a start request and the name of its machine.
const startSelect = `SELECT r.id, m.name, r.dir, r.prompt, r.state, r.requested_by, r.created_at, r.expires_at,
	r.claimed_at, r.finished_at, r.url, r.detail
FROM start_requests r JOIN machines m ON m.id = r.machine_id`

func scanStart(sc scanner) (api.StartRequest, error) {
	var r api.StartRequest
	var created, expires string
	var claimed, finished sql.NullString
	if err := sc.Scan(&r.ID, &r.Machine, &r.Dir, &r.Prompt, &r.State, &r.RequestedBy, &created, &expires,
		&claimed, &finished, &r.URL, &r.Detail); err != nil {
		return api.StartRequest{}, err
	}
	var err error
	if r.CreatedAt, err = parseTS(created); err != nil {
		return api.StartRequest{}, err
	}
	if r.ExpiresAt, err = parseTS(expires); err != nil {
		return api.StartRequest{}, err
	}
	if r.ClaimedAt, err = parseNullTS(claimed); err != nil {
		return api.StartRequest{}, err
	}
	if r.FinishedAt, err = parseNullTS(finished); err != nil {
		return api.StartRequest{}, err
	}
	return r, nil
}

// expireStartsTx stores the expiry of every open start request past
// expires_at. Create, claim, and result run it first; GetStart reports an
// expiry without storing it.
func expireStartsTx(ctx context.Context, tx *sql.Tx, n string) error {
	_, err := tx.ExecContext(ctx, `UPDATE start_requests SET state = ?, finished_at = ?
		WHERE state IN (?, ?) AND expires_at <= ?`,
		api.ControlExpired, n, api.ControlPending, api.ControlClaimed, n)
	return err
}

// CreateStart opens a request to start a new Claude session on machine, in
// in.Dir, started by by (web:<name> or machine:<name>). An unknown machine is
// ErrNotFound; a machine whose watcher hasn't polled in ControlPollWindow is
// ErrNotControllable; more than MaxPendingStartsPerMachine pending is
// ErrTooMany.
func (s *Store) CreateStart(ctx context.Context, machine string, in api.StartIn, by string) (api.StartRequest, error) {
	if !ValidMachineName(machine) {
		return api.StartRequest{}, invalidf("machine %q: use 1-64 letters, digits, '.', '_', or '-', starting with a letter or digit", machine)
	}
	if err := checkStartIn(in); err != nil {
		return api.StartRequest{}, err
	}
	now := s.Now()
	nowS := formatTS(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.StartRequest{}, err
	}
	defer tx.Rollback()
	if err := expireStartsTx(ctx, tx, nowS); err != nil {
		return api.StartRequest{}, err
	}
	var machineID int64
	var lastPoll sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id, last_poll FROM machines WHERE name = ?`, machine).Scan(&machineID, &lastPoll)
	if errors.Is(err, sql.ErrNoRows) {
		return api.StartRequest{}, fmt.Errorf("%w: machine %q", ErrNotFound, machine)
	}
	if err != nil {
		return api.StartRequest{}, err
	}
	ok, err := polled(lastPoll, now)
	if err != nil {
		return api.StartRequest{}, err
	}
	if !ok {
		return api.StartRequest{}, fmt.Errorf("%w: the sessionhub watcher on machine %q is offline (no poll in the last %s)",
			ErrNotControllable, machine, ControlPollWindow)
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM start_requests WHERE machine_id = ? AND state = ?`,
		machineID, api.ControlPending).Scan(&pending); err != nil {
		return api.StartRequest{}, err
	}
	if pending >= MaxPendingStartsPerMachine {
		return api.StartRequest{}, fmt.Errorf("%w: machine %q already has %d pending start requests", ErrTooMany, machine, pending)
	}
	id, err := newRandomID("st_")
	if err != nil {
		return api.StartRequest{}, err
	}
	r := api.StartRequest{ID: id, Machine: machine, Dir: in.Dir, Prompt: in.Prompt, State: api.ControlPending,
		RequestedBy: by, CreatedAt: now, ExpiresAt: now.Add(ControlTTL)}
	if _, err := tx.ExecContext(ctx, `INSERT INTO start_requests (id, machine_id, dir, prompt, state, requested_by,
		created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, machineID, r.Dir, r.Prompt, r.State, r.RequestedBy, nowS, formatTS(r.ExpiresAt)); err != nil {
		return api.StartRequest{}, err
	}
	return r, tx.Commit()
}

// GetStart returns start request id. An open request past expires_at reads
// as expired.
func (s *Store) GetStart(ctx context.Context, id string) (api.StartRequest, error) {
	if !ValidStartID(id) {
		return api.StartRequest{}, invalidf("start id %q: want st_ and 22 base64url characters", id)
	}
	r, err := scanStart(s.db.QueryRowContext(ctx, startSelect+` WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return api.StartRequest{}, fmt.Errorf("%w: start request %s", ErrNotFound, id)
	}
	if err != nil {
		return api.StartRequest{}, err
	}
	r.State = effectiveState(r.State, r.ExpiresAt, s.Now())
	return r, nil
}

// ClaimStart marks the machine's oldest pending start request claimed and
// returns it as a claim. ok is false when nothing is pending.
func (s *Store) ClaimStart(ctx context.Context, machineID int64) (api.ControlClaim, bool, error) {
	now := s.Now()
	nowS := formatTS(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	defer tx.Rollback()
	if err := expireStartsTx(ctx, tx, nowS); err != nil {
		return api.ControlClaim{}, false, err
	}
	r, err := scanStart(tx.QueryRowContext(ctx, startSelect+` WHERE r.machine_id = ? AND r.state = ?
		ORDER BY r.created_at, r.rowid LIMIT 1`, machineID, api.ControlPending))
	if errors.Is(err, sql.ErrNoRows) {
		return api.ControlClaim{}, false, tx.Commit()
	}
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE start_requests SET state = ?, claimed_at = ? WHERE id = ?`,
		api.ControlClaimed, nowS, r.ID); err != nil {
		return api.ControlClaim{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return api.ControlClaim{}, false, err
	}
	r.State, r.ClaimedAt = api.ControlClaimed, &now
	req := api.ControlRequest{ID: r.ID, Machine: r.Machine, Action: api.ActionStart, State: r.State,
		RequestedBy: r.RequestedBy, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, ClaimedAt: r.ClaimedAt}
	return api.ControlClaim{Request: req, Start: &r}, true, nil
}

// FinishStart records the result of a start request that machineID claimed.
// A done result may carry the new session's Remote Control link.
func (s *Store) FinishStart(ctx context.Context, machineID int64, id string, in api.ControlResultIn) (api.StartRequest, error) {
	if !ValidStartID(id) {
		return api.StartRequest{}, invalidf("start id %q: want st_ and 22 base64url characters", id)
	}
	if in.State != api.ControlDone && in.State != api.ControlFailed {
		return api.StartRequest{}, invalidf("state %q: want done or failed", in.State)
	}
	if in.URL != "" && (in.State != api.ControlDone || !ValidRemoteControlURL(in.URL)) {
		return api.StartRequest{}, invalidf("url %q: want https://claude.ai/code/session_<id>, on a done result only", in.URL)
	}
	if len(in.URL) > MaxControlURLLen {
		return api.StartRequest{}, invalidf("url: %d bytes, want at most %d", len(in.URL), MaxControlURLLen)
	}
	if err := checkText("detail", in.Detail, api.MaxControlDetailRunes); err != nil {
		return api.StartRequest{}, err
	}
	nowS := formatTS(s.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.StartRequest{}, err
	}
	defer tx.Rollback()
	if err := expireStartsTx(ctx, tx, nowS); err != nil {
		return api.StartRequest{}, err
	}
	var owner int64
	var state string
	err = tx.QueryRowContext(ctx, `SELECT machine_id, state FROM start_requests WHERE id = ?`, id).Scan(&owner, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return api.StartRequest{}, fmt.Errorf("%w: start request %s", ErrNotFound, id)
	}
	if err != nil {
		return api.StartRequest{}, err
	}
	if owner != machineID {
		return api.StartRequest{}, fmt.Errorf("%w: start request %s belongs to another machine", ErrWrongMachine, id)
	}
	if state != api.ControlClaimed {
		return api.StartRequest{}, fmt.Errorf("%w: start request %s is %s, not claimed", ErrRequestClosed, id, state)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE start_requests SET state = ?, finished_at = ?, url = ?, detail = ? WHERE id = ?`,
		in.State, nowS, in.URL, in.Detail, id); err != nil {
		return api.StartRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.StartRequest{}, err
	}
	return s.GetStart(ctx, id)
}
