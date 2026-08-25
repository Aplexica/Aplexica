package acf

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/filelock"
	"github.com/aplexica/aplexica/internal/privatefs"
)

// scanBufInit / scanBufMax govern the bufio.Scanner buffer sizing used
// throughout this file when walking the JSONL event logs. A single event line
// can approach the max-artifact-size (BRD-03 §4.3; default 64 MiB) — long
// structured-conversation turns and tool outputs (BRD-02 §4.6) — so the per-
// line cap must EXCEED that, or reads of a large-history conversation abort with
// "bufio.Scanner: token too long" (the v0.117.x `aplexica list`/`show`/`log`
// crash). The prior 4 MiB cap sat far below a real conversation event; it is
// raised to 256 MiB. bufio.Scanner only grows the buffer as a line demands, so
// small events stay near scanBufInit and only a genuinely huge line allocates
// up — and a stored event can never exceed the ingest-time max-artifact-size.
// (Attachment bytes are content-addressed in internal/blobstore, not
// inlined.) These are file-internal tunables, not user-facing config — they
// live here (and not in defaults.toml) per the magiclint file-exemption
// convention.
const (
	scanBufInit             = 64 * 1024
	scanBufMax              = 256 * 1024 * 1024
	eventCountReadBufferLen = 1 << 20
	privateReadRetryCount   = 16
)

// Store is the on-disk canonical store.
// Default Root is ~/.aplexica/store but callers (and tests) pass any path.
type Store struct {
	Root string

	// eventFileSync/eventFileClose/eventDirSync are narrow durability seams used
	// by tests to prove that an append cannot be reported as committed when the
	// JSONL bytes, checked close, or any canonical path component fails to reach
	// stable storage. Production stores leave them nil and use os.File.Sync,
	// os.File.Close, and privatefs.Root.SyncDir respectively.
	eventFileSync  func(*os.File) error
	eventFileClose func(*os.File) error
	eventDirSync   func(*privatefs.Root, string) error

	// appendLocks serialize the read-head -> append -> metadata-update
	// transaction per artifact. Remote live and retained deliveries can arrive
	// concurrently; without this guard two writers can both validate the same
	// parent and append sibling heads, leaving the JSONL chain malformed. A
	// small stripe table avoids globally blocking unrelated artifacts.
	appendLocks [appendLockStripeCount]sync.Mutex

	// conversationCache holds a small LRU of materialized main-branch payloads
	// produced or verified in this process. Active native imports already parse
	// the full canonical conversation, so retaining that projection lets local
	// fan-out avoid immediately rereading a multi-gigabyte append log. Entries
	// are authoritative only while their cached head matches artifact metadata.
	conversationCacheMu    sync.Mutex
	conversationCache      map[string]conversationCacheEntry
	conversationCacheClock uint64

	// eventIDIndex is a lazy, bounded-memory index used by inbound redelivery
	// dedup. Loading every Event (including hundreds-of-megabytes Payload
	// fields) for each replayed id made reconnect catch-up quadratic in an
	// artifact's history. One streaming pass records ids only; subsequent
	// checks and appends are O(1).
	eventIDIndexMu sync.Mutex
	eventIDIndex   map[string]map[string]struct{}

	// IngestGate is an optional last-resort admission check consulted at the
	// TOP of every AppendEvent (the universal write chokepoint). When non-nil
	// and it returns a non-nil error, AppendEvent refuses the write and
	// returns that error verbatim WITHOUT touching the chain, head, or the
	// events file.
	//
	// nil (the zero value) means "always allow" — acf imposes no quota of its
	// own. The daemon sets this to enforce the emergency-quota ingest refusal
	// (FR-03.21); acf deliberately takes NO dependency on internal/retention
	// or internal/config — the gate is a plain func the daemon supplies, so
	// the layering stays one-directional.
	IngestGate func() error
}

// StoreSchemaVersion is the in-code expected version. Stores with a
// VERSION file whose contents don't parse to this number are rejected.
// Pre-v0.17.1 stores (no VERSION file) are treated as v1 and transparently
// upgraded with a VERSION marker on Init.
//
// Bumped 1 -> 2 for BRD-03 §4.8 integrity-preserving attachment
// eviction): v2 adds the content-addressed blob store under
// <Root>/blobs/. The upgrade is transparent and additive — a v1 store has
// no inline-attachment events (only the evictor ever produced attachments,
// and it never ran in production), so there is no payload migration. The
// blobs/ directory is created empty and the VERSION marker is rewritten to
// "2"; existing artifacts and event chains read + verify unchanged.
const StoreSchemaVersion = 2

// blobsDirName is the store-relative directory holding content-addressed
// attachment blobs (internal/blobstore). Created by Init from v2 onward.
const blobsDirName = "blobs"

