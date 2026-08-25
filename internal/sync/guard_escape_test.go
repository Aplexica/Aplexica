package syncd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newBareOrch(t *testing.T) *Orchestrator {
	t.Helper()
	root := t.TempDir()
	adapters, store, _ := buildAllThreeAdapters(t, root)
	o, err := NewOrchestrator(Config{Dir: root, Adapters: adapters, Store: store})
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Close() })
	return o
}

// A file larger than maxDestHashBytes could previously never be detected as
// edited-under-us (hashFileCapped bailed, destChangedUnderUs returned false).
// The fingerprint's size/mtime must carry the check for oversized files.
func TestDestChangedUnderUs_OversizedFileUsesMetadata(t *testing.T) {
	o := newBareOrch(t)
	p := filepath.Join(t.TempDir(), "big-session.jsonl")
	require.NoError(t, os.WriteFile(p, make([]byte, maxDestHashBytes+1), 0o644))

	o.recordDestHash(p)
	require.False(t, o.destChangedUnderUs(p), "freshly recorded oversized file must read as unchanged")

	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(`{"type":"user"}` + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.True(t, o.destChangedUnderUs(p),
		"an append to an oversized file must be detected as a real edit")
}

// An atomic rewrite with identical bytes bumps mtime; the hash must prove the
// content is still ours so the rewrite is not misread as a user edit.
func TestDestChangedUnderUs_SameContentRewriteIsNotAnEdit(t *testing.T) {
	o := newBareOrch(t)
	p := filepath.Join(t.TempDir(), "small.jsonl")
	require.NoError(t, os.WriteFile(p, []byte("same bytes\n"), 0o644))

	o.recordDestHash(p)
	later := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(p, later, later))

	require.False(t, o.destChangedUnderUs(p),
		"same content with a newer mtime is not an edit-under-us")
}
