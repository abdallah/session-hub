package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// MinPrefixLen is the shortest ID prefix GET /v1/sessions/{id} accepts.
const MinPrefixLen = 4

// maxEvents is how many events SessionDetail returns.
const maxEvents = 50

// status computes a session's status at read time.
func status(now time.Time, staleAfter time.Duration, lastSeen time.Time, endedAt *time.Time, agentState string) string {
	switch {
	case endedAt != nil:
		return api.StatusEnded
	case now.Sub(lastSeen) > staleAfter:
		return api.StatusStale
	case agentState == "blocked":
		return api.StatusBlocked
	}
	return api.StatusLive
}

// ResumeCommand is the command that resumes session id on its machine. It
// calls sessionhub by path: ssh runs the command in a non-interactive shell whose
// PATH lacks ~/.local/bin. The single quotes keep your local shell from
// expanding the tilde to your local home; the remote shell sees it unquoted
// and expands it to the remote home.
func ResumeCommand(sshHost, machine, id string) string {
	if sshHost == "" {
		sshHost = machine
	}
	return fmt.Sprintf("ssh -t %s '~/.local/bin/sessionhub resume %s'", sshHost, id)
}

// sessionSelect reads a session, its machine, its latest report, and its
// latest control request.
const sessionSelect = `SELECT s.id, s.agent, m.name, m.ssh_host, s.cwd, s.git_repo, s.git_branch,
	s.herdr_session, s.herdr_workspace, s.herdr_pane, s.title, s.title_source, s.agent_state,
	s.started_at, s.last_seen_at, s.ended_at,
	r.ts, r.done_json, r.in_flight_json, r.waiting_on_json, r.note,
	m.last_poll, s.rc_url, s.rc_at,
	c.id, c.action, c.state, c.requested_by, c.created_at, c.expires_at, c.claimed_at, c.finished_at, c.url, c.detail,
	s.last_prompt, s.last_prompt_at, dg.body,
	mv.id, mv.target, mv.state, mv.detail, mv.cloud_url, mv.requested_by, mv.bundle_size, mv.created_at, mv.updated_at, mvs.name,
	s.blocked_on, s.context_percent, s.live_cost_usd, s.usage_at, s.mod_seen_at
FROM sessions s
JOIN machines m ON m.id = s.machine_id
LEFT JOIN reports r ON r.id = (SELECT id FROM reports WHERE session_id = s.id ORDER BY id DESC LIMIT 1)
LEFT JOIN control_requests c ON c.id = (SELECT id FROM control_requests WHERE session_id = s.id
	ORDER BY created_at DESC, rowid DESC LIMIT 1)
LEFT JOIN session_digests dg ON dg.session_id = s.id
LEFT JOIN moves mv ON mv.id = (SELECT id FROM moves WHERE session_id = s.id ORDER BY created_at DESC, rowid DESC LIMIT 1)
LEFT JOIN machines mvs ON mvs.id = mv.source_machine_id`

type scanner interface{ Scan(dest ...any) error }

func (s *Store) scanSession(sc scanner, now time.Time) (api.Session, error) {
	var x api.Session
	var sshHost, started, lastSeen string
	var ended, rTS, rDone, rInFlight, rWaiting, rNote, lastPoll, rcAt, lastPromptAt, digestBody sql.NullString
	var rc nullControl
	var mv nullMove
	var contextPercent sql.NullInt64
	var liveCost sql.NullFloat64
	var usageAt, modSeen sql.NullString
	err := sc.Scan(&x.ID, &x.Agent, &x.Machine, &sshHost, &x.CWD, &x.GitRepo, &x.GitBranch,
		&x.HerdrSession, &x.HerdrWorkspace, &x.HerdrPane, &x.Title, &x.TitleSource, &x.AgentState,
		&started, &lastSeen, &ended, &rTS, &rDone, &rInFlight, &rWaiting, &rNote,
		&lastPoll, &x.RemoteControlURL, &rcAt,
		&rc.id, &rc.action, &rc.state, &rc.requestedBy, &rc.created, &rc.expires, &rc.claimed, &rc.finished, &rc.url, &rc.detail,
		&x.LastPrompt, &lastPromptAt, &digestBody,
		&mv.id, &mv.target, &mv.state, &mv.detail, &mv.cloudURL, &mv.by, &mv.size, &mv.created, &mv.updated, &mv.source,
		&x.BlockedOn, &contextPercent, &liveCost, &usageAt, &modSeen)
	if err != nil {
		return x, err
	}
	if x.StartedAt, err = parseTS(started); err != nil {
		return x, err
	}
	if x.LastSeenAt, err = parseTS(lastSeen); err != nil {
		return x, err
	}
	if x.EndedAt, err = parseNullTS(ended); err != nil {
		return x, err
	}
	x.Status = status(now, s.staleAfter, x.LastSeenAt, x.EndedAt, x.AgentState)
	x.ResumeCommand = ResumeCommand(sshHost, x.Machine, x.ID)
	if rTS.Valid {
		r, err := decodeReport(rTS.String, rDone.String, rInFlight.String, rWaiting.String, rNote.String)
		if err != nil {
			return x, err
		}
		x.LatestReport = &r
	}
	if x.RemoteControlAt, err = parseNullTS(rcAt); err != nil {
		return x, err
	}
	polled, err := parseNullTS(lastPoll)
	if err != nil {
		return x, err
	}
	x.Controllable = x.HerdrPane != "" && polled != nil && now.Sub(*polled) <= ControlPollWindow
	seen, err := parseNullTS(modSeen)
	if err != nil {
		return x, err
	}
	x.Messageable = x.Controllable || modFresh(seen, now)
	if contextPercent.Valid {
		n := int(contextPercent.Int64)
		x.ContextPercent = &n
	}
	if liveCost.Valid {
		c := liveCost.Float64
		x.LiveCostUSD = &c
	}
	if x.UsageAt, err = parseNullTS(usageAt); err != nil {
		return x, err
	}
	if x.RemoteControl, err = rc.request(x.ID, x.Machine, now); err != nil {
		return x, err
	}
	if x.LastPromptAt, err = parseNullTS(lastPromptAt); err != nil {
		return x, err
	}
	if x.Move, err = mv.move(x.ID); err != nil {
		return x, err
	}
	if digestBody.Valid {
		var d api.DigestIn
		if err := json.Unmarshal([]byte(digestBody.String), &d); err != nil {
			return x, fmt.Errorf("stored digest of %s: %w", x.ID, err)
		}
		x.Recap, x.RecapAt = d.Recap, d.RecapAt
		x.Summary = summarize(d)
	}
	return x, nil
}

