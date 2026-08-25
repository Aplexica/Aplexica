package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/require"
)

func TestValidateCompiledRemotePluginPublisherKeyRingAcceptsRotationOverlap(t *testing.T) {
	oldPublic, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	newPublic, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	keys, err := validateCompiledRemotePluginPublisherKeyRing([]compiledRemotePluginPublisherKey{
		{Name: "old-root", PublicKeyHex: hex.EncodeToString(oldPublic)},
		{Name: "new-root", PublicKeyHex: hex.EncodeToString(newPublic)},
	})
	require.NoError(t, err)
	require.Equal(t, []ed25519.PublicKey{oldPublic, newPublic}, keys)
}

func TestValidateCompiledRemotePluginPublisherKeyRingRejectsInvalidEntries(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	encoded := hex.EncodeToString(public)

	tests := []struct {
		name string
		ring []compiledRemotePluginPublisherKey
		want string
	}{
		{name: "empty", want: "between 1 and"},
		{name: "blank name", ring: []compiledRemotePluginPublisherKey{{PublicKeyHex: encoded}}, want: "invalid name"},
		{name: "duplicate name", ring: []compiledRemotePluginPublisherKey{{Name: "root", PublicKeyHex: encoded}, {Name: "root", PublicKeyHex: strings.Repeat("01", ed25519.PublicKeySize)}}, want: "duplicates name"},
		{name: "malformed key", ring: []compiledRemotePluginPublisherKey{{Name: "root", PublicKeyHex: "not-hex"}}, want: "invalid Ed25519"},
		{name: "noncanonical hex", ring: []compiledRemotePluginPublisherKey{{Name: "root", PublicKeyHex: strings.ToUpper(encoded)}}, want: "invalid Ed25519"},
		{name: "zero key", ring: []compiledRemotePluginPublisherKey{{Name: "root", PublicKeyHex: strings.Repeat("00", ed25519.PublicKeySize)}}, want: "all-zero"},
		{name: "duplicate key", ring: []compiledRemotePluginPublisherKey{{Name: "old", PublicKeyHex: encoded}, {Name: "new", PublicKeyHex: encoded}}, want: "duplicates a public key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateCompiledRemotePluginPublisherKeyRing(tt.ring)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestRemotePluginPublisherKeysIncludeTransitionRoots(t *testing.T) {
	keys := remotePluginPublisherKeys()
	wants := []struct {
		name      string
		publicHex string
	}{
		{name: "existing", publicHex: remotePluginPublisherPublicKeyHex},
		{name: "provider-neutral", publicHex: providerNeutralRemotePluginPublisherPublicKeyHex},
	}
	for _, want := range wants {
		t.Run(want.name, func(t *testing.T) {
			decoded, err := hex.DecodeString(want.publicHex)
			require.NoError(t, err)
			var found bool
			for _, key := range keys {
				if string(key) == string(decoded) {
					found = true
					break
				}
			}
			require.True(t, found, "compiled publisher key ring must include the %s root during rotation", want.name)
		})
	}
}

func TestRemotePluginRuntimePolicyRestrictsV2ToProviderNeutralRoot(t *testing.T) {
	policy := remotePluginTrustPolicy()
	require.Len(t, policy.V2Publishers, 2)
	require.Equal(t, mustRemotePluginDigest("ddcaa7baac5957f32d38857a6e551a810975a2e3b3f3b71410b04ebc0174b80f"), policy.V2Publishers[0])
	require.Equal(t, mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex), policy.V2Publishers[1])
	require.NotEqual(t, mustRemotePluginDigest("1a2ed90a1da3fc6888c5a1944076a83c91c7a3e8d835b243d01bb6779bb33897"), policy.V2Publishers[0])
	require.Len(t, policy.LegacyV1, 6)
}

// trustedRemotePluginTestDir keeps verifier fixtures outside /tmp on Unix.
// The real trust path intentionally rejects world-writable ancestors.
func trustedRemotePluginTestDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return t.TempDir()
	}
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	home, err = filepath.EvalSymlinks(home)
	require.NoError(t, err)
	dir, err := os.MkdirTemp(home, ".aplexica-remote-plugin-test-")
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0o700))
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	return dir
}

func TestBalancedRemotePluginAuthorizationRequiresExactCompiledBytes(t *testing.T) {
	root := trustedRemotePluginTestDir(t)
	executable := filepath.Join(root, "aplexica-cloud-plugin")
	binary := []byte("exact Balanced plugin")
	require.NoError(t, os.WriteFile(executable, binary, 0o700))
	authorizations := []proto.BalancedRemotePluginAuthorization{{
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, PluginVersion: "v0.1.2",
		Sequence: 1, RollbackFloor: 1, BinarySHA256: sha256.Sum256(binary),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex),
	}}

	verified, err := verifyBalancedRemotePluginWithAuthorizations(executable, authorizations)
	require.NoError(t, err)
	require.Equal(t, "v0.1.2", verified.Manifest.PluginVersion)
	require.Equal(t, mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex), verified.PublisherKeySHA256)
	require.Contains(t, remotePluginTrustPolicy().V2Publishers, verified.PublisherKeySHA256)

	require.NoError(t, os.WriteFile(executable, []byte("substitution"), 0o700))
	_, err = verifyBalancedRemotePluginWithAuthorizations(executable, authorizations)
	require.ErrorContains(t, err, "digest mismatch")
}

