package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

// knowledgeHub answers the knowledge routes with canned replies and records bodies.
func knowledgeHub(t *testing.T, propose func(w http.ResponseWriter), search []api.Knowledge) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+string(b))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/knowledge":
			propose(w)
		case r.Method == "GET" && r.URL.Path == "/v1/knowledge":
			json.NewEncoder(w).Encode(search)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/confirm"):
			io.WriteString(w, `{}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"no route"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestProposeKnowledge(t *testing.T) {
	srv, seen := knowledgeHub(t, func(w http.ResponseWriter) {
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(api.KnowledgeAdded{
			Entry:   api.Knowledge{ID: 9},
			Similar: []api.Knowledge{{ID: 3, Kind: "fact", Text: "Deploys run on togh."}},
		})
	}, nil)
	s, q, state := newTestServer(t, srv.URL, 42)
	writeCurrent(t, state, 42, sid1)
	msg, isErr := callTool(t, s, "propose_knowledge", `{"kind":"fact","scope":"global","text":" Deploys run on togh. ","why":"asked","replaces":3}`)
	want := "sessionhub: knowledge proposal recorded. The user approves it on the dashboard.\nSimilar existing entries:\n#3 [fact] Deploys run on togh."
	if isErr || msg != want {
		t.Errorf("reply = %q %v", msg, isErr)
	}
	var in api.KnowledgeIn
	if err := json.Unmarshal([]byte(strings.TrimPrefix((*seen)[len(*seen)-1], "POST /v1/knowledge ")), &in); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(in.ClientID, "k_") || in.SourceSession != sid1 || in.Text != "Deploys run on togh." || in.Replaces != 3 || in.Why != "asked" {
		t.Errorf("body = %+v", in)
	}
	if n, _ := q.Len(); n != 0 {
		t.Errorf("queue has %d items", n)
	}
}

func TestProposeKnowledgeNoSimilar(t *testing.T) {
	srv, _ := knowledgeHub(t, func(w http.ResponseWriter) { io.WriteString(w, `{"entry":{"id":1},"similar":[]}`) }, nil)
	s, _, state := newTestServer(t, srv.URL, 42)
	writeCurrent(t, state, 42, sid1)
	msg, _ := callTool(t, s, "propose_knowledge", `{"kind":"fact","scope":"global","text":"x"}`)
	if msg != "sessionhub: knowledge proposal recorded. The user approves it on the dashboard." {
		t.Errorf("reply = %q", msg)
	}
}

func TestProposeKnowledgeOfflineQueues(t *testing.T) {
	s, q, state := newTestServer(t, "http://127.0.0.1:1", 42)
	writeCurrent(t, state, 42, sid1)
	msg, isErr := callTool(t, s, "propose_knowledge", `{"kind":"convention","scope":"repo:gitlab.com/a/b","text":"Use tabs."}`)
	if isErr || !strings.Contains(msg, "queued") {
		t.Errorf("reply = %q %v", msg, isErr)
	}
	items := queued(t, q)
	if len(items) != 1 || items[0].Op != client.OpKnowledgePropose {
		t.Fatalf("queue = %+v", items)
	}
	var in api.KnowledgeIn
	json.Unmarshal(items[0].Body, &in)
	if !strings.HasPrefix(in.ClientID, "k_") || in.SourceSession != sid1 {
		t.Errorf("queued body = %+v", in)
	}
}

func TestProposeKnowledgeRejected(t *testing.T) {
	srv, _ := knowledgeHub(t, func(w http.ResponseWriter) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"text looks like a secret"}`)
	}, nil)
	s, q, state := newTestServer(t, srv.URL, 42)
	writeCurrent(t, state, 42, sid1)
	msg, _ := callTool(t, s, "propose_knowledge", `{"kind":"fact","scope":"global","text":"x"}`)
	if !strings.Contains(msg, "the server rejected the knowledge proposal (400 ") {
		t.Errorf("reply = %q", msg)
	}
	if n, _ := q.Len(); n != 0 {
		t.Errorf("queued %d", n)
	}
}

