//go:build windows

package nativebackup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestCloudKeyringLoadRetriesWindowsTransientLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ring.cbor")
	store := CloudBackupKeyStore{Path: path}
	want, err := store.LoadOrCreate()
	require.NoError(t, err)
	require.True(t, transientCloudKeyringLoadError(windows.ERROR_SHARING_VIOLATION))
	require.True(t, transientCloudKeyringLoadError(windows.ERROR_LOCK_VIOLATION))

	lockedFile, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	var overlapped windows.Overlapped
	require.NoError(t, windows.LockFileEx(windows.Handle(lockedFile.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped))
	locked := true
	t.Cleanup(func() {
		if locked {
			_ = windows.UnlockFileEx(windows.Handle(lockedFile.Fd()), 0, 1, 0, &overlapped)
		}
		_ = lockedFile.Close()
	})

	_, readErr := os.ReadFile(path)
	require.Error(t, readErr)
	require.True(t, transientCloudKeyringLoadError(readErr))

	type loadResult struct {
		ring CloudBackupKeyRingV2
		err  error
	}
	loaded := make(chan loadResult, 1)
	go func() {
		ring, loadErr := store.LoadOrCreate()
		loaded <- loadResult{ring: ring, err: loadErr}
	}()

	// Keep the denial in place long enough to prove LoadOrCreate is retrying,
	// then release it inside the bounded retry window.
	select {
	case result := <-loaded:
		t.Fatalf("LoadOrCreate returned before the sharing violation cleared: %v", result.err)
	case <-time.After(20 * time.Millisecond):
	}
	require.NoError(t, windows.UnlockFileEx(windows.Handle(lockedFile.Fd()), 0, 1, 0, &overlapped))
	locked = false

	select {
	case result := <-loaded:
		require.NoError(t, result.err)
		require.Equal(t, want.CurrentKeyID, result.ring.CurrentKeyID)
	case <-time.After(time.Second):
		t.Fatal("LoadOrCreate did not recover after the sharing violation cleared")
	}
}
