package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// ruleAlertEnv is a test server whose rule-added hook feeds a notifier that
// talks to a fake Telegram. finish stops the notifier and waits for it, so
// the fake has seen every message by then.
func ruleAlertEnv(t *testing.T) (e *env, tg *fakeTelegram, finish func()) {
	t.Helper()
	e = newEnv(t)
	tg = newFakeTelegram(t)
	n := newNotifier(e.st, Config{PublicURL: "https://sessionhub.example.test", TelegramBotToken: testToken, TelegramChatID: "42"},
		log.New(e.log, "", 0))
	n.apiBase = tg.URL
	e.server.ruleAdded = n.enqueueRule
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); n.run(ctx, nil) }()
	var once sync.Once
	finish = func() {
		once.Do(func() {
			// Let the notifier drain the queue before it stops.
			for i := 0; i < 200 && len(n.rules) > 0; i++ {
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(100 * time.Millisecond)
			cancel()
			<-done
		})
	}
	t.Cleanup(finish)
	return e, tg, finish
}

func TestRuleAddedSendsOneAlert(t *testing.T) {
	e, tg, finish := ruleAlertEnv(t)
	var r api.Instruction
	e.must(201, "POST", "/v1/instructions", e.tokA, api.InstructionIn{Text: "Never push to main."}, &r)
	finish()
	got := tg.sent()
	if len(got) != 1 {
		t.Fatalf("sent %d messages, want 1", len(got))
	}
	want := fmt.Sprintf("📌 New rule for all sessions\nNever push to main.\nadded by tower · rule %d", r.ID)
	if got[0].Text != want {
		t.Errorf("text %q, want %q", got[0].Text, want)
	}
	rm := got[0].ReplyMarkup
	if rm == nil || len(rm.InlineKeyboard) != 1 || len(rm.InlineKeyboard[0]) != 1 ||
		rm.InlineKeyboard[0][0] != (inlineButton{Text: "Open rules", URL: "https://sessionhub.example.test/#rules"}) {
		t.Errorf("buttons %+v", rm)
	}
	if path, _ := tg.request(0); path != "/bot"+testToken+"/sendMessage" {
		t.Errorf("path %q", path)
	}
}

func TestRuleAddedFromTheDashboardNamesTheSession(t *testing.T) {
	e, tg, finish := ruleAlertEnv(t)
	if code, body := e.act("POST", "/v1/instructions", api.HeaderActionInstructions, api.InstructionIn{Text: "Write in British English."}); code != 201 {
		t.Fatalf("add: %d %s", code, body)
	}
	finish()
	got := tg.sent()
	if len(got) != 1 || !strings.Contains(got[0].Text, "added by web:phone · rule ") {
		t.Errorf("sent %+v", got)
	}
}

func TestRuleAddedWithAlertsOffSendsNothing(t *testing.T) {
	e := newEnv(t)
	tg := newFakeTelegram(t)
	old := telegramAPI
	telegramAPI = tg.URL
	t.Cleanup(func() { telegramAPI = old })
	var logs syncBuffer
	// No Telegram settings: alerts are off.
	stop, onRule := startAlerts(context.Background(), e.st, Config{PublicURL: "https://sessionhub.example.test"}, log.New(&logs, "", 0))
	defer stop()
	e.server.ruleAdded = onRule
	e.must(201, "POST", "/v1/instructions", e.tokA, api.InstructionIn{Text: "Never push to main."}, nil)
	time.Sleep(100 * time.Millisecond)
	if n := len(tg.sent()); n != 0 {
		t.Errorf("%d requests with alerts off", n)
	}
}

func TestRuleAddedStillSucceedsWhenTelegramFails(t *testing.T) {
	e, tg, finish := ruleAlertEnv(t)
	tg.reply(500, `{"ok":false,"description":"boom `+testToken+`"}`)
	e.must(201, "POST", "/v1/instructions", e.tokA, api.InstructionIn{Text: "Never push to main."}, nil)
	finish()
	if len(tg.sent()) != 1 {
		t.Errorf("want one attempt, got %d", len(tg.sent()))
	}
	var list api.InstructionList
	e.must(200, "GET", "/v1/instructions", e.tokA, nil, &list)
	if len(list.Instructions) != 1 {
		t.Errorf("rule not stored: %+v", list)
	}
	out := e.log.String()
	if !strings.Contains(out, "telegram alerts: send failed") {
		t.Errorf("no failure line:\n%s", out)
	}
	if strings.Contains(out, "TEST-PLACEHOLDER") {
		t.Error("the token is in the log")
	}
}

func TestRuleMessageCleansText(t *testing.T) {
	text, buttons := ruleMessage(api.Instruction{ID: 7, Text: "Use\x1b[31m red\x07 ‮gnp.",
		CreatedBy: "web:ph\x1bone\nx"}, "https://sessionhub.example.test")
	for _, r := range text {
		if r != '\n' && (r < 0x20 || r == 0x7f || r == 0x202e) {
			t.Errorf("control character %U in %q", r, text)
		}
	}
	if lines := strings.Split(text, "\n"); len(lines) != 3 {
		t.Errorf("want 3 lines, got %q", text)
	}
	if len(buttons) != 1 {
		t.Errorf("buttons %+v", buttons)
	}
}

func TestRuleQueueFullDrops(t *testing.T) {
	e := newAlertEnv(t)
	for i := 0; i < ruleQueueSize+5; i++ {
		e.n.enqueueRule(api.Instruction{ID: int64(i + 1), Text: "x", CreatedBy: "tower"}) // must not block
	}
	if len(e.n.rules) != ruleQueueSize || !strings.Contains(e.logs.String(), "dropped the alert") {
		t.Errorf("queue %d, log %q", len(e.n.rules), e.logs.String())
	}
}
