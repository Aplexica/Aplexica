package keys

import (
	"crypto/rand"
	"errors"
	"fmt"
)

// BodyNonceSize is the AES-GCM nonce length used by SealBody/OpenBody (12
// bytes, the GCM standard — same as the key-wrap nonce).
const BodyNonceSize = nonceSize

// ErrBodyAuthenticationFailed is returned by OpenBody on any decryption
// failure — wrong key, tampered ciphertext, or malformed input.
var ErrBodyAuthenticationFailed = errors.New("keys: body authentication failed")

// SealBody AES-GCM-256 encrypts plaintext under a 32-byte content key,
// returning a fresh random nonce and the ciphertext-with-tag. It is the
// symmetric body cipher for the per-event E2E envelope: the content key is
// itself wrapped per-recipient via WrapContentKey, so only authorised devices
// recover it and can then OpenBody the artifact event.
//
// No additional authenticated data is bound here — the envelope's wrapped-key
// set already authenticates the recipient, and the canonical event carries its
// own hash chain. Callers who want the nonce + ciphertext to travel together
// concatenate them in the envelope (this package leaves layout to the caller).
func SealBody(contentKey, plaintext []byte) (nonce, ciphertext []byte, err error) {
	if len(contentKey) != ContentKeySize {
		return nil, nil, fmt.Errorf("keys: body key must be %d bytes, got %d", ContentKeySize, len(contentKey))
	}
	gcm, err := newGCM(contentKey)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, BodyNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("keys: body nonce rand: %w", err)
	}
	ciphertext = gcm.Seal(nil, nonce, plaintext, nil)
	return nonce, ciphertext, nil
}

// OpenBody reverses SealBody. Returns ErrBodyAuthenticationFailed on any
// failure so callers can skip an event they cannot decrypt without leaking
// which check failed.
func OpenBody(contentKey, nonce, ciphertext []byte) ([]byte, error) {
	if len(contentKey) != ContentKeySize || len(nonce) != BodyNonceSize {
		return nil, ErrBodyAuthenticationFailed
	}
	gcm, err := newGCM(contentKey)
	if err != nil {
		return nil, ErrBodyAuthenticationFailed
	}
	pt, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrBodyAuthenticationFailed
	}
	return pt, nil
}
