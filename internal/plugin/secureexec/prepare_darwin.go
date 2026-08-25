//go:build darwin

package secureexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aplexica/aplexica/internal/privatefs"
	"golang.org/x/sys/unix"
)

const darwinExecutablePermissionBits os.FileMode = 0o111

const darwinRemotePluginRoot = "/Library/Aplexica/RemotePlugins"

func validatePlatformLaunchPath(path string) error {
	version := filepath.Base(filepath.Dir(path))
	return validateInstalledRemotePluginPlatform(path, "aplexica-cloud", version)
}

func validateInstalledRemotePluginPlatform(path, pluginID, pluginVersion string) error {
	if err := validateDarwinRemotePluginPathShape(path, pluginID, pluginVersion); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	expected := map[string]os.FileMode{
		"aplexica-cloud-plugin":                    0o555,
		"aplexica-cloud-plugin" + ".manifest.cbor": 0o444,
		"release.inventory.cbor":                   0o444,
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return errors.New("secureexec: read macOS remote-plugin version directory")
	}
	if len(entries) == 1 && entries[0].Name() == "aplexica-cloud-plugin" {
		// Balanced desktop releases authorize exact plugin bytes in the signed
		// daemon itself. They need no separately signed publisher metadata; the
		// one-file directory remains root-owned, ACL-free, and immutable.
		expected = map[string]os.FileMode{"aplexica-cloud-plugin": 0o555}
	} else if len(entries) != len(expected) {
		return errors.New("secureexec: macOS remote-plugin version directory has an unsupported exact layout")
	}
	directoryInfo, err := os.Lstat(directory)
	directoryStat, directoryStatOK := infoSyscallStat(directoryInfo)
	if err != nil || !directoryStatOK || !directoryInfo.IsDir() || directoryInfo.Mode().Perm() != 0o555 || directoryStat.Uid != 0 || directoryStat.Gid != 0 {
		return errors.New("secureexec: macOS remote-plugin version directory must have mode 0555")
	}
	for _, parent := range []string{"/Library", "/Library/Aplexica", darwinRemotePluginRoot, filepath.Join(darwinRemotePluginRoot, pluginID)} {
		info, parentErr := os.Lstat(parent)
		stat, statOK := infoSyscallStat(info)
		if parentErr != nil || !statOK || !info.IsDir() || info.Mode().Perm() != 0o755 || stat.Uid != 0 || stat.Gid != 0 {
			return fmt.Errorf("secureexec: macOS remote-plugin parent must be root:wheel mode 0755: %s", parent)
		}
	}
	for _, entry := range entries {
		mode, ok := expected[entry.Name()]
		if !ok {
			return fmt.Errorf("secureexec: unexpected file in macOS remote-plugin version directory: %s", entry.Name())
		}
		candidate := filepath.Join(directory, entry.Name())
		info, statErr := os.Lstat(candidate)
		stat, statOK := infoSyscallStat(info)
		if statErr != nil || !statOK || !info.Mode().IsRegular() || info.Mode().Perm() != mode || stat.Uid != 0 || stat.Gid != 0 || stat.Nlink != 1 {
			return fmt.Errorf("secureexec: macOS remote-plugin file is not root-owned, single-link, and mode %04o: %s", mode, candidate)
		}
		if err := validatePrivilegedImmutableDarwinFile(candidate, mode&darwinExecutablePermissionBits != 0); err != nil {
			return err
		}
	}
	aclCandidates := []string{
		"/Library",
		"/Library/Aplexica",
		filepath.Join(darwinRemotePluginRoot),
		filepath.Join(darwinRemotePluginRoot, pluginID),
		directory,
	}
	for entry := range expected {
		aclCandidates = append(aclCandidates, filepath.Join(directory, entry))
	}
	for _, candidate := range aclCandidates {
		if err := requireNoDarwinExtendedACL(candidate); err != nil {
			return err
		}
	}
	return nil
}

