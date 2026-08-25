package syncd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/stretchr/testify/require"
)

// failingImportAdapter is a real claude-code adapter (so it declares CLAUDE.md
// in its basename map and a supporting NativePath) whose Import always fails.
type failingImportAdapter struct {
	adapter.Adapter
}

func (failingImportAdapter) Import(_ context.Context, _ *acf.Store, _ string) ([]string, error) {
	return nil, fmt.Errorf("synthetic parse failure")
}

type countingFailingImportAdapter struct {
	adapter.Adapter
	calls int
}

func (a *countingFailingImportAdapter) Import(_ context.Context, _ *acf.Store, _ string) ([]string, error) {
	a.calls++
	return nil, fmt.Errorf("synthetic parse failure")
}

type dynamicCountingFailingImportAdapter struct {
	*countingFailingImportAdapter
	root string
}

type sharedRootRuntimeAdapter struct {
	adapter.Adapter
	name      string
	root      string
	installed bool
	handles   bool
	calls     int
}

func (a *sharedRootRuntimeAdapter) Name() string { return a.name }

func (a *sharedRootRuntimeAdapter) CandidateDiscovery() adapter.Discovery {
	return adapter.Discovery{GlobalRoots: []string{a.root}}
}

func (a *sharedRootRuntimeAdapter) Discover() (adapter.Discovery, error) {
	return adapter.Discovery{Installed: a.installed, GlobalRoots: []string{a.root}}, nil
}

func (a *sharedRootRuntimeAdapter) Capabilities() adapter.Capabilities {
	caps := adapter.Capabilities{Name: a.name, BasenameToKind: map[string]acf.Kind{}}
	if a.handles {
		caps.BasenameToKind["late.txt"] = acf.KindMemory
	}
	return caps
}

func (a *sharedRootRuntimeAdapter) NativePath(acf.Artifact, string) (string, bool, error) {
	return filepath.Join(a.root, "late.txt"), a.handles, nil
}

func (a *sharedRootRuntimeAdapter) Import(context.Context, *acf.Store, string) ([]string, error) {
	a.calls++
	if !a.handles {
		return nil, adapter.ErrNotHandled
	}
	return nil, nil
}

func (a *dynamicCountingFailingImportAdapter) CandidateDiscovery() adapter.Discovery {
	return adapter.Discovery{GlobalRoots: []string{a.root}}
}

func (a *dynamicCountingFailingImportAdapter) Discover() (adapter.Discovery, error) {
	return adapter.Discovery{Installed: true, GlobalRoots: []string{a.root}}, nil
}

// TestPrimaryImport_AdapterLastErrNoRace is the P2-2 (part 1) regression guard:
// primaryImport records claimed-file import failures for status while
// AdapterLastErrors reads the same map. This used to race when import failures
// tripped quarantine; the guard still protects the normal status path now that
// malformed native files no longer quarantine the whole adapter.
func TestPrimaryImport_AdapterLastErrNoRace(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	cc := claudecode.New()
	cc.HomeDir = root
	orch, err := NewOrchestrator(Config{
		Dir:        watched,
		Adapters:   []adapter.Adapter{failingImportAdapter{Adapter: cc}},
		Store:      store,
		Quarantine: NewQuarantineTracker(1, time.Nanosecond),
	})
	require.NoError(t, err)
	defer orch.Close()

	memPath := filepath.Join(watched, "CLAUDE.md")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			orch.primaryImport(context.Background(), memPath)
		}
	}()
	for {
		select {
		case <-done:
			// Drain once more; the assertion is simply "no race / no panic".
			_ = orch.AdapterLastErrors()
			return
		default:
			_ = orch.AdapterLastErrors()
		}
	}
}

func TestPrimaryImportFailure_DoesNotQuarantineAdapter(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	cc := claudecode.New()
	cc.HomeDir = root
	q := NewQuarantineTracker(1, 10*time.Minute)
	orch, err := NewOrchestrator(Config{
		Dir:        watched,
		Adapters:   []adapter.Adapter{failingImportAdapter{Adapter: cc}},
		Store:      store,
		Quarantine: q,
	})
	require.NoError(t, err)
	defer orch.Close()

	_, _, ok := orch.primaryImport(context.Background(), filepath.Join(watched, "CLAUDE.md"))
	require.False(t, ok)
	require.False(t, q.IsQuarantined("claude-code", time.Now()),
		"a malformed or in-progress native file must not take the whole adapter offline")
	require.Contains(t, orch.AdapterLastErrors(), "claude-code")
}

