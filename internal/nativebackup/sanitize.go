package nativebackup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
)

// SanitizeStatus describes automatic maintenance of one existing local native
// snapshot. Legacy snapshots are deliberately distinct from unchanged v2
// snapshots: schema 0 has no authenticity proof and must never be rewritten as
// though its bytes had always been trusted.
type SanitizeStatus string

const (
	SanitizeUnchanged     SanitizeStatus = "unchanged"
	SanitizeComplete      SanitizeStatus = "sanitized"
	SanitizeLegacySkipped SanitizeStatus = "legacy-skipped"
)

type SanitizeResult struct {
	Status         SanitizeStatus
	RemovedFiles   int
	RemovedBytes   int64
	RemovedSkipped int
	RedactedFiles  int
}

type SanitizeOptions struct {
	CurrentAgentRoots []AgentRoots
	ManifestKeyPath   string
	// ExcludeTarget applies an agent-aware machine-state policy after each
	// authenticated manifest target has been resolved beneath its recorded
	// source root. It covers bounded dynamic layouts even when the corresponding
	// live directory is no longer present. A nil policy adds no such exclusions.
	ExcludeTarget func(NativeTarget) bool
	// KnownAgentExcludePaths derives today's machine-secret/runtime policy from
	// the authenticated source roots recorded in an older manifest. This keeps
	// credential cleanup effective even when that adapter is temporarily absent
	// from current discovery. A nil resolver applies only CurrentAgentRoots and
	// the package's generic dependency-directory policy.
	KnownAgentExcludePaths func(agent string, sourceRoots []string) []string
	// KnownAgentRedactions derives typed mixed-config redaction policy from the
	// authenticated source roots recorded in an older manifest. Redacted bytes
	// replace only the copied snapshot file; the native source is never edited.
	KnownAgentRedactions func(agent string, sourceRoots []string) []FileRedaction
}

type SanitizeRecoveryResult struct {
	Recovered int
	Finalized int
	Pending   int
}

const (
	sanitizeTransactionPrefix = ".aplexica-native-sanitize-"
	sanitizeJournalName       = "transaction.json"
	sanitizeRebuiltName       = "rebuilt"
	sanitizeOriginalName      = "original"
	sanitizeJournalVersion    = 1
	sanitizeJournalDomain     = "aplexica/native-sanitize-journal/v1\x00"
	sanitizeManifestMaxBytes  = 16 << 20
	sanitizeSnapshotIDMaxLen  = 240
	sanitizeTokenBytes        = 16
	sanitizeJournalMaxBytes   = 64 << 10
)

type sanitizePhase string

const (
	sanitizePhaseBuilding             sanitizePhase = "building"
	sanitizePhasePrepared             sanitizePhase = "prepared"
	sanitizePhaseOriginalMoved        sanitizePhase = "original-moved"
	sanitizePhaseReplacementInstalled sanitizePhase = "replacement-installed"
	sanitizePhaseCommitted            sanitizePhase = "committed"
	sanitizePhaseRolledBack           sanitizePhase = "rolled-back"
)

type sanitizeStep string

const (
	sanitizeStepJournalDurable       sanitizeStep = "journal-durable"
	sanitizeStepRebuiltVerified      sanitizeStep = "rebuilt-verified"
	sanitizeStepBeforeOriginalVerify sanitizeStep = "before-original-verify"
	sanitizeStepOriginalRenamed      sanitizeStep = "original-renamed"
	sanitizeStepMoveRecorded         sanitizeStep = "move-recorded"
	sanitizeStepRebuiltInstalled     sanitizeStep = "rebuilt-installed"
	sanitizeStepInstallRecorded      sanitizeStep = "install-recorded"
	sanitizeStepCommitted            sanitizeStep = "committed"
)

// sanitizeTestHooks is intentionally package-private. Production never asks a
// failed swap to remain half-applied; tests use leaveOnError to model SIGKILL at
// each durable boundary and exercise the next-start recovery path.
type sanitizeTestHooks struct {
	after        func(sanitizeStep) error
	leaveOnError bool
	removeTree   func(string) error
	rename       func(*privatefs.Root, string, string) error
}

type sanitizeJournalRecord struct {
	Version                 int           `json:"version"`
	Token                   string        `json:"token"`
	SnapshotID              string        `json:"snapshotId"`
	Phase                   sanitizePhase `json:"phase"`
	CreatedAt               string        `json:"createdAt"`
	OriginalModTimeUnixNano int64         `json:"originalModTimeUnixNano"`
	Checksum                string        `json:"checksum"`
}

var sanitizeTokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// SanitizeSnapshotContext removes files covered by today's native-backup
// exclusion policy from one older authenticated snapshot. It never edits the
// original tree in place: retained signed files are rebuilt and verified in a
// hidden transaction sibling, then installed through recoverable directory
// renames. Schema-0 snapshots are returned as legacy-skipped byte-for-byte.
func SanitizeSnapshotContext(ctx context.Context, backupDir string, opts SanitizeOptions) (SanitizeResult, error) {
	return sanitizeSnapshotContext(ctx, backupDir, opts, nil)
}

