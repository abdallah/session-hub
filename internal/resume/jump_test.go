package resume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestPlanJump(t *testing.T) {
	machines := []api.Machine{
		{Name: "bluebox", SSHHost: "bluebox.example.com", HerdrHost: "bluebox.example.com"},
		{Name: "tower", HerdrHost: "tower.example.com"},
		{Name: "bare", SSHHost: "bare.example"},
		{Name: "evil", SSHHost: "-oProxyCommand=x", HerdrHost: "evil.example"},
		{Name: "ctl", SSHHost: "ctl.example", HerdrHost: "host\x15\rrm"},
		{Name: "sp", SSHHost: "sp.example", HerdrHost: "a b"},
	}
	bluebox := jumpPlan{kind: jumpRemote, sshHost: "bluebox.example.com", herdrHost: "bluebox.example.com", tab: "sessionhub:bluebox"}
	cases := []struct {
		name, machine, this string
		want                jumpPlan
	}{
		{"this machine", "bluebox", "bluebox", jumpPlan{kind: jumpLocal}},
		{"another machine", "bluebox", "tower", bluebox},
		{"no machine in the client config: never local", "bluebox", "", bluebox},
		{"no ssh_host: the machine name", "tower", "bluebox", jumpPlan{kind: jumpRemote, sshHost: "tower", herdrHost: "tower.example.com", tab: "sessionhub:tower"}},
		{"no herdr_host", "bare", "bluebox", jumpPlan{kind: jumpPrint, why: `machine "bare" has no herdr_host`}},
		{"a host that is an option", "evil", "bluebox", jumpPlan{kind: jumpPrint, why: `machine "evil" has an SSH or herdr host sessionhub won't use`}},
		{"a herdr_host with a control character", "ctl", "bluebox", jumpPlan{kind: jumpPrint, why: `machine "ctl" has an SSH or herdr host sessionhub won't use`}},
		{"a herdr_host with a space", "sp", "bluebox", jumpPlan{kind: jumpPrint, why: `machine "sp" has an SSH or herdr host sessionhub won't use`}},
		{"a machine the server doesn't list", "zed", "bluebox", jumpPlan{kind: jumpPrint, why: `the server lists no machine "zed"`}},
	}
	for _, c := range cases {
		if got := planJump(api.Session{ID: uuid, Machine: c.machine}, c.this, machines); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	if got := planJump(api.Session{ID: uuid, Machine: "bluebox"}, "tower", nil); got.kind != jumpPrint {
		t.Errorf("no machine list: %+v, want a print", got)
	}
}

// newJumper returns a Jumper on e whose ssh records "host command" and
// returns sshErr.
func newJumper(e *env, sshErr error) (*Jumper, *[]string) {
	var calls []string
	return &Jumper{e: e, herdrBin: "/nonexistent/herdr", ssh: func(_ context.Context, host, command string) error {
		calls = append(calls, host+" "+command)
		return sshErr
	}}, &calls
}

func TestJumpLocal(t *testing.T) {
	ctx := context.Background()
	// The pane runs the session: focus it, and print nothing.
	h := happyHerdr(t, "w1:p1", uuid)
	s := sess("bluebox", "w1:p1")
	e, out := newEnv(t, hubServer(t, s), "bluebox", h.path, nil)
	j, calls := newJumper(e, nil)
	msg, err := j.Jump(ctx, s)
	if err != nil || msg != `Focused pane "w1:p1", which still runs session 0a1b2c3d.` {
		t.Errorf("focus: %q %v", msg, err)
	}
	if strings.Join(h.methods(), " ") != "pane.get pane.focus" || len(*calls) != 0 || out.Len() != 0 {
		t.Errorf("focus: herdr %v, ssh %v, printed %q", h.methods(), *calls, out.String())
	}
	// No pane recorded: the resume command, nothing started.
	s = sess("bluebox", "")
	e, out = newEnv(t, hubServer(t, s), "bluebox", filepath.Join(t.TempDir(), "none.sock"), nil)
	j, _ = newJumper(e, nil)
	msg, err = j.Jump(ctx, s)
	want := "The session has no herdr pane recorded; run: cd '/home/user/my project' && claude --resume " + uuid
	if err != nil || msg != want || out.Len() != 0 {
		t.Errorf("no pane: %q %v printed %q, want %q", msg, err, out.String(), want)
	}
	// Live but no pane runs it: refuse, and say how to force it.
	s = sess("bluebox", "w1:p1")
	s.Status = api.StatusLive
	h = happyHerdr(t, "", "")
	e, _ = newEnv(t, hubServer(t, s), "bluebox", h.path, nil)
	j, _ = newJumper(e, nil)
	if _, err := j.Jump(ctx, s); err == nil || !strings.Contains(err.Error(), "sessionhub resume --force "+uuid) {
		t.Errorf("live without a pane: %v", err)
	}
	for _, m := range h.methods() {
		if m == "agent.start" || m == "pane.split" {
			t.Errorf("live without a pane started something: %v", h.methods())
		}
	}
	// A moving session is refused, here and on another machine, before any
	// herdr call or ssh.
	for _, machine := range []string{"bluebox", "tower"} {
		s = sess(machine, "w1:p1")
		s.Move = &api.Move{ID: "mv_AAAAAAAAAAAAAAAAAAAAAA", SessionID: uuid, Source: machine, Target: "other", State: api.MoveUnpacking}
		h = happyHerdr(t, "", "")
		e, out = newEnv(t, hubServer(t, s), "bluebox", h.path, nil)
		j, calls := newJumper(e, nil)
		msg, err := j.Jump(ctx, s)
		if err == nil || msg != "" || !strings.Contains(err.Error(), "sessionhub move --status mv_AAAAAAAAAAAAAAAAAAAAAA") ||
			strings.Contains(err.Error(), "claude --resume") || len(h.methods()) != 0 || len(*calls) != 0 || out.Len() != 0 {
			t.Errorf("moving session on %s: %q %v, herdr %v, ssh %v", machine, msg, err, h.methods(), *calls)
		}
	}
	// A hostile ID never reaches herdr or ssh.
	bad := sess("bluebox", "w1:p1")
	bad.ID = "-rf"
	if _, err := j.Jump(ctx, bad); err == nil || !strings.Contains(err.Error(), "refusing session ID") {
		t.Errorf("bad ID: %v", err)
	}
}

func TestJumpRemoteFocusesExistingTab(t *testing.T) {
	f := newFakeHerdr(t, func(m string, p map[string]any) (any, string) {
		switch m {
		case "tab.list":
			return map[string]any{"type": "tab_list", "tabs": []any{
				map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "label": "1", "focused": true},
				map[string]any{"tab_id": "w2:t3", "workspace_id": "w2", "label": "sessionhub:bluebox"},
			}}, ""
		case "tab.focus":
			return map[string]any{"type": "ok"}, ""
		}
		return nil, "no_fixture"
	})
	s := sess("bluebox", "w1:p1")
	e, out := newEnv(t, hubServer(t, s), "tower", f.path, nil)
	j, calls := newJumper(e, nil)
	msg, err := j.Jump(context.Background(), s)
	if err != nil || msg != "sessionhub resume ran on bluebox; showing it in tab sessionhub:bluebox" {
		t.Errorf("Jump = %q, %v", msg, err)
	}
	if want := []string{"bluebox.example.com ~/.local/bin/sessionhub resume " + uuid}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("ssh calls %q, want %q", *calls, want)
	}
	if strings.Join(f.methods(), " ") != "tab.list tab.focus" || f.params("tab.focus")["tab_id"] != "w2:t3" {
		t.Errorf("herdr %v, focus %v", f.methods(), f.params("tab.focus"))
	}
	if out.Len() != 0 {
		t.Errorf("printed %q", out.String())
	}
}

