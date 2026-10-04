package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func (e *controlEnv) permission(id, tool, input string) api.PermissionRequest {
	e.t.Helper()
	p, err := e.s.CreatePermission(context.Background(), e.tower.ID, id, api.PermissionIn{ToolName: tool, ToolInput: json.RawMessage(input)})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func TestCreatePermission(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	p := e.permission("s1", "Bash", `{ "command": "git push", "description": "Push" }`)
	if !ValidPermissionID(p.ID) || p.SessionID != "s1" || p.Machine != "tower" || p.ToolName != "Bash" ||
		string(p.ToolInput) != `{"command":"git push","description":"Push"}` || p.Truncated || p.State != api.PermissionOpen ||
		!p.CreatedAt.Equal(e.clock.Now()) || !p.ExpiresAt.Equal(e.clock.Now().Add(PermissionTTL)) || p.DecidedAt != nil {
		t.Errorf("created %+v", p)
	}
	got, err := e.s.GetPermission(ctx, p.ID)
	if err != nil || got.ID != p.ID || got.State != api.PermissionOpen {
		t.Errorf("GetPermission: %+v %v", got, err)
	}
	if _, err := e.s.PermissionFor(ctx, e.bluebox.ID, p.ID); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("bluebox reads tower's request: %v, want ErrWrongMachine", err)
	}
	if _, err := e.s.CreatePermission(ctx, e.bluebox.ID, "s1", api.PermissionIn{ToolName: "Bash"}); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("bluebox creates for tower's session: %v, want ErrWrongMachine", err)
	}
	if _, err := e.s.CreatePermission(ctx, e.tower.ID, "nope", api.PermissionIn{ToolName: "Bash"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v, want ErrNotFound", err)
	}
	for name, in := range map[string]api.PermissionIn{
		"no tool":      {ToolName: " "},
		"tool newline": {ToolName: "Ba\nsh"},
		"long tool":    {ToolName: strings.Repeat("x", 129)},
		"broken input": {ToolName: "Bash", ToolInput: json.RawMessage(`{"a":`)},
	} {
		if _, err := e.s.CreatePermission(ctx, e.tower.ID, "s1", in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	big := e.permission("s1", "Write", `{"file_path":"/x","content":"`+strings.Repeat("a", 9000)+`"}`)
	if !big.Truncated || api.PermissionInputText(big.ToolInput) == "" || len(big.ToolInput) > 8100 {
		t.Errorf("big input: truncated=%v, %d bytes", big.Truncated, len(big.ToolInput))
	}
	if _, err := e.s.GetPermission(ctx, "pr_AAAAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown request: %v, want ErrNotFound", err)
	}
	if _, err := e.s.GetPermission(ctx, "msg_AAAAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a message ID: %v, want ErrInvalid", err)
	}
}

func TestDecidePermissionOnce(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	a := e.permission("s1", "Bash", `{"command":"git push"}`)
	e.clock.Advance(time.Second)
	got, err := e.s.DecidePermission(ctx, a.ID, api.DecisionIn{Decision: api.DecisionAllow, Reason: "ignored"}, "web:phone")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != api.PermissionDecided || got.Decision != api.DecisionAllow || got.Reason != "" || got.DecidedBy != "web:phone" ||
		got.DecidedAt == nil || !got.DecidedAt.Equal(e.clock.Now()) {
		t.Errorf("decided %+v", got)
	}
	if _, err := e.s.DecidePermission(ctx, a.ID, api.DecisionIn{Decision: api.DecisionDeny}, "machine:tower"); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("second decision: %v, want ErrRequestClosed", err)
	}
	b := e.permission("s1", "Bash", `{"command":"rm -rf build"}`)
	got, err = e.s.DecidePermission(ctx, b.ID, api.DecisionIn{Decision: api.DecisionDeny, Reason: "  use make clean  "}, "machine:bluebox")
	if err != nil || got.Decision != api.DecisionDeny || got.Reason != "use make clean" {
		t.Errorf("deny: %+v %v", got, err)
	}
	evs := e.events("s1", api.KindPermission)
	if len(evs) != 2 {
		t.Fatalf("%d permission events, want 2", len(evs))
	}
	var p map[string]string
	json.Unmarshal(evs[0].Payload, &p) // newest first: the deny
	if p["request_id"] != b.ID || p["tool"] != "Bash" || p["input"] != "rm -rf build" || p["decision"] != "deny" ||
		p["reason"] != "use make clean" || p["by"] != "machine:bluebox" || evs[0].Source != api.SourceServer {
		t.Errorf("event %v from %s", p, evs[0].Source)
	}
	c := e.permission("s1", "Bash", `{"command":"x"}`)
	for name, in := range map[string]api.DecisionIn{
		"maybe":          {Decision: "maybe"},
		"reason newline": {Decision: api.DecisionDeny, Reason: "a\nb"},
		"long reason":    {Decision: api.DecisionDeny, Reason: strings.Repeat("r", MaxReasonRunes+1)},
	} {
		if _, err := e.s.DecidePermission(ctx, c.ID, in, "web:phone"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := e.s.DecidePermission(ctx, c.ID, api.DecisionIn{Decision: api.DecisionAllow}, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("no decider: %v, want ErrInvalid", err)
	}
	if _, err := e.s.DecidePermission(ctx, "pr_AAAAAAAAAAAAAAAAAAAAAA", api.DecisionIn{Decision: api.DecisionAllow}, "web:phone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown request: %v, want ErrNotFound", err)
	}
}

