package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

type fakeLogin struct {
	login    api.Login
	sessions []api.WebSession
	err      error
	names    []string // CreateLogin calls
	revoked  []string
	listed   int
}

func (f *fakeLogin) CreateLogin(_ context.Context, name string) (api.Login, error) {
	f.names = append(f.names, name)
	return f.login, f.err
}
func (f *fakeLogin) ListWebSessions(context.Context) ([]api.WebSession, error) {
	f.listed++
	return f.sessions, f.err
}
func (f *fakeLogin) RevokeWebSession(_ context.Context, id string) error {
	f.revoked = append(f.revoked, id)
	return f.err
}

const testLoginURL = "https://sessionhub.example.com/login/abcdefghijklmnopqrstuvwxyz"

func runLogin(t *testing.T, f *fakeLogin, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := &env{login: f, out: &out, now: func() time.Time { return now }}
	err := e.loginCmd(context.Background(), args)
	return out.String(), err
}

func sessionsFixture() []api.WebSession {
	return []api.WebSession{
		{ID: "0123456789abcdef", Name: "phone", Machine: "bluebox", CreatedAt: now.Add(-48 * time.Hour), LastUsedAt: now.Add(-3 * time.Hour), ExpiresAt: now.Add(27 * 24 * time.Hour)},
		{ID: "fedcba9876543210", Name: "windows", Machine: "tower", CreatedAt: now.Add(-24 * time.Hour), LastUsedAt: now.Add(-time.Minute), ExpiresAt: now.Add(30 * 24 * time.Hour)},
	}
}

func TestLoginRequiresName(t *testing.T) {
	f := &fakeLogin{}
	for _, args := range [][]string{nil, {"--no-qr"}, {"--name", ""}, {"phone"}} {
		if _, err := runLogin(t, f, args...); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("args %q: err %v, want a usage error", args, err)
		}
	}
	if len(f.names) != 0 {
		t.Errorf("sent %d requests without a name", len(f.names))
	}
}

func TestLoginPrintsLinkAndQR(t *testing.T) {
	f := &fakeLogin{login: api.Login{URL: testLoginURL, Name: "phone", ExpiresAt: now.Add(10 * time.Minute)}}
	out, err := runLogin(t, f, "--name", "phone")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.names) != 1 || f.names[0] != "phone" {
		t.Errorf("requested %v", f.names)
	}
	link := strings.Index(out, testLoginURL)
	qrAt := strings.Index(out, "█")
	if link < 0 || qrAt < link || !strings.Contains(out, "expires at "+now.Add(10*time.Minute).Local().Format("15:04:05")) ||
		!strings.Contains(out, "sign it in as phone") {
		t.Errorf("output:\n%s", out)
	}
	var qrBuf bytes.Buffer
	renderQR(&qrBuf, testLoginURL)
	if !strings.HasSuffix(out, qrBuf.String()) {
		t.Error("output does not end with the QR code of the link")
	}

	out, err = runLogin(t, f, "--name", "phone", "--no-qr")
	if err != nil || strings.Contains(out, "█") || !strings.Contains(out, testLoginURL) {
		t.Errorf("--no-qr: %v\n%s", err, out)
	}
}

func TestLoginJSON(t *testing.T) {
	exp := time.Date(2026, 10, 1, 12, 10, 0, 0, time.UTC)
	f := &fakeLogin{login: api.Login{URL: testLoginURL, Name: "phone", ExpiresAt: exp}}
	out, err := runLogin(t, f, "--json", "--name", "phone")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"url":"` + testLoginURL + `","name":"phone","expires_at":"2026-10-01T12:10:00Z"}` + "\n"
	if out != want {
		t.Errorf("--json output %q, want %q", out, want)
	}
}

func TestLoginNameTaken(t *testing.T) {
	f := &fakeLogin{err: &client.StatusError{Status: 409, Message: `a browser session named "phone" exists: name taken`}}
	_, err := runLogin(t, f, "--name", "phone")
	want := "login: a browser session named phone exists; revoke it with `sessionhub login rm phone` or pick another name"
	if err == nil || err.Error() != want {
		t.Errorf("err %v, want %q", err, want)
	}
}

func TestLoginUnreachable(t *testing.T) {
	f := &fakeLogin{err: errors.New("dial tcp 127.0.0.1:8787: connect: connection refused")}
	if _, err := runLogin(t, f, "--name", "phone"); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err %v, want the connection error", err)
	}
}

func TestLoginLs(t *testing.T) {
	f := &fakeLogin{sessions: sessionsFixture()}
	out, err := runLogin(t, f, "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "ID", "MACHINE", "CREATED", "LAST_USED", "EXPIRES",
		"phone", "0123456789abcdef", "bluebox", "3h", "windows", "tower", "1m",
		now.Add(27 * 24 * time.Hour).Local().Format("2006-01-02 15:04")} {
		if !strings.Contains(out, want) {
			t.Errorf("ls lacks %q:\n%s", want, out)
		}
	}
	out, err = runLogin(t, f, "ls", "--json")
	var l api.WebSessionList
	if err != nil || json.Unmarshal([]byte(out), &l) != nil || len(l.Sessions) != 2 || l.Sessions[1].Name != "windows" {
		t.Errorf("ls --json: %v %s", err, out)
	}
	out, err = runLogin(t, &fakeLogin{sessions: []api.WebSession{}}, "ls")
	if err != nil || !strings.Contains(out, "no browser sessions") {
		t.Errorf("empty ls: %v %q", err, out)
	}
}

func TestLoginRm(t *testing.T) {
	tests := []struct {
		name    string
		arg     string
		revoked []string
		err     string
	}{
		{"by name", "phone", []string{"0123456789abcdef"}, ""},
		{"by id", "fedcba9876543210", []string{"fedcba9876543210"}, ""},
		{"no match", "tablet", nil, `no browser session named or with ID "tablet"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLogin{sessions: sessionsFixture()}
			out, err := runLogin(t, f, "rm", tt.arg)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Errorf("err %v, want %q", err, tt.err)
				}
			} else if err != nil || !strings.Contains(out, "signed out") {
				t.Errorf("rm %s: %v %q", tt.arg, err, out)
			}
			if strings.Join(f.revoked, ",") != strings.Join(tt.revoked, ",") {
				t.Errorf("revoked %v, want %v", f.revoked, tt.revoked)
			}
		})
	}
	// A name wins over an ID with the same text.
	f := &fakeLogin{sessions: []api.WebSession{
		{ID: "aaaaaaaaaaaaaaaa", Name: "bbbbbbbbbbbbbbbb"},
		{ID: "bbbbbbbbbbbbbbbb", Name: "other"},
	}}
	if _, err := runLogin(t, f, "rm", "bbbbbbbbbbbbbbbb"); err != nil || len(f.revoked) != 1 || f.revoked[0] != "aaaaaaaaaaaaaaaa" {
		t.Errorf("name-over-ID: %v %v", err, f.revoked)
	}
	for _, args := range [][]string{{"rm"}, {"rm", "a", "b"}} {
		if _, err := runLogin(t, &fakeLogin{}, args...); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("args %q: %v, want a usage error", args, err)
		}
	}
}
