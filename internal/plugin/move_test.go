package plugin

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/move"
	"github.com/abdallah/session-hub/internal/store"
)

func TestMoveOutEndsSessionBeforeUpload(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nchanged\n", 0o644)
	writeFile(t, filepath.Join(e.work, "b.txt"), "b\n", 0o644)
	gitT(t, e.work, "add", "b.txt")
	gitT(t, e.work, "commit", "-qm", "local commit")
	writeFile(t, filepath.Join(e.work, "new.txt"), "untracked\n", 0o644)

	claim := e.startMove(t, "bluebox")
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)

	if got := e.order.list(); !slices.Equal(got, []string{"exit", "upload"}) {
		t.Fatalf("order %v, want exit then upload (logs:\n%s)", got, e.logs)
	}
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveUploaded || mv.BundleSize == 0 {
		t.Fatalf("move %+v", mv)
	}
	if got, want := gitT(t, e.origin, "rev-parse", "main"), gitT(t, e.work, "rev-parse", "HEAD"); got != want {
		t.Errorf("the local commit was not pushed: origin %s, local %s", got, want)
	}
	sealed, err := os.ReadFile(filepath.Join(e.sessionhub.moveDir, mv.ID+".sealed"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := move.Open(e.sessionhub.bluebox.key, e.sessionhub.tower.key.PublicKey(), mv.ID, sealed)
	if err != nil {
		t.Fatalf("bluebox cannot open the bundle: %v", err)
	}
	b, err := move.Extract(plain)
	if err != nil {
		t.Fatal(err)
	}
	m := b.Manifest
	if m.SessionID != moveSID || m.MoveID != mv.ID || m.SourceMachine != "tower" || m.Branch != "main" ||
		m.Head != gitT(t, e.work, "rev-parse", "HEAD") || m.ClaudeVersion != "2.1.285" || m.RootRel != "app" ||
		!slices.Equal(m.Untracked, []string{"new.txt"}) || !strings.Contains(string(b.Patch), "+changed") {
		t.Errorf("manifest %+v patch %q", m, b.Patch)
	}
	// The transcript stays until the move is done.
	if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 1 {
		t.Errorf("transcripts after upload: %v", ts)
	}
	if prompts, starts := e.pane.snapshot(); !slices.Equal(prompts, []string{"/exit"}) || len(starts) != 0 {
		t.Errorf("prompts %q starts %v", prompts, starts)
	}
}

func TestMoveOutRefusesBusyPane(t *testing.T) {
	for _, status := range []string{"working", "blocked", ""} {
		t.Run(fmt.Sprintf("status %q", status), func(t *testing.T) {
			e := newMoveEnv(t, status)
			claim := e.startMove(t, "bluebox")
			e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
			mv := e.sessionhub.move(t, claim.Move.ID)
			if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "check pane: ") {
				t.Errorf("move %+v", mv)
			}
			if prompts, _ := e.pane.snapshot(); len(prompts) != 0 {
				t.Errorf("typed into a busy pane: %q", prompts)
			}
			if len(e.order.list()) != 0 {
				t.Errorf("steps ran: %v", e.order.list())
			}
		})
	}
}

// Every check that can fail while the session runs comes before /exit, and
// the pane is checked again right before it.
func TestMoveOutChecksBeforeExit(t *testing.T) {
	for _, c := range []struct {
		name, prefix string
		setup        func(t *testing.T, e *moveEnv)
	}{
		{"no transcript", "build bundle: ", func(t *testing.T, e *moveEnv) { os.RemoveAll(filepath.Join(e.claude, "projects")) }},
		{"too big", "build bundle: untracked files over", func(t *testing.T, e *moveEnv) {
			writeFile(t, filepath.Join(e.work, "huge.bin"), strings.Repeat("x", 10<<20+1), 0o644)
		}},
		{"busy again after the push", "check pane: the agent is working", func(t *testing.T, e *moveEnv) {
			e.pane.mu.Lock()
			e.pane.busyAfter = 1
			e.pane.mu.Unlock()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newMoveEnv(t, "idle")
			c.setup(t, e)
			claim := e.startMove(t, "bluebox")
			e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
			if mv := e.sessionhub.move(t, claim.Move.ID); mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, c.prefix) ||
				strings.HasSuffix(mv.Detail, detailResumes) {
				t.Errorf("move %+v", mv)
			}
			if prompts, starts := e.pane.snapshot(); len(prompts) != 0 || len(starts) != 0 {
				t.Errorf("prompts %q starts %v", prompts, starts)
			}
		})
	}
}

func TestMoveOutRestartsAfterFailure(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	// The sessionhub turns the upload away and still shows the move packing: the
	// bundle did not land, so the move fails and the session restarts here.
	e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/bundle") {
			badGateway(w)
			return true
		}
		return false
	})
	claim := e.startMove(t, "bluebox")
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "upload: ") || !strings.HasSuffix(mv.Detail, detailResumes) {
		t.Fatalf("move %+v", mv)
	}
	if got := e.order.list(); !slices.Equal(got, []string{"exit", "upload", "start"}) {
		t.Errorf("order %v", got)
	}
	if _, starts := e.pane.snapshot(); len(starts) != 1 || !slices.Equal(starts[0], []string{"--resume", moveSID}) {
		t.Errorf("starts %v", starts)
	}
	if _, ok, _ := e.sessionhub.st.ClaimMove(ctx, e.sessionhub.tower.id); ok {
		t.Error("a source that failed its own step got a finish")
	}
}

// The source restarts the session only once the sessionhub recorded the failure:
// an upload whose outcome is unknown, or a failure the sessionhub did not take,
// leaves the session ended for the move's finish.
func TestMoveOutRestartsOnlyAfterRecordedFailure(t *testing.T) {
	for _, c := range []struct {
		name  string
		block func(r *http.Request, afterUpload bool) bool
	}{
		{"upload outcome unknown", func(r *http.Request, afterUpload bool) bool {
			// The move reads fine before /exit and can't be read after the
			// upload.
			return r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/bundle") ||
				r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/moves/") && afterUpload
		}},
		{"failure not recorded", func(r *http.Request, _ bool) bool {
			return r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/bundle") ||
				r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/result")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newMoveEnv(t, "idle")
			var afterUpload atomic.Bool
			e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
				if !isFrom(r, e.sessionhub.tower) {
					return false
				}
				if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/bundle") {
					afterUpload.Store(true)
				}
				if c.block(r, afterUpload.Load()) {
					badGateway(w)
					return true
				}
				return false
			})
			claim := e.startMove(t, "bluebox")
			e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
			if mv := e.sessionhub.move(t, claim.Move.ID); mv.State != api.MovePacking {
				t.Errorf("move %+v, want it left packing for its timeout", mv)
			}
			if got := e.order.list(); !slices.Equal(got, []string{"exit", "upload"}) {
				t.Errorf("order %v: the session restarted without a recorded failure (logs:\n%s)", got, e.logs)
			}
		})
	}
}

