// Package conflicts is the file-based conflict store at
// ~/.aplexica/conflicts/<artifactId>.json (ADR-0038). Conflicts are NOT
// written to the ACF event log — they're a separate state file the
// daemon, CLI, and tray indicator all read.
package conflicts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/safepath"
)

// Head is one of the divergent candidates in a conflict. Captures enough
// metadata for a human to pick a winner without re-reading the canonical
// store.
type Head struct {
	SourceAgent    string  `json:"sourceAgent"`
	EventID        string  `json:"eventId"`
	ContentSHA256  string  `json:"contentSha256"`
	AbsTimestamp   float64 `json:"absTimestamp"`
	PayloadPreview string  `json:"payloadPreview,omitempty"` // first ~200 chars; for the compact UI list
	// FullPayload holds the head's COMPLETE event payload bytes. It exists for
	// heads whose event is NOT in the local canonical log — notably a remote
	// inbound conflict head, which is recorded but never appended to any local
	// branch (B3). Without it, resolution and side-by-side analysis (which look
	// the payload up by EventID in the local store) cannot reconstruct the
	// remote content. omitempty keeps pre-existing on-disk conflict files (which
	// never carried this field) parseable. Privacy/integrity invariant: this
	// lives ONLY in the local-only conflict sidecar (ADR-0038), never in the ACF
	// event log and never sent remote, so the hash chain and remote privacy are
	// unaffected.
	FullPayload json.RawMessage `json:"fullPayload,omitempty"`
}

// Conflict is the file-on-disk shape: one file per artifact in conflict.
type Conflict struct {
	ArtifactID string   `json:"artifactId"`
	Kind       acf.Kind `json:"kind"`
	Heads      []Head   `json:"heads"`

	// Unreadable flags an in-memory-only sentinel produced by List when a
	// conflict sidecar exists but cannot be read or parsed (a torn/corrupt
	// file). It is NEVER written to disk (json:"-"): Record marshals real
	// Conflicts, and a sentinel never round-trips through Record. Its purpose is
	// to keep len(List())-based counts (status/doctor) from silently dropping to
	// zero on a corrupt file, mirroring the propagation gate's fail-safe posture
	// (inUnresolvedConflict treats a read/parse error as "still in conflict").
	// On a sentinel, ArtifactID is the offending file's id and Heads is empty.
	Unreadable bool `json:"-"`
}

type conflictSummaryFile struct {
	ArtifactID string        `json:"artifactId"`
	Kind       acf.Kind      `json:"kind"`
	Heads      []headSummary `json:"heads"`
}

type headSummary struct {
	SourceAgent   string  `json:"sourceAgent"`
	EventID       string  `json:"eventId"`
	ContentSHA256 string  `json:"contentSha256"`
	AbsTimestamp  float64 `json:"absTimestamp"`
}

// Store is the file-backed conflicts repository.
//
// One *Store is shared in-process between the orchestrator's fan-out goroutine
// (which Records/Clears) and the web HTTP handler goroutines (which Get/List and
// auto-resolve). Each on-disk file write is individually atomic (atomicfile
// rename / os.Remove), but the mutex below serializes the store's own
// operations against one another so a Record and a Clear on the SAME artifact
// cannot interleave. The compare-and-delete ClearIf closes the remaining TOCTOU
// window in the web auto-resolve path (Get -> Analyze -> Clear) versus a
// concurrent orchestrator Record.
type Store struct {
	Root string // typically ~/.aplexica/conflicts

	mu sync.Mutex
}

func (s *Store) Init() error {
	if err := privatefs.EnsureDir(s.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return fmt.Errorf("conflicts: mkdir %s: %w", s.Root, err)
	}
	return nil
}
func (s *Store) openRoot() (*privatefs.Root, error) {
	if err := s.Init(); err != nil {
		return nil, err
	}
	return privatefs.OpenRoot(s.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
}
func conflictRel(id string) (string, error) {
	if err := safepath.ValidateStoreComponent(id); err != nil {
		return "", err
	}
	return id + ".json", nil
}

func (s *Store) path(artifactID string) string {
	return filepath.Join(s.Root, artifactID+".json")
}

func (s *Store) Record(c Conflict) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordLocked(c)
}

func (s *Store) recordLocked(c Conflict) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("conflicts: marshal: %w", err)
	}
	rel, err := conflictRel(c.ArtifactID)
	if err != nil {
		return err
	}
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	return root.WriteFile(rel, data, privatefs.FilePolicy{RejectWritableByOthers: true})
}

// ErrNotRecorded is returned (wrapped) by Get when no conflict file exists for
// the artifact, as opposed to a read or parse failure. Callers use it to tell
// "genuinely no conflict" apart from "could not read the conflict" so they can
// fail safe — e.g. withhold propagation — on a corrupt conflict file.
var ErrNotRecorded = errors.New("no conflict recorded")

func (s *Store) Get(artifactID string) (Conflict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(artifactID)
}

// getLocked is the unlocked body of Get. Callers must hold s.mu.
func (s *Store) getLocked(artifactID string) (Conflict, error) {
	rel, err := conflictRel(artifactID)
	if err != nil {
		return Conflict{}, err
	}
	root, err := s.openRoot()
	if err != nil {
		return Conflict{}, err
	}
	defer root.Close()
	f, err := root.OpenReadRegularRepair(rel)
	if err == nil {
		data, readErr := io.ReadAll(io.LimitReader(f, 64<<20))
		_ = f.Close()
		if readErr != nil {
			return Conflict{}, readErr
		}
		if len(data) >= 64<<20 {
			return Conflict{}, fmt.Errorf("conflicts: record exceeds limit")
		}
		var c Conflict
		if err := json.Unmarshal(data, &c); err != nil {
			return Conflict{}, fmt.Errorf("conflicts: parse %s: %w", artifactID, err)
		}
		return c, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return Conflict{}, fmt.Errorf("conflicts: %w for %s", ErrNotRecorded, artifactID)
	}
	if err != nil {
		return Conflict{}, fmt.Errorf("conflicts: read %s: %w", artifactID, err)
	}
	return Conflict{}, fmt.Errorf("conflicts: read %s: %w", artifactID, err)
}