// Init creates the directory tree the store needs, then writes (or
// verifies) the VERSION marker. Pre-v0.17.1 stores without VERSION get a
// fresh marker written transparently. A pre-v2 VERSION ("1") is
// transparently upgraded to the current StoreSchemaVersion (additive blob
// store only). A VERSION file whose contents don't parse to a known
// upgradable version is treated as a forward-incompat condition and
// surfaces a clear error instead of failing deeper in the read path.
func (s *Store) Init() error {
	if err := privatefs.EnsureDir(s.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return fmt.Errorf("acf: secure root: %w", err)
	}
	root, err := privatefs.OpenRoot(s.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	if err != nil {
		return fmt.Errorf("acf: open private root: %w", err)
	}
	defer root.Close()
	if err := root.HardenPrivateTree(); err != nil {
		return fmt.Errorf("acf: validate private store: %w", err)
	}
	dirs := []string{
		filepath.Join("acf", "memories"),
		filepath.Join("events", "memories"),
		blobsDirName,
	}
	for _, d := range dirs {
		if err := root.EnsureDir(d, privatefs.DirPolicy{Access: privatefs.AccessPrivate, AllowExisting: true}); err != nil {
			return fmt.Errorf("acf: mkdir %s: %w", d, err)
		}
	}

	vf, err := root.OpenReadRegular("VERSION")
	var data []byte
	if err == nil {
		data, err = io.ReadAll(io.LimitReader(vf, 128))
		cerr := vf.Close()
		if err == nil {
			err = cerr
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		// Pre-v0.17.1 store (or fresh init) — write the marker.
		if err := s.writeVersionRoot(root); err != nil {
			return err
		}
		return root.WriteFile(".permissions-v1", []byte("1\n"), privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
	}
	if err != nil {
		return fmt.Errorf("acf: read VERSION: %w", err)
	}
	got := strings.TrimSpace(string(data))
	want := fmt.Sprintf("%d", StoreSchemaVersion)
	if got == want {
		return root.WriteFile(".permissions-v1", []byte("1\n"), privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
	}
	// Transparent upgrade of a v1 store to the current version: the only
	// schema delta is the additive blobs/ directory (created above), so we
	// just rewrite the marker. Any other mismatch is forward-incompat.
	if got == "1" {
		if err := s.writeVersionRoot(root); err != nil {
			return err
		}
		return root.WriteFile(".permissions-v1", []byte("1\n"), privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
	}
	return fmt.Errorf("acf: store schema version mismatch: file says %q, this build expects %q", got, want)
}

func (s *Store) writeVersionRoot(root *privatefs.Root) error {
	if err := root.WriteFile("VERSION", []byte(fmt.Sprintf("%d\n", StoreSchemaVersion)), privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true}); err != nil {
		return fmt.Errorf("acf: write VERSION: %w", err)
	}
	return nil
}

// BlobsDir returns the absolute path to the store's content-addressed blob
// directory (<Root>/blobs/). The retention engine roots a
// blobstore.Store here.
func (s *Store) BlobsDir() string {
	return filepath.Join(s.Root, blobsDirName)
}

// kindDir returns the canonical directory name for an artifact kind.
// "memory" is special-cased because the English plural is "memories", not
// "memorys". All other kinds use simple pluralization via string(k)+"s".
func kindDir(k Kind) string {
	switch k {
	case KindMemory:
		return "memories"
	default:
		return string(k) + "s"
	}
}

func (s *Store) artifactPath(k Kind, id string) string {
	return filepath.Join(s.Root, "acf", kindDir(k), id+".json")
}

func artifactRel(k Kind, id string) string { return filepath.Join("acf", kindDir(k), id+".json") }
func eventsRel(k Kind, id string) string   { return filepath.Join("events", kindDir(k), id+".jsonl") }

func (s *Store) openRoot() (*privatefs.Root, error) {
	return privatefs.OpenRoot(s.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
}

func (s *Store) openRead(rel string) (*os.File, error) {
	var lastErr error
	for range privateReadRetryCount {
		root, err := s.openRoot()
		if err != nil {
			return nil, err
		}
		// Current-version files are already protected at creation. Validate the
		// single retained read handle first so a concurrent atomic replacement
		// does not add a second ACL-repair handle (and a much larger race
		// window) to every ordinary read. Only legacy permission failures enter
		// the retained-handle repair path.
		f, err := root.OpenReadRegular(rel)
		if err != nil && !retryablePrivateRead(err) {
			f, err = root.OpenReadRegularRepair(rel)
		}
		closeErr := root.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			return f, nil
		}
		if f != nil {
			_ = f.Close()
		}
		lastErr = err
		if !retryablePrivateRead(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func retryablePrivateRead(err error) bool {
	return errors.Is(err, privatefs.ErrOpenedFileUnlinked) ||
		errors.Is(err, privatefs.ErrUnsafeFileIdentity) ||
		errors.Is(err, privatefs.ErrNodeIdentityChanged)
}

func (s *Store) readPrivateRel(rel string, max int64) ([]byte, error) {
	f, err := s.openRead(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("acf: private sidecar exceeds limit")
	}
	return b, nil
}
func (s *Store) writePrivateRel(rel string, data []byte) error {
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	if err = root.EnsureDir(filepath.Dir(rel), privatefs.DirPolicy{Access: privatefs.AccessPrivate, AllowExisting: true}); err != nil {
		return err
	}
	return root.WriteFile(rel, data, privatefs.FilePolicy{RejectWritableByOthers: true, PreserveStricter: true})
}
func (s *Store) removePrivateRel(rel string) error {
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	err = root.RemoveRegular(rel)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func (s *Store) readPrivateDir(rel string) ([]fs.DirEntry, error) {
	root, err := s.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := root.ReadDir(rel)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return entries, err
}

func (s *Store) eventsPath(k Kind, id string) string {
	return filepath.Join(s.Root, "events", kindDir(k), id+".jsonl")
}

// WriteArtifact writes the Artifact JSON, overwriting any prior version.
// The headEventHash field is intentionally writeable via this method — the
// caller is responsible for keeping it consistent with AppendEvent.
func (s *Store) WriteArtifact(a Artifact) error {
	if err := ValidateKind(a.Kind); err != nil {
		return err
	}
	if a.Scope == ScopeNamespace {
		if err := ValidateWireUUIDv7(a.NamespaceID); err != nil {
			return fmt.Errorf("acf: namespace artifact identity: %w", err)
		}
	} else if a.NamespaceID != "" {
		return fmt.Errorf("acf: namespaceId is valid only for namespace scope")
	}
	if err := privatefs.EnsureDir(s.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return err
	}
	lock, err := filelock.Acquire(filepath.Join(s.Root, ".restore.lock"), 10*time.Second)
	if err != nil {
		return fmt.Errorf("acf: restore coordination lock: %w", err)
	}
	defer lock.Close()
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("acf: marshal artifact: %w", err)
	}
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	rel := artifactRel(a.Kind, a.ArtifactID)
	if err := root.EnsureDir(filepath.Dir(rel), privatefs.DirPolicy{Access: privatefs.AccessPrivate, AllowExisting: true}); err != nil {
		return fmt.Errorf("acf: mkdir for artifact %s/%s: %w", a.Kind, a.ArtifactID, err)
	}
	if err := root.WriteFile(rel, b, privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true, PreserveStricter: true}); err != nil {
		return fmt.Errorf("acf: write artifact %s/%s: %w", a.Kind, a.ArtifactID, err)
	}
	return nil
}

// ReadArtifact loads a single artifact by kind + id.
func (s *Store) ReadArtifact(k Kind, id string) (Artifact, error) {
	if err := ValidateKind(k); err != nil {
		return Artifact{}, err
	}
	var a Artifact
	f, err := s.openRead(artifactRel(k, id))
	if err != nil {
		return a, fmt.Errorf("acf: read artifact %s/%s: %w", k, id, err)
	}
	b, err := io.ReadAll(io.LimitReader(f, scanBufMax+1))
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return a, fmt.Errorf("acf: read artifact %s/%s: %w", k, id, err)
	}
	if len(b) > scanBufMax {
		return a, fmt.Errorf("acf: artifact exceeds read limit")
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return a, fmt.Errorf("acf: unmarshal artifact %s/%s: %w", k, id, err)
	}
	return a, nil
}

// normalizeBranch maps the wire-empty Branch field to MainBranch ("main").
// Pre-v0.95.0 events carry no Branch field — they all belong to main.
func normalizeBranch(b string) string {
	if b == "" {
		return MainBranch
	}
	return b
}

// AppendEvent enforces a per-branch ParentHash chain: e.ParentHash must
// equal the branch head before append. Standard events live on
// normalizeBranch(e.Branch); fork events introduce a NEW branch with no
// prior head and instead require e.ParentHash to reference some event
// already present in the artifact's history.
//
// On success, it computes and stores e.Hash, appends the event JSON line,
// and updates the artifact's branch-head bookkeeping.
// ErrHeadMismatch is returned by AppendEvent when a non-fork event's
// ParentHash does not match the current head of its branch — i.e. the event
// does not continue the local chain. Callers reconciling a REMOTE event whose
// causal history this device is missing (a sync gap) can detect this with
// errors.Is and rebase onto the inbound full-state snapshot instead of failing.
var ErrHeadMismatch = errors.New("acf: append rejected: ParentHash does not match branch head")

func (s *Store) AppendEvent(k Kind, e Event) error {
	return s.appendEvent(k, e, "", "", false)
}

// AppendEventWithRefreshedParent appends an event whose ParentHash was
// resolved from the event-log tail immediately before this call. On the rare
// legacy shape where artifact head bookkeeping is stale, appendEvent may
// accept that refreshed log head while holding the per-artifact lock. A real
// concurrent append still changes the log head and is rejected normally.
func (s *Store) AppendEventWithRefreshedParent(k Kind, e Event) error {
	return s.appendEvent(k, e, "", "", true)
}

// AppendEventWithMaterializedBranch is AppendEvent plus one metadata update
// applied to the freshly re-read Artifact while the per-artifact append lock is
// still held. Conversation continuation importers use it to record which branch
// an agent materialized without a separate stale ReadArtifact/WriteArtifact
// window that could overwrite a concurrently appended head.
//
// An empty agent leaves MaterializedBranchByAgent unchanged.
func (s *Store) AppendEventWithMaterializedBranch(k Kind, e Event, agent, materializedBranch string) error {
	return s.appendEvent(k, e, agent, materializedBranch, true)
}

func (s *Store) appendEvent(
	k Kind,
	e Event,
	materializedAgent, materializedBranch string,
	allowStaleBookkeepingRepair bool,
) error {
	// Last-resort admission gate (FR-03.21). Consulted before any validation
	// or write so a refused ingest is a pure no-op on the store. nil = always
	// allow (the default; existing callers/tests are unaffected).
	if s.IngestGate != nil {
		if err := s.IngestGate(); err != nil {
			return err
		}
	}
	unlock := s.lockArtifactAppend(k, e.ArtifactID)
	defer unlock()

	//
	// A baseline event MUST name the origin head it aligns to: the head
	// bookkeeping below is re-pointed at AlignedHead, so an empty value
	// would corrupt the artifact's head to "". Refused before any write.
	if e.Type == EventTypeBaseline {
		switch {
		case e.AlignedHead == "":
			return fmt.Errorf("acf: baseline append rejected: empty alignedHead (artifact %s, event %s)",
				e.ArtifactID, e.EventID)
		case e.AlignedEventID == "":
			return fmt.Errorf("acf: baseline append rejected: empty alignedEventId (artifact %s, event %s)",
				e.ArtifactID, e.EventID)
		case !HasPayload(e.Payload):
			return fmt.Errorf("acf: baseline append rejected: no full-state payload (artifact %s, event %s)",
				e.ArtifactID, e.EventID)
		}
	}
	branch := normalizeBranch(e.Branch)
	isFork := e.Type == EventTypeForkOuter
	if isFork {
		// First event on a new branch: branch must be non-empty and
		// distinct from any branch that already has events, and the
		// parent hash must reference an existing event in the artifact.
		if branch == MainBranch {
			return fmt.Errorf("acf: fork rejected: cannot fork onto main branch")
		}
		head, herr := s.HeadHashByBranch(k, e.ArtifactID, branch)
		if herr != nil {
			return herr
		}
		if head != "" {
			return fmt.Errorf("acf: fork rejected: branch %q already has events", branch)
		}
		ok, ferr := s.HasEventHash(k, e.ArtifactID, e.ParentHash)
		if ferr != nil {
			return ferr
		}
		if !ok {
			return fmt.Errorf("acf: fork rejected: parent hash %q not found in artifact history", e.ParentHash)
		}
	} else {
		head, herr := s.headHashForAppend(k, e.ArtifactID, branch, e.ParentHash)
		if herr != nil {
			return herr
		}
		if allowStaleBookkeepingRepair && e.ParentHash != head {
			refreshed, rerr := s.refreshedHeadHashFromLog(k, e.ArtifactID, branch)
			if rerr != nil {
				return rerr
			}
			if refreshed == e.ParentHash {
				head = refreshed
			}
		}
		if e.ParentHash != head {
			return fmt.Errorf("%w: ParentHash %q vs head of branch %q (%q)",
				ErrHeadMismatch, e.ParentHash, branch, head)
		}
	}
	computed, err := ComputeHash(e)
	if err != nil {
		return err
	}
	e.Hash = computed

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("acf: marshal event: %w", err)
	}
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	rel := eventsRel(k, e.ArtifactID)
	if err := root.EnsureDir(filepath.Dir(rel), privatefs.DirPolicy{Access: privatefs.AccessPrivate, AllowExisting: true}); err != nil {
		return fmt.Errorf("acf: mkdir for events %s/%s: %w", k, e.ArtifactID, err)
	}
	f, err := root.OpenAppendRegular(rel)
	if err != nil {
		return fmt.Errorf("acf: open events file: %w", err)
	}
	fileOpen := true
	defer func() {
		if fileOpen {
			_ = f.Close()
		}
	}()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("acf: write event: %w", err)
	}
	fileSync := f.Sync
	if s.eventFileSync != nil {
		fileSync = func() error { return s.eventFileSync(f) }
	}
	if err := fileSync(); err != nil {
		return fmt.Errorf("acf: sync event: %w", err)
	}
	fileClose := f.Close
	if s.eventFileClose != nil {
		fileClose = func() error { return s.eventFileClose(f) }
	}
	if err := fileClose(); err != nil {
		return fmt.Errorf("acf: close event: %w", err)
	}
	fileOpen = false
	dirSync := root.SyncDir
	if s.eventDirSync != nil {
		dirSync = func(dir string) error { return s.eventDirSync(root, dir) }
	}
	// EnsureDir durably links every newly-created path component through its
	// parent. Sync the existing leaf once more after the append so the first
	// event file's directory entry is stable before artifact metadata can
	// advertise the new head.
	if dir := filepath.Dir(rel); dir != "." {
		if err := dirSync(dir); err != nil {
			return fmt.Errorf("acf: sync canonical directory %s: %w", dir, err)
		}
	}
	s.cacheAppendedEventID(k, e.ArtifactID, e.EventID)

	a, err := s.ReadArtifact(k, e.ArtifactID)
	if err != nil {
		return err
	}
	// This is append metadata, not a source of chain authority. Legacy
	// artifacts omitted the field and begin a fresh count at their first
	// post-upgrade append; new artifacts remain exact from their create event.
	// Either way, callers get a stable O(1) cadence without rereading a
	// potentially multi-gigabyte JSONL log after every append.
	a.EventCount++
	// A baseline event re-aligns the chain (aligned-chains design rule): the
	// head bookkeeping is set to the ORIGIN head hash it names — NOT to the
	// baseline's own hash — so subsequent verbatim origin events chain
	// natively. VerifyChain applies the identical per-branch reset.
	headHash := e.Hash
	if e.Type == EventTypeBaseline {
		headHash = e.AlignedHead
	}
	if branch == MainBranch {
		a.HeadEventHash = headHash
		a.UpdatedAt = e.Timestamp
	}
	if a.BranchHeads == nil {
		a.BranchHeads = map[string]string{}
	}
	a.BranchHeads[branch] = headHash
	// Tombstone tracking applies to the main branch only — non-main
	// branches do not affect the artifact's primary materialization
	// state. A redaction on a side branch is just a side-branch redaction.
	if branch == MainBranch {
		switch e.Type {
		case EventTypeRedaction:
			a.Tombstoned = true
		case EventTypeCreate, EventTypeUpdate, EventTypeResolution, EventTypeBaseline:
			// EventTypeResolution (v0.34.0) re-asserts a winning payload
			// and must clear any tombstone the same way create/update
			// does. EventTypeBaseline re-asserts the full origin state and
			// clears a tombstone identically.
			a.Tombstoned = false
		}
	}
	if materializedAgent != "" {
		if a.MaterializedBranchByAgent == nil {
			a.MaterializedBranchByAgent = map[string]string{}
		}
		a.MaterializedBranchByAgent[materializedAgent] = materializedBranch
	}
	// MergeConversationByThreadRef historically touched UpdatedAt for both main
	// and side-branch continuations. Keep that presentation behavior inside the
	// same locked metadata write instead of restoring a separate pre-append write.
	if materializedBranch != "" {
		a.UpdatedAt = e.Timestamp
	}
	return s.WriteArtifact(a)
}

// ConfirmEventDurableAndRepairMetadata is the recovery half of a durable
// inbound dedupe. A prior AppendEvent may have made bytes visible and then
// failed its file/close/directory barrier (or crashed before artifact metadata
// was written). Such bytes are not terminal evidence until this method
// reopens the canonical log, binds the exact event identity, repeats every
// durability barrier, and rebuilds append metadata from the verified log.
//
// The per-artifact append lock keeps the scanned log and repaired metadata in
// one local transaction. If a newer event won the lock first, repair follows
// that newer canonical tail and never rewinds the artifact to wantedEventID.
func (s *Store) ConfirmEventDurableAndRepairMetadata(k Kind, id, wantedEventID, wantedEventHash string) (Event, error) {
	if err := ValidateKind(k); err != nil {
		return Event{}, err
	}
	if err := ValidateWireUUIDv7(id); err != nil {
		return Event{}, err
	}
	if err := ValidateWireEventID(wantedEventID); err != nil || len(wantedEventHash) != sha256.Size*2 {
		return Event{}, fmt.Errorf("acf: invalid durable event identity")
	}
	unlock := s.lockArtifactAppend(k, id)
	defer unlock()

	root, err := s.openRoot()
	if err != nil {
		return Event{}, err
	}
	defer root.Close()
	rel := eventsRel(k, id)
	f, err := root.OpenAppendRegular(rel)
	if err != nil {
		return Event{}, fmt.Errorf("acf: open events for durable confirmation: %w", err)
	}
	fileOpen := true
	defer func() {
		if fileOpen {
			_ = f.Close()
		}
	}()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Event{}, fmt.Errorf("acf: seek events for durable confirmation: %w", err)
	}
	var events []Event
	var matched Event
	matches := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, scanBufInit), scanBufMax)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			return Event{}, fmt.Errorf("acf: parse events for durable confirmation: %w", err)
		}
		if event.ArtifactID != id {
			return Event{}, fmt.Errorf("acf: durable confirmation artifact mismatch")
		}
		if event.EventID == wantedEventID {
			matches++
			matched = event
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return Event{}, fmt.Errorf("acf: scan events for durable confirmation: %w", err)
	}
	if matches != 1 || matched.Hash != wantedEventHash {
		return Event{}, fmt.Errorf("acf: durable event identity mismatch")
	}
	if err := VerifyChain(events); err != nil {
		return Event{}, fmt.Errorf("acf: durable confirmation chain invalid: %w", err)
	}
	fileSync := f.Sync
	if s.eventFileSync != nil {
		fileSync = func() error { return s.eventFileSync(f) }
	}
	if err := fileSync(); err != nil {
		return Event{}, fmt.Errorf("acf: sync confirmed event log: %w", err)
	}
	fileClose := f.Close
	if s.eventFileClose != nil {
		fileClose = func() error { return s.eventFileClose(f) }
	}
	if err := fileClose(); err != nil {
		return Event{}, fmt.Errorf("acf: close confirmed event log: %w", err)
	}
	fileOpen = false
	dirSync := root.SyncDir
	if s.eventDirSync != nil {
		dirSync = func(dir string) error { return s.eventDirSync(root, dir) }
	}
	if dir := filepath.Dir(rel); dir != "." {
		if err := dirSync(dir); err != nil {
			return Event{}, fmt.Errorf("acf: sync confirmed event directory %s: %w", dir, err)
		}
	}

	artifact, err := s.ReadArtifact(k, id)
	if err != nil {
		return Event{}, err
	}
	repairArtifactMetadataFromEvents(&artifact, events)
	if err := s.WriteArtifact(artifact); err != nil {
		return Event{}, fmt.Errorf("acf: repair confirmed event metadata: %w", err)
	}
	s.cacheAppendedEventID(k, id, wantedEventID)
	return matched, nil
}

