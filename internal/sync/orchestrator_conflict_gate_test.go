package syncd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/stretchr/testify/require"
)

// stageDivergentCodexHead appends a codex-authored event with content distinct
// from (and not semantically equivalent to) the current head, within the
// conflict window, so the NEXT claude-code import of the same memory artifact
// produces a genuine divergence (claude-code head vs codex head).
func stageDivergentCodexHead(t *testing.T, store *acf.Store, artifactID, content string) {
	t.Helper()
	art, err := store.ReadArtifact(acf.KindMemory, artifactID)
	require.NoError(t, err)
	payload, perr := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: content})
	require.NoError(t, perr)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  time.Now().UTC(),
		Payload:    payload,
		Provenance: acf.Provenance{SourceAgent: "codex"},
		ParentHash: art.HeadEventHash,
	}))
}

// TestHandleEvent_DivergenceBlocksFanOut is the divergence-gate test (BRD-03 §4.6 /
// §10 OQ-03.2): a detected divergence must record a conflict AND withhold
// propagation of the divergent head to other agents until the user resolves it.
// The freshly-imported event must STILL be present in the local event log (the
// local edit is preserved as a branch head; only propagation is withheld).
func TestHandleEvent_DivergenceBlocksFanOut(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	confStore := &conflicts.Store{Root: filepath.Join(root, "conflicts")}
	require.NoError(t, confStore.Init())

	adapters, store, _ := buildAllThreeAdapters(t, root)
	// Wrap every adapter in an Export counter so we can prove fan-out did NOT
	// propagate the divergent artifact to any of them.
	counters := make([]*exportCountingAdapter, 0, len(adapters))
	wrapped := make([]adapter.Adapter, 0, len(adapters))
	var cc adapter.Adapter
	for _, ad := range adapters {
		c := &exportCountingAdapter{Adapter: ad}
		counters = append(counters, c)
		wrapped = append(wrapped, c)
		if ad.Name() == "claude-code" {
			cc = ad
		}
	}
	require.NotNil(t, cc)

	orch, err := NewOrchestrator(Config{
		Dir:            watched,
		Adapters:       wrapped,
		Store:          store,
		QuietPeriod:    50 * time.Millisecond,
		GuardWindow:    1 * time.Second,
		ConflictStore:  confStore,
		ConflictWindow: 30 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	// First write via claudecode (real import — stamps SourceAgent=claude-code).
	claudePath := filepath.Join(watched, "CLAUDE.md")
	require.NoError(t, os.WriteFile(claudePath, []byte("v1"), 0o644))
	ccTyped := cc.(*claudecode.Adapter)
	ids, err := ccTyped.ImportMemory(context.Background(), store, claudePath)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	artifactID := ids[0]

	// Stage a divergent codex head, then write divergent content to CLAUDE.md so
	// the claude-code re-import inside handleEvent becomes the divergent head.
	stageDivergentCodexHead(t, store, artifactID, "v2-from-codex")
	require.NoError(t, os.WriteFile(claudePath, []byte("v3-from-claude"), 0o644))

	// Reset counters: only fan-outs from the divergent handleEvent below count.
	for _, c := range counters {
		c.exports = 0
	}

	orch.handleEvent(claudePath)

	// (a) a conflict was recorded.
	list, err := confStore.List()
	require.NoError(t, err)
	require.Len(t, list, 1, "a divergence must record exactly one conflict")
	require.Equal(t, artifactID, list[0].ArtifactID)

	// (b) fanOut did NOT propagate the divergent artifact to any adapter.
	for _, c := range counters {
		require.Zero(t, c.exports,
			"no adapter must receive a fan-out of a divergent (unresolved) head: %s exported %d times", c.Name(), c.exports)
	}

	// (c) the freshly-imported event is STILL present in the local event log
	// (the local edit is preserved as a branch head; only propagation withheld).
	events, err := store.ReadEvents(acf.KindMemory, artifactID)
	require.NoError(t, err)
	var sawClaudeV3 bool
	for _, e := range events {
		if e.Provenance.SourceAgent != "claude-code" {
			continue
		}
		if mp, derr := acf.DecodeMemoryPayload(e); derr == nil && mp.Content == "v3-from-claude" {
			sawClaudeV3 = true
		}
	}
	require.True(t, sawClaudeV3,
		"the divergent local edit must remain committed to the immutable event log")

	// And inUnresolvedConflict must report the artifact as blocked across the
	// restart-robust check path.
	require.True(t, orch.inUnresolvedConflict(artifactID))
}

// TestHandleEvent_NonDivergentStillFansOut is the no-regression test: an
// ordinary single-author edit (no divergence) must still fan out normally.
func TestHandleEvent_NonDivergentStillFansOut(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	confStore := &conflicts.Store{Root: filepath.Join(root, "conflicts")}
	require.NoError(t, confStore.Init())

	adapters, store, _ := buildAllThreeAdapters(t, root)
	counters := make([]*exportCountingAdapter, 0, len(adapters))
	wrapped := make([]adapter.Adapter, 0, len(adapters))
	var cc adapter.Adapter
	for _, ad := range adapters {
		c := &exportCountingAdapter{Adapter: ad}
		counters = append(counters, c)
		wrapped = append(wrapped, c)
		if ad.Name() == "claude-code" {
			cc = ad
		}
	}
	require.NotNil(t, cc)

	orch, err := NewOrchestrator(Config{
		Dir:            watched,
		Adapters:       wrapped,
		Store:          store,
		QuietPeriod:    50 * time.Millisecond,
		GuardWindow:    1 * time.Second,
		ConflictStore:  confStore,
		ConflictWindow: 30 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	claudePath := filepath.Join(watched, "CLAUDE.md")
	require.NoError(t, os.WriteFile(claudePath, []byte("# single author edit\n"), 0o644))

	orch.handleEvent(claudePath)

	// No conflict recorded.
	list, err := confStore.List()
	require.NoError(t, err)
	require.Empty(t, list, "a single-author edit must not be a conflict")

	// The non-primary adapters received the fan-out (claude-code is primary and
	// is skipped). Sum exports across the wrapped non-primary adapters.
	total := 0
	for _, c := range counters {
		if c.Name() == "claude-code" {
			continue
		}
		total += c.exports
	}
	require.Positive(t, total,
		"a non-divergent edit must still fan out to the other agents")
}

// TestRefanOutAll_SkipsUnresolvedConflict closes the backfill/re-fanout bypass
// of the divergence gate. The live handleEvent path filters blocked ids, but
// RefanOutAll, per-project re-fanout, conversation backfill, and inbound
// materialization all call fanOut directly — so the gate lives INSIDE fanOut
// (the single propagation chokepoint). Without it, enabling a new fan-out
// target (RefanOutAll) would leak an unresolved divergent head, re-opening the
// BRD-03 §4.6 hole. This also asserts propagation RESUMES once the conflict is
// cleared (the building block for resolve-time re-propagation).
func TestRefanOutAll_SkipsUnresolvedConflict(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	confStore := &conflicts.Store{Root: filepath.Join(root, "conflicts")}
	require.NoError(t, confStore.Init())

	adapters, store, _ := buildAllThreeAdapters(t, root)
	counters := make([]*exportCountingAdapter, 0, len(adapters))
	wrapped := make([]adapter.Adapter, 0, len(adapters))
	var cc adapter.Adapter
	for _, ad := range adapters {
		c := &exportCountingAdapter{Adapter: ad}
		counters = append(counters, c)
		wrapped = append(wrapped, c)
		if ad.Name() == "claude-code" {
			cc = ad
		}
	}
	require.NotNil(t, cc)

	orch, err := NewOrchestrator(Config{
		Dir:            watched,
		Adapters:       wrapped,
		Store:          store,
		QuietPeriod:    50 * time.Millisecond,
		GuardWindow:    1 * time.Second,
		ConflictStore:  confStore,
		ConflictWindow: 30 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	// Import a memory artifact, then drive a genuine divergence through
	// handleEvent so a conflict is recorded for it.
	claudePath := filepath.Join(watched, "CLAUDE.md")
	require.NoError(t, os.WriteFile(claudePath, []byte("v1"), 0o644))
	ccTyped := cc.(*claudecode.Adapter)
	ids, err := ccTyped.ImportMemory(context.Background(), store, claudePath)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	artifactID := ids[0]

	stageDivergentCodexHead(t, store, artifactID, "v2-from-codex")
	require.NoError(t, os.WriteFile(claudePath, []byte("v3-from-claude"), 0o644))
	orch.handleEvent(claudePath)

	list, err := confStore.List()
	require.NoError(t, err)
	require.Len(t, list, 1, "divergence must record a conflict")
	require.True(t, orch.inUnresolvedConflict(artifactID))

	// RefanOutAll must NOT propagate the conflicted artifact (the bypass this
	// test closes): every adapter's Export count must stay zero.
	for _, c := range counters {
		c.exports = 0
	}
	_, err = orch.RefanOutAll(context.Background())
	require.NoError(t, err)
	for _, c := range counters {
		require.Zero(t, c.exports,
			"RefanOutAll must not propagate an unresolved-conflict artifact: %s exported %d times", c.Name(), c.exports)
	}

	// Once the conflict is resolved (Cleared), the gate releases and the
	// winning head propagates again.
	require.NoError(t, confStore.Clear(artifactID))
	require.False(t, orch.inUnresolvedConflict(artifactID))
	for _, c := range counters {
		c.exports = 0
	}
	_, err = orch.RefanOutAll(context.Background())
	require.NoError(t, err)
	total := 0
	for _, c := range counters {
		total += c.exports
	}
	require.Positive(t, total,
		"once the conflict is cleared, RefanOutAll must propagate the artifact again")
}
