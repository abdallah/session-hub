package move

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
)

func TestCloudBranch(t *testing.T) {
	if got := CloudBranch("5E5E5E5E-1111-4222-8333-444455556666"); got != "sessionhub/cloud-5e5e5e5e" {
		t.Errorf("CloudBranch = %q", got)
	}
}

// treeState is everything PushCloudBranch must leave alone: HEAD, the
// branch, the status, the index, and the working tree's files.
func treeState(t *testing.T, work string) string {
	t.Helper()
	index, err := os.ReadFile(filepath.Join(work, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	filepath.WalkDir(work, func(p string, d os.DirEntry, err error) error {
		if d != nil && d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d != nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			files = append(files, strings.TrimPrefix(p, work)+"="+string(b))
		}
		return nil
	})
	return gitT(t, work, "rev-parse", "HEAD") + "|" + gitT(t, work, "symbolic-ref", "HEAD") + "|" +
		gitT(t, work, "status", "--porcelain") + "|" + string(index) + "|" + strings.Join(files, ",") + "|" +
		gitT(t, work, "branch", "--list")
}

func TestPushCloudBranchLeavesTreeAlone(t *testing.T) {
	ctx := context.Background()
	origin, work := dirtyRepo(t)
	r, err := ReadRepo(ctx, work, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A file staged but never committed is new to HEAD: the cloud commit
	// carries it.
	writeFile(t, filepath.Join(work, "staged-new.txt"), "staged new\n", 0o644)
	gitT(t, work, "add", "staged-new.txt")
	before := treeState(t, work)
	branch, skipped, err := PushCloudBranch(ctx, r, testSessionID)
	if err != nil || branch != "sessionhub/cloud-11111111" {
		t.Fatalf("branch %q %v", branch, err)
	}
	slices.Sort(skipped)
	if want := []string{".env.local", "CONF/.ENV", "certs/server.pem", "conf/.env.prod", "deploy.key", "link", "prod.tfvars"}; !slices.Equal(skipped, want) {
		t.Errorf("skipped %q, want %q", skipped, want)
	}
	if after := treeState(t, work); after != before {
		t.Fatalf("the working tree, index, or branch changed:\nbefore %s\nafter  %s", before, after)
	}
	commit := gitT(t, origin, "rev-parse", branch)
	// The push leaves a remote-tracking ref, and only that, in the clone.
	if got := gitT(t, work, "rev-parse", "refs/remotes/origin/"+branch); got != commit {
		t.Errorf("refs/remotes/origin/%s = %s, want %s", branch, got, commit)
	}
	if parent := gitT(t, origin, "rev-parse", commit+"^"); parent != r.Head {
		t.Errorf("parent %s, want HEAD %s", parent, r.Head)
	}
	files := gitT(t, origin, "ls-tree", "-r", "--name-only", commit)
	for _, want := range []string{"a.txt", "bin.dat", "notes/new.md", "run.sh", "staged-new.txt", "sub/keep.txt"} {
		if !strings.Contains("\n"+files+"\n", "\n"+want+"\n") {
			t.Errorf("cloud commit lacks %s:\n%s", want, files)
		}
	}
	for _, bad := range []string{"gone.txt", ".env.local", "certs/server.pem", "deploy.key", "prod.tfvars", "link", "build/out.o"} {
		if strings.Contains("\n"+files+"\n", "\n"+bad+"\n") {
			t.Errorf("cloud commit has %s", bad)
		}
	}
	if got := gitT(t, origin, "show", commit+":a.txt"); got != "one\nstaged\nunstaged" {
		t.Errorf("a.txt in the cloud commit: %q", got)
	}
	// The tracked secret keeps HEAD's content: its change stays local.
	if got := gitT(t, origin, "show", commit+":conf/.env.prod"); got != "secret" {
		t.Errorf("conf/.env.prod in the cloud commit: %q", got)
	}
	if got := gitT(t, origin, "show", commit+":CONF/.ENV"); got != "upper secret" {
		t.Errorf("CONF/.ENV in the cloud commit: %q", got)
	}
	// A retry replaces the branch sessionhub pushed, with a lease on that commit.
	writeFile(t, filepath.Join(work, "notes", "new.md"), "newer\n", 0o644)
	if _, _, err := PushCloudBranch(ctx, r, testSessionID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := gitT(t, origin, "show", branch+":notes/new.md"); got != "newer" {
		t.Errorf("after the retry: %q", got)
	}
	// A cloud session pushed to the branch: the next retry refuses rather
	// than drop its commit.
	cloud := filepath.Join(t.TempDir(), "cloud")
	gitT(t, filepath.Dir(cloud), "clone", "-q", "-b", branch, origin, cloud)
	writeFile(t, filepath.Join(cloud, "cloud.txt"), "from the cloud\n", 0o644)
	gitT(t, cloud, "add", "cloud.txt")
	gitT(t, cloud, "commit", "-qm", "cloud work")
	gitT(t, cloud, "push", "-q")
	cloudTip := gitT(t, origin, "rev-parse", branch)
	writeFile(t, filepath.Join(work, "notes", "new.md"), "newest\n", 0o644)
	if _, _, err := PushCloudBranch(ctx, r, testSessionID); err == nil || !strings.Contains(err.Error(), "did not push") {
		t.Errorf("over a cloud session's commit: %v", err)
	}
	if got := gitT(t, origin, "rev-parse", branch); got != cloudTip {
		t.Errorf("the cloud session's commit was dropped: %s, want %s", got, cloudTip)
	}
}

func TestPushCloudBranchCleanTree(t *testing.T) {
	ctx := context.Background()
	origin, work := newRepo(t)
	r, _ := ReadRepo(ctx, work, nil)
	branch, skipped, err := PushCloudBranch(ctx, r, testSessionID)
	if err != nil || branch != "main" || len(skipped) != 0 {
		t.Errorf("clean tree: %q %q %v", branch, skipped, err)
	}
	if out := gitT(t, origin, "branch", "--list", "sessionhub/*"); out != "" {
		t.Errorf("a clean tree pushed %q", out)
	}
}

func TestHandOffPrompt(t *testing.T) {
	s := api.Session{ID: "5e5e5e5e-1111-4222-8333-444455556666", Recap: "Fixed the\nlogin bug.", LastPrompt: "run the tests",
		LatestReport: &api.Report{Done: []string{"fix", "tests"}, InFlight: []string{"docs"}, WaitingOn: []string{}}}
	want := "Continue work moved from a local Claude Code session (5e5e5e5e on bluebox).\n" +
		"Branch: sessionhub/cloud-5e5e5e5e.\n" +
		"Recap: Fixed the login bug.\n" +
		"Last request: run the tests\n" +
		"Status: done: fix; tests / in flight: docs"
	if got := HandOffPrompt(s, "bluebox", "sessionhub/cloud-5e5e5e5e"); got != want {
		t.Errorf("prompt:\n%s\nwant:\n%s", got, want)
	}
	long := HandOffPrompt(api.Session{ID: s.ID, Recap: strings.Repeat("r", 5000), LastPrompt: strings.Repeat("p", 5000)}, "bluebox", "main")
	for _, l := range strings.Split(long, "\n")[2:] {
		if _, v, _ := strings.Cut(l, ": "); utf8.RuneCountInString(v) > 2000 {
			t.Errorf("%.20s... kept %d runes", l, utf8.RuneCountInString(v))
		}
	}
	bare := HandOffPrompt(api.Session{ID: s.ID}, "bluebox", "main")
	if bare != "Continue work moved from a local Claude Code session (5e5e5e5e on bluebox).\nBranch: main.\nRecap: none\nLast request: none" {
		t.Errorf("bare prompt:\n%s", bare)
	}
}

func TestStartCloud(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	args := filepath.Join(t.TempDir(), "args")
	const link = "https://claude.ai/code/session_01CloudCloudCloudCloud1"
	ok := fakeClaude(t, `printf '%s\n' "$1" "$2" > `+args+`; pwd >> `+args+`
echo "Creating a cloud session..."
printf '\033[1mView it at `+link+`\033[0m\n'
exec sleep 30`)
	start := time.Now()
	got, err := StartCloud(ctx, ok, dir, "line one\nline two", 10*time.Second)
	if err != nil || got != link {
		t.Fatalf("link %q %v", got, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("StartCloud waited %s after the link", time.Since(start))
	}
	if b, _ := os.ReadFile(args); string(b) != "--cloud\nline one\nline two\n"+dir+"\n" {
		t.Errorf("claude got %q", b)
	}
	atEnd := fakeClaude(t, `printf 'link: `+link+`'`)
	if got, err := StartCloud(ctx, atEnd, dir, "p", 10*time.Second); err != nil || got != link {
		t.Errorf("link at the very end: %q %v", got, err)
	}
	fails := fakeClaude(t, `echo "error: cloud sessions need a claude.ai sign-in" >&2; exit 1`)
	var started *CloudStartedError
	if _, err := StartCloud(ctx, fails, dir, "p", 10*time.Second); err == nil || !strings.Contains(err.Error(), "claude.ai sign-in") ||
		errors.As(err, &started) {
		t.Errorf("failure: %v", err)
	}
	// A session may exist: a claude.ai link sessionhub does not trust, or a clean
	// exit with no link.
	for name, script := range map[string]string{
		"untrusted link":  `echo "View it at https://claude.ai/code/elsewhere/x"; exit 1`,
		"other host":      `echo "View it at https://evil.claude.ai/code/session_01x"`,
		"no link, exit 0": `echo "Created."`,
	} {
		if _, err := StartCloud(ctx, fakeClaude(t, script), dir, "p", 10*time.Second); !errors.As(err, &started) ||
			!strings.Contains(err.Error(), "a cloud session may exist") {
			t.Errorf("%s: %v", name, err)
		}
	}
	silent := fakeClaude(t, `exec sleep 30`)
	if _, err := StartCloud(ctx, silent, dir, "p", 200*time.Millisecond); err == nil || !strings.Contains(err.Error(), "no cloud session link within") {
		t.Errorf("silence: %v", err)
	}
}

// TestStartCloudNeedsTerminal pins the real claude's behavior: --cloud
// refuses without a terminal, and its link carries a query string.
func TestStartCloudNeedsTerminal(t *testing.T) {
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("util-linux script is not installed")
	}
	const link = "https://claude.ai/code/session_013KT8piYH8zGZSJmWP4TEhR"
	bin := fakeClaude(t, `[ -t 1 ] || { echo "Non-interactive invocations run locally and would silently ignore --cloud." >&2; exit 1; }
echo "Created cloud session: x"
echo "View: `+link+`?from=cli&m=0"
echo "Resume with: claude --teleport session_013KT8piYH8zGZSJmWP4TEhR"`)
	got, err := StartCloud(context.Background(), bin, t.TempDir(), "it's \"quoted\" $HOME `x`", 10*time.Second)
	if err != nil || got != link {
		t.Fatalf("link %q %v", got, err)
	}
}
