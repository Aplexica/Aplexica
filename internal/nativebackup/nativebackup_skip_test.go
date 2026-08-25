//go:build !windows

package nativebackup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSnapshot_UnreadableFileSkippedNotFatal: a *regular* file that exists but
// is unreadable (e.g. a permission-restricted file inside an agent root, mode
// 0o000) must not abort the whole snapshot. FR-01.5 requires export to be safe
// to run while the agent is actively in use; one EACCES file should be recorded
// as skipped while every other file is still backed up.
func TestSnapshot_UnreadableFileSkippedNotFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses 0o000 permissions; cannot simulate an unreadable file")
	}
	workspace := t.TempDir()
	dest := filepath.Join(t.TempDir(), "pre-sync")

	root := nativeRoot(t, workspace, "agentroot", map[string]string{
		"readable.txt": "i am fine",
		"locked.txt":   "you cannot read me",
	})
	// Make one regular file unreadable. os.Open will fail with EACCES (a
	// non-IsNotExist error), which previously aborted the ENTIRE snapshot.
	locked := filepath.Join(root, "locked.txt")
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) }) // so t.TempDir cleanup can remove it

	man, err := Snapshot([]AgentRoots{{Name: "a", Roots: []string{root}}}, dest)
	require.NoError(t, err, "an unreadable file in the tree must not fail the snapshot")

	// The readable file was still copied.
	require.Len(t, man.Agents[0].Roots, 1, "only the readable file is in the manifest")
	require.Equal(t, "readable.txt", filepath.Base(man.Agents[0].Roots[0].Path))
	require.Equal(t, "i am fine", string(mustReadCopy(t, dest, man.Agents[0].Roots[0].Path)))

	// The unreadable file is recorded as skipped (not silently lost).
	require.Len(t, man.Agents[0].Skipped, 1, "the unreadable file must be recorded as skipped")
	require.Equal(t, "locked.txt", filepath.Base(man.Agents[0].Skipped[0].Path))
	require.NotEmpty(t, man.Agents[0].Skipped[0].Reason)

	// The skip survives a manifest round-trip.
	onDisk, err := ReadManifest(dest)
	require.NoError(t, err)
	require.Len(t, onDisk.Agents[0].Skipped, 1)
	require.Equal(t, "locked.txt", filepath.Base(onDisk.Agents[0].Skipped[0].Path))
}

// TestSnapshot_VanishedFileSkippedNotFatal: a regular file that is deleted
// between the directory walk and the per-file open (a realistic TOCTOU race
// under an actively-running agent that constantly creates and deletes transient
// files) must be skipped, not abort the snapshot. We force the race by making
// copyFile observe a path that no longer exists.
func TestSnapshot_VanishedFileSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out", "gone.txt")
	gone := filepath.Join(dir, "gone.txt") // never created → os.Open → IsNotExist

	entry, skipped, err := copyFile(gone, dst, dir)
	require.NoError(t, err, "a vanished source file must not be a fatal error")
	require.Nil(t, skipped, "a vanished file is dropped like a missing root, not recorded")
	require.Equal(t, FileEntry{}, entry, "no manifest entry for a vanished file")
}
