package watcher

import (
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRecursiveSource_Conformance_FlatDir reuses most of the cross-platform
// conformance suite from source_test.go, applied to a RecursiveSource over a
// flat (single-level) directory. The recursive aggregator must satisfy the
// same per-directory contract for DetectsCreateEvent, CloseIsIdempotent, and
// CloseDrainsChannels.
//
// The IgnoresSubdirectoryEvents subtest is intentionally excluded because
// RecursiveSource by design surfaces events from subdirectories — the opposite
// of what that subtest expects. That behavior is correct for a recursive
// watcher and is exercised by TestRecursiveSource_InitialWalk_WatchesPreExistingSubdirs.
func TestRecursiveSource_Conformance_FlatDir(t *testing.T) {
	makeSource := func(dir string) (Source, error) {
		return NewRecursiveSource(dir)
	}

	t.Run("DetectsCreateEvent", func(t *testing.T) {
		dir := realTempDir(t)
		s, err := makeSource(dir)
		require.NoError(t, err)
		defer s.Close()

		time.Sleep(150 * time.Millisecond)

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

func TestRecursiveSource_InitialWalk_WatchesPreExistingSubdirs(t *testing.T) {
	root := realTempDir(t)
	subB := filepath.Join(root, "a", "b")
	require.NoError(t, os.MkdirAll(subB, 0o755))

	s, err := NewRecursiveSource(root)
	require.NoError(t, err)
	defer s.Close()

	// Give the underlying watchers time to register.
	time.Sleep(150 * time.Millisecond)

	// Write a file in the deepest subdirectory. The recursive source should
	// detect it. Drain events until we see the target — FSEvents may also
	// emit intermediate directory-level events (e.g. the a/b dir itself
	// appearing as an OpChange on the a source) before the file event.
	target := filepath.Join(subB, "deep.md")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			if ev.Path == target {
				require.Equal(t, OpChange, ev.Op)
				return
			}
			// Other events (directory change events) are acceptable; keep draining.
		case <-deadline:
			t.Fatal("no event for file created in pre-existing subdirectory")
		}
	}
}

func TestRecursiveSource_EventsFromMultipleDirsAggregate(t *testing.T) {
	root := realTempDir(t)
	dirA := filepath.Join(root, "alpha")
	dirB := filepath.Join(root, "beta")
	require.NoError(t, os.MkdirAll(dirA, 0o755))
	require.NoError(t, os.MkdirAll(dirB, 0o755))

	s, err := NewRecursiveSource(root)
	require.NoError(t, err)
	defer s.Close()

	time.Sleep(150 * time.Millisecond)

	pathA := filepath.Join(dirA, "a.md")
	pathB := filepath.Join(dirB, "b.md")
	require.NoError(t, os.WriteFile(pathA, []byte("A"), 0o644))
	require.NoError(t, os.WriteFile(pathB, []byte("B"), 0o644))

	// Drain events until we've seen both file paths. FSEvents may also emit
	// directory-level events (e.g. alpha/ appearing as OpChange on the root
	// source) — those are acceptable intermediates; we just need to confirm
	// both file events eventually arrive.
	seenA, seenB := false, false
	deadline := time.After(2 * time.Second)
	for !seenA || !seenB {
		select {
		case ev := <-s.Events():
			if ev.Path == pathA {
				seenA = true
			}
			if ev.Path == pathB {
				seenB = true
			}
		case <-deadline:
			t.Fatalf("got seenA=%v seenB=%v — did not see both file events within 2s", seenA, seenB)
		}
	}
}

func TestRecursiveSource_CloseStopsAllUnderlyingSources(t *testing.T) {
	root := realTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "b"), 0o755))

	s, err := NewRecursiveSource(root)
	require.NoError(t, err)

	require.NoError(t, s.Close())
	// Idempotent.
	require.NoError(t, s.Close())

	// After Close, Events() channel must drain and close so range loops exit.
	done := make(chan struct{})
	go func() {
		for range s.Events() {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Events() channel did not close within 2 seconds of Close()")
	}
}

func TestRecursiveSource_IgnoresFilesInRoot_StaysCorrect(t *testing.T) {
	// Sanity: events for files DIRECTLY in the watched root still arrive
	// via the root's underlying Source. This is the same behavior as
	// non-recursive watching.
	root := realTempDir(t)
	s, err := NewRecursiveSource(root)
	require.NoError(t, err)
	defer s.Close()

	time.Sleep(150 * time.Millisecond)

	path := filepath.Join(root, "top.md")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	select {
	case ev := <-s.Events():
		require.Equal(t, path, ev.Path)
	case <-time.After(2 * time.Second):
		t.Fatal("no event for file in root dir")
	}
}

