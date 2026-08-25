package syncd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

func (c *capturingBus) count(kind string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, k := range c.kinds {
		if k == kind {
			n++
		}
	}
	return n
}

// The 5s native scan re-probes an unchanged oversized file every tick; the
// artifact.refused bus event must surface once per report interval, not once
// per tick (two growing session files flooded the UI event stream with a
// refusal pair every 5 seconds).
func TestHandleEvent_OversizeRefusalThrottled(t *testing.T) {
	old := oversizeReportInterval
	oversizeReportInterval = time.Hour
	t.Cleanup(func() { oversizeReportInterval = old })

	root := t.TempDir()
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	bus := &capturingBus{}
	orch, err := NewOrchestrator(Config{
		Dir:              root,
		Adapters:         []adapter.Adapter{&fakeConvSource{name: "src"}},
		Store:            store,
		MaxArtifactBytes: 1 << 20, // 1 MiB cap keeps the test file small
		EventPublisher:   bus,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })

	p := filepath.Join(root, "huge.jsonl")
	require.NoError(t, os.WriteFile(p, make([]byte, 2<<20), 0o644))

	require.True(t, orch.handleEvent(p))
	require.True(t, orch.handleEvent(p))
	require.True(t, orch.handleEvent(p))
	require.Equal(t, 1, bus.count("artifact.refused"),
		"repeat refusals within the interval must not re-publish")

	// After the interval elapses, the refusal re-surfaces (still refused).
	oversizeReportInterval = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	require.True(t, orch.handleEvent(p))
	require.Equal(t, 2, bus.count("artifact.refused"))

	// Passing the size gate clears the throttle state, so a later regrowth
	// re-reports promptly even within a long interval.
	oversizeReportInterval = time.Hour
	require.NoError(t, os.WriteFile(p, []byte("small"), 0o644))
	orch.handleEvent(p) // under the cap: gate passes, throttle state clears
	require.NoError(t, os.WriteFile(p, make([]byte, 2<<20), 0o644))
	require.True(t, orch.handleEvent(p))
	require.Equal(t, 3, bus.count("artifact.refused"))
}

// writeSparseFile creates path with the given LOGICAL size without allocating
// the bytes (Truncate produces a sparse file). The size gates under test stat
// the file, so only the reported size matters — this keeps the 100MB-session
// tests fast and disk-cheap.
func writeSparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())
}

