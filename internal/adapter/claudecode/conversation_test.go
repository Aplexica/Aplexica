package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestImportConversation_WritesArtifactAndEvent(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "projects", "myproj", "session-abc.jsonl")
	content := `{"type":"summary","leafUuid":"01234567-89ab-cdef-0123-456789abcdef","sessionId":"sess-1"}
{"type":"permissions","permissionMode":"default","sessionId":"sess-1"}
{"type":"event","uuid":"event-1","timestamp":"2026-05-20T12:00:00Z"}
`
	writeFile(t, src, content)

	a := New()
	ids, err := a.ImportConversation(context.Background(), s, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := s.ReadArtifact(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindConversation, got.Kind)
	require.Equal(t, "session-abc.jsonl", got.Name)
	require.NotEmpty(t, got.HeadEventHash)

	events, err := s.ReadEvents(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeCreate, events[0].Type)
	require.Equal(t, "claude-code", events[0].Provenance.SourceAgent)
	require.NoError(t, acf.VerifyChain(events))

	payload, err := acf.DecodeConversationPayload(events[0])
	require.NoError(t, err)
	require.Equal(t, "claude-code.session.jsonl", payload.Format)
	require.Equal(t, content, payload.Content)
}

func TestExportConversation_WritesBytesIdenticalToImport(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "in", "session-xyz.jsonl")
	original := `{"type":"summary","leafUuid":"abc","sessionId":"sess-1"}
{"type":"permissions","permissionMode":"default","sessionId":"sess-1"}
{"type":"event","uuid":"event-1","timestamp":"2026-05-20T12:00:00Z","parentUuid":null}
{"type":"event","uuid":"event-2","timestamp":"2026-05-20T12:01:00Z","parentUuid":"event-1"}
`
	writeFile(t, src, original)

	a := New()
	ids, err := a.ImportConversation(context.Background(), s, src)
	require.NoError(t, err)

	dest := filepath.Join(tmp, "out", "session-xyz.jsonl")
	require.NoError(t, a.ExportConversation(context.Background(), s, ids[0], dest))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, original, string(got),
		"conversation round-trip MUST preserve bytes exactly; same fidelity claim as memory and skill")
}

func TestImportConversation_PreservesClaudeDesktopTitleAndRename(t *testing.T) {
	home := t.TempDir()
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())

	cliSessionID := "a5b71172-2a33-4ff3-abb2-22c32758d73d"
	src := filepath.Join(home, ".claude", "projects", "-encoded", cliSessionID+".jsonl")
	writeFile(t, src, `{"type":"user","sessionId":"`+cliSessionID+`","cwd":"/project","message":{"role":"user","content":"hello"}}`+"\n")
	catalog := filepath.Join(home, "desktop-catalog")
	recordPath := filepath.Join(catalog, "project", "local_record.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(recordPath), 0o755))
	writeRecord := func(title string, lastActivity int64) {
		record := `{"sessionId":"local_record","cliSessionId":"` + cliSessionID + `","title":` + quotedClaudeJSON(title) + `,"lastActivityAt":` + fmt.Sprint(lastActivity) + `}`
		require.NoError(t, os.WriteFile(recordPath, []byte(record), 0o600))
		now := time.Now().Add(time.Duration(lastActivity) * time.Nanosecond)
		require.NoError(t, os.Chtimes(recordPath, now, now))
	}
	writeRecord("Testing greeting", 1)

	a := New()
	a.HomeDir = home
	a.DesktopSessionRoots = []string{catalog}
	ids, err := a.ImportConversation(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	art, err := store.ReadArtifact(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Equal(t, "Testing greeting", art.Name)

	// A metadata-only Desktop rename must keep the same artifact id even when
	// the transcript bytes did not change.
	writeRecord("Greeting verified", 2)
	idsAfterRename, err := a.ImportConversation(context.Background(), store, src)
	require.NoError(t, err)
	require.Equal(t, ids, idsAfterRename)
	art, err = store.ReadArtifact(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Equal(t, "Greeting verified", art.Name)
}

func quotedClaudeJSON(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}
