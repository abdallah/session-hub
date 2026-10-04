package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

func (e *env) act(method, path, action string, body any) (int, []byte) {
	e.t.Helper()
	code, b, _ := e.doHdr(method, path, "", e.web, map[string]string{api.HeaderAction: action}, body)
	return code, b
}

func TestInstructionRoutes(t *testing.T) {
	e := newEnv(t)
	var a, b api.Instruction
	e.must(201, "POST", "/v1/instructions", e.tokA, api.InstructionIn{Text: " Never push to main. "}, &a)
	if a.Text != "Never push to main." || a.CreatedBy != "tower" {
		t.Errorf("machine add: %+v", a)
	}
	code, body := e.act("POST", "/v1/instructions", api.HeaderActionInstructions, api.InstructionIn{Text: "Write in British English."})
	if code != 201 || json.Unmarshal(body, &b) != nil || b.CreatedBy != "web:phone" {
		t.Fatalf("browser add: %d %s", code, body)
	}
	var list api.InstructionList
	code, body, _ = e.doWith("GET", "/v1/instructions", "", e.web, nil)
	if code != 200 || json.Unmarshal(body, &list) != nil || len(list.Instructions) != 2 || list.Version == "" {
		t.Fatalf("list: %d %s", code, body)
	}
	if code, _, _ := e.do("POST", "/v1/instructions", e.tokA, api.InstructionIn{Text: "a\nb"}); code != 400 {
		t.Errorf("newline: %d, want 400", code)
	}
	if code, _, _ := e.do("DELETE", "/v1/instructions/abc", e.tokA, nil); code != 400 {
		t.Errorf("bad id: %d, want 400", code)
	}
	if code, _, _ := e.do("DELETE", fmt.Sprintf("/v1/instructions/%d", a.ID+100), e.tokA, nil); code != 404 {
		t.Errorf("unknown id: %d, want 404", code)
	}
	if code, _ := e.act("DELETE", fmt.Sprintf("/v1/instructions/%d", a.ID), api.HeaderActionInstructions, nil); code != 204 {
		t.Errorf("browser delete: %d, want 204", code)
	}
	for i := 0; i < 6; i++ {
		e.must(201, "POST", "/v1/instructions", e.tokA, api.InstructionIn{Text: strings.Repeat("x", 300)}, nil)
	}
	code, body, _ = e.do("POST", "/v1/instructions", e.tokA, api.InstructionIn{Text: strings.Repeat("y", 300)})
	if code != 409 || !strings.Contains(string(body), "remove a rule first") {
		t.Errorf("past 2,000 characters: %d %s, want 409", code, body)
	}
}