// newSessionCapOrch builds a minimal orchestrator + capturing bus for the
// session-cap gate tests.
func newSessionCapOrch(t *testing.T, root string, cfg Config) (*Orchestrator, *capturingBus) {
	t.Helper()
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	bus := &capturingBus{}
	cfg.Dir = root
	cfg.Adapters = []adapter.Adapter{&fakeConvSource{name: "src"}}
	cfg.Store = store
	cfg.EventPublisher = bus
	orch, err := NewOrchestrator(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	return orch, bus
}

// Aligned-chains coverage: agent session transcripts (Claude Code
// ~/.claude/projects/**.jsonl, Codex ~/.codex/sessions/** rollouts) get their
// own, much larger ingest cap (limits.max_session_file_mb, default 512 MB) —
// multi-week sessions legitimately outgrow the generic 64 MiB artifact cap
// and, with aligned chains, replicate as per-turn deltas. Everything else
// keeps the generic cap.
func TestHandleEvent_SessionFilesUseSessionCap(t *testing.T) {
	root := t.TempDir()
	orch, bus := newSessionCapOrch(t, root, Config{})

	// A 100 MB Claude transcript: over the generic 64 MiB cap, comfortably
	// under the 512 MB session default → admitted (the gate falls through to
	// import; NO refusal event).
	claude := filepath.Join(root, ".claude", "projects", "-Users-x-proj", "sess.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(claude), 0o755))
	writeSparseFile(t, claude, 100<<20)
	orch.handleEvent(claude)
	require.Zero(t, bus.count("artifact.refused"),
		"a 100MB Claude session transcript must pass the session-size gate")

	// Codex rollouts share the session cap.
	codex := filepath.Join(root, ".codex", "sessions", "2026", "07", "02", "rollout-x.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(codex), 0o755))
	writeSparseFile(t, codex, 100<<20)
	orch.handleEvent(codex)
	require.Zero(t, bus.count("artifact.refused"),
		"a 100MB Codex rollout must pass the session-size gate")

	// A generic 65 MiB markdown blob keeps the 64 MiB artifact cap.
	md := filepath.Join(root, "notes.md")
	writeSparseFile(t, md, 65<<20)
	require.True(t, orch.handleEvent(md), "oversize refusal is terminal")
	require.Equal(t, 1, bus.count("artifact.refused"),
		"a 65MiB generic file must still refuse at the 64MiB artifact cap")
}

// The session cap is a real cap, not a bypass: a configured
// Config.MaxSessionFileBytes refuses session files above it, and a negative
// value disables the session gate without loosening the generic cap.
func TestHandleEvent_SessionCapConfigurableAndDisabled(t *testing.T) {
	rootA := t.TempDir()
	orchA, busA := newSessionCapOrch(t, rootA, Config{
		MaxSessionFileBytes: 1 << 20, // 1 MiB session cap
	})
	sessA := filepath.Join(rootA, ".claude", "projects", "-Users-x", "s.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sessA), 0o755))
	writeSparseFile(t, sessA, 2<<20)
	require.True(t, orchA.handleEvent(sessA), "oversize refusal is terminal")
	require.Equal(t, 1, busA.count("artifact.refused"),
		"a session file above the configured session cap must refuse")

	rootB := t.TempDir()
	orchB, busB := newSessionCapOrch(t, rootB, Config{
		MaxSessionFileBytes: -1,      // session cap disabled
		MaxArtifactBytes:    1 << 20, // tight generic cap
	})
	sessB := filepath.Join(rootB, ".claude", "projects", "-Users-x", "s.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sessB), 0o755))
	writeSparseFile(t, sessB, 100<<20)
	orchB.handleEvent(sessB)
	require.Zero(t, busB.count("artifact.refused"),
		"-1 must disable the session cap for session paths")
	md := filepath.Join(rootB, "notes.md")
	writeSparseFile(t, md, 2<<20)
	require.True(t, orchB.handleEvent(md))
	require.Equal(t, 1, busB.count("artifact.refused"),
		"the generic cap must keep gating non-session files")
}

// The hot 500ms Claude poll set keeps its own 64MiB bound
// (maxRecentClaudeSessionBytes), but the effective hot-set limit is
// min(hot bound, session cap): a session cap BELOW the hot bound must also
// drop candidates so the poller never respins on a file the ingest gate is
// going to refuse — and a session cap ABOVE it must not loosen the hot bound
// (files that big belong to the watcher + 5s native scan, not the hot poll).
func TestRecentClaudeSessionCandidates_SessionCapBoundsHotSet(t *testing.T) {
	hot := func(t *testing.T, orch *Orchestrator, path string) []scanFileCandidate {
		t.Helper()
		old := time.Now().Add(-2 * time.Second)
		require.NoError(t, os.Chtimes(path, old, old))
		orch.markClaudeHotSession(path)
		orch.recentClaudeScanMu.Lock()
		defer orch.recentClaudeScanMu.Unlock()
		return orch.recentClaudeSessionCandidatesLocked(time.Now())
	}

	rootA := realTempDir(t)
	orchA, _ := newSessionCapOrch(t, rootA, Config{
		RecentClaudeSessionWindow: 15 * time.Minute,
		MaxSessionFileBytes:       1 << 10, // 1 KiB session cap
	})
	dirA := filepath.Join(rootA, ".claude", "projects", "-Users-x")
	require.NoError(t, os.MkdirAll(dirA, 0o755))
	pA := filepath.Join(dirA, "big.jsonl")
	require.NoError(t, os.WriteFile(pA, make([]byte, 2<<10), 0o644))
	require.Empty(t, hot(t, orchA, pA),
		"a hot session above the session cap must drop from the poll set")

	rootB := realTempDir(t)
	orchB, _ := newSessionCapOrch(t, rootB, Config{
		RecentClaudeSessionWindow: 15 * time.Minute, // default 512MB session cap
	})
	dirB := filepath.Join(rootB, ".claude", "projects", "-Users-x")
	require.NoError(t, os.MkdirAll(dirB, 0o755))
	pB := filepath.Join(dirB, "big.jsonl")
	writeSparseFile(t, pB, 65<<20) // over the 64MiB hot bound, under the session cap
	require.Empty(t, hot(t, orchB, pB),
		"the hot-set's own 64MiB bound must survive a large session cap")
}
