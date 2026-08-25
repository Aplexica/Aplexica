package proto

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"runtime"

	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/fxamacker/cbor/v2"
)

// BalancedRemotePluginAuthorization is the exact plugin identity compiled into
// a Balanced desktop-suite daemon. The desktop release signature authenticates
// the daemon and plugin together; this record binds the runtime to those exact
// plugin bytes without reintroducing a separate hosted or Keychain publisher.
//
// A new plugin release therefore requires a new signed daemon release. The
// durable plugin checkpoint still enforces the sequence and predecessor chain.
type BalancedRemotePluginAuthorization struct {
	GOOS          string
	GOARCH        string
	PluginVersion string
	Sequence      uint64
	RollbackFloor uint64
	Previous      *RemotePluginPrevious
	// Capabilities is optional so the frozen authorizations for older
	// Balanced releases retain their exact synthetic manifest and inventory
	// digests. New releases set the complete sorted capability list explicitly.
	Capabilities       []string
	BinarySHA256       [32]byte
	PublisherKeySHA256 [32]byte
}

// VerifyBalancedRemotePluginDetailed verifies one exact compiled Balanced
// authorization. It is deliberately separate from the standalone publisher
// manifest verifier: callers may attempt this path only against a finite table
// shipped in the signed daemon binary.
func VerifyBalancedRemotePluginDetailed(execPath string, authorization BalancedRemotePluginAuthorization) (VerifiedRemotePlugin, error) {
	manifest, err := balancedRemotePluginManifest(authorization)
	if err != nil {
		return VerifiedRemotePlugin{}, err
	}
	if runtime.GOOS != authorization.GOOS || runtime.GOARCH != authorization.GOARCH {
		return VerifiedRemotePlugin{}, errors.New("plugin/proto: balanced authorization is for a different platform")
	}
	binary, err := privatefs.OpenTrustedInput(execPath, privatefs.TrustedInputPolicy{
		MaxBytes: maxRemotePluginBinaryBytes, RequireExecutable: true, AllowSystemOwner: true,
	})
	if err != nil {
		return VerifiedRemotePlugin{}, fmt.Errorf("plugin/proto: open balanced remote binary: %w", err)
	}
	if sha256.Sum256(binary.Bytes) != authorization.BinarySHA256 {
		return VerifiedRemotePlugin{}, errors.New("plugin/proto: balanced remote binary digest mismatch")
	}
	manifestDigest, inventoryDigest, err := balancedRemotePluginDigests(authorization, manifest)
	if err != nil {
		return VerifiedRemotePlugin{}, err
	}
	return VerifiedRemotePlugin{
		Manifest:           manifest,
		PublisherKeySHA256: authorization.PublisherKeySHA256,
		ManifestSHA256:     manifestDigest,
		InventorySHA256:    inventoryDigest,
	}, nil
}

func balancedRemotePluginManifest(authorization BalancedRemotePluginAuthorization) (RemotePluginManifestUnsignedV1, error) {
	if authorization.GOOS == "" || authorization.GOARCH == "" || authorization.BinarySHA256 == ([32]byte{}) || authorization.PublisherKeySHA256 == ([32]byte{}) {
		return RemotePluginManifestUnsignedV1{}, errors.New("plugin/proto: invalid balanced remote authorization")
	}
	capabilities := []string{
		CapabilityInboundAckV2,
		CapabilityPairStdinV1,
		CapabilityTrustProtocolV1,
	}
	if len(authorization.Capabilities) > 0 {
		capabilities = append([]string(nil), authorization.Capabilities...)
	}
	manifest := RemotePluginManifestUnsignedV1{
		Version:       2,
		PluginID:      "aplexica-cloud",
		PluginVersion: authorization.PluginVersion,
		Sequence:      authorization.Sequence,
		RollbackFloor: authorization.RollbackFloor,
		Previous:      authorization.Previous,
		BinarySHA256:  authorization.BinarySHA256,
		Capabilities:  capabilities,
		ProtocolMin:   1,
		ProtocolMax:   1,
	}
	if err := validateRemotePluginManifest(manifest); err != nil {
		return RemotePluginManifestUnsignedV1{}, err
	}
	return manifest, nil
}

func balancedRemotePluginDigests(authorization BalancedRemotePluginAuthorization, manifest RemotePluginManifestUnsignedV1) ([32]byte, [32]byte, error) {
	enc, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		return [32]byte{}, [32]byte{}, err
	}
	identity := []any{authorization.GOOS, authorization.GOARCH, manifest, authorization.PublisherKeySHA256}
	manifestRaw, err := enc.Marshal([]any{"aplexica/balanced-remote-plugin-authorization/v1", identity})
	if err != nil {
		return [32]byte{}, [32]byte{}, err
	}
	inventoryRaw, err := enc.Marshal([]any{"aplexica/balanced-remote-plugin-inventory/v1", identity})
	if err != nil {
		return [32]byte{}, [32]byte{}, err
	}
	return sha256.Sum256(manifestRaw), sha256.Sum256(inventoryRaw), nil
}
