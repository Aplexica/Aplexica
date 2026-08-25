//go:build !windows

// Native snapshot/restore reconstructs absolute paths from a single
// filesystem root; multi-volume Windows path round-trip requires separate
// coverage, so these round-trip tests run on Unix only.
package nativebackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeFile creates path (and parents) with the given content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// nativeRoot builds a fake agent native tree under base and returns the root
// path. It mimics ~/.hermes with a config file, a nested logs dir, and a
// "large" session DB (we just write deterministic bytes, not a real DB).
func nativeRoot(t *testing.T, base, agentDir string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(base, agentDir)
	for rel, content := range files {
		writeFile(t, filepath.Join(root, rel), content)
	}
	return root
}

func TestSnapshot_WritesManifestAndCopiesTree(t *testing.T) {
	home := t.TempDir()
	dest := filepath.Join(t.TempDir(), "pre-sync-2026")

	files := map[string]string{
		"config.json":       `{"k":"v"}`,
		"logs/today.log":    "line1\nline2\n",
		"state.db":          strings.Repeat("DBPAGE", 4096), // "large" session DB
		"sessions/a/b.json": "nested",
	}
	root := nativeRoot(t, home, ".hermes", files)

	man, err := Snapshot([]AgentRoots{{Name: "hermes", Roots: []string{root}}}, dest)
	require.NoError(t, err)

	// Manifest shape.
	require.Equal(t, 1, len(man.Agents))
	require.Equal(t, "hermes", man.Agents[0].Name)
	require.NotEmpty(t, man.AplexicaVersion)
	require.False(t, man.CreatedAt.IsZero())
	require.Len(t, man.Agents[0].Roots, len(files))

	// manifest.json exists and round-trips.
	onDisk, err := ReadManifest(dest)
	require.NoError(t, err)
	require.Equal(t, man.AplexicaVersion, onDisk.AplexicaVersion)
	require.Len(t, onDisk.Agents[0].Roots, len(files))

	// Every manifest entry: file exists at the manifest-relative path, size and
	// sha256 match the source content.
	byBasename := map[string]FileEntry{}
	for _, fe := range man.Agents[0].Roots {
		// Entry path is relative to dest and uses forward slashes.
		require.False(t, filepath.IsAbs(fe.Path))
		require.NotContains(t, fe.Path, "\\")
		copied := filepath.Join(dest, filepath.FromSlash(fe.Path))
		got, err := os.ReadFile(copied)
		require.NoError(t, err)
		require.Equal(t, int64(len(got)), fe.Bytes)
		require.Equal(t, sha256Hex(string(got)), fe.SHA256)
		byBasename[filepath.Base(fe.Path)] = fe
	}

	// The large DB was included and its hash matches.
	dbEntry, ok := byBasename["state.db"]
	require.True(t, ok, "state.db must be in the manifest")
	require.Equal(t, int64(len(files["state.db"])), dbEntry.Bytes)
	require.Equal(t, sha256Hex(files["state.db"]), dbEntry.SHA256)
}

func TestSnapshot_RoundTripRestore(t *testing.T) {
	// Restore writes back to the ABSOLUTE native paths recorded in the
	// manifest. To keep the test hermetic we point the agent root at a
	// subtree of t.TempDir() (an absolute path) and verify the bytes land
	// back there after we corrupt them.
	workspace := t.TempDir()
	backupsRoot := filepath.Join(t.TempDir(), "backups")

	files := map[string]string{
		"settings.toml":  "original-settings",
		"db/sessions.db": "ORIGINAL-DB-CONTENT-" + strings.Repeat("x", 1000),
		"sub/dir/n.txt":  "original-nested",
	}
	root := nativeRoot(t, workspace, "agentroot", files)

	preSync := filepath.Join(backupsRoot, "pre-sync-2026-05-29T00-00-00Z")
	man, err := Snapshot([]AgentRoots{{Name: "claude", Roots: []string{root}}}, preSync)
	require.NoError(t, err)
	require.Len(t, man.Agents[0].Roots, len(files))

	// Corrupt the live native files (simulate post-Aplexica drift).
	for rel := range files {
		writeFile(t, filepath.Join(root, rel), "CORRUPTED-"+rel)
	}

	// Restore from the pre-sync snapshot.
	res, err := Restore(preSync, "")
	require.NoError(t, err)
	require.Len(t, res.Files, len(files))
	for _, fr := range res.Files {
		require.True(t, fr.OK, "file %s should restore OK: %s", fr.Path, fr.Err)
	}

	// Every native file is back to its original content.
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(root, rel))
		require.NoError(t, err)
		require.Equal(t, want, string(got), "restored content for %s", rel)
	}
}

