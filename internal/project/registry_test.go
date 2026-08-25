package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/stretchr/testify/require"
)

func registryTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	physical, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return physical
}

func registryTestChild(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	require.NoError(t, os.MkdirAll(path, 0o700))
	physical, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return physical
}

func writePrivateRegistryTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	require.NoError(t, atomicfile.WriteFile(path, data, 0o600))
}

func TestRegistry_ConcurrentWritersDoNotLoseUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	const writers = 32
	projectPaths := make([]string, writers)
	for i := range projectPaths {
		projectPaths[i] = registryTestDir(t)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := NewRegistry(path)
			if err == nil {
				err = r.Add(Entry{ID: fmt.Sprintf("project-%02d", i), Path: projectPaths[i], VCS: "git"})
			}
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	r, err := NewRegistry(path)
	require.NoError(t, err)
	require.Len(t, r.List(), writers)
	require.Equal(t, uint64(writers), r.Revision())
}

func TestRegistry_RemovePersistsRevocationTombstone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	r, err := NewRegistry(path)
	require.NoError(t, err)
	require.NoError(t, r.Add(Entry{ID: "project-a", Path: registryTestDir(t), VCS: "git"}))
	require.NoError(t, r.Remove("project-a"))
	require.False(t, r.IsAuthorized("project-a", 1))
	ts := r.Tombstones()
	require.Len(t, ts, 1)
	require.Equal(t, uint64(2), ts[0].AuthorizationGeneration)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	require.Equal(t, "3", raw["version"])
}

func TestRegistry_MissingFile_OK(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(filepath.Join(dir, "projects.json"))
	require.NoError(t, err)
	require.Empty(t, r.List(), "fresh registry is empty")
}

func TestRegistry_MissingParent_OK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "nested", "projects.json")
	r, err := NewRegistry(path)
	require.NoError(t, err)
	require.Empty(t, r.List(), "fresh registry is empty")

	info, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	require.True(t, info.IsDir())
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "constructing a registry must not create registry content")
}

func TestRegistry_Add_Get_Remove_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.json")
	r, err := NewRegistry(path)
	require.NoError(t, err)

	e := Entry{
		ID:          "github.com/example-user/sample-project",
		Path:        registryTestDir(t),
		VCS:         "git",
		DisplayName: "Sample Repository",
	}
	require.NoError(t, r.Add(e))

	got, ok := r.Get(e.ID)
	require.True(t, ok)
	require.Equal(t, uint64(1), got.AuthorizationGeneration)
	require.NotNil(t, got.FileIdentity)
	e = got

	// Persisted to disk?
	_, statErr := os.Stat(path)
	require.NoError(t, statErr)

	// Re-open and verify reload.
	r2, err := NewRegistry(path)
	require.NoError(t, err)
	got2, ok := r2.Get(e.ID)
	require.True(t, ok)
	require.Equal(t, e, got2)

	require.NoError(t, r.Remove(e.ID))
	_, ok = r.Get(e.ID)
	require.False(t, ok)
}

func TestRegistry_RefreshAgents_UnionsAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.json")
	r, err := NewRegistry(path)
	require.NoError(t, err)

	e := Entry{
		ID:     "github.com/example-user/sample-project",
		Path:   registryTestDir(t),
		VCS:    "git",
		Scope:  "local",
		Agents: []string{"codex"},
	}
	require.NoError(t, r.Add(e))

	// Union "claude-code" into the already-registered folder's agents set.
	require.NoError(t, r.RefreshAgents(e.Path, []string{"claude-code"}))

	// Re-open from disk and assert the union is sorted + deduped + persisted.
	r2, err := NewRegistry(path)
	require.NoError(t, err)
	got, ok := r2.Get(e.ID)
	require.True(t, ok)
	require.Equal(t, []string{"claude-code", "codex"}, got.Agents)
	require.Equal(t, uint64(2), got.AuthorizationGeneration, "fan-out authorization changes must revoke older work")

	// Re-running with an already-present agent is a no-op (no duplicates).
	revision := r.Revision()
	require.NoError(t, r.RefreshAgents(e.Path, []string{"codex", "claude-code"}))
	require.Equal(t, revision, r.Revision())
	r3, err := NewRegistry(path)
	require.NoError(t, err)
	got3, ok := r3.Get(e.ID)
	require.True(t, ok)
	require.Equal(t, []string{"claude-code", "codex"}, got3.Agents)
	require.Equal(t, uint64(2), got3.AuthorizationGeneration)
}

