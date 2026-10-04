package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

const moveID = "mv_AAAAAAAAAAAAAAAAAAAAAA"

// fakeMoves is a moveAPI. states is what GetMove returns, one per read;
// the last one repeats.
type fakeMoves struct {
	detail      api.SessionDetail
	getErr      error
	createErr   error
	created     []string // "id target"
	states      []api.Move
	reads       int
	getMoveErrs []error // returned by GetMove first, one per read
}

func (f *fakeMoves) GetSession(context.Context, string) (api.SessionDetail, error) {
	return f.detail, f.getErr
}

func (f *fakeMoves) CreateMove(_ context.Context, id, target string) (api.Move, error) {
	f.created = append(f.created, id+" "+target)
	if f.createErr != nil {
		return api.Move{}, f.createErr
	}
	return api.Move{ID: moveID, SessionID: id, Source: "bluebox", Target: target, State: api.MoveRequested}, nil
}

func (f *fakeMoves) GetMove(context.Context, string) (api.Move, error) {
	if len(f.getMoveErrs) > 0 {
		err := f.getMoveErrs[0]
		f.getMoveErrs = f.getMoveErrs[1:]
		return api.Move{}, err
	}
	i := min(f.reads, len(f.states)-1)
	f.reads++
	return f.states[i], nil
}

func runMove(t *testing.T, f *fakeMoves, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	clock := now
	e := &env{moves: f, out: &out, now: func() time.Time { return clock }, pause: func(d time.Duration) { clock = clock.Add(d) }}
	err := e.move(context.Background(), args)
	return out.String(), err
}

func state(s string, mod func(*api.Move)) api.Move {
	m := api.Move{ID: moveID, Source: "bluebox", Target: "tower", State: s, BundleSize: 3 << 20}
	if mod != nil {
		mod(&m)
	}
	return m
}

func session() api.SessionDetail {
	return api.SessionDetail{Session: api.Session{ID: "3f2a9c10-1111-4a4a-8b8b-000000000001", Title: "auth work", Machine: "bluebox"}}
}

func TestMoveFollowsToDone(t *testing.T) {
	f := &fakeMoves{detail: session(), states: []api.Move{state(api.MovePacking, nil), state(api.MovePacking, nil),
		state(api.MoveUploaded, nil), state(api.MoveUnpacking, nil),
		state(api.MoveDone, func(m *api.Move) { m.Detail = "not carried: .env" })}}
	out, err := runMove(t, f, "3f2a", "tower")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 || f.created[0] != "3f2a9c10-1111-4a4a-8b8b-000000000001 tower" {
		t.Errorf("created %v", f.created)
	}
	want := moveID + "  moving 3f2a9c10 (auth work) from bluebox to tower\n" +
		"requested: waiting for the sessionhub watcher on bluebox\n" +
		"packing on bluebox: ending the session and sealing the bundle\n" +
		"uploaded (3.0 MiB): waiting for tower\n" +
		"unpacking on tower\n" +
		"done: the session runs on tower now; not carried: .env\n" +
		"see notes later with: sessionhub move --status " + moveID + "\n"
	if out != want {
		t.Errorf("output:\n%s\nwant:\n%s", out, want)
	}
}

func TestMoveFailedExitsOne(t *testing.T) {
	f := &fakeMoves{detail: session(), states: []api.Move{state(api.MoveFailed, func(m *api.Move) {
		m.Detail = "find clone: no clone of github.com/o/r on tower\x1b[31m"
	})}}
	out, err := runMove(t, f, "3f2a", "tower")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Fatalf("err %v", err)
	}
	if !strings.HasSuffix(out, "failed: find clone: no clone of github.com/o/r on tower[31m\nsee notes later with: sessionhub move --status "+moveID+"\n") ||
		strings.Contains(out, "\x1b") {
		t.Errorf("output %q", out)
	}
}

func TestMoveCloudDone(t *testing.T) {
	f := &fakeMoves{detail: session(), states: []api.Move{state(api.MoveDone, func(m *api.Move) {
		m.Target, m.CloudURL = api.MoveCloud, "https://claude.ai/code/session_01AbC"
	})}}
	out, err := runMove(t, f, "3f2a", "cloud")
	if err != nil || !strings.HasSuffix(out, "done: the session continues in the cloud at https://claude.ai/code/session_01AbC\n"+
		"see notes later with: sessionhub move --status "+moveID+"\n") {
		t.Errorf("output %q %v", out, err)
	}
}

