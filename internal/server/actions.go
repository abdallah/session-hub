package server

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

// decider names who made a request: web:<name> for a browser,
// machine:<name> for a machine token.
func decider(p principal) string {
	if p.web != nil {
		return "web:" + p.web.Name
	}
	return api.RequestedByMachinePrefix + p.machine.Name
}

func webName(p principal) string {
	if p.web == nil {
		return ""
	}
	return p.web.Name
}

// listInstructions is GET /v1/instructions.
func (s *Server) listInstructions(w http.ResponseWriter, r *http.Request, _ principal) {
	list, err := s.store.Instructions(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// addInstruction is POST /v1/instructions.
func (s *Server) addInstruction(w http.ResponseWriter, r *http.Request, p principal) {
	var in api.InstructionIn
	if !decode(w, r, &in) {
		return
	}
	by := p.machine.Name
	if p.web != nil {
		by = "web:" + p.web.Name
	}
	rule, err := s.store.AddInstruction(r.Context(), in.Text, by)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if s.ruleAdded != nil {
		s.ruleAdded(rule)
	}
	writeJSON(w, http.StatusCreated, rule)
}

// deleteInstruction is DELETE /v1/instructions/{id}.
func (s *Server) deleteInstruction(w http.ResponseWriter, r *http.Request, _ principal) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid rule id %q", r.PathValue("id")))
		return
	}
	if err := s.store.DeleteInstruction(r.Context(), id); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// postMessages is POST /v1/messages. It wakes the long poll of each machine
// that got a message.
func (s *Server) postMessages(w http.ResponseWriter, r *http.Request, p principal) {
	var in api.MessagesIn
	if !decode(w, r, &in) {
		return
	}
	sender, limitKey := api.SenderDashboard, "web:"+webName(p)
	if p.web == nil {
		sender, limitKey = "cli on "+p.machine.Name, "machine:"+p.machine.Name
		if in.FromSession != "" {
			if !store.ValidSessionID(in.FromSession) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid from_session %q", in.FromSession))
				return
			}
			// A machine may name only its own sessions as the sender.
			if err := s.store.CheckSessionOwner(r.Context(), p.machine.ID, in.FromSession); err != nil {
				s.storeError(w, err)
				return
			}
			short := in.FromSession
			if len(short) > 8 {
				short = short[:8]
			}
			sender = "session " + short
		}
	}
	results, machines, err := s.store.SendMessages(r.Context(), in.SessionIDs, in.Text, sender, limitKey)
	if err != nil {
		s.storeError(w, err)
		return
	}
	for _, m := range machines {
		s.control.notify(m)
	}
	writeJSON(w, http.StatusOK, api.MessagesOut{Results: results})
}

// getMessage is GET /v1/messages/{id}.
func (s *Server) getMessage(w http.ResponseWriter, r *http.Request, _ principal) {
	m, err := s.store.GetMessage(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// postPermission is POST /v1/sessions/{id}/permissions, from the
// permission hook on the session's own machine.
func (s *Server) postPermission(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in api.PermissionIn
	if !decode(w, r, &in) {
		return
	}
	req, err := s.store.CreatePermission(r.Context(), p.machine.ID, id, in)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, req)
}

// waitDecision is GET /v1/permissions/{id}/decision?wait=N: the permission
// hook's long poll. It answers 200 with the request once it is not open,
// else 204 after the wait. A decision wakes it at once; an answer in the
// terminal or an expiry shows up within permCheck.
func (s *Server) waitDecision(w http.ResponseWriter, r *http.Request, p principal) {
	// The mux routes HEAD to a GET pattern; a HEAD has no body to wait for.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "use GET to wait for a decision")
		return
	}
	id := r.PathValue("id")
	if !store.ValidPermissionID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request id %q", id))
		return
	}
	wait, ok := pollWait(w, r)
	if !ok {
		return
	}
	slot := "decision:" + p.machine.Name
	if !s.control.acquireMax(slot, maxDecisionPolls) {
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("machine %q already has %d open decision polls", p.machine.Name, maxDecisionPolls))
		return
	}
	defer s.control.release(slot)
	// Check the owner and the state before the timer starts, so a refused
	// or settled request answers at once.
	req, err := s.store.PermissionFor(r.Context(), p.machine.ID, id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if req.State != api.PermissionOpen {
		writeJSON(w, http.StatusOK, req)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wait + pollWriteSlack))
	timeout := s.after(wait)
	key := "permission:" + id
	var woken <-chan struct{}
	// A poll that ends without a decision's wake-up forgets its channel, so
	// no entry outlives it.
	defer func() { s.control.drop(key, woken) }()
	for {
		// Take the wake-up channel before the read, so a decision made
		// between the read and the select still wakes this poll.
		woken = s.control.waiter(key)
		req, err = s.store.PermissionFor(r.Context(), p.machine.ID, id)
		if err != nil {
			s.storeError(w, err)
			return
		}
		if req.State != api.PermissionOpen {
			writeJSON(w, http.StatusOK, req)
			return
		}
		select {
		case <-woken:
		case <-time.After(s.permCheck):
		case <-timeout:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-s.control.done:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// decidePermission is POST /v1/permissions/{id}/decide.
func (s *Server) decidePermission(w http.ResponseWriter, r *http.Request, p principal) {
	id := r.PathValue("id")
	if !store.ValidPermissionID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request id %q", id))
		return
	}
	var in api.DecisionIn
	if !decode(w, r, &in) {
		return
	}
	req, err := s.store.DecidePermission(r.Context(), id, in, decider(p))
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.control.notify("permission:" + id)
	writeJSON(w, http.StatusOK, req)
}
