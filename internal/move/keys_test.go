package move

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

func TestKeyPathFollowsHubConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(dir, "config.toml"))
	if got, want := KeyPath(), filepath.Join(dir, "move.key"); got != want {
		t.Errorf("KeyPath() = %q, want %q", got, want)
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "move.key")
	k, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v; want 0600", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(data), "\n") || len(strings.TrimSpace(string(data))) != 44 {
		t.Errorf("key file %q: want 44 base64 characters and a newline", data)
	}
	again, err := LoadOrCreateKey(path)
	if err != nil || !bytes.Equal(again.Bytes(), k.Bytes()) {
		t.Fatalf("second load: %v; same key %v", err, err == nil && bytes.Equal(again.Bytes(), k.Bytes()))
	}
	pub := PublicKeyString(k)
	parsed, err := ParsePublicKey(pub)
	if err != nil || !bytes.Equal(parsed.Bytes(), k.PublicKey().Bytes()) {
		t.Fatalf("ParsePublicKey(PublicKeyString(k)): %v", err)
	}
	if got, want := Fingerprint(parsed), api.MoveKeyFingerprint(k.PublicKey().Bytes()); got != want || len(got) != 16 {
		t.Errorf("Fingerprint = %q, want %q", got, want)
	}

	// A key file others can read is refused, not used or replaced.
	for _, mode := range []os.FileMode{0o644, 0o640} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateKey(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %04o: %v, want a chmod 600 hint", mode, err)
		}
	}
	// A file that holds no key is refused and kept.
	bad := filepath.Join(t.TempDir(), "move.key")
	if err := os.WriteFile(bad, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(bad); err == nil {
		t.Error("garbage key file accepted")
	}
	if b, _ := os.ReadFile(bad); string(b) != "hello\n" {
		t.Errorf("garbage key file rewritten: %q", b)
	}
}

func TestLoadOrCreateKeyRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "move.key")
	const n = 8
	keys := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := LoadOrCreateKey(path)
			errs[i] = err
			if err == nil {
				keys[i] = PublicKeyString(k)
			}
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || keys[i] != keys[0] {
			t.Fatalf("racer %d: %v, key %q vs %q", i, errs[i], keys[i], keys[0])
		}
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".move-key-*")); len(m) != 0 {
		t.Errorf("temp files left: %v", m)
	}
}
