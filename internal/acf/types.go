package acf

import (
	"encoding/json"
	"time"

	"github.com/aplexica/aplexica/internal/project"
)

// BundleTransform is a per-file body transformer used by Bundle when callers
// want to mutate file contents before they hit the tar writer (e.g. PII
// anonymization in v0.31.0). The transformer is invoked AFTER the file is
// read from disk and BEFORE the tar entry header (the post-transform size
// is what's recorded in the header). Returning the input unchanged is the
// no-op contract. archivePath is the slash-separated path the entry will
// have inside the bundle ("acf/memories/<id>.json", "events/...", or
// "secrets/...") — transformers MUST inspect this and refuse to touch
// meta.json or anything they don't understand.
type BundleTransform func(archivePath string, body []byte) ([]byte, error)

const SchemaVersion = "1.0"

// Kind enumerates the four ACF artifact kinds. All four kinds are
// implemented as of V0.1.3.
type Kind string

const (
	KindMemory       Kind = "memory"
	KindSkill        Kind = "skill"
	KindTool         Kind = "tool"
	KindConversation Kind = "conversation"
)

// Scope is the artifact's reach: global, project, or namespace.
// Scope determines where an artifact applies.
type Scope string

const (
	ScopeGlobal    Scope = "global"
	ScopeProject   Scope = "project"
	ScopeNamespace Scope = "namespace"
)

