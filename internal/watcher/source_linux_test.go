//go:build linux

package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSource_Conformance_LinuxInotify(t *testing.T) {
	runSourceConformance(t, newLinuxInotifySource)
}

func TestLinuxInotify_DeliversCreateImmediately(t *testing.T) {
	// inotify on Linux delivers events synchronously when the kernel
	// processes the write syscall — no coalescing layer like macOS.
	dir := t.TempDir()
	s, err := newLinuxInotifySource(dir)
	require.NoError(t, err)
	defer s.Close()

	time.Sleep(50 * time.Millisecond)

	path := filepath.Join(dir, "instant.md")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	select {
	case ev := <-s.Events():
		require.Equal(t, path, ev.Path)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("inotify event not delivered within 500ms — too slow")
	}
}

func TestLinuxInotify_PollingFallback_StillDeliversChanges(t *testing.T) {
	// Construct the fallback path directly (no easy way to exhaust the
	// real budget in tests). The fallback Source has the same Event API.
	dir := t.TempDir()
	s, err := newLinuxPollingFallbackSource(dir)
	require.NoError(t, err)
	defer s.Close()

	// Drain the initial warning on Errors().
	select {
	case <-s.Errors():
	case <-time.After(100 * time.Millisecond):
	}

	path := filepath.Join(dir, "pollme.md")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))

	// Wait up to 2× the poll cadence + slack for the event.
	select {
	case ev := <-s.Events():
		require.Equal(t, path, ev.Path)
		require.Equal(t, OpChange, ev.Op)
	case <-time.After(linuxFallbackPollCadence*2 + 200*time.Millisecond):
		t.Fatal("polling fallback did not deliver the create within 2× cadence")
	}
}
