package syncd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	codexadapter "github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/pausestate"
	"github.com/aplexica/aplexica/internal/pending"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/rbac"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/aplexica/aplexica/internal/syncgate"
	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/aplexica/aplexica/internal/watcher"
)

// defaultMaxArtifactBytes is the default inbound max-artifact-size cap: 64 MiB
// (BRD-03 §4.3 / §5 `[limits] max_artifact_size_mb = 64`). A settled native
// file larger than this is refused at the handleEvent chokepoint rather than
// parsed and canonical-encoded — the "sign of something unusual" the BRD calls
// out (a huge JSON blob in a watched root) must not blow up the store. Used
// when Config.MaxArtifactBytes is zero; a negative MaxArtifactBytes disables
// the cap.
const defaultMaxArtifactBytes int64 = 64 << 20

// defaultMaxSessionFileBytes is the default ingest cap for AGENT SESSION
// transcripts — Claude Code ~/.claude/projects/**.jsonl and Codex
// ~/.codex/sessions/** rollouts (`[limits] max_session_file_mb = 512`).
// Session transcripts are append-mostly conversation logs that can outgrow the
// generic 64 MiB artifact cap and, with aligned-chain delta sync, replicate as
// O(new turn) events — a large transcript is normal operation, not the "sign of
// something unusual" the generic cap guards against. Used when
// Config.MaxSessionFileBytes is zero; a negative value disables the cap.
const defaultMaxSessionFileBytes int64 = 512 << 20

// maxFastLaneImportBytes separates interactive, newly-created Codex
// transcripts from established histories for import admission. Other artifact
// classes retain strict serialization; only a small Codex rollout may use the
// second total slot while large/background work is in flight. Keep this aligned
// with the Codex live scanner's bounded readiness parse so the fast lane never
// admits an unexpectedly large whole-file preflight.
const maxFastLaneImportBytes int64 = 2 << 20

// oversizeReportInterval throttles how often ONE oversized file re-surfaces an
// artifact.refused bus event. The refusal decision itself is deliberately not
// cached (a shrink below the cap re-imports on the next probe), but the 5s
// native scan re-refuses an unchanged oversized file every tick — without a
// throttle one growing session file floods the event stream (~720 events/h).
// One bus event per file per interval keeps the signal; the keyed status
// warning slot ("max-artifact-size" via recordAdapterError) stays continuously
// visible either way. Package var (not const) so tests can shrink it.
var oversizeReportInterval = time.Hour

const (
	scanFileCandidateCap = 256
)

// Config holds the orchestrator's construction-time parameters.
type Config struct {
	// Dir is the absolute path to the directory the orchestrator watches.
	// Must exist and be readable. Non-recursive in v0.7.0.
	Dir string

	// AdditionalRoots are extra absolute directories to watch beyond Dir —
	// the discovered native global-storage roots of installed agents
	// (BRD-03 FR-03.3 / §4 "the daemon watches the global paths
	// permanently"). Each is watched with the same debouncer + handleEvent
	// pipeline as Dir, so edits in e.g. ~/.claude or ~/.codex import into
	// the canonical store. Unreadable roots are skipped (logged, not fatal).
	// Empty = pre-discovery behavior (watch Dir only).
	AdditionalRoots []string

	// RecursiveRoots are extra native roots watched RECURSIVELY regardless of
	// the Recursive flag — for agents that nest artifacts in subdirectories a
	// flat watcher can't reach (e.g. Codex's ~/.codex/sessions/<Y>/<M>/<D>/
	// rollout-*.jsonl). Same debouncer + handleEvent pipeline as AdditionalRoots.
	RecursiveRoots []string

	// MetadataRoots are app-owned directories watched recursively only as
	// read-only import triggers. They are intentionally excluded from native
	// backup/restore inventories.
	MetadataRoots []string

	// WatchFiles are INDIVIDUAL FILES to watch — agent config files that live
	// outside any watchable directory root (e.g. ~/.claude.json at the HOME
	// root, where `claude mcp add -s user` writes). Each file's parent dir is
	// watched with a filter that forwards only that file's events, so the
	// rest of the parent dir stays unwatched. Same debouncer + handleEvent
	// pipeline as the directory roots.
	WatchFiles []string

	// RootsByAdapter maps each adapter Name() to the native storage roots it
	// owns (its GlobalRoots + RecursiveRoots + MetadataRoots). The source-picker uses it to
	// break extension-dispatch ties by PATH OWNERSHIP: a *.jsonl under
	// ~/.codex/sessions/ must be imported by codex, not by whichever adapter
	// sorts first alphabetically (claudecode < codex both claim *.jsonl).
	// Empty/nil = pre-ownership behaviour (pure alphabetical fallback).
	RootsByAdapter map[string][]string

	// SyncGate, when non-nil, gates outbound fan-out by both source and target
	// (FR-03.3 "discover + show, await config"). A disabled source still
	// imports into the canonical store for local visibility, but does not feed
	// any target; a disabled target does not receive exports. Nil = no gating
	// (pre-v-next behavior; every installed adapter is a fan-out candidate,
	// subject to the other gates). Composes with the ProjectRegistry /
	// RulesEngine / PauseStore / Quarantine gates.
	SyncGate *syncgate.Gate

	// Adapters is the set of "installed" adapters. The orchestrator picks
	// the primary adapter for each file by alphabetical Name() order among
	// the adapters whose Import accepts the filename.
	Adapters []adapter.Adapter

	// DynamicAdapterDiscovery enables runtime availability checks for adapters
	// implementing adapter.RuntimeDiscoverable. The production daemon enables
	// it and retains Claude/Codex after a negative startup probe; tests and
	// embedders default to the historical static-adapter behavior unless they
	// opt in.
	DynamicAdapterDiscovery bool

	// RuntimeAdapterActivated runs synchronously before the first import or
	// outbound write after a runtime-discoverable adapter's surface/storage
	// topology changes. The daemon uses it to take the required native safety
	// snapshot and update AdapterBlocker before synchronization can touch a
	// newly installed agent. nil means no activation hook.
	RuntimeAdapterActivated func(name string, discovery adapter.Discovery)

	// Store is the canonical store. Both import and fan-out export route
	// through this single store.
	Store *acf.Store

	// QuietPeriod is the debouncer's per-path quiet wait. Default 500ms
	// per ADR-0031; tests may use a shorter value.
	QuietPeriod time.Duration

	// GuardWindow is how long the recursion guard suppresses events for
	// freshly-written paths. Should cover the longest plausible fan-out
	// round-trip; 5s is the production default. Tests may use a shorter
	// value but it MUST exceed QuietPeriod + a couple of OS scheduling
	// margins, else fan-out writes will leak through as inbound events.
	GuardWindow time.Duration

	// LiveScanInterval, when positive, starts a runtime catch-up scanner over
	// the same configured roots as InitialScan. It is a safety net for missed
	// platform watcher events: scanCache makes unchanged files a cheap stat
	// pass, while changed files still flow through the normal handleEvent
	// import/fan-out/remote-publish path. Zero disables it.
	LiveScanInterval time.Duration

	// DedicatedCodexSessionScan makes RunNativeLiveScan skip ~/.codex/sessions
	// roots because the daemon is running the faster 500ms
	// ScanRecentCodexSessions poller as the sole owner for those rollouts.
	// InitialScan still scans the recent Codex day dirs. Tests leave this false
	// so RunNativeLiveScan remains a complete standalone native-root scanner.
	DedicatedCodexSessionScan bool

	// RecentClaudeSessionWindow is how long an active Claude Code JSONL
	// transcript remains in the hot 500ms poll set after an import or remote
	// materialization. Zero disables the hot poller for tests/custom callers.
	RecentClaudeSessionWindow time.Duration

	// MaxArtifactBytes caps the on-disk size of an inbound native file the
	// orchestrator will ingest (BRD-03 §4.3: "Files larger than 64 MB are
	// flagged with a warning rather than ingested by default ... Threshold is
	// configurable"). handleEvent stats every settled path before the
	// expensive parse + canonical-encode in primaryImport; a file whose size
	// exceeds this cap is REFUSED (never imported, never fanned out) and
	// surfaces a user-visible warning on the status channel — a hostile or
	// runaway multi-GB blob dropped into a watched root must not be read into
	// memory and grow the store unbounded.
	//
	// Zero selects the default (defaultMaxArtifactBytes, 64 MiB). A negative
	// value disables the cap entirely (unlimited ingest). The daemon resolves
	// the BRD's `max_artifact_size_mb` knob into this byte count.
	MaxArtifactBytes int64

	// MaxSessionFileBytes caps the on-disk size of an inbound AGENT SESSION
	// transcript — a path matching isClaudeSessionPath or isCodexSessionPath —
	// replacing MaxArtifactBytes for those paths only. Session transcripts are
	// append-mostly conversation logs that legitimately outgrow the generic
	// artifact cap (multi-week Claude sessions reach hundreds of MB) and, with
	// aligned-chain delta sync, replicate as O(new turn) events — so a large
	// transcript is normal operation, not the runaway blob the generic cap
	// guards against. Everything that is not a session transcript keeps
	// MaxArtifactBytes.
	//
	// Zero selects the default (defaultMaxSessionFileBytes, 512 MiB). A
	// negative value disables the session cap (unlimited session ingest). The
	// daemon resolves the `limits.max_session_file_mb` knob into this byte
	// count.
	MaxSessionFileBytes int64

	// Recursive controls whether the orchestrator watches the directory tree
	// recursively. When false (the default), only direct children of Dir
	// trigger events. When true, the orchestrator uses RecursiveSource which
	// aggregates per-directory Sources over the whole tree, auto-attaching
	// to new subdirs and cleaning up removed ones.
	Recursive bool

	// ConflictStore, when non-nil, enables conflict detection. The
	// orchestrator records divergent concurrent writes here instead of
	// last-writer-wins. Nil = pre-v0.28.0 behavior (skip detection).
	ConflictStore *conflicts.Store

	// ConflictWindow is the duration during which two updates from
	// DIFFERENT adapters to the same artifact, with different content
	// hashes, are considered a conflict. Default 30s. Has no effect when
	// ConflictStore is nil.
	ConflictWindow time.Duration

	// PauseStore, when non-nil, gates outbound fan-out on the
	// BRD-03 §4 / FR-03.11 user pause state. Adapters reporting
	// IsPaused=true at fan-out time are skipped. Nil = pre-v0.88.0
	// behavior (no pause check).
	PauseStore *pausestate.Store

	// Quarantine, when non-nil, gates fan-out on BRD-03 FR-03.15
	// adapter-health state. Adapters whose Export/materialization fail 3+
	// times in 10 minutes are marked quarantined and skipped until the window
	// self-clears or an operator runs `aplexica daemon restart`. Import parse
	// failures are kept as per-file status errors and do not quarantine the
	// whole adapter, because agent histories can contain old or half-written
	// native files. Quarantine is deliberately outbound-only: a failing target
	// must still be allowed to import its own native changes into the canonical
	// store and remote transport. Nil = pre-v0.92.0 behavior (no quarantine
	// check).
	Quarantine *QuarantineTracker

	// AdapterBlocker, when non-nil, gates both import and fan-out for
	// explicit user-facing safety blockers. Unlike Quarantine, these blockers
	// are policy states such as "native backup required" and can be cleared by
	// a portal action without restarting the daemon.
	AdapterBlocker *AdapterBlocker

	// SnapshotCadence holds per-kind event-count thresholds for automatic
	// snapshotting. The orchestrator fires retention.CreateSnapshot when the
	// artifact's persistent append counter crosses the kind's threshold (count
	// % threshold == 0), so threshold=50 produces a snapshot at the 50th,
	// 100th, 150th, ... post-counter event for that kind. Legacy artifacts start
	// their persistent counter at the first event appended by a release carrying
	// that metadata. A missing key, zero, or negative threshold disables
	// automatic snapshotting for that kind.
	// A nil/empty map disables automatic snapshotting entirely.
	//
	// threshold=1 is technically valid but means "snapshot every event" —
	// semantically correct, just inefficient. Operators who set it know
	// what they're asking for.
	//
	// BRD-03 §4.8.1 defaults (applied by the daemon wiring, not this
	// package):
	//   conversation: 100, memory: 50, skill: 50, tool: 50.
	//
	// Hot-reloadable via SetSnapshotCadence (the SIGHUP handler invokes
	// this when the per-kind daemon.Config fields change). v0.29.2 replaced
	// the single SnapshotCadenceEvents int from v0.29.1 — time-based
	// snapshot cadence (24h conversation / 7d memory per BRD-03 §4.8.1)
	// is handled separately by retention.RunTimeBasedSnapshotter.
	SnapshotCadence map[acf.Kind]int

	// ProjectRegistry, when non-nil, gates the fan-out path on project
	// registration (BRD-02 §4.13 stage-and-wait; v0.57.0). An artifact
	// with Scope==ScopeProject AND a populated Project field whose
	// Project.ID is NOT registered in this registry is SKIPPED during
	// fanOut — it lands in the canonical store but does NOT propagate
	// to other adapters until the user runs `aplexica project link
	// <id> <path>` (which adds the entry to the registry; v0.58.0
	// adds the re-fanout trigger).
	//
	// Nil registry = pre-v0.57.0 behavior: every project-scope artifact
	// fans out regardless of registry state. Used by tests that don't
	// want to think about pending projects.
	ProjectRegistry *project.Registry

	// EventPublisher, when non-nil, receives structured event
	// notifications at key orchestrator lifecycle moments. V0.107.0
	// wires the daemon's sse.Bus into this field so the local web UI
	// can subscribe to the live stream at /api/events/stream. The
	// interface is deliberately minimal — one method — so the sync
	// package doesn't take a hard dependency on the web stack and
	// tests can plug in a stub publisher in two lines.
	//
	// Nil = no publishing. All call sites are guarded.
	EventPublisher EventPublisher

	// RemoteEventPublisher, when non-nil, receives one notification
	// per successful committed event (after primaryImport + fanOut
	// have both succeeded). The daemon's RemoteRunner is the typical
	// receiver — it converts the event into a RemoteEvent and queues
	// it for outbound transmission to the remote plugin.
	//
	// The orchestrator calls Publish synchronously after fanOut
	// returns; receivers MUST NOT block on a network call. Buffer
	// the event into a queue and return immediately; the actual
	// transport flush happens on the receiver's own goroutine.
	//
	// Nil = no remote publishing (typical OSS-only
	// daemon).
	RemoteEventPublisher RemoteEventPublisher

	// LocalDeviceID is this device's cloud device identity (the id the
	// remote plugin reports at pairing). It is stamped as OutboundEvent.Origin
	// on every locally-originated event the orchestrator forwards to the
	// remote, and it is the loop-prevention discriminator on the INBOUND
	// path: an event whose recorded origin device is NOT this device is a
	// remote-authored event, so re-importing/re-materializing it must never
	// cause it to be forwarded back out (see publishOutbound's caller in
	// handleEvent). Empty = unknown (OSS / un-paired daemon): outbound still
	// works for visibility but there is no cloud identity to dedupe against,
	// so the structural guard (remote imports never traverse handleEvent;
	// fan-out echoes are recursion-guard-suppressed) is the only protection.
	//
	// Seeded by the daemon from the plugin's --status device_id
	// before the RemoteRunner starts. A later pairing (or RE-pairing, which
	// rotates the cloud id) must be applied via SetLocalDeviceID — all reads
	// go through the localDeviceID accessor, never this field directly.
	LocalDeviceID string

	// Logger receives diagnostic sync-path messages. Nil = no file logging.
	Logger Logger

	// RecipientResolver resolves the device recipient set each OUTBOUND event is
	// end-to-end encrypted for (see envelope.go). The daemon backs it with
	// RemoteRunner.ListNamespaceDevices (cached) and ALWAYS includes this device
	// so the sender can decrypt its own re-imports. Nil = no recipients can be
	// resolved, so every outbound event is DROPPED (the daemon NEVER transmits
	// plaintext).
	RecipientResolver RecipientResolver

	// DeviceKeyProvider hands the orchestrator this device's X25519 private wrap
	// key so it can OPEN inbound envelopes. The daemon backs it with
	// keys.DeviceKeyStore.LoadOrCreate (the same key registered with the cloud
	// at pairing). Nil = inbound events cannot be decrypted (import fails with a
	// clear error).
	DeviceKeyProvider DeviceKeyProvider

	// RequireEnvelopeV2 is enabled after the account's explicit signed-roster
	// security-epoch cutover. Before that durable monotonic gate exists, an
	// existing account may use the encrypted v1 migration overlap. Once enabled,
	// missing or stale verified state pauses remote operations fail-closed.
	RequireEnvelopeV2      bool
	VerifiedRosterProvider VerifiedRosterProvider
	V2IdentityProvider     V2IdentityProvider
	NamespaceKeyProvider   keyrotation.NamespaceKeyProvider

	// RulesEngine, when non-nil, gates fan-out on the BRD-05 selective-
	// sync rules engine (FR-05.5/6/7). For each candidate (adapter,
	// artifact) pair, the orchestrator builds a syncrules.Artifact
	// projection, calls Engine.Evaluate, and skips adapters whose name
	// is NOT in AllowedAgents. Tag-assigning rules are applied to the
	// artifact's Tags at fan-out time (FR-05.5).
	//
	// Nil = pre-v0.104.0 behavior: no rule gating; every adapter sees
	// every artifact subject to format / pause / quarantine / project-
	// registry gates only.
	RulesEngine *syncrules.Engine

	// WriteAuthorizer, when non-nil, is the DESYNC-SAFE client-side write-gate
	// for namespace-scoped local mutations. Before a
	// namespace-scoped event is committed to the canonical store, the
	// orchestrator consults Authorize at the single guarded chokepoint
	// (commitNamespaceEvent) and ABORTS the commit — leaving the store and the
	// hash-chain byte-for-byte unchanged — if and only if it returns a
	// definitive permission deny. The daemon's *RoleService satisfies this
	// structurally; its Authorize returns a non-nil (rbac.ErrForbidden-wrapping)
	// error ONLY for a KNOWN role that lacks the capability, and nil for every
	// unknown/unpaired/offline case, so the gate can never refuse AFTER a commit
	// and therefore can never desync the local chain from peers.
	//
	// Nil = today's behavior unchanged: every commit proceeds (OSS-only /
	// un-paired daemon, and the V1 import paths that produce only
	// global/project artifacts). The server stays authoritative regardless.
	//
	// NOTE (discrepancy): no LOCAL namespace-scoped artifact-mutation path is
	// reachable in V1 — adapter imports produce only global/project artifacts
	// (ScopeNamespace is V2-reserved) and acf.Artifact carries no NamespaceID.
	// This field + commitNamespaceEvent are therefore delivered as the
	// desync-safe pre-commit SEAM the future namespace-write path will route
	// through, not as an active intercept on a live flow.
	//
	// FOLLOW-UP: full offline write-consistency — reconciling a server-side
	// POST-HOC rejection of a write made while the role was unknown/offline —
	// remains a separate effort; this gate only fast-paths the definitive deny.
	WriteAuthorizer WriteAuthorizer

	// SyncLatencyObserver, when non-nil, receives the import-to-fan-out
	// materialization latency of each committed artifact for the NFR-10 §5.2
	// sync_latency_seconds histogram. The daemon wires its *metrics.Registry
	// here (adapted onto the SyncLatencyObserver seam) only when the metrics
	// endpoint is enabled. Nil = no-op (typical OSS daemon). Swappable live via
	// SetSyncLatencyObserver under o.mu.
	SyncLatencyObserver SyncLatencyObserver
}

// WriteAuthorizer is the orchestrator's narrow seam onto the daemon-side RBAC
// write-gate. Declared HERE (not imported from internal/daemon) to keep the
// dependency edge one-way: internal/sync must not import internal/daemon. The
// daemon's *RoleService satisfies it structurally. rbac.Operation is a
// stdlib-only leaf type (internal/rbac has no aplexica-internal deps), so
// naming it here introduces no import cycle.
//
// Contract (see Config.WriteAuthorizer): Authorize returns a non-nil error
// ONLY for a DEFINITIVE permission deny (a known role lacking the capability),
// wrapping rbac.ErrForbidden; it returns nil for every unknown/unpaired/offline
// case so the gate is desync-safe (never blocks after a commit could have
// occurred).
type WriteAuthorizer interface {
	Authorize(ctx context.Context, namespaceID string, op rbac.Operation) error
}

// EventPublisher receives structured live-event notifications from
// the orchestrator. Implementations (the daemon's sse.Bus in
// production; test stubs in unit tests) treat Publish as a best-
// effort notification — Publish must not block and must not fail
// (return type is intentionally void).
//
// Kind is a stable string identifier shared with the SPA's
// schemas — see internal/web/sse/bus.go for the canonical set.
// Body is a marshalable shape whose contract lives at each call
// site.
type EventPublisher interface {
	Publish(kind string, body any)
}

// Logger receives diagnostic sync-path messages from the orchestrator.
// Implementations must not block the import path.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// SyncLatencyObserver receives the import-to-fan-out materialization latency
// (in seconds) of each artifact the orchestrator commits, for the NFR-10 §5.2
// sync_latency_seconds histogram. Declared HERE as a narrow local interface
// (not imported from internal/metrics) to keep the dependency edge one-way —
// internal/metrics stays a leaf, and the daemon adapts its *metrics.Registry
// onto this seam (see SetSyncLatencyObserver).
//
// ObserveSyncLatency is called synchronously on the import path after fan-out
// returns; implementations MUST NOT block (the production sink is an in-memory
// histogram bump under a short-held lock). Nil = no observer wired: the typical
// OSS daemon with the metrics endpoint disabled, where instrumentation is a
// no-op.
type SyncLatencyObserver interface {
	ObserveSyncLatency(seconds float64)
}

// RemoteEventPublisher is the orchestrator's hook for forwarding
// successful canonical-store events to a remote-transport plugin
// (Aplexica Cloud, a self-hosted relay, or a BYO transport). The
// daemon's RemoteRunner is the typical implementation.
//
// PublishOutbound is called synchronously from the orchestrator's
// import path after fanOut completes; receivers MUST enqueue and
// return immediately. The orchestrator does NOT retry — if the
// receiver's queue is full or its plugin is disconnected, the
// receiver bears responsibility for catching up via a subsequent
// remote.fetch cycle.
//
// Optional remote-transport support.
type RemoteEventPublisher interface {
	PublishOutbound(event OutboundEvent)
}

// LargeRetainedCheckpointPublisher is an optional additive capability. The
// orchestrator consults it only after it has sealed a retained conversation
// checkpoint above the legacy 4 MiB ceiling. Implementations must return true
// only when an authenticated, crash-safe bounded transfer path is currently
// available; old/self-hosted publishers therefore preserve legacy refusal.
type LargeRetainedCheckpointPublisher interface {
	SupportsLargeRetainedCheckpoint(event OutboundEvent) bool
}

// Lane values for OutboundEvent.Lane — the two-lane outbound transport of the
// aligned-chains delta sync (2026-07).
const (
	// LaneLive carries the verbatim stored head event: small, published on a
	// non-retained topic, FIFO per artifact, never coalesced. For
	// conversations this is the original compact delta a receiver appends
	// natively; for every other kind it is today's full-state event.
	LaneLive = "live"
	// LaneRetained carries the full materialized conversation state plus the
	// alignment metadata (AlignedHead/AlignedEventID) a receiver adopts a
	// baseline from: retained topic, coalescible per artifact, may be large.
	LaneRetained = "retained"
)

// retainedEventIDSuffix distinguishes the lane=retained WIRE/outbox EventID
// from the live lane's. The two lanes of one conversation commit are two
// distinct transport events: with a shared id the daemon's durable outbox —
// which keys files by EventID — silently no-ops the retained lane's
// persist-before-publish append against the live lane's file, and an
// oversized retained dead-letter would displace the LIVE delta's durable file
// (and block its future appends via the dead/ check). EventIDs are opaque
// wire/outbox identifiers — nothing in the daemon, cmd, or plugin protocol
// parses them as UUIDs — so a suffix is safe. The suffixed id travels ONLY on
// the wire envelope, outbox filename, and retry bookkeeping; the SEALED event
// body and its AlignedEventID keep the head's real EventID (the re-align
// tiebreak is unchanged).
const retainedEventIDSuffix = "-r"

// retainedOriginDiscriminatorLen bounds the origin discriminator appended to
// the retained wire id (see RetainedWireEventID): the leading hex chars of a
// sha256 of the origin device id — short, and collision-uniform regardless of
// the device id's shape. A raw prefix would collide for two ids that share a
// leading hex group (e.g. UUIDv7 device ids paired in the same time window),
// re-creating the retained-lane collision this discriminator exists to break.
const retainedOriginDiscriminatorLen = 8

// RetainedWireEventID returns the wire/outbox EventID for the lane=retained
// companion of the conversation head event with the given EventID:
// eventID + "-r-" + the leading 8 hex chars of sha256(ORIGIN device id) (plain
// eventID + "-r" when the origin id is empty — an unpaired daemon). Hashing the
// FULL id (rather than a raw prefix) makes the discriminator collision-uniform
// without assuming the device id is UUID-shaped.
//
// The origin discriminator exists for the legacy re-authored-head edge: two
// devices that (via legacy-rebase histories) both re-authored a head with the
// SAME EventID under different parents would otherwise publish their retained
// events under ONE colliding wire id — and a receiver that adopted one
// origin's baseline (recording that wire id as its log tail EventID) would
// drop the other origin's different-hash retained event on the tail fast-path
// dedup before the reconcile tiebreak could run. The id stays OPAQUE to every
// receiver and transport (nothing parses it — see the proto.RemoteEvent
// EventID doc), and its ordering property vs the plain UUIDv7 ids peers
// advertise as AlignedEventID is unchanged: any "-r…" extension is a strict
// suffix, so it still sorts AFTER its base EventID.
//
// Exported so the daemon transport layer's tests can pin the two-lane outbox
// contract against the same derivation.
func RetainedWireEventID(eventID, originDeviceID string) string {
	if originDeviceID == "" {
		return eventID + retainedEventIDSuffix
	}
	sum := sha256.Sum256([]byte(originDeviceID))
	disc := hex.EncodeToString(sum[:])[:retainedOriginDiscriminatorLen]
	return eventID + retainedEventIDSuffix + "-" + disc
}

// OutboundEvent is the orchestrator's shape for "an event just
// committed to the canonical store, here's everything a remote
// transport needs to publish it." Mirrors the wire shape of
// internal/plugin/proto.RemoteEvent but doesn't take a dependency
// on that package (the daemon's RemoteRunner translates between
// them).
type OutboundEvent struct {
	ProjectID                      string
	ProjectAuthorizationGeneration uint64
	AccessGeneration               uint64
	AccessSetHash                  [32]byte
	SecurityBarrierID              [32]byte
	SecurityGeneration             uint64
	KeyMode                        string
	KeyVersion                     uint64
	// CheckpointCoverage and CheckpointGeneration are populated only by the
	// explicit durable checkpoint-recovery worker. Ordinary retained twins leave
	// both fields empty and let the plugin's sparse policy select suppression or
	// checkpointing. An obligation checkpoint binds the exact durable position
	// it replaces and the exact authenticated recipient/security generation.
	CheckpointCoverage   uint64
	CheckpointGeneration string
	NamespaceID          string
	BranchID             string
	ArtifactID           string
	EventID              string
	ParentHash           string
	// CheckpointAlignmentHash is populated only for the retained checkpoint
	// companion and names the canonical head covered by that checkpoint. It is
	// separate from ParentHash, the event's real canonical predecessor.
	CheckpointAlignmentHash string
	// EventHash is the canonical ACF content hash of the event sealed in Bytes.
	// It is opaque routing/ancestry metadata for durable parent recovery.
	EventHash   string
	Kind        string // acf.Kind as a string for cross-language consumers
	Type        string // event type (create / update / fork / merge / …)
	Timestamp   time.Time
	Bytes       []byte // canonical bytes; the remote plugin envelope-encrypts before transmitting
	Sequence    uint64
	Origin      string // device ID
	SourceAgent string // content-free provenance for cloud sync activity analytics
	// Lane routes the event on the transport: LaneLive or LaneRetained (see
	// the const docs). A committed conversation head publishes one event per
	// lane — the live lane under the head's own EventID, the retained lane
	// under the DISTINCT, origin-scoped RetainedWireEventID (head + "-r-" +
	// origin discriminator) so EventID-keyed transport bookkeeping (durable
	// outbox, retries) never conflates the lanes and two origins re-authoring
	// one head never collide. Empty only on legacy pre-lane events (e.g. an
	// old persisted outbox entry replayed after upgrade).
	Lane string
	// Clear marks a lane=retained event as a retained-slot CLEAR: Bytes is
	// EMPTY and the transport must clear the artifact's retained slot (MQTT:
	// publish an empty retained payload) instead of performing a normal
	// publish. Emitted when a redaction leaves the artifact with no
	// retainable state, so the broker stops serving the last pre-redaction
	// snapshot to future subscribers (which would otherwise resurrect
	// redacted content via baseline adoption). Ids only — zero-knowledge.
	Clear bool
}

// ruleInputFor builds the syncrules projection for an artifact. originAgent is
// the agent attributed as the artifact's origin (the local fan-out path passes
// the primary adapter; the outbound remote path passes the head event's source
// agent). headBranch is the raw branch of the artifact's head event (Event.Branch)
// — ruleInputFor normalizes it through acf.NormalizeBranchName into BranchName so a
// match.branchName regex predicate (BRD-05 §5.2) can be evaluated; the same
// normalization remote_sync.go applies when deriving the outbound BranchID.
// Shared by fanOut and forwardCommitted so the two evaluations can never drift —
// both must see the SAME projection or a scope/project/path/branch-keyed exclude
// rule could be honored on one path and ignored on the other.
func ruleInputFor(art acf.Artifact, originAgent, headBranch string) syncrules.Artifact {
	in := syncrules.Artifact{
		ArtifactID:  art.ArtifactID,
		Kind:        string(art.Kind),
		Type:        string(art.Kind),
		Tags:        art.Tags,
		ScopeKind:   string(art.Scope),
		OriginAgent: originAgent,
		NativePath:  art.SourcePath,
		BranchName:  ruleBranchName(headBranch),
	}
	if art.Project != nil {
		in.ProjectID = art.Project.ID
		in.ProjectEphemeral = art.Project.Ephemeral
	}
	return in
}

