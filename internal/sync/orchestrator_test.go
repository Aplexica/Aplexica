package syncd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/adapter/kilo"
	"github.com/aplexica/aplexica/internal/adapter/openclaw"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/aplexica/aplexica/internal/hermesdb"
	"github.com/aplexica/aplexica/internal/pausestate"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

// macOS test temp dirs may live under /var/folders symlink to
// /private/var/folders. Resolve to the real path so filepath comparisons
// inside the orchestrator (and recursion guard) line up with native event
// paths from FSEvents.
func realTempDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	resolved, err := filepath.EvalSymlinks(tmp)
	require.NoError(t, err)
	return resolved
}

func buildAllThreeAdapters(t *testing.T, root string) ([]adapter.Adapter, *acf.Store, *secrets.Store) {
	t.Helper()
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(root, "secrets")}
	require.NoError(t, ss.Init())

	cc := claudecode.New()
	cc.HomeDir = root
	cc.SecretsStore = ss

	cx := codex.New()
	cx.HomeDir = root
	cx.SecretsStore = ss

	k := kilo.New()
	k.HomeDir = root
	k.SecretsStore = ss

	return []adapter.Adapter{cc, cx, k}, store, ss
}

// buildAllFiveAdapters extends buildAllThreeAdapters with hermes and
// openclaw — the full V1 set. Used by the BRD-02 §5.4 #5 recursion-
// guard test which must hold across every adapter combination.
//
// hermes is constructed with an empty DB path (its conversation
// importer accepts that for tests) and disabled hermeswatch — the
// recursion-guard test only exercises memory artifacts.
func buildAllFiveAdapters(t *testing.T, root string) ([]adapter.Adapter, *acf.Store, *secrets.Store) {
	t.Helper()
	three, store, ss := buildAllThreeAdapters(t, root)

	h := hermes.New()
	h.HomeDir = root
	h.SecretsStore = ss

	oc := openclaw.New()
	oc.HomeDir = root
	oc.SecretsStore = ss

	all := append(three, h, oc)
	return all, store, ss
}

func TestOrchestrator_Memory_FansOutFromClaudeCode(t *testing.T) {
	root := realTempDir(t)
	// Use a separate watched dir so the store/secrets dirs don't appear
	// as events.
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	// v0.61.0 downgrades non-VCS paths to ScopeGlobal. This test asserts
	// project-scope fan-out semantics, so stage a project marker.
	require.NoError(t, os.MkdirAll(filepath.Join(watched, ".git"), 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := NewOrchestrator(Config{
		Dir:      watched,
		Adapters: adapters,
		Store:    store,
		// Short quiet period and guard window for tests.
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// Write CLAUDE.md — claudecode picks it up, codex and kilo should
	// receive AGENTS.md fan-out.
	require.NoError(t, os.WriteFile(filepath.Join(watched, "CLAUDE.md"),
		[]byte("# from claude-code\n"), 0o644))

	// Wait for inbound debounce + fan-out + filesystem to settle.
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(watched, "AGENTS.md"))
		return err == nil
	}, 3*time.Second, 100*time.Millisecond,
		"AGENTS.md should appear within ~3s of CLAUDE.md write (codex+kilo fan-out)")

	got, err := os.ReadFile(filepath.Join(watched, "AGENTS.md"))
	require.NoError(t, err)
	require.Equal(t, "# from claude-code\n", string(got),
		"fanned-out AGENTS.md must match the source CLAUDE.md content")

	// Canonical store should have exactly 1 memory artifact.
	memories, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, memories, 1,
		"only one memory artifact should exist; fan-out writes are suppressed by the recursion guard")
}

// containsAllAgents reports whether have contains every name in want.
func containsAllAgents(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// TestOrchestrator_Memory_CreditsSharedFileCoTargets asserts that an agent
// whose native memory destination is the SAME file another agent writes (or is
// the source file itself) is still credited in the artifact's SyncedAgents.
//
// codex and kilo both map memory to <project>/AGENTS.md, so when claude-code's
// CLAUDE.md fans out, codex wins the first-wins dedup and writes AGENTS.md —
// but kilo READS that very same AGENTS.md. Coverage surfaces (the per-project
// memory view) must therefore report kilo as synced too. Regression guard for
// "I don't see claude-code/codex memories synced into kilo": the file content
// was always present, but kilo was never credited.
func TestOrchestrator_Memory_CreditsSharedFileCoTargets(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(filepath.Join(watched, ".git"), 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root) // claude-code, codex, kilo
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()
	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// claude-code authors CLAUDE.md; fan-out writes AGENTS.md (codex wins the
	// dedup; kilo shares that exact path).
	require.NoError(t, os.WriteFile(filepath.Join(watched, "CLAUDE.md"),
		[]byte("# from claude-code\n"), 0o644))

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(filepath.Join(watched, "AGENTS.md"))
		return statErr == nil
	}, 3*time.Second, 100*time.Millisecond,
		"AGENTS.md should appear via fan-out")

	// The single memory artifact (the CLAUDE.md source) must credit BOTH codex
	// (wrote AGENTS.md) and kilo (reads the same AGENTS.md).
	require.Eventually(t, func() bool {
		mems, lerr := store.ListArtifacts(acf.KindMemory)
		if lerr != nil || len(mems) != 1 {
			return false
		}
		return containsAllAgents(mems[0].SyncedAgents, "codex", "kilo")
	}, 3*time.Second, 100*time.Millisecond,
		"CLAUDE.md memory must credit codex AND kilo as SyncedAgents (kilo shares AGENTS.md)")
}

// TestOrchestrator_ConfigRootMemory_ImportsAsKiloGlobal locks in the
// path-ownership + scope-inference contract: a memory file under kilo's own
// config root (~/.config/kilo/AGENTS.md) must be imported by KILO as a GLOBAL
// artifact — never claimed by an alphabetically-earlier adapter (codex) nor
// scoped as a project. Regression guard for the spurious scope=project
// ~/.config/kilo/AGENTS.md artifact.
func TestOrchestrator_ConfigRootMemory_ImportsAsKiloGlobal(t *testing.T) {
	root := realTempDir(t)
	adapters, store, _ := buildAllThreeAdapters(t, root) // claude-code, codex, kilo (HomeDir=root)
	configRoot := filepath.Join(root, ".config", "kilo")
	require.NoError(t, os.MkdirAll(configRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configRoot, "AGENTS.md"), []byte("# kilo global memory\n"), 0o644))

	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		AdditionalRoots: []string{configRoot},
		RootsByAdapter:  map[string][]string{"kilo": {configRoot}},
		Adapters:        adapters,
		Store:           store,
		QuietPeriod:     100 * time.Millisecond,
		GuardWindow:     2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	require.NoError(t, orch.InitialScan(ctx))

	mems, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, mems, 1, "exactly one memory artifact for kilo's config-root AGENTS.md")
	require.Equal(t, acf.ScopeGlobal, mems[0].Scope,
		"a file under kilo's config root must import as kilo GLOBAL, not a project artifact")
	require.Equal(t, filepath.Join(configRoot, "AGENTS.md"), mems[0].SourcePath)
	events, err := store.ReadEvents(acf.KindMemory, mems[0].ArtifactID)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	require.Equal(t, "kilo", events[0].Provenance.SourceAgent,
		"path ownership: only kilo (owner of ~/.config/kilo) may import the file")
}

