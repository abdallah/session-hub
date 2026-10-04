package digest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sid = "11111111-2222-3333-4444-555555555555"

// line marshals one transcript entry.
func line(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func ts(min int) string {
	return time.Date(2026, 9, 30, 10, min, 0, 0, time.UTC).Format(time.RFC3339Nano)
}

func assistant(t *testing.T, id string, min int, out int) string {
	return line(t, map[string]any{"type": "assistant", "timestamp": ts(min), "cwd": "/work/repo",
		"message": map[string]any{"id": id, "content": []any{map[string]any{"type": "text", "text": "reply text"}},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": out, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 10}}})
}

// setup makes <claude>/projects/-work-repo/<sid>.jsonl with content and
// returns a Reader over temp dirs and the transcript path.
func setup(t *testing.T, content string) (Reader, string) {
	t.Helper()
	claude := t.TempDir()
	dir := filepath.Join(claude, "projects", "-work-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return Reader{ClaudeDir: claude, StateDir: t.TempDir()}, p
}

func appendTo(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestReadExtractsFields(t *testing.T) {
	content := line(t, map[string]any{"type": "user", "timestamp": ts(0), "cwd": "/work/repo",
		"message": map[string]any{"role": "user", "content": "do the thing"}}) +
		// Claude Code writes one reply several times with the same message ID.
		assistant(t, "msg_1", 1, 50) + assistant(t, "msg_1", 1, 50) + assistant(t, "msg_2", 2, 25) +
		line(t, map[string]any{"type": "ai-title", "aiTitle": "First title"}) +
		line(t, map[string]any{"type": "ai-title", "aiTitle": "Better title"}) +
		line(t, map[string]any{"type": "system", "subtype": "away_summary", "timestamp": ts(3),
			"content": "We did X.\nNext, do Y. (disable recaps in /config)"}) +
		line(t, map[string]any{"type": "pr-link", "prNumber": 391, "prUrl": "https://git.example.com/o/r/-/merge_requests/391",
			"prRepository": "o/r", "timestamp": ts(4)}) +
		line(t, map[string]any{"type": "pr-link", "prNumber": "391", "prUrl": "https://git.example.com/o/r/-/merge_requests/391"}) +
		line(t, map[string]any{"type": "pr-link", "prNumber": 5, "prUrl": "javascript:alert(1)"}) +
		line(t, map[string]any{"type": "some-future-type", "whatever": true}) +
		line(t, map[string]any{"type": "cost-state", "totalCostUSD": 1.25}) +
		line(t, map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": ts(5)})
	r, _ := setup(t, content)
	st, err := r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := st.DigestIn()
	if !ok {
		t.Fatal("DigestIn not ok")
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"output tokens (msg_1 once)", d.Tokens.Output, int64(75)},
		{"cache read", d.Tokens.CacheRead, int64(200)},
		{"ai title", d.AITitle, "Better title"},
		{"recap: newlines folded, suffix removed", d.Recap, "We did X. Next, do Y."},
		{"recap at", d.RecapAt.Format(time.RFC3339), "2026-09-30T10:03:00Z"},
		{"links (deduplicated, javascript dropped)", len(d.Links), 1},
		{"link number from a JSON number", d.Links[0].Number, "391"},
		{"cost", *d.CostUSD, 1.25},
		{"cost at: the preceding timed entry", d.CostAt.Format(time.RFC3339), "2026-09-30T10:04:00Z"},
		{"first at", d.FirstAt.Format(time.RFC3339), "2026-09-30T10:00:00Z"},
		{"as of", d.AsOf.Format(time.RFC3339), "2026-09-30T10:05:00Z"},
		{"cwd", st.CWD, "/work/repo"},
		{"bad lines", d.BadLines, 0},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	// The reply text must never reach the digest.
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "reply text") {
		t.Errorf("digest contains reply text: %s", b)
	}
}

func TestReadIsIncremental(t *testing.T) {
	r, p := setup(t, assistant(t, "msg_1", 1, 10))
	if _, err := r.Read(sid); err != nil {
		t.Fatal(err)
	}
	// A partial line: written without its newline yet.
	partial := assistant(t, "msg_2", 2, 20)
	appendTo(t, p, partial[:len(partial)/2])
	st, err := r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tokens.Output != 10 || st.BadLines != 0 {
		t.Fatalf("after partial line: output=%d bad=%d, want 10, 0", st.Tokens.Output, st.BadLines)
	}
	appendTo(t, p, partial[len(partial)/2:])
	st, err = r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tokens.Output != 30 {
		t.Errorf("after the line completed: output=%d, want 30", st.Tokens.Output)
	}
	// Reading again with nothing new changes nothing.
	st, _ = r.Read(sid)
	if st.Tokens.Output != 30 {
		t.Errorf("idle read: output=%d, want 30", st.Tokens.Output)
	}
}

func TestReadStartsOverWhenReplaced(t *testing.T) {
	r, p := setup(t, assistant(t, "msg_1", 1, 10)+assistant(t, "msg_2", 2, 10))
	if _, err := r.Read(sid); err != nil {
		t.Fatal(err)
	}
	// Shrunk: a new, shorter file at the same path.
	if err := os.WriteFile(p+".new", []byte(assistant(t, "msg_9", 3, 7)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p+".new", p); err != nil {
		t.Fatal(err)
	}
	st, err := r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tokens.Output != 7 {
		t.Errorf("after replace: output=%d, want 7 (read from the start)", st.Tokens.Output)
	}
}

func TestReadSubagents(t *testing.T) {
	r, p := setup(t, assistant(t, "msg_1", 1, 10))
	sub := filepath.Join(strings.TrimSuffix(p, ".jsonl"), "subagents")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	// A subagent's recap-like entry must not become the session's recap.
	subContent := assistant(t, "msg_s1", 1, 5) + line(t, map[string]any{"type": "system", "subtype": "away_summary", "content": "subagent"})
	if err := os.WriteFile(filepath.Join(sub, "agent-a.jsonl"), []byte(subContent), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tokens.Output != 15 || st.Recap != "" {
		t.Errorf("output=%d recap=%q, want 15 and no recap", st.Tokens.Output, st.Recap)
	}
}

func TestReadBadLines(t *testing.T) {
	long := `{"type":"user","x":"` + strings.Repeat("a", 1<<20) + `"}` + "\n"
	r, p := setup(t, assistant(t, "msg_1", 1, 10)+"not json\n"+long+assistant(t, "msg_2", 2, 10)+assistant(t, "msg_3", 3, 10))
	st, err := r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.BadLines != 2 || st.Tokens.Output != 30 {
		t.Errorf("bad=%d output=%d, want 2 and 30", st.BadLines, st.Tokens.Output)
	}
	// Mostly garbage: nothing is saved, the next good run starts from the
	// same offset.
	appendTo(t, p, "garbage\ngarbage\ngarbage\n"+assistant(t, "msg_4", 4, 10))
	if _, err := r.Read(sid); !errors.Is(err, ErrTooManyBad) {
		t.Fatalf("err = %v, want ErrTooManyBad", err)
	}
	st, err = Reader{ClaudeDir: r.ClaudeDir, StateDir: r.StateDir}.loadOnly(sid)
	if err != nil || st.Tokens.Output != 30 {
		t.Errorf("saved state after refused run: output=%d err=%v, want 30", st.Tokens.Output, err)
	}
}

// loadOnly returns the saved state without reading the transcript.
func (r Reader) loadOnly(id string) (*State, error) {
	return loadState(r.stateDir(), id), nil
}

func TestReadWrongTypedFieldIsNotBad(t *testing.T) {
	// A field of an unexpected type (a future format change) leaves that
	// field empty but keeps the rest of the entry.
	r, _ := setup(t, `{"type":"assistant","timestamp":"`+ts(1)+`","cwd":42,"message":{"id":"m","usage":{"output_tokens":9}}}`+"\n")
	st, err := r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.BadLines != 0 || st.Tokens.Output != 9 {
		t.Errorf("bad=%d output=%d, want 0 and 9", st.BadLines, st.Tokens.Output)
	}
}

func TestReadNoTranscriptAndLocked(t *testing.T) {
	r := Reader{ClaudeDir: t.TempDir(), StateDir: t.TempDir()}
	if _, err := r.Read(sid); !errors.Is(err, ErrNoTranscript) {
		t.Errorf("missing: %v, want ErrNoTranscript", err)
	}
	if _, err := r.Read("../../etc/passwd"); !errors.Is(err, ErrNoTranscript) {
		t.Errorf("path-like id: %v, want ErrNoTranscript", err)
	}
	r2, _ := setup(t, assistant(t, "msg_1", 1, 10))
	unlock, ok, err := lock(r2.StateDir, sid)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	defer unlock()
	if _, err := r2.Read(sid); !errors.Is(err, ErrLocked) {
		t.Errorf("held lock: %v, want ErrLocked", err)
	}
}
