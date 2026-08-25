package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/nativebackup"
	apiweb "github.com/aplexica/aplexica/internal/web/api"
)

const (
	// Cloud uploads can legitimately exceed the generic plugin timeout on a
	// modest uplink. Allow ten minutes for plugin/presign overhead plus the
	// encrypted payload's transfer time at a conservative 512 KiB/s. The floor
	// preserves the existing allowance for small backups; the ceiling still
	// bounds a hung plugin and is above the estimate for the largest valid
	// single-part upload.
	cloudBackupUploadTimeoutFloor                   = 30 * time.Minute
	cloudBackupUploadTimeoutOverhead                = 10 * time.Minute
	cloudBackupUploadTimeoutCeiling                 = 4 * time.Hour
	cloudBackupUploadAssumedMinBytesPerSecond int64 = 512 << 10
	cloudBackupDownloadMonitorInterval              = 50 * time.Millisecond
)

// cloudBackupSinglePutMaxBytes is the maximum object size accepted by the
// cloud plugin's current presigned PutObject upload. Larger backups require a
// multipart-upload protocol; fail locally until that protocol exists instead
// of spending more time invoking an upload that S3 will always reject.
const cloudBackupSinglePutMaxBytes int64 = 5 << 30

// errCloudPluginNotPaired gates cloud backups on pairing state: staging and
// encrypting a multi-GB snapshot is pointless work when the plugin cannot
// upload it. Matched with errors.Is by the scheduler so an unpaired device's
// scheduled runs are logged as quiet skips, not failures.
var errCloudPluginNotPaired = errors.New("cloud plugin is not paired")

// errCloudBackupTooLarge marks a permanent failure for the current cloud
// upload protocol. The scheduler uses errors.Is to avoid retrying the same
// multi-gigabyte snapshot every five minutes.
var errCloudBackupTooLarge = errors.New("cloud backup exceeds single-upload limit")

func validateCloudBackupUploadSize(size int64) error {
	if size <= cloudBackupSinglePutMaxBytes {
		return nil
	}
	return fmt.Errorf(
		"%w: encrypted backup is %d bytes; maximum is %d bytes; reduce the selected agents, use local backups, or enable multipart cloud uploads",
		errCloudBackupTooLarge,
		size,
		cloudBackupSinglePutMaxBytes,
	)
}

func cloudBackupUploadTimeout(size int64) time.Duration {
	if size <= 0 {
		return cloudBackupUploadTimeoutFloor
	}

	// Divide before converting to time.Duration so an untrusted or corrupt
	// size cannot overflow its nanosecond representation. Round up partial
	// seconds, then clamp before the duration multiplication.
	transferSeconds := size / cloudBackupUploadAssumedMinBytesPerSecond
	if size%cloudBackupUploadAssumedMinBytesPerSecond != 0 {
		transferSeconds++
	}
	maxTransferSeconds := int64((cloudBackupUploadTimeoutCeiling - cloudBackupUploadTimeoutOverhead) / time.Second)
	if transferSeconds >= maxTransferSeconds {
		return cloudBackupUploadTimeoutCeiling
	}

	timeout := cloudBackupUploadTimeoutOverhead + time.Duration(transferSeconds)*time.Second
	if timeout < cloudBackupUploadTimeoutFloor {
		return cloudBackupUploadTimeoutFloor
	}
	return timeout
}

// cloudBackupDownloadTimeout uses authenticated list metadata when it is
// available, but gives legacy/unknown-size objects the allowance required by
// the largest object the current single-part protocol can produce. A missing
// encryptedBytes field must never silently restore the old 30-minute timeout:
// at 512 KiB/s a valid 5-GiB backup takes almost three hours to transfer.
func cloudBackupDownloadTimeout(size int64) time.Duration {
	if size <= 0 || size > cloudBackupSinglePutMaxBytes {
		size = cloudBackupSinglePutMaxBytes
	}
	return cloudBackupUploadTimeout(size)
}

