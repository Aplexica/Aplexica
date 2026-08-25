package proto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/require"
)

func validRemotePluginManifest(binary []byte) RemotePluginManifestUnsignedV1 {
	return RemotePluginManifestUnsignedV1{
		Version:       1,
		PluginID:      "aplexica-cloud",
		PluginVersion: "v1.2.3",
		BinarySHA256:  sha256.Sum256(binary),
		Capabilities:  []string{CapabilityInboundAckV2, CapabilityPairStdinV1, CapabilityTrustProtocolV1},
		ProtocolMin:   1,
		ProtocolMax:   1,
	}
}

// trustedInputTestDir keeps fixtures used by OpenTrustedInput out of the
// world-writable system temporary directory on Unix. The production verifier
// deliberately rejects every writable ancestor, including /tmp.
func trustedInputTestDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return t.TempDir()
	}
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	home, err = filepath.EvalSymlinks(home)
	require.NoError(t, err)
	dir, err := os.MkdirTemp(home, ".aplexica-trusted-input-test-")
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0o700))
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	return dir
}

func writeSignedRemotePluginFixture(t *testing.T, dir string, binary []byte, manifest RemotePluginManifestUnsignedV1, private ed25519.PrivateKey) string {
	t.Helper()
	binaryPath := filepath.Join(dir, "aplexica-cloud-plugin")
	require.NoError(t, os.WriteFile(binaryPath, binary, 0o700))
	binaryPath, err := filepath.EvalSymlinks(binaryPath)
	require.NoError(t, err)
	enc, err := cbor.CanonicalEncOptions().EncMode()
	require.NoError(t, err)
	domain := "aplexica/remote-plugin-manifest/v1"
	if manifest.Version == 2 {
		domain = "aplexica/remote-plugin-manifest/v2"
	}
	preimage, err := enc.Marshal([]any{domain, manifest})
	require.NoError(t, err)
	signed := RemotePluginManifestV1{Manifest: manifest}
	copy(signed.Signature[:], ed25519.Sign(private, preimage))
	encoded, err := enc.Marshal(signed)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(binaryPath+RemotePluginManifestSuffix, encoded, 0o600))
	return binaryPath
}

