// Package store is the sessionhub server's SQLite database: schema, queries, and the
// read-time status computation.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Errors returned by store methods. The server maps them to HTTP statuses.
var (
	ErrNotFound     = errors.New("not found")
	ErrInvalid      = errors.New("invalid")
	ErrWrongMachine = errors.New("conflict")
	// ErrNotControllable: the session has no herdr pane, or its machine's
	// watcher is offline. The server answers 409.
	ErrNotControllable = errors.New("not controllable")
	// ErrTooMany: a per-machine or global pending-request limit is reached. 429.
	ErrTooMany = errors.New("too many requests")
	// ErrRequestClosed: a result for a request that is not claimed. 409.
	ErrRequestClosed = errors.New("request closed")
	// ErrNameTaken: a browser session with the name exists. 409.
	ErrNameTaken = errors.New("name taken")
	// ErrCodeGone: the sign-in code is unknown, expired, or used. 410.
	ErrCodeGone = errors.New("sign-in link expired or already used")
	// ErrFull: the shared instructions would pass their size limit. 409.
	ErrFull = errors.New("full")
	// ErrGone: a permission request expired, was answered in the terminal,
	// or closed with its session. 410.
	ErrGone = errors.New("gone")
	// ErrInputCut: an allow for a permission request whose input was cut,
	// which the approver cannot have seen whole. 409.
	ErrInputCut = errors.New("the input was cut; allow it in the terminal")
)

// AmbiguousError is returned when an ID prefix matches more than one session.
type AmbiguousError struct {
	Prefix     string
	Candidates []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("id prefix %q matches %d sessions", e.Prefix, len(e.Candidates))
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// DefaultStaleAfter is used when Options.StaleAfter is zero.
const DefaultStaleAfter = 5 * time.Minute

// Options configures a Store.
type Options struct {
	// Now is the clock. Tests inject a fake one; nil means time.Now.
	Now func() time.Time
	// StaleAfter is how long after last_seen_at a session turns stale.
	StaleAfter time.Duration
}

// Store wraps the one *sql.DB the process uses.
type Store struct {
	db         *sql.DB
	now        func() time.Time
	staleAfter time.Duration
}

// Open opens (and creates or migrates) the database at path.
func Open(path string, opts Options) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database dir: %w", err)
	}
	// Pragmas go in the DSN so they apply to every connection the pool opens,
	// not only the first one.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// One connection serializes every write; SQLite allows one writer anyway.
	// Gotcha: inside a transaction, use the *sql.Tx for every query, or the
	// call waits forever for the connection the transaction holds.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: opts.Now, staleAfter: opts.StaleAfter}
	if s.now == nil {
		s.now = time.Now
	}
	if s.staleAfter <= 0 {
		s.staleAfter = DefaultStaleAfter
	}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Now returns the store's clock reading in UTC.
func (s *Store) Now() time.Time { return s.now().UTC() }

// StaleAfter returns the configured staleness threshold.
func (s *Store) StaleAfter() time.Duration { return s.staleAfter }

const schemaVersion = 12

const schemaV1 = `
CREATE TABLE machines (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	token_hash TEXT NOT NULL UNIQUE,      -- SHA-256 hex; the token itself is never stored
	ssh_host   TEXT NOT NULL DEFAULT '',
	herdr_host TEXT NOT NULL DEFAULT '',
	last_seen  TEXT                        -- NULL until the token is first used
);
CREATE TABLE sessions (
	id              TEXT PRIMARY KEY,      -- Claude session UUID
	machine_id      INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	agent           TEXT NOT NULL DEFAULT '',
	cwd             TEXT NOT NULL DEFAULT '',
	git_repo        TEXT NOT NULL DEFAULT '',
	git_branch      TEXT NOT NULL DEFAULT '',
	herdr_session   TEXT NOT NULL DEFAULT '',
	herdr_workspace TEXT NOT NULL DEFAULT '',
	herdr_pane      TEXT NOT NULL DEFAULT '', -- '' means no pane (hooks-only)
	title           TEXT NOT NULL DEFAULT '',
	title_source    TEXT NOT NULL DEFAULT '', -- user|herdr|prompt
	first_prompt    TEXT NOT NULL DEFAULT '',
	agent_state     TEXT NOT NULL DEFAULT '',
	started_at      TEXT NOT NULL,
	last_seen_at    TEXT NOT NULL,
	ended_at        TEXT
	-- No status column: status is computed at read time from ended_at,
	-- last_seen_at, and agent_state, so it can never go out of date.
);
CREATE INDEX sessions_machine ON sessions(machine_id);
CREATE TABLE events (
	id           INTEGER PRIMARY KEY,
	session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	ts           TEXT NOT NULL,
	source       TEXT NOT NULL,
	kind         TEXT NOT NULL,
	payload_json TEXT
);
CREATE INDEX events_session ON events(session_id, ts);
CREATE TABLE reports (
	id              INTEGER PRIMARY KEY,
	session_id      TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	ts              TEXT NOT NULL,
	done_json       TEXT NOT NULL,
	in_flight_json  TEXT NOT NULL,
	waiting_on_json TEXT NOT NULL,
	note            TEXT NOT NULL DEFAULT ''
);
CREATE INDEX reports_session ON reports(session_id, id);
`

