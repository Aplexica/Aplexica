package host

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/syncrules"
)

// stubRemoteHandler is a programmable RemoteHandler whose method
// calls record themselves on the embedded slice. AttachNotifier is
// captured for inbound-push tests.
type stubRemoteHandler struct {
	BaseRemoteHandler
	mu             sync.Mutex
	calls          []string
	notifier       Notifier
	publishOutcome proto.RemotePublishResult
}

func (s *stubRemoteHandler) AttachNotifier(n Notifier) { s.notifier = n }

func (s *stubRemoteHandler) record(m string) {
	s.mu.Lock()
	s.calls = append(s.calls, m)
	s.mu.Unlock()
}

func (s *stubRemoteHandler) Initialize(_ context.Context, _ proto.InitializeParams) (proto.InitializeResult, error) {
	s.record("initialize")
	return proto.InitializeResult{
		PluginName:    "stub-remote",
		PluginVersion: "v0.0.0-test",
		ABIVersion:    proto.ABIVersion,
	}, nil
}

func (s *stubRemoteHandler) Publish(_ context.Context, params proto.RemotePublishParams) (proto.RemotePublishResult, error) {
	s.record("publish")
	if len(s.publishOutcome.Outcomes) > 0 {
		return s.publishOutcome, nil
	}
	out := proto.RemotePublishResult{}
	for _, e := range params.Events {
		out.Outcomes = append(out.Outcomes, proto.RemotePublishOutcome{EventID: e.EventID, Accepted: true})
	}
	return out, nil
}

func (s *stubRemoteHandler) Fetch(_ context.Context, _ proto.RemoteFetchParams) (proto.RemoteFetchResult, error) {
	s.record("fetch")
	return proto.RemoteFetchResult{}, nil
}

func (s *stubRemoteHandler) Enumerate(_ context.Context, _ proto.RemoteEnumerateParams) (proto.RemoteEnumerateResult, error) {
	s.record("enumerate")
	return proto.RemoteEnumerateResult{}, nil
}

func (s *stubRemoteHandler) Subscribe(_ context.Context, _ proto.RemoteSubscribeParams) error {
	s.record("subscribe")
	return nil
}

func (s *stubRemoteHandler) Unsubscribe(_ context.Context, _ proto.RemoteUnsubscribeParams) error {
	s.record("unsubscribe")
	return nil
}

func (s *stubRemoteHandler) Status(_ context.Context) (proto.RemoteStatusResult, error) {
	s.record("status")
	return proto.RemoteStatusResult{ConnState: "disconnected"}, nil
}

func (s *stubRemoteHandler) Shutdown(_ context.Context, _ proto.ShutdownParams) (proto.ShutdownResult, error) {
	s.record("shutdown")
	return proto.ShutdownResult{}, nil
}

type durableStubRemoteHandler struct {
	stubRemoteHandler
	negotiated proto.RemoteNegotiateSyncV1Params
	resumed    proto.RemoteResumeCursorV1Params
	resumedAll proto.RemoteResumeCursorsV1Params
	fetched    proto.RemoteFetchV2Params
	parent     proto.RemoteFetchParentV1Params
	acked      proto.RemoteAckV2Params
	checkpoint proto.RemoteRequestCheckpointV1Params
}

func (s *durableStubRemoteHandler) NegotiateSyncV1(_ context.Context, p proto.RemoteNegotiateSyncV1Params) (proto.RemoteNegotiateSyncV1Result, error) {
	s.record("negotiate_sync_v1")
	s.negotiated = p
	return proto.RemoteNegotiateSyncV1Result{SelectedProtocol: 1, Mode: proto.RemoteSyncModeShadow, StreamID: "stream-1", StreamEpoch: "epoch-1"}, nil
}

func (s *durableStubRemoteHandler) ResumeCursorV1(_ context.Context, p proto.RemoteResumeCursorV1Params) (proto.RemoteResumeCursorV1Result, error) {
	s.record("resume_cursor_v1")
	s.resumed = p
	return proto.RemoteResumeCursorV1Result{Accepted: true, StreamID: p.StreamID, StreamEpoch: p.StreamEpoch, CursorPresent: p.CursorPresent, Cursor: p.Cursor, CursorDigest: p.CursorDigest, Position: p.Position}, nil
}

