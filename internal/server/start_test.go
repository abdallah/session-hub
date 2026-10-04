package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

// startFromDashboard posts a start request for machine with the browser
// cookie.
func (e *env) startFromDashboard(machine string, in api.StartIn) (int, api.StartRequest, []byte) {
	e.t.Helper()
	code, b, _ := e.doHdr("POST", "/v1/machines/"+machine+"/start", "", e.web,
		map[string]string{api.HeaderAction: api.HeaderActionStart}, in)
	var r api.StartRequest
	json.Unmarshal(b, &r)
	return code, r, b
}

func TestStartFlow(t *testing.T) {
	e := newEnv(t)
	in := api.StartIn{Dir: "~/Code/app", Prompt: "fix the build"}
	before := e.snapshot()
	for _, tc := range []struct {
		machine string
		in      api.StartIn
		want    int
		msg     string
	}{
		{"tower", in, 409, "offline"}, // tower's watcher never polled
		{"nosuch", in, 404, "not found"},
		{"-x", in, 400, "invalid machine name"},
		{"tower", api.StartIn{Dir: "relative"}, 400, "absolute"},
	} {
		if code, _, b := e.startFromDashboard(tc.machine, tc.in); code != tc.want || !strings.Contains(string(b), tc.msg) {
			t.Errorf("start on %s %+v: %d %s, want %d with %q", tc.machine, tc.in, code, b, tc.want, tc.msg)
		}
	}
	if after := e.snapshot(); after != before {
		t.Fatalf("refused requests changed state:\nbefore %s\nafter  %s", before, after)
	}

	e.recordPoll(e.tokA)
	var ms []api.Machine
	e.must(http.StatusOK, "GET", "/v1/machines", e.tokB, nil, &ms)
	for _, m := range ms {
		if m.Online != (m.Name == "tower") {
			t.Errorf("machine %s online=%v", m.Name, m.Online)
		}
	}
	code, req, b := e.startFromDashboard("tower", in)
	if code != http.StatusAccepted || req.State != api.ControlPending || req.Machine != "tower" ||
		!strings.HasPrefix(req.RequestedBy, "web:") || req.Dir != in.Dir || req.Prompt != in.Prompt {
		t.Fatalf("create: %d %s", code, b)
	}

	// bluebox's poll doesn't see it; tower's claims it as a start.
	e.must(http.StatusNoContent, "GET", "/v1/machines/self/control?wait=1", e.tokB, nil, nil)
	var claim api.ControlClaim
	e.must(http.StatusOK, "GET", "/v1/machines/self/control?wait=1", e.tokA, nil, &claim)
	if claim.Request.ID != req.ID || claim.Request.Action != api.ActionStart || claim.Start == nil ||
		claim.Start.Dir != in.Dir || claim.Start.Prompt != in.Prompt || claim.Session.ID != "" {
		t.Fatalf("claim: %+v start %+v", claim.Request, claim.Start)
	}

	// Only tower can post the result.
	result := api.ControlResultIn{State: api.ControlDone, URL: testLink}
	if code, b, _ := e.do("POST", "/v1/control/"+req.ID+"/result", e.tokB, result); code != http.StatusConflict {
		t.Errorf("bluebox's result: %d %s", code, b)
	}
	var done api.StartRequest
	e.must(http.StatusOK, "POST", "/v1/control/"+req.ID+"/result", e.tokA, result, &done)
	if done.State != api.ControlDone || done.URL != testLink {
		t.Fatalf("result: %+v", done)
	}
	// The browser follows it with a read.
	var got api.StartRequest
	code, b, _ = e.doHdr("GET", "/v1/starts/"+req.ID, "", e.web, nil, nil)
	if code != http.StatusOK || json.Unmarshal(b, &got) != nil || got.State != api.ControlDone || got.URL != testLink {
		t.Fatalf("read: %d %s", code, b)
	}
	if code, b, _ := e.do("GET", "/v1/starts/st_nope", e.tokA, nil); code != http.StatusBadRequest {
		t.Errorf("bad id: %d %s", code, b)
	}
}
