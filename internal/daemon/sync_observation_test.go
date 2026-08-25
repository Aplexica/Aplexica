package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	pluginproxy "github.com/aplexica/aplexica/internal/plugin/proxy"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

func daemonObservationParams(t *testing.T) proto.RemoteSyncObservationV1Params {
	t.Helper()
	key := make([]byte, 32)
	key[0] = 1
	params, err := proto.NewRemoteSyncObservationV1(
		key, proto.RemoteSyncMetricDuplicateDelivery, 1, proto.RemoteSyncObservationUnitCount, "delivery-1",
	)
	require.NoError(t, err)
	return params
}

func TestRemoteSyncObservationSampleKeyIsPersistentAndNonzero(t *testing.T) {
	store := &secrets.Store{Root: t.TempDir()}
	first, err := LoadOrCreateRemoteSyncObservationSampleKey(store)
	require.NoError(t, err)
	require.NotEqual(t, [32]byte{}, first)
	second, err := LoadOrCreateRemoteSyncObservationSampleKey(store)
	require.NoError(t, err)
	require.Equal(t, first, second)
}

func TestSyncObservationQueueRetriesRejectedMalformedAndPluginRestart(t *testing.T) {
	var calls int
	delivered := make(chan struct{}, 1)
	q := newSyncObservationQueue(4, func(_ context.Context, _ proto.RemoteSyncObservationV1Params) (proto.RemoteSyncObservationV1Result, error) {
		calls++
		switch calls {
		case 1:
			return proto.RemoteSyncObservationV1Result{Accepted: false}, nil
		case 2:
			return proto.RemoteSyncObservationV1Result{}, proto.ErrRemoteSyncObservationInvalid
		case 3:
			return proto.RemoteSyncObservationV1Result{}, ErrRemoteReconnecting
		default:
			delivered <- struct{}{}
			return proto.RemoteSyncObservationV1Result{Accepted: true}, nil
		}
	}, nil)
	q.callTimeout = 100 * time.Millisecond
	q.retryInitial = time.Millisecond
	q.retryMaximum = 2 * time.Millisecond
	q.maxAttempts = 5
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.run(ctx)
	require.True(t, q.enqueue(daemonObservationParams(t)))
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("observation did not survive rejection/error restart window")
	}
	require.Equal(t, 4, calls)
}

func TestSyncObservationQueueWaitsForAllOverlappingReverseResponses(t *testing.T) {
	sent := make(chan struct{}, 1)
	q := newSyncObservationQueue(2, func(_ context.Context, _ proto.RemoteSyncObservationV1Params) (proto.RemoteSyncObservationV1Result, error) {
		sent <- struct{}{}
		return proto.RemoteSyncObservationV1Result{Accepted: true}, nil
	}, nil)
	q.callTimeout = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.run(ctx)

	q.beginInboundRequest()
	q.beginInboundRequest()
	require.True(t, q.enqueue(daemonObservationParams(t)))
	select {
	case <-sent:
		t.Fatal("observation RPC ran while reverse requests were active")
	case <-time.After(20 * time.Millisecond):
	}
	q.endInboundRequest()
	select {
	case <-sent:
		t.Fatal("one overlapping reverse request still awaited its response")
	case <-time.After(20 * time.Millisecond):
	}
	q.endInboundRequest()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("observation was not released after every reverse response")
	}
}

func TestSyncObservationQueueIsBoundedAndNonblocking(t *testing.T) {
	q := newSyncObservationQueue(1, nil, nil)
	params := daemonObservationParams(t)
	require.True(t, q.enqueue(params))
	started := time.Now()
	require.False(t, q.enqueue(params))
	require.Less(t, time.Since(started), 50*time.Millisecond)
}

func TestRemoteRunnerObservationRequiresExactSignedCapabilityAndNonzeroKey(t *testing.T) {
	q := newSyncObservationQueue(2, nil, nil)
	runner := &RemoteRunner{
		proxy:                 &pluginproxy.RemoteProxy{},
		syncObservationSigned: true,
		observations:          q,
	}
	require.False(t, runner.ObserveSyncV1Async(proto.RemoteSyncMetricQuarantine, 1, proto.RemoteSyncObservationUnitCount, "delivery"),
		"key-load failure leaves a zero key and must fail closed")
	runner.ObservationSampleKey[0] = 1
	runner.syncObservationSigned = false
	require.False(t, runner.ObserveSyncV1Async(proto.RemoteSyncMetricQuarantine, 1, proto.RemoteSyncObservationUnitCount, "delivery"),
		"runtime initialize strings cannot replace the signed capability")
	runner.syncObservationSigned = true
	require.True(t, runner.ObserveSyncV1Async(proto.RemoteSyncMetricQuarantine, 1, proto.RemoteSyncObservationUnitCount, "delivery"))
	require.Len(t, q.items, 1)
	runner.stopped.Store(true)
	require.False(t, runner.ObserveSyncV1Async(proto.RemoteSyncMetricQuarantine, 1, proto.RemoteSyncObservationUnitCount, "delivery-2"))
}

func TestValidateSyncObservationSampleKeyRejectsMalformedAndZero(t *testing.T) {
	for _, value := range []string{"", "not-base64", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="} {
		_, err := validateSyncObservationSampleKey(value)
		require.Error(t, err)
	}
	_, err := LoadOrCreateRemoteSyncObservationSampleKey(nil)
	require.Error(t, err)
}