func TestBalancedRemotePluginV013IsExactSuccessorOnSupportedPlatforms(t *testing.T) {
	require.Len(t, balancedRemotePluginArtifacts, 26)

	wantPrevious := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("59212a81548ff6d613c6487bded511424d28ecb5e60f574964094ea9d7f56965"),
		"windows/amd64": mustRemotePluginDigest("a3e240766e4fac6cda57f40a154ea3b73c4d0aff7d945495e0e46cdebf34e383"),
	}
	for _, authorization := range balancedRemotePluginArtifacts[2:4] {
		platform := authorization.GOOS + "/" + authorization.GOARCH
		require.Equal(t, "v0.1.3", authorization.PluginVersion, platform)
		require.Equal(t, uint64(2), authorization.Sequence, platform)
		require.Equal(t, uint64(2), authorization.RollbackFloor, platform)
		require.NotNil(t, authorization.Previous, platform)
		require.Equal(t, uint64(1), authorization.Previous.Sequence, platform)
		require.Equal(t, "v0.1.2", authorization.Previous.PluginVersion, platform)
		require.Equal(t, wantPrevious[platform], authorization.Previous.InventorySHA256, platform)
	}
}

func TestBalancedRemotePluginV014IsExactSuccessorOnSupportedPlatforms(t *testing.T) {
	require.Len(t, balancedRemotePluginArtifacts, 26)

	wantPrevious := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("720bb0e2658c3d556d7537ff9f10639650ec081224d1b0d65c5a818d32924c02"),
		"windows/amd64": mustRemotePluginDigest("d0d83fde5af7ddfb65407a9af8edf9985807e6d08b23f7d0885ea270c37375cf"),
	}
	for _, authorization := range balancedRemotePluginArtifacts[4:6] {
		platform := authorization.GOOS + "/" + authorization.GOARCH
		require.Equal(t, "v0.1.4", authorization.PluginVersion, platform)
		require.Equal(t, uint64(3), authorization.Sequence, platform)
		require.Equal(t, uint64(3), authorization.RollbackFloor, platform)
		require.NotNil(t, authorization.Previous, platform)
		require.Equal(t, uint64(2), authorization.Previous.Sequence, platform)
		require.Equal(t, "v0.1.3", authorization.Previous.PluginVersion, platform)
		require.Equal(t, wantPrevious[platform], authorization.Previous.InventorySHA256, platform)
	}
}

func TestBalancedRemotePluginV015IsExactSuccessorOnSupportedPlatforms(t *testing.T) {
	require.Len(t, balancedRemotePluginArtifacts, 26)

	wantPrevious := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("1cf7902416e13ac711b0416e39957e7b570ba614c4d0ea4948eef464ba342472"),
		"windows/amd64": mustRemotePluginDigest("04256d37c7534deade217c70e4fef97586069d2ea9c27d838d43cb8cf46a2676"),
	}
	for _, authorization := range balancedRemotePluginArtifacts[6:8] {
		platform := authorization.GOOS + "/" + authorization.GOARCH
		require.Equal(t, "v0.1.5", authorization.PluginVersion, platform)
		require.Equal(t, uint64(4), authorization.Sequence, platform)
		require.Equal(t, uint64(4), authorization.RollbackFloor, platform)
		require.NotNil(t, authorization.Previous, platform)
		require.Equal(t, uint64(3), authorization.Previous.Sequence, platform)
		require.Equal(t, "v0.1.4", authorization.Previous.PluginVersion, platform)
		require.Equal(t, wantPrevious[platform], authorization.Previous.InventorySHA256, platform)
	}
}

func TestBalancedRemotePluginV016IsExactSuccessorOnSupportedPlatforms(t *testing.T) {
	require.Len(t, balancedRemotePluginArtifacts, 26)

	wantPrevious := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("2d6b21026b9d811c31edabc50ccdeb7fa6b51e493b14e3fc88f020d189844628"),
		"windows/amd64": mustRemotePluginDigest("0d0cd691572aee93aa9ad72925e6996d21d167fc97de9812a8b59e20d6df2535"),
	}
	for _, authorization := range balancedRemotePluginArtifacts[8:10] {
		platform := authorization.GOOS + "/" + authorization.GOARCH
		require.Equal(t, "v0.1.6", authorization.PluginVersion, platform)
		require.Equal(t, uint64(5), authorization.Sequence, platform)
		require.Equal(t, uint64(5), authorization.RollbackFloor, platform)
		require.NotNil(t, authorization.Previous, platform)
		require.Equal(t, uint64(4), authorization.Previous.Sequence, platform)
		require.Equal(t, "v0.1.5", authorization.Previous.PluginVersion, platform)
		require.Equal(t, wantPrevious[platform], authorization.Previous.InventorySHA256, platform)
		require.Empty(t, authorization.Capabilities, platform)
	}
}

