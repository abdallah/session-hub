package resume

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

// The remote-control forms: every hostile host and label, and every valid pane ID, parses back
// to the intended arguments, and the remote shell runs sessionhub by path.
func TestHostileRemoteControlForms(t *testing.T) {
	ms := func(ssh, herdr string) []api.Machine {
		return []api.Machine{{Name: "tower", SSHHost: ssh, HerdrHost: herdr}}
	}
	rh := newRemoteHome(t)
	fails, n := 0, 0
	for _, h := range append([]string{"tower.example.com"}, hostileValues...) {
		for _, pane := range []string{"w1:p1", "wS:p12", "1-2_3.4:5"} {
			e, out := hostileEnv("bluebox", ms(h, h), []SavedMachine{{Label: h, SSHTarget: "u@" + h}}, "")
			s := api.Session{ID: uuid, Machine: "tower", CWD: "/x y", HerdrSession: "default", HerdrPane: pane}
			n++
			if err := e.remoteControlSession(context.Background(), s); err != nil {
				t.Fatalf("host %q pane %q: %v", h, pane, err)
			}
			body := out.String()[strings.Index(out.String(), "\n\n")+2:]
			got := argvOf(t, body)
			want := [][]string{
				{"ssh", "-t", h, "~/.local/bin/sessionhub remote-control " + uuid},
				{"herdr", "--machine", h, "agent", "prompt", pane, "/remote-control"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("host %q pane %q: parsed %q, want %q\nprinted:\n%s", h, pane, got, want, body)
				fails++
				continue
			}
			if remote, want := rh.run(t, got[0][3]), [][]string{{rh.sessionhub, "remote-control", uuid}}; !reflect.DeepEqual(remote, want) {
				t.Errorf("host %q: remote ran %q, want %q", h, remote, want)
				fails++
			}
		}
	}
	t.Logf("remote-control cases: %d, failures: %d", n, fails)
}

// --remote-control on the resume forms: the flag is one plain word in every
// shell layer, for every hostile host and cwd.
func TestHostileResumeRemoteControlForms(t *testing.T) {
	rh := newRemoteHome(t)
	dead := filepath.Join(t.TempDir(), "none.sock")
	for _, cwd := range append([]string{"/plain"}, hostileValues...) {
		e, out := hostileEnv("bluebox", nil, nil, dead)
		e.remoteControl = true
		s := api.Session{ID: uuid, Machine: "bluebox", CWD: cwd, HerdrPane: "w1:p1"}
		if _, err := e.resumeSession(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		got := argvOf(t, strings.SplitN(out.String(), "\n", 2)[1])
		want := [][]string{{"cd", cwd}, {"claude", "--resume", uuid, "--remote-control"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("local cwd %q: parsed %q, want %q", cwd, got, want)
		}
	}
	for _, h := range append([]string{"tower.example.com"}, hostileValues...) {
		ms := []api.Machine{{Name: "tower", SSHHost: h, HerdrHost: h}}
		// With herdr info: the flag goes to the remote sessionhub resume, before the ID.
		e, out := hostileEnv("bluebox", ms, nil, "")
		e.remoteControl = true
		s := api.Session{ID: uuid, Machine: "tower", CWD: "/x", HerdrSession: "default", HerdrPane: "w1:p1"}
		if _, err := e.resumeSession(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		body := out.String()[strings.Index(out.String(), "\n\n")+2:]
		got := argvOf(t, body)
		if len(got) < 1 || len(got[0]) != 4 || got[0][2] != h || got[0][3] != "~/.local/bin/sessionhub resume --remote-control "+uuid {
			t.Fatalf("host %q: parsed %q\n%s", h, got, body)
		}
		if remote := rh.run(t, got[0][3]); !reflect.DeepEqual(remote, [][]string{{rh.sessionhub, "resume", "--remote-control", uuid}}) {
			t.Errorf("host %q: remote ran %q", h, remote)
		}
		// No herdr info: three shells, then claude with the flag.
		for _, cwd := range append([]string{"/plain"}, hostileValues...) {
			e, out := hostileEnv("bluebox", ms, nil, "")
			e.remoteControl = true
			s := api.Session{ID: uuid, Machine: "tower", CWD: cwd}
			if _, err := e.resumeSession(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			body := out.String()[strings.Index(out.String(), "\n\n")+2:]
			got := argvOf(t, body)
			if len(got) != 1 || len(got[0]) != 4 {
				t.Fatalf("host %q cwd %q: parsed %q", h, cwd, got)
			}
			login := rh.run(t, got[0][3])
			if len(login) != 1 || len(login[0]) != 3 {
				t.Fatalf("host %q cwd %q: remote ran %q", h, cwd, login)
			}
			inner := argvOf(t, login[0][2])
			want := [][]string{{"cd", cwd}, {"claude", "--resume", uuid, "--remote-control"}}
			if !reflect.DeepEqual(inner, want) {
				t.Errorf("host %q cwd %q: login shell parsed %q, want %q", h, cwd, inner, want)
			}
		}
	}
}

func TestRemoteControlRefusals(t *testing.T) {
	dead := filepath.Join(t.TempDir(), "none.sock")
	ms := []api.Machine{{Name: "tower", SSHHost: "tower.example.com", HerdrHost: "tower.example.com"}}
	for _, id := range []string{"-x", "--resume", "a;b", "a b", "$(id)", "a\nb", "it's", "", "a/b", "é"} {
		for _, machine := range []string{"bluebox", "tower"} {
			e, out := hostileEnv("bluebox", ms, nil, dead)
			s := api.Session{ID: id, Machine: machine, CWD: "/x", HerdrPane: "w1:p1"}
			if err := e.remoteControlSession(context.Background(), s); err == nil || out.Len() != 0 {
				t.Errorf("id %q machine %s: err=%v out=%q, want refusal", id, machine, err, out)
			}
		}
	}
	// A pane ID that isn't herdr-shaped is refused, local or remote, before
	// anything is printed or sent.
	bad := append([]string{"-x", "--help", "-", ".a", ":p1", strings.Repeat("a", 65), "é"}, hostileValues...)
	for _, pane := range bad {
		for _, machine := range []string{"bluebox", "tower"} {
			e, out := hostileEnv("bluebox", ms, []SavedMachine{{Label: "tower", SSHTarget: "tower.example.com"}}, dead)
			s := api.Session{ID: uuid, Machine: machine, CWD: "/x", HerdrPane: pane}
			if err := e.remoteControlSession(context.Background(), s); err == nil || out.Len() != 0 {
				t.Errorf("pane %q machine %s: err=%v out=%q, want refusal", pane, machine, err, out)
			}
		}
	}
	for _, m := range []api.Machine{
		{Name: "tower", SSHHost: "-F/etc/x", HerdrHost: "ok"},
		{Name: "tower", SSHHost: "ok", HerdrHost: "--config=/x"},
	} {
		e, out := hostileEnv("bluebox", []api.Machine{m}, nil, dead)
		s := api.Session{ID: uuid, Machine: "tower", CWD: "/x", HerdrPane: "w1:p1"}
		if err := e.remoteControlSession(context.Background(), s); err == nil || out.Len() != 0 {
			t.Errorf("machine %+v: err=%v out=%q, want refusal", m, err, out)
		}
	}
}
