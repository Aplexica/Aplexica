package acf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// OrphanRecord captures one artifact's per-agent orphan state.
// BRD-05 §5 FR-05.10: when a routing decision changes such that an
// artifact previously synced to an agent should no longer be, the
// daemon marks the artifact as "orphaned in <agent>" without deleting
// the agent's native file.
type OrphanRecord struct {
	ArtifactID string                 `json:"artifactId"`
	Kind       Kind                   `json:"kind"`
	ByAgent    map[string]OrphanEntry `json:"byAgent"`
}

// OrphanEntry is the per-agent orphan detail for a single artifact.
type OrphanEntry struct {
	DetectedAt time.Time `json:"detectedAt"`
	NativePath string    `json:"nativePath,omitempty"`
}

func (s *Store) orphansDir() string {
	return filepath.Join(s.Root, "orphans")
}

func (s *Store) orphanPath(id string) string {
	return filepath.Join(s.orphansDir(), id+".json")
}

// LoadOrphanRecord reads an orphan sidecar; returns a zero-valued
// record (no error) when absent.
func (s *Store) LoadOrphanRecord(id string) (OrphanRecord, error) {
	out := OrphanRecord{ArtifactID: id, ByAgent: map[string]OrphanEntry{}}
	data, err := s.readPrivateRel(filepath.Join("orphans", id+".json"), 16<<20)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("acf: read orphan record: %w", err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("acf: parse orphan record: %w", err)
	}
	if out.ByAgent == nil {
		out.ByAgent = map[string]OrphanEntry{}
	}
	return out, nil
}

// WriteOrphanRecord persists the sidecar.
func (s *Store) WriteOrphanRecord(r OrphanRecord) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("acf: marshal orphan record: %w", err)
	}
	return s.writePrivateRel(filepath.Join("orphans", r.ArtifactID+".json"), data)
}

// DeleteOrphanRecord removes the sidecar. Idempotent.
func (s *Store) DeleteOrphanRecord(id string) error {
	if err := s.removePrivateRel(filepath.Join("orphans", id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("acf: delete orphan record: %w", err)
	}
	return nil
}

// MarkOrphan adds (agent, entry) to the artifact's orphan record.
// Idempotent: re-marking with the same agent updates the entry.
func (s *Store) MarkOrphan(kind Kind, id, agent, nativePath string) error {
	rec, err := s.LoadOrphanRecord(id)
	if err != nil {
		return err
	}
	rec.Kind = kind
	rec.ArtifactID = id
	if rec.ByAgent == nil {
		rec.ByAgent = map[string]OrphanEntry{}
	}
	rec.ByAgent[agent] = OrphanEntry{
		DetectedAt: time.Now().UTC(),
		NativePath: nativePath,
	}
	return s.WriteOrphanRecord(rec)
}

// ClearOrphan removes the (agent, entry) mapping. Deletes the sidecar
// if no agents remain.
func (s *Store) ClearOrphan(id, agent string) error {
	rec, err := s.LoadOrphanRecord(id)
	if err != nil {
		return err
	}
	delete(rec.ByAgent, agent)
	if len(rec.ByAgent) == 0 {
		return s.DeleteOrphanRecord(id)
	}
	return s.WriteOrphanRecord(rec)
}

// ListOrphans walks every sidecar in <store>/orphans/ and returns
// every record in sorted ArtifactID order.
func (s *Store) ListOrphans() ([]OrphanRecord, error) {
	entries, err := s.readPrivateDir("orphans")
	if err != nil {
		return nil, fmt.Errorf("acf: list orphans: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		ids = append(ids, name[:len(name)-len(".json")])
	}
	sort.Strings(ids)
	out := make([]OrphanRecord, 0, len(ids))
	for _, id := range ids {
		rec, err := s.LoadOrphanRecord(id)
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, nil
}
