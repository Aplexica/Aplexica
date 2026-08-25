package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func signIdentityValue(t *testing.T, private ed25519.PrivateKey, domain string, value any) [64]byte {
	t.Helper()
	b, err := canonical(domain, value)
	require.NoError(t, err)
	var signature [64]byte
	copy(signature[:], ed25519.Sign(private, b))
	return signature
}

func TestServiceTrustAndSingleLeafTransparency(t *testing.T) {
	releasePublic, releasePrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	logPublic, logPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Now().UTC()
	var logKey [32]byte
	copy(logKey[:], logPublic)
	config := ServiceTrustConfigV1{Config: ServiceTrustConfigUnsignedV1{Version: 1, Sequence: 1, ServiceOrigin: "https://api.aplexica.com", TransparencyLogKey: logKey, NotBeforeUnix: now.Add(-time.Minute).Unix(), NotAfterUnix: now.Add(time.Hour).Unix()}}
	config.ReleaseRootSig = signIdentityValue(t, releasePrivate, "aplexica/service-trust-config/v1", config.Config)
	require.NoError(t, VerifyServiceTrustConfig(nil, config, releasePublic, now))

	leaf := TransparencyLeafV1{Version: 1, LeafType: "roster", ScopeID: "scope-a", Epoch: 1, ObjectHash: sha256.Sum256([]byte("roster"))}
	root, err := TransparencyLeafHash(leaf)
	require.NoError(t, err)
	head := SignedTreeHeadV1{Head: SignedTreeHeadUnsignedV1{Version: 1, LogID: sha256.Sum256(logPublic), TreeSize: 1, RootHash: root, TimestampUnix: now.Unix()}}
	head.Signature = signIdentityValue(t, logPrivate, "aplexica/transparency-tree-head/v1", head.Head)
	proof := TransparencyProofV1{Leaf: leaf, SignedTreeHead: head}
	require.NoError(t, VerifyTransparencyProof(config, nil, proof, leaf, now))

	bad := proof
	bad.SignedTreeHead.Head.RootHash[0] ^= 1
	require.Error(t, VerifyTransparencyProof(config, nil, bad, leaf, now))
}
