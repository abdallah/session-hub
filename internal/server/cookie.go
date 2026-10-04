package server

import (
	"net/http"
	"time"

	"github.com/abdallah/session-hub/internal/store"
)

// sessionCookie carries a browser session token. The __Host- prefix makes
// browsers require Secure, Path=/, and no Domain, which blocks cookie tossing
// from sibling subdomains. SameSite=Lax, not Strict,
// so opening the dashboard from a link in another app sends it. Writes stay
// protected by X-Hub-Action, which needs a CORS preflight the server never
// grants.
const sessionCookie = "__Host-hub_session"

// setSessionCookie sets the session cookie with a Max-Age of the session
// lifetime. It is the only place a session token leaves the server.
func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(store.WebSessionTTL / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie tells the browser to drop the session cookie.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}
