//go:build !windows

// Regression coverage for the ~/.aplexica/backups/.cloud-staging disk leak:
// scheduled cloud backups staged a full ~5-6 GB snapshot per run and never
// deleted it, filling the disk. These tests pin the two guarantees of the fix:
//  1. createCloudBackup removes the staged snapshot dir on every terminal
//     outcome (upload success AND failure), and
//  2. the daemon reclaims/bounds orphaned staging dirs via a sweep.
//
// Unix-only, mirroring native_backups_test.go: the snapshot/restore round-trip
// reconstructs absolute paths from a single filesystem root (Windows multi-
// volume support is a documented V1 follow-up), and the fake plugin is a /bin/sh
// stub.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/nativebackup"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/stretchr/testify/require"
)

type checkingRestoreCoordinator struct {
	check func()
	err   error
}

func (c *checkingRestoreCoordinator) AcquireRestoreLease(context.Context, []nativebackup.NativeTarget) (nativebackup.NativeRestoreLease, error) {
	if c.check != nil {
		c.check()
	}
	return nil, c.err
}

// writeFakeCloudPlugin writes an executable stub that stands in for the remote
// cloud plugin. It answers `--status` with a paired/unpaired report (the format
// queryPluginStatus parses), answers `--backup-upload` with a valid (empty)
// BackupInfo JSON or an exit-1 failure per failUpload, and appends every
// invocation's args to `<script>.calls` so tests can assert which plugin
// commands ran. The download branch records whether pre-allocation cleanup left
// a seeded orphan visible to the plugin.
func writeFakeCloudPlugin(t *testing.T, dir string, paired, failUpload bool) string {
	t.Helper()
	path := filepath.Join(dir, "fake-cloud-plugin.sh")
	statusLines := "echo 'paired: no'"
	if paired {
		statusLines = "echo 'paired: yes'; echo 'device_id: dev-test'; echo 'account_id: acct-test'"
	}
	upload := "echo '{}'"
	if failUpload {
		upload = "echo 'upload refused' 1>&2; exit 1"
	}
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> \"" + path + ".calls\"\n" +
		"case \"$1\" in\n" +
		"  --status) " + statusLines + " ;;\n" +
		"  --backup-upload)\n" +
		"    find \"$(dirname \"$0\")/.aplexica/backups/.cloud-staging\" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l | tr -d ' ' > \"$0.staging-count\"\n" +
		"    " + upload + "\n" +
		"    ;;\n" +
		"  --backup-list) echo '[]' ;;\n" +
		"  --backup-download)\n" +
		"    if test -e \"$(dirname \"$0\")/.aplexica/backups/.cloud-downloads/orphan-marker\"; then echo yes > \"$0.download-orphan-present\"; else echo no > \"$0.download-orphan-present\"; fi\n" +
		"    echo '{}'\n" +
		"    ;;\n" +
		"  *) echo '{}' ;;\n" +
		"esac\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func writeFakeCloudDownloadPlugin(t *testing.T, dir string, archive []byte, info nativebackup.BackupInfo) string {
	t.Helper()
	path := filepath.Join(dir, "fake-cloud-download-plugin.sh")
	fixture := path + ".fixture.apxbk"
	require.NoError(t, os.WriteFile(fixture, archive, 0o600))
	listJSON := fmt.Sprintf(`[{"id":%q,"encryptedBytes":%d}]`, info.ID, info.EncryptedBytes)
	downloadJSON := fmt.Sprintf(`{"id":%q,"encryptedBytes":%d,"cipherSha256":%q}`, info.ID, info.EncryptedBytes, info.CipherSHA256)
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> \"$0.calls\"\n" +
		"case \"$1\" in\n" +
		"  --backup-list) echo '" + listJSON + "' ;;\n" +
		"  --backup-download)\n" +
		"    while test \"$#\" -gt 0; do\n" +
		"      if test \"$1\" = \"--backup-out\"; then shift; out=$1; break; fi\n" +
		"      shift\n" +
		"    done\n" +
		"    cp \"$0.fixture.apxbk\" \"$out\"\n" +
		"    echo '" + downloadJSON + "'\n" +
		"    ;;\n" +
		"  *) echo '{}' ;;\n" +
		"esac\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func writeFakeOversizedCloudDownloadPlugin(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-oversized-cloud-download-plugin.sh")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  --backup-list) echo '[{\"id\":\"oversized\"}]' ;;\n" +
		"  --backup-download)\n" +
		"    while test \"$#\" -gt 0; do\n" +
		"      if test \"$1\" = \"--backup-out\"; then shift; out=$1; break; fi\n" +
		"      shift\n" +
		"    done\n" +
		fmt.Sprintf("    dd if=/dev/zero of=\"$out\" bs=1 count=1 seek=%d 2>/dev/null\n", cloudBackupSinglePutMaxBytes) +
		"    while :; do :; done\n" +
		"    ;;\n" +
		"  *) echo '{}' ;;\n" +
		"esac\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// newCloudBackupTestAccessor wires a nativeBackupsWebAccessor over a temp