func (s *durableStubRemoteHandler) ResumeCursorsV1(_ context.Context, p proto.RemoteResumeCursorsV1Params) (proto.RemoteResumeCursorsV1Result, error) {
	s.record("resume_cursors_v1")
	s.resumedAll = p
	result := proto.RemoteResumeCursorsV1Result{Accepted: true, Cursors: make([]proto.RemoteResumeCursorV1Result, len(p.Cursors))}
	for index, cursor := range p.Cursors {
		result.Cursors[index] = proto.RemoteResumeCursorV1Result{Accepted: true, StreamID: cursor.StreamID, StreamEpoch: cursor.StreamEpoch, CursorPresent: cursor.CursorPresent, Cursor: cursor.Cursor, CursorDigest: cursor.CursorDigest, Position: cursor.Position, PendingFinalizeEvidence: cursor.PendingFinalizeEvidence}
	}
	return result, nil
}

func (s *durableStubRemoteHandler) FetchV2(_ context.Context, p proto.RemoteFetchV2Params) (proto.RemoteFetchV2Result, error) {
	s.record("fetch_v2")
	s.fetched = p
	return proto.RemoteFetchV2Result{Events: []proto.RemoteEvent{{EventID: "event-2"}}, PredecessorCursor: p.Cursor, PredecessorPosition: p.Position, NextCursor: "cursor-2", NextCursorDigest: strings.Repeat("b", 64), NextPosition: p.Position + 1, StreamEpoch: p.StreamEpoch}, nil
}

func (s *durableStubRemoteHandler) FetchParentV1(_ context.Context, p proto.RemoteFetchParentV1Params) (proto.RemoteFetchParentV1Result, error) {
	s.record("fetch_parent_v1")
	s.parent = p
	return proto.RemoteFetchParentV1Result{Found: true, Record: &proto.RemoteRecoveryEventV1{Event: proto.RemoteEvent{EventID: "parent-1", EventHash: p.EventHash}, PredecessorCursor: "cursor-0", Cursor: "cursor-1", CursorDigest: strings.Repeat("a", 64), Position: 1}}, nil
}

func (s *durableStubRemoteHandler) AckV2(_ context.Context, p proto.RemoteAckV2Params) (proto.RemoteAckV2Result, error) {
	s.record("ack_v2")
	s.acked = p
	return proto.RemoteAckV2Result{Accepted: true, AcknowledgedCursor: p.Cursor, AcknowledgedPosition: p.Position}, nil
}

func (s *durableStubRemoteHandler) RequestCheckpointV1(_ context.Context, p proto.RemoteRequestCheckpointV1Params) (proto.RemoteRequestCheckpointV1Result, error) {
	s.record("request_checkpoint_v1")
	s.checkpoint = p
	return proto.RemoteRequestCheckpointV1Result{Requested: true, RequestID: "request-1"}, nil
}

// frame encodes a JSON-RPC request as a framed frame the proto reader expects.
func frame(t *testing.T, req any) []byte {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var buf bytes.Buffer
	fw := proto.NewFrameWriter(&buf)
	if err := fw.Write(body); err != nil {
		t.Fatalf("frame write: %v", err)
	}
	return buf.Bytes()
}

