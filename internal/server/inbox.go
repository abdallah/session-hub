package server

import (
	"net/http"
	"slices"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// getInbox is GET /v1/inbox: the sessions that need you, grouped.
func (s *Server) getInbox(w http.ResponseWriter, r *http.Request, p principal) {
	in, err := s.store.Inbox(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	if sc := p.scope(); sc != "" {
		in.Items = slices.DeleteFunc(in.Items, func(it api.InboxItem) bool { return it.Session.ID != sc })
		in.Counts = api.InboxCounts{}
		for i := range in.Items {
			guestView(&in.Items[i].Session)
		}
		for _, it := range in.Items {
			switch it.Group {
			case api.InboxMoveFailed:
				in.Counts.MoveFailed++
			case api.InboxBlocked:
				in.Counts.Blocked++
			case api.InboxWaiting:
				in.Counts.Waiting++
			case api.InboxFinished:
				in.Counts.Finished++
			}
		}
	}
	writeJSON(w, http.StatusOK, in)
}

// dismissInbox is POST /v1/inbox/{id}/dismiss.
func (s *Server) dismissInbox(w http.ResponseWriter, r *http.Request, _ principal) {
	s.triage(w, r, false)
}

// snoozeInbox is POST /v1/inbox/{id}/snooze.
func (s *Server) snoozeInbox(w http.ResponseWriter, r *http.Request, _ principal) {
	s.triage(w, r, true)
}

// triage stores a dismiss, or a snooze when snooze is set, and answers 204.
// A dismiss ignores until. The store validates since and until.
func (s *Server) triage(w http.ResponseWriter, r *http.Request, snooze bool) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in api.TriageIn
	if !decode(w, r, &in) {
		return
	}
	var until *time.Time
	if snooze {
		if in.Until == nil {
			writeError(w, http.StatusBadRequest, "until is required")
			return
		}
		until = in.Until
	}
	if err := s.store.Triage(r.Context(), id, in.Since, until); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
