package store

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// moveKey is a fresh X25519 public key in the server's form.
func moveKey(t *testing.T) string {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
}

// moveReady registers id on tower in a herdr pane with an idle agent and a
// GitHub remote, gives both machines a key, and records a poll from both.
func (e *controlEnv) moveReady(id string) (towerKey, blueboxKey string) {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: id, Agent: "claude", Source: api.SourcePlugin,
		CWD: "/home/user/proj", GitRepo: "git@github.com:o/r.git", GitBranch: "main", HerdrSession: "default",
		HerdrWorkspace: "w1", HerdrPane: "w1:p1", AgentState: "idle"}); err != nil {
		e.t.Fatal(err)
	}
	towerKey, blueboxKey = moveKey(e.t), moveKey(e.t)
	if err := e.s.SetMoveKey(ctx, e.tower.ID, towerKey); err != nil {
		e.t.Fatal(err)
	}
	if err := e.s.SetMoveKey(ctx, e.bluebox.ID, blueboxKey); err != nil {
		e.t.Fatal(err)
	}
	e.poll(e.tower)
	e.poll(e.bluebox)
	return towerKey, blueboxKey
}

const moveSID = "3f2a9c10-1111-4a4a-8b8b-00000000aaaa"

func TestMigrateV8ToV9(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	if _, _, err := s.AddMachine(ctx, "tower", "", ""); err != nil {
		t.Fatal(err)
	}
	rollbackV9(t, s)
	if _, err := s.db.Exec("PRAGMA user_version = 8"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var v int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version %d %v, want %d", v, err, schemaVersion)
	}
	ms, err := s2.ListMachines(ctx)
	if err != nil || len(ms) != 1 || ms[0].MoveKey != "" || ms[0].MoveReady {
		t.Fatalf("machines after upgrade: %+v %v", ms, err)
	}
	var id int64
	s2.db.QueryRow("SELECT id FROM machines").Scan(&id)
	if err := s2.SetMoveKey(ctx, id, moveKey(t)); err != nil {
		t.Errorf("SetMoveKey after upgrade: %v", err)
	}
}

func TestSetMoveKeyAndMachines(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	if err := e.s.SetMoveKey(ctx, e.tower.ID, "nope"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad key: %v", err)
	}
	key := moveKey(t)
	if err := e.s.SetMoveKey(ctx, e.tower.ID, key); err != nil {
		t.Fatal(err)
	}
	e.poll(e.tower)
	raw, _ := api.ParseMoveKey(key)
	ms, _ := e.s.ListMachines(ctx)
	if ms[0].Name != "bluebox" || ms[0].MoveKey != "" || ms[0].MoveReady || ms[1].MoveKey != api.MoveKeyFingerprint(raw) || !ms[1].MoveReady {
		t.Fatalf("machines: %+v", ms)
	}
	e.clock.Advance(ControlPollWindow + time.Second)
	if ms, _ := e.s.ListMachines(ctx); ms[1].MoveReady {
		t.Error("tower is move-ready without a recent poll")
	}
}

