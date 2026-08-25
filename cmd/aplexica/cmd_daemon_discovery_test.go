package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/adapter/openclaw"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/projectdiscovery"
	"github.com/stretchr/testify/require"
)

// fakeHarvestSource is a projectdiscovery.HarvestSource that reports a fixed
// set of dirs. It mirrors what an adapter.ProjectDirSource contributes to the
// daemon's startup harvest.
type fakeHarvestSource struct {
	name string
	dirs []adapter.ProjectPresence
}

func (f *fakeHarvestSource) Name() string { return f.name }
func (f *fakeHarvestSource) ProjectDirs() ([]adapter.ProjectPresence, error) {
	return f.dirs, nil
}

func TestInstalledAdaptersFromDropsDiscoveryNegativeAdapters(t *testing.T) {
	cc := claudecode.New()
	cx := codex.New()
	oc := openclaw.New()
	adapters := []adapter.Adapter{cc, cx, oc}
	discoveries := map[string]adapter.Discovery{
		cc.Name(): {Installed: true, GlobalRoots: []string{"/home/u/.claude"}},
		cx.Name(): {Installed: false, Detail: "/home/u/.codex present; executable not found: codex"},
		oc.Name(): {Installed: false, Detail: "/home/u/.openclaw present; executable not found: openclaw"},
	}

	got := installedAdaptersFrom(adapters, discoveries)

	require.Equal(t, []adapter.Adapter{cc}, got,
		"sync must only receive adapters positively discovered as installed")
}

func TestRuntimeAdaptersFromKeepsLateInstallCapableAdapters(t *testing.T) {
	cc := &claudecode.Adapter{HomeDir: t.TempDir(), CLIExecutablePaths: []string{}, DesktopAppPaths: []string{}, DesktopSessionRoots: []string{}}
	cx := &codex.Adapter{HomeDir: t.TempDir(), CLIExecutablePaths: []string{}, DesktopExecutablePaths: []string{}, WorktreeRoots: []string{}}
	oc := openclaw.New()
	adapters := []adapter.Adapter{cc, cx, oc}
	discoveries := map[string]adapter.Discovery{
		cc.Name(): {Installed: false},
		cx.Name(): {Installed: false},
		oc.Name(): {Installed: false},
	}

	got := runtimeAdaptersFrom(adapters, discoveries)
	require.Equal(t, []adapter.Adapter{cc, cx}, got,
		"Claude/Codex stay configured for late CLI/Desktop installation; static absent adapters do not")
}

// TestStartupHarvestPopulatesSharedCache asserts the daemon's startup harvest
// wiring: a Reharvest over the harvest sources populates the shared
// discovered cache BEFORE any GET /api/pending request, so discovery works
// even with the local web UI disabled.
func TestStartupHarvestPopulatesSharedCache(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, ".aplexica")

	unreg, err := filepath.Abs(filepath.Join(dir, "Projects", "Unregistered"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(unreg, 0o755)) // harvest drops candidates absent from disk
	src := &fakeHarvestSource{name: "codex", dirs: []adapter.ProjectPresence{
		{Path: unreg, LastActive: time.Unix(500, 0)},
	}}
	sources := []projectdiscovery.HarvestSource{src}

	reg, err := project.NewRegistry(filepath.Join(stateDir, "projects.json"))
	require.NoError(t, err)

	// Mirror the runBody startup wiring: populate the shared cache at startup.
	cache := &projectdiscovery.Cache{}
	require.Empty(t, cache.Snapshot(), "cache is empty before the startup harvest")
	require.NoError(t, cache.Reharvest(sources, reg, stateDir))

	// The cache is populated at startup, independent of any pending request.
	got := cache.Snapshot()
	require.Len(t, got, 1)
	require.Equal(t, unreg, got[0].Path)

	// Approval-gate invariant: the discovered-but-unregistered folder was NOT
	// auto-registered by the harvest.
	require.Empty(t, reg.List(), "startup harvest must never register a discovered folder")
}

// TestStartupHarvestRefreshesRegisteredAgents asserts the startup harvest
// refreshes the agents set of an already-registered folder (RefreshAgents),
// without registering any new folder.
func TestStartupHarvestRefreshesRegisteredAgents(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, ".aplexica")

	registered, err := filepath.Abs(filepath.Join(dir, "Projects", "Registered"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(registered, 0o755)) // harvest drops candidates absent from disk

	reg, err := project.NewRegistry(filepath.Join(stateDir, "projects.json"))
	require.NoError(t, err)
	require.NoError(t, reg.AddOrUpdate(project.Entry{ID: "id-reg", Path: registered, Scope: "local", Agents: []string{"claude-code"}}))

	src := &fakeHarvestSource{name: "codex", dirs: []adapter.ProjectPresence{
		{Path: registered, LastActive: time.Unix(500, 0)},
	}}
	cache := &projectdiscovery.Cache{}
	require.NoError(t, cache.Reharvest([]projectdiscovery.HarvestSource{src}, reg, stateDir))

	e, ok := reg.Get("id-reg")
	require.True(t, ok)
	require.Equal(t, []string{"claude-code", "codex"}, e.Agents)
	require.Len(t, reg.List(), 1, "RefreshAgents must not add new registry entries")
}
