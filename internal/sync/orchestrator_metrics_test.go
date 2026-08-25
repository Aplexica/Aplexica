package syncd

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

// capturingLatencyObserver records every sync-latency observation the
// orchestrator emits on the import -> fan-out path.
type capturingLatencyObserver struct {
	mu      sync.Mutex
	samples []float64
}

func (c *capturingLatencyObserver) ObserveSyncLatency(seconds float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, seconds)
}

func (c *capturingLatencyObserver) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.samples)
}

// A successful import + fan-out cycle MUST record one sync_latency_seconds
// observation per artifact through the wired SyncLatencyObserver, measured at
// the import -> fan-out materialization boundary (NFR-10 §5.2).
func TestOrchestrator_FanOutRecordsSyncLatency(t *testing.T) {
	tmp := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "sec")}
	require.NoError(t, ss.Init())
	cc := claudecode.New()
	cc.HomeDir = tmp
	cc.SecretsStore = ss

	watchDir := filepath.Join(tmp, "proj")
	require.NoError(t, os.MkdirAll(watchDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(watchDir, "CLAUDE.md"), []byte("memory body"), 0o644))

	obs := &capturingLatencyObserver{}
	orch, err := NewOrchestrator(Config{
		Dir:         watchDir,
		Adapters:    []adapter.Adapter{cc},
		Store:       store,
		QuietPeriod: 50 * time.Millisecond,
		GuardWindow: time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()
	orch.SetSyncLatencyObserver(obs)

	require.NoError(t, orch.InitialScan(context.Background()))

	require.GreaterOrEqual(t, obs.count(), 1,
		"a successful import + fan-out must record at least one sync_latency_seconds observation")
	obs.mu.Lock()
	defer obs.mu.Unlock()
	for _, s := range obs.samples {
		require.GreaterOrEqual(t, s, 0.0, "sync latency must be non-negative")
	}
}

// With no observer wired the import path must still work — instrumentation is
// strictly optional (OSS daemon with metrics disabled is the common case).
func TestOrchestrator_NoLatencyObserverIsNoOp(t *testing.T) {
	tmp := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "sec")}
	require.NoError(t, ss.Init())
	cc := claudecode.New()
	cc.HomeDir = tmp
	cc.SecretsStore = ss

	watchDir := filepath.Join(tmp, "proj")
	require.NoError(t, os.MkdirAll(watchDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(watchDir, "CLAUDE.md"), []byte("memory body"), 0o644))

	orch, err := NewOrchestrator(Config{
		Dir:         watchDir,
		Adapters:    []adapter.Adapter{cc},
		Store:       store,
		QuietPeriod: 50 * time.Millisecond,
		GuardWindow: time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	require.NoError(t, orch.InitialScan(context.Background()))
}
