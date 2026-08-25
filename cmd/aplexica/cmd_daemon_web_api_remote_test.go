package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/stretchr/testify/require"
)

type testPreparedRemoteCommand struct{ command *exec.Cmd }

func (prepared *testPreparedRemoteCommand) Cmd() *exec.Cmd { return prepared.command }
func (prepared *testPreparedRemoteCommand) Close() error   { return nil }

func testRemotePluginCommandPreparer(ctx context.Context, path string, _ [32]byte, args ...string) (preparedRemotePluginCommand, error) {
	return &testPreparedRemoteCommand{command: exec.CommandContext(ctx, path, args...)}, nil
}

// TestRemoteExecBitOK guards the Windows fix for remoteExecPath: the cloud
// plugin lands without a POSIX exec bit on Windows (scp/copy → mode 0666), and
// Pair/Verify/Unpair must NOT reject it as "not configured" there. On POSIX a
// missing exec bit is still a real signal and is rejected.
func TestRemoteExecBitOK(t *testing.T) {
	// Exec bit set → runnable on every platform.
	require.True(t, remoteExecBitOK(os.FileMode(0o755)))

	// No exec bit (as a Windows scp/copy leaves it): OK on Windows because
	// there is no exec-bit concept; rejected on POSIX.
	noExec := os.FileMode(0o666)
	if runtime.GOOS == "windows" {
		require.True(t, remoteExecBitOK(noExec), "Windows has no exec bit; must not reject")
	} else {
		require.False(t, remoteExecBitOK(noExec), "POSIX: missing exec bit is a real rejection")
	}
}

func TestPairRevalidatesCheckpointImmediatelyBeforeSpawn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	path := filepath.Join(t.TempDir(), "plugin")
	script := []byte("#!/bin/sh\ncat >/dev/null\nprintf 'paired: device_id=device account_id=account\\n'\n")
	require.NoError(t, os.WriteFile(path, script, 0o700))
	manifest := proto.RemotePluginManifestUnsignedV1{Version: 2, PluginID: "aplexica-cloud", PluginVersion: "v1.0.0",
		Sequence: 1, RollbackFloor: 1, Capabilities: []string{proto.CapabilityInboundAckV2, proto.CapabilityPairStdinV1, proto.CapabilityTrustProtocolV1}, ProtocolMin: 1, ProtocolMax: 1}
	var checks atomic.Int32
	runner := &daemon.RemoteRunner{DeviceID: "startup-device"}
	deps := &webAPIDeps{stateDir: t.TempDir(), remoteCfg: daemon.RemoteConfig{Executable: path},
		remoteRunner:                runner,
		remotePluginCommandPreparer: testRemotePluginCommandPreparer,
		remotePluginVerifier: func(string) (proto.VerifiedRemotePlugin, error) {
			return proto.VerifiedRemotePlugin{Manifest: manifest}, nil
		},
		remotePluginCheckpointVerifier: func(string, proto.VerifiedRemotePlugin) error {
			checks.Add(1)
			return nil
		},
	}
	device, account, err := (&remoteWebAccessor{deps: deps}).Pair(t.Context(), "secret-token", "test")
	require.NoError(t, err)
	require.Equal(t, "device", device)
	require.Equal(t, "account", account)
	require.Equal(t, "device", runner.CurrentDeviceID(), "pair must update the live runner before its replacement handshake")
	require.Equal(t, int32(2), checks.Load(), "pair must verify at preflight and immediately before process launch")
}

func TestPairFailsClosedWhenCheckpointChangesBeforeSpawn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin")
	marker := filepath.Join(dir, "executed")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o700))
	manifest := proto.RemotePluginManifestUnsignedV1{Version: 2, PluginID: "aplexica-cloud", PluginVersion: "v1.0.0",
		Sequence: 1, RollbackFloor: 1, Capabilities: []string{proto.CapabilityInboundAckV2, proto.CapabilityPairStdinV1, proto.CapabilityTrustProtocolV1}, ProtocolMin: 1, ProtocolMax: 1}
	var checks atomic.Int32
	deps := &webAPIDeps{stateDir: t.TempDir(), remoteCfg: daemon.RemoteConfig{Executable: path},
		remotePluginCommandPreparer: testRemotePluginCommandPreparer,
		remotePluginVerifier: func(string) (proto.VerifiedRemotePlugin, error) {
			return proto.VerifiedRemotePlugin{Manifest: manifest}, nil
		},
		remotePluginCheckpointVerifier: func(string, proto.VerifiedRemotePlugin) error {
			if checks.Add(1) == 2 {
				return errors.New("checkpoint substituted")
			}
			return nil
		},
	}
	_, _, err := (&remoteWebAccessor{deps: deps}).Pair(t.Context(), "secret-token", "test")
	require.ErrorContains(t, err, "identity changed before launch")
	_, statErr := os.Stat(marker)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestQueryPluginStatusRequiresImmediateVerifier(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin")
	marker := filepath.Join(dir, "executed")
	script := []byte("#!/bin/sh\ntouch '" + marker + "'\nprintf 'paired: yes\\ndevice_id: device\\naccount_id: account\\n'\n")
	require.NoError(t, os.WriteFile(path, script, 0o700))

	paired, _, _ := queryPluginStatus(t.Context(), path, nil)
	require.False(t, paired)
	_, err := os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)

	paired, _, _ = queryPluginStatus(t.Context(), path, func(context.Context, string, ...string) (preparedRemotePluginCommand, error) {
		return nil, errors.New("checkpoint rejected")
	})
	require.False(t, paired)
	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)

	paired, device, account := queryPluginStatus(t.Context(), path, func(ctx context.Context, candidate string, args ...string) (preparedRemotePluginCommand, error) {
		require.Equal(t, path, candidate)
		return testRemotePluginCommandPreparer(ctx, candidate, [32]byte{}, args...)
	})
	require.True(t, paired)
	require.Equal(t, "device", device)
	require.Equal(t, "account", account)
	require.FileExists(t, marker)
}

