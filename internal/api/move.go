package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

// Moving a session to another machine or to the cloud. See docs/server.md,
// "Moves".
const (
	// HeaderActionMove is the X-Hub-Action value the dashboard sends with
	// POST /v1/sessions/{id}/move.
	HeaderActionMove = "move"

	// Control claim actions. move-out goes to the source machine (to pack, or
	// to finish once the move ended); move-in goes to the target machine.
	ActionMoveOut = "move-out"
	ActionMoveIn  = "move-in"

	// KindMoved is the event the server records when a move is done.
	KindMoved = "moved"

	// MoveCloud is the target of a move to a Claude Code cloud session.
	MoveCloud = "cloud"

	MoveRequested = "requested"
	MovePacking   = "packing"
	MoveUploaded  = "uploaded"
	MoveUnpacking = "unpacking"
	MoveDone      = "done"
	MoveFailed    = "failed"
	// MoveCancelled is defined for readers; no route sets it yet. Treat it
	// like MoveFailed.
	MoveCancelled = "cancelled"

	// MaxMoveDetailRunes caps MoveResultIn.Detail.
	MaxMoveDetailRunes = 300
	// MaxSealedBundle caps a sealed bundle, in bytes.
	MaxSealedBundle = 64 << 20

	// EndedMovedToCloud is the reason of the ended event a cloud move records.
	EndedMovedToCloud = "moved_to_cloud"
)

// MoveIn is the body of POST /v1/sessions/{id}/move: a machine name or
// "cloud".
type MoveIn struct {
	Target string `json:"target"`
}

// MoveKeyIn is the body of PUT /v1/machines/self/move-key: the machine's
// X25519 public key in standard base64.
type MoveKeyIn struct {
	PublicKey string `json:"public_key"`
}

// MoveResultIn is the body of POST /v1/moves/{id}/result: done or failed.
// CloudURL is set on a cloud move's done only.
type MoveResultIn struct {
	State    string `json:"state"`
	Detail   string `json:"detail,omitempty"`
	CloudURL string `json:"cloud_url,omitempty"`
}

// Move is one move of a session. Source and Target are machine names;
// Target is MoveCloud for a cloud move. By is web:<name> or machine:<name>.
type Move struct {
	ID         string    `json:"id"`
	SessionID  string    `json:"session_id"`
	Source     string    `json:"source"`
	Target     string    `json:"target"`
	State      string    `json:"state"`
	Detail     string    `json:"detail,omitempty"`
	CloudURL   string    `json:"cloud_url,omitempty"`
	By         string    `json:"by"`
	BundleSize int64     `json:"bundle_size"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// MoveClaim is the move part of a control claim. State is packing (pack and
// upload, or hand off to the cloud), unpacking (download and resume), or a
// final state (the source's finish: archive on done, restart otherwise).
// PeerKey is the other machine's public key: the target's on a pack, the
// source's on an unpack.
type MoveClaim struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	Target  string `json:"target"`
	State   string `json:"state"`
	PeerKey string `json:"peer_key,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// MoveOpen reports whether a move in state is still under way.
func MoveOpen(state string) bool {
	switch state {
	case MoveRequested, MovePacking, MoveUploaded, MoveUnpacking:
		return true
	}
	return false
}

// moveKeySize is the size of an X25519 public key.
const moveKeySize = 32

// ParseMoveKey decodes a move public key: standard base64 of exactly 32
// bytes, with nothing around it.
func ParseMoveKey(s string) ([]byte, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || len(raw) != moveKeySize || base64.StdEncoding.EncodeToString(raw) != s {
		return nil, errors.New("public_key: want 32 bytes in standard base64")
	}
	return raw, nil
}

// MoveKeyFingerprint is the first 16 hex characters of the SHA-256 of a raw
// public key, as sessionhub machine ls and sessionhub move-key print it.
func MoveKeyFingerprint(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}
