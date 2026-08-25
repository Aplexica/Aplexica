package project

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/stretchr/testify/require"
)

func migrationTestStateDir(t *testing.T) string {
	t.Helper()
	dir := registryTestDir(t)
	require.NoError(t, privatefs.EnsureDir(dir, privatefs.DirPolicy{
		Access:      privatefs.AccessPrivate,
		RepairOwned: true,
	}))
	return dir
}

func writeMigrationV2(t *testing.T, stateDir string, entries []registryV2Entry) ([]byte, string) {
	t.Helper()
	raw, err := json.MarshalIndent(registryV2State{Version: "2", Projects: entries}, "", "  ")
	require.NoError(t, err)
	writePrivateRegistryTestFile(t, filepath.Join(stateDir, "projects.json"), raw)
	return raw, sha256Hex(raw)
}

func requireRegistryTestSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows test token has no symlink privilege: %v", err)
		}
		require.NoError(t, err)
	}
}

func fixedMigrationTime() time.Time {
	return time.Date(2026, 7, 11, 15, 4, 5, 123456000, time.UTC)
}

func TestRegistryV3MigrationPlanApplyRealShapeWithInactivePath(t *testing.T) {
	stateDir := migrationTestStateDir(t)
	active := registryTestDir(t)
	missing := filepath.Join(registryTestDir(t), "removed-worktree")
	source, sourceSHA := writeMigrationV2(t, stateDir, []registryV2Entry{
		{ID: "active-project", Path: active, VCS: "git", Scope: "global", Agents: []string{"codex"}},
		{ID: "missing-project", Path: missing, VCS: "none"},
	})

	planned, err := CreateRegistryV3MigrationPlan(RegistryV3PlanOptions{StateDir: stateDir,
		ExpectedInputSHA256: sourceSHA, PlannedAt: fixedMigrationTime()})
	require.NoError(t, err)
	require.Equal(t, 2, planned.ProjectCount)
	require.Equal(t, 1, planned.ActiveCount)
	require.Equal(t, 1, planned.InactiveCount)
	require.Equal(t, source, mustRead(t, filepath.Join(stateDir, "projects.json")), "planning must not mutate the registry")

	applied, err := ApplyRegistryV3Migration(RegistryV3ApplyOptions{StateDir: stateDir, ApprovedPlanSHA256: planned.PlanSHA256})
	require.NoError(t, err)
	require.Equal(t, 1, applied.ActiveCount)
	require.Equal(t, 1, applied.InactiveCount)
	require.Equal(t, source, mustRead(t, applied.BackupPath), "backup must preserve exact pre-v3 bytes")
	require.Equal(t, sourceSHA, sha256Hex(mustRead(t, applied.BackupPath)))
	var emptyReport RegistryV3CollisionReport
	emptyReportRaw := mustRead(t, applied.CollisionReportPath)
	require.NoError(t, decodeStrictJSON(emptyReportRaw, &emptyReport))
	require.Equal(t, planned.PlanSHA256, emptyReport.PlanSHA256)
	require.NotNil(t, emptyReport.Collisions)
	require.Empty(t, emptyReport.Collisions)
	require.Contains(t, string(emptyReportRaw), `"collisions":[]`)

	registry, err := NewRegistry(applied.RegistryPath)
	require.NoError(t, err)
	require.Equal(t, uint64(1), registry.Revision())
	require.Len(t, registry.List(), 1)
	require.Len(t, registry.ListAll(), 2)
	allMigrated := registry.ListAll()
	require.Equal(t, "global", allMigrated[0].Scope)
	require.Equal(t, []string{"codex"}, allMigrated[0].Agents)
	require.Equal(t, "local", allMigrated[1].Scope)
	require.True(t, registry.IsAuthorized("active-project", 1))
	require.False(t, registry.IsAuthorized("missing-project", 1))
	_, ok := registry.Get("missing-project")
	require.False(t, ok, "inactive entries must fail closed through Get")

	// An inactive path reappearing cannot reactivate authority without an
	// explicit AddOrUpdate generation transition.
	require.NoError(t, os.MkdirAll(missing, 0o700))
	reloaded, err := NewRegistry(applied.RegistryPath)
	require.NoError(t, err)
	require.Len(t, reloaded.List(), 1)
	require.False(t, reloaded.IsAuthorized("missing-project", 1))
	all := reloaded.ListAll()
	require.True(t, all[1].Inactive)
	require.Nil(t, all[1].FileIdentity)
}

