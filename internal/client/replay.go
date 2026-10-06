package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/abdallah/session-hub/internal/api"
)

// Queue ops added for MCP calls. OpUpsert, OpEvent, and OpHerdrSessions are
// in queue.go. For OpReport the Body is api.ReportIn; for OpTitle it is
// api.TitleIn. Both need Item.SessionID.
const (
	OpReport = "report"
	OpTitle  = "title"
	// OpDigest: Body is api.DigestIn; needs Item.SessionID.
	OpDigest = "digest"
)

// permanentError marks an item that can never be sent (bad body, unknown op).
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Replay sends one queued item to the endpoint for its op. Drainers call it
// for every item instead of switching on Op themselves, then use IsRetryable
// on the error to decide whether to keep the item.
func Replay(ctx context.Context, c *Client, it Item) error {
	decode := func(v any) error {
		if err := json.Unmarshal(it.Body, v); err != nil {
			return &permanentError{fmt.Errorf("queue item %s (%s): bad body: %w", it.ID, it.Op, err)}
		}
		return nil
	}
	needID := func() error {
		if it.SessionID == "" {
			return &permanentError{fmt.Errorf("queue item %s (%s): no session_id", it.ID, it.Op)}
		}
		return nil
	}
	switch it.Op {
	case OpUpsert:
		var v api.SessionUpsert
		if err := decode(&v); err != nil {
			return err
		}
		return c.UpsertSession(ctx, v)
	case OpHerdrSessions:
		var v api.HerdrSessionsPut
		if err := decode(&v); err != nil {
			return err
		}
		return c.PutHerdrSessions(ctx, v)
	case OpEvent:
		var v api.EventIn
		if err := needID(); err != nil {
			return err
		}
		if err := decode(&v); err != nil {
			return err
		}
		return c.PostEvent(ctx, it.SessionID, v)
	case OpReport:
		var v api.ReportIn
		if err := needID(); err != nil {
			return err
		}
		if err := decode(&v); err != nil {
			return err
		}
		return c.PostReport(ctx, it.SessionID, v)
	case OpTitle:
		var v api.TitleIn
		if err := needID(); err != nil {
			return err
		}
		if err := decode(&v); err != nil {
			return err
		}
		return c.SetTitle(ctx, it.SessionID, v.Title)
	case OpDigest:
		var v api.DigestIn
		if err := needID(); err != nil {
			return err
		}
		if err := decode(&v); err != nil {
			return err
		}
		return c.PutDigest(ctx, it.SessionID, v)
	case OpBlockedOnClear:
		if err := needID(); err != nil {
			return err
		}
		if len(it.Body) == 0 {
			return c.SetBlockedOn(ctx, it.SessionID, "")
		}
		var v api.BlockedOnIn
		if err := decode(&v); err != nil {
			return err
		}
		if v.Clears == "" {
			return c.SetBlockedOn(ctx, it.SessionID, "")
		}
		return c.ClearBlockedOn(ctx, it.SessionID, v.Clears)
	}
	return &permanentError{fmt.Errorf("queue item %s: unknown op %q", it.ID, it.Op)}
}

// IsNotFound reports whether err is a 404 response.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

// IsAuthError reports whether err is a 401 or 403 from the sessionhub server:
// the token is wrong or revoked, so drainers stop and keep their items.
func IsAuthError(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden)
}

// IsRetryable reports whether a later attempt could succeed: network errors,
// timeouts, 408, 429, and 5xx. Other 4xx responses and unsendable items (bad
// body, unknown op) are not retryable, so drainers drop them with a log line.
// A nil error is not retryable.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var pe *permanentError
	if errors.As(err, &pe) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status == http.StatusRequestTimeout || se.Status == http.StatusTooManyRequests || se.Status >= 500
	}
	return true
}
