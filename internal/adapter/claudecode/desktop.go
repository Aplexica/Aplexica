package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// Claude Code Desktop deliberately maintains a separate session list while
// running the same Claude Code engine as the CLI. These app-owned records map
// the CLI transcript id to Desktop's automatic worktree and, critically, the
// original project cwd. Aplexica creates or updates only its deterministic
// local_<cliSessionId>.json record beside an existing account/workspace record.
// This makes synced sessions visible without invoking a claude:// URL, which
// would launch or focus the application. Unknown app-owned fields and explicit
// user titles are preserved on every update.
type desktopSessionRecord struct {
	SessionID      string `json:"sessionId"`
	CLISessionID   string `json:"cliSessionId"`
	Cwd            string `json:"cwd"`
	OriginCwd      string `json:"originCwd"`
	WorktreePath   string `json:"worktreePath"`
	IsArchived     bool   `json:"isArchived"`
	LastFocusedAt  int64  `json:"lastFocusedAt"`
	CreatedAt      int64  `json:"createdAt"`
	LastActivityAt int64  `json:"lastActivityAt"`
	Title          string `json:"title"`
	TitleSource    string `json:"titleSource"`
}

// NativeMirrorPaths implements adapter.NativeMirrorTarget. Claude Code
// Desktop creates active Git worktrees beneath <project>/.claude/worktrees;
// an uncommitted instruction/config update in the primary checkout is not
// visible there. Mirror project artifacts into verified active worktrees so
// an already-open Desktop session consumes the same state. App catalog paths
// are untrusted input: only existing linked worktrees under the documented
// project-local root qualify.
func (a *Adapter) NativeMirrorPaths(artifact acf.Artifact, contextDir, primaryPath string) ([]string, error) {
	if artifact.Scope != acf.ScopeProject || contextDir == "" || primaryPath == "" {
		return nil, nil
	}
	switch artifact.Kind {
	case acf.KindMemory, acf.KindSkill:
	default:
		return nil, nil
	}
	origin, err := filepath.Abs(contextDir)
	if err != nil {
		return nil, nil
	}
	primary, err := filepath.Abs(primaryPath)
	if err != nil {
		return nil, nil
	}
	rel, err := filepath.Rel(origin, primary)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, nil
	}

	seen := map[string]struct{}{}
	var mirrors []string
	for _, record := range a.desktopSessions() {
		if record.IsArchived || !sameClaudePath(record.OriginCwd, origin) {
			continue
		}
		worktree := record.WorktreePath
		if worktree == "" {
			worktree = record.Cwd
		}
		if !validClaudeDesktopWorktree(origin, worktree) || !safeClaudeMirrorRelativePath(worktree, rel) {
			continue
		}
		dest := filepath.Join(filepath.Clean(worktree), rel)
		if dest == primary {
			continue
		}
		if _, duplicate := seen[dest]; duplicate {
			continue
		}
		seen[dest] = struct{}{}
		mirrors = append(mirrors, dest)
	}
	sort.Strings(mirrors)
	return mirrors, nil
}

// NativeMirrorFirstContactSafe implements adapter.NativeMirrorFirstContactGuard.
// Existing worktree files are overwritten only when Git proves they are an
// untouched checkout copy or their bytes match an earlier Aplexica export.
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

func sameClaudePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func validClaudeDesktopWorktree(origin, candidate string) bool {
	if !filepath.IsAbs(candidate) {
		return false
	}
	root := filepath.Join(filepath.Clean(origin), ".claude", "worktrees")
	worktree := filepath.Clean(candidate)
	rel, err := filepath.Rel(root, worktree)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	resolvedOrigin, err := filepath.EvalSymlinks(filepath.Clean(origin))
	if err != nil {
		return false
	}
	originRel, err := filepath.Rel(resolvedOrigin, resolvedRoot)
	if err != nil || originRel == "." || originRel == ".." || strings.HasPrefix(originRel, ".."+string(filepath.Separator)) {
		return false
	}
	resolvedWorktree, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return false
	}
	resolvedRel, err := filepath.Rel(resolvedRoot, resolvedWorktree)
	if err != nil || resolvedRel == "." || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
		return false
	}
	info, err := os.Lstat(worktree)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	// A linked Git worktree has a .git pointer file. Requiring it keeps a
	// forged/corrupt app record from turning arbitrary subdirectories into
	// Aplexica write targets.
	gitInfo, err := os.Lstat(filepath.Join(worktree, ".git"))
	return err == nil && gitInfo.Mode().IsRegular()
}

// safeClaudeMirrorRelativePath rejects an existing symlink in the destination
// parent chain. Missing components are okay: the normal exporter creates them
// beneath the already-validated worktree.
func safeClaudeMirrorRelativePath(worktree, rel string) bool {
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
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false
		}
	}
	return true
}

const (
	desktopSessionFileMaxBytes = 8 << 20
	desktopSessionFileMaxCount = 10_000
	desktopFingerprintRadix    = 10
)