func TestRegistryV3MigrationCollisionRequiresCompleteBoundResolution(t *testing.T) {
	stateDir := migrationTestStateDir(t)
	physical := registryTestDir(t)
	alias := filepath.Join(registryTestDir(t), "alias")
	requireRegistryTestSymlink(t, physical, alias)
	_, sourceSHA := writeMigrationV2(t, stateDir, []registryV2Entry{
		{ID: "local:0f8933:testuser", Path: physical, VCS: "none"},
		{ID: "local:ed449f:exampleuser", Path: alias, VCS: "none"},
	})
	base := RegistryV3PlanOptions{StateDir: stateDir, ExpectedInputSHA256: sourceSHA, PlannedAt: fixedMigrationTime()}

	_, err := CreateRegistryV3MigrationPlan(base)
	require.ErrorContains(t, err, "partial")
	partial := base
	partial.RetainIDs = []string{"local:0f8933:testuser"}
	_, err = CreateRegistryV3MigrationPlan(partial)
	require.ErrorContains(t, err, "partial")
	unknown := base
	unknown.RetainIDs = []string{"local:0f8933:testuser"}
	unknown.RemoveIDs = []string{"not-in-collision"}
	_, err = CreateRegistryV3MigrationPlan(unknown)
	require.Error(t, err)

	base.RetainIDs = []string{"local:0f8933:testuser"}
	base.RemoveIDs = []string{"local:ed449f:exampleuser"}
	planned, err := CreateRegistryV3MigrationPlan(base)
	require.NoError(t, err)
	require.Equal(t, 1, planned.CollisionCount)
	require.Equal(t, 1, planned.RemovedCount)
	applied, err := ApplyRegistryV3Migration(RegistryV3ApplyOptions{StateDir: stateDir, ApprovedPlanSHA256: planned.PlanSHA256})
	require.NoError(t, err)
	require.Equal(t, 1, applied.ProjectCount)
	require.Equal(t, 1, applied.TombstoneCount)

	var state registryState
	require.NoError(t, decodeStrictJSON(mustRead(t, applied.RegistryPath), &state))
	require.Equal(t, "local:0f8933:testuser", state.Projects[0].ID)
	require.Equal(t, physical, state.Projects[0].Path)
	require.Equal(t, uint64(1), state.Projects[0].AuthorizationGeneration)
	require.Equal(t, "local:ed449f:exampleuser", state.Tombstones[0].ID)
	require.Equal(t, uint64(2), state.Tombstones[0].AuthorizationGeneration)
	require.Equal(t, fixedMigrationTime(), state.Tombstones[0].RemovedAt)

	var report RegistryV3CollisionReport
	reportRaw := mustRead(t, applied.CollisionReportPath)
	require.NoError(t, decodeStrictJSON(reportRaw, &report))
	canonicalReport, err := json.Marshal(report)
	require.NoError(t, err)
	require.Equal(t, canonicalReport, reportRaw)
	require.Equal(t, planned.PlanSHA256, report.PlanSHA256)
	require.Equal(t, "local:0f8933:testuser", report.Collisions[0].RetainedID)
	require.Equal(t, []string{"local:ed449f:exampleuser"}, report.Collisions[0].RemovedIDs)
}

func TestRegistryV3MigrationCaseFoldedMissingPathCollisionPolicy(t *testing.T) {
	stateDir := migrationTestStateDir(t)
	parent := registryTestDir(t)
	_, sourceSHA := writeMigrationV2(t, stateDir, []registryV2Entry{
		{ID: "first", Path: filepath.Join(parent, "FutureProject"), VCS: "none"},
		{ID: "second", Path: filepath.Join(parent, "futureproject"), VCS: "none"},
	})
	options := RegistryV3PlanOptions{StateDir: stateDir, ExpectedInputSHA256: sourceSHA, PlannedAt: fixedMigrationTime()}
	_, err := CreateRegistryV3MigrationPlan(options)
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		require.NoError(t, err)
		return
	}
	require.ErrorContains(t, err, "partial")
	options.RetainIDs = []string{"first"}
	options.RemoveIDs = []string{"second"}
	result, err := CreateRegistryV3MigrationPlan(options)
	require.NoError(t, err)
	require.Equal(t, 1, result.CollisionCount)
}

