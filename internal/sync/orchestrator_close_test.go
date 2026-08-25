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
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

// blockingImportAdapter wraps a real adapter and blocks its first Import call
// until release is closed, signalling entered when the import starts. It lets
// lifecycle tests hold an orchestrator scan mid-import deterministically.
type blockingImportAdapter struct {
	adapter.Adapter
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingImportAdapter) Import(ctx context.Context, store *acf.Store, path string) ([]string, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.Adapter.Import(ctx, store, path)
}

// TestClose_JoinsInFlightNativeLiveScanImport is the regression test for the
// intermittent macOS failure of TestNativeLiveScan_ImportsFlatAndRecursiveAgentRoots
// ("TempDir RemoveAll cleanup: ... directory not empty"): Close returned while
// a RunNativeLiveScan tick was still mid-import, so the scan goroutine kept
// writing under the test's temp root while t.TempDir's RemoveAll deleted it.
// Close must JOIN in-flight scan work before returning.
func TestClose_JoinsInFlightNativeLiveScanImport(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	codexRoot := filepath.Join(root, ".codex")
	require.NoError(t, os.MkdirAll(codexRoot, 0o755))

	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(root, "secrets")}
	require.NoError(t, ss.Init())

	cx := codex.New()
	cx.HomeDir = root
	cx.SecretsStore = ss
	blocking := &blockingImportAdapter{
		Adapter: cx,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	releaseOnce := func() {
		select {
		case <-blocking.release:
		default:
			close(blocking.release)
		}
	}

	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		AdditionalRoots: []string{codexRoot},
		Adapters:        []adapter.Adapter{blocking},
		Store:           store,
		QuietPeriod:     20 * time.Millisecond,
		GuardWindow:     2 * time.Second,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		releaseOnce()
		cancel()
		_ = orch.Close()
	})

	go orch.RunNativeLiveScan(ctx, 20*time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(codexRoot, "AGENTS.md"),
		[]byte("# native flat memory\n"), 0o644))

	select {
	case <-blocking.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("native live scan never reached the adapter Import")
	}

	closed := make(chan struct{})
	go func() {
		_ = orch.Close()
		close(closed)
	}()

	select {
	case <-closed:
		t.Fatal("Close returned while a native live-scan import was still in flight; Close must join scan goroutines")
	case <-time.After(150 * time.Millisecond):
		// Close is (correctly) still waiting on the blocked import.
	}

	releaseOnce()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the in-flight import finished")
	}
}

// TestClose_JoinsInFlightLiveScanImport covers the broader runLiveScan entry
// point. It is invoked directly by tests/embedders as well as from Run, so it
// must own its lifecycle registration rather than relying on its caller.
func TestClose_JoinsInFlightLiveScanImport(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	codexRoot := filepath.Join(root, ".codex")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	require.NoError(t, os.MkdirAll(codexRoot, 0o755))

	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(root, "secrets")}
	require.NoError(t, ss.Init())
	cx := codex.New()
	cx.HomeDir = root
	cx.SecretsStore = ss
	blocking := &blockingImportAdapter{
		Adapter: cx,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	releaseOnce := func() {
		select {
		case <-blocking.release:
		default:
			close(blocking.release)
		}
	}

	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		AdditionalRoots: []string{codexRoot},
		Adapters:        []adapter.Adapter{blocking},
		Store:           store,
		QuietPeriod:     20 * time.Millisecond,
		GuardWindow:     2 * time.Second,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		releaseOnce()
		cancel()
		_ = orch.Close()
	})

	go orch.runLiveScan(ctx, 20*time.Millisecond)
	require.NoError(t, os.WriteFile(filepath.Join(codexRoot, "AGENTS.md"), []byte("# live scan\n"), 0o644))
	select {
	case <-blocking.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("live scan never reached the adapter Import")
	}

	closed := make(chan struct{})
	go func() {
		_ = orch.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while a live-scan import was still in flight")
	case <-time.After(150 * time.Millisecond):
	}
	releaseOnce()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join the in-flight live scan")
	}
}

