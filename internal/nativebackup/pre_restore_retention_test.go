package nativebackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPrunePreRestoreHistoryRemovesIncompleteAndBoundsCompleteHistory(t *testing.T) {
	base := t.TempDir()
	backupsRoot := filepath.Join(base, "backups")
	nativeRoot := filepath.Join(base, "native")
	require.NoError(t, os.MkdirAll(nativeRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(nativeRoot, "state.json"), []byte("state"), 0o600))

	created := []string{
		"pre-restore-2026-07-18T01-00-00Z",
		"pre-restore-2026-07-18T02-00-00Z",
		"pre-restore-2026-07-18T03-00-00Z",
	}
	for i, id := range created {
		dir := filepath.Join(backupsRoot, id)
		man, err := Snapshot([]AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, dir)
		require.NoError(t, err)
		man.CreatedAt = time.Unix(int64(i+1), 0).UTC()
		require.NoError(t, writeManifest(dir, man))
	}
	partial := filepath.Join(backupsRoot, "pre-restore-2026-07-18T04-00-00Z")
	require.NoError(t, os.MkdirAll(partial, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(partial, "partial.bin"), []byte("residue"), 0o600))

	removed, err := PrunePreRestoreHistory(context.Background(), backupsRoot, 2, filepath.Join(backupsRoot, created[0]))
	require.NoError(t, err)
	require.Equal(t, 2, removed)
	require.DirExists(t, filepath.Join(backupsRoot, created[0]), "the explicitly restored older undo point must be preserved")
	require.NoDirExists(t, filepath.Join(backupsRoot, created[1]))
	require.DirExists(t, filepath.Join(backupsRoot, created[2]), "the newest other undo point must be retained")
	require.NoDirExists(t, partial, "an interrupted tree without a strict manifest is not a recovery point")
}

func TestPrunePreRestoreHistoryBrokenNewerInventoriesCannotDisplaceValidRollback(t *testing.T) {
	base := t.TempDir()
	backupsRoot := filepath.Join(base, "backups")
	nativeRoot := filepath.Join(base, "native")
	source := filepath.Join(nativeRoot, "state.json")
	require.NoError(t, os.MkdirAll(nativeRoot, 0o700))
	require.NoError(t, os.WriteFile(source, []byte("valid rollback state"), 0o600))

	validID := "pre-restore-2026-07-18T01-00-00Z"
	validDir := filepath.Join(backupsRoot, validID)
	validManifest, err := Snapshot([]AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, validDir)
	require.NoError(t, err)
	validManifest.CreatedAt = time.Unix(1, 0).UTC()
	require.NoError(t, writeManifest(validDir, validManifest))

	brokenIDs := []string{
		"pre-restore-2026-07-18T02-00-00Z",
		"pre-restore-2026-07-18T03-00-00Z",
		"pre-restore-2026-07-18T04-00-00Z",
	}
	for i, id := range brokenIDs {
		dir := filepath.Join(backupsRoot, id)
		man, snapshotErr := Snapshot([]AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, dir)
		require.NoError(t, snapshotErr)
		require.Len(t, man.Agents[0].Roots, 1)
		man.CreatedAt = time.Unix(int64(i+2), 0).UTC()
		require.NoError(t, writeManifest(dir, man))
		payload := filepath.Join(dir, filepath.FromSlash(man.Agents[0].Roots[0].Path))
		switch i {
		case 0:
			require.NoError(t, os.Remove(payload))
		case 1:
			require.NoError(t, os.WriteFile(payload, []byte("short"), 0o600))
		case 2:
			require.NoError(t, os.Remove(payload))
			require.NoError(t, os.Mkdir(payload, 0o700))
		}
		_, manifestErr := ReadSnapshotManifestContext(context.Background(), dir)
		require.NoError(t, manifestErr, "fixture manifest must remain structurally valid")
	}

	removed, err := PrunePreRestoreHistory(context.Background(), backupsRoot, 1, "")
	require.NoError(t, err)
	require.Equal(t, len(brokenIDs), removed)
	require.DirExists(t, validDir, "broken newer inventories must not evict the last valid rollback")
	for _, id := range brokenIDs {
		require.NoDirExists(t, filepath.Join(backupsRoot, id))
	}
}

