package nativebackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/stretchr/testify/require"
)

type sanitizeFixture struct {
	base       string
	nativeRoot string
	backupDir  string
	policy     AgentRoots
}

func newSanitizeFixture(t *testing.T) sanitizeFixture {
	t.Helper()
	base := t.TempDir()
	nativeRoot := filepath.Join(base, "native", "agent")
	files := map[string]string{
		"keep.txt":                 "keep-original",
		"cache/drop.bin":           "explicit-cache",
		"cache2/keep.bin":          "prefix-boundary",
		"node_modules/drop.js":     "generated-dependency",
		".git/objects/unpublished": "unpublished-history",
	}
	for rel, value := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(nativeRoot, rel)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(nativeRoot, rel), []byte(value), 0o600))
	}
	backupDir := filepath.Join(base, "backups", "manual-old")
	man, err := SnapshotAuthenticated([]AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, backupDir)
	require.NoError(t, err)
	// Current snapshots already omit generic dependency trees. Add a correctly
	// signed node_modules entry to model an authenticated snapshot made by a
	// pre-policy release.
	addFileToSignedSnapshot(t, backupDir, &man, "agent", nativeRoot, "node_modules/drop.js")
	rootPrefix := "agent/" + filepath.ToSlash(relativize(nativeRoot))
	man.Agents[0].Skipped = append(man.Agents[0].Skipped,
		SkippedFile{Path: rootPrefix + "/cache/unreadable", Reason: "old unreadable cache"},
		SkippedFile{Path: rootPrefix + "/notes/missing", Reason: "old unreadable user note"},
	)
	require.NoError(t, SignDefaultManifest(&man, backupDir))
	require.NoError(t, writeManifest(backupDir, man))
	return sanitizeFixture{
		base:       base,
		nativeRoot: nativeRoot,
		backupDir:  backupDir,
		policy: AgentRoots{
			Name:         "agent",
			Roots:        []string{nativeRoot},
			ExcludePaths: []string{filepath.Join(nativeRoot, "cache")},
		},
	}
}

func addFileToSignedSnapshot(t *testing.T, backupDir string, man *Manifest, agent, nativeRoot, rel string) {
	t.Helper()
	source := filepath.Join(nativeRoot, filepath.FromSlash(rel))
	b, err := os.ReadFile(source)
	require.NoError(t, err)
	destination := filepath.Join(backupDir, agent, relativize(nativeRoot), filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(destination), 0o700))
	require.NoError(t, os.WriteFile(destination, b, 0o600))
	relative, err := filepath.Rel(backupDir, destination)
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	entry := FileEntry{Path: filepath.ToSlash(relative), Bytes: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}
	for i := range man.Agents {
		if man.Agents[i].Name == agent {
			man.Agents[i].Roots = append(man.Agents[i].Roots, entry)
			sort.Slice(man.Agents[i].Roots, func(a, b int) bool { return man.Agents[i].Roots[a].Path < man.Agents[i].Roots[b].Path })
			return
		}
	}
	t.Fatalf("agent %q not found", agent)
}

func sanitizeTransactionNames(t *testing.T, backupsRoot string) []string {
	t.Helper()
	entries, err := os.ReadDir(backupsRoot)
	require.NoError(t, err)
	var out []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), sanitizeTransactionPrefix) {
			out = append(out, entry.Name())
		}
	}
	return out
}

