package syncd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// FR-03.5 / BRD-03 §4.3: the daemon must not drain platform watcher Source
// errors (inotify budget / ENOSPC polling fallback / Windows RDC overflow)
// into the void. NewOrchestrator must wire an OnError sink on its watcher that
// surfaces the error through the status channel (AdapterLastErrors).
func TestNewOrchestrator_SurfacesWatcherSourceErrors(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)

	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	require.NotNil(t, orch.watcher.OnError, "primary watcher must have an error sink")
	orch.watcher.OnError(errors.New("inotify watch budget near limit"))

	errs := orch.AdapterLastErrors()
	require.Contains(t, errs, "watcher", "watcher Source errors must surface to status")
	require.Contains(t, errs["watcher"], "budget")
}
