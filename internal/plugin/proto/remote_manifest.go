package proto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/fxamacker/cbor/v2"
)

const (
	RemotePluginManifestSuffix         = ".manifest.cbor"
	remotePluginInventoryAliasSuffix   = ".inventory.cbor"
	CapabilityPairStdinV1              = "pair_stdin_v1"
	CapabilityTrustProtocolV1          = "trust_protocol_v1"
	CapabilityInboundAckV2             = "inbound_ack_v2"
	CapabilityInboundFinalizeV1        = "inbound_finalize_v1"
	CapabilityDurableDeltaSyncV1       = "durable_delta_sync_v1"
	CapabilityDurableCursorResumeV1    = "durable_cursor_resume_v1"
	CapabilityDurableMultiStreamV1     = "durable_multistream_v1"
	CapabilityRedactionSafeBatchV1     = "redaction_safe_replay_batch_v1"
	CapabilityStagedCheckpointV1       = "staged_checkpoint_transfer_v1"
	CapabilityDurableSyncObservationV1 = "durable_sync_observation_v1"
	maxRemotePluginBinaryBytes         = 256 << 20
)

// RemotePluginManifestUnsignedV1 retains its exported historical name for API
// compatibility. Version 2 uses the additional monotonic fields below; their
// omitempty encoding keeps the frozen v1 preimage byte-for-byte unchanged.
type RemotePluginManifestUnsignedV1 struct {
	Version       uint16                `cbor:"version"`
	PluginID      string                `cbor:"pluginId"`
	PluginVersion string                `cbor:"pluginVersion"`
	Sequence      uint64                `cbor:"sequence,omitempty"`
	RollbackFloor uint64                `cbor:"rollbackFloor,omitempty"`
	Previous      *RemotePluginPrevious `cbor:"previous,omitempty"`
	BinarySHA256  [32]byte              `cbor:"binarySha256"`
	Capabilities  []string              `cbor:"capabilities"`
	ProtocolMin   uint16                `cbor:"protocolMin"`
	ProtocolMax   uint16                `cbor:"protocolMax"`
}

type RemotePluginPrevious struct {
	Sequence        uint64   `cbor:"sequence"`
	PluginVersion   string   `cbor:"pluginVersion"`
	InventorySHA256 [32]byte `cbor:"inventorySha256"`
}

type RemotePluginManifestV1 struct {
	Manifest  RemotePluginManifestUnsignedV1 `cbor:"manifest"`
	Signature [64]byte                       `cbor:"signature"`
}

// VerifiedRemotePlugin is deterministic evidence returned after the signed
// manifest, trusted path, executable digest, and publisher signature have all
// verified. PublisherKeySHA256 identifies which member of an overlapping
// publisher-key ring accepted the manifest without exposing any private data.
type VerifiedRemotePlugin struct {
	Manifest           RemotePluginManifestUnsignedV1
	PublisherKeySHA256 [32]byte
	ManifestSHA256     [32]byte
	InventorySHA256    [32]byte
}

