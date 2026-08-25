package syncd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Cloud artifact sync — the orchestrator's two-way bridge to a remote
// transport plugin (Aplexica Cloud / self-hosted relay / BYO transport).
//
// OUTBOUND: handleEvent calls forwardCommitted after a successful commit +
// fanOut to hand the just-committed event to the RemoteEventPublisher (the
// daemon's RemoteRunner), which translates it to a proto.RemoteEvent and
// queues it for transmission.
//
// INBOUND: the RemoteRunner's OnInbound callback is wired to ImportInbound,
// which decrypts each proto.RemoteEvent's opaque Bytes envelope back into a
// canonical acf.Event and appends it to the local store with the remote device
// recorded as the event's origin. That origin is added to o.remoteOrigins so
// the OUTBOUND path never bounces the event back to the relay (loop prevention).
//
// ZERO-KNOWLEDGE: OutboundEvent.Bytes is a per-event end-to-end encrypted
// envelope (see envelope.go) — fresh content key per event, AES-GCM-256 body,
// X25519-wrapped per recipient device. The cloud plugin treats Bytes as opaque
// and publishes it verbatim, so ONLY ciphertext + opaque routing metadata reach
// the relay. The recipient set is resolved via Config.RecipientResolver (the
// daemon backs it with ListNamespaceDevices, ALWAYS including this device).
// If the recipient set is EMPTY, the outbound event is DROPPED (logged) —
// the daemon NEVER transmits plaintext. Inbound, this device's private wrap key
// (Config.DeviceKeyProvider) opens the envelope; an envelope not addressed to
// this device is skipped.
// ---------------------------------------------------------------------------

// forwardCommitted reads the head event of the freshly-committed artifact and,
// unless that event was authored on another (remote) device, hands it to the
// RemoteEventPublisher for outbound transmission. Conversations publish on TWO
// lanes per commit (aligned-chains design rule 5): the verbatim head event on
// lane=live (unless its sealed size exceeds remotePublishLiveMaxBytes) and the
// full materialized state with alignment metadata on lane=retained. Every
// other kind keeps the single lane=live full-state publish. Returns true when
// at least one lane published.
//
// Loop prevention: handleEvent reaches here for events produced by the local
// import pipeline (a user/agent edit, OR a re-import of a native file that
// fan-out materialised). A remote-authored event carries a provenance device
// id this orchestrator recorded in o.remoteOrigins when it arrived over
// ImportInbound; forwardCommitted skips those so an inbound event can never be
// re-published back to the relay. (The structural guard — remote imports never
// pass through handleEvent, and fan-out echoes are recursion-guard-suppressed —
// already prevents the common loop; this origin check defends the residual
// path where a remote event is re-materialised to a native file and re-imported
// here with its provenance device id preserved by the source adapter.)
//
// Best-effort and non-blocking: any read error or a missing publisher is a
// silent skip — the next remote.fetch cycle reconciles. The publisher itself
// MUST enqueue without blocking (see RemoteEventPublisher's contract).
func (o *Orchestrator) forwardCommitted(artifactID string) bool {
	pub := o.remoteEventPublisher()
	if pub == nil {
		return false
	}
	art, found := o.findArtifact(artifactID)
	if !found {
		return false
	}
	projectEntry, projectAuthorized := o.remoteProjectAuthorization(art)
	if !projectAuthorized {
		// A revoked/unlinked project is no longer an outbound authority. Reject
		// it before opening its event log: recent-head and backfill sweeps may
		// encounter legacy artifacts for a removed project, and repeatedly
		// reading their conversation tails can otherwise become a CPU loop.
		o.publishEvent("remote.outbound_dropped", map[string]any{
			"artifact_id": artifactID,
			"reason":      "project authorization revoked",
		})
		return false
	}
	head, ok, err := o.cfg.Store.LastEvent(art.Kind, artifactID)
	if err != nil || !ok {
		return false
	}
	// Loop prevention: do not forward an event that originated on a remote
	// device (i.e. one we imported over ImportInbound).
	if o.isRemoteOrigin(head.Provenance.DeviceID) {
		return false
	}
	// AppendEvent maintains the persistent per-artifact counter alongside the
	// head metadata. Use that O(1) value here: EventCount walks the complete
	// JSONL log, which turns every outbound turn of a long-running conversation
	// into a multi-gigabyte read and can monopolize several CPU cores. Legacy
	// artifacts without the counter take the old scan once so their existing
	// transport sequence does not move backwards; their next real append writes
	// the persistent counter and leaves the hot path permanently.
	sequence := art.EventCount
	if sequence == 0 {
		var err error
		sequence, err = o.cfg.Store.EventCount(art.Kind, artifactID)
		if err != nil || sequence == 0 {
			return false
		}
	}

	// Honor route.remote="exclude" (BRD-05 §5.3 / FR-05.9): an artifact a rule
	// marks remote-excluded must NEVER be propagated via a transport plugin,
	// regardless of other rules. fanOut only governs LOCAL materialization, so
	// the outbound transport path must consult the rules engine itself. Use the
	// SAME projection fanOut uses (ruleInputFor) so the two decisions can't
	// drift. The mutex-guarded o.rulesEngine() avoids the hot-reload race.
	if eng := o.rulesEngine(); eng != nil {
		adapterNames := make([]string, 0, len(o.cfg.Adapters))
		for _, ad := range o.cfg.Adapters {
			adapterNames = append(adapterNames, ad.Name())
		}
		decision := eng.Evaluate(ruleInputFor(art, head.Provenance.SourceAgent, head.Branch), syncrules.EvaluateOpts{
			InstalledAgents: adapterNames,
		})
		if !decision.RemoteAllowed {
			o.publishEvent("remote.outbound_dropped", map[string]any{
				"artifact_id": artifactID,
				"event_id":    head.EventID,
				"reason":      "route.remote=exclude",
			})
			return false
		}
	}

	namespaceID := ""
	if art.Scope == acf.ScopeNamespace {
		namespaceID = art.NamespaceID
		if err := acf.ValidateWireUUIDv7(namespaceID); err != nil {
			o.publishEvent("remote.outbound_paused", map[string]any{"artifact_id": artifactID, "reason": "invalid authenticated namespace identity"})
			return false
		}
	}

	// Resolve the authenticated roster in v2 production mode, or the legacy
	// recipient seam used only by unit/offline compatibility callers.
	var snapshot RosterSnapshot
	var namespaceKey keyrotation.NamespaceKeySnapshot
	var v2Identity keys.DeviceIdentity
	useV2 := o.cfg.RequireEnvelopeV2
	var recipients []recipient
	if useV2 {
		provider := o.verifiedRosterProvider()
		identityProvider := o.v2IdentityProvider()
		if provider == nil || identityProvider == nil {
			o.publishEvent("remote.outbound_paused", map[string]any{"artifact_id": artifactID, "reason": "verified roster or signing identity unavailable"})
			return false
		}
		var err error
		scopeType, scopeID := "account", ""
		if art.Scope == acf.ScopeNamespace {
			scopeType, scopeID = "namespace", namespaceID
		}
		snapshot, err = provider.Current(context.Background(), scopeType, scopeID)
		if err != nil || snapshot.BarrierID == ([32]byte{}) {
			o.publishEvent("remote.outbound_paused", map[string]any{"artifact_id": artifactID, "reason": "verified roster or security barrier stale"})
			return false
		}
		if art.Scope == acf.ScopeNamespace {
			keyProvider := o.namespaceKeyProvider()
			if keyProvider == nil || snapshot.KeyMode != "namespace-key-v1" || snapshot.KeyVersion == 0 {
				return false
			}
			namespaceKey, err = keyProvider.Current(context.Background(), namespaceID)
			if err != nil || !namespaceKey.Finalized || namespaceKey.Version != snapshot.KeyVersion || namespaceKey.AccessGeneration != snapshot.Roster.Manifest.Manifest.AccessGeneration || namespaceKey.AccessSetHash != snapshot.Roster.Manifest.Manifest.AccessSetHash || namespaceKey.IssuedRosterEpoch != snapshot.Roster.Manifest.Manifest.Epoch || namespaceKey.IssuedRosterHash != [32]byte(snapshot.Roster.Hash) {
				return false
			}
		} else if snapshot.KeyMode != "recipient-wrap-v2" || snapshot.KeyVersion != 0 {
			return false
		}
		v2Identity, err = identityProvider.Identity()
		if err != nil {
			return false
		}
		for _, c := range snapshot.Roster.Manifest.Manifest.Devices {
			recipients = append(recipients, recipient{deviceID: c.Certificate.DeviceID, pub: c.Certificate.WrapPublicKey})
		}
	} else {
		var rerr error
		recipients, rerr = o.resolveRecipients(namespaceID)
		if rerr != nil || len(recipients) == 0 {
			o.publishEvent("remote.outbound_dropped", map[string]any{"artifact_id": artifactID, "event_id": head.EventID, "reason": "no recipients (refusing to send plaintext)"})
			return false
		}
	}
	// Envelope v3 seal-time format selection (2026-07-29 envelope
	// wire-efficiency ADR D1+D2+D3), decided per event against the EXACT
	// verified roster snapshot this event is about to be sealed for: v3 only
	// when every recipient device certificate advertises envelope version 3
	// AND the account-level envelope_caps switch (remote.envelope_caps plugin
	// RPC, cached + fail-closed inside the daemon's publish adapter) is on.
	// Every other combination — legacy v1 mode (no verified roster resolved
	// here, so no per-peer capability proof exists), a mixed fleet, caps off,
	// or any RPC failure — keeps exactly today's format.
	useV3 := useV2 && envelopeV3Selected(pub, snapshot.Roster)
	// Resolve the recipient device set for this namespace. The daemon's
	// resolver ALWAYS includes this device and is backed by
	// ListNamespaceDevices. ZERO-KNOWLEDGE: an EMPTY recipient set means we
	// DROP the outbound event — we NEVER transmit plaintext.
	if o.cfg.Logger != nil {
		o.cfg.Logger.Info("remote: outbound recipient set resolved",
			"artifact_id", artifactID,
			"event_id", head.EventID,
			"recipient_count", len(recipients),
			"recipient_devices", recipientDeviceIDs(recipients))
	}

	origin := o.localDeviceID()
	if origin == "" {
		// Fall back to the native adapter provenance when the daemon is not
		// paired yet, so local-only/test transports still get attribution.
		// The wire invariant is "real cloud device id or EMPTY" (an empty
		// Origin is filled with the plugin's current identity): adapter
		// provenance defaults to os.Hostname() while unpaired, and a
		// hostname must never reach the relay as a device origin — it can
		// never satisfy the cloud's publisher-identity check, and inbound
		// peers would teach their loop guards a fake device id.
		origin = o.sanitizeUnpairedOrigin(head.Provenance.DeviceID)
	}
	// eventID is per LANE: the live lane carries the head's own EventID, the
	// retained lane carries the origin-scoped RetainedWireEventID(head.EventID,
	// origin) so the daemon's durable outbox and retry bookkeeping (both keyed
	// by EventID) treat the two lanes of one commit as the two distinct
	// transport events they are — and so two devices that legacy-re-authored
	// the same head EventID never collide on one retained wire id.
	// clear marks a retained-slot CLEAR (empty sealed body — see
	// OutboundEvent.Clear).
	projectID := ""
	projectGeneration := uint64(0)
	if art.Scope == acf.ScopeProject && art.Project != nil && o.cfg.ProjectRegistry != nil {
		projectID = projectEntry.ID
		projectGeneration = projectEntry.AuthorizationGeneration
	}
	buildOutbound := func(lane, eventID, eventHash, checkpointAlignmentHash string, sealed []byte, clear bool) OutboundEvent {
		out := OutboundEvent{
			ProjectID:                      projectID,
			ProjectAuthorizationGeneration: projectGeneration,
			NamespaceID:                    namespaceID,
			BranchID:                       normalizeBranchName(head.Branch),
			ArtifactID:                     artifactID,
			EventID:                        eventID,
			ParentHash:                     head.ParentHash,
			CheckpointAlignmentHash:        checkpointAlignmentHash,
			EventHash:                      eventHash,
			Kind:                           string(art.Kind),
			Type:                           string(head.Type),
			Timestamp:                      head.Timestamp,
			Bytes:                          sealed,   // ciphertext envelope (see envelope.go); empty on a clear
			Sequence:                       sequence, // per-artifact monotonic (1-based count)
			Origin:                         origin,
			SourceAgent:                    head.Provenance.SourceAgent,
			Lane:                           lane,
			Clear:                          clear,
		}
		if useV2 {
			out.AccessGeneration = snapshot.Roster.Manifest.Manifest.AccessGeneration
			out.AccessSetHash = snapshot.Roster.Manifest.Manifest.AccessSetHash
			out.SecurityBarrierID = snapshot.BarrierID
			out.SecurityGeneration = snapshot.CoordinatorGeneration
			out.KeyMode = snapshot.KeyMode
			out.KeyVersion = snapshot.KeyVersion
		}
		return out
	}
	outbound := func(lane, eventID, eventHash, checkpointAlignmentHash string, sealed []byte, clear bool) {
		o.publishOutbound(buildOutbound(lane, eventID, eventHash, checkpointAlignmentHash, sealed, clear))
	}
	sealOne := func(ev acf.Event, lane, wireID string, clear bool) ([]byte, error) {
		if !useV2 {
			if clear {
				return nil, nil
			}
			return sealEnvelope(ev, art.Scope, art.Project, recipients)
		}
		h := NewEventHeaderV2(ev, art.Kind, namespaceID, wireID, lane, sequence, snapshot.Roster, snapshot.BarrierID)
		h.TreeHeadDigest = snapshot.TreeHeadDigest
		h.KeyMode = snapshot.KeyMode
		h.KeyVersion = snapshot.KeyVersion
		if clear {
			h.Purpose = "retained-clear"
			h.Routing.Clear = true
			h.Canonical = CanonicalMetadataV2{}
			if art.Scope == acf.ScopeNamespace {
				if useV3 {
					return SealNamespaceRetainedClearV3(h, snapshot.Roster, v2Identity, namespaceKey)
				}
				return SealNamespaceRetainedClearV2(h, snapshot.Roster, v2Identity, namespaceKey)
			}
			if useV3 {
				return SealRetainedClearV3(h, snapshot.Roster, v2Identity)
			}
			return SealRetainedClearV2(h, snapshot.Roster, v2Identity)
		}
		if art.Scope == acf.ScopeNamespace {
			if useV3 {
				return SealNamespaceEnvelopeV3(ev, art.Scope, art.Project, h, snapshot.Roster, v2Identity, namespaceKey)
			}
			return SealNamespaceEnvelopeV2(ev, art.Scope, art.Project, h, snapshot.Roster, v2Identity, namespaceKey)
		}
		if useV3 {
			return SealEnvelopeV3(ev, art.Scope, art.Project, h, snapshot.Roster, v2Identity)
		}
		return SealEnvelopeV2(ev, art.Scope, art.Project, h, snapshot.Roster, v2Identity)
	}
	dropLane := func(lane, reason string) {
		o.publishEvent("remote.outbound_dropped", map[string]any{
			"artifact_id": artifactID,
			"event_id":    head.EventID,
			"lane":        lane,
			"reason":      reason,
		})
		if o.cfg.Logger != nil {
			o.cfg.Logger.Info("remote: outbound lane dropped",
				"artifact_id", artifactID,
				"event_id", head.EventID,
				"lane", lane,
				"reason", reason)
		}
	}
	recipientsFP := recipientsFingerprint(recipients)

	// Non-conversation kinds keep the single full-state publish — the stored
	// head IS the full state for the cumulative-snapshot kinds — on the live
	// lane. (The plugin keeps duplicating non-conversation live bytes to the
	// retained catch-up topic exactly like today; only conversations get the
	// daemon-side two-lane split below.)
	if art.Kind != acf.KindConversation {
		sealed, err := sealOne(head, LaneLive, head.EventID, false)
		if err != nil {
			// errNoRecipients is already handled above; any other seal
			// failure is a hard drop (never plaintext).
			dropLane(LaneLive, "seal failed: "+err.Error())
			return false
		}
		outbound(LaneLive, head.EventID, head.Hash, "", sealed, false)
		o.markRepublishedLocalRemoteHead(artifactID, head.Branch, head.Hash, recipientsFP)
		return true
	}

	// Conversations publish on TWO lanes (aligned-chains design rule 5), so
	// the wire cost of a growing thread stays O(new turn) on the live lane
	// while the retained lane keeps a full-state recovery point.
	published := false
	retainedHandled := false

	// Lane 1 — live: the stored head event VERBATIM (typically a compact
	// ConversationDeltaFormatV1 delta). A receiver whose head bookkeeping
	// matches ParentHash appends it natively and — by acf.ComputeHash
	// determinism — recomputes the identical hash, keeping the chains
	// aligned. A head whose sealed size exceeds the live cap (a giant
	// create/full-state event) skips the live lane entirely: the daemon
	// would only dead-letter it, and the retained lane below carries the
	// same state.
	liveSealed, err := sealOne(head, LaneLive, head.EventID, false)
	switch {
	case err != nil:
		dropLane(LaneLive, "seal failed: "+err.Error())
	case len(liveSealed) > remotePublishLiveMaxBytes:
		if o.cfg.Logger != nil {
			o.cfg.Logger.Info("remote: live lane skipped (sealed size over cap)",
				"artifact_id", artifactID,
				"event_id", head.EventID,
				"sealed_bytes", len(liveSealed),
				"cap_bytes", remotePublishLiveMaxBytes)
		}
	default:
		outbound(LaneLive, head.EventID, head.Hash, "", liveSealed, false)
		published = true
	}

	// A lane=retained baseline is a full-state recovery point. Rebuilding,
	// compressing, and encrypting it after every tiny live delta is wasteful for
	// a 100+ MB native transcript and was the dominant active CPU spike. Keep
	// lane=live immediate, but refresh a very large retained point at a bounded
	// cadence. Roster changes and redactions bypass the cadence so new recipients
	// can decrypt promptly and removed content is cleared immediately.
	if o.deferLargeRetainedBaseline(art, head, recipientsFP) {
		if published {
			// A prior call for this exact head already reserved (and began) the
			// expensive retained rebuild. Record the successful live publication
			// so the one-minute safety sweep does not keep resealing the same head
			// while that retained attempt is in flight or backing off. A failed
			// retained attempt becomes eligible again at its bounded cadence via
			// shouldRepublishLocalRemoteHead.
			o.markRepublishedLocalRemoteHead(artifactID, head.Branch, art.HeadEventHash, recipientsFP)
		}
		return published
	}

	// Lane 2 — retained: normally attempted after every committed head. The
	// full materialized state is stamped with AlignedHead/AlignedEventID, so a
	// receiver that missed every prior event can adopt it as an aligned baseline
	// (acf.Store.AdoptBaseline). Very large native conversations take the
	// bounded cadence above; their live delta publish never waits for it.
	retained, ok, redacted, err := o.retainedConversationEvent(art, head)
	switch {
	case err != nil:
		dropLane(LaneRetained, "materialize retained conversation state failed: "+err.Error())
		o.recordRetainedBaselineFailure(art, head, recipientsFP)
	case !ok && redacted:
		// Redaction-terminated log: there is no full state to retain — and
		// the transport's retained slot STILL SERVES the last pre-redaction
		// snapshot, which a newly-paired or long-offline device would adopt
		// as a baseline, resurrecting redacted content. Publish a retained-
		// slot CLEAR (empty body, Clear=true — the plugin maps it to an MQTT
		// retained-clear publish). The verbatim live redaction above still
		// carries the redaction itself to connected peers.
		o.clearRetainedOversized(artifactID, head.Branch)
		wireID := RetainedWireEventID(head.EventID, origin)
		clearEnvelope, clearErr := sealOne(head, LaneRetained, wireID, true)
		if clearErr != nil {
			dropLane(LaneRetained, "seal retained clear failed: "+clearErr.Error())
		} else {
			outbound(LaneRetained, wireID, "", "", clearEnvelope, true)
			published = true
		}
		retainedHandled = true
	case !ok:
		// A log that never carried a materializable payload: nothing was
		// ever retained, so there is nothing to clear. The verbatim live
		// event above still ships.
		retainedHandled = true
	default:
		retainedSealed, serr := sealOne(retained, LaneRetained, RetainedWireEventID(head.EventID, origin), false)
		switch {
		case serr != nil:
			dropLane(LaneRetained, "seal failed: "+serr.Error())
			o.recordRetainedBaselineFailure(art, head, recipientsFP)
		case len(retainedSealed) > remotePublishRetainedMaxBytes:
			large := buildOutbound(LaneRetained, RetainedWireEventID(head.EventID, origin), retained.Hash, retained.AlignedHead, retainedSealed, false)
			if staged, ok := pub.(LargeRetainedCheckpointPublisher); ok && staged.SupportsLargeRetainedCheckpoint(large) {
				o.clearRetainedOversized(artifactID, head.Branch)
				o.publishOutbound(large)
				published = true
				retainedHandled = true
				break
			}
			// Compatibility residual, handled honestly: when the exact signed
			// plugin/server pair does not advertise the bounded staged transfer,
			// a baseline this large has no transport path. Refuse it at the
			// source and mark the artifact so the republish sweep/backfill
			// trickle stop re-materializing + re-sealing it until the head
			// changes (shouldRepublishLocalRemoteHead /
			// shouldBackfillLocalRemoteHead), and surface the condition:
			// retained_too_large tells the status surface that peers missing
			// a live delta have NO recovery path for this artifact until its
			// materialized state shrinks below the cap. Ids and sizes only —
			// zero-knowledge.
			//
			// Key the mark on art.HeadEventHash — the SAME field
			// shouldRepublishLocalRemoteHead / shouldBackfillLocalRemoteHead
			// compare against — so writer and readers agree by construction.
			// (It equals head.Hash for every non-baseline local head, and
			// forwardCommitted refuses remote-origin baseline heads before this
			// branch, but keying both sides on one field avoids relying on that
			// implicit invariant.)
			o.markRetainedOversized(artifactID, head.Branch, art.HeadEventHash)
			o.publishEvent("remote.outbound_oversized", map[string]any{
				"artifact_id":        artifactID,
				"event_id":           RetainedWireEventID(head.EventID, origin),
				"lane":               LaneRetained,
				"bytes":              len(retainedSealed),
				"limit":              remotePublishRetainedMaxBytes,
				"retained_too_large": true,
			})
		default:
			o.clearRetainedOversized(artifactID, head.Branch)
			outbound(LaneRetained, RetainedWireEventID(head.EventID, origin), retained.Hash, retained.AlignedHead, retainedSealed, false)
			published = true
			retainedHandled = true
		}
	}

	if retainedHandled {
		o.completeLargeRetainedBaseline(artifactID, head.Branch)
		// Baseline adoption intentionally makes the artifact bookkeeping head
		// (the origin's AlignedHead) differ from the local baseline wrapper's
		// Hash. RepublishLocalRemoteHeads compares against art.HeadEventHash,
		// so recording head.Hash here made every recovered conversation look
		// changed on every one-minute safety sweep forever.
		o.markRepublishedLocalRemoteHead(artifactID, head.Branch, art.HeadEventHash, recipientsFP)
	}
	return published
}

