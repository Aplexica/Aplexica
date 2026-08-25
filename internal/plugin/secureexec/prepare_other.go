//go:build !linux && !darwin && !windows

package secureexec

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"github.com/aplexica/aplexica/internal/privatefs"
)

func validatePlatformLaunchPath(string) error { return nil }

func validateInstalledRemotePluginPlatform(string, string, string) error { return nil }

func preparePlatformCommand(context.Context, string, [32]byte, privatefs.TrustedInput, []string) (*exec.Cmd, []*os.File, error) {
	return nil, nil, errors.New("secureexec: authenticated process launch is unsupported on this platform")
}