var (
	manifestToken      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	gitObjectIDPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

func (m RemotePluginManifestUnsignedV1) HasCapability(capability string) bool {
	i := sort.SearchStrings(m.Capabilities, capability)
	return i < len(m.Capabilities) && m.Capabilities[i] == capability
}

func validateRemotePluginManifest(m RemotePluginManifestUnsignedV1) error {
	if (m.Version != 1 && m.Version != 2) || m.PluginID != "aplexica-cloud" || !manifestToken.MatchString(m.PluginVersion) || m.BinarySHA256 == ([32]byte{}) || m.ProtocolMin == 0 || m.ProtocolMax < m.ProtocolMin || len(m.Capabilities) == 0 || len(m.Capabilities) > 32 {
		return fmt.Errorf("plugin/proto: invalid signed remote manifest")
	}
	if m.Version == 1 {
		if m.Sequence != 0 || m.RollbackFloor != 0 || m.Previous != nil {
			return fmt.Errorf("plugin/proto: legacy manifest carries v2 release fields")
		}
	} else if err := validateRemotePluginSequence(m); err != nil {
		return err
	}
	allowed := map[string]bool{
		CapabilityDurableCursorResumeV1:    true,
		CapabilityDurableDeltaSyncV1:       true,
		CapabilityDurableMultiStreamV1:     true,
		CapabilityInboundAckV2:             true,
		CapabilityInboundFinalizeV1:        true,
		CapabilityPairStdinV1:              true,
		CapabilityRedactionSafeBatchV1:     true,
		CapabilityStagedCheckpointV1:       true,
		CapabilityDurableSyncObservationV1: true,
		CapabilityTrustProtocolV1:          true,
	}
	for i, capability := range m.Capabilities {
		if !allowed[capability] || (i > 0 && m.Capabilities[i-1] >= capability) {
			return fmt.Errorf("plugin/proto: invalid remote capability list")
		}
	}
	return nil
}

func validateRemotePluginSequence(m RemotePluginManifestUnsignedV1) error {
	if m.Sequence == 0 || m.RollbackFloor == 0 || m.RollbackFloor > m.Sequence {
		return fmt.Errorf("plugin/proto: invalid release sequence or rollback floor")
	}
	if m.Sequence == 1 {
		if m.RollbackFloor != 1 || m.Previous != nil {
			return fmt.Errorf("plugin/proto: invalid bootstrap release identity")
		}
		return nil
	}
	if m.Previous == nil || m.Previous.Sequence+1 != m.Sequence || m.Previous.InventorySHA256 == ([32]byte{}) || !manifestToken.MatchString(m.Previous.PluginVersion) {
		return fmt.Errorf("plugin/proto: release does not bind its exact immediate predecessor")
	}
	return nil
}

func VerifyRemotePlugin(execPath string, publisherKeys []ed25519.PublicKey) (RemotePluginManifestUnsignedV1, error) {
	verified, err := VerifyRemotePluginDetailed(execPath, publisherKeys)
	if err != nil {
		return RemotePluginManifestUnsignedV1{}, err
	}
	return verified.Manifest, nil
}

// VerifyRemotePluginDetailed is VerifyRemotePlugin plus the fingerprint of the
// publisher key that verified the manifest. Callers that do not need rotation
// evidence should continue to use VerifyRemotePlugin.
func VerifyRemotePluginDetailed(execPath string, publisherKeys []ed25519.PublicKey) (VerifiedRemotePlugin, error) {
	if !filepath.IsAbs(execPath) || len(publisherKeys) == 0 {
		return VerifiedRemotePlugin{}, fmt.Errorf("plugin/proto: publisher trust unavailable")
	}
	manifestPath := execPath + RemotePluginManifestSuffix
	manifestInput, err := privatefs.OpenTrustedInput(manifestPath, privatefs.TrustedInputPolicy{MaxBytes: 64 << 10, AllowSystemOwner: true})
	if err != nil {
		return VerifiedRemotePlugin{}, fmt.Errorf("plugin/proto: open signed remote manifest: %w", err)
	}
	dec, err := cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden, MaxNestedLevels: 8, MaxArrayElements: 64, MaxMapPairs: 32, ExtraReturnErrors: cbor.ExtraDecErrorUnknownField}.DecMode()
	if err != nil {
		return VerifiedRemotePlugin{}, err
	}
	var signed RemotePluginManifestV1
	if err := dec.Unmarshal(manifestInput.Bytes, &signed); err != nil {
		return VerifiedRemotePlugin{}, fmt.Errorf("plugin/proto: decode signed remote manifest: %w", err)
	}
	if err := validateRemotePluginManifest(signed.Manifest); err != nil {
		return VerifiedRemotePlugin{}, err
	}
	enc, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		return VerifiedRemotePlugin{}, err
	}
	canonicalManifest, err := enc.Marshal(signed)
	if err != nil || !bytes.Equal(canonicalManifest, manifestInput.Bytes) {
		return VerifiedRemotePlugin{}, fmt.Errorf("plugin/proto: signed remote manifest is not canonical")
	}
	domain := "aplexica/remote-plugin-manifest/v1"
	if signed.Manifest.Version == 2 {
		domain = "aplexica/remote-plugin-manifest/v2"
	}
	preimage, err := enc.Marshal([]any{domain, signed.Manifest})
	if err != nil {
		return VerifiedRemotePlugin{}, err
	}
	var publisherKeySHA256 [32]byte
	verified := false
	for _, key := range publisherKeys {
		if len(key) == ed25519.PublicKeySize && ed25519.Verify(key, preimage, signed.Signature[:]) {
			publisherKeySHA256 = sha256.Sum256(key)
			verified = true
			break
		}
	}
	if !verified {
		return VerifiedRemotePlugin{}, fmt.Errorf("plugin/proto: remote manifest signature invalid")
	}
	binary, err := privatefs.OpenTrustedInput(execPath, privatefs.TrustedInputPolicy{MaxBytes: maxRemotePluginBinaryBytes, RequireExecutable: true, AllowSystemOwner: true})
	if err != nil {
		return VerifiedRemotePlugin{}, fmt.Errorf("plugin/proto: open trusted remote binary: %w", err)
	}
	if sha256.Sum256(binary.Bytes) != signed.Manifest.BinarySHA256 {
		return VerifiedRemotePlugin{}, fmt.Errorf("plugin/proto: remote binary digest mismatch")
	}
	result := VerifiedRemotePlugin{Manifest: signed.Manifest, PublisherKeySHA256: publisherKeySHA256,
		ManifestSHA256: sha256.Sum256(manifestInput.Bytes)}
	if signed.Manifest.Version == 2 {
		inventoryBytes, openErr := openRuntimeInventory(execPath)
		if openErr != nil {
			return VerifiedRemotePlugin{}, fmt.Errorf("plugin/proto: open signed release inventory: %w", openErr)
		}
		if verifyErr := verifyRuntimeInventory(inventoryBytes, manifestInput.Bytes, binary.Bytes, publisherKeys, result); verifyErr != nil {
			return VerifiedRemotePlugin{}, fmt.Errorf("plugin/proto: verify signed release inventory: %w", verifyErr)
		}
		result.InventorySHA256 = sha256.Sum256(inventoryBytes)
	}
	return result, nil
}