func TestQueryPairedRejectsConfiguredPathSubstitutionBeforeStatusExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	first := filepath.Join(dir, "plugin-first")
	second := filepath.Join(dir, "plugin-second")
	marker := filepath.Join(dir, "executed")
	for _, path := range []string{first, second} {
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\ntouch '"+marker+"'\nprintf 'paired: yes\\n'\n"), 0o700))
	}
	manifest := proto.RemotePluginManifestUnsignedV1{Version: 2, PluginID: "aplexica-cloud", PluginVersion: "v1.0.0",
		Sequence: 1, RollbackFloor: 1, Capabilities: []string{proto.CapabilityInboundAckV2, proto.CapabilityPairStdinV1, proto.CapabilityTrustProtocolV1}, ProtocolMin: 1, ProtocolMax: 1}
	deps := &webAPIDeps{stateDir: t.TempDir(), remoteCfg: daemon.RemoteConfig{Executable: first},
		remotePluginVerifier: func(string) (proto.VerifiedRemotePlugin, error) {
			return proto.VerifiedRemotePlugin{Manifest: manifest}, nil
		},
	}
	var checks atomic.Int32
	deps.remotePluginCheckpointVerifier = func(string, proto.VerifiedRemotePlugin) error {
		if checks.Add(1) == 1 {
			deps.remoteCfg.Executable = second
		}
		return nil
	}
	paired, _, _ := (&remoteWebAccessor{deps: deps}).queryPaired(t.Context(), first)
	require.False(t, paired)
	require.Equal(t, int32(1), checks.Load(), "configured-path mismatch must fail before a second checkpoint lookup or exec")
	_, err := os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestPairRejectsBinarySubstitutionAfterFinalVerificationBeforePrepare(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin")
	replacement := filepath.Join(dir, "replacement")
	marker := filepath.Join(dir, "unverified-executed")
	verifiedBytes := []byte("#!/bin/sh\ncat >/dev/null\nprintf 'paired: device_id=device account_id=account\\n'\n")
	unverifiedBytes := []byte("#!/bin/sh\ntouch '" + marker + "'\nexit 0\n")
	require.NoError(t, os.WriteFile(path, verifiedBytes, 0o700))
	require.NoError(t, os.WriteFile(replacement, unverifiedBytes, 0o700))
	manifest := proto.RemotePluginManifestUnsignedV1{Version: 2, PluginID: "aplexica-cloud", PluginVersion: "v1.0.0",
		Sequence: 1, RollbackFloor: 1, BinarySHA256: sha256.Sum256(verifiedBytes),
		Capabilities: []string{proto.CapabilityInboundAckV2, proto.CapabilityPairStdinV1, proto.CapabilityTrustProtocolV1}, ProtocolMin: 1, ProtocolMax: 1}
	var checks atomic.Int32
	deps := &webAPIDeps{stateDir: t.TempDir(), remoteCfg: daemon.RemoteConfig{Executable: path},
		remotePluginVerifier: func(string) (proto.VerifiedRemotePlugin, error) {
			return proto.VerifiedRemotePlugin{Manifest: manifest}, nil
		},
		remotePluginCheckpointVerifier: func(string, proto.VerifiedRemotePlugin) error {
			if checks.Add(1) == 2 {
				return os.Rename(replacement, path)
			}
			return nil
		},
	}
	_, _, err := (&remoteWebAccessor{deps: deps}).Pair(t.Context(), "secret-token", "test")
	require.ErrorContains(t, err, "identity changed before launch")
	require.Equal(t, int32(2), checks.Load())
	_, statErr := os.Stat(marker)
	require.ErrorIs(t, statErr, os.ErrNotExist, "unverified pathname replacement must never execute")
}

