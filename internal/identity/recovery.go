package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"runtime"

	"github.com/tyler-smith/go-bip39"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/text/unicode/norm"
)

const (
	RecoveryKDFProfileArgon2idV1 = "argon2id-256m-t3-p1-v1"
	MaxRecoveryMnemonicBytes     = 512
	recoveryArgonMemoryKiB       = 262144
	recoveryArgonIterations      = 3
	recoveryArgonParallelism     = 1
)

type RecoveryKeys struct {
	SigningSeed   [32]byte
	SigningPublic [32]byte
	WrapPrivate   [32]byte
	WrapPublic    [32]byte
	WrapKeyID     [32]byte
}

func GenerateRecoveryMnemonic() (string, error) {
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return "", err
	}
	defer clearBytes(entropy)
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return "", fmt.Errorf("identity: generate recovery phrase: %w", err)
	}
	return mnemonic, nil
}

func NormalizeRecoveryMnemonic(input []byte) ([]byte, error) {
	if len(input) == 0 || len(input) > MaxRecoveryMnemonicBytes {
		return nil, fmt.Errorf("identity: recovery phrase length is invalid")
	}
	normalized := norm.NFKD.Bytes(input)
	defer clearBytes(normalized)
	words := bytes.Fields(normalized)
	if len(words) != 24 {
		return nil, fmt.Errorf("identity: recovery phrase must contain 24 words")
	}
	joined := bytes.Join(words, []byte{' '})
	if len(joined) > MaxRecoveryMnemonicBytes || !bip39.IsMnemonicValid(string(joined)) {
		clearBytes(joined)
		return nil, fmt.Errorf("identity: recovery phrase checksum is invalid")
	}
	return joined, nil
}

// DeriveRecoveryKeys consumes and clears mnemonicInput. Callers that need to
// retain a display value must pass a dedicated mutable copy.
func DeriveRecoveryKeys(mnemonicInput []byte, salt [16]byte, profileID string) (RecoveryKeys, error) {
	defer clearBytes(mnemonicInput)
	if profileID != RecoveryKDFProfileArgon2idV1 {
		return RecoveryKeys{}, fmt.Errorf("identity: unsupported recovery KDF profile")
	}
	normalized, err := NormalizeRecoveryMnemonic(mnemonicInput)
	if err != nil {
		return RecoveryKeys{}, err
	}
	defer clearBytes(normalized)
	argonOutput := argon2.IDKey(normalized, salt[:], recoveryArgonIterations, recoveryArgonMemoryKiB, recoveryArgonParallelism, 32)
	defer clearBytes(argonOutput)
	signingSeed, err := hkdf.Key(sha256.New, argonOutput, nil, "aplexica/recovery-ed25519/v1", 32)
	if err != nil {
		return RecoveryKeys{}, err
	}
	defer clearBytes(signingSeed)
	wrapSeed, err := hkdf.Key(sha256.New, argonOutput, nil, "aplexica/recovery-x25519/v1", 32)
	if err != nil {
		return RecoveryKeys{}, err
	}
	defer clearBytes(wrapSeed)

	var result RecoveryKeys
	copy(result.SigningSeed[:], signingSeed)
	private := ed25519.NewKeyFromSeed(signingSeed)
	copy(result.SigningPublic[:], private[ed25519.SeedSize:])
	clearBytes(private)
	copy(result.WrapPrivate[:], wrapSeed)
	wrapPublic, err := curve25519.X25519(result.WrapPrivate[:], curve25519.Basepoint)
	if err != nil || zeroBytes(wrapPublic) {
		result.Clear()
		return RecoveryKeys{}, fmt.Errorf("identity: recovery wrap key derivation failed")
	}
	copy(result.WrapPublic[:], wrapPublic)
	clearBytes(wrapPublic)
	result.WrapKeyID = sha256.Sum256(result.WrapPublic[:])
	runtime.KeepAlive(mnemonicInput)
	return result, nil
}

func (k *RecoveryKeys) Clear() {
	if k == nil {
		return
	}
	clearBytes(k.SigningSeed[:])
	clearBytes(k.WrapPrivate[:])
}

func (k RecoveryKeys) SignTrustAnchor(unsigned AccountTrustAnchorUnsignedV1) (AccountTrustAnchorV1, error) {
	if unsigned.RecoveryKDFProfileID != RecoveryKDFProfileArgon2idV1 || unsigned.RecoveryRootPublicKey != k.SigningPublic || unsigned.RecoveryWrapPublicKey != k.WrapPublic || unsigned.RecoveryWrapKeyID != k.WrapKeyID {
		return AccountTrustAnchorV1{}, fmt.Errorf("identity: recovery anchor key mismatch")
	}
	preimage, err := canonical("aplexica/account-trust-anchor/v1", unsigned)
	if err != nil {
		return AccountTrustAnchorV1{}, err
	}
	private := ed25519.NewKeyFromSeed(k.SigningSeed[:])
	defer clearBytes(private)
	sig := ed25519.Sign(private, preimage)
	var out AccountTrustAnchorV1
	out.Anchor = unsigned
	copy(out.RecoverySignature[:], sig)
	clearBytes(sig)
	return out, nil
}

func (k RecoveryKeys) MatchesAnchor(anchor AccountTrustAnchorV1) bool {
	a := anchor.Anchor
	return a.RecoveryKDFProfileID == RecoveryKDFProfileArgon2idV1 && a.RecoveryRootPublicKey == k.SigningPublic && a.RecoveryWrapPublicKey == k.WrapPublic && a.RecoveryWrapKeyID == k.WrapKeyID
}

func clearBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
	runtime.KeepAlive(value)
}
