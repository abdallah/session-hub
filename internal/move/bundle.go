package move

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

const manifestVersion = 1

// Bundle entry names. The transcript, its sidecar folder, and the file
// history are under transcript/ and file-history/.
const (
	entryManifest  = "manifest.json"
	entryPatch     = "git/changes.patch"
	entryUntracked = "git/untracked.tar"
)

var (
	// maxUntrackedFile caps one untracked file; maxSealed caps the sealed
	// bundle. Tests lower them.
	maxUntrackedFile int64 = 10 << 20
	maxSealed        int64 = api.MaxSealedBundle

	sessionIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	// moveIDRE matches a move ID: mv_ and 22 base64url characters.
	moveIDRE = regexp.MustCompile(`^mv_[A-Za-z0-9_-]{22}$`)
	// headRE matches a full SHA-1 or SHA-256 object ID.
	headRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// Manifest describes a bundle. Files lists every entry but manifest.json, in
// order; Untracked lists the files in git/untracked.tar; Skipped lists what
// sessionhub did not carry: tracked secret-looking files whose changes it left out
// of the patch, and untracked secret-looking names, links, and nested
// repositories, so the target can say what to copy by hand.
type Manifest struct {
	V             int       `json:"v"`
	MoveID        string    `json:"move_id"`
	SessionID     string    `json:"session_id"`
	SourceMachine string    `json:"source_machine"`
	SourceCWD     string    `json:"source_cwd"`
	RelPath       string    `json:"rel_path"`
	RootRel       string    `json:"root_rel"`
	Remote        string    `json:"remote"`
	Branch        string    `json:"branch"`
	Head          string    `json:"head"`
	ClaudeVersion string    `json:"claude_version"`
	Created       time.Time `json:"created"`
	Files         []string  `json:"files"`
	Untracked     []string  `json:"untracked"`
	Skipped       []string  `json:"skipped"`
}

// File is one file of a bundle. Name is relative to its folder (the sidecar
// folder, the file-history folder, or the repository root) and
// slash-separated. Exec is the executable bit of an untracked file.
type File struct {
	Name string
	Data []byte
	Exec bool
}

// Bundle is an opened, checked bundle.
type Bundle struct {
	Manifest   Manifest
	Transcript []byte
	Sidecar    []File
	History    []File
	Patch      []byte
	Untracked  []File
}

// BuildInput is what Build packs.
type BuildInput struct {
	MoveID, SessionID, Machine, CWD string
	ClaudeDir                       string // ~/.claude, or $CLAUDE_CONFIG_DIR
	ClaudeVersion                   string
	Repo                            Repo
	Now                             time.Time
}

// FindTranscripts lists <claudeDir>/projects/*/<id>.jsonl.
func FindTranscripts(claudeDir, id string) ([]string, error) {
	if !sessionIDRE.MatchString(id) {
		return nil, fmt.Errorf("invalid session ID %q", id)
	}
	return filepath.Glob(filepath.Join(claudeDir, "projects", "*", id+".jsonl"))
}

// IsSecretName reports whether a file looks like it holds a secret: a base
// name that starts with .env or ends in .pem, .key, or .tfvars, in any case.
func IsSecretName(name string) bool {
	b := strings.ToLower(path.Base(filepath.ToSlash(name)))
	return strings.HasPrefix(b, ".env") || strings.HasSuffix(b, ".pem") ||
		strings.HasSuffix(b, ".key") || strings.HasSuffix(b, ".tfvars")
}

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

type entry struct {
	name string
	data []byte
}

// candidate is a file Build means to carry, as Lstat (or, for the
// transcript, Stat) saw it. name is its bundle entry name, or its repository
// path for an untracked file.
type candidate struct {
	name, path string
	info       fs.FileInfo
}

// sized is a name and a size, for the "largest files" list of a size error.
type sized struct {
	name string
	size int64
}

// Build packs the session's transcript, sidecar folder, and file history,
// the repository's tracked changes against HEAD as a binary patch, and its
// untracked files, into a tar archive. Secret-looking files stay out of
// both and are listed in the manifest's Skipped, as are links and other
// files that are not regular files. It fails when the transcript is missing
// or not unique, an untracked file passes 10 MiB, or the sealed bundle would
// pass 64 MiB, naming the files. It checks the sizes before it reads any
// file, and it never writes the repository's index.
func Build(ctx context.Context, in BuildInput) ([]byte, Manifest, error) {
	m := Manifest{V: manifestVersion, MoveID: in.MoveID, SessionID: in.SessionID, SourceMachine: in.Machine,
		SourceCWD: in.CWD, RelPath: in.Repo.RelPath, RootRel: in.Repo.RootRel, Remote: in.Repo.Remote,
		Branch: in.Repo.Branch, Head: in.Repo.Head, ClaudeVersion: in.ClaudeVersion, Created: in.Now.UTC(),
		Files: []string{}, Untracked: []string{}, Skipped: []string{}}
	ts, err := FindTranscripts(in.ClaudeDir, in.SessionID)
	if err != nil {
		return nil, m, err
	}
	if len(ts) != 1 {
		return nil, m, fmt.Errorf("found %d transcripts for session %s under %s, want 1", len(ts), in.SessionID, in.ClaudeDir)
	}

	// Stat everything first, so a bundle that can't fit fails before sessionhub
	// reads a byte of it.
	fi, err := os.Stat(ts[0])
	if err != nil {
		return nil, m, err
	}
	if !fi.Mode().IsRegular() {
		return nil, m, fmt.Errorf("the transcript %s is not a regular file", ts[0])
	}
	cands := []candidate{{"transcript/" + in.SessionID + ".jsonl", ts[0], fi}}
	for _, d := range []struct{ dir, prefix string }{
		{strings.TrimSuffix(ts[0], ".jsonl"), "transcript/" + in.SessionID},
		{filepath.Join(in.ClaudeDir, "file-history", in.SessionID), "file-history/" + in.SessionID},
	} {
		c, skipped, err := statTree(d.dir, d.prefix)
		if err != nil {
			return nil, m, err
		}
		cands = append(cands, c...)
		m.Skipped = append(m.Skipped, skipped...)
	}
	env, cleanup, err := indexCopy(ctx, in.Repo.Root)
	if err != nil {
		return nil, m, err
	}
	defer cleanup()
	untracked, skipped, err := untrackedFiles(ctx, in.Repo.Root, env)
	if err != nil {
		return nil, m, err
	}
	budget := maxSealed - SealOverhead
	var all []sized
	var total int64
	for _, c := range append(slices.Clone(cands), untracked...) {
		all = append(all, sized{c.name, c.info.Size()})
		total += c.info.Size()
	}
	if total > budget {
		return nil, m, tooBig(total+SealOverhead, all)
	}
	patch, secret, err := trackedChanges(ctx, in.Repo.Root, env)
	if err != nil {
		return nil, m, err
	}
	all = append(all, sized{entryPatch, int64(len(patch))})
	if total += int64(len(patch)); total > budget {
		return nil, m, tooBig(total+SealOverhead, all)
	}
	m.Skipped = append(append(m.Skipped, secret...), skipped...)

	// Read, allowing a file to grow only into what is left of the budget.
	left := budget - total
	read := func(c candidate, limit int64) ([]byte, error) {
		data, err := readCandidate(c, min(limit, c.info.Size()+left))
		if err != nil {
			return nil, err
		}
		left -= max(0, int64(len(data))-c.info.Size())
		return data, nil
	}
	var entries []entry
	add := func(name string, data []byte) {
		entries = append(entries, entry{name, data})
		m.Files = append(m.Files, name)
	}
	for _, c := range cands {
		data, err := read(c, budget)
		if err != nil {
			return nil, m, err
		}
		add(c.name, data)
	}
	add(entryPatch, patch)
	var files []File
	for _, c := range untracked {
		data, err := read(c, maxUntrackedFile)
		if err != nil {
			return nil, m, err
		}
		files = append(files, File{Name: c.name, Data: data, Exec: c.info.Mode().Perm()&0o111 != 0})
		m.Untracked = append(m.Untracked, c.name)
	}
	inner, err := tarFiles(files, in.Now)
	if err != nil {
		return nil, m, err
	}
	add(entryUntracked, inner)

	mj, err := json.Marshal(m)
	if err != nil {
		return nil, m, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writeEntry(tw, entryManifest, mj, 0o600, in.Now); err != nil {
		return nil, m, err
	}
	for _, e := range entries {
		if err := writeEntry(tw, e.name, e.data, 0o600, in.Now); err != nil {
			return nil, m, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, m, err
	}
	if size := int64(buf.Len()) + SealOverhead; size > maxSealed {
		all = all[:0]
		for _, e := range entries {
			if e.name != entryUntracked {
				all = append(all, sized{e.name, int64(len(e.data))})
			}
		}
		for _, f := range files {
			all = append(all, sized{f.Name, int64(len(f.Data))})
		}
		return nil, m, tooBig(size, all)
	}
	return buf.Bytes(), m, nil
}

func writeEntry(tw *tar.Writer, name string, data []byte, mode int64, now time.Time) error {
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(data)),
		ModTime: now.UTC(), Format: tar.FormatPAX}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// tooBig is the error for a bundle of size bytes, sealed, over maxSealed. It
// names the five largest files.
func tooBig(size int64, all []sized) error {
	return fmt.Errorf("the bundle is %s, over the %s limit; largest files: %s", mib(size), mib(maxSealed), largest(all, 5))
}

// largest names the n biggest files as "name (size)".
func largest(all []sized, n int) string {
	all = slices.Clone(all)
	sort.SliceStable(all, func(i, j int) bool { return all[i].size > all[j].size })
	var out []string
	for i := 0; i < len(all) && i < n; i++ {
		out = append(out, fmt.Sprintf("%s (%s)", all[i].name, mib(all[i].size)))
	}
	return strings.Join(out, ", ")
}

// readCandidate reads c, which must still be the file Build stat'd and at
// most limit bytes long.
func readCandidate(c candidate, limit int64) ([]byte, error) {
	f, err := os.Open(c.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !os.SameFile(fi, c.info) {
		return nil, fmt.Errorf("%s changed while sessionhub read it; try again", c.name)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s grew past %s while sessionhub read it", c.name, mib(limit))
	}
	return data, nil
}

// statTree lists every regular file under dir, in lexical order, named
// prefix/<path relative to dir>. A missing dir is empty. Anything else, such
// as a link (dir itself included), is returned by name as skipped.
func statTree(dir, prefix string) ([]candidate, []string, error) {
	var out []candidate
	var skipped []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p == dir {
			return filepath.SkipAll
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name := prefix
		if rel != "." {
			name += "/" + filepath.ToSlash(rel)
		}
		switch {
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			skipped = append(skipped, name)
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, candidate{name, p, fi})
		return nil
	})
	return out, skipped, err
}

// indexCopy copies the repository's index into a temporary directory and
// returns the environment that points git at the copy, so git diff can
// refresh it there and never rewrites the user's index. cleanup removes the
// copy. With no index, git starts from an empty one in the same place.
func indexCopy(ctx context.Context, root string) (env []string, cleanup func(), err error) {
	idx, err := git(ctx, root, "rev-parse", "--git-path", "index")
	if err != nil {
		return nil, nil, err
	}
	if !filepath.IsAbs(idx) {
		idx = filepath.Join(root, idx)
	}
	tmp, err := os.MkdirTemp("", "sessionhub-move-index-")
	if err != nil {
		return nil, nil, err
	}
	cleanup = func() { os.RemoveAll(tmp) }
	cp := filepath.Join(tmp, "index")
	if err := copyFile(idx, cp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		cleanup()
		return nil, nil, err
	}
	return []string{"GIT_INDEX_FILE=" + cp}, cleanup, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// readOnlyGit is the options of a read-only git call over the index copy in
// env: fsmonitor off, so no hook runs and nothing updates its token.
func readOnlyGit(env []string) gitOpts {
	return gitOpts{env: env, config: []string{"core.fsmonitor=false"}}
}

// untrackedFiles lists the repository's untracked, not ignored files.
// Secret-looking names, links, and anything that is not a regular file
// (a nested repository shows as "dir/") are returned by name as skipped. It
// fails, naming them, when files pass maxUntrackedFile.
func untrackedFiles(ctx context.Context, root string, env []string) ([]candidate, []string, error) {
	files, skipped, big, err := listUntracked(ctx, root, env)
	if err != nil {
		return nil, nil, err
	}
	if len(big) > 0 {
		return nil, nil, fmt.Errorf("untracked files over %s each: %s", mib(maxUntrackedFile), strings.Join(big, ", "))
	}
	return files, skipped, nil
}

// listUntracked is untrackedFiles without the size check: it returns the
// files over maxUntrackedFile as "name (size)" in big instead of failing.
func listUntracked(ctx context.Context, root string, env []string) (files []candidate, skipped, big []string, err error) {
	out, err := gitRaw(ctx, root, readOnlyGit(env), "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, nil, nil, err
	}
	skipped = []string{}
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" {
			continue
		}
		if IsSecretName(name) {
			skipped = append(skipped, name)
			continue
		}
		p := filepath.Join(root, filepath.FromSlash(name))
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, nil, nil, err
		}
		if !fi.Mode().IsRegular() {
			skipped = append(skipped, name)
			continue
		}
		if fi.Size() > maxUntrackedFile {
			big = append(big, fmt.Sprintf("%s (%s)", name, mib(fi.Size())))
			continue
		}
		files = append(files, candidate{name, p, fi})
	}
	return files, skipped, big, nil
}

