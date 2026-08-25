package syncd

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/aplexica/aplexica/internal/atomicfile"
)

// importScanCacheName is the cache file's basename. It lives under the ACF
// store root so it shares the store's fate: deleting the store also deletes the
// cache, which forces a full re-import on the next start. Delete just this file
// to force a full re-import without dropping the store.
const importScanCacheName = ".import-scan-cache.json"

// importScanCacheSchemaVersion invalidates only entries whose adapter
// projection semantics changed. Versions 2 and 3 introduced v1.0.39 generated
// conversation repairs. Version 4 additionally schedules a byte-bounded set of
// the newest native Codex rollouts for the same one-time portable-projection
// repair, while preserving the rest of the potentially multi-gigabyte history.
const (
	generatedRepairImportScanCacheSchemaVersion = 2
	previousImportScanCacheSchemaVersion        = 3
	importScanCacheSchemaVersion                = 4
	codexSessionYearWidth                       = 4
	codexSessionMonthDayWidth                   = 2
	scanCacheMarkerInitialBuffer                = 64 * 1024
	scanCacheMarkerMaxLine                      = 1024 * 1024
	nativeCodexRepairBudgetBytes                = 256 << 20
	nativeCodexRepairMaxFiles                   = 16
)

type importScanCacheDisk struct {
	Version      int               `json:"version"`
	Fingerprints map[string]scanFP `json:"fingerprints"`
}

// scanFP is a file's cheap change-detection fingerprint: size + mod time. It
// deliberately does NOT hash content — the largest conversation logs run to
// hundreds of megabytes, and the whole point of the cache is to avoid touching
// their bytes on a restart. (size, mtime) is the same heuristic the filesystem
// watcher uses; agent artifacts are append-mostly, so any real change moves the
// size and/or the mtime.
type scanFP struct {
	Size    int64 `json:"size"`
	ModNano int64 `json:"mod_nano"`
}

// importScanCache remembers, per absolute path, the fingerprint a file had the
// last time the orchestrator successfully imported it. The startup InitialScan
// and the watcher "announce" path consult it to skip re-importing — and,
// crucially, re-encoding — files that are byte-for-byte unchanged since the
// previous run. Re-encoding the whole agent history is the dominant cost of
// daemon startup on a large store; this cache turns a multi-minute scan into a
// stat-only pass over unchanged files.
//
// Correctness: a false "unchanged" can only occur if a file's content changed
// while the daemon was down WITHOUT changing its size or mtime, which agent
// artifacts never do. A false "changed" merely re-imports (idempotent — the
// store content-dedups). A missing/corrupt cache treats every file as new,
// i.e. the previous full-scan behavior.
//
// All methods are safe on a nil receiver and on a cache with no backing path
// (empty store root), so callers need not branch on those cases.
type importScanCache struct {
	path  string
	mu    sync.Mutex
	fps   map[string]scanFP
	dirty bool
}

// loadImportScanCache reads the cache from under storeRoot. A missing or
// unreadable/corrupt file yields an empty cache (full re-import), never an
// error — the cache is a pure optimization. An empty storeRoot yields a
// cache with no backing path: it tracks fingerprints in memory but never
// persists (used by tests that construct orchestrators without a store root).
func loadImportScanCache(storeRoot string) *importScanCache {
	c := &importScanCache{fps: map[string]scanFP{}}
	if storeRoot == "" {
		return c
	}
	c.path = filepath.Join(storeRoot, importScanCacheName)
	data, err := os.ReadFile(c.path)
	if err != nil {
		return c
	}
	var disk importScanCacheDisk
	if json.Unmarshal(data, &disk) == nil && disk.Fingerprints != nil {
		switch disk.Version {
		case importScanCacheSchemaVersion:
			c.fps = disk.Fingerprints
			return c
		case generatedRepairImportScanCacheSchemaVersion, previousImportScanCacheSchemaVersion:
			c.fps = disk.Fingerprints
			invalidateConversationProjectionFingerprints(c.fps)
			c.dirty = true
			return c
		}
		return c // unsupported version: fail open to a full rescan
	}
	// Legacy v1 was a bare path->fingerprint map. Preserve most expensive native
	// history entries, but invalidate generated Codex/Claude primary mirrors and
	// the same bounded newest-native set whose canonical projection must be
	// repaired under v1.0.39. Mark dirty so InitialScan's normal final flush
	// upgrades the file even if no candidate was present.
	var legacy map[string]scanFP
	if json.Unmarshal(data, &legacy) == nil && legacy != nil {
		c.fps = legacy
		invalidateConversationProjectionFingerprints(c.fps)
		c.dirty = true
	}
	return c
}

