package openclaw

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/adaptertest"
	"github.com/stretchr/testify/require"
)

// A synthetic native OpenClaw v3 session transcript: user content is a plain
// string, assistant content is a block array, and tool calls ride as content
// blocks of type "toolCall".
const nativeV3Session = `{"type":"session","version":3,"id":"11111111-2222-4333-8444-555555555555","timestamp":"2026-01-02T03:04:05.000Z","cwd":"/Users/testuser/.openclaw/workspace"}
{"type":"message","id":"m1","parentId":null,"timestamp":"2026-01-02T03:04:05.000Z","message":{"role":"user","content":"What is two plus two?","timestamp":1767323045000,"sourceChannel":"webchat"}}
{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-01-02T03:04:06.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Four."}],"api":"example-api","provider":"example","model":"example-model","stopReason":"stop"}}
{"type":"message","id":"m3","parentId":"m2","timestamp":"2026-01-02T03:04:07.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call_1","name":"bash","arguments":{"command":"true"}}],"stopReason":"toolUse"}}
`

// Outbound conversation sync: a conversation born in OpenClaw's TUI must
// import as canonical v1 events whose text turns the other agents'
// materializers can transcode.
func TestEncodeCanonical_NativeV3Session_YieldsTextTurns(t *testing.T) {
	events, err := EncodeCanonical([]byte(nativeV3Session))
	require.NoError(t, err)
	turns := acf.ExtractTextTurns(events)
	require.Len(t, turns, 2, "user + assistant text turns (tool call rows are not text turns)")
	require.Equal(t, acf.TextTurn{Role: "user", Text: "What is two plus two?"}, turns[0])
	require.Equal(t, acf.TextTurn{Role: "assistant", Text: "Four."}, turns[1])
}

func TestImport_SkipsSessionStoreSidecars(t *testing.T) {
	tmp := t.TempDir()
	a := &Adapter{HomeDir: tmp}
	for _, name := range []string{
		"sessions.json",
		"11111111.trajectory.jsonl",
		"11111111.trajectory-path.json",
		"11111111.jsonl.codex-app-server.json",
	} {
		p := filepath.Join(tmp, name)
		require.NoError(t, os.WriteFile(p, []byte("{}"), 0o600))
		ids, err := a.Import(context.Background(), nil, p)
		require.NoError(t, err, "%s must be skipped, not errored (quarantine strikes)", name)
		require.Empty(t, ids)
	}
}

func TestDiscover_WatchesAgentSessionDirs(t *testing.T) {
	adaptertest.WithCommand(t, "openclaw")
	home := t.TempDir()
	sessions := filepath.Join(home, ".openclaw", "agents", "main", "sessions")
	require.NoError(t, os.MkdirAll(sessions, 0o700))
	// The backend-internal tree must NOT be advertised.
	codexHome := filepath.Join(home, ".openclaw", "agents", "main", "agent", "codex-home", "sessions")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))

	a := &Adapter{HomeDir: home}
	d, err := a.Discover()
	require.NoError(t, err)
	require.Contains(t, d.GlobalRoots, sessions)
	require.NotContains(t, d.GlobalRoots, codexHome,
		"backend-internal codex-home must never be watched (the backend is swappable)")
}
