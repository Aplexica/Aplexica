package projectdiscovery

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/stretchr/testify/require"
)

type fakeSource struct {
	name string
	dirs []adapter.ProjectPresence
}

func (f *fakeSource) Name() string                                    { return f.name }
func (f *fakeSource) ProjectDirs() ([]adapter.ProjectPresence, error) { return f.dirs, nil }

func newTestRegistry(t *testing.T) *project.Registry {
	t.Helper()
	r, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)
	return r
}

// absPath converts a POSIX-style test literal to the platform's absolute
// form. HarvestAll absolutizes every harvested path, so on Windows a literal
// like /Users/testuser comes back as D:\Users\testuser — inputs and expectations must
// both go through the same normalization or the tests only pass on unix.
func absPath(t *testing.T, posix string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.FromSlash(posix))
	require.NoError(t, err)
	return abs
}

// realDir materialises a POSIX-style relative layout as actual directories
// under a single per-test temp root and returns the absolute path of each.
// HarvestAll now stat-gates candidates (drops paths that don't exist on
// disk), so any folder a test expects to SURVIVE must physically exist;
// callers that need a deliberately-missing or excluded path keep using the
// synthetic absPath literals (those are dropped before the stat anyway).
func realDir(t *testing.T, posixRel ...string) []string {
	t.Helper()
	base := t.TempDir()
	out := make([]string, len(posixRel))
	for i, rel := range posixRel {
		abs := filepath.Join(base, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(abs, 0o755))
		out[i] = abs
	}
	return out
}

func TestHarvest_UnionsAndFiltersRegistered(t *testing.T) {
	dirs := realDir(t, "testuser", "testuser/repo")
	home, repo := dirs[0], dirs[1]
	t1 := time.Unix(100, 0)
	t2 := time.Unix(200, 0)
	codex := &fakeSource{name: "codex", dirs: []adapter.ProjectPresence{
		{Path: home, LastActive: t1},
		{Path: repo, LastActive: t1},
	}}
	claude := &fakeSource{name: "claude-code", dirs: []adapter.ProjectPresence{
		{Path: home, LastActive: t2},
	}}

	reg := newTestRegistry(t)
	require.NoError(t, reg.AddOrUpdate(project.Entry{ID: "id-repo", Path: repo, Scope: "local"}))

	got, hasMore, err := Harvest([]HarvestSource{codex, claude}, reg, filepath.Join(home, ".aplexica"))
	require.NoError(t, err)
	require.False(t, hasMore, "well under the cap → no truncation")

	// repo is registered → filtered out. home remains, with both agents and
	// the newer LastActive.
	require.Len(t, got, 1)
	require.Equal(t, home, got[0].Path)
	require.ElementsMatch(t, []string{"codex", "claude-code"}, got[0].Agents)
	require.Equal(t, t2.Unix(), got[0].LastActive)
}

func TestHarvestAll_ExcludesAgentRootsAndVendorDirs(t *testing.T) {
	// "Real" must exist on disk to clear the existence gate; the excluded
	// candidates are dropped by excludedCandidate BEFORE the stat, so they
	// stay as synthetic paths under the same base (no need to materialise
	// them). joining onto base keeps the exclude-root prefix match intact.
	real := realDir(t, "testuser/Projects/Real")[0]
	base := filepath.Dir(filepath.Dir(filepath.Dir(real))) // .../<tmp>/testuser/Projects/Real → <tmp>
	under := func(rel string) string { return filepath.Join(base, filepath.FromSlash(rel)) }
	kiloRoot := under("testuser/.config/kilo")
	kilo := &fakeSource{name: "kilo", dirs: []adapter.ProjectPresence{
		{Path: kiloRoot, LastActive: time.Unix(500, 0)},                                        // IS an agent config root
		{Path: under("testuser/.config/kilo/skills"), LastActive: time.Unix(500, 0)},           // UNDER an agent config root
		{Path: under("testuser/Projects/Foo/node_modules/zod"), LastActive: time.Unix(450, 0)}, // vendor segment
		{Path: under("testuser/Projects/Bar/.git"), LastActive: time.Unix(440, 0)},             // VCS-internal segment
		{Path: real, LastActive: time.Unix(300, 0)},                                            // legitimate project
	}}

	got, err := HarvestAll([]HarvestSource{kilo}, under("testuser/.aplexica"),
		kiloRoot, under("testuser/.codex"), under("testuser/.claude"))
	require.NoError(t, err)
	require.Len(t, got, 1, "agent config root + its subtree + node_modules + .git must all be excluded")
	require.Equal(t, real, got[0].Path)
}

func TestHarvestAll_NoExcludeRoots_StillDropsVendorDirs(t *testing.T) {
	// With no excludeRoots passed (backward-compatible call), agent config
	// roots are not known and survive, but vendor/VCS segments are always
	// dropped regardless. The vendor path is dropped before the existence
	// gate, so it stays synthetic; "Real" must exist on disk to survive.
	real := realDir(t, "testuser/Projects/Real")[0]
	kilo := &fakeSource{name: "kilo", dirs: []adapter.ProjectPresence{
		{Path: absPath(t, "/Users/testuser/Projects/Foo/node_modules/zod"), LastActive: time.Unix(450, 0)},
		{Path: real, LastActive: time.Unix(300, 0)},
	}}
	got, err := HarvestAll([]HarvestSource{kilo}, absPath(t, "/Users/testuser/.aplexica"))
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, real, got[0].Path)
}

