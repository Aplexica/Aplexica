// Package syncstate persists the per-tool `syncSecrets` opt-in flag
// outside the canonical store. Per FR-02.16 the default is `false` and
// the user opts in per tool via `aplexica tool sync-secrets <id> --enable`.
//
// The BRD-02 sketch carries this flag as a field on the tool artifact
// (and on a secret-name sidecar in the secrets store). The v1 schema
// doesn't yet model that field; rather than churn the wire format we
// keep the per-artifact toggle in a small JSON sidecar at
// <state-dir>/tool-sync-secrets.json. A future schema bump can fold
// this into the artifact metadata and migrate the sidecar.
package syncstate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/atomicfile"
)

// Store is a tiny JSON-on-disk map of (artifactID → enabled-bool). It is
// NOT safe for concurrent in-process use: Set and DeleteForArtifact do a
// read-modify-write (load → mutate → save), and although atomicfile makes
// each individual file write atomic (tempfile + rename), it provides no
// mutual exclusion across the load/mutate/save sequence — two concurrent
// writers can each load the same snapshot and the second save() drops the
// first's update. Callers must serialize writes. This holds in production:
// the only writers are short-lived `aplexica tool sync-secrets` CLI
// processes (one Set per process) and the daemon as the sole long-lived
// writer. Cross-process consistency is likewise out of scope. A future
// caller that needs in-process concurrency must add a mutex around the
// load/mutate/save sequence here.
type Store struct {
	Path string
}

const Filename = "tool-sync-secrets.json"

// DefaultPath returns the conventional path under the daemon state
// directory.
func DefaultPath(stateDir string) string {
	return filepath.Join(stateDir, Filename)
}

// Get returns the current opt-in flag for an artifact. False is the
// documented default (FR-02.16) when the artifact is not in the map.
func (s *Store) Get(artifactID string) (bool, error) {
	m, err := s.load()
	if err != nil {
		return false, err
	}
	return m[artifactID], nil
}

// Set persists enabled for the artifact. Setting `false` retains the
// entry (so the user's explicit opt-out is visible vs. the implicit
// default); pass DeleteForArtifact to remove the entry entirely.
func (s *Store) Set(artifactID string, enabled bool) error {
	m, err := s.load()
	if err != nil {
		return err
	}
	if m == nil {
		m = map[string]bool{}
	}
	m[artifactID] = enabled
	return s.save(m)
}

// DeleteForArtifact removes the explicit entry for an artifact, so
// future Get() calls fall back to the FR-02.16 default of false.
// Idempotent.
func (s *Store) DeleteForArtifact(artifactID string) error {
	m, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := m[artifactID]; !ok {
		return nil
	}
	delete(m, artifactID)
	return s.save(m)
}

// All returns a copy of the underlying map. Useful for `tool list`
// rendering and for debugging via `aplexica config show`.
func (s *Store) All() (map[string]bool, error) {
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out, nil
}

func (s *Store) load() (map[string]bool, error) {
	if s.Path == "" {
		return nil, fmt.Errorf("syncstate: empty path")
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("syncstate: read %s: %w", s.Path, err)
	}
	var m map[string]bool
	if len(b) == 0 {
		return map[string]bool{}, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("syncstate: parse %s: %w", s.Path, err)
	}
	return m, nil
}

func (s *Store) save(m map[string]bool) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return fmt.Errorf("syncstate: mkdir %s: %w", filepath.Dir(s.Path), err)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("syncstate: marshal: %w", err)
	}
	if err := atomicfile.WriteFile(s.Path, b, 0o644); err != nil {
		return fmt.Errorf("syncstate: write %s: %w", s.Path, err)
	}
	return nil
}
