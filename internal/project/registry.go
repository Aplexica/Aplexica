package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/aplexica/aplexica/internal/privatefs"
)

const registryVersion = "3"
const registryMutationLockTimeout = 30 * time.Second

// ErrLocationDisplacement marks the AddOrUpdate refusal to silently re-point a
// registration away from a still-live location (a second clone of the same
// repository). It is a deterministic, user-actionable conflict — HTTP surfaces
// must map it to a 409-class error, not an internal fault.
var ErrLocationDisplacement = errors.New("location displacement refused")

// Registry is the process-safe persisted list of projects approved on this
// device. Every load and mutation is serialized by an OS file lock. Mutations
// reload the latest disk state and publish new in-memory state only after a
// durable write.
type Registry struct {
	path string

	// opMu orders this process's reload/read/commit sequences. The OS lock
	// orders readers and writers across processes; without opMu, a slow reload
	// could read an older revision, race a local commit, and then incorrectly
	// pause the newer in-memory authority as a rollback.
	opMu      sync.Mutex
	mu        sync.RWMutex
	state     registryState
	highWater uint64
	available bool
	// initialized distinguishes the first load (which has no in-process
	// predecessor to authenticate against) from every later reload.  A zero
	// revision is also a valid in-memory state for a registry that has not yet
	// been created, so Revision alone cannot provide this distinction.
	initialized bool
}

type Entry struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	VCS       string `json:"vcs"`
	Ephemeral bool   `json:"ephemeral,omitempty"`
	// Inactive entries are retained for operator recovery but never grant
	// project authorization or participate in discovery/watch routing. Registry
	// v3 migration uses this for legacy paths that do not currently exist.
	Inactive     bool          `json:"inactive,omitempty"`
	DisplayName  string        `json:"displayName,omitempty"`
	Scope        string        `json:"scope,omitempty"`
	Agents       []string      `json:"agents,omitempty"`
	FileIdentity *FileIdentity `json:"fileIdentity,omitempty"`

	// AuthorizationGeneration changes only when this project's authority
	// changes. Work issued under an older generation is invalid after update or
	// removal even if it remains queued in another subsystem.
	AuthorizationGeneration uint64 `json:"authorizationGeneration"`
}

type Tombstone struct {
	ID                      string    `json:"id"`
	AuthorizationGeneration uint64    `json:"authorizationGeneration"`
	RemovedAt               time.Time `json:"removedAt"`
}

type registryState struct {
	Version    string      `json:"version"`
	Revision   uint64      `json:"revision"`
	Projects   []Entry     `json:"projects"`
	Tombstones []Tombstone `json:"tombstones,omitempty"`
}

func (e Entry) EffectiveScope() string {
	if e.Scope == "" {
		return "local"
	}
	return e.Scope
}

func NewRegistry(path string) (*Registry, error) {
	canonicalPath, err := canonicalRegistryPath(path)
	if err != nil {
		return nil, err
	}
	r := &Registry{path: canonicalPath}
	parent := filepath.Dir(r.path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("project: create registry root: %w", err)
	}
	root, err := privatefs.OpenRoot(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return nil, fmt.Errorf("project: protect registry root: %w", err)
	}
	defer root.Close()
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

func canonicalRegistryPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("project: empty registry path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("project: canonicalize registry path: %w", err)
	}
	abs = filepath.Clean(abs)
	base := filepath.Base(abs)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "", fmt.Errorf("project: invalid registry filename")
	}
	physicalParent, _, _, err := resolveMigrationProjectPath(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("project: canonicalize registry root: %w", err)
	}
	return filepath.Join(physicalParent, base), nil
}

func emptyRegistryState() registryState { return registryState{Version: registryVersion} }

func cloneState(s registryState) registryState {
	out := s
	out.Projects = append([]Entry(nil), s.Projects...)
	for i := range out.Projects {
		out.Projects[i].Agents = append([]string(nil), s.Projects[i].Agents...)
		if s.Projects[i].FileIdentity != nil {
			identity := *s.Projects[i].FileIdentity
			out.Projects[i].FileIdentity = &identity
		}
	}
	out.Tombstones = append([]Tombstone(nil), s.Tombstones...)
	return out
}

func canonicalProjectPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("project: empty project path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("project: canonicalize path: %w", err)
	}
	abs = filepath.Clean(abs)
	if physical, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		return filepath.Clean(physical), nil
	}
	return abs, nil
}

