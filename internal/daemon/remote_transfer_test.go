package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

func TestRemoteTransferSessionStagesExactPrivateBytesAndCleansUp(t *testing.T) {
	base := filepath.Join(t.TempDir(), "remote-transfer-v1")
	session, err := prepareRemoteTransferSession(base)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, proto.MaxSealedEventBytes+1)
	copy(payload, []byte(`{"sealed":true}`))
	event := stagedTransferTestEvent(payload)
	params, err := session.stage(context.Background(), event, "stream-1", "epoch-1")
	if err != nil {
		t.Fatal(err)
	}
	if params.Event.Bytes != nil || params.Transfer.SealedBytes != uint64(len(payload)) || params.Event.BodyDigest != params.Transfer.BodyDigest ||
		params.Transfer.BindingDigest != proto.RemoteStagedBindingDigest(params.Event, params.Transfer) {
		t.Fatalf("staged params not exactly bound: %+v", params.Transfer)
	}
	path := filepath.Join(session.path, params.Transfer.TransferID)
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("staged bytes mismatch len=%d err=%v", len(got), err)
	}
	privateFile, err := session.root.OpenReadRegular(params.Transfer.TransferID)
	if err != nil {
		t.Fatalf("staged file does not satisfy the platform private-file policy: %v", err)
	}
	if err := privateFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.remove(params.Transfer.TransferID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("staged file survived cleanup: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatalf("transfer session survived close: entries=%v err=%v", entries, err)
	}
}

func TestRemoteTransferSessionPreservesCrashResidueForOutboxRecovery(t *testing.T) {
	base := filepath.Join(t.TempDir(), "remote-transfer-v1")
	staleFile := strings.Repeat("b", 64)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, staleFile), []byte("sealed"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := prepareRemoteTransferSession(base)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 1 || entries[0].Name() != staleFile {
		t.Fatalf("durable crash residue was not preserved: entries=%v err=%v", entries, err)
	}
}

func TestRemoteTransferSessionConcurrentRetryReusesOneExactFileUntilAccepted(t *testing.T) {
	base := filepath.Join(t.TempDir(), "remote-transfer-v1")
	session, err := prepareRemoteTransferSession(base)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	payload := make([]byte, proto.MaxSealedEventBytes+1)
	copy(payload, []byte("sealed-checkpoint"))
	event := stagedTransferTestEvent(payload)
	const callers = 8
	type stagedResult struct {
		params proto.RemotePublishStagedV1Params
		key    string
		err    error
	}
	results := make(chan stagedResult, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			params, key, stageErr := session.stageOrReuse(context.Background(), event, "stream-1", "epoch-1")
			results <- stagedResult{params: params, key: key, err: stageErr}
		}()
	}
	wait.Wait()
	close(results)
	var first stagedResult
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if first.key == "" {
			first = result
			continue
		}
		if result.key != first.key || result.params.Transfer.TransferID != first.params.Transfer.TransferID || result.params.Transfer.BindingDigest != first.params.Transfer.BindingDigest {
			t.Fatalf("retry created a second transfer: first=%+v retry=%+v", first.params.Transfer, result.params.Transfer)
		}
	}
	entries, err := os.ReadDir(session.path)
	if err != nil || len(entries) != 1 {
		t.Fatalf("staged files=%v err=%v, want exactly one", entries, err)
	}
	other := event
	other.EventID = "other-event"
	if _, _, err := session.stageOrReuse(context.Background(), other, "stream-1", "epoch-1"); !errors.Is(err, errRemoteTransferBusy) {
		t.Fatalf("second active transfer error = %v, want bounded busy", err)
	}
	if err := session.complete(first.key, first.params.Transfer.TransferID); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(session.path); err != nil || len(entries) != 1 {
		t.Fatalf("outbox-owned transfer was removed before intent retirement: entries=%v err=%v", entries, err)
	}
	if err := session.remove(first.params.Transfer.TransferID); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteTransferSessionRestartReconstructsFromDurableSource(t *testing.T) {
	base := filepath.Join(t.TempDir(), "remote-transfer-v1")
	payload := make([]byte, proto.MaxSealedEventBytes+1)
	copy(payload, []byte("restart-checkpoint"))
	event := stagedTransferTestEvent(payload)
	firstSession, err := prepareRemoteTransferSession(base)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := firstSession.stageOrReuse(context.Background(), event, "stream-1", "epoch-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := firstSession.Close(); err != nil {
		t.Fatal(err)
	}
	secondSession, err := prepareRemoteTransferSession(base)
	if err != nil {
		t.Fatal(err)
	}
	defer secondSession.Close()
	second, _, err := secondSession.stageOrReuse(context.Background(), event, "stream-1", "epoch-1")
	if err != nil {
		t.Fatal(err)
	}
	if second.Transfer.TransferID != first.Transfer.TransferID || second.Transfer.BodyDigest != first.Transfer.BodyDigest || second.Transfer.SealedBytes != first.Transfer.SealedBytes {
		t.Fatalf("restart transfer first=%+v second=%+v", first.Transfer, second.Transfer)
	}
	got, err := os.ReadFile(filepath.Join(secondSession.path, second.Transfer.TransferID))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("restart bytes length=%d err=%v", len(got), err)
	}
}

