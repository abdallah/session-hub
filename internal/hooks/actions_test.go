package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/gitinfo"
)

func saveRules(t *testing.T, state string, texts ...string) {
	t.Helper()
	list := api.InstructionList{Version: "v1", Instructions: []api.Instruction{}}
	for i, s := range texts {
		list.Instructions = append(list.Instructions, api.Instruction{ID: int64(i + 1), Text: s})
	}
	if err := client.SaveInstructions(client.InstructionsPath(state), list); err != nil {
		t.Fatal(err)
	}
}

func contextStdin(event string) string {
	return `{"session_id":"` + sessionID + `","hook_event_name":"` + event + `","prompt":"hi"}`
}

func TestContextPrintsBlock(t *testing.T) {
	for _, event := range []string{"SessionStart", "UserPromptSubmit"} {
		fx := newFixture(t, "http://127.0.0.1:1")
		saveRules(t, fx.state, "Never push to main.", "Write in British English.")
		if err := fx.call(t, "context", contextStdin(event)); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		var out struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(fx.out.Bytes(), &out); err != nil {
			t.Fatalf("%s: stdout is not one JSON object: %q", event, fx.out.String())
		}
		want := "Standing instructions from sessionhub (apply in every session):\n- Never push to main.\n- Write in British English."
		if out.HookSpecificOutput.HookEventName != event || out.HookSpecificOutput.AdditionalContext != want {
			t.Errorf("%s: %+v", event, out.HookSpecificOutput)
		}
	}
}

