package acf

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/anonymize"
	"github.com/aplexica/aplexica/internal/blobstore"
	"github.com/aplexica/aplexica/internal/securityerr"
)

// BundleVersion is the current bundle schema version. Restore refuses bundles
// whose BundleVersion has a higher major component than this.
const BundleVersion = "1.1"

// blobTarMode is the tar-header mode for bundled attachment-blob entries
// Restore routes blobs through blobstore.Put (which sets its own
// file mode), so this is cosmetic; it matches the store's 0o600 file
// convention. Declared as a named const to keep the octal literal off the
// magic-number lint surface.
const blobTarMode os.FileMode = 0o600

// BundleMeta is the JSON header stored as `meta.json` (first entry in the tarball).
type BundleMeta struct {
	BundleVersion   string         `json:"bundleVersion"`
	CreatedAt       time.Time      `json:"createdAt"`
	AplexicaVersion string         `json:"aplexicaVersion"`
	ArtifactCounts  map[string]int `json:"artifactCounts"`
	IncludesSecrets bool           `json:"includesSecrets"`
	// Anonymized is true when bundle payload bytes were passed through
	// internal/anonymize.Scrub before tar-writing (v0.31.0+). Reviewers
	// downstream use this flag to know whether PII may still be present.
	// `omitempty` keeps pre-v0.31.0 bundle meta wire-compatible.
	Anonymized bool `json:"anonymized,omitempty"`

	// Hostname is the name of the machine that produced the bundle, captured
	// via os.Hostname() at Bundle() time (FR-01.8 / FR-01.24). Empty when the
	// hostname could not be determined — the bundle is still written. Older
	// (pre-FR-01.8) bundles omit this field entirely; it decodes to "".
	Hostname string `json:"hostname,omitempty"`

	// TotalBytes is the sum of the pre-compression body lengths of every
	// non-meta archive entry the bundle ships (FR-01.8 / FR-01.24). Older
	// bundles omit it; it decodes to 0.
	TotalBytes int64 `json:"totalBytes,omitempty"`

	// Hashes maps each non-meta archive path (e.g. "acf/memories/<id>.json")
	// to the lowercase-hex sha256 of the bytes shipped under that path
	// (FR-01.8 / FR-01.24). The digest covers the post-transform body — i.e.
	// the exact bytes Restore writes — so it doubles as an integrity check.
	// Older bundles omit it; it decodes to nil.
	Hashes map[string]string `json:"hashes,omitempty"`
}

// BundleOpts controls the Bundle operation.
type BundleOpts struct {
	AplexicaVersion string // e.g. "0.1.10"; recorded in BundleMeta
	SecretsRoot     string // if non-empty, walk this dir and include its contents

	// Anonymize, when true, runs each artifact JSON + each events JSONL
	// file through anonymize.Scrub before tar-writing. Mutually exclusive
	// with SecretsRoot != "" — including secrets in an anonymized bundle
	// would re-leak the credentials the scrubber tries to redact, so v0.31.0
	// returns an error if both are set.
	Anonymize bool

	// AnonymizeHomeDir is the absolute prefix to rewrite to "~". Typically
	// os.UserHomeDir() at backup time. Empty = skip path rewriting (the
	// email + secret passes still run if Anonymize=true).
	AnonymizeHomeDir string

	// ScopeFilter (BRD-02 §4.13 / FR-01.10; v0.63.0). When non-empty,
	// only artifacts whose Scope matches this kind ("global"/"project"/
	// "namespace") are included in the bundle. Empty = no filter (all
	// scopes included).
	ScopeFilter Scope

	// ProjectFilter (BRD-02 §4.13 / FR-01.10; v0.63.0). When non-empty,
	// only artifacts whose Project.ID is in this list are included.
	// Implies project-scope (artifacts without Project.ID can never
	// match). Repeatable on the CLI as `--project <id>`.
	ProjectFilter []string

	// IncludePending (FR-01.10; v0.63.0). When true (the FR-01.10
	// default), project-scope artifacts whose Project.ID isn't in
	// any local registry — i.e., "pending" artifacts — are included.
	// When false, callers can produce a clean per-project bundle that
	// excludes staged-but-unmaterialized cross-device artifacts.
	//
	// Default in the CLI: true. Set false explicitly via
	// `--include-pending-projects=false`.
	IncludePending bool

	// PendingIDs (v0.63.0 internal). When IncludePending == false, the
	// CLI populates this with the set of project IDs currently in the
	// pending list (computed via internal/pending.List). Artifacts
	// whose Project.ID is in PendingIDs are then excluded. Empty =
	// no pending-exclusion (paired with IncludePending = true).
	PendingIDs map[string]struct{}

	// RespectSyncFlags (v0.73.0; FR-02.17 wiring). When true AND
	// SecretsRoot != "", global-name secrets are filtered against
	// their per-name sidecar's `syncEnabled` flag — only secrets
	// the user has explicitly opted in to syncing are bundled. When
	// false, every secret under SecretsRoot is bundled (the
	// pre-v0.73.0 behavior). Per FR-02.16 the BRD default is "secrets
	// stay local", so the CLI defaults RespectSyncFlags=true; library
	// callers can override.
	//
	// Per-artifact secrets (~/.aplexica/secrets/<artifact-id>/<key>)
	// don't have sidecars; they pass through unfiltered. The MCP
	// adapter pipeline writes those itself and they're not part of
	// the user-opt-in surface anyway.
	RespectSyncFlags bool
}

