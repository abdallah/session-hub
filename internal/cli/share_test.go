package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

func TestShare(t *testing.T) {
	run := func(vars map[string]string, args ...string) (*fakeLogin, string, error) {
		f := &fakeLogin{login: api.Login{URL: testLoginURL, Session: "d7782060-full", ExpiresAt: now.Add(10 * time.Minute)}}
		var out bytes.Buffer
		e := &env{login: f, out: &out, now: func() time.Time { return now }, getenv: func(k string) string { return vars[k] }}
		err := e.shareCmd(context.Background(), args)
		return f, out.String(), err
	}
	f, out, err := run(map[string]string{"CLAUDE_CODE_SESSION_ID": "d7782060-full"}, "--no-qr")
	if err != nil || f.scopes[0] != "d7782060-full" || !strings.HasPrefix(f.names[0], "share-") {
		t.Fatalf("default session: err %v, calls %v %v", err, f.names, f.scopes)
	}
	if !strings.Contains(out, "sessionhub attach "+testLoginURL) {
		t.Errorf("output %q lacks the attach command", out)
	}
	if f, _, err := run(nil, "abc123", "--no-qr"); err != nil || f.scopes[0] != "abc123" {
		t.Errorf("explicit ID: err %v, scopes %v", err, f.scopes)
	}
	if _, _, err := run(nil); err == nil || !strings.Contains(err.Error(), "not inside a Claude session") {
		t.Errorf("no session: err %v", err)
	}
	if _, _, err := run(nil, "a", "b"); err == nil {
		t.Error("two IDs accepted")
	}
}
