package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

var errDurableInboundMetadata = errors.New("remote: invalid durable inbound metadata")

const (
	durableInboundIdentityMaxBytes  = 512
	durableInboundPrintableASCIIMin = '\x21'
	durableInboundPrintableASCIIMax = '\x7e'
)

type durableCursorStore interface {
	Load(daemon.DurableCursorKey) (daemon.DurableCursorState, error)
	CompareAndSwap(daemon.DurableCursorKey, *daemon.DurableCursorState, daemon.DurableCursorState) (daemon.DurableCursorState, error)
	CompareAndSwapSpan(daemon.DurableCursorKey, *daemon.DurableCursorState, daemon.DurableCursorState, uint16) (daemon.DurableCursorState, error)
}

type durableInboundCursorBinding struct {
	key         daemon.DurableCursorKey
	predecessor *daemon.DurableCursorState
	next        daemon.DurableCursorState
	span        uint16
}

type durableInboundRestartRepair struct {
	mu       sync.Mutex
	complete bool
}

func (repair *durableInboundRestartRepair) ensure(inbox *daemon.InboundInbox, store *daemon.DurableCursorStore) error {
	if repair == nil || inbox == nil || store == nil {
		return daemon.ErrDurableCursorInvalid
	}
	repair.mu.Lock()
	defer repair.mu.Unlock()
	if repair.complete {
		return nil
	}
	completed, err := inbox.CompletedDurable()
	if err != nil {
		return err
	}
	for _, item := range completed {
		delivery := proto.RemoteInboundDeliveryV2{
			DeliveryID:          item.Ack.DeliveryID,
			Cursor:              item.Cursor,
			Events:              make([]proto.RemoteEvent, len(item.Ack.Outcomes)),
			ProtocolVersion:     item.ProtocolVersion,
			StreamID:            item.StreamID,
			StreamEpoch:         item.StreamEpoch,
			PredecessorCursor:   item.PredecessorCursor,
			PredecessorPosition: item.PredecessorPos,
			Position:            item.Position,
			CursorDigest:        item.CursorDigest,
			BatchEventCount:     item.BatchEventCount,
			BatchDigest:         item.BatchDigest,
		}
		if !durableCachedAckSafe(delivery, item.Ack) || item.Ack.FinalizeEvidence.RemoteIdentity != item.RemoteIdentity {
			return errDurableInboundMetadata
		}
		if _, err := store.RepairFromCompletedDurable(item); err != nil {
			return err
		}
		key := daemon.DurableCursorKey{RemoteIdentity: item.RemoteIdentity, StreamID: item.StreamID, StreamEpoch: item.StreamEpoch}
		if _, pruneErr := inbox.PruneCompletedDurable(store, key); pruneErr != nil {
			return pruneErr
		}
	}
	repair.complete = true
	return nil
}

func pruneDurableInboundCompletion(inbox *daemon.InboundInbox, store *daemon.DurableCursorStore, binding *durableInboundCursorBinding) error {
	if inbox == nil || store == nil || binding == nil {
		return daemon.ErrDurableCursorInvalid
	}
	_, err := inbox.PruneCompletedDurable(store, binding.key)
	return err
}

