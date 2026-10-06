package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// noRedirect is a client that returns the first response as it is.
func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestDashboardHTMLAndCSP(t *testing.T) {
	e := newEnv(t)
	code, body, hdr := e.doWith("GET", "/", "", e.web, nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type %q", ct)
	}
	csp := hdr.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "connect-src 'self'", "base-uri 'none'", "form-action 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-") || strings.Contains(csp, "*") {
		t.Errorf("CSP is not strict: %q", csp)
	}

	// The hashes in the header match the served inline script and style,
	// so the browser will run them and nothing else.
	page := string(body)
	for _, tc := range []struct{ re, directive string }{
		{`(?s)<script>(.*?)</script>`, "script-src"},
		{`(?s)<style>(.*?)</style>`, "style-src"},
	} {
		m := regexp.MustCompile(tc.re).FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("no inline block for %s", tc.directive)
		}
		sum := sha256.Sum256([]byte(m[1]))
		want := tc.directive + " 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if n := strings.Count(page, "<script"); n != 1 {
		t.Errorf("%d script elements, want 1 inline", n)
	}

	// Nothing loads from outside: no external URL, no src/href, no
	// inline event handlers or style attributes (CSP would block them).
	// The only links are the two same-origin icons in the head.
	icons := []string{
		`<link rel="icon" sizes="32x32" href="/favicon.ico?v=1">`,
		`<link rel="apple-touch-icon" href="/apple-touch-icon.png?v=1">`,
	}
	for _, l := range icons {
		if strings.Count(page, l) != 1 {
			t.Errorf("page has %d copies of %s, want 1", strings.Count(page, l), l)
		}
	}
	bare := page
	for _, l := range icons {
		bare = strings.Replace(bare, l, "", 1)
	}
	for _, bad := range []string{"http://", "https://", "//cdn", " src=", " href=", "url(", "@import", " onclick=", " style="} {
		if strings.Contains(bare, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	// The page renders values with textContent only.
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(page, bad) {
			t.Errorf("page uses %s", bad)
		}
	}
	if xc := hdr.Get("X-Content-Type-Options"); xc != "nosniff" {
		t.Errorf("X-Content-Type-Options %q", xc)
	}
	if cc := hdr.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q", cc)
	}
}

func TestDashboardWithoutCredentials(t *testing.T) {
	e := newEnv(t)
	code, body, hdr := e.do("GET", "/", "", nil)
	if code != http.StatusUnauthorized || !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") {
		t.Fatalf("GET / without cookie: %d %q", code, hdr.Get("Content-Type"))
	}
	if strings.Contains(string(body), "<script") || !strings.Contains(string(body), "sessionhub login") {
		t.Errorf("401 page: %s", body)
	}
	if csp := hdr.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("401 page CSP %q", csp)
	}
}

