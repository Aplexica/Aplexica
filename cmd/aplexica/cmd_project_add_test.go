package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/project"
	"github.com/stretchr/testify/require"
)

// runProjectCmd resets projectStateDir and project add's local flags,
// then executes `aplexica project …` using the shared rootCmd helper.
// Cobra retains parsed flag values across calls on the global command
// tree, so we must reset before each invocation to avoid cross-test
// contamination (same pattern as runRulesCmd).
func runProjectCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	old := projectStateDir
	// Reset project add's local flags to their defaults before each call.
	if f := projectAddCmd.Flags().Lookup("scope"); f != nil {
		_ = f.Value.Set("local")
		f.Changed = false
	}
	if f := projectAddCmd.Flags().Lookup("agents"); f != nil {
		_ = f.Value.Set("")
		f.Changed = false
	}
	t.Cleanup(func() {
		projectStateDir = old
		if f := projectAddCmd.Flags().Lookup("scope"); f != nil {
			_ = f.Value.Set("local")
			f.Changed = false
		}
		if f := projectAddCmd.Flags().Lookup("agents"); f != nil {
			_ = f.Value.Set("")
			f.Changed = false
		}
	})
	return runRoot(t, append([]string{"project"}, args...)...)
}

func TestProjectAdd_RegistersEntryWithScopeAndAgents(t *testing.T) {
	stateDir := t.TempDir()
	projectDir := t.TempDir()

	out, err := runProjectCmd(t,
		"--state-dir", stateDir,
		"add", projectDir,
		"--scope", "local",
		"--agents", "codex,claude-code",
	)
	require.NoError(t, err, "project add output:\n%s", out)
	require.Contains(t, out, "registered")

	reg, err := project.NewRegistry(filepath.Join(stateDir, "projects.json"))
	require.NoError(t, err)

	entries := reg.List()
	require.Len(t, entries, 1)
	e := entries[0]
	physicalProjectDir, resolveErr := filepath.EvalSymlinks(projectDir)
	require.NoError(t, resolveErr)

	require.NotEmpty(t, e.ID, "ID must be non-empty")
	require.Equal(t, physicalProjectDir, e.Path)
	require.Equal(t, "local", e.Scope)
	// Agents must be sorted and exactly the two we specified.
	require.Equal(t, []string{"claude-code", "codex"}, e.Agents)
}

func TestProjectAdd_RequiresExistingDirectory(t *testing.T) {
	stateDir := t.TempDir()

	_, err := runProjectCmd(t,
		"--state-dir", stateDir,
		"add", filepath.Join(stateDir, "does-not-exist"),
	)
	require.Error(t, err)
}

func TestProjectAdd_RejectsInvalidScope(t *testing.T) {
	stateDir := t.TempDir()
	projectDir := t.TempDir()

	_, err := runProjectCmd(t,
		"--state-dir", stateDir,
		"add", projectDir,
		"--scope", "universe",
	)
	require.Error(t, err)
}

func TestProjectAdd_DefaultsToLocalScope(t *testing.T) {
	stateDir := t.TempDir()
	projectDir := t.TempDir()

	out, err := runProjectCmd(t,
		"--state-dir", stateDir,
		"add", projectDir,
	)
	require.NoError(t, err, "project add output:\n%s", out)

	reg, err := project.NewRegistry(filepath.Join(stateDir, "projects.json"))
	require.NoError(t, err)

	entries := reg.List()
	require.Len(t, entries, 1)
	require.Equal(t, "local", entries[0].EffectiveScope())
}

func TestProjectAdd_IdempotentUpsert(t *testing.T) {
	stateDir := t.TempDir()
	projectDir := t.TempDir()

	for i := range 2 {
		out, err := runProjectCmd(t,
			"--state-dir", stateDir,
			"add", projectDir,
			"--scope", "global",
		)
		require.NoError(t, err, "iteration %d output:\n%s", i, out)
	}

	reg, err := project.NewRegistry(filepath.Join(stateDir, "projects.json"))
	require.NoError(t, err)
	require.Len(t, reg.List(), 1, "idempotent add must not duplicate entries")
}

// writeGitCloneFixture makes dir look like a git clone with the given origin,
// so project.Detect resolves the SAME canonical ID for every fixture sharing
// an origin URL, exercising the silent-displacement regression shape.
func writeGitCloneFixture(t *testing.T, dir, originURL string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o700))
	cfg := "[remote \"origin\"]\n\turl = " + originURL + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o600))
}