// sanitizeUnpairedOrigin gates the unpaired-daemon origin fallback: only a
// UUID-shaped provenance device id (a real cloud identity, e.g. from an
// artifact that round-tripped through a paired peer) may ride the wire as
// Origin. Anything else — most commonly the adapters' os.Hostname() default
// (for example, "test-host.localdomain") —
// is blanked so the plugin stamps its own current identity instead. Warned
// once per process; the same unpaired provenance would otherwise log on
// every event.
func (o *Orchestrator) sanitizeUnpairedOrigin(origin string) string {
	if origin == "" || isUUIDDeviceID(origin) {
		return origin
	}
	if o.originLeakWarned.CompareAndSwap(false, true) && o.cfg.Logger != nil {
		o.cfg.Logger.Warn("remote: blanking non-cloud outbound origin; plugin will stamp its current identity",
			"origin", origin)
	}
	return ""
}

// isUUIDDeviceID reports whether s is a canonical hyphenated UUID — the shape
// of every cloud-assigned device id. Version is deliberately not pinned; only
// the exact lowercase 8-4-4-4-12 round-trip is required.
func isUUIDDeviceID(s string) bool {
	parsed, err := uuid.Parse(s)
	return err == nil && parsed.String() == s
}

// defaultRemotePublishLiveMaxBytes caps the SEALED size of a lane=live
// conversation event. It mirrors internal/daemon's remotePublishMaxEventBytes
// (4 MB): a live event above the daemon's per-event cap would only be
// dead-lettered downstream, so forwardCommitted skips the live lane entirely
// for a giant create/full-state head — the always-published retained lane
// (shipped solo with a far larger cap) carries the state instead.
const defaultRemotePublishLiveMaxBytes = 4 << 20

// remotePublishLiveMaxBytes is a package var, not a const, so tests can
// shrink it without building multi-MB fixtures.
var remotePublishLiveMaxBytes = defaultRemotePublishLiveMaxBytes

// defaultRemotePublishRetainedMaxBytes caps the SEALED size of a
// lane=retained conversation baseline. It mirrors internal/daemon's
// remotePublishMaxRetainedEventBytes (4 MB, the current practical per-message
// transport budget):
// a baseline above it has NO transport path — the daemon would only stream it
// through the publish queue and dead-letter it. forwardCommitted therefore
// refuses it at the source (design rule 6's acknowledged residual, handled
// honestly): the existing remote.outbound_oversized bus event surfaces with
// retained_too_large=true, and the artifact is marked
// (o.remoteRetainedOversized) so the republish sweep and backfill trickle do
// not spin re-materializing + re-sealing hundreds of MB until the head
// changes. Keeping this producer-side cap identical to the daemon queue cap is
// essential: a larger value here spends CPU compressing/encrypting an event
// that the queue can only dead-letter, and repeated large native updates can
// otherwise create sustained load and transport reconnect pressure.
const defaultRemotePublishRetainedMaxBytes = 4 << 20

// remotePublishRetainedMaxBytes is a package var, not a const, so tests can
// shrink it.
var remotePublishRetainedMaxBytes = defaultRemotePublishRetainedMaxBytes

const (
	remoteRepublishRecentWindow = 24 * time.Hour
	remoteRepublishMaxHeads     = 256
)

// A startup-seeded conversation normally receives one retained-baseline repair
// publish. That best-effort repair must never make the one-minute sweep read a
// giant append line: large conversations already use live deltas and their
// next real edit goes through forwardCommitted directly. The threshold is a
// package variable so regression tests can exercise the gate cheaply.
const defaultRemoteSeededConversationRepairMaxLogBytes int64 = 16 << 20

var remoteSeededConversationRepairMaxLogBytes = defaultRemoteSeededConversationRepairMaxLogBytes

// defaultRemoteRepublishBackfillMaxHeads bounds ONE pass of the slow
// retained-baseline backfill trickle (BackfillLocalRemoteHeads): the daemon
// calls it on a slow ticker, so the whole store converges eventually without
// ever flooding the relay the way an unbounded historical sweep would.
const defaultRemoteRepublishBackfillMaxHeads = 64

// remoteRepublishBackfillMaxHeads is a package var, not a const, so tests can
// shrink it.
var remoteRepublishBackfillMaxHeads = defaultRemoteRepublishBackfillMaxHeads

type remoteRepublishCandidate struct {
	artifactID string
	branch     string
	eventID    string
	headHash   string
	timestamp  time.Time
}

// RepublishLocalRemoteHeads republishes recent local-authored artifact heads to
// the remote transport. It is used after the remote plugin has
// reconnected/subscribed so retained latest-state transports can catch up peers
// without requiring every artifact to be edited again. It ALSO re-seals and
// re-publishes an UNCHANGED head whose last seal targeted a different
// recipient roster (see shouldRepublishLocalRemoteHead) — the daemon's
// membership-changed hint path relies on this so a retained baseline sealed
// while the roster was degraded (e.g. the resolver's self-only fallback)
// recovers without waiting for the artifact's next head change.
//
// Only heads whose provenance belongs to this device are republished when the
// daemon knows its cloud LocalDeviceID. That avoids turning a peer-authored
// inbound head from a previous daemon run into a fresh local-looking outbound
// event after the in-memory remote-origin loop guard has been reset.
//
// The pass is intentionally bounded and newest-first. Retained transports only
// need current heads, and a full historical sweep can bury live edits behind a
// large recovery backlog, violating the near-real-time sync contract. The
// long tail OUTSIDE the recent window is owned by the slow backfill trickle,
// BackfillLocalRemoteHeads.
func (o *Orchestrator) RepublishLocalRemoteHeads(ctx context.Context) (int, error) {
	if o.cfg.Store == nil {
		return 0, fmt.Errorf("remote republish: store is nil")
	}
	if o.remoteEventPublisher() == nil {
		return 0, nil
	}

	localDeviceID := o.localDeviceID()
	var firstErr error
	cutoff := time.Now().Add(-remoteRepublishRecentWindow)
	// Resolve the CURRENT roster identity once for the pass (V1 is
	// single-namespace, so one fingerprint covers every artifact): an
	// unchanged head still republishes when the roster it was last sealed for
	// differs — see shouldRepublishLocalRemoteHead.
	currentFP := o.currentRecipientsFingerprint("")
	candidates := make([]remoteRepublishCandidate, 0, remoteRepublishMaxHeads)
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		if err := ctx.Err(); err != nil {
			if firstErr != nil {
				return 0, firstErr
			}
			return 0, err
		}
		artifacts, err := o.cfg.Store.ListArtifacts(kind)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, art := range artifacts {
			if err := ctx.Err(); err != nil {
				if firstErr != nil {
					return 0, firstErr
				}
				return 0, err
			}
			if o.inUnresolvedConflict(art.ArtifactID) {
				continue
			}
			if _, authorized := o.remoteProjectAuthorization(art); !authorized {
				continue
			}
			// Artifact metadata is enough to reject almost every steady-state
			// entry. Perform these checks before LastEvent: a conversation event
			// is a complete JSON line and can be hundreds of megabytes, so reading
			// the tail merely to discover an unchanged hash made this one-minute
			// safety sweep consume a core continuously.
			if !art.UpdatedAt.IsZero() && art.UpdatedAt.Before(cutoff) {
				continue
			}
			branch := acf.MainBranch
			changed := o.shouldRepublishLocalRemoteHead(art.ArtifactID, branch, art.HeadEventHash, currentFP)
			seededConversation := kind == acf.KindConversation &&
				o.shouldRepublishSeededConversationHead(art.ArtifactID, branch, art.HeadEventHash, currentFP)
			if !changed && !seededConversation {
				continue
			}
			if !changed && seededConversation {
				size, sizeErr := o.cfg.Store.EventLogSize(kind, art.ArtifactID)
				if sizeErr != nil {
					if firstErr == nil {
						firstErr = sizeErr
					}
					continue
				}
				if size > remoteSeededConversationRepairMaxLogBytes {
					continue
				}
			}
			head, ok, err := o.cfg.Store.LastEvent(kind, art.ArtifactID)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if !ok {
				continue
			}
			if !remoteSweepLocalHead(o.cfg.Store, kind, art.ArtifactID, localDeviceID, head) {
				continue
			}
			// A non-main branch can still be the append-order tail. Re-check the
			// actual branch because the metadata prefilter above intentionally
			// assumes main to avoid opening the log for unchanged artifacts.
			if normalizeBranchName(head.Branch) != branch &&
				!o.shouldRepublishLocalRemoteHead(art.ArtifactID, head.Branch, art.HeadEventHash, currentFP) {
				continue
			}
			candidates = append(candidates, remoteRepublishCandidate{
				artifactID: art.ArtifactID,
				branch:     normalizeBranchName(head.Branch),
				eventID:    head.EventID,
				headHash:   art.HeadEventHash,
				timestamp:  head.Timestamp,
			})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].timestamp.After(candidates[j].timestamp)
	})
	if len(candidates) > remoteRepublishMaxHeads {
		candidates = candidates[:remoteRepublishMaxHeads]
	}
	published := 0
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			if firstErr != nil {
				return published, firstErr
			}
			return published, err
		}
		// forwardCommitted itself marks the dedup index on success, with the
		// FRESH head hash it re-read and the roster it actually sealed for —
		// no stale re-mark with the listing-time candidate hash here.
		if o.forwardCommitted(c.artifactID) {
			published++
		}
	}
	return published, firstErr
}

// BackfillLocalRemoteHeads is the SLOW-TRICKLE companion to
// RepublishLocalRemoteHeads. The recent sweep above deliberately covers only
// heads inside remoteRepublishRecentWindow, so an artifact last edited before
// that window NEVER gets a retained baseline published — a peer stays
// diverged on it until its next head change. Each
// backfill pass publishes the next remoteRepublishBackfillMaxHeads OLDEST
// local-authored heads that were never actually handed to the remote
// publisher during this daemon run (startup-seeded dedup entries and heads
// whose publish was missed), tracked via the same republished-heads index, so
// the whole store converges eventually without a flood.
//
// Wedge safety: every candidate is attempted AT MOST ONCE per daemon run
// (o.remoteBackfillAttempted) — an artifact whose publish is persistently
// declined (route.remote=exclude, seal failure) must not occupy the head of
// the oldest-first line forever. A pass with an unresolvable/empty roster is
// skipped ENTIRELY (no attempts burnt): publishing would only drop every
// candidate (never plaintext) and permanently starve them for this run.
func (o *Orchestrator) BackfillLocalRemoteHeads(ctx context.Context) (int, error) {
	if o.cfg.Store == nil {
		return 0, fmt.Errorf("remote backfill: store is nil")
	}
	if o.remoteEventPublisher() == nil {
		return 0, nil
	}
	currentFP := o.currentRecipientsFingerprint("")
	if currentFP == "" {
		return 0, nil
	}

	localDeviceID := o.localDeviceID()
	var firstErr error
	candidates := make([]remoteRepublishCandidate, 0, remoteRepublishBackfillMaxHeads)
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		if err := ctx.Err(); err != nil {
			if firstErr != nil {
				return 0, firstErr
			}
			return 0, err
		}
		artifacts, err := o.cfg.Store.ListArtifacts(kind)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, art := range artifacts {
			if err := ctx.Err(); err != nil {
				if firstErr != nil {
					return 0, firstErr
				}
				return 0, err
			}
			if o.inUnresolvedConflict(art.ArtifactID) {
				continue
			}
			if _, authorized := o.remoteProjectAuthorization(art); !authorized {
				continue
			}
			head, ok, err := o.cfg.Store.LastEvent(kind, art.ArtifactID)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if !ok {
				continue
			}
			if !o.shouldBackfillLocalRemoteHead(art.ArtifactID, head.Branch, art.HeadEventHash, currentFP) {
				continue
			}
			if !remoteSweepLocalHead(o.cfg.Store, kind, art.ArtifactID, localDeviceID, head) {
				continue
			}
			candidates = append(candidates, remoteRepublishCandidate{
				artifactID: art.ArtifactID,
				branch:     normalizeBranchName(head.Branch),
				eventID:    head.EventID,
				headHash:   art.HeadEventHash,
				timestamp:  head.Timestamp,
			})
		}
	}
	// OLDEST first — the recent sweep already owns the newest heads; the
	// trickle's job is the long tail the window never reaches.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].timestamp.Before(candidates[j].timestamp)
	})
	if len(candidates) > remoteRepublishBackfillMaxHeads {
		candidates = candidates[:remoteRepublishBackfillMaxHeads]
	}
	published := 0
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			if firstErr != nil {
				return published, firstErr
			}
			return published, err
		}
		// Burn the attempt BEFORE publishing so a decline advances the line
		// next pass; a SUCCESS both marks the dedup index (inside
		// forwardCommitted, with the roster it sealed for) and refunds the
		// attempt, so a later roster change makes the artifact eligible for a
		// re-seal pass again (see shouldBackfillLocalRemoteHead).
		o.markBackfillAttempted(c.artifactID, c.branch)
		if o.forwardCommitted(c.artifactID) {
			o.clearBackfillAttempted(c.artifactID, c.branch)
			published++
		}
	}
	return published, firstErr
}