// ruleBranchName normalizes a raw head-event branch into the canonical branch
// name the syncrules match.branchName predicate is evaluated against. It mirrors
// remote_sync.go's outbound BranchID derivation: a wire-empty branch maps to
// acf.MainBranch, and any other value is run through acf.NormalizeBranchName so a
// rule's regex sees the same lower-cased/hyphenated form the store + relay use.
// A value acf.NormalizeBranchName cannot normalize falls back to MainBranch — a
// branch-scoped rule then simply won't match it, preserving deny-by-default
// rather than letting an un-normalizable name slip an artifact past a regex gate.
func ruleBranchName(raw string) string {
	if raw == "" {
		return acf.MainBranch
	}
	norm, err := acf.NormalizeBranchName(raw)
	if err != nil {
		return acf.MainBranch
	}
	return norm
}

func selectedBranchForAgent(art acf.Artifact, agent string) string {
	if art.MaterializedBranchByAgent != nil {
		if branch := art.MaterializedBranchByAgent[agent]; branch != "" {
			if norm, err := acf.NormalizeBranchName(branch); err == nil {
				return norm
			}
		}
	}
	return acf.MainBranch
}

// rulesSuppressionReason distinguishes "a rule excluded this target" from
// "there are no rules at all". Both deny, but they are completely different
// operator problems: the first is a routing choice to inspect, the second is
// a device where cross-agent sync is structurally off. Conflating them makes
// an outage difficult to diagnose.
func (o *Orchestrator) rulesSuppressionReason(allowed map[string]struct{}) SuppressionReason {
	if len(allowed) == 0 || o.rulesEngineIsEmpty() {
		return ReasonNoRulesConfigured
	}
	return ReasonRulesDenied
}

// conversationRuleReason reports why the conversation rules gate denied a
// target: no rules configured at all, or a rule that excluded it.
func (o *Orchestrator) conversationRuleReason() SuppressionReason {
	if o.rulesEngineIsEmpty() {
		return ReasonNoRulesConfigured
	}
	return ReasonRulesDenied
}

// rulesEngineIsEmpty reports the fail-closed-by-absence state: a NON-nil
// engine holding zero rules, which denies every fan-out target. A nil engine
// means rules are disabled entirely and fans out to everything, so it is NOT
// this state.
func (o *Orchestrator) rulesEngineIsEmpty() bool {
	eng := o.rulesEngine()
	return eng != nil && len(eng.Rules()) == 0
}

func (o *Orchestrator) conversationRuleAllowsTarget(art acf.Artifact, originAgent, targetAgent, branch string) bool {
	eng := o.rulesEngine()
	if eng == nil {
		return true
	}
	adapterNames := make([]string, 0, len(o.cfg.Adapters))
	for _, ad := range o.cfg.Adapters {
		adapterNames = append(adapterNames, ad.Name())
	}
	decision := eng.Evaluate(ruleInputFor(art, originAgent, branch), syncrules.EvaluateOpts{
		InstalledAgents: adapterNames,
	})
	for _, allowed := range decision.AllowedAgents {
		if allowed == targetAgent {
			return true
		}
	}
	return false
}

// Orchestrator watches a directory and propagates changes among the
// configured adapters via canonical-store import + fan-out export.
type Orchestrator struct {
	nativeRestoreGate       sync.RWMutex
	nativeRestoreGeneration atomic.Uint64
	mu                      sync.Mutex // guards cfg.SnapshotCadence (hot-reloaded via SetSnapshotCadence) and lastActivity; other Config fields are construction-time immutable
	cfg                     Config
	watcher                 *watcher.Watcher
	debouncer               *watcher.Debouncer
	guard                   *RecursionGuard
	srcCloser               func() error // closes the underlying Source (non-recursive: via Watcher; recursive: via RecursiveSource directly)

	// extraWatchers are the watchers for cfg.AdditionalRoots (discovered
	// native global roots, FR-03.3). They share o.debouncer with the
	// primary watcher, so events from any root funnel through the same
	// onSettled -> handleEvent pipeline. extraClosers closes their sources.
	extraWatchers []*watcher.Watcher
	extraClosers  []func() error

	// watchedFolders maps an abs path added via WatchFolder to its live
	// watcher, so UnwatchFolder can find and close that specific watcher (and
	// so re-watching the same path stays an idempotent no-op). Guarded by o.mu.
	watchedFolders map[string]*watcher.Watcher

	// watchingFolders marks paths whose runtime project watcher is being
	// opened outside o.mu. It keeps duplicate WatchFolder calls idempotent
	// without holding the orchestration mutex across fsnotify setup.
	watchingFolders map[string]struct{}

	// sourcePathLocks serializes the handleEvent import pipeline per source
	// path. The watcher debouncer settle, the 5s native live scan, and the
	// recent-session hot scanners can all reach handleEvent for the SAME file
	// within one import's duration; without serialization each pipeline runs
	// the find-or-create/append dance concurrently and can duplicate the artifact
	// or its update event. Zero value is ready to use.
	sourcePathLocks pathLockSet

	// nativeReimportMu guards the diverged-import nudge's two pieces of state:
	// which (artifact, destination bytes) pairs have already been re-read, so an
	// unchanged file is never parsed twice for the same reason, and when the
	// recent nudges ran, so the device-wide rate cap can be enforced. Zero value
	// is ready to use.
	nativeReimportMu   sync.Mutex
	nativeReimportSeen map[string]sessionWriteWitness
	nativeReimportAt   []time.Time

	// importSlots bounds expensive native import pipelines across DIFFERENT
	// paths. Recursive watcher startup can settle hundreds of session files at
	// once; without a global gate every timer goroutine parses/materializes in
	// parallel, consuming multiple cores and many gigabytes. Two total slots
	// leave one latency lane available while a large active rollout is parsed;
	// largeImportSlots still permits at most one large import at a time, so a
	// startup history storm cannot run two memory-heavy parses concurrently.
	// Both nil retains zero-value test compatibility.
	importSlots      chan struct{}
	largeImportSlots chan struct{}

	// destHashes tracks, per native file path, a fingerprint of the content
	// the orchestrator last WROTE there (fan-out Export) or last IMPORTED
	// from there. Before a fan-out overwrites a destination, the current file
	// is compared against this record: a mismatch means the file changed
	// under us — an agent-side edit whose watcher event hasn't imported yet.
	// The export is deferred so the pending import lands first (and the
	// conflict detector sees both heads) instead of the fan-out clobbering
	// the edit and the recursion guard then swallowing its event as an echo
	// of our own write (E2E F6: one of two near-simultaneous divergent edits
	// was silently destroyed). Files larger than maxDestHashBytes carry a
	// size/mtime-only fingerprint (hash == ""), so oversized session files
	// still get edit-under-us detection. Guarded by o.mu.
	destHashes map[string]destFingerprint

	// nativeMirrorTopology remembers each multi-surface adapter's last
	// observed app-worktree topology token. A token change schedules a narrow
	// non-conversation re-fanout so already-synchronized project context reaches
	// a worktree created after the original artifact event. Guarded by o.mu.
	nativeMirrorTopology map[string]string

	// runtimeDiscoveryTokens remembers the independently detected CLI/Desktop
	// surface set and shared-storage roots for adapters that can appear after
	// daemon startup. A change to an installed state triggers the bounded
	// conversation backfill that a normal live artifact event would have
	// performed if the adapter had been present at the time. Guarded by o.mu.
	runtimeDiscoveryTokens    map[string]string
	runtimeDiscoveryInstalled map[string]bool
	runtimeSafetyTokens       map[string]string
	runtimeActivationMu       sync.Mutex

	// deferredMaterialize holds artifact IDs whose otherwise-eligible native
	// fan-out was withheld by AdapterBlocker or safely declined because a native
	// session was changing/ahead. Neither transition necessarily creates a new
	// canonical event, so without this queue a committed update can remain
	// permanently absent from the target. Entries are deduplicated, persisted as
	// metadata-only retry tokens, and bounded per adapter; an overflow coalesces
	// to one target-only canonical reconciliation pass.
	deferredMaterializeMu     sync.Mutex
	deferredMaterialize       map[string]*deferredMaterializationQueue
	adapterBlockerUnsubscribe func()

	// convergence is the periodic self-heal scheduler's memory (guarded by
	// o.mu). It exists so a write lost to a closed gate, an exhausted retry
	// budget or a missed watcher event is eventually re-driven without a
	// human running `aplexica daemon reload`.
	convergence convergenceState

	// suppressions records every decision NOT to write a materialization
	// target. It is an observation surface only: it never retries (the
	// deferred queue does) and never repairs (the convergence reconciler
	// does). Before it existed, a denied target was a bare `continue` and
	// fanOut then returned nil — so a device whose rules engine denied
	// everything reported perfect health while all cross-agent sync was
	// dead. Bounded by (agent x reason), so it is safe on the hot path.
	suppressions *suppressionLedger

	// oversizeReported tracks, per refused path, when its artifact.refused bus
	// event last surfaced, so the 5s scan's re-refusals of the same oversized
	// file don't flood the event stream (see oversizeReportInterval). An entry
	// clears when the path passes the size gate again, so a shrink→regrow
	// cycle re-reports promptly. Guarded by o.mu.
	oversizeReported map[string]time.Time

	// scanCache remembers each file's (size, mtime) at its last successful
	// import, persisted under the store root. handleEvent consults it to skip
	// re-importing — and re-encoding — files unchanged since the previous run,
	// which is the dominant cost of the startup InitialScan on a large history.
	// Has its own internal lock; never accessed under o.mu.
	scanCache *importScanCache

	// Close-join lifecycle. Every orchestrator-owned background goroutine and
	// externally-driven scan entry point registers with bgWG via
	// beginBackground, and Close blocks on bgWG.Wait() after signalling bgDone
	// — so no orchestrator goroutine can still be importing or materializing
	// files after Close returns (an unjoined scan tick or detached
	// conversation fan-out writing into a directory the caller is deleting
	// was the TestNativeLiveScan TempDir flake). bgClosing (guarded by bgMu)
	// makes registration-vs-Close atomic: a goroutine either registers before
	// Close begins (Close waits for it) or observes bgClosing and does no
	// work at all. bgDone doubles as the "stop even if the caller's ctx is
	// still live" signal for the tick loops. Nil bgDone (a zero-value
	// Orchestrator in unit tests) degrades to the pre-join behavior.
	bgMu      sync.Mutex
	bgClosing bool
	bgDone    chan struct{}
	bgOnce    sync.Once
	bgWG      sync.WaitGroup

	// forcedBackfillActive serializes StartForcedConversationBackfill: only
	// one forced full-history pass may run at a time so two passes can't
	// interleave their materializations over the same native session files.
	forcedBackfillActive atomic.Bool

	// localDeviceIDOverride, when set via SetLocalDeviceID, supersedes
	// cfg.LocalDeviceID for every identity read. A re-pair rotates the
	// cloud-assigned device id while the daemon keeps running; without a
	// runtime override the orchestrator would stamp the RETIRED id on every
	// outbound event until restart and cause publisher_identity_conflict after a
	// re-pair. Holds a string.
	localDeviceIDOverride atomic.Value

	// originLeakWarned rate-limits the non-UUID outbound-origin warning to
	// once per process (the same unpaired adapter provenance would otherwise
	// warn on every event).
	originLeakWarned atomic.Bool

	// convBackfill caps, per agent, how many of the most-recent conversations
	// are materialized into that agent during a backfill pass (RefanOutAll) —
	// bounds the "enable an agent → replicate every other agent's entire
	// conversation history into it" flood. Missing agent → DefaultConvBackfill;
	// negative → unlimited. Live fan-out is never capped. Guarded by o.mu.
	convBackfill map[string]int

	// lastActivity is the wall-clock time the orchestrator last
	// completed a successful primary-import + fan-out cycle. Consumed
	// by daemon.ControlServer via the LastActivity() getter (which
	// satisfies daemon.Activity); the value is overlaid onto the
	// StatusInfo's LastActivity field on each "status" control-socket
	// request so the tray indicator's Active heuristic doesn't have to
	// proxy via snapshot-arrival tick liveness (v0.39.0).
	lastActivity time.Time

	// adapterTouched is the per-adapter last-success timestamp, keyed
	// by adapter.Name() (e.g. "claudecode", "codex", "hermes", "kilo",
	// "openclaw"). Updated on successful primaryImport and on
	// successful per-adapter Export inside fanOut. Consumed by
	// AdapterStates() to bucket each adapter as "active" (touched
	// recently) or "idle" — ADR-0159 Candidate B (v0.51.0).
	adapterTouched map[string]time.Time

	// adapterLastErr is the per-adapter most-recent error string,
	// keyed by adapter.Name(). Recorded when an adapter's Export
	// errors inside fanOut. Cleared on next successful Import/Export
	// for the same adapter. Paths under $HOME are redacted to ~/
	// before storage. ADR-0159 Candidate D (v0.51.0).
	adapterLastErr map[string]string

	// remoteOrigins is the set of device IDs the orchestrator has observed
	// as the origin of an inbound (remote-authored) event. It is the
	// loop-prevention discriminator: the outbound path in handleEvent never
	// forwards an event whose provenance device id is in this set, so a
	// remote event that gets re-materialized to a native file and re-imported
	// can never bounce back out to the relay. Guarded by o.mu. Empty in the
	// OSS-only / unpaired case (no inbound ever arrives).
	remoteOrigins map[string]struct{}

	// remoteRepublishedHeads records, per artifact+branch, the last local head hash
	// the retained/live sync safety pass handed to the remote publisher AND the
	// recipient-set fingerprint it was sealed for (see remoteRepublishedHead).
	// The live watcher path normally publishes immediately, but the driver
	// also re-checks changed local heads every short tick to recover from a
	// missed watcher/import notification without flooding the relay with
	// unchanged heads. Keying on the recipient fingerprint too means a head
	// sealed while the roster was degraded (e.g. the resolver's self-only
	// fallback) republishes when the roster recovers/changes — a retained
	// baseline no peer can decrypt would otherwise stay stale until the next
	// head change. Guarded by o.mu.
	remoteRepublishedHeads map[string]remoteRepublishedHead

	// remoteRetainedOversized records, per artifact+branch, the head hash whose
	// SEALED lane=retained baseline exceeded remotePublishRetainedMaxBytes
	// (design rule 6's acknowledged residual: no transport path exists for a
	// baseline that large). While the artifact's head is unchanged, the
	// republish sweep and the backfill trickle SKIP it — every re-attempt
	// would re-materialize and re-seal hundreds of MB only to be refused
	// again. Keyed by head hash, so any head change (a redaction or
	// compaction may shrink the state below the cap) makes the artifact
	// eligible again; a successful retained publish or retained-slot clear
	// removes the entry. Bounded by the (small) set of over-cap
	// conversations; lazily allocated; guarded by o.mu.
	remoteRetainedOversized map[string]string

	// remoteLargeRetainedAttempts rate-limits full retained-baseline rebuilds
	// for very large native conversations. Live deltas remain immediate; the
	// retained recovery point is refreshed periodically or immediately when the
	// recipient roster changes. Guarded by o.mu.
	remoteLargeRetainedAttempts map[string]largeRetainedAttempt

	// remoteBackfillAttempted records the artifact+branch heads whose most recent slow
	// retained-baseline backfill attempt (BackfillLocalRemoteHeads) was
	// DECLINED this daemon run (a successful publish refunds the entry).
	// Without it, an artifact whose publish is persistently declined
	// (route.remote=exclude, seal failure) would occupy the oldest-first line
	// forever and wedge the trickle. Bounded by store artifact count; lazily
	// allocated; guarded by o.mu.
	remoteBackfillAttempted map[string]struct{}

	// needsBaseline tracks conversation artifacts whose lane=live delta
	// arrived with an unknown parent (ImportDeferredNeedsBaseline): the
	// artifact cannot extend its chain natively until a lane=retained
	// baseline re-aligns it. The value is the last time remote.needs_baseline
	// was published for the artifact (the per-artifact notify throttle,
	// needsBaselineNotifyInterval). Bounded at needsBaselineMaxEntries
	// (stalest entry evicted); an entry clears on baseline adoption and on a
	// successful native live append. Lazily allocated; guarded by o.mu.
	// Aligned-chains delta sync, 2026-07.
	needsBaseline map[string]time.Time

	// sourcePathHeads indexes artifact heads by native SourcePath so the hot
	// handleEvent path can snapshot "before import" heads without scanning the
	// whole store for every watched file. Guarded by o.mu.
	sourcePathHeads map[string]map[string]string

	// recentCodexScanMu guards only the recent-session scheduler state below; it
	// is never held while an import waits on the global/large admission gates or
	// a per-path lock. That separation is what lets a later small rollout use the
	// reserved fast lane while one historical rollout remains blocked. The
	// in-flight set prevents overlapping poll ticks from queueing duplicate work;
	// sourcePathLocks remains the final serialization boundary against watcher
	// and startup-scan delivery of the same path.
	recentCodexScanMu        sync.Mutex
	recentCodexInFlight      map[string]struct{}
	recentCodexLargeInFlight bool

	// recentClaudeScanMu prevents overlapping hot scans over ~/.claude/projects.
	// A skipped tick is fine: the next 500ms poll catches it. It also guards
	// recentClaudeHot.
	recentClaudeScanMu sync.Mutex

	// recentClaudeHot is the small set of Claude Code session JSONL files known
	// to be active: files just imported locally or just materialized from a
	// remote device. Polling this set catches fast continuations without walking
	// the entire ~/.claude/projects tree every tick.
	recentClaudeHot map[string]time.Time

	// convMaterializeMu guards convMaterializeLocks; each entry serializes
	// native-session materialization per conversation artifact so a racing
	// stale fan-out cannot overwrite a newer head (the head is re-read inside
	// the per-artifact critical section). Bounded by conversation count.
	convMaterializeMu    sync.Mutex
	convMaterializeLocks map[string]*sync.Mutex

	// largeMaterializePending is the per-artifact trailing-edge debounce
	// state for LARGE-conversation materialization (see the
	// largeMaterializeThreshold var block): each entry holds the armed
	// time.AfterFunc timer plus the newest session/doc plans to re-run when
	// it fires. Entries clear on fire; Close stops any still-armed timers.
	// Lazily allocated; guarded by largeMaterializeMu (never held across a
	// write — the flush re-locks per plan via lockConversationMaterialize).
	largeMaterializeMu      sync.Mutex
	largeMaterializePending map[string]*pendingLargeMaterialize

	// pendingProjectsCache keeps the 5-second tray/status watcher from walking
	// and decoding every artifact metadata file on every poll. The canonical
	// store can contain thousands of artifacts; pending-project membership
	// changes far less frequently than status is requested.
	pendingProjectsCacheMu sync.Mutex
	pendingProjectsCacheAt time.Time
	pendingProjectsCache   []map[string]any
}

const pendingProjectsCacheTTL = 30 * time.Second

// NewOrchestrator constructs an Orchestrator. Returns an error if the
// directory or store cannot be initialized, or if the config is invalid.
// The returned Orchestrator is NOT running yet; call Run to start it.
func NewOrchestrator(cfg Config) (*Orchestrator, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("syncd: Dir is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("syncd: Store is required")
	}
	// Zero adapters is a valid idle configuration, not an error: since install
	// discovery gates the adapter set on real install signals, a machine with
	// no AI agents resolves an empty set - the daemon must still start (serve
	// status/UI, accept remote events) and simply import/materialize nothing.
	// Every Adapters consumer either iterates the slice or explicitly guards
	// the empty case (see materializeInbound).
	abs, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("syncd: resolve dir: %w", err)
	}
	cfg.Dir = abs
	if cfg.QuietPeriod <= 0 {
		cfg.QuietPeriod = 500 * time.Millisecond
	}
	if cfg.GuardWindow <= 0 {
		cfg.GuardWindow = 5 * time.Second
	}
	if cfg.ConflictWindow <= 0 {
		cfg.ConflictWindow = 30 * time.Second
	}

	// Adapters sorted alphabetically for deterministic primary selection.
	sort.Slice(cfg.Adapters, func(i, j int) bool {
		return cfg.Adapters[i].Name() < cfg.Adapters[j].Name()
	})

	needsProjectionRepairMigration := deferredMaterializationProjectionMigrationNeeded(cfg.Store.Root)
	deferredMaterialize, deferredLoadErr := loadDeferredMaterializationQueues(cfg.Store.Root)
	if deferredLoadErr != nil {
		// The marker is an optimization over canonical truth, but treating a
		// corrupt/unreadable marker as empty could silently lose a blocked write.
		// Fail safe by reconciling every configured target once.
		deferredMaterialize = map[string]*deferredMaterializationQueue{}
		for _, ad := range cfg.Adapters {
			queue := newDeferredMaterializationQueue()
			queue.generation = 1
			queue.overflow = true
			queue.conversationsOnly = false
			deferredMaterialize[ad.Name()] = queue
		}
		if cfg.Logger != nil {
			cfg.Logger.Warn("recover native materialization deferrals", "err", deferredLoadErr)
		}
	}
	o := &Orchestrator{
		cfg:          cfg,
		guard:        NewRecursionGuard(cfg.GuardWindow),
		suppressions: newSuppressionLedger(),
		// v0.51.0: pre-populate adapterTouched with a zero timestamp
		// per configured adapter so AdapterStates() always enumerates
		// the full set as "idle" until a real touch lands. Without
		// this, freshly-started daemons would return an empty map and
		// the tray UI couldn't distinguish "no adapters" from "all
		// idle."
		adapterTouched: func() map[string]time.Time {
			m := make(map[string]time.Time, len(cfg.Adapters))
			for _, ad := range cfg.Adapters {
				m[ad.Name()] = time.Time{}
			}
			return m
		}(),
		adapterLastErr:            map[string]string{},
		watchedFolders:            map[string]*watcher.Watcher{},
		watchingFolders:           map[string]struct{}{},
		destHashes:                map[string]destFingerprint{},
		nativeMirrorTopology:      map[string]string{},
		runtimeDiscoveryTokens:    map[string]string{},
		runtimeDiscoveryInstalled: map[string]bool{},
		runtimeSafetyTokens:       map[string]string{},
		deferredMaterialize:       deferredMaterialize,
		oversizeReported:          map[string]time.Time{},
		remoteOrigins:             map[string]struct{}{},
		remoteRepublishedHeads:    buildRemoteRepublishedHeadIndex(cfg.Store),
		sourcePathHeads:           buildSourcePathHeadIndex(cfg.Store),
		scanCache:                 loadImportScanCache(cfg.Store.Root),
		bgDone:                    make(chan struct{}),
		importSlots:               make(chan struct{}, 2),
		largeImportSlots:          make(chan struct{}, 1),
	}
	// Runtime-discoverable adapters supplied in cfg.Adapters were already
	// discovered during daemon startup. Restore the last processed topology, or
	// establish it on the first release carrying this cache. This prevents every
	// unchanged restart from re-fanning the full store while preserving a real
	// absent-to-installed transition that happened while the daemon was down.
	// Safety tokens are deliberately not seeded: the activation hook must still
	// run before the first adapter side effect in this process.
	o.seedRuntimeDiscoveryBaseline()
	o.debouncer = watcher.NewDebouncerWithCommit(cfg.QuietPeriod, o.onSettled)
	if err := o.openConfiguredWatchers(); err != nil {
		return nil, err
	}
	if cfg.AdapterBlocker != nil {
		o.adapterBlockerUnsubscribe = cfg.AdapterBlocker.SubscribeClears(o.resumeDeferredMaterializationAfterUnblock)
	}
	if needsProjectionRepairMigration && deferredLoadErr == nil {
		o.seedLocalConversationProjectionRepairs()
		o.deferredMaterializeMu.Lock()
		if err := o.persistDeferredMaterializationLocked(); err != nil && o.cfg.Logger != nil {
			o.cfg.Logger.Warn("persist native materialization migration state", "err", err)
		}
		o.deferredMaterializeMu.Unlock()
	}
	o.seedPlatformMissingRemoteConversationProjections()
	for target := range deferredMaterialize {
		o.scheduleDeferredMaterializationDrain(target)
	}
	return o, nil
}

func (o *Orchestrator) seedRuntimeDiscoveryBaseline() {
	if !o.cfg.DynamicAdapterDiscovery {
		return
	}
	if states, ok := loadRuntimeDiscoveryCache(o.cfg.Store.Root); ok {
		for name, state := range states {
			o.runtimeDiscoveryTokens[name] = state.Token
			o.runtimeDiscoveryInstalled[name] = state.Installed
		}
		return
	}
	states := map[string]runtimeDiscoveryState{}
	for _, ad := range o.cfg.Adapters {
		if _, ok := ad.(adapter.RuntimeDiscoverable); !ok {
			continue
		}
		discovery, err := ad.Discover()
		if err != nil {
			// A failed startup observation is intentionally not cached. If the
			// adapter becomes discoverable later, the live scan treats that as a
			// real activation and performs the missed-artifact backfill.
			continue
		}
		token := runtimeDiscoveryToken(discovery)
		o.runtimeDiscoveryTokens[ad.Name()] = token
		o.runtimeDiscoveryInstalled[ad.Name()] = discovery.Installed
		states[ad.Name()] = runtimeDiscoveryState{Token: token, Installed: discovery.Installed}
	}
	// This cache is an optimization and migration checkpoint; a write failure
	// must never prevent the daemon from starting.
	_ = writeRuntimeDiscoveryCache(o.cfg.Store.Root, states)
}

func (o *Orchestrator) persistRuntimeDiscoveryBaseline() {
	if !o.cfg.DynamicAdapterDiscovery || o.cfg.Store == nil {
		return
	}
	o.mu.Lock()
	states := make(map[string]runtimeDiscoveryState, len(o.runtimeDiscoveryTokens))
	for name, token := range o.runtimeDiscoveryTokens {
		states[name] = runtimeDiscoveryState{
			Token:     token,
			Installed: o.runtimeDiscoveryInstalled[name],
		}
	}
	o.mu.Unlock()
	_ = writeRuntimeDiscoveryCache(o.cfg.Store.Root, states)
}

func (o *Orchestrator) openConfiguredWatchers() error {
	watchers, closers, primary, primaryCloser, err := o.buildConfiguredWatchers()
	if err != nil {
		return err
	}
	o.watcher = primary
	o.srcCloser = primaryCloser
	o.extraWatchers = watchers
	o.extraClosers = closers
	return nil
}

func (o *Orchestrator) buildConfiguredWatchers() ([]*watcher.Watcher, []func() error, *watcher.Watcher, func() error, error) {
	var primary *watcher.Watcher
	var primaryCloser func() error
	if o.cfg.Recursive {
		src, serr := watcher.NewRecursiveSource(o.cfg.Dir)
		if serr != nil {
			return nil, nil, nil, nil, fmt.Errorf("syncd: recursive watcher: %w", serr)
		}
		primary = watcher.NewWatcherWithSource(src, o.debouncer)
		primaryCloser = src.Close
	} else {
		w, err := watcher.NewWatcher(o.cfg.Dir, o.debouncer)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("syncd: watcher: %w", err)
		}
		primary = w
		primaryCloser = w.Close
	}

	var extras []*watcher.Watcher
	var closers []func() error

	// FR-03.3 §4: watch each discovered native global root with its own
	// watcher feeding the SAME debouncer (so events funnel through one
	// onSettled -> handleEvent pipeline). An unreadable root is skipped,
	// not fatal — a half-installed agent must not stop the daemon.
	for _, r := range o.cfg.AdditionalRoots {
		abs, aerr := filepath.Abs(r)
		if aerr != nil {
			continue
		}
		if info, statErr := os.Stat(abs); statErr != nil || !info.IsDir() {
			// Runtime-discoverable adapters contribute candidate roots before
			// installation. The native live scan keeps polling them; watcher
			// construction should stay silent until the directory exists.
			continue
		}
		if o.cfg.Recursive {
			src, serr := watcher.NewRecursiveSource(abs)
			if serr != nil {
				o.onWatcherError(fmt.Errorf("recursive native root %s: %w", abs, serr))
				continue
			}
			extras = append(extras, watcher.NewWatcherWithSource(src, o.debouncer))
			closers = append(closers, src.Close)
		} else {
			w, werr := watcher.NewWatcher(abs, o.debouncer)
			if werr != nil {
				o.onWatcherError(fmt.Errorf("native root %s: %w", abs, werr))
				continue
			}
			extras = append(extras, w)
			closers = append(closers, w.Close)
		}
	}

	// RecursiveRoots and read-only MetadataRoots are ALWAYS watched recursively
	// (regardless of cfg.Recursive). Both use the same debouncer/import pipeline;
	// MetadataRoots are excluded from backup by discovery construction.
	for _, r := range append(append([]string{}, o.cfg.RecursiveRoots...), o.cfg.MetadataRoots...) {
		abs, aerr := filepath.Abs(r)
		if aerr != nil {
			continue
		}
		if info, statErr := os.Stat(abs); statErr != nil || !info.IsDir() {
			continue
		}
		src, serr := watcher.NewRecursiveSource(abs)
		if serr != nil {
			o.onWatcherError(fmt.Errorf("recursive native root %s: %w", abs, serr))
			continue
		}
		extras = append(extras, watcher.NewWatcherWithSource(src, o.debouncer))
		closers = append(closers, src.Close)
	}

	// WatchFiles: watch each file's PARENT dir, filtered down to just that
	// file — single config files outside any directory root (e.g.
	// ~/.claude.json, where `claude mcp add -s user` writes).
	for _, f := range o.cfg.WatchFiles {
		abs, aerr := filepath.Abs(f)
		if aerr != nil {
			continue
		}
		if info, statErr := os.Stat(filepath.Dir(abs)); statErr != nil || !info.IsDir() {
			continue
		}
		inner, serr := watcher.New(filepath.Dir(abs))
		if serr != nil {
			o.onWatcherError(fmt.Errorf("watched file %s: %w", abs, serr))
			continue
		}
		target := abs
		src := watcher.NewFilteredSource(inner, func(p string) bool { return p == target })
		extras = append(extras, watcher.NewWatcherWithSource(src, o.debouncer))
		closers = append(closers, src.Close)
	}

	// FR-03.5 / BRD-03 §4.3: surface platform watcher Source errors (inotify
	// budget / ENOSPC polling fallback / Windows RDC overflow) through the
	// status channel instead of draining them silently into the void.
	primary.OnError = o.onWatcherError
	for _, w := range extras {
		w.OnError = o.onWatcherError
	}
	return extras, closers, primary, primaryCloser, nil
}