// TestClose_StopsNativeLiveScanLoop: Close alone (without the caller's ctx
// being cancelled) must terminate the RunNativeLiveScan loop. The daemon's
// shutdown ordering happens to cancel ctx first, but Close's contract cannot
// depend on that — tests (and any embedder) may Close first, and a live loop
// surviving Close keeps importing into freed-up roots.
func TestClose_StopsNativeLiveScanLoop(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	codexRoot := filepath.Join(root, ".codex")
	require.NoError(t, os.MkdirAll(codexRoot, 0o755))

	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(root, "secrets")}
	require.NoError(t, ss.Init())

	cx := codex.New()
	cx.HomeDir = root
	cx.SecretsStore = ss

	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		AdditionalRoots: []string{codexRoot},
		Adapters:        []adapter.Adapter{cx},
		Store:           store,
		QuietPeriod:     20 * time.Millisecond,
		GuardWindow:     2 * time.Second,
	})
	require.NoError(t, err)

	// ctx deliberately outlives Close — cancelled only at cleanup so a
	// pre-fix leaked loop cannot keep running past the test body.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = orch.Close()
	})

	go orch.RunNativeLiveScan(ctx, 20*time.Millisecond)

	agentsPath := filepath.Join(codexRoot, "AGENTS.md")
	require.NoError(t, os.WriteFile(agentsPath, []byte("# v1\n"), 0o644))
	require.Eventually(t, func() bool {
		mems, lerr := store.ListArtifacts(acf.KindMemory)
		return lerr == nil && len(mems) == 1
	}, 5*time.Second, 20*time.Millisecond, "sanity: the scan loop must be live before Close")

	require.NoError(t, orch.Close())

	// The loop is dead: a post-Close edit must never import even though ctx
	// is still un-cancelled. Bounded negative-observation window (~7 ticks).
	require.NoError(t, os.WriteFile(agentsPath, []byte("# v2 — post-Close edit, must not import\n"), 0o644))
	mems, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, mems, 1)
	eventsBefore, err := store.ReadEvents(acf.KindMemory, mems[0].ArtifactID)
	require.NoError(t, err)

	time.Sleep(150 * time.Millisecond)

	eventsAfter, err := store.ReadEvents(acf.KindMemory, mems[0].ArtifactID)
	require.NoError(t, err)
	require.Len(t, eventsAfter, len(eventsBefore),
		"a RunNativeLiveScan loop survived Close and imported a post-Close edit")
}

// TestClose_JoinsDetachedConversationFanOut: committing a conversation spawns
// a DETACHED fan-out goroutine (handleEvent's conversationOnly branch) that
// materializes native session files with context.Background(). Close must
// join it — this goroutine writing ~/.claude/projects/... while t.TempDir's
// RemoveAll runs is the exact "…/001/.claude: directory not empty" flake.
func TestClose_JoinsDetachedConversationFanOut(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
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
	blockingTarget := &blockingConversationSessionAdapter{
		Adapter: cx,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	releaseOnce := func() {
		select {
		case <-blockingTarget.release:
		default:
			close(blockingTarget.release)
		}
	}

	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    []adapter.Adapter{cc, blockingTarget},
		Store:       store,
		QuietPeriod: 20 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		releaseOnce()
		_ = orch.Close()
	})

	session := strings.Join([]string{
		`{"type":"summary","summary":"close join conversation","leafUuid":"conv-close-1"}`,
		`{"type":"user","sessionId":"conv-close-1","uuid":"u1","timestamp":"2026-06-29T16:34:12Z","message":{"role":"user","content":[{"type":"text","text":"What is the capital of Germany?"}]}}`,
		`{"type":"assistant","sessionId":"conv-close-1","uuid":"a1","parentUuid":"u1","timestamp":"2026-06-29T16:34:13Z","message":{"role":"assistant","content":[{"type":"text","text":"Berlin."}]}}`,
		"",
	}, "\n")
	sessionPath := filepath.Join(watched, "session-conv-close-1.jsonl")
	require.NoError(t, os.WriteFile(sessionPath, []byte(session), 0o644))

	// Drive the import synchronously — the conversationOnly branch inside
	// handleEvent detaches the fan-out goroutine, which then blocks in the
	// target's MaterializeConversationSession.
	require.True(t, orch.handleScanEvent(sessionPath), "conversation import must commit")

	select {
	case <-blockingTarget.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("detached conversation fan-out never reached the target materializer")
	}

	closed := make(chan struct{})
	go func() {
		_ = orch.Close()
		close(closed)
	}()

	select {
	case <-closed:
		t.Fatal("Close returned while the detached conversation fan-out was still materializing; Close must join it")
	case <-time.After(150 * time.Millisecond):
	}

	releaseOnce()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the detached fan-out finished")
	}
}
