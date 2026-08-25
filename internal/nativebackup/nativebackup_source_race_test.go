package nativebackup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSnapshot_RetainedSourceCopiesStableRegularFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "native")
	source := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(source, []byte("stable conversation"), 0o600))
	dest := filepath.Join(t.TempDir(), "manual")

	man, err := Snapshot([]AgentRoots{{Name: "agent", Roots: []string{root}}}, dest)
	require.NoError(t, err)
	require.Len(t, man.Agents, 1)
	require.Len(t, man.Agents[0].Roots, 1)
	require.Empty(t, man.Agents[0].Skipped)
	entry := man.Agents[0].Roots[0]
	got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(entry.Path)))
	require.NoError(t, err)
	require.Equal(t, []byte("stable conversation"), got)
	require.Equal(t, int64(len(got)), entry.Bytes)
}

func TestSnapshot_SourceGrowthAfterOpenIsSkippedWithoutCommitting(t *testing.T) {
	root := filepath.Join(t.TempDir(), "native")
	source := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(source, []byte("before"), 0o600))
	dest := filepath.Join(t.TempDir(), "manual")
	mutated := false
	ctx := context.WithValue(context.Background(), snapshotCopyHooksContextKey{}, &snapshotCopyHooks{
		afterSourceOpen: func(path string, _ *os.File) {
			if path != source || mutated {
				return
			}
			mutated = true
			require.NoError(t, os.WriteFile(source, []byte("changed-and-longer"), 0o600))
		},
	})

	man, err := SnapshotContext(ctx, []AgentRoots{{Name: "agent", Roots: []string{root}}}, dest)
	require.NoError(t, err)
	require.True(t, mutated)
	require.Empty(t, man.Agents[0].Roots)
	require.Len(t, man.Agents[0].Skipped, 1)
	require.Contains(t, man.Agents[0].Skipped[0].Reason, "source changed")
	_, err = os.Stat(filepath.Join(dest, "agent", relativize(root), filepath.Base(source)))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestSnapshot_SameSizeSourceMutationAfterOpenIsSkipped(t *testing.T) {
	root := filepath.Join(t.TempDir(), "native")
	source := filepath.Join(root, "state.db")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(source, []byte("AAAA"), 0o600))
	dest := filepath.Join(t.TempDir(), "manual")
	mutated := false
	ctx := context.WithValue(context.Background(), snapshotCopyHooksContextKey{}, &snapshotCopyHooks{
		afterSourceOpen: func(path string, _ *os.File) {
			if path != source || mutated {
				return
			}
			mutated = true
			require.NoError(t, os.WriteFile(source, []byte("BBBB"), 0o600))
			future := time.Now().Add(2 * time.Hour)
			require.NoError(t, os.Chtimes(source, future, future))
		},
	})

	man, err := SnapshotContext(ctx, []AgentRoots{{Name: "agent", Roots: []string{root}}}, dest)
	require.NoError(t, err)
	require.True(t, mutated)
	require.Empty(t, man.Agents[0].Roots)
	require.Len(t, man.Agents[0].Skipped, 1)
	require.Contains(t, man.Agents[0].Skipped[0].Reason, "source changed")
}
