//go:build tray && !windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Exit status during TEARDOWN.
//
// The tray must exit 0 no matter which route ends it. The LaunchAgent carries
// KeepAlive{SuccessfulExit:false}, so a NON-zero exit makes launchd respawn the
// tray — and macOS SIGTERMs every job at logout, i.e. exactly when a respawn is
// most damaging because it can trigger crash-loop disablement.
//
// The hazard these tests pin is narrow and easy to reintroduce: whoever ends
// the shutdown must NOT restore the OS default disposition for SIGINT/SIGTERM
// at the START of teardown, because a SECOND signal routinely lands inside the
// teardown window and would then kill the process with 128+SIGTERM = 143:
//
//	a traycontrol `quit-for-update` on the private socket → cancel(), then
//	`launchctl unload ~/Library/LaunchAgents/com.aplexica.tray.plist` — from
//	`aplexica tray uninstall`, an operator, or a package upgrade — whose
//	SIGTERM lands while the tray is still tearing down.
//
// The in-tree self-updater that used to drive that exact pair is gone (see
// the package comment on internal/update: the apply path was removed and only
// advisory discovery remains). The hazard is not: the tray still accepts
// `quit-for-update`, and logout SIGTERMs every job in the session regardless.
// The probe below reproduces the ordering directly rather than through any
// caller, so these tests keep pinning it whoever the caller turns out to be.
//
// signal.NotifyContext does not untrap from its goroutine (verified against
// go1.26.5 $GOROOT/src/os/signal/signal.go: the goroutine only cancels;
// Stop(c.ch) lives solely in the returned stop func), and neither may we.
//
// A unit test cannot express "the process died from a signal", so this drives a
// real child process — the test binary re-executing itself — and reads its wait
// status. Three variants are measured so the table is self-verifying: the
// pre-change baseline, a FROZEN COPY of the regression, and the production
// function.

const (
	// probeEnvVar carries "<variant>/<scenario>" into the child.
	probeEnvVar    = "APLEXICATRAY_SHUTDOWN_PROBE"
	probeChildTest = "TestShutdownExitStatusProbeChild"

	// Variants: which signal-handling wiring the child installs.
	probeVariantNotifyContext = "notify-context" // pre-change main()
	probeVariantUntrapping    = "untrapping"     // the reviewed regression
	probeVariantProduction    = "production"     // watchShutdownSignals

	// Scenarios: how the teardown starts, before the signal that must not
	// be allowed to kill the process.
	probeScenarioSecondSignal     = "second-signal"      // signal → teardown → signal
	probeScenarioCancelThenSignal = "cancel-then-signal" // quit-for-update → teardown → launchd SIGTERM

	// exitTerminatedBySIGTERM is what a shell reports for a process killed
	// by an untrapped SIGTERM (128 + 15).
	exitTerminatedBySIGTERM = 143
)

// untrappingWatchShutdownSignals is a FROZEN COPY of the reviewed regression:
// signal.Stop from inside the watcher goroutine, on both select branches. It
// exists only so the table below proves the probe can actually detect the bug
// (a probe that reports exit 0 for everything would be worthless). Do not
// "fix" it — fix watchShutdownSignals in shutdown.go, which this file also
// measures.
func untrappingWatchShutdownSignals(ctx context.Context, cancel context.CancelFunc,
	rec *shutdownRecorder) func() {

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, signalsToTrap...)
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { signal.Stop(signals) }) }
	go func() {
		select {
		case received := <-signals:
			stop()
			rec.record(reasonSignalPrefix + received.String())
			cancel()
		case <-ctx.Done():
			stop()
		}
	}()
	return stop
}

// TestShutdownExitStatus_SecondSignalDuringTeardownStillExitsZero is the
// headline assertion: with the production wiring, neither teardown route lets a
// signal arriving mid-teardown turn into a non-zero exit.
func TestShutdownExitStatus_SecondSignalDuringTeardownStillExitsZero(t *testing.T) {
	for _, scenario := range []string{probeScenarioSecondSignal, probeScenarioCancelThenSignal} {
		t.Run(scenario, func(t *testing.T) {
			status, detail := runShutdownProbe(t, probeVariantProduction, scenario)
			if status != 0 {
				t.Fatalf("tray exited %d (%s) when a signal arrived during teardown; it must exit 0.\n"+
					"launchd's KeepAlive{SuccessfulExit:false} respawns the tray on any non-zero exit, "+
					"and macOS SIGTERMs every job at logout — that is the v0.116 crash-loop-disable hazard. "+
					"Keep the signals trapped for the whole teardown: do NOT call signal.Stop from inside "+
					"the watcher goroutine (signal.NotifyContext does not either).", status, detail)
			}
		})
	}
}

