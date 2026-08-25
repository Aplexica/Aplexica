// Package project implements BRD-02 §4.13's project identity strategy.
// Pure data layer: given a filesystem path, returns a ProjectInfo with
// a canonical project ID, the local path, the VCS type, and an
// ephemeral flag. No I/O beyond reading VCS config files.
//
// Consumers:
//   - Adapters (Import path: tag artifacts with scope+project on read)
//   - Orchestrator (stage-and-wait routing for project-scope artifacts
//     whose project isn't known locally)
//   - CLI (`aplexica project init/link/rename/list`)
//   - Tray (pending-projects menu)
package project

import (
	"fmt"
	"os"
	"path/filepath"
)

// ProjectInfo is the canonical per-project record. Mirrors BRD-02 §4.13.1's
// `scope.project` field shape. The ID is identity; the Path is a per-device
// hint that may differ across machines (same repo cloned at different
// paths). VCS distinguishes "we read this from .git/config" from "we
// derived this from path hashing."
type ProjectInfo struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	VCS       string `json:"vcs"` // "git" | "hg" | "none"
	Ephemeral bool   `json:"ephemeral,omitempty"`
}

// Detect walks up from path looking for a VCS marker (.git, .hg) and
// returns a populated ProjectInfo:
//
//   - For a git repo: VCS="git", ID from normalized origin remote URL
//     (or path-derived fallback when origin isn't configured).
//   - For a hg repo: VCS="hg", path-derived ID.
//   - For a non-VCS path: VCS="none", path-derived ID rooted at the
//     ORIGINAL input path (not the walk-up cursor, since "non-VCS"
//     means no project boundary was found).
//
// Walk-up terminates at $HOME or filesystem root, whichever comes
// first. Ephemeral is never set here — callers set it explicitly via
// `aplexica project init --ephemeral`.
//
// Errors only when path resolution itself fails (e.g., a relative path
// that can't be made absolute). VCS-config read errors are non-fatal
// and fall through to the path-derived ID branch.
func Detect(path string) (ProjectInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ProjectInfo{}, fmt.Errorf("project: abs path: %w", err)
	}
	abs = filepath.Clean(abs)
	if physical, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		abs = filepath.Clean(physical)
	}
	home, _ := os.UserHomeDir()
	if physicalHome, evalErr := filepath.EvalSymlinks(home); evalErr == nil {
		home = filepath.Clean(physicalHome)
	}

	cur := abs
	for {
		if isGitRepo(cur) {
			id, vcs := identifyGit(cur)
			return ProjectInfo{ID: id, Path: cur, VCS: vcs}, nil
		}
		if isHgRepo(cur) {
			id, vcs := identifyHg(cur)
			return ProjectInfo{ID: id, Path: cur, VCS: vcs}, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break // reached root
		}
		if home != "" && cur == home {
			break // don't walk above $HOME
		}
		cur = parent
	}
	return ProjectInfo{
		ID:   pathDerivedID(abs),
		Path: abs,
		VCS:  "none",
	}, nil
}

func isGitRepo(p string) bool {
	fi, err := os.Stat(filepath.Join(p, ".git"))
	if err != nil {
		return false
	}
	// .git can be a directory (regular repo) OR a file (git worktree
	// or submodule with `gitdir:` pointer). Either qualifies.
	return fi.IsDir() || fi.Mode().IsRegular()
}

func isHgRepo(p string) bool {
	fi, err := os.Stat(filepath.Join(p, ".hg"))
	if err != nil {
		return false
	}
	return fi.IsDir()
}

// identifyGit returns (id, vcs) for a directory we've already
// confirmed to be a git repo via isGitRepo. When .git/config has an
// origin remote, the normalized URL is the ID. When origin is missing
// OR unparseable, falls back to path-derived ID — VCS is still "git"
// (we KNOW it's a git repo, just no upstream).
func identifyGit(repoDir string) (string, string) {
	if url, err := readGitOriginURL(repoDir); err == nil && url != "" {
		if norm := normalizeGitURL(url); norm != "" {
			return norm, "git"
		}
	}
	return pathDerivedID(repoDir), "git"
}

// identifyHg returns (id, vcs) for a directory we've already confirmed
// to be a mercurial repo via isHgRepo. Reads .hg/hgrc's [paths] section
// for the "default" URL — that's hg's equivalent of git's "origin"
// remote and the conventional upstream for `hg pull` / `hg push`.
//
// Falls back to path-derived ID when:
//   - hgrc doesn't exist (a brand-new local repo)
//   - the [paths] section has no "default" entry
//   - the URL is unparseable
//
// VCS is always "hg" — we KNOW the repo is mercurial, we just may not
// have an upstream identity. v0.60.0; BRD-02 §4.13.3 hg-aware
// identity.
func identifyHg(repoDir string) (string, string) {
	if url, err := readHgDefaultURL(repoDir); err == nil && url != "" {
		if norm := normalizeHgURL(url); norm != "" {
			return norm, "hg"
		}
	}
	return pathDerivedID(repoDir), "hg"
}