func repairArtifactMetadataFromEvents(artifact *Artifact, events []Event) {
	artifact.EventCount = uint64(len(events))
	artifact.HeadEventHash = ""
	artifact.BranchHeads = make(map[string]string)
	artifact.Tombstoned = false
	for _, event := range events {
		branch := normalizeBranch(event.Branch)
		head := event.Hash
		if event.Type == EventTypeBaseline {
			head = event.AlignedHead
		}
		artifact.BranchHeads[branch] = head
		if branch != MainBranch {
			continue
		}
		artifact.HeadEventHash = head
		artifact.UpdatedAt = event.Timestamp
		switch event.Type {
		case EventTypeRedaction:
			artifact.Tombstoned = true
		case EventTypeCreate, EventTypeUpdate, EventTypeResolution, EventTypeBaseline:
			artifact.Tombstoned = false
		}
	}
}

// refreshedHeadHashFromLog returns the branch head adapters observe when they
// repair stale artifact bookkeeping. A baseline is a virtual reset, so its
// aligned origin hash—not the local wrapper hash—is authoritative.
func (s *Store) refreshedHeadHashFromLog(k Kind, id, branch string) (string, error) {
	branch = normalizeBranch(branch)
	f, err := s.openRead(eventsRel(k, id))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("acf: open refreshed branch head: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("acf: stat refreshed branch head: %w", err)
	}
	end := st.Size()
	for end > 0 {
		line, nextEnd, ok, rerr := readPreviousNonEmptyLine(f, end)
		if rerr != nil {
			return "", fmt.Errorf("acf: read refreshed branch head %s/%s: %w", k, id, rerr)
		}
		end = nextEnd
		if !ok {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			return "", fmt.Errorf("acf: parse refreshed branch head event: %w", err)
		}
		if normalizeBranch(event.Branch) != branch {
			continue
		}
		if event.Type == EventTypeBaseline {
			return event.AlignedHead, nil
		}
		return event.Hash, nil
	}
	return "", nil
}

