package gitinfo

import (
	"context"
	"os/exec"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestLookup(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "feature-x")
	git(t, dir, "remote", "add", "origin", "git@example.com:o/r.git")
	// No commit yet: rev-parse --abbrev-ref HEAD fails on an unborn branch, so
	// make one.
	git(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x")
	repo, branch := Lookup(context.Background(), dir)
	if repo != "git@example.com:o/r.git" || branch != "feature-x" {
		t.Fatalf("got %q, %q", repo, branch)
	}
}

func TestLookupNoOrigin(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x")
	repo, branch := Lookup(context.Background(), dir)
	if repo != "" || branch != "main" {
		t.Fatalf("got %q, %q; want empty repo, main", repo, branch)
	}
}

func TestLookupErrorsGiveEmpty(t *testing.T) {
	for _, cwd := range []string{"", t.TempDir(), "/nonexistent/dir"} {
		if r, b := Lookup(context.Background(), cwd); r != "" || b != "" {
			t.Errorf("cwd %q: got %q, %q", cwd, r, b)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, b := Lookup(ctx, t.TempDir()); r != "" || b != "" {
		t.Errorf("cancelled: got %q, %q", r, b)
	}
}
