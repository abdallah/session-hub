package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// Control request limits.
const (
	// ControlTTL is how long a request stays open, pending or claimed.
	ControlTTL = 2 * time.Minute
	// ControlPollWindow is how recent a machine's last poll must be for its
	// sessions to be controllable.
	ControlPollWindow = 2 * time.Minute
	// MaxPendingPerMachine caps one machine's pending requests.
	MaxPendingPerMachine = 10
	// MaxControlURLLen caps a result's URL, in bytes.
	MaxControlURLLen = 200
)

var (
	controlIDRE = regexp.MustCompile(`^cr_[A-Za-z0-9_-]{22}$`)
	// remoteControlURLRE is the only link a result may carry.
	remoteControlURLRE = regexp.MustCompile(`^https://claude\.ai/code/session_[A-Za-z0-9_-]+$`)
)

// ValidControlID reports whether id has the shape of a control request ID.
func ValidControlID(id string) bool { return controlIDRE.MatchString(id) }

// ValidRemoteControlURL reports whether u is a claude.ai Remote Control link.
func ValidRemoteControlURL(u string) bool { return remoteControlURLRE.MatchString(u) }

func newControlID() (string, error) {
	return newRandomID("cr_")
}

// effectiveState is the state a read reports: a pending or claimed request
// past expires_at is expired, whether or not a write has stored that yet.
func effectiveState(state string, expires, now time.Time) string {
	if (state == api.ControlPending || state == api.ControlClaimed) && !now.Before(expires) {
		return api.ControlExpired
	}
	return state
}

// expireTx stores the expiry of every open request past expires_at. Create,
// claim, and result run it first; reads use effectiveState instead.
func expireTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	n := formatTS(now)
	_, err := tx.ExecContext(ctx, `UPDATE control_requests SET state = ?, finished_at = ?
		WHERE state IN (?, ?) AND expires_at <= ?`,
		api.ControlExpired, n, api.ControlPending, api.ControlClaimed, n)
	return err
}

// nullControl is a control_requests row, possibly read through a LEFT JOIN.
type nullControl struct {
	id, action, state, requestedBy, created, expires, claimed, finished, url, detail sql.NullString
}

// request returns the row as a request, or nil when the join found none.
func (n nullControl) request(sessionID, machine string, now time.Time) (*api.ControlRequest, error) {
	if !n.id.Valid {
		return nil, nil
	}
	r := api.ControlRequest{ID: n.id.String, SessionID: sessionID, Machine: machine, Action: n.action.String,
		State: n.state.String, RequestedBy: n.requestedBy.String, URL: n.url.String, Detail: n.detail.String}
	var err error
	if r.CreatedAt, err = parseTS(n.created.String); err != nil {
		return nil, err
	}
	if r.ExpiresAt, err = parseTS(n.expires.String); err != nil {
		return nil, err
	}
	if r.ClaimedAt, err = parseNullTS(n.claimed); err != nil {
		return nil, err
	}
	if r.FinishedAt, err = parseNullTS(n.finished); err != nil {
		return nil, err
	}
	r.State = effectiveState(r.State, r.ExpiresAt, now)
	return &r, nil
}

// controlSelect reads a request and the name of its machine.
const controlSelect = `SELECT c.id, c.action, c.state, c.requested_by, c.created_at, c.expires_at,
	c.claimed_at, c.finished_at, c.url, c.detail, c.session_id, m.name
FROM control_requests c JOIN machines m ON m.id = c.machine_id`

func scanControl(sc scanner, now time.Time) (api.ControlRequest, error) {
	var n nullControl
	var sessionID, machine string
	if err := sc.Scan(&n.id, &n.action, &n.state, &n.requestedBy, &n.created, &n.expires,
		&n.claimed, &n.finished, &n.url, &n.detail, &sessionID, &machine); err != nil {
		return api.ControlRequest{}, err
	}
	r, err := n.request(sessionID, machine, now)
	if err != nil {
		return api.ControlRequest{}, err
	}
	return *r, nil
}