// TestOrchestrator_InitialScan_SkipsNodeModules verifies the recursive backfill
// scan prunes node_modules (and .git): a memory file buried in a project's
// node_modules must NOT be imported, only the real top-level one.
func TestOrchestrator_InitialScan_SkipsNodeModules(t *testing.T) {
	root := realTempDir(t)
	adapters, store, _ := buildAllThreeAdapters(t, root)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(filepath.Join(watched, ".git"), 0o755)) // keep ScopeProject
	require.NoError(t, os.WriteFile(filepath.Join(watched, "AGENTS.md"), []byte("# real\n"), 0o644))

	// A memory-shaped file vendored deep inside node_modules must be pruned.
	nm := filepath.Join(watched, "node_modules", "somepkg")
	require.NoError(t, os.MkdirAll(nm, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nm, "AGENTS.md"), []byte("# vendored — must be ignored\n"), 0o644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
		Recursive:   true,
	})
	require.NoError(t, err)
	defer orch.Close()

	require.NoError(t, orch.InitialScan(ctx))

	mems, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, mems, 1, "node_modules/AGENTS.md must be pruned; only the top-level AGENTS.md imports")
	require.Equal(t, filepath.Join(watched, "AGENTS.md"), mems[0].SourcePath)
}

func TestOrchestrator_Memory_FansOutFromCodex(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	// v0.61.0: BRD-02 §4.13.5 downgrades non-VCS paths to ScopeGlobal.
	// This test asserts project-scope fan-out semantics, so stage a
	// .git/ so the project keeps ScopeProject.
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
	})
	require.NoError(t, err)
	defer orch.Close()

	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// Write AGENTS.md — codex (alphabetically first of codex/kilo) picks
	// it up; claudecode should receive a CLAUDE.md fan-out.
	require.NoError(t, os.WriteFile(filepath.Join(watched, "AGENTS.md"),
		[]byte("# from codex\n"), 0o644))

	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(watched, "CLAUDE.md"))
		return err == nil
	}, 3*time.Second, 100*time.Millisecond,
		"CLAUDE.md should appear within ~3s of AGENTS.md write (claudecode fan-out)")

	got, err := os.ReadFile(filepath.Join(watched, "CLAUDE.md"))
	require.NoError(t, err)
	require.Equal(t, "# from codex\n", string(got))
}

func TestOrchestrator_NoInfiniteLoop(t *testing.T) {
	// Stress test: the orchestrator must NOT loop on its own fan-out writes.
	// Write CLAUDE.md, wait long enough for several propagation cycles to
	// have happened if the guard were broken, then confirm exactly one
	// memory artifact exists in the canonical store.
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(watched, "CLAUDE.md"),
		[]byte("# initial\n"), 0o644))

	// Hold open long enough for fan-out + guard window + (would-be) loops.
	time.Sleep(2500 * time.Millisecond)

	memories, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, memories, 1,
		"recursion guard must prevent fan-out writes from being re-imported")

	// The single memory artifact should have exactly one event (the
	// original create); a loop would add update events.
	events, err := store.ReadEvents(acf.KindMemory, memories[0].ArtifactID)
	require.NoError(t, err)
	require.Len(t, events, 1,
		"a single user write must produce a single canonical event; loops would produce more")
}

// TestOrchestrator_PauseSkipsFanout asserts the v0.88.0 FR-03.11
// pause gate: writing a memory file while sync is paused MUST NOT
// produce fan-out writes on other adapters.
func TestOrchestrator_PauseSkipsFanout(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)
	ps := &pausestate.Store{Path: filepath.Join(root, "p.json")}
	require.NoError(t, ps.PauseGlobal(0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
		PauseStore:  ps,
	})
	require.NoError(t, err)
	defer orch.Close()
	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// Write CLAUDE.md while paused. claudecode should still Import
	// (inbound isn't paused — pause is for outbound writes only); but
	// codex/kilo should NOT receive AGENTS.md fan-out.
	require.NoError(t, os.WriteFile(filepath.Join(watched, "CLAUDE.md"),
		[]byte("# paused world\n"), 0o644))

	// Wait long enough for the import + would-be-fanout cycle to settle.
	time.Sleep(1500 * time.Millisecond)

	// Memory artifact exists (inbound import ran).
	memories, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, memories, 1, "inbound import should still run while paused")

	// But AGENTS.md should NOT exist (fan-out was skipped).
	_, err = os.Stat(filepath.Join(watched, "AGENTS.md"))
	require.True(t, os.IsNotExist(err),
		"AGENTS.md should NOT have been written while sync was paused")
}

// TestOrchestrator_PauseAdapter_SkipsOnlyNamed asserts per-adapter
// pause: writing CLAUDE.md while codex is paused fans out to kilo
// but not codex.
func TestOrchestrator_PauseAdapter_SkipsOnlyNamed(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	// Keep the CLAUDE.md import project-scoped so Kilo's AGENTS.md fan-out
	// is folder-local when Codex is paused.
	require.NoError(t, os.MkdirAll(filepath.Join(watched, ".git"), 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)
	ps := &pausestate.Store{Path: filepath.Join(root, "p.json")}
	require.NoError(t, ps.PauseAdapter("codex", 0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
		PauseStore:  ps,
	})
	require.NoError(t, err)
	defer orch.Close()
	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(watched, "CLAUDE.md"),
		[]byte("# per-adapter pause\n"), 0o644))

	// kilo's NativePath for memory writes AGENTS.md, same as codex.
	// First-wins dedupe means whichever is alphabetically first OWNS
	// the destination. codex (paused) < kilo alphabetically; the
	// dedupe map records codex's claim, but the pause filter runs
	// BEFORE dedupe, so codex is dropped and kilo takes the slot.
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(watched, "AGENTS.md"))
		return err == nil
	}, 3*time.Second, 100*time.Millisecond,
		"AGENTS.md should appear via kilo's fan-out (codex paused, kilo not)")
}

// TestOrchestrator_MultiMatch_OneAdapterImports closes the v0.81.0
// deferral: when multiple adapters declare the same basename (here:
// AGENTS.md across codex / hermes / kilo / openclaw; claudecode
// reads it as an alias), only ONE adapter's Import is called.
// Verified by the artifact count + the source-agent of the lone event.
//
// Without v0.81.0's BasenameToKind picker, each adapter's Import
// would have been called in the candidate-scan loop, producing 5
// events on the same artifact ID (one create + four updates). The
// v0.81.0 fix eliminated that; v0.93.0 codifies the invariant.
func TestOrchestrator_MultiMatch_OneAdapterImports(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	adapters, store, _ := buildAllFiveAdapters(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()
	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// Write AGENTS.md. Primary claimants (NativePath produces
	// AGENTS.md) are codex, hermes (when Name="AGENTS.md"), kilo,
	// openclaw. Among those, codex is alphabetically first.
	require.NoError(t, os.WriteFile(filepath.Join(watched, "AGENTS.md"),
		[]byte("# multi-match invariant\n"), 0o644))

	time.Sleep(2500 * time.Millisecond)

	memories, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, memories, 1,
		"multi-match invariant violated: 5 adapters claim AGENTS.md but only "+
			"ONE adapter's Import should run per file event")

	events, err := store.ReadEvents(acf.KindMemory, memories[0].ArtifactID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(events), 1)
	// The first event is the Import; subsequent events would be
	// fan-out writes that the recursion guard suppresses, so we
	// expect ALMOST always 1, occasionally 2 if an adapter's Export
	// re-imports via a path the guard missed. The hard invariant
	// is just "not 5".
	require.Less(t, len(events), 5,
		"multi-match invariant violated: 5+ events on the same artifact suggests "+
			"each claiming adapter ran Import (the pre-v0.81.0 bug)")

	// Confirm the source-agent of the first event is the
	// alphabetically-first PRIMARY claimant for AGENTS.md. Codex's
	// NativePath returns AGENTS.md for memory (primary); claudecode's
	// returns CLAUDE.md (alias). Among primary claimants (codex,
	// hermes, kilo, openclaw), codex is alphabetically first.
	require.Equal(t, "codex", events[0].Provenance.SourceAgent,
		"primary picker must pick codex (alphabetically first primary claimant) "+
			"over claudecode (alias claimant) for AGENTS.md")
}

