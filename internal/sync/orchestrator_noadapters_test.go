package syncd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// A machine with no AI agents installed must still run the daemon: since
// install discovery started gating the adapter set (executable/marker
// signals), a bare machine resolves ZERO installed adapters, and the
// orchestrator constructor used to hard-error — the daemon exited at startup
// ("syncd: at least one Adapter is required", caught by the Web UI smoke CI
// job on agentless runners). Zero adapters is a valid idle state: watch
// nothing, import nothing, serve status/UI, and materialize nothing inbound
// (materializeInbound already no-ops on an empty adapter set).
func TestNewOrchestrator_ZeroAdaptersIsValid(t *testing.T) {
	tmp := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	orch, err := NewOrchestrator(Config{
		Dir:   tmp,
		Store: store,
	})
	require.NoError(t, err, "an agentless machine must still get a running daemon")
	defer orch.Close()

	// The import pipeline must be a harmless no-op, not a panic.
	p := filepath.Join(tmp, "somefile.md")
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	require.NotPanics(t, func() { orch.handleEvent(p) })
	require.NoError(t, orch.InitialScan(context.Background()))

	arts, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Empty(t, arts, "no adapter, no imports")
}
