package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
)

// Report limits: at most MaxReportItems per list, each at most
// MaxReportItemLen characters.
const (
	MaxReportItems   = 20
	MaxReportItemLen = 200
)

// Title sources, from lowest to highest precedence.
const (
	TitleSourcePrompt = "prompt"
	TitleSourceClaude = "claude" // Claude Code's automatic title (ai-title), from a digest
	TitleSourceHerdr  = "herdr"
	TitleSourceUser   = "user"
)

// titleRank orders title sources; a lower rank never replaces a higher one.
func titleRank(src string) int {
	switch src {
	case TitleSourceUser:
		return 4
	case TitleSourceHerdr:
		return 3
	case TitleSourceClaude:
		return 2
	case TitleSourcePrompt:
		return 1
	}
	return 0
}

// nextTitle applies title precedence. cur is the stored title source, hint is
// the incoming herdr title, prompt is the session's first prompt. ok is false
// when the title stays as it is.
func nextTitle(cur, hint, prompt string) (title, source string, ok bool) {
	switch {
	case hint != "" && titleRank(cur) <= titleRank(TitleSourceHerdr):
		return hint, TitleSourceHerdr, true
	case prompt != "" && titleRank(cur) <= titleRank(TitleSourcePrompt):
		return prompt, TitleSourcePrompt, true
	}
	return "", "", false
}

var (
	sessionIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	// identRE covers the herdr identifiers and the agent name. Empty is
	// allowed by the caller and means "unknown".
	identRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,63}$`)
)

// Free-text caps. Runes for prose, bytes for the working directory.
const (
	MaxTitleRunes = 200
	MaxNoteRunes  = 2000
	MaxCWDBytes   = 4096
)

// checkText rejects control characters (C0, C1, and DEL) and text longer than
// maxRunes runes. Newlines count as control characters: no free-text field
// needs them.
func checkText(name, v string, maxRunes int) error {
	if n := utf8.RuneCountInString(v); n > maxRunes {
		return invalidf("%s is %d characters, at most %d allowed", name, n, maxRunes)
	}
	return checkNoControl(name, v)
}

func checkNoControl(name, v string) error {
	for _, r := range v {
		if unicode.IsControl(r) {
			return invalidf("%s contains a control character (U+%04X), which is not allowed", name, r)
		}
	}
	return nil
}

// checkIdent validates an optional identifier field.
func checkIdent(name, v string) error {
	if v != "" && !identRE.MatchString(v) {
		return invalidf("%s %q: use 1-64 letters, digits, ':', '.', '_', or '-', starting with a letter or digit", name, v)
	}
	return nil
}

// ValidSessionID reports whether id is an acceptable session ID (or prefix).
func ValidSessionID(id string) bool { return sessionIDRE.MatchString(id) }

func validSource(s string) bool {
	return s == api.SourcePlugin || s == api.SourceHooks || s == api.SourceMCP
}

func validAgentState(s string) bool {
	switch s {
	case "idle", "working", "blocked", "done", "unknown":
		return true
	}
	return false
}

func validateUpsert(u api.SessionUpsert) error {
	if !ValidSessionID(u.ID) {
		return invalidf("session id %q: use 1-128 letters, digits, '_', or '-', starting with a letter or digit", u.ID)
	}
	if !validSource(u.Source) {
		return invalidf("source %q: want plugin, hooks, or mcp", u.Source)
	}
	if u.AgentState != "" && !validAgentState(u.AgentState) {
		return invalidf("agent_state %q: want idle, working, blocked, done, or unknown", u.AgentState)
	}
	for _, f := range []struct{ name, v string }{
		{"agent", u.Agent}, {"herdr_session", u.HerdrSession},
		{"herdr_workspace", u.HerdrWorkspace}, {"herdr_pane", u.HerdrPane},
	} {
		if err := checkIdent(f.name, f.v); err != nil {
			return err
		}
	}
	for _, f := range []struct {
		name, v string
		max     int
	}{{"title_hint", u.TitleHint, MaxTitleRunes}, {"first_prompt", u.FirstPrompt, MaxTitleRunes}} {
		if err := checkText(f.name, f.v, f.max); err != nil {
			return err
		}
	}
	if len(u.CWD) > MaxCWDBytes {
		return invalidf("cwd is %d bytes, at most %d allowed", len(u.CWD), MaxCWDBytes)
	}
	if err := checkNoControl("cwd", u.CWD); err != nil {
		return err
	}
	if err := checkNoControl("git_repo", u.GitRepo); err != nil {
		return err
	}
	return checkNoControl("git_branch", u.GitBranch)
}

// ownerTx returns the machine that owns session id, as seen inside tx.
func ownerTx(ctx context.Context, tx *sql.Tx, id string) (int64, string, error) {
	var mid int64
	var name string
	err := tx.QueryRowContext(ctx,
		`SELECT s.machine_id, m.name FROM sessions s JOIN machines m ON m.id = s.machine_id WHERE s.id = ?`, id).
		Scan(&mid, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	return mid, name, err
}

// checkOwnerTx returns ErrNotFound or ErrWrongMachine unless machineID owns id.
func checkOwnerTx(ctx context.Context, tx *sql.Tx, machineID int64, id string) error {
	mid, name, err := ownerTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if mid != machineID {
		return fmt.Errorf("%w: session %s is registered to machine %q", ErrWrongMachine, id, name)
	}
	return nil
}

// CheckSessionOwner returns ErrNotFound or ErrWrongMachine unless machineID
// owns session id.
func (s *Store) CheckSessionOwner(ctx context.Context, machineID int64, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return checkOwnerTx(ctx, tx, machineID, id)
}

func insertEventTx(ctx context.Context, tx *sql.Tx, id string, ts time.Time, source, kind string, payload []byte) error {
	var p any
	if len(payload) > 0 {
		p = string(payload)
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO events (session_id, ts, source, kind, payload_json) VALUES (?, ?, ?, ?, ?)`,
		id, formatTS(ts), source, kind, p)
	return err
}

