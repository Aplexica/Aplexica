package nativebackup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/filelock"
	"github.com/aplexica/aplexica/internal/privatefs"
)

type NativeRestoreLimits struct {
	MaxFiles      int
	MaxTotalBytes int64
	MaxFileBytes  int64
}

func DefaultNativeRestoreLimits() NativeRestoreLimits {
	return NativeRestoreLimits{MaxFiles: 100000, MaxTotalBytes: 1 << 40, MaxFileBytes: 64 << 30}
}

type NativeRestoreLease interface {
	CheckCurrent() error
	Close() error
}
type NativeTarget struct {
	Agent        string
	Root         string
	RelativePath string
}
type NativeRestoreCoordinator interface {
	AcquireRestoreLease(context.Context, []NativeTarget) (NativeRestoreLease, error)
}
type NativeRestoreOptions struct {
	Agent               string
	CurrentAgentRoots   []AgentRoots
	Coordinator         NativeRestoreCoordinator
	AllowLegacyUnsigned bool
	Limits              NativeRestoreLimits
	ManifestKeyPath     string
	// ExcludeTarget applies an agent-aware machine-state policy after the
	// authenticated manifest target has been resolved beneath an authorized
	// native root. It complements exact ExcludePaths for layouts whose safe
	// exclusion depends on a bounded path shape rather than a live directory.
	ExcludeTarget func(NativeTarget) bool
	// snapshotPreRestore is a fault-injection seam for package tests. Production
	// callers leave it nil and use createAuthenticatedPreRestoreSnapshot.
	snapshotPreRestore func(context.Context, []AgentRoots, string, string) error
}

type secureRestoreJob struct {
	backupRel, target string
	want              FileEntry
	root              string
	rootID            string
	finalRel          string
	tempRel           string
	rollbackRel       string
	redactionKind     FileRedactionKind
	finalBytes        int64
	finalSHA256       string
}

func stageRetainedRestoreFile(ctx context.Context, sourceRoot *privatefs.Root, sourceRel string, targetRoot *privatefs.Root, targetRel string, want FileEntry) (tempRel string, retErr error) {
	if want.Bytes < 0 || want.SHA256 == "" {
		return "", fmt.Errorf("nativebackup: invalid manifest digest")
	}
	parent := filepath.Dir(targetRel)
	if parent != "." {
		if err := targetRoot.EnsureDir(parent, privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly}); err != nil {
			return "", err
		}
	}
	in, err := sourceRoot.OpenReadRegularIntegrity(sourceRel)
	if err != nil {
		return "", err
	}
	defer in.Close()
	before, err := in.Stat()
	if err != nil || before.Size() != want.Bytes {
		return "", fmt.Errorf("nativebackup: source size mismatch")
	}
	out, tempRel, err := targetRoot.CreateTemp(parent, ".aplexica-restore-verified-")
	if err != nil {
		return "", err
	}
	committed := false
	defer func() {
		_ = out.Close()
		if !committed {
			_ = targetRoot.RemoveRegular(tempRel)
		}
	}()
	h := sha256.New()
	n, err := copyWithContext(ctx, io.MultiWriter(out, h), io.LimitReader(in, want.Bytes+1))
	after, statErr := in.Stat()
	if err == nil && (statErr != nil || !os.SameFile(before, after) || after.Size() != want.Bytes || n != want.Bytes) {
		err = fmt.Errorf("nativebackup: source identity or size changed")
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != want.SHA256 {
		err = fmt.Errorf("nativebackup: source digest mismatch")
	}
	if err == nil {
		err = out.Chmod(filePerm)
	}
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	committed = true
	return tempRel, nil
}

