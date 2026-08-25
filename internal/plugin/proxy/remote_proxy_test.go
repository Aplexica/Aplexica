package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/host"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

func TestValidOpaqueDeliveryValueUsesDistinctDeliveryAndCursorBounds(t *testing.T) {
	if !validOpaqueDeliveryValue(strings.Repeat("d", proto.MaxDeliveryIDBytes), proto.MaxDeliveryIDBytes) ||
		validOpaqueDeliveryValue(strings.Repeat("d", proto.MaxDeliveryIDBytes+1), proto.MaxDeliveryIDBytes) {
		t.Fatal("delivery id bound is not exact")
	}
	if !validOpaqueDeliveryValue(strings.Repeat("c", proto.MaxDurableCursorBytes), proto.MaxDurableCursorBytes) ||
		validOpaqueDeliveryValue(strings.Repeat("c", proto.MaxDurableCursorBytes+1), proto.MaxDurableCursorBytes) {
		t.Fatal("durable cursor bound is not exact")
	}
}

func TestEnqueueInboundV2AllowsCloudCursorBoundWithoutWideningDeliveryID(t *testing.T) {
	p := &RemoteProxy{inboundCh: make(chan inboundDelivery, 1)}
	frameFor := func(delivery proto.RemoteInboundDeliveryV2) []byte {
		params, err := json.Marshal(delivery)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := json.Marshal(proto.Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: proto.MethodRemoteInboundDeliveryV2, Params: params})
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	delivery := proto.RemoteInboundDeliveryV2{
		DeliveryID: "delivery-1",
		Cursor:     strings.Repeat("c", proto.MaxDurableCursorBytes),
		Events:     []proto.RemoteEvent{{EventID: "event-1", Bytes: json.RawMessage(`{}`)}},
	}
	if err := p.enqueueInbound(frameFor(delivery), false); err != nil {
		t.Fatalf("max cloud cursor rejected: %v", err)
	}
	queued := <-p.inboundCh
	p.inboundBytes.Add(-queued.bytes)

	delivery.Cursor += "c"
	if err := p.enqueueInbound(frameFor(delivery), false); err == nil {
		t.Fatal("over-limit cloud cursor accepted")
	}
	delivery.Cursor = "cursor"
	delivery.DeliveryID = strings.Repeat("d", proto.MaxDeliveryIDBytes+1)
	if err := p.enqueueInbound(frameFor(delivery), false); err == nil {
		t.Fatal("over-limit delivery id accepted")
	}
}

func TestEnqueueInboundV2RequiresAuthenticatedDurableAdjacency(t *testing.T) {
	p := &RemoteProxy{inboundCh: make(chan inboundDelivery, 2)}
	frameFor := func(delivery proto.RemoteInboundDeliveryV2) []byte {
		params, err := json.Marshal(delivery)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := json.Marshal(proto.Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: proto.MethodRemoteInboundDeliveryV2, Params: params})
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	deliveryFor := func(predecessor, cursor string, predecessorPosition, position uint64) proto.RemoteInboundDeliveryV2 {
		digest := sha256.Sum256([]byte(cursor))
		return proto.RemoteInboundDeliveryV2{
			DeliveryID:          "delivery-1",
			Cursor:              cursor,
			Events:              []proto.RemoteEvent{{EventID: "event-1", Bytes: json.RawMessage(`{}`)}},
			ProtocolVersion:     1,
			StreamID:            "stream-1",
			StreamEpoch:         "epoch-1",
			PredecessorCursor:   predecessor,
			PredecessorPosition: predecessorPosition,
			Position:            position,
			CursorDigest:        hex.EncodeToString(digest[:]),
		}
	}
	accept := func(delivery proto.RemoteInboundDeliveryV2) {
		t.Helper()
		if err := p.enqueueInbound(frameFor(delivery), false); err != nil {
			t.Fatalf("valid durable delivery rejected: %v", err)
		}
		queued := <-p.inboundCh
		p.inboundBytes.Add(-queued.bytes)
	}
	reject := func(name string, delivery proto.RemoteInboundDeliveryV2) {
		t.Helper()
		if err := p.enqueueInbound(frameFor(delivery), false); err == nil {
			t.Fatalf("%s durable delivery accepted", name)
		}
	}

	accept(deliveryFor("signed-position-zero", "signed-position-one", 0, 1))
	valid := deliveryFor("signed-position-one", "signed-position-two", 1, 2)
	accept(valid)

	wrongDigest := valid
	wrongDigest.CursorDigest = strings.Repeat("0", sha256.Size*2)
	reject("cursor digest mismatch", wrongDigest)
	skipped := valid
	skipped.Position = 3
	reject("non-adjacent position", skipped)
	missingPredecessor := valid
	missingPredecessor.PredecessorCursor = ""
	reject("missing predecessor cursor", missingPredecessor)
	partial := valid
	partial.StreamEpoch = ""
	reject("partial metadata", partial)
}

