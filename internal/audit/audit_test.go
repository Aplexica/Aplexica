package audit

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileRecorderTransactionIsDurableAndIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "audit")
	r := &FileRecorder{Root: root}
	f, err := SafeID("project_id", "project-a")
	require.NoError(t, err)
	id := "0197f30a-3c58-7000-8000-000000000001"
	e := Event{Code: "project.revoked", Fields: []Field{f}}
	require.NoError(t, r.BeginTransaction(context.Background(), id, e))
	require.NoError(t, r.BeginTransaction(context.Background(), id, e))
	require.NoError(t, r.CompleteTransaction(context.Background(), id, "success"))
	// A new process can repeat both calls without duplicating the terminal
	// record. The completed index is part of the durable idempotency contract.
	r = &FileRecorder{Root: root}
	require.NoError(t, r.BeginTransaction(context.Background(), id, e))
	require.NoError(t, r.CompleteTransaction(context.Background(), id, "success"))
	data, err := os.ReadFile(filepath.Join(root, "events.jsonl"))
	require.NoError(t, err)
	require.Contains(t, string(data), `"code":"project.revoked"`)
	require.Contains(t, string(data), `"outcome":"success"`)
	require.Equal(t, 1, bytes.Count(data, []byte(`"transactionId":"`+id+`"`)))
	info, err := os.Stat(filepath.Join(root, "events.jsonl"))
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Zero(t, info.Mode().Perm()&0o077)
	}
}

func TestRecorderRejectsRawOrUnknownFields(t *testing.T) {
	r := &MemoryRecorder{}
	err := r.Record(context.Background(), Event{Code: "made.up", Outcome: "success"})
	require.Error(t, err)
	_, err = SafeID("token", "contains secret spaces")
	require.Error(t, err)
}
