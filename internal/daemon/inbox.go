package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/aplexica/aplexica/internal/securityerr"
)

const (
	inboxVersion          = uint16(1)
	inboxFinalizedVersion = uint16(2)
	inboxMaxDeliveries    = 10000
	inboxMaxBytes         = int64(512 << 20)
	inboxMaxRecord        = int64(32<<20 + 1<<20)
	inboxMaxReasonCode    = 96
)

// ErrInboundFinalizeEvidenceNotFound means no completed durable delivery
// exists for the requested exact cursor. It remains distinct from malformed,
// duplicated, or mismatched evidence so checkpoint-bootstrap cursors can
// intentionally carry no ordinary delivery record without masking corruption.
var ErrInboundFinalizeEvidenceNotFound = errors.New("daemon: inbound finalize evidence not found")

type InboundAdmissionV2 struct {
	DeliveryID                      string   `json:"deliveryId"`
	InputSHA256                     [32]byte `json:"inputSha256"`
	ClaimedRosterEpoch              uint64   `json:"claimedRosterEpoch"`
	ClaimedRosterHash               [32]byte `json:"claimedRosterHash"`
	ClaimedAccessGeneration         uint64   `json:"claimedAccessGeneration"`
	ClaimedAccessSetHash            [32]byte `json:"claimedAccessSetHash"`
	ClaimedSecurityBarrierID        [32]byte `json:"claimedSecurityBarrierId"`
	AdmittedCurrentAccessGeneration uint64   `json:"admittedCurrentAccessGeneration"`
	AdmittedCurrentAccessSetHash    [32]byte `json:"admittedCurrentAccessSetHash"`
	AdmittedCurrentBarrierID        [32]byte `json:"admittedCurrentBarrierId"`
	// Durable cursor metadata is persisted only for authenticated durable-log
	// deliveries. omitempty preserves the checksum/JSON shape of existing
	// legacy inbox records. No artifact body or user-readable content is kept
	// after terminal completion.
	DurableRemoteIdentity    string `json:"durableRemoteIdentity,omitempty"`
	DurableProtocolVersion   uint16 `json:"durableProtocolVersion,omitempty"`
	DurableStreamID          string `json:"durableStreamId,omitempty"`
	DurableStreamEpoch       string `json:"durableStreamEpoch,omitempty"`
	DurablePredecessorCursor string `json:"durablePredecessorCursor,omitempty"`
	DurablePredecessorPos    uint64 `json:"durablePredecessorPosition,omitempty"`
	DurableCursor            string `json:"durableCursor,omitempty"`
	DurableCursorDigest      string `json:"durableCursorDigest,omitempty"`
	DurablePosition          uint64 `json:"durablePosition,omitempty"`
	DurableBodyDigest        string `json:"durableBodyDigest,omitempty"`
	DurableBatchEventCount   uint16 `json:"durableBatchEventCount,omitempty"`
	DurableBatchDigest       string `json:"durableBatchDigest,omitempty"`
}

// InboundDurableCompletion is the content-free cursor evidence retained after
// an inbound delivery reaches a terminal durable ACK. It is sufficient to
// repair the inbox-commit/cursor-commit crash window without retaining sealed
// event bytes or waiting for a cloud redelivery.
type InboundDurableCompletion struct {
	RemoteIdentity    string
	ProtocolVersion   uint16
	StreamID          string
	StreamEpoch       string
	PredecessorCursor string
	PredecessorPos    uint64
	Cursor            string
	CursorDigest      string
	Position          uint64
	BatchEventCount   uint16
	BatchDigest       string
	Ack               proto.RemoteInboundAckV2
	NativeFinalized   bool
}

type durableInboxCompletionRecord struct {
	Name       string
	Completion InboundDurableCompletion
}

type inboxRecord struct {
	Version   uint16                         `json:"version"`
	Admission InboundAdmissionV2             `json:"admission"`
	Delivery  *proto.RemoteInboundDeliveryV2 `json:"delivery,omitempty"`
	Ack       *proto.RemoteInboundAckV2      `json:"ack,omitempty"`
	Finalize  *inboundNativeFinalizeRecord   `json:"nativeFinalize,omitempty"`
	CreatedAt time.Time                      `json:"createdAt"`
	Checksum  [32]byte                       `json:"checksum"`
}

type inboundNativeFinalizeRecord struct {
	EvidenceSHA256 [32]byte  `json:"evidenceSha256"`
	CompletedAt    time.Time `json:"completedAt"`
}

type InboundInbox struct {
	Root string
	mu   sync.Mutex
}

func validOpaqueDeliveryValue(value string, maximum int) bool {
	if value == "" || maximum <= 0 || len(value) > maximum {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func inboxChecksum(record inboxRecord) ([32]byte, error) {
	record.Checksum = [32]byte{}
	b, err := json.Marshal(record)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append([]byte("aplexica/inbound-inbox/v1\x00"), b...)), nil
}

func inboundFinalizeEvidenceDigest(evidence proto.RemoteInboundFinalizeEvidenceV1) ([32]byte, error) {
	b, err := json.Marshal(evidence)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append([]byte("aplexica/inbound-finalize-evidence/v1\x00"), b...)), nil
}

func deliveryDigest(delivery proto.RemoteInboundDeliveryV2) ([32]byte, []byte, error) {
	b, err := json.Marshal(delivery)
	if err != nil || len(b) > int(inboxMaxRecord) {
		return [32]byte{}, nil, securityerr.ErrLimitExceeded
	}
	return sha256.Sum256(b), b, nil
}

func inboxName(deliveryID string) (string, error) {
	if !validOpaqueDeliveryValue(deliveryID, proto.MaxDeliveryIDBytes) {
		return "", securityerr.ErrUnsafeIdentifier
	}
	d := sha256.Sum256([]byte(deliveryID))
	return hex.EncodeToString(d[:]) + ".json", nil
}

