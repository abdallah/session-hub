package resume

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenPickerArguments(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := openPicker(context.Background(), bin, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argsFile)
	got := strings.Join(strings.Fields(string(b)), " ")
	want := "plugin pane open --plugin sessionhub --entrypoint resume-picker --placement overlay --focus"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
	if err := openPicker(context.Background(), filepath.Join(dir, "missing"), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("missing herdr binary must fail")
	}
}

func TestOpenInboxArguments(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := openInbox(context.Background(), bin, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argsFile)
	got := strings.Join(strings.Fields(string(b)), " ")
	want := "plugin pane open --plugin sessionhub --entrypoint inbox --placement split --direction right --focus"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

// herdr answers `plugin pane open` with a JSON line; a successful open prints
// nothing, and a failed one shows what herdr said.
func TestOpenPaneQuietOnSuccess(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "herdr-ok")
	bad := filepath.Join(dir, "herdr-bad")
	if err := os.WriteFile(ok, []byte("#!/bin/sh\necho '{\"id\":\"cli:plugin\",\"result\":{}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho '{\"error\":\"no such plugin\"}'\necho oops >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := openInbox(context.Background(), ok, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Errorf("success printed %q / %q, want nothing", out.String(), errOut.String())
	}
	out.Reset()
	if err := openInbox(context.Background(), bad, &out, &errOut); err == nil {
		t.Fatal("want an error from a failing herdr")
	}
	if !strings.Contains(errOut.String(), "no such plugin") || !strings.Contains(errOut.String(), "oops") {
		t.Errorf("failure output = %q, want herdr's stdout and stderr", errOut.String())
	}
}

func TestOutsideHerdrNote(t *testing.T) {
	in := func(k string) string {
		if k == "HERDR_ENV" {
			return "1"
		}
		return ""
	}
	if got := outsideHerdrNote(in, "tower"); got != "" {
		t.Errorf("inside herdr: %q, want no note", got)
	}
	out := func(string) string { return "" }
	if got := outsideHerdrNote(out, "tower"); !strings.Contains(got, "herdr on tower") {
		t.Errorf("outside herdr: %q, want a note naming tower", got)
	}
}