func TestRegistryV3CollisionGroupsUnionsPathAndIdentityKeys(t *testing.T) {
	firstIdentity := FileIdentity{Platform: "unix", UnixDevice: 1, UnixInode: 10}
	secondIdentity := FileIdentity{Platform: "unix", UnixDevice: 1, UnixInode: 20}
	entries := []Entry{
		{ID: "a", Path: "/projects/one", FileIdentity: &firstIdentity},
		{ID: "b", Path: "/projects/one", FileIdentity: &secondIdentity},
		{ID: "c", Path: "/projects/two", FileIdentity: &secondIdentity},
	}
	require.Equal(t, [][]string{{"a", "b", "c"}}, registryV3CollisionGroups(entries),
		"overlapping path and physical-identity collisions must be one reviewed decision")

	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		entries = []Entry{
			{ID: "upper", Path: "/Volumes/CaseSensitive/Project", FileIdentity: &firstIdentity},
			{ID: "lower", Path: "/volumes/casesensitive/project", FileIdentity: &secondIdentity},
		}
		require.Equal(t, [][]string{{"lower", "upper"}}, registryV3CollisionGroups(entries),
			"case-folded runtime path collisions must be present in the approved plan even when file identities differ")
	}
}

func TestRegistryV3MigrationRejectsChangedInputAndPhysicalIdentity(t *testing.T) {
	t.Run("input bytes", func(t *testing.T) {
		stateDir := migrationTestStateDir(t)
		path := registryTestDir(t)
		_, sourceSHA := writeMigrationV2(t, stateDir, []registryV2Entry{{ID: "one", Path: path, VCS: "git"}})
		plan, err := CreateRegistryV3MigrationPlan(RegistryV3PlanOptions{StateDir: stateDir,
			ExpectedInputSHA256: sourceSHA, PlannedAt: fixedMigrationTime()})
		require.NoError(t, err)
		changed := []byte(strings.Replace(string(mustRead(t, filepath.Join(stateDir, "projects.json"))), `"git"`, `"hg"`, 1))
		writePrivateRegistryTestFile(t, filepath.Join(stateDir, "projects.json"), changed)
		_, err = ApplyRegistryV3Migration(RegistryV3ApplyOptions{StateDir: stateDir, ApprovedPlanSHA256: plan.PlanSHA256})
		require.ErrorContains(t, err, "changed")
		_, statErr := os.Stat(filepath.Join(stateDir, registryV2BackupFilename(sourceSHA)))
		require.ErrorIs(t, statErr, os.ErrNotExist)
	})

	t.Run("directory identity", func(t *testing.T) {
		stateDir := migrationTestStateDir(t)
		parent := registryTestDir(t)
		path := filepath.Join(parent, "project")
		require.NoError(t, os.Mkdir(path, 0o700))
		_, sourceSHA := writeMigrationV2(t, stateDir, []registryV2Entry{{ID: "one", Path: path, VCS: "git"}})
		plan, err := CreateRegistryV3MigrationPlan(RegistryV3PlanOptions{StateDir: stateDir,
			ExpectedInputSHA256: sourceSHA, PlannedAt: fixedMigrationTime()})
		require.NoError(t, err)
		require.NoError(t, os.Rename(path, path+".old"))
		require.NoError(t, os.Mkdir(path, 0o700))
		_, err = ApplyRegistryV3Migration(RegistryV3ApplyOptions{StateDir: stateDir, ApprovedPlanSHA256: plan.PlanSHA256})
		require.ErrorContains(t, err, "changed after plan approval")
	})
}

