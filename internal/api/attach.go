package api

// Attach: a terminal mirror of one session, relayed through the server.
//
// A guest (or the owner) asks POST /v1/sessions/{id}/attach. The server
// creates a control request with ActionAttach for the session's machine and
// answers with the Attach. The guest then reads terminal bytes from
// GET /v1/attach/{id}/output (a chunked stream) and sends keystrokes with
// POST /v1/attach/{id}/input (raw bytes). The machine's watcher claims the
// request, reads guest input from GET /v1/machines/self/attach/{id}/input (a
// chunked stream), posts terminal bytes to
// POST /v1/machines/self/attach/{id}/output (raw bytes), and posts the
// control result when the mirror ends. Both streams are plain terminal
// bytes: a snapshot mirror sends clear-screen then the frame; a byte-stream
// mirror sends what the terminal emits.
const (
	// ActionAttach is the control request action for a terminal mirror.
	ActionAttach = "attach"
	// HeaderActionAttach is the X-Hub-Action value a cookie sign-in sends
	// with the attach routes.
	HeaderActionAttach = "attach"
)

// Attach is the response of POST /v1/sessions/{id}/attach.
type Attach struct {
	// ID is the attach ID, which is also the control request ID.
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Machine   string `json:"machine"`
}