func writeSignedRuntimeInventoryFixture(t *testing.T, binaryPath string, binary, manifestBytes []byte, manifest RemotePluginManifestUnsignedV1, private ed25519.PrivateKey) []byte {
	t.Helper()
	public := private.Public().(ed25519.PublicKey)
	targets := make([]runtimeInventoryTarget, 0, 6)
	for _, target := range []struct{ goos, goarch string }{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}, {"windows", "arm64"}} {
		name := "aplexica-cloud-plugin-v1.2.3-" + target.goos + "-" + target.goarch
		if target.goos == "windows" {
			name += ".exe"
		}
		binaryValue := []byte("other " + target.goos + " " + target.goarch)
		manifestValue := []byte("other manifest " + target.goos + " " + target.goarch)
		if target.goos == runtime.GOOS && target.goarch == runtime.GOARCH {
			binaryValue, manifestValue = binary, manifestBytes
		}
		targets = append(targets, runtimeInventoryTarget{GOOS: target.goos, GOARCH: target.goarch,
			Binary:   runtimeInventoryArtifact{Name: name, SHA256: sha256.Sum256(binaryValue), Size: uint64(len(binaryValue))},
			Manifest: runtimeInventoryArtifact{Name: name + RemotePluginManifestSuffix, SHA256: sha256.Sum256(manifestValue), Size: uint64(len(manifestValue))}})
	}
	var previous *runtimeInventoryPrevious
	if manifest.Previous != nil {
		previous = &runtimeInventoryPrevious{Sequence: manifest.Previous.Sequence, PluginVersion: manifest.Previous.PluginVersion, InventorySHA256: manifest.Previous.InventorySHA256}
	}
	inventory := runtimeInventoryV1{Version: 1, PluginID: manifest.PluginID, PluginVersion: manifest.PluginVersion,
		Sequence: manifest.Sequence, RollbackFloor: manifest.RollbackFloor, Previous: previous,
		PublisherKeySHA256: sha256.Sum256(public), Targets: targets}
	artifact := func(name string) runtimeInventoryArtifact {
		return runtimeInventoryArtifact{Name: name, SHA256: sha256.Sum256([]byte(name)), Size: uint64(len(name))}
	}
	inventory.Source = runtimeInventorySource{Repository: "aplexica/aplexica-cloud", Tag: manifest.PluginVersion,
		TagObject: strings.Repeat("a", 40), Commit: strings.Repeat("b", 40), Tree: strings.Repeat("c", 40),
		TagSignerSHA256: sha256.Sum256([]byte("tag signer")), PolicySHA256: sha256.Sum256([]byte("release-policy.json")),
		SourceDateEpoch: 1_700_000_000, BuildDate: "2023-11-14T22:13:20Z"}
	inventory.VerifiedSource = runtimeVerifiedSource{Claim: artifact("verified-source.json"), Attestation: artifact("verified-source.sig.json"),
		ClaimAuthorityKeySHA256: sha256.Sum256([]byte("claim key")), ClaimAuthorityBlobSHA256: sha256.Sum256([]byte("claim blob"))}
	inventory.Toolchain = runtimeToolchain{GoVersion: "go1.25.12", GoDirective: "1.25.12", ModulePath: "github.com/aplexica/aplexica-cloud/plugin",
		GoModSHA256: sha256.Sum256([]byte("go.mod")), GoSumSHA256: sha256.Sum256([]byte("go.sum")), ModuleGraphSHA256: sha256.Sum256([]byte("graph")),
		GoModCacheSHA256: sha256.Sum256([]byte("cache")), VulnDBSHA256: sha256.Sum256([]byte("vulndb")), VulnDBManifest: artifact("vuln-db-manifest.json")}
	for _, name := range []string{"build-sandbox-profile", "git", "go", "golangci-lint", "govulncheck", "hdiutil", "plugin-release", "sandbox-exec", "source-sandbox-profile", "ssh-keygen"} {
		tool := runtimeToolDescriptor{Name: name, Version: "test", BinarySHA256: sha256.Sum256([]byte(name))}
		if name == "git" || name == "go" {
			tool.TreeSHA256 = sha256.Sum256([]byte(name + " tree"))
		}
		inventory.Toolchain.Tools = append(inventory.Toolchain.Tools, tool)
	}
	inventory.PolicyFile = runtimeInventoryArtifact{Name: "release-policy.json", SHA256: inventory.Source.PolicySHA256, Size: 1}
	inventory.RecipeFile = artifact("build-recipe.json")
	inventory.BuildRecord = artifact("release.build.cbor")
	inventory.Policy = runtimeBuildPolicy{TrimPath: true, GOAMD64: "v1", GOARM64: "v8.0", ReproducibilityRuns: 2, CommandTimeoutSecs: 1800}
	for _, name := range []string{"test", "lint", "reproducible_build", "vulnerability_scan"} {
		inventory.Gates = append(inventory.Gates, runtimeGate{Name: name, SubjectSHA256: sha256.Sum256([]byte("subject")), PolicySHA256: inventory.PolicyFile.SHA256,
			Summary: artifact("gate-" + name + ".json"), Evidence: artifact("gate-" + name + ".evidence.json")})
	}
	enc, err := cbor.CanonicalEncOptions().EncMode()
	require.NoError(t, err)
	inventoryRaw, err := enc.Marshal(inventory)
	require.NoError(t, err)
	preimage, err := enc.Marshal([]any{"aplexica/plugin-release-inventory/v1", cbor.RawMessage(inventoryRaw)})
	require.NoError(t, err)
	signed := signedRuntimeInventory{Inventory: inventoryRaw}
	copy(signed.Signature[:], ed25519.Sign(private, preimage))
	raw, err := enc.Marshal(signed)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(binaryPath), "release.inventory.cbor"), raw, 0o600))
	return raw
}

