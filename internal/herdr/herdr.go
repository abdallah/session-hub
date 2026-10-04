// Package herdr is a small client for the herdr Unix socket API (NDJSON: one
// JSON request per line, one JSON response per line) plus helpers for the
// environment herdr gives plugin commands.
package herdr

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/abdallah/session-hub/internal/paths"
)

// Deadline is the per-call limit for socket requests.
const Deadline = 2 * time.Second

// SocketPath returns the herdr socket path: HERDR_SOCKET_PATH, else
// ~/.config/herdr/herdr.sock.
func SocketPath() string { return paths.HerdrSocket() }

// SessionName maps a socket path to the herdr session name: "<name>" for
// .../sessions/<name>/herdr.sock, else "default".
func SessionName(socketPath string) string {
	dir := filepath.Dir(socketPath)
	if filepath.Base(filepath.Dir(dir)) == "sessions" && filepath.Base(socketPath) == "herdr.sock" {
		return filepath.Base(dir)
	}
	return "default"
}

// Error is an error response from herdr (for example code "pane_not_found").
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("herdr: %s: %s", e.Code, e.Message) }

// Client talks to one herdr socket. herdr closes a connection after it
// answers one request (observed on 0.9.3: a second request on the same
// connection fails with a broken pipe), so every call opens its own
// connection. Calls are safe for concurrent use.
type Client struct {
	path string
	seq  atomic.Int64
}

// Dial checks that the socket at path accepts connections (empty means
// SocketPath()) and returns a Client for it.
func Dial(path string) (*Client, error) {
	if path == "" {
		path = SocketPath()
	}
	conn, err := net.DialTimeout("unix", path, Deadline)
	if err != nil {
		return nil, err
	}
	conn.Close()
	return &Client{path: path}, nil
}

// Close is a no-op kept so callers can defer it; there is no held connection.
func (c *Client) Close() error { return nil }

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

// Call sends one request on a fresh connection and returns the raw "result"
// object. An error response becomes *Error. Connect, write, and read share one
// Deadline.
func (c *Client) Call(method string, params any) (json.RawMessage, error) {
	if params == nil {
		params = struct{}{}
	}
	id := fmt.Sprintf("hub_%d", c.seq.Add(1))
	b, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(Deadline)
	conn, err := net.DialTimeout("unix", c.path, Deadline)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(conn, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var resp response
		if err := json.Unmarshal(line, &resp); err != nil {
			return nil, fmt.Errorf("herdr: bad response: %w", err)
		}
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// AgentSessionInfo is herdr's read-only reference to a native agent session.
// For Claude, Value is the session UUID (Kind "id").
type AgentSessionInfo struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
}

// PaneInfo is the part of herdr's pane object that sessionhub reads. Unknown fields
// are ignored.
type PaneInfo struct {
	PaneID        string            `json:"pane_id"`
	TerminalID    string            `json:"terminal_id"`
	WorkspaceID   string            `json:"workspace_id"`
	TabID         string            `json:"tab_id"`
	Focused       bool              `json:"focused"`
	CWD           string            `json:"cwd"`
	ForegroundCWD string            `json:"foreground_cwd"`
	Label         string            `json:"label"`
	Agent         string            `json:"agent"`
	Title         string            `json:"terminal_title"`
	TitleClean    string            `json:"terminal_title_stripped"`
	AgentStatus   string            `json:"agent_status"`
	AgentSession  *AgentSessionInfo `json:"agent_session"`
	Tokens        map[string]string `json:"tokens"`
	Revision      int64             `json:"revision"`
}

// SessionID returns the agent session UUID, or "" when herdr has none.
func (p PaneInfo) SessionID() string {
	if p.AgentSession == nil {
		return ""
	}
	return p.AgentSession.Value
}

// AgentInfo is one entry of the snapshot's "agents" list.
type AgentInfo struct {
	TerminalID   string            `json:"terminal_id"`
	PaneID       string            `json:"pane_id"`
	WorkspaceID  string            `json:"workspace_id"`
	TabID        string            `json:"tab_id"`
	Agent        string            `json:"agent"`
	Title        string            `json:"terminal_title"`
	TitleClean   string            `json:"terminal_title_stripped"`
	AgentStatus  string            `json:"agent_status"`
	AgentSession *AgentSessionInfo `json:"agent_session"`
	CWD          string            `json:"cwd"`
	Focused      bool              `json:"focused"`
}

// Snapshot is the part of session.snapshot that sessionhub reads.
type Snapshot struct {
	Version            string          `json:"version"`
	Protocol           int             `json:"protocol"`
	FocusedWorkspaceID string          `json:"focused_workspace_id"`
	FocusedTabID       string          `json:"focused_tab_id"`
	FocusedPaneID      string          `json:"focused_pane_id"`
	Workspaces         []WorkspaceInfo `json:"workspaces"`
	Panes              []PaneInfo      `json:"panes"`
	Agents             []AgentInfo     `json:"agents"`
}

// WorkspaceInfo is the part of a snapshot workspace sessionhub reads.
type WorkspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
}

