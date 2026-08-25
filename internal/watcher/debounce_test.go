package watcher

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// shortQuiet keeps tests fast — production default is 500ms (ADR-0031).
const shortQuiet = 50 * time.Millisecond

// fullContentHash returns the sha256 hex of path's entire content, computed
// via a streaming reader so the test reference does not itself allocate the
// whole (potentially over-cap) file. This is the value hashFile MUST NOT
// return for an over-cap file — returning it would prove the unbounded read.
func fullContentHash(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	require.NoError(t, err)
	return hex.EncodeToString(h.Sum(nil))
}

// TestHashFile_OverCapFileNotFullyRead is the regression test for the missing
// size guard in hashFile: every settled path was os.ReadFile'd in full purely
// for change-detection dedup, with no os.Stat/size check first. A continuously
// rewritten multi-MB/GB file the watcher sees (e.g. Codex's logs_*.sqlite-wal,
// or a large ~/.claude.json) was slurped entirely into RAM on every settle.
//
// The contract: a file larger than the hash cap must be hashed WITHOUT reading
// its full content. We prove this by creating a sparse over-cap file and
// asserting hashFile does not return the full-content sha256 (which is the
// only thing an unbounded os.ReadFile could produce). A sparse file is used so
// the test does not need to allocate or write the cap's worth of bytes.
func TestHashFile_OverCapFileNotFullyRead(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "huge.sqlite-wal")

	f, err := os.Create(path)
	require.NoError(t, err)
	// Sparse: extend the file past the cap without writing real bytes.
	require.NoError(t, f.Truncate(maxHashBytes+(1<<20)))
	require.NoError(t, f.Close())

	got, err := hashFile(path)
	require.NoError(t, err,
		"an over-cap file must still hash (so it can fall through to onSettled), not error")

	require.NotEqual(t, fullContentHash(t, path), got,
		"hashFile must NOT read the entire over-cap file — returning the full-content "+
			"sha256 proves the unbounded os.ReadFile is still present")
}

// TestHashFile_UnderCapFileHashesContent locks the happy path: a normal
// (under-cap) file is still hashed by content, so content-change dedup keeps
// working for the common case (memory/skill/config files are tiny).
func TestHashFile_UnderCapFileHashesContent(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "small.md")
	require.NoError(t, os.WriteFile(path, []byte("hello world"), 0o644))

	got, err := hashFile(path)
	require.NoError(t, err)
	require.Equal(t, fullContentHash(t, path), got,
		"an under-cap file must be hashed by its full content")
}

func TestDebouncer_FiresAfterQuietPeriod(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.md")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))

	var firedPaths []string
	var mu sync.Mutex
	d := NewDebouncer(shortQuiet, func(p string) {
		mu.Lock()
		firedPaths = append(firedPaths, p)
		mu.Unlock()
	})
	defer d.Stop()

	d.Notify(path)

	// Immediately after Notify the quiet period cannot have elapsed.
	// (No sleep before this check: on a stalled CI runner a "wait half
	// the quiet period" pre-check can race the timer legitimately firing.)
	mu.Lock()
	require.Empty(t, firedPaths)
	mu.Unlock()

	// Poll rather than sleep a fixed multiple of the quiet period — the
	// timer goroutine can be starved well past 3x on a loaded runner
	// (observed on macos-latest under -race).
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(firedPaths) == 1 && firedPaths[0] == path
	}, 10*time.Second, 10*time.Millisecond,
		"the settled callback must fire once the quiet period elapses")
}

func TestDebouncer_CoalescesBurst(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.md")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))

	var firedCount int
	var mu sync.Mutex
	d := NewDebouncer(shortQuiet, func(p string) {
		mu.Lock()
		firedCount++
		mu.Unlock()
	})
	defer d.Stop()

	for i := 0; i < 10; i++ {
		d.Notify(path)
		time.Sleep(shortQuiet / 5)
	}

	time.Sleep(shortQuiet * 3)

	mu.Lock()
	require.Equal(t, 1, firedCount,
		"a burst within the quiet window must coalesce into exactly one callback")
	mu.Unlock()
}

