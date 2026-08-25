//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
)

// _DETACHED_PROCESS fully detaches a child from any console: no console window
// AND no inherited console control events. _CREATE_NO_WINDOW is redundant on
// most hosts once DETACHED_PROCESS is present, but some Windows shells still
// briefly allocate a console for redirected console-subsystem children unless
// both flags and HideWindow are set.
const _DETACHED_PROCESS = 0x00000008
const _CREATE_NO_WINDOW = 0x08000000

// hideChildWindow makes a spawned console-subsystem child (e.g. the remote
// plugin) run without a visible console window on Windows. Stdin/stdout pipes
// set on cmd are unaffected — only the console is detached.
func hideChildWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: _DETACHED_PROCESS | _CREATE_NO_WINDOW,
		HideWindow:    true,
	}
}