func TestStagedRemotePublishTerminalRequiresExactSingleOutcome(t *testing.T) {
	accepted := proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{{EventID: "event-1", Accepted: true}}}
	if !stagedRemotePublishTerminal(accepted, "event-1") {
		t.Fatal("exact accepted outcome rejected")
	}
	rejected := proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{{EventID: "event-1", Error: "invalid durable event"}}}
	if !stagedRemotePublishTerminal(rejected, "event-1") {
		t.Fatal("exact terminal rejection rejected")
	}
	for _, result := range []proto.RemotePublishResult{
		{},
		{Outcomes: []proto.RemotePublishOutcome{{EventID: "event-1", Retryable: true}}},
		{Outcomes: []proto.RemotePublishOutcome{{EventID: "event-1"}}},
		{Outcomes: []proto.RemotePublishOutcome{{EventID: "other", Accepted: true}}},
		{Outcomes: []proto.RemotePublishOutcome{{EventID: "event-1", Accepted: true}, {EventID: "event-1", Accepted: true}}},
	} {
		if stagedRemotePublishTerminal(result, "event-1") {
			t.Fatalf("non-terminal result accepted: %+v", result)
		}
	}
}

func TestConfigureRemoteTransferEnvironmentRejectsInheritedRoot(t *testing.T) {
	cmd := exec.Command("unused")
	cmd.Env = []string{"X=1", remoteTransferRootEnv + "=/attacker", strings.ToLower(remoteTransferRootEnv) + "=/duplicate"}
	configureRemoteTransferEnvironment(cmd, nil)
	for _, value := range cmd.Env {
		if strings.HasPrefix(strings.ToUpper(value), remoteTransferRootEnv+"=") {
			t.Fatalf("inherited transfer root survived: %q", value)
		}
	}
}

func TestStagedRemoteCheckpointAuthorityRequiresSignedCapabilityAndExactDescriptor(t *testing.T) {
	event := stagedTransferTestEvent(make([]byte, proto.MaxSealedEventBytes+1))
	event.CheckpointCoverage = 0
	event.CheckpointGeneration = ""
	runner := &RemoteRunner{}
	runner.syncMode = proto.RemoteNegotiateSyncV1Result{
		SelectedProtocol: 1, Mode: proto.RemoteSyncModeShadow, FeatureGateEnabled: true, AllActiveDevicesCapable: true,
		ServerCapabilities: []string{proto.CapabilityDurableDeltaSyncV1, proto.CapabilityStagedCheckpointV1},
		Streams:            []proto.RemoteStreamDescriptorV1{{NamespaceID: event.NamespaceID, StreamID: "stream-1", StreamEpoch: "epoch-1"}},
	}
	if _, _, ok := runner.stagedRemoteCheckpointAuthority(event); ok {
		t.Fatal("unsigned staged capability accepted")
	}
	runner.syncStagedCheckpointSigned = true
	runner.syncMode.ServerCapabilities = []string{proto.CapabilityDurableDeltaSyncV1}
	if _, _, ok := runner.stagedRemoteCheckpointAuthority(event); ok {
		t.Fatal("staged transfer accepted without explicit server capability")
	}
	runner.syncMode.ServerCapabilities = []string{proto.CapabilityDurableDeltaSyncV1, proto.CapabilityStagedCheckpointV1}
	streamID, epoch, ok := runner.stagedRemoteCheckpointAuthority(event)
	if !ok || streamID != "stream-1" || epoch != "epoch-1" {
		t.Fatalf("exact signed descriptor rejected: %q %q %v", streamID, epoch, ok)
	}
	runner.syncMode.Streams[0].NamespaceID = "other"
	if _, _, ok := runner.stagedRemoteCheckpointAuthority(event); ok {
		t.Fatal("wrong namespace descriptor accepted")
	}
}

