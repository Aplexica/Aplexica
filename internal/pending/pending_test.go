package pending

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/projectdiscovery"
	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) *acf.Store {
	s := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	return s
}

func writeArtifact(t *testing.T, s *acf.Store, kind acf.Kind, scope acf.Scope, projectID, sourcePath string) {
	t.Helper()
	id := acf.NewID()
	a := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             kind,
		Scope:            scope,
		Name:             filepath.Base(sourcePath),
		SourcePath:       sourcePath,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
	if scope == acf.ScopeProject && projectID != "" {
		a.Project = &project.ProjectInfo{ID: projectID, Path: filepath.Dir(sourcePath), VCS: "git"}
	}
	require.NoError(t, s.WriteArtifact(a))
}

func TestList_EmptyStore(t *testing.T) {
	s := newStore(t)
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)
	got, err := List(s, reg)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestList_GlobalScopeArtifactsAreNotPending(t *testing.T) {
	s := newStore(t)
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)

	writeArtifact(t, s, acf.KindMemory, acf.ScopeGlobal, "", "/home/user/.claude/CLAUDE.md")
	got, err := List(s, reg)
	require.NoError(t, err)
	require.Empty(t, got, "global-scope artifacts MUST NOT appear as pending projects")
}

func TestList_ProjectInRegistryIsNotPending(t *testing.T) {
	s := newStore(t)
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)
	known := t.TempDir()
	require.NoError(t, reg.Add(project.Entry{ID: "github.com/known/repo", Path: known, VCS: "git"}))

	writeArtifact(t, s, acf.KindMemory, acf.ScopeProject, "github.com/known/repo", filepath.Join(known, "CLAUDE.md"))
	got, err := List(s, reg)
	require.NoError(t, err)
	require.Empty(t, got, "artifacts whose project IS in the registry are NOT pending")
}

func TestList_UnknownProjectAppearsAsPending(t *testing.T) {
	s := newStore(t)
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)

	// FromSlash so the expected SamplePath (filepath.Dir of the source)
	// carries the native separator on Windows too.
	src := filepath.FromSlash("/anywhere/CLAUDE.md")
	writeArtifact(t, s, acf.KindMemory, acf.ScopeProject, "github.com/unknown/repo", src)
	got, err := List(s, reg)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "github.com/unknown/repo", got[0].ID)
	require.Equal(t, 1, got[0].ArtifactCount)
	require.Equal(t, filepath.Dir(src), got[0].SamplePath)
}

func TestList_UsesArtifactProjectPathAsFolder(t *testing.T) {
	s := newStore(t)
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)

	projectRoot := filepath.Join(t.TempDir(), "repo")
	sourcePath := filepath.Join(projectRoot, "nested", "AGENTS.md")
	artifact := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       acf.NewID(),
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeProject,
		Name:             "AGENTS.md",
		SourcePath:       sourcePath,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
		Project: &project.ProjectInfo{
			ID:   "local:abc123:repo",
			Path: projectRoot,
			VCS:  "git",
		},
	}
	require.NoError(t, s.WriteArtifact(artifact))

	got, err := List(s, reg)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, projectRoot, got[0].SamplePath, "artifact-backed pending rows should expose an approvable folder, not a native file")
}

func TestList_PathlessLocalIDSuppressedByRegisteredProjectSlug(t *testing.T) {
	s := newStore(t)
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)

	projectRoot := filepath.Join(t.TempDir(), "Aplexica")
	require.NoError(t, os.MkdirAll(projectRoot, 0o755))
	require.NoError(t, reg.Add(project.Entry{
		ID:          "github.com/aplexica/aplexica",
		Path:        projectRoot,
		VCS:         "git",
		DisplayName: "Aplexica",
	}))

	require.NoError(t, s.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       acf.NewID(),
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeProject,
		Name:             "CLAUDE.md",
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
		Project:          &project.ProjectInfo{ID: "local:3a63e9:aplexica", VCS: "none"},
	}))

	got, err := List(s, reg)
	require.NoError(t, err)
	require.Empty(t, got, "pathless local:* artifacts whose slug uniquely matches a registered project should not stay pending")
}

func TestList_AmbiguousPathlessLocalIDRemainsPending(t *testing.T) {
	s := newStore(t)
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)

	left := filepath.Join(t.TempDir(), "Aplexica")
	right := filepath.Join(t.TempDir(), "Aplexica")
	require.NoError(t, os.MkdirAll(left, 0o755))
	require.NoError(t, os.MkdirAll(right, 0o755))
	require.NoError(t, reg.Add(project.Entry{ID: "github.com/example/left", Path: left, VCS: "git"}))
	require.NoError(t, reg.Add(project.Entry{ID: "github.com/example/right", Path: right, VCS: "git"}))

	require.NoError(t, s.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       acf.NewID(),
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeProject,
		Name:             "CLAUDE.md",
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
		Project:          &project.ProjectInfo{ID: "local:3a63e9:aplexica", VCS: "none"},
	}))

	got, err := List(s, reg)
	require.NoError(t, err)
	require.Len(t, got, 1, "same-name registered projects are ambiguous; keep the pathless artifact pending")
	require.Equal(t, "local:3a63e9:aplexica", got[0].ID)
}

