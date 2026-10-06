package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/store"
)

// The Claude Code mod's routes. The mod never calls them itself: `sessionhub mod`
// does, with the machine token. Each one takes the machine that owns the
// session. See docs/server.md, "The Claude Code mod".

const (
	// maxModPollsPerSession caps one session's open message polls: one mod
	// runs per session; the second slot covers a reload's overlap.
	maxModPollsPerSession = 2
	// maxModPollsPerMachine caps one machine's open mod message polls.
	maxModPollsPerMachine = 64
)

// postBlockedOn sets or clears what a session waits on. The text and the
// question a clear names are cleaned to one line of at most
// api.MaxBlockedOnRunes runes here, whatever the client sent, so a clear
// matches the stored text when it names the same question.
func (s *Server) postBlockedOn(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in api.BlockedOnIn
	if !decode(w, r, &in) {
		return
	}
	text := termtext.Clean(in.Text, api.MaxBlockedOnRunes)
	clears := termtext.Clean(in.Clears, api.MaxBlockedOnRunes)
	var err error
	switch {
	case text != "" && clears != "":
		writeError(w, http.StatusBadRequest, "text and clears are both set")
		return
	case clears != "":
		err = s.store.ClearBlockedOn(r.Context(), p.machine.ID, id, clears)
	default:
		err = s.store.SetBlockedOn(r.Context(), p.machine.ID, id, text)
	}
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.respondSession(w, r, id, http.StatusOK)
}

// putUsage stores a session's context window fill and cost.
func (s *Server) putUsage(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in api.UsageIn
	if !decode(w, r, &in) {
		return
	}
	if err := s.store.SetUsage(r.Context(), p.machine.ID, id, in); err != nil {
		s.storeError(w, err)
		return
	}
	s.respondSession(w, r, id, http.StatusOK)
}

// pollModMessage is a mod's long poll for its session's next message: 200
// with an api.ModMessage, or 204 when none arrived within wait=N seconds (1
// to 30, default 30). Every claim attempt records the poll, which keeps the
// session messageable and the watcher away from its messages.
func (s *Server) pollModMessage(w http.ResponseWriter, r *http.Request, p principal) {
	// The mux routes HEAD to a GET pattern. A HEAD must not claim a message.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "use GET to poll")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	wait, ok := pollWait(w, r)
	if !ok {
		return
	}
	// Check the owner before anything else, so a refused poll holds no slot.
	if err := s.store.CheckSessionOwner(r.Context(), p.machine.ID, id); err != nil {
		s.storeError(w, err)
		return
	}
	machineSlot := "modpolls:" + p.machine.Name
	if !s.control.acquireMax(machineSlot, maxModPollsPerMachine) {
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("machine %q already has %d open message polls", p.machine.Name, maxModPollsPerMachine))
		return
	}
	defer s.control.release(machineSlot)
	sessionSlot := "modpoll:" + id
	if !s.control.acquireMax(sessionSlot, maxModPollsPerSession) {
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("session %s already has %d open message polls", id, maxModPollsPerSession))
		return
	}
	defer s.control.release(sessionSlot)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wait + pollWriteSlack))
	// Record the poll before the timer starts, as pollControl does, so the
	// session is messageable once the wait begins.
	if err := s.store.RecordModPoll(r.Context(), p.machine.ID, id); err != nil {
		s.storeError(w, err)
		return
	}
	timeout := s.after(wait)
	for {
		// POST /v1/messages wakes the machine's polls. Take the channel
		// before the claim, so a message sent between the claim and the
		// select still wakes this poll.
		woken := s.control.waiter(p.machine.Name)
		m, ok, err := s.store.ClaimSessionMessage(r.Context(), p.machine.ID, id)
		if err != nil {
			s.storeError(w, err)
			return
		}
		if ok {
			writeJSON(w, http.StatusOK, m)
			return
		}
		// A message offered a moment ago is offered again MessageRetry after
		// that offer.
		var retry <-chan time.Time
		if d, ok, err := s.store.NextSessionMessageRetry(r.Context(), p.machine.ID, id); err != nil {
			s.internalError(w, err)
			return
		} else if ok {
			retry = s.after(d)
		}
		select {
		case <-retry:
		case <-woken:
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

// postMessageResult records a mod's result for a message it claimed. The
// detail is cleaned to one line of at most api.MaxControlDetailRunes runes.
func (s *Server) postMessageResult(w http.ResponseWriter, r *http.Request, p principal) {
	id := r.PathValue("id")
	if !store.ValidMessageID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid message id %q", id))
		return
	}
	var in api.MessageResultIn
	if !decode(w, r, &in) {
		return
	}
	in.Detail = termtext.Clean(in.Detail, api.MaxControlDetailRunes)
	m, err := s.store.FinishModMessage(r.Context(), p.machine.ID, id, in)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}
