package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/paths"
)

type hubAPI interface {
	UpsertSession(ctx context.Context, s api.SessionUpsert) error
	PostReport(ctx context.Context, id string, r api.ReportIn) error
	SetTitle(ctx context.Context, id, title string) error
	Instructions(ctx context.Context) (api.InstructionList, error)
	AddInstruction(ctx context.Context, text string) (api.Instruction, error)
	DeleteInstruction(ctx context.Context, id int64) error
	SendMessages(ctx context.Context, in api.MessagesIn) (api.MessagesOut, error)
}

type queuer interface {
	Append(client.Item) error
}

// Server holds the dependencies of the MCP tools. Use NewServer for the real
// ones; tests substitute fields.
type Server struct {
	// API returns the sessionhub client; it errors when the server is not configured.
	API func() (hubAPI, error)
	// Queue holds calls that failed to reach the server.
	Queue queuer
	// StateDir holds current/<claude-pid>.
	StateDir string
	// PPID returns the Claude Code process ID (this process's parent).
	PPID func() int
}

// NewServer returns a Server wired to the real config, queue, and process.
func NewServer() *Server {
	return &Server{
		API: func() (hubAPI, error) {
			cfg, err := client.LoadConfig()
			if err != nil {
				return nil, err
			}
			return client.New(cfg)
		},
		Queue:    client.DefaultQueue(),
		StateDir: paths.StateDir(),
		PPID:     os.Getppid,
	}
}

// resolveSession finds the current Claude session ID. Order:
// <state>/current/<ppid> (written by the SessionStart hook, cheap, and updated
// on /clear and /resume), then herdr pane.get on HERDR_PANE_ID, then
// CLAUDE_CODE_SESSION_ID. It runs on every call because the ID changes on
// /clear and /resume.
func (s *Server) resolveSession() string {
	if s.PPID != nil {
		b, err := os.ReadFile(filepath.Join(s.StateDir, "current", strconv.Itoa(s.PPID())))
		if id := strings.TrimSpace(string(b)); err == nil && id != "" {
			return id
		}
	}
	if pane := os.Getenv("HERDR_PANE_ID"); pane != "" {
		if id, err := herdrSession(pane); err != nil {
			log.Printf("herdr pane.get %s: %v", pane, err)
		} else if id != "" {
			return id
		}
	}
	return os.Getenv("CLAUDE_CODE_SESSION_ID")
}

func herdrSession(pane string) (string, error) {
	c, err := herdr.Dial(herdr.SocketPath())
	if err != nil {
		return "", err
	}
	defer c.Close()
	p, err := c.PaneGet(pane)
	if err != nil {
		return "", err
	}
	return p.SessionID(), nil
}

const noSession = "sessionhub: could not determine this session's ID, so nothing was reported. Continue with your work."