func TestServeRemote_DispatchesEachMethod(t *testing.T) {
	stub := &stubRemoteHandler{}

	var in bytes.Buffer
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(1), Method: proto.MethodInitialize, Params: paramsJSON(t, proto.InitializeParams{ABIVersion: proto.ABIVersion})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(2), Method: proto.MethodRemotePublish, Params: paramsJSON(t, proto.RemotePublishParams{Events: []proto.RemoteEvent{{EventID: "e1"}}})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(3), Method: proto.MethodRemoteFetch, Params: paramsJSON(t, proto.RemoteFetchParams{NamespaceID: "ns"})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(4), Method: proto.MethodRemoteEnumerate, Params: paramsJSON(t, proto.RemoteEnumerateParams{})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(5), Method: proto.MethodRemoteSubscribe, Params: paramsJSON(t, proto.RemoteSubscribeParams{NamespaceID: "ns"})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(6), Method: proto.MethodRemoteUnsubscribe, Params: paramsJSON(t, proto.RemoteUnsubscribeParams{NamespaceID: "ns"})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(7), Method: proto.MethodRemoteStatus, Params: paramsJSON(t, struct{}{})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(8), Method: proto.MethodShutdown, Params: paramsJSON(t, proto.ShutdownParams{})}))

	var out bytes.Buffer
	if err := ServeRemote(context.Background(), stub, &in, &out); err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}

	want := []string{"initialize", "publish", "fetch", "enumerate", "subscribe", "unsubscribe", "status", "shutdown"}
	if !equalSlice(stub.calls, want) {
		t.Errorf("calls = %v, want %v", stub.calls, want)
	}
}

func TestServeRemote_DispatchesDurableDeltaExtension(t *testing.T) {
	stub := &durableStubRemoteHandler{}

	var in bytes.Buffer
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(1), Method: proto.MethodRemoteNegotiateSyncV1, Params: paramsJSON(t, proto.RemoteNegotiateSyncV1Params{ProtocolMin: 1, ProtocolMax: 1, DaemonCapabilities: []string{proto.CapabilityDurableDeltaSyncV1}})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(2), Method: proto.MethodRemoteResumeCursorV1, Params: paramsJSON(t, proto.RemoteResumeCursorV1Params{Authoritative: true, StreamID: "stream-1", StreamEpoch: "epoch-1", CursorPresent: true, Cursor: "cursor-1", CursorDigest: strings.Repeat("a", 64), Position: 1})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(3), Method: proto.MethodRemoteResumeCursorsV1, Params: paramsJSON(t, proto.RemoteResumeCursorsV1Params{Cursors: []proto.RemoteResumeCursorV1Params{{Authoritative: true, StreamID: "stream-1", StreamEpoch: "epoch-1"}, {Authoritative: true, StreamID: "stream-a", StreamEpoch: "epoch-a"}}})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(4), Method: proto.MethodRemoteFetchV2, Params: paramsJSON(t, proto.RemoteFetchV2Params{StreamID: "stream-1", StreamEpoch: "epoch-1", Cursor: "cursor-1", CursorDigest: strings.Repeat("a", 64), Position: 1, LimitEvents: 1})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(5), Method: proto.MethodRemoteFetchParentV1, Params: paramsJSON(t, proto.RemoteFetchParentV1Params{StreamID: "stream-1", StreamEpoch: "epoch-1", NamespaceID: "ns", ArtifactID: "artifact", EventHash: strings.Repeat("c", 64)})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(6), Method: proto.MethodRemoteAckV2, Params: paramsJSON(t, proto.RemoteAckV2Params{StreamID: "stream-1", StreamEpoch: "epoch-1", Cursor: "cursor-2", CursorDigest: strings.Repeat("b", 64), Position: 2})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(7), Method: proto.MethodRemoteRequestCheckpointV1, Params: paramsJSON(t, proto.RemoteRequestCheckpointV1Params{StreamID: "stream-1", StreamEpoch: "epoch-1", NamespaceID: "ns", ArtifactID: "artifact", Reason: "missing-parent", Cursor: "cursor-2", CursorDigest: strings.Repeat("b", 64), Position: 2, CheckpointGeneration: "checkpoint-generation-1"})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(8), Method: proto.MethodShutdown, Params: paramsJSON(t, proto.ShutdownParams{})}))

	var out bytes.Buffer
	if err := ServeRemote(context.Background(), stub, &in, &out); err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}

	want := []string{"negotiate_sync_v1", "resume_cursor_v1", "resume_cursors_v1", "fetch_v2", "fetch_parent_v1", "ack_v2", "request_checkpoint_v1", "shutdown"}
	if !equalSlice(stub.calls, want) {
		t.Fatalf("calls = %v, want %v", stub.calls, want)
	}
	if stub.negotiated.ProtocolMax != 1 || !stub.resumed.Authoritative || stub.resumed.Cursor != "cursor-1" || len(stub.resumedAll.Cursors) != 2 || stub.resumedAll.Cursors[1].StreamID != "stream-a" || stub.fetched.Cursor != "cursor-1" || stub.fetched.Position != 1 || stub.parent.EventHash != strings.Repeat("c", 64) || stub.acked.Cursor != "cursor-2" || stub.acked.Position != 2 || stub.checkpoint.ArtifactID != "artifact" || stub.checkpoint.CheckpointGeneration != "checkpoint-generation-1" {
		t.Fatalf("durable params were not threaded through: %+v %+v %+v %+v", stub.negotiated, stub.fetched, stub.acked, stub.checkpoint)
	}
	for _, value := range []string{"stream-1", "parent-1", "cursor-2", "request-1"} {
		if !strings.Contains(out.String(), value) {
			t.Errorf("durable response missing %q: %q", value, out.String())
		}
	}
}

