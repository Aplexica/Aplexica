package daemon

// Outbox is the daemon-side durable outbox (B1) for the outbound
// remote-publish path. It is a directory of atomically-written per-event JSON
// files keyed by a monotonic enqueue sequence, mirroring the existing
// conflicts sidecar (internal/conflicts). Each pending event is one file; the
// file is the source of truth for "this event has not yet been ACCEPTED by the
// relay". Steady state is near-empty because the outbox self-drains as the
// relay accepts events; it grows only while the plugin/relay is unreachable.
//
// Guarantees (B1):
//   - persist-before-publish: Append writes the durable file (atomicfile:
//     write-tmp, fsync, rename) BEFORE the event is handed to the in-memory
//     pump queue.
//   - delete-only-after-terminal: a file is removed ONLY when the relay
//     accepts the event (Remove) or returns a non-retryable rejection
//     (Deadletter); a retryable / full-queue / crash never deletes it.
//   - startup-resume: List replays pending files in seq (== FIFO) order.
//   - bounded growth: named count, entry-byte, and aggregate-byte caps reject
//     new writes while leaving a durable full-rescan obligation.
//   - retained-slot compaction: newer retained conversation snapshots supersede
//     older pending snapshots for the same namespace/branch/artifact/origin.
//
// The outbox takes no dependency on internal/sync; it persists the
// proto.RemoteEvent wire shape the pump already handles.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/securityepoch"
)

// outboxSchemaVersion is the on-disk schema version for an outbox entry,
// stamped into every file for forward-compatibility.
const outboxSchemaVersion = 2

// outboxDeadSchemaVersion is deliberately separate from the pending-entry
// schema. Dead letters are terminal tombstones, not a second durable copy of
// the publish payload. Keeping the full encrypted event here used to duplicate
// multi-megabyte conversation events forever.
const outboxDeadSchemaVersion = 1

// outboxMaxEntries is the per-daemon hard intent bound. A live intent is never
// evicted merely to admit a newer one: doing so would lose the only durable
// obligation to publish an immutable local event. Capacity is rejected before
// write and the caller must leave/raise the durable full-rescan obligation.
const outboxMaxEntries = 25000

// A publishable event is capped at 4 MiB by the remote publisher. The larger
// file allowance covers the outbox JSON wrapper and duplicated content-free
// intent metadata while still making every startup read independently bounded.
const outboxMaxEntryBytes int64 = 8 << 20

// outboxMaxBytes is the durable aggregate budget for pending publish intent.
// The canonical store remains authoritative; once this budget is reached a
// dirty rescan marker is retained and new sidecar writes fail closed instead of
// allowing a disconnected relay to consume tens or hundreds of gigabytes.
const outboxMaxBytes int64 = 128 << 20

// List returns one bounded prefix at a time. periodicDrain calls it again after
// the in-memory queues empty, so old upgrade backlogs still drain without a
// single startup allocation proportional to the whole directory.
const outboxListMaxBytes int64 = 32 << 20

// outboxWarnEntries is the pending-file count at/above which Append emits a
// user-visible (rate-limited) Warn so a long disconnect is visible before it
// approaches the cap.
const outboxWarnEntries = 5000

const outboxWarnBytes int64 = 64 << 20

// outboxWarnInterval rate-limits the backlog Warn so a sustained disconnect
// cannot spam the log.
const outboxWarnInterval = 1 * time.Minute

// A status snapshot walks only a bounded number of directory entries. The
// outbox can contain the three managed subdirectories plus a small number of
// torn atomic-write siblings, but anything beyond the live-entry cap and this
// allowance is not trustworthy operator evidence.
const (
	outboxStatusScanBatch      = 256
	outboxStatusInventorySlack = 64
)

// outboxSeqWidth is the fixed width of the zero-padded sequence prefix in a
// filename, chosen so a uint64 sequence never overflows the field and so
// os.ReadDir's lexical sort equals numeric (== FIFO enqueue) order.
const outboxSeqWidth = 16

// outboxSeqBase / outboxSeqBits are the base and bit-size used to parse the
// decimal seq prefix back out of a filename (the sequence is a uint64).
const (
	outboxSeqBase = 10
	outboxSeqBits = 64
)

// outboxFilePerm is the mode for a persisted outbox entry (matches the
// conflicts sidecar's 0o600).
const outboxFilePerm = 0o600

// outboxDirPerm is the mode for the outbox root + dead/ subdir (matches the
// conflicts sidecar's 0o700).
const outboxDirPerm = 0o700

// outboxFileSuffix is the per-entry filename suffix.
const outboxFileSuffix = ".json"

// outboxDeadSubdir is the dead-letter subdirectory name (content-free terminal
// markers retained for bounded idempotency and operator correlation).
const outboxDeadSubdir = "dead"

// outboxDeadTombstoneMaxBytes is a fast-path bound used while migrating old
// dead letters. A valid tombstone is far smaller than this; larger files are
// legacy payload-bearing entries and can be replaced without being read into
// memory.
const outboxDeadTombstoneMaxBytes = 4 * 1024

// Dead letters are only content-free terminal markers. Keep enough recent
// markers to suppress immediate re-enqueue loops, but do not let even those
// markers become an unbounded second history database.
const (
	outboxDeadMaxEntries = 10_000
	outboxDeadMaxBytes   = 32 << 20
)

var ErrOutboxRecoveryTerminal = errors.New("outbox: recovery event has terminal tombstone")

var ErrOutboxRecoveryAuthorityConflict = errors.New("outbox: pending recovery authority conflicts with canonical candidate")

var ErrOutboxRecoveryAuthorityUnavailable = errors.New("outbox: exact recovery seal authority unavailable")

var errOutboxRecoveryMissing = errors.New("outbox: recovery event is not pending")

// outboxEntry is one decoded pending outbox file.
type outboxEntry struct {
	SchemaVersion int               `json:"schemaVersion"`
	Seq           uint64            `json:"seq"`
	EnqueuedAt    time.Time         `json:"enqueuedAt"`
	Event         proto.RemoteEvent `json:"event"`
	Intent        RemoteSealIntent  `json:"intent"`
}

// deadOutboxEntry is the content-free terminal record kept for idempotency and
// operator correlation. The canonical store remains authoritative for the
// artifact; rejected wire bytes are a disposable cache and must not be copied
// into dead/.
type deadOutboxEntry struct {
	SchemaVersion  int       `json:"schemaVersion"`
	Seq            uint64    `json:"seq"`
	EventID        string    `json:"eventId"`
	DeadletteredAt time.Time `json:"deadletteredAt"`
}

// RemoteSealIntent is content-free canonical identity sufficient to locate
// and reseal current state. Event is only a cache and is never authority after
// an access/key/security-barrier change.
type RemoteSealIntent struct {
	IntentKind                     string `json:"intentKind"`
	NamespaceID                    string `json:"namespaceId,omitempty"`
	ProjectID                      string `json:"projectId,omitempty"`
	ProjectAuthorizationGeneration uint64 `json:"projectAuthorizationGeneration,omitempty"`
	Kind                           string `json:"kind"`
	ArtifactID                     string `json:"artifactId"`
	CanonicalEventID               string `json:"canonicalEventId,omitempty"`
	CanonicalEventHash             string `json:"canonicalEventHash,omitempty"`
	SourceHeadHash                 string `json:"sourceHeadHash,omitempty"`
	CheckpointAlignmentHash        string `json:"checkpointAlignmentHash,omitempty"`
	BranchID                       string `json:"branchId,omitempty"`
	Lane                           string `json:"lane,omitempty"`
	WireEventID                    string `json:"wireEventId"`
	OriginDeviceID                 string `json:"originDeviceId,omitempty"`
	Sequence                       uint64 `json:"sequence"`
}

