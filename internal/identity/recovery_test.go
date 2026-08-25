package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecoveryDerivationIsDeterministicAndClearsInput(t *testing.T) {
	mnemonic := strings.TrimSpace(strings.Repeat("abandon ", 23) + "art")
	input := []byte(mnemonic)
	var salt [16]byte
	copy(salt[:], []byte("fixed-test-salt!"))
	first, err := DeriveRecoveryKeys(input, salt, RecoveryKDFProfileArgon2idV1)
	require.NoError(t, err)
	require.Equal(t, make([]byte, len(input)), input)
	secondInput := []byte(mnemonic)
	second, err := DeriveRecoveryKeys(secondInput, salt, RecoveryKDFProfileArgon2idV1)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, sha256.Sum256(first.WrapPublic[:]), first.WrapKeyID)
	private := ed25519.NewKeyFromSeed(first.SigningSeed[:])
	require.Equal(t, first.SigningPublic[:], []byte(private[ed25519.SeedSize:]))
	first.Clear()
	second.Clear()
}

func TestNormalizeRecoveryMnemonicRejectsBadChecksumWithoutEcho(t *testing.T) {
	bad := []byte(strings.Repeat("abandon ", 24))
	_, err := NormalizeRecoveryMnemonic(bad)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "abandon")
	require.False(t, bytes.Contains([]byte(err.Error()), bad))
}