func TestSanitizeSnapshot_RebuildsAuthenticatedSnapshotAndRemainsRestorable(t *testing.T) {
	fx := newSanitizeFixture(t)
	original, err := ReadManifest(fx.backupDir)
	require.NoError(t, err)
	wantModTime := time.Unix(1_700_000_000, 123_000_000)
	require.NoError(t, os.Chtimes(fx.backupDir, wantModTime, wantModTime))

	result, err := SanitizeSnapshotContext(context.Background(), fx.backupDir, SanitizeOptions{CurrentAgentRoots: []AgentRoots{fx.policy}})
	require.NoError(t, err)
	require.Equal(t, SanitizeComplete, result.Status)
	require.Equal(t, 2, result.RemovedFiles)
	require.Equal(t, int64(len("explicit-cache")+len("generated-dependency")), result.RemovedBytes)
	require.Equal(t, 1, result.RemovedSkipped)
	require.Empty(t, sanitizeTransactionNames(t, filepath.Dir(fx.backupDir)))

	man, err := ReadManifest(fx.backupDir)
	require.NoError(t, err)
	require.Equal(t, 2, man.SchemaVersion)
	require.Equal(t, original.CreatedAt, man.CreatedAt)
	require.Equal(t, original.AplexicaVersion, man.AplexicaVersion)
	require.NoError(t, VerifyDefaultManifest(man, fx.backupDir))
	_, err = verifyAuthenticatedSnapshot(context.Background(), fx.backupDir, manifestKeyPathForBackupDir(fx.backupDir), true)
	require.NoError(t, err)
	info, err := os.Stat(fx.backupDir)
	require.NoError(t, err)
	require.WithinDuration(t, wantModTime, info.ModTime(), time.Millisecond)

	mirrored := filepath.Join(fx.backupDir, "agent", relativize(fx.nativeRoot))
	for _, removed := range []string{"cache/drop.bin", "node_modules/drop.js"} {
		_, err := os.Stat(filepath.Join(mirrored, filepath.FromSlash(removed)))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	for rel, want := range map[string]string{
		"keep.txt":                 "keep-original",
		"cache2/keep.bin":          "prefix-boundary",
		".git/objects/unpublished": "unpublished-history",
	} {
		got, err := os.ReadFile(filepath.Join(mirrored, filepath.FromSlash(rel)))
		require.NoError(t, err)
		require.Equal(t, want, string(got))
	}
	require.Len(t, man.Agents[0].Skipped, 1)
	require.Contains(t, man.Agents[0].Skipped[0].Path, "/notes/missing")

	// A sanitized snapshot remains an ordinary reversible restore point. Current
	// excluded runtime stays live, while retained user state and unpublished Git
	// history are restored.
	require.NoError(t, os.WriteFile(filepath.Join(fx.nativeRoot, "keep.txt"), []byte("live-keep"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(fx.nativeRoot, "cache", "drop.bin"), []byte("live-cache"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(fx.nativeRoot, ".git", "objects", "unpublished"), []byte("live-git"), 0o600))
	restore, err := RestoreWithOptions(context.Background(), fx.backupDir, NativeRestoreOptions{
		Agent:             "agent",
		CurrentAgentRoots: []AgentRoots{fx.policy},
		Coordinator:       LocalRestoreCoordinator{LockPath: filepath.Join(fx.base, "state", "native-restore.lock")},
	})
	require.NoError(t, err)
	require.NotEmpty(t, restore.PreRestoreDir)
	got, err := os.ReadFile(filepath.Join(fx.nativeRoot, "keep.txt"))
	require.NoError(t, err)
	require.Equal(t, "keep-original", string(got))
	got, err = os.ReadFile(filepath.Join(fx.nativeRoot, "cache", "drop.bin"))
	require.NoError(t, err)
	require.Equal(t, "live-cache", string(got))
	got, err = os.ReadFile(filepath.Join(fx.nativeRoot, ".git", "objects", "unpublished"))
	require.NoError(t, err)
	require.Equal(t, "unpublished-history", string(got))
}

func TestSanitizeSnapshot_AlreadySanitizedIsIdentityPreservingNoOp(t *testing.T) {
	base := t.TempDir()
	nativeRoot := filepath.Join(base, "native", "agent")
	require.NoError(t, os.MkdirAll(filepath.Join(nativeRoot, "cache"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(nativeRoot, "keep"), []byte("keep"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(nativeRoot, "cache", "drop"), []byte("drop"), 0o600))
	policy := AgentRoots{Name: "agent", Roots: []string{nativeRoot}, ExcludePaths: []string{filepath.Join(nativeRoot, "cache")}}
	backup := filepath.Join(base, "backups", "manual-clean")
	_, err := SnapshotAuthenticated([]AgentRoots{policy}, backup)
	require.NoError(t, err)
	beforeManifest, err := os.ReadFile(filepath.Join(backup, ManifestName))
	require.NoError(t, err)
	beforeHandle, err := os.Open(backup)
	require.NoError(t, err)
	beforeInfo, err := beforeHandle.Stat()
	require.NoError(t, err)
	require.NoError(t, beforeHandle.Close())

	result, err := SanitizeSnapshotContext(context.Background(), backup, SanitizeOptions{CurrentAgentRoots: []AgentRoots{policy}})
	require.NoError(t, err)
	require.Equal(t, SanitizeUnchanged, result.Status)
	afterManifest, err := os.ReadFile(filepath.Join(backup, ManifestName))
	require.NoError(t, err)
	require.Equal(t, beforeManifest, afterManifest)
	afterHandle, err := os.Open(backup)
	require.NoError(t, err)
	afterInfo, err := afterHandle.Stat()
	require.NoError(t, err)
	require.NoError(t, afterHandle.Close())
	require.True(t, os.SameFile(beforeInfo, afterInfo), "no-op must not replace the directory")
	require.Empty(t, sanitizeTransactionNames(t, filepath.Dir(backup)))
}

func TestSanitizeSnapshot_RefusesTamperingBeforeAnyChange(t *testing.T) {
	t.Run("manifest auth failure even when policy is no-op", func(t *testing.T) {
		base := t.TempDir()
		nativeRoot := filepath.Join(base, "native", "agent")
		require.NoError(t, os.MkdirAll(nativeRoot, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(nativeRoot, "keep"), []byte("keep"), 0o600))
		backup := filepath.Join(base, "backups", "manual-auth-failure")
		man, err := SnapshotAuthenticated([]AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, backup)
		require.NoError(t, err)
		man.CreatedAt = man.CreatedAt.Add(time.Second)
		require.NoError(t, writeManifest(backup, man))
		tampered, err := os.ReadFile(filepath.Join(backup, ManifestName))
		require.NoError(t, err)

		_, err = SanitizeSnapshotContext(context.Background(), backup, SanitizeOptions{CurrentAgentRoots: []AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}})
		require.ErrorContains(t, err, "authenticate snapshot")
		after, readErr := os.ReadFile(filepath.Join(backup, ManifestName))
		require.NoError(t, readErr)
		require.Equal(t, tampered, after)
		require.Empty(t, sanitizeTransactionNames(t, filepath.Dir(backup)))
	})

	t.Run("excluded file digest failure", func(t *testing.T) {
		fx := newSanitizeFixture(t)
		copyPath := filepath.Join(fx.backupDir, "agent", relativize(fx.nativeRoot), "cache", "drop.bin")
		require.NoError(t, os.WriteFile(copyPath, []byte("tampered-cache"), 0o600))
		before, err := os.ReadFile(copyPath)
		require.NoError(t, err)

		_, err = SanitizeSnapshotContext(context.Background(), fx.backupDir, SanitizeOptions{CurrentAgentRoots: []AgentRoots{fx.policy}})
		require.ErrorContains(t, err, "verify original snapshot")
		after, readErr := os.ReadFile(copyPath)
		require.NoError(t, readErr)
		require.Equal(t, before, after)
		require.Empty(t, sanitizeTransactionNames(t, filepath.Dir(fx.backupDir)), "sanitize error: %v", err)
	})
}

func TestSanitizeSnapshot_RechecksOriginalAndRetainedKeyBeforeRename(t *testing.T) {
	t.Run("source mutation during rebuild", func(t *testing.T) {
		fx := newSanitizeFixture(t)
		beforeInfo, err := os.Stat(fx.backupDir)
		require.NoError(t, err)
		unexpected := filepath.Join(fx.backupDir, "agent", relativize(fx.nativeRoot), "unexpected.txt")
		_, err = sanitizeSnapshotContext(context.Background(), fx.backupDir, SanitizeOptions{CurrentAgentRoots: []AgentRoots{fx.policy}}, &sanitizeTestHooks{
			after: func(step sanitizeStep) error {
				if step != sanitizeStepBeforeOriginalVerify {
					return nil
				}
				return os.WriteFile(unexpected, []byte("arrived-during-rebuild"), 0o600)
			},
		})
		require.ErrorContains(t, err, "original changed during sanitize rebuild")
		afterInfo, statErr := os.Stat(fx.backupDir)
		require.NoError(t, statErr)
		require.True(t, os.SameFile(beforeInfo, afterInfo), "original directory must never be swapped after a late write")
		got, readErr := os.ReadFile(unexpected)
		require.NoError(t, readErr)
		require.Equal(t, "arrived-during-rebuild", string(got))
		require.Empty(t, sanitizeTransactionNames(t, filepath.Dir(fx.backupDir)), "sanitize error: %v", err)
	})

	t.Run("manifest key substitution", func(t *testing.T) {
		fx := newSanitizeFixture(t)
		keyPath := manifestKeyPathForBackupDir(fx.backupDir)
		originalKey, err := os.ReadFile(keyPath)
		require.NoError(t, err)
		require.Len(t, originalKey, 32)
		beforeInfo, err := os.Stat(fx.backupDir)
		require.NoError(t, err)
		replacement := bytes.Repeat([]byte{0xa5}, 32)

		_, err = sanitizeSnapshotContext(context.Background(), fx.backupDir, SanitizeOptions{CurrentAgentRoots: []AgentRoots{fx.policy}}, &sanitizeTestHooks{
			after: func(step sanitizeStep) error {
				if step != sanitizeStepBeforeOriginalVerify {
					return nil
				}
				return os.WriteFile(keyPath, replacement, 0o600)
			},
		})
		require.ErrorContains(t, err, "manifest key changed during sanitize")
		afterInfo, statErr := os.Stat(fx.backupDir)
		require.NoError(t, statErr)
		require.True(t, os.SameFile(beforeInfo, afterInfo))
		require.Empty(t, sanitizeTransactionNames(t, filepath.Dir(fx.backupDir)))

		// Restore the fixture key only so this test can prove the untouched
		// original still authenticates; production correctly treats a replaced
		// key as a wider backup-integrity incident.
		require.NoError(t, os.WriteFile(keyPath, originalKey, 0o600))
		man, err := ReadManifest(fx.backupDir)
		require.NoError(t, err)
		require.NoError(t, VerifyDefaultManifest(man, fx.backupDir))
	})
}

func TestSanitizeSnapshot_LegacySnapshotRemainsByteForByteUnsigned(t *testing.T) {
	base := t.TempDir()
	nativeRoot := filepath.Join(base, "native", "agent")
	require.NoError(t, os.MkdirAll(filepath.Join(nativeRoot, "cache"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(nativeRoot, "cache", "drop"), []byte("drop"), 0o600))
	backup := filepath.Join(base, "backups", "manual-legacy")
	_, err := Snapshot([]AgentRoots{{Name: "agent", Roots: []string{nativeRoot}}}, backup)
	require.NoError(t, err)
	before, err := os.ReadFile(filepath.Join(backup, ManifestName))
	require.NoError(t, err)

	result, err := SanitizeSnapshotContext(context.Background(), backup, SanitizeOptions{CurrentAgentRoots: []AgentRoots{{Name: "agent", Roots: []string{nativeRoot}, ExcludePaths: []string{filepath.Join(nativeRoot, "cache")}}}})
	require.NoError(t, err)
	require.Equal(t, SanitizeLegacySkipped, result.Status)
	after, err := os.ReadFile(filepath.Join(backup, ManifestName))
	require.NoError(t, err)
	require.Equal(t, before, after)
	_, err = os.Stat(filepath.Join(base, "keys", "native-manifest-hmac-v2"))
	require.ErrorIs(t, err, os.ErrNotExist, "legacy skip must not manufacture an authentication key")
	require.Empty(t, sanitizeTransactionNames(t, filepath.Dir(backup)))
}

func TestSanitizeSnapshot_CrashRecoveryIsIdempotent(t *testing.T) {
	preCommit := []sanitizeStep{
		sanitizeStepJournalDurable,
		sanitizeStepRebuiltVerified,
		sanitizeStepOriginalRenamed,
		sanitizeStepMoveRecorded,
		sanitizeStepRebuiltInstalled,
		sanitizeStepInstallRecorded,
	}
	for _, step := range preCommit {
		t.Run(string(step), func(t *testing.T) {
			fx := newSanitizeFixture(t)
			crash := errors.New("simulated crash")
			_, err := sanitizeSnapshotContext(context.Background(), fx.backupDir, SanitizeOptions{CurrentAgentRoots: []AgentRoots{fx.policy}}, &sanitizeTestHooks{
				leaveOnError: true,
				after: func(got sanitizeStep) error {
					if got == step {
						return crash
					}
					return nil
				},
			})
			require.ErrorIs(t, err, crash)
			require.NotEmpty(t, sanitizeTransactionNames(t, filepath.Dir(fx.backupDir)))

			fast, err := RecoverSanitizeTransactionsContext(context.Background(), filepath.Dir(fx.backupDir), "", false)
			require.NoError(t, err)
			require.GreaterOrEqual(t, fast.Recovered+fast.Pending, 1)
			mirrored := filepath.Join(fx.backupDir, "agent", relativize(fx.nativeRoot))
			got, err := os.ReadFile(filepath.Join(mirrored, "cache", "drop.bin"))
			require.NoError(t, err)
			require.Equal(t, "explicit-cache", string(got), "pre-commit recovery must restore the original tree")

			_, err = RecoverSanitizeTransactionsContext(context.Background(), filepath.Dir(fx.backupDir), "", true)
			require.NoError(t, err)
			require.Empty(t, sanitizeTransactionNames(t, filepath.Dir(fx.backupDir)))
			again, err := RecoverSanitizeTransactionsContext(context.Background(), filepath.Dir(fx.backupDir), "", true)
			require.NoError(t, err)
			require.Equal(t, SanitizeRecoveryResult{}, again)
		})
	}

	t.Run("committed rolls forward", func(t *testing.T) {
		fx := newSanitizeFixture(t)
		crash := errors.New("simulated crash")
		_, err := sanitizeSnapshotContext(context.Background(), fx.backupDir, SanitizeOptions{CurrentAgentRoots: []AgentRoots{fx.policy}}, &sanitizeTestHooks{
			leaveOnError: true,
			after: func(got sanitizeStep) error {
				if got == sanitizeStepCommitted {
					return crash
				}
				return nil
			},
		})
		require.ErrorIs(t, err, crash)
		_, err = os.Stat(filepath.Join(fx.backupDir, "agent", relativize(fx.nativeRoot), "cache", "drop.bin"))
		require.ErrorIs(t, err, os.ErrNotExist)
		fast, err := RecoverSanitizeTransactionsContext(context.Background(), filepath.Dir(fx.backupDir), "", false)
		require.NoError(t, err)
		require.Equal(t, 1, fast.Pending)
		full, err := RecoverSanitizeTransactionsContext(context.Background(), filepath.Dir(fx.backupDir), "", true)
		require.NoError(t, err)
		require.Equal(t, 1, full.Finalized)
		require.Empty(t, sanitizeTransactionNames(t, filepath.Dir(fx.backupDir)))
	})
}

func TestSanitizeSnapshot_OrdinarySwapFailureRollsBackImmediately(t *testing.T) {
	fx := newSanitizeFixture(t)
	failure := errors.New("rename boundary failure")
	_, err := sanitizeSnapshotContext(context.Background(), fx.backupDir, SanitizeOptions{CurrentAgentRoots: []AgentRoots{fx.policy}}, &sanitizeTestHooks{
		after: func(got sanitizeStep) error {
			if got == sanitizeStepOriginalRenamed {
				return failure
			}
			return nil
		},
	})
	require.ErrorIs(t, err, failure)
	got, readErr := os.ReadFile(filepath.Join(fx.backupDir, "agent", relativize(fx.nativeRoot), "cache", "drop.bin"))
	require.NoError(t, readErr)
	require.Equal(t, "explicit-cache", string(got))
	require.Empty(t, sanitizeTransactionNames(t, filepath.Dir(fx.backupDir)))
}

func TestSanitizeSnapshot_PostRenameSyncErrorUsesPresenceBasedRollback(t *testing.T) {
	fx := newSanitizeFixture(t)
	failure := errors.New("destination directory sync failed after rename")
	first := true
	_, err := sanitizeSnapshotContext(context.Background(), fx.backupDir, SanitizeOptions{CurrentAgentRoots: []AgentRoots{fx.policy}}, &sanitizeTestHooks{
		rename: func(root *privatefs.Root, oldPath, newPath string) error {
			if !first {
				return root.Rename(oldPath, newPath)
			}
			first = false
			// Model privatefs.Root.Rename's dangerous error contract: the
			// namespace move succeeded, but its destination SyncDir failed.
			require.NoError(t, root.Rename(oldPath, newPath))
			return failure
		},
	})
	require.ErrorIs(t, err, failure)
	got, readErr := os.ReadFile(filepath.Join(fx.backupDir, "agent", relativize(fx.nativeRoot), "cache", "drop.bin"))
	require.NoError(t, readErr)
	require.Equal(t, "explicit-cache", string(got), "the only original tree must be restored")
	require.Empty(t, sanitizeTransactionNames(t, filepath.Dir(fx.backupDir)))
}

func TestSanitizeSnapshot_CancellationAfterRenameStillRollsBack(t *testing.T) {
	fx := newSanitizeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	_, err := sanitizeSnapshotContext(ctx, fx.backupDir, SanitizeOptions{CurrentAgentRoots: []AgentRoots{fx.policy}}, &sanitizeTestHooks{
		after: func(step sanitizeStep) error {
			if step == sanitizeStepOriginalRenamed {
				cancel()
			}
			return nil
		},
	})
	require.ErrorIs(t, err, context.Canceled)
	got, readErr := os.ReadFile(filepath.Join(fx.backupDir, "agent", relativize(fx.nativeRoot), "cache", "drop.bin"))
	require.NoError(t, readErr)
	require.Equal(t, "explicit-cache", string(got), "rollback must ignore caller cancellation once the original moved")
	require.Empty(t, sanitizeTransactionNames(t, filepath.Dir(fx.backupDir)))
}

func TestSanitizeSnapshot_PostMoveMutationIsNeverOverwritten(t *testing.T) {
	fx := newSanitizeFixture(t)
	mutation := []byte("changed-after-rename")
	_, err := sanitizeSnapshotContext(context.Background(), fx.backupDir, SanitizeOptions{CurrentAgentRoots: []AgentRoots{fx.policy}}, &sanitizeTestHooks{
		after: func(step sanitizeStep) error {
			if step != sanitizeStepOriginalRenamed {
				return nil
			}
			txNames := sanitizeTransactionNames(t, filepath.Dir(fx.backupDir))
			require.Len(t, txNames, 1)
			movedKeep := filepath.Join(filepath.Dir(fx.backupDir), txNames[0], sanitizeOriginalName, "agent", relativize(fx.nativeRoot), "keep.txt")
			return os.WriteFile(movedKeep, mutation, 0o600)
		},
	})
	require.ErrorContains(t, err, "original changed during sanitize rebuild")
	got, readErr := os.ReadFile(filepath.Join(fx.backupDir, "agent", relativize(fx.nativeRoot), "keep.txt"))
	require.NoError(t, readErr)
	require.Equal(t, mutation, got, "the mutated original must be restored, never replaced by older rebuilt bytes")
	require.NotEmpty(t, sanitizeTransactionNames(t, filepath.Dir(fx.backupDir)),
		"the verified rebuilt alternative remains journaled when the restored original no longer authenticates")
}

func TestSanitizeSnapshot_CleanupFailureRetainsCommittedRollbackForRetry(t *testing.T) {
	fx := newSanitizeFixture(t)
	removeFailure := errors.New("remove blocked")
	_, err := sanitizeSnapshotContext(context.Background(), fx.backupDir, SanitizeOptions{CurrentAgentRoots: []AgentRoots{fx.policy}}, &sanitizeTestHooks{
		removeTree: func(string) error { return removeFailure },
	})
	require.ErrorIs(t, err, removeFailure)
	require.NotEmpty(t, sanitizeTransactionNames(t, filepath.Dir(fx.backupDir)))
	_, err = os.Stat(filepath.Join(fx.backupDir, "agent", relativize(fx.nativeRoot), "cache", "drop.bin"))
	require.ErrorIs(t, err, os.ErrNotExist, "verified replacement remains installed after cleanup-only failure")

	result, err := RecoverSanitizeTransactionsContext(context.Background(), filepath.Dir(fx.backupDir), "", true)
	require.NoError(t, err)
	require.Equal(t, 1, result.Finalized)
	require.Empty(t, sanitizeTransactionNames(t, filepath.Dir(fx.backupDir)))
}

func TestSanitizeSnapshot_JournalSurvivesPartialChildCleanup(t *testing.T) {
	fx := newSanitizeFixture(t)
	crash := errors.New("simulated crash")
	_, err := sanitizeSnapshotContext(context.Background(), fx.backupDir, SanitizeOptions{CurrentAgentRoots: []AgentRoots{fx.policy}}, &sanitizeTestHooks{
		leaveOnError: true,
		after: func(step sanitizeStep) error {
			if step == sanitizeStepCommitted {
				return crash
			}
			return nil
		},
	})
	require.ErrorIs(t, err, crash)
	txNames := sanitizeTransactionNames(t, filepath.Dir(fx.backupDir))
	require.Len(t, txNames, 1)
	txName := txNames[0]
	// Model a stale rebuilt child left by an earlier interrupted cleanup so the
	// retry has two large children: the rebuilt child is removed, then removing
	// original fails. transaction.json must remain readable throughout.
	staleRebuilt := filepath.Join(filepath.Dir(fx.backupDir), txName, sanitizeRebuiltName)
	require.NoError(t, os.MkdirAll(staleRebuilt, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(staleRebuilt, "stale"), []byte("stale"), 0o600))

	root, err := privatefs.OpenRoot(filepath.Dir(fx.backupDir), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	require.NoError(t, err)
	key, err := loadManifestKey(manifestKeyPathForBackupDir(fx.backupDir), false)
	require.NoError(t, err)
	childFailure := errors.New("locked rollback child")
	_, err = recoverSanitizeTransaction(context.Background(), root, filepath.Dir(fx.backupDir), txName, manifestKeyPathForBackupDir(fx.backupDir), &key, true, &sanitizeTestHooks{
		removeTree: func(path string) error {
			if filepath.Base(path) == sanitizeOriginalName {
				return childFailure
			}
			return os.RemoveAll(path)
		},
	})
	require.ErrorIs(t, err, childFailure)
	_, err = readSanitizeJournal(root, txName)
	require.NoError(t, err, "journal must be deleted only after every child tree")
	rebuiltExists, err := realChildDirectoryExists(root, txName, sanitizeRebuiltName)
	require.NoError(t, err)
	require.False(t, rebuiltExists)
	originalExists, err := realChildDirectoryExists(root, txName, sanitizeOriginalName)
	require.NoError(t, err)
	require.True(t, originalExists)
	require.NoError(t, root.Close())

	result, err := RecoverSanitizeTransactionsContext(context.Background(), filepath.Dir(fx.backupDir), "", true)
	require.NoError(t, err)
	require.Equal(t, 1, result.Finalized)
	require.Empty(t, sanitizeTransactionNames(t, filepath.Dir(fx.backupDir)))
}

func TestRecoverSanitizeTransactions_RemovesBoundedInitialJournalTemp(t *testing.T) {
	backupsRoot := filepath.Join(t.TempDir(), "backups")
	require.NoError(t, os.MkdirAll(backupsRoot, 0o700))
	txName := sanitizeTransactionPrefix + strings.Repeat("a", 32)
	txDir := filepath.Join(backupsRoot, txName)
	require.NoError(t, os.MkdirAll(txDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(txDir, ".aplexica-write-crash-temp"), []byte(`{"partial":true}`), 0o600))

	result, err := RecoverSanitizeTransactionsContext(context.Background(), backupsRoot, "", true)
	require.NoError(t, err)
	require.Equal(t, 1, result.Finalized)
	require.NoDirExists(t, txDir)
}

func TestSanitizeSnapshot_KnownAgentPolicyAppliesWithoutCurrentDiscovery(t *testing.T) {
	base := t.TempDir()
	nativeRoot := filepath.Join(base, "native", "missing-agent")
	require.NoError(t, os.MkdirAll(nativeRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(nativeRoot, "keep.txt"), []byte("keep"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(nativeRoot, "credential.env"), []byte("TOKEN=secret"), 0o600))
	backup := filepath.Join(base, "backups", "manual-known-policy")
	_, err := SnapshotAuthenticated([]AgentRoots{{Name: "missing-agent", Roots: []string{nativeRoot}}}, backup)
	require.NoError(t, err)

	result, err := SanitizeSnapshotContext(context.Background(), backup, SanitizeOptions{
		KnownAgentExcludePaths: func(agent string, sourceRoots []string) []string {
			require.Equal(t, "missing-agent", agent)
			require.Equal(t, []string{nativeRoot}, sourceRoots)
			return []string{filepath.Join(sourceRoots[0], "credential.env")}
		},
	})
	require.NoError(t, err)
	require.Equal(t, SanitizeComplete, result.Status)
	mirrored := filepath.Join(backup, "missing-agent", relativize(nativeRoot))
	require.FileExists(t, filepath.Join(mirrored, "keep.txt"))
	require.NoFileExists(t, filepath.Join(mirrored, "credential.env"))
}

func TestSanitizeSnapshot_RedactsMixedOpenClawConfigAndResignsManifest(t *testing.T) {
	base := t.TempDir()
	nativeRoot := filepath.Join(base, "native", ".openclaw")
	configPath := filepath.Join(nativeRoot, "openclaw.json")
	raw := []byte(`{
  "channels": {"telegram": {"enabled": true, "botToken": "channel-secret"}},
  "models": {"primary": "openai/gpt-5", "apiKey": "model-secret"},
  "agents": {"worker": {"workspace": "/srv/workspace", "maxTurns": 8}},
  "mcp": {"servers": {"git": {"command": "uvx", "env": {"GITHUB_TOKEN": "env-secret", "RETRIES": 3}}}},
  "gateway": {"port": 18789, "auth": {"mode": "token", "token": "gateway-secret"}}
}`)
	require.NoError(t, os.MkdirAll(nativeRoot, 0o700))
	require.NoError(t, os.WriteFile(configPath, raw, 0o600))
	backup := filepath.Join(base, "backups", "manual-openclaw-old-policy")
	_, err := SnapshotAuthenticated([]AgentRoots{{Name: "openclaw", Roots: []string{nativeRoot}}}, backup)
	require.NoError(t, err)
	beforeHandle, err := os.Open(backup)
	require.NoError(t, err)
	beforeInfo, err := beforeHandle.Stat()
	require.NoError(t, err)
	require.NoError(t, beforeHandle.Close())

	options := SanitizeOptions{
		KnownAgentRedactions: func(agent string, sourceRoots []string) []FileRedaction {
			require.Equal(t, "openclaw", agent)
			require.Equal(t, []string{nativeRoot}, sourceRoots)
			return []FileRedaction{{Path: filepath.Join(sourceRoots[0], "openclaw.json"), Kind: FileRedactionOpenClawConfig}}
		},
	}
	result, err := SanitizeSnapshotContext(context.Background(), backup, options)
	require.NoError(t, err)
	require.Equal(t, SanitizeComplete, result.Status)
	require.Equal(t, 1, result.RedactedFiles)
	require.Zero(t, result.RemovedFiles)

	sourceAfter, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, raw, sourceAfter, "history cleanup must never edit the live OpenClaw config")
	backupConfig := filepath.Join(backup, "openclaw", relativize(nativeRoot), "openclaw.json")
	redacted, err := os.ReadFile(backupConfig)
	require.NoError(t, err)
	require.JSONEq(t, `{
  "channels": {"telegram": {"enabled": true, "botToken": ""}},
  "models": {"primary": "openai/gpt-5", "apiKey": ""},
  "agents": {"worker": {"workspace": "/srv/workspace", "maxTurns": 8}},
  "mcp": {"servers": {"git": {"command": "uvx", "env": {"GITHUB_TOKEN": "", "RETRIES": 3}}}},
  "gateway": {"port": 18789, "auth": {"mode": "token", "token": ""}}
}`, string(redacted))
	man, err := ReadManifest(backup)
	require.NoError(t, err)
	require.NoError(t, VerifyDefaultManifest(man, backup))
	_, err = verifyAuthenticatedSnapshot(context.Background(), backup, manifestKeyPathForBackupDir(backup), true)
	require.NoError(t, err)

	afterHandle, err := os.Open(backup)
	require.NoError(t, err)
	afterInfo, err := afterHandle.Stat()
	require.NoError(t, err)
	require.NoError(t, afterHandle.Close())
	require.False(t, os.SameFile(beforeInfo, afterInfo), "sanitization must replace the authenticated snapshot atomically")
	second, err := SanitizeSnapshotContext(context.Background(), backup, options)
	require.NoError(t, err)
	require.Equal(t, SanitizeUnchanged, second.Status)
	require.Zero(t, second.RedactedFiles)
	finalHandle, err := os.Open(backup)
	require.NoError(t, err)
	finalInfo, err := finalHandle.Stat()
	require.NoError(t, err)
	require.NoError(t, finalHandle.Close())
	require.True(t, os.SameFile(afterInfo, finalInfo), "already-redacted history must remain an identity-preserving no-op")
}

func TestSanitizeSnapshot_DropsUnredactableHistoricalMixedConfigWithoutTouchingLiveSource(t *testing.T) {
	base := t.TempDir()
	nativeRoot := filepath.Join(base, "native", ".openclaw")
	configPath := filepath.Join(nativeRoot, "openclaw.json")
	raw := []byte(`{"gateway":{"token":"raw-secret"},`)
	require.NoError(t, os.MkdirAll(nativeRoot, 0o700))
	require.NoError(t, os.WriteFile(configPath, raw, 0o600))
	backup := filepath.Join(base, "backups", "manual-openclaw-invalid-old-policy")
	_, err := SnapshotAuthenticated([]AgentRoots{{Name: "openclaw", Roots: []string{nativeRoot}}}, backup)
	require.NoError(t, err)

	result, err := SanitizeSnapshotContext(context.Background(), backup, SanitizeOptions{
		KnownAgentRedactions: func(string, []string) []FileRedaction {
			return []FileRedaction{{Path: configPath, Kind: FileRedactionOpenClawConfig}}
		},
	})
	require.NoError(t, err)
	require.Equal(t, SanitizeComplete, result.Status)
	require.Equal(t, 1, result.RemovedFiles)
	require.Equal(t, int64(len(raw)), result.RemovedBytes)
	require.NoFileExists(t, filepath.Join(backup, "openclaw", relativize(nativeRoot), "openclaw.json"))
	sourceAfter, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, raw, sourceAfter)
	man, err := ReadManifest(backup)
	require.NoError(t, err)
	require.NoError(t, VerifyDefaultManifest(man, backup))
}