func normalizeEntry(e Entry) (Entry, error) {
	if !validRegistryProjectID(e.ID) {
		return Entry{}, fmt.Errorf("project: unsafe project ID")
	}
	if e.VCS == "" {
		e.VCS = "none"
	}
	if e.VCS != "git" && e.VCS != "hg" && e.VCS != "none" {
		return Entry{}, fmt.Errorf("project: unsupported VCS")
	}
	if e.Scope != "" && e.Scope != "local" && e.Scope != "global" {
		return Entry{}, fmt.Errorf("project: unsupported scope")
	}
	if hasControl(e.DisplayName) {
		return Entry{}, fmt.Errorf("project: display name contains control characters")
	}
	for _, agent := range e.Agents {
		if agent == "" || hasControl(agent) {
			return Entry{}, fmt.Errorf("project: invalid agent name")
		}
	}
	if e.Inactive {
		canonical, _, _, err := resolveMigrationProjectPath(e.Path)
		if err != nil {
			return Entry{}, err
		}
		e.Path = canonical
		e.FileIdentity = nil
		sort.Strings(e.Agents)
		e.Agents = compactStrings(e.Agents)
		return e, nil
	}
	p, inactive, identity, err := resolveMigrationProjectPath(e.Path)
	if err != nil {
		return Entry{}, err
	}
	if inactive {
		return Entry{}, fmt.Errorf("project: active project path does not exist")
	}
	e.Path = p
	e.FileIdentity = &identity
	sort.Strings(e.Agents)
	e.Agents = compactStrings(e.Agents)
	return e, nil
}

func validatePersistedV3Entry(e Entry) (Entry, error) {
	if !validRegistryProjectID(e.ID) {
		return Entry{}, fmt.Errorf("project: unsafe project ID")
	}
	if e.VCS != "git" && e.VCS != "hg" && e.VCS != "none" {
		return Entry{}, fmt.Errorf("project: unsupported persisted VCS")
	}
	if e.Scope != "" && e.Scope != "local" && e.Scope != "global" {
		return Entry{}, fmt.Errorf("project: unsupported persisted scope")
	}
	if hasControl(e.DisplayName) {
		return Entry{}, fmt.Errorf("project: persisted display name contains control characters")
	}
	if e.Path == "" || !filepath.IsAbs(e.Path) || filepath.Clean(e.Path) != e.Path {
		return Entry{}, fmt.Errorf("project: persisted path must be clean and absolute")
	}
	if e.Inactive {
		canonical, _, _, err := resolveMigrationProjectPath(e.Path)
		if err != nil || canonical != e.Path || e.FileIdentity != nil {
			return Entry{}, fmt.Errorf("project: inactive entry path/identity is not canonical")
		}
	} else {
		if e.FileIdentity == nil || e.FileIdentity.validate() != nil {
			return Entry{}, fmt.Errorf("project: active entry has invalid persisted file identity")
		}
		// A registered directory can be temporarily unavailable (for example, an
		// unmounted volume) or can be replaced at the same pathname. Keep the
		// registry structurally available in both cases so unrelated projects can
		// continue syncing. Authorization remains fail-closed per entry because
		// List, Get, and IsAuthorized all require currentEntryIdentity to match
		// before exposing or authorizing this project.
		canonical, _, _, err := resolveMigrationProjectPath(e.Path)
		if err != nil || canonical != e.Path {
			return Entry{}, fmt.Errorf("project: active entry path is not canonical")
		}
	}
	if !sort.StringsAreSorted(e.Agents) {
		return Entry{}, fmt.Errorf("project: persisted agents are not canonical")
	}
	for i, agent := range e.Agents {
		if agent == "" || hasControl(agent) || i > 0 && agent == e.Agents[i-1] {
			return Entry{}, fmt.Errorf("project: persisted agents are invalid")
		}
	}
	return e, nil
}

func compactStrings(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if s == "" || len(out) > 0 && out[len(out)-1] == s {
			continue
		}
		out = append(out, s)
	}
	return out
}

