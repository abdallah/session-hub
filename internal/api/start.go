package api

import "time"

// Starting a new Claude session on a machine. See docs/server.md, "Starting
// sessions".
const (
	// HeaderActionStart is the X-Hub-Action value for
	// POST /v1/machines/{name}/start.
	HeaderActionStart = "start"
	// ActionStart is the action of a start claim.
	ActionStart = "start"
	// MaxStartDirBytes caps a start request's directory.
	MaxStartDirBytes = 1024
	// MaxStartPromptRunes caps a start request's first prompt.
	MaxStartPromptRunes = 4000
)

// StartIn is the body of POST /v1/machines/{name}/start.
type StartIn struct {
	// Dir is the absolute directory to start Claude in, on that machine.
	Dir string `json:"dir"`
	// Prompt is an optional first prompt, submitted once Claude is up.
	Prompt string `json:"prompt,omitempty"`
}

// StartRequest is a request to start a new Claude session on a machine,
// with Remote Control on. State is ControlPending, ControlClaimed,
// ControlDone, ControlFailed, or ControlExpired.
type StartRequest struct {
	ID          string     `json:"id"`
	Machine     string     `json:"machine"`
	Dir         string     `json:"dir"`
	Prompt      string     `json:"prompt,omitempty"`
	State       string     `json:"state"`
	RequestedBy string     `json:"requested_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	ClaimedAt   *time.Time `json:"claimed_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	// URL is the new session's Remote Control link, on a done result when
	// the watcher saw it.
	URL    string `json:"url,omitempty"`
	Detail string `json:"detail,omitempty"`
}
