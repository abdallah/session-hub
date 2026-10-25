package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

// snapshotOrphanAfter is how long a snapshot file without a row, or a
// partial upload, may stay: past an upload's longest run.
const snapshotOrphanAfter = bundleIOFor + time.Minute

// SetSnapshotDir sets the directory for sealed snapshots and lets moves from
// a snapshot link its file in as their bundle. Without one, the snapshot
// route answers 503 and no move uses a snapshot.
func (s *Server) SetSnapshotDir(dir string) {
	s.snapshotDir = dir
	s.store.SetSnapshotLinker(s.linkSnapshot)
}

func (s *Server) snapshotPath(id string) string { return filepath.Join(s.snapshotDir, id+bundleSuffix) }

// linkSnapshot puts snapshot sealID's file in place as move moveID's
// bundle: a hard link, with a fresh modification time so the move sweep
// keeps it for moveBundleTTL.
func (s *Server) linkSnapshot(moveID, sealID string) error {
	if s.moveDir == "" || s.snapshotDir == "" {
		return errors.New("this server has no bundle directory")
	}
	if err := os.MkdirAll(s.moveDir, 0o700); err != nil {
		return err
	}
	dst := s.bundlePath(moveID)
	if err := os.Link(s.snapshotPath(sealID), dst); err != nil {
		return err
	}
	now := time.Now()
	if err := os.Chtimes(dst, now, now); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

// putSnapshot is PUT /v1/sessions/{id}/snapshots/{machine}: the session's
// machine stores a snapshot sealed for machine, under the ID in the
// X-Snapshot-Id header. It replaces the session's snapshot for machine.
func (s *Server) putSnapshot(w http.ResponseWriter, r *http.Request, p principal) {
	sid, ok := pathID(w, r)
	if !ok {
		return
	}
	recipient := r.PathValue("machine")
	id := r.Header.Get(api.SnapshotHeaderID)
	if !store.ValidMoveID(id) {
		writeError(w, http.StatusBadRequest, "invalid "+api.SnapshotHeaderID+" header")
		return
	}
	if s.snapshotDir == "" {
		writeError(w, http.StatusServiceUnavailable, "this server has no snapshot directory")
		return
	}
	ctx := r.Context()
	if err := s.store.CheckSnapshot(ctx, p.machine.ID, sid, recipient); err != nil {
		s.storeError(w, err)
		return
	}
	tmp, n, ok := s.receiveSealed(w, r, s.snapshotDir, id)
	if !ok {
		return
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, s.snapshotPath(id)); err != nil {
		if errors.Is(err, os.ErrExist) {
			writeError(w, http.StatusConflict, "snapshot id "+id+" is already used")
			return
		}
		s.internalError(w, err)
		return
	}
	old, err := s.store.PutSnapshot(ctx, p.machine.ID, sid, recipient, id, n)
	if err != nil {
		os.Remove(s.snapshotPath(id))
		s.storeError(w, err)
		return
	}
	if old != "" {
		s.removeSnapshot(old)
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeSnapshot deletes snapshot id's file, if there is one.
func (s *Server) removeSnapshot(id string) {
	if err := os.Remove(s.snapshotPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Printf("snapshots: remove %s: %v", id, err)
	}
}

// sweepSnapshots drops the snapshots the store no longer keeps (see
// store.SweepSnapshots), then files without a row and partial uploads older
// than snapshotOrphanAfter. It logs errors and never fails a request.
func (s *Server) sweepSnapshots(ctx context.Context) {
	if s.snapshotDir == "" {
		return
	}
	gone, err := s.store.SweepSnapshots(ctx, store.MaxSnapshotBytes)
	if err != nil {
		s.log.Printf("snapshots: sweep: %v", err)
		return
	}
	for _, id := range gone {
		s.removeSnapshot(id)
	}
	entries, err := os.ReadDir(s.snapshotDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Printf("snapshots: read %s: %v", s.snapshotDir, err)
		}
		return
	}
	keep, err := s.store.SnapshotIDs(ctx)
	if err != nil {
		s.log.Printf("snapshots: list: %v", err)
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < snapshotOrphanAfter {
			continue
		}
		if id, ok := strings.CutSuffix(e.Name(), bundleSuffix); ok && keep[id] {
			continue
		}
		os.Remove(filepath.Join(s.snapshotDir, e.Name()))
	}
}