// cloudBackupDownloadPreflightSize selects the exact cloud object metadata
// returned by the authenticated plugin list call. Older plugin records omit
// encryptedBytes and return zero, which deliberately falls back to the maximum
// supported transfer timeout. Duplicate IDs are rejected because selecting one
// of two inconsistent remote records would make the size/digest preflight
// ambiguous.
func cloudBackupDownloadPreflightSize(backups []nativebackup.BackupInfo, backupID string) (int64, error) {
	found := false
	var size int64
	for _, backup := range backups {
		if backup.ID != backupID {
			continue
		}
		if found {
			return 0, fmt.Errorf("cloud backup download: duplicate backup id %q", backupID)
		}
		found = true
		size = backup.EncryptedBytes
	}
	if !found || size == 0 {
		return 0, nil
	}
	if size < 0 {
		return 0, fmt.Errorf("cloud backup download: invalid encrypted size %d", size)
	}
	if size > cloudBackupSinglePutMaxBytes {
		return 0, fmt.Errorf(
			"%w: encrypted backup is %d bytes; maximum is %d bytes",
			errCloudBackupTooLarge,
			size,
			cloudBackupSinglePutMaxBytes,
		)
	}
	return size, nil
}

func (n *nativeBackupsWebAccessor) preflightCloudBackupDownloadSize(ctx context.Context, backupID string) (int64, error) {
	backups, err := n.listCloudBackups(ctx)
	if err != nil {
		// Listing is an optimization and older plugins may not support it during a
		// restore. Preserve compatibility, but use the maximum valid-object timeout
		// rather than the unsafe historical 30-minute fallback.
		return 0, nil
	}
	return cloudBackupDownloadPreflightSize(backups, backupID)
}

// inspectCloudBackupDownload proves that path still names the exact regular
// inode/file-index created by the daemon for this download and that its current
// logical size is within the protocol cap. The plugin may write through the
// retained pathname, but it may not replace it with a link, special file, or a
// different filesystem object.
func inspectCloudBackupDownload(path string, expected os.FileInfo, maxBytes int64) (int64, error) {
	if expected == nil || !expected.Mode().IsRegular() || maxBytes <= 0 {
		return 0, fmt.Errorf("cloud backup download: invalid retained file guard")
	}
	current, err := os.Lstat(path)
	if err != nil {
		// A pathname that disappears between polls has already stopped naming
		// the retained output object. Classify that race with the same stable
		// identity-replacement error as a new inode/symlink; callers must cancel
		// either way, and tests should not depend on observing the tiny ENOENT
		// window between rename and replacement.
		if errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("cloud backup download: output changed identity or type")
		}
		return 0, fmt.Errorf("cloud backup download: inspect output: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(expected, current) {
		return 0, fmt.Errorf("cloud backup download: output changed identity or type")
	}
	if current.Size() < 0 || current.Size() > maxBytes {
		return 0, fmt.Errorf(
			"%w: downloaded encrypted backup is %d bytes; maximum is %d bytes",
			errCloudBackupTooLarge,
			current.Size(),
			maxBytes,
		)
	}
	return current.Size(), nil
}

func validateCloudBackupDownloadSizes(actual, preflight, reported, maxBytes int64) error {
	if actual <= 0 {
		return fmt.Errorf("cloud backup download: encrypted output is empty")
	}
	if actual > maxBytes {
		return fmt.Errorf(
			"%w: downloaded encrypted backup is %d bytes; maximum is %d bytes",
			errCloudBackupTooLarge,
			actual,
			maxBytes,
		)
	}
	for _, metadata := range []struct {
		label string
		size  int64
	}{{label: "preflight", size: preflight}, {label: "reported", size: reported}} {
		label, size := metadata.label, metadata.size
		if size < 0 {
			return fmt.Errorf("cloud backup download: invalid %s encrypted size %d", label, size)
		}
		if size > maxBytes {
			return fmt.Errorf(
				"%w: %s encrypted backup is %d bytes; maximum is %d bytes",
				errCloudBackupTooLarge,
				label,
				size,
				maxBytes,
			)
		}
		if size > 0 && size != actual {
			return fmt.Errorf("cloud backup download: %s encrypted size mismatch: metadata=%d file=%d", label, size, actual)
		}
	}
	return nil
}

// monitorCloudBackupDownload cancels the plugin process as soon as its exact
// output pathname changes identity/type or grows beyond the current single-put
// protocol ceiling. Post-exit validation closes the final poll race; this loop
// bounds disk consumption while a hung or stale plugin is still running.
func monitorCloudBackupDownload(path string, expected os.FileInfo, maxBytes int64, interval time.Duration, cancel context.CancelFunc, stop <-chan struct{}) <-chan error {
	result := make(chan error, 1)
	go func() {
		defer close(result)
		check := func() bool {
			if _, err := inspectCloudBackupDownload(path, expected, maxBytes); err != nil {
				result <- err
				if cancel != nil {
					cancel()
				}
				return false
			}
			return true
		}
		if !check() {
			return
		}
		if interval <= 0 {
			interval = cloudBackupDownloadMonitorInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if !check() {
					return
				}
			}
		}
	}()
	return result
}