// The sessionhub records the source's failure, but the answer is lost (a tunnel
// 502 after the commit): the source reads the move back, sees its own
// failure, and restarts the session, so it does not stay ended nowhere.
func TestMoveOutRestartsAfterLostFailureAnswer(t *testing.T) {
	for _, c := range []struct {
		name  string
		drops int32 // result answers lost after the commit
	}{{"retry lands", 1}, {"every try lost", 1000}} {
		t.Run(c.name, func(t *testing.T) {
			e := newMoveEnv(t, "idle")
			ctx := context.Background()
			var posts atomic.Int32
			e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
				if !isFrom(r, e.sessionhub.tower) {
					return false
				}
				if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/bundle") {
					badGateway(w)
					return true
				}
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/result") && posts.Add(1) <= c.drops {
					var in api.MoveResultIn
					json.NewDecoder(r.Body).Decode(&in)
					id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/moves/"), "/result")
					e.sessionhub.st.FinishMove(ctx, e.sessionhub.tower.id, id, in)
					badGateway(w)
					return true
				}
				return false
			})
			claim := e.startMove(t, "bluebox")
			e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
			mv := e.sessionhub.move(t, claim.Move.ID)
			if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "upload: ") || !strings.HasSuffix(mv.Detail, detailResumes) ||
				strings.Count(mv.Detail, "upload: ") != 1 {
				t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
			}
			if _, starts := e.pane.snapshot(); len(starts) != 1 || !slices.Equal(starts[0], []string{"--resume", moveSID}) {
				t.Errorf("starts %v (logs:\n%s)", starts, e.logs)
			}
		})
	}
}

// Claude that herdr shows with no session ID after /exit has not left: the
// step fails, nothing is uploaded, and nothing starts a second copy.
func TestMoveOutExitLeavesClaudeWithoutID(t *testing.T) {
	e := newMoveEnv(t, "idle")
	e.pane.mu.Lock()
	e.pane.exitDropsID = true
	e.pane.mu.Unlock()
	e.mover.exitWait = 50 * time.Millisecond
	claim := e.startMove(t, "bluebox")
	e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "end session: ") || !strings.HasSuffix(mv.Detail, detailResumes) {
		t.Errorf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if got := e.order.list(); !slices.Equal(got, []string{"exit"}) {
		t.Errorf("order %v, want only exit", got)
	}
}

// A move that timed out during a slow push does not end the session.
func TestMoveOutSkipsExitWhenMoveEnded(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	var id string
	e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/moves/"+id && isFrom(r, e.sessionhub.tower) {
			if _, err := e.sessionhub.st.FinishMove(ctx, e.sessionhub.tower.id, id, api.MoveResultIn{State: api.MoveFailed, Detail: "packing: timed out"}); err != nil {
				t.Errorf("end the move: %v", err)
			}
		}
		return false
	})
	claim := e.startMove(t, "bluebox")
	id = claim.Move.ID
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
	if mv := e.sessionhub.move(t, id); mv.State != api.MoveFailed || mv.Detail != "packing: timed out" {
		t.Errorf("move %+v", mv)
	}
	if prompts, starts := e.pane.snapshot(); len(prompts) != 0 || len(starts) != 0 {
		t.Errorf("prompts %q starts %v", prompts, starts)
	}
	if len(e.order.list()) != 0 {
		t.Errorf("steps ran: %v", e.order.list())
	}
}

// A remote that is not on GitHub is named without its credentials.
func TestMoveOutCloudDetailHidesCredentials(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	if _, err := e.sessionhub.st.UpsertSession(ctx, e.sessionhub.tower.id, api.SessionUpsert{ID: moveSID, Agent: "claude",
		Source: api.SourcePlugin, CWD: e.work, GitRepo: "https://github.com/example/app", GitBranch: "main", HerdrSession: "default",
		HerdrWorkspace: "w1", HerdrPane: "w1:p1", AgentState: "idle"}); err != nil {
		t.Fatal(err)
	}
	gitT(t, e.work, "remote", "set-url", "origin", "https://user:s3cret@gitlab.example.test/example/app.git")
	claim := e.startMove(t, api.MoveCloud)
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "cloud: ") || strings.Contains(mv.Detail, "s3cret") ||
		!strings.Contains(mv.Detail, "gitlab.example.test/example/app") {
		t.Errorf("move %+v", mv)
	}
	if strings.Contains(e.logs.String(), "s3cret") {
		t.Errorf("logs carry the credential:\n%s", e.logs)
	}
}

// The archive note keeps its hint when the error is long.
func TestFinishArchiveNoteKeepsHint(t *testing.T) {
	long := errors.New(strings.Repeat("x", 500))
	n := archiveNote("tower", long)
	if !strings.HasPrefix(n, "archive on tower failed: x") || !strings.HasSuffix(n, archiveNoteHint) ||
		utf8.RuneCountInString(n) > api.MaxMoveDetailRunes {
		t.Errorf("note %q (%d runes)", n, utf8.RuneCountInString(n))
	}
}

func TestMoveOutExitTimesOut(t *testing.T) {
	e := newMoveEnv(t, "idle")
	e.pane.exits = false
	e.mover.exitWait = 50 * time.Millisecond
	claim := e.startMove(t, "bluebox")
	e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "end session: ") || !strings.HasSuffix(mv.Detail, detailResumes) {
		t.Errorf("move %+v", mv)
	}
	if slices.Contains(e.order.list(), "upload") || slices.Contains(e.order.list(), "start") {
		t.Errorf("order %v", e.order.list())
	}
}