func TestScanCachesFailedImportUntilFileChanges(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	cc := claudecode.New()
	cc.HomeDir = root
	failing := &countingFailingImportAdapter{Adapter: cc}
	orch, err := NewOrchestrator(Config{
		Dir:      watched,
		Adapters: []adapter.Adapter{failing},
		Store:    store,
	})
	require.NoError(t, err)
	defer orch.Close()

	sessionPath := filepath.Join(watched, "session.jsonl")
	require.NoError(t, os.WriteFile(sessionPath, []byte("bad-v1"), 0o644))

	require.NoError(t, orch.scanRoot(watched))
	require.Equal(t, 1, failing.calls)

	require.NoError(t, orch.scanRoot(watched))
	require.Equal(t, 1, failing.calls, "unchanged failed imports should not be reparsed on every scan")

	next := time.Now().Add(time.Second)
	require.NoError(t, os.Chtimes(sessionPath, next, next))
	require.NoError(t, orch.scanRoot(watched))
	require.Equal(t, 2, failing.calls, "a real file change must retry the failed import")
}

func TestScanCachesMalformedFileForInstalledDynamicOwner(t *testing.T) {
	root := realTempDir(t)
	nativeRoot := filepath.Join(root, ".claude")
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(nativeRoot, 0o755))
	require.NoError(t, os.MkdirAll(project, 0o755))
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	cc := claudecode.New()
	cc.HomeDir = root
	failing := &dynamicCountingFailingImportAdapter{
		countingFailingImportAdapter: &countingFailingImportAdapter{Adapter: cc},
		root:                         nativeRoot,
	}
	orch, err := NewOrchestrator(Config{
		Dir:                     project,
		Adapters:                []adapter.Adapter{failing},
		Store:                   store,
		DynamicAdapterDiscovery: true,
		RootsByAdapter:          map[string][]string{failing.Name(): {nativeRoot}},
	})
	require.NoError(t, err)
	defer orch.Close()

	path := filepath.Join(nativeRoot, "CLAUDE.md")
	require.NoError(t, os.WriteFile(path, []byte("malformed"), 0o644))
	require.False(t, orch.handleScanEvent(path))
	firstCalls := failing.calls
	require.Positive(t, firstCalls)
	require.True(t, orch.scanCache.unchanged(path))
	require.True(t, orch.handleScanEvent(path), "an unchanged cached failure is a handled scan no-op")
	require.Equal(t, firstCalls, failing.calls, "installed dynamic parse failures must still be cached")
}

func TestScanDoesNotCacheWhenOneSharedRootOwnerIsStillUnavailable(t *testing.T) {
	root := realTempDir(t)
	nativeRoot := filepath.Join(root, "shared-native")
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(nativeRoot, 0o755))
	require.NoError(t, os.MkdirAll(project, 0o755))
	path := filepath.Join(nativeRoot, "late.txt")
	require.NoError(t, os.WriteFile(path, []byte("preexisting"), 0o644))
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	base := claudecode.New()
	available := &sharedRootRuntimeAdapter{Adapter: base, name: "available", root: nativeRoot, installed: true}
	late := &sharedRootRuntimeAdapter{Adapter: base, name: "late", root: nativeRoot, handles: true}
	orch, err := NewOrchestrator(Config{
		Dir:                     project,
		Adapters:                []adapter.Adapter{available, late},
		Store:                   store,
		DynamicAdapterDiscovery: true,
		RootsByAdapter: map[string][]string{
			available.Name(): {nativeRoot},
			late.Name():      {nativeRoot},
		},
	})
	require.NoError(t, err)
	defer orch.Close()

	require.False(t, orch.handleScanEvent(path))
	require.False(t, orch.scanCache.unchanged(path),
		"an eligible non-handler must not hide an unavailable shared-root owner")
	late.installed = true
	require.True(t, orch.handleScanEvent(path))
	require.Positive(t, late.calls)
}