// backups root with a single fake "hermes" agent tree and a stub plugin.
func newCloudBackupTestAccessor(t *testing.T, paired, failUpload bool) (*nativeBackupsWebAccessor, *nativeBackupManager) {
	t.Helper()
	home := t.TempDir()

	agentRoot := filepath.Join(home, ".hermes")
	require.NoError(t, os.MkdirAll(agentRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(agentRoot, "state.db"), []byte("payload"), 0o600))

	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	require.NoError(t, os.MkdirAll(backupsRoot, 0o700))

	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots {
		return []nativebackup.AgentRoots{{Name: "hermes", Roots: []string{agentRoot}}}
	})
	deps := &webAPIDeps{
		backupsRoot:                 backupsRoot,
		backupMgr:                   mgr,
		remoteCfg:                   daemon.RemoteConfig{Executable: writeFakeCloudPlugin(t, home, paired, failUpload)},
		remotePluginCommandPreparer: testRemotePluginCommandPreparer,
		remotePluginVerifier: func(string) (proto.VerifiedRemotePlugin, error) {
			return proto.VerifiedRemotePlugin{Manifest: proto.RemotePluginManifestUnsignedV1{Capabilities: []string{proto.CapabilityInboundAckV2, proto.CapabilityPairStdinV1, proto.CapabilityTrustProtocolV1}}}, nil
		},
		remotePluginCheckpointVerifier: func(string, proto.VerifiedRemotePlugin) error { return nil },
	}
	return &nativeBackupsWebAccessor{deps: deps}, mgr
}

// stagingDirCount counts snapshot-ID directories left under .cloud-staging.
func stagingDirCount(t *testing.T, stagingRoot string) int {
	t.Helper()
	entries, err := os.ReadDir(stagingRoot)
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	n := 0
	for _, de := range entries {
		if !de.IsDir() {
			continue
		}
		if _, ok := nativebackup.SnapshotKindFromID(de.Name()); ok {
			n++
		}
	}
	return n
}

func requirePluginSawNoRawStaging(t *testing.T, pluginPath string) {
	t.Helper()
	count, err := os.ReadFile(pluginPath + ".staging-count")
	require.NoError(t, err)
	require.Equal(t, "0\n", string(count),
		"raw staged snapshot must be removed before the upload process starts")
}

func requireCloudDownloadsEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return
	}
	require.NoError(t, err)
	require.Empty(t, entries)
}

// seedStagingDir creates a fake leaked staging dir (with a payload file) under
// stagingRoot and stamps its mod-time so sweep ordering is deterministic.
func seedStagingDir(t *testing.T, stagingRoot, id string, modTime time.Time) string {
	t.Helper()
	dir := filepath.Join(stagingRoot, id)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.db"), []byte("payload"), 0o600))
	require.NoError(t, os.Chtimes(dir, modTime, modTime))
	return dir
}

// TestCreateCloudBackup_UploadSuccess_RemovesStagingDir: the staged snapshot
// directory must be gone after a successful upload.
func TestCreateCloudBackup_UploadSuccess_RemovesStagingDir(t *testing.T) {
	n, mgr := newCloudBackupTestAccessor(t, true, false)

	_, err := n.createCloudBackup(context.Background(), "scheduled", []string{"hermes"})
	require.NoError(t, err)

	requirePluginSawNoRawStaging(t, n.deps.remoteCfg.Executable)
	require.Equal(t, 0, stagingDirCount(t, mgr.cloudStagingRoot()),
		"staged snapshot dir must be removed after a successful cloud upload")
}