// The card logic runs under node, the only JavaScript engine on the
// machines that build sessionhub. Without node the table is skipped; the evidence
// for this task records a run with it.
func TestDashboardRemoteControlStates(t *testing.T) {
	type button struct {
		Label    string `json:"label"`
		Disabled bool   `json:"disabled"`
		Note     string `json:"note"`
	}
	type outcome struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
		URL  string `json:"url"`
	}
	const link = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"
	rc := &button{Label: "Remote Control"}
	resume := &button{Label: "Resume with Remote Control"}
	blocked := &button{Label: "Remote Control", Disabled: true, Note: "waiting on a prompt in the pane"}
	buttons := []struct {
		s    map[string]any
		want *button
	}{
		{map[string]any{"status": "live", "agent_state": "idle", "controllable": true}, rc},
		{map[string]any{"status": "live", "agent_state": "working", "controllable": true}, rc},
		{map[string]any{"status": "live", "agent_state": "done", "controllable": true}, rc},
		{map[string]any{"status": "blocked", "agent_state": "blocked", "controllable": true}, blocked},
		{map[string]any{"status": "stale", "agent_state": "idle", "controllable": true}, resume},
		{map[string]any{"status": "ended", "controllable": true}, resume},
		{map[string]any{"status": "live", "agent_state": "idle", "controllable": false}, nil},
		{map[string]any{"status": "ended"}, nil},
		// A moving session offers no Remote Control; an ended move does.
		{map[string]any{"status": "live", "agent_state": "idle", "controllable": true, "move": map[string]any{"state": "packing"}}, nil},
		{map[string]any{"status": "ended", "controllable": true, "move": map[string]any{"state": "unpacking"}}, nil},
		{map[string]any{"status": "ended", "controllable": true, "move": map[string]any{"state": "requested"}}, nil},
		{map[string]any{"status": "live", "agent_state": "idle", "controllable": true, "move": map[string]any{"state": "uploaded"}}, nil},
		{map[string]any{"status": "ended", "controllable": true, "move": map[string]any{"state": "failed"}}, resume},
		{map[string]any{"status": "live", "agent_state": "idle", "controllable": true, "move": map[string]any{"state": "done"}}, rc},
	}
	outcomes := []struct {
		req  map[string]any
		want *outcome
	}{
		{nil, nil},
		{map[string]any{"state": "pending"}, &outcome{"sending", "Sending…", ""}},
		{map[string]any{"state": "claimed"}, &outcome{"sending", "Sending…", ""}},
		{map[string]any{"state": "done", "url": link}, &outcome{"link", "", link}},
		{map[string]any{"state": "done", "url": link, "detail": "resumed in a new herdr workspace"}, &outcome{"link", "resumed in a new herdr workspace", link}},
		{map[string]any{"state": "done", "detail": "sent, link not seen"}, &outcome{"note", "sent, link not seen", ""}},
		{map[string]any{"state": "done", "url": "javascript:alert(1)"}, &outcome{"note", "Sent, but no link appeared.", ""}},
		{map[string]any{"state": "done", "url": "https://evil.example/code/session_x"}, &outcome{"note", "Sent, but no link appeared.", ""}},
		{map[string]any{"state": "done", "url": link + "\n"}, &outcome{"note", "Sent, but no link appeared.", ""}},
		{map[string]any{"state": "failed", "detail": "looks live but pane not found"}, &outcome{"error", "looks live but pane not found", ""}},
		{map[string]any{"state": "failed"}, &outcome{"error", "The machine could not turn on Remote Control.", ""}},
		{map[string]any{"state": "expired"}, &outcome{"error", "The machine didn't respond in time.", ""}},
		{map[string]any{"state": "surprise"}, nil},
	}
	refusals := []struct {
		status int
		body   any
		want   string
	}{
		{409, map[string]any{"error": `not controllable: the sessionhub watcher on machine "tower" is offline (no poll in the last 2m0s)`},
			`not controllable: the sessionhub watcher on machine "tower" is offline (no poll in the last 2m0s)`},
		// Markup in a refusal is text: the card puts it in with textContent.
		{429, map[string]any{"error": "<img src=x onerror=alert(1)>"}, "<img src=x onerror=alert(1)>"},
		{502, nil, "The server returned 502."},
		{500, map[string]any{"error": 5}, "The server returned 500."},
	}
	in := map[string]any{"buttons": []any{}, "outcomes": []any{}, "refusals": []any{}}
	for _, b := range buttons {
		in["buttons"] = append(in["buttons"].([]any), b.s)
	}
	for _, o := range outcomes {
		in["outcomes"] = append(in["outcomes"].([]any), o.req)
	}
	for _, r := range refusals {
		in["refusals"] = append(in["refusals"].([]any), []any{r.status, r.body})
	}
	// rcButton reads moveOpen from the move-state block.
	out := runBlocks(t, []string{"rc-state", "move-state"}, `
process.stdout.write(JSON.stringify({
  buttons: input.buttons.map(rcButton),
  outcomes: input.outcomes.map(rcOutcome),
  refusals: input.refusals.map(function (r) { return rcRefusal(r[0], r[1]); })
}));`, in)
	var got struct {
		Buttons  []*button  `json:"buttons"`
		Outcomes []*outcome `json:"outcomes"`
		Refusals []string   `json:"refusals"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %s: %v", out, err)
	}
	failures, n := 0, 0
	for i, b := range buttons {
		n++
		if !reflect.DeepEqual(got.Buttons[i], b.want) {
			failures++
			t.Errorf("rcButton(%v) = %+v, want %+v", b.s, got.Buttons[i], b.want)
		}
	}
	for i, o := range outcomes {
		n++
		if !reflect.DeepEqual(got.Outcomes[i], o.want) {
			failures++
			t.Errorf("rcOutcome(%v) = %+v, want %+v", o.req, got.Outcomes[i], o.want)
		}
	}
	for i, r := range refusals {
		n++
		if got.Refusals[i] != r.want {
			failures++
			t.Errorf("rcRefusal(%d, %v) = %q, want %q", r.status, r.body, got.Refusals[i], r.want)
		}
	}
	t.Logf("dashboard states: %d cases, %d failures", n, failures)
}

// The page's writes are the Remote Control request, sign-out, and inbox triage, each with its header.
func TestDashboardWrites(t *testing.T) {
	page := string(dashboardHTML)
	for _, c := range []struct {
		s    string
		want int
	}{
		{"method:", 9},
		{`method: "POST"`, 8},
		{`"X-Hub-Action": "start"`, 1},
		{"// start-state:begin", 1},
		{"// start-state:end", 1},
		{`"X-Hub-Action": "move"`, 1},
		{"// move-state:begin", 1},
		{"// move-state:end", 1},
		{"= moveRow(s)", 1},
		{`method: "DELETE"`, 1},
		{`"X-Hub-Action": "triage"`, 1},
		{`"X-Hub-Action": "instructions"`, 2},
		{`"X-Hub-Action": "send"`, 1},
		{`"X-Hub-Action": "approve"`, 1},
		{"// actions-state:begin", 1},
		{"// actions-state:end", 1},
		{"// inbox-state:begin", 1},
		{"// inbox-state:end", 1},
		{`"X-Hub-Action": "remote-control"`, 1},
		{`"X-Hub-Action": "sign-out"`, 1},
		{`"/remote-control"`, 1},
		{"fetch(", 17}, // the list, the inbox, the request, the session poll, the Details read, sign-out, triage,
		// the rules read, a rules write, the send, a decision, the machines read, a move, the move poll,
		// the New session panel's machines read, a start, and the start poll
		{"// rc-state:begin", 1},
		{"// rc-state:end", 1},
		{"// auth-state:begin", 1},
		{"// auth-state:end", 1},
		{"= rcRow(s, now)", 2},                    // the card and the inbox row share the Remote Control row
		{`el("code", null, s.resume_command)`, 1}, // one resume row, shared
		{`resumeRow(s, "Copy resume")`, 1},
		{`resumeRow(s, "Copy")`, 1},
	} {
		if got := strings.Count(page, c.s); got != c.want {
			t.Errorf("page has %q %d times, want %d", c.s, got, c.want)
		}
	}
	// A 401 stops polling and shows the signed-out message.
	for _, want := range []string{"if (auth.signedOut) { showSignedOut(); return null; }", "clearInterval(refreshTimer)",
		"refreshTimer = setInterval(load, REFRESH_MS)", "if (signedOut) return;", "if (signedOut) return null;\n        var auth = sessionAuth", `resp.headers.get("X-Hub-Session-Name")`,
		"whoName.textContent = auth.name", ".who[hidden] { display: none; }"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "?token=") {
		t.Error("page still mentions ?token=")
	}
	// Open Details survive card rebuilds: the state is kept outside the card.
	for _, want := range []string{"var openDetails = {}", "var detailCache = {}", "d.open = !!openDetails[s.id]", "openDetails[s.id] = d.open"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// A non-empty filter expands the ended groups without changing the
	// remembered state.
	for _, want := range []string{`var forced = q !== ""`, "d.open = forced || !!openEnded[name]", "if (!forced) openEnded[name] = d.open"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// The link opens in a new tab without the page as its opener.
	for _, want := range []string{`a.target = "_blank"`, `a.rel = "noopener noreferrer"`, "RC_URL.test(", "rcStale(rcState[", "rcPollUntil(r, now)", "rcServerOutcome(s.remote_control, now, s.remote_control_url)"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// The inbox polls with the list, picks its tab once, hides a row at once,
	// and handles a 401 like the list does.
	for _, want := range []string{"loadInbox();\n    fetch(\"/v1/sessions\"", `fetch("/v1/inbox"`,
		"var askedTab = hashTab(location.hash);\n  if (askedTab) showTab(askedTab);\n  load();",
		"if (t && !signedOut) showTab(t);",
		"pending = prunePending(data, pending)", "if (tab === null) showTab(firstTab(", "document.title = pageTitle(view.total)",
		"pending[id] = it.since", "var req = triageRequest(kind, id, it.since, until)",
		"if (resp.status === 401) { showSignedOut(); return null; }", `btn.textContent = label || "Copy"`,
		"var row = inboxEls[id]", "menu.open = !!openSnooze[s.id]", "openSnooze[s.id] = menu.open", "focusAfterTriage(nextId, it.group)",
		"        render(data);\n        renderInbox();", `<button id="tab-inbox"`, `<main id="inbox" hidden>`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// Machines collapse and cards expand from a header button; the state
	// outlives the 30 s rebuilds and is remembered per browser, with every
	// storage access in a try.
	for _, want := range []string{"// layout-state:begin", "// layout-state:end", "// row-state:begin", "// row-state:end",
		`keyed(el("button", "machine-head"), "machine:" + name)`, `keyed(el("button", "card-head"), "card:" + s.id)`,
		`mb.setAttribute("aria-expanded", open ? "true" : "false")`, `head.setAttribute("aria-expanded", open ? "true" : "false")`,
		"var open = machineOpen(name, collapsed, q)", "collapsed = toggleAll(allNames, collapsed)", "collapsed = toggleOne(collapsed, name)",
		"openCards = toggleOne(openCards, s.id)", `allCollapsed(allNames, collapsed) ? "Expand all" : "Collapse all"`,
		"try { return cleanMap(JSON.parse(window.localStorage.getItem(key)", "try { window.localStorage.setItem(key, JSON.stringify(m)); } catch (e)",
		"if (keep && focusedKey() !== keep) focusKeyed(keep);",
		// The inbox row: one tap target, then Done and Later.
		"var p = inboxPrimary(s, busy)", `el("a", "notif-main")`, `el("button", "notif-main")`, "requestRC(s.id);",
		`el("button", "textbtn", "Done")`, `el("summary", null, "Later")`, "openRows[s.id] = !open", "delete openRows[id]",
		`querySelector(".notif-main")`, "if (lastSessions.length) render(lastSessions);",
		"main { padding: 4px 0 32px; }", ".page { padding: 0 16px; }", "h2.machine-h { font-size: 1.25rem;",
		// Collapse all sits beside the filter; the inbox row names its tap in the actions row.
		`<button id="all" class="textbtn"`, "function syncAllButton(q)", `keyed(act, "act:" + s.id)`, "@media (hover: hover) and (pointer: fine)",
		// Rules, selection, the send bar, and the permission block.
		`<button id="tab-rules" class="tab"`, `<main id="rules" hidden>`, `<section id="sendbar" class="sendbar" hidden`,
		`<button id="select" class="textbtn" type="button" aria-pressed="false" hidden>Select</button>`,
		"if (selectMode || open) art.appendChild(pickBox(s));", `var pv = it.group === "blocked" ? permissionView(it.permission) : null;`,
		"if (pv) c.appendChild(permissionBlock(pv, s));", `if (pv.main) box.appendChild(pv.code ? el("code", "perm-main", pv.main)`,
		`window.confirm("Send this message to "`, `window.prompt("Deny "`, `window.confirm("Delete this rule for every session?`,
		`rulesRoot.hidden = name !== "rules";`, `if (name === "rules") loadRules();`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// Every storage access is inside loadMap or saveMap.
	if n := strings.Count(page, "localStorage"); n != 2 {
		t.Errorf("page mentions localStorage %d times, want 2 (loadMap and saveMap)", n)
	}
	for _, gone := range []string{`el("button", null, "Dismiss")`, `el("summary", null, "Snooze")`, ".actions button"} {
		if strings.Contains(page, gone) {
			t.Errorf("page still has %q", gone)
		}
	}
}

// TestDashboardActionStates runs the page's actions-state block (and
// inbox-state, for hashTab) under node: the Rules tab, the send bar, and
// the permission block.
func TestDashboardActionStates(t *testing.T) {
	script := `
var many = [];
for (var i = 0; i < 21; i++) many.push({ ok: true });
var byId = {
  a: { id: "a", title: "A", machine: "tower", controllable: true, messageable: true, status: "live" },
  b: { id: "b", title: "", machine: "bluebox", controllable: true, messageable: false, status: "live" },
  m: { id: "m", title: "M", machine: "bluebox", controllable: false, messageable: true, status: "live" }
};
var targets = sendTargets({ b: true, a: true, c: true, m: true, z: false }, byId);
process.stdout.write(JSON.stringify({
  hash: hashTab("#rules"),
  view: rulesView({ instructions: [{ id: 1, text: "Never push." }, { id: 2, text: "ééé" }] }),
  emptyView: rulesView(null),
  problems: [ruleProblem("  ", 100), ruleProblem("a\nb", 100), ruleProblem("x".repeat(301), 2000),
    ruleProblem("abcd", 3), ruleProblem(" ok ", 2000)],
  add: addRuleRequest("  Be brief.  "),
  del: deleteRuleRequest(7),
  targets: targets,
  sendProblems: [sendProblem("hi", targets), sendProblem("  ", targets), sendProblem("hi", []),
    sendProblem("x".repeat(4001), targets), sendProblem("hi", many)],
  send: sendRequest(["a"], "hi\nthere"),
  lines: sendResultLines({ results: [{ session_id: "a", state: "queued", id: "msg_x" },
    { session_id: "zzzzzzzzzz", state: "refused", detail: "unknown session" }] }, byId),
  perm: [permissionView({ id: "pr_1", tool_name: "Bash", tool_input: { command: "git push", description: "x" } }),
    permissionView({ id: "pr_2", tool_name: "Write", tool_input: { file_path: "/a", content: "x" } }),
    permissionView({ id: "pr_3", tool_name: "Odd", tool_input: { a: 1 } }),
    permissionView({ id: "pr_4", tool_name: "Write", tool_input: "cut…" }),
    permissionView(null)],
  permCut: [permissionView({ id: "pr_7", tool_name: "Write", truncated: true, tool_input: "{\"file_path\":\"/a\",\"content\":\"ab\u2026" }),
    permissionView({ id: "pr_8", tool_name: "Bash", truncated: true, tool_input: "{\"command\":\"ls\u2026" }),
    permissionView({ id: "pr_9", tool_name: "Write", tool_input: { file_path: "/b", content: "\u00e9".repeat(4000) } }),
    permissionView({ id: "pr_10", tool_name: "Edit", tool_input: { file_path: "/c", old_string: "x\u202ey", new_string: "z" } })],
  permClean: [permissionView({ id: "pr_5", tool_name: "Bash", tool_input: { command: "echo \u001b[31mhi\r\nrm -rf x\u202egnp.\tz\u0085" } }),
    permissionView({ id: "pr_6", tool_name: "Ba\u2066sh", tool_input: { command: "ls" } })],
  denyProblems: [denyProblem(""), denyProblem("use make"), denyProblem("a\nb"), denyProblem("x".repeat(201)), denyProblem("é".repeat(200))],
  allow: decideRequest("pr_1/x", "allow", "ignored"),
  deny: decideRequest("pr_1", "deny", "use make"),
  outcomes: [decisionOutcome(200, "allow", null), decisionOutcome(200, "deny", null), decisionOutcome(409, "allow", null),
    decisionOutcome(410, "allow", null), decisionOutcome(500, "allow", { error: "boom" }), decisionOutcome(502, "allow", null),
    decisionOutcome(409, "allow", { error: "the input was cut; allow it in the terminal" })],
  cutRefusals: [cutRefusal(409, { error: "the input was cut; allow it in the terminal" }), cutRefusal(409, { error: "conflict: already decided" }),
    cutRefusal(409, null), cutRefusal(400, { error: "the input was cut; allow it in the terminal" })]
}));`
	out := runBlocks(t, []string{"inbox-state", "actions-state"}, script, map[string]any{})
	want := `{
  "hash": "rules",
  "view": {"rules": [{"id": 1, "text": "Never push."}, {"id": 2, "text": "ééé"}], "used": 14, "left": 1986},
  "emptyView": {"rules": [], "used": 0, "left": 2000},
  "problems": ["Write the rule first.", "Keep the rule on one line.", "A rule holds at most 300 characters; this one has 301.",
    "The rules hold at most 2,000 characters; remove one first.", ""],
  "add": {"url": "/v1/instructions", "init": {"method": "POST",
    "headers": {"Content-Type": "application/json", "X-Hub-Action": "instructions"},
    "body": "{\"text\":\"Be brief.\"}", "cache": "no-store"}},
  "del": {"url": "/v1/instructions/7", "init": {"method": "DELETE", "headers": {"X-Hub-Action": "instructions"}, "cache": "no-store"}},
  "targets": [
    {"id": "a", "title": "A", "machine": "tower", "ok": true, "why": ""},
    {"id": "b", "title": "b", "machine": "bluebox", "ok": false, "why": "no sessionhub mod in the session, and not in herdr with its watcher running"},
    {"id": "c", "title": "c", "machine": "", "ok": false, "why": "no longer listed"},
    {"id": "m", "title": "M", "machine": "bluebox", "ok": true, "why": ""}],
  "sendProblems": ["", "Write the message first.", "Select at least one session sessionhub can message.",
    "A message holds at most 4,000 characters.", "Send to at most 20 sessions at a time."],
  "send": {"url": "/v1/messages", "init": {"method": "POST",
    "headers": {"Content-Type": "application/json", "X-Hub-Action": "send"},
    "body": "{\"session_ids\":[\"a\"],\"text\":\"hi\\nthere\"}", "cache": "no-store"}},
  "lines": ["A: queued; sessionhub types it in when the session is idle.", "zzzzzzzz: not sent: unknown session"],
  "perm": [
    {"id": "pr_1", "tool": "Bash", "main": "git push", "code": true, "input": "", "cut": false},
    {"id": "pr_2", "tool": "Write", "main": "/a", "code": false, "input": "{\"file_path\":\"/a\",\"content\":\"x\"}", "cut": false},
    {"id": "pr_3", "tool": "Odd", "main": "", "code": false, "input": "{\"a\":1}", "cut": false},
    {"id": "pr_4", "tool": "Write", "main": "", "code": false, "input": "cut…", "cut": false},
    null],
  "permCut": [
    {"id": "pr_7", "tool": "Write", "main": "", "code": false,
      "input": "{\"file_path\":\"/a\",\"content\":\"ab\u2026", "cut": true},
    {"id": "pr_8", "tool": "Bash", "main": "{\"command\":\"ls\u2026", "code": true, "input": "", "cut": true},
    {"id": "pr_9", "tool": "Write", "main": "/b", "code": false,
      "input": "{\"file_path\":\"/b\",\"content\":\"` + strings.Repeat(`\u00e9`, 4000) + `\"}", "cut": false},
    {"id": "pr_10", "tool": "Edit", "main": "/c", "code": false,
      "input": "{\"file_path\":\"/c\",\"old_string\":\"x\ufffdy\",\"new_string\":\"z\"}", "cut": false}],
  "permClean": [
    {"id": "pr_5", "tool": "Bash", "main": "echo \ufffd[31mhi\nrm -rf x\ufffdgnp.\tz\ufffd", "code": true, "input": "", "cut": false},
    {"id": "pr_6", "tool": "Ba\ufffdsh", "main": "ls", "code": false, "input": "{\"command\":\"ls\"}", "cut": false}],
  "denyProblems": ["", "", "Keep the reason on one line.", "A reason holds at most 200 characters; this one has 201.", ""],
  "allow": {"url": "/v1/permissions/pr_1%2Fx/decide", "init": {"method": "POST",
    "headers": {"Content-Type": "application/json", "X-Hub-Action": "approve"},
    "body": "{\"decision\":\"allow\"}", "cache": "no-store"}},
  "deny": {"url": "/v1/permissions/pr_1/decide", "init": {"method": "POST",
    "headers": {"Content-Type": "application/json", "X-Hub-Action": "approve"},
    "body": "{\"decision\":\"deny\",\"reason\":\"use make\"}", "cache": "no-store"}},
  "outcomes": ["Allowed once.", "Denied.", "Someone already answered this request.",
    "Too late: the prompt was answered in the terminal or expired.", "Not sent: boom", "Not sent: the server returned 502.",
    "Not sent: the input was cut; allow it in the terminal."],
  "cutRefusals": [true, false, false, false]
}`
	var got, exp map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %s: %v", out, err)
	}
	if err := json.Unmarshal([]byte(want), &exp); err != nil {
		t.Fatal(err)
	}
	for k, w := range exp {
		if !reflect.DeepEqual(got[k], w) {
			gb, _ := json.Marshal(got[k])
			wb, _ := json.Marshal(w)
			t.Errorf("%s:\n got %s\nwant %s", k, gb, wb)
		}
	}
}

// TestDashboardPermissionConfirm pins the permission block's safety rules:
// Allow once asks once more and shows the command again, the decided
// outcome outlives the inbox refresh, and the page never offers a lasting
// allow.
func TestDashboardPermissionConfirm(t *testing.T) {
	page := string(dashboardHTML)
	for _, want := range []string{
		"permState[pv.id] = { confirm: true, session: s.id, since: since, view: pv };",
		`el("code", "perm-main", pv.main)`, `"Yes, allow once"`, `if (st && st.confirm && !pv.cut)`,
		// An input the approver cannot see whole: a note, no Allow once, Deny stays.
		`el("p", "perm-cut", "Input was cut; check the terminal before allowing.")`,
		"allow.disabled = busy || pv.cut;", "deny.disabled = busy;",
		// The server's cut-input refusal leaves the row open, so Deny stays.
		"var settled = (r.status === 200 || r.status === 409 || r.status === 410) && !cutRefusal(r.status, r.body);",
		"if (pv.input) box.appendChild(permInput(pv));", `cf.appendChild(pv.code ? el("code", "perm-main", pv.main) : permInput(pv));`,
		"if (!pv) pv = decidedView(s.id, it.since);",
		".perm-main { white-space: pre-wrap;",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if n := strings.Count(page, "decide(pv, \"allow\""); n != 1 {
		t.Errorf("page calls decide(pv, \"allow\" %d times, want 1 (the confirm step)", n)
	}
	if strings.Contains(strings.ToLower(page), "always allow") {
		t.Error("page offers an always-allow choice")
	}
	// The page writes bidirectional controls and U+FFFD as \u escapes, so
	// no line of it reads differently from how it runs (Trojan Source).
	for _, r := range page {
		if r == 0x200E || r == 0x200F || r == 0x061C || r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069 || r == 0xFFFD {
			t.Errorf("page has a raw U+%04X; write it as a \\u escape", r)
		}
	}
}

// runBlocks runs the named begin/end blocks of the page under node with
// script appended, input on stdin, and TZ fixed, and returns stdout.
func runBlocks(t *testing.T, blocks []string, script string, input any) []byte {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; this table runs the page's " + strings.Join(blocks, " and ") + " blocks under node")
	}
	var src strings.Builder
	for _, b := range blocks {
		m := regexp.MustCompile(`(?s)// ` + b + `:begin\n(.*?)// ` + b + `:end`).FindSubmatch(dashboardHTML)
		if m == nil {
			t.Fatalf("dashboard/index.html has no %s block", b)
		}
		src.Write(m[1])
	}
	in, _ := json.Marshal(input)
	cmd := exec.Command(node, "-e", src.String()+"\nvar input = JSON.parse(require(\"fs\").readFileSync(0, \"utf8\"));\n"+script)
	cmd.Env = append(os.Environ(), "TZ=UTC")
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	return out
}

// TestDashboardLayoutStates runs the page's layout-state block under node:
// collapsing machines, expanding cards, the stored maps, the machine count,
// and the card header's badge.
func TestDashboardLayoutStates(t *testing.T) {
	const now = "2026-10-02T12:00:00Z"
	out := runBlocks(t, []string{"layout-state"}, `
var now = Date.parse(input.now);
process.stdout.write(JSON.stringify({
  clean: [cleanMap({ a: true, b: false, c: "yes", d: 1 }), cleanMap(null), cleanMap([true]), cleanMap("x")],
  prune: pruneMap({ a: true, b: true, c: true }, ["a", "c", "z"]),
  toggle: [toggleOne({}, "a"), toggleOne({ a: true, b: true }, "a")],
  open: [machineOpen("a", { a: true }, ""), machineOpen("b", { a: true }, ""), machineOpen("a", { a: true }, "x")],
  allColl: [allCollapsed(["a", "b"], { a: true }), allCollapsed(["a", "b"], { a: true, b: true }), allCollapsed([], {})],
  all: [toggleAll(["a", "b"], { a: true }), toggleAll(["a", "b"], { a: true, b: true }), toggleAll(["a"], {})],
  labels: [machineLabel(3, 0, 0), machineLabel(3, 2, 1), machineLabel(0, 1, 2)],
  needs: needsByMachine([{ items: [{ session: { machine: "m1" } }, { session: { machine: "m2" } }] },
    { items: [{ session: { machine: "m1" } }, { session: {} }] }]),
  ages: input.ages.map(function (a) { return shortAge(a, now); }),
  badges: input.badges.map(function (b) { return cardBadge(b[0], b[1], now); }),
  contexts: [contextLabel({ context_percent: 42 }), contextLabel({ context_percent: 0 }), contextLabel({ context_percent: 99.6 }),
    contextLabel({ context_percent: 140 }), contextLabel({ context_percent: "42" }), contextLabel({}), contextLabel(null)]
}));`, map[string]any{
		"now":  now,
		"ages": []any{"2026-10-02T11:59:48Z", "2026-10-02T11:56:00Z", "2026-10-02T09:00:00Z", "2026-09-29T12:00:00Z", "2026-10-02T12:00:30Z", "nope", nil},
		"badges": []any{
			[]any{map[string]any{"status": "blocked", "agent_state": "blocked"}, "2026-10-02T11:56:00Z"},
			[]any{map[string]any{"status": "blocked"}, nil},
			[]any{map[string]any{"status": "live", "agent_state": "working"}, nil},
			[]any{map[string]any{"status": "live", "agent_state": "idle"}, nil},
			[]any{map[string]any{"status": "live"}, nil},
			[]any{map[string]any{"status": "stale", "agent_state": "idle"}, nil},
			[]any{map[string]any{"status": "ended"}, nil},
		},
	})
	var got struct {
		Clean    []map[string]bool `json:"clean"`
		Prune    map[string]bool   `json:"prune"`
		Toggle   []map[string]bool `json:"toggle"`
		Open     []bool            `json:"open"`
		AllColl  []bool            `json:"allColl"`
		All      []map[string]bool `json:"all"`
		Labels   []string          `json:"labels"`
		Needs    map[string]int    `json:"needs"`
		Ages     []string          `json:"ages"`
		Badges   []string          `json:"badges"`
		Contexts []string          `json:"contexts"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %s: %v", out, err)
	}
	empty := map[string]bool{}
	checks := []struct {
		name      string
		got, want any
	}{
		{"cleanMap keeps true values only", got.Clean, []map[string]bool{{"a": true}, empty, empty, empty}},
		{"pruneMap", got.Prune, map[string]bool{"a": true, "c": true}},
		{"toggleOne", got.Toggle, []map[string]bool{{"a": true}, {"b": true}}},
		{"machineOpen: collapsed, not collapsed, filtered", got.Open, []bool{false, true, true}},
		{"allCollapsed", got.AllColl, []bool{false, true, false}},
		{"toggleAll: some, all, none collapsed", got.All, []map[string]bool{{"a": true, "b": true}, empty, {"a": true}}},
		{"machineLabel", got.Labels, []string{"3 active", "3 active, 2 ended · 1 needs you", "0 active, 1 ended · 2 need you"}},
		{"needsByMachine", got.Needs, map[string]int{"m1": 2, "m2": 1}},
		{"shortAge", got.Ages, []string{"12s", "4m", "3h", "3d", "0s", "", ""}},
		{"cardBadge", got.Badges, []string{"blocked 4m", "blocked", "working", "idle", "live", "stale", "ended"}},
		{"contextLabel", got.Contexts, []string{"context 42%", "context 0%", "context 100%", "context 100%", "", "", ""}},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestDashboardInboxRowStates runs the rc-state and row-state blocks under
// node: an inbox row's "what's needed" line and what a tap on the row does.
func TestDashboardInboxRowStates(t *testing.T) {
	const link = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"
	type primary struct {
		Kind  string `json:"kind"`
		Label string `json:"label"`
		URL   string `json:"url"`
	}
	needs := []struct {
		it    map[string]any
		s     map[string]any
		label string
		want  string
	}{
		{map[string]any{"waiting_on": []any{"review MR 391", "a decision on the schema"}}, map[string]any{"recap": "Next, merge."}, "Waiting on you",
			"review MR 391; a decision on the schema"},
		{map[string]any{"waiting_on": []any{"", 5}}, map[string]any{"recap": "Next, merge."}, "Waiting on you", "Next, merge."},
		{map[string]any{}, map[string]any{"recap": "Tests pass."}, "Finished", "Tests pass."},
		{map[string]any{}, map[string]any{}, "Blocked", "Blocked"},
		{map[string]any{}, nil, "Blocked", "Blocked"},
		{map[string]any{"group": "blocked", "waiting_on": []any{"w"}}, map[string]any{"blocked_on": "Question: Which one?", "recap": "r"}, "Blocked",
			"Question: Which one?"},
		{map[string]any{"group": "waiting", "waiting_on": []any{"w"}}, map[string]any{"blocked_on": "Question: Which one?"}, "Waiting on you", "w"},
		{map[string]any{"group": "blocked"}, map[string]any{"blocked_on": 5, "recap": "r"}, "Blocked", "r"},
		{map[string]any{"group": "blocked", "permission": map[string]any{"id": "pr_1", "tool_name": "Bash"}},
			map[string]any{"blocked_on": "Question: Which one?"}, "Blocked", "Question: Which one?"},
	}
	open := &primary{"open", "Open in Claude", link}
	cmd := "ssh -t m 'sessionhub resume x'"
	primaries := []struct {
		name string
		s    map[string]any
		busy bool
		want *primary
	}{
		{"live link", map[string]any{"status": "live", "controllable": true, "remote_control_url": link}, false, open},
		{"link wins over busy", map[string]any{"status": "live", "controllable": true, "remote_control_url": link}, true, open},
		{"bad link is ignored", map[string]any{"status": "live", "controllable": true, "remote_control_url": "javascript:alert(1)"}, false,
			&primary{"rc", "Remote Control", ""}},
		{"controllable live", map[string]any{"status": "live", "controllable": true}, false, &primary{"rc", "Remote Control", ""}},
		{"controllable stale", map[string]any{"status": "stale", "controllable": true, "resume_command": cmd}, false,
			&primary{"rc", "Resume with Remote Control", ""}},
		{"request open", map[string]any{"status": "live", "controllable": true}, true, &primary{"expand", "Sending…", ""}},
		{"blocked: the button is disabled", map[string]any{"status": "blocked", "controllable": true, "resume_command": cmd}, false,
			&primary{"expand", "Show resume command", ""}},
		{"not controllable", map[string]any{"status": "live", "resume_command": cmd}, false, &primary{"expand", "Show resume command", ""}},
		{"nothing at all", map[string]any{"status": "live"}, false, &primary{"expand", "Show more", ""}},
		{"moving: no Remote Control", map[string]any{"status": "ended", "controllable": true, "resume_command": cmd,
			"move": map[string]any{"state": "unpacking"}}, false, &primary{"expand", "Show resume command", ""}},
	}
	in := map[string]any{"needs": []any{}, "primaries": []any{}}
	for _, n := range needs {
		in["needs"] = append(in["needs"].([]any), []any{n.it, n.s, n.label})
	}
	for _, p := range primaries {
		in["primaries"] = append(in["primaries"].([]any), []any{p.s, p.busy})
	}
	out := runBlocks(t, []string{"rc-state", "row-state", "move-state"}, `
process.stdout.write(JSON.stringify({
  needs: input.needs.map(function (n) { return inboxNeed(n[0], n[1], n[2]); }),
  primaries: input.primaries.map(function (p) { return inboxPrimary(p[0], p[1]); })
}));`, in)
	var got struct {
		Needs     []string   `json:"needs"`
		Primaries []*primary `json:"primaries"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %s: %v", out, err)
	}
	for i, n := range needs {
		if got.Needs[i] != n.want {
			t.Errorf("inboxNeed(%v, %v, %q) = %q, want %q", n.it, n.s, n.label, got.Needs[i], n.want)
		}
	}
	for i, p := range primaries {
		if !reflect.DeepEqual(got.Primaries[i], p.want) {
			t.Errorf("inboxPrimary %s = %+v, want %+v", p.name, got.Primaries[i], p.want)
		}
	}
}

// After a reload the card rebuilds from the server's latest request, and
// local state never outlives newer server truth. The pure functions run under
// node like the table above.
func TestDashboardRemoteControlServerTruth(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; this table runs the page's rc-state block under node")
	}
	m := regexp.MustCompile(`(?s)// rc-state:begin\n(.*?)// rc-state:end`).FindSubmatch(dashboardHTML)
	if m == nil {
		t.Fatal("dashboard/index.html has no rc-state block")
	}
	const link = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"
	const now = "2026-09-30T12:00:00Z"
	ago := func(min int) string {
		n, _ := time.Parse(time.RFC3339, now)
		return n.Add(-time.Duration(min) * time.Minute).Format(time.RFC3339)
	}
	type outcome struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
		URL  string `json:"url"`
	}
	req := func(id, state string, extra map[string]any) map[string]any {
		r := map[string]any{"id": id, "state": state, "created_at": ago(30)}
		for k, v := range extra {
			r[k] = v
		}
		return r
	}
	outcomes := []struct {
		name string
		req  map[string]any
		url  string // the session's remote_control_url
		want *outcome
	}{
		{"none", nil, "", nil},
		{"open", req("a", "pending", nil), "", &outcome{"sending", "Sending…", ""}},
		{"failed 2m ago", req("a", "failed", map[string]any{"finished_at": ago(2), "detail": "pane not found"}), "", &outcome{"error", "pane not found", ""}},
		{"failed 11m ago", req("a", "failed", map[string]any{"finished_at": ago(11), "detail": "pane not found"}), "", nil},
		{"failed without finished_at", req("a", "failed", nil), "", nil},
		{"done with link", req("a", "done", map[string]any{"finished_at": ago(1), "url": link}), link, &outcome{"link", "", link}},
		{"done without link", req("a", "done", map[string]any{"finished_at": ago(1), "detail": "sent, link not seen"}), "", &outcome{"note", "sent, link not seen", ""}},
		{"expired 3m ago", req("a", "expired", map[string]any{"expires_at": ago(3)}), "", &outcome{"error", "The machine didn't respond in time.", ""}},
		{"expired 20m ago", req("a", "expired", map[string]any{"expires_at": ago(20)}), "", nil},
		// The request keeps its url after the session ends; the card must not show it.
		{"done with url, session ended (no remote_control_url)", req("a", "done", map[string]any{"finished_at": ago(1), "url": link, "detail": "Remote Control was already on"}), "",
			&outcome{"note", "Remote Control was already on", ""}},
		{"done with url, session ended, no detail", req("a", "done", map[string]any{"finished_at": ago(1), "url": link}), "", &outcome{"note", "", ""}},
		{"done with url, live session with remote_control_url", req("a", "done", map[string]any{"finished_at": ago(1), "url": link}), link, &outcome{"link", "", link}},
	}
	stales := []struct {
		name string
		st   map[string]any
		s    map[string]any
		want bool
	}{
		{"no local state", nil, map[string]any{"remote_control": req("b", "done", nil)}, false},
		{"sending, no request yet", map[string]any{"phase": "sending"}, map[string]any{"remote_control": req("b", "done", nil)}, false},
		{"same request", map[string]any{"phase": "waiting", "req": req("a", "pending", nil)}, map[string]any{"remote_control": req("a", "claimed", nil)}, false},
		{"server request is newer", map[string]any{"phase": "waiting", "req": req("a", "done", nil)},
			map[string]any{"remote_control": req("b", "failed", map[string]any{"created_at": ago(5)})}, true},
		{"server request is older", map[string]any{"phase": "waiting", "req": req("b", "pending", map[string]any{"created_at": ago(1)})},
			map[string]any{"remote_control": req("a", "done", nil)}, false},
		{"link kept while the server keeps it", map[string]any{"phase": "waiting", "req": req("a", "done", map[string]any{"url": link})},
			map[string]any{"remote_control": req("a", "done", nil), "remote_control_url": link}, false},
		{"link dropped when the session ended", map[string]any{"phase": "waiting", "req": req("a", "done", map[string]any{"url": link})},
			map[string]any{"status": "ended", "controllable": true, "remote_control": req("a", "done", nil)}, true},
		{"failure kept on an ended session", map[string]any{"phase": "waiting", "req": req("a", "failed", nil)},
			map[string]any{"status": "ended", "controllable": true, "remote_control": req("a", "failed", nil)}, false},
		{"error phase kept", map[string]any{"phase": "error", "error": "x"}, map[string]any{}, false},
	}
	untils := []struct {
		req  map[string]any
		want string
	}{
		{map[string]any{"expires_at": "2026-09-30T12:01:00Z"}, "2026-09-30T12:01:00Z"},
		{map[string]any{"expires_at": "2026-09-30T13:00:00Z"}, "2026-09-30T12:02:00Z"},
		{map[string]any{}, "2026-09-30T12:02:00Z"},
	}
	in := map[string]any{"now": now, "outcomes": []any{}, "stales": []any{}, "untils": []any{}}
	for _, o := range outcomes {
		in["outcomes"] = append(in["outcomes"].([]any), []any{o.req, o.url})
	}
	for _, s := range stales {
		in["stales"] = append(in["stales"].([]any), []any{s.st, s.s})
	}
	for _, u := range untils {
		in["untils"] = append(in["untils"].([]any), u.req)
	}
	input, _ := json.Marshal(in)
	script := string(m[1]) + `
var input = JSON.parse(require("fs").readFileSync(0, "utf8"));
var now = Date.parse(input.now);
process.stdout.write(JSON.stringify({
  outcomes: input.outcomes.map(function (r) { return rcServerOutcome(r[0], now, r[1]); }),
  stales: input.stales.map(function (p) { return rcStale(p[0], p[1]); }),
  untils: input.untils.map(function (r) { return new Date(rcPollUntil(r, now)).toISOString().slice(0, 19) + "Z"; })
}));`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got struct {
		Outcomes []*outcome `json:"outcomes"`
		Stales   []bool     `json:"stales"`
		Untils   []string   `json:"untils"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %s: %v", out, err)
	}
	for i, o := range outcomes {
		if !reflect.DeepEqual(got.Outcomes[i], o.want) {
			t.Errorf("rcServerOutcome %s = %+v, want %+v", o.name, got.Outcomes[i], o.want)
		}
	}
	for i, s := range stales {
		if got.Stales[i] != s.want {
			t.Errorf("rcStale %s = %v, want %v", s.name, got.Stales[i], s.want)
		}
	}
	for i, u := range untils {
		if got.Untils[i] != u.want {
			t.Errorf("rcPollUntil(%v) = %s, want %s", u.req, got.Untils[i], u.want)
		}
	}
}

// TestDashboardInsightStates runs the page's insight-state block under node:
// the summary line, link labels, the filter, and the link rule.
func TestDashboardInsightStates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; this table runs the page's insight-state block under node")
	}
	m := regexp.MustCompile(`(?s)// insight-state:begin\n(.*?)// insight-state:end`).FindSubmatch(dashboardHTML)
	if m == nil {
		t.Fatal("dashboard/index.html has no insight-state block")
	}
	type part struct {
		Text string `json:"text"`
		Href string `json:"href,omitempty"`
	}
	const mr = "https://git.example.com/o/r/-/merge_requests/391"
	const pr = "https://github.com/o/r/pull/7"
	summaries := []struct {
		sum  any
		want []part
	}{
		{nil, []part{}},
		{map[string]any{"output_tokens": 371471}, []part{{Text: "371k tokens out"}}},
		{map[string]any{
			"git":         map[string]any{"commit_count": 3, "uncommitted": 2, "unpushed": 1},
			"latest_link": map[string]any{"url": mr, "number": 391},
			"cost_usd":    18.271,
		}, []part{{Text: "3 commits"}, {Text: "2 uncommitted"}, {Text: "1 unpushed"}, {Text: "!391", Href: mr}, {Text: "$18.27"}}},
		{map[string]any{
			"git":           map[string]any{"commit_count": 1, "uncommitted": 0, "unpushed": -1},
			"latest_link":   map[string]any{"url": pr, "number": 7},
			"output_tokens": 1500000,
		}, []part{{Text: "1 commit"}, {Text: "#7", Href: pr}, {Text: "1.5M tokens out"}}},
		{map[string]any{"latest_link": map[string]any{"url": "javascript:alert(1)", "number": 5}}, []part{{Text: "#5"}}},
	}
	recap := map[string]any{"recap": "Next, merge MR 391.", "last_prompt": "why is the [build] red?"}
	matchCases := []struct {
		s    map[string]any
		q    string
		want bool
	}{
		{recap, "mr 391", true},
		{recap, "[build]", true},
		{recap, "nothing", false},
		{recap, "", true},
		{map[string]any{}, "", true},
	}
	links := []string{"https://a.b/c", "http://a.b", "javascript:x", ""}
	wantLinks := []any{"https://a.b/c", nil, nil, nil}

	var sums []any
	for _, s := range summaries {
		sums = append(sums, s.sum)
	}
	var ms []any
	for _, c := range matchCases {
		ms = append(ms, []any{c.s, c.q})
	}
	input, _ := json.Marshal(map[string]any{"sums": sums, "matches": ms, "links": links})
	script := string(m[1]) + `
var input = JSON.parse(require("fs").readFileSync(0, "utf8"));
process.stdout.write(JSON.stringify({
  sums: input.sums.map(summaryParts),
  matches: input.matches.map(function (a) { return matches(a[0], a[1]); }),
  links: input.links.map(safeLink)
}));`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got struct {
		Sums    [][]part `json:"sums"`
		Matches []bool   `json:"matches"`
		Links   []any    `json:"links"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %s: %v", out, err)
	}
	for i, s := range summaries {
		if !reflect.DeepEqual(got.Sums[i], s.want) {
			t.Errorf("summaryParts(%v) = %+v, want %+v", s.sum, got.Sums[i], s.want)
		}
	}
	for i, c := range matchCases {
		if got.Matches[i] != c.want {
			t.Errorf("matches(%v, %q) = %v, want %v", c.s, c.q, got.Matches[i], c.want)
		}
	}
	if !reflect.DeepEqual(got.Links, wantLinks) {
		t.Errorf("safeLink(%q) = %v, want %v", links, got.Links, wantLinks)
	}
}

// TestDashboardAuthStates runs the page's auth-state block under node: the
// signed-out decision, the sign-out request, and its outcome.
func TestDashboardAuthStates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; this table runs the page's auth-state block under node")
	}
	m := regexp.MustCompile(`(?s)// auth-state:begin\n(.*?)// auth-state:end`).FindSubmatch(dashboardHTML)
	if m == nil {
		t.Fatal("dashboard/index.html has no auth-state block")
	}
	type auth struct {
		SignedOut bool   `json:"signedOut"`
		Name      string `json:"name"`
	}
	auths := []struct {
		status int
		name   any
		want   auth
	}{
		{401, nil, auth{true, ""}},
		{401, "phone", auth{true, ""}},
		{200, "phone", auth{false, "phone"}},
		{200, nil, auth{false, ""}}, // a machine token: no name
		{500, "phone", auth{false, "phone"}},
	}
	var in []any
	for _, a := range auths {
		in = append(in, []any{a.status, a.name})
	}
	input, _ := json.Marshal(map[string]any{"auths": in, "done": []int{204, 401, 403, 500}})
	script := string(m[1]) + `
var input = JSON.parse(require("fs").readFileSync(0, "utf8"));
process.stdout.write(JSON.stringify({
  auths: input.auths.map(function (a) { return sessionAuth(a[0], a[1]); }),
  req: signOutRequest(),
  done: input.done.map(signOutDone),
  text: SIGNED_OUT
}));`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got struct {
		Auths []auth `json:"auths"`
		Req   struct {
			URL  string `json:"url"`
			Init struct {
				Method  string            `json:"method"`
				Headers map[string]string `json:"headers"`
				Cache   string            `json:"cache"`
			} `json:"init"`
		} `json:"req"`
		Done []bool `json:"done"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %s: %v", out, err)
	}
	for i, a := range auths {
		if got.Auths[i] != a.want {
			t.Errorf("sessionAuth(%d, %v) = %+v, want %+v", a.status, a.name, got.Auths[i], a.want)
		}
	}
	if got.Req.URL != "/logout" || got.Req.Init.Method != "POST" || got.Req.Init.Cache != "no-store" ||
		!reflect.DeepEqual(got.Req.Init.Headers, map[string]string{"X-Hub-Action": "sign-out"}) {
		t.Errorf("signOutRequest() = %+v", got.Req)
	}
	if !reflect.DeepEqual(got.Done, []bool{true, true, false, false}) {
		t.Errorf("signOutDone(204, 401, 403, 500) = %v", got.Done)
	}
	if !strings.Contains(got.Text, "sessionhub login --name <device>") {
		t.Errorf("SIGNED_OUT = %q", got.Text)
	}
}

// TestDashboardInboxStates runs the page's inbox-state block under node:
// grouping and counts, the page title, the snooze times, and the triage
// request. TZ is fixed so "tomorrow" has one right answer.
func TestDashboardInboxStates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; this table runs the page's inbox-state block under node")
	}
	m := regexp.MustCompile(`(?s)// inbox-state:begin\n(.*?)// inbox-state:end`).FindSubmatch(dashboardHTML)
	if m == nil {
		t.Fatal("dashboard/index.html has no inbox-state block")
	}
	const since = "2026-10-01T12:00:00.123456789Z"
	item := func(id, group, since string) map[string]any {
		return map[string]any{"group": group, "since": since, "session": map[string]any{"id": id}}
	}
	inbox := map[string]any{"items": []any{
		item("a", "finished", "2026-10-01T10:00:00Z"),
		item("b", "blocked", "2026-10-01T11:00:00Z"),
		item("c", "waiting", since),
		item("d", "finished", "2026-10-01T10:30:00Z"),
	}}
	input, _ := json.Marshal(map[string]any{"inbox": inbox})
	script := string(m[1]) + `
var input = JSON.parse(require("fs").readFileSync(0, "utf8"));
function keys(v) {
  return v.groups.map(function (g) { return g.key + ":" + g.items.map(function (it) { return it.session.id; }).join(","); });
}
function iso(d) { return d ? d.toISOString() : null; }
var late = Date.parse("2026-10-01T14:30:00Z");  // 23:30 in Tokyo
var early = Date.parse("2026-10-01T15:30:00Z"); // 00:30 the next day in Tokyo
var hidden = inboxView(input.inbox, { d: "2026-10-01T10:30:00Z", a: "2026-10-01T09:00:00Z" });
process.stdout.write(JSON.stringify({
  all: keys(inboxView(input.inbox, {})),
  allTotal: inboxView(input.inbox, {}).total,
  hidden: keys(hidden),
  hiddenTotal: hidden.total,
  emptyGroups: inboxView({ items: [] }, {}).groups.length,
  pruned: prunePending(input.inbox, { a: "2026-10-01T10:00:00Z", c: "2026-10-01T11:59:59Z", x: "2026-10-01T10:00:00Z" }),
  titles: [pageTitle(0), pageTitle(3)],
  tabs: [firstTab(0), firstTab(2)],
  hash: [hashTab("#inbox"), hashTab("#sessions"), hashTab(""), hashTab("#other"), hashTab("#INBOX")],
  snooze: [iso(snoozeUntil("1h", late)), iso(snoozeUntil("4h", late)), iso(snoozeUntil("tomorrow", late)),
    iso(snoozeUntil("tomorrow", early)), iso(snoozeUntil("2h", late))],
  dismiss: triageRequest("dismiss", "a/b c", "` + since + `", "2026-10-01T13:00:00.000Z"),
  snoozeReq: triageRequest("snooze", "abc", "` + since + `", "2026-10-01T13:00:00.000Z")
}));`
	cmd := exec.Command(node, "-e", script)
	cmd.Env = append(os.Environ(), "TZ=Asia/Tokyo")
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	type request struct {
		URL  string `json:"url"`
		Init struct {
			Method  string            `json:"method"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body"`
			Cache   string            `json:"cache"`
		} `json:"init"`
	}
	var got struct {
		All         []string          `json:"all"`
		AllTotal    int               `json:"allTotal"`
		Hidden      []string          `json:"hidden"`
		HiddenTotal int               `json:"hiddenTotal"`
		EmptyGroups int               `json:"emptyGroups"`
		Pruned      map[string]string `json:"pruned"`
		Titles      []string          `json:"titles"`
		Tabs        []string          `json:"tabs"`
		Hash        []*string         `json:"hash"`
		Snooze      []*string         `json:"snooze"`
		Dismiss     request           `json:"dismiss"`
		SnoozeReq   request           `json:"snoozeReq"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %s: %v", out, err)
	}
	if want := []string{"blocked:b", "waiting:c", "finished:a,d"}; !reflect.DeepEqual(got.All, want) || got.AllTotal != 4 {
		t.Errorf("inboxView = %v total %d, want %v total 4", got.All, got.AllTotal, want)
	}
	// d is pending with its own since: hidden. a is pending with an older
	// since: a newer trigger, still shown.
	if want := []string{"blocked:b", "waiting:c", "finished:a"}; !reflect.DeepEqual(got.Hidden, want) || got.HiddenTotal != 3 {
		t.Errorf("inboxView with pending = %v total %d, want %v total 3", got.Hidden, got.HiddenTotal, want)
	}
	if got.EmptyGroups != 0 {
		t.Errorf("empty inbox has %d groups", got.EmptyGroups)
	}
	if !reflect.DeepEqual(got.Pruned, map[string]string{"a": "2026-10-01T10:00:00Z"}) {
		t.Errorf("prunePending = %v", got.Pruned)
	}
	if !reflect.DeepEqual(got.Titles, []string{"sessionhub", "(3) sessionhub"}) || !reflect.DeepEqual(got.Tabs, []string{"sessions", "inbox"}) {
		t.Errorf("titles %v tabs %v", got.Titles, got.Tabs)
	}
	wantHash := []string{"inbox", "sessions"}
	for i, w := range wantHash {
		if got.Hash[i] == nil || *got.Hash[i] != w {
			t.Errorf("hashTab case %d = %v, want %s", i, got.Hash[i], w)
		}
	}
	for i := 2; i < len(got.Hash); i++ {
		if got.Hash[i] != nil {
			t.Errorf("hashTab case %d = %s, want null", i, *got.Hash[i])
		}
	}
	wantSnooze := []string{"2026-10-01T15:30:00.000Z", "2026-10-01T18:30:00.000Z", "2026-10-02T00:00:00.000Z", "2026-10-03T00:00:00.000Z"}
	for i, w := range wantSnooze {
		if got.Snooze[i] == nil || *got.Snooze[i] != w {
			t.Errorf("snoozeUntil case %d = %v, want %s", i, got.Snooze[i], w)
		}
	}
	if got.Snooze[4] != nil {
		t.Errorf("snoozeUntil(\"2h\") = %s, want null", *got.Snooze[4])
	}
	hdr := map[string]string{"Content-Type": "application/json", "X-Hub-Action": "triage"}
	d := got.Dismiss
	if d.URL != "/v1/inbox/a%2Fb%20c/dismiss" || d.Init.Method != "POST" || d.Init.Cache != "no-store" ||
		!reflect.DeepEqual(d.Init.Headers, hdr) || d.Init.Body != `{"since":"`+since+`"}` {
		t.Errorf("triageRequest(dismiss) = %+v", d)
	}
	s := got.SnoozeReq
	if s.URL != "/v1/inbox/abc/snooze" || !reflect.DeepEqual(s.Init.Headers, hdr) ||
		s.Init.Body != `{"since":"`+since+`","until":"2026-10-01T13:00:00.000Z"}` {
		t.Errorf("triageRequest(snooze) = %+v", s)
	}
}

// TestDashboardMoveStates runs the page's move-state block under node: who
// may move, the targets, the confirmation, the request, and the card line.
func TestDashboardMoveStates(t *testing.T) {
	script := `
var idle = { id: "3f2a9c10-1111", title: "auth work", machine: "bluebox", status: "live", controllable: true,
  agent_state: "idle", git_branch: "main", git_repo: "git@github.com:o/r.git" };
var machines = [{ name: "bluebox", move_ready: true, move_key: "aaaa" }, { name: "tower", move_ready: true, move_key: "bbbb" },
  { name: "nas", move_ready: false, move_key: "cccc" }, { name: "pi", move_ready: false }];
function withMove(state, extra) { return Object.assign({ id: "mv_1", source: "bluebox", target: "tower", state: state }, extra || {}); }
process.stdout.write(JSON.stringify({
  github: ["git@github.com:o/r.git", "https://GitHub.com/o/r", "ssh://git@github.com/o/r.git", "git@gitlab.com:o/r.git",
    "https://github.com.evil/o/r", "ssh://git@github.com:22/o/r.git", "https://github.com:443/o/r", "https://github.com:x/o/r", "/srv/github.com/o/r", "", null].map(isGitHub),
  blocked: [moveBlocked(idle), moveBlocked(Object.assign({}, idle, { agent_state: "working" })),
    moveBlocked(Object.assign({}, idle, { agent_state: "done" })),
    moveBlocked(Object.assign({}, idle, { controllable: false })), moveBlocked(Object.assign({}, idle, { status: "ended" })),
    moveBlocked(Object.assign({}, idle, { move: withMove("packing") })), moveBlocked(Object.assign({}, idle, { move: withMove("failed") }))],
  targets: moveTargets(idle, machines),
  noCloud: moveTargets(Object.assign({}, idle, { git_repo: "git@gitlab.com:o/r.git" }), machines.slice(0, 2)),
  confirm: [moveConfirm(idle, { target: "tower", label: "tower" }), moveConfirm(idle, { target: "cloud", label: "Cloud" }),
    moveConfirm(Object.assign({}, idle, { title: "a\u202eb\nc" }), { target: "tower", label: "tower" })],
  req: moveRequest("3f2a/x", "tower"),
  views: [moveView(null), moveView(withMove("requested")), moveView(withMove("packing")), moveView(withMove("uploaded")),
    moveView(withMove("unpacking")), moveView(withMove("done")), moveView(withMove("done", { detail: "not carried: .env" })),
    moveView(withMove("done", { target: "cloud", cloud_url: "https://claude.ai/code/session_01Ab" })),
    moveView(withMove("done", { target: "cloud", cloud_url: "javascript:alert(1)" })),
    moveView(withMove("failed", { detail: "find clone: no clone of github.com/o/r on tower" })),
    moveView(withMove("cancelled"))]
}));`
	out := runBlocks(t, []string{"move-state"}, script, map[string]any{})
	want := `{
  "github": [true, true, true, false, false, true, true, false, false, false, false],
  "blocked": ["", "Wait until the agent is idle or done.", "", "Not in herdr, or its machine's watcher is offline.",
    "The session has ended.", "A move is under way.", ""],
  "targets": [{"target": "tower", "label": "tower", "ok": true, "why": ""},
    {"target": "nas", "label": "nas", "ok": false, "why": "its watcher is offline"},
    {"target": "pi", "label": "pi", "ok": false, "why": "no move key yet"},
    {"target": "cloud", "label": "Cloud", "ok": true, "why": ""}],
  "noCloud": [{"target": "tower", "label": "tower", "ok": true, "why": ""}],
  "confirm": ["Move \"auth work\" to tower? Its conversation, branch main, and uncommitted changes move; the session ends here and resumes on tower.",
    "Hand \"auth work\" to a new Claude Code cloud session? Uncommitted work goes on a sessionhub branch on GitHub, and the session ends here.",
    "Move \"a\ufffdb\ufffdc\" to tower? Its conversation, branch main, and uncommitted changes move; the session ends here and resumes on tower."],
  "req": {"url": "/v1/sessions/3f2a%2Fx/move", "init": {"method": "POST",
    "headers": {"Content-Type": "application/json", "X-Hub-Action": "move"}, "body": "{\"target\":\"tower\"}", "cache": "no-store"}},
  "views": [null,
    {"kind": "note", "text": "Moving to tower: waiting for bluebox\u2026", "url": ""},
    {"kind": "note", "text": "Moving to tower: packing on bluebox\u2026", "url": ""},
    {"kind": "note", "text": "Moving to tower: waiting for tower\u2026", "url": ""},
    {"kind": "note", "text": "Moving to tower: unpacking on tower\u2026", "url": ""},
    {"kind": "note", "text": "Moved to tower.", "url": ""},
    {"kind": "note", "text": "Moved to tower. not carried: .env", "url": ""},
    {"kind": "link", "text": "Moved to the cloud.", "url": "https://claude.ai/code/session_01Ab"},
    {"kind": "note", "text": "Moved to the cloud.", "url": ""},
    {"kind": "error", "text": "Move to tower failed: find clone: no clone of github.com/o/r on tower", "url": ""},
    {"kind": "error", "text": "Move to tower failed: cancelled", "url": ""}]
}`
	var got, exp map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %s: %v", out, err)
	}
	if err := json.Unmarshal([]byte(want), &exp); err != nil {
		t.Fatal(err)
	}
	for k, w := range exp {
		if !reflect.DeepEqual(got[k], w) {
			gb, _ := json.Marshal(got[k])
			wb, _ := json.Marshal(w)
			t.Errorf("%s:\n got %s\nwant %s", k, gb, wb)
		}
	}
	page := string(dashboardHTML)
	for _, want := range []string{"if (mr) c.appendChild(mr);", "if (!window.confirm(moveConfirm(s, t))) return;",
		`fetch("/v1/moves/" + encodeURIComponent(id)`, `fetch("/v1/machines"`, "if (menu.open && !was) loadMachines(s.id);",
		"var why = moveBlocked(s);", "b.disabled = !t.ok || busy;", ".move { display: flex;"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// TestDashboardStartStates runs the page's start-state block under node:
// the Machine choices, the Directory suggestions, the form checks, the
// write, and the status line.
func TestDashboardStartStates(t *testing.T) {
	script := `
var machines = [{ name: "bluebox", online: false }, { name: "tower", online: true }, { name: "nas" }, null, { online: true }];
var sessions = [
  { machine: "tower", cwd: "/home/me/Code/a", last_seen_at: "2026-10-04T10:00:00Z" },
  { machine: "tower", cwd: "/home/me/Code/b", last_seen_at: "2026-10-04T12:00:00Z" },
  { machine: "tower", cwd: "/home/me/Code/a", last_seen_at: "2026-10-04T13:00:00Z" },
  { machine: "tower", cwd: "" },
  { machine: "bluebox", cwd: "/srv/x", last_seen_at: "2026-10-04T14:00:00Z" }];
function req(state, extra) { return Object.assign({ id: "st_1", machine: "tower", state: state }, extra || {}); }
process.stdout.write(JSON.stringify({
  machines: startMachines(machines),
  dirs: startDirsFor(sessions, "tower"),
  none: startDirsFor(sessions, "pi"),
  problems: [startProblem("", "~", ""), startProblem("tower", "  ", ""), startProblem("tower", "Code/x", ""),
    startProblem("tower", "~bob", ""), startProblem("tower", "~", ""), startProblem("tower", " ~/Code/x ", ""),
    startProblem("tower", "/srv", "x".repeat(4001)), startProblem("tower", "/srv", "x".repeat(4000)),
    startProblem("tower", "~", "", true), startProblem("tower", " ~/ ", "", true), startProblem("tower", "~/Code/x", "", true)],
  req: startRequest("tower/x", " ~/Code/app ", "fix it"),
  reqNoPrompt: startRequest("tower", "/srv", "   "),
  reqTrust: startRequest("tower", "~/Code/new", "", true),
  open: [startOpen(req("pending")), startOpen(req("claimed")), startOpen(req("done")), startOpen(null)],
  views: [startView(null), startView(req("pending")), startView(req("claimed")),
    startView(req("done", { url: "https://claude.ai/code/session_01Ab", detail: "started in a new herdr workspace" })),
    startView(req("done", { url: "https://claude.ai/code/session_01Ab", detail: "started in a new herdr workspace; the first prompt was not sent: x" })),
    startView(req("done", { url: "javascript:alert(1)" })),
    startView(req("failed", { detail: "the directory doesn't exist on this machine" })),
    startView(req("failed", { detail: "/home/me/new is not trusted by Claude on tower; open it once or pass --trust" })),
    startView(req("failed")), startView(req("expired"))]
}));`
	out := runBlocks(t, []string{"start-state"}, script, map[string]any{})
	want := `{
  "machines": [{"name": "tower", "ok": true, "why": ""}, {"name": "bluebox", "ok": false, "why": "watcher offline"},
    {"name": "nas", "ok": false, "why": "watcher offline"}],
  "dirs": ["/home/me/Code/a", "/home/me/Code/b"],
  "none": [],
  "problems": ["Pick a machine.", "Enter a directory.", "Use an absolute directory, or one starting with ~/.",
    "Use an absolute directory, or one starting with ~/.", "", "", "The first prompt is over 4,000 characters.", "",
    "Trusting the home directory would trust every folder under it; pick a folder inside it.",
    "Trusting the home directory would trust every folder under it; pick a folder inside it.", ""],
  "req": {"url": "/v1/machines/tower%2Fx/start", "init": {"method": "POST",
    "headers": {"Content-Type": "application/json", "X-Hub-Action": "start"},
    "body": "{\"dir\":\"~/Code/app\",\"prompt\":\"fix it\"}", "cache": "no-store"}},
  "reqNoPrompt": {"url": "/v1/machines/tower/start", "init": {"method": "POST",
    "headers": {"Content-Type": "application/json", "X-Hub-Action": "start"},
    "body": "{\"dir\":\"/srv\"}", "cache": "no-store"}},
  "reqTrust": {"url": "/v1/machines/tower/start", "init": {"method": "POST",
    "headers": {"Content-Type": "application/json", "X-Hub-Action": "start"},
    "body": "{\"dir\":\"~/Code/new\",\"trust\":true}", "cache": "no-store"}},
  "open": [true, true, false, false],
  "views": [null,
    {"kind": "note", "text": "Waiting for the watcher on tower\u2026", "url": ""},
    {"kind": "note", "text": "Starting Claude on tower\u2026", "url": ""},
    {"kind": "link", "text": "Started on tower.", "url": "https://claude.ai/code/session_01Ab"},
    {"kind": "link", "text": "Started on tower. started in a new herdr workspace; the first prompt was not sent: x.", "url": "https://claude.ai/code/session_01Ab"},
    {"kind": "note", "text": "Started on tower, but the link wasn't seen; the session shows here once it reports.", "url": ""},
    {"kind": "error", "text": "Start on tower failed: the directory doesn't exist on this machine", "url": ""},
    {"kind": "error", "text": "Start on tower failed: /home/me/new is not trusted by Claude on tower; open it once or pass --trust. Tick Trust this folder to start there anyway.", "url": ""},
    {"kind": "error", "text": "Start on tower failed: failed", "url": ""},
    {"kind": "error", "text": "The watcher on tower didn't finish in time.", "url": ""}]
}`
	var got, exp map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %s: %v", out, err)
	}
	if err := json.Unmarshal([]byte(want), &exp); err != nil {
		t.Fatal(err)
	}
	for k, w := range exp {
		if !reflect.DeepEqual(got[k], w) {
			gb, _ := json.Marshal(got[k])
			wb, _ := json.Marshal(w)
			t.Errorf("%s:\n got %s\nwant %s", k, gb, wb)
		}
	}
	page := string(dashboardHTML)
	for _, want := range []string{`<button id="new" class="textbtn" type="button" aria-expanded="false" aria-controls="startbar" hidden>New session</button>`,
		`fetch("/v1/starts/" + encodeURIComponent(id)`, "var why = startProblem(startMachine.value, startDir.value, startPrompt.value, startTrust.checked);",
		`<input id="start-trust" type="checkbox">`,
		"o.disabled = !c.ok;", "newBtn.hidden = tab === \"inbox\" || tab === \"rules\" || signedOut;", ".startbar[hidden] { display: none; }"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
