package api

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPermissionInputText(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
	}{
		{`{"command":"git push","description":"Push"}`, "git push"},
		{`{"file_path":"/tmp/a.go","content":"x"}`, "/tmp/a.go"},
		{`{"notebook_path":"/n.ipynb"}`, "/n.ipynb"},
		{`{"url":"https://example.test","prompt":"read"}`, "https://example.test"},
		{`{"pattern":"*.go"}`, "*.go"},
		{`{"command":"","path":"/srv"}`, "/srv"},
		{`{ "a": 1,  "b": [2] }`, `{"a":1,"b":[2]}`},
		{`"cut input…"`, "cut input…"},
		{``, ""},
		{`not json`, "not json"},
	} {
		if got := PermissionInputText(json.RawMessage(c.in)); got != c.want {
			t.Errorf("PermissionInputText(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMessagePrompt(t *testing.T) {
	got := MessagePrompt("cli on tower", "line one\nline two")
	want := "From the user via sessionhub (cli on tower):\n\nline one\nline two"
	if got != want {
		t.Errorf("MessagePrompt = %q, want %q", got, want)
	}
	if got := MessagePrompt(SenderDashboard, "hi"); got != "From the user via sessionhub (dashboard):\n\nhi" {
		t.Errorf("dashboard: %q", got)
	}
	if got, want := MessagePrompt("session 3f2a9c10", "hi"), "From session 3f2a9c10 via sessionhub (sent on the user's behalf):\n\nhi"; got != want {
		t.Errorf("session: %q, want %q", got, want)
	}
}

func TestCapToolInput(t *testing.T) {
	out, cut, err := CapToolInput(json.RawMessage(`{ "command" : "ls" }`))
	if err != nil || cut || string(out) != `{"command":"ls"}` {
		t.Errorf("small input: %s %v %v", out, cut, err)
	}
	for _, empty := range []string{``, `null`, `  `} {
		if out, cut, err := CapToolInput(json.RawMessage(empty)); err != nil || cut || string(out) != `{}` {
			t.Errorf("input %q: %s %v %v, want {}", empty, out, cut, err)
		}
	}
	if _, _, err := CapToolInput(json.RawMessage(`{"a":`)); err == nil {
		t.Error("broken JSON accepted")
	}
	big := `{"file_path":"/tmp/x","content":"` + strings.Repeat("é", 6000) + `"}`
	out, cut, err = CapToolInput(json.RawMessage(big))
	if err != nil || !cut {
		t.Fatalf("big input: cut=%v err=%v", cut, err)
	}
	var s string
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("a cut input is not a JSON string: %v", err)
	}
	if !strings.HasPrefix(s, `{"file_path":"/tmp/x","content":"éé`) || !strings.HasSuffix(s, "…") || !utf8.ValidString(s) {
		t.Errorf("cut input starts %.40q, ends %q", s, s[len(s)-6:])
	}
	if n := len(s) - len("…"); n > 8000 || n < 7997 {
		t.Errorf("kept %d bytes, want 7997 to 8000", n)
	}
}

func TestCapToolInputEscapeHeavy(t *testing.T) {
	for name, content := range map[string]string{
		"html":      strings.Repeat("<", 9000),
		"bash":      strings.Repeat(`a && b > "c" \\ d; `, 1000),
		"quotes":    strings.Repeat(`"`, 9000),
		"newlines":  strings.Repeat("\n", 9000),
		"multibyte": strings.Repeat("é\"", 5000),
	} {
		enc, err := encodeJSONString(content)
		if err != nil {
			t.Fatal(err)
		}
		raw := json.RawMessage(`{"command":` + string(enc) + `}`)
		out, cut, err := CapToolInput(raw)
		if err != nil || !cut {
			t.Fatalf("%s: cut=%v err=%v", name, cut, err)
		}
		if len(out) > MaxToolInputBytes {
			t.Errorf("%s: %d bytes, want at most %d", name, len(out), MaxToolInputBytes)
		}
		var str string
		if err := json.Unmarshal(out, &str); err != nil || !utf8.ValidString(str) {
			t.Errorf("%s: not a valid JSON string: %v", name, err)
		}
		if strings.Contains(string(out), `\u003c`) {
			t.Errorf("%s: HTML-escaped output", name)
		}
	}
}
