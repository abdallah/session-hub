package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

const (
	// moveBundleTTL is the longest a bundle file stays on disk.
	moveBundleTTL = time.Hour
	// moveSweepEvery is how often sessionhub server expires moves and deletes
	// bundle files.
	moveSweepEvery = time.Minute
	// bundleIOFor bounds one bundle upload or download, past the server's
	// 30-second read and write timeouts.
	bundleIOFor  = 10 * time.Minute
	bundleSuffix = ".sealed"
)

// maxBundleBytes caps a sealed bundle upload. Tests lower it.
var maxBundleBytes int64 = api.MaxSealedBundle

// SetMoveDir sets the directory for sealed bundles. Run sets moves/ next to
// the database; without one, the bundle routes answer 503.
func (s *Server) SetMoveDir(dir string) { s.moveDir = dir }

// isBundleUpload reports whether r is PUT /v1/moves/{id}/bundle, the one
// request whose body may pass MaxBodyBytes.
func isBundleUpload(r *http.Request) bool {
	if r.Method != http.MethodPut {
		return false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/moves/")
	id, tail, _ := strings.Cut(rest, "/")
	return ok && id != "" && tail == "bundle"
}

func (s *Server) bundlePath(id string) string { return filepath.Join(s.moveDir, id+bundleSuffix) }

// removeBundle deletes move id's bundle file, if there is one.
func (s *Server) removeBundle(id string) {
	if s.moveDir == "" {
		return
	}
	if err := os.Remove(s.bundlePath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Printf("moves: remove bundle %s: %v", id, err)
	}
}

// moveID returns the {id} path value if it is a move ID, or writes a 400.
func moveID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !store.ValidMoveID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid move id %q", id))
		return "", false
	}
	return id, true
}

// sweepMoves fails moves past their timeouts and wakes their sources for
// the finish, then deletes bundle files whose move ended or is unknown, and
// any file older than moveBundleTTL. It logs errors and never fails a
// request.
func (s *Server) sweepMoves(ctx context.Context) {
	ended, err := s.store.ExpireMoves(ctx)
	if err != nil {
		s.log.Printf("moves: expire: %v", err)
		return
	}
	for _, m := range ended {
		s.removeBundle(m.ID)
		s.control.notify(m.Source)
	}
	if s.moveDir == "" {
		return
	}
	entries, err := os.ReadDir(s.moveDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Printf("moves: read %s: %v", s.moveDir, err)
		}
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(s.moveDir, e.Name())
		if time.Since(info.ModTime()) > moveBundleTTL {
			os.Remove(p)
			continue
		}
		id, ok := strings.CutSuffix(e.Name(), bundleSuffix)
		if !ok {
			continue // an upload in progress
		}
		m, err := s.store.GetMove(ctx, id)
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalid) || (err == nil && !api.MoveOpen(m.State)) {
			os.Remove(p)
		}
	}
}

// runMoveSweeper runs sweepMoves every moveSweepEvery until ctx ends.
func (s *Server) runMoveSweeper(ctx context.Context) {
	t := time.NewTicker(moveSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepMoves(ctx)
		}
	}
}

// postMove is POST /v1/sessions/{id}/move. It wakes the source's poll.
func (s *Server) postMove(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in api.MoveIn
	if !decode(w, r, &in) {
		return
	}
	s.sweepMoves(r.Context())
	mv, err := s.store.CreateMove(r.Context(), id, strings.TrimSpace(in.Target), decider(p))
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.control.notify(mv.Source)
	writeJSON(w, http.StatusAccepted, mv)
}

// getMove is GET /v1/moves/{id}.
func (s *Server) getMove(w http.ResponseWriter, r *http.Request, _ principal) {
	id, ok := moveID(w, r)
	if !ok {
		return
	}
	s.sweepMoves(r.Context())
	mv, err := s.store.GetMove(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mv)
}