func decodeRegistry(data []byte) (registryState, bool, error) {
	if len(data) == 0 {
		return registryState{}, false, fmt.Errorf("project: empty registry")
	}
	var s registryState
	if err := decodeStrictJSON(data, &s); err != nil {
		return registryState{}, false, fmt.Errorf("project: parse registry: %w", err)
	}
	if s.Version != registryVersion {
		return registryState{}, false, fmt.Errorf("project: registry version %q requires the explicit project migrate-v3 ceremony", s.Version)
	}
	if s.Revision == 0 {
		return registryState{}, false, fmt.Errorf("project: persisted Registry v3 revision must be nonzero")
	}
	if s.Projects == nil {
		return registryState{}, false, fmt.Errorf("project: persisted Registry v3 projects must be an array")
	}
	seenID := map[string]struct{}{}
	seenPath := map[string]string{}
	seenIdentity := map[FileIdentity]string{}
	for i := range s.Projects {
		e, err := validatePersistedV3Entry(s.Projects[i])
		if err != nil {
			return registryState{}, false, err
		}
		if _, ok := seenID[e.ID]; ok {
			return registryState{}, false, fmt.Errorf("project: duplicate project ID %q", e.ID)
		}
		pathKey := registryPathKey(e.Path)
		if prior, ok := seenPath[pathKey]; ok && prior != e.ID {
			return registryState{}, false, fmt.Errorf("project: canonical path collision between %q and %q", prior, e.ID)
		}
		if e.FileIdentity != nil {
			if prior, ok := seenIdentity[*e.FileIdentity]; ok && prior != e.ID {
				return registryState{}, false, fmt.Errorf("project: physical directory identity collision between %q and %q", prior, e.ID)
			}
			seenIdentity[*e.FileIdentity] = e.ID
		}
		seenID[e.ID], seenPath[pathKey] = struct{}{}, e.ID
		if e.AuthorizationGeneration == 0 {
			return registryState{}, false, fmt.Errorf("project: Registry v3 entry has zero authorization generation")
		}
		s.Projects[i] = e
	}
	seenTombstone := map[string]struct{}{}
	for _, t := range s.Tombstones {
		_, offset := t.RemovedAt.Zone()
		if !validRegistryProjectID(t.ID) || t.AuthorizationGeneration == 0 || t.RemovedAt.IsZero() || offset != 0 {
			return registryState{}, false, fmt.Errorf("project: malformed revocation tombstone")
		}
		if _, active := seenID[t.ID]; active {
			return registryState{}, false, fmt.Errorf("project: active project also has a revocation tombstone")
		}
		if _, duplicate := seenTombstone[t.ID]; duplicate {
			return registryState{}, false, fmt.Errorf("project: duplicate revocation tombstone")
		}
		seenTombstone[t.ID] = struct{}{}
	}
	s.Version = registryVersion
	sort.Slice(s.Projects, func(i, j int) bool { return s.Projects[i].ID < s.Projects[j].ID })
	sort.Slice(s.Tombstones, func(i, j int) bool { return s.Tombstones[i].ID < s.Tombstones[j].ID })
	canonical, err := marshalRegistry(s)
	if err != nil || !bytes.Equal(data, canonical) {
		return registryState{}, false, fmt.Errorf("project: Registry v3 is not in canonical persisted encoding")
	}
	// The canonical PERSISTED encoding is "projects": []; the canonical
	// IN-MEMORY representation is nil (cloneState normalizes every copied
	// slice through append([]T(nil), ...)). Normalize here — after the
	// canonical-bytes comparison above, which must see the non-nil decode —
	// so a decoded state and a cloned baseline of the same registry are
	// reflect.DeepEqual. Without this, removing the LAST project wedges every
	// observer and writer forever: the equal-revision transition check
	// compares a nil-normalized baseline against a freshly decoded non-nil
	// empty slice, reports a phantom state change, and no mutation can ever
	// advance the revision to heal it.
	if len(s.Projects) == 0 {
		s.Projects = nil
	}
	return s, false, nil
}

func readRegistry(path string) (registryState, bool, error) {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		parent := filepath.Dir(path)
		if _, parentErr := os.Lstat(parent); parentErr == nil {
			root, rootErr := privatefs.OpenRoot(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
			if rootErr != nil {
				return registryState{}, false, fmt.Errorf("project: validate private registry root: %w", rootErr)
			}
			_ = root.Close()
		} else if !errors.Is(parentErr, os.ErrNotExist) {
			return registryState{}, false, fmt.Errorf("project: inspect registry root: %w", parentErr)
		}
		return emptyRegistryState(), false, nil
	}
	if err != nil {
		return registryState{}, false, fmt.Errorf("project: inspect registry: %w", err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return registryState{}, false, fmt.Errorf("project: registry must be a no-follow regular file")
	}
	root, err := privatefs.OpenRoot(filepath.Dir(path), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return registryState{}, false, fmt.Errorf("project: open private registry root: %w", err)
	}
	defer root.Close()
	file, err := root.OpenReadRegular(filepath.Base(path))
	if err != nil {
		return registryState{}, false, fmt.Errorf("project: open registry: %w", err)
	}
	opened, statErr := file.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return registryState{}, false, fmt.Errorf("project: registry changed while opening")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, registryMigrationMaximumBytes+1))
	afterOpened, afterStatErr := file.Stat()
	closeErr := file.Close()
	after, afterErr := os.Lstat(path)
	if readErr != nil || afterStatErr != nil || closeErr != nil {
		if readErr != nil {
			return registryState{}, false, fmt.Errorf("project: read registry: %w", readErr)
		}
		if afterStatErr != nil {
			return registryState{}, false, fmt.Errorf("project: restat registry: %w", afterStatErr)
		}
		return registryState{}, false, fmt.Errorf("project: close registry: %w", closeErr)
	}
	if len(data) > registryMigrationMaximumBytes {
		return registryState{}, false, fmt.Errorf("project: registry exceeds size limit")
	}
	if afterErr != nil || !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) ||
		!os.SameFile(opened, afterOpened) || opened.Size() != afterOpened.Size() || !opened.ModTime().Equal(afterOpened.ModTime()) ||
		before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return registryState{}, false, fmt.Errorf("project: registry changed while reading")
	}
	return decodeRegistry(data)
}

