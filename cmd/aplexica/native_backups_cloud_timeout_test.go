package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/nativebackup"
	"github.com/stretchr/testify/require"
)

func TestCloudBackupUploadTimeout(t *testing.T) {
	require.Equal(t, cloudBackupUploadTimeoutFloor, cloudBackupUploadTimeout(0))
	require.Equal(t, cloudBackupUploadTimeoutFloor, cloudBackupUploadTimeout(-1))

	// Ten minutes of overhead plus twenty minutes of transfer time lands on
	// the floor. One more byte rounds the transfer estimate up by one second.
	floorBytes := int64(20*60) * cloudBackupUploadAssumedMinBytesPerSecond
	require.Equal(t, cloudBackupUploadTimeoutFloor, cloudBackupUploadTimeout(floorBytes))
	require.Equal(t, cloudBackupUploadTimeoutFloor+time.Second, cloudBackupUploadTimeout(floorBytes+1))

	// A representative multi-gigabyte archive receives additional transfer time,
	// and the largest valid single-part archive remains below the cap.
	require.Equal(t, 2*time.Hour+4*time.Minute+46*time.Second, cloudBackupUploadTimeout(3_610_000_000))
	require.Equal(t, 3*time.Hour+40*time.Second, cloudBackupUploadTimeout(cloudBackupSinglePutMaxBytes))

	// Pathological metadata is bounded without overflowing time.Duration.
	require.Equal(t, cloudBackupUploadTimeoutCeiling, cloudBackupUploadTimeout(int64(^uint64(0)>>1)))
}

func TestCloudBackupDownloadTimeout(t *testing.T) {
	// Legacy cloud records do not carry encryptedBytes. They must receive the
	// same allowance as the largest valid object, not the historical 30 minutes.
	wantMaxObject := 3*time.Hour + 40*time.Second
	require.Equal(t, wantMaxObject, cloudBackupDownloadTimeout(0))
	require.Equal(t, wantMaxObject, cloudBackupDownloadTimeout(-1))
	require.Equal(t, wantMaxObject, cloudBackupDownloadTimeout(cloudBackupSinglePutMaxBytes+1))

	// Authenticated list metadata still shortens the bound for small objects.
	require.Equal(t, cloudBackupUploadTimeoutFloor, cloudBackupDownloadTimeout(1<<20))
	require.Equal(t, 2*time.Hour+4*time.Minute+46*time.Second, cloudBackupDownloadTimeout(3_610_000_000))
}

func TestCloudBackupDownloadPreflightSize(t *testing.T) {
	backups := []nativebackup.BackupInfo{
		{ID: "other", EncryptedBytes: 5},
		{ID: "target", EncryptedBytes: 3_610_000_000},
	}
	size, err := cloudBackupDownloadPreflightSize(backups, "target")
	require.NoError(t, err)
	require.Equal(t, int64(3_610_000_000), size)

	// Missing/legacy metadata deliberately selects the maximum valid timeout.
	size, err = cloudBackupDownloadPreflightSize([]nativebackup.BackupInfo{{ID: "target"}}, "target")
	require.NoError(t, err)
	require.Zero(t, size)
	size, err = cloudBackupDownloadPreflightSize(backups, "missing")
	require.NoError(t, err)
	require.Zero(t, size)

	_, err = cloudBackupDownloadPreflightSize([]nativebackup.BackupInfo{{ID: "target", EncryptedBytes: -1}}, "target")
	require.ErrorContains(t, err, "invalid encrypted size")
	_, err = cloudBackupDownloadPreflightSize([]nativebackup.BackupInfo{{ID: "target", EncryptedBytes: cloudBackupSinglePutMaxBytes + 1}}, "target")
	require.ErrorIs(t, err, errCloudBackupTooLarge)
	_, err = cloudBackupDownloadPreflightSize([]nativebackup.BackupInfo{{ID: "target"}, {ID: "target"}}, "target")
	require.ErrorContains(t, err, "duplicate backup id")
}

func TestValidateCloudBackupDownloadSizes(t *testing.T) {
	require.NoError(t, validateCloudBackupDownloadSizes(100, 100, 100, 1024))
	require.NoError(t, validateCloudBackupDownloadSizes(100, 0, 0, 1024), "legacy metadata is allowed only within the hard cap")
	require.ErrorContains(t, validateCloudBackupDownloadSizes(0, 0, 0, 1024), "empty")
	require.ErrorIs(t, validateCloudBackupDownloadSizes(1025, 0, 0, 1024), errCloudBackupTooLarge)
	require.ErrorContains(t, validateCloudBackupDownloadSizes(100, 99, 0, 1024), "preflight encrypted size mismatch")
	require.ErrorContains(t, validateCloudBackupDownloadSizes(100, 0, 99, 1024), "reported encrypted size mismatch")
	require.ErrorContains(t, validateCloudBackupDownloadSizes(100, 0, -1, 1024), "invalid reported encrypted size")
}

func TestMonitorCloudBackupDownloadCancelsOnIdentityReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "download.apxbk")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	retained, err := os.Open(path)
	require.NoError(t, err)
	expected, err := retained.Stat()
	require.NoError(t, err)
	require.NoError(t, retained.Close())
	cancelled := make(chan struct{})
	var cancelOnce sync.Once
	stop := make(chan struct{})
	result := monitorCloudBackupDownload(path, expected, 1024, time.Millisecond, func() {
		cancelOnce.Do(func() { close(cancelled) })
	}, stop)

	require.NoError(t, os.Rename(path, path+".replaced"))
	require.NoError(t, os.WriteFile(path, []byte("different inode"), 0o600))
	select {
	case monitorErr := <-result:
		require.ErrorContains(t, monitorErr, "changed identity or type")
	case <-time.After(time.Second):
		t.Fatal("download identity monitor did not detect replacement")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("download identity monitor did not cancel the plugin context")
	}
	close(stop)
}