func intentForEvent(ev proto.RemoteEvent) RemoteSealIntent {
	kind := "live-event"
	if ev.Lane == "retained" {
		kind = "retained-slot"
		if ev.Clear {
			kind = "retained-clear"
		}
	}
	canonical := ev.EventID
	if ev.Lane == "retained" {
		// The canonical id is authenticated inside envelope v2. Keep this empty
		// rather than guessing by parsing the opaque transport suffix.
		canonical = ""
	}
	return RemoteSealIntent{IntentKind: kind, NamespaceID: ev.NamespaceID, ProjectID: ev.ProjectID, ProjectAuthorizationGeneration: ev.ProjectAuthorizationGeneration, Kind: ev.Kind, ArtifactID: ev.ArtifactID, CanonicalEventID: canonical, SourceHeadHash: ev.ParentHash, CheckpointAlignmentHash: ev.CheckpointAlignmentHash, BranchID: ev.BranchID, Lane: ev.Lane, WireEventID: ev.EventID, OriginDeviceID: ev.Origin, Sequence: ev.Sequence}
}

type retainedOutboxSlot struct {
	NamespaceID string
	BranchID    string
	ArtifactID  string
	Origin      string
	Kind        string
}

type retainedOutboxCandidate struct {
	name string
	seq  uint64
}

// Outbox is the file-backed durable outbox. Root is a sibling of the conflicts
// dir under daemonStateDir (e.g. ~/.aplexica/outbox). All mutating operations
// take mu so seq assignment, the in-memory count, and file ops are consistent
// across concurrent Append callers (the pump is the sole reader, but Append is
// driven synchronously off the orchestrator import path).
type Outbox struct {
	Root   string
	logger interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	mu           sync.Mutex
	initialized  bool   // true only after Init has fully reconciled on-disk state
	seq          uint64 // next sequence to assign; monotonic across restarts
	count        int    // live pending-file count (excludes dead/)
	pendingBytes int64  // total bytes of validated live pending files
	pendingSizes map[string]int64

	// The production limits are initialized from the named constants in Init.
	// Keeping them as fields gives tests small deterministic budgets instead of
	// writing hundreds of megabytes merely to exercise a boundary.
	maxEntries      int
	maxEntryBytes   int64
	maxPendingBytes int64
	listMaxBytes    int64

	// warnInterval gates the backlog Warn; a field (not the bare const) so a
	// test can lower it via a seam.
	warnInterval time.Duration
	lastWarn     time.Time
	mutations    *RemoteMutationCoordinator
}

// dead is the dead-letter subdirectory path.
func (o *Outbox) dead() string { return filepath.Join(o.Root, outboxDeadSubdir) }

func (o *Outbox) staged() string { return filepath.Join(o.Root, "staged") }

// compactDeadFiles replaces payload-bearing dead letters written by older
// daemons with small tombstones. The event id and sequence are already encoded
// in the validated filename, so even a multi-gigabyte legacy entry never
// needs to be read into memory during migration.
func (o *Outbox) compactDeadFiles() (int, error) {
	entries, err := os.ReadDir(o.dead())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("outbox: read dead letters: %w", err)
	}
	compacted := 0
	var firstErr error
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		seq, eventID, ok := parseOutboxName(de.Name())
		if !ok {
			continue
		}
		path := filepath.Join(o.dead(), de.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("outbox: inspect dead letter %s: %w", eventID, statErr)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			if firstErr == nil {
				firstErr = fmt.Errorf("outbox: dead letter %s is not a regular file", eventID)
			}
			continue
		}
		alreadyTombstone := false
		if info.Size() <= outboxDeadTombstoneMaxBytes {
			if data, readErr := readSmallRegularFile(path, info, outboxDeadTombstoneMaxBytes); readErr == nil {
				var tombstone deadOutboxEntry
				if json.Unmarshal(data, &tombstone) == nil &&
					tombstone.SchemaVersion == outboxDeadSchemaVersion &&
					tombstone.Seq == seq && tombstone.EventID == eventID {
					alreadyTombstone = true
				}
			}
		}
		if alreadyTombstone {
			continue
		}
		if err := o.rewriteDeadTombstoneInPlace(de.Name(), seq, eventID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		compacted++
	}
	return compacted, firstErr
}

// readSmallRegularFile reads at most max+1 bytes from the same regular inode
// identified by expected. This keeps validation of existing tombstones
// bounded even if a path is concurrently replaced after Lstat.
func readSmallRegularFile(path string, expected os.FileInfo, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return nil, fmt.Errorf("path no longer names the expected regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("file exceeds %d bytes", max)
	}
	return data, nil
}