// Snapshot calls session.snapshot.
func (c *Client) Snapshot() (Snapshot, error) {
	res, err := c.Call("session.snapshot", nil)
	if err != nil {
		return Snapshot{}, err
	}
	var out struct {
		Snapshot Snapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return Snapshot{}, err
	}
	return out.Snapshot, nil
}

// PaneGet calls pane.get. A missing pane returns *Error with Code
// "pane_not_found".
func (c *Client) PaneGet(id string) (PaneInfo, error) {
	res, err := c.Call("pane.get", map[string]string{"pane_id": id})
	if err != nil {
		return PaneInfo{}, err
	}
	return decodePane(res)
}

func decodePane(res json.RawMessage) (PaneInfo, error) {
	var out struct {
		Pane PaneInfo `json:"pane"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return PaneInfo{}, err
	}
	if out.Pane.PaneID == "" {
		return PaneInfo{}, errors.New("herdr: response has no pane")
	}
	return out.Pane, nil
}

// PaneFocus calls pane.focus.
func (c *Client) PaneFocus(id string) error {
	_, err := c.Call("pane.focus", map[string]string{"pane_id": id})
	return err
}

// PaneClose calls pane.close, which closes the pane and ends whatever runs
// in it. A missing pane returns *Error with Code "pane_not_found".
func (c *Client) PaneClose(id string) error {
	_, err := c.Call("pane.close", map[string]string{"pane_id": id})
	return err
}

// PaneSplitParams mirrors herdr's PaneSplitParams (Direction is "right" or
// "down"; it is required).
type PaneSplitParams struct {
	Direction    string  `json:"direction"`
	CWD          string  `json:"cwd,omitempty"`
	TargetPaneID string  `json:"target_pane_id,omitempty"`
	WorkspaceID  string  `json:"workspace_id,omitempty"`
	Focus        bool    `json:"focus"`
	Ratio        float64 `json:"ratio,omitempty"`
}

// PaneSplit calls pane.split and returns the new pane (result.pane, captured
// in testdata/herdr/socket/pane-split.ndjson). It errors if the response
// carries no pane ID, so callers never start an agent in pane "".
func (c *Client) PaneSplit(p PaneSplitParams) (PaneInfo, error) {
	res, err := c.Call("pane.split", p)
	if err != nil {
		return PaneInfo{}, err
	}
	return decodePane(res)
}

// AgentStartParams mirrors herdr's AgentStartParams. Name, Kind, and PaneID
// are required by herdr. TimeoutMS must be 0 (omitted) or 3001..300000.
type AgentStartParams struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	PaneID    string   `json:"pane_id"`
	Args      []string `json:"args,omitempty"`
	TimeoutMS int      `json:"timeout_ms,omitempty"`
}

// AgentStart calls agent.start and returns the raw result. The response shape
// was not captured.
func (c *Client) AgentStart(p AgentStartParams) (json.RawMessage, error) {
	return c.Call("agent.start", p)
}

// WorkspaceCreateParams is the part of herdr's WorkspaceCreateParams sessionhub
// sends. Focus false leaves the user's focus where it is.
type WorkspaceCreateParams struct {
	CWD   string `json:"cwd,omitempty"`
	Label string `json:"label,omitempty"`
	Focus bool   `json:"focus"`
}

// WorkspaceCreated is the part of a workspace.create result sessionhub reads.
type WorkspaceCreated struct {
	WorkspaceID string
	RootPane    PaneInfo
}

// WorkspaceCreate calls workspace.create (captured in
// testdata/herdr/socket/workspace-create.ndjson). It errors when the result
// has no workspace or root pane, so no caller starts an agent in pane "".
func (c *Client) WorkspaceCreate(p WorkspaceCreateParams) (WorkspaceCreated, error) {
	res, err := c.Call("workspace.create", p)
	if err != nil {
		return WorkspaceCreated{}, err
	}
	var out struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane PaneInfo `json:"root_pane"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return WorkspaceCreated{}, err
	}
	if out.Workspace.WorkspaceID == "" || out.RootPane.PaneID == "" {
		return WorkspaceCreated{}, errors.New("herdr: workspace.create response has no workspace or root pane")
	}
	return WorkspaceCreated{WorkspaceID: out.Workspace.WorkspaceID, RootPane: out.RootPane}, nil
}

