package server

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"net/http"
	"regexp"
)

//go:embed dashboard/index.html
var dashboardHTML []byte

// dashboardCSP allows exactly the page's one inline script and one inline
// style, by hash, and nothing else. The hashes are computed from the
// embedded file, so editing the page cannot leave a stale policy behind.
var dashboardCSP = buildCSP(dashboardHTML)

var (
	scriptRe = regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	styleRe  = regexp.MustCompile(`(?s)<style>(.*?)</style>`)
)

func buildCSP(page []byte) string {
	hash := func(re *regexp.Regexp) string {
		m := re.FindSubmatch(page)
		if m == nil {
			panic("dashboard/index.html: missing inline " + re.String())
		}
		sum := sha256.Sum256(m[1])
		return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	}
	return "default-src 'none'; script-src " + hash(scriptRe) + "; style-src " + hash(styleRe) +
		"; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}

const signedOutPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>sessionhub: signed out</title></head>
<body>
<h1>sessionhub</h1>
<p>You are signed out. Run <code>sessionhub login --name &lt;device&gt;</code> on a machine with sessionhub, then open the link it prints.</p>
</body></html>
`

// dashboard serves GET / to a caller with read credentials, and the
// signed-out page (401) to anyone else. A ?token= parameter is ignored.
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")

	_, ok, err := s.authenticate(w, r, true)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !ok {
		s.signedOutPage(w)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", dashboardCSP)
	w.Write(dashboardHTML)
}

func (s *Server) signedOutPage(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(signedOutPage))
}