// TestPairPropagatesRotatedIdentityBeyondTheRunner covers the web-API
// pair flow: a (re-)pair rotates the cloud device id, and the id must reach
// every stamping component — not only the runner. Before this, the
// orchestrator and the adapters kept the boot-seeded identity, so every
// outbound event carried the RETIRED id until a daemon restart.
func TestPairPropagatesRotatedIdentityBeyondTheRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	const rotated = "22222222-2222-4222-8222-222222222222"
	path := filepath.Join(t.TempDir(), "plugin")
	script := []byte("#!/bin/sh\ncat >/dev/null\nprintf 'paired: device_id=" + rotated + " account_id=account\\n'\n")
	require.NoError(t, os.WriteFile(path, script, 0o700))
	manifest := proto.RemotePluginManifestUnsignedV1{Version: 2, PluginID: "aplexica-cloud", PluginVersion: "v1.0.0",
		Sequence: 1, RollbackFloor: 1, Capabilities: []string{proto.CapabilityInboundAckV2, proto.CapabilityPairStdinV1, proto.CapabilityTrustProtocolV1}, ProtocolMin: 1, ProtocolMax: 1}

	runner := &daemon.RemoteRunner{DeviceID: "11111111-1111-4111-8111-111111111111"}
	orch := &syncd.Orchestrator{}
	orch.SetLocalDeviceID("11111111-1111-4111-8111-111111111111")
	cc := &claudecode.Adapter{DeviceID: "test-host.localdomain"}

	deps := &webAPIDeps{stateDir: t.TempDir(), remoteCfg: daemon.RemoteConfig{Executable: path},
		remoteRunner:                runner,
		orch:                        orch,
		adapters:                    []adapter.Adapter{cc},
		remotePluginCommandPreparer: testRemotePluginCommandPreparer,
		remotePluginVerifier: func(string) (proto.VerifiedRemotePlugin, error) {
			return proto.VerifiedRemotePlugin{Manifest: manifest}, nil
		},
		remotePluginCheckpointVerifier: func(string, proto.VerifiedRemotePlugin) error { return nil },
	}
	device, _, err := (&remoteWebAccessor{deps: deps}).Pair(t.Context(), "secret-token", "test")
	require.NoError(t, err)
	require.Equal(t, rotated, device)
	require.Equal(t, rotated, runner.CurrentDeviceID())
	require.Equal(t, rotated, orch.LocalDeviceID(),
		"the orchestrator must stamp the rotated id on the next outbound event")
	require.Equal(t, rotated, cc.DeviceID,
		"adapter provenance must not keep attributing events to the retired identity")
}

// TestQueryPluginStatusCheckedFailsClosed covers the distinction the repair
// --check-peers guard depends on: an unanswerable --status query (failed
// spawn, non-zero exit, output with no paired field) is an ERROR from the
// checked variant — never a paired=false answer that would wave a fleet
// flag-day through — while a genuine unpaired answer stays error-free.
func TestQueryPluginStatusCheckedFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	pass := func(ctx context.Context, candidate string, args ...string) (preparedRemotePluginCommand, error) {
		return testRemotePluginCommandPreparer(ctx, candidate, [32]byte{}, args...)
	}

	_, _, _, err := queryPluginStatusChecked(t.Context(), "", pass)
	require.ErrorContains(t, err, "no remote plugin")

	_, _, _, err = queryPluginStatusChecked(t.Context(), filepath.Join(dir, "plugin"),
		func(context.Context, string, ...string) (preparedRemotePluginCommand, error) {
			return nil, errors.New("checkpoint rejected")
		})
	require.ErrorContains(t, err, "checkpoint rejected")

	crash := filepath.Join(dir, "crash")
	require.NoError(t, os.WriteFile(crash, []byte("#!/bin/sh\nexit 3\n"), 0o700))
	_, _, _, err = queryPluginStatusChecked(t.Context(), crash, pass)
	require.ErrorContains(t, err, "plugin --status")

	silent := filepath.Join(dir, "silent")
	require.NoError(t, os.WriteFile(silent, []byte("#!/bin/sh\nprintf 'version: v1\\n'\n"), 0o700))
	_, _, _, err = queryPluginStatusChecked(t.Context(), silent, pass)
	require.ErrorContains(t, err, "no paired field")

	unpaired := filepath.Join(dir, "unpaired")
	require.NoError(t, os.WriteFile(unpaired, []byte("#!/bin/sh\nprintf 'paired: no\\n'\n"), 0o700))
	paired, _, _, err := queryPluginStatusChecked(t.Context(), unpaired, pass)
	require.NoError(t, err)
	require.False(t, paired)

	yes := filepath.Join(dir, "paired")
	require.NoError(t, os.WriteFile(yes, []byte("#!/bin/sh\nprintf 'paired: yes\\ndevice_id: device\\naccount_id: account\\n'\n"), 0o700))
	paired, device, account, err := queryPluginStatusChecked(t.Context(), yes, pass)
	require.NoError(t, err)
	require.True(t, paired)
	require.Equal(t, "device", device)
	require.Equal(t, "account", account)
}