func TestBalancedRemotePluginV017IsExactSuccessorWithDurableDeltaOnSupportedPlatforms(t *testing.T) {
	require.Len(t, balancedRemotePluginArtifacts, 26)

	wantPrevious := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("5ae95b7d54bb7755a158f31c6a9b2d6d5e95de44268552a2a8e5e31c4d536e22"),
		"windows/amd64": mustRemotePluginDigest("634b107724342676fff63b8dcbd397cb186e84b8afc18389a88cdb46b36f5815"),
	}
	wantCapabilities := []string{
		proto.CapabilityDurableDeltaSyncV1,
		proto.CapabilityInboundAckV2,
		proto.CapabilityPairStdinV1,
		proto.CapabilityTrustProtocolV1,
	}
	for _, authorization := range balancedRemotePluginArtifacts[10:12] {
		platform := authorization.GOOS + "/" + authorization.GOARCH
		require.Equal(t, "v0.1.7", authorization.PluginVersion, platform)
		require.Equal(t, uint64(6), authorization.Sequence, platform)
		require.Equal(t, uint64(6), authorization.RollbackFloor, platform)
		require.NotNil(t, authorization.Previous, platform)
		require.Equal(t, uint64(5), authorization.Previous.Sequence, platform)
		require.Equal(t, "v0.1.6", authorization.Previous.PluginVersion, platform)
		require.Equal(t, wantPrevious[platform], authorization.Previous.InventorySHA256, platform)
		require.Equal(t, wantCapabilities, authorization.Capabilities, platform)
	}
}

func TestBalancedRemotePluginV018IsExactSuccessorWithCompleteDurableDeltaCapabilities(t *testing.T) {
	require.Len(t, balancedRemotePluginArtifacts, 26)

	wantPrevious := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("cfc43b22dabd17b65bf11f31cdde95484b349b646461c24f294bfe00e9ad9c2d"),
		"windows/amd64": mustRemotePluginDigest("2cb7beaa0726e53d85d2a55cbd539c8445747245e0d8cc1ad6a890f990de145e"),
	}
	wantBinary := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("0fcd15c1d0011ebe272174e344fbe60dee657db1dd3085dc4534df8d4c16952b"),
		"windows/amd64": mustRemotePluginDigest("ccbb85dc7c8595bb80d0f09b3080421d47c093e66e36c24591624d5a9f51ae2a"),
	}
	wantInventory := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("7eea91fefd044c665c4c021ad8010ed4eb28a4f52ae98464297f7a0288c0d67f"),
		"windows/amd64": mustRemotePluginDigest("867afac762343977cf4f51132f0104318724dbf061a33366a7fc1ec41e3cf65c"),
	}
	wantCapabilities := []string{
		proto.CapabilityDurableCursorResumeV1,
		proto.CapabilityDurableDeltaSyncV1,
		proto.CapabilityDurableMultiStreamV1,
		proto.CapabilityInboundAckV2,
		proto.CapabilityInboundFinalizeV1,
		proto.CapabilityPairStdinV1,
		proto.CapabilityRedactionSafeBatchV1,
		proto.CapabilityStagedCheckpointV1,
		proto.CapabilityTrustProtocolV1,
	}
	for _, authorization := range balancedRemotePluginArtifacts[12:14] {
		platform := authorization.GOOS + "/" + authorization.GOARCH
		require.Equal(t, "v0.1.8", authorization.PluginVersion, platform)
		require.Equal(t, uint64(7), authorization.Sequence, platform)
		require.Equal(t, uint64(7), authorization.RollbackFloor, platform)
		require.NotNil(t, authorization.Previous, platform)
		require.Equal(t, uint64(6), authorization.Previous.Sequence, platform)
		require.Equal(t, "v0.1.7", authorization.Previous.PluginVersion, platform)
		require.Equal(t, wantPrevious[platform], authorization.Previous.InventorySHA256, platform)
		require.Equal(t, wantCapabilities, authorization.Capabilities, platform)
		require.Equal(t, wantBinary[platform], authorization.BinarySHA256, platform)
		require.Equal(t, wantInventory[platform], balancedRemoteInventoryDigestForTest(t, authorization), platform)
	}
}

