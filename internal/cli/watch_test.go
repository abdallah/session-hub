package cli

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestParseKeys(t *testing.T) {
	got := parseKeys([]byte("jk\x1b[A\x1b[B\x1bOA\r\n\x03q\x1bs\x01"))
	want := []key{"j", "k", keyUp, keyDown, keyUp, keyEnter, keyEnter, keyCtrlC, "q", keyEsc, "s"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseKeys = %v, want %v", got, want)
	}
}

// press applies keys in order and returns the state and the last action.
func press(s watchState, keys ...key) (watchState, watchAction) {
	var a watchAction
	for _, k := range keys {
		s, a = handleKey(s, k, now)
	}
	return s, a
}

func TestWatchKeys(t *testing.T) {
	start := watchState{}.withInbox(inboxFixture().Items, now)
	if start.selected != "aaaaaaaa-1111" {
		t.Fatalf("first selection %q, want the first row", start.selected)
	}
	tomorrow9 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC) // now is 2026-09-30 12:00 UTC
	cases := []struct {
		name     string
		keys     []key
		selected string
		kind     actionKind
		item     string
		until    time.Time
	}{
		{"j moves down", []key{"j"}, "bbbbbbbb-2222", actNone, "", time.Time{}},
		{"down arrow", []key{keyDown, keyDown}, "cccccccc-3333", actNone, "", time.Time{}},
		{"stops at the last row", []key{"j", "j", "j", "j", "j"}, "cccc0000-4444", actNone, "", time.Time{}},
		{"k and up stop at the first row", []key{"j", "k", keyUp}, "aaaaaaaa-1111", actNone, "", time.Time{}},
		{"Enter jumps to the cursor row", []key{"j", keyEnter}, "bbbbbbbb-2222", actJump, "bbbbbbbb-2222", time.Time{}},
		{"d dismisses", []key{"d"}, "aaaaaaaa-1111", actDismiss, "aaaaaaaa-1111", time.Time{}},
		{"s 1 snoozes an hour", []key{"s", "1"}, "aaaaaaaa-1111", actSnooze, "aaaaaaaa-1111", now.Add(time.Hour)},
		{"s 4 snoozes four hours", []key{"j", "s", "4"}, "bbbbbbbb-2222", actSnooze, "bbbbbbbb-2222", now.Add(4 * time.Hour)},
		{"s t snoozes until 9:00 tomorrow", []key{"s", "t"}, "aaaaaaaa-1111", actSnooze, "aaaaaaaa-1111", tomorrow9},
		{"s Esc cancels", []key{"s", keyEsc}, "aaaaaaaa-1111", actNone, "", time.Time{}},
		{"s x cancels", []key{"s", "x"}, "aaaaaaaa-1111", actNone, "", time.Time{}},
		{"r refreshes", []key{"r"}, "aaaaaaaa-1111", actRefresh, "", time.Time{}},
		{"q quits", []key{"q"}, "aaaaaaaa-1111", actQuit, "", time.Time{}},
		{"Ctrl+C quits", []key{keyCtrlC}, "aaaaaaaa-1111", actQuit, "", time.Time{}},
		{"q quits during a snooze", []key{"s", "q"}, "aaaaaaaa-1111", actQuit, "", time.Time{}},
	}
	for _, c := range cases {
		s, a := press(start, c.keys...)
		if s.selected != c.selected || a.kind != c.kind || a.item.Session.ID != c.item || !a.until.Equal(c.until) {
			t.Errorf("%s: selected %q action %v item %q until %v, want %q %v %q %v",
				c.name, s.selected, a.kind, a.item.Session.ID, a.until, c.selected, c.kind, c.item, c.until)
		}
	}
	// A key after s ends the snooze prompt and does nothing else.
	s, _ := press(start, "s")
	if !s.snoozing || !strings.Contains(s.status, "1 = 1 hour") {
		t.Fatalf("after s: snoozing=%v status %q", s.snoozing, s.status)
	}
	if s, _ = press(s, "j"); s.snoozing || s.selected != "aaaaaaaa-1111" || s.status != "snooze cancelled" {
		t.Errorf("j after s: snoozing=%v selected %q status %q, want a cancel and no move", s.snoozing, s.selected, s.status)
	}
	// With no items, no key acts.
	empty := watchState{}.withInbox(nil, now)
	for _, k := range []key{"j", "k", keyEnter, "d", "s"} {
		if s, a := handleKey(empty, k, now); a.kind != actNone || s.snoozing {
			t.Errorf("%q on an empty inbox: action %v snoozing %v", k, a.kind, s.snoozing)
		}
	}
}

