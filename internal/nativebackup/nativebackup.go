// Package nativebackup snapshots and restores each agent's *native* on-disk
// state — the files an agent (Claude, Codex, Hermes, …) writes for itself,
// including large session databases (e.g. ~/.hermes/state.db). It exists so a
// user can roll back to their pre-Aplexica state if cross-agent sync ever does
// something they didn't want.
//
// This is deliberately distinct from the canonical *store* bundle produced by
// `aplexica backup`: a store bundle archives Aplexica's own normalized
// artifacts, whereas a native snapshot is a byte-for-byte copy of the agents'
// own files plus a manifest of sizes and SHA-256 sums.
//
// The package is a pure library: no daemon wiring, no CLI, no HTTP. Callers
// (the daemon's first-run trigger, the CLI restore command, the web API) live
// elsewhere and supply the discovered roots and destination directories.
//
// Layout of a snapshot directory:
//
//	<destDir>/
//	  manifest.json
//	  <agentName>/<absolute-root-path-mirrored-under-agentName>/...
//
// Each agent's files are mirrored under a per-agent subdirectory keyed by the
// agent name; within that, the agent's absolute root paths are recreated so a
// single agent with multiple roots never collides with itself.
package nativebackup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/version"
)

// ManifestName is the filename of the per-snapshot manifest written into a
// snapshot directory.
const ManifestName = "manifest.json"

// SnapshotPrefix / RestorePrefix / ManualPrefix / ScheduledPrefix are the
// directory-name prefixes List enumerates.
const (
	SnapshotPrefix  = "pre-sync-"
	RestorePrefix   = "pre-restore-"
	ManualPrefix    = "manual-"
	ScheduledPrefix = "scheduled-"
)

// AgentRoots names one agent and the absolute native global root directories it
// owns on this machine (the adapter Discovery.GlobalRoots for that agent).
type AgentRoots struct {
	// Name is the agent's stable identifier (e.g. "claude", "hermes").
	Name string `json:"name"`
	// Roots are absolute directory paths to copy verbatim. Missing roots are
	// recorded as skipped rather than treated as fatal.
	Roots []string `json:"roots"`
	// ExcludePaths are absolute file or directory paths below Roots that are
	// intentionally omitted from snapshots. They hold rebuildable runtime state
	// (for example dependency trees and caches) or machine credentials that a
	// native rollback must never duplicate. Exclusions are local policy, not
	// manifest data, and therefore do not participate in restore authorization.
	ExcludePaths []string `json:"-"`
	// RedactFiles identifies mixed configuration files that contain both
	// restorable user settings and machine credentials. Snapshotting applies the
	// typed redactor to the copied bytes only; the native source is never edited.
	RedactFiles []FileRedaction `json:"-"`
}

type FileRedactionKind string

const FileRedactionOpenClawConfig FileRedactionKind = "openclaw-config"

type FileRedaction struct {
	Path string
	Kind FileRedactionKind
}

// FileEntry records one copied file's manifest line: its path relative to the
// snapshot directory, its byte size, and its SHA-256 hex digest.
type FileEntry struct {
	// Path is the file location relative to the snapshot directory root, using
	// forward slashes, so a manifest is portable and stable across platforms.
	Path string `json:"path"`
	// Bytes is the file's size in bytes.
	Bytes int64 `json:"bytes"`
	// SHA256 is the lowercase hex SHA-256 of the file's contents.
	SHA256 string `json:"sha256"`
}

// SkippedFile records a regular file that existed during the walk but could not
// be read (e.g. a permission-restricted file inside an agent root), so it was
// omitted from the backup rather than aborting the whole snapshot. A file that
// vanished mid-walk (deleted between the directory read and the open) is dropped
// silently like a missing root and is NOT recorded here.
type SkippedFile struct {
	// Path is the file location relative to the snapshot directory, using
	// forward slashes, matching FileEntry.Path's convention.
	Path string `json:"path"`
	// Reason is a human-readable explanation of why the file was skipped.
	Reason string `json:"reason"`
}

// AgentManifest groups one agent's copied files within a snapshot manifest.
type AgentManifest struct {
	Name        string      `json:"name"`
	SourceRoots []string    `json:"sourceRoots,omitempty"`
	Roots       []FileEntry `json:"roots"`
	// Skipped lists regular files that were present but unreadable and so were
	// omitted from the backup. Empty/absent when every file copied cleanly.
	Skipped []SkippedFile `json:"skipped,omitempty"`
}

// Manifest is the snapshot's manifest.json: when it was taken, the daemon
// version that took it, and the per-agent file inventory.
type Manifest struct {
	SchemaVersion   int             `json:"schemaVersion,omitempty"`
	CreatedAt       time.Time       `json:"createdAt"`
	AplexicaVersion string          `json:"aplexicaVersion"`
	Agents          []AgentManifest `json:"agents"`
	Auth            ManifestAuth    `json:"auth,omitempty"`
}

// BackupInfo summarizes one snapshot directory for listing. It is derived from
// the directory name and (when present) its manifest.
type BackupInfo struct {
	// ID is the snapshot directory's base name (e.g. "pre-sync-2026-05-29T...").
	ID string `json:"id"`
	// Path is the absolute path to the snapshot directory.
	Path string `json:"path"`
	// Kind is inferred from the directory prefix.
	Kind string `json:"kind"`
	// CreatedAt is the manifest's createdAt when readable; otherwise the
	// directory's mod time as a fallback.
	CreatedAt time.Time `json:"createdAt"`
	// Agents lists the agent names recorded in the manifest.
	Agents []string `json:"agents,omitempty"`
	// TotalBytes is the sum of all recorded file sizes in the manifest.
	TotalBytes int64 `json:"totalBytes"`
	// FileCount is the number of files recorded in the manifest.
	FileCount int `json:"fileCount"`
	// Location identifies where the restorable backup lives. Empty values from
	// old snapshots are treated by callers as "local".
	Location string `json:"location,omitempty"`
	// Encrypted is true for cloud backups. Local native snapshots are plain
	// files under the user's backup directory.
	Encrypted bool `json:"encrypted,omitempty"`
	// Algorithm names the local encryption format used before cloud upload.
	Algorithm string `json:"algorithm,omitempty"`
	// EncryptedBytes is the encrypted object size for cloud backups.
	EncryptedBytes int64 `json:"encryptedBytes,omitempty"`
	// CipherSHA256 is the SHA-256 of the encrypted object stored in cloud.
	CipherSHA256 string `json:"cipherSha256,omitempty"`
	// PlainSHA256 is the SHA-256 of the local tar.gz archive before encryption.
	PlainSHA256 string `json:"plainSha256,omitempty"`
	// OriginDeviceID / OriginDeviceName identify the device that created the
	// backup. For local records they may be empty.
	OriginDeviceID   string `json:"originDeviceId,omitempty"`
	OriginDeviceName string `json:"originDeviceName,omitempty"`
	// UploadedAt is set after a cloud upload completes.
	UploadedAt time.Time `json:"uploadedAt,omitzero"`
}

