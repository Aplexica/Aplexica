package truststate

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

func verification(sequence, floor uint64, inventorySeed, manifestSeed string, previous *proto.RemotePluginPrevious) proto.VerifiedRemotePlugin {
	version := "v1.0.0"
	if sequence > 1 {
		version = fmt.Sprintf("v1.0.%d", sequence-1)
	}
	return proto.VerifiedRemotePlugin{
		Manifest: proto.RemotePluginManifestUnsignedV1{
			Version: 2, PluginID: "aplexica-cloud", PluginVersion: version, Sequence: sequence, RollbackFloor: floor,
			Previous: previous, BinarySHA256: sha256.Sum256([]byte("binary-" + inventorySeed)),
		},
		PublisherKeySHA256: sha256.Sum256([]byte("publisher")),
		ManifestSHA256:     sha256.Sum256([]byte(manifestSeed)),
		InventorySHA256:    sha256.Sum256([]byte(inventorySeed)),
	}
}

func bootstrapFor(v proto.VerifiedRemotePlugin) Bootstrap {
	return Bootstrap{Sequence: v.Manifest.Sequence, RollbackFloor: v.Manifest.RollbackFloor, InventorySHA256: v.InventorySHA256}
}

func v2Policy(v proto.VerifiedRemotePlugin) Policy {
	return Policy{V2Publishers: [][32]byte{v.PublisherKeySHA256}}
}

func TestV2CheckpointRejectsRollbackEquivocationSkipAndWrongPredecessor(t *testing.T) {
	store := Store{Root: filepath.Join(t.TempDir(), "trust")}
	path := filepath.Join(t.TempDir(), "plugin")
	first := verification(1, 1, "inventory-one", "manifest-one", nil)
	policy := v2Policy(first)
	legacySignerV2 := first
	legacySignerV2.PublisherKeySHA256 = sha256.Sum256([]byte("legacy publisher root"))
	legacySignerStore := Store{Root: filepath.Join(t.TempDir(), "legacy-signer-trust")}
	if _, err := legacySignerStore.Accept(path, legacySignerV2, policy, bootstrapFor(legacySignerV2)); err == nil {
		t.Fatal("v2 release under overlap-only legacy publisher succeeded")
	}
	if _, err := store.Accept(path, first, policy, Bootstrap{}); err == nil {
		t.Fatal("first v2 install without exact out-of-band identity succeeded")
	}
	badBootstrap := bootstrapFor(first)
	badBootstrap.InventorySHA256[0] ^= 1
	if _, err := store.Accept(path, first, policy, badBootstrap); err == nil {
		t.Fatal("first v2 install with wrong out-of-band digest succeeded")
	}
	badBootstrap = bootstrapFor(first)
	badBootstrap.RollbackFloor++
	if _, err := store.Accept(path, first, policy, badBootstrap); err == nil {
		t.Fatal("first v2 install with wrong out-of-band rollback floor succeeded")
	}
	if _, err := store.Accept(path, first, policy, bootstrapFor(first)); err != nil {
		t.Fatalf("bootstrap first release: %v", err)
	}
	if _, err := store.VerifyCurrent(path, first, policy); err != nil {
		t.Fatalf("restart rejected accepted release: %v", err)
	}

	equivocation := first
	equivocation.ManifestSHA256 = sha256.Sum256([]byte("different same-sequence manifest"))
	if _, err := store.Accept(path, equivocation, policy, Bootstrap{}); err == nil {
		t.Fatal("same-sequence equivocation succeeded")
	}
	if _, err := store.Accept(path, first, policy, bootstrapFor(first)); err == nil {
		t.Fatal("reusing bootstrap authorization after checkpoint succeeded")
	}

	second := verification(2, 1, "inventory-two", "manifest-two", &proto.RemotePluginPrevious{
		Sequence: 1, PluginVersion: first.Manifest.PluginVersion, InventorySHA256: first.InventorySHA256,
	})
	wrongPredecessor := second
	wrong := *second.Manifest.Previous
	wrong.InventorySHA256[0] ^= 1
	wrongPredecessor.Manifest.Previous = &wrong
	if _, err := store.Accept(path, wrongPredecessor, policy, Bootstrap{}); err == nil {
		t.Fatal("wrong predecessor succeeded")
	}
	skipped := verification(3, 1, "inventory-three", "manifest-three", &proto.RemotePluginPrevious{
		Sequence: 2, PluginVersion: second.Manifest.PluginVersion, InventorySHA256: second.InventorySHA256,
	})
	if _, err := store.Accept(path, skipped, policy, Bootstrap{}); err == nil {
		t.Fatal("skipped sequence succeeded")
	}
	if _, err := store.Accept(path, second, policy, Bootstrap{}); err != nil {
		t.Fatalf("exact successor rejected: %v", err)
	}
	if _, err := store.VerifyCurrent(path, first, policy); err == nil {
		t.Fatal("old signed release replay succeeded after upgrade")
	}
	if _, err := store.VerifyCurrent(filepath.Join(t.TempDir(), "substituted-plugin"), second, policy); err == nil {
		t.Fatal("runtime config path substitution succeeded")
	}
}