func TestVerifyRemotePluginV2RequiresAndBindsFullSignedInventory(t *testing.T) {
	binary := []byte("v2 provider-neutral executable")
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	manifest := validRemotePluginManifest(binary)
	manifest.Version, manifest.Sequence, manifest.RollbackFloor = 2, 1, 1
	binaryPath := writeSignedRemotePluginFixture(t, trustedInputTestDir(t), binary, manifest, private)
	manifestBytes, err := os.ReadFile(binaryPath + RemotePluginManifestSuffix)
	require.NoError(t, err)

	_, err = VerifyRemotePluginDetailed(binaryPath, []ed25519.PublicKey{public})
	require.ErrorContains(t, err, "signed release inventory")
	inventoryRaw := writeSignedRuntimeInventoryFixture(t, binaryPath, binary, manifestBytes, manifest, private)
	verified, err := VerifyRemotePluginDetailed(binaryPath, []ed25519.PublicKey{public})
	require.NoError(t, err)
	require.Equal(t, uint64(1), verified.Manifest.Sequence)
	require.Equal(t, sha256.Sum256(manifestBytes), verified.ManifestSHA256)
	require.Equal(t, sha256.Sum256(inventoryRaw), verified.InventorySHA256)

	tampered := append([]byte(nil), inventoryRaw...)
	tampered[len(tampered)-1] ^= 1
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(binaryPath), "release.inventory.cbor"), tampered, 0o600))
	_, err = VerifyRemotePluginDetailed(binaryPath, []ed25519.PublicKey{public})
	require.Error(t, err)
}

func TestVerifyRemotePluginV2RequiresExactSiblingAndRejectsAliases(t *testing.T) {
	binary := []byte("v2 executable")
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	manifest := validRemotePluginManifest(binary)
	manifest.Version, manifest.Sequence, manifest.RollbackFloor = 2, 1, 1
	dir := trustedInputTestDir(t)
	binaryPath := writeSignedRemotePluginFixture(t, dir, binary, manifest, private)
	manifestBytes, err := os.ReadFile(binaryPath + RemotePluginManifestSuffix)
	require.NoError(t, err)
	inventoryRaw := writeSignedRuntimeInventoryFixture(t, binaryPath, binary, manifestBytes, manifest, private)

	// Any executable-specific alias expands the accepted runtime closure and is
	// rejected, whether it is identical or conflicting.
	require.NoError(t, os.WriteFile(binaryPath+remotePluginInventoryAliasSuffix, inventoryRaw, 0o600))
	_, err = VerifyRemotePluginDetailed(binaryPath, []ed25519.PublicKey{public})
	require.ErrorContains(t, err, "inventory alias")
	require.NoError(t, os.WriteFile(binaryPath+remotePluginInventoryAliasSuffix, []byte("conflicting inventory"), 0o600))
	_, err = VerifyRemotePluginDetailed(binaryPath, []ed25519.PublicKey{public})
	require.ErrorContains(t, err, "inventory alias")
	require.NoError(t, os.Remove(binaryPath+remotePluginInventoryAliasSuffix))
	_, err = VerifyRemotePluginDetailed(binaryPath, []ed25519.PublicKey{public})
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, "release.inventory.cbor")))
	require.NoError(t, os.WriteFile(binaryPath+remotePluginInventoryAliasSuffix, inventoryRaw, 0o600))
	_, err = VerifyRemotePluginDetailed(binaryPath, []ed25519.PublicKey{public})
	require.ErrorContains(t, err, "inventory alias")
	require.NoError(t, os.Remove(binaryPath+remotePluginInventoryAliasSuffix))
	_, err = VerifyRemotePluginDetailed(binaryPath, []ed25519.PublicKey{public})
	require.ErrorContains(t, err, "signed release inventory")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "release.inventory.cbor"), inventoryRaw, 0o600))

	// A separately signed inventory with a different sequence cannot be used
	// as transport-selected authorization for this manifest.
	other := manifest
	other.Sequence = 2
	other.Previous = &RemotePluginPrevious{Sequence: 1, PluginVersion: manifest.PluginVersion, InventorySHA256: sha256.Sum256(inventoryRaw)}
	otherPath := writeSignedRemotePluginFixture(t, trustedInputTestDir(t), binary, other, private)
	otherManifest, err := os.ReadFile(otherPath + RemotePluginManifestSuffix)
	require.NoError(t, err)
	otherInventory := writeSignedRuntimeInventoryFixture(t, otherPath, binary, otherManifest, other, private)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "release.inventory.cbor"), otherInventory, 0o600))
	_, err = VerifyRemotePluginDetailed(binaryPath, []ed25519.PublicKey{public})
	require.ErrorContains(t, err, "identities differ")
}

