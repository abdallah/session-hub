package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"

	"github.com/abdallah/session-hub/internal/api"
)

// KnowledgeFile is the local copy of the knowledge layer, in the state dir.
// The watcher writes it; `sessionhub hook context` reads it offline.
const KnowledgeFile = "knowledge.json"

// KnowledgePath is the local copy's path in stateDir.
func KnowledgePath(stateDir string) string { return filepath.Join(stateDir, KnowledgeFile) }

// NewKnowledgeID returns a client id for api.KnowledgeIn: "k_" and 8 random
// bytes, base64url. A retried proposal with the same id is a no-op.
func NewKnowledgeID() string { return randomID("k_") }

// ProposeKnowledge proposes an entry; a repeat with the same client_id
// returns the existing one.
func (c *Client) ProposeKnowledge(ctx context.Context, in api.KnowledgeIn) (api.KnowledgeAdded, error) {
	var out api.KnowledgeAdded
	err := c.do(ctx, http.MethodPost, "/v1/knowledge", in, &out)
	return out, err
}

// ListKnowledge lists entries by state and scope; empty values match all.
func (c *Client) ListKnowledge(ctx context.Context, state, scope string) ([]api.Knowledge, error) {
	return c.knowledgeQuery(ctx, url.Values{"state": {state}, "scope": {scope}})
}

// SearchKnowledge searches entries by text, optionally within a scope.
func (c *Client) SearchKnowledge(ctx context.Context, query, scope string) ([]api.Knowledge, error) {
	return c.knowledgeQuery(ctx, url.Values{"q": {query}, "scope": {scope}})
}

func (c *Client) knowledgeQuery(ctx context.Context, v url.Values) ([]api.Knowledge, error) {
	for k := range v {
		if v.Get(k) == "" {
			v.Del(k)
		}
	}
	var out []api.Knowledge
	if err := c.do(ctx, http.MethodGet, "/v1/knowledge?"+v.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ConfirmKnowledge marks an entry as still true.
func (c *Client) ConfirmKnowledge(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPost, "/v1/knowledge/"+strconv.FormatInt(id, 10)+"/confirm", nil, nil)
}

// KnowledgeLocal reads this machine's copy: active entries and session refs.
func (c *Client) KnowledgeLocal(ctx context.Context) (api.KnowledgeLocal, error) {
	var out api.KnowledgeLocal
	err := c.do(ctx, http.MethodGet, "/v1/knowledge/local", nil, &out)
	return out, err
}

// LoadKnowledge reads the local copy. A missing file returns an error that
// matches os.ErrNotExist.
func LoadKnowledge(path string) (api.KnowledgeLocal, error) {
	var k api.KnowledgeLocal
	b, err := os.ReadFile(path)
	if err != nil {
		return k, err
	}
	err = json.Unmarshal(b, &k)
	return k, err
}

// saveKnowledge writes the copy with mode 0600 through a temp file and a
// rename, so a reader never sees half a file.
func saveKnowledge(path string, k api.KnowledgeLocal) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(k)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".knowledge-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// RefreshKnowledge rewrites the local copy when its version or session refs
// differ from the server's, or the copy cannot be read.
func RefreshKnowledge(ctx context.Context, c *Client, path string) (bool, error) {
	k, err := c.KnowledgeLocal(ctx)
	if err != nil {
		return false, err
	}
	if cur, err := LoadKnowledge(path); err == nil && cur.Version == k.Version &&
		reflect.DeepEqual(cur.SessionRefs, k.SessionRefs) {
		return false, nil
	}
	return true, saveKnowledge(path, k)
}
