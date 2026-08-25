package keys

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"testing"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// ---------------------------------------------------------------------------
// Interop oracle.
//
// refWrap/refUnwrap are an INDEPENDENT transcription of the cloud plugin's
// key-wrap algorithm from the canonical remote-plugin wire-format contract:
//
//	wire = [32-byte ephemeral pub] || [12-byte nonce] || [AES-GCM-256 ct+tag]
//	shared = X25519(ephemeral, devicePub)
//	wrapKey = HKDF-SHA256(shared, salt=ephemeralPub||devicePub, info="aplexica/v1/wrap")
//	ct = AES-GCM-256(wrapKey, nonce, contentKey, aad=ephemeralPub)
//
// The production code in this package MUST interoperate with these, byte for
// byte — that is the cross-repo contract that lets a surviving device unwrap a
// key the daemon wrapped (and vice versa).
// ---------------------------------------------------------------------------

func refWrap(t *testing.T, contentKey []byte, devicePub [32]byte) []byte {
	t.Helper()
	var ephemeral [32]byte
	if _, err := rand.Read(ephemeral[:]); err != nil {
		t.Fatalf("ephemeral rand: %v", err)
	}
	ephemeral[0] &= 0xF8
	ephemeral[31] = (ephemeral[31] & 0x7F) | 0x40
	ephemeralPub, err := curve25519.X25519(ephemeral[:], curve25519.Basepoint)
	if err != nil {
		t.Fatalf("ephemeral pub: %v", err)
	}
	shared, err := curve25519.X25519(ephemeral[:], devicePub[:])
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}
	salt := append(append([]byte{}, ephemeralPub...), devicePub[:]...)
	wrapKey := make([]byte, 32)
	r := hkdf.New(sha256.New, shared, salt, []byte("aplexica/v1/wrap"))
	if _, err := io.ReadFull(r, wrapKey); err != nil {
		t.Fatalf("hkdf: %v", err)
	}
	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce rand: %v", err)
	}
	ct := gcm.Seal(nil, nonce, contentKey, ephemeralPub)
	out := make([]byte, 0, 32+gcm.NonceSize()+len(ct))
	out = append(out, ephemeralPub...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out
}

func refUnwrap(t *testing.T, wrapped []byte, devicePriv [32]byte) ([]byte, bool) {
	t.Helper()
	if len(wrapped) < 32+12+ContentKeySize {
		return nil, false
	}
	ephemeralPub := wrapped[:32]
	nonce := wrapped[32:44]
	ct := wrapped[44:]
	shared, err := curve25519.X25519(devicePriv[:], ephemeralPub)
	if err != nil {
		return nil, false
	}
	devicePub, err := curve25519.X25519(devicePriv[:], curve25519.Basepoint)
	if err != nil {
		return nil, false
	}
	salt := append(append([]byte{}, ephemeralPub...), devicePub...)
	wrapKey := make([]byte, 32)
	r := hkdf.New(sha256.New, shared, salt, []byte("aplexica/v1/wrap"))
	if _, err := io.ReadFull(r, wrapKey); err != nil {
		return nil, false
	}
	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		return nil, false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, false
	}
	pt, err := gcm.Open(nil, nonce, ct, ephemeralPub)
	if err != nil {
		return nil, false
	}
	return pt, true
}

// ---------------------------------------------------------------------------

func TestNewContentKey_IsRandom32Bytes(t *testing.T) {
	a, err := NewContentKey()
	if err != nil {
		t.Fatalf("NewContentKey: %v", err)
	}
	if len(a) != ContentKeySize {
		t.Fatalf("len = %d, want %d", len(a), ContentKeySize)
	}
	b, err := NewContentKey()
	if err != nil {
		t.Fatalf("NewContentKey: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two content keys are identical — not random")
	}
}

func TestWrapUnwrap_RoundTrip(t *testing.T) {
	priv, pub, err := NewDeviceKey()
	if err != nil {
		t.Fatalf("NewDeviceKey: %v", err)
	}
	ck, err := NewContentKey()
	if err != nil {
		t.Fatalf("NewContentKey: %v", err)
	}

	wrapped, err := WrapContentKey(ck, pub)
	if err != nil {
		t.Fatalf("WrapContentKey: %v", err)
	}

	got, err := UnwrapContentKey(wrapped, priv)
	if err != nil {
		t.Fatalf("UnwrapContentKey: %v", err)
	}
	if !bytes.Equal(got, ck) {
		t.Fatalf("round-trip mismatch: got %x want %x", got, ck)
	}
}