func TestEnqueueInboundV2AcceptsOnlyExactStagedCheckpointDescriptor(t *testing.T) {
	p := &RemoteProxy{inboundCh: make(chan inboundDelivery, 1)}
	frameFor := func(delivery proto.RemoteInboundDeliveryV2) []byte {
		params, err := json.Marshal(delivery)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := json.Marshal(proto.Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: proto.MethodRemoteInboundDeliveryV2, Params: params})
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	cursor := "signed-position-one"
	cursorDigest := sha256.Sum256([]byte(cursor))
	event := proto.RemoteEvent{
		NamespaceID: "namespace-1", BranchID: "main", ArtifactID: "artifact-1", EventID: "checkpoint-1",
		EventHash: strings.Repeat("e", 64), BodyDigest: strings.Repeat("b", 64), Kind: "conversation", Type: "checkpoint",
		CheckpointCoverage: 1, CheckpointGeneration: strings.Repeat("c", 64), CheckpointAlignmentHash: strings.Repeat("d", 64), Lane: "retained",
	}
	staged := &proto.RemoteStagedFileV1{
		ProtocolVersion: proto.RemoteStagedTransferProtocolV1, TransferID: strings.Repeat("a", 64),
		SealedBytes: proto.MaxSealedEventBytes + 1, BodyDigest: event.BodyDigest, StreamID: "stream-1", StreamEpoch: "epoch-1",
	}
	staged.BindingDigest = proto.RemoteStagedBindingDigest(event, *staged)
	delivery := proto.RemoteInboundDeliveryV2{
		DeliveryID: "delivery-1", Cursor: cursor, Events: []proto.RemoteEvent{event}, StagedCheckpoint: staged,
		ProtocolVersion: 1, StreamID: staged.StreamID, StreamEpoch: staged.StreamEpoch,
		PredecessorCursor: "signed-position-zero", Position: 1, CursorDigest: hex.EncodeToString(cursorDigest[:]),
	}

	if err := p.enqueueInbound(frameFor(delivery), false); err != nil {
		t.Fatalf("valid staged checkpoint rejected: %v", err)
	}
	queued := <-p.inboundCh
	p.inboundBytes.Add(-queued.bytes)
	if err := p.enqueueInbound(frameFor(delivery), true); err == nil {
		t.Fatal("legacy transport accepted staged checkpoint")
	}

	for name, mutate := range map[string]func(*proto.RemoteInboundDeliveryV2){
		"binding": func(candidate *proto.RemoteInboundDeliveryV2) {
			candidate.StagedCheckpoint.BindingDigest = strings.Repeat("f", 64)
		},
		"body": func(candidate *proto.RemoteInboundDeliveryV2) {
			candidate.Events[0].BodyDigest = strings.Repeat("f", 64)
		},
		"bytes": func(candidate *proto.RemoteInboundDeliveryV2) { candidate.Events[0].Bytes = json.RawMessage(`{}`) },
		"shape": func(candidate *proto.RemoteInboundDeliveryV2) { candidate.Events[0].CheckpointGeneration = "" },
		"size": func(candidate *proto.RemoteInboundDeliveryV2) {
			candidate.StagedCheckpoint.SealedBytes = proto.MaxSealedEventBytes
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := delivery
			candidate.Events = append([]proto.RemoteEvent(nil), delivery.Events...)
			clone := *delivery.StagedCheckpoint
			candidate.StagedCheckpoint = &clone
			mutate(&candidate)
			if err := p.enqueueInbound(frameFor(candidate), false); err == nil {
				t.Fatal("tampered staged checkpoint accepted")
			}
		})
	}
}