func TestFinishDoneArchives(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	claim := e.startMove(t, "bluebox")
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
	e.sessionhub.claim(t, e.sessionhub.bluebox) // the target's move-in
	if _, err := e.sessionhub.st.FinishMove(ctx, e.sessionhub.bluebox.id, claim.Move.ID, api.MoveResultIn{State: api.MoveDone}); err != nil {
		t.Fatal(err)
	}
	fin := e.sessionhub.claim(t, e.sessionhub.tower)
	if fin.Request.Action != api.ActionMoveOut || fin.Move.State != api.MoveDone {
		t.Fatalf("finish claim %+v", fin.Move)
	}
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 0 {
		t.Errorf("still resumable on the source: %v", ts)
	}
	if _, err := os.Stat(filepath.Join(e.state, "moved", claim.Move.ID, "transcript", "-src-app", moveSID+".jsonl")); err != nil {
		t.Errorf("archive: %v", err)
	}
	if _, starts := e.pane.snapshot(); len(starts) != 0 {
		t.Errorf("a done move restarted the session: %v", starts)
	}
}

func TestFinishFailedRestarts(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	claim := e.startMove(t, "bluebox")
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
	e.sessionhub.claim(t, e.sessionhub.bluebox)
	if _, err := e.sessionhub.st.FinishMove(ctx, e.sessionhub.bluebox.id, claim.Move.ID, api.MoveResultIn{State: api.MoveFailed, Detail: "find clone: none"}); err != nil {
		t.Fatal(err)
	}
	// A new mover stands for a watcher that restarted since the upload.
	m2 := newMover(e.herdr.Path, e.claude, e.state, nil, e.sessionhub.tower.key, log.New(e.logs, "", 0))
	fastMover(m2)
	fin := e.sessionhub.claim(t, e.sessionhub.tower)
	m2.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	if _, starts := e.pane.snapshot(); len(starts) != 1 || !slices.Equal(starts[0], []string{"--resume", moveSID}) {
		t.Fatalf("starts %v (logs:\n%s)", starts, e.logs)
	}
	if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 1 {
		t.Errorf("a failed move archived the transcript: %v", ts)
	}
	// The session already runs again: a second finish starts nothing.
	m2.finish(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	if _, starts := e.pane.snapshot(); len(starts) != 1 {
		t.Errorf("a running session was started again: %v", starts)
	}
}

// An archive that keeps failing is retried, then shows in the move's detail.
func TestFinishArchiveFailureIsNoted(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	claim := e.startMove(t, "bluebox")
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
	e.sessionhub.claim(t, e.sessionhub.bluebox)
	if _, err := e.sessionhub.st.FinishMove(ctx, e.sessionhub.bluebox.id, claim.Move.ID, api.MoveResultIn{State: api.MoveDone}); err != nil {
		t.Fatal(err)
	}
	// moved is a file, so every try fails.
	writeFile(t, filepath.Join(e.state, "moved"), "not a folder", 0o600)
	fin := e.sessionhub.claim(t, e.sessionhub.tower)
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveDone || !strings.HasPrefix(mv.Detail, "archive on tower failed: ") ||
		!strings.HasSuffix(mv.Detail, "remove the transcript there by hand") {
		t.Errorf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if n := strings.Count(e.logs.String(), "archive session "+moveSID); n != 1 {
		t.Errorf("logged the archive failure %d times", n)
	}
	if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 1 {
		t.Errorf("transcripts %v", ts)
	}
}

func TestControllerWithoutMoverFailsTheStep(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctl := newController(func() (*client.Client, error) { return e.sessionhub.client(t, e.sessionhub.tower), nil }, nil, log.New(e.logs, "", 0))
	claim := e.startMove(t, "bluebox")
	ctl.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
	if mv := e.sessionhub.move(t, claim.Move.ID); mv.State != api.MoveFailed || mv.Detail != "check pane: "+detailNoMover {
		t.Errorf("move %+v", mv)
	}
}

func TestWatcherRegistersMoveKey(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.w.moveKey = "Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyMDA="
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	e.w.step(ctx, t0)
	e.w.step(ctx, t0.Add(heartbeatInterval))
	if n := e.sessionhub.count(http.MethodPut, "/v1/machines/self/move-key"); n != 1 {
		t.Fatalf("key sent %d times in the first hour, want 1", n)
	}
	e.w.step(ctx, t0.Add(moveKeyEvery+heartbeatInterval))
	if n := e.sessionhub.count(http.MethodPut, "/v1/machines/self/move-key"); n != 2 {
		t.Errorf("key sent %d times after an hour, want 2", n)
	}
	// Without a key the watcher never sends one.
	e2 := newTestEnv(t)
	e2.w.step(ctx, t0)
	if n := e2.sessionhub.count(http.MethodPut, "/v1/machines/self/move-key"); n != 0 {
		t.Errorf("sent a key it does not have: %d", n)
	}
}

func TestMoveInResumesOnTarget(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nunstaged\n", 0o644)
	writeFile(t, filepath.Join(e.work, "staged.txt"), "staged\n", 0o644)
	gitT(t, e.work, "add", "staged.txt")
	writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
	writeFile(t, filepath.Join(e.work, ".env"), "SECRET=1\n", 0o600)
	tt := newTargetEnv(t, e)

	in := e.packed(t)
	if in.Request.Action != api.ActionMoveIn {
		t.Fatalf("bluebox got %+v", in.Request)
	}
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	mv := e.sessionhub.move(t, in.Move.ID)
	if mv.State != api.MoveDone || mv.Detail != "not carried: .env" {
		t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if s, _ := e.sessionhub.st.GetSession(ctx, moveSID); s.Machine != "bluebox" {
		t.Errorf("owner %s", s.Machine)
	}
	if got, want := gitT(t, tt.clone, "rev-parse", "HEAD"), gitT(t, e.work, "rev-parse", "HEAD"); got != want ||
		gitT(t, tt.clone, "symbolic-ref", "--short", "HEAD") != "main" {
		t.Errorf("clone at %s, want %s on main", got, want)
	}
	for _, f := range []string{"a.txt", "staged.txt", "notes.md"} {
		want, _ := os.ReadFile(filepath.Join(e.work, f))
		got, _ := os.ReadFile(filepath.Join(tt.clone, f))
		if string(got) != string(want) {
			t.Errorf("%s on bluebox: %q, want %q", f, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(tt.clone, ".env")); !os.IsNotExist(err) {
		t.Errorf(".env was carried: %v", err)
	}
	proj, _ := move.ProjectDir(tt.claude, tt.clone)
	if b, err := os.ReadFile(filepath.Join(proj, moveSID+".jsonl")); err != nil || !strings.Contains(string(b), `"message":"hi"`) {
		t.Errorf("transcript on bluebox: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(tt.claude, "file-history", moveSID, "h@v1")); err != nil {
		t.Errorf("file history on bluebox: %v", err)
	}
	if tt.herdr.cwd != tt.clone || !slices.Equal(tt.herdr.args, []string{"--resume", moveSID}) {
		t.Errorf("started in %q with %q", tt.herdr.cwd, tt.herdr.args)
	}
	// The source's finish archives its copy.
	fin := e.sessionhub.claim(t, e.sessionhub.tower)
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 0 {
		t.Errorf("the source can still resume it: %v", ts)
	}
}

func TestMoveInFailures(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, e *moveEnv, tt *targetEnv)
		prefix string
		has    string
	}{
		// The detail is "find clone: no clone of <origin path> on bluebox";
		// TestFindClone pins the middle.
		{"no clone", func(t *testing.T, e *moveEnv, tt *targetEnv) { os.RemoveAll(tt.clone) },
			"find clone: no clone of ", " on bluebox"},
		{"dirty clone", func(t *testing.T, e *moveEnv, tt *targetEnv) {
			writeFile(t, filepath.Join(tt.clone, "a.txt"), "local edit\n", 0o644)
		}, "check clone: ", "uncommitted changes"},
		{"transcript exists", func(t *testing.T, e *moveEnv, tt *targetEnv) {
			writeFile(t, filepath.Join(tt.claude, "projects", "-old", moveSID+".jsonl"), "{}\n", 0o600)
		}, "transcript: ", "already has a transcript"},
		{"other Claude Code version", func(t *testing.T, e *moveEnv, tt *targetEnv) {
			p := filepath.Join(t.TempDir(), "claude")
			writeFile(t, p, "#!/bin/sh\necho '2.1.999 (Claude Code)'\n", 0o755)
			tt.mover.claudeBin = p
		}, "claude version: ", "2.1.999 here, 2.1.285 on tower"},
		{"wrong sender", func(t *testing.T, e *moveEnv, tt *targetEnv) {
			// The server now hands bluebox a different key for tower.
			k, _ := ecdh.X25519().GenerateKey(rand.Reader)
			e.sessionhub.st.SetMoveKey(context.Background(), e.sessionhub.tower.id, move.PublicKeyString(k))
		}, "open: ", "did not open"},
		{"patch creates a file bluebox has", func(t *testing.T, e *moveEnv, tt *targetEnv) {
			writeFile(t, filepath.Join(tt.clone, "made.txt"), "bluebox's own\n", 0o644)
		}, "check clone: ", "made.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newMoveEnv(t, "idle")
			ctx := context.Background()
			writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
			writeFile(t, filepath.Join(e.work, "made.txt"), "staged new file\n", 0o644)
			gitT(t, e.work, "add", "made.txt")
			tt := newTargetEnv(t, e)
			claim := e.startMove(t, "bluebox")
			e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
			c.setup(t, e, tt)
			before := cloneState(t, tt.clone)
			in := e.sessionhub.claim(t, e.sessionhub.bluebox)
			tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
			mv := e.sessionhub.move(t, claim.Move.ID)
			if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, c.prefix) || !strings.Contains(mv.Detail, c.has) {
				t.Fatalf("move %+v, want %q ... %q (logs:\n%s)", mv, c.prefix, c.has, e.logs)
			}
			if after := cloneState(t, tt.clone); after != before {
				t.Errorf("the clone changed:\nbefore %s\nafter  %s", before, after)
			}
			if started, _ := tt.herdr.state(); started {
				t.Error("bluebox started Claude")
			}
			// The source gets the failure and restarts the session.
			fin := e.sessionhub.claim(t, e.sessionhub.tower)
			e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
			if _, starts := e.pane.snapshot(); len(starts) != 1 {
				t.Errorf("source restarts: %v", starts)
			}
		})
	}
}

// cloneState is the clone's branch, HEAD, status, and file list, or "none".
func cloneState(t *testing.T, clone string) string {
	t.Helper()
	if _, err := os.Stat(clone); err != nil {
		return "none"
	}
	var files []string
	filepath.WalkDir(clone, func(p string, d os.DirEntry, err error) error {
		if d != nil && d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d != nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			files = append(files, strings.TrimPrefix(p, clone)+"="+string(b))
		}
		return nil
	})
	return gitT(t, clone, "symbolic-ref", "--short", "HEAD") + " " + gitT(t, clone, "rev-parse", "HEAD") + " " +
		gitT(t, clone, "status", "--porcelain") + " " + strings.Join(files, ",")
}

func TestMoveInUndoAfterApplyFailure(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nchanged\n", 0o644)
	writeFile(t, filepath.Join(e.work, "made.txt"), "staged new file\n", 0o644)
	gitT(t, e.work, "add", "made.txt")
	writeFile(t, filepath.Join(e.work, "notes", "new.md"), "untracked\n", 0o644)
	tt := newTargetEnv(t, e)
	gitT(t, tt.clone, "checkout", "-q", "-b", "other")
	// bluebox's notes is a link to a folder outside the clone, so writing
	// notes/new.md fails after the patch went in. bluebox's own file is not the
	// move's and must survive the undo.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(tt.clone, "notes")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tt.clone, "mine.txt"), "bluebox's own\n", 0o644)
	in := e.packed(t)
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	mv := e.sessionhub.move(t, in.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "apply: ") || !strings.Contains(mv.Detail, "symbolic link") ||
		!strings.HasSuffix(mv.Detail, "; the clone is back on other") {
		t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if br := gitT(t, tt.clone, "symbolic-ref", "--short", "HEAD"); br != "other" {
		t.Errorf("clone on %s", br)
	}
	if b, _ := os.ReadFile(filepath.Join(tt.clone, "mine.txt")); string(b) != "bluebox's own\n" {
		t.Errorf("bluebox's own file changed: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(tt.clone, "a.txt")); string(b) != "one\n" {
		t.Errorf("a.txt: %q", b)
	}
	if _, err := os.Stat(filepath.Join(tt.clone, "made.txt")); !os.IsNotExist(err) {
		t.Errorf("made.txt left behind: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("wrote through the link: %v", entries)
	}
	if st := gitT(t, tt.clone, "status", "--porcelain"); st != "?? mine.txt\n?? notes" {
		t.Errorf("status:\n%s", st)
	}
}

func TestMoveInUndoAfterWriteFailure(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nchanged\n", 0o644)
	writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
	tt := newTargetEnv(t, e)
	// projects is a file, so the transcript cannot be written.
	writeFile(t, filepath.Join(tt.claude, "projects"), "not a folder", 0o600)
	in := e.packed(t)
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	mv := e.sessionhub.move(t, in.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "write transcript: ") {
		t.Fatalf("move %+v", mv)
	}
	if st := gitT(t, tt.clone, "status", "--porcelain"); st != "" {
		t.Errorf("the clone kept changes:\n%s", st)
	}
	if _, err := os.Stat(filepath.Join(tt.clone, "notes.md")); !os.IsNotExist(err) {
		t.Errorf("notes.md left behind: %v", err)
	}
}

// The move ends (here: fails) while bluebox unpacks. bluebox reads it back before it
// starts Claude, starts nothing, and puts everything back; the source gets
// the session.
func TestMoveInStopsWhenMoveEnded(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nchanged\n", 0o644)
	writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
	tt := newTargetEnv(t, e)
	in := e.packed(t)
	before := cloneState(t, tt.clone)
	e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/moves/"+in.Move.ID && isFrom(r, e.sessionhub.bluebox) {
			e.sessionhub.st.FinishMove(context.Background(), e.sessionhub.bluebox.id, in.Move.ID,
				api.MoveResultIn{State: api.MoveFailed, Detail: "unpacking: timed out"})
		}
		return false
	})
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	if started, prompts := tt.herdr.state(); started || len(prompts) != 0 {
		t.Errorf("bluebox started Claude for a move that ended: %v %q", started, prompts)
	}
	if ts, _ := move.FindTranscripts(tt.claude, moveSID); len(ts) != 0 {
		t.Errorf("transcript left on bluebox: %v", ts)
	}
	if after := cloneState(t, tt.clone); after != before {
		t.Errorf("the clone changed:\nbefore %s\nafter  %s", before, after)
	}
	if mv := e.sessionhub.move(t, in.Move.ID); mv.State != api.MoveFailed || mv.Detail != "unpacking: timed out" {
		t.Errorf("move %+v", mv)
	}
	fin := e.sessionhub.claim(t, e.sessionhub.tower)
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	if _, starts := e.pane.snapshot(); len(starts) != 1 {
		t.Errorf("source restarts: %v", starts)
	}
}

// bluebox started Claude, but the sessionhub never records its done: bluebox ends Claude
// again and puts everything back, so the session never runs on both
// machines.
func TestMoveInStopsWhenDoneNotRecorded(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nchanged\n", 0o644)
	writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
	tt := newTargetEnv(t, e)
	in := e.packed(t)
	before := cloneState(t, tt.clone)
	e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/result") && isFrom(r, e.sessionhub.bluebox) {
			badGateway(w)
			return true
		}
		return false
	})
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	if started, prompts := tt.herdr.state(); started || !slices.Equal(prompts, []string{"/exit"}) {
		t.Errorf("Claude on bluebox: running %v, prompts %q (logs:\n%s)", started, prompts, e.logs)
	}
	if closed, _ := tt.herdr.paneClosed(); !closed {
		t.Error("bluebox did not close the pane it started Claude in")
	}
	if ts, _ := move.FindTranscripts(tt.claude, moveSID); len(ts) != 0 {
		t.Errorf("transcript left on bluebox: %v", ts)
	}
	if after := cloneState(t, tt.clone); after != before {
		t.Errorf("the clone changed:\nbefore %s\nafter  %s", before, after)
	}
	if mv := e.sessionhub.move(t, in.Move.ID); mv.State != api.MoveUnpacking {
		t.Errorf("move %+v", mv)
	}
}