func remoteSweepLocalHead(store *acf.Store, kind acf.Kind, artifactID, localDeviceID string, head acf.Event) bool {
	if localDeviceID == "" {
		return true
	}
	if head.Provenance.DeviceID == localDeviceID {
		return true
	}
	if head.Provenance.DeviceID != "" || head.Type != acf.EventTypeResolution ||
		(head.Provenance.SourceAgent != "aplexica:web-resolve" && head.Provenance.SourceAgent != "aplexica:resolve") {
		return false
	}

	// Releases before v1.0.34 did not stamp the local device identity on
	// conflict-resolution events. Recover only the narrowly attributable
	// legacy case: the unattributed resolution must directly extend a head
	// authored by this device. A peer-authored or orphaned resolution remains
	// ineligible, preserving the restart loop guard.
	events, err := store.ReadRecentEvents(kind, artifactID, 2)
	if err != nil || len(events) < 2 {
		return false
	}
	previous := events[len(events)-2]
	return previous.Hash == head.ParentHash && previous.Provenance.DeviceID == localDeviceID
}

// remoteProjectAuthorization returns the current local authorization entry for
// a project-scoped artifact. A nil registry (legacy/offline callers) and
// legacy project artifacts without Project metadata retain their historical
// behavior. When a registry is configured, however, a missing/inactive/revoked
// entry is a hard outbound gate. Keeping this check reusable lets periodic
// sweeps reject stale project artifacts from metadata alone, before opening a
// potentially very large conversation event log.
func (o *Orchestrator) remoteProjectAuthorization(art acf.Artifact) (project.Entry, bool) {
	if art.Scope != acf.ScopeProject || art.Project == nil || o.cfg.ProjectRegistry == nil {
		return project.Entry{}, true
	}
	return o.cfg.ProjectRegistry.Get(art.Project.ID)
}

// shouldBackfillLocalRemoteHead reports whether the backfill trickle still
// owes artifactID a publish this run: either it was never actually handed to
// the remote publisher (a startup-seeded or absent dedup entry), or it WAS
// published but sealed for a different recipient roster than currentFP (a
// baseline peers added since can never decrypt; the recent sweep only covers
// this for heads inside its window — the trickle owns the long tail). An
// artifact whose last backfill attempt was declined is skipped for the rest
// of the run, and a head whose retained baseline is known OVER-CAP
// (o.remoteRetainedOversized) is skipped WITHOUT burning its once-per-run
// attempt — a later head change makes it a normal candidate again.
func (o *Orchestrator) shouldBackfillLocalRemoteHead(artifactID, branch, headHash, currentFP string) bool {
	if artifactID == "" {
		return false
	}
	key := remoteHeadKey(artifactID, branch)
	o.mu.Lock()
	defer o.mu.Unlock()
	if h, oversized := o.remoteRetainedOversized[key]; oversized && h == headHash {
		return false
	}
	if _, attempted := o.remoteBackfillAttempted[key]; attempted {
		return false
	}
	entry := o.remoteRepublishedHeads[key]
	if !entry.published {
		return true
	}
	return currentFP != "" && entry.recipientsFP != "" && entry.recipientsFP != currentFP
}

// markBackfillAttempted records that a backfill pass spent artifactID+branch's
// once-per-run attempt (see BackfillLocalRemoteHeads wedge safety).
func (o *Orchestrator) markBackfillAttempted(artifactID, branch string) {
	if artifactID == "" {
		return
	}
	key := remoteHeadKey(artifactID, branch)
	o.mu.Lock()
	if o.remoteBackfillAttempted == nil {
		o.remoteBackfillAttempted = map[string]struct{}{}
	}
	o.remoteBackfillAttempted[key] = struct{}{}
	o.mu.Unlock()
}

// clearBackfillAttempted refunds artifactID+branch's backfill attempt after a
// SUCCESSFUL publish: the attempt set only exists to skip persistently-
// declined artifacts, and a published artifact must stay eligible for a
// roster-change re-seal (its dedup-index fingerprint gates any repeat).
func (o *Orchestrator) clearBackfillAttempted(artifactID, branch string) {
	key := remoteHeadKey(artifactID, branch)
	o.mu.Lock()
	delete(o.remoteBackfillAttempted, key)
	o.mu.Unlock()
}

// ImportOutcome classifies how a single inbound RemoteEvent was handled by
// importOneInbound. The remote-sync driver uses it to decide whether its
// per-(namespace,branch) resume cursor may safely advance PAST an event: a
// durably-consumed or intentionally-dropped event may be skipped on the next
// fetch, but a transiently-failed one MUST be refetched (advancing past it
// would lose the event permanently — see FR-03.13 / cmd_daemon reconcile).
type ImportOutcome int

const (
	// ImportApplied: the event was decoded and durably appended (or self-healed
	// via rebase / recorded as a conflict). Safe to advance the cursor past it.
	ImportApplied ImportOutcome = iota
	// ImportDeduped: the event's EventID already exists in the artifact log
	// (idempotent redelivery). Already durable; safe to advance past it.
	ImportDeduped
	// ImportSkipped: the envelope was not sealed for this device
	// (errNotARecipient), or the event is a retained-slot CLEAR (Clear=true
	// — transport plumbing with no body to import). Intentionally dropped;
	// safe to advance past it.
	ImportSkipped
	// ImportRejected: the event is malformed (empty kind / event id / artifact
	// id) and can never be applied. Surfaced on the bus and intentionally
	// dropped; safe to advance past it (refetching would only re-reject).
	ImportRejected
	// ImportRetryable: a transient failure (store / decrypt / rebase / conflict
	// error) prevented a durable apply. The cursor MUST NOT advance past it so
	// the next fetch refetches and retries — silently advancing would drop the
	// event permanently.
	ImportRetryable
	// ImportDeferredNeedsBaseline: a lane=live conversation delta arrived whose
	// ParentHash does not extend this device's chain (a missed delta or an
	// un-aligned artifact). Deltas are not self-contained, so refetching can
	// never make it apply — the event is intentionally dropped, the artifact is
	// marked needs-baseline (see Orchestrator.needsBaseline), and recovery
	// happens via the origin's always-published lane=retained full state
	// (baseline adoption). Safe to advance the cursor past it — deliberately
	// NOT Retryable, which would wedge the branch on an event that cannot
	// succeed.
	ImportDeferredNeedsBaseline
)

// ImportInbound is the entrypoint the daemon wires to RemoteRunner.OnInbound.
// For each event the plugin delivered from the relay it records the origin
// device (for loop prevention) and appends the decoded canonical event to the
// local store with that origin preserved as the event's provenance device id.
//
// Dedup: an event whose EventID already appears in the artifact's log is a
// no-op (idempotent redelivery). An empty EventID is REJECTED — it is mandatory
// on the wire and a missing one signals a malformed plugin/relay (P2-1).
//
// This path deliberately does NOT call publishOutbound: an inbound event must
// never be forwarded straight back out. After a successful append it DOES
// materialise the artifact to local agent native files via materializeInbound
// (recursion-guard-suppressed, so the native write never loops back to the
// relay) so the receiving device's agents actually see the synced artifact.
//
// This is a thin void wrapper over ImportInboundResults so the RemoteRunner
// OnInbound wiring (typed func([]proto.RemoteEvent)) is unchanged; callers that
// need the per-event outcomes (the remote-sync driver) call ImportInboundResults
// directly.
func (o *Orchestrator) ImportInbound(events []proto.RemoteEvent) {
	_ = o.ImportInboundResults(events)
}

// ImportInboundResults applies each inbound RemoteEvent in order and returns one
// ImportOutcome per event, IN THE SAME ORDER as the input slice. The remote-sync
// driver uses the outcomes to advance its resume cursor only through durably-
// consumed / intentionally-dropped events and to STOP at the first transient
// (ImportRetryable) failure, so a per-event store/decrypt/rebase error can never
// be silently skipped past the cursor (FR-03.13 — no silent loss).
func (o *Orchestrator) ImportInboundResults(events []proto.RemoteEvent) []ImportOutcome {
	return o.importInboundResults(events, true)
}

// ImportInboundCanonicalResults durably applies inbound events to the
// canonical store without materialising agent-native files. Durable cloud
// receive uses this split phase so its ordering remains:
//
//	canonical append/fsync -> terminal receipt/cursor -> cloud ACK -> native materialisation
//
// Legacy transports continue to call ImportInboundResults and retain their
// existing immediate-materialisation behaviour.
func (o *Orchestrator) ImportInboundCanonicalResults(events []proto.RemoteEvent) []ImportOutcome {
	return o.importInboundResults(events, false)
}

func (o *Orchestrator) importInboundResults(events []proto.RemoteEvent, materialize bool) []ImportOutcome {
	o.nativeRestoreGate.RLock()
	defer o.nativeRestoreGate.RUnlock()
	if o.cfg.Store == nil {
		return nil
	}
	outcomes := make([]ImportOutcome, len(events))
	for i, re := range events {
		outcome, err := o.importOneInbound(re, materialize)
		outcomes[i] = outcome
		if err != nil && o.cfg.EventPublisher != nil {
			// Surface the failure on the local event bus for visibility; the
			// import is best-effort (a malformed event must not stall the
			// stream). No-op when no publisher is wired.
			o.publishEvent("remote.inbound_error", map[string]any{
				"artifact_id": re.ArtifactID,
				"event_id":    re.EventID,
				"error":       err.Error(),
			})
		}
	}
	return outcomes
}

// terminalEnvelopeError maps a permanent v2 envelope failure to one of the
// stable, content-free security error classes safe to surface in diagnostics.
// Parser, AEAD, compression, and body-decoding errors can contain implementation
// details and are all equivalent at this boundary: the input is
// malformed/corrupt and cannot become valid on retry.
func terminalEnvelopeError(err error) error {
	for _, stable := range []error{
		securityerr.ErrInvalidSignature,
		securityerr.ErrUntrustedRoster,
		securityerr.ErrMetadataMismatch,
		securityerr.ErrUnsafeIdentifier,
		securityerr.ErrLimitExceeded,
		securityerr.ErrUnsignedInput,
	} {
		if errors.Is(err, stable) {
			return stable
		}
	}
	return securityerr.ErrMetadataMismatch
}

func validLowerHexSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validateAuthenticatedCheckpointAlignment keeps canonical ancestry and
// checkpoint coverage as two independent authenticated values. ParentHash is
// always the event's real predecessor (and may be empty); a durable checkpoint
// instead names the covered head through CheckpointAlignmentHash, which must
// equal Canonical.AlignedHead from the signed envelope header.
//
// Legacy MQTT retained records predate the additive outer field, so a retained
// event without durable checkpoint metadata may omit it. If a modern retained
// record supplies it, it is still authenticated. Live/tombstone/laneless events
// must never smuggle an alignment value into durable metadata.
func validateAuthenticatedCheckpointAlignment(re proto.RemoteEvent, header EventHeaderV2) error {
	hasCoverage := re.CheckpointCoverage != 0
	hasGeneration := re.CheckpointGeneration != ""
	if hasCoverage != hasGeneration {
		return securityerr.ErrMetadataMismatch
	}
	if hasCoverage {
		if re.Lane != LaneRetained || re.Clear || !validLowerHexSHA256(re.CheckpointGeneration) ||
			!validLowerHexSHA256(re.CheckpointAlignmentHash) || re.CheckpointAlignmentHash != header.Canonical.AlignedHead {
			return securityerr.ErrMetadataMismatch
		}
		return nil
	}
	if re.Lane != LaneRetained {
		if re.CheckpointAlignmentHash != "" || re.Lane == LaneLive && header.Canonical.AlignedHead != "" {
			return securityerr.ErrMetadataMismatch
		}
		return nil
	}
	if re.Clear {
		if re.CheckpointAlignmentHash != "" {
			return securityerr.ErrMetadataMismatch
		}
		return nil
	}
	if re.CheckpointAlignmentHash != "" &&
		(!validLowerHexSHA256(re.CheckpointAlignmentHash) || re.CheckpointAlignmentHash != header.Canonical.AlignedHead) {
		return securityerr.ErrMetadataMismatch
	}
	return nil
}

// validateAuthenticatedInboundOuter binds every transport routing field to the
// signed envelope header. Durable cloud receive additionally binds EventHash:
// the plugin/cloud-visible hash must be the exact authenticated canonical hash
// (or absent on a signed retained CLEAR) before any terminal local commit.
func validateAuthenticatedInboundOuter(re proto.RemoteEvent, header EventHeaderV2, requireEventHash bool) error {
	routing := header.Routing
	if routing.NamespaceID != re.NamespaceID || routing.BranchID != re.BranchID ||
		routing.ArtifactID != re.ArtifactID || routing.WireEventID != re.EventID ||
		routing.ParentHash != re.ParentHash || routing.Kind != re.Kind || routing.EventType != re.Type ||
		routing.TimestampUnixNano != re.Timestamp.UnixNano() || routing.Sequence != re.Sequence ||
		routing.OriginDevice != re.Origin || routing.SourceAgent != re.SourceAgent ||
		routing.Lane != re.Lane || routing.Clear != re.Clear {
		return securityerr.ErrMetadataMismatch
	}
	if err := validateAuthenticatedCheckpointAlignment(re, header); err != nil {
		return err
	}
	if !requireEventHash {
		return nil
	}
	if re.Clear {
		return securityerr.ErrMetadataMismatch
	}
	if !validLowerHexSHA256(re.EventHash) || re.EventHash != header.Canonical.EventHash {
		return securityerr.ErrMetadataMismatch
	}
	return nil
}