func TestBalancedRemotePluginV019IsExactSuccessorWithCompleteDurableDeltaCapabilities(t *testing.T) {
	require.Len(t, balancedRemotePluginArtifacts, 26)

	wantPrevious := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("7eea91fefd044c665c4c021ad8010ed4eb28a4f52ae98464297f7a0288c0d67f"),
		"windows/amd64": mustRemotePluginDigest("867afac762343977cf4f51132f0104318724dbf061a33366a7fc1ec41e3cf65c"),
	}
	wantBinary := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("c8f794f5e4602dc658cba24e1a61c00813e711b10b4e8529257d7b24e6729803"),
		"windows/amd64": mustRemotePluginDigest("fa4e8bbf80039d776b4df1427b8254ed2dc41e800b5a20dc7e8e6766f5387c8f"),
	}
	wantInventory := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("39bdd0917785a10c1f7606a7d54040de692da9fb9539f48cb41edd74b725c0a4"),
		"windows/amd64": mustRemotePluginDigest("523c573dc24780dc53d72f0779b12b8214c19804ce8bf6888e3c3be801814c3d"),
	}
	wantCapabilities := []string{
		proto.CapabilityDurableCursorResumeV1,
		proto.CapabilityDurableDeltaSyncV1,
		proto.CapabilityDurableMultiStreamV1,
		proto.CapabilityInboundAckV2,
		proto.CapabilityInboundFinalizeV1,
		proto.CapabilityPairStdinV1,
		proto.CapabilityRedactionSafeBatchV1,
		proto.CapabilityStagedCheckpointV1,
		proto.CapabilityTrustProtocolV1,
	}
	for _, authorization := range balancedRemotePluginArtifacts[14:16] {
		platform := authorization.GOOS + "/" + authorization.GOARCH
		require.Equal(t, "v0.1.9", authorization.PluginVersion, platform)
		require.Equal(t, uint64(8), authorization.Sequence, platform)
		require.Equal(t, uint64(8), authorization.RollbackFloor, platform)
		require.NotNil(t, authorization.Previous, platform)
		require.Equal(t, uint64(7), authorization.Previous.Sequence, platform)
		require.Equal(t, "v0.1.8", authorization.Previous.PluginVersion, platform)
		require.Equal(t, wantPrevious[platform], authorization.Previous.InventorySHA256, platform)
		require.Equal(t, wantCapabilities, authorization.Capabilities, platform)
		require.Equal(t, wantBinary[platform], authorization.BinarySHA256, platform)
		require.Equal(t, wantInventory[platform], balancedRemoteInventoryDigestForTest(t, authorization), platform)
	}
}

func TestBalancedRemotePluginV0110IsExactObservationSuccessor(t *testing.T) {
	require.Len(t, balancedRemotePluginArtifacts, 26)

	wantPrevious := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("39bdd0917785a10c1f7606a7d54040de692da9fb9539f48cb41edd74b725c0a4"),
		"windows/amd64": mustRemotePluginDigest("523c573dc24780dc53d72f0779b12b8214c19804ce8bf6888e3c3be801814c3d"),
	}
	wantBinary := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("a22cf06a23b03b3d4687a12ef178a5152f0af66e9f0174b837d9f6de3d65ab87"),
		"windows/amd64": mustRemotePluginDigest("330d12131addc48e5b431dd47939100ad54fea49c9a35707e6051e623bcde15c"),
	}
	wantInventory := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("0c4ca296c54042ad4720254ff036e1a5bb92afd138b96fa999f1a839ed4de752"),
		"windows/amd64": mustRemotePluginDigest("97aa2ca6802f88b849d5802c485a2faeb97d7eac0bd8ef5d90dece1126b515a4"),
	}
	wantCapabilities := []string{
		proto.CapabilityDurableCursorResumeV1,
		proto.CapabilityDurableDeltaSyncV1,
		proto.CapabilityDurableMultiStreamV1,
		proto.CapabilityDurableSyncObservationV1,
		proto.CapabilityInboundAckV2,
		proto.CapabilityInboundFinalizeV1,
		proto.CapabilityPairStdinV1,
		proto.CapabilityRedactionSafeBatchV1,
		proto.CapabilityStagedCheckpointV1,
		proto.CapabilityTrustProtocolV1,
	}
	for _, authorization := range balancedRemotePluginArtifacts[16:18] {
		platform := authorization.GOOS + "/" + authorization.GOARCH
		require.Equal(t, "v0.1.10", authorization.PluginVersion, platform)
		require.Equal(t, uint64(9), authorization.Sequence, platform)
		require.Equal(t, uint64(9), authorization.RollbackFloor, platform)
		require.NotNil(t, authorization.Previous, platform)
		require.Equal(t, uint64(8), authorization.Previous.Sequence, platform)
		require.Equal(t, "v0.1.9", authorization.Previous.PluginVersion, platform)
		require.Equal(t, wantPrevious[platform], authorization.Previous.InventorySHA256, platform)
		require.Equal(t, wantCapabilities, authorization.Capabilities, platform)
		require.Equal(t, wantBinary[platform], authorization.BinarySHA256, platform)
		require.Equal(t, wantInventory[platform], balancedRemoteInventoryDigestForTest(t, authorization), platform)
	}
}