func proxyFinalizeEvidence() proto.RemoteInboundFinalizeEvidenceV1 {
	cursor := "signed-position-one"
	cursorDigest := sha256.Sum256([]byte(cursor))
	return proto.RemoteInboundFinalizeEvidenceV1{
		ProtocolVersion: 1, FinalizeKind: proto.InboundFinalizeCanonicalMaterialize,
		RemoteIdentity: "device-1", DeliveryID: "delivery-1",
		StreamID: "stream-1", StreamEpoch: "epoch-1", Cursor: cursor,
		CursorDigest: hex.EncodeToString(cursorDigest[:]), Position: 1,
		NamespaceID: "namespace-1", BranchID: "main", Kind: "conversation",
		ArtifactID: "artifact-1", WireEventID: "event-1",
		WireEventHash: strings.Repeat("a", sha256.Size*2), BodyDigest: strings.Repeat("b", sha256.Size*2),
		EventType: "update", TimestampUnixNano: 1, Sequence: 1, Origin: "device-2", Lane: "live",
		CanonicalEventID: "canonical-event-1", CanonicalHash: strings.Repeat("c", sha256.Size*2),
	}
}

func TestEnqueueInboundFinalizeRequiresExactBoundedEvidence(t *testing.T) {
	p := &RemoteProxy{finalizeCh: make(chan inboundFinalizeRequest, 1)}
	frameFor := func(evidence proto.RemoteInboundFinalizeEvidenceV1) []byte {
		params, err := json.Marshal(proto.RemoteInboundFinalizeV1Params{Evidence: evidence})
		if err != nil {
			t.Fatal(err)
		}
		frame, err := json.Marshal(proto.Request{JSONRPC: "2.0", ID: json.RawMessage(`7`), Method: proto.MethodRemoteInboundFinalizeV1, Params: params})
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}

	evidence := proxyFinalizeEvidence()
	if err := p.enqueueInboundFinalize(frameFor(evidence)); err != nil {
		t.Fatalf("valid finalize rejected: %v", err)
	}
	<-p.finalizeCh

	wrongCursor := evidence
	wrongCursor.Cursor = "substituted"
	if err := p.enqueueInboundFinalize(frameFor(wrongCursor)); err == nil {
		t.Fatal("cursor substitution accepted")
	}
	missingCanonical := evidence
	missingCanonical.CanonicalHash = ""
	if err := p.enqueueInboundFinalize(frameFor(missingCanonical)); err == nil {
		t.Fatal("missing canonical evidence accepted")
	}
	liveAlignment := evidence
	liveAlignment.CheckpointAlignmentHash = strings.Repeat("d", sha256.Size*2)
	if err := p.enqueueInboundFinalize(frameFor(liveAlignment)); err == nil {
		t.Fatal("live finalize smuggled checkpoint alignment")
	}
	checkpoint := evidence
	checkpoint.Lane = "retained"
	checkpoint.ParentHash = ""
	checkpoint.CheckpointAlignmentHash = strings.Repeat("d", sha256.Size*2)
	if err := p.enqueueInboundFinalize(frameFor(checkpoint)); err != nil {
		t.Fatalf("valid retained checkpoint finalize rejected: %v", err)
	}
	<-p.finalizeCh
	checkpoint.CheckpointAlignmentHash = ""
	if err := p.enqueueInboundFinalize(frameFor(checkpoint)); err == nil {
		t.Fatal("retained checkpoint finalize omitted alignment")
	}
}

