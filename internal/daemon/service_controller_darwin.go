//go:build darwin

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func newPlatformServiceController() ServiceController { return launchdController{} }

// execLaunchctl is the single choke point through which the controller
// shells out to launchctl. It is a package variable so tests can record the
// exact command lines instead of running them. launchctl addresses the job
// by its label, gui/<uid>/com.aplexica.aplexicad, not by any path a test
// controls, so a test that reached the real launchctl would boot out or
// restart the daemon of whoever runs the suite. TestMain in
// service_controller_darwin_test.go replaces it for the whole test binary.
var execLaunchctl = func(args ...string) ([]byte, error) {
	return exec.Command("launchctl", args...).CombinedOutput()
}

const (
	// launchdStopWait bounds how long Stop waits for launchd to remove the
	// job after bootout. launchd sends SIGTERM and escalates to SIGKILL
	// after the job's ExitTimeOut, 20s by default (the plist sets none), so
	// the job is normally gone well within this.
	launchdStopWait = 30 * time.Second
	// launchdPollInterval spaces the `launchctl print` probes Stop makes
	// while it waits.
	launchdPollInterval = 100 * time.Millisecond
)

type launchdController struct{}

func (launchdController) Label() string { return "launchd LaunchAgent" }

// plistPath mirrors launchdInstaller.plistPath for the default location.
func (launchdController) plistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func (c launchdController) Managed() bool {
	path := c.plistPath()
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// domain is the per-user GUI domain the LaunchAgent loads into; target is
// the job inside it.
func (launchdController) domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func (c launchdController) target() string { return c.domain() + "/" + launchdLabel }

// loaded reports whether the job is registered in the GUI domain.
// `launchctl print` exits 0 for a registered job, including one that is not
// running or is still being booted out, and non-zero once it is gone.
func (c launchdController) loaded() bool {
	_, err := execLaunchctl("print", c.target())
	return err == nil
}

// Start loads the agent, then kickstarts it.
//
// Loading is what brings the daemon back after Stop, which boots the job
// out of the domain. `launchctl bootstrap` refuses a job that is already
// loaded, with a generic "5: Input/output error" that an unreadable plist
// also produces, so a failed bootstrap is judged by whether the job is
// loaded afterwards rather than by its text. The plist sets RunAtLoad, so a
// fresh bootstrap starts the daemon by itself. The kickstart, without -k,
// starts a job that was loaded but not running, for example one waiting
// out launchd's relaunch throttle, and leaves a running one alone.
func (c launchdController) Start() error {
	domain, plist := c.domain(), c.plistPath()
	if out, err := execLaunchctl("bootstrap", domain, plist); err != nil && !c.loaded() {
		return fmt.Errorf("daemon: launchctl bootstrap %s %s: %w (output: %s)",
			domain, plist, err, strings.TrimSpace(string(out)))
	}
	target := c.target()
	if out, err := execLaunchctl("kickstart", target); err != nil {
		return fmt.Errorf("daemon: launchctl kickstart %s: %w (output: %s)",
			target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Stop boots the job out of the GUI domain and waits for launchd to finish
// removing it.
//
// The plist sets KeepAlive to true, so launchd relaunches the daemon
// whenever it exits, whatever the exit status: a stop over the control
// socket, `launchctl kill` or the legacy `launchctl stop` is undone within
// seconds. Only taking the job out of the domain keeps it stopped. bootout,
// unlike `unload -w`, records no disabled override, so the agent still
// loads at the next login, as an enabled systemd unit does.
//
// bootout returns while the daemon is still exiting. Until the process is
// gone the job stays registered (`launchctl print` shows it as SIGTERMed),
// a bootstrap in that window fails, and a kickstart succeeds without
// starting anything. Stop waits the teardown out so that `daemon stop`
// followed at once by `daemon start` really starts the daemon.
func (c launchdController) Stop() error { return c.stop(launchdStopWait) }

func (c launchdController) stop(wait time.Duration) error {
	target := c.target()
	if out, err := execLaunchctl("bootout", target); err != nil {
		if !c.loaded() {
			// Nothing was loaded, so there is nothing to stop.
			return nil
		}
		return fmt.Errorf("daemon: launchctl bootout %s: %w (output: %s)",
			target, err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(wait)
	for c.loaded() {
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon: launchd still has %s loaded %s after bootout", target, wait)
		}
		time.Sleep(launchdPollInterval)
	}
	return nil
}

// Restart uses `launchctl kickstart -k`, which stops the job if it is
// running and starts it again under launchd, in one step. Unload followed
// by load would also work but briefly deregisters the job, and a failure
// between the two would leave the agent unregistered rather than merely
// not running.
//
// kickstart needs a loaded job, and Stop leaves none. With nothing loaded
// there is no running instance to replace, so Restart is a Start.
func (c launchdController) Restart() error {
	if !c.loaded() {
		return c.Start()
	}
	target := c.target()
	out, err := execLaunchctl("kickstart", "-k", target)
	if err != nil {
		return fmt.Errorf("daemon: launchctl kickstart -k %s: %w (output: %s)",
			target, err, strings.TrimSpace(string(out)))
	}
	return nil
}
