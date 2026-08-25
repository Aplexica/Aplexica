package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

const claudeBody = "# Project memory\n\n## Conventions\n- Language: Go 1.23.\n\n## Preferences\n- user name is Example User\n"

// type:user auto-memory topics → fold into global ~/.claude/CLAUDE.md.
const dogsTopic = "---\nname: dogs\nmetadata:\n  type: user\n---\n\nExample User's dogs are Comet and Nova.\n"
const locTopic = "---\nname: user-location\nmetadata:\n  type: user\n---\n\nExample User lives in Example City, Example Region.\n"
const memIndex = "# Memory index\n\n- [Dogs](dogs.md) — Comet and Nova\n- [User location](user-location.md)\n"

// homeMemDir returns the HomeDir-rooted auto-memory dir for the adapter (the dir
// for sessions run from the user's home directory).
func homeMemDir(a *Adapter) string {
	return filepath.Join(a.globalClaudeRoot(), "projects", encodeProjectDir(a.HomeDir), "memory")
}

// seedClaudeAutoMemory writes ~/.claude/CLAUDE.md plus the home-rooted
// auto-memory dir (index + two type:user topic files) and returns the adapter,
// store, and the global CLAUDE.md path.
func seedClaudeAutoMemory(t *testing.T) (*Adapter, *acf.Store, string) {
	t.Helper()
	home := t.TempDir()
	s := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, s.Init())
	ss := &secrets.Store{Root: filepath.Join(home, "secrets")}
	require.NoError(t, ss.Init())

	a := &Adapter{HomeDir: home, DeviceID: "dev", SecretsStore: ss}

	claudePath := filepath.Join(home, ".claude", "CLAUDE.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(claudePath), 0o755))
	require.NoError(t, os.WriteFile(claudePath, []byte(claudeBody), 0o644))

	memDir := homeMemDir(a)
	require.NoError(t, os.MkdirAll(memDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte(memIndex), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(memDir, "dogs.md"), []byte(dogsTopic), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(memDir, "user-location.md"), []byte(locTopic), 0o644))

	return a, s, claudePath
}

func replayMemoryForTest(t *testing.T, s *acf.Store, id string) string {
	t.Helper()
	events, err := s.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	p, err := acf.DecodeMemoryPayload(events[len(events)-1])
	require.NoError(t, err)
	return p.Content
}

func TestParseAutoMemoryEntry(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantType string
		wantBody string
	}{
		{
			name:     "project (nested under metadata)",
			content:  "---\nname: t\nmetadata:\n  type: project\n---\n\nThe project fact.\n",
			wantType: "project",
			wantBody: "The project fact.",
		},
		{
			name:     "user (nested under metadata)",
			content:  "---\nname: t\nmetadata:\n  type: user\n---\n\nThe user fact.\n",
			wantType: "user",
			wantBody: "The user fact.",
		},
		{
			name:     "top-level type",
			content:  "---\ntype: user\n---\nA top-level typed fact.\n",
			wantType: "user",
			wantBody: "A top-level typed fact.",
		},
		{
			name:     "no frontmatter",
			content:  "Just a plain body, no fence.\n",
			wantType: "",
			wantBody: "Just a plain body, no fence.",
		},
		{
			name:     "frontmatter without type",
			content:  "---\nname: t\n---\n\nBody with untyped frontmatter.\n",
			wantType: "",
			wantBody: "Body with untyped frontmatter.",
		},
		{
			name:     "multi-line body preserved",
			content:  "---\nmetadata:\n  type: project\n---\n\nLine one.\nLine two.\n",
			wantType: "project",
			wantBody: "Line one.\nLine two.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotBody := parseAutoMemoryEntry(tc.content)
			require.Equal(t, tc.wantType, gotType)
			require.Equal(t, tc.wantBody, gotBody)
		})
	}
}

func TestImportAutoMemory_TypeUser_FoldsIntoGlobalClaudeArtifact(t *testing.T) {
	a, s, claudePath := seedClaudeAutoMemory(t)
	ctx := context.Background()

	// A change to a type:user topic file routes through importGlobalMemory and
	// updates the single GLOBAL CLAUDE.md-keyed artifact with the composed body.
	ids, err := a.Import(ctx, s, filepath.Join(homeMemDir(a), "dogs.md"))
	require.NoError(t, err)
	require.Len(t, ids, 1, "no registry → only the global artifact is touched")

	got, err := s.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Equal(t, claudePath, got.SourcePath, "type:user edits update the global CLAUDE.md-keyed artifact")
	require.Equal(t, acf.ScopeGlobal, got.Scope)

	events, err := s.ReadEvents(acf.KindMemory, ids[0])
	require.NoError(t, err)
	for _, e := range events {
		require.Equal(t, "claude-code", e.Provenance.SourceAgent)
	}

	content := replayMemoryForTest(t, s, ids[0])
	require.Contains(t, content, "user name is Example User")
	require.Contains(t, content, "Comet and Nova")
	require.Contains(t, content, "Example City, Example Region")
	require.NotContains(t, content, "[Dogs](dogs.md)", "the MEMORY.md index must NOT be folded in")
	require.NotContains(t, content, "metadata:", "only the topic BODY folds in, not the frontmatter")
}

