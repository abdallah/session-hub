package move

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/cli/termtext"
)

// Git time limits: local commands, and push or fetch.
const (
	gitTimeout    = 30 * time.Second
	gitNetTimeout = 2 * time.Minute
)

// gitOpts are the less common parts of a git call. config are -c key=value
// settings for this call.
type gitOpts struct {
	timeout time.Duration
	env     []string
	config  []string
	stdin   io.Reader
}

// gitEnv is the environment of every git call: never a password prompt, SSH
// that fails instead of asking (BatchMode, added to the user's
// GIT_SSH_COMMAND when there is one), and no optional locks, so a read-only
// command does not take the index lock to refresh it. extra comes last, so it
// wins.
func gitEnv(extra []string) []string {
	ssh := os.Getenv("GIT_SSH_COMMAND")
	if ssh == "" {
		ssh = "ssh"
	}
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0",
		"GIT_SSH_COMMAND="+ssh+" -o BatchMode=yes")
	return append(env, extra...)
}

// cappedBuffer keeps up to max bytes. A write past max fails and calls stop,
// which kills the command, so a runaway output can't fill the memory.
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int64
	over bool
	stop func()
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if int64(c.buf.Len())+int64(len(p)) > c.max {
		c.over = true
		c.stop()
		return 0, fmt.Errorf("output passes %s", mib(c.max))
	}
	return c.buf.Write(p)
}

// urlCreds matches the user information of a URL, as in
// https://user:token@host/.
var urlCreds = regexp.MustCompile(`://[^/@\s]+@`)

// RedactURLs removes the user information from every URL in s, so a
// credential in a remote URL never reaches a log or a move detail.
func RedactURLs(s string) string { return urlCreds.ReplaceAllString(s, "://") }

// gitRaw runs git -C dir args and returns its stdout as is. The error names
// the subcommand and carries git's message on one line. Output over
// maxBundle fails the call.
func gitRaw(ctx context.Context, dir string, o gitOpts, args ...string) ([]byte, error) {
	if o.timeout == 0 {
		o.timeout = gitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	full := []string{"-C", dir}
	for _, kv := range o.config {
		full = append(full, "-c", kv)
	}
	cmd := exec.CommandContext(ctx, "git", append(full, args...)...)
	cmd.Env = gitEnv(o.env)
	cmd.Stdin = o.stdin
	cmd.WaitDelay = 5 * time.Second
	out := &cappedBuffer{max: maxBundle, stop: cancel}
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.Join(strings.Fields(errb.String()), " ")
		switch {
		case out.over:
			msg = "output passes " + mib(maxBundle)
		case ctx.Err() != nil:
			msg = "timed out after " + o.timeout.String()
		case msg == "":
			msg = err.Error()
		}
		return out.buf.Bytes(), fmt.Errorf("git %s: %s", args[0], termtext.Clean(RedactURLs(msg), 200))
	}
	return out.buf.Bytes(), nil
}

// git is gitRaw with the default timeout and trimmed output.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitRaw(ctx, dir, gitOpts{}, args...)
	return strings.TrimSpace(string(out)), err
}

// Repo is what a move needs to know about the repository a session works in.
type Repo struct {
	Root    string // the work tree's top level, as git reports it
	RelPath string // the session's directory relative to Root, slash-separated; "" at Root
	RootRel string // Root relative to the search root that holds it, else Root's base name
	Remote  string // origin's URL
	Branch  string
	Head    string // the full SHA of HEAD
}

// ReadRepo reads the repository of cwd. It fails when cwd is not an absolute
// directory in a Git work tree with an origin remote, a branch checked out,
// and at least one commit. roots are the search roots (DefaultRoots) that
// RootRel is relative to.
func ReadRepo(ctx context.Context, cwd string, roots []string) (Repo, error) {
	if !filepath.IsAbs(cwd) {
		return Repo{}, fmt.Errorf("the session's directory %q is not an absolute path", cwd)
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return Repo{}, fmt.Errorf("the session's directory %s does not exist", cwd)
	}
	root, err := git(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return Repo{}, fmt.Errorf("%s is not in a Git repository", cwd)
	}
	r := Repo{Root: root}
	if r.Remote, err = git(ctx, root, "remote", "get-url", "origin"); err != nil || r.Remote == "" {
		return Repo{}, errors.New("the repository has no origin remote")
	}
	if r.Branch, err = git(ctx, root, "symbolic-ref", "--short", "-q", "HEAD"); err != nil || r.Branch == "" {
		return Repo{}, errors.New("the repository is on a detached HEAD; check out a branch first")
	}
	if r.Head, err = git(ctx, root, "rev-parse", "--verify", "-q", "HEAD"); err != nil || r.Head == "" {
		return Repo{}, errors.New("the branch has no commits yet")
	}
	if r.RelPath, err = relUnder(root, cwd); err != nil {
		return Repo{}, err
	}
	r.RootRel = rootRel(root, roots)
	return r, nil
}

// relUnder is dir relative to root, slash-separated, "" for root itself,
// with symbolic links resolved on both. It fails when dir is not under root.
func relUnder(root, dir string) (string, error) {
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	dd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rr, dd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is not under %s", dir, root)
	}
	if rel == "." {
		return "", nil
	}
	return filepath.ToSlash(rel), nil
}

// rootRel is root relative to the first search root that holds it, else its
// base name.
func rootRel(root string, roots []string) string {
	for _, r := range roots {
		if rel, err := relUnder(r, root); err == nil && rel != "" {
			return rel
		}
	}
	return filepath.Base(root)
}

// DefaultRoots are the directories searched for clones: ~/Code, then each
// absolute or ~/ entry of extra (the client config's move_roots), once each.
func DefaultRoots(extra []string) []string {
	home, _ := os.UserHomeDir()
	out := []string{filepath.Join(home, "Code")}
	for _, r := range extra {
		if strings.HasPrefix(r, "~/") {
			r = filepath.Join(home, r[2:])
		}
		if !filepath.IsAbs(r) {
			continue
		}
		if r = filepath.Clean(r); !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// NormalizeRemote is a remote URL without its scheme, user, port, trailing
// "/" and ".git", with the host in lower case, and with git@host:path written as
// host/path, so the same repository compares equal over SSH and HTTPS. A
// local path keeps its form, without a trailing "/" or ".git".
func NormalizeRemote(u string) string {
	s := strings.TrimSpace(u)
	local := false
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if at := strings.IndexByte(s, '@'); at >= 0 && at < strings.IndexByte(s+"/", '/') {
			s = s[at+1:]
		}
		host, rest, _ := strings.Cut(s, "/")
		if c := strings.LastIndexByte(host, ':'); c >= 0 && c > strings.LastIndexByte(host, ']') && allDigits(host[c+1:]) {
			host = host[:c]
		}
		s = host + "/" + rest
	} else if c := strings.IndexByte(s, ':'); c > 0 && !strings.Contains(s[:c], "/") {
		host := s[:c]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		s = host + "/" + strings.TrimPrefix(s[c+1:], "/")
	} else {
		local = true
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	if local {
		return s
	}
	host, path, _ := strings.Cut(s, "/")
	return strings.ToLower(host) + "/" + path
}

// allDigits reports whether s is one or more ASCII digits.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// IsGitHub reports whether remote u is on github.com.
func IsGitHub(u string) bool {
	n := NormalizeRemote(u)
	return strings.HasPrefix(n, "github.com/")
}
