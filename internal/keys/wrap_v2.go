package keys

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/aplexica/aplexica/internal/securewire"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
)

type WrappedKeyV2 struct {
	EphemeralPublic [32]byte
	Nonce           [24]byte
	Ciphertext      []byte
}

func wrapContext(headerHash [32]byte, recipientType, recipientID string, keyID [32]byte) ([]byte, error) {
	return securewire.Canonical("aplexica/wrap-context/v2", headerHash[:], recipientType, recipientID, keyID[:])
}
func deriveWrapKeyV2(shared, ephemeral, recipientPublic []byte, ctx []byte) ([32]byte, error) {
	saltInput := append([]byte("aplexica/wrap-salt/v2"), ephemeral...)
	saltInput = append(saltInput, recipientPublic...)
	salt := sha256.Sum256(saltInput)
	ch := sha256.Sum256(ctx)
	info := append([]byte("aplexica/wrap-key/v2"), ch[:]...)
	b, err := hkdf.Key(sha256.New, shared, salt[:], string(info), 32)
	var out [32]byte
	if err == nil {
		copy(out[:], b)
	}
	return out, err
}
func WrapContentKeyV2(content [32]byte, recipientPublic [32]byte, headerHash [32]byte, recipientType, recipientID string, keyID [32]byte) (WrappedKeyV2, error) {
	ctx, err := wrapContext(headerHash, recipientType, recipientID, keyID)
	if err != nil {
		return WrappedKeyV2{}, err
	}
	var eph [32]byte
	if _, err := io.ReadFull(rand.Reader, eph[:]); err != nil {
		return WrappedKeyV2{}, err
	}
	eph[0] &= 248
	eph[31] = (eph[31] & 127) | 64
	epub, err := curve25519.X25519(eph[:], curve25519.Basepoint)
	if err != nil {
		return WrappedKeyV2{}, err
	}
	shared, err := curve25519.X25519(eph[:], recipientPublic[:])
	if err != nil || zeroKey(shared) {
		return WrappedKeyV2{}, fmt.Errorf("keys: invalid recipient public key")
	}
	wk, err := deriveWrapKeyV2(shared, epub, recipientPublic[:], ctx)
	if err != nil {
		return WrappedKeyV2{}, err
	}
	a, err := chacha20poly1305.NewX(wk[:])
	if err != nil {
		return WrappedKeyV2{}, err
	}
	var out WrappedKeyV2
	copy(out.EphemeralPublic[:], epub)
	if _, err := io.ReadFull(rand.Reader, out.Nonce[:]); err != nil {
		return WrappedKeyV2{}, err
	}
	out.Ciphertext = a.Seal(nil, out.Nonce[:], content[:], ctx)
	return out, nil
}
func UnwrapContentKeyV2(w WrappedKeyV2, recipientPrivate [32]byte, headerHash [32]byte, recipientType, recipientID string, keyID [32]byte) ([32]byte, error) {
	if len(w.Ciphertext) != 48 {
		return [32]byte{}, ErrUnwrapAuthenticationFailed
	}
	ctx, err := wrapContext(headerHash, recipientType, recipientID, keyID)
	if err != nil {
		return [32]byte{}, ErrUnwrapAuthenticationFailed
	}
	pub, err := curve25519.X25519(recipientPrivate[:], curve25519.Basepoint)
	if err != nil {
		return [32]byte{}, ErrUnwrapAuthenticationFailed
	}
	shared, err := curve25519.X25519(recipientPrivate[:], w.EphemeralPublic[:])
	if err != nil || zeroKey(shared) {
		return [32]byte{}, ErrUnwrapAuthenticationFailed
	}
	wk, err := deriveWrapKeyV2(shared, w.EphemeralPublic[:], pub, ctx)
	if err != nil {
		return [32]byte{}, ErrUnwrapAuthenticationFailed
	}
	a, err := chacha20poly1305.NewX(wk[:])
	if err != nil {
		return [32]byte{}, ErrUnwrapAuthenticationFailed
	}
	p, err := a.Open(nil, w.Nonce[:], w.Ciphertext, ctx)
	if err != nil || len(p) != 32 {
		return [32]byte{}, ErrUnwrapAuthenticationFailed
	}
	var out [32]byte
	copy(out[:], p)
	return out, nil
}
func zeroKey(b []byte) bool {
	var x byte
	for _, v := range b {
		x |= v
	}
	return x == 0
}