func sanitizeSnapshotContext(ctx context.Context, backupDir string, opts SanitizeOptions, hooks *sanitizeTestHooks) (result SanitizeResult, retErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	abs, err := filepath.Abs(backupDir)
	if err != nil {
		return result, err
	}
	abs = filepath.Clean(abs)
	backupsRoot := filepath.Dir(abs)
	snapshotID := filepath.Base(abs)
	if err := validateSanitizeSnapshotID(snapshotID); err != nil {
		return result, err
	}
	if info, err := os.Lstat(abs); err != nil {
		return result, err
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return result, fmt.Errorf("nativebackup: sanitize target is not a real snapshot directory")
	}

	man, err := readManifestStrictIntegrity(abs)
	if err != nil {
		return result, err
	}
	if man.SchemaVersion == 0 {
		return SanitizeResult{Status: SanitizeLegacySkipped}, nil
	}
	if man.SchemaVersion != 2 {
		return result, fmt.Errorf("nativebackup: unsupported manifest schema %d", man.SchemaVersion)
	}
	keyPath := opts.ManifestKeyPath
	if keyPath == "" {
		keyPath = manifestKeyPathForBackupDir(abs)
	}
	retainedKey, err := loadManifestKey(keyPath, false)
	if err != nil {
		return result, fmt.Errorf("nativebackup: load manifest key before sanitize: %w", err)
	}
	if err := verifyManifest(man, retainedKey); err != nil {
		return result, fmt.Errorf("nativebackup: authenticate snapshot before sanitize: %w", err)
	}
	filtered, result, changed, transformed, err := planSanitizedManifest(ctx, abs, man, opts.CurrentAgentRoots, opts.ExcludeTarget, opts.KnownAgentExcludePaths, opts.KnownAgentRedactions)
	if err != nil {
		return SanitizeResult{}, err
	}
	if !changed {
		result.Status = SanitizeUnchanged
		return result, nil
	}

	// A signed manifest authenticates metadata; prove every listed source byte
	// (including bytes about to be removed) before starting a rebuild.
	verified, err := verifyAuthenticatedSnapshotWithKey(ctx, abs, retainedKey, true)
	if err != nil {
		return SanitizeResult{}, fmt.Errorf("nativebackup: verify original snapshot before sanitize: %w", err)
	}
	filtered, result, changed, transformed, err = planSanitizedManifest(ctx, abs, verified, opts.CurrentAgentRoots, opts.ExcludeTarget, opts.KnownAgentExcludePaths, opts.KnownAgentRedactions)
	if err != nil {
		return SanitizeResult{}, err
	}
	if !changed {
		result.Status = SanitizeUnchanged
		return result, nil
	}

	root, err := privatefs.OpenRoot(backupsRoot, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return SanitizeResult{}, err
	}
	defer root.Close()
	token, err := newSanitizeToken()
	if err != nil {
		return SanitizeResult{}, err
	}
	txName := sanitizeTransactionPrefix + token
	info, err := os.Stat(abs)
	if err != nil {
		return SanitizeResult{}, err
	}
	record := sanitizeJournalRecord{
		Version:                 sanitizeJournalVersion,
		Token:                   token,
		SnapshotID:              snapshotID,
		Phase:                   sanitizePhaseBuilding,
		CreatedAt:               time.Now().UTC().Format(time.RFC3339Nano),
		OriginalModTimeUnixNano: info.ModTime().UnixNano(),
	}
	if err := root.EnsureDir(txName, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true}); err != nil {
		return SanitizeResult{}, err
	}
	if err := root.SyncDir("."); err != nil {
		_ = os.RemoveAll(filepath.Join(backupsRoot, txName))
		return SanitizeResult{}, err
	}
	transactionStarted := true
	journalDurable := false
	namespaceMutationAttempted := false
	defer func() {
		if retErr == nil || !transactionStarted || (hooks != nil && hooks.leaveOnError) {
			return
		}
		var recoveryErr error
		if !journalDurable || !namespaceMutationAttempted {
			recoveryErr = removeSanitizeTransaction(root, backupsRoot, txName, hooks)
		} else {
			// Root.Rename may report a post-rename directory-sync failure. Once
			// the journal is durable, filesystem presence—not a caller-side bool—
			// is the only safe authority for deciding whether the original moved.
			// Cancellation must stop an expensive rebuild, but once the original
			// namespace entry may have moved, rollback is mandatory. Preserve caller
			// values while detaching cancellation/deadline for this best-effort repair.
			rollbackCtx := context.WithoutCancel(ctx)
			_, recoveryErr = recoverSanitizeTransaction(rollbackCtx, root, backupsRoot, txName, keyPath, &retainedKey, true, hooks)
		}
		if recoveryErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("nativebackup: sanitize rollback: %w", recoveryErr))
		}
	}()
	if err := writeSanitizeJournal(root, txName, &record); err != nil {
		_ = os.RemoveAll(filepath.Join(backupsRoot, txName))
		transactionStarted = false
		return SanitizeResult{}, err
	}
	journalDurable = true
	if err := runSanitizeHook(hooks, sanitizeStepJournalDurable); err != nil {
		return SanitizeResult{}, err
	}

	rebuiltRel := filepath.Join(txName, sanitizeRebuiltName)
	if err := root.EnsureDir(rebuiltRel, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true}); err != nil {
		return SanitizeResult{}, err
	}
	rebuiltDir := filepath.Join(backupsRoot, rebuiltRel)
	if err := rebuildSanitizedSnapshot(ctx, abs, rebuiltDir, filtered, transformed, retainedKey, info.ModTime()); err != nil {
		return SanitizeResult{}, err
	}
	rebuiltManifest, err := verifyAuthenticatedSnapshotWithKey(ctx, rebuiltDir, retainedKey, true)
	if err != nil {
		return SanitizeResult{}, fmt.Errorf("nativebackup: verify rebuilt snapshot: %w", err)
	}
	if _, _, stillChanged, _, err := planSanitizedManifest(ctx, rebuiltDir, rebuiltManifest, opts.CurrentAgentRoots, opts.ExcludeTarget, opts.KnownAgentExcludePaths, opts.KnownAgentRedactions); err != nil {
		return SanitizeResult{}, err
	} else if stillChanged {
		return SanitizeResult{}, fmt.Errorf("nativebackup: rebuilt snapshot still contains excluded entries")
	}
	record.Phase = sanitizePhasePrepared
	if err := writeSanitizeJournal(root, txName, &record); err != nil {
		return SanitizeResult{}, err
	}
	if err := root.SyncDir(rebuiltRel); err != nil {
		return SanitizeResult{}, err
	}
	if err := root.SyncDir(txName); err != nil {
		return SanitizeResult{}, err
	}
	if err := runSanitizeHook(hooks, sanitizeStepRebuiltVerified); err != nil {
		return SanitizeResult{}, err
	}
	if err := runSanitizeHook(hooks, sanitizeStepBeforeOriginalVerify); err != nil {
		return SanitizeResult{}, err
	}
	if err := verifyManifestKeyUnchanged(keyPath, retainedKey); err != nil {
		return SanitizeResult{}, err
	}
	// A metadata-only pass catches additions, deletions, links, and manifest
	// replacement before the first namespace mutation without re-reading every
	// multi-gigabyte payload. Content is fully re-authenticated after the move,
	// where path-based writers can no longer win a verification/swap race.
	freshManifest, err := verifyAuthenticatedSnapshotWithKey(ctx, abs, retainedKey, false)
	if err != nil || freshManifest.Auth.MAC != verified.Auth.MAC {
		if err == nil {
			err = fmt.Errorf("manifest changed")
		}
		return SanitizeResult{}, fmt.Errorf("nativebackup: original changed during sanitize rebuild: %w", err)
	}
	if err := verifySnapshotInventoryOnly(ctx, abs, freshManifest); err != nil {
		return SanitizeResult{}, fmt.Errorf("nativebackup: original changed during sanitize rebuild: %w", err)
	}
	originalRel := filepath.Join(txName, sanitizeOriginalName)
	namespaceMutationAttempted = true
	if err := renameSanitizePath(root, snapshotID, originalRel, hooks); err != nil {
		return SanitizeResult{}, err
	}
	if err := root.SyncDir("."); err != nil {
		return SanitizeResult{}, err
	}
	if err := runSanitizeHook(hooks, sanitizeStepOriginalRenamed); err != nil {
		return SanitizeResult{}, err
	}
	// Rebuilding can take minutes. Move the original into the private transaction
	// first, then re-authenticate every byte and the complete inventory at its
	// retained identity. A path-based writer can no longer land a last-millisecond
	// update between verification and the swap that would silently be overwritten.
	originalDir := filepath.Join(backupsRoot, originalRel)
	freshOriginal, err := verifyAuthenticatedSnapshotWithKey(ctx, originalDir, retainedKey, true)
	if err != nil {
		return SanitizeResult{}, fmt.Errorf("nativebackup: original changed during sanitize rebuild: %w", err)
	}
	if freshOriginal.Auth.MAC != verified.Auth.MAC {
		return SanitizeResult{}, fmt.Errorf("nativebackup: original manifest changed during sanitize rebuild")
	}
	if err := verifyManifestKeyUnchanged(keyPath, retainedKey); err != nil {
		return SanitizeResult{}, err
	}
	record.Phase = sanitizePhaseOriginalMoved
	if err := writeSanitizeJournal(root, txName, &record); err != nil {
		return SanitizeResult{}, err
	}
	if err := runSanitizeHook(hooks, sanitizeStepMoveRecorded); err != nil {
		return SanitizeResult{}, err
	}

	if err := renameSanitizePath(root, rebuiltRel, snapshotID, hooks); err != nil {
		return SanitizeResult{}, err
	}
	if err := root.SyncDir(txName); err != nil {
		return SanitizeResult{}, err
	}
	if err := runSanitizeHook(hooks, sanitizeStepRebuiltInstalled); err != nil {
		return SanitizeResult{}, err
	}
	record.Phase = sanitizePhaseReplacementInstalled
	if err := writeSanitizeJournal(root, txName, &record); err != nil {
		return SanitizeResult{}, err
	}
	if err := runSanitizeHook(hooks, sanitizeStepInstallRecorded); err != nil {
		return SanitizeResult{}, err
	}

	installed, err := verifyAuthenticatedSnapshotWithKey(ctx, abs, retainedKey, true)
	if err != nil {
		return SanitizeResult{}, fmt.Errorf("nativebackup: verify installed sanitized snapshot: %w", err)
	}
	if _, _, stillChanged, _, err := planSanitizedManifest(ctx, abs, installed, opts.CurrentAgentRoots, opts.ExcludeTarget, opts.KnownAgentExcludePaths, opts.KnownAgentRedactions); err != nil {
		return SanitizeResult{}, err
	} else if stillChanged {
		return SanitizeResult{}, fmt.Errorf("nativebackup: installed snapshot still contains excluded entries")
	}
	if err := verifyManifestKeyUnchanged(keyPath, retainedKey); err != nil {
		return SanitizeResult{}, err
	}
	record.Phase = sanitizePhaseCommitted
	if err := writeSanitizeJournal(root, txName, &record); err != nil {
		return SanitizeResult{}, err
	}
	if err := runSanitizeHook(hooks, sanitizeStepCommitted); err != nil {
		return SanitizeResult{}, err
	}
	if err := removeSanitizeTransaction(root, backupsRoot, txName, hooks); err != nil {
		return SanitizeResult{}, err
	}
	transactionStarted = false
	result.Status = SanitizeComplete
	return result, nil
}

