package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as the crash helper: when SESSIONHUB_QUEUE_HELPER is set the test
// binary runs a queue operation and never reaches the tests.
func TestMain(m *testing.M) {
	switch os.Getenv("SESSIONHUB_QUEUE_HELPER") {
	case "append":
		helperAppend()
	case "drain-kill":
		helperDrainKill()
	}
	os.Exit(m.Run())
}

// helperAppend appends items forever, printing each ID only after Append
// returned (the item is then acknowledged and must survive a kill).
func helperAppend() {
	q := NewQueue(os.Getenv("SESSIONHUB_QUEUE_DIR"))
	prefix := os.Getenv("SESSIONHUB_QUEUE_PREFIX")
	out := bufio.NewWriter(os.Stdout)
	for i := 0; ; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i)
		if err := q.Append(Item{ID: id, Op: OpEvent, SessionID: "s", Body: json.RawMessage(`{"pad":"` + strings.Repeat("x", 2000) + `"}`)}); err != nil {
			os.Exit(2)
		}
		fmt.Fprintln(out, id)
		out.Flush()
	}
}

// helperDrainKill "sends" every item, then is killed before Drain rewrites
// the file.
func helperDrainKill() {
	q := NewQueue(os.Getenv("SESSIONHUB_QUEUE_DIR"))
	_ = q.Drain(func(items []Item) []Item {
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	})
	os.Exit(3)
}

func helperCmd(t *testing.T, mode, dir, prefix string) *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "SESSIONHUB_QUEUE_HELPER="+mode, "SESSIONHUB_QUEUE_DIR="+dir, "SESSIONHUB_QUEUE_PREFIX="+prefix)
	return cmd
}

func ids(items []Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func newItem(id string) Item {
	return Item{ID: id, Op: OpUpsert, SessionID: "s-" + id, PaneID: "w1:p1", Body: json.RawMessage(`{"id":"x"}`)}
}

func TestQueueAppendDrainRoundTrip(t *testing.T) {
	q := NewQueue(t.TempDir())
	if n, _ := q.Len(); n != 0 {
		t.Fatalf("new queue has %d items", n)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := q.Append(newItem(id)); err != nil {
			t.Fatal(err)
		}
	}
	st, err := os.Stat(q.file())
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("queue file mode = %v, err %v", st.Mode().Perm(), err)
	}
	// fn sends a and c, keeps b.
	err = q.Drain(func(items []Item) []Item {
		if got := strings.Join(ids(items), ","); got != "a,b,c" {
			t.Errorf("drain saw %s, want a,b,c", got)
		}
		return []Item{items[1]}
	})
	if err != nil {
		t.Fatal(err)
	}
	var left []Item
	q.Drain(func(items []Item) []Item { left = items; return items })
	if got := strings.Join(ids(left), ","); got != "b" {
		t.Fatalf("after drain queue = %s, want b", got)
	}
	if left[0].QueuedAt.IsZero() || left[0].PaneID != "w1:p1" || left[0].Op != OpUpsert {
		t.Errorf("item fields lost: %+v", left[0])
	}
	// Send everything: the file goes away.
	q.Drain(func(items []Item) []Item { return nil })
	if _, err := os.Stat(q.file()); !os.IsNotExist(err) {
		t.Errorf("queue file should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(q.file() + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind")
	}
}

func TestQueueDrainKeepsAllWhenNothingSent(t *testing.T) {
	q := NewQueue(t.TempDir())
	q.Append(newItem("a"))
	before, _ := os.ReadFile(q.file())
	q.Drain(func(items []Item) []Item { return items })
	after, _ := os.ReadFile(q.file())
	if string(before) != string(after) {
		t.Fatalf("file changed although nothing was sent")
	}
}

