package plugin

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/resume"
)

const startLink = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"

func TestControllerStartsSession(t *testing.T) {
	h := newLiveHub(t)
	ctx := context.Background()
	if err := h.st.RecordPoll(ctx, h.id); err != nil {
		t.Fatal(err)
	}
	req, err := h.st.CreateStart(ctx, "tower", api.StartIn{Dir: "~/Code/app", Prompt: "fix it"}, "web:phone")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	ctl := newController(h.clientFunc(), nil, log.New(&logs, "", 0))
	ctl.wait = time.Second
	var got []api.StartRequest
	ctl.start = func(_ context.Context, r api.StartRequest) api.ControlResultIn {
		got = append(got, r)
		return api.ControlResultIn{State: api.ControlDone, URL: startLink, Detail: detailStarted}
	}
	ctl.once(ctx)
	if len(got) != 1 || got[0].ID != req.ID || got[0].Dir != "~/Code/app" || got[0].Prompt != "fix it" {
		t.Fatalf("start calls %+v", got)
	}
	if r, _ := h.st.GetStart(ctx, req.ID); r.State != api.ControlDone || r.URL != startLink || r.Detail != detailStarted {
		t.Fatalf("stored %+v", r)
	}
	if !strings.Contains(logs.String(), "for a new session: done") || strings.Contains(logs.String(), "fix it") {
		t.Errorf("log:\n%s", logs.String())
	}

	// A watcher without a starter fails the request at once.
	req2, err := h.st.CreateStart(ctx, "tower", api.StartIn{Dir: "/srv"}, "web:phone")
	if err != nil {
		t.Fatal(err)
	}
	ctl.start = nil
	ctl.once(ctx)
	if r, _ := h.st.GetStart(ctx, req2.ID); r.State != api.ControlFailed || r.Detail != detailNoStarter {
		t.Fatalf("without a starter: %+v", r)
	}
}

func TestStartRunnerOptOut(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("SESSIONHUB_CONFIG", cfgPath)
	t.Setenv("SESSIONHUB_REMOTE_START", "")
	if err := os.WriteFile(cfgPath, []byte("remote_start = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The socket doesn't exist: an opted-out machine never reaches herdr.
	run := startRunner(filepath.Join(t.TempDir(), "none.sock"), time.Millisecond, 10*time.Millisecond)
	req := api.StartRequest{ID: "st_AAAAAAAAAAAAAAAAAAAAAA", Dir: "~"}
	// A config that can't be parsed might opt out: refuse.
	if err := os.WriteFile(cfgPath, []byte("remote_start = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if in := run(context.Background(), req); in.State != api.ControlFailed || !strings.Contains(in.Detail, "can't be read") {
		t.Fatalf("bad config: %+v", in)
	}
	if err := os.WriteFile(cfgPath, []byte("remote_start = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if in := run(context.Background(), req); in.State != api.ControlFailed || in.Detail != detailStartOff {
		t.Fatalf("config opt-out: %+v", in)
	}
	if err := os.WriteFile(cfgPath, []byte("remote_start = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SESSIONHUB_REMOTE_START", "off")
	if in := run(context.Background(), req); in.State != api.ControlFailed || in.Detail != detailStartOff {
		t.Fatalf("env opt-out: %+v", in)
	}
	// Opted in, it checks Claude's trust in the home directory's config.
	t.Setenv("SESSIONHUB_REMOTE_START", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	req.Machine = "tower"
	if in := run(context.Background(), req); in.State != api.ControlFailed ||
		in.Detail != home+" is not trusted by Claude on tower; open it once or pass --trust" {
		t.Fatalf("untrusted: %+v", in)
	}
	// Trusted, it gets as far as herdr, which isn't running.
	trusted := `{"projects": {"` + home + `": {"hasTrustDialogAccepted": true}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	if in := run(context.Background(), req); in.State != api.ControlFailed || !strings.Contains(in.Detail, "herdr is not running") {
		t.Fatalf("opted in: %+v", in)
	}
	// CLAUDE_CONFIG_DIR moves the config Claude reads trust from.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if in := run(context.Background(), req); in.State != api.ControlFailed || !strings.Contains(in.Detail, "not trusted") {
		t.Fatalf("CLAUDE_CONFIG_DIR: %+v", in)
	}
}

func TestStartResultFor(t *testing.T) {
	started := func(url, note string) resume.NewSessionResult {
		return resume.NewSessionResult{ControlResult: resume.ControlResult{Outcome: resume.OutcomeStarted, URL: url}, PromptNote: note}
	}
	for _, tc := range []struct {
		r    resume.NewSessionResult
		want api.ControlResultIn
	}{
		{started(startLink, ""), api.ControlResultIn{State: api.ControlDone, URL: startLink, Detail: detailStarted}},
		{started("", ""), api.ControlResultIn{State: api.ControlDone, Detail: detailStartedNoLink}},
		{started(startLink, "the first prompt was not sent: x"),
			api.ControlResultIn{State: api.ControlDone, URL: startLink, Detail: detailStarted + "; the first prompt was not sent: x"}},
		{resume.NewSessionResult{ControlResult: resume.ControlResult{Outcome: resume.OutcomeNoDir}},
			api.ControlResultIn{State: api.ControlFailed, Detail: detailStartNoDir}},
		{resume.NewSessionResult{ControlResult: resume.ControlResult{Outcome: resume.OutcomeOutsideHome}},
			api.ControlResultIn{State: api.ControlFailed, Detail: detailOutsideHome}},
		{resume.NewSessionResult{ControlResult: resume.ControlResult{Outcome: resume.OutcomeError, Err: errors.New("boom\x1b[31m")}},
			api.ControlResultIn{State: api.ControlFailed, Detail: "boom[31m"}},
		{resume.NewSessionResult{ControlResult: resume.ControlResult{Outcome: resume.OutcomeUntrusted}, Dir: "/home/me/new\x1b[31m"},
			api.ControlResultIn{State: api.ControlFailed, Detail: "/home/me/new[31m is not trusted by Claude on tower; open it once or pass --trust"}},
	} {
		if got := startResultFor("tower", tc.r); got != tc.want {
			t.Errorf("%+v: got %+v, want %+v", tc.r, got, tc.want)
		}
	}
}

func TestCommandLine(t *testing.T) {
	got, err := commandLine(os.Getpid())
	if err != nil || !strings.HasPrefix(got, os.Args[0]) || strings.Contains(got, "\x00") {
		t.Fatalf("commandLine(self) = %q, %v; want it to start with %q", got, err, os.Args[0])
	}
	if _, err := commandLine(1 << 30); err == nil {
		t.Error("a PID that doesn't exist has a command line")
	}
}
