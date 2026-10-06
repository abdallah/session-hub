package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
)

// Message limits. See docs/server.md, "Messages to sessions".
const (
	MaxMessageTargets = 20
	MaxMessageRunes   = 4000
	// MessageTTL is how long a message stays queued.
	MessageTTL = 10 * time.Minute
	// MessageRetry is how long after an offer a queued message is offered
	// again: after a busy result, or a watcher that never answered.
	MessageRetry = 15 * time.Second
	// MessagesPerMinute caps one sender's messages in any 60 seconds.
	MessagesPerMinute = 30
	maxSenderRunes    = 100
	// eventTextRunes is how much of a text a message or permission event
	// keeps.
	eventTextRunes = 200
)

var messageIDRE = regexp.MustCompile(`^msg_[A-Za-z0-9_-]{22}$`)

// ValidMessageID reports whether id has the shape of a message ID.
func ValidMessageID(id string) bool { return messageIDRE.MatchString(id) }

// newRandomID is prefix followed by 16 random bytes in base64url.
func newRandomID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// firstRunes cuts s to at most n runes.
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// checkMessageText allows newlines and tabs, unlike checkText: a message is
// prose a person wrote.
func checkMessageText(text string) error {
	if strings.TrimSpace(text) == "" {
		return invalidf("text is empty")
	}
	if n := utf8.RuneCountInString(text); n > MaxMessageRunes {
		return invalidf("text is %d characters, at most %d allowed", n, MaxMessageRunes)
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return invalidf("text contains a control character (U+%04X), which is not allowed", r)
		}
	}
	return nil
}

func messageEventTx(ctx context.Context, tx *sql.Tx, sessionID, id, sender, state, text string, now time.Time) error {
	payload, err := json.Marshal(map[string]string{"message_id": id, "sender": sender, "state": state,
		"text": firstRunes(text, eventTextRunes)})
	if err != nil {
		return err
	}
	return insertEventTx(ctx, tx, sessionID, now, api.SourceServer, api.KindMessage, payload)
}

// expireMessagesTx stores the expiry of every queued message older than
// MessageTTL and records its event. Sends, claims, and results run it
// first; GetMessage reports the expiry without storing it.
func expireMessagesTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, session_id, sender, text FROM messages WHERE state = ? AND created_at <= ?`,
		api.MessageQueued, formatTS(now.Add(-MessageTTL)))
	if err != nil {
		return err
	}
	type old struct{ id, session, sender, text string }
	var list []old
	for rows.Next() {
		var o old
		if err := rows.Scan(&o.id, &o.session, &o.sender, &o.text); err != nil {
			rows.Close()
			return err
		}
		list = append(list, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range list {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET state = ?, detail = ?, updated_at = ? WHERE id = ?`,
			api.MessageExpired, "not delivered within 10 minutes", formatTS(now), o.id); err != nil {
			return err
		}
		if err := messageEventTx(ctx, tx, o.session, o.id, o.sender, api.MessageExpired, o.text, now); err != nil {
			return err
		}
	}
	return nil
}

