package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestNativeMirrorPaths_DefaultManagedWorktree_MemoryAndSkill(t *testing.T) {
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	worktree := makeCodexLinkedWorktree(t, home, origin, filepath.Join("opaque", "thread-id", "project"))
	a := &Adapter{HomeDir: home}

	memory := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject}
	got, err := a.NativeMirrorPaths(memory, origin, filepath.Join(origin, "AGENTS.md"))
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(worktree, "AGENTS.md")}, got)

	skill := acf.Artifact{Kind: acf.KindSkill, Scope: acf.ScopeProject, Name: "review.md"}
	got, err = a.NativeMirrorPaths(skill, origin, filepath.Join(origin, ".agents", "skills", "review", "SKILL.md"))
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(worktree, ".agents", "skills", "review", "SKILL.md")}, got)
}

func TestNativeMirrorPaths_OnlyProjectMemoryAndSkills(t *testing.T) {
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	makeCodexLinkedWorktree(t, home, origin, filepath.Join("thread", "project"))
	a := &Adapter{HomeDir: home}

	for _, artifact := range []acf.Artifact{
		{Kind: acf.KindMemory, Scope: acf.ScopeGlobal},
		{Kind: acf.KindSkill, Scope: acf.ScopeGlobal},
		{Kind: acf.KindTool, Scope: acf.ScopeProject},
		{Kind: acf.KindConversation, Scope: acf.ScopeProject},
	} {
		got, err := a.NativeMirrorPaths(artifact, origin, filepath.Join(origin, "AGENTS.md"))
		require.NoError(t, err)
		require.Empty(t, got)
	}

	projectMemory := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject}
	got, err := a.NativeMirrorPaths(projectMemory, origin, filepath.Join(home, "outside", "AGENTS.md"))
	require.NoError(t, err)
	require.Empty(t, got, "a primary path outside contextDir must never be mirrored")
}

func TestNativeMirrorPaths_RejectsWrongOriginAndMalformedGitMetadata(t *testing.T) {
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	otherOrigin := filepath.Join(home, "src", "other")
	makeCodexLinkedWorktree(t, home, origin, filepath.Join("a", "project"))
	require.NoError(t, os.MkdirAll(otherOrigin, 0o755))

	// This candidate points at a common directory whose basename is not .git.
	// It looks structurally similar but cannot identify a primary checkout.
	bad := filepath.Join(home, ".codex", "worktrees", "b", "project")
	badAdmin := filepath.Join(home, "fake-git", "worktrees", "bad")
	require.NoError(t, os.MkdirAll(bad, 0o755))
	require.NoError(t, os.MkdirAll(badAdmin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bad, ".git"), []byte("gitdir: "+badAdmin+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(badAdmin, "commondir"), []byte("../..\n"), 0o644))

	artifact := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject}
	got, err := (&Adapter{HomeDir: home}).NativeMirrorPaths(artifact, otherOrigin, filepath.Join(otherOrigin, "AGENTS.md"))
	require.NoError(t, err)
	require.Empty(t, got)

	worktrees := (&Adapter{HomeDir: home}).managedWorktrees()
	require.Len(t, worktrees, 1, "malformed git metadata must not become a mirror target")
	require.True(t, sameCodexPath(worktrees[0].Origin, origin))
}

func TestNativeMirrorPaths_RejectsSymlinkedRootAndNestedEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not available to unprivileged Windows CI")
	}
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	outsideRoot := filepath.Join(home, "outside-root")
	makeCodexLinkedWorktreeAtRoot(t, outsideRoot, origin, filepath.Join("thread", "project"))
	symlinkedRoot := filepath.Join(home, "linked-root")
	require.NoError(t, os.Symlink(outsideRoot, symlinkedRoot))

	artifact := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject}
	got, err := (&Adapter{HomeDir: home, WorktreeRoots: []string{symlinkedRoot}}).NativeMirrorPaths(
		artifact, origin, filepath.Join(origin, "AGENTS.md"),
	)
	require.NoError(t, err)
	require.Empty(t, got, "a symlinked managed root must fail closed")

	managedRoot := filepath.Join(home, "managed-root")
	require.NoError(t, os.MkdirAll(managedRoot, 0o755))
	require.NoError(t, os.Symlink(outsideRoot, filepath.Join(managedRoot, "escape")))
	got, err = (&Adapter{HomeDir: home, WorktreeRoots: []string{managedRoot}}).NativeMirrorPaths(
		artifact, origin, filepath.Join(origin, "AGENTS.md"),
	)
	require.NoError(t, err)
	require.Empty(t, got, "the scanner must not follow a nested directory symlink")
}

