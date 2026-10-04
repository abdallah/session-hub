// Package gitinfo looks up the git remote and branch of a directory.
package gitinfo

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Timeout bounds the whole lookup.
const Timeout = 500 * time.Millisecond

// Lookup returns the origin URL and current branch of the repo at cwd. Either
// value is "" on any error (not a repo, no origin, git missing, timeout).
func Lookup(ctx context.Context, cwd string) (repo, branch string) {
	if cwd == "" {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	repo = run(ctx, cwd, "remote", "get-url", "origin")
	branch = run(ctx, cwd, "rev-parse", "--abbrev-ref", "HEAD")
	return repo, branch
}

func run(ctx context.Context, cwd string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
