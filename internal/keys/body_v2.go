package keys

import (
	"fmt"
	"golang.org/x/crypto/chacha20poly1305"
)

func SealBodyV2(key [32]byte, plaintext, aad []byte, nonce [24]byte) ([]byte, error) {
	if len(aad) == 0 {
		return nil, fmt.Errorf("keys: v2 AAD is required")
	}
	a, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, err
	}
	return a.Seal(nil, nonce[:], plaintext, aad), nil
}
func OpenBodyV2(key [32]byte, ciphertext, aad []byte, nonce [24]byte) ([]byte, error) {
	if len(aad) == 0 {
		return nil, ErrBodyAuthenticationFailed
	}
	a, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, ErrBodyAuthenticationFailed
	}
	p, err := a.Open(nil, nonce[:], ciphertext, aad)
	if err != nil {
		return nil, ErrBodyAuthenticationFailed
	}
	return p, nil
}
