package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

const (
	startID   = "st_AAAAAAAAAAAAAAAAAAAAAA"
	startLink = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"
)

// fakeStarts is a startAPI. states is what GetStart returns, one per read;
// the last one repeats.
type fakeStarts struct {
	createErr error
	created   []string // "machine|dir|prompt", and "|trust" with trust set
	states    []api.StartRequest
	reads     int
	getErr    error
}

func (f *fakeStarts) CreateStart(_ context.Context, machine string, in api.StartIn) (api.StartRequest, error) {
	c := machine + "|" + in.Dir + "|" + in.Prompt
	if in.Trust {
		c += "|trust"
	}
	f.created = append(f.created, c)
	if f.createErr != nil {
		return api.StartRequest{}, f.createErr
	}
	return api.StartRequest{ID: startID, Machine: machine, Dir: in.Dir, State: api.ControlPending}, nil
}

func (f *fakeStarts) GetStart(context.Context, string) (api.StartRequest, error) {
	if f.getErr != nil {
		return api.StartRequest{}, f.getErr
	}
	i := min(f.reads, len(f.states)-1)
	f.reads++
	return f.states[i], nil
}

func runStart(t *testing.T, f *fakeStarts, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	clock := now
	e := &env{cfg: client.Config{Machine: "tower"}, starts: f, out: &out,
		getwd: func() (string, error) { return "/home/me/Code/app", nil },
		now:   func() time.Time { return clock }, pause: func(d time.Duration) { clock = clock.Add(d) }}
	err := e.start(context.Background(), args)
	return out.String(), err
}

func startState(s string, mod func(*api.StartRequest)) api.StartRequest {
	r := api.StartRequest{ID: startID, Machine: "bluebox", Dir: "~", State: s}
	if mod != nil {
		mod(&r)
	}
	return r
}

func TestStartFollowsToDone(t *testing.T) {
	f := &fakeStarts{states: []api.StartRequest{startState(api.ControlClaimed, nil),
		startState(api.ControlDone, func(r *api.StartRequest) { r.URL = startLink; r.Detail = "started in a new herdr workspace" })}}
	out, err := runStart(t, f, "bluebox", "-m", "fix the build")
	if err != nil {
		t.Fatalf("err %v\n%s", err, out)
	}
	want := startID + "  starting a session on bluebox in ~\n" +
		"waiting for the sessionhub watcher on bluebox\n" +
		"starting Claude on bluebox\n" +
		"started on bluebox: " + startLink + " (started in a new herdr workspace)\n"
	if out != want {
		t.Errorf("output:\n%s\nwant:\n%s", out, want)
	}
	if len(f.created) != 1 || f.created[0] != "bluebox|~|fix the build" {
		t.Errorf("created %q", f.created)
	}
}

func TestStartDefaultDir(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"tower"}, "tower|/home/me/Code/app|"}, // this machine: the current directory
		{[]string{"bluebox"}, "bluebox|~|"},             // another machine: its home
		{[]string{"bluebox", "--dir", "~/Code/x"}, "bluebox|~/Code/x|"},
		{[]string{"--dir", "/srv", "bluebox", "-m", "hi"}, "bluebox|/srv|hi"},
	} {
		f := &fakeStarts{states: []api.StartRequest{startState(api.ControlDone, nil)}}
		if _, err := runStart(t, f, tc.args...); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if len(f.created) != 1 || f.created[0] != tc.want {
			t.Errorf("%v: created %q, want %q", tc.args, f.created, tc.want)
		}
	}
}