func TestMessageRoutes(t *testing.T) {
	e := newEnv(t)
	timers := &fakeTimers{}
	e.server.after = timers.after
	e.controllable(sid1, "p1")
	// A poll is open when the message arrives: the send wakes it.
	poll := e.pollAsync(e.tokA, "?wait=30")
	timers.await(t, 1)
	code, body := e.act("POST", "/v1/messages", api.HeaderActionSend, api.MessagesIn{SessionIDs: []string{sid1, sid2}, Text: "Please rebase.",
		FromSession: sid2}) // ignored for a browser
	var out api.MessagesOut
	if code != 200 || json.Unmarshal(body, &out) != nil || len(out.Results) != 2 {
		t.Fatalf("send: %d %s", code, body)
	}
	if out.Results[0].State != api.MessageQueued || out.Results[1].State != api.MessageRefused || out.Results[1].Detail != "unknown session" {
		t.Errorf("results %+v", out.Results)
	}
	r := recv(t, poll)
	var claim api.ControlClaim
	if r.code != 200 || json.Unmarshal(r.body, &claim) != nil {
		t.Fatalf("poll: %d %s", r.code, r.body)
	}
	if claim.Request.Action != api.ActionMessage || claim.Request.ID != out.Results[0].ID ||
		claim.Text != "From the user via sessionhub (dashboard):\n\nPlease rebase." {
		t.Errorf("claim %+v text %q", claim.Request, claim.Text)
	}
	var res api.Message
	e.must(200, "POST", "/v1/control/"+claim.Request.ID+"/result", e.tokA, api.ControlResultIn{State: api.MessageDelivered}, &res)
	if res.State != api.MessageDelivered {
		t.Errorf("result %+v", res)
	}
	var m api.Message
	code, body, _ = e.doWith("GET", "/v1/messages/"+claim.Request.ID, "", e.web, nil)
	if code != 200 || json.Unmarshal(body, &m) != nil || m.State != api.MessageDelivered || m.Sender != api.SenderDashboard {
		t.Errorf("read: %d %s", code, body)
	}
	if code, _, _ := e.do("GET", "/v1/messages/nope", e.tokA, nil); code != 400 {
		t.Errorf("bad id: %d, want 400", code)
	}

	// A machine token sends as the CLI, or as a session with from_session.
	e.must(200, "POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "from bluebox"}, &out)
	if m, _ := e.st.GetMessage(t.Context(), out.Results[0].ID); m.Sender != "cli on bluebox" {
		t.Errorf("machine sender %q", m.Sender)
	}
	e.register(e.tokB, api.SessionUpsert{ID: sid2})
	e.must(200, "POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "from a session", FromSession: sid2}, &out)
	if m, _ := e.st.GetMessage(t.Context(), out.Results[0].ID); m.Sender != "session 3f2a9c10" {
		t.Errorf("session sender %q", m.Sender)
	}
	if code, _, _ := e.do("POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "x", FromSession: "bad id"}); code != 400 {
		t.Errorf("bad from_session: %d, want 400", code)
	}
	if code, _, _ := e.do("POST", "/v1/messages", e.tokB, api.MessagesIn{Text: "x"}); code != 400 {
		t.Errorf("no targets: %d, want 400", code)
	}
	// bluebox has sent two messages (as "cli on bluebox" and as a session): one budget.
	for i := 0; i < store.MessagesPerMinute-2; i++ {
		e.must(200, "POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "flood"}, nil)
	}
	if code, _, _ := e.do("POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "one too many"}); code != 429 {
		t.Errorf("31st message: %d, want 429", code)
	}
}

func TestMessageFromSessionOwnership(t *testing.T) {
	e := newEnv(t)
	e.controllable(sid1, "p1")
	e.register(e.tokB, api.SessionUpsert{ID: sid2})
	send := func(token, from string) int {
		code, _, _ := e.do("POST", "/v1/messages", token, api.MessagesIn{SessionIDs: []string{sid1}, Text: "hi", FromSession: from})
		return code
	}
	if code := send(e.tokB, "no-such-session"); code != 404 {
		t.Errorf("unknown from_session: %d, want 404", code)
	}
	if code := send(e.tokB, sid1); code != 409 {
		t.Errorf("another machine's from_session: %d, want 409", code)
	}
	if code := send(e.tokB, sid2); code != 200 {
		t.Errorf("own from_session: %d, want 200", code)
	}
}

func TestMessageLimitIgnoresFromSession(t *testing.T) {
	e := newEnv(t)
	e.controllable(sid1, "p1")
	// Each of bluebox's sessions has its own display sender; all share one budget.
	var froms []string
	for i := 0; i < store.MessagesPerMinute; i++ {
		id := fmt.Sprintf("%08d-bluebox-session", i)
		e.register(e.tokB, api.SessionUpsert{ID: id})
		froms = append(froms, id)
	}
	for _, from := range froms {
		e.must(200, "POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "x", FromSession: from}, nil)
	}
	extra := "99999999-bluebox-session"
	e.register(e.tokB, api.SessionUpsert{ID: extra})
	if code, _, _ := e.do("POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "x", FromSession: extra}); code != 429 {
		t.Errorf("a new from_session past the budget: %d, want 429", code)
	}
	if code, _, _ := e.do("POST", "/v1/messages", e.tokB, api.MessagesIn{SessionIDs: []string{sid1}, Text: "x"}); code != 429 {
		t.Errorf("the CLI sender past the budget: %d, want 429", code)
	}
	// Another machine and a browser keep their own budgets.
	e.must(200, "POST", "/v1/messages", e.tokA, api.MessagesIn{SessionIDs: []string{sid1}, Text: "x"}, nil)
	if code, _ := e.act("POST", "/v1/messages", api.HeaderActionSend, api.MessagesIn{SessionIDs: []string{sid1}, Text: "x"}); code != 200 {
		t.Errorf("browser: %d, want 200", code)
	}
}

func TestPermissionDecisionRoutes(t *testing.T) {
	e := newEnv(t)
	timers := &fakeTimers{}
	e.server.after = timers.after
	e.server.permCheck = 5 * time.Millisecond
	e.controllable(sid1, "p1")
	var p api.PermissionRequest
	e.must(201, "POST", "/v1/sessions/"+sid1+"/permissions", e.tokA,
		api.PermissionIn{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"git push"}`), CWD: "/home/user/proj"}, &p)
	if p.State != api.PermissionOpen || p.Machine != "tower" {
		t.Fatalf("created %+v", p)
	}
	if code, _, _ := e.do("POST", "/v1/sessions/"+sid1+"/permissions", e.tokB, api.PermissionIn{ToolName: "Bash"}); code != 409 {
		t.Errorf("bluebox posts for tower's session: %d, want 409", code)
	}
	if code, _, _ := e.do("GET", "/v1/permissions/"+p.ID+"/decision?wait=1", e.tokB, nil); code != 409 {
		t.Errorf("bluebox waits on tower's request: %d, want 409", code)
	}
	if code, _, _ := e.do("GET", "/v1/permissions/"+p.ID+"/decision?wait=x", e.tokA, nil); code != 400 {
		t.Errorf("wait=x: %d, want 400", code)
	}
	if code, _, _ := e.do("GET", "/v1/permissions/pr_bad/decision", e.tokA, nil); code != 400 {
		t.Errorf("bad id: %d, want 400", code)
	}

	// A decision wakes the waiting hook.
	wait := e.getAsync(e.tokA, "/v1/permissions/"+p.ID+"/decision?wait=30")
	timers.await(t, 1)
	if timers.wait(0) != 30*time.Second {
		t.Errorf("wait %s, want 30s", timers.wait(0))
	}
	code, body := e.act("POST", "/v1/permissions/"+p.ID+"/decide", api.HeaderActionApprove, api.DecisionIn{Decision: api.DecisionAllow})
	if code != 200 {
		t.Fatalf("decide: %d %s", code, body)
	}
	r := recv(t, wait)
	var got api.PermissionRequest
	if r.code != 200 || json.Unmarshal(r.body, &got) != nil || got.State != api.PermissionDecided ||
		got.Decision != api.DecisionAllow || got.DecidedBy != "web:phone" {
		t.Fatalf("hook got %d %s", r.code, r.body)
	}
	code, body = e.act("POST", "/v1/permissions/"+p.ID+"/decide", api.HeaderActionApprove, api.DecisionIn{Decision: api.DecisionDeny})
	if code != 409 {
		t.Errorf("second decision: %d %s, want 409", code, body)
	}
	// A machine decides as machine:<name>.
	var q api.PermissionRequest
	e.must(201, "POST", "/v1/sessions/"+sid1+"/permissions", e.tokA, api.PermissionIn{ToolName: "Bash"}, &q)
	e.must(200, "POST", "/v1/permissions/"+q.ID+"/decide", e.tokB, api.DecisionIn{Decision: api.DecisionDeny, Reason: "not now"}, &got)
	if got.DecidedBy != "machine:bluebox" || got.Reason != "not now" {
		t.Errorf("machine decision %+v", got)
	}

	// An answer in the terminal ends the wait with answered_locally.
	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"blocked"}`)
	var lp api.PermissionRequest
	e.must(201, "POST", "/v1/sessions/"+sid1+"/permissions", e.tokA, api.PermissionIn{ToolName: "Bash"}, &lp)
	wait = e.getAsync(e.tokA, "/v1/permissions/"+lp.ID+"/decision?wait=30")
	timers.await(t, 2)
	e.clock.Advance(time.Second)
	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"working"}`)
	r = recv(t, wait)
	if r.code != 200 || json.Unmarshal(r.body, &got) != nil || got.State != api.PermissionAnsweredLocally {
		t.Fatalf("after a local answer: %d %s", r.code, r.body)
	}
	if code, _ := e.act("POST", "/v1/permissions/"+lp.ID+"/decide", api.HeaderActionApprove, api.DecisionIn{Decision: api.DecisionAllow}); code != 410 {
		t.Errorf("decide after a local answer: %d, want 410", code)
	}

	// A wait that times out answers 204.
	var open api.PermissionRequest
	e.must(201, "POST", "/v1/sessions/"+sid1+"/permissions", e.tokA, api.PermissionIn{ToolName: "Bash"}, &open)
	wait = e.getAsync(e.tokA, "/v1/permissions/"+open.ID+"/decision?wait=5")
	timers.await(t, 3)
	timers.fire(2)
	if r := recv(t, wait); r.code != 204 {
		t.Errorf("timed-out wait: %d %s, want 204", r.code, r.body)
	}
	if code, _, _ := e.do("HEAD", "/v1/permissions/"+open.ID+"/decision", e.tokA, nil); code != 405 {
		t.Errorf("HEAD: %d, want 405", code)
	}
}

// wakeKeys is how many wake-up channels the sessionhub holds.
func (e *env) wakeKeys() int {
	e.server.control.mu.Lock()
	defer e.server.control.mu.Unlock()
	return len(e.server.control.wake)
}

// A decision poll that ends without a decision leaves no wake key behind.
func TestDecisionPollDropsWakeKey(t *testing.T) {
	e := newEnv(t)
	timers := &fakeTimers{}
	e.server.after = timers.after
	e.server.permCheck = 5 * time.Millisecond
	e.controllable(sid1, "p1")
	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"blocked"}`)
	post := func() api.PermissionRequest {
		var p api.PermissionRequest
		e.must(201, "POST", "/v1/sessions/"+sid1+"/permissions", e.tokA, api.PermissionIn{ToolName: "Bash"}, &p)
		return p
	}

	// Timed out.
	p := post()
	wait := e.getAsync(e.tokA, "/v1/permissions/"+p.ID+"/decision?wait=5")
	timers.await(t, 1)
	timers.fire(0)
	if r := recv(t, wait); r.code != 204 {
		t.Fatalf("timeout: %d %s", r.code, r.body)
	}
	if n := e.wakeKeys(); n != 0 {
		t.Errorf("%d wake keys after a timed-out poll, want 0", n)
	}

	// Answered in the terminal.
	wait = e.getAsync(e.tokA, "/v1/permissions/"+p.ID+"/decision?wait=30")
	timers.await(t, 2)
	e.clock.Advance(time.Second)
	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"working"}`)
	if r := recv(t, wait); r.code != 200 {
		t.Fatalf("local answer: %d %s", r.code, r.body)
	}
	if n := e.wakeKeys(); n != 0 {
		t.Errorf("%d wake keys after a locally answered poll, want 0", n)
	}

	// Expired while waiting.
	e.event(e.tokA, sid1, api.KindStateChanged, `{"agent_state":"blocked"}`)
	p = post()
	wait = e.getAsync(e.tokA, "/v1/permissions/"+p.ID+"/decision?wait=30")
	timers.await(t, 3)
	e.clock.Advance(store.PermissionTTL + time.Second)
	if r := recv(t, wait); r.code != 200 {
		t.Fatalf("expired: %d %s", r.code, r.body)
	}
	if n := e.wakeKeys(); n != 0 {
		t.Errorf("%d wake keys after an expired poll, want 0", n)
	}
	if code, _ := e.act("POST", "/v1/permissions/"+p.ID+"/decide", api.HeaderActionApprove, api.DecisionIn{Decision: api.DecisionAllow}); code != 410 {
		t.Errorf("decide after expiry: %d, want 410", code)
	}

	// A decided request wakes the poll and drops its key too.
	p = post()
	wait = e.getAsync(e.tokA, "/v1/permissions/"+p.ID+"/decision?wait=30")
	timers.await(t, 4)
	e.act("POST", "/v1/permissions/"+p.ID+"/decide", api.HeaderActionApprove, api.DecisionIn{Decision: api.DecisionDeny})
	recv(t, wait)
	if n := e.wakeKeys(); n != 0 {
		t.Errorf("%d wake keys after a decided poll, want 0", n)
	}
}

// A busy message is offered again 15 s after its last offer, and a watcher
// blocked in the long poll gets it then, not at its next poll.
func TestBusyMessageReofferedToOpenPoll(t *testing.T) {
	e := newEnv(t)
	timers := &fakeTimers{}
	e.server.after = timers.after
	e.controllable(sid1, "p1")
	var out api.MessagesOut
	e.must(200, "POST", "/v1/messages", e.tokA, api.MessagesIn{SessionIDs: []string{sid1}, Text: "rebase"}, &out)
	id := out.Results[0].ID
	first := recv(t, e.pollAsync(e.tokA, "?wait=30"))
	var claim api.ControlClaim
	if first.code != 200 || json.Unmarshal(first.body, &claim) != nil || claim.Request.ID != id {
		t.Fatalf("first poll: %d %s", first.code, first.body)
	}
	e.must(200, "POST", "/v1/control/"+id+"/result", e.tokA, api.ControlResultIn{State: api.MessageBusy, Detail: "agent is working"}, nil)

	// The next poll finds nothing yet and arms a timer for the retry.
	poll := e.pollAsync(e.tokA, "?wait=30")
	timers.await(t, 3) // the first poll's timer, then this poll's, then the retry timer
	if got := timers.wait(2); got != store.MessageRetry {
		t.Errorf("retry timer %s, want %s", got, store.MessageRetry)
	}
	select {
	case r := <-poll:
		t.Fatalf("poll answered before the retry: %d %s", r.code, r.body)
	case <-time.After(50 * time.Millisecond):
	}
	e.clock.Advance(store.MessageRetry)
	timers.fire(2)
	r := recv(t, poll)
	if r.code != 200 || json.Unmarshal(r.body, &claim) != nil || claim.Request.ID != id || claim.Request.Action != api.ActionMessage {
		t.Fatalf("re-offer: %d %s", r.code, r.body)
	}
}

// TestPermissionCutInputRefusesAllow: an allow for a cut input is 409 with
// the server's reason and leaves the request open; a deny works.
func TestPermissionCutInputRefusesAllow(t *testing.T) {
	e := newEnv(t)
	e.controllable(sid1, "p1")
	var p api.PermissionRequest
	big := `{"file_path":"/a","content":"` + strings.Repeat("x", 9000) + `"}`
	e.must(201, "POST", "/v1/sessions/"+sid1+"/permissions", e.tokA, api.PermissionIn{ToolName: "Write", ToolInput: json.RawMessage(big)}, &p)
	if !p.Truncated {
		t.Fatalf("created %+v, want truncated", p.Truncated)
	}
	for name, do := range map[string]func() (int, []byte){
		"browser": func() (int, []byte) {
			return e.act("POST", "/v1/permissions/"+p.ID+"/decide", api.HeaderActionApprove, api.DecisionIn{Decision: api.DecisionAllow})
		},
		"machine": func() (int, []byte) {
			code, body, _ := e.do("POST", "/v1/permissions/"+p.ID+"/decide", e.tokB, api.DecisionIn{Decision: api.DecisionAllow})
			return code, body
		},
	} {
		code, body := do()
		if code != 409 || !strings.Contains(string(body), "the input was cut; allow it in the terminal") {
			t.Errorf("%s allow: %d %s, want 409 with the reason", name, code, body)
		}
	}
	// Still open: the deny decides it (a decided request would be 409).
	var got api.PermissionRequest
	e.must(200, "POST", "/v1/permissions/"+p.ID+"/decide", e.tokB, api.DecisionIn{Decision: api.DecisionDeny, Reason: "too long"}, &got)
	if got.State != api.PermissionDecided || got.Decision != api.DecisionDeny {
		t.Errorf("deny: %+v", got)
	}
}

// ErrGone maps to 410: a request for an ended session.
func TestPermissionForEndedSessionIs410(t *testing.T) {
	e := newEnv(t)
	e.controllable(sid1, "p1")
	// ErrGone: a request for an ended session.
	e.event(e.tokA, sid1, api.KindEnded, `{}`)
	if code, body, _ := e.do("POST", "/v1/sessions/"+sid1+"/permissions", e.tokA, api.PermissionIn{ToolName: "Bash"}); code != 410 {
		t.Errorf("request for an ended session: %d %s, want 410", code, body)
	}
}