func TestRegistry_RefreshAgents_UnregisteredPathIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.json")
	r, err := NewRegistry(path)
	require.NoError(t, err)

	// RefreshAgents must NEVER register a new folder — it only refreshes the
	// agents set of folders ALREADY registered (approval-gate invariant).
	require.NoError(t, r.RefreshAgents("/not/registered", []string{"codex"}))
	require.Empty(t, r.List(), "RefreshAgents must not create an entry for an unregistered path")

	// And it must not have written a phantom entry to disk either.
	r2, err := NewRegistry(path)
	require.NoError(t, err)
	require.Empty(t, r2.List())
}

func TestRegistry_RefreshAgentsRejectsUnsafeNameWithoutMutation(t *testing.T) {
	registryPath := filepath.Join(registryTestDir(t), "projects.json")
	projectPath := registryTestDir(t)
	r, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, r.Add(Entry{ID: "project-a", Path: projectPath, VCS: "none", Agents: []string{"codex"}}))
	before := mustRead(t, registryPath)
	err = r.RefreshAgents(projectPath, []string{"bad\nagent"})
	require.ErrorContains(t, err, "invalid agent")
	require.Equal(t, before, mustRead(t, registryPath))
	require.True(t, r.IsAuthorized("project-a", 1))
}

func TestRegistry_Add_DuplicateIDErrors(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(filepath.Join(dir, "projects.json"))
	require.NoError(t, err)

	e := Entry{ID: "x/y", Path: registryTestDir(t), VCS: "git"}
	require.NoError(t, r.Add(e))
	err = r.Add(e)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already registered")
}

func TestRegistry_Update_OnlyExisting(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(filepath.Join(dir, "projects.json"))
	require.NoError(t, err)

	err = r.Update(Entry{ID: "missing", Path: registryTestDir(t)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not registered")

	e := Entry{ID: "x/y", Path: registryTestDir(t), VCS: "git"}
	require.NoError(t, r.Add(e))

	e.DisplayName = "X over Y"
	require.NoError(t, r.Update(e))

	got, _ := r.Get(e.ID)
	require.Equal(t, "X over Y", got.DisplayName)
}

func TestRegistry_Remove_NonexistentNoOp(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(filepath.Join(dir, "projects.json"))
	require.NoError(t, err)
	require.NoError(t, r.Remove("missing"), "removing nonexistent ID is a no-op success")
}

func TestRegistry_FindByPath(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(filepath.Join(dir, "projects.json"))
	require.NoError(t, err)

	a := registryTestDir(t)
	b := registryTestDir(t)
	require.NoError(t, r.Add(Entry{ID: "x", Path: a}))
	require.NoError(t, r.Add(Entry{ID: "y", Path: b}))

	got, ok := r.FindByPath(b)
	require.True(t, ok)
	require.Equal(t, "y", got.ID)

	_, ok = r.FindByPath("/nope")
	require.False(t, ok)
}

func TestRegistry_ListIsSorted(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(filepath.Join(dir, "projects.json"))
	require.NoError(t, err)

	for _, id := range []string{"z", "a", "m", "b"} {
		require.NoError(t, r.Add(Entry{ID: id, Path: registryTestDir(t)}))
	}
	list := r.List()
	require.Equal(t, []string{"a", "b", "m", "z"},
		[]string{list[0].ID, list[1].ID, list[2].ID, list[3].ID})
}

func TestRegistry_ScopeAgents_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "projects.json")
	r, err := NewRegistry(p)
	require.NoError(t, err)

	require.NoError(t, r.AddOrUpdate(Entry{
		ID: "local:abc:home", Path: registryTestDir(t), VCS: "none",
		Scope: "local", Agents: []string{"codex", "claude-code"},
	}))

	r2, err := NewRegistry(p)
	require.NoError(t, err)
	got, ok := r2.Get("local:abc:home")
	require.True(t, ok)
	require.Equal(t, "local", got.Scope)
	require.Equal(t, []string{"claude-code", "codex"}, got.Agents) // sorted on store
}