func (i *InboundInbox) root() (*privatefs.Root, error) {
	if i == nil || !filepath.IsAbs(i.Root) {
		return nil, fmt.Errorf("inbox: absolute private root required")
	}
	if err := privatefs.EnsureDir(i.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return nil, err
	}
	return privatefs.OpenRoot(i.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
}

func readInboxRecord(root *privatefs.Root, name string) (inboxRecord, error) {
	f, err := root.OpenReadRegular(name)
	if err != nil {
		return inboxRecord{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, inboxMaxRecord+1))
	closeErr := f.Close()
	if err != nil || closeErr != nil || int64(len(b)) > inboxMaxRecord {
		return inboxRecord{}, securityerr.ErrLimitExceeded
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var record inboxRecord
	if err := dec.Decode(&record); err != nil {
		return inboxRecord{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return inboxRecord{}, securityerr.ErrMetadataMismatch
	}
	want, _ := inboxChecksum(record)
	if (record.Version != inboxVersion && record.Version != inboxFinalizedVersion) || want != record.Checksum || record.Admission.DeliveryID == "" || record.Admission.InputSHA256 == ([32]byte{}) || (record.Delivery == nil) == (record.Ack == nil) {
		return inboxRecord{}, securityerr.ErrMetadataMismatch
	}
	if record.Finalize != nil {
		if record.Version != inboxFinalizedVersion || record.Ack == nil || record.Ack.FinalizeEvidence == nil || record.Finalize.CompletedAt.IsZero() {
			return inboxRecord{}, securityerr.ErrMetadataMismatch
		}
		digest, digestErr := inboundFinalizeEvidenceDigest(*record.Ack.FinalizeEvidence)
		if digestErr != nil || digest != record.Finalize.EvidenceSHA256 {
			return inboxRecord{}, securityerr.ErrMetadataMismatch
		}
	} else if record.Version == inboxFinalizedVersion {
		return inboxRecord{}, securityerr.ErrMetadataMismatch
	}
	return record, nil
}

func writeInboxRecord(root *privatefs.Root, name string, record inboxRecord) error {
	record.Checksum, _ = inboxChecksum(record)
	b, err := json.Marshal(record)
	if err != nil || int64(len(b)) > inboxMaxRecord {
		return securityerr.ErrLimitExceeded
	}
	return root.WriteFile(name, b, privatefs.FilePolicy{RejectWritableByOthers: true})
}

func claimedAdmission(delivery proto.RemoteInboundDeliveryV2, digest [32]byte, current securityepoch.SecurityEpoch) (InboundAdmissionV2, string, error) {
	if len(delivery.Events) == 0 || len(delivery.Events) > proto.MaxInboundEvents || !validOpaqueDeliveryValue(delivery.Cursor, proto.MaxDurableCursorBytes) {
		return InboundAdmissionV2{}, "", securityerr.ErrMetadataMismatch
	}
	first := delivery.Events[0]
	scope := first.NamespaceID
	if scope == "" {
		scope = "account"
	}
	for _, event := range delivery.Events {
		eventScope := event.NamespaceID
		if eventScope == "" {
			eventScope = "account"
		}
		if eventScope != scope || event.AccessGeneration != first.AccessGeneration || event.AccessSetHash != first.AccessSetHash || event.SecurityBarrierID != first.SecurityBarrierID || event.SecurityGeneration != first.SecurityGeneration || event.KeyMode != first.KeyMode || event.KeyVersion != first.KeyVersion {
			return InboundAdmissionV2{}, "", securityerr.ErrMetadataMismatch
		}
	}
	return InboundAdmissionV2{DeliveryID: delivery.DeliveryID, InputSHA256: digest, ClaimedAccessGeneration: first.AccessGeneration, ClaimedAccessSetHash: first.AccessSetHash, ClaimedSecurityBarrierID: first.SecurityBarrierID, AdmittedCurrentAccessGeneration: current.AccessGeneration, AdmittedCurrentAccessSetHash: current.AccessSetHash, AdmittedCurrentBarrierID: current.BarrierID}, scope, nil
}

// Admit persists the complete bounded delivery while the caller holds the
// security-epoch admission lease. A terminal duplicate returns its exact prior
// acknowledgement without re-importing canonical state.
func (i *InboundInbox) Admit(delivery proto.RemoteInboundDeliveryV2, current securityepoch.SecurityEpoch) (InboundAdmissionV2, *proto.RemoteInboundAckV2, error) {
	return i.admit(delivery, current, "")
}

// AdmitDurable persists a durable-log delivery together with the exact
// content-free stream/cursor binding needed for ordered restart repair.
// Callers must already have verified negotiation and authenticated adjacency;
// this method independently rejects incomplete transport metadata.
func (i *InboundInbox) AdmitDurable(delivery proto.RemoteInboundDeliveryV2, current securityepoch.SecurityEpoch, remoteIdentity string) (InboundAdmissionV2, *proto.RemoteInboundAckV2, error) {
	if !validOpaqueDeliveryValue(remoteIdentity, 512) || delivery.ProtocolVersion != 1 || len(delivery.Events) == 0 || len(delivery.Events) > proto.RemoteReplayBatchMaxEvents ||
		!validOpaqueDeliveryValue(delivery.StreamID, 512) || !validOpaqueDeliveryValue(delivery.StreamEpoch, 512) ||
		!validOpaqueDeliveryValue(delivery.PredecessorCursor, proto.MaxDurableCursorBytes) ||
		!validOpaqueDeliveryValue(delivery.Cursor, proto.MaxDurableCursorBytes) || delivery.Position == 0 ||
		delivery.PredecessorPosition+uint64(len(delivery.Events)) != delivery.Position || delivery.PredecessorCursor == delivery.Cursor ||
		!validLowerHexSHA256(delivery.CursorDigest) || !validInboxStagedCheckpoint(delivery) {
		return InboundAdmissionV2{}, nil, securityerr.ErrMetadataMismatch
	}
	if len(delivery.Events) == 1 {
		if delivery.BatchEventCount != 0 || delivery.BatchDigest != "" {
			return InboundAdmissionV2{}, nil, securityerr.ErrMetadataMismatch
		}
	} else {
		computed, err := proto.RemoteReplayBatchDigest(delivery)
		if err != nil || delivery.BatchEventCount != uint16(len(delivery.Events)) || computed != delivery.BatchDigest {
			return InboundAdmissionV2{}, nil, securityerr.ErrMetadataMismatch
		}
	}
	digest := sha256.Sum256([]byte(delivery.Cursor))
	if delivery.CursorDigest != hex.EncodeToString(digest[:]) {
		return InboundAdmissionV2{}, nil, securityerr.ErrMetadataMismatch
	}
	return i.admit(delivery, current, remoteIdentity)
}

func validInboxStagedCheckpoint(delivery proto.RemoteInboundDeliveryV2) bool {
	if delivery.StagedCheckpoint == nil {
		return true
	}
	if len(delivery.Events) != 1 {
		return false
	}
	event, staged := delivery.Events[0], delivery.StagedCheckpoint
	return len(event.Bytes) == 0 && event.Lane == "retained" && !event.Clear && staged.ProtocolVersion == proto.RemoteStagedTransferProtocolV1 &&
		event.CheckpointCoverage > 0 && validLowerHexSHA256(event.CheckpointGeneration) && validLowerHexSHA256(event.CheckpointAlignmentHash) &&
		validLowerHexSHA256(staged.TransferID) && staged.SealedBytes > proto.MaxSealedEventBytes && staged.SealedBytes <= proto.MaxRemoteStagedCheckpointBytes &&
		validLowerHexSHA256(staged.BodyDigest) && staged.BodyDigest == event.BodyDigest && staged.StreamID == delivery.StreamID && staged.StreamEpoch == delivery.StreamEpoch &&
		validLowerHexSHA256(staged.BindingDigest) && staged.BindingDigest == proto.RemoteStagedBindingDigest(event, *staged)
}

func validLowerHexSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func (i *InboundInbox) admit(delivery proto.RemoteInboundDeliveryV2, current securityepoch.SecurityEpoch, remoteIdentity string) (InboundAdmissionV2, *proto.RemoteInboundAckV2, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	name, err := inboxName(delivery.DeliveryID)
	if err != nil {
		return InboundAdmissionV2{}, nil, err
	}
	digest, encoded, err := deliveryDigest(delivery)
	if err != nil {
		return InboundAdmissionV2{}, nil, err
	}
	admission, _, err := claimedAdmission(delivery, digest, current)
	if err != nil {
		return InboundAdmissionV2{}, nil, err
	}
	if remoteIdentity != "" {
		admission.DurableRemoteIdentity = remoteIdentity
		admission.DurableProtocolVersion = delivery.ProtocolVersion
		admission.DurableStreamID = delivery.StreamID
		admission.DurableStreamEpoch = delivery.StreamEpoch
		admission.DurablePredecessorCursor = delivery.PredecessorCursor
		admission.DurablePredecessorPos = delivery.PredecessorPosition
		admission.DurableCursor = delivery.Cursor
		admission.DurableCursorDigest = delivery.CursorDigest
		admission.DurablePosition = delivery.Position
		admission.DurableBatchEventCount = delivery.BatchEventCount
		admission.DurableBatchDigest = delivery.BatchDigest
		if delivery.StagedCheckpoint != nil {
			admission.DurableBodyDigest = delivery.StagedCheckpoint.BodyDigest
		} else if len(delivery.Events) == 1 {
			bodyDigest := sha256.Sum256(delivery.Events[0].Bytes)
			admission.DurableBodyDigest = hex.EncodeToString(bodyDigest[:])
		}
	}
	root, err := i.root()
	if err != nil {
		return InboundAdmissionV2{}, nil, err
	}
	defer root.Close()
	if existing, err := readInboxRecord(root, name); err == nil {
		if existing.Admission.InputSHA256 != digest || existing.Admission.DeliveryID != delivery.DeliveryID ||
			existing.Admission.DurableRemoteIdentity != remoteIdentity {
			return InboundAdmissionV2{}, nil, securityerr.ErrMetadataMismatch
		}
		return existing.Admission, existing.Ack, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return InboundAdmissionV2{}, nil, err
	}
	entries, err := root.ReadDir(".")
	if err != nil {
		return InboundAdmissionV2{}, nil, err
	}
	count, total := 0, int64(len(encoded))
	for _, entry := range entries {
		if entry.Type().IsRegular() && filepath.Ext(entry.Name()) == ".json" {
			count++
			if info, e := os.Lstat(filepath.Join(i.Root, entry.Name())); e == nil {
				total += info.Size()
			}
		}
	}
	if count >= inboxMaxDeliveries || total > inboxMaxBytes {
		return InboundAdmissionV2{}, nil, securityerr.ErrLimitExceeded
	}
	copyDelivery := delivery
	record := inboxRecord{Version: inboxVersion, Admission: admission, Delivery: &copyDelivery, CreatedAt: time.Now().UTC()}
	if err := writeInboxRecord(root, name, record); err != nil {
		return InboundAdmissionV2{}, nil, err
	}
	return admission, nil, nil
}

func durableCompletionFromInboxRecord(record inboxRecord) (InboundDurableCompletion, bool, error) {
	admission := record.Admission
	if admission.DurableRemoteIdentity == "" {
		return InboundDurableCompletion{}, false, nil
	}
	if admission.DurableProtocolVersion != 1 ||
		!validOpaqueDeliveryValue(admission.DurableRemoteIdentity, 512) ||
		!validOpaqueDeliveryValue(admission.DurableStreamID, 512) ||
		!validOpaqueDeliveryValue(admission.DurableStreamEpoch, 512) ||
		!validOpaqueDeliveryValue(admission.DurablePredecessorCursor, proto.MaxDurableCursorBytes) ||
		!validOpaqueDeliveryValue(admission.DurableCursor, proto.MaxDurableCursorBytes) ||
		admission.DurablePosition == 0 ||
		admission.DurablePredecessorCursor == admission.DurableCursor || !validLowerHexSHA256(admission.DurableCursorDigest) {
		return InboundDurableCompletion{}, true, securityerr.ErrMetadataMismatch
	}
	if admission.DurableBatchEventCount == 0 {
		if admission.DurablePredecessorPos != admission.DurablePosition-1 || admission.DurableBatchDigest != "" || !validLowerHexSHA256(admission.DurableBodyDigest) {
			return InboundDurableCompletion{}, true, securityerr.ErrMetadataMismatch
		}
	} else if admission.DurableBatchEventCount < 2 || admission.DurableBatchEventCount > proto.RemoteReplayBatchMaxEvents ||
		admission.DurablePredecessorPos+uint64(admission.DurableBatchEventCount) != admission.DurablePosition ||
		!validLowerHexSHA256(admission.DurableBatchDigest) || admission.DurableBodyDigest != "" {
		return InboundDurableCompletion{}, true, securityerr.ErrMetadataMismatch
	}
	digest := sha256.Sum256([]byte(admission.DurableCursor))
	if admission.DurableCursorDigest != hex.EncodeToString(digest[:]) {
		return InboundDurableCompletion{}, true, securityerr.ErrMetadataMismatch
	}
	if record.Ack == nil {
		// A well-formed crash record before terminal completion intentionally
		// retains the encrypted delivery for redelivery. It is not cursor repair
		// evidence, but malformed durable metadata above still fails closed.
		return InboundDurableCompletion{}, false, nil
	}
	if record.Ack.DeliveryID != admission.DeliveryID || record.Ack.NextCursor != admission.DurableCursor ||
		record.Ack.NextCursorDigest != admission.DurableCursorDigest || record.Ack.NextPosition != admission.DurablePosition {
		return InboundDurableCompletion{}, true, securityerr.ErrMetadataMismatch
	}
	if evidence := record.Ack.FinalizeEvidence; evidence != nil {
		if evidence.ProtocolVersion != admission.DurableProtocolVersion || evidence.RemoteIdentity != admission.DurableRemoteIdentity ||
			evidence.DeliveryID != admission.DeliveryID || evidence.StreamID != admission.DurableStreamID || evidence.StreamEpoch != admission.DurableStreamEpoch ||
			evidence.Cursor != admission.DurableCursor || evidence.CursorDigest != admission.DurableCursorDigest || evidence.Position != admission.DurablePosition ||
			!validInboxFinalizeEvidenceShape(*evidence) {
			return InboundDurableCompletion{}, true, securityerr.ErrMetadataMismatch
		}
		if admission.DurableBatchEventCount == 0 {
			if evidence.BodyDigest != admission.DurableBodyDigest || evidence.BatchEventCount != 0 || evidence.BatchDigest != "" {
				return InboundDurableCompletion{}, true, securityerr.ErrMetadataMismatch
			}
		} else if evidence.BodyDigest != "" || evidence.BatchEventCount != admission.DurableBatchEventCount || evidence.BatchDigest != admission.DurableBatchDigest {
			return InboundDurableCompletion{}, true, securityerr.ErrMetadataMismatch
		}
	}
	return InboundDurableCompletion{
		RemoteIdentity:    admission.DurableRemoteIdentity,
		ProtocolVersion:   admission.DurableProtocolVersion,
		StreamID:          admission.DurableStreamID,
		StreamEpoch:       admission.DurableStreamEpoch,
		PredecessorCursor: admission.DurablePredecessorCursor,
		PredecessorPos:    admission.DurablePredecessorPos,
		Cursor:            admission.DurableCursor,
		CursorDigest:      admission.DurableCursorDigest,
		Position:          admission.DurablePosition,
		BatchEventCount:   admission.DurableBatchEventCount,
		BatchDigest:       admission.DurableBatchDigest,
		Ack:               *record.Ack,
		NativeFinalized:   record.Finalize != nil,
	}, true, nil
}

// CompletedDurable returns ordered, content-free durable cursor completions.
// Any malformed durable metadata fails the whole scan closed; callers must not
// accept newer deliveries until the local inbox/cursor relationship is known.
func (i *InboundInbox) CompletedDurable() ([]InboundDurableCompletion, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	root, err := i.root()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := root.ReadDir(".")
	if err != nil {
		return nil, err
	}
	completed := make([]InboundDurableCompletion, 0)
	for _, entry := range entries {
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		record, readErr := readInboxRecord(root, entry.Name())
		if readErr != nil {
			return nil, readErr
		}
		completion, durable, completionErr := durableCompletionFromInboxRecord(record)
		if completionErr != nil {
			return nil, completionErr
		}
		if !durable {
			continue
		}
		completed = append(completed, completion)
	}
	sort.Slice(completed, func(left, right int) bool {
		a, b := completed[left], completed[right]
		if a.RemoteIdentity != b.RemoteIdentity {
			return a.RemoteIdentity < b.RemoteIdentity
		}
		if a.StreamID != b.StreamID {
			return a.StreamID < b.StreamID
		}
		if a.StreamEpoch != b.StreamEpoch {
			return a.StreamEpoch < b.StreamEpoch
		}
		return a.Position < b.Position
	})
	return completed, nil
}

// DurableCompletionFinalized reports whether one exact terminal cursor has a
// committed native-finalize marker. It is used as a hard predecessor barrier:
// a later delivery cannot advance while the current delivery is still waiting
// for its post-cloud-ACK materialisation.
func (i *InboundInbox) DurableCompletionFinalized(key DurableCursorKey, state DurableCursorState) (bool, error) {
	if validateDurableCursorKey(key) != nil || validateDurableCursorValue(state, true) != nil || state.Position == 0 {
		return false, securityerr.ErrMetadataMismatch
	}
	completed, err := i.CompletedDurable()
	if err != nil {
		return false, err
	}
	found := false
	finalized := false
	for _, completion := range completed {
		if completion.RemoteIdentity != key.RemoteIdentity || completion.StreamID != key.StreamID || completion.StreamEpoch != key.StreamEpoch || completion.Position != state.Position {
			continue
		}
		if found || completion.Cursor != state.Cursor || completion.CursorDigest != state.CursorDigest {
			return false, securityerr.ErrMetadataMismatch
		}
		found = true
		finalized = completion.NativeFinalized
	}
	if !found {
		return false, ErrInboundFinalizeEvidenceNotFound
	}
	return finalized, nil
}

// RetainedFinalizeEvidenceAtCursor returns the exact content-free terminal
// evidence retained for one authoritative cursor and whether native finalize
// is already committed. Keeping finalized evidence at the current cursor lets
// the daemon validate (but never trust) a replacement plugin's proposal to
// retry a cloud ACK whose response was lost after local finalization.
func (i *InboundInbox) RetainedFinalizeEvidenceAtCursor(key DurableCursorKey, state DurableCursorState) (*proto.RemoteInboundFinalizeEvidenceV1, bool, error) {
	if validateDurableCursorKey(key) != nil || validateDurableCursorValue(state, true) != nil || state.Position == 0 {
		return nil, false, securityerr.ErrMetadataMismatch
	}
	completed, err := i.CompletedDurable()
	if err != nil {
		return nil, false, err
	}
	found := false
	finalized := false
	var retained *proto.RemoteInboundFinalizeEvidenceV1
	for _, completion := range completed {
		if completion.RemoteIdentity != key.RemoteIdentity || completion.StreamID != key.StreamID || completion.StreamEpoch != key.StreamEpoch || completion.Position != state.Position {
			continue
		}
		if found || completion.Cursor != state.Cursor || completion.CursorDigest != state.CursorDigest || completion.Ack.FinalizeEvidence == nil {
			return nil, false, securityerr.ErrMetadataMismatch
		}
		found = true
		finalized = completion.NativeFinalized
		evidence := *completion.Ack.FinalizeEvidence
		retained = &evidence
	}
	if !found {
		return nil, false, ErrInboundFinalizeEvidenceNotFound
	}
	return retained, finalized, nil
}

// PendingFinalizeEvidence returns the exact daemon-owned finalize obligation
// for the authoritative cursor. A plugin replacement receives this evidence in
// the cursor handoff, re-acknowledges the already-committed cloud cursor, and
// echoes it back before fetching a successor. The newest cursor record is
// deliberately retained by pruning, so absence is a migration/corruption error
// rather than permission to forget an unfinalized native write.
func (i *InboundInbox) PendingFinalizeEvidence(key DurableCursorKey, state DurableCursorState) (*proto.RemoteInboundFinalizeEvidenceV1, error) {
	retained, finalized, err := i.RetainedFinalizeEvidenceAtCursor(key, state)
	if err != nil {
		return nil, err
	}
	if finalized {
		return nil, nil
	}
	return retained, nil
}

// PruneCompletedDurable removes only older, already-native-finalized terminal
// records when the daemon cursor store and the newest terminal record prove the
// exact current cursor. The newest exact record is always retained for a lost
// finalize response/retry, and unfinalized records are never evicted. If a
// broken plugin accumulates unfinalized records, admission reaches the hard
// inbox count/byte cap and fails closed with backpressure.
func (i *InboundInbox) PruneCompletedDurable(store *DurableCursorStore, key DurableCursorKey) (int, error) {
	if i == nil || store == nil || validateDurableCursorKey(key) != nil {
		return 0, securityerr.ErrMetadataMismatch
	}
	evidence, err := store.Load(key)
	if err != nil || validateDurableCursorValue(evidence, true) != nil || evidence.Position == 0 {
		if err != nil {
			return 0, err
		}
		return 0, securityerr.ErrMetadataMismatch
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	root, err := i.root()
	if err != nil {
		return 0, err
	}
	defer root.Close()
	entries, err := root.ReadDir(".")
	if err != nil {
		return 0, err
	}
	candidates := make([]durableInboxCompletionRecord, 0)
	exactFound := false
	seenPositions := make(map[uint64]struct{})
	for _, entry := range entries {
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		record, readErr := readInboxRecord(root, entry.Name())
		if readErr != nil {
			return 0, readErr
		}
		completion, durable, completionErr := durableCompletionFromInboxRecord(record)
		if completionErr != nil {
			return 0, completionErr
		}
		if !durable || completion.RemoteIdentity != key.RemoteIdentity || completion.StreamID != key.StreamID || completion.StreamEpoch != key.StreamEpoch || completion.Position > evidence.Position {
			continue
		}
		if _, duplicate := seenPositions[completion.Position]; duplicate {
			return 0, securityerr.ErrMetadataMismatch
		}
		seenPositions[completion.Position] = struct{}{}
		if completion.Position == evidence.Position {
			if completion.Cursor != evidence.Cursor || completion.CursorDigest != evidence.CursorDigest {
				return 0, securityerr.ErrMetadataMismatch
			}
			exactFound = true
		}
		if completion.NativeFinalized && completion.Position < evidence.Position {
			candidates = append(candidates, durableInboxCompletionRecord{Name: entry.Name(), Completion: completion})
		}
	}
	if !exactFound {
		return 0, securityerr.ErrMetadataMismatch
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	sort.Slice(candidates, func(left, right int) bool {
		return candidates[left].Completion.Position < candidates[right].Completion.Position
	})
	removed := 0
	for _, candidate := range candidates {
		if err := root.RemoveRegular(candidate.Name); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// PrepareInboundFinalize authenticates an exact finalize request against the
// retained terminal inbox ACK. It performs no native writes and never reads or
// advances the durable cursor store. A finalized exact retry is reported
// separately so callers can return the cached success without fan-out.
func (i *InboundInbox) PrepareInboundFinalize(evidence proto.RemoteInboundFinalizeEvidenceV1) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	record, err := i.readFinalizeRecordLocked(evidence)
	if err != nil {
		return false, err
	}
	return record.Finalize != nil, nil
}

// MarkInboundFinalized durably closes the native half after fan-out. An exact
// retry is idempotent. A crash after fan-out but before this write safely
// repeats the idempotent materializer; a crash after this write returns the
// cached success without touching native files or cursor state.
func (i *InboundInbox) MarkInboundFinalized(evidence proto.RemoteInboundFinalizeEvidenceV1) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	name, err := inboxName(evidence.DeliveryID)
	if err != nil {
		return false, err
	}
	root, err := i.root()
	if err != nil {
		return false, err
	}
	defer root.Close()
	record, err := readInboxRecord(root, name)
	if err != nil {
		return false, err
	}
	if err := validateExactFinalizeRecord(record, evidence); err != nil {
		return false, err
	}
	if record.Finalize != nil {
		return true, nil
	}
	digest, err := inboundFinalizeEvidenceDigest(evidence)
	if err != nil {
		return false, err
	}
	record.Version = inboxFinalizedVersion
	record.Finalize = &inboundNativeFinalizeRecord{EvidenceSHA256: digest, CompletedAt: time.Now().UTC()}
	if err := writeInboxRecord(root, name, record); err != nil {
		return false, err
	}
	return false, nil
}

func (i *InboundInbox) readFinalizeRecordLocked(evidence proto.RemoteInboundFinalizeEvidenceV1) (inboxRecord, error) {
	name, err := inboxName(evidence.DeliveryID)
	if err != nil {
		return inboxRecord{}, err
	}
	root, err := i.root()
	if err != nil {
		return inboxRecord{}, err
	}
	defer root.Close()
	record, err := readInboxRecord(root, name)
	if err != nil {
		return inboxRecord{}, err
	}
	if err := validateExactFinalizeRecord(record, evidence); err != nil {
		return inboxRecord{}, err
	}
	return record, nil
}

func validateExactFinalizeRecord(record inboxRecord, evidence proto.RemoteInboundFinalizeEvidenceV1) error {
	if record.Delivery != nil || record.Ack == nil || record.Ack.FinalizeEvidence == nil || *record.Ack.FinalizeEvidence != evidence {
		return securityerr.ErrMetadataMismatch
	}
	completion, durable, err := durableCompletionFromInboxRecord(record)
	if err != nil || !durable || completion.RemoteIdentity != evidence.RemoteIdentity || completion.StreamID != evidence.StreamID ||
		completion.StreamEpoch != evidence.StreamEpoch || completion.Cursor != evidence.Cursor || completion.CursorDigest != evidence.CursorDigest ||
		completion.Position != evidence.Position || completion.Ack.DeliveryID != evidence.DeliveryID {
		return securityerr.ErrMetadataMismatch
	}
	return nil
}

func (i *InboundInbox) Complete(deliveryID string, input [32]byte, ack proto.RemoteInboundAckV2) error {
	return i.complete(deliveryID, input, ack, false)
}

// CompleteDurable persists a terminal ACK only when it carries exact
// content-free native-finalize evidence derived from the still-present
// delivery. This is the canonical-commit boundary: the sealed delivery is
// discarded only after its immutable wire tuple and canonical result have
// been bound into the cached ACK.
func (i *InboundInbox) CompleteDurable(deliveryID string, input [32]byte, ack proto.RemoteInboundAckV2) error {
	return i.complete(deliveryID, input, ack, true)
}

func (i *InboundInbox) complete(deliveryID string, input [32]byte, ack proto.RemoteInboundAckV2, requireFinalize bool) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	name, err := inboxName(deliveryID)
	if err != nil {
		return err
	}
	root, err := i.root()
	if err != nil {
		return err
	}
	defer root.Close()
	record, err := readInboxRecord(root, name)
	if err != nil {
		return err
	}
	if record.Admission.InputSHA256 != input || ack.DeliveryID != deliveryID || len(ack.Outcomes) == 0 {
		return securityerr.ErrMetadataMismatch
	}
	for index, outcome := range ack.Outcomes {
		if outcome.Index != uint32(index) || outcome.Disposition == "retryable" {
			return securityerr.ErrMetadataMismatch
		}
	}
	if requireFinalize {
		if record.Delivery == nil || validateInboundFinalizeEvidence(record.Admission, *record.Delivery, ack) != nil {
			return securityerr.ErrMetadataMismatch
		}
	} else if ack.FinalizeEvidence != nil {
		return securityerr.ErrMetadataMismatch
	}
	record.Delivery, record.Ack = nil, &ack
	return writeInboxRecord(root, name, record)
}

func validateInboundFinalizeEvidence(admission InboundAdmissionV2, delivery proto.RemoteInboundDeliveryV2, ack proto.RemoteInboundAckV2) error {
	if len(delivery.Events) > 1 {
		return validateInboundBatchFinalizeEvidence(admission, delivery, ack)
	}
	if admission.DurableRemoteIdentity == "" || ack.FinalizeEvidence == nil || len(delivery.Events) != 1 || delivery.BatchEventCount != 0 || delivery.BatchDigest != "" || len(ack.Outcomes) != 1 ||
		ack.Outcomes[0].Index != 0 || ack.Outcomes[0].Disposition != "accepted" || ack.NextCursor != admission.DurableCursor ||
		ack.NextCursorDigest != admission.DurableCursorDigest || ack.NextPosition != admission.DurablePosition {
		return securityerr.ErrMetadataMismatch
	}
	event := delivery.Events[0]
	evidence := *ack.FinalizeEvidence
	if evidence.ProtocolVersion != admission.DurableProtocolVersion || evidence.RemoteIdentity != admission.DurableRemoteIdentity ||
		evidence.DeliveryID != admission.DeliveryID || evidence.StreamID != admission.DurableStreamID ||
		evidence.StreamEpoch != admission.DurableStreamEpoch || evidence.Cursor != admission.DurableCursor ||
		evidence.CursorDigest != admission.DurableCursorDigest || evidence.Position != admission.DurablePosition ||
		evidence.NamespaceID != event.NamespaceID || evidence.BranchID != event.BranchID || evidence.Kind != event.Kind ||
		evidence.ArtifactID != event.ArtifactID || evidence.WireEventID != event.EventID ||
		evidence.WireEventHash != event.EventHash || evidence.BodyDigest != event.BodyDigest || evidence.BodyDigest != admission.DurableBodyDigest ||
		evidence.ParentHash != event.ParentHash || evidence.CheckpointAlignmentHash != event.CheckpointAlignmentHash ||
		evidence.EventType != event.Type || evidence.TimestampUnixNano != event.Timestamp.UnixNano() ||
		evidence.Sequence != event.Sequence || evidence.Origin != event.Origin || evidence.SourceAgent != event.SourceAgent ||
		evidence.Lane != event.Lane || evidence.Clear != event.Clear || !validInboxFinalizeEvidenceShape(evidence) {
		return securityerr.ErrMetadataMismatch
	}
	return nil
}

func validateInboundBatchFinalizeEvidence(admission InboundAdmissionV2, delivery proto.RemoteInboundDeliveryV2, ack proto.RemoteInboundAckV2) error {
	if admission.DurableRemoteIdentity == "" || ack.FinalizeEvidence == nil || len(delivery.Events) < 2 || len(delivery.Events) > proto.RemoteReplayBatchMaxEvents ||
		delivery.StagedCheckpoint != nil || delivery.BatchEventCount != uint16(len(delivery.Events)) || admission.DurableBatchEventCount != delivery.BatchEventCount ||
		admission.DurableBatchDigest != delivery.BatchDigest || len(ack.Outcomes) != len(delivery.Events) || ack.NextCursor != admission.DurableCursor ||
		ack.NextCursorDigest != admission.DurableCursorDigest || ack.NextPosition != admission.DurablePosition {
		return securityerr.ErrMetadataMismatch
	}
	computed, err := proto.RemoteReplayBatchDigest(delivery)
	if err != nil || computed != delivery.BatchDigest {
		return securityerr.ErrMetadataMismatch
	}
	for index, event := range delivery.Events {
		if ack.Outcomes[index].Index != uint32(index) || ack.Outcomes[index].Disposition != "accepted" || ack.Outcomes[index].ReasonCode == "" || len(ack.Outcomes[index].ReasonCode) > inboxMaxReasonCode {
			return securityerr.ErrMetadataMismatch
		}
		bodyDigest := sha256.Sum256(event.Bytes)
		if event.BodyDigest != hex.EncodeToString(bodyDigest[:]) {
			return securityerr.ErrMetadataMismatch
		}
	}
	evidence := *ack.FinalizeEvidence
	if evidence.ProtocolVersion != admission.DurableProtocolVersion || evidence.RemoteIdentity != admission.DurableRemoteIdentity ||
		evidence.DeliveryID != admission.DeliveryID || evidence.StreamID != admission.DurableStreamID || evidence.StreamEpoch != admission.DurableStreamEpoch ||
		evidence.Cursor != admission.DurableCursor || evidence.CursorDigest != admission.DurableCursorDigest || evidence.Position != admission.DurablePosition ||
		evidence.NamespaceID != delivery.Events[0].NamespaceID || evidence.BatchEventCount != delivery.BatchEventCount || evidence.BatchDigest != delivery.BatchDigest ||
		!validLowerHexSHA256(evidence.BatchResultDigest) || evidence.BranchID != "" || evidence.Kind != "" || evidence.ArtifactID != "" ||
		evidence.WireEventID != "" || evidence.WireEventHash != "" || evidence.BodyDigest != "" || evidence.ParentHash != "" ||
		evidence.CheckpointAlignmentHash != "" || evidence.EventType != "" || evidence.TimestampUnixNano != 0 || evidence.Sequence != 0 ||
		evidence.Origin != "" || evidence.SourceAgent != "" || evidence.Lane != "" || evidence.Clear || evidence.CanonicalEventID != "" ||
		evidence.CanonicalHash != "" || evidence.NoopReason != "" || evidence.AuthenticatedHeaderDigest != "" || evidence.AuthenticatedSignerIdentity != "" {
		return securityerr.ErrMetadataMismatch
	}
	entries, err := proto.DecodeRemoteBatchMaterializationPlan(evidence.BatchMaterializationPlan, evidence.BatchMaterializationDigest)
	if err != nil {
		return securityerr.ErrMetadataMismatch
	}
	available := make(map[string]struct{}, len(delivery.Events))
	for _, event := range delivery.Events {
		available[event.Kind+"\x00"+event.ArtifactID] = struct{}{}
	}
	previous := ""
	for _, entry := range entries {
		key := entry.Kind + "\x00" + entry.ArtifactID
		if entry.Kind == "" || entry.ArtifactID == "" || entry.CanonicalEventID == "" || !validLowerHexSHA256(entry.CanonicalHash) ||
			(previous != "" && previous >= key) {
			return securityerr.ErrMetadataMismatch
		}
		if _, ok := available[key]; !ok {
			return securityerr.ErrMetadataMismatch
		}
		previous = key
	}
	if evidence.FinalizeKind == proto.InboundFinalizeCanonicalBatch && len(entries) == 0 ||
		evidence.FinalizeKind == proto.InboundFinalizeAuthenticatedBatchNoop && len(entries) != 0 {
		return securityerr.ErrMetadataMismatch
	}
	if evidence.FinalizeKind != proto.InboundFinalizeCanonicalBatch && evidence.FinalizeKind != proto.InboundFinalizeAuthenticatedBatchNoop {
		return securityerr.ErrMetadataMismatch
	}
	return nil
}

func validInboxFinalizeEvidenceShape(evidence proto.RemoteInboundFinalizeEvidenceV1) bool {
	if evidence.ProtocolVersion != 1 || evidence.Position == 0 ||
		!validOpaqueDeliveryValue(evidence.RemoteIdentity, 512) || !validOpaqueDeliveryValue(evidence.DeliveryID, proto.MaxDeliveryIDBytes) ||
		!validOpaqueDeliveryValue(evidence.StreamID, 512) || !validOpaqueDeliveryValue(evidence.StreamEpoch, 512) ||
		!validOpaqueDeliveryValue(evidence.Cursor, proto.MaxDurableCursorBytes) || !validLowerHexSHA256(evidence.CursorDigest) {
		return false
	}
	wantCursorDigest := sha256.Sum256([]byte(evidence.Cursor))
	if evidence.CursorDigest != hex.EncodeToString(wantCursorDigest[:]) {
		return false
	}
	if evidence.BatchEventCount != 0 {
		return validInboxBatchFinalizeEvidenceShape(evidence)
	}
	if evidence.BatchDigest != "" || evidence.BatchResultDigest != "" || evidence.BatchMaterializationPlan != "" || evidence.BatchMaterializationDigest != "" ||
		!validOptionalOpaqueDeliveryValue(evidence.NamespaceID, 512) || !validOptionalOpaqueDeliveryValue(evidence.BranchID, 512) ||
		!validOpaqueDeliveryValue(evidence.Kind, 128) || !validOpaqueDeliveryValue(evidence.ArtifactID, 512) ||
		!validOpaqueDeliveryValue(evidence.WireEventID, 512) || !validLowerHexSHA256(evidence.BodyDigest) ||
		!validOptionalLowerHexSHA256(evidence.ParentHash) || !validOpaqueDeliveryValue(evidence.EventType, 128) ||
		!validOpaqueDeliveryValue(evidence.Origin, 512) || !validOptionalOpaqueDeliveryValue(evidence.SourceAgent, 512) ||
		!validOptionalOpaqueDeliveryValue(evidence.Lane, 128) {
		return false
	}
	if evidence.Lane == "retained" {
		if !validLowerHexSHA256(evidence.CheckpointAlignmentHash) {
			return false
		}
	} else if evidence.CheckpointAlignmentHash != "" {
		return false
	}
	switch evidence.FinalizeKind {
	case proto.InboundFinalizeCanonicalMaterialize:
		if evidence.Clear || !validLowerHexSHA256(evidence.WireEventHash) ||
			!validOpaqueDeliveryValue(evidence.CanonicalEventID, 512) || !validLowerHexSHA256(evidence.CanonicalHash) ||
			evidence.NoopReason != "" || evidence.AuthenticatedHeaderDigest != "" || evidence.AuthenticatedSignerIdentity != "" {
			return false
		}
	case proto.InboundFinalizeAuthenticatedNoop:
		if evidence.Clear || evidence.CanonicalEventID != "" || evidence.CanonicalHash != "" ||
			!validLowerHexSHA256(evidence.AuthenticatedHeaderDigest) || !validInboxAuthenticatedSigner(evidence.AuthenticatedSignerIdentity) {
			return false
		}
		switch evidence.NoopReason {
		case proto.InboundFinalizeNoopNotRecipient:
			if !validLowerHexSHA256(evidence.WireEventHash) {
				return false
			}
		default:
			return false
		}
	default:
		return false
	}
	return true
}

func validInboxBatchFinalizeEvidenceShape(evidence proto.RemoteInboundFinalizeEvidenceV1) bool {
	if evidence.BatchEventCount < 2 || evidence.BatchEventCount > proto.RemoteReplayBatchMaxEvents || !validLowerHexSHA256(evidence.BatchDigest) ||
		!validLowerHexSHA256(evidence.BatchResultDigest) || !validOptionalOpaqueDeliveryValue(evidence.NamespaceID, 512) || evidence.BranchID != "" ||
		evidence.Kind != "" || evidence.ArtifactID != "" || evidence.WireEventID != "" || evidence.WireEventHash != "" || evidence.BodyDigest != "" ||
		evidence.ParentHash != "" || evidence.CheckpointAlignmentHash != "" || evidence.EventType != "" || evidence.TimestampUnixNano != 0 ||
		evidence.Sequence != 0 || evidence.Origin != "" || evidence.SourceAgent != "" || evidence.Lane != "" || evidence.Clear ||
		evidence.CanonicalEventID != "" || evidence.CanonicalHash != "" || evidence.NoopReason != "" || evidence.AuthenticatedHeaderDigest != "" ||
		evidence.AuthenticatedSignerIdentity != "" {
		return false
	}
	entries, err := proto.DecodeRemoteBatchMaterializationPlan(evidence.BatchMaterializationPlan, evidence.BatchMaterializationDigest)
	if err != nil {
		return false
	}
	if evidence.FinalizeKind == proto.InboundFinalizeCanonicalBatch {
		return len(entries) > 0
	}
	return evidence.FinalizeKind == proto.InboundFinalizeAuthenticatedBatchNoop && len(entries) == 0
}

func validOptionalLowerHexSHA256(value string) bool {
	return value == "" || validLowerHexSHA256(value)
}

func validInboxAuthenticatedSigner(value string) bool {
	separator := strings.LastIndexByte(value, ':')
	return separator > 0 && validOpaqueDeliveryValue(value[:separator], 512) && validLowerHexSHA256(value[separator+1:])
}

func validOptionalOpaqueDeliveryValue(value string, maximum int) bool {
	return value == "" || validOpaqueDeliveryValue(value, maximum)
}

// QuarantineTerminal durably records a delivery that can be proven terminal
// before a current security epoch is available (for example, a syntactically
// valid legacy envelope below the mandatory v2 minimum). It stores only the
// bounded delivery digest, claimed routing/security metadata, and safe reason
// code acknowledgement; artifact body bytes are never persisted. The exact
// cached acknowledgement makes redelivery idempotent.
func (i *InboundInbox) QuarantineTerminal(delivery proto.RemoteInboundDeliveryV2, ack proto.RemoteInboundAckV2) (*proto.RemoteInboundAckV2, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if ack.DeliveryID != delivery.DeliveryID || len(ack.Outcomes) != len(delivery.Events) || len(ack.Outcomes) == 0 {
		return nil, securityerr.ErrMetadataMismatch
	}
	for index, outcome := range ack.Outcomes {
		if outcome.Index != uint32(index) || outcome.Disposition != "quarantined" || outcome.ReasonCode == "" || len(outcome.ReasonCode) > inboxMaxReasonCode {
			return nil, securityerr.ErrMetadataMismatch
		}
	}
	name, err := inboxName(delivery.DeliveryID)
	if err != nil {
		return nil, err
	}
	digest, encoded, err := deliveryDigest(delivery)
	if err != nil {
		return nil, err
	}
	admission, _, err := claimedAdmission(delivery, digest, securityepoch.SecurityEpoch{})
	if err != nil {
		return nil, err
	}
	root, err := i.root()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if existing, readErr := readInboxRecord(root, name); readErr == nil {
		if existing.Admission.InputSHA256 != digest || existing.Admission.DeliveryID != delivery.DeliveryID || existing.Ack == nil {
			return nil, securityerr.ErrMetadataMismatch
		}
		cached := *existing.Ack
		return &cached, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	entries, err := root.ReadDir(".")
	if err != nil {
		return nil, err
	}
	count, total := 0, int64(len(encoded))
	for _, entry := range entries {
		if entry.Type().IsRegular() && filepath.Ext(entry.Name()) == ".json" {
			count++
			if info, statErr := os.Lstat(filepath.Join(i.Root, entry.Name())); statErr == nil {
				total += info.Size()
			}
		}
	}
	if count >= inboxMaxDeliveries || total > inboxMaxBytes {
		return nil, securityerr.ErrLimitExceeded
	}
	copyAck := ack
	record := inboxRecord{Version: inboxVersion, Admission: admission, Ack: &copyAck, CreatedAt: time.Now().UTC()}
	if err := writeInboxRecord(root, name, record); err != nil {
		return nil, err
	}
	return &copyAck, nil
}

func (i *InboundInbox) PendingIDs() ([]string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	root, err := i.root()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := root.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		record, err := readInboxRecord(root, entry.Name())
		if err != nil {
			return nil, err
		}
		if record.Delivery != nil {
			ids = append(ids, record.Admission.DeliveryID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