func invalidateConversationProjectionFingerprints(fps map[string]scanFP) {
	invalidateGeneratedConversationFingerprints(fps)
	invalidateNewestNativeCodexFingerprints(fps, nativeCodexRepairBudgetBytes, nativeCodexRepairMaxFiles)
}

func invalidateGeneratedConversationFingerprints(fps map[string]scanFP) {
	for path := range fps {
		if aplexicaGeneratedMainConversationSession(path) {
			delete(fps, path)
		}
	}
}

type nativeCodexRepairCandidate struct {
	path string
	fp   scanFP
}

// invalidateNewestNativeCodexFingerprints forces a one-time repair import for
// the newest unchanged native Codex sessions. Older releases stored Codex's
// developer harness, commentary, and tool traffic in their canonical heads;
// without invalidating the source fingerprint, a completed session would never
// revisit the source-derived sanitizer after upgrade.
//
// The migration is deliberately bounded: candidates are newest-first and their
// cumulative source size may not exceed budget, except that the single newest
// file is always selected even when it alone exceeds the budget. A file-count
// cap bounds directories full of tiny rollouts. Changed files need no special
// treatment because the ordinary scan already imports them.
func invalidateNewestNativeCodexFingerprints(fps map[string]scanFP, budget int64, maxFiles int) {
	if len(fps) == 0 || maxFiles <= 0 {
		return
	}
	candidates := make([]nativeCodexRepairCandidate, 0)
	for path, cached := range fps {
		current, ok := fingerprintPath(path)
		if !ok || current != cached || !nativeCodexConversationSession(path) {
			continue
		}
		candidates = append(candidates, nativeCodexRepairCandidate{path: path, fp: cached})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].fp.ModNano != candidates[j].fp.ModNano {
			return candidates[i].fp.ModNano > candidates[j].fp.ModNano
		}
		return candidates[i].path < candidates[j].path
	})
	var selected int
	var selectedBytes int64
	for _, candidate := range candidates {
		if selected >= maxFiles {
			break
		}
		size := candidate.fp.Size
		if size < 0 {
			continue
		}
		if selected > 0 && budget > 0 && size > budget-selectedBytes {
			break
		}
		delete(fps, candidate.path)
		selected++
		selectedBytes += size
		if selected == 1 && budget > 0 && selectedBytes > budget {
			break
		}
	}
}

func nativeCodexConversationSession(path string) bool {
	if !nativeCodexSessionPath(path) {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, scanCacheMarkerInitialBuffer), scanCacheMarkerMaxLine)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var row struct {
			Type    string `json:"type"`
			Payload struct {
				ID               string          `json:"id"`
				AplexicaThreadID string          `json:"aplexica_thread_id"`
				ThreadSource     string          `json:"thread_source"`
				Source           json.RawMessage `json:"source"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil || row.Type != "session_meta" ||
			row.Payload.ID == "" || row.Payload.AplexicaThreadID != "" ||
			row.Payload.ThreadSource == "subagent" {
			return false
		}
		var source map[string]json.RawMessage
		if json.Unmarshal(row.Payload.Source, &source) == nil {
			if _, internal := source["subagent"]; internal {
				return false
			}
		}
		return true
	}
	return false
}

func nativeCodexSessionPath(path string) bool {
	if filepath.Ext(path) != ".jsonl" || !strings.HasPrefix(filepath.Base(path), "rollout-") {
		return false
	}
	dayDir := filepath.Dir(path)
	monthDir := filepath.Dir(dayDir)
	yearDir := filepath.Dir(monthDir)
	sessionsDir := filepath.Dir(yearDir)
	codexDir := filepath.Dir(sessionsDir)
	return filepath.Base(sessionsDir) == "sessions" && filepath.Base(codexDir) == ".codex" &&
		numericPathPart(filepath.Base(yearDir), codexSessionYearWidth) &&
		numericPathPart(filepath.Base(monthDir), codexSessionMonthDayWidth) &&
		numericPathPart(filepath.Base(dayDir), codexSessionMonthDayWidth)
}

func numericPathPart(value string, width int) bool {
	if len(value) != width {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// aplexicaGeneratedMainConversationSession authenticates only deterministic
// primary native mirrors. Codex stamps both payload.id and thread id; Claude
// stamps both sessionId and thread id. Requiring those identities plus main
// excludes native conversations, forks, and unrelated JSONL histories from a
// versioned repair rescan.
func aplexicaGeneratedMainConversationSession(path string) bool {
	if filepath.Ext(path) != ".jsonl" {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		// Never follow symlinks or open device/FIFO/socket paths while migrating
		// the startup cache. Opening a FIFO here would block daemon startup
		// indefinitely before the watcher is running.
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, scanCacheMarkerInitialBuffer), scanCacheMarkerMaxLine)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var row struct {
			Type             string `json:"type"`
			AplexicaThreadID string `json:"aplexicaThreadId"`
			AplexicaBranchID string `json:"aplexicaBranchId"`
			SessionID        string `json:"sessionId"`
			Payload          struct {
				ThreadID  string `json:"aplexica_thread_id"`
				BranchID  string `json:"aplexica_branch_id"`
				SessionID string `json:"id"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			return false
		}
		codexMain := row.Type == "session_meta" && row.Payload.ThreadID != "" &&
			row.Payload.SessionID == row.Payload.ThreadID &&
			(row.Payload.BranchID == "" || row.Payload.BranchID == "main")
		claudeMain := row.AplexicaThreadID != "" && row.SessionID == row.AplexicaThreadID &&
			(row.AplexicaBranchID == "" || row.AplexicaBranchID == "main")
		return codexMain || claudeMain
	}
	return false
}

