package plugin

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// fakeRex puts a rex on PATH that answers `block call` from a state dir:
// format returns the JSON string in frame, write appends its args to
// writes, and once gone exists every call fails the way rex does.
func fakeRex(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "rex"), `#!/bin/sh
state="$FAKE_REX_STATE"
[ "$1 $2 $3 $5" = "block call -s -b" ] || { echo "bad args: $*" >&2; exit 2; }
[ -e "$state/gone" ] && { echo "Error: block target \"$6\" not found: session scope: session \"$4\" not found" >&2; exit 3; }
case "$7" in
  format) printf '{"content":%s}' "$(cat "$state/frame")" ;;
  process) if [ -e "$state/noclaude" ]; then echo '{"foreground":{"pid":2,"user_id":1000,"name":"zsh"}}'; else echo '{"foreground":{"pid":2,"user_id":1000,"name":"claude"},"ancestors":[{"pid":1,"user_id":1000,"name":"zsh"}]}'; fi ;;
  write) printf '%s\n' "$8" >> "$state/writes"; echo '{"bytes_written":1}' ;;
  *) echo "unknown method $7" >&2; exit 2 ;;
esac
`, 0o755)
	writeFile(t, filepath.Join(state, "frame"), `"A"`, 0o644)
	t.Setenv("FAKE_REX_STATE", state)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return state
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRexMirror(t *testing.T) {
	state := fakeRex(t)
	s := api.Session{ID: "s1", RexSession: "session:1", RexBlock: "block:1"}
	m := rexMirror{bin: "rex", every: 10 * time.Millisecond}
	out := &safeBuffer{}
	in, guest := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- m.Run(context.Background(), s, out, in) }()

	waitUntil(t, "first frame", func() bool { return strings.Contains(out.String(), clearScreen+"A") })
	time.Sleep(60 * time.Millisecond) // several unchanged polls
	if n := strings.Count(out.String(), clearScreen); n != 1 {
		t.Errorf("unchanged frame emitted %d times, want 1", n)
	}

	writeFile(t, filepath.Join(state, "frame"), `"B"`, 0o644)
	waitUntil(t, "second frame", func() bool { return strings.HasSuffix(out.String(), clearScreen+"B") })

	if _, err := io.WriteString(guest, "ls\n"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "write forwarded", func() bool {
		b, _ := os.ReadFile(filepath.Join(state, "writes"))
		return strings.Contains(string(b), `{"data":"ls\n"}`)
	})

	// Once Claude is gone from the block, Run ends as "session ended" and
	// nothing more is typed.
	writeFile(t, filepath.Join(state, "noclaude"), "", 0o644)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after Claude exited = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the block was gone")
	}
	guest.Close()
}

func TestRexMirrorCancel(t *testing.T) {
	fakeRex(t)
	s := api.Session{ID: "s1", RexSession: "session:1", RexBlock: "block:1"}
	m := rexMirror{bin: "rex", every: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	in, guest := io.Pipe()
	defer guest.Close()
	go func() { done <- m.Run(ctx, s, &safeBuffer{}, in) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancel = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRexMirrorNoBlock(t *testing.T) {
	err := rexMirror{bin: "rex", every: time.Millisecond}.Run(context.Background(), api.Session{ID: "s1"}, io.Discard, strings.NewReader(""))
	if err == nil {
		t.Error("Run without a Rex block returned nil")
	}
}
