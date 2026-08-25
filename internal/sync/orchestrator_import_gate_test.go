package syncd

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/stretchr/testify/require"
)

type gatedProbeAdapter struct {
	adapter.Adapter
	entered chan string
	release chan struct{}
}

func (a *gatedProbeAdapter) Import(ctx context.Context, store *acf.Store, path string) ([]string, error) {
	a.entered <- path
	<-a.release
	return nil, nil
}

type pathGatedAdapter struct {
	adapter.Adapter
	blockedPath string
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
}

func (a *pathGatedAdapter) Import(ctx context.Context, store *acf.Store, path string) ([]string, error) {
	if path == a.blockedPath {
		a.once.Do(func() { close(a.entered) })
		select {
		case <-a.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return a.Adapter.Import(ctx, store, path)
}

type notifyingConversationTarget struct {
	*recordingConversationTarget
	materialized chan struct{}
	once         sync.Once
}

func (t *notifyingConversationTarget) MaterializeConversationSession(art acf.Artifact, head acf.Event, sourceAgent string) (string, bool, error) {
	path, materialized, err := t.recordingConversationTarget.MaterializeConversationSession(art, head, sourceAgent)
	if err == nil && materialized {
		t.once.Do(func() { close(t.materialized) })
	}
	return path, materialized, err
}

func TestHandleEvent_BoundsLargeDifferentPathImportsGlobally(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	base := codex.New()
	base.HomeDir = root
	base.CanonicalConversations = true
	probe := &gatedProbeAdapter{Adapter: base, entered: make(chan string, 2), release: make(chan struct{})}
	orch, err := NewOrchestrator(Config{Dir: root, Store: store, Adapters: []adapter.Adapter{probe}})
	require.NoError(t, err)
	defer orch.Close()

	dir := filepath.Join(root, ".codex", "sessions", "2026", "07", "19")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	paths := []string{filepath.Join(dir, "one.jsonl"), filepath.Join(dir, "two.jsonl")}
	for _, path := range paths {
		require.NoError(t, os.WriteFile(path, []byte(raceSessionUserLine), 0o600))
		require.NoError(t, os.Truncate(path, maxFastLaneImportBytes+1))
	}

	var wg sync.WaitGroup
	for _, path := range paths {
		wg.Add(1)
		go func(p string) { defer wg.Done(); orch.handleEvent(p) }(path)
	}
	select {
	case <-probe.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first import did not start")
	}
	select {
	case second := <-probe.entered:
		t.Fatalf("second path entered adapter before first completed: %s", second)
	case <-time.After(150 * time.Millisecond):
	}
	close(probe.release)
	wg.Wait()
}

func TestHandleEvent_SmallFastLaneBypassesLargeImportBacklog(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	base := codex.New()
	base.HomeDir = root
	base.CanonicalConversations = true
	probe := &gatedProbeAdapter{Adapter: base, entered: make(chan string, 3), release: make(chan struct{})}
	orch, err := NewOrchestrator(Config{Dir: root, Store: store, Adapters: []adapter.Adapter{probe}})
	require.NoError(t, err)
	defer orch.Close()

	dir := filepath.Join(root, ".codex", "sessions", "2026", "07", "19")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	largeOne := filepath.Join(dir, "large-one.jsonl")
	largeTwo := filepath.Join(dir, "large-two.jsonl")
	small := filepath.Join(dir, "small.jsonl")
	for _, path := range []string{largeOne, largeTwo, small} {
		require.NoError(t, os.WriteFile(path, []byte(raceSessionUserLine), 0o600))
	}
	require.NoError(t, os.Truncate(largeOne, maxFastLaneImportBytes+1))
	require.NoError(t, os.Truncate(largeTwo, maxFastLaneImportBytes+1))

	var wg sync.WaitGroup
	start := func(path string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			orch.handleEvent(path)
		}()
	}
	start(largeOne)
	select {
	case entered := <-probe.entered:
		require.Equal(t, largeOne, entered)
	case <-time.After(3 * time.Second):
		t.Fatal("first large import did not start")
	}

	// Queue another large import first. It must wait for largeOne without
	// consuming the second total slot, which remains available to the small
	// interactive transcript.
	start(largeTwo)
	start(small)
	select {
	case entered := <-probe.entered:
		require.Equal(t, small, entered)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("small import was starved behind a large-import backlog")
	}
	select {
	case entered := <-probe.entered:
		t.Fatalf("second large import entered while first remained active: %s", entered)
	case <-time.After(150 * time.Millisecond):
	}

	close(probe.release)
	wg.Wait()
}

func TestRecentCodexSessionDayScan_DoesNotSerializeSmallBehindLarge(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	base := codex.New()
	base.HomeDir = root
	base.CanonicalConversations = true
	probe := &gatedProbeAdapter{Adapter: base, entered: make(chan string, 2), release: make(chan struct{})}

	now := time.Now()
	sessionsRoot := filepath.Join(root, ".codex", "sessions")
	dir := filepath.Join(sessionsRoot, now.Format("2006"), now.Format("01"), now.Format("02"))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	large := filepath.Join(dir, "rollout-a-large.jsonl")
	small := filepath.Join(dir, "rollout-z-small.jsonl")
	ready := []byte(`{"timestamp":"2026-07-19T12:00:00Z","type":"session_meta","payload":{"id":"scan-fast"}}` + "\n" +
		`{"timestamp":"2026-07-19T12:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}}` + "\n" +
		`{"timestamp":"2026-07-19T12:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"answer"}]}}` + "\n")
	require.NoError(t, os.WriteFile(large, ready, 0o600))
	require.NoError(t, os.Truncate(large, maxFastLaneImportBytes+1))
	require.NoError(t, os.WriteFile(small, ready, 0o600))

	orch, err := NewOrchestrator(Config{
		Dir:            root,
		RecursiveRoots: []string{sessionsRoot},
		RootsByAdapter: map[string][]string{"codex": {sessionsRoot}},
		Adapters:       []adapter.Adapter{probe},
		Store:          store,
	})
	require.NoError(t, err)
	defer orch.Close()
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(probe.release) }) })

	done := make(chan int, 1)
	go func() { done <- orch.scanRecentCodexSessionDays(sessionsRoot, now) }()
	seen := make(map[string]bool, 2)
	deadline := time.NewTimer(750 * time.Millisecond)
	defer deadline.Stop()
	for len(seen) < 2 {
		select {
		case path := <-probe.entered:
			seen[path] = true
		case <-deadline.C:
			t.Fatalf("scanner serialized candidates; entered before timeout: %v", seen)
		}
	}
	require.True(t, seen[large])
	require.True(t, seen[small])
	releaseOnce.Do(func() { close(probe.release) })
	select {
	case processed := <-done:
		require.Equal(t, 2, processed)
	case <-time.After(3 * time.Second):
		t.Fatal("Codex scanner did not finish after imports were released")
	}
}

