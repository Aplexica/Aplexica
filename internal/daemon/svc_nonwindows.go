//go:build !windows

package daemon

// IsWindowsService is a non-windows stub for the svc.IsWindowsService
// re-export in svc_windows.go. Returns (false, nil) so callers in
// build-tag-free files can call it unconditionally on every platform
// without needing their own platform shim.
func IsWindowsService() (bool, error) {
	return false, nil
}

// ServiceName is the canonical service name used by the Windows
// installer (install_windows.go) + RunAsService. Re-declared here so
// non-windows builds can still reference daemon.ServiceName from
// build-tag-free files; on non-windows the constant is never used to
// actually talk to SCM (there isn't one), but keeping it visible
// avoids "undefined: ServiceName" errors at compile time.
const ServiceName = "Aplexica"
