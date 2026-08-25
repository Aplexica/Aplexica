package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/stretchr/testify/require"
)

func TestRemoteProxySubmitsExactAtomicAuthorityRosterPackageMethod(t *testing.T) {
	proxySide, pluginSide := newPipePair()
	requestCh := make(chan proto.Request, 1)
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
		requestCh <- request
		response, _ = json.Marshal(proto.Response{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{}`)})
		_ = fw.Write(response)
	}()
	rp, err := OpenRemote(context.Background(), proxySide, "device-a", "v1.0.37")
	require.NoError(t, err)
	t.Cleanup(func() { _ = proxySide.Close() })

	blob := []byte("canonical-atomic-package")
	object := proto.RemoteOpaqueSignedObject{
		ScopeType: "account", ScopeID: "scope-a", Kind: "atomic-authority-roster-transition", Sequence: 2,
		PreviousHash: sha256.Sum256([]byte("previous-roster")), Hash: sha256.Sum256(blob), Blob: blob,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, rp.SubmitAtomicAuthorityRosterTransition(ctx, object))
	request := <-requestCh
	require.Equal(t, proto.MethodRemoteSubmitAtomicAuthorityRosterTransition, request.Method)
	var params proto.RemoteSubmitSignedObjectParams
	require.NoError(t, json.Unmarshal(request.Params, &params))
	require.Equal(t, object, params.Object)
}