func openRuntimeInventory(execPath string) ([]byte, error) {
	// There is one canonical runtime inventory location. An executable-specific
	// alias is rejected even if its bytes happen to match, so deployment and
	// checkpoint identity never depend on path precedence.
	alias := execPath + remotePluginInventoryAliasSuffix
	if _, err := os.Lstat(alias); err == nil {
		return nil, errors.New("unexpected executable-specific inventory alias")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(execPath), "release.inventory.cbor")
	input, err := privatefs.OpenTrustedInput(path, privatefs.TrustedInputPolicy{MaxBytes: 4 << 20, AllowSystemOwner: true})
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), input.Bytes...), nil
}

type runtimeInventoryPrevious struct {
	Sequence        uint64   `cbor:"sequence"`
	PluginVersion   string   `cbor:"pluginVersion"`
	InventorySHA256 [32]byte `cbor:"inventorySha256"`
}

type runtimeInventorySource struct {
	Repository      string   `cbor:"repository"`
	Tag             string   `cbor:"tag"`
	TagObject       string   `cbor:"tagObject"`
	Commit          string   `cbor:"commit"`
	Tree            string   `cbor:"tree"`
	TagSignerSHA256 [32]byte `cbor:"tagSignerSha256"`
	PolicySHA256    [32]byte `cbor:"policySha256"`
	SourceDateEpoch int64    `cbor:"sourceDateEpoch"`
	BuildDate       string   `cbor:"buildDate"`
}

type runtimeInventoryArtifact struct {
	Name   string   `cbor:"name"`
	SHA256 [32]byte `cbor:"sha256"`
	Size   uint64   `cbor:"size"`
}

type runtimeVerifiedSource struct {
	Claim                    runtimeInventoryArtifact `cbor:"claim"`
	Attestation              runtimeInventoryArtifact `cbor:"attestation"`
	ClaimAuthorityKeySHA256  [32]byte                 `cbor:"claimAuthorityKeySha256"`
	ClaimAuthorityBlobSHA256 [32]byte                 `cbor:"claimAuthorityBlobSha256"`
}

type runtimeToolDescriptor struct {
	Name         string   `cbor:"name"`
	Version      string   `cbor:"version"`
	BinarySHA256 [32]byte `cbor:"binarySha256"`
	TreeSHA256   [32]byte `cbor:"treeSha256"`
}

