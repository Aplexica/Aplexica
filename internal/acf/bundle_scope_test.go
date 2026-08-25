package acf

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/project"
	"github.com/stretchr/testify/require"
)

// scaffoldStoreWithMixedScopes writes 4 artifacts spanning all scopes:
//
//	memory  / global  / no project
//	skill   / project / github.com/a/x
//	memory  / project / github.com/b/y
//	tool    / project / no Project field (legacy v0.55.0 artifact)
//
// Returns the store and the artifact IDs in declaration order.
func scaffoldStoreWithMixedScopes(t *testing.T) (*Store, []string) {
	t.Helper()
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	now := time.Now().UTC()
	ids := make([]string, 4)

	mk := func(i int, kind Kind, scope Scope, proj *project.ProjectInfo, name string) {
		id := NewID()
		ids[i] = id
		require.NoError(t, s.WriteArtifact(Artifact{
			AcfSchemaVersion: SchemaVersion,
			ArtifactID:       id,
			Kind:             kind,
			Scope:            scope,
			Project:          proj,
			Name:             name,
			SourcePath:       "/fake/" + name,
			CreatedAt:        now,
			UpdatedAt:        now,
		}))
	}
	mk(0, KindMemory, ScopeGlobal, nil, "global-memory.md")
	mk(1, KindSkill, ScopeProject, &project.ProjectInfo{ID: "github.com/a/x", Path: "/a/x", VCS: "git"}, "skill-a.md")
	mk(2, KindMemory, ScopeProject, &project.ProjectInfo{ID: "github.com/b/y", Path: "/b/y", VCS: "git"}, "memory-b.md")
	mk(3, KindTool, ScopeProject, nil, "legacy-tool.json") // pre-v0.55.0 shape
	return s, ids
}

// bundleAndListNames runs Bundle, opens the resulting tar.gz, and
// returns the sorted list of archive entry names (excludes meta.json
// for stable comparisons).
func bundleAndListNames(t *testing.T, s *Store, opts BundleOpts) []string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, s.Bundle(&buf, opts))
	gz, err := gzip.NewReader(&buf)
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Name == "meta.json" {
			continue
		}
		names = append(names, hdr.Name)
	}
	return names
}

func TestBundle_NoFilters_IncludesEverything(t *testing.T) {
	s, _ := scaffoldStoreWithMixedScopes(t)
	names := bundleAndListNames(t, s, BundleOpts{
		AplexicaVersion: "test",
	})
	// Every artifact has acf/<plural>/<id>.json. No filter → 4 entries.
	count := 0
	for _, n := range names {
		if strings.HasPrefix(n, "acf/") && strings.HasSuffix(n, ".json") {
			count++
		}
	}
	require.Equal(t, 4, count, "no filters → all 4 artifacts included")
}

func TestBundle_ScopeFilterGlobal_OnlyGlobalArtifacts(t *testing.T) {
	s, ids := scaffoldStoreWithMixedScopes(t)
	names := bundleAndListNames(t, s, BundleOpts{
		AplexicaVersion: "test",
		ScopeFilter:     ScopeGlobal,
	})
	require.True(t, containsArtifact(names, "memories", ids[0]),
		"global memory MUST be included")
	require.False(t, containsArtifact(names, "skills", ids[1]),
		"project skill MUST be excluded")
	require.False(t, containsArtifact(names, "memories", ids[2]),
		"project memory MUST be excluded")
}

func TestBundle_ScopeFilterProject_OnlyProjectArtifacts(t *testing.T) {
	s, ids := scaffoldStoreWithMixedScopes(t)
	names := bundleAndListNames(t, s, BundleOpts{
		AplexicaVersion: "test",
		ScopeFilter:     ScopeProject,
	})
	require.False(t, containsArtifact(names, "memories", ids[0]),
		"global memory MUST be excluded")
	require.True(t, containsArtifact(names, "skills", ids[1]))
	require.True(t, containsArtifact(names, "memories", ids[2]))
	require.True(t, containsArtifact(names, "tools", ids[3]),
		"project-scope artifact with nil Project still passes scope filter")
}

