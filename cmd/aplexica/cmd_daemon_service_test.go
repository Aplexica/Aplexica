//go:build !windows

package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/aplexica/aplexica/internal/daemon"
)

// fakeServiceController stands in for systemd or launchd. Its Start and
// Stop run hooks the test supplies, which bring a stand-in daemon up or
// take it down the way the real manager would.
type fakeServiceController struct {
	managed  bool
	starts   int
	stops    int
	restarts int
	onStart  func() error
	onStop   func() error
}

func (f *fakeServiceController) Managed() bool { return f.managed }

func (f *fakeServiceController) Label() string { return "fake manager" }

func (f *fakeServiceController) Start() error {
	f.starts++
	if f.onStart != nil {
		return f.onStart()
	}
	return nil
}

func (f *fakeServiceController) Stop() error {
	f.stops++
	if f.onStop != nil {
		return f.onStop()
	}
	return nil
}

func (f *fakeServiceController) Restart() error {
	f.restarts++
	return nil
}

// useServiceController makes the daemon commands see ctl as the platform's
// service controller for one test. A nil ctl reproduces a platform that
// ships none, such as Windows.
func useServiceController(t *testing.T, ctl daemon.ServiceController) {
	t.Helper()
	prev := serviceControllerForPlatform
	serviceControllerForPlatform = func() daemon.ServiceController { return ctl }
	t.Cleanup(func() { serviceControllerForPlatform = prev })
}

// recordSpawns replaces the unmanaged start's self-exec with a counter, so
// no test launches a real daemon.
func recordSpawns(t *testing.T) *int {
	t.Helper()
	spawns := 0
	prev := spawnDetachedDaemon
	spawnDetachedDaemon = func(*cobra.Command) error {
		spawns++
		return nil
	}
	prevDir := daemonDir
	t.Cleanup(func() {
		spawnDetachedDaemon = prev
		daemonDir = prevDir
	})
	return &spawns
}

// useServiceTestStateDir points the daemon commands at a fresh state dir
// that is short enough for a unix socket path on macOS. It also turns the
// tray off there, so the already-running path of `daemon start` cannot
// launch a real tray on the machine running the tests.
func useServiceTestStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "darwin" {
		var err error
		dir, err = os.MkdirTemp("/tmp", "apx-svc")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	t.Setenv("APLEXICA_STATE_DIR", dir)
	prev := daemonStateDir
	daemonStateDir = dir
	t.Cleanup(func() { daemonStateDir = prev })
	trayOff := false
	require.NoError(t, daemon.WriteConfig(filepath.Join(dir, "config.json"),
		&daemon.Config{Tray: daemon.TrayConfig{Enabled: &trayOff}}))
	return dir
}

// startStandInDaemon listens on the state dir's control socket with the
// daemon's own control server, which answers probes and shuts itself down
// on a "stop" request the way a running daemon does.
func startStandInDaemon(t *testing.T, dir string) *daemon.ControlServer {
	t.Helper()
	srv := daemon.NewControlServer(filepath.Join(dir, "aplexicad.sock"), &daemon.StatusInfo{PID: 4242}, nil)
	require.NoError(t, srv.Start())
	t.Cleanup(func() { _ = srv.Stop() })
	return srv
}

// standInStopped reports whether the stand-in daemon has shut down: either
// a "stop" reached it over the control socket, or a fake manager's Stop
// hook took it down.
func standInStopped(srv *daemon.ControlServer) bool {
	select {
	case <-srv.Done():
		return true
	default:
		return false
	}
}

// newOutputCmd returns a throwaway command whose output lands in out, so
// tests do not touch the shared daemon commands' writers.
func newOutputCmd(out *bytes.Buffer) *cobra.Command {
	c := &cobra.Command{}
	c.SetOut(out)
	c.SetErr(out)
	return c
}

func runDaemonCommand(c *cobra.Command) (string, error) {
	var out bytes.Buffer
	err := c.RunE(newOutputCmd(&out), nil)
	return out.String(), err
}

