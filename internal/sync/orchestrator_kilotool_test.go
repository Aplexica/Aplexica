package syncd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/stretchr/testify/require"
)

// TestOrchestrator_KiloToolEdit_FansOutToClaude reproduces the E2E F4
// finding: an MCP server added to kilo.jsonc imports into the canonical
// store but never fans out to any other agent's MCP config.
func TestOrchestrator_KiloToolEdit_FansOutToClaude(t *testing.T) {
	root := realTempDir(t)
	adapters, store, _ := buildAllFiveAdapters(t, root)

	kiloCfg := filepath.Join(root, ".config", "kilo")
	require.NoError(t, os.MkdirAll(kiloCfg, 0o755))
	watched := filepath.Join(root, "watched")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	ctx, cancel := context.WithCancel(context.Background())

	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		AdditionalRoots: []string{kiloCfg},
		RootsByAdapter:  map[string][]string{"kilo": {kiloCfg}},
		Adapters:        adapters,
		Store:           store,
		QuietPeriod:     100 * time.Millisecond,
		GuardWindow:     2 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		_ = orch.Close()
		time.Sleep(500 * time.Millisecond)
	})

	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(kiloCfg, "kilo.jsonc"),
		[]byte(`{"mcp":{"e2e-weather":{"command":["uvx","weather-mcp"]}}}`), 0o644))

	// Global MCP fans out to ~/.claude.json's mcpServers key — the only
	// user-scope location Claude Code reads (`claude mcp list`); the old
	// ~/.claude/.mcp.json was a file Claude Code never loaded.
	claudeDest := filepath.Join(root, ".claude.json")
	require.Eventually(t, func() bool {
		data, rerr := os.ReadFile(claudeDest)
		return rerr == nil && strings.Contains(string(data), "e2e-weather")
	}, 15*time.Second, 100*time.Millisecond,
		"kilo MCP config edit must fan out to claude's ~/.claude.json; adapter errors: %v",
		orch.AdapterLastErrors())
}

// TestOrchestrator_ConcurrentDivergentEdits_NotSilentlyLost reproduces the
// E2E F6 race: two views of the same global artifact edited near-
// simultaneously (codex' AGENTS.md gets "alpha", kilo's gets "beta"). The
// first import's fan-out used to OVERWRITE the other file before its edit
// imported, and the recursion guard then swallowed the watcher event as an
// echo of our own write — one edit destroyed with no conflict recorded.
// With dest-hash tracking the losing fan-out defers, the pending edit
// imports, and the divergence is preserved in the event log + conflicts.
func TestOrchestrator_ConcurrentDivergentEdits_NotSilentlyLost(t *testing.T) {
	root := realTempDir(t)
	adapters, store, _ := buildAllThreeAdapters(t, root)

	codexRoot := filepath.Join(root, ".codex")
	kiloRoot := filepath.Join(root, ".config", "kilo")
	require.NoError(t, os.MkdirAll(codexRoot, 0o755))
	require.NoError(t, os.MkdirAll(kiloRoot, 0o755))
	watched := filepath.Join(root, "watched")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	confStore := &conflicts.Store{Root: filepath.Join(root, "conflicts")}
	require.NoError(t, confStore.Init())

	ctx, cancel := context.WithCancel(context.Background())

	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		AdditionalRoots: []string{codexRoot, kiloRoot},
		RootsByAdapter:  map[string][]string{"codex": {codexRoot}, "kilo": {kiloRoot}},
		Adapters:        adapters,
		Store:           store,
		ConflictStore:   confStore,
		ConflictWindow:  30 * time.Second,
		QuietPeriod:     100 * time.Millisecond,
		GuardWindow:     2 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		_ = orch.Close()
		time.Sleep(500 * time.Millisecond)
	})

	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// Seed: one agent writes the base; wait until it lands in the other.
	codexFile := filepath.Join(codexRoot, "AGENTS.md")
	kiloFile := filepath.Join(kiloRoot, "AGENTS.md")
	require.NoError(t, os.WriteFile(codexFile, []byte("base\n"), 0o644))
	require.Eventually(t, func() bool {
		data, rerr := os.ReadFile(kiloFile)
		return rerr == nil && strings.Contains(string(data), "base")
	}, 15*time.Second, 100*time.Millisecond, "seed content must sync to kilo first")

	// Near-simultaneous divergent edits to the two views.
	require.NoError(t, os.WriteFile(codexFile, []byte("base\nalpha\n"), 0o644))
	require.NoError(t, os.WriteFile(kiloFile, []byte("base\nbeta\n"), 0o644))

	// Neither edit may vanish: each must be preserved in the event log
	// (which also implies the conflict detector saw both heads).
	require.Eventually(t, func() bool {
		arts, lerr := store.ListArtifacts(acf.KindMemory)
		if lerr != nil {
			return false
		}
		seenAlpha, seenBeta := false, false
		for _, art := range arts {
			events, _ := store.ReadEvents(acf.KindMemory, art.ArtifactID)
			for _, e := range events {
				p := string(e.Payload)
				if strings.Contains(p, "alpha") {
					seenAlpha = true
				}
				if strings.Contains(p, "beta") {
					seenBeta = true
				}
			}
		}
		return seenAlpha && seenBeta
	}, 15*time.Second, 200*time.Millisecond,
		"both divergent edits must import; losing one silently is data loss")
}
