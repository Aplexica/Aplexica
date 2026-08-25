package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProjectDirs_ReadsCwdFromSessionLines(t *testing.T) {
	home := t.TempDir()
	pdir := filepath.Join(home, ".claude", "projects", "-Users-testuser-repo")
	require.NoError(t, os.MkdirAll(pdir, 0o755))
	line := `{"type":"user","cwd":"/Users/testuser/repo","timestamp":"2026-06-02T03:00:00Z","message":{"role":"user","content":"hi"}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(pdir, "s.jsonl"), []byte(line), 0o644))

	got, err := (&Adapter{HomeDir: home}).ProjectDirs()
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "/Users/testuser/repo", got[0].Path)
}

func TestProjectDirs_DesktopCatalogNormalizesWorktreeToOrigin(t *testing.T) {
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	worktree := filepath.Join(origin, ".claude", "worktrees", "session-one")
	require.NoError(t, os.MkdirAll(origin, 0o755))
	require.NoError(t, os.MkdirAll(worktree, 0o755))

	cliSessionID := "11111111-2222-3333-4444-555555555555"
	pdir := filepath.Join(home, ".claude", "projects", encodeProjectDir(worktree))
	require.NoError(t, os.MkdirAll(pdir, 0o755))
	line := fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"timestamp":"2026-07-01T03:00:00Z","message":{"role":"user","content":"hi"}}`, cliSessionID, worktree) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(pdir, cliSessionID+".jsonl"), []byte(line), 0o644))

	catalog := filepath.Join(home, "desktop-sessions")
	recordDir := filepath.Join(catalog, "org", "account")
	require.NoError(t, os.MkdirAll(recordDir, 0o755))
	lastActivity := time.Date(2026, 7, 2, 4, 5, 6, 0, time.UTC)
	metadata := fmt.Sprintf(`{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","cliSessionId":%q,"cwd":%q,"originCwd":%q,"lastActivityAt":%d}`, cliSessionID, worktree, origin, lastActivity.UnixMilli())
	require.NoError(t, os.WriteFile(filepath.Join(recordDir, "local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.json"), []byte(metadata), 0o644))

	got, err := (&Adapter{HomeDir: home, DesktopSessionRoots: []string{catalog}}).ProjectDirs()
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, origin, got[0].Path)
	require.Equal(t, lastActivity, got[0].LastActive)
}

func TestProjectDirs_DesktopCatalogWorksWithoutCLITranscript(t *testing.T) {
	home := t.TempDir()
	origin := filepath.Join(home, "src", "desktop-only")
	require.NoError(t, os.MkdirAll(origin, 0o755))
	catalog := filepath.Join(home, "desktop-sessions")
	require.NoError(t, os.MkdirAll(catalog, 0o755))
	metadata := fmt.Sprintf(`{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","cwd":%q,"originCwd":%q,"createdAt":1}`, origin, origin)
	require.NoError(t, os.WriteFile(filepath.Join(catalog, "local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.json"), []byte(metadata), 0o644))

	got, err := (&Adapter{HomeDir: home, DesktopSessionRoots: []string{catalog}}).ProjectDirs()
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, origin, got[0].Path)
}