// Reload accepts only monotonic revisions. Corrupt or rollback state is never
// installed as authorization; callers keep the last state for diagnostics but
// receive an error and must pause project-scoped work.
//
// The observed delta may span MULTIPLE serialized external mutations (the OS
// file lock orders writers, not observations), so authentication uses the
// composite-transition validator, not the per-step-exact one.
func (r *Registry) Reload() error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	lock, err := acquireRegistryLock(r.path+".lock", registryMutationLockTimeout)
	if err != nil {
		r.markUnavailable()
		return err
	}
	defer lock.release()

	s, _, err := readRegistry(r.path)
	if err != nil {
		r.mu.Lock()
		r.available = false
		r.mu.Unlock()
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.Revision < r.highWater {
		r.available = false
		return fmt.Errorf("project: registry revision rollback: got %d, require >= %d", s.Revision, r.highWater)
	}
	if r.initialized {
		if err := validateExternalRegistryTransition(r.state, s); err != nil {
			r.available = false
			return fmt.Errorf("project: reject unauthenticated registry transition: %w", err)
		}
	}
	r.state = cloneState(s)
	r.highWater = s.Revision
	r.available = true
	r.initialized = true
	return nil
}

func authorizationChanged(a, b Entry) bool {
	return a.ID != b.ID || a.Path != b.Path || a.VCS != b.VCS || a.Ephemeral != b.Ephemeral || a.Inactive != b.Inactive ||
		a.Scope != b.Scope || !reflect.DeepEqual(a.Agents, b.Agents) || !reflect.DeepEqual(a.FileIdentity, b.FileIdentity)
}

// validateRegistryTransition authenticates exactly ONE registry mutation step.
// Its rules are per-step exact (new entries start at generation one, changes
// advance generations by exactly one, removals tombstone at exactly prior+1),
// so it must only ever compare states this process KNOWS are adjacent — the
// before/after pair around mutate's own mutation closure. Cross-observation
// deltas go through validateExternalRegistryTransition instead.
func validateRegistryTransition(previous, next registryState) error {
	if next.Revision <= previous.Revision {
		if next.Revision == previous.Revision && reflect.DeepEqual(previous, next) {
			return nil
		}
		return fmt.Errorf("project: registry revision did not advance exactly with state")
	}
	previousProjects := make(map[string]Entry, len(previous.Projects))
	nextProjects := make(map[string]Entry, len(next.Projects))
	previousTombstones := make(map[string]Tombstone, len(previous.Tombstones))
	nextTombstones := make(map[string]Tombstone, len(next.Tombstones))
	for _, entry := range previous.Projects {
		previousProjects[entry.ID] = entry
	}
	for _, tombstone := range previous.Tombstones {
		previousTombstones[tombstone.ID] = tombstone
	}
	for _, entry := range next.Projects {
		nextProjects[entry.ID] = entry
		if _, revoked := previousTombstones[entry.ID]; revoked {
			return fmt.Errorf("project: revoked project ID cannot be resurrected")
		}
		prior, existed := previousProjects[entry.ID]
		if !existed {
			if entry.AuthorizationGeneration != 1 {
				return fmt.Errorf("project: new project authorization generation must start at one")
			}
			continue
		}
		expected := prior.AuthorizationGeneration
		if authorizationChanged(prior, entry) {
			if expected == ^uint64(0) {
				return fmt.Errorf("project: authorization generation exhausted")
			}
			expected++
		}
		if entry.AuthorizationGeneration != expected {
			return fmt.Errorf("project: project authorization generation rollback, reuse, or jump")
		}
	}
	for _, tombstone := range next.Tombstones {
		nextTombstones[tombstone.ID] = tombstone
		if _, active := nextProjects[tombstone.ID]; active {
			return fmt.Errorf("project: active project also has a revocation tombstone")
		}
		if prior, existed := previousTombstones[tombstone.ID]; existed {
			if !reflect.DeepEqual(prior, tombstone) {
				return fmt.Errorf("project: existing revocation tombstone changed")
			}
			continue
		}
		prior, existed := previousProjects[tombstone.ID]
		if !existed || prior.AuthorizationGeneration == ^uint64(0) || tombstone.AuthorizationGeneration != prior.AuthorizationGeneration+1 {
			return fmt.Errorf("project: new revocation tombstone is not the exact next generation")
		}
	}
	for id := range previousTombstones {
		if _, retained := nextTombstones[id]; !retained {
			return fmt.Errorf("project: durable revocation tombstone disappeared")
		}
	}
	for id, prior := range previousProjects {
		if _, retained := nextProjects[id]; retained {
			continue
		}
		tombstone, removed := nextTombstones[id]
		if !removed || prior.AuthorizationGeneration == ^uint64(0) || tombstone.AuthorizationGeneration != prior.AuthorizationGeneration+1 {
			return fmt.Errorf("project: project removal lacks an exact next-generation tombstone")
		}
	}
	if reflect.DeepEqual(previous.Projects, next.Projects) && reflect.DeepEqual(previous.Tombstones, next.Tombstones) {
		return fmt.Errorf("project: registry revision advanced without a state change")
	}
	return nil
}