const (
	BackupJobStateRunning   = "running"
	BackupJobStateCanceling = "canceling"
	BackupJobStateSucceeded = "succeeded"
	BackupJobStateFailed    = "failed"
	BackupJobStateCanceled  = "canceled"
)

// BackupJob reports daemon-owned manual backup work. Manual backups are
// started by the web API and continue independently of the browser request that
// created them, so the portal can leave and return to the Backups page while a
// snapshot/encrypt/upload is still running.
type BackupJob struct {
	ID          string      `json:"id"`
	Kind        string      `json:"kind"`
	State       string      `json:"state"`
	Destination string      `json:"destination"`
	Agents      []string    `json:"agents,omitempty"`
	CreatedAt   time.Time   `json:"createdAt"`
	StartedAt   time.Time   `json:"startedAt,omitzero"`
	CompletedAt time.Time   `json:"completedAt,omitzero"`
	Backup      *BackupInfo `json:"backup,omitempty"`
	Error       string      `json:"error,omitempty"`
}

// SafetyStatus is the per-agent backup safety state surfaced by the local API.
type SafetyStatus struct {
	Agent         string    `json:"agent"`
	State         string    `json:"state"`
	Roots         []string  `json:"roots,omitempty"`
	RootSignature string    `json:"rootSignature,omitempty"`
	BackupID      string    `json:"backupId,omitempty"`
	LastBackupAt  time.Time `json:"lastBackupAt,omitzero"`
	LastError     string    `json:"lastError,omitempty"`
	LastFailureAt time.Time `json:"lastFailureAt,omitzero"`
	Override      bool      `json:"override"`
	OverrideAt    time.Time `json:"overrideAt,omitzero"`
	Blocked       bool      `json:"blocked"`
}

// ScheduleConfig is the persisted native-backup schedule.
type ScheduleConfig struct {
	Enabled         bool      `json:"enabled"`
	IntervalMinutes int       `json:"intervalMinutes"`
	Agents          []string  `json:"agents,omitempty"`
	Destination     string    `json:"destination,omitempty"`
	LastRunAt       time.Time `json:"lastRunAt,omitzero"`
	NextRunAt       time.Time `json:"nextRunAt,omitzero"`
}

// CloudStatus reports whether encrypted cloud backups are available to the
// local portal. The daemon keeps the decryption key local; this only describes
// whether the paired cloud plugin can accept/list/download ciphertext.
type CloudStatus struct {
	Configured bool   `json:"configured"`
	Paired     bool   `json:"paired"`
	Available  bool   `json:"available"`
	DeviceID   string `json:"deviceId,omitempty"`
	AccountID  string `json:"accountId,omitempty"`
	Message    string `json:"message,omitempty"`
}

// DefaultRetentionPerAgent is the fallback number of manual/scheduled native
// snapshots kept for each agent when no explicit per-agent setting exists.
const DefaultRetentionPerAgent = 5

// RetentionConfig controls how much user-created backup history is kept.
// Limits are per agent, not a global total. Safety pre-sync snapshots and
// pre-restore undo snapshots are not pruned by this per-agent policy; restore
// allocation bounds undo history separately via MaxPreRestoreSnapshots.
type RetentionConfig struct {
	PerAgent map[string]int `json:"perAgent,omitempty"`
}

// Status summarizes backup safety and schedule state for the portal.
type Status struct {
	Safety    []SafetyStatus  `json:"safety"`
	Schedule  ScheduleConfig  `json:"schedule"`
	Retention RetentionConfig `json:"retention"`
	Cloud     CloudStatus     `json:"cloud"`
	Jobs      []BackupJob     `json:"jobs,omitempty"`
}

// FileResult reports the outcome of restoring a single file.
type FileResult struct {
	// Path is the native (destination) absolute path written.
	Path string `json:"path"`
	// Bytes is the size copied.
	Bytes int64 `json:"bytes"`
	// OK is true when the file was copied and verified successfully.
	OK bool `json:"ok"`
	// Err is a human-readable error when OK is false; empty otherwise.
	Err string `json:"err,omitempty"`
}

// RestoreResult is the per-file report of a Restore call plus the location of
// the reversible pre-restore snapshot it took first.
type RestoreResult struct {
	// PreRestoreDir is the absolute path of the snapshot of the CURRENT native
	// state taken before any files were overwritten, so this Restore itself can
	// be reversed.
	PreRestoreDir string `json:"preRestoreDir"`
	// Files is the per-file restore outcome, in deterministic path order.
	Files []FileResult `json:"files"`
}

// filePerm / dirPerm are the modes used for restored files and created
// directories. Snapshots intentionally normalize permissions rather than
// preserving exotic source modes; native agent state is plain user data.
const (
	filePerm fs.FileMode = 0o600
	dirPerm  fs.FileMode = 0o700
)

// Snapshot copies every root of every agent into destDir, preserving each
// root's absolute structure under a per-agent subdirectory, and writes
// manifest.json describing the copied files. Missing or unreadable roots are
// skipped (not fatal); a root that exists but is a regular file is copied as a
// single file. The returned Manifest is also the one written to disk.
//
// A single unreadable file does NOT abort the snapshot: a regular file that
// vanished between the directory walk and the open is dropped (like a missing
// root), and a file that exists but cannot be read is recorded in the agent's
// Skipped list and the rest of the tree is still copied. This keeps the safety
// backup safe to run while an agent is actively churning files (FR-01.5).
//
// destDir is created if absent. Symlinks are skipped (their targets are not
// followed) to avoid copying outside the intended trees or looping.
func Snapshot(agents []AgentRoots, destDir string) (Manifest, error) {
	return snapshotContext(context.Background(), agents, destDir, false)
}

func SnapshotAuthenticated(agents []AgentRoots, destDir string) (Manifest, error) {
	return snapshotContext(context.Background(), agents, destDir, true)
}

// SnapshotContext is Snapshot with cancellation support. Callers should pass
// the request context for interactive backups so Cancel stops long copies.
func SnapshotContext(ctx context.Context, agents []AgentRoots, destDir string) (Manifest, error) {
	return snapshotContext(ctx, agents, destDir, false)
}

