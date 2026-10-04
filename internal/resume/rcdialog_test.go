package resume

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A tool-output screen that quotes the dialog's strings mid-screen.
func quotedDialogScreen(withFooter bool, trailing int) string {
	var b strings.Builder
	b.WriteString("● Bash(grep -r 'Disconnect this session' testdata)\n")
	b.WriteString("  ⎿  Remote Control\n")
	b.WriteString("     This session is available at " + rcURL + ".\n")
	b.WriteString("     Disconnect this session\n")
	b.WriteString("     Show QR code              Scan with your phone to open this session\n")
	if withFooter {
		b.WriteString("     Enter to select · Esc to continue\n")
	}
	for i := 0; i < trailing; i++ {
		b.WriteString("  more output line\n")
	}
	b.WriteString("\n──────────\n❯ \n──────────\n  ⏵⏵ auto mode on\n")
	return b.String()
}

func runDialogCase(t *testing.T, rc *rcScript, poll time.Duration) (methods string, out string, err error) {
	t.Helper()
	fh := rc.herdr(t)
	e, o := newEnv(t, hubServer(t, sess("bluebox", "w1:p1")), "bluebox", fh.path, nil)
	e.pollEvery, e.pollFor = 5*time.Millisecond, poll
	err = e.remoteControlRun(context.Background(), uuid[:8])
	return strings.Join(fh.methods(), ","), o.String(), err
}

func hasKeys(methods string) bool { return strings.Contains(methods, "pane.send_keys") }