const appendLockStripeCount = 64

func (s *Store) lockArtifactAppend(k Kind, artifactID string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(k))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(artifactID))
	stripe := &s.appendLocks[h.Sum32()%appendLockStripeCount]
	stripe.Lock()
	return stripe.Unlock
}

// AdoptBaseline preserves the original main-branch API. Branch-aware recovery
// uses AdoptBranchBaseline below; keeping this wrapper strict prevents an old
// caller from changing branch semantics merely by setting Event.Branch.
func (s *Store) AdoptBaseline(k Kind, ev Event) error {
	if normalizeBranch(ev.Branch) != MainBranch {
		return fmt.Errorf("acf: adopt baseline rejected: branch %q — use branch-baseline adoption for non-main recovery (artifact %s, event %s)",
			ev.Branch, ev.ArtifactID, ev.EventID)
	}
	return s.AdoptBranchBaseline(k, ev)
}

// AdoptBranchBaseline appends ev (Type=baseline, full payload,
// AlignedHead/AlignedEventID set) onto the current local head of the exact
// branch, then points only that branch's head bookkeeping at AlignedHead so
// subsequent verbatim origin deltas chain natively. For main it also updates
// HeadEventHash through AppendEvent's established baseline semantics.
//
// A branch checkpoint is self-contained: a fresh receiver may have no fork
// ancestry for that branch. In that case the baseline is its local branch
// genesis (ParentHash=""); branch projection treats this authenticated full
// state as a virtual recovery root. Existing local branch history remains in
// the append log, so adoption is lossless.
//
// The baseline's own Hash is computed normally by AppendEvent (its hash is
// NOT the head — the head bookkeeping lands on AlignedHead, which AppendEvent
// applies for EventTypeBaseline). The append is subject to IngestGate like
// every other write.
func (s *Store) AdoptBranchBaseline(k Kind, ev Event) error {
	if ev.Type != EventTypeBaseline {
		return fmt.Errorf("acf: adopt baseline rejected: event type %q is not %q", ev.Type, EventTypeBaseline)
	}
	if ev.ArtifactID == "" {
		return fmt.Errorf("acf: adopt baseline rejected: empty artifact id (event %s)", ev.EventID)
	}
	if ev.AlignedHead == "" {
		return fmt.Errorf("acf: adopt baseline rejected: empty alignedHead (artifact %s, event %s)",
			ev.ArtifactID, ev.EventID)
	}
	if ev.AlignedEventID == "" {
		return fmt.Errorf("acf: adopt baseline rejected: empty alignedEventId (artifact %s, event %s)",
			ev.ArtifactID, ev.EventID)
	}
	if !HasPayload(ev.Payload) {
		return fmt.Errorf("acf: adopt baseline rejected: no payload (artifact %s, event %s) — a baseline is a full-state checkpoint",
			ev.ArtifactID, ev.EventID)
	}
	branch, branchErr := NormalizeBranchName(normalizeBranch(ev.Branch))
	if branchErr != nil {
		return fmt.Errorf("acf: adopt baseline rejected: invalid branch %q: %w", ev.Branch, branchErr)
	}
	ev.Branch = branch

	// Current local head: artifact bookkeeping first, falling back to the
	// branch log when bookkeeping is empty. "" is valid on first contact.
	var head string
	if a, aerr := s.ReadArtifact(k, ev.ArtifactID); aerr == nil {
		if branch == MainBranch {
			head = a.HeadEventHash
		}
		if a.BranchHeads != nil {
			if h := a.BranchHeads[branch]; h != "" {
				head = h
			}
		}
	} else {
		// First inbound contact: mint a minimal artifact shell so
		// AppendEvent's ReadArtifact + head bookkeeping has a record to
		// update (mirrors the sync layer's inbound shell mint).
		now := ev.Timestamp
		if now.IsZero() {
			now = time.Now().UTC()
		}
		shell := Artifact{
			AcfSchemaVersion: SchemaVersion,
			ArtifactID:       ev.ArtifactID,
			Kind:             k,
			Scope:            ScopeGlobal,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if werr := s.WriteArtifact(shell); werr != nil {
			return fmt.Errorf("acf: adopt baseline: write artifact shell: %w", werr)
		}
	}
	if head == "" {
		var herr error
		head, herr = s.HeadHashByBranch(k, ev.ArtifactID, branch)
		if herr != nil {
			return herr
		}
	}

	ev.ParentHash = head
	return s.AppendEvent(k, ev)
}

func (s *Store) headHashForAppend(k Kind, id, branch, _ string) (string, error) {
	branch = normalizeBranch(branch)
	if a, err := s.ReadArtifact(k, id); err == nil {
		var head string
		if branch == MainBranch {
			head = a.HeadEventHash
		}
		if a.BranchHeads != nil {
			if h := a.BranchHeads[branch]; h != "" {
				head = h
			}
		}
		// Artifact bookkeeping is the chain authority whenever it contains a
		// head. In particular, AdoptBaseline deliberately points it at the
		// origin's aligned head, which need not be the hash of any local JSONL
		// line. Falling through on a parent mismatch both substituted the local
		// baseline wrapper's hash for that authority and replayed an entire
		// multi-gigabyte conversation merely to reject an out-of-order delta.
		if head != "" {
			return head, nil
		}
	}
	return s.HeadHashByBranch(k, id, branch)
}

// HeadHashByBranch returns the Hash of the most recent event on the named
// branch (empty branch is treated as MainBranch), or "" if the branch has no
// events. It scans backward and stops at the first matching event instead of
// materializing the entire log. Pre-v0.95.0 events with no Branch field are
// considered part of the main branch.
func (s *Store) HeadHashByBranch(k Kind, id, branch string) (string, error) {
	branch = normalizeBranch(branch)
	f, err := s.openRead(eventsRel(k, id))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("acf: open events file: %w", err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("acf: stat events file: %w", err)
	}
	end := st.Size()
	for end > 0 {
		line, nextEnd, ok, rerr := readPreviousNonEmptyLine(f, end)
		if rerr != nil {
			return "", fmt.Errorf("acf: read branch head %s/%s: %w", k, id, rerr)
		}
		end = nextEnd
		if !ok {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			return "", fmt.Errorf("acf: parse branch head event: %w", err)
		}
		if normalizeBranch(event.Branch) == branch {
			return event.Hash, nil
		}
	}
	return "", nil
}

// ReadEventsByBranch returns the events for an artifact filtered to a
// single branch in append order. Pre-v0.95.0 events (empty Branch) are
// surfaced when branch == MainBranch.
func (s *Store) ReadEventsByBranch(k Kind, id, branch string) ([]Event, error) {
	branch = normalizeBranch(branch)
	events, err := s.ReadEvents(k, id)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(events))
	for _, e := range events {
		if normalizeBranch(e.Branch) == branch {
			out = append(out, e)
		}
	}
	return out, nil
}