func TestLegacyOverlapIsExactExplicitAndRetirable(t *testing.T) {
	legacy := proto.VerifiedRemotePlugin{
		Manifest:           proto.RemotePluginManifestUnsignedV1{Version: 1, PluginID: "aplexica-cloud", PluginVersion: "v0.1.1", BinarySHA256: sha256.Sum256([]byte("legacy binary"))},
		PublisherKeySHA256: sha256.Sum256([]byte("publisher")), ManifestSHA256: sha256.Sum256([]byte("legacy manifest")),
	}
	identity := LegacyIdentity{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, PluginVersion: legacy.Manifest.PluginVersion,
		BinarySHA256: legacy.Manifest.BinarySHA256, ManifestSHA256: legacy.ManifestSHA256, PublisherKeySHA256: legacy.PublisherKeySHA256}
	policy := Policy{AllowLegacyV1: true, LegacyV1: []LegacyIdentity{identity}}
	store := Store{Root: filepath.Join(t.TempDir(), "trust")}
	path := filepath.Join(t.TempDir(), "plugin")
	if _, err := store.Accept(path, legacy, policy, Bootstrap{}); err == nil {
		t.Fatal("implicit legacy migration succeeded")
	}
	if _, err := store.Accept(path, legacy, policy, Bootstrap{LegacyMigration: true}); err != nil {
		t.Fatalf("exact explicit legacy migration rejected: %v", err)
	}
	if _, err := store.VerifyCurrent(path, legacy, policy); err != nil {
		t.Fatalf("overlap daemon rejected exact checkpointed legacy target: %v", err)
	}
	if _, err := store.VerifyCurrent(path, legacy, Policy{}); err == nil {
		t.Fatal("retirement daemon accepted legacy checkpoint")
	}
	changed := legacy
	changed.ManifestSHA256[0] ^= 1
	if _, err := store.VerifyCurrent(path, changed, policy); err == nil {
		t.Fatal("different publisher-valid legacy manifest accepted")
	}

	firstV2 := verification(2, 1, "inventory-v2", "manifest-v2", &proto.RemotePluginPrevious{
		Sequence: 1, PluginVersion: "v0.1.1", InventorySHA256: sha256.Sum256([]byte("signed-v0.1.1-inventory")),
	})
	policy.V2Publishers = [][32]byte{firstV2.PublisherKeySHA256}
	if _, err := store.Accept(path, firstV2, policy, bootstrapFor(firstV2)); err != nil {
		t.Fatalf("exact out-of-band v2 transition rejected: %v", err)
	}
	if _, err := store.VerifyCurrent(path, legacy, policy); err == nil {
		t.Fatal("legacy replay succeeded after v2 transition")
	}
}

