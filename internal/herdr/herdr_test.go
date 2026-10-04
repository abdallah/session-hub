package herdr_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
)

func dial(t *testing.T, s *herdrtest.Server) *herdr.Client {
	t.Helper()
	c, err := herdr.Dial(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestSnapshot(t *testing.T) {
	s := herdrtest.New(t, "session-snapshot.ndjson")
	snap, err := dial(t, s).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Version != "0.9.3" || snap.Protocol != 22 || snap.FocusedPaneID != "w4:p1" || snap.FocusedWorkspaceID != "w4" {
		t.Errorf("header: %+v", snap)
	}
	if len(snap.Panes) != 5 || len(snap.Agents) == 0 {
		t.Fatalf("panes %d agents %d", len(snap.Panes), len(snap.Agents))
	}
	byID := map[string]herdr.PaneInfo{}
	for _, p := range snap.Panes {
		byID[p.PaneID] = p
	}
	// A pane with a real agent_session.
	p := byID["w7:p1"]
	if p.SessionID() != "11111111-2222-4333-8444-555555555555" || p.AgentSession.Kind != "id" || p.AgentSession.Source != "herdr:claude" {
		t.Errorf("w7:p1 session: %+v", p.AgentSession)
	}
	if p.Agent != "claude" || p.AgentStatus != "idle" || p.TitleClean != "session title" || p.CWD != "/home/user/project" {
		t.Errorf("w7:p1: %+v", p)
	}
	// Restored pane: agent_session but no agent field.
	if q := byID["w8:p1"]; q.Agent != "" || q.SessionID() != "66666666-7777-4888-8999-aaaaaaaaaaaa" {
		t.Errorf("w8:p1: %+v", q)
	}
	// Pane with no session and a label.
	if q := byID["w4:p1"]; q.SessionID() != "" || q.Label != "agent-label" || !q.Focused {
		t.Errorf("w4:p1: %+v", q)
	}
	// Non-agent shell pane.
	if q := byID["w4:p5"]; q.Agent != "" || q.AgentStatus != "unknown" {
		t.Errorf("w4:p5: %+v", q)
	}
	var found bool
	for _, a := range snap.Agents {
		if a.PaneID == "w7:p1" {
			found = a.AgentSession != nil && a.AgentSession.Value == "11111111-2222-4333-8444-555555555555"
		}
	}
	if !found {
		t.Error("agents list lost w7:p1 session")
	}
	reqs := s.Requests()
	if len(reqs) != 1 || reqs[0].Method != "session.snapshot" || reqs[0].ID == "" {
		t.Errorf("requests: %+v", reqs)
	}
}

func TestPaneGetExistingAndMissing(t *testing.T) {
	s := herdrtest.New(t, "pane-get-existing.ndjson", "pane-get-missing.ndjson", "pane-focus-missing.ndjson")
	c := dial(t, s)
	p, err := c.PaneGet("w9:p1")
	if err != nil {
		t.Fatal(err)
	}
	if p.PaneID != "w9:p1" || p.Agent != "claude" || p.AgentStatus != "blocked" || p.WorkspaceID != "w9" || p.CWD != "/tmp/sessionhub-probe-work" {
		t.Errorf("pane: %+v", p)
	}
	if p.SessionID() != "" {
		t.Errorf("probe pane has no session, got %q", p.SessionID())
	}

	_, err = c.PaneGet("w99:p99")
	var he *herdr.Error
	if !errors.As(err, &he) || he.Code != "pane_not_found" {
		t.Fatalf("missing pane err = %v", err)
	}
	// The connection stays usable after an error response.
	if _, err := c.PaneGet("w9:p1"); err != nil {
		t.Errorf("second call: %v", err)
	}
	err = c.PaneFocus("w99:p99")
	if !errors.As(err, &he) || he.Code != "pane_not_found" {
		t.Errorf("focus missing err = %v", err)
	}
	// Request IDs are unique.
	seen := map[string]bool{}
	for _, r := range s.Requests() {
		if seen[r.ID] {
			t.Errorf("duplicate request id %s", r.ID)
		}
		seen[r.ID] = true
	}
	// A request with no fixture surfaces as an error, not a hang.
	if _, err := c.PaneGet("w1:p1"); err == nil {
		t.Error("want error for unfixtured pane")
	}
}

func TestReportMetadataAndSummary(t *testing.T) {
	s := herdrtest.New(t, "pane-report-metadata-set.ndjson", "pane-report-metadata-clear.ndjson", "pane-get-with-metadata.ndjson")
	c := dial(t, s)
	if err := c.SetSummary("w9:p1", "probe summary"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetSummary("w9:p1", ""); err != nil {
		t.Fatal(err)
	}
	// The exact request bodies match what was captured and accepted.
	reqs := s.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests: %+v", reqs)
	}
	var set, clr struct {
		PaneID string             `json:"pane_id"`
		Source string             `json:"source"`
		Tokens map[string]*string `json:"tokens"`
	}
	json.Unmarshal(reqs[0].Params, &set)
	json.Unmarshal(reqs[1].Params, &clr)
	if set.Source != "sessionhub" || set.Tokens["hub_summary"] == nil || *set.Tokens["hub_summary"] != "probe summary" {
		t.Errorf("set: %s", reqs[0].Params)
	}
	if v, ok := clr.Tokens["hub_summary"]; !ok || v != nil {
		t.Errorf("clear must send hub_summary:null, got %s", reqs[1].Params)
	}
	p, err := c.PaneGet("w9:p1")
	if err != nil || p.Tokens["hub_summary"] != "probe summary" {
		t.Errorf("tokens: %+v %v", p.Tokens, err)
	}
}

func TestSummaryTruncatedTo80Runes(t *testing.T) {
	s := herdrtest.New(t, "pane-report-metadata-set.ndjson")
	c := dial(t, s)
	long := ""
	for i := 0; i < 100; i++ {
		long += "é"
	}
	c.SetSummary("w9:p1", long) // no fixture for this body: error is fine, the request is what we check
	var got struct {
		Tokens map[string]string `json:"tokens"`
	}
	json.Unmarshal(s.Requests()[0].Params, &got)
	if n := len([]rune(got.Tokens["hub_summary"])); n != 80 {
		t.Errorf("summary length = %d runes, want 80", n)
	}
}

func TestPaneSplitCaptured(t *testing.T) {
	s := herdrtest.New(t, "pane-split.ndjson")
	p, err := dial(t, s).PaneSplit(herdr.PaneSplitParams{Direction: "right", CWD: "/tmp/sessionhub-probe-work", TargetPaneID: "wA:p1"})
	if err != nil {
		t.Fatal(err)
	}
	if p.PaneID != "wA:p2" || p.WorkspaceID != "wA" || p.CWD != "/tmp/sessionhub-probe-work" {
		t.Errorf("pane: %+v", p)
	}
}

func TestPaneCallsErrorWithoutPane(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// One Read can return a partial line; read up to the newline.
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var req struct{ ID string }
			json.Unmarshal(line, &req)
			c.Write([]byte(`{"id":"` + req.ID + `","result":{"type":"ok"}}` + "\n"))
			c.Close()
		}
	}()
	c, _ := herdr.Dial(path)
	if p, err := c.PaneSplit(herdr.PaneSplitParams{Direction: "right"}); err == nil {
		t.Errorf("PaneSplit without pane returned %+v, nil", p)
	}
	if p, err := c.PaneGet("w1:p1"); err == nil {
		t.Errorf("PaneGet without pane returned %+v, nil", p)
	}
}