func TestPrunePreRestoreHistoryInvalidPreservedSourceCannotPruneFallback(t *testing.T) {
	base := t.TempDir()
	backupsRoot := filepath.Join(base, "backups")
	nativeRoot := filepath.Join(base, "native")
	source := filepath.Join(nativeRoot, "state.json")
	require.NoError(t, os.MkdirAll(nativeRoot, 0o700))
	require.NoError(t, os.WriteFile(source, []byte("fallback state"), 0o600))

	fallbackDir := filepath.Join(backupsRoot, "pre-restore-2026-07-18T01-00-00Z")
	_, err := Snapshot([]AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, fallbackDir)
	require.NoError(t, err)
	preservedDir := filepath.Join(backupsRoot, "pre-restore-2026-07-18T02-00-00Z")
	preservedManifest, err := Snapshot([]AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, preservedDir)
	require.NoError(t, err)
	require.Len(t, preservedManifest.Agents[0].Roots, 1)
	require.NoError(t, os.Remove(filepath.Join(preservedDir, filepath.FromSlash(preservedManifest.Agents[0].Roots[0].Path))))

	removed, err := PrunePreRestoreHistory(context.Background(), backupsRoot, 0, preservedDir)
	require.ErrorContains(t, err, "preserved pre-restore snapshot")
	require.Zero(t, removed)
	require.DirExists(t, preservedDir, "the requested invalid source is retained for diagnosis")
	require.DirExists(t, fallbackDir, "a failed restore attempt must not delete a valid fallback")
}

func TestRestoreWithOptionsRemovesFailedPreRestoreAllocationBeforeMutation(t *testing.T) {
	base := t.TempDir()
	nativeRoot := filepath.Join(base, "native", "agent")
	livePath := filepath.Join(nativeRoot, "state.json")
	require.NoError(t, os.MkdirAll(nativeRoot, 0o700))
	require.NoError(t, os.WriteFile(livePath, []byte("snapshot"), 0o600))
	backupDir := filepath.Join(base, "backups", "manual-source")
	_, err := SnapshotAuthenticated([]AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, backupDir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(livePath, []byte("live-must-survive"), 0o600))

	_, err = RestoreWithOptions(context.Background(), backupDir, NativeRestoreOptions{
		Agent:             "agent",
		CurrentAgentRoots: []AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}},
		Coordinator:       LocalRestoreCoordinator{LockPath: filepath.Join(base, "state", "native-restore.lock")},
		snapshotPreRestore: func(_ context.Context, _ []AgentRoots, preDir, _ string) error {
			require.NoError(t, os.MkdirAll(preDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(preDir, "partial.bin"), []byte("large partial copy"), 0o600))
			return errors.New("injected pre-restore manifest failure")
		},
	})
	require.ErrorContains(t, err, "injected pre-restore manifest failure")
	got, readErr := os.ReadFile(livePath)
	require.NoError(t, readErr)
	require.Equal(t, "live-must-survive", string(got))

	entries, readErr := os.ReadDir(filepath.Dir(backupDir))
	require.NoError(t, readErr)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), RestorePrefix, "failed allocation must be removed before returning")
	}
}

func TestRestoreWithOptionsBoundsSuccessfulPreRestoreHistory(t *testing.T) {
	base := t.TempDir()
	nativeRoot := filepath.Join(base, "native", "agent")
	livePath := filepath.Join(nativeRoot, "state.json")
	require.NoError(t, os.MkdirAll(nativeRoot, 0o700))
	require.NoError(t, os.WriteFile(livePath, []byte("snapshot"), 0o600))
	backupDir := filepath.Join(base, "backups", "manual-source")
	_, err := SnapshotAuthenticated([]AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, backupDir)
	require.NoError(t, err)

	for i := 0; i < MaxPreRestoreSnapshots+3; i++ {
		require.NoError(t, os.WriteFile(livePath, []byte{byte('a' + i)}, 0o600))
		_, err := RestoreWithOptions(context.Background(), backupDir, NativeRestoreOptions{
			Agent:             "agent",
			CurrentAgentRoots: []AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}},
			Coordinator:       LocalRestoreCoordinator{LockPath: filepath.Join(base, "state", "native-restore.lock")},
		})
		require.NoError(t, err)
	}

	entries, err := os.ReadDir(filepath.Dir(backupDir))
	require.NoError(t, err)
	count := 0
	for _, entry := range entries {
		if kind, ok := SnapshotKindFromID(entry.Name()); ok && kind == "pre-restore" {
			count++
		}
	}
	require.LessOrEqual(t, count, MaxPreRestoreSnapshots)
}