func stageRedactedRestoreFile(ctx context.Context, sourceRoot *privatefs.Root, sourceRel string, targetRoot *privatefs.Root, targetRel string, want FileEntry, kind FileRedactionKind) (tempRel string, finalBytes int64, finalSHA256 string, retErr error) {
	backup, err := readVerifiedRedactionRestoreSource(ctx, sourceRoot, sourceRel, want)
	if err != nil {
		return "", 0, "", err
	}
	// Sanitize again at restore time so an authenticated snapshot made before
	// typed redaction cannot reintroduce its old machine credentials.
	output, err := redactBackupFile(kind, backup)
	if err != nil {
		return "", 0, "", fmt.Errorf("nativebackup: redact restore source %q: %w", want.Path, err)
	}
	live, exists, err := readLiveRedactionRestoreTarget(ctx, targetRoot, targetRel)
	if err != nil {
		return "", 0, "", err
	}
	if exists {
		output, err = mergeRedactedBackupFile(kind, output, live)
		if err != nil {
			return "", 0, "", err
		}
	}
	if int64(len(output)) > backupRedactionMaxInputBytes {
		return "", 0, "", fmt.Errorf("nativebackup: merged redacted restore file exceeds limit")
	}
	tempRel, digest, err := stageRestoreBytes(ctx, targetRoot, targetRel, output)
	if err != nil {
		return "", 0, "", err
	}
	return tempRel, int64(len(output)), hex.EncodeToString(digest[:]), nil
}

func readVerifiedRedactionRestoreSource(ctx context.Context, root *privatefs.Root, rel string, want FileEntry) ([]byte, error) {
	if want.Bytes < 0 || want.Bytes > backupRedactionMaxInputBytes || want.SHA256 == "" {
		return nil, fmt.Errorf("nativebackup: invalid redaction restore manifest entry")
	}
	f, err := root.OpenReadRegularIntegrity(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || before.Size() != want.Bytes {
		return nil, fmt.Errorf("nativebackup: redaction restore source size mismatch")
	}
	var buf bytes.Buffer
	h := sha256.New()
	n, copyErr := copyWithContext(ctx, io.MultiWriter(&buf, h), io.LimitReader(f, backupRedactionMaxInputBytes+1))
	after, statErr := f.Stat()
	if copyErr != nil {
		return nil, copyErr
	}
	if statErr != nil || !os.SameFile(before, after) || n != want.Bytes || after.Size() != want.Bytes {
		return nil, fmt.Errorf("nativebackup: redaction restore source changed")
	}
	if hex.EncodeToString(h.Sum(nil)) != want.SHA256 {
		return nil, fmt.Errorf("nativebackup: redaction restore source digest mismatch")
	}
	return buf.Bytes(), nil
}

func readLiveRedactionRestoreTarget(ctx context.Context, root *privatefs.Root, rel string) ([]byte, bool, error) {
	f, err := root.OpenReadRegularIntegrity(rel)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || before.Size() < 0 || before.Size() > backupRedactionMaxInputBytes {
		return nil, false, fmt.Errorf("nativebackup: unsafe or oversized live mixed config")
	}
	var buf bytes.Buffer
	n, copyErr := copyWithContext(ctx, &buf, io.LimitReader(f, backupRedactionMaxInputBytes+1))
	after, statErr := f.Stat()
	if copyErr != nil {
		return nil, false, copyErr
	}
	if statErr != nil || !os.SameFile(before, after) || n != before.Size() || after.Size() != before.Size() {
		return nil, false, fmt.Errorf("nativebackup: live mixed config changed during restore staging")
	}
	return buf.Bytes(), true, nil
}

func stageRestoreBytes(ctx context.Context, targetRoot *privatefs.Root, targetRel string, data []byte) (tempRel string, digest [sha256.Size]byte, retErr error) {
	parent := filepath.Dir(targetRel)
	if parent != "." {
		if err := targetRoot.EnsureDir(parent, privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly}); err != nil {
			return "", digest, err
		}
	}
	out, tempRel, err := targetRoot.CreateTemp(parent, ".aplexica-restore-verified-")
	if err != nil {
		return "", digest, err
	}
	committed := false
	defer func() {
		_ = out.Close()
		if !committed {
			_ = targetRoot.RemoveRegular(tempRel)
		}
	}()
	h := sha256.New()
	n, err := copyWithContext(ctx, io.MultiWriter(out, h), bytes.NewReader(data))
	if err == nil && n != int64(len(data)) {
		err = fmt.Errorf("nativebackup: short redacted restore staging write")
	}
	if err == nil {
		err = out.Chmod(filePerm)
	}
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", digest, err
	}
	copy(digest[:], h.Sum(nil))
	committed = true
	return tempRel, digest, nil
}

