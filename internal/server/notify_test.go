package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

// testToken is an obvious placeholder; no real token appears in tests.
const testToken = "123456:TEST-PLACEHOLDER-TOKEN"

type tgReply struct {
	status int
	body   string
}

// fakeTelegram is a Bot API server. It records each request and answers it
// with the next scripted reply, else 200 {"ok":true}.
type fakeTelegram struct {
	*httptest.Server
	mu      sync.Mutex
	paths   []string
	raw     []string
	bodies  []sendMessageIn
	replies []tgReply
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	t.Helper()
	f := &fakeTelegram{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var in sendMessageIn
		json.Unmarshal(b, &in)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.raw = append(f.raw, string(b))
		f.bodies = append(f.bodies, in)
		rep := tgReply{http.StatusOK, `{"ok":true,"result":{"message_id":1}}`}
		if len(f.replies) > 0 {
			rep, f.replies = f.replies[0], f.replies[1:]
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		w.Write([]byte(rep.body))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTelegram) reply(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, tgReply{status, body})
}

// request returns the path and raw body of request i.
func (f *fakeTelegram) request(i int) (path, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paths[i], f.raw[i]
}

func (f *fakeTelegram) sent() []sendMessageIn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sendMessageIn(nil), f.bodies...)
}

// alertEnv is a store with a fake clock and machine tower, and a notifier
// that talks to a fake Telegram.
type alertEnv struct {
	t     *testing.T
	st    *store.Store
	clock *fakeClock
	m     store.Machine
	tg    *fakeTelegram
	n     *notifier
	logs  *syncBuffer
}

