//go:build tray && !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// writeFakeStatusChild writes an executable stand-in for
// `aplexica-status status --watch --json --interval 5s`. It appends one line
// to a marker file per invocation (so the test can count respawns), emits a
// single JSON snapshot on stdout, and then exits with the given behaviour.
func writeFakeStatusChild(t *testing.T, marker, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-status")
	script := "#!/bin/sh\n" +
		"echo run >> " + marker + "\n" +
		`echo '{"timestamp":"2026-07-26T00:00:00Z","daemonAvailable":true,"conflicts":[],"conflictCount":0}'` + "\n" +
		body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// runFeedLikeMain reproduces main()'s status-feed plumbing verbatim: one
// goroutine that supervises runStatus and closes the snapshots channel when
// (and only when) superviseStatus returns.
func runFeedLikeMain(ctx context.Context) (chan StatusSnapshot, *atomic.Bool) {
	snapshots := make(chan StatusSnapshot, 1)
	closed := &atomic.Bool{}
	go func() {
		defer func() {
			closed.Store(true)
			close(snapshots)
		}()
		superviseStatus(ctx, func(c context.Context) error {
			return runStatus(c, snapshots)
		}, time.Millisecond, 5*time.Millisecond)
	}()
	return snapshots, closed
}

// drain keeps the snapshot channel moving and reports whether it was ever
// observed CLOSED — the condition that makes tray.run call systray.Quit().
func drain(t *testing.T, snapshots chan StatusSnapshot, sawClose *atomic.Bool, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case _, ok := <-snapshots:
			if !ok {
				sawClose.Store(true)
				return
			}
		case <-deadline:
			return
		}
	}
}

func countRuns(t *testing.T, marker string) int {
	t.Helper()
	b, err := os.ReadFile(marker)
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

// The status child exiting — cleanly (exit 0), with an error status, or killed
// outright — must NOT close the tray's snapshot channel. tray.run quits the
// whole tray when that channel closes (menu.go: `case s, ok := <-snapshots`),
// so a closed channel here would be a spurious tray exit.
func TestStatusChildExit_DoesNotCloseFeed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"clean exit 0", "exit 0"},
		{"error exit 1", "exit 1"},
		{"killed by signal", "kill -9 $$"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "runs")
			statusWatchPath = writeFakeStatusChild(t, marker, tc.body)
			t.Cleanup(func() { statusWatchPath = "" })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			snapshots, closed := runFeedLikeMain(ctx)

			sawClose := &atomic.Bool{}
			drain(t, snapshots, sawClose, 300*time.Millisecond)

			if sawClose.Load() || closed.Load() {
				t.Fatalf("snapshot feed CLOSED after the status child exited (%s): "+
					"tray.run would call systray.Quit()", tc.name)
			}
			if runs := countRuns(t, marker); runs < 2 {
				t.Fatalf("status child spawned %d times, want >= 2 (supervisor must reconnect)", runs)
			}
			cancel()
			// The feed must close only now, on genuine shutdown.
			deadline := time.After(2 * time.Second)
			for !closed.Load() {
				select {
				case <-deadline:
					t.Fatal("feed did not close after ctx cancel")
				default:
					time.Sleep(2 * time.Millisecond)
				}
			}
		})
	}
}

// The reconnect must be rate-limited: a child that dies instantly, forever,
// must not spin the CPU. With a 1ms/5ms backoff a 300ms window admits at most
// a few hundred respawns; the real tray's 1s/5s bounds are 1000x slower still.
func TestStatusChildExit_ReconnectIsRateLimited(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "runs")
	statusWatchPath = writeFakeStatusChild(t, marker, "exit 0")
	t.Cleanup(func() { statusWatchPath = "" })

	ctx, cancel := context.WithCancel(context.Background())
	snapshots, closed := runFeedLikeMain(ctx)
	sawClose := &atomic.Bool{}
	drain(t, snapshots, sawClose, 300*time.Millisecond)
	cancel()

	// Wait for the supervisor goroutine to actually exit before the test
	// returns: t.Cleanup below mutates the package-global statusWatchPath,
	// and that goroutine's runStatus (main.go:327) reads it on every
	// reconnect attempt. Without this join, the read and the write race.
	deadline := time.After(2 * time.Second)
	for !closed.Load() {
		select {
		case <-deadline:
			t.Fatal("feed did not close after ctx cancel")
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}

	runs := countRuns(t, marker)
	if runs > 200 {
		t.Fatalf("status child respawned %d times in 300ms — backoff is not bounding the loop", runs)
	}
	t.Logf("respawns in 300ms: %d", runs)
}
