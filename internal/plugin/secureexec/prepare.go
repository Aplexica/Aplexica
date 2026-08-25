// Package secureexec turns an already-authorized remote-plugin digest into a
// process launch that cannot be redirected to different pathname bytes between
// verification and kernel image resolution.
package secureexec

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/aplexica/aplexica/internal/privatefs"
)

const maxExecutableBytes = 256 << 20

var installedComponent = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// Command retains every OS resource which makes the eventual launch atomic.
// Callers must keep it open through Cmd.Start/Run. After Start returns, the
// kernel has resolved the image and Close may be called immediately.
type Command struct {
	cmd       *exec.Cmd
	resources []*os.File
}

// Prepare reopens and authenticates path against expected, then constructs the
// strongest platform launch available. It never searches PATH.
func Prepare(ctx context.Context, path string, expected [32]byte, args ...string) (*Command, error) {
	if ctx == nil || !filepath.IsAbs(path) || filepath.Clean(path) != path || expected == ([32]byte{}) {
		return nil, errors.New("secureexec: invalid authenticated launch request")
	}
	if err := validatePlatformLaunchPath(path); err != nil {
		return nil, err
	}
	input, err := privatefs.OpenTrustedInput(path, privatefs.TrustedInputPolicy{
		MaxBytes: maxExecutableBytes, RequireExecutable: true, AllowSystemOwner: true,
	})
	if err != nil {
		return nil, fmt.Errorf("secureexec: open authenticated executable: %w", err)
	}
	if sha256.Sum256(input.Bytes) != expected {
		return nil, errors.New("secureexec: executable differs from the authorized digest")
	}
	cmd, resources, err := preparePlatformCommand(ctx, path, expected, input, args)
	if err != nil {
		return nil, errors.Join(err, closeResources(resources))
	}
	if cmd == nil || cmd.Path == "" || len(cmd.Args) == 0 {
		return nil, errors.Join(errors.New("secureexec: platform returned an invalid command"), closeResources(resources))
	}
	return &Command{cmd: cmd, resources: resources}, nil
}

// ValidateInstalledRemotePlugin enforces the platform's supported installed
// layout before a verified plugin path is committed to daemon configuration.
// Linux executes a sealed authenticated copy and Windows holds deny-write and
// deny-delete handles, so only macOS requires a privileged immutable tree.
func ValidateInstalledRemotePlugin(path, pluginID, pluginVersion string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !installedComponent.MatchString(pluginID) || !installedComponent.MatchString(pluginVersion) {
		return errors.New("secureexec: invalid installed remote-plugin identity")
	}
	return validateInstalledRemotePluginPlatform(path, pluginID, pluginVersion)
}

// Cmd returns the command for caller-owned stdin/stdout/env configuration.
// Do not mutate Path, Args, or ExtraFiles.
func (command *Command) Cmd() *exec.Cmd {
	if command == nil {
		return nil
	}
	return command.cmd
}

func (command *Command) Close() error {
	if command == nil {
		return nil
	}
	err := closeResources(command.resources)
	command.resources = nil
	return err
}

func closeResources(resources []*os.File) error {
	var result error
	for index := len(resources) - 1; index >= 0; index-- {
		if resources[index] != nil {
			result = errors.Join(result, resources[index].Close())
		}
	}
	return result
}