// importOneInbound applies a single inbound RemoteEvent to the canonical
// store. It records the origin device, decodes the canonical event from the
// opaque Bytes, and applies it under a dedup guard. It returns an
// ImportOutcome classifying the result (see ImportOutcome) alongside any
// error — the error is only for bus visibility; the outcome is authoritative
// for cursor advancement.
//
// Conversations arriving on an explicit Lane take the aligned-chains routes
// (design rule 4): lane=live events append VERBATIM (native chain extension —
// or defer needs-baseline on an unknown parent), lane=retained events
// content-classify and adopt the full state as an aligned baseline. Lane==""
// (legacy pre-lane events) and every non-conversation kind keep the original
// reconcile behavior below.
func (o *Orchestrator) importOneInbound(re proto.RemoteEvent, materialize bool) (ImportOutcome, error) {
	kind := acf.Kind(re.Kind)
	if kind == "" {
		return ImportRejected, fmt.Errorf("syncd: inbound event has empty kind (artifact %q)", re.ArtifactID)
	}

	// A retained-slot CLEAR (redaction propagation) carries no body — it
	// exists solely so the PLUGIN clears the broker's retained slot. Should a
	// transport ever bridge one inbound, there is nothing to import (the
	// redaction itself arrives as a normal lane=live event): skip it,
	// advancing the cursor. Attempting to open its empty envelope would
	// instead classify as a permanent ImportRetryable and wedge the cursor.
	if re.Clear && !o.cfg.RequireEnvelopeV2 {
		return ImportRejected, fmt.Errorf("syncd: unsigned retained clear rejected")
	}

	// Realtime retained subscriptions replay the latest event for every
	// artifact after each transport reconnect. Most are exact redeliveries of
	// the local append-order tail. Reject unsafe path components, then dedupe
	// that common case from the already-durable tail metadata before decrypting,
	// decompressing, and unmarshaling a potentially very large envelope.
	//
	// This does not trust or apply unauthenticated content: it only declines an
	// event whose (kind, artifact, wire-event) identity is already durable. The
	// stored event supplies the origin used by the loop guard. Non-current and
	// mismatched events still take the full authenticated envelope path below.
	if !re.Clear && (materialize || !o.cfg.RequireEnvelopeV2) && acf.ValidateKind(kind) == nil &&
		acf.ValidateWireUUIDv7(re.ArtifactID) == nil &&
		acf.ValidateWireEventID(re.EventID) == nil {
		if head, ok, herr := o.cfg.Store.LastEvent(kind, re.ArtifactID); herr == nil && ok && head.EventID == re.EventID {
			o.markRemoteOrigin(head.Provenance.DeviceID)
			return ImportDeduped, nil
		}
	}

	// Decrypt the per-event envelope with this device's private wrap key. An
	// envelope NOT addressed to this device is skipped (not an error) — it was
	// sealed for other recipients. Loading the local key remains retryable, but
	// once that key is available a decode/auth failure for this immutable wire
	// record is terminal. Retrying the same corrupt ciphertext (or a retained
	// envelope wrapped to a superseded key after device repair) can never change
	// the result and must not wedge the branch cursor ahead of later valid
	// baselines.
	var ev acf.Event
	var scope acf.Scope
	var proj *project.ProjectInfo
	var err error
	if o.cfg.RequireEnvelopeV2 {
		provider := o.verifiedRosterProvider()
		identityProvider := o.v2IdentityProvider()
		if provider == nil || identityProvider == nil {
			return ImportRetryable, fmt.Errorf("syncd: verified roster or device identity unavailable")
		}
		scopeType, scopeID := "account", ""
		if re.KeyMode == "namespace-key-v1" {
			scopeType, scopeID = "namespace", re.NamespaceID
		}
		snapshot, serr := provider.Current(context.Background(), scopeType, scopeID)
		if serr != nil {
			return ImportRetryable, fmt.Errorf("syncd: verified roster unavailable: %w", serr)
		}
		if snapshot.BarrierID != re.SecurityBarrierID || snapshot.CoordinatorGeneration != re.SecurityGeneration || snapshot.Roster.Manifest.Manifest.AccessGeneration != re.AccessGeneration || snapshot.Roster.Manifest.Manifest.AccessSetHash != re.AccessSetHash || snapshot.KeyMode != re.KeyMode || snapshot.KeyVersion != re.KeyVersion {
			return ImportRejected, securityerr.ErrMetadataMismatch
		}
		identity, ierr := identityProvider.Identity()
		if ierr != nil {
			return ImportRetryable, ierr
		}
		// v2/v3 share the signed-roster crypto and the authenticated-open
		// contract; only the framing differs. Dispatch on the frozen one-byte
		// discriminator (envelopeIsV3) — a v3 envelope that reaches a build
		// without this decoder fails JSON decode below and is quarantined,
		// which is exactly why sealing v3 requires the roster intersection.
		openAuthenticated := OpenEnvelopeV2AuthenticatedWithNamespaceProvider
		if envelopeIsV3(re.Bytes) {
			openAuthenticated = OpenEnvelopeV3AuthenticatedWithNamespaceProvider
		}
		body, authenticated, oerr := openAuthenticated(re.Bytes, snapshot.Roster, o.localDeviceID(), identity.WrapPrivate, o.namespaceKeyProvider())
		header := authenticated.Header
		if authenticated.SignerDeviceID != "" &&
			(header.SecurityBarrierID != snapshot.BarrierID || header.TreeHeadDigest != snapshot.TreeHeadDigest || validateAuthenticatedInboundOuter(re, header, !materialize) != nil) {
			err = securityerr.ErrMetadataMismatch
		} else if oerr != nil {
			err = oerr
		} else if re.Clear && !materialize {
			return ImportRejected, securityerr.ErrMetadataMismatch
		} else if re.Clear {
			return ImportSkipped, nil
		} else {
			ev = body.Event
			scope = body.EnvScope
			if body.EnvProject != nil {
				proj = &project.ProjectInfo{ID: body.EnvProject.ID, VCS: body.EnvProject.VCS}
			}
		}
	} else {
		priv, kerr := o.devicePrivateKey()
		if kerr != nil {
			return ImportRetryable, fmt.Errorf("syncd: load device key for inbound decrypt: %w", kerr)
		}
		ev, scope, proj, err = openEnvelope(re.Bytes, o.localDeviceID(), priv)
	}
	if err != nil {
		if errors.Is(err, errNotARecipient) {
			if o.cfg.Logger != nil {
				o.cfg.Logger.Info("remote: inbound envelope skipped (not a recipient)",
					"artifact_id", re.ArtifactID,
					"event_id", re.EventID,
					"origin", re.Origin,
					"local_device_id", o.localDeviceID())
			}
			return ImportSkipped, nil // sealed for other devices; nothing to import here
		}
		if o.cfg.RequireEnvelopeV2 {
			// Once the current roster/security epoch and local identity have been
			// loaded above, cryptographic or structural failures are permanent:
			// retrying the same immutable envelope can never repair a bad
			// signature, metadata mismatch, corrupt ciphertext, malformed body, or
			// authenticated size violation. Classify those as rejected so the v2
			// delivery layer can durably quarantine it and later valid events are
			// not held behind one poison record forever. Missing namespace-key history is
			// the sole open-stage transient and remains retryable.
			if errors.Is(err, securityerr.ErrStaleRoster) {
				return ImportRetryable, fmt.Errorf("syncd: open inbound envelope: %w", securityerr.ErrStaleRoster)
			}
			return ImportRejected, fmt.Errorf("syncd: open inbound envelope: %w", terminalEnvelopeError(err))
		}
		return ImportRejected, fmt.Errorf("syncd: open inbound envelope: %w", err)
	}

	// Lane routing flags (aligned-chains design rule 4): a conversation event
	// arriving on an explicit lane takes the aligned-chains inbound paths
	// below — a live event appends verbatim (or defers needs-baseline), a
	// retained event content-classifies and adopts a baseline. Lane=="" is a
	// legacy pre-lane event, and non-conversation kinds duplicate the SAME
	// full-state bytes on both topics — both keep the pre-lane reconcile
	// behavior unchanged (an unknown future lane value falls back the same
	// way).
	laneLive := kind == acf.KindConversation && re.Lane == LaneLive
	laneRetained := kind == acf.KindConversation && re.Lane == LaneRetained

	if !o.cfg.RequireEnvelopeV2 {
		if err := validateLegacyOuterMetadata(re, ev); err != nil {
			return ImportRejected, err
		}
	}
	if laneRetained {
		// The retained lane has a deterministic, origin-scoped transport ID.
		// Validation above proves it is exactly derived from the authenticated
		// canonical EventID and provenance, so it is safe to use for the local
		// synthetic baseline record and keeps live/retained redeliveries distinct.
		ev.EventID = re.EventID
	}
	if laneLive {
		// VERBATIM: a live event must be appended byte-identical to the
		// origin's stored event so AppendEvent's recomputed hash equals the
		// origin's head hash (the alignment invariant). The envelope-field
		// reconciliation below is therefore NOT applied — in particular
		// re.BranchID ("main") would replace a stored empty Branch and
		// re.Origin would replace the stored provenance device id, both of
		// which change the canonical JSON and so the hash. The outbound loop
		// guard still learns the event's own provenance device id here (it
		// is what a re-import of materialized content would carry).
	}
	o.markRemoteOrigin(ev.Provenance.DeviceID)

	if ev.ArtifactID == "" {
		return ImportRejected, fmt.Errorf("syncd: inbound event has no artifact id")
	}

	// EventID is mandatory on the wire (proto.RemoteEvent doc): it is the
	// hash-chain identifier the relay and this device dedupe on. A missing
	// EventID signals a malformed / non-conformant plugin or relay, so reject
	// it outright (P2-1). ImportInbound surfaces remote.inbound_error and
	// continues the stream, so one bad event can't stall the batch. Tolerating
	// it would not only skip dedupe but, on redelivery, churn the destructive
	// rebase path (delete + re-genesis) on every retry.
	if ev.EventID == "" {
		return ImportRejected, fmt.Errorf("syncd: inbound event has no event id (artifact %q)", ev.ArtifactID)
	}
	if scope == acf.ScopeNamespace {
		if err := acf.ValidateWireUUIDv7(re.NamespaceID); err != nil {
			return ImportRejected, err
		}
		if serr := o.ensureInboundArtifactShell(kind, ev, scope, proj, re.NamespaceID); serr != nil {
			return ImportRetryable, serr
		}
	}

	// Fast path: redelivery of the CURRENT head (the common case for retained
	// latest-state catch-up after a reconnect) — one tail read instead of a
	// full log scan. Mandatory v2 authenticates both IDs, so also compare the
	// outer transport ID. Older retained records can have Lane=="" while their
	// transport ID already uses the origin-scoped "-r-" form; after adoption the
	// physical baseline stores that transport ID even though the sealed body
	// keeps the canonical head ID. Checking only ev.EventID made an already-
	// durable baseline look new on every redelivery and wedged the branch cursor.
	if head, ok, herr := o.cfg.Store.LastEvent(kind, ev.ArtifactID); herr == nil && ok &&
		(head.EventID == ev.EventID || (o.cfg.RequireEnvelopeV2 && head.EventID == re.EventID)) {
		return ImportDeduped, nil
	}

	// Dedup: use the store's lazy id-only index. The former ReadEvents loop
	// decoded every payload for every redelivered event; reconnecting with one
	// large conversation therefore replayed tens of gigabytes of JSON and held
	// multiple gigabytes of heap despite needing only the small EventID field.
	if exists, rerr := o.cfg.Store.HasEventID(kind, ev.ArtifactID, ev.EventID); rerr == nil && exists {
		return ImportDeduped, nil
	}
	if o.cfg.RequireEnvelopeV2 && re.EventID != ev.EventID {
		if exists, rerr := o.cfg.Store.HasEventID(kind, ev.ArtifactID, re.EventID); rerr == nil && exists {
			return ImportDeduped, nil
		}
	}

	if laneLive {
		return o.importLiveConversationEvent(ev, scope, proj, materialize)
	}
	if laneRetained {
		if outcome, handled, rerr := o.reconcileRetainedConversation(ev, scope, proj, materialize); handled {
			return outcome, rerr
		}
		// Un-classifiable retained event (missing alignment metadata or a payload
		// that is not comparable ConversationFormatV1): fall through to the
		// legacy reconcile below so the state still lands.
	}

	if serr := o.ensureInboundArtifactShell(kind, ev, scope, proj); serr != nil {
		return ImportRetryable, serr
	}

	if aerr := o.cfg.Store.AppendEvent(kind, ev); aerr != nil {
		if !errors.Is(aerr, acf.ErrHeadMismatch) {
			return ImportRetryable, fmt.Errorf("syncd: append inbound event: %w", aerr)
		}
		// ErrHeadMismatch: conversations are content-classified first; the
		// remaining kinds have two distinct causes that MUST be handled
		// differently (P1-5):
		//
		//   (a) CHAIN GAP / MISSING PARENT — the inbound ParentHash
		//       references history this device never received (a dropped or
		//       reordered redelivery, or a restart). acf events are
		//       CUMULATIVE full-state snapshots, so we self-heal by adopting
		//       the inbound event as a fresh baseline (rebaseInbound). This
		//       is the case commit 1ea1f5d was written for.
		//
		//   (b) GENUINE DIVERGENCE — this device HAS the inbound ParentHash
		//       in its log, but its head has already moved past it via a
		//       different local edit: two real concurrent edits off a shared
		//       ancestor. Per BRD-04 §4.2/§5.7 the daemon MUST NOT pick a
		//       winner — record a conflict and keep BOTH; never delete the
		//       local chain.
		//
		// HasEventHash distinguishes the two (it returns false for an empty
		// or unknown parent), so we default to the self-heal whenever the
		// parent is genuinely absent.
		//
		// Conversations: classify by CONTENT before any hash/conflict logic.
		// Cross-device conversation chains never share hashes (rebase
		// recomputes the genesis), so both the "missing parent" and the
		// "known parent, head moved" shapes land here and both must be
		// content-reconciled — the hash-based conflict path below can only
		// see same-device divergence. Deltas stay retryable: they are not
		// self-contained, so neither comparison nor rebase is sound.
		if kind == acf.KindConversation && !isConversationDeltaEvent(kind, ev) {
			if outcome, handled, cerr := o.reconcileInboundConversation(ev, materialize); handled {
				return outcome, cerr
			}
		}
		if has, herr := o.cfg.Store.HasEventHash(kind, ev.ArtifactID, ev.ParentHash); herr == nil && has {
			if rerr := o.recordInboundConflictWithDurability(kind, ev, !materialize); rerr != nil {
				return ImportRetryable, fmt.Errorf("syncd: record inbound conflict: %w", rerr)
			}
			return ImportApplied, nil
		}
		if isConversationDeltaEvent(kind, ev) {
			return ImportRetryable, fmt.Errorf("syncd: inbound conversation delta is missing parent %q", ev.ParentHash)
		}
		if rerr := o.rebaseInbound(kind, ev); rerr != nil {
			return ImportRetryable, fmt.Errorf("syncd: rebase inbound event: %w", rerr)
		}
	}

	// Materialise the imported artifact to local agent native files so the
	// receiving device's agents (claude-code, codex, ...) actually see the
	// synced memory/skill/tool.
	if materialize {
		o.materializeInbound(ev.ArtifactID)
	}
	return ImportApplied, nil
}

func validateLegacyOuterMetadata(re proto.RemoteEvent, ev acf.Event) error {
	branch := ev.Branch
	if branch == "" {
		branch = acf.MainBranch
	}
	branch = normalizeBranchName(branch)
	wantWire := ev.EventID
	if re.Lane == LaneRetained {
		wantWire = RetainedWireEventID(ev.EventID, ev.Provenance.DeviceID)
	}
	checks := []struct {
		ok    bool
		field string
	}{{re.ArtifactID == ev.ArtifactID, "artifact"}, {re.EventID == wantWire, "wire-event"}, {re.Type == "" || re.Type == string(ev.Type), "type"}, {re.ParentHash == ev.ParentHash, "parent"}, {re.BranchID == "" || normalizeBranchName(re.BranchID) == branch, "branch"}, {re.Origin == "" || re.Origin == ev.Provenance.DeviceID, "origin"}, {re.SourceAgent == "" || re.SourceAgent == ev.Provenance.SourceAgent, "source-agent"}, {re.Timestamp.IsZero() || re.Timestamp.UnixNano() == ev.Timestamp.UnixNano(), "timestamp"}}
	for _, c := range checks {
		if !c.ok {
			return fmt.Errorf("syncd: inbound %s metadata does not match authenticated body", c.field)
		}
	}
	if ev.Hash != "" {
		wantHash, err := acf.ComputeHash(ev)
		if err != nil {
			return err
		}
		if wantHash != ev.Hash {
			return fmt.Errorf("syncd: inbound event hash mismatch")
		}
	}
	return nil
}

// ensureInboundArtifactShell makes sure the artifact record exists so
// AppendEvent's ReadArtifact + head-hash bookkeeping has a record to update. A
// first inbound event for an artifact we've never seen mints a minimal
// artifact carrying the scope/project identity the sender sealed alongside the
// event (so a project-scoped artifact stages/materializes to the right place
// rather than defaulting to global — P1-4); a known artifact is left as-is,
// scope and all (AppendEvent updates its head).
func (o *Orchestrator) ensureInboundArtifactShell(kind acf.Kind, ev acf.Event, scope acf.Scope, proj *project.ProjectInfo, namespaceIDs ...string) error {
	if existing, aerr := o.cfg.Store.ReadArtifact(kind, ev.ArtifactID); aerr == nil {
		// Preserve authenticated remote provenance outside the append log so a
		// later retention snapshot does not force startup backfill to decode the
		// entire compacted history. Only minimal inbound shells receive these
		// fields; native artifacts keep their path/name authorship unchanged.
		if existing.Name == "" && existing.SourcePath == "" &&
			(existing.RemoteOriginDeviceID != ev.Provenance.DeviceID || existing.RemoteSourceAgent != ev.Provenance.SourceAgent) {
			existing.RemoteOriginDeviceID = ev.Provenance.DeviceID
			existing.RemoteSourceAgent = ev.Provenance.SourceAgent
			if werr := o.cfg.Store.WriteArtifact(existing); werr != nil {
				return fmt.Errorf("syncd: update inbound artifact provenance: %w", werr)
			}
		}
		return nil
	}
	effScope := scope
	if effScope == "" {
		effScope = acf.ScopeGlobal
	}
	now := ev.Timestamp
	if now.IsZero() {
		now = time.Now().UTC()
	}
	shell := acf.Artifact{
		AcfSchemaVersion:     acf.SchemaVersion,
		ArtifactID:           ev.ArtifactID,
		Kind:                 kind,
		Scope:                effScope,
		CreatedAt:            now,
		UpdatedAt:            now,
		RemoteOriginDeviceID: ev.Provenance.DeviceID,
		RemoteSourceAgent:    ev.Provenance.SourceAgent,
	}
	if effScope == acf.ScopeNamespace {
		if len(namespaceIDs) != 1 || acf.ValidateWireUUIDv7(namespaceIDs[0]) != nil {
			return securityerr.ErrMetadataMismatch
		}
		shell.NamespaceID = namespaceIDs[0]
	}
	if effScope == acf.ScopeProject {
		shell.Project = o.resolveInboundProject(proj)
	}
	if werr := o.cfg.Store.WriteArtifact(shell); werr != nil {
		return fmt.Errorf("syncd: write inbound artifact shell: %w", werr)
	}
	return nil
}

// importLiveConversationEvent applies a lane=live conversation event: the
// origin's stored head event VERBATIM (aligned-chains design rule 4). When its
// ParentHash extends this device's head bookkeeping, a plain AppendEvent
// recomputes — by acf.ComputeHash determinism — exactly the hash the origin
// stored, so the two chains stay aligned with only the delta on the wire.
//
// A live event whose parent is unknown is NOT retried: it is typically a
// non-self-contained delta, and refetching it can never succeed until a
// baseline re-aligns the artifact. The event is intentionally dropped
// (ImportDeferredNeedsBaseline — the driver cursor advances past it), the
// artifact is marked needs-baseline, and recovery happens via the origin's
// always-published lane=retained full state (reconcileRetainedConversation
// adopts it as a baseline).
//
// After a successful append the recomputed (stored) hash is cross-checked
// against the wire-carried one — the alignment invariant made self-checking
// at the cost of one tail read; see the inline comment.
func (o *Orchestrator) importLiveConversationEvent(ev acf.Event, scope acf.Scope, proj *project.ProjectInfo, materialize bool) (ImportOutcome, error) {
	if serr := o.ensureInboundArtifactShell(acf.KindConversation, ev, scope, proj); serr != nil {
		return ImportRetryable, serr
	}
	// Capture the exact pre-append projection while it is still bound to the
	// current head. A live answer normally arrives as a tiny delta; composing it
	// with this cache avoids replaying a multi-gigabyte transcript before local
	// fan-out. A miss is safe and falls back to bounded materialization after the
	// authenticated append.
	prior, priorOK, priorErr := o.cfg.Store.ValidatedCachedMaterializedConversationPayload(ev.ArtifactID)
	if priorErr != nil {
		return ImportRetryable, fmt.Errorf("syncd: validate live conversation cache: %w", priorErr)
	}
	// The origin's stored-hash CLAIM, carried in the sealed body. AppendEvent
	// recomputes and overwrites the Hash on what it stores; ev (by value)
	// keeps the claim for the cross-check below.
	wireHash := ev.Hash
	if aerr := o.cfg.Store.AppendEvent(acf.KindConversation, ev); aerr != nil {
		if !errors.Is(aerr, acf.ErrHeadMismatch) {
			return ImportRetryable, fmt.Errorf("syncd: append live conversation event: %w", aerr)
		}
		o.markNeedsBaseline(ev.ArtifactID, ev.Branch, ev.EventID)
		return ImportDeferredNeedsBaseline, nil
	}
	// Integrity cross-check (cheap: one tail read). Alignment rests on
	// acf.ComputeHash determinism — any future regression (a new acf.Event
	// field without omitempty, JSON encoding drift across versions) would
	// misalign the chains SILENTLY: every artifact stuck in needs-baseline /
	// adopt churn with no diagnostic. Comparing the recomputed (stored) hash
	// against the origin's carried claim makes the invariant self-checking:
	// on mismatch, warn + surface remote.hash_mismatch and PRE-flag the
	// artifact needs-baseline — the origin's next delta chains onto ITS hash,
	// which this store then does not have. The append itself is durable and
	// content-correct, so the outcome stays ImportApplied (cursor advances)
	// and recovery is the normal retained-baseline adoption. Legacy events
	// with an empty carried hash skip the check.
	aligned := true
	if wireHash != "" {
		if stored, ok, herr := o.cfg.Store.LastEventByBranch(acf.KindConversation, ev.ArtifactID, normalizeBranchName(ev.Branch)); herr == nil && ok &&
			stored.EventID == ev.EventID && stored.Hash != wireHash {
			aligned = false
			if o.cfg.Logger != nil {
				o.cfg.Logger.Warn("remote: recomputed hash of verbatim live event differs from wire hash — hash-determinism regression? flagging needs-baseline",
					"artifact_id", ev.ArtifactID,
					"event_id", ev.EventID,
					"wire_hash", wireHash,
					"computed_hash", stored.Hash)
			}
			// Hashes are opaque digests (they already travel plaintext as
			// ParentHash on the wire) — ids only, zero-knowledge preserved.
			o.publishEvent("remote.hash_mismatch", map[string]any{
				"artifact_id":   ev.ArtifactID,
				"event_id":      ev.EventID,
				"wire_hash":     wireHash,
				"computed_hash": stored.Hash,
			})
			o.markNeedsBaseline(ev.ArtifactID, ev.Branch, ev.EventID)
		}
	}
	if aligned {
		// The chain extended natively, so the artifact is aligned through
		// this event — any needs-baseline mark is stale (the missing parent
		// arrived late, or the peer adopted OUR head on a tiebreak and its
		// deltas now chain onto it). A later delta that still lands on a gap
		// re-marks it.
		o.clearNeedsBaseline(ev.ArtifactID, ev.Branch)
	}
	o.primeCommittedLiveConversation(ev, prior, priorOK)
	if materialize {
		o.materializeInbound(ev.ArtifactID)
	}
	return ImportApplied, nil
}

