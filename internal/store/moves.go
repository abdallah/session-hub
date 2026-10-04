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
	"github.com/abdallah/session-hub/internal/move"
)

// Move timeouts. See docs/server.md, "Moves".
const (
	// MoveRequestTTL is how long a move waits for its source to claim it.
	MoveRequestTTL = 2 * time.Minute
	// MoveStepTTL is how long a move may stay packing, uploaded, or
	// unpacking since its last change.
	MoveStepTTL = 10 * time.Minute
	maxByRunes  = 100
)

var moveIDRE = regexp.MustCompile(`^mv_[A-Za-z0-9_-]{22}$`)

// sourceNoteTx appends in.Detail to the detail of move id when machineID is
// its source, the move ended in in.State, and the source's part is closed.
// It is how the source reports a problem after that, such as a restart or an
// archive that failed. A note the detail already ends with is taken without
// a change, so a retried result is not added twice. It reports whether it
// took the note.
func sourceNoteTx(ctx context.Context, tx *sql.Tx, machineID int64, id string, in api.MoveResultIn, nowS string) (bool, error) {
	if in.Detail == "" || in.CloudURL != "" {
		return false, nil
	}
	var detail string
	// A cancelled move reads like a failed one, so it takes a failed note.
	states := []any{in.State, in.State}
	if in.State == api.MoveFailed {
		states[1] = api.MoveCancelled
	}
	err := tx.QueryRowContext(ctx, `SELECT detail FROM moves WHERE id = ? AND source_machine_id = ? AND state IN (?, ?)
		AND source_closed_at IS NOT NULL`, id, machineID, states[0], states[1]).Scan(&detail)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// A retry of a result whose answer was lost repeats the same detail:
	// take it, and change nothing.
	if detail == in.Detail || strings.HasSuffix(detail, "; "+in.Detail) {
		return true, nil
	}
	if detail != "" {
		detail += "; "
	}
	detail += in.Detail
	if r := []rune(detail); len(r) > api.MaxMoveDetailRunes {
		detail = string(r[:api.MaxMoveDetailRunes])
	}
	if _, err := tx.ExecContext(ctx, `UPDATE moves SET detail = ?, updated_at = ? WHERE id = ?`, detail, nowS, id); err != nil {
		return false, err
	}
	return true, nil
}

// openMoveTx returns the open move of session sessionID (requested,
// packing, uploaded, or unpacking), if it has one.
func openMoveTx(ctx context.Context, tx *sql.Tx, sessionID string) (id, state string, open bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT id, state FROM moves WHERE session_id = ? AND state IN (?, ?, ?, ?)
		ORDER BY created_at DESC LIMIT 1`, sessionID, api.MoveRequested, api.MovePacking, api.MoveUploaded, api.MoveUnpacking).
		Scan(&id, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return id, state, true, nil
}

// ValidMoveID reports whether id has the shape of a move ID.
func ValidMoveID(id string) bool { return moveIDRE.MatchString(id) }

func checkMoveID(id string) error {
	if !ValidMoveID(id) {
		return invalidf("move id %q: want mv_ and 22 base64url characters", id)
	}
	return nil
}

// refusef is a 409 with a reason a person can act on.
func refusef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotControllable, fmt.Sprintf(format, args...))
}

// SetMoveKey stores machineID's move public key, replacing any earlier one.
func (s *Store) SetMoveKey(ctx context.Context, machineID int64, key string) error {
	if _, err := api.ParseMoveKey(key); err != nil {
		return invalidf("%v", err)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE machines SET move_key = ? WHERE id = ?`, key, machineID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: machine %d", ErrNotFound, machineID)
	}
	return nil
}

// moveSelect reads a move and its source machine's name.
const moveSelect = `SELECT mv.id, mv.session_id, ms.name, mv.target, mv.state, mv.detail, mv.cloud_url,
	mv.requested_by, mv.bundle_size, mv.created_at, mv.updated_at
FROM moves mv JOIN machines ms ON ms.id = mv.source_machine_id`

