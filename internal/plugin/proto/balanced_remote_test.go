package proto

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifyBalancedRemotePluginDetailedBindsExactCompiledBytes(t *testing.T) {
	root := trustedInputTestDir(t)
	executable := filepath.Join(root, "aplexica-cloud-plugin")
	binary := []byte("balanced plugin fixture")
	require.NoError(t, os.WriteFile(executable, binary, 0o700))
	authorization := BalancedRemotePluginAuthorization{
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, PluginVersion: "v0.1.2",
		Sequence: 1, RollbackFloor: 1, BinarySHA256: sha256.Sum256(binary),
		PublisherKeySHA256: sha256.Sum256([]byte("balanced release public key")),
	}

	verified, err := VerifyBalancedRemotePluginDetailed(executable, authorization)
	require.NoError(t, err)
	require.Equal(t, uint16(2), verified.Manifest.Version)
	require.Equal(t, "v0.1.2", verified.Manifest.PluginVersion)
	require.NotEqual(t, [32]byte{}, verified.ManifestSHA256)
	require.NotEqual(t, [32]byte{}, verified.InventorySHA256)
	require.NotEqual(t, verified.ManifestSHA256, verified.InventorySHA256)
	require.True(t, verified.Manifest.HasCapability(CapabilityInboundAckV2))
	// Existing compiled authorizations remain legacy-default. A future
	// balanced release must deliberately bind durable_delta_sync_v1 rather
	// than inheriting it merely because the daemon understands the contract.
	require.False(t, verified.Manifest.HasCapability(CapabilityDurableDeltaSyncV1))

	require.NoError(t, os.WriteFile(executable, []byte("substituted plugin"), 0o700))
	_, err = VerifyBalancedRemotePluginDetailed(executable, authorization)
	require.ErrorContains(t, err, "digest mismatch")
}

func TestVerifyBalancedRemotePluginDetailedRejectsWrongPlatformAndBrokenChain(t *testing.T) {
	authorization := BalancedRemotePluginAuthorization{
		GOOS: "not-" + runtime.GOOS, GOARCH: runtime.GOARCH, PluginVersion: "v0.1.2",
		Sequence: 1, RollbackFloor: 1, BinarySHA256: sha256.Sum256([]byte("binary")),
		PublisherKeySHA256: sha256.Sum256([]byte("publisher")),
	}
	_, err := VerifyBalancedRemotePluginDetailed(filepath.Join(t.TempDir(), "missing"), authorization)
	require.ErrorContains(t, err, "different platform")

	authorization.GOOS = runtime.GOOS
	authorization.Sequence = 2
	_, err = VerifyBalancedRemotePluginDetailed(filepath.Join(t.TempDir(), "missing"), authorization)
	require.ErrorContains(t, err, "immediate predecessor")
}

func TestBalancedRemotePluginManifestBindsExplicitSortedCapabilities(t *testing.T) {
	authorization := BalancedRemotePluginAuthorization{
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, PluginVersion: "v0.1.7",
		Sequence: 6, RollbackFloor: 6,
		Previous: &RemotePluginPrevious{
			Sequence: 5, PluginVersion: "v0.1.6", InventorySHA256: sha256.Sum256([]byte("previous inventory")),
		},
		Capabilities: []string{
			CapabilityDurableDeltaSyncV1,
			CapabilityInboundAckV2,
			CapabilityInboundFinalizeV1,
			CapabilityPairStdinV1,
			CapabilityTrustProtocolV1,
		},
		BinarySHA256:       sha256.Sum256([]byte("binary")),
		PublisherKeySHA256: sha256.Sum256([]byte("publisher")),
	}

	manifest, err := balancedRemotePluginManifest(authorization)
	require.NoError(t, err)
	require.Equal(t, authorization.Capabilities, manifest.Capabilities)
	require.True(t, manifest.HasCapability(CapabilityDurableDeltaSyncV1))

	authorization.Capabilities[0], authorization.Capabilities[1] = authorization.Capabilities[1], authorization.Capabilities[0]
	_, err = balancedRemotePluginManifest(authorization)
	require.ErrorContains(t, err, "capability list")
}