// Remote Control already on: Claude answers /remote-control with a dialog.
// sessionhub reports it, closes it with Escape, and doesn't wait out the timeout.
// The dialog is judged on the visible screen only, never on scrollback.
func TestRemoteControlAlreadyOnDialog(t *testing.T) {
	recentDialog := fixtureText(t, "pane-read-remote-control-dialog.ndjson")
	visDialog := fixtureText(t, "pane-read-visible-dialog.ndjson")
	before := fixtureText(t, "pane-read-before-remote-control.ndjson")

	t.Run("the real dialog on the visible screen: report, print the URL, close it", func(t *testing.T) {
		rc := newRCScript(t)
		rc.before, rc.after, rc.visible = before, recentDialog, visDialog
		start := time.Now()
		methods, out, err := runDialogCase(t, rc, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("waited %s; the dialog must end the wait", d)
		}
		if !strings.Contains(out, "Remote Control is already on") || !strings.Contains(out, rcURL+"\n") {
			t.Errorf("output:\n%s", out)
		}
		const want = "pane.get,pane.read,pane.read,agent.prompt,pane.read,pane.read,pane.get,pane.read,pane.send_keys"
		if methods != want {
			t.Errorf("methods = %s\nwant      %s", methods, want)
		}
	})
	t.Run("pane.send_keys params", func(t *testing.T) {
		rc := newRCScript(t)
		rc.before, rc.after, rc.visible = before, recentDialog, visDialog
		fh := rc.herdr(t)
		e, _ := newEnv(t, hubServer(t, sess("bluebox", "w1:p1")), "bluebox", fh.path, nil)
		fastPoll(e)
		if err := e.remoteControlRun(context.Background(), uuid[:8]); err != nil {
			t.Fatal(err)
		}
		if p := fh.params("pane.send_keys"); p["pane_id"] != "w1:p1" || len(p) != 2 || !reflect.DeepEqual(p["keys"], []any{"esc"}) {
			t.Errorf("pane.send_keys params = %v", p)
		}
	})
	t.Run("closing the dialog fails: still exit 0, tell the user to press Esc", func(t *testing.T) {
		rc := newRCScript(t)
		rc.before, rc.after, rc.visible, rc.keysErr = before, recentDialog, visDialog, "pane_not_found"
		_, out, err := runDialogCase(t, rc, 150*time.Millisecond)
		if err != nil || !strings.Contains(out, rcURL) || !strings.Contains(out, "Press Esc in the pane") {
			t.Errorf("err=%v output:\n%s", err, out)
		}
	})
	t.Run("the dialog is on screen before the prompt: nothing is sent", func(t *testing.T) {
		rc := newRCScript(t)
		rc.visibleBefore, rc.visible = visDialog, visDialog
		methods, _, err := runDialogCase(t, rc, 150*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "dialog is open") {
			t.Errorf("err = %v", err)
		}
		if methods != "pane.get,pane.read,pane.read" {
			t.Errorf("methods = %s", methods)
		}
	})
	t.Run("the dialog is gone by the time sessionhub would press Esc: no keys", func(t *testing.T) {
		rc := newRCScript(t)
		rc.before, rc.after, rc.visible = before, recentDialog, visDialog
		rc.visibleLater = fixtureText(t, "pane-read-visible-prompt.ndjson")
		methods, _, err := runDialogCase(t, rc, 150*time.Millisecond)
		if err != nil || hasKeys(methods) {
			t.Errorf("err=%v methods=%s", err, methods)
		}
	})
	t.Run("Claude is working: report the URL but don't press Esc", func(t *testing.T) {
		rc := newRCScript(t)
		rc.before, rc.after, rc.visible, rc.status = before, recentDialog, visDialog, "working"
		// The status guard only applies to the re-read: the first pane.get must pass.
		rc.statusAfterPrompt = true
		methods, out, err := runDialogCase(t, rc, 150*time.Millisecond)
		if err != nil || hasKeys(methods) || !strings.Contains(out, "Press Esc in the pane") || !strings.Contains(out, rcURL) {
			t.Errorf("err=%v methods=%s\n%s", err, methods, out)
		}
	})

	// The next three are the misfire cases: dialog text that isn't the dialog.
	t.Run("dialog strings and a URL in scrollback, a normal prompt visible: no refusal, no Esc, normal URL path", func(t *testing.T) {
		rc := newRCScript(t)
		const fresh = "https://claude.ai/code/session_NewOne123"
		rc.before = recentDialog // scrollback holds the whole dialog text from earlier
		rc.after = recentDialog + "\n/remote-control is active · " + fresh
		// visible stays a normal prompt, before and after
		methods, out, err := runDialogCase(t, rc, 150*time.Millisecond)
		if err != nil {
			t.Fatalf("false refusal: %v", err)
		}
		if hasKeys(methods) || !strings.Contains(out, fresh) || strings.Contains(out, "already on") {
			t.Errorf("methods=%s\n%s", methods, out)
		}
	})
	t.Run("dialog strings quoted mid-screen without the footer: no Esc", func(t *testing.T) {
		for _, screen := range []string{
			quotedDialogScreen(false, 0),
			quotedDialogScreen(false, 30),
		} {
			rc := newRCScript(t)
			rc.visibleBefore, rc.visible = screen, screen
			rc.before, rc.after = screen, screen
			methods, out, err := runDialogCase(t, rc, 150*time.Millisecond)
			if err != nil || hasKeys(methods) || strings.Contains(out, "already on") {
				t.Errorf("err=%v methods=%s\n%s", err, methods, out)
			}
			if !strings.Contains(methods, "agent.prompt") {
				t.Errorf("a quoted dialog must not block the command: %s", methods)
			}
		}
	})
	t.Run("the whole dialog quoted with its footer, but not at the bottom: no Esc", func(t *testing.T) {
		screen := quotedDialogScreen(true, 30)
		rc := newRCScript(t)
		rc.visibleBefore, rc.visible = screen, screen
		rc.before, rc.after = screen, screen
		methods, _, err := runDialogCase(t, rc, 150*time.Millisecond)
		if err != nil || hasKeys(methods) || !strings.Contains(methods, "agent.prompt") {
			t.Errorf("err=%v methods=%s", err, methods)
		}
	})
}