func TestWatchKeepsCursor(t *testing.T) {
	items := inboxFixture().Items // a, b, c, c0
	s, _ := press(watchState{}.withInbox(items, now), "j")
	if s = s.withInbox(items[1:], now); s.selected != "bbbbbbbb-2222" {
		t.Errorf("another row left: selected %q, want b", s.selected)
	}
	if s = s.withInbox(items[2:], now); s.selected != "cccccccc-3333" {
		t.Errorf("the selected row left: selected %q, want the row now at its position", s.selected)
	}
	s, _ = press(s, "j") // c0, index 1 of [c, c0]
	if s = s.withInbox(items[:1], now); s.selected != "aaaaaaaa-1111" {
		t.Errorf("the selected row left past the end: selected %q, want the last row", s.selected)
	}
	if s = s.withInbox(nil, now); s.selected != "" {
		t.Errorf("empty inbox: selected %q", s.selected)
	}
	if _, a := handleKey(s, keyEnter, now); a.kind != actNone {
		t.Errorf("Enter on an empty inbox: %v", a.kind)
	}
	if s = s.withInbox(items, now); s.selected != "aaaaaaaa-1111" {
		t.Errorf("items again: selected %q, want the first row", s.selected)
	}
}

func TestRenderWatch(t *testing.T) {
	s, _ := press(watchState{}.withInbox(inboxFixture().Items, now), "j")
	want := []string{
		watchHelp,
		"Blocked (1)",
		"  aaaaaaaa  fix login  tower  12m  Asked which token to rotate.",
		"",
		"Waiting on you (1)",
		"> bbbbbbbb  docs  bluebox  30m  review MR !12; pick a name",
		"",
		"Finished (2)",
		"  cccccccc  ci [31mred  tower  5m  stale  Tests pass. Next, deploy.",
		"  cccc0000  /home/user/x  tower  2m",
		"",
		"updated 12:00:00",
	}
	if got := renderWatch(s, now, 200, 12); !reflect.DeepEqual(got, want) {
		t.Errorf("render:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Too short: the window scrolls to keep the cursor row in view.
	s, _ = press(s, "j", "j")
	want = []string{
		watchHelp,
		"Finished (2)",
		"  cccccccc  ci [31mred  tower  5m  stale  Tests pass. Next, deploy.",
		"> cccc0000  /home/user/x  tower  2m",
		"updated 12:00:00",
	}
	if got := renderWatch(s, now, 200, 5); !reflect.DeepEqual(got, want) {
		t.Errorf("scrolled:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Narrow: every line fits.
	for i, l := range renderWatch(s, now, 30, 12) {
		if n := len([]rune(l)); n > 30 {
			t.Errorf("line %d is %d runes wide at width 30: %q", i, n, l)
		}
	}
	// Before the first read; empty after a read; status and a cleaned error.
	if got := renderWatch(watchState{}, now, 80, 4); !reflect.DeepEqual(got, []string{watchHelp, "", "", "loading..."}) {
		t.Errorf("loading: %q", got)
	}
	e := watchState{}.withInbox(nil, now)
	if got := renderWatch(e, now, 80, 4); !reflect.DeepEqual(got, []string{watchHelp, "nothing needs you", "", "updated 12:00:00"}) {
		t.Errorf("empty: %q", got)
	}
	e.status, e.errText = "dismissed aaaaaaaa", "refresh: down\x1b[2J"
	if got := renderWatch(e, now, 80, 4)[3]; got != "updated 12:00:00  dismissed aaaaaaaa  error: refresh: down[2J" {
		t.Errorf("status line %q", got)
	}
}

type fakeJumper struct {
	ids []string
	msg string
	err error
}

func (j *fakeJumper) Jump(_ context.Context, s api.Session) (string, error) {
	j.ids = append(j.ids, s.ID)
	return j.msg, j.err
}

// runLoop runs watchLoop over the inputs, one channel element per string,
// then closes the input, and returns the frames it drew.
func runLoop(t *testing.T, e *env, inputs ...string) [][]string {
	t.Helper()
	keys := make(chan []byte, len(inputs))
	for _, in := range inputs {
		keys <- []byte(in)
	}
	close(keys)
	var frames [][]string
	err := e.watchLoop(context.Background(), watchIO{keys: keys,
		draw: func(l []string) { frames = append(frames, l) },
		size: func() (int, int) { return 200, 12 }})
	if err != nil {
		t.Fatalf("watchLoop: %v", err)
	}
	return frames
}

func lastStatus(frames [][]string) string {
	f := frames[len(frames)-1]
	return f[len(f)-1]
}

func TestWatchLoopDismissAndSnooze(t *testing.T) {
	f := &fakeInbox{inbox: inboxFixture()}
	e := &env{inbox: f, now: func() time.Time { return now }}
	frames := runLoop(t, e, "j", "d", "s", "1", "q", "j")
	wantDismiss := "bbbbbbbb-2222 " + now.Add(-30*time.Minute).Format(time.RFC3339Nano)
	if !reflect.DeepEqual(f.dismissed, []string{wantDismiss}) {
		t.Errorf("dismissed %v, want %v", f.dismissed, wantDismiss)
	}
	wantSnooze := wantDismiss + " " + now.Add(time.Hour).Format(time.RFC3339)
	if !reflect.DeepEqual(f.snoozed, []string{wantSnooze}) {
		t.Errorf("snoozed %v, want %v", f.snoozed, wantSnooze)
	}
	if got := lastStatus(frames); got != "updated 12:00:00  snoozed bbbbbbbb until Wed 30 Sep 13:00" {
		t.Errorf("status %q", got)
	}
	if f.reads != 3 { // the first read, then one after each action; q stops before the last j
		t.Errorf("%d inbox reads, want 3", f.reads)
	}
}

func TestWatchLoopJumpAndErrors(t *testing.T) {
	inbox := inboxFixture()
	inbox.Items[0].Session.ResumeCommand = "ssh -t tower '~/.local/bin/sessionhub resume aaaaaaaa-1111'"
	f := &fakeInbox{inbox: inbox}
	e := &env{inbox: f, now: func() time.Time { return now }}
	// No jumper: Enter shows the resume command.
	if got := lastStatus(runLoop(t, e, "\r")); got != "updated 12:00:00  run: ssh -t tower '~/.local/bin/sessionhub resume aaaaaaaa-1111'" {
		t.Errorf("without a jumper: %q", got)
	}
	j := &fakeJumper{msg: `Focused pane "w1:p1"`}
	e.jump = j
	frames := runLoop(t, e, "j", "\r")
	if !reflect.DeepEqual(j.ids, []string{"bbbbbbbb-2222"}) || lastStatus(frames) != `updated 12:00:00  Focused pane "w1:p1"` {
		t.Errorf("jump: ids %v status %q", j.ids, lastStatus(frames))
	}
	// The frame drawn before a slow jump says so.
	if got := frames[len(frames)-2]; got[len(got)-1] != "updated 12:00:00  jumping to bbbbbbbb..." {
		t.Errorf("frame before the jump: %q", got[len(got)-1])
	}
	j.err = errors.New("ssh: connect timed out")
	if got := lastStatus(runLoop(t, e, "\r")); got != "updated 12:00:00  error: jump aaaaaaaa: ssh: connect timed out" {
		t.Errorf("jump error: %q", got)
	}
	// A failed refresh keeps the rows and shows the error.
	fl := &flakyInbox{fakeInbox{inbox: inboxFixture()}}
	frames = runLoop(t, &env{inbox: fl, now: func() time.Time { return now }}, "r")
	last := frames[len(frames)-1]
	if last[2] != "> aaaaaaaa  fix login  tower  12m  Asked which token to rotate." ||
		last[len(last)-1] != "updated 12:00:00  error: refresh: server down" {
		t.Errorf("refresh error frame:\n%s", strings.Join(last, "\n"))
	}
}

// flakyInbox answers the first read and fails every later one.
type flakyInbox struct{ fakeInbox }

func (f *flakyInbox) Inbox(context.Context) (api.Inbox, error) {
	f.reads++
	if f.reads > 1 {
		return api.Inbox{}, errors.New("server down")
	}
	return f.inbox, nil
}

func TestWatchLoopFirstReadFails(t *testing.T) {
	e := &env{inbox: &fakeInbox{err: errors.New("server down")}, now: func() time.Time { return now }}
	frames := runLoop(t, e)
	if got := lastStatus(frames); got != "loading...  error: refresh: server down" {
		t.Errorf("status %q", got)
	}
}

func TestRawTerminalRestores(t *testing.T) {
	var calls []string
	stty := func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if len(args) == 1 && args[0] == "-g" {
			return "500:5:bf:8a3b\n", nil
		}
		return "", nil
	}
	want := []string{"-g", "raw -echo", "500:5:bf:8a3b"}
	boom := errors.New("boom")
	for _, c := range []struct {
		fn      func() error
		wantErr error
	}{
		{func() error { return nil }, nil},
		{func() error { return boom }, boom},
	} {
		calls = nil
		var out bytes.Buffer
		if err := withRawTerminal(stty, &out, c.fn); err != c.wantErr {
			t.Errorf("withRawTerminal returned %v, want %v", err, c.wantErr)
		}
		if !reflect.DeepEqual(calls, want) || out.String() != enterScreen+leaveScreen {
			t.Errorf("stty calls %q, output %q", calls, out.String())
		}
	}
	calls = nil
	var out bytes.Buffer
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed")
			}
		}()
		withRawTerminal(stty, &out, func() error { panic("boom") })
	}()
	if !reflect.DeepEqual(calls, want) || out.String() != enterScreen+leaveScreen {
		t.Errorf("after a panic: stty calls %q, output %q", calls, out.String())
	}
	// stty -g fails: nothing changes and fn never runs.
	calls = nil
	out.Reset()
	failing := func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return "", errors.New("not a tty")
	}
	err := withRawTerminal(failing, &out, func() error { t.Error("fn ran"); return nil })
	if err == nil || !reflect.DeepEqual(calls, []string{"-g"}) || out.Len() != 0 {
		t.Errorf("stty failure: err %v calls %q output %q", err, calls, out.String())
	}
}

