package projectdiscovery

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/project"
)

// Cache is a mutex-guarded snapshot of the most recent HarvestAll result.
// The daemon populates it at startup and refreshes it on a debounced timer
// (see Run); the web pending handler reads the snapshot so the on-demand and
// background views of discovered folders agree — and so discovery works even
// when the local web UI is disabled.
//
// Approval-gate invariant: the cache only ever holds CANDIDATES surfaced into
// the pending list. Reharvest never auto-watches, auto-registers, or
// auto-imports a discovered folder; it only refreshes the agents set of
// folders the user has ALREADY registered (project.Registry.RefreshAgents,
// itself a no-op for unregistered paths).
type Cache struct {
	mu         sync.RWMutex
	discovered []DiscoveredFolder
}

// Snapshot returns a copy of the cached discovered folders. Safe to call from
// any goroutine. Returns nil before the first successful Reharvest.
func (c *Cache) Snapshot() []DiscoveredFolder {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.discovered == nil {
		return nil
	}
	out := make([]DiscoveredFolder, len(c.discovered))
	copy(out, c.discovered)
	return out
}

// Reharvest runs HarvestAll over the sources, stores the result in the cache,
// and refreshes the agents set of every discovered folder that maps to an
// ALREADY-registered project (RefreshAgents is a no-op for unregistered
// paths, preserving the approval gate). A harvest error leaves the previous
// snapshot intact and is returned to the caller.
func (c *Cache) Reharvest(sources []HarvestSource, reg *project.Registry, stateDir string, excludeRoots ...string) error {
	discovered, err := HarvestAll(sources, stateDir, excludeRoots...)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.discovered = discovered
	c.mu.Unlock()

	// Refresh the agents set of folders ALREADY registered. RefreshAgents
	// never registers a new folder, so unregistered discoveries stay as mere
	// pending candidates (approval-gate invariant).
	registered := map[string]struct{}{}
	for _, e := range reg.List() {
		resolved, rerr := filepath.EvalSymlinks(e.Path)
		if rerr != nil {
			resolved = e.Path
		}
		if abs, aerr := filepath.Abs(resolved); aerr == nil {
			registered[abs] = struct{}{}
		} else {
			registered[e.Path] = struct{}{}
		}
	}
	for _, df := range discovered {
		resolved, rerr := filepath.EvalSymlinks(df.Path)
		if rerr != nil {
			resolved = df.Path
		}
		abs, aerr := filepath.Abs(resolved)
		if aerr != nil {
			abs = df.Path
		}
		if _, ok := registered[abs]; !ok {
			continue
		}
		// Best-effort: a persist failure on one folder must not abort the rest.
		_ = reg.RefreshAgents(df.Path, df.Agents)
	}
	return nil
}

// Run drives Reharvest on a debounced cadence: once immediately, then every
// interval until ctx is cancelled. This is the spec-allowed timer path for the
// re-harvest requirement (debounced timer, NOT the core watcher event loop).
// A reharvest error is swallowed (the previous snapshot stays valid); the loop
// keeps ticking.
func (c *Cache) Run(ctx context.Context, sources []HarvestSource, reg *project.Registry, stateDir string, interval time.Duration, excludeRoots ...string) {
	_ = c.Reharvest(sources, reg, stateDir, excludeRoots...)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.Reharvest(sources, reg, stateDir, excludeRoots...)
		}
	}
}
