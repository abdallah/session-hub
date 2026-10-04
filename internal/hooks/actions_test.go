package hooks

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
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

func TestContextNeverCallsServer(t *testing.T) {
	srv := newFakeServer(t)
	for name, setup := range map[string]func(state string){
		"rules":   func(state string) { saveRules(t, state, "Be brief.") },
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
		if name != "rules" && fx.out.Len() != 0 {
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
