//go:build windows

package kilo

import (
	"os/exec"
	"syscall"
)

const _DETACHED_PROCESS = 0x00000008
const _CREATE_NO_WINDOW = 0x08000000

// hideImportWindow suppresses the console window for Kilo's CLI import helper.
// Synced conversations can trigger several short-lived imports; without this,
// Windows flashes a terminal for each `kilo import` subprocess.
func hideImportWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: _DETACHED_PROCESS | _CREATE_NO_WINDOW,
		HideWindow:    true,
	}
}