// A managed start must go through the service manager and must not also
// self-exec a daemon: that detached child is the process that ran outside
// systemd and died with the caller's session.
func TestDaemonStartManagedGoesThroughTheServiceManager(t *testing.T) {
	dir := useServiceTestStateDir(t)
	spawns := recordSpawns(t)
	ctl := &fakeServiceController{managed: true}
	ctl.onStart = func() error {
		startStandInDaemon(t, dir)
		return nil
	}
	useServiceController(t, ctl)

	out, err := runDaemonCommand(daemonStartCmd)
	require.NoError(t, err)
	require.Equal(t, 1, ctl.starts)
	require.Zero(t, *spawns, "a managed start must not self-exec a daemon outside the manager")
	require.Contains(t, out, "daemon: starting via fake manager")
	require.Contains(t, out, "daemon: running")
}

func TestDaemonStartManagedReportsAServiceManagerFailure(t *testing.T) {
	useServiceTestStateDir(t)
	spawns := recordSpawns(t)
	ctl := &fakeServiceController{managed: true}
	ctl.onStart = func() error { return errors.New("systemctl --user start aplexicad.service: exit status 1") }
	useServiceController(t, ctl)

	_, err := runDaemonCommand(daemonStartCmd)
	require.ErrorContains(t, err, "exit status 1")
	require.Zero(t, *spawns, "a failed managed start must not fall back to a daemon outside the manager")
}

// The manager accepting the start is not evidence the daemon came up. Like
// a managed restart, start must fail non-zero, naming the manager and
// pointing at the logs, when nothing answers in time.
func TestDaemonStartManagedFailsWhenTheDaemonNeverAnswers(t *testing.T) {
	useServiceTestStateDir(t)
	ctl := &fakeServiceController{managed: true}

	var out bytes.Buffer
	err := startManagedDaemon(newOutputCmd(&out), ctl, 100*time.Millisecond)
	require.Error(t, err)
	require.Contains(t, err.Error(), "fake manager accepted the start")
	require.Contains(t, err.Error(), "aplexica daemon logs")
	require.Equal(t, 1, ctl.starts)
}

func TestDaemonStartAlreadyRunningLeavesTheServiceManagerAlone(t *testing.T) {
	dir := useServiceTestStateDir(t)
	startStandInDaemon(t, dir)
	spawns := recordSpawns(t)
	ctl := &fakeServiceController{managed: true}
	useServiceController(t, ctl)

	out, err := runDaemonCommand(daemonStartCmd)
	require.NoError(t, err)
	require.Contains(t, out, "daemon: already running")
	require.Zero(t, ctl.starts)
	require.Zero(t, *spawns)
}

// Without an installed service, and on platforms with no controller at all,
// start keeps self-exec'ing a detached `daemon serve`.
func TestDaemonStartUnmanagedStillSelfExecs(t *testing.T) {
	for name, ctl := range map[string]daemon.ServiceController{
		"controller without an installed service": &fakeServiceController{managed: false},
		"no controller for the platform":          nil,
	} {
		t.Run(name, func(t *testing.T) {
			useServiceTestStateDir(t)
			spawns := recordSpawns(t)
			useServiceController(t, ctl)

			_, err := runDaemonCommand(daemonStartCmd)
			require.NoError(t, err)
			require.Equal(t, 1, *spawns)
			if fake, ok := ctl.(*fakeServiceController); ok {
				require.Zero(t, fake.starts)
			}
		})
	}
}

// A managed stop must ask the service manager, and must ask it while the
// daemon is still up: a stop over the control socket first would be undone
// by launchd's KeepAlive, and is not what systemd should be told either.
func TestDaemonStopManagedGoesThroughTheServiceManager(t *testing.T) {
	dir := useServiceTestStateDir(t)
	srv := startStandInDaemon(t, dir)
	ctl := &fakeServiceController{managed: true}
	ctl.onStop = func() error {
		require.False(t, standInStopped(srv),
			"the daemon was stopped over the control socket before the manager was asked")
		return srv.Stop()
	}
	useServiceController(t, ctl)

	out, err := runDaemonCommand(daemonStopCmd)
	require.NoError(t, err)
	require.Equal(t, 1, ctl.stops)
	require.Contains(t, out, "daemon: stopping via fake manager")
	require.Contains(t, out, "daemon: stopped")
	require.NotContains(t, out, "running outside")
}