// validateExternalRegistryTransition authenticates a persisted registry state
// that may be the composition of MULTIPLE serialized mutations performed by
// other authorized processes (CLI, a second daemon-adjacent tool) since this
// process last observed the file. The OS file lock serializes writers but
// does not guarantee every intermediate revision is observed, so per-step
// exactness cannot be enforced here: a legitimate unobserved add followed by
// an update composes into "new entry at generation two", which the strict
// validator misclassifies as forged — and the resulting markUnavailable would
// pause ALL project authorization until a daemon restart.
//
// What IS enforced are the security-relevant invariants that survive
// composition:
//
//   - the revision is monotonic, and an unchanged revision means an unchanged
//     state (the caller has already rejected revision rollback, but the rule
//     is restated here so this validator is safe on its own);
//   - durable revocation tombstones never disappear and never roll back;
//   - a tombstoned ID is never active again (no resurrection);
//   - a surviving project's generation never decreases, and any
//     authorization-relevant change strictly increases it (no reuse of an old
//     generation for new authority — the property that invalidates stale
//     queued work);
//   - a project that disappears leaves a strictly newer-generation tombstone.
//
// Deliberately allowed, because legitimate unobserved sequences produce them:
// new entries at generation > 1 (add+update), generation jumps > 1
// (update+update), tombstones for IDs the baseline never saw (add+remove),
// and a revision advance back to a byte-identical state (display-name round
// trip; DisplayName is not authorization-relevant and does not bump the
// generation).
func validateExternalRegistryTransition(previous, next registryState) error {
	if next.Revision < previous.Revision {
		return fmt.Errorf("project: registry revision rollback: got %d, require >= %d", next.Revision, previous.Revision)
	}
	if next.Revision == previous.Revision {
		if reflect.DeepEqual(previous, next) {
			return nil
		}
		return fmt.Errorf("project: registry state changed without a revision advance")
	}
	previousProjects := make(map[string]Entry, len(previous.Projects))
	for _, entry := range previous.Projects {
		previousProjects[entry.ID] = entry
	}
	nextProjects := make(map[string]Entry, len(next.Projects))
	for _, entry := range next.Projects {
		nextProjects[entry.ID] = entry
	}
	nextTombstones := make(map[string]Tombstone, len(next.Tombstones))
	for _, tombstone := range next.Tombstones {
		nextTombstones[tombstone.ID] = tombstone
	}
	for _, prior := range previous.Tombstones {
		if _, active := nextProjects[prior.ID]; active {
			return fmt.Errorf("project: revoked project ID cannot be resurrected")
		}
		current, retained := nextTombstones[prior.ID]
		if !retained {
			return fmt.Errorf("project: durable revocation tombstone disappeared")
		}
		if current.AuthorizationGeneration < prior.AuthorizationGeneration {
			return fmt.Errorf("project: revocation tombstone generation rollback")
		}
		if current.AuthorizationGeneration == prior.AuthorizationGeneration && !reflect.DeepEqual(prior, current) {
			return fmt.Errorf("project: revocation tombstone changed without a generation advance")
		}
	}
	for _, entry := range next.Projects {
		// An ACTIVE entry at the maximum generation is permanently
		// unrevocable: Remove, Update, and RefreshAgents all fail with
		// "generation exhausted" while IsAuthorized keeps passing, so neither
		// the CLI, the portal, nor revokeProject could ever revoke it.
		// Reaching the maximum legitimately takes ~2^64 serialized writes;
		// ONE forged write can jump straight to it. Fail closed instead of
		// installing an unrevocable authority.
		if entry.AuthorizationGeneration == ^uint64(0) {
			return fmt.Errorf("project: active project at exhausted authorization generation")
		}
		prior, existed := previousProjects[entry.ID]
		if !existed {
			// New since the last observation. decodeRegistry has already
			// enforced a nonzero generation; any non-exhausted value >= 1 is
			// reachable by a legitimate add+update... sequence, so no other
			// rule applies.
			continue
		}
		if entry.AuthorizationGeneration < prior.AuthorizationGeneration {
			return fmt.Errorf("project: project authorization generation rollback")
		}
		if authorizationChanged(prior, entry) && entry.AuthorizationGeneration == prior.AuthorizationGeneration {
			return fmt.Errorf("project: project authorization changed without a generation advance")
		}
	}
	for id, prior := range previousProjects {
		if _, retained := nextProjects[id]; retained {
			continue
		}
		tombstone, removed := nextTombstones[id]
		if !removed || tombstone.AuthorizationGeneration <= prior.AuthorizationGeneration {
			return fmt.Errorf("project: project removal lacks a newer-generation revocation tombstone")
		}
	}
	return nil
}