func decodeReport(ts, done, inFlight, waiting, note string) (api.Report, error) {
	r := api.Report{Note: note}
	var err error
	if r.TS, err = parseTS(ts); err != nil {
		return r, err
	}
	for _, f := range []struct {
		src string
		dst *[]string
	}{{done, &r.Done}, {inFlight, &r.InFlight}, {waiting, &r.WaitingOn}} {
		if err := json.Unmarshal([]byte(f.src), f.dst); err != nil {
			return r, fmt.Errorf("stored report list: %w", err)
		}
		if *f.dst == nil {
			*f.dst = []string{}
		}
	}
	return r, nil
}

// ListFilter selects sessions for ListSessions.
type ListFilter struct {
	LiveOnly bool   // only live and blocked sessions
	Machine  string // only this machine's sessions, if set
}

// ListSessions returns sessions, most recently seen first.
func (s *Store) ListSessions(ctx context.Context, f ListFilter) ([]api.Session, error) {
	q := sessionSelect
	var args []any
	if f.Machine != "" {
		q += " WHERE m.name = ?"
		args = append(args, f.Machine)
	}
	q += " ORDER BY s.last_seen_at DESC, s.id"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.Now()
	out := []api.Session{}
	for rows.Next() {
		x, err := s.scanSession(rows, now)
		if err != nil {
			return nil, err
		}
		if f.LiveOnly && x.Status != api.StatusLive && x.Status != api.StatusBlocked {
			continue
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// GetSession returns the session with exactly this id.
func (s *Store) GetSession(ctx context.Context, id string) (api.Session, error) {
	x, err := s.scanSession(s.db.QueryRowContext(ctx, sessionSelect+" WHERE s.id = ?", id), s.Now())
	if errors.Is(err, sql.ErrNoRows) {
		return x, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	return x, err
}

// ResolveID turns a full ID or a unique prefix of at least MinPrefixLen
// characters into a full session ID. An exact match wins over a prefix match.
func (s *Store) ResolveID(ctx context.Context, prefix string) (string, error) {
	if len(prefix) < MinPrefixLen {
		return "", invalidf("id prefix %q is shorter than %d characters", prefix, MinPrefixLen)
	}
	if !ValidSessionID(prefix) {
		return "", invalidf("id %q: use letters, digits, '_', or '-'", prefix)
	}
	// substr, not LIKE: '_' is a LIKE wildcard and is allowed in IDs.
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM sessions WHERE substr(id, 1, ?) = ? ORDER BY id`, len(prefix), prefix)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		if id == prefix {
			return id, nil
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("%w: no session matches %q", ErrNotFound, prefix)
	case 1:
		return ids[0], nil
	}
	return "", &AmbiguousError{Prefix: prefix, Candidates: ids}
}

// SessionDetail returns the session with every report and the last 50
// events, newest first.
func (s *Store) SessionDetail(ctx context.Context, id string) (api.SessionDetail, error) {
	var d api.SessionDetail
	var err error
	if d.Session, err = s.GetSession(ctx, id); err != nil {
		return d, err
	}
	d.Reports = []api.Report{}
	d.Events = []api.Event{}

	var body, received string
	err = s.db.QueryRowContext(ctx, `SELECT body, received_at FROM session_digests WHERE session_id = ?`, id).Scan(&body, &received)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return d, err
	default:
		dg := &api.Digest{}
		if err := json.Unmarshal([]byte(body), &dg.DigestIn); err != nil {
			return d, fmt.Errorf("stored digest of %s: %w", id, err)
		}
		if dg.ReceivedAt, err = parseTS(received); err != nil {
			return d, err
		}
		d.Digest = dg
	}

	rows, err := s.db.QueryContext(ctx, `SELECT ts, done_json, in_flight_json, waiting_on_json, note
		FROM reports WHERE session_id = ? ORDER BY ts DESC, id DESC`, id)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var ts, done, inFlight, waiting, note string
		if err := rows.Scan(&ts, &done, &inFlight, &waiting, &note); err != nil {
			rows.Close()
			return d, err
		}
		r, err := decodeReport(ts, done, inFlight, waiting, note)
		if err != nil {
			rows.Close()
			return d, err
		}
		d.Reports = append(d.Reports, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return d, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT ts, source, kind, payload_json FROM events
		WHERE session_id = ? ORDER BY ts DESC, id DESC LIMIT ?`, id, maxEvents)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var e api.Event
		var ts string
		var payload sql.NullString
		if err := rows.Scan(&ts, &e.Source, &e.Kind, &payload); err != nil {
			return d, err
		}
		if e.TS, err = parseTS(ts); err != nil {
			return d, err
		}
		if payload.Valid {
			e.Payload = json.RawMessage(payload.String)
		}
		d.Events = append(d.Events, e)
	}
	return d, rows.Err()
}