// Claude was started on bluebox but herdr never detected it, and the pane
// can't be closed: Claude may run on bluebox, so the move ends done there with a
// note, bluebox keeps the transcript and the clone, and the source archives
// instead of restarting.
func TestMoveInStartFailsAndCloseFails(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nchanged\n", 0o644)
	writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
	tt := newTargetEnv(t, e)
	tt.herdr.noDetect, tt.herdr.exitFails, tt.herdr.closeFails = true, true, true
	tt.mover.start.PollFor = 100 * time.Millisecond
	in := e.packed(t)
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	mv := e.sessionhub.move(t, in.Move.ID)
	if mv.State != api.MoveDone || !strings.HasPrefix(mv.Detail, "start: ") ||
		!strings.HasSuffix(mv.Detail, "; Claude may be running in pane w5:p1 on bluebox; check it there") {
		t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if s, _ := e.sessionhub.st.GetSession(ctx, moveSID); s.Machine != "bluebox" {
		t.Errorf("owner %s", s.Machine)
	}
	if ts, _ := move.FindTranscripts(tt.claude, moveSID); len(ts) != 1 {
		t.Errorf("bluebox's transcript: %v", ts)
	}
	if b, _ := os.ReadFile(filepath.Join(tt.clone, "notes.md")); string(b) != "untracked\n" {
		t.Errorf("notes.md on bluebox: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(tt.clone, "a.txt")); string(b) != "one\nchanged\n" {
		t.Errorf("a.txt on bluebox: %q", b)
	}
	fin := e.sessionhub.claim(t, e.sessionhub.tower)
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	if _, starts := e.pane.snapshot(); len(starts) != 0 {
		t.Errorf("source restarted: %v", starts)
	}
	if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 0 {
		t.Errorf("the source can still resume it: %v", ts)
	}
}

// bluebox started Claude, the sessionhub does not record done at first, and neither
// /exit nor closing the pane works: bluebox keeps the files and posts done again
// until the sessionhub records it.
func TestMoveInKeepsDoneWhenStopFails(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
	tt := newTargetEnv(t, e)
	tt.herdr.exitFails, tt.herdr.closeFails = true, true
	tt.mover.exitWait = 100 * time.Millisecond
	tt.mover.doneFor = 10 * time.Second
	in := e.packed(t)
	var posts atomic.Int32
	e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/result") && isFrom(r, e.sessionhub.bluebox) && posts.Add(1) <= 8 {
			badGateway(w)
			return true
		}
		return false
	})
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	if mv := e.sessionhub.move(t, in.Move.ID); mv.State != api.MoveDone {
		t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if n := posts.Load(); n != 9 {
		t.Errorf("%d done posts, want 9", n)
	}
	if started, prompts := tt.herdr.state(); !started || !slices.Equal(prompts, []string{"/exit"}) {
		t.Errorf("Claude on bluebox: running %v, prompts %q", started, prompts)
	}
	if ts, _ := move.FindTranscripts(tt.claude, moveSID); len(ts) != 1 {
		t.Errorf("bluebox's transcript: %v", ts)
	}
	if _, err := os.Stat(filepath.Join(tt.clone, "notes.md")); err != nil {
		t.Errorf("notes.md on bluebox: %v", err)
	}
}

