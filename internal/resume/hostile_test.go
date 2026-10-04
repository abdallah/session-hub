package resume

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

// stubAPI serves one machine list.
type stubAPI struct{ machines []api.Machine }

func (s stubAPI) GetSession(context.Context, string) (api.SessionDetail, error) {
	return api.SessionDetail{}, nil
}
func (s stubAPI) ListSessions(context.Context, bool, string) ([]api.Session, error) { return nil, nil }
func (s stubAPI) ListMachines(context.Context) ([]api.Machine, error)               { return s.machines, nil }

func (s stubAPI) CreateControl(context.Context, string) (api.ControlRequest, error) {
	return api.ControlRequest{}, errors.New("stub: no sessionhub")
}

// argvOf runs script under sh with cd, claude, ssh, and herdr replaced by
// functions that print "name arg... END" NUL-separated, and returns the
// records. It proves what a POSIX shell parses the printed text into.
func argvOf(t *testing.T, script string) [][]string {
	t.Helper()
	const stubs = `rec() { printf '%s\0' "$@" END; }
cd() { rec cd "$@"; }
claude() { rec claude "$@"; }
ssh() { rec ssh "$@"; }
herdr() { rec herdr "$@"; }
`
	out, err := exec.Command("sh", "-c", stubs+script).Output()
	if err != nil {
		t.Fatalf("sh failed on %q: %v", script, err)
	}
	return splitRecords(out)
}

// remoteHome is a fake remote home: ~/.local/bin/sessionhub and $SHELL are scripts
// that record "$0" "$@" like argvOf's stubs.
type remoteHome struct{ dir, sessionhub, shell string }