func scanMove(sc scanner) (api.Move, error) {
	var m api.Move
	var created, updated string
	if err := sc.Scan(&m.ID, &m.SessionID, &m.Source, &m.Target, &m.State, &m.Detail, &m.CloudURL,
		&m.By, &m.BundleSize, &created, &updated); err != nil {
		return api.Move{}, err
	}
	var err error
	if m.CreatedAt, err = parseTS(created); err != nil {
		return api.Move{}, err
	}
	if m.UpdatedAt, err = parseTS(updated); err != nil {
		return api.Move{}, err
	}
	return m, nil
}

// nullMove is a moves row read through a LEFT JOIN.
type nullMove struct {
	id, target, state, detail, cloudURL, by, created, updated, source sql.NullString
	size                                                              sql.NullInt64
}

// move returns the row as a move, or nil when the join found none.
func (n nullMove) move(sessionID string) (*api.Move, error) {
	if !n.id.Valid {
		return nil, nil
	}
	m := api.Move{ID: n.id.String, SessionID: sessionID, Source: n.source.String, Target: n.target.String,
		State: n.state.String, Detail: n.detail.String, CloudURL: n.cloudURL.String, By: n.by.String, BundleSize: n.size.Int64}
	var err error
	if m.CreatedAt, err = parseTS(n.created.String); err != nil {
		return nil, err
	}
	if m.UpdatedAt, err = parseTS(n.updated.String); err != nil {
		return nil, err
	}
	return &m, nil
}

// polled reports whether a machine's last poll is within ControlPollWindow.
func polled(lastPoll sql.NullString, now time.Time) (bool, error) {
	t, err := parseNullTS(lastPoll)
	if err != nil || t == nil {
		return false, err
	}
	return now.Sub(*t) <= ControlPollWindow, nil
}

// expireMovesTx fails every move past its timeout: requested for
// MoveRequestTTL (the source never acted, so its part is closed too), or
// packing, uploaded, or unpacking for MoveStepTTL since its last change. It
// returns the IDs it failed.
func expireMovesTx(ctx context.Context, tx *sql.Tx, now time.Time) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, state FROM moves
		WHERE (state = ? AND updated_at <= ?) OR (state IN (?, ?, ?) AND updated_at <= ?)`,
		api.MoveRequested, formatTS(now.Add(-MoveRequestTTL)),
		api.MovePacking, api.MoveUploaded, api.MoveUnpacking, formatTS(now.Add(-MoveStepTTL)))
	if err != nil {
		return nil, err
	}
	type old struct{ id, state string }
	var list []old
	for rows.Next() {
		var o old
		if err := rows.Scan(&o.id, &o.state); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	nowS := formatTS(now)
	var ids []string
	for _, o := range list {
		closed := sql.NullString{}
		if o.state == api.MoveRequested {
			closed = sql.NullString{String: nowS, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE moves SET state = ?, detail = ?, updated_at = ?,
			source_closed_at = COALESCE(source_closed_at, ?) WHERE id = ?`,
			api.MoveFailed, o.state+": timed out", nowS, closed, o.id); err != nil {
			return nil, err
		}
		ids = append(ids, o.id)
	}
	return ids, nil
}

