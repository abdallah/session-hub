package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

const (
	// attachGuestGone ends an attach whose guest output stream has been
	// closed this long, or never opened.
	attachGuestGone = 5 * time.Second
	// attachIdle ends an attach with no bytes either way for this long.
	attachIdle = 30 * time.Minute
	// attachSweep is how often the hub checks those limits.
	attachSweep = 2 * time.Second
	// attachKeepalive is how long a stream stays silent before a NUL byte
	// goes out, so a proxy's idle timeout never cuts a quiet mirror. Both
	// readers drop NUL.
	attachKeepalive = 20 * time.Second
	// attachQueue is how many chunks each direction buffers; a full queue
	// holds the sender's POST until the reader catches up.
	attachQueue = 64
	// maxAttachOutput caps one POST of terminal bytes from the machine.
	maxAttachOutput = 1 << 20
	// attachWriteWait is the write deadline of each streamed chunk.
	attachWriteWait = 30 * time.Second
)

// attachRelay is one attach: terminal bytes from the machine to the guest
// (out) and keystrokes from the guest to the machine (in).
type attachRelay struct {
	session, machine string
	out, in          chan []byte
	done             chan struct{} // closed when the attach ends

	// Guarded by attachHub.mu.
	reading     map[chan []byte]bool // which streams have a reader
	created     time.Time
	guestLeft   time.Time // when the guest output stream closed; zero while open
	active      time.Time // last bytes either way
	machineSeen bool      // the machine opened its input stream or posted output
}

// attachHub relays attaches in memory. A restart drops them; the guest
// asks again.
type attachHub struct {
	mu       sync.Mutex
	m        map[string]*attachRelay
	sweeping bool
	now      func() time.Time
}

func newAttachHub() *attachHub {
	return &attachHub{m: map[string]*attachRelay{}, now: time.Now}
}

// open makes the relay for attach id unless it exists.
func (h *attachHub) open(id, session, machine string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.m[id]; ok {
		return
	}
	now := h.now()
	h.m[id] = &attachRelay{session: session, machine: machine,
		out: make(chan []byte, attachQueue), in: make(chan []byte, attachQueue), done: make(chan struct{}),
		reading: map[chan []byte]bool{}, created: now, guestLeft: now, active: now}
	if !h.sweeping {
		h.sweeping = true
		go h.sweepLoop()
	}
}

func (h *attachHub) get(id string) *attachRelay {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.m[id]
}

// end closes attach id's relay, which returns both streams.
func (h *attachHub) end(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.endLocked(id)
}

func (h *attachHub) endLocked(id string) {
	if a, ok := h.m[id]; ok {
		close(a.done)
		delete(h.m, id)
	}
}

// take makes the caller ch's one reader; false when it has one.
func (h *attachHub) take(a *attachRelay, ch chan []byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if a.reading[ch] {
		return false
	}
	a.reading[ch] = true
	if ch == a.out {
		a.guestLeft = time.Time{}
	} else {
		a.machineSeen = true
	}
	return true
}

func (h *attachHub) release(a *attachRelay, ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(a.reading, ch)
	if ch == a.out {
		a.guestLeft = h.now()
	}
}

// touch records traffic; fromMachine marks the machine side as connected.
func (h *attachHub) touch(a *attachRelay, fromMachine bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a.active = h.now()
	if fromMachine {
		a.machineSeen = true
	}
}

// sweep ends the attaches past a limit: no guest for attachGuestGone, no
// traffic for attachIdle, or no machine within store.ControlTTL (the
// request expired unclaimed). It reports whether any attach is left.
func (h *attachHub) sweep(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, a := range h.m {
		if (!a.guestLeft.IsZero() && now.Sub(a.guestLeft) >= attachGuestGone) ||
			now.Sub(a.active) >= attachIdle ||
			(!a.machineSeen && now.Sub(a.created) >= store.ControlTTL) {
			h.endLocked(id)
		}
	}
	if len(h.m) == 0 {
		h.sweeping = false
	}
	return h.sweeping
}

func (h *attachHub) sweepLoop() {
	t := time.NewTicker(attachSweep)
	defer t.Stop()
	for range t.C {
		if !h.sweep(h.now()) {
			return
		}
	}
}

// isAttachOutput reports whether r is POST
// /v1/machines/self/attach/{id}/output, whose body may pass MaxBodyBytes.
func isAttachOutput(r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/machines/self/attach/")
	id, tail, _ := strings.Cut(rest, "/")
	return r.Method == http.MethodPost && ok && id != "" && tail == "output"
}