// ListBranchNames returns the unique branch names observed in an
// artifact's event log, sorted with MainBranch first.
func (s *Store) ListBranchNames(k Kind, id string) ([]string, error) {
	events, err := s.ReadEvents(k, id)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{MainBranch: {}}
	for _, e := range events {
		seen[normalizeBranch(e.Branch)] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	out = append(out, MainBranch)
	for b := range seen {
		if b == MainBranch {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out[1:], func(i, j int) bool { return out[1+i] < out[1+j] })
	return out, nil
}

// HasEventHash reports whether the given event hash appears anywhere in
// the artifact's log, on any branch. Used by AppendEvent to validate the
// parent of a fork event.
func (s *Store) HasEventHash(k Kind, id, hash string) (bool, error) {
	if hash == "" {
		return false, nil
	}
	events, err := s.ReadEvents(k, id)
	if err != nil {
		return false, err
	}
	for _, e := range events {
		if e.Hash == hash {
			return true, nil
		}
	}
	return false, nil
}

// HeadHash returns the Hash of the latest event in the artifact's log, or
// "" if no events exist yet.
func (s *Store) HeadHash(k Kind, id string) (string, error) {
	f, err := s.openRead(eventsRel(k, id))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("acf: open events file: %w", err)
	}
	defer f.Close()

	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, scanBufInit), scanBufMax) // 4MB max line for base64 inlining
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return "", fmt.Errorf("acf: parse event: %w", err)
		}
		last = e.Hash
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("acf: scan events: %w", err)
	}
	return last, nil
}

// LastEvent returns the last non-empty JSONL event in the artifact log without
// replaying the whole file. It is intended for hot paths that need only the
// current append-order head, such as unchanged-content guards and outbound
// publication. Branch-specific hot paths should use LastEventByBranch.
func (s *Store) LastEvent(k Kind, id string) (Event, bool, error) {
	var zero Event
	f, err := s.openRead(eventsRel(k, id))
	if errors.Is(err, os.ErrNotExist) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, fmt.Errorf("acf: open events file: %w", err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return zero, false, fmt.Errorf("acf: stat events file: %w", err)
	}
	if st.Size() == 0 {
		return zero, false, nil
	}
	line, _, ok, err := readPreviousNonEmptyLine(f, st.Size())
	if err != nil {
		return zero, false, fmt.Errorf("acf: read events tail: %w", err)
	}
	if !ok {
		return zero, false, nil
	}
	return parseLastEventLine(line)
}

// LastEventByBranch returns the last actual JSONL event on one branch without
// materializing the artifact or projecting fork ancestry. It scans backward
// and stops at the first matching branch, preserving the O(new turn) sync hot
// path when append-order tail belongs to another branch.
func (s *Store) LastEventByBranch(k Kind, id, branch string) (Event, bool, error) {
	var zero Event
	branch = normalizeBranch(branch)
	f, err := s.openRead(eventsRel(k, id))
	if errors.Is(err, os.ErrNotExist) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, fmt.Errorf("acf: open events file: %w", err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return zero, false, fmt.Errorf("acf: stat events file: %w", err)
	}
	end := st.Size()
	for end > 0 {
		line, nextEnd, ok, rerr := readPreviousNonEmptyLine(f, end)
		if rerr != nil {
			return zero, false, fmt.Errorf("acf: read branch tail %s/%s: %w", k, id, rerr)
		}
		end = nextEnd
		if !ok {
			continue
		}
		event, parsed, perr := parseLastEventLine(line)
		if perr != nil {
			return zero, false, perr
		}
		if parsed && normalizeBranch(event.Branch) == branch {
			return event, true, nil
		}
	}
	return zero, false, nil
}

// HasEventID reports whether eventID is already present in an artifact's
// active JSONL log. The first lookup for an artifact builds an in-memory id-only
// index with bounded buffers; event payloads are never unmarshaled or retained.
// AppendEvent keeps an already-loaded index current for the life of the Store.
func (s *Store) HasEventID(k Kind, id, eventID string) (bool, error) {
	if eventID == "" {
		return false, nil
	}
	key := eventIDIndexKey(k, id)
	s.eventIDIndexMu.Lock()
	defer s.eventIDIndexMu.Unlock()
	if s.eventIDIndex != nil {
		if index, ok := s.eventIDIndex[key]; ok {
			_, found := index[eventID]
			return found, nil
		}
	}
	index, err := s.loadEventIDIndex(k, id)
	if err != nil {
		return false, err
	}
	if s.eventIDIndex == nil {
		s.eventIDIndex = make(map[string]map[string]struct{})
	}
	s.eventIDIndex[key] = index
	_, found := index[eventID]
	return found, nil
}

const eventIDPrefixMaxBytes = 64 * 1024

const eventIdentitySuffixMaxBytes = 1 << 20

func (s *Store) loadEventIDIndex(k Kind, id string) (map[string]struct{}, error) {
	index := make(map[string]struct{})
	f, err := s.openRead(eventsRel(k, id))
	if errors.Is(err, os.ErrNotExist) {
		return index, nil
	}
	if err != nil {
		return nil, fmt.Errorf("acf: open events for id index: %w", err)
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, scanBufInit)
	prefix := make([]byte, 0, 256)
	for {
		fragment, readErr := reader.ReadSlice('\n')
		if len(prefix) < eventIDPrefixMaxBytes {
			remaining := eventIDPrefixMaxBytes - len(prefix)
			if len(fragment) > remaining {
				fragment = fragment[:remaining]
			}
			prefix = append(prefix, fragment...)
		}
		lineDone := !errors.Is(readErr, bufio.ErrBufferFull)
		if lineDone {
			trimmed := bytes.TrimSpace(prefix)
			if len(trimmed) > 0 {
				eventID, found, parseErr := eventIDFromJSONPrefix(trimmed)
				if parseErr != nil {
					return nil, fmt.Errorf("acf: parse event id index %s/%s: %w", k, id, parseErr)
				}
				if !found {
					return nil, fmt.Errorf("acf: event id is absent from first %d bytes of %s/%s", eventIDPrefixMaxBytes, k, id)
				}
				index[eventID] = struct{}{}
			}
			prefix = prefix[:0]
		}
		switch {
		case readErr == nil, errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			return index, nil
		default:
			return nil, fmt.Errorf("acf: read events for id index: %w", readErr)
		}
	}
}

