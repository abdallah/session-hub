package move

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Clone search and project folder limits.
const (
	cloneDepth     = 4
	maxProjectName = 200
)

// ErrNoClone is FindClone's error when no clone has the remote.
var ErrNoClone = errors.New("no clone")

// FindClone finds the Git repository whose origin remote is remote, under
// the search roots, at most cloneDepth levels down. It does not descend into
// a repository, a hidden directory, or node_modules. With several, it takes
// the one whose path under its root equals rootRel, ignoring case, and
// otherwise fails listing them. A cancelled ctx stops the search.
func FindClone(ctx context.Context, roots []string, remote, rootRelPath string) (string, error) {
	want := NormalizeRemote(remote)
	var found []string
	seen := map[string]bool{}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if ctx.Err() != nil {
			return
		}
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			real, err := filepath.EvalSymlinks(dir)
			if err != nil || seen[real] {
				return
			}
			seen[real] = true
			if u, err := git(ctx, dir, "remote", "get-url", "origin"); err == nil && NormalizeRemote(u) == want {
				found = append(found, dir)
			}
			return
		}
		if depth >= cloneDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "node_modules" {
				continue
			}
			walk(filepath.Join(dir, e.Name()), depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%w of %s", ErrNoClone, want)
	case 1:
		return found[0], nil
	}
	var match []string
	for _, f := range found {
		if strings.EqualFold(rootRel(f, roots), rootRelPath) {
			match = append(match, f)
		}
	}
	if len(match) == 1 {
		return match[0], nil
	}
	if len(match) > 1 {
		sort.Strings(match)
		return "", fmt.Errorf("several clones of %s at %s: %s", want, rootRelPath, strings.Join(match, ", "))
	}
	sort.Strings(found)
	return "", fmt.Errorf("several clones of %s and none at %s: %s", want, rootRelPath, strings.Join(found, ", "))
}

// CheckClone fails when the clone has uncommitted tracked changes, or when a
// carried path (an untracked file of the bundle, or a file its patch creates:
// PatchNewFiles) already exists there.
func CheckClone(ctx context.Context, root string, carried []string) error {
	out, err := git(ctx, root, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if out != "" {
		return fmt.Errorf("%s has uncommitted changes; commit or put them aside there first", root)
	}
	var clash []string
	for _, n := range carried {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(n))); err == nil {
			clash = append(clash, n)
		}
	}
	if len(clash) > 0 {
		if len(clash) > 5 {
			clash = append(clash[:5], "...")
		}
		return fmt.Errorf("files in %s would be overwritten: %s", root, strings.Join(clash, ", "))
	}
	return nil
}

// PatchNewFiles lists the files a patch from Build creates: each one's
// "diff --git" line is followed by "new file mode". A path git quoted is
// unquoted.
func PatchNewFiles(patch []byte) []string {
	var out []string
	lines := strings.Split(string(patch), "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, "diff --git ") || i+1 == len(lines) || !strings.HasPrefix(lines[i+1], "new file mode ") {
			continue
		}
		if name, ok := diffPath(strings.TrimPrefix(l, "diff --git ")); ok {
			out = append(out, name)
		}
	}
	return out
}

// diffPath is the path of a "diff --git" line whose two sides name the same
// file, as they do for a new file: a/<path> b/<path>, each quoted the way
// git quotes a path with special characters.
func diffPath(rest string) (string, bool) {
	if strings.HasPrefix(rest, `"`) {
		q, err := strconv.QuotedPrefix(rest)
		if err != nil {
			return "", false
		}
		a, err := strconv.Unquote(q)
		if err != nil || !strings.HasPrefix(a, "a/") {
			return "", false
		}
		return a[2:], true
	}
	if n := len(rest) - len("a/ b/"); n > 0 && n%2 == 0 {
		p := rest[2 : 2+n/2]
		if rest == "a/"+p+" b/"+p {
			return p, true
		}
	}
	return "", false
}

