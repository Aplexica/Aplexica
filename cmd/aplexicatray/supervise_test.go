package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// superviseStatus must keep re-running the status feed (reconnecting to a
// restarted daemon) until the context is cancelled — instead of letting the
// feed close after one exit, which quits the whole tray.
func TestSuperviseStatus_ReconnectsUntilCancel(t *testing.T) {
	var calls int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runOnce := func(c context.Context) error {
		// Each call simulates `aplexica status --watch` exiting because the
		// daemon went away (a restart). We expect to be retried.
		if atomic.AddInt32(&calls, 1) >= 3 {
			cancel() // simulate the user quitting the tray after a few reconnects
		}
		return errors.New("daemon gone")
	}

	superviseStatus(ctx, runOnce, time.Millisecond, 4*time.Millisecond)

	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("runOnce called %d times, want 3 (reconnect until ctx cancelled)", got)
	}
}

// A clean exit that coincides with context cancellation (the user quit) must
// NOT trigger a reconnect.
func TestSuperviseStatus_NoReconnectAfterCleanCancel(t *testing.T) {
	var calls int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runOnce := func(c context.Context) error {
		atomic.AddInt32(&calls, 1)
		cancel()
		return nil // clean exit
	}

	superviseStatus(ctx, runOnce, time.Millisecond, time.Millisecond)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("runOnce called %d times, want 1 (cancel during run → no reconnect)", got)
	}
}

// If the context is already cancelled, the supervisor must not run anything.
func TestSuperviseStatus_AlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls int32
	superviseStatus(ctx, func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	}, time.Millisecond, time.Millisecond)
	if calls != 0 {
		t.Fatalf("runOnce called %d times after cancel, want 0", calls)
	}
}
