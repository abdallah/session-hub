package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

func TestNewKnowledgeID(t *testing.T) {
	if id := NewKnowledgeID(); !strings.HasPrefix(id, "k_") || id == NewKnowledgeID() {
		t.Errorf("id = %q", id)
	}
}

func TestReplayKnowledgeProposeTwiceMakesOneEntry(t *testing.T) {
	c, _ := realHub(t)
	ctx := context.Background()
	it := Item{Op: OpKnowledgePropose, Body: jsonBody(t, api.KnowledgeIn{ClientID: NewKnowledgeID(), Kind: api.KnowledgeFact, Scope: "global", Text: "Deploys run on togh."})}
	for i := 0; i < 2; i++ {
		if err := Replay(ctx, c, it); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}
	list, err := c.ListKnowledge(ctx, api.KnowledgeProposed, "")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v; want one proposed entry", list, err)
	}
	if got, err := c.SearchKnowledge(ctx, "togh", ""); err != nil || len(got) != 1 {
		t.Errorf("search = %+v, %v", got, err)
	}
	_ = c.ConfirmKnowledge(ctx, list[0].ID) // a proposed entry may be refused; the call must reach the route
	if _, err := c.KnowledgeLocal(ctx); err != nil {
		t.Errorf("local: %v", err)
	}
}

func TestReplayKnowledgeProposeNeedsClientID(t *testing.T) {
	c, _ := realHub(t)
	err := Replay(context.Background(), c, Item{Op: OpKnowledgePropose, Body: jsonBody(t, api.KnowledgeIn{Kind: api.KnowledgeFact, Scope: "global", Text: "x"})})
	if err == nil || IsRetryable(err) {
		t.Errorf("err = %v, want permanent", err)
	}
}

func TestRefreshKnowledge(t *testing.T) {
	ctx := context.Background()
	local := api.KnowledgeLocal{Version: "v1", SessionRefs: map[string][]string{"s1": {"A-1"}}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(local)
	}))
	t.Cleanup(ts.Close)
	c, _ := New(Config{ServerURL: ts.URL, Token: "hub_m_tok"})
	path := KnowledgePath(t.TempDir())
	if changed, err := RefreshKnowledge(ctx, c, path); err != nil || !changed {
		t.Fatalf("first: %v %v", changed, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	if changed, err := RefreshKnowledge(ctx, c, path); err != nil || changed {
		t.Errorf("unchanged: %v %v", changed, err)
	}
	local.SessionRefs = map[string][]string{"s1": {"A-1", "A-2"}}
	if changed, _ := RefreshKnowledge(ctx, c, path); !changed {
		t.Error("session_refs change did not rewrite")
	}
	local.Version = "v2"
	if changed, _ := RefreshKnowledge(ctx, c, path); !changed {
		t.Error("version change did not rewrite")
	}
	if got, err := LoadKnowledge(path); err != nil || got.Version != "v2" || len(got.SessionRefs["s1"]) != 2 {
		t.Errorf("copy = %+v, %v", got, err)
	}
}