func TestRestore_CreatesReversiblePreRestoreSnapshot(t *testing.T) {
	workspace := t.TempDir()
	backupsRoot := filepath.Join(t.TempDir(), "backups")

	root := nativeRoot(t, workspace, "agentroot", map[string]string{
		"file.txt": "ORIGINAL",
	})

	preSync := filepath.Join(backupsRoot, "pre-sync-2026-05-29T00-00-00Z")
	_, err := Snapshot([]AgentRoots{{Name: "codex", Roots: []string{root}}}, preSync)
	require.NoError(t, err)

	// Mutate the live file to a DIFFERENT, current state.
	current := "CURRENT-STATE-BEFORE-RESTORE"
	writeFile(t, filepath.Join(root, "file.txt"), current)

	res, err := Restore(preSync, "")
	require.NoError(t, err)

	// A pre-restore snapshot dir was created as a sibling of the backup.
	require.NotEmpty(t, res.PreRestoreDir)
	require.Equal(t, backupsRoot, filepath.Dir(res.PreRestoreDir))
	require.True(t, strings.HasPrefix(filepath.Base(res.PreRestoreDir), RestorePrefix))

	// It captured the CURRENT (pre-restore) state, so the restore is reversible.
	preMan, err := ReadManifest(res.PreRestoreDir)
	require.NoError(t, err)
	require.Len(t, preMan.Agents, 1)
	require.Equal(t, "codex", preMan.Agents[0].Name)
	require.Len(t, preMan.Agents[0].Roots, 1)
	require.Equal(t, sha256Hex(current), preMan.Agents[0].Roots[0].SHA256)

	// Concretely: restoring FROM the pre-restore snapshot undoes the restore,
	// putting the "current" state back over the now-original file.
	require.Equal(t, "ORIGINAL", readBack(t, root, "file.txt"))
	_, err = Restore(res.PreRestoreDir, "")
	require.NoError(t, err)
	require.Equal(t, current, readBack(t, root, "file.txt"))
}

func readBack(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	require.NoError(t, err)
	return string(b)
}

func TestRestore_SingleAgentOnly(t *testing.T) {
	workspace := t.TempDir()
	backupsRoot := filepath.Join(t.TempDir(), "backups")

	rootA := nativeRoot(t, workspace, "agentA", map[string]string{"a.txt": "A-ORIG"})
	rootB := nativeRoot(t, workspace, "agentB", map[string]string{"b.txt": "B-ORIG"})

	preSync := filepath.Join(backupsRoot, "pre-sync-x")
	_, err := Snapshot([]AgentRoots{
		{Name: "alpha", Roots: []string{rootA}},
		{Name: "beta", Roots: []string{rootB}},
	}, preSync)
	require.NoError(t, err)

	// Corrupt both live trees.
	writeFile(t, filepath.Join(rootA, "a.txt"), "A-CORRUPT")
	writeFile(t, filepath.Join(rootB, "b.txt"), "B-CORRUPT")

	// Restore only alpha.
	res, err := Restore(preSync, "alpha")
	require.NoError(t, err)
	require.Len(t, res.Files, 1)
	require.True(t, res.Files[0].OK)

	require.Equal(t, "A-ORIG", readBack(t, rootA, "a.txt"), "alpha restored")
	require.Equal(t, "B-CORRUPT", readBack(t, rootB, "b.txt"), "beta untouched")
}