// rewriteDeadTombstoneInPlace deliberately reuses the existing inode. A
// legacy payload commonly fills the disk, so an atomic temp-file replacement
// cannot be relied on to have even one free block. Truncating the verified
// inode first releases its payload blocks; if the process stops before the
// small write completes, the validated filename remains a terminal marker
// and the next post-start sweep retries the rewrite.
func (o *Outbox) rewriteDeadTombstoneInPlace(name string, seq uint64, eventID string) error {
	data, err := json.Marshal(deadOutboxEntry{
		SchemaVersion:  outboxDeadSchemaVersion,
		Seq:            seq,
		EventID:        eventID,
		DeadletteredAt: time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("outbox: marshal dead-letter tombstone %s: %w", eventID, err)
	}
	path := filepath.Join(o.dead(), name)
	expected, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("outbox: inspect dead letter %s: %w", eventID, err)
	}
	if !expected.Mode().IsRegular() {
		return fmt.Errorf("outbox: dead letter %s is not a regular file", eventID)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("outbox: open dead letter %s: %w", eventID, err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return fmt.Errorf("outbox: inspect opened dead letter %s: %w", eventID, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return fmt.Errorf("outbox: dead letter %s changed before rewrite", eventID)
	}
	// Truncating in place is safe only when this pathname is the inode's sole
	// link. Otherwise a valid-looking dead-letter hardlink could release space by
	// destroying an unrelated user file on the same volume. Keep the suspicious
	// object byte-for-byte for operator inspection and fail closed.
	if err := validateDeadletterInPlaceFile(f); err != nil {
		return fmt.Errorf("outbox: dead letter %s is unsafe for in-place rewrite: %w", eventID, err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("outbox: truncate legacy dead letter %s: %w", eventID, err)
	}
	n, err := f.WriteAt(data, 0)
	if err != nil {
		return fmt.Errorf("outbox: write dead-letter tombstone %s: %w", eventID, err)
	}
	if n != len(data) {
		return fmt.Errorf("outbox: short dead-letter tombstone write %s: %d of %d bytes", eventID, n, len(data))
	}
	if err := f.Chmod(outboxFilePerm); err != nil {
		return fmt.Errorf("outbox: chmod dead-letter tombstone %s: %w", eventID, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("outbox: sync compacted dead-letter tombstone %s: %w", eventID, err)
	}
	return nil
}

type deadTombstoneFile struct {
	name string
	seq  uint64
	size int64
}

// pruneDeadTombstones removes the oldest valid tombstones until both global
// limits are satisfied. Non-regular, malformed, and legacy files are not
// silently deleted; Init's compactor handles legacy files and reports anything
// requiring operator attention.
func (o *Outbox) pruneDeadTombstones(maxEntries int, maxBytes int64) (int, error) {
	entries, err := os.ReadDir(o.dead())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("outbox: read dead letters for pruning: %w", err)
	}
	valid := make([]deadTombstoneFile, 0, len(entries))
	var totalBytes int64
	for _, de := range entries {
		seq, eventID, ok := parseOutboxName(de.Name())
		if !ok {
			continue
		}
		path := filepath.Join(o.dead(), de.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > outboxDeadTombstoneMaxBytes {
			continue
		}
		data, err := readSmallRegularFile(path, info, outboxDeadTombstoneMaxBytes)
		if err != nil {
			continue
		}
		var tombstone deadOutboxEntry
		if json.Unmarshal(data, &tombstone) != nil ||
			tombstone.SchemaVersion != outboxDeadSchemaVersion ||
			tombstone.Seq != seq || tombstone.EventID != eventID {
			continue
		}
		valid = append(valid, deadTombstoneFile{name: de.Name(), seq: seq, size: info.Size()})
		totalBytes += info.Size()
	}
	sort.Slice(valid, func(i, j int) bool {
		if valid[i].seq == valid[j].seq {
			return valid[i].name < valid[j].name
		}
		return valid[i].seq < valid[j].seq
	})
	removed := 0
	var firstErr error
	for _, candidate := range valid {
		if len(valid)-removed <= maxEntries && totalBytes <= maxBytes {
			break
		}
		if err := os.Remove(filepath.Join(o.dead(), candidate.name)); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("outbox: prune dead-letter tombstone %s: %w", candidate.name, err)
			}
			continue
		}
		removed++
		totalBytes -= candidate.size
	}
	return removed, firstErr
}

func (o *Outbox) enforceDeadTombstoneBounds() {
	removed, err := o.pruneDeadTombstones(outboxDeadMaxEntries, outboxDeadMaxBytes)
	if err != nil && o.logger != nil {
		o.logger.Warn("outbox: dead-letter tombstone pruning incomplete", "removed", removed, "err", err)
	}
	if removed > 0 && o.logger != nil {
		o.logger.Info("outbox: pruned oldest dead-letter tombstones", "removed", removed)
	}
}

// SweepDeadLetters performs the potentially large one-time legacy migration
// outside Init's startup-critical path. The outbox lock serializes it with new
// dead-letter moves; an interrupted sweep is idempotently resumed next start.
func (o *Outbox) SweepDeadLetters() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if compacted, err := o.compactDeadFiles(); err != nil {
		if o.logger != nil {
			o.logger.Warn("outbox: dead-letter payload cleanup incomplete", "compacted", compacted, "err", err)
		}
	} else if compacted > 0 && o.logger != nil {
		o.logger.Info("outbox: replaced legacy dead-letter payloads with tombstones", "compacted", compacted)
	}
	o.enforceDeadTombstoneBounds()
}

// movePendingToDeadLocked atomically removes a terminal event from the live
// outbox, then replaces its payload-bearing file with a content-free
// tombstone. If the process dies between the rename and rewrite, Init performs
// the same compaction on the next start.
func (o *Outbox) movePendingToDeadLocked(name string) error {
	seq, eventID, ok := parseOutboxName(name)
	if !ok {
		return fmt.Errorf("outbox: invalid pending filename %q", name)
	}
	entry, hasEntry := o.readEntryFileLocked(name)
	if err := os.Rename(filepath.Join(o.Root, name), filepath.Join(o.dead(), name)); err != nil {
		return err
	}
	if err := o.rewriteDeadTombstoneInPlace(name, seq, eventID); err != nil && o.logger != nil {
		// The terminal move already succeeded. Leave the legacy file for Init's
		// retry rather than returning an error that could re-publish the event.
		o.logger.Warn("outbox: dead-letter tombstone write deferred until restart", "event_id", eventID, "err", err)
	}
	if hasEntry {
		if err := o.removeStagedPayloadLocked(entry.Event); err != nil {
			// The terminal intent is already quarantined. Init's orphan
			// reconciliation retries cleanup; never republish solely because the
			// content-free terminal move won its race with file cleanup.
			if o.logger != nil {
				o.logger.Warn("outbox: staged payload cleanup deferred until restart", "event_id", eventID, "err", err)
			}
		}
	}
	return nil
}

// Init creates the outbox root + dead/ subdir and seeds the monotonic sequence
// one past the highest seq already on disk (so ordering stays monotonic across
// restarts) and reconciles the live pending count from the directory.
func (o *Outbox) Init() error {
	// Status must fail closed while initialization (or re-initialization) is in
	// progress. In particular, a zero count is not evidence of a drained outbox
	// until the root has been created and its on-disk contents reconciled.
	o.mu.Lock()
	defer o.mu.Unlock()
	o.initialized = false

	if err := os.MkdirAll(o.Root, outboxDirPerm); err != nil {
		return fmt.Errorf("outbox: mkdir %s: %w", o.Root, err)
	}
	if err := os.MkdirAll(o.dead(), outboxDirPerm); err != nil {
		return fmt.Errorf("outbox: mkdir %s: %w", o.dead(), err)
	}
	stagedRoot, err := privatefs.OpenRoot(o.staged(), privatefs.DirPolicy{
		Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true,
	})
	if err != nil {
		return fmt.Errorf("outbox: initialize staged payload root: %w", err)
	}
	if err := stagedRoot.Close(); err != nil {
		return fmt.Errorf("outbox: close staged payload root: %w", err)
	}
	o.mutations = &RemoteMutationCoordinator{Root: filepath.Join(o.Root, "rescan-markers")}
	if o.maxEntries <= 0 {
		o.maxEntries = outboxMaxEntries
	}
	if o.maxEntryBytes <= 0 {
		o.maxEntryBytes = outboxMaxEntryBytes
	}
	if o.maxPendingBytes <= 0 {
		o.maxPendingBytes = outboxMaxBytes
	}
	if o.listMaxBytes <= 0 {
		o.listMaxBytes = outboxListMaxBytes
	}
	if o.warnInterval <= 0 {
		o.warnInterval = outboxWarnInterval
	}
	entries, err := os.ReadDir(o.Root)
	if err != nil {
		return fmt.Errorf("outbox: read %s: %w", o.Root, err)
	}
	var maxSeq uint64
	seen := false
	type pendingDiskFile struct {
		name string
		size int64
	}
	var pending []pendingDiskFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), outboxFileSuffix) {
			continue
		}
		seq, _, ok := parseOutboxName(e.Name())
		if !ok {
			continue // tmp/garbage sibling; skip (torn-file recovery)
		}
		if !seen || seq > maxSeq {
			maxSeq = seq
			seen = true
		}
		info, statErr := os.Lstat(filepath.Join(o.Root, e.Name()))
		if statErr != nil || !info.Mode().IsRegular() || info.Size() < 0 {
			// List also refuses non-regular paths. They are not counted toward
			// valid pending capacity and are left untouched for diagnosis.
			continue
		}
		pending = append(pending, pendingDiskFile{name: e.Name(), size: info.Size()})
	}
	if seen && maxSeq+1 > o.seq {
		o.seq = maxSeq + 1
	}
	o.count = 0
	o.pendingBytes = 0
	o.pendingSizes = make(map[string]int64, len(pending))
	for _, candidate := range pending {
		o.count++
		if candidate.size > 0 && o.pendingBytes <= int64(^uint64(0)>>1)-candidate.size {
			o.pendingBytes += candidate.size
		} else {
			o.pendingBytes = int64(^uint64(0) >> 1)
		}
		o.pendingSizes[candidate.name] = candidate.size
	}
	if o.count > o.maxEntries || o.pendingBytes > o.maxPendingBytes {
		// Exact sealed bytes are part of the idempotency key once a cloud append
		// may have committed. Never unlink live intents merely because a newer
		// build lowered a local cache budget; keep/drain them and let subsequent
		// appends fail closed with a generation-bound rescan marker.
		if o.logger != nil {
			o.logger.Warn("remote outbox exceeds current durable budget; preserving exact pending ciphertext",
				"pending", o.count, "pending_bytes", o.pendingBytes,
				"entry_cap_bytes", o.maxEntryBytes, "total_cap_bytes", o.maxPendingBytes)
		}
	}
	if err := o.compactSupersededRetainedLocked(); err != nil {
		return err
	}
	if err := o.reconcileStagedPayloadsLocked(); err != nil {
		return err
	}
	o.initialized = true
	return nil
}