func TestCreateMoveRules(t *testing.T) {
	ctx := context.Background()
	refused := func(t *testing.T, e *controlEnv, target, want string) {
		t.Helper()
		before := e.count("SELECT COUNT(*) FROM moves")
		_, err := e.s.CreateMove(ctx, moveSID, target, "machine:tower")
		if !errors.Is(err, ErrNotControllable) || !strings.Contains(err.Error(), want) {
			t.Errorf("target %s: %v, want 409 with %q", target, err, want)
		}
		if n := e.count("SELECT COUNT(*) FROM moves"); n != before {
			t.Errorf("a refused move was stored: %d moves, %d before", n, before)
		}
	}
	cases := []struct {
		name, target, want string
		setup              func(e *controlEnv)
	}{
		{"ended", "bluebox", "has ended", func(e *controlEnv) {
			e.s.AddEvent(ctx, e.tower.ID, moveSID, api.EventIn{Kind: api.KindEnded, Source: api.SourcePlugin})
		}},
		{"no pane", "bluebox", "not in herdr", func(e *controlEnv) {
			e.s.db.Exec("UPDATE sessions SET herdr_pane = '' WHERE id = ?", moveSID)
		}},
		{"working", "bluebox", "the agent is working", func(e *controlEnv) {
			e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: moveSID, Source: api.SourcePlugin, AgentState: "working"})
		}},
		{"blocked", "bluebox", "the agent is blocked", func(e *controlEnv) {
			e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: moveSID, Source: api.SourcePlugin, AgentState: "blocked"})
		}},
		{"source offline", "bluebox", `watcher on "tower" is offline`, func(e *controlEnv) {
			e.clock.Advance(ControlPollWindow + time.Second)
			e.poll(e.bluebox)
		}},
		{"source has no key", "bluebox", `"tower" has no move key`, func(e *controlEnv) {
			e.s.db.Exec("UPDATE machines SET move_key = '' WHERE id = ?", e.tower.ID)
		}},
		{"unknown target", "nosuch", `no machine named "nosuch"`, nil},
		{"same machine", "tower", `already on "tower"`, nil},
		{"target offline", "bluebox", `watcher on "bluebox" is offline`, func(e *controlEnv) {
			e.clock.Advance(ControlPollWindow + time.Second)
			e.poll(e.tower)
		}},
		{"target has no key", "bluebox", `"bluebox" has no move key`, func(e *controlEnv) {
			e.s.db.Exec("UPDATE machines SET move_key = '' WHERE id = ?", e.bluebox.ID)
		}},
		{"cloud without GitHub", api.MoveCloud, "GitHub remote", func(e *controlEnv) {
			e.s.db.Exec("UPDATE sessions SET git_repo = 'git@gitlab.com:o/r.git' WHERE id = ?", moveSID)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newControlEnv(t)
			e.moveReady(moveSID)
			if c.setup != nil {
				c.setup(e)
			}
			refused(t, e, c.target, c.want)
		})
	}

	e := newControlEnv(t)
	e.moveReady(moveSID)
	if _, err := e.s.CreateMove(ctx, "nosuch-session", "bluebox", "machine:tower"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}
	if _, err := e.s.CreateMove(ctx, moveSID, "bad name!", "machine:tower"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad target: %v", err)
	}
	if _, err := e.s.CreateMove(ctx, moveSID, "bluebox", "someone"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad by: %v", err)
	}
	mv, err := e.s.CreateMove(ctx, moveSID, "bluebox", "web:phone")
	if err != nil {
		t.Fatal(err)
	}
	if !ValidMoveID(mv.ID) || mv.State != api.MoveRequested || mv.Source != "tower" || mv.Target != "bluebox" ||
		mv.By != "web:phone" || !mv.CreatedAt.Equal(e.clock.Now()) {
		t.Errorf("move %+v", mv)
	}
	if got := e.get(moveSID).Move; got == nil || got.ID != mv.ID || got.State != api.MoveRequested {
		t.Errorf("session's move: %+v", got)
	}
	refused(t, e, "bluebox", "move "+mv.ID+" of this session is requested")
	if cm, err := e.s.CreateMove(ctx, moveSID, api.MoveCloud, "machine:bluebox"); err == nil {
		t.Errorf("a cloud move beside an open one: %+v", cm)
	}
}

