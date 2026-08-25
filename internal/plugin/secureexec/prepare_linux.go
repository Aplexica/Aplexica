//go:build linux

package secureexec

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/aplexica/aplexica/internal/privatefs"
	"golang.org/x/sys/unix"
)

func validatePlatformLaunchPath(string) error { return nil }

func validateInstalledRemotePluginPlatform(string, string, string) error { return nil }

func preparePlatformCommand(ctx context.Context, originalPath string, expected [32]byte, input privatefs.TrustedInput, args []string) (*exec.Cmd, []*os.File, error) {
	flags := unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING | unix.MFD_EXEC
	fd, err := unix.MemfdCreate("aplexica-remote-plugin", flags)
	if errors.Is(err, unix.EINVAL) {
		// Older kernels predate MFD_EXEC. The resulting memfd remains executable
		// under their default policy and is sealed identically below.
		fd, err = unix.MemfdCreate("aplexica-remote-plugin", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("secureexec: create executable memfd: %w", err)
	}
	file := os.NewFile(uintptr(fd), "aplexica-remote-plugin")
	resources := []*os.File{file}
	if err := file.Chmod(0o500); err != nil {
		return nil, resources, fmt.Errorf("secureexec: chmod executable memfd: %w", err)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), &boundedReader{value: input.Bytes, maximum: maxExecutableBytes})
	if err != nil || written != int64(len(input.Bytes)) || written == 0 || written > maxExecutableBytes {
		return nil, resources, errors.New("secureexec: copy authenticated bytes into memfd")
	}
	var actual [32]byte
	copy(actual[:], hash.Sum(nil))
	if actual != expected {
		return nil, resources, errors.New("secureexec: memfd copy digest mismatch")
	}
	seals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		return nil, resources, fmt.Errorf("secureexec: seal executable memfd: %w", err)
	}
	actualSeals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || actualSeals&seals != seals {
		return nil, resources, errors.New("secureexec: executable memfd seals are incomplete")
	}
	if _, err := file.Seek(0, 0); err != nil {
		return nil, resources, fmt.Errorf("secureexec: rewind executable memfd: %w", err)
	}
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		return nil, resources, fmt.Errorf("secureexec: /proc/self/fd is required for sealed-handle execution: %w", err)
	}
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", args...)
	cmd.Args[0] = originalPath
	cmd.ExtraFiles = []*os.File{file}
	return cmd, resources, nil
}

type boundedReader struct {
	value   []byte
	offset  int
	maximum int64
}

func (reader *boundedReader) Read(output []byte) (int, error) {
	if int64(reader.offset) >= reader.maximum || reader.offset >= len(reader.value) {
		return 0, io.EOF
	}
	limit := len(output)
	if remaining := len(reader.value) - reader.offset; limit > remaining {
		limit = remaining
	}
	if remaining := int(reader.maximum) - reader.offset; limit > remaining {
		limit = remaining
	}
	copy(output[:limit], reader.value[reader.offset:reader.offset+limit])
	reader.offset += limit
	return limit, nil
}