// TestProjectAdd_RefusesSecondCloneOfSameRepo: two live clones of one repo
// resolve to one canonical ID. Adding the second must refuse (naming the
// existing path) instead of silently de-registering the first clone.
func TestProjectAdd_RefusesSecondCloneOfSameRepo(t *testing.T) {
	stateDir := t.TempDir()
	firstClone := t.TempDir()
	secondClone := t.TempDir()
	writeGitCloneFixture(t, firstClone, "https://github.com/example/dupes.git")
	writeGitCloneFixture(t, secondClone, "https://github.com/example/dupes.git")

	out, err := runProjectCmd(t, "--state-dir", stateDir, "add", firstClone)
	require.NoError(t, err, "first clone output:\n%s", out)

	physicalFirst, resolveErr := filepath.EvalSymlinks(firstClone)
	require.NoError(t, resolveErr)
	_, err = runProjectCmd(t, "--state-dir", stateDir, "add", secondClone)
	require.ErrorContains(t, err, "already registered at")
	require.ErrorContains(t, err, physicalFirst)

	reg, err := project.NewRegistry(filepath.Join(stateDir, "projects.json"))
	require.NoError(t, err)
	entries := reg.List()
	require.Len(t, entries, 1)
	require.Equal(t, physicalFirst, entries[0].Path, "the first clone keeps its registration")
	require.Equal(t, uint64(1), entries[0].AuthorizationGeneration)
}

// TestProjectAdd_RepointsMovedClone: when the registered clone has physically
// moved, its recorded location is no longer live, so "project add" at the new
// location is the repair path and re-points the entry.
func TestProjectAdd_RepointsMovedClone(t *testing.T) {
	stateDir := t.TempDir()
	parent := t.TempDir()
	original := filepath.Join(parent, "clone")
	movedTo := filepath.Join(parent, "clone-moved")
	require.NoError(t, os.MkdirAll(original, 0o700))
	writeGitCloneFixture(t, original, "https://github.com/example/moved.git")

	out, err := runProjectCmd(t, "--state-dir", stateDir, "add", original)
	require.NoError(t, err, "original output:\n%s", out)

	require.NoError(t, os.Rename(original, movedTo))
	out, err = runProjectCmd(t, "--state-dir", stateDir, "add", movedTo)
	require.NoError(t, err, "moved output:\n%s", out)

	physicalMoved, resolveErr := filepath.EvalSymlinks(movedTo)
	require.NoError(t, resolveErr)
	reg, err := project.NewRegistry(filepath.Join(stateDir, "projects.json"))
	require.NoError(t, err)
	entries := reg.List()
	require.Len(t, entries, 1)
	require.Equal(t, physicalMoved, entries[0].Path)
	require.Equal(t, uint64(2), entries[0].AuthorizationGeneration, "re-pointing is an authorization change")
}

// TestProjectInit_RefusesSecondCloneOfSameRepo: init's Get+Update path must
// not remain a displacement hole once AddOrUpdate refuses — running init
// inside a second live clone must refuse, not silently re-point the first
// clone's registration.
func TestProjectInit_RefusesSecondCloneOfSameRepo(t *testing.T) {
	stateDir := t.TempDir()
	firstClone := t.TempDir()
	secondClone := t.TempDir()
	writeGitCloneFixture(t, firstClone, "https://github.com/example/dupes.git")
	writeGitCloneFixture(t, secondClone, "https://github.com/example/dupes.git")

	out, err := runProjectCmd(t, "--state-dir", stateDir, "add", firstClone)
	require.NoError(t, err, "first clone output:\n%s", out)
	physicalFirst, resolveErr := filepath.EvalSymlinks(firstClone)
	require.NoError(t, resolveErr)

	t.Chdir(secondClone)
	_, err = runProjectCmd(t, "--state-dir", stateDir, "init")
	require.ErrorContains(t, err, "already registered at")
	require.ErrorContains(t, err, physicalFirst)

	reg, err := project.NewRegistry(filepath.Join(stateDir, "projects.json"))
	require.NoError(t, err)
	entries := reg.List()
	require.Len(t, entries, 1)
	require.Equal(t, physicalFirst, entries[0].Path, "the first clone keeps its registration")
}

// TestProjectInit_SameCloneStaysIdempotent: init inside the ALREADY registered
// clone keeps its documented refresh semantics.
func TestProjectInit_SameCloneStaysIdempotent(t *testing.T) {
	stateDir := t.TempDir()
	clone := t.TempDir()
	writeGitCloneFixture(t, clone, "https://github.com/example/solo.git")

	out, err := runProjectCmd(t, "--state-dir", stateDir, "add", clone)
	require.NoError(t, err, "add output:\n%s", out)

	t.Chdir(clone)
	out, err = runProjectCmd(t, "--state-dir", stateDir, "init", "Solo")
	require.NoError(t, err, "init output:\n%s", out)
	require.Contains(t, out, "updated")

	reg, err := project.NewRegistry(filepath.Join(stateDir, "projects.json"))
	require.NoError(t, err)
	entries := reg.List()
	require.Len(t, entries, 1)
	require.Equal(t, "Solo", entries[0].DisplayName)
}

func TestProjectAdd_OutputContainsIDAndPath(t *testing.T) {
	stateDir := t.TempDir()
	projectDir := t.TempDir()

	out, err := runProjectCmd(t,
		"--state-dir", stateDir,
		"add", projectDir,
		"--scope", "global",
	)
	require.NoError(t, err)
	require.Contains(t, out, "global")
	require.True(t,
		strings.Contains(out, projectDir) || strings.Contains(out, filepath.Base(projectDir)),
		"output should mention the path or dir name: %s", out)
}
