//go:build darwin

package watcher

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSource_Conformance_DarwinFSEvents(t *testing.T) {
	runSourceConformance(t, newDarwinFSEventsSource)
}

// The stat-poll must detect changes from cheap file METADATA (size + mtime),
// never by reading and hashing the whole file. Re-reading + SHA256-hashing
// every file in every watched directory every pollCadence is what pegged 5+
// CPU cores on machines with large agent histories (220+ dirs, multi-MB
// conversation files re-hashed ~10x/sec). Content-level dedup is the
// Debouncer's job, downstream — the poll only needs change DETECTION.
//
// Proof that the scan reads metadata and not content: a file that is
// stat-able but whose content is unreadable (mode 0000) must still be
// fingerprinted and diffed. A content-hashing scan would error on the read
// and drop the file (emitting a spurious OpRemove); a metadata scan keeps it
// and reports the real OpChange.
func TestFingerprintDir_DetectsViaMetadataNotContent(t *testing.T) {
	dir := realTempDir(t)
	path := filepath.Join(dir, "conv.md")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))

	before := fingerprintDir(dir)
	require.Contains(t, before, path)

	// Change the content (and size), then strip read permission so a content
	// read would fail while stat still succeeds.
	require.NoError(t, os.WriteFile(path, []byte("v2 is longer than v1"), 0o644))
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	after := fingerprintDir(dir)
	require.Contains(t, after, path,
		"a stat-able but unreadable file must still be fingerprinted (metadata, not content)")
	require.NotEqual(t, before[path], after[path],
		"a size/mtime change must yield a different fingerprint")

	events := diffFingerprints(before, after)
	require.Equal(t, []Event{{Path: path, Op: OpChange}}, events,
		"a changed file must surface exactly one OpChange — not an OpRemove from a failed read")
}

// diffFingerprints reports a removal when a previously-seen file is gone, and
// nothing at all when the directory is unchanged (so the poll stays silent on
// quiet directories instead of re-emitting on every tick).
func TestDiffFingerprints_RemovalAndSteadyState(t *testing.T) {
	dir := realTempDir(t)
	a := filepath.Join(dir, "a.md")
	b := filepath.Join(dir, "b.md")
	require.NoError(t, os.WriteFile(a, []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(b, []byte("b"), 0o644))

	snap1 := fingerprintDir(dir)
	require.Empty(t, diffFingerprints(snap1, snap1),
		"identical snapshots must produce no events")

	require.NoError(t, os.Remove(b))
	snap2 := fingerprintDir(dir)
	require.Equal(t, []Event{{Path: b, Op: OpRemove}}, diffFingerprints(snap1, snap2),
		"a deleted file must surface exactly one OpRemove")
}

func TestDarwinFSEvents_StatPollingCatchesFastChange(t *testing.T) {
	// FSEvents on macOS has a coalescing latency (50ms+ even with our low
	// setting). Back-to-back changes within a single FSEvents coalesce
	// window must still surface — via FSEvents itself and/or the
	// stat-polling backstop (darwinStatPollCadence).
	dir := t.TempDir()
	s, err := newDarwinFSEventsSource(dir)
	require.NoError(t, err)
	defer s.Close()

	time.Sleep(100 * time.Millisecond)

	path := filepath.Join(dir, "fast.md")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))

	var count int32
	done := make(chan struct{})
	go func() {
		deadline := time.After(2 * time.Second)
		for {
			select {
			case <-s.Events():
				atomic.AddInt32(&count, 1)
			case <-deadline:
				close(done)
				return
			}
		}
	}()

	// Write twice in quick succession.
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, os.WriteFile(path, []byte("v2"), 0o644))
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, os.WriteFile(path, []byte("v3"), 0o644))

	<-done
	// At least one OpChange must surface — possibly multiple. The exact
	// count depends on FSEvents coalescing + stat-poll timing. The
	// invariant we test: events DO reach the consumer.
	require.GreaterOrEqual(t, atomic.LoadInt32(&count), int32(1),
		"FSEvents + stat polling must surface at least one event for the writes")
}
