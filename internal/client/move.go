package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// BundleTimeout bounds one bundle upload or download. It replaces the
// per-request limit, which a 64 MiB transfer through the tunnel would pass.
const BundleTimeout = 10 * time.Minute

// maxBundleDownload caps a downloaded bundle. Tests lower it.
var maxBundleDownload int64 = api.MaxSealedBundle

// CreateMove asks the sessionhub to move session sessionID to target: a machine
// name or api.MoveCloud.
func (c *Client) CreateMove(ctx context.Context, sessionID, target string) (api.Move, error) {
	var out api.Move
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/move", api.MoveIn{Target: target}, &out)
	return out, err
}

// GetMove reads one move.
func (c *Client) GetMove(ctx context.Context, id string) (api.Move, error) {
	var out api.Move
	err := c.do(ctx, http.MethodGet, "/v1/moves/"+url.PathEscape(id), nil, &out)
	return out, err
}

// PutMoveKey registers this machine's move public key.
func (c *Client) PutMoveKey(ctx context.Context, publicKey string) error {
	return c.do(ctx, http.MethodPut, "/v1/machines/self/move-key", api.MoveKeyIn{PublicKey: publicKey}, nil)
}

// PostMoveResult reports this machine's step of a move: done or failed.
func (c *Client) PostMoveResult(ctx context.Context, id string, in api.MoveResultIn) error {
	return c.do(ctx, http.MethodPost, "/v1/moves/"+url.PathEscape(id)+"/result", in, nil)
}

// PutMoveBundle streams a sealed bundle of size bytes from r.
func (c *Client) PutMoveBundle(ctx context.Context, id string, r io.Reader, size int64) error {
	ctx, cancel := context.WithTimeout(ctx, BundleTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/v1/moves/"+url.PathEscape(id)+"/bundle", r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return responseError(resp)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return nil
}

// GetMoveBundle streams a sealed bundle into w and returns its size. A
// bundle over api.MaxSealedBundle is an error.
func (c *Client) GetMoveBundle(ctx context.Context, id string, w io.Writer) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, BundleTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/moves/"+url.PathEscape(id)+"/bundle", nil)
	if err != nil {
		return 0, err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, responseError(resp)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxBundleDownload+1))
	if err != nil {
		return n, err
	}
	if n > maxBundleDownload {
		return n, fmt.Errorf("sessionhub: the bundle is larger than %d bytes", maxBundleDownload)
	}
	return n, nil
}

func (c *Client) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// responseError reads a non-2xx response as a *StatusError, like do.
func responseError(resp *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return errors.Join(&StatusError{Status: resp.StatusCode}, err)
	}
	msg := strings.TrimSpace(string(data))
	var e api.Error
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	return &StatusError{Status: resp.StatusCode, Message: msg}
}
