// Package pending enumerates project-scope artifacts whose canonical
// project ID has no corresponding entry in the local project registry
// — BRD-02 §4.13's "stage and wait" state, computed purely from
// canonical-store + registry data (no separate staging directory).
//
// FR-02.38 sketches a physical staging dir at
// ~/.aplexica/store/pending/<projectId>/. We implement the equivalent
// behavior without moving files: artifacts for unknown projects stay
// in the canonical store, just don't get fanned out to other adapters
// until the user links the project (registry.Add) — at which point
// the orchestrator re-fanouts them. The result is the same observable
// behavior with less disk IO and simpler reasoning.
//
// Consumers:
//   - `aplexica pending list` (CLI; v0.57.0)
//   - StatusInfo.PendingProjects + tray menu (v0.58.0)
//   - aplexica project link → re-fanout trigger (v0.58.0)
package pending

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/projectdiscovery"
)

// Project is one pending project — a canonical project ID that has
// at least one project-scope artifact in the store but no entry in
// the user's project registry on this device, OR a folder discovered
// via the harvester that is not yet registered.
type Project struct {
	// ID is the canonical project ID (e.g., "github.com/example-user/repo"
	// or "local:abc123:dirname"). Matches Artifact.Project.ID.
	ID string `json:"id"`
	// ArtifactCount is the number of project-scope artifacts in the
	// canonical store that reference this project ID. Zero for
	// Source="discovered" entries that have not yet been watched.
	ArtifactCount int `json:"artifactCount"`
	// SamplePath, when non-empty, is the source path of one of the
	// pending artifacts — a hint to the user about where the project
	// originated (e.g., "/Users/example-user/code/repo/CLAUDE.md" tells them
	// the project lived under code/repo when imported). For
	// Source="discovered" entries this is the folder path itself.
	SamplePath string `json:"samplePath,omitempty"`
	// Source distinguishes how this pending entry was found:
	//   "artifact"   — has parked artifacts in the store (classic behavior)
	//   "discovered" — harvested from agent session metadata; no artifacts yet
	Source string `json:"source"`
	// Agents lists the agent names that reported this folder as active.
	// Populated only for Source="discovered" entries.
	Agents []string `json:"agents,omitempty"`
	// LastActive is the unix-second timestamp of the most recent agent
	// activity in this folder. Populated only for Source="discovered".
	LastActive int64 `json:"lastActive,omitempty"`
	// IsGitRepo reports whether the folder is inside a git repository.
	// Populated only for Source="discovered".
	IsGitRepo bool `json:"isGitRepo,omitempty"`
	// Denied reports whether the user dismissed this discovered folder. Denied
	// rows are kept out of the active pending count and rendered in a separate
	// "denied" list the user can re-approve from. Set by ApplyDenied.
	Denied bool `json:"denied,omitempty"`
	// SuggestAgents is set on Source="agent-suggestion" rows: agents discovery
	// found active in an already-registered project (ID/SamplePath) that are not
	// yet in its agent set. The SPA offers a one-click "add" for these.
	SuggestAgents []string `json:"suggestAgents,omitempty"`
}

// SuggestionKey is the dismissed-suggestions store key for a (project, agent)
// pair — so a dismissed suggestion to add one agent doesn't suppress others.
func SuggestionKey(projectID, agent string) string {
	return projectID + "\x1f" + agent
}

// AgentSuggestions returns "agent join" suggestion rows: an agent discovery
// found active in an already-registered project's folder, but which is not in
// that project's agent set. Each row (Source="agent-suggestion") names the
// project (ID + SamplePath = folder) and the agents to add (SuggestAgents).
//
// Projects scoped to ALL agents (empty Agents) never suggest — every agent
// already participates. Dismissed (project, agent) pairs are skipped.
func AgentSuggestions(registry *project.Registry, discovered []projectdiscovery.DiscoveredFolder, dismissed *DeniedStore) []Project {
	if registry == nil {
		return nil
	}
	byID := map[string]project.Entry{}
	byPath := map[string]project.Entry{}
	for _, e := range registry.List() {
		byID[e.ID] = e
		if abs, err := filepath.Abs(e.Path); err == nil {
			byPath[abs] = e
		}
	}

	var out []Project
	for _, df := range discovered {
		abs, err := filepath.Abs(df.Path)
		if err != nil {
			abs = df.Path
		}
		id := abs
		if info, derr := project.Detect(abs); derr == nil {
			id = info.ID
		}

		e, ok := byID[id]
		if !ok {
			e, ok = byPath[abs]
		}
		if !ok || len(e.Agents) == 0 {
			continue // not registered, or already scoped to all agents
		}

		have := map[string]bool{}
		for _, a := range e.Agents {
			have[a] = true
		}
		var add []string
		for _, a := range df.Agents {
			if have[a] {
				continue
			}
			if dismissed != nil && dismissed.Has(SuggestionKey(e.ID, a)) {
				continue
			}
			add = append(add, a)
		}
		if len(add) == 0 {
			continue
		}
		sort.Strings(add)
		out = append(out, Project{
			ID:            e.ID,
			SamplePath:    e.Path,
			Source:        "agent-suggestion",
			SuggestAgents: add,
			LastActive:    df.LastActive,
		})
	}
	return out
}

