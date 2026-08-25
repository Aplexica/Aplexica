package projectdiscovery

import (
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/watcher"
)

// caseFoldPath normalizes a path for case-insensitive comparison on
// filesystems that are case-insensitive by default (Windows, macOS). On those
// platforms a harvested cwd "C:\Users\TestUser\.codex\..." and an excludeRoot
// "C:\Users\testuser\.codex" refer to the same location but differ only in case;
// comparing them with a case-sensitive == / HasPrefix would leak the agent's
// own config-root subtree into Pending. Elsewhere (Linux, where the FS is
// case-sensitive) it is the identity so behavior is unchanged. Callers must
// fold BOTH comparands.
func caseFoldPath(p string) string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(p)
	}
	return p
}

// HarvestSource is the narrow capability Harvest needs from an agent
// adapter: its name and the dirs it has run in. Adapters that implement
// adapter.ProjectDirSource satisfy this (they also have Name()).
type HarvestSource interface {
	Name() string
	ProjectDirs() ([]adapter.ProjectPresence, error)
}

// DiscoveredFolder is a candidate project directory harvested from one
// or more agents' session metadata.
type DiscoveredFolder struct {
	Path       string
	Agents     []string
	LastActive int64 // unix seconds (max across agents)
	IsGitRepo  bool
}

// HarvestAll unions ProjectDirs() across sources, drops the aplexica state dir
// subtree, drops agent-owned and vendor directories (see excludedCandidate),
// resolves git-ness, and returns candidates sorted by LastActive (newest
// first). Unlike Harvest it does NOT drop already-registered folders —
// agent-join suggestions need to see folders that are already projects.
//
// excludeRoots are agents' own native config/data roots (e.g. ~/.config/kilo,
// ~/.codex, ~/.claude). They are watched as native roots (FR-03.3 §4); an
// agent's own config directory is never a user project, so any harvested cwd
// that IS or lives UNDER one is dropped. Callers that don't have the roots
// handy may pass none (the pre-exclusion behavior).
func HarvestAll(sources []HarvestSource, stateDir string, excludeRoots ...string) ([]DiscoveredFolder, error) {
	policy := PathPolicy{StateDir: stateDir, ExcludeRoots: excludeRoots}
	type acc struct {
		agents map[string]struct{}
		last   int64
		path   string
	}
	folders := map[string]*acc{}
	for _, src := range sources {
		dirs, err := src.ProjectDirs()
		if err != nil {
			continue
		}
		for _, d := range dirs {
			candidate, err := policy.ResolveCandidate(d.Path)
			if err != nil {
				continue
			}
			abs, err := filepath.Abs(d.Path)
			if err != nil {
				continue
			}
			canonical := candidate.CanonicalPath
			// Drop candidates that no longer exist on disk (design spec §1.2:
			// "Drop paths that no longer exist on disk"). validCandidatePath is
			// purely lexical and project.Detect returns VCS="none" for a missing
			// path rather than erroring, so without this stat a directory an
			// agent ran in once but that was since deleted/renamed (a /tmp
			// scratch dir, an old clone, a moved repo) would resurface in the
			// pending list forever and could be approved onto a dead path. The
			// already-registered "folder deleted AFTER registration" case (§7)
			// is handled separately and unaffected.
			a := folders[canonical]
			if a == nil {
				a = &acc{agents: map[string]struct{}{}, path: abs}
				folders[canonical] = a
			}
			a.agents[src.Name()] = struct{}{}
			if t := d.LastActive.Unix(); t > a.last {
				a.last = t
			}
		}
	}

	stateAbs, _ := filepath.Abs(stateDir)
	stateFold := caseFoldPath(stateAbs)
	out := make([]DiscoveredFolder, 0, len(folders))
	for canonical, a := range folders {
		path := a.path
		// Fold both sides so the state-dir subtree is dropped even when the
		// recorded cwd differs only in case (case-insensitive FS — see
		// caseFoldPath / excludedCandidate).
		if stateAbs != "" && strings.HasPrefix(caseFoldPath(path)+string(filepath.Separator), stateFold+string(filepath.Separator)) {
			continue
		}
		agents := make([]string, 0, len(a.agents))
		for n := range a.agents {
			agents = append(agents, n)
		}
		sort.Strings(agents)
		isGit := false
		if info, err := project.Detect(canonical); err == nil && info.VCS != "none" {
			isGit = true
		}
		out = append(out, DiscoveredFolder{Path: path, Agents: agents, LastActive: a.last, IsGitRepo: isGit})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActive > out[j].LastActive })
	return out, nil
}

