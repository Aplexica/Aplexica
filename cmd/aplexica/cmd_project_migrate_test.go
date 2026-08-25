package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

func resetProjectMigrationFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		projectMigrationExpectedSHA = ""
		projectMigrationApprovedSHA = ""
		projectMigrationRetainIDs = nil
		projectMigrationRemoveIDs = nil
		for _, command := range []*cobra.Command{projectMigrateV3PlanCmd, projectMigrateV3ApplyCmd} {
			command.Flags().VisitAll(func(flag *pflag.Flag) {
				flag.Changed = false
			})
		}
	})
}

func TestProjectMigrateV3CLIPlanThenApply(t *testing.T) {
	resetProjectMigrationFlags(t)
	stateDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, privatefs.EnsureDir(stateDir, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true}))
	projectDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	legacy, err := json.MarshalIndent(struct {
		Version  string `json:"version"`
		Projects []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
			VCS  string `json:"vcs"`
		} `json:"projects"`
	}{Version: "2", Projects: []struct {
		ID   string `json:"id"`
		Path string `json:"path"`
		VCS  string `json:"vcs"`
	}{{ID: "local:test:project", Path: projectDir, VCS: "none"}}}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, atomicfile.WriteFile(filepath.Join(stateDir, "projects.json"), legacy, 0o600))
	digest := sha256.Sum256(legacy)
	inputSHA := hex.EncodeToString(digest[:])

	out, err := runRoot(t, "project", "--state-dir", stateDir, "migrate-v3", "plan", "--expected-input-sha256", inputSHA)
	require.NoError(t, err, out)
	require.Contains(t, out, "projects=1 active=1 inactive=0 collisions=0 removed=0")
	planSHA := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "plan-sha256: ") {
			planSHA = strings.TrimPrefix(line, "plan-sha256: ")
		}
	}
	require.Len(t, planSHA, 64)

	out, err = runRoot(t, "project", "--state-dir", stateDir, "migrate-v3", "apply", "--approve-plan-sha256", planSHA)
	require.NoError(t, err, out)
	require.Contains(t, out, "projects=1 active=1 inactive=0 collisions=0 tombstones=0")
	registry, err := project.NewRegistry(filepath.Join(stateDir, "projects.json"))
	require.NoError(t, err)
	require.True(t, registry.IsAuthorized("local:test:project", 1))
	auditRaw, err := os.ReadFile(filepath.Join(stateDir, "audit", "events.jsonl"))
	require.NoError(t, err)
	require.Contains(t, string(auditRaw), `"code":"project.registry_v3_migrated"`)
}

func TestProjectMigrateV3CLIRequiresDaemonInstanceExclusion(t *testing.T) {
	resetProjectMigrationFlags(t)
	stateDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, privatefs.EnsureDir(stateDir, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true}))
	legacy := []byte("{\n  \"version\": \"2\",\n  \"projects\": []\n}")
	require.NoError(t, atomicfile.WriteFile(filepath.Join(stateDir, "projects.json"), legacy, 0o600))
	digest := sha256.Sum256(legacy)

	instance, err := daemon.Acquire(filepath.Join(stateDir, "aplexicad.lock"))
	require.NoError(t, err)
	defer instance.Release()
	out, err := runRoot(t, "project", "--state-dir", stateDir, "migrate-v3", "plan",
		"--expected-input-sha256", hex.EncodeToString(digest[:]))
	require.Error(t, err)
	require.Contains(t, out+err.Error(), "requires the daemon to be stopped")
	unchanged, readErr := os.ReadFile(filepath.Join(stateDir, "projects.json"))
	require.NoError(t, readErr)
	require.Equal(t, legacy, unchanged)
}