// ReopenWatchersBeforeRun discards watcher sources that were opened during
// orchestrator construction and opens fresh ones immediately before Run starts.
// Daemon startup intentionally performs a long synchronous InitialScan first;
// on macOS, recursive FSEvents sources can otherwise accumulate unconsumed
// startup events behind small channel buffers before the watch loop begins.
//
// Call only before Run. Runtime WatchFolder registrations are not preserved.
func (o *Orchestrator) ReopenWatchersBeforeRun() error {
	o.mu.Lock()
	oldPrimaryCloser := o.srcCloser
	oldClosers := append([]func() error(nil), o.extraClosers...)
	o.mu.Unlock()

	// Release the construction-time sources before opening replacements. On
	// macOS recursive roots can own hundreds of FSEvents streams; opening the
	// fresh tree before closing the stale one can exhaust per-process stream
	// resources and silently drop a native root.
	for _, c := range oldClosers {
		if c != nil {
			_ = c()
		}
	}
	if oldPrimaryCloser != nil {
		_ = oldPrimaryCloser()
	}

	watchers, closers, primary, primaryCloser, err := o.buildConfiguredWatchers()
	if err != nil {
		return err
	}

	o.mu.Lock()
	o.watcher = primary
	o.srcCloser = primaryCloser
	o.extraWatchers = watchers
	o.extraClosers = closers
	o.watchedFolders = map[string]*watcher.Watcher{}
	o.mu.Unlock()
	return nil
}

// Run blocks until ctx is cancelled, processing watcher events. Extra
// watchers (cfg.AdditionalRoots) run concurrently in their own goroutines;
// they all feed the shared debouncer, so handleEvent sees events from every
// root. ctx cancellation stops them all.
func (o *Orchestrator) Run(ctx context.Context) {
	if !o.beginBackground() {
		return // Close already began; never start watchers after teardown
	}
	defer o.endBackground()
	if o.cfg.LiveScanInterval > 0 {
		// runLiveScan owns its Close-join registration. Registration belongs
		// inside the entry point so direct embedders/tests receive the same
		// teardown guarantee as Run without a register-after-Close race.
		go o.runLiveScan(ctx, o.cfg.LiveScanInterval)
	}
	// Snapshot under o.mu: WatchFolder appends to extraWatchers concurrently
	// (the daemon's boot-window seed loop races Run startup). A watcher
	// appended after this snapshot is not missed — WatchFolder launches its
	// goroutine itself — and never double-started, because it is not in the
	// snapshot.
	o.mu.Lock()
	extras := append([]*watcher.Watcher(nil), o.extraWatchers...)
	o.mu.Unlock()
	for _, w := range extras {
		if !o.beginBackground() {
			return
		}
		go func(w *watcher.Watcher) {
			defer o.endBackground()
			w.Run(ctx)
		}(w)
	}
	o.watcher.Run(ctx)
}

func (o *Orchestrator) runLiveScan(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	if !o.beginBackground() {
		return
	}
	defer o.endBackground()
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.bgDone:
			return
		case <-time.After(interval):
			_ = o.scanConfiguredRoots(ctx)
		}
	}
}

// RunNativeLiveScan starts a focused runtime catch-up scanner over agent-owned
// native roots only (AdditionalRoots, RecursiveRoots, and WatchFiles). It is a
// faster companion to the broader LiveScanInterval full-root sweep: native
// agent histories are exactly where missed platform events hurt cross-device
// sync latency, and scanning them avoids walking the user's whole --dir every
// second.
func (o *Orchestrator) RunNativeLiveScan(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	if !o.beginBackground() {
		return
	}
	defer o.endBackground()
	hasNativeRoots := len(o.cfg.AdditionalRoots) > 0 || len(o.cfg.RecursiveRoots) > 0 || len(o.cfg.MetadataRoots) > 0 || len(o.cfg.WatchFiles) > 0
	if !hasNativeRoots && !o.hasNativeMirrorTopologySource() && !o.hasRuntimeDiscoverableAdapter() {
		select {
		case <-ctx.Done():
		case <-o.bgDone:
		}
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.bgDone:
			return
		case <-ticker.C:
			if hasNativeRoots {
				_ = o.scanNativeRoots(ctx)
			}
			o.refreshNativeMirrorTopology(ctx)
			o.refreshRuntimeAdapterDiscovery(ctx)
		}
	}
}

func (o *Orchestrator) hasNativeMirrorTopologySource() bool {
	for _, ad := range o.cfg.Adapters {
		if _, ok := ad.(adapter.NativeMirrorTopologySource); ok {
			return true
		}
	}
	return false
}

func (o *Orchestrator) hasRuntimeDiscoverableAdapter() bool {
	if !o.cfg.DynamicAdapterDiscovery {
		return false
	}
	for _, ad := range o.cfg.Adapters {
		if _, ok := ad.(adapter.RuntimeDiscoverable); ok {
			return true
		}
	}
	return false
}

func runtimeDiscoveryToken(discovery adapter.Discovery) string {
	active := make([]string, 0, len(discovery.ActiveSurfaces))
	for _, surface := range discovery.ActiveSurfaces {
		active = append(active, string(surface))
	}
	global := append([]string(nil), discovery.GlobalRoots...)
	recursive := append([]string(nil), discovery.RecursiveRoots...)
	metadata := append([]string(nil), discovery.MetadataRoots...)
	watchFiles := append([]string(nil), discovery.WatchFiles...)
	sort.Strings(active)
	sort.Strings(global)
	sort.Strings(recursive)
	sort.Strings(metadata)
	sort.Strings(watchFiles)
	return fmt.Sprintf("installed=%t;surfaces=%s;global=%s;recursive=%s;metadata=%s;files=%s;runtime=%s",
		discovery.Installed,
		strings.Join(active, ","),
		strings.Join(global, "\x00"),
		strings.Join(recursive, "\x00"),
		strings.Join(metadata, "\x00"),
		strings.Join(watchFiles, "\x00"),
		discovery.RuntimeToken,
	)
}

// ensureRuntimeAdapterSafety serializes the activation hook for one observed
// discovery topology and records its token only after the hook returns. The
// hook itself updates AdapterBlocker, so callers must run this before checking
// the blocker and before any adapter Import/Export side effect.
func (o *Orchestrator) ensureRuntimeAdapterSafety(ad adapter.Adapter, discovery adapter.Discovery) {
	if !o.cfg.DynamicAdapterDiscovery || o.cfg.RuntimeAdapterActivated == nil {
		return
	}
	if _, ok := ad.(adapter.RuntimeDiscoverable); !ok {
		return
	}
	token := runtimeDiscoveryToken(discovery)
	o.runtimeActivationMu.Lock()
	defer o.runtimeActivationMu.Unlock()
	o.mu.Lock()
	previous, seen := o.runtimeSafetyTokens[ad.Name()]
	o.mu.Unlock()
	if seen && previous == token {
		return
	}
	o.cfg.RuntimeAdapterActivated(ad.Name(), discovery)
	o.mu.Lock()
	o.runtimeSafetyTokens[ad.Name()] = token
	o.mu.Unlock()
}

func (o *Orchestrator) runtimeAdapterAvailable(ad adapter.Adapter) bool {
	if !o.cfg.DynamicAdapterDiscovery {
		return true
	}
	if _, ok := ad.(adapter.RuntimeDiscoverable); !ok {
		return true
	}
	discovery, err := ad.Discover()
	if err != nil || !discovery.Installed {
		return false
	}
	o.ensureRuntimeAdapterSafety(ad, discovery)
	return true
}

// runtimeAdapterInstalled is runtimeAdapterAvailable WITHOUT the activation
// hook: it answers "is this agent present?" and changes nothing.
//
// It exists for read-only surfaces. runtimeAdapterAvailable's
// ensureRuntimeAdapterSafety call runs cfg.RuntimeAdapterActivated on a
// topology it has not seen before, and on this daemon that closure performs the
// native startup-safety pass — snapshot verification, pruning, potentially
// copying an agent's whole native tree, and possibly BLOCKING the adapter. A
// status query must never do any of that as a side effect of being asked a
// question.
func (o *Orchestrator) runtimeAdapterInstalled(ad adapter.Adapter) bool {
	if !o.cfg.DynamicAdapterDiscovery {
		return true
	}
	if _, ok := ad.(adapter.RuntimeDiscoverable); !ok {
		return true
	}
	discovery, err := ad.Discover()
	return err == nil && discovery.Installed
}

// refreshRuntimeAdapterDiscovery handles artifacts that were committed while
// a runtime-discoverable adapter was absent. Its absent-to-installed transition
// seeds non-conversation artifacts into only that adapter. Any installed
// runtime-readiness change (such as a Desktop app-server helper arriving)
// retries bounded, remote-only conversation registration without rewriting
// locally-authored sessions. App-worktree membership is handled separately by
// the mirror-only topology refresh below.
func (o *Orchestrator) refreshRuntimeAdapterDiscovery(ctx context.Context) {
	if !o.cfg.DynamicAdapterDiscovery {
		return
	}
	activationTargets := map[string]struct{}{}
	conversationTargets := map[string]struct{}{}
	for _, ad := range o.cfg.Adapters {
		if _, ok := ad.(adapter.RuntimeDiscoverable); !ok {
			continue
		}
		discovery, err := ad.Discover()
		if err != nil {
			continue
		}
		if discovery.Installed {
			o.ensureRuntimeAdapterSafety(ad, discovery)
		}
		token := runtimeDiscoveryToken(discovery)
		o.mu.Lock()
		previous, seen := o.runtimeDiscoveryTokens[ad.Name()]
		previousInstalled, installedSeen := o.runtimeDiscoveryInstalled[ad.Name()]
		o.runtimeDiscoveryTokens[ad.Name()] = token
		o.runtimeDiscoveryInstalled[ad.Name()] = discovery.Installed
		o.mu.Unlock()
		if discovery.Installed && (!installedSeen || !previousInstalled) {
			activationTargets[ad.Name()] = struct{}{}
		}
		if discovery.Installed && (!seen || previous != token) {
			conversationTargets[ad.Name()] = struct{}{}
		}
	}
	if o.cfg.Store == nil {
		return
	}
	if len(activationTargets) > 0 {
		// An adapter that was wholly absent missed ordinary memory/skill/tool
		// fan-out. Seed only that logical adapter now; adding a second surface
		// to an already-active adapter does not rewrite its canonical files.
		o.refanNonConversationArtifactsInto(ctx, activationTargets, false, true)
	}
	if len(conversationTargets) > 0 {
		o.backfillConversationsInto(ctx, conversationTargets, true)
	}
	// Commit the observation only after its catch-up work completes. If the
	// process stops midway, the prior on-disk topology remains and the next run
	// safely retries rather than silently treating a partial replay as done.
	o.persistRuntimeDiscoveryBaseline()
}

// refreshNativeMirrorTopology detects app-managed worktrees created after the
// artifact that should populate them. It intentionally re-fans only the small
// non-conversation artifact classes; app session registration has separate
// semantics and must not be replayed on every worktree lifecycle event.
func (o *Orchestrator) refreshNativeMirrorTopology(ctx context.Context) {
	changedTargets := map[string]struct{}{}
	for _, ad := range o.cfg.Adapters {
		source, ok := ad.(adapter.NativeMirrorTopologySource)
		if !ok {
			continue
		}
		token := source.NativeMirrorTopologyToken()
		o.mu.Lock()
		previous, seen := o.nativeMirrorTopology[ad.Name()]
		if !seen || previous != token {
			o.nativeMirrorTopology[ad.Name()] = token
			changedTargets[ad.Name()] = struct{}{}
		}
		o.mu.Unlock()
	}
	if len(changedTargets) == 0 || o.cfg.Store == nil {
		return
	}
	// The first observation intentionally seeds worktrees that appeared while
	// the daemon was stopped. Mirror-only targeting prevents that baseline pass
	// (and later worktree changes) from rewriting canonical or sibling-agent
	// destinations.
	o.refanNonConversationArtifactsInto(ctx, changedTargets, true, false)
}

func (o *Orchestrator) refanNonConversationArtifactsInto(ctx context.Context, targets map[string]struct{}, mirrorsOnly, includeSameSource bool) {
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool} {
		artifacts, err := o.cfg.Store.ListArtifacts(kind)
		if err != nil {
			continue
		}
		for _, art := range artifacts {
			if ctx.Err() != nil || o.closingNow() {
				return
			}
			primary, origin := o.backfillPrimary(art)
			contextDir := ""
			if art.Scope == acf.ScopeProject && art.Project != nil {
				contextDir = art.Project.Path
			}
			sourcePath := art.SourcePath
			if !mirrorsOnly {
				// This is a catch-up pass, not a watcher echo. An adapter may have
				// been reinstalled after its old source path disappeared, so do not
				// suppress the canonical destination merely because the historical
				// path string is equal.
				sourcePath = ""
			}
			// A durable inbound shell whose current head belongs to a peer must
			// bypass the local source gate for every requested late target. The
			// peer's source harness may not exist or be enabled on this device.
			// Explicit origin attribution keeps routing rules honest when the
			// source adapter is not in the local adapter list.
			if includeSameSource && primary != nil && o.peerAuthoredInboundShell(art) {
				o.fanOutWithOptions(ctx, primary, []string{art.ArtifactID}, contextDir, sourcePath, true,
					fanOutOptions{targets: targets, mirrorsOnly: mirrorsOnly, originAgent: &origin})
				continue
			}

			remaining := targets
			if includeSameSource && primary != nil && origin == primary.Name() {
				if _, requested := targets[primary.Name()]; requested {
					// includePrimary is required to restore a same-adapter native
					// destination after reinstall, but it normally denotes inbound
					// materialization and therefore bypasses the source gate. Apply
					// that local gate explicitly before using it here.
					gateEnabled := true
					if gate := o.syncGate(); gate != nil {
						gateEnabled = gate.Enabled(origin)
					}
					_, blocked := o.adapterBlocked(origin)
					if gateEnabled && !blocked {
						self := map[string]struct{}{primary.Name(): {}}
						o.fanOutWithOptions(ctx, primary, []string{art.ArtifactID}, contextDir, sourcePath, true,
							fanOutOptions{targets: self, mirrorsOnly: mirrorsOnly, originAgent: &origin})
					}
					remaining = make(map[string]struct{}, len(targets)-1)
					for target := range targets {
						if target != primary.Name() {
							remaining[target] = struct{}{}
						}
					}
				}
			}
			if len(remaining) > 0 {
				o.fanOutWithOptions(ctx, primary, []string{art.ArtifactID}, contextDir, sourcePath, false,
					fanOutOptions{targets: remaining, mirrorsOnly: mirrorsOnly, originAgent: &origin})
			}
		}
	}
}

// peerAuthoredInboundShell identifies the minimal artifact record created by
// ensureInboundArtifactShell. The authenticated peer identity is retained on
// the shell so retention never forces this hot path to replay compacted event
// bodies. Legacy shells without that metadata fail closed until their next
// authenticated inbound event updates them. Native imports populate Name
// and/or SourcePath and never qualify.
func (o *Orchestrator) peerAuthoredInboundShell(art acf.Artifact) bool {
	if o.cfg.Store == nil || o.localDeviceID() == "" || art.Name != "" || art.SourcePath != "" {
		return false
	}
	return art.RemoteOriginDeviceID != "" && art.RemoteOriginDeviceID != o.localDeviceID()
}

// ScanRecentCodexSessions polls the date-partitioned Codex rollout directories
// directly. It is deliberately narrower than scanRootRecursive: live Codex
// conversations land under ~/.codex/sessions/YYYY/MM/DD, so this path can meet
// the near-real-time sync target without walking historical session trees.
func (o *Orchestrator) ScanRecentCodexSessions(ctx context.Context) int {
	if !o.beginBackground() {
		return 0 // Close in progress; the daemon's ticker is winding down
	}
	defer o.endBackground()

	var candidates []scanFileCandidate
	for _, root := range o.cfg.RecursiveRoots {
		if ctx.Err() != nil {
			return 0
		}
		if !isCodexSessionsRoot(root) {
			continue
		}
		abs, aerr := filepath.Abs(root)
		if aerr != nil {
			continue
		}
		candidates = append(candidates, o.recentCodexSessionCandidates(abs, time.Now())...)
	}
	newerScanFileFirst(candidates)
	fast, large := o.claimRecentCodexCandidates(candidates)
	dispatched := len(fast)
	if large != nil {
		if o.beginBackground() {
			dispatched++
			go func(candidate scanFileCandidate) {
				defer o.endBackground()
				o.processClaimedRecentCodexCandidate(ctx, candidate, true)
				_ = o.scanCache.flush()
			}(*large)
		} else {
			o.releaseRecentCodexCandidate(large.path, true)
		}
	}
	for _, candidate := range fast {
		o.processClaimedRecentCodexCandidate(ctx, candidate, false)
	}
	_ = o.scanCache.flush()
	return dispatched
}

// ScanRecentClaudeSessions polls recently modified Claude Code session JSONL
// files under ~/.claude/projects. macOS recursive FSEvents can miss fast
// appends to a freshly materialized remote conversation; this narrow scanner
// catches those hot files without walking every native root every tick.
func (o *Orchestrator) ScanRecentClaudeSessions(ctx context.Context) int {
	if o.cfg.RecentClaudeSessionWindow <= 0 {
		return 0
	}
	if !o.beginBackground() {
		return 0 // Close in progress; the daemon's ticker is winding down
	}
	defer o.endBackground()
	if !o.recentClaudeScanMu.TryLock() {
		if o.cfg.Logger != nil {
			o.cfg.Logger.Warn("claude recent session scan skipped; previous scan still running")
		}
		return 0
	}

	files := o.recentClaudeSessionCandidatesLocked(time.Now())
	o.recentClaudeScanMu.Unlock()

	for _, f := range files {
		if ctx.Err() != nil || o.closingNow() {
			return 0
		}
		if o.cfg.Logger != nil {
			o.cfg.Logger.Info("claude recent session scan candidate", "path", f.path)
		}
		if !o.handleScanEvent(f.path) && o.cfg.Logger != nil {
			o.cfg.Logger.Warn("claude recent session scan did not commit", "path", f.path)
		}
	}
	_ = o.scanCache.flush()
	return len(files)
}

func isCodexSessionsRoot(root string) bool {
	return filepath.Base(root) == "sessions" && filepath.Base(filepath.Dir(root)) == ".codex"
}

func isCodexSessionPath(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "/.codex/sessions/")
}

func isClaudeSessionPath(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "/.claude/projects/")
}

func recentCodexSessionDayDirs(root string, now time.Time) []string {
	seen := map[string]struct{}{}
	var dirs []string
	add := func(t time.Time) {
		dir := filepath.Join(root, t.Format("2006"), t.Format("01"), t.Format("02"))
		if _, ok := seen[dir]; ok {
			return
		}
		seen[dir] = struct{}{}
		dirs = append(dirs, dir)
	}
	for _, base := range []time.Time{now.Local(), now.UTC()} {
		for delta := -1; delta <= 1; delta++ {
			add(base.AddDate(0, 0, delta))
		}
	}
	return dirs
}

func (o *Orchestrator) recentCodexSessionCandidates(root string, now time.Time) []scanFileCandidate {
	const maxRecentCodexSessionCandidates = 16
	const maxRecentCodexReadinessBytes = maxFastLaneImportBytes

	var files []scanFileCandidate
	for _, dir := range recentCodexSessionDayDirs(root, now) {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if filepath.Ext(e.Name()) != ".jsonl" {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if ignoredNativePath(path) {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			// Codex rollouts use the dedicated session cap (512 MiB by default).
			// Do not impose a smaller polling cap: production makes this scanner
			// the sole catch-up owner for ~/.codex/sessions, and large active
			// rollouts must not become fsnotify-only. For small files we can cheaply
			// pre-parse readiness. Larger changed files go straight to the adapter,
			// whose incremental cache parses only the appended complete JSONL rows;
			// generated-session imports retain their own correctness gate.
			if limit := o.maxSessionFileBytes(); limit > 0 && info.Size() > limit {
				continue
			}
			if o.scanCache.unchanged(path) {
				continue
			}
			if info.Size() <= maxRecentCodexReadinessBytes {
				if _, ready := codexSessionReadyForLiveImport(path, maxRecentCodexReadinessBytes); !ready {
					continue
				}
			}
			files = append(files, scanFileCandidate{path: path, mod: info.ModTime()})
		}
	}
	newerScanFileFirst(files)
	if len(files) > maxRecentCodexSessionCandidates {
		files = files[:maxRecentCodexSessionCandidates]
	}
	return files
}

// claimRecentCodexCandidates atomically selects all bounded fast-lane work and
// at most one historical rollout. The claim is scheduling state, not an import
// lock: no filesystem parsing, admission wait, or materialization happens while
// recentCodexScanMu is held.
func (o *Orchestrator) claimRecentCodexCandidates(files []scanFileCandidate) ([]scanFileCandidate, *scanFileCandidate) {
	o.recentCodexScanMu.Lock()
	defer o.recentCodexScanMu.Unlock()
	if o.recentCodexInFlight == nil {
		o.recentCodexInFlight = make(map[string]struct{})
	}
	fast := make([]scanFileCandidate, 0, len(files))
	var large *scanFileCandidate
	for _, candidate := range files {
		if _, exists := o.recentCodexInFlight[candidate.path]; exists {
			continue
		}
		if fastLaneNativeImport(candidate.path) {
			o.recentCodexInFlight[candidate.path] = struct{}{}
			fast = append(fast, candidate)
			continue
		}
		if o.recentCodexLargeInFlight || large != nil {
			continue
		}
		claimed := candidate
		large = &claimed
		o.recentCodexInFlight[candidate.path] = struct{}{}
		o.recentCodexLargeInFlight = true
	}
	return fast, large
}

func (o *Orchestrator) releaseRecentCodexCandidate(path string, large bool) {
	o.recentCodexScanMu.Lock()
	delete(o.recentCodexInFlight, path)
	if large {
		o.recentCodexLargeInFlight = false
	}
	o.recentCodexScanMu.Unlock()
}

func (o *Orchestrator) processClaimedRecentCodexCandidate(ctx context.Context, candidate scanFileCandidate, large bool) {
	defer o.releaseRecentCodexCandidate(candidate.path, large)
	if ctx.Err() != nil || o.closingNow() {
		return
	}
	if o.cfg.Logger != nil {
		o.cfg.Logger.Info("codex recent session scan candidate", "path", candidate.path)
	}
	if !o.handleScanEvent(candidate.path) && o.cfg.Logger != nil {
		o.cfg.Logger.Warn("codex recent session scan did not commit", "path", candidate.path)
	}
}

func (o *Orchestrator) scanRecentCodexSessionDays(root string, now time.Time) int {
	files := o.recentCodexSessionCandidates(root, now)
	fast := make([]scanFileCandidate, 0, len(files))
	serialized := make([]scanFileCandidate, 0, len(files))
	for _, f := range files {
		if fastLaneNativeImport(f.path) {
			fast = append(fast, f)
		} else {
			serialized = append(serialized, f)
		}
	}
	var processed atomic.Int64
	process := func(batch []scanFileCandidate) {
		for _, f := range batch {
			if o.closingNow() {
				return
			}
			if o.cfg.Logger != nil {
				o.cfg.Logger.Info("codex recent session scan candidate", "path", f.path)
			}
			if !o.handleScanEvent(f.path) && o.cfg.Logger != nil {
				o.cfg.Logger.Warn("codex recent session scan did not commit", "path", f.path)
			}
			processed.Add(1)
		}
	}
	if len(fast) > 0 && len(serialized) > 0 {
		// Run exactly one serialized/background batch beside the fast batch.
		// The two-lane admission gate still caps total work at two and large work
		// at one; this merely lets the dedicated scanner make use of the reserved
		// latency lane instead of blocking its own later small candidate behind a
		// continuously growing history file.
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			process(serialized)
		}()
		process(fast)
		wg.Wait()
	} else {
		process(files)
	}
	return int(processed.Load())
}

func (o *Orchestrator) markClaudeHotSession(path string) {
	if o.cfg.RecentClaudeSessionWindow <= 0 || !isClaudeSessionPath(path) {
		return
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	o.recentClaudeScanMu.Lock()
	defer o.recentClaudeScanMu.Unlock()
	if o.recentClaudeHot == nil {
		o.recentClaudeHot = make(map[string]time.Time)
	}
	o.recentClaudeHot[path] = time.Now().Add(o.cfg.RecentClaudeSessionWindow)
}

func (o *Orchestrator) recentClaudeSessionCandidatesLocked(now time.Time) []scanFileCandidate {
	const maxRecentClaudeSessionCandidates = 32
	// 64MB: long agentic Claude sessions routinely exceed 8MB, and dropping
	// them from the hot set silently degraded live sync to the 5s scan. The
	// per-path incremental encode cache (v0.115) parses only appended bytes,
	// so a large-but-hot file costs a stat per tick, not a re-encode. The cap
	// still bounds pathological files (maxSessionFileBytes gates import
	// anyway, and transcripts past this hot bound stay on the watcher +
	// 5s native scan).
	const maxRecentClaudeSessionBytes = 64 * 1024 * 1024

	files := make([]scanFileCandidate, 0, maxRecentClaudeSessionCandidates)
	for path, expires := range o.recentClaudeHot {
		if now.After(expires) {
			delete(o.recentClaudeHot, path)
			continue
		}
		if ignoredNativePath(path) {
			delete(o.recentClaudeHot, path)
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.IsDir() {
			delete(o.recentClaudeHot, path)
			continue
		}
		if filepath.Ext(path) != ".jsonl" {
			delete(o.recentClaudeHot, path)
			continue
		}
		if now.Sub(info.ModTime()) < o.cfg.QuietPeriod {
			continue
		}
		if info.Size() > maxRecentClaudeSessionBytes {
			continue
		}
		// Hot-set membership is bounded by min(maxRecentClaudeSessionBytes,
		// session cap): the hot bound above keeps very large transcripts on
		// the watcher + 5s native scan (a 500ms stat loop over them buys
		// nothing), while the session cap check keeps the poll set from
		// respinning on a file the ingest gate is going to refuse anyway.
		if limit := o.maxSessionFileBytes(); limit > 0 && info.Size() > limit {
			continue
		}
		if o.scanCache.unchanged(path) {
			continue
		}
		files = append(files, scanFileCandidate{path: path, mod: info.ModTime()})
	}
	newerScanFileFirst(files)
	if len(files) > maxRecentClaudeSessionCandidates {
		files = files[:maxRecentClaudeSessionCandidates]
	}
	return files
}

func codexSessionReadyForLiveImport(path string, maxBytes int64) (scanFP, bool) {
	before, ok := fingerprintPath(path)
	if !ok {
		return scanFP{}, false
	}
	if maxBytes > 0 && before.Size > maxBytes {
		return scanFP{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return scanFP{}, false
	}
	defer f.Close()

	ready, readyErr := codexadapter.SessionReadyForImport(f)
	if readyErr != nil || !ready {
		return scanFP{}, false
	}
	after, ok := fingerprintPath(path)
	if !ok || after != before {
		return scanFP{}, false
	}
	return after, true
}

// WatchFolder begins watching a registered project folder at runtime: it adds a
// non-recursive watcher feeding the shared debouncer (so the folder's memory/
// skill/tool files flow through the same handleEvent pipeline as Dir and
// AdditionalRoots), then scans the folder once so files already present import
// immediately (backfill). ctx is the orchestrator's run context — cancelling it
// stops the added watcher. Safe to call after Run has started.
//
// The append to extraWatchers/extraClosers is guarded by o.mu, and BOTH
// readers (Run's startup launch loop and Close's teardown loop) snapshot the
// slices under the same mutex — WatchFolder legitimately races Run during the
// daemon's boot-window seed loop. A watcher appended here is never
// double-started (WatchFolder launches its own goroutine; Run only starts
// what its snapshot saw) and is torn down on shutdown either by Close's
// snapshot or by the run context it was registered with.
func (o *Orchestrator) WatchFolder(ctx context.Context, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("syncd: resolve folder: %w", err)
	}
	// Dedup guard: the same folder can be registered twice (the daemon's
	// boot-window seed loop racing an onRegister HTTP call, or re-approving an
	// already-registered project). Claim the path under o.mu, then create the
	// watcher without holding the orchestration mutex; fsnotify setup can block
	// on unhealthy filesystems or exhausted descriptors, and live imports must
	// keep moving while that happens.
	o.mu.Lock()
	if o.watchedFolders[abs] != nil {
		o.mu.Unlock()
		return nil // already watching this folder
	}
	if _, inflight := o.watchingFolders[abs]; inflight {
		o.mu.Unlock()
		return nil
	}
	if o.watchingFolders == nil {
		o.watchingFolders = map[string]struct{}{}
	}
	o.watchingFolders[abs] = struct{}{}
	o.mu.Unlock()

	w, werr := watcher.NewWatcher(abs, o.debouncer)
	if werr != nil {
		o.mu.Lock()
		delete(o.watchingFolders, abs)
		o.mu.Unlock()
		return fmt.Errorf("syncd: watch folder %s: %w", abs, werr)
	}
	w.OnError = o.onWatcherError
	o.mu.Lock()
	delete(o.watchingFolders, abs)
	if o.watchedFolders[abs] != nil {
		o.mu.Unlock()
		_ = w.Close()
		return nil
	}
	o.watchedFolders[abs] = w
	o.extraWatchers = append(o.extraWatchers, w)
	o.extraClosers = append(o.extraClosers, w.Close)
	o.mu.Unlock()
	if o.beginBackground() {
		go func() {
			defer o.endBackground()
			w.Run(ctx)
		}()
	} else {
		// Close began while this watcher was being set up; it missed Close's
		// extraClosers snapshot, so close it here rather than leaking a source.
		_ = w.Close()
		return nil
	}
	return o.scanRoot(abs) // backfill files already on disk
}

// UnwatchFolder stops watching a folder previously added via WatchFolder and
// forgets it, so the next discovery pass re-surfaces it as a pending project.
// Closing the watcher ends its Run goroutine (the source's Events channel
// closes). The closed watcher stays in extraClosers, but watcher.Close is
// idempotent so the shutdown loop closing it again is a harmless no-op.
// Unknown / already-unwatched paths are a no-op.
func (o *Orchestrator) UnwatchFolder(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("syncd: resolve folder: %w", err)
	}
	o.mu.Lock()
	w := o.watchedFolders[abs]
	delete(o.watchedFolders, abs)
	o.mu.Unlock()
	if w == nil {
		return nil
	}
	return w.Close()
}

// Debouncer returns the orchestrator's per-path quiet-period debouncer.
// Callers use this to invoke live setters such as SetQuietPeriod from the
// daemon's SIGHUP config-reload handler (v0.27.1). Returns nil if the
// orchestrator was somehow constructed without one — defensive, the
// public NewOrchestrator path always populates this field.
func (o *Orchestrator) Debouncer() *watcher.Debouncer { return o.debouncer }

// Guard returns the orchestrator's recursion guard for live SetWindow
// reconfiguration from the SIGHUP handler (v0.27.1).
func (o *Orchestrator) Guard() *RecursionGuard { return o.guard }

// beginBackground registers a background goroutine or scan entry point with
// the orchestrator's Close-join lifecycle. Returns false when Close has
// already begun — the caller must return immediately without doing any work.
// Every true return MUST be paired with endBackground (usually deferred).
// The bgMu-guarded flag makes the register-vs-Close decision atomic, so a
// goroutine either registers before Close begins (and Close waits for it) or
// never starts.
func (o *Orchestrator) beginBackground() bool {
	o.bgMu.Lock()
	defer o.bgMu.Unlock()
	if o.bgClosing {
		return false
	}
	o.bgWG.Add(1)
	return true
}

// endBackground releases a beginBackground registration.
func (o *Orchestrator) endBackground() { o.bgWG.Done() }

// closingNow reports whether Close has begun. The per-file scan loops check
// it between imports so Close's join is bounded by ONE in-flight file, not a
// full native-tree walk. Safe on a zero-value Orchestrator (nil bgDone: a
// receive from a nil channel never fires, so the default arm reports false).
func (o *Orchestrator) closingNow() bool {
	select {
	case <-o.bgDone:
		return true
	default:
		return false
	}
}

// Close stops the watcher and debouncer, then JOINS every registered
// background goroutine (scan loops, watcher runners, detached conversation
// fan-outs, large-materialize flush timers) before returning — after Close no
// orchestrator goroutine can still be reading or writing watched roots. Safe
// to call multiple times, and does not require the caller's Run/scan context
// to be cancelled first: bgDone stops the loops on its own.
func (o *Orchestrator) Close() error {
	// Refuse new background registrations, then wake every registered loop.
	o.bgMu.Lock()
	o.bgClosing = true
	o.bgMu.Unlock()
	if o.adapterBlockerUnsubscribe != nil {
		o.adapterBlockerUnsubscribe()
		o.adapterBlockerUnsubscribe = nil
	}
	if o.bgDone != nil {
		o.bgOnce.Do(func() { close(o.bgDone) })
	}
	if o.debouncer != nil {
		o.debouncer.Stop()
	}
	// debouncer.Stop drained any in-flight import, so the fingerprint set is
	// final — persist it for the next start. Best-effort: a flush error only
	// costs a re-scan next time, never correctness.
	_ = o.scanCache.flush()
	// Disarm any pending large-conversation materialization flushes so no
	// timer fires a write mid-teardown. Dropping the rewrite is safe: the
	// canonical store already holds the head, and the next daemon start's
	// scan/re-fanout re-materializes it.
	o.largeMaterializeMu.Lock()
	for _, pend := range o.largeMaterializePending {
		if pend.timer != nil {
			pend.timer.Stop()
		}
	}
	o.largeMaterializePending = nil
	o.largeMaterializeMu.Unlock()
	// Close the extra (AdditionalRoots) watcher sources first; we own them
	// (NewWatcher / NewRecursiveSource). Errors are collected but the
	// primary source's close result is the one returned for compatibility.
	// Snapshot under o.mu — WatchFolder may append concurrently (same hazard
	// as Run's startup read); a watcher appended after this snapshot is torn
	// down by its own ctx cancellation rather than this loop.
	o.mu.Lock()
	closers := append([]func() error(nil), o.extraClosers...)
	o.mu.Unlock()
	for _, c := range closers {
		if c != nil {
			_ = c()
		}
	}
	var err error
	if o.srcCloser != nil {
		err = o.srcCloser()
	}
	// JOIN: wait for every registered background goroutine to finish. Done
	// after the source closers so blocked watcher Run loops (whose ctx may
	// still be live) observe their closed Events channels and exit. An
	// in-flight import/fan-out completes its current file first — dropping
	// it mid-write is what this wait exists to prevent — and the closingNow
	// checks in the scan loops keep that residue to one file, not a walk.
	o.bgWG.Wait()
	return err
}

// onSettled is the callback the debouncer invokes when a file path has
// settled. Drops events suppressed by the recursion guard; otherwise
// imports + fans out.
//
// The guard alone can't tell an ECHO of the orchestrator's own write from
// a REAL agent edit that landed just after it (both arrive inside the
// window). destHashes can: if the file's content still matches what the
// orchestrator last wrote there, the event is an echo and is dropped; if
// it differs, an agent changed the file and the event must import — or a
// near-simultaneous divergent edit is silently destroyed (E2E F6).
func (o *Orchestrator) onSettled(path string) bool {
	if o.guard.Suppressed(path) && !o.destChangedUnderUs(path) {
		// Deliberate echo-suppression of our own write is a handled terminal
		// state — report committed so the debouncer records the dedup hash and
		// does not reprocess the echo.
		return true
	}
	return o.handleEvent(path)
}

// ignoredNativePath reports paths the watcher/scan must never route through
// the generic import pipeline. SQLite databases and their WAL/SHM/journal
// sidecars are rewritten continuously (e.g. ~/.codex/logs_2.sqlite-wal is
// multi-megabyte and flushed constantly). DB-backed adapters need dedicated
// incremental importers (Hermes has hermeswatch); letting startup/live scans
// route raw *.db files through primaryImport can re-read huge conversation
// histories and starve ordinary file events. The textual memory Codex exposes
// lives in memories/*.md — NOT matched here — so this does not affect the
// memories-sync path.
func ignoredNativePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".db", ".sqlite", ".sqlite-wal", ".sqlite-shm", ".sqlite-journal",
		".db-wal", ".db-shm", ".db-journal":
		return true
	}
	return false
}

