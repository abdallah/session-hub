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

// Permission request limits. See docs/server.md, "Permission requests".
const (
	// PermissionTTL is how long a request stays open. The hook waits as
	// long, inside its 660-second hook timeout.
	PermissionTTL = 10 * time.Minute
	// MaxReasonRunes caps a deny reason.
	MaxReasonRunes = 200
	// PermissionGrace: a herdr snapshot that shows a session out of
	// blocked leaves a request this recent open, because the snapshot may
	// have been built before the request arrived.
	PermissionGrace   = 5 * time.Second
	maxToolNameRunes  = 128
	maxDecidedByRunes = 100
)

var permissionIDRE = regexp.MustCompile(`^pr_[A-Za-z0-9_-]{22}$`)

// ValidPermissionID reports whether id has the shape of a permission
// request ID.
func ValidPermissionID(id string) bool { return permissionIDRE.MatchString(id) }

// permissionSelect reads a request, its machine's name, and its machine ID.
const permissionSelect = `SELECT p.id, p.session_id, m.name, p.tool_name, p.tool_input, p.truncated, p.state,
	p.decision, p.reason, p.decided_by, p.created_at, p.expires_at, p.decided_at, p.machine_id
FROM permission_requests p JOIN machines m ON m.id = p.machine_id`

// scanPermission reads one permissionSelect row. An open request past
// expires_at reads as expired.
func scanPermission(sc scanner, now time.Time) (api.PermissionRequest, int64, error) {
	var p api.PermissionRequest
	var input, created, expires string
	var truncated int
	var decided sql.NullString
	var machineID int64
	if err := sc.Scan(&p.ID, &p.SessionID, &p.Machine, &p.ToolName, &input, &truncated, &p.State,
		&p.Decision, &p.Reason, &p.DecidedBy, &created, &expires, &decided, &machineID); err != nil {
		return p, 0, err
	}
	p.ToolInput = json.RawMessage(input)
	p.Truncated = truncated != 0
	var err error
	if p.CreatedAt, err = parseTS(created); err != nil {
		return p, 0, err
	}
	if p.ExpiresAt, err = parseTS(expires); err != nil {
		return p, 0, err
	}
	if p.DecidedAt, err = parseNullTS(decided); err != nil {
		return p, 0, err
	}
	if p.State == api.PermissionOpen && !now.Before(p.ExpiresAt) {
		p.State = api.PermissionExpired
	}
	return p, machineID, nil
}

// expirePermissionsTx stores the expiry of every open request past
// expires_at.
func expirePermissionsTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	n := formatTS(now)
	_, err := tx.ExecContext(ctx, `UPDATE permission_requests SET state = ?, updated_at = ?
		WHERE state = ? AND expires_at <= ?`, api.PermissionExpired, n, api.PermissionOpen, n)
	return err
}