func TestDebouncer_HashDedupSuppressesUnchangedContent(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.md")
	require.NoError(t, os.WriteFile(path, []byte("same"), 0o644))

	var firedCount int
	var mu sync.Mutex
	d := NewDebouncer(shortQuiet, func(p string) {
		mu.Lock()
		firedCount++
		mu.Unlock()
	})
	defer d.Stop()

	d.Notify(path)
	time.Sleep(shortQuiet * 3)
	mu.Lock()
	require.Equal(t, 1, firedCount)
	mu.Unlock()

	d.Notify(path)
	time.Sleep(shortQuiet * 3)
	mu.Lock()
	require.Equal(t, 1, firedCount,
		"second notify with unchanged content must be suppressed by hash dedup")
	mu.Unlock()

	require.NoError(t, os.WriteFile(path, []byte("different"), 0o644))
	d.Notify(path)
	time.Sleep(shortQuiet * 3)
	mu.Lock()
	require.Equal(t, 2, firedCount, "content change must produce a new callback")
	mu.Unlock()
}

func TestDebouncer_IndependentPaths_FireSeparately(t *testing.T) {
	tmp := t.TempDir()
	pathA := filepath.Join(tmp, "a.md")
	pathB := filepath.Join(tmp, "b.md")
	require.NoError(t, os.WriteFile(pathA, []byte("A"), 0o644))
	require.NoError(t, os.WriteFile(pathB, []byte("B"), 0o644))

	var fired []string
	var mu sync.Mutex
	d := NewDebouncer(shortQuiet, func(p string) {
		mu.Lock()
		fired = append(fired, filepath.Base(p))
		mu.Unlock()
	})
	defer d.Stop()

	d.Notify(pathA)
	d.Notify(pathB)
	time.Sleep(shortQuiet * 3)

	mu.Lock()
	require.ElementsMatch(t, []string{"a.md", "b.md"}, fired,
		"distinct paths must each get their own callback")
	mu.Unlock()
}

func TestDebouncer_NonexistentFile_NoCallback(t *testing.T) {
	tmp := t.TempDir()
	gone := filepath.Join(tmp, "deleted.md")

	var firedCount int
	var mu sync.Mutex
	d := NewDebouncer(shortQuiet, func(p string) {
		mu.Lock()
		firedCount++
		mu.Unlock()
	})
	defer d.Stop()

	d.Notify(gone)
	time.Sleep(shortQuiet * 3)

	mu.Lock()
	require.Equal(t, 0, firedCount,
		"the debouncer should not fire if the file cannot be read at hash time")
	mu.Unlock()
}

// TestDebouncer_SetQuietPeriod verifies the SIGHUP live-setter path: the new
// quiet window is reflected via the QuietPeriod getter. (The "future-only"
// timer semantics — in-flight timers retain their original deadline — is a
// deliberate design choice documented on SetQuietPeriod itself; restarting
// timers under the setter would race with concurrent Notify calls.)
func TestDebouncer_SetQuietPeriod(t *testing.T) {
	d := NewDebouncer(500*time.Millisecond, func(string) {})
	defer d.Stop()
	require.Equal(t, 500*time.Millisecond, d.QuietPeriod())
	d.SetQuietPeriod(1 * time.Second)
	require.Equal(t, 1*time.Second, d.QuietPeriod())
}

func TestDebouncer_Pending_TracksQueueDepth(t *testing.T) {
	d := NewDebouncer(50*time.Millisecond, func(string) {})
	defer d.Stop()

	require.Equal(t, 0, d.Pending(), "fresh debouncer should report 0 pending")
	d.Notify("/tmp/path-a")
	d.Notify("/tmp/path-b")
	d.Notify("/tmp/path-c")
	require.Equal(t, 3, d.Pending(), "after 3 distinct Notify, Pending should be 3")
	// Notify same path again — same timer key, depth unchanged.
	d.Notify("/tmp/path-a")
	require.Equal(t, 3, d.Pending(), "repeated Notify must not grow Pending")
	// Wait past the quiet period; all timers fire and the map drains.
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 0, d.Pending(), "after quiet-period elapses, Pending should drain to 0")
}