func TestInboundFinalizeResultRequiresExactThreeWaySuccessXOR(t *testing.T) {
	canonical := proxyFinalizeEvidence()
	noop := canonical
	noop.FinalizeKind = proto.InboundFinalizeAuthenticatedNoop
	noop.CanonicalEventID, noop.CanonicalHash = "", ""
	noop.NoopReason = proto.InboundFinalizeNoopNotRecipient
	noop.AuthenticatedHeaderDigest = strings.Repeat("d", sha256.Size*2)
	noop.AuthenticatedSignerIdentity = "device-2:" + strings.Repeat("e", sha256.Size*2)
	for name, tc := range map[string]struct {
		evidence proto.RemoteInboundFinalizeEvidenceV1
		result   proto.RemoteInboundFinalizeV1Result
	}{
		"materialized":       {canonical, proto.RemoteInboundFinalizeV1Result{Accepted: true, Materialized: true}},
		"authenticated noop": {noop, proto.RemoteInboundFinalizeV1Result{Accepted: true, NoopFinalized: true}},
		"already canonical":  {canonical, proto.RemoteInboundFinalizeV1Result{Accepted: true, AlreadyFinalized: true}},
		"already noop":       {noop, proto.RemoteInboundFinalizeV1Result{Accepted: true, AlreadyFinalized: true}},
		"bounded rejection":  {canonical, proto.RemoteInboundFinalizeV1Result{ReasonCode: "metadata-invalid"}},
	} {
		t.Run(name, func(t *testing.T) {
			if !validInboundFinalizeResult(tc.evidence, tc.result) {
				t.Fatalf("valid result rejected: %+v", tc.result)
			}
		})
	}
	for name, tc := range map[string]struct {
		evidence proto.RemoteInboundFinalizeEvidenceV1
		result   proto.RemoteInboundFinalizeV1Result
	}{
		"accepted without disposition": {canonical, proto.RemoteInboundFinalizeV1Result{Accepted: true}},
		"two dispositions":             {canonical, proto.RemoteInboundFinalizeV1Result{Accepted: true, Materialized: true, NoopFinalized: true}},
		"success reason":               {canonical, proto.RemoteInboundFinalizeV1Result{Accepted: true, Materialized: true, ReasonCode: "unexpected"}},
		"rejected success":             {canonical, proto.RemoteInboundFinalizeV1Result{Materialized: true, ReasonCode: "unexpected"}},
		"empty rejection reason":       {canonical, proto.RemoteInboundFinalizeV1Result{}},
		"canonical reported noop":      {canonical, proto.RemoteInboundFinalizeV1Result{Accepted: true, NoopFinalized: true}},
		"noop reported materialized":   {noop, proto.RemoteInboundFinalizeV1Result{Accepted: true, Materialized: true}},
	} {
		t.Run(name, func(t *testing.T) {
			if validInboundFinalizeResult(tc.evidence, tc.result) {
				t.Fatalf("invalid result accepted: %+v", tc.result)
			}
		})
	}
}

// pipeTransport pairs two io.Pipes into a single ReadWriter on each
// side so the proxy and host can talk over an in-memory full-duplex
// channel. Closes idempotently on both ends.
type pipeTransport struct {
	r    *io.PipeReader
	w    *io.PipeWriter
	once sync.Once
}

func TestRemoteProxyInboundV2RequiresAndReturnsTerminalAck(t *testing.T) {
	proxySide, pluginSide := newPipePair()
	send := make(chan struct{})
	gotAck := make(chan proto.RemoteInboundAckV2, 1)
	go func() {
		fr, fw := proto.NewFrameReader(pluginSide), proto.NewFrameWriter(pluginSide)
		frame, _ := fr.Read()
		var initReq proto.Request
		_ = json.Unmarshal(frame, &initReq)
		result, _ := json.Marshal(proto.InitializeResult{PluginName: "stub", PluginVersion: "1", ABIVersion: proto.ABIVersion})
		response, _ := json.Marshal(proto.Response{JSONRPC: "2.0", ID: initReq.ID, Result: result})
		_ = fw.Write(response)
		<-send
		params, _ := json.Marshal(proto.RemoteInboundDeliveryV2{DeliveryID: "delivery-1", Cursor: "cursor-1", Events: []proto.RemoteEvent{{EventID: "event-1", Bytes: json.RawMessage(`{}`)}}})
		request, _ := json.Marshal(proto.Request{JSONRPC: "2.0", ID: json.RawMessage(`-1`), Method: proto.MethodRemoteInboundDeliveryV2, Params: params})
		_ = fw.Write(request)
		ackFrame, _ := fr.Read()
		var ackResponse proto.Response
		_ = json.Unmarshal(ackFrame, &ackResponse)
		var ack proto.RemoteInboundAckV2
		_ = json.Unmarshal(ackResponse.Result, &ack)
		gotAck <- ack
	}()
	rp, err := OpenRemote(context.Background(), proxySide, "dev", "v1")
	if err != nil {
		t.Fatal(err)
	}
	rp.OnInboundV2(func(delivery proto.RemoteInboundDeliveryV2) proto.RemoteInboundAckV2 {
		return proto.RemoteInboundAckV2{DeliveryID: delivery.DeliveryID, NextCursor: delivery.Cursor, Outcomes: []proto.RemoteInboundEventOutcomeV2{{Index: 0, Disposition: "accepted", ReasonCode: "durable"}}}
	})
	close(send)
	select {
	case ack := <-gotAck:
		if ack.DeliveryID != "delivery-1" || ack.NextCursor != "cursor-1" || len(ack.Outcomes) != 1 || ack.Outcomes[0].Disposition != "accepted" {
			t.Fatalf("unexpected ack: %+v", ack)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inbound v2 acknowledgement timed out")
	}
	_ = proxySide.Close()
}

func TestRemoteProxyInboundFinalizeReturnsHandlerResult(t *testing.T) {
	proxySide, pluginSide := newPipePair()
	send := make(chan struct{})
	gotResult := make(chan proto.RemoteInboundFinalizeV1Result, 1)
	go func() {
		fr, fw := proto.NewFrameReader(pluginSide), proto.NewFrameWriter(pluginSide)
		frame, _ := fr.Read()
		var initReq proto.Request
		_ = json.Unmarshal(frame, &initReq)
		result, _ := json.Marshal(proto.InitializeResult{PluginName: "stub", PluginVersion: "1", ABIVersion: proto.ABIVersion})
		response, _ := json.Marshal(proto.Response{JSONRPC: "2.0", ID: initReq.ID, Result: result})
		_ = fw.Write(response)
		<-send
		params, _ := json.Marshal(proto.RemoteInboundFinalizeV1Params{Evidence: proxyFinalizeEvidence()})
		request, _ := json.Marshal(proto.Request{JSONRPC: "2.0", ID: json.RawMessage(`-2`), Method: proto.MethodRemoteInboundFinalizeV1, Params: params})
		_ = fw.Write(request)
		resultFrame, _ := fr.Read()
		var resultResponse proto.Response
		_ = json.Unmarshal(resultFrame, &resultResponse)
		var finalizeResult proto.RemoteInboundFinalizeV1Result
		_ = json.Unmarshal(resultResponse.Result, &finalizeResult)
		gotResult <- finalizeResult
	}()
	rp, err := OpenRemote(context.Background(), proxySide, "dev", "v1")
	if err != nil {
		t.Fatal(err)
	}
	rp.OnInboundFinalizeV1(func(params proto.RemoteInboundFinalizeV1Params) proto.RemoteInboundFinalizeV1Result {
		if params.Evidence.DeliveryID != "delivery-1" {
			t.Fatalf("unexpected evidence: %+v", params.Evidence)
		}
		return proto.RemoteInboundFinalizeV1Result{Accepted: true, Materialized: true}
	})
	close(send)
	select {
	case result := <-gotResult:
		if !result.Accepted || !result.Materialized || result.AlreadyFinalized {
			t.Fatalf("unexpected finalize result: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inbound finalize result timed out")
	}
	_ = proxySide.Close()
}

func (p *pipeTransport) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeTransport) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeTransport) Close() error {
	p.once.Do(func() {
		_ = p.w.Close()
		_ = p.r.Close()
	})
	return nil
}