// fingerprintPath lstats path and returns its (size, mtime). ok is false for
// symlinks, non-regular files, missing, or unstattable paths.
func fingerprintPath(path string) (scanFP, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return scanFP{}, false
	}
	return scanFP{Size: fi.Size(), ModNano: fi.ModTime().UnixNano()}, true
}

// unchanged reports whether path is byte-stable since the last recorded import:
// it has a cached fingerprint AND the file's current (size, mtime) still match.
func (c *importScanCache) unchanged(path string) bool {
	if c == nil {
		return false
	}
	cur, ok := fingerprintPath(path)
	if !ok {
		return false
	}
	c.mu.Lock()
	prev, seen := c.fps[path]
	c.mu.Unlock()
	return seen && prev == cur
}

// invalidate drops ONE path's cached fingerprint, so the next pass over it
// reads as changed and runs the whole import pipeline.
//
// It exists for the diverged-import nudge, which has just been told by an
// adapter — with a real pathname — that this byte-stable file holds turns
// canonical lacks. The cache is a latency optimization built on "byte-stable
// means already consumed", and that premise is exactly what a divergence
// refutes: canonical could not learn those turns the first time, so re-reading
// the same bytes is not a no-op. Scoped to one path on purpose; a bulk
// invalidation would turn the next scan into a whole-history re-encode.
func (c *importScanCache) invalidate(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if _, seen := c.fps[path]; seen {
		delete(c.fps, path)
		c.dirty = true
	}
	c.mu.Unlock()
}

// record stores path's current fingerprint as the last-imported state. A path
// that can't be stat'd is left untracked (it reads as changed next time).
func (c *importScanCache) record(path string) {
	if c == nil {
		return
	}
	cur, ok := fingerprintPath(path)
	if !ok {
		return
	}
	c.recordFingerprint(path, cur)
}

// recordFingerprint stores a fingerprint captured by the caller at the start
// of an import attempt. Importers read append-only session files while agents
// may still be writing them, so recording a fresh stat after Import returns can
// accidentally mark bytes appended during that import as already consumed.
// Keeping the attempt fingerprint is conservative: if the importer happened
// to observe newer bytes, the next scan may perform one harmless idempotent
// re-import, but an append can never be skipped.
func (c *importScanCache) recordFingerprint(path string, fp scanFP) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.fps[path] != fp {
		c.fps[path] = fp
		c.dirty = true
	}
	c.mu.Unlock()
}

// flush atomically writes the cache to disk if it changed since the last flush.
// A nil cache, an empty backing path, or an unchanged cache is a no-op. Errors
// are returned but are non-fatal to callers: the cache only affects next-start
// latency, never correctness.
func (c *importScanCache) flush() error {
	if c == nil || c.path == "" {
		return nil
	}
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	data, err := json.Marshal(importScanCacheDisk{
		Version:      importScanCacheSchemaVersion,
		Fingerprints: c.fps,
	})
	c.dirty = false
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(c.path, data, 0o644)
}