func validCandidatePath(path string) bool {
	if path == "" {
		return false
	}
	clean := filepath.Clean(path)
	if clean == "." {
		return false
	}
	volume := filepath.VolumeName(clean)
	rest := strings.TrimPrefix(clean, volume)
	return rest != "" && rest != string(filepath.Separator)
}

// normalizeRoots resolves each root to a cleaned absolute path, dropping empties.
func normalizeRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == "" {
			continue
		}
		abs, err := filepath.Abs(r)
		if err != nil {
			abs = r
		}
		out = append(out, filepath.Clean(abs))
	}
	return out
}

// excludedCandidate reports whether a harvested directory must NOT be offered
// as a project candidate. Two cases:
//
//   - it IS, or lives UNDER, an agent's own native config/data root (passed in
//     excludeRoots — e.g. ~/.config/kilo, ~/.codex, ~/.claude). Those roots are
//     already watched for the agent's global artifacts (FR-03.3 §4); surfacing
//     them as "projects" would (a) clutter the pending list with the user's
//     tool config dirs and (b) invite recursive watching of their contents.
//
//   - any path segment is a dependency/VCS cache (node_modules, .git, …). An
//     agent session occasionally records such a directory as its cwd; it is
//     never a meaningful project root.
//
// excludeRoots are assumed already normalized (normalizeRoots).
func excludedCandidate(abs string, excludeRoots []string) bool {
	clean := filepath.Clean(abs)
	cleanFold := caseFoldPath(clean)
	for _, root := range excludeRoots {
		rootFold := caseFoldPath(root)
		if cleanFold == rootFold || strings.HasPrefix(cleanFold, rootFold+string(filepath.Separator)) {
			return true
		}
	}
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		if watcher.SkipWalkDir(seg) {
			return true
		}
	}
	return false
}

// MaxSurfacedCandidates caps how many discovered-but-unregistered folders
// Harvest surfaces into the pending list (design spec: "sort by LastActive, cap
// surfaced candidates (e.g. 50) with a 'show all' affordance"). A user with
// hundreds of historical working dirs would otherwise get an unbounded Pending
// list. The cap applies only to the surfaced/pending producer (Harvest); the
// hasMore return reports whether more candidates exist beyond the cap so the
// SPA can render a "show all" affordance. HarvestAll stays UNCAPPED —
// AgentSuggestions needs to see every already-registered folder.
const MaxSurfacedCandidates = 50

// Harvest is HarvestAll with already-registered folders dropped — the candidate
// set for the pending/discovered list (folders not yet known to Aplexica).
// excludeRoots are forwarded to HarvestAll (see that doc).
//
// The result is already sorted newest-first (HarvestAll sorts by LastActive
// descending and this filter preserves order) and is truncated to
// MaxSurfacedCandidates. hasMore is true when the registered-filtered set
// exceeded the cap, i.e. additional candidates were dropped from the tail — a
// signal the SPA renders as a "show all" affordance.
func Harvest(sources []HarvestSource, reg *project.Registry, stateDir string, excludeRoots ...string) (folders []DiscoveredFolder, hasMore bool, err error) {
	all, err := HarvestAll(sources, stateDir, excludeRoots...)
	if err != nil {
		return nil, false, err
	}
	registered := map[string]struct{}{}
	for _, e := range reg.List() {
		if resolved, rerr := filepath.EvalSymlinks(e.Path); rerr == nil {
			if abs, aerr := filepath.Abs(resolved); aerr == nil {
				registered[caseFoldPath(abs)] = struct{}{}
			}
		}
	}
	out := make([]DiscoveredFolder, 0, len(all))
	for _, df := range all {
		resolved, rerr := filepath.EvalSymlinks(df.Path)
		if rerr != nil {
			continue
		}
		abs, aerr := filepath.Abs(resolved)
		if aerr != nil {
			abs = df.Path
		}
		if _, done := registered[caseFoldPath(abs)]; done {
			continue
		}
		out = append(out, df)
	}
	// Cap the LastActive-sorted set; report whether more existed beyond the cap.
	if len(out) > MaxSurfacedCandidates {
		out = out[:MaxSurfacedCandidates]
		hasMore = true
	}
	return out, hasMore, nil
}
