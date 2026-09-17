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

func (c systemdController) Restart() error {
	out, err := exec.Command("systemctl", "--user", "restart", systemdServiceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("daemon: systemctl --user restart %s: %w (output: %s)",
			systemdServiceName, err, strings.TrimSpace(string(out)))
	}
	return nil
}