// SendMessages queues text for each distinct session in ids, in order. A
// target that does not exist or has ended is refused with a reason, and so
// is one whose mod has not polled in ControlPollWindow and that has no herdr
// pane or whose machine's watcher has not polled in ControlPollWindow. The
// others still go. machines lists the machines that got a
// queued message, once each. sender is the name shown in the prompt;
// limitKey names who is counted (web:<name> or machine:<name>), so changing
// the display sender does not buy a new budget. More than MessagesPerMinute
// messages under one limitKey in 60 seconds is ErrTooMany for the whole
// send. The count is the key's stored messages plus this send's distinct targets; refused targets
// of earlier sends are not stored, so they do not count. The text is trimmed
// before it is checked and stored.
func (s *Store) SendMessages(ctx context.Context, ids []string, text, sender, limitKey string) ([]api.MessageResult, []string, error) {
	var uniq []string
	seen := map[string]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 || len(uniq) > MaxMessageTargets {
		return nil, nil, invalidf("session_ids has %d distinct sessions, want 1 to %d", len(uniq), MaxMessageTargets)
	}
	text = strings.TrimSpace(text)
	if err := checkMessageText(text); err != nil {
		return nil, nil, err
	}
	if sender == "" || limitKey == "" {
		return nil, nil, invalidf("sender and limit key are empty")
	}
	if err := checkText("sender", sender, maxSenderRunes); err != nil {
		return nil, nil, err
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if err := expireMessagesTx(ctx, tx, now); err != nil {
		return nil, nil, err
	}
	var recent int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE limit_key = ? AND created_at > ?`,
		limitKey, formatTS(now.Add(-time.Minute))).Scan(&recent); err != nil {
		return nil, nil, err
	}
	if recent+len(uniq) > MessagesPerMinute {
		return nil, nil, fmt.Errorf("%w: %q sent %d messages in the last minute, and at most %d are allowed",
			ErrTooMany, limitKey, recent, MessagesPerMinute)
	}
	results := make([]api.MessageResult, 0, len(uniq))
	var machines []string
	for _, id := range uniq {
		r := api.MessageResult{SessionID: id, State: api.MessageRefused}
		reason, machineID, machine, err := messageTargetTx(ctx, tx, id, now)
		if err != nil {
			return nil, nil, err
		}
		if reason != "" {
			r.Detail = reason
			results = append(results, r)
			continue
		}
		mid, err := newRandomID("msg_")
		if err != nil {
			return nil, nil, err
		}
		nowS := formatTS(now)
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, session_id, machine_id, text, sender, limit_key, state,
			created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			mid, id, machineID, text, sender, limitKey, api.MessageQueued, nowS, nowS); err != nil {
			return nil, nil, err
		}
		r.ID, r.State = mid, api.MessageQueued
		results = append(results, r)
		if !slices.Contains(machines, machine) {
			machines = append(machines, machine)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return results, machines, nil
}

// messageTargetTx checks one target. reason is "" when a message can be
// queued for it.
func messageTargetTx(ctx context.Context, tx *sql.Tx, id string, now time.Time) (reason string, machineID int64, machine string, err error) {
	if !ValidSessionID(id) {
		return "invalid session id", 0, "", nil
	}
	var pane string
	var ended, lastPoll, modSeen sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT s.machine_id, m.name, s.herdr_pane, s.ended_at, m.last_poll, s.mod_seen_at
		FROM sessions s JOIN machines m ON m.id = s.machine_id WHERE s.id = ?`, id).
		Scan(&machineID, &machine, &pane, &ended, &lastPoll, &modSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return "unknown session", 0, "", nil
	}
	if err != nil {
		return "", 0, "", err
	}
	seen, err := parseNullTS(modSeen)
	if err != nil {
		return "", 0, "", err
	}
	switch {
	case ended.Valid:
		return "the session has ended", 0, "", nil
	case modFresh(seen, now):
		// The session's mod takes the message, with or without herdr.
		return "", machineID, machine, nil
	case pane == "":
		return "the session is not in herdr", 0, "", nil
	}
	polled, err := parseNullTS(lastPoll)
	if err != nil {
		return "", 0, "", err
	}
	if polled == nil || now.Sub(*polled) > ControlPollWindow {
		return fmt.Sprintf("the sessionhub watcher on %q is offline", machine), 0, "", nil
	}
	return "", machineID, machine, nil
}