type runtimeToolchain struct {
	GoVersion         string                   `cbor:"goVersion"`
	GoDirective       string                   `cbor:"goDirective"`
	ModulePath        string                   `cbor:"modulePath"`
	GoModSHA256       [32]byte                 `cbor:"goModSha256"`
	GoSumSHA256       [32]byte                 `cbor:"goSumSha256"`
	ModuleGraphSHA256 [32]byte                 `cbor:"moduleGraphSha256"`
	GoModCacheSHA256  [32]byte                 `cbor:"goModCacheSha256"`
	VulnDBSHA256      [32]byte                 `cbor:"vulnDbSha256"`
	VulnDBManifest    runtimeInventoryArtifact `cbor:"vulnDbManifest"`
	Tools             []runtimeToolDescriptor  `cbor:"tools"`
}

type runtimeBuildPolicy struct {
	CGOEnabled          bool   `cbor:"cgoEnabled"`
	TrimPath            bool   `cbor:"trimPath"`
	BuildVCS            bool   `cbor:"buildVcs"`
	NetworkAllowed      bool   `cbor:"networkAllowed"`
	CallerWorktreeUsed  bool   `cbor:"callerWorktreeUsed"`
	LinkerBuildID       string `cbor:"linkerBuildId"`
	GOAMD64             string `cbor:"goamd64"`
	GOARM64             string `cbor:"goarm64"`
	ReproducibilityRuns uint8  `cbor:"reproducibilityRuns"`
	CommandTimeoutSecs  uint32 `cbor:"commandTimeoutSecs"`
}

type runtimeGate struct {
	Name          string                   `cbor:"name"`
	SubjectSHA256 [32]byte                 `cbor:"subjectSha256"`
	PolicySHA256  [32]byte                 `cbor:"policySha256"`
	Summary       runtimeInventoryArtifact `cbor:"summary"`
	Evidence      runtimeInventoryArtifact `cbor:"evidence"`
}

type runtimeInventoryTarget struct {
	GOOS     string                   `cbor:"goos"`
	GOARCH   string                   `cbor:"goarch"`
	Binary   runtimeInventoryArtifact `cbor:"binary"`
	Manifest runtimeInventoryArtifact `cbor:"manifest"`
}

type runtimeInventoryV1 struct {
	Version            uint16                    `cbor:"version"`
	PluginID           string                    `cbor:"pluginId"`
	PluginVersion      string                    `cbor:"pluginVersion"`
	Sequence           uint64                    `cbor:"sequence"`
	RollbackFloor      uint64                    `cbor:"rollbackFloor"`
	Previous           *runtimeInventoryPrevious `cbor:"previous,omitempty"`
	Source             runtimeInventorySource    `cbor:"source"`
	VerifiedSource     runtimeVerifiedSource     `cbor:"verifiedSource"`
	Toolchain          runtimeToolchain          `cbor:"toolchain"`
	PolicyFile         runtimeInventoryArtifact  `cbor:"policyFile"`
	RecipeFile         runtimeInventoryArtifact  `cbor:"recipeFile"`
	Policy             runtimeBuildPolicy        `cbor:"policy"`
	Gates              []runtimeGate             `cbor:"gates"`
	PublisherKeySHA256 [32]byte                  `cbor:"publisherKeySha256"`
	BuildRecord        runtimeInventoryArtifact  `cbor:"buildRecord"`
	Targets            []runtimeInventoryTarget  `cbor:"targets"`
}

type signedRuntimeInventory struct {
	Inventory cbor.RawMessage `cbor:"inventory"`
	Signature [64]byte        `cbor:"signature"`
}

