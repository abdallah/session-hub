package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

func TestCreateLogin(t *testing.T) {
	e := newEnv(t)
	var l api.Login
	e.must(http.StatusCreated, "POST", "/v1/logins", e.tokA, api.LoginIn{Name: "tablet"}, &l)
	if !regexp.MustCompile(`^`+regexp.QuoteMeta(testPublicURL)+`/login/[a-z2-7]{26}$`).MatchString(l.URL) ||
		l.Name != "tablet" || !l.ExpiresAt.Equal(e.clock.Now().Add(10*time.Minute)) {
		t.Errorf("login %+v", l)
	}
	for _, tc := range []struct {
		name string
		want int
		msg  string
	}{
		{"bad name", http.StatusBadRequest, "name"},
		{"phone", http.StatusConflict, `browser session named "phone"`}, // newEnv signed in "phone"
	} {
		code, b, _ := e.do("POST", "/v1/logins", e.tokA, api.LoginIn{Name: tc.name})
		var ae api.Error
		if code != tc.want || json.Unmarshal(b, &ae) != nil || !strings.Contains(ae.Error, tc.msg) {
			t.Errorf("name %q: %d %s, want %d with %q", tc.name, code, b, tc.want, tc.msg)
		}
	}
	for range 4 {
		e.must(http.StatusCreated, "POST", "/v1/logins", e.tokB, api.LoginIn{Name: "tablet"}, nil)
	}
	if code, b, _ := e.do("POST", "/v1/logins", e.tokA, api.LoginIn{Name: "tablet"}); code != http.StatusTooManyRequests {
		t.Errorf("sixth unused link: %d %s, want 429", code, b)
	}
}

func TestLoginPageChangesNothing(t *testing.T) {
	e := newEnv(t)
	code := e.loginCode("tablet")
	for i := range 2 {
		status, body, h := e.do("GET", "/login/"+code, "", nil)
		page := string(body)
		if status != 200 || !strings.Contains(page, "Sign in this browser as <strong>tablet</strong>?") ||
			!strings.Contains(page, `<form method="post">`) || !strings.Contains(page, "Sign in</button>") {
			t.Fatalf("GET %d: %d %s", i, status, page)
		}
		if got := h.Get("Content-Security-Policy"); got != loginCSP {
			t.Errorf("CSP %q, want %q", got, loginCSP)
		}
		for _, want := range []string{"default-src 'none'", "style-src 'sha256-", "form-action 'self'", "base-uri 'none'", "frame-ancestors 'none'"} {
			if !strings.Contains(loginCSP, want) {
				t.Errorf("CSP lacks %q", want)
			}
		}
		// no-referrer would make browsers send Origin: null on the form POST.
		if got := h.Get("Referrer-Policy"); got != "same-origin" {
			t.Errorf("Referrer-Policy %q, want same-origin", got)
		}
		if h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("headers %v", h)
		}
		if strings.Contains(page, "<script") {
			t.Error("login page has a script")
		}
	}
	// The style hash in the CSP matches the page's one inline style.
	_, body, _ := e.do("GET", "/login/"+code, "", nil)
	if got := buildLoginCSP(regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindSubmatch(body)[1]); got != loginCSP {
		t.Errorf("CSP from the served style %q, want %q", got, loginCSP)
	}
	if list, _ := e.st.ListWebSessions(context.Background()); len(list) != 1 {
		t.Errorf("GET created a session: %d sessions", len(list))
	}
	if status, _, _ := e.submitLogin(code, testPublicURL); status != http.StatusSeeOther {
		t.Errorf("POST after two GETs: %d, want 303", status)
	}
	for _, bad := range []string{code, "short", strings.ToUpper(code), "aaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		status, body, _ := e.do("GET", "/login/"+bad, "", nil)
		if status != http.StatusGone || !strings.Contains(string(body), "This link expired or was already used.") ||
			!strings.Contains(string(body), "<code>sessionhub login</code>") {
			t.Errorf("GET /login/%s: %d %s, want the 410 page", bad, status, body)
		}
	}
}