// TestCreateCloudBackup_UploadFailure_RemovesStagingDir: a paired device whose
// upload fails (relay down, transient plugin error). The staged directory must
// still be removed on the failure path.
func TestCreateCloudBackup_UploadFailure_RemovesStagingDir(t *testing.T) {
	n, mgr := newCloudBackupTestAccessor(t, true, true)

	_, err := n.createCloudBackup(context.Background(), "scheduled", []string{"hermes"})
	require.Error(t, err)

	requirePluginSawNoRawStaging(t, n.deps.remoteCfg.Executable)
	require.Equal(t, 0, stagingDirCount(t, mgr.cloudStagingRoot()),
		"staged snapshot dir must be removed even when the cloud upload fails")
}

func TestValidateCloudBackupUploadSize(t *testing.T) {
	require.NoError(t, validateCloudBackupUploadSize(cloudBackupSinglePutMaxBytes))

	err := validateCloudBackupUploadSize(cloudBackupSinglePutMaxBytes + 1)
	require.ErrorIs(t, err, errCloudBackupTooLarge)
	require.Contains(t, err.Error(), "multipart cloud uploads")
}

// TestSweepCloudStagingLocked_KeepsNewest: sweep removes all but the newest
// `keep` staging dirs, reclaims generated archive/metadata siblings, and leaves
// unrelated files alone.
func TestSweepCloudStagingLocked_KeepsNewest(t *testing.T) {
	home := t.TempDir()
	mgr := newNativeBackupManager(filepath.Join(home, ".aplexica", "backups"),
		func() []nativebackup.AgentRoots { return nil })
	stagingRoot := mgr.cloudStagingRoot()
	require.NoError(t, os.MkdirAll(stagingRoot, 0o700))

	base := time.Now().Add(-time.Hour)
	oldest := nativebackup.ScheduledPrefix + "2026-07-09T12-25-00Z"
	middle := nativebackup.ScheduledPrefix + "2026-07-09T13-00-00Z"
	newest := nativebackup.ScheduledPrefix + "2026-07-09T13-34-00Z"
	seedStagingDir(t, stagingRoot, oldest, base)
	seedStagingDir(t, stagingRoot, middle, base.Add(1*time.Minute))
	seedStagingDir(t, stagingRoot, newest, base.Add(2*time.Minute))

	// Sibling transient files are normally cleaned by their own defers, but a
	// process crash skips those defers. The sweep must reclaim them.
	apxbk := filepath.Join(stagingRoot, oldest+"-123.apxbk")
	meta := filepath.Join(stagingRoot, oldest+".metadata.json")
	archiveTmp := filepath.Join(stagingRoot, ".aplexica-cloud-archive-crash")
	note := filepath.Join(stagingRoot, "note.txt")
	require.NoError(t, os.WriteFile(apxbk, []byte("enc"), 0o600))
	require.NoError(t, os.WriteFile(meta, []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(archiveTmp, []byte("partial"), 0o600))
	require.NoError(t, os.WriteFile(note, []byte("keep"), 0o600))

	mgr.mu.Lock()
	removed, err := mgr.sweepCloudStagingLocked(1)
	mgr.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, 5, removed)

	require.NoDirExists(t, filepath.Join(stagingRoot, oldest))
	require.NoDirExists(t, filepath.Join(stagingRoot, middle))
	require.DirExists(t, filepath.Join(stagingRoot, newest))
	require.NoFileExists(t, apxbk)
	require.NoFileExists(t, meta)
	require.NoFileExists(t, archiveTmp)
	require.FileExists(t, note)
}

