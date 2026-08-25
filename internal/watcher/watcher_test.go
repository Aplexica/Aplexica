package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWatcher_DetectsCreateAndWrite(t *testing.T) {
	tmp := t.TempDir()

	var paths []string
	var mu sync.Mutex
	d := NewDebouncer(shortQuiet, func(p string) {
		mu.Lock()
		paths = append(paths, filepath.Base(p))
		mu.Unlock()
	})
	defer d.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := NewWatcher(tmp, d)
	require.NoError(t, err)
	defer w.Close()

	go w.Run(ctx)

	time.Sleep(50 * time.Millisecond)

	target := filepath.Join(tmp, "CLAUDE.md")
	require.NoError(t, os.WriteFile(target, []byte("# hi\n"), 0o644))

	time.Sleep(shortQuiet * 4)

	mu.Lock()
	require.Contains(t, paths, "CLAUDE.md",
		"watcher should report the created file via the debouncer")
	mu.Unlock()
}

func TestWatcher_IgnoresSubdirectoryEvents(t *testing.T) {
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "sub")
	require.NoError(t, os.Mkdir(sub, 0o755))

	var fired int32
	d := NewDebouncer(shortQuiet, func(p string) {
		atomic.AddInt32(&fired, 1)
	})
	defer d.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := NewWatcher(tmp, d)
	require.NoError(t, err)
	defer w.Close()

	go w.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(sub, "ignored.md"), []byte("x"), 0o644))
	time.Sleep(shortQuiet * 4)

	require.Equal(t, int32(0), atomic.LoadInt32(&fired),
		"events in subdirectories must not reach the debouncer (v0.5.0 is non-recursive)")
}

func TestWatcher_ContextCancelStopsLoop(t *testing.T) {
	tmp := t.TempDir()

	d := NewDebouncer(shortQuiet, func(string) {})
	defer d.Stop()

	ctx, cancel := context.WithCancel(context.Background())

	w, err := NewWatcher(tmp, d)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancel")
	}
	require.NoError(t, w.Close())
}

func TestWatcher_NewWatcher_NonexistentDir_IsError(t *testing.T) {
	d := NewDebouncer(shortQuiet, func(string) {})
	defer d.Stop()

	_, err := NewWatcher("/does/not/exist", d)
	require.Error(t, err)
}