func renameSanitizePath(root *privatefs.Root, oldPath, newPath string, hooks *sanitizeTestHooks) error {
	if hooks != nil && hooks.rename != nil {
		return hooks.rename(root, oldPath, newPath)
	}
	return root.Rename(oldPath, newPath)
}

func validateSanitizeSnapshotID(id string) error {
	if id == "" || id == "." || filepath.Base(id) != id || filepath.Clean(id) != id || filepath.VolumeName(id) != "" || strings.ContainsAny(id, `/\\`) || len(id) > sanitizeSnapshotIDMaxLen {
		return fmt.Errorf("nativebackup: invalid sanitize snapshot id %q", id)
	}
	if _, ok := SnapshotKindFromID(id); !ok {
		return fmt.Errorf("nativebackup: sanitize target is not a native snapshot")
	}
	return nil
}

func newSanitizeToken() (string, error) {
	b := make([]byte, sanitizeTokenBytes)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func runSanitizeHook(hooks *sanitizeTestHooks, step sanitizeStep) error {
	if hooks == nil || hooks.after == nil {
		return nil
	}
	return hooks.after(step)
}

func sanitizeJournalChecksum(record sanitizeJournalRecord) (string, error) {
	record.Checksum = ""
	b, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte(sanitizeJournalDomain))
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validSanitizePhase(phase sanitizePhase) bool {
	switch phase {
	case sanitizePhaseBuilding, sanitizePhasePrepared, sanitizePhaseOriginalMoved, sanitizePhaseReplacementInstalled, sanitizePhaseCommitted, sanitizePhaseRolledBack:
		return true
	default:
		return false
	}
}

func writeSanitizeJournal(root *privatefs.Root, txName string, record *sanitizeJournalRecord) error {
	if root == nil || record == nil || txName != sanitizeTransactionPrefix+record.Token || !sanitizeTokenPattern.MatchString(record.Token) || !validSanitizePhase(record.Phase) {
		return fmt.Errorf("nativebackup: invalid sanitize journal")
	}
	if err := validateSanitizeSnapshotID(record.SnapshotID); err != nil {
		return err
	}
	checksum, err := sanitizeJournalChecksum(*record)
	if err != nil {
		return err
	}
	record.Checksum = checksum
	b, err := json.Marshal(*record)
	if err != nil {
		return err
	}
	return root.WriteFile(filepath.Join(txName, sanitizeJournalName), b, privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
}

func readSanitizeJournal(root *privatefs.Root, txName string) (sanitizeJournalRecord, error) {
	var record sanitizeJournalRecord
	token := strings.TrimPrefix(txName, sanitizeTransactionPrefix)
	if txName != sanitizeTransactionPrefix+token || !sanitizeTokenPattern.MatchString(token) {
		return record, fmt.Errorf("nativebackup: invalid sanitize transaction name")
	}
	f, err := root.OpenReadRegular(filepath.Join(txName, sanitizeJournalName))
	if err != nil {
		return record, err
	}
	b, readErr := io.ReadAll(io.LimitReader(f, sanitizeJournalMaxBytes+1))
	closeErr := f.Close()
	if readErr != nil {
		return record, readErr
	}
	if closeErr != nil {
		return record, closeErr
	}
	if len(b) > sanitizeJournalMaxBytes {
		return record, fmt.Errorf("nativebackup: sanitize journal exceeds limit")
	}
	if err := rejectDuplicateJSONKeys(b); err != nil {
		return record, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&record); err != nil {
		return record, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return record, fmt.Errorf("nativebackup: trailing sanitize journal data")
	}
	if record.Version != sanitizeJournalVersion || record.Token != token || !validSanitizePhase(record.Phase) {
		return record, fmt.Errorf("nativebackup: unsupported sanitize journal")
	}
	if err := validateSanitizeSnapshotID(record.SnapshotID); err != nil {
		return record, err
	}
	want, err := sanitizeJournalChecksum(record)
	if err != nil || want != record.Checksum {
		return record, fmt.Errorf("nativebackup: sanitize journal checksum mismatch")
	}
	return record, nil
}

func readManifestStrictIntegrity(backupDir string) (Manifest, error) {
	abs, err := filepath.Abs(backupDir)
	if err != nil {
		return Manifest{}, err
	}
	root, err := privatefs.OpenRoot(abs, privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly})
	if err != nil {
		return Manifest{}, err
	}
	defer root.Close()
	f, err := root.OpenReadRegularIntegrity(ManifestName)
	if err != nil {
		return Manifest{}, err
	}
	b, readErr := io.ReadAll(io.LimitReader(f, sanitizeManifestMaxBytes+1))
	closeErr := f.Close()
	if readErr != nil {
		return Manifest{}, readErr
	}
	if closeErr != nil {
		return Manifest{}, closeErr
	}
	if len(b) > sanitizeManifestMaxBytes {
		return Manifest{}, fmt.Errorf("nativebackup: manifest limit exceeded")
	}
	if err := rejectDuplicateJSONKeys(b); err != nil {
		return Manifest{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var man Manifest
	if err := dec.Decode(&man); err != nil {
		return Manifest{}, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return Manifest{}, fmt.Errorf("nativebackup: trailing manifest data")
	}
	return man, nil
}

type resolvedManifestPath struct {
	target string
	nt     NativeTarget
}

func validateManifestStructure(man Manifest) (map[string]resolvedManifestPath, error) {
	limits := DefaultNativeRestoreLimits()
	resolved := make(map[string]resolvedManifestPath)
	seenTargets := make(map[string]bool)
	agentNames := make(map[string]bool, len(man.Agents))
	var total int64
	files := 0
	for _, agent := range man.Agents {
		if agent.Name == "" || strings.ContainsAny(agent.Name, `/\\`) || agentNames[agent.Name] {
			return nil, fmt.Errorf("nativebackup: unsafe or duplicate manifest agent")
		}
		agentNames[agent.Name] = true
		roots, err := strictManifestSourceRoots(agent.SourceRoots)
		if err != nil {
			return nil, fmt.Errorf("nativebackup: manifest agent %q source roots: %w", agent.Name, err)
		}
		for _, entry := range agent.Roots {
			if entry.Bytes < 0 || entry.Bytes > limits.MaxFileBytes || total > limits.MaxTotalBytes-entry.Bytes {
				return nil, fmt.Errorf("nativebackup: manifest file limits exceeded")
			}
			digest, err := hex.DecodeString(entry.SHA256)
			if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != entry.SHA256 {
				return nil, fmt.Errorf("nativebackup: invalid manifest digest")
			}
			rp, err := resolveManifestPath(agent.Name, entry.Path, roots)
			if err != nil {
				return nil, err
			}
			key := manifestPathKey(entry.Path)
			if _, duplicate := resolved[key]; duplicate {
				return nil, fmt.Errorf("nativebackup: duplicate manifest path %q", entry.Path)
			}
			targetKey := manifestTargetKey(rp.target)
			if seenTargets[targetKey] {
				return nil, fmt.Errorf("nativebackup: duplicate manifest target")
			}
			resolved[key] = rp
			seenTargets[targetKey] = true
			total += entry.Bytes
			files++
			if files > limits.MaxFiles {
				return nil, fmt.Errorf("nativebackup: manifest file limits exceeded")
			}
		}
		for _, skipped := range agent.Skipped {
			rp, err := resolveManifestPath(agent.Name, skipped.Path, roots)
			if err != nil {
				return nil, err
			}
			key := manifestPathKey(skipped.Path)
			if _, duplicate := resolved[key]; duplicate {
				return nil, fmt.Errorf("nativebackup: duplicate manifest path %q", skipped.Path)
			}
			resolved[key] = rp
		}
	}
	return resolved, nil
}

func strictManifestSourceRoots(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("source roots missing")
	}
	seen := make(map[string]bool, len(values))
	roots := make([]string, 0, len(values))
	for _, value := range values {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value || seen[manifestTargetKey(value)] {
			return nil, fmt.Errorf("unsafe or duplicate source root")
		}
		seen[manifestTargetKey(value)] = true
		roots = append(roots, value)
	}
	return roots, nil
}

func resolveManifestPath(agent, path string, roots []string) (resolvedManifestPath, error) {
	if path == "" || strings.Contains(path, `\\`) {
		return resolvedManifestPath{}, fmt.Errorf("nativebackup: unsafe manifest path %q", path)
	}
	relPath := filepath.Clean(filepath.FromSlash(path))
	if relPath == "." || relPath == ".." || filepath.IsAbs(relPath) || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) || filepath.ToSlash(relPath) != path {
		return resolvedManifestPath{}, fmt.Errorf("nativebackup: unsafe manifest path %q", path)
	}
	var matches []resolvedManifestPath
	for _, root := range roots {
		prefix := agent
		if mirrored := filepath.ToSlash(relativize(root)); mirrored != "" {
			prefix += "/" + mirrored
		}
		if path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		suffix := strings.TrimPrefix(strings.TrimPrefix(path, prefix), "/")
		rel := filepath.Clean(filepath.FromSlash(suffix))
		if suffix == "" {
			rel = ""
		}
		if rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return resolvedManifestPath{}, fmt.Errorf("nativebackup: manifest path escapes source root")
		}
		matches = append(matches, resolvedManifestPath{target: filepath.Join(root, rel), nt: NativeTarget{Agent: agent, Root: root, RelativePath: rel}})
	}
	if len(matches) != 1 {
		return resolvedManifestPath{}, fmt.Errorf("nativebackup: manifest path has %d source-root matches", len(matches))
	}
	return matches[0], nil
}

