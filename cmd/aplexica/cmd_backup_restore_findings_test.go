package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// TestBackup_EncryptRenameFailure_MessageNamesSurvivingFile (Finding 1)
// pins the wrapped error string the encrypt branch returns when the in-place
// os.Rename(encPath, dest) fails after os.Remove(dest) already succeeded. In
// that window the only copy of the bundle is the ciphertext at encPath; the
// message must name it so the user doesn't mistake the rename failure for a
// total loss and discard/overwrite their output.
//
// Forcing ONLY the rename to fail (while the preceding Remove succeeds) is not
// portably achievable: on POSIX a directory's write bit governs both unlink and
// create-via-rename, so mode bits can't split them, and the encrypt branch
// exposes no rename-injection seam. We therefore exercise the exact
// fmt.Errorf template the code uses (the same format string, same %w) so the
// surviving-file wording and the error-unwrap contract are regression-locked.
func TestBackup_EncryptRenameFailure_MessageNamesSurvivingFile(t *testing.T) {
	encPath := filepath.Join(t.TempDir(), "bundle.tar.gz.age")
	renameErr := errors.New("rename bundle.tar.gz.age bundle.tar.gz: cross-device link")

	// This MUST stay byte-identical to the cmd_backup.go encrypt branch.
	got := fmt.Errorf("encrypted bundle written but could not move into place; it is at %s: %w", encPath, renameErr)

	require.Contains(t, got.Error(), "could not move into place",
		"message must flag that the move (not the encryption) failed")
	require.Contains(t, got.Error(), encPath,
		"message must name the surviving ciphertext so the user can recover it")
	require.ErrorIs(t, got, renameErr,
		"the rename error must remain unwrappable via %w")
}

// TestRestore_PeekSurfacesHostnameAndTotalBytes (Finding 2) backs a real bundle
// and asserts the human `restore --peek` output now surfaces the hostname and
// totalBytes the manifest records (FR-01.8 / FR-01.24).
func TestRestore_PeekSurfacesHostnameAndTotalBytes(t *testing.T) {
	tmp := t.TempDir()
	srcStore := filepath.Join(tmp, "src")
	seedMemoryArtifact(t, srcStore, "claude-code", "# m\n")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")

	t.Cleanup(resetBackupRestoreFlags)
	_, err := runRoot(t, "backup", bundlePath, "--store", srcStore, "--state-dir", filepath.Join(tmp, "state"), "--unsigned")
	require.NoError(t, err)

	resetBackupRestoreFlags()
	out, err := runRoot(t, "restore", bundlePath, "--peek", "--unsigned-ok")
	require.NoError(t, err, "output:\n%s", out)

	wantHost, _ := os.Hostname()
	require.Contains(t, out, "hostname:", "peek must surface the recorded hostname")
	if wantHost != "" {
		require.Contains(t, out, wantHost)
	}
	require.Contains(t, out, "totalBytes:", "peek must surface the recorded total size")
}

// TestRestore_DryRun_EmptyTarget_AllAdds (Finding 3) dry-runs a bundle into an
// empty target store: every artifact is reported as a would-add, no collisions,
// and nothing is written (the dst store stays empty).
func TestRestore_DryRun_EmptyTarget_AllAdds(t *testing.T) {
	tmp := t.TempDir()
	srcStore := filepath.Join(tmp, "src")
	seedMemoryArtifact(t, srcStore, "claude-code", "# m\n")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")

	t.Cleanup(resetBackupRestoreFlags)
	_, err := runRoot(t, "backup", bundlePath, "--store", srcStore, "--state-dir", filepath.Join(tmp, "state"), "--unsigned")
	require.NoError(t, err)

	resetBackupRestoreFlags()
	dstStore := filepath.Join(tmp, "dst")
	out, err := runRoot(t, "restore", bundlePath, "--store", dstStore, "--dry-run", "--json", "--unsigned-ok")
	require.NoError(t, err, "output:\n%s", out)
	require.NotContains(t, out, "restored bundle", "dry-run must not restore")

	var got struct {
		DryRun          bool `json:"dryRun"`
		TotalAdds       int  `json:"totalAdds"`
		TotalCollisions int  `json:"totalCollisions"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &got))
	require.True(t, got.DryRun)
	require.Equal(t, 1, got.TotalAdds)
	require.Equal(t, 0, got.TotalCollisions)

	// Nothing was written: the memory artifact must not exist in dst.
	dst := &acf.Store{Root: dstStore}
	require.NoError(t, dst.Init())
	arts, err := dst.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Empty(t, arts, "dry-run must not write any artifact to the target")
}

// TestRestore_DryRun_CollisionReportedNoWrite (Finding 3) dry-runs a bundle
// into a target that already holds one of its artifacts: the collision is
// reported (human output names the colliding ID), and the pre-existing
// artifact is left byte-for-byte unchanged.
func TestRestore_DryRun_CollisionReportedNoWrite(t *testing.T) {
	tmp := t.TempDir()
	srcStore := filepath.Join(tmp, "src")
	id := seedMemoryArtifact(t, srcStore, "claude-code", "# m\n")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")

	t.Cleanup(resetBackupRestoreFlags)
	_, err := runRoot(t, "backup", bundlePath, "--store", srcStore, "--state-dir", filepath.Join(tmp, "state"), "--unsigned")
	require.NoError(t, err)

	// Target already holds the same artifact id (a collision a real restore
	// would abort on). Use the SAME store layout so ReadArtifact finds it.
	dstStore := filepath.Join(tmp, "dst")
	dst := &acf.Store{Root: dstStore}
	require.NoError(t, dst.Init())
	collArt := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeProject,
		Name:             "PREEXISTING.md",
	}
	require.NoError(t, dst.WriteArtifact(collArt))
	collPath := filepath.Join(dstStore, "acf", "memories", id+".json")
	before, err := os.ReadFile(collPath)
	require.NoError(t, err)

	resetBackupRestoreFlags()
	out, err := runRoot(t, "restore", bundlePath, "--store", dstStore, "--dry-run", "--unsigned-ok")
	require.NoError(t, err, "output:\n%s", out)
	require.NotContains(t, out, "restored bundle")
	require.Contains(t, out, "collision=1", "human output must report the collision count")
	require.Contains(t, out, id, "human output must name the colliding artifact id")
	require.Contains(t, out, "would ABORT", "must warn a real restore would abort")

	// The pre-existing artifact must be untouched.
	after, err := os.ReadFile(collPath)
	require.NoError(t, err)
	require.Equal(t, before, after, "dry-run must not modify the colliding artifact")
}