func restoreRoots(current []AgentRoots) (map[string]string, map[string]*privatefs.Root, error) {
	set := map[string]bool{}
	for _, agent := range current {
		for _, value := range agent.Roots {
			abs, err := filepath.Abs(value)
			if err != nil {
				return nil, nil, err
			}
			set[filepath.Clean(abs)] = true
		}
	}
	paths := make([]string, 0, len(set))
	for value := range set {
		paths = append(paths, value)
	}
	sort.Strings(paths)
	ids := make(map[string]string, len(paths))
	roots := make(map[string]*privatefs.Root, len(paths))
	for i, value := range paths {
		id := fmt.Sprintf("root-%06d", i+1)
		root, err := privatefs.OpenNativeRoot(value, privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly})
		if err != nil {
			for _, opened := range roots {
				_ = opened.Close()
			}
			return nil, nil, err
		}
		ids[value], roots[id] = id, root
	}
	return ids, roots, nil
}

func closeRestoreRoots(roots map[string]*privatefs.Root) {
	for _, root := range roots {
		_ = root.Close()
	}
}

func digestRegular(root *privatefs.Root, rel string) ([32]byte, bool, error) {
	f, err := root.OpenReadRegularIntegrity(rel)
	if errors.Is(err, os.ErrNotExist) {
		return [32]byte{}, false, nil
	}
	if err != nil {
		return [32]byte{}, false, err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil {
		return [32]byte{}, false, copyErr
	}
	if closeErr != nil {
		return [32]byte{}, false, closeErr
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest, true, nil
}

func resolveAuthorizedTarget(agent string, fe FileEntry, current []AgentRoots) (string, NativeTarget, error) {
	var matches []struct {
		target    string
		nt        NativeTarget
		prefixLen int
	}
	for _, ag := range current {
		if ag.Name != agent {
			continue
		}
		for _, root := range ag.Roots {
			abs, err := filepath.Abs(root)
			if err != nil {
				return "", NativeTarget{}, err
			}
			abs = filepath.Clean(abs)
			prefix := agent + "/" + filepath.ToSlash(relativize(abs))
			if fe.Path != prefix && !strings.HasPrefix(fe.Path, prefix+"/") {
				continue
			}
			suffix := strings.TrimPrefix(strings.TrimPrefix(fe.Path, prefix), "/")
			rel := filepath.Clean(filepath.FromSlash(suffix))
			if rel == "." {
				rel = ""
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				return "", NativeTarget{}, fmt.Errorf("nativebackup: path escapes current root")
			}
			matches = append(matches, struct {
				target    string
				nt        NativeTarget
				prefixLen int
			}{filepath.Join(abs, rel), NativeTarget{Agent: agent, Root: abs, RelativePath: rel}, len(prefix)})
		}
	}
	if len(matches) != 1 {
		return "", NativeTarget{}, fmt.Errorf("nativebackup: manifest path has %d authorized root matches", len(matches))
	}
	return matches[0].target, matches[0].nt, nil
}

func restoreTargetExcluded(target string, nt NativeTarget, exclusions []string) bool {
	if pathExcluded(target, exclusions) {
		return true
	}
	for _, component := range strings.Split(filepath.Clean(nt.RelativePath), string(filepath.Separator)) {
		if genericRebuildablePath(component) {
			return true
		}
	}
	return false
}

func RestoreWithOptions(ctx context.Context, backupDir string, opts NativeRestoreOptions) (RestoreResult, error) {
	man, err := readManifestStrict(backupDir)
	if err != nil {
		return RestoreResult{}, err
	}
	if opts.Limits == (NativeRestoreLimits{}) {
		opts.Limits = DefaultNativeRestoreLimits()
	}
	keyPath := opts.ManifestKeyPath
	if keyPath == "" {
		keyPath = manifestKeyPathForBackupDir(backupDir)
	}
	if man.SchemaVersion == 2 {
		if err := VerifyManifestWithKeyPath(man, keyPath); err != nil {
			return RestoreResult{}, err
		}
	} else if !opts.AllowLegacyUnsigned {
		return RestoreResult{}, fmt.Errorf("nativebackup: legacy unsigned manifest refused")
	}
	validatedExcludes := make(map[string][]string, len(opts.CurrentAgentRoots))
	validatedRedactions := make(map[string]map[string]FileRedactionKind, len(opts.CurrentAgentRoots))
	for _, current := range opts.CurrentAgentRoots {
		exclusions, err := validateExcludePaths(current.Roots, current.ExcludePaths)
		if err != nil {
			return RestoreResult{}, fmt.Errorf("nativebackup: restore agent %q exclusions: %w", current.Name, err)
		}
		validatedExcludes[current.Name] = append(validatedExcludes[current.Name], exclusions...)
		redactions, err := validateRedactionPaths(current.Roots, current.RedactFiles)
		if err != nil {
			return RestoreResult{}, fmt.Errorf("nativebackup: restore agent %q redactions: %w", current.Name, err)
		}
		if err := mergeRedactionPolicies(validatedRedactions, current.Name, redactions); err != nil {
			return RestoreResult{}, err
		}
	}
	var jobs []secureRestoreJob
	var targets []NativeTarget
	seen := map[string]bool{}
	var total int64
	for _, ag := range man.Agents {
		if opts.Agent != "" && ag.Name != opts.Agent {
			continue
		}
		if ag.Name == "" || strings.ContainsAny(ag.Name, "/\\") {
			return RestoreResult{}, fmt.Errorf("nativebackup: unsafe agent name")
		}
		if man.SchemaVersion == 2 {
			allowed := map[string]bool{}
			for _, current := range opts.CurrentAgentRoots {
				if current.Name == ag.Name {
					for _, r := range current.Roots {
						abs, e := filepath.Abs(r)
						if e != nil {
							return RestoreResult{}, e
						}
						allowed[filepath.Clean(abs)] = true
					}
				}
			}
			if len(ag.SourceRoots) == 0 {
				return RestoreResult{}, fmt.Errorf("nativebackup: manifest source roots missing")
			}
			seenRoots := map[string]bool{}
			for _, r := range ag.SourceRoots {
				if !filepath.IsAbs(r) || filepath.Clean(r) != r || !allowed[r] || seenRoots[r] {
					return RestoreResult{}, fmt.Errorf("nativebackup: manifest source root is not currently authorized")
				}
				seenRoots[r] = true
			}
		}
		for _, fe := range ag.Roots {
			target, nt, err := resolveAuthorizedTarget(ag.Name, fe, opts.CurrentAgentRoots)
			if err != nil {
				return RestoreResult{}, err
			}
			// Old authenticated snapshots can predate the exclusion policy. Apply
			// today's policy at restore time as well as snapshot time so a rollback
			// never reinstalls stale machine credentials, caches, or generated
			// dependency trees that are deliberately absent from new backups.
			if restoreTargetExcluded(target, nt, validatedExcludes[ag.Name]) ||
				(opts.ExcludeTarget != nil && opts.ExcludeTarget(nt)) {
				continue
			}
			if fe.Bytes < 0 || fe.Bytes > opts.Limits.MaxFileBytes || total > opts.Limits.MaxTotalBytes-fe.Bytes {
				return RestoreResult{}, fmt.Errorf("nativebackup: restore limits exceeded")
			}
			if seen[target] {
				return RestoreResult{}, fmt.Errorf("nativebackup: duplicate restore target")
			}
			seen[target] = true
			total += fe.Bytes
			jobs = append(jobs, secureRestoreJob{
				backupRel:     filepath.FromSlash(fe.Path),
				target:        target,
				want:          fe,
				root:          nt.Root,
				finalRel:      nt.RelativePath,
				redactionKind: validatedRedactions[ag.Name][manifestTargetKey(target)],
				finalBytes:    fe.Bytes,
				finalSHA256:   fe.SHA256,
			})
			targets = append(targets, nt)
			if len(jobs) > opts.Limits.MaxFiles {
				return RestoreResult{}, fmt.Errorf("nativebackup: restore file limit exceeded")
			}
		}
	}
	if len(jobs) == 0 {
		return RestoreResult{}, fmt.Errorf("nativebackup: no authorized restore entries")
	}
	if opts.Coordinator == nil {
		return RestoreResult{}, fmt.Errorf("nativebackup: restore coordinator required")
	}
	lease, err := opts.Coordinator.AcquireRestoreLease(ctx, targets)
	if err != nil {
		return RestoreResult{}, err
	}
	defer lease.Close()
	rootIDs, roots, err := restoreRoots(opts.CurrentAgentRoots)
	if err != nil {
		return RestoreResult{}, err
	}
	defer closeRestoreRoots(roots)
	backupRoot, err := privatefs.OpenNativeRoot(backupDir, privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly})
	if err != nil {
		return RestoreResult{}, err
	}
	defer backupRoot.Close()
	for i := range jobs {
		jobs[i].rootID = rootIDs[jobs[i].root]
		if jobs[i].rootID == "" || jobs[i].finalRel == "" {
			return RestoreResult{}, fmt.Errorf("nativebackup: restore target root unavailable")
		}
	}
	controlDir := filepath.Join(filepath.Dir(filepath.Clean(backupDir)), ".aplexica-native-restore-state")
	if err := privatefs.EnsureDir(controlDir, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return RestoreResult{}, err
	}
	control, err := privatefs.OpenRoot(controlDir, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return RestoreResult{}, err
	}
	defer control.Close()
	if err := privatefs.RecoverJournals(ctx, control, roots, "native-restore", nil); err != nil {
		return RestoreResult{}, fmt.Errorf("nativebackup: recover interrupted restore: %w", err)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].target < jobs[j].target })
	var stagedTotal int64
	for i := range jobs {
		if err := lease.CheckCurrent(); err != nil {
			return RestoreResult{}, err
		}
		if jobs[i].redactionKind != "" {
			jobs[i].tempRel, jobs[i].finalBytes, jobs[i].finalSHA256, err = stageRedactedRestoreFile(
				ctx, backupRoot, jobs[i].backupRel, roots[jobs[i].rootID], jobs[i].finalRel, jobs[i].want, jobs[i].redactionKind)
		} else {
			jobs[i].tempRel, err = stageRetainedRestoreFile(ctx, backupRoot, jobs[i].backupRel, roots[jobs[i].rootID], jobs[i].finalRel, jobs[i].want)
		}
		if err != nil {
			for j := 0; j < i; j++ {
				_ = roots[jobs[j].rootID].RemoveRegular(jobs[j].tempRel)
			}
			return RestoreResult{}, err
		}
		if jobs[i].finalBytes < 0 || jobs[i].finalBytes > opts.Limits.MaxFileBytes || stagedTotal > opts.Limits.MaxTotalBytes-jobs[i].finalBytes {
			_ = roots[jobs[i].rootID].RemoveRegular(jobs[i].tempRel)
			for j := 0; j < i; j++ {
				_ = roots[jobs[j].rootID].RemoveRegular(jobs[j].tempRel)
			}
			return RestoreResult{}, fmt.Errorf("nativebackup: merged restore limits exceeded")
		}
		stagedTotal += jobs[i].finalBytes
	}
	defer func() {
		for _, j := range jobs {
			_ = roots[j.rootID].RemoveRegular(j.tempRel)
		}
	}()
	if err := lease.CheckCurrent(); err != nil {
		return RestoreResult{}, err
	}
	backupsRoot := filepath.Dir(filepath.Clean(backupDir))
	// Make room before allocating another full undo tree. Failure is terminal:
	// adding a multi-gigabyte snapshot while old history cannot be reclaimed
	// would recreate the unbounded-growth condition this guard prevents. The
	// source is preserved when the user is itself restoring an older undo point.
	if _, err := PrunePreRestoreHistory(ctx, backupsRoot, MaxPreRestoreSnapshots-1, backupDir); err != nil {
		return RestoreResult{}, err
	}
	preDir, err := uniquePreRestoreDir(backupsRoot)
	if err != nil {
		return RestoreResult{}, err
	}
	keepPreRestore := false
	liveMutationStarted := false
	defer func() {
		// Before the first native rename this tree is only allocation residue;
		// an ordinary error/cancellation must not retain it. Once mutation starts,
		// preserve the undo point even if rollback reports an error.
		if !keepPreRestore && !liveMutationStarted {
			_ = os.RemoveAll(preDir)
		}
	}()
	selected := opts.CurrentAgentRoots
	if opts.Agent != "" {
		selected = nil
		for _, a := range opts.CurrentAgentRoots {
			if a.Name == opts.Agent {
				selected = append(selected, a)
			}
		}
	}
	snapshotPreRestore := opts.snapshotPreRestore
	if snapshotPreRestore == nil {
		snapshotPreRestore = createAuthenticatedPreRestoreSnapshot
	}
	err = snapshotPreRestore(ctx, selected, preDir, keyPath)
	if err != nil {
		return RestoreResult{}, err
	}
	// Record every target, staged file, prior digest, and rollback name before
	// the first live rename. Recovery rolls an applying transaction back and
	// rolls a state-committed transaction forward after verifying all digests.
	plan := privatefs.JournalPlan{Kind: "native-restore", TransactionID: acf.NewID(), NativePaths: true, Entries: make([]privatefs.JournalEntry, len(jobs))}
	for i := range jobs {
		j := &jobs[i]
		finalDigest, err := hex.DecodeString(j.finalSHA256)
		if err != nil || len(finalDigest) != sha256.Size {
			return RestoreResult{}, fmt.Errorf("nativebackup: invalid manifest digest")
		}
		var expectedFinal [32]byte
		copy(expectedFinal[:], finalDigest)
		rollbackDigest, existed, err := digestRegular(roots[j.rootID], j.finalRel)
		if err != nil {
			return RestoreResult{}, err
		}
		if existed {
			j.rollbackRel = filepath.Join(filepath.Dir(j.finalRel), ".aplexica-restore-rollback-"+randomHex(8))
		}
		operation := "install"
		if existed {
			operation = "replace"
		}
		plan.Entries[i] = privatefs.JournalEntry{RootID: j.rootID, ObjectKind: "file", Operation: operation, FinalRel: j.finalRel, TempRel: j.tempRel, RollbackRel: j.rollbackRel, FinalExisted: existed, CreatedByTransaction: !existed, ExpectedFinalSHA256: expectedFinal, ExpectedRollbackSHA256: rollbackDigest}
	}
	journal, err := privatefs.BeginJournal(control, plan)
	if err != nil {
		return RestoreResult{}, err
	}
	rollback := func(cause error) error {
		if recoverErr := privatefs.RecoverJournals(ctx, control, roots, "native-restore", nil); recoverErr != nil {
			return errors.Join(cause, fmt.Errorf("nativebackup: restore rollback blocked: %w", recoverErr))
		}
		return cause
	}
	res := RestoreResult{PreRestoreDir: preDir}
	for i := range jobs {
		if err := lease.CheckCurrent(); err != nil {
			return RestoreResult{}, rollback(err)
		}
		j, entry := &jobs[i], plan.Entries[i]
		root := roots[j.rootID]
		liveMutationStarted = true
		if entry.FinalExisted {
			if err := root.Rename(entry.FinalRel, entry.RollbackRel); err != nil {
				return RestoreResult{}, rollback(err)
			}
		}
		if err := root.Rename(entry.TempRel, entry.FinalRel); err != nil {
			return RestoreResult{}, rollback(err)
		}
		if err := journal.MarkApplied(i); err != nil {
			return RestoreResult{}, rollback(err)
		}
		res.Files = append(res.Files, FileResult{Path: j.target, Bytes: j.finalBytes, OK: true})
	}
	if err := journal.MarkStateCommitted(); err != nil {
		return RestoreResult{}, err
	}
	for _, entry := range plan.Entries {
		if entry.RollbackRel != "" {
			if err := roots[entry.RootID].RemoveRegular(entry.RollbackRel); err != nil && !errors.Is(err, os.ErrNotExist) {
				return RestoreResult{}, err
			}
		}
	}
	if err := journal.CloseAndRemove(); err != nil {
		return RestoreResult{}, err
	}
	keepPreRestore = true
	return res, nil
}

