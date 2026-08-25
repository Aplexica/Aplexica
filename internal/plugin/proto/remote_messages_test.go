package proto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRemoteStagedBindingDigestCompatibilityVector(t *testing.T) {
	event := RemoteEvent{
		ProjectID: "project-1", ProjectAuthorizationGeneration: 2,
		AccessGeneration: 3, AccessSetHash: [RemoteDigestBytes]byte{1, 2, 3},
		SecurityBarrierID: [RemoteDigestBytes]byte{4, 5, 6}, SecurityGeneration: 7,
		KeyMode: "account", KeyVersion: 8, CheckpointCoverage: 9, CheckpointGeneration: "generation-1",
		NamespaceID: "namespace-1", BranchID: "main", ArtifactID: "artifact-1", EventID: "event-1",
		ParentHash: strings.Repeat("1", 64), CheckpointAlignmentHash: strings.Repeat("2", 64),
		EventHash: strings.Repeat("3", 64), BodyDigest: strings.Repeat("4", 64), Kind: "conversation", Type: "checkpoint",
		Timestamp: time.Date(2026, time.July, 19, 12, 34, 56, 789, time.UTC), Sequence: 10,
		Origin: "device-1", SourceAgent: "codex", Lane: "retained",
	}
	transfer := RemoteStagedFileV1{
		ProtocolVersion: RemoteStagedTransferProtocolV1, TransferID: strings.Repeat("a", 64), SealedBytes: 4<<20 + 17,
		BodyDigest: event.BodyDigest, StreamID: "stream-1", StreamEpoch: "epoch-1",
	}
	const want = "25a87219045db0c7a0d020d0223e53adc772fe215a77f7a8b2eda3dc23c4a09f"
	if got := RemoteStagedBindingDigest(event, transfer); got != want {
		t.Fatalf("staged binding digest = %s, want %s", got, want)
	}
}

func TestRemoteEvent_JSONRoundtrip(t *testing.T) {
	in := RemoteEvent{
		NamespaceID:             "ns-abc",
		BranchID:                "br-main",
		ArtifactID:              "art-1",
		EventID:                 "evt-1",
		ParentHash:              "evt-0",
		CheckpointAlignmentHash: "aligned-head-1",
		EventHash:               "canonical-hash-1",
		BodyDigest:              "7d793037a0760186574b0282f2f435e7",
		Kind:                    "memory",
		Type:                    "update",
		Timestamp:               time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC),
		Bytes:                   json.RawMessage(`{"opaque":"ciphertext-here"}`),
		Sequence:                42,
		Origin:                  "dev-alpha",
	}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out RemoteEvent
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.NamespaceID != in.NamespaceID || out.EventID != in.EventID || out.CheckpointAlignmentHash != in.CheckpointAlignmentHash || out.EventHash != in.EventHash || out.BodyDigest != in.BodyDigest || out.Sequence != in.Sequence {
		t.Errorf("roundtrip mismatch: %+v != %+v", out, in)
	}
}

func TestRemoteEvent_LegacyJSONRemainsCompatible(t *testing.T) {
	legacy := []byte(`{"namespace_id":"ns","branch_id":"main","artifact_id":"a","event_id":"wire-1","parent_hash":"canonical-0","kind":"conversation","event_type":"update","ts":"2026-07-19T00:00:00Z","bytes":{},"seq":1,"origin":"dev"}`)
	var event RemoteEvent
	if err := json.Unmarshal(legacy, &event); err != nil {
		t.Fatalf("Unmarshal legacy event: %v", err)
	}
	if event.EventHash != "" || event.BodyDigest != "" || event.CheckpointAlignmentHash != "" {
		t.Fatalf("legacy event acquired durable fields: %+v", event)
	}
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal legacy event: %v", err)
	}
	for _, field := range []string{"event_hash", "body_digest", "checkpoint_alignment_hash"} {
		if contains(string(body), field) {
			t.Fatalf("zero-value durable field %q leaked into legacy JSON: %s", field, body)
		}
	}
}

