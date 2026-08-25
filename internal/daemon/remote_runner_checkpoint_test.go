//go:build !windows

package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/plugin/truststate"
	"github.com/fxamacker/cbor/v2"
)

func TestRemoteRunnerNeverSpawnsWithoutDurableCheckpoint(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "executed")
	executable := filepath.Join(dir, "plugin")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest := proto.RemotePluginManifestUnsignedV1{Version: 1, PluginID: "aplexica-cloud", PluginVersion: "v0.1.1",
		BinarySHA256: sha256.Sum256(binary), Capabilities: []string{proto.CapabilityInboundAckV2, proto.CapabilityPairStdinV1, proto.CapabilityTrustProtocolV1},
		ProtocolMin: 1, ProtocolMax: 1}
	enc, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		t.Fatal(err)
	}
	preimage, err := enc.Marshal([]any{"aplexica/remote-plugin-manifest/v1", manifest})
	if err != nil {
		t.Fatal(err)
	}
	signed := proto.RemotePluginManifestV1{Manifest: manifest}
	copy(signed.Signature[:], ed25519.Sign(private, preimage))
	raw, err := enc.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable+proto.RemotePluginManifestSuffix, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &RemoteRunner{Executable: executable, PublisherKeys: []ed25519.PublicKey{public},
		TrustStore: truststate.Store{Root: filepath.Join(dir, "missing-trust")}}
	if err := runner.runOnce(context.Background()); err == nil {
		t.Fatal("runner accepted plugin without checkpoint")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("plugin executed before checkpoint verification: %v", err)
	}
}

func TestRemoteRunnerUsesConfiguredPluginVerifierBeforeCheckpoint(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(dir, "plugin")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	verified := proto.VerifiedRemotePlugin{Manifest: proto.RemotePluginManifestUnsignedV1{
		Version: 2, PluginID: "aplexica-cloud", PluginVersion: "v0.1.2", Sequence: 1, RollbackFloor: 1,
		BinarySHA256: sha256.Sum256([]byte("#!/bin/sh\nexit 1\n")),
		Capabilities: []string{proto.CapabilityInboundAckV2, proto.CapabilityPairStdinV1, proto.CapabilityTrustProtocolV1},
		ProtocolMin:  1, ProtocolMax: 1,
	}}
	verified.ManifestSHA256 = sha256.Sum256([]byte("balanced-manifest"))
	verified.InventorySHA256 = sha256.Sum256([]byte("balanced-inventory"))
	verified.PublisherKeySHA256 = sha256.Sum256([]byte("balanced-publisher"))

	verifierCalls := 0
	runner := &RemoteRunner{
		Executable: executable,
		PluginVerifier: func(path string) (proto.VerifiedRemotePlugin, error) {
			verifierCalls++
			if path != executable {
				t.Fatalf("verifier path = %q, want %q", path, executable)
			}
			return verified, nil
		},
		TrustStore:  truststate.Store{Root: filepath.Join(dir, "missing-trust")},
		TrustPolicy: truststate.Policy{V2Publishers: [][32]byte{verified.PublisherKeySHA256}},
	}
	err = runner.runOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rollback checkpoint") {
		t.Fatalf("runOnce error = %v, want rollback checkpoint failure", err)
	}
	if verifierCalls != 1 {
		t.Fatalf("configured verifier calls = %d, want 1", verifierCalls)
	}
}
