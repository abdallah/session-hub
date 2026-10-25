package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// rollbackV18 removes what schema v18 added.
func rollbackV18(t *testing.T, s *Store) {
	t.Helper()
	rollbackV19(t, s)
	for _, q := range []string{"DROP TABLE snapshots", "ALTER TABLE moves DROP COLUMN seal_id", "ALTER TABLE moves DROP COLUMN snapshot_at"} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

const snapID, snapID2 = "mv_snapsnapsnapsnapsnap01", "mv_snapsnapsnapsnapsnap02"

// offline leaves tower's last poll behind ControlPollWindow and keeps
// bluebox polling.
func (e *controlEnv) offline() {
	e.t.Helper()
	e.clock.Advance(ControlPollWindow + time.Second)
	e.poll(e.bluebox)
}

func TestPutSnapshot(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	if err := e.s.CheckSnapshot(ctx, e.bluebox.ID, moveSID, "tower"); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("another machine's session: %v", err)
	}
	if err := e.s.CheckSnapshot(ctx, e.tower.ID, moveSID, "tower"); !errors.Is(err, ErrNotControllable) {
		t.Errorf("sealed for itself: %v", err)
	}
	if err := e.s.CheckSnapshot(ctx, e.tower.ID, moveSID, "nope"); !errors.Is(err, ErrNotControllable) {
		t.Errorf("unknown recipient: %v", err)
	}
	old, err := e.s.PutSnapshot(ctx, e.tower.ID, moveSID, "bluebox", snapID, 100)
	if err != nil || old != "" {
		t.Fatalf("first: %q %v", old, err)
	}
	if _, err := e.s.PutSnapshot(ctx, e.tower.ID, moveSID, "bluebox", snapID, 100); !errors.Is(err, ErrNotControllable) {
		t.Errorf("reused id: %v", err)
	}
	e.clock.Advance(time.Minute)
	old, err = e.s.PutSnapshot(ctx, e.tower.ID, moveSID, "bluebox", snapID2, 200)
	if err != nil || old != snapID {
		t.Fatalf("replace: %q %v", old, err)
	}
	x, err := e.s.GetSession(ctx, moveSID)
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Snapshots) != 1 || x.Snapshots[0].Machine != "bluebox" || x.Snapshots[0].Size != 200 || !x.Snapshots[0].TakenAt.Equal(e.clock.Now()) {
		t.Errorf("session snapshots %+v", x.Snapshots)
	}
}

func TestMoveFromSnapshot(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	var linked [2]string
	e.s.SetSnapshotLinker(func(moveID, sealID string) error { linked = [2]string{moveID, sealID}; return nil })
	e.offline()
	if _, err := e.s.CreateMove(ctx, moveSID, "bluebox", "web:phone"); !errors.Is(err, ErrNotControllable) {
		t.Fatalf("offline without a snapshot: %v", err)
	}
	takenAt := e.clock.Now()
	if _, err := e.s.PutSnapshot(ctx, e.tower.ID, moveSID, "bluebox", snapID, 123); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateMove(ctx, moveSID, "cloud", "web:phone"); !errors.Is(err, ErrNotControllable) {
		t.Errorf("cloud from a snapshot: %v", err)
	}
	// The agent was working when tower went quiet: the snapshot still moves.
	if _, err := e.s.db.Exec(`UPDATE sessions SET agent_state = 'working' WHERE id = ?`, moveSID); err != nil {
		t.Fatal(err)
	}
	mv, err := e.s.CreateMove(ctx, moveSID, "bluebox", "web:phone")
	if err != nil {
		t.Fatal(err)
	}
	if mv.State != api.MoveUploaded || mv.SnapshotAt == nil || !mv.SnapshotAt.Equal(takenAt) || mv.BundleSize != 123 {
		t.Errorf("move %+v", mv)
	}
	if linked != [2]string{mv.ID, snapID} {
		t.Errorf("linked %v", linked)
	}
	claim, ok, err := e.s.ClaimMove(ctx, e.bluebox.ID)
	if err != nil || !ok || claim.Request.Action != api.ActionMoveIn || claim.Move.SealID != snapID {
		t.Fatalf("target claim: %+v %v %v", claim.Move, ok, err)
	}
	if _, err := e.s.FinishMove(ctx, e.bluebox.ID, mv.ID, api.MoveResultIn{State: api.MoveDone}); err != nil {
		t.Fatal(err)
	}
	// Tower is back: it gets the finish for the moved session.
	e.poll(e.tower)
	fin, ok, err := e.s.ClaimMove(ctx, e.tower.ID)
	if err != nil || !ok || fin.Request.Action != api.ActionMoveOut || fin.Move.State != api.MoveDone || fin.Move.SealID != snapID {
		t.Fatalf("finish: %+v %v %v", fin.Move, ok, err)
	}
	// The session is bluebox's now, so the sweep drops the snapshot.
	gone, err := e.s.SweepSnapshots(ctx, MaxSnapshotBytes)
	if err != nil || len(gone) != 1 || gone[0] != snapID {
		t.Errorf("sweep after the move: %v %v", gone, err)
	}
}

func TestMoveFromSnapshotLinkFails(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	e.s.SetSnapshotLinker(func(string, string) error { return errors.New("disk full") })
	e.offline()
	if _, err := e.s.PutSnapshot(ctx, e.tower.ID, moveSID, "bluebox", snapID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateMove(ctx, moveSID, "bluebox", "web:phone"); err == nil {
		t.Fatal("move created though the link failed")
	}
	if n := e.count("SELECT COUNT(*) FROM moves"); n != 0 {
		t.Errorf("%d moves after a failed link", n)
	}
}

func TestSweepSnapshots(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	e.session(e.tower, "other", "w1:p2")
	if _, err := e.s.PutSnapshot(ctx, e.tower.ID, moveSID, "bluebox", snapID, 600); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Hour)
	if _, err := e.s.PutSnapshot(ctx, e.tower.ID, "other", "bluebox", snapID2, 600); err != nil {
		t.Fatal(err)
	}
	// Over the cap: the older one goes.
	gone, err := e.s.SweepSnapshots(ctx, 1000)
	if err != nil || len(gone) != 1 || gone[0] != snapID {
		t.Fatalf("cap: %v %v", gone, err)
	}
	// Past the TTL: the rest goes.
	e.clock.Advance(api.SnapshotTTL + time.Minute)
	if gone, err = e.s.SweepSnapshots(ctx, MaxSnapshotBytes); err != nil || len(gone) != 1 || gone[0] != snapID2 {
		t.Fatalf("ttl: %v %v", gone, err)
	}
	ids, err := e.s.SnapshotIDs(ctx)
	if err != nil || len(ids) != 0 {
		t.Errorf("left %v %v", ids, err)
	}
}

func TestSweepSnapshotsEnded(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	if _, err := e.s.PutSnapshot(ctx, e.tower.ID, moveSID, "bluebox", snapID, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.s.AddEvent(ctx, e.tower.ID, moveSID, api.EventIn{Kind: api.KindEnded, Source: api.SourceHooks}); err != nil {
		t.Fatal(err)
	}
	gone, err := e.s.SweepSnapshots(ctx, MaxSnapshotBytes)
	if err != nil || len(gone) != 1 {
		t.Errorf("ended: %v %v", gone, err)
	}
}

// rollbackV19 removes what schema v19 added.
func rollbackV19(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec("ALTER TABLE permission_requests DROP COLUMN answers"); err != nil {
		t.Fatalf("drop answers: %v", err)
	}
}