func SnapshotContextAuthenticated(ctx context.Context, agents []AgentRoots, destDir string) (Manifest, error) {
	return snapshotContext(ctx, agents, destDir, true)
}

func snapshotContext(ctx context.Context, agents []AgentRoots, destDir string, authenticate bool) (Manifest, error) {
	if destDir == "" {
		return Manifest{}, fmt.Errorf("nativebackup: empty destDir")
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	validatedExcludes := make([][]string, len(agents))
	validatedRedactions := make([]map[string]FileRedactionKind, len(agents))
	for i, ag := range agents {
		var err error
		validatedExcludes[i], err = validateExcludePaths(ag.Roots, ag.ExcludePaths)
		if err != nil {
			return Manifest{}, fmt.Errorf("nativebackup: snapshot agent %q exclusions: %w", ag.Name, err)
		}
		validatedRedactions[i], err = validateRedactionPaths(ag.Roots, ag.RedactFiles)
		if err != nil {
			return Manifest{}, fmt.Errorf("nativebackup: snapshot agent %q redactions: %w", ag.Name, err)
		}
	}
	if err := os.MkdirAll(destDir, dirPerm); err != nil {
		return Manifest{}, fmt.Errorf("nativebackup: create dest %s: %w", destDir, err)
	}

	man := Manifest{
		SchemaVersion:   0,
		CreatedAt:       time.Now().UTC(),
		AplexicaVersion: version.Version,
		Agents:          make([]AgentManifest, 0, len(agents)),
	}

	for agentIndex, ag := range agents {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		am := AgentManifest{Name: ag.Name, Roots: []FileEntry{}}
		// Mirror under <destDir>/<agentName>/<absolute-root>. filepath.Join
		// strips a leading separator, so an absolute root like /Users/x/.claude
		// becomes <destDir>/<agentName>/Users/x/.claude.
		agentDest := filepath.Join(destDir, ag.Name)
		for _, root := range ag.Roots {
			canonicalRoot, cerr := filepath.Abs(root)
			if cerr != nil {
				return Manifest{}, cerr
			}
			am.SourceRoots = append(am.SourceRoots, filepath.Clean(canonicalRoot))
			entries, skipped, err := copyTree(ctx, canonicalRoot, filepath.Join(agentDest, relativize(canonicalRoot)), destDir, validatedExcludes[agentIndex], validatedRedactions[agentIndex], true)
			if err != nil {
				return Manifest{}, fmt.Errorf("nativebackup: snapshot agent %q root %q: %w", ag.Name, root, err)
			}
			am.Roots = append(am.Roots, entries...)
			am.Skipped = append(am.Skipped, skipped...)
		}
		sort.Slice(am.Roots, func(i, j int) bool { return am.Roots[i].Path < am.Roots[j].Path })
		sort.Strings(am.SourceRoots)
		sort.Slice(am.Skipped, func(i, j int) bool { return am.Skipped[i].Path < am.Skipped[j].Path })
		man.Agents = append(man.Agents, am)
	}
	if authenticate {
		man.SchemaVersion = 2
		if err := SignDefaultManifest(&man, destDir); err != nil {
			return Manifest{}, err
		}
	}

	if err := writeManifest(destDir, man); err != nil {
		return Manifest{}, err
	}
	return man, nil
}

// validateExcludePaths canonicalizes a snapshot policy and proves that every
// excluded path is contained by at least one declared root. Keeping this check
// separate from traversal prevents a typo from silently suppressing unrelated
// data and makes path-component boundaries explicit through filepath.Rel.
func validateExcludePaths(roots, exclusions []string) ([]string, error) {
	if len(exclusions) == 0 {
		return nil, nil
	}
	canonicalRoots := make([]string, 0, len(roots))
	for _, root := range roots {
		canonical, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("resolve root %q: %w", root, err)
		}
		canonicalRoots = append(canonicalRoots, filepath.Clean(canonical))
	}
	seen := make(map[string]struct{}, len(exclusions))
	out := make([]string, 0, len(exclusions))
	for _, excluded := range exclusions {
		if excluded == "" || !filepath.IsAbs(excluded) {
			return nil, fmt.Errorf("exclude path %q is not absolute", excluded)
		}
		clean := filepath.Clean(excluded)
		contained := false
		for _, root := range canonicalRoots {
			if pathWithinOrEqual(clean, root) {
				contained = true
				break
			}
		}
		if !contained {
			return nil, fmt.Errorf("exclude path %q is outside declared roots", excluded)
		}
		if _, duplicate := seen[clean]; duplicate {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	sort.Strings(out)
	return out, nil
}

func validateRedactionPaths(roots []string, policies []FileRedaction) (map[string]FileRedactionKind, error) {
	if len(policies) == 0 {
		return nil, nil
	}
	canonicalRoots := make([]string, 0, len(roots))
	for _, root := range roots {
		canonical, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("resolve root %q: %w", root, err)
		}
		canonicalRoots = append(canonicalRoots, filepath.Clean(canonical))
	}
	out := make(map[string]FileRedactionKind, len(policies))
	for _, policy := range policies {
		if policy.Path == "" || !filepath.IsAbs(policy.Path) {
			return nil, fmt.Errorf("redaction path %q is not absolute", policy.Path)
		}
		if !validFileRedactionKind(policy.Kind) {
			return nil, fmt.Errorf("redaction path %q has unsupported kind %q", policy.Path, policy.Kind)
		}
		clean := filepath.Clean(policy.Path)
		contained := false
		for _, root := range canonicalRoots {
			if pathWithinOrEqual(clean, root) {
				contained = true
				break
			}
		}
		if !contained {
			return nil, fmt.Errorf("redaction path %q is outside declared roots", policy.Path)
		}
		key := redactionPathKey(clean)
		if existing, duplicate := out[key]; duplicate && existing != policy.Kind {
			return nil, fmt.Errorf("redaction path %q has conflicting kinds", policy.Path)
		}
		out[key] = policy.Kind
	}
	return out, nil
}

func redactionPathKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func pathWithinOrEqual(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func pathExcluded(path string, exclusions []string) bool {
	clean := filepath.Clean(path)
	for _, excluded := range exclusions {
		if pathWithinOrEqual(clean, excluded) {
			return true
		}
	}
	return false
}

// genericRebuildablePath reports conventional generated dependency/runtime
// names. Matching the complete basename (rather than a prefix or substring)
// keeps similarly named user data such as node_modules_notes or myvenv.
// Git metadata is intentionally not generic: unpublished local history can be
// user data and only explicitly identified installer repositories may omit it.
func genericRebuildablePath(base string) bool {
	switch base {
	case "node_modules", ".venv", "venv", "__pycache__", ".DS_Store":
		return true
	default:
		return false
	}
}

// relativize turns an absolute path into a relative one suitable for nesting
// under a destination directory, by stripping the leading volume/separator. On
// a path like "/Users/x/.claude" it returns "Users/x/.claude".
func relativize(p string) string {
	p = filepath.Clean(p)
	vol := filepath.VolumeName(p)
	p = strings.TrimPrefix(p, vol)
	return strings.TrimPrefix(p, string(filepath.Separator))
}

// copyTree copies src (a file or directory) to dst, recursing into
// directories, and returns one FileEntry per copied regular file. Entry paths
// are relative to manifestRoot (the snapshot directory) using forward slashes.
// A missing src is not an error: it yields no entries. Symlinks are skipped.
//
// The second return value lists regular files that were present but could not
// be read; such a file is skipped (not fatal) so one EACCES/transient file
// never aborts the whole snapshot.
func copyTree(ctx context.Context, src, dst, manifestRoot string, exclusions []string, redactions map[string]FileRedactionKind, rootEntry bool) ([]FileEntry, []SkippedFile, error) {
	// Restore transaction objects are implementation state, never user data.
	// Watchers and pre-restore snapshots must not observe or preserve them.
	base := filepath.Base(src)
	if strings.HasPrefix(base, ".aplexica-restore-verified-") || strings.HasPrefix(base, ".aplexica-restore-rollback-") {
		return nil, nil, nil
	}
	if pathExcluded(src, exclusions) || (!rootEntry && genericRebuildablePath(base)) {
		return nil, nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, nil // skip symlinks
	}

	// Skip non-regular, non-directory files (sockets, FIFOs, device nodes).
	// Opening them either fails (a unix socket → "operation not supported on
	// socket"; e.g. git's .git/fsmonitor--daemon.ipc) or blocks (a FIFO with
	// no writer), and either way previously aborted the entire agent snapshot.
	// They carry no agent state worth backing up.
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, nil, nil
	}

	if !info.IsDir() {
		if kind, redact := redactions[redactionPathKey(src)]; redact {
			entry, skipped, err := copyRedactedFileContext(ctx, src, dst, manifestRoot, kind)
			if err != nil {
				return nil, nil, err
			}
			if skipped != nil {
				return nil, []SkippedFile{*skipped}, nil
			}
			return []FileEntry{entry}, nil, nil
		}
		entry, skipped, err := copyFileContext(ctx, src, dst, manifestRoot)
		if err != nil {
			return nil, nil, err
		}
		if skipped != nil {
			return nil, []SkippedFile{*skipped}, nil
		}
		if entry == (FileEntry{}) {
			return nil, nil, nil // vanished mid-walk: dropped like a missing root
		}
		return []FileEntry{entry}, nil, nil
	}
	if _, redact := redactions[redactionPathKey(src)]; redact {
		return nil, nil, fmt.Errorf("nativebackup: redaction target %q is a directory", src)
	}

	// Retain the declared native root once and perform every descendant lookup
	// through it. A directory component that is renamed or replaced while the
	// snapshot runs can therefore neither redirect traversal outside the root nor
	// turn later child opens back into ambient-path operations.
	sourceRoot, err := privatefs.OpenNativeRoot(src, privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("retain native root %s: %w", src, err)
	}
	defer sourceRoot.Close()
	retainedInfo, err := sourceRoot.StatRoot()
	if err != nil {
		return nil, nil, fmt.Errorf("inspect retained native root %s: %w", src, err)
	}
	if !info.IsDir() || !retainedInfo.IsDir() || !os.SameFile(info, retainedInfo) {
		return nil, nil, fmt.Errorf("native root %s changed identity while opening", src)
	}
	return copyRetainedTree(ctx, sourceRoot, ".", src, dst, manifestRoot, exclusions, redactions, true)
}

// copyRetainedTree traverses a directory already anchored by OpenNativeRoot.
// src is retained only for policy matching and diagnostics; no source content
// is opened through that ambient path.
func copyRetainedTree(ctx context.Context, sourceRoot *privatefs.Root, rel, src, dst, manifestRoot string, exclusions []string, redactions map[string]FileRedactionKind, rootEntry bool) ([]FileEntry, []SkippedFile, error) {
	base := filepath.Base(src)
	if strings.HasPrefix(base, ".aplexica-restore-verified-") || strings.HasPrefix(base, ".aplexica-restore-rollback-") {
		return nil, nil, nil
	}
	if pathExcluded(src, exclusions) || (!rootEntry && genericRebuildablePath(base)) {
		return nil, nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if _, redact := redactions[redactionPathKey(src)]; redact {
		return nil, nil, fmt.Errorf("nativebackup: redaction target %q is a directory", src)
	}
	if err := os.MkdirAll(dst, dirPerm); err != nil {
		return nil, nil, fmt.Errorf("mkdir %s: %w", dst, err)
	}
	dirEntries, err := sourceRoot.ReadDir(rel)
	if err != nil {
		return nil, nil, fmt.Errorf("read retained dir %s: %w", src, err)
	}
	var out []FileEntry
	var skips []SkippedFile
	for _, de := range dirEntries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		childSrc := filepath.Join(src, de.Name())
		if pathExcluded(childSrc, exclusions) || genericRebuildablePath(de.Name()) ||
			strings.HasPrefix(de.Name(), ".aplexica-restore-verified-") ||
			strings.HasPrefix(de.Name(), ".aplexica-restore-rollback-") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, fmt.Errorf("inspect retained child %s: %w", childSrc, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			continue
		}
		childRel := de.Name()
		if rel != "." {
			childRel = filepath.Join(rel, de.Name())
		}
		childDst := filepath.Join(dst, de.Name())
		var childEntries []FileEntry
		var childSkips []SkippedFile
		if info.IsDir() {
			childEntries, childSkips, err = copyRetainedTree(ctx, sourceRoot, childRel, childSrc, childDst, manifestRoot, exclusions, redactions, false)
		} else if kind, redact := redactions[redactionPathKey(childSrc)]; redact {
			// Redaction has its own bounded, no-follow identity checks and reads
			// through the same retained root as ordinary files. It remains separate
			// because it must read the complete source before transforming it.
			var entry FileEntry
			var skipped *SkippedFile
			entry, skipped, err = copyRetainedRedactedFileContext(ctx, sourceRoot, childRel, info, childSrc, childDst, manifestRoot, kind)
			if skipped != nil {
				childSkips = []SkippedFile{*skipped}
			} else if entry != (FileEntry{}) {
				childEntries = []FileEntry{entry}
			}
		} else {
			var entry FileEntry
			var skipped *SkippedFile
			entry, skipped, err = copyRetainedFileContext(ctx, sourceRoot, childRel, info, childSrc, childDst, manifestRoot)
			if skipped != nil {
				childSkips = []SkippedFile{*skipped}
			} else if entry != (FileEntry{}) {
				childEntries = []FileEntry{entry}
			}
		}
		if err != nil {
			return nil, nil, err
		}
		out = append(out, childEntries...)
		skips = append(skips, childSkips...)
	}
	return out, skips, nil
}

// copyFile copies a single regular file from src to dst, hashing the bytes as
// they are written, and returns the manifest entry. The parent of dst is
// created if needed.
//
// A failure to READ the source is not fatal — it never aborts the snapshot:
//   - if src vanished between the directory walk and the open (os.IsNotExist),
//     copyFile returns the zero FileEntry and a nil SkippedFile, so the caller
//     drops it silently like a missing root;
//   - if src exists but cannot be opened or read (e.g. EACCES on a
//     permission-restricted file), copyFile returns a non-nil SkippedFile with
//     a reason and the caller records it and continues.
//
// Destination-side failures (creating the parent dir, the temp file, fsync,
// rename, …) DO return a fatal error: they mean the backup target itself is
// broken, not that one source file is flaky.
func copyFile(src, dst, manifestRoot string) (FileEntry, *SkippedFile, error) {
	return copyFileContext(context.Background(), src, dst, manifestRoot)
}

func copyFileContext(ctx context.Context, src, dst, manifestRoot string) (FileEntry, *SkippedFile, error) {
	if err := ctx.Err(); err != nil {
		return FileEntry{}, nil, err
	}
	pathBefore, err := os.Lstat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return FileEntry{}, nil, nil
		}
		return FileEntry{}, skipReason(src, dst, manifestRoot, fmt.Sprintf("inspect: %v", err)), nil
	}
	if !pathBefore.Mode().IsRegular() || pathBefore.Mode()&fs.ModeSymlink != 0 {
		return FileEntry{}, skipReason(src, dst, manifestRoot, "unsafe non-regular source"), nil
	}
	if hooks := snapshotCopyHooksFromContext(ctx); hooks != nil && hooks.afterSourceInfo != nil {
		hooks.afterSourceInfo(src)
	}
	sourceRoot, err := privatefs.OpenNativeRoot(filepath.Dir(src), privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly})
	if err != nil {
		if os.IsNotExist(err) {
			return FileEntry{}, nil, nil
		}
		return FileEntry{}, skipReason(src, dst, manifestRoot, fmt.Sprintf("open source parent: %v", err)), nil
	}
	defer sourceRoot.Close()
	return copyRetainedFileContextAfterInfo(ctx, sourceRoot, filepath.Base(src), pathBefore, src, dst, manifestRoot)
}

