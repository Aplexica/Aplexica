//go:build unix

package nativebackup

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSnapshot_LstatOpenSwapToSymlinkCannotEscapeRetainedRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "native")
	source := filepath.Join(root, "session.jsonl")
	outside := filepath.Join(t.TempDir(), "outside-secret")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(source, []byte("inside"), 0o600))
	require.NoError(t, os.WriteFile(outside, []byte("must-never-be-copied"), 0o600))
	dest := filepath.Join(t.TempDir(), "manual")
	var once sync.Once
	var hookErr error
	ctx := context.WithValue(context.Background(), snapshotCopyHooksContextKey{}, &snapshotCopyHooks{
		afterSourceInfo: func(path string) {
			if path != source {
				return
			}
			once.Do(func() {
				if hookErr = os.Rename(source, source+".original"); hookErr == nil {
					hookErr = os.Symlink(outside, source)
				}
			})
		},
	})

	man, err := SnapshotContext(ctx, []AgentRoots{{Name: "agent", Roots: []string{root}}}, dest)
	require.NoError(t, hookErr)
	require.NoError(t, err)
	require.Empty(t, man.Agents[0].Roots, "neither the symlink target nor the moved source may be copied")
	require.Len(t, man.Agents[0].Skipped, 1)
	require.Contains(t, man.Agents[0].Skipped[0].Reason, "open")
	requireSnapshotTreeOmitsBytes(t, dest, []byte("must-never-be-copied"))
}

func TestSnapshot_LstatOpenSwapToFIFODoesNotBlock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "native")
	source := filepath.Join(root, "state.db")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(source, []byte("ordinary"), 0o600))
	dest := filepath.Join(t.TempDir(), "manual")
	var once sync.Once
	var hookErr error
	ctx := context.WithValue(context.Background(), snapshotCopyHooksContextKey{}, &snapshotCopyHooks{
		afterSourceInfo: func(path string) {
			if path != source {
				return
			}
			once.Do(func() {
				if hookErr = os.Rename(source, source+".original"); hookErr == nil {
					hookErr = syscall.Mkfifo(source, 0o600)
				}
			})
		},
	})

	type result struct {
		manifest Manifest
		err      error
	}
	done := make(chan result, 1)
	go func() {
		man, err := SnapshotContext(ctx, []AgentRoots{{Name: "agent", Roots: []string{root}}}, dest)
		done <- result{manifest: man, err: err}
	}()
	select {
	case got := <-done:
		require.NoError(t, hookErr)
		require.NoError(t, got.err)
		require.Empty(t, got.manifest.Agents[0].Roots)
		require.Len(t, got.manifest.Agents[0].Skipped, 1)
		require.Contains(t, got.manifest.Agents[0].Skipped[0].Reason, "open")
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot blocked opening a FIFO swapped in after source inspection")
	}
}

func TestSnapshot_RedactionRootReplacementReadsOnlyRetainedTree(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".openclaw")
	retainedRoot := filepath.Join(base, ".openclaw-retained")
	source := filepath.Join(root, "openclaw.json")
	retained := []byte(`{"profile":"retained-tree","gateway":{"token":"retained-secret"}}`)
	replacement := []byte(`{"profile":"replacement-tree","gateway":{"token":"replacement-secret"}}`)
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(source, retained, 0o600))
	dest := filepath.Join(t.TempDir(), "manual")
	var once sync.Once
	var hookErr error
	ctx := context.WithValue(context.Background(), snapshotCopyHooksContextKey{}, &snapshotCopyHooks{
		afterSourceInfo: func(path string) {
			if path != source {
				return
			}
			once.Do(func() {
				if hookErr = os.Rename(root, retainedRoot); hookErr != nil {
					return
				}
				if hookErr = os.MkdirAll(root, 0o700); hookErr != nil {
					return
				}
				hookErr = os.WriteFile(source, replacement, 0o600)
			})
		},
	})

	man, err := SnapshotContext(ctx, []AgentRoots{{
		Name:        "openclaw",
		Roots:       []string{root},
		RedactFiles: []FileRedaction{{Path: source, Kind: FileRedactionOpenClawConfig}},
	}}, dest)
	require.NoError(t, hookErr)
	require.NoError(t, err)
	require.Len(t, man.Agents[0].Roots, 1)
	require.Empty(t, man.Agents[0].Skipped)
	copied, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(man.Agents[0].Roots[0].Path)))
	require.NoError(t, err)
	require.Contains(t, string(copied), "retained-tree")
	require.NotContains(t, string(copied), "replacement-tree")
	require.NotContains(t, string(copied), "retained-secret")
	require.NotContains(t, string(copied), "replacement-secret")
}

func TestSnapshot_RedactionParentReplacementFailsClosed(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "native")
	parent := filepath.Join(root, "config")
	retainedParent := filepath.Join(root, "config-retained")
	source := filepath.Join(parent, "openclaw.json")
	require.NoError(t, os.MkdirAll(parent, 0o700))
	require.NoError(t, os.WriteFile(source, []byte(`{"profile":"original","gateway":{"token":"original-secret"}}`), 0o600))
	dest := filepath.Join(t.TempDir(), "manual")
	var once sync.Once
	var hookErr error
	ctx := context.WithValue(context.Background(), snapshotCopyHooksContextKey{}, &snapshotCopyHooks{
		afterSourceInfo: func(path string) {
			if path != source {
				return
			}
			once.Do(func() {
				if hookErr = os.Rename(parent, retainedParent); hookErr != nil {
					return
				}
				if hookErr = os.MkdirAll(parent, 0o700); hookErr != nil {
					return
				}
				hookErr = os.WriteFile(source, []byte(`{"profile":"replacement-tree","gateway":{"token":"replacement-secret"}}`), 0o600)
			})
		},
	})

	man, err := SnapshotContext(ctx, []AgentRoots{{
		Name:        "openclaw",
		Roots:       []string{root},
		RedactFiles: []FileRedaction{{Path: source, Kind: FileRedactionOpenClawConfig}},
	}}, dest)
	require.NoError(t, hookErr)
	require.NoError(t, err)
	require.Empty(t, man.Agents[0].Roots, "a replacement below the retained root must not be copied")
	require.Len(t, man.Agents[0].Skipped, 1)
	require.Contains(t, man.Agents[0].Skipped[0].Reason, "unsafe")
	_, err = os.Stat(filepath.Join(dest, "openclaw", relativize(root), "config", "openclaw.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func requireSnapshotTreeOmitsBytes(t *testing.T, root string, forbidden []byte) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Base(path) == ManifestName {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		require.NotEqual(t, forbidden, data, "snapshot copied bytes reached only through an outside-root symlink")
		return nil
	}))
}
