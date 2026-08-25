package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// The tray's snapshot channel is created and closed by exactly one goroutine in
// main.go:
//
//	snapshots := make(chan StatusSnapshot, 1)
//	go func() {
//		defer close(snapshots)
//		superviseStatus(ctx, ...)
//	}()
//
// and menu.go's run() treats a CLOSED snapshots channel as "quit the tray".
// These tests pin the invariant that makes that safe: superviseStatus must
// never return — and therefore the deferred close must never fire — while the
// context is still live, no matter how the feed fails.

// feedCloseHarness replicates main.go's producer wiring verbatim (a
// close-on-return goroutine around superviseStatus) so the test exercises the
// real supervisor, not a reimplementation of it.
func feedCloseHarness(ctx context.Context, runOnce func(context.Context) error,
	minBackoff, maxBackoff time.Duration) (feed <-chan struct{}, done <-chan struct{}) {

	ch := make(chan struct{}, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer close(ch)
		superviseStatus(ctx, runOnce, minBackoff, maxBackoff)
	}()
	return ch, finished
}

// A feed that keeps dying instantly (the daemon is down, the status helper is
// missing, the child exits at once) must NOT make superviseStatus return, which
// is what would close the snapshots channel and quit the tray via
// menu.go run()'s `case s, ok := <-snapshots; if !ok { systray.Quit() }`.
// There is no attempt counter and no backoff-exhaustion exit in supervise.go —
// this test fails if one is ever introduced.
func TestSuperviseStatus_FeedNeverClosesWhileContextLive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	runOnce := func(context.Context) error {
		// Alternate clean exit / error exit: both are "the feed dropped".
		if atomic.AddInt32(&calls, 1)%2 == 0 {
			return nil
		}
		return errors.New("status helper exited")
	}

	feed, done := feedCloseHarness(ctx, runOnce, time.Millisecond, 2*time.Millisecond)

	// Give the supervisor plenty of failure cycles.
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case <-feed:
			t.Fatalf("snapshots channel closed while ctx was live after %d feed failures; "+
				"menu.go run() would have called systray.Quit() (clean exit 0, launchd "+
				"KeepAlive{SuccessfulExit:false} would NOT respawn)", atomic.LoadInt32(&calls))
		case <-done:
			t.Fatalf("superviseStatus returned while ctx was live after %d feed failures",
				atomic.LoadInt32(&calls))
		case <-deadline:
			if got := atomic.LoadInt32(&calls); got < 2 {
				t.Fatalf("runOnce called %d times, expected repeated reconnects", got)
			}
			// Now cancel: the supervisor must return promptly and the feed close.
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("superviseStatus did not return after ctx cancel")
			}
			select {
			case <-feed:
			case <-time.After(time.Second):
				t.Fatal("snapshots channel not closed after supervisor returned")
			}
			return
		}
	}
}

// A feed that returns INSTANTLY on every attempt must not spin hot: the
// exponential backoff bounds the reconnect rate. (Constraint 4: "no busy-loop".)
func TestSuperviseStatus_InstantFailuresAreRateLimited(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	go func() {
		superviseStatus(ctx, func(context.Context) error {
			atomic.AddInt32(&calls, 1)
			return errors.New("instant exit")
		}, 5*time.Millisecond, 20*time.Millisecond)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()
	got := atomic.LoadInt32(&calls)
	// Backoff 5,10,20,20,... over 200ms admits ~12 attempts; a hot loop would be
	// orders of magnitude more.
	if got > 40 {
		t.Fatalf("runOnce called %d times in 200ms — supervisor is busy-looping", got)
	}
	if got < 2 {
		t.Fatalf("runOnce called %d times in 200ms — supervisor is not reconnecting", got)
	}
}

// The ONLY exit from superviseStatus is context cancellation. Pin that the
// caller can therefore attribute a closed feed to a cancelled ctx: whenever the
// feed closes, ctx.Err() is already non-nil. If this ever fails, menu.go's
// `!ok -> systray.Quit()` branch has become an independent, spurious clean-exit
// path.
func TestSuperviseStatus_FeedCloseImpliesContextCancelled(t *testing.T) {
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		var calls int32
		runOnce := func(context.Context) error {
			if atomic.AddInt32(&calls, 1) >= 3 {
				go cancel()
			}
			return errors.New("feed dropped")
		}
		feed, _ := feedCloseHarness(ctx, runOnce, time.Microsecond, time.Microsecond)
		select {
		case <-feed:
			if ctx.Err() == nil {
				t.Fatalf("iteration %d: feed closed with a live context", i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: feed never closed after cancel", i)
		}
		cancel()
	}
}
