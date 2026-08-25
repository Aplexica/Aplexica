package secrets

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStore_PutAndGet(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	require.NoError(t, s.Put("artifact-1", "KEY1", "value1"))
	require.NoError(t, s.Put("artifact-1", "KEY2", "value2"))

	v1, err := s.Get("artifact-1", "KEY1")
	require.NoError(t, err)
	require.Equal(t, "value1", v1)

	v2, err := s.Get("artifact-1", "KEY2")
	require.NoError(t, err)
	require.Equal(t, "value2", v2)
}

func TestStore_Get_NotFound(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	_, err := s.Get("nonexistent", "KEY")
	require.Error(t, err)
}

func TestStore_PutOverwrites(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	require.NoError(t, s.Put("a", "K", "first"))
	require.NoError(t, s.Put("a", "K", "second"))

	v, err := s.Get("a", "K")
	require.NoError(t, err)
	require.Equal(t, "second", v)
}

func TestStore_ListForArtifact(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	require.NoError(t, s.Put("a", "K1", "v1"))
	require.NoError(t, s.Put("a", "K2", "v2"))
	require.NoError(t, s.Put("b", "K3", "v3"))

	aKeys, err := s.ListForArtifact("a")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"K1", "K2"}, aKeys)

	bKeys, err := s.ListForArtifact("b")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"K3"}, bKeys)
}

func TestStore_FilePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits unreliable on Windows")
	}
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	require.NoError(t, s.Put("a", "K", "v"))

	rootInfo, err := os.Stat(s.Root)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), rootInfo.Mode().Perm(),
		"secrets root must be 0o700")

	artifactInfo, err := os.Stat(filepath.Join(s.Root, "a"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), artifactInfo.Mode().Perm(),
		"secrets/<artifact> must be 0o700")

	fileInfo, err := os.Stat(filepath.Join(s.Root, "a", "K"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm(),
		"secret file must be 0o600")
}

func TestStore_InvalidKey(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	cases := []string{"", "../escape", "with/slash", "with\\backslash", ".", "..", "with\x00null"}
	for _, k := range cases {
		require.Error(t, s.Put("a", k, "v"), "key %q must be rejected", k)
	}
}

func TestStore_Put_OverwriteResetsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits unreliable on Windows")
	}
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	require.NoError(t, s.Put("a", "K", "v1"))

	// Loosen the file mode externally — simulate a tampered or
	// accidentally-chmod'd secret file.
	path := filepath.Join(s.Root, "a", "K")
	require.NoError(t, os.Chmod(path, 0o644))

	// Re-Put should restore 0o600 regardless of the prior mode.
	require.NoError(t, s.Put("a", "K", "v2"))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"Put must enforce 0o600 even on overwrite of an existing file")
}

func TestStore_DeleteForArtifact(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	require.NoError(t, s.Put("a", "K1", "v1"))
	require.NoError(t, s.Put("a", "K2", "v2"))
	require.NoError(t, s.Put("b", "K3", "v3"))

	require.NoError(t, s.DeleteForArtifact("a"))

	// Artifact a's secrets should be gone; artifact b's untouched.
	aKeys, err := s.ListForArtifact("a")
	require.NoError(t, err)
	require.Empty(t, aKeys)

	bKeys, err := s.ListForArtifact("b")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"K3"}, bKeys)
}

func TestStore_DeleteForArtifact_NonExistent(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	// Deleting a non-existent artifact's secrets is a no-op (idempotent).
	require.NoError(t, s.DeleteForArtifact("never-existed"))
}

func TestStore_DeleteForArtifact_InvalidID(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	require.Error(t, s.DeleteForArtifact("../escape"))
	require.Error(t, s.DeleteForArtifact(""))
}

func TestStore_Delete_OneKey(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	require.NoError(t, s.Put("a", "K1", "v1"))
	require.NoError(t, s.Put("a", "K2", "v2"))

	require.NoError(t, s.Delete("a", "K1"))

	// K1 gone, K2 still present.
	_, err := s.Get("a", "K1")
	require.Error(t, err)
	v2, err := s.Get("a", "K2")
	require.NoError(t, err)
	require.Equal(t, "v2", v2)
}

func TestStore_Delete_NotFound(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	// Non-existent key returns an os.ErrNotExist-wrapped error.
	err := s.Delete("a", "K-missing")
	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestStore_Delete_PrunesEmptyArtifactDir(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	require.NoError(t, s.Put("a", "K1", "v1"))
	require.NoError(t, s.Delete("a", "K1"))

	// The artifact dir should be gone now (empty after delete).
	_, err := os.Stat(filepath.Join(s.Root, "a"))
	require.True(t, os.IsNotExist(err), "expected artifact dir to be pruned, got %v", err)
}

func TestStore_Delete_InvalidArgs(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	require.Error(t, s.Delete("../escape", "K"))
	require.Error(t, s.Delete("a", "../escape"))
	require.Error(t, s.Delete("", "K"))
	require.Error(t, s.Delete("a", ""))
}

func TestStore_ListAll_Empty(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	pairs, err := s.ListAll()
	require.NoError(t, err)
	require.Empty(t, pairs)
}

func TestStore_ListAll_AcrossArtifacts(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	require.NoError(t, s.Put("a", "K1", "v1"))
	require.NoError(t, s.Put("a", "K2", "v2"))
	require.NoError(t, s.Put("b", "K3", "v3"))

	pairs, err := s.ListAll()
	require.NoError(t, err)
	require.ElementsMatch(t, []Pair{
		{ArtifactID: "a", Key: "K1"},
		{ArtifactID: "a", Key: "K2"},
		{ArtifactID: "b", Key: "K3"},
	}, pairs)
}

func TestStore_ListAll_SkipsNonArtifactEntries(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	require.NoError(t, s.Put("a", "K1", "v1"))

	// Drop a stray file at the secrets root (e.g. user mistakenly placed
	// a value there directly). ListAll should ignore it.
	require.NoError(t, os.WriteFile(filepath.Join(s.Root, "stray.txt"), []byte("nope"), 0o600))

	pairs, err := s.ListAll()
	require.NoError(t, err)
	require.ElementsMatch(t, []Pair{{ArtifactID: "a", Key: "K1"}}, pairs)
}

func TestStore_InvalidArtifactID(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "secrets")}
	require.NoError(t, s.Init())

	cases := []string{"", "../escape", "with/slash", "with\\backslash", ".", "..", "with\x00null"}
	for _, id := range cases {
		require.Error(t, s.Put(id, "K", "v"), "artifact ID %q must be rejected by Put", id)
		_, err := s.Get(id, "K")
		require.Error(t, err, "artifact ID %q must be rejected by Get", id)
		_, err = s.ListForArtifact(id)
		require.Error(t, err, "artifact ID %q must be rejected by ListForArtifact", id)
	}
}