func verifyRuntimeInventory(input, manifestBytes, binaryBytes []byte, publisherKeys []ed25519.PublicKey, verified VerifiedRemotePlugin) error {
	strict, err := cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden, MaxNestedLevels: 24, MaxArrayElements: 256, MaxMapPairs: 128, ExtraReturnErrors: cbor.ExtraDecErrorUnknownField}.DecMode()
	if err != nil {
		return err
	}
	var generic any
	if err := strict.Unmarshal(input, &generic); err != nil {
		return err
	}
	enc, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		return err
	}
	canonical, err := enc.Marshal(generic)
	if err != nil || !bytes.Equal(canonical, input) {
		return fmt.Errorf("release inventory is not canonical")
	}
	var signed signedRuntimeInventory
	if err := strict.Unmarshal(input, &signed); err != nil || len(signed.Inventory) == 0 {
		return fmt.Errorf("invalid signed release inventory envelope")
	}
	var inventory runtimeInventoryV1
	if err := strict.Unmarshal(signed.Inventory, &inventory); err != nil {
		return fmt.Errorf("decode release inventory identity: %w", err)
	}
	manifest := verified.Manifest
	if inventory.Version != 1 || inventory.PluginID != manifest.PluginID || inventory.PluginVersion != manifest.PluginVersion ||
		inventory.Sequence != manifest.Sequence || inventory.RollbackFloor != manifest.RollbackFloor ||
		inventory.PublisherKeySHA256 != verified.PublisherKeySHA256 || !sameRuntimePrevious(inventory.Previous, manifest.Previous) {
		return fmt.Errorf("release inventory and runtime manifest identities differ")
	}
	if err := validateCompleteRuntimeInventory(inventory); err != nil {
		return err
	}
	preimage, err := enc.Marshal([]any{"aplexica/plugin-release-inventory/v1", cbor.RawMessage(signed.Inventory)})
	if err != nil {
		return err
	}
	validSignature := false
	for _, key := range publisherKeys {
		if len(key) == ed25519.PublicKeySize && sha256.Sum256(key) == inventory.PublisherKeySHA256 && ed25519.Verify(key, preimage, signed.Signature[:]) {
			validSignature = true
			break
		}
	}
	if !validSignature {
		return fmt.Errorf("release inventory signature invalid")
	}
	binaryDigest := sha256.Sum256(binaryBytes)
	manifestDigest := sha256.Sum256(manifestBytes)
	matched := 0
	wantTargets := []struct{ goos, goarch string }{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}, {"windows", "arm64"}}
	if len(inventory.Targets) != len(wantTargets) {
		return fmt.Errorf("release inventory does not contain every supported runtime target")
	}
	for index, target := range inventory.Targets {
		expectedName := fmt.Sprintf("aplexica-cloud-plugin-%s-%s-%s", inventory.PluginVersion, target.GOOS, target.GOARCH)
		if target.GOOS == "windows" {
			expectedName += ".exe"
		}
		if target.GOOS != wantTargets[index].goos || target.GOARCH != wantTargets[index].goarch || target.Binary.Name != expectedName ||
			target.Binary.SHA256 == ([32]byte{}) || target.Binary.Size == 0 || target.Manifest.SHA256 == ([32]byte{}) || target.Manifest.Size == 0 ||
			target.Manifest.Name != target.Binary.Name+RemotePluginManifestSuffix {
			return fmt.Errorf("release inventory has an invalid runtime target at index %d", index)
		}
		if target.GOOS == runtime.GOOS && target.GOARCH == runtime.GOARCH && target.Binary.SHA256 == binaryDigest &&
			target.Binary.Size == uint64(len(binaryBytes)) && target.Manifest.SHA256 == manifestDigest && target.Manifest.Size == uint64(len(manifestBytes)) &&
			target.Binary.Name != "" {
			matched++
		}
	}
	if matched != 1 {
		return fmt.Errorf("release inventory does not bind exactly one runtime target")
	}
	return nil
}