// TestOrchestrator_NoInfiniteLoop_AllFiveAdapters extends
// TestOrchestrator_NoInfiniteLoop to the full V1 5-adapter set so
// BRD-02 §5.4 #5 ("Materialize an outbound event; ensure no inbound
// event is fired for the same write") holds across every adapter
// pair, not just the claude-code/codex/kilo subset.
func TestOrchestrator_NoInfiniteLoop_AllFiveAdapters(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	adapters, store, _ := buildAllFiveAdapters(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()
	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// AGENTS.md is the AAIF cross-tool memory form supported by all
	// five adapters. Writing it should produce exactly one canonical
	// memory artifact regardless of how many adapters claim the
	// filename.
	require.NoError(t, os.WriteFile(filepath.Join(watched, "AGENTS.md"),
		[]byte("# all-five recursion guard test\n"), 0o644))

	// Hold open through several would-be loop cycles.
	time.Sleep(2500 * time.Millisecond)

	memories, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)

	// AGENTS.md is shared across 5 adapters but the source-picker
	// selects ONE primary (the alphabetically-first adapter whose
	// NativePath produces AGENTS.md — codex). The other adapters
	// receive fan-out writes via Export but those writes must NOT
	// re-trigger Import (the recursion guard's job). So we expect
	// exactly one canonical artifact across all 5 adapters.
	require.Len(t, memories, 1,
		"5-adapter recursion guard violated: expected 1 canonical memory artifact, "+
			"got %d (loop suspected)", len(memories))

	events, err := store.ReadEvents(acf.KindMemory, memories[0].ArtifactID)
	require.NoError(t, err)
	require.Len(t, events, 1,
		"one user write must produce one event regardless of how many "+
			"adapters fanned out to AGENTS.md; got %d events", len(events))
}

func TestOrchestrator_Recursive_FansOutFromSubdir(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	// Pre-existing subdirectory with a file. With recursive watching, an
	// edit there should produce an artifact + fan-out to that same subdir's
	// AGENTS.md (via codex/kilo's NativePath using contextDir=sub).
	sub := filepath.Join(watched, "feature-x")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	// Mark the subdirectory as the project root so the recursive fan-out
	// target remains inside the subdir, not the parent watched folder.
	require.NoError(t, os.MkdirAll(filepath.Join(sub, ".git"), 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
		Recursive:   true,
	})
	require.NoError(t, err)
	defer orch.Close()

	go orch.Run(ctx)
	time.Sleep(200 * time.Millisecond) // recursive walk + per-dir Sources need a moment

	// Write CLAUDE.md INSIDE the subdir.
	require.NoError(t, os.WriteFile(filepath.Join(sub, "CLAUDE.md"),
		[]byte("# in subdir\n"), 0o644))

	// AGENTS.md should appear in the SAME subdir (contextDir = sub). On
	// Windows, Stat can succeed while the exporter/watcher still holds a
	// transient file handle, so wait until the file is readable too.
	agentsPath := filepath.Join(sub, "AGENTS.md")
	var got []byte
	require.Eventually(t, func() bool {
		var err error
		got, err = os.ReadFile(agentsPath)
		return err == nil && string(got) == "# in subdir\n"
	}, 4*time.Second, 100*time.Millisecond,
		"recursive sync must fan out within the subdir where the change originated")
	require.Equal(t, "# in subdir\n", string(got))
}

// exportCountingAdapter wraps an Adapter and records how many times its
// Export method was invoked. Used by TestFanOut_SkipsAdapterThatDoesNotHandleFormat
// to detect whether the orchestrator's format gate fires before Export
// (the user-visible side-effect is the same — Export would have errored
// out internally — but the gate is what we're actually verifying).
type exportCountingAdapter struct {
	adapter.Adapter
	exports int
}

func (e *exportCountingAdapter) Export(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	e.exports++
	return e.Adapter.Export(ctx, store, artifactID, destPath)
}

func TestFanOut_SkipsAdapterThatDoesNotHandleFormat(t *testing.T) {
	// claude-code imports a .jsonl session; the orchestrator must NOT
	// attempt to fan out to hermes (which says NativePath supports=true
	// for conversation kind but cannot decode claude-code's JSONL format).
	// Pre-v0.14.0, the orchestrator would invoke hermes.Export, which
	// would error inside decodeSessionBundle ("unsupported conversation
	// format"). v0.14.0 skips at the orchestrator level via HandlesFormat.
	//
	// We detect the gate firing by wrapping hermes in an Export counter:
	// after the fan-out cycle settles, Export must have been called zero
	// times for the conversation artifact.
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(root, "secrets")}
	require.NoError(t, ss.Init())

	cc := claudecode.New()
	cc.HomeDir = root
	cc.SecretsStore = ss

	// Hermes needs an initialized state.db at HomeDir/.hermes/state.db for
	// NativePath(conversation) to return supports=true. (The fan-out path
	// also requires the DB to exist; without it, NativePath still returns
	// supports=true so the gate still applies.)
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".hermes"), 0o755))
	dbPath := filepath.Join(root, ".hermes", "state.db")
	dbInit, err := hermesdb.InitTestDB(dbPath)
	require.NoError(t, err)
	require.NoError(t, dbInit.Close())

	h := hermes.New()
	h.HomeDir = root
	h.SecretsStore = ss
	hCounter := &exportCountingAdapter{Adapter: h}

	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    []adapter.Adapter{cc, hCounter},
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// Drop a Claude Code .jsonl into watchDir. claudecode is alphabetically
	// before hermes so it wins as primary.
	jsonlPath := filepath.Join(watched, "session-xyz.jsonl")
	require.NoError(t, os.WriteFile(jsonlPath,
		[]byte(`{"type":"summary","leafUuid":"abc","sessionId":"xyz"}`+"\n"), 0o644))

	// Wait for the canonical store to gain the conversation artifact —
	// that's the signal that the primary import + fan-out cycle ran.
	require.Eventually(t, func() bool {
		convos, err := store.ListArtifacts(acf.KindConversation)
		return err == nil && len(convos) == 1
	}, 3*time.Second, 50*time.Millisecond,
		"claude-code conversation should be imported within ~3s of writing the .jsonl")

	// Brief settle window so a (broken) fan-out has time to fire.
	time.Sleep(300 * time.Millisecond)

	require.Zero(t, hCounter.exports,
		"hermes.Export must not be invoked for a claude-code.session.jsonl artifact — the orchestrator's HandlesFormat gate must skip it")
}

