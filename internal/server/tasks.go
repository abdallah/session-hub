package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

// taskPathID returns the {id} path value if it is a well-formed task ID, or
// writes a 400 and returns false.
func taskPathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !store.ValidTaskID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid task id %q", id))
		return "", false
	}
	return id, true
}

// actorOf decides who acts. The session cookie is web:<name>. A machine token
// with a session id is that session: an agent, bound to the machine. A
// machine token without one is machine:<name>, the CLI.
func actorOf(p principal, sessionID string) store.Actor {
	switch {
	case p.web != nil:
		return store.Actor{Name: "web:" + p.web.Name}
	case sessionID != "":
		return store.Actor{Name: "session:" + sessionID, Agent: true, MachineID: p.machine.ID}
	default:
		return store.Actor{Name: api.RequestedByMachinePrefix + p.machine.Name}
	}
}

// listTasks is GET /v1/tasks. state is open, the default and the only value.
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request, _ principal) {
	if st := r.URL.Query().Get("state"); st != "" && st != "open" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid state %q: use open", st))
		return
	}
	tasks, err := s.store.OpenTasks(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

// createTask is POST /v1/tasks: 201 for a new task, 200 for an existing one.
func (s *Server) createTask(w http.ResponseWriter, r *http.Request, p principal) {
	var in api.TaskIn
	if !decode(w, r, &in) {
		return
	}
	t, created, err := s.store.CreateTask(r.Context(), in, actorOf(p, in.SessionID))
	if err != nil {
		s.storeError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, t)
}

// SetRefURLs sets the link templates for note refs.
func (s *Server) SetRefURLs(u store.RefURLs) { s.refURLs = u }

// addNote is POST /v1/notes: a worklog note goes to a task.
func (s *Server) addNote(w http.ResponseWriter, r *http.Request, p principal) {
	var in api.NoteIn
	if !decode(w, r, &in) {
		return
	}
	res, err := s.store.AddNote(r.Context(), in, actorOf(p, in.SessionID), s.refURLs)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// setTaskState is POST /v1/tasks/{id}/state.
func (s *Server) setTaskState(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := taskPathID(w, r)
	if !ok {
		return
	}
	var in api.TaskStateIn
	if !decode(w, r, &in) {
		return
	}
	t, err := s.store.SetTaskState(r.Context(), id, in.To, in.Note, in.EventID, actorOf(p, in.SessionID))
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// mergeTask is POST /v1/tasks/{id}/merge. The body has no session id, so the
// actor is you.
func (s *Server) mergeTask(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := taskPathID(w, r)
	if !ok {
		return
	}
	var in api.TaskMergeIn
	if !decode(w, r, &in) {
		return
	}
	// MergeTask returns the dropped proposal; the route answers with the target.
	if _, err := s.store.MergeTask(r.Context(), id, in.Into, actorOf(p, "")); err != nil {
		s.storeError(w, err)
		return
	}
	t, err := s.store.GetTask(r.Context(), in.Into)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// linkTask is POST /v1/tasks/{id}/sessions.
func (s *Server) linkTask(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := taskPathID(w, r)
	if !ok {
		return
	}
	var in api.TaskLinkIn
	if !decode(w, r, &in) {
		return
	}
	t, err := s.store.LinkTask(r.Context(), id, in.SessionID, in.EventID, actorOf(p, in.SessionID))
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// editTask is PATCH /v1/tasks/{id}. The body has no session id, so the actor
// is always you.
func (s *Server) editTask(w http.ResponseWriter, r *http.Request, _ principal) {
	id, ok := taskPathID(w, r)
	if !ok {
		return
	}
	var in api.TaskEditIn
	if !decode(w, r, &in) {
		return
	}
	t, err := s.store.EditTask(r.Context(), id, in)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// getTaskDay is GET /v1/tasks/day?date=YYYY-MM-DD&tz=Area/City. A missing tz
// is UTC.
func (s *Server) getTaskDay(w http.ResponseWriter, r *http.Request, _ principal) {
	q := r.URL.Query()
	date := q.Get("date")
	if date == "" {
		writeError(w, http.StatusBadRequest, "date is required (YYYY-MM-DD)")
		return
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid date %q: use YYYY-MM-DD", date))
		return
	}
	loc := time.UTC
	if tz := q.Get("tz"); tz != "" {
		var err error
		if loc, err = time.LoadLocation(tz); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid tz %q: use an IANA name such as Asia/Amman", tz))
			return
		}
	}
	day, err := s.store.TaskDay(r.Context(), date, loc)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, day)
}

// getTaskReview is GET /v1/tasks/review.
func (s *Server) getTaskReview(w http.ResponseWriter, r *http.Request, _ principal) {
	rev, err := s.store.TaskReview(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rev)
}

// ignoreUntasked is POST /v1/tasks/review/ignore: 204. The body names one
// session or a batch of at most api.MaxTaskIgnoreBatch.
func (s *Server) ignoreUntasked(w http.ResponseWriter, r *http.Request, _ principal) {
	var in api.TaskIgnoreIn
	if !decode(w, r, &in) {
		return
	}
	ids := in.SessionIDs
	switch {
	case in.SessionID != "" && len(ids) > 0:
		writeError(w, http.StatusBadRequest, "send session_id or session_ids, not both")
		return
	case in.SessionID != "":
		ids = []string{in.SessionID}
	case len(ids) == 0:
		writeError(w, http.StatusBadRequest, "session_id or session_ids is required")
		return
	case len(ids) > api.MaxTaskIgnoreBatch:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d session ids per request", api.MaxTaskIgnoreBatch))
		return
	}
	for _, id := range ids {
		if !store.ValidSessionID(id) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid session id %q", id))
			return
		}
	}
	if err := s.store.IgnoreUntasked(r.Context(), ids...); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