// FindRecentEventIdentity searches a bounded suffix of the active event log
// for one exact EventID and returns only its persisted id/hash identity. It
// reads fixed-size JSON prefix/suffix windows and never materializes Payload,
// which makes it suitable for durable-receipt verification on very large
// conversations. maxBytes is a scan budget across completed lines; locating a
// line boundary may exceed it by at most one scanBufMax-sized line, after which
// the search stops. Compacted history is deliberately excluded: callers must
// fail closed instead of expanding a hot receipt path into an unbounded gzip
// replay.
func (s *Store) FindRecentEventIdentity(k Kind, id, eventID string, maxEvents int, maxBytes int64) (Event, bool, error) {
	if eventID == "" || maxEvents <= 0 || maxBytes <= 0 {
		return Event{}, false, nil
	}
	f, err := s.openRead(eventsRel(k, id))
	if errors.Is(err, os.ErrNotExist) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("acf: open events for recent identity: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return Event{}, false, fmt.Errorf("acf: stat events for recent identity: %w", err)
	}
	end := stat.Size()
	var scannedBytes int64
	for scannedEvents := 0; end > 0 && scannedEvents < maxEvents && scannedBytes < maxBytes; scannedEvents++ {
		lineStart, lineEnd, nextEnd, ok, readErr := previousNonEmptyLineBounds(f, end)
		if readErr != nil {
			return Event{}, false, fmt.Errorf("acf: read recent event identity %s/%s: %w", k, id, readErr)
		}
		end = nextEnd
		if !ok {
			continue
		}
		lineLen := lineEnd - lineStart
		scannedBytes += lineLen
		prefixLen := lineLen
		if prefixLen > eventIDPrefixMaxBytes {
			prefixLen = eventIDPrefixMaxBytes
		}
		prefix := make([]byte, int(prefixLen))
		if _, readErr := f.ReadAt(prefix, lineStart); readErr != nil && !errors.Is(readErr, io.EOF) {
			return Event{}, false, fmt.Errorf("acf: read recent event id prefix: %w", readErr)
		}
		foundID, found, parseErr := eventIDFromJSONPrefix(bytes.TrimSpace(prefix))
		if parseErr != nil || !found {
			if parseErr == nil {
				parseErr = errors.New("event id absent")
			}
			return Event{}, false, fmt.Errorf("acf: parse recent event id: %w", parseErr)
		}
		if foundID != eventID {
			continue
		}
		suffixLen := lineLen
		if suffixLen > eventIdentitySuffixMaxBytes {
			suffixLen = eventIdentitySuffixMaxBytes
		}
		suffix := make([]byte, int(suffixLen))
		if _, readErr := f.ReadAt(suffix, lineEnd-suffixLen); readErr != nil && !errors.Is(readErr, io.EOF) {
			return Event{}, false, fmt.Errorf("acf: read recent event identity suffix: %w", readErr)
		}
		hash, hashErr := eventHashFromCanonicalJSONSuffix(suffix)
		if hashErr != nil {
			return Event{}, false, fmt.Errorf("acf: parse recent event hash: %w", hashErr)
		}
		return Event{EventID: foundID, Hash: hash}, true, nil
	}
	return Event{}, false, nil
}

func eventHashFromCanonicalJSONSuffix(suffix []byte) (string, error) {
	marker := []byte(`,"hash":"`)
	index := bytes.LastIndex(suffix, marker)
	if index < 0 {
		return "", errors.New("hash field absent")
	}
	start := index + len(marker)
	end := start + sha256.Size*2
	if end >= len(suffix) || suffix[end] != '"' {
		return "", errors.New("invalid hash field")
	}
	for _, value := range suffix[start:end] {
		if (value < '0' || value > '9') && (value < 'a' || value > 'f') {
			return "", errors.New("invalid hash digest")
		}
	}
	return string(suffix[start:end]), nil
}

func eventIDFromJSONPrefix(prefix []byte) (string, bool, error) {
	marker := []byte(`"eventId"`)
	start := bytes.Index(prefix, marker)
	if start < 0 {
		return "", false, nil
	}
	i := start + len(marker)
	for i < len(prefix) && (prefix[i] == ' ' || prefix[i] == '\t' || prefix[i] == '\r') {
		i++
	}
	if i >= len(prefix) || prefix[i] != ':' {
		return "", false, fmt.Errorf("malformed eventId field")
	}
	i++
	for i < len(prefix) && (prefix[i] == ' ' || prefix[i] == '\t' || prefix[i] == '\r') {
		i++
	}
	if i >= len(prefix) || prefix[i] != '"' {
		return "", false, fmt.Errorf("eventId is not a JSON string")
	}
	end := bytes.IndexByte(prefix[i+1:], '"')
	if end < 0 {
		return "", false, fmt.Errorf("unterminated eventId string")
	}
	value := string(prefix[i+1 : i+1+end])
	if value == "" {
		return "", false, fmt.Errorf("empty eventId")
	}
	return value, true, nil
}

func (s *Store) cacheAppendedEventID(k Kind, id, eventID string) {
	if eventID == "" {
		return
	}
	key := eventIDIndexKey(k, id)
	s.eventIDIndexMu.Lock()
	if index, ok := s.eventIDIndex[key]; ok {
		index[eventID] = struct{}{}
	}
	s.eventIDIndexMu.Unlock()
}

func eventIDIndexKey(k Kind, id string) string {
	return string(k) + "\x00" + id
}

// previousNonEmptyLineBounds returns the byte range of the line ending before
// end plus the end offset for the next backward read. It finds boundaries in
// fixed chunks without materializing the line. Metadata-only readers use the
// range with an io.SectionReader so large payloads are never allocated or
// decoded.
func previousNonEmptyLineBounds(f *os.File, end int64) (int64, int64, int64, bool, error) {
	const chunkSize int64 = 1 << 20
	var one [1]byte
	for end > 0 {
		if _, err := f.ReadAt(one[:], end-1); err != nil {
			return 0, 0, end, false, err
		}
		if one[0] != '\n' && one[0] != '\r' {
			break
		}
		end--
	}
	if end == 0 {
		return 0, 0, 0, false, nil
	}
	lineEnd := end
	search := end
	lineStart := int64(0)
	nextEnd := int64(0)
	foundBoundary := false
	for search > 0 {
		start := search - chunkSize
		if start < 0 {
			start = 0
		}
		buf := make([]byte, search-start)
		if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return 0, 0, end, false, err
		}
		for i := len(buf) - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				lineStart = start + int64(i) + 1
				nextEnd = start + int64(i)
				foundBoundary = true
				break
			}
		}
		if foundBoundary {
			break
		}
		search = start
		if lineEnd-search > scanBufMax {
			return 0, 0, end, false, fmt.Errorf("event line exceeds %d bytes", scanBufMax)
		}
	}
	lineLen := lineEnd - lineStart
	if lineLen > scanBufMax {
		return 0, 0, end, false, fmt.Errorf("event line exceeds %d bytes", scanBufMax)
	}
	return lineStart, lineEnd, nextEnd, true, nil
}

// readPreviousNonEmptyLine returns the line ending before end plus the end
// offset for the next backward read. It allocates/copies the line exactly once.
// The former LastEvent code repeatedly prepended 1 MiB chunks; for a 165 MiB
// snapshot event that became quadratic (~13 GiB of memmoves) on a hot path.
func readPreviousNonEmptyLine(f *os.File, end int64) ([]byte, int64, bool, error) {
	lineStart, lineEnd, nextEnd, ok, err := previousNonEmptyLineBounds(f, end)
	if err != nil || !ok {
		return nil, nextEnd, ok, err
	}
	lineLen := lineEnd - lineStart
	line := make([]byte, int(lineLen))
	if _, err := f.ReadAt(line, lineStart); err != nil && !errors.Is(err, io.EOF) {
		return nil, end, false, err
	}
	return line, nextEnd, true, nil
}

