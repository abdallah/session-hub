package move

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude writes an executable script named claude and returns its path.
func fakeClaude(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	writeFile(t, p, "#!/bin/sh\n"+script+"\n", 0o755)
	return p
}

func TestPushBranch(t *testing.T) {
	ctx := context.Background()
	origin, work := newRepo(t)
	r, err := ReadRepo(ctx, work, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := PushBranch(ctx, r); err != nil {
		t.Fatalf("up to date: %v", err)
	}
	writeFile(t, filepath.Join(work, "a.txt"), "two\n", 0o644)
	gitT(t, work, "commit", "-qam", "ahead")
	if err := PushBranch(ctx, r); err != nil {
		t.Fatal(err)
	}
	if got, want := gitT(t, origin, "rev-parse", "main"), gitT(t, work, "rev-parse", "HEAD"); got != want {
		t.Errorf("origin main %s, local %s", got, want)
	}
	// A branch without an upstream is pushed and gets one.
	gitT(t, work, "checkout", "-q", "-b", "feature/x")
	writeFile(t, filepath.Join(work, "f.txt"), "f\n", 0o644)
	gitT(t, work, "add", "f.txt")
	gitT(t, work, "commit", "-qm", "feature")
	r.Branch = "feature/x"
	if err := PushBranch(ctx, r); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, origin, "rev-parse", "feature/x"); got != gitT(t, work, "rev-parse", "HEAD") {
		t.Errorf("origin feature/x = %s", got)
	}
	if up := gitT(t, work, "rev-parse", "--abbrev-ref", "@{u}"); up != "origin/feature/x" {
		t.Errorf("upstream %q", up)
	}
	// A push that fails is an error that names it; the branch is untouched.
	writeFile(t, filepath.Join(work, "g.txt"), "g\n", 0o644)
	gitT(t, work, "add", "g.txt")
	gitT(t, work, "commit", "-qm", "more")
	head := gitT(t, work, "rev-parse", "HEAD")
	gitT(t, work, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	if err := PushBranch(ctx, r); err == nil || !strings.Contains(err.Error(), "git push") {
		t.Errorf("push to a missing remote: %v", err)
	}
	if gitT(t, work, "rev-parse", "HEAD") != head {
		t.Error("a failed push moved HEAD")
	}
}

// A branch that tracks another remote, or a local branch, is pushed to
// origin, and the other remote and the tracking config stay as they were.
func TestPushBranchOtherUpstream(t *testing.T) {
	ctx := context.Background()
	origin, work := newRepo(t)
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	gitT(t, work, "init", "-q", "--bare", "-b", "main", upstream)
	gitT(t, work, "remote", "add", "upstream", upstream)
	gitT(t, work, "push", "-q", "upstream", "main")
	gitT(t, work, "fetch", "-q", "upstream")
	gitT(t, work, "branch", "-q", "--set-upstream-to=upstream/main", "main")
	upHead := gitT(t, upstream, "rev-parse", "main")
	writeFile(t, filepath.Join(work, "a.txt"), "two\n", 0o644)
	gitT(t, work, "commit", "-qam", "ahead of both")
	r, err := ReadRepo(ctx, work, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := PushBranch(ctx, r); err != nil {
		t.Fatal(err)
	}
	if got, want := gitT(t, origin, "rev-parse", "main"), gitT(t, work, "rev-parse", "HEAD"); got != want {
		t.Errorf("origin main %s, local %s", got, want)
	}
	if got := gitT(t, upstream, "rev-parse", "main"); got != upHead {
		t.Errorf("upstream main moved to %s", got)
	}
	if got := gitT(t, work, "config", "--get", "branch.main.remote"); got != "upstream" {
		t.Errorf("branch.main.remote = %q", got)
	}

	// A local branch (remote ".") goes to origin under its own name.
	gitT(t, work, "checkout", "-q", "--track", "-b", "topic", "main")
	writeFile(t, filepath.Join(work, "t.txt"), "t\n", 0o644)
	gitT(t, work, "add", "t.txt")
	gitT(t, work, "commit", "-qm", "topic")
	originMain := gitT(t, origin, "rev-parse", "main")
	r.Branch = "topic"
	if err := PushBranch(ctx, r); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, origin, "rev-parse", "topic"); got != gitT(t, work, "rev-parse", "HEAD") {
		t.Errorf("origin topic = %s", got)
	}
	if got := gitT(t, origin, "rev-parse", "main"); got != originMain {
		t.Errorf("origin main moved to %s", got)
	}
	if got := gitT(t, work, "config", "--get", "branch.topic.remote"); got != "." {
		t.Errorf("branch.topic.remote = %q", got)
	}
}

// A partial failure leaves the transcript in place, so a retry finds the
// session and moves the rest.
func TestArchivePartialFailureRetries(t *testing.T) {
	claude := newClaudeDir(t, testSessionID)
	state := t.TempDir()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	id := "mv_CCCCCCCCCCCCCCCCCCCCCC"
	// A non-empty folder where the sidecar goes makes its rename fail.
	blocker := filepath.Join(state, "moved", id, "transcript", "-home-user-proj", testSessionID)
	writeFile(t, filepath.Join(blocker, "x"), "x", 0o600)
	if err := Archive(claude, state, testSessionID, id, now); err == nil {
		t.Fatal("archive into a blocked folder succeeded")
	}
	if ts, _ := FindTranscripts(claude, testSessionID); len(ts) != 1 {
		t.Fatalf("after a partial failure the transcript is gone: %v", ts)
	}
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	if err := Archive(claude, state, testSessionID, id, now); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if ts, _ := FindTranscripts(claude, testSessionID); len(ts) != 0 {
		t.Errorf("transcript still resumable: %v", ts)
	}
	for _, p := range []string{filepath.Join(claude, "projects", "-home-user-proj", testSessionID), filepath.Join(claude, "file-history", testSessionID)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "moved", id, "transcript", "-home-user-proj", testSessionID, "subagents", "agent-1.jsonl")); err != nil {
		t.Errorf("sidecar not archived: %v", err)
	}
}

