package keys

import (
	"crypto/ed25519"
	"fmt"
)

func SignV2(priv ed25519.PrivateKey, input []byte) ([64]byte, error) {
	if len(priv) != ed25519.PrivateKeySize || len(input) == 0 {
		return [64]byte{}, fmt.Errorf("keys: invalid signing input")
	}
	var out [64]byte
	copy(out[:], ed25519.Sign(priv, input))
	return out, nil
}
func VerifyV2(pub ed25519.PublicKey, input []byte, sig [64]byte) bool {
	return len(pub) == ed25519.PublicKeySize && len(input) > 0 && ed25519.Verify(pub, input, sig[:])
}
