//go:build windows

package daemon

import "golang.org/x/sys/windows"

// stillActiveExitCode is the value GetExitCodeProcess returns for a
// process that hasn't exited yet (STILL_ACTIVE in the Win32 SDK).
const stillActiveExitCode = 259

// pidAlive reports whether pid is currently running. The Unix
// signal-0 probe doesn't work on Windows because os.Process.Signal
// only accepts os.Kill there — every other signal returns "not
// supported", which would make every PID appear dead and cause the
// stale-lock cleanup to delete live lock files (the original bug
// behind TestLock_DoubleAcquire_SecondFails passing on Unix but
// silently corrupting locks on Windows).
//
// Instead, open a query-only handle and read the exit code. If the
// process is still running, GetExitCodeProcess writes 259 (STILL_ACTIVE).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActiveExitCode
}
