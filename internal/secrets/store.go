// Package secrets implements the user-local secrets store at
// ~/.aplexica/secrets/. Per ADR-0027, secret values are stored separately
// from the canonical store and are never hashed into the event chain.
package secrets

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/aplexica/aplexica/internal/privatefs"
)

const atomicSecretMaxBytes = 8 << 10

// Store is the on-disk secrets store. Default Root is ~/.aplexica/secrets but
// callers (and tests) pass any path.
type Store struct {
	Root string
}

// Init creates the secrets root with 0o700 permissions.
func (s *Store) Init() error {
	return privatefs.EnsureDir(s.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
}

// GetOrCreate atomically installs a typed, canonical secret. The final name is
// never opened writable: complete private temporary bytes are validated,
// synced, and moved into place with a native no-replace operation.
func (s *Store) GetOrCreate(artifactID, key string, generate func() (string, error), validate func(string) (string, error)) (value string, created bool, err error) {
	defer func() { s.audit("get-or-create", "per-artifact", artifactID+"/"+key, err) }()
	if err := validateArtifactID(artifactID); err != nil {
		return "", false, err
	}
	if err := validateKey(key); err != nil {
		return "", false, err
	}
	if generate == nil || validate == nil {
		return "", false, fmt.Errorf("secrets: generate and validate callbacks are required")
	}
	if err := s.Init(); err != nil {
		return "", false, err
	}
	root, err := privatefs.OpenRoot(s.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	if err != nil {
		return "", false, err
	}
	defer root.Close()
	if err := root.EnsureDir(artifactID, privatefs.DirPolicy{Access: privatefs.AccessPrivate, AllowExisting: true}); err != nil {
		return "", false, err
	}
	finalRel := filepath.Join(artifactID, key)
	readCanonical := func() (string, error) {
		// Identity material may have been created by an older release or won by
		// a concurrent creator while the Windows directory ACL was being
		// narrowed. Repair only an owned, single-link regular file through the
		// retained root before validating its canonical contents.
		f, e := root.OpenReadRegularRepair(finalRel)
		if e != nil {
			return "", e
		}
		defer f.Close()
		b, e := io.ReadAll(io.LimitReader(f, atomicSecretMaxBytes+1))
		if e != nil {
			return "", e
		}
		if len(b) > atomicSecretMaxBytes {
			return "", fmt.Errorf("secrets: stored secret exceeds limit")
		}
		canonical, e := validate(string(b))
		if e != nil {
			return "", fmt.Errorf("secrets: stored secret is invalid: %w", e)
		}
		if canonical != string(b) {
			return "", fmt.Errorf("secrets: stored secret is not canonical")
		}
		return canonical, nil
	}
	if existing, e := readCanonical(); e == nil {
		return existing, false, nil
	} else if !errors.Is(e, fs.ErrNotExist) {
		return "", false, e
	}
	generated, e := generate()
	if e != nil {
		return "", false, e
	}
	canonical, e := validate(generated)
	if e != nil {
		return "", false, fmt.Errorf("secrets: generated secret is invalid: %w", e)
	}
	if len(canonical) > atomicSecretMaxBytes {
		return "", false, fmt.Errorf("secrets: generated secret exceeds limit")
	}
	f, temp, e := root.CreateTemp(artifactID, ".secret-")
	if e != nil {
		return "", false, e
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = f.Close()
			_ = root.RemoveRegular(temp)
		}
	}()
	if _, e = f.WriteString(canonical); e == nil {
		e = f.Sync()
	}
	if closeErr := f.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		return "", false, e
	}
	// Reopen the completed temp through the retained root and apply the exact
	// same validator before it is eligible to become identity material.
	tf, e := root.OpenReadRegular(temp)
	if e != nil {
		return "", false, e
	}
	tb, e := io.ReadAll(io.LimitReader(tf, atomicSecretMaxBytes+1))
	closeErr := tf.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return "", false, e
	}
	again, e := validate(string(tb))
	if e != nil || again != canonical {
		return "", false, fmt.Errorf("secrets: staged secret validation failed")
	}
	if e = root.InstallNoReplace(temp, finalRel); e != nil {
		if !errors.Is(e, fs.ErrExist) {
			return "", false, e
		}
		winner, readErr := readCanonical()
		if readErr != nil {
			return "", false, readErr
		}
		return winner, false, nil
	}
	cleanup = false
	winner, e := readCanonical()
	if e != nil {
		return "", false, e
	}
	return winner, true, nil
}

func (s *Store) artifactDir(artifactID string) string {
	return filepath.Join(s.Root, artifactID)
}

func (s *Store) secretPath(artifactID, key string) string {
	return filepath.Join(s.artifactDir(artifactID), key)
}

// validateKey rejects key names that would let a caller escape the
// artifact's directory or collide with path separators.
func validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("secrets: empty key name")
	}
	if strings.ContainsRune(key, '\x00') {
		return fmt.Errorf("secrets: invalid key name (contains null byte)")
	}
	if strings.ContainsAny(key, "/\\") || key == "." || key == ".." {
		return fmt.Errorf("secrets: invalid key name %q (no slashes or relative refs)", key)
	}
	return nil
}