// Checkout is a clone sessionhub switched to the moved branch, and what Apply did
// to it, so Undo can take back exactly that and nothing else.
type Checkout struct {
	root    string
	prev    string // the branch, or the SHA when detached, before the switch
	branch  string // the moved branch
	oldTip  string // the branch's commit before the switch; "" when sessionhub created the branch
	newTip  string // the branch's commit after the switch
	hadConf bool   // branch.<branch>.remote or .merge was set before
	patch   []byte // the patch Apply applied, or nil
	wrote   []File // the untracked files Apply wrote, as written
	dirs    []string
}

// CurrentBranch returns the branch checked out in root, or "" when HEAD is
// detached.
func CurrentBranch(ctx context.Context, root string) (string, error) {
	out, err := git(ctx, root, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil && out == "" && gitOK(ctx, root, "rev-parse", "--verify", "-q", "HEAD") {
		return "", nil
	}
	return out, err
}

// PrepareClone fetches origin and checks out branch at head: the local
// branch when there is one, else a tracking branch from origin, else a new
// branch at head. It moves the branch by fast-forward only, and fails, back
// on the previous branch with the moved branch as it was, when the branch
// here has commits head lacks.
func PrepareClone(ctx context.Context, root, branch, head string) (*Checkout, error) {
	c := &Checkout{root: root, branch: branch}
	prev, err := git(ctx, root, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil || prev == "" {
		if prev, err = git(ctx, root, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
			return nil, fmt.Errorf("%s has no commits", root)
		}
	}
	c.prev = prev
	c.oldTip, _ = git(ctx, root, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	c.hadConf = gitOK(ctx, root, "config", "--get", "branch."+branch+".remote") ||
		gitOK(ctx, root, "config", "--get", "branch."+branch+".merge")
	if _, err := gitRaw(ctx, root, gitOpts{timeout: gitNetTimeout}, "fetch", "-q", "origin"); err != nil {
		return nil, err
	}
	if _, err := git(ctx, root, "cat-file", "-e", head+"^{commit}"); err != nil {
		return nil, fmt.Errorf("commit %.12s is not on origin; push it from the source machine", head)
	}
	switch {
	case c.oldTip != "":
		_, err = git(ctx, root, "checkout", "-q", branch)
	case gitOK(ctx, root, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+branch):
		_, err = git(ctx, root, "checkout", "-q", "-b", branch, "--track", "origin/"+branch)
	default:
		_, err = git(ctx, root, "checkout", "-q", "-b", branch, head)
	}
	if err != nil {
		return nil, err
	}
	c.newTip, _ = git(ctx, root, "rev-parse", "HEAD")
	if _, err := git(ctx, root, "merge", "-q", "--ff-only", head); err != nil {
		return nil, c.restoreAfter(ctx, fmt.Errorf("branch %s here cannot fast-forward to %.12s: %v", branch, head, err))
	}
	if c.newTip, _ = git(ctx, root, "rev-parse", "HEAD"); c.newTip != head {
		return nil, c.restoreAfter(ctx, fmt.Errorf("branch %s here has commits the source lacks, so it cannot fast-forward to %.12s", branch, head))
	}
	return c, nil
}

// restoreAfter puts the clone back after PrepareClone failed with err, and
// returns err, with what went wrong when the clone could not be put back.
func (c *Checkout) restoreAfter(ctx context.Context, err error) error {
	if rerr := c.restore(context.WithoutCancel(ctx)); rerr != nil {
		return fmt.Errorf("%w; putting the clone back failed: %v", err, rerr)
	}
	return err
}

func gitOK(ctx context.Context, dir string, args ...string) bool {
	_, err := git(ctx, dir, args...)
	return err == nil
}

// Prev is the branch (or SHA) the clone was on before PrepareClone.
func (c *Checkout) Prev() string { return c.prev }

// restore checks out the previous branch again and puts the moved branch
// back: a branch sessionhub created is deleted, with the tracking config it added,
// and a branch it fast-forwarded returns to its old commit. Each ref update
// is a compare-and-swap on the commit sessionhub left, so a change made since is
// never overwritten.
func (c *Checkout) restore(ctx context.Context) error {
	if _, err := git(ctx, c.root, "checkout", "-q", "--detach"); err != nil {
		return err
	}
	var errs []error
	ref := "refs/heads/" + c.branch
	switch {
	case c.oldTip == "":
		if _, err := git(ctx, c.root, "update-ref", "-d", ref, c.newTip); err != nil {
			errs = append(errs, err)
		} else if !c.hadConf {
			git(ctx, c.root, "config", "--remove-section", "branch."+c.branch) // fails when there is none
		}
	case c.oldTip != c.newTip:
		if _, err := git(ctx, c.root, "update-ref", ref, c.oldTip, c.newTip); err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := git(ctx, c.root, "checkout", "-q", c.prev); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// withPatch writes patch to a temp file (mode 0600) in the clone's Git
// directory for git apply, not in the shared temp dir, and removes it after
// f.
func withPatch(ctx context.Context, root string, patch []byte, f func(name string) error) error {
	gitDir, err := git(ctx, root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	tf, err := os.CreateTemp(gitDir, "sessionhub-move-*.patch")
	if err != nil {
		return err
	}
	defer os.Remove(tf.Name())
	if _, err := tf.Write(patch); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Close(); err != nil {
		return err
	}
	return f(tf.Name())
}

// Apply applies the bundle's patch to the working tree (not the index) and
// writes its untracked files, refusing to overwrite any file or to write
// through a linked folder. It records what it did for Undo.
func (c *Checkout) Apply(ctx context.Context, b *Bundle) error {
	if len(b.Patch) > 0 {
		if err := withPatch(ctx, c.root, b.Patch, func(name string) error {
			_, err := gitRaw(ctx, c.root, gitOpts{}, "apply", "--binary", name)
			return err
		}); err != nil {
			return err
		}
		c.patch = b.Patch
	}
	for _, u := range b.Untracked {
		if err := c.writeUntracked(u); err != nil {
			return fmt.Errorf("untracked file %s: %w", u.Name, err)
		}
	}
	return nil
}

// writeUntracked writes one untracked file with O_EXCL. Each folder on the
// way is checked with Lstat: it must be a real folder, not a link, so a file
// never lands outside the clone. Missing folders are made and recorded.
func (c *Checkout) writeUntracked(u File) error {
	parts := strings.Split(u.Name, "/")
	dir := c.root
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		fi, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(dir, 0o755); err != nil {
				return err
			}
			c.dirs = append(c.dirs, dir)
		case err != nil:
			return err
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symbolic link", part)
		case !fi.IsDir():
			return fmt.Errorf("%s is not a folder", part)
		}
	}
	mode := os.FileMode(0o644)
	if u.Exec {
		mode = 0o755
	}
	p := filepath.Join(dir, parts[len(parts)-1])
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, werr := f.Write(u.Data)
	if err := errors.Join(werr, f.Close()); err != nil {
		os.Remove(p)
		return err
	}
	c.wrote = append(c.wrote, u)
	return nil
}

// Undo takes back what Apply did, and only that: it removes the untracked
// files Apply wrote while they still hold what it wrote, removes the
// folders it made once they are empty, reverses the patch with
// git apply -R, and puts the branch back (restore). It never resets or
// cleans the tree, so work that is not the move's survives. A file that
// changed since stays, and the error names it; when the patch no longer
// reverses, the tree and the branch stay as they are.
func (c *Checkout) Undo(ctx context.Context) error {
	var errs []error
	for i := len(c.wrote) - 1; i >= 0; i-- {
		u := c.wrote[i]
		p := filepath.Join(c.root, filepath.FromSlash(u.Name))
		data, err := os.ReadFile(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err == nil && bytes.Equal(data, u.Data):
			errs = append(errs, os.Remove(p))
		default:
			errs = append(errs, fmt.Errorf("kept %s: it changed after the move wrote it", u.Name))
		}
	}
	for i := len(c.dirs) - 1; i >= 0; i-- {
		os.Remove(c.dirs[i]) // only succeeds when empty
	}
	if c.patch != nil {
		if err := withPatch(ctx, c.root, c.patch, func(name string) error {
			_, err := gitRaw(ctx, c.root, gitOpts{}, "apply", "-R", "--binary", name)
			return err
		}); err != nil {
			return errors.Join(append(errs, fmt.Errorf("reverse the changes: %w", err))...)
		}
	}
	errs = append(errs, c.restore(ctx))
	return errors.Join(errs...)
}

// projectName is Claude Code's project folder name for cwd: every character
// that is not an ASCII letter or digit becomes "-", once per UTF-16 unit.
func projectName(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteString(strings.Repeat("-", max(utf16.RuneLen(r), 1)))
	}
	return b.String()
}

// ProjectDir is <claudeDir>/projects/<folder of cwd>. A name over 200
// characters gets a hash suffix in Claude Code that sessionhub does not compute, so
// it uses the one existing folder that starts with the first 200, else
// fails.
func ProjectDir(claudeDir, cwd string) (string, error) {
	name := projectName(cwd)
	base := filepath.Join(claudeDir, "projects")
	if len(name) <= maxProjectName {
		return filepath.Join(base, name), nil
	}
	m, _ := filepath.Glob(filepath.Join(base, name[:maxProjectName]+"*"))
	if len(m) == 1 {
		return m[0], nil
	}
	return "", fmt.Errorf("the project folder name for %s is longer than %d characters; start Claude there once, then move again", cwd, maxProjectName)
}

// WriteSession writes the bundle's transcript and sidecar folder into the
// project folder of cwd, and its file history, each file new (mode 0600).
// It fails when the session already has a transcript anywhere here. On a
// failure it removes what it wrote; on success it returns the paths, for
// RemoveFiles.
func WriteSession(claudeDir, cwd string, b *Bundle) ([]string, error) {
	id := b.Manifest.SessionID
	if ts, err := FindTranscripts(claudeDir, id); err != nil {
		return nil, err
	} else if len(ts) > 0 {
		return nil, fmt.Errorf("session %s already has a transcript here: %s", id, ts[0])
	}
	proj, err := ProjectDir(claudeDir, cwd)
	if err != nil {
		return nil, err
	}
	var written []string
	write := func(p string, data []byte) error {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		written = append(written, p)
		_, werr := f.Write(data)
		return errors.Join(werr, f.Close())
	}
	err = write(filepath.Join(proj, id+".jsonl"), b.Transcript)
	for _, f := range b.Sidecar {
		if err == nil {
			err = write(filepath.Join(proj, id, filepath.FromSlash(f.Name)), f.Data)
		}
	}
	for _, f := range b.History {
		if err == nil {
			err = write(filepath.Join(claudeDir, "file-history", id, filepath.FromSlash(f.Name)), f.Data)
		}
	}
	if err != nil {
		RemoveFiles(written)
		return nil, err
	}
	return written, nil
}

// RemoveFiles removes the files WriteSession wrote, then the folders they
// leave empty, deepest first.
func RemoveFiles(paths []string) {
	var dirs []string
	for _, p := range paths {
		os.Remove(p)
		for d := filepath.Dir(p); !slices.Contains(dirs, d); d = filepath.Dir(d) {
			dirs = append(dirs, d)
			if base := filepath.Base(filepath.Dir(d)); base == "projects" || base == "file-history" || d == filepath.Dir(d) {
				break
			}
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		os.Remove(d) // only succeeds when empty
	}
}