// schemaV2 adds sessions.state_ts, the time of the last applied agent state.
// A state_changed event older than it is stored but does not change the
// state, so a queued event that arrives late cannot overwrite a newer one.
const schemaV2 = `ALTER TABLE sessions ADD COLUMN state_ts TEXT;`

// schemaV3 adds Remote Control requests: the control_requests table,
// machines.last_poll (the watcher's last long poll, which makes the
// machine's sessions controllable), and the session's last Remote Control
// link.
const schemaV3 = `
CREATE TABLE control_requests (
	id           TEXT PRIMARY KEY,        -- cr_ + 16 random bytes, base64url
	session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	machine_id   INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	action       TEXT NOT NULL,           -- remote_control
	state        TEXT NOT NULL,           -- pending|claimed|done|failed|expired
	requested_by TEXT NOT NULL,           -- dashboard | machine:<name>
	created_at   TEXT NOT NULL,
	expires_at   TEXT NOT NULL,
	claimed_at   TEXT,
	finished_at  TEXT,
	url          TEXT NOT NULL DEFAULT '',
	detail       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX control_requests_machine ON control_requests(machine_id, state, created_at);
CREATE INDEX control_requests_session ON control_requests(session_id, created_at);
ALTER TABLE machines ADD COLUMN last_poll TEXT;
ALTER TABLE sessions ADD COLUMN rc_url TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN rc_at TEXT;
`

// schemaV4 adds session digests (one row per session, replaced by newer
// ones) and the session's last prompt, filled once from existing prompt
// events. A malformed payload is skipped rather than failing the upgrade.
// sessions.title_source also takes "claude".
const schemaV4 = `
CREATE TABLE session_digests (
	session_id  TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
	as_of       TEXT NOT NULL,   -- the digest's last transcript entry; newer wins
	received_at TEXT NOT NULL,
	body        TEXT NOT NULL    -- api.DigestIn as JSON, validated
);
ALTER TABLE sessions ADD COLUMN last_prompt TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN last_prompt_at TEXT;
UPDATE sessions SET
	last_prompt = COALESCE((SELECT json_extract(CASE WHEN json_valid(e.payload_json) THEN e.payload_json END, '$.prompt')
		FROM events e
		WHERE e.session_id = sessions.id AND e.kind = 'prompt'
			AND json_type(CASE WHEN json_valid(e.payload_json) THEN e.payload_json END, '$.prompt') = 'text'
		ORDER BY e.ts DESC, e.id DESC LIMIT 1), ''),
	last_prompt_at = (SELECT e.ts FROM events e
		WHERE e.session_id = sessions.id AND e.kind = 'prompt'
			AND json_type(CASE WHEN json_valid(e.payload_json) THEN e.payload_json END, '$.prompt') = 'text'
		ORDER BY e.ts DESC, e.id DESC LIMIT 1);
`

// schemaV5 adds web sign-in: one-time login codes and browser sessions. Only
// SHA-256 hashes of codes and session tokens are stored. Removing a machine
// deletes the codes and sessions it created.
const schemaV5 = `
CREATE TABLE login_codes (
	code_hash  TEXT PRIMARY KEY,          -- SHA-256 hex of the code
	name       TEXT NOT NULL,             -- the name the session gets
	machine_id INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,             -- created_at + 10 minutes
	used_at    TEXT                       -- NULL until the code is used
);
CREATE INDEX login_codes_machine ON login_codes(machine_id);
CREATE TABLE web_sessions (
	id           TEXT PRIMARY KEY,        -- 8 random bytes, hex; public
	token_hash   TEXT NOT NULL UNIQUE,    -- SHA-256 hex of the cookie token
	name         TEXT NOT NULL UNIQUE,
	machine_id   INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	created_at   TEXT NOT NULL,
	last_used_at TEXT NOT NULL,
	expires_at   TEXT NOT NULL            -- last_used_at + 30 days
);
CREATE INDEX web_sessions_machine ON web_sessions(machine_id);
`

