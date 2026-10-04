package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
)

// Telegram alert timing and limits. See docs/server.md, "Telegram alerts".
const (
	alertEvery       = 15 * time.Second // notifier tick
	alertGrace       = 30 * time.Second // an item younger than this never alerts
	maxAlertsPerTick = 10
	alertLogEvery    = time.Minute // at most one failure line per minute
	alertTitleRunes  = 120
	alertDetailRunes = 300
	// alertToolRunes caps the tool name in the request line.
	alertToolRunes    = 40
	telegramTimeout   = 10 * time.Second
	defaultRetryAfter = 30 * time.Second // HTTP 429 without retry_after
	staleItemAfter    = time.Hour        // on the first tick, an older item is recorded, not sent
	ruleQueueSize     = 16               // rule-added alerts waiting for the notifier
	alertRuleRunes    = 300
)

// telegramAPI is the Bot API base URL. Tests point a notifier's apiBase at a
// fake server instead.
var telegramAPI = "https://api.telegram.org"

// alertStore is the part of *store.Store the notifier uses.
type alertStore interface {
	Now() time.Time
	Inbox(ctx context.Context) (api.Inbox, error)
	LastAlerts(ctx context.Context) (map[string]time.Time, error)
	RecordAlert(ctx context.Context, id string, since time.Time) error
}

// notifier sends one Telegram message for each new Blocked or Waiting inbox
// item. It
// only calls sendMessage, so it never reads the bot's updates and never
// competes with another program that does.
type notifier struct {
	st        alertStore
	publicURL string
	token     string
	chatID    string
	apiBase   string
	http      *http.Client
	log       *log.Logger
	notBefore time.Time            // after HTTP 429, no send before this
	lastLog   time.Time            // when the last failure line was written
	ticked    bool                 // the first tick has read the inbox
	rules     chan api.Instruction // rules added, waiting for a rule-added alert
}

// rejectedError is a Bot API answer other than success or HTTP 429: the
// request reached Telegram and Telegram refused this message. The next
// message can still succeed, so the tick goes on.
type rejectedError struct{ msg string }

func (e *rejectedError) Error() string { return e.msg }

func newNotifier(st alertStore, cfg Config, logger *log.Logger) *notifier {
	return &notifier{
		st:        st,
		publicURL: strings.TrimRight(cfg.PublicURL, "/"),
		token:     string(cfg.TelegramBotToken),
		chatID:    cfg.TelegramChatID,
		apiBase:   telegramAPI,
		// No redirects: a redirect error would quote the request URL.
		http: &http.Client{Timeout: telegramTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		log:   logger,
		rules: make(chan api.Instruction, ruleQueueSize),
	}
}

// startAlerts starts the notifier when both Telegram settings are set, or
// logs once that alerts are off. stop cancels the notifier and waits for it
// to return.
//
// onRule hands a stored rule to the notifier without blocking. It does
// nothing when alerts are off.
func startAlerts(ctx context.Context, st alertStore, cfg Config, logger *log.Logger) (stop func(), onRule func(api.Instruction)) {
	if cfg.TelegramProblem != "" {
		logger.Printf("telegram alerts are off: %s", cfg.TelegramProblem)
		return func() {}, func(api.Instruction) {}
	}
	if !cfg.AlertsEnabled() {
		logger.Printf("telegram alerts are off: set telegram_bot_token and telegram_chat_id in server.toml to turn them on")
		return func() {}, func(api.Instruction) {}
	}
	ctx, cancel := context.WithCancel(ctx)
	n := newNotifier(st, cfg, logger)
	t := time.NewTicker(alertEvery)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer t.Stop()
		n.run(ctx, t.C)
	}()
	logger.Printf("telegram alerts are on: checking the inbox every %s", alertEvery)
	return func() {
		cancel()
		<-done
	}, n.enqueueRule
}

// run ticks at once, then on every tick, until ctx ends.
func (n *notifier) run(ctx context.Context, ticks <-chan time.Time) {
	n.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			n.tick(ctx)
		case r := <-n.rules:
			n.sendRule(ctx, r)
		}
	}
}

// alertable reports whether an inbox group alerts. Finished items stay silent.
func alertable(group string) bool {
	return group == api.InboxBlocked || group == api.InboxWaiting
}

