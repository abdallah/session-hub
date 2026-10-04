package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/abdallah/session-hub/internal/api"
)

// CreateStart asks machine's watcher to start a new Claude session.
func (c *Client) CreateStart(ctx context.Context, machine string, in api.StartIn) (api.StartRequest, error) {
	var out api.StartRequest
	err := c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machine)+"/start", in, &out)
	return out, err
}

// GetStart reads one start request.
func (c *Client) GetStart(ctx context.Context, id string) (api.StartRequest, error) {
	var out api.StartRequest
	err := c.do(ctx, http.MethodGet, "/v1/starts/"+url.PathEscape(id), nil, &out)
	return out, err
}
