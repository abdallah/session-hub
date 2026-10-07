package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

// authCase is one route with a request that succeeds for a machine token.
type authCase struct {
	path   string // concrete path to request
	body   any
	ok     int               // status for an authorized request
	alsoOK int               // another status an authorized request may get, or 0
	okB    int               // status for the other machine's token (bluebox), if not ok
	fresh  func() string     // if set, makes the path for each request (the route uses it up)
	hdr    map[string]string // extra request headers
}

// TestAuthMatrix covers every route × every credential, and proves a
// rejected request changed nothing.
func TestAuthMatrix(t *testing.T) {
	e := newEnv(t)
	e.server.after = instantTimer // a poll with nothing pending answers 204 at once
	e.register(e.tokA, api.SessionUpsert{ID: sid1, HerdrSession: "default", HerdrPane: "p1", CWD: "/home/user/proj"})
	// sid3 carries a request tower claimed, for the result route. Its herdr
	// session differs, so the herdr-sessions case doesn't end it, and its ID
	// prefix differs from sid1's, so the sid1[:8] lookup stays unique.
	e.register(e.tokA, api.SessionUpsert{ID: sid3, HerdrSession: "other", HerdrPane: "p2"})
	e.recordPoll(e.tokA)
	ctx := context.Background()
	if _, _, err := e.st.CreateControl(ctx, sid3, api.RequestedByDashboard); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := e.st.ClaimControl(ctx, e.machine(e.tokA).ID)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	const link = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"

	// A message to sid1 for GET /v1/messages/{id}. The authorized control
	// poll may claim it, which its alsoOK covers.
	sent, _, err := e.st.SendMessages(ctx, []string{sid1}, "matrix message", "cli on tower", "machine:tower")
	if err != nil || sent[0].ID == "" {
		t.Fatalf("send: %+v %v", sent, err)
	}
	// sid3's open request serves the decision poll; each decide request
	// gets a fresh one, so a rejected decide that wrote shows up.
	newPermission := func() api.PermissionRequest {
		p, err := e.st.CreatePermission(ctx, e.machine(e.tokA).ID, sid3, api.PermissionIn{ToolName: "Bash",
			ToolInput: json.RawMessage(`{"command":"ls"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	waitOn := newPermission()
	ruleN := 0
	freshRule := func() string {
		ruleN++
		r, err := e.st.AddInstruction(ctx, fmt.Sprintf("matrix rule %d", ruleN), "tower")
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("/v1/instructions/%d", r.ID)
	}

	// Moves. Both machines have a key and polled. Each upload and result
	// gets a fresh move in the state it needs, so a rejected request that
	// wrote shows up. claimUntil claims past older moves that authorized
	// requests left behind.
	towerKey := testMoveKey(t)
	for _, k := range []struct{ tok, key string }{{e.tokA, towerKey}, {e.tokB, testMoveKey(t)}} {
		if err := e.st.SetMoveKey(ctx, e.machine(k.tok).ID, k.key); err != nil {
			t.Fatal(err)
		}
	}
	e.recordPoll(e.tokB)
	moveN := 0
	movable := func(tok string) string {
		moveN++
		id := fmt.Sprintf("move-%d", moveN)
		e.register(tok, api.SessionUpsert{ID: id, HerdrSession: fmt.Sprintf("mv%d", moveN), HerdrPane: "p1", AgentState: "idle"})
		return id
	}
	claimUntil := func(tok, id string) {
		for range 50 {
			c, ok, err := e.st.ClaimMove(ctx, e.machine(tok).ID)
			if err != nil || !ok {
				t.Fatalf("claim %s: %v %v", id, ok, err)
			}
			if c.Move.ID == id {
				return
			}
		}
		t.Fatalf("move %s never claimed", id)
	}
	// packed is a move from tower to bluebox that tower claimed: tower uploads.
	packed := func() string {
		mv, err := e.st.CreateMove(ctx, movable(e.tokA), "bluebox", "machine:tower")
		if err != nil {
			t.Fatal(err)
		}
		claimUntil(e.tokA, mv.ID)
		return mv.ID
	}
	// unpacking is a move from bluebox to tower, uploaded and claimed by tower:
	// tower downloads and posts the result.
	unpacking := func() string {
		mv, err := e.st.CreateMove(ctx, movable(e.tokB), "tower", "machine:bluebox")
		if err != nil {
			t.Fatal(err)
		}
		claimUntil(e.tokB, mv.ID)
		if _, err := e.st.MoveUploaded(ctx, e.machine(e.tokB).ID, mv.ID, 6); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(e.moveDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.moveDir, mv.ID+".sealed"), []byte("sealed"), 0o600); err != nil {
			t.Fatal(err)
		}
		claimUntil(e.tokA, mv.ID)
		return mv.ID
	}
	downloadID := unpacking()
	linkCode := e.loginCode("tablet") // one unused code serves every GET
	freshName := 0
	nextName := func() string { freshName++; return fmt.Sprintf("fresh%d", freshName) }

	// Each triage request targets a session nobody triaged yet, so a
	// rejected request that wrote a row shows up in the snapshot.
	inboxN := 0
	freshInbox := func(action string) func() string {
		return func() string {
			inboxN++
			id := fmt.Sprintf("inbox-%d", inboxN)
			e.register(e.tokA, api.SessionUpsert{ID: id})
			return "/v1/inbox/" + id + "/" + action
		}
	}
	triageSince := e.clock.Now()
	snoozeUntil := triageSince.Add(time.Hour)

	// tower's watcher polled, so it takes start requests.
	if err := e.st.RecordPoll(ctx, e.machine(e.tokA).ID); err != nil {
		t.Fatal(err)
	}
	started, err := e.st.CreateStart(ctx, "tower", api.StartIn{Dir: "/srv/app"}, "machine:tower")
	if err != nil {
		t.Fatal(err)
	}

	// The mod's routes. Each blocked-on request targets a new session, so
	// sid1's state stays as the events case left it; each result gets a new
	// queued message, which an authorized result closes.
	modN := 0
	freshBlocked := func() string {
		modN++
		id := fmt.Sprintf("mod-%d", modN)
		e.register(e.tokA, api.SessionUpsert{ID: id})
		return "/v1/sessions/" + id + "/blocked-on"
	}
	freshResult := func() string {
		modN++
		res, _, err := e.st.SendMessages(ctx, []string{sid1}, "result message", "cli on tower", fmt.Sprintf("machine:matrix%d", modN))
		if err != nil || res[0].ID == "" {
			t.Fatalf("send: %+v %v", res, err)
		}
		return "/v1/messages/" + res[0].ID + "/result"
	}
	// Tasks. Each request gets a fresh task in the state it needs, so a
	// rejected request that wrote shows up in the snapshot.
	taskN := 0
	towerYou := store.Actor{Name: "machine:tower"}
	newTask := func(agent bool) string {
		taskN++
		id := fmt.Sprintf("t_matrix%05d", taskN)
		a := towerYou
		if agent {
			a = store.Actor{Name: "machine:tower", Agent: true, MachineID: e.machine(e.tokA).ID}
		}
		if _, _, err := e.st.CreateTask(ctx, api.TaskIn{ID: id, Title: "matrix task"}, a); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mergeInto := newTask(false)
	editTitle := "edited"
	usagePct, usageCost := 37, 0.42

	cases := map[string]authCase{
		"POST /v1/sessions/{id}/blocked-on": {fresh: freshBlocked, body: api.BlockedOnIn{Text: "Question: Which one?"}, ok: 200, okB: 409},
		"PUT /v1/sessions/{id}/usage": {path: "/v1/sessions/" + sid1 + "/usage",
			body: api.UsageIn{ContextPercent: &usagePct, CostUSD: &usageCost}, ok: 200, okB: 409},
		// 200 when the poll claims a queued message to sid1, else 204.
		"GET /v1/sessions/{id}/messages/next": {path: "/v1/sessions/" + sid1 + "/messages/next?wait=1", ok: 204, alsoOK: 200, okB: 409},
		"POST /v1/messages/{id}/result":       {fresh: freshResult, body: api.MessageResultIn{State: api.MessageDelivered}, ok: 200, okB: 409},
		"GET /v1/inbox":                       {path: "/v1/inbox", ok: 200},
		"GET /v1/instructions":                {path: "/v1/instructions", ok: 200},
		"GET /v1/tasks":                       {path: "/v1/tasks", ok: 200},
		"GET /v1/tasks/day":                   {path: "/v1/tasks/day?date=2026-10-03&tz=Asia/Amman", ok: 200},
		"GET /v1/tasks/review":                {path: "/v1/tasks/review", ok: 200},
		"POST /v1/tasks":                      {path: "/v1/tasks", body: api.TaskIn{Title: "matrix create", SessionID: sid1}, ok: 201, okB: 409},
		"POST /v1/notes":                      {path: "/v1/notes", body: api.NoteIn{ID: "te_MATRIXNOTE1", Text: "matrix note", SessionID: sid1}, ok: 200, okB: 409},
		"POST /v1/tasks/review/ignore":        {path: "/v1/tasks/review/ignore", body: api.TaskLinkIn{SessionID: sid1}, ok: 204},
		"POST /v1/tasks/{id}/state": {fresh: func() string { return "/v1/tasks/" + newTask(false) + "/state" },
			body: api.TaskStateIn{To: api.TaskInProgress}, ok: 200},
		"POST /v1/tasks/{id}/merge": {fresh: func() string {
			taskN++
			id := fmt.Sprintf("t_matrix%05d", taskN)
			a := store.Actor{Name: "machine:tower", Agent: true, MachineID: e.machine(e.tokA).ID}
			if _, _, err := e.st.CreateTask(ctx, api.TaskIn{ID: id, Title: "matrix proposal"}, a); err != nil {
				t.Fatal(err)
			}
			return "/v1/tasks/" + id + "/merge"
		}, body: api.TaskMergeIn{Into: mergeInto}, ok: 200},
		"POST /v1/tasks/{id}/sessions": {fresh: func() string { return "/v1/tasks/" + newTask(false) + "/sessions" },
			body: api.TaskLinkIn{SessionID: sid1}, ok: 200, okB: 409},
		"PATCH /v1/tasks/{id}": {fresh: func() string { return "/v1/tasks/" + newTask(false) },
			body: api.TaskEditIn{Title: &editTitle}, ok: 200},
		"POST /v1/instructions":        {path: "/v1/instructions", body: api.InstructionIn{Text: "Use British spelling."}, ok: 201},
		"DELETE /v1/instructions/{id}": {fresh: freshRule, ok: 204},
		"POST /v1/messages":            {path: "/v1/messages", body: api.MessagesIn{SessionIDs: []string{sid1}, Text: "hello"}, ok: 200},
		"GET /v1/messages/{id}":        {path: "/v1/messages/" + sent[0].ID, ok: 200},
		"POST /v1/sessions/{id}/permissions": {path: "/v1/sessions/" + sid1 + "/permissions",
			body: api.PermissionIn{ToolName: "Bash"}, ok: 201, okB: 409},
		"GET /v1/permissions/{id}/decision": {path: "/v1/permissions/" + waitOn.ID + "/decision?wait=1", ok: 204, okB: 409},
		"POST /v1/permissions/{id}/decide": {fresh: func() string { return "/v1/permissions/" + newPermission().ID + "/decide" },
			body: api.DecisionIn{Decision: api.DecisionAllow}, ok: 200},
		"POST /v1/inbox/{id}/dismiss": {fresh: freshInbox("dismiss"),
			body: api.TriageIn{Since: triageSince}, ok: 204},
		"POST /v1/inbox/{id}/snooze": {fresh: freshInbox("snooze"),
			body: api.TriageIn{Since: triageSince, Until: &snoozeUntil}, ok: 204},
		"POST /v1/sessions/{id}/move": {fresh: func() string { return "/v1/sessions/" + movable(e.tokA) + "/move" },
			body: api.MoveIn{Target: "bluebox"}, ok: 202},
		"GET /v1/moves/{id}": {path: "/v1/moves/" + downloadID, ok: 200},
		"POST /v1/machines/{name}/start": {path: "/v1/machines/tower/start",
			body: api.StartIn{Dir: "~/Code/app", Prompt: "hello"}, ok: 202},
		"GET /v1/starts/{id}": {path: "/v1/starts/" + started.ID, ok: 200},
		"PUT /v1/moves/{id}/bundle": {fresh: func() string { return "/v1/moves/" + packed() + "/bundle" },
			body: []byte("sealed bundle"), ok: 200, okB: 409},
		"GET /v1/moves/{id}/bundle": {path: "/v1/moves/" + downloadID + "/bundle", ok: 200, okB: 409},
		"POST /v1/moves/{id}/result": {fresh: func() string { return "/v1/moves/" + unpacking() + "/result" },
			body: api.MoveResultIn{State: api.MoveFailed, Detail: "matrix"}, ok: 200, okB: 409},
		"PUT /v1/machines/self/move-key": {path: "/v1/machines/self/move-key", body: api.MoveKeyIn{PublicKey: towerKey}, ok: 204},
		"GET /healthz":                   {path: "/healthz", ok: 200},
		"GET /favicon.ico":               {path: "/favicon.ico", ok: 200},
		"GET /apple-touch-icon.png":      {path: "/apple-touch-icon.png", ok: 200},
		"GET /{$}":                       {path: "/", ok: 200}, // the dashboard page
		"POST /v1/sessions": {path: "/v1/sessions",
			body: api.SessionUpsert{ID: sid1, Source: api.SourcePlugin, GitBranch: "main"}, ok: 200, okB: 409},
		"PUT /v1/machines/self/herdr-sessions": {path: "/v1/machines/self/herdr-sessions",
			body: api.HerdrSessionsPut{HerdrSession: "default", Sessions: []api.SessionUpsert{{ID: sid1, HerdrPane: "p1"}}}, ok: 200},
		"PUT /v1/sessions/{id}/digest": {path: "/v1/sessions/" + sid1 + "/digest",
			body: api.DigestIn{AsOf: time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)}, ok: 200, okB: 409},
		"POST /v1/sessions/{id}/events": {path: "/v1/sessions/" + sid1 + "/events",
			body: api.EventIn{Kind: api.KindStateChanged, Source: api.SourcePlugin, Payload: json.RawMessage(`{"agent_state":"working"}`)}, ok: 200, okB: 409},
		"POST /v1/sessions/{id}/report": {path: "/v1/sessions/" + sid1 + "/report",
			body: api.ReportIn{Done: []string{"wrote tests"}}, ok: 200, okB: 409},
		"POST /v1/sessions/{id}/title": {path: "/v1/sessions/" + sid1 + "/title", body: api.TitleIn{Title: "auth work"}, ok: 200, okB: 409},
		// The first authorized request creates (202); later ones return it (200).
		"POST /v1/sessions/{id}/remote-control": {path: "/v1/sessions/" + sid1 + "/remote-control", ok: 202, alsoOK: 200},
		// 204 with nothing pending; 200 once the case above created a request.
		"GET /v1/machines/self/control": {path: "/v1/machines/self/control?wait=1", ok: 204, alsoOK: 200},
		"POST /v1/control/{id}/result": {path: "/v1/control/" + claim.Request.ID + "/result",
			body: api.ControlResultIn{State: api.ControlDone, URL: link}, ok: 200, okB: 409},
		"GET /v1/sessions":      {path: "/v1/sessions", ok: 200},
		"GET /v1/sessions/{id}": {path: "/v1/sessions/" + sid1[:8], ok: 200},
		"GET /v1/machines":      {path: "/v1/machines", ok: 200},
		"GET /login/{code}":     {path: "/login/" + linkCode, ok: 200},
		"POST /login/{code}": {fresh: func() string { return "/login/" + e.loginCode(nextName()) },
			hdr: map[string]string{"Origin": testPublicURL}, ok: 303},
		"POST /logout":         {path: "/logout", ok: 204},
		"POST /v1/logins":      {path: "/v1/logins", body: api.LoginIn{Name: "tablet"}, ok: 201},
		"GET /v1/web-sessions": {path: "/v1/web-sessions", ok: 200},
		"DELETE /v1/web-sessions/{id}": {fresh: func() string {
			_, id := e.webSession(nextName())
			return "/v1/web-sessions/" + id
		}, ok: 204},
	}

	// Mechanical coverage: every registered route has a case, and no case
	// is left over from a removed route.
	routes := e.server.routes()
	seen := map[string]bool{}
	for _, rt := range routes {
		key := rt.method + " " + rt.path
		seen[key] = true
		if _, ok := cases[key]; !ok {
			t.Errorf("route %s has no auth test case", key)
		}
	}
	for key := range cases {
		if !seen[key] {
			t.Errorf("auth case %s matches no route", key)
		}
	}

	// Browser credentials get a fresh session for each request, because a
	// request may end its session (POST /logout).
	const newSession = "new session"
	tokens := []struct {
		name   string
		bearer string
		cookie string // session cookie value; newSession mints one per request
		action string // X-Hub-Action value to send; empty sends none
	}{
		{"none", "", "", ""},
		{"bad", "hub_m_not-a-real-token", "", ""},
		{"read token", "hub_r_test-read-token-0123456789abcdefghijklmnop", "", ""},
		{"machine", e.tokA, "", ""},
		{"machineB", e.tokB, "", ""},
		{"cookie", "", newSession, ""},
		{"cookie+remote-control", "", newSession, api.HeaderActionRemoteControl},
		{"cookie+sign-out", "", newSession, api.HeaderActionSignOut},
		{"cookie+triage", "", newSession, api.HeaderActionTriage},
		{"cookie+instructions", "", newSession, api.HeaderActionInstructions},
		{"cookie+send", "", newSession, api.HeaderActionSend},
		{"cookie+approve", "", newSession, api.HeaderActionApprove},
		{"cookie+move", "", newSession, api.HeaderActionMove},
		{"cookie+start", "", newSession, api.HeaderActionStart},
		{"cookie+tasks", "", newSession, api.HeaderActionTasks},
		{"cookie+wrong action", "", newSession, "title"},
		{"badcookie", "", "hub_s_wrong", ""},
	}
	// actionFor is the X-Hub-Action value each actor route needs with the
	// cookie. The credential named cookie+<value> sends exactly that value.
	actionFor := map[string]string{
		accessAction:       api.HeaderActionRemoteControl,
		accessTriage:       api.HeaderActionTriage,
		accessInstructions: api.HeaderActionInstructions,
		accessSend:         api.HeaderActionSend,
		accessApprove:      api.HeaderActionApprove,
		accessMove:         api.HeaderActionMove,
		accessStart:        api.HeaderActionStart,
		accessTasks:        api.HeaderActionTasks,
	}
	expect := func(rt route, c authCase, tk string) []int {
		switch {
		case rt.access == accessPublic:
		case tk == "none" || tk == "bad" || tk == "read token" || tk == "badcookie":
			return []int{http.StatusUnauthorized}
		case (rt.access == accessWrite || rt.access == accessMachine) && strings.HasPrefix(tk, "cookie"):
			return []int{http.StatusUnauthorized} // the cookie never authorizes a machine route
		case actionFor[rt.access] != "" && strings.HasPrefix(tk, "cookie") && tk != "cookie+"+actionFor[rt.access]:
			return []int{http.StatusForbidden} // the cookie only with the route's own X-Hub-Action
		case rt.access == accessSignOut && tk != "cookie+sign-out":
			return []int{http.StatusForbidden} // only a browser ends its own session
		case tk == "machineB" && c.okB != 0:
			return []int{c.okB}
		}
		if c.alsoOK != 0 {
			return []int{c.ok, c.alsoOK}
		}
		return []int{c.ok}
	}
	minted := 0
	failures := 0
	for _, rt := range routes {
		c := cases[rt.method+" "+rt.path]
		for _, tk := range tokens {
			want := expect(rt, c, tk.name)
			var hdr map[string]string
			if tk.action != "" {
				hdr = map[string]string{api.HeaderAction: tk.action}
			}
			cookie := tk.cookie
			if cookie == newSession {
				minted++
				cookie, _ = e.webSession(fmt.Sprintf("matrix%d", minted))
			}
			path := c.path
			if c.fresh != nil {
				path = c.fresh()
			}
			for k, v := range c.hdr {
				if hdr == nil {
					hdr = map[string]string{}
				}
				hdr[k] = v
			}
			before := e.snapshot()
			code, body, h := e.doHdr(rt.method, path, tk.bearer, cookie, hdr, c.body)
			if strings.Contains(string(body), store.SessionTokenPrefix) {
				failures++
				t.Errorf("%s %s with %s: response body contains a session token", rt.method, rt.path, tk.name)
			}
			if !slices.Contains(want, code) {
				failures++
				t.Errorf("%s %s with %s: status %d, want %v; body %s", rt.method, rt.path, tk.name, code, want, body)
				continue
			}
			if code >= 400 && rt.access == accessPage {
				if ct := h.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") || !strings.Contains(string(body), "sessionhub login") {
					failures++
					t.Errorf("GET / with %s: 401 must be an HTML page naming sessionhub login; got %q %s", tk.name, ct, body)
				}
				if after := e.snapshot(); after != before {
					failures++
					t.Errorf("GET / with %s changed state", tk.name)
				}
			} else if code >= 400 {
				var ae api.Error
				if err := json.Unmarshal(body, &ae); err != nil || ae.Error == "" {
					failures++
					t.Errorf("%s %s with %s: error body %q is not api.Error", rt.method, rt.path, tk.name, body)
				}
				if ct := h.Get("Content-Type"); ct != "application/json" {
					t.Errorf("%s %s: error Content-Type %q", rt.method, rt.path, ct)
				}
				if code == http.StatusUnauthorized && rt.access != accessPage && !strings.HasPrefix(h.Get("WWW-Authenticate"), "Bearer") {
					t.Errorf("%s %s: 401 without WWW-Authenticate", rt.method, rt.path)
				}
				if after := e.snapshot(); after != before {
					failures++
					t.Errorf("%s %s with %s was rejected but changed state:\nbefore %s\nafter  %s",
						rt.method, rt.path, tk.name, before, after)
				}
			}
		}
	}
	t.Logf("auth matrix: %d routes x %d credentials, %d failures", len(routes), len(tokens), failures)

	// The authorized writes landed, and were attributed to tower.
	d := e.detail(sid1)
	if d.Machine != "tower" || d.Title != "auth work" || d.GitBranch != "main" || d.AgentState != "working" || len(d.Reports) != 1 {
		t.Errorf("authorized writes not visible: %+v", d.Session)
	}
	if d.ContextPercent == nil || *d.ContextPercent != usagePct || d.LiveCostUSD == nil || *d.LiveCostUSD != usageCost || !d.Messageable {
		t.Errorf("authorized mod writes not visible: %v %v %v", d.ContextPercent, d.LiveCostUSD, d.Messageable)
	}
	blocked := 0
	for _, x := range e.list("") {
		if x.BlockedOn == "Question: Which one?" && x.AgentState == "blocked" {
			blocked++
		}
	}
	if blocked != 1 {
		t.Errorf("%d sessions blocked on the question, want 1: the authorized blocked-on only", blocked)
	}
	if d3 := e.detail(sid3); d3.RemoteControlURL != link {
		t.Errorf("authorized result not visible: %q", d3.RemoteControlURL)
	}
	if list, _ := e.st.Instructions(ctx); !slices.ContainsFunc(list.Instructions, func(in api.Instruction) bool {
		return in.Text == "Use British spelling."
	}) {
		t.Error("authorized rule not stored")
	}
}

func TestTokenVariantsRejected(t *testing.T) {
	e := newEnv(t)
	// Case and prefix variations of real tokens are rejected.
	for _, tok := range []string{strings.ToUpper(e.tokA), e.tokA[:len(e.tokA)-1], e.tokA + "x", "hub_r_" + e.tokA[len("hub_m_"):], e.web} {
		if code, _, _ := e.do("GET", "/v1/sessions", tok, nil); code != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", tok, code)
		}
	}
	// Scheme is case-insensitive; other schemes are not bearer tokens.
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/sessions", nil)
	req.Header.Set("Authorization", "bearer "+e.tokA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("lowercase bearer: %d", resp.StatusCode)
	}
	req.Header.Set("Authorization", "Basic "+e.tokA)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("Basic scheme: %d, want 401", resp.StatusCode)
	}
}

func TestMachineLastSeenAndList(t *testing.T) {
	e := newEnv(t)
	start := e.clock.Now()
	e.clock.Advance(3e9)
	var ms []api.Machine
	code, b, _ := e.doWith("GET", "/v1/machines", "", e.web, nil)
	if code != 200 {
		t.Fatalf("GET /v1/machines with cookie: %d %s", code, b)
	}
	json.Unmarshal(b, &ms)
	if len(ms) != 2 || ms[0].Name != "bluebox" || ms[1].Name != "tower" {
		t.Fatalf("machines: %+v", ms)
	}
	if ms[0].LastSeen != nil || ms[1].LastSeen == nil || !ms[1].LastSeen.Equal(start) {
		t.Errorf("the session cookie must not mark machines seen: %+v", ms)
	}
	e.clock.Advance(3e9)
	e.must(200, "GET", "/v1/machines", e.tokA, nil, &ms)
	if ms[1].LastSeen == nil || !ms[1].LastSeen.Equal(e.clock.Now()) {
		t.Errorf("tower last_seen = %v, want %v", ms[1].LastSeen, e.clock.Now())
	}
	if ms[0].LastSeen != nil {
		t.Errorf("bluebox was never used but has last_seen %v", ms[0].LastSeen)
	}
	if ms[1].SSHHost != "tower.example.com" || ms[1].HerdrHost != "tower.example.com" || ms[0].SSHHost != "bluebox.example.com" {
		t.Errorf("hosts: %+v", ms)
	}
}