func TestParamsForSplitAndAgentStart(t *testing.T) {
	s := herdrtest.New(t) // no fixture for these: errors expected, requests recorded
	c := dial(t, s)
	c.PaneSplit(herdr.PaneSplitParams{Direction: "right", CWD: "/home/user/project", TargetPaneID: "w4:p1", Focus: true})
	c.AgentStart(herdr.AgentStartParams{Name: "resume", Kind: "claude", PaneID: "w4:p9", Args: []string{"--resume", "abc"}})
	reqs := s.Requests()
	if len(reqs) != 2 || reqs[0].Method != "pane.split" || reqs[1].Method != "agent.start" {
		t.Fatalf("requests: %+v", reqs)
	}
	var sp map[string]any
	json.Unmarshal(reqs[0].Params, &sp)
	if sp["direction"] != "right" || sp["cwd"] != "/home/user/project" || sp["target_pane_id"] != "w4:p1" || sp["focus"] != true {
		t.Errorf("split params: %s", reqs[0].Params)
	}
	if _, has := sp["workspace_id"]; has {
		t.Errorf("empty workspace_id must be omitted: %s", reqs[0].Params)
	}
	var as map[string]any
	json.Unmarshal(reqs[1].Params, &as)
	args, _ := as["args"].([]any)
	if as["name"] != "resume" || as["kind"] != "claude" || as["pane_id"] != "w4:p9" || len(args) != 2 || args[0] != "--resume" {
		t.Errorf("agent.start params: %s", reqs[1].Params)
	}
	if _, has := as["timeout_ms"]; has {
		t.Errorf("zero timeout must be omitted: %s", reqs[1].Params)
	}
}

