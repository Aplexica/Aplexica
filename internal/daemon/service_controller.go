package daemon

// ServiceController is the control surface of a service manager that owns
// an installed daemon: systemd on Linux, launchd on macOS.
//
// It exists because restarting a managed daemon by hand is not a restart.
// `daemon restart` used to stop the running process over the control socket
// and then self-exec a detached replacement. Under a service manager that
// produces two wrong outcomes at once: the manager's own unit is left
// stopped — systemd treats a clean exit as success, so `Restart=on-failure`
// does not bring it back — and the replacement runs outside the manager,
// unsupervised, inside the caller's session scope, so it dies with the
// shell that launched it. The command reported a pid while leaving no
// daemon running at all.
//
// A controller reports only whether a registered unit exists. It does not
// report whether the daemon is currently up; the caller already learns that
// from the control socket, which is the authority on readiness.
type ServiceController interface {
	// Managed reports whether a service definition this controller owns is
	// currently registered. False means the daemon, if any, was started by
	// hand and the caller should fall back to process-level control.
	Managed() bool

	// Restart restarts the registered service through its manager. It
	// returns once the manager has accepted the request, which is not the
	// same as the daemon being ready — callers wait on the control socket
	// for that.
	Restart() error

	// Label names the manager for status output, matching the vocabulary
	// Installer.PlatformLabel already uses ("systemd --user").
	Label() string
}

// ServiceControllerForPlatform returns the controller for this platform, or
// nil where no service manager backend ships. It takes no options on
// purpose: locating an installed unit needs only well-known paths, so the
// caller does not have to reconstruct the InstallOptions the unit was
// created from just to restart it.
func ServiceControllerForPlatform() ServiceController {
	return newPlatformServiceController()
}