func TestScanRecentCodexSessions_LaterSmallRolloutBypassesBlockedLargeTick(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	now := time.Now()
	sessionsRoot := filepath.Join(root, ".codex", "sessions")
	dir := filepath.Join(sessionsRoot, now.Format("2006"), now.Format("01"), now.Format("02"))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	large := filepath.Join(dir, "rollout-large-blocked.jsonl")
	small := filepath.Join(dir, "rollout-small-later.jsonl")
	rollout := func(id, prompt, answer string) []byte {
		return []byte(`{"timestamp":"2026-07-20T12:00:00Z","type":"session_meta","payload":{"id":"` + id + `"}}` + "\n" +
			`{"timestamp":"2026-07-20T12:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + prompt + `"}]}}` + "\n" +
			`{"timestamp":"2026-07-20T12:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"` + answer + `"}]}}` + "\n")
	}
	require.NoError(t, os.WriteFile(large, rollout("large", "large prompt", "large answer"), 0o600))
	require.NoError(t, os.Truncate(large, maxFastLaneImportBytes+1))

	base := codex.New()
	base.HomeDir = root
	base.CanonicalConversations = true
	blocked := &pathGatedAdapter{
		Adapter: base, blockedPath: large, entered: make(chan struct{}), release: make(chan struct{}),
	}
	target := &notifyingConversationTarget{
		recordingConversationTarget: &recordingConversationTarget{
			fakeConvSource: fakeConvSource{name: "claude-code"},
			dest:           filepath.Join(root, ".claude", "projects", "-test", "materialized.jsonl"),
		},
		materialized: make(chan struct{}),
	}
	orch, err := NewOrchestrator(Config{
		Dir:            root,
		RecursiveRoots: []string{sessionsRoot},
		RootsByAdapter: map[string][]string{"codex": {sessionsRoot}},
		Adapters:       []adapter.Adapter{blocked, target},
		Store:          store,
	})
	require.NoError(t, err)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocked.release) }) }
	t.Cleanup(func() {
		release()
		require.NoError(t, orch.Close())
	})

	ctx := context.Background()
	require.Equal(t, 1, orch.ScanRecentCodexSessions(ctx), "first tick must dispatch the large candidate")
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("large Codex import did not reach the deterministic gate")
	}

	require.NoError(t, os.WriteFile(small, rollout("small", "bounded prompt", "bounded answer"), 0o600))
	newer := now.Add(time.Second)
	require.NoError(t, os.Chtimes(small, newer, newer))

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tickDone := make(chan int, 1)
	go func() { tickDone <- orch.ScanRecentCodexSessions(ctx) }()
	select {
	case dispatched := <-tickDone:
		require.Equal(t, 1, dispatched, "second tick must dispatch only the later small rollout")
	case <-deadline.C:
		t.Fatal("later small rollout waited behind the blocked large scheduler")
	}
	select {
	case <-target.materialized:
	case <-deadline.C:
		t.Fatal("later small rollout did not materialize before the bounded deadline")
	}
	_, turns := target.snapshot()
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "bounded prompt"},
		{Role: "assistant", Text: "bounded answer"},
	}, turns, "later small Codex rollout must materialize while the large import remains blocked")
}
