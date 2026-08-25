package openclaw

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestImportConversation_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	jsonl := `{"type":"user","timestamp":"2026-05-21T10:00:00Z","content":"hi"}
{"type":"assistant","timestamp":"2026-05-21T10:00:01Z","content":"hello"}
`
	sessPath := filepath.Join(tmp, "session.jsonl")
	require.NoError(t, os.WriteFile(sessPath, []byte(jsonl), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev"}
	ids, err := a.ImportConversation(context.Background(), store, sessPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	outPath := filepath.Join(tmp, "out.jsonl")
	require.NoError(t, a.ExportConversation(context.Background(), store, ids[0], outPath))

	got, err := os.ReadFile(outPath)
	require.NoError(t, err)
	require.Equal(t, jsonl, string(got), "opaque conversation round-trip must be byte-identical")
}
