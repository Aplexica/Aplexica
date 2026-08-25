package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestImportConversation_WritesArtifactAndEvent(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "sessions", "rollout-test.jsonl")
	content := `{"timestamp":"2026-05-20T12:00:00Z","type":"user","payload":{"text":"hi"}}
{"timestamp":"2026-05-20T12:00:01Z","type":"assistant","payload":{"text":"hello"}}
`
	writeFile(t, src, content)

	a := New()
	ids, err := a.ImportConversation(context.Background(), s, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := s.ReadArtifact(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindConversation, got.Kind)
	require.Equal(t, "rollout-test.jsonl", got.Name)

	events, err := s.ReadEvents(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "codex", events[0].Provenance.SourceAgent)
	require.NoError(t, acf.VerifyChain(events))

	payload, err := acf.DecodeConversationPayload(events[0])
	require.NoError(t, err)
	require.Equal(t, "codex.session.jsonl", payload.Format)
	require.Equal(t, content, payload.Content)
}

func TestExportConversation_WritesBytesIdenticalToImport(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "in", "rollout.jsonl")
	original := `{"timestamp":"2026-05-20T12:00:00Z","type":"user","payload":{"text":"hi"}}` + "\n"
	writeFile(t, src, original)

	a := New()
	ids, err := a.ImportConversation(context.Background(), s, src)
	require.NoError(t, err)

	dest := filepath.Join(tmp, "out", "rollout.jsonl")
	require.NoError(t, a.ExportConversation(context.Background(), s, ids[0], dest))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, original, string(got))
}

func TestImportConversation_PreservesCodexDesktopThreadNameAndRename(t *testing.T) {
	home := t.TempDir()
	s := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, s.Init())
	sessionID := "019f5d5b-878c-7222-816a-e3453328eb00"
	src := filepath.Join(home, ".codex", "sessions", "2026", "07", "13", "rollout-2026-07-13T17-25-00-"+sessionID+".jsonl")
	writeFile(t, src, codexConvLine("user", "Build native app adapters."))
	index := filepath.Join(home, ".codex", "session_index.jsonl")
	require.NoError(t, os.WriteFile(index, []byte(`{"id":"`+sessionID+`","thread_name":"Add native app adapters"}`+"\n"), 0o600))

	a := New()
	a.HomeDir = home
	a.CanonicalConversations = true
	ids, err := a.ImportConversation(context.Background(), s, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	art, err := s.ReadArtifact(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Equal(t, "Add native app adapters", art.Name)

	// A native rename is metadata-only in Codex, but ACF records a full-state
	// update so the portable display title fans out without duplicating turns.
	require.NoError(t, os.WriteFile(index, []byte(
		`{"id":"`+sessionID+`","thread_name":"Add native app adapters"}`+"\n"+
			`{"id":"`+sessionID+`","thread_name":"Fix native app conversation titles"}`+"\n",
	), 0o600))
	ids2, err := a.ImportConversation(context.Background(), s, src)
	require.NoError(t, err)
	require.Equal(t, ids, ids2)
	art, err = s.ReadArtifact(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Equal(t, "Fix native app conversation titles", art.Name)
	events, err := s.ReadEvents(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 3)
	require.NoError(t, acf.VerifyChain(events))
}
