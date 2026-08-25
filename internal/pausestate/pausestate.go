// Package pausestate persists the BRD-03 §4 / FR-03.8 / FR-03.11 sync
// pause state outside the canonical store. Two scopes:
//
//  1. Global: all sync activity paused.
//  2. Per-adapter: outbound writes to a specific adapter paused.
//
// Each scope has an optional `until` time after which the pause
// auto-expires; the daemon checks this on every decision and treats
// expired pauses as resumed.
//
// State lives at <state-dir>/sync-pause.json with atomicfile-backed
// writes so a power-loss mid-write can't corrupt the state.
package pausestate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/atomicfile"
)

const Filename = "sync-pause.json"

// State is the on-disk shape. JSON-serialized as-is.
type State struct {
	// Global pause: all adapters skipped on fan-out. Empty Until = no
	// expiry (paused until explicit resume).
	Global struct {
		Paused bool      `json:"paused"`
		Until  time.Time `json:"until,omitempty"`
	} `json:"global"`

	// Per-adapter pause. Adapter name → state. Adapters absent from
	// this map are considered NOT paused.
	Adapters map[string]AdapterState `json:"adapters,omitempty"`
}

// AdapterState is the per-adapter pause state.
type AdapterState struct {
	Paused bool      `json:"paused"`
	Until  time.Time `json:"until,omitempty"`
}

// Store reads + writes State atomically. Safe for concurrent use
// within a single process via the embedded mutex.
type Store struct {
	Path string
	mu   sync.Mutex
}

// DefaultPath returns the standard <stateDir>/sync-pause.json.
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
		return State{}, fmt.Errorf("pausestate: empty path")
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("pausestate: read %s: %w", s.Path, err)
	}
	var st State
	if len(b) == 0 {
		return State{}, nil
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("pausestate: parse %s: %w", s.Path, err)
	}
	return st, nil
}

func (s *Store) saveLocked(st State) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return fmt.Errorf("pausestate: mkdir %s: %w", filepath.Dir(s.Path), err)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("pausestate: marshal: %w", err)
	}
	if err := atomicfile.WriteFile(s.Path, b, 0o644); err != nil {
		return fmt.Errorf("pausestate: write %s: %w", s.Path, err)
	}
	return nil
}

// PauseGlobal sets the global pause flag. duration > 0 sets an
// auto-expiry time; duration == 0 means "until explicit resume."
func (s *Store) PauseGlobal(duration time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return err
	}
	st.Global.Paused = true
	if duration > 0 {
		st.Global.Until = time.Now().UTC().Add(duration)
	} else {
		st.Global.Until = time.Time{}
	}
	return s.saveLocked(st)
}

// ResumeGlobal clears the global pause flag. Does NOT touch per-
// adapter pauses; resume those explicitly.
func (s *Store) ResumeGlobal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return err
	}
	st.Global.Paused = false
	st.Global.Until = time.Time{}
	return s.saveLocked(st)
}

// PauseAdapter pauses outbound writes to the named adapter.
// duration > 0 = auto-expiry; duration == 0 = until explicit resume.
func (s *Store) PauseAdapter(name string, duration time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return err
	}
	if st.Adapters == nil {
		st.Adapters = map[string]AdapterState{}
	}
	as := AdapterState{Paused: true}
	if duration > 0 {
		as.Until = time.Now().UTC().Add(duration)
	}
	st.Adapters[name] = as
	return s.saveLocked(st)
}

// ResumeAdapter clears the pause flag on the named adapter.
func (s *Store) ResumeAdapter(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return err
	}
	if st.Adapters == nil {
		return nil
	}
	delete(st.Adapters, name)
	return s.saveLocked(st)
}

// IsPaused returns the effective pause state for adapterName at the
// supplied moment. Returns (true, "global") when global pause is
// active; (true, "adapter") when only this adapter is paused;
// (false, "") otherwise.
//
// Expired pauses (Until < now) are treated as NOT paused — the
// daemon SHOULD call CleanExpired periodically to clean them up
// from disk, but consumers don't need to wait for that to act on
// expiry.
func (s *Store) IsPaused(adapterName string, now time.Time) (bool, string) {
	st, err := s.Load()
	if err != nil {
		return false, "" // best-effort; default to "not paused" on read error
	}
	if st.Global.Paused {
		if st.Global.Until.IsZero() || st.Global.Until.After(now) {
			return true, "global"
		}
	}
	if as, ok := st.Adapters[adapterName]; ok && as.Paused {
		if as.Until.IsZero() || as.Until.After(now) {
			return true, "adapter"
		}
	}
	return false, ""
}

// CleanExpired removes pause entries whose Until time has passed.
// Safe to call from the daemon's periodic tick goroutine.
func (s *Store) CleanExpired(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return err
	}
	changed := false
	if st.Global.Paused && !st.Global.Until.IsZero() && !st.Global.Until.After(now) {
		st.Global.Paused = false
		st.Global.Until = time.Time{}
		changed = true
	}
	for name, as := range st.Adapters {
		if as.Paused && !as.Until.IsZero() && !as.Until.After(now) {
			delete(st.Adapters, name)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.saveLocked(st)
}
