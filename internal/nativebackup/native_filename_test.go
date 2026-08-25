package nativebackup

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

const historicalCodexColonName = "rollout-2026-06-30T18:16:48.3NZ-ae04c012.jsonl"

func TestAuthenticatedSnapshotWithHostNativeColonNameVerifiesAndRestores(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("colon is not a legal Windows filename component")
	}
	base := t.TempDir()
	nativeRoot := filepath.Join(base, "native", "codex")
	require.NoError(t, os.MkdirAll(nativeRoot, 0o700))
	nativePath := filepath.Join(nativeRoot, historicalCodexColonName)
	require.NoError(t, os.WriteFile(nativePath, []byte("original conversation"), 0o600))
	cacheDir := filepath.Join(nativeRoot, "cache")
	require.NoError(t, os.MkdirAll(cacheDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "generated.bin"), []byte("rebuildable"), 0o600))

	backupDir := filepath.Join(base, ".aplexica", "backups", "manual-colon")
	man, err := SnapshotAuthenticated([]AgentRoots{{Name: "codex", Roots: []string{nativeRoot}}}, backupDir)
	require.NoError(t, err)
	require.NoError(t, VerifySnapshotFilesContext(context.Background(), backupDir, man))
	_, err = VerifyAuthenticatedSnapshotContext(context.Background(), backupDir, manifestKeyPathForBackupDir(backupDir))
	require.NoError(t, err)
	sanitized, err := SanitizeSnapshotContext(context.Background(), backupDir, SanitizeOptions{CurrentAgentRoots: []AgentRoots{{
		Name: "codex", Roots: []string{nativeRoot}, ExcludePaths: []string{cacheDir},
	}}})
	require.NoError(t, err)
	require.Equal(t, SanitizeComplete, sanitized.Status)
	_, err = VerifyAuthenticatedSnapshotContext(context.Background(), backupDir, manifestKeyPathForBackupDir(backupDir))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(backupDir, "codex", relativize(nativeRoot), "cache"))
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, os.WriteFile(nativePath, []byte("changed"), 0o600))
	result, err := RestoreWithOptions(context.Background(), backupDir, NativeRestoreOptions{
		Agent:             "codex",
		CurrentAgentRoots: []AgentRoots{{Name: "codex", Roots: []string{nativeRoot}, ExcludePaths: []string{cacheDir}}},
		Coordinator:       LocalRestoreCoordinator{LockPath: filepath.Join(base, ".aplexica", "state", "native-restore.lock")},
	})
	require.NoError(t, err)
	require.Len(t, result.Files, 1)
	require.True(t, result.Files[0].OK)
	got, err := os.ReadFile(nativePath)
	require.NoError(t, err)
	require.Equal(t, "original conversation", string(got))
}

func TestCloudArchiveRoundTripsHostNativeColonName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("colon is not a legal Windows filename component")
	}
	base := t.TempDir()
	source := filepath.Join(base, "source")
	require.NoError(t, os.MkdirAll(filepath.Join(source, "codex", "sessions"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), []byte(`{"schemaVersion":2}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "codex", "sessions", historicalCodexColonName), []byte("conversation"), 0o600))

	archiveDir := filepath.Join(base, "archives")
	keyDir := filepath.Join(base, "keys")
	require.NoError(t, os.MkdirAll(archiveDir, 0o700))
	require.NoError(t, os.MkdirAll(keyDir, 0o700))
	archive := filepath.Join(archiveDir, "snapshot.enc")
	keyring := filepath.Join(keyDir, "native-cloud-keyring-v2.cbor")
	_, err := EncryptSnapshotDir(source, archive, keyring)
	require.NoError(t, err)
	restored := filepath.Join(base, "restored")
	_, err = DecryptSnapshotArchive(archive, restored, keyring)
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(restored, "codex", "sessions", historicalCodexColonName))
	require.NoError(t, err)
	require.Equal(t, "conversation", string(got))
}

func TestSecureRestoreDoesNotStageThroughNativeParentSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires Unix semantics")
	}
	base := t.TempDir()
	nativeRoot := filepath.Join(base, "native", "codex")
	sessions := filepath.Join(nativeRoot, "sessions")
	require.NoError(t, os.MkdirAll(sessions, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sessions, "state.json"), []byte("original"), 0o600))
	backupDir := filepath.Join(base, ".aplexica", "backups", "manual-symlink-target")
	_, err := SnapshotAuthenticated([]AgentRoots{{Name: "codex", Roots: []string{nativeRoot}}}, backupDir)
	require.NoError(t, err)

	require.NoError(t, os.RemoveAll(sessions))
	outside := filepath.Join(base, "outside-target")
	require.NoError(t, os.Mkdir(outside, 0o700))
	require.NoError(t, os.Symlink(outside, sessions))
	_, err = RestoreWithOptions(context.Background(), backupDir, NativeRestoreOptions{
		Agent:             "codex",
		CurrentAgentRoots: []AgentRoots{{Name: "codex", Roots: []string{nativeRoot}}},
		Coordinator:       LocalRestoreCoordinator{LockPath: filepath.Join(base, ".aplexica", "state", "native-restore.lock")},
	})
	require.Error(t, err)
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Empty(t, entries, "restore must not stage plaintext through a symlinked target parent")
}

func TestSecureRestoreRejectsSymlinkedBackupParentBeforeTargetWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires Unix semantics")
	}
	base := t.TempDir()
	nativeRoot := filepath.Join(base, "native", "codex")
	sessions := filepath.Join(nativeRoot, "sessions")
	nativePath := filepath.Join(sessions, "state.json")
	require.NoError(t, os.MkdirAll(sessions, 0o700))
	require.NoError(t, os.WriteFile(nativePath, []byte("original"), 0o600))
	backupDir := filepath.Join(base, ".aplexica", "backups", "manual-symlink-source")
	_, err := SnapshotAuthenticated([]AgentRoots{{Name: "codex", Roots: []string{nativeRoot}}}, backupDir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(nativePath, []byte("changed"), 0o600))

	backupSessions := filepath.Join(backupDir, "codex", relativize(nativeRoot), "sessions")
	held := backupSessions + ".held"
	require.NoError(t, os.Rename(backupSessions, held))
	outside := filepath.Join(base, "outside-source")
	require.NoError(t, os.Mkdir(outside, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "state.json"), []byte("original"), 0o600))
	require.NoError(t, os.Symlink(outside, backupSessions))
	_, err = RestoreWithOptions(context.Background(), backupDir, NativeRestoreOptions{
		Agent:             "codex",
		CurrentAgentRoots: []AgentRoots{{Name: "codex", Roots: []string{nativeRoot}}},
		Coordinator:       LocalRestoreCoordinator{LockPath: filepath.Join(base, ".aplexica", "state", "native-restore.lock")},
	})
	require.Error(t, err)
	got, err := os.ReadFile(nativePath)
	require.NoError(t, err)
	require.Equal(t, "changed", string(got), "rejected backup source must not mutate the live target")
}