// ClaimMessage hands machineID a queued message that was never offered or
// was last offered at least MessageRetry ago, as a control
// claim whose action is api.ActionMessage and whose Text is the prompt to
// submit. A session's messages go in order: a message is not claimed while an
// older queued message for the same session waits. A session whose mod
// polled in ControlPollWindow is skipped: its mod claims its messages with
// ClaimSessionMessage. So is a session without a herdr pane, whose messages
// only a mod can deliver. ok is false when there is none.
func (s *Store) ClaimMessage(ctx context.Context, machineID int64) (api.ControlClaim, bool, error) {
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	defer tx.Rollback()
	if err := expireMessagesTx(ctx, tx, now); err != nil {
		return api.ControlClaim{}, false, err
	}
	var id, sessionID, text, sender, created, machine string
	err = tx.QueryRowContext(ctx, `SELECT m.id, m.session_id, m.text, m.sender, m.created_at, mc.name
		FROM messages m JOIN machines mc ON mc.id = m.machine_id
		WHERE m.machine_id = ? AND m.state = ? AND (m.offered_at IS NULL OR m.offered_at <= ?)
		AND NOT EXISTS (SELECT 1 FROM messages o WHERE o.session_id = m.session_id AND o.state = m.state
			AND (o.created_at < m.created_at OR (o.created_at = m.created_at AND o.rowid < m.rowid)))
		AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = m.session_id AND (s.herdr_pane = '' OR `+modFreshSQL+`))
		ORDER BY m.created_at, m.rowid LIMIT 1`, machineID, api.MessageQueued, formatTS(now.Add(-MessageRetry)),
		formatTS(now.Add(-ControlPollWindow))).
		Scan(&id, &sessionID, &text, &sender, &created, &machine)
	if errors.Is(err, sql.ErrNoRows) {
		return api.ControlClaim{}, false, tx.Commit()
	}
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET offered_at = ?, updated_at = ? WHERE id = ?`,
		formatTS(now), formatTS(now), id); err != nil {
		return api.ControlClaim{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return api.ControlClaim{}, false, err
	}
	createdAt, err := parseTS(created)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	// GetSession uses s.db, so it runs after the commit.
	sess, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	req := api.ControlRequest{ID: id, SessionID: sessionID, Machine: machine, Action: api.ActionMessage,
		State: api.ControlClaimed, RequestedBy: sender, CreatedAt: createdAt, ExpiresAt: createdAt.Add(MessageTTL), ClaimedAt: &now}
	return api.ControlClaim{Request: req, Session: sess, Text: api.MessagePrompt(sender, text)}, true, nil
}

// FinishMessage records a watcher's result for a queued message of
// machineID: delivered, refused, or busy (still queued). A failed result,
// from a watcher that does not know the message action, counts as refused.
func (s *Store) FinishMessage(ctx context.Context, machineID int64, id string, in api.ControlResultIn) (api.Message, error) {
	if !ValidMessageID(id) {
		return api.Message{}, invalidf("message id %q: want msg_ and 22 base64url characters", id)
	}
	state := in.State
	if state == api.ControlFailed {
		state = api.MessageRefused
	}
	if state != api.MessageDelivered && state != api.MessageRefused && state != api.MessageBusy {
		return api.Message{}, invalidf("state %q: want delivered, refused, or busy", in.State)
	}
	if in.URL != "" {
		return api.Message{}, invalidf("url: a message result carries no link")
	}
	if err := checkText("detail", in.Detail, api.MaxControlDetailRunes); err != nil {
		return api.Message{}, err
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Message{}, err
	}
	defer tx.Rollback()
	if err := expireMessagesTx(ctx, tx, now); err != nil {
		return api.Message{}, err
	}
	var owner int64
	var cur, sessionID, sender, text string
	err = tx.QueryRowContext(ctx, `SELECT machine_id, state, session_id, sender, text FROM messages WHERE id = ?`, id).
		Scan(&owner, &cur, &sessionID, &sender, &text)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Message{}, fmt.Errorf("%w: message %s", ErrNotFound, id)
	}
	if err != nil {
		return api.Message{}, err
	}
	if owner != machineID {
		return api.Message{}, fmt.Errorf("%w: message %s belongs to another machine", ErrWrongMachine, id)
	}
	if cur != api.MessageQueued {
		return api.Message{}, fmt.Errorf("%w: message %s is %s, not queued", ErrRequestClosed, id, cur)
	}
	nowS := formatTS(now)
	if state == api.MessageBusy {
		_, err = tx.ExecContext(ctx, `UPDATE messages SET detail = ?, updated_at = ? WHERE id = ?`, in.Detail, nowS, id)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE messages SET state = ?, detail = ?, updated_at = ? WHERE id = ?`, state, in.Detail, nowS, id)
	}
	if err != nil {
		return api.Message{}, err
	}
	if state == api.MessageDelivered {
		if err := messageEventTx(ctx, tx, sessionID, id, sender, state, text, now); err != nil {
			return api.Message{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return api.Message{}, err
	}
	return s.GetMessage(ctx, id)
}

// GetMessage reads one message. A queued message past MessageTTL reads as
// expired, whether or not a write stored that yet.
func (s *Store) GetMessage(ctx context.Context, id string) (api.Message, error) {
	if !ValidMessageID(id) {
		return api.Message{}, invalidf("message id %q: want msg_ and 22 base64url characters", id)
	}
	var m api.Message
	var created, updated string
	err := s.db.QueryRowContext(ctx, `SELECT m.id, m.session_id, mc.name, m.sender, m.text, m.state, m.detail,
		m.created_at, m.updated_at FROM messages m JOIN machines mc ON mc.id = m.machine_id WHERE m.id = ?`, id).
		Scan(&m.ID, &m.SessionID, &m.Machine, &m.Sender, &m.Text, &m.State, &m.Detail, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Message{}, fmt.Errorf("%w: message %s", ErrNotFound, id)
	}
	if err != nil {
		return api.Message{}, err
	}
	if m.CreatedAt, err = parseTS(created); err != nil {
		return api.Message{}, err
	}
	if m.UpdatedAt, err = parseTS(updated); err != nil {
		return api.Message{}, err
	}
	if m.State == api.MessageQueued && !s.Now().Before(m.CreatedAt.Add(MessageTTL)) {
		m.State = api.MessageExpired
	}
	return m, nil
}

// NextMessageRetry reports how long until the earliest queued message of
// machineID that a watcher already saw, of a session the watcher serves
// (with a herdr pane and without a fresh mod), is offered again (MessageRetry after
// its last offer), and false when there is none. A long poll waits that long
// instead of for its next turn. The delay is at least one second, so a
// message held back behind an older one for the same session does not make
// the poll spin.
func (s *Store) NextMessageRetry(ctx context.Context, machineID int64) (time.Duration, bool, error) {
	now := s.Now()
	var offered sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT MIN(m.offered_at) FROM messages m
		WHERE m.machine_id = ? AND m.state = ? AND m.offered_at IS NOT NULL AND m.created_at > ?
		AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = m.session_id AND (s.herdr_pane = '' OR `+modFreshSQL+`))`,
		machineID, api.MessageQueued, formatTS(now.Add(-MessageTTL)), formatTS(now.Add(-ControlPollWindow))).Scan(&offered)
	if err != nil || !offered.Valid {
		return 0, false, err
	}
	at, err := parseTS(offered.String)
	if err != nil {
		return 0, false, err
	}
	return max(at.Add(MessageRetry).Sub(now), time.Second), true, nil
}