// BlockedError is the error AgentPrompt returns when herdr refuses the prompt
// because the agent is blocked on an interactive prompt (error code
// "agent_blocked"). herdr sends no input in that case. It unwraps to the
// underlying *Error.
type BlockedError struct{ Err *Error }

func (e *BlockedError) Error() string { return e.Err.Error() }
func (e *BlockedError) Unwrap() error { return e.Err }

// AgentPrompt calls agent.prompt: it submits text to the agent in target (a
// pane ID) as if typed and entered. Captured in
// testdata/herdr/socket/agent-prompt-ok.ndjson. A blocked agent returns
// *BlockedError (agent-prompt-blocked.ndjson).
func (c *Client) AgentPrompt(target, text string) error {
	_, err := c.Call("agent.prompt", map[string]string{"target": target, "text": text})
	var he *Error
	if errors.As(err, &he) && he.Code == "agent_blocked" {
		return &BlockedError{Err: he}
	}
	return err
}

// SourceRecentUnwrapped is the pane.read source for recent scrollback with
// soft-wrapped lines joined. On the wire it is "recent_unwrapped"; the CLI
// spells it "recent-unwrapped".
const SourceRecentUnwrapped = "recent_unwrapped"

// SourceVisible is the pane.read source for the screen as it is drawn now,
// with no scrollback. The wire value is "visible".
const SourceVisible = "visible"

// PaneRead calls pane.read and returns result.read.text: the last lines lines
// of the pane's terminal text from source. A lines value of 0 or less omits
// the limit, which suits SourceVisible. Captured in
// testdata/herdr/socket/pane-read-*.ndjson. The text is terminal output from
// another process: clean it before printing.
func (c *Client) PaneRead(paneID, source string, lines int) (string, error) {
	params := map[string]any{"pane_id": paneID, "source": source}
	if lines > 0 {
		params["lines"] = lines
	}
	res, err := c.Call("pane.read", params)
	if err != nil {
		return "", err
	}
	var out struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	return out.Read.Text, nil
}

// SendKeys calls pane.send_keys: it presses keys in a pane's terminal, using
// herdr's key names ("esc" is the canonical Escape; captured in
// testdata/herdr/socket/pane-send-keys-esc.ndjson). It sends input to
// whatever runs in the pane, so callers must have checked the pane first.
func (c *Client) SendKeys(paneID string, keys ...string) error {
	_, err := c.Call("pane.send_keys", map[string]any{"pane_id": paneID, "keys": keys})
	return err
}