func manifestPathKey(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func manifestTargetKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func planSanitizedManifest(
	ctx context.Context,
	backupDir string,
	man Manifest,
	current []AgentRoots,
	excludeTarget func(NativeTarget) bool,
	knownPolicy func(string, []string) []string,
	knownRedactions func(string, []string) []FileRedaction,
) (Manifest, SanitizeResult, bool, map[string][]byte, error) {
	resolved, err := validateManifestStructure(man)
	if err != nil {
		return Manifest{}, SanitizeResult{}, false, nil, err
	}
	exclusions := make(map[string][]string, len(current))
	redactions := make(map[string]map[string]FileRedactionKind, len(current))
	for _, agent := range current {
		validated, err := validateExcludePaths(agent.Roots, agent.ExcludePaths)
		if err != nil {
			return Manifest{}, SanitizeResult{}, false, nil, fmt.Errorf("nativebackup: sanitize agent %q exclusions: %w", agent.Name, err)
		}
		exclusions[agent.Name] = append(exclusions[agent.Name], validated...)
		validatedRedactions, err := validateRedactionPaths(agent.Roots, agent.RedactFiles)
		if err != nil {
			return Manifest{}, SanitizeResult{}, false, nil, fmt.Errorf("nativebackup: sanitize agent %q redactions: %w", agent.Name, err)
		}
		if err := mergeRedactionPolicies(redactions, agent.Name, validatedRedactions); err != nil {
			return Manifest{}, SanitizeResult{}, false, nil, err
		}
	}
	if knownPolicy != nil {
		for _, agent := range man.Agents {
			derived := knownPolicy(agent.Name, append([]string(nil), agent.SourceRoots...))
			validated, err := validateExcludePaths(agent.SourceRoots, derived)
			if err != nil {
				return Manifest{}, SanitizeResult{}, false, nil, fmt.Errorf("nativebackup: sanitize known agent %q exclusions: %w", agent.Name, err)
			}
			exclusions[agent.Name] = append(exclusions[agent.Name], validated...)
		}
	}
	if knownRedactions != nil {
		for _, agent := range man.Agents {
			derived := knownRedactions(agent.Name, append([]string(nil), agent.SourceRoots...))
			validated, err := validateRedactionPaths(agent.SourceRoots, derived)
			if err != nil {
				return Manifest{}, SanitizeResult{}, false, nil, fmt.Errorf("nativebackup: sanitize known agent %q redactions: %w", agent.Name, err)
			}
			if err := mergeRedactionPolicies(redactions, agent.Name, validated); err != nil {
				return Manifest{}, SanitizeResult{}, false, nil, err
			}
		}
	}
	out := man
	out.Auth = ManifestAuth{}
	out.Agents = make([]AgentManifest, 0, len(man.Agents))
	result := SanitizeResult{}
	changed := false
	transformed := make(map[string][]byte)
	for _, agent := range man.Agents {
		filtered := AgentManifest{Name: agent.Name, SourceRoots: append([]string(nil), agent.SourceRoots...)}
		for _, entry := range agent.Roots {
			rp := resolved[manifestPathKey(entry.Path)]
			if restoreTargetExcluded(rp.target, rp.nt, exclusions[agent.Name]) ||
				(excludeTarget != nil && excludeTarget(rp.nt)) {
				result.RemovedFiles++
				result.RemovedBytes += entry.Bytes
				changed = true
				continue
			}
			if kind, ok := redactions[agent.Name][manifestTargetKey(rp.target)]; ok {
				redacted, redactionChanged, removeUnredactable, err := planRedactedManifestEntry(ctx, backupDir, entry, kind)
				if err != nil {
					return Manifest{}, SanitizeResult{}, false, nil, err
				}
				if removeUnredactable {
					// Invalid or oversized historical mixed configs cannot be safely
					// transformed. Drop that authenticated backup copy rather than
					// retaining raw credentials indefinitely; the live source is never
					// touched and new snapshots already record such files as skipped.
					result.RemovedFiles++
					result.RemovedBytes += entry.Bytes
					changed = true
					continue
				}
				if redactionChanged {
					entry.Bytes = int64(len(redacted))
					digest := sha256.Sum256(redacted)
					entry.SHA256 = hex.EncodeToString(digest[:])
					transformed[entry.Path] = redacted
					result.RedactedFiles++
					changed = true
				}
			}
			filtered.Roots = append(filtered.Roots, entry)
		}
		for _, skipped := range agent.Skipped {
			rp := resolved[manifestPathKey(skipped.Path)]
			if restoreTargetExcluded(rp.target, rp.nt, exclusions[agent.Name]) {
				result.RemovedSkipped++
				changed = true
				continue
			}
			filtered.Skipped = append(filtered.Skipped, skipped)
		}
		out.Agents = append(out.Agents, filtered)
	}
	return out, result, changed, transformed, nil
}

func mergeRedactionPolicies(dst map[string]map[string]FileRedactionKind, agent string, incoming map[string]FileRedactionKind) error {
	if len(incoming) == 0 {
		return nil
	}
	if dst[agent] == nil {
		dst[agent] = make(map[string]FileRedactionKind, len(incoming))
	}
	for path, kind := range incoming {
		key := manifestTargetKey(path)
		if existing, ok := dst[agent][key]; ok && existing != kind {
			return fmt.Errorf("nativebackup: agent %q redaction path %q has conflicting kinds", agent, path)
		}
		dst[agent][key] = kind
	}
	return nil
}

// planRedactedManifestEntry authenticates the one bounded mixed-config file
// against its already-authenticated manifest line before inspecting it. The
// later full-snapshot verification remains mandatory before any transaction is
// created, but this targeted proof lets an already-redacted snapshot remain a
// cheap identity-preserving no-op on subsequent maintenance passes.
func planRedactedManifestEntry(ctx context.Context, backupDir string, entry FileEntry, kind FileRedactionKind) ([]byte, bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, false, err
	}
	if entry.Bytes < 0 {
		return nil, false, false, fmt.Errorf("nativebackup: sanitize redaction source %q has invalid size", entry.Path)
	}
	if entry.Bytes > backupRedactionMaxInputBytes {
		return nil, false, true, nil
	}
	root, err := privatefs.OpenNativeRoot(backupDir, privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly})
	if err != nil {
		return nil, false, false, err
	}
	defer root.Close()
	in, err := root.OpenReadRegularIntegrity(filepath.FromSlash(entry.Path))
	if err != nil {
		return nil, false, false, fmt.Errorf("nativebackup: open redaction source %q: %w", entry.Path, err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(in, backupRedactionMaxInputBytes+1))
	closeErr := in.Close()
	if readErr != nil {
		return nil, false, false, fmt.Errorf("nativebackup: read redaction source %q: %w", entry.Path, readErr)
	}
	if closeErr != nil {
		return nil, false, false, closeErr
	}
	digest := sha256.Sum256(raw)
	if int64(len(raw)) != entry.Bytes || hex.EncodeToString(digest[:]) != entry.SHA256 {
		return nil, false, false, fmt.Errorf("nativebackup: redaction source %q does not match authenticated manifest", entry.Path)
	}
	redacted, err := redactBackupFile(kind, raw)
	if err != nil {
		return nil, false, true, nil
	}
	if bytes.Equal(raw, redacted) {
		return nil, false, false, nil
	}
	return redacted, true, false, nil
}

