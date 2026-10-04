package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// Triage limits. See docs/server.md, "Inbox".
const (
	// TriageSinceSkew is how far ahead of the server clock a triage since
	// may be.
	TriageSinceSkew = 5 * time.Minute
	// MaxSnooze is the longest snooze.
	MaxSnooze = 7 * 24 * time.Hour
)

// inboxRank orders the groups. A session appears only in its first match.
var inboxRank = map[string]int{api.InboxBlocked: 0, api.InboxWaiting: 1, api.InboxFinished: 2}

// classifyInbox returns x's inbox item without its session, or ok=false when
// x needs nothing. blockedAt, stateTS, and turnEndedAt are the sessions
// columns.
func classifyInbox(x api.Session, blockedAt, stateTS, turnEndedAt *time.Time) (item api.InboxItem, ok bool) {
	if x.EndedAt != nil {
		return item, false
	}
	if x.AgentState == "blocked" {
		// blocked_at is when the state last moved into blocked. Rows from
		// before it existed fall back to state_ts, then to started_at, which
		// stays stable across polls so a dismiss sticks.
		since := x.StartedAt
		switch {
		case blockedAt != nil:
			since = *blockedAt
		case stateTS != nil:
			since = *stateTS
		}
		return api.InboxItem{Group: api.InboxBlocked, Since: since}, true
	}
	if r := x.LatestReport; r != nil && len(r.WaitingOn) > 0 && notAfter(x.LastPromptAt, r.TS) {
		return api.InboxItem{Group: api.InboxWaiting, Since: r.TS, WaitingOn: r.WaitingOn}, true
	}
	if (x.AgentState == "idle" || x.AgentState == "done") && turnEndedAt != nil && notAfter(x.LastPromptAt, *turnEndedAt) {
		return api.InboxItem{Group: api.InboxFinished, Since: *turnEndedAt}, true
	}
	return item, false
}

// notAfter reports whether the prompt time p is unset or not later than t.
func notAfter(p *time.Time, t time.Time) bool { return p == nil || !p.After(t) }

// triageHides reports whether a triage row hides an item whose trigger time
// is since: the row is for this trigger or a later one, and it is a dismiss
// or a snooze that has not ended.
func triageHides(since time.Time, triagedSince, snoozeUntil *time.Time, now time.Time) bool {
	if triagedSince == nil || since.After(*triagedSince) {
		return false
	}
	return snoozeUntil == nil || now.Before(*snoozeUntil)
}

// inboxState is what Inbox reads besides the session object.
type inboxState struct {
	blockedAt, stateTS, turnEndedAt, triagedSince, snoozeUntil *time.Time
}