// Artifact is the top-level ACF document for a single piece of agent state.
type Artifact struct {
	AcfSchemaVersion string `json:"acfSchemaVersion"`
	ArtifactID       string `json:"artifactId"` // UUIDv7
	Kind             Kind   `json:"kind"`
	Scope            Scope  `json:"scope"`
	// NamespaceID is mandatory and UUIDv7 when Scope is namespace. It is
	// client-authenticated identity, never a server-selected routing default.
	NamespaceID string `json:"namespaceId,omitempty"`
	Name        string `json:"name"` // human label, not an identifier
	// SourcePath is the absolute filesystem path the artifact was last
	// imported from on its source device. Used by importers to reconcile
	// re-imports of the same file to the same ArtifactID (per the stable-
	// UUIDv7 rule) instead of minting a new one. Empty for artifacts
	// imported before v0.2.0; such artifacts are treated as "no source
	// path" and will not match identity-reconciliation lookups.
	SourcePath string    `json:"sourcePath,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	// RemoteOriginDeviceID and RemoteSourceAgent retain the authenticated
	// provenance of the newest inbound event on minimal remote artifact shells.
	// Retention snapshots intentionally omit event provenance; keeping these
	// small routing fields on the artifact prevents startup backfill from
	// replaying multi-gigabyte active/compacted histories just to rediscover the
	// sender. Native artifacts leave both fields empty.
	RemoteOriginDeviceID string `json:"remoteOriginDeviceId,omitempty"`
	RemoteSourceAgent    string `json:"remoteSourceAgent,omitempty"`
	// HeadEventHash is the SHA-256 of the most recent event in this artifact's log.
	HeadEventHash string `json:"headEventHash"`
	// EventCount is the number of events appended since this metadata field was
	// introduced. New artifacts therefore carry the exact active-log count;
	// legacy artifacts begin at zero and establish a new persistent cadence as
	// subsequent events arrive. Keeping this small counter beside the head hash
	// lets hot metadata paths avoid rescanning multi-gigabyte JSONL logs.
	EventCount uint64 `json:"eventCount,omitempty"`
	// Tombstoned is true when the artifact's most recent event is a redaction.
	// Cleared by any subsequent create/update event. Export paths use this to
	// distinguish "redacted" from "created empty".
	Tombstoned bool `json:"tombstoned,omitempty"`
	// Tags are free-form labels attached to an artifact. Added in v0.29.0
	// to express retention policy: "pinned" or "keep-forever" exempts the
	// artifact from snapshot-driven pruning (BRD-03 §4.8.2). Future use
	// cases (faceted listing, policy routing) can layer on the same field
	// without a schema migration. `omitempty` keeps pre-v0.29.0 artifacts
	// wire-compatible.
	Tags []string `json:"tags,omitempty"`
	// Project carries BRD-02 §4.13's rich project identity when
	// Scope == ScopeProject. Nil for ScopeGlobal / ScopeNamespace and
	// for pre-v0.54.0 artifacts (additive per FR-02.25).
	//
	// Wire shape note: BRD §4.13.1 sketches a nested `scope: {kind,
	// project: {...}}` object. We implement additively with sibling
	// fields (`scope: "project"` enum + `project: {id, path, vcs,
	// ephemeral}`); functionally equivalent and wire-compatible with
	// every pre-v0.54.0 artifact in the store.
	Project *project.ProjectInfo `json:"project,omitempty"`

	// BranchHeads maps branch name to the SHA-256 of the head event on
	// that branch. The empty/missing "main" key is implicitly equal to
	// HeadEventHash; new branches added by `fork` events live here.
	// BRD-04 §4.1 / FR-04.11. Wire-compatible via omitempty: pre-v0.95.0
	// artifacts with no BranchHeads field continue to read fine and are
	// treated as having only a `main` branch headed by HeadEventHash.
	BranchHeads map[string]string `json:"branchHeads,omitempty"`

	// MaterializedBranchByAgent maps agent name → branch name to record
	// which branch each adapter currently has materialized into its
	// native storage. BRD-04 §4.1 ("Materialization is per-agent") /
	// FR-04.11 / FR-04.12.
	MaterializedBranchByAgent map[string]string `json:"materializedBranchByAgent,omitempty"`

	// SyncedAgents lists every adapter name this artifact has been
	// successfully fanned out to. Populated by the orchestrator's
	// post-export hook (v0.105.0). Used by orphan detection
	// (FR-05.10): a routing-rule change that excludes an agent in this
	// set is grounds for marking the artifact as orphaned in that
	// agent.
	SyncedAgents []string `json:"syncedAgents,omitempty"`
}

// EventType is the string-typed kind of an Event. Aliased to string for
// wire compatibility (existing JSON payloads with bare strings continue to
// parse). The four V1 event-log types are declared as constants; future
// types (fork/merge/snapshot for conversation-branching) follow the same
// pattern.
type EventType string

const (
	EventTypeCreate    EventType = "create"
	EventTypeUpdate    EventType = "update"
	EventTypeRedaction EventType = "redaction"
	EventTypeAmendment EventType = "amendment"
	// v0.95.0 (BRD-04 §6.1/§6.2): branch events on the OUTER event log.
	// These reuse the same string values as conversation_events.go's inner
	// ConversationEvent constants — Go treats an untyped string constant
	// "fork" and a typed EventType "fork" as equal in switch comparisons,
	// so the inner ConversationEvent constants in conversation_events.go
	// remain unchanged.
	EventTypeForkOuter  EventType = "fork"
	EventTypeMergeOuter EventType = "merge"

	// EventTypeBaseline (aligned-chains delta sync, 2026-07) is appended by
	// a RECEIVER when it adopts an origin device's full materialized
	// conversation state. It carries the full payload (a self-contained
	// checkpoint, like a payload-bearing FR-02.32 snapshot) plus the
	// origin's head identity in AlignedHead/AlignedEventID. It chains
	// normally onto the local head via ParentHash, but after the append the
	// artifact's head bookkeeping points at AlignedHead — NOT at the
	// baseline's own hash — so subsequent verbatim origin delta events
	// chain natively and both stores converge on identical head hashes.
	// Appended via Store.AdoptBaseline.
	EventTypeBaseline EventType = "baseline"
)

// MainBranch is the canonical name for the implicit root branch of every
// artifact. An event with an empty Branch field is treated as belonging to
// the main branch. BRD-04 §4.1.
const MainBranch = "main"

// Event is one append-only entry in an artifact's event log.
// Events are append-only and never edited. ParentHash links the hash chain.
type Event struct {
	EventID    string     `json:"eventId"`    // UUIDv7
	ArtifactID string     `json:"artifactId"` // matches the parent Artifact
	Type       EventType  `json:"type"`       // one of EventType* constants
	Timestamp  time.Time  `json:"timestamp"`
	Provenance Provenance `json:"provenance"`
	// Payload is the polymorphic per-kind body. The artifact's Kind determines
	// which payload type to decode it as (MemoryPayload for KindMemory,
	// SkillPayload for KindSkill). Use the helpers in payload.go to encode/decode.
	Payload json.RawMessage `json:"payload"`
	// MaterializedConversation is an in-process projection cache for callers
	// that already reconstructed a full conversation while preparing this
	// event for fan-out. It is never persisted and never participates in the
	// event hash. DecodeConversationPayload consults it before JSON decoding so
	// several native targets can share one large projection instead of each
	// decoding the same payload independently.
	MaterializedConversation *ConversationPayload `json:"-"`
	ParentHash               string               `json:"parentHash"` // SHA-256 of previous event; "" for genesis
	Hash                     string               `json:"hash"`       // SHA-256 of this event excluding the Hash field itself
	// SnapshotState is populated only on EventTypeSnapshot events; format is
	// "sha256:<hex>" of the materialized payload at that point. Empty for
	// all other event types. Added in v0.29.0 for the retention engine —
	// previously this field existed only on ConversationEvent (v0.25.0,
	// conversation-branching only). Wire-compatible via `omitempty`:
	// pre-v0.29.0 events with no snapshot field continue to round-trip.
	SnapshotState string `json:"snapshotState,omitempty"`

	// Branch names the branch this event belongs to. Empty means "main".
	// BRD-04 §4.1 / FR-04.1. Pre-v0.95.0 events have no Branch field and
	// are treated as main-branch events on read. AppendEvent chains
	// ParentHash per-branch, so a non-main branch keeps its own head
	// chain independent of main.
	Branch string `json:"branch,omitempty"`

	// ForkFromEventID — fork events only. The event hash on the source
	// branch this branch diverges from. BRD-04 §6.1's `from` field.
	ForkFromEventID string `json:"forkFromEventId,omitempty"`

	// ForkSourceBranch — fork events only. Name of the source branch
	// (empty = "main"). The fork event ITSELF lives on the new branch
	// (Branch field), so the source branch is recorded here.
	ForkSourceBranch string `json:"forkSourceBranch,omitempty"`

	// ForkOriginAgent — fork events only. The source/origin agent of the event
	// the fork was created from. Routing presets can resolve
	// __originatingAgent__ from this field; the target agent lives in
	// Artifact.MaterializedBranchByAgent.
	ForkOriginAgent string `json:"forkOriginAgent,omitempty"`

	// ForkRationale — fork events only. Optional user-supplied note.
	ForkRationale string `json:"forkRationale,omitempty"`

	// MergeFromBranch — merge events only. The source branch being
	// merged into Branch. BRD-04 §6.2 records the parent-on-source via
	// MergeFromHash; we also keep the branch name for human-friendly log
	// output.
	MergeFromBranch string `json:"mergeFromBranch,omitempty"`

	// MergeFromHash — merge events only. Head event of the source
	// branch at merge time. BRD-04 §6.2's `from` field.
	MergeFromHash string `json:"mergeFromHash,omitempty"`

	// MergeStrategy — merge events only. One of "fast-forward",
	// "manual", "ours", "theirs". BRD-04 §6.2.
	MergeStrategy string `json:"mergeStrategy,omitempty"`

	// MergeResolutionNotes — merge events only. Free-text notes captured
	// during interactive resolution. BRD-04 §6.2 `resolutionNotes`.
	MergeResolutionNotes string `json:"mergeResolutionNotes,omitempty"`

	// EventTags — optional list of free-form tags on this event.
	// FR-04.17. System-reserved namespaces (aplexica:*, auto:*) are
	// permitted in the wire format but the CLI's tag-write commands
	// reject them on input.
	EventTags []string `json:"eventTags,omitempty"`

	// AlignedHead — baseline events only (EventTypeBaseline). The ORIGIN
	// device's head event Hash at the moment the baseline was published.
	// After a baseline is appended, the artifact's head bookkeeping
	// (HeadEventHash / BranchHeads[main]) is set to this value so verbatim
	// origin delta events chain natively; VerifyChain applies the same
	// reset. `omitempty` keeps every pre-baseline event's canonical JSON —
	// and therefore its ComputeHash — byte-identical.
	AlignedHead string `json:"alignedHead,omitempty"`

	// AlignedEventID — baseline events only. The EventID of the origin
	// head event AlignedHead names. UUIDv7, so string ordering is time
	// ordering — the deterministic re-align tiebreak the sync layer uses
	// when two devices hold equal content under different heads.
	AlignedEventID string `json:"alignedEventId,omitempty"`
}

// Provenance records who/what produced this event.
type Provenance struct {
	DeviceID       string `json:"deviceId"`
	SourceAgent    string `json:"sourceAgent"`    // e.g. "claude-code"
	AgentVersion   string `json:"agentVersion"`   // FR-02.28: "unknown" when the agent exposes no version; never empty
	AdapterVersion string `json:"adapterVersion"` // semver of the adapter that wrote this
	// CausedBy is the event hash of the source event that triggered this
	// event. Empty for first-source events (e.g. a user manually editing a
	// file). Populated by the sync orchestrator on fan-out exports.
	//
	// A future durable recursion guard (BRD-03 §4.5) will compare CausedBy
	// against the event's causal ancestry to refuse materializing a
	// cross-device or cross-restart bounce-back; that check is deferred (see
	// ADR-0045 recursion-defense-v1-scope). Today the field is plumbed through
	// but not yet consulted — the in-memory path-based RecursionGuard plus
	// destHashes (content idempotency) handle same-process bounce-back.
	CausedBy string `json:"causedBy,omitempty"`
}

// UnknownAgentVersion is the FR-02.28 sentinel for createdBy.agentVersion when
// the source agent does not expose its version. Adapters MUST set this rather
// than leaving AgentVersion empty (omitting the field is non-conformant).
const UnknownAgentVersion = "unknown"

// MemoryPayload is the body of a memory event.
// CLAUDE.md round-trips losslessly: Format="markdown", Content=verbatim file bytes.
type MemoryPayload struct {
	Format  string `json:"format"`  // "markdown" for CLAUDE.md
	Content string `json:"content"` // verbatim
}

// SkillPayload is the body of a skill event. In V0.2 a skill is treated as
// opaque file bytes — Format="skill.md", Content=verbatim file bytes. V0.3+
// will add structured fields (name, description, version) parsed from the
// frontmatter, additively per ADR-0019.
type SkillPayload struct {
	Format  string `json:"format"`  // "skill.md" for SKILL.md files
	Content string `json:"content"` // verbatim
}

// Attachment is the metadata for one binary content item in a conversation
// (image, audio, video, file). Added v0.34.0 per BRD-03 §4.8.3 to separate
// text from binary content so the retention engine can evict big blobs
// while preserving conversation text.
//
// Integrity-preserving eviction (BRD-03 §4.8) keeps the raw bytes
// CONTENT-ADDRESSED and TRANSIENT rather than inlined-and-hashed:
//
//   - ContentHash (hex sha256) is the blob id in internal/blobstore. It is
//     the only attachment identity that participates in the event hash
//     chain; the bytes themselves are kept out of the hashed payload.
//   - Data carries the raw bytes ONLY in memory, populated on demand from
//     the blob store. Its `json:"-"` tag means it is NEVER serialized and
//     therefore NEVER affects ComputeHash — populated or nil, the event
//     hash is identical (proven by hash_test.go). This is what lets
//     eviction be append-only: evicting a blob does not perturb any
//     historical event hash, so acf.VerifyChain stays green.
//   - Evicted, when non-nil, is the canonical evicted-slot marker. Because
//     it IS serialized, appending an updated payload that sets it produces
//     a new, distinct event hash — the marker is canonical content, the
//     bytes are not.
//
// Wiring caveat: orchestrator/adapter handoff still treats payloads
// opaquely (the schema change does NOT migrate the existing canonical
// session translators). Cross-adapter attachment fidelity is a future
// milestone.
type Attachment struct {
	Kind     string `json:"kind"` // "image" | "audio" | "video" | "file"
	MimeType string `json:"mimeType"`
	// ContentHash is the hex sha256 blob id in internal/blobstore. Always
	// populated; the only attachment identity that is hashed into the
	// event chain. (Renamed from the v0.34.0 `hash` field.)
	ContentHash string `json:"contentHash"`
	Bytes       int64  `json:"bytes"` // size of the blob, always populated
	// Data is the raw bytes, in memory only. `json:"-"` excludes it from
	// the wire AND from ComputeHash — it never affects the event hash.
	// Populated on demand from the blob store; nil otherwise.
	Data     []byte       `json:"-"`
	Filename string       `json:"filename,omitempty"`
	Evicted  *EvictedInfo `json:"evicted,omitempty"`
}

// EvictedInfo is the FR-03.20 evicted-slot marker written into an
// attachment when the retention engine evicts its blob. The spec does not
// pin a wire shape for the evicted slot; this struct is the chosen default
// — a self-describing record (when, why, original size, and the blob id
// that was dropped) so a UI can render "[evicted, image/png, 1.4 MB]" and
// a forensic reader can correlate the dropped blob.
type EvictedInfo struct {
	At           time.Time `json:"at"`
	Reason       string    `json:"reason"`
	OriginalSize int64     `json:"originalSize"`
	ContentHash  string    `json:"contentHash"`
}

// IsEvicted returns true when the attachment's blob has been evicted by the
// retention engine (the canonical Evicted marker is set). Callers should
// treat this as a hint to render a placeholder rather than the blob.
func (a Attachment) IsEvicted() bool {
	return a.Evicted != nil
}

// ConversationPayload is the body of a conversation event. Two coexisting
// shapes are supported as of V0.15.0:
//
//   - Legacy opaque (V0.1.2+): Format="claude-code.session.jsonl" (or
//     "codex.session.jsonl", "acf.hermes.session.v1"), Content=verbatim file
//     bytes. The Events field is empty.
//   - Structured (V0.15.0+): Format=ConversationFormatV1
//     ("acf.conversation.v1"), Events=canonical event list. The Content field
//     is empty.
//
// Callers must inspect Format and read the appropriate field. No migration
// happens automatically; legacy artifacts stay legacy.
type ConversationPayload struct {
	Format string `json:"format"`

	// Legacy opaque mode: verbatim file bytes when Format is a per-agent
	// format like "claude-code.session.jsonl".
	Content string `json:"content,omitempty"`

	// Structured mode (added V0.15.0): canonical event list when Format is
	// ConversationFormatV1.
	Events []ConversationEvent `json:"events,omitempty"`

	// Attachments — added v0.34.0 per BRD-03 §4.8.3. Separates binary
	// content from text so the retention engine can evict attachment
	// blobs without losing conversation text. The bytes are
	// content-addressed (Attachment.ContentHash -> internal/blobstore);
	// eviction sets each attachment's Evicted marker via an appended
	// event rather than mutating this slice in place. omitempty keeps
	// pre-v0.34.0 payloads wire-compatible.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// ToolPayload is the body of a tool event. In V0.1.3 a tool artifact holds the
// REDACTED .mcp.json content — secrets in env blocks have been replaced by
// "${secret:<name>}" reference placeholders. Raw secret values live in the
// separate secrets store at ~/.aplexica/secrets/ and are NEVER part of this
// payload, never hashed into the chain, never written to the canonical store
// (per ADR-0027).
type ToolPayload struct {
	Format  string `json:"format"`  // "claude-code.mcp.json" for Claude Code MCP configs
	Content string `json:"content"` // redacted JSON (secrets as ${secret:...} refs)
}
