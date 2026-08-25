package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

// TestDelete_RemovesSecrets asserts that deleting an artifact that owns
// secret material also removes that material from the secrets store.
func TestDelete_RemovesSecrets(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	secretsRoot := filepath.Join(tmp, "secrets")

	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# m\n")

	ss := &secrets.Store{Root: secretsRoot}
	require.NoError(t, ss.Init())
	require.NoError(t, ss.Put(id, "API_KEY", "secret-value"))

	t.Cleanup(func() {
		deleteStoreRoot = ""
		deleteSecretsRoot = ""
		deleteYes = false
	})
	_, err := runRoot(t,
		"delete", id,
		"--store", storeRoot,
		"--secrets-root", secretsRoot,
		"--yes",
	)
	require.NoError(t, err)

	// Artifact gone.
	_, err = (&acf.Store{Root: storeRoot}).ReadArtifact(acf.KindMemory, id)
	require.Error(t, err)

	// Secret material gone too — not orphaned.
	keys, err := ss.ListForArtifact(id)
	require.NoError(t, err)
	require.Empty(t, keys)
}

// TestDelete_SurfacesSecretListError asserts that when listing an
// artifact's secrets fails (e.g. an unreadable secrets dir), delete does
// NOT silently destroy the artifact and orphan the secret material; it
// surfaces the error instead.
func TestDelete_SurfacesSecretListError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("0o000 dir perms do not block ReadDir on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses 0o000 permissions; cannot simulate an unreadable dir")
	}

	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	secretsRoot := filepath.Join(tmp, "secrets")

	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# m\n")

	ss := &secrets.Store{Root: secretsRoot}
	require.NoError(t, ss.Init())
	require.NoError(t, ss.Put(id, "API_KEY", "secret-value"))

	// Make the per-artifact secrets dir unreadable so ListForArtifact's
	// os.ReadDir returns EACCES rather than (nil,nil).
	artDir := filepath.Join(secretsRoot, id)
	require.NoError(t, os.Chmod(artDir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(artDir, 0o700) })

	t.Cleanup(func() {
		deleteStoreRoot = ""
		deleteSecretsRoot = ""
		deleteYes = false
	})
	_, err := runRoot(t,
		"delete", id,
		"--store", storeRoot,
		"--secrets-root", secretsRoot,
		"--yes",
	)
	require.Error(t, err, "delete must not succeed when secret listing fails")

	// The artifact must still exist (recoverable) rather than having been
	// destroyed while its secrets were orphaned.
	require.NoError(t, os.Chmod(artDir, 0o700))
	_, rErr := (&acf.Store{Root: storeRoot}).ReadArtifact(acf.KindMemory, id)
	require.NoError(t, rErr, "artifact must be preserved when secret listing fails")
}
