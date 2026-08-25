//go:build !darwin && !linux && !windows

package trayinstall

import "runtime"

func newPlatformInstaller(_ Options) Installer {
	return &unsupportedInstaller{platform: runtime.GOOS}
}