func TestRegistry_RejectsLegacyVersionOutsideExplicitMigration(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "projects.json")
	v1 := fmt.Sprintf(`{"version":"1","projects":[{"id":"local:x:repo","path":%q,"vcs":"git","displayName":"repo"}]}`, registryTestDir(t))
	writePrivateRegistryTestFile(t, p, []byte(v1))

	_, err := NewRegistry(p)
	require.ErrorContains(t, err, "explicit project migrate-v3 ceremony")
}

func TestRegistry_ReloadRejectsForgedAuthorityTransitionAndPausesConsumers(t *testing.T) {
	dir := registryTestDir(t)
	registryPath := filepath.Join(dir, "projects.json")
	first := registryTestDir(t)
	second := registryTestDir(t)
	r, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, r.Add(Entry{ID: "project-a", Path: first, VCS: "git"}))

	raw, err := os.ReadFile(registryPath)
	require.NoError(t, err)
	var forged registryState
	require.NoError(t, decodeStrictJSON(raw, &forged))
	_, inactive, identity, err := resolveMigrationProjectPath(second)
	require.NoError(t, err)
	require.False(t, inactive)
	forged.Revision++
	forged.Projects[0].Path = second
	forged.Projects[0].FileIdentity = &identity
	// Deliberately reuse generation one for different authority.
	forgedRaw, err := marshalRegistry(forged)
	require.NoError(t, err)
	writePrivateRegistryTestFile(t, registryPath, forgedRaw)

	err = r.Reload()
	require.ErrorContains(t, err, "authorization changed without a generation advance")
	require.False(t, r.Available())
	require.Empty(t, r.List(), "authorization-sensitive enumeration must pause")
	require.False(t, r.IsAuthorized("project-a", 1))
	_, ok := r.Get("project-a")
	require.False(t, ok)
	diagnostics := r.ListAll()
	require.Len(t, diagnostics, 1)
	require.Equal(t, first, diagnostics[0].Path, "last good state remains diagnostic only")
}

func TestRegistry_ReloadIsolatesUnavailableProjectIdentity(t *testing.T) {
	registryPath := filepath.Join(registryTestDir(t), "projects.json")
	healthyPath := registryTestDir(t)
	transientPath := registryTestDir(t)
	r, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, r.Add(Entry{ID: "healthy", Path: healthyPath, VCS: "none"}))
	require.NoError(t, r.Add(Entry{ID: "transient", Path: transientPath, VCS: "none"}))

	// Simulate an unmounted volume. The unavailable entry must fail closed,
	// while the rest of the authenticated registry remains usable.
	unmountedPath := transientPath + ".unmounted"
	require.NoError(t, os.Rename(transientPath, unmountedPath))
	require.NoError(t, r.Reload())
	require.True(t, r.Available())
	require.True(t, r.IsAuthorized("healthy", 1))
	require.False(t, r.IsAuthorized("transient", 1))
	projects := r.List()
	require.Len(t, projects, 1)
	require.Equal(t, "healthy", projects[0].ID)

	// Restoring the same physical directory restores its existing authority.
	require.NoError(t, os.Rename(unmountedPath, transientPath))
	require.True(t, r.IsAuthorized("transient", 1))
	require.Len(t, r.List(), 2)

	// A different directory at the same pathname is not authorized, but it
	// also must not pause healthy projects.
	require.NoError(t, os.Rename(transientPath, unmountedPath))
	require.NoError(t, os.Mkdir(transientPath, 0o700))
	require.NoError(t, r.Reload())
	require.True(t, r.Available())
	require.True(t, r.IsAuthorized("healthy", 1))
	require.False(t, r.IsAuthorized("transient", 1))
	require.Len(t, r.List(), 1)
}