type desktopSessionCandidate struct {
	path        string
	size        int64
	modTime     int64
	mode        fs.FileMode
	regularFile bool
}

func (a *Adapter) desktopSessionCatalogRoots() []string {
	if a.DesktopSessionRoots != nil {
		return append([]string(nil), a.DesktopSessionRoots...)
	}
	if a.HomeDir == "" {
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		return []string{filepath.Join(a.HomeDir, "Library", "Application Support", "Claude", "claude-code-sessions")}
	case "windows":
		// Honor redirected roaming profiles for the real user. Tests and
		// callers that override HomeDir stay deterministic via the fallback.
		roaming := claudeWindowsRoamingAppData(a.HomeDir)
		return claudeWindowsDesktopSessionCatalogRoots(claudeWindowsLocalAppData(a.HomeDir), roaming)
	case "linux":
		// Claude Desktop for Linux follows the per-user XDG config tree. Keep
		// both observed capitalization variants as read-only candidates while
		// the app is in beta.
		base := filepath.Join(a.HomeDir, ".config")
		if claudeActualUserHome(a.HomeDir) {
			if xdg := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
				base = xdg
			}
		}
		return []string{
			filepath.Join(base, "Claude", "claude-code-sessions"),
			filepath.Join(base, "claude", "claude-code-sessions"),
		}
	default:
		return nil
	}
}

func claudeWindowsDesktopSessionCatalogRoots(localAppData, roamingAppData string) []string {
	return []string{
		filepath.Join(localAppData, "Packages", claudeWindowsPackageFamily, "LocalCache", "Roaming", "Claude", "claude-code-sessions"),
		filepath.Join(roamingAppData, "Claude", "claude-code-sessions"),
	}
}

func (a *Adapter) hasDesktopSessionCatalog() bool {
	for _, root := range a.desktopSessionCatalogRoots() {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// NativeMirrorTopologyToken implements adapter.NativeMirrorTopologySource.
// Only the validated origin/worktree membership contributes: focus/activity
// timestamps change constantly while a Desktop session runs and must not cause
// repeated re-fanout of otherwise unchanged project context.
func (a *Adapter) NativeMirrorTopologyToken() string {
	seen := map[string]struct{}{}
	var topology []string
	for _, record := range a.desktopSessions() {
		if record.IsArchived || record.OriginCwd == "" {
			continue
		}
		worktree := record.WorktreePath
		if worktree == "" {
			worktree = record.Cwd
		}
		if !validClaudeDesktopWorktree(record.OriginCwd, worktree) {
			continue
		}
		entry := filepath.Clean(record.OriginCwd) + "\x00" + filepath.Clean(worktree)
		if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
			entry = strings.ToLower(entry)
		}
		if _, duplicate := seen[entry]; duplicate {
			continue
		}
		seen[entry] = struct{}{}
		topology = append(topology, entry)
	}
	sort.Strings(topology)
	sum := sha256.Sum256([]byte(strings.Join(topology, "\n")))
	return hex.EncodeToString(sum[:])
}

// desktopSessions reads the small subset of app metadata needed for project
// discovery. Corrupt, oversized, symlinked, and unrelated files are ignored;
// a broken UI cache must never prevent the shared ~/.claude adapter starting.
//
// Every call scans candidate metadata and fingerprints it before consulting
// the decoded-record cache. This keeps fanout bursts from repeatedly decoding
// the app's large JSON records without hiding a record created during the old
// time-based cache window.
func (a *Adapter) desktopSessions() []desktopSessionRecord {
	roots := a.desktopSessionCatalogRoots()
	candidates, cacheKey := scanDesktopSessionCandidates(roots, desktopSessionFileMaxCount)
	a.desktopCacheMu.Lock()
	defer a.desktopCacheMu.Unlock()
	if cacheKey == a.desktopCacheKey {
		return append([]desktopSessionRecord(nil), a.desktopCacheRecords...)
	}

	var out []desktopSessionRecord
	for _, candidate := range candidates {
		if !candidate.regularFile {
			continue
		}
		record, ok := readDesktopSessionRecord(candidate.path)
		if ok {
			out = append(out, record)
		}
	}
	a.desktopCacheKey = cacheKey
	a.desktopCacheRecords = append(a.desktopCacheRecords[:0], out...)
	return append([]desktopSessionRecord(nil), out...)
}

// desktopSessionForCLI returns the newest Claude Code Desktop catalog record
// for one CLI transcript. Desktop owns the catalog; callers use this read-only
// projection to preserve user-facing metadata without writing private app
// state.
func (a *Adapter) desktopSessionForCLI(cliSessionID string) (desktopSessionRecord, bool) {
	if cliSessionID == "" {
		return desktopSessionRecord{}, false
	}
	var best desktopSessionRecord
	found := false
	for _, record := range a.desktopSessions() {
		if record.CLISessionID != cliSessionID {
			continue
		}
		if !found || record.lastActive().After(best.lastActive()) {
			best = record
			found = true
		}
	}
	return best, found
}

func (a *Adapter) isDesktopSessionRecordPath(path string) bool {
	if !strings.HasPrefix(filepath.Base(path), "local_") || filepath.Ext(path) != ".json" {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for _, root := range a.desktopSessionCatalogRoots() {
		absRoot, rootErr := filepath.Abs(root)
		if rootErr != nil {
			continue
		}
		rel, relErr := filepath.Rel(absRoot, absPath)
		if relErr == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (a *Adapter) desktopTranscriptPath(record desktopSessionRecord) (string, bool) {
	if a.HomeDir == "" || record.CLISessionID == "" || filepath.Base(record.CLISessionID) != record.CLISessionID {
		return "", false
	}
	seen := map[string]struct{}{}
	for _, cwd := range []string{record.Cwd, record.WorktreePath, record.OriginCwd} {
		if !filepath.IsAbs(cwd) {
			continue
		}
		path := filepath.Join(a.HomeDir, ".claude", "projects", encodeProjectDir(filepath.Clean(cwd)), record.CLISessionID+".jsonl")
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		info, err := os.Lstat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return path, true
		}
	}
	return "", false
}

// scanDesktopSessionCandidates returns at most limit matching catalog entries
// plus a stable metadata fingerprint. Matching but corrupt, unreadable, or
// symlinked entries still consume the limit, preventing an attacker-controlled
// catalog from defeating the traversal bound with invalid files. SkipAll ends
// the current tree immediately and the outer loop stops before another root.
func scanDesktopSessionCandidates(roots []string, limit int) ([]desktopSessionCandidate, string) {
	var candidates []desktopSessionCandidate
	attempted := 0
	limitReached := limit <= 0

	for _, root := range roots {
		if limitReached {
			break
		}
		if root == "" {
			continue
		}
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if entry != nil && entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				if entry.Type()&os.ModeSymlink != 0 {
					return filepath.SkipDir
				}
				return nil
			}
			name := entry.Name()
			if !strings.HasPrefix(name, "local_") || filepath.Ext(name) != ".json" {
				return nil
			}

			attempted++
			candidate := desktopSessionCandidate{path: path}
			if info, err := entry.Info(); err == nil {
				candidate.size = info.Size()
				candidate.modTime = info.ModTime().UnixNano()
				candidate.mode = info.Mode()
				candidate.regularFile = info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
			}
			candidates = append(candidates, candidate)

			if attempted >= limit {
				limitReached = true
				return filepath.SkipAll
			}
			return nil
		})
	}

	return candidates, desktopSessionCandidateFingerprint(roots, candidates, attempted, limitReached)
}