// TestSweepCloudStagingLocked_KeepZeroRemovesAll: keep=0 clears the staging
// area entirely (valid boundary input).
func TestSweepCloudStagingLocked_KeepZeroRemovesAll(t *testing.T) {
	home := t.TempDir()
	mgr := newNativeBackupManager(filepath.Join(home, ".aplexica", "backups"),
		func() []nativebackup.AgentRoots { return nil })
	stagingRoot := mgr.cloudStagingRoot()
	require.NoError(t, os.MkdirAll(stagingRoot, 0o700))
	seedStagingDir(t, stagingRoot, nativebackup.ManualPrefix+"2026-07-09T12-00-00Z", time.Now())
	seedStagingDir(t, stagingRoot, nativebackup.ScheduledPrefix+"2026-07-09T12-30-00Z", time.Now())

	mgr.mu.Lock()
	removed, err := mgr.sweepCloudStagingLocked(0)
	mgr.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, 2, removed)
	require.Equal(t, 0, stagingDirCount(t, stagingRoot))
}

// TestSweepCloudStaging_StartupReclaimsPreExisting: the exported startup entry
// point reclaims already-accumulated orphans down to cloudStagingRetain — the
// migration path for installs that already leaked (e.g. 26 dirs / 123 GB).
func TestSweepCloudStaging_StartupReclaimsPreExisting(t *testing.T) {
	home := t.TempDir()
	mgr := newNativeBackupManager(filepath.Join(home, ".aplexica", "backups"),
		func() []nativebackup.AgentRoots { return nil })
	stagingRoot := mgr.cloudStagingRoot()
	require.NoError(t, os.MkdirAll(stagingRoot, 0o700))

	base := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 6; i++ {
		id := nativebackup.ScheduledPrefix + "2026-07-0" + string(rune('1'+i)) + "T12-00-00Z"
		seedStagingDir(t, stagingRoot, id, base.Add(time.Duration(i)*time.Minute))
	}

	mgr.SweepCloudStaging(nil)

	require.Zero(t, stagingDirCount(t, stagingRoot))
}

func TestSweepCloudStaging_WaitsForActiveCloudOperation(t *testing.T) {
	home := t.TempDir()
	mgr := newNativeBackupManager(filepath.Join(home, ".aplexica", "backups"),
		func() []nativebackup.AgentRoots { return nil })
	stagingRoot := mgr.cloudStagingRoot()
	orphan := seedStagingDir(t, stagingRoot, nativebackup.ScheduledPrefix+"2026-07-09T12-00-00Z", time.Now())

	mgr.cloudMu.Lock()
	done := make(chan struct{})
	go func() {
		mgr.SweepCloudStaging(nil)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("staging sweep ran while a cloud operation held its lifetime lease")
	case <-time.After(25 * time.Millisecond):
		require.DirExists(t, orphan)
	}
	mgr.cloudMu.Unlock()

	select {
	case <-done:
		require.NoDirExists(t, orphan)
	case <-time.After(time.Second):
		t.Fatal("staging sweep did not resume after the cloud operation completed")
	}
}

func TestSweepCloudDownloads_StartupReclaimsKilledRestoreArtifacts(t *testing.T) {
	home := t.TempDir()
	mgr := newNativeBackupManager(filepath.Join(home, ".aplexica", "backups"),
		func() []nativebackup.AgentRoots { return nil })
	root := mgr.cloudDownloadsRoot()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "cloud-id-restore-killed", "hermes"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cloud-id-killed.apxbk"), []byte("encrypted"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cloud-id-restore-killed", "hermes", "state.db"), []byte("extracted"), 0o600))
	// Unknown children are also disposable: the work directory has no durable
	// user content, and future/partial temp naming must not create a new leak.
	require.NoError(t, os.WriteFile(filepath.Join(root, "partial-from-newer-version"), []byte("partial"), 0o600))

	lg := &captureLog{}
	mgr.SweepCloudDownloads(lg)

	requireCloudDownloadsEmpty(t, root)
	require.Contains(t, lg.infos, "cloud-download sweep: reclaimed orphaned restore data")
}

