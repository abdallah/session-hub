package server

import (
	"net/http"
	"sort"
	"strings"

	"github.com/abdallah/session-hub/internal/api"
)

// Access levels a route requires.
const (
	accessPublic = "public"
	accessRead   = "read"   // machine token or session cookie
	accessPage   = "page"   // the dashboard: read credentials, else an HTML 401
	accessWrite  = "write"  // machine token only
	accessAction = "action" // machine token, or the session cookie with X-Hub-Action: remote-control

	accessMachine = "machine"  // machine token only, also for reads (sign-in management)
	accessSignOut = "sign-out" // the session cookie with X-Hub-Action: sign-out only
	accessTriage  = "triage"   // machine token, or the session cookie with X-Hub-Action: triage

	accessInstructions = "instructions" // machine token, or the session cookie with X-Hub-Action: instructions
	accessSend         = "send"         // machine token, or the session cookie with X-Hub-Action: send
	accessApprove      = "approve"      // machine token, or the session cookie with X-Hub-Action: approve
	accessMove         = "move"         // machine token, or the session cookie with X-Hub-Action: move
	accessStart        = "start"        // machine token, or the session cookie with X-Hub-Action: start
)

// route is one method and path pattern of the API.
type route struct {
	method  string
	path    string
	access  string
	handler http.Handler
}

// routes is the one place every route is registered. The auth test walks
// this table, so a new route without an auth test case fails the build.
func (s *Server) routes() []route {
	return []route{
		{"GET", "/healthz", accessPublic, http.HandlerFunc(s.healthz)},
		{"GET", "/favicon.ico", accessPublic, http.HandlerFunc(s.favicon)},
		{"GET", "/apple-touch-icon.png", accessPublic, http.HandlerFunc(s.touchIcon)},
		{"GET", "/{$}", accessPage, http.HandlerFunc(s.dashboard)},
		{"GET", "/login/{code}", accessPublic, http.HandlerFunc(s.loginPage)},
		{"POST", "/login/{code}", accessPublic, http.HandlerFunc(s.loginSubmit)},
		{"POST", "/logout", accessSignOut, s.browserAction(api.HeaderActionSignOut, s.logout)},

		{"POST", "/v1/sessions", accessWrite, s.writer(s.upsertSession)},
		{"PUT", "/v1/machines/self/herdr-sessions", accessWrite, s.writer(s.putHerdrSessions)},
		{"POST", "/v1/sessions/{id}/events", accessWrite, s.writer(s.postEvent)},
		{"POST", "/v1/sessions/{id}/report", accessWrite, s.writer(s.postReport)},
		{"POST", "/v1/sessions/{id}/title", accessWrite, s.writer(s.postTitle)},
		{"PUT", "/v1/sessions/{id}/digest", accessWrite, s.writer(s.putDigest)},
		{"POST", "/v1/sessions/{id}/remote-control", accessAction, s.actor(api.HeaderActionRemoteControl, s.postRemoteControl)},
		{"POST", "/v1/inbox/{id}/dismiss", accessTriage, s.actor(api.HeaderActionTriage, s.dismissInbox)},
		{"POST", "/v1/inbox/{id}/snooze", accessTriage, s.actor(api.HeaderActionTriage, s.snoozeInbox)},
		{"GET", "/v1/machines/self/control", accessWrite, s.writer(s.pollControl)},
		{"POST", "/v1/control/{id}/result", accessWrite, s.writer(s.postControlResult)},
		{"POST", "/v1/instructions", accessInstructions, s.actor(api.HeaderActionInstructions, s.addInstruction)},
		{"DELETE", "/v1/instructions/{id}", accessInstructions, s.actor(api.HeaderActionInstructions, s.deleteInstruction)},
		{"POST", "/v1/messages", accessSend, s.actor(api.HeaderActionSend, s.postMessages)},
		{"POST", "/v1/sessions/{id}/permissions", accessWrite, s.writer(s.postPermission)},
		{"GET", "/v1/permissions/{id}/decision", accessWrite, s.writer(s.waitDecision)},
		{"POST", "/v1/permissions/{id}/decide", accessApprove, s.actor(api.HeaderActionApprove, s.decidePermission)},
		{"POST", "/v1/sessions/{id}/move", accessMove, s.actor(api.HeaderActionMove, s.postMove)},
		{"PUT", "/v1/moves/{id}/bundle", accessWrite, s.writer(s.putMoveBundle)},
		{"GET", "/v1/moves/{id}/bundle", accessWrite, s.writer(s.getMoveBundle)},
		{"POST", "/v1/moves/{id}/result", accessWrite, s.writer(s.postMoveResult)},
		{"PUT", "/v1/machines/self/move-key", accessWrite, s.writer(s.putMoveKey)},
		{"POST", "/v1/machines/{name}/start", accessStart, s.actor(api.HeaderActionStart, s.postStart)},

		{"GET", "/v1/sessions", accessRead, s.reader(s.listSessions)},
		{"GET", "/v1/sessions/{id}", accessRead, s.reader(s.getSession)},
		{"GET", "/v1/machines", accessRead, s.reader(s.listMachines)},
		{"GET", "/v1/inbox", accessRead, s.reader(s.getInbox)},
		{"GET", "/v1/instructions", accessRead, s.reader(s.listInstructions)},
		{"GET", "/v1/messages/{id}", accessRead, s.reader(s.getMessage)},
		{"GET", "/v1/moves/{id}", accessRead, s.reader(s.getMove)},
		{"GET", "/v1/starts/{id}", accessRead, s.reader(s.getStart)},
		{"POST", "/v1/logins", accessMachine, s.writer(s.createLogin)},
		{"GET", "/v1/web-sessions", accessMachine, s.writer(s.listWebSessions)},
		{"DELETE", "/v1/web-sessions/{id}", accessMachine, s.writer(s.revokeWebSession)},
	}
}

// mux registers routes, a JSON 405 for a known path with the wrong method,
// and a JSON 404 for everything else.
func (s *Server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	allowed := map[string][]string{}
	var order []string
	for _, rt := range s.routes() {
		mux.Handle(rt.method+" "+rt.path, rt.handler)
		if _, ok := allowed[rt.path]; !ok {
			order = append(order, rt.path)
		}
		allowed[rt.path] = append(allowed[rt.path], rt.method)
		if rt.method == http.MethodGet {
			allowed[rt.path] = append(allowed[rt.path], http.MethodHead)
		}
	}
	for _, p := range order {
		methods := allowed[p]
		sort.Strings(methods)
		allow := strings.Join(methods, ", ")
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", allow)
			writeError(w, http.StatusMethodNotAllowed, "method "+r.Method+" not allowed; use "+allow)
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint: "+r.URL.Path)
	})
	return mux
}