func TestWrap_WireFormatLayout(t *testing.T) {
	_, pub, _ := NewDeviceKey()
	ck, _ := NewContentKey()
	wrapped, err := WrapContentKey(ck, pub)
	if err != nil {
		t.Fatalf("WrapContentKey: %v", err)
	}
	// 32 (ephemeral pub) + 12 (nonce) + 32 (content key) + 16 (GCM tag).
	const wantLen = 32 + 12 + 32 + 16
	if len(wrapped) != wantLen {
		t.Fatalf("wrapped len = %d, want %d", len(wrapped), wantLen)
	}
	// First 32 bytes must be a non-zero curve point (the ephemeral pub).
	zero := make([]byte, 32)
	if bytes.Equal(wrapped[:32], zero) {
		t.Fatal("ephemeral pub is all-zero")
	}
}

// TestWrap_UnwrappableByReferenceImpl proves the daemon's wrap output is
// byte-compatible with the cloud plugin's unwrap (the surviving device's
// real decrypt path).
func TestWrap_UnwrappableByReferenceImpl(t *testing.T) {
	priv, pub, _ := NewDeviceKey()
	ck, _ := NewContentKey()

	wrapped, err := WrapContentKey(ck, pub)
	if err != nil {
		t.Fatalf("WrapContentKey: %v", err)
	}
	got, ok := refUnwrap(t, wrapped, priv)
	if !ok {
		t.Fatal("reference unwrap rejected daemon-wrapped key")
	}
	if !bytes.Equal(got, ck) {
		t.Fatalf("reference unwrap mismatch: got %x want %x", got, ck)
	}
}

// TestUnwrap_AcceptsReferenceWrappedKey proves the daemon can unwrap what the
// cloud plugin wrapped (the inbound broadcast path on a surviving device).
func TestUnwrap_AcceptsReferenceWrappedKey(t *testing.T) {
	priv, pub, _ := NewDeviceKey()
	ck, _ := NewContentKey()

	wrapped := refWrap(t, ck, pub)
	got, err := UnwrapContentKey(wrapped, priv)
	if err != nil {
		t.Fatalf("UnwrapContentKey rejected reference-wrapped key: %v", err)
	}
	if !bytes.Equal(got, ck) {
		t.Fatalf("unwrap mismatch: got %x want %x", got, ck)
	}
}

func TestUnwrap_WrongDeviceKeyFails(t *testing.T) {
	_, pub, _ := NewDeviceKey()
	otherPriv, _, _ := NewDeviceKey()
	ck, _ := NewContentKey()

	wrapped, err := WrapContentKey(ck, pub)
	if err != nil {
		t.Fatalf("WrapContentKey: %v", err)
	}
	if _, err := UnwrapContentKey(wrapped, otherPriv); err == nil {
		t.Fatal("expected unwrap with wrong device key to fail")
	}
}

func TestUnwrap_TamperedCiphertextFails(t *testing.T) {
	priv, pub, _ := NewDeviceKey()
	ck, _ := NewContentKey()
	wrapped, err := WrapContentKey(ck, pub)
	if err != nil {
		t.Fatalf("WrapContentKey: %v", err)
	}
	// Flip a bit in the ciphertext region (after ephemeral pub + nonce).
	wrapped[60] ^= 0x01
	if _, err := UnwrapContentKey(wrapped, priv); err == nil {
		t.Fatal("expected unwrap of tampered ciphertext to fail")
	}
}

func TestUnwrap_ShortInputFails(t *testing.T) {
	priv, _, _ := NewDeviceKey()
	if _, err := UnwrapContentKey([]byte("too short"), priv); err == nil {
		t.Fatal("expected unwrap of short input to fail")
	}
}

func TestWrap_RejectsWrongContentKeySize(t *testing.T) {
	_, pub, _ := NewDeviceKey()
	if _, err := WrapContentKey([]byte("not 32 bytes"), pub); err == nil {
		t.Fatal("expected wrap of wrong-size content key to fail")
	}
}