// inboxStates reads the inbox columns of every session that has not ended.
func (s *Store) inboxStates(ctx context.Context) (map[string]inboxState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, s.blocked_at, s.state_ts, s.turn_ended_at, t.triaged_since, t.snooze_until
		FROM sessions s LEFT JOIN inbox_triage t ON t.session_id = s.id
		WHERE s.ended_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]inboxState{}
	for rows.Next() {
		var id string
		var blocked, stateTS, turnEnded, triaged, snooze sql.NullString
		if err := rows.Scan(&id, &blocked, &stateTS, &turnEnded, &triaged, &snooze); err != nil {
			return nil, err
		}
		var st inboxState
		if st.blockedAt, err = parseNullTS(blocked); err != nil {
			return nil, err
		}
		if st.stateTS, err = parseNullTS(stateTS); err != nil {
			return nil, err
		}
		if st.turnEndedAt, err = parseNullTS(turnEnded); err != nil {
			return nil, err
		}
		if st.triagedSince, err = parseNullTS(triaged); err != nil {
			return nil, err
		}
		if st.snoozeUntil, err = parseNullTS(snooze); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// Inbox returns the sessions that need you: blocked, waiting on you, or
// finished a turn, without the ones a dismiss or snooze hides. Items are
// sorted by group, then since, then session ID. Counts cover the returned
// items only.
func (s *Store) Inbox(ctx context.Context) (api.Inbox, error) {
	out := api.Inbox{Items: []api.InboxItem{}}
	states, err := s.inboxStates(ctx)
	if err != nil {
		return out, err
	}
	list, err := s.ListSessions(ctx, ListFilter{})
	if err != nil {
		return out, err
	}
	perms, err := s.openPermissions(ctx)
	if err != nil {
		return out, err
	}
	now := s.Now()
	for _, x := range list {
		st, ok := states[x.ID]
		if !ok {
			continue // ended, or registered between the two reads
		}
		item, ok := classifyInbox(x, st.blockedAt, st.stateTS, st.turnEndedAt)
		if !ok || triageHides(item.Since, st.triagedSince, st.snoozeUntil, now) {
			continue
		}
		item.Session = x
		if p, ok := perms[x.ID]; ok && item.Group == api.InboxBlocked {
			item.Permission = &p
		}
		out.Items = append(out.Items, item)
		switch item.Group {
		case api.InboxBlocked:
			out.Counts.Blocked++
		case api.InboxWaiting:
			out.Counts.Waiting++
		case api.InboxFinished:
			out.Counts.Finished++
		}
	}
	sort.SliceStable(out.Items, func(i, j int) bool {
		a, b := out.Items[i], out.Items[j]
		if ra, rb := inboxRank[a.Group], inboxRank[b.Group]; ra != rb {
			return ra < rb
		}
		if !a.Since.Equal(b.Since) {
			return a.Since.Before(b.Since)
		}
		return a.Session.ID < b.Session.ID
	})
	return out, nil
}

// Triage dismisses session id's inbox item (snoozeUntil nil) or snoozes it
// until snoozeUntil. since is the item's since as the caller showed it; an
// item with a later since shows again. One row per session: a new triage
// replaces the old one. It records no event and leaves last_seen_at alone.
func (s *Store) Triage(ctx context.Context, id string, since time.Time, snoozeUntil *time.Time) error {
	now := s.Now()
	if since.IsZero() {
		return invalidf("since is required")
	}
	if since.After(now.Add(TriageSinceSkew)) {
		return invalidf("since %s is more than %s ahead of the server clock", since.UTC().Format(time.RFC3339), TriageSinceSkew)
	}
	var until any // NULL for a dismiss
	if snoozeUntil != nil {
		switch {
		case !snoozeUntil.After(now):
			return invalidf("until %s is not in the future", snoozeUntil.UTC().Format(time.RFC3339))
		case snoozeUntil.After(now.Add(MaxSnooze)):
			return invalidf("until %s is more than 7 days ahead", snoozeUntil.UTC().Format(time.RFC3339))
		}
		until = formatTS(*snoozeUntil)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_triage (session_id, triaged_since, snooze_until, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET triaged_since = excluded.triaged_since,
			snooze_until = excluded.snooze_until, updated_at = excluded.updated_at`,
		id, formatTS(since), until, formatTS(now)); err != nil {
		return err
	}
	return tx.Commit()
}

// autoDismissSeenTx dismisses session id's Finished item inside tx, with
// triaged_since set to the item's since (turn_ended_at). Callers run it after
// an update that moved agent_state from done to idle, which is herdr marking
// the pane seen. A Blocked or Waiting item, or no item, is left alone.
//
// Every read goes through tx: the store has one connection, and the
// transaction holds it, so a read on s.db here would wait forever.
func (s *Store) autoDismissSeenTx(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	x, err := s.scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE s.id = ?", id), now)
	if err != nil {
		return err
	}
	var blocked, stateTS, turnEnded sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT blocked_at, state_ts, turn_ended_at FROM sessions WHERE id = ?`, id).
		Scan(&blocked, &stateTS, &turnEnded); err != nil {
		return err
	}
	var st inboxState
	if st.blockedAt, err = parseNullTS(blocked); err != nil {
		return err
	}
	if st.stateTS, err = parseNullTS(stateTS); err != nil {
		return err
	}
	if st.turnEndedAt, err = parseNullTS(turnEnded); err != nil {
		return err
	}
	item, ok := classifyInbox(x, st.blockedAt, st.stateTS, st.turnEndedAt)
	if !ok || item.Group != api.InboxFinished {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO inbox_triage (session_id, triaged_since, snooze_until, updated_at)
		VALUES (?, ?, NULL, ?)
		ON CONFLICT(session_id) DO UPDATE SET triaged_since = excluded.triaged_since,
			snooze_until = NULL, updated_at = excluded.updated_at
			WHERE excluded.triaged_since >= inbox_triage.triaged_since`,
		id, formatTS(item.Since), formatTS(now))
	return err
}
