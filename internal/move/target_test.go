package move

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProjectDir(t *testing.T) {
	claude := t.TempDir()
	for _, c := range []struct{ cwd, want string }{
		{"/home/user/my project", "-home-user-my-project"},
		{"/home/user/Code/OTGS/app.v2", "-home-user-Code-OTGS-app-v2"},
		{"/tmp/café", "-tmp-caf-"},
		{"/tmp/\U0001F600x", "-tmp---x"}, // one rune outside the BMP is two UTF-16 units, so two dashes
	} {
		got, err := ProjectDir(claude, c.cwd)
		if err != nil || got != filepath.Join(claude, "projects", c.want) {
			t.Errorf("ProjectDir(%q) = %q, %v; want projects/%s", c.cwd, got, err, c.want)
		}
	}
	long := "/" + strings.Repeat("d", 250)
	if _, err := ProjectDir(claude, long); err == nil || !strings.Contains(err.Error(), "longer than 200") {
		t.Errorf("long path with no folder: %v", err)
	}
	existing := filepath.Join(claude, "projects", "-"+strings.Repeat("d", 199)+"-abc123")
	os.MkdirAll(existing, 0o700)
	if got, err := ProjectDir(claude, long); err != nil || got != existing {
		t.Errorf("long path with its folder: %q %v", got, err)
	}
}

func TestWriteSession(t *testing.T) {
	claude := t.TempDir()
	cwd := "/home/user/proj"
	b := &Bundle{Manifest: Manifest{SessionID: testSessionID}, Transcript: []byte("t\n"),
		Sidecar: []File{{Name: "subagents/a.jsonl", Data: []byte("s")}}, History: []File{{Name: "x@v1", Data: []byte("h")}}}
	written, err := WriteSession(claude, cwd, b)
	if err != nil || len(written) != 3 {
		t.Fatalf("written %v %v", written, err)
	}
	proj := filepath.Join(claude, "projects", "-home-user-proj")
	for _, p := range []string{filepath.Join(proj, testSessionID+".jsonl"), filepath.Join(proj, testSessionID, "subagents", "a.jsonl"),
		filepath.Join(claude, "file-history", testSessionID, "x@v1")} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", p, fi, err)
		}
	}
	if _, err := WriteSession(claude, "/elsewhere", b); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("second write: %v", err)
	}
	RemoveFiles(written)
	if ts, _ := FindTranscripts(claude, testSessionID); len(ts) != 0 {
		t.Errorf("RemoveFiles left %v", ts)
	}
	if _, err := os.Stat(filepath.Join(proj, testSessionID)); !os.IsNotExist(err) {
		t.Errorf("empty sidecar folder left: %v", err)
	}
}

