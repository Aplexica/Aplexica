//go:build tray && windows

package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func platformTerminalCommand(argv []string) (*exec.Cmd, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE}
	return cmd, nil
}
