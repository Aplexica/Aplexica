package acf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Event-tag storage (BRD-04 §5.8 / FR-04.17 / FR-04.18).
//
// Event tags annotate individual events in an artifact's history. They
// cannot be stored on the event itself post-write because events are
// hash-chained and immutable. Instead they live in a per-artifact
// sidecar file under <store>/eventTags/<kind>/<artifactId>.json keyed by
// event hash.
//
// Write-time event tags supplied via Event.EventTags (e.g. by an
// adapter setting `aplexica:system-note` on an inbound event) DO live in
// the event payload and ARE hashed. Read code MUST union the two
// sources — see EventTagsFor.

// ReservedEventTagPrefixes enumerates the namespaces FR-04.17 reserves
// for the system. Any user-facing CLI command MUST reject tag-write
// operations using these prefixes.
var ReservedEventTagPrefixes = []string{"aplexica:", "auto:"}

// IsReservedEventTag reports whether the given tag string begins with
// any reserved namespace. Used by CLI write paths to enforce FR-04.17.
func IsReservedEventTag(tag string) bool {
	for _, p := range ReservedEventTagPrefixes {
		if strings.HasPrefix(tag, p) {
			return true
		}
	}
	return false
}

// EventTagsFile is the on-disk shape of the per-artifact sidecar.
type EventTagsFile struct {
	ArtifactID string              `json:"artifactId"`
	Kind       Kind                `json:"kind"`
	ByHash     map[string][]string `json:"byHash"`
}

// eventTagsMu serialises concurrent sidecar updates within one process.
// File-level locking across processes is out of scope (same single-
// process assumption as AppendEvent).
var eventTagsMu sync.Mutex

func (s *Store) eventTagsPath(k Kind, id string) string {
	return filepath.Join(s.Root, "eventTags", kindDir(k), id+".json")
}

// LoadEventTagsFile reads the per-artifact sidecar. Returns an empty
// (but valid) EventTagsFile if the file does not yet exist.
func (s *Store) LoadEventTagsFile(k Kind, id string) (EventTagsFile, error) {
	out := EventTagsFile{ArtifactID: id, Kind: k, ByHash: map[string][]string{}}
	data, err := s.readPrivateRel(filepath.Join("eventTags", kindDir(k), id+".json"), 16<<20)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("acf: read event tags: %w", err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("acf: parse event tags: %w", err)
	}
	if out.ByHash == nil {
		out.ByHash = map[string][]string{}
	}
	return out, nil
}

// WriteEventTagsFile persists the sidecar atomically.
func (s *Store) WriteEventTagsFile(f EventTagsFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("acf: marshal event tags: %w", err)
	}
	if err := s.writePrivateRel(filepath.Join("eventTags", kindDir(f.Kind), f.ArtifactID+".json"), data); err != nil {
		return fmt.Errorf("acf: write event tags: %w", err)
	}
	return nil
}

// AddEventTag adds tag to the sidecar entry for eventHash. Idempotent.
// Returns the new tag set for that event after the add.
func (s *Store) AddEventTag(k Kind, artifactID, eventHash, tag string) ([]string, error) {
	eventTagsMu.Lock()
	defer eventTagsMu.Unlock()
	f, err := s.LoadEventTagsFile(k, artifactID)
	if err != nil {
		return nil, err
	}
	existing := f.ByHash[eventHash]
	for _, t := range existing {
		if t == tag {
			return append([]string(nil), existing...), nil // already present
		}
	}
	existing = append(existing, tag)
	sort.Strings(existing)
	f.ByHash[eventHash] = existing
	if err := s.WriteEventTagsFile(f); err != nil {
		return nil, err
	}
	return append([]string(nil), existing...), nil
}

// RemoveEventTag removes tag from the sidecar entry for eventHash.
// Idempotent. Returns the new tag set after the remove (nil if empty).
func (s *Store) RemoveEventTag(k Kind, artifactID, eventHash, tag string) ([]string, error) {
	eventTagsMu.Lock()
	defer eventTagsMu.Unlock()
	f, err := s.LoadEventTagsFile(k, artifactID)
	if err != nil {
		return nil, err
	}
	existing := f.ByHash[eventHash]
	out := existing[:0]
	for _, t := range existing {
		if t != tag {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		delete(f.ByHash, eventHash)
	} else {
		f.ByHash[eventHash] = out
	}
	if err := s.WriteEventTagsFile(f); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return append([]string(nil), out...), nil
}

// EventTagsFor returns the union of an event's write-time EventTags
// (from the immutable event payload) and any sidecar tags added later.
// The result is deduplicated and sorted.
func (s *Store) EventTagsFor(k Kind, artifactID, eventHash string, writeTime []string) ([]string, error) {
	f, err := s.LoadEventTagsFile(k, artifactID)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, t := range writeTime {
		seen[t] = struct{}{}
	}
	for _, t := range f.ByHash[eventHash] {
		seen[t] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

// ListAllEventTags walks every event of an artifact and returns the
// sorted union of write-time + sidecar tags across the whole log.
func (s *Store) ListAllEventTags(k Kind, artifactID string) ([]string, error) {
	events, err := s.ReadEvents(k, artifactID)
	if err != nil {
		return nil, err
	}
	f, err := s.LoadEventTagsFile(k, artifactID)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, e := range events {
		for _, t := range e.EventTags {
			seen[t] = struct{}{}
		}
	}
	for _, tags := range f.ByHash {
		for _, t := range tags {
			seen[t] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}