// RecordPoll stores now as the machine's last long poll.
func (s *Store) RecordPoll(ctx context.Context, machineID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machines SET last_poll = ? WHERE id = ?`, formatTS(s.Now()), machineID)
	return err
}

// CreateControl opens a Remote Control request for session id, or returns
// the session's open one with created false. requestedBy is
// api.RequestedByDashboard or api.RequestedByMachinePrefix + <name>. Checks
// run in this order: unknown session (ErrNotFound), no herdr pane, an
// offline watcher, or an open move of the session (ErrNotControllable), an
// open request (returned), and MaxPendingPerMachine (ErrTooMany).
func (s *Store) CreateControl(ctx context.Context, id, requestedBy string) (api.ControlRequest, bool, error) {
	if !ValidSessionID(id) {
		return api.ControlRequest{}, false, invalidf("session id %q: use 1-128 letters, digits, '_', or '-', starting with a letter or digit", id)
	}
	if requestedBy != api.RequestedByDashboard &&
		!(strings.HasPrefix(requestedBy, api.RequestedByMachinePrefix) && ValidMachineName(strings.TrimPrefix(requestedBy, api.RequestedByMachinePrefix))) {
		return api.ControlRequest{}, false, invalidf("requested_by %q: want dashboard or machine:<name>", requestedBy)
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ControlRequest{}, false, err
	}
	defer tx.Rollback()
	if err := expireTx(ctx, tx, now); err != nil {
		return api.ControlRequest{}, false, err
	}
	var machineID int64
	var machine, pane string
	var lastPoll sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT s.machine_id, m.name, s.herdr_pane, m.last_poll
		FROM sessions s JOIN machines m ON m.id = s.machine_id WHERE s.id = ?`, id).
		Scan(&machineID, &machine, &pane, &lastPoll)
	if errors.Is(err, sql.ErrNoRows) {
		return api.ControlRequest{}, false, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	if err != nil {
		return api.ControlRequest{}, false, err
	}
	if pane == "" {
		return api.ControlRequest{}, false, fmt.Errorf("%w: session %s is not in herdr", ErrNotControllable, id)
	}
	polled, err := parseNullTS(lastPoll)
	if err != nil {
		return api.ControlRequest{}, false, err
	}
	if polled == nil || now.Sub(*polled) > ControlPollWindow {
		return api.ControlRequest{}, false, fmt.Errorf("%w: the sessionhub watcher on machine %q is offline (no poll in the last %s)",
			ErrNotControllable, machine, ControlPollWindow)
	}
	// During a move the session must start nowhere else: on the source
	// Remote Control would resume it while the target runs it.
	if _, _, moving, err := openMoveTx(ctx, tx, id); err != nil {
		return api.ControlRequest{}, false, err
	} else if moving {
		return api.ControlRequest{}, false, fmt.Errorf("%w: %s", ErrNotControllable, detailMoveOpen)
	}
	open, err := scanControl(tx.QueryRowContext(ctx, controlSelect+` WHERE c.session_id = ? AND c.state IN (?, ?)
		ORDER BY c.created_at DESC, c.rowid DESC LIMIT 1`, id, api.ControlPending, api.ControlClaimed), now)
	if err == nil {
		return open, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return api.ControlRequest{}, false, err
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_requests WHERE machine_id = ? AND state = ?`,
		machineID, api.ControlPending).Scan(&pending); err != nil {
		return api.ControlRequest{}, false, err
	}
	if pending >= MaxPendingPerMachine {
		return api.ControlRequest{}, false, fmt.Errorf("%w: machine %q already has %d pending requests", ErrTooMany, machine, pending)
	}
	rid, err := newControlID()
	if err != nil {
		return api.ControlRequest{}, false, err
	}
	r := api.ControlRequest{ID: rid, SessionID: id, Machine: machine, Action: api.ActionRemoteControl,
		State: api.ControlPending, RequestedBy: requestedBy, CreatedAt: now, ExpiresAt: now.Add(ControlTTL)}
	if _, err := tx.ExecContext(ctx, `INSERT INTO control_requests (id, session_id, machine_id, action, state,
		requested_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, id, machineID, r.Action, r.State, r.RequestedBy, formatTS(r.CreatedAt), formatTS(r.ExpiresAt)); err != nil {
		return api.ControlRequest{}, false, err
	}
	payload, err := json.Marshal(map[string]string{"request_id": r.ID, "requested_by": requestedBy})
	if err != nil {
		return api.ControlRequest{}, false, err
	}
	if err := insertEventTx(ctx, tx, id, now, api.SourceServer, api.KindRemoteControlRequested, payload); err != nil {
		return api.ControlRequest{}, false, err
	}
	return r, true, tx.Commit()
}

// detailMoveOpen is why a Remote Control request is refused or failed
// while its session moves.
const detailMoveOpen = "a move is open for this session"

// ClaimControl marks the machine's oldest pending request claimed and
// returns it with its session. ok is false when nothing is pending. A
// pending request whose session has an open move (one queued just before
// the move) fails instead of being handed out.
func (s *Store) ClaimControl(ctx context.Context, machineID int64) (api.ControlClaim, bool, error) {
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	defer tx.Rollback()
	if err := expireTx(ctx, tx, now); err != nil {
		return api.ControlClaim{}, false, err
	}
	var r api.ControlRequest
	for {
		r, err = scanControl(tx.QueryRowContext(ctx, controlSelect+` WHERE c.machine_id = ? AND c.state = ?
			ORDER BY c.created_at, c.rowid LIMIT 1`, machineID, api.ControlPending), now)
		if errors.Is(err, sql.ErrNoRows) {
			return api.ControlClaim{}, false, tx.Commit()
		}
		if err != nil {
			return api.ControlClaim{}, false, err
		}
		_, _, moving, err := openMoveTx(ctx, tx, r.SessionID)
		if err != nil {
			return api.ControlClaim{}, false, err
		}
		if !moving {
			break
		}
		if _, err := tx.ExecContext(ctx, `UPDATE control_requests SET state = ?, finished_at = ?, detail = ? WHERE id = ?`,
			api.ControlFailed, formatTS(now), detailMoveOpen, r.ID); err != nil {
			return api.ControlClaim{}, false, err
		}
		payload, err := json.Marshal(map[string]string{"request_id": r.ID, "state": api.ControlFailed, "detail": detailMoveOpen})
		if err != nil {
			return api.ControlClaim{}, false, err
		}
		if err := insertEventTx(ctx, tx, r.SessionID, now, api.SourceServer, api.KindRemoteControlResult, payload); err != nil {
			return api.ControlClaim{}, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE control_requests SET state = ?, claimed_at = ? WHERE id = ?`,
		api.ControlClaimed, formatTS(now), r.ID); err != nil {
		return api.ControlClaim{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return api.ControlClaim{}, false, err
	}
	r.State, r.ClaimedAt = api.ControlClaimed, &now
	// GetSession uses s.db, so it runs after the commit: inside the
	// transaction it would wait forever for the one connection.
	sess, err := s.GetSession(ctx, r.SessionID)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	return api.ControlClaim{Request: r, Session: sess}, true, nil
}

// FinishControl records the result of a request that machineID claimed. A
// done result with a URL also sets the session's last link.
func (s *Store) FinishControl(ctx context.Context, machineID int64, reqID string, in api.ControlResultIn) (api.ControlRequest, error) {
	if !ValidControlID(reqID) {
		return api.ControlRequest{}, invalidf("request id %q: want cr_ and 22 base64url characters", reqID)
	}
	if in.State != api.ControlDone && in.State != api.ControlFailed {
		return api.ControlRequest{}, invalidf("state %q: want done or failed", in.State)
	}
	if in.URL != "" && (in.State != api.ControlDone || !ValidRemoteControlURL(in.URL)) {
		return api.ControlRequest{}, invalidf("url %q: want https://claude.ai/code/session_<id>, on a done result only", in.URL)
	}
	if len(in.URL) > MaxControlURLLen {
		return api.ControlRequest{}, invalidf("url: %d bytes, want at most %d", len(in.URL), MaxControlURLLen)
	}
	if err := checkText("detail", in.Detail, api.MaxControlDetailRunes); err != nil {
		return api.ControlRequest{}, err
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ControlRequest{}, err
	}
	defer tx.Rollback()
	if err := expireTx(ctx, tx, now); err != nil {
		return api.ControlRequest{}, err
	}
	var owner int64
	var state, sessionID string
	err = tx.QueryRowContext(ctx, `SELECT machine_id, state, session_id FROM control_requests WHERE id = ?`, reqID).
		Scan(&owner, &state, &sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return api.ControlRequest{}, fmt.Errorf("%w: request %s", ErrNotFound, reqID)
	}
	if err != nil {
		return api.ControlRequest{}, err
	}
	if owner != machineID {
		return api.ControlRequest{}, fmt.Errorf("%w: request %s belongs to another machine", ErrWrongMachine, reqID)
	}
	if state != api.ControlClaimed {
		return api.ControlRequest{}, fmt.Errorf("%w: request %s is %s, not claimed", ErrRequestClosed, reqID, state)
	}
	nowS := formatTS(now)
	if _, err := tx.ExecContext(ctx, `UPDATE control_requests SET state = ?, finished_at = ?, url = ?, detail = ? WHERE id = ?`,
		in.State, nowS, in.URL, in.Detail, reqID); err != nil {
		return api.ControlRequest{}, err
	}
	if in.URL != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET rc_url = ?, rc_at = ? WHERE id = ?`, in.URL, nowS, sessionID); err != nil {
			return api.ControlRequest{}, err
		}
	}
	p := map[string]string{"request_id": reqID, "state": in.State}
	if in.URL != "" {
		p["url"] = in.URL
	}
	if in.Detail != "" {
		p["detail"] = in.Detail
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return api.ControlRequest{}, err
	}
	if err := insertEventTx(ctx, tx, sessionID, now, api.SourcePlugin, api.KindRemoteControlResult, payload); err != nil {
		return api.ControlRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.ControlRequest{}, err
	}
	return scanControl(s.db.QueryRowContext(ctx, controlSelect+` WHERE c.id = ?`, reqID), now)
}