func TestRuntimeInventoryRejectsUnknownDuplicateAndNoncanonicalMapsAtEveryLevel(t *testing.T) {
	binary := []byte("strict full inventory executable")
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	manifest := validRemotePluginManifest(binary)
	manifest.Version, manifest.PluginVersion, manifest.Sequence, manifest.RollbackFloor = 2, "v1.2.3", 2, 1
	manifest.Previous = &RemotePluginPrevious{Sequence: 1, PluginVersion: "v1.2.2", InventorySHA256: sha256.Sum256([]byte("previous inventory"))}
	dir := trustedInputTestDir(t)
	binaryPath := writeSignedRemotePluginFixture(t, dir, binary, manifest, private)
	manifestBytes, err := os.ReadFile(binaryPath + RemotePluginManifestSuffix)
	require.NoError(t, err)
	validEnvelope := writeSignedRuntimeInventoryFixture(t, binaryPath, binary, manifestBytes, manifest, private)
	levels := []string{
		"envelope", "inventory", "previous", "source", "verified-source", "toolchain", "tool",
		"build-policy", "gate", "target", "artifact",
	}
	mutations := []struct {
		name string
		fn   func(*testing.T, []byte, ed25519.PrivateKey, string) []byte
	}{
		{name: "unknown", fn: inventoryUnknownField},
		{name: "duplicate", fn: inventoryDuplicateField},
		{name: "noncanonical", fn: inventoryNoncanonicalMap},
	}
	for _, mutation := range mutations {
		for _, level := range levels {
			t.Run(mutation.name+"-"+level, func(t *testing.T) {
				raw := mutation.fn(t, validEnvelope, private, level)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "release.inventory.cbor"), raw, 0o600))
				_, verifyErr := VerifyRemotePluginDetailed(binaryPath, []ed25519.PublicKey{public})
				require.Error(t, verifyErr, "%s map with %s encoding was accepted", level, mutation.name)
			})
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "release.inventory.cbor"), validEnvelope, 0o600))
	_, err = VerifyRemotePluginDetailed(binaryPath, []ed25519.PublicKey{public})
	require.NoError(t, err)
}

func TestRuntimeInventoryRejectsSignedStructurallyInvalidSourceAndAuthorityClaims(t *testing.T) {
	binary := []byte("structural inventory executable")
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	manifest := validRemotePluginManifest(binary)
	manifest.Version, manifest.Sequence, manifest.RollbackFloor = 2, 1, 1
	dir := trustedInputTestDir(t)
	binaryPath := writeSignedRemotePluginFixture(t, dir, binary, manifest, private)
	manifestBytes, err := os.ReadFile(binaryPath + RemotePluginManifestSuffix)
	require.NoError(t, err)
	validEnvelope := writeSignedRuntimeInventoryFixture(t, binaryPath, binary, manifestBytes, manifest, private)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "wrong logical repository", mutate: func(inventory map[string]any) {
			inventory["source"].(map[string]any)["repository"] = "attacker/aplexica-cloud"
		}},
		{name: "noncanonical uppercase object id", mutate: func(inventory map[string]any) {
			inventory["source"].(map[string]any)["tagObject"] = strings.Repeat("A", 40)
		}},
		{name: "mixed object id lengths", mutate: func(inventory map[string]any) {
			inventory["source"].(map[string]any)["tree"] = strings.Repeat("d", 64)
		}},
		{name: "build date not derived from source epoch", mutate: func(inventory map[string]any) {
			inventory["source"].(map[string]any)["buildDate"] = "2023-11-14T22:13:21Z"
		}},
		{name: "claim and tag authority collapse", mutate: func(inventory map[string]any) {
			source := inventory["source"].(map[string]any)
			inventory["verifiedSource"].(map[string]any)["claimAuthorityBlobSha256"] = source["tagSignerSha256"]
		}},
		{name: "tag and publisher authority collapse", mutate: func(inventory map[string]any) {
			publisher := inventory["publisherKeySha256"].([]byte)
			inventory["source"].(map[string]any)["tagSignerSha256"] = append([]byte(nil), publisher...)
		}},
		{name: "claim and publisher authority collapse", mutate: func(inventory map[string]any) {
			publisher := inventory["publisherKeySha256"].([]byte)
			inventory["verifiedSource"].(map[string]any)["claimAuthorityBlobSha256"] = append([]byte(nil), publisher...)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inventory := inventoryMap(t, validEnvelope)
			test.mutate(inventory)
			raw := signInventoryMap(t, inventory, private)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "release.inventory.cbor"), raw, 0o600))
			_, verifyErr := VerifyRemotePluginDetailed(binaryPath, []ed25519.PublicKey{public})
			require.Error(t, verifyErr)
		})
	}
}