func TestProposeKnowledgeValidation(t *testing.T) {
	srv, seen := knowledgeHub(t, func(w http.ResponseWriter) {}, nil)
	s, _, state := newTestServer(t, srv.URL, 42)
	writeCurrent(t, state, 42, sid1)
	for _, args := range []string{
		`{"kind":"","scope":"global","text":"x"}`,
		`{"kind":"opinion","scope":"global","text":"x"}`,
		`{"kind":"fact","scope":"","text":"x"}`,
		`{"kind":"fact","scope":"global","text":" "}`,
		`{"kind":"fact","scope":"global","text":"` + strings.Repeat("a", 301) + `"}`,
		`{"kind":"fact","scope":"global","text":"x","why":"` + strings.Repeat("a", 201) + `"}`,
		`{"kind":"fact","scope":"global","text":"x","replaces":-1}`,
	} {
		if msg, isErr := callTool(t, s, "propose_knowledge", args); !isErr {
			t.Errorf("%s = %q, want tool error", args, msg)
		}
	}
	if len(*seen) != 0 {
		t.Errorf("validation errors reached the server: %v", *seen)
	}
}

func TestSearchKnowledge(t *testing.T) {
	var many []api.Knowledge
	for i := 1; i <= 25; i++ {
		many = append(many, api.Knowledge{ID: int64(i), Kind: "fact", Scope: "global", Text: fmt.Sprintf("entry %d", i)})
	}
	srv, seen := knowledgeHub(t, nil, many)
	s, _, _ := newTestServer(t, srv.URL, 1)
	msg, isErr := callTool(t, s, "search_knowledge", `{"query":"togh","scope":"global"}`)
	lines := strings.Split(msg, "\n")
	if isErr || len(lines) != 20 || lines[0] != "#1 [fact] (global) entry 1" {
		t.Errorf("reply = %q %v", msg, isErr)
	}
	if got := (*seen)[0]; !strings.Contains(got, "q=togh") || !strings.Contains(got, "scope=global") {
		t.Errorf("request = %q", got)
	}
}

func TestSearchKnowledgeEmptyAndDown(t *testing.T) {
	srv, _ := knowledgeHub(t, nil, nil)
	s, _, _ := newTestServer(t, srv.URL, 1)
	if msg, isErr := callTool(t, s, "search_knowledge", `{"query":"x"}`); isErr || msg != "no matching knowledge" {
		t.Errorf("empty = %q %v", msg, isErr)
	}
	if _, isErr := callTool(t, s, "search_knowledge", `{}`); !isErr {
		t.Error("empty query should be a tool error")
	}
	s, _, _ = newTestServer(t, "http://127.0.0.1:1", 1)
	if msg, isErr := callTool(t, s, "search_knowledge", `{"query":"x"}`); !isErr || !strings.Contains(msg, "could not search knowledge") {
		t.Errorf("down = %q %v", msg, isErr)
	}
}

func TestConfirmKnowledge(t *testing.T) {
	srv, seen := knowledgeHub(t, nil, nil)
	s, _, _ := newTestServer(t, srv.URL, 1)
	msg, isErr := callTool(t, s, "confirm_knowledge", `{"id":7}`)
	if isErr || msg != "sessionhub: knowledge 7 confirmed." || !strings.HasPrefix((*seen)[0], "POST /v1/knowledge/7/confirm") {
		t.Errorf("reply = %q %v %v", msg, isErr, *seen)
	}
	if _, isErr := callTool(t, s, "confirm_knowledge", `{}`); !isErr {
		t.Error("missing id should be a tool error")
	}
	s, _, _ = newTestServer(t, "http://127.0.0.1:1", 1)
	if msg, isErr := callTool(t, s, "confirm_knowledge", `{"id":7}`); !isErr || !strings.Contains(msg, "could not confirm") {
		t.Errorf("down = %q %v", msg, isErr)
	}
}

func TestKnowledgeToolsListedAndInstructions(t *testing.T) {
	s, _, _ := newTestServer(t, "http://127.0.0.1:1", 1)
	got := transcript(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{}}`)
	names := map[string]bool{}
	for _, x := range decode(t, got[0])["result"].(map[string]any)["tools"].([]any) {
		names[x.(map[string]any)["name"].(string)] = true
	}
	for _, n := range []string{"propose_knowledge", "search_knowledge", "confirm_knowledge"} {
		if !names[n] {
			t.Errorf("tool %s not listed", n)
		}
	}
	ins, _ := decode(t, got[1])["result"].(map[string]any)["instructions"].(string)
	if !strings.Contains(ins, "Propose knowledge with propose_knowledge") {
		t.Errorf("instructions = %q", ins)
	}
}