func TestExportGlobalMemory_KeepsClaudePristine(t *testing.T) {
	a, s, claudePath := seedClaudeAutoMemory(t)
	ctx := context.Background()

	ids, err := a.Import(ctx, s, filepath.Join(homeMemDir(a), "dogs.md"))
	require.NoError(t, err)

	require.NoError(t, a.ExportMemory(ctx, s, ids[0], claudePath))
	after, err := os.ReadFile(claudePath)
	require.NoError(t, err)
	require.Equal(t, claudeBody, string(after),
		"CLAUDE.md MUST stay pristine — type:user auto-memory already read by /memory must not be duplicated")
}

func TestExportGlobalMemory_OtherAgentGetsMergedView(t *testing.T) {
	a, s, _ := seedClaudeAutoMemory(t)
	ctx := context.Background()

	ids, err := a.Import(ctx, s, filepath.Join(homeMemDir(a), "dogs.md"))
	require.NoError(t, err)

	codexAgents := filepath.Join(t.TempDir(), "AGENTS.md")
	require.NoError(t, a.ExportMemory(ctx, s, ids[0], codexAgents))
	out, err := os.ReadFile(codexAgents)
	require.NoError(t, err)
	require.Contains(t, string(out), "Comet and Nova", "other agents receive the merged memory")
	require.Contains(t, string(out), "Example City, Example Region")
}

func TestImportGlobalClaude_NoAutoMemory_ByteIdentical(t *testing.T) {
	home := t.TempDir()
	s := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, s.Init())
	a := &Adapter{HomeDir: home, DeviceID: "dev"}

	claudePath := filepath.Join(home, ".claude", "CLAUDE.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(claudePath), 0o755))
	require.NoError(t, os.WriteFile(claudePath, []byte(claudeBody), 0o644))

	ids, err := a.Import(context.Background(), s, claudePath)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	require.Equal(t, claudeBody, replayMemoryForTest(t, s, ids[0]))
}

func TestImportAutoMemoryIndex_AddsNothing(t *testing.T) {
	a, s, _ := seedClaudeAutoMemory(t)
	ctx := context.Background()

	// Importing via the MEMORY.md index (its own change) composes from the topic
	// bodies only; the index's wikilinks never enter the artifact.
	ids, err := a.Import(ctx, s, filepath.Join(homeMemDir(a), "MEMORY.md"))
	require.NoError(t, err)
	require.Len(t, ids, 1)
	content := replayMemoryForTest(t, s, ids[0])
	require.NotContains(t, content, "[Dogs](dogs.md)")
	require.Contains(t, content, "Comet and Nova")
}

// projectFact / projectTopic are the type:project topic that must route to the
// REGISTERED project's memory, never to global.
const projectTopicBody = "The user considers Test123 their second-best test project."
const projectTopic = "---\nname: test123-second-best\nmetadata:\n  type: project\n---\n\n" + projectTopicBody + "\n"

// seedTypeRouting builds the full type-aware fixture: a global CLAUDE.md, a
// HomeDir auto-memory dir with a type:user topic (dogs), a registered Test123
// project with its own auto-memory dir holding a type:project topic, and a
// registry set on the adapter. Returns the adapter, store, global CLAUDE.md
// path, and the Test123 project path.
func seedTypeRouting(t *testing.T) (a *Adapter, s *acf.Store, globalClaude, test123 string) {
	t.Helper()
	home := t.TempDir()
	s = &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, s.Init())
	ss := &secrets.Store{Root: filepath.Join(home, "secrets")}
	require.NoError(t, ss.Init())

	// Test123 lives OUTSIDE HomeDir so its encoded dir differs from the home dir.
	test123 = t.TempDir()
	physicalTest123, err := filepath.EvalSymlinks(test123)
	require.NoError(t, err)
	test123 = physicalTest123
	reg, err := project.NewRegistry(filepath.Join(home, "projects.json"))
	require.NoError(t, err)
	require.NoError(t, reg.Add(project.Entry{
		ID:    "test123",
		Path:  test123,
		VCS:   "git",
		Scope: "local",
	}))

	a = &Adapter{HomeDir: home, DeviceID: "dev", SecretsStore: ss, Registry: reg}

	globalClaude = filepath.Join(home, ".claude", "CLAUDE.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(globalClaude), 0o755))
	require.NoError(t, os.WriteFile(globalClaude, []byte(claudeBody), 0o644))

	// HomeDir auto-memory: a type:user topic (dogs).
	hmem := homeMemDir(a)
	require.NoError(t, os.MkdirAll(hmem, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hmem, "dogs.md"), []byte(dogsTopic), 0o644))

	// Test123 auto-memory: a type:project topic.
	pmem := filepath.Join(home, ".claude", "projects", encodeProjectDir(test123), "memory")
	require.NoError(t, os.MkdirAll(pmem, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pmem, "test123-second-best.md"), []byte(projectTopic), 0o644))

	return a, s, globalClaude, test123
}