func copyRetainedFileContext(ctx context.Context, sourceRoot *privatefs.Root, rel string, pathBefore os.FileInfo, src, dst, manifestRoot string) (FileEntry, *SkippedFile, error) {
	if hooks := snapshotCopyHooksFromContext(ctx); hooks != nil && hooks.afterSourceInfo != nil {
		hooks.afterSourceInfo(src)
	}
	return copyRetainedFileContextAfterInfo(ctx, sourceRoot, rel, pathBefore, src, dst, manifestRoot)
}

func copyRetainedFileContextAfterInfo(ctx context.Context, sourceRoot *privatefs.Root, rel string, pathBefore os.FileInfo, src, dst, manifestRoot string) (FileEntry, *SkippedFile, error) {
	if err := ctx.Err(); err != nil {
		return FileEntry{}, nil, err
	}
	limiter, _ := ctx.Value(throughputLimitContextKey{}).(*throughputLimiter)
	if err := limiter.waitFile(ctx); err != nil {
		return FileEntry{}, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return FileEntry{}, nil, fmt.Errorf("mkdir parent of %s: %w", dst, err)
	}
	in, err := sourceRoot.OpenReadRegularIntegrity(rel)
	if err != nil {
		if os.IsNotExist(err) {
			// Vanished mid-walk (TOCTOU under an active agent): drop it like a
			// missing root rather than failing the whole snapshot.
			return FileEntry{}, nil, nil
		}
		return FileEntry{}, skipReason(src, dst, manifestRoot, fmt.Sprintf("open: %v", err)), nil
	}
	defer in.Close()
	openedInfo, err := in.Stat()
	if err != nil || !sameStableSnapshotFile(pathBefore, openedInfo) {
		return FileEntry{}, skipReason(src, dst, manifestRoot, "source changed identity, type, size, or modification time while opening"), nil
	}
	if hooks := snapshotCopyHooksFromContext(ctx); hooks != nil && hooks.afterSourceOpen != nil {
		hooks.afterSourceOpen(src, in)
	}

	// Atomic write: stream into a sibling temp file, fsync, then rename over
	// dst. A partial copy (disk full, I/O error, or SIGKILL mid-write) never
	// truncates the live destination — the half-written temp is discarded. On
	// restore, dst is a LIVE native file, so this is what keeps a failed/
	// interrupted restore from leaving it inconsistent (NFR-01.4 / FR-01.16).
	out, err := os.CreateTemp(filepath.Dir(dst), ".nativebackup-*.tmp")
	if err != nil {
		return FileEntry{}, nil, fmt.Errorf("create temp for %s: %w", dst, err)
	}
	tmpName := out.Name()
	committed := false
	defer func() {
		if !committed {
			_ = out.Close()
			_ = os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	n, err := copyWithContext(ctx, io.MultiWriter(out, h), in)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return FileEntry{}, nil, err
		}
		// A read error mid-copy (e.g. the source went away or became
		// unreadable) is treated as a per-file skip, not a fatal abort; the
		// half-written temp is cleaned up by the deferred rollback above.
		return FileEntry{}, skipReason(src, dst, manifestRoot, fmt.Sprintf("read: %v", err)), nil
	}
	afterInfo, statErr := in.Stat()
	if statErr != nil || !sameStableSnapshotFile(openedInfo, afterInfo) || n != openedInfo.Size() {
		return FileEntry{}, skipReason(src, dst, manifestRoot, "source changed identity, type, size, or modification time while reading"), nil
	}
	if limiter == nil || !limiter.skipFileSync {
		if err := out.Sync(); err != nil {
			return FileEntry{}, nil, fmt.Errorf("fsync %s: %w", dst, err)
		}
	}
	if err := out.Close(); err != nil {
		return FileEntry{}, nil, fmt.Errorf("close temp for %s: %w", dst, err)
	}
	// CreateTemp makes the file 0600; restore the intended mode before commit.
	if err := os.Chmod(tmpName, filePerm); err != nil {
		return FileEntry{}, nil, fmt.Errorf("chmod temp for %s: %w", dst, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return FileEntry{}, nil, fmt.Errorf("rename temp -> %s: %w", dst, err)
	}
	committed = true

	manifestRel, err := filepath.Rel(manifestRoot, dst)
	if err != nil {
		return FileEntry{}, nil, fmt.Errorf("relativize %s under %s: %w", dst, manifestRoot, err)
	}
	return FileEntry{
		Path:   filepath.ToSlash(manifestRel),
		Bytes:  n,
		SHA256: hex.EncodeToString(h.Sum(nil)),
	}, nil, nil
}

// sameStableSnapshotFile authenticates the exact regular-file observation a
// snapshot is about to copy. SameFile covers the platform inode/file-index;
// explicit type, size, and modification-time checks reject replacement and
// ordinary active-writer races before a partial/mixed copy can be committed.
func sameStableSnapshotFile(before, after os.FileInfo) bool {
	return before != nil && after != nil &&
		before.Mode().IsRegular() && after.Mode().IsRegular() &&
		os.SameFile(before, after) && before.Size() == after.Size() &&
		before.ModTime().Equal(after.ModTime())
}

type snapshotCopyHooksContextKey struct{}

// snapshotCopyHooks provides deterministic race points to package tests. It is
// carried by context (rather than a mutable package global) so parallel and
// -race runs cannot influence unrelated snapshots.
type snapshotCopyHooks struct {
	afterSourceInfo func(path string)
	afterSourceOpen func(path string, opened *os.File)
}

func snapshotCopyHooksFromContext(ctx context.Context) *snapshotCopyHooks {
	hooks, _ := ctx.Value(snapshotCopyHooksContextKey{}).(*snapshotCopyHooks)
	return hooks
}

// copyBufSize is the io buffer for copyWithContext: large enough to keep
// snapshot copies fast, small enough that a cancel lands within one chunk.
const copyBufSize = 128 * 1024

// DefaultScheduledThroughputBytesPerSecond is the background native-backup
// budget. Scheduled snapshots routinely cover multi-gigabyte agent histories;
// without pacing, their copy+hash+compression work monopolizes one or more CPU
// cores on every paired device. 1 MiB/s keeps the twice-daily background job
// deliberately unobtrusive even on slower Macs while still completing a large
// backup inside its 12-hour interval. Manual and safety backups remain
// unthrottled because they are explicit, foreground user actions.
const DefaultScheduledThroughputBytesPerSecond int64 = 1 << 20

// DefaultScheduledFilesPerSecond bounds metadata-heavy scheduled snapshots.
// A byte-only limit is insufficient for histories containing thousands of
// small JSONL files: open, chmod, rename, and durability calls can otherwise
// arrive in short CPU/I/O bursts even while copied bytes remain below budget.
const DefaultScheduledFilesPerSecond int64 = 20

type throughputLimitContextKey struct{}

type throughputLimiter struct {
	mu           sync.Mutex
	started      time.Time
	bytes        int64
	files        int64
	rate         int64
	fileRate     int64
	skipFileSync bool
}

// WithThroughputLimit attaches a shared byte-rate budget to ctx. Every
// copyWithContext call using the derived context participates in the same
// budget, so a scheduled cloud backup is paced across both its staging copy
// and archive compression rather than bursting anew for each file.
func WithThroughputLimit(ctx context.Context, bytesPerSecond int64) context.Context {
	if bytesPerSecond <= 0 {
		return ctx
	}
	return context.WithValue(ctx, throughputLimitContextKey{}, &throughputLimiter{
		started: time.Now(),
		rate:    bytesPerSecond,
	})
}

// WithScheduledBackgroundBudget applies both byte and file-operation pacing to
// disposable scheduled-backup staging. Per-file fsync is omitted because the
// staging tree is never accepted as a backup: it is immediately read into an
// authenticated archive, and any crash or failure removes/sweeps the staging
// directory. Manual, safety, and restore snapshots do not use this context and
// retain their per-file durability barrier.
func WithScheduledBackgroundBudget(ctx context.Context, bytesPerSecond, filesPerSecond int64) context.Context {
	if bytesPerSecond <= 0 && filesPerSecond <= 0 {
		return ctx
	}
	return context.WithValue(ctx, throughputLimitContextKey{}, &throughputLimiter{
		started:      time.Now(),
		rate:         bytesPerSecond,
		fileRate:     filesPerSecond,
		skipFileSync: true,
	})
}

func (l *throughputLimiter) wait(ctx context.Context, n int) error {
	if l == nil || n <= 0 || l.rate <= 0 {
		return nil
	}
	l.mu.Lock()
	l.bytes += int64(n)
	due := l.started.Add(time.Duration(float64(l.bytes) / float64(l.rate) * float64(time.Second)))
	delay := time.Until(due)
	l.mu.Unlock()
	if delay <= 0 {
		return nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (l *throughputLimiter) waitFile(ctx context.Context) error {
	if l == nil || l.fileRate <= 0 {
		return nil
	}
	l.mu.Lock()
	l.files++
	due := l.started.Add(time.Duration(float64(l.files) / float64(l.fileRate) * float64(time.Second)))
	delay := time.Until(due)
	l.mu.Unlock()
	if delay <= 0 {
		return nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	limiter, _ := ctx.Value(throughputLimitContextKey{}).(*throughputLimiter)
	buf := make([]byte, copyBufSize)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		nr, er := src.Read(buf)
		if nr > 0 {
			if err := ctx.Err(); err != nil {
				return written, err
			}
			nw, ew := dst.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				return written, ew
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
			if err := limiter.wait(ctx, nw); err != nil {
				return written, err
			}
		}
		if er == io.EOF {
			return written, nil
		}
		if er != nil {
			return written, er
		}
	}
}

// skipReason builds a SkippedFile for a source file that could not be read. The
// recorded Path mirrors FileEntry.Path (relative to manifestRoot, forward
// slashes); if it cannot be relativized, the destination path is used verbatim.
func skipReason(src, dst, manifestRoot, reason string) *SkippedFile {
	path := dst
	if rel, err := filepath.Rel(manifestRoot, dst); err == nil {
		path = filepath.ToSlash(rel)
	}
	return &SkippedFile{Path: path, Reason: fmt.Sprintf("%s: %s", src, reason)}
}

// writeManifest serializes man to <destDir>/manifest.json with indentation.
func writeManifest(destDir string, man Manifest) error {
	data, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return fmt.Errorf("nativebackup: marshal manifest: %w", err)
	}
	path := filepath.Join(destDir, ManifestName)
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("nativebackup: write manifest %s: %w", path, err)
	}
	return nil
}

// ReadManifest loads the manifest.json from a snapshot directory.
func ReadManifest(backupDir string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(backupDir, ManifestName))
	if err != nil {
		return Manifest{}, fmt.Errorf("nativebackup: read manifest in %s: %w", backupDir, err)
	}
	var man Manifest
	if err := json.Unmarshal(data, &man); err != nil {
		return Manifest{}, fmt.Errorf("nativebackup: parse manifest in %s: %w", backupDir, err)
	}
	return man, nil
}

