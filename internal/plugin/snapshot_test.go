package plugin

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/move"
	"github.com/abdallah/session-hub/internal/store"
)

// newTestSnapper is tower's snapper for e, with a stat over e's ~/.claude.
func newTestSnapper(t *testing.T, e *moveEnv) *snapper {
	t.Helper()
	stat := func(id string) (int64, time.Time, bool) {
		ts, err := move.FindTranscripts(e.claude, id)
		if err != nil || len(ts) != 1 {
			return 0, time.Time{}, false
		}
		fi, err := os.Stat(ts[0])
		if err != nil {
			return 0, time.Time{}, false
		}
		return fi.Size(), fi.ModTime(), true
	}
	newClient := func() (*client.Client, error) { return e.sessionhub.client(t, e.sessionhub.tower), nil }
	return newSnapper("tower", e.claude, []string{e.base}, e.sessionhub.tower.key, newClient, stat, log.New(e.logs, "snap ", 0))
}

// snapshotOf returns the session's snapshot for bluebox, or fails.
func snapshotOf(t *testing.T, e *moveEnv) api.Snapshot {
	t.Helper()
	s, err := e.sessionhub.st.GetSession(context.Background(), moveSID)
	if err != nil || len(s.Snapshots) != 1 || s.Snapshots[0].Machine != "bluebox" {
		t.Fatalf("snapshots %+v %v (logs:\n%s)", s.Snapshots, err, e.logs)
	}
	return s.Snapshots[0]
}

// TestSnapshotMoveEndToEnd: tower snapshots its session, goes quiet, and
// the session moves to bluebox from the snapshot with its uncommitted change
// and a commit tower never pushed. When tower is back, its finish ends the
// Claude left in its pane and archives the transcript.
func TestSnapshotMoveEndToEnd(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	writeFile(t, filepath.Join(e.work, "c.txt"), "local only\n", 0o644)
	gitT(t, e.work, "add", "c.txt")
	gitT(t, e.work, "commit", "-qm", "not pushed", "--", "c.txt")
	writeFile(t, filepath.Join(e.work, "a.txt"), "one\nunstaged\n", 0o644)

	sn := newTestSnapper(t, e)
	cands := []snapCand{{moveSID, e.work}}
	sn.run(ctx, cands)
	first := snapshotOf(t, e)
	// Nothing changed: no new snapshot.
	sn.run(ctx, cands)
	if again := snapshotOf(t, e); !again.TakenAt.Equal(first.TakenAt) {
		t.Errorf("re-snapshotted an unchanged session")
	}
	if prompts, _ := e.pane.snapshot(); len(prompts) != 0 {
		t.Errorf("a snapshot typed into the pane: %v", prompts)
	}

	// Tower goes quiet.
	e.sessionhub.skew.Store(int64(store.ControlPollWindow + time.Minute))
	if err := e.sessionhub.st.RecordPoll(ctx, e.sessionhub.bluebox.id); err != nil {
		t.Fatal(err)
	}
	mv, err := e.sessionhub.st.CreateMove(ctx, moveSID, "bluebox", "web:phone")
	if err != nil || mv.State != api.MoveUploaded {
		t.Fatalf("move: %+v %v", mv, err)
	}
	tt := newTargetEnv(t, e)
	in := e.sessionhub.claim(t, e.sessionhub.bluebox)
	tt.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.bluebox), in)
	if got := e.sessionhub.move(t, mv.ID); got.State != api.MoveDone {
		t.Fatalf("move %+v (logs:\n%s)", got, e.logs)
	}
	if got, want := gitT(t, tt.clone, "rev-parse", "HEAD"), gitT(t, e.work, "rev-parse", "HEAD"); got != want {
		t.Errorf("clone at %s, want the unpushed %s", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(tt.clone, "a.txt")); string(b) != "one\nunstaged\n" {
		t.Errorf("a.txt on bluebox: %q", b)
	}

	// Tower is back, with Claude still in its pane.
	if err := e.sessionhub.st.RecordPoll(ctx, e.sessionhub.tower.id); err != nil {
		t.Fatal(err)
	}
	fin := e.sessionhub.claim(t, e.sessionhub.tower)
	if fin.Move.SealID == "" {
		t.Fatalf("finish claim without a seal ID: %+v", fin.Move)
	}
	e.mover.handle(ctx, e.sessionhub.client(t, e.sessionhub.tower), fin)
	if prompts, _ := e.pane.snapshot(); !slices.Contains(prompts, "/exit") {
		t.Errorf("tower's leftover Claude was not ended: %v", prompts)
	}
	if ts, _ := move.FindTranscripts(e.claude, moveSID); len(ts) != 0 {
		t.Errorf("tower can still resume it: %v", ts)
	}
}

// A snapshot taken less than snapshotEvery ago is not taken again, even
// after the transcript changed; the next one waits for the gap.
func TestSnapshotRateLimit(t *testing.T) {
	e := newMoveEnv(t, "idle")
	ctx := context.Background()
	sn := newTestSnapper(t, e)
	now := time.Now()
	sn.now = func() time.Time { return now }
	cands := []snapCand{{moveSID, e.work}}
	sn.run(ctx, cands)
	first := snapshotOf(t, e)
	tp := filepath.Join(e.claude, "projects", "-src-app", moveSID+".jsonl")
	f, _ := os.OpenFile(tp, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"assistant"}` + "\n")
	f.Close()
	now = now.Add(time.Minute)
	sn.run(ctx, cands)
	if snapshotOf(t, e).TakenAt != first.TakenAt {
		t.Error("snapshotted again within snapshotEvery")
	}
	now = now.Add(snapshotEvery)
	sn.run(ctx, cands)
	if snapshotOf(t, e).TakenAt.Equal(first.TakenAt) {
		t.Error("no snapshot after snapshotEvery with a changed transcript")
	}
}