func TestRegistryTransitionRequiresExactGenerationForAgentAuthorization(t *testing.T) {
	path := registryTestDir(t)
	_, inactive, identity, err := resolveMigrationProjectPath(path)
	require.NoError(t, err)
	require.False(t, inactive)
	previous := registryState{Version: "3", Revision: 7, Projects: []Entry{{ID: "project-a", Path: path, VCS: "git",
		Scope: "local", Agents: []string{"codex"}, FileIdentity: &identity, AuthorizationGeneration: 1}}}

	for name, generation := range map[string]uint64{"reused": 1, "jumped": 3} {
		t.Run(name, func(t *testing.T) {
			next := cloneState(previous)
			next.Revision++
			next.Projects[0].Agents = []string{"claude-code", "codex"}
			next.Projects[0].AuthorizationGeneration = generation
			require.Error(t, validateRegistryTransition(previous, next))
		})
	}
	next := cloneState(previous)
	next.Revision++
	next.Projects[0].Agents = []string{"claude-code", "codex"}
	next.Projects[0].AuthorizationGeneration = 2
	require.NoError(t, validateRegistryTransition(previous, next))

	displayOnly := cloneState(previous)
	displayOnly.Revision++
	displayOnly.Projects[0].DisplayName = "new display"
	require.NoError(t, validateRegistryTransition(previous, displayOnly))
}

func TestRegistry_ReloadRejectsTombstoneRemovalAndResurrection(t *testing.T) {
	for _, testCase := range []string{"remove tombstone", "resurrect ID"} {
		t.Run(testCase, func(t *testing.T) {
			registryPath := filepath.Join(registryTestDir(t), "projects.json")
			projectPath := registryTestDir(t)
			r, err := NewRegistry(registryPath)
			require.NoError(t, err)
			require.NoError(t, r.Add(Entry{ID: "revoked", Path: projectPath, VCS: "none"}))
			require.NoError(t, r.Remove("revoked"))

			var forged registryState
			require.NoError(t, decodeStrictJSON(mustRead(t, registryPath), &forged))
			forged.Revision++
			forged.Tombstones = nil
			if testCase == "resurrect ID" {
				_, inactive, identity, identityErr := resolveMigrationProjectPath(projectPath)
				require.NoError(t, identityErr)
				require.False(t, inactive)
				forged.Projects = []Entry{{ID: "revoked", Path: projectPath, VCS: "none", FileIdentity: &identity, AuthorizationGeneration: 1}}
			}
			forgedRaw, err := marshalRegistry(forged)
			require.NoError(t, err)
			writePrivateRegistryTestFile(t, registryPath, forgedRaw)

			err = r.Reload()
			require.Error(t, err)
			require.False(t, r.Available())
			require.Empty(t, r.List())
		})
	}
}

func TestRegistry_UpdateScansAllEntriesForPathCollision(t *testing.T) {
	registryPath := filepath.Join(registryTestDir(t), "projects.json")
	first := registryTestDir(t)
	second := registryTestDir(t)
	r, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, r.Add(Entry{ID: "a-first", Path: first, VCS: "none"}))
	require.NoError(t, r.Add(Entry{ID: "z-second", Path: second, VCS: "none"}))

	entry, ok := r.Get("a-first")
	require.True(t, ok)
	entry.Path = second
	err = r.Update(entry)
	require.ErrorContains(t, err, "path already registered")
	stillFirst, ok := r.Get("a-first")
	require.True(t, ok)
	require.Equal(t, first, stillFirst.Path)
}