// ReportMetadataParams mirrors the fields of herdr's PaneReportMetadataParams
// that sessionhub uses. A nil token value clears that token.
type ReportMetadataParams struct {
	PaneID string             `json:"pane_id"`
	Source string             `json:"source"`
	Tokens map[string]*string `json:"tokens,omitempty"`
	TTLMS  int                `json:"ttl_ms,omitempty"`
}

// SummaryToken is the token key sessionhub writes for the sidebar.
const SummaryToken = "hub_summary"

// MaxSummary is the longest hub_summary value.
const MaxSummary = 80

// ReportMetadata calls pane.report_metadata.
func (c *Client) ReportMetadata(p ReportMetadataParams) error {
	_, err := c.Call("pane.report_metadata", p)
	return err
}

// SetSummary writes hub_summary (source "sessionhub") for a pane, truncating to
// MaxSummary runes. An empty summary clears the token.
func (c *Client) SetSummary(paneID, summary string) error {
	p := ReportMetadataParams{PaneID: paneID, Source: "sessionhub", Tokens: map[string]*string{SummaryToken: nil}}
	if summary != "" {
		if r := []rune(summary); len(r) > MaxSummary {
			summary = string(r[:MaxSummary])
		}
		p.Tokens[SummaryToken] = &summary
	}
	return c.ReportMetadata(p)
}

// NotificationParams mirrors herdr's NotificationShowParams. Sound is "none",
// "done", or "request".
type NotificationParams struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Sound string `json:"sound,omitempty"`
}