func TestDaemonStopManagedReportsAServiceManagerFailure(t *testing.T) {
	dir := useServiceTestStateDir(t)
	srv := startStandInDaemon(t, dir)
	ctl := &fakeServiceController{managed: true}
	ctl.onStop = func() error { return errors.New("launchctl bootout: exit status 1") }
	useServiceController(t, ctl)

	_, err := runDaemonCommand(daemonStopCmd)
	require.ErrorContains(t, err, "launchctl bootout")
	require.False(t, standInStopped(srv),
		"a failed managed stop must not fall back to a control-socket stop the manager would undo")
}

// A daemon that still answers after the manager stopped its service runs
// outside the manager, as the old self-exec'd start and restart left them.
// Stop must shut it down over the control socket rather than leave it.
func TestDaemonStopManagedStopsADaemonRunningOutsideTheManager(t *testing.T) {
	dir := useServiceTestStateDir(t)
	srv := startStandInDaemon(t, dir)
	ctl := &fakeServiceController{managed: true}

	var out bytes.Buffer
	err := stopManagedDaemon(newOutputCmd(&out), ctl, 300*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, 1, ctl.stops)
	// The control server stops answering a moment before it marks itself
	// done, so allow for that gap.
	require.Eventually(t, func() bool { return standInStopped(srv) }, 5*time.Second, 10*time.Millisecond,
		"the daemon outside the manager must be stopped over the control socket")
	require.Contains(t, out.String(), "running outside fake manager")
	require.Contains(t, out.String(), "daemon: stopped")
}

// Stop must not report success while anything still answers on the socket.
func TestDaemonStopManagedFailsWhileADaemonStillAnswers(t *testing.T) {
	dir := useServiceTestStateDir(t)
	// Answers every dial and ignores every request, including "stop".
	ln, err := net.Listen("unix", filepath.Join(dir, "aplexicad.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	ctl := &fakeServiceController{managed: true}

	var out bytes.Buffer
	err = stopManagedDaemon(newOutputCmd(&out), ctl, 100*time.Millisecond)
	require.Error(t, err)
	require.Contains(t, err.Error(), "still answering")
	require.Contains(t, err.Error(), "aplexica daemon status")
}

// Without an installed service, and on platforms with no controller at all,
// stop keeps sending "stop" over the control socket, and keeps failing when
// no daemon answers.
func TestDaemonStopUnmanagedStillUsesTheControlSocket(t *testing.T) {
	for name, ctl := range map[string]daemon.ServiceController{
		"controller without an installed service": &fakeServiceController{managed: false},
		"no controller for the platform":          nil,
	} {
		t.Run(name, func(t *testing.T) {
			dir := useServiceTestStateDir(t)
			useServiceController(t, ctl)

			_, err := runDaemonCommand(daemonStopCmd)
			require.ErrorContains(t, err, "not running or unreachable")

			srv := startStandInDaemon(t, dir)
			out, err := runDaemonCommand(daemonStopCmd)
			require.NoError(t, err)
			require.Contains(t, out, "daemon: stop signal sent")
			require.Eventually(t, func() bool { return standInStopped(srv) }, 5*time.Second, 10*time.Millisecond,
				"an unmanaged stop must reach the daemon over the control socket")
			if fake, ok := ctl.(*fakeServiceController); ok {
				require.Zero(t, fake.stops)
			}
		})
	}
}

func TestDaemonRestartManagedGoesThroughTheServiceManager(t *testing.T) {
	dir := useServiceTestStateDir(t)
	srv := startStandInDaemon(t, dir)
	ctl := &fakeServiceController{managed: true}
	useServiceController(t, ctl)

	out, err := runDaemonCommand(daemonRestartCmd)
	require.NoError(t, err)
	require.Equal(t, 1, ctl.restarts)
	require.False(t, standInStopped(srv), "a managed restart must not stop the daemon over the control socket")
	require.Contains(t, out, "daemon: restarting via fake manager")
}
