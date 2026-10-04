package move

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

const (
	sealVersion = 1
	ephSize     = 32
	nonceSize   = 12
	tagSize     = 16
	// SealOverhead is how many bytes Seal adds: the version byte, the
	// ephemeral public key, the nonce, and the GCM tag.
	SealOverhead = 1 + ephSize + nonceSize + tagSize
	hkdfInfo     = "sessionhub move v1"
)

// ErrOpen is every Open failure. It never says which check failed.
var ErrOpen = errors.New("the bundle did not open: it was not sealed by the source machine for this one, or it changed on the way")

// sealAEAD derives the AES-256-GCM key from both shared secrets, with the
// move ID as the salt.
func sealAEAD(shared1, shared2 []byte, moveID string) (cipher.AEAD, error) {
	secret := make([]byte, 0, len(shared1)+len(shared2))
	secret = append(append(secret, shared1...), shared2...)
	key, err := hkdf.Key(sha256.New, secret, []byte(moveID), hkdfInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealAAD is the GCM additional data: the move ID, then the ephemeral public
// key as sent. X25519 ignores the key's top bit and reduces non-canonical
// values, so binding the exact bytes stops a changed key from opening.
func sealAAD(moveID string, eph []byte) []byte {
	return append([]byte(moveID), eph...)
}

// Seal encrypts plaintext for recipient. An ephemeral key pair gives
// forward secrecy; the second ECDH, between sender's static key and the
// recipient, makes only sender able to produce a bundle the recipient opens
// with sender's public key. The move ID is the HKDF salt and the GCM
// additional data (with the ephemeral public key), so a bundle opens only for its own move.
func Seal(sender *ecdh.PrivateKey, recipient *ecdh.PublicKey, moveID string, plaintext []byte) ([]byte, error) {
	if moveID == "" {
		return nil, errors.New("seal: empty move ID")
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	s1, err := eph.ECDH(recipient)
	if err != nil {
		return nil, err
	}
	s2, err := sender.ECDH(recipient)
	if err != nil {
		return nil, err
	}
	aead, err := sealAEAD(s1, s2, moveID)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, SealOverhead+len(plaintext))
	out = append(out, sealVersion)
	out = append(out, eph.PublicKey().Bytes()...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, sealAAD(moveID, eph.PublicKey().Bytes())), nil
}

// Open decrypts a sealed bundle with the recipient's private key and the
// sender's registered public key. Every failure is ErrOpen.
func Open(recipient *ecdh.PrivateKey, sender *ecdh.PublicKey, moveID string, sealed []byte) ([]byte, error) {
	if len(sealed) < SealOverhead || sealed[0] != sealVersion || moveID == "" {
		return nil, ErrOpen
	}
	eph, err := ecdh.X25519().NewPublicKey(sealed[1 : 1+ephSize])
	if err != nil {
		return nil, ErrOpen
	}
	s1, err := recipient.ECDH(eph)
	if err != nil {
		return nil, ErrOpen
	}
	s2, err := recipient.ECDH(sender)
	if err != nil {
		return nil, ErrOpen
	}
	aead, err := sealAEAD(s1, s2, moveID)
	if err != nil {
		return nil, ErrOpen
	}
	nonce := sealed[1+ephSize : 1+ephSize+nonceSize]
	out, err := aead.Open(nil, nonce, sealed[1+ephSize+nonceSize:], sealAAD(moveID, sealed[1:1+ephSize]))
	if err != nil {
		return nil, ErrOpen
	}
	return out, nil
}