func TestBalancedRemotePluginV0111IsExactEgressSuccessor(t *testing.T) {
	require.Len(t, balancedRemotePluginArtifacts, 26)

	wantPrevious := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("0c4ca296c54042ad4720254ff036e1a5bb92afd138b96fa999f1a839ed4de752"),
		"windows/amd64": mustRemotePluginDigest("97aa2ca6802f88b849d5802c485a2faeb97d7eac0bd8ef5d90dece1126b515a4"),
	}
	wantBinary := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("a0edded216312fbb55f260a988f982c52c9552da07810abde380c161a0414d7f"),
		"windows/amd64": mustRemotePluginDigest("b670d6cc0efd87508748bde798d59bb2153fd9e031d4890eb9ad9e4d64015dcf"),
	}
	wantInventory := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("f81c12f0c8c3474e6a2e884a2cca4a846fe4eb311bcdfba165d0703fbd979a06"),
		"windows/amd64": mustRemotePluginDigest("f979bf84a8c0e0150c05b601e2f58e891f63aa0242833d3a760967ad9f1ee86c"),
	}
	// v0.1.11 is the broker egress fix. It changes runtime MQTT behaviour only;
	// the negotiated capability set is identical to v0.1.10.
	wantCapabilities := []string{
		proto.CapabilityDurableCursorResumeV1,
		proto.CapabilityDurableDeltaSyncV1,
		proto.CapabilityDurableMultiStreamV1,
		proto.CapabilityDurableSyncObservationV1,
		proto.CapabilityInboundAckV2,
		proto.CapabilityInboundFinalizeV1,
		proto.CapabilityPairStdinV1,
		proto.CapabilityRedactionSafeBatchV1,
		proto.CapabilityStagedCheckpointV1,
		proto.CapabilityTrustProtocolV1,
	}
	for _, authorization := range balancedRemotePluginArtifacts[18:20] {
		platform := authorization.GOOS + "/" + authorization.GOARCH
		require.Equal(t, "v0.1.11", authorization.PluginVersion, platform)
		require.Equal(t, uint64(10), authorization.Sequence, platform)
		require.Equal(t, uint64(10), authorization.RollbackFloor, platform)
		require.NotNil(t, authorization.Previous, platform)
		require.Equal(t, uint64(9), authorization.Previous.Sequence, platform)
		require.Equal(t, "v0.1.10", authorization.Previous.PluginVersion, platform)
		require.Equal(t, wantPrevious[platform], authorization.Previous.InventorySHA256, platform)
		require.Equal(t, wantCapabilities, authorization.Capabilities, platform)
		require.Equal(t, wantBinary[platform], authorization.BinarySHA256, platform)
		require.Equal(t, wantInventory[platform], balancedRemoteInventoryDigestForTest(t, authorization), platform)
	}
}

func TestBalancedRemotePluginV0114IsExactEgressSuccessor(t *testing.T) {
	require.Len(t, balancedRemotePluginArtifacts, 26)

	wantPrevious := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("a6468416dfc1da5525b6ccac4f29685c874184f5abf291a03adb5cd3990904af"),
		"windows/amd64": mustRemotePluginDigest("e64c44df410cfaa99429e1d41b0da4d44a9222296188ca014f7e7e01f009763d"),
	}
	wantBinary := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("aedc263d02e568c2d0ceec2fef0937a30fcd50c3912f044d5970347318432300"),
		"windows/amd64": mustRemotePluginDigest("75d5845cd434de18ad4211dd4fd93ba0dfa171515aa395e75f1f5fdfc1b78198"),
	}
	wantCapabilities := []string{
		proto.CapabilityDurableCursorResumeV1,
		proto.CapabilityDurableDeltaSyncV1,
		proto.CapabilityDurableMultiStreamV1,
		proto.CapabilityDurableSyncObservationV1,
		proto.CapabilityInboundAckV2,
		proto.CapabilityInboundFinalizeV1,
		proto.CapabilityPairStdinV1,
		proto.CapabilityRedactionSafeBatchV1,
		proto.CapabilityStagedCheckpointV1,
		proto.CapabilityTrustProtocolV1,
	}
	for _, authorization := range balancedRemotePluginArtifacts[24:] {
		platform := authorization.GOOS + "/" + authorization.GOARCH
		require.Equal(t, "v0.1.14", authorization.PluginVersion, platform)
		require.Equal(t, uint64(13), authorization.Sequence, platform)
		require.Equal(t, uint64(13), authorization.RollbackFloor, platform)
		require.NotNil(t, authorization.Previous, platform)
		require.Equal(t, uint64(12), authorization.Previous.Sequence, platform)
		require.Equal(t, "v0.1.13", authorization.Previous.PluginVersion, platform)
		require.Equal(t, wantPrevious[platform], authorization.Previous.InventorySHA256, platform)
		require.Equal(t, wantCapabilities, authorization.Capabilities, platform)
		require.Equal(t, wantBinary[platform], authorization.BinarySHA256, platform)
		require.Equal(t, mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex), authorization.PublisherKeySHA256, platform)
	}
}