// putMoveKey is PUT /v1/machines/self/move-key.
func (s *Server) putMoveKey(w http.ResponseWriter, r *http.Request, p principal) {
	var in api.MoveKeyIn
	if !decode(w, r, &in) {
		return
	}
	if err := s.store.SetMoveKey(r.Context(), p.machine.ID, in.PublicKey); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// putMoveBundle is PUT /v1/moves/{id}/bundle: the source streams the sealed
// bundle to a temp file, which is renamed into place before the move turns
// uploaded. It wakes the target's poll.
func (s *Server) putMoveBundle(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := moveID(w, r)
	if !ok {
		return
	}
	if s.moveDir == "" {
		writeError(w, http.StatusServiceUnavailable, "this server has no bundle directory")
		return
	}
	ctx := r.Context()
	s.sweepMoves(ctx)
	if _, err := s.store.MoveForUpload(ctx, p.machine.ID, id); err != nil {
		s.storeError(w, err)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bundleIOFor))
	_ = rc.SetWriteDeadline(time.Now().Add(bundleIOFor))
	if err := os.MkdirAll(s.moveDir, 0o700); err != nil {
		s.internalError(w, err)
		return
	}
	if err := os.Chmod(s.moveDir, 0o700); err != nil { // fix a directory that already existed
		s.internalError(w, err)
		return
	}
	f, err := os.CreateTemp(s.moveDir, id+".*.tmp")
	if err != nil {
		s.internalError(w, err)
		return
	}
	tmp := f.Name()
	defer os.Remove(tmp) // the final name is a hard link, so this never removes the bundle
	body := &errRecorder{r: r.Body}
	n, err := io.Copy(f, body)
	cerr := f.Close()
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(body.err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge, "the bundle is larger than "+strconv.FormatInt(maxBundleBytes, 10)+" bytes")
		return
	case body.err != nil:
		s.log.Printf("moves: read bundle %s: %v", id, body.err)
		writeError(w, http.StatusBadRequest, "the upload was cut off")
		return
	case err != nil:
		s.internalError(w, err)
		return
	case cerr != nil:
		s.internalError(w, cerr)
		return
	case n == 0:
		writeError(w, http.StatusBadRequest, "the bundle is empty")
		return
	}
	// Link, not rename: a concurrent upload for the same move must not
	// replace a bundle that is already in place.
	if err := os.Link(tmp, s.bundlePath(id)); err != nil {
		if errors.Is(err, os.ErrExist) {
			writeError(w, http.StatusConflict, "this move already has a bundle")
			return
		}
		s.internalError(w, err)
		return
	}
	mv, err := s.store.MoveUploaded(ctx, p.machine.ID, id, n)
	if err != nil {
		s.removeBundle(id) // this request created it
		s.storeError(w, err)
		return
	}
	s.control.notify(mv.Target)
	writeJSON(w, http.StatusOK, mv)
}

// getMoveBundle is GET /v1/moves/{id}/bundle: the target streams the
// sealed bundle.
func (s *Server) getMoveBundle(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := moveID(w, r)
	if !ok {
		return
	}
	if s.moveDir == "" {
		writeError(w, http.StatusServiceUnavailable, "this server has no bundle directory")
		return
	}
	ctx := r.Context()
	s.sweepMoves(ctx)
	if _, err := s.store.MoveForDownload(ctx, p.machine.ID, id); err != nil {
		s.storeError(w, err)
		return
	}
	f, err := os.Open(s.bundlePath(id))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusGone, "the bundle is gone")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		s.internalError(w, err)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(bundleIOFor))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, f); err != nil {
		s.log.Printf("moves: send bundle %s: %v", id, err)
	}
}

// postMoveResult is POST /v1/moves/{id}/result. A move that ended loses its
// bundle, and its source's poll wakes for the finish.
func (s *Server) postMoveResult(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := moveID(w, r)
	if !ok {
		return
	}
	var in api.MoveResultIn
	if !decode(w, r, &in) {
		return
	}
	s.sweepMoves(r.Context())
	mv, err := s.store.FinishMove(r.Context(), p.machine.ID, id, in)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if !api.MoveOpen(mv.State) {
		s.removeBundle(id)
	}
	s.control.notify(mv.Source)
	writeJSON(w, http.StatusOK, mv)
}

// errRecorder remembers the error its reader returned, so a failed copy can
// tell a read error (the client) from a write error (the disk).
type errRecorder struct {
	r   io.Reader
	err error
}

func (e *errRecorder) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF {
		e.err = err
	}
	return n, err
}
