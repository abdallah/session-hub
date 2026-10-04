package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// Timeout is the default per-request limit for a sessionhub call. Hooks, the MCP
// server, the herdr watcher, and digests keep it, so a slow server never holds
// up Claude Code.
const Timeout = 2 * time.Second

// InteractiveTimeout is the per-request limit for commands a person runs and
// waits for (sessionhub ls, sessionhub login, sessionhub inbox, sessionhub resume). It allows for a slow
// tunnel between the machine and the server.
const InteractiveTimeout = 15 * time.Second

// StatusError is returned for any non-2xx response. Message is the
// api.Error message when the body carried one, else the raw body (trimmed).
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("sessionhub: HTTP %d: %s", e.Status, e.Message)
}

// Client talks to the sessionhub server with bearer auth.
type Client struct {
	base    string
	token   string
	http    *http.Client
	timeout time.Duration
}

// New builds a Client from cfg. It errors when server_url is empty.
func New(cfg Config) (*Client, error) {
	if cfg.ServerURL == "" {
		return nil, errors.New("sessionhub: server_url is not configured")
	}
	return &Client{
		base:  strings.TrimRight(cfg.ServerURL, "/"),
		token: cfg.Token,
		// No http.Client.Timeout: doTimeout bounds every call with a context
		// deadline, which the long poll sets longer than Timeout.
		http:    &http.Client{},
		timeout: Timeout,
	}, nil
}

// SetTimeout changes the per-request limit for every call except the long
// poll, which sets its own.
func (c *Client) SetTimeout(d time.Duration) { c.timeout = d }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	return c.doTimeout(ctx, c.timeout, method, path, in, out)
}

// PollSlack is added to a long poll's wait for its timeout.
const PollSlack = 10 * time.Second

// PollControl holds GET /v1/machines/self/control open for up to wait
// (whole seconds, at least 1) and returns the request this machine claimed,
// or nil when none arrived (204).
func (c *Client) PollControl(ctx context.Context, wait time.Duration) (*api.ControlClaim, error) {
	secs := max(int(wait/time.Second), 1)
	var raw json.RawMessage
	if err := c.doTimeout(ctx, time.Duration(secs)*time.Second+PollSlack, http.MethodGet,
		"/v1/machines/self/control?wait="+strconv.Itoa(secs), nil, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil // 204
	}
	var claim api.ControlClaim
	if err := json.Unmarshal(raw, &claim); err != nil {
		return nil, err
	}
	if claim.Request.ID == "" {
		return nil, errors.New("sessionhub: control poll answered without a request")
	}
	return &claim, nil
}

// PostControlResult reports what happened to a claimed request.
func (c *Client) PostControlResult(ctx context.Context, id string, in api.ControlResultIn) error {
	return c.do(ctx, http.MethodPost, "/v1/control/"+url.PathEscape(id)+"/result", in, nil)
}

func (c *Client) doTimeout(ctx context.Context, timeout time.Duration, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return responseError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// UpsertSession registers or refreshes one session.
func (c *Client) UpsertSession(ctx context.Context, s api.SessionUpsert) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions", s, nil)
}

// PutHerdrSessions replaces this machine's set of herdr agent panes.
func (c *Client) PutHerdrSessions(ctx context.Context, p api.HerdrSessionsPut) error {
	_, err := c.PutHerdrSessionsResult(ctx, p)
	return err
}

// InvalidEntry is a snapshot entry the server skipped, and why.
type InvalidEntry struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// HerdrSessionsResult is the server's response to a herdr-sessions snapshot.
type HerdrSessionsResult struct {
	Upserted  int            `json:"upserted"`
	Ended     []string       `json:"ended"`
	Conflicts []string       `json:"conflicts"`
	Invalid   []InvalidEntry `json:"invalid"`
}

// PutHerdrSessionsResult is PutHerdrSessions that also returns the response,
// so a caller can log the entries the server skipped.
func (c *Client) PutHerdrSessionsResult(ctx context.Context, p api.HerdrSessionsPut) (HerdrSessionsResult, error) {
	var res HerdrSessionsResult
	err := c.do(ctx, http.MethodPut, "/v1/machines/self/herdr-sessions", p, &res)
	return res, err
}

// PostEvent records one event for a session.
func (c *Client) PostEvent(ctx context.Context, id string, e api.EventIn) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/events", e, nil)
}

// PostReport records a progress report for a session.
func (c *Client) PostReport(ctx context.Context, id string, r api.ReportIn) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/report", r, nil)
}

// SetTitle sets the user title of a session.
func (c *Client) SetTitle(ctx context.Context, id, title string) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/title", api.TitleIn{Title: title}, nil)
}

// PutDigest sends a session's digest.
func (c *Client) PutDigest(ctx context.Context, id string, d api.DigestIn) error {
	return c.do(ctx, http.MethodPut, "/v1/sessions/"+url.PathEscape(id)+"/digest", d, nil)
}

// ListSessions lists sessions. live=true limits to live and blocked
// sessions (stale and ended are left out); machine="" means all machines. The
// server returns a bare JSON array.
func (c *Client) ListSessions(ctx context.Context, live bool, machine string) ([]api.Session, error) {
	q := url.Values{}
	if live {
		q.Set("live", "true")
	}
	if machine != "" {
		q.Set("machine", machine)
	}
	path := "/v1/sessions"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out []api.Session
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetSession fetches one session with reports and events. idOrPrefix may be a
// unique prefix.
func (c *Client) GetSession(ctx context.Context, idOrPrefix string) (api.SessionDetail, error) {
	var d api.SessionDetail
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(idOrPrefix), nil, &d)
	return d, err
}