func (s *Server) reportProgress(ctx context.Context, args json.RawMessage) toolResult {
	var in struct {
		Done      []string `json:"done"`
		InFlight  []string `json:"in_flight"`
		WaitingOn []string `json:"waiting_on"`
		Note      string   `json:"note"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return text("invalid arguments: "+reason(err.Error()), true)
	}
	r := api.ReportIn{
		Done:      clamp(in.Done),
		InFlight:  clamp(in.InFlight),
		WaitingOn: clamp(in.WaitingOn),
		Note:      clean(in.Note, maxNoteLen),
	}
	id := s.resolveSession()
	if id == "" {
		return text(noSession, false)
	}

	if pane := os.Getenv("HERDR_PANE_ID"); pane != "" {
		if err := setSummary(pane, Summary(r)); err != nil {
			log.Printf("herdr report_metadata: %v", err)
		}
	}

	body, _ := json.Marshal(r)
	return s.send(ctx, id, client.OpReport, body, "report", func(a hubAPI) error { return a.PostReport(ctx, id, r) })
}

func (s *Server) setTitle(ctx context.Context, args json.RawMessage) toolResult {
	var in struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return text("invalid arguments: "+reason(err.Error()), true)
	}
	title := clean(in.Title, maxTitleLen)
	if title == "" {
		return text("title must not be empty", true)
	}
	id := s.resolveSession()
	if id == "" {
		return text(noSession, false)
	}
	body, _ := json.Marshal(api.TitleIn{Title: title})
	return s.send(ctx, id, client.OpTitle, body, "title", func(a hubAPI) error { return a.SetTitle(ctx, id, title) })
}

// send delivers one call and fails open: if the server is unreachable or
// errors in a way a retry could fix, the call is queued and the tool still
// succeeds.
func (s *Server) send(ctx context.Context, id, op string, body []byte, what string, call func(hubAPI) error) toolResult {
	a, err := s.API()
	if err == nil {
		err = call(a)
		if client.IsNotFound(err) {
			// Unknown session: register it minimally and retry once.
			if err = a.UpsertSession(ctx, minimalSession(id)); err == nil {
				err = call(a)
			}
		}
	}
	if err == nil {
		return text(fmt.Sprintf("sessionhub: %s recorded.", what), false)
	}
	log.Printf("%s: %v", what, err)
	var se *client.StatusError
	if !client.IsRetryable(err) && !client.IsNotFound(err) {
		// A retry cannot fix this, so do not queue it.
		msg := serverMessage(err)
		if errors.As(err, &se) {
			msg = fmt.Sprintf("%d %s", se.Status, reason(se.Message))
		}
		return text(fmt.Sprintf("sessionhub: the server rejected the %s (%s). Continue with your work.", what, msg), false)
	}
	if qerr := s.Queue.Append(client.Item{Op: op, SessionID: id, Body: body, QueuedAt: time.Now().UTC()}); qerr != nil {
		log.Printf("queue: %v", qerr)
		return text(fmt.Sprintf("sessionhub: the server is unreachable and the %s could not be queued. Continue with your work.", what), false)
	}
	return text(fmt.Sprintf("sessionhub: the server is unreachable, so the %s was queued and will be sent later.", what), false)
}

func setSummary(pane, summary string) error {
	c, err := herdr.Dial(herdr.SocketPath())
	if err != nil {
		return err
	}
	defer c.Close()
	return c.SetSummary(pane, summary)
}

// Summary builds the herdr sidebar text: the first waiting_on item prefixed
// "waiting: ", else the first in_flight item, else "done: " plus the first
// done item, truncated to 80 runes. It is empty when the report has no items.
func Summary(r api.ReportIn) string {
	var s string
	switch {
	case len(r.WaitingOn) > 0:
		s = "waiting: " + r.WaitingOn[0]
	case len(r.InFlight) > 0:
		s = r.InFlight[0]
	case len(r.Done) > 0:
		s = "done: " + r.Done[0]
	}
	if rs := []rune(s); len(rs) > herdr.MaxSummary {
		s = string(rs[:herdr.MaxSummary])
	}
	return s
}

// Server caps on free text. The server rejects text over these limits and
// text with control characters, so the client shortens and cleans instead: a
// model-written report is never rejected.
const (
	maxItems    = 20
	maxItemLen  = 200
	maxTitleLen = 200
	maxNoteLen  = 2000
)

// clean replaces every control character (C0, DEL, and C1, including tab and
// newline) with a space, trims surrounding space, and cuts the result to max
// runes.
func clean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max]))
	}
	return s
}

// clamp returns a non-nil slice of at most maxItems cleaned items of at most
// maxItemLen runes each. Items that are empty after cleaning are dropped.
func clamp(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if len(out) == maxItems {
			break
		}
		if s = clean(s, maxItemLen); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// minimalSession describes this session from the environment, for a session
// the server has not seen (it started before sessionhub was installed).
func minimalSession(id string) api.SessionUpsert {
	u := api.SessionUpsert{
		ID:             id,
		Agent:          "claude",
		Source:         api.SourceMCP,
		CWD:            os.Getenv("CLAUDE_PROJECT_DIR"),
		HerdrPane:      os.Getenv("HERDR_PANE_ID"),
		HerdrWorkspace: os.Getenv("HERDR_WORKSPACE_ID"),
	}
	if u.HerdrPane != "" {
		u.HerdrSession = herdr.SessionName(herdr.SocketPath())
	}
	return u
}

// maxRuleLen is the server's limit on one rule (store.MaxInstructionRunes).
const maxRuleLen = 300

// maxReasonLen caps an error text a tool result quotes.
const maxReasonLen = 300

// reason is an error text as a tool result quotes it: control characters
// replaced and cut to maxReasonLen runes. Server and network errors are
// untrusted, so every one passes through here.
func reason(msg string) string { return clean(msg, maxReasonLen) }

// serverMessage is a short reason for a failed call: the server's message
// for an HTTP error, else the error text, through reason.
func serverMessage(err error) string {
	var se *client.StatusError
	if errors.As(err, &se) {
		return reason(se.Message)
	}
	return reason(err.Error())
}

// remember adds a standing rule. Nothing is queued: the user asked for it
// now, so a failure is reported.
func (s *Server) remember(ctx context.Context, args json.RawMessage) toolResult {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return text("invalid arguments: "+reason(err.Error()), true)
	}
	rule := strings.Join(strings.Fields(clean(in.Text, 1<<20)), " ")
	if rule == "" {
		return text("text is empty: say the rule in one line", true)
	}
	if n := len([]rune(rule)); n > maxRuleLen {
		return text(fmt.Sprintf("the rule is %d characters; keep it to %d", n, maxRuleLen), true)
	}
	a, err := s.API()
	if err != nil {
		return text("sessionhub: the rule could not be saved: "+serverMessage(err), true)
	}
	r, err := a.AddInstruction(ctx, rule)
	var se *client.StatusError
	switch {
	case errors.As(err, &se) && se.Status == 409:
		return text("sessionhub: the rules are full ("+reason(se.Message)+"). Ask the user which rule to remove, call forget with its ID, then try again.", true)
	case err != nil:
		return text("sessionhub: the rule could not be saved: "+serverMessage(err), true)
	}
	return text(fmt.Sprintf("sessionhub: rule %d saved: %q. Every session on every machine sees it from its next prompt.", r.ID, r.Text), false)
}

// forget removes a standing rule by ID.
func (s *Server) forget(ctx context.Context, args json.RawMessage) toolResult {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.ID <= 0 {
		return text("invalid arguments: id must be a rule number from the instructions tool", true)
	}
	a, err := s.API()
	if err != nil {
		return text("sessionhub: the rule could not be removed: "+serverMessage(err), true)
	}
	if err := a.DeleteInstruction(ctx, in.ID); err != nil {
		if client.IsNotFound(err) {
			return text(fmt.Sprintf("sessionhub: there is no rule %d; call instructions to list them.", in.ID), true)
		}
		return text("sessionhub: the rule could not be removed: "+serverMessage(err), true)
	}
	return text(fmt.Sprintf("sessionhub: rule %d removed.", in.ID), false)
}

// instructions lists the standing rules with their IDs.
func (s *Server) instructions(ctx context.Context, _ json.RawMessage) toolResult {
	a, err := s.API()
	if err != nil {
		return text("sessionhub: could not read the rules: "+serverMessage(err), true)
	}
	list, err := a.Instructions(ctx)
	if err != nil {
		return text("sessionhub: could not read the rules: "+serverMessage(err), true)
	}
	if len(list.Instructions) == 0 {
		return text("sessionhub: there are no standing rules.", false)
	}
	lines := []string{"sessionhub standing rules:"}
	for _, r := range list.Instructions {
		lines = append(lines, fmt.Sprintf("%d. %s (added by %s)", r.ID, clean(r.Text, maxRuleLen), clean(r.CreatedBy, 100)))
	}
	return text(strings.Join(lines, "\n"), false)
}

// sendToSessions sends a message to other sessions as this one.
func (s *Server) sendToSessions(ctx context.Context, args json.RawMessage) toolResult {
	var in struct {
		SessionIDs []string `json:"session_ids"`
		Text       string   `json:"text"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return text("invalid arguments: "+reason(err.Error()), true)
	}
	if len(in.SessionIDs) == 0 || len(in.SessionIDs) > 20 {
		return text("session_ids must list 1 to 20 full session IDs", true)
	}
	msg := strings.TrimSpace(in.Text)
	if msg == "" {
		return text("text is empty", true)
	}
	from := s.resolveSession()
	if from == "" {
		return text("sessionhub: could not determine this session's ID, so the message was not sent.", true)
	}
	a, err := s.API()
	if err != nil {
		return text("sessionhub: the message could not be sent: "+serverMessage(err), true)
	}
	out, err := a.SendMessages(ctx, api.MessagesIn{SessionIDs: in.SessionIDs, Text: msg, FromSession: from})
	var se *client.StatusError
	switch {
	case errors.As(err, &se) && se.Status == 404:
		return text("sessionhub: the message was not sent: a session ID is unknown, or this session is not registered with sessionhub yet ("+reason(se.Message)+"). Check the IDs with sessionhub ls.", true)
	case errors.As(err, &se) && se.Status == 409:
		return text("sessionhub: the message was not sent: this session belongs to another machine ("+reason(se.Message)+").", true)
	case errors.As(err, &se) && se.Status == 429:
		return text("sessionhub: the message was not sent: too many messages in the last minute ("+reason(se.Message)+"). Wait a minute and try again.", true)
	case err != nil:
		return text("sessionhub: the message could not be sent: "+serverMessage(err), true)
	}
	lines := make([]string, 0, len(out.Results))
	for _, r := range out.Results {
		id := clean(r.SessionID, 8)
		if r.State == api.MessageQueued {
			lines = append(lines, id+": queued; it is typed into the session when its agent is idle (within 10 minutes)")
		} else {
			lines = append(lines, id+": refused: "+clean(r.Detail, 200))
		}
	}
	return text(strings.Join(lines, "\n"), false)
}