// TestRegistry_AcceptsExternalCompositeAddThenUpdate replays the 2026-07-30
// incident shape: while one process (the daemon) holds an older observation,
// another process legitimately adds a project and then updates it, so the
// composite observation is a NEW entry at generation two. Both the Reload path
// and the pre-mutation authentication in mutate must accept it; the strict
// per-step validator used to reject it and wedge authorization until restart.
func TestRegistry_AcceptsExternalCompositeAddThenUpdate(t *testing.T) {
	registryPath := filepath.Join(registryTestDir(t), "projects.json")
	seedPath := registryTestDir(t)
	firstClone := registryTestDir(t)
	secondClone := registryTestDir(t)
	freshPath := registryTestDir(t)

	reloadObserver, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, reloadObserver.Add(Entry{ID: "seed", Path: seedPath, VCS: "none"}))
	mutateObserver, err := NewRegistry(registryPath)
	require.NoError(t, err)

	// External writer: add then deliberately re-point (link semantics).
	writer, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, writer.Add(Entry{ID: "github.com/example/dupes", Path: firstClone, VCS: "git"}))
	moved, ok := writer.Get("github.com/example/dupes")
	require.True(t, ok)
	moved.Path = secondClone
	require.NoError(t, writer.Update(moved))

	// Reload path: the composite transition (new entry at generation 2) must
	// be accepted and authorization must stay available.
	require.NoError(t, reloadObserver.Reload())
	require.True(t, reloadObserver.Available())
	got, ok := reloadObserver.Get("github.com/example/dupes")
	require.True(t, ok)
	require.Equal(t, secondClone, got.Path)
	require.Equal(t, uint64(2), got.AuthorizationGeneration)

	// Mutation path: a stale observer mutating on top of the composite state
	// must authenticate it pre-mutation and succeed, not markUnavailable.
	require.NoError(t, mutateObserver.Add(Entry{ID: "fresh", Path: freshPath, VCS: "none"}))
	require.True(t, mutateObserver.Available())
	require.Len(t, mutateObserver.List(), 3)
}

// TestRegistry_AcceptsExternalAddThenRemove: an unobserved add+remove leaves a
// generation-2 tombstone for an ID the observer's baseline never contained.
// Composite-legitimate; must not pause authorization.
func TestRegistry_AcceptsExternalAddThenRemove(t *testing.T) {
	registryPath := filepath.Join(registryTestDir(t), "projects.json")
	seedPath := registryTestDir(t)
	transientPath := registryTestDir(t)

	observer, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, observer.Add(Entry{ID: "seed", Path: seedPath, VCS: "none"}))

	writer, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, writer.Add(Entry{ID: "transient", Path: transientPath, VCS: "none"}))
	require.NoError(t, writer.Remove("transient"))

	require.NoError(t, observer.Reload())
	require.True(t, observer.Available())
	require.False(t, observer.IsAuthorized("transient", 1))
	tombstones := observer.Tombstones()
	require.Len(t, tombstones, 1)
	require.Equal(t, "transient", tombstones[0].ID)
	require.Equal(t, uint64(2), tombstones[0].AuthorizationGeneration)
}