func newPipePair() (*pipeTransport, *pipeTransport) {
	// proxy writes -> host reads; host writes -> proxy reads
	hostR, proxyW := io.Pipe()
	proxyR, hostW := io.Pipe()
	return &pipeTransport{r: proxyR, w: proxyW}, &pipeTransport{r: hostR, w: hostW}
}

// remoteStub is a minimal RemoteHandler used by these tests. Records
// inbound calls; keeps track of the Notifier so the test can push
// pretend "inbound events" through it.
type remoteStub struct {
	host.BaseRemoteHandler
	mu       sync.Mutex
	calls    []string
	notifier host.Notifier
}

func (s *remoteStub) record(m string) {
	s.mu.Lock()
	s.calls = append(s.calls, m)
	s.mu.Unlock()
}

func (s *remoteStub) AttachNotifier(n host.Notifier) { s.notifier = n }

func (s *remoteStub) Initialize(_ context.Context, _ proto.InitializeParams) (proto.InitializeResult, error) {
	s.record("initialize")
	return proto.InitializeResult{
		PluginName:    "stub",
		PluginVersion: "0.0.0",
		ABIVersion:    proto.ABIVersion,
	}, nil
}

func (s *remoteStub) Publish(_ context.Context, params proto.RemotePublishParams) (proto.RemotePublishResult, error) {
	s.record("publish")
	out := proto.RemotePublishResult{}
	for _, e := range params.Events {
		out.Outcomes = append(out.Outcomes, proto.RemotePublishOutcome{EventID: e.EventID, Accepted: true})
	}
	return out, nil
}

func (s *remoteStub) Fetch(_ context.Context, _ proto.RemoteFetchParams) (proto.RemoteFetchResult, error) {
	s.record("fetch")
	return proto.RemoteFetchResult{Events: []proto.RemoteEvent{{EventID: "from-fetch"}}, NextCursor: ""}, nil
}