func TestInboxWatchNeedsTerminal(t *testing.T) {
	f := &fakeInbox{inbox: inboxFixture()}
	if _, err := runInbox(t, f, 100, now, "--watch"); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("--watch without a terminal: %v", err)
	}
	if _, err := runInbox(t, f, 100, now, "--watch", "--json"); err == nil || !strings.Contains(err.Error(), "--json") {
		t.Errorf("--watch --json: %v", err)
	}
	if f.reads != 0 {
		t.Errorf("%d inbox reads, want 0", f.reads)
	}
}

func TestWatchSnoozeCancelsWhenRowChanges(t *testing.T) {
	items := inboxFixture().Items
	s, _ := press(watchState{}.withInbox(items, now), "s")
	s = s.withInbox(items[1:], now) // a left; the cursor moved to b
	s, a := press(s, "1")
	if a.kind != actNone || s.status != "snooze cancelled" || s.snoozing {
		t.Errorf("snooze after the row changed: action %v status %q snoozing %v", a.kind, s.status, s.snoozing)
	}
}

// blockingInbox blocks every read until its context ends.
type blockingInbox struct {
	fakeInbox
	started chan struct{}
}

func (b *blockingInbox) Inbox(ctx context.Context) (api.Inbox, error) {
	b.started <- struct{}{}
	<-ctx.Done()
	return api.Inbox{}, ctx.Err()
}

