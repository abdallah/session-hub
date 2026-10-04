package move

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"testing"
)

func newKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpen(t *testing.T) {
	src, dst, other := newKey(t), newKey(t), newKey(t)
	const id = "mv_AAAAAAAAAAAAAAAAAAAAAA"
	msg := bytes.Repeat([]byte("transcript line\n"), 1000)
	sealed, err := Seal(src, dst.PublicKey(), id, msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) != len(msg)+SealOverhead || sealed[0] != 1 {
		t.Fatalf("sealed size %d (want %d), version %d", len(sealed), len(msg)+SealOverhead, sealed[0])
	}
	if bytes.Contains(sealed, []byte("transcript line")) {
		t.Fatal("sealed bundle contains plaintext")
	}
	got, err := Open(dst, src.PublicKey(), id, sealed)
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("Open: %v, equal %v", err, bytes.Equal(got, msg))
	}
	again, _ := Seal(src, dst.PublicKey(), id, msg)
	if bytes.Equal(again, sealed) {
		t.Error("two seals of one bundle are identical; the ephemeral key or nonce is reused")
	}

	fails := map[string]func() ([]byte, error){
		"wrong target key": func() ([]byte, error) { return Open(other, src.PublicKey(), id, sealed) },
		"wrong source key": func() ([]byte, error) { return Open(dst, other.PublicKey(), id, sealed) },
		"other move id":    func() ([]byte, error) { return Open(dst, src.PublicKey(), "mv_BBBBBBBBBBBBBBBBBBBBBB", sealed) },
		"truncated":        func() ([]byte, error) { return Open(dst, src.PublicKey(), id, sealed[:SealOverhead-1]) },
		"empty":            func() ([]byte, error) { return Open(dst, src.PublicKey(), id, nil) },
	}
	for _, at := range []int{0, 1, 32, 33, 44, 45, len(sealed) / 2, len(sealed) - 1} {
		flipped := bytes.Clone(sealed)
		flipped[at] ^= 0x01
		fails["changed byte "+itoa(at)] = func() ([]byte, error) { return Open(dst, src.PublicKey(), id, flipped) }
	}
	// X25519 ignores the top bit of the ephemeral key, so the flip must fail
	// through the additional data. A non-canonical encoding of the same
	// point (x + 2^255 - 19 is out of range; use x with the top bit set and
	// the all-ones value) must fail too.
	topBit := bytes.Clone(sealed)
	topBit[32] ^= 0x80
	fails["ephemeral top bit"] = func() ([]byte, error) { return Open(dst, src.PublicKey(), id, topBit) }
	nonCanon := bytes.Clone(sealed)
	for i := 1; i <= 32; i++ {
		nonCanon[i] = 0xff
	}
	nonCanon[32] = 0x7f
	fails["non-canonical ephemeral"] = func() ([]byte, error) { return Open(dst, src.PublicKey(), id, nonCanon) }
	for name, open := range fails {
		if out, err := open(); !errors.Is(err, ErrOpen) || out != nil {
			t.Errorf("%s: %v, %d bytes; want ErrOpen and nothing", name, err, len(out))
		}
	}
	if _, err := Seal(src, dst.PublicKey(), "", msg); err == nil {
		t.Error("Seal with an empty move ID succeeded")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