func TestList_MultipleArtifactsAggregateByProject(t *testing.T) {
	s := newStore(t)
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)

	writeArtifact(t, s, acf.KindMemory, acf.ScopeProject, "github.com/a/x", "/p/a/CLAUDE.md")
	writeArtifact(t, s, acf.KindSkill, acf.ScopeProject, "github.com/a/x", "/p/a/.claude/skills/x.md")
	writeArtifact(t, s, acf.KindMemory, acf.ScopeProject, "github.com/b/y", "/p/b/CLAUDE.md")

	got, err := List(s, reg)
	require.NoError(t, err)
	require.Len(t, got, 2)

	// Sorted by ID — github.com/a/x first, github.com/b/y second.
	require.Equal(t, "github.com/a/x", got[0].ID)
	require.Equal(t, 2, got[0].ArtifactCount, "memory + skill for project a")
	require.Equal(t, "github.com/b/y", got[1].ID)
	require.Equal(t, 1, got[1].ArtifactCount)
}

func TestList_NilProjectField_SkippedCleanly(t *testing.T) {
	s := newStore(t)
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)

	// ScopeProject artifact with NIL Project (simulates pre-v0.56.0
	// store entry written before InferProject was wired).
	id := acf.NewID()
	require.NoError(t, s.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeProject,
		Name:             "CLAUDE.md",
		SourcePath:       "/legacy/CLAUDE.md",
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
		// Project: nil intentionally
	}))
	got, err := List(s, reg)
	require.NoError(t, err)
	require.Empty(t, got, "nil Project on ScopeProject artifact must be skipped, not crash")
}

func TestCount(t *testing.T) {
	s := newStore(t)
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)
	writeArtifact(t, s, acf.KindMemory, acf.ScopeProject, "x/y", "/p/CLAUDE.md")
	writeArtifact(t, s, acf.KindMemory, acf.ScopeProject, "x/z", "/q/CLAUDE.md")
	n, err := Count(s, reg)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

func TestListWithDiscovered_IncludesHarvestedFolders(t *testing.T) {
	store := newStore(t)
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)
	// ListWithDiscovered absolutizes the discovered path; on Windows a POSIX
	// literal gains a drive prefix, so derive the expectation via Abs too.
	dfPath := filepath.FromSlash("/Users/testuser")
	dfAbs, err := filepath.Abs(dfPath)
	require.NoError(t, err)
	disc := []projectdiscovery.DiscoveredFolder{
		{Path: dfPath, Agents: []string{"codex"}, IsGitRepo: false},
	}
	got, err := ListWithDiscovered(store, reg, disc)
	require.NoError(t, err)
	var found *Project
	for i := range got {
		if got[i].SamplePath == dfAbs {
			found = &got[i]
		}
	}
	require.NotNil(t, found, "discovered folder must appear in the pending list")
	require.Equal(t, "discovered", found.Source)
	require.Equal(t, []string{"codex"}, found.Agents)
}

func TestListWithDiscovered_SkipsRegisteredFolder(t *testing.T) {
	store := newStore(t)
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)

	// Use a real temp dir so project.Detect resolves a stable ID for
	// both the registry entry and the discovered folder.
	dir := t.TempDir()
	dir, err = filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	info, err := project.Detect(dir)
	require.NoError(t, err)
	require.NoError(t, reg.AddOrUpdate(project.Entry{ID: info.ID, Path: dir, VCS: info.VCS}))

	disc := []projectdiscovery.DiscoveredFolder{
		{Path: dir, Agents: []string{"codex"}, IsGitRepo: false},
	}
	got, err := ListWithDiscovered(store, reg, disc)
	require.NoError(t, err)

	// The registered folder must NOT appear as a Source="discovered" dup.
	for _, p := range got {
		if p.Source == "discovered" {
			require.NotEqual(t, dir, p.SamplePath,
				"already-registered folder must not be re-surfaced as discovered")
			require.NotEqual(t, info.ID, p.ID,
				"already-registered project ID must not be re-surfaced as discovered")
		}
	}
}

