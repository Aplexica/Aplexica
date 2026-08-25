package syncd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The import-scan cache remembers a file's (size, mtime) fingerprint after a
// successful import so a restart can skip re-importing — and, critically,
// re-encoding — unchanged files. unchanged() is true only for a recorded path
// whose size+mtime still match; a fresh path, a resized file, or a re-touched
// file all read as changed.
func TestImportScanCache_RecordUnchangedAndChange(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "conv.jsonl")
	require.NoError(t, os.WriteFile(f, []byte("v1"), 0o644))

	c := loadImportScanCache(dir)
	require.False(t, c.unchanged(f), "never-recorded path must read as changed")

	c.record(f)
	require.True(t, c.unchanged(f), "just-recorded, untouched file must read as unchanged")

	// A content/size change must be detected.
	require.NoError(t, os.WriteFile(f, []byte("v2 is longer"), 0o644))
	require.False(t, c.unchanged(f), "a size change must read as changed")
}

// The cache persists under the store root and survives reconstruction (the
// daemon-restart case): record + flush, then a fresh cache loaded from the same
// root reports the unchanged file as unchanged without re-recording.
func TestImportScanCache_PersistsAcrossReload(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "big.jsonl")
	require.NoError(t, os.WriteFile(f, []byte("payload"), 0o644))

	c1 := loadImportScanCache(root)
	c1.record(f)
	require.NoError(t, c1.flush())

	// Fresh cache loaded from the same root (simulates a daemon restart).
	c2 := loadImportScanCache(root)
	require.True(t, c2.unchanged(f), "a reloaded cache must still recognize the unchanged file")

	// And a change after reload is still detected.
	require.NoError(t, os.WriteFile(f, []byte("payload-v2-different-size"), 0o644))
	require.False(t, c2.unchanged(f))
}

// A missing or corrupt cache file must degrade to "everything is new" (full
// re-import), never an error — the cache is a pure optimization.
func TestImportScanCache_CorruptOrMissingIsEmpty(t *testing.T) {
	missing := loadImportScanCache(t.TempDir())
	require.NotNil(t, missing)
	require.False(t, missing.unchanged(filepath.Join(t.TempDir(), "nope")))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, importScanCacheName), []byte("{not json"), 0o644))
	corrupt := loadImportScanCache(root)
	require.NotNil(t, corrupt)
	f := filepath.Join(root, "x")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))
	require.False(t, corrupt.unchanged(f), "corrupt cache must behave as empty")
}

func TestImportScanCache_V1InvalidatesOnlyGeneratedCodexProjection(t *testing.T) {
	root := t.TempDir()
	generated := filepath.Join(root, "generated.jsonl")
	regular := filepath.Join(root, "regular.jsonl")
	require.NoError(t, os.WriteFile(generated, []byte(
		`{"type":"session_meta","payload":{"id":"019f0000-0000-7000-8000-000000000001","aplexica_branch_id":"main","aplexica_thread_id":"019f0000-0000-7000-8000-000000000001"}}`+"\n",
	), 0o644))
	require.NoError(t, os.WriteFile(regular, []byte(`{"type":"session_meta","payload":{"id":"native"}}`+"\n"), 0o644))
	generatedFP, ok := fingerprintPath(generated)
	require.True(t, ok)
	regularFP, ok := fingerprintPath(regular)
	require.True(t, ok)
	legacy, err := json.Marshal(map[string]scanFP{generated: generatedFP, regular: regularFP})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, importScanCacheName), legacy, 0o644))

	c := loadImportScanCache(root)
	require.False(t, c.unchanged(generated), "v1.0.39 must rescan an unchanged generated Codex rollout once")
	require.True(t, c.unchanged(regular), "unrelated large native histories must retain their restart fingerprint")
	c.record(generated)
	require.NoError(t, c.flush())

	reloaded := loadImportScanCache(root)
	require.True(t, reloaded.unchanged(generated), "the projection migration must not repeat after the v4 cache is written")
	require.True(t, reloaded.unchanged(regular))
}