// parseOutboxName parses a "<16-digit seq>-<eventID>.json" filename, returning
// the seq and eventID. ok is false for any name that does not match (a
// .tmp.<rand> sibling from atomicfile, or other garbage) so callers skip it.
func parseOutboxName(name string) (seq uint64, eventID string, ok bool) {
	if !strings.HasSuffix(name, outboxFileSuffix) {
		return 0, "", false
	}
	base := strings.TrimSuffix(name, outboxFileSuffix)
	dash := strings.IndexByte(base, '-')
	if dash <= 0 || dash >= len(base) {
		return 0, "", false
	}
	seqPart := base[:dash]
	if len(seqPart) != outboxSeqWidth {
		return 0, "", false
	}
	n, err := strconv.ParseUint(seqPart, outboxSeqBase, outboxSeqBits)
	if err != nil {
		return 0, "", false
	}
	id := base[dash+1:]
	if id == "" {
		return 0, "", false
	}
	return n, id, true
}

// sanitizeEventID defensively rejects path separators in an event id. ACF
// EventIDs are hex/base32 hash strings (already filename-safe); the seq prefix
// is the authoritative ordering key regardless.
func sanitizeEventID(id string) (string, error) {
	if id == "" {
		return "", errors.New("empty event id")
	}
	if strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return "", fmt.Errorf("unsafe event id %q", id)
	}
	return id, nil
}

// fileName builds the on-disk filename for a (seq, eventID) pair. The seq is
// zero-padded to outboxSeqWidth so lexical sort == numeric == FIFO order.
func outboxFileName(seq uint64, eventID string) string {
	return fmt.Sprintf("%0*d-%s%s", outboxSeqWidth, seq, eventID, outboxFileSuffix)
}

