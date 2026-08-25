package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

const (
	syncObservationQueueCapacity  = 256
	syncObservationMaxAttempts    = 16
	syncObservationCallTimeout    = 3 * time.Second
	syncObservationRetryInitial   = 250 * time.Millisecond
	syncObservationRetryMaximum   = 5 * time.Second
	syncObservationSampleKeyBytes = 32

	syncObservationSecretArtifact = "_device"
	syncObservationSecretName     = "durable-sync-observation-hmac-v1"
)

type syncObservationSecretStore interface {
	GetOrCreate(artifactID, key string, generate func() (string, error), validate func(string) (string, error)) (string, bool, error)
}

// LoadOrCreateRemoteSyncObservationSampleKey returns a dedicated persistent
// HMAC key. It prevents a server that knows delivery IDs or cadence buckets
// from dictionary-correlating those local source identities with SampleID.
func LoadOrCreateRemoteSyncObservationSampleKey(store syncObservationSecretStore) ([syncObservationSampleKeyBytes]byte, error) {
	var key [syncObservationSampleKeyBytes]byte
	if store == nil {
		return key, errors.New("daemon: sync observation secret store unavailable")
	}
	encoded, _, err := store.GetOrCreate(syncObservationSecretArtifact, syncObservationSecretName, func() (string, error) {
		var generated [syncObservationSampleKeyBytes]byte
		if _, err := rand.Read(generated[:]); err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString(generated[:]), nil
	}, validateSyncObservationSampleKey)
	if err != nil {
		return key, fmt.Errorf("daemon: load or create sync observation sample key: %w", err)
	}
	decoded, _ := base64.StdEncoding.Strict().DecodeString(encoded)
	copy(key[:], decoded)
	return key, nil
}

func validateSyncObservationSampleKey(value string) (string, error) {
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != syncObservationSampleKeyBytes {
		return "", errors.New("invalid sync observation sample key")
	}
	var nonzero byte
	for _, value := range decoded {
		nonzero |= value
	}
	if nonzero == 0 {
		return "", errors.New("invalid zero sync observation sample key")
	}
	return base64.StdEncoding.EncodeToString(decoded), nil
}

type syncObservationQueueItem struct {
	params   proto.RemoteSyncObservationV1Params
	attempts int
}

type syncObservationQueue struct {
	items chan syncObservationQueueItem
	wake  chan struct{}

	inboundRequests atomic.Int64

	send         func(context.Context, proto.RemoteSyncObservationV1Params) (proto.RemoteSyncObservationV1Result, error)
	warn         func(string, ...any)
	callTimeout  time.Duration
	retryInitial time.Duration
	retryMaximum time.Duration
	maxAttempts  int
}

func newSyncObservationQueue(
	capacity int,
	send func(context.Context, proto.RemoteSyncObservationV1Params) (proto.RemoteSyncObservationV1Result, error),
	warn func(string, ...any),
) *syncObservationQueue {
	if capacity <= 0 {
		capacity = 1
	}
	return &syncObservationQueue{
		items: make(chan syncObservationQueueItem, capacity), wake: make(chan struct{}, 1),
		send: send, warn: warn, callTimeout: syncObservationCallTimeout,
		retryInitial: syncObservationRetryInitial, retryMaximum: syncObservationRetryMaximum, maxAttempts: syncObservationMaxAttempts,
	}
}

func (q *syncObservationQueue) enqueue(params proto.RemoteSyncObservationV1Params) bool {
	if q == nil || params.Validate() != nil {
		return false
	}
	select {
	case q.items <- syncObservationQueueItem{params: params}:
		return true
	default:
		if q.warn != nil {
			q.warn("remote sync observation queue saturated; sample omitted", "metric", params.Metric)
		}
		return false
	}
}

func (q *syncObservationQueue) beginInboundRequest() {
	if q == nil {
		return
	}
	q.inboundRequests.Add(1)
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *syncObservationQueue) endInboundRequest() {
	if q == nil {
		return
	}
	for {
		current := q.inboundRequests.Load()
		if current <= 0 || q.inboundRequests.CompareAndSwap(current, current-1) {
			break
		}
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *syncObservationQueue) resetInboundRequests() {
	if q == nil {
		return
	}
	q.inboundRequests.Store(0)
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *syncObservationQueue) waitInboundResponse(ctx context.Context) bool {
	for {
		if q.inboundRequests.Load() == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-q.wake:
		}
	}
}

