// Package adapterstate persists per-adapter enable/disable state
// outside the canonical store. Disabled adapters are skipped wholesale
// at daemon startup — they don't Import, don't Export, don't watch.
//
// This is DIFFERENT from pause (internal/pausestate) which is timed
// outbound-only. Disable is "treat this adapter as not installed."
//
// State lives at <state-dir>/adapters.json. atomicfile-backed writes.
package adapterstate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/aplexica/aplexica/internal/atomicfile"
)

const Filename = "adapters.json"

// State is the on-disk shape.
type State struct {
	// Disabled is the set of adapter names the user has explicitly
	// disabled via `aplexica adapters disable`. Order is not
	// significant; deduplicated on write.
	Disabled []string `json:"disabled,omitempty"`
}

// Store is the persistence handle.
type Store struct {
	Path string
	mu   sync.Mutex
}

// DefaultPath returns <stateDir>/adapters.json.
func DefaultPath(stateDir string) string {
	return filepath.Join(stateDir, Filename)
}

// Load returns the persisted state. Missing file → zero State.
func (s *Store) Load() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (State, error) {
	if s.Path == "" {
		return State{}, fmt.Errorf("adapterstate: empty path")
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("adapterstate: read %s: %w", s.Path, err)
	}
	var st State
	if len(b) == 0 {
		return State{}, nil
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("adapterstate: parse %s: %w", s.Path, err)
	}
	return st, nil
}

func (s *Store) saveLocked(st State) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return fmt.Errorf("adapterstate: mkdir %s: %w", filepath.Dir(s.Path), err)
	}
	// Dedup + sort for stable on-disk shape.
	seen := map[string]struct{}{}
	dedup := st.Disabled[:0]
	for _, n := range st.Disabled {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		dedup = append(dedup, n)
	}
	sort.Strings(dedup)
	st.Disabled = dedup

	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("adapterstate: marshal: %w", err)
	}
	if err := atomicfile.WriteFile(s.Path, b, 0o644); err != nil {
		return fmt.Errorf("adapterstate: write %s: %w", s.Path, err)
	}
	return nil
}

// Disable adds name to the disabled set. Idempotent.
func (s *Store) Disable(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return err
	}
	for _, n := range st.Disabled {
		if n == name {
			return nil
		}
	}
	st.Disabled = append(st.Disabled, name)
	return s.saveLocked(st)
}

// Enable removes name from the disabled set. Idempotent.
func (s *Store) Enable(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return err
	}
	out := st.Disabled[:0]
	for _, n := range st.Disabled {
		if n != name {
			out = append(out, n)
		}
	}
	st.Disabled = out
	return s.saveLocked(st)
}

// IsDisabled reports whether the named adapter is in the disabled
// set. Missing-file path returns false (everything enabled by default).
func (s *Store) IsDisabled(name string) bool {
	st, err := s.Load()
	if err != nil {
		return false
	}
	for _, n := range st.Disabled {
		if n == name {
			return true
		}
	}
	return false
}

// DisabledSet returns a fresh string-set of disabled adapter names.
// Useful for filtering an adapter slice at daemon startup.
func (s *Store) DisabledSet() map[string]struct{} {
	st, err := s.Load()
	if err != nil {
		return nil
	}
	out := make(map[string]struct{}, len(st.Disabled))
	for _, n := range st.Disabled {
		out[n] = struct{}{}
	}
	return out
}