func TestFindClone(t *testing.T) {
	ctx := context.Background()
	gitIsolate(t)
	rootA, rootB := t.TempDir(), t.TempDir()
	mk := func(dir, remote string) string {
		gitT(t, filepath.Dir(dir), "init", "-q", dir)
		gitT(t, dir, "remote", "add", "origin", remote)
		return dir
	}
	os.MkdirAll(filepath.Join(rootA, "OTGS"), 0o755)
	a := mk(filepath.Join(rootA, "OTGS", "app"), "git@github.com:Org/app.git")
	os.MkdirAll(filepath.Join(rootA, "a", "b", "c", "d"), 0o755)
	mk(filepath.Join(rootA, "a", "b", "c", "d", "deep"), "git@github.com:Org/deep.git") // depth 5: not searched
	mk(filepath.Join(rootA, "other"), "git@github.com:Org/other.git")

	got, err := FindClone(ctx, []string{rootA}, "https://GitHub.com/Org/app", "otgs/app")
	if err != nil || got != a {
		t.Fatalf("one clone: %q %v", got, err)
	}
	if _, err := FindClone(ctx, []string{rootA}, "git@github.com:Org/deep.git", "x"); !errors.Is(err, ErrNoClone) {
		t.Errorf("too deep: %v", err)
	}
	if _, err := FindClone(ctx, []string{rootA}, "git@github.com:Org/none.git", "x"); !errors.Is(err, ErrNoClone) ||
		!strings.Contains(err.Error(), "github.com/Org/none") {
		t.Errorf("none: %v", err)
	}
	// A second clone: the one whose path under its root matches wins, in any
	// case; with no match, the error lists both.
	os.MkdirAll(filepath.Join(rootB, "forks"), 0o755)
	b := mk(filepath.Join(rootB, "forks", "app"), "ssh://git@github.com/Org/app.git")
	if got, err := FindClone(ctx, []string{rootA, rootB}, "git@github.com:Org/app.git", "FORKS/app"); err != nil || got != b {
		t.Errorf("by relative path: %q %v", got, err)
	}
	if _, err := FindClone(ctx, []string{rootA, rootB}, "git@github.com:Org/app.git", "elsewhere/app"); err == nil ||
		!strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
		t.Errorf("ambiguous: %v", err)
	}
	// A missing root is skipped.
	if got, err := FindClone(ctx, []string{filepath.Join(rootA, "nope"), rootA}, "git@github.com:Org/app.git", ""); err != nil || got != a {
		t.Errorf("missing root: %q %v", got, err)
	}
	// Two clones at the same path under their roots: the error lists those two.
	os.MkdirAll(filepath.Join(rootB, "otgs"), 0o755)
	c := mk(filepath.Join(rootB, "otgs", "app"), "https://github.com/Org/app")
	if _, err := FindClone(ctx, []string{rootA, rootB}, "git@github.com:Org/app.git", "OTGS/app"); err == nil ||
		!strings.Contains(err.Error(), "at OTGS/app: ") || !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), c) ||
		strings.Contains(err.Error(), b) {
		t.Errorf("same relative path twice: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := FindClone(cancelled, []string{rootA}, "git@github.com:Org/app.git", ""); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// moved is a clone of origin, as the target has it, on branch other.
func moved(t *testing.T, origin string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "clone")
	gitT(t, filepath.Dir(clone), "clone", "-q", origin, clone)
	gitT(t, clone, "checkout", "-q", "-b", "other")
	return clone
}

func TestPrepareCloneRefusesDirty(t *testing.T) {
	ctx := context.Background()
	origin, _ := newRepo(t)
	clone := moved(t, origin)
	if err := CheckClone(ctx, clone, []string{"notes/new.md"}); err != nil {
		t.Fatalf("clean clone: %v", err)
	}
	writeFile(t, filepath.Join(clone, "a.txt"), "local edit\n", 0o644)
	if err := CheckClone(ctx, clone, nil); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("tracked change: %v", err)
	}
	gitT(t, clone, "checkout", "-q", "--", "a.txt")
	writeFile(t, filepath.Join(clone, "notes", "new.md"), "mine\n", 0o644)
	writeFile(t, filepath.Join(clone, "unrelated.txt"), "fine\n", 0o644)
	if err := CheckClone(ctx, clone, []string{"notes/new.md", "run.sh"}); err == nil || !strings.Contains(err.Error(), "notes/new.md") ||
		strings.Contains(err.Error(), "unrelated") {
		t.Errorf("collision: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(clone, "notes", "new.md")); string(b) != "mine\n" {
		t.Errorf("the check changed a file: %q", b)
	}
}

func TestPatchNewFiles(t *testing.T) {
	_, work := dirtyRepo(t)
	writeFile(t, filepath.Join(work, "new dir", "a b.txt"), "x\n", 0o644)
	writeFile(t, filepath.Join(work, "\u00fc.bin"), "\x00\x01", 0o644)
	writeFile(t, filepath.Join(work, "empty.txt"), "", 0o644)
	gitT(t, work, "add", "new dir/a b.txt", "\u00fc.bin", "empty.txt")
	data, _ := buildFor(t, work, newClaudeDir(t, testSessionID))
	b, err := Extract(data)
	if err != nil {
		t.Fatal(err)
	}
	got := PatchNewFiles(b.Patch)
	slices.Sort(got)
	// a.txt and bin.dat change and gone.txt goes: none of them is new.
	if want := []string{"empty.txt", "new dir/a b.txt", "\u00fc.bin"}; !slices.Equal(got, want) {
		t.Errorf("PatchNewFiles = %q, want %q", got, want)
	}
}

func TestPrepareCloneApplyUndo(t *testing.T) {
	ctx := context.Background()
	origin, work := dirtyRepo(t)
	clone := moved(t, origin) // made before the next push, so PrepareClone must fetch
	oldMain := gitT(t, clone, "rev-parse", "main")
	writeFile(t, filepath.Join(work, "c.txt"), "committed\n", 0o644)
	gitT(t, work, "add", "c.txt")
	gitT(t, work, "commit", "-qm", "ahead", "--", "c.txt") // only c.txt; a.txt stays staged
	gitT(t, work, "push", "-q")
	data, _ := buildFor(t, work, newClaudeDir(t, testSessionID))
	b, err := Extract(data)
	if err != nil {
		t.Fatal(err)
	}
	co, err := PrepareClone(ctx, clone, "main", b.Manifest.Head)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, clone, "rev-parse", "HEAD"); got != b.Manifest.Head || gitT(t, clone, "symbolic-ref", "--short", "HEAD") != "main" {
		t.Fatalf("after checkout: HEAD %s on %s", got, gitT(t, clone, "symbolic-ref", "--short", "HEAD"))
	}
	if err := co.Apply(ctx, b); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.txt", "bin.dat", "c.txt", "notes/new.md", "run.sh"} {
		want, _ := os.ReadFile(filepath.Join(work, f))
		got, _ := os.ReadFile(filepath.Join(clone, f))
		if string(got) != string(want) {
			t.Errorf("%s: %q, want %q", f, got, want)
		}
	}
	if fi, _ := os.Stat(filepath.Join(clone, "run.sh")); fi.Mode().Perm()&0o111 == 0 {
		t.Error("run.sh lost its executable bit")
	}
	if _, err := os.Stat(filepath.Join(clone, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("gone.txt: %v", err)
	}
	// Work that is not the move's, made after Apply: a new file, an edit to
	// a file the patch did not touch, and an edit to a file Apply wrote.
	writeFile(t, filepath.Join(clone, "bystander.txt"), "mine\n", 0o644)
	writeFile(t, filepath.Join(clone, "sub", "keep.txt"), "keep\nmine\n", 0o644)
	writeFile(t, filepath.Join(clone, "notes", "new.md"), "edited here\n", 0o644)
	if err := co.Undo(ctx); err == nil || !strings.Contains(err.Error(), "kept notes/new.md") {
		t.Errorf("undo: %v, want it to keep notes/new.md", err)
	}
	if br := gitT(t, clone, "symbolic-ref", "--short", "HEAD"); br != "other" {
		t.Errorf("after undo on %s, want other", br)
	}
	for f, want := range map[string]string{"bystander.txt": "mine\n", "sub/keep.txt": "keep\nmine\n", "notes/new.md": "edited here\n",
		"a.txt": "one\n"} {
		if got, _ := os.ReadFile(filepath.Join(clone, f)); string(got) != want {
			t.Errorf("%s after undo: %q, want %q", f, got, want)
		}
	}
	for _, f := range []string{"run.sh", "c.txt"} {
		if _, err := os.Stat(filepath.Join(clone, f)); !os.IsNotExist(err) {
			t.Errorf("%s survived the undo: %v", f, err)
		}
	}
	if st := gitT(t, clone, "status", "--porcelain"); st != "M sub/keep.txt\n?? bystander.txt\n?? notes/" {
		t.Errorf("after undo:\n%s", st)
	}
	// main goes back to where it was: the undo takes back its own fast-forward.
	if got := gitT(t, clone, "rev-parse", "main"); got != oldMain {
		t.Errorf("main at %s after undo, want %s", got, oldMain)
	}
}