func (s *remoteStub) NegotiateSyncV1(_ context.Context, _ proto.RemoteNegotiateSyncV1Params) (proto.RemoteNegotiateSyncV1Result, error) {
	s.record("negotiate_sync_v1")
	return proto.RemoteNegotiateSyncV1Result{SelectedProtocol: 1, Mode: proto.RemoteSyncModeShadow, StreamID: "stream-1", StreamEpoch: "epoch-1"}, nil
}

func (s *remoteStub) ResumeCursorV1(_ context.Context, p proto.RemoteResumeCursorV1Params) (proto.RemoteResumeCursorV1Result, error) {
	s.record("resume_cursor_v1")
	return proto.RemoteResumeCursorV1Result{Accepted: true, StreamID: p.StreamID, StreamEpoch: p.StreamEpoch, CursorPresent: p.CursorPresent, Cursor: p.Cursor, CursorDigest: p.CursorDigest, Position: p.Position}, nil
}

func (s *remoteStub) ResumeCursorsV1(_ context.Context, p proto.RemoteResumeCursorsV1Params) (proto.RemoteResumeCursorsV1Result, error) {
	s.record("resume_cursors_v1")
	result := proto.RemoteResumeCursorsV1Result{Accepted: true, Cursors: make([]proto.RemoteResumeCursorV1Result, len(p.Cursors))}
	for index, cursor := range p.Cursors {
		result.Cursors[index] = proto.RemoteResumeCursorV1Result{Accepted: true, StreamID: cursor.StreamID, StreamEpoch: cursor.StreamEpoch, CursorPresent: cursor.CursorPresent, Cursor: cursor.Cursor, CursorDigest: cursor.CursorDigest, Position: cursor.Position, PendingFinalizeEvidence: cursor.PendingFinalizeEvidence}
	}
	return result, nil
}

func (s *remoteStub) FetchV2(_ context.Context, p proto.RemoteFetchV2Params) (proto.RemoteFetchV2Result, error) {
	s.record("fetch_v2")
	digest := sha256.Sum256([]byte("cursor-2"))
	return proto.RemoteFetchV2Result{Events: []proto.RemoteEvent{{EventID: "from-fetch-v2"}}, PredecessorCursor: p.Cursor, PredecessorPosition: p.Position, NextCursor: "cursor-2", NextCursorDigest: hex.EncodeToString(digest[:]), NextPosition: p.Position + 1, StreamEpoch: p.StreamEpoch}, nil
}

func (s *remoteStub) FetchParentV1(_ context.Context, p proto.RemoteFetchParentV1Params) (proto.RemoteFetchParentV1Result, error) {
	s.record("fetch_parent_v1")
	digest := sha256.Sum256([]byte("parent-cursor"))
	return proto.RemoteFetchParentV1Result{Found: true, Record: &proto.RemoteRecoveryEventV1{Event: proto.RemoteEvent{EventID: "parent-1", EventHash: p.EventHash}, PredecessorCursor: "parent-predecessor", PredecessorPosition: 1, Cursor: "parent-cursor", CursorDigest: hex.EncodeToString(digest[:]), Position: 2}}, nil
}

func (s *remoteStub) AckV2(_ context.Context, p proto.RemoteAckV2Params) (proto.RemoteAckV2Result, error) {
	s.record("ack_v2")
	return proto.RemoteAckV2Result{Accepted: true, AcknowledgedCursor: p.Cursor, AcknowledgedPosition: p.Position}, nil
}

func (s *remoteStub) RequestCheckpointV1(_ context.Context, _ proto.RemoteRequestCheckpointV1Params) (proto.RemoteRequestCheckpointV1Result, error) {
	s.record("request_checkpoint_v1")
	return proto.RemoteRequestCheckpointV1Result{Requested: true, RequestID: "request-1"}, nil
}

func (s *remoteStub) Enumerate(_ context.Context, _ proto.RemoteEnumerateParams) (proto.RemoteEnumerateResult, error) {
	s.record("enumerate")
	return proto.RemoteEnumerateResult{}, nil
}

func (s *remoteStub) Subscribe(_ context.Context, _ proto.RemoteSubscribeParams) error {
	s.record("subscribe")
	return nil
}

func (s *remoteStub) Unsubscribe(_ context.Context, _ proto.RemoteUnsubscribeParams) error {
	s.record("unsubscribe")
	return nil
}

func (s *remoteStub) Status(_ context.Context) (proto.RemoteStatusResult, error) {
	s.record("status")
	return proto.RemoteStatusResult{ConnState: "connected"}, nil
}

