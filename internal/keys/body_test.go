package keys

import (
	"bytes"
	"testing"
)

func TestSealOpenBody_RoundTrip(t *testing.T) {
	key, err := NewContentKey()
	if err != nil {
		t.Fatalf("content key: %v", err)
	}
	pt := []byte(`{"eventId":"abc","payload":"secret memory content"}`)
	nonce, ct, err := SealBody(key, pt)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if len(nonce) != BodyNonceSize {
		t.Fatalf("nonce len = %d, want %d", len(nonce), BodyNonceSize)
	}
	// Ciphertext must not contain the plaintext marker.
	if bytes.Contains(ct, []byte("secret memory content")) {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := OpenBody(key, nonce, ct)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round-trip mismatch: %q != %q", got, pt)
	}
}

func TestOpenBody_TamperFails(t *testing.T) {
	key, _ := NewContentKey()
	nonce, ct, err := SealBody(key, []byte("hello"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	ct[0] ^= 0xFF // flip a bit
	if _, err := OpenBody(key, nonce, ct); err == nil {
		t.Fatal("expected auth failure on tampered ciphertext")
	}
}

func TestOpenBody_WrongKeyFails(t *testing.T) {
	key, _ := NewContentKey()
	other, _ := NewContentKey()
	nonce, ct, err := SealBody(key, []byte("hello"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := OpenBody(other, nonce, ct); err == nil {
		t.Fatal("expected auth failure with wrong key")
	}
}

func TestSealBody_RejectsBadKeyLen(t *testing.T) {
	if _, _, err := SealBody([]byte("short"), []byte("x")); err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
}
