package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// Shared instructions, messages to sessions, and remote permission answers.
// See docs/server.md, "Shared instructions", "Messages to sessions", and
// "Permission requests".
const (
	// X-Hub-Action values the dashboard sends with these writes.
	HeaderActionInstructions = "instructions" // POST /v1/instructions, DELETE /v1/instructions/{id}
	HeaderActionSend         = "send"         // POST /v1/messages
	HeaderActionApprove      = "approve"      // POST /v1/permissions/{id}/decide

	// ActionMessage is the control request kind that carries a message.
	ActionMessage = "message"

	// Event kinds the server records.
	KindMessage    = "message"
	KindPermission = "permission"

	// Message states. MessageBusy is a watcher result only: the agent is
	// working or blocked, and the message stays queued.
	MessageQueued    = "queued"
	MessageDelivered = "delivered"
	MessageExpired   = "expired"
	MessageRefused   = "refused"
	MessageBusy      = "busy"

	// Permission request states.
	PermissionOpen            = "open"
	PermissionDecided         = "decided"
	PermissionExpired         = "expired"
	PermissionAnsweredLocally = "answered_locally"
	PermissionClosed          = "closed"

	DecisionAllow = "allow"
	DecisionDeny  = "deny"

	// SenderDashboard is the sender of a message from a browser.
	SenderDashboard = "dashboard"
)

// Instruction is one standing rule.
type Instruction struct {
	ID        int64     `json:"id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

// InstructionList is the response of GET /v1/instructions, oldest first.
// Version changes whenever the list does.
type InstructionList struct {
	Instructions []Instruction `json:"instructions"`
	Version      string        `json:"version"`
}

// InstructionIn is the body of POST /v1/instructions.
type InstructionIn struct {
	Text string `json:"text"`
}

// MessagesIn is the body of POST /v1/messages.
type MessagesIn struct {
	SessionIDs []string `json:"session_ids"`
	Text       string   `json:"text"`
	// FromSession is the sending session's ID, which the MCP tool sets. The
	// server ignores it for a browser.
	FromSession string `json:"from_session,omitempty"`
}

// MessageResult is what happened to one target of a send: queued with an
// ID, or refused with the reason.
type MessageResult struct {
	SessionID string `json:"session_id"`
	ID        string `json:"id,omitempty"`
	State     string `json:"state"`
	Detail    string `json:"detail,omitempty"`
}

// MessagesOut is the response of POST /v1/messages, one result per distinct
// target, in the order sent.
type MessagesOut struct {
	Results []MessageResult `json:"results"`
}

// Message is one stored message, from GET /v1/messages/{id}. Text is the
// text as sent, without the prefix.
type Message struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Machine   string    `json:"machine"`
	Sender    string    `json:"sender"`
	Text      string    `json:"text"`
	State     string    `json:"state"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PermissionIn is the body of POST /v1/sessions/{id}/permissions: the
// PermissionRequest hook's input.
type PermissionIn struct {
	ToolName    string          `json:"tool_name"`
	ToolInput   json.RawMessage `json:"tool_input,omitempty"`
	CWD         string          `json:"cwd,omitempty"`
	Suggestions json.RawMessage `json:"suggestions,omitempty"`
}

// PermissionRequest is one permission prompt waiting for, or past, an
// answer.
type PermissionRequest struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	Machine   string          `json:"machine"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	Truncated bool            `json:"truncated,omitempty"`
	State     string          `json:"state"`
	Decision  string          `json:"decision,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	DecidedBy string          `json:"decided_by,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	DecidedAt *time.Time      `json:"decided_at,omitempty"`
}

// DecisionIn is the body of POST /v1/permissions/{id}/decide.
type DecisionIn struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// permissionFields are the tool input fields that say what a tool will do,
// in the order PermissionInputText tries them.
var permissionFields = []string{"command", "file_path", "notebook_path", "path", "url", "pattern", "query", "prompt"}

// PermissionInputText is the part of a tool input a person reads to decide:
// the first non-empty string among permissionFields, a cut input (stored as
// a JSON string) as itself, else the compact JSON. The result is untrusted
// text: clean it before printing.
func PermissionInputText(input json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(input, &m) == nil {
		for _, k := range permissionFields {
			var s string
			if v, ok := m[k]; ok && json.Unmarshal(v, &s) == nil && s != "" {
				return s
			}
		}
	}
	var s string
	if json.Unmarshal(input, &s) == nil {
		return s
	}
	var buf bytes.Buffer
	if json.Compact(&buf, input) == nil {
		return buf.String()
	}
	return string(input)
}

// MessagePrompt is the text the watcher submits for a message: who sent it,
// a blank line, and the text. A person's message (sender "dashboard" or
// "cli on <machine>") reads "From the user via sessionhub (<sender>):"; a session's
// (sender "session <8 characters>") reads "From session <8 characters> via
// sessionhub (sent on the user's behalf):", so the reader can tell an agent from
// the person.
func MessagePrompt(sender, text string) string {
	if id, ok := strings.CutPrefix(sender, "session "); ok {
		return "From session " + id + " via sessionhub (sent on the user's behalf):\n\n" + text
	}
	return "From the user via sessionhub (" + sender + "):\n\n" + text
}

// MaxToolInputBytes caps a permission request's tool input, as compact JSON.
const MaxToolInputBytes = 8 << 10

// cutToolInputBytes is how much of a bigger input is kept.
const cutToolInputBytes = 8000

// CapToolInput compacts a tool input. An empty or null input is {}. An
// input over MaxToolInputBytes becomes a JSON string holding its first
// 8,000 bytes (fewer when escapes make the encoding longer, cut on a rune
// boundary) and "…", and cut is true. The result is at most MaxToolInputBytes. Both the
// hook and the server apply it, so a big input never passes the server's
// 64 KiB body limit.
func CapToolInput(input json.RawMessage) (out json.RawMessage, cut bool, err error) {
	in := bytes.TrimSpace(input)
	if len(in) == 0 || string(in) == "null" {
		return json.RawMessage(`{}`), false, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, in); err != nil {
		return nil, false, err
	}
	if buf.Len() <= MaxToolInputBytes {
		return json.RawMessage(buf.Bytes()), false, nil
	}
	kept := buf.Bytes()[:cutToolInputBytes]
	for {
		for len(kept) > 0 && !utf8.Valid(kept) {
			kept = kept[:len(kept)-1]
		}
		s, err := encodeJSONString(string(kept) + "…")
		if err != nil {
			return nil, false, err
		}
		if len(s) <= MaxToolInputBytes {
			return json.RawMessage(s), true, nil
		}
		// Escapes (quotes, backslashes, control characters) grow the encoding:
		// drop at least the overshoot, and every dropped byte saves one.
		kept = kept[:len(kept)-(len(s)-MaxToolInputBytes)]
	}
}

// encodeJSONString encodes s without HTML escaping, so <, >, and & cost one
// byte each.
func encodeJSONString(s string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