// List enumerates snapshot directories under backupsRoot, newest first. A
// missing backupsRoot yields an empty slice, not an error. Directories without a
// readable manifest are still listed (with a mod-time fallback CreatedAt) so
// partial/old snapshots remain visible.
func List(backupsRoot string) ([]BackupInfo, error) {
	entries, err := os.ReadDir(backupsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return []BackupInfo{}, nil
		}
		return nil, fmt.Errorf("nativebackup: read backups root %s: %w", backupsRoot, err)
	}

	out := make([]BackupInfo, 0, len(entries))
	for _, de := range entries {
		if !de.IsDir() {
			continue
		}
		name := de.Name()
		kind, ok := SnapshotKindFromID(name)
		if !ok {
			continue
		}

		dir := filepath.Join(backupsRoot, name)
		info := BackupInfo{ID: name, Path: dir, Kind: kind, Location: "local"}
		if man, err := ReadManifest(dir); err == nil {
			info.CreatedAt = man.CreatedAt
			for _, ag := range man.Agents {
				info.Agents = append(info.Agents, ag.Name)
				for _, fe := range ag.Roots {
					info.TotalBytes += fe.Bytes
					info.FileCount++
				}
			}
		} else if fi, statErr := de.Info(); statErr == nil {
			info.CreatedAt = fi.ModTime().UTC()
		}
		out = append(out, info)
	}

	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// SnapshotKindFromID returns the user-facing kind for a snapshot directory ID.