func marshalRegistry(s registryState) ([]byte, error) {
	s.Version = registryVersion
	if s.Projects == nil {
		// The persisted form is always an array — decodeRegistry rejects
		// "projects": null — so the in-memory nil normalization (see
		// decodeRegistry) must round-trip back to [] here.
		s.Projects = []Entry{}
	}
	sort.Slice(s.Projects, func(i, j int) bool { return s.Projects[i].ID < s.Projects[j].ID })
	sort.Slice(s.Tombstones, func(i, j int) bool { return s.Tombstones[i].ID < s.Tombstones[j].ID })
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("project: marshal registry: %w", err)
	}
	return data, nil
}

// mutate is the only registry write path.
func (r *Registry) mutate(fn func(*registryState) error) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	parent := filepath.Dir(r.path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("project: create registry root: %w", err)
	}
	root, err := privatefs.OpenRoot(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return fmt.Errorf("project: protect registry root: %w", err)
	}
	defer root.Close()
	lock, err := acquireRegistryLock(r.path+".lock", registryMutationLockTimeout)
	if err != nil {
		return err
	}
	defer lock.release()

	s, _, err := readRegistry(r.path)
	if err != nil {
		r.markUnavailable()
		return err
	}
	r.mu.RLock()
	high := r.highWater
	baseline := cloneState(r.state)
	initialized := r.initialized
	r.mu.RUnlock()
	if s.Revision < high {
		r.markUnavailable()
		return fmt.Errorf("project: registry revision rollback: got %d, require >= %d", s.Revision, high)
	}
	// Other authorized processes may have performed any number of serialized
	// mutations since this process last observed the file, so the pre-mutation
	// authentication is the composite validator. Our OWN step below remains
	// per-step exact.
	if initialized {
		if err := validateExternalRegistryTransition(baseline, s); err != nil {
			r.markUnavailable()
			return fmt.Errorf("project: reject unauthenticated registry transition before mutation: %w", err)
		}
	}
	before := cloneState(s)
	if err := fn(&s); err != nil {
		return err
	}
	if reflect.DeepEqual(before, s) {
		return nil
	}
	if s.Revision == ^uint64(0) {
		return fmt.Errorf("project: registry revision exhausted")
	}
	s.Revision++
	if err := validateRegistryTransition(before, s); err != nil {
		return err
	}
	data, err := marshalRegistry(s)
	if err != nil {
		return err
	}
	if _, _, err := decodeRegistry(data); err != nil {
		return fmt.Errorf("project: refuse invalid registry mutation output: %w", err)
	}
	if err := atomicfile.WriteFile(r.path, data, 0o600); err != nil {
		r.markUnavailable()
		return fmt.Errorf("project: persist registry: %w", err)
	}
	if err := syncRegistryParent(filepath.Dir(r.path)); err != nil {
		r.markUnavailable()
		return fmt.Errorf("project: fsync registry parent: %w", err)
	}
	r.mu.Lock()
	r.state = cloneState(s)
	r.highWater = s.Revision
	r.available = true
	r.initialized = true
	r.mu.Unlock()
	return nil
}

func (r *Registry) markUnavailable() {
	r.mu.Lock()
	r.available = false
	r.mu.Unlock()
}

func (r *Registry) Revision() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.state.Revision
}

func (r *Registry) Available() bool { r.mu.RLock(); defer r.mu.RUnlock(); return r.available }

