package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ok\n")
}

// pathID returns the {id} path value if it is a well-formed full ID, or
// writes a 400 and returns false.
func pathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !store.ValidSessionID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid session id %q", id))
		return "", false
	}
	return id, true
}

// respondSession writes the session's current state after a write.
func (s *Server) respondSession(w http.ResponseWriter, r *http.Request, id string, status int) {
	x, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, status, x)
}

func (s *Server) upsertSession(w http.ResponseWriter, r *http.Request, p principal) {
	var u api.SessionUpsert
	if !decode(w, r, &u) {
		return
	}
	created, err := s.store.UpsertSession(r.Context(), p.machine.ID, u)
	if err != nil {
		s.storeError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.respondSession(w, r, u.ID, status)
}

func (s *Server) putHerdrSessions(w http.ResponseWriter, r *http.Request, p principal) {
	var put api.HerdrSessionsPut
	if !decode(w, r, &put) {
		return
	}
	res, err := s.store.ReconcileHerdr(r.Context(), p.machine.ID, put)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) postEvent(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var e api.EventIn
	if !decode(w, r, &e) {
		return
	}
	if err := s.store.AddEvent(r.Context(), p.machine.ID, id, e); err != nil {
		s.storeError(w, err)
		return
	}
	s.respondSession(w, r, id, http.StatusOK)
}

func (s *Server) postReport(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in api.ReportIn
	if !decode(w, r, &in) {
		return
	}
	if err := s.store.AddReport(r.Context(), p.machine.ID, id, in); err != nil {
		s.storeError(w, err)
		return
	}
	s.respondSession(w, r, id, http.StatusOK)
}

func (s *Server) postTitle(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in api.TitleIn
	if !decode(w, r, &in) {
		return
	}
	if err := s.store.SetTitle(r.Context(), p.machine.ID, id, in.Title); err != nil {
		s.storeError(w, err)
		return
	}
	s.respondSession(w, r, id, http.StatusOK)
}

// MaxDigestBytes caps a digest body, below the 64 KiB cap on every body.
const MaxDigestBytes = 16 << 10

func (s *Server) putDigest(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxDigestBytes))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "digest body is larger than 16 KiB")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var in api.DigestIn
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if _, err := s.store.PutDigest(r.Context(), p.machine.ID, id, in); err != nil {
		s.storeError(w, err)
		return
	}
	s.respondSession(w, r, id, http.StatusOK)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request, p principal) {
	q := r.URL.Query()
	f := store.ListFilter{Machine: q.Get("machine")}
	if v := q.Get("live"); v != "" {
		live, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("live=%q: want true or false", v))
			return
		}
		f.LiveOnly = live
	}
	list, err := s.store.ListSessions(r.Context(), f)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if p.web != nil {
		w.Header().Set(api.HeaderSessionName, p.web.Name)
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request, _ principal) {
	id, err := s.store.ResolveID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	d, err := s.store.SessionDetail(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) listMachines(w http.ResponseWriter, r *http.Request, _ principal) {
	list, err := s.store.ListMachines(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
