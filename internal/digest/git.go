package digest

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
)

// GitTimeout bounds all git commands of one digest.
const GitTimeout = 3 * time.Second

var shaRE = regexp.MustCompile(`^[0-9a-f]{4,40}$`)

const (
	maxCommits = 5
	maxSubject = 120
)

// Git summarizes HEAD's commits in [since, until] and the uncommitted and
// unpushed counts in cwd. It returns nil when cwd is missing or not a
// repository, HEAD has no commits, or git fails or times out.
func Git(ctx context.Context, cwd string, since, until time.Time) *api.DigestGit {
	if cwd == "" {
		return nil
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, GitTimeout)
	defer cancel()
	out, err := git(ctx, cwd, "-c", "log.showSignature=false", "log", "--since="+since.UTC().Format(time.RFC3339),
		"--until="+until.UTC().Format(time.RFC3339), "--format=%h%x00%s", "HEAD")
	if err != nil {
		return nil
	}
	g := parseLog(out)
	status, err := git(ctx, cwd, "status", "--porcelain")
	if err != nil {
		return nil
	}
	for _, l := range strings.Split(status, "\n") {
		if l != "" {
			g.Uncommitted++
		}
	}
	g.Unpushed = -1
	if n, err := git(ctx, cwd, "rev-list", "--count", "@{u}..HEAD"); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			g.Unpushed = v
		}
	}
	return g
}

// parseLog reads `git log --format=%h%x00%s` output. It skips lines that
// don't start with a hex sha and a NUL, such as signature output, because the
// server rejects a digest with a bad sha for good.
func parseLog(out string) *api.DigestGit {
	g := &api.DigestGit{Commits: []api.DigestCommit{}}
	for _, l := range strings.Split(out, "\n") {
		sha, subj, ok := strings.Cut(l, "\x00")
		if !ok || !shaRE.MatchString(sha) {
			continue
		}
		g.CommitCount++
		if len(g.Commits) < maxCommits {
			g.Commits = append(g.Commits, api.DigestCommit{SHA: sha, Subject: termtext.Clean(subj, maxSubject)})
		}
	}
	return g
}

// git runs one read-only git command. GIT_OPTIONAL_LOCKS=0 keeps
// `git status` from taking index.lock, which would get in the way of the
// user's own git commands.
func git(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	out, err := cmd.Output()
	return string(out), err
}