// TestValidateExternalRegistryTransition_InvariantMatrix pins down exactly
// which shapes the composite validator accepts (reachable by a legitimate
// sequence of serialized mutations) and which it still rejects (violating an
// invariant that survives composition).
func TestValidateExternalRegistryTransition_InvariantMatrix(t *testing.T) {
	removedAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	base := func() registryState {
		return registryState{Version: "3", Revision: 7,
			Projects:   []Entry{{ID: "project-a", Path: "/p/a", VCS: "git", AuthorizationGeneration: 2}},
			Tombstones: []Tombstone{{ID: "gone", AuthorizationGeneration: 3, RemovedAt: removedAt}}}
	}

	type testCase struct {
		name   string
		accept bool
		shape  func(next *registryState)
	}
	cases := []testCase{
		{"new entry at generation five (add plus updates)", true, func(next *registryState) {
			next.Projects = append(next.Projects, Entry{ID: "project-b", Path: "/p/b", VCS: "git", AuthorizationGeneration: 5})
		}},
		{"revision advance back to equal state (display round trip)", true, func(next *registryState) {
			next.Revision = 9
		}},
		{"generation jump on authorization change (two updates)", true, func(next *registryState) {
			next.Projects[0].Agents = []string{"codex"}
			next.Projects[0].AuthorizationGeneration = 4
		}},
		{"removal with strictly newer tombstone", true, func(next *registryState) {
			next.Projects = nil
			next.Tombstones = append(next.Tombstones, Tombstone{ID: "project-a", AuthorizationGeneration: 3, RemovedAt: removedAt})
		}},
		{"tombstone for an ID the baseline never saw (add plus remove)", true, func(next *registryState) {
			next.Tombstones = append(next.Tombstones, Tombstone{ID: "phantom", AuthorizationGeneration: 2, RemovedAt: removedAt})
		}},
		{"display-name change without generation advance (external rename)", true, func(next *registryState) {
			next.Projects[0].DisplayName = "renamed"
		}},
		{"unchanged state at unchanged revision", true, func(next *registryState) {
			next.Revision = 7
		}},
		{"revision rollback", false, func(next *registryState) {
			next.Revision = 6
		}},
		{"state change without revision advance", false, func(next *registryState) {
			next.Revision = 7
			next.Projects[0].DisplayName = "forged"
		}},
		{"generation rollback", false, func(next *registryState) {
			next.Projects[0].AuthorizationGeneration = 1
		}},
		{"generation reuse for changed authorization", false, func(next *registryState) {
			next.Projects[0].Agents = []string{"codex"}
		}},
		{"tombstone disappeared", false, func(next *registryState) {
			next.Tombstones = nil
		}},
		{"tombstone generation rollback", false, func(next *registryState) {
			next.Tombstones[0].AuthorizationGeneration = 2
		}},
		{"tombstone mutated without generation advance", false, func(next *registryState) {
			next.Tombstones[0].RemovedAt = removedAt.Add(time.Hour)
		}},
		{"tombstoned ID resurrected", false, func(next *registryState) {
			next.Tombstones = nil
			next.Projects = append(next.Projects, Entry{ID: "gone", Path: "/p/gone", VCS: "none", AuthorizationGeneration: 1})
		}},
		{"removal without tombstone", false, func(next *registryState) {
			next.Projects = nil
		}},
		{"removal with stale-generation tombstone", false, func(next *registryState) {
			next.Projects = nil
			next.Tombstones = append(next.Tombstones, Tombstone{ID: "project-a", AuthorizationGeneration: 2, RemovedAt: removedAt})
		}},
		{"surviving entry jumped to the exhausted generation", false, func(next *registryState) {
			next.Projects[0].AuthorizationGeneration = ^uint64(0)
		}},
		{"new entry at the exhausted generation", false, func(next *registryState) {
			next.Projects = append(next.Projects, Entry{ID: "project-b", Path: "/p/b", VCS: "git", AuthorizationGeneration: ^uint64(0)})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previous := base()
			next := base()
			next.Revision++
			tc.shape(&next)
			err := validateExternalRegistryTransition(previous, next)
			if tc.accept {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// TestRegistry_AddOrUpdateRefusesDisplacingLiveClone: two clones of one repo
// share one canonical ID; registering the second while the first still exists
// exactly as recorded must refuse instead of silently de-registering it.
func TestRegistry_AddOrUpdateRefusesDisplacingLiveClone(t *testing.T) {
	registryPath := filepath.Join(registryTestDir(t), "projects.json")
	firstClone := registryTestDir(t)
	secondClone := registryTestDir(t)
	r, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, r.Add(Entry{ID: "github.com/example/dupes", Path: firstClone, VCS: "git"}))

	err = r.AddOrUpdate(Entry{ID: "github.com/example/dupes", Path: secondClone, VCS: "git"})
	require.ErrorContains(t, err, "already registered at")
	require.ErrorContains(t, err, firstClone)

	got, ok := r.Get("github.com/example/dupes")
	require.True(t, ok)
	require.Equal(t, firstClone, got.Path, "the live clone keeps its registration")
	require.Equal(t, uint64(1), got.AuthorizationGeneration, "a refused displacement must not burn a generation")
}

// TestRegistry_AddOrUpdateRepointsMovedClone: when the recorded location is no
// longer live (the repo moved), AddOrUpdate remains the repair path and
// re-points the entry with a generation advance.
func TestRegistry_AddOrUpdateRepointsMovedClone(t *testing.T) {
	registryPath := filepath.Join(registryTestDir(t), "projects.json")
	parent := registryTestDir(t)
	original := registryTestChild(t, parent, "clone")
	movedTo := filepath.Join(parent, "clone-moved")
	r, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, r.Add(Entry{ID: "github.com/example/moved", Path: original, VCS: "git"}))

	require.NoError(t, os.Rename(original, movedTo))
	require.NoError(t, r.AddOrUpdate(Entry{ID: "github.com/example/moved", Path: movedTo, VCS: "git"}))

	got, ok := r.Get("github.com/example/moved")
	require.True(t, ok)
	require.Equal(t, movedTo, got.Path)
	require.Equal(t, uint64(2), got.AuthorizationGeneration, "re-pointing is an authorization change")
}

// TestRegistry_RemoveLastProjectDoesNotWedgeObserversOrWriters pins the
// nil-vs-empty Projects normalization: an empty registry persists as
// "projects": [] but is held in memory as a nil slice (cloneState), and
// without decodeRegistry's normalization the equal-revision DeepEqual check
// reports a phantom state change after removing the LAST project — wedging
// every observer's Reload AND every writer's pre-mutation check forever,
// with no mutation able to advance the revision to heal it.
func TestRegistry_RemoveLastProjectDoesNotWedgeObserversOrWriters(t *testing.T) {
	registryPath := filepath.Join(registryTestDir(t), "projects.json")
	projectPath := registryTestDir(t)
	freshPath := registryTestDir(t)

	r, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, r.Add(Entry{ID: "only", Path: projectPath, VCS: "none"}))
	observer, err := NewRegistry(registryPath)
	require.NoError(t, err)

	require.NoError(t, r.Remove("only"))

	// The remover's own next poll observes its own write at equal revision.
	require.NoError(t, r.Reload())
	require.True(t, r.Available())
	// A second observer authenticates the composite remove.
	require.NoError(t, observer.Reload())
	require.True(t, observer.Available())
	// A fresh writer's pre-mutation check passes at equal revision, so the
	// registry remains mutable (revision can advance; self-heal reachable).
	writer, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, writer.Add(Entry{ID: "next", Path: freshPath, VCS: "none"}))
	require.NoError(t, r.Reload())
	require.True(t, r.Available())
	require.True(t, r.IsAuthorized("next", 1))
}

// TestRegistry_MutatePreCheckRejectsForgedExternalState pins the fail-closed
// pre-mutation authentication inside mutate: a stale-baseline process asked
// to mutate on top of a forged file (generation reused for different
// authority) must refuse, mark authorization unavailable, and leave the file
// untouched — never launder the forgery by persisting its own write on top.
func TestRegistry_MutatePreCheckRejectsForgedExternalState(t *testing.T) {
	registryPath := filepath.Join(registryTestDir(t), "projects.json")
	first := registryTestDir(t)
	second := registryTestDir(t)
	freshPath := registryTestDir(t)
	r, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, r.Add(Entry{ID: "project-a", Path: first, VCS: "git"}))

	var forged registryState
	require.NoError(t, decodeStrictJSON(mustRead(t, registryPath), &forged))
	_, inactive, identity, err := resolveMigrationProjectPath(second)
	require.NoError(t, err)
	require.False(t, inactive)
	forged.Revision++
	forged.Projects[0].Path = second
	forged.Projects[0].FileIdentity = &identity
	// Deliberately reuse generation one for different authority.
	forgedRaw, err := marshalRegistry(forged)
	require.NoError(t, err)
	writePrivateRegistryTestFile(t, registryPath, forgedRaw)

	err = r.Add(Entry{ID: "fresh", Path: freshPath, VCS: "none"})
	require.ErrorContains(t, err, "reject unauthenticated registry transition before mutation")
	require.False(t, r.Available())
	require.Equal(t, forgedRaw, mustRead(t, registryPath), "a refused mutation must not launder the forged file")
}