// secretGlobs match the names IsSecretName matches, at any depth, as git
// pathspecs with the icase and glob magic.
var secretGlobs = []string{"**/.env*", "**/*.pem", "**/*.key", "**/*.tfvars"}

// secretPathspecs are pathspecs for secret-looking files: with exclude, the
// whole tree without them; else only them.
func secretPathspecs(exclude bool) []string {
	magic, out := ":(icase,glob)", []string{}
	if exclude {
		magic, out = ":(exclude,icase,glob)", []string{"."}
	}
	for _, g := range secretGlobs {
		out = append(out, magic+g)
	}
	return out
}

// diffArgs is a git diff command line that the user's diff config can't
// change: no color, no external diff or textconv, no rename detection, full
// object IDs (so git apply -R can reverse a binary hunk), and the a/ and b/
// prefixes.
func diffArgs(args ...string) []string {
	return append([]string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--full-index",
		"--src-prefix=a/", "--dst-prefix=b/"}, args...)
}

// changedSecrets lists the tracked secret-looking files that differ from
// HEAD, staged or not.
func changedSecrets(ctx context.Context, root string, env []string) ([]string, error) {
	out, err := gitRaw(ctx, root, readOnlyGit(env), append(diffArgs("--name-only", "-z", "HEAD", "--"), secretPathspecs(false)...)...)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Split(string(out), "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// trackedChanges is the binary patch of the tracked changes against HEAD,
// staged and unstaged, without secret-looking files, and the names of the
// secret-looking files it left out. env points git at a copy of the index
// (indexCopy).
func trackedChanges(ctx context.Context, root string, env []string) ([]byte, []string, error) {
	secret, err := changedSecrets(ctx, root, env)
	if err != nil {
		return nil, nil, err
	}
	patch, err := gitRaw(ctx, root, readOnlyGit(env), append(diffArgs("--binary", "HEAD", "--"), secretPathspecs(true)...)...)
	return patch, secret, err
}

// tarFiles packs untracked files with mode 0755 or 0644.
func tarFiles(files []File, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		mode := int64(0o644)
		if f.Exec {
			mode = 0o755
		}
		if err := writeEntry(tw, f.Name, f.Data, mode, now); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// safeName accepts a relative, clean, slash-separated name with no "." or
// ".." element.
func safeName(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// checkManifest checks the manifest fields a move later uses as paths or Git
// arguments.
func checkManifest(m Manifest) error {
	bad := func(field, v string) error { return fmt.Errorf("bundle manifest has %s %q", field, v) }
	switch {
	case !sessionIDRE.MatchString(m.SessionID):
		return bad("session ID", m.SessionID)
	case !moveIDRE.MatchString(m.MoveID):
		return bad("move ID", m.MoveID)
	case m.RelPath != "" && !safeName(m.RelPath):
		return bad("relative path", m.RelPath)
	case m.RootRel != "" && !safeName(m.RootRel):
		return bad("root path", m.RootRel)
	case !validBranch(m.Branch):
		return bad("branch", m.Branch)
	case !headRE.MatchString(m.Head):
		return bad("HEAD", m.Head)
	}
	return nil
}

// validBranch follows the rules of git check-ref-format --branch and also
// refuses a leading "-", so the name can't pass for an option, and "@". A
// component can't be empty, start with ".", or end in ".lock"; the name
// can't be "HEAD", end in "/" or ".", or hold "..", "@{", a control character, a space, or
// any of ~^:?*[\.
func validBranch(b string) bool {
	if b == "" || b == "@" || b == "HEAD" || strings.HasPrefix(b, "-") || strings.HasSuffix(b, ".") ||
		strings.Contains(b, "..") || strings.Contains(b, "@{") || strings.ContainsAny(b, " ~^:?*[\\") {
		return false
	}
	for _, r := range b {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, part := range strings.Split(b, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// Extract reads and checks a bundle: manifest.json first, with a valid move
// ID, session ID, relative paths, branch, and HEAD; then only the entry names
// a move uses, each a regular file listed in the manifest, once;
// the manifest's list complete; the untracked files safe repository paths
// outside .git that match the manifest. It never touches the disk.
func Extract(data []byte) (*Bundle, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	b := &Bundle{}
	seen := map[string]bool{}
	first := true
	var untracked []byte
	haveTranscript, havePatch, haveUntracked := false, false, false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read bundle: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("bundle entry %q is not a regular file", h.Name)
		}
		if !safeName(h.Name) {
			return nil, fmt.Errorf("bundle entry %q has an unsafe name", h.Name)
		}
		if seen[h.Name] {
			return nil, fmt.Errorf("bundle entry %q appears twice", h.Name)
		}
		seen[h.Name] = true
		if h.Size < 0 || h.Size > maxSealed {
			return nil, fmt.Errorf("bundle entry %q has size %d", h.Name, h.Size)
		}
		body, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return nil, fmt.Errorf("read bundle entry %q: %w", h.Name, err)
		}
		if first {
			if h.Name != entryManifest {
				return nil, errors.New("bundle does not start with manifest.json")
			}
			if err := json.Unmarshal(body, &b.Manifest); err != nil {
				return nil, fmt.Errorf("bundle manifest: %w", err)
			}
			if b.Manifest.V != manifestVersion {
				return nil, fmt.Errorf("bundle manifest version %d, want %d", b.Manifest.V, manifestVersion)
			}
			if err := checkManifest(b.Manifest); err != nil {
				return nil, err
			}
			first = false
			continue
		}
		id := b.Manifest.SessionID
		switch name := h.Name; {
		case name == "transcript/"+id+".jsonl":
			b.Transcript, haveTranscript = body, true
		case strings.HasPrefix(name, "transcript/"+id+"/"):
			b.Sidecar = append(b.Sidecar, File{Name: strings.TrimPrefix(name, "transcript/"+id+"/"), Data: body})
		case strings.HasPrefix(name, "file-history/"+id+"/"):
			b.History = append(b.History, File{Name: strings.TrimPrefix(name, "file-history/"+id+"/"), Data: body})
		case name == entryPatch:
			b.Patch, havePatch = body, true
		case name == entryUntracked:
			untracked, haveUntracked = body, true
		default:
			return nil, fmt.Errorf("bundle entry %q is not part of a move", name)
		}
	}
	switch {
	case first:
		return nil, errors.New("bundle is empty")
	case !haveTranscript || !havePatch || !haveUntracked:
		return nil, errors.New("bundle lacks the transcript, the patch, or the untracked files")
	}
	delete(seen, entryManifest)
	if !sameSet(seen, b.Manifest.Files) {
		return nil, errors.New("bundle entries do not match its manifest")
	}
	files, err := readUntracked(untracked)
	if err != nil {
		return nil, err
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f.Name] = true
	}
	if !sameSet(got, b.Manifest.Untracked) {
		return nil, errors.New("bundle's untracked files do not match its manifest")
	}
	b.Untracked = files
	return b, nil
}

// sameSet reports whether the keys of have are exactly list, each once.
func sameSet(have map[string]bool, list []string) bool {
	if len(have) != len(list) {
		return false
	}
	for _, n := range list {
		if !have[n] {
			return false
		}
	}
	return len(slices.Compact(slices.Sorted(slices.Values(list)))) == len(list)
}

// readUntracked reads git/untracked.tar: regular files with safe names
// outside .git, each once.
func readUntracked(data []byte) ([]File, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	var out []File
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read untracked files: %w", err)
		}
		bad := h.Typeflag != tar.TypeReg || !safeName(h.Name) || seen[h.Name] || h.Size < 0 || h.Size > maxUntrackedFile
		for _, part := range strings.Split(h.Name, "/") {
			if strings.EqualFold(part, ".git") {
				bad = true
			}
		}
		if bad {
			return nil, fmt.Errorf("untracked file %q is not allowed", h.Name)
		}
		seen[h.Name] = true
		body, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return nil, err
		}
		out = append(out, File{Name: h.Name, Data: body, Exec: h.Mode&0o111 != 0})
	}
}
