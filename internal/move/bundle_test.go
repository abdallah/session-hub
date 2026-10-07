package move

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

var bundleNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// dirtyRepo is newRepo with two tracked secret-looking files, one with an
// upper-case name (committed and pushed, then changed), and staged, unstaged, binary, deleted, untracked,
// executable, secret-looking, linked, and ignored changes.
func dirtyRepo(t *testing.T) (origin, work string) {
	t.Helper()
	origin, work = newRepo(t)
	writeFile(t, filepath.Join(work, "conf", ".env.prod"), "secret\n", 0o600)
	writeFile(t, filepath.Join(work, "CONF", ".ENV"), "upper secret\n", 0o600)
	gitT(t, work, "add", "conf/.env.prod", "CONF/.ENV")
	gitT(t, work, "commit", "-q", "-m", "tracked secret")
	gitT(t, work, "push", "-q")
	writeFile(t, filepath.Join(work, "conf", ".env.prod"), "secret\nrotated\n", 0o600)
	writeFile(t, filepath.Join(work, "CONF", ".ENV"), "upper secret\nUPPER-ROTATED\n", 0o600)
	writeFile(t, filepath.Join(work, "a.txt"), "one\nstaged\n", 0o644)
	gitT(t, work, "add", "a.txt")
	writeFile(t, filepath.Join(work, "a.txt"), "one\nstaged\nunstaged\n", 0o644)
	writeFile(t, filepath.Join(work, "bin.dat"), "\x00\x01\x02changed\xff\xfe", 0o644)
	if err := os.Remove(filepath.Join(work, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(work, "notes", "new.md"), "new note\n", 0o644)
	writeFile(t, filepath.Join(work, "run.sh"), "#!/bin/sh\necho hi\n", 0o755)
	for _, s := range []string{".env.local", "certs/server.pem", "deploy.key", "prod.tfvars"} {
		writeFile(t, filepath.Join(work, s), "secret\n", 0o600)
	}
	if err := os.Symlink("a.txt", filepath.Join(work, "link")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(work, "build", "out.o"), "object", 0o644)
	return origin, work
}

func buildFor(t *testing.T, work, claude string) ([]byte, Manifest) {
	t.Helper()
	ctx := context.Background()
	repo, err := ReadRepo(ctx, filepath.Join(work, "sub"), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, man, err := Build(ctx, BuildInput{MoveID: "mv_AAAAAAAAAAAAAAAAAAAAAA", SessionID: testSessionID, Machine: "bluebox",
		CWD: filepath.Join(work, "sub"), ClaudeDir: claude, ClaudeVersion: "2.1.285", Repo: repo, Now: bundleNow})
	if err != nil {
		t.Fatal(err)
	}
	return data, man
}

func names(fs []File) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

func TestBuildAndExtract(t *testing.T) {
	origin, work := dirtyRepo(t)
	claude := newClaudeDir(t, testSessionID)
	data, man := buildFor(t, work, claude)

	b, err := Extract(data)
	if err != nil {
		t.Fatal(err)
	}
	m := b.Manifest
	if m.V != 2 || m.MoveID != "mv_AAAAAAAAAAAAAAAAAAAAAA" || m.SessionID != testSessionID || m.SourceMachine != "bluebox" ||
		m.RelPath != "sub" || m.RootRel != "work" || m.Branch != "main" || m.Head != gitT(t, work, "rev-parse", "HEAD") ||
		m.Remote != origin || m.ClaudeVersion != "2.1.285" || !m.Created.Equal(bundleNow) || m.SourceCWD != filepath.Join(work, "sub") {
		t.Errorf("manifest %+v", m)
	}
	if !slices.Equal(m.Files, man.Files) {
		t.Errorf("extracted files %q, built %q", m.Files, man.Files)
	}
	if string(b.Transcript) != `{"type":"user","message":"hello"}`+"\n" {
		t.Errorf("transcript %q", b.Transcript)
	}
	if got := names(b.Sidecar); !slices.Equal(got, []string{"subagents/agent-1.jsonl", "tool-results/r1.txt"}) {
		t.Errorf("sidecar %q", got)
	}
	if got := names(b.History); !slices.Equal(got, []string{"abc@v1"}) {
		t.Errorf("file history %q", got)
	}
	if got := names(b.Untracked); !slices.Equal(got, []string{"notes/new.md", "run.sh"}) {
		t.Errorf("untracked %q", got)
	}
	for _, f := range b.Untracked {
		if f.Exec != (f.Name == "run.sh") {
			t.Errorf("%s: exec %v", f.Name, f.Exec)
		}
	}

	// The patch carries staged, unstaged, binary, and deleted changes: a
	// fresh clone at HEAD ends up with the same files.
	fresh := filepath.Join(t.TempDir(), "fresh")
	gitT(t, filepath.Dir(fresh), "clone", "-q", origin, fresh)
	patch := filepath.Join(t.TempDir(), "changes.patch")
	if err := os.WriteFile(patch, b.Patch, 0o600); err != nil {
		t.Fatal(err)
	}
	gitT(t, fresh, "apply", "--binary", patch)
	for _, f := range []string{"a.txt", "bin.dat"} {
		want, _ := os.ReadFile(filepath.Join(work, f))
		got, _ := os.ReadFile(filepath.Join(fresh, f))
		if !bytes.Equal(got, want) {
			t.Errorf("%s after the patch: %q, want %q", f, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(fresh, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("gone.txt survived the patch: %v", err)
	}
}

func TestBuildSkipsSecrets(t *testing.T) {
	_, work := dirtyRepo(t)
	data, man := buildFor(t, work, newClaudeDir(t, testSessionID))
	skipped := slices.Clone(man.Skipped)
	slices.Sort(skipped)
	if want := []string{".env.local", "CONF/.ENV", "certs/server.pem", "conf/.env.prod", "deploy.key", "link", "prod.tfvars"}; !slices.Equal(skipped, want) {
		t.Errorf("skipped %q, want %q", skipped, want)
	}
	// The skipped names are in the manifest on purpose; their content, the
	// tracked secret's change, and ignored files must stay out.
	for _, leak := range []string{"secret\n", "rotated", "UPPER-ROTATED", "build/out.o", "object"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Errorf("bundle contains %q", leak)
		}
	}
	for _, n := range []string{".env", ".ENV.prod", "a/b/x.pem", "id.key", "x.tfvars", ".envrc"} {
		if !IsSecretName(n) {
			t.Errorf("IsSecretName(%q) = false", n)
		}
	}
	for _, n := range []string{"env.go", "keys.go", "pem.md", "terraform.tf", "a.txt"} {
		if IsSecretName(n) {
			t.Errorf("IsSecretName(%q) = true", n)
		}
	}
}

// The user's diff config (no prefixes, color, rename and copy detection, an
// external diff tool) must not change the patch.
func TestBuildIgnoresDiffConfig(t *testing.T) {
	origin, work := dirtyRepo(t)
	for _, kv := range [][2]string{{"diff.noprefix", "true"}, {"color.diff", "always"}, {"color.ui", "always"},
		{"diff.renames", "copies"}, {"diff.external", "false"}, {"diff.mnemonicPrefix", "true"}} {
		gitT(t, work, "config", kv[0], kv[1])
	}
	data, _ := buildFor(t, work, newClaudeDir(t, testSessionID))
	b, err := Extract(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b.Patch, []byte("diff --git a/a.txt b/a.txt\n")) || bytes.Contains(b.Patch, []byte("\x1b[")) {
		t.Fatalf("patch shaped by the user's config:\n%s", b.Patch)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	gitT(t, filepath.Dir(fresh), "clone", "-q", origin, fresh)
	patch := filepath.Join(t.TempDir(), "changes.patch")
	if err := os.WriteFile(patch, b.Patch, 0o600); err != nil {
		t.Fatal(err)
	}
	gitT(t, fresh, "apply", "--binary", patch)
	gitT(t, fresh, "apply", "-R", "--binary", patch) // full object IDs make the binary hunk reversible
	if st := gitT(t, fresh, "status", "--porcelain"); st != "" {
		t.Errorf("apply then apply -R left:\n%s", st)
	}
}

func TestBuildLimits(t *testing.T) {
	ctx := context.Background()
	_, work := newRepo(t)
	claude := newClaudeDir(t, testSessionID)
	repo, err := ReadRepo(ctx, work, nil)
	if err != nil {
		t.Fatal(err)
	}
	in := BuildInput{MoveID: "mv_AAAAAAAAAAAAAAAAAAAAAA", SessionID: testSessionID, Machine: "bluebox", CWD: work,
		ClaudeDir: claude, Repo: repo, Now: bundleNow}

	oldFile, oldSealed := maxUntrackedFile, maxSealed
	t.Cleanup(func() { maxUntrackedFile, maxSealed = oldFile, oldSealed })

	maxUntrackedFile = 16
	writeFile(t, filepath.Join(work, "big.bin"), strings.Repeat("x", 17), 0o644)
	writeFile(t, filepath.Join(work, "small.txt"), "ok", 0o644)
	if _, _, err := Build(ctx, in); err == nil || !strings.Contains(err.Error(), "big.bin") || strings.Contains(err.Error(), "small.txt") {
		t.Errorf("file limit: %v, want an error naming big.bin only", err)
	}
	maxUntrackedFile = oldFile

	oldBundle := maxBundle
	t.Cleanup(func() { maxBundle = oldBundle })
	maxBundle = 4096
	writeFile(t, filepath.Join(claude, "projects", "-home-user-proj", testSessionID+".jsonl"), strings.Repeat("y", 8192), 0o600)
	if _, _, err := Build(ctx, in); err == nil || !strings.Contains(err.Error(), "limit") ||
		!strings.Contains(err.Error(), "transcript/"+testSessionID+".jsonl") {
		t.Errorf("bundle limit: %v, want an error naming the transcript", err)
	}

	// The sizes are checked before any file is read: an unreadable file over
	// the limit fails on the limit, not on the read.
	hist := filepath.Join(claude, "file-history", testSessionID, "big@v1")
	writeFile(t, hist, strings.Repeat("z", 8192), 0o000)
	if _, _, err := Build(ctx, in); err == nil || !strings.Contains(err.Error(), "limit") ||
		!strings.Contains(err.Error(), "file-history/"+testSessionID+"/big@v1") {
		t.Errorf("bundle limit before reading: %v, want an error naming the file history", err)
	}
	if err := os.Remove(hist); err != nil {
		t.Fatal(err)
	}

	// The sealed limit applies to the compressed bundle: 8 KiB of repeats
	// fits in 4 KiB, 8 KiB of random bytes does not.
	maxBundle, maxSealed = oldBundle, 4096
	if _, _, err := Build(ctx, in); err != nil {
		t.Errorf("compressible bundle under the sealed limit: %v", err)
	}
	noise := make([]byte, 8192)
	rand.Read(noise)
	writeFile(t, filepath.Join(claude, "projects", "-home-user-proj", testSessionID+".jsonl"), string(noise), 0o600)
	if _, _, err := Build(ctx, in); err == nil || !strings.Contains(err.Error(), "compressed bundle") ||
		!strings.Contains(err.Error(), "transcript/"+testSessionID+".jsonl") {
		t.Errorf("sealed limit: %v, want an error naming the compressed bundle and the transcript", err)
	}

	// Two transcripts with one ID is refused.
	maxSealed = oldSealed
	writeFile(t, filepath.Join(claude, "projects", "-other", testSessionID+".jsonl"), "x\n", 0o600)
	if _, _, err := Build(ctx, in); err == nil || !strings.Contains(err.Error(), "found 2 transcripts") {
		t.Errorf("two transcripts: %v", err)
	}
}

// unzstd decompresses a bundle to its tar.
func unzstd(t *testing.T, data []byte) []byte {
	t.Helper()
	zr, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out, err := zr.DecodeAll(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// zstdOf compresses a tar into a bundle.
func zstdOf(t *testing.T, data []byte) []byte {
	t.Helper()
	zw, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer zw.Close()
	return zw.EncodeAll(data, nil)
}

type tarEntry struct {
	hdr  tar.Header
	data []byte
}

func readEntries(t *testing.T, data []byte) []tarEntry {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	var out []tarEntry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out = append(out, tarEntry{*h, b})
	}
}

func writeEntries(t *testing.T, es []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		h := e.hdr
		h.Size = int64(len(e.data))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		tw.Write(e.data)
	}
	tw.Close()
	return buf.Bytes()
}

func TestExtractRefuses(t *testing.T) {
	_, work := dirtyRepo(t)
	data, _ := buildFor(t, work, newClaudeDir(t, testSessionID))
	base := readEntries(t, unzstd(t, data))
	reg := func(name, body string) tarEntry {
		return tarEntry{tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Format: tar.FormatPAX}, []byte(body)}
	}
	// withListed adds name to the manifest's file list, so only the check
	// under test can refuse it.
	withListed := func(es []tarEntry, name string) []tarEntry {
		var m Manifest
		json.Unmarshal(es[0].data, &m)
		m.Files = append(m.Files, name)
		es[0].data, _ = json.Marshal(m)
		return es
	}
	innerWith := func(name string) []byte {
		return writeEntries(t, []tarEntry{reg(name, "x")})
	}
	// withManifest changes the manifest.
	withManifest := func(edit func(*Manifest)) func([]tarEntry) []tarEntry {
		return func(es []tarEntry) []tarEntry {
			var m Manifest
			json.Unmarshal(es[0].data, &m)
			edit(&m)
			es[0].data, _ = json.Marshal(m)
			return es
		}
	}
	// The whole bundle fits well under 64 KiB, so this limit refuses only
	// the oversized entry.
	oldBundle, oldFile := maxBundle, maxUntrackedFile
	t.Cleanup(func() { maxBundle, maxUntrackedFile = oldBundle, oldFile })
	maxBundle, maxUntrackedFile = 64<<10, 1<<10
	cases := map[string]func([]tarEntry) []tarEntry{
		"move ID":        withManifest(func(m *Manifest) { m.MoveID = "../../x" }),
		"session ID":     withManifest(func(m *Manifest) { m.SessionID = "../x" }),
		"rel path up":    withManifest(func(m *Manifest) { m.RelPath = "../x" }),
		"rel path abs":   withManifest(func(m *Manifest) { m.RelPath = "/etc" }),
		"root rel up":    withManifest(func(m *Manifest) { m.RootRel = "a/../../x" }),
		"root rel abs":   withManifest(func(m *Manifest) { m.RootRel = "/etc" }),
		"branch empty":   withManifest(func(m *Manifest) { m.Branch = "" }),
		"branch option":  withManifest(func(m *Manifest) { m.Branch = "--upload-pack=x" }),
		"branch dots":    withManifest(func(m *Manifest) { m.Branch = "a..b" }),
		"branch space":   withManifest(func(m *Manifest) { m.Branch = "a b" }),
		"branch lock":    withManifest(func(m *Manifest) { m.Branch = "a.lock" }),
		"branch reflog":  withManifest(func(m *Manifest) { m.Branch = "a@{1}" }),
		"branch control": withManifest(func(m *Manifest) { m.Branch = "a\nb" }),
		"head short":     withManifest(func(m *Manifest) { m.Head = "abc123" }),
		"head upper":     withManifest(func(m *Manifest) { m.Head = strings.Repeat("A", 40) }),
		"head option":    withManifest(func(m *Manifest) { m.Head = "--all" }),
		"hardlink": func(es []tarEntry) []tarEntry {
			n := "transcript/" + testSessionID + "/h"
			return append(withListed(es, n), tarEntry{tar.Header{Typeflag: tar.TypeLink, Name: n,
				Linkname: "transcript/" + testSessionID + ".jsonl"}, nil})
		},
		"directory": func(es []tarEntry) []tarEntry {
			n := "transcript/" + testSessionID + "/d"
			return append(withListed(es, n), tarEntry{tar.Header{Typeflag: tar.TypeDir, Name: n, Mode: 0o700}, nil})
		},
		"oversized": func(es []tarEntry) []tarEntry {
			n := "file-history/" + testSessionID + "/huge"
			return append(withListed(es, n), reg(n, strings.Repeat("x", int(maxBundle)+1)))
		},
		"untracked oversized": func(es []tarEntry) []tarEntry {
			es[len(es)-1].data = writeEntries(t, []tarEntry{reg("big.bin", strings.Repeat("x", int(maxUntrackedFile)+1))})
			return withManifest(func(m *Manifest) { m.Untracked = []string{"big.bin"} })(es)
		},
		"traversal": func(es []tarEntry) []tarEntry {
			n := "transcript/" + testSessionID + "/../../escape"
			return append(withListed(es, n), reg(n, "x"))
		},
		"absolute": func(es []tarEntry) []tarEntry { return append(withListed(es, "/etc/x"), reg("/etc/x", "x")) },
		"unknown":  func(es []tarEntry) []tarEntry { return append(withListed(es, "other.txt"), reg("other.txt", "x")) },
		"unlisted": func(es []tarEntry) []tarEntry {
			return append(es, reg("file-history/"+testSessionID+"/extra", "x"))
		},
		"listed but missing": func(es []tarEntry) []tarEntry { return withListed(es, "file-history/"+testSessionID+"/ghost") },
		"duplicate":          func(es []tarEntry) []tarEntry { return append(es, es[len(es)-1]) },
		"link": func(es []tarEntry) []tarEntry {
			n := "transcript/" + testSessionID + "/l"
			return append(withListed(es, n), tarEntry{tar.Header{Typeflag: tar.TypeSymlink, Name: n, Linkname: "/etc/passwd"}, nil})
		},
		"manifest last": func(es []tarEntry) []tarEntry { return append(es[1:], es[0]) },
		"no manifest":   func(es []tarEntry) []tarEntry { return es[1:] },
		"no transcript": func(es []tarEntry) []tarEntry {
			var out []tarEntry
			for _, e := range es {
				if e.hdr.Name != "transcript/"+testSessionID+".jsonl" {
					out = append(out, e)
				}
			}
			return out
		},
		"untracked escape": func(es []tarEntry) []tarEntry {
			es[len(es)-1].data = innerWith("../evil")
			return es
		},
		"untracked into .git": func(es []tarEntry) []tarEntry {
			es[len(es)-1].data = innerWith(".git/hooks/post-checkout")
			return es
		},
		"untracked not in manifest": func(es []tarEntry) []tarEntry {
			es[len(es)-1].data = innerWith("surprise.txt")
			return es
		},
	}
	for name, edit := range cases {
		es := edit(slices.Clone(base))
		if b, err := Extract(zstdOf(t, writeEntries(t, es))); err == nil || b != nil {
			t.Errorf("%s: Extract accepted it", name)
		}
	}
	if _, err := Extract(data); err != nil {
		t.Fatalf("the unedited bundle no longer extracts: %v", err)
	}
	if _, err := Extract([]byte("not a tar")); err == nil {
		t.Error("garbage accepted")
	}
	if _, err := Extract(append(slices.Clone(zstdMagic), "not zstd"...)); err == nil {
		t.Error("bad zstd accepted")
	}
}

// A bundle that unpacks past maxBundle is refused, however small it is.
func TestExtractRefusesBomb(t *testing.T) {
	old := maxBundle
	t.Cleanup(func() { maxBundle = old })
	maxBundle = 1 << 20
	bomb := zstdOf(t, make([]byte, 8<<20))
	if len(bomb) > 4<<10 {
		t.Fatalf("bomb is %d bytes", len(bomb))
	}
	if _, err := Extract(bomb); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("bomb: %v, want an error about the unpacked size", err)
	}
	// A streamed frame doesn't declare its size up front.
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf, zstd.WithWindowSize(1<<17))
	if err != nil {
		t.Fatal(err)
	}
	zw.Write(make([]byte, 8<<20))
	zw.Close()
	if _, err := Extract(buf.Bytes()); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("streamed bomb: %v, want an error about the unpacked size", err)
	}
}

// A version 1 bundle (a plain tar, or a manifest that says 1) is
// ErrOldBundle; a later version is a *NewerBundleError.
func TestExtractVersions(t *testing.T) {
	_, work := dirtyRepo(t)
	data, _ := buildFor(t, work, newClaudeDir(t, testSessionID))
	plain := unzstd(t, data)
	if _, err := Extract(plain); !errors.Is(err, ErrOldBundle) {
		t.Errorf("uncompressed tar: %v, want ErrOldBundle", err)
	}
	withV := func(v int) []byte {
		es := readEntries(t, plain)
		var m Manifest
		json.Unmarshal(es[0].data, &m)
		m.V = v
		es[0].data, _ = json.Marshal(m)
		return zstdOf(t, writeEntries(t, es))
	}
	if _, err := Extract(withV(1)); !errors.Is(err, ErrOldBundle) {
		t.Errorf("manifest version 1: %v, want ErrOldBundle", err)
	}
	var newer *NewerBundleError
	if _, err := Extract(withV(3)); !errors.As(err, &newer) || newer.V != 3 {
		t.Errorf("manifest version 3: %v, want a NewerBundleError", err)
	}
}

// Build must not refresh or rewrite the user's index, even when a tracked
// file's stat data no longer matches it.
func TestBuildLeavesIndex(t *testing.T) {
	_, work := dirtyRepo(t)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(work, "sub", "keep.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	idx := filepath.Join(work, ".git", "index")
	before, err := os.Stat(idx)
	if err != nil {
		t.Fatal(err)
	}
	buildFor(t, work, newClaudeDir(t, testSessionID))
	after, err := os.Stat(idx)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("Build rewrote the index: size %d, mtime %v, then size %d, mtime %v (same file %v)",
			before.Size(), before.ModTime(), after.Size(), after.ModTime(), os.SameFile(before, after))
	}
	if _, err := os.Stat(idx + ".lock"); !os.IsNotExist(err) {
		t.Errorf("index.lock left behind: %v", err)
	}
}

// Links in the sidecar folder, and a linked file-history folder, are listed
// as skipped, not followed.
func TestBuildSkipsTreeLinks(t *testing.T) {
	_, work := newRepo(t)
	claude := newClaudeDir(t, testSessionID)
	outside := filepath.Join(t.TempDir(), "outside")
	writeFile(t, filepath.Join(outside, "leak@v1"), "OUTSIDE-DATA", 0o600)
	side := filepath.Join(claude, "projects", "-home-user-proj", testSessionID)
	if err := os.Symlink(filepath.Join(outside, "leak@v1"), filepath.Join(side, "tool-results", "l.txt")); err != nil {
		t.Fatal(err)
	}
	hist := filepath.Join(claude, "file-history", testSessionID)
	if err := os.RemoveAll(hist); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, hist); err != nil {
		t.Fatal(err)
	}
	data, man := buildFor(t, work, claude)
	skipped := slices.Clone(man.Skipped)
	slices.Sort(skipped)
	if want := []string{"file-history/" + testSessionID, "transcript/" + testSessionID + "/tool-results/l.txt"}; !slices.Equal(skipped, want) {
		t.Errorf("skipped %q, want %q", skipped, want)
	}
	if bytes.Contains(data, []byte("OUTSIDE-DATA")) {
		t.Error("bundle followed a link")
	}
}
