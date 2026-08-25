//go:build !windows

package daemon

import "os/exec"

// hideChildWindow is a no-op on non-windows (no console-window concept).
func hideChildWindow(cmd *exec.Cmd) {}