// SweepCloudDownloads is the daemon-startup entry point for reclaiming
// encrypted downloads and extracted restore trees left when an earlier process
// died before its defers ran. cloudMu is the complete cloud-operation lifetime
// lease, so this can never delete the files of an active backup or restore.
func (m *nativeBackupManager) SweepCloudDownloads(lg nativeBackupsLogger) {
	if m == nil {
		return
	}
	m.cloudMu.Lock()
	defer m.cloudMu.Unlock()
	removed, err := m.sweepCloudDownloadsLocked()
	if lg == nil {
		return
	}
	if err != nil {
		lg.Warn("cloud-download sweep: cleanup failed", "err", err, "removed", removed)
		return
	}
	if removed > 0 {
		lg.Info("cloud-download sweep: reclaimed orphaned restore data", "removed", removed)
	}
}

// sweepCloudDownloadsLocked removes every child of .cloud-downloads. The
// directory is reserved exclusively for disposable encrypted objects and
// extracted restore staging, so retaining unknown children after a process
// upgrade would merely create another unbounded orphan class. Caller holds
// cloudMu; any failure is returned so restore allocation can fail closed.
func (m *nativeBackupManager) sweepCloudDownloadsLocked() (int, error) {
	root := m.cloudDownloadsRoot()
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("cloud download cleanup: inspect root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return 0, fmt.Errorf("cloud download cleanup: root is not a real directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("cloud download cleanup: enumerate: %w", err)
	}
	victims := make([]string, 0, len(entries))
	for _, entry := range entries {
		victims = append(victims, filepath.Join(root, entry.Name()))
	}
	sort.Strings(victims)
	removed := 0
	for _, victim := range victims {
		if err := os.RemoveAll(victim); err != nil {
			return removed, fmt.Errorf("cloud download cleanup: remove %s: %w", filepath.Base(victim), err)
		}
		if _, err := os.Lstat(victim); err == nil || !os.IsNotExist(err) {
			if err == nil {
				err = fmt.Errorf("path still exists")
			}
			return removed, fmt.Errorf("cloud download cleanup: verify removal of %s: %w", filepath.Base(victim), err)
		}
		removed++
	}
	return removed, nil
}

func ensureCloudDownloadsRoot(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("cloud download: create staging root: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("cloud download: inspect staging root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("cloud download: staging root is not a real directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("cloud download: secure staging root: %w", err)
	}
	return nil
}

func (n *nativeBackupsWebAccessor) cloudBackupStatus(ctx context.Context) nativebackup.CloudStatus {
	execPath, err := (&remoteWebAccessor{deps: n.deps}).remoteExecPath()
	if err != nil {
		return nativebackup.CloudStatus{Configured: n.deps.remoteCfg.Executable != "", Message: err.Error()}
	}
	remote := &remoteWebAccessor{deps: n.deps}
	paired, deviceID, accountID := queryPluginStatus(ctx, execPath, remote.prepareConfiguredRemotePluginCommand)
	if !paired {
		return nativebackup.CloudStatus{Configured: true, Message: "cloud plugin is not paired"}
	}
	return nativebackup.CloudStatus{
		Configured: true,
		Paired:     true,
		Available:  true,
		DeviceID:   deviceID,
		AccountID:  accountID,
		Message:    "encrypted cloud backups available",
	}
}

func (n *nativeBackupsWebAccessor) listCloudBackups(ctx context.Context) ([]nativebackup.BackupInfo, error) {
	execPath, err := (&remoteWebAccessor{deps: n.deps}).remoteExecPath()
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, remoteExecTimeout)
	defer cancel()
	prepared, err := (&remoteWebAccessor{deps: n.deps}).prepareConfiguredRemotePluginCommand(cctx, execPath, "--backup-list")
	if err != nil {
		return nil, fmt.Errorf("cloud backup list: plugin identity changed before launch: %w", err)
	}
	defer func() { _ = prepared.Close() }()
	out, err := prepared.Cmd().CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			trimmed = err.Error()
		}
		return nil, fmt.Errorf("%w: %s", apiweb.ErrRemoteNotConfigured, trimmed)
	}
	var backups []nativebackup.BackupInfo
	if err := json.Unmarshal(out, &backups); err != nil {
		return nil, fmt.Errorf("cloud backup list: parse plugin JSON: %w", err)
	}
	for i := range backups {
		backups[i].Location = "cloud"
		backups[i].Encrypted = true
	}
	return backups, nil
}