// findFilesIn locates every on-disk filename for an eventID in dir (the seq
// prefix is not known to callers that hold only the EventID, e.g.
// Remove/Deadletter). Multiple matches should not be created by current code,
// but older daemons could append duplicate pending files for the same event ID;
// terminal cleanup removes all of them so stale duplicates cannot keep
// re-publishing forever.
func (o *Outbox) findFilesIn(dir, eventID string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("outbox: read %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		_, id, ok := parseOutboxName(e.Name())
		if ok && id == eventID {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// findFiles locates every pending on-disk filename for an eventID.
func (o *Outbox) findFiles(eventID string) ([]string, error) {
	return o.findFilesIn(o.Root, eventID)
}

// findDeadFiles locates every dead-lettered on-disk filename for an eventID.
func (o *Outbox) findDeadFiles(eventID string) ([]string, error) {
	return o.findFilesIn(o.dead(), eventID)
}

// findFile returns the first pending file for eventID. Kept for tests and
// single-file probes; terminal cleanup paths use findFiles.
func (o *Outbox) findFile(eventID string) (string, error) {
	names, err := o.findFiles(eventID)
	if err != nil || len(names) == 0 {
		return "", err
	}
	return names[0], nil
}

func retainedSlotFor(ev proto.RemoteEvent) (retainedOutboxSlot, bool) {
	if ev.Lane != "retained" || ev.ArtifactID == "" {
		return retainedOutboxSlot{}, false
	}
	branch := ev.BranchID
	if branch == "" {
		branch = "main"
	}
	return retainedOutboxSlot{
		NamespaceID: ev.NamespaceID,
		BranchID:    branch,
		ArtifactID:  ev.ArtifactID,
		Origin:      ev.Origin,
		Kind:        ev.Kind,
	}, true
}

func readOutboxEntryFile(root, name string, maxBytes int64) (outboxEntry, int64, bool) {
	seq, eventID, ok := parseOutboxName(name)
	if !ok {
		return outboxEntry{}, 0, false
	}
	path := filepath.Join(root, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxBytes {
		return outboxEntry{}, 0, false
	}
	data, err := readSmallRegularFile(path, info, maxBytes)
	if err != nil {
		// The bounded reader may have consumed maxBytes+1 while detecting a
		// concurrently-grown file. Charge the complete caller budget so a
		// scan cannot repeatedly spend the same allowance on malformed files.
		return outboxEntry{}, maxBytes, false
	}
	var entry outboxEntry
	if err := json.Unmarshal(data, &entry); err != nil || entry.Seq != seq || entry.Event.EventID != eventID {
		return outboxEntry{}, int64(len(data)), false
	}
	// json.RawMessage(nil) is persisted as the JSON literal null. Restore the
	// original empty wire body on read so exact-body digests and retained CLEAR
	// semantics do not change across a restart.
	if string(entry.Event.Bytes) == "null" {
		entry.Event.Bytes = nil
	}
	if !validOutboxStagedDescriptor(root, entry.Event) {
		return outboxEntry{}, int64(len(data)), false
	}
	return entry, int64(len(data)), true
}

func validOutboxStagedDescriptor(root string, event proto.RemoteEvent) bool {
	staged := event.DaemonStagedPayload
	if staged == nil {
		return true
	}
	if len(event.Bytes) != 0 || !validRemoteTransferToken(staged.FileID) ||
		staged.SealedBytes <= proto.MaxSealedEventBytes || staged.SealedBytes > proto.MaxRemoteStagedCheckpointBytes ||
		!validRemoteTransferToken(staged.BodyDigest) || event.BodyDigest != staged.BodyDigest ||
		event.Lane != "retained" || event.Clear {
		return false
	}
	info, err := os.Lstat(filepath.Join(root, "staged", staged.FileID))
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Size() == int64(staged.SealedBytes)
}

// reconcileStagedPayloadsLocked removes only valid, private-root regular files
// that are no longer referenced by a live outbox entry. This closes the
// unavoidable crash window between staging a body and atomically publishing
// its lightweight JSON intent, while never deleting an unknown node.
func (o *Outbox) reconcileStagedPayloadsLocked() error {
	refs := make(map[string]struct{})
	entries, err := os.ReadDir(o.Root)
	if err != nil {
		return fmt.Errorf("outbox: list staged references: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, _, ok := parseOutboxName(entry.Name()); !ok {
			continue
		}
		persisted, ok := o.readEntryFileLocked(entry.Name())
		if ok && persisted.Event.DaemonStagedPayload != nil {
			refs[persisted.Event.DaemonStagedPayload.FileID] = struct{}{}
		}
	}
	root, err := privatefs.OpenRoot(o.staged(), privatefs.DirPolicy{Access: privatefs.AccessPrivate, AllowExisting: true})
	if err != nil {
		return fmt.Errorf("outbox: retain staged payload root: %w", err)
	}
	defer root.Close()
	files, err := root.ReadDir(".")
	if err != nil {
		return fmt.Errorf("outbox: list staged payloads: %w", err)
	}
	for _, file := range files {
		if !validRemoteTransferToken(file.Name()) || file.Type()&fs.ModeSymlink != 0 || !file.Type().IsRegular() {
			return errors.New("outbox: unsafe node in staged payload root")
		}
		if _, retained := refs[file.Name()]; retained {
			continue
		}
		if err := root.RemoveRegular(file.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("outbox: remove orphan staged payload: %w", err)
		}
	}
	return root.SyncDir(".")
}

// ReconcileStagedPayloads removes only staged files that have no live durable
// outbox reference. It is safe after an outbox append failure: a duplicate or
// previously persisted event keeps its referenced exact retry body, while the
// pre-append crash residue is reclaimed immediately instead of consuming the
// bounded staging budget until restart.
func (o *Outbox) ReconcileStagedPayloads() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reconcileStagedPayloadsLocked()
}

func (o *Outbox) readEntryFileLocked(name string) (outboxEntry, bool) {
	entry, _, ok := readOutboxEntryFile(o.Root, name, o.maxEntryBytes)
	return entry, ok
}

func (o *Outbox) forgetPendingFileLocked(name string) {
	size, ok := o.pendingSizes[name]
	if !ok {
		return
	}
	delete(o.pendingSizes, name)
	if o.count > 0 {
		o.count--
	}
	if size >= o.pendingBytes {
		o.pendingBytes = 0
	} else {
		o.pendingBytes -= size
	}
}

func (o *Outbox) removePendingFilesLocked(names []string) error {
	for _, name := range names {
		entry, hasEntry := o.readEntryFileLocked(name)
		err := os.Remove(filepath.Join(o.Root, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("outbox: remove pending file %s: %w", name, err)
		}
		o.forgetPendingFileLocked(name)
		if hasEntry {
			if err := o.removeStagedPayloadLocked(entry.Event); err != nil {
				return err
			}
		}
	}
	return nil
}

func (o *Outbox) removeStagedPayloadLocked(event proto.RemoteEvent) error {
	staged := event.DaemonStagedPayload
	if staged == nil {
		return nil
	}
	if !validRemoteTransferToken(staged.FileID) {
		return errors.New("outbox: invalid staged payload reference")
	}
	root, err := privatefs.OpenRoot(o.staged(), privatefs.DirPolicy{Access: privatefs.AccessPrivate, AllowExisting: true})
	if err != nil {
		return fmt.Errorf("outbox: retain staged payload root: %w", err)
	}
	defer root.Close()
	if err := root.RemoveRegular(staged.FileID); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("outbox: remove staged payload: %w", err)
	}
	return root.SyncDir(".")
}

func (o *Outbox) removeSupersededRetainedLocked(ev proto.RemoteEvent) error {
	slot, ok := retainedSlotFor(ev)
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(o.Root)
	if err != nil {
		return fmt.Errorf("outbox: read %s: %w", o.Root, err)
	}
	var remove []string
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		_, id, ok := parseOutboxName(de.Name())
		if !ok || id == ev.EventID {
			continue
		}
		entry, ok := o.readEntryFileLocked(de.Name())
		if !ok {
			continue
		}
		if existingSlot, ok := retainedSlotFor(entry.Event); ok && existingSlot == slot {
			remove = append(remove, de.Name())
		}
	}
	return o.removePendingFilesLocked(remove)
}

func (o *Outbox) compactSupersededRetainedLocked() error {
	entries, err := os.ReadDir(o.Root)
	if err != nil {
		return fmt.Errorf("outbox: read %s: %w", o.Root, err)
	}
	newest := map[retainedOutboxSlot]retainedOutboxCandidate{}
	var remove []string
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		seq, _, ok := parseOutboxName(de.Name())
		if !ok {
			continue
		}
		entry, ok := o.readEntryFileLocked(de.Name())
		if !ok {
			continue
		}
		slot, ok := retainedSlotFor(entry.Event)
		if !ok {
			continue
		}
		candidate := retainedOutboxCandidate{name: de.Name(), seq: seq}
		if prev, exists := newest[slot]; exists {
			if candidate.seq > prev.seq {
				remove = append(remove, prev.name)
				newest[slot] = candidate
			} else {
				remove = append(remove, candidate.name)
			}
			continue
		}
		newest[slot] = candidate
	}
	return o.removePendingFilesLocked(remove)
}

// Append durably persists ev BEFORE it is handed to the in-memory pump queue.
// It assigns the next monotonic sequence under mu, writes the file via
// atomicfile (write-tmp, fsync, rename), and enforces count, per-entry, and
// aggregate-byte budgets. Capacity rejection never evicts a live intent; the
// mutation marker remains dirty so a later canonical-store rescan repairs the
// intentionally missing cache entry.
func (o *Outbox) Append(ev proto.RemoteEvent) error {
	_, err := o.append(ev, true)
	return err
}

// AppendForPublish is the normal live path plus the exact persisted event and
// scope recovery state after persistence. Returning the persisted event is
// essential because envelope sealing is randomized: a duplicate callback for
// one EventID must queue the already-durable ciphertext, never a fresh reseal.
// A dirty result means the intent must stay off the priority live queue while
// canonical recovery owns the complete range ordering.
func (o *Outbox) AppendForPublish(ev proto.RemoteEvent) (proto.RemoteEvent, bool, error) {
	if _, err := o.append(ev, true); err != nil {
		return proto.RemoteEvent{}, true, err
	}
	persisted, err := o.pendingRecoveryAuthority(ev)
	if err != nil {
		// append may have found an older exact EventID or a terminal tombstone
		// before it opened a marker transaction. Reserve the current canonical
		// target now so this conflict cannot be mistaken for successful publish.
		markErr := o.RequireCanonicalRecovery(ev)
		if markErr != nil {
			return proto.RemoteEvent{}, true, fmt.Errorf("outbox: pending authority: %v; reserve recovery: %w", err, markErr)
		}
		return proto.RemoteEvent{}, true, err
	}
	if o.mutations == nil {
		return persisted, false, nil
	}
	dirty, err := o.mutations.IsDirty(ev.NamespaceID)
	if err != nil {
		return proto.RemoteEvent{}, true, fmt.Errorf("outbox: inspect rescan obligation: %w", err)
	}
	return persisted, dirty, nil
}

// RequireCanonicalRecovery leaves a generation-bound dirty marker for an
// event that was durably committed to the canonical store but cannot enter the
// realtime transport path (for example, an oversized live delta). The exact
// pending ciphertext, if any, is intentionally left untouched. A checkpoint
// producer must later fulfill the obligation before the marker can clear.
func (o *Outbox) RequireCanonicalRecovery(ev proto.RemoteEvent) error {
	if o == nil || o.mutations == nil {
		return errors.New("outbox: recovery coordinator unavailable")
	}
	mutation, err := o.mutations.Begin(ev.NamespaceID, outboxEntry{
		SchemaVersion: outboxSchemaVersion,
		Event:         ev,
		Intent:        intentForEvent(ev),
	})
	if err != nil {
		return fmt.Errorf("outbox: reserve canonical recovery: %w", err)
	}
	return mutation.Close()
}

func recoveryBranchID(value string) string {
	if value == "" {
		return "main"
	}
	return value
}

func sameRecoveredEventAuthority(persisted, candidate proto.RemoteEvent) bool {
	return persisted.ProjectID == candidate.ProjectID &&
		persisted.ProjectAuthorizationGeneration == candidate.ProjectAuthorizationGeneration &&
		persisted.AccessGeneration == candidate.AccessGeneration &&
		persisted.AccessSetHash == candidate.AccessSetHash &&
		persisted.SecurityGeneration == candidate.SecurityGeneration &&
		persisted.SecurityBarrierID == candidate.SecurityBarrierID &&
		persisted.KeyMode == candidate.KeyMode && persisted.KeyVersion == candidate.KeyVersion &&
		persisted.NamespaceID == candidate.NamespaceID && recoveryBranchID(persisted.BranchID) == recoveryBranchID(candidate.BranchID) &&
		persisted.ArtifactID == candidate.ArtifactID && persisted.EventID == candidate.EventID &&
		persisted.ParentHash == candidate.ParentHash && persisted.EventHash == candidate.EventHash &&
		persisted.Kind == candidate.Kind && persisted.Type == candidate.Type && persisted.Timestamp.Equal(candidate.Timestamp) &&
		persisted.Sequence == candidate.Sequence && persisted.Origin == candidate.Origin &&
		persisted.SourceAgent == candidate.SourceAgent && persisted.Lane == candidate.Lane &&
		persisted.CheckpointCoverage == candidate.CheckpointCoverage &&
		persisted.CheckpointGeneration == candidate.CheckpointGeneration &&
		persisted.CheckpointAlignmentHash == candidate.CheckpointAlignmentHash && persisted.Clear == candidate.Clear
}

func (o *Outbox) pendingRecoveryAuthority(candidate proto.RemoteEvent) (proto.RemoteEvent, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	names, err := o.findFiles(candidate.EventID)
	if err != nil {
		return proto.RemoteEvent{}, err
	}
	dead, deadErr := o.findDeadFiles(candidate.EventID)
	if deadErr != nil {
		return proto.RemoteEvent{}, deadErr
	}
	if len(names) > 1 || (len(names) > 0 && len(dead) > 0) {
		return proto.RemoteEvent{}, ErrOutboxRecoveryAuthorityConflict
	}
	if len(names) == 0 {
		if len(dead) > 0 {
			return proto.RemoteEvent{}, ErrOutboxRecoveryTerminal
		}
		return proto.RemoteEvent{}, errOutboxRecoveryMissing
	}
	entry, ok := o.readEntryFileLocked(names[0])
	if !ok {
		return proto.RemoteEvent{}, ErrOutboxRecoveryAuthorityConflict
	}
	validBody := entry.Event.BodyDigest == "" || entry.Event.DaemonStagedPayload != nil ||
		strings.EqualFold(entry.Event.BodyDigest, sealedBodyDigest(entry.Event.Bytes))
	if !sameRecoveredEventAuthority(entry.Event, candidate) || !validBody {
		return proto.RemoteEvent{}, ErrOutboxRecoveryAuthorityConflict
	}
	return entry.Event, nil
}

// AppendRecovered persists an event selected by the canonical recovery
// scanner without opening another marker mutation. The existing marker is the
// durable umbrella for the whole range and remains dirty until cloud
// watermarks prove every rebuilt event committed.
func (o *Outbox) AppendRecovered(ev proto.RemoteEvent, allowCreate bool) (proto.RemoteEvent, error) {
	// Re-sealing is randomized. If this EventID was already pending (including
	// the crash-after-cloud-commit / receipt-loss case), the persisted bytes are
	// the only safe idempotent retry authority; never queue the freshly sealed
	// candidate over them.
	persisted, err := o.pendingRecoveryAuthority(ev)
	if err == nil {
		return persisted, nil
	}
	if !errors.Is(err, errOutboxRecoveryMissing) {
		return proto.RemoteEvent{}, err
	}
	if !allowCreate {
		return proto.RemoteEvent{}, ErrOutboxRecoveryAuthorityUnavailable
	}
	// Only the marker's exact first failed target is known never to have
	// entered the network without a durable file. Other absent events may have
	// been published before their exact randomized seal disappeared, so the
	// caller must route them to checkpoint recovery instead of re-sealing.
	if _, err := o.append(ev, false); err != nil {
		return proto.RemoteEvent{}, err
	}
	return o.pendingRecoveryAuthority(ev)
}

func (o *Outbox) append(ev proto.RemoteEvent, reserveMutation bool) (bool, error) {
	id, err := sanitizeEventID(ev.EventID)
	if err != nil {
		return false, fmt.Errorf("outbox: %w", err)
	}
	if !validOutboxStagedDescriptor(o.Root, ev) {
		return false, errors.New("outbox: invalid staged payload descriptor")
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	existing, err := o.findFiles(id)
	if err != nil {
		return false, err
	}
	if len(existing) > 0 {
		return false, nil
	}
	dead, err := o.findDeadFiles(id)
	if err != nil {
		return false, err
	}
	if len(dead) > 0 {
		if !reserveMutation {
			return false, ErrOutboxRecoveryTerminal
		}
		return false, nil
	}
	entry := outboxEntry{SchemaVersion: outboxSchemaVersion, EnqueuedAt: time.Now().UTC(), Event: ev, Intent: intentForEvent(ev)}
	var mutation *RemoteMutation
	if reserveMutation && o.mutations != nil {
		mutation, err = o.mutations.Begin(ev.NamespaceID, entry)
		if err != nil {
			return false, fmt.Errorf("outbox: reserve rescan obligation: %w", err)
		}
		defer mutation.Close()
	}
	if err := o.removeSupersededRetainedLocked(ev); err != nil {
		return false, err
	}
	if o.count >= o.maxEntries {
		return false, fmt.Errorf("outbox: intent count capacity reached (%d entries)", o.maxEntries)
	}
	if ev.DaemonStagedPayload == nil && int64(remoteEventApproxBytes(ev)) > o.maxEntryBytes {
		return false, fmt.Errorf("outbox: event exceeds per-entry capacity (%d bytes)", o.maxEntryBytes)
	}

	seq := o.seq
	entry.Seq = seq
	// Plain Marshal (not MarshalIndent) so the embedded RawMessage event.Bytes
	// is persisted verbatim — indenting would reflow the opaque canonical
	// payload and is unnecessary for a machine-only sidecar.
	data, err := json.Marshal(entry)
	if err != nil {
		return false, fmt.Errorf("outbox: marshal %s: %w", id, err)
	}
	entryBytes := int64(len(data))
	if entryBytes > o.maxEntryBytes {
		return false, fmt.Errorf("outbox: entry %s exceeds per-entry capacity (%d bytes)", id, o.maxEntryBytes)
	}
	if entryBytes > o.maxPendingBytes || o.pendingBytes > o.maxPendingBytes-entryBytes {
		return false, fmt.Errorf("outbox: aggregate byte capacity reached (%d bytes)", o.maxPendingBytes)
	}
	path := filepath.Join(o.Root, outboxFileName(seq, id))
	if err := atomicfile.WriteFile(path, data, outboxFilePerm); err != nil {
		return false, fmt.Errorf("outbox: persist %s: %w", id, err)
	}
	o.seq++
	o.count++
	o.pendingBytes += entryBytes
	if o.pendingSizes == nil {
		o.pendingSizes = make(map[string]int64)
	}
	o.pendingSizes[filepath.Base(path)] = entryBytes
	if mutation != nil {
		if err := mutation.Complete(); err != nil {
			return true, fmt.Errorf("outbox: complete rescan obligation: %w", err)
		}
	}

	o.maybeWarnLocked()
	return true, nil
}

// maybeWarnLocked emits a rate-limited backlog Warn when either the pending
// count or pending bytes reach their early-warning threshold. Caller holds mu.
func (o *Outbox) maybeWarnLocked() {
	if (o.count < outboxWarnEntries && o.pendingBytes < outboxWarnBytes) || o.logger == nil {
		return
	}
	now := time.Now()
	if !o.lastWarn.IsZero() && now.Sub(o.lastWarn) < o.warnInterval {
		return
	}
	o.lastWarn = now
	o.logger.Warn("remote outbox backlog growing; relay may be unreachable",
		"pending", o.count, "pending_bytes", o.pendingBytes,
		"warn_entries", outboxWarnEntries, "warn_bytes", outboxWarnBytes,
		"entry_cap", o.maxEntries, "byte_cap", o.maxPendingBytes)
}

// evictOldestLocked is retained for explicit legacy-recovery tooling. Normal
// Append never calls it: reaching a capacity limit rejects new cache writes
// and preserves the dirty rescan obligation instead of sacrificing live work.
// Caller holds mu.
func (o *Outbox) evictOldestLocked() {
	entries, err := os.ReadDir(o.Root)
	if err != nil {
		if o.logger != nil {
			o.logger.Error("outbox: cap eviction scan failed", "err", err)
		}
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, _, ok := parseOutboxName(e.Name()); ok {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	oldest := names[0]
	if err := o.movePendingToDeadLocked(oldest); err != nil {
		if o.logger != nil {
			o.logger.Error("outbox: cap eviction rename failed", "file", oldest, "err", err)
		}
		return
	}
	o.forgetPendingFileLocked(oldest)
	o.enforceDeadTombstoneBounds()
	if o.logger != nil {
		_, id, _ := parseOutboxName(oldest)
		o.logger.Error("remote outbox at cap; evicting oldest pending event to dead-letter (recoverable via remote.fetch)",
			"event_id", id, "entry_cap", o.maxEntries, "byte_cap", o.maxPendingBytes)
	}
}

// Remove deletes the durable file for an ACCEPTED event (terminal-accepted).
// A missing file is not an error (idempotent: a resumed-then-accepted event
// may already be gone).
func (o *Outbox) Remove(eventID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	names, err := o.findFiles(eventID)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	if err := o.removePendingFilesLocked(names); err != nil {
		return fmt.Errorf("outbox: remove %s: %w", eventID, err)
	}
	return nil
}

// Deadletter moves the durable file for a NON-RETRYABLE rejected event into
// dead/ (terminal-nonretryable), then replaces its payload with a bounded
// content-free marker. A missing file is not an error.
func (o *Outbox) Deadletter(eventID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	names, err := o.findFiles(eventID)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	for _, name := range names {
		if err := o.movePendingToDeadLocked(name); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				o.forgetPendingFileLocked(name)
				continue
			}
			return fmt.Errorf("outbox: deadletter %s: %w", eventID, err)
		}
		o.forgetPendingFileLocked(name)
	}
	o.enforceDeadTombstoneBounds()
	return nil
}

// PurgeProject quarantines every pending intent issued under projectID. It is
// called as part of live project revocation before success is returned. Files
// are moved to dead/ and reduced to content-free markers so recovery and audit
// can correlate what was withheld without retaining the publish payload.
func (o *Outbox) PurgeProject(projectID string) (int, error) {
	if projectID == "" {
		return 0, fmt.Errorf("outbox: empty project id")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	entries, err := os.ReadDir(o.Root)
	if err != nil {
		return 0, fmt.Errorf("outbox: list for project purge: %w", err)
	}
	purged := 0
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		if _, _, ok := parseOutboxName(de.Name()); !ok {
			continue
		}
		entry, ok := o.readEntryFileLocked(de.Name())
		if !ok || entry.Event.ProjectID != projectID {
			continue
		}
		if err := o.movePendingToDeadLocked(de.Name()); err != nil {
			return purged, fmt.Errorf("outbox: quarantine revoked project intent: %w", err)
		}
		purged++
		o.forgetPendingFileLocked(de.Name())
	}
	o.enforceDeadTombstoneBounds()
	return purged, nil
}

// PurgeSecurityScope removes only reconstructable cached/outbox bytes that
// cannot be published under next. The caller must durably establish a full
// canonical rescan marker first. Future-generation or same-generation
// equivocation fails closed instead of being silently discarded.
func (o *Outbox) PurgeSecurityScope(scopeID string, next securityepoch.SecurityEpoch) (int, error) {
	if o == nil || scopeID == "" || next.CoordinatorGeneration == 0 || next.AccessGeneration == 0 ||
		next.AccessSetHash == ([32]byte{}) || next.BarrierID == ([32]byte{}) || !validRecoveryKeyModeVersion(next.KeyMode, next.KeyVersion) || o.mutations == nil {
		return 0, fmt.Errorf("outbox: invalid security scope purge")
	}
	marker, exists, err := o.mutations.Snapshot(scopeID)
	if err != nil || !exists || marker.State != "dirty" || marker.ReasonFlags&rescanReasonAccessCutover == 0 ||
		marker.TargetAccessGeneration != next.AccessGeneration || marker.TargetAccessSetHash != next.AccessSetHash ||
		marker.TargetSecurityGeneration != next.CoordinatorGeneration || marker.TargetSecurityBarrierID != next.BarrierID ||
		marker.TargetKeyMode != next.KeyMode || marker.TargetKeyVersion != next.KeyVersion {
		return 0, fmt.Errorf("outbox: exact security rescan obligation required before purge")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	entries, err := os.ReadDir(o.Root)
	if err != nil {
		return 0, fmt.Errorf("outbox: list for security purge: %w", err)
	}
	var remove []string
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		if _, _, ok := parseOutboxName(de.Name()); !ok {
			continue
		}
		entry, ok := o.readEntryFileLocked(de.Name())
		if !ok {
			return 0, fmt.Errorf("outbox: invalid pending entry during security purge")
		}
		eventScope := entry.Event.NamespaceID
		if eventScope == "" {
			eventScope = "account"
		}
		if eventScope != scopeID {
			continue
		}
		exactNext := entry.Event.SecurityGeneration == next.CoordinatorGeneration &&
			entry.Event.AccessGeneration == next.AccessGeneration && entry.Event.AccessSetHash == next.AccessSetHash &&
			entry.Event.SecurityBarrierID == next.BarrierID && entry.Event.KeyMode == next.KeyMode && entry.Event.KeyVersion == next.KeyVersion
		if exactNext {
			continue
		}
		if entry.Event.SecurityGeneration >= next.CoordinatorGeneration || entry.Event.AccessGeneration >= next.AccessGeneration ||
			(entry.Event.KeyMode == next.KeyMode && entry.Event.KeyVersion >= next.KeyVersion) {
			return 0, fmt.Errorf("outbox: future or equivocal security generation")
		}
		remove = append(remove, de.Name())
	}
	if err := o.removePendingFilesLocked(remove); err != nil {
		return 0, fmt.Errorf("outbox: purge obsolete security generation: %w", err)
	}
	root, err := privatefs.OpenRoot(o.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return 0, err
	}
	err = root.SyncDir(".")
	_ = root.Close()
	return len(remove), err
}

// List returns a byte-bounded FIFO prefix of PENDING entries (the dead/ subdir
// is excluded). periodicDrain invokes List again after accepted entries are
// removed, so a large backlog drains without allocating every payload at once.
// Per-file decode errors and non-matching siblings are skipped; every read is
// size-bounded and rejects links/special files before opening.
func (o *Outbox) List() ([]outboxEntry, error) {
	entries, err := os.ReadDir(o.Root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("outbox: list %s: %w", o.Root, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, _, ok := parseOutboxName(e.Name()); ok {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // lexical == seq == FIFO
	out := make([]outboxEntry, 0, min(len(names), remotePublishQueueDepth))
	var listedBytes int64
	for _, name := range names {
		path := filepath.Join(o.Root, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > o.maxEntryBytes {
			continue
		}
		if len(out) > 0 && (info.Size() > o.listMaxBytes || listedBytes > o.listMaxBytes-info.Size()) {
			break
		}
		entry, size, ok := readOutboxEntryFile(o.Root, name, o.maxEntryBytes)
		if !ok {
			continue
		}
		out = append(out, entry)
		listedBytes += size
	}
	return out, nil
}

// PendingSnapshot returns one consistent, byte-bounded observation of the
// validated pending count and oldest pending age. The outbox lock remains held
// across both the in-memory count read and the on-disk FIFO-prefix scan, so an
// Append or terminal Remove cannot make the two fields describe different
// points in time.
func (o *Outbox) PendingSnapshot(now time.Time) (pending uint64, oldestAge time.Duration, oldestPresent bool, err error) {
	if o == nil || now.IsZero() {
		return 0, 0, false, errors.New("outbox: invalid pending snapshot")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.initialized {
		return 0, 0, false, errors.New("outbox: pending snapshot unavailable before initialization")
	}
	if err := o.validatePendingInventoryLocked(); err != nil {
		return 0, 0, false, err
	}
	if o.count > 0 {
		pending = uint64(o.count)
	}
	oldestAge, oldestPresent, err = o.oldestPendingAgeLocked(now)
	if err != nil {
		return 0, 0, false, err
	}
	if (pending > 0) != oldestPresent {
		return 0, 0, false, errors.New("outbox: pending snapshot is inconsistent")
	}
	return pending, oldestAge, oldestPresent, nil
}

// validatePendingInventoryLocked proves that the in-memory counters describe
// the exact current set of pending-looking regular files. The mutex makes this
// atomic with every supported Outbox mutation; an external or torn addition,
// removal, replacement, or count/byte mismatch makes status unavailable
// instead of reporting a falsely drained queue.
func (o *Outbox) validatePendingInventoryLocked() error {
	directory, err := os.Open(o.Root)
	if err != nil {
		return fmt.Errorf("outbox: open pending inventory %s: %w", o.Root, err)
	}
	defer directory.Close()

	seen := make(map[string]struct{}, len(o.pendingSizes))
	var diskCount int
	var diskBytes int64
	visited := 0
	limit := o.maxEntries + outboxStatusInventorySlack
	for {
		entries, readErr := directory.ReadDir(outboxStatusScanBatch)
		for _, entry := range entries {
			visited++
			if visited > limit {
				return errors.New("outbox: pending inventory exceeds bounded status scan")
			}
			if _, _, ok := parseOutboxName(entry.Name()); !ok {
				continue
			}
			info, infoErr := entry.Info()
			if infoErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 {
				return errors.New("outbox: pending inventory contains an unsafe entry")
			}
			expectedSize, tracked := o.pendingSizes[entry.Name()]
			if !tracked || expectedSize != info.Size() {
				return errors.New("outbox: pending inventory differs from locked accounting")
			}
			if _, duplicate := seen[entry.Name()]; duplicate {
				return errors.New("outbox: pending inventory contains a duplicate entry")
			}
			seen[entry.Name()] = struct{}{}
			diskCount++
			if info.Size() > int64(^uint64(0)>>1)-diskBytes {
				return errors.New("outbox: pending inventory byte count overflow")
			}
			diskBytes += info.Size()
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("outbox: read pending inventory %s: %w", o.Root, readErr)
		}
	}
	if diskCount != o.count || len(seen) != len(o.pendingSizes) || diskBytes != o.pendingBytes {
		return errors.New("outbox: pending inventory count or bytes differ from locked accounting")
	}
	return nil
}

// OldestPendingAge returns the age of the oldest valid live intent. It scans
// filenames in FIFO order and decodes only a byte-bounded prefix, stopping as
// soon as it finds one valid enqueue timestamp. Malformed entries consume the
// same read budget, so they cannot turn this periodic gauge into an unbounded
// disk or allocation scan.
func (o *Outbox) OldestPendingAge(now time.Time) (time.Duration, bool, error) {
	if o == nil || now.IsZero() {
		return 0, false, errors.New("outbox: invalid oldest-pending observation")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.initialized {
		return 0, false, errors.New("outbox: oldest-pending observation unavailable before initialization")
	}
	return o.oldestPendingAgeLocked(now)
}

// oldestPendingAgeLocked scans one byte-bounded FIFO prefix while the caller
// holds mu. A missing root is an unavailable observation, never an empty,
// successfully drained outbox.
func (o *Outbox) oldestPendingAgeLocked(now time.Time) (time.Duration, bool, error) {
	entries, err := os.ReadDir(o.Root)
	if err != nil {
		return 0, false, fmt.Errorf("outbox: list oldest pending %s: %w", o.Root, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, _, ok := parseOutboxName(entry.Name()); ok {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	remaining := o.listMaxBytes
	for _, name := range names {
		if remaining <= 0 {
			break
		}
		path := filepath.Join(o.Root, name)
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > o.maxEntryBytes {
			continue
		}
		if info.Size() > remaining {
			break
		}
		entry, consumed, ok := readOutboxEntryFile(o.Root, name, min(o.maxEntryBytes, remaining))
		if consumed >= remaining {
			remaining = 0
		} else if consumed > 0 {
			remaining -= consumed
		}
		if !ok {
			continue
		}
		if entry.EnqueuedAt.IsZero() {
			continue
		}
		age := now.UTC().Sub(entry.EnqueuedAt.UTC())
		if age < 0 {
			age = 0
		}
		return age, true, nil
	}
	return 0, false, nil
}
