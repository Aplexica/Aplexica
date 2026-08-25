package adapter

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestSafeNativeMirrorFirstContact_HistoricalSkillBytesAreSafe(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, "source", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(src), 0o755))
	require.NoError(t, os.WriteFile(src, []byte("# Probe\n"), 0o644))
	ids, err := ImportSkillReconciled(context.Background(), store, testParams("claude-code"), src, testSkillEncode)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	art, err := store.ReadArtifact(acf.KindSkill, ids[0])
	require.NoError(t, err)

	mirror := filepath.Join(tmp, "worktree", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(mirror), 0o755))
	require.NoError(t, os.WriteFile(mirror, AppendSkillMarker([]byte("# Probe\n"), ids[0]), 0o644))

	safe, err := SafeNativeMirrorFirstContact(store, art, mirror, testSkillDecode, true)
	require.NoError(t, err)
	require.True(t, safe, "a byte-exact prior Aplexica materialization is a safe restart baseline")

	require.NoError(t, os.WriteFile(mirror, AppendSkillMarker([]byte("# Locally edited\n"), ids[0]), 0o644))
	safe, err = SafeNativeMirrorFirstContact(store, art, mirror, testSkillDecode, true)
	require.NoError(t, err)
	require.False(t, safe, "unknown app-side bytes must fail closed")
}

func TestSafeNativeMirrorFirstContact_MissingSafeSymlinkUnsafe(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	art := acf.Artifact{ArtifactID: "missing", Kind: acf.KindSkill}

	missing := filepath.Join(tmp, "missing", "SKILL.md")
	safe, err := SafeNativeMirrorFirstContact(store, art, missing, testSkillDecode, true)
	require.NoError(t, err)
	require.True(t, safe)

	target := filepath.Join(tmp, "target")
	require.NoError(t, os.WriteFile(target, []byte("# target\n"), 0o644))
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows runner cannot create the reparse-point fixture: %v", err)
		}
		require.NoError(t, err)
	}
	safe, err = SafeNativeMirrorFirstContact(store, art, link, testSkillDecode, true)
	require.NoError(t, err)
	require.False(t, safe)
}