func TestListWithDiscovered_DiscoveredFieldsPopulated(t *testing.T) {
	store := newStore(t)
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)

	// An artifact-pending project (Source="artifact") and a distinct
	// discovered folder (Source="discovered").
	writeArtifact(t, store, acf.KindMemory, acf.ScopeProject, "github.com/unknown/repo", "/anywhere/CLAUDE.md")
	disc := []projectdiscovery.DiscoveredFolder{
		{Path: "/Users/testuser/code/widget", Agents: []string{"claude-code", "codex"}, LastActive: 1717000000, IsGitRepo: true},
	}

	got, err := ListWithDiscovered(store, reg, disc)
	require.NoError(t, err)

	var artifact, discovered *Project
	for i := range got {
		switch got[i].Source {
		case "artifact":
			artifact = &got[i]
		case "discovered":
			discovered = &got[i]
		}
	}

	require.NotNil(t, artifact, "artifact-pending entry must be present")
	require.Equal(t, "artifact", artifact.Source)
	require.Equal(t, "github.com/unknown/repo", artifact.ID)

	require.NotNil(t, discovered, "discovered entry must be present")
	require.Equal(t, "discovered", discovered.Source)
	require.Equal(t, 0, discovered.ArtifactCount, "discovered entries have no parked artifacts yet")
	require.True(t, discovered.IsGitRepo, "IsGitRepo must pass through from DiscoveredFolder")
	require.Equal(t, int64(1717000000), discovered.LastActive, "LastActive must pass through from DiscoveredFolder")
	require.Equal(t, []string{"claude-code", "codex"}, discovered.Agents)
}

func TestListWithDiscovered_UpgradesArtifactPendingToDiscovered(t *testing.T) {
	store := newStore(t)
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)

	// A folder the user just removed: it still has project-scoped artifacts in
	// the store AND is freshly discovered. project.Detect gives the same ID for
	// both, so they must MERGE into one discovered row (Approve flow) rather
	// than leaving a "from artifacts" row with the legacy Link flow.
	dir := t.TempDir()
	info, err := project.Detect(dir)
	require.NoError(t, err)
	writeArtifact(t, store, acf.KindMemory, acf.ScopeProject, info.ID, filepath.Join(dir, "CLAUDE.md"))

	disc := []projectdiscovery.DiscoveredFolder{
		{Path: dir, Agents: []string{"claude-code"}, LastActive: 1717000000, IsGitRepo: false},
	}
	got, err := ListWithDiscovered(store, reg, disc)
	require.NoError(t, err)

	var rows []Project
	for _, p := range got {
		if p.ID == info.ID {
			rows = append(rows, p)
		}
	}
	require.Len(t, rows, 1, "artifact-pending + discovered for one folder must merge into a single row")
	row := rows[0]
	require.Equal(t, "discovered", row.Source, "merged row must use the discovered (Approve) flow")
	require.Equal(t, dir, row.SamplePath, "merged row must carry the folder path, not the artifact file path")
	require.Equal(t, []string{"claude-code"}, row.Agents)
	require.Greater(t, row.ArtifactCount, 0, "the parked artifact count must be preserved on the upgraded row")
}

func TestAgentSuggestions(t *testing.T) {
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)

	// A registered project scoped to claude-code only.
	dir := t.TempDir()
	dir, err = filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	info, err := project.Detect(dir)
	require.NoError(t, err)
	require.NoError(t, reg.AddOrUpdate(project.Entry{ID: info.ID, Path: dir, VCS: info.VCS, Agents: []string{"claude-code"}}))

	// An all-agents project (empty Agents) must never suggest.
	allDir := t.TempDir()
	allDir, err = filepath.EvalSymlinks(allDir)
	require.NoError(t, err)
	allInfo, err := project.Detect(allDir)
	require.NoError(t, err)
	require.NoError(t, reg.AddOrUpdate(project.Entry{ID: allInfo.ID, Path: allDir, VCS: allInfo.VCS}))

	disc := []projectdiscovery.DiscoveredFolder{
		{Path: dir, Agents: []string{"claude-code", "codex"}, LastActive: 1717000000},
		{Path: allDir, Agents: []string{"codex"}},
	}

	dismissed, err := LoadDenied(filepath.Join(t.TempDir(), "dismissed.json"))
	require.NoError(t, err)

	got := AgentSuggestions(reg, disc, dismissed)
	require.Len(t, got, 1, "only the scoped project with a new agent should suggest")
	require.Equal(t, info.ID, got[0].ID)
	require.Equal(t, "agent-suggestion", got[0].Source)
	require.Equal(t, dir, got[0].SamplePath)
	require.Equal(t, []string{"codex"}, got[0].SuggestAgents, "claude-code already in the set; only codex is new")

	// Once dismissed, the (project, codex) suggestion disappears.
	require.NoError(t, dismissed.Add(SuggestionKey(info.ID, "codex"), ""))
	require.Empty(t, AgentSuggestions(reg, disc, dismissed), "dismissed suggestion must not re-appear")
}