// NotificationShow calls notification.show. When herdr shows nothing, it
// answers shown=false with a reason. "disabled" (the user turned
// notifications off) and "no_foreground_client" (nobody is watching) are not
// errors. Any other reason (rate_limited, busy) comes back as an error.
func (c *Client) NotificationShow(p NotificationParams) error {
	res, err := c.Call("notification.show", p)
	if err != nil {
		return err
	}
	var out struct {
		Shown  bool   `json:"shown"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return err
	}
	if !out.Shown && out.Reason != "disabled" && out.Reason != "no_foreground_client" {
		return fmt.Errorf("herdr: notification not shown: %s", out.Reason)
	}
	return nil
}

// Event is HERDR_PLUGIN_EVENT_JSON: {"event": "...", "data": {...}}.
type Event struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// Event kinds sessionhub hooks, as they appear in the payload (snake_case).
const (
	EventPaneCreated       = "pane_created"
	EventPaneClosed        = "pane_closed"
	EventPaneExited        = "pane_exited"
	EventAgentDetected     = "pane_agent_detected"
	EventAgentStatusChange = "pane_agent_status_changed"
)

// ParseEvent decodes an event payload. The event name is normalized to
// snake_case, so "pane.closed" and "pane_closed" both work.
func ParseEvent(data []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(data, &e); err != nil {
		return Event{}, err
	}
	if e.Event == "" {
		return Event{}, fmt.Errorf("herdr: event payload has no \"event\" field")
	}
	e.Event = strings.ReplaceAll(e.Event, ".", "_")
	return e, nil
}

// EventFromEnv parses HERDR_PLUGIN_EVENT_JSON.
func EventFromEnv() (Event, error) {
	v := os.Getenv("HERDR_PLUGIN_EVENT_JSON")
	if v == "" {
		return Event{}, fmt.Errorf("herdr: HERDR_PLUGIN_EVENT_JSON is not set")
	}
	return ParseEvent([]byte(v))
}

// PaneRef is the payload of pane_closed and pane_exited.
type PaneRef struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
}

// AgentDetected is the payload of pane_agent_detected.
type AgentDetected struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	Agent       string `json:"agent"`
	FinalStatus string `json:"final_status"`
	Released    bool   `json:"released"`
}

// AgentStatusChanged is the payload of pane_agent_status_changed. It carries
// no session ID: look the pane up with PaneGet.
type AgentStatusChanged struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	AgentStatus string `json:"agent_status"`
	Agent       string `json:"agent"`
	Title       string `json:"title"`
}

func (e Event) decode(kind string, v any) error {
	if e.Event != kind {
		return fmt.Errorf("herdr: event is %q, not %q", e.Event, kind)
	}
	return json.Unmarshal(e.Data, v)
}

// PaneCreated decodes a pane_created event.
func (e Event) PaneCreated() (PaneInfo, error) {
	var d struct {
		Pane PaneInfo `json:"pane"`
	}
	err := e.decode(EventPaneCreated, &d)
	return d.Pane, err
}

// PaneClosed decodes a pane_closed event.
func (e Event) PaneClosed() (PaneRef, error) {
	var d PaneRef
	err := e.decode(EventPaneClosed, &d)
	return d, err
}

// PaneExited decodes a pane_exited event. No payload was captured; the shape
// comes from herdr's schema (see docs/dev/NOTES.md).
func (e Event) PaneExited() (PaneRef, error) {
	var d PaneRef
	err := e.decode(EventPaneExited, &d)
	return d, err
}

// AgentDetected decodes a pane_agent_detected event.
func (e Event) AgentDetected() (AgentDetected, error) {
	var d AgentDetected
	err := e.decode(EventAgentDetected, &d)
	return d, err
}

// AgentStatusChanged decodes a pane_agent_status_changed event.
func (e Event) AgentStatusChanged() (AgentStatusChanged, error) {
	var d AgentStatusChanged
	err := e.decode(EventAgentStatusChange, &d)
	return d, err
}

// WorkspaceMetadataParams mirrors herdr's WorkspaceReportMetadataParams:
// display-only tokens on a workspace, which the sidebar shows as $name. A
// nil token value clears that token. Tokens is required by herdr.
type WorkspaceMetadataParams struct {
	WorkspaceID string             `json:"workspace_id"`
	Source      string             `json:"source"`
	Tokens      map[string]*string `json:"tokens"`
}

// ReportWorkspaceMetadata calls workspace.report_metadata.
func (c *Client) ReportWorkspaceMetadata(p WorkspaceMetadataParams) error {
	_, err := c.Call("workspace.report_metadata", p)
	return err
}

// TabInfo is the part of herdr's tab object sessionhub reads.
type TabInfo struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
}

// TabList calls tab.list without a workspace: every tab of this herdr
// session.
func (c *Client) TabList() ([]TabInfo, error) {
	res, err := c.Call("tab.list", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tabs []TabInfo `json:"tabs"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	return out.Tabs, nil
}

// TabFocus calls tab.focus.
func (c *Client) TabFocus(id string) error {
	_, err := c.Call("tab.focus", map[string]string{"tab_id": id})
	return err
}

// TabCreateParams is the part of herdr's TabCreateParams sessionhub sends. An empty
// WorkspaceID lets herdr pick the focused workspace. herdr's tab.create
// cannot run a command.
type TabCreateParams struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	Label       string `json:"label,omitempty"`
	Focus       bool   `json:"focus"`
}

// TabCreated is the part of a tab.create result sessionhub reads.
type TabCreated struct {
	Tab      TabInfo
	RootPane PaneInfo
}

// TabCreate calls tab.create. It errors when the result has no tab or root
// pane, so no caller runs a command in pane "".
func (c *Client) TabCreate(p TabCreateParams) (TabCreated, error) {
	res, err := c.Call("tab.create", p)
	if err != nil {
		return TabCreated{}, err
	}
	var out struct {
		Tab      TabInfo  `json:"tab"`
		RootPane PaneInfo `json:"root_pane"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return TabCreated{}, err
	}
	if out.Tab.TabID == "" || out.RootPane.PaneID == "" {
		return TabCreated{}, errors.New("herdr: tab.create response has no tab or root pane")
	}
	return TabCreated{Tab: out.Tab, RootPane: out.RootPane}, nil
}