func TestRestore_DigestMismatchNeverOverwritesLiveTarget(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "agent", "state.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(live), 0o700))
	require.NoError(t, os.WriteFile(live, []byte("before"), 0o600))
	backup := filepath.Join(root, "backups", "pre-sync-test")
	_, err := Snapshot([]AgentRoots{{Name: "agent", Roots: []string{live}}}, backup)
	require.NoError(t, err)
	man, err := ReadManifest(backup)
	require.NoError(t, err)
	copyPath := filepath.Join(backup, filepath.FromSlash(man.Agents[0].Roots[0].Path))
	require.NoError(t, os.WriteFile(copyPath, []byte("tampered"), 0o600))
	_, err = Restore(backup, "agent")
	require.Error(t, err)
	got, err := os.ReadFile(live)
	require.NoError(t, err)
	require.Equal(t, "before", string(got))
}

func TestList_EnumeratesSnapshots(t *testing.T) {
	workspace := t.TempDir()
	backupsRoot := filepath.Join(t.TempDir(), "backups")
	root := nativeRoot(t, workspace, "agentroot", map[string]string{
		"one.txt": "11111",
		"two.txt": "2222222",
	})

	// Two pre-sync snapshots, manual/scheduled snapshots, a stray
	// non-snapshot dir, and a loose file.
	for _, id := range []string{"pre-sync-2026-05-01T00-00-00Z", "pre-sync-2026-05-29T00-00-00Z"} {
		_, err := Snapshot([]AgentRoots{{Name: "hermes", Roots: []string{root}}}, filepath.Join(backupsRoot, id))
		require.NoError(t, err)
	}
	for _, id := range []string{"manual-2026-06-05T12-00-00Z", "scheduled-2026-06-05T13-00-00Z"} {
		_, err := Snapshot([]AgentRoots{{Name: "hermes", Roots: []string{root}}}, filepath.Join(backupsRoot, id))
		require.NoError(t, err)
	}
	require.NoError(t, os.MkdirAll(filepath.Join(backupsRoot, "not-a-snapshot"), 0o700))
	writeFile(t, filepath.Join(backupsRoot, "loose.txt"), "ignore me")

	// Trigger a restore so a pre-restore-* snapshot also appears.
	_, err := Restore(filepath.Join(backupsRoot, "pre-sync-2026-05-29T00-00-00Z"), "")
	require.NoError(t, err)

	infos, err := List(backupsRoot)
	require.NoError(t, err)

	kinds := map[string]int{}
	var ids []string
	for _, bi := range infos {
		kinds[bi.Kind]++
		ids = append(ids, bi.ID)
		require.NotZero(t, bi.FileCount)
		require.NotZero(t, bi.TotalBytes)
		require.False(t, bi.CreatedAt.IsZero())
	}
	require.Equal(t, 2, kinds["pre-sync"])
	require.Equal(t, 1, kinds["pre-restore"])
	require.Equal(t, 1, kinds["manual"])
	require.Equal(t, 1, kinds["scheduled"])
	require.NotContains(t, ids, "not-a-snapshot")
	require.NotContains(t, ids, "loose.txt")

	// Newest first by manifest createdAt; the pre-restore (taken just now) is
	// the most recent, so it sorts first.
	require.Equal(t, "pre-restore", infos[0].Kind)

	// TotalBytes for a pre-sync matches the sum of the two source files.
	for _, bi := range infos {
		if bi.ID == "pre-sync-2026-05-01T00-00-00Z" {
			require.Equal(t, int64(len("11111")+len("2222222")), bi.TotalBytes)
			require.Equal(t, 2, bi.FileCount)
			require.Equal(t, []string{"hermes"}, bi.Agents)
		}
	}
}

func TestList_MissingRootIsEmpty(t *testing.T) {
	infos, err := List(filepath.Join(t.TempDir(), "does-not-exist"))
	require.NoError(t, err)
	require.Empty(t, infos)
}