// Claude was started on bluebox but herdr never detected it. "No agent" in that
// pane proves nothing, so bluebox closes the pane, and only once herdr no longer
// has it does bluebox take its files back and fail the move; the source then
// restarts the session.
func TestMoveInStartFailsClosesPane(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nchanged\n", 0o644)
	writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
	tt := newTargetEnv(t, e)
	tt.herdr.noDetect = true
	tt.mover.start.PollFor = 100 * time.Millisecond
	in := e.packed(t)
	before := cloneState(t, tt.clone)
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	mv := e.sessionhub.move(t, in.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "start: ") || !strings.HasSuffix(mv.Detail, "; the clone is back on main") {
		t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if closed, n := tt.herdr.paneClosed(); !closed || n != 1 {
		t.Errorf("pane closed %v after %d closes", closed, n)
	}
	if ts, _ := move.FindTranscripts(tt.claude, moveSID); len(ts) != 0 {
		t.Errorf("transcript left on bluebox: %v", ts)
	}
	if after := cloneState(t, tt.clone); after != before {
		t.Errorf("the clone changed:\nbefore %s\nafter  %s", before, after)
	}
	fin := e.sessionhub.claim(t, e.sessionhub.tower)
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	if _, starts := e.pane.snapshot(); len(starts) != 1 {
		t.Errorf("source restarts: %v", starts)
	}
}

// endSession never counts an agentless pane as "Claude left" when Claude was
// never seen there: herdr may not have detected it yet.
func TestEndSessionNeedsClaudeSeen(t *testing.T) {
	e := newMoveEnv(t, "idle")
	tt := newTargetEnv(t, e)
	tt.herdr.noDetect = true
	tt.herdr.started = true
	tt.mover.exitWait = 50 * time.Millisecond
	if err := tt.mover.endSession(context.Background(), "w5:p1", "", false); err == nil {
		t.Error("an agentless pane where Claude was never seen counted as ended")
	}
	if err := tt.mover.endSession(context.Background(), "w5:p1", "", true); err != nil {
		t.Errorf("Claude seen, then gone: %v", err)
	}
}

// Another agent on bluebox works in the clone: the move must not switch the
// clone's branch under it. On the same branch, or with the agent elsewhere,
// the move goes ahead.
func TestMoveInRefusesCloneInUse(t *testing.T) {
	for _, c := range []struct {
		name    string
		branch  string // the clone's branch on bluebox before the move
		dir     func(tt *targetEnv) string
		refused bool
	}{
		{"other branch, agent in the clone", "other", func(tt *targetEnv) string { return filepath.Join(tt.clone, "sub") }, true},
		{"same branch, agent in the clone", "main", func(tt *targetEnv) string { return tt.clone }, false},
		{"other branch, agent elsewhere", "other", func(tt *targetEnv) string { return tt.root }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newMoveEnv(t, "idle")
			ctx := context.Background()
			writeFile(t, filepath.Join(e.work, "notes.md"), "untracked\n", 0o644)
			tt := newTargetEnv(t, e)
			if err := os.MkdirAll(filepath.Join(tt.clone, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			if c.branch != "main" {
				gitT(t, tt.clone, "checkout", "-q", "-b", c.branch)
			}
			tt.herdr.others = []map[string]any{{"pane_id": "w9:p1", "workspace_id": "w9", "agent": "claude", "agent_status": "working",
				"cwd": c.dir(tt), "agent_session": map[string]any{"agent": "claude", "kind": "id", "source": "herdr:claude",
					"value": "0a0a0a0a-1111-4222-8333-444455556666"}}}
			before := cloneState(t, tt.clone)
			in := e.packed(t)
			tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
			mv := e.sessionhub.move(t, in.Move.ID)
			if !c.refused {
				if mv.State != api.MoveDone {
					t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
				}
				return
			}
			if want := "check clone: " + tt.clone + " is in use by a session in pane w9:p1"; mv.State != api.MoveFailed || mv.Detail != want {
				t.Fatalf("move %+v, want failed with %q (logs:\n%s)", mv, want, e.logs)
			}
			if after := cloneState(t, tt.clone); after != before {
				t.Errorf("the clone changed:\nbefore %s\nafter  %s", before, after)
			}
			if started, _ := tt.herdr.state(); started {
				t.Error("bluebox started Claude")
			}
			if ts, _ := move.FindTranscripts(tt.claude, moveSID); len(ts) != 0 {
				t.Errorf("transcript on bluebox: %v", ts)
			}
		})
	}
}

// The session's directory is a linked folder on bluebox that leads out of the
// clone: bluebox starts nothing there and puts the clone back.
func TestMoveInRefusesLinkedSessionDir(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	if err := os.Mkdir(filepath.Join(e.work, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.sessionhub.st.UpsertSession(ctx, e.sessionhub.tower.id, api.SessionUpsert{ID: moveSID, Agent: "claude",
		Source: api.SourcePlugin, CWD: filepath.Join(e.work, "sub"), GitRepo: e.origin, GitBranch: "main", HerdrSession: "default",
		HerdrWorkspace: "w1", HerdrPane: "w1:p1", AgentState: "idle"}); err != nil {
		t.Fatal(err)
	}
	tt := newTargetEnv(t, e)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(tt.clone, "sub")); err != nil {
		t.Fatal(err)
	}
	before := cloneState(t, tt.clone)
	in := e.packed(t)
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	mv := e.sessionhub.move(t, in.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "unpack: sub leaves the clone") {
		t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if started, _ := tt.herdr.state(); started {
		t.Error("bluebox started Claude")
	}
	if ts, _ := move.FindTranscripts(tt.claude, moveSID); len(ts) != 0 {
		t.Errorf("transcript left on bluebox: %v", ts)
	}
	if after := cloneState(t, tt.clone); after != before {
		t.Errorf("the clone changed:\nbefore %s\nafter  %s", before, after)
	}
}

func TestMoveDoneForIsStepTTL(t *testing.T) {
	if moveDoneFor != store.MoveStepTTL {
		t.Errorf("moveDoneFor %s, store.MoveStepTTL %s", moveDoneFor, store.MoveStepTTL)
	}
}

const cloudLink = "https://claude.ai/code/session_01CloudCloudCloudCloud1"

// cloudClaude is a fake claude whose --cloud writes its prompt to
// $SESSIONHUB_TEST_PROMPT and prints a link, or fails when $SESSIONHUB_TEST_CLOUD_FAIL
// is set.
const cloudClaude = `case "$1" in
--version) echo "2.1.285 (Claude Code)";;
--cloud)
  printf '%s' "$2" > "$SESSIONHUB_TEST_PROMPT"
  if [ -n "$SESSIONHUB_TEST_CLOUD_FAIL" ]; then echo "error: $SESSIONHUB_TEST_CLOUD_FAIL" >&2; exit 1; fi
  echo "Creating a cloud session..."
  echo "View it at ` + cloudLink + `"
  exec sleep 30;;
esac`

func TestCloudMove(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	fakeClaude(t, cloudClaude)
	prompt := filepath.Join(t.TempDir(), "prompt")
	t.Setenv("SESSIONHUB_TEST_PROMPT", prompt)
	e.onGitHub(t)
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nwip\n", 0o644)
	before := gitT(t, e.work, "status", "--porcelain")

	claim := e.startMove(t, api.MoveCloud)
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveDone || mv.CloudURL != cloudLink {
		t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if s, _ := e.sessionhub.st.GetSession(ctx, moveSID); s.Status != api.StatusEnded {
		t.Errorf("session %s after a cloud move", s.Status)
	}
	branch := "sessionhub/cloud-5e5e5e5e"
	if got := gitT(t, e.origin, "show", branch+":a.txt"); got != "one\nwip" {
		t.Errorf("cloud branch a.txt: %q", got)
	}
	if after := gitT(t, e.work, "status", "--porcelain"); after != before || gitT(t, e.work, "symbolic-ref", "--short", "HEAD") != "main" {
		t.Errorf("the local tree changed: %q -> %q", before, after)
	}
	p, _ := os.ReadFile(prompt)
	if !strings.HasPrefix(string(p), "Continue work moved from a local Claude Code session (5e5e5e5e on tower).\nBranch: "+branch+".\n") {
		t.Errorf("prompt %q", p)
	}
	if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 1 {
		t.Errorf("a cloud move must keep the local transcript: %v", ts)
	}
	if _, ok, _ := e.sessionhub.st.ClaimMove(ctx, e.sessionhub.tower.id); ok {
		t.Error("a cloud move got a finish")
	}
}

func TestCloudMoveRefusesNonGitHub(t *testing.T) {
	e := newMoveEnv(t, "idle")
	// The sessionhub thinks the remote is on GitHub; the clone says otherwise.
	if _, err := e.sessionhub.st.UpsertSession(context.Background(), e.sessionhub.tower.id, api.SessionUpsert{ID: moveSID,
		Source: api.SourcePlugin, GitRepo: "https://github.com/o/app.git"}); err != nil {
		t.Fatal(err)
	}
	claim := e.startMove(t, api.MoveCloud)
	e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
	if mv := e.sessionhub.move(t, claim.Move.ID); mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "cloud: ") ||
		!strings.Contains(mv.Detail, "not on GitHub") {
		t.Errorf("move %+v", mv)
	}
	if prompts, _ := e.pane.snapshot(); len(prompts) != 0 {
		t.Errorf("ended the session anyway: %q", prompts)
	}
}

func TestCloudMoveFailureRestarts(t *testing.T) {
	e := newMoveEnv(t, "idle")
	fakeClaude(t, cloudClaude)
	t.Setenv("SESSIONHUB_TEST_PROMPT", filepath.Join(t.TempDir(), "prompt"))
	t.Setenv("SESSIONHUB_TEST_CLOUD_FAIL", "cloud sessions are not enabled for this organization")
	e.onGitHub(t)
	claim := e.startMove(t, api.MoveCloud)
	e.mover.handle(context.Background(), e.sessionhub.client(t, e.sessionhub.tower), claim)
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "start cloud: ") ||
		!strings.Contains(mv.Detail, "not enabled") || !strings.HasSuffix(mv.Detail, detailResumes) {
		t.Errorf("move %+v", mv)
	}
	if _, starts := e.pane.snapshot(); len(starts) != 1 {
		t.Errorf("starts %v", starts)
	}
}

// The sessionhub does not record the cloud done: the source keeps posting it until
// doneFor, marks the cloud session, and the finish of the timed-out move
// adds a note instead of restarting the session here.
func TestCloudMoveDoneNotRecorded(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusConflict} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			e := newMoveEnv(t, "idle")
			ctx := context.Background()
			fakeClaude(t, cloudClaude)
			t.Setenv("SESSIONHUB_TEST_PROMPT", filepath.Join(t.TempDir(), "prompt"))
			e.onGitHub(t)
			writeFile(t, filepath.Join(e.work, "a.txt"), "one\nwip\n", 0o644)
			e.mover.doneFor = 200 * time.Millisecond
			var posts atomic.Int32
			e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/result") && isFrom(r, e.sessionhub.tower) {
					posts.Add(1)
					http.Error(w, `{"error":"not now"}`, status)
					return true
				}
				return false
			})
			claim := e.startMove(t, api.MoveCloud)
			e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
			if n := posts.Load(); n < 2 {
				t.Errorf("%d done posts, want retries", n)
			}
			if mv := e.sessionhub.move(t, claim.Move.ID); mv.State != api.MovePacking {
				t.Fatalf("move %+v (logs:\n%s)", mv, e.logs)
			}
			mark, err := os.ReadFile(filepath.Join(e.state, "moved", claim.Move.ID, "cloud"))
			if err != nil || string(mark) != cloudLink+"\n" {
				t.Errorf("marker %q %v", mark, err)
			}
			if strings.Contains(e.logs.String(), "until the move ends") {
				t.Errorf("misleading log:\n%s", e.logs)
			}

			e.sessionhub.setIntercept(nil)
			e.sessionhub.skew.Store(int64(11 * time.Minute))
			if _, err := e.sessionhub.st.ExpireMoves(ctx); err != nil {
				t.Fatal(err)
			}
			e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), e.sessionhub.claim(t, e.sessionhub.tower))
			mv := e.sessionhub.move(t, claim.Move.ID)
			if want := "packing: timed out; cloud session started (" + cloudLink + "); not restarted here"; mv.State != api.MoveFailed || mv.Detail != want {
				t.Errorf("move %+v, want detail %q (logs:\n%s)", mv, want, e.logs)
			}
			if _, starts := e.pane.snapshot(); len(starts) != 0 {
				t.Errorf("restarted here: %v", starts)
			}
		})
	}
}

