package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/plugin/truststate"
	"github.com/fxamacker/cbor/v2"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func validRemotePluginManifestForCLI() proto.RemotePluginManifestUnsignedV1 {
	return proto.RemotePluginManifestUnsignedV1{
		Version:       1,
		PluginID:      "aplexica-cloud",
		PluginVersion: "v1.2.3",
		BinarySHA256:  sha256.Sum256([]byte("verified test binary")),
		Capabilities:  []string{proto.CapabilityInboundAckV2, proto.CapabilityPairStdinV1, proto.CapabilityTrustProtocolV1},
		ProtocolMin:   1,
		ProtocolMax:   1,
	}
}

func validV2RemotePluginVerificationForCLI() proto.VerifiedRemotePlugin {
	manifest := validRemotePluginManifestForCLI()
	manifest.Version, manifest.Sequence, manifest.RollbackFloor = 2, 1, 1
	return proto.VerifiedRemotePlugin{Manifest: manifest, PublisherKeySHA256: mustRemotePluginDigest("ddcaa7baac5957f32d38857a6e551a810975a2e3b3f3b71410b04ebc0174b80f"),
		ManifestSHA256: sha256.Sum256([]byte("manifest")), InventorySHA256: sha256.Sum256([]byte("inventory"))}
}

func commandWithOutput() (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	return cmd, out
}

func writeSignedRemotePluginForCLI(t *testing.T, dir string, binary []byte, manifest proto.RemotePluginManifestUnsignedV1, private ed25519.PrivateKey) string {
	t.Helper()
	pluginPath := filepath.Join(dir, "signed-test-plugin")
	require.NoError(t, os.WriteFile(pluginPath, binary, 0o700))
	pluginPath, err := filepath.EvalSymlinks(pluginPath)
	require.NoError(t, err)
	enc, err := cbor.CanonicalEncOptions().EncMode()
	require.NoError(t, err)
	preimage, err := enc.Marshal([]any{"aplexica/remote-plugin-manifest/v1", manifest})
	require.NoError(t, err)
	signed := proto.RemotePluginManifestV1{Manifest: manifest}
	copy(signed.Signature[:], ed25519.Sign(private, preimage))
	encoded, err := enc.Marshal(signed)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(pluginPath+proto.RemotePluginManifestSuffix, encoded, 0o600))
	return pluginPath
}

func TestSetRemoteEnabled_RequiresConfiguredExecutable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)

	if err := setRemoteEnabled(true); err == nil {
		t.Error("setRemoteEnabled(true) should fail when no executable is configured")
	}
}

func TestSetRemoteEnabled_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)

	// Seed config with an installed plugin path.
	cfgPath := filepath.Join(dir, "config.json")
	pre := &daemon.Config{Remote: daemon.RemoteConfig{Executable: "/usr/bin/test-plugin"}}
	if err := daemon.WriteConfig(cfgPath, pre); err != nil {
		t.Fatal(err)
	}

	if err := setRemoteEnabled(true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	cfg, _ := daemon.LoadConfig(cfgPath)
	if cfg.Remote.Enabled == nil || !*cfg.Remote.Enabled {
		t.Errorf("Enabled after enable() = %v, want true", cfg.Remote.Enabled)
	}

	if err := setRemoteEnabled(false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	cfg, _ = daemon.LoadConfig(cfgPath)
	if cfg.Remote.Enabled == nil || *cfg.Remote.Enabled {
		t.Errorf("Enabled after disable() = %v, want false", cfg.Remote.Enabled)
	}
}

func TestRemoteInstall_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)

	// Use the temp dir itself as a "executable" — must fail.
	cmd := remoteInstallCmd
	if err := cmd.RunE(cmd, []string{dir}); err == nil {
		t.Error("install <directory> should fail")
	}
}

