package server

import (
	"fmt"
	"net/http"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

// postStart is POST /v1/machines/{name}/start: ask the machine's watcher to
// start a new Claude session with Remote Control on.
func (s *Server) postStart(w http.ResponseWriter, r *http.Request, p principal) {
	name := r.PathValue("name")
	if !store.ValidMachineName(name) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid machine name %q", name))
		return
	}
	var in api.StartIn
	if !decode(w, r, &in) {
		return
	}
	req, err := s.store.CreateStart(r.Context(), name, in, decider(p))
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.control.notify(req.Machine)
	writeJSON(w, http.StatusAccepted, req)
}

// getStart is GET /v1/starts/{id}.
func (s *Server) getStart(w http.ResponseWriter, r *http.Request, _ principal) {
	id := r.PathValue("id")
	if !store.ValidStartID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid start id %q", id))
		return
	}
	req, err := s.store.GetStart(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}