func TestSnapshot_MissingRootSkippedNotFatal(t *testing.T) {
	workspace := t.TempDir()
	dest := filepath.Join(t.TempDir(), "pre-sync")

	present := nativeRoot(t, workspace, "present", map[string]string{"x.txt": "hi"})
	missing := filepath.Join(workspace, "does-not-exist")

	man, err := Snapshot([]AgentRoots{
		{Name: "a", Roots: []string{present, missing}},
	}, dest)
	require.NoError(t, err)
	require.Len(t, man.Agents, 1)
	require.Len(t, man.Agents[0].Roots, 1) // only the present file
	require.Equal(t, "hi", string(mustReadCopy(t, dest, man.Agents[0].Roots[0].Path)))
}

func mustReadCopy(t *testing.T, dest, manifestRel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(manifestRel)))
	require.NoError(t, err)
	return b
}

func TestSnapshot_SymlinkSkipped(t *testing.T) {
	workspace := t.TempDir()
	dest := filepath.Join(t.TempDir(), "pre-sync")

	root := nativeRoot(t, workspace, "agentroot", map[string]string{"real.txt": "real"})
	// Add a symlink inside the tree pointing outside it.
	outside := filepath.Join(workspace, "secret.txt")
	writeFile(t, outside, "should-not-be-copied")
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link.txt")))

	man, err := Snapshot([]AgentRoots{{Name: "a", Roots: []string{root}}}, dest)
	require.NoError(t, err)
	require.Len(t, man.Agents[0].Roots, 1) // only real.txt, the symlink is skipped
	require.Equal(t, "real.txt", filepath.Base(man.Agents[0].Roots[0].Path))
}

func TestSnapshot_SpecialFileSkipped(t *testing.T) {
	// A live unix socket inside the tree (in the wild: git's
	// .git/fsmonitor--daemon.ipc) must be skipped, not opened. os.Open on a
	// socket fails ("operation not supported on socket"), which previously
	// aborted the ENTIRE agent snapshot and blocked the agent at startup.
	//
	// The socket is created under /tmp (not t.TempDir()) to keep its path
	// within the ~104-byte sun_path limit that bind(2) enforces.
	base, err := os.MkdirTemp("/tmp", "nb-special")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	root := filepath.Join(base, "agentroot")
	require.NoError(t, os.MkdirAll(root, 0o700))
	writeFile(t, filepath.Join(root, "real.txt"), "real")

	l, err := net.Listen("unix", filepath.Join(root, "x.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	man, err := Snapshot([]AgentRoots{{Name: "a", Roots: []string{root}}}, filepath.Join(base, "pre-sync"))
	require.NoError(t, err, "a non-regular file in the tree must not fail the snapshot")
	require.Len(t, man.Agents[0].Roots, 1) // only real.txt; the socket is skipped
	require.Equal(t, "real.txt", filepath.Base(man.Agents[0].Roots[0].Path))
}

func TestSnapshot_EmptyDestErrors(t *testing.T) {
	_, err := Snapshot(nil, "")
	require.Error(t, err)
}

func TestSnapshotContext_Canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := SnapshotContext(ctx, []AgentRoots{{Name: "a", Roots: []string{t.TempDir()}}}, filepath.Join(t.TempDir(), "manual"))
	require.ErrorIs(t, err, context.Canceled)
}

func TestSnapshot_SingleFileRoot(t *testing.T) {
	workspace := t.TempDir()
	dest := filepath.Join(t.TempDir(), "pre-sync")
	fileRoot := filepath.Join(workspace, "loose.cfg")
	writeFile(t, fileRoot, "config-bytes")

	man, err := Snapshot([]AgentRoots{{Name: "a", Roots: []string{fileRoot}}}, dest)
	require.NoError(t, err)
	require.Len(t, man.Agents[0].Roots, 1)
	require.Equal(t, sha256Hex("config-bytes"), man.Agents[0].Roots[0].SHA256)
}