func verifyAuthenticatedSnapshot(ctx context.Context, backupDir, keyPath string, full bool) (Manifest, error) {
	key, err := loadManifestKey(keyPath, false)
	if err != nil {
		return Manifest{}, err
	}
	return verifyAuthenticatedSnapshotWithKey(ctx, backupDir, key, full)
}

// ReadSnapshotManifestContext performs the same bounded, duplicate-key and
// unknown-field rejecting, no-follow read used by authenticated snapshot
// verification, then validates the complete manifest structure. It does not
// authenticate the manifest and is therefore only suitable for compatibility
// decisions about genuine legacy schema-0 snapshots; payload acceptance still
// requires VerifySnapshotFilesContext.
func ReadSnapshotManifestContext(ctx context.Context, backupDir string) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	directoryBefore, err := realDirectoryInfo(backupDir)
	if err != nil {
		return Manifest{}, err
	}
	man, err := readManifestStrictIntegrity(backupDir)
	if err != nil {
		return Manifest{}, err
	}
	if _, err := validateManifestStructure(man); err != nil {
		return Manifest{}, err
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if err := verifyDirectoryIdentity(backupDir, directoryBefore); err != nil {
		return Manifest{}, err
	}
	return man, nil
}

// AuthenticateSnapshotManifestContext performs a strict, bounded, no-follow
// read of a snapshot manifest and authenticates its complete signed projection.
// It also validates the signed inventory structure, but deliberately does not
// open or hash any payload file. Callers may use the returned metadata only to
// decide whether a snapshot is a relevant candidate; accepting a snapshot as a
// recovery point still requires VerifyAuthenticatedSnapshotContext.
func AuthenticateSnapshotManifestContext(ctx context.Context, backupDir, keyPath string) (Manifest, error) {
	man, err := verifyAuthenticatedSnapshot(ctx, backupDir, keyPath, false)
	if err != nil {
		return Manifest{}, err
	}
	if _, err := validateManifestStructure(man); err != nil {
		return Manifest{}, err
	}
	return man, nil
}

// VerifyAuthenticatedSnapshotContext authenticates a v2 manifest and streams
// every listed file while rejecting links, special files, unmanifested files,
// path replacement, and digest mismatches. Startup safety uses the same verifier
// as sanitizer commit/recovery so a same-size corrupt snapshot is never treated
// as a valid rollback point.
func VerifyAuthenticatedSnapshotContext(ctx context.Context, backupDir, keyPath string) (Manifest, error) {
	return verifyAuthenticatedSnapshot(ctx, backupDir, keyPath, true)
}

