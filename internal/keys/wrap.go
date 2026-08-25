// Package keys implements the daemon-side content-key wrapping primitive
// used by namespace key rotation.
//
// WIRE-FORMAT CONTRACT — this MUST stay byte-compatible with every remote
// plugin implementation, because a key wrapped by one surviving device is
// unwrapped by another implementation, and vice versa:
//
//	wire = [32-byte ephemeral X25519 pub] || [12-byte nonce] || [AES-GCM-256 ct+tag]
//
// Wrapping derives an ECDH shared secret between a fresh ephemeral keypair
// and the recipient device's static X25519 public key, runs it through
// HKDF-SHA256 (salt = ephemeralPub||devicePub, info = "aplexica/v1/wrap")
// to get a 32-byte AES key, then AES-GCM-256 encrypts the content key with
// the ephemeral pub as additional authenticated data.
//
// ZERO-KNOWLEDGE: all of this is client-side. The control plane only ever
// stores opaque wrapped blobs keyed by device pubkey id; it never sees the
// plaintext content key. See docs/09-security-and-trust-model.md.
package keys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// X25519KeySize is the byte length of an X25519 key.
const X25519KeySize = 32

// ContentKeySize is the byte length of a namespace content key.
const ContentKeySize = 32

// nonceSize is the AES-GCM nonce length (12 bytes, the GCM standard).
const nonceSize = 12

// wrapKeySize is the AES-256 key length HKDF derives for the GCM wrap.
const wrapKeySize = 32

// X25519 scalar clamping constants per RFC 7748 §5: clear the low 3 bits
// of the first byte, clear the top bit of the last byte, and set its
// second-from-top bit. scalarHighByteIdx is the index of that last byte in
// a 32-byte scalar. These are fixed protocol constants, not tunables.
const (
	clampFirstByteMask = 0xF8
	clampLastByteMask  = 0x7F
	clampLastByteSet   = 0x40
	scalarHighByteIdx  = X25519KeySize - 1
)

// hkdfInfo domain-separates this HKDF use from every other use of a
// shared secret in the key hierarchy. Must match the cloud plugin.
var hkdfInfo = []byte("aplexica/v1/wrap")

// ErrUnwrapAuthenticationFailed is returned by UnwrapContentKey on any
// failure — malformed input, wrong recipient, or tampered ciphertext.
// Callers don't need to distinguish; all mean "this key is not for me or
// not intact."
var ErrUnwrapAuthenticationFailed = errors.New("keys: wrapped content key authentication failed")

// NewContentKey generates a fresh 32-byte random content key.
func NewContentKey() ([]byte, error) {
	k := make([]byte, ContentKeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("keys: rand for content key: %w", err)
	}
	return k, nil
}

// NewDeviceKey generates a fresh X25519 keypair for a device. The private
// key stays on-device; the public key is what other devices wrap content
// keys against. The private scalar is returned in RFC 7748 clamped form so
// callers handing it back to curve25519.X25519 don't need to re-clamp.
func NewDeviceKey() (priv [X25519KeySize]byte, pub [X25519KeySize]byte, err error) {
	var p [X25519KeySize]byte
	if _, e := rand.Read(p[:]); e != nil {
		return priv, pub, fmt.Errorf("keys: rand for device key: %w", e)
	}
	p[0] &= clampFirstByteMask
	p[scalarHighByteIdx] = (p[scalarHighByteIdx] & clampLastByteMask) | clampLastByteSet
	pubKey, e := curve25519.X25519(p[:], curve25519.Basepoint)
	if e != nil {
		return priv, pub, fmt.Errorf("keys: derive device pub: %w", e)
	}
	copy(pub[:], pubKey)
	priv = p
	return priv, pub, nil
}