func TestDebouncer_StopPreventsLateCallbacks(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.md")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	var firedCount int
	var mu sync.Mutex
	d := NewDebouncer(shortQuiet, func(p string) {
		mu.Lock()
		firedCount++
		mu.Unlock()
	})

	d.Notify(path)
	d.Stop()
	time.Sleep(shortQuiet * 3)

	mu.Lock()
	require.Equal(t, 0, firedCount,
		"Stop must cancel pending timers; no callback fires after Stop")
	mu.Unlock()
}

// TestDebouncer_StopWaitsForInFlightCallback locks the drain contract that
// fixes the Windows TempDir-cleanup flake: a quiet-period timer that has
// ALREADY fired runs the full import/fan-out pipeline on its goroutine;
// Stop used to cancel only PENDING timers and return while that callback
// was still writing the store, so test cleanup (TempDir RemoveAll) raced
// in-flight file writes. Stop must block until in-flight callbacks finish.
func TestDebouncer_StopWaitsForInFlightCallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	started := make(chan struct{})
	release := make(chan struct{})
	var done atomic.Bool
	d := NewDebouncer(1*time.Millisecond, func(string) {
		close(started)
		<-release
		done.Store(true)
	})
	d.Notify(path)
	<-started // callback is in flight

	stopReturned := make(chan struct{})
	go func() {
		d.Stop()
		close(stopReturned)
	}()

	select {
	case <-stopReturned:
		t.Fatal("Stop returned while the settled callback was still running")
	case <-time.After(200 * time.Millisecond):
		// good: Stop is blocked on the in-flight callback
	}

	close(release)
	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned after the callback finished")
	}
	require.True(t, done.Load(), "callback must have completed before Stop returned")
}

// TestDebouncer_FailedSettleIsRetried is the regression test for the lastHash-
// poisoning bug: evaluate() recorded the file's content hash BEFORE invoking the
// settle callback and ignored its result, so a settle that failed to commit
// (e.g. a transient import error during the unstable window right after a daemon
// restart) left the path's dedup entry pointing at the final content hash. No
// later event re-fired for the now-static file, stranding a freshly-written
// conversation out of the canonical store until its bytes changed.
//
// Contract (NewDebouncerWithCommit): the content hash is recorded ONLY when the
// callback reports a commit. A settle that reports failure must NOT suppress a
// subsequent settle of the SAME (unchanged) content — it must retry.
func TestDebouncer_FailedSettleIsRetried(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "conversation.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("stranded-content"), 0o644))

	var calls int
	commit := false // first settle fails to commit; later ones succeed
	var mu sync.Mutex
	d := NewDebouncerWithCommit(shortQuiet, func(string) bool {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return commit
	})
	defer d.Stop()

	// First settle: callback reports NO commit. The hash must not be recorded.
	d.Notify(path)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls == 1
	}, 2*time.Second, 5*time.Millisecond, "first settle must invoke the callback")

	// Second settle with UNCHANGED content. The buggy implementation recorded the
	// hash on the failed first settle and suppresses this as a dedup hit; the fix
	// must let it retry because nothing committed yet.
	mu.Lock()
	commit = true
	mu.Unlock()
	d.Notify(path)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls == 2
	}, 2*time.Second, 5*time.Millisecond,
		"a settle that did not commit must not poison dedup; unchanged content must retry")

	// Third settle, now that the second committed: unchanged content IS deduped.
	d.Notify(path)
	time.Sleep(shortQuiet * 3)
	mu.Lock()
	require.Equal(t, 2, calls,
		"after a committed settle, an unchanged-content settle must be suppressed")
	mu.Unlock()
}
