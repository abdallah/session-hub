package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

// TestGuestScope: a browser signed in for one session sees and acts on
// that session only.
func TestGuestScope(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.register(e.tokA, api.SessionUpsert{ID: sid1, HerdrSession: "default", HerdrPane: "p1"})
	e.register(e.tokA, api.SessionUpsert{ID: sid3, HerdrSession: "other", HerdrPane: "p2"})
	cookie := e.guestSession("guest", sid1)
	get := func(path string) (int, []byte, http.Header) { return e.doHdr("GET", path, "", cookie, nil, nil) }
	act := func(path, action string, body any) int {
		code, _, _ := e.doHdr("POST", path, "", cookie, map[string]string{api.HeaderAction: action}, body)
		return code
	}

	code, body, h := get("/v1/sessions")
	var list []api.Session
	if err := json.Unmarshal(body, &list); err != nil || code != 200 || len(list) != 1 || list[0].ID != sid1 {
		t.Fatalf("GET /v1/sessions: %d %s %v, want only %s", code, body, err, sid1)
	}
	if h.Get(api.HeaderScope) != sid1 {
		t.Errorf("%s = %q, want %s", api.HeaderScope, h.Get(api.HeaderScope), sid1)
	}
	if code, _, _ := get("/v1/sessions/" + sid1); code != 200 {
		t.Errorf("own session: %d, want 200", code)
	}
	if code, _, _ := get("/v1/sessions/" + sid3); code != 404 {
		t.Errorf("other session: %d, want 404", code)
	}
	if code, _, _ := get("/v1/tasks"); code != 403 {
		t.Errorf("tasks: %d, want 403", code)
	}

	// Both sessions block on a permission; the guest's inbox has one item.
	mine, err := e.st.CreatePermission(ctx, e.machine(e.tokA).ID, sid1, api.PermissionIn{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`)})
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.st.CreatePermission(ctx, e.machine(e.tokA).ID, sid3, api.PermissionIn{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`)})
	if err != nil {
		t.Fatal(err)
	}
	code, body, _ = get("/v1/inbox")
	var in api.Inbox
	if err := json.Unmarshal(body, &in); err != nil || code != 200 {
		t.Fatalf("GET /v1/inbox: %d %s %v", code, body, err)
	}
	for _, it := range in.Items {
		if it.Session.ID != sid1 {
			t.Errorf("inbox shows %s", it.Session.ID)
		}
	}
	if n := in.Counts.MoveFailed + in.Counts.Blocked + in.Counts.Waiting + in.Counts.Finished; n != len(in.Items) {
		t.Errorf("counts %+v do not add up to %d items", in.Counts, len(in.Items))
	}

	allow := api.DecisionIn{Decision: api.DecisionAllow}
	if code := act("/v1/permissions/"+other.ID+"/decide", api.HeaderActionApprove, allow); code != 404 {
		t.Errorf("decide other session's request: %d, want 404", code)
	}
	if code := act("/v1/permissions/"+mine.ID+"/decide", api.HeaderActionApprove, allow); code != 200 {
		t.Errorf("decide own request: %d, want 200", code)
	}
	if code := act("/v1/messages", api.HeaderActionSend, api.MessagesIn{SessionIDs: []string{sid1, sid3}, Text: "hi"}); code != 403 {
		t.Errorf("message to another session: %d, want 403", code)
	}
	if code := act("/v1/messages", api.HeaderActionSend, api.MessagesIn{SessionIDs: []string{sid1}, Text: "hi"}); code != 200 {
		t.Errorf("message to own session: %d, want 200", code)
	}
}