func TestRemotePublishOutcome_RetryHints(t *testing.T) {
	in := RemotePublishOutcome{
		EventID:    "evt-1",
		Accepted:   false,
		Retryable:  true,
		RetryAfter: 5 * time.Second,
		Error:      "rate limited",
	}
	body, _ := json.Marshal(in)
	var out RemotePublishOutcome
	_ = json.Unmarshal(body, &out)
	if !out.Retryable || out.RetryAfter != 5*time.Second {
		t.Errorf("retry hint roundtrip wrong: %+v", out)
	}
}

func TestRemotePublishOutcome_DurableReceiptRoundtrip(t *testing.T) {
	in := RemotePublishOutcome{
		EventID:             "evt-1",
		Accepted:            true,
		Durability:          RemoteDurabilityCommitted,
		CommitCursor:        "cursor-7",
		CommitPosition:      7,
		StreamID:            "stream-1",
		StreamEpoch:         "epoch-2",
		BodyDigest:          "sha256-body",
		EventIdentityDigest: "event-identity",
		MetadataDigest:      "metadata",
		Duplicate:           true,
	}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out RemotePublishOutcome
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("durable receipt mismatch: %+v != %+v", out, in)
	}
}

func TestRemotePublishOutcome_LegacyShapeOmitsDurability(t *testing.T) {
	body, err := json.Marshal(RemotePublishOutcome{EventID: "evt-1", Accepted: true})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, field := range []string{"durability", "commit_cursor", "commit_position", "stream_id", "stream_epoch", "body_digest", "event_identity_digest", "metadata_digest", "duplicate"} {
		if contains(string(body), field) {
			t.Fatalf("legacy outcome includes %q: %s", field, body)
		}
	}
}

