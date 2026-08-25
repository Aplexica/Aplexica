package proto

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"time"

	"github.com/aplexica/aplexica/internal/syncrules"
)

// RemoteDigestBytes is the fixed SHA-256 width used by remote protocol fields.
// It is a wire-format invariant, not a runtime tuning value.
const RemoteDigestBytes = 32

// ---------------------------------------------------------------------------
// Remote-plugin protocol.
//
// The Remote-plugin protocol is a sibling of the Adapter-plugin protocol
// defined alongside it. Where adapter plugins translate between native
// agent files and the canonical store, remote plugins move canonical-
// store events to and from a remote endpoint (Aplexica Cloud, a self-
// hosted relay, a BYO transport, etc).
//
// Trust posture: the OSS daemon does NOT know what specific remote
// transport a plugin uses (MQTT, S3, SSH, …). Every implementation
// satisfies the same JSON-RPC surface declared here: publish, subscribe,
// fetch, enumerate, plus a status
// query. This file defines the on-wire messages; the host wiring lives
// in internal/plugin/host/remote.go.
//
// Method namespace: Remote methods are prefixed `remote.` to avoid
// collision with the adapter-plugin method names (`initialize`,
// `import`, `export`, …). The shared `initialize` and `shutdown`
// methods are reused; the manifest's `kind` field (see manifest.go)
// distinguishes adapter vs remote at load time.
// ---------------------------------------------------------------------------

const (
	// Outbound (daemon -> plugin) remote methods.
	MethodRemotePublish             = "remote.publish"
	MethodRemotePublishStagedV1     = "remote.publish_staged_v1"
	MethodRemoteFetch               = "remote.fetch"
	MethodRemoteNegotiateSyncV1     = "remote.negotiate_sync_v1"
	MethodRemoteResumeCursorV1      = "remote.resume_cursor_v1"
	MethodRemoteResumeCursorsV1     = "remote.resume_cursors_v1"
	MethodRemoteFetchV2             = "remote.fetch_v2"
	MethodRemoteFetchParentV1       = "remote.fetch_parent_v1"
	MethodRemoteAckV2               = "remote.ack_v2"
	MethodRemoteRequestCheckpointV1 = "remote.request_checkpoint_v1"
	MethodRemoteEnumerate           = "remote.enumerate"
	MethodRemoteSubscribe           = "remote.subscribe"
	MethodRemoteUnsubscribe         = "remote.unsubscribe"
	MethodRemoteStatus              = "remote.status"

	// Outbound key-rotation methods. The daemon calls
	// these on the plugin during client-side namespace key rotation: list
	// the surviving member devices + their wrap keys, write the freshly
	// wrapped key material back to the namespace_keys row, and broadcast it
	// to surviving devices over the live transport.
	MethodRemoteListNamespaceDevices  = "remote.list_namespace_devices"
	MethodRemotePutNamespaceKey       = "remote.put_namespace_key"
	MethodRemoteGetNamespaceKey       = "remote.get_namespace_key"
	MethodRemoteBroadcastNamespaceKey = "remote.broadcast_namespace_key"

	// Account-scoped wrap-key discovery for end-to-end encryption. Unlike the
	// namespace-scoped device list above, these are keyed on the caller's
	// ACCOUNT (resolved server-side from the device proof), so the daemon can
	// resolve outbound recipients WITHOUT knowing a namespace id — which the
	// Personal tier never learns. register_wrap_key persists THIS device's
	// X25519 wrap public key to the control plane (no re-pair); list_account_-
	// devices returns every active device in the account that has a registered
	// wrap pubkey (including self), so the orchestrator can seal each event to
	// all of them.
	MethodRemoteRegisterWrapKey    = "remote.register_wrap_key"
	MethodRemoteListAccountDevices = "remote.list_account_devices"

	// Account-level envelope capability probe (2026-07-29 envelope
	// wire-efficiency ADR D3). Same dispatch family as
	// remote.list_account_devices: account-scoped, no params, resolved
	// server-side from the plugin's cached entitlement. The daemon uses it as
	// the fleet-wide kill switch for envelope v3 ENCODING, layered on top of
	// the per-peer roster EnvelopeVersions intersection — never instead of it.
	MethodRemoteEnvelopeCaps = "remote.envelope_caps"

	// Outbound RBAC method. The daemon asks the
	// plugin for the caller's role in a namespace so it can gate team
	// operations client-side (defense-in-depth; the server stays
	// authoritative). The plugin's authenticated transport identifies the
	// caller server-side, so the request carries only the namespace id.
	MethodRemoteGetNamespaceRole = "remote.get_namespace_role"

	// Client-authenticated trust protocol. Every Blob is opaque to the
	// plugin/control plane and is cryptographically verified by the daemon.
	MethodRemoteGetTrustAnchor                        = "remote.get_trust_anchor"
	MethodRemoteSubmitTrustAnchor                     = "remote.submit_trust_anchor"
	MethodRemoteGetServiceTrustConfig                 = "remote.get_service_trust_config"
	MethodRemoteGetAuthorityTransitions               = "remote.get_authority_transitions"
	MethodRemoteSubmitAuthorityTransition             = "remote.submit_authority_transition"
	MethodRemoteGetSignedRoster                       = "remote.get_signed_roster"
	MethodRemoteSubmitRosterTransition                = "remote.submit_roster_transition"
	MethodRemoteSubmitAtomicAuthorityRosterTransition = "remote.submit_atomic_authority_roster_transition"
	MethodRemoteRegisterDeviceCredential              = "remote.register_device_credential"
	MethodRemoteGetRosterConsistency                  = "remote.get_roster_consistency"
	MethodRemoteActivateSyncGeneration                = "remote.activate_sync_generation"
	MethodRemoteGetSyncGenerationStatus               = "remote.get_sync_generation_status"
	MethodRemoteSecurityEpochPrepare                  = "remote.security_epoch_prepare"
	MethodRemoteSecurityEpochCommit                   = "remote.security_epoch_commit"
	MethodRemoteSecurityEpochActivate                 = "remote.security_epoch_activate"
	MethodRemoteSecurityEpochStatus                   = "remote.security_epoch_status"
	MethodRemoteSubmitDeviceTransitionPlan            = "remote.submit_device_transition_plan"
	MethodRemoteGetDeviceTransitionPlans              = "remote.get_device_transition_plans"

	// Inbound (plugin -> daemon) notifications. The host's JSON-RPC
	// dispatcher accepts these as server-to-client notifications and
	// routes them into the daemon's sync orchestrator.
	NotificationRemoteInbound            = "remote.inbound"
	MethodRemoteInboundDeliveryV2        = "remote.inbound_v2"
	MethodRemoteInboundFinalizeV1        = "remote.inbound_finalize_v1"
	NotificationRemoteConnState          = "remote.conn_state"
	NotificationRemoteEnumerateHint      = "remote.enumerate_hint"
	NotificationRemoteCheckpointNeededV1 = "remote.checkpoint_needed_v1"

	// NotificationRemoteRulesUpdate carries a cloud-pushed selective-sync
	// ruleset. The remote (relay / cloud plugin) sends this when the
	// account's routing rules change in the portal so the daemon can
	// rebuild its syncrules.Engine and apply the new ruleset live —
	// without a restart and without the user editing the local
	// rules.toml. ChangeID is an idempotency token the daemon may use to
	// dedupe redelivery.
	NotificationRemoteRulesUpdate = "remote.rules_update"

	// NotificationRemoteNamespaceKeyRotated carries the control plane's
	// namespace.key_rotated audit signal (member removed -> key_version
	// bumped). The plugin extracts the audit payload and forwards it; the
	// daemon's keyrotation.Rotator turns it into client-side key work.
	NotificationRemoteNamespaceKeyRotated = "remote.namespace_key_rotated"

	// NotificationRemoteNamespaceKeyBroadcast carries freshly-wrapped key
	// material pushed by the rotation leader to surviving devices. A
	// receiving daemon unwraps the blob addressed to its device key and
	// installs the new content key.
	NotificationRemoteNamespaceKeyBroadcast = "remote.namespace_key_broadcast"
)

// RemoteKind is the manifest.kind value identifying a plugin as a
// remote-transport plugin (rather than an adapter). The daemon's
// plugin loader inspects this to decide which method set to dispatch
// over the channel.
const RemoteKind = "remote"