// tick sends the alerts that are due and returns how many it sent. An item
// is due when it is Blocked or Waiting, is alertGrace old, and has a since
// later than the last alert for its session. Triage already hides dismissed
// and snoozed items. On the first tick after start, an alertable item that is
// more than staleItemAfter old is recorded without a
// send, so a deploy does not burst stale alerts. A network error or HTTP 429
// ends the tick; any other refusal is logged and the tick goes on with the
// next item, leaving the refused item unrecorded for a retry on the next tick.
func (n *notifier) tick(ctx context.Context) int {
	now := n.st.Now()
	if now.Before(n.notBefore) {
		return 0
	}
	in, err := n.st.Inbox(ctx)
	if err != nil {
		n.logf(ctx, now, "telegram alerts: read the inbox: %v", err)
		return 0
	}
	last, err := n.st.LastAlerts(ctx)
	if err != nil {
		n.logf(ctx, now, "telegram alerts: read the last alerts: %v", err)
		return 0
	}
	if !n.ticked {
		n.ticked = true
		for _, it := range in.Items {
			if !alertable(it.Group) || now.Sub(it.Since) <= staleItemAfter {
				continue
			}
			if t, ok := last[it.Session.ID]; ok && !it.Since.After(t) {
				continue
			}
			if err := n.st.RecordAlert(ctx, it.Session.ID, it.Since); err != nil {
				n.logf(ctx, now, "telegram alerts: record the alert for session %s: %v", it.Session.ID, err)
				continue
			}
			last[it.Session.ID] = it.Since
		}
	}
	sent, attempts := 0, 0
	for _, it := range in.Items {
		if !alertable(it.Group) || now.Sub(it.Since) < alertGrace {
			continue
		}
		if t, ok := last[it.Session.ID]; ok && !it.Since.After(t) {
			continue
		}
		if attempts == maxAlertsPerTick {
			break // a refused chat must not cost a request per item every tick
		}
		attempts++
		text, buttons := alertMessage(it, now, n.publicURL)
		retry, err := n.send(ctx, text, buttons)
		if err != nil {
			var rej *rejectedError
			if errors.As(err, &rej) {
				n.logf(ctx, now, "telegram alerts: Telegram refused the message for session %s, retrying on a later tick: %v", it.Session.ID, err)
				continue
			}
			if retry > 0 {
				n.notBefore = now.Add(retry)
			}
			n.logf(ctx, now, "telegram alerts: send failed, retrying on a later tick: %v", err)
			return sent
		}
		if err := n.st.RecordAlert(ctx, it.Session.ID, it.Since); err != nil {
			n.logf(ctx, now, "telegram alerts: record the alert for session %s: %v", it.Session.ID, err)
		}
		sent++
	}
	return sent
}

// logf writes one failure line at most every alertLogEvery. The line never
// holds the token, even when an error quotes it.
func (n *notifier) logf(ctx context.Context, now time.Time, format string, args ...any) {
	if ctx.Err() != nil {
		return // shutting down: the failure is the cancellation
	}
	if !n.lastLog.IsZero() && now.Sub(n.lastLog) < alertLogEvery {
		return
	}
	n.lastLog = now
	line := fmt.Sprintf(format, args...)
	if n.token != "" {
		line = strings.ReplaceAll(line, n.token, "[redacted]")
	}
	n.log.Print(line)
}

// inlineButton is one Telegram URL button.
type inlineButton struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

type replyMarkup struct {
	InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
}

// sendMessageIn is the sendMessage body. It sets no parse_mode, so the text
// is plain.
type sendMessageIn struct {
	ChatID      string       `json:"chat_id"`
	Text        string       `json:"text"`
	ReplyMarkup *replyMarkup `json:"reply_markup,omitempty"`
}

// telegramOut is the part of a Bot API response the notifier reads.
type telegramOut struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// send posts one message, one button per row. retry is how long to wait
// before the next send after HTTP 429, else 0. No error it returns holds
// the request URL, which holds the token.
func (n *notifier) send(ctx context.Context, text string, buttons []inlineButton) (retry time.Duration, err error) {
	in := sendMessageIn{ChatID: n.chatID, Text: text}
	if len(buttons) > 0 {
		rm := &replyMarkup{}
		for _, b := range buttons {
			rm.InlineKeyboard = append(rm.InlineKeyboard, []inlineButton{b})
		}
		in.ReplyMarkup = rm
	}
	body, err := json.Marshal(in)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.apiBase+"/bot"+n.token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("build the sendMessage request") // the parse error quotes the URL
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // *url.Error's text holds the URL
		}
		return 0, err
	}
	defer resp.Body.Close()
	var out telegramOut
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(b, &out)
	if resp.StatusCode == http.StatusTooManyRequests {
		retry = time.Duration(out.Parameters.RetryAfter) * time.Second
		if retry <= 0 {
			retry = defaultRetryAfter
		}
		return retry, fmt.Errorf("HTTP 429, waiting %s", retry)
	}
	if resp.StatusCode != http.StatusOK || !out.OK {
		return 0, &rejectedError{fmt.Sprintf("HTTP %d: %s", resp.StatusCode, termtext.Clean(strings.ReplaceAll(out.Description, n.token, "[redacted]"), 200))}
	}
	return 0, nil
}

