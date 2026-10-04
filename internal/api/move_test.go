package api

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func TestMoveOpen(t *testing.T) {
	for state, want := range map[string]bool{
		MoveRequested: true, MovePacking: true, MoveUploaded: true, MoveUnpacking: true,
		MoveDone: false, MoveFailed: false, MoveCancelled: false, "": false, "other": false,
	} {
		if got := MoveOpen(state); got != want {
			t.Errorf("MoveOpen(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestParseMoveKey(t *testing.T) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	good := base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
	raw, err := ParseMoveKey(good)
	if err != nil || len(raw) != 32 {
		t.Fatalf("ParseMoveKey(good) = %d bytes, %v", len(raw), err)
	}
	for _, bad := range []string{
		"",
		"not base64!",
		base64.StdEncoding.EncodeToString(make([]byte, 31)),
		base64.StdEncoding.EncodeToString(make([]byte, 33)),
		base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
		good + "\n",
		" " + good,
	} {
		if _, err := ParseMoveKey(bad); err == nil {
			t.Errorf("ParseMoveKey(%q) accepted", bad)
		}
	}
}

func TestMoveKeyFingerprint(t *testing.T) {
	// SHA-256 of 32 zero bytes is 66687aadf862bd776c8fc18b8e9f8e20...
	if got := MoveKeyFingerprint(make([]byte, 32)); got != "66687aadf862bd77" {
		t.Errorf("fingerprint = %q", got)
	}
	if got := MoveKeyFingerprint(nil); len(got) != 16 || strings.Trim(got, "0123456789abcdef") != "" {
		t.Errorf("fingerprint of nothing = %q", got)
	}
}