func TestDeadline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		var held []net.Conn // keep them referenced so the GC does not close them
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c) // accept and stay silent
		}
	}()
	c, err := herdr.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	if _, err := c.Snapshot(); err == nil {
		t.Fatal("want deadline error")
	}
	if d := time.Since(start); d < 1500*time.Millisecond || d > 3*time.Second {
		t.Errorf("gave up after %v, want about 2s", d)
	}
}

func TestDialMissingSocket(t *testing.T) {
	if _, err := herdr.Dial(filepath.Join(t.TempDir(), "nope.sock")); err == nil {
		t.Fatal("want error")
	}
}

func TestSocketPathAndSessionName(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "")
	home, _ := os.UserHomeDir()
	if got := herdr.SocketPath(); got != filepath.Join(home, ".config", "herdr", "herdr.sock") {
		t.Errorf("default = %s", got)
	}
	t.Setenv("HERDR_SOCKET_PATH", "/x/sessions/work/herdr.sock")
	if got := herdr.SocketPath(); got != "/x/sessions/work/herdr.sock" {
		t.Errorf("env = %s", got)
	}
	for path, want := range map[string]string{
		"/home/u/.config/herdr/herdr.sock":               "default",
		"/home/u/.config/herdr/sessions/work/herdr.sock": "work",
		"/home/u/.config/herdr/sessions/a b/herdr.sock":  "a b",
		"/run/user/1000/other.sock":                      "default",
		"/home/u/.config/herdr/sessions/work/other.sock": "default",
	} {
		if got := herdr.SessionName(path); got != want {
			t.Errorf("SessionName(%s) = %q, want %q", path, got, want)
		}
	}
}

func readFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "herdr", "events", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseEvents(t *testing.T) {
	// pane_created
	e, err := herdr.ParseEvent(readFile(t, "pane.created-split.json"))
	if err != nil || e.Event != herdr.EventPaneCreated {
		t.Fatalf("%+v %v", e, err)
	}
	p, err := e.PaneCreated()
	if err != nil || p.PaneID != "w9:p2" || p.WorkspaceID != "w9" || p.AgentStatus != "unknown" {
		t.Errorf("created: %+v %v", p, err)
	}
	// pane_closed
	e, _ = herdr.ParseEvent(readFile(t, "pane.closed-split.json"))
	r, err := e.PaneClosed()
	if err != nil || r.PaneID != "w9:p2" || r.WorkspaceID != "w9" {
		t.Errorf("closed: %+v %v", r, err)
	}
	// pane_agent_detected
	e, _ = herdr.ParseEvent(readFile(t, "pane.agent_detected.json"))
	d, err := e.AgentDetected()
	if err != nil || d.PaneID != "w9:p1" || d.Agent != "claude" || d.Released {
		t.Errorf("detected: %+v %v", d, err)
	}
	// pane_agent_status_changed
	e, _ = herdr.ParseEvent(readFile(t, "pane.agent_status_changed-blocked.json"))
	a, err := e.AgentStatusChanged()
	if err != nil || a.PaneID != "w9:p1" || a.AgentStatus != "blocked" || a.Agent != "claude" {
		t.Errorf("status: %+v %v", a, err)
	}
	// Wrong accessor for the kind is an error, not a zero value.
	if _, err := e.PaneClosed(); err == nil {
		t.Error("PaneClosed on a status event must fail")
	}
	// pane_exited: no capture exists (testdata/README.md); shape from the schema.
	e, err = herdr.ParseEvent([]byte(`{"event":"pane.exited","data":{"type":"pane_exited","pane_id":"w1:p1","workspace_id":"w1"}}`))
	if err != nil || e.Event != herdr.EventPaneExited {
		t.Fatalf("exited: %+v %v", e, err)
	}
	if r, err := e.PaneExited(); err != nil || r.PaneID != "w1:p1" {
		t.Errorf("exited: %+v %v", r, err)
	}
}

func TestParseEventErrors(t *testing.T) {
	for _, in := range []string{``, `not json`, `{}`, `{"data":{}}`} {
		if _, err := herdr.ParseEvent([]byte(in)); err == nil {
			t.Errorf("ParseEvent(%q) = nil error", in)
		}
	}
}

func TestEventFromEnv(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", "")
	if _, err := herdr.EventFromEnv(); err == nil {
		t.Error("unset env must fail")
	}
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", string(readFile(t, "pane.closed-agent-pane.json")))
	e, err := herdr.EventFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if r, err := e.PaneClosed(); err != nil || r.PaneID != "w9:p1" {
		t.Errorf("%+v %v", r, err)
	}
}

func TestAgentPromptAgainstCapturedFixtures(t *testing.T) {
	s := herdrtest.New(t, "agent-prompt-ok.ndjson", "agent-prompt-blocked.ndjson")
	c := dial(t, s)
	if err := c.AgentPrompt("wS:p1", "/remote-control"); err != nil {
		t.Fatalf("AgentPrompt ok: %v", err)
	}
	err := c.AgentPrompt("wS:p2", "/remote-control")
	var blocked *herdr.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("blocked: err = %v, want *herdr.BlockedError", err)
	}
	var he *herdr.Error
	if !errors.As(err, &he) || he.Code != "agent_blocked" {
		t.Errorf("blocked error should unwrap to *herdr.Error agent_blocked: %v", err)
	}
	// Any other error stays a plain *herdr.Error.
	err = c.AgentPrompt("w9:p9", "x")
	if errors.As(err, &blocked) || !errors.As(err, &he) || he.Code != "no_fixture" {
		t.Errorf("other error: %v", err)
	}
	reqs := s.Requests()
	if len(reqs) != 3 || reqs[0].Method != "agent.prompt" || string(reqs[0].Params) != `{"target":"wS:p1","text":"/remote-control"}` {
		t.Errorf("requests: %+v", reqs)
	}
}

func TestSendKeysAgainstCapturedFixture(t *testing.T) {
	s := herdrtest.New(t, "pane-send-keys-esc.ndjson")
	c := dial(t, s)
	if err := c.SendKeys("wV:p1", "esc"); err != nil {
		t.Fatal(err)
	}
	reqs := s.Requests()
	var p map[string]any
	if len(reqs) != 1 || reqs[0].Method != "pane.send_keys" || json.Unmarshal(reqs[0].Params, &p) != nil ||
		p["pane_id"] != "wV:p1" || len(p) != 2 || len(p["keys"].([]any)) != 1 || p["keys"].([]any)[0] != "esc" {
		t.Errorf("requests: %+v", reqs)
	}
	if err := c.SendKeys("w9:p9", "esc"); err == nil {
		t.Error("unknown fixture: want an error")
	}
}

