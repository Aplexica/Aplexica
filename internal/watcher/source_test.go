package watcher

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// realTempDir returns t.TempDir() with symlinks resolved so that test-
// constructed expected paths match the canonical paths that the platform
// Sources emit. On macOS, t.TempDir() returns /var/folders/... which is a
// symlink to /private/var/folders/...; FSEvents emits the canonical form.
func realTempDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	resolved, err := filepath.EvalSymlinks(tmp)
	require.NoError(t, err)
	return resolved
}

// runSourceConformance exercises the Source behavioral contract from
// source.go. Per-platform tests in source_<goos>_test.go reuse this by
// constructing their platform-specific Source and passing it here.
func runSourceConformance(t *testing.T, makeSource func(dir string) (Source, error)) {
	t.Helper()

	t.Run("DetectsCreateEvent", func(t *testing.T) {
		dir := realTempDir(t)
		s, err := makeSource(dir)
		require.NoError(t, err)
		defer s.Close()

		time.Sleep(100 * time.Millisecond)

		path := filepath.Join(dir, "new.md")
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

		select {
		case ev := <-s.Events():
			require.Equal(t, path, ev.Path)
			require.Equal(t, OpChange, ev.Op)
		case <-time.After(2 * time.Second):
			t.Fatal("no event received within 2 seconds of file creation")
		}
	})

	t.Run("IgnoresSubdirectoryEvents", func(t *testing.T) {
		dir := realTempDir(t)
		sub := filepath.Join(dir, "sub")
		require.NoError(t, os.Mkdir(sub, 0o755))

		s, err := makeSource(dir)
		require.NoError(t, err)
		defer s.Close()

		// Drain any directory-creation event that the source may legitimately
		// emit for the mkdir (e.g. FSEvents emits an OpChange for the subdir
		// path itself since it is a direct child of the watched dir).
		time.Sleep(100 * time.Millisecond)
		drainDeadline := time.After(300 * time.Millisecond)
	drainLoop:
		for {
			select {
			case <-s.Events():
			case <-drainDeadline:
				break drainLoop
			}
		}

		// Now write a file INSIDE the subdir. A non-recursive source must NOT
		// surface this event (the file lives at depth > 1 relative to dir).
		require.NoError(t, os.WriteFile(filepath.Join(sub, "ignored.md"), []byte("x"), 0o644))

		select {
		case ev := <-s.Events():
			t.Fatalf("unexpected event for subdirectory file: %+v", ev)
		case <-time.After(500 * time.Millisecond):
			// expected — non-recursive
		}
	})

	t.Run("CloseIsIdempotent", func(t *testing.T) {
		dir := realTempDir(t)
		s, err := makeSource(dir)
		require.NoError(t, err)
		require.NoError(t, s.Close())
		require.NoError(t, s.Close(), "second Close must be a no-op, not an error")
	})

	t.Run("CloseDrainsChannels", func(t *testing.T) {
		dir := realTempDir(t)
		s, err := makeSource(dir)
		require.NoError(t, err)
		require.NoError(t, s.Close())

		done := make(chan struct{})
		go func() {
			for range s.Events() {
				// drain
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Events() channel did not close within 2 seconds of Close()")
		}
	})
}

func TestSource_Conformance_Fsnotify(t *testing.T) {
	runSourceConformance(t, newFsnotifySource)
}

func TestNew_ReturnsSourceForCurrentPlatform(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	require.NoError(t, err)
	require.NotNil(t, s)
	require.NoError(t, s.Close())
}

// fakeInnerSource is a trivial Source used to construct a FilteredSource in
// isolation (no platform deps). It records how many times Close was called so
// the test can assert FilteredSource.Close still forwards to the inner Source.
type fakeInnerSource struct {
	events chan Event
	errors chan error
	mu     sync.Mutex
	closes int
}

func newFakeInnerSource() *fakeInnerSource {
	return &fakeInnerSource{
		events: make(chan Event),
		errors: make(chan error),
	}
}

func (f *fakeInnerSource) Add(string) error     { return nil }
func (f *fakeInnerSource) Events() <-chan Event { return f.events }
func (f *fakeInnerSource) Errors() <-chan error { return f.errors }
func (f *fakeInnerSource) Close() error {
	f.mu.Lock()
	f.closes++
	f.mu.Unlock()
	return nil
}

// closeConcurrently fires n goroutines that all block on a shared start channel
// and then race to call s.Close() as tightly as possible. It fails the test if
// any Close returns an error; a "close of closed channel" panic surfaces as a
// crashed test run (and as a data race under -race), which is the pre-fix
// failure mode this guards against.
func closeConcurrently(t *testing.T, s Source, n int) {
	t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := s.Close(); err != nil {
				t.Errorf("Close returned error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
}

// TestFilteredSource_ConcurrentCloseNoPanic verifies FilteredSource.Close is
// safe under concurrent callers. Before the sync.Once fix, two goroutines could
// both take the default branch of the check-then-close select and both
// close(done) -> panic "close of closed channel". Run under -race.
func TestFilteredSource_ConcurrentCloseNoPanic(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		inner := newFakeInnerSource()
		fs := NewFilteredSource(inner, func(string) bool { return true })
		closeConcurrently(t, fs, 20)
	}
}

// TestFsnotifySource_ConcurrentCloseNoPanic verifies fsnotifySource.Close is
// safe under concurrent callers (same double-close(done) race). fsnotify is
// available on the host platform here (the conformance suite already uses it),
// so the source is constructible against a real temp dir. Run under -race.
func TestFsnotifySource_ConcurrentCloseNoPanic(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		dir := realTempDir(t)
		s, err := newFsnotifySource(dir)
		require.NoError(t, err)
		closeConcurrently(t, s, 20)
	}
}
