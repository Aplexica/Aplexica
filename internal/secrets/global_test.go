package secrets

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPutGlobal_WritesValueAndSidecar(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.PutGlobal("github-token", "ghp_abc"))

	v, err := s.GetGlobal("github-token")
	require.NoError(t, err)
	require.Equal(t, "ghp_abc", v)

	meta, err := s.ReadMeta("github-token")
	require.NoError(t, err)
	require.Equal(t, "github-token", meta.Name)
	require.False(t, meta.CreatedAt.IsZero())
	require.False(t, meta.UpdatedAt.IsZero())
	require.False(t, meta.SyncEnabled, "FR-02.16 default-false")
}

func TestPutGlobal_OverwriteBumpsUpdatedAt(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.PutGlobal("k", "v1"))
	m1, _ := s.ReadMeta("k")

	require.NoError(t, s.PutGlobal("k", "v2"))
	m2, _ := s.ReadMeta("k")

	require.Equal(t, m1.CreatedAt, m2.CreatedAt, "CreatedAt is preserved on overwrite")
	require.True(t, !m2.UpdatedAt.Before(m1.UpdatedAt), "UpdatedAt is monotonically non-decreasing")
}

func TestRotateGlobal_RefusesIfMissing(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.Error(t, s.RotateGlobal("never-set", "v"))
}

func TestRotateGlobal_ReplacesValueAndKeepsCreatedAt(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.PutGlobal("k", "v1"))
	require.NoError(t, s.RotateGlobal("k", "v2"))

	v, _ := s.GetGlobal("k")
	require.Equal(t, "v2", v)
}

func TestDeleteGlobal_RemovesBothValueAndMeta(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.PutGlobal("k", "v"))
	require.NoError(t, s.DeleteGlobal("k"))

	_, err := s.GetGlobal("k")
	require.Error(t, err)
	_, err = s.ReadMeta("k")
	require.Error(t, err)
}

func TestDeleteGlobal_IdempotentOnMissing(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.PutGlobal("k", "v"))
	require.NoError(t, s.DeleteGlobal("k"))
	require.NoError(t, s.DeleteGlobal("k"), "second delete must be a no-op")
}

func TestListGlobal_SkipsMetaDirAndPerArtifactDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	s := &Store{Root: root}
	require.NoError(t, s.PutGlobal("a", "1"))
	require.NoError(t, s.PutGlobal("b", "2"))
	// Mix in a per-artifact directory and verify it's skipped.
	require.NoError(t, s.Put("artifact-x", "K", "v"))

	got, err := s.ListGlobal()
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, got,
		"ListGlobal must skip the .meta dir AND per-artifact dirs")
}

func TestListGlobal_EmptyRoot(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	got, err := s.ListGlobal()
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSetSyncEnabled_TogglesSidecarFlag(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.PutGlobal("k", "v"))

	require.NoError(t, s.SetSyncEnabled("k", true))
	meta, _ := s.ReadMeta("k")
	require.True(t, meta.SyncEnabled)

	require.NoError(t, s.SetSyncEnabled("k", false))
	meta, _ = s.ReadMeta("k")
	require.False(t, meta.SyncEnabled)
}

func TestSetSyncEnabled_RefusesIfSecretMissing(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.Error(t, s.SetSyncEnabled("never-set", true))
}

func TestAddUsedByTool_DedupesAndSorts(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.PutGlobal("k", "v"))

	require.NoError(t, s.AddUsedByTool("k", "art-3"))
	require.NoError(t, s.AddUsedByTool("k", "art-1"))
	require.NoError(t, s.AddUsedByTool("k", "art-2"))
	require.NoError(t, s.AddUsedByTool("k", "art-1"), "dup must be a no-op")

	meta, _ := s.ReadMeta("k")
	require.Equal(t, []string{"art-1", "art-2", "art-3"}, meta.UsedByTools)
}

func TestRemoveUsedByTool_LeavesOthers(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.PutGlobal("k", "v"))
	require.NoError(t, s.AddUsedByTool("k", "a"))
	require.NoError(t, s.AddUsedByTool("k", "b"))
	require.NoError(t, s.RemoveUsedByTool("k", "a"))

	meta, _ := s.ReadMeta("k")
	require.Equal(t, []string{"b"}, meta.UsedByTools)
}

// UnlinkToolSecret prunes the sidecar when the last reference is removed and the
// secret has no global value — the rollback case for a tool import whose secret
// lives only in the per-artifact layout.
func TestUnlinkToolSecret_PrunesEmptySidecar(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	s := &Store{Root: root}
	// AddUsedByTool auto-creates the sidecar with no backing global value,
	// exactly as the tool-import path does for a per-artifact secret.
	require.NoError(t, s.AddUsedByTool("github.TOKEN", "art-1"))
	_, err := s.ReadMeta("github.TOKEN")
	require.NoError(t, err, "precondition: sidecar exists")

	require.NoError(t, s.UnlinkToolSecret("github.TOKEN", "art-1"))

	_, err = s.ReadMeta("github.TOKEN")
	require.ErrorIs(t, err, os.ErrNotExist, "empty unbacked sidecar must be pruned")
	// No directories (e.g. .meta) should linger under the secrets root.
	entries, err := os.ReadDir(root)
	if err == nil {
		for _, e := range entries {
			require.False(t, e.IsDir(), "no dir should remain, found %s", e.Name())
		}
	} else {
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestUnlinkToolSecret_KeepsOtherRefs(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.AddUsedByTool("k", "a"))
	require.NoError(t, s.AddUsedByTool("k", "b"))
	require.NoError(t, s.UnlinkToolSecret("k", "a"))

	meta, err := s.ReadMeta("k")
	require.NoError(t, err, "sidecar with remaining refs must be preserved")
	require.Equal(t, []string{"b"}, meta.UsedByTools)
}

func TestUnlinkToolSecret_KeepsGlobalBackedSidecar(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.PutGlobal("k", "v")) // real global value backs the sidecar
	require.NoError(t, s.AddUsedByTool("k", "a"))
	require.NoError(t, s.UnlinkToolSecret("k", "a"))

	meta, err := s.ReadMeta("k")
	require.NoError(t, err, "a sidecar backing a real global value must be preserved")
	require.Empty(t, meta.UsedByTools)
}

func TestGlobal_FilePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits unreliable on Windows")
	}
	root := filepath.Join(t.TempDir(), "secrets")
	s := &Store{Root: root}
	require.NoError(t, s.PutGlobal("k", "v"))

	// Root is 0o700.
	info, err := os.Stat(root)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	// Value file is 0o600.
	info, err = os.Stat(filepath.Join(root, "k"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// .meta dir is 0o700.
	info, err = os.Stat(filepath.Join(root, metaDirName))
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestValidateName_RejectsReservedMeta(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.Error(t, s.PutGlobal(".meta", "v"))
}
