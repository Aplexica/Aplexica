package main

import (
	"bytes"
	"testing"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

func runAdaptersCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(append([]string{"adapters"}, args...))
	t.Cleanup(func() {
		adaptersCheckSecretsRoot = ""
		adaptersStateDir = ""
	})
	err := rootCmd.Execute()
	return out.String(), err
}

func TestAdaptersList_ShowsAllFive(t *testing.T) {
	out, err := runAdaptersCmd(t, "list", "--secrets-root", t.TempDir())
	require.NoError(t, err)
	for _, name := range []string{"claude-code", "codex", "hermes", "openclaw", "kilo"} {
		require.Contains(t, out, name)
	}
	require.Contains(t, out, "mcp-server")
	require.Contains(t, out, "AGENTS.md")
	require.Contains(t, out, "supported surfaces: cli, desktop")
	require.Contains(t, out, "detected surfaces:")
}

func TestAdaptersCheck_ClaudeCodePasses(t *testing.T) {
	out, err := runAdaptersCmd(t,
		"check", "claude-code", "--secrets-root", t.TempDir())
	require.NoError(t, err)
	require.Contains(t, out, "passes the in-process conformance subset")
	require.Contains(t, out, "AGENTS.md")
	require.Contains(t, out, "CLAUDE.md")
	require.Contains(t, out, "SKILL.md")
	require.Contains(t, out, ".mcp.json")
}

func TestAdaptersCheck_EveryAdapterPasses(t *testing.T) {
	for _, name := range []string{"claude-code", "codex", "hermes", "openclaw", "kilo"} {
		t.Run(name, func(t *testing.T) {
			out, err := runAdaptersCmd(t,
				"check", name, "--secrets-root", t.TempDir())
			require.NoError(t, err, "adapter %s failed conformance:\n%s", name, out)
			require.Contains(t, out, "passes the in-process conformance subset")
		})
	}
}

func TestAdaptersCheck_UnknownAdapter(t *testing.T) {
	_, err := runAdaptersCmd(t,
		"check", "no-such-adapter", "--secrets-root", t.TempDir())
	require.Error(t, err)
}

// ─────────────────────────────────────────────────────────────────────
// v0.90.0 — adapters enable / disable
// ─────────────────────────────────────────────────────────────────────

func TestAdaptersDisable_PersistsToStateFile(t *testing.T) {
	tmp := t.TempDir()
	out, err := runAdaptersCmd(t,
		"disable", "codex",
		"--state-dir", tmp,
		"--secrets-root", tmp,
	)
	require.NoError(t, err)
	require.Contains(t, out, "disabled codex")
}

func TestAdaptersEnable_ClearsDisabled(t *testing.T) {
	tmp := t.TempDir()
	_, err := runAdaptersCmd(t,
		"disable", "codex",
		"--state-dir", tmp,
		"--secrets-root", tmp,
	)
	require.NoError(t, err)

	out, err := runAdaptersCmd(t,
		"enable", "codex",
		"--state-dir", tmp,
		"--secrets-root", tmp,
	)
	require.NoError(t, err)
	require.Contains(t, out, "enabled codex")
}

func TestAdaptersList_ShowsDisabledMarker(t *testing.T) {
	tmp := t.TempDir()
	_, err := runAdaptersCmd(t,
		"disable", "kilo",
		"--state-dir", tmp,
		"--secrets-root", tmp,
	)
	require.NoError(t, err)

	out, err := runAdaptersCmd(t,
		"list", "--state-dir", tmp, "--secrets-root", tmp,
	)
	require.NoError(t, err)
	require.Contains(t, out, "kilo [DISABLED]")
	require.Contains(t, out, "claude-code [enabled]")
}

func TestAdaptersEnableDisable_RejectsUnknown(t *testing.T) {
	tmp := t.TempDir()
	_, err := runAdaptersCmd(t,
		"disable", "no-such-adapter",
		"--state-dir", tmp,
		"--secrets-root", tmp,
	)
	require.Error(t, err)

	_, err = runAdaptersCmd(t,
		"enable", "no-such-adapter",
		"--state-dir", tmp,
		"--secrets-root", tmp,
	)
	require.Error(t, err)
}

func TestJoinSurfaces(t *testing.T) {
	if got := joinSurfaces([]adapter.Surface{adapter.SurfaceCLI, adapter.SurfaceDesktop}); got != "cli, desktop" {
		t.Fatalf("joinSurfaces() = %q, want %q", got, "cli, desktop")
	}
	if got := joinSurfaces(nil); got != "(not declared)" {
		t.Fatalf("joinSurfaces(nil) = %q, want %q", got, "(not declared)")
	}
}
