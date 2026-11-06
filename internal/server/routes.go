package server

import (
	"net/http"
	"slices"
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
	accessRename  = "rename"   // machine token, or the session cookie with X-Hub-Action: rename

	accessInstructions = "instructions"  // machine token, or the session cookie with X-Hub-Action: instructions
	accessSend         = "send"          // machine token, or the session cookie with X-Hub-Action: send
	accessApprove      = "approve"       // machine token, or the session cookie with X-Hub-Action: approve
	accessMove         = "move"          // machine token, or the session cookie with X-Hub-Action: move
	accessStart        = "start"         // machine token, or the session cookie with X-Hub-Action: start
	accessTasks        = "tasks"         // machine token, or the session cookie with X-Hub-Action: tasks
	accessKnowledge    = "knowledge"     // machine token, or the session cookie with X-Hub-Action: knowledge
	accessKnowledgeWeb = "knowledge-web" // the session cookie with X-Hub-Action: knowledge only
)

// route is one method and path pattern of the API. guest marks the routes a
// browser signed in for one session may call; its handler then limits the
// answer to that session. Every other route gives a guest 403.
type route struct {
	method  string
	path    string
	access  string
	handler http.Handler
	guest   bool
}

// routes is the one place every route is registered. The auth test walks
// this table, so a new route without an auth test case fails the build.
func (s *Server) routes() []route {
	return []route{
		{"GET", "/healthz", accessPublic, http.HandlerFunc(s.healthz), false},
		{"GET", "/favicon.ico", accessPublic, http.HandlerFunc(s.favicon), false},
		{"GET", "/apple-touch-icon.png", accessPublic, http.HandlerFunc(s.touchIcon), false},
		{"GET", "/{$}", accessPage, http.HandlerFunc(s.dashboard), true},
		{"GET", "/login/{code}", accessPublic, http.HandlerFunc(s.loginPage), false},
		{"POST", "/login/{code}", accessPublic, http.HandlerFunc(s.loginSubmit), false},
		{"POST", "/logout", accessSignOut, s.browserAction(api.HeaderActionSignOut, s.logout), true},

		{"POST", "/v1/sessions", accessWrite, s.writer(s.upsertSession), false},
		{"PUT", "/v1/machines/self/herdr-sessions", accessWrite, s.writer(s.putHerdrSessions), false},
		{"POST", "/v1/sessions/{id}/events", accessWrite, s.writer(s.postEvent), false},
		{"POST", "/v1/sessions/{id}/report", accessWrite, s.writer(s.postReport), false},
		{"POST", "/v1/sessions/{id}/title", accessWrite, s.writer(s.postTitle), false},
		{"POST", "/v1/sessions/{id}/rename", accessRename, s.actor(api.HeaderActionRename, s.postRename), false},
		{"PUT", "/v1/sessions/{id}/digest", accessWrite, s.writer(s.putDigest), false},
		{"POST", "/v1/sessions/{id}/remote-control", accessAction, s.actor(api.HeaderActionRemoteControl, s.postRemoteControl), false},
		{"POST", "/v1/inbox/{id}/dismiss", accessTriage, s.actor(api.HeaderActionTriage, s.dismissInbox), false},
		{"POST", "/v1/inbox/{id}/snooze", accessTriage, s.actor(api.HeaderActionTriage, s.snoozeInbox), false},
		{"GET", "/v1/machines/self/control", accessWrite, s.writer(s.pollControl), false},
		{"POST", "/v1/control/{id}/result", accessWrite, s.writer(s.postControlResult), false},
		{"POST", "/v1/instructions", accessInstructions, s.actor(api.HeaderActionInstructions, s.addInstruction), false},
		{"DELETE", "/v1/instructions/{id}", accessInstructions, s.actor(api.HeaderActionInstructions, s.deleteInstruction), false},
		{"POST", "/v1/messages", accessSend, s.actor(api.HeaderActionSend, s.postMessages), true},
		{"POST", "/v1/sessions/{id}/permissions", accessWrite, s.writer(s.postPermission), false},
		{"GET", "/v1/permissions/{id}/decision", accessWrite, s.writer(s.waitDecision), false},
		{"POST", "/v1/permissions/{id}/decide", accessApprove, s.actor(api.HeaderActionApprove, s.decidePermission), true},
		{"POST", "/v1/sessions/{id}/move", accessMove, s.actor(api.HeaderActionMove, s.postMove), false},
		{"PUT", "/v1/moves/{id}/bundle", accessWrite, s.writer(s.putMoveBundle), false},
		{"PUT", "/v1/sessions/{id}/snapshots/{machine}", accessWrite, s.writer(s.putSnapshot), false},
		{"GET", "/v1/moves/{id}/bundle", accessWrite, s.writer(s.getMoveBundle), false},
		{"POST", "/v1/moves/{id}/result", accessWrite, s.writer(s.postMoveResult), false},
		{"PUT", "/v1/machines/self/move-key", accessWrite, s.writer(s.putMoveKey), false},
		{"POST", "/v1/sessions/{id}/blocked-on", accessWrite, s.writer(s.postBlockedOn), false},
		{"PUT", "/v1/sessions/{id}/usage", accessWrite, s.writer(s.putUsage), false},
		{"GET", "/v1/sessions/{id}/messages/next", accessWrite, s.writer(s.pollModMessage), false},
		{"POST", "/v1/messages/{id}/result", accessWrite, s.writer(s.postMessageResult), false},
		{"POST", "/v1/machines/{name}/start", accessStart, s.actor(api.HeaderActionStart, s.postStart), false},
		{"POST", "/v1/tasks", accessTasks, s.actor(api.HeaderActionTasks, s.createTask), false},
		{"POST", "/v1/notes", accessTasks, s.actor(api.HeaderActionTasks, s.addNote), false},
		{"POST", "/v1/tasks/review/ignore", accessTasks, s.actor(api.HeaderActionTasks, s.ignoreUntasked), false},
		{"POST", "/v1/tasks/{id}/state", accessTasks, s.actor(api.HeaderActionTasks, s.setTaskState), false},
		{"POST", "/v1/tasks/{id}/merge", accessTasks, s.actor(api.HeaderActionTasks, s.mergeTask), false},
		{"POST", "/v1/tasks/{id}/sessions", accessTasks, s.actor(api.HeaderActionTasks, s.linkTask), false},
		{"PATCH", "/v1/tasks/{id}", accessTasks, s.actor(api.HeaderActionTasks, s.editTask), false},
		{"POST", "/v1/knowledge", accessKnowledge, s.actor(api.HeaderActionKnowledge, s.addKnowledge), false},
		{"POST", "/v1/knowledge/{id}/confirm", accessKnowledge, s.actor(api.HeaderActionKnowledge, s.confirmKnowledge), false},
		{"POST", "/v1/knowledge/{id}/decide", accessKnowledgeWeb, s.webOnly(api.HeaderActionKnowledge, s.decideKnowledge), false},
		{"DELETE", "/v1/knowledge/{id}", accessKnowledgeWeb, s.webOnly(api.HeaderActionKnowledge, s.retireKnowledge), false},

		{"GET", "/v1/sessions", accessRead, s.reader(s.listSessions), true},
		{"GET", "/v1/sessions/{id}", accessRead, s.reader(s.getSession), true},
		{"GET", "/v1/machines", accessRead, s.reader(s.listMachines), false},
		{"GET", "/v1/inbox", accessRead, s.reader(s.getInbox), true},
		{"GET", "/v1/tasks", accessRead, s.reader(s.listTasks), false},
		{"GET", "/v1/tasks/day", accessRead, s.reader(s.getTaskDay), false},
		{"GET", "/v1/tasks/standup", accessRead, s.reader(s.getTaskStandup), false},
		{"GET", "/v1/tasks/review", accessRead, s.reader(s.getTaskReview), false},
		{"GET", "/v1/knowledge", accessRead, s.reader(s.listKnowledge), false},
		{"GET", "/v1/knowledge/local", accessWrite, s.writer(s.knowledgeLocal), false},
		{"GET", "/v1/instructions", accessRead, s.reader(s.listInstructions), false},
		{"GET", "/v1/messages/{id}", accessRead, s.reader(s.getMessage), false},
		{"GET", "/v1/moves/{id}", accessRead, s.reader(s.getMove), false},
		{"GET", "/v1/starts/{id}", accessRead, s.reader(s.getStart), false},
		{"POST", "/v1/logins", accessMachine, s.writer(s.createLogin), false},
		{"GET", "/v1/web-sessions", accessMachine, s.writer(s.listWebSessions), false},
		{"DELETE", "/v1/web-sessions/{id}", accessMachine, s.writer(s.revokeWebSession), false},
	}
}

// tryHandle registers pattern and reports false if the mux rejects it as
// conflicting with one already registered.
func tryHandle(mux *http.ServeMux, pattern string, h http.HandlerFunc) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	mux.HandleFunc(pattern, h)
	return true
}

// mux registers routes, a JSON 405 for a known path with the wrong method,
// and a JSON 404 for everything else.
func (s *Server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	allowed := map[string][]string{}
	var order []string
	for _, rt := range s.routes() {
		h := rt.handler
		if rt.guest {
			h = guestRoute(h)
		}
		mux.Handle(rt.method+" "+rt.path, h)
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
		notAllowed := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", allow)
			writeError(w, http.StatusMethodNotAllowed, "method "+r.Method+" not allowed; use "+allow)
		}
		if !tryHandle(mux, p, notAllowed) {
			// A literal path next to a method-specific wildcard, such as
			// /v1/tasks/day beside PATCH /v1/tasks/{id}, conflicts with a
			// method-less pattern. Register the methods the path lacks.
			for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
				if !slices.Contains(methods, m) && !(m == "HEAD" && slices.Contains(methods, "GET")) {
					mux.HandleFunc(m+" "+p, notAllowed)
				}
			}
		}
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint: "+r.URL.Path)
	})
	return mux
}