func TestLoginSubmit(t *testing.T) {
	e := newEnv(t)
	code := e.loginCode("tablet")
	status, body, h := e.submitLogin(code, testPublicURL)
	if status != http.StatusSeeOther || h.Get("Location") != "/" {
		t.Fatalf("POST: %d, Location %q; want 303 to /", status, h.Get("Location"))
	}
	c := sessionCookieFrom(h)
	if c == nil || !strings.HasPrefix(c.Value, store.SessionTokenPrefix) || !c.HttpOnly || !c.Secure ||
		c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != 30*24*3600 {
		t.Fatalf("cookie %+v", c)
	}
	if strings.Contains(string(body), c.Value) {
		t.Error("session token in the response body")
	}
	gs, _, gh := e.doWith("GET", "/v1/sessions", "", c.Value, nil)
	if gs != 200 || gh.Get(api.HeaderSessionName) != "tablet" {
		t.Errorf("the new cookie: %d, name %q", gs, gh.Get(api.HeaderSessionName))
	}
	status, body, h = e.submitLogin(code, testPublicURL)
	if status != http.StatusGone || !strings.Contains(string(body), "expired or was already used") || sessionCookieFrom(h) != nil {
		t.Errorf("second press: %d %s", status, body)
	}
	if !strings.Contains(e.log.String(), "login refused") {
		t.Errorf("refused press not logged:\n%s", e.log.String())
	}
}

func TestLoginSubmitOrigin(t *testing.T) {
	e := newEnv(t)
	code := e.loginCode("tablet")
	for _, origin := range []string{"", "https://evil.example", testPublicURL + "/", "http://sessionhub.example.test", "null", "https://sessionhub.example.test:8443"} {
		status, body, h := e.submitLogin(code, origin)
		if status != http.StatusForbidden || !strings.HasPrefix(h.Get("Content-Type"), "text/html") || sessionCookieFrom(h) != nil {
			t.Errorf("Origin %q: %d %s, want the 403 page", origin, status, body)
		}
	}
	// The refused presses did not use the code.
	if status, _, _ := e.submitLogin(code, testPublicURL); status != http.StatusSeeOther {
		t.Errorf("right Origin after refusals: %d, want 303", status)
	}
	// Refusals are logged with Cf-Connecting-Ip when present.
	e.doHdr("POST", "/login/"+code, "", "", map[string]string{"Origin": "https://evil.example", "Cf-Connecting-Ip": "203.0.113.9"}, nil)
	if !strings.Contains(e.log.String(), `login refused from "203.0.113.9"`) {
		t.Errorf("log lacks the Cf-Connecting-Ip address:\n%s", e.log.String())
	}
	if !strings.Contains(e.log.String(), `login refused from "127.0.0.1"`) {
		t.Errorf("log lacks the remote address:\n%s", e.log.String())
	}
}

// An empty public_url must not let a press without an Origin header through.
func TestLoginSubmitOriginEmptyPublicURL(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(New(e.st, "", log.New(io.Discard, "", 0)).Handler())
	t.Cleanup(srv.Close)
	code := e.loginCode("tablet")
	resp, err := http.Post(srv.URL+"/login/"+code, "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || sessionCookieFrom(resp.Header) != nil {
		t.Errorf("no Origin, empty public_url: %d, want 403 and no cookie", resp.StatusCode)
	}
	if status, _, _ := e.submitLogin(code, testPublicURL); status != http.StatusSeeOther {
		t.Errorf("code after the refusal: %d, want 303", status)
	}
}