func (s *Server) postAttach(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if !p.inScope(id) {
		writeError(w, http.StatusForbidden, "this sign-in may attach only to its own session")
		return
	}
	by := api.RequestedByDashboard
	if p.web == nil {
		by = api.RequestedByMachinePrefix + p.machine.Name
	}
	req, created, err := s.store.CreateControlAction(r.Context(), id, api.ActionAttach, by)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.attach.open(req.ID, req.SessionID, req.Machine)
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
		s.control.notify(req.Machine)
	}
	writeJSON(w, status, api.Attach{ID: req.ID, SessionID: req.SessionID, Machine: req.Machine})
}

// openAttach returns the open attach the path names, or writes the error.
// A machine route passes its caller as machine; the guest routes pass p and
// "".
func (s *Server) openAttach(w http.ResponseWriter, r *http.Request, p principal, machine string) (*attachRelay, bool) {
	id := r.PathValue("id")
	if !store.ValidControlID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid attach id %q", id))
		return nil, false
	}
	a := s.attach.get(id)
	if a == nil {
		writeError(w, http.StatusNotFound, "no open attach "+id)
		return nil, false
	}
	if !p.inScope(a.session) {
		writeError(w, http.StatusForbidden, "this sign-in may attach only to its own session")
		return nil, false
	}
	if machine != "" && a.machine != machine {
		writeError(w, http.StatusConflict, "attach "+id+" belongs to another machine")
		return nil, false
	}
	return a, true
}

func (s *Server) attachOutput(w http.ResponseWriter, r *http.Request, p principal) {
	if a, ok := s.openAttach(w, r, p, ""); ok {
		s.streamAttach(w, r, a, a.out)
	}
}

func (s *Server) attachInput(w http.ResponseWriter, r *http.Request, p principal) {
	if a, ok := s.openAttach(w, r, p, ""); ok {
		s.sendAttach(w, r, a, a.in, false)
	}
}

func (s *Server) machineAttachInput(w http.ResponseWriter, r *http.Request, p principal) {
	if a, ok := s.openAttach(w, r, principal{}, p.machine.Name); ok {
		s.streamAttach(w, r, a, a.in)
	}
}

func (s *Server) machineAttachOutput(w http.ResponseWriter, r *http.Request, p principal) {
	if a, ok := s.openAttach(w, r, principal{}, p.machine.Name); ok {
		s.sendAttach(w, r, a, a.out, true)
	}
}

// streamAttach writes ch's chunks to w until the attach ends, the caller
// leaves, or the server stops.
func (s *Server) streamAttach(w http.ResponseWriter, r *http.Request, a *attachRelay, ch chan []byte) {
	// The mux routes HEAD to a GET pattern; a HEAD must not take the reader.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "use GET to stream")
		return
	}
	if !s.attach.take(a, ch) {
		writeError(w, http.StatusConflict, "this attach stream already has a reader")
		return
	}
	defer s.attach.release(a, ch)
	rc := http.NewResponseController(w)
	// sessionhub server's ReadTimeout would cancel the request mid-stream.
	_ = rc.SetReadDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	write := func(b []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(attachWriteWait))
		if _, err := w.Write(b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if rc.Flush() != nil { // send the headers now
		return
	}
	keep := time.NewTicker(attachKeepalive)
	defer keep.Stop()
	for {
		select {
		case b := <-ch:
			if !write(b) {
				return
			}
			keep.Reset(attachKeepalive)
		case <-keep.C:
			if !write([]byte{0}) {
				return
			}
		case <-a.done:
			// Bytes posted just before the end still go out.
			for {
				select {
				case b := <-ch:
					if !write(b) {
						return
					}
				default:
					return
				}
			}
		case <-s.control.done:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// sendAttach queues the request body on ch and answers 204.
func (s *Server) sendAttach(w http.ResponseWriter, r *http.Request, a *attachRelay, ch chan []byte, fromMachine bool) {
	body, err := io.ReadAll(r.Body) // logRequests caps it
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body is larger than %d bytes", tooBig.Limit))
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(body) > 0 {
		select {
		case <-a.done:
		case ch <- body:
		case <-s.control.done:
			writeError(w, http.StatusServiceUnavailable, "the server is stopping")
			return
		case <-r.Context().Done():
			return
		}
	}
	select {
	case <-a.done:
		writeError(w, http.StatusGone, "the attach ended")
		return
	default:
	}
	s.attach.touch(a, fromMachine)
	w.WriteHeader(http.StatusNoContent)
}