func TestClaimMoveFlow(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	towerKey, blueboxKey := e.moveReady(moveSID)
	mv, err := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := e.s.ClaimMove(ctx, e.bluebox.ID); ok || err != nil {
		t.Fatalf("the target claimed a requested move: %v %v", ok, err)
	}
	out, ok, err := e.s.ClaimMove(ctx, e.tower.ID)
	if err != nil || !ok {
		t.Fatalf("source claim: %v %v", ok, err)
	}
	if out.Request.Action != api.ActionMoveOut || out.Request.ID != mv.ID || out.Request.Machine != "tower" ||
		out.Session.ID != moveSID || out.Move == nil || out.Move.State != api.MovePacking || out.Move.PeerKey != blueboxKey ||
		out.Move.Source != "tower" || out.Move.Target != "bluebox" {
		t.Fatalf("move-out claim %+v move %+v", out.Request, out.Move)
	}
	if _, ok, _ := e.s.ClaimMove(ctx, e.tower.ID); ok {
		t.Fatal("a packing move was handed out twice")
	}

	if _, err := e.s.MoveForUpload(ctx, e.bluebox.ID, mv.ID); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("target upload check: %v", err)
	}
	if _, err := e.s.MoveForUpload(ctx, e.tower.ID, mv.ID); err != nil {
		t.Errorf("source upload check: %v", err)
	}
	if _, err := e.s.MoveUploaded(ctx, e.tower.ID, mv.ID, 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty bundle: %v", err)
	}
	if _, err := e.s.MoveUploaded(ctx, e.tower.ID, mv.ID, api.MaxSealedBundle+1); !errors.Is(err, ErrInvalid) {
		t.Errorf("huge bundle: %v", err)
	}
	up, err := e.s.MoveUploaded(ctx, e.tower.ID, mv.ID, 1234)
	if err != nil || up.State != api.MoveUploaded || up.BundleSize != 1234 {
		t.Fatalf("uploaded: %+v %v", up, err)
	}
	if _, err := e.s.MoveUploaded(ctx, e.tower.ID, mv.ID, 1234); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("second upload (the target's step now): %v", err)
	}
	if _, err := e.s.MoveForDownload(ctx, e.bluebox.ID, mv.ID); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("download before the claim: %v", err)
	}

	in, ok, err := e.s.ClaimMove(ctx, e.bluebox.ID)
	if err != nil || !ok || in.Request.Action != api.ActionMoveIn || in.Request.Machine != "bluebox" || in.Move.State != api.MoveUnpacking ||
		in.Move.PeerKey != towerKey {
		t.Fatalf("move-in claim: %v %v %+v %+v", ok, err, in.Request, in.Move)
	}
	if _, err := e.s.MoveForDownload(ctx, e.tower.ID, mv.ID); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("source download: %v", err)
	}
	if _, err := e.s.MoveForDownload(ctx, e.bluebox.ID, mv.ID); err != nil {
		t.Errorf("target download: %v", err)
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveDone}); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("source result while unpacking: %v", err)
	}
	if _, err := e.s.FinishMove(ctx, e.bluebox.ID, mv.ID, api.MoveResultIn{State: api.MoveDone, CloudURL: "https://claude.ai/code/session_x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("machine move with a cloud link: %v", err)
	}
	done, err := e.s.FinishMove(ctx, e.bluebox.ID, mv.ID, api.MoveResultIn{State: api.MoveDone})
	if err != nil || done.State != api.MoveDone {
		t.Fatalf("done: %+v %v", done, err)
	}
	s := e.get(moveSID)
	if s.Machine != "bluebox" || s.HerdrPane != "" || s.HerdrSession != "" || s.HerdrWorkspace != "" || s.Move.State != api.MoveDone {
		t.Errorf("session after the move: %+v", s)
	}
	ev := e.events(moveSID, api.KindMoved)
	var p map[string]string
	if len(ev) != 1 || json.Unmarshal(ev[0].Payload, &p) != nil || p["move_id"] != mv.ID || p["from"] != "tower" || p["to"] != "bluebox" {
		t.Errorf("moved event: %+v", ev)
	}
	// The new owner's writes work; the old owner's do not.
	if _, err := e.s.UpsertSession(ctx, e.bluebox.ID, api.SessionUpsert{ID: moveSID, Source: api.SourcePlugin, HerdrPane: "w2:p1"}); err != nil {
		t.Errorf("target upsert: %v", err)
	}
	if _, err := e.s.UpsertSession(ctx, e.tower.ID, api.SessionUpsert{ID: moveSID, Source: api.SourcePlugin}); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("source upsert after the move: %v", err)
	}
}

