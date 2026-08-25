package projectdiscovery

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/stretchr/testify/require"
)

// countingSource records how many times ProjectDirs() was queried so the
// debounce/interval loop can be asserted by call count.
type countingSource struct {
	name  string
	dirs  []adapter.ProjectPresence
	calls int64
}

func (c *countingSource) Name() string { return c.name }
func (c *countingSource) ProjectDirs() ([]adapter.ProjectPresence, error) {
	atomic.AddInt64(&c.calls, 1)
	return c.dirs, nil
}

func TestCache_ReharvestPopulatesAndRefreshesRegistered(t *testing.T) {
	// Both folders must exist on disk to clear HarvestAll's existence gate
	// (it now drops candidates that no longer exist).
	dirs := realDir(t, "Projects/Registered", "Projects/Unregistered")
	registered, unregistered := dirs[0], dirs[1]
	src := &countingSource{name: "codex", dirs: []adapter.ProjectPresence{
		{Path: registered, LastActive: time.Unix(500, 0)},
		{Path: unregistered, LastActive: time.Unix(400, 0)},
	}}

	reg := newTestRegistry(t)
	// Registered with only "claude-code"; reharvest must union "codex" in.
	require.NoError(t, reg.AddOrUpdate(project.Entry{ID: "id-reg", Path: registered, Scope: "local", Agents: []string{"claude-code"}}))

	cache := &Cache{}
	stateDir := absPath(t, "/Users/testuser/.aplexica")
	require.NoError(t, cache.Reharvest([]HarvestSource{src}, reg, stateDir))

	// Cache is populated with BOTH folders (HarvestAll semantics — registered
	// folders are NOT dropped; AgentSuggestions needs them).
	got := cache.Snapshot()
	require.Len(t, got, 2)

	// The registered folder's agents set got "codex" unioned in + persisted.
	e, ok := reg.Get("id-reg")
	require.True(t, ok)
	require.Equal(t, []string{"claude-code", "codex"}, e.Agents)

	// The unregistered folder must NOT have been registered (approval gate).
	require.Len(t, reg.List(), 1, "reharvest must never register a new folder")
}

func TestCache_RunTicksOnInterval(t *testing.T) {
	src := &countingSource{name: "codex", dirs: []adapter.ProjectPresence{
		{Path: absPath(t, "/Users/testuser/Projects/Foo"), LastActive: time.Unix(100, 0)},
	}}
	reg := newTestRegistry(t)
	cache := &Cache{}
	stateDir := absPath(t, "/Users/testuser/.aplexica")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run with a tiny interval and no initial harvest (Run does the first tick
	// immediately, then on each interval).
	done := make(chan struct{})
	go func() {
		cache.Run(ctx, []HarvestSource{src}, reg, stateDir, 10*time.Millisecond)
		close(done)
	}()

	require.Eventually(t, func() bool {
		return atomic.LoadInt64(&src.calls) >= 3
	}, 2*time.Second, 5*time.Millisecond, "expected the loop to re-harvest on its interval")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