// TestRegistry_AvailabilitySelfHealsAfterRejectedReload pins the spec-claimed
// invariant in BOTH directions: rejection pauses authorization (fail-closed,
// non-latching) and a LATER state that authenticates against the retained
// baseline restores availability without a restart. Guards against two
// regressions: a sticky latch (wedged-until-restart, the incident symptom)
// and resetting initialized on rejection (which would accept the forgery on
// the very next reload under first-load semantics).
func TestRegistry_AvailabilitySelfHealsAfterRejectedReload(t *testing.T) {
	registryPath := filepath.Join(registryTestDir(t), "projects.json")
	first := registryTestDir(t)
	second := registryTestDir(t)
	r, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, r.Add(Entry{ID: "project-a", Path: first, VCS: "git"}))

	// Forge a re-point that reuses generation one for different authority.
	var forged registryState
	require.NoError(t, decodeStrictJSON(mustRead(t, registryPath), &forged))
	_, inactive, identity, err := resolveMigrationProjectPath(second)
	require.NoError(t, err)
	require.False(t, inactive)
	forged.Revision++
	forged.Projects[0].Path = second
	forged.Projects[0].FileIdentity = &identity
	forgedRaw, err := marshalRegistry(forged)
	require.NoError(t, err)
	writePrivateRegistryTestFile(t, registryPath, forgedRaw)

	require.Error(t, r.Reload())
	require.False(t, r.Available())
	// Rejection must not latch: the forgery is still rejected, not accepted,
	// on a subsequent poll (initialized must survive the rejection).
	require.Error(t, r.Reload())
	require.False(t, r.Available())

	// An external write that composes into a valid transition from the
	// retained baseline (generation advanced with the authorization change)
	// heals availability. A fresh process accepts the on-disk state under
	// first-load semantics and updates on top of it, exactly like the
	// post-incident CLI repair.
	repairer, err := NewRegistry(registryPath)
	require.NoError(t, err)
	repaired, ok := repairer.Get("project-a")
	require.True(t, ok)
	repaired.Agents = []string{"codex"}
	require.NoError(t, repairer.Update(repaired))

	require.NoError(t, r.Reload())
	require.True(t, r.Available())
	got, ok := r.Get("project-a")
	require.True(t, ok)
	require.Equal(t, uint64(2), got.AuthorizationGeneration)
	require.Equal(t, second, got.Path)
}

func TestRegistry_MutationFailureMarksAuthorizationUnavailable(t *testing.T) {
	registryPath := filepath.Join(registryTestDir(t), "projects.json")
	projectPath := registryTestDir(t)
	r, err := NewRegistry(registryPath)
	require.NoError(t, err)
	require.NoError(t, r.Add(Entry{ID: "project-a", Path: projectPath, VCS: "none"}))
	writePrivateRegistryTestFile(t, registryPath, []byte(`{"version":"3"}`))

	err = r.RefreshAgents(projectPath, []string{"codex"})
	require.Error(t, err)
	require.False(t, r.Available())
	require.Empty(t, r.List())
	require.False(t, r.IsAuthorized("project-a", 1))
}