func TestTypeRouting_ProjectTopicRoutesToProjectGlobalGetsUser(t *testing.T) {
	a, s, globalClaude, test123 := seedTypeRouting(t)
	ctx := context.Background()

	// Importing the type:project topic file recomposes GLOBAL (type:user) and the
	// PROJECT (type:project) — returns the union of ids.
	pmem := filepath.Join(a.HomeDir, ".claude", "projects", encodeProjectDir(test123), "memory")
	ids, err := a.Import(ctx, s, filepath.Join(pmem, "test123-second-best.md"))
	require.NoError(t, err)
	require.Len(t, ids, 2, "global recompose + project recompose")

	projectClaude := filepath.Join(test123, "CLAUDE.md")

	// Locate the project artifact and the global artifact by SourcePath.
	var projArt, globalArt acf.Artifact
	for _, id := range ids {
		art, rerr := s.ReadArtifact(acf.KindMemory, id)
		require.NoError(t, rerr)
		switch art.SourcePath {
		case projectClaude:
			projArt = art
		case globalClaude:
			globalArt = art
		}
	}
	require.Equal(t, projectClaude, projArt.SourcePath, "the project artifact is keyed by <Test123>/CLAUDE.md")
	require.Equal(t, globalClaude, globalArt.SourcePath, "the global artifact is keyed by ~/.claude/CLAUDE.md")

	// (a) The PROJECT artifact is project-scoped and carries the project body.
	require.Equal(t, acf.ScopeProject, projArt.Scope, "type:project memory stays project-scoped")
	require.NotNil(t, projArt.Project)
	require.Equal(t, "test123", projArt.Project.ID)
	projContent := replayMemoryForTest(t, s, projArt.ArtifactID)
	require.Contains(t, projContent, projectTopicBody)

	// (b) The GLOBAL artifact does NOT contain the project body but DOES contain
	// the type:user (dogs) body.
	require.Equal(t, acf.ScopeGlobal, globalArt.Scope)
	globalContent := replayMemoryForTest(t, s, globalArt.ArtifactID)
	require.NotContains(t, globalContent, projectTopicBody, "type:project body must NEVER leak into global")
	require.Contains(t, globalContent, "Comet and Nova", "type:user body folds into global")

	// (c) ExportMemory to the global CLAUDE.md stays pristine.
	require.NoError(t, a.ExportMemory(ctx, s, globalArt.ArtifactID, globalClaude))
	gAfter, err := os.ReadFile(globalClaude)
	require.NoError(t, err)
	require.Equal(t, claudeBody, string(gAfter), "global CLAUDE.md stays pristine")

	// (d) The project fact lives in the artifact (which fans out to other agents
	// — verified via a foreign dest below). Exporting to <Test123>/CLAUDE.md —
	// the registered project's OWN file — strips the type:project body back out
	// so the hand-authored CLAUDE.md stays pristine (no duplication, no loop).
	foreignDest := filepath.Join(t.TempDir(), "AGENTS.md")
	require.NoError(t, a.ExportMemory(ctx, s, projArt.ArtifactID, foreignDest))
	fAfter, err := os.ReadFile(foreignDest)
	require.NoError(t, err)
	require.Contains(t, string(fAfter), projectTopicBody, "another agent's file gets the merged project memory")

	require.NoError(t, a.ExportMemory(ctx, s, projArt.ArtifactID, projectClaude))
	pAfter, err := os.ReadFile(projectClaude)
	require.NoError(t, err)
	require.NotContains(t, string(pAfter), projectTopicBody,
		"export to the registered project's own CLAUDE.md strips the type:project body back out (pristine)")
}

