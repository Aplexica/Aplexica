package main

import (
	"os"
	"path/filepath"
)

// persistentExecutable returns the absolute path this process should bake
// into a launchd/systemd/Scheduled-Task unit so the service still resolves
// after this command exits.
//
// It used to indirect through the direct-install stable launcher: the
// installer wrote an immutable per-version runtime tree and dropped a
// launcher shim in ~/.aplexica/bin, and this function re-authenticated the
// environment variable naming that shim before recording it in place of
// the running binary. That whole delivery model is retired — aplexica now
// ships as ordinary archives, .deb packages, a Homebrew formula and a
// WinGet package, all of which install a real binary at a path that stays
// valid across upgrades. The package manager owns the stable path, so the
// running executable IS the persistent one.
func persistentExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}
