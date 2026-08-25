package proxy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/host"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/stretchr/testify/require"
)

type observationProxyStub struct {
	remoteStub
	observed chan proto.RemoteSyncObservationV1Params
	accepted bool
}

func (s *observationProxyStub) ObserveSyncV1(_ context.Context, params proto.RemoteSyncObservationV1Params) (proto.RemoteSyncObservationV1Result, error) {
	s.observed <- params
	return proto.RemoteSyncObservationV1Result{Accepted: s.accepted}, nil
}

func proxyObservationParams(t *testing.T) proto.RemoteSyncObservationV1Params {
	t.Helper()
	key := make([]byte, 32)
	key[0] = 1
	params, err := proto.NewRemoteSyncObservationV1(
		key, proto.RemoteSyncMetricDuplicateDelivery, 1, proto.RemoteSyncObservationUnitCount, "delivery-1",
	)
	require.NoError(t, err)
	return params
}

func TestRemoteProxyObserveSyncV1ExactRoundTrip(t *testing.T) {
	proxySide, hostSide := newPipePair()
	stub := &observationProxyStub{observed: make(chan proto.RemoteSyncObservationV1Params, 1), accepted: true}
	hostDone := make(chan error, 1)
	go func() { hostDone <- host.ServeRemote(context.Background(), stub, hostSide, hostSide) }()

	rp, err := OpenRemote(context.Background(), proxySide, "dev", "v1")
	require.NoError(t, err)
	params := proxyObservationParams(t)
	result, err := rp.ObserveSyncV1(context.Background(), params)
	require.NoError(t, err)
	require.True(t, result.Accepted)
	require.Equal(t, params, <-stub.observed)

	invalid := params
	invalid.Unit = proto.RemoteSyncObservationUnitSeconds
	_, err = rp.ObserveSyncV1(context.Background(), invalid)
	require.ErrorIs(t, err, proto.ErrRemoteSyncObservationInvalid)
	require.NoError(t, proxySide.Close())
}

func TestRemoteProxyObserveSyncV1RejectsMalformedResult(t *testing.T) {
	proxySide, pluginSide := newPipePair()
	go func() {
		fr, fw := proto.NewFrameReader(pluginSide), proto.NewFrameWriter(pluginSide)
		frame, _ := fr.Read()
		var initialize proto.Request
		_ = json.Unmarshal(frame, &initialize)
		result, _ := json.Marshal(proto.InitializeResult{PluginName: "stub", PluginVersion: "1", ABIVersion: proto.ABIVersion})
		response, _ := json.Marshal(proto.Response{JSONRPC: "2.0", ID: initialize.ID, Result: result})
		_ = fw.Write(response)
		frame, _ = fr.Read()
		var request proto.Request
		_ = json.Unmarshal(frame, &request)
		malformed, _ := json.Marshal(proto.Response{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{"accepted":true,"reason":"forbidden"}`)})
		_ = fw.Write(malformed)
	}()
	rp, err := OpenRemote(context.Background(), proxySide, "dev", "v1")
	require.NoError(t, err)
	_, err = rp.ObserveSyncV1(context.Background(), proxyObservationParams(t))
	require.ErrorIs(t, err, proto.ErrRemoteSyncObservationInvalid)
	require.NoError(t, proxySide.Close())
}

func TestRemoteProxyPostResponseHooksRunOnlyAfterReverseResponsesAreWritten(t *testing.T) {
	for _, test := range []struct {
		name     string
		method   string
		params   func(t *testing.T) json.RawMessage
		register func(*RemoteProxy, <-chan struct{}, chan<- struct{}, chan<- struct{})
	}{
		{
			name:   "inbound v2",
			method: proto.MethodRemoteInboundDeliveryV2,
			params: func(t *testing.T) json.RawMessage {
				raw, err := json.Marshal(proto.RemoteInboundDeliveryV2{DeliveryID: "delivery-1", Cursor: "cursor-1", Events: []proto.RemoteEvent{{EventID: "event-1", Bytes: json.RawMessage(`{}`)}}})
				require.NoError(t, err)
				return raw
			},
			register: func(rp *RemoteProxy, release <-chan struct{}, entered, after chan<- struct{}) {
				rp.OnInboundV2(func(delivery proto.RemoteInboundDeliveryV2) proto.RemoteInboundAckV2 {
					entered <- struct{}{}
					<-release
					return proto.RemoteInboundAckV2{DeliveryID: delivery.DeliveryID, Outcomes: []proto.RemoteInboundEventOutcomeV2{{Index: 0, Disposition: "retryable", ReasonCode: "test"}}}
				})
				rp.OnInboundV2ResponseWritten(func() { after <- struct{}{} })
			},
		},
		{
			name:   "inbound finalize",
			method: proto.MethodRemoteInboundFinalizeV1,
			params: func(t *testing.T) json.RawMessage {
				raw, err := json.Marshal(proto.RemoteInboundFinalizeV1Params{Evidence: proxyFinalizeEvidence()})
				require.NoError(t, err)
				return raw
			},
			register: func(rp *RemoteProxy, release <-chan struct{}, entered, after chan<- struct{}) {
				rp.OnInboundFinalizeV1(func(proto.RemoteInboundFinalizeV1Params) proto.RemoteInboundFinalizeV1Result {
					entered <- struct{}{}
					<-release
					return proto.RemoteInboundFinalizeV1Result{Accepted: true, Materialized: true}
				})
				rp.OnInboundFinalizeV1ResponseWritten(func() { after <- struct{}{} })
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			proxySide, pluginSide := newPipePair()
			send := make(chan struct{})
			responseRead := make(chan struct{})
			go func() {
				fr, fw := proto.NewFrameReader(pluginSide), proto.NewFrameWriter(pluginSide)
				frame, _ := fr.Read()
				var initialize proto.Request
				_ = json.Unmarshal(frame, &initialize)
				result, _ := json.Marshal(proto.InitializeResult{PluginName: "stub", PluginVersion: "1", ABIVersion: proto.ABIVersion})
				response, _ := json.Marshal(proto.Response{JSONRPC: "2.0", ID: initialize.ID, Result: result})
				_ = fw.Write(response)
				<-send
				request, _ := json.Marshal(proto.Request{JSONRPC: "2.0", ID: json.RawMessage(`-9`), Method: test.method, Params: test.params(t)})
				_ = fw.Write(request)
				_, _ = fr.Read()
				close(responseRead)
			}()
			rp, err := OpenRemote(context.Background(), proxySide, "dev", "v1")
			require.NoError(t, err)
			release := make(chan struct{})
			entered := make(chan struct{}, 1)
			after := make(chan struct{}, 1)
			test.register(rp, release, entered, after)
			close(send)
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("reverse handler not entered")
			}
			select {
			case <-after:
				t.Fatal("post-response hook ran before handler returned")
			default:
			}
			close(release)
			select {
			case <-responseRead:
			case <-time.After(time.Second):
				t.Fatal("reverse response not read")
			}
			select {
			case <-after:
			case <-time.After(time.Second):
				t.Fatal("post-response hook not called after write")
			}
			require.NoError(t, proxySide.Close())
		})
	}
}