// causedByCapturingAdapter wraps an Adapter and records the
// adapter.CausedByFromContext value the orchestrator stamps on the
// fan-out Export ctx. Used by TestFanOut_PopulatesCausedByOnFanOutEvents
// to verify the v0.20.0 plumbing — the orchestrator must call
// adapter.WithCausedBy(ctx, sourceHash) before invoking Export.
type causedByCapturingAdapter struct {
	adapter.Adapter
	mu       sync.Mutex
	captured []string
}

func (c *causedByCapturingAdapter) Export(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	c.mu.Lock()
	c.captured = append(c.captured, adapter.CausedByFromContext(ctx))
	c.mu.Unlock()
	return c.Adapter.Export(ctx, store, artifactID, destPath)
}

func (c *causedByCapturingAdapter) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.captured))
	copy(out, c.captured)
	return out
}

func TestFanOut_PopulatesCausedByOnFanOutEvents(t *testing.T) {
	// v0.20.0: the orchestrator must wrap the per-Export ctx with
	// adapter.WithCausedBy(sourceHash) so the destination adapter can stamp
	// Provenance.CausedBy on any event it writes. The current fan-out path
	// only writes a file (no destination event is appended in v0.20.0; the
	// receive-side guard is a future milestone), so we verify the plumbing
	// by wrapping the secondary adapter and inspecting the ctx via
	// adapter.CausedByFromContext directly. The captured value must equal
	// the source artifact's most-recent event hash.
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	// v0.61.0: BRD-02 §4.13.5 downgrades non-VCS paths to ScopeGlobal.
	// This test verifies fan-out plumbing on a project-scope artifact;
	// stage a .git/ so the project keeps ScopeProject (and hence fans
	// out per v0.57.0's gate).
	require.NoError(t, os.MkdirAll(filepath.Join(watched, ".git"), 0o755))

	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(root, "secrets")}
	require.NoError(t, ss.Init())

	cc := claudecode.New()
	cc.HomeDir = root
	cc.SecretsStore = ss

	cx := codex.New()
	cx.HomeDir = root
	cx.SecretsStore = ss
	cxCapture := &causedByCapturingAdapter{Adapter: cx}

	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    []adapter.Adapter{cc, cxCapture},
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// Write CLAUDE.md — claudecode primary, codex fan-out target.
	require.NoError(t, os.WriteFile(filepath.Join(watched, "CLAUDE.md"),
		[]byte("# causedBy test\n"), 0o644))

	// Wait for the fan-out file to appear AND at least one Export to be
	// recorded on the capturing adapter.
	require.Eventually(t, func() bool {
		if _, err := os.Stat(filepath.Join(watched, "AGENTS.md")); err != nil {
			return false
		}
		return len(cxCapture.snapshot()) > 0
	}, 3*time.Second, 50*time.Millisecond,
		"codex.Export must run for the CLAUDE.md fan-out to AGENTS.md")

	// The source event for the only memory artifact carries the hash the
	// orchestrator should propagate.
	memories, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, memories, 1, "exactly one memory artifact must exist (the source CLAUDE.md)")
	events, err := store.ReadEvents(acf.KindMemory, memories[0].ArtifactID)
	require.NoError(t, err)
	require.NotEmpty(t, events, "source artifact must have at least one event")
	sourceHash := events[len(events)-1].Hash
	require.NotEmpty(t, sourceHash, "source event must have a non-empty Hash")

	captured := cxCapture.snapshot()
	require.NotEmpty(t, captured, "the capturing adapter must have observed at least one Export call")
	require.Equal(t, sourceHash, captured[0],
		"orchestrator must stamp Provenance.CausedBy = source event hash via adapter.WithCausedBy on the fan-out Export ctx")
}

func TestOrchestrator_InitialScan_ImportsPreExistingFiles(t *testing.T) {
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
	// File exists BEFORE the orchestrator starts.
	require.NoError(t, os.WriteFile(filepath.Join(watchDir, "CLAUDE.md"), []byte("memory body"), 0o644))

	orch, err := NewOrchestrator(Config{
		Dir:         watchDir,
		Adapters:    []adapter.Adapter{cc},
		Store:       store,
		QuietPeriod: 50 * time.Millisecond,
		GuardWindow: 1 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	// InitialScan should detect and import the pre-existing CLAUDE.md.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = orch.InitialScan(ctx)
	require.NoError(t, err)

	arts, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, arts, 1, "InitialScan should have imported the pre-existing CLAUDE.md")
}

// TestOrchestrator_InitialScan_TwiceIsIdempotent reproduces the restart bug:
// the daemon runs InitialScan on every startup, re-importing every native
// file. A second scan over unchanged files must NOT append a redundant
// "update" event per artifact — otherwise every restart bloats the event log
// and floods the events feed with a same-second burst of "synced" rows.
func TestOrchestrator_InitialScan_TwiceIsIdempotent(t *testing.T) {
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
	path := filepath.Join(watchDir, "CLAUDE.md")
	require.NoError(t, os.WriteFile(path, []byte("memory body"), 0o644))

	local := newTestDevice(t, "this-device")
	pub := &stubRemotePublisher{}

	orch, err := NewOrchestrator(Config{
		Dir:                  watchDir,
		Adapters:             []adapter.Adapter{cc},
		Store:                store,
		QuietPeriod:          50 * time.Millisecond,
		GuardWindow:          1 * time.Second,
		RemoteEventPublisher: pub,
		LocalDeviceID:        local.id,
		RecipientResolver:    staticResolver{recipients: []Recipient{{DeviceID: local.id, PubKey: local.pub}}},
		DeviceKeyProvider:    fixedKeyProvider{priv: local.priv},
	})
	require.NoError(t, err)
	defer orch.Close()

	ctx := context.Background()

	// First scan: imports the file (create event).
	require.NoError(t, orch.InitialScan(ctx))
	arts, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, arts, 1)
	id := arts[0].ArtifactID
	events, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, events, 1, "first scan creates exactly one event")
	require.Equal(t, 1, pub.Count(), "first scan publishes the fresh event once")

	// Second scan: simulates a daemon restart re-walking the SAME unchanged file.
	require.NoError(t, orch.InitialScan(ctx))
	arts, err = store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, arts, 1, "no new artifact minted on rescan")
	events, err = store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, events, 1,
		"a restart rescan over unchanged content must NOT append a redundant update event")
	require.Equal(t, 1, pub.Count(),
		"a byte-unchanged rescan must not republish the already-sent head event")

	// Third scan: force cache invalidation by changing only mtime. This mirrors
	// a fresh daemon process or metadata churn that makes the scanner re-import
	// an unchanged native file. The adapter returns the existing artifact ID for
	// identity, but no fresh canonical event was appended, so no remote publish
	// should happen.
	touch := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(path, touch, touch))
	require.NoError(t, orch.InitialScan(ctx))
	events, err = store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, events, 1,
		"a metadata-only rescan must not append a redundant update event")
	require.Equal(t, 1, pub.Count(),
		"a metadata-only rescan must not republish the already-sent head event")

	// Fourth scan: a real content edit appends a new event and should publish
	// exactly once.
	require.NoError(t, os.WriteFile(path, []byte("changed memory body"), 0o644))
	require.NoError(t, orch.InitialScan(ctx))
	events, err = store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, events, 2, "content edit should append one update event")
	require.Equal(t, 2, pub.Count(), "content edit should publish exactly one new event")
}