func (s *Store) List() ([]Conflict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := s.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := root.ReadDir(".")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("conflicts: list %s: %w", s.Root, err)
	}
	out := make([]Conflict, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		c, err := s.getLocked(id)
		if err != nil {
			// ErrNotRecorded means the file vanished between ReadDir and Get
			// (a genuine race) — nothing to surface. Any other error is a
			// read/parse failure on a file that DOES exist (a torn/corrupt
			// sidecar). Surfacing it as an Unreadable sentinel keeps the
			// conflict counted, matching inUnresolvedConflict's fail-safe
			// posture rather than silently forgetting it.
			if errors.Is(err, ErrNotRecorded) {
				continue
			}
			out = append(out, Conflict{ArtifactID: id, Unreadable: true})
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ArtifactID < out[j].ArtifactID
	})
	return out, nil
}

// ListSummaries returns conflict rows suitable for counts and list UIs. It
// intentionally skips PayloadPreview and FullPayload so large conversation
// conflicts do not make the sidebar badge or conflicts table slow to load.
func (s *Store) ListSummaries() ([]Conflict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := s.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := root.ReadDir(".")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("conflicts: list %s: %w", s.Root, err)
	}
	out := make([]Conflict, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		c, err := s.getSummaryLocked(id)
		if err != nil {
			if errors.Is(err, ErrNotRecorded) {
				continue
			}
			out = append(out, Conflict{ArtifactID: id, Unreadable: true})
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ArtifactID < out[j].ArtifactID
	})
	return out, nil
}

func (s *Store) getSummaryLocked(artifactID string) (Conflict, error) {
	rel, err := conflictRel(artifactID)
	if err != nil {
		return Conflict{}, err
	}
	root, err := s.openRoot()
	if err != nil {
		return Conflict{}, err
	}
	defer root.Close()
	f, err := root.OpenReadRegularRepair(rel)
	if errors.Is(err, os.ErrNotExist) {
		return Conflict{}, fmt.Errorf("conflicts: %w for %s", ErrNotRecorded, artifactID)
	}
	if err != nil {
		return Conflict{}, fmt.Errorf("conflicts: read %s: %w", artifactID, err)
	}
	defer f.Close()

	var summary conflictSummaryFile
	if err := json.NewDecoder(f).Decode(&summary); err != nil {
		return Conflict{}, fmt.Errorf("conflicts: parse %s: %w", artifactID, err)
	}
	out := Conflict{
		ArtifactID: summary.ArtifactID,
		Kind:       summary.Kind,
		Heads:      make([]Head, len(summary.Heads)),
	}
	for i, h := range summary.Heads {
		out.Heads[i] = Head{
			SourceAgent:   h.SourceAgent,
			EventID:       h.EventID,
			ContentSHA256: h.ContentSHA256,
			AbsTimestamp:  h.AbsTimestamp,
		}
	}
	return out, nil
}

func (s *Store) Clear(artifactID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clearLocked(artifactID)
}

// clearLocked is the unlocked body of Clear. Callers must hold s.mu.
func (s *Store) clearLocked(artifactID string) error {
	rel, e := conflictRel(artifactID)
	if e != nil {
		return e
	}
	root, e := s.openRoot()
	if e != nil {
		return e
	}
	defer root.Close()
	err := root.RemoveRegular(rel)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("conflicts: clear %s: %w", artifactID, err)
	}
	return nil
}

// ClearIf is a compare-and-delete: it removes the conflict for want.ArtifactID
// only if the conflict currently on disk still has the same heads want carries,
// and reports whether it deleted anything. It closes the TOCTOU window in the
// web auto-resolve path, where a caller Gets a conflict, decides it is
// auto-resolvable, then Clears it: if the orchestrator Records a NEW,
// non-equivalent divergence for the same artifact in between, an unconditional
// Clear would silently drop that genuine conflict (last-op-wins). ClearIf
// re-reads under the store lock and removes only when the on-disk heads are
// unchanged, so a freshly-recorded divergence is preserved.
//
// Returns (false, nil) when no conflict is recorded or the on-disk heads no
// longer match want (nothing deleted, not an error). A read/parse failure on an
// existing file is returned as an error and nothing is deleted (fail-safe: a
// torn sidecar is never cleared on a stale snapshot).
func (s *Store) ClearIf(want Conflict) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, err := s.getLocked(want.ArtifactID)
	if err != nil {
		if errors.Is(err, ErrNotRecorded) {
			return false, nil
		}
		return false, err
	}
	if !sameHeads(current.Heads, want.Heads) {
		return false, nil
	}
	if err := s.clearLocked(want.ArtifactID); err != nil {
		return false, err
	}
	return true, nil
}

// sameHeads reports whether two head slices identify the same divergence. Heads
// are compared in order by the fields that pin a candidate's identity
// (SourceAgent, EventID, ContentSHA256); volatile/presentation fields
// (PayloadPreview, FullPayload, AbsTimestamp) are intentionally ignored so a
// re-Record that only refreshes those does not look like a different conflict.
func sameHeads(a, b []Head) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].SourceAgent != b[i].SourceAgent ||
			a[i].EventID != b[i].EventID ||
			a[i].ContentSHA256 != b[i].ContentSHA256 {
			return false
		}
	}
	return true
}
