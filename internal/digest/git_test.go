package digest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitRepo makes a repo whose commits have fixed dates.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	run := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), append([]string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e"}, env...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run(nil, "init", "-q", "-b", "main")
	for i, when := range []string{"2026-09-30T08:00:00Z", "2026-09-30T10:10:00Z", "2026-09-30T10:20:00Z", "2026-09-30T12:00:00Z"} {
		name := filepath.Join(dir, "f"+string(rune('a'+i)))
		os.WriteFile(name, []byte("x"), 0o600)
		run(nil, "add", ".")
		subj := "commit " + string(rune('a'+i))
		if i == 2 {
			subj += "\twith a tab and " + strings.Repeat("long ", 40)
		}
		run([]string{"GIT_AUTHOR_DATE=" + when, "GIT_COMMITTER_DATE=" + when}, "commit", "-q", "-m", subj)
	}
	os.WriteFile(filepath.Join(dir, "dirty"), []byte("x"), 0o600)
	return dir
}

func TestGit(t *testing.T) {
	dir := gitRepo(t)
	since := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	g := Git(context.Background(), dir, since, until)
	if g == nil {
		t.Fatal("Git = nil")
	}
	if g.CommitCount != 2 || len(g.Commits) != 2 || !strings.HasPrefix(g.Commits[0].Subject, "commit c") ||
		g.Commits[1].Subject != "commit b" {
		t.Errorf("commits = %d %+v, want c then b", g.CommitCount, g.Commits)
	}
	if n := len([]rune(g.Commits[0].Subject)); n > 120 || strings.ContainsAny(g.Commits[0].Subject, "\t\n") {
		t.Errorf("subject not cleaned: %d runes %q", n, g.Commits[0].Subject)
	}
	if g.Uncommitted != 1 || g.Unpushed != -1 {
		t.Errorf("uncommitted=%d unpushed=%d, want 1 and -1", g.Uncommitted, g.Unpushed)
	}

	for name, cwd := range map[string]string{
		"missing directory": filepath.Join(t.TempDir(), "gone"),
		"not a repository":  t.TempDir(),
		"empty":             "",
	} {
		if g := Git(context.Background(), cwd, since, until); g != nil {
			t.Errorf("%s: Git = %+v, want nil", name, g)
		}
	}
	// unborn HEAD: a repository with no commits yet.
	empty := t.TempDir()
	exec.Command("git", "-C", empty, "init", "-q").Run()
	if g := Git(context.Background(), empty, since, until); g != nil {
		t.Errorf("unborn HEAD: Git = %+v, want nil", g)
	}
}

func TestGitIgnoresLinesWithoutSeparator(t *testing.T) {
	dir := gitRepo(t)
	if out, err := exec.Command("git", "-C", dir, "config", "log.showSignature", "true").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v %s", err, out)
	}
	since := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	g := Git(context.Background(), dir, since, until)
	if g == nil || g.CommitCount != 2 {
		t.Fatalf("Git = %+v, want 2 commits", g)
	}
	for _, c := range g.Commits {
		if !shaRE.MatchString(c.SHA) {
			t.Errorf("sha %q is not hex", c.SHA)
		}
	}
	// A stray line, as a signature verifier prints, is skipped and not counted.
	got := parseLog("gpg: Signature made\nabc1234\x00subject\n")
	if got.CommitCount != 1 || len(got.Commits) != 1 || got.Commits[0].SHA != "abc1234" {
		t.Errorf("parseLog = %+v", got)
	}
}
