package acf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

// BranchInfo summarises one named branch of an artifact. Persisted under
// <store>/branches/<kind>/<artifact-id>.json as a list keyed by branch
// name. BRD-04 §4.3 (branch lifecycle).
type BranchInfo struct {
	Name           string    `json:"name"`
	CreatedAt      time.Time `json:"createdAt,omitempty"`
	LastEventAt    time.Time `json:"lastEventAt,omitempty"`
	Head           string    `json:"head,omitempty"`
	ForkedFrom     string    `json:"forkedFrom,omitempty"`     // source branch name
	ForkedFromHash string    `json:"forkedFromHash,omitempty"` // source event hash
	OriginAgent    string    `json:"originAgent,omitempty"`
	Rationale      string    `json:"rationale,omitempty"`
	Archived       bool      `json:"archived,omitempty"`
	ArchivedAt     time.Time `json:"archivedAt,omitempty"`
	ArchiveReason  string    `json:"archiveReason,omitempty"`
	MergedInto     string    `json:"mergedInto,omitempty"`
	MergedIntoAt   time.Time `json:"mergedIntoAt,omitempty"`
	Tags           []string  `json:"tags,omitempty"`
	EventCount     int       `json:"eventCount,omitempty"`
}

// BranchIndex is the persisted per-artifact branch directory.
type BranchIndex struct {
	ArtifactID string                 `json:"artifactId"`
	Kind       Kind                   `json:"kind"`
	Branches   map[string]*BranchInfo `json:"branches"`
	// Renames maps an event-log branch name (the immutable name carried in
	// Event.Branch) to its current user-facing display name. `branch rename`
	// records the alias here instead of rewriting event payloads — which
	// would break the hash chain — and RefreshBranchIndex re-keys
	// event-derived branches through it so renames survive index rebuilds.
	Renames map[string]string `json:"renames,omitempty"`
}

// branchNameRegex enforces FR-04.9: lower-case, hyphen-delimited, max 64
// chars. Empty strings are rejected upstream by NormalizeBranchName.
var branchNameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// NormalizeBranchName lower-cases the input, replaces whitespace and
// underscores with hyphens, strips disallowed runes, collapses repeated
// hyphens, and truncates to 64 characters. Returns the normalised name
// and an error if the result is empty or otherwise invalid per FR-04.9.
func NormalizeBranchName(in string) (string, error) {
	if in == "" {
		return "", errors.New("acf: branch name cannot be empty")
	}
	if in == MainBranch {
		return MainBranch, nil
	}
	s := strings.ToLower(in)
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case r == '-' || r == '_' || r == ' ' || r == '/' || r == '.':
			if !prevHyphen && b.Len() > 0 {
				b.WriteRune('-')
				prevHyphen = true
			}
		default:
			// drop disallowed runes
		}
	}
	out := strings.TrimRight(b.String(), "-")
	const maxBranchNameLen = 64
	if len(out) > maxBranchNameLen {
		out = strings.TrimRight(out[:maxBranchNameLen], "-")
	}
	if !branchNameRegex.MatchString(out) {
		return "", fmt.Errorf("acf: invalid branch name %q (must match %s)", in, branchNameRegex.String())
	}
	return out, nil
}

func (s *Store) branchIndexPath(k Kind, id string) string {
	return filepath.Join(s.Root, "branches", kindDir(k), id+".json")
}

// LoadBranchIndex reads the on-disk branch index for an artifact. Returns
// an empty BranchIndex (no error) if the file does not yet exist —
// callers should treat that as "main only".
func (s *Store) LoadBranchIndex(k Kind, id string) (BranchIndex, error) {
	bi := BranchIndex{ArtifactID: id, Kind: k, Branches: map[string]*BranchInfo{}}
	data, err := s.readPrivateRel(filepath.Join("branches", kindDir(k), id+".json"), 16<<20)
	if errors.Is(err, os.ErrNotExist) {
		return bi, nil
	}
	if err != nil {
		return bi, fmt.Errorf("acf: read branch index: %w", err)
	}
	if err := json.Unmarshal(data, &bi); err != nil {
		return bi, fmt.Errorf("acf: parse branch index: %w", err)
	}
	if bi.Branches == nil {
		bi.Branches = map[string]*BranchInfo{}
	}
	return bi, nil
}