// turnEndedExpr sets sessions.turn_ended_at when an update moves the agent
// state from working or blocked to idle or done. SQLite evaluates every SET
// expression on the old row, so agent_state here is the state before the
// update. Its two arguments are the new state and the time.
const turnEndedExpr = `CASE WHEN agent_state IN ('working', 'blocked') AND ? IN ('idle', 'done') THEN ? ELSE turn_ended_at END`

// blockedAtExpr sets sessions.blocked_at when an update moves the agent state
// into blocked from any other value, including none. Like turnEndedExpr it
// reads the old row. Its two arguments are the new state and the time.
const blockedAtExpr = `CASE WHEN ? = 'blocked' AND COALESCE(agent_state, '') != 'blocked' THEN ? ELSE blocked_at END`

// UpsertSession registers or refreshes one session for machineID. It returns
// created=true on first insert.
func (s *Store) UpsertSession(ctx context.Context, machineID int64, u api.SessionUpsert) (created bool, err error) {
	if err := validateUpsert(u); err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	created, err = s.upsertTx(ctx, tx, machineID, u, s.Now())
	if err != nil {
		return false, err
	}
	return created, tx.Commit()
}

func (s *Store) upsertTx(ctx context.Context, tx *sql.Tx, machineID int64, u api.SessionUpsert, now time.Time) (bool, error) {
	var titleSource, firstPrompt, prevState string
	err := tx.QueryRowContext(ctx, `SELECT title_source, first_prompt, agent_state FROM sessions WHERE id = ?`, u.ID).
		Scan(&titleSource, &firstPrompt, &prevState)
	if errors.Is(err, sql.ErrNoRows) {
		title, src, _ := nextTitle("", u.TitleHint, u.FirstPrompt)
		nowS := formatTS(now)
		_, err = tx.ExecContext(ctx, `INSERT INTO sessions (id, machine_id, agent, cwd, git_repo, git_branch,
			herdr_session, herdr_workspace, herdr_pane, title, title_source, first_prompt, agent_state,
			started_at, last_seen_at, blocked_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			u.ID, machineID, u.Agent, u.CWD, u.GitRepo, u.GitBranch, u.HerdrSession, u.HerdrWorkspace,
			u.HerdrPane, title, src, u.FirstPrompt, u.AgentState, nowS, nowS, blockedAtInsert(u.AgentState, nowS))
		if err != nil {
			return false, err
		}
		return true, insertEventTx(ctx, tx, u.ID, now, u.Source, api.KindRegistered, nil)
	}
	if err != nil {
		return false, err
	}
	if err := checkOwnerTx(ctx, tx, machineID, u.ID); err != nil {
		return false, err
	}

	sets := []string{"last_seen_at = ?", "ended_at = NULL"}
	args := []any{formatTS(now)}
	set := func(col, v string) {
		if v != "" { // empty fields never overwrite
			sets = append(sets, col+" = ?")
			args = append(args, v)
		}
	}
	set("agent", u.Agent)
	set("cwd", u.CWD)
	set("git_repo", u.GitRepo)
	set("git_branch", u.GitBranch)
	set("herdr_session", u.HerdrSession)
	set("herdr_workspace", u.HerdrWorkspace)
	set("herdr_pane", u.HerdrPane)
	// state_ts is the event clock (client timestamps, clamped to server now).
	// An upsert carries no event time, so it sets the state and leaves
	// state_ts alone: stamping it with server time here would make a
	// state_changed event from a client clock a little behind lose to it.
	set("agent_state", u.AgentState)
	if u.AgentState != "" {
		sets = append(sets, "turn_ended_at = "+turnEndedExpr, "blocked_at = "+blockedAtExpr)
		args = append(args, u.AgentState, formatTS(now), u.AgentState, formatTS(now))
		if u.AgentState != "blocked" {
			// What the session waited on is answered once it leaves blocked.
			sets = append(sets, "blocked_on = ''")
		}
	}
	if firstPrompt == "" { // the first prompt is recorded once
		set("first_prompt", u.FirstPrompt)
		firstPrompt = u.FirstPrompt
	}
	if title, src, ok := nextTitle(titleSource, u.TitleHint, firstPrompt); ok {
		set("title", title)
		set("title_source", src)
	}
	args = append(args, u.ID)
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return false, err
	}
	if prevState == "blocked" && u.AgentState != "" && u.AgentState != "blocked" {
		// The prompt was answered in the terminal. The snapshot may predate
		// a request that just arrived, so the newest ones stay open.
		if err := closePermissionsTx(ctx, tx, u.ID, api.PermissionAnsweredLocally, now.Add(-PermissionGrace), now); err != nil {
			return false, err
		}
	}
	if u.Source == api.SourcePlugin && prevState == "done" && u.AgentState == "idle" {
		// The herdr plugin reports the pane seen: its Finished item is read.
		// A hooks idle (Claude Stop) is not a sighting.
		return false, s.autoDismissSeenTx(ctx, tx, u.ID, now)
	}
	return false, nil
}

// clientKinds are the event kinds clients may post. registered is recorded
// by the server itself.
var clientKinds = map[string]bool{
	api.KindStateChanged: true,
	api.KindPrompt:       true,
	api.KindPaneClosed:   true,
	api.KindEnded:        true,
}

// AddEvent records a lifecycle event for session id.
func (s *Store) AddEvent(ctx context.Context, machineID int64, id string, e api.EventIn) error {
	if !clientKinds[e.Kind] {
		return invalidf("kind %q: want state_changed, prompt, pane_closed, or ended", e.Kind)
	}
	if !validSource(e.Source) {
		return invalidf("source %q: want plugin, hooks, or mcp", e.Source)
	}
	payload := bytes.TrimSpace(e.Payload)
	if string(payload) == "null" {
		payload = nil
	}
	if len(payload) > 0 {
		var buf bytes.Buffer
		if err := json.Compact(&buf, payload); err != nil {
			return invalidf("payload: %v", err)
		}
		payload = buf.Bytes()
	}
	var state string
	if e.Kind == api.KindStateChanged {
		var p struct {
			AgentState string `json:"agent_state"`
		}
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &p); err != nil {
				return invalidf("state_changed payload: %v", err)
			}
		}
		if !validAgentState(p.AgentState) {
			return invalidf("state_changed payload agent_state %q: want idle, working, blocked, done, or unknown", p.AgentState)
		}
		state = p.AgentState
	}

	// A prompt event also sets the session's last prompt. A payload sessionhub
	// can't use is still stored as an event, as before.
	var prompt string
	if e.Kind == api.KindPrompt && len(payload) > 0 {
		var p struct {
			Prompt string `json:"prompt"`
		}
		if json.Unmarshal(payload, &p) == nil && p.Prompt != "" && checkText("prompt", p.Prompt, MaxTitleRunes) == nil {
			prompt = p.Prompt
		}
	}

	now := s.Now()
	ts := e.TS.UTC()
	if e.TS.IsZero() || ts.After(now) {
		// A client clock ahead of the server must not stamp events in the
		// future, where they would win every ordering check.
		ts = now
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkOwnerTx(ctx, tx, machineID, id); err != nil {
		return err
	}
	if err := insertEventTx(ctx, tx, id, ts, e.Source, e.Kind, payload); err != nil {
		return err
	}
	nowS := formatTS(now)
	ended := "NULL"
	if e.Kind == api.KindPaneClosed || e.Kind == api.KindEnded {
		ended = "COALESCE(ended_at, ?)"
	}
	q := "UPDATE sessions SET last_seen_at = ?, ended_at = " + ended
	args := []any{nowS}
	if ended != "NULL" {
		// The session ended (pane_closed or ended): its last Remote Control
		// link goes with it.
		q += ", rc_url = '', rc_at = NULL"
		args = append(args, nowS)
	}
	seen := false        // a plugin event moves the state from done to idle
	leftBlocked := false // the event moves the state out of blocked
	if state != "" {
		var last sql.NullString
		var prev string
		if err := tx.QueryRowContext(ctx, `SELECT state_ts, agent_state FROM sessions WHERE id = ?`, id).Scan(&last, &prev); err != nil {
			return err
		}
		// Apply the state unless a newer one is already in place. The event
		// row is stored either way.
		// Claude's Stop hook reports idle right after herdr reports done. That
		// idle is not herdr's "seen", so a hooks idle never leaves done.
		hooksIdleOverDone := e.Source == api.SourceHooks && prev == "done" && state == "idle"
		if (!last.Valid || formatTS(ts) >= last.String) && !hooksIdleOverDone {
			q += ", agent_state = ?, state_ts = ?, turn_ended_at = " + turnEndedExpr + ", blocked_at = " + blockedAtExpr
			args = append(args, state, formatTS(ts), state, formatTS(ts), state, formatTS(ts))
			if state != "blocked" {
				// What the session waited on is answered once it leaves
				// blocked: the hooks' prompt (working) and Stop (idle) clear it.
				q += ", blocked_on = ''"
			}
			seen = e.Source == api.SourcePlugin && prev == "done" && state == "idle"
			leftBlocked = prev == "blocked" && state != "blocked"
		}
	}
	if prompt != "" {
		// SQLite evaluates every SET expression on the old row, so both
		// columns compare against the stored time. An older event replayed
		// late leaves them alone.
		q += `, last_prompt = CASE WHEN last_prompt_at IS NULL OR last_prompt_at <= ? THEN ? ELSE last_prompt END,
			last_prompt_at = CASE WHEN last_prompt_at IS NULL OR last_prompt_at <= ? THEN ? ELSE last_prompt_at END`
		args = append(args, formatTS(ts), prompt, formatTS(ts), formatTS(ts))
	}
	args = append(args, id)
	if _, err := tx.ExecContext(ctx, q+" WHERE id = ?", args...); err != nil {
		return err
	}
	if seen {
		// herdr marked the pane seen: its Finished item is read.
		if err := s.autoDismissSeenTx(ctx, tx, id, now); err != nil {
			return err
		}
	}
	if leftBlocked {
		// The prompt was answered in the terminal. Only requests from
		// before this event's own time close: a newer one is a new prompt.
		if err := closePermissionsTx(ctx, tx, id, api.PermissionAnsweredLocally, ts, now); err != nil {
			return err
		}
	}
	if ended != "NULL" {
		if err := closePermissionsTx(ctx, tx, id, api.PermissionClosed, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func checkReportList(name string, items []string) error {
	if len(items) > MaxReportItems {
		return invalidf("%s has %d items, at most %d allowed", name, len(items), MaxReportItems)
	}
	for i, it := range items {
		if err := checkText(fmt.Sprintf("%s[%d]", name, i), it, MaxReportItemLen); err != nil {
			return err
		}
	}
	return nil
}

func listJSON(items []string) string {
	if items == nil {
		items = []string{}
	}
	b, _ := json.Marshal(items)
	return string(b)
}

// AddReport stores a progress report for session id.
func (s *Store) AddReport(ctx context.Context, machineID int64, id string, r api.ReportIn) error {
	for _, l := range []struct {
		name  string
		items []string
	}{{"done", r.Done}, {"in_flight", r.InFlight}, {"waiting_on", r.WaitingOn}} {
		if err := checkReportList(l.name, l.items); err != nil {
			return err
		}
	}
	if err := checkText("note", r.Note, MaxNoteRunes); err != nil {
		return err
	}
	now := formatTS(s.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkOwnerTx(ctx, tx, machineID, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO reports (session_id, ts, done_json, in_flight_json,
		waiting_on_json, note) VALUES (?, ?, ?, ?, ?, ?)`,
		id, now, listJSON(r.Done), listJSON(r.InFlight), listJSON(r.WaitingOn), r.Note); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, ended_at = NULL WHERE id = ?`, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetTitle sets a user title, which outranks every other title source.
func (s *Store) SetTitle(ctx context.Context, machineID int64, id, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return invalidf("title is empty")
	}
	if err := checkText("title", title, MaxTitleRunes); err != nil {
		return err
	}
	now := formatTS(s.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkOwnerTx(ctx, tx, machineID, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET title = ?, title_source = ?, last_seen_at = ?,
		ended_at = NULL WHERE id = ?`, title, TitleSourceUser, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ReconcileResult is the response to a herdr-sessions snapshot.
type ReconcileResult struct {
	Upserted  int       `json:"upserted"`
	Ended     []string  `json:"ended"`     // ended because they were missing from the snapshot
	Conflicts []string  `json:"conflicts"` // registered to another machine; skipped
	Invalid   []Invalid `json:"invalid"`   // failed validation; skipped
}

// Invalid names one snapshot entry the server skipped and why.
type Invalid struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// missingPayload is the payload of the ended event a snapshot records.
const missingPayload = `{"reason":"missing_from_snapshot"}`

// ReconcileHerdr upserts every session in the snapshot with source plugin,
// then ends this machine's paned sessions in the same herdr session that the
// snapshot no longer lists. Sessions without a pane are never touched. An
// entry registered to another machine is skipped and reported as a conflict,
// and an entry that fails validation is skipped and reported as invalid, so
// one bad entry cannot block the machine's heartbeat. The snapshot's own
// herdr_session must still be valid. An invalid entry still counts as listed,
// so its session is not ended as missing, and so does a session whose pane is
// in UnidentifiedPanes: Claude still runs there, herdr only lost its ID.
func (s *Store) ReconcileHerdr(ctx context.Context, machineID int64, put api.HerdrSessionsPut) (ReconcileResult, error) {
	res := ReconcileResult{Ended: []string{}, Conflicts: []string{}, Invalid: []Invalid{}}
	if put.HerdrSession == "" {
		return res, invalidf("herdr_session is empty")
	}
	if err := checkIdent("herdr_session", put.HerdrSession); err != nil {
		return res, err
	}
	seen := make(map[string]bool, len(put.Sessions))
	entries := make([]api.SessionUpsert, 0, len(put.Sessions))
	for _, u := range put.Sessions {
		u.Source = api.SourcePlugin
		if u.HerdrSession == "" {
			u.HerdrSession = put.HerdrSession
		}
		seen[u.ID] = true
		if err := validateUpsert(u); err != nil {
			res.Invalid = append(res.Invalid, Invalid{ID: u.ID, Reason: strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")})
			continue
		}
		entries = append(entries, u)
	}
	unidentified := make(map[string]bool, len(put.UnidentifiedPanes))
	for _, p := range put.UnidentifiedPanes {
		unidentified[p] = true
	}

	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	for _, u := range entries {
		if _, err := s.upsertTx(ctx, tx, machineID, u, now); err != nil {
			if errors.Is(err, ErrWrongMachine) {
				res.Conflicts = append(res.Conflicts, u.ID)
				continue
			}
			return res, err
		}
		res.Upserted++
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, herdr_pane FROM sessions WHERE machine_id = ? AND herdr_pane <> ''
		AND herdr_session = ? AND ended_at IS NULL ORDER BY id`, machineID, put.HerdrSession)
	if err != nil {
		return res, err
	}
	var gone []string
	for rows.Next() {
		var id, pane string
		if err := rows.Scan(&id, &pane); err != nil {
			rows.Close()
			return res, err
		}
		if !seen[id] && !unidentified[pane] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, id := range gone {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at = ?, rc_url = '', rc_at = NULL WHERE id = ?`, formatTS(now), id); err != nil {
			return res, err
		}
		if err := insertEventTx(ctx, tx, id, now, api.SourcePlugin, api.KindEnded, []byte(missingPayload)); err != nil {
			return res, err
		}
		if err := closePermissionsTx(ctx, tx, id, api.PermissionClosed, now, now); err != nil {
			return res, err
		}
		res.Ended = append(res.Ended, id)
	}
	return res, tx.Commit()
}

// blockedAtInsert is blocked_at for a new row: the time when it starts blocked.
func blockedAtInsert(state, nowS string) any {
	if state == "blocked" {
		return nowS
	}
	return nil
}
