package main

import (
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/project"
	"github.com/stretchr/testify/require"
)

type capturingControllerLogger struct {
	infos []string
	warns []string
}

func (l *capturingControllerLogger) Info(msg string, _ ...any) { l.infos = append(l.infos, msg) }
func (l *capturingControllerLogger) Warn(msg string, _ ...any) { l.warns = append(l.warns, msg) }

// TestRegisterImplicitDirProject_DisplacementRefusalIsWarnOnly pins the
// best-effort contract: when --dir is a second live clone of an already
// registered repository, the AddOrUpdate displacement refusal must be logged
// as a warning and swallowed — never propagated where it could block daemon
// startup — and the first clone's registration must be untouched.
func TestRegisterImplicitDirProject_DisplacementRefusalIsWarnOnly(t *testing.T) {
	stateDir := t.TempDir()
	firstClone := t.TempDir()
	secondClone := t.TempDir()
	writeGitCloneFixture(t, firstClone, "https://github.com/example/dupes.git")
	writeGitCloneFixture(t, secondClone, "https://github.com/example/dupes.git")

	reg, err := project.NewRegistry(filepath.Join(stateDir, "projects.json"))
	require.NoError(t, err)
	info, err := project.Detect(firstClone)
	require.NoError(t, err)
	require.NoError(t, reg.Add(project.Entry{ID: info.ID, Path: info.Path, VCS: info.VCS}))

	lg := &capturingControllerLogger{}
	registerImplicitDirProject(reg, lg, secondClone)

	require.Len(t, lg.warns, 1, "the refusal must surface as exactly one warning")
	require.Contains(t, lg.warns[0], "failed")
	entries := reg.List()
	require.Len(t, entries, 1)
	require.Equal(t, info.Path, entries[0].Path, "the first clone keeps its registration")

	// The success path still registers and logs at info level.
	freshDir := t.TempDir()
	success := &capturingControllerLogger{}
	registerImplicitDirProject(reg, success, freshDir)
	require.Empty(t, success.warns)
	require.Len(t, success.infos, 1)
	require.Len(t, reg.List(), 2)

	// An empty --dir is a silent no-op.
	quiet := &capturingControllerLogger{}
	registerImplicitDirProject(reg, quiet, "")
	require.Empty(t, quiet.infos)
	require.Empty(t, quiet.warns)
}