// VerifySnapshotFilesContext performs the full file/inventory proof for a
// manifest whose trust decision was made by the caller (notably legacy schema-0
// safety snapshots). It does not manufacture or imply manifest authenticity.
func VerifySnapshotFilesContext(ctx context.Context, backupDir string, man Manifest) error {
	return verifySnapshotFilesAndInventory(ctx, backupDir, man)
}

func verifyAuthenticatedSnapshotWithKey(ctx context.Context, backupDir string, key [manifestAuthKeyBytes]byte, full bool) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	directoryBefore, err := realDirectoryInfo(backupDir)
	if err != nil {
		return Manifest{}, err
	}
	man, err := readManifestStrictIntegrity(backupDir)
	if err != nil {
		return Manifest{}, err
	}
	if err := verifyManifest(man, key); err != nil {
		return Manifest{}, err
	}
	if !full {
		if err := verifyDirectoryIdentity(backupDir, directoryBefore); err != nil {
			return Manifest{}, err
		}
		return man, nil
	}
	if err := verifySnapshotFilesAndInventory(ctx, backupDir, man); err != nil {
		return Manifest{}, err
	}
	// The manifest is a file inside the tree and can be replaced independently
	// while large payloads are hashed. Re-read/authenticate it after the inventory
	// pass and require the exact signed projection we started with.
	freshManifest, err := readManifestStrictIntegrity(backupDir)
	if err != nil {
		return Manifest{}, err
	}
	if err := verifyManifest(freshManifest, key); err != nil {
		return Manifest{}, err
	}
	if freshManifest.Auth.MAC != man.Auth.MAC {
		return Manifest{}, fmt.Errorf("nativebackup: manifest changed during snapshot verification")
	}
	if err := verifyDirectoryIdentity(backupDir, directoryBefore); err != nil {
		return Manifest{}, err
	}
	return man, nil
}

func verifyManifestKeyUnchanged(keyPath string, retained [manifestAuthKeyBytes]byte) error {
	current, err := loadManifestKey(keyPath, false)
	if err != nil {
		return fmt.Errorf("nativebackup: reload manifest key: %w", err)
	}
	if subtle.ConstantTimeCompare(current[:], retained[:]) != 1 {
		return fmt.Errorf("nativebackup: manifest key changed during sanitize")
	}
	return nil
}

func verifySnapshotFilesAndInventory(ctx context.Context, backupDir string, man Manifest) error {
	if _, err := validateManifestStructure(man); err != nil {
		return err
	}
	directoryBefore, err := realDirectoryInfo(backupDir)
	if err != nil {
		return err
	}
	root, err := privatefs.OpenNativeRoot(backupDir, privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly})
	if err != nil {
		return err
	}
	defer root.Close()
	type listedFileState struct {
		info os.FileInfo
	}
	listed := make(map[string]listedFileState)
	for _, agent := range man.Agents {
		for _, entry := range agent.Roots {
			if err := ctx.Err(); err != nil {
				return err
			}
			rel := filepath.FromSlash(entry.Path)
			f, err := root.OpenReadRegularIntegrity(rel)
			if err != nil {
				return fmt.Errorf("nativebackup: verify file %q: %w", entry.Path, err)
			}
			infoBefore, statErr := f.Stat()
			if statErr != nil || infoBefore.Size() != entry.Bytes {
				_ = f.Close()
				return fmt.Errorf("nativebackup: verify file %q size mismatch", entry.Path)
			}
			h := sha256.New()
			n, copyErr := copyWithContext(ctx, h, f)
			infoAfter, restatErr := f.Stat()
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if restatErr != nil || !os.SameFile(infoBefore, infoAfter) || infoAfter.Size() != entry.Bytes {
				return fmt.Errorf("nativebackup: verify file %q identity changed", entry.Path)
			}
			if closeErr != nil {
				return closeErr
			}
			if n != entry.Bytes || hex.EncodeToString(h.Sum(nil)) != entry.SHA256 {
				return fmt.Errorf("nativebackup: verify file %q digest mismatch", entry.Path)
			}
			listed[filepath.ToSlash(filepath.Clean(rel))] = listedFileState{info: infoAfter}
		}
	}
	seen := make(map[string]bool, len(listed))
	if err := filepath.WalkDir(backupDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == backupDir {
			info, err := entry.Info()
			if err != nil || entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() || !os.SameFile(directoryBefore, info) {
				return fmt.Errorf("nativebackup: snapshot directory identity changed")
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("nativebackup: snapshot contains unmanifested link %q", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("nativebackup: snapshot contains special file %q", path)
		}
		rel, err := filepath.Rel(backupDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestName {
			return nil
		}
		state, ok := listed[rel]
		if !ok {
			return fmt.Errorf("nativebackup: snapshot contains unmanifested file %q", rel)
		}
		if !os.SameFile(state.info, info) || state.info.Size() != info.Size() || !state.info.ModTime().Equal(info.ModTime()) {
			return fmt.Errorf("nativebackup: snapshot file %q changed during inventory", rel)
		}
		seen[rel] = true
		return nil
	}); err != nil {
		return err
	}
	if len(seen) != len(listed) {
		return fmt.Errorf("nativebackup: snapshot lost a manifested file during inventory")
	}
	return verifyDirectoryIdentity(backupDir, directoryBefore)
}

func verifySnapshotInventoryOnly(ctx context.Context, backupDir string, man Manifest) error {
	if _, err := validateManifestStructure(man); err != nil {
		return err
	}
	directoryBefore, err := realDirectoryInfo(backupDir)
	if err != nil {
		return err
	}
	listed := make(map[string]int64)
	for _, agent := range man.Agents {
		for _, file := range agent.Roots {
			listed[filepath.ToSlash(filepath.Clean(filepath.FromSlash(file.Path)))] = file.Bytes
		}
	}
	seen := make(map[string]bool, len(listed))
	if err := filepath.WalkDir(backupDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == backupDir {
			info, err := entry.Info()
			if err != nil || entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() || !os.SameFile(directoryBefore, info) {
				return fmt.Errorf("nativebackup: snapshot directory identity changed")
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("nativebackup: snapshot contains unmanifested link %q", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("nativebackup: snapshot contains special file %q", path)
		}
		rel, err := filepath.Rel(backupDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestName {
			return nil
		}
		wantBytes, ok := listed[rel]
		if !ok {
			return fmt.Errorf("nativebackup: snapshot contains unmanifested file %q", rel)
		}
		if info.Size() != wantBytes {
			return fmt.Errorf("nativebackup: snapshot file %q size changed", rel)
		}
		seen[rel] = true
		return nil
	}); err != nil {
		return err
	}
	if len(seen) != len(listed) {
		return fmt.Errorf("nativebackup: snapshot lost a manifested file during inventory")
	}
	return verifyDirectoryIdentity(backupDir, directoryBefore)
}

func realDirectoryInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("nativebackup: snapshot path is not a real directory")
	}
	return info, nil
}

func verifyDirectoryIdentity(path string, before os.FileInfo) error {
	after, err := realDirectoryInfo(path)
	if err != nil {
		return err
	}
	if before == nil || !os.SameFile(before, after) {
		return fmt.Errorf("nativebackup: snapshot directory identity changed")
	}
	return nil
}

func rebuildSanitizedSnapshot(ctx context.Context, sourceDir, rebuiltDir string, man Manifest, transformed map[string][]byte, key [manifestAuthKeyBytes]byte, originalModTime time.Time) error {
	source, err := privatefs.OpenNativeRoot(sourceDir, privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly})
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := privatefs.OpenNativeRoot(rebuiltDir, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return err
	}
	defer destination.Close()

	for _, agent := range man.Agents {
		for _, entry := range agent.Roots {
			if err := ctx.Err(); err != nil {
				return err
			}
			rel := filepath.FromSlash(entry.Path)
			parent := filepath.Dir(rel)
			if err := destination.EnsureDir(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true}); err != nil {
				return err
			}
			if redacted, ok := transformed[entry.Path]; ok {
				n, digest, writeErr := destination.WriteReader(rel, bytes.NewReader(redacted), int64(len(redacted)), privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
				if writeErr != nil {
					return writeErr
				}
				if n != entry.Bytes || hex.EncodeToString(digest[:]) != entry.SHA256 {
					return fmt.Errorf("nativebackup: redacted output does not match planned manifest for %q", entry.Path)
				}
				continue
			}
			in, err := source.OpenReadRegularIntegrity(rel)
			if err != nil {
				return err
			}
			n, digest, writeErr := destination.WriteReader(rel, in, entry.Bytes, privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
			closeErr := in.Close()
			if writeErr != nil {
				return writeErr
			}
			if closeErr != nil {
				return closeErr
			}
			if n != entry.Bytes || hex.EncodeToString(digest[:]) != entry.SHA256 {
				return fmt.Errorf("nativebackup: source changed while rebuilding %q", entry.Path)
			}
		}
	}
	man.SchemaVersion = 2
	man.Auth = ManifestAuth{}
	if err := signManifest(&man, key); err != nil {
		return err
	}
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	if err := destination.WriteFile(ManifestName, b, privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true}); err != nil {
		return err
	}
	if err := syncSnapshotDirectories(destination, rebuiltDir); err != nil {
		return err
	}
	if err := os.Chtimes(rebuiltDir, originalModTime, originalModTime); err != nil {
		return err
	}
	return destination.SyncDir(".")
}

