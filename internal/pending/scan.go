package pending

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aplexica/aplexica/internal/project"
)

// Detected is one VCS repository found during a filesystem scan.
// Returned by Scan; consumers cross-reference against pending project
// IDs to decide whether to auto-link (BRD-02 §4.13.4 last paragraph:
// "the daemon detects the git remote on its next scan").
type Detected struct {
	// Path is the absolute working-copy path on this device.
	Path string
	// Info is the resolved project identity (canonical ID via git
	// remote / hg default path, OR path-derived for repos without
	// upstream config).
	Info project.ProjectInfo
}

// Scan walks each root recursively up to maxDepth, looking for
// directories containing a `.git/` or `.hg/` marker. For each match
// it calls project.Detect and returns the resulting (Path, Info) pair.
// Stops descending into a directory once it's identified as a VCS
// root (a repo's own subdirs don't need rescanning — submodules /
// nested repos would have separate .git entries and get caught on
// their own).
//
// Skip rules:
//   - Dot-directories under maxDepth (.Trash, .cache, .Trashes etc.)
//     are never entered. The VCS marker check happens BEFORE skipping
//     so .git/.hg themselves don't accidentally get walked.
//   - Common large-tree dirs (node_modules, vendor, target) are
//     skipped to keep the scan fast on user homedirs.
//   - Symlinks are not followed (avoids infinite loops on circular
//     filesystem layouts).
//
// maxDepth==0 means "scan the root itself only, no descent".
// maxDepth<0 means "unbounded" — careful with system roots.
//
// Errors at the walk level (permission denied on an individual dir)
// are silently skipped; the overall Scan returns nil + the partial
// list. A nil-or-empty root list returns (nil, nil).
func Scan(roots []string, maxDepth int) ([]Detected, error) {
	var out []Detected
	seen := map[string]struct{}{}
	for _, r := range roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		if _, dup := seen[abs]; dup {
			continue
		}
		seen[abs] = struct{}{}
		walkOne(abs, 0, maxDepth, &out)
	}
	return out, nil
}

// walkOne is the recursive worker; depth is the current depth below
// the root (0 at the root itself). On finding a VCS marker it appends
// to out and returns WITHOUT descending into the matched dir's
// children (a repo's own subdirs don't need rescanning).
func walkOne(dir string, depth, maxDepth int, out *[]Detected) {
	// Stop check on hidden / skip-list dirs. Only applies below the
	// root (depth > 0): a dot-prefixed or skip-listed dir is allowed
	// as the scan root itself — e.g. scanning ~/.config or a dotfiles
	// repo directly must work (see shouldSkip's doc comment).
	if depth > 0 && shouldSkip(dir) {
		return
	}
	// VCS marker check.
	if isVCSRoot(dir) {
		if info, err := project.Detect(dir); err == nil {
			*out = append(*out, Detected{Path: dir, Info: info})
		}
		return // don't descend into a repo's subdirs
	}
	// Depth gate.
	if maxDepth >= 0 && depth >= maxDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		walkOne(filepath.Join(dir, e.Name()), depth+1, maxDepth, out)
	}
}

// isVCSRoot reports whether dir contains a `.git/` directory, a
// `.git` worktree pointer file, OR a `.hg/` directory.
func isVCSRoot(dir string) bool {
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if fi.IsDir() || fi.Mode().IsRegular() {
			return true
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, ".hg")); err == nil && fi.IsDir() {
		return true
	}
	return false
}

// shouldSkip identifies dirs we never descend into: dot-prefixed
// directory names (avoids walking ~/.cache, ~/.local, ~/.npm, etc.,
// each of which can contain tens of thousands of files), plus an
// explicit skip-list of well-known large directories.
func shouldSkip(dir string) bool {
	base := filepath.Base(dir)
	// Dot-prefixed dirs (but allow the root itself if it happens to
	// be one — e.g., scanning `~/.config` directly should work).
	if strings.HasPrefix(base, ".") {
		return true
	}
	switch base {
	case "node_modules", "vendor", "target", "build", "dist",
		"__pycache__", ".venv", "venv", ".gradle":
		return true
	}
	// macOS: never descend into the TCC-protected home folders. Walking
	// them (os.ReadDir) triggers a privacy permission prompt — Photos,
	// Music/Media, Desktop, Downloads, "data from other apps" (~/Library
	// containers) — on every (re)launch, and an ad-hoc-signed dev build
	// re-prompts on every update. A coding-project scan never needs these,
	// so skip them outright. ~/Documents is deliberately NOT in the set: it
	// is a common code location and stays scannable (a project the user
	// puts there is still auto-linked; the cost is a one-time prompt for a
	// dir they opted into, not an uninvited sweep of their media library).
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			if macProtectedHomeDir(dir, home) {
				return true
			}
		}
	}
	return false
}

// macProtectedHomeDirs are the macOS top-level home folders gated behind a
// TCC privacy prompt (Files & Folders, Photos, Media, app sandbox data).
var macProtectedHomeDirs = []string{
	"Desktop", "Downloads", "Music", "Movies", "Pictures",
	"Library", "Public", "Applications",
}

// macProtectedHomeDir reports whether dir is EXACTLY one of the macOS
// TCC-protected folders directly under home (never a child, never a
// same-named dir elsewhere). home is passed in for testability; callers use
// os.UserHomeDir(). Returns false for an empty home.
func macProtectedHomeDir(dir, home string) bool {
	if home == "" {
		return false
	}
	clean := filepath.Clean(dir)
	for _, name := range macProtectedHomeDirs {
		if clean == filepath.Join(home, name) {
			return true
		}
	}
	return false
}

// MatchPending takes the output of Scan + the current pending-projects
// list and returns the subset of detections that match an unlinked
// pending project ID. Callers can then auto-link each match via the
// project registry + trigger refanout.
//
// Match criterion: detected.Info.ID == pending.ID. Both fields
// already share the canonical "host/owner/repo" shape from
// project.Detect, so simple equality is enough.
func MatchPending(detected []Detected, pending []Project) []Detected {
	if len(detected) == 0 || len(pending) == 0 {
		return nil
	}
	pset := make(map[string]struct{}, len(pending))
	for _, p := range pending {
		pset[p.ID] = struct{}{}
	}
	var out []Detected
	for _, d := range detected {
		if _, ok := pset[d.Info.ID]; ok {
			out = append(out, d)
		}
	}
	return out
}

// HomeRoot returns a sensible default scan root: the user's home
// directory, with a fail-safe of "/" when home can't be determined
// (extremely rare; the resulting scan would be slow but bounded by
// maxDepth).
func HomeRoot() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "/"
}

// DefaultScanRoots returns the candidate root list when the daemon
// has no explicit --project-scan-roots configured: just $HOME. Users
// who organize repos under a single subdir (e.g. ~/code) can override
// for a faster scan. fmt is exported to satisfy go vet's unused-import
// detection when DefaultScanRoots is the only consumer.
func DefaultScanRoots() []string {
	_ = fmt.Sprintf
	return []string{HomeRoot()}
}