func TestClaimMoveFinishOnce(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	mv, _ := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower")
	e.s.ClaimMove(ctx, e.tower.ID)
	e.s.MoveUploaded(ctx, e.tower.ID, mv.ID, 10)
	e.s.ClaimMove(ctx, e.bluebox.ID)
	if _, err := e.s.FinishMove(ctx, e.bluebox.ID, mv.ID, api.MoveResultIn{State: api.MoveFailed, Detail: "find clone: no clone of github.com/o/r on bluebox"}); err != nil {
		t.Fatal(err)
	}
	if s := e.get(moveSID); s.Machine != "tower" || s.HerdrPane != "w1:p1" {
		t.Errorf("a failed move changed the session: %+v", s)
	}
	if _, ok, _ := e.s.ClaimMove(ctx, e.bluebox.ID); ok {
		t.Error("the target got a finish")
	}
	fin, ok, err := e.s.ClaimMove(ctx, e.tower.ID)
	if err != nil || !ok || fin.Request.Action != api.ActionMoveOut || fin.Move.State != api.MoveFailed ||
		!strings.HasPrefix(fin.Move.Detail, "find clone:") || fin.Move.PeerKey != "" {
		t.Fatalf("finish: %v %v %+v", ok, err, fin.Move)
	}
	if _, ok, _ := e.s.ClaimMove(ctx, e.tower.ID); ok {
		t.Error("the finish was handed out twice")
	}
	if n := e.count("SELECT COUNT(*) FROM moves WHERE source_closed_at IS NOT NULL"); n != 1 {
		t.Errorf("source_closed_at set on %d moves", n)
	}
}

func TestFinishMoveBySource(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	mv, _ := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower")
	e.s.ClaimMove(ctx, e.tower.ID)
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveDone}); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("source done on a machine move: %v", err)
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: "uploaded"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("state uploaded through the result route: %v", err)
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveFailed, Detail: "line\nbreak"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("detail with a newline: %v", err)
	}
	f, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveFailed, Detail: "check pane: the agent is working"})
	if err != nil || f.State != api.MoveFailed {
		t.Fatalf("failed: %+v %v", f, err)
	}
	if _, ok, _ := e.s.ClaimMove(ctx, e.tower.ID); ok {
		t.Error("a source that failed its own step got a finish")
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveFailed}); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("second result: %v", err)
	}
}

// TestFinishMoveSourceNote: once a move ended and the source's part closed,
// the source may add a note to the detail (a restart or an archive that
// failed), and nothing else changes.
func TestFinishMoveSourceNote(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	mv, _ := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower")
	e.s.ClaimMove(ctx, e.tower.ID)
	e.s.MoveUploaded(ctx, e.tower.ID, mv.ID, 10)
	e.s.ClaimMove(ctx, e.bluebox.ID)
	note := api.MoveResultIn{State: api.MoveDone, Detail: "archive on tower failed: disk full"}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, note); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("a note while the target unpacks: %v", err)
	}
	if _, err := e.s.FinishMove(ctx, e.bluebox.ID, mv.ID, api.MoveResultIn{State: api.MoveDone, Detail: "not carried: .env"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, note); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("a note before the finish was claimed: %v", err)
	}
	if _, ok, _ := e.s.ClaimMove(ctx, e.tower.ID); !ok {
		t.Fatal("no finish")
	}
	for _, bad := range []api.MoveResultIn{{State: api.MoveFailed, Detail: "restart failed: x"}, {State: api.MoveDone}} {
		if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, bad); !errors.Is(err, ErrRequestClosed) {
			t.Errorf("note %+v: %v", bad, err)
		}
	}
	if _, err := e.s.FinishMove(ctx, e.bluebox.ID, mv.ID, note); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("the target added a note: %v", err)
	}
	got, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, note)
	if err != nil || got.State != api.MoveDone || got.Detail != "not carried: .env; archive on tower failed: disk full" {
		t.Fatalf("note: %+v %v", got, err)
	}
	// The same note again, a retry after a lost answer, is taken once.
	if got, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, note); err != nil || got.Detail != "not carried: .env; archive on tower failed: disk full" {
		t.Errorf("repeated note: %+v %v", got, err)
	}
	if s := e.get(moveSID); s.Machine != "bluebox" {
		t.Errorf("a note moved the session: %+v", s)
	}
	if moved := e.events(moveSID, api.KindMoved); len(moved) != 1 {
		t.Errorf("a note recorded another moved event: %+v", moved)
	}
	// A cancelled move (no route sets it yet) takes a failed note.
	e.s.db.Exec("UPDATE moves SET state = ?, detail = '' WHERE id = ?", api.MoveCancelled, mv.ID)
	if got, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveFailed, Detail: "restart failed: x"}); err != nil ||
		got.State != api.MoveCancelled || got.Detail != "restart failed: x" {
		t.Errorf("failed note on a cancelled move: %+v %v", got, err)
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveDone, Detail: "y"}); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("done note on a cancelled move: %v", err)
	}
	e.s.db.Exec("UPDATE moves SET state = ? WHERE id = ?", api.MoveDone, mv.ID)
	long := api.MoveResultIn{State: api.MoveDone, Detail: strings.Repeat("x", api.MaxMoveDetailRunes)}
	if got, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, long); err != nil || len([]rune(got.Detail)) != api.MaxMoveDetailRunes {
		t.Errorf("long note: %d runes, %v", len([]rune(got.Detail)), err)
	}
}