func TestServeRemote_DurableDeltaDefaultsAreLegacySafe(t *testing.T) {
	stub := &stubRemoteHandler{}

	var in bytes.Buffer
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(1), Method: proto.MethodRemoteNegotiateSyncV1, Params: paramsJSON(t, proto.RemoteNegotiateSyncV1Params{ProtocolMin: 1, ProtocolMax: 1})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(2), Method: proto.MethodShutdown, Params: paramsJSON(t, proto.ShutdownParams{})}))

	var out bytes.Buffer
	if err := ServeRemote(context.Background(), stub, &in, &out); err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	if strings.Contains(out.String(), "-32601") {
		t.Fatalf("durable method was not recognized: %q", out.String())
	}
	if !strings.Contains(out.String(), ErrDurableDeltaSyncUnsupported.Error()) {
		t.Fatalf("legacy default did not fail safely: %q", out.String())
	}
}

// acctStubHandler overrides RegisterWrapKey/ListAccountDevices so the test
// can prove dispatchRemote routes the account-scoped methods to the handler
// (and threads params/results), not just that they avoid -32601.
type acctStubHandler struct {
	stubRemoteHandler
	gotWrapPub []byte
	devices    []proto.RemoteAccountDevice
}

func (s *acctStubHandler) RegisterWrapKey(_ context.Context, p proto.RemoteRegisterWrapKeyParams) error {
	s.record("register_wrap_key")
	s.gotWrapPub = p.WrapPubKey
	return nil
}

func (s *acctStubHandler) ListAccountDevices(_ context.Context) (proto.RemoteListAccountDevicesResult, error) {
	s.record("list_account_devices")
	return proto.RemoteListAccountDevicesResult{Devices: s.devices}, nil
}

func TestServeRemote_DispatchesAccountWrapKeyMethods(t *testing.T) {
	stub := &acctStubHandler{devices: []proto.RemoteAccountDevice{{DeviceID: "dev-1", PubKey: []byte("0123456789abcdef0123456789abcdef")}}}
	wantPub := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	var in bytes.Buffer
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(1), Method: proto.MethodRemoteRegisterWrapKey, Params: paramsJSON(t, proto.RemoteRegisterWrapKeyParams{WrapPubKey: wantPub})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(2), Method: proto.MethodRemoteListAccountDevices, Params: paramsJSON(t, struct{}{})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(3), Method: proto.MethodShutdown, Params: paramsJSON(t, proto.ShutdownParams{})}))

	var out bytes.Buffer
	if err := ServeRemote(context.Background(), stub, &in, &out); err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}

	// Neither method may fall through to the -32601 default branch.
	if strings.Contains(out.String(), "-32601") {
		t.Fatalf("a recognized account method returned method-not-found: %q", out.String())
	}
	want := []string{"register_wrap_key", "list_account_devices", "shutdown"}
	if !equalSlice(stub.calls, want) {
		t.Fatalf("calls = %v, want %v", stub.calls, want)
	}
	// Params threaded through to the handler.
	if !bytes.Equal(stub.gotWrapPub, wantPub) {
		t.Errorf("RegisterWrapKey got pub %q, want %q", stub.gotWrapPub, wantPub)
	}
	// Result threaded back out: the device id must appear in the response body.
	if !strings.Contains(out.String(), "dev-1") {
		t.Errorf("list_account_devices result missing device id: %q", out.String())
	}
}

