package projectdiscovery

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/watcher"
)

type PathPolicy struct {
	StateDir     string
	ExcludeRoots []string
}
type FileIdentity struct {
	Platform      string   `json:"platform"`
	UnixDevice    uint64   `json:"unixDevice,omitempty"`
	UnixInode     uint64   `json:"unixInode,omitempty"`
	VolumeSerial  uint64   `json:"volumeSerial,omitempty"`
	WindowsFileID [16]byte `json:"windowsFileId,omitempty"`
}
type ResolvedCandidate struct {
	CanonicalPath string
	Identity      FileIdentity
}

func (p PathPolicy) ResolveCandidate(path string) (ResolvedCandidate, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ResolvedCandidate{}, fmt.Errorf("projectdiscovery: absolute candidate: %w", err)
	}
	if !validCandidatePath(abs) {
		return ResolvedCandidate{}, fmt.Errorf("projectdiscovery: root is not a project candidate")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ResolvedCandidate{}, fmt.Errorf("projectdiscovery: resolve candidate: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return ResolvedCandidate{}, err
	}
	dir, err := os.Open(resolved)
	if err != nil {
		return ResolvedCandidate{}, err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return ResolvedCandidate{}, err
	}
	if !info.IsDir() {
		return ResolvedCandidate{}, fmt.Errorf("projectdiscovery: candidate is not a directory")
	}
	roots := append([]string{p.StateDir}, p.ExcludeRoots...)
	for _, raw := range roots {
		if raw == "" {
			continue
		}
		root, err := resolvePolicyRoot(raw)
		if err != nil {
			return ResolvedCandidate{}, err
		}
		if pathWithin(root, abs) || pathWithin(root, resolved) {
			return ResolvedCandidate{}, fmt.Errorf("projectdiscovery: candidate is under an excluded root")
		}
	}
	if excludedSegments(abs) || excludedSegments(resolved) {
		return ResolvedCandidate{}, fmt.Errorf("projectdiscovery: candidate contains an excluded segment")
	}
	id, err := fileIdentityFromHandle(dir, info)
	if err != nil {
		return ResolvedCandidate{}, err
	}
	return ResolvedCandidate{CanonicalPath: filepath.Clean(resolved), Identity: id}, nil
}

func resolvePolicyRoot(raw string) (string, error) {
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	suffix := []string{}
	cur := filepath.Clean(abs)
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("projectdiscovery: unsafe exclusion root: %w", err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("projectdiscovery: exclusion root has no resolvable ancestor")
		}
		base := filepath.Base(cur)
		if base == "." || base == ".." || strings.ContainsAny(base, `/\`) {
			return "", fmt.Errorf("projectdiscovery: unsafe exclusion component")
		}
		suffix = append(suffix, base)
		cur = parent
	}
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(caseFoldPath(root), caseFoldPath(candidate))
	return err == nil && (rel == "." || (!filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}
func excludedSegments(path string) bool {
	for _, seg := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if watcher.SkipWalkDir(seg) {
			return true
		}
	}
	return false
}
func (p PathPolicy) RevalidateOpenedDirectory(dir *os.File, want FileIdentity) error {
	if dir == nil {
		return fmt.Errorf("projectdiscovery: nil directory")
	}
	info, err := dir.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("projectdiscovery: opened object is not a directory")
	}
	got, err := fileIdentityFromHandle(dir, info)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("projectdiscovery: directory identity changed")
	}
	return nil
}
