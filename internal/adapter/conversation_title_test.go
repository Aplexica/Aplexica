package adapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveConversationTitle_ReadsLatestCodexThreadName(t *testing.T) {
	home := t.TempDir()
	sessionID := "019f5cf2-31f9-7912-9733-55ab9166f16b"
	source := filepath.Join(home, ".codex", "sessions", "2026", "07", "13", "rollout-2026-07-13T15-26-44-"+sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".codex", "session_index.jsonl"), []byte(
		`{"id":"`+sessionID+`","thread_name":"Old title","updated_at":"2026-07-13T19:00:00Z"}`+"\n"+
			`{"id":"`+sessionID+`","thread_name":"  Find   email\ncontext  ","updated_at":"2026-07-13T19:28:31Z"}`+"\n",
	), 0o600))

	got := ResolveConversationTitle(home, "codex", source, filepath.Base(source))
	require.Equal(t, "Find email context", got)
}

func TestResolveConversationTitle_PrefersPortableHumanNameOutsideLocalCodex(t *testing.T) {
	home := t.TempDir()
	require.Equal(t, "Dashboard redesign and branch sync",
		ResolveConversationTitle(home, "codex", "/Users/peer/.codex/sessions/rollout.jsonl", "Dashboard redesign and branch sync"))
	require.Empty(t, ResolveConversationTitle(home, "codex", "/Users/peer/.codex/sessions/rollout.jsonl", "rollout.jsonl"))
}

func TestResolveConversationTitle_RejectsIndexLookupOutsideCodexSessions(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".codex", "session_index.jsonl"), []byte(
		`{"id":"019f5cf2-31f9-7912-9733-55ab9166f16b","thread_name":"Must not leak"}`+"\n",
	), 0o600))

	got := ResolveConversationTitle(home, "codex", filepath.Join(home, "other", "rollout-019f5cf2-31f9-7912-9733-55ab9166f16b.jsonl"), "rollout.jsonl")
	require.Empty(t, got)
}

func TestConversationBranchDisplayTitle(t *testing.T) {
	require.Equal(t, "What is the temperature on Mercury?",
		ConversationBranchDisplayTitle("What is the temperature on Mercury?", "main"))
	require.Equal(t, "[test2] What is the temperature on Mercury?",
		ConversationBranchDisplayTitle("What is the temperature on Mercury?", "Test2"))
	require.Equal(t, "[test2]",
		ConversationBranchDisplayTitle("", "test2"))
	require.Equal(t, "[test2] Already labelled",
		ConversationBranchDisplayTitle("[test2] Already labelled", "test2"))
	require.Equal(t, "Unchanged",
		ConversationBranchDisplayTitle("Unchanged", "!!!"))
}

func TestResolveConversationCWD_NormalizesClaudeDesktopWorktree(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "code", "aplexica")
	worktree := filepath.Join(project, ".claude", "worktrees", "testing-greeting")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	source := filepath.Join(home, ".claude", "projects", "-encoded-project", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	require.NoError(t, os.WriteFile(source, []byte(`{"type":"user","cwd":`+quotedJSON(worktree)+`}`+"\n"), 0o600))

	require.Equal(t, project, ResolveConversationCWD(home, "claude-code", source))
}

func TestResolveConversationCWD_RejectsForeignSourcePath(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "code", "aplexica")
	require.NoError(t, os.MkdirAll(project, 0o755))
	source := filepath.Join(home, "downloads", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	require.NoError(t, os.WriteFile(source, []byte(`{"cwd":`+quotedJSON(project)+`}`+"\n"), 0o600))

	require.Empty(t, ResolveConversationCWD(home, "claude-code", source))
}

func quotedJSON(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}