// TestShutdownExitStatus_VariantTable reproduces the reviewer's three-variant
// measurement in full, so the fix is anchored against both the pre-change
// baseline and the regression it replaced.
//
//	variant                       second-signal  cancel-then-signal
//	notify-context (pre-change)   0              143
//	untrapping (regression)       143            143
//	production (fixed)            0              0
//
// The pre-change 143 in the right-hand column is not a typo: main used to hand
// traycontrol.NewServer the CancelFunc returned by signal.NotifyContext, which
// is signalCtx.stop — it cancels AND calls signal.Stop. So a quit-for-update
// untrapped the signals before `launchctl unload`'s SIGTERM arrived. The
// production row fixes that too.
func TestShutdownExitStatus_VariantTable(t *testing.T) {
	for _, tc := range []struct {
		variant  string
		scenario string
		want     int
		why      string
	}{
		{probeVariantNotifyContext, probeScenarioSecondSignal, 0,
			"signal.NotifyContext's goroutine only cancels; the signals stay trapped through teardown"},
		{probeVariantNotifyContext, probeScenarioCancelThenSignal, exitTerminatedBySIGTERM,
			"the CancelFunc NotifyContext returns is signalCtx.stop, which also calls signal.Stop"},
		{probeVariantUntrapping, probeScenarioSecondSignal, exitTerminatedBySIGTERM,
			"the regression stops signal delivery on the signal branch, before teardown runs"},
		{probeVariantUntrapping, probeScenarioCancelThenSignal, exitTerminatedBySIGTERM,
			"the regression stops signal delivery on the ctx.Done() branch too"},
		{probeVariantProduction, probeScenarioSecondSignal, 0,
			"the watcher must never untrap; main's deferred stop is the only one"},
		{probeVariantProduction, probeScenarioCancelThenSignal, 0,
			"quit-for-update must survive the launchctl-unload SIGTERM that follows it"},
	} {
		t.Run(tc.variant+"/"+tc.scenario, func(t *testing.T) {
			status, detail := runShutdownProbe(t, tc.variant, tc.scenario)
			if status != tc.want {
				t.Fatalf("exit status = %d (%s), want %d — %s", status, detail, tc.want, tc.why)
			}
			t.Logf("%-14s %-18s exit %d (%s)", tc.variant, tc.scenario, status, detail)
		})
	}
}

// runShutdownProbe re-executes this test binary in probe mode and returns the
// child's wait status, normalised the way a shell reports it (128+signal for a
// process killed by a signal).
func runShutdownProbe(t *testing.T, variant, scenario string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+probeChildTest+"$")
	cmd.Env = append(os.Environ(), probeEnvVar+"="+variant+"/"+scenario)
	out, err := cmd.CombinedOutput()
	if cmd.ProcessState == nil {
		t.Fatalf("probe %s/%s never ran: %v\n%s", variant, scenario, err, out)
	}
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("probe %s/%s: unexpected wait status type %T", variant, scenario, cmd.ProcessState.Sys())
	}
	detail := "exited normally"
	if err != nil {
		detail = err.Error()
	}
	if ws.Signaled() {
		status := 128 + int(ws.Signal())
		if status != exitTerminatedBySIGTERM {
			t.Logf("probe %s/%s output:\n%s", variant, scenario, out)
		}
		return status, detail
	}
	status := ws.ExitStatus()
	if status != 0 {
		t.Logf("probe %s/%s output:\n%s", variant, scenario, out)
	}
	return status, detail
}

// TestShutdownExitStatusProbeChild is the child half. Without the env var it is
// an ordinary skipped test, so a normal `go test` run ignores it.
func TestShutdownExitStatusProbeChild(t *testing.T) {
	spec := os.Getenv(probeEnvVar)
	if spec == "" {
		t.Skip("not a shutdown-exit-status probe child")
	}
	variant, scenario, ok := strings.Cut(spec, "/")
	if !ok {
		probeFatal("malformed probe spec %q, want <variant>/<scenario>", spec)
	}
	runShutdownProbeChild(variant, scenario)
}

// runShutdownProbeChild installs one signal-handling variant, drives one
// teardown scenario, delivers a signal INSIDE the teardown window and — if it
// is still alive — exits 0. Being killed by that signal is the failure the
// parent measures.
func runShutdownProbeChild(variant, scenario string) {
	// teardownWindow gives the watcher goroutine time to run its branch to
	// completion (including an untrap, if it does one) before the signal
	// that must not kill us.
	const teardownWindow = 150 * time.Millisecond
	// survivalWindow is how long we stay alive after that signal. An
	// untrapped SIGTERM kills within microseconds, so anything comfortably
	// above scheduler noise proves the signal was trapped.
	const survivalWindow = 250 * time.Millisecond

	rec := &shutdownRecorder{}
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	var ctx context.Context
	var stop func()
	// quitForUpdateLike is whatever main hands to traycontrol.NewServer in
	// that variant — the second cancel source.
	var quitForUpdateLike func()

	switch variant {
	case probeVariantNotifyContext:
		// Pre-change main(): the SAME CancelFunc went to traycontrol.
		sigCtx, sigStop := signal.NotifyContext(parent, signalsToTrap...)
		ctx, stop, quitForUpdateLike = sigCtx, sigStop, sigStop
	case probeVariantUntrapping:
		child, cancel := context.WithCancel(parent)
		ctx = child
		stop = untrappingWatchShutdownSignals(child, cancel, rec)
		quitForUpdateLike = quitForUpdate(cancel, rec)
	case probeVariantProduction:
		child, cancel := context.WithCancel(parent)
		ctx = child
		stop = watchShutdownSignals(child, cancel, rec)
		quitForUpdateLike = quitForUpdate(cancel, rec)
	default:
		probeFatal("unknown probe variant %q", variant)
	}
	// main defers its stop; nothing may untrap before this point.
	defer stop()

	switch scenario {
	case probeScenarioSecondSignal:
		signalSelf()
	case probeScenarioCancelThenSignal:
		quitForUpdateLike()
	default:
		probeFatal("unknown probe scenario %q", scenario)
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		probeFatal("context was never cancelled by %s", scenario)
	}

	// ---- teardown window: the tray is unwinding its defers here ----
	time.Sleep(teardownWindow)
	signalSelf()
	time.Sleep(survivalWindow)
	// Still alive: the signal was trapped, exactly as before the change.
	os.Exit(0)
}

func signalSelf() {
	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		probeFatal("FindProcess: %v", err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		probeFatal("send SIGTERM to self: %v", err)
	}
}

// probeFatal ends the CHILD with a status the parent will never mistake for
// either a clean exit or a signal death.
func probeFatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "probe child: "+format+"\n", args...)
	os.Exit(97)
}
