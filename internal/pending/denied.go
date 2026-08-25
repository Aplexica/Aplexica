// SPDX-License-Identifier: AGPL-3.0-or-later
package pending

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// DeniedEntry is a discovered folder the user explicitly dismissed from the
// pending list. It is persisted (not silently forgotten) so the folder keeps
// showing in the "denied" section and can be re-approved later, and so the
// discovery pass stops re-surfacing it as a fresh pending row every poll.
type DeniedEntry struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// DeniedStore is a small JSON-backed set of denied discovered folders, keyed by
// canonical project ID. It lives next to projects.json in the daemon state dir.
// All methods are safe for concurrent use.
type DeniedStore struct {
	path    string
	mu      sync.Mutex
	entries map[string]DeniedEntry
}

type deniedFile struct {
	Version string        `json:"version"`
	Denied  []DeniedEntry `json:"denied"`
}

// LoadDenied opens (or initializes) the denied store at path. A missing file
// yields an empty store; a corrupt file is treated as empty rather than fatal,
// so a bad write can never wedge the daemon's pending list.
func LoadDenied(path string) (*DeniedStore, error) {
	s := &DeniedStore{path: path, entries: map[string]DeniedEntry{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		// Return a usable empty store alongside the error so the caller can log
		// and continue rather than failing to start over a denied-list read.
		return s, err
	}
	var f deniedFile
	if json.Unmarshal(b, &f) == nil {
		for _, e := range f.Denied {
			if e.ID != "" && !isFilesystemRoot(e.Path) {
				s.entries[e.ID] = e
			}
		}
	}
	return s, nil
}

// Add records a denied folder (idempotent on ID) and persists.
func (s *DeniedStore) Add(id, path string) error {
	if id == "" {
		return nil
	}
	if isFilesystemRoot(path) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[id] = DeniedEntry{ID: id, Path: path}
	return s.saveLocked()
}

// Remove un-denies a folder (idempotent) and persists.
func (s *DeniedStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[id]; !ok {
		return nil
	}
	delete(s.entries, id)
	return s.saveLocked()
}

// Has reports whether id is currently denied.
func (s *DeniedStore) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[id]
	return ok
}

// List returns the denied entries sorted by path for stable display.
func (s *DeniedStore) List() []DeniedEntry {
	s.mu.Lock()
	out := make([]DeniedEntry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// saveLocked atomically rewrites the store file. Caller must hold s.mu.
func (s *DeniedStore) saveLocked() error {
	f := deniedFile{Version: "1", Denied: make([]DeniedEntry, 0, len(s.entries))}
	for _, e := range s.entries {
		f.Denied = append(f.Denied, e)
	}
	sort.Slice(f.Denied, func(i, j int) bool { return f.Denied[i].ID < f.Denied[j].ID })
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func isFilesystemRoot(path string) bool {
	if path == "" {
		return false
	}
	clean := filepath.Clean(path)
	if clean == "." {
		return false
	}
	volume := filepath.VolumeName(clean)
	rest := strings.TrimPrefix(clean, volume)
	return rest == "" || rest == string(filepath.Separator)
}