func inventoryUnknownField(t *testing.T, envelope []byte, private ed25519.PrivateKey, level string) []byte {
	t.Helper()
	if level == "envelope" {
		value := decodeCBORMap(t, envelope)
		value["unknown"] = uint64(1)
		return encodeCBOR(t, value)
	}
	inventory := inventoryMap(t, envelope)
	mutateNestedInventoryMap(t, inventory, level, func(value map[string]any) cbor.RawMessage {
		value["unknown"] = uint64(1)
		return encodeCBOR(t, value)
	})
	return signInventoryMap(t, inventory, private)
}

func inventoryDuplicateField(t *testing.T, envelope []byte, private ed25519.PrivateKey, level string) []byte {
	t.Helper()
	if level == "envelope" {
		signed := decodeSignedInventory(t, envelope)
		return appendDuplicateMapEntry(t, envelope, "signature", signed.Signature[:])
	}
	inventory := inventoryMap(t, envelope)
	keys := map[string]string{
		"inventory": "pluginId", "previous": "sequence", "source": "repository", "verified-source": "claim",
		"toolchain": "goVersion", "tool": "name", "build-policy": "trimPath", "gate": "name", "target": "goos", "artifact": "name",
	}
	mutateNestedInventoryMap(t, inventory, level, func(value map[string]any) cbor.RawMessage {
		key := keys[level]
		return appendDuplicateMapEntry(t, encodeCBOR(t, value), key, value[key])
	})
	return signInventoryMap(t, inventory, private)
}

func inventoryNoncanonicalMap(t *testing.T, envelope []byte, private ed25519.PrivateKey, level string) []byte {
	t.Helper()
	if level == "envelope" {
		return noncanonicalMapHeader(t, envelope)
	}
	inventory := inventoryMap(t, envelope)
	mutateNestedInventoryMap(t, inventory, level, func(value map[string]any) cbor.RawMessage {
		return noncanonicalMapHeader(t, encodeCBOR(t, value))
	})
	return signInventoryMap(t, inventory, private)
}

func mutateNestedInventoryMap(t *testing.T, inventory map[string]any, level string, mutate func(map[string]any) cbor.RawMessage) {
	t.Helper()
	if level == "inventory" {
		raw := mutate(inventory)
		// Preserve deliberately noncanonical/duplicate raw bytes at the top level
		// by carrying them through a sentinel consumed by signInventoryMap.
		inventory["__raw_inventory_test"] = raw
		return
	}
	if level == "previous" {
		inventory["previous"] = mutate(inventory["previous"].(map[string]any))
		return
	}
	if level == "source" {
		inventory["source"] = mutate(inventory["source"].(map[string]any))
		return
	}
	if level == "verified-source" {
		inventory["verifiedSource"] = mutate(inventory["verifiedSource"].(map[string]any))
		return
	}
	if level == "toolchain" {
		inventory["toolchain"] = mutate(inventory["toolchain"].(map[string]any))
		return
	}
	if level == "tool" {
		toolchain := inventory["toolchain"].(map[string]any)
		tools := toolchain["tools"].([]any)
		tools[0] = mutate(tools[0].(map[string]any))
		toolchain["tools"] = tools
		inventory["toolchain"] = cbor.RawMessage(encodeCBOR(t, toolchain))
		return
	}
	if level == "build-policy" {
		inventory["policy"] = mutate(inventory["policy"].(map[string]any))
		return
	}
	if level == "gate" {
		gates := inventory["gates"].([]any)
		gates[0] = mutate(gates[0].(map[string]any))
		inventory["gates"] = gates
		return
	}
	targets := inventory["targets"].([]any)
	target := targets[0].(map[string]any)
	if level == "target" {
		targets[0] = mutate(target)
	} else {
		target["binary"] = mutate(target["binary"].(map[string]any))
		targets[0] = cbor.RawMessage(encodeCBOR(t, target))
	}
	inventory["targets"] = targets
}

