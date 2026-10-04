package api

import (
	"encoding/json"
	"time"
)

const (
	StatusLive    = "live"
	StatusStale   = "stale"
	StatusEnded   = "ended"
	StatusBlocked = "blocked"

	SourcePlugin = "plugin"
	SourceHooks  = "hooks"
	SourceMCP    = "mcp"

	KindRegistered   = "registered"
	KindStateChanged = "state_changed"
	KindPrompt       = "prompt"
	KindPaneClosed   = "pane_closed"
	KindEnded        = "ended"
	// KindDigest is recorded by the server when a digest changes the recap
	// or the set of links.
	KindDigest = "digest"
)

// Remote Control requests. See docs/server.md, "Remote Control requests".
const (
	// SourceServer marks events the server records itself.
	SourceServer = "server"

	KindRemoteControlRequested = "remote_control_requested"
	KindRemoteControlResult    = "remote_control_result"

	// ActionRemoteControl is the only action a control request carries.
	ActionRemoteControl = "remote_control"

	ControlPending = "pending"
	ControlClaimed = "claimed"
	ControlDone    = "done"
	ControlFailed  = "failed"
	ControlExpired = "expired"

	// ControlRequest.RequestedBy is RequestedByDashboard, or
	// RequestedByMachinePrefix followed by the machine name.
	RequestedByDashboard     = "dashboard"
	RequestedByMachinePrefix = "machine:"

	// The dashboard sends HeaderAction: HeaderActionRemoteControl with its
	// one write, POST /v1/sessions/{id}/remote-control.
	HeaderAction              = "X-Hub-Action"
	HeaderActionRemoteControl = "remote-control"
	// HeaderActionSignOut is the X-Hub-Action value the dashboard sends with
	// POST /logout.
	HeaderActionSignOut = "sign-out"
	// HeaderActionTriage is the X-Hub-Action value the dashboard sends with
	// POST /v1/inbox/{id}/dismiss and /snooze.
	HeaderActionTriage = "triage"
	// HeaderSessionName carries the browser session's name on GET
	// /v1/sessions when the caller used the session cookie.
	HeaderSessionName = "X-Hub-Session-Name"

	// MaxControlDetailRunes caps ControlResultIn.Detail.
	MaxControlDetailRunes = 200
)

// ControlRequest is one request to act on a session on its machine.
type ControlRequest struct {
	ID          string     `json:"id"`
	SessionID   string     `json:"session_id"`
	Machine     string     `json:"machine"`
	Action      string     `json:"action"`
	State       string     `json:"state"`
	RequestedBy string     `json:"requested_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	ClaimedAt   *time.Time `json:"claimed_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	URL         string     `json:"url,omitempty"`
	Detail      string     `json:"detail,omitempty"`
}

