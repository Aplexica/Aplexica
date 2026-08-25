package claudecode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSurfaceDiscovery_CLIOnlyAndDesktopOnly(t *testing.T) {
	t.Run("cli only", func(t *testing.T) {
		home := t.TempDir()
		executable := filepath.Join(home, "bin", "claude")
		require.NoError(t, os.MkdirAll(filepath.Dir(executable), 0o755))
		require.NoError(t, os.WriteFile(executable, []byte("binary"), 0o755))
		a := &Adapter{
			HomeDir:             home,
			CLIExecutablePaths:  []string{executable},
			DesktopAppPaths:     []string{},
			DesktopSessionRoots: []string{},
		}
		discovery, err := a.Discover()
		require.NoError(t, err)
		require.True(t, discovery.Installed)
		require.True(t, a.claudeCLISurfaceInstalled())
		require.False(t, a.claudeDesktopSurfaceInstalled())
	})

	t.Run("desktop only", func(t *testing.T) {
		home := t.TempDir()
		app := filepath.Join(home, "Applications", "Claude.app")
		require.NoError(t, os.MkdirAll(app, 0o755))
		a := &Adapter{
			HomeDir:             home,
			CLIExecutablePaths:  []string{},
			DesktopAppPaths:     []string{app},
			DesktopSessionRoots: []string{},
		}
		discovery, err := a.Discover()
		require.NoError(t, err)
		require.True(t, discovery.Installed)
		require.False(t, a.claudeCLISurfaceInstalled())
		require.True(t, a.claudeDesktopSurfaceInstalled())
	})
}

func TestSurfaceDiscovery_PicksUpLateCLIOrDesktopInstall(t *testing.T) {
	for _, surface := range []string{"cli", "desktop"} {
		t.Run(surface, func(t *testing.T) {
			t.Setenv("PATH", "")
			home := t.TempDir()
			cli := filepath.Join(home, "bin", "claude")
			app := filepath.Join(home, "Applications", "Claude.app")
			a := &Adapter{
				HomeDir:             home,
				CLIExecutablePaths:  []string{cli},
				DesktopAppPaths:     []string{app},
				DesktopSessionRoots: []string{},
			}
			before, err := a.Discover()
			require.NoError(t, err)
			require.False(t, before.Installed)
			require.Empty(t, before.RuntimeToken)
			if surface == "cli" {
				require.NoError(t, os.MkdirAll(filepath.Dir(cli), 0o755))
				require.NoError(t, os.WriteFile(cli, []byte("binary"), 0o755))
			} else {
				require.NoError(t, os.MkdirAll(app, 0o755))
			}
			after, err := a.Discover()
			require.NoError(t, err)
			require.True(t, after.Installed)
			require.Len(t, after.ActiveSurfaces, 1)
			if surface == "desktop" {
				require.Equal(t, claudeDesktopRegistrationRuntimeToken, after.RuntimeToken)
			} else {
				require.Empty(t, after.RuntimeToken)
			}
		})
	}
}

func TestStableCLICandidates_DoNotDependOnDaemonPATH(t *testing.T) {
	t.Setenv("PATH", "")
	darwin := claudeStableCLIExecutableCandidates("darwin", "")
	require.Contains(t, darwin, filepath.Join(string(filepath.Separator), "opt", "homebrew", "bin", "claude"))
	require.Contains(t, darwin, filepath.Join(string(filepath.Separator), "usr", "local", "bin", "claude"))

	roaming := filepath.Join("C:\\Users\\person", "AppData", "Roaming")
	windows := claudeStableCLIExecutableCandidates("windows", roaming)
	require.Contains(t, windows, filepath.Join(roaming, "npm", "claude.cmd"))
}

func TestCandidateDiscovery_DoesNotCreateClaudeRoots(t *testing.T) {
	home := t.TempDir()
	a := &Adapter{HomeDir: home}
	candidate := a.CandidateDiscovery()
	require.Equal(t, []string{filepath.Join(home, ".claude")}, candidate.GlobalRoots)
	require.Equal(t, []string{filepath.Join(home, ".claude.json")}, candidate.WatchFiles)
	_, err := os.Stat(filepath.Join(home, ".claude"))
	require.True(t, os.IsNotExist(err))
}

func TestDiscover_WatchesDesktopCatalogReadOnly(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o755))
	catalog := filepath.Join(home, "desktop-catalog")
	require.NoError(t, os.MkdirAll(catalog, 0o755))
	a := &Adapter{
		HomeDir:             home,
		CLIExecutablePaths:  []string{},
		DesktopAppPaths:     []string{},
		DesktopSessionRoots: []string{catalog},
	}

	discovery, err := a.Discover()
	require.NoError(t, err)
	require.True(t, discovery.Installed)
	require.Contains(t, discovery.MetadataRoots, catalog)
	require.NotContains(t, discovery.GlobalRoots, catalog, "the app-owned catalog is watched, not imported as a storage root")
}

func TestClaudeWindowsDesktopCandidates_IncludeMSIXAndLegacyLayouts(t *testing.T) {
	local := filepath.Join("C:\\Users\\person", "AppData", "Local")
	candidates := claudeWindowsDesktopAppCandidates(local)
	require.Equal(t, filepath.Join(local, "Packages", claudeWindowsPackageFamily), candidates[0])
	require.Contains(t, candidates, filepath.Join(local, "AnthropicClaude", "Claude.exe"))
	require.Contains(t, candidates, filepath.Join(local, "Programs", "Claude", "Claude.exe"))
}

func TestClaudeWindowsDesktopCatalogCandidates_IncludeMSIXVirtualizedStateFirst(t *testing.T) {
	local := filepath.Join("C:\\Users\\person", "AppData", "Local")
	roaming := filepath.Join("C:\\Users\\person", "AppData", "Roaming")
	got := claudeWindowsDesktopSessionCatalogRoots(local, roaming)
	require.Equal(t, []string{
		filepath.Join(local, "Packages", claudeWindowsPackageFamily, "LocalCache", "Roaming", "Claude", "claude-code-sessions"),
		filepath.Join(roaming, "Claude", "claude-code-sessions"),
	}, got)
}
