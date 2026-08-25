package acf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BRD-05 §4 artifact tag taxonomy + metadata.

// ReservedTagPrefixes enumerates the system-owned namespaces from
// BRD-05 §4.1. CLI write paths MUST reject these on user input.
var ReservedTagPrefixes = []string{
	"aplexica:",
	"fork-of:",
	"device:",
	"conflict:",
}

// IsReservedArtifactTag reports whether the given tag string begins
// with any reserved namespace.
func IsReservedArtifactTag(tag string) bool {
	for _, p := range ReservedTagPrefixes {
		if strings.HasPrefix(tag, p) {
			return true
		}
	}
	return false
}

// NormalizeArtifactTag validates and lower-cases a tag. Allowed runes:
// `[a-z0-9:_-]` with `:` for namespace separation. Tags MUST NOT be
// empty and MUST be <=64 characters.
func NormalizeArtifactTag(in string) (string, error) {
	if in == "" {
		return "", errors.New("acf: tag cannot be empty")
	}
	const maxTagLen = 64
	out := strings.ToLower(strings.TrimSpace(in))
	if len(out) > maxTagLen {
		return "", fmt.Errorf("acf: tag too long (max %d chars)", maxTagLen)
	}
	for _, r := range out {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') ||
			r == ':' || r == '-' || r == '_' || r == '.' || r == '/'
		if !ok {
			return "", fmt.Errorf("acf: invalid character %q in tag %q (allowed: a-z0-9:_-./)", r, in)
		}
	}
	return out, nil
}

// TagMetadata is the BRD-05 §4.3 sidecar describing one tag. Stored at
// <store>/tags/<tag-name>.json. Renaming colons → underscores for
// filesystem safety (Windows + macOS HFS).
type TagMetadata struct {
	Tag         string    `json:"tag"`
	Description string    `json:"description,omitempty"`
	Color       string    `json:"color,omitempty"` // optional hex (#aabbcc)
	Scope       string    `json:"scope,omitempty"` // e.g., "personal", "team"
	CreatedAt   time.Time `json:"createdAt,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
}

func (s *Store) tagsDir() string {
	return filepath.Join(s.Root, "tags")
}

func tagFsName(tag string) string {
	// Replace colons (problematic on Windows) with double-dashes; round-
	// trip is unambiguous because tag names cannot contain `--`.
	return strings.ReplaceAll(tag, ":", "--")
}

func (s *Store) tagMetaPath(tag string) string {
	return filepath.Join(s.tagsDir(), tagFsName(tag)+".json")
}

// LoadTagMetadata reads the metadata sidecar for tag. Returns a zero-
// valued struct (no error) when the file does not exist.
func (s *Store) LoadTagMetadata(tag string) (TagMetadata, error) {
	var out TagMetadata
	data, err := s.readPrivateRel(filepath.Join("tags", tagFsName(tag)+".json"), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return TagMetadata{Tag: tag}, nil
	}
	if err != nil {
		return out, fmt.Errorf("acf: read tag metadata: %w", err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("acf: parse tag metadata: %w", err)
	}
	return out, nil
}

// WriteTagMetadata persists metadata for tag.
func (s *Store) WriteTagMetadata(meta TagMetadata) error {
	if meta.Tag == "" {
		return errors.New("acf: WriteTagMetadata: empty tag")
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}
	meta.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("acf: marshal tag metadata: %w", err)
	}
	if err := s.writePrivateRel(filepath.Join("tags", tagFsName(meta.Tag)+".json"), data); err != nil {
		return fmt.Errorf("acf: write tag metadata: %w", err)
	}
	return nil
}

// DeleteTagMetadata removes the sidecar file. Idempotent.
func (s *Store) DeleteTagMetadata(tag string) error {
	if err := s.removePrivateRel(filepath.Join("tags", tagFsName(tag)+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("acf: delete tag metadata: %w", err)
	}
	return nil
}

// AddArtifactTag adds tag to the artifact's Tags slice (deduplicated).
// Returns the new tag list. Caller is responsible for normalization +
// reserved-namespace enforcement.
func (s *Store) AddArtifactTag(k Kind, id, tag string) ([]string, error) {
	a, err := s.ReadArtifact(k, id)
	if err != nil {
		return nil, err
	}
	for _, t := range a.Tags {
		if t == tag {
			return append([]string(nil), a.Tags...), nil
		}
	}
	a.Tags = append(a.Tags, tag)
	sort.Strings(a.Tags)
	if err := s.WriteArtifact(a); err != nil {
		return nil, err
	}
	return append([]string(nil), a.Tags...), nil
}

// RemoveArtifactTag removes tag from the artifact's Tags slice.
// Idempotent. Returns the new list (nil when emptied).
func (s *Store) RemoveArtifactTag(k Kind, id, tag string) ([]string, error) {
	a, err := s.ReadArtifact(k, id)
	if err != nil {
		return nil, err
	}
	out := a.Tags[:0]
	for _, t := range a.Tags {
		if t != tag {
			out = append(out, t)
		}
	}
	a.Tags = out
	if err := s.WriteArtifact(a); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return append([]string(nil), out...), nil
}

// RenameArtifactTag walks every artifact and rewrites oldTag → newTag.
// Returns the count of artifacts updated. Metadata sidecar is renamed
// from oldTag's path to newTag's path; oldTag's sidecar is removed.
func (s *Store) RenameArtifactTag(oldTag, newTag string) (int, error) {
	if oldTag == newTag {
		return 0, nil
	}
	count := 0
	for _, k := range []Kind{KindMemory, KindSkill, KindTool, KindConversation} {
		arts, err := s.ListArtifacts(k)
		if err != nil {
			return count, err
		}
		for _, a := range arts {
			changed := false
			for i, t := range a.Tags {
				if t == oldTag {
					a.Tags[i] = newTag
					changed = true
				}
			}
			if changed {
				// dedupe
				seen := map[string]struct{}{}
				out := a.Tags[:0]
				for _, t := range a.Tags {
					if _, ok := seen[t]; ok {
						continue
					}
					seen[t] = struct{}{}
					out = append(out, t)
				}
				a.Tags = out
				sort.Strings(a.Tags)
				if err := s.WriteArtifact(a); err != nil {
					return count, err
				}
				count++
			}
		}
	}
	if meta, err := s.LoadTagMetadata(oldTag); err == nil && meta.Tag != "" {
		meta.Tag = newTag
		_ = s.WriteTagMetadata(meta)
		_ = s.DeleteTagMetadata(oldTag)
	}
	return count, nil
}

// ListArtifactTags walks every artifact and returns the sorted union of
// every tag seen.
func (s *Store) ListArtifactTags() ([]string, error) {
	seen := map[string]struct{}{}
	for _, k := range []Kind{KindMemory, KindSkill, KindTool, KindConversation} {
		arts, err := s.ListArtifacts(k)
		if err != nil {
			return nil, err
		}
		for _, a := range arts {
			for _, t := range a.Tags {
				seen[t] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}