func newRemoteHome(t *testing.T) remoteHome {
	t.Helper()
	dir := t.TempDir()
	h := remoteHome{dir: dir, sessionhub: filepath.Join(dir, ".local", "bin", "sessionhub"), shell: filepath.Join(dir, "login-shell")}
	const rec = "#!/bin/sh\nprintf '%s\\0' \"$0\" \"$@\" END\n"
	if err := os.MkdirAll(filepath.Dir(h.sessionhub), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{h.sessionhub, h.shell} {
		if err := os.WriteFile(p, []byte(rec), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// run parses cmd the way the remote side of `ssh -t host cmd` does: sh -c
// with the remote HOME and SHELL, and a PATH without ~/.local/bin.
func (h remoteHome) run(t *testing.T, cmd string) [][]string {
	t.Helper()
	c := exec.Command("sh", "-c", cmd)
	c.Env = []string{"HOME=" + h.dir, "SHELL=" + h.shell, "PATH=/usr/bin:/bin"}
	out, err := c.Output()
	if err != nil {
		t.Fatalf("remote sh failed on %q: %v", cmd, err)
	}
	return splitRecords(out)
}

func splitRecords(out []byte) [][]string {
	var recs [][]string
	var cur []string
	for _, f := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if f == "END" {
			recs = append(recs, cur)
			cur = nil
			continue
		}
		cur = append(cur, f)
	}
	return recs
}

func hostileEnv(machine string, machines []api.Machine, saved []SavedMachine, socket string) (*env, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return &env{
		cfg: client.Config{Machine: machine}, api: stubAPI{machines}, socket: socket,
		saved: func(context.Context) []SavedMachine { return saved },
		out:   out,
	}, out
}

var hostileValues = []string{"a;b", "a!b", "it's", "$(touch /tmp/pwned)", "a\nb", "`id`", "a b", `a"b`, "x; rm -rf ~", "a&&b", "a|b", "$HOME", `a\b`}

func TestHostileCwdLocalFallback(t *testing.T) {
	dead := filepath.Join(t.TempDir(), "none.sock")
	for _, cwd := range append([]string{"/plain/dir"}, hostileValues...) {
		e, out := hostileEnv("bluebox", nil, nil, dead)
		s := api.Session{ID: uuid, Machine: "bluebox", CWD: cwd, HerdrPane: "w1:p1"}
		if _, err := e.resumeSession(context.Background(), s); err != nil {
			t.Fatalf("cwd %q: %v", cwd, err)
		}
		cmd := strings.SplitN(out.String(), "\n", 2)[1]
		got := argvOf(t, cmd)
		want := [][]string{{"cd", cwd}, {"claude", "--resume", uuid}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("cwd %q: parsed %q, want %q\nprinted: %s", cwd, got, want, cmd)
		}
	}
}

func TestHostileRemoteForms(t *testing.T) {
	ms := func(ssh, herdr string) []api.Machine {
		return []api.Machine{{Name: "tower", SSHHost: ssh, HerdrHost: herdr}}
	}
	hosts := append([]string{"tower.example.com"}, hostileValues...)
	panes := append([]string{"w1:p1"}, hostileValues...)
	rh := newRemoteHome(t)
	for _, h := range hosts {
		for _, pane := range panes {
			// Remote with herdr info and a matching saved machine (the label is hostile too).
			e, out := hostileEnv("bluebox", ms(h, h), []SavedMachine{{Label: h, SSHTarget: "u@" + h}}, "")
			s := api.Session{ID: uuid, Machine: "tower", CWD: "/x y", HerdrSession: "default", HerdrPane: pane}
			if _, err := e.resumeSession(context.Background(), s); err != nil {
				t.Fatalf("host %q pane %q: %v", h, pane, err)
			}
			body := out.String()[strings.Index(out.String(), "\n\n")+2:]
			got := argvOf(t, body)
			// The local shell leaves the tilde alone; ssh gets one string.
			want := [][]string{
				{"ssh", "-t", h, "~/.local/bin/sessionhub resume " + uuid},
				{"herdr", "--remote", h},
				{"herdr", "--machine", h, "pane", "focus", pane},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("host %q pane %q: parsed %q, want %q\nprinted:\n%s", h, pane, got, want, body)
				continue
			}
			// The remote shell expands the tilde to its own home and runs sessionhub by path.
			if remote, want := rh.run(t, got[0][3]), [][]string{{rh.sessionhub, "resume", uuid}}; !reflect.DeepEqual(remote, want) {
				t.Errorf("host %q: remote ran %q, want %q", h, remote, want)
			}
		}
	}
}

// Three shells parse the no-herdr form: the local one, the remote one ssh
// starts, and the remote login shell ($SHELL -lc). Each must hand the next
// exactly the intended arguments for every hostile host and cwd.
func TestHostileRemoteNoHerdrInfo(t *testing.T) {
	rh := newRemoteHome(t)
	double, single, fails := 0, 0, 0
	for _, h := range append([]string{"tower.example.com"}, hostileValues...) {
		for _, cwd := range append([]string{"/plain"}, hostileValues...) {
			e, out := hostileEnv("bluebox", []api.Machine{{Name: "tower", SSHHost: h, HerdrHost: h}}, nil, "")
			s := api.Session{ID: uuid, Machine: "tower", CWD: cwd}
			if _, err := e.resumeSession(context.Background(), s); err != nil {
				t.Fatalf("host %q cwd %q: %v", h, cwd, err)
			}
			body := out.String()[strings.Index(out.String(), "\n\n")+2:]
			got := argvOf(t, body)
			if len(got) != 1 || len(got[0]) != 4 || got[0][0] != "ssh" || got[0][1] != "-t" || got[0][2] != h {
				t.Fatalf("host %q cwd %q: parsed %q\nprinted: %s", h, cwd, got, body)
			}
			// The remote shell parses the fourth argument: exec the login shell.
			login := rh.run(t, got[0][3])
			if len(login) != 1 || len(login[0]) != 3 || login[0][0] != rh.shell || login[0][1] != "-lc" {
				t.Fatalf("host %q cwd %q: remote ran %q, want %s -lc <cmd>\nprinted: %s", h, cwd, login, rh.shell, body)
			}
			// The login shell parses its -c string into cd and claude.
			inner := argvOf(t, login[0][2])
			want := [][]string{{"cd", cwd}, {"claude", "--resume", uuid}}
			if !reflect.DeepEqual(inner, want) {
				t.Errorf("host %q cwd %q: login shell parsed %q, want %q\nprinted: %s", h, cwd, inner, want, body)
				fails++
			}
			if strings.Contains(body, ` "exec \$SHELL -lc `) {
				double++
			} else {
				single++
			}
		}
	}
	// Both local quoting forms were exercised.
	t.Logf("no-herdr cases: %d double-quoted, %d single-quoted, %d failures", double, single, fails)
	if double == 0 || single == 0 {
		t.Errorf("quoting forms: %d double, %d single; want both", double, single)
	}
}

func TestRefusals(t *testing.T) {
	dead := filepath.Join(t.TempDir(), "none.sock")
	// A hostile ID is refused everywhere, printing nothing.
	for _, id := range []string{"-x", "--resume", "a;b", "a b", "$(id)", "a\nb", "it's", "", "a/b", "é"} {
		for _, machine := range []string{"bluebox", "tower"} {
			e, out := hostileEnv("bluebox", []api.Machine{{Name: "tower", SSHHost: "tower.example.com", HerdrHost: "tower.example.com"}}, nil, dead)
			s := api.Session{ID: id, Machine: machine, CWD: "/x", HerdrPane: "w1:p1"}
			acted, err := e.resumeSession(context.Background(), s)
			if err == nil || acted || out.Len() != 0 {
				t.Errorf("id %q machine %s: err=%v acted=%v out=%q, want refusal", id, machine, err, acted, out)
			}
		}
	}
	// A bad host is refused for both hosts and for the ResumeCommand fallback.
	for _, tc := range []struct {
		name  string
		ms    []api.Machine
		resum string
	}{
		{"leading dash ssh", []api.Machine{{Name: "tower", SSHHost: "-F/etc/x", HerdrHost: "ok"}}, ""},
		{"leading dash herdr", []api.Machine{{Name: "tower", SSHHost: "ok", HerdrHost: "--config=/x"}}, ""},
		{"empty everything", nil, ""},
		{"fallback with dash", nil, "ssh -t -oProxyCommand=id sessionhub resume " + uuid},
		{"fallback host is a dash", nil, "ssh -t -F/x sessionhub resume " + uuid},
	} {
		e, out := hostileEnv("bluebox", tc.ms, nil, dead)
		s := api.Session{ID: uuid, Machine: "tower", CWD: "/x", HerdrPane: "w1:p1", ResumeCommand: tc.resum}
		if tc.ms == nil && tc.resum == "" {
			s.Machine = "" // nothing names a host
		}
		_, err := e.resumeSession(context.Background(), s)
		if err == nil || out.Len() != 0 {
			t.Errorf("%s: err=%v out=%q, want refusal and no output", tc.name, err, out)
		}
	}
}

func TestAgentNameMatchesHerdrRule(t *testing.T) {
	fh := happyHerdr(t, "", "")
	e, _ := hostileEnv("bluebox", nil, nil, fh.path)
	id := "ABCDEF12-1111-4222-8333-444455556666"
	if _, err := e.resumeSession(context.Background(), api.Session{ID: id, Machine: "bluebox", CWD: "/x", HerdrPane: "w1:p1"}); err != nil {
		t.Fatal(err)
	}
	if name := fh.params("agent.start")["name"]; name != "sessionhub-abcdef12" {
		t.Errorf("name = %v, want sessionhub-abcdef12", name)
	}
}
