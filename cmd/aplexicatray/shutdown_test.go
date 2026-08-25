//go:build tray

package main

import (
	"bytes"
	"context"
	"go/build"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Every route out of the tray's event loop used to be SILENT: clickLoop's Quit
// case, run()'s ctx.Done() case and run()'s closed-feed case all called
// systray.Quit() without logging a thing, and the two cancel sources (a real
// SIGTERM and a traycontrol `quit-for-update`) were literally the same
// context.CancelFunc, so they were indistinguishable at ctx.Done(). These tests
// pin one distinct, greppable line per route plus the cancel-source attribution.

// syncBuffer is a race-safe log sink: the tray writes from its run/click
// goroutines while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// reset lets one sink serve many iterations of a repeated test without
// registering a t.Cleanup per iteration.
func (s *syncBuffer) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Reset()
}

func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	sink := &syncBuffer{}
	previous := log.Writer()
	log.SetOutput(sink)
	t.Cleanup(func() { log.SetOutput(previous) })
	return sink
}

// resetShutdownSource isolates a test from the package-level recorder.
func resetShutdownSource(t *testing.T) {
	t.Helper()
	shutdownSource.clear()
	t.Cleanup(shutdownSource.clear)
}

func waitForLog(t *testing.T, sink *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(sink.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("log never contained %q\n--- captured ---\n%s", want, sink.String())
}

// --- the recorder itself -------------------------------------------------

// First writer wins: launchd SIGTERMs a job it is booting out, so a SIGTERM
// arriving right after a traycontrol quit-for-update must not overwrite the
// true cause.
func TestShutdownRecorderFirstWriterWins(t *testing.T) {
	rec := &shutdownRecorder{}
	if got := rec.cause(); got != reasonCancelUnrecorded {
		t.Errorf("empty recorder cause = %q, want %q", got, reasonCancelUnrecorded)
	}
	rec.record(reasonTraycontrolQuit)
	rec.record(reasonSignalPrefix + "terminated")
	if got := rec.cause(); got != reasonTraycontrolQuit {
		t.Errorf("cause = %q, want the first recorded cause %q", got, reasonTraycontrolQuit)
	}
}

// The recorder is written by a signal goroutine and a socket goroutine, and
// read by the UI loop. Run it hot so `-race` has something to complain about.
func TestShutdownRecorderIsRaceFree(t *testing.T) {
	rec := &shutdownRecorder{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); rec.record(reasonSignalPrefix + "terminated") }()
		go func() { defer wg.Done(); _ = rec.cause() }()
	}
	wg.Wait()
	if got := rec.cause(); got != reasonSignalPrefix+"terminated" {
		t.Errorf("cause = %q after concurrent writes, want the recorded signal cause", got)
	}
}

func TestShutdownReasonsAreDistinctAndGreppable(t *testing.T) {
	seen := map[string]bool{}
	for _, reason := range []string{
		reasonUserQuit,
		reasonSystrayExit,
		reasonFeedClosed,
		reasonTraycontrolQuit,
		reasonCancelUnrecorded,
		reasonSignalPrefix + "terminated",
	} {
		if reason == "" {
			t.Fatal("empty shutdown reason")
		}
		if seen[reason] {
			t.Errorf("duplicate shutdown reason %q — the routes would be indistinguishable in a log", reason)
		}
		seen[reason] = true
	}
	if shutdownLogPrefix == "" || !strings.HasSuffix(shutdownLogPrefix, " ") {
		t.Errorf("shutdownLogPrefix %q should be a non-empty grep anchor ending in a space", shutdownLogPrefix)
	}
}

// --- cancel-source attribution ------------------------------------------

// The callback handed to traycontrol.NewServer must record itself BEFORE
// cancelling, so the UI loop's ctx.Done() branch can name the source.
func TestQuitForUpdateRecordsTraycontrolSource(t *testing.T) {
	rec := &shutdownRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	quitForUpdate(cancel, rec)()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("quitForUpdate did not cancel the context")
	}
	if got := rec.cause(); got != reasonTraycontrolQuit {
		t.Errorf("cause = %q, want %q", got, reasonTraycontrolQuit)
	}
}