func TestImportScanCache_V2InvalidatesOnlyGeneratedMainConversationProjection(t *testing.T) {
	root := t.TempDir()
	generated := filepath.Join(root, "generated-codex.jsonl")
	generatedClaude := filepath.Join(root, "generated-claude.jsonl")
	regular := filepath.Join(root, "regular.jsonl")
	require.NoError(t, os.WriteFile(generated, []byte(
		`{"type":"session_meta","payload":{"id":"019f0000-0000-7000-8000-000000000001","aplexica_branch_id":"main","aplexica_thread_id":"019f0000-0000-7000-8000-000000000001"}}`+"\n",
	), 0o644))
	require.NoError(t, os.WriteFile(generatedClaude, []byte(
		`{"type":"custom-title","sessionId":"019f0000-0000-7000-8000-000000000002","aplexicaBranchId":"main","aplexicaThreadId":"019f0000-0000-7000-8000-000000000002"}`+"\n",
	), 0o644))
	require.NoError(t, os.WriteFile(regular, []byte(`{"type":"session_meta","payload":{"id":"native"}}`+"\n"), 0o644))
	generatedFP, ok := fingerprintPath(generated)
	require.True(t, ok)
	generatedClaudeFP, ok := fingerprintPath(generatedClaude)
	require.True(t, ok)
	regularFP, ok := fingerprintPath(regular)
	require.True(t, ok)
	v2, err := json.Marshal(importScanCacheDisk{
		Version:      2,
		Fingerprints: map[string]scanFP{generated: generatedFP, generatedClaude: generatedClaudeFP, regular: regularFP},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, importScanCacheName), v2, 0o644))

	c := loadImportScanCache(root)
	require.False(t, c.unchanged(generated), "v4 must retry the exact generated Codex repair once")
	require.False(t, c.unchanged(generatedClaude), "v4 must retry the exact generated Claude repair once")
	require.True(t, c.unchanged(regular), "v4 must preserve unrelated native history fingerprints")
	c.record(generated)
	c.record(generatedClaude)
	require.NoError(t, c.flush())

	reloaded := loadImportScanCache(root)
	require.True(t, reloaded.unchanged(generated), "the repair migration must not repeat after the v4 cache is written")
	require.True(t, reloaded.unchanged(generatedClaude))
	require.True(t, reloaded.unchanged(regular))
}

func TestImportScanCache_V3InvalidatesNewestNativeCodexProjection(t *testing.T) {
	root := t.TempDir()
	native := writeNativeCodexCacheSession(t, root, "rollout-current.jsonl", "native-current", time.Now())
	regular := filepath.Join(root, "regular.jsonl")
	require.NoError(t, os.WriteFile(regular, []byte(`{"type":"session_meta","payload":{"id":"native"}}`+"\n"), 0o644))
	nativeFP, ok := fingerprintPath(native)
	require.True(t, ok)
	regularFP, ok := fingerprintPath(regular)
	require.True(t, ok)
	v3, err := json.Marshal(importScanCacheDisk{
		Version:      previousImportScanCacheSchemaVersion,
		Fingerprints: map[string]scanFP{native: nativeFP, regular: regularFP},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, importScanCacheName), v3, 0o644))

	c := loadImportScanCache(root)
	require.False(t, c.unchanged(native), "v4 must repair the newest unchanged native Codex rollout once")
	require.True(t, c.unchanged(regular), "non-Codex JSONL history must retain its fingerprint")
	c.record(native)
	require.NoError(t, c.flush())

	reloaded := loadImportScanCache(root)
	require.True(t, reloaded.unchanged(native), "the native repair must not repeat after the v4 cache is written")
	require.True(t, reloaded.unchanged(regular))
}