func TestStartFailures(t *testing.T) {
	// Bad usage creates nothing.
	for _, args := range [][]string{{}, {"a", "b"}, {"--nope", "a"}, {"a", "--dir"}} {
		f := &fakeStarts{}
		if _, err := runStart(t, f, args...); err == nil || !strings.Contains(err.Error(), "usage") || len(f.created) != 0 {
			t.Errorf("%v: err %v, created %q", args, err, f.created)
		}
	}
	// The server refuses.
	f := &fakeStarts{createErr: &client.StatusError{Status: 409, Message: `not controllable: the sessionhub watcher on machine "bluebox" is offline`}}
	if _, err := runStart(t, f, "bluebox"); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("refused: %v", err)
	}
	// The watcher fails it: exit 1 with the reason, cleaned.
	f = &fakeStarts{states: []api.StartRequest{startState(api.ControlFailed, func(r *api.StartRequest) {
		r.Detail = "the directory is outside this machine's home directory\x1b[31m"
	})}}
	out, err := runStart(t, f, "bluebox", "--dir", "/etc")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 || !strings.Contains(out, "failed: the directory is outside") || strings.Contains(out, "\x1b") {
		t.Errorf("failed: %v\n%q", err, out)
	}
	// Expired: exit 1.
	f = &fakeStarts{states: []api.StartRequest{startState(api.ControlExpired, nil)}}
	if out, err := runStart(t, f, "bluebox"); !errors.As(err, &ee) || ee.Code != 1 || !strings.Contains(out, "expired") {
		t.Errorf("expired: %v\n%s", err, out)
	}
	// Still pending when the follow window ends: exit 1, says so.
	f = &fakeStarts{states: []api.StartRequest{startState(api.ControlPending, nil)}}
	if out, err := runStart(t, f, "bluebox"); !errors.As(err, &ee) || ee.Code != 1 || !strings.Contains(out, "still pending") {
		t.Errorf("timeout: %v\n%s", err, out)
	}
	// A 404 while following stops at once.
	f = &fakeStarts{getErr: &client.StatusError{Status: 404, Message: "not found"}}
	if _, err := runStart(t, f, "bluebox"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("404: %v", err)
	}
}

func TestStartTrust(t *testing.T) {
	done := []api.StartRequest{startState(api.ControlDone, nil)}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"bluebox", "--dir", "~/Code/app", "--trust"}, "bluebox|~/Code/app||trust"},
		{[]string{"bluebox", "--trust", "--dir", "/srv/app", "-m", "hi"}, "bluebox|/srv/app|hi|trust"},
		// On this machine the default is this directory, which is absolute.
		{[]string{"tower", "--trust"}, "tower|/home/me/Code/app||trust"},
		{[]string{"bluebox", "--dir", "~/Code/app"}, "bluebox|~/Code/app|"},
	} {
		f := &fakeStarts{states: done}
		if out, err := runStart(t, f, tc.args...); err != nil || len(f.created) != 1 || f.created[0] != tc.want {
			t.Errorf("%v: created %v, err %v\n%s", tc.args, f.created, err, out)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"bluebox", "--trust"}, "home directory"}, // elsewhere the default is ~
		{[]string{"bluebox", "--trust", "--dir", "~"}, "home directory"},
		{[]string{"bluebox", "--trust", "--dir", "Code/app"}, "absolute --dir"},
		{[]string{"bluebox", "--trust", "--dir", "./app"}, "absolute --dir"},
		{[]string{"bluebox", "--trust", "--dir", "~other/app"}, "absolute --dir"},
	} {
		f := &fakeStarts{states: done}
		_, err := runStart(t, f, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) || len(f.created) != 0 {
			t.Errorf("%v: err %v, created %v; want %q and no request", tc.args, err, f.created, tc.want)
		}
	}
}

func TestStartUntrustedFails(t *testing.T) {
	detail := "/home/me/new is not trusted by Claude on bluebox; open it once or pass --trust"
	f := &fakeStarts{states: []api.StartRequest{startState(api.ControlFailed, func(r *api.StartRequest) { r.Detail = detail })}}
	out, err := runStart(t, f, "bluebox", "--dir", "/home/me/new")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(out, "failed: "+detail+"\n") || strings.Contains(out, "started on") {
		t.Errorf("output:\n%s", out)
	}
}