func TestRemoteInstall_PersistsAndDefaultsToScheduled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)

	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	binary := []byte("#!/bin/sh\necho hi\n")
	manifest := validRemotePluginManifestForCLI()
	manifest.BinarySHA256 = sha256.Sum256(binary)
	plug := writeSignedRemotePluginForCLI(t, dir, binary, manifest, private)

	cmd, _ := commandWithOutput()
	verified := validV2RemotePluginVerificationForCLI()
	verified.Manifest.BinarySHA256 = sha256.Sum256(binary)
	verify := func(path string) (proto.VerifiedRemotePlugin, error) {
		return verified, nil
	}
	bootstrap := truststate.Bootstrap{Sequence: 1, RollbackFloor: 1, InventorySHA256: verified.InventorySHA256}
	if err := runRemoteInstallWithBootstrapAndLayout(cmd, plug, verify, bootstrap, acceptRemotePluginInstallLayoutForTest); err != nil {
		t.Fatalf("install: %v", err)
	}
	cfg, _ := daemon.LoadConfig(filepath.Join(dir, "config.json"))
	if cfg.Remote.Executable != plug {
		t.Errorf("Executable = %q, want %q", cfg.Remote.Executable, plug)
	}
	if cfg.Remote.Enabled == nil || !*cfg.Remote.Enabled {
		t.Errorf("Enabled = %v, want pointer-to-true", cfg.Remote.Enabled)
	}
	if cfg.Remote.SyncMode != "scheduled" {
		t.Errorf("SyncMode = %q, want scheduled", cfg.Remote.SyncMode)
	}
}

func TestRemoteInstall_LayoutRejectionNeverMutatesCheckpointOrConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)
	plugin := filepath.Join(dir, "candidate")
	require.NoError(t, os.WriteFile(plugin, []byte("candidate"), 0o700))
	verified := validV2RemotePluginVerificationForCLI()
	verify := func(string) (proto.VerifiedRemotePlugin, error) { return verified, nil }
	reject := func(string, string, string) error { return errors.New("mutable install root") }
	cmd, _ := commandWithOutput()
	err := runRemoteInstallWithBootstrapAndLayout(cmd, plugin, verify,
		truststate.Bootstrap{Sequence: 1, RollbackFloor: 1, InventorySHA256: verified.InventorySHA256}, reject)
	require.ErrorContains(t, err, "remote plugin install layout rejected")
	require.ErrorContains(t, err, "mutable install root")
	_, statErr := os.Stat(filepath.Join(dir, "config.json"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
	_, statErr = os.Stat(filepath.Join(dir, "remote-plugin-trust"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRemoteInstall_FailedPreflightNeverMutatesConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)
	cfgPath := filepath.Join(dir, "config.json")
	enabled := false
	beforeConfig := &daemon.Config{
		LogLevel: "warn",
		Remote: daemon.RemoteConfig{
			Executable: "/existing/plugin",
			Enabled:    &enabled,
			SyncMode:   "manual",
		},
	}
	require.NoError(t, daemon.WriteConfig(cfgPath, beforeConfig))
	before, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	tests := []struct {
		name           string
		binary         []byte
		mutateManifest func(*proto.RemotePluginManifestUnsignedV1)
		want           string
	}{
		{
			name:   "signed digest mismatch",
			binary: []byte("candidate with the wrong digest"),
			mutateManifest: func(manifest *proto.RemotePluginManifestUnsignedV1) {
				manifest.BinarySHA256 = sha256.Sum256([]byte("different expected binary"))
			},
			want: "digest mismatch",
		},
		{
			name:   "signed manifest missing required capability",
			binary: []byte("candidate missing a capability"),
			mutateManifest: func(manifest *proto.RemotePluginManifestUnsignedV1) {
				manifest.BinarySHA256 = sha256.Sum256([]byte("candidate missing a capability"))
				manifest.Capabilities = []string{proto.CapabilityInboundAckV2, proto.CapabilityTrustProtocolV1}
			},
			want: proto.CapabilityPairStdinV1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := validRemotePluginManifestForCLI()
			tt.mutateManifest(&manifest)
			pluginPath := writeSignedRemotePluginForCLI(t, trustedRemotePluginTestDir(t), tt.binary, manifest, private)
			verify := func(path string) (proto.VerifiedRemotePlugin, error) {
				return proto.VerifyRemotePluginDetailed(path, []ed25519.PublicKey{public})
			}
			cmd, _ := commandWithOutput()
			err := runRemoteInstall(cmd, pluginPath, verify)
			require.ErrorContains(t, err, tt.want)
			after, readErr := os.ReadFile(cfgPath)
			require.NoError(t, readErr)
			require.Equal(t, before, after, "failed install must leave config byte-for-byte unchanged")
		})
	}
}

func TestRemoteInstall_MissingManifestDoesNotCreateState(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state-that-must-not-be-created")
	t.Setenv("APLEXICA_STATE_DIR", stateDir)
	pluginPath := filepath.Join(base, "unsigned-plugin")
	require.NoError(t, os.WriteFile(pluginPath, []byte("unsigned"), 0o700))

	cmd, _ := commandWithOutput()
	err := runRemoteInstall(cmd, pluginPath, verifyRemotePluginWithCompiledTrust)
	require.ErrorContains(t, err, "open signed remote manifest")
	_, statErr := os.Stat(stateDir)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRemoteInstall_MissingBootstrapDoesNotMutateConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)
	cfgPath := filepath.Join(dir, "config.json")
	enabled := false
	require.NoError(t, daemon.WriteConfig(cfgPath, &daemon.Config{Remote: daemon.RemoteConfig{Executable: "/existing", Enabled: &enabled, SyncMode: "manual"}}))
	before, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	plugin := filepath.Join(dir, "candidate")
	require.NoError(t, os.WriteFile(plugin, []byte("candidate"), 0o700))
	verified := validV2RemotePluginVerificationForCLI()
	verify := func(string) (proto.VerifiedRemotePlugin, error) { return verified, nil }
	cmd, _ := commandWithOutput()
	err = runRemoteInstallWithBootstrapAndLayout(cmd, plugin, verify, truststate.Bootstrap{}, acceptRemotePluginInstallLayoutForTest)
	require.ErrorContains(t, err, "first v2 install requires exact out-of-band")
	after, readErr := os.ReadFile(cfgPath)
	require.NoError(t, readErr)
	require.Equal(t, before, after)
}