func (n *nativeBackupsWebAccessor) createCloudBackup(ctx context.Context, kind string, agents []string) (nativebackup.BackupInfo, error) {
	if n.deps.backupMgr == nil {
		return nativebackup.BackupInfo{}, fmt.Errorf("native backup manager not wired")
	}
	execPath, err := (&remoteWebAccessor{deps: n.deps}).remoteExecPath()
	if err != nil {
		return nativebackup.BackupInfo{}, err
	}
	// Pairing gate — BEFORE staging. An installed-but-unpaired plugin passes
	// the exec-path check and the local encryption key is auto-generated, so
	// without this check the daemon would copy + encrypt a multi-GB snapshot
	// every run only to fail (or misdirect) the upload. One cheap --status exec
	// replaces gigabytes of doomed staging I/O. A status-query failure counts
	// as unpaired: if the plugin can't even report status, it can't upload.
	if paired, _, _ := queryPluginStatus(ctx, execPath, (&remoteWebAccessor{deps: n.deps}).prepareConfiguredRemotePluginCommand); !paired {
		return nativebackup.BackupInfo{}, fmt.Errorf(
			"cloud backup: %w; pair this device via Connect to Cloud or switch the backup destination to local",
			errCloudPluginNotPaired)
	}
	// Serialize the complete staging lifetime, not merely the initial tree
	// copy. CreateCloudStagingContext's opMu is released when it returns, while
	// this method still needs the tree for archive/encrypt/upload. cloudMu also
	// excludes the startup orphan sweep and another scheduled/manual cloud run.
	n.deps.backupMgr.cloudMu.Lock()
	defer n.deps.backupMgr.cloudMu.Unlock()
	info, err := n.deps.backupMgr.CreateCloudStagingContext(ctx, kind, agents)
	if err != nil {
		return nativebackup.BackupInfo{}, err
	}
	// Capture the staging path in a dedicated local: info.Path is blanked below
	// (a cloud backup exposes no local path to callers), and the cleanup must not
	// depend on a field we intentionally zero. Removing the staged snapshot on
	// every terminal outcome — upload success OR failure — is what keeps
	// .cloud-staging from accumulating a ~5-6 GB dir per run until the disk fills.
	stagingPath := info.Path
	defer func() { _ = os.RemoveAll(stagingPath) }()

	if err := os.MkdirAll(n.deps.backupMgr.cloudStagingRoot(), 0o700); err != nil {
		return nativebackup.BackupInfo{}, err
	}
	encryptedFile, err := os.CreateTemp(n.deps.backupMgr.cloudStagingRoot(), info.ID+"-*.apxbk")
	if err != nil {
		return nativebackup.BackupInfo{}, err
	}
	encryptedPath := encryptedFile.Name()
	_ = encryptedFile.Close()
	defer func() { _ = os.Remove(encryptedPath) }()

	if err := n.deps.backupMgr.migrateLegacyCloudKey(); err != nil {
		return nativebackup.BackupInfo{}, fmt.Errorf("cloud backup key migration: %w", err)
	}
	meta, err := nativebackup.EncryptSnapshotDirContext(ctx, info.Path, encryptedPath, n.deps.backupMgr.cloudKeyPath())
	if err != nil {
		return nativebackup.BackupInfo{}, err
	}
	// Encryption has produced the complete compressed archive. The raw snapshot
	// can be several gigabytes while the archive is only a small fraction of that
	// size, and an upload may take minutes (or hang until its timeout). Reclaim the
	// raw copy before validation and network I/O so cloud backup disk usage is
	// bounded by one raw tree only during archive creation, never for the upload's
	// lifetime. The defer above remains as crash/error-path insurance.
	if err := os.RemoveAll(stagingPath); err != nil {
		return nativebackup.BackupInfo{}, fmt.Errorf("cloud backup: remove raw staging after encryption: %w", err)
	}
	if err := validateCloudBackupUploadSize(meta.EncryptedBytes); err != nil {
		return nativebackup.BackupInfo{}, err
	}
	info.Path = ""
	info.Location = "cloud"
	info.Encrypted = true
	info.Algorithm = meta.Algorithm
	info.EncryptedBytes = meta.EncryptedBytes
	info.CipherSHA256 = meta.CipherSHA256
	info.PlainSHA256 = meta.PlainSHA256
	metaFile, err := writeCloudBackupMetadata(n.deps.backupMgr.cloudStagingRoot(), info)
	if err != nil {
		return nativebackup.BackupInfo{}, err
	}
	defer func() { _ = os.Remove(metaFile) }()

	cctx, cancel := context.WithTimeout(ctx, cloudBackupUploadTimeout(meta.EncryptedBytes))
	defer cancel()
	// Staging can take minutes. Reauthenticate after it completes so a binary,
	// manifest, inventory, checkpoint, or configured-path substitution during
	// staging cannot become the upload process.
	prepared, err := (&remoteWebAccessor{deps: n.deps}).prepareConfiguredRemotePluginCommand(cctx, execPath, "--backup-upload", encryptedPath, "--backup-metadata", metaFile)
	if err != nil {
		return nativebackup.BackupInfo{}, fmt.Errorf("cloud backup upload: plugin identity changed before launch: %w", err)
	}
	defer func() { _ = prepared.Close() }()
	out, err := prepared.Cmd().CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			trimmed = err.Error()
		}
		return nativebackup.BackupInfo{}, fmt.Errorf("cloud backup upload failed: %s", trimmed)
	}
	var uploaded nativebackup.BackupInfo
	if err := json.Unmarshal(out, &uploaded); err != nil {
		return nativebackup.BackupInfo{}, fmt.Errorf("cloud backup upload: parse plugin JSON: %w", err)
	}
	if uploaded.Location == "" {
		uploaded.Location = "cloud"
	}
	uploaded.Encrypted = true
	return uploaded, nil
}