func TestLoginSubmitRace(t *testing.T) {
	e := newEnv(t)
	code := e.loginCode("tablet")
	start := make(chan struct{})
	codes := make(chan int, 2)
	for range 2 {
		go func() {
			<-start
			req, _ := http.NewRequest("POST", e.srv.URL+"/login/"+code, nil)
			req.Header.Set("Origin", testPublicURL)
			resp, err := noRedirect().Do(req)
			if err != nil {
				codes <- 0
				return
			}
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	close(start)
	got := []int{<-codes, <-codes}
	slices.Sort(got)
	if !slices.Equal(got, []int{http.StatusSeeOther, http.StatusGone}) {
		t.Errorf("two presses: %v, want one 303 and one 410", got)
	}
	list, _ := e.st.ListWebSessions(context.Background())
	if n := len(list); n != 2 { // phone and tablet
		t.Errorf("%d sessions, want 2", n)
	}
}

func TestLoginSubmitNameTaken(t *testing.T) {
	e := newEnv(t)
	code := e.loginCode("tablet")
	_, id := e.webSession("tablet")
	status, body, h := e.submitLogin(code, testPublicURL)
	if status != http.StatusConflict || !strings.Contains(string(body), "A browser session named tablet exists.") ||
		!strings.Contains(string(body), "<code>sessionhub login rm tablet</code>") || sessionCookieFrom(h) != nil {
		t.Fatalf("taken name: %d %s", status, body)
	}
	if err := e.st.RevokeWebSession(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := e.submitLogin(code, testPublicURL); status != http.StatusSeeOther {
		t.Errorf("same link after the revoke: %d, want 303", status)
	}
}

func TestLoginExpired(t *testing.T) {
	e := newEnv(t)
	code := e.loginCode("tablet")
	e.clock.Advance(10 * time.Minute)
	if status, _, _ := e.do("GET", "/login/"+code, "", nil); status != http.StatusGone {
		t.Errorf("GET after 10m: %d, want 410", status)
	}
	if status, _, _ := e.submitLogin(code, testPublicURL); status != http.StatusGone {
		t.Errorf("POST after 10m: %d, want 410", status)
	}
}

func TestWebSessionsAPI(t *testing.T) {
	e := newEnv(t)
	code, body, _ := e.do("GET", "/v1/web-sessions", e.tokB, nil)
	var l api.WebSessionList
	if code != 200 || json.Unmarshal(body, &l) != nil || len(l.Sessions) != 1 ||
		l.Sessions[0].Name != "phone" || l.Sessions[0].Machine != "tower" || l.Sessions[0].ID != e.webID {
		t.Fatalf("list: %d %s", code, body)
	}
	if strings.Contains(string(body), store.SessionTokenPrefix) || strings.Contains(string(body), store.HashToken(e.web)) {
		t.Errorf("list leaks the token or its hash: %s", body)
	}
	if code, _, _ := e.do("DELETE", "/v1/web-sessions/0000000000000000", e.tokA, nil); code != http.StatusNotFound {
		t.Errorf("DELETE unknown: %d, want 404", code)
	}
	if code, _, _ := e.do("DELETE", "/v1/web-sessions/"+e.webID, e.tokB, nil); code != http.StatusNoContent {
		t.Errorf("DELETE: %d, want 204", code)
	}
	if code, _, _ := e.doWith("GET", "/v1/sessions", "", e.web, nil); code != http.StatusUnauthorized {
		t.Errorf("revoked cookie: %d, want 401", code)
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t)
	other, _ := e.webSession("tablet")
	signOut := map[string]string{api.HeaderAction: api.HeaderActionSignOut}
	code, _, h := e.doHdr("POST", "/logout", "", e.web, signOut, nil)
	c := sessionCookieFrom(h)
	if code != http.StatusNoContent || c == nil || c.MaxAge >= 0 || c.Value != "" {
		t.Fatalf("logout: %d, cookie %+v; want 204 and a cleared cookie", code, c)
	}
	if code, _, _ := e.doWith("GET", "/v1/sessions", "", e.web, nil); code != http.StatusUnauthorized {
		t.Errorf("after sign-out: %d, want 401", code)
	}
	if code, _, _ := e.doWith("GET", "/v1/sessions", "", other, nil); code != 200 {
		t.Errorf("the other browser after sign-out: %d, want 200", code)
	}
	if code, _, _ := e.doHdr("POST", "/logout", e.tokA, "", signOut, nil); code != http.StatusForbidden {
		t.Errorf("machine token on /logout: %d, want 403", code)
	}
}

func TestLoginCodeNotLogged(t *testing.T) {
	e := newEnv(t)
	code := e.loginCode("tablet")
	e.do("GET", "/login/"+code, "", nil)
	e.submitLogin(code, "https://evil.example")
	e.submitLogin(code, testPublicURL)
	out := e.log.String()
	if strings.Contains(out, code) {
		t.Errorf("login code in the request log:\n%s", out)
	}
	if !strings.Contains(out, `GET "/login/<code>" 200`) || !strings.Contains(out, `POST "/login/<code>" 303`) {
		t.Errorf("log lacks the redacted login lines:\n%s", out)
	}
}
