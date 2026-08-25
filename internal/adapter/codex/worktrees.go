package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

const (
	codexWorktreeScanMaxEntries = 10_000
	codexWorktreeScanMaxCount   = 1_000
	codexWorktreeScanMaxRoots   = 16
	codexGitMetadataMaxBytes    = 64 << 10
	codexSkillPathMinParts      = 4
)

// managedCodexWorktree is one app-managed linked worktree and the primary
// checkout recovered from its Git administrative metadata. Path remains the
// app-visible lexical path used for materialization; Origin is fully resolved
// so a forged pointer cannot win a comparison through a symlink alias.
type managedCodexWorktree struct {
	Path   string
	Origin string
}

var (
	_ adapter.NativeMirrorTarget            = (*Adapter)(nil)
	_ adapter.NativeMirrorFirstContactGuard = (*Adapter)(nil)
)

func (a *Adapter) managedWorktreeRoots() []string {
	if a.WorktreeRoots != nil {
		return append([]string(nil), a.WorktreeRoots...)
	}
	if a.HomeDir == "" {
		return nil
	}
	return []string{filepath.Join(a.HomeDir, ".codex", "worktrees")}
}

func (a *Adapter) hasManagedWorktreeRoot() bool {
	for rootIndex, root := range a.managedWorktreeRoots() {
		if rootIndex >= codexWorktreeScanMaxRoots {
			break
		}
		if _, ok := validCodexWorktreeRoot(root); ok {
			return true
		}
	}
	return false
}

// managedWorktrees enumerates only linked Git worktrees below Codex's
// app-owned root. filepath.WalkDir does not follow directory symlinks; the
// explicit Lstat and physical-containment checks below also reject a symlinked
// root and aliases that resolve outside it. Traversal and result caps bound a
// corrupt or adversarial app cache.
func (a *Adapter) managedWorktrees() []managedCodexWorktree {
	seen := make(map[string]struct{})
	var out []managedCodexWorktree
	for _, root := range a.managedWorktreeRoots() {
		for _, worktree := range scanCodexWorktreeRoot(root, codexWorktreeScanMaxEntries, codexWorktreeScanMaxCount) {
			key := filepath.Clean(worktree.Path)
			if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
				key = strings.ToLower(key)
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, worktree)
			if len(out) >= codexWorktreeScanMaxCount {
				break
			}
		}
		if len(out) >= codexWorktreeScanMaxCount {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// NativeMirrorTopologyToken implements adapter.NativeMirrorTopologySource.
// The token changes only when the set of validated managed worktrees changes,
// allowing the orchestrator to seed a newly-created Desktop worktree with
// already-existing project context.
func (a *Adapter) NativeMirrorTopologyToken() string {
	worktrees := a.managedWorktrees()
	entries := make([]string, 0, len(worktrees))
	for _, worktree := range worktrees {
		entry := filepath.Clean(worktree.Origin) + "\x00" + filepath.Clean(worktree.Path)
		if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
			entry = strings.ToLower(entry)
		}
		entries = append(entries, entry)
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return hex.EncodeToString(sum[:])
}

func validCodexWorktreeRoot(root string) (string, bool) {
	if root == "" || !filepath.IsAbs(root) {
		return "", false
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", false
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil || !resolvedInfo.IsDir() {
		return "", false
	}
	return filepath.Clean(resolved), true
}

func scanCodexWorktreeRoot(root string, maxEntries, maxWorktrees int) []managedCodexWorktree {
	resolvedRoot, ok := validCodexWorktreeRoot(root)
	if !ok || maxEntries <= 0 || maxWorktrees <= 0 {
		return nil
	}
	root = filepath.Clean(root)
	entries := 0
	var out []managedCodexWorktree
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		entries++
		if entries > maxEntries || len(out) >= maxWorktrees {
			return filepath.SkipAll
		}
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			// WalkDir already avoids following this entry. Be explicit so this
			// invariant is visible if the traversal implementation changes.
			return nil
		}
		if path == root || !entry.IsDir() {
			return nil
		}

		gitInfo, gitErr := os.Lstat(filepath.Join(path, ".git"))
		if gitErr != nil {
			if !os.IsNotExist(gitErr) {
				return filepath.SkipDir
			}
			return nil
		}
		// A directory containing any .git entry is a repository boundary. Do
		// not traverse its checked-out content looking for nested repositories.
		if gitInfo.Mode()&os.ModeSymlink == 0 && gitInfo.Mode().IsRegular() {
			if worktree, valid := linkedCodexWorktree(path, resolvedRoot); valid {
				out = append(out, worktree)
			}
		}
		return filepath.SkipDir
	})
	return out
}