func (r *Registry) IsAuthorized(id string, generation uint64) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.available || id == "" || generation == 0 {
		return false
	}
	for _, p := range r.state.Projects {
		if p.ID == id && currentEntryIdentity(p) {
			return p.AuthorizationGeneration == generation
		}
	}
	return false
}

// List returns only active projects. Keeping inactive legacy entries out of
// this long-standing API makes every existing discovery, watcher, adapter,
// pending, and controller consumer fail closed without relying on each caller
// to remember a new check.
func (r *Registry) List() []Entry {
	return r.list(false)
}

// ListAll includes inactive recovery records for explicit diagnostic UI/CLI
// surfaces. Authorization-sensitive code must use List.
func (r *Registry) ListAll() []Entry {
	return r.list(true)
}

func (r *Registry) list(includeInactive bool) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !includeInactive && !r.available {
		return nil
	}
	out := make([]Entry, 0, len(r.state.Projects))
	for _, entry := range r.state.Projects {
		if !includeInactive && !currentEntryIdentity(entry) {
			continue
		}
		entry.Agents = append([]string(nil), entry.Agents...)
		if entry.FileIdentity != nil {
			identity := *entry.FileIdentity
			entry.FileIdentity = &identity
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Registry) Tombstones() []Tombstone {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Tombstone(nil), r.state.Tombstones...)
}

func (r *Registry) Get(id string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.available {
		return Entry{}, false
	}
	for _, p := range r.state.Projects {
		if p.ID == id && currentEntryIdentity(p) {
			p.Agents = append([]string(nil), p.Agents...)
			if p.FileIdentity != nil {
				identity := *p.FileIdentity
				p.FileIdentity = &identity
			}
			return p, true
		}
	}
	return Entry{}, false
}

func (r *Registry) Add(e Entry) error {
	e, err := normalizeEntry(e)
	if err != nil {
		return err
	}
	return r.mutate(func(s *registryState) error {
		if hasTombstone(s, e.ID) {
			return fmt.Errorf("project: %q was revoked and cannot be implicitly resurrected", e.ID)
		}
		for _, p := range s.Projects {
			if p.ID == e.ID {
				return fmt.Errorf("project: %q already registered (path: %s)", e.ID, p.Path)
			}
			if projectLocationsCollide(p, e) {
				return fmt.Errorf("project: path already registered as %q", p.ID)
			}
		}
		e.AuthorizationGeneration = 1
		s.Projects = append(s.Projects, e)
		return nil
	})
}

func (r *Registry) Update(e Entry) error {
	e, err := normalizeEntry(e)
	if err != nil {
		return err
	}
	return r.mutate(func(s *registryState) error {
		index := -1
		for i := range s.Projects {
			if s.Projects[i].ID == e.ID {
				index = i
				break
			}
		}
		if index < 0 {
			return fmt.Errorf("project: %q not registered", e.ID)
		}
		for i, other := range s.Projects {
			if i != index && projectLocationsCollide(other, e) {
				return fmt.Errorf("project: path already registered as %q", other.ID)
			}
		}
		prior := s.Projects[index]
		e.AuthorizationGeneration = prior.AuthorizationGeneration
		if authorizationChanged(prior, e) {
			if e.AuthorizationGeneration == ^uint64(0) {
				return fmt.Errorf("project: authorization generation exhausted")
			}
			e.AuthorizationGeneration++
		}
		s.Projects[index] = e
		return nil
	})
}

func (r *Registry) AddOrUpdate(e Entry) error {
	e, err := normalizeEntry(e)
	if err != nil {
		return err
	}
	return r.mutate(func(s *registryState) error {
		if hasTombstone(s, e.ID) {
			return fmt.Errorf("project: %q was revoked and cannot be implicitly resurrected", e.ID)
		}
		index := -1
		for i := range s.Projects {
			if s.Projects[i].ID == e.ID {
				index = i
				break
			}
		}
		for i, other := range s.Projects {
			if i != index && projectLocationsCollide(other, e) {
				return fmt.Errorf("project: path already registered as %q", other.ID)
			}
		}
		if index >= 0 {
			prior := s.Projects[index]
			// Two clones of one repository share one canonical ID, so an
			// AddOrUpdate from a DIFFERENT physical directory would silently
			// de-register the location recorded here. "Same location" is
			// decided by physical file identity, NOT registryPathKey: the
			// path key case-folds on darwin/windows, and on an explicitly
			// case-sensitive volume that folding would alias two distinct
			// live clones and fail this guard open. Allow the re-point only
			// when the recorded location is no longer live (the moved-repo
			// repair case); refuse while it still exists exactly as
			// registered — displacement must be an explicit "aplexica
			// project link", never a side effect of registering what looks
			// like a fresh folder. A second clone must not displace the first
			// without warning.
			sameLocation := prior.FileIdentity != nil && e.FileIdentity != nil &&
				reflect.DeepEqual(*prior.FileIdentity, *e.FileIdentity)
			if !sameLocation && currentEntryIdentity(prior) {
				return fmt.Errorf("project: %w: %q is already registered at %s, which still exists; use \"aplexica project link\" to re-point it to %s deliberately", ErrLocationDisplacement, e.ID, prior.Path, e.Path)
			}
			e.AuthorizationGeneration = prior.AuthorizationGeneration
			if authorizationChanged(prior, e) {
				if e.AuthorizationGeneration == ^uint64(0) {
					return fmt.Errorf("project: authorization generation exhausted")
				}
				e.AuthorizationGeneration++
			}
			s.Projects[index] = e
			return nil
		}
		e.AuthorizationGeneration = 1
		s.Projects = append(s.Projects, e)
		return nil
	})
}