func SnapshotKindFromID(id string) (string, bool) {
	switch {
	case strings.HasPrefix(id, SnapshotPrefix):
		return "pre-sync", true
	case strings.HasPrefix(id, RestorePrefix):
		return "pre-restore", true
	case strings.HasPrefix(id, ManualPrefix):
		return "manual", true
	case strings.HasPrefix(id, ScheduledPrefix):
		return "scheduled", true
	default:
		return "", false
	}
}

// Restore copies a snapshot's files back over the live native roots. It is
// REVERSIBLE: before overwriting anything it snapshots the CURRENT native state
// of the same files into a sibling pre-restore-<RFC3339> directory under the
// backup's parent, so this Restore can itself be undone. When agent is "" all
// agents in the manifest are restored; otherwise only the named agent.
//
// The per-file outcome is returned in RestoreResult.Files (deterministic path
// order). A copy/verify failure on one file is recorded in that file's result
// and does not abort the remaining files; only setup failures (unreadable
// manifest, inability to create the pre-restore dir) return a non-nil error.
func Restore(backupDir, agent string) (RestoreResult, error) {
	man, err := ReadManifest(backupDir)
	if err != nil {
		return RestoreResult{}, err
	}

	// Reconstruct the native destination for each manifest entry and gather the
	// set of (backup-copy, native-target) pairs to restore.
	type job struct {
		backupCopy   string // absolute path of the file inside backupDir
		nativeTarget string // absolute native path to overwrite
		want         FileEntry
		verifiedTemp string
	}
	var jobs []job
	// refused collects manifest entries whose reconstructed native target would
	// escape the agent's mirrored root (path traversal via a malicious/corrupt
	// manifest). They are excluded from the jobs and the pre-restore snapshot and
	// surfaced as per-file failures, never written.
	var refused []FileResult
	for _, ag := range man.Agents {
		if agent != "" && ag.Name != agent {
			continue
		}
		for _, fe := range ag.Roots {
			nativeTarget, ok := nativeTargetForSafe(ag.Name, fe.Path)
			if !ok {
				refused = append(refused, FileResult{
					Path: fe.Path,
					Err:  fmt.Sprintf("refused: manifest path %q escapes agent root %q", fe.Path, ag.Name),
				})
				continue
			}
			backupCopy := filepath.Join(backupDir, filepath.FromSlash(fe.Path))
			jobs = append(jobs, job{
				backupCopy:   backupCopy,
				nativeTarget: nativeTarget,
				want:         fe,
			})
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].nativeTarget < jobs[j].nativeTarget })
	for i := range jobs {
		temp, err := stageRestoreFile(jobs[i].backupCopy, jobs[i].nativeTarget, jobs[i].want)
		if err != nil {
			for j := 0; j < i; j++ {
				_ = os.Remove(jobs[j].verifiedTemp)
			}
			return RestoreResult{}, fmt.Errorf("nativebackup: validate restore source: %w", err)
		}
		jobs[i].verifiedTemp = temp
	}
	defer func() {
		for _, j := range jobs {
			_ = os.Remove(j.verifiedTemp)
		}
	}()

	// 1) Snapshot the CURRENT native state of exactly these files so the
	// restore is reversible. We build the AgentRoots from the native targets.
	// The directory name must be unique even for two restores in the same
	// second (otherwise the second would overwrite the first's snapshot), so
	// uniquePreRestoreDir adds a counter/random suffix on collision.
	backupsRoot := filepath.Dir(filepath.Clean(backupDir))
	if _, err := PrunePreRestoreHistory(context.Background(), backupsRoot, MaxPreRestoreSnapshots-1, backupDir); err != nil {
		return RestoreResult{}, fmt.Errorf("nativebackup: prune pre-restore history: %w", err)
	}
	preRestoreDir, err := uniquePreRestoreDir(backupsRoot)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("nativebackup: allocate pre-restore dir: %w", err)
	}
	preAgents := currentNativeAgents(man, agent)
	if _, err := Snapshot(preAgents, preRestoreDir); err != nil {
		// No live file has been renamed yet, so a failed snapshot has no
		// recovery value and must not become another permanent full copy.
		_ = os.RemoveAll(preRestoreDir)
		return RestoreResult{}, fmt.Errorf("nativebackup: pre-restore snapshot: %w", err)
	}

	res := RestoreResult{PreRestoreDir: preRestoreDir, Files: make([]FileResult, 0, len(jobs)+len(refused))}
	res.Files = append(res.Files, refused...)

	// 2) Copy the backup's files back over the native roots, verifying each
	// against the manifest's recorded SHA-256.
	for _, j := range jobs {
		fr := FileResult{Path: j.nativeTarget}
		if err := os.Rename(j.verifiedTemp, j.nativeTarget); err != nil {
			fr.Err = err.Error()
			res.Files = append(res.Files, fr)
			continue
		}
		fr.Bytes = j.want.Bytes
		fr.OK = true
		res.Files = append(res.Files, fr)
	}

	return res, nil
}