func TestDurableSyncMessages_JSONRoundtrip(t *testing.T) {
	params := RemoteNegotiateSyncV1Params{
		ProtocolMin:          1,
		ProtocolMax:          1,
		DaemonCapabilities:   []string{CapabilityDurableDeltaSyncV1, CapabilityInboundAckV2},
		RequestedMaximumMode: RemoteSyncModeShadow,
	}
	body, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("Marshal negotiation params: %v", err)
	}
	var decodedParams RemoteNegotiateSyncV1Params
	if err := json.Unmarshal(body, &decodedParams); err != nil || decodedParams.RequestedMaximumMode != RemoteSyncModeShadow {
		t.Fatalf("negotiation params mismatch: %+v, err=%v", decodedParams, err)
	}

	negotiation := RemoteNegotiateSyncV1Result{
		SelectedProtocol:        1,
		Mode:                    RemoteSyncModeShadow,
		ServerCapabilities:      []string{CapabilityDurableDeltaSyncV1},
		AllActiveDevicesCapable: true,
		CheckpointReady:         true,
		FeatureGateEnabled:      true,
		StreamID:                "stream-1",
		StreamEpoch:             "epoch-1",
		Streams: []RemoteStreamDescriptorV1{
			{NamespaceID: "", StreamID: "stream-1", StreamEpoch: "epoch-1", MaxEventBytes: 4 << 20, MaxPageEvents: 100, MaxPageBytes: 32 << 20, MinAvailableCursor: "cursor-0", TipCursor: "cursor-9", TipPosition: 9, RetentionSeconds: 86400, CheckpointReady: true},
			{NamespaceID: "namespace-a", StreamID: "stream-a", StreamEpoch: "epoch-a", MaxEventBytes: 4 << 20, MaxPageEvents: 100, MaxPageBytes: 32 << 20, MinAvailableCursor: "cursor-a0", TipCursor: "cursor-a4", TipPosition: 4, RetentionSeconds: 86400, CheckpointReady: true},
		},
		MaxEventBytes:      4 << 20,
		MaxPageEvents:      100,
		MaxPageBytes:       32 << 20,
		MinAvailableCursor: "cursor-0",
		RetentionSeconds:   86400,
	}
	body, err = json.Marshal(negotiation)
	if err != nil {
		t.Fatalf("Marshal negotiation: %v", err)
	}
	var decoded RemoteNegotiateSyncV1Result
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("Unmarshal negotiation: %v", err)
	}
	if decoded.StreamEpoch != negotiation.StreamEpoch || decoded.MaxPageEvents != negotiation.MaxPageEvents || decoded.Mode != RemoteSyncModeShadow || len(decoded.Streams) != 2 || decoded.Streams[1] != negotiation.Streams[1] {
		t.Fatalf("negotiation mismatch: %+v", decoded)
	}

	resume := RemoteResumeCursorV1Params{Authoritative: true, StreamID: "stream-1", StreamEpoch: "epoch-1", CursorPresent: true, Cursor: "cursor-4", CursorDigest: "digest-4", Position: 4}
	body, _ = json.Marshal(resume)
	var decodedResume RemoteResumeCursorV1Params
	if err := json.Unmarshal(body, &decodedResume); err != nil || decodedResume != resume {
		t.Fatalf("resume cursor roundtrip mismatch: %+v, err=%v", decodedResume, err)
	}
	pluralResume := RemoteResumeCursorsV1Params{Cursors: []RemoteResumeCursorV1Params{resume, {Authoritative: true, StreamID: "stream-a", StreamEpoch: "epoch-a"}}}
	body, _ = json.Marshal(pluralResume)
	var decodedPluralResume RemoteResumeCursorsV1Params
	if err := json.Unmarshal(body, &decodedPluralResume); err != nil || len(decodedPluralResume.Cursors) != 2 || decodedPluralResume.Cursors[0] != resume || decodedPluralResume.Cursors[1] != pluralResume.Cursors[1] {
		t.Fatalf("plural resume cursor roundtrip mismatch: %+v, err=%v", decodedPluralResume, err)
	}
	pluralResult := RemoteResumeCursorsV1Result{Accepted: true, Cursors: []RemoteResumeCursorV1Result{{Accepted: true, StreamID: "stream-1", StreamEpoch: "epoch-1", CursorPresent: true, Cursor: "cursor-4", CursorDigest: "digest-4", Position: 4}, {Accepted: true, StreamID: "stream-a", StreamEpoch: "epoch-a"}}}
	body, _ = json.Marshal(pluralResult)
	var decodedPluralResult RemoteResumeCursorsV1Result
	if err := json.Unmarshal(body, &decodedPluralResult); err != nil || !decodedPluralResult.Accepted || len(decodedPluralResult.Cursors) != 2 || decodedPluralResult.Cursors[1].StreamID != "stream-a" {
		t.Fatalf("plural resume result roundtrip mismatch: %+v, err=%v", decodedPluralResult, err)
	}

	fetch := RemoteFetchV2Params{StreamID: "stream-1", StreamEpoch: "epoch-1", Cursor: "cursor-4", CursorDigest: "digest-4", Position: 4, LimitEvents: 1, LimitBytes: 4096}
	body, _ = json.Marshal(fetch)
	var decodedFetch RemoteFetchV2Params
	if err := json.Unmarshal(body, &decodedFetch); err != nil || decodedFetch != fetch {
		t.Fatalf("fetch roundtrip mismatch: %+v, err=%v", decodedFetch, err)
	}
	staged := &RemoteStagedFileV1{ProtocolVersion: RemoteStagedTransferProtocolV1, TransferID: strings.Repeat("a", 64), SealedBytes: MaxSealedEventBytes + 1, BodyDigest: strings.Repeat("b", 64), BindingDigest: strings.Repeat("c", 64), StreamID: "stream-1", StreamEpoch: "epoch-1"}
	fetchResult := RemoteFetchV2Result{Events: []RemoteEvent{{EventID: "event-5"}}, StagedCheckpoint: staged, PredecessorCursor: "cursor-4", PredecessorPosition: 4, NextCursor: "cursor-5", NextCursorDigest: "digest-5", NextPosition: 5, StreamEpoch: "epoch-1", HasMore: true}
	body, _ = json.Marshal(fetchResult)
	var decodedFetchResult RemoteFetchV2Result
	if err := json.Unmarshal(body, &decodedFetchResult); err != nil || decodedFetchResult.PredecessorCursor != fetchResult.PredecessorCursor || decodedFetchResult.PredecessorPosition != fetchResult.PredecessorPosition || decodedFetchResult.NextPosition != fetchResult.NextPosition || decodedFetchResult.StagedCheckpoint == nil || *decodedFetchResult.StagedCheckpoint != *staged {
		t.Fatalf("fetch result roundtrip mismatch: %+v, err=%v", decodedFetchResult, err)
	}
	recovery := RemoteFetchParentV1Result{Found: true, Record: &RemoteRecoveryEventV1{Event: RemoteEvent{EventID: "parent", EventHash: "hash-parent", CheckpointCoverage: 2}, StagedCheckpoint: staged, PredecessorCursor: "cursor-3", PredecessorPosition: 3, Cursor: "cursor-4", CursorDigest: "digest-4", Position: 4, CoverageCursor: "cursor-2", CoverageCursorDigest: "coverage-digest-2", CoveragePosition: 2}}
	body, _ = json.Marshal(recovery)
	var decodedRecovery RemoteFetchParentV1Result
	if err := json.Unmarshal(body, &decodedRecovery); err != nil || !decodedRecovery.Found || decodedRecovery.Record == nil || decodedRecovery.Record.Position != recovery.Record.Position || decodedRecovery.Record.Event.EventID != "parent" || decodedRecovery.Record.CoverageCursor != recovery.Record.CoverageCursor || decodedRecovery.Record.CoveragePosition != recovery.Record.CoveragePosition || decodedRecovery.Record.StagedCheckpoint == nil || *decodedRecovery.Record.StagedCheckpoint != *staged {
		t.Fatalf("recovery result roundtrip mismatch: %+v, err=%v", decodedRecovery, err)
	}

	ack := RemoteAckV2Params{StreamID: "stream-1", StreamEpoch: "epoch-1", Cursor: "cursor-5", CursorDigest: "index-digest", Position: 5}
	body, _ = json.Marshal(ack)
	var decodedAck RemoteAckV2Params
	if err := json.Unmarshal(body, &decodedAck); err != nil || decodedAck != ack {
		t.Fatalf("ack roundtrip mismatch: %+v, err=%v", decodedAck, err)
	}

	checkpoint := RemoteCheckpointNeededV1Notification{
		RequestID: "request-1", RequestingDeviceID: "device-1", StreamID: "stream-1", StreamEpoch: "epoch-1",
		NamespaceID: "ns", BranchID: "main", ArtifactID: "artifact", Kind: "conversation",
		Reason: "missing-parent", MissingParentHash: strings.Repeat("1", 64), MinAvailableCursor: "cursor-4",
		CheckpointCoverage: 5, CheckpointAlignmentHash: strings.Repeat("2", 64), CheckpointGeneration: strings.Repeat("3", 64),
		AccessGeneration: 7, AccessSetHash: [RemoteDigestBytes]byte{1, 2, 3},
		SecurityGeneration: 8, SecurityBarrierID: [RemoteDigestBytes]byte{4, 5, 6}, KeyMode: "recipient-wrap-v2",
	}
	body, _ = json.Marshal(checkpoint)
	var decodedCheckpoint RemoteCheckpointNeededV1Notification
	if err := json.Unmarshal(body, &decodedCheckpoint); err != nil || decodedCheckpoint != checkpoint {
		t.Fatalf("checkpoint roundtrip mismatch: %+v, err=%v", decodedCheckpoint, err)
	}

	delivery := RemoteInboundDeliveryV2{
		DeliveryID: "delivery-1", Cursor: "cursor-6", Events: []RemoteEvent{{EventID: "event-6", CheckpointCoverage: 5, CheckpointGeneration: "checkpoint-generation-1", CheckpointAlignmentHash: "alignment-hash-5"}},
		StagedCheckpoint: staged,
		ProtocolVersion:  1, StreamID: "stream-1", StreamEpoch: "epoch-1",
		PredecessorCursor: "cursor-5", PredecessorPosition: 5,
		Position: 6, CursorDigest: "index-digest-6",
	}
	body, _ = json.Marshal(delivery)
	var decodedDelivery RemoteInboundDeliveryV2
	if err := json.Unmarshal(body, &decodedDelivery); err != nil || decodedDelivery.StreamID != delivery.StreamID || decodedDelivery.StreamEpoch != delivery.StreamEpoch || decodedDelivery.PredecessorCursor != delivery.PredecessorCursor || decodedDelivery.PredecessorPosition != delivery.PredecessorPosition || decodedDelivery.Position != delivery.Position || decodedDelivery.CursorDigest != delivery.CursorDigest || decodedDelivery.Events[0].CheckpointGeneration != delivery.Events[0].CheckpointGeneration || decodedDelivery.Events[0].CheckpointAlignmentHash != delivery.Events[0].CheckpointAlignmentHash || decodedDelivery.StagedCheckpoint == nil || *decodedDelivery.StagedCheckpoint != *staged {
		t.Fatalf("inbound delivery roundtrip mismatch: %+v, err=%v", decodedDelivery, err)
	}

	finalize := RemoteInboundFinalizeV1Params{Evidence: RemoteInboundFinalizeEvidenceV1{
		ProtocolVersion: 1, FinalizeKind: InboundFinalizeCanonicalMaterialize,
		RemoteIdentity: "device-1", DeliveryID: "delivery-1",
		StreamID: "stream-1", StreamEpoch: "epoch-1", Cursor: "cursor-6",
		CursorDigest: "digest-6", Position: 6, NamespaceID: "namespace-1",
		BranchID: "main", Kind: "conversation", ArtifactID: "artifact-1",
		WireEventID: "event-6", WireEventHash: "wire-hash", BodyDigest: "body-digest",
		ParentHash: "parent-hash", CheckpointAlignmentHash: "alignment-hash", EventType: "update", TimestampUnixNano: 6, Sequence: 6,
		Origin: "device-2", SourceAgent: "codex", Lane: "retained",
		CanonicalEventID: "event-6", CanonicalHash: "canonical-hash",
	}}
	body, _ = json.Marshal(finalize)
	var decodedFinalize RemoteInboundFinalizeV1Params
	if err := json.Unmarshal(body, &decodedFinalize); err != nil || decodedFinalize != finalize {
		t.Fatalf("inbound finalize roundtrip mismatch: %+v, err=%v", decodedFinalize, err)
	}
	params.PendingFinalizeEvidence = &finalize.Evidence
	body, _ = json.Marshal(params)
	decodedParams = RemoteNegotiateSyncV1Params{}
	if err := json.Unmarshal(body, &decodedParams); err != nil || decodedParams.PendingFinalizeEvidence == nil || *decodedParams.PendingFinalizeEvidence != finalize.Evidence {
		t.Fatalf("negotiation pending-finalize roundtrip mismatch: %+v, err=%v", decodedParams, err)
	}
	negotiation.PendingFinalizeEvidence = &finalize.Evidence
	body, _ = json.Marshal(negotiation)
	decoded = RemoteNegotiateSyncV1Result{}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.PendingFinalizeEvidence == nil || *decoded.PendingFinalizeEvidence != finalize.Evidence {
		t.Fatalf("negotiation result pending-finalize roundtrip mismatch: %+v, err=%v", decoded, err)
	}
	resume.PendingFinalizeEvidence = &finalize.Evidence
	body, _ = json.Marshal(resume)
	decodedResume = RemoteResumeCursorV1Params{}
	if err := json.Unmarshal(body, &decodedResume); err != nil || decodedResume.PendingFinalizeEvidence == nil || *decodedResume.PendingFinalizeEvidence != finalize.Evidence {
		t.Fatalf("resume pending-finalize roundtrip mismatch: %+v, err=%v", decodedResume, err)
	}
	resumeResult := RemoteResumeCursorV1Result{
		Accepted: true, StreamID: resume.StreamID, StreamEpoch: resume.StreamEpoch,
		CursorPresent: true, Cursor: resume.Cursor, CursorDigest: resume.CursorDigest, Position: resume.Position,
		PendingFinalizeEvidence: &finalize.Evidence,
	}
	body, _ = json.Marshal(resumeResult)
	var decodedResumeResult RemoteResumeCursorV1Result
	if err := json.Unmarshal(body, &decodedResumeResult); err != nil || decodedResumeResult.PendingFinalizeEvidence == nil || *decodedResumeResult.PendingFinalizeEvidence != finalize.Evidence {
		t.Fatalf("resume result pending-finalize roundtrip mismatch: %+v, err=%v", decodedResumeResult, err)
	}
}

