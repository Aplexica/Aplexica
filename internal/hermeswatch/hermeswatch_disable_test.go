package hermeswatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/stretchr/testify/require"
)

// captureLogger records Error messages so the test can assert the watcher
// logged a single "disabled" line instead of one tick-failed line every 5s.
type captureLogger struct {
	mu     sync.Mutex
	errors []string
}

func (c *captureLogger) Info(string, ...any) {}
func (c *captureLogger) Error(msg string, _ ...any) {
	c.mu.Lock()
	c.errors = append(c.errors, msg)
	c.mu.Unlock()
}
func (c *captureLogger) errorMsgs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.errors...)
}

// A hermeswatch loop pointed at a stale 0-byte (non-Hermes) state.db must
// DISABLE itself after the first tick — not fail-and-retry every interval,
// which is what flooded the daemon log with "hermeswatch tick failed" forever.
func TestRun_DisablesOnNonHermesDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	require.NoError(t, os.WriteFile(dbPath, nil, 0o644)) // 0-byte: the dev-Mac case

	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}
	lg := &captureLogger{}

	w := &Watcher{
		Adapter:   a,
		Store:     store,
		DBPath:    dbPath,
		Interval:  20 * time.Millisecond, // tiny: a spinning loop would log dozens of times
		Direction: DirectionBoth,
		StateFile: filepath.Join(t.TempDir(), "hw.state.json"),
		Logger:    lg,
	}

	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()

	select {
	case err := <-done:
		require.NoError(t, err, "Run should return nil after self-disabling")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not self-disable on a non-Hermes DB; it is spinning/retrying forever")
	}

	// Let any (incorrectly) still-running ticker prove itself by spamming.
	time.Sleep(100 * time.Millisecond)
	msgs := lg.errorMsgs()
	require.LessOrEqual(t, len(msgs), 2, "expected a one-time disable, got spam: %v", msgs)
	joined := strings.Join(msgs, "|")
	require.Contains(t, joined, "disabled", "should log a clear one-time 'disabled' message")
}
