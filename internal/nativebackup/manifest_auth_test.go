package nativebackup

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/stretchr/testify/require"
)

func TestAuthenticatedSnapshotManifestRejectsTampering(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "native", "state")
	require.NoError(t, os.MkdirAll(src, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a"), []byte("x"), 0o600))
	dir := filepath.Join(root, "backups", "manual-test")
	man, err := SnapshotAuthenticated([]AgentRoots{{Name: "agent", Roots: []string{src}}}, dir)
	require.NoError(t, err)
	require.Equal(t, 2, man.SchemaVersion)
	require.NoError(t, VerifyDefaultManifest(man, dir))
	man.Agents[0].Roots[0].Bytes++
	require.Error(t, VerifyDefaultManifest(man, dir))
	keyInfo, err := os.Stat(filepath.Join(root, "keys", "native-manifest-hmac-v2"))
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Zero(t, keyInfo.Mode().Perm()&0o077)
	}
}

func TestAuthenticateSnapshotManifestContextDoesNotReplaceFullPayloadProof(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "native", "state")
	require.NoError(t, os.MkdirAll(src, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, "state.json"), []byte("original"), 0o600))
	backupDir := filepath.Join(root, "backups", "pre-sync-agent-test")
	man, err := SnapshotAuthenticated([]AgentRoots{{Name: "agent", Roots: []string{src}}}, backupDir)
	require.NoError(t, err)
	require.Len(t, man.Agents, 1)
	require.Len(t, man.Agents[0].Roots, 1)

	// The metadata-only API authenticates the signed manifest without reading
	// payload bytes. A same-size payload mutation therefore remains a candidate,
	// but the unchanged full verifier must still reject it before acceptance.
	payloadPath := filepath.Join(backupDir, filepath.FromSlash(man.Agents[0].Roots[0].Path))
	require.NoError(t, os.WriteFile(payloadPath, []byte("mutated!"), 0o600))
	keyPath := manifestKeyPathForBackupDir(backupDir)
	classified, err := AuthenticateSnapshotManifestContext(context.Background(), backupDir, keyPath)
	require.NoError(t, err)
	require.Equal(t, "agent", classified.Agents[0].Name)
	_, err = VerifyAuthenticatedSnapshotContext(context.Background(), backupDir, keyPath)
	require.ErrorContains(t, err, "digest mismatch")
}

func TestRestoreWithOptionsRecoversApplyingJournalBeforeSnapshot(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native", "agent")
	file := filepath.Join(nativeRoot, "state.txt")
	rel := "state.txt"
	require.NoError(t, os.MkdirAll(nativeRoot, 0o700))
	require.NoError(t, os.WriteFile(file, []byte("snapshot"), 0o600))
	backup := filepath.Join(root, "backups", "manual-test")
	_, err := SnapshotAuthenticated([]AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, backup)
	require.NoError(t, err)

	// Model a process death after replacing the first file but before commit.
	require.NoError(t, os.WriteFile(file, []byte("old-live"), 0o600))
	native, err := privatefs.OpenRoot(nativeRoot, privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly})
	require.NoError(t, err)
	defer native.Close()
	stateDir := filepath.Join(root, "backups", ".aplexica-native-restore-state")
	require.NoError(t, privatefs.EnsureDir(stateDir, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}))
	control, err := privatefs.OpenRoot(stateDir, privatefs.DirPolicy{Access: privatefs.AccessPrivate})
	require.NoError(t, err)
	defer control.Close()
	tempRel, rollbackRel := ".aplexica-restore-verified-crash", ".aplexica-restore-rollback-crash"
	require.NoError(t, os.WriteFile(filepath.Join(nativeRoot, tempRel), []byte("crashed-new"), 0o600))
	j, err := privatefs.BeginJournal(control, privatefs.JournalPlan{Kind: "native-restore", TransactionID: acf.NewID(), Entries: []privatefs.JournalEntry{{RootID: "root-000001", ObjectKind: "file", Operation: "replace", FinalRel: rel, TempRel: tempRel, RollbackRel: rollbackRel, FinalExisted: true, ExpectedFinalSHA256: sha256.Sum256([]byte("crashed-new")), ExpectedRollbackSHA256: sha256.Sum256([]byte("old-live"))}}})
	require.NoError(t, err)
	require.NoError(t, native.Rename(rel, rollbackRel))
	require.NoError(t, native.Rename(tempRel, rel))
	require.NoError(t, j.MarkApplied(0))

	res, err := RestoreWithOptions(context.Background(), backup, NativeRestoreOptions{Agent: "agent", CurrentAgentRoots: []AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, Coordinator: LocalRestoreCoordinator{LockPath: filepath.Join(root, "state", "native-restore.lock")}})
	require.NoError(t, err)
	// The pre-restore snapshot must contain the rolled-back live bytes, not the
	// half-applied transaction bytes.
	preCopy := filepath.Join(res.PreRestoreDir, "agent", relativize(nativeRoot), rel)
	got, err := os.ReadFile(preCopy)
	require.NoError(t, err)
	require.Equal(t, "old-live", string(got))
}

