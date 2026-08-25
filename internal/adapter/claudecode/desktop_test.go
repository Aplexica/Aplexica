package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestNativeMirrorPaths_ActiveDesktopWorktree(t *testing.T) {
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	worktree := filepath.Join(origin, ".claude", "worktrees", "desktop-one")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /tmp/git/worktrees/desktop-one\n"), 0o644))

	catalog := filepath.Join(home, "desktop-sessions")
	require.NoError(t, os.MkdirAll(catalog, 0o755))
	record := fmt.Sprintf(`{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","cwd":%q,"originCwd":%q,"worktreePath":%q}`, worktree, origin, worktree)
	require.NoError(t, os.WriteFile(filepath.Join(catalog, "local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.json"), []byte(record), 0o644))

	art := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject}
	primary := filepath.Join(origin, "CLAUDE.md")
	got, err := (&Adapter{HomeDir: home, DesktopSessionRoots: []string{catalog}}).NativeMirrorPaths(art, origin, primary)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(worktree, "CLAUDE.md")}, got)

	skill := acf.Artifact{Kind: acf.KindSkill, Scope: acf.ScopeProject, Name: "review.md"}
	skillPrimary := filepath.Join(origin, ".claude", "skills", "review", "SKILL.md")
	got, err = (&Adapter{HomeDir: home, DesktopSessionRoots: []string{catalog}}).NativeMirrorPaths(skill, origin, skillPrimary)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(worktree, ".claude", "skills", "review", "SKILL.md")}, got)
}

func TestNativeMirrorPaths_RejectsArchivedAndUnverifiedDestinations(t *testing.T) {
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	outside := filepath.Join(home, "outside")
	archived := filepath.Join(origin, ".claude", "worktrees", "archived")
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.MkdirAll(archived, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(archived, ".git"), []byte("gitdir: x\n"), 0o644))

	catalog := filepath.Join(home, "desktop-sessions")
	require.NoError(t, os.MkdirAll(catalog, 0o755))
	unsafeRecord := fmt.Sprintf(`{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","originCwd":%q,"cwd":%q}`, origin, outside)
	archivedRecord := fmt.Sprintf(`{"sessionId":"local_bbbbbbbb-cccc-dddd-eeee-ffffffffffff","originCwd":%q,"cwd":%q,"isArchived":true}`, origin, archived)
	require.NoError(t, os.WriteFile(filepath.Join(catalog, "local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.json"), []byte(unsafeRecord), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(catalog, "local_bbbbbbbb-cccc-dddd-eeee-ffffffffffff.json"), []byte(archivedRecord), 0o644))

	art := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject}
	got, err := (&Adapter{HomeDir: home, DesktopSessionRoots: []string{catalog}}).NativeMirrorPaths(art, origin, filepath.Join(origin, "CLAUDE.md"))
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestNativeMirrorPaths_DoesNotMirrorGlobalOrConversations(t *testing.T) {
	a := &Adapter{HomeDir: t.TempDir(), DesktopSessionRoots: []string{}}
	for _, art := range []acf.Artifact{
		{Kind: acf.KindMemory, Scope: acf.ScopeGlobal},
		{Kind: acf.KindConversation, Scope: acf.ScopeProject},
		{Kind: acf.KindTool, Scope: acf.ScopeProject},
	} {
		got, err := a.NativeMirrorPaths(art, "/project", "/project/CLAUDE.md")
		require.NoError(t, err)
		require.Empty(t, got)
	}
}