// primeCommittedLiveConversation seeds the main-head projection immediately
// after a verified live append. Full prompt states can be cached directly;
// answer deltas compose with the cache captured before AppendEvent. A cold
// receiver performs one backward, anchor-bounded materialization instead of a
// full forward replay, then subsequent deltas stay on the O(new turn) path.
func (o *Orchestrator) primeCommittedLiveConversation(ev acf.Event, prior acf.ConversationPayload, priorOK bool) {
	if normalizeBranchName(ev.Branch) != acf.MainBranch || !acf.HasPayload(ev.Payload) {
		return
	}
	payload, err := acf.DecodeConversationPayload(ev)
	if err != nil {
		return
	}
	switch payload.Format {
	case acf.ConversationFormatV1:
		o.cfg.Store.PrimeMaterializedConversationAtHeadEvent(ev.ArtifactID, ev.EventID, payload)
		return
	case acf.ConversationDeltaFormatV1:
		if priorOK && prior.Format == acf.ConversationFormatV1 {
			prior.Events = append(append([]acf.ConversationEvent(nil), prior.Events...), payload.Events...)
			prior.Attachments = append(append([]acf.Attachment(nil), prior.Attachments...), payload.Attachments...)
			o.cfg.Store.PrimeMaterializedConversationAtHeadEvent(ev.ArtifactID, ev.EventID, prior)
			return
		}
	default:
		return
	}
	materialized, materializedHead, ok, err := o.cfg.Store.MaterializedConversationHeadFromStore(ev.ArtifactID)
	if err == nil && ok && materialized.Format == acf.ConversationFormatV1 {
		o.cfg.Store.PrimeMaterializedConversationAtHeadEvent(ev.ArtifactID, materializedHead.EventID, materialized)
	}
}

// reconcileRetainedConversation handles a lane=retained conversation event:
// the origin's full materialized state stamped with AlignedHead/AlignedEventID
// (aligned-chains design rule 4). It classifies inbound vs local CONTENT and
// re-aligns the local chain by ADOPTING the inbound state as a baseline
// (acf.Store.AdoptBaseline — a lossless append; prior local events stay in the
// log) where the pre-lane path used the destructive rebase:
//
//	no comparable local state → adopt (first contact / catch-up)
//	convEqual, same head      → ImportDeduped (already aligned)
//	convEqual, heads differ   → deterministic re-align tiebreak: the SMALLER
//	                            AlignedEventID wins (UUIDv7 — string order is
//	                            time order); when the EventIDs TIE (legacy-
//	                            rebase re-authored ids), the SMALLER
//	                            AlignedHead wins (string order — see the arm
//	                            for why any total order is sound). The losing
//	                            side adopts; the winning side dedupes and
//	                            waits for the peer's mirror-image tiebreak to
//	                            adopt OURS. Exactly one side adopts, so both
//	                            converge on one head with no publish
//	                            ping-pong.
//	convInboundStale          → skip (old redelivery; cursor advances)
//	convInboundExtends        → adopt (fast-forward re-align)
//	convDiverged              → union-merge (the unchanged legacy path) and
//	                            publish the merge; re-alignment then happens
//	                            on the next retained exchange via the
//	                            tiebreak above.
//
// handled=false with no side effects when the event lacks alignment metadata
// or carries a payload that is not comparable ConversationFormatV1 — the
// caller falls back to the legacy reconcile paths. Retained checkpoints are
// branch-scoped; a side-branch checkpoint can therefore restore a receiver
// that never observed the original fork ancestry.
func (o *Orchestrator) reconcileRetainedConversation(ev acf.Event, scope acf.Scope, proj *project.ProjectInfo, materialize bool) (ImportOutcome, bool, error) {
	if ev.AlignedHead == "" || ev.AlignedEventID == "" {
		return 0, false, nil
	}
	branch := normalizeBranchName(ev.Branch)
	inPayload, derr := acf.DecodeConversationPayload(ev)
	if derr != nil || inPayload.Format != acf.ConversationFormatV1 {
		return 0, false, nil
	}

	localHead, hasLocal, herr := conversationHeadForBranch(o.cfg.Store, ev.ArtifactID, branch)
	if errors.Is(herr, acf.ErrBranchNotFound) {
		herr = nil
		hasLocal = false
	}
	if herr != nil {
		return ImportRetryable, true, fmt.Errorf("syncd: read local branch conversation for retained reconcile: %w", herr)
	}
	var localPayload acf.ConversationPayload
	comparableLocal := false
	if hasLocal {
		if lp, lerr := acf.DecodeConversationPayload(localHead); lerr == nil && lp.Format == acf.ConversationFormatV1 {
			localPayload, comparableLocal = lp, true
		}
	}
	if !comparableLocal {
		// First contact, or local state that cannot be content-compared (e.g.
		// a legacy opaque session payload): adopting is lossless — whatever
		// local events exist stay in the log under the baseline.
		return o.adoptInboundBaseline(ev, scope, proj, materialize)
	}

	mergedAttachments := unionConversationAttachments(localPayload.Attachments, inPayload.Attachments)
	localHasAllAttachments := stringSlicesEqual(
		conversationAttachmentKeys(localPayload.Attachments),
		conversationAttachmentKeys(mergedAttachments),
	)
	inboundHasAllAttachments := stringSlicesEqual(
		conversationAttachmentKeys(inPayload.Attachments),
		conversationAttachmentKeys(mergedAttachments),
	)

	switch classifyConversationEvents(localPayload.Events, inPayload.Events) {
	case convEqual:
		if !localHasAllAttachments || !inboundHasAllAttachments {
			outcome, cerr := o.unionMergeInboundConversation(ev, localHead, localPayload, inPayload, materialize)
			return outcome, true, cerr
		}
		localHeadHash, localHeadID, lherr := o.localConversationHead(ev.ArtifactID, branch)
		if lherr != nil {
			return ImportRetryable, true, lherr
		}
		if localHeadHash == ev.AlignedHead {
			return ImportDeduped, true, nil // identical content, already aligned
		}
		if ev.AlignedEventID < localHeadID {
			return o.adoptInboundBaseline(ev, scope, proj, materialize)
		}
		// EQUAL EventIDs under DIFFERENT hashes (the hashes are guaranteed
		// unequal here — the aligned case returned above): reachable via
		// legacy-rebase histories where both devices re-authored an event
		// with the same EventID under different parents. Without its own arm
		// both sides dedupe and the artifact sits content-converged but
		// hash-misaligned — every live delta deferring needs-baseline — until
		// the next real commit. Break the tie on the HASH strings instead:
		// the smaller AlignedHead wins. ANY strict total order works here,
		// because the two devices evaluate the same predicate with the
		// operands swapped — this device compares (inbound=peerHash,
		// local=ourHash) while the peer compares (inbound=ourHash,
		// local=peerHash) — so with unequal hashes exactly one side sees
		// "inbound < local" and adopts; the other dedupes and converges when
		// the peer's mirror-image tiebreak adopts ours.
		if ev.AlignedEventID == localHeadID && ev.AlignedHead < localHeadHash {
			return o.adoptInboundBaseline(ev, scope, proj, materialize)
		}
		return ImportDeduped, true, nil // ours wins; the peer's tiebreak adopts ours

	case convInboundStale:
		if !localHasAllAttachments {
			outcome, cerr := o.unionMergeInboundConversation(ev, localHead, localPayload, inPayload, materialize)
			return outcome, true, cerr
		}
		o.publishEvent("remote.inbound_stale_skipped", map[string]any{
			"artifact_id": ev.ArtifactID,
			"event_id":    ev.EventID,
		})
		return ImportApplied, true, nil

	case convInboundExtends:
		if !inboundHasAllAttachments ||
			inboundOnlyReplaysLocalTurns(localPayload.Events, inPayload.Events) {
			outcome, cerr := o.unionMergeInboundConversation(ev, localHead, localPayload, inPayload, materialize)
			return outcome, true, cerr
		}
		return o.adoptInboundBaseline(ev, scope, proj, materialize)

	case convDiverged:
		outcome, cerr := o.unionMergeInboundConversation(ev, localHead, localPayload, inPayload, materialize)
		return outcome, true, cerr
	}
	return 0, false, nil
}

// localConversationHead returns the selected branch's head BOOKKEEPING hash
// and the EventID naming it, for the retained-lane tiebreak. The EventID is
// read from that branch's actual JSONL tail: after a native chain the tail IS the
// bookkeeping head; after a baseline adoption the tail is the baseline, which
// records the origin head's retained WIRE id (origin head EventID + "-r-" +
// origin discriminator — see RetainedWireEventID). The suffixed form still
// orders deterministically against the plain UUIDv7 ids peers advertise as
// AlignedEventID — a strict-prefix extension sorts AFTER its base and
// equal-length UUIDs never prefix each other — so both sides of a tiebreak
// keep computing the same winner.
func (o *Orchestrator) localConversationHead(artifactID, branchID string) (string, string, error) {
	branch := normalizeBranchName(branchID)
	art, aerr := o.cfg.Store.ReadArtifact(acf.KindConversation, artifactID)
	if aerr != nil {
		return "", "", fmt.Errorf("syncd: read artifact for retained tiebreak: %w", aerr)
	}
	head := ""
	if branch == acf.MainBranch {
		head = art.HeadEventHash
	}
	if art.BranchHeads != nil {
		if h := art.BranchHeads[branch]; h != "" {
			head = h
		}
	}
	tail, ok, terr := o.cfg.Store.LastEventByBranch(acf.KindConversation, artifactID, branch)
	if terr != nil {
		return "", "", fmt.Errorf("syncd: read local head for retained tiebreak: %w", terr)
	}
	if !ok {
		return "", "", fmt.Errorf("syncd: conversation %s branch %s has no local events for retained tiebreak", artifactID, branch)
	}
	return head, tail.EventID, nil
}

// adoptInboundBaseline appends the inbound full-state event as an
// EventTypeBaseline via acf.Store.AdoptBranchBaseline: the full payload becomes
// a local checkpoint chained onto the current head of its exact branch, and
// that branch's bookkeeping re-points at AlignedHead so the origin's subsequent
// VERBATIM deltas chain natively — both stores converge on identical head
// hashes (the alignment invariant). Lossless: prior local events stay in the
// log. The baseline records the retained WIRE EventID (the envelope-
// authoritative RetainedWireEventID, head + "-r-" + origin discriminator), so
// a redelivery of the SAME retained event dedupes on it directly — while a
// DIFFERENT origin's retained event for a legacy-re-authored twin of the same
// head EventID carries a different wire id and still reaches the reconcile
// tiebreak. A post-adoption redelivery of
// that commit's LIVE lane (plain head EventID) does NOT dedupe — its parent no
// longer matches the moved head, so it lands as a benign
// ImportDeferredNeedsBaseline (the needs-baseline set is a bounded HINT; the
// next native live append clears it) rather than a duplicate append.
//
// forwardCommitted is deliberately NOT called — adoption is not local
// authorship, and re-publishing adopted state would ping-pong retained events
// between already-aligned peers.
func (o *Orchestrator) adoptInboundBaseline(ev acf.Event, scope acf.Scope, proj *project.ProjectInfo, materialize bool) (ImportOutcome, bool, error) {
	if serr := o.ensureInboundArtifactShell(acf.KindConversation, ev, scope, proj); serr != nil {
		return ImportRetryable, true, serr
	}
	baseline := ev
	baseline.Type = acf.EventTypeBaseline
	baseline.ParentHash = "" // AdoptBranchBaseline chains it onto the exact branch head
	baseline.Hash = ""
	if aerr := o.cfg.Store.AdoptBranchBaseline(acf.KindConversation, baseline); aerr != nil {
		return ImportRetryable, true, fmt.Errorf("syncd: adopt inbound baseline: %w", aerr)
	}
	branch := normalizeBranchName(baseline.Branch)
	if branch == acf.MainBranch {
		if payload, perr := acf.DecodeConversationPayload(baseline); perr == nil && payload.Format == acf.ConversationFormatV1 {
			// The baseline body was already decoded and authenticated. Seed the
			// materialization cache before fan-out so the receiving device does not
			// immediately replay a very large historical log it just superseded.
			o.cfg.Store.PrimeMaterializedConversationAtHeadEvent(ev.ArtifactID, baseline.EventID, payload)
		}
	}
	o.clearNeedsBaseline(ev.ArtifactID, branch)
	o.publishEvent("remote.baseline_adopted", map[string]any{
		"artifact_id": ev.ArtifactID,
		"event_id":    ev.EventID,
		"branch_id":   branch,
	})
	if materialize {
		o.materializeInbound(ev.ArtifactID)
	}
	return ImportApplied, true, nil
}

// retainedConversationEvent builds the lane=retained companion of a committed
// conversation head: a copy of the head event whose payload is the FULL
// materialized conversation state (delta logs folded — the former
// remoteEnvelopeEvent logic, moved here for the two-lane split) and whose
// AlignedHead/AlignedEventID name the head's Hash/EventID, so a receiver can
// adopt the event as a baseline (acf.Store.AdoptBaseline) and re-align its
// chain onto the origin's hashes. The SEALED copy keeps the head's EventID —
// live and retained are the same logical commit on two lanes — but
// forwardCommitted publishes it under the DISTINCT, origin-scoped wire/outbox
// id RetainedWireEventID(head.EventID, origin) so the transport treats the
// lanes as two events; receivers dedupe retained events by CONTENT
// (convEqual), never by matching the live lane's EventID in the log.
//
// ok=false (nil error) when the artifact has no materializable payload: an
// empty or redaction-terminated log has no full state worth retaining — and
// re-publishing pre-redaction state on a retained topic would defeat the
// redaction — so the caller skips the retained lane. redacted (meaningful
// only alongside ok=false) distinguishes the two shapes: true when the log is
// redaction-TERMINATED (a redaction barrier cleared the state and nothing
// payload-bearing followed — the caller must CLEAR the transport's retained
// slot, which still serves the last pre-redaction snapshot), false when the
// log simply never carried a payload (nothing was ever retained).
func (o *Orchestrator) retainedConversationEvent(art acf.Artifact, head acf.Event) (retained acf.Event, ok, redacted bool, err error) {
	branch := normalizeBranchName(head.Branch)
	var payload json.RawMessage
	if branch == acf.MainBranch {
		materialized, found, materializeErr := o.cfg.Store.MaterializedConversationPayloadFromStore(art.ArtifactID)
		if materializeErr == nil && found {
			payload, materializeErr = acf.EncodePayload(materialized)
		}
		ok, err = found, materializeErr
	} else {
		var projected []acf.Event
		payload, projected, ok, err = o.cfg.Store.MaterializedConversationPayloadForBranch(art.ArtifactID, branch)
		if err == nil && !ok && len(projected) > 0 {
			redacted = projected[len(projected)-1].Type == acf.EventTypeRedaction
		}
	}
	if err != nil || !ok {
		if err != nil {
			return acf.Event{}, false, false, err
		}
		if branch != acf.MainBranch {
			return acf.Event{}, false, redacted, nil
		}
		// Determine whether the newest mutating event is a redaction from the
		// JSONL tail. The former whole-log ReadEvents call decoded gigabytes just
		// to answer this one-bit question after an empty projection.
		recent, rerr := o.cfg.Store.ReadRecentEvents(
			acf.KindConversation,
			art.ArtifactID,
			1,
			acf.EventTypeCreate,
			acf.EventTypeUpdate,
			acf.EventTypeResolution,
			acf.EventTypeSnapshot,
			acf.EventTypeBaseline,
			acf.EventTypeRedaction,
		)
		if rerr != nil {
			return acf.Event{}, false, false, rerr
		}
		return acf.Event{}, false, len(recent) == 1 && recent[0].Type == acf.EventTypeRedaction, nil
	}
	retained = head
	retained.Payload = payload
	retained.AlignedHead = head.Hash
	retained.AlignedEventID = head.EventID
	retained.Hash, err = acf.ComputeHash(retained)
	if err != nil {
		return acf.Event{}, false, false, err
	}
	return retained, true, false, nil
}