// WrapContentKey wraps contentKey for the recipient device's X25519 public
// key. Output layout: [32-byte ephemeral pub] || [12-byte nonce] ||
// [AES-GCM-256 ciphertext-with-tag].
func WrapContentKey(contentKey []byte, devicePub [X25519KeySize]byte) ([]byte, error) {
	if len(contentKey) != ContentKeySize {
		return nil, fmt.Errorf("keys: content key must be %d bytes, got %d", ContentKeySize, len(contentKey))
	}

	var ephemeral [X25519KeySize]byte
	if _, err := rand.Read(ephemeral[:]); err != nil {
		return nil, fmt.Errorf("keys: ephemeral rand: %w", err)
	}
	ephemeral[0] &= clampFirstByteMask
	ephemeral[scalarHighByteIdx] = (ephemeral[scalarHighByteIdx] & clampLastByteMask) | clampLastByteSet
	ephemeralPub, err := curve25519.X25519(ephemeral[:], curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("keys: ephemeral pub: %w", err)
	}
	shared, err := curve25519.X25519(ephemeral[:], devicePub[:])
	if err != nil {
		return nil, fmt.Errorf("keys: ecdh: %w", err)
	}

	wrapKey, err := deriveWrapKey(shared, ephemeralPub, devicePub[:])
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(wrapKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("keys: nonce rand: %w", err)
	}
	ct := gcm.Seal(nil, nonce, contentKey, ephemeralPub)

	out := make([]byte, 0, X25519KeySize+nonceSize+len(ct))
	out = append(out, ephemeralPub...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// UnwrapContentKey reverses WrapContentKey using the device's X25519
// private key. Returns ErrUnwrapAuthenticationFailed on any failure.
func UnwrapContentKey(wrapped []byte, devicePriv [X25519KeySize]byte) ([]byte, error) {
	if len(wrapped) < X25519KeySize+nonceSize+ContentKeySize {
		return nil, ErrUnwrapAuthenticationFailed
	}
	ephemeralPub := wrapped[:X25519KeySize]
	nonce := wrapped[X25519KeySize : X25519KeySize+nonceSize]
	ct := wrapped[X25519KeySize+nonceSize:]

	shared, err := curve25519.X25519(devicePriv[:], ephemeralPub)
	if err != nil {
		return nil, ErrUnwrapAuthenticationFailed
	}
	// Reject the all-zero shared secret produced by a low-order-point
	// ephemeral pub (a known X25519 attack) up front rather than trusting
	// the GCM tag alone. Kept as defense-in-depth against a future X25519
	// backend swap: with the current golang.org/x/crypto curve25519.X25519
	// (which delegates to crypto/ecdh) this branch is unreachable, because
	// X25519 above already errors on every low-order point (including the
	// all-zero point) and short-circuits to ErrUnwrapAuthenticationFailed.
	// That is why this true-branch shows as uncovered under NFR-09.6.
	zero := make([]byte, len(shared))
	if subtle.ConstantTimeCompare(shared, zero) == 1 {
		return nil, ErrUnwrapAuthenticationFailed
	}
	devicePub, err := curve25519.X25519(devicePriv[:], curve25519.Basepoint)
	if err != nil {
		return nil, ErrUnwrapAuthenticationFailed
	}

	wrapKey, err := deriveWrapKey(shared, ephemeralPub, devicePub)
	if err != nil {
		return nil, ErrUnwrapAuthenticationFailed
	}
	gcm, err := newGCM(wrapKey)
	if err != nil {
		return nil, ErrUnwrapAuthenticationFailed
	}
	plaintext, err := gcm.Open(nil, nonce, ct, ephemeralPub)
	if err != nil {
		return nil, ErrUnwrapAuthenticationFailed
	}
	if len(plaintext) != ContentKeySize {
		return nil, ErrUnwrapAuthenticationFailed
	}
	return plaintext, nil
}

// deriveWrapKey runs HKDF-SHA256 over the ECDH shared secret with the two
// public keys mixed into the salt, binding the wrap to this exact
// (ephemeral, device) pair. Salt order MUST be ephemeralPub||devicePub on
// both the wrap and unwrap sides.
func deriveWrapKey(shared, ephemeralPub, devicePub []byte) ([]byte, error) {
	salt := make([]byte, 0, len(ephemeralPub)+len(devicePub))
	salt = append(salt, ephemeralPub...)
	salt = append(salt, devicePub...)
	wrapKey := make([]byte, wrapKeySize)
	r := hkdf.New(sha256.New, shared, salt, hkdfInfo)
	if _, err := io.ReadFull(r, wrapKey); err != nil {
		return nil, fmt.Errorf("keys: hkdf: %w", err)
	}
	return wrapKey, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("keys: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keys: gcm: %w", err)
	}
	return gcm, nil
}