// WriteBranchIndex persists the artifact's branch index atomically.
func (s *Store) WriteBranchIndex(bi BranchIndex) error {
	data, err := json.MarshalIndent(bi, "", "  ")
	if err != nil {
		return fmt.Errorf("acf: marshal branch index: %w", err)
	}
	if err := s.writePrivateRel(filepath.Join("branches", kindDir(bi.Kind), bi.ArtifactID+".json"), data); err != nil {
		return fmt.Errorf("acf: write branch index: %w", err)
	}
	return nil
}

// RefreshBranchIndex walks the artifact's event log and recomputes the
// branch index, preserving any sticky metadata (Archived, Tags, etc.)
// from the previously-persisted index.
func (s *Store) RefreshBranchIndex(k Kind, id string) (BranchIndex, error) {
	old, err := s.LoadBranchIndex(k, id)
	if err != nil {
		return old, err
	}
	events, err := s.ReadEvents(k, id)
	if err != nil {
		return old, err
	}
	// display maps an event-log branch name (Event.Branch) to its current
	// user-facing name via the persisted rename aliases, so a `branch rename`
	// survives this from-scratch rebuild instead of reverting.
	display := func(eventName string) string {
		if d, ok := old.Renames[eventName]; ok {
			return d
		}
		return eventName
	}
	out := BranchIndex{ArtifactID: id, Kind: k, Branches: map[string]*BranchInfo{}, Renames: old.Renames}
	// seed main so it always exists
	out.Branches[MainBranch] = &BranchInfo{Name: MainBranch}
	for _, e := range events {
		bn := display(normalizeBranch(e.Branch))
		info, ok := out.Branches[bn]
		if !ok {
			info = &BranchInfo{Name: bn}
			out.Branches[bn] = info
		}
		if info.CreatedAt.IsZero() {
			info.CreatedAt = e.Timestamp
		}
		info.LastEventAt = e.Timestamp
		info.Head = e.Hash
		info.EventCount++
		if e.Type == EventTypeForkOuter && info.ForkedFromHash == "" {
			info.ForkedFromHash = e.ParentHash
			fb := e.ForkSourceBranch
			if fb == "" {
				fb = MainBranch
			}
			info.ForkedFrom = fb
			info.OriginAgent = e.ForkOriginAgent
			info.Rationale = e.ForkRationale
		}
		if e.Type == EventTypeMergeOuter && e.MergeFromBranch != "" {
			src := out.Branches[e.MergeFromBranch]
			if src != nil {
				src.MergedInto = bn
				src.MergedIntoAt = e.Timestamp
			}
		}
	}
	// carry over sticky metadata
	for name, oldInfo := range old.Branches {
		cur, ok := out.Branches[name]
		if !ok {
			continue
		}
		if oldInfo.Archived {
			cur.Archived = true
			cur.ArchivedAt = oldInfo.ArchivedAt
			cur.ArchiveReason = oldInfo.ArchiveReason
		}
		if len(oldInfo.Tags) > 0 {
			cur.Tags = append([]string(nil), oldInfo.Tags...)
		}
	}
	// Skip the write (and its temp-file + fsync + rename churn) when the
	// recomputed index is byte-for-byte equivalent to the one already on
	// disk. RefreshBranchIndex runs on every read path (branch
	// list/checkout/merge/diff) and on the retention auto-archive tick, so
	// an unconditional rewrite is pure idle write churn this repo is
	// documented to be sensitive to. DeepEqual covers every field that
	// matters: the Branches map (incl. per-branch Name/Archived/Tags/
	// LastEventAt/Head/EventCount) and the Renames map. `old` is the exact
	// on-disk version loaded at the top of this function.
	if reflect.DeepEqual(out, old) {
		return out, nil
	}
	if err := s.WriteBranchIndex(out); err != nil {
		return out, err
	}
	return out, nil
}

// ListBranches returns the artifact's branches, optionally hiding
// archived ones. The result is sorted with MainBranch first then by
// CreatedAt ascending.
func (s *Store) ListBranches(k Kind, id string, includeArchived bool) ([]BranchInfo, error) {
	bi, err := s.RefreshBranchIndex(k, id)
	if err != nil {
		return nil, err
	}
	out := make([]BranchInfo, 0, len(bi.Branches))
	for _, info := range bi.Branches {
		if !includeArchived && info.Archived {
			continue
		}
		out = append(out, *info)
	}
	sort.Slice(out, func(i, j int) bool {
		switch {
		case out[i].Name == MainBranch:
			return true
		case out[j].Name == MainBranch:
			return false
		case out[i].CreatedAt.Equal(out[j].CreatedAt):
			return out[i].Name < out[j].Name
		default:
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
	})
	return out, nil
}
