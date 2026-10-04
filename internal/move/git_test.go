package move

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeRemote(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"git@github.com:Owner/Repo.git", "github.com/Owner/Repo"},
		{"https://github.com/Owner/Repo", "github.com/Owner/Repo"},
		{"ssh://git@GitHub.com/Owner/Repo.git/", "github.com/Owner/Repo"},
		{"https://user:secret@github.com/Owner/Repo.git", "github.com/Owner/Repo"},
		{"git@gitlab.example.com:group/sub/proj.git", "gitlab.example.com/group/sub/proj"},
		{"/srv/git/proj.git", "/srv/git/proj"},
		{"  https://github.com/Owner/Repo.git\n", "github.com/Owner/Repo"},
		{"https://github.com:443/Owner/Repo.git", "github.com/Owner/Repo"},
		{"ssh://git@github.com:22/Owner/Repo.git", "github.com/Owner/Repo"},
		{"ssh://git@[::1]:2222/o/r.git", "[::1]/o/r"},
		{"ssh://git@[::1]/o/r.git", "[::1]/o/r"},
	} {
		if got := NormalizeRemote(c.in); got != c.want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for u, want := range map[string]bool{
		"git@github.com:o/r.git":      true,
		"https://GITHUB.COM/o/r":      true,
		"git@gitlab.com:o/r.git":      false,
		"https://github.com.evil/o/r": false,
		"/srv/git/github.com/o/r":     false,
		"https://github.com:443/o/r":  true,
		"ssh://git@github.com:22/o/r": true,
	} {
		if got := IsGitHub(u); got != want {
			t.Errorf("IsGitHub(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestGitEnvBatchMode(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "ssh -i /k")
	env := gitEnv([]string{"GIT_INDEX_FILE=/tmp/i"})
	last := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		last[k] = v
	}
	if last["GIT_SSH_COMMAND"] != "ssh -i /k -o BatchMode=yes" || last["GIT_TERMINAL_PROMPT"] != "0" ||
		last["GIT_INDEX_FILE"] != "/tmp/i" || last["GIT_OPTIONAL_LOCKS"] != "0" {
		t.Errorf("env: ssh %q prompt %q index %q optional locks %q", last["GIT_SSH_COMMAND"], last["GIT_TERMINAL_PROMPT"],
			last["GIT_INDEX_FILE"], last["GIT_OPTIONAL_LOCKS"])
	}
	t.Setenv("GIT_SSH_COMMAND", "")
	for _, kv := range gitEnv(nil) {
		if strings.HasPrefix(kv, "GIT_SSH_COMMAND=") && kv != "GIT_SSH_COMMAND=" && kv != "GIT_SSH_COMMAND=ssh -o BatchMode=yes" {
			t.Errorf("default ssh: %q", kv)
		}
	}
}

func TestDefaultRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := DefaultRoots([]string{"~/src", "/opt/x", "relative", "/opt/x/"})
	want := []string{filepath.Join(home, "Code"), filepath.Join(home, "src"), "/opt/x"}
	if !slices.Equal(got, want) {
		t.Errorf("DefaultRoots = %q, want %q", got, want)
	}
}

func TestReadRepo(t *testing.T) {
	ctx := context.Background()
	_, work := newRepo(t)
	r, err := ReadRepo(ctx, filepath.Join(work, "sub"), []string{filepath.Dir(filepath.Dir(work))})
	if err != nil {
		t.Fatal(err)
	}
	wantRootRel := filepath.Base(filepath.Dir(work)) + "/work"
	if r.Branch != "main" || r.Head != gitT(t, work, "rev-parse", "HEAD") || r.RelPath != "sub" || r.RootRel != wantRootRel ||
		!strings.HasSuffix(r.Remote, "origin.git") {
		t.Errorf("repo %+v; want branch main, rel sub, root rel %q", r, wantRootRel)
	}
	if r, err := ReadRepo(ctx, work, nil); err != nil || r.RelPath != "" || r.RootRel != "work" {
		t.Errorf("at the root: %+v %v", r, err)
	}

	fails := []struct {
		name, want string
		cwd        string
		setup      func()
	}{
		{"relative", "not an absolute path", "work", nil},
		{"missing", "does not exist", filepath.Join(work, "nope"), nil},
		{"not a repo", "not in a Git repository", t.TempDir(), nil},
		{"detached", "detached HEAD", work, func() { gitT(t, work, "checkout", "-q", "--detach") }},
		{"no origin", "no origin remote", work, func() { gitT(t, work, "checkout", "-q", "main"); gitT(t, work, "remote", "remove", "origin") }},
	}
	for _, f := range fails {
		if f.setup != nil {
			f.setup()
		}
		if _, err := ReadRepo(ctx, f.cwd, nil); err == nil || !strings.Contains(err.Error(), f.want) {
			t.Errorf("%s: %v, want %q", f.name, err, f.want)
		}
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatal(err)
	}
}

// gitRaw fails, instead of growing without bound, once the output passes
// maxSealed, and says so rather than calling it a timeout.
func TestGitRawCapsOutput(t *testing.T) {
	_, work := newRepo(t)
	old := maxSealed
	t.Cleanup(func() { maxSealed = old })
	maxSealed = 64
	out, err := gitRaw(context.Background(), work, gitOpts{}, "cat-file", "-p", "HEAD")
	if err == nil || !strings.Contains(err.Error(), "output passes") || len(out) > 64 {
		t.Errorf("capped output: %d bytes, %v", len(out), err)
	}
}

// validBranch agrees with git check-ref-format --branch, but also refuses a
// leading "-" and "@".
func TestValidBranch(t *testing.T) {
	gitIsolate(t)
	dir := t.TempDir()
	for _, b := range []string{"main", "feat/x", "a@b", "v1.2", "é", "a..b", "a b", "x.lock", "a/b.lock/c", "a/", "a.",
		"a//b", ".a", "a/.b", "a@{b", "@{-1}", "HEAD", "a~1", "a^", "a:b", "a?", "a*", "a[b", `a\b`, "a\x01b",
		"a\x7fb", "", "/a"} {
		want := exec.Command("git", "-C", dir, "check-ref-format", "--branch", b).Run() == nil
		if got := validBranch(b); got != want {
			t.Errorf("validBranch(%q) = %v, git says %v", b, got, want)
		}
	}
	for _, b := range []string{"-x", "@"} {
		if validBranch(b) {
			t.Errorf("validBranch(%q) = true", b)
		}
	}
}

func TestRedactURLs(t *testing.T) {
	for in, want := range map[string]string{
		"fatal: unable to access 'https://user:s3cret@github.com/o/r.git/'": "fatal: unable to access 'https://github.com/o/r.git/'",
		"https://tok@h/x and ssh://git@h:22/y":                              "https://h/x and ssh://h:22/y",
		"git@github.com:o/r.git":                                            "git@github.com:o/r.git",
		"https://github.com/o/r and a@b":                                    "https://github.com/o/r and a@b",
	} {
		if got := RedactURLs(in); got != want {
			t.Errorf("RedactURLs(%q) = %q, want %q", in, got, want)
		}
	}
}

// git's message names the URL it was given; the error carries it without
// the credentials.
func TestGitRawRedactsCredentials(t *testing.T) {
	_, work := newRepo(t)
	_, err := gitRaw(context.Background(), work, gitOpts{}, "checkout", "https://user:s3cret@example.test/o/r.git")
	if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "https://example.test/o/r.git") {
		t.Errorf("error %v", err)
	}
}