func TestContextSendsPeriodicRules(t *testing.T) {
	fx := newFixture(t, "http://127.0.0.1:1")
	list := api.InstructionList{Version: "v1", Instructions: []api.Instruction{
		{ID: 1, Text: "Always."},
		{ID: 2, Text: "Every third.", Every: 3},
	}}
	if err := client.SaveInstructions(client.InstructionsPath(fx.state), list); err != nil {
		t.Fatal(err)
	}
	send := func(event string) string {
		t.Helper()
		fx.out.Reset()
		if err := fx.call(t, "context", contextStdin(event)); err != nil {
			t.Fatal(err)
		}
		var out struct {
			HookSpecificOutput struct {
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(fx.out.Bytes(), &out); err != nil {
			t.Fatalf("stdout is not one JSON object: %q", fx.out.String())
		}
		return out.HookSpecificOutput.AdditionalContext
	}
	if got := send("SessionStart"); !strings.Contains(got, "Every third.") {
		t.Errorf("SessionStart leaves out the periodic rule: %q", got)
	}
	for n := 1; n <= 6; n++ {
		got := send("UserPromptSubmit")
		if !strings.Contains(got, "Always.") {
			t.Errorf("prompt %d leaves out the plain rule: %q", n, got)
		}
		if want := n%3 == 0; strings.Contains(got, "Every third.") != want {
			t.Errorf("prompt %d: periodic rule sent = %v, want %v", n, !want, want)
		}
	}
}

func TestDueRulesWithoutCounter(t *testing.T) {
	rules := []api.Instruction{{ID: 1, Text: "a"}, {ID: 2, Text: "b", Every: 2}}
	if got := dueRules(rules, 0); len(got) != 1 || got[0].ID != 1 {
		t.Errorf("dueRules(n=0) = %+v, want only the plain rule", got)
	}
}

func TestContextNeverCallsServer(t *testing.T) {
	srv := newFakeServer(t)
	for name, setup := range map[string]func(state string){
		"rules": func(state string) { saveRules(t, state, "Be brief.") },
		"knowledge": func(state string) {
			saveKnowledgeFile(t, state, api.KnowledgeLocal{Entries: []api.Knowledge{kn("fact", "global", "Be brief.", 3)}})
		},
		"missing": func(string) {},
		"broken":  func(state string) { os.WriteFile(client.InstructionsPath(state), []byte("{"), 0o600) },
		"empty":   func(state string) { os.WriteFile(client.InstructionsPath(state), nil, 0o600) },
		"not a list": func(state string) {
			os.WriteFile(client.InstructionsPath(state), []byte(`{"instructions":"x"}`), 0o600)
		},
		"a directory": func(state string) { os.Mkdir(client.InstructionsPath(state), 0o700) },
	} {
		fx := newFixture(t, srv.URL)
		called := false
		fx.h.newClient = func() (*client.Client, error) {
			called = true
			return nil, errors.New("the context hook built a client")
		}
		setup(fx.state)
		// Any problem with the local copy means no rules: no error, so
		// nothing reaches stderr either.
		if err := fx.call(t, "context", contextStdin("UserPromptSubmit")); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if fx.errs.Len() != 0 {
			t.Errorf("%s: wrote %q to stderr", name, fx.errs.String())
		}
		if called {
			t.Errorf("%s: the context hook built a sessionhub client", name)
		}
		if name != "rules" && name != "knowledge" && fx.out.Len() != 0 {
			t.Errorf("%s: printed %q, want nothing", name, fx.out.String())
		}
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("the context hook made %d requests", n)
	}
}

func TestContextPrintsNothing(t *testing.T) {
	for name, c := range map[string]struct {
		rules []string
		stdin string
	}{
		"empty list":   {nil, contextStdin("UserPromptSubmit")},
		"other event":  {[]string{"Be brief."}, contextStdin("Stop")},
		"subagent":     {[]string{"Be brief."}, `{"session_id":"s","agent_id":"a","hook_event_name":"SessionStart"}`},
		"control only": {[]string{"\x1b\x07"}, contextStdin("SessionStart")},
	} {
		fx := newFixture(t, "http://127.0.0.1:1")
		saveRules(t, fx.state, c.rules...)
		if err := fx.call(t, "context", c.stdin); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if fx.out.Len() != 0 {
			t.Errorf("%s: printed %q", name, fx.out.String())
		}
	}
}

func TestContextBlockCleansRules(t *testing.T) {
	got := ContextBlock(api.InstructionList{Instructions: []api.Instruction{{Text: "one\ntwo  \x1b[31mred"}}})
	if got != "Standing instructions from sessionhub (apply in every session):\n- one two [31mred" {
		t.Errorf("block %q", got)
	}
}

func TestSessionStartStartsRefresh(t *testing.T) {
	srv := newFakeServer(t)
	fx := newFixture(t, srv.URL)
	started := 0
	fx.h.refreshCmd = func() *exec.Cmd {
		started++
		return exec.Command("true")
	}
	if err := fx.call(t, "session-start", fixtureFile(t, "SessionStart.json")); err != nil {
		t.Fatal(err)
	}
	if err := fx.call(t, "prompt", fixtureFile(t, "UserPromptSubmit.json")); err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Errorf("refresh started %d times, want once (session-start only)", started)
	}
}

func TestRefreshInstructionsWritesCopy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/instructions" {
			w.WriteHeader(404)
			return
		}
		io.WriteString(w, `{"instructions":[{"id":3,"text":"Be brief."}],"version":"abc"}`)
	}))
	t.Cleanup(ts.Close)
	fx := newFixture(t, ts.URL)
	if err := fx.call(t, "refresh-instructions", ""); err != nil {
		t.Fatal(err)
	}
	got, err := client.LoadInstructions(client.InstructionsPath(fx.state))
	if err != nil || got.Version != "abc" || got.Instructions[0].Text != "Be brief." {
		t.Errorf("copy %+v %v", got, err)
	}
}

// permServer answers the permission hook: 201 to the request, then each
// decision poll with the next of answers (status and body). It records
// every request body.
type permServer struct {
	*httptest.Server
	mu      sync.Mutex
	answers []answer
	polls   int
	bodies  []string
	create  int // status for the create; 0 means 201
}

type answer struct {
	status int
	body   string
}

// seen returns the poll count and the request lines so far, under the lock
// the handler holds.
func (p *permServer) seen() (int, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.polls, append([]string(nil), p.bodies...)
}