// alertMessage is the text and buttons for one Blocked or Waiting item. Every server
// string is cleaned first. The Remote Control button needs an https link:
// Telegram rejects other URL buttons.
func alertMessage(it api.InboxItem, now time.Time, publicURL string) (string, []inlineButton) {
	s := it.Session
	title := termtext.Clean(s.Title, alertTitleRunes)
	if title == "" {
		title = termtext.Clean(s.CWD, alertTitleRunes)
	}
	if title == "" {
		id := []rune(termtext.Clean(s.ID, 0))
		if len(id) > 8 {
			id = id[:8]
		}
		title = "session " + string(id)
	}
	head, verb := "⏸ Blocked: ", "blocked"
	if it.Group == api.InboxWaiting {
		head, verb = "💬 Waiting on you: ", "waiting"
	}
	lines := []string{
		head + title,
		termtext.Clean(s.Machine, 0) + " · " + verb + " " + alertAge(now.Sub(it.Since)) + " ago",
	}
	if p := it.Permission; p != nil && it.Group == api.InboxBlocked {
		line := "Asks to use " + termtext.Clean(p.ToolName, alertToolRunes) + ": " +
			termtext.Clean(api.PermissionInputText(p.ToolInput), alertDetailRunes)
		if p.Truncated {
			line += " (cut)"
		}
		lines = append(lines, line)
	}
	if d := termtext.Clean(alertDetail(it), alertDetailRunes); d != "" {
		lines = append(lines, d)
	}
	buttons := []inlineButton{{Text: "Open inbox", URL: publicURL + "/#inbox"}}
	if u := s.RemoteControlURL; strings.HasPrefix(u, "https://") && termtext.Clean(u, 0) == u {
		buttons = append(buttons, inlineButton{Text: "Remote Control", URL: u})
	}
	return strings.Join(lines, "\n"), buttons
}

// alertDetail is what the session waits on: the item's waiting_on, else the
// latest report's when no prompt came after that report, else the recap.
func alertDetail(it api.InboxItem) string {
	if len(it.WaitingOn) > 0 {
		return strings.Join(it.WaitingOn, "; ")
	}
	s := it.Session
	if r := s.LatestReport; r != nil && len(r.WaitingOn) > 0 && (s.LastPromptAt == nil || !s.LastPromptAt.After(r.TS)) {
		return strings.Join(r.WaitingOn, "; ")
	}
	return s.Recap
}

// alertAge is a short duration: 45s, 12m, 3h, or 2d. It mirrors cli.age.
func alertAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// enqueueRule hands a stored rule to the notifier. It never blocks: when the
// queue is full it drops the alert and logs one line.
func (n *notifier) enqueueRule(r api.Instruction) {
	select {
	case n.rules <- r:
	default:
		n.logf(context.Background(), n.st.Now(), "telegram alerts: too many rule alerts waiting, dropped the alert for rule %d", r.ID)
	}
}

// sendRule sends the alert for one added rule: one attempt, no retry. Inside
// an HTTP 429 wait it sends nothing. A failure is logged like any other.
func (n *notifier) sendRule(ctx context.Context, r api.Instruction) {
	now := n.st.Now()
	if now.Before(n.notBefore) {
		n.logf(ctx, now, "telegram alerts: waiting out HTTP 429, no alert for rule %d", r.ID)
		return
	}
	text, buttons := ruleMessage(r, n.publicURL)
	retry, err := n.send(ctx, text, buttons)
	if err == nil {
		return
	}
	if retry > 0 {
		n.notBefore = now.Add(retry)
	}
	n.logf(ctx, now, "telegram alerts: send failed for the alert for rule %d: %v", r.ID, err)
}

// ruleMessage is the text and button for an added rule. The text and the
// author are cleaned, because a session can write both.
func ruleMessage(r api.Instruction, publicURL string) (string, []inlineButton) {
	lines := []string{
		"📌 New rule for all sessions",
		termtext.Clean(r.Text, alertRuleRunes),
		"added by " + termtext.Clean(r.CreatedBy, 0) + " · rule " + strconv.FormatInt(r.ID, 10),
	}
	return strings.Join(lines, "\n"), []inlineButton{{Text: "Open rules", URL: publicURL + "/#rules"}}
}
