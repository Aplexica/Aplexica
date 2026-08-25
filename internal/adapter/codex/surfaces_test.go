package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSurfaceDiscovery_CLIOnlyAndDesktopOnly(t *testing.T) {
	t.Run("cli only does not register Desktop inventory", func(t *testing.T) {
		home := t.TempDir()
		cli := filepath.Join(home, "bin", codexExecutableName())
		writeExecutable(t, cli)
		a := New()
		a.HomeDir = home
		a.CLIExecutablePaths = []string{cli}
		a.DesktopExecutablePaths = []string{}
		registrations := 0
		a.registerAppServerThread = func(context.Context, string, string, string, string, string) error {
			registrations++
			return nil
		}

		discovery, err := a.Discover()
		require.NoError(t, err)
		require.True(t, discovery.Installed)
		require.True(t, a.codexCLISurfaceInstalled())
		require.False(t, a.codexDesktopSurfaceInstalled())
		a.bestEffortRegisterAppThread("thread", "", "", false)
		require.Zero(t, registrations, "CLI availability must not be treated as a Desktop inventory")
		a.bestEffortRegisterAppThread("branch-thread", "", "[test2] Branch", true)
		require.Equal(t, 1, registrations, "a named branch may use Codex CLI's supported app-server naming path")
	})

	t.Run("desktop only", func(t *testing.T) {
		home := t.TempDir()
		desktopHost := filepath.Join(home, "Codex.app", "codex")
		writeExecutable(t, desktopHost)
		a := New()
		a.HomeDir = home
		a.CLIExecutablePaths = []string{}
		a.DesktopExecutablePaths = []string{desktopHost}
		var registeredExecutable string
		a.registerAppServerThread = func(_ context.Context, executable, _, _, _, _ string) error {
			registeredExecutable = executable
			return nil
		}

		discovery, err := a.Discover()
		require.NoError(t, err)
		require.True(t, discovery.Installed)
		require.False(t, a.codexCLISurfaceInstalled())
		require.True(t, a.codexDesktopSurfaceInstalled())
		a.bestEffortRegisterAppThread("thread", "", "", false)
		require.Equal(t, desktopHost, registeredExecutable)
	})
}

func TestSurfaceDiscovery_PicksUpLateCLIOrDesktopInstall(t *testing.T) {
	for _, surface := range []string{"cli", "desktop"} {
		t.Run(surface, func(t *testing.T) {
			t.Setenv("PATH", "")
			home := t.TempDir()
			cli := filepath.Join(home, "bin", codexExecutableName())
			desktopHost := filepath.Join(home, "Codex.app", "codex")
			a := New()
			a.HomeDir = home
			a.CLIExecutablePaths = []string{cli}
			a.DesktopExecutablePaths = []string{desktopHost}
			registrations := 0
			a.registerAppServerThread = func(context.Context, string, string, string, string, string) error {
				registrations++
				return nil
			}

			before, err := a.Discover()
			require.NoError(t, err)
			require.False(t, before.Installed)
			a.bestEffortRegisterAppThread("before-install", "", "", false)
			require.Zero(t, registrations)
			if surface == "cli" {
				writeExecutable(t, cli)
			} else {
				writeExecutable(t, desktopHost)
			}
			after, err := a.Discover()
			require.NoError(t, err)
			require.True(t, after.Installed)
			require.Len(t, after.ActiveSurfaces, 1)
			a.bestEffortRegisterAppThread("after-install", "", "", false)
			if surface == "desktop" {
				require.Equal(t, 1, registrations)
			} else {
				require.Zero(t, registrations, "late CLI install must not be mistaken for Desktop")
			}
		})
	}
}

func TestStableCLICandidates_DoNotDependOnDaemonPATH(t *testing.T) {
	t.Setenv("PATH", "")
	darwin := codexStableCLIExecutableCandidates("darwin", "")
	require.Contains(t, darwin, filepath.Join(string(filepath.Separator), "opt", "homebrew", "bin", "codex"))
	require.Contains(t, darwin, filepath.Join(string(filepath.Separator), "usr", "local", "bin", "codex"))

	roaming := filepath.Join("C:\\Users\\person", "AppData", "Roaming")
	windows := codexStableCLIExecutableCandidates("windows", roaming)
	require.Contains(t, windows, filepath.Join(roaming, "npm", "codex.cmd"))
}

func TestCandidateDiscovery_DoesNotCreateCodexRoots(t *testing.T) {
	home := t.TempDir()
	a := &Adapter{HomeDir: home}
	candidate := a.CandidateDiscovery()
	require.Equal(t, []string{
		filepath.Join(home, ".codex"),
		filepath.Join(home, ".codex", "memories"),
	}, candidate.GlobalRoots)
	require.Contains(t, candidate.RecursiveRoots, filepath.Join(home, ".codex", "sessions"))
	_, err := os.Stat(filepath.Join(home, ".codex"))
	require.True(t, os.IsNotExist(err))
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o755))
}
