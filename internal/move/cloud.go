package move

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
)

// cloudLinkRE is the cloud session link in claude --cloud's output.
var cloudLinkRE = regexp.MustCompile(`https://claude\.ai/code/session_[A-Za-z0-9_-]+`)

const (
	// maxPromptPartRunes caps the recap and the last request in the
	// hand-off prompt, which goes on claude's command line.
	maxPromptPartRunes = 2000
	outputTail         = 64 << 10
)

func shortSessionID(id string) string {
	if len(id) > 8 {
		id = id[:8]
	}
	return strings.ToLower(id)
}

// CloudBranch is the sessionhub-owned branch for session id's uncommitted work.
func CloudBranch(id string) string { return "sessionhub/cloud-" + shortSessionID(id) }

// cloudMessage is the message of every commit sessionhub makes on CloudBranch(id).
func cloudMessage(id string) string {
	return "sessionhub: uncommitted work from session " + shortSessionID(id)
}

// PushCloudBranch records the working tree's changes (tracked changes,
// staged or not, and untracked files, without secret-looking files, links,
// or untracked files over 10 MiB) as one commit on top of HEAD, built in a
// temporary index so the working tree, the real index, and the current
// branch do not change, and pushes it to the sessionhub-owned CloudBranch with
// --force-with-lease (cloudLease). It returns the branch and the files it
// left out. With nothing to record it returns the current branch and pushes
// nothing.
func PushCloudBranch(ctx context.Context, r Repo, sessionID string) (string, []string, error) {
	tmp, err := os.MkdirTemp("", "sessionhub-cloud-index-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(tmp)
	// The temporary index starts as HEAD's tree, so a file the user staged
	// but never committed counts as untracked here and is carried.
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	opts := readOnlyGit(env)
	if _, err := gitRaw(ctx, r.Root, opts, "read-tree", r.Head); err != nil {
		return "", nil, err
	}
	// read-tree leaves no stat data, so refresh it once; else add -u hashes
	// every tracked file.
	if _, err := gitRaw(ctx, r.Root, opts, "update-index", "-q", "--refresh"); err != nil {
		return "", nil, err
	}
	files, skipped, big, err := listUntracked(ctx, r.Root, env)
	if err != nil {
		return "", nil, err
	}
	secret, err := changedSecrets(ctx, r.Root, env)
	if err != nil {
		return "", nil, err
	}
	skipped = append(append(secret, skipped...), big...)
	if _, err := gitRaw(ctx, r.Root, opts, append([]string{"add", "-u", "--"}, secretPathspecs(true)...)...); err != nil {
		return "", nil, err
	}
	if len(files) > 0 {
		var names bytes.Buffer
		for _, f := range files {
			names.WriteString(f.name)
			names.WriteByte(0)
		}
		// Literal pathspecs: an untracked file named like a glob or a
		// pathspec magic adds only itself.
		lit := readOnlyGit(append(slices.Clone(env), "GIT_LITERAL_PATHSPECS=1"))
		lit.stdin = &names
		if _, err := gitRaw(ctx, r.Root, lit, "add", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return "", nil, err
		}
	}
	tree, err := gitRaw(ctx, r.Root, opts, "write-tree")
	if err != nil {
		return "", nil, err
	}
	head, err := git(ctx, r.Root, "rev-parse", r.Head+"^{tree}")
	if err != nil {
		return "", nil, err
	}
	if head == strings.TrimSpace(string(tree)) {
		return r.Branch, skipped, nil
	}
	commit, err := git(ctx, r.Root, "commit-tree", "--no-gpg-sign", strings.TrimSpace(string(tree)), "-p", r.Head, "-m", cloudMessage(sessionID))
	if err != nil {
		return "", nil, err
	}
	branch := CloudBranch(sessionID)
	lease, err := cloudLease(ctx, r, sessionID, branch)
	if err != nil {
		return "", nil, err
	}
	if _, err := gitRaw(ctx, r.Root, gitOpts{timeout: gitNetTimeout}, "push", "--force-with-lease=refs/heads/"+branch+":"+lease,
		"origin", commit+":refs/heads/"+branch); err != nil {
		return "", nil, err
	}
	return branch, skipped, nil
}

// objectIDRE is a full SHA-1 or SHA-256 object ID.
var objectIDRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// cloudLease is the commit sessionhub expects on the push remote's CloudBranch:
// "" (the branch must not exist yet), or the last commit sessionhub pushed there,
// which this repository has and whose message is cloudMessage. Anything
// else, such as commits a cloud session pushed, is refused, so a retry never
// drops them.
func cloudLease(ctx context.Context, r Repo, sessionID, branch string) (string, error) {
	url, err := git(ctx, r.Root, "remote", "get-url", "--push", "origin")
	if err != nil {
		return "", err
	}
	out, err := gitRaw(ctx, r.Root, gitOpts{timeout: gitNetTimeout}, "ls-remote", "--", url, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "", nil
	}
	if objectIDRE.MatchString(f[0]) {
		raw, err := git(ctx, r.Root, "cat-file", "commit", f[0])
		if _, msg, ok := strings.Cut(raw, "\n\n"); err == nil && ok && strings.TrimSpace(msg) == cloudMessage(sessionID) {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("origin's %s holds commits sessionhub did not push there (a cloud session's work?), so sessionhub did not push; merge or delete that branch, then move again", branch)
}

// HandOffPrompt is the first prompt of the cloud session.
func HandOffPrompt(s api.Session, machine, branch string) string {
	orNone := func(v string, n int) string {
		if v = termtext.Clean(v, n); strings.TrimSpace(v) == "" {
			return "none"
		}
		return v
	}
	lines := []string{
		fmt.Sprintf("Continue work moved from a local Claude Code session (%s on %s).", shortSessionID(s.ID), machine),
		"Branch: " + branch + ".",
		"Recap: " + orNone(s.Recap, maxPromptPartRunes),
		"Last request: " + orNone(s.LastPrompt, maxPromptPartRunes),
	}
	if r := s.LatestReport; r != nil {
		var parts []string
		for _, p := range []struct {
			name  string
			items []string
		}{{"done", r.Done}, {"in flight", r.InFlight}, {"waiting on", r.WaitingOn}} {
			if len(p.items) > 0 {
				parts = append(parts, p.name+": "+termtext.Clean(strings.Join(p.items, "; "), 500))
			}
		}
		if len(parts) > 0 {
			lines = append(lines, "Status: "+strings.Join(parts, " / "))
		}
	}
	return strings.Join(lines, "\n")
}

// cloudLinkFullRE is a whole cloud session link, the shape the sessionhub accepts.
var cloudLinkFullRE = regexp.MustCompile(`^https://claude\.ai/code/session_[A-Za-z0-9_-]+$`)

// ValidCloudLink reports whether link is an https link to claude.ai with
// nothing but a session path, and no character termtext.Clean would change.
func ValidCloudLink(link string) bool {
	u, err := url.Parse(link)
	return err == nil && cloudLinkFullRE.MatchString(link) && u.Scheme == "https" && u.Host == "claude.ai" &&
		u.User == nil && u.RawQuery == "" && u.Fragment == "" && termtext.Clean(link, 0) == link
}

// linkWatcher collects a command's output and calls found once it holds a
// whole cloud session link.
type linkWatcher struct {
	mu    sync.Mutex
	buf   []byte
	link  string
	found func()
}

func (w *linkWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > outputTail {
		w.buf = w.buf[len(w.buf)-outputTail:]
	}
	if w.link == "" {
		// A link that ends the buffer may continue in the next write.
		if loc := cloudLinkRE.FindIndex(w.buf); loc != nil && loc[1] < len(w.buf) {
			w.link = string(w.buf[loc[0]:loc[1]])
			w.found()
		}
	}
	return len(p), nil
}

// lastLine is the last non-blank output line, cleaned.
func (w *linkWatcher) lastLine() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	lines := strings.Split(string(w.buf), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := termtext.Clean(lines[i], 200); strings.TrimSpace(l) != "" {
			return l
		}
	}
	return "no output"
}

// CloudStartedError is a StartCloud failure after which a cloud session may
// exist anyway: claude --cloud exited 0, or printed a claude.ai link sessionhub
// does not trust. The caller must not restart the session here.
type CloudStartedError struct{ Err error }

func (e *CloudStartedError) Error() string { return e.Err.Error() + "; a cloud session may exist" }
func (e *CloudStartedError) Unwrap() error { return e.Err }

// anyClaudeLinkRE is any link to claude.ai or a subdomain, trusted or not.
var anyClaudeLinkRE = regexp.MustCompile(`(?i)https?://(?:[a-z0-9-]+\.)*claude\.ai\b`)

// cloudCommand builds the claude --cloud command. claude refuses --cloud
// without a terminal ("Non-interactive invocations ... run locally"), so
// when util-linux script is installed the command runs under it, which
// gives claude a pseudo-terminal. The binary and the prompt pass through
// the environment, so the shell script sees no user text. Tests replace it.
var cloudCommand = func(ctx context.Context, bin, prompt string) *exec.Cmd {
	script, err := exec.LookPath("script")
	if err != nil {
		return exec.CommandContext(ctx, bin, "--cloud", prompt)
	}
	cmd := exec.CommandContext(ctx, script, "-qfec", `exec "$SESSIONHUB_CLAUDE_BIN" --cloud "$SESSIONHUB_CLOUD_PROMPT"`, "/dev/null")
	cmd.Env = append(os.Environ(), "SESSIONHUB_CLAUDE_BIN="+bin, "SESSIONHUB_CLOUD_PROMPT="+prompt)
	return cmd
}

// StartCloud runs `claude --cloud <prompt>` in dir and returns the first
// cloud session link it prints. It stops the command once the link is out,
// and gives up after wait.
func StartCloud(ctx context.Context, bin, dir, prompt string, wait time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	w := &linkWatcher{found: cancel}
	cmd := cloudCommand(ctx, bin, prompt)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = w, w
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	w.mu.Lock()
	link := w.link
	if link == "" {
		link = string(cloudLinkRE.Find(w.buf))
	}
	anyLink := anyClaudeLinkRE.Match(w.buf)
	w.mu.Unlock()
	switch {
	case link != "" && ValidCloudLink(link):
		return link, nil
	case link != "" || anyLink:
		return "", &CloudStartedError{fmt.Errorf("claude --cloud printed a claude.ai link sessionhub does not trust: %s", w.lastLine())}
	case err == nil:
		return "", &CloudStartedError{fmt.Errorf("claude --cloud printed no session link: %s", w.lastLine())}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", fmt.Errorf("no cloud session link within %s: %s", wait, w.lastLine())
	}
	return "", fmt.Errorf("claude --cloud: %v: %s", err, w.lastLine())
}
