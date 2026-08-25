package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/stretchr/testify/require"
)

type poisonFetchClient struct {
	fetches atomic.Int64
}

func (c *poisonFetchClient) RegisterWrapKey(context.Context, []byte) error { return nil }
func (c *poisonFetchClient) Enumerate(context.Context, proto.RemoteEnumerateParams) (proto.RemoteEnumerateResult, error) {
	return proto.RemoteEnumerateResult{Namespaces: []proto.RemoteNamespaceManifest{{
		NamespaceID: "ns-1",
		Branches:    []proto.RemoteBranchManifest{{BranchID: "main", TipEventID: "tip-9"}},
	}}}, nil
}
func (c *poisonFetchClient) Subscribe(context.Context, string) error { return nil }
func (c *poisonFetchClient) Fetch(context.Context, proto.RemoteFetchParams) (proto.RemoteFetchResult, error) {
	c.fetches.Add(1)
	return proto.RemoteFetchResult{Events: []proto.RemoteEvent{{
		ArtifactID: "art-1", EventID: "poison-1", Kind: "memory",
	}}}, nil
}
func (c *poisonFetchClient) RestartCount() uint64 { return 0 }

type kindsBus struct{ kinds atomic.Value }

func (b *kindsBus) Publish(kind string, body any) {
	cur, _ := b.kinds.Load().([]string)
	b.kinds.Store(append(append([]string{}, cur...), kind))
}

// A permanently-retryable inbound event must back the branch off exponentially
// instead of refetching every driver tick, and must surface a wedged event.
func TestRemoteSyncDriver_BacksOffPoisonBranch(t *testing.T) {
	oldInterval, oldWarm, oldBase, oldMax := remoteSyncDriverInterval, remoteSyncDriverWarmup, remoteFetchBackoffBase, remoteFetchBackoffMax
	remoteSyncDriverInterval, remoteSyncDriverWarmup = 10*time.Millisecond, time.Millisecond
	remoteFetchBackoffBase, remoteFetchBackoffMax = 40*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() {
		remoteSyncDriverInterval, remoteSyncDriverWarmup = oldInterval, oldWarm
		remoteFetchBackoffBase, remoteFetchBackoffMax = oldBase, oldMax
	})

	client := &poisonFetchClient{}
	bus := &kindsBus{}
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runRemoteSyncDriver(ctx, client, nil,
			func([]proto.RemoteEvent) []syncd.ImportOutcome {
				return []syncd.ImportOutcome{syncd.ImportRetryable}
			},
			nil, noopLogger{}, bus)
	}()
	<-done

	fetches := client.fetches.Load()
	// ~90 ticks fit in 900ms; with backoff 40→80→160→200… the branch is
	// fetched far fewer times. Generous ceiling to avoid flakes.
	require.LessOrEqual(t, fetches, int64(12), "poison branch must not be fetched every tick")
	require.GreaterOrEqual(t, fetches, int64(3), "branch must still retry")
}

// flakyPoisonFetchClient alternates Fetch outcomes by call count: odd calls
// return one permanently-retryable event, even calls fail outright (a
// reconnecting relay). Same manifest shape as poisonFetchClient.
type flakyPoisonFetchClient struct {
	fetches atomic.Int64
}

func (c *flakyPoisonFetchClient) RegisterWrapKey(context.Context, []byte) error { return nil }
func (c *flakyPoisonFetchClient) Enumerate(context.Context, proto.RemoteEnumerateParams) (proto.RemoteEnumerateResult, error) {
	return proto.RemoteEnumerateResult{Namespaces: []proto.RemoteNamespaceManifest{{
		NamespaceID: "ns-1",
		Branches:    []proto.RemoteBranchManifest{{BranchID: "main", TipEventID: "tip-9"}},
	}}}, nil
}
func (c *flakyPoisonFetchClient) Subscribe(context.Context, string) error { return nil }
func (c *flakyPoisonFetchClient) Fetch(context.Context, proto.RemoteFetchParams) (proto.RemoteFetchResult, error) {
	if c.fetches.Add(1)%2 == 0 {
		return proto.RemoteFetchResult{}, errors.New("relay reconnecting")
	}
	return proto.RemoteFetchResult{Events: []proto.RemoteEvent{{
		ArtifactID: "art-1", EventID: "poison-1", Kind: "memory",
	}}}, nil
}
func (c *flakyPoisonFetchClient) RestartCount() uint64 { return 0 }

// A transient Fetch error between retryable-import passes must NOT reset the
// branch's backoff state: the same-event failure counter keeps escalating to
// the wedged threshold, and the held backoff keeps bounding the fetch cadence.
func TestRemoteSyncDriver_BackoffSurvivesFetchErrors(t *testing.T) {
	oldInterval, oldWarm, oldBase, oldMax := remoteSyncDriverInterval, remoteSyncDriverWarmup, remoteFetchBackoffBase, remoteFetchBackoffMax
	remoteSyncDriverInterval, remoteSyncDriverWarmup = 10*time.Millisecond, time.Millisecond
	remoteFetchBackoffBase, remoteFetchBackoffMax = 20*time.Millisecond, 60*time.Millisecond
	t.Cleanup(func() {
		remoteSyncDriverInterval, remoteSyncDriverWarmup = oldInterval, oldWarm
		remoteFetchBackoffBase, remoteFetchBackoffMax = oldBase, oldMax
	})

	client := &flakyPoisonFetchClient{}
	bus := &kindsBus{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runRemoteSyncDriver(ctx, client, nil,
			func([]proto.RemoteEvent) []syncd.ImportOutcome {
				return []syncd.ImportOutcome{syncd.ImportRetryable}
			},
			nil, noopLogger{}, bus)
	}()
	<-done

	kinds, _ := bus.kinds.Load().([]string)
	// Escalating 20→40→60ms (capped) with one error pass interleaved per cycle
	// reaches remoteFetchWedgedThreshold (10) same-event failures in well under
	// 2s — but only if the fetch-error passes don't reset the counter.
	require.Contains(t, kinds, "remote.backfill_wedged",
		"same-event failures must keep counting across transient fetch errors")

	fetches := client.fetches.Load()
	// ~200 ticks fit in 2s. Each error/poison call pair is gated by the held
	// 20→40→60ms backoff, so the branch is fetched far fewer times than once
	// per tick. Generous ceiling to avoid flakes.
	require.LessOrEqual(t, fetches, int64(80), "backoff must be held across transient fetch errors")
	require.GreaterOrEqual(t, fetches, int64(6), "branch must still retry")
}
