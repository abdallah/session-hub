// Package move carries a Claude Code session to another machine or to the
// cloud: the machine key pair, the sealed bundle, the Git work on both ends,
// the transcript files, and the cloud hand-off. See docs/client.md, "Moves".
package move

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/paths"
)

// KeyPath is move.key next to the client config: ~/.config/sessionhub/move.key, or
// beside $SESSIONHUB_CONFIG.
func KeyPath() string { return filepath.Join(filepath.Dir(paths.ClientConfig()), "move.key") }

// LoadOrCreateKey reads this machine's X25519 private key from path, or
// creates it (mode 0600) when the file is missing. The new key is written to
// a temp file and linked into place, so a reader never sees half a key and a
// second process that loses the race reads the winner's key. A file others
// can read, or one that holds no key, is an error: sessionhub never replaces it.
func LoadOrCreateKey(path string) (*ecdh.PrivateKey, error) {
	k, err := readKey(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return k, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	k, err = ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".move-key-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.WriteString(base64.StdEncoding.EncodeToString(k.Bytes()) + "\n"); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(tmp.Name(), path); errors.Is(err, fs.ErrExist) {
		return readKey(path)
	} else if err != nil {
		return nil, err
	}
	return k, nil
}

func readKey(path string) (*ecdh.PrivateKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("move key %s has mode %04o; others must not read it: run chmod 600 %s", path, perm, path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("move key %s is owned by another user; move it aside to make a new one", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("move key %s does not hold a key; move it aside to make a new one", path)
	}
	return ecdh.X25519().NewPrivateKey(raw)
}

// PublicKeyString is k's public key as the server stores it: standard
// base64 of 32 bytes.
func PublicKeyString(k *ecdh.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
}

// ParsePublicKey reads a public key in the server's form.
func ParsePublicKey(s string) (*ecdh.PublicKey, error) {
	raw, err := api.ParseMoveKey(s)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPublicKey(raw)
}

// Fingerprint is the key's short form for checking it out of band.
func Fingerprint(pub *ecdh.PublicKey) string { return api.MoveKeyFingerprint(pub.Bytes()) }