// claude --cloud prints a claude.ai link sessionhub does not trust: a cloud
// session may exist, so the move fails without a restart here.
func TestCloudMoveUntrustedLink(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	fakeClaude(t, `case "$1" in
--version) echo "2.1.285 (Claude Code)";;
--cloud) echo "View it at https://claude.ai/code/elsewhere/01x";;
esac`)
	e.onGitHub(t)
	claim := e.startMove(t, api.MoveCloud)
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
	mv := e.sessionhub.move(t, claim.Move.ID)
	if mv.State != api.MoveFailed || !strings.HasPrefix(mv.Detail, "start cloud: ") ||
		!strings.Contains(mv.Detail, "a cloud session may exist") || strings.HasSuffix(mv.Detail, detailResumes) || mv.CloudURL != "" {
		t.Errorf("move %+v (logs:\n%s)", mv, e.logs)
	}
	if mark, err := os.ReadFile(filepath.Join(e.state, "moved", claim.Move.ID, "cloud")); err != nil || string(mark) != "unknown\n" {
		t.Errorf("marker %q %v", mark, err)
	}
	if prompts, starts := e.pane.snapshot(); !slices.Equal(prompts, []string{"/exit"}) || len(starts) != 0 {
		t.Errorf("prompts %q starts %v", prompts, starts)
	}
	if _, ok, _ := e.sessionhub.st.ClaimMove(ctx, e.sessionhub.tower.id); ok {
		t.Error("the source's own failure got a finish")
	}
}

