//go:build !windows

package syncd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGeneratedConversationCacheMigrationRejectsFIFOAndSymlink(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo.jsonl")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	done := make(chan bool, 1)
	go func() {
		done <- aplexicaGeneratedMainConversationSession(fifo)
	}()
	select {
	case generated := <-done:
		require.False(t, generated)
	case <-time.After(time.Second):
		t.Fatal("cache migration blocked opening a FIFO")
	}
	_, ok := fingerprintPath(fifo)
	require.False(t, ok, "non-regular files must not enter the scan cache")

	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte(`{"type":"session_meta"}`+"\n"), 0o600))
	symlink := filepath.Join(dir, "symlink.jsonl")
	require.NoError(t, os.Symlink(target, symlink))
	require.False(t, aplexicaGeneratedMainConversationSession(symlink),
		"migration marker reads must not follow symlinks")
	_, ok = fingerprintPath(symlink)
	require.False(t, ok, "symlinks must not enter the scan cache")
}