func TestBalancedRemotePluginV0113IsExactEgressSuccessor(t *testing.T) {
	require.Len(t, balancedRemotePluginArtifacts, 26)

	wantPrevious := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("ddc93d919649909b522081211aa35b553b57df7912a40af39be470a71f91da13"),
		"windows/amd64": mustRemotePluginDigest("369126936ad422bdaba12505cb9f022f225aab2d8c32163f086c6c46bfbac168"),
	}
	wantBinary := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("223032e9b50c28642399155347b792b5767050bd9b454e7a13bc260c7286944f"),
		"windows/amd64": mustRemotePluginDigest("26540e6150127bb2eea45489edef96bb601012797a26abb3622ce8ec5d53cb81"),
	}
	wantInventory := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("a6468416dfc1da5525b6ccac4f29685c874184f5abf291a03adb5cd3990904af"),
		"windows/amd64": mustRemotePluginDigest("e64c44df410cfaa99429e1d41b0da4d44a9222296188ca014f7e7e01f009763d"),
	}
	// v0.1.13 carries the ADR-D3 client half (the fail-closed
	// remote.envelope_caps RPC the daemon consults before sealing envelope v3)
	// and the hardened shadow-soak evidence semantics. Runtime behaviour only;
	// the negotiated capability set is identical to v0.1.12.
	wantCapabilities := []string{
		proto.CapabilityDurableCursorResumeV1,
		proto.CapabilityDurableDeltaSyncV1,
		proto.CapabilityDurableMultiStreamV1,
		proto.CapabilityDurableSyncObservationV1,
		proto.CapabilityInboundAckV2,
		proto.CapabilityInboundFinalizeV1,
		proto.CapabilityPairStdinV1,
		proto.CapabilityRedactionSafeBatchV1,
		proto.CapabilityStagedCheckpointV1,
		proto.CapabilityTrustProtocolV1,
	}
	for _, authorization := range balancedRemotePluginArtifacts[22:24] {
		platform := authorization.GOOS + "/" + authorization.GOARCH
		require.Equal(t, "v0.1.13", authorization.PluginVersion, platform)
		require.Equal(t, uint64(12), authorization.Sequence, platform)
		require.Equal(t, uint64(12), authorization.RollbackFloor, platform)
		require.NotNil(t, authorization.Previous, platform)
		require.Equal(t, uint64(11), authorization.Previous.Sequence, platform)
		require.Equal(t, "v0.1.12", authorization.Previous.PluginVersion, platform)
		require.Equal(t, wantPrevious[platform], authorization.Previous.InventorySHA256, platform)
		require.Equal(t, wantCapabilities, authorization.Capabilities, platform)
		require.Equal(t, wantBinary[platform], authorization.BinarySHA256, platform)
		require.Equal(t, wantInventory[platform], balancedRemoteInventoryDigestForTest(t, authorization), platform)
	}
}

func balancedRemoteInventoryDigestForTest(t *testing.T, authorization proto.BalancedRemotePluginAuthorization) [32]byte {
	t.Helper()
	enc, err := cbor.CanonicalEncOptions().EncMode()
	require.NoError(t, err)
	manifest := proto.RemotePluginManifestUnsignedV1{
		Version:       2,
		PluginID:      "aplexica-cloud",
		PluginVersion: authorization.PluginVersion,
		Sequence:      authorization.Sequence,
		RollbackFloor: authorization.RollbackFloor,
		Previous:      authorization.Previous,
		BinarySHA256:  authorization.BinarySHA256,
		Capabilities:  append([]string(nil), authorization.Capabilities...),
		ProtocolMin:   1,
		ProtocolMax:   1,
	}
	identity := []any{authorization.GOOS, authorization.GOARCH, manifest, authorization.PublisherKeySHA256}
	raw, err := enc.Marshal([]any{"aplexica/balanced-remote-plugin-inventory/v1", identity})
	require.NoError(t, err)
	return sha256.Sum256(raw)
}