func validateDarwinRemotePluginPathShape(path, pluginID, pluginVersion string) error {
	if !installedComponent.MatchString(pluginID) || !installedComponent.MatchString(pluginVersion) {
		return errors.New("secureexec: invalid macOS remote-plugin versioned identity")
	}
	expected := filepath.Join(darwinRemotePluginRoot, pluginID, pluginVersion, "aplexica-cloud-plugin")
	if path != expected {
		return fmt.Errorf("secureexec: macOS remote plugins must use the exact root-sealed versioned path %s", expected)
	}
	return nil
}

func infoSyscallStat(info os.FileInfo) (*syscall.Stat_t, bool) {
	if info == nil {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return stat, ok
}

func requireNoDarwinExtendedACL(path string) error {
	cmd := exec.Command("/bin/ls", "-lde", "--", path)
	cmd.Env = []string{"LC_ALL=C", "LANG=C", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("secureexec: inspect macOS ACL for %s: %w", path, err)
	}
	if bytes.Contains(bytes.TrimSuffix(output, []byte("\n")), []byte("\n")) {
		return fmt.Errorf("secureexec: extended ACL is forbidden on macOS remote-plugin path: %s", path)
	}
	return nil
}

func preparePlatformCommand(ctx context.Context, path string, expected [32]byte, input privatefs.TrustedInput, args []string) (*exec.Cmd, []*os.File, error) {
	if os.Geteuid() == 0 {
		return nil, nil, errors.New("secureexec: remote plugin must run as an unprivileged account")
	}
	if err := validatePrivilegedImmutableDarwinPath(path); err != nil {
		return nil, nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("secureexec: retain executable: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	resources := []*os.File{file}
	info, err := file.Stat()
	if err != nil || !os.SameFile(info, input.Info) {
		return nil, resources, errors.New("secureexec: executable identity changed before retained open")
	}
	hash := sha256.New()
	read, err := io.Copy(hash, io.LimitReader(file, maxExecutableBytes+1))
	if err != nil || read != int64(len(input.Bytes)) || read == 0 || read > maxExecutableBytes {
		return nil, resources, errors.New("secureexec: retained executable has invalid size")
	}
	var actual [32]byte
	copy(actual[:], hash.Sum(nil))
	if actual != expected {
		return nil, resources, errors.New("secureexec: retained executable digest mismatch")
	}
	if err := validatePrivilegedImmutableDarwinPath(path); err != nil {
		return nil, resources, fmt.Errorf("secureexec: privileged path changed after retained open: %w", err)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(current, info) {
		return nil, resources, errors.New("secureexec: privileged executable pathname identity changed")
	}
	cmd := exec.CommandContext(ctx, path, args...)
	return cmd, resources, nil
}

func validatePrivilegedImmutableDarwinPath(path string) error {
	return validatePrivilegedImmutableDarwinFile(path, true)
}

func validatePrivilegedImmutableDarwinFile(path string, requireExecutable bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("secureexec: privileged executable path must be clean and absolute")
	}
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	current := string(filepath.Separator)
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			return errors.New("secureexec: privileged executable path has an unsafe component")
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("secureexec: privileged path component is missing or linked: %s", current)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("secureexec: privileged path component is not root-owned and immutable: %s", current)
		}
		final := index == len(parts)-1
		if (!final && !info.IsDir()) || (final && (!info.Mode().IsRegular() || stat.Nlink != 1 || (requireExecutable && info.Mode().Perm()&darwinExecutablePermissionBits == 0))) {
			return fmt.Errorf("secureexec: privileged path component has the wrong type: %s", current)
		}
		// access(2) evaluates the effective ACL for this daemon account. Mode bits
		// alone would miss a named ACL granting a peer process write/delete power.
		if err := unix.Access(current, unix.W_OK); err == nil || (!errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EROFS)) {
			return fmt.Errorf("secureexec: daemon account can mutate privileged path component: %s", current)
		}
	}
	return nil
}
