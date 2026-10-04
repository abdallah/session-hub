package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// waitUp polls addr's /healthz until it answers, and fails if run returns
// first.
func waitUp(t *testing.T, addr string, done <-chan error) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("run returned early: %v", err)
		default:
		}
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			resp.Body.Close()
			return
		}
	}
	t.Fatalf("%s never came up", addr)
}

// TestRunWarnsAboutReadToken: read_token and SESSIONHUB_READ_TOKEN are ignored, with
// one warning, and the server starts with or without them.
func TestRunWarnsAboutReadToken(t *testing.T) {
	tests := []struct {
		name     string
		toml     string
		env      string
		warnings int
	}{
		{"neither", "", "", 0},
		{"file", "read_token = \"hub_r_old\"\n", "", 1},
		{"env", "", "hub_r_env", 1},
		{"both", "read_token = \"hub_r_old\"\n", "hub_r_env", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configEnv(t, tt.toml)
			t.Setenv("SESSIONHUB_READ_TOKEN", tt.env)
			ln := listenLocal(t)
			ctx, cancel := context.WithCancel(context.Background())
			var logs syncBuffer
			done := make(chan error, 1)
			go func() { done <- runWithListeners(ctx, nil, &logs, []net.Listener{ln}) }()
			waitUp(t, ln.Addr().String(), done)
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("run: %v", err)
			}
			if n := strings.Count(logs.String(), "warning: read_token"); n != tt.warnings {
				t.Errorf("%d read_token warnings, want %d:\n%s", n, tt.warnings, logs.String())
			}
		})
	}
}

func TestRunRejectsArgsAndBadListen(t *testing.T) {
	configEnv(t, "")
	if err := run(context.Background(), []string{"--port", "1"}, io.Discard); err == nil {
		t.Error("extra arguments accepted")
	}
	t.Setenv("SESSIONHUB_LISTEN", "127.0.0.1:not-a-port")
	if err := run(context.Background(), nil, io.Discard); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("bad listen address: %v", err)
	}
}

// listenLocal binds a loopback port and keeps it bound, so no other process
// can take the port before run serves on it.
func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func TestRunServesAllListenersAndStops(t *testing.T) {
	configEnv(t, "")
	l1, l2 := listenLocal(t), listenLocal(t)
	a1, a2 := l1.Addr().String(), l2.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	var logs syncBuffer
	done := make(chan error, 1)
	go func() { done <- runWithListeners(ctx, nil, &logs, []net.Listener{l1, l2}) }()

	for _, addr := range []string{a1, a2} {
		waitUp(t, addr, done)
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(b) != "ok\n" {
			t.Errorf("%s /healthz: %d %q", addr, resp.StatusCode, b)
		}
	}
	resp, err := http.Get("http://" + a1 + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/v1/sessions without credentials: %d, want 401", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v after shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if _, err := http.Get("http://" + a1 + "/healthz"); err == nil {
		t.Error("server still answering after shutdown")
	}
	out := logs.String()
	for _, want := range []string{"listening on " + a1, "listening on " + a2, `GET "/v1/sessions" 401`, "machine=-", "shutting down"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "warning:") {
		t.Errorf("warning without read_token:\n%s", out)
	}
	if _, err := os.Stat(os.Getenv("SESSIONHUB_DB")); err != nil {
		t.Errorf("database not created: %v", err)
	}
}

// TestRunTelegramAlerts: the server logs once whether alerts are on, never
// logs the token, and stops the notifier with the server.
func TestRunTelegramAlerts(t *testing.T) {
	cases := []struct{ name, toml, token, chat, want string }{
		{"off with a bad chat id type", "telegram_chat_id = [42]\n", "", "", "telegram alerts are off: telegram_chat_id must be a string or an integer"},
		{"off with a misspelled key", "telegram_chat_idd = \"1\"\n", "", "", "warning: "},
		{"off without settings", "", "", "", "telegram alerts are off"},
		{"off with the token only", "", testToken, "", "telegram alerts are off"},
		{"on", "", testToken, "42", "telegram alerts are on"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configEnv(t, tc.toml)
			// Never reach api.telegram.org: point the notifier at a dead server.
			dead := httptest.NewServer(http.NotFoundHandler())
			dead.Close()
			old := telegramAPI
			telegramAPI = dead.URL
			t.Cleanup(func() { telegramAPI = old })
			t.Setenv("SESSIONHUB_TELEGRAM_BOT_TOKEN", tc.token)
			t.Setenv("SESSIONHUB_TELEGRAM_CHAT_ID", tc.chat)
			ln := listenLocal(t)
			ctx, cancel := context.WithCancel(context.Background())
			var logs syncBuffer
			done := make(chan error, 1)
			go func() { done <- runWithListeners(ctx, nil, &logs, []net.Listener{ln}) }()
			waitUp(t, ln.Addr().String(), done)
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("run: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("run did not return: the notifier did not stop")
			}
			out := logs.String()
			if n := strings.Count(out, "telegram alerts are"); n != 1 || !strings.Contains(out, tc.want) {
				t.Errorf("want one %q line:\n%s", tc.want, out)
			}
			if tc.toml != "" && !strings.Contains(out, "telegram alerts are off") {
				t.Errorf("alerts must stay off:\n%s", out)
			}
			if strings.Contains(out, "TEST-PLACEHOLDER") {
				t.Error("the token is in the log")
			}
		})
	}
}