func desktopSessionCandidateFingerprint(roots []string, candidates []desktopSessionCandidate, attempted int, limitReached bool) string {
	var manifest strings.Builder
	for _, root := range roots {
		appendDesktopFingerprintField(&manifest, root)
	}
	for _, candidate := range candidates {
		appendDesktopFingerprintField(&manifest, candidate.path)
		appendDesktopFingerprintField(&manifest, strconv.FormatInt(candidate.size, desktopFingerprintRadix))
		appendDesktopFingerprintField(&manifest, strconv.FormatInt(candidate.modTime, desktopFingerprintRadix))
		appendDesktopFingerprintField(&manifest, strconv.FormatUint(uint64(candidate.mode), desktopFingerprintRadix))
	}
	appendDesktopFingerprintField(&manifest, strconv.Itoa(attempted))
	appendDesktopFingerprintField(&manifest, strconv.FormatBool(limitReached))
	sum := sha256.Sum256([]byte(manifest.String()))
	return hex.EncodeToString(sum[:])
}

func appendDesktopFingerprintField(manifest *strings.Builder, field string) {
	manifest.WriteString(strconv.Itoa(len(field)))
	manifest.WriteByte(':')
	manifest.WriteString(field)
	manifest.WriteByte(';')
}

func readDesktopSessionRecord(path string) (desktopSessionRecord, bool) {
	f, err := os.Open(path)
	if err != nil {
		return desktopSessionRecord{}, false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, desktopSessionFileMaxBytes+1))
	if err != nil || len(raw) > desktopSessionFileMaxBytes {
		return desktopSessionRecord{}, false
	}
	var record desktopSessionRecord
	if json.Unmarshal(raw, &record) != nil || record.SessionID == "" {
		return desktopSessionRecord{}, false
	}
	return record, true
}

func (r desktopSessionRecord) projectPath() string {
	if filepath.IsAbs(r.OriginCwd) {
		return filepath.Clean(r.OriginCwd)
	}
	if filepath.IsAbs(r.Cwd) {
		return filepath.Clean(r.Cwd)
	}
	return ""
}

func (r desktopSessionRecord) lastActive() time.Time {
	millis := r.LastActivityAt
	if r.LastFocusedAt > millis {
		millis = r.LastFocusedAt
	}
	if r.CreatedAt > millis {
		millis = r.CreatedAt
	}
	if millis <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(millis).UTC()
}