func TestWatchQuitsDuringSlowRequest(t *testing.T) {
	for _, quit := range []string{"q", "\x03"} {
		b := &blockingInbox{started: make(chan struct{}, 1)}
		e := &env{inbox: b, now: func() time.Time { return now }}
		keys := make(chan []byte)
		errc := make(chan error, 1)
		go func() {
			errc <- e.watchLoop(context.Background(), watchIO{keys: keys,
				draw: func([]string) {}, size: func() (int, int) { return 80, 10 }})
		}()
		<-b.started // the first read is in flight
		keys <- []byte(quit)
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("%q: watchLoop returned %v, want nil", quit, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%q did not quit during a blocked read", quit)
		}
	}
}

func TestWatchLoopSignalReturnsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := &env{inbox: &fakeInbox{inbox: inboxFixture()}, now: func() time.Time { return now }}
	err := e.watchLoop(ctx, watchIO{draw: func([]string) {}, size: func() (int, int) { return 80, 10 }})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("watchLoop on a cancelled context returned %v, want context.Canceled", err)
	}
}

func TestRawTerminalRawFails(t *testing.T) {
	var calls []string
	stty := func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "-g" {
			return "500:5\n", nil
		}
		return "", errors.New("stty: failed")
	}
	var out bytes.Buffer
	err := withRawTerminal(stty, &out, func() error { t.Error("fn ran"); return nil })
	if err == nil || !reflect.DeepEqual(calls, []string{"-g", "raw -echo", "500:5"}) || out.Len() != 0 {
		t.Errorf("raw failure: err %v calls %q output %q", err, calls, out.String())
	}
}