func TestJumpRemoteCreatesTab(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFakeHerdr(t, func(m string, p map[string]any) (any, string) {
		switch m {
		case "tab.list":
			return map[string]any{"type": "tab_list", "tabs": []any{
				map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "label": "1", "focused": true}}}, ""
		case "tab.create":
			return map[string]any{"type": "tab_created",
				"tab":       map[string]any{"tab_id": "w1:t4", "workspace_id": "w1", "label": "sessionhub:bluebox", "focused": true},
				"root_pane": map[string]any{"pane_id": "w1:p8", "workspace_id": "w1", "tab_id": "w1:t4"}}, ""
		}
		return nil, "no_fixture"
	})
	s := sess("bluebox", "w1:p1")
	e, _ := newEnv(t, hubServer(t, s), "tower", f.path, nil)
	j, _ := newJumper(e, nil)
	j.herdrBin = bin
	msg, err := j.Jump(context.Background(), s)
	if err != nil || msg != "sessionhub resume ran on bluebox; showing it in tab sessionhub:bluebox" {
		t.Errorf("Jump = %q, %v", msg, err)
	}
	if strings.Join(f.methods(), " ") != "tab.list tab.create" ||
		!reflect.DeepEqual(f.params("tab.create"), map[string]any{"label": "sessionhub:bluebox", "focus": true}) {
		t.Errorf("herdr %v, create %v", f.methods(), f.params("tab.create"))
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimRight(string(b), "\n"), "\n"); !reflect.DeepEqual(got, []string{"pane", "run", "w1:p8", "herdr --remote bluebox.example.com"}) {
		t.Errorf("herdr args %q", got)
	}
}