// A cloud move that timed out during the pushes does not end the session
// or start a cloud session.
func TestCloudMoveSkipsExitWhenMoveEnded(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	fakeClaude(t, cloudClaude)
	prompt := filepath.Join(t.TempDir(), "prompt")
	t.Setenv("SESSIONHUB_TEST_PROMPT", prompt)
	e.onGitHub(t)
	var id string
	e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/moves/"+id && isFrom(r, e.sessionhub.tower) {
			if _, err := e.sessionhub.st.FinishMove(ctx, e.sessionhub.tower.id, id, api.MoveResultIn{State: api.MoveFailed, Detail: "packing: timed out"}); err != nil {
				t.Errorf("end the move: %v", err)
			}
		}
		return false
	})
	claim := e.startMove(t, api.MoveCloud)
	id = claim.Move.ID
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), claim)
	if mv := e.sessionhub.move(t, id); mv.State != api.MoveFailed || mv.Detail != "packing: timed out" {
		t.Errorf("move %+v", mv)
	}
	if prompts, starts := e.pane.snapshot(); len(prompts) != 0 || len(starts) != 0 {
		t.Errorf("prompts %q starts %v", prompts, starts)
	}
	if _, err := os.Stat(prompt); !os.IsNotExist(err) {
		t.Errorf("claude --cloud ran: %v", err)
	}
}

