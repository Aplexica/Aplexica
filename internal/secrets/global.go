package secrets

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aplexica/aplexica/internal/atomicfile"
)

// Global-name secrets surface — BRD-02 §4.4.1.
//
// Layout:
//
//	~/.aplexica/secrets/<name>            # value, mode 0o600
//	~/.aplexica/secrets/.meta/<name>.json # sidecar metadata
//
// The sidecar records createdAt / updatedAt / usedByTools / syncEnabled.
// `usedByTools` is updated by callers (typically the import path of an
// MCP config) when a tool artifact references the named secret.
// `syncEnabled` mirrors the most recent syncSecrets flag for any tool
// using the secret; the CLI surface is `aplexica secret sync-enable
// <name>` / `sync-disable <name>`.
//
// This surface coexists with the v0.64.0 per-artifact layout under the
// same Store. Per-artifact entries live under <artifact-id>/<key>;
// global entries live as top-level files plus a sibling .meta dir.

// Meta is the per-secret-name sidecar shape persisted at
// ~/.aplexica/secrets/.meta/<name>.json.
type Meta struct {
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	UsedByTools []string  `json:"usedByTools,omitempty"`
	SyncEnabled bool      `json:"syncEnabled"`
}

const metaDirName = ".meta"

// validateName uses the same rules as validateKey: no slashes, no '.'
// or '..', no nulls, non-empty. Additionally rejects ".meta" itself
// (which is the metadata directory name and must not also be a secret
// name).
func validateName(name string) error {
	if err := validateKey(name); err != nil {
		return err
	}
	if name == metaDirName {
		return fmt.Errorf("secrets: name %q is reserved", name)
	}
	return nil
}

func (s *Store) globalPath(name string) string {
	return filepath.Join(s.Root, name)
}

func (s *Store) metaPath(name string) string {
	return filepath.Join(s.Root, metaDirName, name+".json")
}

// PutGlobal writes a global-name secret value + sidecar. If the secret
// already exists, the sidecar's UpdatedAt is bumped; otherwise a fresh
// sidecar is created with CreatedAt=UpdatedAt=now. The on-disk value
// is mode 0o600; the .meta dir is 0o700.
func (s *Store) PutGlobal(name, value string) (err error) {
	defer func() { s.audit("write", "global", name, err) }()
	return s.putGlobal(name, value)
}

func (s *Store) putGlobal(name, value string) error {
	if err := validateName(name); err != nil {
		return err
	}
	// Ensure the root + meta dir exist with documented perms.
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return fmt.Errorf("secrets: mkdir %s: %w", s.Root, err)
	}
	_ = os.Chmod(s.Root, 0o700)
	metaDir := filepath.Join(s.Root, metaDirName)
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		return fmt.Errorf("secrets: mkdir %s: %w", metaDir, err)
	}
	_ = os.Chmod(metaDir, 0o700)

	path := s.globalPath(name)
	if err := atomicfile.WriteFile(path, []byte(value), 0o600); err != nil {
		return fmt.Errorf("secrets: write %s: %w", name, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secrets: chmod %s: %w", name, err)
	}

	now := time.Now().UTC()
	meta, err := s.ReadMeta(name)
	if err != nil {
		// First write — fresh sidecar.
		meta = Meta{Name: name, CreatedAt: now}
	}
	meta.UpdatedAt = now
	return s.writeMeta(name, meta)
}

// GetGlobal reads a global-name secret value.
func (s *Store) GetGlobal(name string) (val string, err error) {
	defer func() { s.audit("read", "global", name, err) }()
	if err := validateName(name); err != nil {
		return "", err
	}
	b, err := os.ReadFile(s.globalPath(name))
	if err != nil {
		return "", fmt.Errorf("secrets: read %s: %w", name, err)
	}
	return string(b), nil
}

