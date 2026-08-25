package host

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/stretchr/testify/require"
)

type observationRemoteHandler struct {
	stubRemoteHandler
	calls  int
	params proto.RemoteSyncObservationV1Params
	result proto.RemoteSyncObservationV1Result
}

func (h *observationRemoteHandler) ObserveSyncV1(_ context.Context, params proto.RemoteSyncObservationV1Params) (proto.RemoteSyncObservationV1Result, error) {
	h.calls++
	h.params = params
	return h.result, nil
}

func observationHostFrame(t *testing.T, params json.RawMessage) []byte {
	t.Helper()
	raw, err := json.Marshal(proto.Request{
		JSONRPC: "2.0", ID: json.RawMessage(`17`), Method: proto.MethodRemoteObserveSyncV1, Params: params,
	})
	require.NoError(t, err)
	return raw
}

func TestDispatchRemoteSyncObservationV1UsesAdditiveHandler(t *testing.T) {
	params, err := proto.NewRemoteSyncObservationV1(
		make([]byte, 32), proto.RemoteSyncMetricQuarantine, 1, proto.RemoteSyncObservationUnitCount, "delivery",
	)
	require.Error(t, err, "zero private key must fail before dispatch")
	key := make([]byte, 32)
	key[0] = 1
	params, err = proto.NewRemoteSyncObservationV1(key, proto.RemoteSyncMetricQuarantine, 1, proto.RemoteSyncObservationUnitCount, "delivery")
	require.NoError(t, err)
	encoded, err := json.Marshal(params)
	require.NoError(t, err)

	handler := &observationRemoteHandler{result: proto.RemoteSyncObservationV1Result{Accepted: true}}
	response := dispatchRemote(context.Background(), handler, observationHostFrame(t, encoded))
	require.Nil(t, response.Error)
	require.Equal(t, 1, handler.calls)
	require.Equal(t, params, handler.params)
	var result proto.RemoteSyncObservationV1Result
	require.NoError(t, json.Unmarshal(response.Result, &result))
	require.True(t, result.Accepted)
}

func TestDispatchRemoteSyncObservationV1BaseDefaultAndStrictValidation(t *testing.T) {
	key := make([]byte, 32)
	key[0] = 1
	params, err := proto.NewRemoteSyncObservationV1(key, proto.RemoteSyncMetricQuarantine, 1, proto.RemoteSyncObservationUnitCount, "delivery")
	require.NoError(t, err)
	encoded, err := json.Marshal(params)
	require.NoError(t, err)

	baseResponse := dispatchRemote(context.Background(), &stubRemoteHandler{}, observationHostFrame(t, encoded))
	require.NotNil(t, baseResponse.Error)
	require.Contains(t, baseResponse.Error.Message, ErrDurableSyncObservationUnsupported.Error())

	handler := &observationRemoteHandler{}
	malformed := append(encoded[:len(encoded)-1], []byte(`,"content":"forbidden"}`)...)
	response := dispatchRemote(context.Background(), handler, observationHostFrame(t, malformed))
	require.NotNil(t, response.Error)
	require.Equal(t, proto.CodeInvalidParams, response.Error.Code)
	require.Zero(t, handler.calls, "invalid content-bearing extension must fail before handler invocation")
}