// Bundle writes a gzipped tar archive of the entire canonical store to w.
// The first entry is meta.json; subsequent entries mirror the store's
// acf/<kind>s/<id>.json + events/<kind>s/<id>.jsonl layout. If
// opts.SecretsRoot is non-empty, secrets/ is also included (file modes
// preserved at 0o600).
func (s *Store) Bundle(w io.Writer, opts BundleOpts) error {
	// Mutual-exclusion guard: v0.31.0 refuses to bundle secrets alongside
	// anonymization. The point of --anonymize is to ship a sanitized bundle
	// for sharing/review; including the secrets store would re-leak the
	// credentials the scrubber tries to redact. CLI surface enforces the
	// same rule with a warning + auto-disable, but library callers get an
	// explicit error.
	if opts.Anonymize && opts.SecretsRoot != "" {
		return fmt.Errorf("acf: BundleOpts.Anonymize and SecretsRoot are mutually exclusive in v0.31.0")
	}

	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	// v0.63.0 BRD-02 §4.13 + FR-01.10: build the per-(kind,id) include
	// allowlist BEFORE walking, by reading every artifact's metadata
	// and applying ScopeFilter / ProjectFilter / IncludePending. When
	// no filters are set, includeSet stays nil and walkAndAddToTar
	// includes everything (legacy v0.59.0 behavior).
	var includeSet map[string]struct{}
	if opts.hasFilters() {
		includeSet = map[string]struct{}{}
	}

	// Walk all kinds, count artifacts (post-filter when filters set).
	counts := map[string]int{}
	for _, k := range []Kind{KindMemory, KindSkill, KindTool, KindConversation} {
		artifacts, err := s.ListArtifacts(k)
		if err != nil {
			return fmt.Errorf("acf: bundle list %s: %w", k, err)
		}
		if includeSet == nil {
			counts[string(k)] = len(artifacts)
			continue
		}
		// Filter pass.
		kept := 0
		for _, a := range artifacts {
			if !opts.includeArtifact(a) {
				continue
			}
			includeSet[bundleKey(k, a.ArtifactID)] = struct{}{}
			kept++
		}
		counts[string(k)] = kept
	}

	// Per-file transformer. Nil = passthrough; otherwise applied to each
	// acf/ + events/ body (NOT secrets/, which is mutually exclusive above,
	// and NOT meta.json). Wired into walkAndAddToTar below.
	var transform BundleTransform
	if opts.Anonymize {
		homeDir := opts.AnonymizeHomeDir
		transform = func(archivePath string, body []byte) ([]byte, error) {
			scrubbed, _ := anonymize.Scrub(body, anonymize.Options{
				HomeDir:      homeDir,
				RedactEmails: true,
				ScrubSecrets: true,
			})
			return scrubbed, nil
		}
	}

	// FR-01.8 / FR-01.24: the manifest records per-artifact hashes and the
	// summed body size, which can only be known after the walk produces the
	// bodies — yet meta.json must remain the FIRST tar entry. To avoid a
	// second accounting walk that could drift from what actually ships, buffer
	// every non-meta entry into a scratch tar first, derive the manifest from
	// exactly those buffered bytes, then write meta.json followed by a verbatim
	// replay of the scratch entries.
	scratchDir, err := os.MkdirTemp("", "aplexica-bundle-plan-")
	if err != nil {
		return fmt.Errorf("acf: create private bundle plan: %w", err)
	}
	_ = os.Chmod(scratchDir, 0o700)
	defer os.RemoveAll(scratchDir)
	artifactFile, err := os.OpenFile(filepath.Join(scratchDir, "entries.tar"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("acf: create bundle plan: %w", err)
	}
	defer artifactFile.Close()
	btw := tar.NewWriter(artifactFile)

	// Walk acf/, events/, eventTags/, and branches/ trees. When
	// includeSet is non-nil, the walker only emits paths whose (kind,
	// id) is in the set.
	//
	// eventTags/ + branches/ added in v0.100.0 / v0.95.0 respectively;
	// they carry per-artifact sidecar state (event-tag annotations,
	// branch lifecycle metadata) that MUST propagate alongside the
	// event log to satisfy FR-04.18.
	for _, top := range []string{"acf", "events", "eventTags", "branches"} {
		topRoot := filepath.Join(s.Root, top)
		if err := walkAndAddToTarFiltered(btw, topRoot, top, transform, includeSet); err != nil {
			return err
		}
	}

	// Include the content-addressed attachment blobs referenced by the
	// bundled conversation artifacts. The event payloads carry only
	// each attachment's ContentHash; the raw bytes live out-of-line in the
	// blob store, so without this a restored bundle would have dangling
	// ContentHashes (blobstore.Open would fail) and the attachment bytes
	// would be lost. collectBundleBlobHashes returns exactly the hashes
	// whose LATEST assertion is non-evicted — respecting the same artifact
	// filters as the rest of the bundle (approach (b)) — so an evicted
	// attachment (its marker already in the event payload) needs no blob and
	// a missing blob for it is never an error.
	//
	// Anonymized bundles deliberately OMIT blobs: --anonymize ships a
	// sanitized copy for sharing/review, and the scrubber CANNOT sanitize
	// binary attachment bytes (image/audio/video/file), so shipping them raw
	// would re-introduce unscrubbable PII — the same reasoning behind the
	// Anonymize/SecretsRoot mutual exclusion above. An anonymized bundle is a
	// lossy export, not a restore source (scrubbing already breaks
	// VerifyChain), so dropping the bytes loses nothing a restore relied on.
	if !opts.Anonymize {
		blobHashes, err := s.collectBundleBlobHashes(includeSet)
		if err != nil {
			return err
		}
		blobs := &blobstore.Store{Root: s.BlobsDir()}
		for _, hash := range blobHashes {
			rc, oerr := blobs.Open(hash)
			if oerr != nil {
				return fmt.Errorf("acf: bundle attachment blob %s: %w", hash, oerr)
			}
			info, serr := os.Stat(blobs.Path(hash))
			if serr != nil || !info.Mode().IsRegular() {
				_ = rc.Close()
				return fmt.Errorf("acf: bundle stat attachment blob %s: %w", hash, serr)
			}
			// Route blobs through the SAME scratch tar as the walk/secrets
			// (btw, not tw) so they are emitted after meta.json on replay
			// (preserving the meta-first invariant) and are covered by the
			// manifest's per-path hashes + TotalBytes.
			if werr := writeTarReader(btw, blobArchivePath(s.BlobsDir(), hash), rc, info.Size(), blobTarMode); werr != nil {
				_ = rc.Close()
				return werr
			}
			if cerr := rc.Close(); cerr != nil {
				return cerr
			}
		}
	}

	// Optionally walk secrets/. Transform is nil here — secrets are written
	// verbatim. (Anonymize=true above blocks this path entirely via the
	// mutual-exclusion guard at the top of Bundle.)
	if opts.SecretsRoot != "" {
		if opts.RespectSyncFlags {
			if err := walkSecretsRespectingSyncFlags(btw, opts.SecretsRoot); err != nil {
				return err
			}
		} else {
			if err := walkAndAddToTar(btw, opts.SecretsRoot, "secrets", nil); err != nil {
				return err
			}
		}
	}
	if err := btw.Close(); err != nil {
		return fmt.Errorf("acf: close scratch tar: %w", err)
	}
	if err := artifactFile.Sync(); err != nil {
		return err
	}
	if _, err := artifactFile.Seek(0, io.SeekStart); err != nil {
		return err
	}

	// Derive TotalBytes + per-path sha256 from the buffered entries — the very
	// bytes that get replayed below, so the manifest can't drift from reality.
	hashes, totalBytes, err := hashTarBodiesReader(artifactFile)
	if err != nil {
		return fmt.Errorf("acf: hash bundle bodies: %w", err)
	}

	// os.Hostname() failure is non-fatal (FR-01.8): record "" and ship anyway.
	hostname, _ := os.Hostname()

	meta := BundleMeta{
		BundleVersion:   BundleVersion,
		CreatedAt:       time.Now().UTC(),
		AplexicaVersion: opts.AplexicaVersion,
		ArtifactCounts:  counts,
		IncludesSecrets: opts.SecretsRoot != "",
		Anonymized:      opts.Anonymize,
		Hostname:        hostname,
		TotalBytes:      totalBytes,
		Hashes:          hashes,
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("acf: marshal bundle meta: %w", err)
	}
	// meta.json itself is NEVER anonymized — it's bundle metadata and
	// callers downstream need to read it verbatim to know what's in the
	// bundle (Anonymized=true, version, counts, etc.).
	if err := writeTarFile(tw, "meta.json", metaBytes, 0o600); err != nil {
		return err
	}

	// Replay the buffered artifact entries verbatim into the real tar.
	if _, err := artifactFile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := replayTarReader(tw, artifactFile); err != nil {
		return fmt.Errorf("acf: replay bundle bodies: %w", err)
	}

	return nil
}

// hashTarBodies reads an (uncompressed) tar image and returns, for every
// regular-file entry, a map of entry-name -> lowercase-hex sha256 of its body,
// plus the summed body length across all entries. Directory entries contribute
// nothing. Reuses the FileEntry.SHA256 idiom (crypto/sha256 + hex). Used by
// Bundle to populate BundleMeta.Hashes / BundleMeta.TotalBytes from exactly the
// bytes it is about to ship.
func hashTarBodies(tarImage []byte) (map[string]string, int64, error) {
	return hashTarBodiesReader(bytes.NewReader(tarImage))
}

func hashTarBodiesReader(r io.Reader) (map[string]string, int64, error) {
	hashes := map[string]string{}
	var total int64
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		h := sha256.New()
		n, err := io.Copy(h, tr)
		if err != nil || n != hdr.Size {
			return nil, 0, fmt.Errorf("acf: hash tar body: %w", err)
		}
		if n > math.MaxInt64-total {
			return nil, 0, securityerr.ErrLimitExceeded
		}
		hashes[hdr.Name] = hex.EncodeToString(h.Sum(nil))
		total += n
	}
	return hashes, total, nil
}

