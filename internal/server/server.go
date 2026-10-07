// Package server is the sessionhub HTTP API, its auth, and the `sessionhub server` and
// `sessionhub machine` commands.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

// MaxBodyBytes is the largest request body the API accepts.
const MaxBodyBytes = 64 << 10

// Server serves the sessionhub HTTP API.
type Server struct {
	store *store.Store
	// refURLs link the refs that notes name (SetRefURLs).
	refURLs store.RefURLs
	// publicURL is public_url without a trailing slash: the base of login
	// links and the only Origin POST /login/{code} accepts.
	publicURL string
	log       *log.Logger
	control   *controlHub
	// after is the long poll's timer. Tests replace it.
	after func(time.Duration) <-chan time.Time
	// permCheck is how often an open decision poll rereads its request, to
	// see an answer in the terminal or an expiry. Tests shorten it.
	permCheck time.Duration
	// ruleAdded is called with each stored rule; it must not block. Run sets
	// it to the Telegram notifier when alerts are on.
	ruleAdded func(api.Instruction)
	// moveDir holds sealed move bundles; "" turns the bundle routes off.
	moveDir string
}

// New returns a Server. publicURL is the server's public_url.
func New(st *store.Store, publicURL string, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Server{store: st, publicURL: strings.TrimRight(publicURL, "/"), log: logger, control: newControlHub(),
		after: time.After, permCheck: time.Second}
}

// Handler returns the API with request logging.
func (s *Server) Handler() http.Handler {
	return s.logRequests(s.mux())
}

// principal is who made a request: a machine token or a browser session.
type principal struct {
	machine store.Machine   // set for a machine token
	web     *api.WebSession // set for the hub_session cookie
}

// reqInfo carries what the logger prints but only handlers learn.
type reqInfo struct{ who string }

type ctxKey struct{}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer; the
// long poll uses it to extend its write deadline.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests logs one line per request: method, path, status, duration, and
// the caller (a machine name, "web:<name>" for a browser session, "-" for
// none). It logs the path only, never the query string, so no token in a URL
// reaches the log.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &reqInfo{who: "-"}
		sw := &statusWriter{ResponseWriter: w}
		limit := int64(MaxBodyBytes)
		if isBundleUpload(r) {
			limit = maxBundleBytes // the handler checks the caller first
		}
		r.Body = http.MaxBytesReader(sw, r.Body, limit)
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), ctxKey{}, info)))
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		// %q, because the decoded path can hold a newline (from "%0a") that
		// would otherwise forge a second log line.
		// A login path holds a live one-time code; log a placeholder.
		path := r.URL.Path
		if strings.HasPrefix(path, "/login/") {
			path = "/login/<code>"
		}
		s.log.Printf("%s %q %d %s machine=%s", r.Method, path, sw.status,
			time.Since(start).Round(time.Microsecond), info.who)
	})
}

func setWho(r *http.Request, who string) {
	if info, ok := r.Context().Value(ctxKey{}).(*reqInfo); ok {
		info.who = who
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// authenticate resolves the caller. A bearer token matches machine tokens
// only. When allowCookie is set and there is no bearer header, the
// hub_session cookie counts if it names a live browser session; when the
// store slides that session's expiry, the cookie goes out again with a fresh
// Max-Age. ok is false when nothing matches; err is set only for a database
// failure.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, allowCookie bool) (p principal, ok bool, err error) {
	tok := bearer(r)
	if tok == "" && allowCookie {
		c, cerr := r.Cookie(sessionCookie)
		if cerr != nil || c.Value == "" {
			return p, false, nil
		}
		ws, touched, err := s.store.WebSessionByToken(r.Context(), c.Value)
		if errors.Is(err, store.ErrNotFound) {
			return p, false, nil
		}
		if err != nil {
			return p, false, err
		}
		if touched {
			setSessionCookie(w, c.Value)
		}
		setWho(r, "web:"+ws.Name)
		return principal{web: &ws}, true, nil
	}
	if tok == "" {
		return p, false, nil
	}
	m, err := s.store.MachineByToken(r.Context(), tok)
	if errors.Is(err, store.ErrNotFound) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	setWho(r, m.Name)
	return principal{machine: m}, true, nil
}

type authedHandler func(w http.ResponseWriter, r *http.Request, p principal)

// reader accepts a machine token or the session cookie.
func (s *Server) reader(h authedHandler) http.Handler {
	return s.auth(false, h)
}

// writer accepts only a machine token.
func (s *Server) writer(h authedHandler) http.Handler {
	return s.auth(true, h)
}

// actor accepts a machine token, or the session cookie with the header
// X-Hub-Action: action. The custom header makes a cross-site request need a
// CORS preflight, which the server never grants. The cookie without the
// header, or with another value, gets 403.
func (s *Server) actor(action string, h authedHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok, err := s.authenticate(w, r, true)
		switch {
		case err != nil:
			s.internalError(w, err)
			return
		case !ok:
			w.Header().Set("WWW-Authenticate", `Bearer realm="sessionhub"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		case p.web != nil && r.Header.Get(api.HeaderAction) != action:
			writeError(w, http.StatusForbidden, "the dashboard must send "+api.HeaderAction+": "+action+" with this request")
			return
		}
		h(w, r, p)
	})
}

// browserAction accepts only the session cookie with X-Hub-Action: action.
// A machine token gets 403: it has no browser session to act on.
func (s *Server) browserAction(action string, h authedHandler) http.Handler {
	return s.actor(action, func(w http.ResponseWriter, r *http.Request, p principal) {
		if p.web == nil {
			writeError(w, http.StatusForbidden, "only a signed-in browser can sign out; revoke a browser session with `sessionhub login rm`")
			return
		}
		h(w, r, p)
	})
}

func (s *Server) auth(write bool, h authedHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok, err := s.authenticate(w, r, !write)
		switch {
		case err != nil:
			s.internalError(w, err)
			return
		case !ok:
			w.Header().Set("WWW-Authenticate", `Bearer realm="sessionhub"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		h(w, r, p)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, api.Error{Error: msg})
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.log.Printf("internal error: %v", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// ambiguousError is api.Error plus the IDs an ambiguous prefix matched.
type ambiguousError struct {
	Error      string   `json:"error"`
	Candidates []string `json:"candidates"`
}

// storeError maps a store error to an HTTP response.
func (s *Server) storeError(w http.ResponseWriter, err error) {
	var amb *store.AmbiguousError
	switch {
	case errors.As(err, &amb):
		writeJSON(w, http.StatusConflict, ambiguousError{Error: amb.Error(), Candidates: amb.Candidates})
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrCodeGone), errors.Is(err, store.ErrGone):
		writeError(w, http.StatusGone, err.Error())
	case errors.Is(err, store.ErrFull):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrWrongMachine), errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrNotControllable), errors.Is(err, store.ErrRequestClosed), errors.Is(err, store.ErrInputCut):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrTooMany):
		writeError(w, http.StatusTooManyRequests, err.Error())
	default:
		s.internalError(w, err)
	}
}

// decode reads the whole body (capped by logRequests) into v. It writes the
// error response and returns false on failure.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(r.Body)
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body is larger than 64 KiB")
		return false
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
