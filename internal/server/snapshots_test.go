package server

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

const testSnapID = "mv_testsnaptestsnap000001"

func (e *env) putSnapshot(token, sid, machine, id string, body []byte) (int, []byte) {
	e.t.Helper()
	code, b, _ := e.doHdr("PUT", "/v1/sessions/"+sid+"/snapshots/"+machine, token, "", map[string]string{api.SnapshotHeaderID: id}, body)
	return code, b
}

func TestSnapshotMoveFlow(t *testing.T) {
	e := newEnv(t)
	towerKey, _ := e.moveSetup(sid1)
	var ms []api.Machine
	e.must(200, "GET", "/v1/machines", e.tokA, nil, &ms)
	if ms[1].Name != "tower" || ms[1].MovePublicKey != towerKey {
		t.Fatalf("machines: %+v", ms)
	}
	sealed := bytes.Repeat([]byte{0x5A}, 2048)
	if code, b := e.putSnapshot(e.tokB, sid1, "tower", testSnapID, sealed); code != http.StatusConflict {
		t.Errorf("another machine's session: %d %s", code, b)
	}
	if code, b := e.putSnapshot(e.tokA, sid1, "bluebox", "nope", sealed); code != http.StatusBadRequest {
		t.Errorf("bad id: %d %s", code, b)
	}
	if code, b := e.putSnapshot(e.tokA, sid1, "bluebox", testSnapID, sealed); code != http.StatusNoContent {
		t.Fatalf("put: %d %s", code, b)
	}
	snapDir := filepath.Join(filepath.Dir(e.moveDir), "snapshots")
	if fi, err := os.Stat(filepath.Join(snapDir, testSnapID+".sealed")); err != nil || fi.Size() != 2048 {
		t.Fatalf("snapshot file: %v %v", fi, err)
	}
	var x api.Session
	e.must(200, "GET", "/v1/sessions/"+sid1, e.tokA, nil, &x)
	if len(x.Snapshots) != 1 || x.Snapshots[0].Machine != "bluebox" {
		t.Errorf("session snapshots %+v", x.Snapshots)
	}

	// Tower goes quiet; the move uses the snapshot.
	e.clock.Advance(store.ControlPollWindow + time.Second)
	e.recordPoll(e.tokB)
	mv := e.startMove(sid1, "bluebox")
	if mv.State != api.MoveUploaded || mv.SnapshotAt == nil {
		t.Fatalf("move %+v", mv)
	}
	in := e.claim(e.tokB)
	if in.Request.Action != api.ActionMoveIn || in.Move.SealID != testSnapID || in.Move.PeerKey != towerKey {
		t.Fatalf("move-in claim %+v", in.Move)
	}
	code, got, _ := e.do("GET", "/v1/moves/"+mv.ID+"/bundle", e.tokB, nil)
	if code != 200 || !bytes.Equal(got, sealed) {
		t.Fatalf("download: %d, %d bytes", code, len(got))
	}
	e.must(200, "POST", "/v1/moves/"+mv.ID+"/result", e.tokB, api.MoveResultIn{State: api.MoveDone}, nil)

	// The session is bluebox's: the sweep removes the snapshot and its file.
	e.server.sweepSnapshots(t.Context())
	if _, err := os.Stat(filepath.Join(snapDir, testSnapID+".sealed")); !os.IsNotExist(err) {
		t.Errorf("snapshot file after the move: %v", err)
	}
}

func TestSnapshotMoveOfflineWithoutSnapshot(t *testing.T) {
	e := newEnv(t)
	e.moveSetup(sid1)
	e.clock.Advance(store.ControlPollWindow + time.Second)
	e.recordPoll(e.tokB)
	code, b, _ := e.doHdr("POST", "/v1/sessions/"+sid1+"/move", "", e.web, map[string]string{api.HeaderAction: api.HeaderActionMove},
		api.MoveIn{Target: "bluebox"})
	if code != http.StatusConflict || !strings.Contains(string(b), "no snapshot") {
		t.Errorf("offline without a snapshot: %d %s", code, b)
	}
}
