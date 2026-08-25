package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func transitionCLIStateDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the daemon control transport uses a Unix-domain socket")
	}
	if runtime.GOOS == "darwin" {
		dir, err := os.MkdirTemp("/tmp", "apdtransition")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	return t.TempDir()
}

func TestRemoteTransitionSubmitForwardsExactPlanToRunningDaemon(t *testing.T) {
	state := transitionCLIStateDir(t)
	t.Setenv("APLEXICA_STATE_DIR", state)
	srv := daemon.NewControlServer(filepath.Join(state, "aplexicad.sock"), &daemon.StatusInfo{}, nil)
	want := []byte(`{"opaque":"signed transition plan"}`)
	received := make(chan []byte, 1)
	srv.SetDeviceTransitionSubmitter(func(_ context.Context, blob []byte) error {
		received <- append([]byte(nil), blob...)
		return nil
	})
	require.NoError(t, srv.Start())
	defer srv.Stop()

	planPath := filepath.Join(state, "transition.json")
	require.NoError(t, os.WriteFile(planPath, want, 0o600))
	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	require.NoError(t, remoteTransitionSubmitCmd.RunE(cmd, []string{planPath}))
	require.Equal(t, want, <-received)
	require.Equal(t, "Signed device transition plan accepted.\n", out.String())
}

func TestReadSignedDeviceTransitionPlanRejectsNonRegularEmptyAndOversized(t *testing.T) {
	dir := t.TempDir()
	_, err := readSignedDeviceTransitionPlan(dir)
	require.ErrorContains(t, err, "regular file")

	empty := filepath.Join(dir, "empty.json")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	_, err = readSignedDeviceTransitionPlan(empty)
	require.ErrorContains(t, err, "non-empty")

	large := filepath.Join(dir, "large.json")
	file, err := os.OpenFile(large, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(signedDeviceTransitionPlanMax+1))
	require.NoError(t, file.Close())
	_, err = readSignedDeviceTransitionPlan(large)
	require.ErrorContains(t, err, "no larger")
}