// A real SIGTERM must be recorded AS a signal (naming which one) and must
// cancel the context — without killing the process. The tray's exit-0-on-
// SIGTERM contract depends on the signal being trapped: macOS SIGTERMs every
// job at logout, and a non-zero exit would make KeepAlive respawn the tray
// mid-logout (the v0.116 crash-loop-disable hazard).
func TestSignalWatcherRecordsSignalAndDoesNotKillTheProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX signal delivery to self on windows")
	}
	rec := &shutdownRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := watchShutdownSignals(ctx, cancel, rec)
	defer stop()

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("SIGTERM did not cancel the tray context")
	}
	want := reasonSignalPrefix + syscall.SIGTERM.String()
	if got := rec.cause(); got != want {
		t.Errorf("cause = %q, want %q", got, want)
	}
	// Reaching this line at all is the assertion that the signal was trapped
	// rather than terminating the process.
}

// Cancelling the context by other means (defer cancel(), onExit) must leave the
// watcher goroutine free to exit and must not record a bogus signal.
func TestSignalWatcherStopsWithTheContext(t *testing.T) {
	rec := &shutdownRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	stop := watchShutdownSignals(ctx, cancel, rec)
	cancel()
	stop()
	if got := rec.cause(); got != reasonCancelUnrecorded {
		t.Errorf("cause = %q, want %q for a cancel with no signal", got, reasonCancelUnrecorded)
	}
}

// --- one distinct log line per systray.Quit() route ----------------------

func TestClickLoop_UserQuitLogsItsOwnReason(t *testing.T) {
	resetShutdownSource(t)
	sink := captureLog(t)
	tr := newClickLoopTray()
	done := runClickLoop(t, tr)
	tr.miQuit.ClickedCh <- struct{}{}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("clickLoop did not return after a Quit click")
	}
	waitForLog(t, sink, shutdownLogPrefix+reasonUserQuit)
	if got := shutdownSource.cause(); got != reasonUserQuit {
		t.Errorf("recorded cause = %q, want %q so a follow-on ctx.Done() names the user quit", got, reasonUserQuit)
	}
}

func TestRun_ContextCancelLogsTheSignalSource(t *testing.T) {
	resetShutdownSource(t)
	sink := captureLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		newClickLoopTray().run(ctx, make(chan StatusSnapshot), time.Hour, time.Hour)
	}()
	shutdownSource.record(reasonSignalPrefix + "terminated")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return on ctx cancel")
	}
	waitForLog(t, sink, shutdownLogPrefix+reasonContextCancelled+reasonSignalPrefix+"terminated")
}

func TestRun_ContextCancelLogsTheTraycontrolSource(t *testing.T) {
	resetShutdownSource(t)
	sink := captureLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		newClickLoopTray().run(ctx, make(chan StatusSnapshot), time.Hour, time.Hour)
	}()
	quitForUpdate(cancel, shutdownSource)()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return on a traycontrol quit-for-update")
	}
	waitForLog(t, sink, shutdownLogPrefix+reasonContextCancelled+reasonTraycontrolQuit)
}

func TestRun_ContextCancelWithNoRecordedSourceSaysSo(t *testing.T) {
	resetShutdownSource(t)
	sink := captureLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		newClickLoopTray().run(ctx, make(chan StatusSnapshot), time.Hour, time.Hour)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return on ctx cancel")
	}
	waitForLog(t, sink, shutdownLogPrefix+reasonContextCancelled+reasonCancelUnrecorded)
}