// A move detail or log line never carries a URL's credentials.
func TestCleanErrRedactsCredentials(t *testing.T) {
	got := cleanErr(errors.New("push: git push: fatal: unable to access 'https://user:s3cret@github.com/o/r.git/'\x1b[2J"))
	if strings.Contains(got, "s3cret") || strings.Contains(got, "\x1b") || !strings.Contains(got, "https://github.com/o/r.git/") {
		t.Errorf("cleanErr = %q", got)
	}
}

// A note on a cancelled move (a state no route sets yet, read like failed)
// posts failed, the only ending the result route takes besides done.
func TestFinishNoteOnCancelledMovePostsFailed(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	var states []string
	var mu sync.Mutex
	e.sessionhub.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/result") {
			var in api.MoveResultIn
			json.NewDecoder(r.Body).Decode(&in)
			mu.Lock()
			states = append(states, in.State)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
			return true
		}
		return false
	})
	const id = "mv_AAAAAAAAAAAAAAAAAAAAAA"
	// The restart fails (the directory is gone), so the finish posts a note.
	gone := api.Session{ID: "0b0b0b0b-1111-4222-8333-444455556666", CWD: filepath.Join(t.TempDir(), "gone")}
	e.mover.finish(ctx, e.sessionhub.client(t, e.sessionhub.tower), api.ControlClaim{Session: gone,
		Move: &api.MoveClaim{ID: id, Source: "tower", Target: "bluebox", State: api.MoveCancelled}})
	// A cloud move with a marker posts the marker note.
	if err := e.mover.markCloud(id, ""); err != nil {
		t.Fatal(err)
	}
	e.mover.finish(ctx, e.sessionhub.client(t, e.sessionhub.tower), api.ControlClaim{Session: gone,
		Move: &api.MoveClaim{ID: id, Source: "tower", Target: api.MoveCloud, State: api.MoveCancelled}})
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(states, []string{api.MoveFailed, api.MoveFailed}) {
		t.Errorf("posted states %q (logs:\n%s)", states, e.logs)
	}
}