func TestFreshlyCommittedIDs_UsesHeadHashNotNativeTimestamp(t *testing.T) {
	tmp := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	path := filepath.Join(tmp, "rollout-old-timestamp.jsonl")
	id := acf.NewID()
	now := time.Now().UTC()
	oldNativeTime := now.Add(-30 * time.Second)
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             filepath.Base(path),
		SourcePath:       path,
		CreatedAt:        oldNativeTime,
		UpdatedAt:        oldNativeTime,
	}))
	firstPayload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{{
			Type:    acf.EventTypeTurn,
			Role:    "user",
			Content: []acf.ContentBlock{{Type: "text", Text: "old prompt"}},
		}},
	})
	require.NoError(t, err)
	first := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  oldNativeTime,
		Provenance: acf.Provenance{DeviceID: "dev", SourceAgent: "codex"},
		Payload:    firstPayload,
	}
	require.NoError(t, store.AppendEvent(acf.KindConversation, first))

	orch := &Orchestrator{cfg: Config{Store: store}}
	prior := orch.sourcePathHeadHashes(path)

	art, err := store.ReadArtifact(acf.KindConversation, id)
	require.NoError(t, err)
	updatePayload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationDeltaFormatV1,
		Events: []acf.ConversationEvent{{
			Type:    acf.EventTypeTurn,
			Role:    "assistant",
			Content: []acf.ContentBlock{{Type: "text", Text: "old answer"}},
		}},
	})
	require.NoError(t, err)
	update := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  oldNativeTime.Add(time.Second),
		Provenance: acf.Provenance{DeviceID: "dev", SourceAgent: "codex"},
		Payload:    updatePayload,
		ParentHash: art.HeadEventHash,
	}
	require.True(t, update.Timestamp.Before(now), "test must simulate a native timestamp older than import processing")
	require.NoError(t, store.AppendEvent(acf.KindConversation, update))

	require.Equal(t, []string{id}, orch.freshlyCommittedIDs([]string{id}, prior),
		"a changed head hash is fresh even when the native event timestamp is older than import start")
	require.Empty(t, orch.freshlyCommittedIDs([]string{id}, orch.sourcePathHeadHashes(path)),
		"an unchanged rescan with the same head hash must not republish")
}

func TestSourcePathHeadIndex_RefreshesAfterImport(t *testing.T) {
	tmp := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	path := filepath.Join(tmp, "AGENTS.md")
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeGlobal,
		Name:             filepath.Base(path),
		SourcePath:       path,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	firstPayload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "first"})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Payload:    firstPayload,
	}))

	orch := &Orchestrator{cfg: Config{Store: store}, sourcePathHeads: buildSourcePathHeadIndex(store)}
	prior := orch.sourcePathHeadHashes(path)
	require.Equal(t, storeMustReadArtifact(t, store, acf.KindMemory, id).HeadEventHash, prior[id])

	art := storeMustReadArtifact(t, store, acf.KindMemory, id)
	nextPayload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "second"})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now.Add(time.Second),
		ParentHash: art.HeadEventHash,
		Payload:    nextPayload,
	}))

	orch.refreshSourcePathHeads([]string{id})
	require.NotEqual(t, prior[id], orch.sourcePathHeadHashes(path)[id])
	require.Equal(t, []string{id}, orch.freshlyCommittedIDs([]string{id}, prior))
}

func TestSourcePathHeadHashes_FallsBackWhenOrchestratorLockBusy(t *testing.T) {
	tmp := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	path := filepath.Join(tmp, "rollout-hot.jsonl")
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             filepath.Base(path),
		SourcePath:       path,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{{
			Type:    acf.EventTypeTurn,
			Role:    "user",
			Content: []acf.ContentBlock{{Type: "text", Text: "prompt"}},
		}},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Payload:    payload,
	}))

	orch := &Orchestrator{cfg: Config{Store: store}, sourcePathHeads: buildSourcePathHeadIndex(store)}
	orch.mu.Lock()
	defer orch.mu.Unlock()

	done := make(chan map[string]string, 1)
	go func() {
		done <- orch.sourcePathHeadHashes(path)
	}()
	select {
	case heads := <-done:
		require.Equal(t, storeMustReadArtifact(t, store, acf.KindConversation, id).HeadEventHash, heads[id])
	case <-time.After(2 * time.Second):
		t.Fatal("sourcePathHeadHashes blocked behind unrelated orchestrator lock")
	}
}

func storeMustReadArtifact(t *testing.T, store *acf.Store, kind acf.Kind, id string) acf.Artifact {
	t.Helper()
	art, err := store.ReadArtifact(kind, id)
	require.NoError(t, err)
	return art
}

// TestOrchestrator_DetectsDivergentConcurrentWrites verifies BRD-03 §4.6 +
// ADR-0038: when the same artifact is written by two different adapters
// within ConflictWindow with different payloads, the orchestrator records a
// conflict file capturing both divergent heads.
//
// Driving the detection via the file watcher is flaky in tests (one of the
// two writes can be debounced away or coalesced); we call the
// MaybeRecordConflictForTest helper directly after manually staging two
// divergent events into the same artifact's log.
func TestOrchestrator_DetectsDivergentConcurrentWrites(t *testing.T) {
	tmp := realTempDir(t)
	confStore := &conflicts.Store{Root: filepath.Join(tmp, "conflicts")}
	require.NoError(t, confStore.Init())

	adapters, store, _ := buildAllThreeAdapters(t, tmp)
	// Pick cc as the "primary" adapter for the test helper call (adapter
	// identity doesn't actually affect the detection logic — it inspects
	// the last two events' provenance, not the adapter argument).
	var cc adapter.Adapter
	for _, ad := range adapters {
		if ad.Name() == "claude-code" {
			cc = ad
			break
		}
	}
	require.NotNil(t, cc)

	watchDir := filepath.Join(tmp, "proj")
	require.NoError(t, os.MkdirAll(watchDir, 0o755))

	orch, err := NewOrchestrator(Config{
		Dir:            watchDir,
		Adapters:       adapters,
		Store:          store,
		QuietPeriod:    50 * time.Millisecond,
		GuardWindow:    1 * time.Second,
		ConflictStore:  confStore,
		ConflictWindow: 30 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	// First write via claudecode (real import — stamps SourceAgent="claude-code").
	claudePath := filepath.Join(watchDir, "CLAUDE.md")
	require.NoError(t, os.WriteFile(claudePath, []byte("v1"), 0o644))
	ccTyped, ok := cc.(*claudecode.Adapter)
	require.True(t, ok)
	ids, err := ccTyped.ImportMemory(context.Background(), store, claudePath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	// Simulate codex writing the same artifact with different content,
	// within the conflict window. We chain on the prior head's hash to
	// satisfy AppendEvent's chain check.
	art, err := store.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	now := time.Now().UTC()
	payload, perr := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v2-from-codex"})
	require.NoError(t, perr)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: ids[0],
		Type:       acf.EventTypeUpdate,
		Timestamp:  now,
		Payload:    payload,
		Provenance: acf.Provenance{SourceAgent: "codex"},
		ParentHash: art.HeadEventHash,
	}))

	// Trigger detection via the public test wrapper.
	orch.MaybeRecordConflictForTest(cc, ids[0])

	list, err := confStore.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Len(t, list[0].Heads, 2)
	require.Equal(t, "claude-code", list[0].Heads[0].SourceAgent)
	require.Equal(t, "codex", list[0].Heads[1].SourceAgent)
}

