package syncd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/syncgate"
	"github.com/stretchr/testify/require"
)

// TestSyncGate_DeniesFanOutUntilEnabled is the Slice 3 core test for the
// "discover + show, await config" default: a primary import still lands in
// the canonical store, but with a default-deny SyncGate the daemon must NOT
// fan out to other agents (no AGENTS.md materialized).
func TestSyncGate_DeniesFanOutUntilEnabled(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	// Stage .git so the artifact keeps ScopeProject and codex/kilo fan-out
	// targets land in `watched` as AGENTS.md (BRD-02 §4.13.5 downgrades
	// non-VCS paths to ScopeGlobal, which would route to ~/.codex instead).
	require.NoError(t, os.MkdirAll(filepath.Join(watched, ".git"), 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
		SyncGate:    syncgate.New(syncgate.Config{}), // default-deny
	})
	require.NoError(t, err)
	defer orch.Close()
	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(watched, "CLAUDE.md"),
		[]byte("# from claude-code\n"), 0o644))

	// Import still lands in the canonical store (visibility is never gated).
	require.Eventually(t, func() bool {
		mems, lerr := store.ListArtifacts(acf.KindMemory)
		return lerr == nil && len(mems) >= 1
	}, 3*time.Second, 100*time.Millisecond,
		"import must land in the store even when fan-out is gated")

	// But NO fan-out to codex/kilo (AGENTS.md) — give it a full settle window.
	time.Sleep(1 * time.Second)
	_, statErr := os.Stat(filepath.Join(watched, "AGENTS.md"))
	require.True(t, os.IsNotExist(statErr),
		"default-deny SyncGate must suppress AGENTS.md fan-out (await config)")
}

// TestSyncGate_AllowsEnabledTarget verifies that once both source and target
// agents are enabled, fan-out resumes.
func TestSyncGate_AllowsEnabledTarget(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(watched, ".git"), 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
		SyncGate: syncgate.New(syncgate.Config{Agents: map[string]bool{
			"claude-code": true,
			"codex":       true,
		}}),
	})
	require.NoError(t, err)
	defer orch.Close()
	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(watched, "CLAUDE.md"),
		[]byte("# from claude-code\n"), 0o644))

	require.Eventually(t, func() bool {
		_, serr := os.Stat(filepath.Join(watched, "AGENTS.md"))
		return serr == nil
	}, 3*time.Second, 100*time.Millisecond,
		"enabling codex must allow AGENTS.md fan-out")
}

// TestSyncGate_DeniesFanOutFromDisabledSource verifies the agent toggle is
// bidirectional for fan-out: disabling Kilo means Kilo's native files remain
// visible in the canonical store, but they do not feed enabled target agents.
func TestSyncGate_DeniesFanOutFromDisabledSource(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	kiloRoot := filepath.Join(root, ".config", "kilo")
	require.NoError(t, os.MkdirAll(kiloRoot, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		AdditionalRoots: []string{kiloRoot},
		RootsByAdapter: map[string][]string{
			"kilo": {kiloRoot},
		},
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
		SyncGate: syncgate.New(syncgate.Config{Agents: map[string]bool{
			"claude-code": true,
			"codex":       true,
			// kilo intentionally absent => disabled as a sync source.
		}}),
	})
	require.NoError(t, err)
	defer orch.Close()
	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(kiloRoot, "AGENTS.md"),
		[]byte("# from kilo\n"), 0o644))

	require.Eventually(t, func() bool {
		mems, lerr := store.ListArtifacts(acf.KindMemory)
		return lerr == nil && len(mems) >= 1
	}, 3*time.Second, 100*time.Millisecond,
		"disabled source imports must still land in the canonical store")

	time.Sleep(1 * time.Second)
	_, codexErr := os.Stat(filepath.Join(root, ".codex", "AGENTS.md"))
	require.True(t, os.IsNotExist(codexErr),
		"disabled source must not fan out to enabled codex target")
	_, claudeErr := os.Stat(filepath.Join(root, ".claude", "CLAUDE.md"))
	require.True(t, os.IsNotExist(claudeErr),
		"disabled source must not fan out to enabled claude-code target")
}