func TestNativeMirrorPaths_RejectsSymlinkedDestinationParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not available to unprivileged Windows CI")
	}
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	worktree := makeCodexLinkedWorktree(t, home, origin, filepath.Join("thread", "project"))
	require.NoError(t, os.Symlink(filepath.Join(home, "outside"), filepath.Join(worktree, ".agents")))

	artifact := acf.Artifact{Kind: acf.KindSkill, Scope: acf.ScopeProject, Name: "review.md"}
	got, err := (&Adapter{HomeDir: home}).NativeMirrorPaths(
		artifact, origin, filepath.Join(origin, ".agents", "skills", "review", "SKILL.md"),
	)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestScanCodexWorktreeRoot_TraversalCap(t *testing.T) {
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	root := filepath.Join(home, "managed")
	makeCodexLinkedWorktreeAtRoot(t, root, origin, filepath.Join("a", "b", "project"))

	// root, a consume the two allowed callbacks; b/project cannot be visited.
	require.Empty(t, scanCodexWorktreeRoot(root, 2, 10))
	require.Len(t, scanCodexWorktreeRoot(root, 100, 10), 1)
}

func TestNativeMirrorFirstContactSafe_PreservesLocalEdit(t *testing.T) {
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	worktree := makeCodexLinkedWorktree(t, home, origin, filepath.Join("thread", "project"))
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	source := filepath.Join(origin, "AGENTS.md")
	writeFile(t, source, "canonical\n")
	a := &Adapter{HomeDir: home, DeviceID: "device"}
	ids, err := a.ImportMemory(context.Background(), store, source)
	require.NoError(t, err)
	artifact, err := store.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)

	mirror := filepath.Join(worktree, "AGENTS.md")
	writeFile(t, mirror, "desktop local edit\n")
	safe, err := a.NativeMirrorFirstContactSafe(store, artifact, mirror)
	require.NoError(t, err)
	require.False(t, safe)

	writeFile(t, mirror, "canonical\n")
	safe, err = a.NativeMirrorFirstContactSafe(store, artifact, mirror)
	require.NoError(t, err)
	require.True(t, safe, "an earlier Aplexica materialization remains safe after restart")

	missing := filepath.Join(worktree, "new", "AGENTS.md")
	safe, err = a.NativeMirrorFirstContactSafe(store, artifact, missing)
	require.NoError(t, err)
	require.True(t, safe)

	safe, err = a.NativeMirrorFirstContactSafe(store, acf.Artifact{Kind: acf.KindTool}, mirror)
	require.NoError(t, err)
	require.False(t, safe)
}

func TestNormalizeManagedWorktreeCwd(t *testing.T) {
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	worktree := makeCodexLinkedWorktree(t, home, origin, filepath.Join("thread", "project"))
	worktrees := (&Adapter{HomeDir: home}).managedWorktrees()
	require.Len(t, worktrees, 1)

	require.True(t, sameCodexPath(normalizeManagedWorktreeCwd(worktree, worktrees), origin))
	require.True(t, sameCodexPath(
		normalizeManagedWorktreeCwd(filepath.Join(worktree, "pkg"), worktrees),
		filepath.Join(origin, "pkg"),
	))
	require.Equal(t, filepath.Join(home, "unrelated"), normalizeManagedWorktreeCwd(filepath.Join(home, "unrelated"), worktrees))
}

func TestManagedWorktrees_EmptyOverrideDisablesDefault(t *testing.T) {
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	makeCodexLinkedWorktree(t, home, origin, filepath.Join("thread", "project"))
	require.Empty(t, (&Adapter{HomeDir: home, WorktreeRoots: []string{}}).managedWorktrees())
}

func TestNativeMirrorTopologyToken_ChangesWithManagedWorktrees(t *testing.T) {
	home := t.TempDir()
	a := &Adapter{HomeDir: home}
	empty := a.NativeMirrorTopologyToken()
	origin := filepath.Join(home, "src", "project")
	makeCodexLinkedWorktree(t, home, origin, filepath.Join("thread", "project"))
	present := a.NativeMirrorTopologyToken()
	require.NotEqual(t, empty, present)
	require.Equal(t, present, a.NativeMirrorTopologyToken(), "stable topology must produce a stable token")
}

func makeCodexLinkedWorktree(t *testing.T, home, origin, rel string) string {
	t.Helper()
	return makeCodexLinkedWorktreeAtRoot(t, filepath.Join(home, ".codex", "worktrees"), origin, rel)
}

func makeCodexLinkedWorktreeAtRoot(t *testing.T, root, origin, rel string) string {
	t.Helper()
	worktree := filepath.Join(root, rel)
	id := strings.NewReplacer(string(filepath.Separator), "-", " ", "-").Replace(rel)
	admin := filepath.Join(origin, ".git", "worktrees", id)
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.MkdirAll(admin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"), []byte(fmt.Sprintf("gitdir: %s\n", admin)), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o644))
	return worktree
}