func TestPaneReadVisibleFixtures(t *testing.T) {
	for name, want := range map[string]string{
		"pane-read-visible-prompt.ndjson": "Claude Code v2.1.285",
		"pane-read-visible-dialog.ndjson": "Disconnect this session",
	} {
		s := herdrtest.New(t, name)
		got, err := dial(t, s).PaneRead("wW:p1", herdr.SourceVisible, 0)
		if err != nil || !strings.Contains(got, want) {
			t.Errorf("%s: %q, %v", name, got, err)
		}
		reqs := s.Requests()
		var p map[string]any
		if len(reqs) != 1 || json.Unmarshal(reqs[0].Params, &p) != nil || p["source"] != "visible" || len(p) != 2 {
			t.Errorf("%s: requests %+v (lines must be omitted)", name, reqs)
		}
	}
}

func TestPaneReadAgainstCapturedFixtures(t *testing.T) {
	// The two fixtures share one request, so each gets its own server.
	for name, wantURL := range map[string]bool{
		"pane-read-before-remote-control.ndjson": false,
		"pane-read-after-remote-control.ndjson":  true,
	} {
		s := herdrtest.New(t, name)
		c := dial(t, s)
		got, err := c.PaneRead("wS:p1", herdr.SourceRecentUnwrapped, 120)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "Claude Code v2.1.285") || strings.Contains(got, "claude.ai/code/session_") == !wantURL {
			t.Errorf("%s: text = %q", name, got)
		}
		reqs := s.Requests()
		var p map[string]any
		if len(reqs) != 1 || reqs[0].Method != "pane.read" || json.Unmarshal(reqs[0].Params, &p) != nil ||
			p["pane_id"] != "wS:p1" || p["source"] != "recent_unwrapped" || p["lines"] != float64(120) {
			t.Errorf("%s: requests: %+v", name, reqs)
		}
	}
	s := herdrtest.New(t, "pane-read-before-remote-control.ndjson")
	c := dial(t, s)
	if _, err := c.PaneRead("w9:p9", herdr.SourceRecentUnwrapped, 120); err == nil {
		t.Error("PaneRead of an unknown fixture: want an error")
	}
}

func TestWorkspaceCreateCaptured(t *testing.T) {
	s := herdrtest.New(t, "workspace-create.ndjson")
	ws, err := dial(t, s).WorkspaceCreate(herdr.WorkspaceCreateParams{CWD: "/tmp/sessionhub-rc-fixture", Label: "sessionhub: fixture capture"})
	if err != nil {
		t.Fatal(err)
	}
	if ws.WorkspaceID == "" || !strings.HasPrefix(ws.RootPane.PaneID, ws.WorkspaceID+":") {
		t.Errorf("workspace %+v", ws)
	}
	reqs := s.Requests()
	var p map[string]any
	if len(reqs) != 1 || reqs[0].Method != "workspace.create" || json.Unmarshal(reqs[0].Params, &p) != nil ||
		p["cwd"] != "/tmp/sessionhub-rc-fixture" || p["label"] != "sessionhub: fixture capture" || p["focus"] != false || len(p) != 3 {
		t.Errorf("requests: %+v", reqs)
	}
	// Any other params have no fixture: an error, never a workspace.
	if _, err := dial(t, s).WorkspaceCreate(herdr.WorkspaceCreateParams{CWD: "/elsewhere"}); err == nil {
		t.Error("unknown params: want an error")
	}
}

