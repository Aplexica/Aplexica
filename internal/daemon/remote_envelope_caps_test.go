package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

type envelopeCapsFetchStub struct {
	calls  int
	result proto.RemoteEnvelopeCapsResult
	err    error
}

func (s *envelopeCapsFetchStub) fetch(context.Context) (proto.RemoteEnvelopeCapsResult, error) {
	s.calls++
	return s.result, s.err
}

func TestEnvelopeCapsCacheEnabledOnlyForEntitlementSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result proto.RemoteEnvelopeCapsResult
		err    error
		want   bool
	}{
		{"entitlement-on", proto.RemoteEnvelopeCapsResult{V3Enabled: true, Source: proto.RemoteEnvelopeCapsSourceEntitlement}, nil, true},
		{"entitlement-off", proto.RemoteEnvelopeCapsResult{V3Enabled: false, Source: proto.RemoteEnvelopeCapsSourceEntitlement}, nil, false},
		{"absent", proto.RemoteEnvelopeCapsResult{V3Enabled: false, Source: proto.RemoteEnvelopeCapsSourceAbsent}, nil, false},
		// A plugin violating the fail-closed contract (enabled without the
		// entitlement source) is still refused daemon-side.
		{"enabled-but-absent-source", proto.RemoteEnvelopeCapsResult{V3Enabled: true, Source: proto.RemoteEnvelopeCapsSourceAbsent}, nil, false},
		{"rpc-error", proto.RemoteEnvelopeCapsResult{}, errors.New("plugin reconnecting"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &envelopeCapsFetchStub{result: tc.result, err: tc.err}
			cache := &envelopeCapsCache{}
			require.Equal(t, tc.want, cache.resolve(context.Background(), stub.fetch))
			require.Equal(t, 1, stub.calls)
		})
	}
}

func TestEnvelopeCapsCacheTTLAndFailureNegativeCache(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }

	// A successful answer is held for the full 5-minute TTL.
	stub := &envelopeCapsFetchStub{result: proto.RemoteEnvelopeCapsResult{V3Enabled: true, Source: proto.RemoteEnvelopeCapsSourceEntitlement}}
	cache := &envelopeCapsCache{now: clock}
	require.True(t, cache.resolve(context.Background(), stub.fetch))
	require.True(t, cache.resolve(context.Background(), stub.fetch))
	require.Equal(t, 1, stub.calls, "fresh cache must not re-fetch")
	now = now.Add(envelopeCapsCacheTTL - time.Second)
	require.True(t, cache.resolve(context.Background(), stub.fetch))
	require.Equal(t, 1, stub.calls)
	now = now.Add(2 * time.Second)
	require.True(t, cache.resolve(context.Background(), stub.fetch))
	require.Equal(t, 2, stub.calls, "expired cache must re-fetch")

	// A failure answers false and is negative-cached only briefly, so a
	// recovered plugin is re-consulted promptly.
	now = time.Unix(1_800_000_000, 0)
	failing := &envelopeCapsFetchStub{err: errors.New("plugin reconnecting")}
	failCache := &envelopeCapsCache{now: clock}
	require.False(t, failCache.resolve(context.Background(), failing.fetch))
	require.False(t, failCache.resolve(context.Background(), failing.fetch))
	require.Equal(t, 1, failing.calls, "failure must be negative-cached")
	now = now.Add(envelopeCapsFailureCacheTTL + time.Second)
	failing.err = nil
	failing.result = proto.RemoteEnvelopeCapsResult{V3Enabled: true, Source: proto.RemoteEnvelopeCapsSourceEntitlement}
	require.True(t, failCache.resolve(context.Background(), failing.fetch))
	require.Equal(t, 2, failing.calls, "recovered plugin must be re-consulted after the short failure TTL")
}

type envelopeCapsClientStub struct {
	enabled bool
}

func (envelopeCapsClientStub) Publish(context.Context, []proto.RemoteEvent) (proto.RemotePublishResult, error) {
	return proto.RemotePublishResult{}, nil
}
func (c envelopeCapsClientStub) EnvelopeV3Enabled(context.Context) bool { return c.enabled }

type envelopeCapsPlainClientStub struct{}

func (envelopeCapsPlainClientStub) Publish(context.Context, []proto.RemoteEvent) (proto.RemotePublishResult, error) {
	return proto.RemotePublishResult{}, nil
}

func TestRemotePublishAdapterEnvelopeV3EnabledFailsClosed(t *testing.T) {
	require.False(t, (*RemotePublishAdapter)(nil).EnvelopeV3Enabled(context.Background()))
	require.False(t, (&RemotePublishAdapter{}).EnvelopeV3Enabled(context.Background()))
	require.False(t, (&RemotePublishAdapter{client: envelopeCapsPlainClientStub{}}).EnvelopeV3Enabled(context.Background()),
		"a client without the caps seam must answer disabled")
	require.False(t, (&RemotePublishAdapter{client: envelopeCapsClientStub{enabled: false}}).EnvelopeV3Enabled(context.Background()))
	require.True(t, (&RemotePublishAdapter{client: envelopeCapsClientStub{enabled: true}}).EnvelopeV3Enabled(context.Background()))

	// The adapter is the object the sync orchestrator probes at seal time.
	var pub syncd.RemoteEventPublisher = &RemotePublishAdapter{client: envelopeCapsClientStub{enabled: true}}
	caps, ok := pub.(syncd.EnvelopeCapsPublisher)
	require.True(t, ok)
	require.True(t, caps.EnvelopeV3Enabled(context.Background()))
}