// TestServeRemote_BaseAccountMethodsReturnUnsupported asserts a handler that
// embeds BaseRemoteHandler (a BYO transport with no account concept) gets the
// methods dispatched — surfacing a clear "unsupported" application error rather
// than the JSON-RPC -32601 "method not found".
func TestServeRemote_BaseAccountMethodsReturnUnsupported(t *testing.T) {
	stub := &stubRemoteHandler{} // embeds BaseRemoteHandler defaults

	var in bytes.Buffer
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(1), Method: proto.MethodRemoteRegisterWrapKey, Params: paramsJSON(t, proto.RemoteRegisterWrapKeyParams{WrapPubKey: []byte("x")})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(2), Method: proto.MethodRemoteListAccountDevices, Params: paramsJSON(t, struct{}{})}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(3), Method: proto.MethodShutdown, Params: paramsJSON(t, proto.ShutdownParams{})}))

	var out bytes.Buffer
	if err := ServeRemote(context.Background(), stub, &in, &out); err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	// Recognized (dispatched), so NOT -32601 ...
	if strings.Contains(out.String(), "-32601") {
		t.Fatalf("base account method returned -32601 (not dispatched): %q", out.String())
	}
	// ... but the default returns the application-level "unsupported" error.
	if !strings.Contains(out.String(), "not supported") {
		t.Errorf("base account method response lacks an unsupported-transport error: %q", out.String())
	}
}

func TestServeRemote_UnknownMethodReturns32601(t *testing.T) {
	stub := &stubRemoteHandler{}

	var in bytes.Buffer
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(1), Method: "remote.totally-made-up", Params: nil}))
	in.Write(frame(t, proto.Request{JSONRPC: "2.0", ID: rawID(2), Method: proto.MethodShutdown}))

	var out bytes.Buffer
	if err := ServeRemote(context.Background(), stub, &in, &out); err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	// The first response in `out` must be an error with code -32601 ("method not found")
	if !strings.Contains(out.String(), "-32601") {
		t.Errorf("response body lacks -32601 code: %q", out.String())
	}
}

func TestServeRemote_PluginPushesInboundNotification(t *testing.T) {
	// The notifier is attached by ServeRemote's wrapWithNotifier
	// inside the body. We control timing via piped readers/writers
	// so the test can push frames between calls.

	stub := &stubRemoteHandler{}

	stdinR, stdinW := io.Pipe()
	var stdout bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- ServeRemote(context.Background(), stub, stdinR, &stdout)
	}()

	// Frame 1: Initialize. Attachment is synchronous on entry into
	// ServeRemote, so by the time we read the response below the
	// notifier has been wired.
	fw := proto.NewFrameWriter(stdinW)
	initBody, _ := json.Marshal(proto.Request{JSONRPC: "2.0", ID: rawID(1), Method: proto.MethodInitialize, Params: paramsJSON(t, proto.InitializeParams{ABIVersion: proto.ABIVersion})})
	if err := fw.Write(initBody); err != nil {
		t.Fatalf("write init: %v", err)
	}

	// Wait until the stub records the initialize call (i.e. dispatch ran).
	deadlineLoop(t, func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		for _, c := range stub.calls {
			if c == "initialize" {
				return true
			}
		}
		return false
	})

	// Push an Inbound notification through the notifier from "outside"
	// the dispatch (simulating a transport callback firing on a
	// background goroutine).
	if stub.notifier == nil {
		t.Fatal("notifier was never attached")
	}
	if err := stub.notifier.Inbound([]proto.RemoteEvent{{EventID: "e1", NamespaceID: "ns"}}); err != nil {
		t.Fatalf("Inbound: %v", err)
	}

	// Frame 2: Shutdown. Closes the loop.
	shutdownBody, _ := json.Marshal(proto.Request{JSONRPC: "2.0", ID: rawID(2), Method: proto.MethodShutdown})
	if err := fw.Write(shutdownBody); err != nil {
		t.Fatalf("write shutdown: %v", err)
	}
	_ = stdinW.Close()

	if err := <-done; err != nil {
		t.Fatalf("ServeRemote: %v", err)
	}
	if !strings.Contains(stdout.String(), "remote.inbound") {
		t.Errorf("output missing remote.inbound notification: %q", stdout.String())
	}
}