// schemaV6 adds the inbox: sessions.turn_ended_at, the time the agent last
// stopped working, sessions.blocked_at, the time it last became blocked (no
// backfill for either), and inbox_triage, one dismiss or snooze per
// session, shared by every viewer. A null snooze_until is a dismiss.
const schemaV6 = `
ALTER TABLE sessions ADD COLUMN turn_ended_at TEXT;
ALTER TABLE sessions ADD COLUMN blocked_at TEXT;  -- when agent_state last moved into blocked
CREATE TABLE inbox_triage (
	session_id    TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
	triaged_since TEXT NOT NULL,          -- the item's since when it was triaged
	snooze_until  TEXT,                   -- NULL for a dismiss
	updated_at    TEXT NOT NULL
);
`

// schemaV7 adds inbox_alerts: the blocked item each session was last sent a
// Telegram alert for. The notifier writes the row only after Telegram
// accepts the message, so a failed send is retried on the next tick.
const schemaV7 = `
CREATE TABLE inbox_alerts (
	session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
	since      TEXT NOT NULL,             -- the blocked item's since that was alerted on
	sent_at    TEXT NOT NULL
);
`

// schemaV8 adds shared instructions, messages to sessions, and permission
// requests. Messages and permission requests go with their session or
// machine.
const schemaV8 = `
CREATE TABLE instructions (
	id         INTEGER PRIMARY KEY,
	text       TEXT NOT NULL,
	created_at TEXT NOT NULL,
	created_by TEXT NOT NULL              -- machine name, or web:<session name>
);
CREATE TABLE messages (
	id         TEXT PRIMARY KEY,          -- msg_ + 16 random bytes, base64url
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	machine_id INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	text       TEXT NOT NULL,             -- as sent, without the prefix
	sender     TEXT NOT NULL,             -- dashboard | cli on <machine> | session <id8>
	limit_key  TEXT NOT NULL,             -- who the per-minute limit counts: web:<name> | machine:<name>
	state      TEXT NOT NULL,             -- queued|delivered|expired|refused
	detail     TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	offered_at TEXT                       -- the last time a watcher was handed it
);
CREATE INDEX messages_machine ON messages(machine_id, state, created_at);
CREATE INDEX messages_limit ON messages(limit_key, created_at);
CREATE INDEX messages_session ON messages(session_id, state, created_at);
CREATE TABLE permission_requests (
	id         TEXT PRIMARY KEY,          -- pr_ + 16 random bytes, base64url
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	machine_id INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	tool_name  TEXT NOT NULL,
	tool_input TEXT NOT NULL,             -- compact JSON, at most 8 KiB
	truncated  INTEGER NOT NULL DEFAULT 0,
	state      TEXT NOT NULL,             -- open|decided|expired|answered_locally|closed
	decision   TEXT NOT NULL DEFAULT '',  -- allow|deny once decided
	reason     TEXT NOT NULL DEFAULT '',
	decided_by TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	decided_at TEXT
);
CREATE INDEX permission_requests_session ON permission_requests(session_id, state, created_at);
`

// schemaV9 adds moves of a session to another machine or to the cloud, and
// each machine's move public key. A move goes with its session and with
// either machine. source_closed_at is NULL while the source still owes its
// finish (archive on done, restart otherwise).
const schemaV9 = `
ALTER TABLE machines ADD COLUMN move_key TEXT NOT NULL DEFAULT '';  -- X25519 public key, base64
CREATE TABLE moves (
	id                TEXT PRIMARY KEY,   -- mv_ + 16 random bytes, base64url
	session_id        TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	source_machine_id INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	target            TEXT NOT NULL,      -- a machine name, or cloud
	target_machine_id INTEGER REFERENCES machines(id) ON DELETE CASCADE, -- NULL for cloud
	state             TEXT NOT NULL,      -- requested|packing|uploaded|unpacking|done|failed|cancelled
	detail            TEXT NOT NULL DEFAULT '',
	created_at        TEXT NOT NULL,
	updated_at        TEXT NOT NULL,
	bundle_size       INTEGER NOT NULL DEFAULT 0,
	cloud_url         TEXT NOT NULL DEFAULT '',
	requested_by      TEXT NOT NULL,      -- web:<name> | machine:<name>
	source_closed_at  TEXT
);
CREATE INDEX moves_session ON moves(session_id, created_at);
CREATE INDEX moves_source ON moves(source_machine_id, state);
CREATE INDEX moves_target ON moves(target_machine_id, state);
`