func TestNativeMirrorPaths_RejectsSymlinkedSkillParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not available to unprivileged Windows CI")
	}
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	worktree := filepath.Join(origin, ".claude", "worktrees", "desktop-one")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: x\n"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(home, "outside"), filepath.Join(worktree, ".claude")))

	catalog := filepath.Join(home, "desktop-sessions")
	require.NoError(t, os.MkdirAll(catalog, 0o755))
	record := fmt.Sprintf(`{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","originCwd":%q,"cwd":%q}`, origin, worktree)
	require.NoError(t, os.WriteFile(filepath.Join(catalog, "local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.json"), []byte(record), 0o644))

	art := acf.Artifact{Kind: acf.KindSkill, Scope: acf.ScopeProject, Name: "review.md"}
	primary := filepath.Join(origin, ".claude", "skills", "review", "SKILL.md")
	got, err := (&Adapter{HomeDir: home, DesktopSessionRoots: []string{catalog}}).NativeMirrorPaths(art, origin, primary)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestNativeMirrorPaths_RejectsWorktreeRootSymlinkEscapingOrigin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not available to unprivileged Windows CI")
	}
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	outsideRoot := filepath.Join(home, "outside-worktrees")
	worktree := filepath.Join(outsideRoot, "desktop-one")
	require.NoError(t, os.MkdirAll(filepath.Join(origin, ".claude"), 0o755))
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: x\n"), 0o644))
	require.NoError(t, os.Symlink(outsideRoot, filepath.Join(origin, ".claude", "worktrees")))

	catalog := filepath.Join(home, "desktop-sessions")
	require.NoError(t, os.MkdirAll(catalog, 0o755))
	lexicalWorktree := filepath.Join(origin, ".claude", "worktrees", "desktop-one")
	record := fmt.Sprintf(`{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","originCwd":%q,"cwd":%q}`, origin, lexicalWorktree)
	require.NoError(t, os.WriteFile(filepath.Join(catalog, "local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.json"), []byte(record), 0o644))

	art := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject}
	got, err := (&Adapter{HomeDir: home, DesktopSessionRoots: []string{catalog}}).NativeMirrorPaths(art, origin, filepath.Join(origin, "CLAUDE.md"))
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestDesktopSessions_CacheInvalidatesWhenRecordCreated(t *testing.T) {
	home := t.TempDir()
	catalog := filepath.Join(home, "desktop-sessions")
	require.NoError(t, os.MkdirAll(catalog, 0o755))

	first := `{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","cwd":"/project-one"}`
	require.NoError(t, os.WriteFile(filepath.Join(catalog, "local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.json"), []byte(first), 0o644))
	a := &Adapter{HomeDir: home, DesktopSessionRoots: []string{catalog}}
	require.Len(t, a.desktopSessions(), 1)

	second := `{"sessionId":"local_bbbbbbbb-cccc-dddd-eeee-ffffffffffff","cwd":"/project-two"}`
	require.NoError(t, os.WriteFile(filepath.Join(catalog, "local_bbbbbbbb-cccc-dddd-eeee-ffffffffffff.json"), []byte(second), 0o644))
	require.Len(t, a.desktopSessions(), 2, "a newly-created record must not wait for a cache TTL")
}

func TestNativeMirrorTopologyToken_TracksMembershipNotActivity(t *testing.T) {
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	worktree := filepath.Join(origin, ".claude", "worktrees", "desktop-one")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: x\n"), 0o644))
	catalog := filepath.Join(home, "desktop-sessions")
	require.NoError(t, os.MkdirAll(catalog, 0o755))
	a := &Adapter{HomeDir: home, DesktopSessionRoots: []string{catalog}}

	empty := a.NativeMirrorTopologyToken()
	path := filepath.Join(catalog, "local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.json")
	record := fmt.Sprintf(`{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","originCwd":%q,"cwd":%q}`, origin, worktree)
	require.NoError(t, os.WriteFile(path, []byte(record), 0o644))
	present := a.NativeMirrorTopologyToken()
	require.NotEqual(t, empty, present)

	activeRecord := fmt.Sprintf(`{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","originCwd":%q,"cwd":%q,"lastActivityAt":123}`, origin, worktree)
	require.NoError(t, os.WriteFile(path, []byte(activeRecord), 0o644))
	require.Equal(t, present, a.NativeMirrorTopologyToken(), "session activity alone must not trigger a context re-fanout")

	archivedRecord := fmt.Sprintf(`{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","originCwd":%q,"cwd":%q,"isArchived":true}`, origin, worktree)
	require.NoError(t, os.WriteFile(path, []byte(archivedRecord), 0o644))
	require.Equal(t, empty, a.NativeMirrorTopologyToken())
}

func TestScanDesktopSessionCandidates_CorruptCandidatesConsumeCap(t *testing.T) {
	catalog := t.TempDir()
	laterCatalog := t.TempDir()
	for _, name := range []string{
		"local_01.json",
		"local_02.json",
		"local_03.json",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(catalog, name), []byte("not json"), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(laterCatalog, "local_04.json"), []byte("not json"), 0o644))

	candidates, _ := scanDesktopSessionCandidates([]string{catalog, laterCatalog}, 2)
	require.Len(t, candidates, 2)
	require.Equal(t, []string{
		filepath.Join(catalog, "local_01.json"),
		filepath.Join(catalog, "local_02.json"),
	}, []string{candidates[0].path, candidates[1].path})
}