func TestBundle_ProjectFilter_OnlyMatchingIDs(t *testing.T) {
	s, ids := scaffoldStoreWithMixedScopes(t)
	names := bundleAndListNames(t, s, BundleOpts{
		AplexicaVersion: "test",
		ProjectFilter:   []string{"github.com/a/x"},
	})
	// Only the skill for github.com/a/x survives.
	require.True(t, containsArtifact(names, "skills", ids[1]))
	require.False(t, containsArtifact(names, "memories", ids[0]),
		"global has no Project.ID — must be excluded")
	require.False(t, containsArtifact(names, "memories", ids[2]),
		"other project's memory must be excluded")
	require.False(t, containsArtifact(names, "tools", ids[3]),
		"nil Project → cannot match ProjectFilter")
}

func TestBundle_PendingExclusion_DropsPendingArtifacts(t *testing.T) {
	s, ids := scaffoldStoreWithMixedScopes(t)
	// Mark github.com/a/x as pending (NOT in registry on this device).
	// The skill should drop; everything else stays.
	names := bundleAndListNames(t, s, BundleOpts{
		AplexicaVersion: "test",
		PendingIDs: map[string]struct{}{
			"github.com/a/x": {},
		},
	})
	require.False(t, containsArtifact(names, "skills", ids[1]),
		"pending project's skill MUST be excluded")
	require.True(t, containsArtifact(names, "memories", ids[0]),
		"global is unaffected by pending-exclusion")
	require.True(t, containsArtifact(names, "memories", ids[2]),
		"different-project memory is unaffected")
	require.True(t, containsArtifact(names, "tools", ids[3]),
		"nil Project artifact is never marked pending (no ID to match)")
}

func TestBundle_FiltersCompose_ScopeAndProject(t *testing.T) {
	s, ids := scaffoldStoreWithMixedScopes(t)
	// Both filters: project scope + only github.com/b/y.
	names := bundleAndListNames(t, s, BundleOpts{
		AplexicaVersion: "test",
		ScopeFilter:     ScopeProject,
		ProjectFilter:   []string{"github.com/b/y"},
	})
	require.False(t, containsArtifact(names, "memories", ids[0]),
		"global memory excluded by scope filter")
	require.False(t, containsArtifact(names, "skills", ids[1]),
		"github.com/a/x excluded by project filter")
	require.True(t, containsArtifact(names, "memories", ids[2]))
	require.False(t, containsArtifact(names, "tools", ids[3]),
		"nil Project artifact excluded by project filter")
}

func TestBundleOpts_IncludeArtifact_TableDriven(t *testing.T) {
	gitInfo := &project.ProjectInfo{ID: "github.com/x/y", VCS: "git"}

	cases := []struct {
		name string
		opts BundleOpts
		art  Artifact
		want bool
	}{
		{
			name: "no filters → include",
			opts: BundleOpts{},
			art:  Artifact{Scope: ScopeGlobal},
			want: true,
		},
		{
			name: "scope match → include",
			opts: BundleOpts{ScopeFilter: ScopeProject},
			art:  Artifact{Scope: ScopeProject, Project: gitInfo},
			want: true,
		},
		{
			name: "scope mismatch → exclude",
			opts: BundleOpts{ScopeFilter: ScopeProject},
			art:  Artifact{Scope: ScopeGlobal},
			want: false,
		},
		{
			name: "project ID match → include",
			opts: BundleOpts{ProjectFilter: []string{"github.com/x/y"}},
			art:  Artifact{Scope: ScopeProject, Project: gitInfo},
			want: true,
		},
		{
			name: "project ID mismatch → exclude",
			opts: BundleOpts{ProjectFilter: []string{"github.com/other/zz"}},
			art:  Artifact{Scope: ScopeProject, Project: gitInfo},
			want: false,
		},
		{
			name: "project filter set + nil Project → exclude",
			opts: BundleOpts{ProjectFilter: []string{"github.com/x/y"}},
			art:  Artifact{Scope: ScopeProject, Project: nil},
			want: false,
		},
		{
			name: "pending exclusion → exclude",
			opts: BundleOpts{PendingIDs: map[string]struct{}{"github.com/x/y": {}}},
			art:  Artifact{Scope: ScopeProject, Project: gitInfo},
			want: false,
		},
		{
			name: "pending set but artifact not pending → include",
			opts: BundleOpts{PendingIDs: map[string]struct{}{"github.com/other/zz": {}}},
			art:  Artifact{Scope: ScopeProject, Project: gitInfo},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.opts.includeArtifact(tc.art)
			require.Equal(t, tc.want, got)
		})
	}
}

// containsArtifact reports whether bundle entry "acf/<kindplural>/<id>.json"
// appears in names.
func containsArtifact(names []string, kindplural, id string) bool {
	want := "acf/" + kindplural + "/" + id + ".json"
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
