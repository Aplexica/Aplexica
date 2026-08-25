package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/project"
	"github.com/stretchr/testify/require"
)

func TestProjectListNeverMutatesRegistry(t *testing.T) {
	oldStateDir := projectStateDir
	t.Cleanup(func() { projectStateDir = oldStateDir })
	projectStateDir = t.TempDir()
	registryPath := filepath.Join(projectStateDir, "projects.json")
	registry, err := project.NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, registry.Add(project.Entry{ID: "local:abc:repo", Path: t.TempDir(), VCS: "none"}))

	var output bytes.Buffer
	oldOutput := projectListCmd.OutOrStdout()
	projectListCmd.SetOut(&output)
	t.Cleanup(func() { projectListCmd.SetOut(oldOutput) })
	require.NoError(t, projectListCmd.Args(projectListCmd, nil))
	require.NoError(t, projectListCmd.RunE(projectListCmd, nil))

	reloaded, err := project.NewRegistry(registryPath)
	require.NoError(t, err)
	_, present := reloaded.Get("local:abc:repo")
	require.True(t, present, "a read-only project list must never remove a project")
	require.Contains(t, output.String(), "local:abc:repo")
}

func TestProjectListRejectsArguments(t *testing.T) {
	require.Error(t, projectListCmd.Args(projectListCmd, []string{"local:must-not-be-removed"}))
}