func (q *syncObservationQueue) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-q.items:
			if !q.waitInboundResponse(ctx) {
				return
			}
			if q.deliver(ctx, &item) {
				continue
			}
			if item.attempts >= q.maxAttempts {
				if q.warn != nil {
					q.warn("remote sync observation retry budget exhausted; sample omitted", "metric", item.params.Metric)
				}
				continue
			}
			delay := q.retryDelay(item.attempts)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			select {
			case <-ctx.Done():
				return
			case q.items <- item:
			default:
				if q.warn != nil {
					q.warn("remote sync observation retry queue saturated; sample omitted", "metric", item.params.Metric)
				}
			}
		}
	}
}

func (q *syncObservationQueue) deliver(ctx context.Context, item *syncObservationQueueItem) bool {
	item.attempts++
	if q.send == nil {
		return false
	}
	callCtx, cancel := context.WithTimeout(ctx, q.callTimeout)
	result, err := q.send(callCtx, item.params)
	cancel()
	return err == nil && result.Accepted
}

func (q *syncObservationQueue) retryDelay(attempt int) time.Duration {
	delay := q.retryInitial
	for current := 1; current < attempt && delay < q.retryMaximum; current++ {
		if delay > q.retryMaximum/2 {
			return q.retryMaximum
		}
		delay *= 2
	}
	if delay > q.retryMaximum {
		return q.retryMaximum
	}
	return delay
}

func (r *RemoteRunner) startSyncObservationQueue(ctx context.Context) {
	if r == nil {
		return
	}
	q := newSyncObservationQueue(syncObservationQueueCapacity, r.sendSyncObservationV1, r.warn)
	r.observationMu.Lock()
	r.observations = q
	r.observationMu.Unlock()
	go q.run(ctx)
}

func (r *RemoteRunner) currentSyncObservationQueue() *syncObservationQueue {
	if r == nil {
		return nil
	}
	r.observationMu.Lock()
	q := r.observations
	r.observationMu.Unlock()
	return q
}

func (r *RemoteRunner) beginSyncObservationInboundBarrier() {
	if q := r.currentSyncObservationQueue(); q != nil {
		q.beginInboundRequest()
	}
}

func (r *RemoteRunner) endSyncObservationInboundBarrier() {
	if q := r.currentSyncObservationQueue(); q != nil {
		q.endInboundRequest()
	}
}

func (r *RemoteRunner) resetSyncObservationInboundBarrier() {
	if q := r.currentSyncObservationQueue(); q != nil {
		q.resetInboundRequests()
	}
}

func (r *RemoteRunner) signedSyncObservationReady() bool {
	if r == nil {
		return false
	}
	r.proxyMu.Lock()
	ready := r.proxy != nil && r.syncObservationSigned
	r.proxyMu.Unlock()
	return ready
}

// ObserveSyncV1Async validates and enqueues one content-free observation. It
// never performs plugin I/O on the caller goroutine, and returns false on an
// unsigned plugin, missing private sample key, invalid sample, or full queue.
func (r *RemoteRunner) ObserveSyncV1Async(metric string, value float64, unit, sourceIdentity string) bool {
	if r == nil || r.stopped.Load() || !r.signedSyncObservationReady() {
		return false
	}
	params, err := proto.NewRemoteSyncObservationV1(r.ObservationSampleKey[:], metric, value, unit, sourceIdentity)
	if err != nil {
		return false
	}
	q := r.currentSyncObservationQueue()
	return q != nil && q.enqueue(params)
}

func (r *RemoteRunner) sendSyncObservationV1(ctx context.Context, params proto.RemoteSyncObservationV1Params) (proto.RemoteSyncObservationV1Result, error) {
	if err := params.Validate(); err != nil {
		return proto.RemoteSyncObservationV1Result{}, err
	}
	r.proxyMu.Lock()
	p, signed := r.proxy, r.syncObservationSigned
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteSyncObservationV1Result{}, ErrRemoteReconnecting
	}
	if !signed {
		return proto.RemoteSyncObservationV1Result{}, ErrRemoteNotConfigured
	}
	return p.ObserveSyncV1(ctx, params)
}
