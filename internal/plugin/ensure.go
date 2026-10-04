package plugin

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// State dir files owned by the plugin.
const (
	watcherLockFile = "watcher.lock" // flock held by the watcher; content is its PID
	watcherLogFile  = "watcher.log"  // watcher stderr
	maxWatcherLog   = 1 << 20        // truncated on start when larger
)

// tryLock opens path and takes a non-blocking exclusive flock. held is true
// when another process holds it; then f is nil.
func tryLock(path string) (f *os.File, held bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != syscall.EINTR {
			break
		}
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		f.Close()
		return nil, true, nil
	}
	if err != nil {
		f.Close()
		return nil, false, err
	}
	return f, false, nil
}

// watcherRunning reports whether a process holds <stateDir>/watcher.lock.
func watcherRunning(stateDir string) (bool, error) {
	f, held, err := tryLock(filepath.Join(stateDir, watcherLockFile))
	if err != nil {
		return false, err
	}
	if f != nil {
		f.Close()
	}
	return held, nil
}

// ensureWatcher starts `<exe> plugin watch` detached when no watcher holds
// the lock. The lock is only probed here and released before the start, as
// the brief says; the watcher takes it itself and exits at once when another
// watcher won a race, so concurrent calls leave exactly one watcher. The
// returned process (nil when none was started) is only for tests to reap.
func ensureWatcher(stateDir, exe string) (*os.Process, error) {
	running, err := watcherRunning(stateDir)
	if err != nil || running {
		return nil, err
	}
	return spawnWatcher(stateDir, exe)
}

func spawnWatcher(stateDir, exe string) (*os.Process, error) {
	logPath := filepath.Join(stateDir, watcherLogFile)
	if st, err := os.Stat(logPath); err == nil && st.Size() > maxWatcherLog {
		os.Truncate(logPath, 0)
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer devnull.Close()
	cmd := exec.Command(exe, "plugin", "watch")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = watcherEnv(os.Environ())
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

// watcherEnv drops the per-event variables; the watcher outlives the event.
func watcherEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "HERDR_PLUGIN_EVENT") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// stopWatcher sends SIGTERM to the watcher that holds the lock and waits up to
// timeout for it to release the lock. It checks the process's command line
// first (commandLine), so a reused PID is never signalled. It reports whether a watcher was stopped.
func stopWatcher(stateDir string, timeout time.Duration) (bool, error) {
	path := filepath.Join(stateDir, watcherLockFile)
	running, err := watcherRunning(stateDir)
	if err != nil || !running {
		return false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return false, fmt.Errorf("watcher lock %s has no PID", path)
	}
	cmdline, err := commandLine(pid)
	if err != nil || !strings.Contains(cmdline, " plugin watch") {
		return false, fmt.Errorf("process %d holding %s is not a sessionhub watcher", pid, path)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return false, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if running, _ := watcherRunning(stateDir); !running {
			return true, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false, fmt.Errorf("watcher %d did not exit within %s", pid, timeout)
}
