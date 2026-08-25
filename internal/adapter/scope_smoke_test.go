package adapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/stretchr/testify/require"
)

// TestImportOpaque_PopulatesProject_WhenScopeProject (v0.56.0)
// asserts ImportOpaque honors the InferProject callback and writes
// the resolved ProjectInfo onto Artifact.Project. End-to-end smoke
// across the shared opaque pipeline — replaces what would otherwise
// be five near-identical per-adapter tests.
func TestImportOpaque_PopulatesProject_WhenScopeProject(t *testing.T) {
	// Stage a fake git repo so DefaultInferProject finds something.
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, ".git", "config"),
		[]byte(`[remote "origin"]
	url = git@github.com:Example-User/Sample-Repo.git
`), 0o644))

	// Stage a memory-like file inside the fake repo.
	memPath := filepath.Join(repo, "CLAUDE.md")
	require.NoError(t, os.WriteFile(memPath, []byte("# test memory\n"), 0o644))

	// Run ImportOpaque with a fresh store.
	storeRoot := filepath.Join(t.TempDir(), "store")
	s := &acf.Store{Root: storeRoot}
	require.NoError(t, s.Init())

	encoder := func(content []byte) (json.RawMessage, error) {
		// Mimic claudecode memory encoder: MemoryPayload{Format, Content}.
		return json.Marshal(map[string]string{
			"format":  "markdown",
			"content": string(content),
		})
	}

	ids, err := ImportOpaque(context.Background(), s, acf.KindMemory, OpaqueParams{
		DeviceID:       "test-device",
		SourceAgent:    "claude-code",
		AdapterVersion: "test",
		InferScope:     func(string) acf.Scope { return acf.ScopeProject },
		InferProject:   DefaultInferProject,
	}, memPath, encoder)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	// Read back and assert Project was populated correctly.
	got, err := s.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.ScopeProject, got.Scope)
	require.NotNil(t, got.Project, "Project should be set when scope is project + InferProject is wired")
	require.Equal(t, "github.com/example-user/sample-repo", got.Project.ID)
	require.Equal(t, "git", got.Project.VCS)
	physicalRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	require.Equal(t, physicalRepo, got.Project.Path)
}

// TestImportOpaque_NoProject_WhenScopeGlobal asserts global-scope
// imports skip the project-resolution call even when InferProject is
// wired — preserves the v0.55.0-era wire shape for ScopeGlobal
// artifacts.
func TestImportOpaque_NoProject_WhenScopeGlobal(t *testing.T) {
	dir := t.TempDir()
	memPath := filepath.Join(dir, "CLAUDE.md")
	require.NoError(t, os.WriteFile(memPath, []byte("hi"), 0o644))

	storeRoot := filepath.Join(t.TempDir(), "store")
	s := &acf.Store{Root: storeRoot}
	require.NoError(t, s.Init())

	encoder := func(content []byte) (json.RawMessage, error) {
		return json.Marshal(map[string]string{"format": "markdown", "content": string(content)})
	}

	ids, err := ImportOpaque(context.Background(), s, acf.KindMemory, OpaqueParams{
		DeviceID:       "test-device",
		SourceAgent:    "claude-code",
		AdapterVersion: "test",
		InferScope:     func(string) acf.Scope { return acf.ScopeGlobal },
		InferProject:   DefaultInferProject,
	}, memPath, encoder)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := s.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.ScopeGlobal, got.Scope)
	require.Nil(t, got.Project, "Project must stay nil for global-scope imports")
}

// TestImportOpaque_AdHocPath_DowngradesToGlobal (v0.61.0; BRD-02
// §4.13.5): when InferScope returns ScopeProject but the path has
// no VCS marker (.git/.hg walk-up fails), the artifact lands as
// ScopeGlobal with nil Project. Prevents per-directory "projects"
// for ad-hoc workdirs like ~/scratch/, /tmp/play/, Downloads, etc.
func TestImportOpaque_AdHocPath_DowngradesToGlobal(t *testing.T) {
	dir := t.TempDir() // no .git / no .hg → vcs="none"
	memPath := filepath.Join(dir, "CLAUDE.md")
	require.NoError(t, os.WriteFile(memPath, []byte("# adhoc"), 0o644))

	s := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	encoder := func(content []byte) (json.RawMessage, error) {
		return json.Marshal(map[string]string{"format": "markdown", "content": string(content)})
	}

	ids, err := ImportOpaque(context.Background(), s, acf.KindMemory, OpaqueParams{
		DeviceID:       "test-device",
		SourceAgent:    "claude-code",
		AdapterVersion: "test",
		InferScope:     func(string) acf.Scope { return acf.ScopeProject },
		InferProject:   DefaultInferProject,
	}, memPath, encoder)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := s.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.ScopeGlobal, got.Scope,
		"ad-hoc (vcs=none) path MUST downgrade ScopeProject → ScopeGlobal per BRD-02 §4.13.5")
	require.Nil(t, got.Project, "Project must be nil after downgrade")
}

// TestDowngradeAdHocToGlobal exercises the helper directly across
// the four branches (v0.61.0).
func TestDowngradeAdHocToGlobal(t *testing.T) {
	gitInfo := &project.ProjectInfo{ID: "github.com/x/y", VCS: "git"}
	noneInfo := &project.ProjectInfo{ID: "local:abc:dir", VCS: "none"}

	// Project + vcs=none → Global, nil
	s, p := DowngradeAdHocToGlobal(acf.ScopeProject, noneInfo)
	require.Equal(t, acf.ScopeGlobal, s)
	require.Nil(t, p)

	// Project + vcs=git → unchanged
	s, p = DowngradeAdHocToGlobal(acf.ScopeProject, gitInfo)
	require.Equal(t, acf.ScopeProject, s)
	require.Equal(t, gitInfo, p)

	// Global + nil → unchanged
	s, p = DowngradeAdHocToGlobal(acf.ScopeGlobal, nil)
	require.Equal(t, acf.ScopeGlobal, s)
	require.Nil(t, p)

	// Idempotent: second call has the same effect.
	s, p = DowngradeAdHocToGlobal(acf.ScopeProject, noneInfo)
	s, p = DowngradeAdHocToGlobal(s, p)
	require.Equal(t, acf.ScopeGlobal, s)
	require.Nil(t, p)
}
