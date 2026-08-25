package keyrotation

import (
	"crypto/rand"
	"crypto/sha256"
	"testing"

	"golang.org/x/crypto/curve25519"

	"github.com/stretchr/testify/require"
)

func TestNamespaceWrapV2RejectsCrossContextReplay(t *testing.T) {
	var private [32]byte
	_, err := rand.Read(private[:])
	require.NoError(t, err)
	publicBytes, err := curve25519.X25519(private[:], curve25519.Basepoint)
	require.NoError(t, err)
	var public [32]byte
	copy(public[:], publicBytes)
	var content [32]byte
	_, err = rand.Read(content[:])
	require.NoError(t, err)
	ctx := WrapContextV2{NamespaceID: "0197f30a-3c58-7000-8000-000000000001", KeyVersion: 2, StatementHash: sha256.Sum256([]byte("statement")), RecipientType: "device", RecipientID: "device-a", RecipientWrapKeyID: sha256.Sum256(public[:]), AccessGeneration: 2, AccessSetHash: sha256.Sum256([]byte("access"))}
	wrapped, err := WrapKeyV2(content, public, ctx)
	require.NoError(t, err)
	opened, err := UnwrapKeyV2(wrapped, private, ctx)
	require.NoError(t, err)
	require.Equal(t, content, opened)

	changed := ctx
	changed.KeyVersion++
	_, err = UnwrapKeyV2(wrapped, private, changed)
	require.Error(t, err)
	changed = ctx
	changed.RecipientID = "device-b"
	_, err = UnwrapKeyV2(wrapped, private, changed)
	require.Error(t, err)
}