// exitErr is an *exec.ExitError with the given status, wrapped with a stderr
// message the way runSSH does.
func exitErr(t *testing.T, code int, msg string) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("exit %d: %v", code, err)
	}
	return fmt.Errorf("%w: %s", err, msg)
}

func TestJumpRemoteFallbacks(t *testing.T) {
	ctx := context.Background()
	s := sess("bluebox", "w1:p1")
	// ssh fails: the resume command, and no local tab.
	f := newFakeHerdr(t, func(string, map[string]any) (any, string) { return nil, "no_fixture" })
	e, _ := newEnv(t, hubServer(t, s), "tower", f.path, nil)
	j, _ := newJumper(e, exitErr(t, 255, "Permission denied (publickey)"))
	msg, err := j.Jump(ctx, s)
	want := "ssh bluebox.example.com failed (exit status 255: Permission denied (publickey)); run: " + s.ResumeCommand
	if err != nil || msg != want || len(f.methods()) != 0 {
		t.Errorf("ssh failure: %q %v, herdr %v; want %q", msg, err, f.methods(), want)
	}
	// ssh ran but the remote sessionhub resume exited non-zero: say so, not "ssh failed".
	j, _ = newJumper(e, exitErr(t, 1, "no pane runs it"))
	msg, err = j.Jump(ctx, s)
	want = "sessionhub resume on bluebox failed (exit status 1: no pane runs it); run: " + s.ResumeCommand
	if err != nil || msg != want || len(f.methods()) != 0 {
		t.Errorf("remote failure: %q %v, herdr %v; want %q", msg, err, f.methods(), want)
	}
	// A process killed by the timeout has exit code -1: still ssh's failure.
	kctx, kcancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer kcancel()
	killed := exec.CommandContext(kctx, "sleep", "5").Run()
	var ee *exec.ExitError
	if !errors.As(killed, &ee) || ee.ExitCode() != -1 {
		t.Fatalf("killed process: %v, want an ExitError with code -1", killed)
	}
	j, _ = newJumper(e, killed)
	if msg, _ = j.Jump(ctx, s); !strings.HasPrefix(msg, "ssh bluebox.example.com failed (signal: killed)") {
		t.Errorf("killed ssh: %q", msg)
	}
	// A plain error (a missing ssh binary) is still ssh's.
	j, _ = newJumper(e, errors.New("executable file not found"))
	if msg, _ = j.Jump(ctx, s); !strings.HasPrefix(msg, "ssh bluebox.example.com failed (executable file not found)") {
		t.Errorf("non-exit error: %q", msg)
	}
	// ssh works but the local herdr refuses: the herdr --remote command.
	j, _ = newJumper(e, nil)
	msg, err = j.Jump(ctx, s)
	if err != nil || !strings.HasPrefix(msg, "sessionhub resume ran on bluebox, but the local tab failed") ||
		!strings.HasSuffix(msg, "run: herdr --remote bluebox.example.com") {
		t.Errorf("tab failure: %q %v", msg, err)
	}
	// A machine the server doesn't list: no ssh at all.
	zed := sess("zed", "w1:p1")
	e, _ = newEnv(t, hubServer(t, zed), "tower", f.path, nil)
	j, calls := newJumper(e, nil)
	msg, err = j.Jump(ctx, zed)
	if err != nil || msg != `the server lists no machine "zed"; run: `+zed.ResumeCommand || len(*calls) != 0 {
		t.Errorf("unknown machine: %q %v ssh %v", msg, err, *calls)
	}
}

