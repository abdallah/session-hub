package server

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

const (
	// maxPollsPerMachine caps a machine's open long polls. One watcher runs
	// per machine; the second slot covers a restart's overlap.
	maxPollsPerMachine = 2
	// maxPollWait is the longest, and the default, poll wait.
	maxPollWait = 30 * time.Second
	// maxDecisionPolls caps one machine's open decision polls: one per
	// waiting permission hook.
	maxDecisionPolls = 20
	// pollWriteSlack is added to a poll's write deadline on top of its wait.
	pollWriteSlack = 10 * time.Second
)

// controlHub tracks open long polls and wakes them when a request is
// created for their machine. It is in-process: a restart drops open polls,
// and the watchers reconnect. Pending requests are in the database.
type controlHub struct {
	mu   sync.Mutex
	open map[string]int           // machine name → open polls
	wake map[string]chan struct{} // machine name → closed on its next new request
	done chan struct{}            // closed by StopPolls
	stop sync.Once
}

func newControlHub() *controlHub {
	return &controlHub{open: map[string]int{}, wake: map[string]chan struct{}{}, done: make(chan struct{})}
}

func (h *controlHub) acquire(machine string) bool { return h.acquireMax(machine, maxPollsPerMachine) }

// acquireMax takes one of n slots under key. Control polls use the machine
// name; decision polls use "decision:" + the machine name.
func (h *controlHub) acquireMax(key string, n int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[key] >= n {
		return false
	}
	h.open[key]++
	return true
}

func (h *controlHub) release(machine string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[machine]--; h.open[machine] <= 0 {
		delete(h.open, machine)
	}
}

// waiter returns a channel that closes on the machine's next new request.
func (h *controlHub) waiter(machine string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.wake[machine]
	if !ok {
		c = make(chan struct{})
		h.wake[machine] = c
	}
	return c
}

// drop forgets key's wake-up channel c when nothing notified it, so a poll
// that ends by timeout or by a read leaves no entry behind. A channel that a
// later waiter replaced stays.
func (h *controlHub) drop(key string, c <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.wake[key]; ok && (<-chan struct{})(cur) == c {
		delete(h.wake, key)
	}
}

func (h *controlHub) notify(machine string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.wake[machine]; ok {
		close(c)
		delete(h.wake, machine)
	}
}

// StopPolls ends every open long poll with 204, and makes later polls answer
// 204 at once. `sessionhub server` registers it with http.Server.RegisterOnShutdown,
// so a restart doesn't wait out open polls.
func (s *Server) StopPolls() { s.control.stop.Do(func() { close(s.control.done) }) }

func (s *Server) postRemoteControl(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	by := api.RequestedByDashboard
	if p.web == nil {
		by = api.RequestedByMachinePrefix + p.machine.Name
	}
	req, created, err := s.store.CreateControl(r.Context(), id, by)
	if err != nil {
		s.storeError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
		s.control.notify(req.Machine)
	}
	writeJSON(w, status, req)
}

func (s *Server) pollControl(w http.ResponseWriter, r *http.Request, p principal) {
	// The mux routes HEAD to a GET pattern. A HEAD must not claim a request.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "use GET to poll")
		return
	}
	wait, ok := pollWait(w, r)
	if !ok {
		return
	}
	name := p.machine.Name
	if !s.control.acquire(name) {
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("machine %q already has %d open polls", name, maxPollsPerMachine))
		return
	}
	defer s.control.release(name)
	// sessionhub server's WriteTimeout (30 s) is shorter than a full poll plus its
	// answer, so extend this response's deadline.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wait + pollWriteSlack))
	if err := s.store.RecordPoll(r.Context(), p.machine.ID); err != nil {
		s.internalError(w, err)
		return
	}
	timeout := s.after(wait)
	for {
		// Take the wake-up channel before the claim, so a request created
		// between the claim and the select still wakes this poll.
		woken := s.control.waiter(name)
		claim, ok, err := s.store.ClaimControl(r.Context(), p.machine.ID)
		if err == nil && !ok {
			claim, ok, err = s.store.ClaimMessage(r.Context(), p.machine.ID)
		}
		if err == nil && !ok {
			claim, ok, err = s.store.ClaimMove(r.Context(), p.machine.ID)
		}
		if err == nil && !ok {
			claim, ok, err = s.store.ClaimStart(r.Context(), p.machine.ID)
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		if ok {
			writeJSON(w, http.StatusOK, claim)
			return
		}
		// A message offered a moment ago (a busy result, or a watcher that
		// never answered) is offered again MessageRetry after its last
		// offer, so wake for that instead of waiting for the next poll.
		var retry <-chan time.Time
		if d, ok, err := s.store.NextMessageRetry(r.Context(), p.machine.ID); err != nil {
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

func (s *Server) postControlResult(w http.ResponseWriter, r *http.Request, p principal) {
	id := r.PathValue("id")
	if !store.ValidControlID(id) && !store.ValidMessageID(id) && !store.ValidStartID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request id %q", id))
		return
	}
	var in api.ControlResultIn
	if !decode(w, r, &in) {
		return
	}
	if store.ValidStartID(id) {
		st, err := s.store.FinishStart(r.Context(), p.machine.ID, id, in)
		if err != nil {
			s.storeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
		return
	}
	if store.ValidMessageID(id) {
		m, err := s.store.FinishMessage(r.Context(), p.machine.ID, id, in)
		if err != nil {
			s.storeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, m)
		return
	}
	req, err := s.store.FinishControl(r.Context(), p.machine.ID, id, in)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// pollWait reads a long poll's wait=N (seconds, clamped to 1 to 30; 30 when
// absent). It writes a 400 and returns false for a value that is not a
// number.
func pollWait(w http.ResponseWriter, r *http.Request) (time.Duration, bool) {
	v := r.URL.Query().Get("wait")
	if v == "" {
		return maxPollWait, true
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("wait=%q: want a number of seconds", v))
		return 0, false
	}
	return time.Duration(min(max(n, 1), 30)) * time.Second, true
}
