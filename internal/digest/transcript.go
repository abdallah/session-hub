// Package digest builds a session's digest from its Claude Code transcript
// and git, for PUT /v1/sessions/{id}/digest. The transcript format is not
// documented, so every field is optional and unknown entries are skipped.
// See docs/client.md, "Digests".
package digest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
)

const (
	maxLine      = 1 << 20 // longer lines are skipped and counted as bad
	maxRecap     = 600
	maxTitle     = 200
	maxLinks     = 20
	maxLinkBytes = 300
	recapSuffix  = "(disable recaps in /config)"
	stateVersion = 1
)

var (
	ErrNoTranscript = errors.New("no transcript for this session")
	ErrLocked       = errors.New("another digest run holds the lock")
	ErrTooManyBad   = errors.New("more than half the new transcript lines are unreadable")

	idRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	prNumberRE = regexp.MustCompile(`^[0-9]{1,10}$`)
)

// fileState is how far one transcript file has been read.
type fileState struct {
	Offset int64  `json:"offset"`
	Dev    uint64 `json:"dev"`
	Ino    uint64 `json:"ino"`
}

// State is the running result of reading one session's transcripts, saved
// between runs so each run reads only new lines.
type State struct {
	V           int                  `json:"v"`
	Files       map[string]fileState `json:"files"` // main and subagent files
	Seen        map[string]bool      `json:"seen"`  // assistant message IDs already counted
	Tokens      api.DigestTokens     `json:"tokens"`
	Recap       string               `json:"recap,omitempty"`
	RecapAt     *time.Time           `json:"recap_at,omitempty"`
	AITitle     string               `json:"ai_title,omitempty"`
	CustomTitle string               `json:"custom_title,omitempty"`
	Links       []api.DigestLink     `json:"links,omitempty"`
	CostUSD     *float64             `json:"cost_usd,omitempty"`
	CostAt      *time.Time           `json:"cost_at,omitempty"`
	FirstAt     *time.Time           `json:"first_at,omitempty"`
	AsOf        *time.Time           `json:"as_of,omitempty"`
	CWD         string               `json:"cwd,omitempty"`
	BadLines    int                  `json:"bad_lines"`
}

func newState() *State {
	return &State{V: stateVersion, Files: map[string]fileState{}, Seen: map[string]bool{}}
}

type usage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

// entry holds the transcript fields the digest reads. The message content
// (the reply and prompt text) is deliberately not declared.
type entry struct {
	Type         string          `json:"type"`
	Subtype      string          `json:"subtype"`
	Timestamp    string          `json:"timestamp"`
	CWD          string          `json:"cwd"`
	Content      json.RawMessage `json:"content"`
	AITitle      string          `json:"aiTitle"`
	CustomTitle  string          `json:"customTitle"`
	PRNumber     json.RawMessage `json:"prNumber"`
	PRURL        string          `json:"prUrl"`
	PRRepository string          `json:"prRepository"`
	TotalCostUSD *float64        `json:"totalCostUSD"`
	Message      *struct {
		ID    string `json:"id"`
		Usage *usage `json:"usage"`
	} `json:"message"`
}

// DefaultClaudeDir is CLAUDE_CONFIG_DIR, else ~/.claude.
func DefaultClaudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// TranscriptPath finds <claudeDir>/projects/*/<id>.jsonl. Session IDs are
// unique, so it needs exactly one match; it never rebuilds the project
// folder name from the working directory.
func TranscriptPath(claudeDir, id string) (string, error) {
	if !idRE.MatchString(id) {
		return "", ErrNoTranscript
	}
	m, err := filepath.Glob(filepath.Join(claudeDir, "projects", "*", id+".jsonl"))
	if err != nil || len(m) != 1 {
		return "", ErrNoTranscript
	}
	return m[0], nil
}

func identity(fi os.FileInfo) (dev, ino uint64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), uint64(st.Ino)
	}
	return 0, 0
}

// replaced reports whether the file at p is not the one st read: it shrank
// below the saved offset or has a different inode.
func (st *State) replaced(p string) bool {
	fs, ok := st.Files[p]
	if !ok {
		return false
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false // gone; a missing subagent file just stops adding
	}
	dev, ino := identity(fi)
	return fi.Size() < fs.Offset || dev != fs.Dev || ino != fs.Ino
}

// update reads the new lines of the main transcript and its subagent files
// into st. It starts over when any file was replaced. It returns a nil state
// and ErrTooManyBad when more than half the lines read were bad; the caller
// then keeps its saved state.
func update(st *State, mainPath string) (*State, error) {
	subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(mainPath, ".jsonl"), "subagents", "*.jsonl"))
	sort.Strings(subs)
	files := append([]string{mainPath}, subs...)
	for _, p := range files {
		if st.replaced(p) {
			st = newState()
			break
		}
	}
	var lines, bad int
	for i, p := range files {
		n, b, err := st.readFile(p, i == 0)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			continue
		}
		lines, bad = lines+n, bad+b
	}
	if bad*2 > lines {
		return nil, ErrTooManyBad
	}
	st.BadLines += bad
	return st, nil
}