func signInventoryMap(t *testing.T, inventory map[string]any, private ed25519.PrivateKey) []byte {
	t.Helper()
	var inventoryRaw []byte
	if raw, ok := inventory["__raw_inventory_test"].(cbor.RawMessage); ok {
		delete(inventory, "__raw_inventory_test")
		inventoryRaw = raw
	} else {
		inventoryRaw = encodeCBOR(t, inventory)
	}
	enc, err := cbor.CanonicalEncOptions().EncMode()
	require.NoError(t, err)
	preimage, err := enc.Marshal([]any{"aplexica/plugin-release-inventory/v1", cbor.RawMessage(inventoryRaw)})
	require.NoError(t, err)
	signed := signedRuntimeInventory{Inventory: inventoryRaw}
	copy(signed.Signature[:], ed25519.Sign(private, preimage))
	raw, err := enc.Marshal(signed)
	require.NoError(t, err)
	return raw
}

func inventoryMap(t *testing.T, envelope []byte) map[string]any {
	t.Helper()
	return decodeCBORMap(t, decodeSignedInventory(t, envelope).Inventory)
}

func decodeSignedInventory(t *testing.T, raw []byte) signedRuntimeInventory {
	t.Helper()
	dec, err := cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden}.DecMode()
	require.NoError(t, err)
	var signed signedRuntimeInventory
	require.NoError(t, dec.Unmarshal(raw, &signed))
	return signed
}

func decodeCBORMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	dec, err := cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden,
		DefaultMapType: reflect.TypeOf(map[string]any{})}.DecMode()
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, dec.Unmarshal(raw, &value))
	return value
}

func encodeCBOR(t *testing.T, value any) cbor.RawMessage {
	t.Helper()
	enc, err := cbor.CanonicalEncOptions().EncMode()
	require.NoError(t, err)
	raw, err := enc.Marshal(value)
	require.NoError(t, err)
	return raw
}

func appendDuplicateMapEntry(t *testing.T, raw []byte, key string, value any) cbor.RawMessage {
	t.Helper()
	require.NotEmpty(t, raw)
	require.GreaterOrEqual(t, raw[0], byte(0xa0))
	require.Less(t, raw[0], byte(0xb7), "fixture map must use a one-byte definite-length header")
	result := append([]byte(nil), raw...)
	result[0]++
	result = append(result, encodeCBOR(t, key)...)
	result = append(result, encodeCBOR(t, value)...)
	return result
}

func noncanonicalMapHeader(t *testing.T, raw []byte) cbor.RawMessage {
	t.Helper()
	require.NotEmpty(t, raw)
	require.GreaterOrEqual(t, raw[0], byte(0xa0))
	require.LessOrEqual(t, raw[0], byte(0xb7))
	length := raw[0] - 0xa0
	return append([]byte{0xb8, length}, raw[1:]...)
}

func TestVerifyRemotePluginBindsSignedManifestToBinary(t *testing.T) {
	binary := []byte("test executable")
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	binaryPath := writeSignedRemotePluginFixture(t, trustedInputTestDir(t), binary, validRemotePluginManifest(binary), private)

	verified, err := VerifyRemotePlugin(binaryPath, []ed25519.PublicKey{public})
	require.NoError(t, err)
	require.True(t, verified.HasCapability(CapabilityPairStdinV1))
	require.NoError(t, os.WriteFile(binaryPath, []byte("substituted"), 0o700))
	_, err = VerifyRemotePlugin(binaryPath, []ed25519.PublicKey{public})
	require.ErrorContains(t, err, "digest mismatch")
}

func TestVerifyRemotePluginAcceptsPublisherKeyOverlap(t *testing.T) {
	oldPublic, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	newPublic, newPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	binary := []byte("rotation-overlap executable")
	binaryPath := writeSignedRemotePluginFixture(t, trustedInputTestDir(t), binary, validRemotePluginManifest(binary), newPrivate)

	verified, err := VerifyRemotePluginDetailed(binaryPath, []ed25519.PublicKey{oldPublic, newPublic})
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(newPublic), verified.PublisherKeySHA256)
}