// schemaV10 adds requests to start a new Claude session on a machine. A
// request goes with its machine; it has no session until Claude starts.
const schemaV10 = `
CREATE TABLE start_requests (
	id           TEXT PRIMARY KEY,        -- st_ + 16 random bytes, base64url
	machine_id   INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	dir          TEXT NOT NULL,
	prompt       TEXT NOT NULL DEFAULT '',
	state        TEXT NOT NULL,           -- pending|claimed|done|failed|expired
	requested_by TEXT NOT NULL,           -- web:<name> | machine:<name>
	created_at   TEXT NOT NULL,
	expires_at   TEXT NOT NULL,
	claimed_at   TEXT,
	finished_at  TEXT,
	url          TEXT NOT NULL DEFAULT '',
	detail       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX start_requests_machine ON start_requests(machine_id, state, created_at);
`

// schemaV11 adds a start request's trust flag: mark its directory trusted in
// Claude's config before starting.
const schemaV11 = `ALTER TABLE start_requests ADD COLUMN trust INTEGER NOT NULL DEFAULT 0;`

// schemaV12 adds what the Claude Code mod reports: the question a blocked
// session waits on, the context window fill and cost after each turn, and
// when the session's mod last polled for messages. The usage columns stay
// NULL until a mod reports them.
const schemaV12 = `
ALTER TABLE sessions ADD COLUMN blocked_on TEXT NOT NULL DEFAULT '';  -- one cleaned line, at most 300 runes
ALTER TABLE sessions ADD COLUMN context_percent INTEGER;              -- 0 to 100
ALTER TABLE sessions ADD COLUMN usage_at TEXT;                        -- when the mod last reported usage
ALTER TABLE sessions ADD COLUMN live_cost_usd REAL;
ALTER TABLE sessions ADD COLUMN mod_seen_at TEXT;                     -- the mod's last message poll
`

// afterVersionRead runs in migrate between the unlocked version read and the
// transaction. Tests use it to line two opens up on the same stale version.
var afterVersionRead = func() {}

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	afterVersionRead()
	switch {
	case v == schemaVersion:
		return nil
	case v > schemaVersion:
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", v, schemaVersion)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Another process may have migrated the file between the read above and
	// this transaction taking the write lock (_txlock=immediate). Read the
	// version again under the lock so two concurrent opens do not both run
	// the migration; the second one would fail on a duplicate column.
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("re-read schema version: %w", err)
	}
	switch {
	case v == schemaVersion:
		return nil
	case v > schemaVersion:
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", v, schemaVersion)
	}
	if v < 1 {
		if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
	}
	if v < 2 {
		if _, err := tx.ExecContext(ctx, schemaV2); err != nil {
			return fmt.Errorf("migrate schema to v2: %w", err)
		}
	}
	if v < 3 {
		if _, err := tx.ExecContext(ctx, schemaV3); err != nil {
			return fmt.Errorf("migrate schema to v3: %w", err)
		}
	}
	if v < 4 {
		if _, err := tx.ExecContext(ctx, schemaV4); err != nil {
			return fmt.Errorf("migrate schema to v4: %w", err)
		}
	}
	if v < 5 {
		if _, err := tx.ExecContext(ctx, schemaV5); err != nil {
			return fmt.Errorf("migrate schema to v5: %w", err)
		}
	}
	if v < 6 {
		if _, err := tx.ExecContext(ctx, schemaV6); err != nil {
			return fmt.Errorf("migrate schema to v6: %w", err)
		}
	}
	if v < 7 {
		if _, err := tx.ExecContext(ctx, schemaV7); err != nil {
			return fmt.Errorf("migrate schema to v7: %w", err)
		}
	}
	if v < 8 {
		if _, err := tx.ExecContext(ctx, schemaV8); err != nil {
			return fmt.Errorf("migrate schema to v8: %w", err)
		}
	}
	if v < 9 {
		if _, err := tx.ExecContext(ctx, schemaV9); err != nil {
			return fmt.Errorf("migrate schema to v9: %w", err)
		}
	}
	if v < 10 {
		if _, err := tx.ExecContext(ctx, schemaV10); err != nil {
			return fmt.Errorf("migrate schema to v10: %w", err)
		}
	}
	if v < 11 {
		if _, err := tx.ExecContext(ctx, schemaV11); err != nil {
			return fmt.Errorf("migrate schema to v11: %w", err)
		}
	}
	if v < 12 {
		if _, err := tx.ExecContext(ctx, schemaV12); err != nil {
			return fmt.Errorf("migrate schema to v12: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// tsLayout is RFC 3339 in UTC with a fixed nine-digit fraction, so stored
// times also sort correctly as strings.
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTS(t time.Time) string { return t.UTC().Format(tsLayout) }

func parseTS(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("stored time %q: %w", s, err)
	}
	return t.UTC(), nil
}

func parseNullTS(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := parseTS(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