func TestSweepCloudDownloads_WaitsForActiveCloudRestore(t *testing.T) {
	home := t.TempDir()
	mgr := newNativeBackupManager(filepath.Join(home, ".aplexica", "backups"),
		func() []nativebackup.AgentRoots { return nil })
	root := mgr.cloudDownloadsRoot()
	orphan := filepath.Join(root, "active.apxbk")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(orphan, []byte("encrypted"), 0o600))

	mgr.cloudMu.Lock()
	done := make(chan struct{})
	go func() {
		mgr.SweepCloudDownloads(nil)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("cloud-download sweep ran while an active restore held cloudMu")
	case <-time.After(25 * time.Millisecond):
		require.FileExists(t, orphan)
	}
	mgr.cloudMu.Unlock()

	select {
	case <-done:
		require.NoFileExists(t, orphan)
	case <-time.After(time.Second):
		t.Fatal("cloud-download sweep did not resume after the active restore completed")
	}
}

func TestRestoreCloudBackup_SweepsOrphansBeforeDownloadAllocation(t *testing.T) {
	n, mgr := newCloudBackupTestAccessor(t, true, false)
	root := mgr.cloudDownloadsRoot()
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "orphan-marker"), []byte("old"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "old-restore-tree"), 0o700))

	_, err := n.restoreCloudBackup(context.Background(), "cloud-id", "hermes")
	require.Error(t, err) // the stub intentionally returns an empty archive
	marker, readErr := os.ReadFile(n.deps.remoteCfg.Executable + ".download-orphan-present")
	require.NoError(t, readErr)
	require.Equal(t, "no\n", string(marker), "orphan cleanup must finish before the plugin can allocate/download")
	requireCloudDownloadsEmpty(t, root)
}

func TestRestoreCloudBackup_CleanupFailureAbortsBeforePluginInvocation(t *testing.T) {
	n, mgr := newCloudBackupTestAccessor(t, true, false)
	root := mgr.cloudDownloadsRoot()
	require.NoError(t, os.WriteFile(root, []byte("not a directory"), 0o600))

	_, err := n.restoreCloudBackup(context.Background(), "cloud-id", "hermes")
	require.ErrorContains(t, err, "cleanup must complete before allocation")
	_, callsErr := os.Stat(n.deps.remoteCfg.Executable + ".calls")
	require.ErrorIs(t, callsErr, os.ErrNotExist, "cleanup failure must fail closed before any plugin process starts")
}

func TestRestoreCloudBackup_RemovesAuthenticatedDownloadBeforeNativeRestore(t *testing.T) {
	n, mgr := newCloudBackupTestAccessor(t, true, false)
	home := filepath.Dir(filepath.Dir(mgr.backupsRoot))
	t.Setenv("HOME", home)

	snapshotDir := filepath.Join(mgr.backupsRoot, nativebackup.ManualPrefix+"download-source")
	_, err := nativebackup.SnapshotAuthenticated(mgr.agentRoots(), snapshotDir)
	require.NoError(t, err)
	archivePath := filepath.Join(home, "download-source.apxbk")
	meta, err := nativebackup.EncryptSnapshotDir(snapshotDir, archivePath, mgr.cloudKeyPath())
	require.NoError(t, err)
	archive, err := os.ReadFile(archivePath)
	require.NoError(t, err)
	info := nativebackup.BackupInfo{
		ID:             "cloud-id",
		EncryptedBytes: meta.EncryptedBytes,
		CipherSHA256:   meta.CipherSHA256,
	}
	n.deps.remoteCfg.Executable = writeFakeCloudDownloadPlugin(t, home, archive, info)

	stop := errors.New("stop after download cleanup check")
	checked := false
	mgr.restoreCoordinator = &checkingRestoreCoordinator{
		err: stop,
		check: func() {
			checked = true
			entries, readErr := os.ReadDir(mgr.cloudDownloadsRoot())
			require.NoError(t, readErr)
			for _, entry := range entries {
				require.NotEqual(t, ".apxbk", filepath.Ext(entry.Name()),
					"encrypted input must be removed before the native restore lease is acquired")
			}
		},
	}

	_, err = n.restoreCloudBackup(context.Background(), info.ID, "hermes")
	require.ErrorIs(t, err, stop)
	require.True(t, checked)
	requireCloudDownloadsEmpty(t, mgr.cloudDownloadsRoot())
}

