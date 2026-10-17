package server

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/abdallah/session-hub/internal/api"
)

func knowledgePathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid knowledge id %q", r.PathValue("id")))
		return 0, false
	}
	return id, true
}

// knowledgeBy names who wrote: the machine for a machine token, web:<name>
// for a browser.
func knowledgeBy(p principal) string {
	if p.web != nil {
		return "web:" + p.web.Name
	}
	return p.machine.Name
}

// listKnowledge is GET /v1/knowledge?state=&scope=&q=.
func (s *Server) listKnowledge(w http.ResponseWriter, r *http.Request, _ principal) {
	q := r.URL.Query()
	list, err := s.store.ListKnowledge(r.Context(), q.Get("state"), q.Get("scope"), q.Get("q"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// knowledgeLocal is GET /v1/knowledge/local, for the calling machine.
func (s *Server) knowledgeLocal(w http.ResponseWriter, r *http.Request, p principal) {
	out, err := s.store.KnowledgeLocal(r.Context(), p.machine.ID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// addKnowledge is POST /v1/knowledge. A machine proposes; the dashboard
// adds an active entry.
func (s *Server) addKnowledge(w http.ResponseWriter, r *http.Request, p principal) {
	var in api.KnowledgeIn
	if !decode(w, r, &in) {
		return
	}
	start := s.store.Now()
	out, err := s.store.AddKnowledge(r.Context(), in, knowledgeBy(p), p.web != nil)
	if err != nil {
		s.storeError(w, err)
		return
	}
	// A client_id replay returns the stored entry, which predates the request.
	if p.web == nil && s.knowledgeProposed != nil && !out.Entry.CreatedAt.Before(start) {
		s.knowledgeProposed(out.Entry)
	}
	writeJSON(w, http.StatusCreated, out)
}

// decideKnowledge is POST /v1/knowledge/{id}/decide.
func (s *Server) decideKnowledge(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := knowledgePathID(w, r)
	if !ok {
		return
	}
	var in api.KnowledgeDecisionIn
	if !decode(w, r, &in) {
		return
	}
	k, err := s.store.DecideKnowledge(r.Context(), id, in, knowledgeBy(p))
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, k)
}

// confirmKnowledge is POST /v1/knowledge/{id}/confirm.
func (s *Server) confirmKnowledge(w http.ResponseWriter, r *http.Request, _ principal) {
	id, ok := knowledgePathID(w, r)
	if !ok {
		return
	}
	if err := s.store.ConfirmKnowledge(r.Context(), id); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// retireKnowledge is DELETE /v1/knowledge/{id}.
func (s *Server) retireKnowledge(w http.ResponseWriter, r *http.Request, _ principal) {
	id, ok := knowledgePathID(w, r)
	if !ok {
		return
	}
	if err := s.store.RetireKnowledge(r.Context(), id); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
