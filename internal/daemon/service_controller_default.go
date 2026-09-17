//go:build !darwin && !linux

package daemon

// newPlatformServiceController returns nil where no service-manager backend
// ships. Windows registers a logon-triggered Scheduled Task rather than a
// supervised service, and its detached child is not tied to the caller's
// session the way a systemd session scope is, so it does not exhibit the
// failure this interface exists to fix. Callers fall back to process-level
// stop and start there.
func newPlatformServiceController() ServiceController { return nil }