func TestRestoreWithOptionsUsesCurrentRootsAndAuthenticatedManifest(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native", "agent")
	file := filepath.Join(nativeRoot, "state.txt")
	require.NoError(t, os.MkdirAll(nativeRoot, 0o700))
	require.NoError(t, os.WriteFile(file, []byte("snapshot"), 0o600))
	backup := filepath.Join(root, "backups", "manual-test")
	_, err := SnapshotAuthenticated([]AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, backup)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, []byte("changed"), 0o600))
	res, err := RestoreWithOptions(context.Background(), backup, NativeRestoreOptions{Agent: "agent", CurrentAgentRoots: []AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, Coordinator: LocalRestoreCoordinator{LockPath: filepath.Join(root, "state", "native-restore.lock")}})
	require.NoError(t, err)
	require.Len(t, res.Files, 1)
	got, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, "snapshot", string(got))
}

func TestRestoreWithOptions_ExcludedRuntimeStateIsNeitherRestoredNorPreSnapshotted(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native", "agent")
	stateFile := filepath.Join(nativeRoot, "state.txt")
	runtimeDir := filepath.Join(nativeRoot, "cache")
	runtimeFile := filepath.Join(runtimeDir, "runtime.bin")
	require.NoError(t, os.MkdirAll(runtimeDir, 0o700))
	require.NoError(t, os.WriteFile(stateFile, []byte("snapshot-state"), 0o600))
	require.NoError(t, os.WriteFile(runtimeFile, []byte("snapshot-runtime"), 0o600))
	policy := AgentRoots{Name: "agent", Roots: []string{nativeRoot}, ExcludePaths: []string{runtimeDir}}
	backup := filepath.Join(root, "backups", "manual-test")
	_, err := SnapshotAuthenticated([]AgentRoots{policy}, backup)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(stateFile, []byte("live-state"), 0o600))
	require.NoError(t, os.WriteFile(runtimeFile, []byte("live-runtime"), 0o600))
	res, err := RestoreWithOptions(context.Background(), backup, NativeRestoreOptions{
		Agent:             "agent",
		CurrentAgentRoots: []AgentRoots{policy},
		Coordinator:       LocalRestoreCoordinator{LockPath: filepath.Join(root, "state", "native-restore.lock")},
	})
	require.NoError(t, err)
	require.Len(t, res.Files, 1, "only the user-state file belongs to the backup")

	got, err := os.ReadFile(stateFile)
	require.NoError(t, err)
	require.Equal(t, "snapshot-state", string(got))
	got, err = os.ReadFile(runtimeFile)
	require.NoError(t, err)
	require.Equal(t, "live-runtime", string(got), "excluded runtime state must remain current")

	preManifest, err := ReadManifest(res.PreRestoreDir)
	require.NoError(t, err)
	require.Len(t, preManifest.Agents, 1)
	require.Len(t, preManifest.Agents[0].Roots, 1)
	_, err = os.Stat(filepath.Join(res.PreRestoreDir, "agent", relativize(nativeRoot), "cache"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRestoreWithOptions_AppliesCurrentExclusionsToOlderAuthenticatedBackup(t *testing.T) {
	root := t.TempDir()
	nativeRoot := filepath.Join(root, "native", "agent")
	stateFile := filepath.Join(nativeRoot, "state.txt")
	runtimeDir := filepath.Join(nativeRoot, "cache")
	runtimeFile := filepath.Join(runtimeDir, "runtime.bin")
	require.NoError(t, os.MkdirAll(runtimeDir, 0o700))
	require.NoError(t, os.WriteFile(stateFile, []byte("snapshot-state"), 0o600))
	require.NoError(t, os.WriteFile(runtimeFile, []byte("stale-snapshot-runtime"), 0o600))
	backup := filepath.Join(root, "backups", "manual-test")
	_, err := SnapshotAuthenticated([]AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, backup)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(stateFile, []byte("live-state"), 0o600))
	require.NoError(t, os.WriteFile(runtimeFile, []byte("current-runtime"), 0o600))
	policy := AgentRoots{Name: "agent", Roots: []string{nativeRoot}, ExcludePaths: []string{runtimeDir}}
	res, err := RestoreWithOptions(context.Background(), backup, NativeRestoreOptions{
		Agent:             "agent",
		CurrentAgentRoots: []AgentRoots{policy},
		Coordinator:       LocalRestoreCoordinator{LockPath: filepath.Join(root, "state", "native-restore.lock")},
	})
	require.NoError(t, err)
	require.Len(t, res.Files, 1)

	got, err := os.ReadFile(stateFile)
	require.NoError(t, err)
	require.Equal(t, "snapshot-state", string(got))
	got, err = os.ReadFile(runtimeFile)
	require.NoError(t, err)
	require.Equal(t, "current-runtime", string(got), "current policy must protect restores from stale excluded data")
}
