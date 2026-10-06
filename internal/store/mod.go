package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// The Claude Code mod's reports and its message claims. See docs/server.md,
// "The Claude Code mod".

// modFresh reports whether a mod poll at seen is within ControlPollWindow
// of now: the session's mod takes its messages.
func modFresh(seen *time.Time, now time.Time) bool {
	return seen != nil && now.Sub(*seen) <= ControlPollWindow
}

// modFreshSQL is true for a sessions row (aliased s) whose mod polled at or
// after its one argument, now minus ControlPollWindow.
const modFreshSQL = `s.mod_seen_at IS NOT NULL AND s.mod_seen_at >= ?`

// SetBlockedOn records what session id waits on, for machineID, which must
// own it. A non-empty text sets blocked_on and moves the agent state to
// blocked, unless the state is idle or done: the turn is over, so the
// question was answered, and nothing changes. An empty text clears blocked_on and, when the state is blocked
// with a stored blocked_on (the mod's own block), moves it to working. Any
// other state stays, so a clear that arrives after the turn ended, or after
// a permission prompt blocked the session again, does not undo it. Either
// way it records a blocked_on event and marks the session seen
// (last_seen_at). Only the message poll marks the mod seen (mod_seen_at): a
// mod whose message loop died must not keep the watcher away.
//
// A session that has ended is left as it is, and the call returns nil: a
// late report for a session that was cleared or closed must not revive it.
//
// The caller cleans text first (the server uses termtext.Clean); the store
// still refuses control characters and more than api.MaxBlockedOnRunes
// runes. Like an upsert, it sets the agent state but not state_ts: the mod
// has no event clock, and stamping server time would make a hooks event
// from a client clock a little behind lose to it.
func (s *Store) SetBlockedOn(ctx context.Context, machineID int64, id, text string) error {
	return s.setBlockedOn(ctx, machineID, id, text, "")
}

// ClearBlockedOn clears what session id waits on when it is question, the
// line a SetBlockedOn stored, and moves a blocked session to working. When
// the stored text is anything else, it changes nothing: the clear is late,
// and a newer question (or none) stands. The caller cleans question as it
// cleaned the text it set.
func (s *Store) ClearBlockedOn(ctx context.Context, machineID int64, id, question string) error {
	question = strings.TrimSpace(question)
	if question == "" {
		return invalidf("clears is empty")
	}
	return s.setBlockedOn(ctx, machineID, id, "", question)
}