// RemoteEvent is the on-wire payload for one canonical-store event
// crossing the daemon<->plugin boundary. The full ACF event is
// serialized and sealed opaquely as Bytes. The plugin and remote service
// transport the ciphertext envelope without decrypting or reserializing it.
//
// EventID is an opaque transport identity. ParentHash is the prior
// canonical ACF event's content hash, never the prior RemoteEvent EventID.
// EventHash is the current canonical ACF event's content hash. Canonical
// hashes remain opaque to the plugin and remote service; they are carried so
// a daemon can resolve ancestry without exposing event content.
//
// EventID is OPAQUE to the plugin — for a
// lane="retained" conversation event it is the head EventID plus a
// "-r-<origin>" suffix, where <origin> is a short discriminator derived
// from the ORIGIN device id (the two lanes of one commit are two
// distinct transport events, and two devices that re-authored the same
// head EventID must not collide on one retained wire id; plain "-r"
// when the daemon is unpaired). It must never be parsed as a UUID or
// otherwise decomposed — treat it as an opaque string end to end.
type RemoteEvent struct {
	// Project authorization is local-only routing metadata carried through the
	// daemon/plugin spool. The cloud relay treats it as opaque. A daemon must
	// reject a queued intent after the project's generation changes.
	ProjectID                      string                  `json:"project_id,omitempty"`
	ProjectAuthorizationGeneration uint64                  `json:"project_authorization_generation,omitempty"`
	AccessGeneration               uint64                  `json:"access_generation,omitempty"`
	AccessSetHash                  [RemoteDigestBytes]byte `json:"access_set_hash,omitempty"`
	SecurityBarrierID              [RemoteDigestBytes]byte `json:"security_barrier_id,omitempty"`
	SecurityGeneration             uint64                  `json:"security_generation,omitempty"`
	KeyMode                        string                  `json:"key_mode,omitempty"`
	KeyVersion                     uint64                  `json:"key_version,omitempty"`
	// CheckpointCoverage is the highest durable server position covered by a
	// checkpoint event. CheckpointGeneration is an opaque compatibility
	// generation selected by the checkpoint producer. Both stay absent on
	// ordinary and legacy events.
	CheckpointCoverage   uint64 `json:"checkpoint_coverage,omitempty"`
	CheckpointGeneration string `json:"checkpoint_generation,omitempty"`
	NamespaceID          string `json:"namespace_id"`
	BranchID             string `json:"branch_id"`
	ArtifactID           string `json:"artifact_id"`
	EventID              string `json:"event_id"`
	ParentHash           string `json:"parent_hash"`
	// CheckpointAlignmentHash is the authenticated canonical head covered by a
	// retained checkpoint. It is deliberately distinct from ParentHash, which
	// remains the checkpoint event's actual canonical predecessor. The value is
	// absent on live/tombstone and legacy events.
	CheckpointAlignmentHash string `json:"checkpoint_alignment_hash,omitempty"`
	// EventHash is empty on legacy events. BodyDigest is the lowercase-hex
	// SHA-256 of the exact sealed Bytes crossing this boundary; it is empty on
	// legacy events. Strings keep absent fields truly absent under omitempty,
	// which preserves the frozen v1 JSON shape.
	EventHash   string          `json:"event_hash,omitempty"`
	BodyDigest  string          `json:"body_digest,omitempty"`
	Kind        string          `json:"kind"`       // acf.Kind as string for cross-language consumers
	Type        string          `json:"event_type"` // acf event type (create / update / fork / merge / …)
	Timestamp   time.Time       `json:"ts"`
	Bytes       json.RawMessage `json:"bytes"`  // exact sealed envelope; plugin transports it opaquely
	Sequence    uint64          `json:"seq"`    // per-artifact monotonic
	Origin      string          `json:"origin"` // device ID (the daemon's; used for loop prevention)
	SourceAgent string          `json:"source_agent,omitempty"`
	// Lane routes the event on the transport (aligned-chains delta sync,
	// 2026-07): "live" carries the VERBATIM stored head event — small,
	// published on the non-retained live topic, FIFO per artifact, never
	// coalesced; "retained" carries the full materialized state plus the
	// alignment metadata a receiver adopts a baseline from — retained topic,
	// coalescible per artifact, may be large. Mirrors the daemon's
	// syncd.OutboundEvent.Lane; the plugin's inbound bridge stamps it from
	// the topic shape. Empty on legacy pre-lane events (e.g. an old persisted
	// outbox entry replayed after upgrade) — the daemon receiver then keeps
	// the pre-lane reconcile behavior.
	//
	// Size budgets are lane-aware on the daemon publisher side: live (and
	// legacy laneless) events keep the realtime 4 MB cap, while a retained
	// event may carry a moderate multi-MB baseline under the daemon's
	// practical retained-publish cap and always arrives as a SOLO
	// remote.publish batch with a longer call deadline — see internal/daemon's
	// remotePublishMaxEventBytes / remotePublishMaxRetainedEventBytes.
	Lane string `json:"lane,omitempty"`
	// Clear marks a lane="retained" event as a retained-slot CLEAR: Bytes
	// is EMPTY and the plugin must clear the artifact's retained topic slot
	// (MQTT: publish an empty retained payload) instead of performing a
	// normal publish. The daemon emits one when a redaction leaves a
	// conversation with no retainable state, so the broker stops serving
	// the last pre-redaction snapshot; the redaction event itself still
	// travels on the live lane. Inbound, a receiving daemon skips Clear
	// events (there is no body to import).
	Clear bool `json:"clear,omitempty"`
	// DaemonStagedPayload is a daemon-private durable reference used only for
	// exceptional retained checkpoints whose sealed body is larger than the
	// bounded JSON outbox/frame budget. The referenced file lives in the
	// daemon-owned private outbox staging directory; the runner converts it to
	// RemotePublishStagedV1Params and removes this field before crossing the
	// plugin RPC boundary. Keeping only this content-free descriptor in the
	// outbox avoids base64-expanding a 256 MiB checkpoint while preserving the
	// exact sealed bytes across daemon and plugin restarts.
	DaemonStagedPayload *RemoteDaemonStagedPayloadV1 `json:"daemon_staged_payload_v1,omitempty"`
}

// RemoteDaemonStagedPayloadV1 is an on-disk daemon outbox descriptor, not a
// public remote-plugin capability. FileID is a safe basename under the exact
// private transfer root granted to the authenticated plugin process.
type RemoteDaemonStagedPayloadV1 struct {
	FileID      string `json:"file_id"`
	SealedBytes uint64 `json:"sealed_bytes"`
	BodyDigest  string `json:"body_digest"`
}

// RemotePublishParams — daemon hands one batch of events to the
// plugin for outbound transmission. Plugins MUST respect per-namespace
// ordering: events with the same NamespaceID + ArtifactID must be
// transmitted in Sequence order. Cross-namespace ordering is free.
type RemotePublishParams struct {
	Events []RemoteEvent `json:"events"`
}

const (
	// RemoteStagedTransferProtocolV1 identifies the additive, file-backed
	// daemon-to-plugin checkpoint handoff. It never changes remote.publish.
	RemoteStagedTransferProtocolV1 uint16 = 1
	// MaxRemoteStagedCheckpointBytes is the exceptional sealed-checkpoint
	// ceiling. Ordinary events remain constrained by MaxSealedEventBytes.
	MaxRemoteStagedCheckpointBytes = 256 << 20
)

// RemoteStagedFileV1 names a single private file beneath the transfer root
// inherited by the authenticated plugin process. TransferID is both a random
// capability and the complete root-relative basename; arbitrary paths never
// cross the RPC boundary.
type RemoteStagedFileV1 struct {
	ProtocolVersion uint16 `json:"protocol_version"`
	TransferID      string `json:"transfer_id"`
	SealedBytes     uint64 `json:"sealed_bytes"`
	BodyDigest      string `json:"body_digest"`
	StreamID        string `json:"stream_id"`
	StreamEpoch     string `json:"stream_epoch"`
	BindingDigest   string `json:"binding_digest"`
}

// RemotePublishStagedV1Params carries metadata only. Event.Bytes MUST be JSON
// null; the exact sealed body is read from the private file named by Transfer.
// The method is allowed only for one retained checkpoint and is independently
// gated by the signed staged-checkpoint capability and negotiated descriptor.
type RemotePublishStagedV1Params struct {
	Event    RemoteEvent        `json:"event"`
	Transfer RemoteStagedFileV1 `json:"transfer"`
}