func TestCloudMoveDone(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	e.s.db.Exec("UPDATE machines SET move_key = '' WHERE id = ?", e.bluebox.ID) // a cloud move needs no target key
	mv, err := e.s.CreateMove(ctx, moveSID, api.MoveCloud, "machine:tower")
	if err != nil {
		t.Fatal(err)
	}
	out, ok, _ := e.s.ClaimMove(ctx, e.tower.ID)
	if !ok || out.Move.Target != api.MoveCloud || out.Move.PeerKey != "" {
		t.Fatalf("cloud claim %+v", out.Move)
	}
	if _, err := e.s.MoveUploaded(ctx, e.tower.ID, mv.ID, 10); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("a cloud move took a bundle: %v", err)
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveDone}); !errors.Is(err, ErrInvalid) {
		t.Errorf("cloud done without a link: %v", err)
	}
	if _, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveDone, CloudURL: "https://evil.test/x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("cloud done with a foreign link: %v", err)
	}
	const link = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"
	d, err := e.s.FinishMove(ctx, e.tower.ID, mv.ID, api.MoveResultIn{State: api.MoveDone, CloudURL: link})
	if err != nil || d.State != api.MoveDone || d.CloudURL != link {
		t.Fatalf("cloud done: %+v %v", d, err)
	}
	s := e.get(moveSID)
	if s.Status != api.StatusEnded || s.Machine != "tower" || s.Move.CloudURL != link {
		t.Errorf("session after a cloud move: %+v", s)
	}
	// No pane is left for Remote Control to resume the local copy in.
	if s.HerdrSession != "" || s.HerdrWorkspace != "" || s.HerdrPane != "" || s.Controllable {
		t.Errorf("herdr fields after a cloud move: %q %q %q controllable=%v", s.HerdrSession, s.HerdrWorkspace, s.HerdrPane, s.Controllable)
	}
	if _, _, err := e.s.CreateControl(ctx, moveSID, api.RequestedByDashboard); !errors.Is(err, ErrNotControllable) {
		t.Errorf("Remote Control after a cloud move: %v", err)
	}
	ended := e.events(moveSID, api.KindEnded)
	if len(ended) != 1 || !strings.Contains(string(ended[0].Payload), `"reason":"moved_to_cloud"`) {
		t.Errorf("ended events: %+v", ended)
	}
	if moved := e.events(moveSID, api.KindMoved); len(moved) != 1 || !strings.Contains(string(moved[0].Payload), link) {
		t.Errorf("moved events: %+v", moved)
	}
	if _, ok, _ := e.s.ClaimMove(ctx, e.tower.ID); ok {
		t.Error("a cloud move the source finished itself got a finish")
	}
}

