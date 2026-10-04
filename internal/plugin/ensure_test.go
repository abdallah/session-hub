package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Concurrent ensureWatcher calls leave exactly one watcher holding the lock,
// and a call while it runs starts nothing. The helper "sessionhub" is this test
// binary (see TestMain).
func TestEnsureWatcherConcurrent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SESSIONHUB_STATE_DIR", dir)
	t.Setenv("SESSIONHUB_PLUGIN_TEST_HELPER", "watch")
	if running, _ := watcherRunning(dir); running {
		t.Fatal("lock held before the test")
	}

	const calls = 12
	var mu sync.Mutex
	var procs []*os.Process
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := ensureWatcher(dir, os.Args[0])
			if err != nil {
				t.Error(err)
			}
			if p != nil {
				mu.Lock()
				procs = append(procs, p)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(procs) == 0 {
		t.Fatal("no watcher started")
	}
	// Every started process but the lock holder exits at once.
	exited := make(chan struct{}, len(procs))
	for _, p := range procs {
		go func(p *os.Process) { p.Wait(); exited <- struct{}{} }(p)
	}
	for i := 0; i < len(procs)-1; i++ {
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d losing watchers exited", i, len(procs)-1)
		}
	}
	holders := func() []string {
		b, _ := os.ReadFile(filepath.Join(dir, "holders"))
		return strings.Fields(string(b))
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(holders()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h := holders(); len(h) != 1 {
		t.Fatalf("watchers that took the lock: %v, want exactly 1 (of %d started, %d calls)", h, len(procs), calls)
	}
	if running, _ := watcherRunning(dir); !running {
		t.Fatal("lock not held by the watcher")
	}
	// With a watcher running, ensureWatcher starts nothing.
	if p, err := ensureWatcher(dir, os.Args[0]); p != nil || err != nil {
		reap(p)
		t.Fatalf("second ensureWatcher started %v (err %v)", p, err)
	}
	t.Logf("%d concurrent calls started %d processes; 1 holds the lock", calls, len(procs))

	os.WriteFile(filepath.Join(dir, "stop"), nil, 0o600)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("holder did not exit")
	}
	if running, _ := watcherRunning(dir); running {
		t.Error("lock still held after the watcher exited")
	}
}

// stopWatcher signals the lock holder and waits for the lock to free.
func TestStopWatcher(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SESSIONHUB_STATE_DIR", dir)
	t.Setenv("SESSIONHUB_PLUGIN_TEST_HELPER", "watch")
	if stopped, err := stopWatcher(dir, time.Second); stopped || err != nil {
		t.Fatalf("no watcher: stopped=%v err=%v", stopped, err)
	}
	p, err := ensureWatcher(dir, os.Args[0])
	if err != nil || p == nil {
		t.Fatalf("start: %v %v", p, err)
	}
	go p.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if b, _ := os.ReadFile(filepath.Join(dir, "holders")); len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper never took the lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopped, err := stopWatcher(dir, 3*time.Second)
	if err != nil || !stopped {
		t.Fatalf("stopped=%v err=%v", stopped, err)
	}
	if running, _ := watcherRunning(dir); running {
		t.Error("lock still held")
	}
}

// The log is truncated on start when it passed 1 MiB, and kept otherwise.
func TestWatcherLogCap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SESSIONHUB_PLUGIN_TEST_HELPER", "exit")
	path := filepath.Join(dir, watcherLogFile)
	os.WriteFile(path, make([]byte, maxWatcherLog+1), 0o600)
	p, err := spawnWatcher(dir, os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	reap(p)
	if st, _ := os.Stat(path); st.Size() != 0 {
		t.Errorf("log size %d after start, want 0", st.Size())
	}
	os.WriteFile(path, []byte("keep\n"), 0o600)
	p, err = spawnWatcher(dir, os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	reap(p)
	if b, _ := os.ReadFile(path); string(b) != "keep\n" {
		t.Errorf("small log changed: %q", b)
	}
}

func TestWatcherEnvDropsEventVars(t *testing.T) {
	got := watcherEnv([]string{"A=1", "HERDR_PLUGIN_EVENT=pane.closed", "HERDR_PLUGIN_EVENT_JSON={}", "HERDR_SOCKET_PATH=/s"})
	if strings.Join(got, " ") != "A=1 HERDR_SOCKET_PATH=/s" {
		t.Errorf("env %v", got)
	}
}