func TestRetainedV011OverlapManifestsUseProviderNeutralRootAndExactAllowlist(t *testing.T) {
	fixtures := []struct {
		goos, goarch, rawHex, binaryHex, manifestHex string
	}{
		{"darwin", "amd64", "a2686d616e6966657374a76776657273696f6e0168706c7567696e49646e61706c65786963612d636c6f75646b70726f746f636f6c4d6178016b70726f746f636f6c4d696e016c62696e61727953686132353658206348ab00f3a60c319e83539bca9c9f62fe7fe4968e56d671af797d2e701fb2706c6361706162696c6974696573836e696e626f756e645f61636b5f76326d706169725f737464696e5f76317174727573745f70726f746f636f6c5f76316d706c7567696e56657273696f6e6676302e312e31697369676e61747572655840f1ae9ede440b714f13772be2416019ed5fae49fb3006e241d8fe03f6ff32fb5ffc200e3aafeb90ff3238f29b8e40447d683dbe60c112a39f77d8653da10d3f05", "6348ab00f3a60c319e83539bca9c9f62fe7fe4968e56d671af797d2e701fb270", "2ecaa972f23d989a4eb353421c409af6d3a07f03ce5c8e997c193eb6d94c7942"},
		{"darwin", "arm64", "a2686d616e6966657374a76776657273696f6e0168706c7567696e49646e61706c65786963612d636c6f75646b70726f746f636f6c4d6178016b70726f746f636f6c4d696e016c62696e61727953686132353658206f59f982cac737543e4fadbcb279b92f648b8f2061b57d96ada7637596eeffc26c6361706162696c6974696573836e696e626f756e645f61636b5f76326d706169725f737464696e5f76317174727573745f70726f746f636f6c5f76316d706c7567696e56657273696f6e6676302e312e31697369676e6174757265584070531dec118f29f0802bde95dca805c2cc3d5d10e545e3af971124910fd2d1784a0bdfd0f7b5db4952bd6515405901009ae6e1778f7bcfed4115094aed8d5c01", "6f59f982cac737543e4fadbcb279b92f648b8f2061b57d96ada7637596eeffc2", "cacf6fed7c5b50d7ef9dce781a3146143279bc4a641c1cce7a28af113e9ea1e4"},
		{"linux", "amd64", "a2686d616e6966657374a76776657273696f6e0168706c7567696e49646e61706c65786963612d636c6f75646b70726f746f636f6c4d6178016b70726f746f636f6c4d696e016c62696e6172795368613235365820aed359f4e2d84eb40cbaa40d909e2e25f7a32ba9eaefc4eef46130f7963c8e6a6c6361706162696c6974696573836e696e626f756e645f61636b5f76326d706169725f737464696e5f76317174727573745f70726f746f636f6c5f76316d706c7567696e56657273696f6e6676302e312e31697369676e61747572655840560d122932531803d6bc581dad127871e03c059520c9268f8330151f6ba46f64a1dedacb4520dcaee644ed088c768cb84d9c821c0aab038e06f9446fd3fdd30f", "aed359f4e2d84eb40cbaa40d909e2e25f7a32ba9eaefc4eef46130f7963c8e6a", "121b05a844c197963921385099c7612143d6a7be6a984bdb9aac7cfc5f8d27c0"},
		{"linux", "arm64", "a2686d616e6966657374a76776657273696f6e0168706c7567696e49646e61706c65786963612d636c6f75646b70726f746f636f6c4d6178016b70726f746f636f6c4d696e016c62696e617279536861323536582080b6bc3c44bd98403401f338d0a1d5b931c34f8189189c14ab2a67d76d11e0ee6c6361706162696c6974696573836e696e626f756e645f61636b5f76326d706169725f737464696e5f76317174727573745f70726f746f636f6c5f76316d706c7567696e56657273696f6e6676302e312e31697369676e6174757265584069101884891c4e8f921ad212bb204ec117b0724b4c5fb0149e634231fe04daf25afdc43ac27de3ba1397b32ac38c85e7a304ba2a04cd5a67b0183f6860ffd208", "80b6bc3c44bd98403401f338d0a1d5b931c34f8189189c14ab2a67d76d11e0ee", "167e57a0b974f9a1203b5cdd90d32416c30e28cb885f9afef7448bebe7cb54fe"},
		{"windows", "amd64", "a2686d616e6966657374a76776657273696f6e0168706c7567696e49646e61706c65786963612d636c6f75646b70726f746f636f6c4d6178016b70726f746f636f6c4d696e016c62696e6172795368613235365820a7cb5d6e3d6ea610b6ea587ed29244e5f9888e12303d34133a079a47899f5c4c6c6361706162696c6974696573836e696e626f756e645f61636b5f76326d706169725f737464696e5f76317174727573745f70726f746f636f6c5f76316d706c7567696e56657273696f6e6676302e312e31697369676e617475726558404aa060b2eecb2153906b7d7d2670e350a77ae036f9b1c7dbdb979af56b59a8f4dfca6eeca8ef9900ae375741aaae352ba1c966754c3a71d8ff199fb7b912970b", "a7cb5d6e3d6ea610b6ea587ed29244e5f9888e12303d34133a079a47899f5c4c", "6fa83ee6dc0e171130e96813dcd667daa0292c79442f0f613e8fe0114d61a952"},
		{"windows", "arm64", "a2686d616e6966657374a76776657273696f6e0168706c7567696e49646e61706c65786963612d636c6f75646b70726f746f636f6c4d6178016b70726f746f636f6c4d696e016c62696e617279536861323536582083aa479885dbc683ae8035e5a0f3711aae540f7aebd67b6aee986075f8d9146b6c6361706162696c6974696573836e696e626f756e645f61636b5f76326d706169725f737464696e5f76317174727573745f70726f746f636f6c5f76316d706c7567696e56657273696f6e6676302e312e31697369676e61747572655840d527c12fe46e722dc96cd457a71ec86083e212261d379493fb5e7b2426df01254184be39a7bc6ea9a37c8edec24f44e4edc5407d09a32296dca05c74e610f807", "83aa479885dbc683ae8035e5a0f3711aae540f7aebd67b6aee986075f8d9146b", "4fb93d5d71f2f4c44fd8ca0b256af37154117bb7fc75d6738c8f4264e7cb2244"},
	}
	oldRaw, err := hex.DecodeString(remotePluginPublisherPublicKeyHex)
	require.NoError(t, err)
	newRaw, err := hex.DecodeString(providerNeutralRemotePluginPublisherPublicKeyHex)
	require.NoError(t, err)
	oldKey, newKey := ed25519.PublicKey(oldRaw), ed25519.PublicKey(newRaw)
	enc, err := cbor.CanonicalEncOptions().EncMode()
	require.NoError(t, err)
	dec, err := cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden, ExtraReturnErrors: cbor.ExtraDecErrorUnknownField}.DecMode()
	require.NoError(t, err)
	for _, fixture := range fixtures {
		t.Run(fixture.goos+"-"+fixture.goarch, func(t *testing.T) {
			raw, decodeErr := hex.DecodeString(fixture.rawHex)
			require.NoError(t, decodeErr)
			require.Equal(t, mustRemotePluginDigest(fixture.manifestHex), sha256.Sum256(raw))
			var signed proto.RemotePluginManifestV1
			require.NoError(t, dec.Unmarshal(raw, &signed))
			require.Equal(t, "v0.1.1", signed.Manifest.PluginVersion)
			require.Equal(t, mustRemotePluginDigest(fixture.binaryHex), signed.Manifest.BinarySHA256)
			preimage, encodeErr := enc.Marshal([]any{"aplexica/remote-plugin-manifest/v1", signed.Manifest})
			require.NoError(t, encodeErr)
			require.False(t, ed25519.Verify(oldKey, preimage, signed.Signature[:]), "retained fixture unexpectedly verifies under old GitHub root")
			require.True(t, ed25519.Verify(newKey, preimage, signed.Signature[:]), "retained fixture must verify under provider-neutral root")
			var found bool
			for _, allowed := range remotePluginLegacyOverlapArtifacts {
				if allowed.GOOS == fixture.goos && allowed.GOARCH == fixture.goarch && allowed.BinarySHA256 == signed.Manifest.BinarySHA256 &&
					allowed.ManifestSHA256 == sha256.Sum256(raw) && allowed.PublisherKeySHA256 == sha256.Sum256(newKey) {
					found = true
				}
			}
			require.True(t, found, "retained fixture is not exactly represented in the finite allowlist")
		})
	}
}