func isConversationDeltaEvent(kind acf.Kind, ev acf.Event) bool {
	if kind != acf.KindConversation || !acf.HasPayload(ev.Payload) {
		return false
	}
	p, err := acf.DecodeConversationPayload(ev)
	return err == nil && p.Format == acf.ConversationDeltaFormatV1
}

// reconcileInboundConversation handles a full-state inbound conversation event
// whose ParentHash does not extend the local chain (cross-device chains never
// share hashes after the first rebase, so EVERY cross-device conversation
// event lands here). It classifies inbound vs local at event granularity:
//
//	convEqual          → ImportDeduped (identical thread; nothing to do)
//	convInboundStale   → ImportApplied (old redelivery; drop, cursor advances)
//	convInboundExtends → lossless fast-forward rebase. The prefix relation
//	                     PROVES inbound is newer, so the wall-clock regression
//	                     guard is bypassed — this is the clock-skew fix.
//	convDiverged       → append a locally-authored union-merge event (never
//	                     delete the local chain), publish it so the peer
//	                     converges, then materialize.
//
// handled=false with no side effects when either side's payload is not a
// comparable ConversationFormatV1 (e.g. hermes SessionBundle) — the caller
// falls back to the legacy conflict/rebase paths.
func (o *Orchestrator) reconcileInboundConversation(ev acf.Event, materialize bool) (ImportOutcome, bool, error) {
	inPayload, err := acf.DecodeConversationPayload(ev)
	if err != nil || inPayload.Format != acf.ConversationFormatV1 {
		return 0, false, nil
	}
	events, rerr := o.cfg.Store.ReadEvents(acf.KindConversation, ev.ArtifactID)
	if rerr != nil {
		return ImportRetryable, true, fmt.Errorf("syncd: read local conversation for reconcile: %w", rerr)
	}
	localHead, hasLocal, herr := conversationHead(o.cfg.Store, ev.ArtifactID, events)
	if herr != nil {
		return ImportRetryable, true, herr
	}
	if !hasLocal {
		return 0, false, nil // nothing comparable locally; legacy rebase adopts inbound
	}
	localPayload, lerr := acf.DecodeConversationPayload(localHead)
	if lerr != nil || localPayload.Format != acf.ConversationFormatV1 {
		return 0, false, nil
	}

	mergedAttachments := unionConversationAttachments(localPayload.Attachments, inPayload.Attachments)
	localHasAllAttachments := stringSlicesEqual(
		conversationAttachmentKeys(localPayload.Attachments),
		conversationAttachmentKeys(mergedAttachments),
	)
	inboundHasAllAttachments := stringSlicesEqual(
		conversationAttachmentKeys(inPayload.Attachments),
		conversationAttachmentKeys(mergedAttachments),
	)

	switch classifyConversationEvents(localPayload.Events, inPayload.Events) {
	case convEqual:
		if localHasAllAttachments && inboundHasAllAttachments {
			return ImportDeduped, true, nil
		}
		outcome, cerr := o.unionMergeInboundConversation(ev, localHead, localPayload, inPayload, materialize)
		return outcome, true, cerr

	case convInboundStale:
		if !localHasAllAttachments {
			outcome, cerr := o.unionMergeInboundConversation(ev, localHead, localPayload, inPayload, materialize)
			return outcome, true, cerr
		}
		o.publishEvent("remote.inbound_stale_skipped", map[string]any{
			"artifact_id": ev.ArtifactID,
			"event_id":    ev.EventID,
		})
		return ImportApplied, true, nil

	case convInboundExtends:
		if !inboundHasAllAttachments ||
			inboundOnlyReplaysLocalTurns(localPayload.Events, inPayload.Events) {
			outcome, cerr := o.unionMergeInboundConversation(ev, localHead, localPayload, inPayload, materialize)
			return outcome, true, cerr
		}
		if rerr := o.rebaseInboundGuarded(acf.KindConversation, ev, false); rerr != nil {
			return ImportRetryable, true, fmt.Errorf("syncd: fast-forward inbound conversation: %w", rerr)
		}
		if materialize {
			o.materializeInbound(ev.ArtifactID)
		}
		return ImportApplied, true, nil

	case convDiverged:
		outcome, cerr := o.unionMergeInboundConversation(ev, localHead, localPayload, inPayload, materialize)
		return outcome, true, cerr
	}
	return 0, false, nil
}

// unionMergeInboundConversation is the shared convDiverged arm of the legacy
// (reconcileInboundConversation) and retained-lane
// (reconcileRetainedConversation) inbound reconciles: append a locally-
// authored union-merge event (never delete the local chain), publish it so
// the peer converges, then materialize. After a retained-lane merge the two
// devices hold equal content under DIFFERENT heads; the next retained
// exchange re-aligns them via the AlignedEventID tiebreak.
func (o *Orchestrator) unionMergeInboundConversation(
	ev acf.Event,
	localHead acf.Event,
	localPayload, inPayload acf.ConversationPayload,
	materialize bool,
) (ImportOutcome, error) {
	// A single adjacent identical assistant answer is ambiguous from payload
	// structure alone: it may be a legitimate repeated response. The adapter
	// that inspected the exact generated native session marks its corrective
	// full-state update, and peers honor that marker only when the complete
	// timestamp-agnostic historical shape also matches. Propagate the marker on
	// the locally-authored convergence event so a retained dirty peer cannot
	// reintroduce the echo in a later round.
	correctsDirtyLocalAdjacent := eventHasTag(ev, acf.LegacyAdjacentAssistantEchoRepairEventTag) &&
		acf.IsLegacyAdjacentAssistantEchoRepairCleanup(inPayload.Events, localPayload.Events)
	correctsDirtyInboundAdjacent := eventHasTag(localHead, acf.LegacyAdjacentAssistantEchoRepairEventTag) &&
		acf.IsLegacyAdjacentAssistantEchoRepairCleanup(localPayload.Events, inPayload.Events)
	var merged []acf.ConversationEvent
	switch {
	case correctsDirtyLocalAdjacent:
		merged = append([]acf.ConversationEvent(nil), inPayload.Events...)
	case correctsDirtyInboundAdjacent:
		merged = append([]acf.ConversationEvent(nil), localPayload.Events...)
	default:
		merged = unionConversationEvents(localPayload.Events, inPayload.Events)
	}
	mergedAttachments := unionConversationAttachments(localPayload.Attachments, inPayload.Attachments)
	mergedMatchesLocal := stringSlicesEqual(conversationEventKeys(merged), conversationEventKeys(localPayload.Events)) &&
		stringSlicesEqual(conversationAttachmentKeys(mergedAttachments), conversationAttachmentKeys(localPayload.Attachments))
	correctsDirtyInbound := acf.IsLegacyAssistantEchoCleanup(localPayload.Events, inPayload.Events) ||
		correctsDirtyInboundAdjacent
	correctsInboundAttachments := !stringSlicesEqual(
		conversationAttachmentKeys(mergedAttachments),
		conversationAttachmentKeys(inPayload.Attachments),
	)
	if mergedMatchesLocal && !correctsDirtyInbound && !correctsInboundAttachments {
		return ImportApplied, nil // inbound adds nothing new
	}
	payload, perr := acf.EncodePayload(acf.ConversationPayload{
		Format:      acf.ConversationFormatV1,
		Events:      merged,
		Attachments: mergedAttachments,
	})
	if perr != nil {
		return ImportRetryable, perr
	}
	deviceID := o.localDeviceID()
	if deviceID == "" {
		deviceID = localHead.Provenance.DeviceID
	}
	// The merge chains onto the branch HEAD BOOKKEEPING. For every legacy
	// tail that is last.Hash itself, but a baseline tail (aligned-chains
	// adoption) re-pointed the bookkeeping at its AlignedHead — chaining on
	// the baseline's own hash would be rejected with ErrHeadMismatch.
	parent := localHead.Hash
	if localHead.Type == acf.EventTypeBaseline {
		parent = localHead.AlignedHead
	}
	mergeEv := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: ev.ArtifactID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  time.Now().UTC(),
		ParentHash: parent,
		Branch:     localHead.Branch,
		Provenance: acf.Provenance{
			DeviceID:    deviceID,
			SourceAgent: localHead.Provenance.SourceAgent,
		},
		Payload: payload,
	}
	if correctsDirtyLocalAdjacent || correctsDirtyInboundAdjacent {
		mergeEv.EventTags = []string{acf.LegacyAdjacentAssistantEchoRepairEventTag}
	}
	if aerr := o.cfg.Store.AppendEvent(acf.KindConversation, mergeEv); aerr != nil {
		return ImportRetryable, fmt.Errorf("syncd: append conversation merge event: %w", aerr)
	}
	// unionMergeInboundConversation already holds the complete materialized
	// projection. Preserve it as the Store's head-bound cache before outbound
	// sealing and local fan-out; reconstructing it from a hundreds-of-megabytes
	// JSONL log here was the dominant CPU and memory spike on busy peers.
	if normalizeBranchName(mergeEv.Branch) == acf.MainBranch {
		o.cfg.Store.PrimeMaterializedConversationAtHeadEvent(ev.ArtifactID, mergeEv.EventID, acf.ConversationPayload{
			Format:      acf.ConversationFormatV1,
			Events:      merged,
			Attachments: mergedAttachments,
		})
	}
	o.publishEvent("remote.inbound_merged", map[string]any{
		"artifact_id":    ev.ArtifactID,
		"event_id":       ev.EventID,
		"merge_event":    mergeEv.EventID,
		"local_events":   len(localPayload.Events),
		"inbound_events": len(inPayload.Events),
		"merged_events":  len(merged),
		"attachments":    len(mergedAttachments),
	})
	// Publish the merged head FIRST (cross-device convergence must not
	// wait on local target agents — mirrors handleEvent's conversation-
	// first ordering), then materialize to native files. The merge event
	// carries LOCAL provenance, so forwardCommitted publishes it; that is
	// deliberate and is what closes the convergence loop.
	o.forwardCommitted(ev.ArtifactID)
	if materialize {
		o.materializeInbound(ev.ArtifactID)
	}
	return ImportApplied, nil
}

func eventHasTag(event acf.Event, want string) bool {
	for _, tag := range event.EventTags {
		if tag == want {
			return true
		}
	}
	return false
}

// rebaseInbound adopts a remote event as a fresh baseline for an artifact whose
// local chain has DIVERGED — the receiver missed earlier events, so the inbound
// event's ParentHash references history this device never got. It discards the
// stale local chain (DeleteArtifact) and writes the inbound event as a genesis
// (ParentHash "" — AppendEvent then accepts it and recomputes the hash). This
// is sound because acf events carry the full current artifact state, so the
// genesis alone reconstructs everything; the cost is that subsequent updates
// re-base again (the receiver's hash never matches the sender's), which is
// cheap for the cumulative-snapshot kinds this path serves.
//
// A timestamp guard prevents an out-of-order/backlog redelivery from REGRESSING
// the artifact to an older snapshot. The artifact's scope/project metadata is
// preserved across the rebase so materialisation still targets the right place.
//
// See rebaseInboundGuarded; this wrapper keeps the wall-clock regression guard
// for the legacy (non-conversation / uncomparable-payload) callers.
func (o *Orchestrator) rebaseInbound(kind acf.Kind, ev acf.Event) error {
	return o.rebaseInboundGuarded(kind, ev, true)
}

func (o *Orchestrator) rebaseInboundGuarded(kind acf.Kind, ev acf.Event, regressionGuard bool) error {
	if regressionGuard {
		if existing, err := o.cfg.Store.ReadEvents(kind, ev.ArtifactID); err == nil && len(existing) > 0 {
			if ev.Timestamp.Before(existing[len(existing)-1].Timestamp) {
				// Wall-clock anti-regression for cumulative snapshot kinds.
				// Comparisons here are usually same-origin-clock (an old
				// redelivery vs. that origin's own newer event), so skew is
				// not in play; conversations never reach this guard — they are
				// content-classified in reconcileInboundConversation. Surfaced
				// for visibility.
				o.publishEvent("remote.inbound_regression_skipped", map[string]any{
					"artifact_id": ev.ArtifactID,
					"event_id":    ev.EventID,
					"inbound_ts":  ev.Timestamp,
					"local_ts":    existing[len(existing)-1].Timestamp,
				})
				return nil
			}
		}
	}
	art, _ := o.cfg.Store.ReadArtifact(kind, ev.ArtifactID)
	if err := o.cfg.Store.DeleteArtifact(kind, ev.ArtifactID); err != nil {
		return err
	}
	if art.ArtifactID == "" {
		now := ev.Timestamp
		if now.IsZero() {
			now = time.Now().UTC()
		}
		art = acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion,
			ArtifactID:       ev.ArtifactID,
			Kind:             kind,
			Scope:            acf.ScopeGlobal,
			CreatedAt:        now,
		}
	}
	art.HeadEventHash = ""
	art.BranchHeads = nil
	art.Tombstoned = false
	art.UpdatedAt = ev.Timestamp
	if err := o.cfg.Store.WriteArtifact(art); err != nil {
		return err
	}
	genesis := ev
	genesis.ParentHash = "" // adopt as a fresh genesis; AppendEvent recomputes the hash
	if err := o.cfg.Store.AppendEvent(kind, genesis); err != nil {
		return err
	}
	if kind == acf.KindConversation && normalizeBranchName(genesis.Branch) == acf.MainBranch {
		if payload, err := acf.DecodeConversationPayload(genesis); err == nil && payload.Format == acf.ConversationFormatV1 {
			o.cfg.Store.PrimeMaterializedConversationAtHeadEvent(genesis.ArtifactID, genesis.EventID, payload)
		}
	}
	return nil
}

// recordInboundConflict handles the GENUINE-DIVERGENCE case of an inbound
// head-mismatch (P1-5): two real edits off a shared ancestor. It records a
// conflict between the local head and the inbound event and leaves the local
// chain INTACT (the inbound edit is not appended — it stays recoverable on the
// sender, and its content preview is captured in the conflict). This is the
// minimal BRD-04 §5.7 "neither is silently discarded" guarantee; full
// sibling/branch materialization of the remote head is a follow-up.
//
// If no conflict store is wired we still must NOT delete the local chain: we
// surface a remote.inbound_conflict event and keep local. Identical edits
// (SemanticallyEquivalent) are not a real conflict and are dropped silently.
func (o *Orchestrator) recordInboundConflict(kind acf.Kind, ev acf.Event) error {
	return o.recordInboundConflictWithDurability(kind, ev, false)
}

// recordInboundConflictWithDurability preserves the legacy best-effort
// behavior while making durable-cloud consumption fail closed. Once a cloud
// cursor is acknowledged, the sender and retention service may discard the
// ciphertext; a genuine sibling therefore cannot be terminal unless its full
// payload is already represented locally or has reached the fsynced conflict
// sidecar. Event-bus diagnostics alone are not durable ownership.
func (o *Orchestrator) recordInboundConflictWithDurability(kind acf.Kind, ev acf.Event, requireDurable bool) error {
	events, err := o.cfg.Store.ReadEvents(kind, ev.ArtifactID)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	localHead := events[len(events)-1]
	if conflicts.SemanticallyEquivalent(kind, localHead, ev) {
		return nil // same content from both sides — no real conflict
	}
	if o.cfg.ConflictStore == nil {
		o.publishEvent("remote.inbound_conflict", map[string]any{
			"artifact_id": ev.ArtifactID,
			"event_id":    ev.EventID,
			"reason":      "no conflict store; kept local, dropped inbound",
		})
		if requireDurable {
			return errors.New("syncd: durable inbound conflict store unavailable")
		}
		return nil
	}
	remoteHead := conflictHeadFromEvent(ev)
	if requireDurable {
		existing, getErr := o.cfg.ConflictStore.Get(ev.ArtifactID)
		switch {
		case getErr == nil:
			// A crash after the fsynced conflict sidecar but before terminal
			// inbox/cursor evidence must be an idempotent redelivery. A different
			// successor may not replace the only surviving full payload of the
			// already cloud-ACKed sibling.
			if existing.ArtifactID == ev.ArtifactID && existing.Kind == kind && conflictContainsExactDurableHead(existing, remoteHead) {
				return nil
			}
			return ErrInboundUnresolvedConflict
		case !errors.Is(getErr, conflicts.ErrNotRecorded):
			return getErr
		}
	}
	c := conflicts.Conflict{
		ArtifactID: ev.ArtifactID,
		Kind:       kind,
		Heads: []conflicts.Head{
			conflictHeadFromEvent(localHead),
			conflictHeadFromEvent(ev),
		},
	}
	if rerr := o.cfg.ConflictStore.Record(c); rerr != nil {
		return rerr
	}
	o.publishEvent("remote.inbound_conflict", map[string]any{
		"artifact_id": ev.ArtifactID,
		"event_id":    ev.EventID,
		"local_head":  localHead.EventID,
	})
	return nil
}

// ErrInboundUnresolvedConflict is content-free and retryable. Durable receive
// uses it to stop a later sibling before it can overwrite an earlier conflict
// payload whose cloud cursor may already have been acknowledged.
var ErrInboundUnresolvedConflict = errors.New("syncd: unresolved durable inbound conflict")

func conflictContainsExactDurableHead(existing conflicts.Conflict, wanted conflicts.Head) bool {
	for _, head := range existing.Heads {
		if head.SourceAgent != wanted.SourceAgent || head.EventID != wanted.EventID ||
			head.ContentSHA256 == "" || head.ContentSHA256 != wanted.ContentSHA256 || len(head.FullPayload) == 0 {
			continue
		}
		var have, want bytes.Buffer
		if json.Compact(&have, head.FullPayload) != nil || json.Compact(&want, wanted.FullPayload) != nil {
			continue
		}
		if bytes.Equal(have.Bytes(), want.Bytes()) {
			return true
		}
	}
	return false
}