func acceptRemotePluginInstallLayoutForTest(string, string, string) error { return nil }

func TestRemoteVerify_JSONEvidenceIsDeterministicAndReadOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)
	rawPath := filepath.Join(dir, "verified-plugin")
	publisherKeySHA256 := sha256.Sum256([]byte("test publisher public key"))
	verify := func(string) (proto.VerifiedRemotePlugin, error) {
		return proto.VerifiedRemotePlugin{Manifest: validRemotePluginManifestForCLI(), PublisherKeySHA256: publisherKeySHA256}, nil
	}

	run := func() string {
		cmd, out := commandWithOutput()
		require.NoError(t, runRemoteVerify(cmd, rawPath, true, verify))
		return out.String()
	}
	first := run()
	second := run()
	require.Equal(t, first, second)
	var evidence remotePluginVerificationEvidence
	require.NoError(t, json.Unmarshal([]byte(first), &evidence))
	require.True(t, evidence.Verified)
	require.Equal(t, rawPath, evidence.Executable)
	require.Equal(t, rawPath+proto.RemotePluginManifestSuffix, evidence.ManifestPath)
	require.Equal(t, validRemotePluginManifestForCLI().Capabilities, evidence.Capabilities)
	require.Equal(t, hex.EncodeToString(publisherKeySHA256[:]), evidence.PublisherKeySHA256)
	_, statErr := os.Stat(filepath.Join(dir, "config.json"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRemoteMode_ValidateUnknownAndRealtime(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)

	cmd := remoteModeCmd
	if err := cmd.RunE(cmd, []string{"realtime"}); err == nil {
		t.Error("realtime should be rejected at Personal tier")
	}
	if err := cmd.RunE(cmd, []string{"bogus"}); err == nil {
		t.Error("unknown mode should be rejected")
	}
}

func TestRemoteMode_Persists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)

	cmd := remoteModeCmd
	if err := cmd.RunE(cmd, []string{"manual"}); err != nil {
		t.Fatalf("mode manual: %v", err)
	}
	cfg, _ := daemon.LoadConfig(filepath.Join(dir, "config.json"))
	if cfg.Remote.SyncMode != "manual" {
		t.Errorf("SyncMode = %q, want manual", cfg.Remote.SyncMode)
	}
}

func TestRemoteUninstall_ClearsAllFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)

	// Seed
	enabled := true
	pre := &daemon.Config{Remote: daemon.RemoteConfig{
		Executable: "/p",
		Enabled:    &enabled,
		SyncMode:   "manual",
	}}
	cfgPath := filepath.Join(dir, "config.json")
	if err := daemon.WriteConfig(cfgPath, pre); err != nil {
		t.Fatal(err)
	}

	cmd := remoteUninstallCmd
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	cfg, _ := daemon.LoadConfig(cfgPath)
	if cfg.Remote.Executable != "" || cfg.Remote.Enabled != nil || cfg.Remote.SyncMode != "" {
		t.Errorf("Remote after uninstall = %+v; want zero", cfg.Remote)
	}
}
