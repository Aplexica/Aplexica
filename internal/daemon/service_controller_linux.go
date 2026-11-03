//go:build linux

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func newPlatformServiceController() ServiceController { return systemdController{} }

// execSystemctl is the single choke point through which the controller
// shells out to systemctl. It is a package variable so tests can record the
// exact command lines instead of running them: TestMain in
// service_controller_linux_test.go replaces it for the whole test binary, so
// no test can start, stop or restart the real aplexicad.service of whoever
// runs the suite.
var execSystemctl = func(args ...string) ([]byte, error) {
	return exec.Command("systemctl", args...).CombinedOutput()
}

type systemdController struct{}

func (systemdController) Label() string { return "systemd --user" }

// unitPath mirrors systemdInstaller.unitPath for the default (non-test)
// location. The controller deliberately does not take an override: it
// answers for the unit a real install wrote, which is the only one a
// service manager knows about.
func (systemdController) unitPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdServiceName)
}

func (c systemdController) Managed() bool {
	path := c.unitPath()
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Start runs `systemctl --user start`, which leaves the unit active under
// systemd. The unit is Type=simple, so systemctl returns once the process is
// forked and the caller waits on the control socket for readiness.
func (c systemdController) Start() error { return c.systemctl("start") }

// Stop runs `systemctl --user stop`. The unit sets Restart=on-failure, but
// systemd never applies Restart= to a stop it was asked for, so the unit
// stays inactive until it is started again. It stays enabled, so it still
// starts when the user's systemd instance next starts, normally at login.
// systemctl waits for the stop job, which ends every process in the unit's
// control group, so the daemon has exited by the time Stop returns.
func (c systemdController) Stop() error { return c.systemctl("stop") }

func (c systemdController) Restart() error { return c.systemctl("restart") }

// systemctl runs `systemctl --user <verb> aplexicad.service`.
func (systemdController) systemctl(verb string) error {
	out, err := execSystemctl("--user", verb, systemdServiceName)
	if err != nil {
		return fmt.Errorf("daemon: systemctl --user %s %s: %w (output: %s)",
			verb, systemdServiceName, err, strings.TrimSpace(string(out)))
	}
	return nil
}
