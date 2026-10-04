package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// sessionCookieFrom returns the session cookie a response sets, or nil.
func sessionCookieFrom(h http.Header) *http.Cookie {
	for _, c := range (&http.Response{Header: h}).Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

func TestSessionCookieReadsAndNamesTheBrowser(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1})
	code, body, h := e.doWith("GET", "/v1/sessions", "", e.web, nil)
	if code != 200 || !strings.Contains(string(body), sid1) {
		t.Fatalf("GET /v1/sessions with cookie: %d %s", code, body)
	}
	if got := h.Get(api.HeaderSessionName); got != "phone" {
		t.Errorf("%s = %q, want phone", api.HeaderSessionName, got)
	}
	if c := sessionCookieFrom(h); c != nil {
		t.Errorf("a fresh session re-sent its cookie: %+v", c)
	}
	if _, _, h := e.do("GET", "/v1/sessions", e.tokA, nil); h.Get(api.HeaderSessionName) != "" {
		t.Errorf("machine token got %s %q", api.HeaderSessionName, h.Get(api.HeaderSessionName))
	}
	before := e.snapshot()
	if code, _, _ := e.doWith("POST", "/v1/sessions", "", e.web, api.SessionUpsert{ID: sid2, Source: api.SourceHooks, Agent: "claude"}); code != http.StatusUnauthorized {
		t.Errorf("POST /v1/sessions with cookie: %d, want 401", code)
	}
	if e.snapshot() != before {
		t.Error("a refused cookie write changed state")
	}
	if strings.Contains(e.log.String(), e.web) {
		t.Error("session token in the request log")
	}
}

func TestSessionCookieSlidingExpiry(t *testing.T) {
	e := newEnv(t)
	e.clock.Advance(30 * time.Minute)
	if code, _, h := e.doWith("GET", "/v1/sessions", "", e.web, nil); code != 200 || sessionCookieFrom(h) != nil {
		t.Fatalf("at +30m: %d, cookie %v; want 200 and no cookie", code, sessionCookieFrom(h))
	}
	e.clock.Advance(31 * time.Minute)
	code, _, h := e.doWith("GET", "/v1/sessions", "", e.web, nil)
	c := sessionCookieFrom(h)
	if code != 200 || c == nil {
		t.Fatalf("at +61m: %d, cookie %v; want 200 and a re-sent cookie", code, c)
	}
	if c.Value != e.web || c.MaxAge != 30*24*3600 || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Errorf("re-sent cookie attributes: %+v", c)
	}
	// 30 days after the slide, the session has expired.
	e.clock.Advance(30 * 24 * time.Hour)
	if code, _, _ := e.doWith("GET", "/v1/sessions", "", e.web, nil); code != http.StatusUnauthorized {
		t.Errorf("30 days after the last slide: %d, want 401", code)
	}
}

func TestSessionCookieRevokedOrMachineRemoved(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.st.RevokeWebSession(ctx, e.webID); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := e.doWith("GET", "/v1/sessions", "", e.web, nil); code != http.StatusUnauthorized {
		t.Errorf("revoked session: %d, want 401", code)
	}
	tok, _ := e.webSession("tablet")
	if err := e.st.RemoveMachine(ctx, "tower"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := e.doWith("GET", "/v1/sessions", "", tok, nil); code != http.StatusUnauthorized {
		t.Errorf("session of a removed machine: %d, want 401", code)
	}
}

func TestOldReadCookieIsSignedOut(t *testing.T) {
	e := newEnv(t)
	old := map[string]string{"Cookie": "hub_read=hub_r_test-read-token-0123456789abcdefghijklmnop"}
	code, body, h := e.doHdr("GET", "/", "", "", old, nil)
	if code != http.StatusUnauthorized || !strings.HasPrefix(h.Get("Content-Type"), "text/html") || !strings.Contains(string(body), "sessionhub login") {
		t.Errorf("GET / with the old hub_read cookie: %d %q %s", code, h.Get("Content-Type"), body)
	}
	if code, _, _ := e.doHdr("GET", "/v1/sessions", "", "", old, nil); code != http.StatusUnauthorized {
		t.Errorf("GET /v1/sessions with the old hub_read cookie: %d, want 401", code)
	}
	if code, _, _ := e.do("GET", "/v1/sessions", "hub_r_test-read-token-0123456789abcdefghijklmnop", nil); code != http.StatusUnauthorized {
		t.Errorf("bearer read token: %d, want 401", code)
	}
}

func TestDashboardIgnoresTokenParameter(t *testing.T) {
	e := newEnv(t)
	code, body, h := e.do("GET", "/?token=hub_r_test-read-token-0123456789abcdefghijklmnop", "", nil)
	if code != http.StatusUnauthorized || !strings.Contains(string(body), "sessionhub login") || len((&http.Response{Header: h}).Cookies()) != 0 {
		t.Errorf("?token= without a cookie: %d, cookies %v; want the signed-out page and no cookie", code, (&http.Response{Header: h}).Cookies())
	}
	if code, _, _ := e.doWith("GET", "/?token=junk", "", e.web, nil); code != 200 {
		t.Errorf("?token=junk with a valid cookie: %d, want 200", code)
	}
}