// TestSerializingNotifier_RulesUpdateWritesFrame asserts the host SDK's
// notifier exposes RulesUpdate and serializes it to a remote.rules_update
// frame carrying the ruleset — the plugin-side counterpart of the daemon's
// RemoteProxy.handleNotification consuming remote.rules_update into
// OnRulesUpdate. Mirrors how NamespaceKeyRotated/Inbound are exercised.
func TestSerializingNotifier_RulesUpdateWritesFrame(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	n := &serializingNotifier{fw: proto.NewFrameWriter(&buf), mu: &mu}

	notif := proto.RemoteRulesUpdateNotification{
		ChangeID: "chg-7",
		Rules:    []syncrules.Rule{{Name: "r1"}},
	}
	if err := n.RulesUpdate(notif); err != nil {
		t.Fatalf("RulesUpdate: %v", err)
	}

	// Decode the single framed notification back out and assert method +
	// payload survived the round-trip.
	fr := proto.NewFrameReader(&buf)
	frameBytes, err := fr.Read()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var got struct {
		Method string                              `json:"method"`
		Params proto.RemoteRulesUpdateNotification `json:"params"`
	}
	if err := json.Unmarshal(frameBytes, &got); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	if got.Method != proto.NotificationRemoteRulesUpdate {
		t.Errorf("method = %q, want %q", got.Method, proto.NotificationRemoteRulesUpdate)
	}
	if got.Params.ChangeID != "chg-7" {
		t.Errorf("ChangeID = %q, want %q", got.Params.ChangeID, "chg-7")
	}
	if len(got.Params.Rules) != 1 || got.Params.Rules[0].Name != "r1" {
		t.Errorf("Rules = %+v, want one rule with Name r1", got.Params.Rules)
	}
}

func TestSerializingNotifier_CheckpointNeededWritesFrame(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	n := &serializingNotifier{fw: proto.NewFrameWriter(&buf), mu: &mu}

	notif := proto.RemoteCheckpointNeededV1Notification{
		RequestID:   "request-1",
		StreamID:    "stream-1",
		StreamEpoch: "epoch-1",
		NamespaceID: "ns",
		ArtifactID:  "artifact",
		Reason:      "compaction-floor",
	}
	if err := n.CheckpointNeeded(notif); err != nil {
		t.Fatalf("CheckpointNeeded: %v", err)
	}

	fr := proto.NewFrameReader(&buf)
	frameBytes, err := fr.Read()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var got struct {
		Method string                                     `json:"method"`
		Params proto.RemoteCheckpointNeededV1Notification `json:"params"`
	}
	if err := json.Unmarshal(frameBytes, &got); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	if got.Method != proto.NotificationRemoteCheckpointNeededV1 || got.Params.RequestID != notif.RequestID || got.Params.ArtifactID != notif.ArtifactID {
		t.Fatalf("checkpoint notification mismatch: %+v", got)
	}
}

// deadlineLoop polls fn until it returns true or 2s elapses. It sleeps
// (yielding to the scheduler) between checks rather than busy-spinning — a
// tight CPU loop never yields, so on a constrained runner (Windows CI,
// GOMAXPROCS=1, race detector) the dispatcher goroutine could starve and the
// condition would never become true. time.Sleep lets it run.
func deadlineLoop(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("deadline elapsed waiting for condition")
}

// ---- helpers --------------------------------------------------------------

func rawID(i int) json.RawMessage {
	body, _ := json.Marshal(i)
	return body
}

func paramsJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("paramsJSON: %v", err)
	}
	return body
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ensure io.Reader interface is satisfied at compile time
var _ io.Reader = &bytes.Buffer{}