// In PRODUCTION a closed snapshots channel never means "the feed gave up on its
// own". superviseStatus loops `for ctx.Err() == nil` and returns only once the
// context is cancelled; main's producer goroutine then runs its deferred
// close(snapshots). So by the time run() sees `!ok`, ctx.Done() is ALREADY
// closed and both cases of the select are ready — and Go picks between ready
// cases uniformly at random. Reporting the feed there would misattribute a
// plain SIGTERM shutdown roughly half the time, and a diagnostic that lies is
// worse than no diagnostic at all.
//
// One iteration would be a coin flip, so this asserts over many.
func TestRun_ClosedFeedDuringCancelReportsTheCancelSource(t *testing.T) {
	resetShutdownSource(t)
	sink := captureLog(t)
	const iterations = 300
	want := shutdownLogPrefix + reasonContextCancelled + reasonSignalPrefix + "terminated"
	for i := 0; i < iterations; i++ {
		shutdownSource.clear()
		sink.reset()

		ctx, cancel := context.WithCancel(context.Background())
		snapshots := make(chan StatusSnapshot)
		// Production ordering: the supervisor returned BECAUSE ctx was
		// cancelled, and its deferred close fired on the way out. Both
		// select cases are therefore ready before run() even starts.
		shutdownSource.record(reasonSignalPrefix + "terminated")
		cancel()
		close(snapshots)

		done := make(chan struct{})
		go func() {
			defer close(done)
			newClickLoopTray().run(ctx, snapshots, time.Hour, time.Hour)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: run did not return", i)
		}

		got := sink.String()
		if strings.Contains(got, reasonFeedClosed) {
			t.Fatalf("iteration %d of %d: a SIGTERM-driven shutdown was reported as %q.\n"+
				"superviseStatus only returns on ctx cancel, so a closed feed always arrives "+
				"with ctx already done and the select picks at random — the !ok branch must "+
				"consult ctx.Err() and report the recorded cancel source.\n--- captured ---\n%s",
				i, iterations, reasonFeedClosed, got)
		}
		if !strings.Contains(got, want) {
			t.Fatalf("iteration %d of %d: log = %q, want it to contain %q", i, iterations, got, want)
		}
	}
}

func TestRun_ClosedFeedLogsItsOwnReason(t *testing.T) {
	resetShutdownSource(t)
	sink := captureLog(t)
	snapshots := make(chan StatusSnapshot)
	done := make(chan struct{})
	go func() {
		defer close(done)
		newClickLoopTray().run(context.Background(), snapshots, time.Hour, time.Hour)
	}()
	close(snapshots)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return on a closed snapshots channel")
	}
	waitForLog(t, sink, shutdownLogPrefix+reasonFeedClosed)
	// The feed-closed route must not be confusable with a cancel route.
	if strings.Contains(sink.String(), reasonContextCancelled) {
		t.Errorf("closed-feed shutdown also logged a context-cancel reason:\n%s", sink.String())
	}
}

// --- exit status is unchanged -------------------------------------------

// Exit 0 on SIGTERM and on user-quit is load-bearing: KeepAlive
// {SuccessfulExit:false} respawns the tray on any NON-zero exit, and macOS
// SIGTERMs every job at logout — a non-zero exit would respawn the tray during
// logout and can trip launchd's crash-loop disable. Neither main nor any
// shutdown route may exit non-zero, and the only way to do that from this
// binary is os.Exit.
func TestTrayBuildNeverExitsNonZero(t *testing.T) {
	ctx := build.Default
	ctx.BuildTags = []string{"tray"}
	pkg, err := ctx.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("import package with -tags tray: %v", err)
	}
	if len(pkg.GoFiles) == 0 {
		t.Fatal("no tray-tagged files found")
	}
	for _, name := range pkg.GoFiles {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(body), "os.Exit(") {
			t.Errorf("%s calls os.Exit — the tray must return from main so SIGTERM and "+
				"user-quit both exit 0; a non-zero exit makes launchd's "+
				"KeepAlive{SuccessfulExit:false} respawn the tray (v0.116 crash-loop hazard)",
				name)
		}
	}
}