// linkedCodexWorktree validates Git's public linked-worktree pointer shape:
//
//	<worktree>/.git -> gitdir: <origin>/.git/worktrees/<id>
//	<gitdir>/commondir -> ../..
//
// The resolved common directory must be the origin's .git directory and the
// gitdir must live below its worktrees administration directory.
func linkedCodexWorktree(candidate, resolvedManagedRoot string) (managedCodexWorktree, bool) {
	info, err := os.Lstat(candidate)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return managedCodexWorktree{}, false
	}
	resolvedCandidate, err := filepath.EvalSymlinks(filepath.Clean(candidate))
	if err != nil || !codexPathStrictlyWithin(resolvedManagedRoot, resolvedCandidate) {
		return managedCodexWorktree{}, false
	}

	gitFile := filepath.Join(candidate, ".git")
	rawGitDir, ok := readCodexGitMetadata(gitFile)
	if !ok || !strings.HasPrefix(rawGitDir, "gitdir:") {
		return managedCodexWorktree{}, false
	}
	gitDirValue := strings.TrimSpace(strings.TrimPrefix(rawGitDir, "gitdir:"))
	if gitDirValue == "" || strings.ContainsRune(gitDirValue, '\x00') {
		return managedCodexWorktree{}, false
	}
	gitDir := gitDirValue
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(candidate, gitDir)
	}
	gitDir = filepath.Clean(gitDir)
	gitDirInfo, err := os.Lstat(gitDir)
	if err != nil || !gitDirInfo.IsDir() || gitDirInfo.Mode()&os.ModeSymlink != 0 {
		return managedCodexWorktree{}, false
	}
	resolvedGitDir, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		return managedCodexWorktree{}, false
	}

	rawCommonDir, ok := readCodexGitMetadata(filepath.Join(resolvedGitDir, "commondir"))
	if !ok || rawCommonDir == "" || strings.ContainsRune(rawCommonDir, '\x00') {
		return managedCodexWorktree{}, false
	}
	commonDir := strings.TrimSpace(rawCommonDir)
	if !filepath.IsAbs(commonDir) {
		// Preserve the gitdir pointer's lexical path for user-facing project
		// discovery (notably macOS /var -> /private/var) while validating the
		// fully resolved path below.
		commonDir = filepath.Join(gitDir, commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	commonInfo, err := os.Lstat(commonDir)
	if err != nil || !commonInfo.IsDir() || commonInfo.Mode()&os.ModeSymlink != 0 {
		return managedCodexWorktree{}, false
	}
	resolvedCommonDir, err := filepath.EvalSymlinks(commonDir)
	if err != nil || filepath.Base(resolvedCommonDir) != ".git" {
		return managedCodexWorktree{}, false
	}
	if !codexPathStrictlyWithin(filepath.Join(resolvedCommonDir, "worktrees"), resolvedGitDir) {
		return managedCodexWorktree{}, false
	}
	origin := filepath.Dir(commonDir)
	originInfo, err := os.Stat(origin)
	if err != nil || !originInfo.IsDir() {
		return managedCodexWorktree{}, false
	}
	return managedCodexWorktree{Path: filepath.Clean(candidate), Origin: origin}, true
}

func readCodexGitMetadata(path string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > codexGitMetadataMaxBytes {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, codexGitMetadataMaxBytes+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || len(raw) > codexGitMetadataMaxBytes {
		return "", false
	}
	return strings.TrimSpace(string(raw)), true
}

// NativeMirrorPaths implements adapter.NativeMirrorTarget. Codex Desktop
// sessions run in linked worktrees under $CODEX_HOME/worktrees, so project
// instructions and skills must be copied into each verified worktree. Global
// state is shared already; conversations and tool config remain app-owned or
// have process-level semantics and are deliberately excluded.
func (a *Adapter) NativeMirrorPaths(artifact acf.Artifact, contextDir, primaryPath string) ([]string, error) {
	if artifact.Scope != acf.ScopeProject || contextDir == "" || primaryPath == "" {
		return nil, nil
	}
	if artifact.Kind != acf.KindMemory && artifact.Kind != acf.KindSkill {
		return nil, nil
	}
	origin, err := filepath.Abs(contextDir)
	if err != nil {
		return nil, nil
	}
	resolvedOrigin, err := filepath.EvalSymlinks(origin)
	if err != nil {
		return nil, nil
	}
	primary, err := filepath.Abs(primaryPath)
	if err != nil {
		return nil, nil
	}
	rel, inside := codexRelativeWithin(origin, primary)
	if !inside || !validCodexMirrorRelativePath(artifact.Kind, rel) {
		return nil, nil
	}

	seen := make(map[string]struct{})
	var out []string
	for _, worktree := range a.managedWorktrees() {
		if !sameCodexPath(worktree.Origin, resolvedOrigin) || !safeCodexMirrorParent(worktree.Path, rel) {
			continue
		}
		dest := filepath.Join(worktree.Path, rel)
		if sameCodexPath(dest, primary) {
			continue
		}
		key := filepath.Clean(dest)
		if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, dest)
	}
	sort.Strings(out)
	return out, nil
}

// NativeMirrorFirstContactSafe implements adapter.NativeMirrorFirstContactGuard.
// An existing worktree file is writable only when Git proves it is unchanged
// or it exactly matches an earlier Aplexica materialization.
func (a *Adapter) NativeMirrorFirstContactSafe(store *acf.Store, artifact acf.Artifact, mirrorPath string) (bool, error) {
	switch artifact.Kind {
	case acf.KindMemory:
		return adapter.SafeNativeMirrorFirstContact(store, artifact, mirrorPath, memoryDecode, false)
	case acf.KindSkill:
		return adapter.SafeNativeMirrorFirstContact(store, artifact, mirrorPath, skillDecode, true)
	default:
		return false, nil
	}
}

func validCodexMirrorRelativePath(kind acf.Kind, rel string) bool {
	switch kind {
	case acf.KindMemory:
		return rel == "AGENTS.md"
	case acf.KindSkill:
		parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
		return len(parts) >= codexSkillPathMinParts && parts[0] == ".agents" && parts[1] == "skills" && parts[len(parts)-1] == "SKILL.md"
	default:
		return false
	}
}

func safeCodexMirrorParent(worktree, rel string) bool {
	if _, inside := codexRelativeWithin(worktree, filepath.Join(worktree, rel)); !inside {
		return false
	}
	parentRel := filepath.Dir(rel)
	if parentRel == "." {
		return true
	}
	cur := filepath.Clean(worktree)
	for _, part := range strings.Split(parentRel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return true
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

func sameCodexPath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a = resolveCodexPath(a)
	b = resolveCodexPath(b)
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// resolveCodexPath resolves the longest existing prefix and reattaches any
// missing suffix. filepath.EvalSymlinks alone cannot compare a path whose
// final file or directory has not been materialized yet.
func resolveCodexPath(path string) string {
	path = filepath.Clean(path)
	cur := path
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return path
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}

func codexPathStrictlyWithin(base, candidate string) bool {
	rel, ok := codexRelativeWithin(base, candidate)
	return ok && rel != "."
}

func codexRelativeWithin(base, candidate string) (string, bool) {
	base = filepath.Clean(base)
	candidate = filepath.Clean(candidate)
	if sameCodexPath(base, candidate) {
		return ".", true
	}
	rel, err := filepath.Rel(base, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// normalizeManagedWorktreeCwd maps a Desktop session's worktree cwd back to
// the primary checkout. A cwd below the worktree retains its relative suffix.
func normalizeManagedWorktreeCwd(cwd string, worktrees []managedCodexWorktree) string {
	bestPath := ""
	bestOrigin := ""
	bestRel := ""
	for _, worktree := range worktrees {
		rel, inside := codexRelativeWithin(worktree.Path, cwd)
		if !inside || len(worktree.Path) <= len(bestPath) {
			continue
		}
		bestPath = worktree.Path
		bestOrigin = worktree.Origin
		bestRel = rel
	}
	if bestPath == "" {
		return cwd
	}
	if bestRel == "." {
		return bestOrigin
	}
	return filepath.Join(bestOrigin, bestRel)
}