// decodeEventHeader reads only the fields needed by metadata and activity
// surfaces. Canonical Event JSON always writes these fields before Payload, so
// returning as soon as Provenance is decoded avoids allocating or parsing a
// conversation snapshot that can be hundreds of megabytes.
//
// This is deliberately not an integrity verification API: callers requiring
// the payload or hash chain must continue to use ReadEvents/VerifyChain.
func decodeEventHeader(r io.Reader) (Event, error) {
	var event Event
	decoder := json.NewDecoder(r)
	token, err := decoder.Token()
	if err != nil {
		return event, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return event, fmt.Errorf("event is not a JSON object")
	}

	var (
		haveEventID    bool
		haveType       bool
		haveTimestamp  bool
		haveProvenance bool
	)
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return Event{}, err
		}
		key, ok := token.(string)
		if !ok {
			return Event{}, fmt.Errorf("event field name is not a string")
		}
		switch key {
		case "eventId":
			err = decoder.Decode(&event.EventID)
			haveEventID = err == nil
		case "type":
			err = decoder.Decode(&event.Type)
			haveType = err == nil
		case "timestamp":
			err = decoder.Decode(&event.Timestamp)
			haveTimestamp = err == nil
		case "provenance":
			err = decoder.Decode(&event.Provenance)
			haveProvenance = err == nil
		case "payload":
			return Event{}, fmt.Errorf("event header fields must precede payload")
		default:
			var ignored json.RawMessage
			err = decoder.Decode(&ignored)
		}
		if err != nil {
			return Event{}, err
		}
		if haveEventID && haveType && haveTimestamp && haveProvenance {
			return event, nil
		}
	}
	return Event{}, fmt.Errorf("event header is missing required fields")
}

// EventLogModTime returns the filesystem modification time for an artifact's
// active JSONL event log. A missing log is reported as a zero time without an
// error because an artifact shell can exist before any events are appended.
func (s *Store) EventLogModTime(k Kind, id string) (time.Time, error) {
	f, err := s.openRead(eventsRel(k, id))
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("acf: stat events file: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return time.Time{}, fmt.Errorf("acf: stat events file: %w", err)
	}
	return st.ModTime(), nil
}

// EventLogSize returns the current append-log size without reading event
// payloads. Callers use it to keep optional background maintenance from
// decoding a giant conversation merely to decide whether that maintenance is
// affordable. It is advisory only; security-sensitive reads still validate the
// selected event before use.
func (s *Store) EventLogSize(k Kind, id string) (int64, error) {
	f, err := s.openRead(eventsRel(k, id))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// ArtifactCatalogModTime returns the modification time of a kind's artifact
// directory. WriteArtifact replaces files atomically inside this directory, so
// its timestamp is a cheap generation cursor for pollers that otherwise would
// reopen every artifact JSON on every idle tick.
func (s *Store) ArtifactCatalogModTime(k Kind) (time.Time, error) {
	if err := ValidateKind(k); err != nil {
		return time.Time{}, err
	}
	path := filepath.Join(s.Root, "acf", kindDir(k))
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("acf: stat artifact catalog: %w", err)
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return time.Time{}, fmt.Errorf("acf: artifact catalog is not a directory")
	}
	return st.ModTime(), nil
}

func parseLastEventLine(line []byte) (Event, bool, error) {
	var zero Event
	for len(line) > 0 && (line[len(line)-1] == '\r' || line[len(line)-1] == '\n') {
		line = line[:len(line)-1]
	}
	if len(line) == 0 {
		return zero, false, nil
	}
	var e Event
	if err := json.Unmarshal(line, &e); err != nil {
		return zero, false, fmt.Errorf("acf: parse last event: %w", err)
	}
	return e, true, nil
}

// EventCount returns the number of non-empty JSONL event lines for an artifact
// without unmarshalling each event. It is for metadata paths that need a
// sequence/count but not event bodies.
func (s *Store) EventCount(k Kind, id string) (uint64, error) {
	f, err := s.openRead(eventsRel(k, id))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("acf: open events file: %w", err)
	}
	defer f.Close()

	var count uint64
	lineHasData := false
	buf := make([]byte, eventCountReadBufferLen)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			for _, b := range buf[:n] {
				switch b {
				case '\n':
					if lineHasData {
						count++
						lineHasData = false
					}
				case '\r':
				default:
					lineHasData = true
				}
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return 0, fmt.Errorf("acf: count events: %w", rerr)
		}
	}
	if lineHasData {
		count++
	}
	return count, nil
}

// ReadEvents returns all events for an artifact in append order.
func (s *Store) ReadEvents(k Kind, id string) ([]Event, error) {
	f, err := s.openRead(eventsRel(k, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("acf: open events file: %w", err)
	}
	defer f.Close()

	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, scanBufInit), scanBufMax)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("acf: parse event: %w", err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("acf: scan events %s/%s: %w", k, id, err)
	}
	return out, nil
}

// ReadRecentEvents returns at most limit matching events in append order while
// reading the JSONL log from its tail. When eventTypes is empty, every event
// type matches. This is the bounded alternative for code that only needs the
// newest event(s): using ReadEvents for that purpose makes a hot-path check
// replay an artifact's entire history and is especially expensive for long
// conversations.
func (s *Store) ReadRecentEvents(k Kind, id string, limit int, eventTypes ...EventType) ([]Event, error) {
	if limit <= 0 {
		return nil, nil
	}
	f, err := s.openRead(eventsRel(k, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("acf: open events file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("acf: stat events file: %w", err)
	}
	accepted := make(map[EventType]struct{}, len(eventTypes))
	for _, eventType := range eventTypes {
		accepted[eventType] = struct{}{}
	}

	// Collect newest-to-oldest, then reverse once so callers receive the same
	// chronological ordering as ReadEvents.
	reversed := make([]Event, 0, limit)
	end := fi.Size()
	for end > 0 && len(reversed) < limit {
		line, nextEnd, ok, rerr := readPreviousNonEmptyLine(f, end)
		if rerr != nil {
			return nil, fmt.Errorf("acf: read recent event %s/%s: %w", k, id, rerr)
		}
		end = nextEnd
		if !ok {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("acf: parse recent event: %w", err)
		}
		if len(accepted) > 0 {
			if _, match := accepted[event.Type]; !match {
				continue
			}
		}
		reversed = append(reversed, event)
	}

	out := make([]Event, len(reversed))
	for i := range reversed {
		out[len(reversed)-1-i] = reversed[i]
	}
	return out, nil
}

// ReadRecentEventHeaders returns at most limit event headers in append order
// without decoding Payload. beforeMillis is an exclusive upper timestamp
// bound; zero or negative starts from the newest event.
//
// The limit is soft when several events share the boundary millisecond: the
// complete same-millisecond group is returned so cursor-based callers cannot
// drop tied events between pages.
func (s *Store) ReadRecentEventHeaders(k Kind, id string, beforeMillis int64, limit int) ([]Event, error) {
	if limit <= 0 {
		return nil, nil
	}
	f, err := s.openRead(eventsRel(k, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("acf: open events file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("acf: stat events file: %w", err)
	}

	// Collect newest-to-oldest, then reverse once so callers receive the same
	// chronological ordering as ReadEvents.
	reversed := make([]Event, 0, limit)
	var boundaryMillis int64
	end := fi.Size()
	for end > 0 {
		lineStart, lineEnd, nextEnd, ok, rerr := previousNonEmptyLineBounds(f, end)
		if rerr != nil {
			return nil, fmt.Errorf("acf: read recent event header %s/%s: %w", k, id, rerr)
		}
		end = nextEnd
		if !ok {
			continue
		}
		event, parseErr := decodeEventHeader(io.NewSectionReader(f, lineStart, lineEnd-lineStart))
		if parseErr != nil {
			return nil, fmt.Errorf("acf: parse recent event header: %w", parseErr)
		}
		eventMillis := int64(0)
		if !event.Timestamp.IsZero() {
			eventMillis = event.Timestamp.UnixNano() / int64(time.Millisecond)
		}
		if beforeMillis > 0 && eventMillis >= beforeMillis {
			continue
		}
		if len(reversed) >= limit && eventMillis != boundaryMillis {
			break
		}
		reversed = append(reversed, event)
		if len(reversed) == limit {
			boundaryMillis = eventMillis
		}
	}

	out := make([]Event, len(reversed))
	for i := range reversed {
		out[len(reversed)-1-i] = reversed[i]
	}
	return out, nil
}

// ReadFirstEvent returns the first non-empty event for an artifact without
// parsing the rest of its JSONL log. It is used by list/search surfaces that
// only need a lightweight conversation preview and must not pay the cost of
// materializing large histories.
func (s *Store) ReadFirstEvent(k Kind, id string) (Event, bool, error) {
	f, err := s.openRead(eventsRel(k, id))
	if errors.Is(err, os.ErrNotExist) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("acf: open events file: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, scanBufInit), scanBufMax)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return Event{}, false, fmt.Errorf("acf: parse first event: %w", err)
		}
		return e, true, nil
	}
	if err := sc.Err(); err != nil {
		return Event{}, false, fmt.Errorf("acf: scan first event %s/%s: %w", k, id, err)
	}
	return Event{}, false, nil
}

// ReadEventsIncludingCompacted returns the union of ReadEvents and any
// events under <store>/events/.compacted/<kind>/<id>.jsonl.gz, merged in
// timestamp order. Used by `aplexica log --include-compacted` for
// forensics across the pruning grace period (BRD-03 §4.8.2).
//
// Returns the active events alone when no .compacted file exists.
// Returns an empty slice (no error) when neither layer has any events.
func (s *Store) ReadEventsIncludingCompacted(k Kind, id string) ([]Event, error) {
	active, err := s.ReadEvents(k, id)
	if err != nil {
		return nil, err
	}
	compactedRel := filepath.Join("events", ".compacted", kindDir(k), id+".jsonl.gz")
	f, err := s.openRead(compactedRel)
	if errors.Is(err, os.ErrNotExist) {
		return active, nil
	}
	if err != nil {
		return nil, fmt.Errorf("acf: open compacted: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("acf: gzip header on compacted: %w", err)
	}
	defer gz.Close()
	var compacted []Event
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, scanBufInit), scanBufMax)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var e Event
		if uerr := json.Unmarshal([]byte(line), &e); uerr != nil {
			return nil, fmt.Errorf("acf: parse compacted event: %w", uerr)
		}
		compacted = append(compacted, e)
	}
	if serr := sc.Err(); serr != nil {
		return nil, fmt.Errorf("acf: scan compacted %s/%s: %w", k, id, serr)
	}
	merged := append(compacted, active...)
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Timestamp.Before(merged[j].Timestamp)
	})
	return merged, nil
}