// handleEvent runs the full primary-import + fan-out pipeline for a single
// settled path. It returns whether the path reached a committed/handled
// terminal state: false ONLY when a transient failure (no adapter could commit
// the file) prevented an expected import, so the watcher debouncer retries on
// the path's next event instead of permanently dedup-suppressing it. Deliberate
// skips (ignored sidecar, byte-unchanged since last import, oversize-refused)
// return true.
func (o *Orchestrator) handleEvent(path string) bool {
	handled, _ := o.handleEventWithDisposition(path, eventHandlingOptions{})
	return handled
}

// eventHandlingOptions narrows what one pass through the import pipeline is
// allowed to do. The zero value is the ordinary pass, so every existing caller
// is byte-for-byte unchanged.
type eventHandlingOptions struct {
	// importOnly stops the pass after the import has been committed and the
	// deferral queue told about it, and BEFORE fanOut.
	//
	// This flag is load-bearing, not a convenience — see the repair-pass budget
	// on nativeReimportNudgesPerWindow. The quarantine breaker is fed from
	// exactly one call site, the Export loop in fanOut, so a pass that returns
	// before fanOut can charge it nothing however it fails. A refactor that
	// "simplified" this into a full pass would reproduce the whole-store-repair
	// outage class in miniature.
	importOnly bool
}

// fastLaneNativeImport authenticates the narrowly-scoped second admission
// lane: only a regular, bounded Codex session file qualifies.
func fastLaneNativeImport(path string) bool {
	fp, ok := fingerprintPath(path)
	return ok && isCodexSessionPath(path) && fp.Size <= maxFastLaneImportBytes
}

// acquireImportSlot keeps native imports globally bounded without letting one
// continuously-growing conversation monopolize every interactive update. A
// large caller takes the single large-work token BEFORE a total-work token, so
// another queued large file cannot occupy the capacity reserved for a small
// transcript. The returned release function is non-nil only after admission.
func (o *Orchestrator) acquireImportSlot(path string) (func(), bool) {
	if o.importSlots == nil {
		return func() {}, true
	}

	large := !fastLaneNativeImport(path)
	largeHeld := false
	if large && o.largeImportSlots != nil {
		select {
		case o.largeImportSlots <- struct{}{}:
			largeHeld = true
		case <-o.bgDone:
			return nil, false
		}
	}

	select {
	case o.importSlots <- struct{}{}:
		return func() {
			<-o.importSlots
			if largeHeld {
				<-o.largeImportSlots
			}
		}, true
	case <-o.bgDone:
		if largeHeld {
			<-o.largeImportSlots
		}
		return nil, false
	}
}

// handleEventWithDisposition also reports whether an owning adapter was
// skipped for a reversible availability/safety reason during the actual import
// attempt. Startup/live scans use that disposition to keep unchanged native
// files retryable when their CLI/Desktop runtime appears or becomes unblocked.
func (o *Orchestrator) handleEventWithDisposition(
	path string, opts eventHandlingOptions,
) (handled, ownerDeferred bool) {
	if ignoredNativePath(path) {
		return true, false
	}

	// Bound the heavyweight parse/materialize/fan-out section. Admission comes
	// before nativeRestoreGate so a restore writer is never starved by a backlog
	// of settled watcher callbacks already holding read locks. Close closes
	// bgDone, waking queued imports without processing stale work.
	releaseImportSlot, admitted := o.acquireImportSlot(path)
	if !admitted {
		return true, false
	}
	defer releaseImportSlot()
	o.nativeRestoreGate.RLock()
	defer o.nativeRestoreGate.RUnlock()

	// Serialize the whole pipeline per path: the debouncer settle, the 5s
	// native live scan, and the recent-session hot scanners can all deliver
	// the same file concurrently. The losers block here, then hit the
	// scanCache fast-path below (the winner records (size, mtime) on commit)
	// or compute an empty delta against the winner's committed head — either
	// way a no-op instead of a duplicated artifact/update event.
	unlock := o.sourcePathLocks.lock(path)
	defer unlock()

	// Restart fast-path: if this file is byte-stable (same size + mtime) since
	// we last imported it, there is nothing new to import, fan out, or forward
	// — skip the whole pipeline, and most importantly the expensive parse +
	// canonical-encode inside the adapter's Import. This is what turns the
	// startup InitialScan over a large unchanged history from a multi-minute
	// re-encode into a stat-only pass. A genuine edit moves size/mtime and
	// falls through; a brand-new file has no cache entry and falls through.
	if o.scanCache.unchanged(path) {
		return true, false
	}

	// Max-artifact-size gate (BRD-03 §4.3). Refuse a file whose on-disk size
	// exceeds the configured cap BEFORE primaryImport reads and canonical-
	// encodes it: a hostile or runaway multi-GB blob dropped into a watched
	// root (the "sign of something unusual" the BRD calls out) must not be
	// pulled into memory and grow the store unbounded. Agent session
	// transcripts get the separate, larger session cap (see ingestSizeLimit);
	// everything else the generic artifact cap. The refusal is surfaced
	// to the user via the status channel (FR-03.5) and the live event stream;
	// it is NOT cached, so shrinking the file below the cap lets a later edit
	// import it normally. A non-positive resolved cap disables the gate.
	if limit := o.ingestSizeLimit(path); limit > 0 {
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
			if fi.Size() > limit {
				// Deliberate refusal: not cached (shrinking below the cap
				// re-imports), but a terminal decision for this file's current
				// bytes — suppress retry so the debouncer does not respin on
				// every settle. The bus event is throttled per path so the 5s
				// scan's repeat refusals don't flood the event stream.
				if o.shouldReportOversize(path) {
					o.recordOversizeArtifact(path, fi.Size(), limit)
				}
				return true, false
			}
			o.clearOversizeReported(path)
		}
	}

	// Snapshot the source fingerprint BEFORE handing the path to an adapter.
	// Session logs are append-only and an assistant answer can arrive while a
	// prompt-only import is still running. Caching a post-import stat would then
	// claim that the appended answer had already been consumed even though the
	// adapter only committed the earlier snapshot. Recording this attempt
	// fingerprint below makes such an append remain visibly changed and eligible
	// for the next hot-scan/watcher pass.
	importFP, importFPOK := fingerprintPath(path)
	importDestFP, importDestFPOK := fingerprintDest(path)

	ctx := context.Background()

	// NFR-10 §5.2 sync_latency_seconds: start the clock at the import->fan-out
	// materialization boundary. importStart marks the moment real work begins on
	// this changed file (after the cheap stat/size/scan-cache gates above bailed
	// on no-op paths, so the histogram measures genuine sync work, not skipped
	// probes). It is observed once per committed artifact after fanOut returns.
	importStart := time.Now()
	priorHeads := o.sourcePathHeadHashes(path)

	primary, ids, ok, ownerDeferred := o.primaryImportWithDisposition(ctx, path)
	if !ok {
		// No adapter committed this file (an adapter claimed it but Import
		// failed transiently, or every owner was unavailable/safety-blocked). Report
		// not-committed so the debouncer retries on the path's next event
		// rather than stranding a freshly-written file out of the store.
		return false, ownerDeferred
	}
	// Some importers synthesize a canonical artifact from a sibling/managed
	// path: e.g. ~/.codex/memories/*.md updates the AGENTS.md-keyed artifact,
	// and Claude Code auto-memory topics update the CLAUDE.md-keyed artifact.
	// Snapshot those artifact source-path heads before refreshing the index so
	// an unchanged managed-file scan is not mistaken for a fresh commit.
	priorHeads = o.expandPriorHeadsForImportedSources(path, ids, priorHeads)
	o.refreshSourcePathHeads(ids)

	// Record what this file looked like when this import attempt began so a
	// later fan-out can tell "unchanged since the imported snapshot" from
	// "edited under us" (see destHashes). Never fingerprint again here: an
	// agent may have appended bytes while Import was parsing the earlier
	// snapshot, and treating those bytes as imported could let fan-out clobber
	// them before the next scan commits them.
	if importDestFPOK {
		o.recordDestFingerprint(path, importDestFP)
	}
	o.markClaudeHotSession(path)
	// This file has now been imported, which is precisely the state transition a
	// queued write to it may have been declined on: an adapter refuses to append
	// a canonical suffix to a native session holding an unimported continuation.
	// The drain's unchanged-inputs short circuit cannot observe that — it
	// watches the canonical head and the destination bytes, and an import moves
	// neither — so tell it explicitly instead of letting it skip the one pass
	// that would now succeed.
	o.reopenDeferredMaterializationForDest(path)

	// Record the fingerprint captured before Import, never a fresh fingerprint
	// of bytes that may have been appended while the adapter was reading. A file
	// that changed during Import therefore remains eligible for the next pass.
	// Recorded only on the import-success path — a file no adapter claimed
	// (ok == false above) stays uncached and is cheaply re-probed next start.
	if importFPOK {
		o.scanCache.recordFingerprint(path, importFP)
	}

	// Post-import origin-session check: if the file this import just consumed
	// is a conversation's OWN source and can no longer present the post-import
	// canonical head on its agent's resume walk (the still-open-TUI fork),
	// queue the one write fan-out's same-source exclusion would otherwise
	// never attempt. Runs on the raw ids deliberately — the fork's later
	// imports are disposition no-ops (freshlyCommittedIDs drops them below)
	// while the file stays broken — and never on the scanCache-unchanged
	// short circuit above. See queueOriginSessionRepairs.
	o.queueOriginSessionRepairs(primary, path, ids)

	if opts.importOnly {
		// The diverged-import nudge stops HERE. Everything above is the import
		// half — the file has been read, whatever canonical could absorb from it
		// is committed, and reopenDeferredMaterializationForDest has retracted
		// the short circuit that would otherwise skip the queued write's next
		// pass. Everything below is fan-out, which is the half that can charge
		// the quarantine breaker.
		//
		// Stopping is not the same as forgetting, though. The absorbed turns are
		// a genuine canonical commit and every OTHER agent is now behind on
		// them, and fanOut is the only thing that derives that — so the targets
		// are handed to the deferral queue instead, which performs the same
		// fan-out under the drain's backoff and quarantine awareness. Without
		// this the absorbed turns reached canonical and no other local agent,
		// which is the silent partial convergence this whole change exists to
		// end: the scan cache has just recorded this file's fingerprint, so no
		// ordinary pass revisits it either.
		o.deferAbsorbedConversationFanOut(o.freshlyCommittedIDs(ids, priorHeads))
		return true, false
	}

	// Some adapter Import paths intentionally return an existing artifact ID
	// when the native file names an already-current canonical artifact. That is
	// useful identity resolution, but it is not a new commit and must not fan out
	// or publish the old head to cloud again (startup scans can otherwise
	// resurrect stale events indefinitely).
	ids = o.freshlyCommittedIDs(ids, priorHeads)
	if len(ids) == 0 {
		if primary != nil {
			// A no-op native import can be the exact state transition a
			// previously safety-deferred projection was waiting for. Wake that
			// target before returning even though canonical truth did not change.
			o.scheduleDeferredMaterializationDrain(primary.Name())
		}
		return true, false
	}

	contextDir := filepath.Dir(path)

	// Conflict detection (BRD-03 §4.6 / §10 OQ-03.2 / ADR-0038): if enabled,
	// look at the artifact's prior head event; if a DIFFERENT adapter wrote it
	// within ConflictWindow with a DIFFERENT payload, record the divergent heads
	// to the conflicts store so a human can pick a winner via
	// `aplexica resolve <artifactId>`.
	//
	// The freshly-imported event STAYS committed to the immutable local event
	// log (it is NOT rolled back — the local edit is preserved as a branch
	// head). What changes per the spec is PROPAGATION: a detected divergence
	// blocks the artifact from fanning out to other agents — and from leaving
	// this device via the remote transport — until the user resolves it.
	// inUnresolvedConflict re-checks the store too, so a conflict recorded on a
	// prior run still blocks propagation after a daemon restart.
	blocked := map[string]bool{}
	if o.cfg.ConflictStore != nil {
		for _, id := range ids {
			if o.maybeRecordConflict(primary, id) || o.inUnresolvedConflict(id) {
				blocked[id] = true
			}
		}
	}

	// Gate LOCAL propagation: fan out only the ids that are NOT in an
	// unresolved conflict. If every id is blocked, skip fanOut entirely.
	propagatable := ids
	if len(blocked) > 0 {
		propagatable = propagatable[:0:0]
		for _, id := range ids {
			if !blocked[id] {
				propagatable = append(propagatable, id)
			}
		}
	}
	if len(propagatable) > 0 {
		// Conversations may trigger native-session materializers during fanOut
		// (Codex rollout files, Kilo import, etc.). Those are local targets and
		// must not gate cloud propagation: the canonical event is already
		// committed, and unresolved conflicts are still blocked above. Publish
		// conversation events before local materialization so a slow/hung target
		// agent cannot strand cross-device sync.
		conversationOnly := true
		if o.remoteEventPublisher() != nil {
			for _, id := range propagatable {
				if art, found := o.findArtifact(id); found && art.Kind == acf.KindConversation {
					o.forwardCommitted(id)
				} else {
					conversationOnly = false
				}
			}
		} else {
			for _, id := range propagatable {
				if art, found := o.findArtifact(id); !found || art.Kind != acf.KindConversation {
					conversationOnly = false
					break
				}
			}
		}
		if conversationOnly {
			// Detached — but registered with the Close-join lifecycle: this
			// goroutine materializes native session files, and Close returning
			// while it still writes is exactly the teardown race the join
			// exists for. When Close already began, skip the local
			// materialization entirely: the canonical event is committed, and
			// the next start's scan/re-fanout re-materializes it (same
			// rationale as dropping largeMaterializePending in Close).
			if o.beginBackground() {
				idsForFanOut := append([]string(nil), propagatable...)
				go func() {
					defer o.endBackground()
					o.fanOut(context.Background(), primary, idsForFanOut, contextDir, path, false, nil)
				}()
			}
		} else {
			o.fanOut(ctx, primary, propagatable, contextDir, path, false, nil)
		}
	}

	// NFR-10 §5.2: record the import->fan-out materialization latency, once per
	// committed artifact so the histogram's _count tracks artifacts synced (the
	// same per-id granularity as the artifact.synced live event below). Measured
	// for every committed id, including conflict-blocked ones whose propagation
	// was withheld — the artifact still landed in the canonical store, which is
	// the work the latency SLO (§3) covers. No-op when no observer is wired.
	if obs := o.syncLatencyObserver(); obs != nil {
		elapsed := time.Since(importStart).Seconds()
		for range ids {
			obs.ObserveSyncLatency(elapsed)
		}
	}

	// Publish a meaningful "artifact.synced" live event per committed artifact
	// so the web UI's Event stream reads "<agent> synced <name>" in real time,
	// matching the persisted backfill — instead of the contentless
	// "agent.activity" ping that used to be the only live signal. SSE-only
	// (never persisted); the bus drops on a slow subscriber.
	for _, id := range ids {
		o.publishArtifactSyncedEvent(primary.Name(), id, path)
	}

	// Forward each freshly committed event to the remote
	// transport plugin. Runs after fanOut so the outbound event reflects the
	// fully-committed state. No-op when no RemoteEventPublisher is wired
	// (OSS-only daemon). LOOP PREVENTION: forwardCommitted skips any event
	// whose provenance device id is a known remote origin — i.e. an event we
	// imported FROM the relay (and possibly re-materialized to a native file
	// and re-imported here) is never bounced back out. See forwardCommitted.
	// An unresolved divergent head must NOT leave this device either, so skip
	// forwardCommitted for blocked ids (BRD-03 §4.6 / §10 OQ-03.2). Resolution
	// re-propagates the winning head.
	if o.remoteEventPublisher() != nil {
		for _, id := range ids {
			if blocked[id] {
				continue
			}
			if art, found := o.findArtifact(id); found && art.Kind == acf.KindConversation {
				continue
			}
			o.forwardCommitted(id)
		}
	}

	// v0.39.0: stamp the activity timestamp now that fan-out has
	// returned. Done before the auto-snapshot block so the timestamp
	// reflects user-visible work landing, not the bookkeeping that
	// follows. The tray indicator's deriveState reads this value via
	// daemon.StatusInfo.LastActivity to drive its Active/Paused decay.
	o.setLastActivity(time.Now())

	// Auto-snapshot trigger (BRD-03 §4.8.1 primitive). When the per-artifact
	// event count crosses the per-kind threshold in SnapshotCadence, append
	// a snapshot event via retention.CreateSnapshot. Runs after fan-out so
	// the snapshot reflects the just-imported event. Best-effort: errors
	// are swallowed because a failed snapshot does NOT invalidate the
	// just-imported user event — the next call attempts again at the
	// next threshold crossing. The snapshot itself is a real Event in
	// the log, so it bumps the count by one; the next trigger fires
	// after threshold-1 more user events.
	//
	// Reads SnapshotCadence under o.mu to play nicely with
	// SetSnapshotCadence callers (SIGHUP handler). Skips an artifact
	// entirely when its kind has no threshold or a non-positive
	// threshold (disabled for that kind). v0.29.2 replaced v0.29.1's
	// single threshold with this per-kind map per BRD-03 §4.8.1.
	cadence := o.snapshotCadence()
	if len(cadence) > 0 {
		for _, id := range ids {
			art, found := o.findArtifact(id)
			if !found {
				continue
			}
			threshold, ok := cadence[art.Kind]
			if !ok || threshold <= 0 {
				continue
			}
			// AppendEvent maintains this counter with the artifact head metadata.
			// Reading it here is O(1); even a raw newline scan of a multi-gigabyte
			// conversation log on every new turn monopolizes a core.
			if art.EventCount == 0 {
				continue
			}
			if art.EventCount%uint64(threshold) != 0 {
				continue
			}
			if !retention.AutomaticSnapshotAllowed(o.cfg.Store, art) {
				continue
			}
			_, _ = retention.CreateSnapshot(ctx, o.cfg.Store, art.Kind, id)
		}
	}
	return true, false
}

// LastActivity returns the wall-clock time of the orchestrator's last
// successful primary-import + fan-out cycle. Returns the zero time
// when no event has been processed yet (fresh start). Satisfies the
// daemon.Activity interface so the daemon's control server can overlay
// this field onto StatusInfo without an explicit dependency on this
// package (v0.39.0).
func (o *Orchestrator) LastActivity() time.Time {
	if !o.mu.TryLock() {
		return time.Time{}
	}
	defer o.mu.Unlock()
	return o.lastActivity
}

// PendingImports returns the number of paths the debouncer is currently
// holding for their quiet-period to elapse — i.e., the per-path import
// queue depth. Surfaced to the daemon control server so the tray
// indicator can render "active (N pending)" in its status header
// (v0.44.0; ADR-0159 Candidate A). Zero is the steady-state value.
func (o *Orchestrator) PendingImports() int {
	if o.debouncer == nil {
		return 0
	}
	return o.debouncer.Pending()
}

// adapterActiveWindow is the recency threshold used by AdapterStates to
// bucket a per-adapter touched-at timestamp as "active" vs "idle".
// Chosen to match the tray's default --active-window (30s) so the per-
// adapter dots visually correlate with the icon's global state.
//
// Not currently a configurable knob — adjustment would need a
// downstream user-facing config field; the 30s window is a sensible
// default for v0.51.0.
const adapterActiveWindow = 30 * time.Second

// AdapterStates returns a snapshot of the orchestrator's per-adapter
// state bucket (ADR-0159 Candidate B). Each entry's value is one of:
//   - "active"   : adapter touched within adapterActiveWindow
//   - "idle"     : adapter touched longer ago (or never)
//
// Adapters not yet observed appear as "idle" (the orchestrator
// pre-populates the map at startup with one entry per configured
// adapter so consumers can always enumerate the full set).
//
// Returns a copy — callers may mutate freely. v0.51.0.
func (o *Orchestrator) AdapterStates() map[string]string {
	if !o.mu.TryLock() {
		return nil
	}
	defer o.mu.Unlock()
	now := time.Now()
	out := make(map[string]string, len(o.adapterTouched))
	for name, t := range o.adapterTouched {
		if _, blocked := o.adapterBlocked(name); blocked {
			out[name] = "blocked"
		} else if t.IsZero() || now.Sub(t) > adapterActiveWindow {
			out[name] = "idle"
		} else {
			out[name] = "active"
		}
	}
	return out
}

func (o *Orchestrator) adapterBlocked(name string) (string, bool) {
	if o == nil || o.cfg.AdapterBlocker == nil {
		return "", false
	}
	return o.cfg.AdapterBlocker.Blocked(name)
}

// PendingProjects (BRD-02 §4.13; v0.58.0) returns the canonical
// pending-project list the daemon should surface to its tray /
// status consumers. Computed via internal/pending.List against
// the orchestrator's store + registry.
//
// Returns nil (not an empty slice) when no pending projects exist,
// matching the omitempty wire-shape rule. Returns nil on
// enumeration error (best-effort surface; the daemon's logger
// records the underlying error elsewhere).
//
// The map[string]any return type matches daemon.StatusInfo's wire
// shape (avoids an import cycle daemon → pending → … → daemon).
// Each entry has keys: "id", "artifactCount", "samplePath".
func (o *Orchestrator) PendingProjects() []map[string]any {
	if o.cfg.Store == nil || o.cfg.ProjectRegistry == nil {
		return nil
	}
	o.pendingProjectsCacheMu.Lock()
	defer o.pendingProjectsCacheMu.Unlock()
	if !o.pendingProjectsCacheAt.IsZero() && time.Since(o.pendingProjectsCacheAt) < pendingProjectsCacheTTL {
		return clonePendingProjectMaps(o.pendingProjectsCache)
	}
	list, err := pending.List(o.cfg.Store, o.cfg.ProjectRegistry)
	if err != nil || len(list) == 0 {
		o.pendingProjectsCacheAt = time.Now()
		o.pendingProjectsCache = nil
		return nil
	}
	out := make([]map[string]any, len(list))
	for i, p := range list {
		out[i] = map[string]any{
			"id":            p.ID,
			"artifactCount": p.ArtifactCount,
			"samplePath":    p.SamplePath,
		}
	}
	o.pendingProjectsCacheAt = time.Now()
	o.pendingProjectsCache = out
	return clonePendingProjectMaps(out)
}

func clonePendingProjectMaps(in []map[string]any) []map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make([]map[string]any, len(in))
	for i, project := range in {
		copyProject := make(map[string]any, len(project))
		for key, value := range project {
			copyProject[key] = value
		}
		out[i] = copyProject
	}
	return out
}

func (o *Orchestrator) invalidatePendingProjectsCache() {
	o.pendingProjectsCacheMu.Lock()
	o.pendingProjectsCacheAt = time.Time{}
	o.pendingProjectsCache = nil
	o.pendingProjectsCacheMu.Unlock()
}

// RefanOutByProject (BRD-02 §4.13; v0.58.0) re-runs fanOut for every
// artifact whose Project.ID matches projectID. Called via the
// control-socket "refanout" command when `aplexica project link`
// persists a new registry entry — the newly-linked project's
// previously-pending artifacts materialize to agent native paths
// without waiting for a fresh edit.
//
// Returns the number of artifacts re-fanouted. Errors are logged
// per-artifact but don't short-circuit the overall pass; a partial
// failure still counts whatever succeeded.
func (o *Orchestrator) RefanOutByProject(projectID string) (int, error) {
	o.invalidatePendingProjectsCache()
	o.nativeRestoreGate.RLock()
	defer o.nativeRestoreGate.RUnlock()
	if projectID == "" {
		return 0, fmt.Errorf("refanout: empty projectID")
	}
	if o.cfg.Store == nil {
		return 0, fmt.Errorf("refanout: store is nil")
	}
	ctx := context.Background()
	count := 0
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		artifacts, err := o.cfg.Store.ListArtifacts(kind)
		if err != nil {
			continue
		}
		for _, art := range artifacts {
			if art.Scope != acf.ScopeProject {
				continue
			}
			if art.Project == nil || art.Project.ID != projectID {
				continue
			}
			// Pick the source adapter for this artifact by looking
			// up the first adapter whose Name matches the artifact's
			// most recent event's SourceAgent. fanOut wants the
			// "primary" adapter so it can skip it when iterating
			// destinations. Using the SourceAgent is the closest
			// approximation available from the store.
			var primary adapter.Adapter
			events, _ := o.cfg.Store.ReadEvents(kind, art.ArtifactID)
			if len(events) > 0 {
				srcName := events[len(events)-1].Provenance.SourceAgent
				for _, ad := range o.cfg.Adapters {
					if ad.Name() == srcName {
						primary = ad
						break
					}
				}
			}
			if primary == nil && len(o.cfg.Adapters) > 0 {
				// Fallback: use the first adapter alphabetically.
				// Worst case fanOut also exports to the source's
				// own native path, which is idempotent if the file
				// already exists with the same content.
				primary = o.cfg.Adapters[0]
			}
			// Use the project's Path as the contextDir so adapters
			// resolve NativePath relative to the linked location.
			contextDir := art.Project.Path
			o.fanOut(ctx, primary, []string{art.ArtifactID}, contextDir, art.SourcePath, false, nil)
			count++
		}
	}
	return count, nil
}