func TestRecursiveSource_NewSubdir_AttachesAndWatches(t *testing.T) {
	root := realTempDir(t)

	s, err := NewRecursiveSource(root)
	require.NoError(t, err)
	defer s.Close()

	time.Sleep(150 * time.Millisecond)

	// Create a brand-new subdirectory AFTER the recursive source is running.
	sub := filepath.Join(root, "newsub")
	require.NoError(t, os.Mkdir(sub, 0o755))

	// Drain any events from the mkdir itself (different OSes emit
	// different events when a subdir is created in a watched dir).
	deadline := time.After(500 * time.Millisecond)
draining:
	for {
		select {
		case <-s.Events():
		case <-deadline:
			break draining
		}
	}

	// Give the source a moment to notice the new subdir and attach.
	time.Sleep(150 * time.Millisecond)

	// Now write a file INSIDE the new subdir. The recursive source must
	// have attached a watcher for the new subdir; otherwise this event
	// is missed.
	target := filepath.Join(sub, "child.md")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))

	select {
	case ev := <-s.Events():
		require.Equal(t, target, ev.Path,
			"recursive source must attach to subdirs created at runtime")
	case <-time.After(2 * time.Second):
		t.Fatal("no event for file created in subdir that appeared after start")
	}
}

// The typical skill-creation sequence is mkdir IMMEDIATELY followed by a
// file write — the write lands before the new subdir's Source starts
// watching, so the file must be announced by the dynamic attach itself or it
// is silently missed until the next touch (a new agent-created
// skill never imported). No drain-and-settle between mkdir and write here —
// that gap is exactly what this test refuses to allow.
func TestRecursiveSource_NewSubdir_AnnouncesFilesWrittenBeforeAttach(t *testing.T) {
	root := realTempDir(t)

	s, err := NewRecursiveSource(root)
	require.NoError(t, err)
	defer s.Close()

	time.Sleep(150 * time.Millisecond)

	sub := filepath.Join(root, "fast-skill")
	require.NoError(t, os.Mkdir(sub, 0o755))
	target := filepath.Join(sub, "SKILL.md")
	require.NoError(t, os.WriteFile(target, []byte("---\nname: fast\n---\nBody.\n"), 0o644))

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			if ev.Path == target && ev.Op == OpChange {
				return // announced (synthetically or via the attached Source)
			}
		case <-deadline:
			t.Fatal("file written immediately after mkdir was never announced")
		}
	}
}

func TestRecursiveSource_NewSubdir_NestedDepth(t *testing.T) {
	// When a new subdir is created and IMMEDIATELY has children, the
	// recursive source's WalkDir-on-attach must catch those too — otherwise
	// a `mkdir -p a/b/c && touch a/b/c/file` race would slip past us.
	//
	// On Windows the race between attaching z's per-dir Source and the
	// first event firing inside z is notoriously hard to pin down: each
	// per-dir Source costs a CreateFile + ReadDirectoryChanges round trip
	// (AV-padded), and there's a sub-millisecond window between RDC being
	// queued and the kernel actually wiring the watch where the first
	// FILE_ACTION_ADDED can be lost. Rather than gambling on a longer
	// pre-write sleep covering that window, re-touch the file every
	// second — RDC is reliable for subsequent FILE_ACTION_MODIFIED
	// events, and the test only needs to see ONE event for `target` to
	// prove z's Source is wired up. Single-write expectation on Unix is
	// preserved because the first event arrives almost immediately.
	root := realTempDir(t)

	s, err := NewRecursiveSource(root)
	require.NoError(t, err)
	defer s.Close()

	time.Sleep(150 * time.Millisecond)

	// Create a nested chain. With WalkDir at attach time, every dir gets
	// its own Source eventually.
	nested := filepath.Join(root, "x", "y", "z")
	require.NoError(t, os.MkdirAll(nested, 0o755))

	// Give time for the recursive source to attach to the new top-level
	// subdir AND for walkAndAttach to bring in the nested dirs.
	time.Sleep(1500 * time.Millisecond)

	target := filepath.Join(nested, "deep.md")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			if ev.Path == target {
				return
			}
			// drain other events
		case <-deadline:
			t.Fatal("never saw the file inside the nested-on-creation subdir")
		}
	}
}

func TestRecursiveSource_RemoveSubdir_DropsAndClosesSource(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin uses one recursive FSEvents source instead of one child source per subdir")
	}
	root := realTempDir(t)
	sub := filepath.Join(root, "removable")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	rs, err := NewRecursiveSource(root)
	require.NoError(t, err)
	defer rs.Close()

	time.Sleep(150 * time.Millisecond)

	// Verify the sub is tracked.
	rs.mu.Lock()
	_, before := rs.children[sub]
	rs.mu.Unlock()
	require.True(t, before, "sub should be in children map after initial walk")

	// Remove the subdir.
	require.NoError(t, os.RemoveAll(sub))

	// Wait for the recursive source to notice and clean up.
	time.Sleep(400 * time.Millisecond)

	rs.mu.Lock()
	_, after := rs.children[sub]
	rs.mu.Unlock()
	require.False(t, after,
		"sub must be removed from children map after directory deletion")
}

// drainEventsRecursive drains events until either count is reached or timeout fires.
// Useful for tests where the exact count matters but we don't want to
// hardcode order.
func drainEventsRecursive(s Source, count int, timeout time.Duration) int32 {
	var got int32
	done := make(chan struct{})
	go func() {
		deadline := time.After(timeout)
		for atomic.LoadInt32(&got) < int32(count) {
			select {
			case <-s.Events():
				atomic.AddInt32(&got, 1)
			case <-deadline:
				close(done)
				return
			}
		}
		close(done)
	}()
	<-done
	return atomic.LoadInt32(&got)
}
