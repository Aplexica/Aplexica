//go:build windows

package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSource_Conformance_WindowsRDC(t *testing.T) {
	runSourceConformance(t, newWindowsRDCSource)
}

func TestWindowsRDC_DeliversCreateEvent(t *testing.T) {
	dir := t.TempDir()
	s, err := newWindowsRDCSource(dir)
	require.NoError(t, err)
	defer s.Close()

	time.Sleep(100 * time.Millisecond)

	path := filepath.Join(dir, "wintest.md")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	select {
	case ev := <-s.Events():
		require.Equal(t, path, ev.Path)
	case <-time.After(2 * time.Second):
		t.Fatal("Windows RDC did not deliver create event within 2 seconds")
	}
}

// TestWindowsRDC_DeliversSingleCharNameEvent guards against the
// FILE_NOTIFY_INFORMATION header-size mistake: Go's
// unsafe.Sizeof(windows.FileNotifyInformation{}) is 16 (the [1]uint16
// FileName placeholder pads to 4-byte alignment), but the kernel emits
// records as small as 14 bytes for a single-character name. Using the
// padded value as the minimum-buffer check in parseAndEmit silently
// drops every single-character file/directory event — which is what
// made the recursive watcher fail on Windows CI for mkdir -p x/y/z
// chains. Keep this test sharp so the regression can't reappear.
func TestWindowsRDC_DeliversSingleCharNameEvent(t *testing.T) {
	dir := t.TempDir()
	s, err := newWindowsRDCSource(dir)
	require.NoError(t, err)
	defer s.Close()

	time.Sleep(100 * time.Millisecond)

	sub := filepath.Join(dir, "x")
	require.NoError(t, os.Mkdir(sub, 0o755))

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			if ev.Path == sub {
				return
			}
		case <-deadline:
			t.Fatal("Windows RDC did not deliver single-character-name event within 3 seconds")
		}
	}
}

func TestWindowsRDC_OverflowRecovery(t *testing.T) {
	// Synthesize an overflow by calling handleOverflow directly — easier
	// than provoking a real kernel overflow in CI.
	dir := t.TempDir()
	src, err := newWindowsRDCSource(dir)
	require.NoError(t, err)
	s := src.(*windowsRDCSource)
	defer s.Close()

	// Pre-populate the dir.
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644))
	}

	time.Sleep(100 * time.Millisecond)
	// Drain any natural events from the writes above.
	drainEvents(s, 500*time.Millisecond)

	s.handleOverflow()

	// We expect (a) one warning on Errors(), (b) three OpChange events.
	select {
	case warn := <-s.Errors():
		require.Contains(t, warn.Error(), "overflow")
	case <-time.After(time.Second):
		t.Fatal("expected overflow warning on Errors() channel")
	}

	gotPaths := map[string]bool{}
	deadline := time.After(time.Second)
	for len(gotPaths) < 3 {
		select {
		case ev := <-s.Events():
			gotPaths[filepath.Base(ev.Path)] = true
		case <-deadline:
			t.Fatalf("only got %d events; expected 3", len(gotPaths))
		}
	}
	require.True(t, gotPaths["a.md"])
	require.True(t, gotPaths["b.md"])
	require.True(t, gotPaths["c.md"])
}

func drainEvents(s Source, timeout time.Duration) {
	deadline := time.After(timeout)
	for {
		select {
		case <-s.Events():
		case <-deadline:
			return
		}
	}
}
