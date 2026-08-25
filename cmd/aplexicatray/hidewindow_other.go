//go:build tray && !windows

package main

import "os/exec"

// hideChildConsole is a no-op on non-windows (no console-window concept).
func hideChildConsole(cmd *exec.Cmd) {}