// readFile applies every complete line after the saved offset. A last line
// without a newline is left for the next run. main is false for subagent
// files, which only add tokens.
func (st *State) readFile(p string, main bool) (lines, bad int, err error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	dev, ino := identity(fi)
	off := st.Files[p].Offset
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, 0, err
	}
	br := bufio.NewReaderSize(f, 64<<10)
	var buf []byte
	var pending int64
	skipping := false
	for {
		chunk, err := br.ReadSlice('\n')
		pending += int64(len(chunk))
		if !skipping {
			if len(buf)+len(chunk) > maxLine {
				skipping, buf = true, buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == bufio.ErrBufferFull:
			continue
		case err == io.EOF:
			st.Files[p] = fileState{Offset: off, Dev: dev, Ino: ino}
			return lines, bad, nil
		case err != nil:
			return lines, bad, err
		}
		off, pending = off+pending, 0
		if l := bytes.TrimSpace(buf); skipping || len(l) > 0 {
			lines++
			if skipping || !st.apply(l, main) {
				bad++
			}
		}
		skipping, buf = false, buf[:0]
	}
}

// apply folds one line into st. It returns false when the line isn't JSON.
func (st *State) apply(line []byte, main bool) bool {
	var e entry
	if err := json.Unmarshal(line, &e); err != nil {
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			return false
		}
		// A field of an unexpected type stays empty; the rest is used.
	}
	if e.Type == "assistant" && e.Message != nil && e.Message.Usage != nil && e.Message.ID != "" && !st.Seen[e.Message.ID] {
		st.Seen[e.Message.ID] = true
		u := e.Message.Usage
		st.Tokens.Input += u.Input
		st.Tokens.Output += u.Output
		st.Tokens.CacheRead += u.CacheRead
		st.Tokens.CacheWrite += u.CacheWrite
	}
	if !main {
		return true
	}
	at, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	hasTS := err == nil
	if hasTS {
		at = at.UTC()
		if st.FirstAt == nil {
			first := at
			st.FirstAt = &first
		}
		if st.AsOf == nil || at.After(*st.AsOf) {
			last := at
			st.AsOf = &last
		}
	}
	if e.CWD != "" {
		st.CWD = e.CWD
	}
	switch {
	case e.Type == "system" && e.Subtype == "away_summary":
		var s string
		if json.Unmarshal(e.Content, &s) == nil {
			s = strings.TrimSuffix(strings.TrimSpace(s), recapSuffix)
			if s = termtext.Clean(s, maxRecap); s != "" {
				st.Recap, st.RecapAt = s, nil
				if hasTS {
					r := at
					st.RecapAt = &r
				}
			}
		}
	case e.Type == "ai-title":
		if s := termtext.Clean(e.AITitle, maxTitle); s != "" {
			st.AITitle = s
		}
	case e.Type == "custom-title":
		if s := termtext.Clean(e.CustomTitle, maxTitle); s != "" {
			st.CustomTitle = s
		}
	case e.Type == "pr-link":
		st.addLink(e)
	case e.Type == "cost-state" && e.TotalCostUSD != nil:
		// cost-state has no timestamp: use the last timed entry before it.
		c := *e.TotalCostUSD
		st.CostUSD, st.CostAt = &c, nil
		if st.AsOf != nil {
			t := *st.AsOf
			st.CostAt = &t
		}
	}
	return true
}

// addLink keeps https links only, once per URL, the newest maxLinks.
func (st *State) addLink(e entry) {
	u := strings.TrimSpace(e.PRURL)
	if !strings.HasPrefix(u, "https://") || len(u) > maxLinkBytes || termtext.Clean(u, 0) != u {
		return
	}
	for _, l := range st.Links {
		if l.URL == u {
			return
		}
	}
	num := strings.Trim(string(e.PRNumber), `"`)
	if !prNumberRE.MatchString(num) {
		num = ""
	}
	st.Links = append(st.Links, api.DigestLink{Number: num, URL: u, Repo: termtext.Clean(e.PRRepository, maxTitle)})
	if len(st.Links) > maxLinks {
		st.Links = st.Links[len(st.Links)-maxLinks:]
	}
}

// DigestIn is the API body for this state, without git. ok is false until
// a timed entry has been read.
func (st *State) DigestIn() (api.DigestIn, bool) {
	if st.AsOf == nil {
		return api.DigestIn{}, false
	}
	return api.DigestIn{
		AsOf: *st.AsOf, FirstAt: st.FirstAt, Recap: st.Recap, RecapAt: st.RecapAt,
		AITitle: st.AITitle, CustomTitle: st.CustomTitle, Links: st.Links, Tokens: st.Tokens,
		CostUSD: st.CostUSD, CostAt: st.CostAt, BadLines: st.BadLines,
	}, true
}