func TestPrepareCloneUndoDeletesCreatedBranch(t *testing.T) {
	ctx := context.Background()
	origin, work := newRepo(t)
	clone := moved(t, origin)
	gitT(t, work, "checkout", "-q", "-b", "feature/x")
	writeFile(t, filepath.Join(work, "f.txt"), "f\n", 0o644)
	gitT(t, work, "add", "f.txt")
	gitT(t, work, "commit", "-qm", "feature")
	gitT(t, work, "push", "-q", "-u", "origin", "feature/x")
	co, err := PrepareClone(ctx, clone, "feature/x", gitT(t, work, "rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if up := gitT(t, clone, "rev-parse", "--abbrev-ref", "feature/x@{u}"); up != "origin/feature/x" {
		t.Fatalf("tracking %q", up)
	}
	if err := co.Undo(ctx); err != nil {
		t.Fatal(err)
	}
	if br := gitT(t, clone, "symbolic-ref", "--short", "HEAD"); br != "other" {
		t.Errorf("after undo on %s", br)
	}
	if out := gitT(t, clone, "branch", "--list", "feature/x"); out != "" {
		t.Errorf("the tracking branch sessionhub made stayed: %q", out)
	}
	if gitOK(ctx, clone, "config", "--get", "branch.feature/x.merge") {
		t.Error("its tracking config stayed")
	}
	if st := gitT(t, clone, "status", "--porcelain"); st != "" {
		t.Errorf("status:\n%s", st)
	}
}

func TestApplyRefusesLinkedFolder(t *testing.T) {
	ctx := context.Background()
	origin, work := dirtyRepo(t)
	clone := moved(t, origin)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(clone, "notes")); err != nil {
		t.Fatal(err)
	}
	data, _ := buildFor(t, work, newClaudeDir(t, testSessionID))
	b, err := Extract(data)
	if err != nil {
		t.Fatal(err)
	}
	co, err := PrepareClone(ctx, clone, "main", b.Manifest.Head)
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Apply(ctx, b); err == nil || !strings.Contains(err.Error(), "notes is a symbolic link") {
		t.Errorf("apply through a link: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("wrote outside the clone: %v", entries)
	}
	if err := co.Undo(ctx); err != nil {
		t.Fatal(err)
	}
	if st := gitT(t, clone, "status", "--porcelain"); st != "?? notes" {
		t.Errorf("after undo:\n%s", st)
	}
}

func TestPrepareCloneFastForwardOnly(t *testing.T) {
	ctx := context.Background()
	origin, work := newRepo(t)
	clone := moved(t, origin)
	gitT(t, clone, "checkout", "-q", "main")
	writeFile(t, filepath.Join(clone, "local.txt"), "x\n", 0o644)
	gitT(t, clone, "add", "local.txt")
	gitT(t, clone, "commit", "-qm", "only here")
	localMain := gitT(t, clone, "rev-parse", "main")
	gitT(t, clone, "checkout", "-q", "other")
	if _, err := PrepareClone(ctx, clone, "main", gitT(t, work, "rev-parse", "HEAD")); err == nil || !strings.Contains(err.Error(), "fast-forward") {
		t.Errorf("diverged: %v", err)
	}
	if br := gitT(t, clone, "symbolic-ref", "--short", "HEAD"); br != "other" || gitT(t, clone, "rev-parse", "main") != localMain {
		t.Errorf("a refused checkout left the clone on %s, main at %s", br, gitT(t, clone, "rev-parse", "main"))
	}
	if _, err := PrepareClone(ctx, clone, "main", strings.Repeat("ab", 20)); err == nil || !strings.Contains(err.Error(), "not on origin") {
		t.Errorf("unknown commit: %v", err)
	}
}

// A restore that fails after a refused checkout is part of the error. A
// reference-transaction hook refuses to delete the tracking branch
// PrepareClone created.
func TestPrepareCloneReportsFailedRestore(t *testing.T) {
	ctx := context.Background()
	origin, work := newRepo(t)
	clone := moved(t, origin)
	gitT(t, work, "checkout", "-q", "-b", "feature/x")
	writeFile(t, filepath.Join(work, "f.txt"), "f\n", 0o644)
	gitT(t, work, "add", "f.txt")
	gitT(t, work, "commit", "-qm", "feature")
	gitT(t, work, "push", "-q", "-u", "origin", "feature/x")
	// head is on another branch, so feature/x cannot fast-forward to it.
	gitT(t, work, "checkout", "-q", "-b", "side", "main")
	writeFile(t, filepath.Join(work, "s.txt"), "s\n", 0o644)
	gitT(t, work, "add", "s.txt")
	gitT(t, work, "commit", "-qm", "side")
	gitT(t, work, "push", "-q", "-u", "origin", "side")
	head := gitT(t, work, "rev-parse", "HEAD")
	hook := "#!/bin/sh\n[ \"$1\" = prepared ] || exit 0\nif grep -q '^[0-9a-f]* 0* refs/heads/feature/x$'; then exit 1; fi\nexit 0\n"
	writeFile(t, filepath.Join(clone, ".git", "hooks", "reference-transaction"), hook, 0o755)
	_, err := PrepareClone(ctx, clone, "feature/x", head)
	if err == nil || !strings.Contains(err.Error(), "cannot fast-forward") || !strings.Contains(err.Error(), "putting the clone back failed") {
		t.Errorf("PrepareClone: %v", err)
	}
}

func TestCurrentBranch(t *testing.T) {
	ctx := context.Background()
	origin, _ := newRepo(t)
	clone := moved(t, origin)
	if b, err := CurrentBranch(ctx, clone); err != nil || b != "other" {
		t.Errorf("on other: %q %v", b, err)
	}
	gitT(t, clone, "checkout", "-q", "--detach")
	if b, err := CurrentBranch(ctx, clone); err != nil || b != "" {
		t.Errorf("detached: %q %v", b, err)
	}
	if _, err := CurrentBranch(ctx, t.TempDir()); err == nil {
		t.Error("not a repository: no error")
	}
}