func TestHarvestAll_DropsVanishedDirs(t *testing.T) {
	// A candidate an agent ran in once but that has since been deleted or
	// renamed must NOT resurface (design spec §1.2: "Drop paths that no
	// longer exist on disk"). validCandidatePath only rejects malformed
	// strings, and project.Detect happily returns VCS="none" for a missing
	// path, so without an existence gate a deleted folder surfaces forever.
	live := t.TempDir() // exists on disk
	gone := filepath.Join(t.TempDir(), "since-deleted")
	require.NoDirExists(t, gone)

	src := &fakeSource{name: "codex", dirs: []adapter.ProjectPresence{
		{Path: live, LastActive: time.Unix(300, 0)},
		{Path: gone, LastActive: time.Unix(200, 0)},
	}}

	got, err := HarvestAll([]HarvestSource{src}, filepath.Join(t.TempDir(), ".aplexica"))
	require.NoError(t, err)
	require.Len(t, got, 1, "a candidate folder that no longer exists on disk must be dropped")
	require.Equal(t, live, got[0].Path)
}

func TestHarvestAll_DropsNonDirCandidate(t *testing.T) {
	// A candidate that exists but is a regular FILE (not a directory) must
	// also be dropped — wiring a watcher onto it would be meaningless.
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	src := &fakeSource{name: "codex", dirs: []adapter.ProjectPresence{
		{Path: dir, LastActive: time.Unix(300, 0)},
		{Path: file, LastActive: time.Unix(200, 0)},
	}}

	got, err := HarvestAll([]HarvestSource{src}, filepath.Join(t.TempDir(), ".aplexica"))
	require.NoError(t, err)
	require.Len(t, got, 1, "a candidate that is a file, not a directory, must be dropped")
	require.Equal(t, dir, got[0].Path)
}

// TestHarvest_CapsSurfacedCandidates verifies Finding 2: the pending/discovered
// producer (Harvest) caps the surfaced set at MaxSurfacedCandidates, sorted
// newest-first, and reports hasMore=true when it truncates. A user with
// hundreds of historical working dirs must not get an unbounded Pending list.
// HarvestAll stays UNCAPPED (AgentSuggestions needs the full set).
func TestHarvest_CapsSurfacedCandidates(t *testing.T) {
	const extra = 10
	n := MaxSurfacedCandidates + extra

	// Materialise n real dirs (HarvestAll stat-gates candidates) and hand each
	// a distinct LastActive so newest-first ordering is unambiguous. dir[i] gets
	// LastActive = base+i, so the highest indices are the newest.
	base := t.TempDir()
	dirs := make([]adapter.ProjectPresence, n)
	wantNewest := make([]string, 0, MaxSurfacedCandidates)
	for i := 0; i < n; i++ {
		abs := filepath.Join(base, "p", fmt.Sprintf("d%04d", i))
		require.NoError(t, os.MkdirAll(abs, 0o755))
		dirs[i] = adapter.ProjectPresence{Path: abs, LastActive: time.Unix(int64(1000+i), 0)}
	}
	// Expected: the MaxSurfacedCandidates newest, i.e. indices n-1 .. n-cap,
	// in descending-time order.
	for i := n - 1; i >= n-MaxSurfacedCandidates; i-- {
		wantNewest = append(wantNewest, dirs[i].Path)
	}

	src := &fakeSource{name: "codex", dirs: dirs}
	reg := newTestRegistry(t) // nothing registered → all n are candidates
	stateDir := filepath.Join(base, ".aplexica")

	got, hasMore, err := Harvest([]HarvestSource{src}, reg, stateDir)
	require.NoError(t, err)
	require.True(t, hasMore, "more than the cap were available → hasMore must be true")
	require.Len(t, got, MaxSurfacedCandidates, "Harvest must cap at MaxSurfacedCandidates")

	gotPaths := make([]string, len(got))
	for i, df := range got {
		gotPaths[i] = df.Path
	}
	require.Equal(t, wantNewest, gotPaths,
		"the capped set must be the newest MaxSurfacedCandidates, newest-first")

	// HarvestAll stays uncapped: it must still return every candidate.
	all, err := HarvestAll([]HarvestSource{src}, stateDir)
	require.NoError(t, err)
	require.Len(t, all, n, "HarvestAll must NOT be capped")
}

