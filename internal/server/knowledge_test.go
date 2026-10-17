package server

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestKnowledgeMachineProposes(t *testing.T) {
	e := newEnv(t)
	var out api.KnowledgeAdded
	e.must(201, "POST", "/v1/knowledge", e.tokA, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "Staging is eu-west-1."}, &out)
	if out.Entry.State != api.KnowledgeProposed || out.Entry.CreatedBy != "tower" {
		t.Errorf("entry %+v", out.Entry)
	}
	e.must(400, "POST", "/v1/knowledge", e.tokA, api.KnowledgeIn{Kind: "nope", Scope: "global", Text: "x"}, nil)
}

func TestKnowledgeWebNeedsAction(t *testing.T) {
	e := newEnv(t)
	in := api.KnowledgeIn{Kind: "convention", Scope: "global", Text: "Use British spelling."}
	if code, _ := e.act("POST", "/v1/knowledge", "tasks", in); code != 403 {
		t.Errorf("wrong header: %d, want 403", code)
	}
	code, body := e.act("POST", "/v1/knowledge", api.HeaderActionKnowledge, in)
	var out api.KnowledgeAdded
	if code != 201 || json.Unmarshal(body, &out) != nil || out.Entry.State != api.KnowledgeActive || out.Entry.CreatedBy != "web:phone" {
		t.Errorf("web add: %d %s", code, body)
	}
}

func TestKnowledgeDecideWebOnly(t *testing.T) {
	e := newEnv(t)
	var out api.KnowledgeAdded
	e.must(201, "POST", "/v1/knowledge", e.tokA, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "Staging is eu-west-1."}, &out)
	path := "/v1/knowledge/" + strconv.FormatInt(out.Entry.ID, 10)
	e.must(403, "POST", path+"/decide", e.tokA, api.KnowledgeDecisionIn{Decision: "approve"}, nil)
	e.must(403, "DELETE", path, e.tokA, nil, nil)
	var k api.Knowledge
	code, body := e.act("POST", path+"/decide", api.HeaderActionKnowledge, api.KnowledgeDecisionIn{Decision: "approve"})
	if code != 200 || json.Unmarshal(body, &k) != nil || k.State != api.KnowledgeActive || k.DecidedBy != "web:phone" {
		t.Fatalf("decide: %d %s", code, body)
	}
	e.must(204, "POST", path+"/confirm", e.tokA, nil, nil)
	if code, _ := e.act("DELETE", path, api.HeaderActionKnowledge, nil); code != 204 {
		t.Errorf("retire: %d", code)
	}
	e.must(404, "POST", "/v1/knowledge/999/confirm", e.tokA, nil, nil)
	var list []api.Knowledge
	e.must(200, "GET", "/v1/knowledge?state=retired", e.tokA, nil, &list)
	if len(list) != 1 {
		t.Errorf("retired list %+v", list)
	}
}

func TestKnowledgeLocalForMachine(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1, CWD: "/home/user/proj"})
	e.register(e.tokB, api.SessionUpsert{ID: sid2, CWD: "/home/user/proj"})
	e.must(201, "POST", "/v1/tasks", e.tokA, api.TaskIn{Title: "a", Ref: "SYS-1", SessionID: sid1}, nil)
	e.must(201, "POST", "/v1/tasks", e.tokB, api.TaskIn{Title: "b", Ref: "SYS-2", SessionID: sid2}, nil)
	if code, b := e.act("POST", "/v1/knowledge", api.HeaderActionKnowledge, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "Staging is eu-west-1."}); code != 201 {
		t.Fatalf("add: %d %s", code, b)
	}
	var local api.KnowledgeLocal
	e.must(200, "GET", "/v1/knowledge/local", e.tokA, nil, &local)
	if len(local.Entries) != 1 || local.Version == "" || len(local.SessionRefs) != 1 || len(local.SessionRefs[sid1]) != 1 {
		t.Errorf("local %+v", local)
	}
	if code, _, _ := e.doWith("GET", "/v1/knowledge/local", "", e.web, nil); code != 401 {
		t.Errorf("cookie on local: %d, want 401", code)
	}
}

func TestInstructionsViewAfterMigration(t *testing.T) {
	e := newEnv(t)
	e.act("POST", "/v1/knowledge", api.HeaderActionKnowledge, api.KnowledgeIn{Kind: "convention", Scope: "global", Text: "Never push to main.", Every: ptr(5)})
	e.act("POST", "/v1/knowledge", api.HeaderActionKnowledge, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "Not a rule."})
	var list api.InstructionList
	e.must(200, "GET", "/v1/instructions", e.tokA, nil, &list)
	if len(list.Instructions) != 1 || list.Instructions[0].Text != "Never push to main." || list.Instructions[0].Every != 5 || list.Version == "" {
		t.Errorf("list %+v", list)
	}
}

func ptr(n int) *int { return &n }

func TestKnowledgeProposalAlert(t *testing.T) {
	e := newEnv(t)
	tg := newFakeTelegram(t)
	n := newNotifier(e.st, Config{PublicURL: "https://sessionhub.example.test", TelegramBotToken: testToken, TelegramChatID: "42"}, log.New(e.log, "", 0))
	n.apiBase = tg.URL
	e.server.knowledgeProposed = n.enqueueKnowledge
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); n.run(ctx, nil) }()
	finish := func() {
		for i := 0; i < 200 && len(n.proposals) > 0; i++ {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		cancel()
		<-done
	}
	in := api.KnowledgeIn{ClientID: "c1", Kind: "decision", Scope: "repo:gitlab.com/g/r", Text: "Use sqlite."}
	e.must(201, "POST", "/v1/knowledge", e.tokA, in, nil)
	e.clock.Advance(time.Minute)
	e.must(201, "POST", "/v1/knowledge", e.tokA, in, nil) // replay: no second alert (status is 201 either way)
	e.act("POST", "/v1/knowledge", api.HeaderActionKnowledge, api.KnowledgeIn{Kind: "fact", Scope: "global", Text: "From the web."})
	finish()
	got := tg.sent()
	if len(got) != 1 {
		t.Fatalf("sent %d messages, want 1: %+v", len(got), got)
	}
	if !strings.HasPrefix(got[0].Text, "💡 Knowledge proposed") {
		t.Errorf("text %q", got[0].Text)
	}
	for _, w := range []string{"decision", "repo:gitlab.com/g/r", "Use sqlite."} {
		if !strings.Contains(got[0].Text, w) {
			t.Errorf("text %q lacks %q", got[0].Text, w)
		}
	}
	rm := got[0].ReplyMarkup
	if rm == nil || len(rm.InlineKeyboard) != 1 || len(rm.InlineKeyboard[0]) != 1 ||
		rm.InlineKeyboard[0][0] != (inlineButton{Text: "Open knowledge", URL: "https://sessionhub.example.test/#knowledge"}) {
		t.Errorf("buttons %+v", rm)
	}
}