// DeleteGlobal removes a global-name secret + its sidecar. Idempotent
// on missing files.
func (s *Store) DeleteGlobal(name string) (err error) {
	defer func() { s.audit("delete", "global", name, err) }()
	if err := validateName(name); err != nil {
		return err
	}
	if err := os.Remove(s.globalPath(name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("secrets: delete value %s: %w", name, err)
	}
	if err := os.Remove(s.metaPath(name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("secrets: delete meta %s: %w", name, err)
	}
	return nil
}

// RotateGlobal is PutGlobal with the additional precondition that the
// secret must already exist. Documented forward-only rotation.
func (s *Store) RotateGlobal(name, value string) (err error) {
	defer func() { s.audit("rotate", "global", name, err) }()
	if err := validateName(name); err != nil {
		return err
	}
	if _, err := os.Stat(s.globalPath(name)); err != nil {
		return fmt.Errorf("secrets: rotate: secret %q does not exist: %w", name, err)
	}
	return s.putGlobal(name, value) // putGlobal (not PutGlobal) avoids a duplicate audit entry
}

// ListGlobal returns every global secret name (top-level file under
// Root that isn't a directory and isn't .meta itself). Unspecified
// order; sort at the caller if a stable ordering is required.
func (s *Store) ListGlobal() ([]string, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("secrets: list global: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip dotfiles (.DS_Store on macOS, .meta sentinel, etc.) so
		// the global list stays clean.
		if len(name) > 0 && name[0] == '.' {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// ReadMeta returns the sidecar metadata for a global-name secret.
// Returns an error wrapping os.ErrNotExist when the sidecar is
// missing, so callers can distinguish "never set" from real I/O errors.
func (s *Store) ReadMeta(name string) (Meta, error) {
	if err := validateName(name); err != nil {
		return Meta{}, err
	}
	b, err := os.ReadFile(s.metaPath(name))
	if err != nil {
		return Meta{}, fmt.Errorf("secrets: read meta %s: %w", name, err)
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return Meta{}, fmt.Errorf("secrets: parse meta %s: %w", name, err)
	}
	return m, nil
}

// writeMeta atomically rewrites the sidecar.
func (s *Store) writeMeta(name string, m Meta) error {
	if err := os.MkdirAll(filepath.Join(s.Root, metaDirName), 0o700); err != nil {
		return fmt.Errorf("secrets: mkdir meta: %w", err)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("secrets: marshal meta: %w", err)
	}
	if err := atomicfile.WriteFile(s.metaPath(name), b, 0o600); err != nil {
		return fmt.Errorf("secrets: write meta %s: %w", name, err)
	}
	return nil
}

// SetSyncEnabled toggles the sidecar's syncEnabled flag. Returns an
// error when the secret doesn't exist (a sync toggle implies the
// secret is in use).
func (s *Store) SetSyncEnabled(name string, enabled bool) (err error) {
	defer func() { s.audit("sync-toggle", "global", name, err) }()
	if err := validateName(name); err != nil {
		return err
	}
	meta, err := s.ReadMeta(name)
	if err != nil {
		// Auto-create the sidecar if the value exists but the sidecar
		// is missing (this can happen for secrets that pre-date v0.72.0).
		if _, statErr := os.Stat(s.globalPath(name)); statErr != nil {
			return fmt.Errorf("secrets: sync toggle on missing secret %q: %w", name, statErr)
		}
		now := time.Now().UTC()
		meta = Meta{Name: name, CreatedAt: now, UpdatedAt: now}
	}
	meta.SyncEnabled = enabled
	meta.UpdatedAt = time.Now().UTC()
	return s.writeMeta(name, meta)
}

// AddUsedByTool records a tool artifact's reference to a secret in the
// sidecar. Idempotent — duplicates are de-duped. Used by the inbound
// adapter translation path when a tool config is imported.
func (s *Store) AddUsedByTool(name, artifactID string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if err := validateArtifactID(artifactID); err != nil {
		return err
	}
	meta, err := s.ReadMeta(name)
	if err != nil {
		now := time.Now().UTC()
		meta = Meta{Name: name, CreatedAt: now, UpdatedAt: now}
	}
	for _, id := range meta.UsedByTools {
		if id == artifactID {
			return nil // already recorded; no-op
		}
	}
	meta.UsedByTools = append(meta.UsedByTools, artifactID)
	sort.Strings(meta.UsedByTools)
	meta.UpdatedAt = time.Now().UTC()
	return s.writeMeta(name, meta)
}

// RemoveUsedByTool removes a tool artifact's reference. Idempotent.
func (s *Store) RemoveUsedByTool(name, artifactID string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if err := validateArtifactID(artifactID); err != nil {
		return err
	}
	meta, err := s.ReadMeta(name)
	if err != nil {
		return nil // no sidecar → no entry to remove
	}
	filtered := meta.UsedByTools[:0]
	removed := false
	for _, id := range meta.UsedByTools {
		if id == artifactID {
			removed = true
			continue
		}
		filtered = append(filtered, id)
	}
	if !removed {
		return nil
	}
	meta.UsedByTools = filtered
	meta.UpdatedAt = time.Now().UTC()
	return s.writeMeta(name, meta)
}

// UnlinkToolSecret removes a tool artifact's usedByTools reference and, when
// that leaves the sidecar carrying no information of its own — no remaining
// references, not sync-enabled, and no backing global value file — deletes the
// sidecar entirely. This is the rollback for a usedByTools entry added by a tool
// import whose secret lives only in the per-artifact layout (so its sidecar was
// created purely as a side effect of AddUsedByTool); without it a failed/rolled
// back import would leave an orphan .meta/<name>.json behind. A sidecar that
// still has other references, is sync-enabled, or backs a real global secret is
// preserved. Idempotent.
func (s *Store) UnlinkToolSecret(name, artifactID string) error {
	if err := s.RemoveUsedByTool(name, artifactID); err != nil {
		return err
	}
	meta, err := s.ReadMeta(name)
	if err != nil {
		return nil // sidecar already gone
	}
	if len(meta.UsedByTools) > 0 || meta.SyncEnabled {
		return nil // still carries information
	}
	if _, err := os.Stat(s.globalPath(name)); err == nil {
		return nil // backs a real global value
	}
	if err := os.Remove(s.metaPath(name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("secrets: prune sidecar %s: %w", name, err)
	}
	// Best-effort: drop the .meta dir if this was its last sidecar, so a rolled
	// back import leaves no trace under the secrets root.
	metaDir := filepath.Join(s.Root, metaDirName)
	if entries, derr := os.ReadDir(metaDir); derr == nil && len(entries) == 0 {
		_ = os.Remove(metaDir)
	}
	return nil
}