func (n *nativeBackupsWebAccessor) restoreCloudBackup(ctx context.Context, backupID, agent string) (nativebackup.RestoreResult, error) {
	if n.deps.backupMgr == nil {
		return nativebackup.RestoreResult{}, fmt.Errorf("native backup manager not wired")
	}
	// Serialize the full download -> decrypt -> native restore lifetime with the
	// startup orphan sweep and every other cloud operation. In particular, a
	// sweep that starts while this restore is active waits rather than deleting
	// its encrypted or extracted staging data.
	n.deps.backupMgr.cloudMu.Lock()
	defer n.deps.backupMgr.cloudMu.Unlock()
	// A killed process bypasses defers and can leave both a 5-GiB encrypted file
	// and a much larger extracted tree. Reclaim all such objects before admitting
	// another allocation; failure is terminal so repeated attempts cannot grow
	// this directory without bound.
	if _, err := n.deps.backupMgr.sweepCloudDownloadsLocked(); err != nil {
		return nativebackup.RestoreResult{}, fmt.Errorf("cloud backup restore: cleanup must complete before allocation: %w", err)
	}
	if err := ensureCloudDownloadsRoot(n.deps.backupMgr.cloudDownloadsRoot()); err != nil {
		return nativebackup.RestoreResult{}, err
	}
	execPath, err := (&remoteWebAccessor{deps: n.deps}).remoteExecPath()
	if err != nil {
		return nativebackup.RestoreResult{}, err
	}
	downloadSize, err := n.preflightCloudBackupDownloadSize(ctx, backupID)
	if err != nil {
		return nativebackup.RestoreResult{}, err
	}
	download, err := os.CreateTemp(n.deps.backupMgr.cloudDownloadsRoot(), backupID+"-*.apxbk")
	if err != nil {
		return nativebackup.RestoreResult{}, err
	}
	downloadPath := download.Name()
	// Retain the identity from the open handle before handing the pathname to
	// the plugin. On Windows, Stat/Lstat pathname results load their file ID
	// lazily when os.SameFile is first called; if the path has already been
	// replaced by then, that lazy lookup can accidentally identify the
	// replacement. File.Stat records the identity of this exact open object.
	downloadIdentity, statErr := download.Stat()
	closeErr := download.Close()
	if statErr != nil || closeErr != nil || !downloadIdentity.Mode().IsRegular() {
		_ = os.Remove(downloadPath)
		if statErr != nil {
			err = statErr
		} else if closeErr != nil {
			err = closeErr
		} else {
			err = fmt.Errorf("download output is not a regular file")
		}
		return nativebackup.RestoreResult{}, fmt.Errorf("cloud backup download: retain output identity: %w", err)
	}
	downloadPresent := true
	defer func() {
		if downloadPresent {
			_ = os.Remove(downloadPath)
		}
	}()

	cctx, cancel := context.WithTimeout(ctx, cloudBackupDownloadTimeout(downloadSize))
	defer cancel()
	prepared, err := (&remoteWebAccessor{deps: n.deps}).prepareConfiguredRemotePluginCommand(cctx, execPath, "--backup-download", backupID, "--backup-out", downloadPath)
	if err != nil {
		return nativebackup.RestoreResult{}, fmt.Errorf("cloud backup download: plugin identity changed before launch: %w", err)
	}
	defer func() { _ = prepared.Close() }()
	monitorStop := make(chan struct{})
	monitorResult := monitorCloudBackupDownload(
		downloadPath,
		downloadIdentity,
		cloudBackupSinglePutMaxBytes,
		cloudBackupDownloadMonitorInterval,
		cancel,
		monitorStop,
	)
	out, err := prepared.Cmd().CombinedOutput()
	close(monitorStop)
	monitorErr := <-monitorResult
	downloadedBytes, inspectErr := inspectCloudBackupDownload(downloadPath, downloadIdentity, cloudBackupSinglePutMaxBytes)
	if monitorErr != nil {
		return nativebackup.RestoreResult{}, monitorErr
	}
	if inspectErr != nil {
		return nativebackup.RestoreResult{}, inspectErr
	}
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			trimmed = err.Error()
		}
		return nativebackup.RestoreResult{}, fmt.Errorf("cloud backup download failed: %s", trimmed)
	}
	var info nativebackup.BackupInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nativebackup.RestoreResult{}, fmt.Errorf("cloud backup download: parse plugin JSON: %w", err)
	}
	if err := validateCloudBackupDownloadSizes(downloadedBytes, downloadSize, info.EncryptedBytes, cloudBackupSinglePutMaxBytes); err != nil {
		return nativebackup.RestoreResult{}, err
	}

	restoreDir, err := os.MkdirTemp(n.deps.backupMgr.cloudDownloadsRoot(), backupID+"-restore-*")
	if err != nil {
		return nativebackup.RestoreResult{}, err
	}
	defer func() { _ = os.RemoveAll(restoreDir) }()
	if err := n.deps.backupMgr.migrateLegacyCloudKey(); err != nil {
		return nativebackup.RestoreResult{}, fmt.Errorf("cloud backup key migration: %w", err)
	}
	meta, err := nativebackup.DecryptSnapshotArchive(downloadPath, restoreDir, n.deps.backupMgr.cloudKeyPath())
	if err != nil {
		return nativebackup.RestoreResult{}, err
	}
	// DecryptSnapshotArchive has authenticated and closed the complete encrypted
	// input. Its bytes are no longer needed for hash comparison or native restore,
	// so release them before the potentially long restore transaction begins.
	if err := os.Remove(downloadPath); err != nil {
		return nativebackup.RestoreResult{}, fmt.Errorf("cloud backup restore: remove authenticated download: %w", err)
	}
	downloadPresent = false
	if info.CipherSHA256 != "" && !strings.EqualFold(info.CipherSHA256, meta.CipherSHA256) {
		return nativebackup.RestoreResult{}, fmt.Errorf("cloud backup hash mismatch after decrypt: metadata=%s file=%s", info.CipherSHA256, meta.CipherSHA256)
	}
	return n.deps.backupMgr.Restore(ctx, restoreDir, agent)
}