func TestPermissionExpiry(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	p := e.permission("s1", "Bash", `{"command":"x"}`)
	e.clock.Advance(PermissionTTL)
	got, err := e.s.PermissionFor(ctx, e.tower.ID, p.ID)
	if err != nil || got.State != api.PermissionExpired {
		t.Errorf("after TTL: %+v %v, want expired", got, err)
	}
	if _, err := e.s.DecidePermission(ctx, p.ID, api.DecisionIn{Decision: api.DecisionAllow}, "web:phone"); !errors.Is(err, ErrGone) {
		t.Errorf("decide after TTL: %v, want ErrGone", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM permission_requests WHERE state = 'expired'`); n != 1 {
		t.Errorf("stored expired rows %d, want 1", n)
	}
}

func TestPermissionAnsweredLocally(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	e.setState("s1", "blocked")
	e.clock.Advance(time.Second)
	p := e.permission("s1", "Bash", `{"command":"x"}`)
	e.clock.Advance(time.Second)
	e.setState("s1", "working")
	got, _ := e.s.GetPermission(ctx, p.ID)
	if got.State != api.PermissionAnsweredLocally {
		t.Fatalf("after leaving blocked: %s, want answered_locally", got.State)
	}
	if _, err := e.s.DecidePermission(ctx, p.ID, api.DecisionIn{Decision: api.DecisionAllow}, "web:phone"); !errors.Is(err, ErrGone) {
		t.Errorf("decide after a local answer: %v, want ErrGone", err)
	}
	// A state change that does not leave blocked closes nothing.
	q := e.permission("s1", "Bash", `{"command":"y"}`)
	e.setState("s1", "idle")
	if got, _ := e.s.GetPermission(ctx, q.ID); got.State != api.PermissionOpen {
		t.Errorf("idle after working closed a request: %s", got.State)
	}
}

func TestPermissionAnsweredLocallyKeepsLaterRequest(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	t0 := e.clock.Now()
	e.setStateAt("s1", "blocked", t0)
	e.clock.Advance(2 * time.Second)
	p := e.permission("s1", "Bash", `{"command":"second prompt"}`)
	// The working event for the first prompt's answer arrives late, stamped
	// before the second request.
	e.setStateAt("s1", "working", t0.Add(time.Second))
	if got, _ := e.s.GetPermission(ctx, p.ID); got.State != api.PermissionOpen {
		t.Errorf("a late event closed a newer request: %s", got.State)
	}
}

func TestPermissionSnapshotPath(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.snapshot("s1", "blocked")
	p := e.permission("s1", "Bash", `{"command":"x"}`)
	e.clock.Advance(time.Second)
	// The snapshot may have been built before the request: within the
	// grace it leaves the request open.
	e.snapshot("s1", "working")
	if got, _ := e.s.GetPermission(ctx, p.ID); got.State != api.PermissionOpen {
		t.Fatalf("a snapshot within the grace closed the request: %s", got.State)
	}
	e.snapshot("s1", "blocked")
	e.clock.Advance(PermissionGrace + time.Second)
	e.snapshot("s1", "working")
	if got, _ := e.s.GetPermission(ctx, p.ID); got.State != api.PermissionAnsweredLocally {
		t.Errorf("after the grace: %s, want answered_locally", got.State)
	}
}

func TestPermissionClosedWithSession(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	p := e.permission("s1", "Bash", `{"command":"x"}`)
	if err := e.s.AddEvent(ctx, e.tower.ID, "s1", api.EventIn{Kind: api.KindPaneClosed, Source: api.SourcePlugin}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.GetPermission(ctx, p.ID); got.State != api.PermissionClosed {
		t.Errorf("after pane_closed: %s, want closed", got.State)
	}
	if _, err := e.s.CreatePermission(ctx, e.tower.ID, "s1", api.PermissionIn{ToolName: "Bash"}); !errors.Is(err, ErrGone) {
		t.Errorf("request for an ended session: %v, want ErrGone", err)
	}
	e.session(e.tower, "s3", "w1:p3")
	r := e.permission("s3", "Bash", `{"command":"z"}`)
	if err := e.s.AddEvent(ctx, e.tower.ID, "s3", api.EventIn{Kind: api.KindEnded, Source: api.SourceHooks}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.GetPermission(ctx, r.ID); got.State != api.PermissionClosed {
		t.Errorf("after ended: %s, want closed", got.State)
	}
	// Missing from a snapshot ends the session too.
	e.snapshot("s2", "blocked")
	q := e.permission("s2", "Bash", `{"command":"y"}`)
	if _, err := e.s.ReconcileHerdr(ctx, e.tower.ID, api.HerdrSessionsPut{HerdrSession: "default"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.GetPermission(ctx, q.ID); got.State != api.PermissionClosed {
		t.Errorf("after a snapshot without it: %s, want closed", got.State)
	}
}

func TestInboxCarriesPermission(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	e.setState("s1", "blocked")
	if it := e.inboxFor("s1"); it == nil || it.Permission != nil {
		t.Fatalf("blocked item without a request: %+v", it)
	}
	e.permission("s1", "Bash", `{"command":"first"}`)
	e.clock.Advance(time.Second)
	newest := e.permission("s1", "Bash", `{"command":"second"}`)
	it := e.inboxFor("s1")
	if it == nil || it.Permission == nil || it.Permission.ID != newest.ID {
		t.Fatalf("inbox item %+v, want the newest request", it)
	}
	if _, err := e.s.DecidePermission(ctx, newest.ID, api.DecisionIn{Decision: api.DecisionAllow}, "web:phone"); err != nil {
		t.Fatal(err)
	}
	if it := e.inboxFor("s1"); it == nil || it.Permission == nil || api.PermissionInputText(it.Permission.ToolInput) != "first" {
		t.Errorf("after deciding the newest, the item shows %+v, want the first", it)
	}
	// A waiting item never carries one.
	e.session(e.tower, "s2", "w1:p2")
	e.permission("s2", "Bash", `{"command":"z"}`)
	e.sendReport("s2", "pick a name")
	if it := e.inboxFor("s2"); it == nil || it.Group != api.InboxWaiting || it.Permission != nil {
		t.Errorf("waiting item %+v", it)
	}
}

func TestDecidePermissionConcurrent(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	p := e.permission("s1", "Bash", `{"command":"x"}`)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, d := range []string{api.DecisionAllow, api.DecisionDeny} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.s.DecidePermission(ctx, p.ID, api.DecisionIn{Decision: d}, "web:phone")
		}()
	}
	wg.Wait()
	ok, closed := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrRequestClosed):
			closed++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || closed != 1 {
		t.Errorf("%d succeeded, %d closed; want 1 and 1", ok, closed)
	}
	if n := len(e.events("s1", api.KindPermission)); n != 1 {
		t.Errorf("%d permission events, want 1", n)
	}
}

// TestDecidePermissionCutInput: an allow for a cut input is ErrInputCut
// and leaves the request open; a deny still decides it.
func TestDecidePermissionCutInput(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "s1", "w1:p1")
	p := e.permission("s1", "Write", `{"file_path":"/a","content":"`+strings.Repeat("x", 9000)+`"}`)
	if !p.Truncated {
		t.Fatal("a 9 KB input is not cut")
	}
	_, err := e.s.DecidePermission(ctx, p.ID, api.DecisionIn{Decision: api.DecisionAllow}, "web:phone")
	if !errors.Is(err, ErrInputCut) || err.Error() != "the input was cut; allow it in the terminal" {
		t.Errorf("allow: %v, want ErrInputCut", err)
	}
	got, err := e.s.PermissionFor(ctx, e.tower.ID, p.ID)
	if err != nil || got.State != api.PermissionOpen {
		t.Errorf("after a refused allow: %+v %v, want open", got, err)
	}
	got, err = e.s.DecidePermission(ctx, p.ID, api.DecisionIn{Decision: api.DecisionDeny, Reason: "too long"}, "web:phone")
	if err != nil || got.State != api.PermissionDecided || got.Decision != api.DecisionDeny {
		t.Errorf("deny: %+v %v", got, err)
	}
}
