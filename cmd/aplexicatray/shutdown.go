//go:build tray

package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// signalsToTrap mirrors the set main previously handed to
// signal.NotifyContext. Keep them in sync: these are the signals whose arrival
// must produce a clean, exit-0 tray shutdown.
var signalsToTrap = []os.Signal{os.Interrupt, syscall.SIGTERM}

// Shutdown diagnostics.
//
// The tray used to die under launchd leaving zero evidence. Two things made
// that possible: the LaunchAgent discarded the tray's stderr (fixed in
// internal/trayinstall, which now points StandardOutPath/StandardErrorPath at
// the tray's log dir), and every route out of the event loop was silent.
//
// There are exactly three such routes, and they must be distinguishable in a
// log after the fact:
//
//	menu.go clickLoop  — the user picked the Quit menu item
//	menu.go run        — ctx.Done()
//	menu.go run        — the status feed (snapshots channel) closed
//
// The ctx.Done() route is ambiguous on its own: main hands the SAME
// context.CancelFunc to signal handling and to the traycontrol socket, so a
// logout SIGTERM and an updater's `quit-for-update` look identical at the
// receiving end. shutdownRecorder closes that gap by having each cancel SOURCE
// record itself before cancelling.
const (
	// shutdownLogPrefix anchors every shutdown line for grepping:
	//   grep 'tray shutdown: ' ~/.aplexica/logs/tray.launchd.log
	shutdownLogPrefix = "tray shutdown: "

	reasonUserQuit = "user clicked the Quit menu item"
	// reasonSystrayExit covers main's onExit callback, which cancels the
	// context after the systray event loop has already ended (e.g. macOS
	// terminating the app). First-writer-wins keeps the more specific causes.
	reasonSystrayExit = "systray event loop exited (onExit)"
	// reasonFeedClosed is reported ONLY for a feed that ended while the
	// context was still live. superviseStatus loops `for ctx.Err() == nil`,
	// and main closes the snapshots channel only once it returns, so in
	// production a closed feed arrives with ctx already done — and menu.go
	// attributes that to the recorded cancel source instead, rather than
	// letting a random select win misname a plain SIGTERM.
	reasonFeedClosed       = "status feed closed (the supervised `aplexica status --watch` feed ended)"
	reasonTraycontrolQuit  = "traycontrol quit-for-update"
	reasonCancelUnrecorded = "cancel source not recorded"
	reasonContextCancelled = "context cancelled: "
	// reasonSignalPrefix is completed with the signal's own name, e.g.
	// "signal terminated" (SIGTERM) or "signal interrupt" (SIGINT).
	reasonSignalPrefix = "signal "
)

// shutdownRecorder remembers WHY the tray's context was cancelled.
//
// Written by the signal goroutine and by the traycontrol socket goroutine;
// read by the UI loop in menu.go — hence the mutex. First writer wins: launchd
// SIGTERMs a job it is booting out, so a SIGTERM arriving just after a
// quit-for-update must not overwrite the cause that actually started the
// shutdown.
type shutdownRecorder struct {
	mu       sync.Mutex
	recorded string
}

func (r *shutdownRecorder) record(cause string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recorded == "" {
		r.recorded = cause
	}
}

// cause returns the recorded shutdown source, or reasonCancelUnrecorded when
// the context was cancelled by something that did not identify itself.
func (r *shutdownRecorder) cause() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recorded == "" {
		return reasonCancelUnrecorded
	}
	return r.recorded
}

func (r *shutdownRecorder) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded = ""
}

// shutdownSource is the process-wide recorder. The UI loop reads it from
// menu.go's ctx.Done() branch.
var shutdownSource = &shutdownRecorder{}

// logShutdown emits the single greppable line for one shutdown route.
func logShutdown(reason string) {
	log.Printf("%s%s", shutdownLogPrefix, reason)
}

// watchShutdownSignals replaces signal.NotifyContext(os.Interrupt, SIGTERM):
// the first of those signals cancels ctx, exactly as before, but it records
// WHICH signal arrived before cancelling, so the ctx.Done() branch can say
// "signal terminated" instead of shrugging.
//
// Exit status is deliberately untouched: the signal is trapped and turned into
// the same clean, exit-0 shutdown as before. macOS SIGTERMs every job at
// logout, and the LaunchAgent carries KeepAlive{SuccessfulExit:false}, so an
// exit-non-zero-on-SIGTERM would respawn the tray during logout and risks
// launchd's crash-loop disable.
//
// That is why the goroutine below NEVER calls stop(). Untrapping restores the
// OS default disposition, and a second SIGTERM routinely lands INSIDE the
// teardown window: macOS logout SIGTERMs every job in the session, and
// anything that unloads the tray's LaunchAgent — `aplexica tray uninstall`,
// an operator running `launchctl unload …/com.aplexica.tray.plist`, a package
// upgrade — sends its own on top of whatever started the teardown. A second
// signal after untrapping would kill the process with 128+SIGTERM = 143 and
// have launchd respawn it mid-shutdown. signal.NotifyContext has the same property: its
// goroutine only cancels, and Stop(c.ch) lives solely in the returned stop
// func (go1.26.5 $GOROOT/src/os/signal/signal.go). Untrapping is main's job,
// via the deferred stop, once main is actually done.
//
// The returned func stops signal delivery; it is safe to call more than once.
func watchShutdownSignals(ctx context.Context, cancel context.CancelFunc, rec *shutdownRecorder) func() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, signalsToTrap...)
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { signal.Stop(signals) }) }
	go func() {
		select {
		case received := <-signals:
			rec.record(reasonSignalPrefix + received.String())
			cancel()
		case <-ctx.Done():
		}
	}()
	return stop
}

// quitForUpdate wraps the cancel func handed to traycontrol.NewServer so a
// `quit-for-update` command over the private socket is attributable, instead of
// being indistinguishable from a signal at ctx.Done().
func quitForUpdate(cancel context.CancelFunc, rec *shutdownRecorder) func() {
	return func() {
		rec.record(reasonTraycontrolQuit)
		cancel()
	}
}