func TestImportRegisteredProjectClaudeMd_FoldsProjectTopics(t *testing.T) {
	a, s, _, test123 := seedTypeRouting(t)
	ctx := context.Background()

	// Seed the project's own hand-authored CLAUDE.md, then import THAT path.
	projectClaude := filepath.Join(test123, "CLAUDE.md")
	projBody := "# Test123\n\n- build with make\n"
	require.NoError(t, os.WriteFile(projectClaude, []byte(projBody), 0o644))

	ids, err := a.Import(ctx, s, projectClaude)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	art, err := s.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Equal(t, projectClaude, art.SourcePath)
	require.Equal(t, acf.ScopeProject, art.Scope, "a registered project's CLAUDE.md is project-scoped")

	content := replayMemoryForTest(t, s, ids[0])
	require.Contains(t, content, "build with make", "the hand-authored body is preserved")
	require.Contains(t, content, projectTopicBody, "the project's type:project auto-memory folds in")
	require.NotContains(t, content, "Comet and Nova", "type:user memory does NOT leak into the project artifact")
}

// TestTypeRouting_GlobalScopeProject_DoesNotGlobalizeProjectTopic pins I1: a
// project registered scope:"global" must NOT have its type:project auto-memory
// fanned out to global. The local-only gate (projectForEnc /
// registeredProjectForClaudePath) skips global-scope projects entirely, so the
// project body lands in NO global artifact (global-project memory is deferred).
func TestTypeRouting_GlobalScopeProject_DoesNotGlobalizeProjectTopic(t *testing.T) {
	home := t.TempDir()
	s := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, s.Init())
	ss := &secrets.Store{Root: filepath.Join(home, "secrets")}
	require.NoError(t, ss.Init())

	// gproj is registered scope:"global" — the dangerous case I1 closes.
	gproj := t.TempDir()
	reg, err := project.NewRegistry(filepath.Join(home, "projects.json"))
	require.NoError(t, err)
	require.NoError(t, reg.Add(project.Entry{
		ID:    "gproj",
		Path:  gproj,
		VCS:   "git",
		Scope: "global",
	}))

	a := &Adapter{HomeDir: home, DeviceID: "dev", SecretsStore: ss, Registry: reg}

	globalClaude := filepath.Join(home, ".claude", "CLAUDE.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(globalClaude), 0o755))
	require.NoError(t, os.WriteFile(globalClaude, []byte(claudeBody), 0o644))

	// gproj's auto-memory holds a type:project topic.
	pmem := filepath.Join(home, ".claude", "projects", encodeProjectDir(gproj), "memory")
	require.NoError(t, os.MkdirAll(pmem, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pmem, "secret.md"), []byte(projectTopic), 0o644))

	ctx := context.Background()
	ids, err := a.Import(ctx, s, filepath.Join(pmem, "secret.md"))
	require.NoError(t, err)
	// Only the global recompose runs (the global-scope project is NOT routed),
	// and it does NOT pull in the type:project body.
	require.Len(t, ids, 1, "global-scope project is not routed → no project artifact minted")

	// No artifact anywhere carries the project body at global scope.
	artifacts, err := s.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	for _, art := range artifacts {
		body := replayMemoryForTest(t, s, art.ArtifactID)
		if art.Scope == acf.ScopeGlobal {
			require.NotContains(t, body, projectTopicBody,
				"type:project memory MUST NOT be globalized for a scope:global project (I1)")
		}
	}

	// Specifically, the global ~/.claude/CLAUDE.md artifact stays pristine + the
	// project fact is absent.
	globalArt, ok := func() (acf.Artifact, bool) {
		for _, art := range artifacts {
			if art.SourcePath == globalClaude {
				return art, true
			}
		}
		return acf.Artifact{}, false
	}()
	require.True(t, ok, "the global CLAUDE.md artifact exists")
	require.Equal(t, acf.ScopeGlobal, globalArt.Scope)
	require.NotContains(t, replayMemoryForTest(t, s, globalArt.ArtifactID), projectTopicBody,
		"global CLAUDE.md artifact must not contain the global-scope project's type:project body")

	// And importing the project's own CLAUDE.md is NOT routed through
	// importProjectMemory either (registeredProjectForClaudePath gate) — it falls
	// to the plain memory importer, where the type:project topic never composes
	// in, so the body still can't reach a global artifact.
	gprojClaude := filepath.Join(gproj, "CLAUDE.md")
	require.NoError(t, os.WriteFile(gprojClaude, []byte("# G\n"), 0o644))
	cids, err := a.Import(ctx, s, gprojClaude)
	require.NoError(t, err)
	require.Len(t, cids, 1)
	require.NotContains(t, replayMemoryForTest(t, s, cids[0]), projectTopicBody,
		"plain import of a global-scope project's CLAUDE.md does not fold in its type:project auto-memory")
}