func TestRemoteInboundDeliveryV2_LegacyShapeOmitsDurableMetadata(t *testing.T) {
	body, err := json.Marshal(RemoteInboundDeliveryV2{DeliveryID: "delivery-1", Cursor: "cursor-1", Events: []RemoteEvent{{EventID: "event-1"}}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, field := range []string{"staged_checkpoint", "protocol_version", "stream_id", "stream_epoch", "predecessor_cursor", "predecessor_position", "position", "cursor_digest"} {
		if contains(string(body), field) {
			t.Fatalf("legacy inbound-v2 delivery includes %q: %s", field, body)
		}
	}
}

func TestRemoteInboundAckV2_LegacyShapeOmitsFinalizeEvidence(t *testing.T) {
	body, err := json.Marshal(RemoteInboundAckV2{
		DeliveryID: "delivery-1",
		Outcomes:   []RemoteInboundEventOutcomeV2{{Index: 0, Disposition: "accepted", ReasonCode: "durable"}},
		NextCursor: "cursor-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(body), "finalize_evidence") {
		t.Fatalf("legacy/shadow inbound ACK changed shape: %s", body)
	}
}

func TestRemoteStatusResult_OmitsZeroTimes(t *testing.T) {
	s := RemoteStatusResult{ConnState: "connecting"}
	body, _ := json.Marshal(s)
	str := string(body)
	// omitzero on Time fields should keep them out of the JSON when zero.
	for _, k := range []string{"last_conn_attempt", "last_successful_sync"} {
		if contains(str, k) {
			t.Errorf("expected %q to be omitted; body=%s", k, str)
		}
	}
}

func TestRemoteMethodConstantsDoNotCollideWithAdapterMethods(t *testing.T) {
	adapterMethods := map[string]bool{
		MethodInitialize:    true,
		MethodImport:        true,
		MethodExport:        true,
		MethodNativePath:    true,
		MethodHandlesFormat: true,
		MethodCapabilities:  true,
		MethodShutdown:      true,
	}
	remoteMethods := []string{
		MethodRemotePublish, MethodRemoteFetch, MethodRemoteEnumerate,
		MethodRemoteSubscribe, MethodRemoteUnsubscribe, MethodRemoteStatus,
		MethodRemoteNegotiateSyncV1, MethodRemoteResumeCursorV1, MethodRemoteResumeCursorsV1, MethodRemoteFetchV2, MethodRemoteFetchParentV1, MethodRemoteAckV2,
		MethodRemoteRequestCheckpointV1, MethodRemoteInboundFinalizeV1, NotificationRemoteCheckpointNeededV1,
	}
	for _, m := range remoteMethods {
		if adapterMethods[m] {
			t.Errorf("remote method %q collides with adapter method", m)
		}
	}
}

func TestRemoteKindConstant(t *testing.T) {
	if RemoteKind != "remote" {
		t.Errorf("RemoteKind = %q, want \"remote\"", RemoteKind)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