func (s *Store) setBlockedOn(ctx context.Context, machineID int64, id, text, clears string) error {
	text = strings.TrimSpace(text)
	if err := checkText("text", text, api.MaxBlockedOnRunes); err != nil {
		return err
	}
	if err := checkText("clears", clears, api.MaxBlockedOnRunes); err != nil {
		return err
	}
	now := s.Now()
	nowS := formatTS(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkOwnerTx(ctx, tx, machineID, id); err != nil {
		return err
	}
	var prev, prevOn string
	var ended sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT agent_state, blocked_on, ended_at FROM sessions WHERE id = ?`, id).
		Scan(&prev, &prevOn, &ended); err != nil {
		return err
	}
	switch {
	case ended.Valid:
		return nil
	case text != "" && (prev == "idle" || prev == "done"):
		// The turn is over, so the question was answered: a set that
		// arrives now is late and would block a finished session.
		return nil
	case clears != "" && prevOn != clears:
		// A newer question, or none: not the one this clear is for.
		return nil
	}
	q := `UPDATE sessions SET blocked_on = ?, last_seen_at = ?`
	args := []any{text, nowS}
	state := ""
	switch {
	case text != "":
		state = "blocked"
	case prev == "blocked" && prevOn != "":
		state = "working" // a clear of the mod's own block
	}
	if state != "" {
		q += ", agent_state = ?, turn_ended_at = " + turnEndedExpr + ", blocked_at = " + blockedAtExpr
		args = append(args, state, state, nowS, state, nowS)
	}
	args = append(args, id)
	if _, err := tx.ExecContext(ctx, q+" WHERE id = ?", args...); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]string{"text": firstRunes(text, eventTextRunes)})
	if err != nil {
		return err
	}
	if err := insertEventTx(ctx, tx, id, now, api.SourceMod, api.KindBlockedOn, payload); err != nil {
		return err
	}
	if prev == "blocked" && state == "working" {
		// The question was answered. As for an upsert, the newest permission
		// requests stay open: they may be a prompt that just arrived.
		if err := closePermissionsTx(ctx, tx, id, api.PermissionAnsweredLocally, now.Add(-PermissionGrace), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetUsage stores the context window fill and the cost a session's mod
// reported after a turn, and the time it did, and marks the session seen
// (last_seen_at, not mod_seen_at). A nil field keeps the stored value; at least one must be set.
// machineID must own the session. Like SetBlockedOn, it leaves an ended
// session as it is and returns nil.
func (s *Store) SetUsage(ctx context.Context, machineID int64, id string, in api.UsageIn) error {
	if in.ContextPercent == nil && in.CostUSD == nil {
		return invalidf("context_percent and cost_usd are both missing")
	}
	if p := in.ContextPercent; p != nil && (*p < 0 || *p > 100) {
		return invalidf("context_percent %d: want 0 to 100", *p)
	}
	if c := in.CostUSD; c != nil && (math.IsNaN(*c) || *c < 0 || *c > api.MaxLiveCostUSD) {
		return invalidf("cost_usd %v: want 0 to %d", *c, api.MaxLiveCostUSD)
	}
	var pct, cost any
	if in.ContextPercent != nil {
		pct = *in.ContextPercent
	}
	if in.CostUSD != nil {
		cost = *in.CostUSD
	}
	nowS := formatTS(s.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkOwnerTx(ctx, tx, machineID, id); err != nil {
		return err
	}
	// An ended session matches no row and stays as it is.
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET context_percent = COALESCE(?, context_percent),
		live_cost_usd = COALESCE(?, live_cost_usd), usage_at = ?, last_seen_at = ?
		WHERE id = ? AND ended_at IS NULL`,
		pct, cost, nowS, nowS, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordModPoll checks that machineID owns session id and records a mod's
// message poll in mod_seen_at, as ClaimSessionMessage does. The server calls
// it before a long poll's wait starts, so a send right after the poll began
// finds the session messageable.
func (s *Store) RecordModPoll(ctx context.Context, machineID int64, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkOwnerTx(ctx, tx, machineID, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET mod_seen_at = ? WHERE id = ?`, formatTS(s.Now()), id); err != nil {
		return err
	}
	return tx.Commit()
}

// ClaimSessionMessage is a mod's message poll for session id, which
// machineID must own. It records the poll in mod_seen_at, which makes the
// session messageable and keeps the watcher's ClaimMessage away from its
// messages for ControlPollWindow, then hands out the session's oldest queued
// message when it was never offered or was last offered at least
// MessageRetry ago. Messages go in order, as for ClaimMessage. ok is false
// when there is none; mod_seen_at is set either way.
func (s *Store) ClaimSessionMessage(ctx context.Context, machineID int64, id string) (api.ModMessage, bool, error) {
	now := s.Now()
	nowS := formatTS(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ModMessage{}, false, err
	}
	defer tx.Rollback()
	if err := checkOwnerTx(ctx, tx, machineID, id); err != nil {
		return api.ModMessage{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET mod_seen_at = ? WHERE id = ?`, nowS, id); err != nil {
		return api.ModMessage{}, false, err
	}
	if err := expireMessagesTx(ctx, tx, now); err != nil {
		return api.ModMessage{}, false, err
	}
	var mid, text, sender string
	var offered sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id, text, sender, offered_at FROM messages
		WHERE session_id = ? AND machine_id = ? AND state = ? ORDER BY created_at, rowid LIMIT 1`,
		id, machineID, api.MessageQueued).Scan(&mid, &text, &sender, &offered)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && offered.Valid && offered.String > formatTS(now.Add(-MessageRetry))) {
		return api.ModMessage{}, false, tx.Commit()
	}
	if err != nil {
		return api.ModMessage{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET offered_at = ?, updated_at = ? WHERE id = ?`, nowS, nowS, mid); err != nil {
		return api.ModMessage{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return api.ModMessage{}, false, err
	}
	return api.ModMessage{ID: mid, Text: api.MessagePrompt(sender, text)}, true, nil
}

// NextSessionMessageRetry is NextMessageRetry for one session's mod: how
// long until the oldest queued message of session id that a mod or watcher
// already saw is offered again, and false when there is none. It is at
// least one second.
func (s *Store) NextSessionMessageRetry(ctx context.Context, machineID int64, id string) (time.Duration, bool, error) {
	now := s.Now()
	var offered sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT MIN(offered_at) FROM messages
		WHERE session_id = ? AND machine_id = ? AND state = ? AND offered_at IS NOT NULL AND created_at > ?`,
		id, machineID, api.MessageQueued, formatTS(now.Add(-MessageTTL))).Scan(&offered)
	if err != nil || !offered.Valid {
		return 0, false, err
	}
	at, err := parseTS(offered.String)
	if err != nil {
		return 0, false, err
	}
	return max(at.Add(MessageRetry).Sub(now), time.Second), true, nil
}

// FinishModMessage records a mod's result for a message of machineID:
// delivered, busy (still queued, offered again after MessageRetry), or
// refused. The transitions are FinishMessage's; unlike a watcher's result,
// failed is not accepted. It leaves mod_seen_at alone.
func (s *Store) FinishModMessage(ctx context.Context, machineID int64, id string, in api.MessageResultIn) (api.Message, error) {
	switch in.State {
	case api.MessageDelivered, api.MessageBusy, api.MessageRefused:
	default:
		return api.Message{}, invalidf("state %q: want delivered, busy, or refused", in.State)
	}
	return s.FinishMessage(ctx, machineID, id, api.ControlResultIn{State: in.State, Detail: in.Detail})
}