// ListArtifacts returns every artifact of the given kind, sorted by CreatedAt
// ascending (oldest first). Returns an empty slice (no error) when no
// artifacts of this kind exist or when the kind's directory has never been
// created.
func (s *Store) ListArtifacts(k Kind) ([]Artifact, error) {
	root, err := s.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := root.ReadDir(filepath.Join("acf", kindDir(k)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("acf: list %s artifacts: %w", k, err)
	}

	var out []Artifact
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		a, err := s.ReadArtifact(k, id)
		if err != nil {
			return nil, fmt.Errorf("acf: list %s artifacts: read %s: %w", k, id, err)
		}
		out = append(out, a)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// DeleteArtifact removes the artifact JSON file and its event log. Returns an
// error wrapping os.ErrNotExist if the artifact doesn't exist. Idempotent on
// the events file (no error if absent, since an artifact may have no events
// for some flow that was interrupted).
func (s *Store) DeleteArtifact(k Kind, id string) error {
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.RemoveRegular(artifactRel(k, id)); err != nil {
		return fmt.Errorf("acf: delete artifact %s/%s: %w", k, id, err)
	}
	// Events file is best-effort; absence is fine.
	if err := root.RemoveRegular(eventsRel(k, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("acf: delete events %s/%s: %w", k, id, err)
	}
	s.eventIDIndexMu.Lock()
	delete(s.eventIDIndex, eventIDIndexKey(k, id))
	s.eventIDIndexMu.Unlock()
	return nil
}

// FindBySourcePath returns the artifact of the given kind whose SourcePath
// matches sourcePath, plus a boolean indicating whether a match was found.
// An empty sourcePath never matches anything (this is the documented guard
// against pre-v0.2.0 artifacts with empty SourcePath colliding with each
// other or with new lookups).
//
// Lookup is O(N) over artifacts of the kind. Suitable for V0.2 store sizes
// (~100s of artifacts). If this becomes hot, add a SourcePath index file.
func (s *Store) FindBySourcePath(k Kind, sourcePath string) (Artifact, bool, error) {
	if sourcePath == "" {
		return Artifact{}, false, nil
	}
	artifacts, err := s.ListArtifacts(k)
	if err != nil {
		return Artifact{}, false, fmt.Errorf("acf: find by sourcePath: %w", err)
	}
	var best Artifact
	found := false
	for _, a := range artifacts {
		if a.SourcePath == sourcePath {
			if !found || sourcePathCandidateIsNewer(a, best) {
				best = a
				found = true
			}
		}
	}
	if found {
		return best, true, nil
	}
	return Artifact{}, false, nil
}

func sourcePathCandidateIsNewer(candidate, current Artifact) bool {
	if !candidate.UpdatedAt.Equal(current.UpdatedAt) {
		return candidate.UpdatedAt.After(current.UpdatedAt)
	}
	if !candidate.CreatedAt.Equal(current.CreatedAt) {
		return candidate.CreatedAt.After(current.CreatedAt)
	}
	return candidate.ArtifactID > current.ArtifactID
}

// FindByNativeID resolves an artifact by a NATIVE agent identifier — most
// importantly a Claude Code conversation session-id (the .jsonl basename) — when
// the caller has the native id but not the daemon-assigned ArtifactID. For every
// kind it matches id against: the ArtifactID; the artifact Name (with and
// without its extension); and the SourcePath basename (with and without
// extension). A leading path/namespace segment on id (e.g. "conversation/<id>")
// is tolerated by matching only the last "/"-segment. Returns the first match,
// or found=false when nothing matches.
//
// `aplexica show`/`log` historically resolved only by ArtifactID, so a native
// session-id returned "not found" even when the conversation was fully stored
// (the session-id lives in Name/SourcePath, not the id). Lookup is O(N) over
// artifacts — acceptable at V0.2 store sizes, matching FindBySourcePath.
func (s *Store) FindByNativeID(id string) (Kind, Artifact, bool, error) {
	needle := id
	if i := strings.LastIndexByte(needle, '/'); i >= 0 {
		needle = needle[i+1:]
	}
	if needle == "" {
		return "", Artifact{}, false, nil
	}
	for _, k := range []Kind{KindMemory, KindSkill, KindTool, KindConversation} {
		artifacts, err := s.ListArtifacts(k)
		if err != nil {
			return "", Artifact{}, false, fmt.Errorf("acf: find by native id: %w", err)
		}
		for _, a := range artifacts {
			if artifactMatchesNativeID(a, needle) {
				return k, a, true, nil
			}
		}
	}
	return "", Artifact{}, false, nil
}

// artifactMatchesNativeID reports whether needle identifies a — by ArtifactID,
// by Name (with/without extension), or by SourcePath basename (with/without
// extension). needle is assumed already stripped of any leading "/"-segment.
func artifactMatchesNativeID(a Artifact, needle string) bool {
	if a.ArtifactID == needle {
		return true
	}
	candidates := []string{a.Name, strings.TrimSuffix(a.Name, filepath.Ext(a.Name))}
	if a.SourcePath != "" {
		base := filepath.Base(a.SourcePath)
		candidates = append(candidates, base, strings.TrimSuffix(base, filepath.Ext(base)))
	}
	for _, c := range candidates {
		if c != "" && c == needle {
			return true
		}
	}
	return false
}