// restoreFileVerified stages and authenticates the complete backup copy before
// the live target is renamed. A missing, oversized, short, or digest-mismatched
// source leaves the existing native file byte-identical.
func stageRestoreFile(src, dst string, want FileEntry) (string, error) {
	if want.Bytes < 0 || want.SHA256 == "" {
		return "", fmt.Errorf("nativebackup: invalid manifest digest")
	}
	info, err := os.Lstat(src)
	if err != nil {
		return "", err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("nativebackup: backup source is not a regular file")
	}
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", fmt.Errorf("nativebackup: backup source identity changed")
	}
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return "", err
	}
	out, err := os.CreateTemp(filepath.Dir(dst), ".aplexica-restore-verified-")
	if err != nil {
		return "", err
	}
	temp := out.Name()
	valid := false
	defer func() {
		if !valid {
			_ = out.Close()
			_ = os.Remove(temp)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, want.Bytes+1))
	if err == nil && n != want.Bytes {
		err = fmt.Errorf("nativebackup: source size mismatch")
	}
	got := hex.EncodeToString(h.Sum(nil))
	if err == nil && got != want.SHA256 {
		err = fmt.Errorf("nativebackup: source digest mismatch")
	}
	if err == nil {
		err = out.Sync()
	}
	if ce := out.Close(); err == nil {
		err = ce
	}
	if err != nil {
		return "", err
	}
	if err = os.Chmod(temp, filePerm); err != nil {
		return "", err
	}
	valid = true
	return temp, nil
}