func TestOrchestrator_IgnoresConversationConflictsWhenVisibleTurnsMatch(t *testing.T) {
	tmp := realTempDir(t)
	confStore := &conflicts.Store{Root: filepath.Join(tmp, "conflicts")}
	require.NoError(t, confStore.Init())

	adapters, store, _ := buildAllThreeAdapters(t, tmp)
	var cc adapter.Adapter
	for _, ad := range adapters {
		if ad.Name() == "claude-code" {
			cc = ad
			break
		}
	}
	require.NotNil(t, cc)

	watchDir := filepath.Join(tmp, "proj")
	require.NoError(t, os.MkdirAll(watchDir, 0o755))

	orch, err := NewOrchestrator(Config{
		Dir:            watchDir,
		Adapters:       adapters,
		Store:          store,
		QuietPeriod:    50 * time.Millisecond,
		GuardWindow:    1 * time.Second,
		ConflictStore:  confStore,
		ConflictWindow: 30 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	artifactID := acf.NewID()
	artifact := acf.Artifact{
		ArtifactID: artifactID,
		Kind:       acf.KindConversation,
		Scope:      acf.ScopeGlobal,
		Name:       "same visible thread",
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}
	require.NoError(t, store.WriteArtifact(artifact))

	now := time.Now().UTC()
	payloadA, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "what is my name?"}}},
			{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "Your name is Example User."}}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now,
		Payload:    payloadA,
		Provenance: acf.Provenance{SourceAgent: "claude-code"},
		ParentHash: "",
	}))

	art, err := store.ReadArtifact(acf.KindConversation, artifactID)
	require.NoError(t, err)
	payloadB, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "<permissions instructions>\nFilesystem sandboxing defines..."}}},
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "what is my name?"}}},
			{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "Your name is Example User."}}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now.Add(3 * time.Second),
		Payload:    payloadB,
		Provenance: acf.Provenance{SourceAgent: "codex"},
		ParentHash: art.HeadEventHash,
	}))

	orch.MaybeRecordConflictForTest(cc, artifactID)

	list, err := confStore.List()
	require.NoError(t, err)
	require.Empty(t, list, "hidden context-only conversation differences should settle without human conflict")
}

func TestOrchestrator_TreatsCrossAgentConversationDeltaAsLinearAppendAndRepairsSidecar(t *testing.T) {
	tmp := realTempDir(t)
	confStore := &conflicts.Store{Root: filepath.Join(tmp, "conflicts")}
	require.NoError(t, confStore.Init())
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	orch := &Orchestrator{cfg: Config{Store: store, ConflictStore: confStore, ConflictWindow: time.Minute}}

	artifactID := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		CreatedAt: now, UpdatedAt: now,
	}))
	full, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "answer one"}}}},
	})
	require.NoError(t, err)
	first := acf.Event{EventID: acf.NewID(), ArtifactID: artifactID, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: full, Provenance: acf.Provenance{SourceAgent: "claude-code"}}
	require.NoError(t, store.AppendEvent(acf.KindConversation, first))
	first, _, err = store.LastEvent(acf.KindConversation, artifactID)
	require.NoError(t, err)

	delta, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationDeltaFormatV1,
		Events: []acf.ConversationEvent{{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "question two"}}}},
	})
	require.NoError(t, err)
	second := acf.Event{EventID: acf.NewID(), ArtifactID: artifactID, Type: acf.EventTypeUpdate,
		Timestamp: now.Add(time.Second), Payload: delta, ParentHash: first.Hash,
		Provenance: acf.Provenance{SourceAgent: "codex"}}
	require.NoError(t, store.AppendEvent(acf.KindConversation, second))
	second, _, err = store.LastEvent(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.False(t, orch.maybeRecordConflict(nil, artifactID))
	list, err := confStore.List()
	require.NoError(t, err)
	require.Empty(t, list)

	// Simulate a sidecar written by the old detector, then add the assistant
	// answer. The next inspection must compare-delete only that exact stale pair.
	require.NoError(t, confStore.Record(conflicts.Conflict{
		ArtifactID: artifactID, Kind: acf.KindConversation,
		Heads: []conflicts.Head{conflictHeadFromEvent(first), conflictHeadFromEvent(second)},
	}))
	third := acf.Event{EventID: acf.NewID(), ArtifactID: artifactID, Type: acf.EventTypeUpdate,
		Timestamp: now.Add(2 * time.Second), Payload: delta, ParentHash: second.Hash,
		Provenance: acf.Provenance{SourceAgent: "codex"}}
	require.NoError(t, store.AppendEvent(acf.KindConversation, third))
	require.False(t, orch.maybeRecordConflict(nil, artifactID))
	list, err = confStore.List()
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestOrchestrator_TreatsCrossAgentFullProjectionAsConvergence(t *testing.T) {
	tmp := realTempDir(t)
	confStore := &conflicts.Store{Root: filepath.Join(tmp, "conflicts")}
	require.NoError(t, confStore.Init())
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	orch := &Orchestrator{cfg: Config{Store: store, ConflictStore: confStore, ConflictWindow: time.Minute}}

	artifactID := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		CreatedAt: now, UpdatedAt: now,
	}))
	turn := func(role, text string, at time.Time) acf.ConversationEvent {
		return acf.ConversationEvent{
			Type: acf.EventTypeTurn, Role: role, Timestamp: at,
			Content: []acf.ContentBlock{{Type: "text", Text: text}},
		}
	}
	appendPayload := func(source, format string, turns []acf.ConversationEvent, at time.Time) acf.Event {
		t.Helper()
		payload, err := acf.EncodePayload(acf.ConversationPayload{Format: format, Events: turns})
		require.NoError(t, err)
		art, err := store.ReadArtifact(acf.KindConversation, artifactID)
		require.NoError(t, err)
		ev := acf.Event{
			EventID: acf.NewID(), ArtifactID: artifactID, Type: acf.EventTypeUpdate,
			Timestamp: at, ParentHash: art.HeadEventHash, Payload: payload,
			Provenance: acf.Provenance{SourceAgent: source},
		}
		require.NoError(t, store.AppendEvent(acf.KindConversation, ev))
		head, ok, err := store.LastEvent(acf.KindConversation, artifactID)
		require.NoError(t, err)
		require.True(t, ok)
		return head
	}

	u1 := turn("user", "what is the size of the Earth?", now)
	a1 := turn("assistant", "About 12,742 km in diameter.", now.Add(time.Second))
	u2 := turn("user", "What is its average temperature?", now.Add(2*time.Second))
	a2 := turn("assistant", "About 15 C.", now.Add(3*time.Second))
	appendPayload("codex", acf.ConversationFormatV1, []acf.ConversationEvent{u1}, now)
	appendPayload("codex", acf.ConversationDeltaFormatV1, []acf.ConversationEvent{a1}, now.Add(time.Second))
	appendPayload("codex", acf.ConversationDeltaFormatV1, []acf.ConversationEvent{u2}, now.Add(2*time.Second))
	codexHead := appendPayload("codex", acf.ConversationDeltaFormatV1, []acf.ConversationEvent{a2}, now.Add(3*time.Second))

	// Claude re-authors the materialized base timestamps when it imports the
	// generated transcript. Visible content is unchanged and must propagate as
	// a convergence checkpoint rather than creating a conflict with Codex.
	claudeProjection := []acf.ConversationEvent{
		turn("user", u1.Content[0].Text, now.Add(10*time.Millisecond)),
		turn("assistant", a1.Content[0].Text, now.Add(time.Second+10*time.Millisecond)),
		u2, a2,
	}
	// Regress the outer event timestamp as well: canonical append order, not
	// wall-clock order, defines the history being compared.
	claudeHead := appendPayload("claude-code", acf.ConversationFormatV1, claudeProjection, now.Add(-time.Second))

	require.False(t, orch.maybeRecordConflict(nil, artifactID))
	list, err := confStore.List()
	require.NoError(t, err)
	require.Empty(t, list)

	// A sidecar written by an older daemon for this exact pair is repaired on
	// the next inspection.
	require.NoError(t, confStore.Record(conflicts.Conflict{
		ArtifactID: artifactID, Kind: acf.KindConversation,
		Heads: []conflicts.Head{conflictHeadFromEvent(codexHead), conflictHeadFromEvent(claudeHead)},
	}))
	require.False(t, orch.maybeRecordConflict(nil, artifactID))
	list, err = confStore.List()
	require.NoError(t, err)
	require.Empty(t, list)

	divergedProjection := append([]acf.ConversationEvent(nil), claudeProjection...)
	divergedProjection[len(divergedProjection)-1] = turn(
		"assistant", "A genuinely different answer.", now.Add(5*time.Second),
	)
	appendPayload("codex", acf.ConversationFormatV1, divergedProjection, now.Add(5*time.Second))
	require.True(t, orch.maybeRecordConflict(nil, artifactID))
	list, err = confStore.List()
	require.NoError(t, err)
	require.Len(t, list, 1, "a real cross-agent transcript change must still conflict")
}

