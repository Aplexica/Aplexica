package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

// ---------------------------------------------------------------------------
// remote.envelope_caps — the daemon->plugin account-level envelope capability
// probe (2026-07-29 envelope wire-efficiency ADR D3).
//
// The answer is the fleet-wide kill switch for envelope v3 ENCODING: the sync
// orchestrator seals v3 only when this switch is on AND every recipient
// device in the verified signed roster advertises envelope version 3 (ADR
// D2). The switch can therefore disable v3 without a release, but can never
// enable it past a mixed fleet.
//
// FAIL-CLOSED at every layer: a reconnecting plugin, an RPC/transport error,
// an older plugin that does not know the method, a non-"entitlement" source,
// or v3_enabled=false all answer "disabled". Errors are cached only briefly
// (envelopeCapsFailureCacheTTL) so a recovered plugin is re-consulted
// promptly, while positive/negative entitlement answers are cached for the
// full envelopeCapsCacheTTL — the ADR's "~5 min" enable/disable propagation
// bound.
// ---------------------------------------------------------------------------

const (
	// envelopeCapsCacheTTL bounds how stale a successfully fetched
	// envelope_caps answer may be. Flipping the server-side switch reaches
	// every sealing daemon within this window.
	envelopeCapsCacheTTL = 5 * time.Minute
	// envelopeCapsFailureCacheTTL is the short negative-cache window applied
	// after an RPC failure: long enough that a wedged plugin is not hammered
	// once per outbound event, short enough that a reconnect restores the
	// real answer quickly. The cached value on failure is always false.
	envelopeCapsFailureCacheTTL = 30 * time.Second
	// envelopeCapsCallTimeout bounds the (rare, cache-miss-only) blocking RPC
	// issued from the outbound seal path.
	envelopeCapsCallTimeout = 5 * time.Second
)

// envelopeCapsCache is the TTL cache behind RemoteRunner.EnvelopeV3Enabled.
// The zero value is ready to use. now is a test seam; nil means time.Now.
type envelopeCapsCache struct {
	mu      sync.Mutex
	at      time.Time
	ttl     time.Duration
	enabled bool
	now     func() time.Time
}

// resolve returns the cached fail-closed answer, refreshing it through fetch
// when the cached entry is missing or expired. fetch errors answer false and
// are negative-cached for envelopeCapsFailureCacheTTL; fetch successes are
// cached for envelopeCapsCacheTTL and are true only for an explicit
// {v3_enabled:true, source:"entitlement"} answer.
func (c *envelopeCapsCache) resolve(ctx context.Context, fetch func(context.Context) (proto.RemoteEnvelopeCapsResult, error)) bool {
	clock := c.clock()
	c.mu.Lock()
	if !c.at.IsZero() && clock().Sub(c.at) < c.ttl {
		enabled := c.enabled
		c.mu.Unlock()
		return enabled
	}
	c.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, envelopeCapsCallTimeout)
	res, err := fetch(callCtx)
	cancel()
	enabled := err == nil && res.V3Enabled && res.Source == proto.RemoteEnvelopeCapsSourceEntitlement
	ttl := time.Duration(envelopeCapsCacheTTL)
	if err != nil {
		ttl = envelopeCapsFailureCacheTTL
	}
	c.mu.Lock()
	c.at, c.ttl, c.enabled = clock(), ttl, enabled
	c.mu.Unlock()
	return enabled
}

func (c *envelopeCapsCache) clock() func() time.Time {
	if c.now != nil {
		return c.now
	}
	return time.Now
}

// EnvelopeCaps delegates the raw remote.envelope_caps call to the live proxy;
// returns ErrRemoteReconnecting when the plugin is mid-restart. Callers that
// want the cached fail-closed boolean use EnvelopeV3Enabled instead.
func (r *RemoteRunner) EnvelopeCaps(ctx context.Context) (proto.RemoteEnvelopeCapsResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteEnvelopeCapsResult{}, ErrRemoteReconnecting
	}
	return p.EnvelopeCaps(ctx)
}

// EnvelopeV3Enabled is the cached, fail-closed account switch consumed by the
// publish adapter (and through it the sync orchestrator's v3 seal-time
// selection). Any error — including a reconnecting plugin or an older plugin
// without the method — answers false.
func (r *RemoteRunner) EnvelopeV3Enabled(ctx context.Context) bool {
	return r.envelopeCaps.resolve(ctx, r.EnvelopeCaps)
}

// remoteEnvelopeCapsPolicy is the narrow seam the publish adapter probes for
// the account-level envelope capability switch — satisfied by *RemoteRunner.
// An interface so the adapter stays unit-testable with a fake client.
type remoteEnvelopeCapsPolicy interface {
	EnvelopeV3Enabled(ctx context.Context) bool
}

// EnvelopeV3Enabled implements syncd.EnvelopeCapsPublisher (the optional
// additive capability the orchestrator probes at seal time, mirroring
// SupportsLargeRetainedCheckpoint). A publisher whose client cannot answer —
// nil adapter, nil client, or a client without the policy seam — reports
// false, preserving today's envelope format.
func (a *RemotePublishAdapter) EnvelopeV3Enabled(ctx context.Context) bool {
	if a == nil || a.client == nil {
		return false
	}
	policy, ok := a.client.(remoteEnvelopeCapsPolicy)
	return ok && policy.EnvelopeV3Enabled(ctx)
}

// compile-time assertion: the publish adapter advertises the optional
// envelope-caps capability to the sync orchestrator.
var _ syncd.EnvelopeCapsPublisher = (*RemotePublishAdapter)(nil)