func TestInvalidateNewestNativeCodexFingerprints_OrdersAndBoundsBytes(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	newest := writeNativeCodexCacheSession(t, root, "rollout-c.jsonl", "native-c", base.Add(3*time.Second))
	middle := writeNativeCodexCacheSession(t, root, "rollout-b.jsonl", "native-b", base.Add(2*time.Second))
	oldest := writeNativeCodexCacheSession(t, root, "rollout-a.jsonl", "native-a", base.Add(time.Second))
	fps := map[string]scanFP{}
	for _, path := range []string{newest, middle, oldest} {
		fp, ok := fingerprintPath(path)
		require.True(t, ok)
		fps[path] = fp
	}
	budget := fps[newest].Size + fps[middle].Size
	invalidateNewestNativeCodexFingerprints(fps, budget, nativeCodexRepairMaxFiles)
	require.NotContains(t, fps, newest)
	require.NotContains(t, fps, middle)
	require.Contains(t, fps, oldest, "the first rollout beyond the cumulative budget must remain cached")
}

func TestInvalidateNewestNativeCodexFingerprints_AlwaysSelectsNewestOversize(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	newest := writeNativeCodexCacheSession(t, root, "rollout-new.jsonl", "native-new", base.Add(time.Second))
	older := writeNativeCodexCacheSession(t, root, "rollout-old.jsonl", "native-old", base)
	fps := map[string]scanFP{}
	for _, path := range []string{newest, older} {
		fp, ok := fingerprintPath(path)
		require.True(t, ok)
		fps[path] = fp
	}
	invalidateNewestNativeCodexFingerprints(fps, 1, nativeCodexRepairMaxFiles)
	require.NotContains(t, fps, newest, "the newest rollout must migrate even when it alone exceeds the budget")
	require.Contains(t, fps, older)
}

func TestGeneratedMainConversationSession_RequiresMatchingMainIdentity(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(root, name)
		require.NoError(t, os.WriteFile(path, []byte(body+"\n"), 0o644))
		return path
	}
	codexFork := write("codex-fork.jsonl",
		`{"type":"session_meta","payload":{"id":"thread","aplexica_branch_id":"fork","aplexica_thread_id":"thread"}}`)
	codexMismatch := write("codex-mismatch.jsonl",
		`{"type":"session_meta","payload":{"id":"other","aplexica_branch_id":"main","aplexica_thread_id":"thread"}}`)
	claudeFork := write("claude-fork.jsonl",
		`{"type":"custom-title","sessionId":"thread","aplexicaBranchId":"fork","aplexicaThreadId":"thread"}`)
	claudeMismatch := write("claude-mismatch.jsonl",
		`{"type":"custom-title","sessionId":"other","aplexicaBranchId":"main","aplexicaThreadId":"thread"}`)

	for _, path := range []string{codexFork, codexMismatch, claudeFork, claudeMismatch} {
		require.False(t, aplexicaGeneratedMainConversationSession(path), path)
	}
}

// A nil cache and an empty-root cache must be safe no-ops (some orchestrators
// are constructed without a store root in tests).
func TestImportScanCache_NilAndEmptyRootSafe(t *testing.T) {
	var c *importScanCache
	require.False(t, c.unchanged("/whatever"))
	require.NotPanics(t, func() { c.record("/whatever") })
	require.NoError(t, c.flush())

	empty := loadImportScanCache("")
	require.NotPanics(t, func() { empty.record("/whatever") })
	require.NoError(t, empty.flush())
}

func writeNativeCodexCacheSession(t *testing.T, root, name, sessionID string, mod time.Time) string {
	t.Helper()
	dir := filepath.Join(root, ".codex", "sessions", "2026", "07", "19")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, name)
	body := []byte(`{"type":"session_meta","payload":{"id":"` + sessionID + `"}}` + "\n")
	require.NoError(t, os.WriteFile(path, body, 0o644))
	require.NoError(t, os.Chtimes(path, mod, mod))
	return path
}