// nativeTargetForSafe reconstructs the absolute native path of a manifest entry
// and validates it cannot escape the agent's mirrored root. Snapshot stores
// files at <destDir>/<agentName>/<relativized-abs-path>, so a well-formed
// manifest Path is "<agentName>/<relativized-abs-path>"; stripping the agent
// segment and re-rooting at the filesystem root recovers the original.
//
// Because the manifest is attacker-influenced input on the restore path (a
// corrupt or malicious snapshot directory), the reconstruction is hardened: an
// entry whose Path does not begin with "<agentName>/", or whose relativized
// tail contains a "../" component that would climb out of the mirror and write
// outside the intended native tree, is REFUSED (ok=false) rather than written.
// Callers treat a refusal as a per-file failure, matching the package's
// skip-not-abort philosophy, so one poisoned entry never derails the rest.
func nativeTargetForSafe(agentName, manifestPath string) (string, bool) {
	prefix := agentName + "/"
	if !strings.HasPrefix(manifestPath, prefix) {
		// Not a "<agentName>/..." mirror path: an absolute-looking or otherwise
		// malformed entry that must not be silently re-rooted.
		return "", false
	}
	rel := filepath.FromSlash(strings.TrimPrefix(manifestPath, prefix))
	// Clean the RELATIVE tail before re-rooting. This is the load-bearing step:
	// re-rooting first and cleaning the absolute path would let filepath.Clean
	// silently absorb leading "../" against the root (e.g. "/../etc" -> "/etc"),
	// hiding the escape. Cleaning the relative form instead surfaces a climb as
	// a leading ".." (or "..") and an absolute tail as a rooted path — either of
	// which means the entry left the mirror, so reject it.
	cleanRel := filepath.Clean(rel)
	if cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if filepath.IsAbs(cleanRel) {
		return "", false
	}
	return string(filepath.Separator) + cleanRel, true
}

// uniquePreRestoreDir returns a not-yet-existing pre-restore-<ts> directory
// path under backupsRoot. The base name is the RFC3339-ish UTC timestamp to the
// second; if that already exists (two restores in the same second), a short
// random suffix is appended until a free name is found. The directory itself is
// not created here — Snapshot's MkdirAll does that.
func uniquePreRestoreDir(backupsRoot string) (string, error) {
	base := RestorePrefix + time.Now().UTC().Format("2006-01-02T15-04-05Z")
	candidate := filepath.Join(backupsRoot, base)
	if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
		return candidate, nil
	}
	for i := 0; i < 100; i++ {
		suffix := make([]byte, 4)
		if _, err := rand.Read(suffix); err != nil {
			return "", err
		}
		candidate = filepath.Join(backupsRoot, base+"-"+hex.EncodeToString(suffix))
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not allocate a unique pre-restore dir under %s", backupsRoot)
}

// currentNativeAgents builds the AgentRoots describing the CURRENT native files
// that a Restore is about to overwrite, derived from the manifest's recorded
// paths (restricted to the named agent when agent != ""). Each native file is
// supplied as its own root so the pre-restore snapshot mirrors exactly the
// files being replaced, regardless of whether they still exist.
func currentNativeAgents(man Manifest, agent string) []AgentRoots {
	out := make([]AgentRoots, 0, len(man.Agents))
	for _, ag := range man.Agents {
		if agent != "" && ag.Name != agent {
			continue
		}
		roots := make([]string, 0, len(ag.Roots))
		for _, fe := range ag.Roots {
			// Skip entries that escape the agent root: Restore refuses them as
			// per-file failures, so the reversible pre-restore snapshot must not
			// reach out to their bogus reconstructed native paths either.
			target, ok := nativeTargetForSafe(ag.Name, fe.Path)
			if !ok {
				continue
			}
			roots = append(roots, target)
		}
		out = append(out, AgentRoots{Name: ag.Name, Roots: roots})
	}
	return out
}