func syncSnapshotDirectories(root *privatefs.Root, snapshotDir string) error {
	var dirs []string
	err := filepath.WalkDir(snapshotDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("nativebackup: rebuilt snapshot contains a link")
		}
		if entry.IsDir() {
			rel, err := filepath.Rel(snapshotDir, path)
			if err != nil {
				return err
			}
			dirs = append(dirs, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(filepath.Clean(dirs[i]), string(filepath.Separator)) > strings.Count(filepath.Clean(dirs[j]), string(filepath.Separator))
	})
	for _, rel := range dirs {
		if err := root.SyncDir(rel); err != nil {
			return err
		}
	}
	return nil
}

// RecoverSanitizeTransactionsContext repairs interrupted directory swaps. A
// fast pass (cleanup=false) performs only the renames needed to make every
// original snapshot ID visible before startup safety checks; the asynchronous
// cleanup pass verifies full file digests and removes hidden rebuild/rollback
// trees.
func RecoverSanitizeTransactionsContext(ctx context.Context, backupsRoot, keyPath string, cleanup bool) (SanitizeRecoveryResult, error) {
	var result SanitizeRecoveryResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, err := os.Lstat(backupsRoot); errors.Is(err, os.ErrNotExist) {
		return result, nil
	} else if err != nil {
		return result, err
	}
	root, err := privatefs.OpenRoot(backupsRoot, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return result, err
	}
	defer root.Close()
	entries, err := root.ReadDir(".")
	if err != nil {
		return result, err
	}
	var errs []error
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), sanitizeTransactionPrefix) {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			errs = append(errs, fmt.Errorf("nativebackup: unsafe sanitize transaction object %q", entry.Name()))
			continue
		}
		action, err := recoverSanitizeTransaction(ctx, root, backupsRoot, entry.Name(), keyPath, nil, cleanup, nil)
		if err != nil {
			// A crash can leave either an empty directory after journal-last
			// cleanup, or only the bounded private temp used to install the very
			// first journal. Reclaim only those exact journal-less shapes; any bulk
			// child or unexpected object remains untouched for diagnosis.
			if errors.Is(err, os.ErrNotExist) {
				if removed, removeErr := removeJournalLessSanitizeTransaction(root, entry.Name()); removeErr == nil && removed {
					result.Finalized++
					continue
				}
			}
			errs = append(errs, fmt.Errorf("nativebackup: recover sanitize transaction %q: %w", entry.Name(), err))
			continue
		}
		switch action {
		case "recovered":
			result.Recovered++
		case "finalized":
			result.Finalized++
		case "pending":
			result.Pending++
		}
	}
	return result, errors.Join(errs...)
}