// replayTar copies every entry from an (uncompressed) tar image into tw,
// preserving headers and bodies byte-for-byte. Used by Bundle to emit the
// scratch-buffered artifact entries after meta.json.
func replayTar(tw *tar.Writer, tarImage []byte) error {
	return replayTarReader(tw, bytes.NewReader(tarImage))
}

func replayTarReader(tw *tar.Writer, r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if _, err := io.Copy(tw, tr); err != nil { //nolint:gosec // bounded by the in-memory scratch tar we just wrote
			return err
		}
	}
	return nil
}

func writeTarReader(tw *tar.Writer, name string, body io.Reader, size int64, mode os.FileMode) error {
	if size < 0 {
		return securityerr.ErrLimitExceeded
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(mode), Size: size, ModTime: time.Now().UTC()}); err != nil {
		return err
	}
	n, err := io.Copy(tw, io.LimitReader(body, size+1))
	if err != nil {
		return err
	}
	if n != size {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// collectBundleBlobHashes returns the sorted set of attachment content
// hashes whose blobs must be carried in the bundle so ContentHashes resolve
// after restore. It scans the bundled conversation artifacts (those in
// includeSet, or all when includeSet is nil) and applies latest-wins PER
// ARTIFACT over create/update/resolution events and payload-bearing
// checkpoint events (snapshot/baseline): a ContentHash whose most recent
// assertion is non-evicted is referenced; one whose latest assertion is
// evicted (or that is never asserted) is not.
//
// This mirrors retention.LiveBlobSet — which acf cannot import without an
// import cycle (retention imports acf) — so the bundled set matches exactly
// the blobs a healthy store keeps on disk. In particular, a blob created
// non-evicted then re-asserted evicted by a later append (and since GC'd)
// is NOT collected, so Bundle does not fail trying to read a deleted blob.
//
// Only KindConversation carries attachments; the other three kinds have no
// blob references, so they are not scanned.
func (s *Store) collectBundleBlobHashes(includeSet map[string]struct{}) ([]string, error) {
	arts, err := s.ListArtifacts(KindConversation)
	if err != nil {
		return nil, fmt.Errorf("acf: bundle list conversations for blobs: %w", err)
	}
	refs := map[string]struct{}{}
	for _, art := range arts {
		if includeSet != nil {
			if _, ok := includeSet[bundleKey(KindConversation, art.ArtifactID)]; !ok {
				continue
			}
		}
		// Active + compacted, matching the on-disk live set (LiveBlobSet).
		events, eerr := s.ReadEventsIncludingCompacted(KindConversation, art.ArtifactID)
		if eerr != nil {
			return nil, fmt.Errorf("acf: bundle read events %s: %w", art.ArtifactID, eerr)
		}
		// latest-wins WITHIN this artifact: the last event (append order)
		// to assert a ContentHash decides whether it is still referenced.
		perArtifact := map[string]bool{} // hash -> evicted
		for _, e := range events {
			switch e.Type {
			case EventTypeCreate, EventTypeUpdate, EventTypeResolution:
				// Payload-bearing content events assert attachment slots.
			case EventTypeSnapshot, EventTypeBaseline:
				// A payload-bearing checkpoint (FR-02.32 snapshot, or an
				// aligned-chains baseline) carries the full materialized
				// state INCLUDING the attachment list, and can be the ONLY
				// event naming a ContentHash (post-prune snapshot-only log
				// whose compacted segment was grace-deleted, or a baseline
				// adoption where the origin history never existed locally).
				// Skipping it would ship a bundle whose restored checkpoint
				// has a dangling non-evicted ContentHash. Mirrors
				// retention.isCheckpointEvent (unimportable here — cycle).
				// A legacy payload-LESS snapshot asserts nothing.
				if !HasPayload(e.Payload) {
					continue
				}
			default:
				continue
			}
			var p ConversationPayload
			if jerr := json.Unmarshal(e.Payload, &p); jerr != nil {
				continue
			}
			for _, att := range p.Attachments {
				if att.ContentHash == "" {
					continue
				}
				perArtifact[att.ContentHash] = att.IsEvicted()
			}
		}
		for hash, evicted := range perArtifact {
			if !evicted {
				refs[hash] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(refs))
	for hash := range refs {
		out = append(out, hash)
	}
	sort.Strings(out) // deterministic bundle ordering
	return out, nil
}

// blobArchivePath returns the slash-separated bundle path for a content
// blob, mirroring the on-disk shard layout under the store's blobs/ dir
// (blobs/<AA>/<BB>/<hash>). It reuses blobstore.Store.Path so the
// sharding/hashing rules live in exactly one place.
func blobArchivePath(blobsRoot, hash string) string {
	bs := &blobstore.Store{Root: blobsRoot}
	rel, err := filepath.Rel(blobsRoot, bs.Path(hash))
	if err != nil {
		// Path is always under Root for any well-formed hash; degrade to a
		// flat entry if Rel ever fails (restore re-shards via Put anyway).
		rel = hash
	}
	return path_join(blobsDirName, filepath.ToSlash(rel))
}

// restoreBlob writes one bundled attachment blob into the target store's
// content-addressed blob store and verifies content-addressed integrity:
// blobstore.Put recomputes sha256(body), and the result MUST equal the
// blob's archive leaf name (the hash the bundle claimed). A mismatch means a
// corrupt or tampered bundle and aborts the restore.
func restoreBlob(blobsRoot, archiveName string, body []byte) error {
	parts := strings.Split(archiveName, "/")
	wantHash := parts[len(parts)-1]
	bs := &blobstore.Store{Root: blobsRoot}
	gotHash, err := bs.Put(body)
	if err != nil {
		return fmt.Errorf("acf: restore attachment blob %s: %w", wantHash, err)
	}
	if gotHash != wantHash {
		return fmt.Errorf("acf: restore attachment blob: content hash mismatch (bundle claimed %q, computed %q)",
			wantHash, gotHash)
	}
	return nil
}

// walkSecretsRespectingSyncFlags walks the secrets root and emits tar
// entries only for global-name secrets whose sidecar at
// .meta/<name>.json carries `syncEnabled = true`. Per-artifact
// directories pass through unfiltered (they're outside the BRD §4.4.1
// opt-in model). The .meta dir itself is included only for the
// included names (so the receiving side gets matching sidecars).
//
// Filtering happens via two passes:
//  1. Read .meta/ to build the set of opt-in names (syncEnabled=true).
//  2. Walk root and include only:
//     - Per-artifact directories and their contents.
//     - Top-level files whose name is in the opt-in set.
//     - .meta/<name>.json files whose <name> is in the opt-in set.
//
// Returns nil on success; the io.Writer is closed by the caller.
func walkSecretsRespectingSyncFlags(tw *tar.Writer, secretsRoot string) error {
	optIn, err := loadSyncEnabledNames(secretsRoot)
	if err != nil {
		return err
	}

	return filepath.Walk(secretsRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(secretsRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Tar paths use forward slashes regardless of host OS.
		relSlash := filepath.ToSlash(rel)
		archivePath := path_join("secrets", relSlash)

		// Decide inclusion before reading the body.
		if info.IsDir() {
			// Always emit directory entries so the receiving Restore can
			// honor permissions (Restore creates dirs lazily anyway, but
			// emitting the entry matches the verbatim-walk shape).
			return writeTarDir(tw, archivePath, info)
		}

		// .meta/<name>.json — include only if the name is opt-in.
		if strings.HasPrefix(relSlash, ".meta/") {
			name := strings.TrimSuffix(strings.TrimPrefix(relSlash, ".meta/"), ".json")
			if !optIn[name] {
				return nil
			}
			return writeTarFileFromPath(tw, archivePath, path, info)
		}

		// Top-level (relSlash has no "/" separator) — global secret.
		if !strings.Contains(relSlash, "/") {
			if !optIn[relSlash] {
				return nil
			}
			return writeTarFileFromPath(tw, archivePath, path, info)
		}

		// Per-artifact path (relSlash like "<artifact-id>/<key>"). Pass
		// through unfiltered.
		return writeTarFileFromPath(tw, archivePath, path, info)
	})
}

// loadSyncEnabledNames reads every .meta/<name>.json sidecar under
// secretsRoot and returns the set of names whose JSON carries
// `"syncEnabled": true`. Missing .meta dir → empty set (no
// global-name secrets are opt-in yet).
func loadSyncEnabledNames(secretsRoot string) (map[string]bool, error) {
	out := map[string]bool{}
	metaDir := filepath.Join(secretsRoot, ".meta")
	entries, err := os.ReadDir(metaDir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(metaDir, e.Name()))
		if err != nil {
			return nil, err
		}
		var meta struct {
			Name        string `json:"name"`
			SyncEnabled bool   `json:"syncEnabled"`
		}
		if err := json.Unmarshal(body, &meta); err != nil {
			continue // malformed sidecars are skipped; the value is what's load-bearing
		}
		if meta.SyncEnabled {
			name := meta.Name
			if name == "" {
				name = strings.TrimSuffix(e.Name(), ".json")
			}
			out[name] = true
		}
	}
	return out, nil
}

// path_join joins archive-path components using forward slashes
// regardless of the host OS. Avoids the platform-dependent
// filepath.Join for tar headers.
func path_join(parts ...string) string {
	return strings.Join(parts, "/")
}

// writeTarFileFromPath streams one file body into the tar writer.
// Distinct from the legacy writeTarFile (which takes body bytes + mode)
// — this helper is used by the v0.73.0 sync-aware secrets walk.
func writeTarFileFromPath(tw *tar.Writer, archivePath, srcPath string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("acf: unsafe bundle producer node: %w", securityerr.ErrUnsafeFilesystemNode)
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		_ = f.Close()
		return fmt.Errorf("acf: bundle producer identity changed: %w", securityerr.ErrUnsafeFilesystemNode)
	}
	err = writeTarReader(tw, archivePath, f, opened.Size(), 0o600)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func writeTarDir(tw *tar.Writer, archivePath string, info os.FileInfo) error {
	hdr := &tar.Header{
		Name:     archivePath + "/",
		Mode:     int64(info.Mode().Perm()),
		Typeflag: tar.TypeDir,
		ModTime:  info.ModTime(),
	}
	return tw.WriteHeader(hdr)
}

// Restore reads a gzipped tar archive from r and writes its contents into s.
// Bundle entries under "secrets/" route to secretsRoot when non-empty;
// otherwise they fall back to <s.Root>/secrets/ (pre-v0.17.2 behavior).
// Returns an error if an artifact already exists in the target store
// (no merge/overwrite) or if the bundle's major version is newer than this
// binary supports.
func (s *Store) restoreLegacyUnsigned(r io.Reader, secretsRoot string) error {
	return securityerr.ErrUnsignedInput
	/* Legacy parser retained temporarily as unreachable source compatibility;
	all callable paths fail before reading attacker-controlled bytes.
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("acf: bundle gzip header: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	validator := newBundlePathValidator(DefaultBundleLimits())

	metaSeen := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("acf: bundle tar: %w", err)
		}
		if err := validator.validateHeader(hdr); err != nil {
			return err
		}
		if !metaSeen && hdr.Name != "meta.json" {
			return fmt.Errorf("acf: bundle first entry: %w", securityerr.ErrUnsafeIdentifier)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}

		body, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("acf: bundle read %s: %w", hdr.Name, err)
		}

		if hdr.Name == "meta.json" {
			var meta BundleMeta
			if err := json.Unmarshal(body, &meta); err != nil {
				return fmt.Errorf("acf: bundle parse meta: %w", err)
			}
			if !versionCompatible(meta.BundleVersion) {
				return fmt.Errorf("acf: bundle version %q is newer than this binary supports (max %s)",
					meta.BundleVersion, BundleVersion)
			}
			metaSeen = true
			continue
		}
		if !metaSeen {
			return fmt.Errorf("acf: bundle is missing meta.json as the first entry")
		}

		// Attachment blobs: route blobs/ entries into the TARGET
		// store's content-addressed blob store, verifying sha256(bytes) ==
		// the claimed hash on the way in. Done via blobstore.Put (atomic,
		// idempotent, recomputes the shard from the hash) so the blob lands
		// under s.BlobsDir() and ContentHashes resolve after restore —
		// regardless of secretsRoot, and never subject to the artifact
		// existence check below.
		if strings.HasPrefix(hdr.Name, blobsDirName+"/") {
			if berr := restoreBlob(s.BlobsDir(), hdr.Name, body); berr != nil {
				return berr
			}
			continue
		}

		// Reject existing artifacts (no merge/overwrite). Paths look like
		// "acf/memories/<id>.json" — extract kind + id, check existence.
		if k, id, ok := acfArtifactRef(hdr.Name); ok {
			if _, err := s.ReadArtifact(k, id); err == nil {
				return fmt.Errorf("acf: artifact %s/%s already exists in target store", k, id)
			}
		}

		// Write the file. Route secrets/ entries to secretsRoot when set;
		// otherwise fall back to <store>/secrets/ (pre-v0.17.2 behavior).
		var dst string
		dstRoot := s.Root
		rel := hdr.Name
		if strings.HasPrefix(hdr.Name, "secrets/") && secretsRoot != "" {
			dstRoot = secretsRoot
			rel = strings.TrimPrefix(hdr.Name, "secrets/")
		}
		dst = filepath.Join(dstRoot, filepath.FromSlash(rel))
		if !safepath.Within(dstRoot, dst) {
			return fmt.Errorf("acf: bundle destination: %w", securityerr.ErrPathEscape)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fmt.Errorf("acf: bundle create destination parent: %w", err)
		}
		if err := atomicfile.WriteFile(dst, body, 0o600); err != nil {
			return fmt.Errorf("acf: bundle write entry: %w", err)
		}
	}
	if !metaSeen {
		return fmt.Errorf("acf: bundle missing meta.json")
	}
	return nil */
}

// KindDryRun is the per-kind classification a RestoreDryRun produces: how many
// artifacts of this kind would be newly added vs. already present (and thus a
// collision that a real Restore would abort on), plus the colliding IDs.
type KindDryRun struct {
	// Adds is the count of acf/<kind>/<id>.json entries whose id is NOT yet in
	// the target store.
	Adds int `json:"adds"`
	// CollisionIDs lists the ids whose artifact already exists in the target
	// store (sorted). A real Restore fails on the first of these.
	CollisionIDs []string `json:"collisionIds,omitempty"`
}

// DryRunResult is the outcome of Store.RestoreDryRun (FR-01.13): the full diff a
// restore WOULD apply, classified per kind, with nothing written. It answers
// "which artifact IDs are new vs. already-present in THIS target store" — the
// question --peek (which only echoes the bundle's own manifest) cannot.
type DryRunResult struct {
	// ByKind maps each artifact kind to its add/collision classification.
	// Kinds with no bundle entries are absent.
	ByKind map[Kind]*KindDryRun `json:"byKind"`
}

// TotalAdds returns the number of artifacts (across all kinds) that would be
// newly written by a real restore.
func (r DryRunResult) TotalAdds() int {
	n := 0
	for _, kd := range r.ByKind {
		n += kd.Adds
	}
	return n
}

// TotalCollisions returns the number of artifacts (across all kinds) that
// already exist in the target store and would therefore abort a real restore.
func (r DryRunResult) TotalCollisions() int {
	n := 0
	for _, kd := range r.ByKind {
		n += len(kd.CollisionIDs)
	}
	return n
}

// CollisionIDs returns every colliding artifact id across all kinds, sorted, so
// callers can preview exactly which IDs would block a restore.
func (r DryRunResult) CollisionIDs() []string {
	var ids []string
	for _, kd := range r.ByKind {
		ids = append(ids, kd.CollisionIDs...)
	}
	sort.Strings(ids)
	return ids
}

// RestoreDryRun streams a bundle exactly like Restore (gzip+tar walk, same
// version gate, same kind/id derivation) but writes NOTHING. For each
// acf/<kind>/<id>.json entry it classifies the (kind,id) as a would-add or an
// already-exists collision against the TARGET store, returning the full diff a
// real Restore would apply (FR-01.13). It shares Restore's collision check
// (Store.ReadArtifact) so the two paths can't drift on what counts as a
// collision; the only difference is that the writes are replaced by tallying.
func (s *Store) restoreDryRunLegacyUnsigned(r io.Reader) (DryRunResult, error) {
	return DryRunResult{}, securityerr.ErrUnsignedInput
	/* Unreachable legacy implementation; see restoreLegacyUnsigned.
	res := DryRunResult{ByKind: map[Kind]*KindDryRun{}}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return res, fmt.Errorf("acf: bundle gzip header: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	validator := newBundlePathValidator(DefaultBundleLimits())

	metaSeen := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, fmt.Errorf("acf: bundle tar: %w", err)
		}
		if err := validator.validateHeader(hdr); err != nil {
			return res, err
		}
		if !metaSeen && hdr.Name != "meta.json" {
			return res, fmt.Errorf("acf: bundle first entry: %w", securityerr.ErrUnsafeIdentifier)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}

		if hdr.Name == "meta.json" {
			body, err := io.ReadAll(tr)
			if err != nil {
				return res, fmt.Errorf("acf: bundle read %s: %w", hdr.Name, err)
			}
			var meta BundleMeta
			if err := json.Unmarshal(body, &meta); err != nil {
				return res, fmt.Errorf("acf: bundle parse meta: %w", err)
			}
			if !versionCompatible(meta.BundleVersion) {
				return res, fmt.Errorf("acf: bundle version %q is newer than this binary supports (max %s)",
					meta.BundleVersion, BundleVersion)
			}
			metaSeen = true
			continue
		}
		if !metaSeen {
			return res, fmt.Errorf("acf: bundle is missing meta.json as the first entry")
		}

		// Only acf/<kind>/<id>.json entries carry artifact identity; everything
		// else (events/, eventTags/, branches/, secrets/) rides along with its
		// artifact and is not independently classified. Share Restore's exact
		// path parsing + existence check via acfArtifactRef so the two can't
		// disagree on what's a collision.
		if k, id, ok := acfArtifactRef(hdr.Name); ok {
			kd := res.ByKind[k]
			if kd == nil {
				kd = &KindDryRun{}
				res.ByKind[k] = kd
			}
			if _, err := s.ReadArtifact(k, id); err == nil {
				kd.CollisionIDs = append(kd.CollisionIDs, id)
			} else {
				kd.Adds++
			}
		}
	}
	if !metaSeen {
		return res, fmt.Errorf("acf: bundle missing meta.json")
	}
	// Stable per-kind ordering for deterministic CLI output.
	for _, kd := range res.ByKind {
		sort.Strings(kd.CollisionIDs)
	}
	return res, nil */
}

// PeekBundleMeta reads just the meta.json from a bundle without restoring.
// Used by the CLI to display bundle contents before user confirmation.
func PeekBundleMeta(r io.Reader) (BundleMeta, error) {
	return PeekBundleMetaWithLimits(r, DefaultBundleLimits())
}

// writeTarFile writes a single file entry into the tar archive.
func writeTarFile(tw *tar.Writer, name string, body []byte, mode os.FileMode) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    int64(mode),
		Size:    int64(len(body)),
		ModTime: time.Now().UTC(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("acf: tar header %s: %w", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("acf: tar body %s: %w", name, err)
	}
	return nil
}

// walkAndAddToTar walks rootDir and adds each file to the tar archive under
// archivePrefix/<relative-path>. Preserves the file mode (so secrets stay
// 0o600). When transform is non-nil, each file body is passed through it
// AFTER read + BEFORE tar header (so the post-transform length is what's
// recorded as Size). nil transform = passthrough.
//
// Split (v0.31.0): the read/transform/write sequence is explicit so the
// anonymization pass can intercept payload bytes without forking the walk.
// hasFilters reports whether any v0.63.0 scope/project filter is active.
// When false, Bundle skips the include-set pre-pass entirely and tars
// every artifact (legacy v0.59.0 behavior).
func (o BundleOpts) hasFilters() bool {
	return o.ScopeFilter != "" || len(o.ProjectFilter) > 0 || len(o.PendingIDs) > 0
}

// includeArtifact returns true when a should be in the bundle per the
// v0.63.0 BRD-02 §4.13 / FR-01.10 rules:
//
//   - ScopeFilter set: a.Scope must equal it.
//   - ProjectFilter non-empty: a.Project.ID must be in the list. Implies
//     project-scope; global/namespace artifacts never match.
//   - PendingIDs non-empty: a.Project.ID must NOT be in the set (paired
//     with --include-pending-projects=false at the CLI).
//
// Filters compose with AND. Missing Project on a project-scope artifact
// is treated as "unknown project ID" — included unless ProjectFilter is
// set (in which case there's nothing to match against).
func (o BundleOpts) includeArtifact(a Artifact) bool {
	if o.ScopeFilter != "" && a.Scope != o.ScopeFilter {
		return false
	}
	if len(o.ProjectFilter) > 0 {
		if a.Project == nil || a.Project.ID == "" {
			return false
		}
		found := false
		for _, id := range o.ProjectFilter {
			if a.Project.ID == id {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(o.PendingIDs) > 0 && a.Project != nil {
		if _, pending := o.PendingIDs[a.Project.ID]; pending {
			return false
		}
	}
	return true
}

// bundleKey is the canonical key used in the include-set: "<kind>/<id>".
// Walker rebuilds this from filesystem paths during the filtered walk.
func bundleKey(k Kind, id string) string {
	return string(k) + "/" + id
}

// walkAndAddToTarFiltered is walkAndAddToTar plus an include-set gate.
// nil includeSet means "include everything" (v0.59.0 behavior). When
// non-nil, the walker maps each archive path back to a (kind, id) and
// skips paths not in the set.
//
// Path shapes the walker expects:
//
//	acf/<kindplural>/<id>.json       (e.g., acf/memories/<id>.json)
//	events/<kindplural>/<id>.jsonl   (e.g., events/memories/<id>.jsonl)
//	events/<kindplural>/.compacted/<id>.jsonl  (retention-grace tier)
//
// Any other shape (unrecognized) is included unconditionally to avoid
// silently dropping store-internal bookkeeping files.
func walkAndAddToTarFiltered(tw *tar.Writer, rootDir, archivePrefix string,
	transform BundleTransform, includeSet map[string]struct{}) error {
	if includeSet == nil {
		return walkAndAddToTar(tw, rootDir, archivePrefix, transform)
	}
	info, err := os.Stat(rootDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("acf: stat %s: %w", rootDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("acf: %s is not a directory", rootDir)
	}
	return filepath.Walk(rootDir, func(path string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if fi.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(rootDir, path)
		if err != nil {
			return err
		}
		archiveName := filepath.ToSlash(filepath.Join(archivePrefix, rel))
		if k, id, ok := parseArchivePath(archiveName); ok {
			if _, want := includeSet[bundleKey(k, id)]; !want {
				return nil // skip
			}
		}
		if transform != nil {
			if !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 || fi.Size() > DefaultBundleLimits().MaxEntryBytes {
				return securityerr.ErrUnsafeFilesystemNode
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			body, err := io.ReadAll(io.LimitReader(f, DefaultBundleLimits().MaxEntryBytes+1))
			_ = f.Close()
			if err != nil || int64(len(body)) > DefaultBundleLimits().MaxEntryBytes {
				return securityerr.ErrLimitExceeded
			}
			out, terr := transform(archiveName, body)
			if terr != nil {
				return fmt.Errorf("acf: bundle transform %s: %w", archiveName, terr)
			}
			return writeTarFile(tw, archiveName, out, 0o600)
		}
		return writeTarFileFromPath(tw, archiveName, path, fi)
	})
}

// acfArtifactRef parses an "acf/<kindplural>/<id>.json" archive path into its
// (Kind, id). It returns ok=false for any other shape (non-acf prefix, wrong
// segment count, unknown kind dir). Both Restore and RestoreDryRun route their
// per-entry collision check through this single helper so the two paths agree
// on exactly which entries carry artifact identity.
func acfArtifactRef(archivePath string) (k Kind, id string, ok bool) {
	if !strings.HasPrefix(archivePath, "acf/") || !strings.HasSuffix(archivePath, ".json") {
		return "", "", false
	}
	parts := strings.Split(archivePath, "/")
	if len(parts) != acfArtifactPathSegments {
		return "", "", false
	}
	k = kindFromDirName(parts[1])
	if k == "" {
		return "", "", false
	}
	return k, strings.TrimSuffix(parts[2], ".json"), true
}

// acfArtifactPathSegments is the segment count of a canonical artifact archive
// path "acf/<kindplural>/<id>.json" once split on "/".
const acfArtifactPathSegments = 3

// parseArchivePath maps an "acf/memories/<id>.json" or
// "events/memories/<id>.jsonl" archive name back to (Kind, id, true).
// Unrecognized shapes return (_, _, false) — the filtered walker then
// includes them unconditionally.
func parseArchivePath(archive string) (Kind, string, bool) {
	// archive uses forward-slashes (we filepath.ToSlash before storing).
	parts := strings.Split(archive, "/")
	if len(parts) < 3 {
		return "", "", false
	}
	top := parts[0]
	if top != "acf" && top != "events" {
		return "", "", false
	}
	kind := kindFromDirName(parts[1])
	if kind == "" {
		return "", "", false
	}
	last := parts[len(parts)-1]
	id := strings.TrimSuffix(last, ".json")
	id = strings.TrimSuffix(id, ".jsonl")
	if id == last {
		return "", "", false
	}
	return kind, id, true
}

func walkAndAddToTar(tw *tar.Writer, rootDir, archivePrefix string, transform BundleTransform) error {
	info, err := os.Stat(rootDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // not an error — store may have no events for this kind yet
		}
		return fmt.Errorf("acf: stat %s: %w", rootDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("acf: %s is not a directory", rootDir)
	}

	return filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(rootDir, path)
		if err != nil {
			return err
		}
		archiveName := filepath.ToSlash(filepath.Join(archivePrefix, rel))
		if transform != nil {
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > DefaultBundleLimits().MaxEntryBytes {
				return securityerr.ErrUnsafeFilesystemNode
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			body, err := io.ReadAll(io.LimitReader(f, DefaultBundleLimits().MaxEntryBytes+1))
			_ = f.Close()
			if err != nil || int64(len(body)) > DefaultBundleLimits().MaxEntryBytes {
				return securityerr.ErrLimitExceeded
			}
			out, terr := transform(archiveName, body)
			if terr != nil {
				return fmt.Errorf("acf: bundle transform %s: %w", archiveName, terr)
			}
			return writeTarFile(tw, archiveName, out, 0o600)
		}
		return writeTarFileFromPath(tw, archiveName, path, info)
	})
}

// kindFromDirName maps "memories" -> KindMemory, etc.
func kindFromDirName(d string) Kind {
	switch d {
	case "memories":
		return KindMemory
	case "skills":
		return KindSkill
	case "tools":
		return KindTool
	case "conversations":
		return KindConversation
	}
	return ""
}

// versionCompatible reports whether a bundle with declared bundleVersion can
// be safely restored by the current binary. V0.1.10 accepts the exact major
// version "1" (so "1.0", "1.1", "1.2" all work; "2.0" does not).
func versionCompatible(bundleVersion string) bool {
	bundleMajor := strings.SplitN(bundleVersion, ".", 2)[0]
	ourMajor := strings.SplitN(BundleVersion, ".", 2)[0]
	return bundleMajor == ourMajor
}

// writeMetaOnlyBundle is used by tests to build a minimal valid bundle with
// a custom meta.json (e.g. to test version gating).
func writeMetaOnlyBundle(w *bytes.Buffer, metaBytes []byte) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := writeTarFile(tw, "meta.json", metaBytes, 0o600); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
