package client

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/abdallah/session-hub/internal/paths"
)

// Queue item ops.
const (
	OpUpsert        = "upsert"
	OpEvent         = "event"
	OpHerdrSessions = "herdr_sessions"
)

// Item is one queued request. Body is the JSON body of the matching API
// call: api.SessionUpsert for "upsert", api.EventIn for "event" (SessionID
// set), api.HerdrSessionsPut for "herdr_sessions".
type Item struct {
	ID        string          `json:"id,omitempty"` // random; lets a consumer detect a replay
	Op        string          `json:"op"`
	SessionID string          `json:"session_id,omitempty"`
	PaneID    string          `json:"pane_id,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
	QueuedAt  time.Time       `json:"queued_at"`
}

// Queue is an append-only JSONL file guarded by an flock, safe across
// processes.
type Queue struct {
	dir string
}

// NewQueue returns a queue rooted at dir (queue.jsonl and queue.lock).
func NewQueue(dir string) *Queue { return &Queue{dir: dir} }

// DefaultQueue returns the queue under paths.StateDir().
func DefaultQueue() *Queue { return NewQueue(paths.StateDir()) }

func (q *Queue) file() string { return filepath.Join(q.dir, "queue.jsonl") }

func (q *Queue) lock() (unlock func(), err error) {
	if err := os.MkdirAll(q.dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(q.dir, "queue.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil // closing the fd releases the flock
}

// Append adds one item: lock, write one line, fsync. It sets QueuedAt and ID
// when empty. If a crash left a partial last line, Append starts a new line so
// the partial line cannot swallow this item.
func (q *Queue) Append(it Item) error {
	if it.QueuedAt.IsZero() {
		it.QueuedAt = time.Now().UTC()
	}
	if it.ID == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		it.ID = hex.EncodeToString(b[:])
	}
	line, err := json.Marshal(it)
	if err != nil {
		return err
	}
	unlock, err := q.lock()
	if err != nil {
		return err
	}
	defer unlock()
	f, err := os.OpenFile(q.file(), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := line
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], st.Size()-1); err == nil && last[0] != '\n' {
			buf = append([]byte{'\n'}, line...)
		}
	}
	buf = append(buf, '\n')
	if _, err := f.Write(buf); err != nil {
		return err
	}
	return f.Sync()
}

func (q *Queue) read() ([]Item, error) {
	f, err := os.Open(q.file())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var items []Item
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 {
			var it Item
			// A line cut short by a crash is dropped; it was never acknowledged.
			if json.Unmarshal(t, &it) == nil && it.Op != "" {
				items = append(items, it)
			}
		}
		if err != nil {
			if err != io.EOF {
				// A partial read must not look like a short queue: Drain
				// would rewrite the file without the unread items.
				return nil, err
			}
			break
		}
	}
	return items, nil
}

// Len returns the number of queued items (takes the lock).
func (q *Queue) Len() (int, error) {
	unlock, err := q.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	items, err := q.read()
	return len(items), err
}

// Drain holds the lock, reads all items, and calls fn. fn returns the items
// that were NOT delivered; the file is atomically replaced with exactly those
// (or removed when none remain).
//
// Delivery is at-least-once: a kill after fn sent items but before the rename
// leaves them queued, and the next Drain sends them again. Item.ID is stable
// across that replay. The queue file itself is never torn: it is always
// either the old or the new content. Append waits on the lock while fn runs,
// so fn should stay within the HTTP timeout per item.
func (q *Queue) Drain(fn func(items []Item) (unsent []Item)) error {
	unlock, err := q.lock()
	if err != nil {
		return err
	}
	defer unlock()
	items, err := q.read()
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	rest := fn(items)
	if sameItems(rest, items) {
		return nil
	}
	if len(rest) == 0 {
		if err := os.Remove(q.file()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return syncDir(q.dir)
	}
	tmp := q.file() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, it := range rest {
		b, err := json.Marshal(it)
		if err != nil {
			f.Close()
			return err
		}
		w.Write(b)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, q.file()); err != nil {
		return err
	}
	return syncDir(q.dir)
}

func sameItems(a, b []Item) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || !a[i].QueuedAt.Equal(b[i].QueuedAt) {
			return false
		}
	}
	return true
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
