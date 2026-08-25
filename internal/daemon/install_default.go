//go:build !darwin && !linux && !windows

package daemon

import "runtime"

func newPlatformInstaller(opts InstallOptions) Installer {
	return &unsupportedInstaller{platform: runtime.GOOS, opts: opts}
}
