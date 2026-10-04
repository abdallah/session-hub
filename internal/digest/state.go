package digest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/abdallah/session-hub/internal/paths"
)

// Reader reads transcripts and keeps per-session state. Empty fields take
// DefaultClaudeDir and DefaultStateDir.
type Reader struct {
	ClaudeDir string
	StateDir  string
}

// DefaultStateDir is <sessionhub state dir>/digest.
func DefaultStateDir() string { return filepath.Join(paths.StateDir(), "digest") }

func (r Reader) claudeDir() string {
	if r.ClaudeDir != "" {
		return r.ClaudeDir
	}
	return DefaultClaudeDir()
}

func (r Reader) stateDir() string {
	if r.StateDir != "" {
		return r.StateDir
	}
	return DefaultStateDir()
}

// Read takes the session's lock, reads new transcript lines, saves the
// state, and returns it. It returns ErrLocked at once when another run holds
// the lock.
func (r Reader) Read(id string) (*State, error) {
	path, err := TranscriptPath(r.claudeDir(), id)
	if err != nil {
		return nil, err
	}
	unlock, ok, err := lock(r.stateDir(), id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrLocked
	}
	defer unlock()
	st, err := update(loadState(r.stateDir(), id), path)
	if err != nil {
		return nil, err
	}
	if err := saveState(r.stateDir(), id, st); err != nil {
		return nil, err
	}
	return st, nil
}

// loadState returns the saved state, or a fresh one when the file is
// missing, unreadable, or from another state version.
func loadState(dir, id string) *State {
	b, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return newState()
	}
	st := newState()
	if json.Unmarshal(b, st) != nil || st.V != stateVersion || st.Files == nil || st.Seen == nil {
		return newState()
	}
	return st
}

// saveState writes the state atomically (temp file and rename).
func saveState(dir, id string, st *State) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, id+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, id+".json"))
}

// lock takes <dir>/<id>.lock without waiting. ok is false when another
// process holds it.
func lock(dir, id string) (unlock func(), ok bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { f.Close() }, true, nil
}