func TestExpireMoves(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	mv, _ := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower")
	e.clock.Advance(MoveRequestTTL - time.Second)
	if got, _ := e.s.ExpireMoves(ctx); len(got) != 0 {
		t.Fatalf("expired early: %+v", got)
	}
	e.clock.Advance(time.Second)
	got, err := e.s.ExpireMoves(ctx)
	if err != nil || len(got) != 1 || got[0].ID != mv.ID || got[0].State != api.MoveFailed || got[0].Detail != "requested: timed out" {
		t.Fatalf("expire requested: %+v %v", got, err)
	}
	if _, ok, _ := e.s.ClaimMove(ctx, e.tower.ID); ok {
		t.Error("a move the source never claimed got a finish")
	}

	// A move stuck in packing fails after MoveStepTTL, and the source gets
	// a finish, because it may have ended the session.
	e.poll(e.tower)
	e.poll(e.bluebox)
	mv2, err := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower")
	if err != nil {
		t.Fatal(err)
	}
	e.s.ClaimMove(ctx, e.tower.ID)
	e.clock.Advance(MoveStepTTL)
	if got, _ := e.s.ExpireMoves(ctx); len(got) != 1 || got[0].ID != mv2.ID || got[0].Detail != "packing: timed out" {
		t.Fatalf("expire packing: %+v", got)
	}
	fin, ok, _ := e.s.ClaimMove(ctx, e.tower.ID)
	if !ok || fin.Move.ID != mv2.ID || fin.Move.State != api.MoveFailed {
		t.Errorf("finish after a timeout: %v %+v", ok, fin.Move)
	}
	if g, err := e.s.GetMove(ctx, mv2.ID); err != nil || g.State != api.MoveFailed {
		t.Errorf("GetMove: %+v %v", g, err)
	}
	if _, err := e.s.GetMove(ctx, "mv_nosuchnosuchnosuchnosu"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown move: %v", err)
	}
	if _, err := e.s.GetMove(ctx, "x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad move id: %v", err)
	}
}

// While a move is open, Remote Control must not start the session on the
// source: a new request is refused, and a request queued just before the
// move fails at the claim instead of being handed out.
func TestControlRefusedDuringMove(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.moveReady(moveSID)
	queued := e.create(moveSID)
	mv, err := e.s.CreateMove(ctx, moveSID, "bluebox", "machine:tower")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{api.MoveRequested, api.MovePacking, api.MoveUploaded, api.MoveUnpacking} {
		e.s.db.Exec("UPDATE moves SET state = ? WHERE id = ?", state, mv.ID)
		_, _, err := e.s.CreateControl(ctx, moveSID, api.RequestedByDashboard)
		if !errors.Is(err, ErrNotControllable) || !strings.Contains(err.Error(), "a move is open for this session") {
			t.Errorf("CreateControl while %s: %v, want 409", state, err)
		}
	}
	if c, ok, err := e.s.ClaimControl(ctx, e.tower.ID); err != nil || (ok && c.Request.ID == queued.ID) {
		t.Fatalf("the queued request was handed out: %+v %v %v", c, ok, err)
	}
	if x := e.get(moveSID); x.RemoteControl == nil || x.RemoteControl.State != api.ControlFailed ||
		x.RemoteControl.Detail != "a move is open for this session" {
		t.Errorf("queued request: %+v", x.RemoteControl)
	}
	if evs := e.events(moveSID, api.KindRemoteControlResult); len(evs) != 1 {
		t.Errorf("result events: %+v", evs)
	}
	// Once the move ended, Remote Control works again.
	e.s.db.Exec("UPDATE moves SET state = ? WHERE id = ?", api.MoveFailed, mv.ID)
	e.create(moveSID)
}