// TestRunSSH uses a fake ssh on PATH: it gets BatchMode, stdin from
// /dev/null, and stops when ctx is cancelled.
func TestRunSSH(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nif [ \"$3\" = hang ]; then exec sleep 30; fi\n" +
		"if [ \"$3\" = fail ]; then echo 'Permission denied' >&2; exit 255; fi\n[ -z \"$(cat)\" ] || exit 9\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := runSSH(context.Background(), "h", "cmd"); err != nil {
		t.Fatalf("runSSH: %v", err)
	}
	b, _ := os.ReadFile(argsFile)
	if got := strings.Fields(string(b)); !reflect.DeepEqual(got, []string{"-o", "BatchMode=yes", "h", "cmd"}) {
		t.Errorf("ssh args %q", got)
	}
	if err := runSSH(context.Background(), "fail", "cmd"); err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("error %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := runSSH(ctx, "hang", "cmd"); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("cancelled ssh: %v after %v", err, time.Since(start))
	}
}

func TestAliasTarget(t *testing.T) {
	for _, c := range []struct {
		out, name string
		want      bool
	}{
		{"user x\nhostname 192.168.1.145\nport 22\n", "tower", true},
		{"hostname bluebox\n", "bluebox", false},
		{"hostname BLUEBOX\n", "bluebox", false},
		{"port 22\n", "tower", false},
	} {
		if got := aliasTarget(c.out, c.name); got != c.want {
			t.Errorf("aliasTarget(%q, %q) = %v, want %v", c.out, c.name, got, c.want)
		}
	}
}

// With an SSH alias named after the machine, the jump uses it for ssh and
// for herdr --remote instead of the server's host (often unreachable from
// this machine, like a Cloudflare hostname).
func TestJumpRemotePrefersAlias(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFakeHerdr(t, func(m string, p map[string]any) (any, string) {
		switch m {
		case "tab.list":
			return map[string]any{"type": "tab_list", "tabs": []any{}}, ""
		case "tab.create":
			return map[string]any{"type": "tab_created",
				"tab":       map[string]any{"tab_id": "w1:t4", "workspace_id": "w1", "label": "sessionhub:bluebox", "focused": true},
				"root_pane": map[string]any{"pane_id": "w1:p8", "workspace_id": "w1", "tab_id": "w1:t4"}}, ""
		}
		return nil, "no_fixture"
	})
	s := sess("bluebox", "w1:p1")
	e, _ := newEnv(t, hubServer(t, s), "tower", f.path, nil)
	j, calls := newJumper(e, nil)
	j.herdrBin = bin
	j.alias = func(_ context.Context, name string) bool { return name == "bluebox" }
	if _, err := j.Jump(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "bluebox ") {
		t.Errorf("ssh calls %q, want one to host bluebox", *calls)
	}
	b, _ := os.ReadFile(argsFile)
	if got := strings.Split(strings.TrimRight(string(b), "\n"), "\n"); !reflect.DeepEqual(got, []string{"pane", "run", "w1:p8", "herdr --remote bluebox"}) {
		t.Errorf("herdr args %q", got)
	}

	// Without an alias, the server's host is used as before.
	j2, calls2 := newJumper(e, nil)
	j2.herdrBin = bin
	j2.alias = func(context.Context, string) bool { return false }
	if _, err := j2.Jump(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(*calls2) != 1 || !strings.HasPrefix((*calls2)[0], "bluebox.example.com ") {
		t.Errorf("ssh calls %q, want one to bluebox.example.com", *calls2)
	}
}