func (s *remoteStub) Shutdown(_ context.Context, _ proto.ShutdownParams) (proto.ShutdownResult, error) {
	s.record("shutdown")
	return proto.ShutdownResult{}, nil
}

func TestRemoteProxy_FullRoundTrip(t *testing.T) {
	proxySide, hostSide := newPipePair()
	stub := &remoteStub{}

	// Start the host (plugin) in a goroutine.
	hostDone := make(chan error, 1)
	go func() {
		hostDone <- host.ServeRemote(context.Background(), stub, hostSide, hostSide)
	}()

	rp, err := OpenRemote(context.Background(), proxySide, "dev-test", "v0.0.0-test")
	if err != nil {
		t.Fatalf("OpenRemote: %v", err)
	}
	if rp.Name() != "stub" {
		t.Errorf("Name = %q, want stub", rp.Name())
	}

	// Wire inbound callback BEFORE subscribe.
	var inboundMu sync.Mutex
	var inboundEvents []proto.RemoteEvent
	rp.OnInbound(func(events []proto.RemoteEvent) {
		inboundMu.Lock()
		inboundEvents = append(inboundEvents, events...)
		inboundMu.Unlock()
	})

	// Publish round-trip
	pubResult, err := rp.Publish(context.Background(), []proto.RemoteEvent{{EventID: "e1"}, {EventID: "e2"}})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(pubResult.Outcomes) != 2 || !pubResult.Outcomes[0].Accepted {
		t.Errorf("Publish result = %+v", pubResult)
	}

	negotiated, err := rp.NegotiateSyncV1(context.Background(), proto.RemoteNegotiateSyncV1Params{ProtocolMin: 1, ProtocolMax: 1, DaemonCapabilities: []string{proto.CapabilityDurableDeltaSyncV1}})
	if err != nil || negotiated.Mode != proto.RemoteSyncModeShadow || negotiated.StreamEpoch != "epoch-1" {
		t.Fatalf("NegotiateSyncV1 = %+v, err=%v", negotiated, err)
	}
	cursorOneDigest := sha256.Sum256([]byte("cursor-1"))
	resumed, err := rp.ResumeCursorV1(context.Background(), proto.RemoteResumeCursorV1Params{Authoritative: true, StreamID: "stream-1", StreamEpoch: "epoch-1", CursorPresent: true, Cursor: "cursor-1", CursorDigest: hex.EncodeToString(cursorOneDigest[:]), Position: 1})
	if err != nil || !resumed.Accepted || resumed.Cursor != "cursor-1" || resumed.Position != 1 {
		t.Fatalf("ResumeCursorV1 = %+v, err=%v", resumed, err)
	}
	pluralResumed, err := rp.ResumeCursorsV1(context.Background(), proto.RemoteResumeCursorsV1Params{Cursors: []proto.RemoteResumeCursorV1Params{
		{Authoritative: true, StreamID: "stream-1", StreamEpoch: "epoch-1", CursorPresent: true, Cursor: "cursor-1", CursorDigest: hex.EncodeToString(cursorOneDigest[:]), Position: 1},
		{Authoritative: true, StreamID: "stream-a", StreamEpoch: "epoch-a"},
	}})
	if err != nil || !pluralResumed.Accepted || len(pluralResumed.Cursors) != 2 || pluralResumed.Cursors[1].StreamID != "stream-a" {
		t.Fatalf("ResumeCursorsV1 = %+v, err=%v", pluralResumed, err)
	}
	fetched, err := rp.FetchV2(context.Background(), proto.RemoteFetchV2Params{StreamID: "stream-1", StreamEpoch: "epoch-1", Cursor: "cursor-1", CursorDigest: hex.EncodeToString(cursorOneDigest[:]), Position: 1, LimitEvents: 1})
	if err != nil || len(fetched.Events) != 1 || fetched.Events[0].EventID != "from-fetch-v2" || fetched.PredecessorCursor != "cursor-1" || fetched.PredecessorPosition != 1 || fetched.NextCursor != "cursor-2" || fetched.NextPosition != 2 {
		t.Fatalf("FetchV2 = %+v, err=%v", fetched, err)
	}
	parentHash := strings.Repeat("c", 64)
	parent, err := rp.FetchParentV1(context.Background(), proto.RemoteFetchParentV1Params{StreamID: "stream-1", StreamEpoch: "epoch-1", NamespaceID: "ns", ArtifactID: "artifact", EventHash: parentHash})
	if err != nil || !parent.Found || parent.Record == nil || parent.Record.Event.EventHash != parentHash || parent.Record.Position != 2 {
		t.Fatalf("FetchParentV1 = %+v, err=%v", parent, err)
	}
	acked, err := rp.AckV2(context.Background(), proto.RemoteAckV2Params{StreamID: "stream-1", StreamEpoch: "epoch-1", Cursor: "cursor-2", CursorDigest: fetched.NextCursorDigest, Position: fetched.NextPosition})
	if err != nil || !acked.Accepted || acked.AcknowledgedCursor != "cursor-2" || acked.AcknowledgedPosition != 2 {
		t.Fatalf("AckV2 = %+v, err=%v", acked, err)
	}
	requested, err := rp.RequestCheckpointV1(context.Background(), proto.RemoteRequestCheckpointV1Params{StreamID: "stream-1", StreamEpoch: "epoch-1", NamespaceID: "ns", ArtifactID: "artifact", Reason: "missing-parent"})
	if err != nil || !requested.Requested || requested.RequestID != "request-1" {
		t.Fatalf("RequestCheckpointV1 = %+v, err=%v", requested, err)
	}

	// Subscribe + Status
	if err := rp.Subscribe(context.Background(), "ns-1"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	status, err := rp.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.ConnState != "connected" {
		t.Errorf("ConnState = %q", status.ConnState)
	}

	// Push inbound notification from the host side.
	// The stub's notifier must be wired; AttachNotifier is invoked
	// synchronously in ServeRemote's setup.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stub.mu.Lock()
		n := stub.notifier
		stub.mu.Unlock()
		if n != nil {
			if err := n.Inbound([]proto.RemoteEvent{{EventID: "from-plugin"}}); err != nil {
				t.Fatalf("Inbound: %v", err)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Allow time for the proxy's read pump to dispatch the notification.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		inboundMu.Lock()
		n := len(inboundEvents)
		inboundMu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	inboundMu.Lock()
	got := append([]proto.RemoteEvent{}, inboundEvents...)
	inboundMu.Unlock()
	if len(got) != 1 || got[0].EventID != "from-plugin" {
		t.Errorf("inboundEvents = %+v", got)
	}

	checkpointCh := make(chan proto.RemoteCheckpointNeededV1Notification, 1)
	rp.OnCheckpointNeededV1(func(n proto.RemoteCheckpointNeededV1Notification) {
		checkpointCh <- n
	})
	durableNotifier, ok := stub.notifier.(host.DurableDeltaSyncNotifier)
	if !ok {
		t.Fatal("host notifier does not provide the additive durable-sync extension")
	}
	if err := durableNotifier.CheckpointNeeded(proto.RemoteCheckpointNeededV1Notification{RequestID: "request-2", StreamID: "stream-1", StreamEpoch: "epoch-1", NamespaceID: "ns", ArtifactID: "artifact", Reason: "new-recipient"}); err != nil {
		t.Fatalf("CheckpointNeeded: %v", err)
	}
	select {
	case notification := <-checkpointCh:
		if notification.RequestID != "request-2" || notification.ArtifactID != "artifact" {
			t.Fatalf("checkpoint notification = %+v", notification)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("checkpoint notification was not dispatched")
	}

	// Shutdown cleanly.
	if err := rp.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}

	select {
	case err := <-hostDone:
		if err != nil {
			t.Errorf("host ServeRemote returned: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("host ServeRemote did not return within 2s")
	}
}

func TestRemoteProxy_CtxCancelInterruptsCall(t *testing.T) {
	proxySide, hostSide := newPipePair()

	// Plug a deliberately stuck host: ServeRemote reads frames but
	// never replies. Implementation: ignore the request after reading
	// it by just blocking on hostSide reads forever.
	go func() {
		buf := make([]byte, 4096)
		// Read whatever comes in, never write.
		for {
			if _, err := hostSide.Read(buf); err != nil {
				return
			}
		}
	}()

	// We can't open via OpenRemote (which requires initialize); build
	// a RemoteProxy by hand to avoid that handshake.
	p := &RemoteProxy{
		fr:         proto.NewFrameReader(proxySide),
		fw:         proto.NewFrameWriter(proxySide),
		pending:    map[int64]chan proto.Response{},
		readDoneCh: make(chan struct{}),
	}
	go p.readPump()
	defer func() {
		_ = proxySide.Close()
		<-p.readDoneCh
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := p.call(ctx, proto.MethodRemoteStatus, struct{}{}, nil)
	if err == nil {
		t.Error("expected ctx-cancel error, got nil")
	}
}