func projectLocationsCollide(a, b Entry) bool {
	if registryPathKey(a.Path) == registryPathKey(b.Path) {
		return true
	}
	return !a.Inactive && !b.Inactive && a.FileIdentity != nil && b.FileIdentity != nil &&
		reflect.DeepEqual(*a.FileIdentity, *b.FileIdentity)
}

func hasTombstone(state *registryState, id string) bool {
	for _, tombstone := range state.Tombstones {
		if tombstone.ID == id {
			return true
		}
	}
	return false
}

func (r *Registry) RefreshAgents(path string, agents []string) error {
	if len(agents) == 0 {
		return nil
	}
	for _, agent := range agents {
		if hasControl(agent) {
			return fmt.Errorf("project: invalid agent name")
		}
	}
	canonical, err := canonicalProjectPath(path)
	if err != nil {
		return err
	}
	return r.mutate(func(s *registryState) error {
		for i := range s.Projects {
			if s.Projects[i].Inactive {
				continue
			}
			if registryPathKey(s.Projects[i].Path) != registryPathKey(canonical) {
				continue
			}
			before := s.Projects[i]
			before.Agents = append([]string(nil), before.Agents...)
			have := map[string]bool{}
			for _, a := range s.Projects[i].Agents {
				have[a] = true
			}
			for _, a := range agents {
				if a != "" && !have[a] {
					s.Projects[i].Agents = append(s.Projects[i].Agents, a)
					have[a] = true
				}
			}
			sort.Strings(s.Projects[i].Agents)
			s.Projects[i].Agents = compactStrings(s.Projects[i].Agents)
			if authorizationChanged(before, s.Projects[i]) {
				if s.Projects[i].AuthorizationGeneration == ^uint64(0) {
					return fmt.Errorf("project: authorization generation exhausted")
				}
				s.Projects[i].AuthorizationGeneration++
			}
			return nil
		}
		return nil
	})
}

// Remove writes a durable, monotonically-generated tombstone in the same
// atomic registry transaction as removal. It is intentionally retained so a
// stale registry copy or queued intent cannot resurrect the old authority.
func (r *Registry) Remove(id string) error {
	return r.mutate(func(s *registryState) error {
		idx := -1
		var generation uint64
		for i, p := range s.Projects {
			if p.ID == id {
				idx, generation = i, p.AuthorizationGeneration
				break
			}
		}
		if idx < 0 {
			return nil
		}
		if generation == ^uint64(0) {
			return fmt.Errorf("project: authorization generation exhausted")
		}
		generation++
		s.Projects = append(s.Projects[:idx], s.Projects[idx+1:]...)
		for i := range s.Tombstones {
			if s.Tombstones[i].ID == id {
				if s.Tombstones[i].AuthorizationGeneration < generation {
					s.Tombstones[i].AuthorizationGeneration = generation
					s.Tombstones[i].RemovedAt = time.Now().UTC()
				}
				return nil
			}
		}
		s.Tombstones = append(s.Tombstones, Tombstone{ID: id, AuthorizationGeneration: generation, RemovedAt: time.Now().UTC()})
		return nil
	})
}

func (r *Registry) FindByPath(p string) (Entry, bool) {
	canonical, err := canonicalProjectPath(p)
	if err != nil {
		return Entry{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.available {
		return Entry{}, false
	}
	for _, e := range r.state.Projects {
		if registryPathKey(e.Path) == registryPathKey(canonical) && currentEntryIdentity(e) {
			e.Agents = append([]string(nil), e.Agents...)
			return e, true
		}
	}
	return Entry{}, false
}
