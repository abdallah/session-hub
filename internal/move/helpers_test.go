package move

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testSessionID looks like a Claude session UUID.
const testSessionID = "11111111-2222-4333-8444-555555555555"

// gitIsolate keeps the developer's Git config (signing, hooks, aliases) out
// of the test and gives commits a fixed identity.
func gitIsolate(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Hub Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "sessionhub@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "Hub Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "sessionhub@example.test")
}

// gitT runs git in dir and fails the test on an error.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeFile writes data to path with mode, creating parent directories.
func writeFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// newRepo makes a bare origin and a clone of it with one pushed commit on
// main: a.txt, bin.dat (binary), gone.txt, sub/keep.txt, and a .gitignore
// that ignores build/.
func newRepo(t *testing.T) (origin, work string) {
	t.Helper()
	gitIsolate(t)
	base := t.TempDir()
	origin = filepath.Join(base, "origin.git")
	gitT(t, base, "init", "-q", "--bare", "-b", "main", origin)
	work = filepath.Join(base, "work")
	gitT(t, base, "clone", "-q", origin, work)
	writeFile(t, filepath.Join(work, "a.txt"), "one\n", 0o644)
	writeFile(t, filepath.Join(work, "bin.dat"), "\x00\x01\x02\x03binary", 0o644)
	writeFile(t, filepath.Join(work, "gone.txt"), "delete me\n", 0o644)
	writeFile(t, filepath.Join(work, "sub", "keep.txt"), "keep\n", 0o644)
	writeFile(t, filepath.Join(work, ".gitignore"), "build/\n", 0o644)
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "-q", "-m", "initial")
	gitT(t, work, "push", "-q", "-u", "origin", "main")
	return origin, work
}

// newClaudeDir makes a temp ~/.claude with session id's transcript in a
// project folder, a sidecar folder (subagents/ and tool-results/), and file
// history.
func newClaudeDir(t *testing.T, id string) string {
	t.Helper()
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "-home-user-proj")
	writeFile(t, filepath.Join(proj, id+".jsonl"), `{"type":"user","message":"hello"}`+"\n", 0o600)
	writeFile(t, filepath.Join(proj, id, "subagents", "agent-1.jsonl"), `{"type":"assistant"}`+"\n", 0o600)
	writeFile(t, filepath.Join(proj, id, "tool-results", "r1.txt"), "tool output\n", 0o600)
	writeFile(t, filepath.Join(dir, "file-history", id, "abc@v1"), "snapshot\n", 0o600)
	return dir
}
