package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// resetBackupRestoreFlags clears the package-global flag vars the backup
// and restore commands bind to, so values don't leak across test runs.
func resetBackupRestoreFlags() {
	backupStoreRoot = ""
	backupSecretsRoot = ""
	backupIncludeSecrets = false
	backupRespectSyncFlags = true
	backupSign = false
	backupKeyPath = ""
	backupEncrypt = false
	backupRecipientPath = ""
	backupPassphraseEnvVar = ""
	backupPassphraseStdin = false
	backupUnsigned = false
	backupAnonymize = false
	backupAnonymizeDryRun = false
	backupScope = ""
	backupProjects = nil
	backupIncludePendingProjects = true
	backupStateDir = ""
	backupJSON = false

	restoreStoreRoot = ""
	restoreSecretsRoot = ""
	restorePeek = false
	restoreDryRun = false
	restoreVerify = false
	restorePubKeyPath = ""
	restoreExpectedKeyID = ""
	restoreUnsignedOK = false
	restoreDecrypt = false
	restoreIdentityPath = ""
	restorePassphraseEnvVar = ""
	restoreJSON = false
}

func TestBackup_JSONOutput(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	seedMemoryArtifact(t, storeRoot, "claude-code", "# m\n")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")

	t.Cleanup(resetBackupRestoreFlags)
	out, err := runRoot(t,
		"backup", bundlePath,
		"--store", storeRoot,
		"--state-dir", filepath.Join(tmp, "state"),
		"--unsigned",
		"--json",
	)
	require.NoError(t, err, "output:\n%s", out)

	// Output must be valid JSON, with no human "note:"/"wrote" lines.
	require.NotContains(t, out, "note:")
	require.NotContains(t, out, "wrote ")

	var got struct {
		BundlePath      string `json:"bundlePath"`
		Bytes           int64  `json:"bytes"`
		IncludesSecrets bool   `json:"includesSecrets"`
		Anonymized      bool   `json:"anonymized"`
		SignaturePath   string `json:"signaturePath"`
		Encrypted       bool   `json:"encrypted"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &got))
	require.Equal(t, bundlePath, got.BundlePath)
	require.Greater(t, got.Bytes, int64(0))
	require.False(t, got.IncludesSecrets)
	require.False(t, got.Encrypted)
	require.Empty(t, got.SignaturePath)
}

func TestRestore_JSONOutput(t *testing.T) {
	tmp := t.TempDir()
	srcStore := filepath.Join(tmp, "src")
	seedMemoryArtifact(t, srcStore, "claude-code", "# m\n")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")

	t.Cleanup(resetBackupRestoreFlags)
	_, err := runRoot(t, "backup", bundlePath, "--store", srcStore, "--state-dir", filepath.Join(tmp, "state"), "--unsigned")
	require.NoError(t, err)

	resetBackupRestoreFlags()
	dstStore := filepath.Join(tmp, "dst")
	out, err := runRoot(t,
		"restore", bundlePath,
		"--store", dstStore,
		"--unsigned-ok",
		"--json",
	)
	require.NoError(t, err, "output:\n%s", out)
	require.NotContains(t, out, "restored bundle")

	var got struct {
		BundlePath string `json:"bundlePath"`
		StoreRoot  string `json:"storeRoot"`
		Restored   bool   `json:"restored"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &got))
	require.Equal(t, bundlePath, got.BundlePath)
	require.Equal(t, dstStore, got.StoreRoot)
	require.True(t, got.Restored)
}

func TestRestore_PeekJSONOutput(t *testing.T) {
	tmp := t.TempDir()
	srcStore := filepath.Join(tmp, "src")
	seedMemoryArtifact(t, srcStore, "claude-code", "# m\n")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")

	t.Cleanup(resetBackupRestoreFlags)
	_, err := runRoot(t, "backup", bundlePath, "--store", srcStore, "--state-dir", filepath.Join(tmp, "state"), "--unsigned")
	require.NoError(t, err)

	resetBackupRestoreFlags()
	out, err := runRoot(t,
		"restore", bundlePath,
		"--peek",
		"--unsigned-ok",
		"--json",
	)
	require.NoError(t, err, "output:\n%s", out)
	require.NotContains(t, out, "Bundle metadata:")

	var meta acf.BundleMeta
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &meta))
	require.NotEmpty(t, meta.BundleVersion)
	require.False(t, meta.IncludesSecrets)
}