func TestRestoreCloudBackup_CancelsOversizedDownloadAndLeavesNoResidue(t *testing.T) {
	n, mgr := newCloudBackupTestAccessor(t, true, false)
	home := filepath.Dir(filepath.Dir(mgr.backupsRoot))
	n.deps.remoteCfg.Executable = writeFakeOversizedCloudDownloadPlugin(t, home)

	started := time.Now()
	_, err := n.restoreCloudBackup(context.Background(), "oversized", "hermes")
	require.ErrorIs(t, err, errCloudBackupTooLarge)
	require.Less(t, time.Since(started), 5*time.Second, "the live size monitor must kill the plugin instead of waiting for its timeout")
	requireCloudDownloadsEmpty(t, mgr.cloudDownloadsRoot())
}

// TestCreateCloudStaging_SweepsPreExistingOrphans: staging a new backup first
// bounds pre-existing orphans, so .cloud-staging can never grow without bound
// even if a run is interrupted before its own cleanup.
func TestCreateCloudStaging_SweepsPreExistingOrphans(t *testing.T) {
	_, mgr := newCloudBackupTestAccessor(t, true, false)
	stagingRoot := mgr.cloudStagingRoot()
	require.NoError(t, os.MkdirAll(stagingRoot, 0o700))

	base := time.Now().Add(-time.Hour)
	for i := 0; i < 4; i++ {
		id := nativebackup.ScheduledPrefix + "2026-07-08T0" + string(rune('1'+i)) + "-00-00Z"
		seedStagingDir(t, stagingRoot, id, base.Add(time.Duration(i)*time.Minute))
	}

	info, err := mgr.CreateCloudStagingContext(context.Background(), "scheduled", []string{"hermes"})
	require.NoError(t, err)
	require.NotEmpty(t, info.Path)

	// Pre-existing orphans bounded to cloudStagingRetain, plus the one just
	// allocated (which the caller — createCloudBackup — removes after upload).
	require.Equal(t, cloudStagingRetain+1, stagingDirCount(t, stagingRoot))
}

func TestCreateCloudStaging_CleanupFailureAbortsBeforeAllocatingAnotherRawTree(t *testing.T) {
	_, mgr := newCloudBackupTestAccessor(t, true, false)
	stagingRoot := mgr.cloudStagingRoot()
	require.NoError(t, os.MkdirAll(stagingRoot, 0o700))
	orphan := seedStagingDir(t, stagingRoot,
		nativebackup.ScheduledPrefix+"2026-07-08T01-00-00Z", time.Now().Add(-time.Hour))
	wantErr := fmt.Errorf("simulated Windows sharing violation")
	mgr.removeCloudStaging = func(victims []string) (int, error) {
		require.Equal(t, []string{orphan}, victims)
		return 0, wantErr
	}

	_, err := mgr.CreateCloudStagingContext(context.Background(), "scheduled", []string{"hermes"})
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, 1, stagingDirCount(t, stagingRoot),
		"a cleanup failure must not admit another multi-gigabyte raw staging tree")
}

// TestCreateCloudBackup_NotPaired_SkipsStagingAndUpload: the pairing gate. On a
// device whose plugin is installed but not paired, the upload cannot succeed —
// so the daemon must not copy + encrypt a multi-GB snapshot only to throw it
// away. createCloudBackup must fail fast with errCloudPluginNotPaired, never
// invoke --backup-upload, and never create the staging area.
func TestCreateCloudBackup_NotPaired_SkipsStagingAndUpload(t *testing.T) {
	n, mgr := newCloudBackupTestAccessor(t, false, false)

	_, err := n.createCloudBackup(context.Background(), "scheduled", []string{"hermes"})
	require.ErrorIs(t, err, errCloudPluginNotPaired)

	calls, rerr := os.ReadFile(n.deps.remoteCfg.Executable + ".calls")
	require.NoError(t, rerr)
	require.Contains(t, string(calls), "--status")
	require.NotContains(t, string(calls), "--backup-upload",
		"unpaired device must not attempt an upload")

	_, statErr := os.Stat(mgr.cloudStagingRoot())
	require.True(t, os.IsNotExist(statErr),
		"unpaired device must not create the cloud-staging area at all")
}

