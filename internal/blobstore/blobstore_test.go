package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return &Store{Root: t.TempDir()}
}

func TestPut_DedupAndSha256Filename(t *testing.T) {
	s := newStore(t)
	raw := []byte("hello-blob-bytes")

	h1, err := s.Put(raw)
	require.NoError(t, err)
	h2, err := s.Put(raw) // byte-identical re-Put
	require.NoError(t, err)
	require.Equal(t, h1, h2, "identical bytes hash to the same id")

	// sha256(file contents) == filename.
	sum := sha256.Sum256(raw)
	require.Equal(t, hex.EncodeToString(sum[:]), h1)

	// Exactly one file exists for this content (dedup, not two copies).
	var files int
	require.NoError(t, filepath.WalkDir(s.Root, func(_ string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		if !d.IsDir() {
			files++
		}
		return nil
	}))
	require.Equal(t, 1, files, "dedup writes a single file")
}

// TestPut_FsyncsShardDir exercises the post-rename directory fsync path that
// makes a freshly-written blob's <AA>/<BB> directory entry durable. Crash
// durability can't be observed directly in a unit test, so this asserts the
// fsync-dir path runs to completion (Put succeeds with the blob landing at its
// sharded location) and, directly, that fsyncDir Syncs a real shard directory
// without error.
func TestPut_FsyncsShardDir(t *testing.T) {
	s := newStore(t)
	raw := []byte("fsync-the-shard-dir")

	h, err := s.Put(raw)
	require.NoError(t, err)

	shardDir := filepath.Dir(s.Path(h))
	info, err := os.Stat(shardDir)
	require.NoError(t, err, "shard directory exists after Put")
	require.True(t, info.IsDir())

	// The exact path Put fsyncs must itself be fsync-able (the happy branch of
	// fsyncDir: open + Sync + close with no error on a normal directory).
	require.NoError(t, fsyncDir(shardDir), "fsyncDir Syncs an existing shard directory")
}

// TestFsyncDir_MissingDirErrors confirms fsyncDir surfaces a real open failure
// (a non-existent directory) rather than swallowing it — the graceful-degrade
// branch is reserved for platforms/filesystems that reject a directory Sync,
// not for a missing path.
func TestFsyncDir_MissingDirErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-shard")
	require.Error(t, fsyncDir(missing), "fsyncDir must report a missing directory")
}

func TestPath_TwoLevelShard(t *testing.T) {
	s := newStore(t)
	raw := []byte("shard-me")
	h, err := s.Put(raw)
	require.NoError(t, err)

	want := filepath.Join(s.Root, h[0:2], h[2:4], h)
	require.Equal(t, want, s.Path(h))

	info, err := os.Stat(want)
	require.NoError(t, err, "blob lands at the sharded path")
	require.False(t, info.IsDir())
}

func TestOpen_RoundTrip(t *testing.T) {
	s := newStore(t)
	raw := []byte("round-trip-content")
	h, err := s.Put(raw)
	require.NoError(t, err)

	rc, err := s.Open(h)
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, raw, got)
}

func TestHas(t *testing.T) {
	s := newStore(t)
	require.False(t, s.Has("0000000000000000000000000000000000000000000000000000000000000000"))
	h, err := s.Put([]byte("present"))
	require.NoError(t, err)
	require.True(t, s.Has(h))
}

func TestDelete_Idempotent(t *testing.T) {
	s := newStore(t)
	h, err := s.Put([]byte("delete-me"))
	require.NoError(t, err)
	require.True(t, s.Has(h))

	require.NoError(t, s.Delete(h))
	require.False(t, s.Has(h))
	// Deleting again is not an error.
	require.NoError(t, s.Delete(h))
	// Deleting a never-existing blob is not an error.
	require.NoError(t, s.Delete("deadbeef0000000000000000000000000000000000000000000000000000beef"))
}

func TestGC_KeepsLiveAndWithinGrace_DeletesOldOrphans(t *testing.T) {
	s := newStore(t)

	liveH, err := s.Put([]byte("live-blob"))
	require.NoError(t, err)
	oldOrphanH, err := s.Put([]byte("old-orphan-blob"))
	require.NoError(t, err)
	freshOrphanH, err := s.Put([]byte("fresh-orphan-blob"))
	require.NoError(t, err)

	now := time.Now()

	// Backdate the old orphan well past the grace window.
	old := now.Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(s.Path(oldOrphanH), old, old))

	// graceCutoff = now - 1h: the old orphan (2h old) is collectible;
	// the fresh orphan (just written) is within grace and kept.
	graceCutoff := now.Add(-1 * time.Hour)
	live := map[string]bool{liveH: true}

	deleted, err := s.GC(live, graceCutoff)
	require.NoError(t, err)
	require.Equal(t, 1, deleted, "only the old orphan is collected")

	require.True(t, s.Has(liveH), "live blob kept")
	require.True(t, s.Has(freshOrphanH), "fresh orphan kept (within grace)")
	require.False(t, s.Has(oldOrphanH), "old orphan deleted")
}

func TestGC_EmptyRoot(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "does-not-exist")}
	deleted, err := s.GC(map[string]bool{}, time.Now())
	require.NoError(t, err)
	require.Equal(t, 0, deleted)
}

// TestGC_DeletesMislocatedFileAndCountsOnlyRealDeletes guards against GC
// overstating its delete count for a file that is NOT at its canonical shard
// position. PlanGC walks by directory, so any regular file under Root is a
// candidate; GC must delete the path it actually walked, not a path
// reconstructed by re-sharding the basename through Path(). A 64-hex-named
// file laid flat at Root (e.g. a pre-shard layout or a foreign drop) hashes
// back to a sharded path that does not exist, so a re-shard remove is a silent
// no-op — yet the blob must still be reclaimed and counted exactly once.
func TestGC_DeletesMislocatedFileAndCountsOnlyRealDeletes(t *testing.T) {
	s := newStore(t)

	// A valid 64-hex blob id, but written flat at Root rather than under the
	// <AA>/<BB>/<hash> shard. Path(flatHash) therefore points elsewhere.
	flatHash := "deadbeef00000000000000000000000000000000000000000000000000000000"
	flatPath := filepath.Join(s.Root, flatHash)
	require.NoError(t, os.WriteFile(flatPath, []byte("mislocated-orphan"), filePerm))
	require.NotEqual(t, flatPath, s.Path(flatHash),
		"precondition: the flat file is not at its canonical shard path")

	// Backdate past the grace window so it is collectible.
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(flatPath, old, old))

	graceCutoff := time.Now().Add(-1 * time.Hour)
	deleted, err := s.GC(map[string]bool{}, graceCutoff)
	require.NoError(t, err)

	// The file walked must actually be gone, and counted exactly once.
	_, statErr := os.Stat(flatPath)
	require.True(t, os.IsNotExist(statErr), "the mislocated orphan must be removed from disk")
	require.Equal(t, 1, deleted, "GC must count exactly the files it really deleted")
}