// bindDurableInboundCursor separates the additive durable-log delivery shape
// from the frozen MQTT inbound-v2 shape. A delivery with no durable metadata is
// deliberately legacy even while durable-read is negotiated: retained MQTT
// remains authoritative during the migration overlap. Once any durable field
// is present, however, the whole binding must validate or the delivery pauses;
// a partial or stale-epoch delivery must never fall through to legacy cursor
// semantics.
func bindDurableInboundCursor(remoteIdentity string, negotiated proto.RemoteNegotiateSyncV1Result, delivery proto.RemoteInboundDeliveryV2) (*durableInboundCursorBinding, error) {
	metadataPresent := delivery.ProtocolVersion != 0 || delivery.StreamID != "" || delivery.StreamEpoch != "" ||
		delivery.PredecessorCursor != "" || delivery.PredecessorPosition != 0 ||
		delivery.Position != 0 || delivery.CursorDigest != "" || delivery.BatchEventCount != 0 || delivery.BatchDigest != ""
	if !metadataPresent {
		return nil, nil
	}
	if delivery.ProtocolVersion != 1 || len(delivery.Events) == 0 || len(delivery.Events) > proto.RemoteReplayBatchMaxEvents ||
		!validDurableInboundOpaque(remoteIdentity, durableInboundIdentityMaxBytes) ||
		!validDurableInboundOpaque(delivery.StreamID, durableInboundIdentityMaxBytes) ||
		!validDurableInboundOpaque(delivery.StreamEpoch, durableInboundIdentityMaxBytes) ||
		!validDurableInboundOpaque(delivery.Cursor, proto.MaxDurableCursorBytes) ||
		delivery.Position == 0 || !validDurableInboundDigest(delivery.CursorDigest) {
		return nil, errDurableInboundMetadata
	}
	wantDigest := sha256.Sum256([]byte(delivery.Cursor))
	if delivery.CursorDigest != hex.EncodeToString(wantDigest[:]) {
		return nil, errDurableInboundMetadata
	}
	if negotiated.SelectedProtocol != 1 || !negotiated.FeatureGateEnabled ||
		!negotiatedDurableScopedStream(negotiated, delivery.StreamID, delivery.StreamEpoch, delivery.Events[0].NamespaceID) ||
		!containsRemoteCapability(negotiated.ServerCapabilities, proto.CapabilityDurableDeltaSyncV1) ||
		!containsRemoteCapability(negotiated.ServerCapabilities, proto.CapabilityInboundAckV2) ||
		!containsRemoteCapability(negotiated.ServerCapabilities, proto.CapabilityInboundFinalizeV1) {
		return nil, errDurableInboundMetadata
	}
	if len(negotiated.Streams) != 0 && !containsRemoteCapability(negotiated.ServerCapabilities, proto.CapabilityDurableMultiStreamV1) {
		return nil, errDurableInboundMetadata
	}
	batch := len(delivery.Events) > 1
	if batch {
		if delivery.StagedCheckpoint != nil || delivery.BatchEventCount != uint16(len(delivery.Events)) || !validDurableInboundDigest(delivery.BatchDigest) ||
			!containsRemoteCapability(negotiated.ServerCapabilities, proto.CapabilityRedactionSafeBatchV1) {
			return nil, errDurableInboundMetadata
		}
		computed, err := proto.RemoteReplayBatchDigest(delivery)
		if err != nil || computed != delivery.BatchDigest {
			return nil, errDurableInboundMetadata
		}
		for _, event := range delivery.Events[1:] {
			if event.NamespaceID != delivery.Events[0].NamespaceID || event.AccessGeneration != delivery.Events[0].AccessGeneration ||
				event.AccessSetHash != delivery.Events[0].AccessSetHash || event.SecurityBarrierID != delivery.Events[0].SecurityBarrierID ||
				event.SecurityGeneration != delivery.Events[0].SecurityGeneration || event.KeyMode != delivery.Events[0].KeyMode || event.KeyVersion != delivery.Events[0].KeyVersion {
				return nil, errDurableInboundMetadata
			}
		}
	} else if delivery.BatchEventCount != 0 || delivery.BatchDigest != "" {
		return nil, errDurableInboundMetadata
	}
	switch negotiated.Mode {
	case proto.RemoteSyncModeDurableRead, proto.RemoteSyncModeDeltaPreferred, proto.RemoteSyncModeDeltaRequired:
	default:
		return nil, errDurableInboundMetadata
	}
	if delivery.PredecessorPosition+uint64(len(delivery.Events)) != delivery.Position ||
		!validDurableInboundOpaque(delivery.PredecessorCursor, proto.MaxDurableCursorBytes) ||
		delivery.PredecessorCursor == delivery.Cursor {
		return nil, errDurableInboundMetadata
	}
	predecessorDigest := sha256.Sum256([]byte(delivery.PredecessorCursor))
	predecessor := &daemon.DurableCursorState{
		Cursor:       delivery.PredecessorCursor,
		CursorDigest: hex.EncodeToString(predecessorDigest[:]),
		Position:     delivery.PredecessorPosition,
	}
	return &durableInboundCursorBinding{
		key: daemon.DurableCursorKey{
			RemoteIdentity: remoteIdentity,
			StreamID:       delivery.StreamID,
			StreamEpoch:    delivery.StreamEpoch,
		},
		predecessor: predecessor,
		next: daemon.DurableCursorState{
			Cursor:       delivery.Cursor,
			CursorDigest: delivery.CursorDigest,
			Position:     delivery.Position,
		},
		span: uint16(len(delivery.Events)),
	}, nil
}

func negotiatedDurableScopedStream(negotiated proto.RemoteNegotiateSyncV1Result, streamID, streamEpoch, namespaceID string) bool {
	if len(negotiated.Streams) == 0 {
		return negotiated.StreamID == streamID && negotiated.StreamEpoch == streamEpoch
	}
	for _, descriptor := range negotiated.Streams {
		if descriptor.StreamID == streamID && descriptor.StreamEpoch == streamEpoch && descriptor.NamespaceID == namespaceID {
			return true
		}
	}
	return false
}

func validDurableInboundDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func validDurableInboundOpaque(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, r := range value {
		if r < durableInboundPrintableASCIIMin || r > durableInboundPrintableASCIIMax {
			return false
		}
	}
	return true
}

