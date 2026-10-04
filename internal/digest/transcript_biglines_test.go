package digest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadLongLinesAndPrivacy(t *testing.T) {
	big := strings.Repeat("x", 300<<10)   // 300 KB, crosses the 64 KB buffer
	huge := strings.Repeat("y", 1100<<10) // over 1 MB
	content := assistant(t, "m1", 1, 5) +
		line(t, map[string]any{"type": "user", "timestamp": ts(2), "message": map[string]any{"content": "SECRETPROMPT " + big}}) +
		assistant(t, "m2", 3, 7) +
		line(t, map[string]any{"type": "user", "timestamp": ts(4), "message": map[string]any{"content": huge}}) +
		line(t, map[string]any{"type": "system", "subtype": "away_summary", "timestamp": ts(5), "content": "Did things\nacross lines (disable recaps in /config)"}) +
		assistant(t, "m3", 6, 11)
	r, p := setup(t, content)
	st, err := r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := st.DigestIn()
	if d.Tokens.Output != 23 || st.BadLines != 1 || d.Recap != "Did things across lines" {
		t.Fatalf("tokens %d bad %d recap %q", d.Tokens.Output, st.BadLines, d.Recap)
	}
	fi, _ := os.Stat(p)
	if st.Files[p].Offset != fi.Size() {
		t.Fatalf("offset %d size %d", st.Files[p].Offset, fi.Size())
	}
	b, _ := json.Marshal(d)
	sb, _ := os.ReadFile(filepath.Join(r.StateDir, sid+".json"))
	for _, leak := range []string{"reply text", "SECRETPROMPT"} {
		if strings.Contains(string(b), leak) || strings.Contains(string(sb), leak) {
			t.Fatalf("leak %q", leak)
		}
	}
	// A partial long line is held back until its newline arrives.
	appendTo(t, p, `{"type":"assistant","timestamp":"`+ts(7)+`","message":{"id":"m4","usage":{"output_tokens":100}},"pad":"`+big)
	if _, err = r.Read(sid); err != nil {
		t.Fatal(err)
	}
	appendTo(t, p, "\"}\n")
	st, err = r.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tokens.Output != 123 {
		t.Fatalf("after split big line: %d", st.Tokens.Output)
	}
}