// MaterializeConversationBranch writes one conversation branch into one local
// agent immediately. It is the daemon control-socket path used after
// `aplexica fork` / `aplexica checkout`, where waiting for the next incidental
// fan-out would make the selected branch feel like it did not open.
func (o *Orchestrator) MaterializeConversationBranch(artifactID, agentName, branch string) (string, bool, error) {
	o.nativeRestoreGate.RLock()
	defer o.nativeRestoreGate.RUnlock()
	artifactID = strings.TrimSpace(artifactID)
	agentName = strings.TrimSpace(agentName)
	if artifactID == "" {
		return "", false, fmt.Errorf("materialize: empty artifactID")
	}
	if agentName == "" {
		return "", false, fmt.Errorf("materialize: empty agent")
	}
	if o.cfg.Store == nil {
		return "", false, fmt.Errorf("materialize: store is nil")
	}
	if branch == "" {
		branch = acf.MainBranch
	}
	branch, err := acf.NormalizeBranchName(branch)
	if err != nil {
		return "", false, fmt.Errorf("materialize: branch: %w", err)
	}
	art, err := o.cfg.Store.ReadArtifact(acf.KindConversation, artifactID)
	if err != nil {
		return "", false, fmt.Errorf("materialize: read conversation %s: %w", artifactID, err)
	}
	bi, err := o.cfg.Store.RefreshBranchIndex(acf.KindConversation, artifactID)
	if err != nil {
		return "", false, fmt.Errorf("materialize: refresh branch index: %w", err)
	}
	info, ok := bi.Branches[branch]
	if !ok {
		return "", false, fmt.Errorf("materialize: branch %q does not exist on conversation %s", branch, artifactID)
	}
	if info.Archived {
		return "", false, fmt.Errorf("materialize: branch %q is archived", branch)
	}

	var target adapter.Adapter
	for _, ad := range o.cfg.Adapters {
		if ad.Name() == agentName {
			target = ad
			break
		}
	}
	if target == nil {
		return "", false, fmt.Errorf("materialize: agent %q is not configured", agentName)
	}
	if reason, blocked := o.adapterBlocked(agentName); blocked {
		if reason == "" {
			reason = "blocked"
		}
		return "", false, fmt.Errorf("materialize: agent %q is %s", agentName, reason)
	}

	unlock := o.lockConversationMaterialize(artifactID)
	defer unlock()
	head, hasHead, err := conversationHeadForBranch(o.cfg.Store, artifactID, branch)
	if err != nil {
		return "", false, fmt.Errorf("materialize: conversation head: %w", err)
	}
	if !hasHead {
		return "", false, fmt.Errorf("materialize: no materializable conversation payload for branch %q", branch)
	}
	sourceAgent := head.Provenance.SourceAgent
	if sourceAgent == "" {
		sourceAgent = "aplexica"
	}

	var path string
	var materialized bool
	if st, ok := target.(adapter.ConversationSessionTarget); ok {
		path, materialized, err = o.materializeConversationBranchSession(st, agentName, art, head, sourceAgent)
	} else if dt, ok := target.(adapter.ConversationDocTarget); ok {
		path, materialized, err = o.materializeConversationBranchDoc(dt, agentName, art, head, sourceAgent, branch)
	} else {
		return "", false, fmt.Errorf("materialize: agent %q does not support conversation materialization", agentName)
	}
	if err != nil || !materialized {
		return path, materialized, err
	}
	if err := o.markConversationBranchMaterialized(art, agentName, branch); err != nil {
		return path, materialized, err
	}
	return path, materialized, nil
}

func (o *Orchestrator) materializeConversationBranchSession(
	st adapter.ConversationSessionTarget,
	agentName string,
	art acf.Artifact,
	head acf.Event,
	sourceAgent string,
) (string, bool, error) {
	if planner, ok := st.(adapter.ConversationSessionPathTarget); ok {
		path, ok, reason, err := planConversationSessionPath(planner, art, head, sourceAgent)
		if err != nil {
			o.recordAdapterError(agentName, err)
			return "", false, err
		}
		if !ok {
			return "", false, fmt.Errorf("materialize: agent %q declined conversation session path (%s)",
				agentName, conversationDeclineExplanation(reason))
		}
		if path != "" {
			if o.destChangedUnderUs(path) {
				o.markClaudeHotSession(path)
				return "", false, fmt.Errorf("materialize: destination changed under us; wait for the pending import, then retry")
			}
			if o.guard != nil {
				o.guard.Mark(path)
			}
			o.markClaudeHotSession(path)
		}
	}
	path, ok, reason, err := materializeConversationSessionInto(st, art, head, sourceAgent)
	if err != nil {
		o.recordAdapterError(agentName, err)
		return path, false, err
	}
	if !ok {
		return path, false, fmt.Errorf("materialize: agent %q did not materialize the conversation (%s)",
			agentName, conversationDeclineExplanation(reason))
	}
	if path != "" {
		if o.guard != nil {
			o.guard.Mark(path)
		}
		o.recordDestHash(path)
		o.markClaudeHotSession(path)
	}
	o.recordAdapterSuccess(agentName)
	return path, true, nil
}

func (o *Orchestrator) materializeConversationBranchDoc(
	dt adapter.ConversationDocTarget,
	agentName string,
	art acf.Artifact,
	head acf.Event,
	sourceAgent string,
	branch string,
) (string, bool, error) {
	dir, ok := dt.ConversationDocDir()
	if !ok {
		return "", false, fmt.Errorf("materialize: agent %q has no conversation document directory", agentName)
	}
	path := filepath.Join(dir, conversationDocFilenameForBranch(sourceAgent, art.ArtifactID, branch))
	if o.destChangedUnderUs(path) {
		return "", false, fmt.Errorf("materialize: destination changed under us; wait for the pending import, then retry")
	}
	md, err := renderConversationMarkdown(art, sourceAgent, head)
	if err != nil {
		o.recordAdapterError(agentName, err)
		return "", false, err
	}
	if o.guard != nil {
		o.guard.Mark(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		o.recordAdapterError(agentName, err)
		return "", false, err
	}
	if err := atomicfile.WriteFile(path, []byte(md), 0o644); err != nil {
		o.recordAdapterError(agentName, err)
		return "", false, err
	}
	o.recordDestHash(path)
	o.recordAdapterSuccess(agentName)
	return path, true, nil
}

func (o *Orchestrator) markConversationBranchMaterialized(art acf.Artifact, agentName, branch string) error {
	cur, err := o.cfg.Store.ReadArtifact(art.Kind, art.ArtifactID)
	if err != nil {
		return fmt.Errorf("materialize: read artifact after write: %w", err)
	}
	if cur.MaterializedBranchByAgent == nil {
		cur.MaterializedBranchByAgent = map[string]string{}
	}
	cur.MaterializedBranchByAgent[agentName] = branch
	seen := false
	for _, name := range cur.SyncedAgents {
		if name == agentName {
			seen = true
			break
		}
	}
	if !seen {
		cur.SyncedAgents = append(cur.SyncedAgents, agentName)
		sortStrings(cur.SyncedAgents)
	}
	if err := o.cfg.Store.WriteArtifact(cur); err != nil {
		return fmt.Errorf("materialize: write artifact branch pointer: %w", err)
	}
	_ = o.cfg.Store.ClearOrphan(art.ArtifactID, agentName)
	return nil
}

// RefanOutAll re-runs fanOut for EVERY artifact in the canonical store,
// regardless of scope. It backfills a newly-enabled fan-out target: the
// await-config gate only makes FUTURE artifact events fan out to a freshly
// enabled agent, so this pass materializes the artifacts ALREADY in the
// store too (so flipping the gate on actually syncs what's there). fanOut
// still honors the gate + routing rules, so only currently-enabled targets
// receive. Returns the number of artifacts re-fanned. Per-artifact errors
// are logged inside fanOut and don't short-circuit the pass.
func (o *Orchestrator) RefanOutAll(ctx context.Context) (int, error) {
	o.nativeRestoreGate.RLock()
	defer o.nativeRestoreGate.RUnlock()
	if o.cfg.Store == nil {
		return 0, fmt.Errorf("refanout-all: store is nil")
	}
	count := 0
	// Memories/skills/tools are few — back-fill them all. Conversations can be
	// enormous, so they go through backfillConversations, which caps each agent
	// at its most-recent-N (DefaultConvBackfill) to avoid flooding a freshly
	// enabled agent with another agent's entire conversation history.
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool} {
		artifacts, err := o.cfg.Store.ListArtifacts(kind)
		if err != nil {
			continue
		}
		for _, art := range artifacts {
			primary, _ := o.backfillPrimary(art)
			contextDir := ""
			if art.Scope == acf.ScopeProject && art.Project != nil {
				contextDir = art.Project.Path
			}
			o.fanOut(ctx, primary, []string{art.ArtifactID}, contextDir, art.SourcePath, false, nil)
			count++
		}
	}
	count += o.backfillConversations(ctx)
	return count, nil
}

// IngestExternalConversations propagates conversations that were imported into
// the canonical store OUTSIDE the file-watcher/handleEvent path — currently the
// hermeswatch poller, which imports Hermes state.db sessions through the adapter
// directly (store.AppendEvent) and so never reaches handleEvent's fan-out +
// forward tail. For each conversation id it fans out to the OTHER agents
// (`primary` is the source agent, so the artifact is not materialized back to
// itself) and forwards the committed head to the relay. Without this, a turn a
// user adds by resuming a materialized conversation in Hermes reaches neither
// sibling agents (Claude Code/Codex/…) nor other devices until the next daemon
// restart's catch-up scan. fanOut's unresolved-divergence gate still applies, so
// a conflicted artifact is withheld.
func (o *Orchestrator) IngestExternalConversations(ctx context.Context, primary adapter.Adapter, ids []string) {
	if len(ids) == 0 || o.cfg.Store == nil {
		return
	}
	for _, id := range ids {
		sourcePath := ""
		if art, err := o.cfg.Store.ReadArtifact(acf.KindConversation, id); err == nil {
			sourcePath = art.SourcePath
		}
		// includePrimary=false: the source agent (Hermes) already holds the turn.
		// convBackfillAllow=nil: live propagation, never capped.
		o.fanOut(ctx, primary, []string{id}, "", sourcePath, false, nil)
	}
	if o.remoteEventPublisher() != nil {
		for _, id := range ids {
			o.forwardCommitted(id)
		}
	}
	o.setLastActivity(time.Now())
}

// AdapterLastErrors returns a snapshot of the orchestrator's per-
// adapter last-error map (ADR-0159 Candidate D). Each value is the
// most-recent error string for an adapter whose Export failed, with
// $HOME-prefixed paths redacted to "~/" so the wire shape doesn't
// leak local-filesystem layout. Cleared on next success.
//
// Empty map when no adapter has errored recently. Returns a copy.
// v0.51.0.
func (o *Orchestrator) AdapterLastErrors() map[string]string {
	if !o.mu.TryLock() {
		return nil
	}
	defer o.mu.Unlock()
	if len(o.adapterLastErr) == 0 {
		return nil // omitempty-friendly
	}
	out := make(map[string]string, len(o.adapterLastErr))
	for k, v := range o.adapterLastErr {
		out[k] = v
	}
	return out
}

// recordAdapterSuccess stamps the adapter as recently-touched AND
// clears any stale error string for it. Called from primaryImport
// success path and from per-adapter fanOut Export success.
func (o *Orchestrator) recordAdapterSuccess(name string) {
	o.mu.Lock()
	if o.adapterTouched == nil {
		o.adapterTouched = map[string]time.Time{}
	}
	o.adapterTouched[name] = time.Now()
	delete(o.adapterLastErr, name)
	o.mu.Unlock()
	// NOTE: the live "agent.activity" SSE ping used to fire here, but it was
	// contentless (just {adapter,state}) and flooded the Event stream with a
	// row per import. handleEvent now publishes a meaningful "artifact.synced"
	// event instead. The adapterTouched stamp above still drives the agents'
	// active indicator (read via the status API, not SSE).
}

// recordAdapterError captures the redacted error string for an adapter
// whose operation failed. Path redaction strips $HOME prefixes to
// "~/" — minimal but covers the most common username-leak vector.
// Called from per-adapter fanOut Export error path.
// onWatcherError surfaces a platform watcher Source error (inotify budget /
// ENOSPC polling fallback / Windows RDC overflow) through the same status
// channel as adapter errors instead of draining it silently, so `aplexica
// status` and the tray warn the user (FR-03.5 / BRD-03 §4.3).
func (o *Orchestrator) onWatcherError(err error) {
	o.recordAdapterError("watcher", err)
}

func (o *Orchestrator) recordAdapterError(name string, err error) {
	if err == nil {
		return
	}
	msg := redactPaths(err.Error())
	o.mu.Lock()
	if o.adapterLastErr == nil {
		o.adapterLastErr = map[string]string{}
	}
	o.adapterLastErr[name] = msg
	o.mu.Unlock()
}

// maxArtifactBytes resolves the effective inbound size cap: Config.MaxArtifactBytes
// when positive, defaultMaxArtifactBytes (64 MiB) when zero, and a non-positive
// sentinel (caller treats <= 0 as "no cap") when the operator set it negative.
func (o *Orchestrator) maxArtifactBytes() int64 {
	if o.cfg.MaxArtifactBytes == 0 {
		return defaultMaxArtifactBytes
	}
	return o.cfg.MaxArtifactBytes
}

// maxSessionFileBytes resolves the effective agent-session-transcript size
// cap: Config.MaxSessionFileBytes when positive, defaultMaxSessionFileBytes
// (512 MiB) when zero, and a non-positive sentinel (caller treats <= 0 as
// "no cap") when the operator set it negative.
func (o *Orchestrator) maxSessionFileBytes() int64 {
	if o.cfg.MaxSessionFileBytes == 0 {
		return defaultMaxSessionFileBytes
	}
	return o.cfg.MaxSessionFileBytes
}

// ingestSizeLimit returns the size cap governing path at the handleEvent
// gate: agent session transcripts (Claude/Codex) get the session cap,
// everything else the generic artifact cap. <= 0 means "no cap".
func (o *Orchestrator) ingestSizeLimit(path string) int64 {
	if isClaudeSessionPath(path) || isCodexSessionPath(path) {
		return o.maxSessionFileBytes()
	}
	return o.maxArtifactBytes()
}

// shouldReportOversize reports whether path's refusal is due for a bus event:
// first refusal ever, or oversizeReportInterval elapsed since the last one.
// Marks the report time when returning true. Guarded by o.mu.
func (o *Orchestrator) shouldReportOversize(path string) bool {
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.oversizeReported == nil {
		o.oversizeReported = map[string]time.Time{}
	}
	if last, ok := o.oversizeReported[path]; ok && now.Sub(last) < oversizeReportInterval {
		return false
	}
	o.oversizeReported[path] = now
	return true
}

// clearOversizeReported forgets a path's refusal-report state once it passes
// the size gate again, so a later regrowth re-reports promptly.
func (o *Orchestrator) clearOversizeReported(path string) {
	o.mu.Lock()
	if len(o.oversizeReported) > 0 {
		delete(o.oversizeReported, path)
	}
	o.mu.Unlock()
}

// recordOversizeArtifact surfaces a max-artifact-size refusal (BRD-03 §4.3) on
// the same status channel as adapter errors (so `aplexica status` and the tray
// warn the user per FR-03.5) and on the live event stream. The keyed name
// "max-artifact-size" groups every refusal under one user-facing warning slot
// rather than one per oversized path. The path is redacted via recordAdapterError.
func (o *Orchestrator) recordOversizeArtifact(path string, size, limit int64) {
	o.recordAdapterError("max-artifact-size", fmt.Errorf(
		"refused %s: %d bytes exceeds max artifact size %d bytes (BRD-03 §4.3); not ingested",
		path, size, limit))
	o.publishEvent("artifact.refused", map[string]any{
		"name":       filepath.Base(path),
		"action":     "refused",
		"sourcePath": redactPaths(path),
		"size":       size,
		"limit":      limit,
		"reason":     "max-artifact-size",
	})
}

func (o *Orchestrator) publishArtifactSyncedEvent(sourceAgent, artifactID, fallbackPath string) {
	if o.cfg.EventPublisher == nil {
		return
	}
	o.publishEvent("artifact.synced", o.artifactSyncedEventBody(sourceAgent, artifactID, fallbackPath))
}

func (o *Orchestrator) artifactSyncedEventBody(sourceAgent, artifactID, fallbackPath string) map[string]any {
	name := filepath.Base(fallbackPath)
	sourcePath := fallbackPath
	body := map[string]any{
		"source":     sourceAgent,
		"agent":      sourceAgent,
		"artifactId": artifactID,
		"name":       name,
		"action":     "synced",
		"origin":     "local",
	}
	if art, found := o.findArtifact(artifactID); found {
		if art.Name != "" {
			name = art.Name
			body["name"] = name
		}
		body["kind"] = string(art.Kind)
		body["scope"] = string(art.Scope)
		if art.SourcePath != "" {
			sourcePath = art.SourcePath
		}
		targets := eventTargetAgents(art.SyncedAgents)
		if len(targets) > 0 {
			body["targetAgents"] = targets
		}
		if art.Project != nil {
			body["projectId"] = art.Project.ID
			if art.Project.Path != "" {
				body["projectPath"] = redactPaths(art.Project.Path)
			}
		}
	}
	if sourcePath != "" {
		body["sourcePath"] = redactPaths(sourcePath)
	}
	return body
}

func eventTargetAgents(agents []string) []string {
	if len(agents) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(agents))
	out := make([]string, 0, len(agents))
	for _, agent := range agents {
		if agent == "" {
			continue
		}
		if _, ok := seen[agent]; ok {
			continue
		}
		seen[agent] = struct{}{}
		out = append(out, agent)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// redactPaths strips $HOME-prefixed substrings from s, replacing with
// "~/". Best-effort: relies on os.UserHomeDir which may return an
// empty string on weird environments (in which case redaction is a
// no-op and the caller falls back to the raw error string).
func redactPaths(s string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return s
	}
	// Replace both "/Users/foo" and (just in case) "/Users/foo/"
	// prefixes with "~" / "~/" respectively.
	s = strings.ReplaceAll(s, home+"/", "~/")
	s = strings.ReplaceAll(s, home, "~")
	return s
}

// publishEvent forwards a kind+body to the configured EventPublisher
// (the daemon's sse.Bus in production). No-op when no publisher is
// wired — keeps unit tests that don't care about events terse.
//
// Best-effort: never blocks, never errors. The publisher's own back-
// pressure handling (channel buffer + drop-on-full in sse.Bus)
// absorbs slow subscribers.
func (o *Orchestrator) publishEvent(kind string, body any) {
	if o.cfg.EventPublisher == nil {
		return
	}
	o.cfg.EventPublisher.Publish(kind, body)
}

// publishOutbound forwards a freshly-committed canonical-store event
// to the configured RemoteEventPublisher. No-op when no publisher is
// wired (typical OSS-only daemon).
//
// Receivers MUST treat this as best-effort and enqueue without
// blocking — see RemoteEventPublisher's contract.
//
// A committed conversation head arrives here TWICE — once per lane
// (OutboundEvent.Lane): the verbatim delta on LaneLive (the head's own
// EventID) and the full materialized state on LaneRetained (the distinct
// RetainedWireEventID). Transports route by lane (live topic vs retained
// topic); receivers dedupe live events by EventID and retained events by
// content. Non-conversation kinds publish once, on LaneLive.
//
// Lanes use aligned-chains delta sync.
func (o *Orchestrator) publishOutbound(event OutboundEvent) {
	pub := o.remoteEventPublisher()
	if pub == nil {
		return
	}
	pub.PublishOutbound(event)
}

// remoteEventPublisher returns the live outbound hook under o.mu so
// SetRemoteEventPublisher can install/clear it concurrently with a commit.
// Returns a properly-nil interface when none is wired (the typed-nil hazard is
// avoided because the daemon only ever calls SetRemoteEventPublisher with a
// non-nil adapter).
func (o *Orchestrator) remoteEventPublisher() RemoteEventPublisher {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg.RemoteEventPublisher
}

// setLastActivity stamps the orchestrator's lastActivity field. Called
// at the end of every successful handleEvent and at the end of
// InitialScan when at least one artifact was touched. Mu-protected so
// concurrent LastActivity() readers always see a coherent value.
func (o *Orchestrator) setLastActivity(t time.Time) {
	o.mu.Lock()
	o.lastActivity = t
	o.mu.Unlock()
}

// snapshotCadence returns the current SnapshotCadence map under o.mu.
// Used by handleEvent to coordinate with SetSnapshotCadence (SIGHUP
// hot-reload). Returns a nil/empty map when disabled. The returned
// map reference is the live config map; callers must NOT mutate it.
func (o *Orchestrator) snapshotCadence() map[acf.Kind]int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg.SnapshotCadence
}

// SetSnapshotCadence replaces the per-kind snapshot cadence map live.
// Used by the daemon's SIGHUP handler when any of the per-kind
// daemon.Config fields change. A nil/empty map disables
// auto-snapshotting entirely; per-kind entries set to <=0 disable that
// kind only. Safe to call from any goroutine; subsequent handleEvent
// calls observe the new value.
func (o *Orchestrator) SetSnapshotCadence(cadence map[acf.Kind]int) {
	o.mu.Lock()
	o.cfg.SnapshotCadence = cadence
	o.mu.Unlock()
}

// SetRulesEngine swaps the orchestrator's live selective-sync rules
// engine. The local web UI calls this after a rule Add/Update/Delete so
// edits hot-apply without a daemon restart (the accessor rebuilds the
// engine from the updated rules.toml and hands the new engine here).
// Guarded by the same mutex as SetSnapshotCadence; subsequent fan-out
// cycles observe the new engine. A nil engine disables rule gating.
func (o *Orchestrator) SetRulesEngine(eng *syncrules.Engine) {
	o.mu.Lock()
	o.cfg.RulesEngine = eng
	o.mu.Unlock()
}

// SetRemoteEventPublisher installs (or clears) the outbound remote-event hook
// live under the orchestrator mutex. The daemon wires its RemotePublishAdapter
// here AFTER both the orchestrator and the RemoteRunner are constructed (the
// orchestrator is built first). Pass nil to disable outbound forwarding.
func (o *Orchestrator) SetRemoteEventPublisher(p RemoteEventPublisher) {
	o.mu.Lock()
	o.cfg.RemoteEventPublisher = p
	o.mu.Unlock()
}

// SetSyncLatencyObserver installs (or clears) the NFR-10 §5.2 sync-latency
// sink live under the orchestrator mutex. The daemon wires its metrics
// registry here on startup; observation is a cheap in-memory histogram bump,
// so it runs regardless of whether the HTTP endpoint is enabled (the series
// is only exposed over /metrics when it is). Pass nil to disable
// instrumentation (the import path then records no latency).
func (o *Orchestrator) SetSyncLatencyObserver(obs SyncLatencyObserver) {
	o.mu.Lock()
	o.cfg.SyncLatencyObserver = obs
	o.mu.Unlock()
}

// syncLatencyObserver returns the live sink under o.mu so SetSyncLatencyObserver
// can swap it concurrently with an import cycle.
func (o *Orchestrator) syncLatencyObserver() SyncLatencyObserver {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg.SyncLatencyObserver
}

// SetRecipientResolver installs the outbound E2E-encryption recipient resolver
// live under the orchestrator mutex. The daemon wires its *RecipientResolver
// here after both the orchestrator and the RemoteRunner exist. Nil = no
// recipients resolvable, so every outbound event is dropped (never plaintext).
// Optional remote-transport support.
func (o *Orchestrator) SetRecipientResolver(r RecipientResolver) {
	o.mu.Lock()
	o.cfg.RecipientResolver = r
	o.mu.Unlock()
}

// SetDeviceKeyProvider installs the inbound-decrypt device key provider live
// under the orchestrator mutex. Nil = inbound envelopes cannot be opened.
func (o *Orchestrator) SetDeviceKeyProvider(p DeviceKeyProvider) {
	o.mu.Lock()
	o.cfg.DeviceKeyProvider = p
	o.mu.Unlock()
}

func (o *Orchestrator) SetVerifiedRosterProvider(p VerifiedRosterProvider) {
	o.mu.Lock()
	o.cfg.VerifiedRosterProvider = p
	o.mu.Unlock()
}

func (o *Orchestrator) SetV2IdentityProvider(p V2IdentityProvider) {
	o.mu.Lock()
	o.cfg.V2IdentityProvider = p
	o.mu.Unlock()
}

func (o *Orchestrator) SetNamespaceKeyProvider(p keyrotation.NamespaceKeyProvider) {
	o.mu.Lock()
	o.cfg.NamespaceKeyProvider = p
	o.mu.Unlock()
}

// recipientResolver / deviceKeyProvider return the live seams under o.mu so the
// setters can install them concurrently with a commit.
func (o *Orchestrator) recipientResolver() RecipientResolver {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg.RecipientResolver
}

func (o *Orchestrator) deviceKeyProvider() DeviceKeyProvider {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg.DeviceKeyProvider
}

func (o *Orchestrator) verifiedRosterProvider() VerifiedRosterProvider {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg.VerifiedRosterProvider
}
func (o *Orchestrator) v2IdentityProvider() V2IdentityProvider {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg.V2IdentityProvider
}
func (o *Orchestrator) namespaceKeyProvider() keyrotation.NamespaceKeyProvider {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg.NamespaceKeyProvider
}

// SetSyncGate swaps the live await-config fan-out gate (FR-03.3) under the
// orchestrator mutex, so the portal / CLI can flip per-agent fan-out
// enablement without a daemon restart.
func (o *Orchestrator) SetSyncGate(g *syncgate.Gate) {
	o.mu.Lock()
	o.cfg.SyncGate = g
	o.mu.Unlock()
}

// syncGate returns the live gate under the orchestrator mutex so SetSyncGate
// can swap it concurrently with a fan-out cycle.
func (o *Orchestrator) syncGate() *syncgate.Gate {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg.SyncGate
}

// SetWriteAuthorizer swaps the live desync-safe client-side write gate
// under the orchestrator mutex, so the daemon can wire its *RoleService AFTER
// both the orchestrator and the role service are constructed (the daemon
// builds the orchestrator before the role service), and so a pairing /
// un-pairing transition can install or clear the gate without a restart. A
// nil authorizer disables the gate (today's default: every commit proceeds).
func (o *Orchestrator) SetWriteAuthorizer(a WriteAuthorizer) {
	o.mu.Lock()
	o.cfg.WriteAuthorizer = a
	o.mu.Unlock()
}

// writeAuthorizer returns the live write-gate under the orchestrator mutex so
// SetWriteAuthorizer can swap it concurrently with a commit.
func (o *Orchestrator) writeAuthorizer() WriteAuthorizer {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg.WriteAuthorizer
}

// commitNamespaceEvent is the SINGLE desync-safe pre-commit chokepoint for a
// namespace-scoped local mutation. It consults the WriteAuthorizer BEFORE
// touching the canonical store and aborts cleanly — leaving the store and the
// hash-chain byte-for-byte unchanged (no Store.AppendEvent, no event line, no
// chain extension) — if and only if the authorizer returns a DEFINITIVE
// permission deny. On a proceed (nil authorizer, or any unknown/unpaired/
// offline answer, or a sufficient role), it appends the event exactly as the
// ungated path would.
//
// The check is strictly ordered: deny first, mutate second. Because the
// authorizer never denies on a non-definitive answer (see
// Config.WriteAuthorizer), this can never refuse AFTER the commit could have
// occurred, so it can never desync the local chain from peers. The server
// stays authoritative; this only refuses earlier.
//
// V1 NOTE: no LOCAL namespace-scoped artifact mutation is reachable yet
// (ScopeNamespace is V2-reserved; adapter imports produce only global/project
// artifacts; acf.Artifact carries no NamespaceID). This method is the seam the
// future namespace-write path will route through — namespaceID is supplied by
// that caller, since it cannot be read off the artifact at the commit boundary.
func (o *Orchestrator) commitNamespaceEvent(ctx context.Context, namespaceID string, op rbac.Operation, kind acf.Kind, e acf.Event) error {
	if auth := o.writeAuthorizer(); auth != nil {
		if err := auth.Authorize(ctx, namespaceID, op); err != nil {
			// DEFINITIVE deny: abort BEFORE any store mutation. The store and
			// the hash-chain are left exactly as they were.
			return err
		}
	}
	return o.cfg.Store.AppendEvent(kind, e)
}

// FanOutEnabled reports whether agentName may participate in cross-agent
// fan-out under the await-config gate (a nil gate is permissive). Surfaced via
// the web API so the portal can show + toggle per-agent fan-out.
func (o *Orchestrator) FanOutEnabled(agentName string) bool {
	g := o.syncGate()
	return g == nil || g.Enabled(agentName)
}

// LocalDeviceID returns this device's cloud identity (empty when unpaired).
// Surfaced so the web API can tell a cross-DEVICE sync (an event whose
// provenance device differs from this one) apart from a local import — the two
// look identical when the same agent (e.g. claude-code) authored the artifact
// on both devices.
func (o *Orchestrator) LocalDeviceID() string { return o.localDeviceID() }

// SetLocalDeviceID replaces the cloud device identity at runtime. Pairing (and
// especially RE-pairing, which retires the previous cloud id) must call this:
// the id is otherwise seeded once per process from the plugin's --status, and
// a daemon that keeps stamping a retired id has every durable append rejected
// as publisher_identity_conflict until restart. An empty id is refused: the
// identity is sticky-until-replaced, and blanking it mid-flight would tear
// the non-empty-then-compare reads scattered through the import path.
func (o *Orchestrator) SetLocalDeviceID(id string) {
	if id == "" {
		return
	}
	// o.mu makes read-compare-store one step so concurrent setters cannot
	// interleave (and mislog which id was replaced); reads stay lock-free.
	o.mu.Lock()
	defer o.mu.Unlock()
	old := o.localDeviceID()
	if old == id {
		return
	}
	o.localDeviceIDOverride.Store(id)
	if o.cfg.Logger == nil {
		return
	}
	if old == "" {
		o.cfg.Logger.Info("sync: cloud device identity adopted", "device_id", id)
	} else {
		o.cfg.Logger.Warn("sync: cloud device identity rotated", "old_device_id", old, "new_device_id", id)
	}
}

// localDeviceID is the single identity read path: the runtime override when
// one was applied, else the construction-time Config seed. Every consumer of
// the local cloud identity MUST go through here (not cfg.LocalDeviceID) so a
// re-pair rotation is observed without a daemon restart.
func (o *Orchestrator) localDeviceID() string {
	if v, ok := o.localDeviceIDOverride.Load().(string); ok {
		return v
	}
	return o.cfg.LocalDeviceID
}

// rulesEngine returns the live rules engine under the orchestrator mutex
// so SetRulesEngine can swap it concurrently with a fan-out cycle.
func (o *Orchestrator) rulesEngine() *syncrules.Engine {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg.RulesEngine
}