func TestClaudeVersion(t *testing.T) {
	ctx := context.Background()
	v, err := ClaudeVersion(ctx, fakeClaude(t, `[ "$1" = --version ] && echo "2.1.285 (Claude Code)"`))
	if err != nil || v != "2.1.285" {
		t.Errorf("version %q %v", v, err)
	}
	if _, err := ClaudeVersion(ctx, fakeClaude(t, "exit 3")); err == nil {
		t.Error("a failing claude gave a version")
	}
	if _, err := ClaudeVersion(ctx, fakeClaude(t, "true")); err == nil {
		t.Error("an empty version was accepted")
	}
	if _, err := ClaudeVersion(ctx, filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("a missing claude gave a version")
	}
}

func TestArchive(t *testing.T) {
	claude := newClaudeDir(t, testSessionID)
	state := t.TempDir()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	old := filepath.Join(state, "moved", "mv_old")
	writeFile(t, filepath.Join(old, "transcript", "x.jsonl"), "x", 0o600)
	recent := filepath.Join(state, "moved", "mv_recent")
	writeFile(t, filepath.Join(recent, "x"), "x", 0o600)
	// Every age counts from the fixed now, never from the clock.
	for p, age := range map[string]time.Duration{old: 31 * 24 * time.Hour, recent: 24 * time.Hour} {
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}

	if err := Archive(claude, state, testSessionID, "mv_AAAAAAAAAAAAAAAAAAAAAA", now); err != nil {
		t.Fatal(err)
	}
	if ts, _ := FindTranscripts(claude, testSessionID); len(ts) != 0 {
		t.Errorf("transcript still resumable: %v", ts)
	}
	dest := filepath.Join(state, "moved", "mv_AAAAAAAAAAAAAAAAAAAAAA")
	for _, p := range []string{
		"transcript/-home-user-proj/" + testSessionID + ".jsonl",
		"transcript/-home-user-proj/" + testSessionID + "/subagents/agent-1.jsonl",
		"file-history/" + testSessionID + "/abc@v1",
	} {
		if _, err := os.Stat(filepath.Join(dest, p)); err != nil {
			t.Errorf("archive lacks %s: %v", p, err)
		}
	}
	for _, p := range []string{filepath.Join(claude, "projects", "-home-user-proj", testSessionID), filepath.Join(claude, "file-history", testSessionID)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", p, err)
		}
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("a 31-day-old archive was kept: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("a recent archive was removed: %v", err)
	}
	// Archiving a session with no transcript left is not an error.
	if err := Archive(claude, state, testSessionID, "mv_BBBBBBBBBBBBBBBBBBBBBB", now); err != nil {
		t.Errorf("second archive: %v", err)
	}
}