func newAlertEnv(t *testing.T) *alertEnv {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	st, err := store.Open(filepath.Join(t.TempDir(), "sessionhub.db"), store.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	tok, _, err := st.AddMachine(ctx, "tower", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.MachineByToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	tg := newFakeTelegram(t)
	logs := &syncBuffer{}
	n := newNotifier(st, Config{PublicURL: "https://sessionhub.example.test", TelegramBotToken: testToken, TelegramChatID: "42"},
		log.New(logs, "", 0))
	n.apiBase = tg.URL
	return &alertEnv{t: t, st: st, clock: clock, m: m, tg: tg, n: n, logs: logs}
}

// set registers or updates session id with this agent state, the way the
// watcher's heartbeat does.
func (e *alertEnv) set(id, state string) {
	e.t.Helper()
	if _, err := e.st.UpsertSession(context.Background(), e.m.ID, api.SessionUpsert{ID: id, Source: api.SourcePlugin,
		AgentState: state, TitleHint: "fix login"}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *alertEnv) tick() int { return e.n.tick(context.Background()) }

func (e *alertEnv) alerted() map[string]time.Time {
	e.t.Helper()
	m, err := e.st.LastAlerts(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func TestNotifierGraceAndMessage(t *testing.T) {
	e := newAlertEnv(t)
	e.set("s1", "blocked")
	since := e.clock.Now()
	e.clock.Advance(29 * time.Second)
	if n := e.tick(); n != 0 || len(e.tg.sent()) != 0 {
		t.Fatalf("29 s after blocking: sent %d, requests %d, want none", n, len(e.tg.sent()))
	}
	if len(e.alerted()) != 0 {
		t.Fatal("alert recorded before any send")
	}
	e.clock.Advance(time.Second)
	if n := e.tick(); n != 1 {
		t.Fatalf("30 s after blocking: sent %d, want 1", n)
	}
	want := sendMessageIn{ChatID: "42", Text: "⏸ Blocked: fix login\ntower · blocked 30s ago",
		ReplyMarkup: &replyMarkup{InlineKeyboard: [][]inlineButton{{{Text: "Open inbox", URL: "https://sessionhub.example.test/#inbox"}}}}}
	if got := e.tg.sent(); !reflect.DeepEqual(got[0], want) {
		t.Errorf("message %+v\nwant    %+v", got[0], want)
	}
	path, raw := e.tg.request(0)
	if path != "/bot"+testToken+"/sendMessage" {
		t.Error("request path is not /bot<token>/sendMessage")
	}
	if strings.Contains(raw, "parse_mode") {
		t.Errorf("request sets parse_mode: %s", raw)
	}
	if got := e.alerted(); !got["s1"].Equal(since) {
		t.Errorf("recorded alert %v, want since %v", got, since)
	}
}

// TestNotifierAlertsOncePerBlock: a block whose since never moves alerts
// once, however many heartbeats repeat it. Blocking again alerts again.
func TestNotifierAlertsOncePerBlock(t *testing.T) {
	e := newAlertEnv(t)
	e.set("s1", "blocked")
	e.clock.Advance(30 * time.Second)
	if n := e.tick(); n != 1 {
		t.Fatalf("first tick sent %d, want 1", n)
	}
	for i := 0; i < 5; i++ {
		e.clock.Advance(15 * time.Second)
		e.set("s1", "blocked")
		if n := e.tick(); n != 0 {
			t.Fatalf("repeat %d sent %d, want 0", i, n)
		}
	}
	e.set("s1", "working")
	e.clock.Advance(time.Minute)
	e.set("s1", "blocked")
	if n := e.tick(); n != 0 {
		t.Fatalf("new block inside the grace sent %d", n)
	}
	e.clock.Advance(30 * time.Second)
	if n := e.tick(); n != 1 {
		t.Fatalf("new block after the grace sent %d, want 1", n)
	}
	if got := len(e.tg.sent()); got != 2 {
		t.Errorf("%d messages in total, want 2", got)
	}
}

// TestNotifierSkipsTriagedAndFinished: a dismissed or snoozed Blocked item,
// and a Finished item, never alert. A snooze that ends alerts.
func TestNotifierSkipsTriagedAndFinished(t *testing.T) {
	e := newAlertEnv(t)
	ctx := context.Background()
	e.set("dis", "blocked")
	e.set("snz", "blocked")
	e.set("fin", "working")
	e.set("fin", "idle")
	e.clock.Advance(30 * time.Second)
	in, err := e.st.Inbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range in.Items {
		switch it.Session.ID {
		case "dis":
			if err := e.st.Triage(ctx, "dis", it.Since, nil); err != nil {
				t.Fatal(err)
			}
		case "snz":
			until := e.clock.Now().Add(time.Hour)
			if err := e.st.Triage(ctx, "snz", it.Since, &until); err != nil {
				t.Fatal(err)
			}
		}
	}
	groups := map[string]int{}
	in, err = e.st.Inbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range in.Items {
		groups[it.Group]++
	}
	if groups[api.InboxFinished] != 1 || groups[api.InboxBlocked] != 0 || groups[api.InboxWaiting] != 0 {
		t.Fatalf("inbox groups %v, want one Finished, no Blocked, no Waiting", groups)
	}
	if n := e.tick(); n != 0 {
		t.Fatalf("sent %d with every blocked item triaged, want 0", n)
	}
	e.clock.Advance(time.Hour)
	if n := e.tick(); n != 1 {
		t.Fatalf("after the snooze ended: sent %d, want 1", n)
	}
	if got := e.alerted(); len(got) != 1 || got["snz"].IsZero() {
		t.Errorf("alerts %v, want snz only", got)
	}
}

// report records a report that waits on text for session id.
func (e *alertEnv) report(id string, waitingOn ...string) {
	e.t.Helper()
	if err := e.st.AddReport(context.Background(), e.m.ID, id, api.ReportIn{WaitingOn: waitingOn}); err != nil {
		e.t.Fatal(err)
	}
}

// TestNotifierAlertsWaiting: a Waiting item alerts after the grace, with the
// Waiting first line and its waiting_on text. It alerts once per report, and
// a new report alerts again.
func TestNotifierAlertsWaiting(t *testing.T) {
	e := newAlertEnv(t)
	e.set("s1", "working")
	e.report("s1", "approve the plan", "pick a name")
	since := e.clock.Now()
	e.clock.Advance(29 * time.Second)
	if n := e.tick(); n != 0 {
		t.Fatalf("29 s after the report: sent %d, want 0", n)
	}
	e.clock.Advance(time.Second)
	if n := e.tick(); n != 1 {
		t.Fatalf("30 s after the report: sent %d, want 1", n)
	}
	want := "💬 Waiting on you: fix login\ntower · waiting 30s ago\napprove the plan; pick a name"
	if got := e.tg.sent(); got[0].Text != want {
		t.Errorf("message %q\nwant    %q", got[0].Text, want)
	}
	if got := e.alerted(); !got["s1"].Equal(since) {
		t.Errorf("recorded alert %v, want since %v", got, since)
	}
	e.clock.Advance(time.Minute)
	if n := e.tick(); n != 0 {
		t.Fatalf("same report, next tick: sent %d, want 0", n)
	}
	e.report("s1", "choose a color")
	e.clock.Advance(30 * time.Second)
	if n := e.tick(); n != 1 {
		t.Fatalf("new report: sent %d, want 1", n)
	}
	if got := e.tg.sent(); len(got) != 2 || !strings.HasSuffix(got[1].Text, "\nchoose a color") {
		t.Errorf("sent %+v, want a second message for the new report", got)
	}
}

// TestNotifierSkipsTriagedWaiting: a dismissed Waiting item never alerts.
func TestNotifierSkipsTriagedWaiting(t *testing.T) {
	e := newAlertEnv(t)
	ctx := context.Background()
	e.set("s1", "working")
	e.report("s1", "pick a name")
	e.clock.Advance(time.Second)
	in, err := e.st.Inbox(ctx)
	if err != nil || len(in.Items) != 1 || in.Items[0].Group != api.InboxWaiting {
		t.Fatalf("inbox %+v, err %v, want one Waiting item", in.Items, err)
	}
	if err := e.st.Triage(ctx, "s1", in.Items[0].Since, nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	if n := e.tick(); n != 0 {
		t.Fatalf("sent %d for a dismissed Waiting item, want 0", n)
	}
}

// TestNotifierSkipsStaleWaitingOnFirstTick: after a start, a Waiting item
// older than an hour is recorded without a send.
func TestNotifierSkipsStaleWaitingOnFirstTick(t *testing.T) {
	e := newAlertEnv(t)
	e.set("s1", "working")
	e.report("s1", "pick a name")
	e.clock.Advance(61 * time.Minute)
	if n := e.tick(); n != 0 || len(e.tg.sent()) != 0 {
		t.Fatalf("stale Waiting item: sent %d, want 0", n)
	}
	if e.alerted()["s1"].IsZero() {
		t.Error("stale Waiting item not recorded")
	}
}

// TestNotifierRetriesFailedSend: a 500, or a 200 with ok false, records
// nothing, and the next tick sends again.
func TestNotifierRetriesFailedSend(t *testing.T) {
	e := newAlertEnv(t)
	e.tg.reply(http.StatusInternalServerError, `{"ok":false,"error_code":500,"description":"Internal Server Error"}`)
	e.set("s1", "blocked")
	e.clock.Advance(30 * time.Second)
	if n := e.tick(); n != 0 || len(e.alerted()) != 0 {
		t.Fatalf("failed send: sent %d, alerts %v", n, e.alerted())
	}
	e.clock.Advance(15 * time.Second)
	if n := e.tick(); n != 1 || e.alerted()["s1"].IsZero() {
		t.Fatalf("retry: sent %d, alerts %v", n, e.alerted())
	}
	e.tg.reply(http.StatusOK, `{"ok":false,"description":"Bad Request: chat not found"}`)
	e.set("s2", "blocked")
	e.clock.Advance(30 * time.Second)
	if n := e.tick(); n != 0 || !e.alerted()["s2"].IsZero() {
		t.Fatalf("ok false: sent %d, alerts %v", n, e.alerted())
	}
	e.clock.Advance(15 * time.Second)
	if n := e.tick(); n != 1 {
		t.Fatalf("retry after ok false: sent %d, want 1", n)
	}
	if got := len(e.tg.sent()); got != 4 {
		t.Errorf("%d requests, want 4 (two failures, two retries)", got)
	}
}

// TestNotifierContinuesAfterRefusal: Telegram refusing one message (HTTP 400)
// does not end the tick. The next item is sent and recorded, and the refused
// item is retried on the next tick.
func TestNotifierContinuesAfterRefusal(t *testing.T) {
	e := newAlertEnv(t)
	e.tg.reply(http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`)
	e.set("s1", "blocked")
	e.clock.Advance(time.Second)
	e.set("s2", "blocked")
	e.clock.Advance(30 * time.Second)
	if n := e.tick(); n != 1 || len(e.tg.sent()) != 2 {
		t.Fatalf("tick with a refusal: sent %d, requests %d, want 1 and 2", n, len(e.tg.sent()))
	}
	if got := e.alerted(); len(got) != 1 || got["s2"].IsZero() {
		t.Fatalf("alerts %v, want only s2", got)
	}
	if !strings.Contains(e.logs.String(), "refused") {
		t.Errorf("no log line for the refusal:\n%s", e.logs.String())
	}
	e.clock.Advance(15 * time.Second)
	if n := e.tick(); n != 1 || e.alerted()["s1"].IsZero() {
		t.Fatalf("retry: sent %d, alerts %v, want s1 sent", n, e.alerted())
	}
}

// TestNotifierCapsRefusedAttempts: a chat Telegram always refuses costs at
// most maxAlertsPerTick requests a tick, not one per Blocked item.
func TestNotifierCapsRefusedAttempts(t *testing.T) {
	e := newAlertEnv(t)
	for i := 0; i < 12; i++ {
		e.tg.reply(http.StatusForbidden, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked"}`)
		e.set(fmt.Sprintf("s%02d", i), "blocked")
	}
	e.clock.Advance(30 * time.Second)
	if n := e.tick(); n != 0 || len(e.tg.sent()) != maxAlertsPerTick {
		t.Fatalf("tick: sent %d, requests %d, want 0 and %d", n, len(e.tg.sent()), maxAlertsPerTick)
	}
	if len(e.alerted()) != 0 {
		t.Errorf("refused items recorded: %v", e.alerted())
	}
}

// TestNotifierSkipsStaleBlocksOnFirstTick: after a start, a Blocked item that
// has been blocked for more than an hour is recorded without a send. Newer
// items send.
func TestNotifierSkipsStaleBlocksOnFirstTick(t *testing.T) {
	e := newAlertEnv(t)
	e.set("old", "blocked")
	oldSince := e.clock.Now()
	e.clock.Advance(30 * time.Minute)
	e.set("recent", "blocked")
	e.clock.Advance(31 * time.Minute) // old: 61 min, recent: 31 min
	if n := e.tick(); n != 1 {
		t.Fatalf("first tick sent %d, want 1", n)
	}
	sent := e.tg.sent()
	if len(sent) != 1 || !strings.Contains(sent[0].Text, "blocked 31m ago") {
		t.Fatalf("sent %+v, want only the recent item", sent)
	}
	got := e.alerted()
	if !got["old"].Equal(oldSince) || got["recent"].IsZero() {
		t.Errorf("alerts %v, want old recorded at %v and recent recorded", got, oldSince)
	}
}

// TestNotifierHonoursRetryAfter: after HTTP 429 the notifier sends nothing
// until retry_after has passed, or 30 s without it.
func TestNotifierHonoursRetryAfter(t *testing.T) {
	e := newAlertEnv(t)
	e.tg.reply(http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 40","parameters":{"retry_after":40}}`)
	e.set("s1", "blocked")
	e.clock.Advance(30 * time.Second)
	if n := e.tick(); n != 0 {
		t.Fatalf("429: sent %d", n)
	}
	for _, d := range []time.Duration{15 * time.Second, 24 * time.Second} {
		e.clock.Advance(d)
		if n := e.tick(); n != 0 || len(e.tg.sent()) != 1 {
			t.Fatalf("inside retry_after: sent %d, requests %d, want 0 and 1", n, len(e.tg.sent()))
		}
	}
	e.clock.Advance(time.Second)
	if n := e.tick(); n != 1 || len(e.tg.sent()) != 2 {
		t.Fatalf("at retry_after: sent %d, requests %d, want 1 and 2", n, len(e.tg.sent()))
	}
	e.tg.reply(http.StatusTooManyRequests, `{"ok":false,"error_code":429}`)
	e.set("s2", "blocked")
	e.clock.Advance(30 * time.Second)
	e.tick()
	e.clock.Advance(29 * time.Second)
	if n := e.tick(); n != 0 || len(e.tg.sent()) != 3 {
		t.Fatalf("429 without retry_after, 29 s later: sent %d, requests %d", n, len(e.tg.sent()))
	}
	e.clock.Advance(time.Second)
	if n := e.tick(); n != 1 {
		t.Fatalf("429 without retry_after, 30 s later: sent %d, want 1", n)
	}
}

func TestNotifierLimitsSendsPerTick(t *testing.T) {
	e := newAlertEnv(t)
	for i := 0; i < 12; i++ {
		e.set(fmt.Sprintf("s%02d", i), "blocked")
	}
	e.clock.Advance(30 * time.Second)
	for i, want := range []int{10, 2, 0} {
		if n := e.tick(); n != want {
			t.Errorf("tick %d sent %d, want %d", i, n, want)
		}
	}
	if got := len(e.alerted()); got != 12 {
		t.Errorf("%d sessions alerted, want 12", got)
	}
}

// TestNotifierNeverLogsToken: a network error, a 500 whose description
// echoes the token, and a 429 each log one line a minute apart, and no line
// holds the token or the request path. Failures inside a minute log once.
func TestNotifierNeverLogsToken(t *testing.T) {
	e := newAlertEnv(t)
	e.set("s1", "blocked")
	e.clock.Advance(30 * time.Second)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	e.n.apiBase = dead.URL
	e.tick()
	e.n.apiBase = e.tg.URL
	e.tg.reply(http.StatusInternalServerError, `{"ok":false,"description":"boom `+testToken+`"}`)
	e.clock.Advance(time.Minute)
	e.tick()
	e.tg.reply(http.StatusTooManyRequests, `{"ok":false,"parameters":{"retry_after":1}}`)
	e.clock.Advance(time.Minute)
	e.tick()
	e.tg.reply(http.StatusInternalServerError, `{"ok":false,"description":"again"}`)
	e.clock.Advance(2 * time.Second)
	e.tick()
	out := e.logs.String()
	if n := strings.Count(out, "\n"); n != 3 {
		t.Errorf("%d log lines, want 3 (one per minute)", n)
	}
	if strings.Contains(out, testToken) || strings.Contains(out, "TEST-PLACEHOLDER") || strings.Contains(out, "/bot") {
		t.Error("a log line holds the token or the request path")
	}
	if !strings.Contains(out, "HTTP 500") || !strings.Contains(out, "HTTP 429") {
		t.Errorf("log lacks the failures:\n%s", out)
	}
}

func TestAlertMessage(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rc := "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"
	before, after := now.Add(-time.Hour), now.Add(-time.Minute)
	report := &api.Report{TS: now.Add(-10 * time.Minute), WaitingOn: []string{"approve the plan", "pick a name"}}
	open := inlineButton{Text: "Open inbox", URL: "https://sessionhub.example.test/#inbox"}
	cases := []struct {
		name    string
		s       api.Session
		since   time.Duration
		text    string
		buttons []inlineButton
	}{
		{"report waiting_on, cleaned title, Remote Control",
			api.Session{ID: "0a1b2c3d-1111", Title: "fix \x1b[31mlogin‮", Machine: "bluebox", Recap: "Asked about tokens.",
				RemoteControlURL: rc, LastPromptAt: &before, LatestReport: report},
			3 * time.Minute, "⏸ Blocked: fix [31mlogin\nbluebox · blocked 3m ago\napprove the plan; pick a name",
			[]inlineButton{open, {Text: "Remote Control", URL: rc}}},
		{"report older than the last prompt: the recap",
			api.Session{ID: "0a1b2c3d-1111", Title: "t", Machine: "bluebox", Recap: "Asked\nabout tokens.", LastPromptAt: &after, LatestReport: report},
			2 * time.Hour, "⏸ Blocked: t\nbluebox · blocked 2h ago\nAsked about tokens.", []inlineButton{open}},
		{"no detail: two lines; cwd as title",
			api.Session{ID: "0a1b2c3d-1111", CWD: "/home/user/x", Machine: "tower"},
			72 * time.Hour, "⏸ Blocked: /home/user/x\ntower · blocked 3d ago", []inlineButton{open}},
		{"no title or cwd: the ID; an http link gets no button",
			api.Session{ID: "0a1b2c3d-1111", Machine: "tower", RemoteControlURL: "http://claude.ai/x"},
			45 * time.Second, "⏸ Blocked: session 0a1b2c3d\ntower · blocked 45s ago", []inlineButton{open}},
		{"the mod's question wins over the report and the recap",
			api.Session{ID: "0a1b2c3d-1111", Title: "t", Machine: "bluebox", Recap: "Asked about tokens.", LatestReport: report,
				BlockedOn: "Question: Which\x1b[2J library? (a, b)"},
			time.Minute, "⏸ Blocked: t\nbluebox · blocked 1m ago\nQuestion: Which[2J library? (a, b)", []inlineButton{open}},
		{"the question is plain text: Telegram gets no parse_mode, so nothing is escaped",
			api.Session{ID: "0a1b2c3d-1111", Title: "t", Machine: "bluebox", BlockedOn: `Question: <b>bold</b> & <a href="x">link</a>?`},
			time.Minute, "⏸ Blocked: t\nbluebox · blocked 1m ago\nQuestion: <b>bold</b> & <a href=\"x\">link</a>?", []inlineButton{open}},
		{"detail cut to 300 characters",
			api.Session{ID: "0a1b2c3d-1111", Title: "t", Machine: "tower", Recap: strings.Repeat("x", 400)},
			time.Minute, "⏸ Blocked: t\ntower · blocked 1m ago\n" + strings.Repeat("x", 299) + "…", []inlineButton{open}},
	}
	for _, c := range cases {
		it := api.InboxItem{Group: api.InboxBlocked, Since: now.Add(-c.since), Session: c.s}
		text, buttons := alertMessage(it, now, "https://sessionhub.example.test")
		if text != c.text {
			t.Errorf("%s: text\n%q\nwant\n%q", c.name, text, c.text)
		}
		if !reflect.DeepEqual(buttons, c.buttons) {
			t.Errorf("%s: buttons %+v, want %+v", c.name, buttons, c.buttons)
		}
	}
	w := api.InboxItem{Group: api.InboxWaiting, Since: now.Add(-5 * time.Minute), WaitingOn: []string{"a\x1b[31m", "b"},
		Session: api.Session{ID: "0a1b2c3d-1111", Title: "t", Machine: "bluebox", Recap: "ignored", RemoteControlURL: rc}}
	text, buttons := alertMessage(w, now, "https://sessionhub.example.test")
	if want := "💬 Waiting on you: t\nbluebox · waiting 5m ago\na[31m; b"; text != want {
		t.Errorf("Waiting text\n%q\nwant\n%q", text, want)
	}
	if len(buttons) != 2 {
		t.Errorf("Waiting buttons %+v, want Open inbox and Remote Control", buttons)
	}
}

// TestNotifierRedactsBeforeTruncating: a description that holds the token
// across the 200-rune cut leaks no prefix of it.
func TestNotifierRedactsBeforeTruncating(t *testing.T) {
	e := newAlertEnv(t)
	e.set("s1", "blocked")
	e.clock.Advance(30 * time.Second)
	e.tg.reply(http.StatusInternalServerError, `{"ok":false,"description":"`+strings.Repeat("x", 190)+testToken+`"}`)
	e.tick()
	if out := e.logs.String(); strings.Contains(out, "TEST-") || strings.Contains(out, "123456") {
		t.Errorf("a prefix of the token is logged: %s", out)
	}
}

// TestNotifierSilentOnShutdown: a failure after ctx ends is not logged.
func TestNotifierSilentOnShutdown(t *testing.T) {
	e := newAlertEnv(t)
	e.set("s1", "blocked")
	e.clock.Advance(30 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.n.tick(ctx)
	if out := e.logs.String(); out != "" {
		t.Errorf("logged during shutdown: %s", out)
	}
}

func TestAlertMessageShowsPermission(t *testing.T) {
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	s := api.Session{ID: "0a1b2c3d-1111", Title: "deploy", Machine: "tower", Recap: "Ready to push."}
	it := api.InboxItem{Group: api.InboxBlocked, Since: at.Add(-time.Minute), Session: s,
		Permission: &api.PermissionRequest{ID: "pr_x", ToolName: "Bash",
			ToolInput: json.RawMessage(`{"command":"git push --force\u001b[31m","description":"Push"}`)}}
	text, buttons := alertMessage(it, at, "https://sessionhub.example.test")
	want := "⏸ Blocked: deploy\ntower · blocked 1m ago\nAsks to use Bash: git push --force[31m\nReady to push."
	if text != want {
		t.Errorf("text\n%q\nwant\n%q", text, want)
	}
	if len(buttons) != 1 || buttons[0].URL != "https://sessionhub.example.test/#inbox" {
		t.Errorf("buttons %+v, want Open inbox only", buttons)
	}
	// The input is cut to 300 characters; a long tool name to 40.
	it.Permission.ToolName = strings.Repeat("T", 50)
	it.Permission.ToolInput = json.RawMessage(`{"command":"` + strings.Repeat("x", 400) + `"}`)
	text, _ = alertMessage(it, at, "https://sessionhub.example.test")
	line := strings.Split(text, "\n")[2]
	if want := "Asks to use " + strings.Repeat("T", 39) + "…: " + strings.Repeat("x", 299) + "…"; line != want {
		t.Errorf("long request line\n%q\nwant\n%q", line, want)
	}
	if strings.Contains(line, "(cut)") {
		t.Errorf("an uncut request says cut: %q", line)
	}
	// A cut input says so after the shortened text.
	it.Permission.ToolName = "Write"
	it.Permission.ToolInput = json.RawMessage(`"{\"file_path\":\"/a/b.go\",\"content\":\"abc…"`)
	it.Permission.Truncated = true
	text, _ = alertMessage(it, at, "https://sessionhub.example.test")
	if line := strings.Split(text, "\n")[2]; line != `Asks to use Write: {"file_path":"/a/b.go","content":"abc… (cut)` {
		t.Errorf("cut request line %q", line)
	}
	// With the mod's question too, the alert shows both: the request line,
	// then the question in place of the recap.
	it.Session.BlockedOn = "Question: Push now?"
	text, _ = alertMessage(it, at, "https://sessionhub.example.test")
	if lines := strings.Split(text, "\n"); len(lines) != 4 || !strings.HasPrefix(lines[2], "Asks to use Write: ") || lines[3] != "Question: Push now?" {
		t.Errorf("request and question %q", text)
	}
	// A Waiting item never carries a request line.
	w := api.InboxItem{Group: api.InboxWaiting, Since: at.Add(-time.Minute), WaitingOn: []string{"review"}, Session: s}
	if text, _ := alertMessage(w, at, "https://sessionhub.example.test"); strings.Contains(text, "Asks to use") {
		t.Errorf("waiting text %q", text)
	}
}