// RulesEngine returns the live selective-sync engine (the same value a
// fan-out cycle would consult). Exported read-only counterpart to
// SetRulesEngine; the daemon uses it to confirm a hot-reload landed and
// tests use it to observe an Add/Update/Delete taking effect. May be nil
// when rule gating is disabled.
func (o *Orchestrator) RulesEngine() *syncrules.Engine {
	return o.rulesEngine()
}

// maxDestHashBytes caps the file size the read-before-clobber tracking will
// hash. Native text artifacts (memory/skill/config files) are tiny; anything
// larger (session DBs, huge transcripts) is skipped — those paths aren't
// fan-out-overwritten file destinations in the same sense, and hashing them
// per cycle would be wasteful.
const maxDestHashBytes = 8 << 20

// destFingerprint records what a destination file looked like when the
// orchestrator last wrote or imported it. hash is empty when the file was
// larger than maxDestHashBytes — size/mtime then carry the change check alone.
type destFingerprint struct {
	size      int64
	mtimeNano int64
	hash      string
}

// fingerprintDest stats (and, when small enough, hashes) path. ok=false when
// the file is missing, unreadable, or a directory.
func fingerprintDest(path string) (destFingerprint, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return destFingerprint{}, false
	}
	fp := destFingerprint{size: fi.Size(), mtimeNano: fi.ModTime().UnixNano()}
	if fi.Size() <= maxDestHashBytes {
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return destFingerprint{}, false
		}
		sum := sha256.Sum256(data)
		fp.hash = hex.EncodeToString(sum[:])
	}
	return fp, true
}

// recordDestHash stores a fingerprint of path's current content in
// destHashes. Called after a successful import of path and after a
// successful fan-out Export to path. Best-effort: missing or unreadable
// files just clear any stale entry (so a later destChangedUnderUs can't act
// on stale state).
func (o *Orchestrator) recordDestHash(path string) {
	fp, ok := fingerprintDest(path)
	o.mu.Lock()
	defer o.mu.Unlock()
	if !ok {
		delete(o.destHashes, path)
		return
	}
	o.destHashes[path] = fp
}

// recordDestFingerprint records a fingerprint captured by the caller before
// an import. It is intentionally separate from recordDestHash, which is for
// files the orchestrator has just finished writing itself.
func (o *Orchestrator) recordDestFingerprint(path string, fp destFingerprint) {
	o.mu.Lock()
	o.destHashes[path] = fp
	o.mu.Unlock()
}

// destHashKnown reports whether the orchestrator has already established a
// baseline for path. Native surface mirrors use this to distinguish normal
// read-before-clobber checks from their stricter first-contact policy.
func (o *Orchestrator) destHashKnown(path string) bool {
	o.mu.Lock()
	_, ok := o.destHashes[path]
	o.mu.Unlock()
	return ok
}

// destChangedUnderUs reports whether path's current content differs from what
// the orchestrator last wrote to / imported from it. Unknown paths (no record)
// and missing/unreadable files report false — first-contact exports proceed
// exactly as before. A byte-stable file (same size+mtime) is never an edit; a
// metadata change with an identical hash (atomic same-content rewrite) is not
// an edit either; a metadata change we cannot hash-verify (oversized) is
// treated as edited — deferring a fan-out is always safer than clobbering.
func (o *Orchestrator) destChangedUnderUs(path string) bool {
	o.mu.Lock()
	known, ok := o.destHashes[path]
	o.mu.Unlock()
	if !ok {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	if fi.Size() == known.size && fi.ModTime().UnixNano() == known.mtimeNano {
		return false
	}
	if known.hash != "" && fi.Size() <= maxDestHashBytes {
		if data, rerr := os.ReadFile(path); rerr == nil {
			sum := sha256.Sum256(data)
			return hex.EncodeToString(sum[:]) != known.hash
		}
	}
	return true
}

// lockConversationMaterialize acquires the per-artifact materialization lock
// and returns its unlock func.
func (o *Orchestrator) lockConversationMaterialize(artifactID string) func() {
	o.convMaterializeMu.Lock()
	if o.convMaterializeLocks == nil {
		o.convMaterializeLocks = map[string]*sync.Mutex{}
	}
	mu, ok := o.convMaterializeLocks[artifactID]
	if !ok {
		mu = &sync.Mutex{}
		o.convMaterializeLocks[artifactID] = mu
	}
	o.convMaterializeMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// maybeRecordConflict inspects the last materialized content events for
// an artifact and records a conflict file if they look divergent (different
// agents + different payloads + within ConflictWindow). Snapshot/fork/merge
// bookkeeping events are skipped because they are not agent-authored content
// heads. Best-effort; any read errors silently skip the artifact.
//
// Returns true ONLY when an UNRESOLVED divergence was recorded (the
// ConflictStore.Record path). Every early return — and the
// semantically-equivalent auto-resolve/Clear path — returns false, because
// those leave no divergent head requiring human resolution. handleEvent uses
// this signal to WITHHOLD propagation of a divergent head until the user runs
// `aplexica resolve <artifactId>` (BRD-03 §4.6 / §10 OQ-03.2): a detected
// divergence must create a branch and NOT propagate either version to other
// agents (locally or to the remote) before resolution.
const adjacentAssistantConflictChainEvents = 2 + 1

func (o *Orchestrator) maybeRecordConflict(primary adapter.Adapter, artifactID string) bool {
	art, found := o.findArtifact(artifactID)
	if !found {
		return false
	}
	events, err := o.cfg.Store.ReadRecentEvents(
		art.Kind,
		artifactID,
		adjacentAssistantConflictChainEvents,
		acf.EventTypeCreate,
		acf.EventTypeUpdate,
		acf.EventTypeResolution,
	)
	if err != nil || len(events) < 2 {
		return false // can only conflict with at least 2 events
	}
	prev, latest := events[len(events)-2], events[len(events)-1]

	// Conversation deltas are append operations, not competing document
	// snapshots. A normal cross-agent handoff therefore looks exactly like
	// different agents writing different payloads inside ConflictWindow: the
	// first agent finishes an answer and the second appends the next question.
	// Treat a hash-linked delta on the same branch as the linear successor it
	// declares itself to be. Full snapshots and non-linked events still flow
	// through the conservative divergence detector below.
	if art.Kind == acf.KindConversation && linearConversationDelta(prev, latest) {
		o.clearExactLinearConversationConflict(artifactID, events)
		return false
	}
	// A tagged adjacent-answer correction is a content-removing event, so it
	// gets a stricter proof than ordinary semantic equivalence. It may close an
	// existing sidecar only when the immediately preceding dirty-full/Claude-
	// delta heads, their parent chain, the complete clean payload, and the recorded conflict
	// heads all match exactly. Missing, corrupt, changed, or mismatched sidecars
	// are never overwritten or unconditionally cleared. A valid correction with
	// no sidecar is still not a new conflict.
	if art.Kind == acf.KindConversation &&
		o.isAuthenticatedAdjacentAssistantCorrection(primary, artifactID, events) {
		return false
	}

	// Different agents?
	if prev.Provenance.SourceAgent == "" ||
		latest.Provenance.SourceAgent == "" ||
		prev.Provenance.SourceAgent == latest.Provenance.SourceAgent {
		return false
	}
	// Within window? (treat negative deltas — clock skew between agents —
	// as still inside the window; absolute value)
	delta := latest.Timestamp.Sub(prev.Timestamp)
	if delta < 0 {
		delta = -delta
	}
	if delta > o.cfg.ConflictWindow {
		return false
	}
	// Different content?
	if string(prev.Payload) == string(latest.Payload) {
		return false
	}
	// A generated session may re-encode the complete portable transcript when
	// another agent opens or continues it. Native formats assign their own
	// timestamps to materialized rows, so the replacement is not necessarily a
	// canonical-event prefix even though its visible transcript is exactly the
	// state produced by the preceding delta chain. Compare the replacement
	// against the materialized state immediately before it; this is a
	// convergence checkpoint, not a competing edit. Keep this after the cheap
	// source/window/payload checks so large histories are replayed only for an
	// otherwise genuine conflict candidate.
	if art.Kind == acf.KindConversation &&
		equivalentConversationProjection(o.cfg.Store, artifactID, latest) {
		if o.cfg.ConflictStore != nil {
			_, _ = o.cfg.ConflictStore.ClearIf(conflicts.Conflict{
				ArtifactID: artifactID,
				Kind:       acf.KindConversation,
				Heads: []conflicts.Head{
					conflictHeadFromEvent(prev),
					conflictHeadFromEvent(latest),
				},
			})
		}
		return false
	}
	if conflicts.SemanticallyEquivalent(art.Kind, prev, latest) {
		o.autoResolveEquivalentConflict(art.Kind, artifactID, prev, latest)
		_ = o.cfg.ConflictStore.Clear(artifactID)
		return false
	}

	c := conflicts.Conflict{
		ArtifactID: artifactID,
		Kind:       art.Kind,
		Heads: []conflicts.Head{
			conflictHeadFromEvent(prev),
			conflictHeadFromEvent(latest),
		},
	}
	// Best-effort write; the daemon's own logger handles operator
	// visibility. We don't fail the import on a conflict-store error.
	_ = o.cfg.ConflictStore.Record(c)
	return true
}

func linearConversationDelta(parent, child acf.Event) bool {
	if parent.Hash == "" || child.ParentHash != parent.Hash ||
		normalizeBranchName(parent.Branch) != normalizeBranchName(child.Branch) {
		return false
	}
	payload, err := acf.DecodeConversationPayload(child)
	return err == nil && payload.Format == acf.ConversationDeltaFormatV1 && len(payload.Events) > 0
}

func equivalentConversationProjection(store *acf.Store, artifactID string, replacement acf.Event) bool {
	replacementPayload, err := acf.DecodeConversationPayload(replacement)
	if err != nil || replacementPayload.Format != acf.ConversationFormatV1 {
		return false
	}
	for _, event := range replacementPayload.Events {
		if event.Type != acf.EventTypeTurn ||
			(event.Role != "user" && event.Role != "assistant") ||
			len(acf.ExtractTextTurns([]acf.ConversationEvent{event})) != 1 {
			return false
		}
	}
	// Active events are in canonical append order. The forensic
	// ReadEventsIncludingCompacted API sorts by wall clock, which is unsuitable
	// here because this detector explicitly supports clock-skewed agents.
	events, err := store.ReadEvents(acf.KindConversation, artifactID)
	if err != nil {
		return false
	}
	replacementIndex := -1
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].EventID == replacement.EventID {
			replacementIndex = i
			break
		}
	}
	if replacementIndex <= 0 {
		return false
	}
	history := events[:replacementIndex]
	hasAnchor := false
	for _, event := range history {
		if !acf.HasPayload(event.Payload) {
			continue
		}
		payload, decodeErr := acf.DecodeConversationPayload(event)
		if decodeErr != nil {
			return false
		}
		if payload.Format == acf.ConversationFormatV1 {
			hasAnchor = true
		}
	}
	if !hasAnchor {
		// A delta-only active window cannot prove the complete prior state.
		return false
	}
	current, ok, err := acf.MaterializedConversationPayload(history)
	if err != nil || !ok || current.Format != acf.ConversationFormatV1 {
		return false
	}
	return acf.TextTurnsEqual(
		acf.ExtractTextTurns(current.Events),
		acf.ExtractTextTurns(replacementPayload.Events),
	) && reflect.DeepEqual(current.Attachments, replacementPayload.Attachments)
}

// clearExactLinearConversationConflict compare-deletes a sidecar produced by
// the pre-fix detector when its recorded heads are an adjacent, hash-linked
// conversation delta pair in the recent event chain. The exact-head check
// preserves a newer or genuinely divergent conflict that raced this repair.
func (o *Orchestrator) clearExactLinearConversationConflict(artifactID string, events []acf.Event) {
	if o.cfg.ConflictStore == nil || len(events) < 2 {
		return
	}
	snapshot, err := o.cfg.ConflictStore.Get(artifactID)
	if err != nil || snapshot.Kind != acf.KindConversation || len(snapshot.Heads) != 2 {
		return
	}
	for index := 1; index < len(events); index++ {
		parent, child := events[index-1], events[index]
		if !linearConversationDelta(parent, child) {
			continue
		}
		expected := []conflicts.Head{conflictHeadFromEvent(parent), conflictHeadFromEvent(child)}
		if sameConflictHeadIdentities(snapshot.Heads, expected) {
			_, _ = o.cfg.ConflictStore.ClearIf(snapshot)
			return
		}
	}
}

// isAuthenticatedAdjacentAssistantCorrection validates the exact three-event
// repair chain and, when present, compare-deletes its exact conflict sidecar.
// It returns true only when latest is a valid correction; the caller then must
// not record a fresh conflict regardless of whether a sidecar existed.
func (o *Orchestrator) isAuthenticatedAdjacentAssistantCorrection(
	primary adapter.Adapter,
	artifactID string,
	events []acf.Event,
) bool {
	if primary == nil || primary.Name() != "codex" || len(events) < adjacentAssistantConflictChainEvents {
		return false
	}
	dirty := events[len(events)-adjacentAssistantConflictChainEvents]
	delta := events[len(events)-2]
	correction := events[len(events)-1]
	for _, event := range []acf.Event{dirty, delta, correction} {
		computed, err := acf.ComputeHash(event)
		if err != nil || computed != event.Hash {
			return false
		}
	}
	if !eventHasTag(correction, acf.LegacyAdjacentAssistantEchoRepairEventTag) ||
		dirty.Provenance.SourceAgent != "codex" ||
		delta.Provenance.SourceAgent != "claude-code" ||
		correction.Provenance.SourceAgent != "codex" ||
		dirty.Provenance.DeviceID == "" ||
		delta.Provenance.DeviceID != dirty.Provenance.DeviceID ||
		// The dirty Codex/Claude pair came from one peer, but the generated
		// Codex mirror that proves and writes the correction belongs to this
		// receiving device. Requiring the correction to impersonate the peer
		// leaves the exact received conflict permanently propagation-blocking.
		correction.Provenance.DeviceID == "" ||
		normalizeBranchName(dirty.Branch) != acf.MainBranch ||
		normalizeBranchName(delta.Branch) != acf.MainBranch ||
		normalizeBranchName(correction.Branch) != acf.MainBranch ||
		delta.ParentHash != dirty.Hash || correction.ParentHash != delta.Hash {
		return false
	}
	dirtyPayload, dirtyErr := acf.DecodeConversationPayload(dirty)
	deltaPayload, deltaErr := acf.DecodeConversationPayload(delta)
	correctionPayload, correctionErr := acf.DecodeConversationPayload(correction)
	if dirtyErr != nil || deltaErr != nil || correctionErr != nil ||
		dirtyPayload.Format != acf.ConversationFormatV1 ||
		deltaPayload.Format != acf.ConversationDeltaFormatV1 ||
		correctionPayload.Format != acf.ConversationFormatV1 ||
		dirtyPayload.Content != "" || deltaPayload.Content != "" || correctionPayload.Content != "" ||
		!acf.IsLegacyAdjacentAssistantEchoConflictDelta(
			correctionPayload.Events, dirtyPayload.Events, deltaPayload.Events,
		) {
		return false
	}
	expectedAttachments := append(
		append([]acf.Attachment(nil), dirtyPayload.Attachments...),
		deltaPayload.Attachments...,
	)
	if !reflect.DeepEqual(correctionPayload.Attachments, expectedAttachments) {
		return false
	}

	if o.cfg.ConflictStore == nil {
		return true
	}
	snapshot, err := o.cfg.ConflictStore.Get(artifactID)
	if err != nil {
		// ErrNotRecorded means there is nothing to clear. Any other error is a
		// corrupt/unreadable sidecar and deliberately remains propagation-blocking.
		return true
	}
	expected := []conflicts.Head{
		conflictHeadFromEvent(dirty),
		conflictHeadFromEvent(delta),
	}
	if snapshot.ArtifactID != artifactID || snapshot.Kind != acf.KindConversation ||
		!sameConflictHeadIdentities(snapshot.Heads, expected) {
		return true
	}
	_, _ = o.cfg.ConflictStore.ClearIf(snapshot)
	return true
}

func sameConflictHeadIdentities(a, b []conflicts.Head) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].SourceAgent != b[i].SourceAgent ||
			a[i].EventID != b[i].EventID ||
			a[i].ContentSHA256 != b[i].ContentSHA256 {
			return false
		}
	}
	return true
}

// inUnresolvedConflict reports whether artifactID currently has a recorded,
// not-yet-resolved divergence in the ConflictStore. A conflict file existing
// IS the unresolved state (resolution Clears the file; ADR-0038). Robust
// across restarts: a conflict recorded on a prior run still blocks propagation
// on this run even though maybeRecordConflict did not fire this cycle. Returns
// false when no ConflictStore is wired or no conflict is recorded.
func (o *Orchestrator) inUnresolvedConflict(artifactID string) bool {
	if o.cfg.ConflictStore == nil {
		return false
	}
	_, err := o.cfg.ConflictStore.Get(artifactID)
	if err == nil {
		return true
	}
	if errors.Is(err, conflicts.ErrNotRecorded) {
		return false
	}
	// Fail safe: a read/parse error (e.g. a corrupt conflict file) must not
	// silently allow propagation of a possibly-divergent head.
	return true
}

func (o *Orchestrator) autoResolveEquivalentConflict(kind acf.Kind, artifactID string, prev, latest acf.Event) {
	winner := latest
	if prev.Timestamp.After(latest.Timestamp) {
		winner = prev
	}
	if winner.EventID == latest.EventID {
		return
	}
	art, err := o.cfg.Store.ReadArtifact(kind, artifactID)
	if err != nil {
		return
	}
	_ = o.cfg.Store.AppendEvent(kind, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeResolution,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{SourceAgent: "aplexica:auto-resolve"},
		Payload:    winner.Payload,
		ParentHash: art.HeadEventHash,
	})
}

// MaybeRecordConflictForTest exposes the internal maybeRecordConflict helper
// to package-external tests. Production callers should not use this — the
// orchestrator handles conflict detection internally during fan-out. Returns
// the recordedConflict signal (true only when an unresolved divergence was
// written to the ConflictStore).
func (o *Orchestrator) MaybeRecordConflictForTest(primary adapter.Adapter, artifactID string) bool {
	return o.maybeRecordConflict(primary, artifactID)
}

func conflictHeadFromEvent(e acf.Event) conflicts.Head {
	preview := string(e.Payload)
	if len(preview) > 200 {
		preview = preview[:200] + "…"
	}
	// FullPayload preserves the complete payload bytes so a head whose event is
	// NOT in the local canonical log (a remote inbound conflict head) can still
	// be resolved and analyzed. The remote event is never appended locally, so
	// the conflict sidecar is the only place this content survives (B3). Copy
	// the bytes so a later mutation of e.Payload cannot corrupt the record.
	var full json.RawMessage
	if len(e.Payload) > 0 {
		full = append(json.RawMessage(nil), e.Payload...)
	}
	return conflicts.Head{
		SourceAgent:    e.Provenance.SourceAgent,
		EventID:        e.EventID,
		ContentSHA256:  e.Hash,
		AbsTimestamp:   float64(e.Timestamp.Unix()) + float64(e.Timestamp.Nanosecond())/1e9,
		PayloadPreview: preview,
		FullPayload:    full,
	}
}

// primaryImport selects the source adapter for a freshly-detected
// native file and invokes Import on JUST that one adapter — no side
// effects on the other adapters' state (BRD-02 §5.4 #5 recursion-
// guard correctness).
//
// Selection algorithm (v0.81.0):
//
//  1. Find every adapter whose Capabilities().BasenameToKind contains
//     the watched basename. These are the candidate handlers.
//  2. Among candidates, prefer those whose NativePath would PRODUCE
//     the watched basename for a synthetic artifact of the dispatched
//     kind. These are "primary" claimants; the rest are "alias"
//     claimants.
//  3. Pick the alphabetically-first PRIMARY claimant if any exist;
//     otherwise the alphabetically-first ALIAS claimant.
//  4. Call Import on the picked adapter only.
//
// Pre-v0.81.0 (after v0.78.0's source-picker bug) called Import on
// every candidate, each writing an event. That broke the 5-adapter
// recursion-guard test (5 events from one user write).
func (o *Orchestrator) primaryImport(ctx context.Context, path string) (adapter.Adapter, []string, bool) {
	primary, ids, committed, _ := o.primaryImportWithDisposition(ctx, path)
	return primary, ids, committed
}

func (o *Orchestrator) primaryImportWithDisposition(ctx context.Context, path string) (adapter.Adapter, []string, bool, bool) {
	base := filepath.Base(path)

	// Path ownership beats a basename coincidence. When the file lives inside
	// some adapter's OWN watched root (RootsByAdapter), only that owner may
	// import it — a foreign adapter that merely shares the basename (e.g.
	// hermes' MEMORY.md vs Claude Code's auto-memory
	// ~/.claude/projects/<cwd>/memory/MEMORY.md) must not claim a file in
	// another agent's folder. When no adapter owns the path (project/watched-
	// dir files), owners is empty and selection is unrestricted as before.
	owners := o.pathOwners(path)

	type candidate struct {
		ad        adapter.Adapter
		kind      acf.Kind
		isPrimary bool
	}
	var primary []candidate
	var alias []candidate
	ownerDeferred := false

	for _, ad := range o.cfg.Adapters {
		// Ownership gate precedes availability probing so an unrelated absent
		// runtime cannot make this owned path look deferred.
		if len(owners) > 0 && !owners[ad.Name()] {
			continue
		}
		caps := ad.Capabilities()
		kind, claimsBasename := caps.BasenameToKind[base]
		if !o.runtimeAdapterAvailable(ad) {
			if len(owners) > 0 || claimsBasename {
				ownerDeferred = true
			}
			continue
		}
		if _, blocked := o.adapterBlocked(ad.Name()); blocked {
			if len(owners) > 0 || claimsBasename {
				ownerDeferred = true
			}
			continue
		}
		if !claimsBasename {
			// Adapter doesn't declare this basename. Note: pre-v0.79.0
			// adapters had no BasenameToKind; for those we fall back
			// to the legacy "try Import and see" behavior at the end.
			continue
		}
		// Probe NativePath with a synthetic artifact carrying the
		// basename as Name. For non-global scopes, we pass a placeholder
		// contextDir so adapters that gate on contextDir != "" don't
		// reject the probe.
		probeScope := acf.ScopeGlobal
		// Use the dispatched kind for the probe.
		probeArt := acf.Artifact{Kind: kind, Scope: probeScope, Name: base}
		nativeOut, supports, _ := ad.NativePath(probeArt, "/probe")
		isPrimary := supports && filepath.Base(nativeOut) == base
		c := candidate{ad: ad, kind: kind, isPrimary: isPrimary}
		if isPrimary {
			primary = append(primary, c)
		} else {
			alias = append(alias, c)
		}
	}

	var winner adapter.Adapter
	switch {
	case len(primary) > 0:
		winner = primary[0].ad
	case len(alias) > 0:
		winner = alias[0].ad
	}

	if winner != nil {
		ids, err := winner.Import(ctx, o.cfg.Store, path)
		if err == nil {
			o.recordAdapterSuccess(winner.Name())
			if o.cfg.Quarantine != nil {
				o.cfg.Quarantine.RecordSuccess(winner.Name())
			}
			return winner, ids, true, ownerDeferred
		}
		// Capabilities-declared dispatch said this adapter handles the
		// basename, but Import rejected it (e.g. parse failure on a claimed or
		// still-being-written native file). Surface the failure for status, but
		// do not quarantine the whole adapter: one corrupt historical transcript
		// must not stop future live sessions from syncing.
		o.recordAdapterError(winner.Name(), err)
	}

	// Fallback path: the watched basename didn't match any adapter's
	// declared BasenameToKind. Most commonly this is an extension-
	// based dispatch (e.g. claudecode's *.jsonl conversations, codex's
	// *.toml tools, hermes's *.db SQLite). Try each adapter's Import
	// in alphabetical order; the first success wins. This preserves
	// the pre-v0.81.0 behavior for these cases AND avoids the
	// multi-event regression because extension-based dispatch is
	// historically one-adapter-per-extension in V1.
	//
	// If a future V1 extension becomes shared across adapters (e.g.
	// both claudecode and codex claim *.jsonl), the right fix is to
	// declare BasenameExtensions on Capabilities alongside
	// BasenameToKind; v0.81.0 ships only the basename half.
	for _, ad := range o.adaptersByPathOwnership(path) {
		// Same ownership gate as the basename branch: a file inside another
		// adapter's root must not be smuggled into a non-owner via the
		// extension/legacy Import probe (hermes.Import would happily accept a
		// MEMORY.md that lives under ~/.claude). When nothing owns the path,
		// owners is empty and every adapter is tried as before.
		if len(owners) > 0 && !owners[ad.Name()] {
			continue
		}
		if !o.runtimeAdapterAvailable(ad) {
			if len(owners) > 0 {
				ownerDeferred = true
			}
			continue
		}
		if _, blocked := o.adapterBlocked(ad.Name()); blocked {
			if len(owners) > 0 {
				ownerDeferred = true
			}
			continue
		}
		ids, err := ad.Import(ctx, o.cfg.Store, path)
		if err == nil {
			o.recordAdapterSuccess(ad.Name())
			if o.cfg.Quarantine != nil {
				o.cfg.Quarantine.RecordSuccess(ad.Name())
			}
			return ad, ids, true, ownerDeferred
		}
		// A "not handled" sentinel is a benign probe-miss — this adapter
		// doesn't own/recognize the file. Any other import error means the
		// adapter claimed-and-failed this one file; record it for status, but do
		// not quarantine the whole adapter. Agent histories often contain old,
		// truncated, or actively-written session files, and quarantining on
		// import would strand unrelated future edits until restart/window expiry.
		if errors.Is(err, adapter.ErrNotHandled) {
			continue
		}
		o.recordAdapterError(ad.Name(), err)
	}
	return nil, nil, false, ownerDeferred
}

// adaptersByPathOwnership returns o.cfg.Adapters reordered so that adapters
// whose native roots (RootsByAdapter) CONTAIN path come first, preserving the
// existing alphabetical order within each group. This breaks extension-dispatch
// ties by ownership: a *.jsonl under ~/.codex/sessions/ is imported by codex
// even though claudecode sorts first and also claims *.jsonl. With no
// RootsByAdapter configured it returns the adapters unchanged.
func (o *Orchestrator) adaptersByPathOwnership(path string) []adapter.Adapter {
	owned := o.pathOwners(path)
	if len(owned) == 0 {
		return o.cfg.Adapters
	}
	owners := make([]adapter.Adapter, 0, len(o.cfg.Adapters))
	others := make([]adapter.Adapter, 0, len(o.cfg.Adapters))
	for _, ad := range o.cfg.Adapters {
		if owned[ad.Name()] {
			owners = append(owners, ad)
		} else {
			others = append(others, ad)
		}
	}
	return append(owners, others...)
}