func recoverSanitizeTransaction(ctx context.Context, root *privatefs.Root, backupsRoot, txName, keyPath string, retainedKey *[manifestAuthKeyBytes]byte, cleanup bool, hooks *sanitizeTestHooks) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	record, err := readSanitizeJournal(root, txName)
	if err != nil {
		return "", err
	}
	if keyPath == "" {
		keyPath = manifestKeyPathForBackupDir(filepath.Join(backupsRoot, record.SnapshotID))
	}
	key := [manifestAuthKeyBytes]byte{}
	if retainedKey != nil {
		key = *retainedKey
	} else {
		key, err = loadManifestKey(keyPath, false)
		if err != nil {
			return "", err
		}
	}
	finalExists, err := realChildDirectoryExists(root, ".", record.SnapshotID)
	if err != nil {
		return "", err
	}
	originalExists, err := realChildDirectoryExists(root, txName, sanitizeOriginalName)
	if err != nil {
		return "", err
	}
	rebuiltExists, err := realChildDirectoryExists(root, txName, sanitizeRebuiltName)
	if err != nil {
		return "", err
	}
	finalDir := filepath.Join(backupsRoot, record.SnapshotID)
	originalDir := filepath.Join(backupsRoot, txName, sanitizeOriginalName)

	if record.Phase == sanitizePhaseCommitted {
		if !finalExists {
			if !originalExists {
				return "", fmt.Errorf("committed transaction has neither installed nor rollback snapshot")
			}
			if _, err := verifyAuthenticatedSnapshotWithKey(ctx, originalDir, key, false); err != nil {
				return "", fmt.Errorf("authenticate rollback snapshot: %w", err)
			}
			if err := root.Rename(filepath.Join(txName, sanitizeOriginalName), record.SnapshotID); err != nil {
				return "", err
			}
			if err := root.SyncDir(txName); err != nil {
				return "", err
			}
			record.Phase = sanitizePhaseRolledBack
			if err := writeSanitizeJournal(root, txName, &record); err != nil {
				return "", err
			}
			finalExists, originalExists = true, false
		}
		if !cleanup {
			if _, err := verifyAuthenticatedSnapshotWithKey(ctx, finalDir, key, false); err != nil {
				return "", err
			}
			return "pending", nil
		}
		if _, err := verifyAuthenticatedSnapshotWithKey(ctx, finalDir, key, true); err != nil {
			if !originalExists {
				return "", fmt.Errorf("installed snapshot invalid and rollback unavailable: %w", err)
			}
			if _, rollbackErr := verifyAuthenticatedSnapshotWithKey(ctx, originalDir, key, true); rollbackErr != nil {
				return "", errors.Join(err, fmt.Errorf("rollback snapshot invalid: %w", rollbackErr))
			}
			if rebuiltExists {
				return "", fmt.Errorf("cannot preserve invalid installed tree: rebuilt slot occupied")
			}
			if renameErr := root.Rename(record.SnapshotID, filepath.Join(txName, sanitizeRebuiltName)); renameErr != nil {
				return "", renameErr
			}
			if renameErr := root.Rename(filepath.Join(txName, sanitizeOriginalName), record.SnapshotID); renameErr != nil {
				_ = root.Rename(filepath.Join(txName, sanitizeRebuiltName), record.SnapshotID)
				return "", renameErr
			}
			record.Phase = sanitizePhaseRolledBack
			if err := writeSanitizeJournal(root, txName, &record); err != nil {
				return "", err
			}
		}
		if err := removeSanitizeTransaction(root, backupsRoot, txName, hooks); err != nil {
			return "", err
		}
		return "finalized", nil
	}

	// Every pre-commit phase rolls back. Actual directory presence is used as
	// well as the recorded phase because a crash can land after a durable rename
	// but before the following journal update.
	noMoveCleanup := false
	if originalExists {
		if _, err := verifyAuthenticatedSnapshotWithKey(ctx, originalDir, key, false); err != nil {
			return "", fmt.Errorf("authenticate rollback snapshot: %w", err)
		}
		if finalExists {
			if rebuiltExists {
				return "", fmt.Errorf("sanitize recovery has both installed and rebuilt trees")
			}
			if err := root.Rename(record.SnapshotID, filepath.Join(txName, sanitizeRebuiltName)); err != nil {
				return "", err
			}
		}
		if err := root.Rename(filepath.Join(txName, sanitizeOriginalName), record.SnapshotID); err != nil {
			if finalExists {
				_ = root.Rename(filepath.Join(txName, sanitizeRebuiltName), record.SnapshotID)
			}
			return "", err
		}
		if err := root.SyncDir(txName); err != nil {
			return "", err
		}
		record.Phase = sanitizePhaseRolledBack
		if err := writeSanitizeJournal(root, txName, &record); err != nil {
			return "", err
		}
		finalExists, originalExists, rebuiltExists = true, false, true
	} else {
		if !finalExists {
			return "", fmt.Errorf("sanitize recovery cannot find original snapshot")
		}
		if _, err := verifyAuthenticatedSnapshotWithKey(ctx, finalDir, key, false); err != nil {
			return "", err
		}
		writeRollbackPhase := true
		switch record.Phase {
		case sanitizePhaseBuilding, sanitizePhasePrepared:
			noMoveCleanup = true
			writeRollbackPhase = false
		case sanitizePhaseRolledBack:
			record.Phase = sanitizePhaseRolledBack
		case sanitizePhaseOriginalMoved, sanitizePhaseReplacementInstalled:
			if !rebuiltExists {
				return "", fmt.Errorf("sanitize rollback identity is ambiguous")
			}
			record.Phase = sanitizePhaseRolledBack
		default:
			return "", fmt.Errorf("unsupported sanitize recovery phase")
		}
		if writeRollbackPhase {
			if err := writeSanitizeJournal(root, txName, &record); err != nil {
				return "", err
			}
		}
	}
	if !cleanup {
		if noMoveCleanup {
			return "pending", nil
		}
		return "recovered", nil
	}
	// If neither namespace rename ever happened, the final tree is untouched and
	// may legitimately have changed while a long rebuild was in progress. Delete
	// only disposable transaction state; do not require the untouched source to
	// continue matching its older signed inventory. A true rollback still keeps
	// the rebuilt alternative until the restored original fully verifies.
	if !noMoveCleanup {
		if _, err := verifyAuthenticatedSnapshotWithKey(ctx, finalDir, key, true); err != nil {
			return "", fmt.Errorf("verify rolled-back snapshot: %w", err)
		}
	}
	if err := removeSanitizeTransaction(root, backupsRoot, txName, hooks); err != nil {
		return "", err
	}
	return "recovered", nil
}

func removeJournalLessSanitizeTransaction(root *privatefs.Root, txName string) (bool, error) {
	token := strings.TrimPrefix(txName, sanitizeTransactionPrefix)
	if txName != sanitizeTransactionPrefix+token || !sanitizeTokenPattern.MatchString(token) {
		return false, fmt.Errorf("nativebackup: invalid journal-less sanitize transaction")
	}
	entries, err := root.ReadDir(txName)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".aplexica-write-") || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return false, nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > sanitizeJournalMaxBytes+1 {
			return false, nil
		}
	}
	for _, entry := range entries {
		if err := root.RemoveRegular(filepath.Join(txName, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	if err := root.RemoveEmptyDir(txName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := root.SyncDir("."); err != nil {
		return false, err
	}
	return true, nil
}

func realChildDirectoryExists(root *privatefs.Root, parent, name string) (bool, error) {
	entries, err := root.ReadDir(parent)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() != name {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			return false, fmt.Errorf("nativebackup: sanitize transaction path is not a real directory")
		}
		return true, nil
	}
	return false, nil
}

func removeSanitizeTransaction(root *privatefs.Root, backupsRoot, txName string, hooks *sanitizeTestHooks) error {
	token := strings.TrimPrefix(txName, sanitizeTransactionPrefix)
	if txName != sanitizeTransactionPrefix+token || !sanitizeTokenPattern.MatchString(token) {
		return fmt.Errorf("nativebackup: refusing unsafe sanitize transaction removal")
	}
	remove := os.RemoveAll
	if hooks != nil && hooks.removeTree != nil {
		remove = hooks.removeTree
	}
	// Keep transaction.json until every potentially multi-gigabyte child tree
	// is gone. On Windows, antivirus/open handles can make RemoveAll partially
	// succeed; journal-last cleanup guarantees the next start can still recover
	// and retry instead of stranding an unauditable hidden tree.
	for _, child := range []string{sanitizeRebuiltName, sanitizeOriginalName} {
		exists, err := realChildDirectoryExists(root, txName, child)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if err := remove(filepath.Join(backupsRoot, txName, child)); err != nil {
			return err
		}
		if stillExists, err := realChildDirectoryExists(root, txName, child); err != nil {
			return err
		} else if stillExists {
			return fmt.Errorf("nativebackup: sanitize child cleanup did not remove %s", child)
		}
		if err := root.SyncDir(txName); err != nil {
			return err
		}
	}
	entries, err := root.ReadDir(txName)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".aplexica-write-") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("nativebackup: unsafe sanitize journal temp %q", entry.Name())
		}
		if err := root.RemoveRegular(filepath.Join(txName, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	entries, err = root.ReadDir(txName)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != sanitizeJournalName || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("nativebackup: sanitize transaction contains unexpected object %q", entry.Name())
		}
	}
	if err := root.RemoveRegular(filepath.Join(txName, sanitizeJournalName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := root.RemoveEmptyDir(txName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return root.SyncDir(".")
}