// A workspace.create result without a workspace ID or root pane is an error,
// so no caller starts an agent in pane "". These results are synthetic: the
// captured fixture always has both.
func TestWorkspaceCreateNeedsWorkspaceAndRootPane(t *testing.T) {
	for name, result := range map[string]string{
		"no workspace":         `{"type":"workspace_created","root_pane":{"pane_id":"w9:p1"}}`,
		"empty workspace ID":   `{"type":"workspace_created","workspace":{"workspace_id":""},"root_pane":{"pane_id":"w9:p1"}}`,
		"no root pane":         `{"type":"workspace_created","workspace":{"workspace_id":"w9"}}`,
		"empty root pane ID":   `{"type":"workspace_created","workspace":{"workspace_id":"w9"},"root_pane":{"pane_id":""}}`,
		"not an object":        `"ok"`,
		"empty result":         `{}`,
		"root pane not object": `{"workspace":{"workspace_id":"w9"},"root_pane":"w9:p1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.sock")
			ln, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					line, _ := bufio.NewReader(c).ReadBytes('\n')
					var req struct{ ID string }
					json.Unmarshal(line, &req)
					c.Write([]byte(`{"id":"` + req.ID + `","result":` + result + "}\n"))
					c.Close()
				}
			}()
			c, err := herdr.Dial(path)
			if err != nil {
				t.Fatal(err)
			}
			if ws, err := c.WorkspaceCreate(herdr.WorkspaceCreateParams{CWD: "/tmp"}); err == nil {
				t.Errorf("WorkspaceCreate returned %+v, nil", ws)
			}
		})
	}
}

func TestNotificationShow(t *testing.T) {
	srv := herdrtest.New(t, "pane-get-existing.ndjson")
	reason := "shown"
	srv.Handle("notification.show", func(json.RawMessage) (any, string) {
		return map[string]any{"type": "notification_show", "shown": reason == "shown", "reason": reason}, ""
	})
	c, err := herdr.Dial(srv.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.NotificationShow(herdr.NotificationParams{Title: "Blocked: x", Body: "tower", Sound: "request"}); err != nil {
		t.Fatal(err)
	}
	reqs := srv.RequestsFor("notification.show")
	if len(reqs) != 1 || string(reqs[0].Params) != `{"title":"Blocked: x","body":"tower","sound":"request"}` {
		t.Errorf("requests %+v", reqs)
	}
	if n := len(srv.RequestsFor("pane.get")); n != 0 {
		t.Errorf("RequestsFor(pane.get) = %d, want 0", n)
	}
	// Nothing to show because the user turned notifications off or nobody is
	// watching is not an error.
	for _, r := range []string{"disabled", "no_foreground_client"} {
		reason = r
		if err := c.NotificationShow(herdr.NotificationParams{Title: "t"}); err != nil {
			t.Errorf("reason %s: %v, want no error", r, err)
		}
	}
	for _, r := range []string{"rate_limited", "busy"} {
		reason = r
		if err := c.NotificationShow(herdr.NotificationParams{Title: "t"}); err == nil || !strings.Contains(err.Error(), r) {
			t.Errorf("reason %s: %v, want an error with the reason", r, err)
		}
	}
}

func TestReportWorkspaceMetadata(t *testing.T) {
	srv := herdrtest.New(t, "session-snapshot.ndjson")
	srv.Handle("workspace.report_metadata", func(json.RawMessage) (any, string) {
		return map[string]any{"type": "ok"}, ""
	})
	c := dial(t, srv)
	snap, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Workspaces) == 0 || snap.Workspaces[0].WorkspaceID != "w4" || snap.Workspaces[0].Label != "workspace-a" {
		t.Fatalf("snapshot workspaces %+v", snap.Workspaces)
	}
	text := "3 · 1 blocked"
	if err := c.ReportWorkspaceMetadata(herdr.WorkspaceMetadataParams{WorkspaceID: "w4", Source: "sessionhub",
		Tokens: map[string]*string{"inbox": &text}}); err != nil {
		t.Fatal(err)
	}
	if err := c.ReportWorkspaceMetadata(herdr.WorkspaceMetadataParams{WorkspaceID: "w4", Source: "sessionhub",
		Tokens: map[string]*string{"inbox": nil}}); err != nil {
		t.Fatal(err)
	}
	reqs := srv.RequestsFor("workspace.report_metadata")
	want := []string{
		`{"workspace_id":"w4","source":"sessionhub","tokens":{"inbox":"3 · 1 blocked"}}`,
		`{"workspace_id":"w4","source":"sessionhub","tokens":{"inbox":null}}`,
	}
	if len(reqs) != len(want) {
		t.Fatalf("%d requests, want %d", len(reqs), len(want))
	}
	for i := range want {
		if string(reqs[i].Params) != want[i] {
			t.Errorf("request %d params %s, want %s", i, reqs[i].Params, want[i])
		}
	}
}

func TestTabs(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("tab.list", func(json.RawMessage) (any, string) {
		return map[string]any{"type": "tab_list", "tabs": []any{
			map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "number": 1, "label": "1", "focused": true, "pane_count": 2, "agent_status": "idle"},
			map[string]any{"tab_id": "w2:t3", "workspace_id": "w2", "number": 3, "label": "sessionhub:bluebox", "focused": false, "pane_count": 1, "agent_status": "unknown"},
		}}, ""
	})
	srv.Handle("tab.focus", func(json.RawMessage) (any, string) { return map[string]any{"type": "ok"}, "" })
	created := map[string]any{"type": "tab_created",
		"tab":       map[string]any{"tab_id": "w1:t4", "workspace_id": "w1", "number": 4, "label": "sessionhub:bluebox", "focused": true, "pane_count": 1, "agent_status": "unknown"},
		"root_pane": map[string]any{"pane_id": "w1:p8", "workspace_id": "w1", "tab_id": "w1:t4"}}
	srv.Handle("tab.create", func(json.RawMessage) (any, string) { return created, "" })
	c := dial(t, srv)

	tabs, err := c.TabList()
	if err != nil || len(tabs) != 2 || tabs[1] != (herdr.TabInfo{TabID: "w2:t3", WorkspaceID: "w2", Label: "sessionhub:bluebox"}) {
		t.Fatalf("TabList = %+v, %v", tabs, err)
	}
	if err := c.TabFocus("w2:t3"); err != nil {
		t.Fatal(err)
	}
	got, err := c.TabCreate(herdr.TabCreateParams{Label: "sessionhub:bluebox", Focus: true})
	if err != nil || got.Tab.TabID != "w1:t4" || got.RootPane.PaneID != "w1:p8" {
		t.Fatalf("TabCreate = %+v, %v", got, err)
	}
	for _, c := range []struct{ method, params string }{
		{"tab.list", `{}`},
		{"tab.focus", `{"tab_id":"w2:t3"}`},
		{"tab.create", `{"label":"sessionhub:bluebox","focus":true}`},
	} {
		reqs := srv.RequestsFor(c.method)
		if len(reqs) != 1 || string(reqs[0].Params) != c.params {
			t.Errorf("%s requests %+v, want params %s", c.method, reqs, c.params)
		}
	}
	// A result without a root pane is an error, so no caller runs a
	// command in pane "".
	delete(created, "root_pane")
	if _, err := c.TabCreate(herdr.TabCreateParams{Label: "x"}); err == nil {
		t.Error("TabCreate without a root pane: no error")
	}
}

func TestPaneClose(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("pane.close", func(params json.RawMessage) (any, string) {
		var q struct {
			PaneID string `json:"pane_id"`
		}
		json.Unmarshal(params, &q)
		if q.PaneID != "w5:p1" {
			return nil, "pane_not_found"
		}
		return map[string]any{"type": "ok"}, ""
	})
	c := dial(t, srv)
	if err := c.PaneClose("w5:p1"); err != nil {
		t.Fatal(err)
	}
	var he *herdr.Error
	if err := c.PaneClose("w9:p9"); !errors.As(err, &he) || he.Code != "pane_not_found" {
		t.Errorf("missing pane: %v", err)
	}
	if reqs := srv.RequestsFor("pane.close"); len(reqs) != 2 || string(reqs[0].Params) != `{"pane_id":"w5:p1"}` {
		t.Errorf("requests %+v", reqs)
	}
}
