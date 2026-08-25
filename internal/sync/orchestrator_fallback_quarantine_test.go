package syncd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/stretchr/testify/require"
)

// fallbackStubAdapter forces primaryImport's extension/legacy FALLBACK path by
// declaring an empty BasenameToKind (so the basename branch never matches), and
// returns a controllable Import error so the fallback's quarantine accounting
// can be exercised. It embeds a real adapter only to satisfy the unused methods.
type fallbackStubAdapter struct {
	adapter.Adapter
	importErr error
}

func (fallbackStubAdapter) Name() string                       { return "claude-code" }
func (fallbackStubAdapter) Capabilities() adapter.Capabilities { return adapter.Capabilities{} }
func (a fallbackStubAdapter) Import(_ context.Context, _ *acf.Store, _ string) ([]string, error) {
	return nil, a.importErr
}

func newFallbackOrch(t *testing.T, stub adapter.Adapter, q *QuarantineTracker) (*Orchestrator, string) {
	t.Helper()
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	o, err := NewOrchestrator(Config{
		Dir:        watched,
		Adapters:   []adapter.Adapter{stub},
		Store:      store,
		Quarantine: q,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Close() })
	// A basename in NO adapter's BasenameToKind -> forces the fallback path.
	return o, filepath.Join(watched, "weird.xyz")
}

// TestFallbackImport_GenuineFailureDoesNotQuarantine is the fallback-path
// companion to TestPrimaryImportFailure_DoesNotQuarantineAdapter: a parse
// failure on one native/session file is recorded for status, but must not take
// the whole adapter offline. Agent histories often contain old, truncated, or
// actively-written files, and quarantining on Import would strand unrelated
// future live edits until restart/window expiry.
func TestFallbackImport_GenuineFailureDoesNotQuarantine(t *testing.T) {
	cc := claudecode.New()
	stub := fallbackStubAdapter{Adapter: cc, importErr: fmt.Errorf("synthetic parse boom")}
	q := NewQuarantineTracker(2, time.Minute)
	o, path := newFallbackOrch(t, stub, q)

	o.primaryImport(context.Background(), path)
	o.primaryImport(context.Background(), path)
	require.False(t, q.IsQuarantined("claude-code", time.Now()),
		"a malformed or in-progress fallback-native file must not take the whole adapter offline")
	require.Contains(t, o.AdapterLastErrors()["claude-code"], "boom")
}

// TestFallbackImport_NotHandledProbeMissDoesNotQuarantine: an adapter that
// returns adapter.ErrNotHandled (a benign "not my file" probe-miss) must NEVER
// be quarantined no matter how many foreign files it's probed with.
func TestFallbackImport_NotHandledProbeMissDoesNotQuarantine(t *testing.T) {
	cc := claudecode.New()
	stub := fallbackStubAdapter{Adapter: cc, importErr: fmt.Errorf("not mine: %w", adapter.ErrNotHandled)}
	q := NewQuarantineTracker(2, time.Minute)
	o, path := newFallbackOrch(t, stub, q)

	for i := 0; i < 5; i++ {
		o.primaryImport(context.Background(), path)
	}
	require.False(t, q.IsQuarantined("claude-code", time.Now()),
		"a not-handled probe-miss must never quarantine a healthy adapter")
	require.Empty(t, o.AdapterLastErrors()["claude-code"])
}

// TestQuarantinedAdapterStillImportsOwnedNativeConversation protects the
// directionality of FR-03.15: quarantine contains failing outbound writes into
// an adapter, but must never suppress reading that adapter's own native changes.
// Otherwise three unrelated export failures can strand a real conversation for
// the entire quarantine window and prevent both local fan-out and remote sync.
//
// The fixture also mirrors Claude Code 2.1.211's metadata-first transcript
// layout: session bookkeeping rows can precede the first user/assistant turn.
func TestQuarantinedAdapterStillImportsOwnedNativeConversation(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "watched")
	claudeRoot := filepath.Join(root, ".claude")
	projectsRoot := filepath.Join(claudeRoot, "projects")
	sessionDir := filepath.Join(projectsRoot, "-Users-test")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	cc := claudecode.New()
	cc.HomeDir = root
	cc.CanonicalConversations = true

	q := NewQuarantineTracker(1, 10*time.Minute)
	require.True(t, q.RecordFailure(cc.Name(), time.Now()))
	require.True(t, q.IsQuarantined(cc.Name(), time.Now()))

	o, err := NewOrchestrator(Config{
		Dir:            watched,
		RecursiveRoots: []string{projectsRoot},
		RootsByAdapter: map[string][]string{
			cc.Name(): {claudeRoot, projectsRoot},
		},
		Adapters:                  []adapter.Adapter{cc},
		Store:                     store,
		Quarantine:                q,
		QuietPeriod:               10 * time.Millisecond,
		RecentClaudeSessionWindow: time.Minute,
	})
	require.NoError(t, err)
	defer o.Close()

	sessionPath := filepath.Join(sessionDir, "metadata-first.jsonl")
	transcript := strings.Join([]string{
		`{"type":"last-prompt","sessionId":"metadata-first"}`,
		`{"type":"mode","mode":"normal","sessionId":"metadata-first"}`,
		`{"type":"permission-mode","permissionMode":"default","sessionId":"metadata-first"}`,
		`{"type":"user","sessionId":"metadata-first","cwd":"/Users/test","isSidechain":false,"message":{"role":"user","content":"What is the temperature on Saturn?"}}`,
		`{"type":"assistant","sessionId":"metadata-first","cwd":"/Users/test","isSidechain":false,"message":{"role":"assistant","content":[{"type":"text","text":"About -140 C near the cloud tops."}]}}`,
		"",
	}, "\n")
	require.NoError(t, os.WriteFile(sessionPath, []byte(transcript), 0o600))
	readyAt := time.Now().Add(-time.Second)
	require.NoError(t, os.Chtimes(sessionPath, readyAt, readyAt))
	o.markClaudeHotSession(sessionPath)

	require.Equal(t, 1, o.ScanRecentClaudeSessions(context.Background()))

	conversations, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, conversations, 1)
	require.Equal(t, sessionPath, conversations[0].SourcePath)
	require.True(t, o.scanCache.unchanged(sessionPath),
		"a successful quarantined-source import must become terminal, not retry every 500ms")
	require.Equal(t, 0, o.ScanRecentClaudeSessions(context.Background()))
	require.True(t, q.IsQuarantined(cc.Name(), time.Now()),
		"reading native source data must not clear the outbound quarantine")
}