// CreateControl asks the sessionhub to turn on Remote Control for session id. It
// returns the new request, or the session's open one.
func (c *Client) CreateControl(ctx context.Context, id string) (api.ControlRequest, error) {
	var r api.ControlRequest
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/remote-control", struct{}{}, &r)
	return r, err
}

// ListMachines lists known machines.
func (c *Client) ListMachines(ctx context.Context) ([]api.Machine, error) {
	var out []api.Machine
	if err := c.do(ctx, http.MethodGet, "/v1/machines", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Health calls GET /healthz and returns nil on 2xx.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil)
}

// CreateLogin asks for a one-time sign-in link for a browser named name.
// Nothing is queued: an unreachable server is an error.
func (c *Client) CreateLogin(ctx context.Context, name string) (api.Login, error) {
	var out api.Login
	err := c.do(ctx, http.MethodPost, "/v1/logins", api.LoginIn{Name: name}, &out)
	return out, err
}

// ListWebSessions lists signed-in browsers.
func (c *Client) ListWebSessions(ctx context.Context) ([]api.WebSession, error) {
	var out api.WebSessionList
	if err := c.do(ctx, http.MethodGet, "/v1/web-sessions", nil, &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

// RevokeWebSession signs out the browser session with id.
func (c *Client) RevokeWebSession(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/web-sessions/"+url.PathEscape(id), nil, nil)
}

// Inbox reads the sessions that need you.
func (c *Client) Inbox(ctx context.Context) (api.Inbox, error) {
	var out api.Inbox
	err := c.do(ctx, http.MethodGet, "/v1/inbox", nil, &out)
	return out, err
}

// DismissInbox hides session id's inbox item until a trigger later than
// since. Pass the since the inbox returned, unchanged.
func (c *Client) DismissInbox(ctx context.Context, id string, since time.Time) error {
	return c.do(ctx, http.MethodPost, "/v1/inbox/"+url.PathEscape(id)+"/dismiss", api.TriageIn{Since: since}, nil)
}

// SnoozeInbox hides session id's inbox item until until, or until a trigger
// later than since.
func (c *Client) SnoozeInbox(ctx context.Context, id string, since, until time.Time) error {
	return c.do(ctx, http.MethodPost, "/v1/inbox/"+url.PathEscape(id)+"/snooze", api.TriageIn{Since: since, Until: &until}, nil)
}

// Instructions reads the standing rules and their version.
func (c *Client) Instructions(ctx context.Context) (api.InstructionList, error) {
	var out api.InstructionList
	err := c.do(ctx, http.MethodGet, "/v1/instructions", nil, &out)
	return out, err
}

// AddInstruction adds a standing rule.
func (c *Client) AddInstruction(ctx context.Context, text string) (api.Instruction, error) {
	var out api.Instruction
	err := c.do(ctx, http.MethodPost, "/v1/instructions", api.InstructionIn{Text: text}, &out)
	return out, err
}

// DeleteInstruction removes standing rule id.
func (c *Client) DeleteInstruction(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, "/v1/instructions/"+strconv.FormatInt(id, 10), nil, nil)
}

// SendMessages sends a message to sessions. Each target's result says
// whether it was queued or refused.
func (c *Client) SendMessages(ctx context.Context, in api.MessagesIn) (api.MessagesOut, error) {
	var out api.MessagesOut
	err := c.do(ctx, http.MethodPost, "/v1/messages", in, &out)
	return out, err
}

// GetMessage reads one message and its delivery state.
func (c *Client) GetMessage(ctx context.Context, id string) (api.Message, error) {
	var out api.Message
	err := c.do(ctx, http.MethodGet, "/v1/messages/"+url.PathEscape(id), nil, &out)
	return out, err
}

// CreatePermission posts a permission prompt of session sessionID.
func (c *Client) CreatePermission(ctx context.Context, sessionID string, in api.PermissionIn) (api.PermissionRequest, error) {
	var out api.PermissionRequest
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/permissions", in, &out)
	return out, err
}

// WaitDecision holds GET /v1/permissions/{id}/decision open for up to wait
// (whole seconds, at least 1) and returns the request once it is not open,
// or nil when the wait ended first (204).
func (c *Client) WaitDecision(ctx context.Context, id string, wait time.Duration) (*api.PermissionRequest, error) {
	secs := max(int(wait/time.Second), 1)
	var raw json.RawMessage
	if err := c.doTimeout(ctx, time.Duration(secs)*time.Second+PollSlack, http.MethodGet,
		"/v1/permissions/"+url.PathEscape(id)+"/decision?wait="+strconv.Itoa(secs), nil, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil // 204
	}
	var out api.PermissionRequest
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DecidePermission allows or denies permission request id, once.
func (c *Client) DecidePermission(ctx context.Context, id string, in api.DecisionIn) (api.PermissionRequest, error) {
	var out api.PermissionRequest
	err := c.do(ctx, http.MethodPost, "/v1/permissions/"+url.PathEscape(id)+"/decide", in, &out)
	return out, err
}