// RemoteStagedBindingDigest cryptographically binds a staged basename and its
// exact bytes to every identity, stream, authorization, security-generation,
// and checkpoint-generation field used by the durable coordinator. Both ends
// recompute this value before file or cloud I/O.
func RemoteStagedBindingDigest(event RemoteEvent, transfer RemoteStagedFileV1) string {
	h := sha256.New()
	stagedHashString(h, "aplexica/remote-staged-checkpoint/v1")
	stagedHashU64(h, uint64(transfer.ProtocolVersion))
	stagedHashString(h, transfer.TransferID)
	stagedHashU64(h, transfer.SealedBytes)
	stagedHashString(h, transfer.BodyDigest)
	stagedHashString(h, transfer.StreamID)
	stagedHashString(h, transfer.StreamEpoch)
	stagedHashString(h, event.ProjectID)
	stagedHashU64(h, event.ProjectAuthorizationGeneration)
	stagedHashU64(h, event.AccessGeneration)
	_, _ = h.Write(event.AccessSetHash[:])
	_, _ = h.Write(event.SecurityBarrierID[:])
	stagedHashU64(h, event.SecurityGeneration)
	stagedHashString(h, event.KeyMode)
	stagedHashU64(h, event.KeyVersion)
	stagedHashU64(h, event.CheckpointCoverage)
	stagedHashString(h, event.CheckpointGeneration)
	stagedHashString(h, event.NamespaceID)
	stagedHashString(h, event.BranchID)
	stagedHashString(h, event.ArtifactID)
	stagedHashString(h, event.EventID)
	stagedHashString(h, event.ParentHash)
	stagedHashString(h, event.CheckpointAlignmentHash)
	stagedHashString(h, event.EventHash)
	stagedHashString(h, event.BodyDigest)
	stagedHashString(h, event.Kind)
	stagedHashString(h, event.Type)
	stagedHashU64(h, uint64(event.Timestamp.Unix()))
	stagedHashU64(h, uint64(event.Timestamp.Nanosecond()))
	stagedHashU64(h, event.Sequence)
	stagedHashString(h, event.Origin)
	stagedHashString(h, event.SourceAgent)
	stagedHashString(h, event.Lane)
	if event.Clear {
		stagedHashU64(h, 1)
	} else {
		stagedHashU64(h, 0)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func stagedHashString(h hash.Hash, value string) {
	stagedHashU64(h, uint64(len(value)))
	_, _ = h.Write([]byte(value))
}

func stagedHashU64(h hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = h.Write(encoded[:])
}

// RemotePublishResult — plugin reports per-event publication outcome.
// Daemon retries entries with Accepted=false on the next publish tick
// (rate-limited; honor the plugin's Retryable/RetryAfter hints when
// present).
type RemotePublishResult struct {
	Outcomes []RemotePublishOutcome `json:"outcomes"`
}

// RemotePublishOutcome describes the fate of one event in the
// preceding RemotePublishParams.Events array. Index matches input
// position 1:1 so the daemon can pair them.
type RemotePublishOutcome struct {
	EventID    string        `json:"event_id"`
	Accepted   bool          `json:"accepted"`
	Retryable  bool          `json:"retryable,omitempty"`
	RetryAfter time.Duration `json:"retry_after,omitempty"`
	Error      string        `json:"error,omitempty"`
	// The fields below are present only for a negotiated durable append.
	// Accepted without RemoteDurabilityCommitted retains the legacy meaning and is
	// not proof that the sealed bytes reached durable cloud storage.
	Durability   string `json:"durability,omitempty"`
	CommitCursor string `json:"commit_cursor,omitempty"`
	// CommitPosition is the server-authenticated monotonically increasing
	// position represented by CommitCursor. Durable publishers require both:
	// the opaque cursor is used for protocol continuation while the numeric
	// position drives monotonic local watermark recovery.
	CommitPosition uint64 `json:"commit_position,omitempty"`
	StreamID       string `json:"stream_id,omitempty"`
	StreamEpoch    string `json:"stream_epoch,omitempty"`
	BodyDigest     string `json:"body_digest,omitempty"`
	// These digests are supplied only after the signed plugin has validated the
	// server receipt against the original append request. They prevent a
	// same-body response from another event or namespace from becoming a
	// daemon-visible durable acknowledgement.
	EventIdentityDigest string `json:"event_identity_digest,omitempty"`
	MetadataDigest      string `json:"metadata_digest,omitempty"`
	Duplicate           bool   `json:"duplicate,omitempty"`
}

const (
	RemoteDurabilityCommitted = "committed"

	RemoteSyncModeLegacy         = "legacy"
	RemoteSyncModeShadow         = "shadow"
	RemoteSyncModeDurableRead    = "durable_read"
	RemoteSyncModeDeltaPreferred = "delta_preferred"
	RemoteSyncModeDeltaRequired  = "delta_required"

	// Machine-readable negotiation reasons for recovering a daemon-owned
	// terminal finalize obligation after total plugin state loss.
	RemoteSyncReasonTerminalFinalizeDrainPinned           = "terminal-finalize-drain-pinned"
	RemoteSyncReasonTerminalFinalizeRecoveryBlocked       = "terminal-finalize-recovery-blocked"
	RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid  = "terminal-finalize-recovery-state-invalid"
	RemoteSyncReasonTerminalFinalizeCapabilityUnavailable = "terminal-finalize-recovery-capability-unavailable"
	RemoteSyncReasonTerminalFinalizeAwaitingNegotiation   = "terminal-finalize-recovery-awaiting-negotiation"
)

// RemoteFetchParams — daemon asks the plugin to pull any events the
// remote has that the local store doesn't yet. Since is a hash-chain
// cursor (the local store's last-known event ID per branch); empty
// "" means "fetch from the beginning of recorded history."
type RemoteFetchParams struct {
	NamespaceID string `json:"namespace_id"`
	BranchID    string `json:"branch_id,omitempty"` // empty = all branches in namespace
	Since       string `json:"since"`
	Limit       int    `json:"limit,omitempty"` // 0 = plugin chooses (typical: 100)
}

// RemoteFetchResult — plugin returns the events the local store
// doesn't have, in chain order. NextCursor is the cursor to pass to
// the next RemoteFetch call; empty when caught up.
type RemoteFetchResult struct {
	Events     []RemoteEvent `json:"events"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// RemoteNegotiateSyncV1Params advertises daemon support at runtime. A caller
// must still verify CapabilityDurableDeltaSyncV1 in the signed plugin manifest
// before invoking this method. Failure, an unsupported response, or an empty
// selection means legacy behavior.
type RemoteNegotiateSyncV1Params struct {
	ProtocolMin          uint16   `json:"protocol_min"`
	ProtocolMax          uint16   `json:"protocol_max"`
	DaemonCapabilities   []string `json:"daemon_capabilities,omitempty"`
	RequestedMaximumMode string   `json:"requested_maximum_mode,omitempty"`
	// PendingFinalizeEvidence is the daemon-owned, content-free terminal
	// obligation that survived total plugin state loss. It is presented before
	// the server selects a stream generation so a replacement plugin can pin it
	// only when fresh server authority names this exact stream and epoch.
	PendingFinalizeEvidence *RemoteInboundFinalizeEvidenceV1 `json:"pending_finalize_evidence,omitempty"`
}

// RemoteNegotiateSyncV1Result is the server-controlled, fail-safe runtime
// selection. Mode is legacy unless every durable-mode safety gate is true.
// StreamID, StreamEpoch, and cursors are opaque authenticated values.
type RemoteNegotiateSyncV1Result struct {
	SelectedProtocol        uint16   `json:"selected_protocol"`
	Mode                    string   `json:"mode"`
	ServerCapabilities      []string `json:"server_capabilities,omitempty"`
	AllActiveDevicesCapable bool     `json:"all_active_devices_capable"`
	CheckpointReady         bool     `json:"checkpoint_ready"`
	FeatureGateEnabled      bool     `json:"feature_gate_enabled"`
	StreamID                string   `json:"stream_id,omitempty"`
	StreamEpoch             string   `json:"stream_epoch,omitempty"`
	// Streams is the complete server-authorized stream set for one negotiation
	// generation. The singular fields above remain the account-default stream
	// for compatibility with shadow-mode plugins that predate multistream.
	Streams            []RemoteStreamDescriptorV1 `json:"streams,omitempty"`
	MaxEventBytes      uint64                     `json:"max_event_bytes,omitempty"`
	MaxPageEvents      uint32                     `json:"max_page_events,omitempty"`
	MaxPageBytes       uint64                     `json:"max_page_bytes,omitempty"`
	MinAvailableCursor string                     `json:"min_available_cursor,omitempty"`
	RetentionSeconds   uint64                     `json:"retention_seconds,omitempty"`
	Reason             string                     `json:"reason,omitempty"`
	// PendingFinalizeEvidence is a replacement plugin's content-free proposal
	// for an obligation it retained while the daemon no longer considers native
	// finalization pending. The daemon treats this as untrusted until it proves
	// an identical, already-finalized completion at its exact current cursor.
	PendingFinalizeEvidence *RemoteInboundFinalizeEvidenceV1 `json:"pending_finalize_evidence,omitempty"`
}

// RemoteStreamDescriptorV1 binds one authorized account or namespace scope to
// its opaque durable stream generation and its independently enforced page
// limits. NamespaceID is empty only for the account-default stream. Tip fields
// are scheduling hints; they never replace the daemon-owned contiguous cursor.
type RemoteStreamDescriptorV1 struct {
	NamespaceID        string `json:"namespace_id"`
	StreamID           string `json:"stream_id"`
	StreamEpoch        string `json:"stream_epoch"`
	MaxEventBytes      uint64 `json:"max_event_bytes"`
	MaxPageEvents      uint32 `json:"max_page_events"`
	MaxPageBytes       uint64 `json:"max_page_bytes"`
	MinAvailableCursor string `json:"min_available_cursor,omitempty"`
	TipCursor          string `json:"tip_cursor,omitempty"`
	TipPosition        uint64 `json:"tip_position,omitempty"`
	RetentionSeconds   uint64 `json:"retention_seconds,omitempty"`
	CheckpointReady    bool   `json:"checkpoint_ready"`
}

// RemoteResumeCursorV1Params transfers the daemon-owned authoritative resume
// cursor into a freshly started or replaced plugin. CursorPresent=false is an
// explicit authenticated genesis state; a plugin must not substitute a cursor
// retained by an older executable instance.
type RemoteResumeCursorV1Params struct {
	Authoritative           bool                             `json:"authoritative"`
	StreamID                string                           `json:"stream_id"`
	StreamEpoch             string                           `json:"stream_epoch"`
	CursorPresent           bool                             `json:"cursor_present"`
	Cursor                  string                           `json:"cursor,omitempty"`
	CursorDigest            string                           `json:"cursor_digest,omitempty"`
	Position                uint64                           `json:"position,omitempty"`
	PendingFinalizeEvidence *RemoteInboundFinalizeEvidenceV1 `json:"pending_finalize_evidence,omitempty"`
}

// RemoteResumeCursorV1Result echoes the exact authoritative state installed by
// the plugin. The daemon rejects a partial or different echo before durable
// reads can be selected.
type RemoteResumeCursorV1Result struct {
	Accepted                bool                             `json:"accepted"`
	StreamID                string                           `json:"stream_id"`
	StreamEpoch             string                           `json:"stream_epoch"`
	CursorPresent           bool                             `json:"cursor_present"`
	Cursor                  string                           `json:"cursor,omitempty"`
	CursorDigest            string                           `json:"cursor_digest,omitempty"`
	Position                uint64                           `json:"position,omitempty"`
	PendingFinalizeEvidence *RemoteInboundFinalizeEvidenceV1 `json:"pending_finalize_evidence,omitempty"`
}

// RemoteResumeCursorsV1Params atomically transfers the complete daemon-owned
// cursor set for one negotiated stream generation. Cursors must contain every
// negotiated descriptor exactly once; a plugin must install all or none.
type RemoteResumeCursorsV1Params struct {
	Cursors []RemoteResumeCursorV1Params `json:"cursors"`
}

// RemoteResumeCursorsV1Result is an exact full echo of the atomically installed
// cursor set. Accepted=false or any missing, reordered, duplicated, or changed
// cursor keeps durable multistream reads disabled.
type RemoteResumeCursorsV1Result struct {
	Accepted bool                         `json:"accepted"`
	Cursors  []RemoteResumeCursorV1Result `json:"cursors"`
}

// RemoteFetchV2Params uses an opaque durable-log cursor rather than the v1
// canonical EventID cursor. LimitEvents and LimitBytes are simultaneous hard
// bounds; zero lets the plugin choose a value no larger than negotiation.
type RemoteFetchV2Params struct {
	StreamID     string `json:"stream_id"`
	StreamEpoch  string `json:"stream_epoch"`
	Cursor       string `json:"cursor,omitempty"`
	CursorDigest string `json:"cursor_digest,omitempty"`
	Position     uint64 `json:"position,omitempty"`
	LimitEvents  uint32 `json:"limit_events,omitempty"`
	LimitBytes   uint64 `json:"limit_bytes,omitempty"`
}

type RemoteFetchV2Result struct {
	Events              []RemoteEvent       `json:"events"`
	StagedCheckpoint    *RemoteStagedFileV1 `json:"staged_checkpoint,omitempty"`
	PredecessorCursor   string              `json:"predecessor_cursor,omitempty"`
	PredecessorPosition uint64              `json:"predecessor_position,omitempty"`
	NextCursor          string              `json:"next_cursor,omitempty"`
	NextCursorDigest    string              `json:"next_cursor_digest,omitempty"`
	NextPosition        uint64              `json:"next_position,omitempty"`
	StreamEpoch         string              `json:"stream_epoch"`
	MinAvailableCursor  string              `json:"min_available_cursor,omitempty"`
	HasMore             bool                `json:"has_more,omitempty"`
}

// RemoteRecoveryEventV1 carries one server-authenticated event fetched outside
// normal contiguous delivery (a missing canonical parent or compatible
// checkpoint). Its cursor proves the event's durable-log position, but the
// daemon never advances its main stream cursor from this out-of-band record.
// Checkpoint records additionally carry the exact authenticated global cursor
// represented by Event.CheckpointCoverage; ordinary parent records omit the
// coverage tuple.
type RemoteRecoveryEventV1 struct {
	Event                RemoteEvent         `json:"event"`
	StagedCheckpoint     *RemoteStagedFileV1 `json:"staged_checkpoint,omitempty"`
	PredecessorCursor    string              `json:"predecessor_cursor"`
	PredecessorPosition  uint64              `json:"predecessor_position"`
	Cursor               string              `json:"cursor"`
	CursorDigest         string              `json:"cursor_digest"`
	Position             uint64              `json:"position"`
	CoverageCursor       string              `json:"coverage_cursor,omitempty"`
	CoverageCursorDigest string              `json:"coverage_cursor_digest,omitempty"`
	CoveragePosition     uint64              `json:"coverage_position,omitempty"`
}

type RemoteFetchParentV1Params struct {
	StreamID           string                  `json:"stream_id"`
	StreamEpoch        string                  `json:"stream_epoch"`
	NamespaceID        string                  `json:"namespace_id"`
	BranchID           string                  `json:"branch_id,omitempty"`
	ArtifactID         string                  `json:"artifact_id"`
	EventHash          string                  `json:"event_hash"`
	AccessGeneration   uint64                  `json:"access_generation,omitempty"`
	AccessSetHash      [RemoteDigestBytes]byte `json:"access_set_hash,omitempty"`
	SecurityBarrierID  [RemoteDigestBytes]byte `json:"security_barrier_id,omitempty"`
	SecurityGeneration uint64                  `json:"security_generation,omitempty"`
	KeyMode            string                  `json:"key_mode,omitempty"`
	KeyVersion         uint64                  `json:"key_version,omitempty"`
}

type RemoteFetchParentV1Result struct {
	Found              bool                   `json:"found"`
	Record             *RemoteRecoveryEventV1 `json:"record,omitempty"`
	CheckpointRequired bool                   `json:"checkpoint_required,omitempty"`
	ReasonCode         string                 `json:"reason_code,omitempty"`
	MinAvailableCursor string                 `json:"min_available_cursor,omitempty"`
}

// RemoteAckV2Params acknowledges only the caller's highest contiguous,
// durably-applied cursor. CursorDigest is the lowercase-hex SHA-256 of the
// exact opaque cursor token. Position is the authenticated server position
// represented by that token. Checkpoint fields are optional recovery and
// retention evidence.
type RemoteAckV2Params struct {
	StreamID                 string `json:"stream_id"`
	StreamEpoch              string `json:"stream_epoch"`
	Cursor                   string `json:"cursor"`
	CursorDigest             string `json:"cursor_digest,omitempty"`
	Position                 uint64 `json:"position,omitempty"`
	CheckpointID             string `json:"checkpoint_id,omitempty"`
	CheckpointCoverageCursor string `json:"checkpoint_coverage_cursor,omitempty"`
}

type RemoteAckV2Result struct {
	Accepted             bool   `json:"accepted"`
	AcknowledgedCursor   string `json:"acknowledged_cursor,omitempty"`
	AcknowledgedPosition uint64 `json:"acknowledged_position,omitempty"`
	Duplicate            bool   `json:"duplicate,omitempty"`
}

// RemoteRequestCheckpointV1Params requests a fresh client-produced encrypted
// checkpoint. It contains routing and security-generation metadata only; no
// artifact body or user-readable content crosses this control message.
type RemoteRequestCheckpointV1Params struct {
	RequestID         string `json:"request_id,omitempty"`
	StreamID          string `json:"stream_id"`
	StreamEpoch       string `json:"stream_epoch"`
	NamespaceID       string `json:"namespace_id"`
	BranchID          string `json:"branch_id,omitempty"`
	ArtifactID        string `json:"artifact_id"`
	Kind              string `json:"kind"`
	MissingParentHash string `json:"missing_parent_hash,omitempty"`
	Reason            string `json:"reason"`
	Cursor            string `json:"cursor,omitempty"`
	CursorDigest      string `json:"cursor_digest,omitempty"`
	Position          uint64 `json:"position,omitempty"`
	// MinimumCoverage is the exact cloud-log position of the blocked event a
	// replacement full checkpoint must cover. It is distinct from Position,
	// which remains bound to Cursor and therefore names the delivery's
	// predecessor. Older plugins ignore this additive field and remain
	// fail-closed at the receiver if they return an insufficient checkpoint.
	MinimumCoverage      uint64                  `json:"minimum_coverage,omitempty"`
	CheckpointGeneration string                  `json:"checkpoint_generation,omitempty"`
	AccessGeneration     uint64                  `json:"access_generation,omitempty"`
	AccessSetHash        [RemoteDigestBytes]byte `json:"access_set_hash,omitempty"`
	SecurityBarrierID    [RemoteDigestBytes]byte `json:"security_barrier_id,omitempty"`
	SecurityGeneration   uint64                  `json:"security_generation,omitempty"`
	KeyMode              string                  `json:"key_mode,omitempty"`
	KeyVersion           uint64                  `json:"key_version,omitempty"`
}

type RemoteRequestCheckpointV1Result struct {
	Requested  bool                   `json:"requested"`
	RequestID  string                 `json:"request_id,omitempty"`
	Duplicate  bool                   `json:"duplicate,omitempty"`
	Pending    bool                   `json:"pending,omitempty"`
	Checkpoint *RemoteRecoveryEventV1 `json:"checkpoint,omitempty"`
}

type RemoteInboundDeliveryV2 struct {
	DeliveryID       string              `json:"delivery_id"`
	Cursor           string              `json:"cursor"`
	Events           []RemoteEvent       `json:"events"`
	StagedCheckpoint *RemoteStagedFileV1 `json:"staged_checkpoint,omitempty"`
	// Durable-log metadata is absent on existing inbound-v2 deliveries. Its
	// presence lets the daemon bind a contiguous cursor to the correct opaque
	// stream and epoch without changing canonical event content.
	// PredecessorCursor is the exact server-authenticated cursor from which the
	// event was fetched (including a signed position-zero cursor at genesis).
	// Cursor is the server-authenticated cursor for Position, and CursorDigest
	// is the lowercase-hex SHA-256 of that exact opaque token.
	ProtocolVersion     uint16 `json:"protocol_version,omitempty"`
	StreamID            string `json:"stream_id,omitempty"`
	StreamEpoch         string `json:"stream_epoch,omitempty"`
	PredecessorCursor   string `json:"predecessor_cursor,omitempty"`
	PredecessorPosition uint64 `json:"predecessor_position,omitempty"`
	Position            uint64 `json:"position,omitempty"`
	CursorDigest        string `json:"cursor_digest,omitempty"`
	// Present together only for a negotiated redaction-safe replay batch.
	// Singletons deliberately retain the exact pre-batch JSON shape.
	BatchEventCount uint16 `json:"batch_event_count,omitempty"`
	BatchDigest     string `json:"batch_digest,omitempty"`
}

type RemoteInboundEventOutcomeV2 struct {
	Index       uint32 `json:"index"`
	Disposition string `json:"disposition"`
	ReasonCode  string `json:"reason_code"`
}

type RemoteInboundAckV2 struct {
	DeliveryID       string                        `json:"delivery_id"`
	Outcomes         []RemoteInboundEventOutcomeV2 `json:"outcomes"`
	NextCursor       string                        `json:"next_cursor,omitempty"`
	NextCursorDigest string                        `json:"next_cursor_digest,omitempty"`
	NextPosition     uint64                        `json:"next_position,omitempty"`
	// FinalizeEvidence is present only for a durable delivery that has reached
	// a terminal canonical commit without native materialisation. The signed
	// plugin must echo it through remote.inbound_finalize_v1 only after the
	// exact cloud cursor acknowledgement succeeds.
	FinalizeEvidence *RemoteInboundFinalizeEvidenceV1 `json:"finalize_evidence,omitempty"`
}

// RemoteInboundFinalizeEvidenceV1 is content-free, exact evidence for the
// split durable receive transaction. It binds the remote process identity,
// stream cursor, immutable wire event, and the canonical event that was
// durably present when the daemon emitted its terminal inbound ACK.
//
// The plugin treats every field as opaque and echoes it byte-for-byte after
// cloud ACK. It must never synthesize or partially populate this structure.
type RemoteInboundFinalizeEvidenceV1 struct {
	ProtocolVersion         uint16 `json:"protocol_version"`
	FinalizeKind            string `json:"finalize_kind"`
	RemoteIdentity          string `json:"remote_identity"`
	DeliveryID              string `json:"delivery_id"`
	StreamID                string `json:"stream_id"`
	StreamEpoch             string `json:"stream_epoch"`
	Cursor                  string `json:"cursor"`
	CursorDigest            string `json:"cursor_digest"`
	Position                uint64 `json:"position"`
	NamespaceID             string `json:"namespace_id,omitempty"`
	BranchID                string `json:"branch_id,omitempty"`
	Kind                    string `json:"kind"`
	ArtifactID              string `json:"artifact_id"`
	WireEventID             string `json:"wire_event_id"`
	WireEventHash           string `json:"wire_event_hash,omitempty"`
	BodyDigest              string `json:"body_digest"`
	ParentHash              string `json:"parent_hash,omitempty"`
	CheckpointAlignmentHash string `json:"checkpoint_alignment_hash,omitempty"`
	EventType               string `json:"event_type"`
	TimestampUnixNano       int64  `json:"timestamp_unix_nano"`
	Sequence                uint64 `json:"sequence"`
	Origin                  string `json:"origin"`
	SourceAgent             string `json:"source_agent,omitempty"`
	Lane                    string `json:"lane,omitempty"`
	Clear                   bool   `json:"clear,omitempty"`

	CanonicalEventID string `json:"canonical_event_id,omitempty"`
	CanonicalHash    string `json:"canonical_hash,omitempty"`

	NoopReason                  string `json:"noop_reason,omitempty"`
	AuthenticatedHeaderDigest   string `json:"authenticated_header_digest,omitempty"`
	AuthenticatedSignerIdentity string `json:"authenticated_signer_identity,omitempty"`

	// Batch fields are content-free aggregate evidence. The materialization
	// plan contains only artifact and final canonical event identities.
	BatchEventCount            uint16 `json:"batch_event_count,omitempty"`
	BatchDigest                string `json:"batch_digest,omitempty"`
	BatchResultDigest          string `json:"batch_result_digest,omitempty"`
	BatchMaterializationPlan   string `json:"batch_materialization_plan,omitempty"`
	BatchMaterializationDigest string `json:"batch_materialization_digest,omitempty"`
	CheckpointCoveragePlan     string `json:"checkpoint_coverage_plan,omitempty"`
	CheckpointCoverageDigest   string `json:"checkpoint_coverage_digest,omitempty"`
}

const (
	InboundFinalizeCanonicalMaterialize   = "canonical_materialize"
	InboundFinalizeCheckpointCovered      = "checkpoint_covered_materialize"
	InboundFinalizeAuthenticatedNoop      = "authenticated_noop"
	InboundFinalizeCanonicalBatch         = "canonical_batch_materialize"
	InboundFinalizeAuthenticatedBatchNoop = "authenticated_batch_noop"
	InboundFinalizeNoopNotRecipient       = "not_recipient"
)

type RemoteInboundFinalizeV1Params struct {
	Evidence RemoteInboundFinalizeEvidenceV1 `json:"evidence"`
}

// RemoteInboundFinalizeV1Result distinguishes a first materialisation from an
// exact idempotent retry. Accepted=false is fail-closed and ReasonCode is a
// bounded, content-free machine code; JSON-RPC transport errors remain
// reserved for malformed frames or a disconnected daemon.
type RemoteInboundFinalizeV1Result struct {
	Accepted         bool   `json:"accepted"`
	Materialized     bool   `json:"materialized,omitempty"`
	NoopFinalized    bool   `json:"noop_finalized,omitempty"`
	AlreadyFinalized bool   `json:"already_finalized,omitempty"`
	ReasonCode       string `json:"reason_code,omitempty"`
}

// RemoteEnumerateParams — daemon asks for the per-artifact, per-
// branch event manifest the plugin knows about. Used at startup to
// discover the namespaces the remote stores for this account and to
// reconcile after a long offline period.
type RemoteEnumerateParams struct {
	NamespaceID string `json:"namespace_id,omitempty"` // empty = list all namespaces this device has access to
}

// RemoteEnumerateResult — branch tips per namespace + artifact.
type RemoteEnumerateResult struct {
	Namespaces []RemoteNamespaceManifest `json:"namespaces"`
}

// RemoteNamespaceManifest is one namespace's tip table.
type RemoteNamespaceManifest struct {
	NamespaceID string                   `json:"namespace_id"`
	Branches    []RemoteBranchManifest   `json:"branches"`
	Artifacts   []RemoteArtifactManifest `json:"artifacts,omitempty"` // optional detail
}

// RemoteBranchManifest gives the latest event ID per branch within a
// namespace. Daemon uses (Since=BranchManifest.TipEventID) on the next
// fetch to walk forward.
type RemoteBranchManifest struct {
	BranchID   string    `json:"branch_id"`
	TipEventID string    `json:"tip_event_id"`
	EventCount uint64    `json:"event_count"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// RemoteArtifactManifest is the per-artifact tip view. Optional in
// EnumerateResult; plugins MAY omit it when the namespace is large
// and per-branch tips are sufficient.
type RemoteArtifactManifest struct {
	ArtifactID string    `json:"artifact_id"`
	BranchID   string    `json:"branch_id"`
	TipEventID string    `json:"tip_event_id"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// RemoteSubscribeParams — daemon expresses interest in inbound events
// for a namespace. Triggers the plugin to open subscription channels
// (e.g. MQTT topic subscription, S3 polling for BYO transports, …).
//
// Plugins MUST emit `remote.inbound` notifications for events that
// arrive on subscribed namespaces. The daemon is the source of truth
// for "what should I be subscribed to right now"; the plugin merely
// honors current subscriptions.
type RemoteSubscribeParams struct {
	NamespaceID string `json:"namespace_id"`
}

// RemoteUnsubscribeParams — daemon withdraws interest. Plugin tears
// down the underlying subscription.
type RemoteUnsubscribeParams struct {
	NamespaceID string `json:"namespace_id"`
}

// RemoteStatusResult — daemon polls this on a slow cadence (default
// 30s) to expose connectivity + per-namespace counters to the local
// /api/daemon REST surface and the tray indicator.
type RemoteStatusResult struct {
	ConnState           string                     `json:"conn_state"` // "connected" | "connecting" | "disconnected" | "paired_but_idle" | "unpaired"
	LastConnAttempt     time.Time                  `json:"last_conn_attempt,omitzero"`
	LastSuccessfulSync  time.Time                  `json:"last_successful_sync,omitzero"`
	PendingOutbound     uint64                     `json:"pending_outbound"`
	BytesUp             uint64                     `json:"bytes_up"`
	BytesDown           uint64                     `json:"bytes_down"`
	PerNamespace        map[string]NamespaceStatus `json:"per_namespace,omitempty"`
	HumanReadableStatus string                     `json:"human_status,omitempty"`
	SyncEvidence        *RemoteSyncEvidenceV1      `json:"sync_evidence,omitempty"`
}

// RemoteSyncEvidenceV1 is the signed plugin's bounded, content-free view of
// server and local durable-sync convergence. Cursor tokens are never exposed;
// only their SHA-256 digests and authenticated positions cross this RPC.
type RemoteSyncEvidenceV1 struct {
	SchemaVersion uint16                       `json:"schema_version"`
	SelectedMode  string                       `json:"selected_mode"`
	Complete      bool                         `json:"complete"`
	CollectedAt   time.Time                    `json:"collected_at"`
	Streams       []RemoteSyncStreamEvidenceV1 `json:"streams,omitempty"`
	Outbound      RemoteOutboundEvidenceV1     `json:"outbound"`
}

type RemoteSyncStreamEvidenceV1 struct {
	StreamID                    string `json:"stream_id"`
	StreamEpoch                 string `json:"stream_epoch"`
	ServerMode                  string `json:"server_mode"`
	ServerTipPosition           uint64 `json:"server_tip_position"`
	ServerTipCursorDigest       string `json:"server_tip_cursor_digest,omitempty"`
	ServerDevicePosition        uint64 `json:"server_device_position"`
	ServerLagEvents             uint64 `json:"server_lag_events"`
	LocalCursorPresent          bool   `json:"local_cursor_present"`
	LocalCursorPosition         uint64 `json:"local_cursor_position"`
	LocalCursorDigest           string `json:"local_cursor_digest,omitempty"`
	CursorAndHeadConverged      bool   `json:"cursor_and_head_converged"`
	BootstrapPending            bool   `json:"bootstrap_pending"`
	CheckpointPolicies          uint64 `json:"checkpoint_policies"`
	CheckpointAnchors           uint64 `json:"checkpoint_anchors"`
	CheckpointRequired          uint64 `json:"checkpoint_required"`
	CheckpointReady             uint64 `json:"checkpoint_ready"`
	CheckpointReadinessComplete bool   `json:"checkpoint_readiness_complete"`
}

type RemoteOutboundEvidenceV1 struct {
	DeltaCommitted          uint64    `json:"delta_committed,omitempty"`
	RetainedSuppressed      uint64    `json:"retained_suppressed,omitempty"`
	CheckpointCommitted     uint64    `json:"checkpoint_committed,omitempty"`
	RetainedLegacyFallbacks uint64    `json:"retained_legacy_fallbacks,omitempty"`
	UpdatedAt               time.Time `json:"updated_at,omitzero"`
}

// NamespaceStatus is the per-namespace connectivity overlay.
type NamespaceStatus struct {
	Subscribed          bool      `json:"subscribed"`
	LastInbound         time.Time `json:"last_inbound,omitzero"`
	LastOutbound        time.Time `json:"last_outbound,omitzero"`
	PendingOutbound     uint64    `json:"pending_outbound"`
	UnresolvedConflicts uint64    `json:"unresolved_conflicts,omitempty"`
}

// ---------------------------------------------------------------------------
// Inbound notifications (plugin -> daemon).
// ---------------------------------------------------------------------------

// RemoteInboundNotification — plugin delivers one or more events that
// arrived from the remote. Daemon's sync orchestrator dedupes against
// the local event log (same EventID is a no-op), then runs the normal
// import + fan-out pipeline.
type RemoteInboundNotification struct {
	Events []RemoteEvent `json:"events"`
}

// RemoteConnStateNotification — plugin asynchronously reports
// connectivity transitions so the daemon's `/api/daemon` status
// surface and the tray indicator can paint live. Emit on every
// connect/disconnect transition; suppress duplicates.
type RemoteConnStateNotification struct {
	ConnState           string    `json:"conn_state"`
	Since               time.Time `json:"since"`
	HumanReadableStatus string    `json:"human_status,omitempty"`
}

// RemoteEnumerateHintNotification — plugin tells the daemon that
// remote-side state may have changed in a way that warrants
// re-enumeration (e.g. a new namespace was provisioned via the
// account portal; an out-of-band team-membership change occurred).
// Daemon responds by scheduling a `remote.enumerate` call. Plugins
// SHOULD throttle: at most one hint per 5 minutes per cause.
type RemoteEnumerateHintNotification struct {
	Reason string `json:"reason"` // freeform; "namespace_added" | "namespace_removed" | "membership_changed" | …
}

// RemoteCheckpointNeededV1Notification asks an authorized daemon to create
// and publish a checkpoint for the supplied opaque artifact route. It mirrors
// the request fields needed by the daemon and adds the cloud compaction floor
// that made replay insufficient, when applicable.
type RemoteCheckpointNeededV1Notification struct {
	RequestID               string                  `json:"request_id"`
	RequestingDeviceID      string                  `json:"requesting_device_id,omitempty"`
	StreamID                string                  `json:"stream_id"`
	StreamEpoch             string                  `json:"stream_epoch"`
	NamespaceID             string                  `json:"namespace_id"`
	BranchID                string                  `json:"branch_id,omitempty"`
	ArtifactID              string                  `json:"artifact_id"`
	Kind                    string                  `json:"kind"`
	MissingParentHash       string                  `json:"missing_parent_hash,omitempty"`
	MinAvailableCursor      string                  `json:"min_available_cursor,omitempty"`
	Reason                  string                  `json:"reason"`
	CheckpointCoverage      uint64                  `json:"checkpoint_coverage"`
	CheckpointAlignmentHash string                  `json:"checkpoint_alignment_hash"`
	CheckpointGeneration    string                  `json:"checkpoint_generation"`
	AccessGeneration        uint64                  `json:"access_generation"`
	AccessSetHash           [RemoteDigestBytes]byte `json:"access_set_hash"`
	SecurityBarrierID       [RemoteDigestBytes]byte `json:"security_barrier_id"`
	SecurityGeneration      uint64                  `json:"security_generation"`
	KeyMode                 string                  `json:"key_mode"`
	KeyVersion              uint64                  `json:"key_version,omitempty"`
}

// RemoteRulesUpdateNotification — the remote pushes a complete
// selective-sync ruleset that the daemon should adopt. The daemon
// rebuilds a syncrules.Engine from Rules (validating + compiling via
// the same path the local rules.toml uses) and swaps it into the live
// orchestrator. The ruleset is REPLACE semantics — Rules is the full
// new set, not a delta.
//
// Each element of Rules unmarshals from the SAME camelCase JSON the
// local rules API and the rules.toml mirror emit (syncrules.Rule's
// json tags); there is no import cycle because syncrules depends only
// on the TOML decoder + stdlib.
//
// ChangeID is an opaque idempotency token (typically a UUID) the
// remote stamps on each push. The daemon may store the last-applied
// ChangeID to drop a redelivered notification, but applying the same
// ruleset twice is harmless (engine rebuild is deterministic).
type RemoteRulesUpdateNotification struct {
	ChangeID string           `json:"change_id"`
	Rules    []syncrules.Rule `json:"rules"`
}

// ---------------------------------------------------------------------------
// Namespace key rotation.
//
// All key material on this surface is OPAQUE wrapped bytes — the control
// plane and plugin never see plaintext content keys (zero-knowledge
// invariant). The daemon generates, wraps, and unwraps locally.
// ---------------------------------------------------------------------------

// RemoteWrappedKey is a content key wrapped for one device's public key.
// Wrapped is raw bytes; encoding/json renders it base64 on the wire.
type RemoteWrappedKey struct {
	DeviceID string `json:"device_id"`
	Wrapped  []byte `json:"wrapped"`
}

// RemoteDevice is one surviving member device and its registered X25519
// public key (raw 32 bytes; base64 on the wire).
type RemoteDevice struct {
	DeviceID string `json:"device_id"`
	PubKey   []byte `json:"pubkey"`
}

// RemoteListNamespaceDevicesParams — daemon asks the plugin for the
// namespace's surviving member devices and their wrap keys. Called after a
// rotation signal; the plugin's list already excludes the removed member.
type RemoteListNamespaceDevicesParams struct {
	NamespaceID string `json:"namespace_id"`
}

// RemoteListNamespaceDevicesResult — surviving devices for the namespace.
type RemoteListNamespaceDevicesResult struct {
	Devices []RemoteDevice `json:"devices"`
}

// RemoteRegisterWrapKeyParams — daemon registers THIS device's X25519 wrap
// public key with the control plane (account-scoped; identified server-side by
// the device proof). Lets a device that paired before it had a wrap key — or
// whose key was never persisted server-side — become a valid encryption
// recipient WITHOUT re-pairing. WrapPubKey is the raw 32-byte X25519 public
// key; encoding/json renders it base64-std on the wire (matching the plugin).
type RemoteRegisterWrapKeyParams struct {
	WrapPubKey []byte `json:"wrap_pubkey"`
}

// RemoteAccountDevice is one active device in the caller's account that has a
// registered wrap public key. PubKey is the raw 32-byte X25519 key (base64-std
// on the wire). Mirrors RemoteDevice but is sourced account-scoped rather than
// namespace-scoped.
type RemoteAccountDevice struct {
	DeviceID string `json:"device_id"`
	PubKey   []byte `json:"pubkey"`
}

// RemoteListAccountDevicesResult — every active device in the caller's account
// with a registered wrap pubkey (INCLUDES the calling device). The daemon seals
// each outbound event to all of them, so the recipient set is resolvable
// without any namespace id (the Personal-tier blocker). The request carries no
// params — the account is resolved server-side from the device proof.
type RemoteListAccountDevicesResult struct {
	Devices []RemoteAccountDevice `json:"devices"`
}

// The two source values a remote.envelope_caps answer may carry. FROZEN
// cross-repo contract with the cloud plugin: "entitlement" means the answer
// was read from the plugin's cached entitlement; "absent" means the
// entitlement (or its envelope_caps block) was missing or any error occurred.
const (
	RemoteEnvelopeCapsSourceEntitlement = "entitlement"
	RemoteEnvelopeCapsSourceAbsent      = "absent"
)

// RemoteEnvelopeCapsResult — the server-asserted account-level envelope
// capability switch (2026-07-29 envelope wire-efficiency ADR D3). FROZEN
// cross-repo contract: the request carries no params; v3_enabled is true ONLY
// when the plugin's cached entitlement carries envelope_caps.v3_enabled=true
// (source "entitlement"). A missing entitlement, a missing envelope_caps
// block, or any error MUST answer {v3_enabled:false, source:"absent"} —
// fail-closed always. The daemon additionally treats a transport error, an
// unknown method (older plugin), or a non-"entitlement" source as disabled.
type RemoteEnvelopeCapsResult struct {
	V3Enabled bool   `json:"v3_enabled"`
	Source    string `json:"source"`
}

// RemotePutNamespaceKeyParams — daemon conditionally writes the wrapped key
// material for a new key version back to the namespace_keys row (populating
// the server's WrappedForDevicePubkeyIDs without exposing plaintext). The
// write is a compare-and-swap: it only succeeds if the version is still
// unclaimed (empty WrappedForDevicePubkeyIDs).
type RemotePutNamespaceKeyParams struct {
	NamespaceID string             `json:"namespace_id"`
	KeyVersion  int                `json:"key_version"`
	Wrapped     []RemoteWrappedKey `json:"wrapped"`
}

// RemotePutNamespaceKeyResult — outcome of the conditional write. Claimed
// is true when THIS write won the compare-and-swap; false when another
// device had already populated the version (the daemon then adopts theirs).
type RemotePutNamespaceKeyResult struct {
	Claimed bool `json:"claimed"`
}

// RemoteGetNamespaceKeyParams — daemon reads back the wrapped key material
// already written for a version, to adopt the winner's key after losing the
// compare-and-swap.
type RemoteGetNamespaceKeyParams struct {
	NamespaceID string `json:"namespace_id"`
	KeyVersion  int    `json:"key_version"`
}

// RemoteGetNamespaceKeyResult — the wrapped set for a version. Found is
// false when no write has landed for the version yet.
type RemoteGetNamespaceKeyResult struct {
	Found   bool               `json:"found"`
	Wrapped []RemoteWrappedKey `json:"wrapped"`
}

// RemoteBroadcastNamespaceKeyParams — daemon asks the plugin to push the
// wrapped key material to surviving devices over the live transport.
type RemoteBroadcastNamespaceKeyParams struct {
	NamespaceID string             `json:"namespace_id"`
	KeyVersion  int                `json:"key_version"`
	Wrapped     []RemoteWrappedKey `json:"wrapped"`
}

// RemoteNamespaceKeyRotatedNotification mirrors the control plane's
// namespace.key_rotated audit payload field-for-field.
type RemoteNamespaceKeyRotatedNotification struct {
	NamespaceID   string `json:"namespace_id"`
	NewVersion    int    `json:"new_version"`
	RemovedUserID string `json:"removed_user_id"`
}

// RemoteNamespaceKeyBroadcastNotification carries wrapped key material
// pushed to surviving devices. Each device picks the blob addressed to it.
type RemoteNamespaceKeyBroadcastNotification struct {
	NamespaceID string             `json:"namespace_id"`
	KeyVersion  int                `json:"key_version"`
	Wrapped     []RemoteWrappedKey `json:"wrapped"`
}

// ---------------------------------------------------------------------------
// Client-side RBAC.
//
// The daemon mirrors the server's per-namespace capability matrix locally so
// it can refuse team operations the caller's role does not permit BEFORE a
// round-trip — defense-in-depth, with the server remaining authoritative.
// The role is plaintext authorization metadata, NOT key material: nothing on
// this surface touches the zero-knowledge content-key path.
// ---------------------------------------------------------------------------

// RemoteGetNamespaceRoleParams — daemon asks the plugin for the caller's
// role in a namespace. The plugin's authenticated transport identifies the
// caller server-side (as with the device-listing call), so only the
// namespace id is sent.
type RemoteGetNamespaceRoleParams struct {
	NamespaceID string `json:"namespace_id"`
}

// RemoteGetNamespaceRoleResult — the resolved role for the caller. Role is
// one of the canonical role strings (owner/admin/editor/contributor/reader).
// Found is false when the caller holds no membership in the namespace; the
// daemon then treats the caller as having no capabilities (deny-by-default).
type RemoteGetNamespaceRoleResult struct {
	Role  string `json:"role"`
	Found bool   `json:"found"`
}

// RemoteOpaqueSignedObject is structural routing metadata plus one canonical
// signed client object. Neither the plugin nor the service may synthesize or
// alter Blob. Hash is SHA-256(Blob) and is independently checked by clients.
type RemoteOpaqueSignedObject struct {
	ScopeType    string                  `json:"scope_type"`
	ScopeID      string                  `json:"scope_id"`
	Kind         string                  `json:"kind"`
	Sequence     uint64                  `json:"sequence"`
	PreviousHash [RemoteDigestBytes]byte `json:"previous_hash"`
	Hash         [RemoteDigestBytes]byte `json:"hash"`
	Blob         []byte                  `json:"blob"`
	ProofBlob    []byte                  `json:"proof_blob,omitempty"`
}

type RemoteGetTrustAnchorResult struct {
	Found  bool                     `json:"found"`
	Object RemoteOpaqueSignedObject `json:"object"`
}

type RemoteSubmitTrustAnchorResult struct {
	ObjectHash [RemoteDigestBytes]byte `json:"object_hash"`
	Duplicate  bool                    `json:"duplicate"`
}

type RemoteGetServiceTrustConfigResult struct {
	Blob []byte `json:"blob"`
}

type RemoteGetAuthorityTransitionsParams struct {
	FromExclusive uint64 `json:"from_exclusive"`
}
type RemoteGetAuthorityTransitionsResult struct {
	Objects []RemoteOpaqueSignedObject `json:"objects"`
}

type RemoteSubmitSignedObjectParams struct {
	Object RemoteOpaqueSignedObject `json:"object"`
}

type RemoteGetSignedRosterParams struct {
	ScopeType string `json:"scope_type"`
	ScopeID   string `json:"scope_id"`
	Epoch     uint64 `json:"epoch,omitempty"`
}
type RemoteGetSignedRosterResult struct {
	Found  bool                     `json:"found"`
	Object RemoteOpaqueSignedObject `json:"object"`
}

type RemoteRegisterDeviceCredentialParams struct {
	CredentialBlob   []byte                  `json:"credential_blob"`
	SigningKeyID     [RemoteDigestBytes]byte `json:"signing_key_id"`
	WrapKeyID        [RemoteDigestBytes]byte `json:"wrap_key_id"`
	EnvelopeVersions []uint16                `json:"envelope_versions"`
	RosterEpoch      uint64                  `json:"roster_epoch"`
}

type RemoteActivateSyncGenerationParams struct {
	AttestationBlob []byte `json:"attestation_blob"`
}

type RemoteActivateSyncGenerationResult struct {
	AuthorityDigest string `json:"authority_digest"`
	Revision        uint64 `json:"revision"`
	Duplicate       bool   `json:"duplicate"`
}

type RemoteGetSyncGenerationStatusParams struct {
	AttestationBlob []byte `json:"attestation_blob"`
}

type RemoteGetSyncGenerationStatusResult struct {
	Status          string `json:"status"`
	AuthorityDigest string `json:"authority_digest,omitempty"`
	Revision        uint64 `json:"revision,omitempty"`
	Duplicate       bool   `json:"duplicate,omitempty"`
}

type RemoteGetRosterConsistencyParams struct {
	OldTreeSize uint64 `json:"old_tree_size"`
	LeafIndex   uint64 `json:"leaf_index"`
}
type RemoteGetRosterConsistencyResult struct {
	ProofBlob []byte `json:"proof_blob"`
}

// Device transition plans are opaque to the plugin/service and contain only
// client-signed public roster metadata plus wrapped ciphertext. Sequence is
// the signed next roster epoch; PreviousHash binds the immediate predecessor
// roster, not a server-created plan chain.
type RemoteSubmitDeviceTransitionPlanParams struct {
	Object RemoteOpaqueSignedObject `json:"object"`
}

type RemoteGetDeviceTransitionPlansParams struct {
	ScopeID       string `json:"scope_id"`
	FromExclusive uint64 `json:"from_exclusive"`
}

type RemoteGetDeviceTransitionPlansResult struct {
	Objects []RemoteOpaqueSignedObject `json:"objects"`
}

type RemoteSecurityEpoch struct {
	Generation       uint64                  `json:"generation"`
	AccessGeneration uint64                  `json:"access_generation"`
	AccessSetHash    [RemoteDigestBytes]byte `json:"access_set_hash"`
	BarrierID        [RemoteDigestBytes]byte `json:"barrier_id"`
	KeyMode          string                  `json:"key_mode"`
	KeyVersion       uint64                  `json:"key_version"`
}
type RemoteSecurityEpochPrepareParams struct {
	ScopeID           string                  `json:"scope_id"`
	BarrierID         [RemoteDigestBytes]byte `json:"barrier_id"`
	Current           RemoteSecurityEpoch     `json:"current"`
	Next              RemoteSecurityEpoch     `json:"next"`
	StagedPackageHash [RemoteDigestBytes]byte `json:"staged_package_hash"`
}
type RemoteSecurityEpochCommandParams struct {
	ScopeID   string                  `json:"scope_id"`
	BarrierID [RemoteDigestBytes]byte `json:"barrier_id"`
}
type RemoteSecurityEpochStatusResult struct {
	ScopeID           string                  `json:"scope_id"`
	Phase             string                  `json:"phase"`
	Current           RemoteSecurityEpoch     `json:"current"`
	Next              RemoteSecurityEpoch     `json:"next"`
	StagedPackageHash [RemoteDigestBytes]byte `json:"staged_package_hash"`
}