func TestConcurrentSameSequenceAcceptsExactlyOneInventory(t *testing.T) {
	store := Store{Root: filepath.Join(t.TempDir(), "trust")}
	path := filepath.Join(t.TempDir(), "plugin")
	first := verification(1, 1, "first", "first-manifest", nil)
	policy := v2Policy(first)
	if _, err := store.Accept(path, first, policy, bootstrapFor(first)); err != nil {
		t.Fatal(err)
	}
	previous := &proto.RemotePluginPrevious{Sequence: 1, PluginVersion: first.Manifest.PluginVersion, InventorySHA256: first.InventorySHA256}
	candidates := []proto.VerifiedRemotePlugin{
		verification(2, 1, "second-a", "manifest-a", previous),
		verification(2, 1, "second-b", "manifest-b", previous),
	}
	var wg sync.WaitGroup
	results := make(chan error, len(candidates))
	for _, candidate := range candidates {
		candidate := candidate
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Accept(path, candidate, policy, Bootstrap{})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent equivocation successes = %d, want 1", successes)
	}
}

func TestCheckpointMissingCorruptUnsafeAndSymlinkFailClosed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "trust")
	store := Store{Root: root}
	path := filepath.Join(t.TempDir(), "plugin")
	v := verification(1, 1, "inventory", "manifest", nil)
	policy := v2Policy(v)
	if _, err := store.VerifyCurrent(path, v, policy); err == nil {
		t.Fatal("missing checkpoint accepted")
	}
	if _, err := store.Accept(path, v, policy, bootstrapFor(v)); err != nil {
		t.Fatal(err)
	}
	checkpointPath := filepath.Join(root, checkpointFilename)
	if err := os.WriteFile(checkpointPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VerifyCurrent(path, v, policy); err == nil {
		t.Fatal("corrupt checkpoint accepted")
	}
	if err := os.Remove(checkpointPath); err != nil {
		t.Fatal(err)
	}
	t.Run("symlink checkpoint", func(t *testing.T) {
		if err := os.Symlink(filepath.Join(t.TempDir(), "target"), checkpointPath); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("Windows account cannot create test symlink: %v", err)
			}
			t.Fatal(err)
		}
		if _, err := store.VerifyCurrent(path, v, policy); err == nil {
			t.Fatal("symlink checkpoint accepted")
		}
	})

	t.Run("symlink lock", func(t *testing.T) {
		caseRoot := filepath.Join(t.TempDir(), "trust")
		if err := os.MkdirAll(caseRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "must-not-be-opened")
		if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(caseRoot, lockFilename)); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("Windows account cannot create test symlink: %v", err)
			}
			t.Fatal(err)
		}
		if _, err := (Store{Root: caseRoot}).Accept(path, v, policy, bootstrapFor(v)); err == nil {
			t.Fatal("symlink lock accepted")
		}
		raw, err := os.ReadFile(target)
		if err != nil || string(raw) != "unchanged" {
			t.Fatalf("lock symlink target changed: %q %v", raw, err)
		}
	})

	for _, test := range []struct {
		name        string
		skipWindows bool
		mutate      func(string) error
	}{
		{name: "group-readable mode", skipWindows: true, mutate: func(checkpoint string) error { return os.Chmod(checkpoint, 0o640) }},
		{name: "hardlink", mutate: func(checkpoint string) error { return os.Link(checkpoint, checkpoint+".alias") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && test.skipWindows {
				t.Skip("Windows security is validated through ownership and DACL tests, not POSIX mode bits")
			}
			caseRoot := filepath.Join(t.TempDir(), "trust")
			caseStore := Store{Root: caseRoot}
			if _, err := caseStore.Accept(path, v, policy, bootstrapFor(v)); err != nil {
				t.Fatal(err)
			}
			if err := test.mutate(filepath.Join(caseRoot, checkpointFilename)); err != nil {
				t.Fatal(err)
			}
			if _, err := caseStore.VerifyCurrent(path, v, policy); err == nil {
				t.Fatal("unsafe checkpoint accepted")
			}
		})
	}
}