// ExpireMoves fails every move past its timeout and returns them as they
// are now. The server deletes their bundles and wakes their sources.
func (s *Store) ExpireMoves(ctx context.Context) ([]api.Move, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids, err := expireMovesTx(ctx, tx, s.Now())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	var out []api.Move
	for _, id := range ids {
		m, err := s.GetMove(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// GetMove reads one move.
func (s *Store) GetMove(ctx context.Context, id string) (api.Move, error) {
	if err := checkMoveID(id); err != nil {
		return api.Move{}, err
	}
	m, err := scanMove(s.db.QueryRowContext(ctx, moveSelect+` WHERE mv.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return api.Move{}, fmt.Errorf("%w: move %s", ErrNotFound, id)
	}
	return m, err
}

// CreateMove opens a move of session sessionID to target (a machine name or
// api.MoveCloud), started by by (web:<name> or machine:<name>). Every rule
// in docs/server.md, "Moves", that fails is ErrNotControllable with the
// reason; an unknown session is ErrNotFound.
func (s *Store) CreateMove(ctx context.Context, sessionID, target, by string) (api.Move, error) {
	if !ValidSessionID(sessionID) {
		return api.Move{}, invalidf("session id %q: use 1-128 letters, digits, '_', or '-', starting with a letter or digit", sessionID)
	}
	if target != api.MoveCloud && !ValidMachineName(target) {
		return api.Move{}, invalidf("target %q: want a machine name or cloud", target)
	}
	if !strings.HasPrefix(by, "web:") && !strings.HasPrefix(by, api.RequestedByMachinePrefix) {
		return api.Move{}, invalidf("by %q: want web:<name> or machine:<name>", by)
	}
	if err := checkText("by", by, maxByRunes); err != nil {
		return api.Move{}, err
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Move{}, err
	}
	defer tx.Rollback()
	if _, err := expireMovesTx(ctx, tx, now); err != nil {
		return api.Move{}, err
	}
	var srcID int64
	var srcName, srcKey, pane, agentState, gitRepo string
	var ended, srcPoll sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT s.machine_id, m.name, m.move_key, s.herdr_pane, s.agent_state, s.git_repo,
		s.ended_at, m.last_poll FROM sessions s JOIN machines m ON m.id = s.machine_id WHERE s.id = ?`, sessionID).
		Scan(&srcID, &srcName, &srcKey, &pane, &agentState, &gitRepo, &ended, &srcPoll)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Move{}, fmt.Errorf("%w: session %s", ErrNotFound, sessionID)
	}
	if err != nil {
		return api.Move{}, err
	}
	switch {
	case ended.Valid:
		return api.Move{}, refusef("session %s has ended", sessionID)
	case pane == "":
		return api.Move{}, refusef("session %s is not in herdr", sessionID)
	case agentState != "idle" && agentState != "done":
		state := agentState
		if state == "" {
			state = "in an unknown state"
		}
		return api.Move{}, refusef("the agent is %s; a session moves only when it is idle or done", state)
	}
	if ok, err := polled(srcPoll, now); err != nil {
		return api.Move{}, err
	} else if !ok {
		return api.Move{}, refusef("the sessionhub watcher on %q is offline", srcName)
	}
	if srcKey == "" {
		return api.Move{}, refusef("machine %q has no move key yet; run sessionhub install-plugin there", srcName)
	}
	if openID, openState, open, err := openMoveTx(ctx, tx, sessionID); err != nil {
		return api.Move{}, err
	} else if open {
		return api.Move{}, refusef("move %s of this session is %s", openID, openState)
	}
	var targetID sql.NullInt64
	if target == api.MoveCloud {
		if !move.IsGitHub(gitRepo) {
			return api.Move{}, refusef("a cloud move needs a GitHub remote; this session's remote is %q", gitRepo)
		}
	} else {
		var tid int64
		var tkey string
		var tpoll sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id, move_key, last_poll FROM machines WHERE name = ?`, target).Scan(&tid, &tkey, &tpoll)
		if errors.Is(err, sql.ErrNoRows) {
			return api.Move{}, refusef("no machine named %q", target)
		}
		if err != nil {
			return api.Move{}, err
		}
		if tid == srcID {
			return api.Move{}, refusef("session %s is already on %q", sessionID, target)
		}
		if ok, err := polled(tpoll, now); err != nil {
			return api.Move{}, err
		} else if !ok {
			return api.Move{}, refusef("the sessionhub watcher on %q is offline", target)
		}
		if tkey == "" {
			return api.Move{}, refusef("machine %q has no move key yet; run sessionhub install-plugin there", target)
		}
		targetID = sql.NullInt64{Int64: tid, Valid: true}
	}
	id, err := newRandomID("mv_")
	if err != nil {
		return api.Move{}, err
	}
	nowS := formatTS(now)
	if _, err := tx.ExecContext(ctx, `INSERT INTO moves (id, session_id, source_machine_id, target, target_machine_id,
		state, created_at, updated_at, requested_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sessionID, srcID, target, targetID, api.MoveRequested, nowS, nowS, by); err != nil {
		return api.Move{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Move{}, err
	}
	return api.Move{ID: id, SessionID: sessionID, Source: srcName, Target: target, State: api.MoveRequested,
		By: by, CreatedAt: now, UpdatedAt: now}, nil
}

// ClaimMove hands machineID its next move step, oldest first: a requested
// move it is the source of (now packing, action move-out, with the target's
// public key), else an uploaded move it is the target of (now unpacking,
// action move-in, with the source's public key), else an ended move whose
// source still owes its finish (action move-out with the final state; the
// finish is marked given). ok is false when there is none.
func (s *Store) ClaimMove(ctx context.Context, machineID int64) (api.ControlClaim, bool, error) {
	now := s.Now()
	nowS := formatTS(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	defer tx.Rollback()
	if _, err := expireMovesTx(ctx, tx, now); err != nil {
		return api.ControlClaim{}, false, err
	}
	var id, peerKey, action string
	err = tx.QueryRowContext(ctx, `SELECT mv.id, COALESCE(mt.move_key, '') FROM moves mv
		LEFT JOIN machines mt ON mt.id = mv.target_machine_id
		WHERE mv.source_machine_id = ? AND mv.state = ? ORDER BY mv.created_at, mv.rowid LIMIT 1`,
		machineID, api.MoveRequested).Scan(&id, &peerKey)
	if err == nil {
		action = api.ActionMoveOut
		_, err = tx.ExecContext(ctx, `UPDATE moves SET state = ?, updated_at = ? WHERE id = ?`, api.MovePacking, nowS, id)
	} else if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT mv.id, ms.move_key FROM moves mv JOIN machines ms ON ms.id = mv.source_machine_id
			WHERE mv.target_machine_id = ? AND mv.state = ? ORDER BY mv.created_at, mv.rowid LIMIT 1`,
			machineID, api.MoveUploaded).Scan(&id, &peerKey)
		if err == nil {
			action = api.ActionMoveIn
			_, err = tx.ExecContext(ctx, `UPDATE moves SET state = ?, updated_at = ? WHERE id = ?`, api.MoveUnpacking, nowS, id)
		} else if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `SELECT id FROM moves WHERE source_machine_id = ? AND state IN (?, ?, ?)
				AND source_closed_at IS NULL ORDER BY updated_at, rowid LIMIT 1`,
				machineID, api.MoveDone, api.MoveFailed, api.MoveCancelled).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return api.ControlClaim{}, false, tx.Commit()
			}
			if err == nil {
				action, peerKey = api.ActionMoveOut, ""
				_, err = tx.ExecContext(ctx, `UPDATE moves SET source_closed_at = ? WHERE id = ?`, nowS, id)
			}
		}
	}
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return api.ControlClaim{}, false, err
	}
	// GetMove and GetSession use s.db, so they run after the commit.
	mv, err := s.GetMove(ctx, id)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	sess, err := s.GetSession(ctx, mv.SessionID)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	machine := mv.Source
	if action == api.ActionMoveIn {
		machine = mv.Target
	}
	req := api.ControlRequest{ID: mv.ID, SessionID: mv.SessionID, Machine: machine, Action: action, State: api.ControlClaimed,
		RequestedBy: mv.By, CreatedAt: mv.CreatedAt, ExpiresAt: mv.UpdatedAt.Add(MoveStepTTL), ClaimedAt: &now}
	return api.ControlClaim{Request: req, Session: sess, Move: &api.MoveClaim{ID: mv.ID, Source: mv.Source,
		Target: mv.Target, State: mv.State, PeerKey: peerKey, Detail: mv.Detail}}, true, nil
}

// moveRow is what the step checks read.
type moveRow struct {
	sessionID string
	source    int64
	target    sql.NullInt64 // invalid for a cloud move
	state     string
}

// stepTx reads move id after expiring old moves, and checks that it is
// machineID's step: the source while packing, or the target while
// unpacking. A machine whose step it is not is ErrWrongMachine; the machine
// whose step it is, in another state, or either machine once the move
// ended, is ErrRequestClosed.
func stepTx(ctx context.Context, tx *sql.Tx, machineID int64, id string, now time.Time) (moveRow, error) {
	if _, err := expireMovesTx(ctx, tx, now); err != nil {
		return moveRow{}, err
	}
	var r moveRow
	err := tx.QueryRowContext(ctx, `SELECT session_id, source_machine_id, target_machine_id, state FROM moves WHERE id = ?`, id).
		Scan(&r.sessionID, &r.source, &r.target, &r.state)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("%w: move %s", ErrNotFound, id)
	}
	if err != nil {
		return r, err
	}
	isSource := r.source == machineID
	isTarget := r.target.Valid && r.target.Int64 == machineID
	// The source acts while requested or packing, the target while uploaded
	// or unpacking.
	stepOf := r.source
	if (r.state == api.MoveUploaded || r.state == api.MoveUnpacking) && r.target.Valid {
		stepOf = r.target.Int64
	}
	switch {
	case r.state == api.MovePacking && isSource, r.state == api.MoveUnpacking && isTarget:
		return r, nil
	case (isSource || isTarget) && !api.MoveOpen(r.state), machineID == stepOf:
		return r, fmt.Errorf("%w: move %s is %s, not this machine's step", ErrRequestClosed, id, r.state)
	}
	return r, fmt.Errorf("%w: move %s: this machine has no step in it now", ErrWrongMachine, id)
}

// MoveForUpload checks that machineID may upload move id's bundle now: it
// is the source of a machine move that is packing.
func (s *Store) MoveForUpload(ctx context.Context, machineID int64, id string) (api.Move, error) {
	if err := checkMoveID(id); err != nil {
		return api.Move{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Move{}, err
	}
	defer tx.Rollback()
	r, err := stepTx(ctx, tx, machineID, id, s.Now())
	if err != nil {
		return api.Move{}, err
	}
	if r.source != machineID || !r.target.Valid {
		return api.Move{}, fmt.Errorf("%w: move %s takes no bundle from this machine", ErrRequestClosed, id)
	}
	if err := tx.Commit(); err != nil {
		return api.Move{}, err
	}
	return s.GetMove(ctx, id)
}

// MoveUploaded records that move id's sealed bundle of size bytes is
// stored: the move becomes uploaded, ready for the target's claim.
func (s *Store) MoveUploaded(ctx context.Context, machineID int64, id string, size int64) (api.Move, error) {
	if err := checkMoveID(id); err != nil {
		return api.Move{}, err
	}
	if size <= 0 || size > api.MaxSealedBundle {
		return api.Move{}, invalidf("bundle size %d: want 1 to %d bytes", size, api.MaxSealedBundle)
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Move{}, err
	}
	defer tx.Rollback()
	r, err := stepTx(ctx, tx, machineID, id, now)
	if err != nil {
		return api.Move{}, err
	}
	if r.source != machineID || !r.target.Valid {
		return api.Move{}, fmt.Errorf("%w: move %s takes no bundle from this machine", ErrRequestClosed, id)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE moves SET state = ?, bundle_size = ?, updated_at = ? WHERE id = ?`,
		api.MoveUploaded, size, formatTS(now), id); err != nil {
		return api.Move{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Move{}, err
	}
	return s.GetMove(ctx, id)
}

// MoveForDownload checks that machineID may download move id's bundle now:
// it is the target and the move is unpacking.
func (s *Store) MoveForDownload(ctx context.Context, machineID int64, id string) (api.Move, error) {
	if err := checkMoveID(id); err != nil {
		return api.Move{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Move{}, err
	}
	defer tx.Rollback()
	r, err := stepTx(ctx, tx, machineID, id, s.Now())
	if err != nil {
		return api.Move{}, err
	}
	if r.state != api.MoveUnpacking {
		return api.Move{}, fmt.Errorf("%w: move %s is %s, not unpacking", ErrRequestClosed, id, r.state)
	}
	if err := tx.Commit(); err != nil {
		return api.Move{}, err
	}
	return s.GetMove(ctx, id)
}

// FinishMove records the result of machineID's step of move id: done or
// failed. The source may fail a packing move, or finish a cloud move done
// with its link; the target may finish or fail an unpacking move. A
// machine move's done hands the session to the target; a cloud move's done
// ends it. A result the source posts closes its part, so it gets no finish.
// Once the move ended and the source's part closed, the source may post the
// final state again with a detail, which is added to the move's detail
// (sourceNoteTx).
func (s *Store) FinishMove(ctx context.Context, machineID int64, id string, in api.MoveResultIn) (api.Move, error) {
	if err := checkMoveID(id); err != nil {
		return api.Move{}, err
	}
	if in.State != api.MoveDone && in.State != api.MoveFailed {
		return api.Move{}, invalidf("state %q: want done or failed", in.State)
	}
	if err := checkText("detail", in.Detail, api.MaxMoveDetailRunes); err != nil {
		return api.Move{}, err
	}
	if in.CloudURL != "" && (in.State != api.MoveDone || !ValidRemoteControlURL(in.CloudURL)) {
		return api.Move{}, invalidf("cloud_url %q: want https://claude.ai/code/session_<id>, on a done result only", in.CloudURL)
	}
	now := s.Now()
	nowS := formatTS(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Move{}, err
	}
	defer tx.Rollback()
	r, err := stepTx(ctx, tx, machineID, id, now)
	if errors.Is(err, ErrRequestClosed) {
		noted, nerr := sourceNoteTx(ctx, tx, machineID, id, in, nowS)
		if nerr != nil {
			return api.Move{}, nerr
		}
		if noted {
			if err := tx.Commit(); err != nil {
				return api.Move{}, err
			}
			return s.GetMove(ctx, id)
		}
	}
	if err != nil {
		return api.Move{}, err
	}
	cloud := !r.target.Valid
	bySource := r.state == api.MovePacking
	switch {
	case in.State == api.MoveDone && bySource && !cloud:
		return api.Move{}, fmt.Errorf("%w: move %s is done only when its target reports it", ErrRequestClosed, id)
	case in.State == api.MoveDone && cloud && in.CloudURL == "":
		return api.Move{}, invalidf("cloud_url: a cloud move's done carries the cloud session link")
	case in.CloudURL != "" && !cloud:
		return api.Move{}, invalidf("cloud_url: only a cloud move has a link")
	}
	closed := sql.NullString{}
	if bySource {
		closed = sql.NullString{String: nowS, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE moves SET state = ?, detail = ?, cloud_url = ?, updated_at = ?,
		source_closed_at = COALESCE(source_closed_at, ?) WHERE id = ?`,
		in.State, in.Detail, in.CloudURL, nowS, closed, id); err != nil {
		return api.Move{}, err
	}
	if in.State == api.MoveDone {
		var from, to string
		if err := tx.QueryRowContext(ctx, `SELECT ms.name, mv.target FROM moves mv JOIN machines ms ON ms.id = mv.source_machine_id
			WHERE mv.id = ?`, id).Scan(&from, &to); err != nil {
			return api.Move{}, err
		}
		p := map[string]string{"move_id": id, "from": from, "to": to}
		if cloud {
			p["cloud_url"] = in.CloudURL
			// The pane goes too, so no Remote Control resumes the local
			// copy while the cloud session works on it.
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at = COALESCE(ended_at, ?), rc_url = '', rc_at = NULL,
				herdr_session = '', herdr_workspace = '', herdr_pane = '' WHERE id = ?`, nowS, r.sessionID); err != nil {
				return api.Move{}, err
			}
			ended, _ := json.Marshal(map[string]string{"reason": api.EndedMovedToCloud})
			if err := insertEventTx(ctx, tx, r.sessionID, now, api.SourceServer, api.KindEnded, ended); err != nil {
				return api.Move{}, err
			}
			if err := closePermissionsTx(ctx, tx, r.sessionID, api.PermissionClosed, now, now); err != nil {
				return api.Move{}, err
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE sessions SET machine_id = ?, herdr_session = '', herdr_workspace = '',
			herdr_pane = '', rc_url = '', rc_at = NULL WHERE id = ?`, r.target.Int64, r.sessionID); err != nil {
			return api.Move{}, err
		}
		payload, err := json.Marshal(p)
		if err != nil {
			return api.Move{}, err
		}
		if err := insertEventTx(ctx, tx, r.sessionID, now, api.SourceServer, api.KindMoved, payload); err != nil {
			return api.Move{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return api.Move{}, err
	}
	return s.GetMove(ctx, id)
}
