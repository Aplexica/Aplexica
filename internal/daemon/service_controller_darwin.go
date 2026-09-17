//go:build darwin

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func newPlatformServiceController() ServiceController { return launchdController{} }

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

// Restart uses `launchctl kickstart -k`, which stops the job if it is
// running and starts it again under launchd, in one step. Unload followed
// by load would also work but briefly deregisters the job, and a failure
// between the two would leave the agent unregistered rather than merely
// not running.
func (c launchdController) Restart() error {
	target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + launchdLabel
	out, err := exec.Command("launchctl", "kickstart", "-k", target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("daemon: launchctl kickstart -k %s: %w (output: %s)",
			target, err, strings.TrimSpace(string(out)))
	}
	return nil
}
