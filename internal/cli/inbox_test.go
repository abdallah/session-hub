package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
)

type fakeInbox struct {
	inbox     api.Inbox
	err       error
	reads     int
	dismissed []string // "id since"
	snoozed   []string // "id since until"
}

func (f *fakeInbox) Inbox(context.Context) (api.Inbox, error) {
	f.reads++
	return f.inbox, f.err
}

func (f *fakeInbox) DismissInbox(_ context.Context, id string, since time.Time) error {
	f.dismissed = append(f.dismissed, id+" "+since.Format(time.RFC3339Nano))
	return f.err
}

func (f *fakeInbox) SnoozeInbox(_ context.Context, id string, since, until time.Time) error {
	f.snoozed = append(f.snoozed, id+" "+since.Format(time.RFC3339Nano)+" "+until.Format(time.RFC3339))
	return f.err
}

func runInbox(t *testing.T, f *fakeInbox, width int, at time.Time, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := &env{inbox: f, out: &out, now: func() time.Time { return at }, width: func() int { return width }}
	err := e.inboxCmd(context.Background(), args)
	return out.String(), err
}

func inboxFixture() api.Inbox {
	ago := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	return api.Inbox{
		Items: []api.InboxItem{
			{Group: api.InboxBlocked, Since: ago(12), Session: api.Session{ID: "aaaaaaaa-1111", Title: "fix login",
				Machine: "tower", Status: api.StatusBlocked, Recap: "Asked which token to rotate."}},
			{Group: api.InboxWaiting, Since: ago(30), WaitingOn: []string{"review MR !12", "pick a name"},
				Session: api.Session{ID: "bbbbbbbb-2222", Title: "docs", Machine: "bluebox", Status: api.StatusLive}},
			{Group: api.InboxFinished, Since: ago(5), Session: api.Session{ID: "cccccccc-3333", Title: "ci \x1b[31mred",
				Machine: "tower", Status: api.StatusStale, Recap: "Tests pass.\nNext, deploy."}},
			{Group: api.InboxFinished, Since: ago(2), Session: api.Session{ID: "cccc0000-4444", Machine: "tower",
				Status: api.StatusLive, CWD: "/home/user/x"}},
		},
		Counts: api.InboxCounts{Blocked: 1, Waiting: 1, Finished: 2},
	}
}

func TestInboxList(t *testing.T) {
	f := &fakeInbox{inbox: inboxFixture()}
	out, err := runInbox(t, f, 200, now)
	if err != nil {
		t.Fatal(err)
	}
	want := `Blocked (1)
  aaaaaaaa  fix login  tower  12m  Asked which token to rotate.

Waiting on you (1)
  bbbbbbbb  docs  bluebox  30m  review MR !12; pick a name

Finished (2)
  cccccccc  ci [31mred  tower  5m  stale  Tests pass. Next, deploy.
  cccc0000  /home/user/x  tower  2m
`
	if out != want {
		t.Errorf("output:\n%s\nwant:\n%s", out, want)
	}
}

func TestInboxListCutsToWidth(t *testing.T) {
	out, err := runInbox(t, &fakeInbox{inbox: inboxFixture()}, 30, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if n := utf8.RuneCountInString(l); n > 30 {
			t.Errorf("line %q is %d runes, want at most 30", l, n)
		}
	}
	if !strings.Contains(out, "  aaaaaaaa  fix login  tower …\n") {
		t.Errorf("first item not cut with …:\n%s", out)
	}
}

func TestInboxListEmptyJSONAndErrors(t *testing.T) {
	out, err := runInbox(t, &fakeInbox{inbox: api.Inbox{Items: []api.InboxItem{}}}, 80, now)
	if err != nil || out != "nothing needs you\n" {
		t.Errorf("empty: %q %v", out, err)
	}

	out, err = runInbox(t, &fakeInbox{inbox: inboxFixture()}, 80, now, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got api.Inbox
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Items) != 4 || got.Counts.Finished != 2 {
		t.Errorf("--json: %v %+v", err, got)
	}

	if _, err := runInbox(t, &fakeInbox{err: errors.New("boom")}, 80, now); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("server error: %v", err)
	}
	for _, args := range [][]string{{"extra"}, {"--bogus"}} {
		if _, err := runInbox(t, &fakeInbox{}, 80, now, args...); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("args %v: %v, want a usage error", args, err)
		}
	}
}

func TestWidthOf(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notatty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, c := range []struct {
		columns string
		want    int
	}{{"77", 77}, {"", defaultWidth}, {"abc", defaultWidth}, {"-5", defaultWidth}, {"0", defaultWidth}} {
		got := widthOf(f.Fd(), func(k string) string {
			if k == "COLUMNS" {
				return c.columns
			}
			return ""
		})
		if got != c.want {
			t.Errorf("COLUMNS=%q on a file: %d, want %d", c.columns, got, c.want)
		}
	}
}