func TestRegistryV3MigrationRejectsAmbiguousJSONAndUnsafeFilesystemObjects(t *testing.T) {
	projectPath := registryTestDir(t)
	cases := map[string]string{
		"unknown root":    `{"version":"2","projects":[],"surprise":true}`,
		"duplicate root":  `{"version":"2","version":"2","projects":[]}`,
		"unknown project": `{"version":"2","projects":[{"id":"x","path":"` + projectPath + `","vcs":"git","surprise":true}]}`,
		"duplicate id":    `{"version":"2","projects":[{"id":"x","id":"y","path":"` + projectPath + `","vcs":"git"}]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			stateDir := migrationTestStateDir(t)
			writePrivateRegistryTestFile(t, filepath.Join(stateDir, "projects.json"), []byte(raw))
			_, err := CreateRegistryV3MigrationPlan(RegistryV3PlanOptions{StateDir: stateDir,
				ExpectedInputSHA256: sha256Hex([]byte(raw)), PlannedAt: fixedMigrationTime()})
			require.Error(t, err)
		})
	}

	t.Run("registry symlink", func(t *testing.T) {
		stateDir := migrationTestStateDir(t)
		target := filepath.Join(migrationTestStateDir(t), "target.json")
		raw := []byte(`{"version":"2","projects":[]}`)
		writePrivateRegistryTestFile(t, target, raw)
		requireRegistryTestSymlink(t, target, filepath.Join(stateDir, "projects.json"))
		_, err := CreateRegistryV3MigrationPlan(RegistryV3PlanOptions{StateDir: stateDir,
			ExpectedInputSHA256: sha256Hex(raw), PlannedAt: fixedMigrationTime()})
		require.Error(t, err)
	})

	t.Run("state directory alias", func(t *testing.T) {
		target := migrationTestStateDir(t)
		alias := filepath.Join(migrationTestStateDir(t), "state-alias")
		requireRegistryTestSymlink(t, target, alias)
		_, err := CreateRegistryV3MigrationPlan(RegistryV3PlanOptions{StateDir: alias,
			ExpectedInputSHA256: strings.Repeat("0", 64), PlannedAt: fixedMigrationTime()})
		require.ErrorContains(t, err, "aliases")
	})

	t.Run("registry lock symlink", func(t *testing.T) {
		stateDir := migrationTestStateDir(t)
		_, sourceSHA := writeMigrationV2(t, stateDir, []registryV2Entry{})
		target := filepath.Join(migrationTestStateDir(t), "lock-target")
		writePrivateRegistryTestFile(t, target, nil)
		requireRegistryTestSymlink(t, target, filepath.Join(stateDir, "projects.json.lock"))
		_, err := CreateRegistryV3MigrationPlan(RegistryV3PlanOptions{StateDir: stateDir,
			ExpectedInputSHA256: sourceSHA, PlannedAt: fixedMigrationTime()})
		require.ErrorContains(t, err, "unsafe registry lock")
	})
}

func TestRegistryV3MigrationRequiresExactCanonicalV2Encoding(t *testing.T) {
	stateDir := migrationTestStateDir(t)
	projectPath := registryTestDir(t)
	compact := []byte(fmt.Sprintf(`{"version":"2","projects":[{"id":"project-a","path":%q,"vcs":"git"}]}`, projectPath))
	writePrivateRegistryTestFile(t, filepath.Join(stateDir, "projects.json"), compact)

	_, err := CreateRegistryV3MigrationPlan(RegistryV3PlanOptions{
		StateDir: stateDir, ExpectedInputSHA256: sha256Hex(compact), PlannedAt: fixedMigrationTime(),
	})
	require.ErrorContains(t, err, "exact canonical persisted encoding")
}

func TestRegistryV3MigrationNoClobberEvidenceAndPlanApproval(t *testing.T) {
	stateDir := migrationTestStateDir(t)
	_, sourceSHA := writeMigrationV2(t, stateDir, []registryV2Entry{{ID: "one", Path: registryTestDir(t), VCS: "git"}})
	plan, err := CreateRegistryV3MigrationPlan(RegistryV3PlanOptions{StateDir: stateDir,
		ExpectedInputSHA256: sourceSHA, PlannedAt: fixedMigrationTime()})
	require.NoError(t, err)

	_, err = ApplyRegistryV3Migration(RegistryV3ApplyOptions{StateDir: stateDir, ApprovedPlanSHA256: strings.ToUpper(plan.PlanSHA256)})
	require.Error(t, err)
	backup := filepath.Join(stateDir, registryV2BackupFilename(sourceSHA))
	writePrivateRegistryTestFile(t, backup, []byte("attacker bytes"))
	original := mustRead(t, filepath.Join(stateDir, "projects.json"))
	_, err = ApplyRegistryV3Migration(RegistryV3ApplyOptions{StateDir: stateDir, ApprovedPlanSHA256: plan.PlanSHA256})
	require.ErrorContains(t, err, "different bytes")
	require.Equal(t, []byte("attacker bytes"), mustRead(t, backup))
	require.Equal(t, original, mustRead(t, filepath.Join(stateDir, "projects.json")))
}

func TestRegistryV3InactiveReactivationAndIdentityReplacementFailClosed(t *testing.T) {
	stateDir := migrationTestStateDir(t)
	missing := filepath.Join(registryTestDir(t), "later")
	_, sourceSHA := writeMigrationV2(t, stateDir, []registryV2Entry{{ID: "inactive", Path: missing, VCS: "none"}})
	plan, err := CreateRegistryV3MigrationPlan(RegistryV3PlanOptions{StateDir: stateDir,
		ExpectedInputSHA256: sourceSHA, PlannedAt: fixedMigrationTime()})
	require.NoError(t, err)
	_, err = ApplyRegistryV3Migration(RegistryV3ApplyOptions{StateDir: stateDir, ApprovedPlanSHA256: plan.PlanSHA256})
	require.NoError(t, err)
	registry, err := NewRegistry(filepath.Join(stateDir, "projects.json"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(missing, 0o700))
	require.NoError(t, registry.AddOrUpdate(Entry{ID: "inactive", Path: missing, VCS: "none"}))
	entry, ok := registry.Get("inactive")
	require.True(t, ok)
	require.False(t, entry.Inactive)
	require.Equal(t, uint64(2), entry.AuthorizationGeneration)
	require.True(t, registry.IsAuthorized("inactive", 2))

	// Replacing the directory at the same pathname invalidates cached
	// authority immediately, without waiting for the periodic registry reload.
	require.NoError(t, os.Rename(missing, missing+".old"))
	require.NoError(t, os.Mkdir(missing, 0o700))
	require.False(t, registry.IsAuthorized("inactive", 2))
	_, ok = registry.Get("inactive")
	require.False(t, ok)
	require.Empty(t, registry.List())
}

func TestRegistryV3PersistedIdentityAndInactiveSchemaFailClosed(t *testing.T) {
	path := registryTestDir(t)
	_, _, identity, err := resolveMigrationProjectPath(path)
	require.NoError(t, err)
	base := registryState{Version: "3", Revision: 1, Projects: []Entry{{ID: "x", Path: path, VCS: "git",
		AuthorizationGeneration: 1, FileIdentity: &identity}}}

	for name, mutate := range map[string]func(*registryState){
		"unknown identity platform": func(state *registryState) { state.Projects[0].FileIdentity.Platform = "future" },
		"mixed identity":            func(state *registryState) { state.Projects[0].FileIdentity.VolumeSerial = 12 },
		"inactive with identity":    func(state *registryState) { state.Projects[0].Inactive = true },
		"active without identity":   func(state *registryState) { state.Projects[0].FileIdentity = nil },
	} {
		t.Run(name, func(t *testing.T) {
			state := cloneState(base)
			mutate(&state)
			raw, marshalErr := json.Marshal(state)
			require.NoError(t, marshalErr)
			_, _, decodeErr := decodeRegistry(raw)
			require.Error(t, decodeErr)
		})
	}
	// marshalRegistry now normalizes nil to [] (an empty registry must stay
	// readable), so build the null-projects file directly: a persisted
	// "projects": null must still fail closed.
	nullProjects, err := json.Marshal(registryState{Version: "3", Revision: 1, Projects: nil})
	require.NoError(t, err)
	_, _, err = decodeRegistry(nullProjects)
	require.ErrorContains(t, err, "projects must be an array")
}

func TestRegistryV3AddOrUpdateRejectsGenerationOverflowWithoutMutation(t *testing.T) {
	stateDir := migrationTestStateDir(t)
	path := registryTestDir(t)
	_, inactive, identity, err := resolveMigrationProjectPath(path)
	require.NoError(t, err)
	require.False(t, inactive)
	state := registryState{Version: "3", Revision: 9, Projects: []Entry{{ID: "max-generation", Path: path,
		VCS: "none", DisplayName: "before", FileIdentity: &identity, AuthorizationGeneration: ^uint64(0)}}}
	raw, err := marshalRegistry(state)
	require.NoError(t, err)
	registryPath := filepath.Join(stateDir, "projects.json")
	writePrivateRegistryTestFile(t, registryPath, raw)
	registry, err := NewRegistry(registryPath)
	require.NoError(t, err)

	err = registry.AddOrUpdate(Entry{ID: "max-generation", Path: path, VCS: "none", Ephemeral: true, DisplayName: "after"})
	require.ErrorContains(t, err, "generation exhausted")
	require.Equal(t, raw, mustRead(t, registryPath))
}

func TestRegistryV3InactivePathStillBlocksDuplicateRegistration(t *testing.T) {
	stateDir := migrationTestStateDir(t)
	path := filepath.Join(registryTestDir(t), "missing")
	registry, err := NewRegistry(filepath.Join(stateDir, "projects.json"))
	require.NoError(t, err)
	require.NoError(t, registry.Add(Entry{ID: "inactive", Path: path, VCS: "none", Inactive: true}))
	require.NoError(t, os.Mkdir(path, 0o700))
	err = registry.Add(Entry{ID: "different", Path: path, VCS: "none"})
	require.ErrorContains(t, err, "path already registered")
	require.False(t, registry.IsAuthorized("inactive", 1))
	require.Empty(t, registry.List())
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

func TestStrictJSONRejectsInvalidUTF8AndTrailingValues(t *testing.T) {
	var value map[string]any
	require.Error(t, decodeStrictJSON([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}, &value))
	require.Error(t, decodeStrictJSON([]byte(`{} {}`), &value))
	require.NoError(t, decodeStrictJSON(bytes.TrimSpace([]byte("  {}\n")), &value))
}