// resolveInboundProject builds the local project identity for an inbound
// project-scoped artifact. It adopts the sender's project ID/VCS/ephemeral
// identity but resolves the filesystem Path from THIS device's registry — the
// wire Path is per-device and must never be trusted (a foreign absolute path
// could write outside the watched root). An unregistered project gets an empty
// Path, so the fan-out stage-and-wait gate (BRD-02 §4.13 / FR-02.38) parks the
// artifact until `aplexica project link` registers it and triggers a re-fanout.
func (o *Orchestrator) resolveInboundProject(proj *project.ProjectInfo) *project.ProjectInfo {
	if proj == nil || proj.ID == "" {
		return nil
	}
	out := &project.ProjectInfo{ID: proj.ID, VCS: proj.VCS, Ephemeral: proj.Ephemeral}
	if o.cfg.ProjectRegistry != nil {
		if e, ok := o.cfg.ProjectRegistry.Get(proj.ID); ok {
			out.Path = e.Path
		}
	}
	return out
}

// materializeInbound fans an inbound-imported artifact out to local agent
// native files. It reuses the orchestrator's normal fanOut path, which
// recursion-guard-marks every destination BEFORE writing — so the resulting
// watcher events are suppressed and the materialised native file is NOT
// re-imported and re-published back to the relay (the same guarantee that keeps
// ordinary local fan-out from looping).
//
// Loop safety, defence in depth: even if a guard-suppressed write somehow leaked
// a watcher event, the re-import would land on the SAME artifact id whose head
// event carries the remote origin device — and forwardCommitted skips remote-
// origin events. (The re-import would re-stamp local provenance, so the guard
// remains the primary protection; this is why fanOut guard-marks destinations.)
//
// The "primary" adapter passed to fanOut is the one fanOut SKIPS as the source.
// We resolve it from the imported event's SourceAgent (the agent that authored
// the artifact on the origin device) so its own native path is not redundantly
// written here; absent a match we fall back to the first adapter (a re-export to
// its own native path is idempotent). Best-effort: a missing artifact or empty
// adapter set is a silent no-op.
func (o *Orchestrator) materializeInbound(artifactID string) {
	_ = o.materializeInboundWithDurability(artifactID, false)
}

// ErrInboundNativeMaterialization is intentionally content-free. Durable
// receive maps it to a bounded retry code while adapter-specific details stay
// only in local diagnostics.
var ErrInboundNativeMaterialization = errors.New("syncd: inbound native materialization incomplete")

func (o *Orchestrator) materializeInboundWithDurability(artifactID string, strict bool) error {
	if o.cfg.Store == nil || len(o.cfg.Adapters) == 0 {
		return nil
	}
	art, found := o.findArtifact(artifactID)
	if !found {
		if strict {
			return ErrInboundNativeMaterialization
		}
		return nil
	}
	var primary adapter.Adapter
	sourceAgent := ""
	if head, ok, err := o.cfg.Store.LastEvent(art.Kind, artifactID); err == nil && ok {
		sourceAgent = head.Provenance.SourceAgent
		for _, ad := range o.cfg.Adapters {
			if ad.Name() == sourceAgent {
				primary = ad
				break
			}
		}
	} else if err != nil && strict {
		return ErrInboundNativeMaterialization
	}
	if primary == nil {
		primary = o.cfg.Adapters[0]
	}
	contextDir := ""
	if art.Scope == acf.ScopeProject && art.Project != nil && art.Project.Path != "" {
		contextDir = art.Project.Path
	} else {
		// Global/un-projected artifacts materialise relative to the watched dir
		// so adapters that resolve a project-relative native path have a base.
		contextDir = o.cfg.Dir
	}
	return o.fanOutWithOptions(context.Background(), primary, []string{artifactID}, contextDir, art.SourcePath, true,
		fanOutOptions{originAgent: &sourceAgent, strict: strict})
}

// MaterializeInboundArtifacts completes the second phase of a durable inbound
// delivery after its cloud ACK is committed. Empty ids are ignored and
// duplicates are collapsed so retrying a crash-interrupted finalize request is
// idempotent. The native-restore read lock preserves the same exclusion as the
// legacy one-phase import path.
func (o *Orchestrator) MaterializeInboundArtifacts(artifactIDs []string) {
	o.nativeRestoreGate.RLock()
	defer o.nativeRestoreGate.RUnlock()
	seen := make(map[string]struct{}, len(artifactIDs))
	for _, artifactID := range artifactIDs {
		if artifactID == "" {
			continue
		}
		if _, ok := seen[artifactID]; ok {
			continue
		}
		seen[artifactID] = struct{}{}
		o.materializeInbound(artifactID)
	}
}

// InboundCanonicalEvidence identifies the exact canonical record that proves a
// split-phase durable inbound event reached local storage. It intentionally
// contains no payload bytes.
type InboundCanonicalEvidence struct {
	FinalizeKind              string
	Kind                      acf.Kind
	ArtifactID                string
	EventID                   string
	EventHash                 string
	NoopReason                string
	AuthenticatedHeaderDigest string
	AuthenticatedSigner       string
}

const (
	// Evidence recovery is a fail-closed suffix lookup, not a forensic replay.
	// Normal delivery hits LastEvent; these caps cover a small concurrent-append
	// window without decoding payloads or walking multi-gigabyte history.
	inboundCanonicalEvidenceRecentMaxEvents = 64
	inboundCanonicalEvidenceRecentMaxBytes  = int64(64 << 20)
)

// CanonicalEvidenceForInbound resolves the exact wire EventID in a bounded
// suffix of the active canonical log after a non-materialising import. Normal
// live-delta delivery is a tail lookup; the metadata-only recent lookup covers
// only an idempotent redelivery or small concurrent local-append window. It
// deliberately never expands into compacted/full-history payload replay. A
// live delta must also preserve its authenticated origin hash byte-for-byte.
func (o *Orchestrator) CanonicalEvidenceForInbound(re proto.RemoteEvent) (InboundCanonicalEvidence, error) {
	o.nativeRestoreGate.RLock()
	defer o.nativeRestoreGate.RUnlock()
	if o.cfg.Store == nil || acf.ValidateKind(acf.Kind(re.Kind)) != nil || re.ArtifactID == "" || re.EventID == "" {
		return InboundCanonicalEvidence{}, securityerr.ErrMetadataMismatch
	}
	kind := acf.Kind(re.Kind)
	match := func(event acf.Event) bool {
		if event.EventID != re.EventID {
			return false
		}
		return re.Lane != LaneLive || re.EventHash != "" && event.Hash == re.EventHash
	}
	if tail, ok, err := o.cfg.Store.LastEvent(kind, re.ArtifactID); err == nil && ok && match(tail) {
		return InboundCanonicalEvidence{FinalizeKind: proto.InboundFinalizeCanonicalMaterialize, Kind: kind, ArtifactID: re.ArtifactID, EventID: tail.EventID, EventHash: tail.Hash}, nil
	}
	recent, found, err := o.cfg.Store.FindRecentEventIdentity(
		kind, re.ArtifactID, re.EventID,
		inboundCanonicalEvidenceRecentMaxEvents, inboundCanonicalEvidenceRecentMaxBytes,
	)
	if err != nil {
		return InboundCanonicalEvidence{}, err
	}
	if found && match(recent) {
		return InboundCanonicalEvidence{FinalizeKind: proto.InboundFinalizeCanonicalMaterialize, Kind: kind, ArtifactID: re.ArtifactID, EventID: recent.EventID, EventHash: recent.Hash}, nil
	}
	return InboundCanonicalEvidence{}, securityerr.ErrMetadataMismatch
}

// CanonicalEvidenceForTerminalInbound extends the strict wire-event lookup for
// terminal imports that intentionally consume an inbound event without
// appending its wire identity: a true non-conversation sibling conflict, or a
// retained conversation reconcile that is content-equal/stale/merged under a
// local canonical head. The current local head is the exact canonical state
// that remains authoritative and that native fan-out may safely re-materialize
// after cloud ACK. Conversation live deltas retain the strict event-id/hash
// requirement; their ancestry must never be collapsed into a head fallback.
func (o *Orchestrator) CanonicalEvidenceForTerminalInbound(re proto.RemoteEvent, outcome ImportOutcome) (InboundCanonicalEvidence, error) {
	if outcome == ImportSkipped {
		return o.authenticatedNoopEvidenceForInbound(re)
	}
	evidence, err := o.CanonicalEvidenceForInbound(re)
	if err == nil {
		if outcome == ImportDeduped {
			// A prior append can leave bytes visible after its fsync/close/dir
			// barrier or metadata write failed. Dedupe is terminal only after the
			// exact stored identity repeats those barriers and repairs metadata
			// under the store's artifact lock. A semantic retained dedupe whose
			// wire identity was never appended takes the canonical-head fallback
			// below instead.
			o.nativeRestoreGate.RLock()
			confirmed, confirmErr := o.cfg.Store.ConfirmEventDurableAndRepairMetadata(
				evidence.Kind, evidence.ArtifactID, evidence.EventID, evidence.EventHash,
			)
			o.nativeRestoreGate.RUnlock()
			if confirmErr != nil {
				return InboundCanonicalEvidence{}, confirmErr
			}
			evidence.EventID, evidence.EventHash = confirmed.EventID, confirmed.Hash
		}
		return evidence, nil
	}
	kind := acf.Kind(re.Kind)
	if !errors.Is(err, securityerr.ErrMetadataMismatch) ||
		(outcome != ImportApplied && outcome != ImportDeduped) ||
		(kind == acf.KindConversation && re.Lane != LaneRetained) ||
		acf.ValidateKind(kind) != nil || re.ArtifactID == "" {
		return InboundCanonicalEvidence{}, err
	}
	o.nativeRestoreGate.RLock()
	defer o.nativeRestoreGate.RUnlock()
	if o.cfg.Store == nil {
		return InboundCanonicalEvidence{}, securityerr.ErrMetadataMismatch
	}
	head, ok, headErr := o.cfg.Store.LastEvent(kind, re.ArtifactID)
	if headErr != nil {
		return InboundCanonicalEvidence{}, headErr
	}
	if !ok || head.EventID == "" || head.Hash == "" {
		return InboundCanonicalEvidence{}, securityerr.ErrMetadataMismatch
	}
	return InboundCanonicalEvidence{FinalizeKind: proto.InboundFinalizeCanonicalMaterialize, Kind: kind, ArtifactID: re.ArtifactID, EventID: head.EventID, EventHash: head.Hash}, nil
}

func (o *Orchestrator) authenticatedNoopEvidenceForInbound(re proto.RemoteEvent) (InboundCanonicalEvidence, error) {
	if !o.cfg.RequireEnvelopeV2 || acf.ValidateKind(acf.Kind(re.Kind)) != nil || re.ArtifactID == "" || re.EventID == "" {
		return InboundCanonicalEvidence{}, securityerr.ErrMetadataMismatch
	}
	provider := o.verifiedRosterProvider()
	identityProvider := o.v2IdentityProvider()
	if provider == nil || identityProvider == nil {
		return InboundCanonicalEvidence{}, securityerr.ErrMetadataMismatch
	}
	scopeType, scopeID := "account", ""
	if re.KeyMode == "namespace-key-v1" {
		scopeType, scopeID = "namespace", re.NamespaceID
	}
	snapshot, err := provider.Current(context.Background(), scopeType, scopeID)
	if err != nil || snapshot.BarrierID != re.SecurityBarrierID ||
		snapshot.CoordinatorGeneration != re.SecurityGeneration ||
		snapshot.Roster.Manifest.Manifest.AccessGeneration != re.AccessGeneration ||
		snapshot.Roster.Manifest.Manifest.AccessSetHash != re.AccessSetHash ||
		snapshot.KeyMode != re.KeyMode || snapshot.KeyVersion != re.KeyVersion {
		return InboundCanonicalEvidence{}, securityerr.ErrMetadataMismatch
	}
	localIdentity, err := identityProvider.Identity()
	if err != nil {
		return InboundCanonicalEvidence{}, err
	}
	// Same v2/v3 dispatch as ImportInbound: the durable authenticated-no-op
	// evidence path must accept every envelope generation this device
	// advertises it can decode.
	openAuthenticated := OpenEnvelopeV2AuthenticatedWithNamespaceProvider
	if envelopeIsV3(re.Bytes) {
		openAuthenticated = OpenEnvelopeV3AuthenticatedWithNamespaceProvider
	}
	_, authenticated, openErr := openAuthenticated(
		re.Bytes, snapshot.Roster, o.localDeviceID(), localIdentity.WrapPrivate, o.namespaceKeyProvider(),
	)
	if authenticated.SignerDeviceID == "" || authenticated.Header.SecurityBarrierID != snapshot.BarrierID ||
		authenticated.Header.TreeHeadDigest != snapshot.TreeHeadDigest ||
		validateAuthenticatedInboundOuter(re, authenticated.Header, true) != nil {
		return InboundCanonicalEvidence{}, securityerr.ErrMetadataMismatch
	}
	noopReason := ""
	switch {
	case errors.Is(openErr, errNotARecipient) && !re.Clear:
		noopReason = proto.InboundFinalizeNoopNotRecipient
	default:
		if openErr != nil {
			return InboundCanonicalEvidence{}, openErr
		}
		return InboundCanonicalEvidence{}, securityerr.ErrMetadataMismatch
	}
	return InboundCanonicalEvidence{
		FinalizeKind:              proto.InboundFinalizeAuthenticatedNoop,
		Kind:                      acf.Kind(re.Kind),
		ArtifactID:                re.ArtifactID,
		NoopReason:                noopReason,
		AuthenticatedHeaderDigest: hex.EncodeToString(authenticated.HeaderAADSHA256[:]),
		AuthenticatedSigner:       authenticated.SignerDeviceID + ":" + hex.EncodeToString(authenticated.SignerKeyID[:]),
	}, nil
}

// FinalizeInboundCanonicalEvidence performs only native fan-out. The exact
// canonical record is re-verified first so an unknown, stale, or substituted
// finalize request cannot materialise an unrelated artifact. Cursor and cloud
// acknowledgement state are deliberately outside this method and are never
// changed here.
func (o *Orchestrator) FinalizeInboundCanonicalEvidence(evidence InboundCanonicalEvidence) error {
	o.nativeRestoreGate.RLock()
	defer o.nativeRestoreGate.RUnlock()
	if evidence.FinalizeKind != proto.InboundFinalizeCanonicalMaterialize || o.cfg.Store == nil || acf.ValidateKind(evidence.Kind) != nil || evidence.ArtifactID == "" || evidence.EventID == "" || evidence.EventHash == "" {
		return securityerr.ErrMetadataMismatch
	}
	if tail, ok, err := o.cfg.Store.LastEvent(evidence.Kind, evidence.ArtifactID); err == nil && ok && tail.EventID == evidence.EventID && tail.Hash == evidence.EventHash {
		// A terminal redaction is already fully represented by the fsynced
		// canonical tombstone. Export paths intentionally have no payload to
		// write and may otherwise fall back to older materialized content. Treat
		// the exact tombstone as a successful native no-op: replay batches never
		// expose the superseded state, and finalize remains restart-idempotent.
		if tail.Type == acf.EventTypeRedaction {
			return nil
		}
		return o.materializeInboundWithDurability(evidence.ArtifactID, true)
	}
	recent, found, err := o.cfg.Store.FindRecentEventIdentity(
		evidence.Kind, evidence.ArtifactID, evidence.EventID,
		inboundCanonicalEvidenceRecentMaxEvents, inboundCanonicalEvidenceRecentMaxBytes,
	)
	if err != nil {
		return err
	}
	if found && recent.EventID == evidence.EventID && recent.Hash == evidence.EventHash {
		return o.materializeInboundWithDurability(evidence.ArtifactID, true)
	}
	return securityerr.ErrMetadataMismatch
}

// needsBaselineMaxEntries bounds the in-memory needsBaseline set. 256 mirrors
// remoteRepublishMaxHeads: the set is a bounded recovery HINT, not durable
// state — an evicted artifact simply re-marks on its next deferred delta, and
// its recovery path (the origin's always-published retained event) does not
// depend on the entry existing.
const needsBaselineMaxEntries = 256

// needsBaselineNotifyInterval throttles the remote.needs_baseline bus event to
// at most one per artifact branch per interval, so a burst of unknown-parent
// deltas for one wedged branch (e.g. an active session streaming turns) cannot
// flood the event stream. Matches the oversizeReportInterval throttle idiom.
const needsBaselineNotifyInterval = time.Minute

func needsBaselineKey(artifactID, branchID string) string {
	return artifactID + "\x00" + normalizeBranchName(branchID)
}

// markNeedsBaseline records that one artifact branch's live lane is unusable
// until a retained-lane checkpoint re-aligns it, and surfaces
// remote.needs_baseline on the event bus (ids only — zero-knowledge;
// throttled per branch per needsBaselineNotifyInterval). Bounded by
// needsBaselineMaxEntries with stalest-entry eviction.
func (o *Orchestrator) markNeedsBaseline(artifactID, branchID, eventID string) {
	if artifactID == "" {
		return
	}
	branch := normalizeBranchName(branchID)
	key := needsBaselineKey(artifactID, branch)
	now := time.Now()
	notify := false
	o.mu.Lock()
	if o.needsBaseline == nil {
		o.needsBaseline = map[string]time.Time{}
	}
	last, exists := o.needsBaseline[key]
	switch {
	case !exists:
		if len(o.needsBaseline) >= needsBaselineMaxEntries {
			var oldestID string
			var oldest time.Time
			for id, ts := range o.needsBaseline {
				if oldestID == "" || ts.Before(oldest) {
					oldestID, oldest = id, ts
				}
			}
			delete(o.needsBaseline, oldestID)
		}
		o.needsBaseline[key] = now
		notify = true
	case now.Sub(last) >= needsBaselineNotifyInterval:
		o.needsBaseline[key] = now
		notify = true
	}
	o.mu.Unlock()
	if notify {
		o.publishEvent("remote.needs_baseline", map[string]any{
			"artifact_id": artifactID,
			"event_id":    eventID,
			"branch_id":   branch,
		})
	}
}