func validateCompleteRuntimeInventory(inventory runtimeInventoryV1) error {
	nonzero := func(value [32]byte) bool { return value != ([32]byte{}) }
	artifact := func(value runtimeInventoryArtifact, name string) bool {
		return value.Name == name && nonzero(value.SHA256) && value.Size > 0 && value.Size <= 512<<20
	}
	source := inventory.Source
	if source.Repository != "aplexica/aplexica-cloud" || source.Tag != inventory.PluginVersion || !gitObjectIDPattern.MatchString(source.TagObject) ||
		!gitObjectIDPattern.MatchString(source.Commit) || !gitObjectIDPattern.MatchString(source.Tree) || len(source.TagObject) != len(source.Commit) ||
		len(source.Commit) != len(source.Tree) || !nonzero(source.TagSignerSHA256) || !nonzero(source.PolicySHA256) || source.SourceDateEpoch <= 0 ||
		source.BuildDate != time.Unix(source.SourceDateEpoch, 0).UTC().Format(time.RFC3339) {
		return fmt.Errorf("release inventory has incomplete source authorization")
	}
	if !artifact(inventory.VerifiedSource.Claim, "verified-source.json") || !artifact(inventory.VerifiedSource.Attestation, "verified-source.sig.json") ||
		!nonzero(inventory.VerifiedSource.ClaimAuthorityKeySHA256) || !nonzero(inventory.VerifiedSource.ClaimAuthorityBlobSHA256) ||
		inventory.VerifiedSource.ClaimAuthorityBlobSHA256 == source.TagSignerSHA256 || source.TagSignerSHA256 == inventory.PublisherKeySHA256 ||
		inventory.VerifiedSource.ClaimAuthorityBlobSHA256 == inventory.PublisherKeySHA256 {
		return fmt.Errorf("release inventory has incomplete verified-source authorization")
	}
	toolchain := inventory.Toolchain
	if toolchain.GoVersion == "" || toolchain.GoDirective == "" || toolchain.ModulePath != "github.com/aplexica/aplexica-cloud/plugin" ||
		!nonzero(toolchain.GoModSHA256) || !nonzero(toolchain.GoSumSHA256) || !nonzero(toolchain.ModuleGraphSHA256) || !nonzero(toolchain.GoModCacheSHA256) ||
		!nonzero(toolchain.VulnDBSHA256) || !artifact(toolchain.VulnDBManifest, "vuln-db-manifest.json") || len(toolchain.Tools) != 10 {
		return fmt.Errorf("release inventory has incomplete toolchain authorization")
	}
	wantTools := []string{"build-sandbox-profile", "git", "go", "golangci-lint", "govulncheck", "hdiutil", "plugin-release", "sandbox-exec", "source-sandbox-profile", "ssh-keygen"}
	for index, tool := range toolchain.Tools {
		if tool.Name != wantTools[index] || tool.Version == "" || !nonzero(tool.BinarySHA256) {
			return fmt.Errorf("release inventory has invalid tool descriptor at index %d", index)
		}
		if (tool.Name == "git" || tool.Name == "go") && !nonzero(tool.TreeSHA256) {
			return fmt.Errorf("release inventory tool runtime tree is missing")
		}
	}
	if !artifact(inventory.PolicyFile, "release-policy.json") || inventory.PolicyFile.SHA256 != source.PolicySHA256 ||
		!artifact(inventory.RecipeFile, "build-recipe.json") || !artifact(inventory.BuildRecord, "release.build.cbor") {
		return fmt.Errorf("release inventory has incomplete policy, recipe, or build record")
	}
	wantPolicy := runtimeBuildPolicy{TrimPath: true, GOAMD64: "v1", GOARM64: "v8.0", ReproducibilityRuns: 2, CommandTimeoutSecs: 1800}
	if inventory.Policy != wantPolicy {
		return fmt.Errorf("release inventory uses unsupported build policy")
	}
	wantGates := []string{"test", "lint", "reproducible_build", "vulnerability_scan"}
	if len(inventory.Gates) != len(wantGates) {
		return fmt.Errorf("release inventory does not contain every release gate")
	}
	for index, gate := range inventory.Gates {
		if gate.Name != wantGates[index] || !nonzero(gate.SubjectSHA256) || gate.PolicySHA256 != inventory.PolicyFile.SHA256 ||
			!artifact(gate.Summary, "gate-"+gate.Name+".json") || !artifact(gate.Evidence, "gate-"+gate.Name+".evidence.json") {
			return fmt.Errorf("release inventory has invalid gate at index %d", index)
		}
	}
	return nil
}

func sameRuntimePrevious(inventory *runtimeInventoryPrevious, manifest *RemotePluginPrevious) bool {
	if inventory == nil || manifest == nil {
		return inventory == nil && manifest == nil
	}
	return inventory.Sequence == manifest.Sequence && inventory.PluginVersion == manifest.PluginVersion && inventory.InventorySHA256 == manifest.InventorySHA256
}