func containsRemoteCapability(capabilities []string, wanted string) bool {
	for _, capability := range capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

// advanceDurableInboundCursor runs only after the exact terminal ACK is
// durable in InboundInbox. Until durable deliveries carry an authenticated
// predecessor cursor/position. A missing record can begin only at the genesis
// position. Every later advance requires the persisted cursor, digest, and
// position to match the authenticated predecessor exactly; the cursor store
// independently enforces the numeric +1 transition. An identical redelivery
// remains an idempotent repair after the inbox-commit/cursor-commit crash
// window.
func advanceDurableInboundCursor(store durableCursorStore, binding *durableInboundCursorBinding, _ bool) error {
	if store == nil || binding == nil {
		return daemon.ErrDurableCursorInvalid
	}
	current, err := store.Load(binding.key)
	if errors.Is(err, daemon.ErrDurableCursorNotFound) {
		if binding.span == 0 || binding.next.Position != uint64(binding.span) || binding.predecessor == nil || binding.predecessor.Position != 0 {
			return daemon.ErrDurableCursorConflict
		}
		_, err = store.CompareAndSwapSpan(binding.key, nil, binding.next, binding.span)
		return err
	}
	if err != nil {
		return err
	}
	if current.Cursor == binding.next.Cursor && current.CursorDigest == binding.next.CursorDigest && current.Position == binding.next.Position {
		_, err = store.CompareAndSwapSpan(binding.key, &current, binding.next, binding.span)
		return err
	}
	if binding.predecessor == nil || current.Cursor != binding.predecessor.Cursor ||
		current.CursorDigest != binding.predecessor.CursorDigest || current.Position != binding.predecessor.Position {
		return daemon.ErrDurableCursorConflict
	}
	_, err = store.CompareAndSwapSpan(binding.key, &current, binding.next, binding.span)
	return err
}

// durableCachedAckSafe rejects terminal ACKs produced by an older build that
// may have quarantined a malformed/security-failing durable delivery. Durable
// cursor advancement is allowed only for exact accepted outcomes; retryable,
// quarantined, malformed, or cursor-mismatched records remain stopped.
func durableCachedAckSafe(delivery proto.RemoteInboundDeliveryV2, ack proto.RemoteInboundAckV2) bool {
	if ack.DeliveryID != delivery.DeliveryID || ack.NextCursor != delivery.Cursor || ack.NextCursorDigest != delivery.CursorDigest ||
		ack.NextPosition != delivery.Position || len(ack.Outcomes) != len(delivery.Events) || len(ack.Outcomes) == 0 || ack.FinalizeEvidence == nil {
		return false
	}
	evidence := ack.FinalizeEvidence
	if evidence.ProtocolVersion != delivery.ProtocolVersion || evidence.DeliveryID != delivery.DeliveryID ||
		evidence.StreamID != delivery.StreamID || evidence.StreamEpoch != delivery.StreamEpoch || evidence.Cursor != delivery.Cursor ||
		evidence.CursorDigest != delivery.CursorDigest || evidence.Position != delivery.Position || evidence.BatchEventCount != delivery.BatchEventCount || evidence.BatchDigest != delivery.BatchDigest ||
		!validDurableInboundFinalizeEvidence(*evidence) {
		return false
	}
	for index, outcome := range ack.Outcomes {
		if outcome.Index != uint32(index) || outcome.Disposition != "accepted" {
			return false
		}
	}
	return true
}

// inboundV2AckFromResults preserves the exact legacy classifications while
// making missing ancestry and authenticated-input rejection non-terminal for
// the durable log. Legacy can still rely on its retained baseline; a durable
// cursor cannot advance until the dependency/security failure is resolved.
func inboundV2AckFromResults(delivery proto.RemoteInboundDeliveryV2, results []syncd.ImportOutcome, durable bool) (proto.RemoteInboundAckV2, bool) {
	ack := proto.RemoteInboundAckV2{DeliveryID: delivery.DeliveryID, Outcomes: make([]proto.RemoteInboundEventOutcomeV2, len(results))}
	terminal := len(results) == len(delivery.Events)
	for index, result := range results {
		outcome := proto.RemoteInboundEventOutcomeV2{Index: uint32(index)}
		switch result {
		case syncd.ImportRetryable:
			outcome.Disposition, outcome.ReasonCode = "retryable", "local-state-unavailable"
			terminal = false
		case syncd.ImportRejected:
			if durable {
				outcome.Disposition, outcome.ReasonCode = "retryable", "authenticated-input-rejected"
				terminal = false
			} else {
				outcome.Disposition, outcome.ReasonCode = "quarantined", "authenticated-input-rejected"
			}
		case syncd.ImportDeferredNeedsBaseline:
			if durable {
				outcome.Disposition, outcome.ReasonCode = "retryable", "missing-parent"
				terminal = false
			} else {
				outcome.Disposition, outcome.ReasonCode = "accepted", "durable"
			}
		case syncd.ImportApplied, syncd.ImportDeduped:
			outcome.Disposition, outcome.ReasonCode = "accepted", "durable"
		case syncd.ImportSkipped:
			outcome.Disposition = "accepted"
			if durable {
				outcome.ReasonCode = "authenticated-noop"
			} else {
				outcome.ReasonCode = "durable"
			}
		default:
			// Future importer outcomes are not implicitly terminal. A newer
			// daemon may add a disposition with different durability semantics;
			// advancing an older daemon's cloud cursor would lose that event.
			outcome.Disposition, outcome.ReasonCode = "retryable", "upgrade-required"
			terminal = false
		}
		ack.Outcomes[index] = outcome
	}
	return ack, terminal
}