// clearNeedsBaseline drops one artifact branch from the needs-baseline set
// (no-op when absent). A checkpoint on one branch must never conceal a gap on
// another branch of the same artifact.
func (o *Orchestrator) clearNeedsBaseline(artifactID, branchID string) {
	o.mu.Lock()
	delete(o.needsBaseline, needsBaselineKey(artifactID, branchID))
	o.mu.Unlock()
}

// needsBaselinePending reports whether any branch of artifactID is currently
// waiting for a retained-lane baseline. It preserves the artifact-level test
// and status contract while the internal authority stays branch-scoped.
func (o *Orchestrator) needsBaselinePending(artifactID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	prefix := artifactID + "\x00"
	for key := range o.needsBaseline {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func (o *Orchestrator) needsBaselineBranchPending(artifactID, branchID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.needsBaseline[needsBaselineKey(artifactID, branchID)]
	return ok
}

// markRemoteOrigin records deviceID as a remote-authored origin for the
// outbound loop guard. Idempotent; guarded by o.mu.
func (o *Orchestrator) markRemoteOrigin(deviceID string) {
	if deviceID == "" {
		return
	}
	if o.localDeviceID() != "" && deviceID == o.localDeviceID() {
		return
	}
	o.mu.Lock()
	if o.remoteOrigins == nil {
		o.remoteOrigins = map[string]struct{}{}
	}
	o.remoteOrigins[deviceID] = struct{}{}
	o.mu.Unlock()
}

// buildRemoteRepublishedHeadIndex seeds the republish dedup index at daemon
// startup with every artifact's current head so an unchanged store does not
// replay wholesale after a restart. Seeded entries carry an UNKNOWN recipient
// fingerprint and published=false: the roster-change comparison skips them,
// and the slow backfill trickle (BackfillLocalRemoteHeads) is what eventually
// re-publishes their retained baselines.
func buildRemoteRepublishedHeadIndex(store *acf.Store) map[string]remoteRepublishedHead {
	idx := map[string]remoteRepublishedHead{}
	if store == nil {
		return idx
	}
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		artifacts, err := store.ListArtifacts(kind)
		if err != nil {
			continue
		}
		for _, art := range artifacts {
			if art.ArtifactID == "" || art.HeadEventHash == "" {
				continue
			}
			// Artifact bookkeeping already carries every branch head. Do not read
			// the append-order tail here: startup index construction runs for the
			// whole store, and one legacy full-state conversation can be a single
			// 100+ MB JSON line. The exact event is read only if a later sweep
			// proves this hash actually needs publication.
			idx[remoteHeadKey(art.ArtifactID, acf.MainBranch)] = remoteRepublishedHead{headHash: art.HeadEventHash}
			for branch, headHash := range art.BranchHeads {
				if headHash == "" {
					continue
				}
				idx[remoteHeadKey(art.ArtifactID, branch)] = remoteRepublishedHead{headHash: headHash}
			}
		}
	}
	return idx
}

// isRemoteOrigin reports whether deviceID has been observed as the origin of an
// inbound (remote-authored) event. The empty device id is never remote (an
// un-attributed local edit). Guarded by o.mu.
func (o *Orchestrator) isRemoteOrigin(deviceID string) bool {
	if deviceID == "" {
		return false
	}
	// A device id equal to THIS device's cloud identity is local by
	// definition, regardless of any stale remoteOrigins entry.
	if o.localDeviceID() != "" && deviceID == o.localDeviceID() {
		return false
	}
	o.mu.Lock()
	_, ok := o.remoteOrigins[deviceID]
	o.mu.Unlock()
	return ok
}

// remoteRepublishedHead is one entry of the republish dedup index
// (o.remoteRepublishedHeads): the last head hash handed to the remote
// publisher for an artifact+branch, plus the identity of the recipient roster the
// envelope was sealed for. A retained baseline is only useful to the devices
// it was ENCRYPTED for — the same head sealed for a different roster is a
// different wire artifact, so dedup must key on both.
type remoteRepublishedHead struct {
	headHash string
	// recipientsFP is the sorted recipient-device-id fingerprint the head was
	// last sealed for. Empty means UNKNOWN (an entry seeded at daemon startup
	// by buildRemoteRepublishedHeadIndex, before any publish this run); an
	// unknown fingerprint never triggers a roster-change republish, so a
	// restart keeps today's no-flood behavior and the slow backfill trickle
	// covers those artifacts instead.
	recipientsFP string
	// published is true once the artifact's head was actually handed to the
	// remote publisher during THIS daemon run (false = merely seeded at
	// startup). The backfill trickle uses it to find artifacts that never got
	// a retained baseline published.
	published bool
}

type largeRetainedAttempt struct {
	at           time.Time
	recipientsFP string
	headHash     string
}

const defaultLargeRetainedBaselineMinInterval = 5 * time.Minute

var largeRetainedBaselineMinInterval = defaultLargeRetainedBaselineMinInterval

// deferLargeRetainedBaseline atomically reserves the next expensive retained
// rebuild for a large native conversation. It returns true when a recent
// attempt for the same recipient roster is still fresh. Recording the attempt
// before serialization also coalesces concurrent forwardCommitted calls and
// prevents a persistent seal failure from becoming a CPU loop.
func (o *Orchestrator) deferLargeRetainedBaseline(art acf.Artifact, head acf.Event, recipientsFP string) bool {
	if head.Type == acf.EventTypeRedaction {
		key := remoteHeadKey(art.ArtifactID, head.Branch)
		o.mu.Lock()
		delete(o.remoteLargeRetainedAttempts, key)
		o.mu.Unlock()
		return false
	}
	if !o.conversationExceedsLargeThresholdBeforeReplay(art) {
		return false
	}

	now := time.Now()
	key := remoteHeadKey(art.ArtifactID, head.Branch)
	o.mu.Lock()
	defer o.mu.Unlock()
	if previous, ok := o.remoteLargeRetainedAttempts[key]; ok &&
		previous.recipientsFP == recipientsFP &&
		now.Sub(previous.at) < largeRetainedBaselineMinInterval {
		// Coalesce head churn without postponing the original deadline. The
		// retained retry must eventually publish the NEWEST head, but every live
		// delta in a busy large conversation must not bypass the cadence and
		// rebuild the complete baseline immediately.
		previous.headHash = art.HeadEventHash
		o.remoteLargeRetainedAttempts[key] = previous
		return true
	}
	if o.remoteLargeRetainedAttempts == nil {
		o.remoteLargeRetainedAttempts = map[string]largeRetainedAttempt{}
	}
	o.remoteLargeRetainedAttempts[key] = largeRetainedAttempt{at: now, recipientsFP: recipientsFP, headHash: art.HeadEventHash}
	return false
}

// recordRetainedBaselineFailure applies the same bounded retry cadence to a
// retained rebuild that failed for a smaller conversation. Otherwise a
// startup-seeded head remains unpublished in the in-memory index and the
// one-minute safety sweep repeats the same expensive parse/seal forever.
func (o *Orchestrator) recordRetainedBaselineFailure(art acf.Artifact, head acf.Event, recipientsFP string) {
	key := remoteHeadKey(art.ArtifactID, head.Branch)
	o.mu.Lock()
	if o.remoteLargeRetainedAttempts == nil {
		o.remoteLargeRetainedAttempts = map[string]largeRetainedAttempt{}
	}
	o.remoteLargeRetainedAttempts[key] = largeRetainedAttempt{
		at:           time.Now(),
		recipientsFP: recipientsFP,
		headHash:     art.HeadEventHash,
	}
	o.mu.Unlock()
}

func (o *Orchestrator) completeLargeRetainedBaseline(artifactID, branch string) {
	key := remoteHeadKey(artifactID, branch)
	o.mu.Lock()
	delete(o.remoteLargeRetainedAttempts, key)
	o.mu.Unlock()
}

// recipientsFingerprint is the recipient-set identity recorded in the
// republish dedup index: the sorted recipient device ids, joined. Wrap-key
// bytes are deliberately excluded — key rotation has its own re-wrap machinery
// by the key-rotation service and must not force a full republish sweep.
func recipientsFingerprint(recipients []recipient) string {
	return strings.Join(recipientDeviceIDs(recipients), "\n")
}

// currentRecipientsFingerprint resolves the CURRENT roster fingerprint for the
// republish dedup comparison. A resolver error or an empty set returns ""
// (unknown): fingerprint comparison is disabled for the pass — outbound
// publishes would be dropped anyway (never plaintext), and burning the dedup
// entries on a degraded roster would flood the relay when it recovers.
func (o *Orchestrator) currentRecipientsFingerprint(namespaceID string) string {
	recipients, err := o.resolveRecipients(namespaceID)
	if err != nil || len(recipients) == 0 {
		return ""
	}
	return recipientsFingerprint(recipients)
}

// shouldRepublishLocalRemoteHead reports whether the artifact's head must be
// (re-)handed to the remote publisher: the head hash changed, OR the head is
// unchanged but was last sealed for a DIFFERENT recipient roster than
// currentFP (roster recovered/changed — peers added since the last seal can
// never decrypt the retained baseline on the wire). An empty fingerprint on
// either side (unknown roster now, or a startup-seeded entry) skips the
// roster comparison. A head whose retained baseline is known OVER-CAP
// (o.remoteRetainedOversized) is never republished while unchanged:
// re-attempting would re-materialize + re-seal hundreds of MB only to be
// refused again, and remote.outbound_oversized already surfaced the gap.
func (o *Orchestrator) shouldRepublishLocalRemoteHead(artifactID, branch, headHash, currentFP string) bool {
	if artifactID == "" || headHash == "" {
		return false
	}
	key := remoteHeadKey(artifactID, branch)
	o.mu.Lock()
	defer o.mu.Unlock()
	if h, oversized := o.remoteRetainedOversized[key]; oversized && h == headHash {
		return false
	}
	attempt, pending := o.remoteLargeRetainedAttempts[key]
	retryMatches := pending &&
		attempt.headHash == headHash &&
		(currentFP == "" || attempt.recipientsFP == currentFP)
	if retryMatches && time.Since(attempt.at) < largeRetainedBaselineMinInterval {
		return false
	}
	entry, ok := o.remoteRepublishedHeads[key]
	if !ok || entry.headHash != headHash {
		return true
	}
	if currentFP != "" && entry.recipientsFP != "" && entry.recipientsFP != currentFP {
		return true
	}
	// A deferred/failed large retained rebuild is retried only at its bounded
	// cadence. The live lane is already marked above, so ordinary one-minute
	// safety sweeps remain cheap between attempts.
	if retryMatches {
		return true
	}
	return false
}

// shouldRepublishSeededConversationHead lets the recent reconnect sweep repair
// conversation baselines that were merely startup-seeded into the dedup index.
// Startup seeds prevent a wholesale replay after daemon restart, but
// conversations have a durable retained recovery slot: if the daemon restarted
// after a live delta was observed but before that retained slot existed on the
// broker, the normal same-hash dedup would skip the only fast repair path.
func (o *Orchestrator) shouldRepublishSeededConversationHead(artifactID, branch, headHash, currentFP string) bool {
	if artifactID == "" || headHash == "" {
		return false
	}
	key := remoteHeadKey(artifactID, branch)
	o.mu.Lock()
	defer o.mu.Unlock()
	if h, oversized := o.remoteRetainedOversized[key]; oversized && h == headHash {
		return false
	}
	if attempt, pending := o.remoteLargeRetainedAttempts[key]; pending &&
		attempt.headHash == headHash &&
		(currentFP == "" || attempt.recipientsFP == currentFP) &&
		time.Since(attempt.at) < largeRetainedBaselineMinInterval {
		return false
	}
	entry, ok := o.remoteRepublishedHeads[key]
	return ok && entry.headHash == headHash && !entry.published
}

// markRetainedOversized records that artifactID's sealed retained baseline at
// headHash exceeded remotePublishRetainedMaxBytes — see the field doc for the
// skip semantics.
func (o *Orchestrator) markRetainedOversized(artifactID, branch, headHash string) {
	if artifactID == "" || headHash == "" {
		return
	}
	key := remoteHeadKey(artifactID, branch)
	o.mu.Lock()
	if o.remoteRetainedOversized == nil {
		o.remoteRetainedOversized = map[string]string{}
	}
	o.remoteRetainedOversized[key] = headHash
	o.mu.Unlock()
}

// clearRetainedOversized drops artifactID's oversized-retained mark (no-op
// when absent). Called when the retained lane succeeds again: a published
// baseline or a retained-slot clear.
func (o *Orchestrator) clearRetainedOversized(artifactID, branch string) {
	key := remoteHeadKey(artifactID, branch)
	o.mu.Lock()
	delete(o.remoteRetainedOversized, key)
	o.mu.Unlock()
}

func (o *Orchestrator) markRepublishedLocalRemoteHead(artifactID, branch, headHash, recipientsFP string) {
	if artifactID == "" || headHash == "" {
		return
	}
	key := remoteHeadKey(artifactID, branch)
	o.mu.Lock()
	if o.remoteRepublishedHeads == nil {
		o.remoteRepublishedHeads = map[string]remoteRepublishedHead{}
	}
	o.remoteRepublishedHeads[key] = remoteRepublishedHead{
		headHash:     headHash,
		recipientsFP: recipientsFP,
		published:    true,
	}
	o.mu.Unlock()
}

func remoteHeadKey(artifactID, branch string) string {
	return artifactID + "\x00" + normalizeBranchName(branch)
}

// normalizeBranchName maps the wire-empty branch to acf.MainBranch so the
// OutboundEvent always carries an explicit branch id for the relay.
func normalizeBranchName(b string) string {
	if b == "" {
		return acf.MainBranch
	}
	norm, err := acf.NormalizeBranchName(b)
	if err != nil {
		return acf.MainBranch
	}
	return norm
}

// resolveRecipients returns the device recipient set the outbound envelope is
// sealed for, via the configured RecipientResolver. A nil resolver yields an
// empty set (so the caller drops the event rather than sending plaintext).
func (o *Orchestrator) resolveRecipients(namespaceID string) ([]recipient, error) {
	res := o.recipientResolver()
	if res == nil {
		return nil, nil
	}
	rs, err := res.Recipients(namespaceID)
	if err != nil {
		return nil, err
	}
	out := make([]recipient, 0, len(rs))
	for _, r := range rs {
		if r.DeviceID == "" {
			continue
		}
		out = append(out, recipient{deviceID: r.DeviceID, pub: r.PubKey})
	}
	return out, nil
}

func recipientDeviceIDs(recipients []recipient) []string {
	ids := make([]string, 0, len(recipients))
	for _, r := range recipients {
		if r.deviceID != "" {
			ids = append(ids, r.deviceID)
		}
	}
	sort.Strings(ids)
	return ids
}

// devicePrivateKey loads this device's X25519 private wrap key via the
// configured DeviceKeyProvider (used to open inbound envelopes). Returns an
// error when no provider is wired — inbound decryption is impossible without it.
func (o *Orchestrator) devicePrivateKey() ([keys.X25519KeySize]byte, error) {
	kp := o.deviceKeyProvider()
	if kp == nil {
		return [keys.X25519KeySize]byte{}, fmt.Errorf("syncd: no device key provider configured")
	}
	return kp.Private()
}

// Recipient is one device the outbound envelope is encrypted for: its cloud
// device id + its registered X25519 wrap public key. Returned by a
// RecipientResolver.
type Recipient struct {
	DeviceID string
	PubKey   [keys.X25519KeySize]byte
}

// RecipientResolver resolves the recipient device set for a namespace so the
// orchestrator can seal each outbound event to every authorised device. The
// daemon backs this with RemoteRunner.ListNamespaceDevices (cached) and ALWAYS
// includes this device so a sender can decrypt its own re-imports.
//
// Declared HERE (not imported from internal/daemon) to keep the dependency edge
// one-way: internal/sync must not import internal/daemon. The daemon's resolver
// satisfies it structurally.
//
// CONTRACT: an empty slice means "no recipients" — the orchestrator then DROPS
// the outbound event. The resolver MUST NOT return a set that would let the
// orchestrator transmit a body no device can decrypt; returning empty (drop) is
// always safer than returning a stale/partial set.
type RecipientResolver interface {
	Recipients(namespaceID string) ([]Recipient, error)
}

// DeviceKeyProvider hands the orchestrator this device's X25519 private wrap
// key for opening inbound envelopes. The daemon backs it with
// keys.DeviceKeyStore.LoadOrCreate (the same key registered with the cloud at
// pairing). Declared here to keep the one-way dependency edge.
type DeviceKeyProvider interface {
	Private() ([keys.X25519KeySize]byte, error)
}

type RosterSnapshot struct {
	Roster                identity.VerifiedRoster
	BarrierID             [32]byte
	TreeHeadDigest        [32]byte
	KeyMode               string
	KeyVersion            uint64
	CoordinatorGeneration uint64
}

type VerifiedRosterProvider interface {
	Current(ctx context.Context, scopeType, scopeID string) (RosterSnapshot, error)
}

type V2IdentityProvider interface {
	Identity() (keys.DeviceIdentity, error)
}

// SubscribeActiveNamespaces drives the receive-side namespace gate: it calls
// Subscribe on the supplied subscriber for each namespace id so the plugin
// admits inbound events for them. Errors are returned joined for the caller to
// log; a per-namespace failure does not stop the others.
//
// V1 NOTE: acf artifacts carry no namespace id, so the daemon's notion of
// "active namespaces" is whatever the caller supplies (the paired device's
// namespace set, surfaced via remote.enumerate). With an empty namespace this
// is a no-op. See the daemon wiring + the report's blocker list.
func SubscribeActiveNamespaces(ctx context.Context, sub NamespaceSubscriber, namespaceIDs []string) error {
	var firstErr error
	for _, ns := range namespaceIDs {
		if ns == "" {
			continue
		}
		if err := sub.Subscribe(ctx, ns); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// NamespaceSubscriber is the narrow seam the sync driver loop uses to express
// inbound interest. The daemon's RemoteRunner satisfies it structurally.
type NamespaceSubscriber interface {
	Subscribe(ctx context.Context, namespaceID string) error
}