func TestBalancedRemotePluginV0112IsExactEgressSuccessor(t *testing.T) {
	require.Len(t, balancedRemotePluginArtifacts, 26)

	wantPrevious := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("f81c12f0c8c3474e6a2e884a2cca4a846fe4eb311bcdfba165d0703fbd979a06"),
		"windows/amd64": mustRemotePluginDigest("f979bf84a8c0e0150c05b601e2f58e891f63aa0242833d3a760967ad9f1ee86c"),
	}
	wantBinary := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("a51d6ab9863a9df9b75eec1c458bcf3f2f26c462b28a284345a4dcf76d514c9f"),
		"windows/amd64": mustRemotePluginDigest("5aaaf080d137991feb0c861c5b166f45d440df5e793c4762272d7bcf840df7e3"),
	}
	wantInventory := map[string][32]byte{
		"darwin/arm64":  mustRemotePluginDigest("ddc93d919649909b522081211aa35b553b57df7912a40af39be470a71f91da13"),
		"windows/amd64": mustRemotePluginDigest("369126936ad422bdaba12505cb9f022f225aab2d8c32163f086c6c46bfbac168"),
	}
	// v0.1.12 carries the FR debounce/probe/backoff/provenance set plus the
	// SR shadow-lane hardening (poison-pill, self-healing evidence classifier,
	// journal GC, operation-scoped conflict labels). Runtime behaviour only;
	// the negotiated capability set is identical to v0.1.11.
	wantCapabilities := []string{
		proto.CapabilityDurableCursorResumeV1,
		proto.CapabilityDurableDeltaSyncV1,
		proto.CapabilityDurableMultiStreamV1,
		proto.CapabilityDurableSyncObservationV1,
		proto.CapabilityInboundAckV2,
		proto.CapabilityInboundFinalizeV1,
		proto.CapabilityPairStdinV1,
		proto.CapabilityRedactionSafeBatchV1,
		proto.CapabilityStagedCheckpointV1,
		proto.CapabilityTrustProtocolV1,
	}
	for _, authorization := range balancedRemotePluginArtifacts[20:22] {
		platform := authorization.GOOS + "/" + authorization.GOARCH
		require.Equal(t, "v0.1.12", authorization.PluginVersion, platform)
		require.Equal(t, uint64(11), authorization.Sequence, platform)
		require.Equal(t, uint64(11), authorization.RollbackFloor, platform)
		require.NotNil(t, authorization.Previous, platform)
		require.Equal(t, uint64(10), authorization.Previous.Sequence, platform)
		require.Equal(t, "v0.1.11", authorization.Previous.PluginVersion, platform)
		require.Equal(t, wantPrevious[platform], authorization.Previous.InventorySHA256, platform)
		require.Equal(t, wantCapabilities, authorization.Capabilities, platform)
		require.Equal(t, wantBinary[platform], authorization.BinarySHA256, platform)
		require.Equal(t, wantInventory[platform], balancedRemoteInventoryDigestForTest(t, authorization), platform)
	}
}