func createAuthenticatedPreRestoreSnapshot(ctx context.Context, selected []AgentRoots, preDir, keyPath string) error {
	preMan, err := snapshotContext(ctx, selected, preDir, false)
	if err != nil {
		return err
	}
	preMan.SchemaVersion = 2
	if err := signManifestWithKeyPath(&preMan, keyPath); err != nil {
		return err
	}
	return writeManifest(preDir, preMan)
}

func readManifestStrict(backupDir string) (Manifest, error) {
	abs, err := filepath.Abs(backupDir)
	if err != nil {
		return Manifest{}, err
	}
	root, err := privatefs.OpenRoot(abs, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return Manifest{}, err
	}
	defer root.Close()
	f, err := root.OpenReadRegularRepair(ManifestName)
	if err != nil {
		return Manifest{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	_ = f.Close()
	if err != nil || len(b) > 16<<20 {
		return Manifest{}, fmt.Errorf("nativebackup: manifest limit exceeded")
	}
	if err := rejectDuplicateJSONKeys(b); err != nil {
		return Manifest{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return Manifest{}, fmt.Errorf("nativebackup: trailing manifest data")
	}
	return m, nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var value func() error
	value = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k, ok := kt.(string)
				if !ok || seen[k] {
					return fmt.Errorf("nativebackup: duplicate manifest key")
				}
				seen[k] = true
				if err := value(); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim('}') {
				return fmt.Errorf("nativebackup: malformed manifest object")
			}
		case '[':
			for dec.More() {
				if err := value(); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim(']') {
				return fmt.Errorf("nativebackup: malformed manifest array")
			}
		default:
			return fmt.Errorf("nativebackup: invalid manifest delimiter")
		}
		return nil
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("nativebackup: trailing manifest data")
	}
	return nil
}

type localRestoreLease struct{ lock *filelock.Lock }

func (l *localRestoreLease) CheckCurrent() error {
	if l == nil || l.lock == nil {
		return fmt.Errorf("nativebackup: restore lease closed")
	}
	return nil
}
func (l *localRestoreLease) Close() error {
	if l == nil || l.lock == nil {
		return nil
	}
	err := l.lock.Close()
	l.lock = nil
	return err
}

type LocalRestoreCoordinator struct{ LockPath string }

func (l LocalRestoreCoordinator) AcquireRestoreLease(_ context.Context, _ []NativeTarget) (NativeRestoreLease, error) {
	if l.LockPath == "" {
		return nil, fmt.Errorf("nativebackup: local restore lock path required")
	}
	if err := os.MkdirAll(filepath.Dir(l.LockPath), 0o700); err != nil {
		return nil, err
	}
	lock, err := filelock.Acquire(l.LockPath, 10*time.Second)
	if err != nil {
		return nil, err
	}
	return &localRestoreLease{lock: lock}, nil
}

func randomHex(n int) string { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }
