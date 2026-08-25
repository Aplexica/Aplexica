package main

import (
	"context"
	"log"
	"time"
)

const (
	// statusReconnectMinBackoff and statusReconnectMaxBackoff bound the wait
	// between attempts to re-attach the tray's `aplexica status --watch` feed
	// after it drops (typically a daemon restart). Short enough that the icon
	// resumes within a few seconds of the daemon coming back; capped so a
	// persistently-down daemon doesn't spin.
	statusReconnectMinBackoff = time.Second
	statusReconnectMaxBackoff = 5 * time.Second
)

// superviseStatus keeps the tray's status feed alive across daemon restarts.
//
// The tray drives its icon from `aplexica status --watch` and QUITS when that
// feed closes (menu.go: a closed snapshots channel calls systray.Quit). A
// daemon restart makes the watch child exit, which — without this supervisor —
// closed the feed and tore down the whole tray, so the menu-bar icon vanished
// until the user's next login. That was the "every restart kills the tray" bug.
//
// Instead we RE-RUN the feed: when runOnce returns while the context is still
// live (the daemon went away, not a user-quit), we wait a short, exponentially
// backed-off interval and reconnect. The loop ends ONLY when the context is
// cancelled — a genuine tray shutdown / user-quit — so a clean quit never
// reconnects (preserving "user-quit must not respawn"). A run that lasted at
// least maxBackoff was a healthy connection, so we reset the backoff: the next
// outage reconnects promptly rather than at an interval inflated by an earlier one.
func superviseStatus(ctx context.Context, runOnce func(context.Context) error, minBackoff, maxBackoff time.Duration) {
	if minBackoff <= 0 {
		minBackoff = time.Second
	}
	if maxBackoff < minBackoff {
		maxBackoff = minBackoff
	}
	backoff := minBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := runOnce(ctx)
		if ctx.Err() != nil {
			return // genuine shutdown — do not reconnect
		}
		if time.Since(started) >= maxBackoff {
			backoff = minBackoff // the last run was a healthy connection
		}
		if err != nil {
			log.Printf("tray: status feed ended (%v); reconnecting in %s", err, backoff)
		} else {
			log.Printf("tray: status feed ended; reconnecting in %s", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}