// pathOwners returns the set of adapter names whose native roots
// (RootsByAdapter) CONTAIN path. A root "owns" a path when the absolute path
// is under "<root>/". Empty when no adapter owns the path (e.g. project files
// under the orchestrator's watched Dir, or when RootsByAdapter is unset) — in
// which case callers impose no ownership restriction and preserve the legacy
// basename/alphabetical selection.
func (o *Orchestrator) pathOwners(path string) map[string]bool {
	if len(o.cfg.RootsByAdapter) == 0 {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	owners := map[string]bool{}
	for name, roots := range o.cfg.RootsByAdapter {
		for _, r := range roots {
			if r == "" {
				continue
			}
			// Exact match covers single-file watch entries (WatchFiles,
			// e.g. ~/.claude.json) registered as ownership roots.
			if abs == r {
				owners[name] = true
				break
			}
			prefix := strings.TrimRight(r, string(filepath.Separator)) + string(filepath.Separator)
			if strings.HasPrefix(abs, prefix) {
				owners[name] = true
				break
			}
		}
	}
	return owners
}

// fanOut propagates each newly-imported artifact ID to every adapter OTHER
// than the primary. Deduplicates writes to the same path across multiple
// target adapters (e.g. codex + kilo both claim AGENTS.md; only one export
// runs per path). Marks each destination path in the recursion guard
// BEFORE writing so the resulting watcher event is suppressed.
// includePrimary (the trailing bool) controls whether the `primary` (source)
// adapter is itself a fan-out TARGET. For LOCAL fan-out it is false: the source
// agent already holds the artifact in its native file, so re-exporting to it is
// redundant (and the file that triggered the import must not be rewritten). For
// INBOUND materialization (materializeInbound) it is TRUE: the "source" agent
// ran on a REMOTE device, so the local same-named agent is a distinct instance
// that has NOTHING yet — it must receive the artifact (e.g. a Mac claude-code
// conversation must land in this device's ~/.claude/projects so it shows in
// `claude /resume`). The recursion guard suppresses the resulting watcher event,
// and forwardCommitted skips the remote-origin re-import, so no loop results.
// fanOut materializes the given artifacts into every enabled+allowed target
// adapter. convBackfillAllow, when non-nil, additionally gates CONVERSATION
// materialization per target agent — the backfill pass passes a closure that
// caps each agent at its most-recent-N; live callers pass nil (uncapped).
func (o *Orchestrator) fanOut(ctx context.Context, primary adapter.Adapter, ids []string, contextDir, sourcePath string, includePrimary bool, convBackfillAllow func(targetAgent string) bool) {
	o.fanOutWithOptions(ctx, primary, ids, contextDir, sourcePath, includePrimary, fanOutOptions{targetAllow: convBackfillAllow})
}

type fanOutOptions struct {
	// targetAllow and targets are evaluated before runtime discovery or safety
	// hooks, so a targeted catch-up cannot touch an unrelated adapter.
	targetAllow func(targetAgent string) bool
	targets     map[string]struct{}
	// originAgent overrides provenance/rule attribution without inventing an
	// installed adapter when retained history no longer names the source. nil
	// preserves the ordinary primary-adapter origin; a pointer to "" means the
	// source is explicitly unknown.
	originAgent *string
	// flushLarge writes conversations over largeMaterializeThreshold inline
	// instead of arming the one-minute coalescing timers. The timers exist to
	// absorb the live per-turn import path's churn; a deliberate bulk pass
	// (the forced backfill) is already sequential and background, and parking
	// its large conversations would fire every deferred replay+transcode
	// simultaneously one minute after the loop finishes. Unlike strict, this
	// does NOT change failure handling: a declined write still enters the
	// ordinary deferral retry queue.
	flushLarge bool
	// mirrorsOnly plans only additional app-managed worktree destinations. The
	// adapter's canonical NativePath is still resolved because it is the safe
	// base used by NativeMirrorPaths, but it is never written.
	mirrorsOnly bool
	// strict is reserved for the post-cloud-ACK durable inbound finalizer. It
	// preserves policy gates, but a planned eligible write that fails or is
	// safety-deferred makes the phase retryable instead of silently complete.
	strict bool
}

func (o *Orchestrator) fanOutWithOptions(ctx context.Context, primary adapter.Adapter, ids []string, contextDir, sourcePath string, includePrimary bool, options fanOutOptions) error {
	strictFailed := false
	// strictErr keeps the FIRST non-nil cause a strict phase produced. Before
	// this, the funnel returned the bare sentinel and every typed cause built
	// below — including the conversation adapter's decline reason — was
	// destroyed one frame above the code that constructed it, so the retry loop
	// (the only thing still running for a stuck artifact) could never refresh
	// its classification. Callers with nothing to attach still pass nil and
	// still get exactly the bare sentinel.
	var strictErr error
	markStrictFailure := func(cause error) {
		if !options.strict {
			return
		}
		strictFailed = true
		if cause == nil {
			return
		}
		// Classify BEFORE recording. A withheld cause never reached the target —
		// a closed policy gate, a shutdown — and the deferral drain reads that
		// marker to pace the entry without charging it an attempt. Keeping the
		// FIRST cause unconditionally would let one withheld plan mask a real
		// refusal from a later plan in the same pass, and the entry that most
		// needs its budget spent would never spend it. Both still make the
		// strict phase retryable: a durable inbound finalize that wrote nothing
		// must be retried, never acknowledged.
		if strictErr == nil ||
			(deferredMaterializationWithheld(strictErr) && !deferredMaterializationWithheld(cause)) {
			strictErr = cause
		}
	}
	originAgent := ""
	if primary != nil {
		originAgent = primary.Name()
	}
	if options.originAgent != nil {
		originAgent = *options.originAgent
	}
	// The adapter object may be only a dispatch fallback when the real source
	// harness is absent. Prefer explicit provenance for source exclusion, but
	// retain the physical fallback for an explicitly unknown origin so a local
	// conversation whose provenance expired is never synthesized back into its
	// own shared CLI/Desktop session store.
	sourceAdapterName := originAgent
	if sourceAdapterName == "" && primary != nil {
		sourceAdapterName = primary.Name()
	}
	// BRD-03 §4.6 / §10 OQ-03.2: fanOut is the single chokepoint for cross-agent
	// propagation, so the unresolved-divergence gate lives here. Every caller —
	// the live import path, per-project re-fanout, RefanOutAll, conversation
	// backfill, and inbound materialization — is therefore covered, and no path
	// can leak a divergent head. An artifact with an open conflict stays withheld
	// until the user resolves it (which Clears the conflict file); its event
	// remains committed to the local log meanwhile.
	if o.cfg.ConflictStore != nil {
		gated := make([]string, 0, len(ids))
		for _, id := range ids {
			if o.inUnresolvedConflict(id) {
				continue
			}
			gated = append(gated, id)
		}
		if len(gated) == 0 {
			return nil
		}
		ids = gated
	}

	// Build (destPath -> adapter) plan, deduped by destPath.
	type plan struct {
		ad         adapter.Adapter
		art        acf.Artifact
		dest       string
		mirror     bool
		sourceHash string
	}
	plans := map[string]plan{}

	// Conversations don't materialize as native session files cross-agent
	// (schemas differ), so they're rendered into a read-only markdown
	// transcript under each target's ConversationDocDir. Keyed by dest path,
	// deduped like plans; executed in a separate pass below.
	convDocPlans := map[string]convDocPlan{}

	// Higher-fidelity path: adapters that can transcode a conversation into
	// their OWN native session store (e.g. Claude Code → ~/.claude/projects/…
	// so it appears in /resume) get a session plan instead of a markdown doc.
	var convSessionPlans []convSessionPlan

	// coTargets are enabled+allowed adapters whose native destination for an
	// artifact is ALREADY covered without a separate Export — either it equals
	// the source path (the adapter reads the source file directly) or another
	// adapter already writes that exact path (shared-file formats, e.g. codex
	// and kilo both map memory to <project>/AGENTS.md). They hold the content
	// in their native location, so they're credited as SyncedAgents in a pass
	// after the writes below even though no Export ran for them. Without this,
	// coverage surfaces (the per-project memory view, per-agent sync state)
	// under-report agents that share an output file with the dedup winner —
	// which is why memories synced via a shared AGENTS.md never showed up as
	// "synced into kilo".
	type coTarget struct {
		artifactID string
		kind       acf.Kind
		name       string
		dest       string // shared destination (point B); "" when it is the source file (point A)
	}
	var coTargets []coTarget

	for _, id := range ids {
		art, found := o.findArtifact(id)
		if !found {
			markStrictFailure(nil)
			continue
		}
		// Project-scoped artifacts fan out to their PROJECT folder, not the
		// triggering file's directory — the trigger may be an auto-memory file
		// (~/.claude/projects/<cwd>/memory/) far from the project folder. This
		// mirrors RefanOutByProject/RefanOutAll, which already set contextDir =
		// art.Project.Path for project scope. Computed per-artifact because
		// fanOut processes multiple ids, each potentially a different scope.
		artCtxDir := contextDir
		if art.Scope == acf.ScopeProject && art.Project != nil && art.Project.Path != "" {
			artCtxDir = art.Project.Path
		}
		// v0.57.0 stage-and-wait gate (BRD-02 §4.13 / FR-02.38):
		// project-scope artifacts whose Project.ID isn't registered
		// in the local project registry get parked in the canonical
		// store but are NOT fanned out to other adapters. The user
		// materializes them by running `aplexica project link`,
		// which adds the registry entry and (v0.58.0) triggers a
		// re-fanout pass.
		//
		// Nil registry = pre-v0.57.0 behavior (every project artifact
		// fans out regardless). Nil Project = artifact predates
		// v0.56.0's InferProject wiring and we have no project info
		// to gate on — treat as fan-out-eligible (registry-bypass
		// for legacy artifacts).
		if o.cfg.ProjectRegistry != nil && art.Scope == acf.ScopeProject && art.Project != nil {
			if _, known := o.cfg.ProjectRegistry.Get(art.Project.ID); !known {
				continue
			}
		}
		// FR-03.3 await-config gate: sync enablement is bidirectional for
		// LOCAL cross-agent fan-out. A disabled local source may still be
		// imported into the canonical store for visibility, but it must not feed
		// any target agent until the user enables that source too.
		//
		// includePrimary=true is the inbound cloud materialization path. There,
		// primary names the REMOTE origin agent, which may not be installed or
		// enabled on this receiving device at all (e.g. Codex on a Mac syncing
		// into Claude Code-only Windows). For inbound, target gates below still
		// apply; the remote source gate must not suppress materialization.
		if !includePrimary {
			if g := o.syncGate(); g != nil && !g.Enabled(originAgent) {
				continue
			}
			if _, blocked := o.adapterBlocked(originAgent); blocked {
				continue
			}
		}
		// Read the append-order head once to determine the current payload
		// format and the event hash for CausedBy propagation. Most import
		// commits end with a payload-bearing create/update event, so a tail read
		// avoids replaying huge conversation logs on every native edit.
		var sourceHash string
		var headBranch string
		var format string
		var hasFormat bool
		if head, ok, eerr := o.cfg.Store.LastEvent(art.Kind, art.ArtifactID); eerr == nil && ok {
			sourceHash = head.Hash
			headBranch = head.Branch
			format, hasFormat = acf.LatestEventFormat([]acf.Event{head})
		} else if eerr != nil {
			markStrictFailure(eerr)
			continue
		}
		if !hasFormat {
			events, eerr := o.cfg.Store.ReadEvents(art.Kind, art.ArtifactID)
			if eerr != nil {
				markStrictFailure(eerr)
				continue
			}
			format, hasFormat = acf.LatestEventFormat(events)
		}
		// v0.104.0 (FR-05.5/6/7): consult the rules engine for this
		// artifact ONCE and gate adapter selection by AllowedAgents. We
		// also apply tag-assigning rules (§5.5) so any tag-mutation a
		// rule contributes lands on the artifact's persistent Tags
		// slice — this is the FR-05.4 "syncable change" hook (the
		// orchestrator's next fan-out cycle will pick up the new tag).
		var ruleAllowed map[string]struct{}
		var ruleSkillModeStrict bool
		if eng := o.rulesEngine(); eng != nil {
			input := ruleInputFor(art, originAgent, headBranch)
			adapterNames := make([]string, 0, len(o.cfg.Adapters))
			for _, ad := range o.cfg.Adapters {
				adapterNames = append(adapterNames, ad.Name())
			}
			// Use the mutex-captured `eng` (from o.rulesEngine() above),
			// NOT the raw o.cfg.RulesEngine field — SetRulesEngine may swap
			// the field concurrently (rule hot-reload), so reading it here
			// unguarded is a data race + nil-deref window.
			decision := eng.Evaluate(input, syncrules.EvaluateOpts{
				InstalledAgents: adapterNames,
			})
			ruleAllowed = map[string]struct{}{}
			for _, n := range decision.AllowedAgents {
				ruleAllowed[n] = struct{}{}
			}
			ruleSkillModeStrict = decision.SkillMode == syncrules.SkillModeStrict
			// Tag-assigning rules: persist contributed tags onto the
			// artifact so subsequent reads see them and downstream
			// adapters get them via the fan-out export. We compare
			// existing tags to decision.AssignedTags and write only
			// when the set actually changed (avoid spurious mtime).
			if len(decision.AssignedTags) > 0 {
				existing := map[string]struct{}{}
				for _, t := range art.Tags {
					existing[t] = struct{}{}
				}
				added := false
				for _, t := range decision.AssignedTags {
					if _, ok := existing[t]; !ok {
						art.Tags = append(art.Tags, t)
						existing[t] = struct{}{}
						added = true
					}
				}
				if added {
					_ = o.cfg.Store.WriteArtifact(art)
				}
			}
		}
		// Folder-local fan-out (project scope): a registered "local" project
		// syncs ONLY to the agents observed working in that folder (Entry.Agents).
		// Empty Agents, a non-local entry, or an unregistered/global artifact
		// imposes no restriction (projectAgents stays nil).
		var projectAgents map[string]bool
		if o.cfg.ProjectRegistry != nil && art.Scope == acf.ScopeProject && art.Project != nil {
			if e, ok := o.cfg.ProjectRegistry.Get(art.Project.ID); ok && e.EffectiveScope() == "local" && len(e.Agents) > 0 {
				projectAgents = make(map[string]bool, len(e.Agents))
				for _, n := range e.Agents {
					projectAgents[n] = true
				}
			}
		}

		// v0.105.0 (FR-05.10): if the rules engine now excludes an
		// adapter that previously received this artifact, mark it as
		// orphaned. The daemon does NOT delete from the agent's
		// native storage; the user runs `aplexica orphans clean` to
		// act explicitly.
		if ruleAllowed != nil && art.Kind != acf.KindConversation {
			for _, prior := range art.SyncedAgents {
				if options.targets != nil {
					if _, requested := options.targets[prior]; !requested {
						continue
					}
				}
				if _, stillAllowed := ruleAllowed[prior]; stillAllowed {
					continue
				}
				if prior == originAgent {
					continue
				}
				_ = o.cfg.Store.MarkOrphan(art.Kind, art.ArtifactID, prior, "")
			}
		}

		for _, ad := range o.cfg.Adapters {
			// Targeted runtime and conversation catch-up must filter before
			// discovery and safety hooks so an unrelated adapter is untouched.
			if options.targets != nil {
				if _, requested := options.targets[ad.Name()]; !requested {
					continue
				}
			}
			if options.targetAllow != nil && !options.targetAllow(ad.Name()) {
				continue
			}
			if !includePrimary && ad.Name() == sourceAdapterName {
				// A logical adapter may span more than one native surface.
				// Let its NativeMirrorTarget participate for non-conversation
				// artifacts so an edit from one surface (for example a Claude
				// Desktop worktree) reaches the adapter's other surfaces. The
				// primary NativePath/source equality check below still prevents
				// rewriting the triggering file. A conversation imported from its
				// original native SourcePath stays excluded to avoid an echo. A
				// continuation imported from an Aplexica-generated session has a
				// different Artifact.SourcePath and must be rematerialized so the
				// source agent receives the canonical union. Session adapters are
				// responsible for preserving active files or branching safely.
				if art.Kind == acf.KindConversation {
					if filepath.Clean(sourcePath) == filepath.Clean(art.SourcePath) {
						continue
					}
				} else if _, mirrors := ad.(adapter.NativeMirrorTarget); !mirrors {
					continue
				}
			}
			if !o.runtimeAdapterAvailable(ad) {
				o.suppressions.record(ad.Name(), ReasonAdapterNotInstalled, art.ArtifactID, time.Now().UTC())
				continue
			}
			if _, blocked := o.adapterBlocked(ad.Name()); blocked {
				o.suppressions.record(ad.Name(), ReasonAdapterBlockedSafety, art.ArtifactID, time.Now().UTC())
				// Legacy/native fan-out has already committed the artifact to the
				// canonical store. Remember this exact target so the safety-clear
				// transition can finish the native write instead of silently losing
				// it. Durable finalize remains fail-closed and is retried by its ACK
				// protocol; queueing it here could materialize before that ACK.
				//
				// No cause: this is a reversible policy gate, not a target-side
				// refusal, and attaching one here would mislabel it.
				markStrictFailure(nil)
				if !options.strict {
					o.deferMaterialization(ad.Name(), art.ArtifactID, originAgent, includePrimary, options.mirrorsOnly, false)
				}
				continue
			}
			// FR-03.3 await-config gate: skip target agents whose sync the
			// user hasn't enabled. Source gating happened above; the artifact
			// already landed in the canonical store, so this only withholds
			// cross-agent export. Nil SyncGate = no gating (pre-v-next behavior).
			if g := o.syncGate(); g != nil && !g.Enabled(ad.Name()) {
				o.suppressions.record(ad.Name(), ReasonTargetSyncDisabled, art.ArtifactID, time.Now().UTC())
				continue
			}
			// v0.104.0 rules gate: skip adapters the rule engine did not
			// allow. ruleAllowed==nil when the engine is disabled, in
			// which case all adapters are considered.
			if ruleAllowed != nil && art.Kind != acf.KindConversation {
				if _, ok := ruleAllowed[ad.Name()]; !ok {
					// An EMPTY allow-set means the engine matched no rule at
					// all — on a device with no rules.toml that is every
					// artifact.
					// Distinguish it so the operator is told "no rules are
					// configured" instead of "a rule excluded this".
					o.suppressions.record(ad.Name(), o.rulesSuppressionReason(ruleAllowed), art.ArtifactID, time.Now().UTC())
					continue
				}
			}
			// Folder-local scope gate (see projectAgents above).
			if projectAgents != nil && !projectAgents[ad.Name()] {
				o.suppressions.record(ad.Name(), ReasonProjectAgentNotListed, art.ArtifactID, time.Now().UTC())
				continue
			}
			// v0.105.0 (FR-05.16): route.skillMode="strict" means
			// don't fan out a skill artifact to adapters that don't
			// natively support skills (no annotated-document fallback).
			if ruleSkillModeStrict && art.Kind == acf.KindSkill {
				caps := ad.Capabilities()
				if !caps.Artifacts.Skill {
					o.suppressions.record(ad.Name(), ReasonSkillModeStrict, art.ArtifactID, time.Now().UTC())
					continue
				}
			}
			// FR-03.11 pause gate: skip adapters paused via
			// `aplexica sync pause`. Global pause skips every adapter;
			// per-adapter pause skips just this one.
			if o.cfg.PauseStore != nil {
				if paused, _ := o.cfg.PauseStore.IsPaused(ad.Name(), time.Now().UTC()); paused {
					o.suppressions.record(ad.Name(), ReasonPaused, art.ArtifactID, time.Now().UTC())
					continue
				}
			}
			// FR-03.15 quarantine gate.
			if o.cfg.Quarantine != nil && o.cfg.Quarantine.IsQuarantined(ad.Name(), time.Now()) {
				o.suppressions.record(ad.Name(), ReasonQuarantined, art.ArtifactID, time.Now().UTC())
				continue
			}
			// Conversation cross-agent materialization: a conversation can't be
			// written as a native session in another agent (schemas differ), so
			// instead render a deterministic read-only markdown transcript into
			// the target's ConversationDocDir. Reaches here only after every
			// gate above (sync gate, rules, pause, quarantine), so it honors the
			// same rule that enabled "conversation" in match.type.
			if art.Kind == acf.KindConversation {
				branch := selectedBranchForAgent(art, ad.Name())
				if !o.conversationRuleAllowsTarget(art, originAgent, ad.Name(), branch) {
					// A device with no rules.toml
					// denies every conversation here, and before this line
					// existed the target vanished with no log, no metric and
					// no queue entry, after which fanOut returned nil.
					o.suppressions.record(ad.Name(), o.conversationRuleReason(), art.ArtifactID, time.Now().UTC())
					continue
				}
				// Backfill cap: skip this target when the per-agent
				// most-recent-N budget for conversations is exhausted (only
				// set during RefanOutAll's backfill; nil for live fan-out).
				// Prefer a native-session materialization (shows up in the
				// target's own session list, e.g. Claude Code /resume).
				if st, ok := ad.(adapter.ConversationSessionTarget); ok {
					convSessionPlans = append(convSessionPlans, convSessionPlan{
						st: st, name: ad.Name(), art: art, branch: branch, sourceAgent: originAgent,
						includePrimary: includePrimary, mirrorsOnly: options.mirrorsOnly,
					})
					continue
				}
				// Fallback: a read-only markdown transcript.
				if dt, ok := ad.(adapter.ConversationDocTarget); ok {
					dir, ok := dt.ConversationDocDir()
					if !ok {
						continue
					}
					docPath := filepath.Join(dir, conversationDocFilenameForBranch(originAgent, art.ArtifactID, branch))
					if docPath == sourcePath {
						continue
					}
					if _, already := convDocPlans[docPath]; already {
						continue
					}
					convDocPlans[docPath] = convDocPlan{art: art, branch: branch, dest: docPath, sourceAgent: originAgent}
					continue
				}
				// Some agents keep conversations in a shared native database rather
				// than one file per session. Hermes is the current example: its
				// adapter can losslessly Export the canonical conversation into
				// ~/.hermes/state.db, but it is intentionally not a
				// ConversationSessionTarget because there is no standalone session
				// path to return. Let these adapters fall through to the ordinary
				// HandlesFormat + NativePath + Export plan below. This keeps live
				// fan-out synchronous; the five-second hermeswatch inbound poll stays
				// as an idempotent recovery net instead of being the primary path.
			}
			// Format gate: an adapter that doesn't HandlesFormat the
			// artifact's current payload would error on Export anyway
			// (e.g. hermes can't decode a claude-code.session.jsonl).
			// Skip cleanly here instead of triggering a noisy decode
			// failure downstream.
			if hasFormat && !ad.HandlesFormat(art.Kind, format) {
				continue
			}
			dest, supports, err := ad.NativePath(art, artCtxDir)
			if err != nil {
				markStrictFailure(err)
				continue
			}
			if !supports {
				continue
			}
			type destination struct {
				path   string
				mirror bool
			}
			var destinations []destination
			if !options.mirrorsOnly {
				destinations = append(destinations, destination{path: dest})
			}
			if mt, ok := ad.(adapter.NativeMirrorTarget); ok {
				mirrors, merr := mt.NativeMirrorPaths(art, artCtxDir, dest)
				if merr != nil {
					o.recordAdapterError(ad.Name(), merr)
					markStrictFailure(merr)
				} else {
					for _, mirror := range mirrors {
						destinations = append(destinations, destination{path: mirror, mirror: true})
					}
				}
			}
			for _, target := range destinations {
				targetDest := target.path
				if targetDest == "" {
					continue
				}
				if targetDest == sourcePath {
					// Same path as the source — that's one of this logical
					// adapter's own native surfaces; a no-op write would be
					// wasteful. A different adapter that shares the path is a
					// co-target; the primary is already the recorded author and
					// needs no redundant artifact metadata rewrite. Continue so
					// this logical adapter's additional Desktop mirrors are still
					// planned.
					if ad.Name() != sourceAdapterName {
						coTargets = append(coTargets, coTarget{art.ArtifactID, art.Kind, ad.Name(), ""})
					}
					continue
				}
				// First-wins dedup normally keys on destination path: adapters that
				// share one native file read the same bytes, so one write covers all
				// of them. A shared conversation database is different: each
				// artifact is a distinct row set in the SAME state.db and every one
				// must be exported. Include the artifact ID in that plan key while
				// still deduping duplicate adapters for the same artifact + DB.
				planKey := targetDest
				if art.Kind == acf.KindConversation {
					planKey += "\x00conversation\x00" + art.ArtifactID
				}
				if _, already := plans[planKey]; already {
					coTargets = append(coTargets, coTarget{art.ArtifactID, art.Kind, ad.Name(), targetDest})
					continue
				}
				plans[planKey] = plan{
					ad: ad, art: art, dest: targetDest, mirror: target.mirror, sourceHash: sourceHash,
				}
			}
		}
	}

	// writtenDests records destinations a fan-out Export actually wrote this
	// cycle, so point-B co-targets are only credited when the shared file was
	// produced (or already exists, checked below).
	writtenDests := map[string]bool{}
	for _, p := range plans {
		// App-managed worktrees may already contain an independently edited
		// file before this daemon has ever fingerprinted the path. Canonical
		// destinations retain their historical first-contact behavior, while
		// additional native mirrors fail closed: an existing file must be
		// recognized by the adapter as an untouched checkout copy or as bytes
		// Aplexica previously wrote for this artifact.
		if p.mirror && !o.destHashKnown(p.dest) {
			if _, statErr := os.Lstat(p.dest); statErr == nil {
				guard, ok := p.ad.(adapter.NativeMirrorFirstContactGuard)
				if !ok {
					unguarded := fmt.Errorf("native mirror already exists; refusing first write: %s", p.dest)
					o.recordAdapterError(p.ad.Name(), unguarded)
					markStrictFailure(unguarded)
					continue
				}
				safe, guardErr := guard.NativeMirrorFirstContactSafe(o.cfg.Store, p.art, p.dest)
				if guardErr != nil {
					o.recordAdapterError(p.ad.Name(), guardErr)
					markStrictFailure(guardErr)
					continue
				}
				if !safe {
					dirty := fmt.Errorf("native mirror has local changes; refusing overwrite: %s", p.dest)
					o.recordAdapterError(p.ad.Name(), dirty)
					markStrictFailure(dirty)
					continue
				}
			} else if !os.IsNotExist(statErr) {
				inspectErr := fmt.Errorf("inspect native mirror: %w", statErr)
				o.recordAdapterError(p.ad.Name(), inspectErr)
				markStrictFailure(inspectErr)
				continue
			}
		}
		// Read-before-clobber: if the destination changed since the
		// orchestrator last wrote or imported it, an agent-side edit is
		// sitting there whose watcher event hasn't imported yet.
		// Overwriting now would destroy that edit AND the recursion
		// guard would swallow its event as an echo of our own write.
		// Defer this export — the pending import lands first, the
		// conflict detector sees both heads, and a later fan-out cycle
		// re-exports. (Unknown paths — first contact — proceed.)
		if o.destChangedUnderUs(p.dest) {
			// No cause: this is a read-before-clobber deferral awaiting a
			// pending import, not a refusal the target reported.
			markStrictFailure(nil)
			continue
		}
		// Mark in the guard FIRST so a fast OS event delivery doesn't
		// race past.
		o.guard.Mark(p.dest)
		// Stamp the source event's hash onto context so the destination
		// adapter's Import path (via adapter.ImportOpaque, called from
		// inside Export -> Import flows in a future receive-side guard,
		// OR more directly when an adapter writes its own event via
		// ImportOpaque-shaped code) can populate Provenance.CausedBy on
		// the fan-out event. v0.20.0 plumbs the field through; future
		// milestones will use it for cross-process recursion-guard dedup.
		exportCtx := adapter.WithCausedBy(ctx, p.sourceHash)
		if p.mirror {
			exportCtx = adapter.WithNativeMirror(exportCtx)
		}
		if err := p.ad.Export(exportCtx, o.cfg.Store, p.art.ArtifactID, p.dest); err != nil {
			// Log-and-continue. A future milestone may surface this via
			// structured errors; v0.7.0 silently skips failed fan-out
			// (still no infinite loop because the guard is set).
			//
			// v0.51.0 (ADR-0159 Candidate D): capture the redacted
			// error string so AdapterLastErrors() can surface it on
			// the daemon control surface.
			o.recordAdapterError(p.ad.Name(), err)
			markStrictFailure(err)
			// v0.92.0 FR-03.15: record the failure on the quarantine
			// tracker so repeated Export failures within the window
			// trip quarantine.
			if o.cfg.Quarantine != nil {
				o.cfg.Quarantine.RecordFailure(p.ad.Name(), time.Now())
			}
			continue
		}
		// v0.92.0: a successful Export clears the quarantine's failure
		// history (RecordSuccess is idempotent on a clean slate).
		if o.cfg.Quarantine != nil {
			o.cfg.Quarantine.RecordSuccess(p.ad.Name())
		}
		// v0.51.0: successful Export — stamp adapter as touched and
		// clear any stale error string.
		o.recordAdapterSuccess(p.ad.Name())
		writtenDests[p.dest] = true
		// Track what we just wrote so the next fan-out to this path can
		// detect agent-side edits made in between (read-before-clobber).
		o.recordDestHash(p.dest)

		// v0.105.0 (FR-05.10): record this adapter as a known sync
		// target on the artifact so a future rule change that excludes
		// it can be detected as an orphan.
		if cur, err := o.cfg.Store.ReadArtifact(p.art.Kind, p.art.ArtifactID); err == nil {
			seen := false
			for _, n := range cur.SyncedAgents {
				if n == p.ad.Name() {
					seen = true
					break
				}
			}
			if !seen {
				cur.SyncedAgents = append(cur.SyncedAgents, p.ad.Name())
				sortStrings(cur.SyncedAgents)
				_ = o.cfg.Store.WriteArtifact(cur)
			}
			// If this artifact was previously marked orphan in this
			// agent, clearing the orphan is appropriate (the agent is
			// receiving fresh content again).
			_ = o.cfg.Store.ClearOrphan(p.art.ArtifactID, p.ad.Name())
		}
	}

	// Co-target crediting pass (see coTargets above). Credit adapters that
	// share an output file with the dedup winner — or read the source file
	// directly — so per-agent coverage surfaces reflect that they hold the
	// artifact. Point-A co-targets (dest == "") read the source file, which
	// exists by definition. Point-B co-targets are credited only when the
	// shared destination was actually written this cycle OR is already present
	// on disk, so a failed winner Export doesn't over-credit.
	for _, ct := range coTargets {
		if ct.dest != "" && !writtenDests[ct.dest] {
			if _, statErr := os.Stat(ct.dest); statErr != nil {
				continue
			}
		}
		cur, err := o.cfg.Store.ReadArtifact(ct.kind, ct.artifactID)
		if err != nil {
			continue
		}
		seen := false
		for _, n := range cur.SyncedAgents {
			if n == ct.name {
				seen = true
				break
			}
		}
		if !seen {
			cur.SyncedAgents = append(cur.SyncedAgents, ct.name)
			sortStrings(cur.SyncedAgents)
			if werr := o.cfg.Store.WriteArtifact(cur); werr != nil {
				continue
			}
		}
		o.recordAdapterSuccess(ct.name)
		_ = o.cfg.Store.ClearOrphan(ct.artifactID, ct.name)
	}

	// Conversation transcript materialization pass. Render the artifact's head
	// event to markdown and write it under each target's ConversationDocDir.
	// Guard-marked before the write so the resulting watcher event is
	// suppressed (the file lives in a non-watched subdir anyway, and no adapter
	// imports an arbitrary *.md, so this never loops).
	for _, p := range convDocPlans {
		if err := o.materializeConversationDoc(p, options.strict || options.flushLarge); err != nil {
			markStrictFailure(err)
		}
	}

	// Native-session materialization pass: the target adapter transcodes the
	// conversation into its own session store (e.g. Claude Code /resume). The
	// adapter writes the file and returns its path. When the adapter can predict
	// the destination, guard it BEFORE the write; these paths live under watched
	// native roots, and fast watcher delivery can otherwise re-import a remote
	// materialization as a local edit before the post-write guard below lands.
	for _, sp := range convSessionPlans {
		err := o.materializeConversationSession(sp, options.strict || options.flushLarge)
		if err == nil {
			continue
		}
		// markStrictFailure tests deferredMaterializationWithheld(err) before it
		// records anything, so a pass that never reached the adapter cannot
		// displace a genuine refusal as the cause this funnel returns — and the
		// drain, reading that same marker, does not charge it an attempt.
		markStrictFailure(err)
		if !options.strict {
			o.deferMaterialization(sp.name, sp.art.ArtifactID, sp.sourceAgent, sp.includePrimary, sp.mirrorsOnly, true)
		}
	}
	if strictFailed {
		return strictMaterializationFailure(strictErr)
	}
	return nil
}

// strictMaterializationFailure renders a strict phase's captured cause as the
// funnel's return value. ErrInboundNativeMaterialization is the contract three
// packages match with errors.Is, so it is always present: a cause that already
// carries it passes through untouched (keeping errors.As able to recover a
// *ConversationDeclineError), anything else is wrapped, and no cause at all
// yields exactly the bare sentinel this function replaced.
//
// Wrapping is transparent to deferredMaterializationWithheld: a cause carrying
// the withheld marker keeps carrying it, and a target-side decline carries only
// the sentinel, so a refused write is still charged and a policy gate is still
// only paced. Both strict callers pass context.Background(), so no cancellation
// can enter here and silently turn a charged failure into a paced one.
func strictMaterializationFailure(cause error) error {
	if cause == nil {
		return ErrInboundNativeMaterialization
	}
	if errors.Is(cause, ErrInboundNativeMaterialization) {
		return cause
	}
	return fmt.Errorf("%w: %w", ErrInboundNativeMaterialization, cause)
}

// convDocPlan is one planned markdown-transcript materialization: render the
// conversation's head into a read-only transcript under the target's
// ConversationDocDir. Built (deduped by dest path) by fanOut, executed by
// materializeConversationDoc — either inline or from a deferred
// large-artifact flush.
type convDocPlan struct {
	art         acf.Artifact
	branch      string
	dest        string
	sourceAgent string
}

// convSessionPlan is one planned native-session materialization: the target
// adapter transcodes the conversation into its OWN session store (e.g.
// Claude Code → ~/.claude/projects/… so it appears in /resume). Built by
// fanOut, executed by materializeConversationSession — either inline or from
// a deferred large-artifact flush.
type convSessionPlan struct {
	st             adapter.ConversationSessionTarget
	name           string
	art            acf.Artifact
	branch         string
	sourceAgent    string
	includePrimary bool
	mirrorsOnly    bool
}

// materializeConversationDoc executes one markdown-transcript plan.
// flush=false is the live fan-out dispatch, which defers artifacts above
// largeMaterializeThreshold to the per-artifact trailing-edge timer;
// flush=true is that timer's coalesced re-run and always writes.
func (o *Orchestrator) materializeConversationDoc(p convDocPlan, flush bool) error {
	key := largeMaterializeKey(p.art.ArtifactID, p.branch)
	// A flush is already armed for this artifact → fold this plan in and let
	// the timer write once. Checked BEFORE the head read: skipping the
	// re-materialization of a multi-MB head on every dispatch is the point
	// of the coalescing.
	if !flush && o.joinPendingLargeMaterialize(key, func(pend *pendingLargeMaterialize) {
		pend.docs[p.dest] = p
	}) {
		return nil
	}
	// Gate on cheap native-source/remote-log metadata BEFORE reconstructing the
	// materialized head. A long-lived delta conversation can have a 100 KiB
	// tail but a multi-gigabyte history; reading the head merely to discover
	// that it needs debouncing is precisely the CPU and memory spike this path
	// must prevent.
	if !flush && o.conversationExceedsLargeThresholdBeforeReplay(p.art) {
		o.scheduleLargeMaterialize(key, func(pend *pendingLargeMaterialize) {
			pend.docs[p.dest] = p
		})
		return nil
	}
	head, hasHead, herr := conversationHeadForBranch(o.cfg.Store, p.art.ArtifactID, p.branch)
	if herr != nil {
		o.recordAdapterError(p.sourceAgent, herr)
		return herr
	}
	if !hasHead {
		return ErrInboundNativeMaterialization
	}
	// Large artifact on the live path: arm the trailing-edge flush and skip
	// this write (Design rule 8). Deliberately BEFORE guard.Mark — marking
	// belongs to the actual write, not the schedule.
	if !flush && len(head.Payload) > largeMaterializeThreshold {
		o.scheduleLargeMaterialize(key, func(pend *pendingLargeMaterialize) {
			pend.docs[p.dest] = p
		})
		return nil
	}
	return o.writeConversationDoc(p, head)
}

func (o *Orchestrator) writeConversationDoc(p convDocPlan, head acf.Event) error {
	md, rerr := renderConversationMarkdown(p.art, p.sourceAgent, head)
	if rerr != nil {
		o.recordAdapterError(p.sourceAgent, rerr)
		return rerr
	}
	guardToken := o.guard.Mark(p.dest)
	if err := os.MkdirAll(filepath.Dir(p.dest), 0o755); err != nil {
		// Nothing reached p.dest, so the mark guards no write of ours. Leaving
		// it would suppress a real agent-side edit for the whole window.
		o.guard.Unmark(p.dest, guardToken)
		o.recordAdapterError(p.sourceAgent, err)
		return err
	}
	if werr := atomicfile.WriteFile(p.dest, []byte(md), 0o644); werr != nil {
		o.recordAdapterError(p.sourceAgent, werr)
		return werr
	}
	return nil
}

// materializeConversationSession executes one native-session plan. flush has
// the same meaning as in materializeConversationDoc. Guard-marking and
// hot-marking happen at WRITE time inside this method, so a deferred flush
// carries identical recursion-guard semantics to an inline write.
func (o *Orchestrator) materializeConversationSession(sp convSessionPlan, flush bool) error {
	key := largeMaterializeKey(sp.art.ArtifactID, sp.branch)
	if !flush && o.joinPendingLargeMaterialize(key, func(pend *pendingLargeMaterialize) {
		pend.sessions[materializeTargetKey(sp.name, sp.branch)] = sp
	}) {
		return nil
	}
	if !flush && o.conversationExceedsLargeThresholdBeforeReplay(sp.art) {
		o.scheduleLargeMaterialize(key, func(pend *pendingLargeMaterialize) {
			pend.sessions[materializeTargetKey(sp.name, sp.branch)] = sp
		})
		return nil
	}
	// Serialize the head-read + native write per artifact: a racing
	// stale fan-out must re-read the head inside the critical section
	// so it can never overwrite a newer materialization.
	unlock := o.lockConversationMaterialize(sp.art.ArtifactID)
	defer unlock()
	head, hasHead, herr := conversationHeadForBranch(o.cfg.Store, sp.art.ArtifactID, sp.branch)
	if herr != nil {
		o.recordAdapterError(sp.name, herr)
		return herr
	}
	if !hasHead {
		return ErrInboundNativeMaterialization
	}
	if !flush && len(head.Payload) > largeMaterializeThreshold {
		o.scheduleLargeMaterialize(key, func(pend *pendingLargeMaterialize) {
			pend.sessions[materializeTargetKey(sp.name, sp.branch)] = sp
		})
		return nil
	}
	return o.writeConversationSession(sp, head)
}

// sessionWriteWitness is a stat-only snapshot of a planned native session
// file. It answers exactly one question — did this materialization pass write
// anything? — cheaply enough to run on every declined attempt, unlike
// fingerprintDest, which hashes the whole file.
type sessionWriteWitness struct {
	exists bool
	size   int64
	mtime  int64
}

func witnessSessionFile(path string) sessionWriteWitness {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return sessionWriteWitness{}
	}
	return sessionWriteWitness{exists: true, size: fi.Size(), mtime: fi.ModTime().UnixNano()}
}

