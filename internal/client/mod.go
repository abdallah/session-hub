package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// The Claude Code mod's calls. `sessionhub mod` makes them for the mod, with this
// machine's token. See docs/client.md, "The Claude Code mod".

// OpBlockedOnClear queues the clear of a session's blocked-on question. It
// needs Item.SessionID; its Body is api.BlockedOnIn with Clears, the
// question it clears, so a replay never clears a newer question. An item
// with no Body (from an older sessionhub) clears only a mod-set block. There is no
// queue op for setting a question: a set replayed later would block a
// session that was already answered, so setting one is best effort.
const OpBlockedOnClear = "blocked_on_clear"

// SetBlockedOn reports what session id waits on, or clears it when text is
// "". The server cleans the text to one line of at most
// api.MaxBlockedOnRunes characters.
func (c *Client) SetBlockedOn(ctx context.Context, id, text string) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/blocked-on", api.BlockedOnIn{Text: text}, nil)
}

// ClearBlockedOn clears what session id waits on, but only while it is
// question, the line an earlier SetBlockedOn sent; otherwise the server
// changes nothing.
func (c *Client) ClearBlockedOn(ctx context.Context, id, question string) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/blocked-on", api.BlockedOnIn{Clears: question}, nil)
}

// PutUsage stores session id's context window fill and cost. A nil field
// keeps the stored value.
func (c *Client) PutUsage(ctx context.Context, id string, in api.UsageIn) error {
	return c.do(ctx, http.MethodPut, "/v1/sessions/"+url.PathEscape(id)+"/usage", in, nil)
}

// PollSessionMessage holds GET /v1/sessions/{id}/messages/next open for up
// to wait (whole seconds, 1 to 30) and returns the message session id's mod
// claimed, or nil when none arrived (204). Each call keeps the session
// messageable for 2 minutes.
func (c *Client) PollSessionMessage(ctx context.Context, id string, wait time.Duration) (*api.ModMessage, error) {
	secs := min(max(int(wait/time.Second), 1), 30)
	var raw json.RawMessage
	if err := c.doTimeout(ctx, time.Duration(secs)*time.Second+PollSlack, http.MethodGet,
		"/v1/sessions/"+url.PathEscape(id)+"/messages/next?wait="+strconv.Itoa(secs), nil, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil // 204
	}
	var m api.ModMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.ID == "" {
		return nil, errors.New("sessionhub: message poll answered without a message")
	}
	return &m, nil
}

// PostMessageResult reports what happened to message id: api.MessageDelivered,
// api.MessageBusy (offered again later), or api.MessageRefused.
func (c *Client) PostMessageResult(ctx context.Context, id string, in api.MessageResultIn) (api.Message, error) {
	var out api.Message
	err := c.do(ctx, http.MethodPost, "/v1/messages/"+url.PathEscape(id)+"/result", in, &out)
	return out, err
}
