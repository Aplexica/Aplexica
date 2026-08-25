package daemon

import (
	"errors"
	"fmt"
)

// ErrNotSupported is returned by Installer methods on platforms where
// v0.9.0 does not yet ship a native service-install backend (Windows,
// non-V1 BSDs, etc.). Callers should treat this as "user must register
// the daemon manually" — the error message points at the workaround.
var ErrNotSupported = errors.New("daemon: service install not supported on this platform")

// InstallOptions captures everything the platform Installer needs to
// generate a service definition. The CLI populates this from the user's
// flags; the Installer turns it into a launchd plist / systemd unit /
// Windows service registration.
type InstallOptions struct {
	// AplexicaPath is the absolute path to the aplexica binary the
	// installed service will exec. Typically the result of os.Executable().
	AplexicaPath string

	// Dir is the directory the daemon will watch. Required.
	Dir string

	// StoreRoot, SecretsRoot, StateDir, LogDir map to the corresponding
	// daemon flags. Empty values are not propagated to the service
	// definition — the daemon picks its own defaults.
	StoreRoot   string
	SecretsRoot string
	StateDir    string
	LogDir      string

	// Quiet + GuardWindow are passed as strings (Go duration format).
	// Empty values are not propagated.
	Quiet       string
	GuardWindow string

	// Recursive: when true, the installed service includes the
	// --recursive flag.
	Recursive bool

	// HermesWatch, when non-nil, is propagated as --hermes-watch=<bool>.
	// nil means "don't propagate; let the daemon use its default".
	HermesWatch *bool

	// HermesWatchInterval is propagated as --hermes-watch-interval <duration>
	// when non-empty. Empty means "don't propagate".
	HermesWatchInterval string

	// HermesDB is propagated as --hermes-db <path> when non-empty.
	HermesDB string
}

// Validate returns an error if required fields are missing.
func (o InstallOptions) Validate() error {
	if o.AplexicaPath == "" {
		return fmt.Errorf("InstallOptions: AplexicaPath is required")
	}
	if o.Dir == "" {
		return fmt.Errorf("InstallOptions: Dir is required")
	}
	return nil
}

// Installer is the platform-specific service-install surface. Implementations
// MUST be idempotent: Install on an already-installed service updates the
// definition; Uninstall on a not-installed service is a no-op (no error).
type Installer interface {
	// Install registers the service so it auto-starts at user login.
	// Returns ErrNotSupported on platforms without a native backend.
	Install() error

	// Uninstall removes the service registration. Returns nil if the
	// service wasn't installed. Returns ErrNotSupported on platforms
	// without a native backend.
	Uninstall() error

	// PlatformLabel returns a human-readable platform name for status
	// messages (e.g., "launchd LaunchAgent", "systemd --user", "not supported").
	PlatformLabel() string
}

// newInstaller validates opts and returns the platform-appropriate Installer.
// Implementations live in install_<goos>.go (darwin/linux/windows) plus
// install_default.go for non-V1 platforms.
func newInstaller(opts InstallOptions) (Installer, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return newPlatformInstaller(opts), nil
}

// New is the public constructor; CLI callers use this.
func New(opts InstallOptions) (Installer, error) {
	return newInstaller(opts)
}

// unsupportedInstaller is a shared fallback used by install_default.go and
// platform stubs before a native backend is wired in. It returns
// ErrNotSupported for every method.
type unsupportedInstaller struct {
	platform string
	opts     InstallOptions
}

func (u *unsupportedInstaller) Install() error {
	return fmt.Errorf("%w: %s; aplexica daemon serve can be run manually under your platform's init system", ErrNotSupported, u.platform)
}

func (u *unsupportedInstaller) Uninstall() error {
	return fmt.Errorf("%w: %s", ErrNotSupported, u.platform)
}

func (u *unsupportedInstaller) PlatformLabel() string {
	return "not supported (" + u.platform + ")"
}