func TestVerifyRemotePluginRejectsUntrustedPublisherKey(t *testing.T) {
	_, signerPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	untrustedPublic, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	binary := []byte("untrusted executable")
	binaryPath := writeSignedRemotePluginFixture(t, trustedInputTestDir(t), binary, validRemotePluginManifest(binary), signerPrivate)

	_, err = VerifyRemotePlugin(binaryPath, []ed25519.PublicKey{untrustedPublic})
	require.ErrorContains(t, err, "signature invalid")
}

func TestVerifyRemotePluginRejectsMissingAndMalformedManifest(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	t.Run("missing", func(t *testing.T) {
		binaryPath := filepath.Join(trustedInputTestDir(t), "aplexica-cloud-plugin")
		require.NoError(t, os.WriteFile(binaryPath, []byte("executable"), 0o700))
		resolved, resolveErr := filepath.EvalSymlinks(binaryPath)
		require.NoError(t, resolveErr)
		binaryPath = resolved
		_, err := VerifyRemotePlugin(binaryPath, []ed25519.PublicKey{public})
		require.ErrorContains(t, err, "open signed remote manifest")
	})

	t.Run("malformed", func(t *testing.T) {
		binaryPath := filepath.Join(trustedInputTestDir(t), "aplexica-cloud-plugin")
		require.NoError(t, os.WriteFile(binaryPath, []byte("executable"), 0o700))
		resolved, resolveErr := filepath.EvalSymlinks(binaryPath)
		require.NoError(t, resolveErr)
		binaryPath = resolved
		require.NoError(t, os.WriteFile(binaryPath+RemotePluginManifestSuffix, []byte("not canonical CBOR"), 0o600))
		_, err := VerifyRemotePlugin(binaryPath, []ed25519.PublicKey{public})
		require.ErrorContains(t, err, "decode signed remote manifest")
	})
}

func TestVerifyRemotePluginRejectsInvalidCapabilityLists(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	binary := []byte("capability executable")
	tests := []struct {
		name         string
		capabilities []string
	}{
		{name: "duplicate", capabilities: []string{CapabilityInboundAckV2, CapabilityInboundAckV2}},
		{name: "unsorted", capabilities: []string{CapabilityTrustProtocolV1, CapabilityInboundAckV2}},
		{name: "unknown", capabilities: []string{"future_untrusted_capability"}},
		{name: "observation capability must be exact", capabilities: []string{CapabilityDurableSyncObservationV1 + "_extra"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := validRemotePluginManifest(binary)
			manifest.Capabilities = tt.capabilities
			binaryPath := writeSignedRemotePluginFixture(t, trustedInputTestDir(t), binary, manifest, private)
			_, err := VerifyRemotePlugin(binaryPath, []ed25519.PublicKey{public})
			require.ErrorContains(t, err, "capability list")
		})
	}
}

func TestVerifyRemotePluginAcceptsSignedDurableDeltaCapability(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	binary := []byte("durable delta capable executable")
	manifest := validRemotePluginManifest(binary)
	manifest.Capabilities = []string{
		CapabilityDurableDeltaSyncV1,
		CapabilityDurableMultiStreamV1,
		CapabilityDurableSyncObservationV1,
		CapabilityInboundAckV2,
		CapabilityInboundFinalizeV1,
		CapabilityPairStdinV1,
		CapabilityTrustProtocolV1,
	}
	binaryPath := writeSignedRemotePluginFixture(t, trustedInputTestDir(t), binary, manifest, private)
	verified, err := VerifyRemotePlugin(binaryPath, []ed25519.PublicKey{public})
	require.NoError(t, err)
	require.True(t, verified.HasCapability(CapabilityDurableDeltaSyncV1))
	require.True(t, verified.HasCapability(CapabilityDurableMultiStreamV1))
	require.True(t, verified.HasCapability(CapabilityDurableSyncObservationV1))
	require.True(t, verified.HasCapability(CapabilityInboundFinalizeV1))
}