// TestHarvest_UnderCap_NoHasMore confirms the boundary: exactly the cap (not
// over) does not flag hasMore and returns the full set.
func TestHarvest_UnderCap_NoHasMore(t *testing.T) {
	n := MaxSurfacedCandidates // exactly at the cap, not over
	base := t.TempDir()
	dirs := make([]adapter.ProjectPresence, n)
	for i := 0; i < n; i++ {
		abs := filepath.Join(base, "p", fmt.Sprintf("d%04d", i))
		require.NoError(t, os.MkdirAll(abs, 0o755))
		dirs[i] = adapter.ProjectPresence{Path: abs, LastActive: time.Unix(int64(1000+i), 0)}
	}
	src := &fakeSource{name: "codex", dirs: dirs}
	reg := newTestRegistry(t)

	got, hasMore, err := Harvest([]HarvestSource{src}, reg, filepath.Join(base, ".aplexica"))
	require.NoError(t, err)
	require.False(t, hasMore, "exactly at the cap must NOT flag hasMore")
	require.Len(t, got, n)
}

func TestHarvestAll_DropsFilesystemRoot(t *testing.T) {
	// The filesystem root in its NATIVE form (what a real agent session would
	// record): "/" on unix; on Windows filepath.Abs(separator) yields the
	// current drive root (e.g. "D:\"), which validCandidatePath also rejects.
	fsRoot, err := filepath.Abs(string(filepath.Separator))
	require.NoError(t, err)
	real := realDir(t, "testuser/Projects/Test123")[0]
	kilo := &fakeSource{name: "kilo", dirs: []adapter.ProjectPresence{
		{Path: fsRoot, LastActive: time.Unix(300, 0)},
		{Path: real, LastActive: time.Unix(200, 0)},
	}}

	got, err := HarvestAll([]HarvestSource{kilo}, absPath(t, "/Users/testuser/.aplexica"))
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, real, got[0].Path)
}

// TestExcludedCandidate_CaseInsensitiveRootMatch verifies Finding 3: on a
// case-insensitive filesystem (Windows/macOS) a harvested cwd that differs only
// in case from an agent's own config root MUST still be excluded. The test host
// is darwin, so runtime.GOOS=="darwin" and case-folding is active; before the
// fix, the case-sensitive == / HasPrefix in excludedCandidate let a mismatched-
// case path leak the agent's config-root subtree into Pending.
func TestExcludedCandidate_CaseInsensitiveRootMatch(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("case-folding only active on case-insensitive filesystems (windows/darwin)")
	}
	// excludeRoots arrive normalized (lower-cased here to mimic a config that
	// stored the canonical lower form); the candidate uses mixed case.
	root := normalizeRoots([]string{filepath.FromSlash("/Users/testuser/.codex")})[0]
	// Make the candidates absolute the same way normalizeRoots does for the
	// root. On Windows a FromSlash("/Users/...") path is drive-RELATIVE, so
	// without Abs the normalized root (C:\Users\...) and a raw candidate
	// (\Users\...) differ by drive and never match — a test artifact, not a
	// production case (harvested cwds are always fully qualified).
	rootIS := absPath(t, "/Users/TESTUSER/.codex")           // same dir, different case
	under := absPath(t, "/Users/TestUser/.codex/sessions/x") // under it, different case

	require.True(t, excludedCandidate(rootIS, []string{root}),
		"a mixed-case path equal to the excludeRoot must be excluded on a case-insensitive FS")
	require.True(t, excludedCandidate(under, []string{root}),
		"a mixed-case path UNDER the excludeRoot must be excluded on a case-insensitive FS")
}

// TestExcludedCandidate_DistinctPathNotExcluded guards against over-folding: a
// genuinely different directory that merely shares a prefix-looking segment is
// NOT excluded.
func TestExcludedCandidate_DistinctPathNotExcluded(t *testing.T) {
	root := normalizeRoots([]string{filepath.FromSlash("/Users/testuser/.codex")})[0]
	other := filepath.FromSlash("/Users/testuser/.codex-backup/repo") // sibling, not under
	require.False(t, excludedCandidate(other, []string{root}),
		"a sibling dir sharing a name prefix must NOT be excluded")
}

// TestHarvestAll_CaseInsensitiveStateDirSubtree verifies Finding 3 for the
// stateDir prefix check: a real candidate dir that lives under the aplexica
// state dir but differs only in case is dropped on a case-insensitive FS.
func TestHarvestAll_CaseInsensitiveStateDirSubtree(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("case-folding only active on windows/darwin")
	}
	base := t.TempDir()
	// Real candidate physically at <base>/State/sub (must exist for the stat
	// gate); the stateDir we pass is the lower-cased <base>/state. On a
	// case-insensitive FS these are the same subtree, so the candidate must be
	// dropped. A second, unrelated real dir survives to prove the harvest works.
	inState := filepath.Join(base, "State", "sub")
	require.NoError(t, os.MkdirAll(inState, 0o755))
	keep := filepath.Join(base, "Projects", "Real")
	require.NoError(t, os.MkdirAll(keep, 0o755))

	src := &fakeSource{name: "codex", dirs: []adapter.ProjectPresence{
		{Path: inState, LastActive: time.Unix(300, 0)},
		{Path: keep, LastActive: time.Unix(200, 0)},
	}}
	stateDir := filepath.Join(base, "state") // lower-case vs the real "State"

	got, err := HarvestAll([]HarvestSource{src}, stateDir)
	require.NoError(t, err)
	require.Len(t, got, 1, "a candidate under the state dir (case-differing) must be dropped")
	require.Equal(t, keep, got[0].Path)
}