// validateArtifactID rejects IDs that would let a caller escape the secrets
// root via path traversal. Real artifact IDs are UUIDv7 strings but we don't
// enforce that — just block the obvious traversal vectors.
func validateArtifactID(id string) error {
	if id == "" {
		return fmt.Errorf("secrets: empty artifact ID")
	}
	if strings.ContainsRune(id, '\x00') {
		return fmt.Errorf("secrets: invalid artifact ID (contains null byte)")
	}
	if strings.ContainsAny(id, "/\\") || id == "." || id == ".." {
		return fmt.Errorf("secrets: invalid artifact ID %q (no slashes or relative refs)", id)
	}
	return nil
}

// Put writes a secret value with 0o600 permissions. The artifact directory is
// created lazily with 0o700. On overwrite, the file mode is explicitly
// chmod'd back to 0o600 because os.WriteFile's mode arg only applies on
// initial creation.
func (s *Store) Put(artifactID, key, value string) (err error) {
	defer func() { s.audit("write", "per-artifact", artifactID+"/"+key, err) }()
	if err := validateArtifactID(artifactID); err != nil {
		return err
	}
	if err := validateKey(key); err != nil {
		return err
	}
	dir := s.artifactDir(artifactID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("secrets: mkdir %s: %w", dir, err)
	}
	path := s.secretPath(artifactID, key)
	if err := atomicfile.WriteFile(path, []byte(value), 0o600); err != nil {
		return fmt.Errorf("secrets: write %s/%s: %w", artifactID, key, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secrets: chmod %s/%s: %w", artifactID, key, err)
	}
	return nil
}

// Get reads a secret value.
func (s *Store) Get(artifactID, key string) (val string, err error) {
	defer func() { s.audit("read", "per-artifact", artifactID+"/"+key, err) }()
	if err := validateArtifactID(artifactID); err != nil {
		return "", err
	}
	if err := validateKey(key); err != nil {
		return "", err
	}
	b, err := os.ReadFile(s.secretPath(artifactID, key))
	if err != nil {
		return "", fmt.Errorf("secrets: read %s/%s: %w", artifactID, key, err)
	}
	return string(b), nil
}

// DeleteForArtifact removes the entire <artifactID> directory under the secrets
// root. Idempotent — no error if the directory doesn't exist.
func (s *Store) DeleteForArtifact(artifactID string) (err error) {
	defer func() { s.audit("delete", "per-artifact", artifactID, err) }()
	if err := validateArtifactID(artifactID); err != nil {
		return err
	}
	dir := s.artifactDir(artifactID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("secrets: delete %s: %w", artifactID, err)
	}
	return nil
}

// Delete removes a single named secret under <artifactID>. Returns
// os.ErrNotExist-wrapped error if neither the artifact dir nor the key
// exists, so callers can distinguish "already gone" from a real I/O error.
// If, after the delete, the artifact directory has no remaining secrets,
// the directory is removed too (idempotent cleanup).
func (s *Store) Delete(artifactID, key string) (err error) {
	defer func() { s.audit("delete", "per-artifact", artifactID+"/"+key, err) }()
	if err := validateArtifactID(artifactID); err != nil {
		return err
	}
	if err := validateKey(key); err != nil {
		return err
	}
	path := s.secretPath(artifactID, key)
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("secrets: delete %s/%s: %w", artifactID, key, err)
	}
	// Best-effort: prune the artifact dir if it's now empty.
	entries, err := os.ReadDir(s.artifactDir(artifactID))
	if err == nil && len(entries) == 0 {
		_ = os.Remove(s.artifactDir(artifactID))
	}
	return nil
}

// Pair is one (artifactID, key) tuple returned by ListAll.
type Pair struct {
	ArtifactID string
	Key        string
}

// ListAll walks the secrets root and returns every (artifactID, key)
// pair, in unspecified order. Skips files at the root that aren't
// artifact directories. Returns (nil, nil) if the root doesn't exist.
func (s *Store) ListAll() ([]Pair, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("secrets: list root: %w", err)
	}
	var out []Pair
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		// Defense in depth: skip anything that wouldn't pass validation,
		// so a stray directory at the root can't poison the result.
		if validateArtifactID(id) != nil {
			continue
		}
		keys, err := s.ListForArtifact(id)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			out = append(out, Pair{ArtifactID: id, Key: k})
		}
	}
	return out, nil
}

// ListForArtifact returns all key names for an artifact, in unspecified order.
func (s *Store) ListForArtifact(artifactID string) ([]string, error) {
	if err := validateArtifactID(artifactID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.artifactDir(artifactID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("secrets: list %s: %w", artifactID, err)
	}
	var keys []string
	for _, e := range entries {
		if !e.IsDir() {
			keys = append(keys, e.Name())
		}
	}
	return keys, nil
}