func (w sessionWriteWitness) same(other sessionWriteWitness) bool {
	return w == other
}

func (o *Orchestrator) writeConversationSession(sp convSessionPlan, head acf.Event) error {
	plannedPath := ""
	// Set together with plannedPath's guard mark so a declined pass can prove
	// it wrote nothing and withdraw the mark again.
	guardedPath := ""
	guardToken := uint64(0)
	guardedWitness := sessionWriteWitness{}
	// withdrawUnwrittenGuard releases a pre-write guard mark when the target
	// file is byte-identical to what it was before the attempt. An adapter
	// that declines by design (native session open, ahead, or divergent)
	// otherwise leaves a suppression window over a live path — and because the
	// deferral queue retries the same declining artifact indefinitely, those
	// windows overlap and can swallow the agent-side continuation the adapter
	// was waiting for. Adapters may also decline AFTER writing (a post-write
	// verification that fails), so the witness check is mandatory, not an
	// optimization.
	withdrawUnwrittenGuard := func() {
		if guardedPath == "" {
			return
		}
		if !witnessSessionFile(guardedPath).same(guardedWitness) {
			return
		}
		o.guard.Unmark(guardedPath, guardToken)
	}
	// declined builds the typed cause for one decline: it publishes the
	// (redacted) deferral event exactly as before and returns an error that
	// still satisfies errors.Is(err, ErrInboundNativeMaterialization) while
	// carrying the adapter's reason out to the deferral layer.
	declined := func(path string, reason adapter.SessionDeclineReason) error {
		cause := newConversationDeclineError(sp.name, sp.art.ArtifactID, sp.branch, reason, path)
		o.publishEvent("conversation.materialize_deferred", map[string]any{
			"artifact_id": sp.art.ArtifactID,
			"agent":       sp.name,
			"branch":      sp.branch,
			"path":        cause.Path,
			"reason":      string(cause.Reason),
			"retry_class": string(cause.RetryClass),
		})
		return cause
	}
	if planner, ok := sp.st.(adapter.ConversationSessionPathTarget); ok {
		path, supported, reason, perr := planConversationSessionPath(planner, sp.art, head, sp.sourceAgent)
		if perr != nil {
			o.recordAdapterError(sp.name, perr)
			return perr
		}
		plannedPath = path
		if !supported {
			if path == "" {
				return nil
			}
			return declined(path, reason)
		}
		if path != "" {
			// Read-before-clobber (same contract as the generic plan pass
			// above): if the planned session file changed since we last
			// wrote or imported it, an agent-side continuation is sitting
			// there whose import hasn't landed yet. Overwriting now would
			// destroy those turns before they ever reach the canonical
			// thread. Defer — the pending import re-records the dest
			// fingerprint and the next fan-out cycle re-materializes the
			// merged head. Keep the path hot so that import happens on the
			// next 500ms tick rather than waiting for a watcher event.
			if o.destChangedUnderUs(path) {
				o.markClaudeHotSession(path)
				// The orchestrator, not the adapter, made this call, and it made
				// it because a write is in flight — the definition of a race.
				return declined(path, adapter.SessionDeclineRace)
			}
			guardedWitness = witnessSessionFile(path)
			guardToken = o.guard.Mark(path)
			guardedPath = path
			o.markClaudeHotSession(path)
		}
	}
	path, ok, reason, werr := materializeConversationSessionInto(sp.st, sp.art, head, sp.sourceAgent)
	if werr != nil {
		withdrawUnwrittenGuard()
		o.recordAdapterError(sp.name, werr)
		return werr
	}
	if !ok {
		withdrawUnwrittenGuard()
		// A non-empty stable path means this payload is supported but the
		// adapter declined the write because its native file was changing,
		// ahead, or divergent. Keep it retryable. Empty-path supports=false is
		// the adapter contract's permanent payload/runtime opt-out.
		if path != "" || plannedPath != "" {
			deferredPath := path
			if deferredPath == "" {
				deferredPath = plannedPath
			}
			return declined(deferredPath, reason)
		}
		return nil
	}
	if path != guardedPath {
		// The adapter wrote somewhere other than the planned path (or declined
		// the plan and published elsewhere); the planned path's mark guards
		// nothing.
		withdrawUnwrittenGuard()
	}
	if path != "" {
		o.guard.Mark(path)
		o.recordDestHash(path)
		o.markClaudeHotSession(path)
	}
	o.recordAdapterSuccess(sp.name)
	return nil
}

// pendingLargeMaterialize is one artifact's armed trailing-edge flush: the
// debounce timer plus the newest materialization plans to re-run when it
// fires. Plans are keyed — target adapter name for sessions, destination
// path for docs — so a burst of dispatches folds into one write per
// destination, with the freshest plan metadata winning (the head itself is
// re-read at flush time, so content freshness never depends on the merge).
type pendingLargeMaterialize struct {
	timer    *time.Timer
	sessions map[string]convSessionPlan // key: target adapter name + branch
	docs     map[string]convDocPlan     // key: destination path
}

func largeMaterializeKey(artifactID, branch string) string {
	return artifactID + "\x00" + ruleBranchName(branch)
}

func materializeTargetKey(agent, branch string) string {
	return agent + "\x00" + ruleBranchName(branch)
}

func (o *Orchestrator) conversationExceedsLargeThresholdBeforeReplay(art acf.Artifact) bool {
	if o.cfg.Store == nil || largeMaterializeThreshold <= 0 {
		return false
	}
	// Native artifacts have the best proxy available for free: their source
	// transcript size. Remote shells have no source path until fan-out, so fall
	// back to append-log size for those. Avoid using log size for ordinary
	// source-less local/test artifacts: many tiny superseded updates can make a
	// large log while the current materialized conversation remains small.
	if art.SourcePath != "" {
		info, err := os.Stat(art.SourcePath)
		return err == nil && !info.IsDir() && info.Size() > int64(largeMaterializeThreshold)
	}
	if art.RemoteOriginDeviceID == "" {
		return false
	}
	size, err := o.cfg.Store.EventLogSize(acf.KindConversation, art.ArtifactID)
	return err == nil && size > int64(largeMaterializeThreshold)
}

// joinPendingLargeMaterialize folds a materialization plan into an already
// armed trailing-edge flush for the artifact, if one exists. Returns true
// when the plan was deferred (the pending flush will re-run it with the
// newest head); false when no flush is armed and the caller must proceed.
func (o *Orchestrator) joinPendingLargeMaterialize(key string, join func(*pendingLargeMaterialize)) bool {
	o.largeMaterializeMu.Lock()
	defer o.largeMaterializeMu.Unlock()
	pend, ok := o.largeMaterializePending[key]
	if !ok {
		return false
	}
	join(pend)
	// True trailing edge: fresh activity postpones the expensive full rewrite.
	// If the callback has already fired, Stop returns false and the in-flight
	// flush wins; the next dispatch can arm a new window after it clears.
	if pend.timer != nil && pend.timer.Stop() {
		pend.timer.Reset(largeMaterializeDebounce)
	}
	return true
}

// scheduleLargeMaterialize arms the artifact's trailing-edge flush timer if
// none is armed and folds the plan in. Idempotent under races: two
// dispatches that both missed joinPendingLargeMaterialize merge into a
// single timer rather than double-arming.
func (o *Orchestrator) scheduleLargeMaterialize(key string, join func(*pendingLargeMaterialize)) {
	o.largeMaterializeMu.Lock()
	defer o.largeMaterializeMu.Unlock()
	pend, ok := o.largeMaterializePending[key]
	if !ok {
		pend = &pendingLargeMaterialize{
			sessions: map[string]convSessionPlan{},
			docs:     map[string]convDocPlan{},
		}
		pend.timer = time.AfterFunc(largeMaterializeDebounce, func() {
			// Registered with the Close-join lifecycle: Close stops pending
			// timers, but a callback already fired keeps writing after the
			// Stop — Close must wait for it, and one armed AFTER Close's
			// timer sweep must not run at all.
			if !o.beginBackground() {
				return
			}
			defer o.endBackground()
			o.flushLargeMaterialize(key)
		})
		if o.largeMaterializePending == nil {
			o.largeMaterializePending = map[string]*pendingLargeMaterialize{}
		}
		o.largeMaterializePending[key] = pend
	}
	join(pend)
}

// flushLargeMaterialize is the debounce timer's callback: clear the entry
// (cleared-on-fire — the next dispatch arms a fresh window), then re-run the
// deferred materializations with flush=true so they write regardless of
// size. Each re-run re-reads the head — under the per-artifact materialize
// lock for sessions — so the flush always writes the NEWEST state, and
// guard/hot-marking runs at write time exactly like an inline write.
func (o *Orchestrator) flushLargeMaterialize(key string) {
	o.largeMaterializeMu.Lock()
	pend, ok := o.largeMaterializePending[key]
	delete(o.largeMaterializePending, key)
	o.largeMaterializeMu.Unlock()
	if !ok {
		return
	}
	// Every plan in this pending entry addresses the same artifact+branch.
	// Read and materialize that head once, then share the immutable event with
	// all markdown and native-session targets. Previously every target replayed
	// and re-encoded the same potentially 100+ MB conversation independently.
	var artifactID, branch, sourceAgent string
	for _, p := range pend.docs {
		artifactID, branch, sourceAgent = p.art.ArtifactID, p.branch, p.sourceAgent
		break
	}
	if artifactID == "" {
		for _, sp := range pend.sessions {
			artifactID, branch, sourceAgent = sp.art.ArtifactID, sp.branch, sp.sourceAgent
			break
		}
	}
	if artifactID == "" {
		return
	}
	unlock := o.lockConversationMaterialize(artifactID)
	defer unlock()
	head, hasHead, err := conversationHeadForBranch(o.cfg.Store, artifactID, branch)
	if err != nil {
		o.recordAdapterError(sourceAgent, err)
		return
	}
	if !hasHead {
		return
	}
	for _, p := range pend.docs {
		o.writeConversationDoc(p, head)
	}
	for _, sp := range pend.sessions {
		if err := o.writeConversationSession(sp, head); err != nil {
			o.deferMaterialization(sp.name, sp.art.ArtifactID, sp.sourceAgent, sp.includePrimary, sp.mirrorsOnly, true)
		}
	}
}

// Large-conversation materialization coalescing (aligned-chains Design rule
// 8, 2026-07). With delta-on-wire sync a busy remote conversation delivers
// many small live events per minute, and every inbound append funnels through
// fanOut into a FULL rewrite of the native session file and markdown
// transcript — for an 80 MB session that is an 80 MB transcode + write per
// turn. Artifacts whose append log OR materialized head payload exceeds the
// threshold therefore coalesce: the first dispatch arms a per-artifact
// trailing-edge timer and the write happens ONCE after activity settles,
// re-reading the head so the newest state lands. The cheap log-size test runs
// before materialization so a small delta cannot hide a huge conversation.
// Smaller conversations keep the immediate write.
const (
	// defaultLargeMaterializeThreshold is the materialized-payload size above
	// which native-session + markdown rewrites are debounced instead of
	// immediate. 8 MB keeps every interactively-sized conversation on the
	// instant path; only the huge sessions that motivated aligned-chains
	// defer.
	defaultLargeMaterializeThreshold = 8 << 20
	// defaultLargeMaterializeDebounce is the trailing-edge window: at most
	// one rewrite per large artifact per this interval. Huge transcripts can
	// take several CPU-seconds to transcode even after replay is eliminated;
	// one minute keeps mirrors reasonably fresh without turning every pause
	// between agent turns into another full native-session rewrite.
	defaultLargeMaterializeDebounce = time.Minute
)

// Package vars, not consts, so tests can shrink them without multi-MB
// fixtures or 15 s waits (same convention as remotePublishLiveMaxBytes).
var (
	largeMaterializeThreshold = defaultLargeMaterializeThreshold
	largeMaterializeDebounce  = defaultLargeMaterializeDebounce
)

// sortStrings is a small helper to keep SyncedAgents deterministic.
func sortStrings(s []string) {
	// Simple O(n²) sort — these lists are bounded by the adapter count
	// (<10 in V1). Avoids pulling in sort.Slice for trivial use.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// InitialScan walks the watched directory (and every cfg.AdditionalRoots
// native global root, FR-03.3 §4) once and runs the same primary-import +
// fan-out pipeline for every file present. Used by the daemon at startup to
// catch up on changes that happened while the daemon was offline (BRD
// FR-03.4).
//
// Honors o.cfg.Recursive: when true, walks the full tree; otherwise scans
// only direct children of each root.
//
// Errors during individual file imports are logged via the orchestrator's
// existing log-and-continue policy in fanOut. The primary Dir scan failing
// is returned as an error; AdditionalRoots that fail to scan (e.g. an agent
// dir that disappeared) are skipped so a flaky native root can't abort the
// whole startup catch-up.
func (o *Orchestrator) InitialScan(ctx context.Context) error {
	// Registered with the Close-join lifecycle: the daemon runs InitialScan on
	// a bare goroutine, so a shutdown mid-catch-up must wait for the in-flight
	// file (the closingNow checks below bound the residue to one file).
	if !o.beginBackground() {
		return nil
	}
	defer o.endBackground()
	if err := o.scanConfiguredRoots(ctx); err != nil {
		return fmt.Errorf("syncd: initial scan: %w", err)
	}
	return nil
}

func (o *Orchestrator) scanConfiguredRoots(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := o.scanRoot(o.cfg.Dir); err != nil {
		return err
	}
	for _, root := range o.cfg.AdditionalRoots {
		if ctx.Err() != nil || o.closingNow() {
			return nil
		}
		abs, aerr := filepath.Abs(root)
		if aerr != nil {
			continue
		}
		_ = o.scanRoot(abs) // best-effort per native root
	}
	for _, root := range o.cfg.RecursiveRoots {
		if ctx.Err() != nil || o.closingNow() {
			return nil
		}
		abs, aerr := filepath.Abs(root)
		if aerr != nil {
			continue
		}
		if isCodexSessionsRoot(abs) {
			o.scanRecentCodexSessionDays(abs, time.Now())
			continue
		}
		_ = o.scanRootRecursive(abs) // best-effort; ALWAYS recursive
	}
	for _, root := range o.cfg.MetadataRoots {
		if ctx.Err() != nil || o.closingNow() {
			return nil
		}
		abs, aerr := filepath.Abs(root)
		if aerr != nil {
			continue
		}
		_ = o.scanRootRecursive(abs)
	}
	for _, f := range o.cfg.WatchFiles {
		if ctx.Err() != nil || o.closingNow() {
			return nil
		}
		abs, aerr := filepath.Abs(f)
		if aerr != nil {
			continue
		}
		if fi, serr := os.Stat(abs); serr != nil || fi.IsDir() {
			continue
		}
		o.handleScanEvent(abs) // single watched file: same pipeline as a scan candidate
	}
	// Persist the fingerprints gathered this scan so a restart that loses the
	// process (kill, crash) before Close still skips the unchanged history.
	// Best-effort: a flush error only costs a re-scan next start.
	_ = o.scanCache.flush()
	return nil
}

func (o *Orchestrator) scanNativeRoots(ctx context.Context) error {
	for _, root := range o.cfg.AdditionalRoots {
		if ctx.Err() != nil || o.closingNow() {
			return nil
		}
		abs, aerr := filepath.Abs(root)
		if aerr != nil {
			continue
		}
		_ = o.scanRoot(abs) // best-effort per native root
	}
	for _, root := range o.cfg.RecursiveRoots {
		if ctx.Err() != nil || o.closingNow() {
			return nil
		}
		abs, aerr := filepath.Abs(root)
		if aerr != nil {
			continue
		}
		if isCodexSessionsRoot(abs) && o.cfg.DedicatedCodexSessionScan {
			continue
		}
		if isCodexSessionsRoot(abs) {
			o.scanRecentCodexSessionDays(abs, time.Now())
			continue
		}
		_ = o.scanRootRecursive(abs) // best-effort; ALWAYS recursive
	}
	for _, root := range o.cfg.MetadataRoots {
		if ctx.Err() != nil || o.closingNow() {
			return nil
		}
		abs, aerr := filepath.Abs(root)
		if aerr != nil {
			continue
		}
		_ = o.scanRootRecursive(abs)
	}
	for _, f := range o.cfg.WatchFiles {
		if ctx.Err() != nil || o.closingNow() {
			return nil
		}
		abs, aerr := filepath.Abs(f)
		if aerr != nil {
			continue
		}
		if fi, serr := os.Stat(abs); serr != nil || fi.IsDir() {
			continue
		}
		o.handleScanEvent(abs)
	}
	_ = o.scanCache.flush()
	return nil
}

// ScanRoots imports files currently present under roots using the same scan path
// as InitialScan. It is used after a live safety-blocker override/back-up so the
// newly-unblocked adapter can catch up without a daemon restart.
func (o *Orchestrator) ScanRoots(_ context.Context, roots []string) {
	if !o.beginBackground() {
		return
	}
	defer o.endBackground()
	for _, root := range roots {
		if o.closingNow() {
			return
		}
		abs, aerr := filepath.Abs(root)
		if aerr != nil {
			continue
		}
		_ = o.scanRoot(abs)
	}
}

func (o *Orchestrator) handleScanEvent(path string) bool {
	// Capture before the adapter attempt for the same reason as the successful
	// import path: a parse failure must not cache bytes appended while the failed
	// attempt was in flight. An older fingerprint is intentionally conservative
	// and keeps the changed file retryable.
	attemptFP, attemptFPOK := fingerprintPath(path)
	handled, ownerDeferred := o.handleEventWithDisposition(path, eventHandlingOptions{})
	if handled {
		return true
	}
	// Scans are a catch-up/backstop path, not an interactive retry loop. If a
	// native file is malformed, unsupported, or still mid-write, remember its
	// current fingerprint so every startup/live scan does not reparse the same
	// bytes forever. A real edit/appended answer moves size or mtime and falls
	// through on the next scan or watcher event.
	// A runtime/safety skip is different from a parse failure: the
	// file may stay byte-identical when that owner becomes eligible, so preserve
	// it as retryable instead of persisting its fingerprint.
	if !ownerDeferred && attemptFPOK {
		o.scanCache.recordFingerprint(path, attemptFP)
	}
	return false
}

type scanFileCandidate struct {
	path string
	mod  time.Time
}

func newerScanFileFirst(files []scanFileCandidate) {
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].mod.After(files[j].mod)
	})
}

// scanRootRecursive walks a root's full tree (regardless of o.cfg.Recursive)
// and dispatches each file to handleEvent. Used for RecursiveRoots — agents
// whose artifacts live in nested subdirectories (e.g. Codex sessions). Files
// are processed newest-first so live catch-up reaches fresh sessions before a
// large historical tree.
func (o *Orchestrator) scanRootRecursive(root string) error {
	files := make([]scanFileCandidate, 0, scanFileCandidateCap)
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		if info.IsDir() {
			// Prune dependency/VCS caches — a project's node_modules holds
			// nothing any adapter imports, and walking it is pure waste.
			if watcher.SkipWalkDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, scanFileCandidate{path: path, mod: info.ModTime()})
		return nil
	}); err != nil {
		return err
	}
	newerScanFileFirst(files)
	for _, f := range files {
		if o.closingNow() {
			return nil
		}
		o.handleScanEvent(f.path)
	}
	return nil
}

// scanRoot walks a single root and dispatches each file to handleEvent.
// Honors o.cfg.Recursive. Used by InitialScan for both the primary Dir and
// each AdditionalRoots entry.
func (o *Orchestrator) scanRoot(root string) error {
	if o.cfg.Recursive {
		files := make([]scanFileCandidate, 0, scanFileCandidateCap)
		if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // skip unreadable
			}
			if info.IsDir() {
				// Prune dependency/VCS caches (node_modules, .git, …) — a
				// recursively-watched project's node_modules holds nothing any
				// adapter imports; walking it is pure waste.
				if watcher.SkipWalkDir(info.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			files = append(files, scanFileCandidate{path: path, mod: info.ModTime()})
			return nil
		}); err != nil {
			return err
		}
		newerScanFileFirst(files)
		for _, f := range files {
			if o.closingNow() {
				return nil
			}
			o.handleScanEvent(f.path)
		}
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	files := make([]scanFileCandidate, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		files = append(files, scanFileCandidate{path: filepath.Join(root, e.Name()), mod: info.ModTime()})
	}
	newerScanFileFirst(files)
	for _, f := range files {
		if o.closingNow() {
			return nil
		}
		o.handleScanEvent(f.path)
	}
	return nil
}

// findArtifact loads the artifact by ID, trying every kind. Returns
// the artifact + true on success.
func (o *Orchestrator) findArtifact(id string) (acf.Artifact, bool) {
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		art, err := o.cfg.Store.ReadArtifact(kind, id)
		if err == nil {
			return art, true
		}
		// A read error other than not-found is treated as not-found for fan-out
		// purposes; nothing actionable here.
	}
	return acf.Artifact{}, false
}

func buildSourcePathHeadIndex(store *acf.Store) map[string]map[string]string {
	idx := map[string]map[string]string{}
	if store == nil {
		return idx
	}
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := store.ListArtifacts(kind)
		if err != nil {
			continue
		}
		for _, art := range arts {
			recordSourcePathHead(idx, art)
		}
	}
	return idx
}

func normalizedSourcePath(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func recordSourcePathHead(idx map[string]map[string]string, art acf.Artifact) {
	path := normalizedSourcePath(art.SourcePath)
	if path == "" {
		return
	}
	heads := idx[path]
	if heads == nil {
		heads = map[string]string{}
		idx[path] = heads
	}
	heads[art.ArtifactID] = art.HeadEventHash
}

func (o *Orchestrator) refreshSourcePathHeads(ids []string) {
	if len(ids) == 0 || o.cfg.Store == nil {
		return
	}
	arts := make([]acf.Artifact, 0, len(ids))
	for _, id := range ids {
		art, found := o.findArtifact(id)
		if found {
			arts = append(arts, art)
		}
	}
	if len(arts) == 0 {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sourcePathHeads == nil {
		return
	}
	for _, art := range arts {
		for path, heads := range o.sourcePathHeads {
			delete(heads, art.ArtifactID)
			if len(heads) == 0 {
				delete(o.sourcePathHeads, path)
			}
		}
		recordSourcePathHead(o.sourcePathHeads, art)
	}
}

// sourcePathHeadHashes snapshots the current head hash for any artifact whose
// SourcePath is this native file before an import runs. Importers preserve
// native event timestamps, so timestamp-based freshness is not reliable for
// delayed watcher processing; a head-hash change is the actual commit signal.
func (o *Orchestrator) sourcePathHeadHashes(path string) map[string]string {
	if o.cfg.Store == nil {
		return nil
	}
	path = normalizedSourcePath(path)
	out := map[string]string{}
	if o.mu.TryLock() {
		idx := o.sourcePathHeads
		if idx != nil {
			for id, head := range idx[path] {
				out[id] = head
			}
			o.mu.Unlock()
			return out
		}
		o.mu.Unlock()
	}

	// Tests and legacy construction paths sometimes instantiate Orchestrator
	// directly. Also fall back to the store scan if unrelated orchestration
	// work is holding o.mu; live imports must not park behind project watcher
	// setup or long status mutations.
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		art, found, err := o.cfg.Store.FindBySourcePath(kind, path)
		if err != nil || !found {
			continue
		}
		out[art.ArtifactID] = art.HeadEventHash
	}
	return out
}

func (o *Orchestrator) sourcePathHeadHashesFromIndex(path string) map[string]string {
	path = normalizedSourcePath(path)
	out := map[string]string{}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sourcePathHeads == nil {
		return out
	}
	for id, head := range o.sourcePathHeads[path] {
		out[id] = head
	}
	return out
}

func (o *Orchestrator) expandPriorHeadsForImportedSources(triggerPath string, ids []string, priorHeads map[string]string) map[string]string {
	if len(ids) == 0 || o.cfg.Store == nil {
		return priorHeads
	}
	triggerPath = normalizedSourcePath(triggerPath)
	out := priorHeads
	for _, id := range ids {
		if _, ok := out[id]; ok {
			continue
		}
		art, found := o.findArtifact(id)
		if !found {
			continue
		}
		sourcePath := normalizedSourcePath(art.SourcePath)
		if sourcePath == "" || sourcePath == triggerPath {
			continue
		}
		for sid, head := range o.sourcePathHeadHashesFromIndex(sourcePath) {
			if out == nil {
				out = map[string]string{}
			}
			if _, exists := out[sid]; !exists {
				out[sid] = head
			}
		}
	}
	return out
}

// freshlyCommittedIDs keeps only IDs whose current main-branch head changed
// during this import attempt. Importers may return an existing ID for unchanged
// content so callers can preserve identity; propagation paths need the narrower
// "new event committed" meaning.
func (o *Orchestrator) freshlyCommittedIDs(ids []string, priorHeads map[string]string) []string {
	if len(ids) == 0 || o.cfg.Store == nil {
		return nil
	}
	fresh := ids[:0:0]
	for _, id := range ids {
		art, found := o.findArtifact(id)
		if !found {
			continue
		}
		if prior, ok := priorHeads[id]; ok && art.HeadEventHash == prior {
			continue
		}
		fresh = append(fresh, id)
	}
	return fresh
}