func TestOrchestrator_AutoResolvesEquivalentConversationToNewestTimestamp(t *testing.T) {
	tmp := realTempDir(t)
	confStore := &conflicts.Store{Root: filepath.Join(tmp, "conflicts")}
	require.NoError(t, confStore.Init())

	adapters, store, _ := buildAllThreeAdapters(t, tmp)
	var cc adapter.Adapter
	for _, ad := range adapters {
		if ad.Name() == "claude-code" {
			cc = ad
			break
		}
	}
	require.NotNil(t, cc)

	watchDir := filepath.Join(tmp, "proj")
	require.NoError(t, os.MkdirAll(watchDir, 0o755))

	orch, err := NewOrchestrator(Config{
		Dir:            watchDir,
		Adapters:       adapters,
		Store:          store,
		QuietPeriod:    50 * time.Millisecond,
		GuardWindow:    1 * time.Second,
		ConflictStore:  confStore,
		ConflictWindow: 30 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	artifactID := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		ArtifactID: artifactID,
		Kind:       acf.KindConversation,
		Scope:      acf.ScopeGlobal,
		Name:       "newer timestamp wins",
		CreatedAt:  now,
		UpdatedAt:  now,
	}))

	newerPayload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "same visible question"}}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now.Add(3 * time.Second),
		Payload:    newerPayload,
		Provenance: acf.Provenance{SourceAgent: "claude-code"},
		ParentHash: "",
	}))

	art, err := store.ReadArtifact(acf.KindConversation, artifactID)
	require.NoError(t, err)
	olderPayload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "<environment_context>\nolder appended payload"}}},
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "same visible question"}}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now,
		Payload:    olderPayload,
		Provenance: acf.Provenance{SourceAgent: "codex"},
		ParentHash: art.HeadEventHash,
	}))

	orch.MaybeRecordConflictForTest(cc, artifactID)

	list, err := confStore.List()
	require.NoError(t, err)
	require.Empty(t, list)
	events, err := store.ReadEvents(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.Len(t, events, 3)
	require.Equal(t, acf.EventType(acf.EventTypeResolution), events[2].Type)
	require.JSONEq(t, string(newerPayload), string(events[2].Payload))
}

func TestOrchestrator_IgnoresSnapshotEventsForConflictDetection(t *testing.T) {
	tmp := realTempDir(t)
	confStore := &conflicts.Store{Root: filepath.Join(tmp, "conflicts")}
	require.NoError(t, confStore.Init())

	adapters, store, _ := buildAllThreeAdapters(t, tmp)
	var cc adapter.Adapter
	for _, ad := range adapters {
		if ad.Name() == "claude-code" {
			cc = ad
			break
		}
	}
	require.NotNil(t, cc)

	watchDir := filepath.Join(tmp, "proj")
	require.NoError(t, os.MkdirAll(watchDir, 0o755))

	orch, err := NewOrchestrator(Config{
		Dir:            watchDir,
		Adapters:       adapters,
		Store:          store,
		QuietPeriod:    50 * time.Millisecond,
		GuardWindow:    1 * time.Second,
		ConflictStore:  confStore,
		ConflictWindow: 30 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	claudePath := filepath.Join(watchDir, "CLAUDE.md")
	require.NoError(t, os.WriteFile(claudePath, []byte("v1"), 0o644))
	ccTyped, ok := cc.(*claudecode.Adapter)
	require.True(t, ok)
	ids, err := ccTyped.ImportMemory(context.Background(), store, claudePath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	art, err := store.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:       acf.NewID(),
		ArtifactID:    ids[0],
		Type:          acf.EventTypeSnapshot,
		Timestamp:     now,
		Payload:       nil,
		ParentHash:    art.HeadEventHash,
		SnapshotState: "sha256:test",
	}))

	art, err = store.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	payload, perr := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v2"})
	require.NoError(t, perr)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: ids[0],
		Type:       acf.EventTypeUpdate,
		Timestamp:  now.Add(60 * time.Millisecond),
		Payload:    payload,
		Provenance: acf.Provenance{SourceAgent: "claude-code"},
		ParentHash: art.HeadEventHash,
	}))

	orch.MaybeRecordConflictForTest(cc, ids[0])

	list, err := confStore.List()
	require.NoError(t, err)
	require.Empty(t, list, "snapshot bookkeeping events must not become conflict heads")
}

func TestOrchestrator_UnrecognizedFilename_Skipped(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// README.md is not in any adapter's dispatch table.
	require.NoError(t, os.WriteFile(filepath.Join(watched, "README.md"),
		[]byte("# readme\n"), 0o644))
	time.Sleep(400 * time.Millisecond)

	memories, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Empty(t, memories, "unrecognized filenames must not be imported")
}