func (n *nativeBackupsWebAccessor) deleteCloudBackup(ctx context.Context, backupID string) (nativebackup.BackupInfo, error) {
	execPath, err := (&remoteWebAccessor{deps: n.deps}).remoteExecPath()
	if err != nil {
		return nativebackup.BackupInfo{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, remoteExecTimeout)
	defer cancel()
	prepared, err := (&remoteWebAccessor{deps: n.deps}).prepareConfiguredRemotePluginCommand(cctx, execPath, "--backup-delete", backupID)
	if err != nil {
		return nativebackup.BackupInfo{}, fmt.Errorf("cloud backup delete: plugin identity changed before launch: %w", err)
	}
	defer func() { _ = prepared.Close() }()
	out, err := prepared.Cmd().CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			trimmed = err.Error()
		}
		return nativebackup.BackupInfo{}, fmt.Errorf("cloud backup delete failed: %s", trimmed)
	}
	var info nativebackup.BackupInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nativebackup.BackupInfo{}, fmt.Errorf("cloud backup delete: parse plugin JSON: %w", err)
	}
	info.Location = "cloud"
	info.Encrypted = true
	return info, nil
}

func writeCloudBackupMetadata(dir string, info nativebackup.BackupInfo) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, info.ID+".metadata.json")
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