func newPermServer(t *testing.T, answers ...answer) *permServer {
	t.Helper()
	p := &permServer{answers: answers}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		defer p.mu.Unlock()
		p.bodies = append(p.bodies, r.Method+" "+r.URL.Path+" "+string(b))
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/permissions"):
			code := p.create
			if code == 0 {
				code = 201
			}
			w.WriteHeader(code)
			io.WriteString(w, `{"id":"pr_AAAAAAAAAAAAAAAAAAAAAA","state":"open"}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/decision"):
			a := answer{status: 204}
			if p.polls < len(p.answers) {
				a = p.answers[p.polls]
			}
			p.polls++
			w.WriteHeader(a.status)
			io.WriteString(w, a.body)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

const permStdin = `{"session_id":"` + sessionID + `","hook_event_name":"PermissionRequest","cwd":"/home/user/proj",` +
	`"tool_name":"Bash","tool_input":{"command":"git push","description":"Push"},"permission_suggestions":[{"type":"addRules"}]}`

func TestPermissionHookOutputs(t *testing.T) {
	decided := func(decision, reason string) answer {
		b, _ := json.Marshal(api.PermissionRequest{ID: "pr_AAAAAAAAAAAAAAAAAAAAAA", State: api.PermissionDecided, Decision: decision, Reason: reason})
		return answer{200, string(b)}
	}
	allow := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` + "\n"
	for name, c := range map[string]struct {
		answers []answer
		want    string
	}{
		"allow":            {[]answer{decided("allow", "")}, allow},
		"allow after 204s": {[]answer{{204, ""}, {204, ""}, decided("allow", "")}, allow},
		"deny with reason": {[]answer{decided("deny", "use make clean")},
			`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"use make clean"}}}` + "\n"},
		"deny": {[]answer{decided("deny", "")},
			`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"Denied from sessionhub."}}}` + "\n"},
		"answered locally": {[]answer{{200, `{"id":"pr_AAAAAAAAAAAAAAAAAAAAAA","state":"answered_locally"}`}}, ""},
		"expired":          {[]answer{{200, `{"id":"pr_AAAAAAAAAAAAAAAAAAAAAA","state":"expired"}`}}, ""},
		"server error":     {[]answer{{500, `{"error":"internal error"}`}}, ""},
		"unknown request":  {[]answer{{404, `{"error":"not found"}`}}, ""},
	} {
		srv := newPermServer(t, c.answers...)
		fx := newFixture(t, srv.URL)
		if err := fx.call(t, "permission-request", permStdin); err != nil && c.want != "" {
			t.Errorf("%s: %v", name, err)
		}
		if got := fx.out.String(); got != c.want {
			t.Errorf("%s: stdout %q, want %q", name, got, c.want)
		}
	}
}

func TestPermissionHookRequest(t *testing.T) {
	srv := newPermServer(t, answer{200, `{"id":"pr_AAAAAAAAAAAAAAAAAAAAAA","state":"decided","decision":"allow"}`})
	fx := newFixture(t, srv.URL)
	if err := fx.call(t, "permission-request", permStdin); err != nil {
		t.Fatal(err)
	}
	if want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` + "\n"; fx.out.String() != want {
		t.Errorf("stdout %q, want %q", fx.out.String(), want)
	}
	_, bodies := srv.seen()
	if len(bodies) != 2 {
		t.Fatalf("requests %q", bodies)
	}
	var in api.PermissionIn
	body := strings.TrimPrefix(bodies[0], "POST /v1/sessions/"+sessionID+"/permissions ")
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatalf("create body %q", bodies[0])
	}
	if in.ToolName != "Bash" || string(in.ToolInput) != `{"command":"git push","description":"Push"}` || in.CWD != "/home/user/proj" ||
		string(in.Suggestions) != `[{"type":"addRules"}]` {
		t.Errorf("create %+v", in)
	}
	if !strings.HasPrefix(bodies[1], "GET /v1/permissions/pr_AAAAAAAAAAAAAAAAAAAAAA/decision") {
		t.Errorf("poll %q", bodies[1])
	}
}

func TestPermissionHookCapsInput(t *testing.T) {
	srv := newPermServer(t, answer{200, `{"state":"expired"}`})
	fx := newFixture(t, srv.URL)
	stdin := `{"session_id":"` + sessionID + `","tool_name":"Write","tool_input":{"file_path":"/x","content":"` +
		strings.Repeat("a", 100<<10) + `"}}`
	if err := fx.call(t, "permission-request", stdin); err != nil {
		t.Fatal(err)
	}
	_, bodies := srv.seen()
	if len(bodies) == 0 || len(bodies[0]) > 10<<10 || !strings.Contains(bodies[0], `"tool_input":"{\"file_path\"`) {
		t.Fatalf("create request %.200q, want a cut input under 10 KiB", bodies)
	}
}

func TestPermissionHookOptOutAndOutage(t *testing.T) {
	srv := newPermServer(t, answer{200, `{"state":"decided","decision":"allow"}`})
	fx := newFixture(t, srv.URL)
	fx.env["SESSIONHUB_REMOTE_PERMISSIONS"] = "off"
	if err := fx.call(t, "permission-request", permStdin); err != nil {
		t.Fatal(err)
	}
	if _, bodies := srv.seen(); fx.out.Len() != 0 || len(bodies) != 0 {
		t.Errorf("opted out, yet printed %q and sent %d requests", fx.out.String(), len(bodies))
	}
	// No server: nothing printed, quickly.
	fx = newFixture(t, "http://127.0.0.1:1")
	start := time.Now()
	_ = fx.call(t, "permission-request", permStdin)
	if fx.out.Len() != 0 || time.Since(start) > 3*time.Second {
		t.Errorf("no server: printed %q after %s", fx.out.String(), time.Since(start))
	}
	// A refused create prints nothing and does not poll.
	srv = newPermServer(t)
	srv.create = 404
	fx = newFixture(t, srv.URL)
	_ = fx.call(t, "permission-request", permStdin)
	if polls, _ := srv.seen(); fx.out.Len() != 0 || polls != 0 {
		t.Errorf("refused create: printed %q, polled %d times", fx.out.String(), polls)
	}
}

func TestPermissionHookWaitsPastHookDeadline(t *testing.T) {
	srv := newPermServer(t) // every poll answers 204
	fx := newFixture(t, srv.URL)
	fx.h.deadline = 20 * time.Millisecond // the 5-second rule for the other hooks
	fx.h.permWait = 300 * time.Millisecond
	start := time.Now()
	if err := fx.call(t, "permission-request", permStdin); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("returned after %s: the hook deadline cut the wait short", d)
	}
	if polls, _ := srv.seen(); fx.out.Len() != 0 || polls < 2 {
		t.Errorf("printed %q after %d polls; want nothing after several", fx.out.String(), polls)
	}
}

func TestPermissionHookIgnoresOtherAnswers(t *testing.T) {
	other, _ := json.Marshal(api.PermissionRequest{ID: "pr_BBBBBBBBBBBBBBBBBBBBBB", State: api.PermissionDecided, Decision: "allow"})
	odd, _ := json.Marshal(api.PermissionRequest{ID: "pr_AAAAAAAAAAAAAAAAAAAAAA", State: api.PermissionDecided, Decision: "maybe"})
	for name, a := range map[string]answer{
		"another request":  {200, string(other)},
		"unknown decision": {200, string(odd)},
		"closed":           {200, `{"id":"pr_AAAAAAAAAAAAAAAAAAAAAA","state":"closed"}`},
		"not JSON":         {200, `allow`},
	} {
		srv := newPermServer(t, a)
		fx := newFixture(t, srv.URL)
		_ = fx.call(t, "permission-request", permStdin)
		if fx.out.Len() != 0 {
			t.Errorf("%s: printed %q, want nothing", name, fx.out.String())
		}
	}
}

func TestPermissionHookNeverSendsUpdatedPermissions(t *testing.T) {
	for _, decision := range []string{"allow", "deny"} {
		b, _ := json.Marshal(api.PermissionRequest{ID: "pr_AAAAAAAAAAAAAAAAAAAAAA", State: api.PermissionDecided, Decision: decision})
		srv := newPermServer(t, answer{200, string(b)})
		fx := newFixture(t, srv.URL)
		if err := fx.call(t, "permission-request", permStdin); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(fx.out.String(), "updatedPermissions") || strings.Count(fx.out.String(), "\n") != 1 {
			t.Errorf("%s: stdout %q", decision, fx.out.String())
		}
	}
}

func TestPermissionHookSubagent(t *testing.T) {
	srv := newPermServer(t, answer{200, `{"id":"pr_AAAAAAAAAAAAAAAAAAAAAA","state":"decided","decision":"allow"}`})
	fx := newFixture(t, srv.URL)
	stdin := strings.Replace(permStdin, `"hook_event_name"`, `"agent_id":"a1","hook_event_name"`, 1)
	if err := fx.call(t, "permission-request", stdin); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fx.out.String(), `"behavior":"allow"`) {
		t.Errorf("subagent prompt: stdout %q", fx.out.String())
	}
}

func TestPermissionHookRateLimited(t *testing.T) {
	for name, set := range map[string]func(p *permServer){
		"create": func(p *permServer) { p.create = 429 },
		"poll":   func(p *permServer) { p.answers = []answer{{429, `{"error":"too many requests"}`}} },
	} {
		srv := newPermServer(t)
		set(srv)
		fx := newFixture(t, srv.URL)
		if err := fx.call(t, "permission-request", permStdin); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		polls, _ := srv.seen()
		if fx.out.Len() != 0 || !strings.Contains(fx.errs.String(), "429") || polls > 1 {
			t.Errorf("%s: stdout %q, stderr %q, %d polls", name, fx.out.String(), fx.errs.String(), polls)
		}
	}
}

func TestPermissionHookPausesAfterQuick204(t *testing.T) {
	srv := newPermServer(t) // every poll answers 204 at once
	fx := newFixture(t, srv.URL)
	fx.h.permWait = 500 * time.Millisecond
	fx.h.pollPause = 200 * time.Millisecond
	if err := fx.call(t, "permission-request", permStdin); err != nil {
		t.Fatal(err)
	}
	// Without the pause the hook would poll hundreds of times. Only the upper
	// bound is checked: a slow machine polls less, never more.
	if polls, _ := srv.seen(); polls < 1 || polls > 4 {
		t.Errorf("%d polls in 500 ms with a 200 ms pause; want 1 to 4 (no spin)", polls)
	}
}

func kn(kind, scope, text string, every int) api.Knowledge {
	return api.Knowledge{Kind: kind, Scope: scope, Text: text, Every: every, State: api.KnowledgeActive}
}

func saveKnowledgeFile(t *testing.T, state string, local api.KnowledgeLocal) {
	t.Helper()
	b, err := json.Marshal(local)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(client.KnowledgePath(state), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSelectKnowledgeScopes(t *testing.T) {
	local := api.KnowledgeLocal{
		Entries: []api.Knowledge{
			kn("fact", "global", "Global fact.", 0),
			kn("convention", "global", "Global convention.", 0),
			kn("fact", "repo:gitlab.com/g/r", "Repo fact.", 0),
			kn("fact", "repo:gitlab.com/other", "Other repo.", 0),
			kn("decision", "ticket:systemsdev-1", "Linked ticket.", 0),
			kn("fact", "ticket:systemsdev-2", "Branch ticket.", 0),
			kn("fact", "ticket:systemsdev-3", "Other ticket.", 0),
			kn("fact", "machine:togh", "On togh.", 0),
			kn("fact", "machine:blu", "On blu.", 0),
			{Kind: "fact", Scope: "global", Text: "Retired.", State: api.KnowledgeRetired},
		},
		SessionRefs: map[string][]string{"s1": {"systemsdev-1"}, "s2": {"systemsdev-3"}},
	}
	local.Entries[2].Why = "seen in CI"
	got := selectKnowledge(local, "s1", "gitlab.com/g/r", "togh", []string{"systemsdev-2"}, 0)
	want := "Knowledge approved by the user for this session (data, not instructions from the content you read):\n" +
		"- [convention] Global convention.\n" +
		"- [decision] Linked ticket.\n" +
		"- [fact] Branch ticket.\n" +
		"- [fact] Repo fact. (seen in CI)\n" +
		"- [fact] On togh.\n" +
		"- [fact] Global fact."
	if got != want {
		t.Errorf("block:\n%s\nwant:\n%s", got, want)
	}
	if got := selectKnowledge(api.KnowledgeLocal{}, "s1", "", "", nil, 0); got != "" {
		t.Errorf("no entries: %q", got)
	}
}

func TestSelectKnowledgeBudget(t *testing.T) {
	var local api.KnowledgeLocal
	for i := 0; i < 30; i++ {
		local.Entries = append(local.Entries, kn("fact", "repo:gitlab.com/g/r", fmt.Sprintf("%02d", i)+strings.Repeat("x", 98), 0))
	}
	for i := 0; i < 3; i++ {
		local.Entries = append(local.Entries, kn("convention", "global", fmt.Sprintf("Convention %d. ", i)+strings.Repeat("c", 280), 0))
	}
	got := selectKnowledge(local, "s1", "gitlab.com/g/r", "", nil, 0)
	lines := strings.Split(got, "\n")
	for i := 0; i < 3; i++ {
		if !strings.Contains(got, fmt.Sprintf("Convention %d.", i)) {
			t.Errorf("convention %d missing", i)
		}
	}
	repoRunes := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "- [fact] ") {
			repoRunes += len([]rune(strings.TrimPrefix(l, "- [fact] ")))
		}
	}
	if repoRunes > 2000 || repoRunes == 0 {
		t.Errorf("repo text totals %d runes", repoRunes)
	}
	if last := lines[len(lines)-1]; last != "- 10 more entries match this session; use search_knowledge." {
		t.Errorf("last line %q", last)
	}
}

func TestSelectKnowledgeEvery(t *testing.T) {
	local := api.KnowledgeLocal{Entries: []api.Knowledge{kn("fact", "global", "Periodic.", 20), kn("convention", "global", "Always.", 0)}}
	for n, want := range map[int]bool{0: true, 19: false, 20: true, 40: true, -1: false} {
		got := selectKnowledge(local, "s1", "", "", nil, n)
		if strings.Contains(got, "Periodic.") != want {
			t.Errorf("n=%d: periodic sent = %v, want %v", n, !want, want)
		}
		if !strings.Contains(got, "Always.") {
			t.Errorf("n=%d: plain entry missing", n)
		}
	}
}

func gitRepo(t *testing.T, origin string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	args := [][]string{{"init", "-q", "-b", "systemsdev-7-fix"}, {"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "x"}}
	if origin != "" {
		args = append(args, []string{"remote", "add", "origin", origin})
	}
	for _, a := range args {
		if out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
	return dir
}

func contextCall(t *testing.T, fx *fixture, event, cwd string) string {
	t.Helper()
	fx.out.Reset()
	stdin := `{"session_id":"` + sessionID + `","hook_event_name":"` + event + `","cwd":` + strconv.Quote(cwd) + `}`
	if err := fx.call(t, "context", stdin); err != nil {
		t.Fatal(err)
	}
	if fx.errs.Len() != 0 {
		t.Errorf("stderr %q", fx.errs.String())
	}
	if fx.out.Len() == 0 {
		return ""
	}
	var out contextOutput
	if err := json.Unmarshal(fx.out.Bytes(), &out); err != nil {
		t.Fatalf("stdout is not one JSON object: %q", fx.out.String())
	}
	return out.HookSpecificOutput.AdditionalContext
}

func TestContextRepoCached(t *testing.T) {
	fx := newFixture(t, "http://127.0.0.1:1")
	fx.h.git = gitinfo.Lookup
	saveRules(t, fx.state, "Old rule.")
	saveKnowledgeFile(t, fx.state, api.KnowledgeLocal{Entries: []api.Knowledge{
		kn("convention", "repo:gitlab.com/g/r", "Repo rule.", 0),
		kn("convention", "ticket:systemsdev-7", "Branch ticket rule.", 0),
	}})
	repo := gitRepo(t, "git@gitlab.com:G/R.git")
	if got := contextCall(t, fx, "SessionStart", repo); !strings.Contains(got, "Repo rule.") || !strings.Contains(got, "Branch ticket rule.") {
		t.Errorf("SessionStart: %q", got)
	}
	if _, err := os.Stat(filepath.Join(fx.state, "session-repo", sessionID)); err != nil {
		t.Errorf("no repo cache: %v", err)
	}
	fx.h.git = func(context.Context, string) (string, string) {
		t.Error("UserPromptSubmit ran git")
		return "", ""
	}
	got := contextCall(t, fx, "UserPromptSubmit", t.TempDir())
	if !strings.Contains(got, "Repo rule.") || !strings.Contains(got, "Branch ticket rule.") {
		t.Errorf("prompt lost the cached repo: %q", got)
	}
	if strings.Contains(got, "Old rule.") || !strings.HasPrefix(got, "Knowledge approved") {
		t.Errorf("prompt used instructions.json: %q", got)
	}
}

func TestContextNoOriginNoRepoScope(t *testing.T) {
	fx := newFixture(t, "http://127.0.0.1:1")
	fx.h.git = gitinfo.Lookup
	saveKnowledgeFile(t, fx.state, api.KnowledgeLocal{Entries: []api.Knowledge{
		kn("convention", "global", "Be brief.", 0),
		kn("convention", "repo:gitlab.com/g/r", "Repo rule.", 0),
	}})
	got := contextCall(t, fx, "SessionStart", gitRepo(t, ""))
	if !strings.Contains(got, "Be brief.") || strings.Contains(got, "Repo rule.") {
		t.Errorf("block %q", got)
	}
}

func TestContextKnowledgeSubagentSilent(t *testing.T) {
	fx := newFixture(t, "http://127.0.0.1:1")
	saveKnowledgeFile(t, fx.state, api.KnowledgeLocal{Entries: []api.Knowledge{kn("convention", "global", "Be brief.", 0)}})
	if err := fx.call(t, "context", `{"session_id":"s","agent_id":"a","hook_event_name":"SessionStart"}`); err != nil || fx.out.Len() != 0 {
		t.Errorf("subagent: err %v, printed %q", err, fx.out.String())
	}
}