// ApplyDenied reconciles a pending list against the persisted denied set: rows
// whose ID is denied are flagged Denied=true, and any denied entry discovery
// did not surface this round is appended as a synthetic denied row — so a
// folder the user dismissed keeps showing in the denied list even when it is
// momentarily inactive. The input slice is not mutated.
func ApplyDenied(list []Project, denied []DeniedEntry) []Project {
	deniedByID := make(map[string]DeniedEntry, len(denied))
	for _, d := range denied {
		deniedByID[d.ID] = d
	}
	seen := make(map[string]bool, len(list))
	out := make([]Project, len(list))
	for i, p := range list {
		seen[p.ID] = true
		if _, ok := deniedByID[p.ID]; ok {
			p.Denied = true
		}
		out[i] = p
	}
	for _, d := range denied {
		if seen[d.ID] {
			continue
		}
		out = append(out, Project{
			ID:         d.ID,
			SamplePath: d.Path,
			Source:     "discovered",
			Denied:     true,
		})
	}
	return out
}

// List walks every artifact across all four kinds in the canonical
// store and returns the set of project IDs that:
//   - have at least one ScopeProject artifact, AND
//   - have a non-nil Project field with a non-empty ID, AND
//   - are NOT registered in the user's project registry on this device.
//
// Returns the pending projects sorted by ID for deterministic output.
// Empty slice (not nil) when no pending projects exist.
func List(store *acf.Store, registry *project.Registry) ([]Project, error) {
	if store == nil {
		return nil, fmt.Errorf("pending: store is required")
	}
	type bucket struct {
		count int
		path  string
	}
	pending := map[string]*bucket{}
	registered := newRegisteredMatcher(registry)

	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		artifacts, err := store.ListArtifacts(kind)
		if err != nil {
			return nil, fmt.Errorf("pending: list %s: %w", kind, err)
		}
		for _, art := range artifacts {
			if art.Scope != acf.ScopeProject {
				continue
			}
			if art.Project == nil || art.Project.ID == "" {
				// Project-scope but no project info — shouldn't happen
				// post-v0.56.0 but skip cleanly if it does (e.g., an
				// artifact written by a pre-v0.56.0 build of the
				// daemon, before InferProject was wired).
				continue
			}
			if registered.matchesArtifact(art) {
				continue
			}
			b, ok := pending[art.Project.ID]
			if !ok {
				b = &bucket{}
				pending[art.Project.ID] = b
			}
			b.count++
			if b.path == "" {
				b.path = artifactFolderPath(art)
			}
		}
	}

	out := make([]Project, 0, len(pending))
	for id, b := range pending {
		out = append(out, Project{
			ID:            id,
			ArtifactCount: b.count,
			SamplePath:    b.path,
			Source:        "artifact",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

type registeredMatcher struct {
	ids             map[string]struct{}
	paths           map[string]struct{}
	localSlugCounts map[string]int
}

func newRegisteredMatcher(registry *project.Registry) registeredMatcher {
	m := registeredMatcher{
		ids:             map[string]struct{}{},
		paths:           map[string]struct{}{},
		localSlugCounts: map[string]int{},
	}
	if registry == nil {
		return m
	}
	for _, e := range registry.List() {
		if e.ID != "" {
			m.ids[e.ID] = struct{}{}
		}
		abs, err := filepath.Abs(e.Path)
		if err != nil {
			abs = e.Path
		}
		if abs != "" {
			m.paths[abs] = struct{}{}
			if slug, ok := localIDSlug(project.PathDerivedID(abs)); ok {
				m.localSlugCounts[slug]++
			}
		}
	}
	return m
}

func (m registeredMatcher) matchesArtifact(art acf.Artifact) bool {
	if art.Project == nil || art.Project.ID == "" {
		return false
	}
	if _, found := m.ids[art.Project.ID]; found {
		return true
	}
	if art.Project.Path != "" {
		abs, err := filepath.Abs(art.Project.Path)
		if err != nil {
			abs = art.Project.Path
		}
		if _, found := m.paths[abs]; found {
			return true
		}
		return false
	}
	if artifactFolderPath(art) != "" {
		return false
	}
	slug, ok := localIDSlug(art.Project.ID)
	return ok && m.localSlugCounts[slug] == 1
}

// localIDPartCount is the field count of a local project id:
// "local:<machine>:<slug>".
const localIDPartCount = 3

func localIDSlug(id string) (string, bool) {
	if !strings.HasPrefix(id, "local:") {
		return "", false
	}
	parts := strings.SplitN(id, ":", localIDPartCount)
	if len(parts) != localIDPartCount || parts[2] == "" {
		return "", false
	}
	return parts[2], true
}

func artifactFolderPath(art acf.Artifact) string {
	if art.Project != nil && art.Project.Path != "" {
		return art.Project.Path
	}
	if art.SourcePath == "" {
		return ""
	}
	if filepath.Ext(art.SourcePath) != "" {
		return filepath.Dir(art.SourcePath)
	}
	return art.SourcePath
}

// ListWithDiscovered returns the same set as List (artifact-backed pending
// projects, Source="artifact") plus any folders from discovered that are
// not already represented on this device.
//
// Deduplication is by canonical project ID, not file path: an artifact-
// pending entry's SamplePath is a FILE (e.g. /x/repo/CLAUDE.md) while a
// DiscoveredFolder.Path is a FOLDER (/x/repo), so a path comparison would
// never match. Instead a discovered folder is skipped when its
// project.Detect ID equals either an artifact-pending entry's ID or a
// registered project's ID. For a git repo both resolve to the same
// canonical ID, so the overlap is caught; for a non-git folder the ID
// falls back to the path and there is no matching project-pending entry
// (non-git artifacts are global, never project-pending), so nothing to
// dedupe — correct either way.
//
// A registered folder is additionally skipped by absolute path, so a
// non-git folder the user has explicitly linked (whose registry ID is a
// path-derived fallback that may not equal project.Detect's) is not
// re-surfaced as discovered.
//
// Discovered entries have Source="discovered", ArtifactCount=0, and carry
// the Agents/LastActive/IsGitRepo metadata from the harvester. The ID is
// derived via project.Detect; when detection fails the folder path is used
// as a fallback ID.
func ListWithDiscovered(store *acf.Store, registry *project.Registry, discovered []projectdiscovery.DiscoveredFolder) ([]Project, error) {
	base, err := List(store, registry)
	if err != nil {
		return nil, err
	}

	out := make([]Project, len(base))
	copy(out, base)

	// idxByID indexes the artifact-pending rows by canonical project ID. When a
	// discovered folder matches one (e.g. a folder the user just removed that
	// still has project-scoped artifacts in the store), we UPGRADE that row to a
	// discovered folder in place — real folder path + agents/activity, keeping
	// its artifact count — instead of dropping the discovered entry as a dup.
	// That hands the SPA the discovered "Approve" flow rather than the legacy
	// "Link to local path" flow.
	idxByID := map[string]int{}
	for i, p := range out {
		idxByID[p.ID] = i
	}

	// Registered projects are not pending; guard by both ID and absolute path
	// (a non-git folder's registry ID may differ from project.Detect's fallback).
	registeredID := map[string]bool{}
	registeredPath := map[string]bool{}
	if registry != nil {
		for _, e := range registry.List() {
			registeredID[e.ID] = true
			if abs, err := filepath.Abs(e.Path); err == nil {
				registeredPath[abs] = true
			}
		}
	}

	for _, df := range discovered {
		abs, err := filepath.Abs(df.Path)
		if err != nil {
			abs = df.Path
		}

		id := abs
		if info, err := project.Detect(abs); err == nil {
			id = info.ID
		}

		if registeredID[id] || registeredPath[abs] {
			continue // already a registered project — not pending
		}

		if i, ok := idxByID[id]; ok {
			// Upgrade the matching artifact-pending row (or merge a duplicate
			// discovered entry). ArtifactCount is left untouched.
			out[i].SamplePath = abs
			out[i].Source = "discovered"
			out[i].Agents = df.Agents
			out[i].LastActive = df.LastActive
			out[i].IsGitRepo = df.IsGitRepo
			continue
		}

		out = append(out, Project{
			ID:         id,
			SamplePath: abs,
			Source:     "discovered",
			Agents:     df.Agents,
			LastActive: df.LastActive,
			IsGitRepo:  df.IsGitRepo,
		})
		idxByID[id] = len(out) - 1
	}
	return out, nil
}

// Count is a thin wrapper over List that returns just the number of
// pending projects. Convenient for surfaces that only need the
// count (the daemon's StatusInfo.PendingProjects in v0.58.0).
func Count(store *acf.Store, registry *project.Registry) (int, error) {
	list, err := List(store, registry)
	if err != nil {
		return 0, err
	}
	return len(list), nil
}
