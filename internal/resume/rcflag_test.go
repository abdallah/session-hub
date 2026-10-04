package resume

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// --remote-control adds the flag to every claude start and every printed
// claude command, and never adds a name.
func TestResumeRemoteControlFlag(t *testing.T) {
	deadSock := filepath.Join(t.TempDir(), "none.sock")
	wantArgs := []any{"--resume", uuid, "--remote-control"}
	sameArgs := func(got any) bool {
		g, _ := got.([]any)
		if len(g) != len(wantArgs) {
			return false
		}
		for i := range g {
			if g[i] != wantArgs[i] {
				return false
			}
		}
		return true
	}
	t.Run("start in the session's pane", func(t *testing.T) {
		fh := paneHerdr(t, "w1:p1", uuid, "")
		e, _ := newEnv(t, hubServer(t, sess("bluebox", "w1:p1")), "bluebox", fh.path, nil)
		e.remoteControl = true
		if _, err := e.resume(context.Background(), uuid[:8]); err != nil {
			t.Fatal(err)
		}
		if as := fh.params("agent.start"); !sameArgs(as["args"]) {
			t.Errorf("agent.start params = %v", as)
		}
	})
	t.Run("start in a new split", func(t *testing.T) {
		fh := happyHerdr(t, "", "")
		e, out := newEnv(t, hubServer(t, sess("bluebox", "w1:p1")), "bluebox", fh.path, nil)
		e.remoteControl = true
		if _, err := e.resume(context.Background(), uuid[:8]); err != nil {
			t.Fatal(err)
		}
		if as := fh.params("agent.start"); !sameArgs(as["args"]) {
			t.Errorf("agent.start params = %v", as)
		}
		if !strings.Contains(out.String(), "claude --resume "+uuid+" --remote-control in new pane") {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("without the flag the args are unchanged", func(t *testing.T) {
		fh := happyHerdr(t, "", "")
		e, _ := newEnv(t, hubServer(t, sess("bluebox", "w1:p1")), "bluebox", fh.path, nil)
		if _, err := e.resume(context.Background(), uuid[:8]); err != nil {
			t.Fatal(err)
		}
		if args, _ := fh.params("agent.start")["args"].([]any); len(args) != 2 {
			t.Errorf("args = %v", args)
		}
	})
	t.Run("a start failure prints the fallback with the flag", func(t *testing.T) {
		fh := newFakeHerdr(t, func(m string, p map[string]any) (any, string) {
			if m == "pane.get" {
				return paneResult("w1:p1", uuid, ""), ""
			}
			return nil, "pane_not_available"
		})
		e, _ := newEnv(t, hubServer(t, sess("bluebox", "w1:p1")), "bluebox", fh.path, nil)
		e.remoteControl = true
		_, err := e.resume(context.Background(), uuid[:8])
		if err == nil || !strings.Contains(err.Error(), "claude --resume "+uuid+" --remote-control") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("the pane already runs the session: focus it and say how to turn it on", func(t *testing.T) {
		fh := happyHerdr(t, "w1:p1", uuid)
		e, out := newEnv(t, hubServer(t, sess("bluebox", "w1:p1")), "bluebox", fh.path, nil)
		e.remoteControl = true
		acted, err := e.resume(context.Background(), uuid[:8])
		if err != nil || !acted {
			t.Fatalf("acted=%v err=%v", acted, err)
		}
		if got := strings.Join(fh.methods(), ","); got != "pane.get,pane.focus" {
			t.Errorf("methods = %s", got)
		}
		if !strings.Contains(out.String(), "sessionhub remote-control "+uuid) {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("printed local command", func(t *testing.T) {
		e, out := newEnv(t, hubServer(t, sess("bluebox", "w1:p1")), "bluebox", deadSock, nil)
		e.remoteControl = true
		if _, err := e.resume(context.Background(), uuid[:8]); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "cd '/home/user/my project' && claude --resume "+uuid+" --remote-control\n") {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("printed remote commands pass the flag through", func(t *testing.T) {
		e, out := newEnv(t, hubServer(t, sess("tower", "w1:p1")), "bluebox", deadSock, nil)
		e.remoteControl = true
		if _, err := e.resume(context.Background(), uuid[:8]); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "ssh -t tower.example.com '~/.local/bin/sessionhub resume --remote-control "+uuid+"'") {
			t.Errorf("output:\n%s", out)
		}
		e, out = newEnv(t, hubServer(t, sess("tower", "")), "bluebox", deadSock, nil)
		e.remoteControl = true
		if _, err := e.resume(context.Background(), uuid[:8]); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "claude --resume "+uuid+" --remote-control") {
			t.Errorf("output:\n%s", out)
		}
	})
}

func TestResumeUsageMentionsRemoteControl(t *testing.T) {
	if !strings.Contains(usage, "--remote-control") {
		t.Errorf("usage = %q", usage)
	}
}