func TestMoveGivesUpFollowing(t *testing.T) {
	f := &fakeMoves{detail: session(), states: []api.Move{state(api.MovePacking, nil)}}
	out, err := runMove(t, f, "3f2a", "tower")
	if err != nil || !strings.HasSuffix(out, "still packing after 3 minutes; check with: sessionhub move --status "+moveID+"\n") {
		t.Errorf("output %q %v", out, err)
	}
	if strings.Count(out, "packing on bluebox") != 1 {
		t.Errorf("a repeated state printed twice:\n%s", out)
	}
}

func TestMoveRefusedAndUsage(t *testing.T) {
	f := &fakeMoves{detail: session(), createErr: &client.StatusError{Status: 409,
		Message: "not controllable: the agent is working; a session moves only when it is idle or done"}}
	if _, err := runMove(t, f, "3f2a", "tower"); err == nil || !strings.Contains(err.Error(), "only when it is idle or done") {
		t.Errorf("refusal: %v", err)
	}
	f = &fakeMoves{getErr: &client.StatusError{Status: 404, Message: `not found: no session matches "zzzz"`}}
	if _, err := runMove(t, f, "zzzz", "tower"); err == nil || !strings.Contains(err.Error(), "no session matches") {
		t.Errorf("unknown session: %v", err)
	}
	for _, args := range [][]string{nil, {"3f2a"}, {"3f2a", "tower", "x"}, {"--status"}, {"--bogus", "x"}} {
		if _, err := runMove(t, &fakeMoves{}, args...); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("%q: %v", args, err)
		}
	}
}

func TestMoveStatus(t *testing.T) {
	f := &fakeMoves{states: []api.Move{state(api.MoveUnpacking, nil)}}
	out, err := runMove(t, f, "--status", moveID)
	if err != nil || out != moveID+"  unpacking on tower\n" {
		t.Errorf("status %q %v", out, err)
	}
	if len(f.created) != 0 {
		t.Error("--status started a move")
	}
}

func TestMoveKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "move.key")
	var out bytes.Buffer
	if err := moveKey(&out, path, nil); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}\n$`).MatchString(out.String()) {
		t.Errorf("output %q", out.String())
	}
	first := out.String()
	out.Reset()
	if err := moveKey(&out, path, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != first {
		t.Errorf("the fingerprint changed: %q then %q", first, out.String())
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", fi.Mode().Perm())
	}
	if err := moveKey(&out, path, []string{"x"}); err == nil {
		t.Error("an argument was accepted")
	}
}

func TestMoveInterrupted(t *testing.T) {
	f := &fakeMoves{detail: session(), states: []api.Move{state(api.MovePacking, nil)}}
	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	e := &env{moves: f, out: &out, now: func() time.Time { return now }, pause: func(time.Duration) { cancel() }}
	err := e.move(ctx, []string{"3f2a", "tower"})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 130 {
		t.Fatalf("err %v", err)
	}
	if !strings.HasSuffix(out.String(), "move "+moveID+" continues; check with: sessionhub move --status "+moveID+"\n") || strings.Contains(out.String(), "3 minutes") {
		t.Errorf("output %q", out.String())
	}
}

func TestMoveFollowReadErrors(t *testing.T) {
	boom := errors.New("network down\x1b[31m")
	f := &fakeMoves{detail: session(), states: []api.Move{state(api.MovePacking, nil), state(api.MoveDone, nil)},
		getMoveErrs: []error{boom, boom, boom, boom}}
	out, err := runMove(t, f, "3f2a", "tower")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "can't read move "+moveID+": network down[31m\n") != 1 || strings.Contains(out, "\x1b") ||
		!strings.HasSuffix(out, "done: the session runs on tower now\nsee notes later with: sessionhub move --status "+moveID+"\n") {
		t.Errorf("output %q", out)
	}
	f = &fakeMoves{detail: session(), states: []api.Move{state(api.MovePacking, nil)},
		getMoveErrs: []error{&client.StatusError{Status: 404, Message: "not found"}}}
	if _, err := runMove(t, f, "3f2a", "tower"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("404: %v", err)
	}
}