func TestParseSnooze(t *testing.T) {
	zone := time.FixedZone("CEST", 2*60*60)
	late := time.Date(2026, 9, 30, 23, 30, 0, 0, zone)
	early := time.Date(2026, 10, 1, 0, 30, 0, 0, zone)
	ok := []struct {
		arg  string
		now  time.Time
		want time.Time
	}{
		{"1h", late, late.Add(time.Hour)},
		{"4h", late, late.Add(4 * time.Hour)},
		{"90m", late, late.Add(90 * time.Minute)},
		{"167h59m", late, late.Add(167*time.Hour + 59*time.Minute)},
		{"tomorrow", late, time.Date(2026, 10, 1, 9, 0, 0, 0, zone)},
		{"tomorrow", early, time.Date(2026, 10, 2, 9, 0, 0, 0, zone)},
	}
	for _, c := range ok {
		got, err := parseSnooze(c.arg, c.now)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("parseSnooze(%q, %v) = %v %v, want %v", c.arg, c.now, got, err, c.want)
		}
	}
	for _, bad := range []string{"168h", "169h", "0s", "-1h", "soon", ""} {
		if _, err := parseSnooze(bad, late); err == nil {
			t.Errorf("parseSnooze(%q) accepted", bad)
		}
	}
}

func TestInboxDismiss(t *testing.T) {
	f := &fakeInbox{inbox: inboxFixture()}
	out, err := runInbox(t, f, 80, now, "dismiss", "bbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.dismissed) != 1 || f.dismissed[0] != "bbbbbbbb-2222 2026-09-30T11:30:00Z" {
		t.Errorf("dismissed %v", f.dismissed)
	}
	if out != "dismissed bbbbbbbb (docs)\n" {
		t.Errorf("output %q", out)
	}

	// A full ID resolves even when it is a prefix of nothing else.
	f = &fakeInbox{inbox: inboxFixture()}
	if _, err := runInbox(t, f, 80, now, "dismiss", "cccc0000-4444"); err != nil || len(f.dismissed) != 1 {
		t.Errorf("full ID: %v %v", f.dismissed, err)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"dismiss", "cccc"}, "matches 2"},
		{[]string{"dismiss", "dddd"}, "not in the inbox"},
		{[]string{"dismiss", "abc"}, "shorter than 4"},
		{[]string{"dismiss"}, "usage:"},
		{[]string{"dismiss", "aaaa", "bbbb"}, "usage:"},
	} {
		f := &fakeInbox{inbox: inboxFixture()}
		_, err := runInbox(t, f, 80, now, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
		if len(f.dismissed) != 0 {
			t.Errorf("%v: sent a dismiss", c.args)
		}
	}
	// The ambiguous error names both candidates.
	_, err = runInbox(t, &fakeInbox{inbox: inboxFixture()}, 80, now, "dismiss", "cccc")
	if !strings.Contains(err.Error(), "cccccccc-3333") || !strings.Contains(err.Error(), "cccc0000-4444") {
		t.Errorf("ambiguous: %v", err)
	}
}

func TestInboxSnooze(t *testing.T) {
	f := &fakeInbox{inbox: inboxFixture()}
	out, err := runInbox(t, f, 80, now, "snooze", "aaaa", "90m")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.snoozed) != 1 || f.snoozed[0] != "aaaaaaaa-1111 2026-09-30T11:48:00Z 2026-09-30T13:30:00Z" {
		t.Errorf("snoozed %v", f.snoozed)
	}
	if out != "snoozed aaaaaaaa (fix login) until Wed 30 Sep 13:30\n" {
		t.Errorf("output %q", out)
	}

	for _, args := range [][]string{
		{"snooze", "aaaa", "168h"},
		{"snooze", "aaaa", "soon"},
		{"snooze", "aaaa"},
	} {
		f := &fakeInbox{inbox: inboxFixture()}
		if _, err := runInbox(t, f, 80, now, args...); err == nil {
			t.Errorf("%v accepted", args)
		}
		if f.reads != 0 || len(f.snoozed) != 0 {
			t.Errorf("%v: read the inbox %d times and snoozed %v before checking the time", args, f.reads, f.snoozed)
		}
	}
	f = &fakeInbox{inbox: inboxFixture()}
	if _, err := runInbox(t, f, 80, now, "snooze", "dddd", "1h"); err == nil || !strings.Contains(err.Error(), "not in the inbox") {
		t.Errorf("unknown item: %v", err)
	}
}

func TestIsTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notatty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f.Fd()) {
		t.Error("a regular file is a terminal")
	}
}

func TestWantsWatch(t *testing.T) {
	for args, want := range map[string]bool{"--watch": true, "-watch": true, "--watch=true": true, "--json": false, "dismiss": false, "watch": false} {
		if got := wantsWatch([]string{args}); got != want {
			t.Errorf("wantsWatch(%q) = %v, want %v", args, got, want)
		}
	}
}

func TestFailedJumper(t *testing.T) {
	_, err := failedJumper{errors.New("no config")}.Jump(context.Background(), api.Session{})
	if err == nil || !strings.Contains(err.Error(), "no config") {
		t.Errorf("err = %v", err)
	}
}