// closePermissionsTx moves session id's open requests created at or before
// upTo to state: answered_locally when the session left blocked, closed
// when it ended.
func closePermissionsTx(ctx context.Context, tx *sql.Tx, sessionID, state string, upTo, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE permission_requests SET state = ?, updated_at = ?
		WHERE session_id = ? AND state = ? AND created_at <= ?`,
		state, formatTS(now), sessionID, api.PermissionOpen, formatTS(upTo))
	return err
}

// CreatePermission opens a permission request for session id, which
// machineID must own. The tool input is capped with api.CapToolInput; cwd
// and suggestions are not stored.
func (s *Store) CreatePermission(ctx context.Context, machineID int64, sessionID string, in api.PermissionIn) (api.PermissionRequest, error) {
	if !ValidSessionID(sessionID) {
		return api.PermissionRequest{}, invalidf("session id %q: use 1-128 letters, digits, '_', or '-', starting with a letter or digit", sessionID)
	}
	tool := strings.TrimSpace(in.ToolName)
	if tool == "" {
		return api.PermissionRequest{}, invalidf("tool_name is empty")
	}
	if err := checkText("tool_name", tool, maxToolNameRunes); err != nil {
		return api.PermissionRequest{}, err
	}
	input, cut, err := api.CapToolInput(in.ToolInput)
	if err != nil {
		return api.PermissionRequest{}, invalidf("tool_input: %v", err)
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.PermissionRequest{}, err
	}
	defer tx.Rollback()
	if err := checkOwnerTx(ctx, tx, machineID, sessionID); err != nil {
		return api.PermissionRequest{}, err
	}
	var ended sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT ended_at FROM sessions WHERE id = ?`, sessionID).Scan(&ended); err != nil {
		return api.PermissionRequest{}, err
	}
	if ended.Valid {
		return api.PermissionRequest{}, fmt.Errorf("%w: session %s has ended", ErrGone, sessionID)
	}
	id, err := newRandomID("pr_")
	if err != nil {
		return api.PermissionRequest{}, err
	}
	truncated := 0
	if cut {
		truncated = 1
	}
	nowS := formatTS(now)
	if _, err := tx.ExecContext(ctx, `INSERT INTO permission_requests (id, session_id, machine_id, tool_name, tool_input,
		truncated, state, created_at, expires_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sessionID, machineID, tool, string(input), truncated, api.PermissionOpen, nowS,
		formatTS(now.Add(PermissionTTL)), nowS); err != nil {
		return api.PermissionRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.PermissionRequest{}, err
	}
	return s.GetPermission(ctx, id)
}

// GetPermission reads one request.
func (s *Store) GetPermission(ctx context.Context, id string) (api.PermissionRequest, error) {
	p, _, err := s.permission(ctx, id)
	return p, err
}

// PermissionFor reads one request for the machine that owns it.
func (s *Store) PermissionFor(ctx context.Context, machineID int64, id string) (api.PermissionRequest, error) {
	p, owner, err := s.permission(ctx, id)
	if err != nil {
		return p, err
	}
	if owner != machineID {
		return api.PermissionRequest{}, fmt.Errorf("%w: request %s belongs to another machine", ErrWrongMachine, id)
	}
	return p, nil
}

func (s *Store) permission(ctx context.Context, id string) (api.PermissionRequest, int64, error) {
	if !ValidPermissionID(id) {
		return api.PermissionRequest{}, 0, invalidf("request id %q: want pr_ and 22 base64url characters", id)
	}
	p, owner, err := scanPermission(s.db.QueryRowContext(ctx, permissionSelect+` WHERE p.id = ?`, id), s.Now())
	if errors.Is(err, sql.ErrNoRows) {
		return api.PermissionRequest{}, 0, fmt.Errorf("%w: request %s", ErrNotFound, id)
	}
	return p, owner, err
}

// DecidePermission records the one decision for an open request: allow or
// deny, with a reason for a deny. by is web:<name> or machine:<name>. A
// decided request is ErrRequestClosed; an expired, answered-locally, or
// closed one is ErrGone. An allow for a request whose input was cut is
// ErrInputCut, and the request stays open.
func (s *Store) DecidePermission(ctx context.Context, id string, in api.DecisionIn, by string) (api.PermissionRequest, error) {
	if !ValidPermissionID(id) {
		return api.PermissionRequest{}, invalidf("request id %q: want pr_ and 22 base64url characters", id)
	}
	if in.Decision != api.DecisionAllow && in.Decision != api.DecisionDeny {
		return api.PermissionRequest{}, invalidf("decision %q: want allow or deny", in.Decision)
	}
	reason := strings.TrimSpace(in.Reason)
	if in.Decision == api.DecisionAllow {
		reason = "" // only a deny carries a reason
	}
	if err := checkText("reason", reason, MaxReasonRunes); err != nil {
		return api.PermissionRequest{}, err
	}
	if by == "" {
		return api.PermissionRequest{}, invalidf("decided_by is empty")
	}
	if err := checkText("decided_by", by, maxDecidedByRunes); err != nil {
		return api.PermissionRequest{}, err
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.PermissionRequest{}, err
	}
	defer tx.Rollback()
	if err := expirePermissionsTx(ctx, tx, now); err != nil {
		return api.PermissionRequest{}, err
	}
	var state, sessionID, tool, input string
	var truncated int
	err = tx.QueryRowContext(ctx, `SELECT state, session_id, tool_name, tool_input, truncated FROM permission_requests WHERE id = ?`, id).
		Scan(&state, &sessionID, &tool, &input, &truncated)
	if errors.Is(err, sql.ErrNoRows) {
		return api.PermissionRequest{}, fmt.Errorf("%w: request %s", ErrNotFound, id)
	}
	if err != nil {
		return api.PermissionRequest{}, err
	}
	switch state {
	case api.PermissionOpen:
	case api.PermissionDecided:
		return api.PermissionRequest{}, fmt.Errorf("%w: request %s is already decided", ErrRequestClosed, id)
	default:
		// Keep the expiry stored above even though this call fails.
		if err := tx.Commit(); err != nil {
			return api.PermissionRequest{}, err
		}
		return api.PermissionRequest{}, fmt.Errorf("%w: request %s is %s", ErrGone, id, strings.ReplaceAll(state, "_", " "))
	}
	if truncated != 0 && in.Decision == api.DecisionAllow {
		// Nobody can have read all of a cut input, so only the terminal,
		// which shows it whole, may allow it. A deny is still fine.
		if err := tx.Commit(); err != nil {
			return api.PermissionRequest{}, err
		}
		return api.PermissionRequest{}, ErrInputCut
	}
	nowS := formatTS(now)
	res, err := tx.ExecContext(ctx, `UPDATE permission_requests SET state = ?, decision = ?, reason = ?, decided_by = ?,
		decided_at = ?, updated_at = ? WHERE id = ? AND state = ?`, api.PermissionDecided, in.Decision, reason, by, nowS, nowS, id, api.PermissionOpen)
	if err != nil {
		return api.PermissionRequest{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return api.PermissionRequest{}, err
	} else if n != 1 {
		return api.PermissionRequest{}, fmt.Errorf("%w: request %s is already decided", ErrRequestClosed, id)
	}
	payload, err := json.Marshal(map[string]string{"request_id": id, "tool": tool,
		"input":    firstRunes(api.PermissionInputText(json.RawMessage(input)), eventTextRunes),
		"decision": in.Decision, "reason": reason, "by": by})
	if err != nil {
		return api.PermissionRequest{}, err
	}
	if err := insertEventTx(ctx, tx, sessionID, now, api.SourceServer, api.KindPermission, payload); err != nil {
		return api.PermissionRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.PermissionRequest{}, err
	}
	return s.GetPermission(ctx, id)
}

// openPermissions maps each session with an open, unexpired request to its
// newest one.
func (s *Store) openPermissions(ctx context.Context) (map[string]api.PermissionRequest, error) {
	now := s.Now()
	rows, err := s.db.QueryContext(ctx, permissionSelect+` WHERE p.state = ? AND p.expires_at > ?
		ORDER BY p.created_at, p.rowid`, api.PermissionOpen, formatTS(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]api.PermissionRequest{}
	for rows.Next() {
		p, _, err := scanPermission(rows, now)
		if err != nil {
			return nil, err
		}
		out[p.SessionID] = p // later rows are newer
	}
	return out, rows.Err()
}