// TestOrchestrator_GlobalMemory_FansOutToHermes verifies that a global memory
// edited in Claude Code's native root (~/.claude/CLAUDE.md) also reaches
// Hermes (~/.hermes/memories/) alongside the other adapters.
// Mirrors the daemon wiring: native global roots are watched via
// AdditionalRoots, hermes' own root is NOT watched (hermeswatch owns it),
// and the artifact is ScopeGlobal.
func TestOrchestrator_GlobalMemory_FansOutToHermes(t *testing.T) {
	root := realTempDir(t)
	adapters, store, _ := buildAllFiveAdapters(t, root)

	claudeRoot := filepath.Join(root, ".claude")
	require.NoError(t, os.MkdirAll(claudeRoot, 0o755))
	watched := filepath.Join(root, "watched")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	ctx, cancel := context.WithCancel(context.Background())

	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		AdditionalRoots: []string{claudeRoot},
		Adapters:        adapters,
		Store:           store,
		QuietPeriod:     100 * time.Millisecond,
		GuardWindow:     2 * time.Second,
	})
	require.NoError(t, err)
	// Cleanup runs BEFORE t.TempDir's RemoveAll (LIFO): stop the event
	// loop and let in-flight store writes drain, or RemoveAll races a
	// late write ("directory not empty" flake).
	t.Cleanup(func() {
		cancel()
		_ = orch.Close()
		time.Sleep(500 * time.Millisecond)
	})

	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(claudeRoot, "CLAUDE.md"),
		[]byte("# global memory from claude\n"), 0o644))

	hermesDest := filepath.Join(root, ".hermes", "memories", "MEMORY.md")
	// Generous timeout for the debounce and fan-out chain under full-suite load.
	require.Eventually(t, func() bool {
		_, serr := os.Stat(hermesDest)
		return serr == nil
	}, 15*time.Second, 100*time.Millisecond,
		"global memory must fan out to hermes' native memories dir; adapter errors: %v",
		orch.AdapterLastErrors())

	got, rerr := os.ReadFile(hermesDest)
	require.NoError(t, rerr)
	require.Equal(t, "# global memory from claude\n", string(got))
}

// TestOrchestrator_HermesMemoriesRoot_ImportAndFanOut covers the E2E F2
// finding: ~/.hermes is excluded from the generic native-root watcher
// (hermeswatch owns the conversation DB), which silently made hermes
// memory sync EXPORT-ONLY — a memory written by hermes into
// ~/.hermes/memories/ never reached any other agent. The daemon now
// watches the memories subdir as a flat root with hermes registered as
// its path owner; this test locks the orchestrator-level behavior.
func TestOrchestrator_HermesMemoriesRoot_ImportAndFanOut(t *testing.T) {
	root := realTempDir(t)
	adapters, store, _ := buildAllFiveAdapters(t, root)

	hermesMemories := filepath.Join(root, ".hermes", "memories")
	require.NoError(t, os.MkdirAll(hermesMemories, 0o755))
	watched := filepath.Join(root, "watched")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	ctx, cancel := context.WithCancel(context.Background())

	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		AdditionalRoots: []string{hermesMemories},
		RootsByAdapter:  map[string][]string{"hermes": {hermesMemories}},
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

	require.NoError(t, os.WriteFile(filepath.Join(hermesMemories, "MEMORY.md"),
		[]byte("- hermes remembers Neptune\n"), 0o644))

	// The memory must escape hermes: claude-code's global view is
	// HomeDir/.claude/CLAUDE.md (its adapter folds global memories there).
	claudeDest := filepath.Join(root, ".claude", "CLAUDE.md")
	require.Eventually(t, func() bool {
		data, rerr := os.ReadFile(claudeDest)
		return rerr == nil && strings.Contains(string(data), "Neptune")
	}, 15*time.Second, 100*time.Millisecond,
		"hermes-sourced memory must fan out to claude-code; adapter errors: %v",
		orch.AdapterLastErrors())

	// Attribution: the artifact must be hermes-sourced (path ownership),
	// not mis-attributed via basename collision.
	memories, lerr := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, lerr)
	found := false
	for _, art := range memories {
		events, _ := store.ReadEvents(acf.KindMemory, art.ArtifactID)
		for _, e := range events {
			if e.Provenance.SourceAgent == "hermes" {
				found = true
			}
		}
	}
	require.True(t, found, "the import event must attribute to hermes")
}

func memoryEventCount(t *testing.T, store *acf.Store) int {
	t.Helper()
	memories, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	total := 0
	for _, art := range memories {
		events, rerr := store.ReadEvents(acf.KindMemory, art.ArtifactID)
		require.NoError(t, rerr)
		total += len(events)
	}
	return total
}

func TestOrchestrator_GlobalMemoryNativeScanConverges(t *testing.T) {
	root := realTempDir(t)
	adapters, store, _ := buildAllThreeAdapters(t, root)
	var kiloCounter *exportCountingAdapter
	for i, ad := range adapters {
		if ad.Name() == "kilo" {
			kiloCounter = &exportCountingAdapter{Adapter: ad}
			adapters[i] = kiloCounter
		}
	}
	require.NotNil(t, kiloCounter)

	claudeRoot := filepath.Join(root, ".claude")
	claudeProjects := filepath.Join(claudeRoot, "projects")
	claudeMemory := filepath.Join(claudeProjects, "home", "memory")
	codexRoot := filepath.Join(root, ".codex")
	codexMemories := filepath.Join(codexRoot, "memories")
	kiloRoot := filepath.Join(root, ".config", "kilo")
	for _, dir := range []string{claudeMemory, codexMemories, kiloRoot} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	shared := "# Shared instructions\n"
	claudeFacts := "# Personal memory\n- Example User's dog's name is Comet.\n- Example User's son's name is Jordan.\n"
	codexFacts := "Example User has two dogs: Comet and Nova.\nExample User lives in Example City, Example Region.\n"
	require.NoError(t, os.WriteFile(filepath.Join(claudeRoot, "CLAUDE.md"), []byte(shared+"\n"+claudeFacts), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(claudeMemory, "personal.md"), []byte("---\ntype: user\n---\n\n"+codexFacts), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(codexRoot, "AGENTS.md"), []byte(shared+"\n"+codexFacts), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(codexMemories, "personal.md"), []byte(claudeFacts), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(kiloRoot, "AGENTS.md"), []byte(shared+"\n"+claudeFacts+"\n"+codexFacts), 0o644))

	watched := filepath.Join(root, "watched")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		AdditionalRoots: []string{claudeRoot, codexRoot, codexMemories, kiloRoot},
		RecursiveRoots:  []string{claudeProjects},
		RootsByAdapter: map[string][]string{
			"claude-code": {claudeRoot, claudeProjects},
			"codex":       {codexRoot, codexMemories},
			"kilo":        {kiloRoot},
		},
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	ctx := context.Background()
	require.NoError(t, orch.InitialScan(ctx))
	for i := 0; i < 3; i++ {
		require.NoError(t, orch.scanNativeRoots(ctx))
	}
	settled := memoryEventCount(t, store)
	settledExports := kiloCounter.exports
	require.NoError(t, orch.scanNativeRoots(ctx))
	require.Equal(t, settled, memoryEventCount(t, store),
		"native safety scans must not keep appending global-memory events once files are byte-stable")
	require.Equal(t, settledExports, kiloCounter.exports,
		"unchanged managed-memory scans must not fan out stale global-memory heads")
}