func TestRemoteRunnerHydratesVerifiesAndTerminallyCleansInboundCheckpoint(t *testing.T) {
	base := filepath.Join(t.TempDir(), "remote-transfer-v1")
	session, err := prepareRemoteTransferSession(base)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	body := []byte(`{"ciphertext":"` + strings.Repeat("x", proto.MaxSealedEventBytes) + `"}`)
	digest := sha256.Sum256(body)
	event := stagedTransferTestEvent(nil)
	event.BodyDigest = hex.EncodeToString(digest[:])
	staged := &proto.RemoteStagedFileV1{
		ProtocolVersion: proto.RemoteStagedTransferProtocolV1,
		TransferID:      strings.Repeat("a", 64),
		SealedBytes:     uint64(len(body)),
		BodyDigest:      event.BodyDigest,
		StreamID:        "stream-1",
		StreamEpoch:     "epoch-1",
	}
	staged.BindingDigest = proto.RemoteStagedBindingDigest(event, *staged)
	path := filepath.Join(base, staged.TransferID)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &RemoteRunner{transfer: session}
	runner.syncMode = proto.RemoteNegotiateSyncV1Result{
		SelectedProtocol: 1, Mode: proto.RemoteSyncModeDurableRead,
		FeatureGateEnabled: true, AllActiveDevicesCapable: true,
		ServerCapabilities: []string{proto.CapabilityStagedCheckpointV1},
		Streams:            []proto.RemoteStreamDescriptorV1{{NamespaceID: event.NamespaceID, StreamID: staged.StreamID, StreamEpoch: staged.StreamEpoch}},
	}
	runner.syncStagedCheckpointSigned = true
	delivery := proto.RemoteInboundDeliveryV2{
		DeliveryID: "delivery-1", Events: []proto.RemoteEvent{event}, StagedCheckpoint: staged,
		StreamID: staged.StreamID, StreamEpoch: staged.StreamEpoch,
	}

	hydrated, err := runner.HydrateInboundStagedCheckpoint(context.Background(), delivery)
	if err != nil {
		t.Fatal(err)
	}
	if string(hydrated.Events[0].Bytes) != string(body) || len(delivery.Events[0].Bytes) != 0 {
		t.Fatal("hydration did not preserve descriptor-only input and exact working bytes")
	}

	corrupt := append([]byte(nil), body...)
	corrupt[len(corrupt)-2] ^= 1
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.HydrateInboundStagedCheckpoint(context.Background(), delivery); err == nil {
		t.Fatal("same-length staged checkpoint digest corruption accepted")
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runner.CompleteInboundStagedCheckpoint(delivery); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("terminal staged checkpoint survived cleanup: %v", err)
	}
	if err := runner.CompleteInboundStagedCheckpoint(delivery); err != nil {
		t.Fatalf("terminal cleanup was not idempotent: %v", err)
	}
}

func stagedTransferTestEvent(payload []byte) proto.RemoteEvent {
	return proto.RemoteEvent{
		ProjectID: "project", ProjectAuthorizationGeneration: 7, AccessGeneration: 11,
		AccessSetHash: [32]byte{1, 2, 3}, SecurityBarrierID: [32]byte{4, 5, 6}, SecurityGeneration: 13,
		KeyMode: "recipient-wrap-v2", KeyVersion: 2, CheckpointCoverage: 17,
		CheckpointGeneration: strings.Repeat("c", 64), NamespaceID: "ns", BranchID: "main", ArtifactID: "artifact",
		EventID: "event-r", ParentHash: strings.Repeat("d", 64), CheckpointAlignmentHash: strings.Repeat("e", 64),
		EventHash: strings.Repeat("f", 64), Kind: "conversation", Type: "update",
		Timestamp: time.Date(2026, 7, 19, 12, 0, 0, 123, time.FixedZone("offset", -4*60*60)),
		Bytes:     payload, Sequence: 9, Origin: "device-1", SourceAgent: "codex", Lane: "retained",
	}
}