// TestRunScheduledIfDue_NotPaired_LogsInfoNotWarn: an installed-but-unpaired
// device with a cloud-destination schedule is an expected steady state, not a
// failure — the scheduler logs it at INFO (no warn spam) and keeps the short
// retry so backups start promptly once the device is paired.
func TestRunScheduledIfDue_NotPaired_LogsInfoNotWarn(t *testing.T) {
	home := t.TempDir()
	mgr := newNativeBackupManager(filepath.Join(home, ".aplexica", "backups"),
		func() []nativebackup.AgentRoots { return nil })
	require.NoError(t, os.MkdirAll(mgr.backupsRoot, 0o700))
	require.NoError(t, writeNativeBackupSchedule(mgr.schedulePath(), nativebackup.ScheduleConfig{
		Enabled:         true,
		IntervalMinutes: 30,
		Destination:     "cloud",
		NextRunAt:       time.Now().UTC().Add(-time.Minute),
	}))

	lg := &captureLog{}
	mgr.RunScheduledIfDue(lg, func(kind string, agents []string, destination string) (nativebackup.BackupInfo, error) {
		return nativebackup.BackupInfo{}, fmt.Errorf("cloud backup: %w", errCloudPluginNotPaired)
	})

	require.Empty(t, lg.warns, "not-paired skip must not warn")
	require.Contains(t, lg.infos, "scheduled cloud backup skipped: cloud plugin is not paired")

	cfg, err := mgr.LoadSchedule()
	require.NoError(t, err)
	require.True(t, cfg.NextRunAt.After(time.Now().UTC()), "a retry must be scheduled")
	require.True(t, cfg.LastRunAt.IsZero(), "a skipped run must not count as a successful run")
}

func TestRunScheduledIfDue_TooLargeUsesConfiguredCadence(t *testing.T) {
	home := t.TempDir()
	mgr := newNativeBackupManager(filepath.Join(home, ".aplexica", "backups"),
		func() []nativebackup.AgentRoots { return nil })
	require.NoError(t, os.MkdirAll(mgr.backupsRoot, 0o700))
	require.NoError(t, writeNativeBackupSchedule(mgr.schedulePath(), nativebackup.ScheduleConfig{
		Enabled:         true,
		IntervalMinutes: 720,
		Destination:     "cloud",
		NextRunAt:       time.Now().UTC().Add(-time.Minute),
	}))

	started := time.Now().UTC()
	mgr.RunScheduledIfDue(&captureLog{}, func(kind string, agents []string, destination string) (nativebackup.BackupInfo, error) {
		return nativebackup.BackupInfo{}, fmt.Errorf("upload: %w", errCloudBackupTooLarge)
	})

	cfg, err := mgr.LoadSchedule()
	require.NoError(t, err)
	require.True(t, cfg.LastRunAt.IsZero())
	require.GreaterOrEqual(t, cfg.NextRunAt.Sub(started), 719*time.Minute,
		"permanent size failures must not retry after five minutes")
}

// TestCloudStagingVictimsLocked_ReturnsAllButNewest: the enumerate step returns
// the absolute paths of every staged dir except the newest `keep` (and never a
// non-snapshot file), so SweepCloudStaging can run the slow deletion off-lock —
// the guard against a large startup reclaim blocking the daemon from starting.
func TestCloudStagingVictimsLocked_ReturnsAllButNewest(t *testing.T) {
	home := t.TempDir()
	mgr := newNativeBackupManager(filepath.Join(home, ".aplexica", "backups"),
		func() []nativebackup.AgentRoots { return nil })
	stagingRoot := mgr.cloudStagingRoot()
	require.NoError(t, os.MkdirAll(stagingRoot, 0o700))

	base := time.Now().Add(-time.Hour)
	oldest := nativebackup.ScheduledPrefix + "2026-07-09T12-00-00Z"
	newest := nativebackup.ScheduledPrefix + "2026-07-09T13-00-00Z"
	seedStagingDir(t, stagingRoot, oldest, base)
	seedStagingDir(t, stagingRoot, newest, base.Add(time.Minute))
	require.NoError(t, os.WriteFile(filepath.Join(stagingRoot, "note.txt"), []byte("x"), 0o600))

	mgr.mu.Lock()
	victims, err := mgr.cloudStagingVictimsLocked(1)
	mgr.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(stagingRoot, oldest)}, victims)
}
