package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/stretchr/testify/require"
)

// TestBuildAdapter_StampsDaemonCloudDeviceID pins the fix for CLI-authored
// events being unpublishable. The adapters' constructor default for DeviceID
// is os.Hostname(), and the outbound sweep publishes only heads whose
// provenance names the daemon's cloud device id — so an `aplexica import`
// used to author heads that never reached any peer. buildAdapter must adopt
// the identity the running daemon reports over its control socket.
func TestBuildAdapter_StampsDaemonCloudDeviceID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-domain-socket control server unsupported on the Windows CI runner")
	}
	// The control socket lives under $HOME/.aplexica/state, and darwin caps
	// sun_path at ~104 bytes, so the fake HOME must be short.
	home := t.TempDir()
	if runtime.GOOS == "darwin" {
		var err error
		home, err = os.MkdirTemp("/tmp", "aphome")
		require.NoError(t, err)
		t.Cleanup(func() { os.RemoveAll(home) })
	}
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".aplexica", "state")
	require.NoError(t, os.MkdirAll(stateDir, 0o700))

	const cloudID = "11111111-1111-4111-8111-111111111111"
	srv := daemon.NewControlServer(filepath.Join(stateDir, "aplexicad.sock"), &daemon.StatusInfo{
		PID:           1,
		StartedAt:     time.Now().UTC(),
		LocalDeviceID: cloudID,
	}, nil)
	require.NoError(t, srv.Start())
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	a, err := buildAdapter("claude-code", filepath.Join(home, ".aplexica", "secrets"))
	require.NoError(t, err)
	cc, ok := a.(*claudecode.Adapter)
	require.True(t, ok)
	require.Equal(t, cloudID, cc.DeviceID,
		"CLI-authored provenance must carry the daemon's cloud identity, not the hostname")
}

// TestBuildAdapter_KeepsHostnameDefaultWhenDaemonDown pins the fallback: with
// no daemon to ask, the adapter keeps its constructor default so a local-only
// (never-synced) store behaves exactly as before.
func TestBuildAdapter_KeepsHostnameDefaultWhenDaemonDown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// os.UserHomeDir reads USERPROFILE on Windows, and the Windows CI runner
	// shares its box with a real paired daemon — point it at the empty temp
	// home too so the lookup cannot find that daemon's socket.
	t.Setenv("USERPROFILE", home)

	a, err := buildAdapter("claude-code", filepath.Join(home, ".aplexica", "secrets"))
	require.NoError(t, err)
	cc, ok := a.(*claudecode.Adapter)
	require.True(t, ok)
	host, _ := os.Hostname()
	require.Equal(t, host, cc.DeviceID)
}
