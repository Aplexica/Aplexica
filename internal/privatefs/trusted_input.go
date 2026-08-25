package privatefs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type TrustedInputPolicy struct {
	MaxBytes          int64
	RequirePrivate    bool
	RequireExecutable bool
	AllowSystemOwner  bool
}

type TrustedInput struct {
	Bytes []byte
	Info  os.FileInfo
}

// OpenTrustedInput walks an explicitly selected absolute path without
// following links and rechecks every ancestor after the bounded read. It never
// searches PATH, repairs permissions, or reopens a final by pathname.
func OpenTrustedInput(path string, policy TrustedInputPolicy) (TrustedInput, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || policy.MaxBytes <= 0 {
		return TrustedInput{}, fmt.Errorf("privatefs: invalid trusted input request")
	}
	volume := filepath.VolumeName(path)
	rootPath := volume + string(filepath.Separator)
	rel := strings.TrimPrefix(path, rootPath)
	if rel == "" {
		return TrustedInput{}, fmt.Errorf("privatefs: trusted input must be a file")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return TrustedInput{}, err
	}
	defer root.Close()
	identities, err := validateTrustedChain(root, rel, policy)
	if err != nil {
		return TrustedInput{}, err
	}
	f, err := root.OpenFile(rel, os.O_RDONLY, 0)
	if err != nil {
		return TrustedInput{}, err
	}
	info, err := f.Stat()
	if err == nil {
		err = validateTrustedFinal(f, info, policy)
	}
	if err != nil {
		_ = f.Close()
		return TrustedInput{}, err
	}
	data := make([]byte, 0, minInt64(policy.MaxBytes, 64<<10))
	buf := make([]byte, 64<<10)
	var total int64
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if total > policy.MaxBytes-int64(n) {
				_ = f.Close()
				return TrustedInput{}, fmt.Errorf("privatefs: trusted input exceeds limit")
			}
			data = append(data, buf[:n]...)
			total += int64(n)
		}
		if readErr != nil {
			if readErr != io.EOF {
				_ = f.Close()
				return TrustedInput{}, readErr
			}
			break
		}
	}
	openedAgain, err := f.Stat()
	closeErr := f.Close()
	if err != nil || closeErr != nil || !os.SameFile(info, openedAgain) {
		return TrustedInput{}, fmt.Errorf("privatefs: trusted input identity changed")
	}
	after, err := validateTrustedChain(root, rel, policy)
	if err != nil || len(after) != len(identities) {
		return TrustedInput{}, fmt.Errorf("privatefs: trusted input chain changed")
	}
	for i := range identities {
		if !os.SameFile(identities[i], after[i]) {
			return TrustedInput{}, fmt.Errorf("privatefs: trusted input chain changed")
		}
	}
	return TrustedInput{Bytes: data, Info: info}, nil
}

func validateTrustedChain(root *os.Root, rel string, policy TrustedInputPolicy) ([]os.FileInfo, error) {
	parts := strings.FieldsFunc(filepath.ToSlash(rel), func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return nil, fmt.Errorf("privatefs: empty trusted input")
	}
	identities := make([]os.FileInfo, 0, len(parts))
	current := ""
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("privatefs: unsafe trusted input component")
		}
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !info.IsDir()) || (i == len(parts)-1 && !info.Mode().IsRegular()) {
			return nil, fmt.Errorf("privatefs: trusted input link or type rejected")
		}
		if err := validateTrustedPathInfo(current, info, i == len(parts)-1, policy); err != nil {
			return nil, err
		}
		identities = append(identities, info)
	}
	return identities, nil
}

func minInt64(a, b int64) int {
	if a < b {
		return int(a)
	}
	return int(b)
}
