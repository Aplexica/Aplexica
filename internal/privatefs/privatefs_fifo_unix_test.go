//go:build unix

package privatefs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestOpenReadRegularIntegrityRejectsFIFOWithoutBlockingForWriter(t *testing.T) {
	rootPath := t.TempDir()
	fifo := filepath.Join(rootPath, "manifest.json")
	require.NoError(t, unix.Mkfifo(fifo, 0o600))
	root, err := OpenRoot(rootPath, DirPolicy{Access: AccessIntegrityOnly})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	done := make(chan error, 1)
	go func() {
		f, openErr := root.OpenReadRegularIntegrity("manifest.json")
		if f != nil {
			_ = f.Close()
		}
		done <- openErr
	}()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		// Unblock a regressed blocking open before failing so the test process can
		// shut down cleanly instead of leaving the goroutine behind.
		if fd, openErr := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0); openErr == nil {
			_ = unix.Close(fd)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("integrity open blocked on a FIFO writer")
	}

	info, err := os.Lstat(fifo)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeNamedPipe)
}