func TestQueueSurvivesTornLastLine(t *testing.T) {
	q := NewQueue(t.TempDir())
	q.Append(newItem("a"))
	f, _ := os.OpenFile(q.file(), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"id":"torn","op":"ev`) // crash mid-write, no newline
	f.Close()
	if err := q.Append(newItem("b")); err != nil {
		t.Fatal(err)
	}
	var got []Item
	q.Drain(func(items []Item) []Item { got = items; return nil })
	if s := strings.Join(ids(got), ","); s != "a,b" {
		t.Fatalf("items = %s, want a,b (torn line dropped, next item intact)", s)
	}
}

func TestQueueKillDuringAppendsLosesNothing(t *testing.T) {
	dir := t.TempDir()
	acked := map[string]bool{}
	for round := 0; round < 4; round++ {
		prefix := fmt.Sprintf("r%d", round)
		cmd := helperCmd(t, "append", dir, prefix)
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(out)
		n := 0
		for sc.Scan() {
			acked[sc.Text()] = true
			if n++; n == 20+round*15 {
				cmd.Process.Signal(syscall.SIGKILL)
				break
			}
		}
		for sc.Scan() { // ids printed before the kill landed
			acked[sc.Text()] = true
		}
		cmd.Wait()
	}
	if len(acked) == 0 {
		t.Fatal("helper acknowledged nothing")
	}
	var got []Item
	if err := NewQueue(dir).Drain(func(items []Item) []Item { got = items; return nil }); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, it := range got {
		seen[it.ID]++
	}
	for id := range acked {
		if seen[id] != 1 {
			t.Errorf("acknowledged item %s appears %d times, want 1", id, seen[id])
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("item %s duplicated %d times", id, n)
		}
	}
}

func TestQueueKillDuringDrainKeepsFileIntact(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue(dir)
	for _, id := range []string{"a", "b", "c"} {
		q.Append(newItem(id))
	}
	before, _ := os.ReadFile(q.file())
	cmd := helperCmd(t, "drain-kill", dir, "")
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != -1 {
		t.Fatalf("helper should die by signal, got %v", err)
	}
	after, _ := os.ReadFile(q.file())
	if string(before) != string(after) {
		t.Fatalf("queue file changed by a killed drain:\n%s\nvs\n%s", before, after)
	}
	// The dead process's lock is released: we can drain right away.
	done := make(chan []Item, 1)
	go func() {
		q.Drain(func(items []Item) []Item { done <- items; return nil })
	}()
	select {
	case items := <-done:
		if s := strings.Join(ids(items), ","); s != "a,b,c" {
			t.Fatalf("items = %s, want a,b,c (no loss, no duplicates)", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lock still held after the helper was killed")
	}
}

func TestQueueStaleTempFileIgnored(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue(dir)
	q.Append(newItem("a"))
	q.Append(newItem("b"))
	// A crash after writing the temp file but before the rename.
	os.WriteFile(filepath.Join(dir, "queue.jsonl.tmp"), []byte(`{"id":"ghost","op":"event"}`+"\n"), 0o600)
	q.Drain(func(items []Item) []Item { return items[1:] })
	var got []Item
	q.Drain(func(items []Item) []Item { got = items; return nil })
	if s := strings.Join(ids(got), ","); s != "b" {
		t.Fatalf("items = %s, want b", s)
	}
}

// A read error other than EOF must reach the caller before fn runs, so Drain
// never rewrites the queue from a partial read. A directory at the queue path
// opens fine and fails on read with EISDIR.
func TestDrainReturnsReadErrorAndKeepsFile(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue(dir)
	if err := os.Mkdir(q.file(), 0o700); err != nil {
		t.Fatal(err)
	}
	called := false
	err := q.Drain(func(items []Item) []Item { called = true; return nil })
	if err == nil {
		t.Fatal("Drain must return the read error")
	}
	if called {
		t.Error("fn must not run after a failed read")
	}
	if st, serr := os.Stat(q.file()); serr != nil || !st.IsDir() {
		t.Errorf("queue path must be untouched, stat = %v %v", st, serr)
	}
	if _, err := q.Len(); err == nil {
		t.Error("Len must return the read error")
	}
}
