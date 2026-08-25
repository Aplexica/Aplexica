//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal(0) is the standard liveness probe — succeeds for any
	// process the current user can signal, errors for missing or
	// unreachable PIDs. Same idiom as `kill -0`.
	return proc.Signal(syscall.Signal(0)) == nil
}
