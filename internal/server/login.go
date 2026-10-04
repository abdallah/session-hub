package server

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"net"
	"net/http"
	"strconv"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

// loginStyle is the login pages' one inline style. The CSP allows it by hash.
const loginStyle = `body{font:16px/1.5 system-ui,sans-serif;margin:0 auto;max-width:32rem;padding:24px 16px}button{font:inherit;min-height:44px;padding:0 20px}code{overflow-wrap:anywhere}`

func buildLoginCSP(style []byte) string {
	sum := sha256.Sum256(style)
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
		"'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
}

// loginCSP is the login pages' Content-Security-Policy.
var loginCSP = buildLoginCSP([]byte(loginStyle))

// loginPage is what the login template shows: the confirm form when Name is
// set, else Message and an optional command to run.
type loginPage struct {
	Name    string // the session name to confirm
	Message string
	Command string // shown in <code> after "Run"
	Tail    string // the rest of the sentence after the command
}

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>sessionhub: sign in</title>
<style>` + loginStyle + `</style></head>
<body>
<h1>sessionhub</h1>
{{if .Name}}<p>Sign in this browser as <strong>{{.Name}}</strong>?</p>
<form method="post"><button type="submit">Sign in</button></form>
{{else}}<p>{{.Message}}</p>
{{if .Command}}<p>Run <code>{{.Command}}</code>{{.Tail}}</p>
{{end}}{{end}}</body></html>
`))

var (
	goneLogin = loginPage{Message: "This link expired or was already used.", Command: "sessionhub login", Tail: " again."}
	// A refused Origin: the POST did not come from the page this server
	// served, for example a form on another site.
	forbiddenLogin = loginPage{Message: "sessionhub refused this sign-in because it did not come from the sessionhub's own page. Open the link again and press Sign in."}
)

func takenLogin(name string) loginPage {
	return loginPage{
		Message: "A browser session named " + name + " exists.",
		Command: "sessionhub login rm " + name,
		Tail:    " to sign that browser out, then press Sign in again. This link works until it expires.",
	}
}

// writeLoginPage sends a login page with the dashboard's security headers
// and its own CSP. Referrer-Policy is same-origin, not no-referrer: with
// no-referrer, browsers send Origin: null on the form POST, which the
// Origin check would refuse. same-origin still keeps the code out of any
// cross-site Referer.
func (s *Server) writeLoginPage(w http.ResponseWriter, status int, pg loginPage) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", loginCSP)
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := loginTmpl.Execute(w, pg); err != nil {
		s.log.Printf("login page: %v", err)
	}
}

// createLogin is POST /v1/logins: a one-time sign-in link for a browser.
func (s *Server) createLogin(w http.ResponseWriter, r *http.Request, p principal) {
	var in api.LoginIn
	if !decode(w, r, &in) {
		return
	}
	code, exp, err := s.store.CreateLoginCode(r.Context(), p.machine.ID, in.Name)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.Login{URL: s.publicURL + "/login/" + code, Name: in.Name, ExpiresAt: exp})
}

// loginPage is GET /login/{code}: the confirm page. It changes nothing, so
// a link previewer that fetches the link does not use it.
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !store.ValidLoginCode(code) {
		s.writeLoginPage(w, http.StatusGone, goneLogin)
		return
	}
	name, err := s.store.LoginCodeName(r.Context(), code)
	if errors.Is(err, store.ErrCodeGone) {
		s.writeLoginPage(w, http.StatusGone, goneLogin)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeLoginPage(w, http.StatusOK, loginPage{Name: name})
}

// loginSubmit is POST /login/{code}, the Sign in button. It uses the code
// once, creates the session, sets the cookie, and redirects to the
// dashboard.
func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if origin := r.Header.Get("Origin"); origin == "" || origin != s.publicURL {
		s.loginRefused(r, "Origin "+strconv.Quote(origin))
		s.writeLoginPage(w, http.StatusForbidden, forbiddenLogin)
		return
	}
	if !store.ValidLoginCode(code) {
		s.loginRefused(r, "malformed code")
		s.writeLoginPage(w, http.StatusGone, goneLogin)
		return
	}
	token, ws, err := s.store.RedeemLoginCode(r.Context(), code)
	switch {
	case errors.Is(err, store.ErrCodeGone):
		s.loginRefused(r, "code expired, used, or unknown")
		s.writeLoginPage(w, http.StatusGone, goneLogin)
		return
	case errors.Is(err, store.ErrNameTaken):
		s.loginRefused(r, "name "+strconv.Quote(ws.Name)+" taken")
		s.writeLoginPage(w, http.StatusConflict, takenLogin(ws.Name))
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	setWho(r, "web:"+ws.Name)
	setSessionCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// loginRefused logs a failed POST /login/{code} with the client address.
func (s *Server) loginRefused(r *http.Request, reason string) {
	s.log.Printf("login refused from %q: %s", clientIP(r), reason)
}

// clientIP is Cf-Connecting-Ip (set by the Cloudflare tunnel), else the
// remote address without its port.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("Cf-Connecting-Ip"); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// listWebSessions is GET /v1/web-sessions. It never returns a token or hash.
func (s *Server) listWebSessions(w http.ResponseWriter, r *http.Request, _ principal) {
	list, err := s.store.ListWebSessions(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.WebSessionList{Sessions: list})
}

// revokeWebSession is DELETE /v1/web-sessions/{id}.
func (s *Server) revokeWebSession(w http.ResponseWriter, r *http.Request, _ principal) {
	if err := s.store.RevokeWebSession(r.Context(), r.PathValue("id")); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// logout is POST /logout: it ends the calling browser's session only.
func (s *Server) logout(w http.ResponseWriter, r *http.Request, p principal) {
	if err := s.store.RevokeWebSession(r.Context(), p.web.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, err)
		return
	}
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}