// ControlResultIn is the body of POST /v1/control/{id}/result.
type ControlResultIn struct {
	State  string `json:"state"` // done|failed
	URL    string `json:"url,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ControlClaim is a request the machine just claimed, with its session: the
// body of a 200 from GET /v1/machines/self/control.
type ControlClaim struct {
	Request ControlRequest `json:"request"`
	Session Session        `json:"session"`
	// Text is the prompt to submit, for a request whose action is
	// ActionMessage.
	Text string `json:"text,omitempty"`
	// Move is set for ActionMoveOut and ActionMoveIn.
	Move *MoveClaim `json:"move,omitempty"`
	// Start is set for ActionStart. Session is empty then: the session
	// doesn't exist yet.
	Start *StartRequest `json:"start,omitempty"`
}

// SessionUpsert registers or refreshes one session. Empty fields never
// overwrite stored values.
type SessionUpsert struct {
	ID             string `json:"id"`     // Claude session UUID
	Agent          string `json:"agent"`  // "claude"
	Source         string `json:"source"` // plugin|hooks|mcp
	CWD            string `json:"cwd,omitempty"`
	GitRepo        string `json:"git_repo,omitempty"`
	GitBranch      string `json:"git_branch,omitempty"`
	HerdrSession   string `json:"herdr_session,omitempty"`
	HerdrWorkspace string `json:"herdr_workspace,omitempty"`
	HerdrPane      string `json:"herdr_pane,omitempty"`
	AgentState     string `json:"agent_state,omitempty"`
	TitleHint      string `json:"title_hint,omitempty"`   // herdr terminal_title_stripped
	FirstPrompt    string `json:"first_prompt,omitempty"` // truncated to 200 chars by the client
}

// HerdrSessionsPut is the full set of herdr agent panes on the calling machine.
type HerdrSessionsPut struct {
	HerdrSession string          `json:"herdr_session"`
	Sessions     []SessionUpsert `json:"sessions"`
	// UnidentifiedPanes lists panes that run Claude but have no session ID
	// in herdr. The server does not end a session registered to one of them.
	UnidentifiedPanes []string `json:"unidentified_panes,omitempty"`
}

type EventIn struct {
	Kind    string          `json:"kind"`
	Source  string          `json:"source"`
	TS      time.Time       `json:"ts"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type ReportIn struct {
	Done      []string `json:"done"`
	InFlight  []string `json:"in_flight"`
	WaitingOn []string `json:"waiting_on"`
	Note      string   `json:"note,omitempty"`
}

type TitleIn struct {
	Title string `json:"title"`
}

type Report struct {
	TS        time.Time `json:"ts"`
	Done      []string  `json:"done"`
	InFlight  []string  `json:"in_flight"`
	WaitingOn []string  `json:"waiting_on"`
	Note      string    `json:"note,omitempty"`
}

type Event struct {
	TS      time.Time       `json:"ts"`
	Source  string          `json:"source"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Session struct {
	ID             string     `json:"id"`
	Agent          string     `json:"agent"`
	Machine        string     `json:"machine"`
	CWD            string     `json:"cwd,omitempty"`
	GitRepo        string     `json:"git_repo,omitempty"`
	GitBranch      string     `json:"git_branch,omitempty"`
	HerdrSession   string     `json:"herdr_session,omitempty"`
	HerdrWorkspace string     `json:"herdr_workspace,omitempty"`
	HerdrPane      string     `json:"herdr_pane,omitempty"`
	Title          string     `json:"title,omitempty"`
	TitleSource    string     `json:"title_source,omitempty"` // user|herdr|prompt
	StartedAt      time.Time  `json:"started_at"`
	LastSeenAt     time.Time  `json:"last_seen_at"`
	Status         string     `json:"status"`
	AgentState     string     `json:"agent_state,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	ResumeCommand  string     `json:"resume_command"` // ssh -t <ssh_host> '~/.local/bin/sessionhub resume <id>'
	LatestReport   *Report    `json:"latest_report,omitempty"`
	// RemoteControl is the session's latest control request.
	RemoteControl *ControlRequest `json:"remote_control,omitempty"`
	// RemoteControlURL and RemoteControlAt are the last Remote Control link
	// a machine reported, cleared when the session ends.
	RemoteControlURL string     `json:"remote_control_url,omitempty"`
	RemoteControlAt  *time.Time `json:"remote_control_at,omitempty"`
	// Controllable is true when the session has a herdr pane and its
	// machine's watcher polled for requests in the last 2 minutes.
	Controllable bool `json:"controllable"`
	// Recap and RecapAt are Claude's latest recap, from the digest.
	Recap   string     `json:"recap,omitempty"`
	RecapAt *time.Time `json:"recap_at,omitempty"`
	// LastPrompt and LastPromptAt are the newest prompt event's text (the
	// first 200 characters the hooks client sends) and time.
	LastPrompt   string     `json:"last_prompt,omitempty"`
	LastPromptAt *time.Time `json:"last_prompt_at,omitempty"`
	// Summary is the digest's counts for the card; nil with no digest.
	Summary *SessionSummary `json:"summary,omitempty"`
	// Move is the session's latest move, open or not; nil with none.
	Move *Move `json:"move,omitempty"`
}

type SessionDetail struct {
	Session
	Reports []Report `json:"reports"`          // newest first
	Events  []Event  `json:"events"`           // newest first, at most 50
	Digest  *Digest  `json:"digest,omitempty"` // nil until a machine sends one
}

type Machine struct {
	Name      string     `json:"name"`
	SSHHost   string     `json:"ssh_host"`
	HerdrHost string     `json:"herdr_host"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	// MoveKey is the fingerprint of the machine's move key, or "".
	MoveKey string `json:"move_key,omitempty"`
	// MoveReady is true when the machine has a move key and its watcher
	// polled in the last 2 minutes: it can take a moved session.
	MoveReady bool `json:"move_ready"`
	// Online is true when the machine's watcher polled in the last 2
	// minutes: it can take a start request.
	Online bool `json:"online"`
}

type Error struct {
	Error string `json:"error"`
}

// DigestLink is one merge request or pull request a session created.
type DigestLink struct {
	Number string `json:"number,omitempty"`
	URL    string `json:"url"`
	Repo   string `json:"repo,omitempty"`
}

// DigestTokens sums the usage of every assistant reply, each counted once,
// including subagents.
type DigestTokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// DigestCommit is one commit made during the session.
type DigestCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
}

// GitCounts are the git numbers the card shows. Unpushed is -1 when the
// branch has no upstream.
type GitCounts struct {
	CommitCount int `json:"commit_count"`
	Uncommitted int `json:"uncommitted"`
	Unpushed    int `json:"unpushed"`
}

// DigestGit is the git activity in the session's window.
type DigestGit struct {
	GitCounts
	Commits []DigestCommit `json:"commits"` // newest first, at most 5
}

// DigestIn is the body of PUT /v1/sessions/{id}/digest: everything a machine
// learned about a session from its transcript and git. Each digest replaces
// the previous one.
type DigestIn struct {
	AsOf        time.Time    `json:"as_of"` // the last transcript entry's time
	FirstAt     *time.Time   `json:"first_at,omitempty"`
	Recap       string       `json:"recap,omitempty"`
	RecapAt     *time.Time   `json:"recap_at,omitempty"`
	AITitle     string       `json:"ai_title,omitempty"`
	CustomTitle string       `json:"custom_title,omitempty"`
	Links       []DigestLink `json:"links,omitempty"`
	Tokens      DigestTokens `json:"tokens"`
	CostUSD     *float64     `json:"cost_usd,omitempty"`
	CostAt      *time.Time   `json:"cost_at,omitempty"`
	Git         *DigestGit   `json:"git,omitempty"`
	BadLines    int          `json:"bad_lines"`
}

// Digest is a stored digest and when the server received it.
type Digest struct {
	DigestIn
	ReceivedAt time.Time `json:"received_at"`
}

// SessionSummary is the part of the digest the session list carries.
type SessionSummary struct {
	AsOf         time.Time   `json:"as_of"`
	Git          *GitCounts  `json:"git,omitempty"`
	LatestLink   *DigestLink `json:"latest_link,omitempty"`
	CostUSD      *float64    `json:"cost_usd,omitempty"`
	OutputTokens int64       `json:"output_tokens"`
}

// LoginIn is the body of POST /v1/logins.
type LoginIn struct {
	Name string `json:"name"`
}

// Login is a one-time sign-in link, from POST /v1/logins.
type Login struct {
	URL       string    `json:"url"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
}

// WebSession is one signed-in browser. It never carries the token or its
// hash.
type WebSession struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Machine    string    `json:"machine"` // the machine whose link created it
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// WebSessionList is the response of GET /v1/web-sessions.
type WebSessionList struct {
	Sessions []WebSession `json:"sessions"`
}

// Inbox groups, in precedence order. See docs/server.md, "Inbox".
const (
	InboxBlocked  = "blocked"
	InboxWaiting  = "waiting"
	InboxFinished = "finished"
)

// InboxItem is one session that needs you. Since is the trigger time; a
// dismiss or snooze hides the item until a later Since. WaitingOn is set for
// the waiting group only.
type InboxItem struct {
	Group     string    `json:"group"`
	Since     time.Time `json:"since"`
	WaitingOn []string  `json:"waiting_on,omitempty"`
	Session   Session   `json:"session"`
	// Permission is the session's newest open permission request, on a
	// blocked item only.
	Permission *PermissionRequest `json:"permission,omitempty"`
}

// InboxCounts counts the items GET /v1/inbox returns, per group.
type InboxCounts struct {
	Blocked  int `json:"blocked"`
	Waiting  int `json:"waiting"`
	Finished int `json:"finished"`
}

// Inbox is the response of GET /v1/inbox: items sorted by group, then since.
type Inbox struct {
	Items  []InboxItem `json:"items"`
	Counts InboxCounts `json:"counts"`
}

// TriageIn is the body of POST /v1/inbox/{id}/dismiss (Since only) and
// /snooze (Since and Until). Since is the item's since as the caller showed
// it.
type TriageIn struct {
	Since time.Time  `json:"since"`
	Until *time.Time `json:"until,omitempty"`
}
